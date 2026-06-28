package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/submission"
	"github.com/ernestns/daily-docs/internal/topicsource"
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
	if result.EligibleCount != 2 {
		t.Fatalf("expected 2 eligible candidates, got %+v", result)
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
			AND status = 'eligible'
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
	for _, expected := range []string{"Concept Overview", "Guide Page"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected %q in candidates, got %v", expected, titles)
		}
	}
	if strings.Contains(joined, "Docs Home") {
		t.Fatalf("did not expect docs home candidate, got %v", titles)
	}
	if strings.Contains(joined, "Release Notes") {
		t.Fatalf("did not expect release notes candidate, got %v", titles)
	}

	var rejected int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM page_candidates WHERE documentation_submission_id = ? AND status = 'rejected'", sub.ID).Scan(&rejected); err != nil {
		t.Fatalf("count rejected candidates: %v", err)
	}
	if rejected == 0 {
		t.Fatalf("expected rejected candidates to be persisted")
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
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM page_candidates WHERE documentation_submission_id = ? AND status = 'eligible'", sub.ID).Scan(&candidates); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if candidates != 2 {
		t.Fatalf("expected 2 eligible candidates after rerun, got %d", candidates)
	}

	var allCandidates int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM page_candidates WHERE documentation_submission_id = ?", sub.ID).Scan(&allCandidates); err != nil {
		t.Fatalf("count all candidates: %v", err)
	}
	if allCandidates != 4 {
		t.Fatalf("expected 4 persisted candidates after rerun, got %d", allCandidates)
	}

	var runs int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pipeline_runs WHERE documentation_submission_id = ?", sub.ID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("expected 2 pipeline runs, got %d", runs)
	}
}

