package app_test

import (
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
	"open_live_platform/internal/relay"
)

func TestPostgresAnchorPresenceAcrossRelays(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func() (*app.App, string) {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return platform, "ws" + strings.TrimPrefix(server.URL, "http")
	}
	first, firstURL := start()
	second, secondURL := start()
	owner, err := first.Accounts.SignUp(ctx, "host@example.org", "password", "Host")
	check(err)
	viewer, err := first.Accounts.SignUp(ctx, "viewer@example.org", "password", "Viewer")
	check(err)
	room, err := first.Rooms.Start(ctx, owner.User.ID, "Live", "")
	check(err)
	roomID, viewerID, liveID := room.ID, viewer.User.ID, room.Session.LiveID
	check(first.Rooms.Join(ctx, roomID, viewerID, ""))
	const originalChat = "before the anchor disconnects"
	check(first.Rooms.Send(ctx, roomID, viewerID, originalChat))
	firstEvents, unsubFirst, err := first.Rooms.Subscribe(ctx, roomID, viewerID)
	check(err)
	t.Cleanup(unsubFirst)
	secondEvents, unsubSecond, err := second.Rooms.Subscribe(ctx, roomID, viewerID)
	check(err)
	t.Cleanup(unsubSecond)

	connectAnchor := func(serverURL string) *websocket.Conn {
		t.Helper()
		dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 5 * time.Second}
		url := serverURL + "/api/connector/" + owner.Token + "/" + relay.Signature(owner.Token, config.RelaySecret) + "?room_id=" + strconv.FormatInt(roomID, 10)
		conn, _, err := dialer.DialContext(ctx, url, nil)
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
		var greeting struct{ Content string }
		check(conn.ReadJSON(&greeting))
		if greeting.Content != "control" {
			t.Fatalf("anchor greeting = %+v, want control", greeting)
		}
		return conn
	}
	assertStatus := func(want live.RoomStatus) {
		t.Helper()
		for _, store := range []live.RoomStore{first.Rooms, second.Rooms} {
			got, err := store.Info(ctx, roomID)
			check(err)
			if got.Status() != want || !got.Online || got.Session != room.Session || got.MemberCount != 1 {
				t.Fatalf("room = %+v, want status %d with the original session and viewer", got, want)
			}
		}
	}
	expectStatus := func(want live.RoomStatus) {
		t.Helper()
		var createdAt time.Time
		for _, events := range []<-chan live.Event{firstEvents, secondEvents} {
			select {
			case event := <-events:
				if event.Type != "room_status" || event.RoomID != roomID || event.UserID != owner.User.ID ||
					event.RoomStatus != want || event.CreatedAt.IsZero() {
					t.Fatalf("event = %+v, want room status %d with a timestamp", event, want)
				}
				if !createdAt.IsZero() && !event.CreatedAt.Equal(createdAt) {
					t.Fatalf("replicated event timestamp = %v, want %v", event.CreatedAt, createdAt)
				}
				createdAt = event.CreatedAt
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for room status %d", want)
			}
		}
		assertStatus(want)
	}
	awaitConnections := func(want int) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(5 * time.Second)
		for {
			var count int
			check(db.QueryRow(ctx, `SELECT count(*) FROM room_anchor_connections WHERE room_id=$1`, roomID).Scan(&count))
			if count == want {
				return
			}
			select {
			case <-ticker.C:
			case <-deadline:
				t.Fatalf("room has %d anchor connections, want %d", count, want)
			}
		}
	}

	firstAnchor, secondAnchor := connectAnchor(firstURL), connectAnchor(secondURL)
	awaitConnections(2)
	check(firstAnchor.Close())
	awaitConnections(1)
	assertStatus(live.RoomOnline)
	check(secondAnchor.Close())
	expectStatus(live.RoomAnchorAway)

	joined, err := first.Rooms.JoinRoom(ctx, roomID, owner.User.ID, "")
	check(err)
	if !joined.AnchorJoined || joined.Status() != live.RoomOnline || joined.Session != room.Session {
		t.Fatalf("anchor rejoin = %+v, want the original live session", joined)
	}
	expectStatus(live.RoomOnline)
	anchor := connectAnchor(firstURL)
	check(anchor.Close())
	expectStatus(live.RoomAnchorAway)
	connectAnchor(secondURL)
	expectStatus(live.RoomOnline)
	history, err := first.Rooms.ChatHistory(ctx, roomID, viewerID, 0, 10)
	check(err)
	if len(history) != 1 || history[0].Text != originalChat {
		t.Fatalf("history = %+v, want the original chat after anchor recovery", history)
	}

	check(first.Rooms.Close(ctx, roomID, owner.User.ID))
	awaitConnections(0)
	reopened, err := first.Rooms.Start(ctx, owner.User.ID, "Reopened", "")
	check(err)
	if err := second.Rooms.AnchorConnected(ctx, roomID, liveID, "stale"); !errors.Is(err, live.ErrRoomClosed) {
		t.Fatalf("stale anchor connect = %v, want room closed", err)
	}
	check(second.Rooms.AnchorDisconnected(ctx, roomID, liveID, "stale"))
	current, err := second.Rooms.Info(ctx, roomID)
	check(err)
	if current != reopened || current.Status() != live.RoomOnline || current.Session.LiveID == liveID {
		t.Fatalf("reopened room after stale anchor cleanup = %+v", current)
	}

	check(first.Rooms.Join(ctx, roomID, viewerID, ""))
	events, unsubscribe, err := first.Rooms.Subscribe(ctx, roomID, viewerID)
	check(err)
	t.Cleanup(unsubscribe)
	connectAnchor(secondURL)
	second.Close()
	awaitConnections(0)
	select {
	case event := <-events:
		if event.Type != "room_status" || event.RoomStatus != live.RoomAnchorAway {
			t.Fatalf("event after graceful relay shutdown = %+v, want anchor away", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for anchor away after graceful relay shutdown")
	}
}
