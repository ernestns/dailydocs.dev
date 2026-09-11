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

	"github.com/ernestns/daily-docs/internal/topicname"
)

const (
	DefaultMaxResults              = 3
	MinimumUsefulResults           = 2
	DefaultMinInterval             = 5 * time.Minute
	DefaultMinScore                = 65
	DefaultDailyLimit              = 20
	DefaultMaxPlannedSearches      = 6
	DefaultPlannedSearchResultSize = 3
	DefaultReviewBatchSize         = 20
	StaleRunTimeout                = 30 * time.Minute

	runStatusRunning     = "running"
	runStatusCompleted   = "completed"
	runStatusFailed      = "failed"
	runStatusRateLimited = "rate_limited"

	runStagePlanning  = "planning"
	runStageSearching = "searching"
	runStageReviewing = "reviewing"
	runStageStoring   = "storing"
)

var (
	ErrRateLimited         = errors.New("topic search rate limited")
	ErrInsufficientResults = errors.New("not enough useful distinct readings")
	ErrNoResults           = ErrInsufficientResults
	ErrInvalidTopic        = topicname.ErrInvalid
)

type Provider interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
}

type RequestProvider interface {
	SearchWithRequest(ctx context.Context, request SearchRequest) ([]SearchResult, error)
}

type Planner interface {
	Plan(ctx context.Context, topic string) (PlanOutput, error)
}

type Reviewer interface {
	Review(ctx context.Context, topic string, candidates []ReviewCandidate) (ReviewOutput, error)
}

type SearchRequest struct {
	Query          string
	MaxResults     int
	IncludeDomains []string
}

type SearchResult struct {
	Title   string
	URL     string
	Content string
	Score   float64
}

type PlannedTopic struct {
	Name           string   `json:"name"`
	Category       string   `json:"category"`
	Reason         string   `json:"reason"`
	SearchQueries  []string `json:"search_queries"`
	IncludeDomains []string `json:"include_domains"`
	ExpectedTerms  []string `json:"expected_terms"`
}

type PlanOutput struct {
	ValidTopic     *bool
	ValidityReason string
	Topics         []PlannedTopic
	Model          string
	InputTokens    int
	OutputTokens   int
	TotalTokens    int
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
	Provider                   Provider
	Planner                    Planner
	Reviewer                   Reviewer
	Now                        func() time.Time
	MaxResults                 int
	MinInterval                time.Duration
	MinScore                   int
	DailyLimit                 int
	MaxPlannedSearches         int
	MaxResultsPerPlannedSearch int
	ReviewBatchSize            int
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
	Reviewed     bool
}

func SearchTopic(ctx context.Context, conn *sql.DB, topic string, opts Options) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	slug, name, err := ResolveTopic(ctx, conn, topic)
	if err != nil {
		return Result{}, err
	}
	return searchResolvedTopic(ctx, conn, slug, name, opts)
}

