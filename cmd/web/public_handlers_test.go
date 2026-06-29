package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestTopicsPageListsAllActiveTopics(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	importWebTopic(t, ctx, conn, "go", "Go")
	importWebTopic(t, ctx, conn, "docker", "Docker")

	if _, err := conn.ExecContext(ctx, "UPDATE topics SET status = 'disabled' WHERE slug = 'docker'"); err != nil {
		t.Fatalf("disable topic: %v", err)
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
	if strings.Contains(body, `href="/docker"`) {
		t.Fatalf("did not expect disabled docker topic:\n%s", body)
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
