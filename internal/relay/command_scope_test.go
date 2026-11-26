package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

type observedCommandScope struct {
	roomID, liveID int64
	scoped         bool
}

type commandScopeStore struct {
	*live.Store
	muteScope chan observedCommandScope
}

func (s *commandScopeStore) Mute(ctx context.Context, roomID, actorID, targetID int64, duration time.Duration) error {
	err := s.Store.Mute(ctx, roomID, actorID, targetID, duration)
	scopedRoom, scopedLive, scoped := live.RoomCommandScope(ctx)
	s.muteScope <- observedCommandScope{roomID: scopedRoom, liveID: scopedLive, scoped: scoped}
	return err
}

func TestConnectorScopesCommandsToSubscription(t *testing.T) {
	const secret = "relay-command-scope"
	ctx := t.Context()
	accounts := auth.NewStore()
	owner, err := accounts.SignUp(ctx, "owner@example.com", "password", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(ctx, "viewer@example.com", "password", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	rooms := &commandScopeStore{Store: live.NewStore(), muteScope: make(chan observedCommandScope, 1)}
	room, err := rooms.Start(ctx, owner.User.ID, "Broadcast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := rooms.Join(ctx, room.ID, viewer.User.ID, ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, accounts, rooms, secret, "")
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/connector/" + owner.Token + "/" + Signature(owner.Token, secret)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(command{Type: "mute", TargetID: viewer.User.ID, DurationSeconds: 60}); err != nil {
		t.Fatal(err)
	}

	want := observedCommandScope{roomID: room.ID, liveID: room.Session.LiveID, scoped: true}
	select {
	case got := <-rooms.muteScope:
		if got != want {
			t.Fatalf("connector command scope = %+v, want %+v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connector did not send the room command")
	}
	muted, err := rooms.Muted(ctx, room.ID, viewer.User.ID)
	if err != nil || !muted {
		t.Fatalf("viewer muted = %t, %v", muted, err)
	}
}
