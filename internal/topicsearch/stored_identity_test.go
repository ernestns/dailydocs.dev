package topicsearch

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/seed"
)

type identityRefreshProvider struct {
	calls   int
	results []SearchResult
}

func (p *identityRefreshProvider) Search(context.Context, string, int) ([]SearchResult, error) {
	p.calls++
	return p.results, nil
}

func importIdentityFixture(t *testing.T, conn *sql.DB, urls []string) {
	t.Helper()
	fixture := seed.TopicFile{Topic: "example", Name: "Example"}
	for i, raw := range urls {
		fixture.Pages = append(fixture.Pages, seed.PageFile{Title: fmt.Sprintf("Saved reading %d", i+1), URL: raw, Official: true})
	}
	if _, err := seed.ImportTopic(context.Background(), conn, fixture); err != nil {
		t.Fatal(err)
	}
	// Retain both a historical assignment and candidate identity for each page.
	if _, err := conn.Exec(`
		INSERT INTO topic_search_runs(id,topic_id,provider,query,status,started_at,error)
		SELECT 10,id,'fixture','old attempt','failed','2026-01-01 00:00:00','old failure' FROM topics;
		INSERT INTO daily_readings(topic_id,reading_date,page_id)
		SELECT topic_id,date('2026-01-01','+' || (reading_order-1) || ' days'),id FROM pages;
		INSERT INTO topic_search_results(id,topic_id,search_run_id,title,url,rank,accepted,reviewer_score,stored_as_page_id)
		SELECT 100+id,topic_id,10,title,url,reading_order,1,90,id FROM pages;
	`); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryRefreshPreservesImportedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, saved, returned, wantSaved string
		distinct                         bool
	}{
		{"SQLite normalized alias", "https://WWW.sqlite.org/wal.html/?UTM_SOURCE=feed#top", "https://sqlite.org/wal.html", "", false},
		{"SQLite actual HTTPS upgrade", "http://WWW.sqlite.org/wal.html/?utm_source=feed#top", "https://www.sqlite.org/wal.html", "https://www.sqlite.org/wal.html", false},
		{"ordinary tracking", "https://docs.example.org/wal?utm_source=feed&fbclid=123#top", "https://docs.example.org/wal", "", false},
		{"ordinary trailing slash", "https://docs.example.org/wal/", "https://docs.example.org/wal", "", false},
		{"same meaningful query", "https://sqlite.org/wal.html?view=full&utm_source=feed#section", "https://www.sqlite.org/wal.html?view=full#section", "", false},
		{"different queries", "https://sqlite.org/wal.html?view=full", "https://www.sqlite.org/wal.html?view=short", "", true},
		{"different fragments", "https://sqlite.org/wal.html#one", "https://www.sqlite.org/wal.html#two", "", true},
		{"different hosts", "https://docs.example.org/wal", "https://www.docs.example.org/wal", "", true},
		{"different schemes", "http://docs.example.org/wal", "https://docs.example.org/wal", "", true},
		{"opaque query", "https://sqlite.org/wal.html?section=a;b&utm_source=feed", "https://sqlite.org/wal.html?section=a;b", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			importIdentityFixture(t, conn, []string{tc.saved})
			before, err := reading.GetDailyReading(ctx, conn, "example", "2026-01-01")
			if err != nil {
				t.Fatal(err)
			}
			provider := &identityRefreshProvider{results: []SearchResult{
				{Title: "Fresh reading", URL: tc.returned},
				{Title: "Distinct guide", URL: "https://docs.example.org/distinct"},
			}}
			opts := Options{Provider: provider, Now: fixedTopicSearchTime, MinInterval: time.Nanosecond}
			queued, err := ProcessQueuedTopic(ctx, conn, "example", opts)
			if err != nil || !queued.Processed || queued.Result.Status != "completed" || provider.calls != 1 {
				t.Fatal("explicit retry failed", queued, provider.calls, err)
			}
			// A completed public retry must become a lookup, with no provider work.
			if again, err := ProcessQueuedTopic(ctx, conn, "example", opts); err != nil || again.Processed || provider.calls != 1 {
				t.Fatal("completed catalog was regenerated", again, provider.calls, err)
			}
			// Explicit operator refreshes must reuse the same IDs on later runs too.
			opts.Now = func() time.Time { return fixedTopicSearchTime().Add(time.Minute) }
			refreshed, err := SearchTopic(ctx, conn, "Example", opts)
			if err != nil || refreshed.Status != "completed" || provider.calls != 2 {
				t.Fatal(refreshed, provider.calls, err)
			}
			wantCount := 2
			if tc.distinct {
				wantCount = 3
			}
			for _, table := range []string{"pages", "topic_search_results"} {
				var count int
				if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != wantCount {
					t.Fatalf("%s identity count=%d want=%d err=%v", table, count, wantCount, err)
				}
			}
			var candidateID, pageID int64
			if err := conn.QueryRow(`SELECT id,stored_as_page_id FROM topic_search_results WHERE search_run_id=? AND title='Fresh reading' AND accepted=1`, refreshed.RunID).Scan(&candidateID, &pageID); err != nil {
				t.Fatal(err)
			}
			if !tc.distinct && (candidateID != 100+before.PageID || pageID != before.PageID) {
				t.Fatal("refresh replaced stored identities", candidateID, pageID, before.PageID)
			}
			if tc.distinct && (candidateID == 100+before.PageID || pageID == before.PageID) {
				t.Fatal("different resource reused historical identity", candidateID, pageID)
			}
			after, err := reading.GetDailyReading(ctx, conn, "example", before.Date)
			wantSaved := tc.wantSaved
			if wantSaved == "" {
				wantSaved = tc.saved
			}
			if err != nil || after.PageID != before.PageID || after.URL != wantSaved || !after.Official {
				t.Fatal("historical reading changed", before, after, err)
			}
			var order, oldFailures int
			if err := conn.QueryRow("SELECT reading_order FROM pages WHERE id=?", before.PageID).Scan(&order); err != nil || order != 1 {
				t.Fatal("historical order changed", order, err)
			}
			if err := conn.QueryRow("SELECT COUNT(*) FROM topic_search_runs WHERE id=10 AND status='failed' AND error='old failure'").Scan(&oldFailures); err != nil || oldFailures != 1 {
				t.Fatal("old attempt changed", oldFailures, err)
			}
			if available, err := AvailableReadings(ctx, conn, refreshed.TopicID); err != nil || available != wantCount {
				t.Fatal("rotation and readiness disagree", available, wantCount, err)
			}
			seen := map[string]bool{}
			for day := 10; day < 10+wantCount; day++ {
				got, err := reading.GetOrCreateDailyReading(ctx, conn, "example", fmt.Sprintf("2026-01-%02d", day))
				if err != nil {
					t.Fatal(err)
				}
				key := readingURLKey(got.URL)
				if seen[key] {
					t.Fatal("refresh added a repeated daily destination", got)
				}
				seen[key] = true
			}
		})
	}
}

