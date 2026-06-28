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
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ernestns/daily-docs/internal/activation"
	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/inspect"
	"github.com/ernestns/daily-docs/internal/pipeline"
	"github.com/ernestns/daily-docs/internal/queue"
	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/seed"
	"github.com/ernestns/daily-docs/internal/submission"
	"github.com/ernestns/daily-docs/internal/topicsource"
	"github.com/ernestns/daily-docs/internal/validator"
)

var topicPathPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type app struct {
	db  *sql.DB
	now func() time.Time
}

func main() {
	ctx := context.Background()

	if len(os.Args) > 1 {
		if err := runCommand(ctx, os.Args[1:]); err != nil {
			log.Printf("command failed: %v", err)
			os.Exit(1)
		}
		return
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dbPath := os.Getenv("DB_PATH")
	conn, err := db.Open(ctx, dbPath)
	if err != nil {
		log.Printf("database startup failed: %v", err)
		os.Exit(1)
	}
	defer conn.Close()

	app := app{
		db:  conn,
		now: func() time.Time { return time.Now().UTC() },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/admin", app.adminHandler)
	mux.HandleFunc("/admin/", app.adminHandler)
	mux.HandleFunc("/submissions", app.submissionsHandler)
	mux.HandleFunc("/topics", app.topicsHandler)
	mux.HandleFunc("/topics/search", app.searchTopicsHandler)
	mux.HandleFunc("/read", app.generateReadingHandler)
	mux.HandleFunc("/", app.routeHandler)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Printf("starting DailyDocs web server addr=%s", addr)
		errs <- server.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errs:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server failed: %v", err)
			os.Exit(1)
		}
	case sig := <-shutdown:
		log.Printf("shutdown signal received signal=%s", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("server shutdown failed: %v", err)
			os.Exit(1)
		}
	}
}

func (a app) routeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/" {
		a.homeHandler(w, r)
		return
	}

	topic, date, ok := parseReadingPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if date == "" {
		date = a.now().UTC().Format("2006-01-02")
	}

	dailyReading, err := reading.GetDailyReading(r.Context(), a.db, topic, date)
	if err != nil {
		switch {
		case errors.Is(err, reading.ErrTopicNotFound):
			http.NotFound(w, r)
		case errors.Is(err, reading.ErrNoActivePages):
			http.Error(w, "topic has no active pages", http.StatusNotFound)
		case errors.Is(err, reading.ErrInvalidDate):
			http.Error(w, "invalid date", http.StatusBadRequest)
		default:
			log.Printf("reading page failed: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}

	renderTemplate(w, readingTemplate, dailyReading)
}

func (a app) homeHandler(w http.ResponseWriter, r *http.Request) {
	topics, err := listTopics(r.Context(), a.db, "", 10)
	if err != nil {
		log.Printf("list topics failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, homeTemplate, struct {
		Topics []topicOption
	}{Topics: topics})
}

func (a app) topicsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	topics, err := listTopics(r.Context(), a.db, "", 0)
	if err != nil {
		log.Printf("list all topics failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, topicsTemplate, struct {
		Topics []topicOption
	}{Topics: topics})
}

func (a app) submissionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.submissionsPageHandler(w, r, "")
	case http.MethodPost:
		a.createSubmissionHandler(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a app) submissionsPageHandler(w http.ResponseWriter, r *http.Request, message string) {
	submissions, err := submission.ListPublic(r.Context(), a.db, 50)
	if err != nil {
		log.Printf("list submissions failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, submissionsTemplate, struct {
		Message      string
		PrefillTopic string
		Submissions  []submission.Submission
	}{
		Message:      message,
		PrefillTopic: strings.TrimSpace(r.URL.Query().Get("topic")),
		Submissions:  submissions,
	})
}

func (a app) createSubmissionHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(r.Form.Get("website")) != "" {
		http.Redirect(w, r, "/submissions", http.StatusSeeOther)
		return
	}

	_, err := submission.Create(r.Context(), a.db, submission.CreateInput{
		URL:            r.Form.Get("url"),
		SuggestedTopic: r.Form.Get("topic"),
		SubmitterIP:    clientIP(r),
		IPHashSalt:     os.Getenv("IP_HASH_SALT"),
	})
	if err != nil {
		if errors.Is(err, submission.ErrInvalidURL) {
			http.Error(w, "documentation URL must use http or https", http.StatusBadRequest)
			return
		}
		log.Printf("create submission failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/submissions", http.StatusSeeOther)
}

func (a app) generateReadingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	topic := strings.TrimSpace(r.URL.Query().Get("topic"))
	if topic == "" {
		http.Error(w, "invalid topic", http.StatusBadRequest)
		return
	}

	match, ok, err := findTopic(r.Context(), a.db, topic)
	if err != nil {
		log.Printf("find topic failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Redirect(w, r, "/submissions?topic="+url.QueryEscape(topic), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/"+match.Slug, http.StatusSeeOther)
}

func (a app) searchTopicsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	topics, err := listTopics(r.Context(), a.db, r.URL.Query().Get("q"), 10)
	if err != nil {
		log.Printf("search topics failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(topics); err != nil {
		log.Printf("encode topic search failed: %v", err)
	}
}

func parseReadingPath(path string) (topic string, date string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 1 && len(parts) != 2 {
		return "", "", false
	}
	if !topicPathPattern.MatchString(parts[0]) {
		return "", "", false
	}
	if len(parts) == 2 {
		if _, err := time.Parse("2006-01-02", parts[1]); err != nil {
			return parts[0], parts[1], true
		}
		return parts[0], parts[1], true
	}
	return parts[0], "", true
}

type topicOption struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func listTopics(ctx context.Context, conn *sql.DB, query string, limit int) ([]topicOption, error) {
	query = strings.TrimSpace(strings.ToLower(query))

	sqlQuery := `
		SELECT slug, name
		FROM topics
		WHERE status = 'active'
	`
	args := []any{}
	if query != "" {
		sqlQuery += " AND (slug LIKE ? OR lower(name) LIKE ?)"
		like := "%" + query + "%"
		args = append(args, like, like)
	}
	sqlQuery += " ORDER BY name ASC"
	if limit > 0 {
		sqlQuery += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := conn.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query topics: %w", err)
	}
	defer rows.Close()

	var topics []topicOption
	for rows.Next() {
		var topic topicOption
		if err := rows.Scan(&topic.Slug, &topic.Name); err != nil {
			return nil, fmt.Errorf("scan topic: %w", err)
		}
		topics = append(topics, topic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate topics: %w", err)
	}
	return topics, nil
}

func findTopic(ctx context.Context, conn *sql.DB, value string) (topicOption, bool, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return topicOption{}, false, nil
	}

	var topic topicOption
	err := conn.QueryRowContext(ctx, `
		SELECT slug, name
		FROM topics
		WHERE status = 'active'
			AND (slug = ? OR lower(name) = ?)
		ORDER BY CASE WHEN slug = ? THEN 0 ELSE 1 END, name ASC
		LIMIT 1
	`, value, value, value).Scan(&topic.Slug, &topic.Name)
	if err == nil {
		return topic, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return topicOption{}, false, nil
	}
	return topicOption{}, false, fmt.Errorf("query topic: %w", err)
}

func renderTemplate(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("render template failed: %v", err)
	}
}

