package app

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	platformv1 "open_live_platform/api/platform/v1"
)

func TestInternalGRPCAndPublicHTTP(t *testing.T) {
	a, err := New(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if a.Config.GRPCAddr != "127.0.0.1:9090" {
		t.Fatalf("default gRPC address = %q", a.Config.GRPCAddr)
	}
	listen := func() net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	}
	public, internal := listen(), listen()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx, public, internal) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("servers did not stop")
		}
	})

	conn, err := grpc.NewClient(internal.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := platformv1.NewPlatformServiceClient(conn).GetStats(callCtx, &platformv1.GetStatsRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: 5 * time.Second}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodPost, "/internal/rpc/platform.stats", http.StatusNotFound},
		{http.MethodPost, platformv1.PlatformService_GetStats_FullMethodName, http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(ctx, tc.method, "http://"+public.Addr().String()+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s returned %d, want %d", tc.path, res.StatusCode, tc.want)
		}
	}
}

func TestRunReportsUnavailableGRPCPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	t.Setenv("PLATFORM_GRPC_ADDR", occupied.Addr().String())
	t.Setenv("PLATFORM_ADDR", "127.0.0.1:0")
	cfg := ConfigFromEnv()
	cfg.DatabaseURL, cfg.GiftCatalogFile = "", ""
	a, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := a.Run(ctx); err == nil || !strings.Contains(err.Error(), "listen gRPC") {
		t.Errorf("occupied gRPC port: Run returned %v", err)
	}
}
