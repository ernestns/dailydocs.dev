package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDisabledTopicKeepsLinksAndHistoryWithoutOfferingGeneration(t *testing.T) {
	for _, activeCount := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("%d_active_readings", activeCount), func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			if _, err := conn.Exec(`
				INSERT INTO topics(id,slug,name,status) VALUES(1,'sqlite','SQLite','disabled');
				INSERT INTO pages(id,topic_id,title,url,reading_order,active) VALUES(1,1,'Saved historical reading','https://docs.example.org/history',1,0);
				INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',1);
				INSERT INTO topic_search_runs(topic_id,provider,query,status,error) VALUES(1,'fake','prior request','failed','source unavailable');
			`); err != nil {
				t.Fatal(err)
			}
			for i := range activeCount {
				if _, err := conn.Exec("INSERT INTO pages(topic_id,title,url,reading_order) VALUES(1,?,?,?)", fmt.Sprintf("Saved link %d", i), fmt.Sprintf("https://docs.example.org/guide-%d", i), i+2); err != nil {
					t.Fatal(err)
				}
			}
			p := &quantityProvider{count: 3}
			handler := newTestHandlerWithProvider(conn, p)
			for _, path := range []string{"/sqlite", "/topics/sqlite/status"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				body := response.Body.String()
				if response.Code != http.StatusOK || !strings.Contains(body, "Topic disabled") || !strings.Contains(body, "New readings and topic processing are unavailable") {
					t.Errorf("%s must explain disabled state: HTTP %d, disabled label=%v, explanation=%v", path, response.Code, strings.Contains(body, "Topic disabled"), strings.Contains(body, "New readings and topic processing are unavailable"))
				}
				for _, misleading := range []string{"queued", "Try again", "try again", "Process topic", "data-on-interval", "action=\"/process-topic\""} {
					if strings.Contains(body, misleading) {
						t.Errorf("%s advertises unavailable generation: %q", path, misleading)
					}
				}
				for i := range activeCount {
					if !strings.Contains(body, fmt.Sprintf(`href="https://docs.example.org/guide-%d"`, i)) {
						t.Errorf("%s hid saved active link %d", path, i)
					}
				}
				if strings.Contains(body, `href="https://docs.example.org/history"`) {
					t.Errorf("%s reintroduced an inactive page into the active links", path)
				}
			}
			archived := httptest.NewRecorder()
			handler.ServeHTTP(archived, httptest.NewRequest(http.MethodGet, "/sqlite/2026-06-26", nil))
			if archived.Code != http.StatusOK || !strings.Contains(archived.Body.String(), `href="https://docs.example.org/history"`) || !strings.Contains(archived.Body.String(), "Saved historical reading") {
				t.Fatal("disabled topic lost its saved historical reading", archived.Code)
			}
			for _, path := range []string{"/read", "/process-topic"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, topicRequest(http.MethodPost, path, "sqlite"))
				if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/sqlite" {
					t.Errorf("%s changed disabled topic request behavior: %d %s", path, response.Code, response.Header().Get("Location"))
				}
			}
			var status string
			var pages, runs, assignments, archivedPageID int
			if err := conn.QueryRow(`SELECT status,
				(SELECT COUNT(*) FROM pages),
				(SELECT COUNT(*) FROM topic_search_runs),
				(SELECT COUNT(*) FROM daily_readings),
				(SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-06-26')
				FROM topics WHERE id=1`).Scan(&status, &pages, &runs, &assignments, &archivedPageID); err != nil {
				t.Fatal(err)
			}
			if status != "disabled" || pages != activeCount+1 || runs != 1 || assignments != 1 || archivedPageID != 1 || p.calls != 0 {
				t.Fatalf("disabled topic changed: status=%s pages=%d runs=%d assignments=%d archived_page=%d provider_calls=%d", status, pages, runs, assignments, archivedPageID, p.calls)
			}
		})
	}
}
