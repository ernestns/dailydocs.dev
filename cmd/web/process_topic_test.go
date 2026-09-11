package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestFailedTopicPageShowsProcessButton(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedWebTopic(t, ctx, conn, "rust", "Rust", "failed")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{`method="post"`, `action="/process-topic"`, `name="topic" value="rust"`, `Process topic`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in failed page:\n%s", expected, body)
		}
	}
}

func TestStaleSearchingTopicPageShowsProcessButton(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedWebTopic(t, ctx, conn, "rust", "Rust", "searching")
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, stage, started_at)
		VALUES ((SELECT id FROM topics WHERE slug = 'rust'), 'tavily', 'Rust docs', 'running', 'reviewing', '2000-01-01 00:00:00')
	`); err != nil {
		t.Fatalf("seed stale run: %v", err)
	}

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{`Needs more readings`, `name="topic" value="rust"`, `Process topic`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in stale searching page:\n%s", expected, body)
		}
	}
}

func TestProcessTopicProcessesRequestedQueuedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "about", "About")
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")

	handler := newTestHandlerWithProvider(conn, webFakeProvider{
		results: []topicsearch.SearchResult{
			{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"},
			{Title: "Ownership", URL: "https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html"},
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

	var rustStatus, aboutStatus string
	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&rustStatus); err != nil {
		t.Fatalf("read rust status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'about'").Scan(&aboutStatus); err != nil {
		t.Fatalf("read about status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if rustStatus != "active" || aboutStatus != "queued" || pageCount != 2 {
		t.Fatalf("expected only rust active with page, got rust=%q about=%q pages=%d", rustStatus, aboutStatus, pageCount)
	}
}

func TestProcessTopicRetriesFailedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedWebTopic(t, ctx, conn, "rust", "Rust", "failed")

	handler := newTestHandlerWithProvider(conn, webFakeProvider{
		results: []topicsearch.SearchResult{
			{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"},
			{Title: "Ownership", URL: "https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html"},
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/process-topic", strings.NewReader("topic=rust"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}

	var status string
	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&status); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if status != "active" || pageCount != 2 {
		t.Fatalf("expected retried topic to become active, got status=%q pages=%d", status, pageCount)
	}
}

func TestProcessTopicDatastarRequestReturnsStatusFragment(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")

	handler := newTestHandlerWithProvider(conn, webFakeProvider{
		results: []topicsearch.SearchResult{
			{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"},
			{Title: "Ownership", URL: "https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html"},
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/process-topic", strings.NewReader("topic=rust"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Datastar-Request", "true")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{`id="topic-status"`, `View reading`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in Datastar response:\n%s", expected, body)
		}
	}
}

func TestProcessTopicReturnsBeforeAsyncProcessingCompletes(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")

	provider := blockingWebProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	handler := newTestHandlerWithProviderMode(conn, provider, true)
	request := httptest.NewRequest(http.MethodPost, "/process-topic", strings.NewReader("topic=rust"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected process-topic response before provider finished")
	}
	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}

	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("expected async provider to start")
	}
	close(provider.release)
	waitForWebTopicStatus(t, ctx, conn, "rust", "active")
}

func seedQueuedWebTopic(t *testing.T, ctx context.Context, conn *sql.DB, slug string, name string) {
	t.Helper()

	seedWebTopic(t, ctx, conn, slug, name, "queued")
}

func waitForWebTopicStatus(t *testing.T, ctx context.Context, conn *sql.DB, slug string, want string) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = ?", slug).Scan(&status); err == nil && status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for topic %q status %q", slug, want)
}

type blockingWebProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p blockingWebProvider) Search(context.Context, string, int) ([]topicsearch.SearchResult, error) {
	close(p.started)
	<-p.release
	return []topicsearch.SearchResult{
		{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"},
		{Title: "Ownership", URL: "https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html"},
	}, nil
}

func seedWebTopic(t *testing.T, ctx context.Context, conn *sql.DB, slug string, name string, status string) {
	t.Helper()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES (?, ?, ?)", slug, name, status); err != nil {
		t.Fatalf("seed topic: %v", err)
	}
}
