package live

import (
	"testing"
	"time"
)

func TestRoomBroadcastLifecycle(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer = 42, 43
	first, err := store.Start(ctx, owner, "First broadcast", "")
	requireError(t, err, nil)
	if first.Session.LiveID <= 1<<32-1 || first.Session.AnchorNID != DefaultAnchorNID || first.Session.StartedAt.IsZero() ||
		!first.Session.EndedAt.IsZero() {
		t.Fatalf("first session = %+v", first.Session)
	}

	same, err := store.Start(ctx, owner, "Retry", "")
	requireError(t, err, nil)
	if same != first {
		t.Fatalf("retry changed the open broadcast: %+v", same)
	}
	joined, err := store.JoinRoom(ctx, first.ID, viewer, "")
	requireError(t, err, nil)
	if joined.Session != first.Session || joined.MemberCount != 1 {
		t.Fatalf("joined room = %+v", joined)
	}

	requireError(t, store.Leave(ctx, first.ID, owner), nil)
	closed, err := store.Info(ctx, first.ID)
	requireError(t, err, nil)
	if closed.Online || closed.Session.LiveID != first.Session.LiveID ||
		closed.Session.EndedAt.Before(closed.Session.StartedAt) {
		t.Fatalf("closed broadcast = %+v", closed)
	}
	duration := closed.Session.EndedAt.Sub(first.Session.StartedAt)
	if got := closed.Session.Duration(closed.Session.EndedAt.Add(time.Hour)); got != duration {
		t.Fatalf("ended duration = %v; want %v", got, duration)
	}
	second, err := store.Start(ctx, owner, "Second broadcast", "")
	requireError(t, err, nil)
	if second.ID != first.ID || second.Session.LiveID == first.Session.LiveID || !second.Session.EndedAt.IsZero() ||
		second.Session.StartedAt.Before(closed.Session.EndedAt) {
		t.Fatalf("second broadcast = %+v", second)
	}
}