func TestProcessSourcePersistsSourceLinkedCandidates(t *testing.T) {
	ctx := context.Background()
	conn := openPipelineTestDB(t, ctx)
	defer conn.Close()

	server := docsServer()
	defer server.Close()

	sub, err := submission.Create(ctx, conn, submission.CreateInput{
		URL:            server.URL + "/docs/",
		SuggestedTopic: "Docs",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	source, err := topicsource.CreateFromSubmission(ctx, conn, topicsource.CreateFromSubmissionInput{
		SubmissionID: sub.ID,
		TopicSlug:    "docs",
		TopicName:    "Docs",
	})
	if err != nil {
		t.Fatalf("create topic source: %v", err)
	}

	result, err := ProcessSource(ctx, conn, source.ID, Options{Client: server.Client()})
	if err != nil {
		t.Fatalf("process source: %v", err)
	}
	if result.TopicSourceID != source.ID {
		t.Fatalf("expected result source id %d, got %+v", source.ID, result)
	}

	var runSourceID sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT topic_source_id FROM pipeline_runs WHERE id = ?", result.PipelineRunID).Scan(&runSourceID); err != nil {
		t.Fatalf("read run source id: %v", err)
	}
	if !runSourceID.Valid || runSourceID.Int64 != source.ID {
		t.Fatalf("expected linked run source id, got %+v", runSourceID)
	}

	var candidateCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM page_candidates
		WHERE topic_source_id = ?
			AND topic_id = ?
	`, source.ID, source.TopicID).Scan(&candidateCount); err != nil {
		t.Fatalf("count source candidates: %v", err)
	}
	if candidateCount == 0 {
		t.Fatalf("expected source-linked candidates")
	}

	var processed sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT last_processed_at FROM topic_sources WHERE id = ?", source.ID).Scan(&processed); err != nil {
		t.Fatalf("read source processed timestamp: %v", err)
	}
	if !processed.Valid || processed.String == "" {
		t.Fatalf("expected source last_processed_at to be set")
	}
}

func TestProcessSourcePersistsReviewTokenUsage(t *testing.T) {
	ctx := context.Background()
	conn := openPipelineTestDB(t, ctx)
	defer conn.Close()

	server := docsServer()
	defer server.Close()

	sub, err := submission.Create(ctx, conn, submission.CreateInput{
		URL:            server.URL + "/docs/",
		SuggestedTopic: "Docs",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	source, err := topicsource.CreateFromSubmission(ctx, conn, topicsource.CreateFromSubmissionInput{
		SubmissionID: sub.ID,
		TopicSlug:    "docs",
		TopicName:    "Docs",
	})
	if err != nil {
		t.Fatalf("create topic source: %v", err)
	}

	_, err = ProcessSource(ctx, conn, source.ID, Options{
		Client:   server.Client(),
		Reviewer: usageReviewer{},
	})
	if err != nil {
		t.Fatalf("process source: %v", err)
	}

	var gateInput int
	var gateOutput int
	var gateReasoning int
	var gateTotal int
	var enrichmentInput int
	var enrichmentOutput int
	var enrichmentReasoning int
	var enrichmentTotal int
	err = conn.QueryRowContext(ctx, `
		SELECT
			gate_input_tokens,
			gate_output_tokens,
			gate_reasoning_tokens,
			gate_total_tokens,
			enrichment_input_tokens,
			enrichment_output_tokens,
			enrichment_reasoning_tokens,
			enrichment_total_tokens
		FROM page_candidates
		WHERE topic_source_id = ?
			AND status = 'eligible'
		ORDER BY id
		LIMIT 1
	`, source.ID).Scan(&gateInput, &gateOutput, &gateReasoning, &gateTotal, &enrichmentInput, &enrichmentOutput, &enrichmentReasoning, &enrichmentTotal)
	if err != nil {
		t.Fatalf("read token usage: %v", err)
	}
	if gateInput != 620 || gateOutput != 84 || gateReasoning != 128 || gateTotal != 704 {
		t.Fatalf("expected persisted gate token usage, got input=%d output=%d reasoning=%d total=%d", gateInput, gateOutput, gateReasoning, gateTotal)
	}
	if enrichmentInput != 980 || enrichmentOutput != 210 || enrichmentReasoning != 256 || enrichmentTotal != 1190 {
		t.Fatalf("expected persisted enrichment token usage, got input=%d output=%d reasoning=%d total=%d", enrichmentInput, enrichmentOutput, enrichmentReasoning, enrichmentTotal)
	}
}

func TestProcessSourceMarksBroadSourceAsNeedsScope(t *testing.T) {
	ctx := context.Background()
	conn := openPipelineTestDB(t, ctx)
	defer conn.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		links := make([]string, 0, DefaultMaxDiscovered+1)
		for i := 0; i <= DefaultMaxDiscovered; i++ {
			links = append(links, fmt.Sprintf(`<a href="/docs/page-%d.html">Page %d</a>`, i, i))
		}
		writeDoc(w, "Docs Home", "Docs Home", links)
	})
	mux.HandleFunc("/docs/page-", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Generated Page", "Generated Page", nil)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	sub, err := submission.Create(ctx, conn, submission.CreateInput{
		URL:            server.URL + "/docs/",
		SuggestedTopic: "Docs",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	source, err := topicsource.CreateFromSubmission(ctx, conn, topicsource.CreateFromSubmissionInput{
		SubmissionID: sub.ID,
		TopicSlug:    "docs",
		TopicName:    "Docs",
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	_, err = ProcessSource(ctx, conn, source.ID, Options{Client: server.Client()})
	if err == nil {
		t.Fatalf("expected broad discovery error")
	}
	var tooBroad DiscoveryTooBroadError
	if !errors.As(err, &tooBroad) {
		t.Fatalf("expected DiscoveryTooBroadError, got %T %v", err, err)
	}

	var status string
	var lastError string
	if err := conn.QueryRowContext(ctx, "SELECT status, last_error FROM topic_sources WHERE id = ?", source.ID).Scan(&status, &lastError); err != nil {
		t.Fatalf("read source status: %v", err)
	}
	if status != "needs_scope" {
		t.Fatalf("expected needs_scope source, got %q", status)
	}
	if !strings.Contains(lastError, "too broad") {
		t.Fatalf("expected broad source error, got %q", lastError)
	}
}

func TestProcessSubmissionDiscoversLinkedDocumentationPages(t *testing.T) {
	ctx := context.Background()
	conn := openPipelineTestDB(t, ctx)
	defer conn.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Docs Home", "Docs Home", []string{
			`<a href="/docs/book/">Book</a>`,
		})
	})
	mux.HandleFunc("/docs/book/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Book Home", "Book Home", []string{
			`<a href="/docs/book/chapter-1">Chapter 1</a>`,
		})
	})
	mux.HandleFunc("/docs/book/chapter-1", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Chapter 1", "Chapter 1", nil)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	sub, err := submission.Create(ctx, conn, submission.CreateInput{URL: server.URL + "/docs/"})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}

	result, err := ProcessSubmission(ctx, conn, sub.ID, Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("process submission: %v", err)
	}
	if result.DiscoveredCount != 3 {
		t.Fatalf("expected 3 discovered pages, got %+v", result)
	}

	var exists bool
	if err := conn.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM page_candidates
			WHERE documentation_submission_id = ?
				AND title = 'Chapter 1'
		)
	`, sub.ID).Scan(&exists); err != nil {
		t.Fatalf("check chapter candidate: %v", err)
	}
	if !exists {
		t.Fatalf("expected recursively discovered chapter candidate")
	}
}

