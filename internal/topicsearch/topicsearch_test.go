package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
)

func TestSearchTopicStoresResultsAndActivatesTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	provider := fakeProvider{
		results: []SearchResult{
			{Title: "Rust Book", URL: "https://doc.rust-lang.org/book/?utm_source=test#top", Content: "Learn Rust."},
			{Title: "Cargo Book", URL: "https://doc.rust-lang.org/cargo/", Content: "Cargo guide."},
		},
	}

	result, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider:    provider,
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("search topic: %v", err)
	}
	if result.Status != "completed" || result.StoredCount != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}

	var topicStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if topicStatus != "active" {
		t.Fatalf("expected active topic, got %q", topicStatus)
	}

	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages WHERE topic_id = ? AND active = 1", result.TopicID).Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if pageCount != 2 {
		t.Fatalf("expected 2 pages, got %d", pageCount)
	}

	var normalizedURL string
	if err := conn.QueryRowContext(ctx, "SELECT url FROM pages WHERE title = 'Rust Book'").Scan(&normalizedURL); err != nil {
		t.Fatalf("read normalized url: %v", err)
	}
	if normalizedURL != "https://doc.rust-lang.org/book" {
		t.Fatalf("expected normalized url, got %q", normalizedURL)
	}
}

func TestSearchTopicDeduplicatesResults(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{
				{Title: "Rust Book", URL: "https://doc.rust-lang.org/book/"},
				{Title: "Rust Book Duplicate", URL: "https://doc.rust-lang.org/book#top"},
				{Title: "", URL: "https://doc.rust-lang.org/nomicon/"},
			},
		},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("search topic: %v", err)
	}

	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if pageCount != 1 {
		t.Fatalf("expected 1 deduped page, got %d", pageCount)
	}
}

func TestSearchTopicAppendsReadingOrderForExistingTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('rust', 'Rust', 'active')"); err != nil {
		t.Fatalf("seed existing topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO pages (topic_id, title, url, reading_order) VALUES (1, 'Existing', 'https://example.com/existing', 1)"); err != nil {
		t.Fatalf("seed existing page: %v", err)
	}

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{
				{Title: "Rust Book", URL: "https://doc.rust-lang.org/book/"},
			},
		},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("search topic: %v", err)
	}

	var readingOrder int
	if err := conn.QueryRowContext(ctx, "SELECT reading_order FROM pages WHERE title = 'Rust Book'").Scan(&readingOrder); err != nil {
		t.Fatalf("read reading order: %v", err)
	}
	if readingOrder != 2 {
		t.Fatalf("expected appended reading order 2, got %d", readingOrder)
	}
}

func TestSearchTopicRecordsProviderFailure(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider:    fakeProvider{err: errors.New("provider unavailable")},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err == nil {
		t.Fatal("expected provider error")
	}

	var topicStatus, runStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topic_search_runs").Scan(&runStatus); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if topicStatus != "failed" || runStatus != "failed" {
		t.Fatalf("expected failed statuses, got topic=%q run=%q", topicStatus, runStatus)
	}
}

func TestSearchTopicRateLimitsGlobally(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider:    fakeProvider{results: []SearchResult{{Title: "Rust Book", URL: "https://doc.rust-lang.org/book/"}}},
		Now:         fixedTopicSearchTime,
		MinInterval: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("first search: %v", err)
	}

	result, err := SearchTopic(ctx, conn, "Go", Options{
		Provider:    fakeProvider{results: []SearchResult{{Title: "Go Docs", URL: "https://go.dev/doc/"}}},
		Now:         func() time.Time { return fixedTopicSearchTime().Add(time.Minute) },
		MinInterval: 5 * time.Minute,
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected rate limit, got result=%+v err=%v", result, err)
	}
	if !result.RateLimited || result.Status != "rate_limited" {
		t.Fatalf("expected rate limited result, got %+v", result)
	}

	var topicStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'go'").Scan(&topicStatus); err != nil {
		t.Fatalf("read queued topic: %v", err)
	}
	if topicStatus != "queued" {
		t.Fatalf("expected queued topic, got %q", topicStatus)
	}
}

func openTopicSearchTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}

func fixedTopicSearchTime() time.Time {
	return time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
}

type fakeProvider struct {
	results []SearchResult
	err     error
}

func (p fakeProvider) Search(context.Context, string, int) ([]SearchResult, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.results, nil
}
