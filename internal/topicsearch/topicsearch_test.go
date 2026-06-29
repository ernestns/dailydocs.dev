package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestSearchTopicFiltersLowValueResults(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{
				{Title: "Video", URL: "https://www.youtube.com/watch?v=rust"},
				{Title: "Forum", URL: "https://www.reddit.com/r/rust/comments/example"},
				{Title: "Social", URL: "https://m.facebook.com/rust-guide"},
				{Title: "PDF", URL: "https://example.com/rust.pdf"},
				{Title: "Rust Docs", URL: "https://doc.rust-lang.org/book/"},
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
		t.Fatalf("expected only one unblocked page, got %d", pageCount)
	}
}

func TestSearchTopicRanksSpecificPagesAheadOfGenericResults(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{
				{Title: "Rust Programming Language", URL: "https://www.rust-lang.org/", Content: "Why Rust? Rust is fast and reliable.", Score: 0.95},
				{Title: "Why Rust Docs Are the Gold Standard", URL: "https://medium.com/example/rust-docs", Content: "Every Rust library has documentation.", Score: 0.9},
				{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html", Content: "Generic types, traits, and lifetimes.", Score: 0.8},
			},
		},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("search topic: %v", err)
	}

	var firstTitle string
	if err := conn.QueryRowContext(ctx, "SELECT title FROM pages WHERE reading_order = 1").Scan(&firstTitle); err != nil {
		t.Fatalf("read first page: %v", err)
	}
	if firstTitle != "Generics" {
		t.Fatalf("expected concept page first, got %q", firstTitle)
	}
}

func TestSearchTopicUsesReviewerToFilterResults(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{
				{Title: "The Rust Programming Language", URL: "https://doc.rust-lang.org/book/", Content: "Learn Rust concepts.", Score: 0.8},
				{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html", Content: "Generic types and traits.", Score: 0.7},
				{Title: "Rust Listicle", URL: "https://example.com/best-rust-posts", Content: "A shallow list.", Score: 0.9},
			},
		},
		Reviewer: fakeReviewer{
			output: ReviewOutput{
				Results: []ReviewResult{
					{Index: 1, DailyDocsScore: 88, PageType: "concept", ShouldStore: true, Reason: "Specific concept page."},
					{Index: 2, DailyDocsScore: 92, PageType: "guide", ShouldStore: true, Reason: "Broad book."},
					{Index: 3, DailyDocsScore: 25, PageType: "listicle", ShouldStore: false, Reason: "Shallow listicle."},
				},
				Model:        "gpt-5-nano-test",
				InputTokens:  100,
				OutputTokens: 40,
				TotalTokens:  140,
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
		t.Fatalf("expected one reviewed page, got %d", pageCount)
	}

	var evaluatedCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM topic_search_results").Scan(&evaluatedCount); err != nil {
		t.Fatalf("count evaluated results: %v", err)
	}
	if evaluatedCount != 3 {
		t.Fatalf("expected three evaluated results, got %d", evaluatedCount)
	}

	var broadAccepted int
	if err := conn.QueryRowContext(ctx, "SELECT accepted FROM topic_search_results WHERE title = 'The Rust Programming Language'").Scan(&broadAccepted); err != nil {
		t.Fatalf("read broad accepted flag: %v", err)
	}
	if broadAccepted != 0 {
		t.Fatalf("expected broad book to be rejected, got accepted=%d", broadAccepted)
	}

	var score int
	var pageType string
	var accepted int
	if err := conn.QueryRowContext(ctx, "SELECT reviewer_score, page_type, accepted FROM topic_search_results WHERE title = 'Generics'").Scan(&score, &pageType, &accepted); err != nil {
		t.Fatalf("read review metadata: %v", err)
	}
	if score != 88 || pageType != "concept" || accepted != 1 {
		t.Fatalf("unexpected review metadata score=%d page_type=%q accepted=%d", score, pageType, accepted)
	}

	var model, stage string
	var totalTokens int
	if err := conn.QueryRowContext(ctx, "SELECT reviewer_model, reviewer_total_tokens, stage FROM topic_search_runs").Scan(&model, &totalTokens, &stage); err != nil {
		t.Fatalf("read review usage: %v", err)
	}
	if model != "gpt-5-nano-test" || totalTokens != 140 {
		t.Fatalf("unexpected review usage model=%q total_tokens=%d", model, totalTokens)
	}
	if stage != "" {
		t.Fatalf("expected completed run stage to be cleared, got %q", stage)
	}
}

