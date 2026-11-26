package live

import "testing"

func TestAnchorDisconnectRemainsScopedToBroadcast(t *testing.T) {
	ctx := t.Context()
	store := NewStore()
	const anchor = 1
	first, err := store.Start(ctx, anchor, "First", "")
	requireError(t, err, nil)
	requireError(t, store.AnchorConnected(ctx, first.ID, first.Session.LiveID, "old-connection"), nil)
	requireError(t, store.Close(ctx, first.ID, anchor), nil)

	second, err := store.Start(ctx, anchor, "Second", "")
	requireError(t, err, nil)
	if second.Session.LiveID == first.Session.LiveID {
		t.Fatal("reopen reused live ID")
	}
	sub, err := store.SubscribeLive(ctx, second.ID, anchor)
	requireError(t, err, nil)
	defer sub.Unsubscribe()
	requireError(t, store.AnchorConnected(ctx, first.ID, first.Session.LiveID, "late-connect"), ErrRoomClosed)
	requireError(t, store.AnchorDisconnected(ctx, first.ID, first.Session.LiveID, "old-connection"), nil)
	requireError(t, store.AnchorDisconnected(ctx, second.ID, second.Session.LiveID, "unknown"), nil)
	select {
	case event := <-sub.Events:
		t.Fatalf("late transport emitted %+v in the new broadcast", event)
	default:
	}

	requireError(t, store.AnchorConnected(ctx, second.ID, second.Session.LiveID, "new-connection"), nil)
	requireError(t, store.AnchorDisconnected(ctx, second.ID, second.Session.LiveID, "new-connection"), nil)
	away, err := store.Info(ctx, second.ID)
	requireError(t, err, nil)
	if away.Status() != RoomAnchorAway || away.Session != second.Session {
		t.Fatalf("temporarily away room = %+v", away)
	}
	if event := <-sub.Events; event.Type != "room_status" || event.RoomStatus != RoomAnchorAway {
		t.Fatalf("away event = %+v", event)
	}
	requireError(t, store.AnchorDisconnected(ctx, second.ID, second.Session.LiveID, "new-connection"), nil)
	select {
	case event := <-sub.Events:
		t.Fatalf("duplicate disconnect emitted %+v", event)
	default:
	}
}
