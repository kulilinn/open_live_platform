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

	"open_live_platform/internal/platformgrpc"
)

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
	return a.serve(ctx, public, internal)
}

func (a *App) serve(ctx context.Context, public, internal net.Listener) error {
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		a.monitorAnchorAway(monitorCtx)
	}()

	rpcServer := grpc.NewServer()
	platformgrpc.Register(rpcServer, a.Accounts, a.Rooms)
	served := make(chan error, 2)
	go func() { served <- a.Server.Serve(public) }()
	go func() { served <- rpcServer.Serve(internal) }()

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
	httpDone := make(chan error, 1)
	go func() { httpDone <- a.Server.Shutdown(shutdown) }()
	forceStop := context.AfterFunc(shutdown, rpcServer.Stop)
	rpcServer.GracefulStop()
	forceStop()
	httpErr := <-httpDone
	if httpErr != nil {
		a.Server.Close()
	}
	<-monitorDone
	return errors.Join(serveErr, httpErr)
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