func searchResolvedTopic(ctx context.Context, conn *sql.DB, slug, name string, opts Options) (result Result, err error) {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	if err := topicname.Validate(name); err != nil {
		return Result{}, err
	}
	if opts.Provider == nil {
		return Result{}, errors.New("topic search provider is required")
	}

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
	maxPlannedSearches := opts.MaxPlannedSearches
	if maxPlannedSearches < 1 {
		maxPlannedSearches = DefaultMaxPlannedSearches
	}
	maxResultsPerPlannedSearch := opts.MaxResultsPerPlannedSearch
	if maxResultsPerPlannedSearch < 1 {
		maxResultsPerPlannedSearch = DefaultPlannedSearchResultSize
	}
	reviewBatchSize := opts.ReviewBatchSize
	if reviewBatchSize < 1 {
		reviewBatchSize = DefaultReviewBatchSize
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

	searchLimit := maxResults
	if opts.Reviewer != nil {
		searchLimit = maxResults * 2
	}
	searchRequests := []SearchRequest{{Query: buildQuery(name), MaxResults: searchLimit}}
	stage := runStageSearching
	if opts.Planner != nil {
		stage = runStagePlanning
	}
	runID, err := createSearchRun(ctx, conn, topicID, summarizeSearchRequests(searchRequests), runStatusRunning, stage, now)
	if err != nil {
		return Result{}, err
	}
	result = Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: runStatusFailed}
	defer func() {
		if err != nil {
			result.Status = runStatusFailed
			err = errors.Join(err, failRunAndTopic(ctx, conn, topicID, runID, err))
		}
	}()

	if opts.Planner != nil {
		plan, planErr := opts.Planner.Plan(ctx, name)
		if planErr != nil {
			return result, planErr
		}
		if plan.ValidTopic != nil && !*plan.ValidTopic {
			invalid := fmt.Errorf("%w: %s", ErrInvalidTopic, sanitizeASCII(truncate(plan.ValidityReason, 240)))
			return result, invalid
		}
		searchRequests = plannedSearchRequests(name, plan, maxPlannedSearches, maxResultsPerPlannedSearch)
		if len(searchRequests) == 0 {
			searchRequests = []SearchRequest{{Query: buildQuery(name), MaxResults: searchLimit}}
		}
		if err := updateSearchRunQueryAndStage(ctx, conn, runID, summarizeSearchRequests(searchRequests), runStageSearching); err != nil {
			return result, err
		}
	}

	return discoverReadings(ctx, conn, topicID, runID, slug, name, searchRequests, opts, maxResults, minScore, reviewBatchSize)
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

	var slug, topic string
	err = conn.QueryRowContext(ctx, `
		SELECT slug, name
		FROM topics
		WHERE status = 'queued'
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`).Scan(&slug, &topic)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueResult{}, nil
	}
	if err != nil {
		return QueueResult{}, fmt.Errorf("read next queued topic: %w", err)
	}

	result, err := searchResolvedTopic(ctx, conn, slug, topic, opts)
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

	var topic, status, latestRun string
	var topicID int64
	err = conn.QueryRowContext(ctx, `
        SELECT id,name,status,COALESCE((SELECT status FROM topic_search_runs WHERE topic_id=topics.id ORDER BY id DESC LIMIT 1),'')
        FROM topics WHERE slug=? AND status IN ('queued','failed','active')
    `, slug).Scan(&topicID, &topic, &status, &latestRun)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueResult{}, nil
	}
	if err != nil {
		return QueueResult{}, fmt.Errorf("read queued topic %q: %w", slug, err)
	}
	if status == "active" && latestRun != "failed" {
		available, err := AvailableReadings(ctx, conn, topicID)
		if err != nil {
			return QueueResult{}, err
		}
		if available >= MinimumUsefulResults {
			return QueueResult{}, nil
		}
	}

	result, err := searchResolvedTopic(ctx, conn, slug, topic, opts)
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

func plannedSearchRequests(topic string, plan PlanOutput, maxSearches int, maxResultsPerSearch int) []SearchRequest {
	if maxSearches < 1 {
		maxSearches = DefaultMaxPlannedSearches
	}
	if maxResultsPerSearch < 1 {
		maxResultsPerSearch = DefaultPlannedSearchResultSize
	}

	seen := map[string]struct{}{}
	requests := make([]SearchRequest, 0, maxSearches)
	for _, planned := range plan.Topics {
		for _, query := range planned.SearchQueries {
			query = strings.TrimSpace(query)
			if query == "" {
				continue
			}
			key := strings.ToLower(query)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, SearchRequest{
				Query:          query,
				MaxResults:     maxResultsPerSearch,
				IncludeDomains: sanitizeDomains(planned.IncludeDomains),
			})
			break
		}
		if len(requests) >= maxSearches {
			break
		}
	}

	if len(requests) == 0 {
		return []SearchRequest{{Query: buildQuery(topic), MaxResults: maxResultsPerSearch}}
	}
	return requests
}

