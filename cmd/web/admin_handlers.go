package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ernestns/daily-docs/internal/activation"
	"github.com/ernestns/daily-docs/internal/pipeline"
	"github.com/ernestns/daily-docs/internal/topicsource"
)

const adminSessionCookie = "dailydocs_admin"
const adminSessionTTL = 12 * time.Hour

type adminSubmissionRow struct {
	ID             int64
	SuggestedTopic string
	SourceHost     string
	Status         string
	RequestCount   int
	LastSubmitted  string
	LastError      string
}

type adminSubmissionDetail struct {
	ID             int64
	SuggestedTopic string
	SuggestedSlug  string
	SourceHost     string
	Status         string
	RequestCount   int
	SubmittedURL   string
	NormalizedURL  string
	LastSubmitted  string
	LastError      string
	Sources        []adminSourceRow
	Runs           []adminRunRow
	Candidates     []adminCandidateRow
}

type adminSourceRow struct {
	ID               int64
	SubmissionID     int64
	TopicID          int64
	TopicSlug        string
	TopicName        string
	Status           string
	SourceType       string
	BaseURL          string
	NormalizedURL    string
	LastProcessedAt  string
	LastError        string
	LastDiscoveredAt string
	DiscoveryCount   int
	DiscoverySample  []string
	DiscoveryError   string
	DiscoveryStatus  string
	WorkflowStatus   string
	NextAction       string
}

type adminSourceDetail struct {
	adminSourceRow
	Runs       []adminRunRow
	Candidates []adminCandidateRow
}

type adminRunDetail struct {
	adminRunRow
	SubmissionID int64
	SourceID     int64
	TopicSlug    string
	TopicName    string
	SourceURL    string
	Candidates   []adminCandidateRow
}

type adminRunRow struct {
	ID              int64
	Status          string
	StartedAt       string
	CompletedAt     string
	DiscoveredCount int
	CrawledCount    int
	EligibleCount   int
	RejectedCount   int
	FailureCount    int
	Error           string
}

type adminCandidateRow struct {
	ID               int64
	Title            string
	URL              string
	Classification   string
	Score            int
	Gate             string
	RejectStage      string
	Status           string
	EstimatedMinutes string
	Reason           string
	TokenSummary     string
	ReviewModel      string
	Confidence       string
	Rationale        string
}

func (a app) adminHandler(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("ADMIN_TOKEN")
	if token == "" {
		http.NotFound(w, r)
		return
	}

	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/admin"
	}

	if path == "/admin/login" {
		a.adminLoginHandler(w, r, token)
		return
	}

	if !validAdminSession(r, token) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}

	switch {
	case path == "/admin":
		http.Redirect(w, r, "/admin/submissions", http.StatusSeeOther)
	case path == "/admin/sources":
		a.adminSourcesHandler(w, r, token)
	case strings.HasPrefix(path, "/admin/sources/"):
		a.adminSourceHandler(w, r, token, path)
	case strings.HasPrefix(path, "/admin/runs/"):
		a.adminRunHandler(w, r, path)
	case path == "/admin/submissions":
		a.adminSubmissionsHandler(w, r, token)
	case strings.HasPrefix(path, "/admin/submissions/"):
		a.adminSubmissionHandler(w, r, token, path)
	default:
		http.NotFound(w, r)
	}
}

func (a app) adminRunHandler(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runID, err := parsePositiveID(strings.TrimPrefix(path, "/admin/runs/"), "run-id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	detail, err := adminGetRun(r.Context(), a.db, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("admin show run failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, adminRunDetailTemplate, struct {
		Run adminRunDetail
	}{Run: detail})
}

