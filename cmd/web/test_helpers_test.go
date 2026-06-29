package main

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/seed"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func newTestHandler(conn *sql.DB) http.Handler {
	return newTestHandlerWithProvider(conn, nil)
}

func newTestHandlerWithProvider(conn *sql.DB, provider topicsearch.Provider) http.Handler {
	app := app{
		db:             conn,
		now:            func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
		searchMu:       &sync.Mutex{},
		searchProvider: provider,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/topics", app.topicsHandler)
	mux.HandleFunc("/topics/search", app.searchTopicsHandler)
	mux.HandleFunc("/read", app.generateReadingHandler)
	mux.HandleFunc("/", app.routeHandler)
	return mux
}

func openWebTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}

func importWebTopic(t *testing.T, ctx context.Context, conn *sql.DB, slug string, name string) {
	t.Helper()

	if _, err := seed.ImportTopic(ctx, conn, seed.TopicFile{
		Topic: slug,
		Name:  name,
		Pages: []seed.PageFile{
			{
				Title:            "Write-Ahead Logging",
				URL:              "https://sqlite.org/wal.html",
				Source:           "SQLite Documentation",
				Official:         true,
				EstimatedMinutes: webIntPtr(12),
			},
		},
	}); err != nil {
		t.Fatalf("import topic: %v", err)
	}
}

func webIntPtr(value int) *int {
	return &value
}