func executeSearchRequests(ctx context.Context, provider Provider, requests []SearchRequest) ([]SearchResult, error) {
	results := make([]SearchResult, 0)
	for _, request := range requests {
		query := strings.TrimSpace(request.Query)
		if query == "" {
			continue
		}
		limit := request.MaxResults
		if limit < 1 {
			limit = DefaultMaxResults
		}
		request.Query = query
		request.MaxResults = limit

		var searchResults []SearchResult
		var err error
		if requestProvider, ok := provider.(RequestProvider); ok {
			searchResults, err = requestProvider.SearchWithRequest(ctx, request)
		} else {
			searchResults, err = provider.Search(ctx, query, limit)
		}
		if err != nil {
			return results, fmt.Errorf("search query %q: %w", query, err)
		}
		if len(searchResults) > limit {
			searchResults = searchResults[:limit]
		}
		results = append(results, searchResults...)
	}
	return results, nil
}

func summarizeSearchRequests(requests []SearchRequest) string {
	if len(requests) == 0 {
		return ""
	}
	if len(requests) == 1 {
		return requests[0].Query
	}
	queries := make([]string, 0, len(requests))
	for _, request := range requests {
		query := strings.TrimSpace(request.Query)
		if query != "" {
			queries = append(queries, query)
		}
	}
	return truncate(strings.Join(queries, "\n"), 4000)
}

