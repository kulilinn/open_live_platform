package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
)

func TestRoomOwnerLeaveClosesH5Viewers(t *testing.T) {
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	platform, err := app.New(ctx, app.Config{RelaySecret: "room-close-test"})
	check(err)
	t.Cleanup(platform.Close)
	owner, err := platform.Accounts.SignUp(ctx, "host@example.test", "password", "Host")
	check(err)
	first, err := platform.Accounts.SignUp(ctx, "first@example.test", "password", "First")
	check(err)
	second, err := platform.Accounts.SignUp(ctx, "second@example.test", "password", "Second")
	check(err)

	server := httptest.NewServer(platform.Server.Handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(method, path, token, body string, status int, result any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		check(err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		check(err)
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("%s %s returned %d, want %d", method, path, res.StatusCode, status)
		}
		if result != nil {
			check(json.NewDecoder(res.Body).Decode(result))
		}
	}

	var room live.Room
	request(http.MethodPost, "/api/live/room/start", owner.Token, `{"title":"Live"}`, http.StatusOK, &room)
	roomID := strconv.FormatInt(room.ID, 10)
	ownerID := strconv.FormatInt(owner.User.ID, 10)
	for _, token := range []string{first.Token, second.Token} {
		request(http.MethodPost, "/api/live/room/join/"+roomID, token, "", http.StatusNoContent, nil)
	}

	connect := func(token, protocol string) *websocket.Conn {
		t.Helper()
		var ticket struct{ URL string }
		request(http.MethodGet, "/api/device/qrcode", token, "", http.StatusOK, &ticket)
		dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		if protocol != "" {
			dialer.Subprotocols = []string{protocol}
		}
		conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
		check(err)
		t.Cleanup(func() { conn.Close() })
		if conn.Subprotocol() != protocol {
			t.Fatalf("subprotocol = %q, want %q", conn.Subprotocol(), protocol)
		}
		if protocol == "h5relayer" {
			check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
			var greeting struct{ Content string }
			check(conn.ReadJSON(&greeting))
			if greeting.Content != "control" {
				t.Fatalf("H5 greeting = %+v, want control", greeting)
			}
		}
		return conn
	}
	viewers := []*websocket.Conn{connect(first.Token, "h5relayer"), connect(second.Token, "h5relayer")}
	legacy := connect(owner.Token, "")

	closedAfter := time.Now().Unix()
	request(http.MethodPost, "/api/live/room/leave/"+roomID, owner.Token, "", http.StatusNoContent, nil)

	expectNormalClose := func(conn *websocket.Conn) {
		t.Helper()
		if _, data, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			t.Fatalf("frame after room close = %s, %v, want normal WebSocket close", data, err)
		}
	}
	var id, timestamp string
	for index, conn := range viewers {
		check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
		var frame struct {
			Content, Queue string
			Payload        []struct {
				ID, Timestamp, Sender, Room, Channel string
				Priority                             *int
				Message                              struct {
					Type     string `json:"msgType"`
					Group    string `json:"msgGroup"`
					Text     string `json:"message"`
					UserInfo map[string]json.RawMessage
				}
			}
		}
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("viewer %d read room status: %v", index+1, err)
		}
		if frame.Content != "live_chat" || frame.Queue != roomID || len(frame.Payload) != 1 {
			t.Fatalf("viewer %d room status frame = %+v", index+1, frame)
		}
		p := frame.Payload[0]
		if p.Channel != "room_status" || p.Sender != ownerID || p.Room != roomID || p.Priority == nil || *p.Priority != 0 ||
			p.Message.Type != "O" || p.Message.Group != "chat" || p.Message.Text != "0" {
			t.Fatalf("viewer %d room status payload = %+v", index+1, p)
		}
		user := p.Message.UserInfo
		if string(user["uid"]) != ownerID || string(user["role"]) != "5" {
			t.Fatalf("viewer %d room status user ID = %s, role = %s", index+1, user["uid"], user["role"])
		}
		for _, key := range []string{"avatar", "searchName", "name", "country", "userLevel", "fanLevel"} {
			if string(user[key]) != "null" {
				t.Fatalf("viewer %d room status userInfo.%s = %s, want null", index+1, key, user[key])
			}
		}
		messageID, idErr := strconv.ParseInt(p.ID, 10, 64)
		sentAt, timestampErr := strconv.ParseInt(p.Timestamp, 10, 64)
		if idErr != nil || messageID <= 0 || timestampErr != nil || sentAt < closedAfter || sentAt > time.Now().Unix() {
			t.Fatalf("viewer %d room status id = %q, timestamp = %q", index+1, p.ID, p.Timestamp)
		}
		if index == 0 {
			id, timestamp = p.ID, p.Timestamp
		} else if p.ID != id || p.Timestamp != timestamp {
			t.Fatalf("second viewer got room status id = %q, timestamp = %q, want %q, %q", p.ID, p.Timestamp, id, timestamp)
		}
		expectNormalClose(conn)
	}

	check(legacy.SetReadDeadline(time.Now().Add(5 * time.Second)))
	var event live.Event
	check(legacy.ReadJSON(&event))
	if want := (live.Event{Type: "close", RoomID: room.ID, UserID: owner.User.ID}); event != want {
		t.Fatalf("default WebSocket close event = %+v, want %+v", event, want)
	}
	expectNormalClose(legacy)
}
