package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/seed"
)

func TestHomePageListsTopics(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `value="sqlite"`) {
		t.Fatalf("expected sqlite topic option in home page:\n%s", response.Body.String())
	}
}

func TestGenerateReadingRedirectsToTopicURL(t *testing.T) {
	handler := newTestHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=sqlite", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/sqlite" {
		t.Fatalf("expected redirect to /sqlite, got %q", location)
	}
}

func TestReadingPageUsesTodayForTopicOnlyURL(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sqlite", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "2026-06-27") {
		t.Fatalf("expected today's UTC date in body:\n%s", body)
	}
	if !strings.Contains(body, "Write-Ahead Logging") {
		t.Fatalf("expected reading title in body:\n%s", body)
	}
}

func TestDatedReadingPageCreatesAssignment(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "go", "Go")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/go/1970-01-01", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}

	var assignments int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings WHERE reading_date = '1970-01-01'").Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 1 {
		t.Fatalf("expected one assignment, got %d", assignments)
	}
}

func TestTopicSearchReturnsJSON(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	importWebTopic(t, ctx, conn, "go", "Go")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/search?q=sql", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"slug":"sqlite"`) {
		t.Fatalf("expected sqlite in search response: %s", body)
	}
	if strings.Contains(body, `"slug":"go"`) {
		t.Fatalf("did not expect go in filtered search response: %s", body)
	}
}

func newTestHandler(conn *sql.DB) http.Handler {
	app := app{
		db:  conn,
		now: func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
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
