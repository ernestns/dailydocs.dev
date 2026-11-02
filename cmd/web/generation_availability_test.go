package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDailyLimitExplainsWhenToRetryWithoutSchedulingWork(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	now := time.Date(2026, 11, 2, 23, 59, 0, 0, time.UTC).In(time.FixedZone("reader time", -8*60*60))
	for range topicProcessingDailyLimit {
		if _, err := conn.Exec(`INSERT INTO topic_search_runs(topic_id,provider,query,status,started_at) SELECT id,'fake','prior attempt','failed',? FROM topics WHERE slug='rust'`, now.UTC().Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}
	}
	p := &quantityProvider{count: 3}
	a := app{db: conn, now: func() time.Time { return now }, searchProvider: p, pendingSearches: &sync.Map{}}
	for _, path := range []string{"/rust", "/topics/rust/status", "/sqlite"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/topics/rust/status" {
			a.topicStatusHandler(response, request, "rust")
		} else {
			a.routeHandler(response, request)
		}
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "Daily limit reached") || !strings.Contains(body, "2026-11-03 00:00 UTC") {
			t.Fatalf("daily cap must have actionable feedback: %d %s", response.Code, body)
		}
		if strings.Contains(body, "Process topic") || strings.Contains(body, "data-on-interval") {
			t.Fatal("capped work must not advertise immediate processing or polling", body)
		}
		if path == "/sqlite" && !strings.Contains(body, "Write-Ahead Logging") {
			t.Fatal("a global daily limit hid the saved reading")
		}
	}
	retry := httptest.NewRecorder()
	a.processTopicHandler(retry, topicRequest(http.MethodPost, "/process-topic", "rust"))
	if retry.Code != http.StatusSeeOther || p.calls != 0 {
		t.Fatal("daily cap spent provider work", retry.Code, p.calls)
	}
	if _, pending := a.pendingSearches.Load("rust"); pending {
		t.Fatal("daily cap scheduled a futile job")
	}
	now = now.Add(2 * time.Minute)
	available, err := a.loadTopicStatus(ctx, "rust")
	if err != nil || !available.CanProcess {
		t.Fatalf("next UTC day must restore explicit retry: %+v %v", available, err)
	}
	a.processTopicHandler(httptest.NewRecorder(), topicRequest(http.MethodPost, "/process-topic", "rust"))
	if p.calls != 1 {
		t.Fatalf("explicit retry did not resume after reset: %d calls", p.calls)
	}
}

func TestUnavailableGenerationPreservesRequestsAndReadings(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	a := app{db: conn, now: time.Now}
	request := httptest.NewRecorder()
	a.generateReadingHandler(request, topicRequest(http.MethodPost, "/read", "Rust"))
	if request.Code != http.StatusSeeOther {
		t.Fatal(request.Code, request.Body.String())
	}
	for _, path := range []string{"/rust", "/sqlite"} {
		response := httptest.NewRecorder()
		a.routeHandler(response, httptest.NewRequest(http.MethodGet, path, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "Generation temporarily unavailable") || strings.Contains(body, "Process topic") {
			t.Fatalf("unavailable generation must be explained: %d %s", response.Code, body)
		}
		if path == "/sqlite" && !strings.Contains(body, "Write-Ahead Logging") {
			t.Fatal("saved reading disappeared during a generation outage")
		}
	}
	var queued, runs int
	if err := conn.QueryRow("SELECT count(*) FROM topics WHERE slug='rust' AND status='queued'").Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT count(*) FROM topic_search_runs").Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || runs != 0 {
		t.Fatalf("unavailable provider must preserve one request without inventing a run: queued=%d runs=%d", queued, runs)
	}
	a.searchProvider = &quantityProvider{count: 3}
	available, err := a.loadTopicStatus(ctx, "rust")
	if err != nil || !available.CanProcess {
		t.Fatalf("configured provider must restore explicit retry: %+v %v", available, err)
	}
}