func TestDiscoveryRefreshPreservesHistoricalDuplicateRows(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("exact=%v", exact), func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			urls := []string{"https://sqlite.org/wal.html?utm_source=old#top", "https://www.sqlite.org/wal.html?utm_source=other"}
			returned := "https://www.sqlite.org/wal.html"
			wantID := int64(1)
			if exact {
				urls[1] = returned
				wantID = 2
			}
			importIdentityFixture(t, conn, urls)
			result, err := ProcessQueuedTopic(ctx, conn, "example", Options{Now: fixedTopicSearchTime, Provider: fakeProvider{results: []SearchResult{
				{Title: "Fresh reading", URL: returned},
				{Title: "Distinct guide", URL: "https://docs.example.org/distinct"},
			}}})
			if err != nil || !result.Processed {
				t.Fatal(result, err)
			}
			for _, table := range []string{"pages", "topic_search_results"} {
				var count int
				if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 3 {
					t.Fatal("historical duplicate removed or padded", table, count, err)
				}
			}
			var candidateID, pageID int64
			if err := conn.QueryRow(`SELECT id,stored_as_page_id FROM topic_search_results WHERE search_run_id=? AND title='Fresh reading'`, result.Result.RunID).Scan(&candidateID, &pageID); err != nil || candidateID != 100+wantID || pageID != wantID {
				t.Fatal("exact/oldest identity preference changed", candidateID, pageID, wantID, err)
			}
			for i, raw := range urls {
				got, err := reading.GetDailyReading(ctx, conn, "example", fmt.Sprintf("2026-01-%02d", i+1))
				if err != nil || got.PageID != int64(i+1) || got.URL != raw {
					t.Fatal("historical duplicate assignment changed", got, raw, err)
				}
			}
		})
	}
}
