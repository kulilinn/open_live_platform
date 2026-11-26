package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"open_live_platform/internal/app"
)

func TestH5ChatIdentityMatchesHTTPHistory(t *testing.T) {
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	platform, err := app.New(ctx, app.Config{RelaySecret: "chat-identity-test"})
	check(err)
	t.Cleanup(platform.Close)
	owner, err := platform.Accounts.SignUp(ctx, "host@example.test", "password", "Host")
	check(err)
	viewer, err := platform.Accounts.SignUp(ctx, "viewer@example.test", "password", "Viewer")
	check(err)
	room, err := platform.Rooms.Start(ctx, owner.User.ID, "Live", "")
	check(err)
	check(platform.Rooms.Join(ctx, room.ID, viewer.User.ID, ""))
	roomID := strconv.FormatInt(room.ID, 10)
	viewerID := strconv.FormatInt(viewer.User.ID, 10)

	server := httptest.NewServer(platform.Server.Handler)
	t.Cleanup(server.Close)
	request := func(method, path, token, body string, status int, result any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		check(err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := server.Client().Do(req)
		check(err)
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("%s %s returned %d, want %d", method, path, res.StatusCode, status)
		}
		if result != nil {
			check(json.NewDecoder(res.Body).Decode(result))
		}
	}
	connect := func(token string) *websocket.Conn {
		t.Helper()
		var ticket struct{ URL string }
		request(http.MethodGet, "/api/device/qrcode", token, "", http.StatusOK, &ticket)
		dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}}
		conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
		var greeting struct{ Content string }
		check(conn.ReadJSON(&greeting))
		if conn.Subprotocol() != "h5relayer" || greeting.Content != "control" {
			t.Fatalf("H5 greeting = %+v, subprotocol = %q", greeting, conn.Subprotocol())
		}
		return conn
	}
	subscribers := []*websocket.Conn{connect(owner.Token), connect(viewer.Token)}

	type chatFrame struct {
		Content, Queue string
		Payload        []struct {
			ID, Timestamp, Sender, Room, Channel string
			Message                              struct {
				Text      string `json:"message"`
				Extension json.RawMessage
				UserInfo  struct {
					UID, Name string
					Role      *int
				}
			}
		}
	}
	assertChat := func(frame chatFrame, id, timestamp, text, name string, history bool) {
		t.Helper()
		if frame.Content != "live_chat" || frame.Queue != roomID || len(frame.Payload) != 1 {
			t.Fatalf("chat frame = %+v", frame)
		}
		p := frame.Payload[0]
		if p.ID != id || p.Timestamp != timestamp || p.Sender != viewerID || p.Room != roomID || p.Channel != "chat" ||
			p.Message.Text != text || p.Message.UserInfo.UID != viewerID || p.Message.UserInfo.Name != name {
			t.Fatalf("chat payload = %+v, want %q by %q with ID %q at %q", p, text, name, id, timestamp)
		}
		if role := p.Message.UserInfo.Role; history && (role == nil || *role != 2) || !history && role != nil {
			t.Fatalf("chat user fields = %+v (HTTP history = %t)", p.Message.UserInfo, history)
		}
	}
	const chatPath = "/api/live/room/chat"
	request(http.MethodPost, chatPath, viewer.Token, fmt.Sprintf(`{"room_id":%s,"text":"Go chat"}`, roomID), http.StatusNoContent, nil)
	stored, err := platform.Rooms.ChatHistory(ctx, room.ID, viewer.User.ID, 0, 1)
	check(err)
	if len(stored) != 1 || stored[0].ID < 1 || stored[0].CreatedAt.IsZero() {
		t.Fatalf("persisted Go chat = %+v", stored)
	}
	id, timestamp := strconv.FormatInt(stored[0].ID, 10), strconv.FormatInt(stored[0].CreatedAt.Unix(), 10)
	for _, conn := range subscribers {
		var frame chatFrame
		check(conn.ReadJSON(&frame))
		assertChat(frame, id, timestamp, "Go chat", "Viewer", false)
	}

	const rustID, rustTimestamp = "78591199061446657", "1760000000"
	const rustExtension = `{"counter":9007199254740993}`
	var ack struct {
		Data  bool
		MsgID json.RawMessage
	}
	request(http.MethodPost, chatPath, viewer.Token, fmt.Sprintf(`{
		"id":%q,"channel":"chat","room":%q,"priority":0,"sender":"0","timestamp":%q,
		"message":{"message":"Rust chat","userInfo":{"uid":%q,"name":"Original name"},"extension":%s}
	}`, rustID, roomID, rustTimestamp, viewerID, rustExtension), http.StatusOK, &ack)
	if !ack.Data || string(ack.MsgID) != rustID {
		t.Fatalf("Rust chat acknowledgement = %+v", ack)
	}
	assertRustChat := func(frame chatFrame) {
		t.Helper()
		assertChat(frame, rustID, rustTimestamp, "Rust chat", "Original name", false)
		if extension := frame.Payload[0].Message.Extension; string(extension) != rustExtension {
			t.Fatalf("Rust extension = %s, want %s", extension, rustExtension)
		}
	}
	for _, conn := range subscribers {
		var frame chatFrame
		check(conn.ReadJSON(&frame))
		assertRustChat(frame)
	}
	var history struct{ Data []chatFrame }
	request(http.MethodPost, "/api/live/room/messages", viewer.Token, fmt.Sprintf(`{"id":%q}`, roomID), http.StatusOK, &history)
	if len(history.Data) != 2 {
		t.Fatalf("HTTP chat history = %+v", history)
	}
	assertRustChat(history.Data[0])
	assertChat(history.Data[1], id, timestamp, "Go chat", "Viewer", true)
}
