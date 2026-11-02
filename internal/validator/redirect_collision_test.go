package validator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/seed"
)

func TestRedirectCollisionsPreserveHistoryAndContinueValidation(t *testing.T) {
	for _, existingDestination := range []bool{false, true} {
		t.Run(fmt.Sprintf("destination_exists_%v", existingDestination), func(t *testing.T) {
			ctx := context.Background()
			conn := openValidatorTestDB(t, ctx)
			defer conn.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/old-a", "/old-b":
					http.Redirect(w, r, "/current", http.StatusMovedPermanently)
				case "/broken":
					w.WriteHeader(http.StatusNotFound)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			pages := []seed.PageFile{
				{Title: "First alias", URL: server.URL + "/old-a"},
				{Title: "Second alias", URL: server.URL + "/old-b"},
				{Title: "Broken reference", URL: server.URL + "/broken"},
			}
			if existingDestination {
				pages = append(pages, seed.PageFile{Title: "Current reference", URL: server.URL + "/current"})
			}
			if _, err := seed.ImportTopic(ctx, conn, seed.TopicFile{Topic: "http", Name: "HTTP", Pages: pages}); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(`
				INSERT INTO daily_readings(topic_id,reading_date,page_id) SELECT topic_id,date('2026-01-01','+' || (reading_order-1) || ' days'),id FROM pages;
				UPDATE pages SET verification_failures=2,last_error='old failure';
			`); err != nil {
				t.Fatal(err)
			}
			first, err := reading.GetDailyReading(ctx, conn, "http", "2026-01-01")
			if err != nil {
				t.Fatal(err)
			}
			second, err := reading.GetDailyReading(ctx, conn, "http", "2026-01-02")
			if err != nil {
				t.Fatal(err)
			}
			result, err := ValidateLinks(ctx, conn, server.Client(), 3)
			if err != nil {
				t.Fatalf("healthy redirects must not abort remaining link validation: %v", err)
			}
			if result.Checked != len(pages) || result.Healthy != len(pages)-1 || result.Failed != 1 || result.Disabled != 1 {
				t.Fatalf("later broken links must still be checked: %+v", result)
			}
			for i, original := range []reading.Reading{first, second} {
				got, err := reading.GetDailyReading(ctx, conn, "http", fmt.Sprintf("2026-01-0%d", i+1))
				if err != nil || got.PageID != original.PageID || got.Title != original.Title {
					t.Fatalf("redirect handling changed historical identity: original=%+v got=%+v err=%v", original, got, err)
				}
				if (existingDestination || i == 1) && got.URL != original.URL {
					t.Fatalf("colliding alias must retain its working redirect URL: %q", got.URL)
				}
				var active, failures int
				var verified, diagnostic string
				if err := conn.QueryRow("SELECT active,verification_failures,last_verified,last_error FROM pages WHERE id=?", original.PageID).Scan(&active, &failures, &verified, &diagnostic); err != nil {
					t.Fatal(err)
				}
				if active != 1 || failures != 0 || verified == "" || diagnostic != "" {
					t.Fatalf("healthy alias was not recorded as verified: active=%d failures=%d verified=%q error=%q", active, failures, verified, diagnostic)
				}
			}
			var rows, assignments int
			if err := conn.QueryRow("SELECT (SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings)").Scan(&rows, &assignments); err != nil {
				t.Fatal(err)
			}
			if rows != len(pages) || assignments != len(pages) {
				t.Fatalf("redirect handling deleted catalog/history rows: pages=%d assignments=%d", rows, assignments)
			}
		})
	}
}

func TestRedirectDestinationInAnotherTopicDoesNotBlockURLUpdate(t *testing.T) {
	ctx := context.Background()
	conn := openValidatorTestDB(t, ctx)
	defer conn.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/current", http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	importValidatorTopic(t, ctx, conn, server.URL+"/old")
	if _, err := seed.ImportTopic(ctx, conn, seed.TopicFile{Topic: "another-topic", Name: "Another topic", Pages: []seed.PageFile{{Title: "Current reference", URL: server.URL + "/current"}}}); err != nil {
		t.Fatal(err)
	}
	result, err := ValidateLinks(ctx, conn, server.Client(), 3)
	if err != nil || result.Healthy != 2 {
		t.Fatalf("shared destination across topics must remain valid: %+v %v", result, err)
	}
	var destination string
	if err := conn.QueryRow("SELECT p.url FROM pages p JOIN topics t ON t.id=p.topic_id WHERE t.slug='http'").Scan(&destination); err != nil {
		t.Fatal(err)
	}
	if destination != server.URL+"/current" {
		t.Fatalf("another topic incorrectly prevented redirect update: %q", destination)
	}
}
