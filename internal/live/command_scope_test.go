package live

import (
	"testing"
	"time"
)

func TestRoomCommandsStayInSubscribedBroadcast(t *testing.T) {
	ctx := t.Context()
	const ownerID, viewerID, otherOwnerID = int64(1), int64(2), int64(3)
	store := NewStore()
	first, err := store.Start(ctx, ownerID, "First broadcast", "")
	requireError(t, err, nil)
	oldSub, err := store.SubscribeLive(ctx, first.ID, ownerID)
	requireError(t, err, nil)
	defer oldSub.Unsubscribe()
	if oldSub.LiveID != first.Session.LiveID {
		t.Fatalf("subscription LiveID = %d, want %d", oldSub.LiveID, first.Session.LiveID)
	}
	oldCommands := WithRoomCommandScope(ctx, first.ID, oldSub.LiveID)

	requireError(t, store.Close(ctx, first.ID, ownerID), nil)
	closed, err := store.Info(ctx, first.ID)
	requireError(t, err, nil)
	requireEvent(t, oldSub.Events, Event{Type: "close", RoomID: first.ID, UserID: ownerID, CreatedAt: closed.Session.EndedAt})
	requireClosed(t, oldSub.Events)
	requireError(t, store.Close(oldCommands, first.ID, ownerID), ErrRoomClosed)
	requireError(t, store.Close(ctx, first.ID, ownerID), nil)

	second, err := store.Start(ctx, ownerID, "Second broadcast", "")
	requireError(t, err, nil)
	requireError(t, store.Join(ctx, second.ID, viewerID, ""), nil)
	newSub, err := store.SubscribeLive(ctx, second.ID, ownerID)
	requireError(t, err, nil)
	defer newSub.Unsubscribe()
	if second.ID != first.ID || newSub.LiveID == oldSub.LiveID || newSub.LiveID != second.Session.LiveID {
		t.Fatalf("second room and subscription = %+v, LiveID %d", second, newSub.LiveID)
	}

	requireError(t, store.Send(oldCommands, second.ID, ownerID, "First broadcast chat"), ErrRoomClosed)
	_, err = store.SendChat(oldCommands, ChatMessage{RoomID: second.ID, UserID: ownerID, Text: "First broadcast chat"})
	requireError(t, err, ErrRoomClosed)
	history, err := store.ChatHistory(ctx, second.ID, ownerID, 0, 10)
	requireError(t, err, nil)
	if len(history) != 0 {
		t.Fatalf("second broadcast history = %+v, want no messages", history)
	}

	requireError(t, store.Mute(oldCommands, second.ID, ownerID, viewerID, time.Hour), ErrRoomClosed)
	muted, err := store.Muted(ctx, second.ID, viewerID)
	requireError(t, err, nil)
	if muted {
		t.Fatal("viewer was muted in the second broadcast")
	}
	requireError(t, store.Mute(ctx, second.ID, ownerID, viewerID, time.Hour), nil)
	requireError(t, store.Mute(oldCommands, second.ID, ownerID, viewerID, 0), ErrRoomClosed)
	muted, err = store.Muted(ctx, second.ID, viewerID)
	requireError(t, err, nil)
	if !muted {
		t.Fatal("viewer was unmuted in the second broadcast")
	}

	requireError(t, store.Kick(oldCommands, second.ID, ownerID, viewerID), ErrRoomClosed)
	members, err := store.Members(ctx, second.ID)
	requireError(t, err, nil)
	if len(members) != 1 || members[0] != viewerID {
		t.Fatalf("second broadcast members = %v, want [%d]", members, viewerID)
	}
	requireError(t, store.Close(oldCommands, second.ID, ownerID), ErrRoomClosed)
	current, err := store.Info(ctx, second.ID)
	requireError(t, err, nil)
	if !current.Online {
		t.Fatal("second broadcast was closed")
	}

	newCommands := WithRoomCommandScope(ctx, second.ID, newSub.LiveID)
	other, err := store.Start(ctx, otherOwnerID, "Other room", "")
	requireError(t, err, nil)
	requireError(t, store.Close(newCommands, other.ID, otherOwnerID), ErrRoomClosed)
	wrongRoom := WithRoomCommandScope(ctx, other.ID, newSub.LiveID)
	requireError(t, store.Close(wrongRoom, second.ID, ownerID), ErrRoomClosed)

	requireError(t, store.Send(newCommands, second.ID, ownerID, "Second broadcast chat"), nil)
	requireError(t, store.Mute(newCommands, second.ID, ownerID, viewerID, 0), nil)
	requireError(t, store.Kick(newCommands, second.ID, ownerID, viewerID), nil)
	requireError(t, store.Close(newCommands, second.ID, ownerID), nil)
}