func TestSearchTopicKeepsCandidatesWhenReviewerFails(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{
				{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html", Content: "Generic types and traits."},
				{Title: "Ownership", URL: "https://doc.rust-lang.org/stable/book/ch04-00-understanding-ownership.html", Content: "Ownership concepts."},
			},
		},
		Reviewer:    fakeReviewer{err: errors.New("review unavailable")},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err == nil {
		t.Fatal("expected reviewer error")
	}

	var topicStatus, runStatus, runStage string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT status, stage FROM topic_search_runs").Scan(&runStatus, &runStage); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if topicStatus != "failed" || runStatus != "failed" || runStage != "" {
		t.Fatalf("expected failed topic/run with cleared stage, got topic=%q run=%q stage=%q", topicStatus, runStatus, runStage)
	}

	var resultCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM topic_search_results WHERE reviewer_score IS NULL AND accepted = 0").Scan(&resultCount); err != nil {
		t.Fatalf("count saved candidates: %v", err)
	}
	if resultCount != 2 {
		t.Fatalf("expected two saved unreviewed candidates, got %d", resultCount)
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

func TestSearchTopicRateLimitPreservesExistingActiveTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('go', 'Go', 'active')"); err != nil {
		t.Fatalf("seed active topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, started_at)
		VALUES (1, 'tavily', 'Rust docs', 'completed', ?)
	`, formatTime(fixedTopicSearchTime())); err != nil {
		t.Fatalf("seed previous run: %v", err)
	}

	_, err := SearchTopic(ctx, conn, "Go", Options{
		Provider:    fakeProvider{results: []SearchResult{{Title: "Go Docs", URL: "https://go.dev/doc/"}}},
		Now:         func() time.Time { return fixedTopicSearchTime().Add(time.Minute) },
		MinInterval: 5 * time.Minute,
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected rate limit, got %v", err)
	}

	var topicStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'go'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if topicStatus != "active" {
		t.Fatalf("expected active topic to stay active, got %q", topicStatus)
	}
}

func TestProcessNextQueuedTopicProcessesOldestQueuedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics (slug, name, status, created_at) VALUES
			('go', 'Go', 'queued', '2026-06-29 11:00:00'),
			('rust', 'Rust', 'queued', '2026-06-29 10:00:00')
	`); err != nil {
		t.Fatalf("seed queued topics: %v", err)
	}

	result, err := ProcessNextQueuedTopic(ctx, conn, Options{
		Provider: fakeProvider{
			results: []SearchResult{{Title: "Rust Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"}},
		},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("process next queued topic: %v", err)
	}
	if !result.Processed || result.Result.TopicSlug != "rust" {
		t.Fatalf("expected rust to be processed first, got %+v", result)
	}

	var rustStatus, goStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&rustStatus); err != nil {
		t.Fatalf("read rust status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'go'").Scan(&goStatus); err != nil {
		t.Fatalf("read go status: %v", err)
	}
	if rustStatus != "active" || goStatus != "queued" {
		t.Fatalf("unexpected statuses rust=%q go=%q", rustStatus, goStatus)
	}
}

func TestProcessQueuedTopicProcessesSpecificQueuedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics (slug, name, status, created_at) VALUES
			('about', 'About', 'queued', '2026-06-29 10:00:00'),
			('rust', 'Rust', 'queued', '2026-06-29 11:00:00')
	`); err != nil {
		t.Fatalf("seed queued topics: %v", err)
	}

	result, err := ProcessQueuedTopic(ctx, conn, "rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{{Title: "Rust Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"}},
		},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("process queued topic: %v", err)
	}
	if !result.Processed || result.Result.TopicSlug != "rust" {
		t.Fatalf("expected rust to be processed, got %+v", result)
	}

	var rustStatus, aboutStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&rustStatus); err != nil {
		t.Fatalf("read rust status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'about'").Scan(&aboutStatus); err != nil {
		t.Fatalf("read about status: %v", err)
	}
	if rustStatus != "active" || aboutStatus != "queued" {
		t.Fatalf("unexpected statuses rust=%q about=%q", rustStatus, aboutStatus)
	}
}

func TestProcessQueuedTopicRetriesFailedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('rust', 'Rust', 'failed')"); err != nil {
		t.Fatalf("seed failed topic: %v", err)
	}

	result, err := ProcessQueuedTopic(ctx, conn, "rust", Options{
		Provider: fakeProvider{
			results: []SearchResult{{Title: "Rust Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"}},
		},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("retry failed topic: %v", err)
	}
	if !result.Processed || result.Result.TopicSlug != "rust" {
		t.Fatalf("expected rust to be retried, got %+v", result)
	}

	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&status); err != nil {
		t.Fatalf("read rust status: %v", err)
	}
	if status != "active" {
		t.Fatalf("expected retried topic to become active, got %q", status)
	}
}

func TestSearchTopicExpiresStaleRunningSearches(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('env', 'Env', 'queued')"); err != nil {
		t.Fatalf("seed env topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, started_at)
		VALUES (1, 'tavily', 'Env docs', 'running', ?)
	`, formatTime(fixedTopicSearchTime().Add(-time.Hour))); err != nil {
		t.Fatalf("seed stale run: %v", err)
	}

	_, err := SearchTopic(ctx, conn, "Rust", Options{
		Provider:    fakeProvider{results: []SearchResult{{Title: "Rust Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"}}},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("search topic with stale running run: %v", err)
	}

	var staleStatus, staleError string
	if err := conn.QueryRowContext(ctx, "SELECT status, error FROM topic_search_runs WHERE query = 'Env docs'").Scan(&staleStatus, &staleError); err != nil {
		t.Fatalf("read stale run: %v", err)
	}
	if staleStatus != "failed" || staleError != "stale running search timed out" {
		t.Fatalf("expected stale run to fail, got status=%q error=%q", staleStatus, staleError)
	}
}

