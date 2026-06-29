package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dailydocs.sqlite")

	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer conn.Close()

	for _, table := range []string{"topics", "pages", "daily_readings", "imports", "topic_search_runs", "topic_search_results", "schema_migrations"} {
		if !tableExists(ctx, t, conn, table) {
			t.Fatalf("expected table %q to exist", table)
		}
	}
}

func TestApplyMigrationsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dailydocs.sqlite")

	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer conn.Close()

	if err := ApplyMigrations(ctx, conn); err != nil {
		t.Fatalf("apply migrations again: %v", err)
	}

	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if count != len(files) {
		t.Fatalf("expected %d migration records, got %d", len(files), count)
	}
}

func tableExists(ctx context.Context, t *testing.T, conn *sql.DB, name string) bool {
	t.Helper()

	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count); err != nil {
		t.Fatalf("check table %q: %v", name, err)
	}
	return count == 1
}
