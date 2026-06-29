package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	DefaultMaxResults  = 10
	DefaultMinInterval = 5 * time.Minute
	DefaultMinScore    = 65
	DefaultDailyLimit  = 20
	StaleRunTimeout    = 30 * time.Minute

	runStatusRunning     = "running"
	runStatusCompleted   = "completed"
	runStatusFailed      = "failed"
	runStatusRateLimited = "rate_limited"

	runStageSearching = "searching"
	runStageReviewing = "reviewing"
	runStageStoring   = "storing"
)

var (
	ErrRateLimited = errors.New("topic search rate limited")
	ErrNoResults   = errors.New("topic search returned no usable results")
)

type Provider interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
}

type Reviewer interface {
	Review(ctx context.Context, topic string, candidates []ReviewCandidate) (ReviewOutput, error)
}

type SearchResult struct {
	Title   string
	URL     string
	Content string
	Score   float64
}

type ReviewCandidate struct {
	Index        int    `json:"index"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	Source       string `json:"source"`
	Snippet      string `json:"snippet"`
	ProviderRank int    `json:"provider_rank"`
}

type ReviewResult struct {
	Index          int    `json:"index"`
	DailyDocsScore int    `json:"dailydocs_score"`
	PageType       string `json:"page_type"`
	ShouldStore    bool   `json:"should_store"`
	Reason         string `json:"reason"`
}

type ReviewOutput struct {
	Results      []ReviewResult
	Model        string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

type Options struct {
	Provider    Provider
	Reviewer    Reviewer
	Now         func() time.Time
	MaxResults  int
	MinInterval time.Duration
	MinScore    int
	DailyLimit  int
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

type QueueResult struct {
	Processed         bool
	DailyLimitReached bool
	Result            Result
}

type storedResult struct {
	Title        string
	URL          string
	Source       string
	Snippet      string
	Rank         int
	ReadingOrder int
	Score        int
	PageType     string
	Reason       string
	Accepted     bool
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
	minScore := opts.MinScore
	if minScore < 1 {
		minScore = DefaultMinScore
	}
	minInterval := opts.MinInterval
	if minInterval == 0 {
		minInterval = DefaultMinInterval
	}

	topicID, err := ensureTopic(ctx, conn, slug, name)
	if err != nil {
		return Result{}, err
	}

	if err := ExpireStaleRunningSearches(ctx, conn, now); err != nil {
		return Result{}, err
	}
	if limited, err := searchRateLimited(ctx, conn, now, minInterval); err != nil {
		return Result{}, err
	} else if limited {
		runID, runErr := createSearchRun(ctx, conn, topicID, buildQuery(name), runStatusRateLimited, "", now)
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
	runID, err := createSearchRun(ctx, conn, topicID, query, runStatusRunning, runStageSearching, now)
	if err != nil {
		return Result{}, err
	}

	searchLimit := maxResults
	if opts.Reviewer != nil {
		searchLimit = maxResults * 2
	}
	providerResults, searchErr := opts.Provider.Search(ctx, query, searchLimit)
	if searchErr != nil {
		if err := failRunAndTopic(ctx, conn, topicID, runID, searchErr); err != nil {
			return Result{}, err
		}
		return Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: "failed"}, searchErr
	}

	normalized := normalizeResults(providerResults, searchLimit)
	if len(normalized) == 0 {
		if err := failRunAndTopic(ctx, conn, topicID, runID, ErrNoResults); err != nil {
			return Result{}, err
		}
		return Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: "failed"}, ErrNoResults
	}
	if err := storeSearchCandidates(ctx, conn, topicID, runID, normalized); err != nil {
		if err := failRunAndTopic(ctx, conn, topicID, runID, err); err != nil {
			return Result{}, err
		}
		return Result{}, err
	}

	var reviewOutput ReviewOutput
	if opts.Reviewer != nil {
		if err := updateSearchRunStage(ctx, conn, runID, runStageReviewing); err != nil {
			return Result{}, err
		}
		normalized, reviewOutput, err = reviewResults(ctx, name, opts.Reviewer, normalized, minScore)
		if err != nil {
			if err := failRunAndTopic(ctx, conn, topicID, runID, err); err != nil {
				return Result{}, err
			}
			return Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: "failed"}, err
		}
	}
	if len(normalized) > maxResults {
		normalized = capAcceptedResults(normalized, maxResults)
	}
	acceptedCount := countAccepted(normalized)
	for i := range normalized {
		normalized[i].Rank = i + 1
	}

	if err := updateSearchRunStage(ctx, conn, runID, runStageStoring); err != nil {
		return Result{}, err
	}
	storedCount, err := storeReviewedResults(ctx, conn, topicID, runID, normalized)
	if err != nil {
		if err := failRunAndTopic(ctx, conn, topicID, runID, err); err != nil {
			return Result{}, err
		}
		return Result{}, err
	}

	if acceptedCount == 0 {
		if err := failRunAndTopic(ctx, conn, topicID, runID, ErrNoResults); err != nil {
			return Result{}, err
		}
		return Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: "failed", ResultCount: len(providerResults)}, ErrNoResults
	}

	if err := completeRunAndTopic(ctx, conn, topicID, runID, len(providerResults), storedCount, reviewOutput); err != nil {
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

func ProcessNextQueuedTopic(ctx context.Context, conn *sql.DB, opts Options) (QueueResult, error) {
	now := currentTime(opts.Now)
	dailyLimit := opts.DailyLimit
	if dailyLimit < 1 {
		dailyLimit = DefaultDailyLimit
	}
	limited, err := dailyLimitReached(ctx, conn, now, dailyLimit)
	if err != nil {
		return QueueResult{}, err
	}
	if limited {
		return QueueResult{DailyLimitReached: true}, nil
	}

	var topic string
	err = conn.QueryRowContext(ctx, `
		SELECT name
		FROM topics
		WHERE status = 'queued'
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`).Scan(&topic)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueResult{}, nil
	}
	if err != nil {
		return QueueResult{}, fmt.Errorf("read next queued topic: %w", err)
	}

	result, err := SearchTopic(ctx, conn, topic, opts)
	return QueueResult{Processed: true, Result: result}, err
}

func ProcessQueuedTopic(ctx context.Context, conn *sql.DB, slug string, opts Options) (QueueResult, error) {
	now := currentTime(opts.Now)
	dailyLimit := opts.DailyLimit
	if dailyLimit < 1 {
		dailyLimit = DefaultDailyLimit
	}
	limited, err := dailyLimitReached(ctx, conn, now, dailyLimit)
	if err != nil {
		return QueueResult{}, err
	}
	if limited {
		return QueueResult{DailyLimitReached: true}, nil
	}

	var topic string
	err = conn.QueryRowContext(ctx, `
		SELECT name
		FROM topics
		WHERE slug = ?
			AND status IN ('queued', 'failed')
	`, slug).Scan(&topic)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueResult{}, nil
	}
	if err != nil {
		return QueueResult{}, fmt.Errorf("read queued topic %q: %w", slug, err)
	}

	result, err := SearchTopic(ctx, conn, topic, opts)
	return QueueResult{Processed: true, Result: result}, err
}

func dailyLimitReached(ctx context.Context, conn *sql.DB, now time.Time, limit int) (bool, error) {
	if limit < 1 {
		return false, nil
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var count int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM topic_search_runs
		WHERE status IN ('running', 'completed', 'failed')
			AND started_at >= ?
	`, formatTime(dayStart)).Scan(&count); err != nil {
		return false, fmt.Errorf("count daily topic searches: %w", err)
	}
	return count >= limit, nil
}