func clientIP(r *http.Request) string {
	if forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwardedFor != "" {
		ip, _, _ := strings.Cut(forwardedFor, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return strings.TrimSpace(host)
}

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
	ID              int64
	TopicSlug       string
	Status          string
	SourceType      string
	NormalizedURL   string
	LastProcessedAt string
	LastError       string
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
	case path == "/admin/submissions":
		a.adminSubmissionsHandler(w, r, token)
	case strings.HasPrefix(path, "/admin/submissions/"):
		a.adminSubmissionHandler(w, r, token, path)
	default:
		http.NotFound(w, r)
	}
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
			t.slug,
			ts.status,
			ts.source_type,
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
		if err := rows.Scan(&source.ID, &source.TopicSlug, &source.Status, &source.SourceType, &source.NormalizedURL, &source.LastProcessedAt, &source.LastError); err != nil {
			return nil, fmt.Errorf("scan admin source: %w", err)
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
		return nil, fmt.Errorf("iterate admin sources: %w", err)
	}
	return sources, nil
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
			CASE WHEN status = 'rejected' THEN reject_reason ELSE reason END
		FROM page_candidates
		WHERE documentation_submission_id = ?
		ORDER BY score DESC, title ASC
	`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("query admin candidates: %w", err)
	}
	defer rows.Close()

	var candidates []adminCandidateRow
	for rows.Next() {
		var cand adminCandidateRow
		var estimated sql.NullInt64
		var gateScore sql.NullInt64
		var gateReason string
		if err := rows.Scan(&cand.ID, &cand.Title, &cand.URL, &cand.Classification, &cand.Score, &gateScore, &gateReason, &cand.RejectStage, &cand.Status, &estimated, &cand.Reason); err != nil {
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

var homeTemplate = template.Must(template.New("home").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>DailyDocs</title>
  <style>
    body {
      margin: 0;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #1f2933;
      background: #f7f8fa;
    }
    main {
      min-height: 100vh;
      display: grid;
      place-items: center;
      padding: 2rem;
      box-sizing: border-box;
    }
    section {
      width: min(42rem, 100%);
    }
    h1 {
      margin: 0 0 0.75rem;
      font-size: clamp(2.5rem, 8vw, 5rem);
      line-height: 1;
    }
    p {
      margin: 0 0 1.5rem;
      max-width: 34rem;
      color: #52606d;
      font-size: 1.125rem;
      line-height: 1.6;
    }
    form {
      display: flex;
      gap: 0.75rem;
      align-items: center;
      max-width: 32rem;
      margin-bottom: 0.75rem;
    }
    .topic-lookup {
      position: relative;
      flex: 1;
      min-width: 0;
    }
    input {
      width: 100%;
      box-sizing: border-box;
      min-width: 0;
      padding: 0.75rem 0.875rem;
      border: 1px solid #cbd2d9;
      border-radius: 6px;
      font: inherit;
      background: #ffffff;
    }
    button {
      padding: 0.75rem 1rem;
      border: 0;
      border-radius: 6px;
      font: inherit;
      color: #ffffff;
      background: #1f2933;
      cursor: pointer;
    }
    ul {
      margin: 1.25rem 0 0;
      padding: 0;
      list-style: none;
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
    }
    a {
      color: #1f2933;
    }
    .lookup-status {
      margin: 0 0 1rem;
      color: #52606d;
      font-size: 0.95rem;
    }
    .topic-results {
      position: absolute;
      z-index: 2;
      top: calc(100% + 0.25rem);
      right: 0;
      left: 0;
      display: grid;
      gap: 0;
      margin: 0;
      padding: 0.25rem;
      list-style: none;
      border: 1px solid #cbd2d9;
      border-radius: 6px;
      background: #ffffff;
      box-shadow: 0 8px 24px rgba(31, 41, 51, 0.12);
    }
    .topic-results[hidden] {
      display: none;
    }
    .topic-option {
      padding: 0.65rem 0.75rem;
      border-radius: 4px;
      cursor: pointer;
    }
    .topic-option[aria-selected="true"] {
      color: #ffffff;
      background: #1f2933;
    }
  </style>
</head>
<body>
  <main>
    <section>
      <h1>DailyDocs</h1>
      <p>One documentation link per topic per day.</p>
      <form method="get" action="/read" id="topic-form">
        <div class="topic-lookup">
          <input
            name="topic"
            id="topic-input"
            autocomplete="off"
            placeholder="sqlite"
            aria-label="Topic"
            role="combobox"
            aria-autocomplete="list"
            aria-expanded="false"
            aria-controls="topic-results"
          >
          <ul class="topic-results" id="topic-results" role="listbox" hidden></ul>
        </div>
        <button type="submit" id="topic-button">View Reading</button>
      </form>
      <p class="lookup-status" id="topic-status"></p>
      {{if .Topics}}
      <ul>
        {{range .Topics}}<li><a href="/{{.Slug}}">{{.Name}}</a></li>{{end}}
      </ul>
      {{end}}
      <p><a href="/topics">All topics</a></p>
    </section>
  </main>
  <script>
    const input = document.getElementById("topic-input");
    const button = document.getElementById("topic-button");
    const status = document.getElementById("topic-status");
    const results = document.getElementById("topic-results");
    let controller = null;
    let matches = [];
    let activeIndex = -1;

    function exactMatch() {
      const value = input.value.trim().toLowerCase();
      return matches.find((topic) => topic.slug.toLowerCase() === value || topic.name.toLowerCase() === value);
    }

    function closeResults() {
      results.hidden = true;
      input.setAttribute("aria-expanded", "false");
      input.removeAttribute("aria-activedescendant");
      activeIndex = -1;
      updateActiveOption();
    }

    function openResults() {
      if (matches.length === 0) return;
      results.hidden = false;
      input.setAttribute("aria-expanded", "true");
    }

    function updateActiveOption() {
      Array.from(results.children).forEach((option, index) => {
        const selected = index === activeIndex;
        option.setAttribute("aria-selected", selected ? "true" : "false");
        if (selected) {
          input.setAttribute("aria-activedescendant", option.id);
        }
      });
      if (activeIndex < 0) {
        input.removeAttribute("aria-activedescendant");
      }
    }

    function selectMatch(index) {
      const topic = matches[index];
      if (!topic) return;
      input.value = topic.name;
      matches = [topic];
      closeResults();
      setLookupState();
    }

    function renderResults() {
      results.innerHTML = "";
      matches.forEach((topic, index) => {
        const option = document.createElement("li");
        option.id = "topic-option-" + index;
        option.className = "topic-option";
        option.setAttribute("role", "option");
        option.setAttribute("aria-selected", "false");
        option.textContent = topic.name;
        option.addEventListener("mousedown", (event) => {
          event.preventDefault();
          selectMatch(index);
        });
        results.appendChild(option);
      });
      activeIndex = -1;
      updateActiveOption();
      if (matches.length > 0) openResults();
      else closeResults();
    }

    function setLookupState() {
      const value = input.value.trim().toLowerCase();
      if (!value) {
        button.textContent = "View Reading";
        status.textContent = "";
        return;
      }

      if (exactMatch()) {
        button.textContent = "View Reading";
        status.textContent = "";
        return;
      }

      button.textContent = "Submit Documentation";
      status.textContent = matches.length > 0 ? "Select a topic from the list." : "No matching topic found.";
    }

    input.addEventListener("input", async () => {
      const value = input.value.trim();
      if (controller) controller.abort();
      if (!value) {
        matches = [];
        renderResults();
        setLookupState();
        return;
      }

      controller = new AbortController();
      try {
        const response = await fetch("/topics/search?q=" + encodeURIComponent(value), { signal: controller.signal });
        if (!response.ok) return;
        matches = await response.json();
        renderResults();
        setLookupState();
      } catch (error) {
        if (error.name !== "AbortError") status.textContent = "";
      }
    });

    input.addEventListener("keydown", (event) => {
      if (event.key === "ArrowDown") {
        event.preventDefault();
        if (results.hidden) openResults();
        activeIndex = Math.min(activeIndex + 1, matches.length - 1);
        updateActiveOption();
      } else if (event.key === "ArrowUp") {
        event.preventDefault();
        activeIndex = Math.max(activeIndex - 1, 0);
        updateActiveOption();
      } else if (event.key === "Enter" && activeIndex >= 0 && !results.hidden) {
        event.preventDefault();
        selectMatch(activeIndex);
      } else if (event.key === "Escape") {
        closeResults();
      }
    });

    input.addEventListener("blur", () => {
      window.setTimeout(closeResults, 100);
    });
  </script>
</body>
</html>
`))