func TestDiscoverURLReturnsScopedCandidateURLs(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Docs Home", "Docs Home", []string{
			`<a href="/docs/guide">Guide</a>`,
			`<a href="/docs/book/">Book</a>`,
			`<a href="/blog/out-of-scope">Blog</a>`,
		})
	})
	mux.HandleFunc("/docs/book/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Book Home", "Book Home", []string{
			`<a href="/docs/book/chapter-1">Chapter 1</a>`,
		})
	})
	mux.HandleFunc("/docs/book/chapter-1", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Chapter 1", "Chapter 1", nil)
	})
	mux.HandleFunc("/docs/guide", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Guide", "Guide", nil)
	})
	mux.HandleFunc("/blog/out-of-scope", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Blog", "Blog", nil)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := DiscoverURL(ctx, server.URL+"/docs/", Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("discover url: %v", err)
	}
	if result.NormalizedURL != server.URL+"/docs" {
		t.Fatalf("expected normalized base URL, got %q", result.NormalizedURL)
	}
	if result.DiscoveredCount != 4 {
		t.Fatalf("expected 4 discovered URLs, got %+v", result)
	}
	for _, expected := range []string{server.URL + "/docs", server.URL + "/docs/book", server.URL + "/docs/book/chapter-1", server.URL + "/docs/guide"} {
		if !containsString(result.URLs, expected) {
			t.Fatalf("expected discovered URL %q, got %v", expected, result.URLs)
		}
	}
	if containsString(result.URLs, server.URL+"/blog/out-of-scope") {
		t.Fatalf("did not expect out-of-scope URL, got %v", result.URLs)
	}
}

func TestDiscoverURLScopesFileSourceToParentDirectory(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Docs Index", "Docs Index", []string{
			`<a href="/wal.html">WAL</a>`,
			`<a href="/queryplanner.html">Query Planner</a>`,
			`<a href="/news.html">News</a>`,
		})
	})
	mux.HandleFunc("/wal.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "WAL", "WAL", nil)
	})
	mux.HandleFunc("/queryplanner.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Query Planner", "Query Planner", nil)
	})
	mux.HandleFunc("/news.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "News", "News", nil)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := DiscoverURL(ctx, server.URL+"/docs.html", Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("discover url: %v", err)
	}
	for _, expected := range []string{server.URL + "/docs.html", server.URL + "/wal.html", server.URL + "/queryplanner.html"} {
		if !containsString(result.URLs, expected) {
			t.Fatalf("expected discovered URL %q, got %v", expected, result.URLs)
		}
	}
}

func TestDiscoverURLSkipsAssetLinks(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Docs Home", "Docs Home", []string{
			`<a href="/docs/guide.html">Guide</a>`,
			`<a href="/docs/gopher.jpg">Gopher</a>`,
			`<a href="/docs/app.js">Script</a>`,
		})
	})
	mux.HandleFunc("/docs/guide.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Guide", "Guide", nil)
	})
	mux.HandleFunc("/docs/gopher.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpg"))
	})
	mux.HandleFunc("/docs/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("js"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := DiscoverURL(ctx, server.URL+"/docs/", Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("discover url: %v", err)
	}
	if containsString(result.URLs, server.URL+"/docs/gopher.jpg") || containsString(result.URLs, server.URL+"/docs/app.js") {
		t.Fatalf("did not expect asset URLs, got %v", result.URLs)
	}
	if !containsString(result.URLs, server.URL+"/docs/guide.html") {
		t.Fatalf("expected guide URL, got %v", result.URLs)
	}
}

func TestDiscoverURLFollowsSitemapIndex(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/concepts/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Concepts", "Concepts", nil)
	})
	mux.HandleFunc("/docs/concepts/workloads/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Workloads", "Workloads", nil)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
	<sitemap><loc>%s/docs-sitemap.xml</loc></sitemap>
