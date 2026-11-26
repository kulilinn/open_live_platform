package postgres

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDiscoverMigrations(t *testing.T) {
	files := fstest.MapFS{
		"migrations/001_first.sql":   {},
		"migrations/009_second.sql":  {},
		"migrations/999_third.sql":   {},
		"migrations/1000_fourth.sql": {},
		"migrations/README.md":       {},
	}
	migrations, err := discoverMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int32
	for _, migration := range migrations {
		versions = append(versions, migration.version)
	}
	if want := []int32{1, 9, 999, 1000}; !slices.Equal(versions, want) {
		t.Fatalf("migration versions = %v, want %v", versions, want)
	}
}

func TestDiscoverMigrationsRejectsInvalidVersions(t *testing.T) {
	for _, tc := range []struct{ filename, message string }{
		{"migrations/no_version.sql", "invalid PostgreSQL migration filename"},
		{"migrations/000_zero.sql", "invalid PostgreSQL migration filename"},
		{"migrations/002_.sql", "invalid PostgreSQL migration filename"},
		{"migrations/0001_duplicate.sql", "duplicate PostgreSQL migration version 1"},
	} {
		t.Run(tc.filename, func(t *testing.T) {
			_, err := discoverMigrations(fstest.MapFS{
				"migrations/001_first.sql": {},
				tc.filename:                {},
			})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("migration discovery error = %v, want %q", err, tc.message)
			}
		})
	}
}
