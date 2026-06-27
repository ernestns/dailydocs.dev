package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func ApplyMigrations(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	files, err := migrationFiles()
	if err != nil {
		return err
	}

	for _, file := range files {
		version, err := migrationVersion(file.Name())
		if err != nil {
			return err
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", file.Name(), err)
		}

		applied, err := alreadyApplied(ctx, tx, version)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("check migration %s: %w", file.Name(), err)
		}
		if applied {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit skipped migration %s: %w", file.Name(), err)
			}
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + file.Name())
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("read migration %s: %w", file.Name(), err)
		}

		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", file.Name(), err)
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES (?, ?)", version, file.Name()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", file.Name(), err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", file.Name(), err)
		}
	}

	return nil
}
