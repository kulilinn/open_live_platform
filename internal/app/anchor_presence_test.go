package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"open_live_platform/internal/app"
)

func TestH5AnchorPresenceKeepsBroadcastAndViewerConnected(t *testing.T) {
	ctx := t.Context()
	const timeout = 5 * time.Second
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	platform, err := app.New(ctx, app.Config{RelaySecret: "anchor-presence-test"})
	check(err)
	t.Cleanup(platform.Close)
	owner, err := platform.Accounts.SignUp(ctx, "presence-owner@example.test", "password", "Owner")
	check(err)
	viewer, err := platform.Accounts.SignUp(ctx, "presence-viewer@example.test", "password", "Viewer")
	check(err)

	// Wait for the relay's disconnect handling before testing the remaining connection.
	const socketHeader = "X-Test-Socket"
	finished := make(chan string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platform.Server.Handler.ServeHTTP(w, r)
		if name := r.Header.Get(socketHeader); name != "" {
			select {
			case finished <- name:
			case <-ctx.Done():
			}
		}
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = timeout
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
			data, _ := io.ReadAll(res.Body)
			t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, status, data)
		}
		if result != nil {
			check(json.NewDecoder(res.Body).Decode(result))
		}
	}
	request(http.MethodPost, "/api/live/room/start", owner.Token, `{"title":"Live"}`, http.StatusOK, nil)
	initial, err := platform.Rooms.Info(ctx, owner.User.ID)
	check(err)
	roomID := strconv.FormatInt(initial.ID, 10)
	viewerID := strconv.FormatInt(viewer.User.ID, 10)
	joinPath := "/api/live/room/join/" + roomID
	request(http.MethodPost, joinPath, viewer.Token, "", http.StatusNoContent, nil)

	connect := func(token, name string) *websocket.Conn {
		t.Helper()
		var ticket struct{ URL string }
		request(http.MethodGet, "/api/device/qrcode", token, "", http.StatusOK, &ticket)
		dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: timeout}
		conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, http.Header{socketHeader: {name}})
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(timeout)))
		var greeting struct{ Content string }
		check(conn.ReadJSON(&greeting))
		if conn.Subprotocol() != "h5relayer" || greeting.Content != "control" {
			t.Fatalf("%s H5 greeting = %+v, subprotocol = %q", name, greeting, conn.Subprotocol())
		}
		return conn
	}
	disconnect := func(conn *websocket.Conn, name string) {
		t.Helper()
		check(conn.Close())
		select {
		case got := <-finished:
			if got != name {
				t.Fatalf("%s relay finished while waiting for %s", got, name)
			}
		case <-time.After(timeout):
			t.Fatalf("%s relay did not finish", name)
		}
	}

	watcher := connect(viewer.Token, "viewer")
	first := connect(owner.Token, "first anchor")
	second := connect(owner.Token, "second anchor")

	type payload struct {
		ID, Timestamp, Sender, Room, Channel string
		Priority                             *int
		Message                              struct {
			Type     string `json:"msgType"`
			Group    string `json:"msgGroup"`
			Text     string `json:"message"`
			UserInfo map[string]json.RawMessage
		}
	}
	type frame struct {
		Content, Queue string
		Payload        []payload
	}
	checkFrame := func(f frame, channel, sender, msgType, message string) payload {
		t.Helper()
		if f.Content != "live_chat" || f.Queue != roomID || len(f.Payload) != 1 {
			t.Fatalf("frame for %s %q = %+v", channel, message, f)
		}
		p := f.Payload[0]
		if p.Channel != channel || p.Sender != sender || p.Room != roomID ||
			p.Message.Type != msgType || p.Message.Group != "chat" || p.Message.Text != message {
			t.Fatalf("payload = %+v, want %s %s %q from %s", p, channel, msgType, message, sender)
		}
		return p
	}
	readFrame := func(channel, sender, msgType, message string) payload {
		t.Helper()
		check(watcher.SetReadDeadline(time.Now().Add(timeout)))
		var f frame
		if err := watcher.ReadJSON(&f); err != nil {
			t.Fatalf("read viewer %s %q: %v", channel, message, err)
		}
		return checkFrame(f, channel, sender, msgType, message)
	}
	expectStatus := func(value string) {
		t.Helper()
		p := readFrame("room_status", roomID, "O", value)
		id, idErr := strconv.ParseInt(p.ID, 10, 64)
		at, atErr := strconv.ParseInt(p.Timestamp, 10, 64)
		if idErr != nil || id <= 0 || atErr != nil || at < initial.Session.StartedAt.Unix() || at > time.Now().Unix() {
			t.Fatalf("room status %s has invalid id %q or timestamp %q", value, p.ID, p.Timestamp)
		}
		user := p.Message.UserInfo
		if p.Priority == nil || *p.Priority != 0 || string(user["uid"]) != roomID || string(user["role"]) != "5" {
			t.Fatalf("room status %s payload = %+v", value, p)
		}
		for _, key := range []string{"avatar", "searchName", "name", "country", "userLevel", "fanLevel"} {
			if string(user[key]) != "null" {
				t.Fatalf("room status %s userInfo.%s = %s, want null", value, key, user[key])
			}
		}
	}
	assertSameBroadcast := func(wantStatus int) {
		t.Helper()
		room, err := platform.Rooms.Info(ctx, initial.ID)
		check(err)
		if !room.Online || room.MemberCount != 1 || room.Session.LiveID != initial.Session.LiveID ||
			!room.Session.StartedAt.Equal(initial.Session.StartedAt) || !room.Session.EndedAt.IsZero() {
			t.Fatalf("expected same broadcast with viewer still present; room = %+v, initial = %+v", room, initial)
		}
		var info struct {
			Online bool
			Data   struct {
				Online int
				LiveID string
			}
		}
		request(http.MethodGet, "/api/live/room/info/"+roomID, viewer.Token, "", http.StatusOK, &info)
		if !info.Online || info.Data.Online != wantStatus || info.Data.LiveID != strconv.FormatInt(initial.Session.LiveID, 10) {
			t.Fatalf("room info = %+v, want status %d on broadcast %d", info, wantStatus, initial.Session.LiveID)
		}
		var online struct{ Data struct{ Online int } }
		request(http.MethodGet, "/api/live/room/online", owner.Token, "", http.StatusOK, &online)
		if online.Data.Online != wantStatus {
			t.Fatalf("owner online status = %d, want %d", online.Data.Online, wantStatus)
		}
	}
	viewerChat := func(id, text string) payload {
		t.Helper()
		check(watcher.SetWriteDeadline(time.Now().Add(timeout)))
		message := fmt.Sprintf(`{"content":"live_chat","queue":%q,"payload":[{"id":%q,"channel":"chat","room":%q,"priority":0,"sender":%q,"timestamp":%q,"message":{"msgType":"C","msgGroup":"chat","message":%q}}]}`,
			roomID, id, roomID, viewerID, strconv.FormatInt(time.Now().Unix(), 10), text)
		check(watcher.WriteMessage(websocket.TextMessage, []byte(message)))
		return readFrame("chat", viewerID, "C", text)
	}

	request(http.MethodPost, "/api/live/room/chat", owner.Token,
		fmt.Sprintf(`{"room_id":%s,"text":"before disconnect"}`, roomID), http.StatusNoContent, nil)
	before := readFrame("chat", roomID, "C", "before disconnect")
	disconnect(first, "first anchor")
	assertSameBroadcast(1)
	withSecond := viewerChat("78591199061446656", "one anchor connection remains")
	disconnect(second, "second anchor")
	expectStatus("3")
	assertSameBroadcast(3)
	whileAway := viewerChat("78591199061446657", "while anchor is away")

	var joined struct {
		Data struct {
			StartTime int64
			Room      struct {
				Online int
				LiveID string
			}
		}
	}
	request(http.MethodPost, joinPath+"?compat=rust", owner.Token, "", http.StatusOK, &joined)
	expectStatus("1")
	assertSameBroadcast(1)
	if joined.Data.Room.Online != 1 || joined.Data.StartTime != initial.Session.StartedAt.Unix() ||
		joined.Data.Room.LiveID != strconv.FormatInt(initial.Session.LiveID, 10) {
		t.Fatalf("owner HTTP join = %+v, want resumed broadcast %+v", joined, initial.Session)
	}

	third := connect(owner.Token, "third anchor")
	disconnect(third, "third anchor")
	expectStatus("3")
	connect(owner.Token, "reconnected anchor")
	expectStatus("1")
	assertSameBroadcast(1)

	var history struct{ Data []frame }
	request(http.MethodPost, "/api/live/room/messages", viewer.Token, fmt.Sprintf(`{"id":%q}`, roomID), http.StatusOK, &history)
	wantHistory := []payload{whileAway, withSecond, before}
	if len(history.Data) != len(wantHistory) {
		t.Fatalf("history = %+v, want %d chat messages", history, len(wantHistory))
	}
	for i, want := range wantHistory {
		p := checkFrame(history.Data[i], "chat", want.Sender, "C", want.Message.Text)
		if p.ID != want.ID || p.Timestamp != want.Timestamp {
			t.Fatalf("history message %d identity = (%q, %q), want (%q, %q)", i, p.ID, p.Timestamp, want.ID, want.Timestamp)
		}
	}
}
