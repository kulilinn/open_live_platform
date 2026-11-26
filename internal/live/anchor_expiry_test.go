package live

import (
	"testing"
	"time"
)

func TestAnchorAwayExpiryClosesAtDeadline(t *testing.T) {
	if DefaultAnchorAwayTimeout != 5*time.Minute {
		t.Fatalf("default anchor timeout = %v, want 5 minutes", DefaultAnchorAwayTimeout)
	}
	for _, test := range []struct {
		name    string
		store   *Store
		timeout time.Duration
	}{
		{name: "default", store: NewStore(), timeout: DefaultAnchorAwayTimeout},
		{name: "configured", store: NewStoreWithAnchorTimeout(30 * time.Second), timeout: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			store := test.store
			const owner, viewer = 1, 2
			room, err := store.Start(ctx, owner, "Live", "")
			requireError(t, err, nil)
			requireError(t, store.Join(ctx, room.ID, viewer, ""), nil)
			sub, err := store.SubscribeLive(ctx, room.ID, viewer)
			requireError(t, err, nil)
			defer sub.Unsubscribe()

			liveID := room.Session.LiveID
			requireError(t, store.AnchorConnected(ctx, room.ID, liveID, "first"), nil)
			requireError(t, store.AnchorConnected(ctx, room.ID, liveID, "last"), nil)
			requireError(t, store.AnchorDisconnected(ctx, room.ID, liveID, "first"), nil)
			online := requireAnchorExpiryStatus(t, store, room.ID, RoomOnline)
			if !online.AnchorAwayUntil.IsZero() {
				t.Fatalf("connected anchor has an expiry of %v", online.AnchorAwayUntil)
			}
			requireNoAnchorExpiryEvent(t, sub.Events)

			before := time.Now()
			requireError(t, store.AnchorDisconnected(ctx, room.ID, liveID, "last"), nil)
			after := time.Now()
			away := requireAnchorExpiryStatus(t, store, room.ID, RoomAnchorAway)
			deadline := away.AnchorAwayUntil
			if deadline.Before(before.Add(test.timeout)) || deadline.After(after.Add(test.timeout)) {
				t.Fatalf("anchor expiry = %v, want %v from disconnect", deadline, test.timeout)
			}
			select {
			case event := <-sub.Events:
				if event.Type != "room_status" || event.RoomID != room.ID || event.UserID != owner || event.RoomStatus != RoomAnchorAway {
					t.Fatalf("away event = %+v", event)
				}
			default:
				t.Fatal("away event was not delivered")
			}

			requireError(t, store.AnchorDisconnected(ctx, room.ID, liveID, "last"), nil)
			requireError(t, store.CloseExpiredAnchorRooms(ctx, deadline.Add(-time.Nanosecond)), nil)
			pending := requireAnchorExpiryStatus(t, store, room.ID, RoomAnchorAway)
			if !pending.AnchorAwayUntil.Equal(deadline) {
				t.Fatalf("pending expiry changed from %v to %v", deadline, pending.AnchorAwayUntil)
			}
			requireNoAnchorExpiryEvent(t, sub.Events)

			requireError(t, store.CloseExpiredAnchorRooms(ctx, deadline), nil)
			closed := requireAnchorExpiryStatus(t, store, room.ID, RoomOffline)
			if !closed.AnchorAwayUntil.IsZero() || closed.AnchorAway || closed.MemberCount != 0 ||
				closed.Session.LiveID != liveID || closed.Session.EndedAt.IsZero() {
				t.Fatalf("expired broadcast = %+v", closed)
			}
			requireEvent(t, sub.Events, Event{Type: "close", RoomID: room.ID, UserID: owner, CreatedAt: closed.Session.EndedAt})
			requireError(t, store.CloseExpiredAnchorRooms(ctx, deadline.Add(time.Minute)), nil)
			requireClosed(t, sub.Events)
			if rescanned := requireAnchorExpiryStatus(t, store, room.ID, RoomOffline); rescanned != closed {
				t.Fatalf("repeat expiry scan changed the closed room from %+v to %+v", closed, rescanned)
			}
		})
	}
}

