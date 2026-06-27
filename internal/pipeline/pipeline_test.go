package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/submission"
)

func TestProcessSubmissionPersistsEligibleCandidates(t *testing.T) {
	ctx := context.Background()
	conn := openPipelineTestDB(t, ctx)
	defer conn.Close()

	server := docsServer()
	defer server.Close()

	sub, err := submission.Create(ctx, conn, submission.CreateInput{
		URL:            server.URL + "/docs/",
		SuggestedTopic: "Example Docs",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}

	result, err := ProcessSubmission(ctx, conn, sub.ID, Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("process submission: %v", err)
	}
	if result.EligibleCount != 3 {
		t.Fatalf("expected 3 eligible candidates, got %+v", result)
	}
	if result.CrawledCount < 3 {
		t.Fatalf("expected at least 3 crawled pages, got %+v", result)
	}

	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM documentation_submissions WHERE id = ?", sub.ID).Scan(&status); err != nil {
		t.Fatalf("read submission status: %v", err)
	}
	if status != "candidates_ready" {
		t.Fatalf("expected candidates_ready status, got %q", status)
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT title, source, primary_classification, score, estimated_minutes
		FROM page_candidates
		WHERE documentation_submission_id = ?
		ORDER BY title
	`, sub.ID)
	if err != nil {
		t.Fatalf("query candidates: %v", err)
	}
	defer rows.Close()

	var titles []string
	for rows.Next() {
		var title, source, classification string
		var score int
		var estimated int
		if err := rows.Scan(&title, &source, &classification, &score, &estimated); err != nil {
			t.Fatalf("scan candidate: %v", err)
		}
		titles = append(titles, title)
		if source == "" {
			t.Fatalf("expected source for %q", title)
		}
		if classification == "" {
			t.Fatalf("expected classification for %q", title)
		}
		if score < DefaultMinScore {
			t.Fatalf("expected score >= %d for %q, got %d", DefaultMinScore, title, score)
		}
		if estimated < 1 {
			t.Fatalf("expected estimated minutes for %q", title)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate candidates: %v", err)
	}

	joined := strings.Join(titles, ",")
	for _, expected := range []string{"Concept Overview", "Docs Home", "Guide Page"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected %q in candidates, got %v", expected, titles)
		}
	}
	if strings.Contains(joined, "Release Notes") {
		t.Fatalf("did not expect release notes candidate, got %v", titles)
	}
}

func TestProcessSubmissionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	conn := openPipelineTestDB(t, ctx)
	defer conn.Close()

	server := docsServer()
	defer server.Close()

	sub, err := submission.Create(ctx, conn, submission.CreateInput{URL: server.URL + "/docs/"})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}

	if _, err := ProcessSubmission(ctx, conn, sub.ID, Options{Client: server.Client(), MaxPages: 10}); err != nil {
		t.Fatalf("process first run: %v", err)
	}
	if _, err := ProcessSubmission(ctx, conn, sub.ID, Options{Client: server.Client(), MaxPages: 10}); err != nil {
		t.Fatalf("process second run: %v", err)
	}

	var candidates int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM page_candidates WHERE documentation_submission_id = ?", sub.ID).Scan(&candidates); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if candidates != 3 {
		t.Fatalf("expected 3 candidates after rerun, got %d", candidates)
	}

	var runs int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pipeline_runs WHERE documentation_submission_id = ?", sub.ID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("expected 2 pipeline runs, got %d", runs)
	}
}

func openPipelineTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}

func docsServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "Sitemap: http://%s/sitemap.xml\n", r.Host)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset>
  <url><loc>http://%s/docs/concepts/overview</loc></url>
  <url><loc>http://%s/docs/releases</loc></url>
  <url><loc>http://%s/blog/out-of-scope</loc></url>
</urlset>`, r.Host, r.Host, r.Host)
	})
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Docs Home", "Docs Home", []string{
			`<a href="/docs/guide">Guide</a>`,
			`<a href="/docs/releases">Release Notes</a>`,
			`<a href="/blog/out-of-scope">Blog</a>`,
		})
	})
	mux.HandleFunc("/docs/guide", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Guide Page", "Guide Page", nil)
	})
	mux.HandleFunc("/docs/concepts/overview", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Concept Overview", "Concept Overview", nil)
	})
	mux.HandleFunc("/docs/releases", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Release Notes", "Release Notes", nil)
	})
	mux.HandleFunc("/blog/out-of-scope", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Out of Scope", "Out of Scope", nil)
	})
	return httptest.NewServer(mux)
}

func writeDoc(w http.ResponseWriter, title string, h1 string, links []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html>
<head>
  <title>%s</title>
  <meta name="description" content="%s">
</head>
<body>
  <nav>%s</nav>
  <main>
    <h1>%s</h1>
    <h2>First section</h2>
    <p>%s</p>
    <h2>Second section</h2>
    <p>%s</p>
  </main>
</body>
</html>`, title, title, strings.Join(links, "\n"), h1, repeatedWords(80), repeatedWords(80))
}

func repeatedWords(count int) string {
	words := make([]string, 0, count)
	for i := 0; i < count; i++ {
		words = append(words, "documentation")
	}
	return strings.Join(words, " ")
}
