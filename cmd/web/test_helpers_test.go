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
		_, _ = fmt.Fprint(w, `<!doctype html>
<html>
<head><title>Rust Documentation</title></head>
<body>
<main>
<h1>Rust Documentation</h1>
<a href="/docs/ownership">Ownership</a>
</main>
</body>
</html>`)
	})
	mux.HandleFunc("/docs/ownership", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html>
<html>
<head><title>Ownership Guide</title></head>
<body>
<main>
<h1>Ownership Guide</h1>
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
