package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	if _, err := conn.ExecContext(ctx, "UPDATE topic_sources SET status = 'needs_scope', last_error = 'too broad' WHERE id = ?", source.ID); err != nil {
		t.Fatalf("mark source needs scope: %v", err)
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

	var status string
	var processed sql.NullString
	var lastError string
	if err := conn.QueryRowContext(ctx, "SELECT status, last_processed_at, last_error FROM topic_sources WHERE id = ?", source.ID).Scan(&status, &processed, &lastError); err != nil {
		t.Fatalf("read source processed timestamp: %v", err)
	}
	if status != "candidates_ready" {
		t.Fatalf("expected source status candidates_ready after successful processing, got %q", status)
	}
	if !processed.Valid || processed.String == "" {
		t.Fatalf("expected source last_processed_at to be set")
	}
	if lastError != "" {
		t.Fatalf("expected source last error cleared, got %q", lastError)
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
