package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/edge"
	"open_live_platform/internal/etcdlive"
	"open_live_platform/internal/platformgrpc"
)

type managementListener struct {
	server   *http.Server
	listener net.Listener
}

func (a *App) Run(ctx context.Context) error {
	public, err := net.Listen("tcp", a.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	defer public.Close()
	internal, err := net.Listen("tcp", a.Config.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen gRPC: %w", err)
	}
	defer internal.Close()
	var management []managementListener
	for _, server := range []*http.Server{a.Backoffice, a.MCNBackoffice} {
		if server == nil {
			continue
		}
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return fmt.Errorf("listen management HTTP: %w", err)
		}
		defer listener.Close()
		management = append(management, managementListener{server, listener})
	}
	if a.metrics != nil {
		listener, err := net.Listen("tcp", a.Config.MetricsAddr)
		if err != nil {
			return fmt.Errorf("listen Prometheus metrics: %w", err)
		}
		defer listener.Close()
		management = append(management, managementListener{
			server: &http.Server{Handler: a.metrics.Handler(), ReadHeaderTimeout: 5 * time.Second}, listener: listener,
		})
	}
	return a.serve(ctx, public, internal, management...)
}

func (a *App) serve(ctx context.Context, public, internal net.Listener, management ...managementListener) error {
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		a.monitorAnchorAway(monitorCtx)
	}()
	sttDone := make(chan struct{})
	if a.Subtitles != nil && a.Subtitles.Enabled() {
		go func() {
			defer close(sttDone)
			a.monitorSTT(monitorCtx)
		}()
	} else {
		close(sttDone)
	}
	dateDone := make(chan struct{})
	if a.dates != nil {
		go func() {
			defer close(dateDone)
			a.monitorDates(monitorCtx)
		}()
	} else {
		close(dateDone)
	}
	mailDone := make(chan struct{})
	if a.MCNBackoffice != nil && a.db != nil && a.mcnMailer != nil {
		go func() {
			defer close(mailDone)
			a.monitorMCNInviteMail(monitorCtx)
		}()
	} else {
		close(mailDone)
	}

	rpcServer := grpc.NewServer()
	platformgrpc.Register(rpcServer, a.Accounts, a.Rooms)
	rpcHealth := health.NewServer()
	rpcHealth.SetServingStatus(etcdlive.PlatformService, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(rpcServer, rpcHealth)
	clusterDone := a.monitorCluster(monitorCtx, internal)
	served := make(chan error, 3+len(management))
	kafkaDone := make(chan struct{})
	if len(a.Config.Kafka.Topics) > 0 {
		go func() {
			defer close(kafkaDone)
			served <- edge.ConsumeNotices(monitorCtx, a.Config.Kafka, a.push)
		}()
	} else {
		close(kafkaDone)
	}
	go func() { served <- a.Server.Serve(public) }()
	go func() { served <- rpcServer.Serve(internal) }()
	servers := []*http.Server{a.Server}
	for _, item := range management {
		servers = append(servers, item.server)
		go func() { served <- item.server.Serve(item.listener) }()
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	}
	stopMonitor()
	rpcHealth.Shutdown()

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpDone := make(chan error, len(servers))
	for _, server := range servers {
		go func() {
			err := server.Shutdown(shutdown)
			if err != nil {
				server.Close()
			}
			httpDone <- err
		}()
	}
	forceStop := context.AfterFunc(shutdown, rpcServer.Stop)
	rpcServer.GracefulStop()
	forceStop()
	for range servers {
		serveErr = errors.Join(serveErr, <-httpDone)
	}
	<-monitorDone
	<-sttDone
	<-dateDone
	<-mailDone
	<-kafkaDone
	clusterDone.Wait()
	return serveErr
}

func (a *App) monitorMCNInviteMail(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := admin.DeliverMCNInviteMail(ctx, a.db, a.mcnMailer); err != nil && ctx.Err() == nil {
			log.Print("MCN invitation could not be delivered")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) monitorDates(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := a.dates.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Printf("expire 1v1 sessions: %v", err)
		}
		if err := a.dates.SweepGiftQueue(ctx); err != nil && ctx.Err() == nil {
			log.Printf("start queued 1v1 gifts: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) monitorSTT(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := a.Subtitles.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Printf("stop inactive Agora STT tasks: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) monitorAnchorAway(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := a.Rooms.CloseExpiredAnchorRooms(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("close expired anchor rooms: %v", err)
		}
		if len(a.Config.RoomBots.Accounts) > 0 {
			if err := a.db.SweepRoomBots(ctx); err != nil && ctx.Err() == nil {
				log.Printf("update live room bots: %v", err)
			}
		}
		if a.db != nil {
			if err := a.db.SweepRoomGiftEffects(ctx); err != nil && ctx.Err() == nil {
				log.Printf("start queued hardware gifts: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
