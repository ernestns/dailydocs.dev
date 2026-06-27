package inspect

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

type SubmissionSummary struct {
	ID             int64
	SuggestedTopic string
	SourceHost     string
	Status         string
	RequestCount   int
	LastSubmitted  string
	LastError      string
}

type SubmissionDetail struct {
	SubmissionSummary
	SubmittedURL   string
	NormalizedURL  string
	Visibility     string
	LatestRunID    sql.NullInt64
	AttemptCount   int
	LastAttemptAt  sql.NullString
	LockedUntil    sql.NullString
	FirstSubmitted string
}

type RunSummary struct {
	ID              int64
	Status          string
	StartedAt       string
	CompletedAt     sql.NullString
	DiscoveredCount int
	CrawledCount    int
	EligibleCount   int
	RejectedCount   int
	FailureCount    int
	Error           string
}

type CandidateSummary struct {
	ID               int64
	Title            string
	URL              string
	Classification   string
	Score            int
	Status           string
	EstimatedMinutes sql.NullInt64
	Reason           string
}

func WriteSubmissions(ctx context.Context, conn *sql.DB, out io.Writer) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, suggested_topic, source_host, status, request_count, last_submitted_at, last_error
		FROM documentation_submissions
		ORDER BY id ASC
	`)
	if err != nil {
		return fmt.Errorf("query submissions: %w", err)
	}
	defer rows.Close()

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tTOPIC\tSOURCE\tSTATUS\tREQUESTS\tLAST SUBMITTED\tERROR")
	for rows.Next() {
		var sub SubmissionSummary
		if err := rows.Scan(&sub.ID, &sub.SuggestedTopic, &sub.SourceHost, &sub.Status, &sub.RequestCount, &sub.LastSubmitted, &sub.LastError); err != nil {
			return fmt.Errorf("scan submission: %w", err)
		}
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%d\t%s\t%s\n", sub.ID, dash(sub.SuggestedTopic), sub.SourceHost, sub.Status, sub.RequestCount, sub.LastSubmitted, dash(shorten(sub.LastError, 80)))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate submissions: %w", err)
	}
	return writer.Flush()
}

func WriteSubmission(ctx context.Context, conn *sql.DB, out io.Writer, submissionID int64) error {
	var sub SubmissionDetail
	err := conn.QueryRowContext(ctx, `
		SELECT
			id,
			suggested_topic,
			source_host,
			status,
			request_count,
			last_submitted_at,
			last_error,
			submitted_url,
			normalized_url,
			visibility,
			latest_pipeline_run_id,
			attempt_count,
			last_attempt_at,
			locked_until,
			first_submitted_at
		FROM documentation_submissions
		WHERE id = ?
	`, submissionID).Scan(&sub.ID, &sub.SuggestedTopic, &sub.SourceHost, &sub.Status, &sub.RequestCount, &sub.LastSubmitted, &sub.LastError, &sub.SubmittedURL, &sub.NormalizedURL, &sub.Visibility, &sub.LatestRunID, &sub.AttemptCount, &sub.LastAttemptAt, &sub.LockedUntil, &sub.FirstSubmitted)
	if err != nil {
		return fmt.Errorf("query submission: %w", err)
	}

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(writer, "id\t%d\n", sub.ID)
	_, _ = fmt.Fprintf(writer, "topic\t%s\n", dash(sub.SuggestedTopic))
	_, _ = fmt.Fprintf(writer, "source_host\t%s\n", sub.SourceHost)
	_, _ = fmt.Fprintf(writer, "status\t%s\n", sub.Status)
	_, _ = fmt.Fprintf(writer, "visibility\t%s\n", sub.Visibility)
	_, _ = fmt.Fprintf(writer, "requests\t%d\n", sub.RequestCount)
	_, _ = fmt.Fprintf(writer, "attempts\t%d\n", sub.AttemptCount)
	_, _ = fmt.Fprintf(writer, "latest_run_id\t%s\n", nullInt(sub.LatestRunID))
	_, _ = fmt.Fprintf(writer, "submitted_url\t%s\n", sub.SubmittedURL)
	_, _ = fmt.Fprintf(writer, "normalized_url\t%s\n", sub.NormalizedURL)
	_, _ = fmt.Fprintf(writer, "first_submitted\t%s\n", sub.FirstSubmitted)
	_, _ = fmt.Fprintf(writer, "last_submitted\t%s\n", sub.LastSubmitted)
	_, _ = fmt.Fprintf(writer, "last_attempt\t%s\n", nullString(sub.LastAttemptAt))
	_, _ = fmt.Fprintf(writer, "locked_until\t%s\n", nullString(sub.LockedUntil))
	_, _ = fmt.Fprintf(writer, "last_error\t%s\n", dash(sub.LastError))
	return writer.Flush()
}

func WriteRuns(ctx context.Context, conn *sql.DB, out io.Writer, submissionID int64) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, status, started_at, completed_at, discovered_count, crawled_count, eligible_count, rejected_count, failure_count, error
		FROM pipeline_runs
		WHERE documentation_submission_id = ?
		ORDER BY id ASC
	`, submissionID)
	if err != nil {
		return fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tSTATUS\tSTARTED\tCOMPLETED\tDISCOVERED\tCRAWLED\tELIGIBLE\tREJECTED\tFAILED\tERROR")
	for rows.Next() {
		var run RunSummary
		if err := rows.Scan(&run.ID, &run.Status, &run.StartedAt, &run.CompletedAt, &run.DiscoveredCount, &run.CrawledCount, &run.EligibleCount, &run.RejectedCount, &run.FailureCount, &run.Error); err != nil {
			return fmt.Errorf("scan run: %w", err)
		}
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", run.ID, run.Status, run.StartedAt, nullString(run.CompletedAt), run.DiscoveredCount, run.CrawledCount, run.EligibleCount, run.RejectedCount, run.FailureCount, dash(shorten(run.Error, 80)))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate runs: %w", err)
	}
	return writer.Flush()
}

func WriteCandidates(ctx context.Context, conn *sql.DB, out io.Writer, submissionID int64) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, title, url, primary_classification, score, status, estimated_minutes, reason
		FROM page_candidates
		WHERE documentation_submission_id = ?
		ORDER BY score DESC, title ASC
	`, submissionID)
	if err != nil {
		return fmt.Errorf("query candidates: %w", err)
	}
	defer rows.Close()

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tSCORE\tSTATUS\tMIN\tCLASS\tTITLE\tURL\tREASON")
	for rows.Next() {
		var cand CandidateSummary
		if err := rows.Scan(&cand.ID, &cand.Title, &cand.URL, &cand.Classification, &cand.Score, &cand.Status, &cand.EstimatedMinutes, &cand.Reason); err != nil {
			return fmt.Errorf("scan candidate: %w", err)
		}
		_, _ = fmt.Fprintf(writer, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", cand.ID, cand.Score, cand.Status, nullInt(cand.EstimatedMinutes), cand.Classification, cand.Title, cand.URL, shorten(cand.Reason, 120))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate candidates: %w", err)
	}
	return writer.Flush()
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func nullString(value sql.NullString) string {
	if !value.Valid || value.String == "" {
		return "-"
	}
	return value.String
}

func nullInt(value sql.NullInt64) string {
	if !value.Valid {
		return "-"
	}
	return fmt.Sprintf("%d", value.Int64)
}

func shorten(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}
