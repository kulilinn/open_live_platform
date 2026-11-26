package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

func TestRustRelayTicketsAndDeviceInteroperability(t *testing.T) {
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	accounts := auth.NewStore()
	owner, err := accounts.SignUp(ctx, "owner@example.test", "password", "Owner")
	check(err)
	other, err := accounts.SignUp(ctx, "other@example.test", "password", "Other")
	check(err)
	const appKey, appSecret = "test-app", "test-secret"
	const ttl = 2 * time.Second
	mux := http.NewServeMux()
	Register(mux, accounts, live.NewStore(), "", "", RustOptions{AppKey: appKey, AppSecret: appSecret, TTL: ttl})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	issue := func(token string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/device/qrcode", nil)
		check(err)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := server.Client().Do(req)
		check(err)
		defer res.Body.Close()
		var ticket struct{ Status, URL string }
		check(json.NewDecoder(res.Body).Decode(&ticket))
		if res.StatusCode != http.StatusOK || ticket.Status != "200" || ticket.URL == "" || strings.Contains(ticket.URL, token) {
			t.Fatal("invalid Rust connector ticket response")
		}
		return ticket.URL
	}
	verify := func(path string, status int) {
		t.Helper()
		res, err := server.Client().Get(server.URL + "/relay/verify/" + path)
		check(err)
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("relay ticket verification = %d, want %d", res.StatusCode, status)
		}
		if status == http.StatusOK {
			var info struct {
				Status string
				Data   struct {
					AppKey string `json:"app_key"`
					UID    int64
				}
			}
			check(json.NewDecoder(res.Body).Decode(&info))
			if info.Status != "200" || info.Data.AppKey != appKey || info.Data.UID != owner.User.ID {
				t.Fatalf("relay verify = %+v, want original app and user", info)
			}
		}
	}
	dial := func(address, protocol string) *websocket.Conn {
		t.Helper()
		dialer := websocket.Dialer{Subprotocols: []string{protocol}, HandshakeTimeout: 3 * time.Second}
		conn, response, err := dialer.DialContext(ctx, address, nil)
		if err != nil && response != nil {
			response.Body.Close()
		}
		check(err)
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	readBrowser := func(conn *websocket.Conn, content, name string) deviceFrame {
		t.Helper()
		check(conn.SetReadDeadline(time.Now().Add(3 * time.Second)))
		var frame deviceFrame
		check(conn.ReadJSON(&frame))
		if frame.Content != content || frame.Queue != strconv.FormatInt(owner.User.ID, 10) || frame.Payload.Name != name {
			t.Fatalf("browser frame = %+v, want %s/%s for owner", frame, content, name)
		}
		return frame
	}
	readDevice := func(conn *websocket.Conn, name string) deviceCommand {
		t.Helper()
		check(conn.SetReadDeadline(time.Now().Add(3 * time.Second)))
		var cmd deviceCommand
		check(conn.ReadJSON(&cmd))
		if cmd.Name != name {
			t.Fatalf("device command = %+v, want %s", cmd, name)
		}
		return cmd
	}

	issuedAt := time.Now().Unix()
	address := issue(owner.Token)
	if repeated := issue(owner.Token); repeated != address {
		t.Fatal("valid ticket was replaced by another QR request")
	}
	parsed, err := url.Parse(address)
	check(err)
	path := strings.TrimPrefix(parsed.Path, "/api/connector/")
	token, signature, ok := strings.Cut(path, "/")
	if !ok || len(token) != 32 || len(signature) != 40 {
		t.Fatal("unexpected Rust relay token or signature format")
	}
	var signatureMatched bool
	for ts := issuedAt; ts <= time.Now().Unix(); ts++ {
		message := token + "&" + strconv.FormatInt(ts, 10) + "&" + strconv.FormatInt(owner.User.ID, 10)
		signatureMatched = signatureMatched || signature == rustSignature(appKey, appSecret, message)
	}
	if !signatureMatched {
		t.Fatal("ticket signature does not match the Rust signing inputs")
	}
	verify(path, http.StatusOK)
	verify(token+"/"+signature+"0", http.StatusForbidden)
	h5 := dial(address, "h5relayer")
	readBrowser(h5, "control", "user")
	readBrowser(h5, "device", "devices")
	device := dial(address, "devicerelayer")
	readDevice(device, "user")
	readBrowser(h5, "device", "connector_connected")
	check(device.WriteJSON(makeDeviceCommand("devices", []deviceInfo{{Status: "connected", PID: "p1", ID: "toy-1", Battery: 93}})))
	status := readBrowser(h5, "device", "devices")
	if !strings.Contains(string(status.Payload.Data), "toy-1") {
		t.Fatalf("browser device status = %s, want toy-1", status.Payload.Data)
	}
	refresh, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/device/toyinfo", nil)
	check(err)
	refresh.Header.Set("Authorization", "Bearer "+owner.Token)
	res, err := server.Client().Do(refresh)
	check(err)
	var refreshed struct{ Status string }
	check(json.NewDecoder(res.Body).Decode(&refreshed))
	check(res.Body.Close())
	if res.StatusCode != http.StatusOK || refreshed.Status != "200" {
		t.Fatalf("toy status refresh = %d %+v, want success", res.StatusCode, refreshed)
	}
	readBrowser(h5, "device", "devices")
	check(device.WriteJSON(makeDeviceCommand("joystick", map[string]any{"device": "toy-1", "code": "AXIS_TRIGGER", "event": "motion", "value": 31})))
	readBrowser(h5, "device", "joystick")
	otherDevice := dial(issue(other.Token), "devicerelayer")
	readDevice(otherDevice, "user")
	control := makeDeviceCommand("vibrate", map[string]any{"device": "", "primary": 110, "secondary": 10, "duration": 5})
	check(h5.WriteJSON(deviceFrame{Content: "control", Queue: strconv.FormatInt(other.User.ID, 10), Payload: control}))
	check(h5.WriteJSON(deviceFrame{Content: "control", Payload: control}))
	vibrate := readDevice(device, "vibrate")
	if string(vibrate.Data) != `{"device":"toy-1","primary":100,"secondary":10,"duration":5}` {
		t.Fatalf("default-device vibrate = %s, want clamped command for toy-1", vibrate.Data)
	}
	check(otherDevice.WriteJSON(deviceCommand{Name: "heartbeat"}))
	readDevice(otherDevice, "heartbeat")
	for _, name := range []string{"mode_vibrate", "mode_vibrate_v2", "motor_strength"} {
		check(h5.WriteJSON(deviceFrame{Content: "control", Payload: makeDeviceCommand(name, map[string]any{"device": "toy-1", "index": 2, "duration": 5, "wave_id": 7, "repeat": 1, "min": 10, "max": 80})}))
		readDevice(device, name)
	}
	check(device.Close())
	readBrowser(h5, "device", "devices")
	readBrowser(h5, "device", "connector_disconnected")
	device = dial(address, "devicerelayer")
	readDevice(device, "user")
	readBrowser(h5, "device", "connector_connected")

	// An active socket extends the ticket beyond its original lifetime.
	time.Sleep(ttl + 50*time.Millisecond)
	verify(path, http.StatusOK)
	check(h5.Close())
	check(device.Close())
	check(accounts.Revoke(ctx, owner.Token))
	verify(path, http.StatusForbidden)
}

func TestRustTicketExpiration(t *testing.T) {
	accounts := auth.NewStore()
	login, ok := auth.RelaySessionForToken(accounts, "opaque-session")
	if !ok {
		t.Fatal("could not identify an opaque session")
	}
	now := time.Now()
	registry := newRustTickets(RustOptions{AppKey: "test-app", AppSecret: "test-secret"})
	ticket, err := registry.issue(t.Context(), login, 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := registry.verify(t.Context(), ticket.Token, ticket.signature, now.Add(24*time.Hour-time.Nanosecond)); err != nil || found != ticket {
		t.Fatal("ticket expired before its default TTL")
	}
	if found, err := registry.verify(t.Context(), ticket.Token, ticket.signature, now.Add(24*time.Hour)); err != nil || found != nil {
		t.Fatal("ticket was accepted at its deadline")
	}
	if replacement, err := registry.issue(t.Context(), login, 42, now.Add(24*time.Hour)); err != nil || replacement.Token == ticket.Token {
		t.Fatal("expired ticket was reissued")
	}
}
