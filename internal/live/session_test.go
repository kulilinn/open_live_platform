package live

import (
	"testing"
	"time"
)

func TestLiveSessionStoreCreatesIndependentBroadcasts(t *testing.T) {
	store := NewLiveSessionStore()
	start := time.Date(2026, time.November, 26, 9, 0, 0, 0, time.UTC)
	first := store.Start(42, 0, start)
	if first.RoomID != 42 || first.LiveID != 1 || first.AnchorNID != DefaultAnchorNID || !first.StartedAt.Equal(start) {
		t.Fatalf("first session = %+v", first)
	}
	if got, ok := store.Get(42); !ok || got != first {
		t.Fatalf("stored first session = %+v, %v", got, ok)
	}
	ended, ok := store.End(42)
	if !ok || ended != first {
		t.Fatalf("ended first session = %+v, %v", ended, ok)
	}
	second := store.Start(42, 200000099, start.Add(time.Hour))
	if second.RoomID != 42 || second.LiveID != 2 || second.AnchorNID != 200000099 ||
		!second.StartedAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("second session = %+v", second)
	}
	if _, ok := store.Get(42); !ok {
		t.Fatal("second session missing")
	}
}

func TestLiveSessionStoreEndMissingRoom(t *testing.T) {
	store := NewLiveSessionStore()
	if _, ok := store.End(99); ok {
		t.Fatal("missing session reported as ended")
	}
}
