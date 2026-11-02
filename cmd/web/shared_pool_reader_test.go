package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
)

func TestRuntimePoolKeepsSavedReadersAvailableDuringPendingAssignment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(id,slug,name,status) VALUES(1,'sqlite','SQLite','active'),(2,'go','Go','active'),(3,'unrelated','Unrelated','queued');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(11,1,'Saved SQLite guide','https://docs.example.org/sqlite-guide',1),(21,2,'Saved Go guide','https://docs.example.org/go-guide',1);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',11),(2,'2026-06-27',21);
	`); err != nil {
		t.Fatal(err)
	}
	// Match run(): traffic and handlers share this DB with its runtime pool
	// configuration. Do not override the connection count in the fixture.
	collector := newTrafficCollector(conn)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := collector.Close(cleanup); err != nil {
			t.Error(err)
		}
	}()
	provider := &quantityProvider{count: 3}
	handler := collector.Middleware(newTestHandlerWithProvider(conn, provider))
	owner, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	writer, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, "UPDATE topics SET description='unrelated write' WHERE id=3"); err != nil {
		t.Fatal(err)
	}
	todayDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sqlite", nil).WithContext(ctx))
		todayDone <- response
	}()
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse == 0 {
		if time.Now().After(deadline) {
			t.Fatal("new assignment did not reach the held writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case response := <-todayDone:
		t.Fatalf("new assignment returned before writer released: %d", response.Code)
	case <-time.After(100 * time.Millisecond):
	}
	type reader struct {
		request  *http.Request
		status   int
		contains string
		location string
	}
	readers := []reader{
		{httptest.NewRequest(http.MethodGet, "/sqlite/2026-06-26", nil), http.StatusOK, "Saved SQLite guide", ""},
		{httptest.NewRequest(http.MethodGet, "/go", nil), http.StatusOK, "Saved Go guide", ""},
		{topicRequest(http.MethodPost, "/read", "SQLite"), http.StatusSeeOther, "", "/sqlite"},
		{httptest.NewRequest(http.MethodGet, "/topics", nil), http.StatusOK, `href="/sqlite"`, ""},
		{httptest.NewRequest(http.MethodGet, "/topics/sqlite/status", nil), http.StatusOK, "View reading", ""},
	}
	type outcome struct {
		reader   reader
		response *httptest.ResponseRecorder
	}
	done := make(chan outcome, len(readers))
	for _, read := range readers {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, read.request.WithContext(ctx))
			done <- outcome{read, response}
		}()
	}
	var responses []outcome
	readerDeadline := time.NewTimer(time.Second)
	defer readerDeadline.Stop()
collect:
	for len(responses) < len(readers) {
		select {
		case got := <-done:
			responses = append(responses, got)
		case <-readerDeadline.C:
			break collect
		}
	}
	completedBeforeRelease := len(responses)
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	for len(responses) < len(readers) {
		select {
		case got := <-done:
			responses = append(responses, got)
		case <-ctx.Done():
			t.Fatal("readers did not finish after writer released")
		}
	}
	today := <-todayDone
	if completedBeforeRelease != len(readers) {
		t.Errorf("only %d/%d saved readers completed while an unrelated writer delayed today's assignment", completedBeforeRelease, len(readers))
	}
	for _, got := range responses {
		if got.response.Code != got.reader.status || !strings.Contains(got.response.Body.String(), got.reader.contains) || got.response.Header().Get("Location") != got.reader.location {
			t.Errorf("%s %s failed: status=%d location=%s body=%s", got.reader.request.Method, got.reader.request.URL.Path, got.response.Code, got.response.Header().Get("Location"), got.response.Body.String())
		}
	}
	if today.Code != http.StatusOK || !strings.Contains(today.Body.String(), "Saved SQLite guide") {
		t.Fatal("pending assignment did not succeed after writer released", today.Code)
	}
	var assignments, todayAssignments, archivedID, todayID, goID, runs int
	var archivedURL string
	if err := conn.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM daily_readings),
		(SELECT count(*) FROM daily_readings WHERE topic_id=1 AND reading_date='2026-06-27'),
		(SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-06-26'),
		(SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-06-27'),
		(SELECT page_id FROM daily_readings WHERE topic_id=2 AND reading_date='2026-06-27'),
		(SELECT url FROM pages WHERE id=11),
		(SELECT count(*) FROM topic_search_runs)
	`).Scan(&assignments, &todayAssignments, &archivedID, &todayID, &goID, &archivedURL, &runs); err != nil {
		t.Fatal(err)
	}
	if assignments != 3 || todayAssignments != 1 || archivedID != 11 || todayID != 11 || goID != 21 || archivedURL != "https://docs.example.org/sqlite-guide" || runs != 0 || provider.calls != 0 {
		t.Fatal("runtime pool changed reading history or generated work", assignments, todayAssignments, archivedID, todayID, goID, archivedURL, runs, provider.calls)
	}
}