func buildQuery(topic string) string {
	return fmt.Sprintf("%s specific concept tutorial guide deep dive documentation", topic)
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
		WHERE status = ?
	`, runStatusRunning).Scan(&running); err != nil {
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

func ExpireStaleRunningSearches(ctx context.Context, conn *sql.DB, now time.Time) error {
	cutoff := now.Add(-StaleRunTimeout)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin expire stale running searches: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `
		UPDATE topics
		SET status = 'failed',
			updated_at = ?
		WHERE status = 'searching'
			AND id IN (
				SELECT topic_id
				FROM topic_search_runs
				WHERE status = ?
					AND started_at < ?
			)
			AND id NOT IN (
				SELECT topic_id
				FROM topic_search_runs
				WHERE status = ?
					AND started_at >= ?
			)
	`, formatTime(now), runStatusRunning, formatTime(cutoff), runStatusRunning, formatTime(cutoff)); err != nil {
		return fmt.Errorf("expire stale search topics: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET status = 'failed',
			stage = '',
			completed_at = ?,
			error = 'stale running search timed out'
		WHERE status = ?
			AND started_at < ?
	`, formatTime(now), runStatusRunning, formatTime(cutoff)); err != nil {
		return fmt.Errorf("expire stale running searches: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit expire stale running searches: %w", err)
	}
	return nil
}

