package live

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestHistoryPage(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	messages := []ChatMessage{
		{ID: 4, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 3, CreatedAt: now.Add(-14 * time.Hour)},
		{ID: 2, CreatedAt: now.Add(-27 * time.Hour)},
		{ID: 1, CreatedAt: now.Add(-51 * time.Hour)},
	}
	for _, tc := range []struct {
		name       string
		page, size uint64
		now        time.Time
		want       []ChatMessage
	}{
		{"first page", 0, 2, now, messages[:2]},
		{"partial page", 1, 2, now, messages[2:3]},
		{"zero size ignores page and stops at idle gap", math.MaxUint64, 0, now, messages[:3]},
		{"oversized first page", 0, math.MaxUint64, now, messages[:3]},
		{"page beyond retained history", 2, 2, now, nil},
		{"offset overflow", 1 << 63, 2, now, nil},
		{"expired at 24 hours", 0, 100, now.Add(22 * time.Hour), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := HistoryPage(messages, tc.page, tc.size, tc.now)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("HistoryPage = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestStoreChatHistoryLifecycle(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer = 1, 2
	_, err := store.ChatHistory(ctx, owner, owner, 0, 0)
	requireError(t, err, ErrRoomNotFound)
	_, err = store.Start(ctx, owner, "Live", "")
	requireError(t, err, nil)
	_, err = store.ChatHistory(ctx, owner, viewer, 0, 0)
	requireError(t, err, ErrNotMember)
	requireError(t, store.Send(ctx, owner, viewer, "not joined"), ErrNotMember)
	requireError(t, store.Join(ctx, owner, viewer, ""), nil)
	requireError(t, store.Mute(ctx, owner, owner, viewer, time.Minute), nil)
	requireError(t, store.Send(ctx, owner, viewer, "muted"), ErrMuted)
	requireError(t, store.Mute(ctx, owner, owner, viewer, 0), nil)
	const payload = `{"message":{"text":"hello"}}`
	events, unsubscribe, err := store.Subscribe(ctx, owner, owner)
	requireError(t, err, nil)
	defer unsubscribe()
	id, err := store.SendChat(ctx, ChatMessage{ID: 1000, RoomID: owner, UserID: viewer, Text: "hello", Payload: payload})
	requireError(t, err, nil)
	requireEvent(t, events, Event{Type: "chat", RoomID: owner, UserID: viewer, Text: "hello"})
	got, err := store.ChatHistory(ctx, owner, viewer, 0, 100)
	requireError(t, err, nil)
	if len(got) != 1 || id <= 0 || id == 1000 || got[0].ID != id || got[0].RoomID != owner || got[0].UserID != viewer || got[0].Text != "hello" || got[0].Payload != payload || got[0].CreatedAt.IsZero() {
		t.Fatalf("ChatHistory = %+v, want the successful chat with storage ID %d", got, id)
	}
	got[0].Text = "changed by caller"
	got, err = store.ChatHistory(ctx, owner, owner, 0, 100)
	requireError(t, err, nil)
	if len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("ChatHistory after changing previous result = %+v", got)
	}
	requireError(t, store.Leave(ctx, owner, viewer), nil)
	_, err = store.ChatHistory(ctx, owner, viewer, 0, 0)
	requireError(t, err, ErrNotMember)
	requireError(t, store.Close(ctx, owner, owner), nil)
	_, err = store.ChatHistory(ctx, owner, owner, 0, 0)
	requireError(t, err, ErrRoomClosed)
	_, err = store.Start(ctx, owner, "Next live", "")
	requireError(t, err, nil)
	got, err = store.ChatHistory(ctx, owner, owner, 0, 0)
	requireError(t, err, nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("ChatHistory after reopening = %#v, want empty slice", got)
	}
	newID, err := store.SendChat(ctx, ChatMessage{RoomID: owner, UserID: owner, Text: "new broadcast"})
	requireError(t, err, nil)
	if newID <= id {
		t.Fatalf("new broadcast message ID = %d, previous message ID = %d", newID, id)
	}
}

func TestStoreChatHistoryLimitAndIdleExpiry(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner = 1
	_, err := store.Start(ctx, owner, "Live", "")
	requireError(t, err, nil)
	for range HistoryLimit + 1 {
		requireError(t, store.Send(ctx, owner, owner, "message"), nil)
	}
	got, err := store.ChatHistory(ctx, owner, owner, 0, 0)
	requireError(t, err, nil)
	if len(got) != HistoryLimit || got[0].ID != HistoryLimit+1 || got[len(got)-1].ID != 2 {
		t.Fatalf("ChatHistory = %+v, want IDs from %d through 2", got, HistoryLimit+1)
	}
	for i := range store.rooms[owner].history {
		store.rooms[owner].history[i].CreatedAt = got[i].CreatedAt.Add(-24 * time.Hour)
	}
	got, err = store.ChatHistory(ctx, owner, owner, 0, 0)
	requireError(t, err, nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("ChatHistory after 24 idle hours = %#v, want empty slice", got)
	}
	requireError(t, store.Send(ctx, owner, owner, "fresh message"), nil)
	got, err = store.ChatHistory(ctx, owner, owner, 0, 0)
	requireError(t, err, nil)
	if len(got) != 1 || got[0].Text != "fresh message" {
		t.Fatalf("ChatHistory after resuming = %+v, want the fresh message", got)
	}
}
