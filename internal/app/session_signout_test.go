package app_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/relay"
)

func TestSignoutDisconnectsSessionSockets(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testSignoutDisconnectsSessionSockets(t, app.Config{RelaySecret: "test-relay-secret"})
	})
	t.Run("postgres", func(t *testing.T) {
		config, _ := postgresTestDatabase(t)
		testSignoutDisconnectsSessionSockets(t, config)
	})
}

func testSignoutDisconnectsSessionSockets(t *testing.T, config app.Config) {
	t.Helper()
	ctx := t.Context()
	config.JWTSecret = "test-jwt-secret"
	start := func() (*app.App, *httptest.Server) {
		t.Helper()
		platform, err := app.New(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(platform.Close)
		httpServer := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(httpServer.Close)
		return platform, httpServer
	}
	platform, first := start()
	second := first
	if config.DatabaseURL != "" {
		_, second = start()
	}
	const email, password = "owner@example.org", "test-password"
	opaque, err := platform.Accounts.SignUp(ctx, email, password, "Owner")
	if err != nil {
		t.Fatal(err)
	}
	independent, err := platform.Accounts.SignIn(ctx, email, password)
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := platform.Accounts.(*auth.JWTAccounts).SignInJWT(ctx, email, password)
	if err != nil {
		t.Fatal(err)
	}
	access := func() string {
		t.Helper()
		session, ok, err := platform.Accounts.Refresh(ctx, jwt.Token)
		if err != nil || !ok {
			t.Fatalf("refresh accepted = %t, error = %v", ok, err)
		}
		return session.Token
	}
	jwtAccess, secondAccess := access(), access()
	if jwtAccess == secondAccess {
		t.Fatal("expected two distinct access tokens for one family")
	}
	if _, err := platform.Rooms.Start(ctx, opaque.User.ID, "Live", ""); err != nil {
		t.Fatal(err)
	}
	request := func(server *httptest.Server, method, route, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("%s %s status = %d", method, route, response.StatusCode)
		}
		return response
	}
	dial := func(server *httptest.Server, token string, ticket bool, protocol string) *websocket.Conn {
		t.Helper()
		address := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/connector/" + token + "/" + relay.Signature(token, config.RelaySecret)
		if ticket {
			response := request(server, http.MethodGet, "/api/device/qrcode", token)
			defer response.Body.Close()
			var ticket struct{ URL string }
			if err := json.NewDecoder(response.Body).Decode(&ticket); err != nil {
				t.Fatal(err)
			}
			address = ticket.URL
		}
		dialer := *websocket.DefaultDialer
		if protocol != "" {
			dialer.Subprotocols = []string{protocol}
		}
		conn, response, err := dialer.DialContext(ctx, address, nil)
		if err != nil {
			if response != nil {
				response.Body.Close()
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if protocol != "" {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatalf("%s initial frame: %v", protocol, err)
			}
		}
		return conn
	}
	wantRevoked := func(conn *websocket.Conn, code int, reason string) {
		t.Helper()
		_, _, err := conn.ReadMessage()
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != code || closed.Text != reason {
			t.Fatalf("socket read = %v, want close %d %q", err, code, reason)
		}
	}
	wantChat := func(conn *websocket.Conn, text string) {
		t.Helper()
		var event struct{ Type, Text string }
		if err := conn.ReadJSON(&event); err != nil || event.Type != "chat" || event.Text != text {
			t.Fatalf("socket event = %+v, error = %v, want chat %q", event, err, text)
		}
	}
	jwtFirst := dial(first, jwtAccess, false, "")
	jwtSecond := dial(second, secondAccess, true, "")
	h5 := dial(second, secondAccess, false, "h5relayer")
	device := dial(second, secondAccess, false, "devicerelayer")
	opaqueFirst := dial(first, opaque.Token, true, "")
	opaqueSecond := dial(second, opaque.Token, false, "")
	other := dial(second, independent.Token, false, "")

	request(first, http.MethodPost, "/api/auth/signout", jwtAccess).Body.Close()
	wantRevoked(jwtFirst, websocket.ClosePolicyViolation, "session revoked")
	wantRevoked(jwtSecond, websocket.ClosePolicyViolation, "session revoked")
	wantRevoked(h5, websocket.CloseNormalClosure, "SERVICE_LOGOUT")
	wantRevoked(device, websocket.CloseNormalClosure, "SERVICE_LOGOUT")
	if err := platform.Rooms.Send(ctx, opaque.User.ID, opaque.User.ID, "after JWT logout"); err != nil {
		t.Fatal(err)
	}
	wantChat(opaqueFirst, "after JWT logout")
	wantChat(opaqueSecond, "after JWT logout")
	wantChat(other, "after JWT logout")

	request(first, http.MethodPost, "/api/auth/signout", opaque.Token).Body.Close()
	wantRevoked(opaqueFirst, websocket.ClosePolicyViolation, "session revoked")
	wantRevoked(opaqueSecond, websocket.ClosePolicyViolation, "session revoked")
	if err := platform.Rooms.Send(ctx, opaque.User.ID, opaque.User.ID, "after opaque logout"); err != nil {
		t.Fatal(err)
	}
	wantChat(other, "after opaque logout")
}
