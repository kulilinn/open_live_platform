package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"open_live_platform/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	platform, err := app.New(ctx, app.ConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("starting HTTP on %s and internal gRPC on %s", platform.Server.Addr, platform.Config.GRPCAddr)
	err = platform.Run(ctx)
	platform.Close()
	if err != nil {
		log.Fatal(err)
	}
}
