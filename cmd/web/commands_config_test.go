package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
)

type commandConfigTransport func(*http.Request) (*http.Response, error)

func (f commandConfigTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type commandConfigHTTP struct{ planner, search, review, closed int }

type commandConfigBody struct {
	io.Reader
	closed *int
}

func (b commandConfigBody) Close() error { *b.closed++; return nil }

func fakeCommandConfigurationHTTP(t *testing.T) *commandConfigHTTP {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "fixture-planner-key")
	t.Setenv("OPENAI_ENDPOINT", "https://command-config.test/responses")
	t.Setenv("TAVILY_ENDPOINT", "https://command-config.test/search")
	counts := &commandConfigHTTP{}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	// Intercept every request, including unexpected destinations. This fixture
	// cannot contact configured services or inherit a real provider transport.
	http.DefaultTransport = commandConfigTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			defer r.Body.Close()
		}
		if r.Method != http.MethodPost || r.URL.Host != "command-config.test" {
			return nil, fmt.Errorf("unexpected request blocked by fixture")
		}
		var payload any
		switch r.URL.Path {
		case "/search":
			counts.search++
			if r.Header.Get("Authorization") != "Bearer fixture-search-key" {
				return nil, fmt.Errorf("fixture search key not trimmed")
			}
			payload = map[string]any{"results": []map[string]string{
				{"title": "Ownership guide", "url": "https://docs.example.org/ownership", "content": "Ownership rules"},
				{"title": "Borrowing guide", "url": "https://docs.example.org/borrowing", "content": "Borrowing rules"},
				{"title": "Lifetime guide", "url": "https://docs.example.org/lifetimes", "content": "Lifetime rules"},
			}}
		case "/responses":
			var request struct {
				Text struct{ Format struct{ Name string } }
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				return nil, err
			}
			var output string
			switch request.Text.Format.Name {
			case "dailydocs_topic_plan":
				counts.planner++
				output = `{"valid_topic":true,"validity_reason":"Fixture subject","topics":[]}`
			case "dailydocs_review":
				counts.review++
				output = `{"results":[{"index":1,"dailydocs_score":90,"page_type":"guide","should_store":true,"reason":"Useful fixture guide"},{"index":2,"dailydocs_score":90,"page_type":"guide","should_store":true,"reason":"Useful fixture guide"},{"index":3,"dailydocs_score":90,"page_type":"guide","should_store":true,"reason":"Useful fixture guide"}]}`
			default:
				return nil, fmt.Errorf("unexpected provider operation blocked by fixture")
			}
			payload = map[string]string{"model": "fixture", "output_text": output}
		default:
			return nil, fmt.Errorf("unexpected endpoint blocked by fixture")
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: commandConfigBody{Reader: strings.NewReader(string(body)), closed: &counts.closed}, Request: r}, nil
	})
	return counts
}

func commandConfigurationSnapshot(t *testing.T, conn *sql.DB) string {
	t.Helper()
	snapshot := map[string][][]any{}
	for _, table := range []string{"topics", "pages", "daily_readings", "topic_search_runs", "topic_search_results", "imports"} {
		rows, err := conn.Query("SELECT * FROM " + table + " ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			snapshot[table] = append(snapshot[table], values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSearchTopicCommandRejectsMissingProviderBeforeWork(t *testing.T) {
	for _, key := range []string{"", " \t\r\n "} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("whitespace=%v/existing=%v", key != "", existing), func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "database", "config.sqlite")
				t.Setenv("DB_PATH", path)
				t.Setenv("TAVILY_API_KEY", key)
				calls := fakeCommandConfigurationHTTP(t)
				var conn *sql.DB
				var before string
				if existing {
					var err error
					conn, err = db.Open(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					importWebTopic(t, ctx, conn, "rust", "Rust")
					seedWebDailyReading(t, ctx, conn, "rust", "2026-06-26", "Write-Ahead Logging")
					if _, err := conn.Exec(`INSERT INTO topic_search_runs(id,topic_id,provider,query,status,started_at,completed_at,error) SELECT 11,id,'fixture','saved attempt','completed','2020-01-01 00:00:00','2020-01-01 00:00:01','retained diagnostic' FROM topics`); err != nil {
						t.Fatal(err)
					}
					before = commandConfigurationSnapshot(t, conn)
				}
				err := runCommand(ctx, []string{"search-topic", "Rust"})
				if err == nil || !strings.Contains(err.Error(), "TAVILY_API_KEY is required") {
					t.Fatal("missing key did not report configuration error", err)
				}
				if calls.planner != 0 || calls.search != 0 || calls.review != 0 {
					t.Errorf("missing key performed HTTP work: %+v", calls)
				}
				if existing {
					if after := commandConfigurationSnapshot(t, conn); after != before {
						t.Error("missing provider configuration changed saved database rows")
					}
					archive, err := reading.GetDailyReading(ctx, conn, "rust", "2026-06-26")
					if err != nil || archive.PageID != 1 || archive.URL != "https://sqlite.org/wal.html" {
						t.Fatal("missing configuration changed saved reading", archive, err)
					}
				} else {
					if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
						t.Errorf("missing key created database directory or files: %v", err)
					}
				}
			})
		}
	}
}

func TestSearchTopicCommandConfiguredProvidersStillComplete(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "configured.sqlite")
	t.Setenv("DB_PATH", path)
	t.Setenv("TAVILY_API_KEY", " \tfixture-search-key\r\n ")
	calls := fakeCommandConfigurationHTTP(t)
	if err := runCommand(ctx, []string{"search-topic", "Rust"}); err != nil {
		t.Fatal("configured command failed", err)
	}
	if calls.planner != 1 || calls.search != 1 || calls.review != 1 || calls.closed != 3 {
		t.Fatalf("configured pipeline or response cleanup changed: %+v", calls)
	}
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var status, runStatus, stage string
	var runs, pages, candidates, accepted, readings int
	if err := conn.QueryRow(`SELECT t.status,r.status,r.stage,
		(SELECT count(*) FROM topic_search_runs),(SELECT count(*) FROM pages WHERE active=1),
		(SELECT count(*) FROM topic_search_results),(SELECT count(*) FROM topic_search_results WHERE accepted=1),
		(SELECT count(*) FROM daily_readings)
		FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.slug='rust'`).Scan(&status, &runStatus, &stage, &runs, &pages, &candidates, &accepted, &readings); err != nil {
		t.Fatal(err)
	}
	if status != "active" || runStatus != "completed" || stage != "" || runs != 1 || pages != 3 || candidates != 3 || accepted != 3 || readings != 0 {
		t.Fatal("configured pipeline did not preserve its normal output", status, runStatus, stage, runs, pages, candidates, accepted, readings)
	}
}
