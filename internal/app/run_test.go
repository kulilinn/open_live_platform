package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	platformv1 "open_live_platform/api/platform/v1"
)

func TestInternalGRPCAndPublicHTTP(t *testing.T) {
	const serviceToken = "internal-directory-fixture"
	a, err := New(t.Context(), Config{InternalUserToken: serviceToken})
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
	account, err := a.Accounts.SignUp(callCtx, "trusted-rpc@example.test", "test-password", "RPC recipient")
	if err != nil {
		t.Fatal(err)
	}
	directory := platformv1.NewUserDirectoryServiceClient(conn)
	query := &platformv1.GetUserRequest{Id: account.User.ID}
	if _, err := directory.GetUser(callCtx, query); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("gRPC accepted missing service auth: %v", err)
	}
	unauthorized := metadata.AppendToOutgoingContext(callCtx, "x-inner-service", "innow", "authorization", "Bearer unrelated")
	if _, err := directory.GetUser(unauthorized, query); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("gRPC accepted an unrelated token: %v", err)
	}
	authorized := metadata.AppendToOutgoingContext(callCtx, "x-inner-service", "innow", "authorization", "Bearer "+serviceToken)
	view, err := directory.GetUser(authorized, query)
	if err != nil || view.GetId() != account.User.ID || view.GetDisplayName() != "RPC recipient" || view.GetEmail() != account.User.Email {
		t.Fatalf("typed internal user response = %+v, error=%v", view, err)
	}
	if _, err := directory.GetUser(authorized, &platformv1.GetUserRequest{Id: account.User.ID + 1}); status.Code(err) != codes.NotFound {
		t.Fatalf("gRPC did not report a missing user: %v", err)
	}
	client := http.Client{Timeout: 5 * time.Second}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodPost, "/internal/rpc/platform.stats", http.StatusNotFound},
		{http.MethodPost, platformv1.PlatformService_GetStats_FullMethodName, http.StatusNotFound},
		{http.MethodPost, platformv1.UserDirectoryService_GetUser_FullMethodName, http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(ctx, tc.method, "http://"+public.Addr().String()+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatalf("read %s response: %v", tc.path, err)
		}
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
