package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/topicsearch"
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
	if !strings.Contains(response.Body.String(), `href="/sqlite"`) {
		t.Fatalf("expected sqlite topic link in home page:\n%s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), `Submit documentation`) {
		t.Fatalf("did not expect documentation submission copy on home page:\n%s", response.Body.String())
	}
}

func TestHomePageRendersTopicCombobox(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "rust", "Rust")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`role="combobox"`,
		`aria-autocomplete="list"`,
		`aria-controls="topic-results"`,
		`role="listbox"`,
		`id="topic-results"`,
		`ArrowDown`,
		`aria-activedescendant`,
		`Request Topic`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in home page:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "<datalist") {
		t.Fatalf("did not expect datalist after combobox replacement:\n%s", body)
	}
}

func TestGenerateReadingRedirectsToTopicURL(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=sqlite", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/sqlite" {
		t.Fatalf("expected redirect to /sqlite, got %q", location)
	}
}

func TestGenerateReadingRedirectsTopicNameToTopicURL(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=SQLite", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/sqlite" {
		t.Fatalf("expected redirect to /sqlite, got %q", location)
	}
}

func TestGenerateReadingQueuesMissingTopic(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=Rust", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/rust" {
		t.Fatalf("expected redirect to /rust, got %q", location)
	}

	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&status); err != nil {
		t.Fatalf("read queued topic: %v", err)
	}
	if status != "queued" {
		t.Fatalf("expected queued topic, got %q", status)
	}
}

func TestGenerateReadingKeepsTopicQueuedWithoutProvider(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=Rust", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}

	var topicStatus string
	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if topicStatus != "queued" || pageCount != 0 {
		t.Fatalf("expected queued topic without provider, got status=%q pages=%d", topicStatus, pageCount)
	}
}

func TestGenerateReadingProcessesMissingTopicWhenProviderExists(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandlerWithProvider(conn, webFakeProvider{
		results: []topicsearch.SearchResult{
			{Title: "Rust Book", URL: "https://doc.rust-lang.org/book/"},
		},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=Rust", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/rust" {
		t.Fatalf("expected redirect to /rust, got %q", location)
	}

	var topicStatus string
	var pageCount int
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug = 'rust'").Scan(&topicStatus); err != nil {
		t.Fatalf("read topic status: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if topicStatus != "active" || pageCount != 1 {
		t.Fatalf("expected active topic with inline pages, got status=%q pages=%d", topicStatus, pageCount)
	}
}

func TestMissingTopicPageShowsFailedStateWhenProviderFails(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandlerWithProvider(conn, webFakeProvider{err: errors.New("search unavailable")})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "failed") {
		t.Fatalf("expected failed state in body:\n%s", body)
	}
}

func TestMissingTopicPageProcessesRequestedTopicWhenProviderExists(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "about", "About")

	handler := newTestHandlerWithProvider(conn, webFakeProvider{
		results: []topicsearch.SearchResult{
			{Title: "Generics", URL: "https://doc.rust-lang.org/stable/book/ch10-00-generics.html"},
		},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", response.Code, response.Body.String())
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
	if rustStatus != "active" || aboutStatus != "queued" || pageCount != 1 {
		t.Fatalf("expected only rust active with page, got rust=%q about=%q pages=%d", rustStatus, aboutStatus, pageCount)
	}
}

func TestMissingTopicPageShowsQueuedState(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "Rust") {
		t.Fatalf("expected topic name in queued page:\n%s", body)
	}
	if !strings.Contains(body, "queued") {
		t.Fatalf("expected queued state in body:\n%s", body)
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
	if strings.Contains(body, `/submissions`) {
		t.Fatalf("did not expect submission link:\n%s", body)
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

func TestTopicSearchReturnsCaseInsensitivePartialMatches(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "rust", "Rust")
	importWebTopic(t, ctx, conn, "go", "Go")

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/search?q=Ru", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"slug":"rust"`) {
		t.Fatalf("expected rust in search response: %s", body)
	}
	if strings.Contains(body, `"slug":"go"`) {
		t.Fatalf("did not expect go in filtered search response: %s", body)
	}
}

func TestTopicsPageListsRequestedTopicsWithStatus(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	importWebTopic(t, ctx, conn, "go", "Go")
	importWebTopic(t, ctx, conn, "docker", "Docker")

	if _, err := conn.ExecContext(ctx, "UPDATE topics SET status = 'queued' WHERE slug = 'go'"); err != nil {
		t.Fatalf("queue topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE topics SET status = 'failed' WHERE slug = 'docker'"); err != nil {
		t.Fatalf("fail topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name, status) VALUES ('rust', 'Rust', 'searching')"); err != nil {
		t.Fatalf("seed searching topic: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, stage)
		VALUES ((SELECT id FROM topics WHERE slug = 'rust'), 'tavily', 'Rust docs', 'running', 'reviewing')
	`); err != nil {
		t.Fatalf("seed searching run: %v", err)
	}

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `href="/go"`) {
		t.Fatalf("expected go topic link:\n%s", body)
	}
	if !strings.Contains(body, `href="/sqlite"`) {
		t.Fatalf("expected sqlite topic link:\n%s", body)
	}
	if !strings.Contains(body, `queued`) {
		t.Fatalf("expected queued status:\n%s", body)
	}
	if !strings.Contains(body, `failed`) {
		t.Fatalf("expected failed status:\n%s", body)
	}
	if !strings.Contains(body, `reviewing`) {
		t.Fatalf("expected reviewing stage:\n%s", body)
	}
	if strings.Contains(body, `>Evaluations<`) {
		t.Fatalf("did not expect separate evaluations link:\n%s", body)
	}
	if strings.Count(body, `href="/topics/sqlite/evaluations"`) < 2 {
		t.Fatalf("expected linked accepted and evaluated counts:\n%s", body)
	}
}

func TestTopicEvaluationsPageListsReviewedCandidates(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "rust", "Rust")

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, result_count, stored_count)
		VALUES (1, 'tavily', 'Rust concepts', 'completed', 2, 1)
	`); err != nil {
		t.Fatalf("seed search run: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_results (
			topic_id,
			search_run_id,
			title,
			url,
			source,
			snippet,
			rank,
			reviewer_score,
			page_type,
			reviewer_reason,
			accepted,
			stored_as_page_id
		)
		VALUES
			(1, 1, 'Generics', 'https://doc.rust-lang.org/stable/book/ch10-00-generics.html', 'doc.rust-lang.org', '', 1, 92, 'concept', 'Specific concept page.', 1, 1),
			(1, 1, 'The Rust Book', 'https://doc.rust-lang.org/book', 'doc.rust-lang.org', '', 2, 40, 'landing', 'Too broad.', 0, NULL)
	`); err != nil {
		t.Fatalf("seed search results: %v", err)
	}

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/rust/evaluations", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{"Generics", "Accepted", "The Rust Book", "Rejected", "Too broad."} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in evaluations page:\n%s", expected, body)
		}
	}
}

func TestTopicsPageRequiresGet(t *testing.T) {
	handler := newTestHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/topics", nil))

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", response.Code)
	}
}

type webFakeProvider struct {
	results []topicsearch.SearchResult
	err     error
}

func (p webFakeProvider) Search(context.Context, string, int) ([]topicsearch.SearchResult, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.results, nil
}
