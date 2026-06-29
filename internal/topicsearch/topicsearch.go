package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultMaxResults  = 10
	DefaultMinInterval = 5 * time.Minute
)

var (
	ErrRateLimited = errors.New("topic search rate limited")
	ErrNoResults   = errors.New("topic search returned no usable results")
)

type Provider interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
}

type SearchResult struct {
	Title   string
	URL     string
	Content string
	Score   float64
}

type Options struct {
	Provider    Provider
	Now         func() time.Time
	MaxResults  int
	MinInterval time.Duration
}

type Result struct {
	TopicID     int64
	TopicSlug   string
	TopicName   string
	RunID       int64
	Status      string
	ResultCount int
	StoredCount int
	RateLimited bool
}

type storedResult struct {
	Title        string
	URL          string
	Source       string
	Snippet      string
	Rank         int
	ReadingOrder int
}

func SearchTopic(ctx context.Context, conn *sql.DB, topic string, opts Options) (Result, error) {
	if opts.Provider == nil {
		return Result{}, errors.New("topic search provider is required")
	}

	slug := slugFromTopicName(topic)
	if slug == "" {
		return Result{}, fmt.Errorf("invalid topic %q", topic)
	}
	name := displayTopicName(topic, slug)
	now := currentTime(opts.Now)
	maxResults := opts.MaxResults
	if maxResults < 1 {
		maxResults = DefaultMaxResults
	}
	minInterval := opts.MinInterval
	if minInterval == 0 {
		minInterval = DefaultMinInterval
	}

	topicID, err := upsertTopicStatus(ctx, conn, slug, name, "queued")
	if err != nil {
		return Result{}, err
	}

	if limited, err := searchRateLimited(ctx, conn, now, minInterval); err != nil {
		return Result{}, err
	} else if limited {
		runID, runErr := createSearchRun(ctx, conn, topicID, buildQuery(name), "rate_limited", now)
		if runErr != nil {
			return Result{}, runErr
		}
		return Result{
			TopicID:     topicID,
			TopicSlug:   slug,
			TopicName:   name,
			RunID:       runID,
			Status:      "rate_limited",
			RateLimited: true,
		}, ErrRateLimited
	}

	if _, err := upsertTopicStatus(ctx, conn, slug, name, "searching"); err != nil {
		return Result{}, err
	}

	query := buildQuery(name)
	runID, err := createSearchRun(ctx, conn, topicID, query, "running", now)
	if err != nil {
		return Result{}, err
	}

	providerResults, searchErr := opts.Provider.Search(ctx, query, maxResults)
	if searchErr != nil {
		if err := failRunAndTopic(ctx, conn, topicID, runID, searchErr); err != nil {
			return Result{}, err
		}
		return Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: "failed"}, searchErr
	}

	normalized := normalizeResults(providerResults, maxResults)
	if len(normalized) == 0 {
		if err := failRunAndTopic(ctx, conn, topicID, runID, ErrNoResults); err != nil {
			return Result{}, err
		}
		return Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: "failed"}, ErrNoResults
	}

	storedCount, err := storeResults(ctx, conn, topicID, runID, normalized)
	if err != nil {
		if err := failRunAndTopic(ctx, conn, topicID, runID, err); err != nil {
			return Result{}, err
		}
		return Result{}, err
	}

	if err := completeRunAndTopic(ctx, conn, topicID, runID, len(providerResults), storedCount); err != nil {
		return Result{}, err
	}

	return Result{
		TopicID:     topicID,
		TopicSlug:   slug,
		TopicName:   name,
		RunID:       runID,
		Status:      "completed",
		ResultCount: len(providerResults),
		StoredCount: storedCount,
	}, nil
}

func buildQuery(topic string) string {
	return fmt.Sprintf("%s official documentation guides concepts tutorials reference", topic)
}

func currentTime(now func() time.Time) time.Time {
	if now == nil {
		return time.Now().UTC()
	}
	return now().UTC()
}

func searchRateLimited(ctx context.Context, conn *sql.DB, now time.Time, minInterval time.Duration) (bool, error) {
	var running int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM topic_search_runs
		WHERE status = 'running'
	`).Scan(&running); err != nil {
		return false, fmt.Errorf("count running searches: %w", err)
	}
	if running > 0 {
		return true, nil
	}

	var latest sql.NullString
	if err := conn.QueryRowContext(ctx, `
		SELECT MAX(started_at)
		FROM topic_search_runs
		WHERE status IN ('running', 'completed', 'failed')
	`).Scan(&latest); err != nil {
		return false, fmt.Errorf("read latest search: %w", err)
	}
	if !latest.Valid || strings.TrimSpace(latest.String) == "" {
		return false, nil
	}
	started, err := time.Parse("2006-01-02 15:04:05", latest.String)
	if err != nil {
		return false, fmt.Errorf("parse latest search time: %w", err)
	}
	return now.Sub(started) < minInterval, nil
}

func upsertTopicStatus(ctx context.Context, conn *sql.DB, slug string, name string, status string) (int64, error) {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO topics (slug, name, status, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(slug) DO UPDATE SET
			name = CASE
				WHEN topics.name = topics.slug THEN excluded.name
				ELSE topics.name
			END,
			status = excluded.status,
			updated_at = datetime('now')
	`, slug, name, status)
	if err != nil {
		return 0, fmt.Errorf("upsert search topic: %w", err)
	}

	var topicID int64
	if err := conn.QueryRowContext(ctx, "SELECT id FROM topics WHERE slug = ?", slug).Scan(&topicID); err != nil {
		return 0, fmt.Errorf("read search topic id: %w", err)
	}
	return topicID, nil
}

