package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

type shutdownWaitingProvider struct {
	started chan struct{}
	calls   atomic.Int32
}

func (p *shutdownWaitingProvider) Search(ctx context.Context, _ string, _ int) ([]topicsearch.SearchResult, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSearchShutdownFinalizesActiveJobAndLeavesWaitersRetryable(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	t.Cleanup(func() { _ = conn.Close() })
	_, err := conn.Exec(`
		INSERT INTO topics(id,slug,name,status) VALUES (1,'rust','Rust','failed'),(2,'python','Python','queued');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES
			(1,1,'Saved ownership','https://doc.rust-lang.org/saved-ownership',1),
			(2,1,'Saved generics','https://doc.rust-lang.org/saved-generics',2);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-09-10',1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newTopicSearchLifecycle()
	t.Cleanup(func() {
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := lifecycle.wait(deadline); err != nil {
			t.Error(err)
		}
	})
	p := &shutdownWaitingProvider{started: make(chan struct{})}
	a := app{db: conn, now: time.Now, searchMu: &sync.Mutex{}, pendingSearches: &sync.Map{}, searchProvider: p, asyncProcessing: true, searchLifecycle: lifecycle}
	if !a.processQueuedTopicAsync("rust") {
		t.Fatal("first request was not admitted")
	}
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if a.processQueuedTopicAsync("rust") {
		t.Fatal("duplicate job was admitted")
	}
	if !a.processQueuedTopicAsync("python") {
		t.Fatal("second request was not queued")
	}

	// Keep persistence occupied briefly to prove drain includes failure cleanup,
	// not merely the provider returning when its context is canceled.
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec("UPDATE topics SET description='brief write' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	lifecycle.stop()
	if a.processQueuedTopicAsync("go") {
		t.Fatal("shutdown admitted a new request")
	}
	drained := make(chan error, 1)
	go func() {
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		drained <- lifecycle.wait(deadline)
	}()
	select {
	case err := <-drained:
		t.Fatalf("shutdown returned before database cleanup could finish: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 {
		t.Fatalf("shutdown started waiting provider work: calls=%d", p.calls.Load())
	}
	a.pendingSearches.Range(func(key, value any) bool {
		t.Error("shutdown left pending work", key)
		return true
	})
	var runStatus, diagnostic, topicStatus, waitingStatus string
	var completed, waitingRuns int
	if err := conn.QueryRow(`SELECT r.status,r.error,r.completed_at IS NOT NULL,t.status FROM topic_search_runs r JOIN topics t ON t.id=r.topic_id WHERE t.slug='rust'`).Scan(&runStatus, &diagnostic, &completed, &topicStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" || !strings.Contains(diagnostic, context.Canceled.Error()) || completed != 1 || topicStatus != "active" {
		t.Fatal("shutdown did not finalize failure while preserving saved catalog", runStatus, diagnostic, completed, topicStatus)
	}
	if err := conn.QueryRow(`SELECT status,(SELECT COUNT(*) FROM topic_search_runs WHERE topic_id=2) FROM topics WHERE id=2`).Scan(&waitingStatus, &waitingRuns); err != nil || waitingStatus != "queued" || waitingRuns != 0 {
		t.Fatal("waiting topic changed during shutdown", waitingStatus, waitingRuns, err)
	}
	if got, err := reading.GetDailyReading(ctx, conn, "rust", "2026-09-10"); err != nil || got.PageID != 1 {
		t.Fatal("historical reading changed", got, err)
	}

	// A fresh process can immediately honor explicit retries, without waiting
	// for the stale-running timeout or automatically resuming the old queue.
	retryProvider := &quantityProvider{count: 3}
	restarted := app{db: conn, now: time.Now, searchProvider: retryProvider}
	for _, slug := range []string{"rust", "python"} {
		response := httptest.NewRecorder()
		restarted.processTopicHandler(response, topicRequest(http.MethodPost, "/process-topic", slug))
		if response.Code != http.StatusSeeOther {
			t.Fatal("explicit retry was blocked after clean shutdown", slug, response.Code, response.Body.String())
		}
	}
	if retryProvider.calls != 2 {
		t.Fatalf("expected both explicit retries, provider calls=%d", retryProvider.calls)
	}
}

func TestSearchShutdownWaitHasBoundAndAdmissionStaysClosed(t *testing.T) {
	lifecycle := newTopicSearchLifecycle()
	jobCtx, finish, ok := lifecycle.admit()
	if !ok {
		t.Fatal("job was not admitted")
	}
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lifecycle.wait(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished job should hit shutdown deadline: %v", err)
	}
	if !errors.Is(jobCtx.Err(), context.Canceled) {
		t.Fatal("shutdown did not cancel the job context")
	}
	if _, _, ok := lifecycle.admit(); ok {
		t.Fatal("shutdown admission gate reopened")
	}
	finish()
	if err := lifecycle.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSearchShutdownAdmissionRaceDrainsEveryAcceptedJob(t *testing.T) {
	lifecycle := newTopicSearchLifecycle()
	var attempts sync.WaitGroup
	for range 100 {
		attempts.Add(1)
		go func() {
			defer attempts.Done()
			ctx, finish, accepted := lifecycle.admit()
			if accepted {
				<-ctx.Done()
				finish()
			}
		}()
	}
	lifecycle.stop()
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lifecycle.wait(deadline); err != nil {
		t.Fatal(err)
	}
	attempts.Wait()
}
