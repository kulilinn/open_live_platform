package app_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"open_live_platform/internal/app"
)

func TestPostgresMigrationHistoryAndRestart(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	platform.Close()
	versions := postgresMigrationVersions(t, db)

	// The core script would recreate this index if it ran again.
	if _, err := db.Exec(ctx, "DROP INDEX sessions_user_id_idx"); err != nil {
		t.Fatal(err)
	}
	platform, err = app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if restarted := postgresMigrationVersions(t, db); !slices.Equal(restarted, versions) {
		t.Fatalf("migration versions after restart = %v, want %v", restarted, versions)
	}
	var indexMissing bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('sessions_user_id_idx') IS NULL").Scan(&indexMissing); err != nil || !indexMissing {
		t.Fatalf("core migration was replayed: index missing = %t, error = %v", indexMissing, err)
	}
}

func TestPostgresMigrationRollbackAndRetry(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	core, err := os.ReadFile("../store/postgres/migrations/001_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(core)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users(id,email,account,password_salt,password_hash)
		VALUES (1,'legacy@example.org','legacy','','');
		INSERT INTO rooms(id,owner_id,title,updated_at)
		VALUES (1,1,'Existing room','2023-11-14 22:13:20Z');
		ALTER TABLE rooms ADD COLUMN live_id TEXT`); err != nil {
		t.Fatal(err)
	}

	platform, err := app.New(ctx, config)
	if err == nil {
		platform.Close()
		t.Fatal("migration accepted a conflicting live_id column")
	}
	if !strings.Contains(err.Error(), "005_live_session.sql") {
		t.Fatalf("migration error = %v, want failure in 005_live_session.sql", err)
	}
	var historyMissing, attemptsMissing, sequenceMissing bool
	err = db.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NULL,
		to_regclass('room_password_attempts') IS NULL, to_regclass('room_live_id_seq') IS NULL`).
		Scan(&historyMissing, &attemptsMissing, &sequenceMissing)
	if err != nil || !historyMissing || !attemptsMissing || !sequenceMissing {
		t.Fatalf("rollback left migration changes: history missing = %t, attempts missing = %t, sequence missing = %t, error = %v",
			historyMissing, attemptsMissing, sequenceMissing, err)
	}

	if _, err := db.Exec(ctx, "ALTER TABLE rooms DROP COLUMN live_id"); err != nil {
		t.Fatal(err)
	}
	platform, err = app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	postgresMigrationVersions(t, db)
	room, err := platform.Rooms.Info(ctx, 1)
	if err != nil || room.Session.LiveID == 0 || !room.Session.StartedAt.Equal(time.Unix(1_700_000_000, 0)) {
		t.Fatalf("legacy room after retry = %+v, error = %v", room, err)
	}
}

func postgresMigrationVersions(t *testing.T, db *pgx.Conn) []int32 {
	t.Helper()
	var versions []int32
	if err := db.QueryRow(t.Context(), "SELECT ARRAY(SELECT version FROM schema_migrations ORDER BY version)").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if want := []int32{1, 2, 3, 4, 5, 6, 7, 8}; len(versions) < len(want) || !slices.Equal(versions[:len(want)], want) {
		t.Fatalf("migration versions = %v, want at least %v", versions, want)
	}
	return versions
}
