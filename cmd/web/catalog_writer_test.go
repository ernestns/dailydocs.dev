package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTopicCatalogReadsDuringUnrelatedWriter(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "no searches"
		if running {
			name = "healthy running search"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(2)
			importWebTopic(t, ctx, conn, "sqlite", "SQLite")
			seedWebDailyReading(t, ctx, conn, "sqlite", "2026-06-27", "Write-Ahead Logging")
			if _, err := conn.ExecContext(ctx, `INSERT INTO topics(slug,name,status) VALUES('other','Other','queued')`); err != nil {
				t.Fatal(err)
			}
			if running {
				if _, err := conn.ExecContext(ctx, `
					UPDATE topics SET status='searching' WHERE slug='sqlite';
					INSERT INTO topic_search_runs(topic_id,provider,query,status,stage,started_at)
					SELECT id,'fake','SQLite','running','reviewing',? FROM topics WHERE slug='sqlite'
				`, time.Now().UTC().Add(-time.Minute).Format("2006-01-02 15:04:05")); err != nil {
					t.Fatal(err)
				}
			}
			writer, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(ctx, `UPDATE topics SET description='unrelated write' WHERE slug='other'`); err != nil {
				t.Fatal(err)
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics", nil).WithContext(ctx))
				done <- response
			}()
			select {
			case response := <-done:
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `href="/sqlite"`) {
					t.Fatalf("catalog unavailable during unrelated write: %d %s", response.Code, response.Body.String())
				}
			case <-time.After(time.Second):
				// Release the fixture lock before reporting the regression, so the
				// request can finish without waiting for SQLite's busy timeout.
				_ = writer.Rollback()
				<-done
				t.Fatal("catalog waited for an unrelated writer despite having no cleanup work")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sqlite/2026-06-27", nil).WithContext(ctx))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Write-Ahead Logging") {
				t.Fatal("saved reading was hidden during unrelated write", response.Code)
			}
			if err := writer.Rollback(); err != nil {
				t.Fatal(err)
			}
			var topics, pages, assignments, runs, unfinished int
			var status string
			if err := conn.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM topics), (SELECT count(*) FROM pages),
				(SELECT count(*) FROM daily_readings), (SELECT count(*) FROM topic_search_runs),
				(SELECT count(*) FROM topic_search_runs WHERE status='running' AND stage='reviewing' AND completed_at IS NULL),
				(SELECT status FROM topics WHERE slug='sqlite')
			`).Scan(&topics, &pages, &assignments, &runs, &unfinished, &status); err != nil {
				t.Fatal(err)
			}
			wantRuns, wantStatus := 0, "active"
			if running {
				wantRuns, wantStatus = 1, "searching"
			}
			if topics != 2 || pages != 1 || assignments != 1 || runs != wantRuns || unfinished != wantRuns || status != wantStatus || provider.calls != 0 {
				t.Fatalf("catalog changed saved state: topics=%d pages=%d assignments=%d runs=%d unfinished=%d status=%s calls=%d", topics, pages, assignments, runs, unfinished, status, provider.calls)
			}
		})
	}
}