func ensureTopic(ctx context.Context, conn *sql.DB, slug string, name string) (int64, error) {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO topics (slug, name, status, updated_at)
		VALUES (?, ?, 'queued', datetime('now'))
		ON CONFLICT(slug) DO UPDATE SET
			name = CASE
				WHEN topics.name = topics.slug THEN excluded.name
				ELSE topics.name
			END,
			updated_at = datetime('now')
	`, slug, name)
	if err != nil {
		return 0, fmt.Errorf("ensure search topic: %w", err)
	}

	var topicID int64
	if err := conn.QueryRowContext(ctx, "SELECT id FROM topics WHERE slug = ?", slug).Scan(&topicID); err != nil {
		return 0, fmt.Errorf("read search topic id: %w", err)
	}
	return topicID, nil
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

func createSearchRun(ctx context.Context, conn *sql.DB, topicID int64, query string, status string, stage string, now time.Time) (int64, error) {
	result, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs (topic_id, provider, query, status, stage, started_at, completed_at)
		VALUES (?, 'tavily', ?, ?, ?, ?, CASE WHEN ? != 'running' THEN ? ELSE NULL END)
	`, topicID, query, status, stage, formatTime(now), status, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("create topic search run: %w", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read topic search run id: %w", err)
	}
	return runID, nil
}

func updateSearchRunStage(ctx context.Context, conn *sql.DB, runID int64, stage string) error {
	if _, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET stage = ?
		WHERE id = ?
	`, stage, runID); err != nil {
		return fmt.Errorf("update topic search stage: %w", err)
	}
	return nil
}

func failRunAndTopic(ctx context.Context, conn *sql.DB, topicID int64, runID int64, runErr error) error {
	if _, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET status = 'failed',
			stage = '',
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

func completeRunAndTopic(ctx context.Context, conn *sql.DB, topicID int64, runID int64, resultCount int, storedCount int, review ReviewOutput) error {
	if _, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET status = 'completed',
			stage = '',
			completed_at = datetime('now'),
			result_count = ?,
			stored_count = ?,
			reviewer_model = ?,
			reviewer_input_tokens = ?,
			reviewer_output_tokens = ?,
			reviewer_total_tokens = ?
		WHERE id = ?
	`, resultCount, storedCount, review.Model, review.InputTokens, review.OutputTokens, review.TotalTokens, runID); err != nil {
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

func storeSearchCandidates(ctx context.Context, conn *sql.DB, topicID int64, runID int64, results []storedResult) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store search candidates: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for _, result := range results {
		if err := upsertSearchCandidate(ctx, tx, topicID, runID, result); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search candidates: %w", err)
	}
	return nil
}

