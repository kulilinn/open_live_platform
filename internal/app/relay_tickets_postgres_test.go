package app_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/relay"
)

func TestPostgresRustRelayTicketAcrossInstancesAndRestart(t *testing.T) {
	config, db := postgresTestDatabase(t)
	config.JWTSecret = "test-jwt-secret"
	config.RelayRust = relay.RustOptions{AppKey: "test-app", AppSecret: "test-app-secret"}
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func(config app.Config) (*app.App, *httptest.Server) {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return platform, server
	}
	first, firstServer := start(config)
	second, secondServer := start(config)
	owner, err := first.Accounts.SignUp(ctx, "host@example.org", "password", "Host")
	check(err)
	other, err := second.Accounts.SignUp(ctx, "other@example.org", "password", "Other")
	check(err)
	refresh, err := first.Accounts.(*auth.JWTAccounts).SignInJWT(ctx, owner.User.Email, "password")
	check(err)
	access, accepted, err := first.Accounts.Refresh(ctx, refresh.Token)
	check(err)
	if !accepted {
		t.Fatal("initial access token was rejected")
	}
	issue := func(server *httptest.Server, bearer string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/device/qrcode", nil)
		check(err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := server.Client().Do(req)
		check(err)
		defer res.Body.Close()
		var body struct{ Status, URL string }
		check(json.NewDecoder(res.Body).Decode(&body))
		if res.StatusCode != http.StatusOK || body.Status != "200" || body.URL == "" {
			t.Fatalf("ticket issuance = %d, %+v", res.StatusCode, body)
		}
		parsed, err := url.Parse(body.URL)
		check(err)
		return strings.TrimPrefix(parsed.Path, "/api/connector/")
	}
	verify := func(server *httptest.Server, path string, want int) {
		t.Helper()
		res, err := server.Client().Get(server.URL + "/relay/verify/" + path)
		check(err)
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("ticket verify returned %d, want %d", res.StatusCode, want)
		}
		if want != http.StatusOK {
			return
		}
		var body struct {
			Status string
			Data   struct {
				AppKey string `json:"app_key"`
				UID    int64
			}
		}
		check(json.NewDecoder(res.Body).Decode(&body))
		if body.Status != "200" || body.Data.AppKey != config.RelayRust.AppKey || body.Data.UID != owner.User.ID {
			t.Fatalf("verified ticket = %+v, want configured app identity and owner", body)
		}
	}
	dial := func(server *httptest.Server, path string, want int) *websocket.Conn {
		t.Helper()
		address := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/connector/" + path
		dialer := websocket.Dialer{Subprotocols: []string{"devicerelayer"}, HandshakeTimeout: 3 * time.Second}
		conn, res, err := dialer.DialContext(ctx, address, nil)
		if want != http.StatusSwitchingProtocols {
			if res != nil {
				defer res.Body.Close()
			}
			if err == nil || res == nil || res.StatusCode != want {
				t.Fatalf("invalid ticket upgrade accepted = %t, want %d", err == nil, want)
			}
			return nil
		}
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
		var greeting struct{ Cmd string }
		check(conn.ReadJSON(&greeting))
		if greeting.Cmd != "user" {
			t.Fatalf("greeting = %+v, want the Rust device user frame", greeting)
		}
		return conn
	}

	jwtPath := issue(firstServer, access.Token)
	opaquePath := issue(firstServer, owner.Token)
	verify(secondServer, jwtPath, http.StatusOK)
	verify(secondServer, opaquePath, http.StatusOK)
	if repeated := issue(secondServer, access.Token); repeated != jwtPath {
		t.Fatal("issuing via peer replaced an unexpired ticket")
	}
	firstServer.Close()
	first.Close()
	first, firstServer = start(config)
	verify(firstServer, jwtPath, http.StatusOK)
	conn := dial(secondServer, jwtPath, http.StatusSwitchingProtocols)
	// Browser and device are admitted separately with one ticket and use
	// different relay instances without either opening a live room.
	browserDialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 3 * time.Second}
	browserURL := "ws" + strings.TrimPrefix(firstServer.URL, "http") + "/api/connector/" + jwtPath
	browser, res, err := browserDialer.DialContext(ctx, browserURL, nil)
	if err != nil && res != nil {
		res.Body.Close()
	}
	check(err)
	defer browser.Close()
	check(browser.SetReadDeadline(time.Now().Add(5 * time.Second)))
	otherDevice := dial(secondServer, issue(firstServer, other.Token), http.StatusSwitchingProtocols)
	check(conn.WriteJSON(map[string]any{"cmd": "devices", "data": []map[string]any{
		{"id": "cross-instance-toy", "pid": "test-toy", "status": "connected", "battery": 96},
	}}))
	var connected bool
	for {
		var frame struct {
			Content string
			Queue   string
			Payload struct {
				Cmd  string
				Data json.RawMessage
			}
		}
		check(browser.ReadJSON(&frame))
		if frame.Queue != strconv.FormatInt(owner.User.ID, 10) {
			t.Fatalf("device signal escaped its account: %+v", frame)
		}
		connected = connected || frame.Payload.Cmd == "connector_connected"
		if frame.Payload.Cmd == "devices" && strings.Contains(string(frame.Payload.Data), "cross-instance-toy") {
			if !connected {
				t.Fatal("cross-instance device state arrived before connector presence")
			}
			break
		}
	}
	control := map[string]any{
		"cmd": "vibrate", "data": map[string]any{"device": "", "primary": 27, "secondary": 9, "duration": 4},
	}
	check(browser.WriteJSON(map[string]any{"content": "control", "queue": strconv.FormatInt(other.User.ID, 10), "payload": control}))
	check(browser.WriteJSON(map[string]any{"content": "control", "payload": control}))
	var vibration struct {
		Cmd  string
		Data struct {
			Device  string
			Primary int
		}
	}
	check(conn.ReadJSON(&vibration))
	if vibration.Cmd != "vibrate" || vibration.Data.Device != "cross-instance-toy" || vibration.Data.Primary != 27 {
		t.Fatalf("device command from other instance = %+v", vibration)
	}
	wave := strings.Repeat(`{"primary":27,"secondary":9},`, 900)
	check(browser.WriteJSON(map[string]any{"content": "control", "payload": map[string]any{
		"cmd": "mode_vibrate_v2", "data": map[string]any{"device": "cross-instance-toy", "wave": wave, "repeat": 1, "duration": 5},
	}}))
	var waveform struct {
		Cmd  string
		Data struct{ Wave string }
	}
	check(conn.ReadJSON(&waveform))
	if waveform.Cmd != "mode_vibrate_v2" || waveform.Data.Wave != wave {
		t.Fatalf("cross-instance mode command %q carried %d bytes, want %d", waveform.Cmd, len(waveform.Data.Wave), len(wave))
	}
	check(otherDevice.WriteJSON(map[string]string{"cmd": "heartbeat"}))
	var unrelated struct{ Cmd string }
	check(otherDevice.ReadJSON(&unrelated))
	if unrelated.Cmd != "heartbeat" {
		t.Fatalf("device in another account received %+v, want its heartbeat", unrelated)
	}

	wrongApp := config
	wrongApp.RelayRust.AppKey = "other-app"
	_, wrongAppServer := start(wrongApp)
	verify(wrongAppServer, jwtPath, http.StatusForbidden)
	wrongSecret := config
	wrongSecret.RelayRust.AppSecret = "rotated-secret"
	_, wrongSecretServer := start(wrongSecret)
	verify(wrongSecretServer, jwtPath, http.StatusForbidden)

	opaqueToken, _, _ := strings.Cut(opaquePath, "/")
	_, err = db.Exec(ctx, `UPDATE relay_tickets SET expires_at=statement_timestamp()-interval '1 second' WHERE token=$1`, opaqueToken)
	check(err)
	verify(secondServer, opaquePath, http.StatusForbidden)
	dial(secondServer, opaquePath, http.StatusForbidden)
	if replacement := issue(secondServer, owner.Token); replacement == opaquePath {
		t.Fatal("expired ticket was not replaced")
	}
	check(first.Accounts.Revoke(ctx, access.Token))
	_, _, err = conn.ReadMessage()
	var closeError *websocket.CloseError
	if !errors.As(err, &closeError) || closeError.Code != websocket.CloseNormalClosure || closeError.Text != "SERVICE_LOGOUT" {
		t.Fatalf("relay after JWT logout = %v, want 1000 SERVICE_LOGOUT", err)
	}
	verify(secondServer, jwtPath, http.StatusForbidden)
	dial(secondServer, jwtPath, http.StatusForbidden)
	verify(firstServer, issue(secondServer, owner.Token), http.StatusOK)
	check(second.Accounts.Revoke(ctx, owner.Token))
}