func TestExpireStaleRunningSearchesFailsSearchingTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('rust', 'Rust', 'searching')"); err != nil {
		t.Fatalf("seed searching topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, stage, started_at)
		VALUES (1, 'tavily', 'Rust docs', 'running', 'reviewing', ?)
	`, formatTime(fixedTopicSearchTime().Add(-time.Hour))); err != nil {
		t.Fatalf("seed stale run: %v", err)
	}

	if err := ExpireStaleRunningSearches(ctx, conn, fixedTopicSearchTime()); err != nil {
		t.Fatalf("expire stale running searches: %v", err)
	}

	var topicStatus, runStatus, runStage string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT status, stage FROM topic_search_runs").Scan(&runStatus, &runStage); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if topicStatus != "failed" || runStatus != "failed" || runStage != "" {
		t.Fatalf("expected failed topic/run with cleared stage, got topic=%q run=%q stage=%q", topicStatus, runStatus, runStage)
	}
}

func TestProcessNextQueuedTopicNoopsWithoutQueuedTopic(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	result, err := ProcessNextQueuedTopic(ctx, conn, Options{
		Provider:    fakeProvider{results: []SearchResult{{Title: "Rust Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"}}},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("process next queued topic: %v", err)
	}
	if result.Processed {
		t.Fatalf("expected no queued topic to process, got %+v", result)
	}
}

func TestProcessNextQueuedTopicStopsAtDailyLimit(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('rust', 'Rust', 'queued')"); err != nil {
		t.Fatalf("seed queued topic: %v", err)
	}
	for i := range 2 {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO topic_search_runs (topic_id, provider, query, status, started_at)
			VALUES (1, 'tavily', ?, 'completed', ?)
		`, fmt.Sprintf("query-%d", i), formatTime(fixedTopicSearchTime())); err != nil {
			t.Fatalf("seed search run: %v", err)
		}
	}

	result, err := ProcessNextQueuedTopic(ctx, conn, Options{
		Provider:    fakeProvider{results: []SearchResult{{Title: "Rust Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"}}},
		Now:         fixedTopicSearchTime,
		MinInterval: time.Nanosecond,
		DailyLimit:  2,
	})
	if err != nil {
		t.Fatalf("process next queued topic: %v", err)
	}
	if result.Processed || !result.DailyLimitReached {
		t.Fatalf("expected daily limit without processing, got %+v", result)
	}

	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&status); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("expected topic to remain queued, got %q", status)
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

type fakeReviewer struct {
	output ReviewOutput
	err    error
}

func (r fakeReviewer) Review(context.Context, string, []ReviewCandidate) (ReviewOutput, error) {
	if r.err != nil {
		return ReviewOutput{}, r.err
	}
	return r.output, nil
}
