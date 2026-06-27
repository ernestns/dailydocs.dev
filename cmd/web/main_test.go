package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestSubmissionsPageShowsSafePublicFields(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	form := url.Values{}
	form.Set("url", "https://go.dev/doc/")
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
	if !strings.Contains(body, "go.dev") {
		t.Fatalf("expected source host in queue:\n%s", body)
	}
	if !strings.Contains(body, "Go") {
		t.Fatalf("expected suggested topic in queue:\n%s", body)
	}
	if strings.Contains(body, "https://go.dev/doc") {
		t.Fatalf("did not expect raw submitted URL in public queue:\n%s", body)
	}
}

func TestAdminDisabledReturnsNotFound(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")

	handler := newTestHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", response.Code)
	}
}

func TestAdminLoginSetsSessionCookie(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	handler := newTestHandler(nil)
	form := url.Values{"token": {"test-admin-token"}}
	request := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/admin/submissions" {
		t.Fatalf("expected redirect to admin submissions, got %q", location)
	}
	cookie := findCookie(response.Result().Cookies(), adminSessionCookie)
	if cookie == nil {
		t.Fatal("expected admin session cookie")
	}
	if !cookie.HttpOnly {
		t.Fatal("expected admin session cookie to be HttpOnly")
	}
	if !cookie.Secure {
		t.Fatal("expected admin session cookie to be Secure")
	}
}

func TestAdminRequiresAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	handler := newTestHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/submissions", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/admin/login" {
		t.Fatalf("expected redirect to login, got %q", location)
	}
}

func TestAdminSubmissionsListsSubmissions(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	insertWebSubmission(t, ctx, conn, "https://doc.rust-lang.org/book/", "Rust")

	handler := newTestHandler(conn)
	cookie := adminLoginCookie(t, handler, "test-admin-token")
	request := httptest.NewRequest(http.MethodGet, "/admin/submissions", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "Rust") {
		t.Fatalf("expected Rust submission:\n%s", body)
	}
	if !strings.Contains(body, "/admin/submissions/1") {
		t.Fatalf("expected submission detail link:\n%s", body)
	}
}

func TestAdminCanProcessAndActivateSubmission(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	server := adminDocsServer()
	defer server.Close()
	submissionID := insertWebSubmission(t, ctx, conn, server.URL+"/docs", "Rust")

	handler := newTestHandler(conn)
	cookie := adminLoginCookie(t, handler, "test-admin-token")
	csrf := adminCSRFToken(t, handler, cookie, submissionID)

	processForm := url.Values{"csrf": {csrf}}
	processRequest := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/submissions/%d/process", submissionID), strings.NewReader(processForm.Encode()))
	processRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	processRequest.AddCookie(cookie)
	processResponse := httptest.NewRecorder()
	handler.ServeHTTP(processResponse, processRequest)

	if processResponse.Code != http.StatusSeeOther {
		t.Fatalf("expected process redirect, got %d: %s", processResponse.Code, processResponse.Body.String())
	}

	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM documentation_submissions WHERE id = ?", submissionID).Scan(&status); err != nil {
		t.Fatalf("read processed status: %v", err)
	}
	if status != "candidates_ready" {
		t.Fatalf("expected candidates_ready, got %q", status)
	}

	csrf = adminCSRFToken(t, handler, cookie, submissionID)
	activateForm := url.Values{"csrf": {csrf}}
	activateRequest := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/submissions/%d/activate", submissionID), strings.NewReader(activateForm.Encode()))
	activateRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	activateRequest.AddCookie(cookie)
	activateResponse := httptest.NewRecorder()
	handler.ServeHTTP(activateResponse, activateRequest)

	if activateResponse.Code != http.StatusSeeOther {
		t.Fatalf("expected activate redirect, got %d: %s", activateResponse.Code, activateResponse.Body.String())
	}

	var topicCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM topics WHERE slug = 'rust' AND status = 'active'").Scan(&topicCount); err != nil {
		t.Fatalf("count rust topic: %v", err)
	}
	if topicCount != 1 {
		t.Fatalf("expected active rust topic, got %d", topicCount)
	}
}

func newTestHandler(conn *sql.DB) http.Handler {
	app := app{
		db:  conn,
		now: func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/admin", app.adminHandler)
	mux.HandleFunc("/admin/", app.adminHandler)
	mux.HandleFunc("/submissions", app.submissionsHandler)
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

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func adminLoginCookie(t *testing.T, handler http.Handler, token string) *http.Cookie {
	t.Helper()

	form := url.Values{"token": {token}}
	request := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("admin login failed: %d %s", response.Code, response.Body.String())
	}
	cookie := findCookie(response.Result().Cookies(), adminSessionCookie)
	if cookie == nil {
		t.Fatal("expected admin session cookie")
	}
	return cookie
}

func adminCSRFToken(t *testing.T, handler http.Handler, cookie *http.Cookie, submissionID int64) string {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/submissions/%d", submissionID), nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("admin detail failed: %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	marker := `name="csrf" value="`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("expected csrf token in admin detail:\n%s", body)
	}
	start += len(marker)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatalf("expected csrf token closing quote:\n%s", body)
	}
	return body[start : start+end]
}

func insertWebSubmission(t *testing.T, ctx context.Context, conn *sql.DB, rawURL string, topic string) int64 {
	t.Helper()

	result, err := conn.ExecContext(ctx, `
		INSERT INTO documentation_submissions (
			submitted_url,
			normalized_url,
			source_host,
			suggested_topic,
			status
		)
		VALUES (?, ?, 'example.com', ?, 'pending')
	`, rawURL, rawURL, topic)
	if err != nil {
		t.Fatalf("insert web submission: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read submission id: %v", err)
	}
	return id
}

func adminDocsServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html>
<html>
<head><title>Rust Guide</title></head>
<body>
<main>
<h1>Rust Guide</h1>
<h2>Overview</h2>
<p>%s</p>
<h2>Ownership</h2>
<p>%s</p>
</main>
</body>
</html>`, repeatedWebWords(80), repeatedWebWords(80))
	})
	return httptest.NewServer(mux)
}

func repeatedWebWords(count int) string {
	words := make([]string, 0, count)
	for i := 0; i < count; i++ {
		words = append(words, "documentation")
	}
	return strings.Join(words, " ")
}
