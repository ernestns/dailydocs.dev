package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func TestRateLimitedRetryPreservesFailedAttemptAndLaterRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	other, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	now := time.Now().UTC()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','active');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(11,1,'Saved first','https://docs.example.org/saved-first',1),(12,1,'Saved second','https://docs.example.org/saved-second',2);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',11);
		INSERT INTO topic_search_runs(id,topic_id,provider,query,status,error,started_at,completed_at)
		VALUES(1,1,'fake','Rust','failed','review service unavailable',?,?);
	`, now.Add(-time.Hour).Format("2006-01-02 15:04:05"), now.Add(-55*time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	provider := &quantityProvider{count: 3}
	planner := &recordingTopicPlanner{}
	a := app{db: conn, now: func() time.Time { return now }, searchProvider: provider, searchPlanner: planner}
	checkFeedback := func(wantRun string, retry bool) {
		t.Helper()
		feedback, err := a.loadTopicStatus(ctx, "rust")
		if err != nil || feedback.CanProcess != retry || strings.Contains(feedback.Message, "could not finish") != retry {
			t.Fatalf("rejected admission changed real-attempt feedback: %+v %v", feedback, err)
		}
		evaluations, _, err := loadTopicEvaluations(ctx, conn, "rust")
		if err != nil || evaluations.RunStatus != wantRun {
			t.Fatal("evaluation status lost the latest actual attempt", evaluations, err)
		}
		catalog, err := listRequestedTopics(ctx, conn, topicCatalogFilter{Query: "rust", Page: 1})
		if err != nil || len(catalog.Topics) != 1 || catalog.Topics[0].RunStatus != wantRun {
			t.Fatal("catalog status lost the latest actual attempt", catalog, err)
		}
		response := httptest.NewRecorder()
		a.routeHandler(response, httptest.NewRequest(http.MethodGet, "/rust/2026-06-26", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Saved first") || strings.Contains(response.Body.String(), `action="/process-topic"`) != retry {
			t.Fatal("saved reading lost history or the appropriate retry action", response.Code, response.Body.String())
		}
	}
	checkFeedback("failed", true)
	external := &waitingWebProvider{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	var release sync.Once
	var worker sync.WaitGroup
	worker.Add(1)
	defer func() {
		release.Do(func() { close(external.release) })
		worker.Wait()
	}()
	go func() {
		defer worker.Done()
		_, err := topicsearch.SearchTopic(ctx, other, "Other Subject", topicsearch.Options{Provider: external, Now: func() time.Time { return now }, MinInterval: time.Nanosecond})
		done <- err
	}()
	select {
	case <-external.started:
	case <-ctx.Done():
		t.Fatal("competing search did not start")
	}
	blocked := httptest.NewRecorder()
	a.processTopicHandler(blocked, topicRequest(http.MethodPost, "/process-topic", "rust"))
	if blocked.Code != http.StatusSeeOther || provider.calls != 0 || len(planner.topics) != 0 {
		t.Fatal("rejected public retry spent provider work", blocked.Code, provider.calls, planner.topics)
	}
	var rejected int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM topic_search_runs WHERE topic_id=1 AND status='rate_limited' AND completed_at IS NOT NULL").Scan(&rejected); err != nil || rejected != 1 {
		t.Fatal("rejected admission audit record was not preserved", rejected, err)
	}
	checkFeedback("failed", true)
	release.Do(func() { close(external.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	retry := httptest.NewRecorder()
	a.processTopicHandler(retry, topicRequest(http.MethodPost, "/process-topic", "rust"))
	if retry.Code != http.StatusSeeOther || provider.calls != 1 || len(planner.topics) != 1 || planner.topics[0] != "Rust" {
		t.Fatal("retry was lost after competing work ended", retry.Code, provider.calls, planner.topics)
	}
	checkFeedback("completed", false)
	// A newer rejected direct request must not resurrect the older failure
	// after an actual generation attempt has succeeded.
	result, err := topicsearch.SearchTopic(ctx, conn, "Rust", topicsearch.Options{Provider: provider, Planner: planner, Now: func() time.Time { return now }, MinInterval: 5 * time.Minute})
	if !errors.Is(err, topicsearch.ErrRateLimited) || !result.RateLimited || provider.calls != 1 || len(planner.topics) != 1 {
		t.Fatal("minimum-interval rejection spent work or changed its audit outcome", result, err)
	}
	checkFeedback("completed", false)
	a.processTopicHandler(httptest.NewRecorder(), topicRequest(http.MethodPost, "/process-topic", "rust"))
	var failed, limited, completed, assignments, pageID, originalPages int
	var originalError, archivedURL string
	if err := conn.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM topic_search_runs WHERE topic_id=1 AND status='failed'),
		(SELECT count(*) FROM topic_search_runs WHERE topic_id=1 AND status='rate_limited'),
		(SELECT count(*) FROM topic_search_runs WHERE topic_id=1 AND status='completed'),
		(SELECT error FROM topic_search_runs WHERE id=1),
		(SELECT count(*) FROM daily_readings),
		(SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-06-26'),
		(SELECT url FROM pages WHERE id=11),
		(SELECT count(*) FROM pages WHERE id IN (11,12) AND active=1)
	`).Scan(&failed, &limited, &completed, &originalError, &assignments, &pageID, &archivedURL, &originalPages); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || limited != 2 || completed != 1 || originalError != "review service unavailable" || assignments != 1 || pageID != 11 || archivedURL != "https://docs.example.org/saved-first" || originalPages != 2 || provider.calls != 1 || len(planner.topics) != 1 {
		t.Fatal("retry changed prior audit/history or repeated completed work", failed, limited, completed, originalError, assignments, pageID, archivedURL, originalPages, provider.calls, planner.topics)
	}
}

func TestRateLimitedAuditDoesNotEraseInvalidAttemptDiagnostic(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	now := time.Now().UTC()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','active');
		INSERT INTO pages(topic_id,title,url,reading_order) VALUES(1,'Saved first','https://docs.example.org/first',1),(1,'Saved second','https://docs.example.org/second',2);
		INSERT INTO topic_search_runs(topic_id,provider,query,status,error,started_at,completed_at)
		VALUES(1,'fake','Rust','failed',?,?,?);
		INSERT INTO topic_search_runs(topic_id,provider,query,status,started_at,completed_at)
		VALUES(1,'fake','Rust','rate_limited',?,?);
	`, topicsearch.ErrInvalidTopic.Error()+": planner rejected topic", now.Add(-time.Minute).Format("2006-01-02 15:04:05"), now.Add(-time.Minute).Format("2006-01-02 15:04:05"), now.Format("2006-01-02 15:04:05"), now.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	provider := &quantityProvider{count: 3}
	a := app{db: conn, now: func() time.Time { return now }, searchProvider: provider}
	feedback, err := a.loadTopicStatus(ctx, "rust")
	if err != nil || !feedback.InvalidTopic || feedback.CanProcess || feedback.StatusLabel != "Choose a topic" {
		t.Fatal("rejected admission erased the actual attempt's diagnostic", feedback, err)
	}
	response := httptest.NewRecorder()
	a.processTopicHandler(response, topicRequest(http.MethodPost, "/process-topic", "rust"))
	if response.Code != http.StatusBadRequest || provider.calls != 0 {
		t.Fatal("invalid topic diagnostic no longer protects explicit processing", response.Code, provider.calls)
	}
}