func createSearchRun(ctx context.Context, conn *sql.DB, topicID int64, query string, status string, now time.Time) (int64, error) {
	result, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, started_at, completed_at)
		VALUES (?, 'tavily', ?, ?, ?, CASE WHEN ? != 'running' THEN ? ELSE NULL END)
	`, topicID, query, status, formatTime(now), status, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("create topic search run: %w", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read topic search run id: %w", err)
	}
	return runID, nil
}

func failRunAndTopic(ctx context.Context, conn *sql.DB, topicID int64, runID int64, runErr error) error {
	if _, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET status = 'failed',
			completed_at = datetime('now'),
			error = ?
		WHERE id = ?
	`, runErr.Error(), runID); err != nil {
		return fmt.Errorf("record topic search failure: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE topics
		SET status = 'failed',
			updated_at = datetime('now')
		WHERE id = ?
	`, topicID); err != nil {
		return fmt.Errorf("record failed topic status: %w", err)
	}
	return nil
}

func completeRunAndTopic(ctx context.Context, conn *sql.DB, topicID int64, runID int64, resultCount int, storedCount int) error {
	if _, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET status = 'completed',
			completed_at = datetime('now'),
			result_count = ?,
			stored_count = ?
		WHERE id = ?
	`, resultCount, storedCount, runID); err != nil {
		return fmt.Errorf("record topic search completion: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE topics
		SET status = 'active',
			updated_at = datetime('now')
		WHERE id = ?
	`, topicID); err != nil {
		return fmt.Errorf("record active topic status: %w", err)
	}
	return nil
}

func storeResults(ctx context.Context, conn *sql.DB, topicID int64, runID int64, results []storedResult) (int, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin store search results: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stored := 0
	nextOrder, err := nextReadingOrder(ctx, tx, topicID)
	if err != nil {
		return 0, err
	}
	for i, result := range results {
		result.ReadingOrder = nextOrder + i
		pageID, err := upsertPage(ctx, tx, topicID, runID, result)
		if err != nil {
			return 0, err
		}
		if err := upsertSearchResult(ctx, tx, topicID, runID, pageID, result); err != nil {
			return 0, err
		}
		stored++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit search results: %w", err)
	}
	return stored, nil
}

func nextReadingOrder(ctx context.Context, tx *sql.Tx, topicID int64) (int, error) {
	var maxOrder sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(reading_order)
		FROM pages
		WHERE topic_id = ?
	`, topicID).Scan(&maxOrder); err != nil {
		return 0, fmt.Errorf("read max reading order: %w", err)
	}
	if !maxOrder.Valid {
		return 1, nil
	}
	return int(maxOrder.Int64) + 1, nil
}

func upsertPage(ctx context.Context, tx *sql.Tx, topicID int64, runID int64, result storedResult) (int64, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO pages (
			topic_id,
			title,
			url,
			source,
			official,
			reading_order,
			active,
			discovered_at,
			search_run_id,
			updated_at
		)
		VALUES (?, ?, ?, ?, 0, ?, 1, datetime('now'), ?, datetime('now'))
		ON CONFLICT(topic_id, url) DO UPDATE SET
			title = excluded.title,
			source = excluded.source,
			active = 1,
			search_run_id = excluded.search_run_id,
			updated_at = datetime('now')
	`, topicID, result.Title, result.URL, result.Source, result.ReadingOrder, runID)
	if err != nil {
		return 0, fmt.Errorf("upsert search page %q: %w", result.URL, err)
	}

	var pageID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM pages WHERE topic_id = ? AND url = ?", topicID, result.URL).Scan(&pageID); err != nil {
		return 0, fmt.Errorf("read search page id: %w", err)
	}
	return pageID, nil
}

func upsertSearchResult(ctx context.Context, tx *sql.Tx, topicID int64, runID int64, pageID int64, result storedResult) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO topic_search_results (
			topic_id,
			search_run_id,
			title,
			url,
			source,
			snippet,
			rank,
			stored_as_page_id
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(topic_id, url) DO UPDATE SET
			search_run_id = excluded.search_run_id,
			title = excluded.title,
			source = excluded.source,
			snippet = excluded.snippet,
			rank = excluded.rank,
			stored_as_page_id = excluded.stored_as_page_id
	`, topicID, runID, result.Title, result.URL, result.Source, result.Snippet, result.Rank, pageID)
	if err != nil {
		return fmt.Errorf("upsert search result %q: %w", result.URL, err)
	}
	return nil
}

func normalizeResults(results []SearchResult, maxResults int) []storedResult {
	seen := map[string]struct{}{}
	normalized := make([]storedResult, 0, len(results))
	for _, result := range results {
		title := strings.TrimSpace(result.Title)
		rawURL := strings.TrimSpace(result.URL)
		if title == "" || rawURL == "" {
			continue
		}
		normalizedURL, source, err := normalizeURL(rawURL)
		if err != nil {
			continue
		}
		if _, exists := seen[normalizedURL]; exists {
			continue
		}
		seen[normalizedURL] = struct{}{}
		normalized = append(normalized, storedResult{
			Title:   title,
			URL:     normalizedURL,
			Source:  source,
			Snippet: strings.TrimSpace(result.Content),
			Rank:    len(normalized) + 1,
		})
		if len(normalized) == maxResults {
			break
		}
	}
	return normalized
}

func normalizeURL(raw string) (string, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", fmt.Errorf("invalid url %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported url scheme %q", parsed.Scheme)
	}
	parsed.Fragment = ""
	parsed.RawQuery = ""
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Path != "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	return parsed.String(), parsed.Host, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func slugFromTopicName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	previousDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			previousDash = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			previousDash = false
		default:
			if builder.Len() > 0 && !previousDash {
				builder.WriteByte('-')
				previousDash = true
			}
		}
	}
	return strings.Trim(builder.String(), "-")
}

func displayTopicName(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_'
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}
