package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ernestns/daily-docs/internal/topicname"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

type readingLink struct{ Title, URL string }

func addTopicFeedback(ctx context.Context, conn *sql.DB, topicID int64, runStatus string, topic *queuedTopicView) error {
	count, err := topicsearch.AvailableReadings(ctx, conn, topicID)
	if err != nil {
		return err
	}
	topic.AvailableCount = count
	var diagnostic string
	if err := conn.QueryRowContext(ctx, "SELECT COALESCE((SELECT error FROM topic_search_runs WHERE topic_id=? ORDER BY id DESC LIMIT 1),'')", topicID).Scan(&diagnostic); err != nil {
		return err
	}
	topic.InvalidTopic = strings.HasPrefix(diagnostic, topicname.ErrInvalid.Error()) || topicname.Validate(topic.Name) != nil
	topic.RetryBlocked = topic.RetryBlocked && !topic.InvalidTopic
	topic.CanProcess = !topic.Pending && !topic.RetryBlocked && topic.Status != "disabled" && !topic.InvalidTopic && (topic.Status == "queued" || topic.Status == "failed" || topic.Status == "searching" || count < topicsearch.MinimumUsefulResults || runStatus == "failed" || runStatus == "running")
	switch {
	case topic.InvalidTopic:
		topic.StatusLabel = "Choose a topic"
		topic.Message = "Enter a specific subject or technology to find documentation. This request was not treated as a valid topic."
	case topic.RetryBlocked:
		topic.StatusLabel = "Retry temporarily unavailable"
		topic.Message = "The previous request has not been confirmed complete. To avoid overlapping searches, try again when it finishes or its 30-minute timeout expires. Saved links remain available."
	case topic.IsProcessing:
		topic.Message = "Finding useful readings. Saved links remain available while this request runs."
	case topic.Status == "queued":
		topic.Message = "This request is saved. Use Process topic to try now. Saved requests are not automatically scheduled; the daily limit is 20 topics."
	case count < topicsearch.MinimumUsefulResults:
		topic.StatusLabel = "Needs more readings"
		topic.Message = fmt.Sprintf("Useful distinct readings found: %d. We need at least 2 and aim for 3. Try again to find more; saved links are kept.", count)
		if runStatus == "failed" && diagnostic != "" && !strings.HasPrefix(diagnostic, topicsearch.ErrInsufficientResults.Error()) {
			topic.Message += " A source or review service could not finish this attempt."
		}
	case runStatus == "failed":
		topic.Message = "A source or review service could not finish the latest attempt. Your saved readings remain available; you can try again."
	case topic.Status == "searching":
		topic.StatusLabel = "Retry available"
		topic.Message = "Use Process topic to try again. Your saved readings remain available."
	default:
		topic.Message = fmt.Sprintf("%d useful distinct readings are available.", count)
	}
	rows, err := conn.QueryContext(ctx, "SELECT title,url FROM pages WHERE topic_id=? AND active=1 ORDER BY reading_order LIMIT 3", topicID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var link readingLink
		if err := rows.Scan(&link.Title, &link.URL); err != nil {
			return err
		}
		topic.Links = append(topic.Links, link)
	}
	return rows.Err()
}

// Pending work exists only for an explicit request in this process, never for
// historical queued rows. Keep database/run status unchanged while it waits.
func (a app) loadTopicStatus(ctx context.Context, slug string) (queuedTopicView, error) {
	var pending bool
	if a.pendingSearches != nil {
		_, pending = a.pendingSearches.Load(slug)
	}
	topic, err := loadQueuedTopic(ctx, a.db, slug, pending, a.now())
	if err != nil {
		return topic, err
	}
	if topic.Pending && !topic.IsProcessing {
		topic.StatusLabel = "Waiting for worker"
		topic.Message = "This explicit request is waiting for the current worker. The daily limit is checked before generation starts."
	}
	return topic, nil
}
