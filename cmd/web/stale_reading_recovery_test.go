package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaleCleanupRestoresUnassignedTodayAndPreservesArchive(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "stale running retry"
		if orphan {
			name = "orphan after failed retry"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			if _, err := conn.Exec(`
				INSERT INTO topics(id,slug,name,status) VALUES(1,'sqlite','SQLite','searching');
				INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES
				(11,1,'Saved WAL guide','https://sqlite.org/wal.html',1),
				(12,1,'Saved planner guide','https://sqlite.org/queryplanner.html',2);
				INSERT INTO daily_readings(id,topic_id,reading_date,page_id) VALUES(21,1,'2026-06-26',11);
				INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at,result_count,stored_count)
				VALUES(31,1,'fixture','SQLite','running','reviewing','2000-01-01 00:00:00',4,2);
			`); err != nil {
				t.Fatal(err)
			}
			if orphan {
				if _, err := conn.Exec("UPDATE topic_search_runs SET status='failed',stage='',completed_at='2000-01-01 00:01:00',error='source service unavailable' WHERE id=31"); err != nil {
					t.Fatal(err)
				}
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			get := func(path string) *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				return response
			}
			// A saved archive remains readable even before cleanup. The dated
			// endpoint must not invent today's missing assignment.
			archive := get("/sqlite/2026-06-26")
			if archive.Code != http.StatusOK || !strings.Contains(archive.Body.String(), `class="reading-link" href="https://sqlite.org/wal.html"`) {
				t.Fatal("interrupted retry hid the saved archive", archive.Code)
			}
			if missing := get("/sqlite/2026-06-27"); missing.Code != http.StatusNotFound {
				t.Fatal("dated visit created a missing assignment", missing.Code)
			}
			catalog := get("/topics")
			if catalog.Code != http.StatusOK || !strings.Contains(catalog.Body.String(), `href="/sqlite">2</a>`) {
				t.Fatal("catalog lost available source links", catalog.Code)
			}
			var status string
			var assignments int
			if err := conn.QueryRow("SELECT status,(SELECT count(*) FROM daily_readings) FROM topics WHERE id=1").Scan(&status, &assignments); err != nil {
				t.Fatal(err)
			}
			if status != "active" || assignments != 1 {
				t.Fatalf("cleanup hid a usable catalog or created a reading: status=%s assignments=%d", status, assignments)
			}
			today := get("/sqlite")
			if today.Code != http.StatusOK || !strings.Contains(today.Body.String(), `class="reading-link"`) || !strings.Contains(today.Body.String(), `href="/sqlite/2026-06-27">Share or save this reading`) {
				t.Fatal("usable catalog unavailable to a new day's reader", today.Code, today.Body.String())
			}
			feedback := get("/topics/sqlite/status")
			for _, body := range []string{today.Body.String(), feedback.Body.String()} {
				if !strings.Contains(body, "could not finish the latest attempt") || !strings.Contains(body, "Process topic") || strings.Contains(body, "did not finish with enough readings") || strings.Contains(body, "data-on-interval") {
					t.Fatal("cleanup lost honest failure feedback or explicit retry", body)
				}
			}
			// Repeated current/dated/catalog GETs retain the chosen assignment and
			// do not retry the failed attempt or modify source identities.
			get("/sqlite")
			get("/topics")
			archive = get("/sqlite/2026-06-26")
			if archive.Code != http.StatusOK || !strings.Contains(archive.Body.String(), `class="reading-link" href="https://sqlite.org/wal.html"`) {
				t.Fatal("cleanup changed the archived source", archive.Code)
			}
			var pages, runs, archivePage, archiveID, todayRows, results, stored, complete int
			var runStatus, stage, diagnostic, started, source string
			if err := conn.QueryRow(`SELECT
				(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs),
				(SELECT page_id FROM daily_readings WHERE reading_date='2026-06-26'),
				(SELECT id FROM daily_readings WHERE reading_date='2026-06-26'),
				(SELECT count(*) FROM daily_readings WHERE reading_date='2026-06-27' AND page_id IN(11,12)),
				(SELECT url FROM pages WHERE id=11),status,stage,error,started_at,result_count,stored_count,completed_at IS NOT NULL
				FROM topic_search_runs WHERE id=31`).Scan(&pages, &assignments, &runs, &archivePage, &archiveID, &todayRows, &source, &runStatus, &stage, &diagnostic, &started, &results, &stored, &complete); err != nil {
				t.Fatal(err)
			}
			wantDiagnostic := "stale running search timed out"
			if orphan {
				wantDiagnostic = "source service unavailable"
			}
			if pages != 2 || assignments != 2 || runs != 1 || archivePage != 11 || archiveID != 21 || todayRows != 1 || source != "https://sqlite.org/wal.html" || runStatus != "failed" || stage != "" || diagnostic != wantDiagnostic || started != "2000-01-01 00:00:00" || results != 4 || stored != 2 || complete != 1 || provider.calls != 0 {
				t.Fatal("reader recovery changed history or started generation", pages, assignments, runs, archivePage, archiveID, todayRows, source, runStatus, stage, diagnostic, started, results, stored, complete, provider.calls)
			}
		})
	}
}
