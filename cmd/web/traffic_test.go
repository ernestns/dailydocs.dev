package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
