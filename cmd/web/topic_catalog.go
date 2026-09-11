package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

const topicCatalogPageSize = 50

type topicCatalogFilter struct {
	Query  string
	Status string
	Page   int
}

type topicCatalogPage struct {
	Topics      []topicListItem
	Query       string
	Status      string
	Page        int
	TotalPages  int
	Total       int
	Start       int
	End         int
	PreviousURL string
	NextURL     string
}

func (a app) topicsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filter, err := parseTopicCatalogFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, err := listRequestedTopics(r.Context(), a.db, filter)
	if err != nil {
		log.Printf("list topic catalog failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	renderTemplate(w, topicsTemplate, page)
}

func parseTopicCatalogFilter(values url.Values) (topicCatalogFilter, error) {
	filter := topicCatalogFilter{Query: strings.TrimSpace(values.Get("q")), Status: values.Get("status"), Page: 1}
	switch filter.Status {
	case "", "active", "queued", "searching", "failed":
	default:
		return topicCatalogFilter{}, fmt.Errorf("invalid topic status filter")
	}
	if raw := values.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return topicCatalogFilter{}, fmt.Errorf("page must be a positive integer")
		}
		filter.Page = page
	}
	return filter, nil
}

func topicCatalogWhere(filter topicCatalogFilter) (string, []any) {
	where := "t.status != 'disabled'"
	var args []any
	if filter.Status != "" {
		where += " AND t.status = ?"
		args = append(args, filter.Status)
	}
	if filter.Query != "" {
		where += ` AND (t.slug LIKE ? ESCAPE '\' OR t.name LIKE ? ESCAPE '\')`
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(filter.Query)
		like := "%" + escaped + "%"
		args = append(args, like, like)
	}
	return where, args
}

func topicCatalogURL(filter topicCatalogFilter, page int) string {
	values := url.Values{"page": {strconv.Itoa(page)}}
	if filter.Query != "" {
		values.Set("q", filter.Query)
	}
	if filter.Status != "" {
		values.Set("status", filter.Status)
	}
	return "/topics?" + values.Encode()
}

func listRequestedTopics(ctx context.Context, conn *sql.DB, filter topicCatalogFilter) (topicCatalogPage, error) {
	if err := topicsearch.ExpireStaleRunningSearches(ctx, conn, time.Now().UTC()); err != nil {
		return topicCatalogPage{}, err
	}
	page := topicCatalogPage{Query: filter.Query, Status: filter.Status, Page: filter.Page, TotalPages: 1}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, fmt.Errorf("begin topic catalog: %w", err)
	}
	defer tx.Rollback()
	where, args := topicCatalogWhere(filter)
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM topics t WHERE "+where, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count topic catalog: %w", err)
	}
	if page.Total > 0 {
		page.TotalPages = (page.Total-1)/topicCatalogPageSize + 1
	}
	if page.Page > page.TotalPages {
		page.Page = page.TotalPages
	}
	offset := (page.Page - 1) * topicCatalogPageSize
	// Materialize the bounded topic selection before counting its candidates/pages.
	// The count query above scans only topics, never the entire candidate catalog.
	rows, err := tx.QueryContext(ctx, `
  WITH selected AS MATERIALIZED (
   SELECT t.id,t.slug,t.name,t.status
   FROM topics t WHERE `+where+`
   ORDER BY t.name COLLATE NOCASE,t.id
   LIMIT ? OFFSET ?
  )
  SELECT t.id,t.slug,t.name,t.status,
   COALESCE((SELECT sr.status FROM topic_search_runs sr WHERE sr.topic_id=t.id ORDER BY sr.started_at DESC,sr.id DESC LIMIT 1),''),
   COALESCE((SELECT sr.stage FROM topic_search_runs sr WHERE sr.topic_id=t.id ORDER BY sr.started_at DESC,sr.id DESC LIMIT 1),''),
   (SELECT COUNT(*) FROM topic_search_results r WHERE r.topic_id=t.id),
   (SELECT COUNT(*) FROM topic_search_results r WHERE r.topic_id=t.id AND r.accepted=1)
  FROM selected t
  ORDER BY t.name COLLATE NOCASE,t.id
 `, append(args, topicCatalogPageSize, offset)...)
	if err != nil {
		return page, fmt.Errorf("query topic catalog: %w", err)
	}
	defer rows.Close()
	var topicIDs []int64
	for rows.Next() {
		var topic topicListItem
		var topicID int64
		if err := rows.Scan(&topicID, &topic.Slug, &topic.Name, &topic.Status, &topic.RunStatus, &topic.RunStage, &topic.EvaluatedCount, &topic.AcceptedCount); err != nil {
			return page, fmt.Errorf("scan topic catalog: %w", err)
		}
		topic.StatusLabel = topicStatusLabel(topic.Status, topic.RunStatus, topic.RunStage)
		page.Topics = append(page.Topics, topic)
		topicIDs = append(topicIDs, topicID)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("iterate topic catalog: %w", err)
	}
	if err := rows.Close(); err != nil {
		return page, err
	}
	for i, topicID := range topicIDs {
		count, err := topicsearch.AvailableReadings(ctx, tx, topicID)
		if err != nil {
			return page, fmt.Errorf("count topic readings: %w", err)
		}
		page.Topics[i].ReadingCount = count
	}
	if err := tx.Commit(); err != nil {
		return page, fmt.Errorf("finish topic catalog: %w", err)
	}
	if len(page.Topics) > 0 {
		page.Start = offset + 1
		page.End = offset + len(page.Topics)
	}
	if page.Page > 1 {
		page.PreviousURL = topicCatalogURL(filter, page.Page-1)
	}
	if page.Page < page.TotalPages {
		page.NextURL = topicCatalogURL(filter, page.Page+1)
	}
	return page, nil
}
