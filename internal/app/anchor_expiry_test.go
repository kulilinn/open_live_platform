package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"open_live_platform/internal/live"
)

func TestAnchorAwayTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 5 * time.Minute},
		{"45", 45 * time.Second},
		{"1m", 5 * time.Minute},
		{"0", 5 * time.Minute},
	} {
		t.Setenv("TEMP_OFFLINE_TIMEOUT", tc.value)
		if got := ConfigFromEnv().AnchorAwayTimeout; got != tc.want {
			t.Errorf("TEMP_OFFLINE_TIMEOUT=%q returned %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestServeClosesExpiredAnchorRoomForH5Viewer(t *testing.T) {
	const (
		awayTimeout = 70 * time.Millisecond
		testTimeout = 5 * time.Second
	)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	platform, err := New(ctx, Config{
		RelaySecret:       "anchor-expiry-test",
		AnchorAwayTimeout: awayTimeout,
	})
	check(err)
	t.Cleanup(platform.Close)
	owner, err := platform.Accounts.SignUp(ctx, "expiry-owner@example.test", "password", "Owner")
	check(err)
	viewer, err := platform.Accounts.SignUp(ctx, "expiry-viewer@example.test", "password", "Viewer")
	check(err)

	listen := func() net.Listener {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		t.Cleanup(func() { listener.Close() })
		return listener
	}
	public, internal := listen(), listen()
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- platform.serve(serveCtx, public, internal) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("app did not stop")
		}
	})

	client := &http.Client{Timeout: testTimeout}
	t.Cleanup(client.CloseIdleConnections)
	request := func(method, path, token, body string, wantStatus int, result any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+public.Addr().String()+path, strings.NewReader(body))
		check(err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		check(err)
		defer res.Body.Close()
		if res.StatusCode != wantStatus {
			data, _ := io.ReadAll(res.Body)
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, res.StatusCode, wantStatus, data)
		}
		if result != nil {
			check(json.NewDecoder(res.Body).Decode(result))
		}
	}

	var started live.Room
	request(http.MethodPost, "/api/live/room/start", owner.Token, `{"title":"Live"}`, http.StatusOK, &started)
	roomID := strconv.FormatInt(started.ID, 10)
	initial, err := platform.Rooms.Info(ctx, started.ID)
	check(err)
	request(http.MethodPost, "/api/live/room/join/"+roomID, viewer.Token, "", http.StatusNoContent, nil)

	connect := func(token string) *websocket.Conn {
		t.Helper()
		var ticket struct{ URL string }
		request(http.MethodGet, "/api/device/qrcode", token, "", http.StatusOK, &ticket)
		dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: testTimeout}
		conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(testTimeout)))
		var greeting struct{ Content string }
		check(conn.ReadJSON(&greeting))
		if conn.Subprotocol() != "h5relayer" || greeting.Content != "control" {
			t.Fatalf("H5 greeting = %+v with subprotocol %q", greeting, conn.Subprotocol())
		}
		return conn
	}
	watcher := connect(viewer.Token)
	anchor := connect(owner.Token)

	expectStatus := func(want string) {
		t.Helper()
		var frame struct {
			Content, Queue string
			Payload        []struct {
				Channel, Room, Sender string
				Message               struct {
					Type string `json:"msgType"`
					Text string `json:"message"`
				}
			}
		}
		if err := watcher.ReadJSON(&frame); err != nil {
			t.Fatalf("read H5 room status %s: %v", want, err)
		}
		if frame.Content != "live_chat" || frame.Queue != roomID || len(frame.Payload) != 1 {
			t.Fatalf("frame for room status %s = %+v", want, frame)
		}
		p := frame.Payload[0]
		if p.Channel != "room_status" || p.Room != roomID || p.Sender != roomID || p.Message.Type != "O" || p.Message.Text != want {
			t.Fatalf("H5 payload = %+v, want room status %s", p, want)
		}
	}

	check(watcher.SetReadDeadline(time.Now().Add(testTimeout)))
	disconnectRequested := time.Now()
	check(anchor.Close())
	expectStatus("3")
	expectStatus("0")
	if _, message, err := watcher.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("frame after room close = %s, %v, want normal WebSocket close", message, err)
	}

	closed, err := platform.Rooms.Info(ctx, started.ID)
	check(err)
	if closed.Status() != live.RoomOffline || closed.Session.LiveID != initial.Session.LiveID || closed.MemberCount != 0 ||
		closed.Session.EndedAt.Before(disconnectRequested.Add(awayTimeout)) {
		t.Fatalf("closed room = %+v, want broadcast %d ended at or after %v", closed, initial.Session.LiveID, disconnectRequested.Add(awayTimeout))
	}
}
