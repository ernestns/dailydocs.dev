package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
)

func TestTrafficFlushCannotInvalidateAnApplicationWriteTransaction(t *testing.T) {
	conn, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := newTrafficCollector(conn)
	defer func() { _ = c.Close(context.Background()) }()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://dailydocs.dev/", nil))
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var topics int
	if err = tx.QueryRow(`SELECT count(*) FROM topics`).Scan(&topics); err != nil {
		t.Fatal(err)
	}
	// Reading assignment follows this same immediate read-then-write transaction
	// pattern. Analytics gets a separate connection but waits for its writer lock.
	flushed := make(chan error, 1)
	go func() { flushed <- c.Close(context.Background()) }()
	deadline := time.After(2 * time.Second)
	for conn.Stats().InUse < 2 {
		select {
		case err = <-flushed:
			t.Fatalf("analytics finished before the app transaction released its writer lock: %v", err)
		case <-deadline:
			t.Fatal("analytics did not acquire a separate pooled connection")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-flushed:
		t.Fatalf("analytics committed while the app held its read-then-write snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err = tx.Exec(`INSERT INTO topics(slug,name) VALUES('sqlite','SQLite')`); err != nil {
		t.Fatalf("analytics invalidated app write: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-flushed; err != nil {
		t.Fatal(err)
	}
	var views int
	if err = conn.QueryRow(`SELECT sum(count) FROM traffic_daily WHERE kind='total'`).Scan(&views); err != nil || views != 1 {
		t.Fatalf("analytics flush lost after app commit: views=%d err=%v", views, err)
	}
}

func TestBlockedTrafficFlushLeavesSavedReadingAvailable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dailydocs.sqlite")
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	external, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	seedWebDailyReading(t, ctx, conn, "sqlite", "2026-06-27", "Write-Ahead Logging")
	c := newTrafficCollector(conn)
	defer func() { _ = c.Close(ctx) }()
	provider := &countingWebProvider{}
	h := c.Middleware(newTestHandlerWithProvider(conn, provider))
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "http://dailydocs.dev/sqlite", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("seeded saved reading: %d", first.Code)
	}

	writer, err := external.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec(`UPDATE topics SET description='uncommitted external writer' WHERE slug='sqlite'`); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	// Close drains the real middleware event and runs the same flush as the
	// periodic collector tick, without waiting ten seconds for that tick.
	go func() { flushed <- c.Close(ctx) }()
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse != 1 {
		if time.Now().After(deadline) {
			t.Fatal("collector did not occupy a pooled connection")
		}
		time.Sleep(time.Millisecond)
	}
	// Allow the short WAL label read to finish; the flush then remains blocked
	// at writer admission until the independent transaction releases its lock.
	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-flushed:
		t.Fatalf("flush unexpectedly finished under the external writer: %v", err)
	default:
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dailydocs.dev/sqlite", nil))
		done <- response
	}()
	select {
	case response := <-done:
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Write-Ahead Logging") {
			t.Fatalf("saved reading failed: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(500 * time.Millisecond):
		_ = writer.Rollback()
		<-done
		t.Fatal("saved reading waited for a blocked analytics flush")
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("analytics flush after release: %v", err)
	}
	var assignments, runs, views int
	if err := conn.QueryRow(`SELECT (SELECT count(*) FROM daily_readings),
		(SELECT count(*) FROM topic_search_runs),
		(SELECT sum(count) FROM traffic_daily WHERE kind='total')`).Scan(&assignments, &runs, &views); err != nil {
		t.Fatal(err)
	}
	if assignments != 1 || runs != 0 || views != 1 || provider.calls != 0 {
		t.Fatalf("changed application state or lost view: assignments=%d runs=%d views=%d provider=%d", assignments, runs, views, provider.calls)
	}
}

func TestTrafficCountsUnicodeAndLongApplicationRoutes(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	c := newTrafficCollector(conn)
	defer func() { _ = c.Close(context.Background()) }()
	h := c.Middleware(newTestHandler(conn))
	want := map[string]int{}
	for _, slug := range []string{"日本語", strings.Repeat("long-", 20) + "topic", strings.Repeat("日本語", 40)} {
		var topicID int64
		if err := conn.QueryRow(`INSERT INTO topics(slug,name,status) VALUES(?,?,'active') RETURNING id`, slug, slug).Scan(&topicID); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`INSERT INTO pages(topic_id,title,url,reading_order) VALUES(?,'Saved reading','https://docs.example.org/guide',1)`, topicID); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/" + slug, "/" + slug + "/2026-06-27", "/topics/" + slug + "/evaluations"} {
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dailydocs.dev"+path+"?private=not-recorded", nil))
			if response.Code != http.StatusOK {
				t.Fatal("fixture is not a successful app route", path, response.Code, response.Body.String())
			}
			label := "/" + slug
			if strings.HasSuffix(path, "/evaluations") {
				label = path
			}
			if len(label) > 256 {
				label = "(other)"
			}
			want[label]++
		}
	}
	for _, path := range []string{"/health", "/topics/日本語/status", "/topics/search?q=日本語", "/static/style.css", "/absent"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://dailydocs.dev"+path, nil))
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(`SELECT label,sum(count) FROM traffic_daily WHERE kind='page' GROUP BY label`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var label string
		var count int
		if err := rows.Scan(&label, &count); err != nil {
			t.Fatal(err)
		}
		got[label] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("application pages omitted or mislabeled: got=%v want=%v", got, want)
	}
	var views, drops int
	if err := conn.QueryRow(`SELECT coalesce(sum(CASE WHEN kind='total' THEN count ELSE 0 END),0),coalesce(sum(CASE WHEN kind='dropped' THEN count ELSE 0 END),0) FROM traffic_daily`).Scan(&views, &drops); err != nil || views != 9 || drops != 0 {
		t.Fatal("valid application traffic lost", views, drops, err)
	}
}