var topicsTemplate = template.Must(template.New("topics").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Topics - DailyDocs</title>
  <style>
    body {
      margin: 0;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #1f2933;
      background: #f7f8fa;
    }
    main {
      width: min(42rem, 100%);
      margin: 0 auto;
      padding: 2rem;
      box-sizing: border-box;
    }
    h1 {
      margin: 0 0 1rem;
      font-size: clamp(2rem, 7vw, 4rem);
      line-height: 1;
    }
    ul {
      margin: 0 0 1.5rem;
      padding: 0;
      list-style: none;
      display: grid;
      gap: 0.65rem;
    }
    a {
      color: #1f2933;
    }
    p {
      margin: 0;
      color: #52606d;
      line-height: 1.6;
    }
  </style>
</head>
<body>
  <main>
    <h1>Topics</h1>
    {{if .Topics}}
    <ul>
      {{range .Topics}}<li><a href="/{{.Slug}}">{{.Name}}</a></li>{{end}}
    </ul>
    {{else}}
    <p>No topics yet.</p>
    {{end}}
    <p><a href="/">Home</a></p>
  </main>
</body>
</html>
`))

var submissionsTemplate = template.Must(template.New("submissions").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <title>Submissions - DailyDocs</title>
  <style>
    body {
      margin: 0;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #1f2933;
      background: #f7f8fa;
    }
    main {
      width: min(48rem, 100%);
      margin: 0 auto;
      padding: 2rem;
      box-sizing: border-box;
    }
    h1 {
      margin: 0 0 0.75rem;
      font-size: clamp(2rem, 7vw, 4rem);
      line-height: 1;
    }
    p {
      margin: 0 0 1.5rem;
      color: #52606d;
      font-size: 1rem;
      line-height: 1.6;
    }
    form {
      display: grid;
      gap: 0.75rem;
      margin: 0 0 2rem;
      max-width: 36rem;
    }
    label {
      display: grid;
      gap: 0.35rem;
      color: #52606d;
      font-size: 0.95rem;
    }
    input {
      min-width: 0;
      padding: 0.75rem 0.875rem;
      border: 1px solid #cbd2d9;
      border-radius: 6px;
      font: inherit;
      background: #ffffff;
      color: #1f2933;
    }
    .honeypot {
      position: absolute;
      left: -10000px;
      width: 1px;
      height: 1px;
      overflow: hidden;
    }
    button {
      justify-self: start;
      padding: 0.75rem 1rem;
      border: 0;
      border-radius: 6px;
      font: inherit;
      color: #ffffff;
      background: #1f2933;
      cursor: pointer;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      background: #ffffff;
    }
    th, td {
      padding: 0.75rem;
      border-bottom: 1px solid #e4e7eb;
      text-align: left;
      vertical-align: top;
    }
    th {
      color: #52606d;
      font-size: 0.875rem;
      font-weight: 600;
    }
    a {
      color: #1f2933;
    }
  </style>
</head>
<body>
  <main>
    <h1>Documentation submissions</h1>
    <p>Submit a documentation source URL for a new or existing topic.</p>
    <form method="post" action="/submissions">
      <label>
        Documentation URL
        <input name="url" type="url" autocomplete="off" placeholder="https://sqlite.org/docs.html" required>
      </label>
      <label>
        Topic
        <input name="topic" autocomplete="off" placeholder="SQLite" value="{{.PrefillTopic}}">
      </label>
      <label class="honeypot">
        Website
        <input name="website" autocomplete="off" tabindex="-1">
      </label>
      <button type="submit">Submit</button>
    </form>

    {{if .Submissions}}
    <table>
      <thead>
        <tr>
          <th>Source</th>
          <th>Topic</th>
          <th>Status</th>
          <th>Requests</th>
          <th>Last submitted</th>
        </tr>
      </thead>
      <tbody>
        {{range .Submissions}}
        <tr>
          <td>{{.SourceHost}}</td>
          <td>{{if .SuggestedTopic}}{{.SuggestedTopic}}{{else}}-{{end}}</td>
          <td>{{.Status}}</td>
          <td>{{.RequestCount}}</td>
          <td>{{.LastSubmitted}}</td>
        </tr>
        {{end}}
      </tbody>
    </table>
    {{else}}
    <p>No submissions yet.</p>
    {{end}}
    <p><a href="/">All topics</a></p>
  </main>
</body>
</html>
`))

