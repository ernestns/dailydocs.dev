package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestEvaluationsLinkBackToCurrentTopicWithoutProcessing(t *testing.T) {
	for _, status := range []string{"active", "queued", "failed"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			if status == "active" {
				importWebTopic(t, ctx, conn, "sqlite", "SQLite")
			} else if _, err := conn.ExecContext(ctx, `INSERT INTO topics(slug,name,status) VALUES('sqlite','SQLite',?)`, status); err != nil {
				t.Fatal(err)
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/sqlite/evaluations", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("evaluations: %d %s", response.Code, response.Body.String())
			}
			link := regexp.MustCompile(`<a[^>]*href="([^"]+)"[^>]*>View topic</a>`).FindStringSubmatch(response.Body.String())
			if len(link) != 2 || link[1] != "/sqlite" {
				t.Fatalf("evaluations need a direct current-topic link: %s", response.Body.String())
			}
			current := httptest.NewRecorder()
			handler.ServeHTTP(current, httptest.NewRequest(http.MethodGet, link[1], nil))
			if current.Code != http.StatusOK {
				t.Fatalf("current topic: %d %s", current.Code, current.Body.String())
			}
			want := "Process topic"
			if status == "active" {
				want = "Write-Ahead Logging"
			}
			if !strings.Contains(current.Body.String(), want) {
				t.Fatalf("topic navigation did not reach expected reading/status: %s", current.Body.String())
			}
			if provider.calls != 0 {
				t.Fatalf("viewing topic evaluations called provider %d times", provider.calls)
			}
			var storedStatus string
			var runs int
			if err := conn.QueryRowContext(ctx, `SELECT status, (SELECT COUNT(*) FROM topic_search_runs) FROM topics WHERE slug='sqlite'`).Scan(&storedStatus, &runs); err != nil {
				t.Fatal(err)
			}
			if storedStatus != status || runs != 0 {
				t.Fatalf("navigation changed generation state: status=%s runs=%d", storedStatus, runs)
			}
		})
	}
}

func TestEvaluationTableProvidesKeyboardRegionAndColumnAssociations(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "sqlite", "SQLite")
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topic_search_runs(topic_id,provider,query,status) VALUES(1,'fake','SQLite','failed');
		INSERT INTO topic_search_results(topic_id,search_run_id,title,url,source,rank)
		VALUES(1,1,'Write-Ahead Logging','https://sqlite.org/wal.html','sqlite.org',1)
	`); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	newTestHandler(conn).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/sqlite/evaluations", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("evaluations: %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `role="region" aria-label="Evaluated pages" tabindex="0"`) {
		t.Fatal("wide evaluation content needs a named, keyboard-focusable scrolling region")
	}
	for _, heading := range []string{"Result", "Decision", "Score", "Type", "Reason"} {
		if !strings.Contains(body, `<th scope="col">`+heading+`</th>`) {
			t.Fatalf("%s column needs an explicit table header association", heading)
		}
	}
	if !strings.Contains(body, `href="https://sqlite.org/wal.html"`) || !strings.Contains(body, "Not reviewed") {
		t.Fatal("accessible table must preserve candidate links and review decisions")
	}
}