</sitemapindex>`, serverHost(r))
	})
	mux.HandleFunc("/docs-sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
	<url><loc>%s/docs/concepts/workloads/</loc></url>
	<url><loc>%s/blog/out-of-scope/</loc></url>
</urlset>`, serverHost(r), serverHost(r))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := DiscoverURL(ctx, server.URL+"/docs/concepts/", Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("discover url: %v", err)
	}
	if !containsString(result.URLs, server.URL+"/docs/concepts/workloads") {
		t.Fatalf("expected sitemap child URL, got %v", result.URLs)
	}
	if containsString(result.URLs, server.URL+"/blog/out-of-scope") {
		t.Fatalf("did not expect out-of-scope sitemap URL, got %v", result.URLs)
	}
}

func TestDiscoverURLFollowsEmbeddedNavigationFrame(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Docs Home", "Docs Home", []string{
			`<a href="/docs/book/">Book</a>`,
		})
	})
	mux.HandleFunc("/docs/book/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html>
<html>
<head><title>Book</title></head>
<body>
	<nav><iframe src="toc.html"></iframe></nav>
	<main><h1>Book</h1></main>
</body>
</html>`)
	})
	mux.HandleFunc("/docs/book/toc.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Table of Contents", "Table of Contents", []string{
			`<a href="chapter-1.html">Chapter 1</a>`,
			`<a href="chapter-2.html">Chapter 2</a>`,
		})
	})
	mux.HandleFunc("/docs/book/chapter-1.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Chapter 1", "Chapter 1", nil)
	})
	mux.HandleFunc("/docs/book/chapter-2.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Chapter 2", "Chapter 2", nil)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := DiscoverURL(ctx, server.URL+"/docs/", Options{Client: server.Client(), MaxPages: 10})
	if err != nil {
		t.Fatalf("discover url: %v", err)
	}
	for _, expected := range []string{server.URL + "/docs/book/toc.html", server.URL + "/docs/book/chapter-1.html", server.URL + "/docs/book/chapter-2.html"} {
		if !containsString(result.URLs, expected) {
			t.Fatalf("expected embedded navigation URL %q, got %v", expected, result.URLs)
		}
	}
}

func TestDiscoverURLPrioritizesCurrentPageChildrenBeforeNoisySiblings(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		links := []string{
			`<a href="/docs/book/">Book</a>`,
			`<a href="/docs/reference/">Reference</a>`,
		}
		writeDoc(w, "Docs Home", "Docs Home", links)
	})
	mux.HandleFunc("/docs/book/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html>
<html>
<head><title>Book</title></head>
<body>
	<nav><iframe src="toc.html"></iframe></nav>
	<main><h1>Book</h1></main>
</body>
</html>`)
	})
	mux.HandleFunc("/docs/book/toc.html", func(w http.ResponseWriter, r *http.Request) {
		writeDoc(w, "Table of Contents", "Table of Contents", []string{
			`<a href="chapter-1.html">Chapter 1</a>`,
			`<a href="chapter-2.html">Chapter 2</a>`,
		})
	})
	mux.HandleFunc("/docs/reference/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/docs/reference/" {
			writeDoc(w, "Reference Item", "Reference Item", nil)
			return
		}
		links := []string{}
		for i := 0; i < 20; i++ {
			links = append(links, fmt.Sprintf(`<a href="/docs/reference/item-%d.html">Item %d</a>`, i, i))
		}
		writeDoc(w, "Reference", "Reference", links)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := DiscoverURL(ctx, server.URL+"/docs/", Options{Client: server.Client(), MaxPages: 8})
	if err != nil {
		t.Fatalf("discover url: %v", err)
	}
	for _, expected := range []string{server.URL + "/docs/book/toc.html", server.URL + "/docs/book/chapter-1.html", server.URL + "/docs/book/chapter-2.html"} {
		if !containsString(result.URLs, expected) {
			t.Fatalf("expected current page child URL %q before noisy siblings, got %v", expected, result.URLs)
		}
	}
}

