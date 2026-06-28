package submission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

var ErrInvalidURL = errors.New("invalid documentation URL")

type CreateInput struct {
	URL            string
	SuggestedTopic string
	SubmitterIP    string
	IPHashSalt     string
}

type Submission struct {
	ID              int64
	SubmittedURL    string
	NormalizedURL   string
	SourceHost      string
	SuggestedTopic  string
	Status          string
	SourceStatus    string
	DiscoveryCount  int
	Visibility      string
	RequestCount    int
	SubmitterIPHash string
	LastError       string
	FirstSubmitted  string
	LastSubmitted   string
}

func Create(ctx context.Context, conn *sql.DB, input CreateInput) (Submission, error) {
	normalized, host, err := NormalizeURL(input.URL)
	if err != nil {
		return Submission{}, err
	}

	suggestedTopic := strings.TrimSpace(input.SuggestedTopic)
	if len(suggestedTopic) > 120 {
		suggestedTopic = suggestedTopic[:120]
	}

	ipHash := HashIP(input.SubmitterIP, input.IPHashSalt)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Submission{}, fmt.Errorf("begin submission insert: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO documentation_submissions (
			submitted_url,
			normalized_url,
			source_host,
			suggested_topic,
			submitter_ip_hash
		)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(normalized_url) DO UPDATE SET
			request_count = request_count + 1,
			suggested_topic = CASE
				WHEN documentation_submissions.suggested_topic = '' THEN excluded.suggested_topic
				ELSE documentation_submissions.suggested_topic
			END,
			last_submitted_at = datetime('now')
	`, strings.TrimSpace(input.URL), normalized, host, suggestedTopic, ipHash)
	if err != nil {
		return Submission{}, fmt.Errorf("insert submission: %w", err)
	}

	sub, err := getByNormalizedURL(ctx, tx, normalized)
	if err != nil {
		return Submission{}, err
	}

	if err := tx.Commit(); err != nil {
		return Submission{}, fmt.Errorf("commit submission insert: %w", err)
	}

	return sub, nil
}

func ListPublic(ctx context.Context, conn *sql.DB, limit int) ([]Submission, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT
			id,
			submitted_url,
			normalized_url,
			source_host,
			suggested_topic,
			status,
			COALESCE((
				SELECT ts.status
				FROM topic_sources ts
				WHERE ts.created_from_submission_id = documentation_submissions.id
				ORDER BY ts.id DESC
				LIMIT 1
			), ''),
			COALESCE((
				SELECT ts.discovery_count
				FROM topic_sources ts
				WHERE ts.created_from_submission_id = documentation_submissions.id
				ORDER BY ts.id DESC
				LIMIT 1
			), 0),
			visibility,
			request_count,
			submitter_ip_hash,
			last_error,
			first_submitted_at,
			last_submitted_at
		FROM documentation_submissions
		WHERE visibility = 'public'
		ORDER BY last_submitted_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query public submissions: %w", err)
	}
	defer rows.Close()

	var submissions []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		submissions = append(submissions, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate public submissions: %w", err)
	}

	return submissions, nil
}

func NormalizeURL(raw string) (normalized string, sourceHost string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("%w: url is required", ErrInvalidURL)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("%w: use http or https", ErrInvalidURL)
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("%w: host is required", ErrInvalidURL)
	}
	if parsed.User != nil {
		return "", "", fmt.Errorf("%w: userinfo is not allowed", ErrInvalidURL)
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", "", fmt.Errorf("%w: host is required", ErrInvalidURL)
	}
	if strings.Contains(host, " ") {
		return "", "", fmt.Errorf("%w: host is invalid", ErrInvalidURL)
	}

	port := parsed.Port()
	if port != "" && !isDefaultPort(parsed.Scheme, port) {
		host = net.JoinHostPort(host, port)
	}

	cleanPath := path.Clean("/" + parsed.Path)
	if cleanPath == "." {
		cleanPath = "/"
	}
	if cleanPath != "/" {
		cleanPath = strings.TrimRight(cleanPath, "/")
	}

	u := url.URL{
		Scheme: parsed.Scheme,
		Host:   host,
		Path:   cleanPath,
	}

	return u.String(), host, nil
}

func HashIP(ip string, salt string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(salt + "|" + ip))
	return hex.EncodeToString(sum[:])
}

func getByNormalizedURL(ctx context.Context, tx *sql.Tx, normalizedURL string) (Submission, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT
			id,
			submitted_url,
			normalized_url,
			source_host,
			suggested_topic,
			status,
			'' AS source_status,
			0 AS discovery_count,
			visibility,
			request_count,
			submitter_ip_hash,
			last_error,
			first_submitted_at,
			last_submitted_at
		FROM documentation_submissions
		WHERE normalized_url = ?
	`, normalizedURL)

	sub, err := scanSubmission(row)
	if err != nil {
		return Submission{}, err
	}
	return sub, nil
}

func (s Submission) PublicStatus() string {
	switch s.SourceStatus {
	case "pending_discovery":
		return "Queued for discovery"
	case "ready_to_process":
		return "Discovered"
	case "processing":
		return "Processing"
	case "candidates_ready":
		return "Ready for review"
	case "needs_scope":
		return "Needs narrower URL"
	case "discovery_failed":
		return "Discovery failed"
	case "disabled":
		return "Disabled"
	}

	switch s.Status {
	case "pending":
		return "Submitted"
	case "processing":
		return "Processing"
	case "candidates_ready":
		return "Ready for review"
	case "active":
		return "Active"
	case "rejected":
		return "Rejected"
	case "failed":
		return "Failed"
	default:
		return s.Status
	}
}

type submissionScanner interface {
	Scan(dest ...any) error
}

func scanSubmission(scanner submissionScanner) (Submission, error) {
	var sub Submission
	if err := scanner.Scan(
		&sub.ID,
		&sub.SubmittedURL,
		&sub.NormalizedURL,
		&sub.SourceHost,
		&sub.SuggestedTopic,
		&sub.Status,
		&sub.SourceStatus,
		&sub.DiscoveryCount,
		&sub.Visibility,
		&sub.RequestCount,
		&sub.SubmitterIPHash,
		&sub.LastError,
		&sub.FirstSubmitted,
		&sub.LastSubmitted,
	); err != nil {
		return Submission{}, fmt.Errorf("scan submission: %w", err)
	}
	return sub, nil
}

func isDefaultPort(scheme string, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}
