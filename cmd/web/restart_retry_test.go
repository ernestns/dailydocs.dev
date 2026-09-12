package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func TestUnownedRunningTopicKeepsGuardWithoutPolling(t *testing.T) {
	for _, tc := range []struct {
		status string
		saved  int
	}{{"searching", 0}, {"searching", 1}, {"searching", 2}, {"active", 2}} {
		t.Run(fmt.Sprintf("%s_saved_%d", tc.status, tc.saved), func(t *testing.T) {
			saved := tc.saved
			ctx := context.Background()
			date := time.Now().UTC()
			now := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, time.UTC)
			started := now.Add(-time.Minute).Format("2006-01-02 15:04:05")
			yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
			path := filepath.Join(t.TempDir(), "restart.sqlite")
			conn, err := db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			seedWebTopic(t, ctx, conn, "rust", "Rust", tc.status)
			if _, err := conn.Exec(`INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at,result_count,stored_count) VALUES(1,1,'fake','Rust docs','running','reviewing',?,3,?)`, started, saved); err != nil {
				t.Fatal(err)
			}
			for i := range saved {
				if _, err := conn.Exec(`INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(?,1,?,?,?)`, i+1, fmt.Sprintf("Saved Rust reading %d", i), fmt.Sprintf("https://doc.rust-lang.org/saved-%d", i), i+1); err != nil {
					t.Fatal(err)
				}
			}
			if saved > 0 {
				for _, date := range []string{yesterday, now.Format("2006-01-02")} {
					seedWebDailyReading(t, ctx, conn, "rust", date, "Saved Rust reading 0")
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			conn, err = db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			p := &quantityProvider{count: 3}
			a := app{db: conn, now: func() time.Time { return now }, searchMu: &sync.Mutex{}, pendingSearches: &sync.Map{}, searchProvider: p}
			mux := http.NewServeMux()
			mux.HandleFunc("/topics/", a.topicEvaluationsHandler)
			mux.HandleFunc("/process-topic", a.processTopicHandler)
			mux.HandleFunc("/read", a.generateReadingHandler)
			mux.HandleFunc("/", a.routeHandler)
			checkResponse := func(response *httptest.ResponseRecorder, wantCode int, blocked bool) {
				t.Helper()
				body := response.Body.String()
				if response.Code != wantCode || strings.Contains(body, "Retry temporarily unavailable") != blocked || !strings.Contains(body, "Process topic") {
					t.Fatalf("retry guidance: HTTP=%d blocked=%v body=%s", response.Code, blocked, body)
				}
				for _, forbidden := range []string{"data-on-interval", "being processed", "Waiting for worker", `class="status">reviewing`} {
					if strings.Contains(body, forbidden) {
						t.Fatalf("unowned request claims live work: %s", body)
					}
				}
				if saved > 0 && !strings.Contains(body, "Saved Rust reading 0") && !strings.Contains(body, "View reading") {
					t.Fatal("saved reading hidden", body)
				}
			}
			checkGETs := func(blocked bool) {
				t.Helper()
				if _, err := conn.Exec("PRAGMA query_only=ON"); err != nil {
					t.Fatal(err)
				}
				paths := []string{"/rust", "/topics/rust/status"}
				if saved > 0 {
					paths = append(paths, "/rust/"+yesterday)
				}
				for _, path := range paths {
					response := httptest.NewRecorder()
					mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
					checkResponse(response, http.StatusOK, blocked)
				}
				if _, err := conn.Exec("PRAGMA query_only=OFF"); err != nil {
					t.Fatal(err)
				}
			}
			checkUnchangedRun := func() {
				t.Helper()
				var topicStatus, runStatus, stage, runStart, diagnostic string
				var runs, results, stored, unfinished int
				if err := conn.QueryRow(`SELECT t.status,r.status,r.stage,r.started_at,r.error,r.result_count,r.stored_count,r.completed_at IS NULL,(SELECT count(*) FROM topic_search_runs) FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE r.id=1`).Scan(&topicStatus, &runStatus, &stage, &runStart, &diagnostic, &results, &stored, &unfinished, &runs); err != nil {
					t.Fatal(err)
				}
				if topicStatus != tc.status || runStatus != "running" || stage != "reviewing" || runStart != started || diagnostic != "" || results != 3 || stored != saved || unfinished != 1 || runs != 1 || p.calls != 0 {
					t.Fatal("unowned run changed or provider called", topicStatus, runStatus, stage, runStart, diagnostic, results, stored, unfinished, runs, p.calls)
				}
				a.pendingSearches.Range(func(key, value any) bool {
					t.Error("unowned request created pending work", key, value)
					return true
				})
			}
			checkGETs(true)
			checkUnchangedRun()
			for _, datastar := range []bool{false, true} {
				post := topicRequest(http.MethodPost, "/process-topic", "rust")
				wantCode := http.StatusConflict
				if datastar {
					post.Header.Set("Datastar-Request", "true")
					wantCode = http.StatusOK
				}
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, post)
				checkResponse(response, wantCode, true)
				if response.Header().Get("Location") != "" {
					t.Fatal("blocked retry silently redirected", response.Header())
				}
				checkUnchangedRun()
			}
			readPost := httptest.NewRecorder()
			mux.ServeHTTP(readPost, topicRequest(http.MethodPost, "/read", "Rust"))
			if tc.status == "active" {
				if readPost.Code != http.StatusSeeOther || readPost.Header().Get("Location") != "/rust" {
					t.Fatal("named active lookup became a retry", readPost.Code, readPost.Header())
				}
			} else {
				checkResponse(readPost, http.StatusConflict, true)
			}
			checkUnchangedRun()
			now = now.Add(topicsearch.StaleRunTimeout)
			checkGETs(false)
			checkUnchangedRun()
			retry := httptest.NewRecorder()
			mux.ServeHTTP(retry, topicRequest(http.MethodPost, "/process-topic", "rust"))
			if retry.Code != http.StatusSeeOther || retry.Header().Get("Location") != "/rust" || p.calls != 1 {
				t.Fatal("explicit retry after stale timeout did not run", retry.Code, retry.Header(), p.calls, retry.Body.String())
			}
			var oldStatus, oldError, latestStatus, topicStatus string
			var runs, oldStored int
			if err := conn.QueryRow(`SELECT r.status,r.error,r.stored_count,t.status,(SELECT status FROM topic_search_runs ORDER BY id DESC LIMIT 1),(SELECT count(*) FROM topic_search_runs) FROM topic_search_runs r JOIN topics t ON t.id=r.topic_id WHERE r.id=1`).Scan(&oldStatus, &oldError, &oldStored, &topicStatus, &latestStatus, &runs); err != nil {
				t.Fatal(err)
			}
			if oldStatus != "failed" || oldError != "stale running search timed out" || oldStored != saved || topicStatus != "active" || latestStatus != "completed" || runs != 2 {
				t.Fatal("existing expiration/retry behavior changed", oldStatus, oldError, oldStored, topicStatus, latestStatus, runs)
			}
			var preserved int
			if err := conn.QueryRow(`SELECT count(*) FROM pages WHERE id<=? AND topic_id=1 AND active=1 AND url LIKE 'https://doc.rust-lang.org/saved-%'`, saved).Scan(&preserved); err != nil || preserved != saved {
				t.Fatal("saved page identities changed", preserved, err)
			}
			if saved > 0 {
				var pageID int
				if err := conn.QueryRow(`SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date=?`, yesterday).Scan(&pageID); err != nil || pageID != 1 {
					t.Fatal("historical assignment changed", pageID, err)
				}
			}
			complete := httptest.NewRecorder()
			mux.ServeHTTP(complete, httptest.NewRequest(http.MethodGet, "/topics/rust/status", nil))
			if complete.Code != http.StatusOK || strings.Contains(complete.Body.String(), "data-on-interval") || strings.Contains(complete.Body.String(), "Retry temporarily unavailable") || !strings.Contains(complete.Body.String(), "View reading") {
				t.Fatal("completed retry retained processing or retry guard", complete.Code, complete.Body.String())
			}
		})
	}
}

type blockingStatusReviewer struct{ started, release chan struct{} }

func (r *blockingStatusReviewer) Review(ctx context.Context, _ string, candidates []topicsearch.ReviewCandidate) (topicsearch.ReviewOutput, error) {
	close(r.started)
	select {
	case <-ctx.Done():
		return topicsearch.ReviewOutput{}, ctx.Err()
	case <-r.release:
	}
	var output topicsearch.ReviewOutput
	for _, candidate := range candidates {
		output.Results = append(output.Results, topicsearch.ReviewResult{Index: candidate.Index, DailyDocsScore: 90, ShouldStore: true})
	}
	return output, nil
}

func waitForWebRequestCompletion(t *testing.T, a app, slug string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, pending := a.pendingSearches.Load(slug); !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not finish", slug)
		}
		time.Sleep(time.Millisecond)
	}
}
