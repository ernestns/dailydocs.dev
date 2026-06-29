package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

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

	createOnMissing := date == ""
	if createOnMissing {
		date = a.now().UTC().Format("2006-01-02")
	}

	var dailyReading reading.Reading
	var err error
	if createOnMissing {
		dailyReading, err = reading.GetOrCreateDailyReading(r.Context(), a.db, topic, date)
	} else {
		dailyReading, err = reading.GetDailyReading(r.Context(), a.db, topic, date)
	}
	if err != nil {
		switch {
		case errors.Is(err, reading.ErrTopicNotFound), errors.Is(err, reading.ErrNoActivePages):
			a.handleMissingTopic(w, r, topic)
		case errors.Is(err, reading.ErrReadingNotFound):
			http.NotFound(w, r)
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

func (a app) handleMissingTopic(w http.ResponseWriter, r *http.Request, topic string) {
	queued, err := a.queueTopic(r.Context(), topic)
	if err != nil {
		log.Printf("queue topic failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	started := false
	if queued.Status == "queued" {
		started = a.processQueuedTopicAsync(queued.Slug)
	}
	updated, err := loadQueuedTopic(r.Context(), a.db, queued.Slug)
	if err != nil {
		log.Printf("load queued topic after starting processor failed: %v", err)
		renderTemplate(w, queuedTopicTemplate, queued)
		return
	}
	if started && updated.Status == "queued" {
		updated = processingTopicView(updated)
	}
	renderTemplate(w, queuedTopicTemplate, updated)
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

	topics, err := listRequestedTopics(r.Context(), a.db)
	if err != nil {
		log.Printf("list all topics failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, topicsTemplate, struct {
		Topics []topicListItem
	}{Topics: topics})
}

func (a app) topicEvaluationsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if slug, ok := parseTopicStatusPath(r.URL.Path); ok {
		a.topicStatusHandler(w, r, slug)
		return
	}

	slug, ok := parseTopicEvaluationsPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	topic, results, err := loadTopicEvaluations(r.Context(), a.db, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("load topic evaluations failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, topicEvaluationsTemplate, struct {
		Topic   topicListItem
		Results []evaluationResult
	}{Topic: topic, Results: results})
}

func (a app) processTopicHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	slug := strings.TrimSpace(r.FormValue("topic"))
	if !topicPathPattern.MatchString(slug) {
		http.Error(w, "invalid topic", http.StatusBadRequest)
		return
	}

	started := a.processQueuedTopicAsync(slug)
	if isDatastarRequest(r) {
		a.renderTopicStatus(w, r, slug, started)
		return
	}
	http.Redirect(w, r, "/"+slug, http.StatusSeeOther)
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
		queued, queueErr := a.queueTopic(r.Context(), topic)
		if queueErr != nil {
			log.Printf("queue topic failed: %v", queueErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if queued.Status == "queued" {
			a.processQueuedTopicAsync(queued.Slug)
		}
		http.Redirect(w, r, "/"+queued.Slug, http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/"+match.Slug, http.StatusSeeOther)
}

func (a app) topicStatusHandler(w http.ResponseWriter, r *http.Request, slug string) {
	a.renderTopicStatus(w, r, slug, false)
}

func (a app) renderTopicStatus(w http.ResponseWriter, r *http.Request, slug string, started bool) {
	queued, err := loadQueuedTopic(r.Context(), a.db, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("load topic status failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if started && (queued.Status == "queued" || queued.Status == "failed") {
		queued = processingTopicView(queued)
	}
	renderTemplateByName(w, topicStatusTemplate, "topic_status", queued)
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

type topicListItem struct {
	Slug           string
	Name           string
	Status         string
	StatusLabel    string
	RunStatus      string
	RunStage       string
	EvaluatedCount int
	AcceptedCount  int
}

type evaluationResult struct {
	Title          string
	URL            string
	Source         string
	Snippet        string
	Rank           int
	ReviewerScore  int
	PageType       string
	ReviewerReason string
	Accepted       bool
}

type queuedTopicView struct {
	Slug         string
	Name         string
	Status       string
	StatusLabel  string
	CanProcess   bool
	IsProcessing bool
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

func listRequestedTopics(ctx context.Context, conn *sql.DB) ([]topicListItem, error) {
	if err := topicsearch.ExpireStaleRunningSearches(ctx, conn, time.Now().UTC()); err != nil {
		return nil, err
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT
			t.slug,
			t.name,
			t.status,
			COALESCE((
				SELECT sr.status
				FROM topic_search_runs sr
				WHERE sr.topic_id = t.id
				ORDER BY sr.started_at DESC, sr.id DESC
				LIMIT 1
			), '') AS run_status,
			COALESCE((
				SELECT sr.stage
				FROM topic_search_runs sr
				WHERE sr.topic_id = t.id
				ORDER BY sr.started_at DESC, sr.id DESC
				LIMIT 1
			), '') AS run_stage,
			COUNT(r.id) AS evaluated_count,
			COALESCE(SUM(r.accepted), 0) AS accepted_count
		FROM topics t
		LEFT JOIN topic_search_results r ON r.topic_id = t.id
		WHERE t.status != 'disabled'
		GROUP BY t.id
		ORDER BY t.updated_at DESC, t.name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query requested topics: %w", err)
	}
	defer rows.Close()

	var topics []topicListItem
	for rows.Next() {
		var topic topicListItem
		if err := rows.Scan(&topic.Slug, &topic.Name, &topic.Status, &topic.RunStatus, &topic.RunStage, &topic.EvaluatedCount, &topic.AcceptedCount); err != nil {
			return nil, fmt.Errorf("scan requested topic: %w", err)
		}
		topic.StatusLabel = topicStatusLabel(topic.Status, topic.RunStatus, topic.RunStage)
		topics = append(topics, topic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate requested topics: %w", err)
	}
	return topics, nil
}

func loadTopicEvaluations(ctx context.Context, conn *sql.DB, slug string) (topicListItem, []evaluationResult, error) {
	var topic topicListItem
	var topicID int64
	if err := conn.QueryRowContext(ctx, `
		SELECT
			t.id,
			t.slug,
			t.name,
			t.status,
			COALESCE((
				SELECT sr.status
				FROM topic_search_runs sr
				WHERE sr.topic_id = t.id
				ORDER BY sr.started_at DESC, sr.id DESC
				LIMIT 1
			), ''),
			COALESCE((
				SELECT sr.stage
				FROM topic_search_runs sr
				WHERE sr.topic_id = t.id
				ORDER BY sr.started_at DESC, sr.id DESC
				LIMIT 1
			), '')
		FROM topics t
		WHERE t.slug = ?
			AND t.status != 'disabled'
	`, slug).Scan(&topicID, &topic.Slug, &topic.Name, &topic.Status, &topic.RunStatus, &topic.RunStage); err != nil {
		return topicListItem{}, nil, err
	}
	topic.StatusLabel = topicStatusLabel(topic.Status, topic.RunStatus, topic.RunStage)

	rows, err := conn.QueryContext(ctx, `
		SELECT
			title,
			url,
			source,
			snippet,
			rank,
			COALESCE(reviewer_score, 0),
			page_type,
			reviewer_reason,
			accepted
		FROM topic_search_results
		WHERE topic_id = ?
		ORDER BY search_run_id DESC, accepted DESC, COALESCE(reviewer_score, 0) DESC, rank ASC
	`, topicID)
	if err != nil {
		return topicListItem{}, nil, fmt.Errorf("query topic evaluations: %w", err)
	}
	defer rows.Close()

	var results []evaluationResult
	for rows.Next() {
		var result evaluationResult
		var accepted int
		if err := rows.Scan(&result.Title, &result.URL, &result.Source, &result.Snippet, &result.Rank, &result.ReviewerScore, &result.PageType, &result.ReviewerReason, &accepted); err != nil {
			return topicListItem{}, nil, fmt.Errorf("scan topic evaluation: %w", err)
		}
		result.Accepted = accepted == 1
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return topicListItem{}, nil, fmt.Errorf("iterate topic evaluations: %w", err)
	}
	topic.EvaluatedCount = len(results)
	for _, result := range results {
		if result.Accepted {
			topic.AcceptedCount++
		}
	}
	return topic, results, nil
}

func (a app) queueTopic(ctx context.Context, topic string) (queuedTopicView, error) {
	slug := slugFromTopicName(topic)
	if slug == "" {
		return queuedTopicView{}, fmt.Errorf("invalid topic %q", topic)
	}
	name := displayTopicName(topic, slug)
	_, err := a.db.ExecContext(ctx, `
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
		return queuedTopicView{}, fmt.Errorf("upsert queued topic: %w", err)
	}
	return loadQueuedTopic(ctx, a.db, slug)
}

func parseTopicEvaluationsPath(path string) (string, bool) {
	prefix := "/topics/"
	suffix := "/evaluations"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	slug = strings.Trim(slug, "/")
	if !topicPathPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

func parseTopicStatusPath(path string) (string, bool) {
	prefix := "/topics/"
	suffix := "/status"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	slug = strings.Trim(slug, "/")
	if !topicPathPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

func loadQueuedTopic(ctx context.Context, conn *sql.DB, slug string) (queuedTopicView, error) {
	if err := topicsearch.ExpireStaleRunningSearches(ctx, conn, time.Now().UTC()); err != nil {
		return queuedTopicView{}, err
	}

	var queued queuedTopicView
	var runStatus string
	var runStage string
	err := conn.QueryRowContext(ctx, `
		SELECT
			t.slug,
			t.name,
			t.status,
			COALESCE((
				SELECT sr.status
				FROM topic_search_runs sr
				WHERE sr.topic_id = t.id
				ORDER BY sr.started_at DESC, sr.id DESC
				LIMIT 1
			), ''),
			COALESCE((
				SELECT sr.stage
				FROM topic_search_runs sr
				WHERE sr.topic_id = t.id
				ORDER BY sr.started_at DESC, sr.id DESC
				LIMIT 1
			), '')
		FROM topics t
		WHERE t.slug = ?
	`, slug).Scan(&queued.Slug, &queued.Name, &queued.Status, &runStatus, &runStage)
	if err != nil {
		return queuedTopicView{}, fmt.Errorf("load queued topic: %w", err)
	}
	queued.StatusLabel = topicStatusLabel(queued.Status, runStatus, runStage)
	queued.CanProcess = queued.Status == "queued" || queued.Status == "failed"
	queued.IsProcessing = queued.Status == "searching"
	return queued, nil
}

func topicStatusLabel(topicStatus string, runStatus string, runStage string) string {
	if topicStatus == "searching" && runStatus == "running" && strings.TrimSpace(runStage) != "" {
		return runStage
	}
	return topicStatus
}

func processingTopicView(queued queuedTopicView) queuedTopicView {
	queued.Status = "searching"
	queued.StatusLabel = "searching"
	queued.CanProcess = false
	queued.IsProcessing = true
	return queued
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

func renderTemplateByName(w http.ResponseWriter, tmpl *template.Template, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render template %q failed: %v", name, err)
	}
}

func isDatastarRequest(r *http.Request) bool {
	return r.Header.Get("Datastar-Request") != ""
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
