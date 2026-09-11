package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ernestns/daily-docs/internal/topicsearch"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
)

type aliasCLIRoundTrip func(*http.Request) (*http.Response, error)

func (f aliasCLIRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCLIAliasRefreshPreservesReadingIdentity(t *testing.T) {
	for _, secondURL := range []string{"https://www.sqlite.org/eqp.html", "https://sqlite.org/eqp.html", "http://www.sqlite.org/eqp.html"} {
		t.Run(secondURL, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sqlite.db")
			t.Setenv("DB_PATH", path)
			t.Setenv("TAVILY_API_KEY", "fake-test-key")
			t.Setenv("OPENAI_API_KEY", "fake-test-key")
			t.Setenv("TAVILY_ENDPOINT", "http://fake.invalid/search")
			t.Setenv("OPENAI_ENDPOINT", "http://fake.invalid/responses")
			original := http.DefaultTransport
			defer func() { http.DefaultTransport = original }()
			generation := 0
			calls := 0
			http.DefaultTransport = aliasCLIRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "fake.invalid" {
					return nil, fmt.Errorf("blocked non-fake endpoint %s", r.URL)
				}
				calls++
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					return nil, err
				}
				var response any
				if r.URL.Path == "/search" {
					u := "https://sqlite.org/eqp.html"
					if generation == 1 {
						u = secondURL
					}
					response = map[string]any{"results": []any{map[string]any{"title": "EXPLAIN QUERY PLAN", "url": u, "content": "Query plan documentation", "score": 0.95}}}
				} else {
					format := req["text"].(map[string]any)["format"].(map[string]any)
					output := `{"topics":[{"name":"Query plans","category":"performance","reason":"Useful","search_queries":["SQLite EXPLAIN QUERY PLAN"],"include_domains":["sqlite.org"],"expected_terms":["EXPLAIN"]}]}`
					if format["name"] == "dailydocs_review" {
						output = `{"results":[{"index":1,"dailydocs_score":90,"page_type":"guide","should_store":true,"reason":"A focused query plan guide."}]}`
					}
					response = map[string]any{"status": "completed", "model": "fake-model", "output_text": output}
				}
				raw, err := json.Marshal(response)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}, nil
			})
			if err := runCommand(context.Background(), []string{"search-topic", "SQLite"}); !errors.Is(err, topicsearch.ErrInsufficientResults) {
				t.Fatal(err)
			}
			conn, err := db.Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var pageID, readingOrder, candidateID int64
			if err := conn.QueryRow("SELECT id,reading_order FROM pages").Scan(&pageID, &readingOrder); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRow("SELECT id FROM topic_search_results").Scan(&candidateID); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec("INSERT INTO daily_readings(topic_id,reading_date,page_id) SELECT topic_id,'2026-09-09',id FROM pages"); err != nil {
				t.Fatal(err)
			}
			// Simulate waiting longer than the documented search cooldown, without sleeping.
			if _, err := conn.Exec("UPDATE topic_search_runs SET started_at=datetime('now','-6 minutes')"); err != nil {
				t.Fatal(err)
			}
			generation = 1
			if err := runCommand(context.Background(), []string{"search-topic", "SQLite"}); !errors.Is(err, topicsearch.ErrInsufficientResults) {
				t.Fatal(err)
			}
			rows, err := conn.Query("SELECT url FROM pages WHERE active=1 ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var urls []string
			for rows.Next() {
				var u string
				if err := rows.Scan(&u); err != nil {
					t.Fatal(err)
				}
				urls = append(urls, u)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Logf("supported invocation=search-topic SQLite twice; second_alias=%s fake_calls=%d active_pages=%d urls=%v", secondURL, calls, len(urls), urls)
			if len(urls) != 1 || urls[0] != "https://sqlite.org/eqp.html" || calls != 6 {
				t.Fatalf("alias refresh duplicated/downgraded existing reading: %v; calls=%d", urls, calls)
			}
			var finalPage, finalOrder, assignedPage, finalCandidate, linkedPage int64
			if err := conn.QueryRow("SELECT p.id,p.reading_order,d.page_id FROM pages p JOIN daily_readings d ON d.page_id=p.id").Scan(&finalPage, &finalOrder, &assignedPage); err != nil {
				t.Fatal(err)
			}
			if finalPage != pageID || assignedPage != pageID || finalOrder != readingOrder {
				t.Fatal("alias refresh changed historical identity/order", finalPage, assignedPage, finalOrder)
			}
			var candidates int
			if err := conn.QueryRow("SELECT count(*) FROM topic_search_results").Scan(&candidates); err != nil {
				t.Fatal(err)
			}
			if candidates != 1 {
				t.Fatalf("alias refresh added %d candidates", candidates)
			}
			if err := conn.QueryRow("SELECT id,stored_as_page_id FROM topic_search_results").Scan(&finalCandidate, &linkedPage); err != nil {
				t.Fatal(err)
			}
			if finalCandidate != candidateID || linkedPage != pageID {
				t.Fatal("candidate identity/link changed", finalCandidate, linkedPage)
			}
		})
	}
}
