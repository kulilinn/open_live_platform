package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int32
	path    string
}

func discoverMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	migrations := make([]migration, 0, len(names))
	versions := make(map[int32]string, len(names))
	for _, name := range names {
		prefix, suffix, found := strings.Cut(strings.TrimSuffix(path.Base(name), ".sql"), "_")
		version, err := strconv.ParseUint(prefix, 10, 31)
		if err != nil || version == 0 || !found || suffix == "" {
			return nil, fmt.Errorf("invalid PostgreSQL migration filename %q", name)
		}
		number := int32(version)
		if previous, exists := versions[number]; exists {
			return nil, fmt.Errorf("duplicate PostgreSQL migration version %d in %q and %q", number, previous, name)
		}
		versions[number] = name
		migrations = append(migrations, migration{version: number, path: name})
	}
	slices.SortFunc(migrations, func(a, b migration) int { return int(a.version - b.version) })
	return migrations, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := discoverMigrations(migrationFiles)
	if err != nil {
		return err
	}
	// Read committed sees migrations committed by a process that held the lock before us.
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(733516, 1)"); err != nil {
			return err
		}
		var schema string
		if err := tx.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
			return err
		}
		history := pgx.Identifier{schema, "schema_migrations"}.Sanitize()
		if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+history+" (version INTEGER PRIMARY KEY)"); err != nil {
			return fmt.Errorf("create PostgreSQL migration history: %w", err)
		}
		var applied []int32
		if err := tx.QueryRow(ctx, "SELECT ARRAY(SELECT version FROM "+history+")").Scan(&applied); err != nil {
			return fmt.Errorf("read PostgreSQL migration history: %w", err)
		}
		// A versionless schema runs the existing idempotent scripts, including their backfills.
		for _, migration := range migrations {
			if slices.Contains(applied, migration.version) {
				continue
			}
			sql, err := migrationFiles.ReadFile(migration.path)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("apply PostgreSQL migration %s: %w", migration.path, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO "+history+" (version) VALUES ($1)", migration.version); err != nil {
				return fmt.Errorf("record PostgreSQL migration %s: %w", migration.path, err)
			}
		}
		return nil
	})
}
