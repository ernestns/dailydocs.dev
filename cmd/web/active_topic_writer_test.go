package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExistingCatalogFormReadsDuringUnrelatedWriter(t *testing.T) {
	for _, tc := range []struct{ slug, name, input string }{
		{"sqlite", "SQLite", " sqlite "},
		{"c", "C++", "C++"},
		{"health-topic", "Health", "Health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(2)
			importWebTopic(t, ctx, conn, tc.slug, tc.name)
			seedWebDailyReading(t, ctx, conn, tc.slug, "2026-06-26", "Write-Ahead Logging")
			if _, err := conn.ExecContext(ctx, `
				UPDATE topics SET updated_at='2020-01-01 00:00:00';
				INSERT INTO topics(slug,name,status) VALUES('other','Other','queued')
			`); err != nil {
				t.Fatal(err)
			}
			writer, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(ctx, `UPDATE topics SET description='unrelated writer' WHERE slug='other'`); err != nil {
				t.Fatal(err)
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, topicRequest(http.MethodPost, "/read", tc.input).WithContext(ctx))
				done <- response
			}()
			select {
			case response := <-done:
				if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/"+tc.slug {
					t.Fatalf("existing catalog lookup failed: %d %q", response.Code, response.Header().Get("Location"))
				}
			case <-time.After(time.Second):
				_ = writer.Rollback()
				<-done
				t.Fatal("existing catalog form waited for an unrelated writer")
			}
			if err := writer.Rollback(); err != nil {
				t.Fatal(err)
			}
			var name, status, updated string
			var topics, pages, assignments, runs int
			if err := conn.QueryRowContext(ctx, `SELECT name,status,updated_at,
				(SELECT count(*) FROM topics), (SELECT count(*) FROM pages),
				(SELECT count(*) FROM daily_readings), (SELECT count(*) FROM topic_search_runs)
				FROM topics WHERE slug=?`, tc.slug).Scan(&name, &status, &updated, &topics, &pages, &assignments, &runs); err != nil {
				t.Fatal(err)
			}
			if name != tc.name || status != "active" || updated != "2020-01-01 00:00:00" || topics != 2 || pages != 1 || assignments != 1 || runs != 0 || provider.calls != 0 {
				t.Fatalf("existing form mutated data or generated: name=%s status=%s updated=%s topics=%d pages=%d assignments=%d runs=%d calls=%d", name, status, updated, topics, pages, assignments, runs, provider.calls)
			}
		})
	}
}

func TestTopicRequestRechecksCatalogActivatedWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(2)
	importWebTopic(t, ctx, conn, "sqlite", "SQLite")
	seedWebDailyReading(t, ctx, conn, "sqlite", "2026-06-26", "Write-Ahead Logging")
	if _, err := conn.ExecContext(ctx, `UPDATE topics SET status='queued'`); err != nil {
		t.Fatal(err)
	}
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE topics SET status='active',updated_at='2020-01-01 00:00:00' WHERE slug='sqlite'`); err != nil {
		t.Fatal(err)
	}
	provider := &countingWebProvider{}
	handler := newTestHandlerWithProvider(conn, provider)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, topicRequest(http.MethodPost, "/read", "SQLite").WithContext(ctx))
		done <- response
	}()
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse < 2 {
		if time.Now().After(deadline) {
			t.Fatal("request did not reach the held writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case response := <-done:
		t.Fatalf("non-active lookup returned before activation committed: %d", response.Code)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	response := <-done
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/sqlite" || provider.calls != 0 {
		t.Fatalf("newly active catalog was not reused: %d %q calls=%d", response.Code, response.Header().Get("Location"), provider.calls)
	}
	var updated, status string
	var topics, assignments, runs int
	if err := conn.QueryRowContext(ctx, `SELECT updated_at,status,
		(SELECT count(*) FROM topics),(SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs)
		FROM topics WHERE slug='sqlite'`).Scan(&updated, &status, &topics, &assignments, &runs); err != nil {
		t.Fatal(err)
	}
	if updated != "2020-01-01 00:00:00" || status != "active" || topics != 1 || assignments != 1 || runs != 0 {
		t.Fatal("request changed the newly active catalog", updated, status, topics, assignments, runs)
	}
}
