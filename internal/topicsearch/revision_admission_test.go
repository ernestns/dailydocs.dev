package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
)

type revisionFailureProvider struct {
	calls atomic.Int32
	err   error
}

func (p *revisionFailureProvider) Search(context.Context, string, int) ([]SearchResult, error) {
	p.calls.Add(1)
	return nil, p.err
}

func TestObservedRevisionAdmitsOneAttemptAcrossHandles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "revision.sqlite")
	var handles []*sql.DB
	for range 2 {
		conn, err := db.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		handles = append(handles, conn)
	}
	if _, err := handles[0].Exec("INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','queued'); INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(11,1,'Saved guide','https://docs.example.org/saved',1); INSERT INTO daily_readings(id,topic_id,reading_date,page_id) VALUES(21,1,'2026-06-26',11)"); err != nil {
		t.Fatal(err)
	}
	provider := &revisionFailureProvider{err: errors.New("fixture source unavailable")}
	var observed int64
	opts := Options{Provider: provider, ExpectedRunID: &observed, MinInterval: time.Nanosecond}
	start := make(chan struct{})
	type outcome struct {
		result QueueResult
		err    error
	}
	const requests = 12
	done := make(chan outcome, requests)
	var jobs sync.WaitGroup
	for i := range requests {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			<-start
			result, err := ProcessQueuedTopic(ctx, handles[i%2], "rust", opts)
			done <- outcome{result, err}
		}()
	}
	close(start)
	jobs.Wait()
	processed := 0
	for range requests {
		got := <-done
		if got.result.Processed {
			processed++
			if !errors.Is(got.err, provider.err) || got.result.Result.Status != "failed" {
				t.Fatal("claimed attempt changed failure semantics", got)
			}
		} else if got.err != nil || got.result.DailyLimitReached {
			t.Fatal("stale revision must be a no-op", got)
		}
	}
	var runs int
	var latest int64
	if err := handles[0].QueryRow("SELECT count(*),MAX(id) FROM topic_search_runs").Scan(&runs, &latest); err != nil || runs != 1 || processed != 1 || provider.calls.Load() != 1 {
		t.Fatal("one observed revision admitted multiple attempts/audit rows", runs, processed, provider.calls.Load(), err)
	}
	// A newly opened handle has no memory of the first admission.
	restarted, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if replay, err := ProcessQueuedTopic(ctx, restarted, "rust", opts); err != nil || replay.Processed || provider.calls.Load() != 1 {
		t.Fatal("restart forgot the consumed revision", replay, provider.calls.Load(), err)
	}
	opts.ExpectedRunID = &latest
	if fresh, err := ProcessQueuedTopic(ctx, restarted, "rust", opts); !fresh.Processed || !errors.Is(err, provider.err) || provider.calls.Load() != 2 {
		t.Fatal("fresh observed revision could not retry", fresh, provider.calls.Load(), err)
	}
	// Direct explicit searches retain their API policy even if options include
	// an old observation; the guard belongs to named queued processing only.
	opts.ExpectedRunID = &observed
	if direct, err := SearchTopic(ctx, restarted, "Rust", opts); !errors.Is(err, provider.err) || direct.Status != "failed" || provider.calls.Load() != 3 {
		t.Fatal("queued revision altered direct API behavior", direct, provider.calls.Load(), err)
	}
	saved, err := reading.GetDailyReading(ctx, restarted, "rust", "2026-06-26")
	if err != nil || saved.PageID != 11 || saved.URL != "https://docs.example.org/saved" {
		t.Fatal("revision admission changed saved identity", saved, err)
	}
	var assignments, pages, original int
	if err := restarted.QueryRow("SELECT (SELECT count(*) FROM daily_readings),(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings WHERE id=21 AND page_id=11)").Scan(&assignments, &pages, &original); err != nil || assignments != 1 || pages != 1 || original != 1 {
		t.Fatal("revision admission changed stored history", assignments, pages, original, err)
	}
}

func TestObservedRevisionIncludesRateLimitedAdmission(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.Exec("INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','queued'),(2,'other','Other','searching'); INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at) VALUES(11,2,'fake','other','running','searching',?)", formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	provider := &revisionFailureProvider{err: errors.New("fixture source unavailable")}
	var observed int64
	opts := Options{Provider: provider, ExpectedRunID: &observed, MinInterval: time.Nanosecond}
	first, err := ProcessQueuedTopic(ctx, conn, "rust", opts)
	if !errors.Is(err, ErrRateLimited) || !first.Processed || !first.Result.RateLimited || provider.calls.Load() != 0 {
		t.Fatal("first competing request lost its admission audit", first, provider.calls.Load(), err)
	}
	if _, err := conn.Exec("UPDATE topic_search_runs SET status='completed',stage='',completed_at=datetime('now') WHERE id=11"); err != nil {
		t.Fatal(err)
	}
	replay, err := ProcessQueuedTopic(ctx, conn, "rust", opts)
	if err != nil || replay.Processed || provider.calls.Load() != 0 {
		t.Fatal("replayed rejected request started work after capacity freed", replay, provider.calls.Load(), err)
	}
	var runs, limited int
	if err := conn.QueryRow("SELECT count(*),sum(status='rate_limited') FROM topic_search_runs WHERE topic_id=1").Scan(&runs, &limited); err != nil || runs != 1 || limited != 1 {
		t.Fatal("replay added or changed audit history", runs, limited, err)
	}
	opts.ExpectedRunID = &first.Result.RunID
	if retry, err := ProcessQueuedTopic(ctx, conn, "rust", opts); !retry.Processed || !errors.Is(err, provider.err) || provider.calls.Load() != 1 {
		t.Fatal("fresh request after rejected admission could not retry", retry, provider.calls.Load(), err)
	}
}