func TestAnchorReturnClearsExpiry(t *testing.T) {
	for _, action := range []string{"join", "reconnect"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			store := NewStore()
			room, err := store.Start(ctx, 1, "Live", "")
			requireError(t, err, nil)
			deadline := startAnchorAbsence(t, store, room)
			requireError(t, store.CloseExpiredAnchorRooms(ctx, deadline.Add(-time.Nanosecond)), nil)

			if action == "join" {
				_, err = store.JoinRoom(ctx, room.ID, room.OwnerID, "")
			} else {
				err = store.AnchorConnected(ctx, room.ID, room.Session.LiveID, "anchor")
			}
			requireError(t, err, nil)
			requireError(t, store.CloseExpiredAnchorRooms(ctx, deadline), nil)
			rejoined := requireAnchorExpiryStatus(t, store, room.ID, RoomOnline)
			if !rejoined.AnchorAwayUntil.IsZero() || rejoined.Session != room.Session {
				t.Fatalf("returned anchor's broadcast = %+v", rejoined)
			}
		})
	}
}

func TestAnchorExpiryUsesCurrentAbsence(t *testing.T) {
	for _, scenario := range []string{"same broadcast", "reopened broadcast"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			store := NewStore()
			room, err := store.Start(ctx, 1, "Live", "")
			requireError(t, err, nil)
			expiry := startAnchorAbsence(t, store, room)
			// Separate the two deadlines even if both disconnects share the same clock tick.
			oldDeadline := expiry.Add(-DefaultAnchorAwayTimeout / 2)
			store.mu.Lock()
			store.rooms[room.ID].AnchorAwayUntil = oldDeadline
			store.mu.Unlock()

			if scenario == "reopened broadcast" {
				requireError(t, store.Close(ctx, room.ID, room.OwnerID), nil)
				closed := requireAnchorExpiryStatus(t, store, room.ID, RoomOffline)
				if !closed.AnchorAwayUntil.IsZero() {
					t.Fatalf("closed broadcast has an expiry of %v", closed.AnchorAwayUntil)
				}
				reopened, err := store.Start(ctx, room.OwnerID, "Next broadcast", "")
				requireError(t, err, nil)
				if reopened.ID != room.ID || reopened.Session.LiveID == room.Session.LiveID || !reopened.AnchorAwayUntil.IsZero() {
					t.Fatalf("reopened broadcast = %+v", reopened)
				}
				room = reopened
			}
			newDeadline := startAnchorAbsence(t, store, room)
			if !newDeadline.After(oldDeadline) {
				t.Fatalf("current expiry = %v, want later than old expiry %v", newDeadline, oldDeadline)
			}
			requireError(t, store.CloseExpiredAnchorRooms(ctx, oldDeadline), nil)
			current := requireAnchorExpiryStatus(t, store, room.ID, RoomAnchorAway)
			if !current.AnchorAwayUntil.Equal(newDeadline) || current.Session.LiveID != room.Session.LiveID {
				t.Fatalf("old deadline scan changed the current broadcast: %+v", current)
			}
			requireError(t, store.CloseExpiredAnchorRooms(ctx, newDeadline), nil)
			requireAnchorExpiryStatus(t, store, room.ID, RoomOffline)
		})
	}
}

func startAnchorAbsence(t *testing.T, store *Store, room Room) time.Time {
	t.Helper()
	ctx := t.Context()
	requireError(t, store.AnchorConnected(ctx, room.ID, room.Session.LiveID, "anchor"), nil)
	requireError(t, store.AnchorDisconnected(ctx, room.ID, room.Session.LiveID, "anchor"), nil)
	return requireAnchorExpiryStatus(t, store, room.ID, RoomAnchorAway).AnchorAwayUntil
}

func requireAnchorExpiryStatus(t *testing.T, store *Store, roomID int64, want RoomStatus) Room {
	t.Helper()
	room, err := store.Info(t.Context(), roomID)
	requireError(t, err, nil)
	if room.Status() != want {
		t.Fatalf("room %d status = %v, want %v", roomID, room.Status(), want)
	}
	return room
}

func requireNoAnchorExpiryEvent(t *testing.T, events <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-events:
		t.Fatalf("unexpected event %+v, subscription open = %t", event, ok)
	default:
	}
}
