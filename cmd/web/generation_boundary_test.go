package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func TestTopicGETDoesNotGenerate(t *testing.T) {
	for _, path := range []string{"/rust", "/wp-admin", "/phpinfo", "/webmail", "/rust/2026-06-27", "/read?topic=Rust", "/read?topic=wp-admin"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code == http.StatusSeeOther {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, response.Header().Get("Location"), nil))
			}
			if provider.calls != 0 {
				t.Fatalf("GET called provider %d times", provider.calls)
			}
			for _, table := range []string{"topics", "topic_search_runs", "topic_search_results", "pages", "daily_readings"} {
				var count int
				if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Errorf("GET created %d rows in %s", count, table)
				}
			}
		})
	}
}

func TestQueuedTopicGETDoesNotStartProcessing(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
	provider := &countingWebProvider{}
	handler := newTestHandlerWithProvider(conn, provider)
	for _, path := range []string{"/rust", "/read?topic=rust"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if provider.calls != 0 {
		t.Fatalf("GET started queued processing: %d calls", provider.calls)
	}
	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM topics WHERE slug='rust'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("GET changed status to %s", status)
	}
}

type countingWebProvider struct{ calls int }

func (p *countingWebProvider) Search(context.Context, string, int) ([]topicsearch.SearchResult, error) {
	p.calls++
	return []topicsearch.SearchResult{{Title: "Generics", URL: "https://doc.rust-lang.org/book/ch10-00-generics.html"}}, nil
}
