package main

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestProcessFailureFeedbackOffersReadOnlyRecovery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to execute the process-form feedback behavior")
	}
	for _, archived := range []bool{false, true} {
		name, slug, path := "queued topic", "rust", "/rust"
		if archived {
			name, slug, path = "saved reading", "sqlite", "/sqlite/2026-06-26"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			if archived {
				importWebTopic(t, ctx, conn, slug, "SQLite")
				for _, date := range []string{"2026-06-26", "2026-06-27"} {
					seedWebDailyReading(t, ctx, conn, slug, date, "Write-Ahead Logging")
				}
			} else {
				seedQueuedWebTopic(t, ctx, conn, slug, "Rust")
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			body := response.Body.String()
			if response.Code != http.StatusOK {
				t.Fatal("topic page unavailable", response.Code)
			}
			listener := regexp.MustCompile(`data-on:datastar-fetch__document="([^"]+)"`).FindStringSubmatch(body)
			options := regexp.MustCompile(`@post\('/process-topic', ([^"]+)\)`).FindStringSubmatch(body)
			alert := regexp.MustCompile(`(?s)<p data-process-feedback role="alert" hidden>(.*?)</p>`).FindStringSubmatch(body)
			if len(listener) != 2 || len(options) != 2 || len(alert) != 2 {
				t.Fatal("process form lacks document-scoped failure feedback or initially hidden alert")
			}
			link := regexp.MustCompile(`<a href="([^"]+)">Check topic status</a>`).FindStringSubmatch(alert[1])
			if len(link) != 2 || !strings.Contains(alert[1], "before submitting again") {
				t.Fatal("uncertain request needs explicit status recovery before retry", alert[1])
			}
			command := exec.CommandContext(ctx, node, "testdata/process-topic-feedback.cjs", html.UnescapeString(options[1]), html.UnescapeString(listener[1]))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("process feedback behavior: %v\n%s", err, output)
			} else {
				t.Logf("%s", output)
			}
			location, err := url.Parse(html.UnescapeString(link[1]))
			if err != nil || location.Scheme != "" || location.Host != "" || location.Path != "/"+slug || location.RawQuery != "" {
				t.Fatal("recovery link must be the internal topic GET", location, err)
			}
			recovery := httptest.NewRecorder()
			handler.ServeHTTP(recovery, httptest.NewRequest(http.MethodGet, location.String(), nil))
			if recovery.Code != http.StatusOK {
				t.Fatal("GET recovery unavailable", recovery.Code)
			}
			var topics, pages, assignments, runs int
			if err := conn.QueryRow("SELECT (SELECT count(*) FROM topics),(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs)").Scan(&topics, &pages, &assignments, &runs); err != nil {
				t.Fatal(err)
			}
			wantPages, wantAssignments := 0, 0
			if archived {
				wantPages, wantAssignments = 1, 2
				var historicalPage int
				if err := conn.QueryRow("SELECT page_id FROM daily_readings WHERE reading_date='2026-06-26'").Scan(&historicalPage); err != nil || historicalPage != 1 {
					t.Fatal("GET recovery changed the saved archive", historicalPage, err)
				}
				if !strings.Contains(recovery.Body.String(), `class="reading-link" href="https://sqlite.org/wal.html"`) {
					t.Fatal("recovery hid the saved current reading")
				}
			}
			if topics != 1 || pages != wantPages || assignments != wantAssignments || runs != 0 || provider.calls != 0 {
				t.Fatal("error recovery changed saved state or started generation", topics, pages, assignments, runs, provider.calls)
			}
		})
	}
}
