package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/submission"
	"github.com/ernestns/daily-docs/internal/topicsource"
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

	created, err := submission.Create(r.Context(), a.db, submission.CreateInput{
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

	a.maybeCreateSourceAndDiscover(r.Context(), created)

	http.Redirect(w, r, "/submissions", http.StatusSeeOther)
}

func (a app) maybeCreateSourceAndDiscover(ctx context.Context, sub submission.Submission) {
	if strings.TrimSpace(sub.SuggestedTopic) == "" {
		return
	}
	topic, ok, err := findTopic(ctx, a.db, sub.SuggestedTopic)
	if err != nil {
		log.Printf("auto source topic lookup failed submission_id=%d error=%v", sub.ID, err)
		return
	}
	if !ok {
		source, err := topicsource.CreateFromSubmission(ctx, a.db, topicsource.CreateFromSubmissionInput{
			SubmissionID: sub.ID,
			TopicSlug:    sub.SuggestedTopic,
			TopicName:    sub.SuggestedTopic,
		})
		if err != nil {
			log.Printf("auto source create for new topic failed submission_id=%d topic=%s error=%v", sub.ID, sub.SuggestedTopic, err)
			return
		}
		a.autoDiscoverSource(ctx, sub.ID, source.ID)
		return
	}

	topicID, err := topicIDBySlug(ctx, a.db, topic.Slug)
	if err != nil {
		log.Printf("auto source topic id lookup failed submission_id=%d topic=%s error=%v", sub.ID, topic.Slug, err)
		return
	}
	source, err := topicsource.CreateForTopic(ctx, a.db, topicsource.CreateForTopicInput{
		TopicID:      topicID,
		URL:          sub.NormalizedURL,
		SubmissionID: sub.ID,
	})
	if err != nil {
		log.Printf("auto source create failed submission_id=%d topic=%s error=%v", sub.ID, topic.Slug, err)
		return
	}
	a.autoDiscoverSource(ctx, sub.ID, source.ID)
}

func (a app) autoDiscoverSource(ctx context.Context, submissionID int64, sourceID int64) {
	discoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := a.discoverSourcePreview(discoverCtx, sourceID); err != nil {
		log.Printf("auto source discovery failed submission_id=%d source_id=%d error=%v", submissionID, sourceID, err)
	}
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

func topicIDBySlug(ctx context.Context, conn *sql.DB, slug string) (int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `
		SELECT id
		FROM topics
		WHERE slug = ?
			AND status = 'active'
	`, slug).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("query topic id: %w", err)
	}
	return id, nil
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
