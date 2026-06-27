package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

const DefaultPath = "data/dailydocs.sqlite"

func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		path = DefaultPath
	}

	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable sqlite wal mode: %w", err)
	}

	if err := ApplyMigrations(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return conn, nil
}

func ensureParentDir(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}

	dir := filepath.Dir(path)
	if dir == "." {
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create database directory %q: %w", dir, err)
	}

	return nil
}

func migrationFiles() ([]fs.DirEntry, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var files []fs.DirEntry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		files = append(files, entry)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	return files, nil
}

func migrationVersion(name string) (string, error) {
	version, _, ok := strings.Cut(name, "_")
	if !ok || version == "" {
		return "", fmt.Errorf("migration %q must start with a version prefix", name)
	}
	return version, nil
}

func alreadyApplied(ctx context.Context, tx *sql.Tx, version string) (bool, error) {
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT version FROM schema_migrations WHERE version = ?", version).Scan(&existing)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}