func TestOpenAIReviewerSkipsEnrichmentBelowGateThreshold(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeOpenAIResponse(t, w, map[string]any{
			"dailydocs_score": 30,
			"page_type":       "index_page",
		})
	}))
	defer server.Close()

	reviewer := openAIReviewer{
		client:   server.Client(),
		apiKey:   "test-key",
		model:    "gpt-5-nano",
		endpoint: server.URL,
	}

	review, err := reviewer.ReviewPage(ctx, document{
		Title:         "Docs Home",
		NormalizedURL: server.URL + "/docs",
		H1:            "Docs Home",
		Headings:      []string{"Start"},
		WordCount:     500,
	})
	if err != nil {
		t.Fatalf("review page: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one gate call, got %d", calls)
	}
	if review.Decision != "exclude" {
		t.Fatalf("expected exclude decision, got %+v", review)
	}
	if review.GateScore != 30 || review.RejectStage != "ai_gate" {
		t.Fatalf("expected gate rejection metadata, got %+v", review)
	}
	if review.GateUsage.InputTokens != 620 || review.GateUsage.OutputTokens != 84 || review.GateUsage.ReasoningTokens != 128 || review.GateUsage.TotalTokens != 704 {
		t.Fatalf("expected gate token usage, got %+v", review.GateUsage)
	}
	if review.EnrichmentUsage.TotalTokens != 0 {
		t.Fatalf("did not expect enrichment usage below gate threshold, got %+v", review.EnrichmentUsage)
	}
}

func TestOpenAIReviewerEnrichesAboveGateThreshold(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeOpenAIResponse(t, w, map[string]any{
				"dailydocs_score": 80,
				"page_type":       "concept",
			})
			return
		}
		writeOpenAIResponse(t, w, map[string]any{
			"decision":          "include",
			"classification":    "Concept",
			"confidence":        0.91,
			"estimated_minutes": 7,
			"rationale":         "Standalone concept page.",
			"rejection_reason":  "",
		})
	}))
	defer server.Close()

	reviewer := openAIReviewer{
		client:   server.Client(),
		apiKey:   "test-key",
		model:    "gpt-5-nano",
		endpoint: server.URL,
	}

	review, err := reviewer.ReviewPage(ctx, document{
		Title:         "Ownership",
		NormalizedURL: server.URL + "/docs/ownership",
		H1:            "Ownership",
		Headings:      []string{"Introduction", "Borrowing"},
		Text:          repeatedWords(200),
		WordCount:     200,
	})
	if err != nil {
		t.Fatalf("review page: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected gate and enrichment calls, got %d", calls)
	}
	if review.Decision != "include" || review.Classification != "Concept" {
		t.Fatalf("expected enriched include review, got %+v", review)
	}
	if review.GateScore != 80 || review.GatePageType != "concept" {
		t.Fatalf("expected gate metadata, got %+v", review)
	}
	if review.GateUsage.InputTokens != 620 || review.GateUsage.OutputTokens != 84 || review.GateUsage.ReasoningTokens != 128 || review.GateUsage.TotalTokens != 704 {
		t.Fatalf("expected gate token usage, got %+v", review.GateUsage)
	}
	if review.EnrichmentUsage.InputTokens != 620 || review.EnrichmentUsage.OutputTokens != 84 || review.EnrichmentUsage.ReasoningTokens != 128 || review.EnrichmentUsage.TotalTokens != 704 {
		t.Fatalf("expected enrichment token usage, got %+v", review.EnrichmentUsage)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type usageReviewer struct{}

func (usageReviewer) ReviewPage(_ context.Context, doc document) (Review, error) {
	return Review{
		Decision:         "include",
		Classification:   "Concept",
		Confidence:       0.9,
		EstimatedMinutes: 1,
		Rationale:        "Included by test reviewer.",
		GateScore:        90,
		GatePageType:     "concept",
		Model:            "test-reviewer",
		PromptVersion:    reviewPromptVersion,
		InputHash:        reviewInputHash(doc),
		GateUsage: TokenUsage{
			InputTokens:     620,
			OutputTokens:    84,
			ReasoningTokens: 128,
			TotalTokens:     704,
		},
		EnrichmentUsage: TokenUsage{
			InputTokens:     980,
			OutputTokens:    210,
			ReasoningTokens: 256,
			TotalTokens:     1190,
		},
	}, nil
}

func writeOpenAIResponse(t *testing.T, w http.ResponseWriter, value map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal response value: %v", err)
	}
	response := map[string]any{
		"output": []map[string]any{
			{
				"content": []map[string]any{
					{
						"type": "output_text",
						"text": string(encoded),
					},
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":  620,
			"output_tokens": 84,
			"total_tokens":  704,
			"output_tokens_details": map[string]any{
				"reasoning_tokens": 128,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Fatalf("encode response: %v", err)
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

func serverHost(r *http.Request) string {
	return "http://" + r.Host
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