var readingTemplate = template.Must(template.New("reading").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.TopicName}} - DailyDocs</title>
  <style>
    body {
      margin: 0;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #1f2933;
      background: #f7f8fa;
    }
    main {
      min-height: 100vh;
      display: grid;
      place-items: center;
      padding: 2rem;
      box-sizing: border-box;
    }
    article {
      width: min(42rem, 100%);
    }
    .date {
      margin: 0 0 0.5rem;
      color: #52606d;
      font-size: 0.95rem;
    }
    h1 {
      margin: 0 0 0.5rem;
      font-size: clamp(2.25rem, 7vw, 4.5rem);
      line-height: 1;
    }
    h2 {
      margin: 0 0 1rem;
      font-size: clamp(1.5rem, 4vw, 2.25rem);
      line-height: 1.15;
    }
    p {
      margin: 0 0 1.5rem;
      color: #52606d;
      font-size: 1.05rem;
      line-height: 1.6;
    }
    .badge {
      display: inline-block;
      margin-left: 0.5rem;
      font-size: 0.85rem;
      color: #1f2933;
    }
    a.button {
      display: inline-block;
      padding: 0.75rem 1rem;
      border-radius: 6px;
      color: #ffffff;
      background: #1f2933;
      text-decoration: none;
    }
    nav {
      margin-top: 1.5rem;
    }
    nav a {
      color: #52606d;
    }
  </style>
