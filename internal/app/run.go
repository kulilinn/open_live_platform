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

	"open_live_platform/internal/edge"
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

	rpcServer := grpc.NewServer()
	platformgrpc.Register(rpcServer, a.Accounts, a.Rooms)
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
	<-kafkaDone
	return serveErr
}

func (a *App) monitorDates(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := a.dates.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Printf("expire 1v1 sessions: %v", err)
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
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
