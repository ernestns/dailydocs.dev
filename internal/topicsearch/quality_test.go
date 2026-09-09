package topicsearch

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSQLiteQualityReplay(t *testing.T) {
	data, err := os.ReadFile("testdata/sqlite-quality.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Results []SearchResult
		Reviews []struct {
			URL string
			ReviewResult
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	reviewer := replayReviewer{}
	for _, review := range fixture.Reviews {
		u, _, err := normalizeURL(review.URL)
		if err != nil {
			t.Fatal(err)
		}
		reviewer[readingURLKey(u)] = review.ReviewResult
	}
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	result, err := SearchTopic(ctx, conn, "SQLite", Options{Provider: fakeProvider{results: fixture.Results}, Reviewer: reviewer, MinInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if result.StoredCount != 3 {
		t.Fatalf("expected three distinct useful readings from captured sample, got %+v", result)
	}
	var unreviewed, candidates int
	if err := conn.QueryRow("SELECT COUNT(*) FROM topic_search_results WHERE reviewer_score IS NULL AND accepted=0").Scan(&unreviewed); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM topic_search_results").Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if unreviewed != 7 || candidates != 10 {
		t.Fatalf("omissions must remain unreviewed: %d unreviewed/%d candidates", unreviewed, candidates)
	}
	var insecure int
	if err := conn.QueryRow("SELECT COUNT(*) FROM pages WHERE url LIKE 'http:%'").Scan(&insecure); err != nil {
		t.Fatal(err)
	}
	if insecure != 0 {
		t.Fatal("did not prefer returned HTTPS alias")
	}
}

type replayReviewer map[string]ReviewResult

func (r replayReviewer) Review(_ context.Context, _ string, candidates []ReviewCandidate) (ReviewOutput, error) {
	var output ReviewOutput
	for _, c := range candidates {
		if review, ok := r[readingURLKey(c.URL)]; ok {
			review.Index = c.Index
			output.Results = append(output.Results, review)
		}
	}
	return output, nil
}

func TestURLIdentityPreservesDifferentResources(t *testing.T) {
	input := []SearchResult{
		{Title: "Section A", URL: "https://sqlite.org/doc.html?view=full&utm_source=test#section-a"},
		{Title: "Section B", URL: "https://sqlite.org/doc.html?view=full#section-b"},
		{Title: "Other view", URL: "https://sqlite.org/doc.html?view=short#section-a"},
		{Title: "Different site", URL: "https://docs.example.org/page"},
		{Title: "Another host", URL: "https://www.docs.example.org/page"},
		{Title: "Different scheme", URL: "http://docs.example.org/page"},
	}
	results := normalizeResults(input)
	if len(results) != len(input) {
		t.Fatalf("collapsed distinct destinations: %+v", results)
	}
	for _, result := range results {
		if strings.Contains(result.URL, "utm_source") {
			t.Fatal("tracking retained")
		}
		if result.Title == "Section A" && result.URL != "https://sqlite.org/doc.html?view=full#section-a" {
			t.Fatalf("lost query/fragment: %s", result.URL)
		}
	}
}

func TestReviewRejectsAmbiguousIndices(t *testing.T) {
	for name, indices := range map[string][]int{"duplicate": {1, 1}, "unknown": {1, 99}} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			var reviews []ReviewResult
			for _, index := range indices {
				reviews = append(reviews, ReviewResult{Index: index, DailyDocsScore: 90, ShouldStore: true, PageType: "guide"})
			}
			_, err := SearchTopic(ctx, conn, "SQLite", Options{Provider: fakeProvider{results: []SearchResult{{Title: "WAL", URL: "https://sqlite.org/wal.html"}}}, Reviewer: fakeReviewer{output: ReviewOutput{Results: reviews}}})
			if err == nil {
				t.Fatal("expected ambiguous review error")
			}
			var pages, unreviewed int
			if err := conn.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pages); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRow("SELECT COUNT(*) FROM topic_search_results WHERE reviewer_score IS NULL").Scan(&unreviewed); err != nil {
				t.Fatal(err)
			}
			if pages != 0 || unreviewed != 1 {
				t.Fatalf("ambiguous decisions published: pages=%d unreviewed=%d", pages, unreviewed)
			}
		})
	}
}

func TestURLNormalizationPreservesOpaqueQuery(t *testing.T) {
	raw := "https://example.org/docs?section=a;b&utm_source=test#examples"
	normalized, _, err := normalizeURL(raw)
	if err != nil || normalized != raw {
		t.Fatalf("must not partially parse and lose an opaque query: %q, %v", normalized, err)
	}
}
