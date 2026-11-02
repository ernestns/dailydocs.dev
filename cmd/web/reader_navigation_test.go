package main

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/seed"
)

func TestReadingNavigationPreservesSharedReadingAndReturnsToToday(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	if _, err := seed.ImportTopic(ctx, conn, seed.TopicFile{
		Topic: "sqlite",
		Name:  "SQLite",
		Pages: []seed.PageFile{
			{Title: "Write-Ahead Logging", URL: "https://sqlite.org/wal.html"},
			{Title: "Query Planner", URL: "https://sqlite.org/queryplanner.html"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	application := app{db: conn, now: func() time.Time { return now }}
	readPage := func(path string) string {
		t.Helper()
		response := httptest.NewRecorder()
		application.routeHandler(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	link := func(body, label string) string {
		t.Helper()
		pattern := regexp.MustCompile(`<a[^>]*href="([^"]+)"[^>]*>` + regexp.QuoteMeta(label) + `</a>`)
		match := pattern.FindStringSubmatch(body)
		if len(match) != 2 {
			t.Fatalf("reading page has no %q link", label)
		}
		return html.UnescapeString(match[1])
	}
	source := func(body string) string {
		t.Helper()
		match := regexp.MustCompile(`<a class="reading-link" href="([^"]+)">`).FindStringSubmatch(body)
		if len(match) != 2 {
			t.Fatal("reading page has no source link")
		}
		return html.UnescapeString(match[1])
	}

	today := readPage("/sqlite")
	permalink := link(today, "Share or save this reading")
	if permalink != "/sqlite/2026-06-27" {
		t.Fatalf("unexpected dated link %q", permalink)
	}
	if !strings.Contains(today, `<time datetime="2026-06-27">2026-06-27</time>`) {
		t.Fatal("reading date must be machine-readable and visible")
	}
	originalSource := source(today)
	now = now.AddDate(0, 0, 1)
	shared := readPage(permalink)
	if got := source(shared); got != originalSource {
		t.Fatalf("shared reading changed after midnight: %q became %q", originalSource, got)
	}
	if got := link(shared, "Share or save this reading"); got != permalink {
		t.Fatalf("historical reading shared today's URL instead: %q", got)
	}
	currentPath := link(shared, "Today's reading")
	if currentPath != "/sqlite" {
		t.Fatalf("expected current topic link, got %q", currentPath)
	}
	current := readPage(currentPath)
	if source(current) == originalSource {
		t.Fatal("today's link did not advance to the next daily reading")
	}
	if got := link(current, "Share or save this reading"); got != "/sqlite/2026-06-28" {
		t.Fatalf("current reading has incorrect dated link %q", got)
	}
	var assignments int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings").Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	if assignments != 2 {
		t.Fatalf("navigation altered historical assignments: got %d, want 2", assignments)
	}
}
