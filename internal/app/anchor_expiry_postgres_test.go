package app_test

import (
	"context"
	"testing"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresAnchorAwayDeadlineAcrossStores(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	open := func(timeout time.Duration) *postgres.Store {
		t.Helper()
		store, err := postgres.OpenWithAnchorTimeout(ctx, config.DatabaseURL, timeout)
		check(err)
		t.Cleanup(store.Shutdown)
		return store
	}
	const (
		ownerID       = 1
		viewerID      = 2
		firstTimeout  = 43 * time.Second
		secondTimeout = 71 * time.Second
	)
	first, second := open(firstTimeout), open(secondTimeout)
	_, err := db.Exec(ctx, `INSERT INTO users(id,email,account,password_salt,password_hash) VALUES
		(1,'host@example.org','Host','',''), (2,'viewer@example.org','Viewer','','')`)
	check(err)
	room, err := first.Start(ctx, ownerID, "Live", "")
	check(err)
	check(first.Join(ctx, room.ID, viewerID, ""))
	liveID := room.Session.LiveID

	assertAway := func(timeout time.Duration) live.Room {
		t.Helper()
		got, err := second.Info(ctx, room.ID)
		check(err)
		var changedAt time.Time
		check(db.QueryRow(ctx, "SELECT updated_at FROM rooms WHERE id=$1", room.ID).Scan(&changedAt))
		if got.Status() != live.RoomAnchorAway || got.Session != room.Session || got.MemberCount != 1 ||
			got.AnchorAwayUntil.Sub(changedAt) != timeout {
			t.Fatalf("away room = %+v, want deadline %s after %v", got, timeout, changedAt)
		}
		return got
	}
	check(first.AnchorConnected(ctx, room.ID, liveID, "first"))
	check(first.AnchorDisconnected(ctx, room.ID, liveID, "first"))
	oldDeadline := assertAway(firstTimeout).AnchorAwayUntil
	check(first.AnchorDisconnected(ctx, room.ID, liveID, "first"))
	if repeated := assertAway(firstTimeout).AnchorAwayUntil; !repeated.Equal(oldDeadline) {
		t.Fatalf("repeated disconnect changed deadline from %v to %v", oldDeadline, repeated)
	}
	first.Shutdown()

	joined, err := second.JoinRoom(ctx, room.ID, ownerID, "")
	check(err)
	if joined.Status() != live.RoomOnline || !joined.AnchorAwayUntil.IsZero() || joined.Session != room.Session {
		t.Fatalf("anchor rejoin = %+v, want original live session with no deadline", joined)
	}
	check(second.AnchorConnected(ctx, room.ID, liveID, "second"))
	check(second.AnchorDisconnected(ctx, room.ID, liveID, "second"))
	deadline := assertAway(secondTimeout).AnchorAwayUntil
	if !deadline.After(oldDeadline) {
		t.Fatalf("new deadline %v must be after old deadline %v", deadline, oldDeadline)
	}
	check(second.CloseExpiredAnchorRooms(ctx, oldDeadline))
	check(second.CloseExpiredAnchorRooms(ctx, deadline.Add(-time.Microsecond)))
	assertAway(secondTimeout)

	other := open(0)
	if persisted, err := other.Info(ctx, room.ID); err != nil || !persisted.AnchorAwayUntil.Equal(deadline) {
		t.Fatalf("deadline from new instance = %+v, error = %v, want %v", persisted, err, deadline)
	}
	check(other.CloseExpiredAnchorRooms(ctx, deadline.Add(time.Hour)))
	assertAway(secondTimeout)

	var expired time.Time
	check(db.QueryRow(ctx, `UPDATE rooms SET anchor_away_until=clock_timestamp()-interval '1 second'
		WHERE id=$1 RETURNING anchor_away_until`, room.ID).Scan(&expired))
	events, unsubscribe, err := other.Subscribe(ctx, room.ID, viewerID)
	check(err)
	defer unsubscribe()
	results := make(chan error, 2)
	for _, store := range []*postgres.Store{second, other} {
		go func() { results <- store.CloseExpiredAnchorRooms(ctx, expired) }()
	}
	for range 2 {
		check(<-results)
	}
	closed, err := other.Info(ctx, room.ID)
	check(err)
	if closed.Online || closed.AnchorAway || !closed.AnchorAwayUntil.IsZero() || closed.Session.LiveID != liveID ||
		closed.Session.EndedAt.IsZero() || closed.MemberCount != 0 {
		t.Fatalf("expired room = %+v, want closed original session and clean membership", closed)
	}
	select {
	case event := <-events:
		if event.Type != "close" || event.RoomID != room.ID || event.UserID != ownerID || !event.CreatedAt.Equal(closed.Session.EndedAt) {
			t.Fatalf("expiry event = %+v, want ordinary close event", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for close event")
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("subscription received an event after close")
		}
	case <-ctx.Done():
		t.Fatal("close event did not end the subscription")
	}
	reopened, err := other.Start(ctx, ownerID, "Reopened", "")
	check(err)
	check(second.CloseExpiredAnchorRooms(ctx, deadline))
	current, err := other.Info(ctx, room.ID)
	check(err)
	if current != reopened || !current.Online || current.Session.LiveID == liveID || !current.AnchorAwayUntil.IsZero() {
		t.Fatalf("room after stale expiry = %+v, want reopened session", current)
	}
}

func TestPostgresAnchorAwayDeadlineMigration(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := postgres.Open(ctx, config.DatabaseURL)
	check(err)
	store.Shutdown()
	_, err = db.Exec(ctx, `ALTER TABLE rooms DROP COLUMN anchor_away_until;
		DELETE FROM schema_migrations WHERE version=10;
		INSERT INTO users(id,email,account,password_salt,password_hash) VALUES
			(1,'away@example.org','Away','',''), (2,'online@example.org','Online','','');
		INSERT INTO rooms(id,owner_id,title,anchor_away,live_id,live_started_at,updated_at) VALUES
			(1,1,'Away',true,nextval('room_live_id_seq'),now()-interval '1 day',now()-interval '1 day'),
			(2,2,'Online',false,nextval('room_live_id_seq'),now()-interval '1 day',now()-interval '1 day')`)
	check(err)
	var before, after time.Time
	check(db.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before))
	store, err = postgres.OpenWithAnchorTimeout(ctx, config.DatabaseURL, time.Second)
	check(err)
	defer store.Shutdown()
	check(db.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after))
	check(store.CloseExpiredAnchorRooms(ctx, after))
	away, err := store.Info(ctx, 1)
	check(err)
	if away.Status() != live.RoomAnchorAway ||
		away.AnchorAwayUntil.Before(before.Add(live.DefaultAnchorAwayTimeout)) ||
		away.AnchorAwayUntil.After(after.Add(live.DefaultAnchorAwayTimeout)) {
		t.Fatalf("legacy away room = %+v, want grace period starting during migration", away)
	}
	online, err := store.Info(ctx, 2)
	check(err)
	if online.Status() != live.RoomOnline || !online.AnchorAwayUntil.IsZero() {
		t.Fatalf("legacy available room = %+v, want online with no deadline", online)
	}
}
