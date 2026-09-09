package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

// Hold the first real generation while the second request follows its redirect.
type waitingWebProvider struct {
	started, release chan struct{}
	calls            atomic.Int32
}

func (p *waitingWebProvider) Search(ctx context.Context, _ string, _ int) ([]topicsearch.SearchResult, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []topicsearch.SearchResult{{Title: "Generics", URL: "https://doc.rust-lang.org/book/ch10-00-generics.html"}}, nil
}

func topicRequest(method, path, topic string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(url.Values{"topic": {topic}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestExplicitQueuedRequestKeepsPolling(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	p := &waitingWebProvider{started: make(chan struct{}), release: make(chan struct{})}
	a := app{db: conn, now: time.Now, searchMu: &sync.Mutex{}, searchProvider: p, asyncProcessing: true}
	var release sync.Once
	defer release.Do(func() { close(p.release) })
	first := httptest.NewRecorder()
	a.generateReadingHandler(first, topicRequest(http.MethodPost, "/read", "Rust"))
	if first.Code != http.StatusSeeOther {
		t.Fatal(first.Code, first.Body.String())
	}
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("first generation did not start")
	}
	second := httptest.NewRecorder()
	a.generateReadingHandler(second, topicRequest(http.MethodPost, "/read", "Python"))
	if second.Code != http.StatusSeeOther || second.Header().Get("Location") != "/python" {
		t.Fatal(second.Code, second.Header())
	}
	waiting := httptest.NewRecorder()
	a.routeHandler(waiting, topicRequest(http.MethodGet, second.Header().Get("Location"), ""))
	if waiting.Code != http.StatusOK || !strings.Contains(waiting.Body.String(), "This topic is queued") || !strings.Contains(waiting.Body.String(), "data-on-interval") {
		t.Errorf("redirected queued page must keep polling: %d %s", waiting.Code, waiting.Body.String())
	}
	// Status refreshes must preserve polling without adding generation work.
	for range 2 {
		status := httptest.NewRecorder()
		a.topicStatusHandler(status, topicRequest(http.MethodGet, "/topics/python/status", ""), "python")
		if !strings.Contains(status.Body.String(), "data-on-interval") {
			t.Error("queued status refresh stopped polling")
		}
	}
	if p.calls.Load() != 1 {
		t.Error("queued GET started generation")
	}
	release.Do(func() { close(p.release) })
	waitForWebTopicStatus(t, ctx, conn, "rust", "active")
	waitForWebTopicStatus(t, ctx, conn, "python", "active")
	ready := httptest.NewRecorder()
	a.topicStatusHandler(ready, topicRequest(http.MethodGet, "/topics/python/status", ""), "python")
	if strings.Contains(ready.Body.String(), "data-on-interval") || !strings.Contains(ready.Body.String(), "View reading") || p.calls.Load() != 2 {
		t.Fatal("completed status must offer reading and stop polling", ready.Body.String(), p.calls.Load())
	}
}

type deadlineWebPlanner struct{}

func (deadlineWebPlanner) Plan(ctx context.Context, _ string) (topicsearch.PlanOutput, error) {
	<-ctx.Done()
	return topicsearch.PlanOutput{}, ctx.Err()
}

func TestPlannerDeadlineShowsRetryAndReleasesOtherTopics(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	p := &countingWebProvider{}
	a := app{db: conn, now: time.Now, searchMu: &sync.Mutex{}, searchProvider: p, searchPlanner: deadlineWebPlanner{}}
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	a.processQueuedTopic(deadline, "rust")
	var topicStatus, runStatus, runError string
	if err := conn.QueryRow("SELECT t.status,r.status,r.error FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.slug='rust'").Scan(&topicStatus, &runStatus, &runError); err != nil {
		t.Fatal(err)
	}
	if topicStatus != "failed" || runStatus != "failed" || runError != context.DeadlineExceeded.Error() {
		t.Fatalf("deadline must persist original failure: topic=%s run=%s error=%s", topicStatus, runStatus, runError)
	}
	failed := httptest.NewRecorder()
	a.routeHandler(failed, topicRequest(http.MethodGet, "/rust", ""))
	if !strings.Contains(failed.Body.String(), "Process topic") || strings.Contains(failed.Body.String(), "data-on-interval") {
		t.Fatal("failed topic must stop polling and offer retry", failed.Body.String())
	}
	a.searchPlanner = nil
	other := httptest.NewRecorder()
	a.generateReadingHandler(other, topicRequest(http.MethodPost, "/read", "Python"))
	if other.Code != http.StatusSeeOther || p.calls != 1 {
		t.Fatal("failed run must release unrelated generation", other.Code, p.calls)
	}
	retry := httptest.NewRecorder()
	a.processTopicHandler(retry, topicRequest(http.MethodPost, "/process-topic", "rust"))
	if retry.Code != http.StatusSeeOther || p.calls != 2 {
		t.Fatal("explicit retry must succeed", retry.Code, p.calls)
	}
	waitForWebTopicStatus(t, ctx, conn, "rust", "active")
	waitForWebTopicStatus(t, ctx, conn, "python", "active")
}
