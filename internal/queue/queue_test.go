package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
)

func TestProcessPendingClaimsAndProcessesSubmissions(t *testing.T) {
	ctx := context.Background()
	conn := openQueueTestDB(t, ctx)
	defer conn.Close()

	first := insertQueueSubmission(t, ctx, conn, "https://example.com/one", "pending")
	second := insertQueueSubmission(t, ctx, conn, "https://example.com/two", "pending")
	insertQueueSubmission(t, ctx, conn, "https://example.com/three", "pending")

	var processed []int64
	result, err := ProcessPending(ctx, conn, Options{
		Limit:    2,
		WorkerID: "test-worker",
		Processor: func(ctx context.Context, conn *sql.DB, submissionID int64) error {
			processed = append(processed, submissionID)
			_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'candidates_ready' WHERE id = ?", submissionID)
			return err
		},
	})
	if err != nil {
		t.Fatalf("process pending: %v", err)
	}
	if result.Claimed != 2 || result.Processed != 2 || result.Failed != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !reflect.DeepEqual(processed, []int64{first, second}) {
		t.Fatalf("expected processed ids [%d %d], got %v", first, second, processed)
	}

	var locked int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM documentation_submissions WHERE locked_by != ''").Scan(&locked); err != nil {
		t.Fatalf("count locks: %v", err)
	}
	if locked != 0 {
		t.Fatalf("expected locks released, got %d", locked)
	}
}

func TestProcessPendingRecordsFailuresAndContinues(t *testing.T) {
	ctx := context.Background()
	conn := openQueueTestDB(t, ctx)
	defer conn.Close()

	first := insertQueueSubmission(t, ctx, conn, "https://example.com/one", "pending")
	second := insertQueueSubmission(t, ctx, conn, "https://example.com/two", "pending")

	result, err := ProcessPending(ctx, conn, Options{
		Limit:    2,
		WorkerID: "test-worker",
		Processor: func(ctx context.Context, conn *sql.DB, submissionID int64) error {
			if submissionID == first {
				return errors.New("boom")
			}
			_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'candidates_ready' WHERE id = ?", submissionID)
			return err
		},
	})
	if err != nil {
		t.Fatalf("process pending: %v", err)
	}
	if result.Claimed != 2 || result.Processed != 1 || result.Failed != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}

	var status, lastError string
	if err := conn.QueryRowContext(ctx, "SELECT status, last_error FROM documentation_submissions WHERE id = ?", first).Scan(&status, &lastError); err != nil {
		t.Fatalf("read failed submission: %v", err)
	}
	if status != "failed" || lastError != "boom" {
		t.Fatalf("expected failed boom, got status=%q error=%q", status, lastError)
	}

	var secondStatus string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM documentation_submissions WHERE id = ?", second).Scan(&secondStatus); err != nil {
		t.Fatalf("read second submission: %v", err)
	}
	if secondStatus != "candidates_ready" {
		t.Fatalf("expected second candidates_ready, got %q", secondStatus)
	}
}

func TestProcessPendingSkipsLockedSubmissions(t *testing.T) {
	ctx := context.Background()
	conn := openQueueTestDB(t, ctx)
	defer conn.Close()

	locked := insertQueueSubmission(t, ctx, conn, "https://example.com/locked", "pending")
	available := insertQueueSubmission(t, ctx, conn, "https://example.com/available", "pending")
	if _, err := conn.ExecContext(ctx, `
		UPDATE documentation_submissions
		SET locked_at = datetime('now'),
			locked_until = datetime('now', '+1 hour'),
			locked_by = 'other-worker'
		WHERE id = ?
	`, locked); err != nil {
		t.Fatalf("lock submission: %v", err)
	}

	var processed []int64
	result, err := ProcessPending(ctx, conn, Options{
		Limit:    5,
		WorkerID: "test-worker",
		Processor: func(ctx context.Context, conn *sql.DB, submissionID int64) error {
			processed = append(processed, submissionID)
			_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'candidates_ready' WHERE id = ?", submissionID)
			return err
		},
	})
	if err != nil {
		t.Fatalf("process pending: %v", err)
	}
	if result.Claimed != 1 || len(processed) != 1 || processed[0] != available {
		t.Fatalf("expected only available submission %d, got result=%+v processed=%v", available, result, processed)
	}
}

func openQueueTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}

func insertQueueSubmission(t *testing.T, ctx context.Context, conn *sql.DB, rawURL string, status string) int64 {
	t.Helper()

	result, err := conn.ExecContext(ctx, `
		INSERT INTO documentation_submissions (
			submitted_url,
			normalized_url,
			source_host,
			status,
			last_submitted_at
		)
		VALUES (?, ?, 'example.com', ?, ?)
	`, rawURL, rawURL, status, time.Now().UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read submission id: %v", err)
	}
	return id
}
