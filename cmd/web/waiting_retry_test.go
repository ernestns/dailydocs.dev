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

func TestWaitingRetryKeepsClientUpdated(t *testing.T) {
	for _, status := range []string{"queued", "failed", "active"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			if _, err := conn.Exec("INSERT INTO topics(id,slug,name,status)VALUES(1,'python','Python',?)", status); err != nil {
				t.Fatal(err)
			}
			if status == "active" {
				if _, err := conn.Exec("INSERT INTO pages(topic_id,title,url,reading_order)VALUES(1,'Saved Python reading','https://docs.python.org/saved',1)"); err != nil {
					t.Fatal(err)
				}
			}
			p := &waitingWebProvider{started: make(chan struct{}), release: make(chan struct{})}
			a := app{db: conn, now: time.Now, searchMu: &sync.Mutex{}, pendingSearches: &sync.Map{}, searchProvider: p, asyncProcessing: true}
			var release sync.Once
			defer release.Do(func() { close(p.release) })
			first := httptest.NewRecorder()
			a.generateReadingHandler(first, topicRequest(http.MethodPost, "/read", "Rust"))
			select {
			case <-p.started:
			case <-time.After(time.Second):
				t.Fatal("first worker did not start")
			}
			retry := httptest.NewRecorder()
			a.processTopicHandler(retry, topicRequest(http.MethodPost, "/process-topic", "python"))
			if retry.Code != http.StatusSeeOther {
				t.Fatal(retry.Code, retry.Body.String())
			}
			// Duplicate explicit retry cannot enqueue a second job for the same topic.
			duplicate := httptest.NewRecorder()
			a.processTopicHandler(duplicate, topicRequest(http.MethodPost, "/process-topic", "python"))
			waiting := httptest.NewRecorder()
			a.routeHandler(waiting, topicRequest(http.MethodGet, retry.Header().Get("Location"), ""))
			refresh := httptest.NewRecorder()
			a.topicStatusHandler(refresh, topicRequest(http.MethodGet, "/topics/python/status", ""), "python")
			var persisted string
			if err := conn.QueryRow("SELECT status FROM topics WHERE id=1").Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			pagePoll := strings.Contains(waiting.Body.String(), "data-on-interval")
			statusPoll := strings.Contains(refresh.Body.String(), "data-on-interval")
			t.Logf("before worker release: seed=%s persisted=%s POST=%d GET=%d page_poll=%v status_poll=%v provider_calls=%d", status, persisted, retry.Code, waiting.Code, pagePoll, statusPoll, p.calls.Load())
			if persisted != status {
				t.Fatal("waiting request invented database processing state", persisted, status)
			}
			if !strings.Contains(waiting.Body.String(), "Waiting for worker") || !strings.Contains(refresh.Body.String(), "Waiting for worker") {
				t.Error("actual pending request not explained")
			}
			if status == "active" && !strings.Contains(waiting.Body.String(), "Saved Python reading") {
				t.Error("saved reading hidden")
			}
			if p.calls.Load() != 1 {
				t.Fatal("GET caused generation")
			}
			release.Do(func() { close(p.release) })
			waitForWebTopicStatus(t, ctx, conn, "rust", "active")
			waitForWebTopicStatus(t, ctx, conn, "python", "active")
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, pending := a.pendingSearches.Load("python"); !pending {
					break
				}
				time.Sleep(time.Millisecond)
			}
			complete := httptest.NewRecorder()
			a.topicStatusHandler(complete, topicRequest(http.MethodGet, "/topics/python/status", ""), "python")
			if strings.Contains(complete.Body.String(), "data-on-interval") || p.calls.Load() != 2 {
				t.Error("completed retry keeps polling or generated twice", p.calls.Load())
			}
			t.Logf("after worker release: provider_calls=%d retry actually completed", p.calls.Load())
			if !pagePoll || !statusPoll {
				t.Errorf("accepted asynchronous retry has no client update while waiting behind unrelated generation")
			}
		})
	}
}
