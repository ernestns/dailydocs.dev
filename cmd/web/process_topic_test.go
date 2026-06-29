package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func TestQueuedTopicPageShowsProcessButton(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{`method="post"`, `action="/process-topic"`, `name="topic" value="rust"`, `Process topic`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in queued page:\n%s", expected, body)
		}
	}
}

func TestProcessTopicProcessesOldestQueuedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")

	handler := newTestHandlerWithProvider(conn, webFakeProvider{
		results: []topicsearch.SearchResult{
			{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"},
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/process-topic", strings.NewReader("topic=rust"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/rust" {
		t.Fatalf("expected redirect to /rust, got %q", location)
	}

	var status string
	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&status); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if status != "active" || pageCount != 1 {
		t.Fatalf("expected active topic with page, got status=%q pages=%d", status, pageCount)
	}
}

func seedQueuedWebTopic(t *testing.T, ctx context.Context, conn *sql.DB, slug string, name string) {
	t.Helper()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES (?, ?, 'queued')", slug, name); err != nil {
		t.Fatalf("seed queued topic: %v", err)
	}
}
