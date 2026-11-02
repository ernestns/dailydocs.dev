package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebMissingSearchKeyPreservesRequestsAndReadings(t *testing.T) {
	for _, key := range []string{"", " \t\r\n "} {
		t.Run(fmt.Sprintf("whitespace=%v", key != ""), func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			calls := fakeCommandConfigurationHTTP(t)
			t.Setenv("TAVILY_API_KEY", key)
			importWebTopic(t, ctx, conn, "sqlite", "SQLite")
			seedWebDailyReading(t, ctx, conn, "sqlite", "2026-06-26", "Write-Ahead Logging")
			before := commandConfigurationSnapshot(t, conn)
			a := app{db: conn, now: time.Now, searchProvider: tavilyProviderFromEnv(), searchPlanner: openAIPlannerFromEnv(), searchReviewer: openAIReviewerFromEnv()}
			archive := httptest.NewRecorder()
			a.routeHandler(archive, httptest.NewRequest(http.MethodGet, "/sqlite/2026-06-26", nil))
			if archive.Code != http.StatusOK || !strings.Contains(archive.Body.String(), "Write-Ahead Logging") || !strings.Contains(archive.Body.String(), "Generation temporarily unavailable") || strings.Contains(archive.Body.String(), "Process topic") {
				t.Fatalf("unconfigured search must preserve reading and explain pause: %d %s", archive.Code, archive.Body.String())
			}
			process := httptest.NewRecorder()
			a.processTopicHandler(process, topicRequest(http.MethodPost, "/process-topic", "sqlite"))
			if process.Code != http.StatusSeeOther || commandConfigurationSnapshot(t, conn) != before {
				t.Fatal("unconfigured process request changed saved catalog or history", process.Code)
			}
			request := httptest.NewRecorder()
			a.generateReadingHandler(request, topicRequest(http.MethodPost, "/read", "Rust"))
			var queued, runs int
			if err := conn.QueryRow(`SELECT (SELECT count(*) FROM topics WHERE slug='rust' AND status='queued'),(SELECT count(*) FROM topic_search_runs)`).Scan(&queued, &runs); err != nil {
				t.Fatal(err)
			}
			if request.Code != http.StatusSeeOther || queued != 1 || runs != 0 || calls.planner != 0 || calls.search != 0 || calls.review != 0 {
				t.Fatalf("unconfigured request must remain saved without provider work: status=%d queued=%d runs=%d calls=%+v", request.Code, queued, runs, calls)
			}
		})
	}
}

func TestWebOptionalOpenAIConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, key  string
		modelCalls int
	}{
		{"empty", "", 0},
		{"whitespace", " \t\r\n ", 0},
		{"configured", " \tfixture-planner-key\r\n ", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			calls := fakeCommandConfigurationHTTP(t)
			t.Setenv("TAVILY_API_KEY", " \tfixture-search-key\r\n ")
			t.Setenv("OPENAI_API_KEY", tc.key)
			a := app{db: conn, now: time.Now, searchProvider: tavilyProviderFromEnv(), searchPlanner: openAIPlannerFromEnv(), searchReviewer: openAIReviewerFromEnv()}
			request := httptest.NewRecorder()
			a.generateReadingHandler(request, topicRequest(http.MethodPost, "/read", "Rust"))
			if request.Code != http.StatusSeeOther || calls.planner != tc.modelCalls || calls.search != 1 || calls.review != tc.modelCalls || calls.closed != 1+2*tc.modelCalls {
				t.Fatalf("configured search failed or optional model behavior changed: status=%d calls=%+v", request.Code, calls)
			}
			var status, runStatus string
			var pages, runs int
			if err := conn.QueryRow(`SELECT t.status,r.status,(SELECT count(*) FROM pages WHERE active=1),(SELECT count(*) FROM topic_search_runs) FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.slug='rust'`).Scan(&status, &runStatus, &pages, &runs); err != nil {
				t.Fatal(err)
			}
			if status != "active" || runStatus != "completed" || pages != 3 || runs != 1 {
				t.Fatal("request failed to create a useful catalog", status, runStatus, pages, runs)
			}
		})
	}
}
