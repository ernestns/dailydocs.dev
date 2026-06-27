package inspect

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
)

func TestWriteSubmissions(t *testing.T) {
	ctx := context.Background()
	conn := openInspectTestDB(t, ctx)
	defer conn.Close()
	submissionID, _ := insertInspectData(t, ctx, conn)

	var out bytes.Buffer
	if err := WriteSubmissions(ctx, conn, &out); err != nil {
		t.Fatalf("write submissions: %v", err)
	}

	body := out.String()
	for _, expected := range []string{
		"ID",
		"TOPIC",
		"Rust",
		"rust-lang.org",
		"candidates_ready",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in output for submission %d:\n%s", expected, submissionID, body)
		}
	}
}

func TestWriteSubmission(t *testing.T) {
	ctx := context.Background()
	conn := openInspectTestDB(t, ctx)
	defer conn.Close()
	submissionID, runID := insertInspectData(t, ctx, conn)
	if _, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET latest_pipeline_run_id = ? WHERE id = ?", runID, submissionID); err != nil {
		t.Fatalf("update latest run: %v", err)
	}

	var out bytes.Buffer
	if err := WriteSubmission(ctx, conn, &out, submissionID); err != nil {
		t.Fatalf("write submission: %v", err)
	}

	body := out.String()
	for _, expected := range []string{
		"id",
		"topic",
		"Rust",
		"submitted_url",
		"https://www.rust-lang.org/learn",
		"latest_run_id",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in output:\n%s", expected, body)
		}
	}
}

func TestWriteRuns(t *testing.T) {
	ctx := context.Background()
	conn := openInspectTestDB(t, ctx)
	defer conn.Close()
	submissionID, _ := insertInspectData(t, ctx, conn)

	var out bytes.Buffer
	if err := WriteRuns(ctx, conn, &out, submissionID); err != nil {
		t.Fatalf("write runs: %v", err)
	}

	body := out.String()
	for _, expected := range []string{
		"DISCOVERED",
		"CRAWLED",
		"ELIGIBLE",
		"completed",
		"12",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in output:\n%s", expected, body)
		}
	}
}

func TestWriteCandidates(t *testing.T) {
	ctx := context.Background()
	conn := openInspectTestDB(t, ctx)
	defer conn.Close()
	submissionID, _ := insertInspectData(t, ctx, conn)

	var out bytes.Buffer
	if err := WriteCandidates(ctx, conn, &out, submissionID); err != nil {
		t.Fatalf("write candidates: %v", err)
	}

	body := out.String()
	for _, expected := range []string{
		"SCORE",
		"Rust Book",
		"https://doc.rust-lang.org/book/",
		"Guide",
		"+50 official",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in output:\n%s", expected, body)
		}
	}
}

func openInspectTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}

func insertInspectData(t *testing.T, ctx context.Context, conn *sql.DB) (int64, int64) {
	t.Helper()

	result, err := conn.ExecContext(ctx, `
		INSERT INTO documentation_submissions (
			submitted_url,
			normalized_url,
			source_host,
			suggested_topic,
			status,
			request_count
		)
		VALUES ('https://www.rust-lang.org/learn', 'https://www.rust-lang.org/learn', 'rust-lang.org', 'Rust', 'candidates_ready', 2)
	`)
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	submissionID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read submission id: %v", err)
	}

	result, err = conn.ExecContext(ctx, `
		INSERT INTO pipeline_runs (
			documentation_submission_id,
			status,
			completed_at,
			discovered_count,
			crawled_count,
			eligible_count,
			rejected_count,
			failure_count
		)
		VALUES (?, 'completed', datetime('now'), 12, 8, 1, 6, 1)
	`, submissionID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read run id: %v", err)
	}

	_, err = conn.ExecContext(ctx, `
		INSERT INTO page_candidates (
			documentation_submission_id,
			pipeline_run_id,
			proposed_topic_slug,
			proposed_topic_name,
			title,
			url,
			normalized_url,
			source,
			word_count,
			primary_classification,
			score,
			score_components,
			official,
			estimated_minutes,
			reason,
			status
		)
		VALUES (?, ?, 'rust', 'Rust', 'Rust Book', 'https://doc.rust-lang.org/book/', 'https://doc.rust-lang.org/book', 'rust-lang.org', 1200, 'Guide', 95, '["+50 official"]', 1, 6, '+50 official; +20 guide', 'eligible')
	`, submissionID, runID)
	if err != nil {
		t.Fatalf("insert candidate: %v", err)
	}

	return submissionID, runID
}
