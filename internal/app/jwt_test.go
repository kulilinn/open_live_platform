package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
)

func TestJWTSessionFlow(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testJWTSessionFlow(t, app.Config{RelaySecret: "test-relay-secret"})
	})
	t.Run("postgres", func(t *testing.T) {
		config, _ := postgresTestDatabase(t)
		testJWTSessionFlow(t, config)
	})
}

func testJWTSessionFlow(t *testing.T, config app.Config) {
	t.Helper()
	config.JWTSecret = "test-secret-for-original-live-protocol"
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { platform.Close() }()

	credentials := `{"email":"host@example.org","account":"Host","password":"example-password"}`
	request := func(method, path, token, body string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s %s: got HTTP %d, want %d", method, path, w.Code, status)
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return ""
		}
		var response struct {
			Token string
			URL   string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.URL != "" {
			return response.URL
		}
		return response.Token
	}

	legacy := request(http.MethodPost, "/api/auth/signup", "", credentials, http.StatusCreated)
	refresh := request(http.MethodPost, "/api/auth/signin?compat=rust", "", credentials, http.StatusOK)
	if strings.Count(refresh, ".") != 2 {
		t.Fatal("compatibility login did not return a JWT")
	}
	request(http.MethodGet, "/api/user/me", refresh, "", http.StatusUnauthorized)
	request(http.MethodGet, "/api/device/qrcode", refresh, "", http.StatusUnauthorized)
	request(http.MethodPost, "/api/auth/signout", refresh, "", http.StatusUnauthorized)
	access := request(http.MethodGet, "/api/auth/session", refresh, "", http.StatusOK)
	secondAccess := request(http.MethodGet, "/api/auth/session?compat=rust", refresh, "", http.StatusOK)
	request(http.MethodGet, "/api/auth/session", access, "", http.StatusUnauthorized)
	request(http.MethodGet, "/api/auth/session?compat=rust", access, "", http.StatusUnauthorized)
	request(http.MethodGet, "/api/user/me", access, "", http.StatusOK)
	request(http.MethodGet, "/api/user/me", secondAccess, "", http.StatusOK)

	nextRefresh := request(http.MethodPost, "/api/auth/signin?compat=rust", "", credentials, http.StatusOK)
	request(http.MethodGet, "/api/auth/session", refresh, "", http.StatusUnauthorized)
	request(http.MethodGet, "/api/user/me", secondAccess, "", http.StatusUnauthorized)

	if config.DatabaseURL != "" {
		platform.Close()
		platform, err = app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
	}
	nextAccess := request(http.MethodGet, "/api/auth/session", nextRefresh, "", http.StatusOK)
	request(http.MethodPost, "/api/live/room/start", nextAccess, `{"title":"Room"}`, http.StatusOK)
	ticket := request(http.MethodGet, "/api/device/qrcode", nextAccess, "", http.StatusOK)
	request(http.MethodPost, "/api/auth/signout", nextAccess, "", http.StatusOK)
	request(http.MethodGet, "/api/user/me", nextAccess, "", http.StatusUnauthorized)
	request(http.MethodGet, "/api/auth/session", nextRefresh, "", http.StatusUnauthorized)

	// A previously issued relay ticket is invalid after the family is revoked.
	server := httptest.NewServer(platform.Server.Handler)
	defer server.Close()
	address := "ws" + strings.TrimPrefix(server.URL, "http") + strings.TrimPrefix(ticket, "ws://example.com")
	ws, response, err := websocket.DefaultDialer.DialContext(t.Context(), address, nil)
	if ws != nil {
		ws.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("relay accepted a ticket from a revoked session")
	}
	if got := request(http.MethodGet, "/api/auth/session", legacy, "", http.StatusOK); got == "" || got != legacy {
		t.Fatal("legacy session was changed by JWT login or signout")
	}
}
