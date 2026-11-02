package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/validator"
)

type unavailableReadingTransport struct{}

func (unavailableReadingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("not found")),
		Request:    request,
	}, nil
}

func TestEmptyActiveCatalogDoesNotOfferUnavailableReading(t *testing.T) {
	for _, tc := range []struct {
		name                string
		disable, savedToday bool
	}{
		{"no current reading", true, false},
		{"saved current reading", true, true},
		{"one available reading", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			importWebTopic(t, ctx, conn, "sqlite", "SQLite")
			seedWebDailyReading(t, ctx, conn, "sqlite", "2026-06-26", "Write-Ahead Logging")
			if tc.savedToday {
				seedWebDailyReading(t, ctx, conn, "sqlite", "2026-06-27", "Write-Ahead Logging")
			}
			if tc.disable {
				// Real link validation can disable the last page without changing
				// the topic's active status. This transport never makes a request.
				client := &http.Client{Transport: unavailableReadingTransport{}}
				for range validator.DefaultFailureThreshold {
					if _, err := validator.ValidateLinks(ctx, conn, client, validator.DefaultFailureThreshold); err != nil {
						t.Fatal(err)
					}
				}
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			for _, path := range []string{"/sqlite", "/topics/sqlite/status"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				body := response.Body.String()
				if response.Code != http.StatusOK || !strings.Contains(body, "Needs more readings") || !strings.Contains(body, "Process topic") {
					t.Fatal("empty/partial catalog lost recovery", path, response.Code, body)
				}
				if strings.Contains(body, ">View reading</a>") == tc.disable {
					t.Fatalf("View reading availability disagrees with the catalog: disabled=%v path=%s", tc.disable, path)
				}
				if path == "/sqlite" && (tc.savedToday || !tc.disable) && !strings.Contains(body, `class="reading-link" href="https://sqlite.org/wal.html"`) {
					t.Fatal("saved/current reading was hidden", path)
				}
			}
			archived := httptest.NewRecorder()
			handler.ServeHTTP(archived, httptest.NewRequest(http.MethodGet, "/sqlite/2026-06-26", nil))
			if archived.Code != http.StatusOK || !strings.Contains(archived.Body.String(), `class="reading-link" href="https://sqlite.org/wal.html"`) {
				t.Fatal("link validation or presentation lost the saved reading", archived.Code)
			}
			wantActive, wantAssignments := 0, 1
			if !tc.disable {
				wantActive = 1
			}
			if tc.savedToday || !tc.disable {
				wantAssignments = 2
			}
			var status string
			var pages, active, assignments, historicalPage, runs int
			if err := conn.QueryRowContext(ctx, `SELECT status,
				(SELECT count(*) FROM pages), (SELECT count(*) FROM pages WHERE active=1),
				(SELECT count(*) FROM daily_readings), (SELECT page_id FROM daily_readings WHERE reading_date='2026-06-26'),
				(SELECT count(*) FROM topic_search_runs) FROM topics WHERE slug='sqlite'
			`).Scan(&status, &pages, &active, &assignments, &historicalPage, &runs); err != nil {
				t.Fatal(err)
			}
			if status != "active" || pages != 1 || active != wantActive || assignments != wantAssignments || historicalPage != 1 || runs != 0 || provider.calls != 0 {
				t.Fatal("presentation changed saved data or generated", status, pages, active, assignments, historicalPage, runs, provider.calls)
			}
		})
	}
}
