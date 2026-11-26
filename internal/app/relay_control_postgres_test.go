package app_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/relay"
)

func TestPostgresRustAppControlRequiresAccountBindingOrConsent(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	const appKey, appSecret, appID = "test-app", "test-app-secret", "42"
	config.RelayRust = relay.RustOptions{AppID: 42, AppKey: appKey, AppSecret: appSecret, AppUsers: []int64{1}}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func() (*app.App, *httptest.Server) {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return platform, server
	}
	first, firstServer := start()
	_, secondServer := start()
	owner, err := first.Accounts.SignUp(ctx, "device-owner@example.org", "password", "Owner")
	check(err)
	controller, err := first.Accounts.SignUp(ctx, "controller@example.org", "password", "Controller")
	check(err)
	ownerID, controllerID := strconv.FormatInt(owner.User.ID, 10), strconv.FormatInt(controller.User.ID, 10)
	if owner.User.ID != 1 {
		t.Fatalf("owner ID = %d, want database fixture ID 1", owner.User.ID)
	}
	type response struct{ Status, URL string }
	post := func(method, route, token string, payload any, want int) response {
		t.Helper()
		data, err := json.Marshal(payload)
		check(err)
		req, err := http.NewRequestWithContext(ctx, method, firstServer.URL+route, bytes.NewReader(data))
		check(err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		result, err := firstServer.Client().Do(req)
		check(err)
		defer result.Body.Close()
		var body response
		check(json.NewDecoder(result.Body).Decode(&body))
		if result.StatusCode != want {
			t.Fatalf("%s returned %d (%s), want %d", route, result.StatusCode, body.Status, want)
		}
		return body
	}
	acquire := func(id, bearer string, offset int64, want int) response {
		t.Helper()
		ts := strconv.FormatInt(time.Now().Unix()+offset, 10)
		mac := hmac.New(sha1.New, []byte(appKey+appSecret))
		mac.Write([]byte(appID + "&" + id + "&" + ts))
		return post(http.MethodPost, "/api/connector/acquire", bearer, map[string]any{
			"appId": appID, "instanceId": id, "timestamp": ts, "signature": hex.EncodeToString(mac.Sum(nil)), "isStaff": true,
		}, want)
	}
	if stale := acquire(ownerID, "", -11, http.StatusOK); stale.Status != "4017" {
		t.Fatalf("stale signed acquisition = %+v, want 4017", stale)
	}
	acquire(controllerID, "", 0, http.StatusForbidden)
	ownerTicket := acquire(ownerID, "", 0, http.StatusOK)
	controllerTicket := acquire(controllerID, controller.Token, 0, http.StatusOK)
	if ownerTicket.URL == "" || controllerTicket.URL == "" {
		t.Fatal("successful external acquisition did not issue URLs")
	}
	if repeat := acquire(ownerID, "", 0, http.StatusOK); repeat.URL == ownerTicket.URL {
		t.Fatal("external acquire reused its previous grant URL")
	}
	dial := func(server *httptest.Server, address, protocol string) *websocket.Conn {
		t.Helper()
		parsed, err := url.Parse(address)
		check(err)
		dialer := websocket.Dialer{Subprotocols: []string{protocol}, HandshakeTimeout: 3 * time.Second}
		conn, res, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+parsed.Path, nil)
		if err != nil && res != nil {
			res.Body.Close()
		}
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
		return conn
	}
	device := dial(secondServer, ownerTicket.URL, "devicerelayer")
	var greeting struct {
		Cmd  string
		Data struct{ Debug bool }
	}
	check(device.ReadJSON(&greeting))
	if greeting.Cmd != "user" || !greeting.Data.Debug {
		t.Fatalf("enrolled app device greeting = %+v, want isStaff debug", greeting)
	}
	browser := dial(firstServer, controllerTicket.URL, "h5relayer")
	if _, _, err := browser.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	controllerDevice := dial(secondServer, controllerTicket.URL, "devicerelayer")
	check(controllerDevice.ReadJSON(&greeting))
	readDevice := func(peer *websocket.Conn, want string) {
		t.Helper()
		var result struct{ Cmd string }
		check(peer.ReadJSON(&result))
		if result.Cmd != want {
			t.Fatalf("device received %s, want %s", result.Cmd, want)
		}
	}
	control := map[string]any{"cmd": "mode_vibrate", "data": map[string]any{"device": "test-device", "index": 2, "duration": 3}}
	vibrate := map[string]any{"appId": appID, "instanceId": ownerID, "appSecret": appSecret, "payload": control}
	if body := post(http.MethodPost, "/api/connector/vibrate", "", vibrate, http.StatusOK); body.Status != "200" {
		t.Fatalf("enrolled application vibrate returned %+v", body)
	}
	readDevice(device, "mode_vibrate")
	vibrate["instanceId"] = controllerID
	post(http.MethodPost, "/api/connector/vibrate", "", vibrate, http.StatusForbidden)
	post(http.MethodPost, "/api/connector/vibrate", owner.Token, vibrate, http.StatusForbidden)
	frame := map[string]any{"content": "control", "queue": ownerID, "payload": control}
	check(browser.WriteJSON(frame))
	grant := map[string]any{"appId": appID, "controller": controllerID, "controlled": ownerID, "appSecret": appSecret, "duplex": true, "expiry": 60}
	post(http.MethodPost, "/api/connector/remote", "", grant, http.StatusForbidden)
	post(http.MethodPost, "/api/connector/remote", controller.Token, grant, http.StatusForbidden)
	// A heartbeat barrier on the same browser proves the pre-consent command was processed.
	check(browser.WriteJSON(map[string]any{"content": "device", "payload": map[string]string{"cmd": "heartbeat"}}))
	for {
		var event struct{ Payload struct{ Cmd string } }
		check(browser.ReadJSON(&event))
		if event.Payload.Cmd == "heartbeat" {
			break
		}
	}
	check(device.WriteJSON(map[string]string{"cmd": "heartbeat"}))
	readDevice(device, "heartbeat")
	if body := post(http.MethodPost, "/api/connector/remote", owner.Token, grant, http.StatusOK); body.Status != "200" {
		t.Fatalf("remote consent returned %+v", body)
	}
	check(browser.WriteJSON(frame))
	readDevice(device, "mode_vibrate")
	readDevice(controllerDevice, "mode_vibrate")
	_, err = db.Exec(ctx, `UPDATE relay_control_grants SET expires_at=statement_timestamp()-interval '1 second'`)
	check(err)
	check(browser.WriteJSON(frame))
	delete(frame, "queue")
	check(browser.WriteJSON(frame))
	readDevice(controllerDevice, "mode_vibrate")
	check(device.WriteJSON(map[string]string{"cmd": "heartbeat"}))
	readDevice(device, "heartbeat")
	grant["expiry"] = 0
	post(http.MethodPost, "/api/connector/remote", owner.Token, grant, http.StatusOK)
	var remaining int
	check(db.QueryRow(ctx, `SELECT count(*) FROM relay_control_grants`).Scan(&remaining))
	if remaining != 0 {
		t.Fatalf("revoked grant left %d rows", remaining)
	}
}
