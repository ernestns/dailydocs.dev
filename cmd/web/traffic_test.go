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
	// Reading assignment follows this same read-then-write transaction pattern.
	// Without one pool connection the analytics commit invalidates its snapshot.
	flushed := make(chan error, 1)
	go func() { flushed <- c.Close(context.Background()) }()
	deadline := time.After(2 * time.Second)
	finished := false
wait:
	for conn.Stats().WaitCount == 0 {
		select {
		case err = <-flushed:
			if err != nil {
				t.Fatal(err)
			}
			finished = true
			break wait
		case <-deadline:
			t.Fatal("analytics did not flush or wait for the app transaction")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err = tx.Exec(`INSERT INTO topics(slug,name) VALUES('sqlite','SQLite')`); err != nil {
		t.Fatalf("analytics invalidated app write: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !finished {
		if err = <-flushed; err != nil {
			t.Fatal(err)
		}
	}
	var views int
	if err = conn.QueryRow(`SELECT sum(count) FROM traffic_daily WHERE kind='total'`).Scan(&views); err != nil || views != 1 {
		t.Fatalf("analytics flush lost after app commit: views=%d err=%v", views, err)
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