</head>
<body>
  <main>
    <article>
      <p class="date">{{.Date}}</p>
      <h1>{{.TopicName}}</h1>
      <h2>{{.Title}}</h2>
      <p>
        {{if .Source}}{{.Source}}{{else}}Documentation{{end}}
        {{if .Official}}<span class="badge">Official</span>{{end}}
        {{if .EstimatedMinutes}}<br>{{.EstimatedMinutes}} min{{end}}
      </p>
      <a class="button" href="{{.URL}}">Read</a>
      <nav><a href="/topics">All topics</a></nav>
      <nav><a href="/submissions?topic={{.TopicName}}">Suggest documentation source</a></nav>
    </article>
  </main>
</body>
</html>
`))

var adminLoginTemplate = template.Must(template.New("admin-login").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <title>Admin - DailyDocs</title>
  <style>
    body { margin: 0; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: #1f2933; background: #f7f8fa; }
    main { width: min(28rem, 100%); margin: 0 auto; padding: 2rem; box-sizing: border-box; }
    h1 { margin: 0 0 1rem; font-size: 2rem; }
    form { display: grid; gap: 0.75rem; }
    input { padding: 0.75rem 0.875rem; border: 1px solid #cbd2d9; border-radius: 6px; font: inherit; }
    button { justify-self: start; padding: 0.75rem 1rem; border: 0; border-radius: 6px; font: inherit; color: #fff; background: #1f2933; cursor: pointer; }
    .error { color: #b42318; }
  </style>
</head>
<body>
  <main>
    <h1>Admin</h1>
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
    <form method="post" action="/admin/login">
      <label>
        Admin token
        <input name="token" type="password" autocomplete="current-password" autofocus>
      </label>
      <button type="submit">Sign in</button>
    </form>
  </main>
</body>
</html>
`))

