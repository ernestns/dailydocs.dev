package topicsearch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type cancelingStage struct{ cancel context.CancelFunc }

func (s cancelingStage) Plan(ctx context.Context, _ string) (PlanOutput, error) {
	s.cancel()
	return PlanOutput{}, fmt.Errorf("planner stopped: %w", ctx.Err())
}

func (s cancelingStage) Search(ctx context.Context, _ string, _ int) ([]SearchResult, error) {
	s.cancel()
	return nil, fmt.Errorf("search stopped: %w", ctx.Err())
}

func (s cancelingStage) Review(ctx context.Context, _ string, _ []ReviewCandidate) (ReviewOutput, error) {
	s.cancel()
	return ReviewOutput{}, fmt.Errorf("review stopped: %w", ctx.Err())
}

func TestCanceledStageFinalizesFailure(t *testing.T) {
	for _, stage := range []string{"planner", "search", "review"} {
		t.Run(stage, func(t *testing.T) {
			conn := openTopicSearchTestDB(t, context.Background())
			defer conn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := Options{Now: fixedTopicSearchTime, MinInterval: time.Nanosecond, Provider: fakeProvider{results: []SearchResult{{Title: "WAL guide", URL: "https://sqlite.org/wal.html"}}}}
			switch stage {
			case "planner":
				opts.Planner = cancelingStage{cancel}
			case "search":
				opts.Provider = cancelingStage{cancel}
			case "review":
				opts.Reviewer = cancelingStage{cancel}
			}
			result, err := SearchTopic(ctx, conn, "SQLite", opts)
			if !errors.Is(err, context.Canceled) || result.Status != "failed" {
				t.Fatalf("expected original cancellation and failed result: %+v, %v", result, err)
			}
			var topicStatus, runStatus, runError string
			if scanErr := conn.QueryRow("SELECT t.status,r.status,r.error FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id").Scan(&topicStatus, &runStatus, &runError); scanErr != nil {
				t.Fatal(scanErr)
			}
			if topicStatus != "failed" || runStatus != "failed" || runError != err.Error() {
				t.Fatal("failure state/diagnostic not persisted", topicStatus, runStatus, runError)
			}
			_, err = SearchTopic(context.Background(), conn, "Rust", Options{Now: func() time.Time { return fixedTopicSearchTime().Add(time.Minute) }, MinInterval: time.Nanosecond, Provider: fakeProvider{results: []SearchResult{{Title: "Ownership", URL: "https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html"}, {Title: "Generics", URL: "https://doc.rust-lang.org/book/ch10-00-generics.html"}}}})
			if err != nil {
				t.Fatal("failed run still blocks unrelated topic", err)
			}
		})
	}
}

func TestRepeatedSearchPreservesURLIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		count               int
	}{
		{"returned HTTPS upgrade", "http://sqlite.org/queryplanner-ng.html", "https://www.sqlite.org/queryplanner-ng.html", 1},
		{"distinct fragments", "https://sqlite.org/doc.html#one", "https://www.sqlite.org/doc.html#two", 2},
		{"distinct queries", "https://sqlite.org/doc.html?view=full", "https://www.sqlite.org/doc.html?view=short", 2},
		{"distinct hosts", "https://docs.example.org/page", "https://www.docs.example.org/page", 2},
		{"distinct schemes", "http://docs.example.org/page", "https://docs.example.org/page", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			var originalID, originalOrder int64
			for i, u := range []string{tc.first, tc.second} {
				_, err := SearchTopic(ctx, conn, "Example", Options{Now: func() time.Time { return fixedTopicSearchTime().Add(time.Duration(i) * time.Minute) }, MinInterval: time.Nanosecond, Provider: fakeProvider{results: []SearchResult{{Title: "Focused guide", URL: u}}}})
				if (i == 0 || tc.count == 1) && !errors.Is(err, ErrInsufficientResults) || i == 1 && tc.count == 2 && err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					if err := conn.QueryRow("SELECT id,reading_order FROM pages").Scan(&originalID, &originalOrder); err != nil {
						t.Fatal(err)
					}
					if _, err := conn.Exec("INSERT INTO daily_readings(topic_id,reading_date,page_id) SELECT topic_id,'2026-09-09',id FROM pages"); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, table := range []string{"pages", "topic_search_results"} {
				var count int
				if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != tc.count {
					t.Fatalf("%s identities: count=%d, error=%v", table, count, err)
				}
			}
			var assignedID, order int64
			var destination string
			if err := conn.QueryRow("SELECT p.id,p.reading_order,p.url FROM pages p JOIN daily_readings d ON d.page_id=p.id").Scan(&assignedID, &order, &destination); err != nil {
				t.Fatal(err)
			}
			if assignedID != originalID || order != originalOrder {
				t.Fatal("changed historical assignment/order", assignedID, order)
			}
			if tc.count == 1 && destination != tc.second {
				t.Fatal("did not prefer returned HTTPS destination", destination)
			}
		})
	}
}

func TestAliasRefreshRetainsHistoricalDuplicates(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.Exec(`
		INSERT INTO topics(id,slug,name) VALUES(1,'sqlite','SQLite');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES
			(1,1,'Plan','https://sqlite.org/eqp.html',1),
			(2,1,'Plan alias','https://www.sqlite.org/eqp.html',2);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES
			(1,'2026-09-08',1),(1,'2026-09-09',2);
	`); err != nil {
		t.Fatal(err)
	}
	_, err := SearchTopic(ctx, conn, "SQLite", Options{Provider: fakeProvider{results: []SearchResult{{Title: "Plan refreshed", URL: "https://www.sqlite.org/eqp.html"}}}})
	if !errors.Is(err, ErrInsufficientResults) {
		t.Fatal(err)
	}
	var pages, assigned int
	if err := conn.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM daily_readings d JOIN pages p ON p.id=d.page_id WHERE p.reading_order=p.id").Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if pages != 2 || assigned != 2 {
		t.Fatal("historical aliases or assignments changed", pages, assigned)
	}
}