func sanitizeDomains(domains []string) []string {
	seen := map[string]struct{}{}
	cleaned := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.TrimSpace(strings.ToLower(domain))
		domain = strings.TrimPrefix(domain, "https://")
		domain = strings.TrimPrefix(domain, "http://")
		domain = strings.TrimPrefix(domain, "www.")
		domain = strings.Trim(domain, "/")
		if domain == "" || strings.ContainsAny(domain, "/?#") {
			continue
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		cleaned = append(cleaned, domain)
	}
	return cleaned
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
	if _, err := tx.ExecContext(ctx, `
		UPDATE topics
		SET status = 'failed',
			updated_at = ?
		WHERE status = 'searching'
			AND NOT EXISTS (
				SELECT 1
				FROM topic_search_runs
				WHERE topic_search_runs.topic_id = topics.id
					AND topic_search_runs.status = ?
			)
	`, formatTime(now), runStatusRunning); err != nil {
		return fmt.Errorf("expire stale search topics: %w", err)
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

func updateSearchRunQueryAndStage(ctx context.Context, conn *sql.DB, runID int64, query string, stage string) error {
	if _, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET query = ?,
			stage = ?
		WHERE id = ?
	`, query, stage, runID); err != nil {
		return fmt.Errorf("update topic search query: %w", err)
	}
	return nil
}

func failRunAndTopic(ctx context.Context, conn *sql.DB, topicID int64, runID int64, runErr error) error {
	// A provider deadline/cancellation must not leave the global running slot occupied.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	updated, err := conn.ExecContext(ctx, `
		UPDATE topic_search_runs
		SET status = 'failed',
			stage = '',
			completed_at = datetime('now'),
			error = ?
		WHERE id = ? AND status = 'running'
	`, runErr.Error(), runID)
	if err != nil {
		return fmt.Errorf("record topic search failure: %w", err)
	}
	if count, err := updated.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return nil
	}
	available, err := AvailableReadings(ctx, conn, topicID)
	if err != nil {
		return err
	}
	status := "failed"
	if available >= MinimumUsefulResults {
		status = "active"
	}
	if _, err := conn.ExecContext(ctx, `
        UPDATE topics SET status=?, updated_at=datetime('now') WHERE id=?
    `, status, topicID); err != nil {
		return fmt.Errorf("record failed topic status: %w", err)
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
	result, err := reuseStoredSQLiteURL(ctx, tx, "pages", topicID, result)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `
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
	result, err := reuseStoredSQLiteURL(ctx, tx, "topic_search_results", topicID, result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
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
	result, err := reuseStoredSQLiteURL(ctx, tx, "topic_search_results", topicID, result)
	if err != nil {
		return err
	}
	accepted := 0
	var score any
	if result.Reviewed {
		score = result.Score
	}
	if result.Accepted {
		accepted = 1
	}
	_, err = tx.ExecContext(ctx, `
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
	`, topicID, runID, result.Title, result.URL, result.Source, result.Snippet, result.Rank, score, result.PageType, result.Reason, accepted, pageID)
	if err != nil {
		return fmt.Errorf("upsert search result %q: %w", result.URL, err)
	}
	return nil
}

// Resolve the same verified SQLite identity across searches as within one batch.
// Existing IDs, reading order and references survive; historical duplicates are not deleted.
func reuseStoredSQLiteURL(ctx context.Context, tx *sql.Tx, table string, topicID int64, result storedResult) (storedResult, error) {
	parsed, err := url.Parse(readingURLKey(result.URL))
	if err != nil || parsed.User != nil || parsed.Host != "sqlite.org" || parsed.Scheme != "https" {
		return result, nil
	}
	// Table names are internal constants, never provider or request input.
	if table != "pages" && table != "topic_search_results" {
		return storedResult{}, fmt.Errorf("unsupported reading identity table %q", table)
	}
	var aliases []any
	for _, scheme := range []string{"https", "http"} {
		for _, host := range []string{"sqlite.org", "www.sqlite.org"} {
			parsed.Scheme, parsed.Host = scheme, host
			aliases = append(aliases, parsed.String())
		}
	}
	var id int64
	var existingURL string
	// Prefer an exact existing row if a historical catalog already contains aliases.
	err = tx.QueryRowContext(ctx, "SELECT id, url FROM "+table+`
		WHERE topic_id = ? AND url IN (?, ?, ?, ?)
		ORDER BY (url = ?) DESC, id LIMIT 1
	`, topicID, aliases[0], aliases[1], aliases[2], aliases[3], result.URL).Scan(&id, &existingURL)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return storedResult{}, fmt.Errorf("resolve stored SQLite URL: %w", err)
	}
	if strings.HasPrefix(existingURL, "http://") && strings.HasPrefix(result.URL, "https://") {
		// Upgrade only to the actual HTTPS destination returned by this search.
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET url = ? WHERE id = ?", result.URL, id); err != nil {
			return storedResult{}, fmt.Errorf("update stored SQLite destination: %w", err)
		}
	} else {
		result.URL, result.Source, err = normalizeURL(existingURL)
		if err != nil {
			return storedResult{}, err
		}
	}
	return result, nil
}

func normalizeResults(results []SearchResult) []storedResult {
	seen := map[string]int{}
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
		key := readingURLKey(normalizedURL)
		if prior, exists := seen[key]; exists {
			// Prefer a returned HTTPS link when a verified alias was first found over HTTP.
			if strings.HasPrefix(normalized[prior].URL, "http://") && strings.HasPrefix(normalizedURL, "https://") {
				normalized[prior].URL = normalizedURL
				normalized[prior].Source = source
			}
			continue
		}
		seen[key] = len(normalized)
		snippet := strings.TrimSpace(result.Content)
		normalized = append(normalized, storedResult{
			Title:    title,
			URL:      normalizedURL,
			Source:   source,
			Snippet:  snippet,
			Rank:     i + 1,
			Score:    interestingnessScore(title, normalizedURL, source, snippet, result.Score),
			Accepted: true,
			Reviewed: true,
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

func reviewResults(ctx context.Context, topic string, reviewer Reviewer, results []storedResult, minScore int, batchSize int) ([]storedResult, ReviewOutput, error) {
	if batchSize < 1 || batchSize >= len(results) {
		return reviewResultBatch(ctx, topic, reviewer, results, minScore)
	}

	reviewed := make([]storedResult, 0, len(results))
	var combined ReviewOutput
	for start := 0; start < len(results); start += batchSize {
		end := start + batchSize
		if end > len(results) {
			end = len(results)
		}
		batchReviewed, batchOutput, err := reviewResultBatch(ctx, topic, reviewer, results[start:end], minScore)
		reviewed = append(reviewed, batchReviewed...)
		mergeReviewUsage(&combined, batchOutput)
		if err != nil {
			reviewed = append(reviewed, unreviewedResults(results[end:])...)
			sortReviewedResults(reviewed)
			return reviewed, combined, err
		}
	}
	sortReviewedResults(reviewed)
	return reviewed, combined, nil
}

func reviewResultBatch(ctx context.Context, topic string, reviewer Reviewer, results []storedResult, minScore int) ([]storedResult, ReviewOutput, error) {
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
		return unreviewedResults(results), ReviewOutput{}, fmt.Errorf("review topic search results: %w", err)
	}

	// Keep omitted candidates visible as unreviewed, without inventing a rejection.
	reviewed := append([]storedResult(nil), results...)
	for i := range reviewed {
		reviewed[i].Accepted = false
		reviewed[i].Reviewed = false
		reviewed[i].Score = 0
		reviewed[i].PageType = ""
		reviewed[i].Reason = ""
	}
	seen := make(map[int]bool, len(reviewOutput.Results))
	for _, review := range reviewOutput.Results {
		resultIndex, ok := byIndex[review.Index]
		if !ok || seen[review.Index] {
			return unreviewedResults(results), reviewOutput, fmt.Errorf("review returned invalid or duplicate candidate index %d", review.Index)
		}
		seen[review.Index] = true
		result := results[resultIndex]
		result.Reviewed = true
		result.Score = review.DailyDocsScore
		result.PageType = strings.TrimSpace(review.PageType)
		result.Reason = sanitizeASCII(truncate(strings.TrimSpace(review.Reason), 500))
		result.Accepted = true
		if !review.ShouldStore || review.DailyDocsScore < minScore || rejectedPageType(review.PageType) || broadReadingURL(result.URL) {
			result.Accepted = false
		}
		reviewed[resultIndex] = result
	}
	sortReviewedResults(reviewed)
	return reviewed, reviewOutput, nil
}

func sortReviewedResults(reviewed []storedResult) {
	sort.SliceStable(reviewed, func(i, j int) bool {
		if reviewed[i].Accepted != reviewed[j].Accepted {
			return reviewed[i].Accepted
		}
		if reviewed[i].Score != reviewed[j].Score {
			return reviewed[i].Score > reviewed[j].Score
		}
		return reviewed[i].Rank < reviewed[j].Rank
	})
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
	if parsed.Fragment == "top" {
		parsed.Fragment = ""
	}
	query, queryErr := url.ParseQuery(parsed.RawQuery)
	removedTracking := false
	if queryErr == nil {
		for key := range query {
			if strings.HasPrefix(strings.ToLower(key), "utm_") || key == "gclid" || key == "fbclid" {
				query.Del(key)
				removedTracking = true
			}
		}
	}
	if removedTracking {
		parsed.RawQuery = query.Encode()
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Path != "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	return parsed.String(), parsed.Host, nil
}

// SQLite serves these documented host/scheme aliases as the same resource.
// Do not assume www or HTTP aliases for unrelated sites, or discard query/section identity.
func readingURLKey(rawURL string) string {
	normalized, _, err := normalizeURL(rawURL)
	if err != nil {
		return rawURL
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return rawURL
	}
	if parsed.User == nil && (parsed.Host == "sqlite.org" || parsed.Host == "www.sqlite.org") {
		parsed.Host = "sqlite.org"
		parsed.Scheme = "https"
	}
	return parsed.String()
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

func displayTopicName(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	return topicname.Display(value)
}