func (a app) adminSourceHandler(w http.ResponseWriter, r *http.Request, token string, path string) {
	rest := strings.TrimPrefix(path, "/admin/sources/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	sourceID, err := parsePositiveID(parts[0], "source-id")
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.adminSourceDetailHandler(w, r, token, sourceID)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validCSRF(r, token) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	switch parts[1] {
	case "discover":
		count, err := a.discoverSourcePreview(r.Context(), sourceID)
		if err != nil {
			log.Printf("admin discover source failed source_id=%d error=%v", sourceID, err)
			redirectAdminSource(w, r, sourceID, "", err.Error())
			return
		}
		redirectAdminSource(w, r, sourceID, fmt.Sprintf("discovered %d candidate URLs", count), "")
	case "process":
		result, err := pipeline.ProcessSource(r.Context(), a.db, sourceID, pipeline.Options{})
		if err != nil {
			log.Printf("admin process source failed source_id=%d error=%v", sourceID, err)
			redirectAdminSource(w, r, sourceID, "", err.Error())
			return
		}
		redirectAdminSource(w, r, sourceID, fmt.Sprintf("processed source %d: %d eligible", sourceID, result.EligibleCount), "")
	case "activate":
		result, err := activation.ActivateSourceCandidates(r.Context(), a.db, sourceID)
		if err != nil {
			log.Printf("admin activate source failed source_id=%d error=%v", sourceID, err)
			redirectAdminSource(w, r, sourceID, "", err.Error())
			return
		}
		redirectAdminSource(w, r, sourceID, fmt.Sprintf("activated %d candidates", result.Activated), "")
	case "create-source":
		current, err := topicsource.Load(r.Context(), a.db, sourceID)
		if err != nil {
			log.Printf("admin load source failed source_id=%d error=%v", sourceID, err)
			redirectAdminSource(w, r, sourceID, "", err.Error())
			return
		}
		created, err := topicsource.CreateForTopic(r.Context(), a.db, topicsource.CreateForTopicInput{
			TopicID: current.TopicID,
			URL:     r.Form.Get("url"),
		})
		if err != nil {
			log.Printf("admin create sibling source failed source_id=%d error=%v", sourceID, err)
			redirectAdminSource(w, r, sourceID, "", err.Error())
			return
		}
		redirectAdminSource(w, r, created.ID, fmt.Sprintf("created source %d", created.ID), "")
	default:
		http.NotFound(w, r)
	}
}

func (a app) adminSourcesHandler(w http.ResponseWriter, r *http.Request, token string) {
	switch r.Method {
	case http.MethodGet:
		sources, err := adminListAllSources(r.Context(), a.db)
		if err != nil {
			log.Printf("admin list sources failed: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		renderTemplate(w, adminSourcesTemplate, struct {
			Sources []adminSourceRow
			CSRF    string
			Notice  string
			Error   string
		}{
			Sources: sources,
			CSRF:    csrfToken(r, token),
			Notice:  r.URL.Query().Get("notice"),
			Error:   r.URL.Query().Get("error"),
		})
	case http.MethodPost:
		if !validCSRF(r, token) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		sourceID, err := parsePositiveID(r.Form.Get("source_id"), "source-id")
		if err != nil {
			redirectAdminSources(w, r, "", err.Error())
			return
		}
		result, err := pipeline.ProcessSource(r.Context(), a.db, sourceID, pipeline.Options{})
		if err != nil {
			log.Printf("admin process source failed source_id=%d error=%v", sourceID, err)
			redirectAdminSources(w, r, "", err.Error())
			return
		}
		redirectAdminSources(w, r, fmt.Sprintf("processed source %d: %d eligible", sourceID, result.EligibleCount), "")
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a app) adminSourceDetailHandler(w http.ResponseWriter, r *http.Request, token string, sourceID int64) {
	detail, err := adminGetSource(r.Context(), a.db, sourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("admin show source failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, adminSourceDetailTemplate, struct {
		Source adminSourceDetail
		CSRF   string
		Notice string
		Error  string
	}{
		Source: detail,
		CSRF:   csrfToken(r, token),
		Notice: r.URL.Query().Get("notice"),
		Error:  r.URL.Query().Get("error"),
	})
}

func (a app) adminLoginHandler(w http.ResponseWriter, r *http.Request, token string) {
	switch r.Method {
	case http.MethodGet:
		renderTemplate(w, adminLoginTemplate, struct{ Error string }{})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !constantTimeEqual(r.Form.Get("token"), token) {
			log.Printf("admin login failed ip=%s", clientIP(r))
			renderTemplate(w, adminLoginTemplate, struct{ Error string }{Error: "Invalid token"})
			return
		}
		setAdminSession(w, token)
		http.Redirect(w, r, "/admin/submissions", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a app) adminSubmissionsHandler(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	submissions, err := adminListSubmissions(r.Context(), a.db)
	if err != nil {
		log.Printf("admin list submissions failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, adminSubmissionsTemplate, struct {
		Submissions []adminSubmissionRow
		CSRF        string
		Notice      string
		Error       string
	}{
		Submissions: submissions,
		CSRF:        csrfToken(r, token),
		Notice:      r.URL.Query().Get("notice"),
		Error:       r.URL.Query().Get("error"),
	})
}

func (a app) adminSubmissionHandler(w http.ResponseWriter, r *http.Request, token string, path string) {
	rest := strings.TrimPrefix(path, "/admin/submissions/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	submissionID, err := parsePositiveID(parts[0], "submission-id")
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.adminSubmissionDetailHandler(w, r, token, submissionID)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validCSRF(r, token) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	switch parts[1] {
	case "process":
		_, err := pipeline.ProcessSubmission(r.Context(), a.db, submissionID, pipeline.Options{})
		if err != nil {
			log.Printf("admin process submission failed id=%d error=%v", submissionID, err)
			redirectAdminSubmission(w, r, submissionID, "", err.Error())
			return
		}
		redirectAdminSubmission(w, r, submissionID, "processed", "")
	case "activate":
		_, err := activation.ActivateCandidates(r.Context(), a.db, submissionID)
		if err != nil {
			log.Printf("admin activate candidates failed id=%d error=%v", submissionID, err)
			redirectAdminSubmission(w, r, submissionID, "", err.Error())
			return
		}
		redirectAdminSubmission(w, r, submissionID, "activated", "")
	case "create-source":
		source, err := topicsource.CreateFromSubmission(r.Context(), a.db, topicsource.CreateFromSubmissionInput{
			SubmissionID: submissionID,
			TopicSlug:    r.Form.Get("topic_slug"),
			TopicName:    r.Form.Get("topic_name"),
		})
		if err != nil {
			log.Printf("admin create source failed submission_id=%d error=%v", submissionID, err)
			redirectAdminSubmission(w, r, submissionID, "", err.Error())
			return
		}
		redirectAdminSubmission(w, r, submissionID, fmt.Sprintf("created source %d", source.ID), "")
	case "process-source":
		sourceID, err := parsePositiveID(r.Form.Get("source_id"), "source-id")
		if err != nil {
			redirectAdminSubmission(w, r, submissionID, "", err.Error())
			return
		}
		source, err := topicsource.Load(r.Context(), a.db, sourceID)
		if err != nil {
			log.Printf("admin load source failed submission_id=%d source_id=%d error=%v", submissionID, sourceID, err)
			redirectAdminSubmission(w, r, submissionID, "", err.Error())
			return
		}
		if !source.CreatedFromSubmissionID.Valid || source.CreatedFromSubmissionID.Int64 != submissionID {
			redirectAdminSubmission(w, r, submissionID, "", "source does not belong to submission")
			return
		}
		result, err := pipeline.ProcessSource(r.Context(), a.db, sourceID, pipeline.Options{})
		if err != nil {
			log.Printf("admin process source failed submission_id=%d source_id=%d error=%v", submissionID, sourceID, err)
			redirectAdminSubmission(w, r, submissionID, "", err.Error())
			return
		}
		redirectAdminSubmission(w, r, submissionID, fmt.Sprintf("processed source %d: %d eligible", sourceID, result.EligibleCount), "")
	default:
		http.NotFound(w, r)
	}
}

func (a app) adminSubmissionDetailHandler(w http.ResponseWriter, r *http.Request, token string, submissionID int64) {
	detail, err := adminGetSubmission(r.Context(), a.db, submissionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("admin show submission failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, adminSubmissionDetailTemplate, struct {
		Submission adminSubmissionDetail
		CSRF       string
		Notice     string
		Error      string
	}{
		Submission: detail,
		CSRF:       csrfToken(r, token),
		Notice:     r.URL.Query().Get("notice"),
		Error:      r.URL.Query().Get("error"),
	})
}

func (a app) discoverSourcePreview(ctx context.Context, sourceID int64) (int, error) {
	source, err := topicsource.Load(ctx, a.db, sourceID)
	if err != nil {
		return 0, err
	}
	discovery, err := pipeline.DiscoverURL(ctx, source.NormalizedURL, pipeline.Options{MaxPages: 50, MaxDepth: 2})
	preview := topicsource.DiscoveryPreview{}
	if err != nil {
		preview.Error = err.Error()
		var tooBroad pipeline.DiscoveryTooBroadError
		if errors.As(err, &tooBroad) {
			preview.Count = tooBroad.Count
			preview.NeedsScope = true
		}
		if recordErr := topicsource.RecordDiscoveryPreview(ctx, a.db, sourceID, preview); recordErr != nil {
			log.Printf("record discovery preview failed source_id=%d error=%v", sourceID, recordErr)
		}
		return preview.Count, err
	}
	preview.Count = discovery.DiscoveredCount
	preview.Sample = discovery.URLs
	if err := topicsource.RecordDiscoveryPreview(ctx, a.db, sourceID, preview); err != nil {
		return 0, err
	}
	return discovery.DiscoveredCount, nil
}

func sourceDiscoveryStatus(source adminSourceRow) string {
	switch source.Status {
	case "needs_scope", "discovery_failed", "ready_to_process", "pending_discovery":
		return source.Status
	default:
		if source.DiscoveryError != "" {
			return "discovery_failed"
		}
		if source.DiscoveryCount > 0 {
			return "ready_to_process"
		}
		return "not_discovered"
	}
}

func sourceWorkflowStatus(source adminSourceRow, runs []adminRunRow, candidates []adminCandidateRow) (string, string) {
	switch source.Status {
	case "pending_discovery":
		return "Submission -> Source", "Discover"
	case "ready_to_process":
		return "Submission -> Source -> Discovery", "Process"
	case "processing":
		return "Submission -> Source -> Discovery -> Processing", "Wait for processing"
	case "candidates_ready":
		if hasEligibleCandidates(candidates) {
			return "Submission -> Source -> Discovery -> Process -> Review", "Activate candidates"
		}
		return "Submission -> Source -> Discovery -> Process", "Review rejected candidates"
	case "needs_scope":
		return "Submission -> Source -> Discovery", "Add narrower source"
	case "discovery_failed":
		return "Submission -> Source", "Fix URL or discover again"
	case "disabled":
		return "Disabled", "No action"
	default:
		if len(runs) > 0 {
			return "Submission -> Source -> Process", "Review latest run"
		}
		return "Submission -> Source", "Discover"
	}
}

func hasEligibleCandidates(candidates []adminCandidateRow) bool {
	for _, candidate := range candidates {
		if candidate.Status == "eligible" {
			return true
		}
	}
	return false
}

func adminListSubmissions(ctx context.Context, conn *sql.DB) ([]adminSubmissionRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, suggested_topic, source_host, status, request_count, last_submitted_at, last_error
		FROM documentation_submissions
		ORDER BY id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query admin submissions: %w", err)
	}
	defer rows.Close()

	var submissions []adminSubmissionRow
	for rows.Next() {
		var sub adminSubmissionRow
		if err := rows.Scan(&sub.ID, &sub.SuggestedTopic, &sub.SourceHost, &sub.Status, &sub.RequestCount, &sub.LastSubmitted, &sub.LastError); err != nil {
			return nil, fmt.Errorf("scan admin submission: %w", err)
		}
		submissions = append(submissions, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin submissions: %w", err)
	}
	return submissions, nil
}

func adminGetSubmission(ctx context.Context, conn *sql.DB, submissionID int64) (adminSubmissionDetail, error) {
	var detail adminSubmissionDetail
	err := conn.QueryRowContext(ctx, `
		SELECT id, suggested_topic, source_host, status, request_count, submitted_url, normalized_url, last_submitted_at, last_error
		FROM documentation_submissions
		WHERE id = ?
	`, submissionID).Scan(&detail.ID, &detail.SuggestedTopic, &detail.SourceHost, &detail.Status, &detail.RequestCount, &detail.SubmittedURL, &detail.NormalizedURL, &detail.LastSubmitted, &detail.LastError)
	if err != nil {
		return adminSubmissionDetail{}, err
	}
	detail.SuggestedSlug = slugFromTopicName(detail.SuggestedTopic)

	sources, err := adminListSources(ctx, conn, submissionID)
	if err != nil {
		return adminSubmissionDetail{}, err
	}
	runs, err := adminListRuns(ctx, conn, submissionID)
	if err != nil {
		return adminSubmissionDetail{}, err
	}
	candidates, err := adminListCandidates(ctx, conn, submissionID)
	if err != nil {
		return adminSubmissionDetail{}, err
	}
	detail.Sources = sources
	detail.Runs = runs
	detail.Candidates = candidates
	return detail, nil
}

func adminListSources(ctx context.Context, conn *sql.DB, submissionID int64) ([]adminSourceRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT
			ts.id,
			ts.topic_id,
			t.slug,
			t.name,
			ts.status,
			ts.source_type,
			ts.base_url,
			ts.normalized_url,
			COALESCE(ts.last_processed_at, ''),
			ts.last_error
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE ts.created_from_submission_id = ?
		ORDER BY ts.id DESC
	`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("query admin sources: %w", err)
	}
	defer rows.Close()

	var sources []adminSourceRow
	for rows.Next() {
		var source adminSourceRow
		if err := rows.Scan(&source.ID, &source.TopicID, &source.TopicSlug, &source.TopicName, &source.Status, &source.SourceType, &source.BaseURL, &source.NormalizedURL, &source.LastProcessedAt, &source.LastError); err != nil {
			return nil, fmt.Errorf("scan admin source: %w", err)
		}
		source.SubmissionID = submissionID
		if source.LastProcessedAt == "" {
			source.LastProcessedAt = "-"
		}
		if source.LastError == "" {
			source.LastError = "-"
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin sources: %w", err)
	}
	return sources, nil
}

func adminListAllSources(ctx context.Context, conn *sql.DB) ([]adminSourceRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT
			ts.id,
			COALESCE(ts.created_from_submission_id, 0),
			ts.topic_id,
			t.slug,
			t.name,
			ts.status,
			ts.source_type,
			ts.base_url,
			ts.normalized_url,
			COALESCE(ts.last_processed_at, ''),
			ts.last_error
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		ORDER BY ts.id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query admin all sources: %w", err)
	}
	defer rows.Close()

	var sources []adminSourceRow
	for rows.Next() {
		var source adminSourceRow
		if err := rows.Scan(&source.ID, &source.SubmissionID, &source.TopicID, &source.TopicSlug, &source.TopicName, &source.Status, &source.SourceType, &source.BaseURL, &source.NormalizedURL, &source.LastProcessedAt, &source.LastError); err != nil {
			return nil, fmt.Errorf("scan admin all source: %w", err)
		}
		if source.LastProcessedAt == "" {
			source.LastProcessedAt = "-"
		}
		if source.LastError == "" {
			source.LastError = "-"
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin all sources: %w", err)
	}
	return sources, nil
}

func adminGetSource(ctx context.Context, conn *sql.DB, sourceID int64) (adminSourceDetail, error) {
	var detail adminSourceDetail
	var discoverySample string
	err := conn.QueryRowContext(ctx, `
		SELECT
			ts.id,
			COALESCE(ts.created_from_submission_id, 0),
			ts.topic_id,
			t.slug,
			t.name,
			ts.status,
			ts.source_type,
			ts.base_url,
			ts.normalized_url,
			COALESCE(ts.last_processed_at, ''),
			ts.last_error,
			COALESCE(ts.last_discovered_at, ''),
			ts.discovery_count,
			ts.discovery_sample,
			ts.discovery_error
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE ts.id = ?
	`, sourceID).Scan(&detail.ID, &detail.SubmissionID, &detail.TopicID, &detail.TopicSlug, &detail.TopicName, &detail.Status, &detail.SourceType, &detail.BaseURL, &detail.NormalizedURL, &detail.LastProcessedAt, &detail.LastError, &detail.LastDiscoveredAt, &detail.DiscoveryCount, &discoverySample, &detail.DiscoveryError)
	if err != nil {
		return adminSourceDetail{}, err
	}
	if detail.LastProcessedAt == "" {
		detail.LastProcessedAt = "-"
	}
	if detail.LastError == "" {
		detail.LastError = "-"
	}
	if detail.LastDiscoveredAt == "" {
		detail.LastDiscoveredAt = "-"
	}
	if discoverySample != "" {
		_ = json.Unmarshal([]byte(discoverySample), &detail.DiscoverySample)
	}
	runs, err := adminListSourceRuns(ctx, conn, sourceID)
	if err != nil {
		return adminSourceDetail{}, err
	}
	candidates, err := adminListSourceCandidates(ctx, conn, sourceID)
	if err != nil {
		return adminSourceDetail{}, err
	}
	detail.Runs = runs
	detail.Candidates = candidates
	detail.DiscoveryStatus = sourceDiscoveryStatus(detail.adminSourceRow)
	detail.WorkflowStatus, detail.NextAction = sourceWorkflowStatus(detail.adminSourceRow, runs, candidates)
	return detail, nil
}

func adminListSourceRuns(ctx context.Context, conn *sql.DB, sourceID int64) ([]adminRunRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, status, started_at, completed_at, discovered_count, crawled_count, eligible_count, rejected_count, failure_count, error
		FROM pipeline_runs
		WHERE topic_source_id = ?
		ORDER BY id DESC
	`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query admin source runs: %w", err)
	}
	defer rows.Close()
	return scanAdminRuns(rows)
}

func adminGetRun(ctx context.Context, conn *sql.DB, runID int64) (adminRunDetail, error) {
	var detail adminRunDetail
	var completed sql.NullString
	var sourceID sql.NullInt64
	err := conn.QueryRowContext(ctx, `
		SELECT
			pr.id,
			pr.documentation_submission_id,
			pr.topic_source_id,
			COALESCE(t.slug, ''),
			COALESCE(t.name, ''),
			COALESCE(ts.normalized_url, ds.normalized_url),
			pr.status,
			pr.started_at,
			pr.completed_at,
			pr.discovered_count,
			pr.crawled_count,
			pr.eligible_count,
			pr.rejected_count,
			pr.failure_count,
			pr.error
		FROM pipeline_runs pr
		JOIN documentation_submissions ds ON ds.id = pr.documentation_submission_id
		LEFT JOIN topic_sources ts ON ts.id = pr.topic_source_id
		LEFT JOIN topics t ON t.id = ts.topic_id
		WHERE pr.id = ?
	`, runID).Scan(
		&detail.ID,
		&detail.SubmissionID,
		&sourceID,
		&detail.TopicSlug,
		&detail.TopicName,
		&detail.SourceURL,
		&detail.Status,
		&detail.StartedAt,
		&completed,
		&detail.DiscoveredCount,
		&detail.CrawledCount,
		&detail.EligibleCount,
		&detail.RejectedCount,
		&detail.FailureCount,
		&detail.Error,
	)
	if err != nil {
		return adminRunDetail{}, err
	}
	if sourceID.Valid {
		detail.SourceID = sourceID.Int64
	}
	detail.CompletedAt = "-"
	if completed.Valid && completed.String != "" {
		detail.CompletedAt = completed.String
	}
	candidates, err := adminListRunCandidates(ctx, conn, runID)
	if err != nil {
		return adminRunDetail{}, err
	}
	detail.Candidates = candidates
	return detail, nil
}

func adminListRuns(ctx context.Context, conn *sql.DB, submissionID int64) ([]adminRunRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, status, started_at, completed_at, discovered_count, crawled_count, eligible_count, rejected_count, failure_count, error
		FROM pipeline_runs
		WHERE documentation_submission_id = ?
		ORDER BY id DESC
	`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("query admin runs: %w", err)
	}
	defer rows.Close()
	return scanAdminRuns(rows)
}

func scanAdminRuns(rows *sql.Rows) ([]adminRunRow, error) {
	var runs []adminRunRow
	for rows.Next() {
		var run adminRunRow
		var completed sql.NullString
		if err := rows.Scan(&run.ID, &run.Status, &run.StartedAt, &completed, &run.DiscoveredCount, &run.CrawledCount, &run.EligibleCount, &run.RejectedCount, &run.FailureCount, &run.Error); err != nil {
			return nil, fmt.Errorf("scan admin run: %w", err)
		}
		run.CompletedAt = "-"
		if completed.Valid && completed.String != "" {
			run.CompletedAt = completed.String
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin runs: %w", err)
	}
	return runs, nil
}

func adminListCandidates(ctx context.Context, conn *sql.DB, submissionID int64) ([]adminCandidateRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, title, url, primary_classification, score, gate_score, gate_page_type, reject_stage, status, estimated_minutes,
			CASE WHEN status = 'rejected' THEN reject_reason ELSE reason END,
			gate_input_tokens,
			gate_output_tokens,
			gate_reasoning_tokens,
			gate_total_tokens,
			enrichment_total_tokens,
			review_model,
			review_confidence,
			review_rationale
		FROM page_candidates
		WHERE documentation_submission_id = ?
		ORDER BY score DESC, title ASC
	`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("query admin candidates: %w", err)
	}
	defer rows.Close()
	return scanAdminCandidates(rows)
}

func adminListSourceCandidates(ctx context.Context, conn *sql.DB, sourceID int64) ([]adminCandidateRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, title, url, primary_classification, score, gate_score, gate_page_type, reject_stage, status, estimated_minutes,
			CASE WHEN status = 'rejected' THEN reject_reason ELSE reason END,
			gate_input_tokens,
			gate_output_tokens,
			gate_reasoning_tokens,
			gate_total_tokens,
			enrichment_total_tokens,
			review_model,
			review_confidence,
			review_rationale
		FROM page_candidates
		WHERE topic_source_id = ?
		ORDER BY score DESC, title ASC
	`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query admin source candidates: %w", err)
	}
	defer rows.Close()
	return scanAdminCandidates(rows)
}

func adminListRunCandidates(ctx context.Context, conn *sql.DB, runID int64) ([]adminCandidateRow, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, title, url, primary_classification, score, gate_score, gate_page_type, reject_stage, status, estimated_minutes,
			CASE WHEN status = 'rejected' THEN reject_reason ELSE reason END,
			gate_input_tokens,
			gate_output_tokens,
			gate_reasoning_tokens,
			gate_total_tokens,
			enrichment_total_tokens,
			review_model,
			review_confidence,
			review_rationale
		FROM page_candidates
		WHERE pipeline_run_id = ?
		ORDER BY status ASC, score DESC, title ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query admin run candidates: %w", err)
	}
	defer rows.Close()
	return scanAdminCandidates(rows)
}

func scanAdminCandidates(rows *sql.Rows) ([]adminCandidateRow, error) {
	var candidates []adminCandidateRow
	for rows.Next() {
		var cand adminCandidateRow
		var estimated sql.NullInt64
		var gateScore sql.NullInt64
		var gateReason string
		var reviewConfidence float64
		var gateInput int
		var gateOutput int
		var gateReasoning int
		var gateTotal int
		var enrichmentTotal int
		if err := rows.Scan(&cand.ID, &cand.Title, &cand.URL, &cand.Classification, &cand.Score, &gateScore, &gateReason, &cand.RejectStage, &cand.Status, &estimated, &cand.Reason, &gateInput, &gateOutput, &gateReasoning, &gateTotal, &enrichmentTotal, &cand.ReviewModel, &reviewConfidence, &cand.Rationale); err != nil {
			return nil, fmt.Errorf("scan admin candidate: %w", err)
		}
		cand.Gate = "-"
		if gateScore.Valid {
			cand.Gate = strconv.FormatInt(gateScore.Int64, 10)
		}
		if gateReason != "" {
			cand.Gate += "/" + gateReason
		}
		if cand.RejectStage == "" {
			cand.RejectStage = "-"
		}
		cand.EstimatedMinutes = "-"
		if estimated.Valid {
			cand.EstimatedMinutes = strconv.FormatInt(estimated.Int64, 10)
		}
		cand.TokenSummary = fmt.Sprintf("gate %d/%d/%d/%d enrich %d", gateInput, gateOutput, gateReasoning, gateTotal, enrichmentTotal)
		cand.Confidence = fmt.Sprintf("%.2f", reviewConfidence)
		if cand.ReviewModel == "" {
			cand.ReviewModel = "-"
		}
		if cand.Rationale == "" {
			cand.Rationale = "-"
		}
		candidates = append(candidates, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin candidates: %w", err)
	}
	return candidates, nil
}

func redirectAdminSubmission(w http.ResponseWriter, r *http.Request, submissionID int64, notice string, errorMessage string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if errorMessage != "" {
		values.Set("error", errorMessage)
	}
	target := fmt.Sprintf("/admin/submissions/%d", submissionID)
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func redirectAdminSources(w http.ResponseWriter, r *http.Request, notice string, errorMessage string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if errorMessage != "" {
		values.Set("error", errorMessage)
	}
	target := "/admin/sources"
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func redirectAdminSource(w http.ResponseWriter, r *http.Request, sourceID int64, notice string, errorMessage string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if errorMessage != "" {
		values.Set("error", errorMessage)
	}
	target := fmt.Sprintf("/admin/sources/%d", sourceID)
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func setAdminSession(w http.ResponseWriter, token string) {
	issuedAt := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	signature := signAdminValue(issuedAt, token)
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    issuedAt + "." + signature,
		Path:     "/admin",
		MaxAge:   int(adminSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func validAdminSession(r *http.Request, token string) bool {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return false
	}
	issuedAt, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok || issuedAt == "" || signature == "" {
		return false
	}
	expected := signAdminValue(issuedAt, token)
	if !constantTimeEqual(signature, expected) {
		return false
	}
	issuedUnix, err := strconv.ParseInt(issuedAt, 10, 64)
	if err != nil {
		return false
	}
	issued := time.Unix(issuedUnix, 0)
	return time.Since(issued) >= 0 && time.Since(issued) <= adminSessionTTL
}

func csrfToken(r *http.Request, token string) string {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return ""
	}
	return signAdminValue("csrf:"+cookie.Value, token)
}

func validCSRF(r *http.Request, token string) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	return constantTimeEqual(r.Form.Get("csrf"), csrfToken(r, token))
}

func signAdminValue(value string, token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func constantTimeEqual(left string, right string) bool {
	return hmac.Equal([]byte(left), []byte(right))
}
