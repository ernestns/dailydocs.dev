package topicsearch

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConcurrentSearchTopicCreationKeepsDistinctCatalogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(3)
	// WAL permits lookups while an unrelated writer briefly delays creation.
	// Before atomic resolution, both names chose the same unclaimed slug.
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	type outcome struct {
		name   string
		result Result
		err    error
	}
	done := make(chan outcome, 2)
	for _, name := range []string{"Health", "Health Topic"} {
		go func() {
			result, err := SearchTopic(ctx, conn, name, Options{
				Provider: fakeProvider{results: []SearchResult{
					{Title: "First guide", URL: "https://docs.example.org/first"},
					{Title: "Second guide", URL: "https://docs.example.org/second"},
				}},
				Now: fixedTopicSearchTime, MinInterval: time.Nanosecond,
			})
			done <- outcome{name, result, err}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse < 3 {
		if time.Now().After(deadline) {
			t.Fatal("requests did not reach the held writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-done:
		t.Fatalf("request completed before writer released: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	results := []outcome{<-done, <-done}
	for _, got := range results {
		if got.err != nil && !errors.Is(got.err, ErrRateLimited) {
			t.Fatal(got.err)
		}
		if got.result.TopicName != got.name {
			t.Fatalf("request captured another topic: %+v", got)
		}
		var stored string
		if err := conn.QueryRow("SELECT name FROM topics WHERE id=?", got.result.TopicID).Scan(&stored); err != nil || stored != got.name {
			t.Fatalf("catalog identity changed: requested=%q stored=%q err=%v", got.name, stored, err)
		}
	}
	if results[0].result.TopicID == results[1].result.TopicID || results[0].result.TopicSlug == results[1].result.TopicSlug {
		t.Fatalf("distinct searches merged catalogs: %+v", results)
	}
}

func TestSearchTopicWithoutProviderDoesNotReserveIdentity(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := SearchTopic(ctx, conn, "Health", Options{}); err == nil {
		t.Fatal("missing provider was accepted")
	}
	var count int
	if err := conn.QueryRow("SELECT COUNT(*) FROM topics").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unconfigured request reserved a topic: count=%d err=%v", count, err)
	}
}
