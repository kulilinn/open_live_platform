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

func TestConnectorAuthenticatesAndRelaysRoomEvents(t *testing.T) {
	const secret = "relay-test-secret"
	accounts := auth.NewStore()
	owner, err := accounts.SignUp(t.Context(), "owner@example.com", "password", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(t.Context(), "viewer@example.com", "password", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	rooms := live.NewStore()
	room, err := rooms.Start(t.Context(), owner.User.ID, "Test Room", "invite")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, accounts, rooms, secret, "")
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/connector/"
	issue := func(token string, want int) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/device/qrcode", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("issue ticket status = %d, want %d", res.StatusCode, want)
		}
		if want != http.StatusOK {
			return ""
		}
		var data struct{ Status, URL string }
		if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		if data.Status != "200" || !strings.HasPrefix(data.URL, base+"ws-") || strings.Contains(data.URL, token) {
			t.Fatal("invalid connector ticket response")
		}
		return data.URL
	}
	denyUpgrade := func(address string) {
		t.Helper()
		conn, res, err := websocket.DefaultDialer.DialContext(t.Context(), address, nil)
		if conn != nil {
			conn.Close()
		}
		if res != nil {
			defer res.Body.Close()
		}
		if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Fatal("expected the connector to reject invalid credentials with 403")
		}
	}
	issue("", http.StatusUnauthorized)
	issue("invalid-token", http.StatusUnauthorized)
	denyUpgrade(base + viewer.Token + "/bad")
	if err := rooms.Join(t.Context(), room.ID, viewer.User.ID, "wrong"); err != live.ErrInvalidRoomPassword {
		t.Fatalf("join with wrong password = %v", err)
	}
	denyUpgrade(issue(viewer.Token, http.StatusOK) + "?room_id=" + strconv.FormatInt(room.ID, 10))
	if err := rooms.Join(t.Context(), room.ID, viewer.User.ID, "invite"); err != nil {
		t.Fatal(err)
	}

	connect := func(address string) *websocket.Conn {
		t.Helper()
		headers := http.Header{"Origin": {"https://client.example.com"}}
		conn, _, err := websocket.DefaultDialer.DialContext(t.Context(), address, headers)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		return conn
	}
	read := func(conn *websocket.Conn, want live.Event) {
		t.Helper()
		var got live.Event
		if err := conn.ReadJSON(&got); err != nil || got != want {
			t.Fatalf("event = %+v, %v, want %+v", got, err, want)
		}
	}

	ownerURL := issue(owner.Token, http.StatusOK)
	viewerURL := issue(viewer.Token, http.StatusOK)
	u, err := url.Parse(viewerURL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/api/connector/"), "/")
	if len(parts) != 2 || parts[1] == "" {
		t.Fatal("expected ticket and signature in connector URL")
	}
	issue(parts[0], http.StatusUnauthorized)
	denyUpgrade(base + parts[0] + "/bad")
	creator := connect(ownerURL)
	audience := connect(viewerURL + "?hash=/live/" + strconv.FormatInt(room.ID, 10))
	denyUpgrade(ownerURL)
	newPassword := "updated invite"
	if _, err := rooms.UpdateSettings(t.Context(), owner.User.ID, nil, &newPassword); err != nil {
		t.Fatal(err)
	}
	if err := audience.WriteJSON(command{Type: "chat", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	chat := live.Event{Type: "chat", RoomID: room.ID, UserID: viewer.User.ID, Text: "hello"}
	read(creator, chat)
	read(audience, chat)

	if err := creator.WriteJSON(command{Type: "kick", TargetID: viewer.User.ID}); err != nil {
		t.Fatal(err)
	}
	kick := live.Event{Type: "kick", RoomID: room.ID, UserID: owner.User.ID, TargetID: viewer.User.ID}
	read(creator, kick)
	read(audience, kick)
	if _, _, err := audience.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("kicked viewer connection closed with %v", err)
	}
	// Existing integrations may still present a server-signed session URL.
	connect(base + owner.Token + "/" + Signature(owner.Token, secret))
}