func storeReviewedResults(ctx context.Context, conn *sql.DB, topicID int64, runID int64, results []storedResult) (int, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin store reviewed results: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stored := 0
	nextOrder, err := nextReadingOrder(ctx, tx, topicID)
	if err != nil {
		return 0, err
	}
	for _, result := range results {
		var pageID sql.NullInt64
		if result.Accepted {
			result.ReadingOrder = nextOrder + stored
			id, err := upsertPage(ctx, tx, topicID, runID, result)
			if err != nil {
				return 0, err
			}
			pageID = sql.NullInt64{Int64: id, Valid: true}
			stored++
		}
		if err := upsertSearchResult(ctx, tx, topicID, runID, pageID, result); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit reviewed results: %w", err)
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

func upsertSearchCandidate(ctx context.Context, tx *sql.Tx, topicID int64, runID int64, result storedResult) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO topic_search_results (
			topic_id,
			search_run_id,
			title,
			url,
			source,
			snippet,
			rank,
			reviewer_score,
			page_type,
			reviewer_reason,
			accepted,
			stored_as_page_id
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, '', '', 0, NULL)
		ON CONFLICT(topic_id, url) DO UPDATE SET
			search_run_id = excluded.search_run_id,
			title = excluded.title,
			source = excluded.source,
			snippet = excluded.snippet,
			rank = excluded.rank,
			reviewer_score = NULL,
			page_type = '',
			reviewer_reason = '',
			accepted = 0,
			stored_as_page_id = NULL
	`, topicID, runID, result.Title, result.URL, result.Source, result.Snippet, result.Rank)
	if err != nil {
		return fmt.Errorf("upsert search candidate %q: %w", result.URL, err)
	}
	return nil
}

func upsertSearchResult(ctx context.Context, tx *sql.Tx, topicID int64, runID int64, pageID sql.NullInt64, result storedResult) error {
	accepted := 0
	if result.Accepted {
		accepted = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO topic_search_results (
			topic_id,
			search_run_id,
			title,
			url,
			source,
			snippet,
			rank,
			reviewer_score,
			page_type,
			reviewer_reason,
			accepted,
			stored_as_page_id
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(topic_id, url) DO UPDATE SET
			search_run_id = excluded.search_run_id,
			title = excluded.title,
			source = excluded.source,
			snippet = excluded.snippet,
			rank = excluded.rank,
			reviewer_score = excluded.reviewer_score,
			page_type = excluded.page_type,
			reviewer_reason = excluded.reviewer_reason,
			accepted = excluded.accepted,
			stored_as_page_id = excluded.stored_as_page_id
	`, topicID, runID, result.Title, result.URL, result.Source, result.Snippet, result.Rank, result.Score, result.PageType, result.Reason, accepted, pageID)
	if err != nil {
		return fmt.Errorf("upsert search result %q: %w", result.URL, err)
	}
	return nil
}

func normalizeResults(results []SearchResult, maxResults int) []storedResult {
	seen := map[string]struct{}{}
	normalized := make([]storedResult, 0, len(results))
	for i, result := range results {
		title := strings.TrimSpace(result.Title)
		rawURL := strings.TrimSpace(result.URL)
		if title == "" || rawURL == "" {
			continue
		}
		normalizedURL, source, err := normalizeURL(rawURL)
		if err != nil {
			continue
		}
		if isBlockedResult(source, normalizedURL) {
			continue
		}
		if _, exists := seen[normalizedURL]; exists {
			continue
		}
		seen[normalizedURL] = struct{}{}
		snippet := strings.TrimSpace(result.Content)
		normalized = append(normalized, storedResult{
			Title:    title,
			URL:      normalizedURL,
			Source:   source,
			Snippet:  snippet,
			Rank:     i + 1,
			Score:    interestingnessScore(title, normalizedURL, source, snippet, result.Score),
			Accepted: true,
		})
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].Score != normalized[j].Score {
			return normalized[i].Score > normalized[j].Score
		}
		return normalized[i].Rank < normalized[j].Rank
	})
	return normalized
}

func reviewResults(ctx context.Context, topic string, reviewer Reviewer, results []storedResult, minScore int) ([]storedResult, ReviewOutput, error) {
	candidates := make([]ReviewCandidate, 0, len(results))
	byIndex := map[int]int{}
	for i, result := range results {
		index := i + 1
		candidates = append(candidates, ReviewCandidate{
			Index:        index,
			Title:        result.Title,
			URL:          result.URL,
			Source:       result.Source,
			Snippet:      truncate(result.Snippet, 700),
			ProviderRank: result.Rank,
		})
		byIndex[index] = i
	}

	reviewOutput, err := reviewer.Review(ctx, topic, candidates)
	if err != nil {
		return nil, ReviewOutput{}, fmt.Errorf("review topic search results: %w", err)
	}

	reviewed := make([]storedResult, 0, len(results))
	for _, review := range reviewOutput.Results {
		resultIndex, ok := byIndex[review.Index]
		if !ok {
			continue
		}
		result := results[resultIndex]
		result.Score = review.DailyDocsScore
		result.PageType = strings.TrimSpace(review.PageType)
		result.Reason = sanitizeASCII(truncate(strings.TrimSpace(review.Reason), 500))
		result.Accepted = true
		if !review.ShouldStore || review.DailyDocsScore < minScore || rejectedPageType(review.PageType) || broadReadingURL(result.URL) {
			result.Accepted = false
		}
		reviewed = append(reviewed, result)
	}
	sort.SliceStable(reviewed, func(i, j int) bool {
		if reviewed[i].Accepted != reviewed[j].Accepted {
			return reviewed[i].Accepted
		}
		if reviewed[i].Score != reviewed[j].Score {
			return reviewed[i].Score > reviewed[j].Score
		}
		return reviewed[i].Rank < reviewed[j].Rank
	})
	return reviewed, reviewOutput, nil
}

