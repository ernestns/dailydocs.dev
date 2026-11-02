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

func TestConcurrentFirstRequestsPreserveDistinctTopics(t *testing.T) {
	checkConcurrentTopicRequests(t, []string{"Health", "Health Topic"}, 2)
}

func TestConcurrentSameNameRequestsReuseOneTopic(t *testing.T) {
	checkConcurrentTopicRequests(t, []string{"Health", "Health"}, 1)
}

func checkConcurrentTopicRequests(t *testing.T, names []string, wantCount int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(3)
	// A brief unrelated writer leaves WAL reads available, but holds both inserts.
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	a := app{db: conn, now: time.Now}
	type result struct {
		input string
		topic queuedTopicView
		err   error
	}
	done := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, name := range names {
		go func(name string) {
			ready.Done()
			<-start
			got, _, err := a.queueTopic(ctx, name)
			done <- result{name, got, err}
		}(name)
	}
	ready.Wait()
	close(start)
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse < 3 {
		if time.Now().After(deadline) {
			t.Fatal("two request connections did not reach held writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("request returned while writer held: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	results := []result{<-done, <-done}
	for _, r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.topic.Name != r.input {
			t.Fatalf("request %q was assigned to %q", r.input, r.topic.Name)
		}
		t.Logf("input=%q returned_slug=%q returned_name=%q", r.input, r.topic.Slug, r.topic.Name)
	}
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM topics").Scan(&count); err != nil {
		t.Fatal(err)
	}
	t.Logf("persisted_topic_count=%d", count)
	if count != wantCount || (results[0].topic.Slug == results[1].topic.Slug) != (wantCount == 1) {
		t.Fatalf("simultaneous requests lost their identities: got %d topics, want %d", count, wantCount)
	}
}

func TestTopicRequestDoesNotCaptureConcurrentlyCreatedActiveCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(2)
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `INSERT INTO topics(slug,name,status) VALUES('health-topic','Health Topic','active')`); err != nil {
		t.Fatal(err)
	}
	a := app{db: conn, now: time.Now}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		a.generateReadingHandler(response, topicRequest(http.MethodPost, "/read", "Health"))
		done <- response
	}()
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse < 2 {
		if time.Now().After(deadline) {
			t.Fatal("request did not reach held writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case response := <-done:
		t.Fatalf("request completed before concurrent catalog committed: %d", response.Code)
	case <-time.After(150 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	response := <-done
	location := response.Header().Get("Location")
	if response.Code != http.StatusSeeOther || location == "/health-topic" || location == "" {
		t.Fatalf("request captured the unrelated catalog: %d %q", response.Code, location)
	}
	var name, status string
	if err := conn.QueryRowContext(ctx, "SELECT name,status FROM topics WHERE slug=?", strings.TrimPrefix(location, "/")).Scan(&name, &status); err != nil {
		t.Fatal(err)
	}
	if name != "Health" || status != "queued" {
		t.Fatalf("request identity changed: name=%q status=%q", name, status)
	}
	if err := conn.QueryRowContext(ctx, "SELECT name,status FROM topics WHERE slug='health-topic'").Scan(&name, &status); err != nil {
		t.Fatal(err)
	}
	if name != "Health Topic" || status != "active" {
		t.Fatalf("concurrent catalog was changed: name=%q status=%q", name, status)
	}
}

func TestExistingCatalogFormLookupDoesNotWriteOrGenerate(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	importWebTopic(t, ctx, conn, "health-topic", "Health")
	seedWebDailyReading(t, ctx, conn, "health-topic", "2026-06-26", "Write-Ahead Logging")
	if _, err := conn.ExecContext(ctx, `UPDATE topics SET updated_at='2020-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	provider := &countingWebProvider{}
	handler := newTestHandlerWithProvider(conn, provider)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, topicRequest(http.MethodPost, "/read", "Health"))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/health-topic" || provider.calls != 0 {
		t.Fatalf("saved catalog lookup changed behavior: %d %q calls=%d", response.Code, response.Header().Get("Location"), provider.calls)
	}
	var updated string
	var assignments, runs int
	if err := conn.QueryRowContext(ctx, `SELECT updated_at, (SELECT COUNT(*) FROM daily_readings), (SELECT COUNT(*) FROM topic_search_runs) FROM topics WHERE slug='health-topic'`).Scan(&updated, &assignments, &runs); err != nil {
		t.Fatal(err)
	}
	if updated != "2020-01-01 00:00:00" || assignments != 1 || runs != 0 {
		t.Fatalf("lookup mutated saved state: updated=%s assignments=%d runs=%d", updated, assignments, runs)
	}
}
