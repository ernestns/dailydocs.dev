package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/seed"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

type importedReadingState struct {
	PageID, Order, AssignmentID, AssignedPage int64
	Title, URL, Date                          string
	Active                                    bool
}

func importedReadingStates(t *testing.T, conn *sql.DB, topicID int64) []importedReadingState {
	t.Helper()
	rows, err := conn.Query(`SELECT p.id,p.reading_order,d.id,d.page_id,p.title,p.url,d.reading_date,p.active FROM pages p JOIN daily_readings d ON d.page_id=p.id AND d.topic_id=p.topic_id WHERE p.topic_id=? ORDER BY p.id`, topicID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var saved []importedReadingState
	for rows.Next() {
		var row importedReadingState
		if err := rows.Scan(&row.PageID, &row.Order, &row.AssignmentID, &row.AssignedPage, &row.Title, &row.URL, &row.Date, &row.Active); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestImportedURLNormalizationControlsReadiness(t *testing.T) {
	const wal = "https://sqlite.org/wal.html"
	for _, tc := range []struct {
		name string
		urls []string
		want int
	}{
		{"tracking only", []string{wal, wal + "?utm_source=feed"}, 1},
		{"normalization and verified alias", []string{wal, "http://WWW.sqlite.org/wal.html/?UTM_SOURCE=feed#top"}, 1},
		{"meaningful queries", []string{wal + "?view=full&utm_source=feed", wal + "?view=short"}, 2},
		{"meaningful fragments", []string{wal + "?utm_source=feed#one", wal + "#two"}, 2},
		{"three distinct readings", []string{wal, wal + "?view=full", wal + "#checkpointing"}, 3},
		{"opaque queries", []string{wal + "?section=a;b&utm_source=feed#examples", wal + "?section=a;b#examples"}, 2},
		{"unrelated hosts", []string{"https://docs.example.org/wal", "https://www.docs.example.org/wal"}, 2},
		{"unrelated schemes", []string{"https://docs.example.org/wal", "http://docs.example.org/wal"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			seedCatalog(t, conn, 50)
			fixture := seed.TopicFile{Topic: "sqlite", Name: "Z SQLite"}
			for i, raw := range tc.urls {
				fixture.Pages = append(fixture.Pages, seed.PageFile{Title: fmt.Sprintf("Saved reading %d", i+1), URL: raw})
			}
			imported, err := seed.ImportTopic(ctx, conn, fixture)
			if err != nil || imported.PagesImported != len(tc.urls) {
				t.Fatal("import fixture failed", imported, err)
			}
			var topicID int64
			if err := conn.QueryRow(`SELECT id FROM topics WHERE slug='sqlite'`).Scan(&topicID); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(`INSERT INTO daily_readings(topic_id,reading_date,page_id) SELECT topic_id,date('2026-06-01','+' || (reading_order-1) || ' days'),id FROM pages WHERE topic_id=?`, topicID); err != nil {
				t.Fatal(err)
			}
			before := importedReadingStates(t, conn, topicID)
			if len(before) != len(tc.urls) {
				t.Fatal("fixture lost imported rows or assignments", before)
			}
			if got, err := topicsearch.AvailableReadings(ctx, conn, topicID); err != nil || got != tc.want {
				t.Fatalf("imported availability=%d want=%d err=%v", got, tc.want, err)
			}
			page, err := listRequestedTopics(ctx, conn, topicCatalogFilter{Page: 2})
			if err != nil || page.Total != 51 || len(page.Topics) != 1 || page.Topics[0].Slug != "sqlite" || page.Topics[0].ReadingCount != tc.want {
				t.Fatal("paginated catalog used different reading identities", page, err)
			}
			provider := &quantityProvider{count: 0}
			handler := newTestHandlerWithProvider(conn, provider)
			retryable := tc.want < topicsearch.MinimumUsefulResults
			for _, path := range []string{"/topics?page=2", "/sqlite/2026-06-01", "/topics/sqlite/status"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusOK {
					t.Fatal(path, response.Code, response.Body.String())
				}
				body := response.Body.String()
				if strings.Contains(body, "Needs more readings") != retryable {
					t.Fatal("rendered readiness disagrees with distinct readings", path, body)
				}
				if path != "/topics?page=2" && strings.Contains(body, "Process topic") != retryable {
					t.Fatal("retry action disagrees with distinct readings", path, body)
				}
			}
			if provider.calls != 0 {
				t.Fatal("viewing imported readings called the provider")
			}
			post := httptest.NewRecorder()
			handler.ServeHTTP(post, topicRequest(http.MethodPost, "/process-topic", "sqlite"))
			wantCalls := 0
			if retryable {
				wantCalls = 1
			}
			if post.Code != http.StatusSeeOther || provider.calls != wantCalls {
				t.Fatal("processing admission disagrees with distinct readings", post.Code, provider.calls, wantCalls)
			}
			after := importedReadingStates(t, conn, topicID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("normalization rewrote saved rows or assignments: before=%+v after=%+v", before, after)
			}
			var rows int
			if err := conn.QueryRow(`SELECT count(*) FROM pages WHERE topic_id=?`, topicID).Scan(&rows); err != nil || rows != len(tc.urls) {
				t.Fatal("normalization deleted or padded imported pages", rows, err)
			}
		})
	}
}