func countAccepted(results []storedResult) int {
	count := 0
	for _, result := range results {
		if result.Accepted {
			count++
		}
	}
	return count
}

func capAcceptedResults(results []storedResult, maxAccepted int) []storedResult {
	if maxAccepted < 1 {
		return results
	}
	accepted := 0
	for i := range results {
		if !results[i].Accepted {
			continue
		}
		accepted++
		if accepted > maxAccepted {
			results[i].Accepted = false
		}
	}
	return results
}

func rejectedPageType(pageType string) bool {
	switch strings.TrimSpace(strings.ToLower(pageType)) {
	case "api", "landing", "listicle", "resource_list", "social":
		return true
	default:
		return false
	}
}

func broadReadingURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Host), "www.")
	path := strings.Trim(strings.ToLower(parsed.Path), "/")
	if path == "" {
		return true
	}
	switch host {
	case "doc.rust-lang.org":
		return path == "book" || path == "stable/book" || strings.HasSuffix(path, "/book")
	case "rust-lang.org":
		return path == "learn"
	default:
		return false
	}
}

func interestingnessScore(title string, normalizedURL string, host string, snippet string, providerScore float64) int {
	lowerTitle := strings.ToLower(title)
	lowerURL := strings.ToLower(normalizedURL)
	lowerSnippet := strings.ToLower(snippet)
	score := int(providerScore * 10)

	addForContains(&score, lowerTitle, 24, "guide", "tutorial", "concept", "deep dive", "ownership", "borrowing", "lifetime", "context", "transaction", "index")
	addForContains(&score, lowerURL, 20, "/book", "/guide", "/learn", "/docs", "/doc", "/reference", "/tutorial", "/manual")
	addForContains(&score, lowerSnippet, 12, "learn", "guide", "explain", "concept", "reference", "documentation", "tutorial")

	trimmedHost := strings.TrimPrefix(strings.ToLower(host), "www.")
	if strings.Contains(trimmedHost, "docs.") || strings.Contains(trimmedHost, "doc.") || strings.HasPrefix(trimmedHost, "developer.") || strings.HasPrefix(trimmedHost, "learn.") {
		score += 24
	}
	if strings.HasSuffix(trimmedHost, ".org") {
		score += 10
	}

	penalizeForContains(&score, lowerTitle, 30, "why ", "gold standard", "homepage", "home page", "best ", "learn rust", "programming language")
	penalizeForContains(&score, lowerURL, 30, "web.mit.edu", "/releases", "/news", "/blog", "/tags", "/search")
	if strings.HasSuffix(lowerURL, "/") || strings.Count(strings.TrimSuffix(lowerURL, "/"), "/") <= 2 {
		score -= 10
	}
	if strings.Contains(lowerSnippet, "mirror") || strings.Contains(lowerSnippet, "version 1.") {
		score -= 15
	}

	return score
}

func addForContains(score *int, value string, weight int, needles ...string) {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			*score += weight
			return
		}
	}
}

func penalizeForContains(score *int, value string, weight int, needles ...string) {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			*score -= weight
			return
		}
	}
}

func truncate(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if maxLen < 1 || len(value) <= maxLen {
		return value
	}
	return strings.TrimSpace(value[:maxLen])
}

func sanitizeASCII(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= 32 && r <= 126 {
			builder.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(builder.String()), " ")
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

func isBlockedResult(host string, normalizedURL string) bool {
	blockedDomains := []string{
		"facebook.com",
		"github.com",
		"instagram.com",
		"news.ycombinator.com",
		"reddit.com",
		"stackoverflow.com",
		"w3schools.com",
		"youtube.com",
	}
	trimmedHost := strings.TrimPrefix(strings.ToLower(host), "www.")
	for _, blockedDomain := range blockedDomains {
		if trimmedHost == blockedDomain || strings.HasSuffix(trimmedHost, "."+blockedDomain) {
			return true
		}
	}

	parsed, err := url.Parse(normalizedURL)
	if err != nil {
		return true
	}
	path := strings.ToLower(parsed.Path)
	return strings.HasSuffix(path, ".pdf")
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