var adminSubmissionsTemplate = template.Must(template.New("admin-submissions").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <title>Admin Submissions - DailyDocs</title>
  <style>
    body { margin: 0; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: #1f2933; background: #f7f8fa; }
    main { width: min(68rem, 100%); margin: 0 auto; padding: 2rem; box-sizing: border-box; }
    h1 { margin: 0 0 1rem; font-size: 2rem; }
    table { width: 100%; border-collapse: collapse; background: #fff; }
    th, td { padding: 0.75rem; border-bottom: 1px solid #e4e7eb; text-align: left; vertical-align: top; }
    th { color: #52606d; font-size: 0.875rem; }
    a { color: #1f2933; }
    tr[data-href] { cursor: pointer; }
    tr[data-href]:hover { background: #f1f5f9; }
    tr[data-href]:focus { outline: 2px solid #1f2933; outline-offset: -2px; }
    .notice { color: #067647; }
    .error { color: #b42318; }
  </style>
</head>
<body>
  <main>
    <h1>Submissions</h1>
    {{if .Notice}}<p class="notice">{{.Notice}}</p>{{end}}
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
    {{if .Submissions}}
    <table>
      <thead>
        <tr>
          <th>ID</th>
          <th>Topic</th>
          <th>Source</th>
          <th>Status</th>
          <th>Requests</th>
          <th>Last submitted</th>
          <th>Error</th>
        </tr>
      </thead>
      <tbody>
        {{range .Submissions}}
        <tr data-href="/admin/submissions/{{.ID}}" tabindex="0">
          <td><a href="/admin/submissions/{{.ID}}">{{.ID}}</a></td>
          <td>{{if .SuggestedTopic}}{{.SuggestedTopic}}{{else}}-{{end}}</td>
          <td>{{.SourceHost}}</td>
          <td>{{.Status}}</td>
          <td>{{.RequestCount}}</td>
          <td>{{.LastSubmitted}}</td>
          <td>{{if .LastError}}{{.LastError}}{{else}}-{{end}}</td>
        </tr>
        {{end}}
      </tbody>
    </table>
    {{else}}
    <p>No submissions.</p>
    {{end}}
  </main>
  <script>
    document.querySelectorAll("tr[data-href]").forEach((row) => {
      row.addEventListener("click", (event) => {
        if (event.target.closest("a, button")) return;
        window.location.href = row.dataset.href;
      });
      row.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          window.location.href = row.dataset.href;
        }
      });
    });
  </script>
</body>
</html>
`))

var adminSubmissionDetailTemplate = template.Must(template.New("admin-submission-detail").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <title>Submission {{.Submission.ID}} - DailyDocs</title>
  <style>
    body { margin: 0; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; color: #1f2933; background: #f7f8fa; }
    main { width: min(76rem, 100%); margin: 0 auto; padding: 2rem; box-sizing: border-box; }
    h1, h2 { margin: 0 0 1rem; }
    section { margin-top: 2rem; }
    dl { display: grid; grid-template-columns: 12rem 1fr; gap: 0.5rem 1rem; }
    dt { color: #52606d; }
    dd { margin: 0; }
    table { width: 100%; border-collapse: collapse; background: #fff; }
    th, td { padding: 0.75rem; border-bottom: 1px solid #e4e7eb; text-align: left; vertical-align: top; }
    th { color: #52606d; font-size: 0.875rem; }
    form { display: inline; margin-right: 0.5rem; }
    .source-form { display: grid; gap: 0.75rem; max-width: 32rem; margin: 0 0 1rem; }
    .source-form label { display: grid; gap: 0.35rem; color: #52606d; }
    input { min-width: 0; padding: 0.6rem 0.75rem; border: 1px solid #cbd2d9; border-radius: 6px; font: inherit; color: #1f2933; background: #fff; }
    button { padding: 0.6rem 0.85rem; border: 0; border-radius: 6px; font: inherit; color: #fff; background: #1f2933; cursor: pointer; }
    a { color: #1f2933; }
    .notice { color: #067647; }
    .error { color: #b42318; }
    .url { overflow-wrap: anywhere; }
  </style>
</head>
<body>
  <main>
    <p><a href="/admin/submissions">Submissions</a></p>
    <h1>Submission {{.Submission.ID}}</h1>
    {{if .Notice}}<p class="notice">{{.Notice}}</p>{{end}}
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}

    <form method="post" action="/admin/submissions/{{.Submission.ID}}/process">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <button type="submit">Process</button>
    </form>
    <form method="post" action="/admin/submissions/{{.Submission.ID}}/activate">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <button type="submit">Activate Candidates</button>
    </form>

    <section>
      <h2>Create Source</h2>
      <form class="source-form" method="post" action="/admin/submissions/{{.Submission.ID}}/create-source">
        <input type="hidden" name="csrf" value="{{.CSRF}}">
        <label>
          Topic slug
          <input name="topic_slug" value="{{.Submission.SuggestedSlug}}" required>
        </label>
        <label>
          Topic name
          <input name="topic_name" value="{{.Submission.SuggestedTopic}}">
        </label>
        <button type="submit">Create Source</button>
      </form>
    </section>

    <section>
      <h2>Details</h2>
      <dl>
        <dt>Topic</dt><dd>{{if .Submission.SuggestedTopic}}{{.Submission.SuggestedTopic}}{{else}}-{{end}}</dd>
        <dt>Source</dt><dd>{{.Submission.SourceHost}}</dd>
        <dt>Status</dt><dd>{{.Submission.Status}}</dd>
        <dt>Requests</dt><dd>{{.Submission.RequestCount}}</dd>
        <dt>Submitted URL</dt><dd class="url">{{.Submission.SubmittedURL}}</dd>
        <dt>Normalized URL</dt><dd class="url">{{.Submission.NormalizedURL}}</dd>
        <dt>Last submitted</dt><dd>{{.Submission.LastSubmitted}}</dd>
        <dt>Last error</dt><dd>{{if .Submission.LastError}}{{.Submission.LastError}}{{else}}-{{end}}</dd>
      </dl>
    </section>

    <section>
      <h2>Sources</h2>
      {{if .Submission.Sources}}
      <table>
        <thead><tr><th>ID</th><th>Topic</th><th>Status</th><th>Type</th><th>URL</th><th>Last processed</th><th>Error</th><th>Action</th></tr></thead>
        <tbody>
          {{range .Submission.Sources}}
          <tr>
            <td>{{.ID}}</td>
            <td>{{.TopicSlug}}</td>
            <td>{{.Status}}</td>
            <td>{{.SourceType}}</td>
            <td class="url">{{.NormalizedURL}}</td>
            <td>{{.LastProcessedAt}}</td>
            <td>{{.LastError}}</td>
            <td>
              <form method="post" action="/admin/submissions/{{$.Submission.ID}}/process-source">
                <input type="hidden" name="csrf" value="{{$.CSRF}}">
                <input type="hidden" name="source_id" value="{{.ID}}">
                <button type="submit">Process Source</button>
              </form>
            </td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else}}<p>No sources.</p>{{end}}
    </section>

    <section>
      <h2>Runs</h2>
      {{if .Submission.Runs}}
      <table>
        <thead><tr><th>ID</th><th>Status</th><th>Started</th><th>Completed</th><th>Discovered</th><th>Crawled</th><th>Eligible</th><th>Rejected</th><th>Failed</th><th>Error</th></tr></thead>
        <tbody>
          {{range .Submission.Runs}}
          <tr><td>{{.ID}}</td><td>{{.Status}}</td><td>{{.StartedAt}}</td><td>{{.CompletedAt}}</td><td>{{.DiscoveredCount}}</td><td>{{.CrawledCount}}</td><td>{{.EligibleCount}}</td><td>{{.RejectedCount}}</td><td>{{.FailureCount}}</td><td>{{if .Error}}{{.Error}}{{else}}-{{end}}</td></tr>
          {{end}}
        </tbody>
      </table>
      {{else}}<p>No runs.</p>{{end}}
    </section>

    <section>
      <h2>Candidates</h2>
      {{if .Submission.Candidates}}
      <table>
        <thead><tr><th>ID</th><th>Score</th><th>Gate</th><th>Status</th><th>Stage</th><th>Min</th><th>Class</th><th>Title</th><th>URL</th><th>Reason</th></tr></thead>
        <tbody>
          {{range .Submission.Candidates}}
          <tr><td>{{.ID}}</td><td>{{.Score}}</td><td>{{.Gate}}</td><td>{{.Status}}</td><td>{{.RejectStage}}</td><td>{{.EstimatedMinutes}}</td><td>{{.Classification}}</td><td>{{.Title}}</td><td class="url">{{.URL}}</td><td>{{.Reason}}</td></tr>
          {{end}}
        </tbody>
      </table>
      {{else}}<p>No candidates.</p>{{end}}
    </section>
  </main>
</body>
</html>
`))

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "ok")
}

func runCommand(ctx context.Context, args []string) error {
	switch args[0] {
	case "import-file":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs import-file path/to/topic.yaml")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := seed.ImportFile(ctx, conn, args[1])
		if err != nil {
			return err
		}

		log.Printf("imported topic=%s pages_found=%d pages_imported=%d", result.TopicSlug, result.PagesFound, result.PagesImported)
		return nil
	case "validate-links":
		if len(args) != 1 {
			return fmt.Errorf("usage: dailydocs validate-links")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := validator.ValidateLinks(ctx, conn, nil, validator.DefaultFailureThreshold)
		if err != nil {
			return err
		}

		log.Printf("validated links checked=%d healthy=%d failed=%d disabled=%d", result.Checked, result.Healthy, result.Failed, result.Disabled)
		return nil
	case "process-submission":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs process-submission submission-id")
		}
		submissionID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || submissionID < 1 {
			return fmt.Errorf("submission-id must be a positive integer")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := pipeline.ProcessSubmission(ctx, conn, submissionID, pipeline.Options{})
		if err != nil {
			return err
		}

		log.Printf("processed submission id=%d run_id=%d discovered=%d crawled=%d eligible=%d rejected=%d failed=%d", result.SubmissionID, result.PipelineRunID, result.DiscoveredCount, result.CrawledCount, result.EligibleCount, result.RejectedCount, result.FailureCount)
		return nil
	case "create-source-from-submission":
		if len(args) < 3 || len(args) > 4 {
			return fmt.Errorf("usage: dailydocs create-source-from-submission submission-id topic-slug [topic-name]")
		}
		submissionID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || submissionID < 1 {
			return fmt.Errorf("submission-id must be a positive integer")
		}
		topicName := ""
		if len(args) == 4 {
			topicName = args[3]
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		source, err := topicsource.CreateFromSubmission(ctx, conn, topicsource.CreateFromSubmissionInput{
			SubmissionID: submissionID,
			TopicSlug:    args[2],
			TopicName:    topicName,
		})
		if err != nil {
			return err
		}
		log.Printf("created topic source id=%d topic=%s url=%s", source.ID, source.TopicSlug, source.NormalizedURL)
		return nil
	case "list-sources":
		if len(args) != 1 {
			return fmt.Errorf("usage: dailydocs list-sources")
		}
		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()
		return topicsource.WriteList(ctx, conn, os.Stdout)
	case "process-source":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs process-source source-id")
		}
		sourceID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || sourceID < 1 {
			return fmt.Errorf("source-id must be a positive integer")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := pipeline.ProcessSource(ctx, conn, sourceID, pipeline.Options{})
		if err != nil {
			return err
		}

		log.Printf("processed source id=%d submission_id=%d run_id=%d discovered=%d crawled=%d eligible=%d rejected=%d failed=%d", result.TopicSourceID, result.SubmissionID, result.PipelineRunID, result.DiscoveredCount, result.CrawledCount, result.EligibleCount, result.RejectedCount, result.FailureCount)
		return nil
	case "inspect-url":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs inspect-url url")
		}
		result, err := pipeline.InspectURL(ctx, args[1], pipeline.Options{})
		if err != nil {
			return err
		}
		return writePrettyJSON(os.Stdout, result)
	case "discover-url":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs discover-url url")
		}
		result, err := pipeline.DiscoverURL(ctx, args[1], pipeline.Options{})
		if err != nil {
			return err
		}
		return writePrettyJSON(os.Stdout, result)
	case "gate-url":
		showRequest := false
		showResponse := false
		var rawURL string
		for _, arg := range args[1:] {
			switch arg {
			case "--show-request":
				showRequest = true
			case "--show-response":
				showResponse = true
			default:
				if rawURL != "" {
					return fmt.Errorf("usage: dailydocs gate-url [--show-request] [--show-response] url")
				}
				rawURL = arg
			}
		}
		if rawURL == "" {
			return fmt.Errorf("usage: dailydocs gate-url [--show-request] [--show-response] url")
		}
		result, err := pipeline.GateURL(ctx, rawURL, pipeline.Options{}, showResponse)
		if err != nil {
			if showRequest && result.Request != nil {
				_ = writePrettyJSON(os.Stdout, map[string]any{"request": result.Request})
			}
			return err
		}
		output := map[string]any{
			"review": result.Review,
		}
		if showRequest {
			output["request"] = result.Request
		}
		if showResponse {
			var raw any
			if len(result.Response) > 0 && json.Unmarshal(result.Response, &raw) == nil {
				output["response"] = raw
			} else {
				output["response"] = string(result.Response)
			}
		}
		return writePrettyJSON(os.Stdout, output)
	case "activate-candidates":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs activate-candidates submission-id")
		}
		submissionID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || submissionID < 1 {
			return fmt.Errorf("submission-id must be a positive integer")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := activation.ActivateCandidates(ctx, conn, submissionID)
		if err != nil {
			return err
		}

		log.Printf("activated candidates submission_id=%d topic=%s pages=%d", result.SubmissionID, result.TopicSlug, result.Activated)
		return nil
	case "process-pending-submissions":
		limit := 5
		if len(args) > 3 {
			return fmt.Errorf("usage: dailydocs process-pending-submissions [--limit N]")
		}
		if len(args) == 3 {
			if args[1] != "--limit" {
				return fmt.Errorf("usage: dailydocs process-pending-submissions [--limit N]")
			}
			parsedLimit, err := strconv.Atoi(args[2])
			if err != nil || parsedLimit < 1 {
				return fmt.Errorf("limit must be a positive integer")
			}
			limit = parsedLimit
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := queue.ProcessPending(ctx, conn, queue.Options{Limit: limit})
		if err != nil {
			return err
		}

		log.Printf("processed pending submissions claimed=%d processed=%d failed=%d", result.Claimed, result.Processed, result.Failed)
		return nil
	case "list-submissions":
		if len(args) != 1 {
			return fmt.Errorf("usage: dailydocs list-submissions")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		return inspect.WriteSubmissions(ctx, conn, os.Stdout)
	case "show-submission":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs show-submission submission-id")
		}
		submissionID, err := parsePositiveID(args[1], "submission-id")
		if err != nil {
			return err
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		return inspect.WriteSubmission(ctx, conn, os.Stdout, submissionID)
	case "list-runs":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs list-runs submission-id")
		}
		submissionID, err := parsePositiveID(args[1], "submission-id")
		if err != nil {
			return err
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		return inspect.WriteRuns(ctx, conn, os.Stdout, submissionID)
	case "list-candidates":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs list-candidates submission-id")
		}
		submissionID, err := parsePositiveID(args[1], "submission-id")
		if err != nil {
			return err
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		return inspect.WriteCandidates(ctx, conn, os.Stdout, submissionID)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func parsePositiveID(value string, name string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return id, nil
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

func writePrettyJSON(out *os.File, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
