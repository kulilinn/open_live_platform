package app_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/store/postgres"
)

const anchorCrashTestDatabaseEnv = "OPEN_LIVE_ANCHOR_CRASH_TEST_DATABASE"

func TestPostgresAnchorLeaseAfterForcedExitAndFailedCleanup(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := postgres.OpenWithAnchorTimeout(ctx, config.DatabaseURL, 100*time.Millisecond)
	check(err)
	t.Cleanup(store.Shutdown)
	const (
		ownerID  = 1
		viewerID = 2
		stopped  = "stopped-relay"
		healthy  = "healthy-relay"
		failed   = "failed-cleanup"
	)
	_, err = db.Exec(ctx, `INSERT INTO users(id,email,account,password_salt,password_hash) VALUES
		(1,'host@example.org','Host','',''), (2,'viewer@example.org','Viewer','','')`)
	check(err)
	room, err := store.Start(ctx, ownerID, "Live", "")
	check(err)
	check(store.Join(ctx, room.ID, viewerID, ""))
	events, unsubscribe, err := store.Subscribe(ctx, room.ID, viewerID)
	check(err)
	defer unsubscribe()
	liveID := room.Session.LiveID
	check(store.AnchorConnected(ctx, room.ID, liveID, healthy))
	check(store.AnchorConnected(ctx, room.ID, liveID, failed))

	lease := func(connection string) time.Time {
		t.Helper()
		var expires time.Time
		check(db.QueryRow(ctx, `SELECT lease_until FROM room_anchor_connections WHERE room_id=$1 AND live_id=$2 AND connection_id=$3`,
			room.ID, liveID, connection).Scan(&expires))
		return expires
	}
	expire := func(connection string) {
		t.Helper()
		_, err := db.Exec(ctx, `UPDATE room_anchor_connections SET lease_until=statement_timestamp()-interval '1 second'
			WHERE room_id=$1 AND live_id=$2 AND connection_id=$3`, room.ID, liveID, connection)
		check(err)
	}

	executable, err := os.Executable()
	check(err)
	child := exec.CommandContext(ctx, executable, "-test.run=^TestPostgresAnchorLeaseCrashProcess$")
	child.Env = append(os.Environ(), anchorCrashTestDatabaseEnv+"="+config.DatabaseURL,
		"OPEN_LIVE_ANCHOR_CRASH_TEST_ROOM="+strconv.FormatInt(room.ID, 10),
		"OPEN_LIVE_ANCHOR_CRASH_TEST_LIVE="+strconv.FormatInt(liveID, 10))
	check(child.Start())
	defer func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	}()
	awaitAnchorLeaseCondition(t, "registration from external relay process", 7*time.Second, func() bool {
		var registered bool
		check(db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_anchor_connections
			WHERE room_id=$1 AND live_id=$2 AND connection_id=$3)`, room.ID, liveID, stopped).Scan(&registered))
		return registered
	})
	check(child.Process.Kill())
	if err := child.Wait(); err == nil {
		t.Fatal("terminated relay exited successfully")
	}
	check(store.CloseExpiredAnchorRooms(ctx, time.Now()))
	if lease(stopped).IsZero() {
		t.Fatal("terminated relay registration did not retain its lease")
	}
	expire(stopped)
	check(store.CloseExpiredAnchorRooms(ctx, time.Now()))
	var registrations int
	check(db.QueryRow(ctx, `SELECT count(*) FROM room_anchor_connections WHERE room_id=$1`, room.ID).Scan(&registrations))
	current, err := store.Info(ctx, room.ID)
	check(err)
	if registrations != 2 || current.Status() != live.RoomOnline || current.Session != room.Session {
		t.Fatalf("room after removing terminated relay = %+v with %d registrations, want online and two remaining anchors", current, registrations)
	}

	// Block the real DELETE in PostgreSQL until the disconnect context expires.
	// Deferring a successful cleanup would not exercise lease fallback.
	tx, err := db.Begin(ctx)
	check(err)
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `SELECT 1 FROM room_anchor_connections WHERE room_id=$1 AND live_id=$2 AND connection_id=$3 FOR UPDATE`,
		room.ID, liveID, failed)
	check(err)
	disconnectCtx, cancelDisconnect := context.WithTimeout(ctx, 100*time.Millisecond)
	err = store.AnchorDisconnected(disconnectCtx, room.ID, liveID, failed)
	cancelDisconnect()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked anchor disconnect = %v, want deadline exceeded", err)
	}
	check(tx.Rollback(ctx))
	failedUntil, healthyUntil := lease(failed), lease(healthy)
	awaitAnchorLeaseCondition(t, "renewal of live anchor", 8*time.Second, func() bool {
		return lease(healthy).After(healthyUntil)
	})
	if renewed := lease(failed); !renewed.Equal(failedUntil) {
		t.Fatalf("failed cleanup renewed an inactive anchor from %v to %v", failedUntil, renewed)
	}
	check(store.AnchorDisconnected(ctx, room.ID, liveID, healthy))
	expire(failed)
	check(store.CloseExpiredAnchorRooms(ctx, time.Now()))
	select {
	case event := <-events:
		if event.Type != "room_status" || event.RoomStatus != live.RoomAnchorAway || event.RoomID != room.ID {
			t.Fatalf("event after last lease expired = %+v, want anchor away", event)
		}
	case <-ctx.Done():
		t.Fatal("no anchor away event after last lease expired")
	}
	away, err := store.Info(ctx, room.ID)
	check(err)
	if away.Status() != live.RoomAnchorAway || away.AnchorAwayUntil.IsZero() || away.Session != room.Session || away.MemberCount != 1 {
		t.Fatalf("away room = %+v, want original session, viewer, and deadline", away)
	}
	awaitAnchorLeaseCondition(t, "broadcast close after away deadline", 3*time.Second, func() bool {
		check(store.CloseExpiredAnchorRooms(ctx, time.Now()))
		closed, err := store.Info(ctx, room.ID)
		check(err)
		return !closed.Online && closed.Session.LiveID == liveID && !closed.Session.EndedAt.IsZero()
	})
	select {
	case event := <-events:
		if event.Type != "close" || event.RoomID != room.ID {
			t.Fatalf("event after away deadline = %+v, want close", event)
		}
	case <-ctx.Done():
		t.Fatal("no close event after away deadline")
	}
}

func TestPostgresAnchorLeaseCrashProcess(t *testing.T) {
	dsn := os.Getenv(anchorCrashTestDatabaseEnv)
	if dsn == "" {
		return
	}
	roomID, err := strconv.ParseInt(os.Getenv("OPEN_LIVE_ANCHOR_CRASH_TEST_ROOM"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	liveID, err := strconv.ParseInt(os.Getenv("OPEN_LIVE_ANCHOR_CRASH_TEST_LIVE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	store, err := postgres.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown()
	if err := store.AnchorConnected(t.Context(), roomID, liveID, "stopped-relay"); err != nil {
		t.Fatal(err)
	}
	<-time.After(30 * time.Second)
	t.Fatal("parent did not terminate relay process")
}

func TestPostgresAnchorLeaseMigration(t *testing.T) {
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
	_, err = db.Exec(ctx, `ALTER TABLE room_anchor_connections DROP COLUMN lease_until;
		DELETE FROM schema_migrations WHERE version=11;
		INSERT INTO users(id,email,account,password_salt,password_hash) VALUES (1,'legacy@example.org','Legacy','','');
		INSERT INTO rooms(id,owner_id,title,live_id,live_started_at) VALUES (1,1,'Live',2,statement_timestamp());
		INSERT INTO room_anchor_connections(room_id,live_id,connection_id) VALUES (1,1,'previous-broadcast'), (1,2,'legacy-host')`)
	check(err)
	var before, after time.Time
	check(db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before))
	store, err = postgres.Open(ctx, config.DatabaseURL)
	check(err)
	defer store.Shutdown()
	check(db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after))
	var legacyExpiry time.Time
	check(db.QueryRow(ctx, `SELECT lease_until FROM room_anchor_connections WHERE connection_id='legacy-host'`).Scan(&legacyExpiry))
	if legacyExpiry.Before(before.Add(30*time.Second)) || legacyExpiry.After(after.Add(30*time.Second)) {
		t.Fatalf("migrated lease = %v, want 30 seconds from migration", legacyExpiry)
	}
	_, err = db.Exec(ctx, `UPDATE room_anchor_connections SET lease_until=statement_timestamp()-interval '1 second'
		WHERE connection_id='previous-broadcast'`)
	check(err)
	check(store.CloseExpiredAnchorRooms(ctx, time.Now()))
	current, err := store.Info(ctx, 1)
	check(err)
	if current.Status() != live.RoomOnline || current.Session.LiveID != 2 {
		t.Fatalf("current room after expiring old broadcast's lease = %+v, want active broadcast 2", current)
	}
}

func awaitAnchorLeaseCondition(t *testing.T, description string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}
