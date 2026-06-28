package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if strings.Contains(response.Body.String(), `>Submit documentation</a>`) {
		t.Fatalf("did not expect static submit documentation link on home page:\n%s", response.Body.String())
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

func TestGenerateReadingRedirectsMissingTopicToSubmission(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic=Rust", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/submissions?topic=Rust" {
		t.Fatalf("expected redirect to /submissions?topic=Rust, got %q", location)
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
	if !strings.Contains(body, `href="/submissions?topic=SQLite"`) {
		t.Fatalf("expected source suggestion link:\n%s", body)
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

func TestSubmissionsPageIsNoindexed(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/submissions", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `<meta name="robots" content="noindex">`) {
		t.Fatalf("expected noindex meta tag:\n%s", body)
	}
	if !strings.Contains(body, "Documentation URL") {
		t.Fatalf("expected submission form:\n%s", body)
	}
	if !strings.Contains(body, "Submit a documentation source URL for a new or existing topic.") {
		t.Fatalf("expected source submission copy:\n%s", body)
	}
}

func TestSubmissionsPagePrefillsTopic(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/submissions?topic=Rust", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `value="Rust"`) {
		t.Fatalf("expected prefilled topic:\n%s", response.Body.String())
	}
}

func TestCreateSubmissionRedirectsAndStoresSubmission(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	form := url.Values{}
	form.Set("url", "https://SQLite.org/docs.html#top")
	form.Set("topic", "SQLite")

	handler := newTestHandler(conn)
	request := httptest.NewRequest(http.MethodPost, "/submissions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "203.0.113.1:12345"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/submissions" {
		t.Fatalf("expected redirect to /submissions, got %q", location)
	}

	var normalizedURL, sourceHost, suggestedTopic string
	var requestCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT normalized_url, source_host, suggested_topic, request_count
		FROM documentation_submissions
	`).Scan(&normalizedURL, &sourceHost, &suggestedTopic, &requestCount); err != nil {
		t.Fatalf("read submission: %v", err)
	}
	if normalizedURL != "https://sqlite.org/docs.html" {
		t.Fatalf("expected normalized url, got %q", normalizedURL)
	}
	if sourceHost != "sqlite.org" {
		t.Fatalf("expected source host sqlite.org, got %q", sourceHost)
	}
	if suggestedTopic != "SQLite" {
		t.Fatalf("expected suggested topic SQLite, got %q", suggestedTopic)
	}
	if requestCount != 1 {
		t.Fatalf("expected request count 1, got %d", requestCount)
	}
}

func TestCreateSubmissionForExistingTopicCreatesSourceAndDiscoveryPreview(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "rust", "Rust")

	server := adminDocsServer()
	defer server.Close()

	form := url.Values{}
	form.Set("url", server.URL+"/docs")
	form.Set("topic", "Rust")

	handler := newTestHandler(conn)
	request := httptest.NewRequest(http.MethodPost, "/submissions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", response.Code, response.Body.String())
	}

	var sourceID int64
	var discoveryCount int
	var discoverySample string
	var createdFrom int64
	if err := conn.QueryRowContext(ctx, `
		SELECT ts.id, ts.discovery_count, ts.discovery_sample, COALESCE(ts.created_from_submission_id, 0)
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE t.slug = 'rust'
	`).Scan(&sourceID, &discoveryCount, &discoverySample, &createdFrom); err != nil {
		t.Fatalf("read auto-created source: %v", err)
	}
	if sourceID < 1 {
		t.Fatalf("expected source id")
	}
	if discoveryCount == 0 {
		t.Fatalf("expected discovery preview count")
	}
	if !strings.Contains(discoverySample, "/docs/ownership") {
		t.Fatalf("expected discovered ownership URL, got %q", discoverySample)
	}
	if createdFrom == 0 {
		t.Fatalf("expected source to be linked to submission")
	}

	var runCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pipeline_runs WHERE topic_source_id = ?", sourceID).Scan(&runCount); err != nil {
		t.Fatalf("count source runs: %v", err)
	}
	if runCount != 0 {
		t.Fatalf("expected auto discovery not to create pipeline runs, got %d", runCount)
	}
}

func TestCreateSubmissionForNewTopicCreatesSourceAndDiscoveryPreview(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	server := adminDocsServer()
	defer server.Close()

	form := url.Values{}
	form.Set("url", server.URL+"/docs")
	form.Set("topic", "Rust")

	handler := newTestHandler(conn)
	request := httptest.NewRequest(http.MethodPost, "/submissions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", response.Code, response.Body.String())
	}

	var sourceID int64
	var discoveryCount int
	var discoverySample string
	var createdFrom int64
	if err := conn.QueryRowContext(ctx, `
		SELECT ts.id, ts.discovery_count, ts.discovery_sample, COALESCE(ts.created_from_submission_id, 0)
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE t.slug = 'rust'
	`).Scan(&sourceID, &discoveryCount, &discoverySample, &createdFrom); err != nil {
		t.Fatalf("read auto-created source: %v", err)
	}
	if sourceID < 1 {
		t.Fatalf("expected source id")
	}
	if discoveryCount == 0 {
		t.Fatalf("expected discovery preview count")
	}
	if !strings.Contains(discoverySample, "/docs/ownership") {
		t.Fatalf("expected discovered ownership URL, got %q", discoverySample)
	}
	if createdFrom == 0 {
		t.Fatalf("expected source to be linked to submission")
	}

	var historyCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM source_discovery_runs WHERE topic_source_id = ?", sourceID).Scan(&historyCount); err != nil {
		t.Fatalf("count discovery history: %v", err)
	}
	if historyCount != 1 {
		t.Fatalf("expected one discovery history row, got %d", historyCount)
	}
}

func TestSubmissionsPageShowsSafePublicFields(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	server := adminDocsServer()
	defer server.Close()

	form := url.Values{}
	form.Set("url", server.URL+"/docs")
	form.Set("topic", "Go")

	handler := newTestHandler(conn)
	request := httptest.NewRequest(http.MethodPost, "/submissions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/submissions", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	serverHost := strings.TrimPrefix(server.URL, "http://")
	if !strings.Contains(body, serverHost) {
		t.Fatalf("expected source host in queue:\n%s", body)
	}
	if !strings.Contains(body, "Go") {
		t.Fatalf("expected suggested topic in queue:\n%s", body)
	}
	if !strings.Contains(body, "Discovered") {
		t.Fatalf("expected public status label in queue:\n%s", body)
	}
	if strings.Contains(body, server.URL+"/docs") {
		t.Fatalf("did not expect raw submitted URL in public queue:\n%s", body)
	}
}
