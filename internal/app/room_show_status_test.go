package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
)

func TestRoomShowStateEventsAndRecovery(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testRoomShowStateEventsAndRecovery(t, app.Config{})
	})
	t.Run("postgres", func(t *testing.T) {
		config, _ := postgresTestDatabase(t)
		testRoomShowStateEventsAndRecovery(t, config)
	})
}

func testRoomShowStateEventsAndRecovery(t *testing.T, config app.Config) {
	t.Helper()
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	open := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		return platform
	}
	first := open()
	peer := first
	if config.DatabaseURL != "" {
		peer = open()
	}
	owner, err := first.Accounts.SignUp(ctx, "show-owner@example.org", "password", "Show")
	check(err)
	roomID := owner.User.ID
	room, err := first.Rooms.Start(ctx, roomID, "Show", "")
	check(err)
	events, unsub, err := peer.Rooms.Subscribe(ctx, roomID, roomID)
	check(err)
	defer unsub()
	update := func(query string, code int, online live.RoomStatus) {
		t.Helper()
		incoming := httptest.NewRequest(http.MethodGet, "/api/live/room/online"+query, nil).WithContext(ctx)
		incoming.Header.Set("Authorization", "Bearer "+owner.Token)
		res := httptest.NewRecorder()
		first.Server.Handler.ServeHTTP(res, incoming)
		if res.Code != code {
			t.Fatalf("online%s = %d, want %d: %s", query, res.Code, code, res.Body.String())
		}
		if code == http.StatusOK {
			var payload struct {
				Status string
				Data   struct{ Online live.RoomStatus }
			}
			check(json.NewDecoder(res.Body).Decode(&payload))
			if payload.Status != "200" || payload.Data.Online != online {
				t.Fatalf("online%s = %+v, want status %d", query, payload, online)
			}
		}
	}
	wantEvent := func(kind string, status live.RoomStatus) {
		t.Helper()
		select {
		case event := <-events:
			if event.Type != kind || event.RoomStatus != status || event.CreatedAt.IsZero() {
				t.Fatalf("room event = %+v, want %s status %d", event, kind, status)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("room status event timeout")
		}
	}
	update("?status=4", http.StatusForbidden, 0)
	update("?status=2", http.StatusOK, live.RoomLiveShow)
	wantEvent("room_status", live.RoomLiveShow)
	check(first.Rooms.AnchorConnected(ctx, roomID, room.Session.LiveID, "old"))
	check(first.Rooms.AnchorDisconnected(ctx, roomID, room.Session.LiveID, "old"))
	wantEvent("room_status", live.RoomAnchorAway)
	update("", http.StatusOK, live.RoomAnchorAway)
	check(peer.Rooms.AnchorConnected(ctx, roomID, room.Session.LiveID, "recovered"))
	wantEvent("room_status", live.RoomLiveShow)
	if config.DatabaseURL != "" {
		fresh := open()
		stored, err := fresh.Rooms.Info(ctx, roomID)
		check(err)
		if stored.Status() != live.RoomLiveShow || stored.Session != room.Session {
			t.Fatalf("reopened store returned %+v instead of current show", stored)
		}
	}
	update("?status=1", http.StatusOK, live.RoomOnline)
	wantEvent("room_status", live.RoomOnline)
	update("?status=2", http.StatusOK, live.RoomLiveShow)
	wantEvent("room_status", live.RoomLiveShow)
	update("?status=0", http.StatusOK, live.RoomOffline)
	wantEvent("close", live.RoomOffline)
	update("?status=2", http.StatusOK, live.RoomOffline)
	reopened, err := first.Rooms.Start(ctx, roomID, "Public", "")
	check(err)
	if reopened.Status() != live.RoomOnline || reopened.Session.LiveID == room.Session.LiveID {
		t.Fatalf("new broadcast = %+v, want ordinary live state and fresh live ID", reopened)
	}
}
