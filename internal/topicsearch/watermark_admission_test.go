package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
)

func TestRunWatermarkAdmitsOneAttemptAcrossHandles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "watermark.sqlite")
	var handles []*sql.DB
	for range 2 {
		conn, err := db.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		handles = append(handles, conn)
	}
	if _, err := handles[0].Exec(`
		INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','failed'),(2,'go','Go','failed');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(11,1,'Saved guide','https://docs.example.org/saved',1);
		INSERT INTO daily_readings(id,topic_id,reading_date,page_id) VALUES(21,1,'2026-06-26',11);
		INSERT INTO topic_search_runs(id,topic_id,provider,query,status) VALUES(7,1,'fixture','Rust','failed'),(11,2,'fixture','Go','failed');
	`); err != nil {
		t.Fatal(err)
	}
	provider := &revisionFailureProvider{err: errors.New("fixture source unavailable")}
	observed := int64(11) // Global observed ID is greater than this topic's latest.
	opts := Options{Provider: provider, ObservedRunID: &observed, MinInterval: time.Nanosecond}
	type outcome struct {
		result QueueResult
		err    error
	}
	const requests = 12
	done := make(chan outcome, requests)
	start := make(chan struct{})
	for i := range requests {
		go func() {
			<-start
			result, err := ProcessQueuedTopic(ctx, handles[i%2], "rust", opts)
			done <- outcome{result, err}
		}()
	}
	close(start)
	processed := 0
	for range requests {
		got := <-done
		if got.result.Processed {
			processed++
			if !errors.Is(got.err, provider.err) || got.result.Result.Status != "failed" {
				t.Error("admission changed failure result", got)
			}
		} else if got.err != nil || got.result.DailyLimitReached {
			t.Error("stale watermark was not a no-op", got)
		}
	}
	var runs int
	var latest int64
	if err := handles[0].QueryRow("SELECT count(*),MAX(id) FROM topic_search_runs").Scan(&runs, &latest); err != nil || runs != 3 || processed != 1 || provider.calls.Load() != 1 {
		t.Fatal("one observed watermark admitted multiple attempts/audits", runs, processed, provider.calls.Load(), err)
	}
	restarted, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if replay, err := ProcessQueuedTopic(ctx, restarted, "rust", opts); err != nil || replay.Processed || provider.calls.Load() != 1 {
		t.Fatal("restart forgot watermark", replay, provider.calls.Load(), err)
	}
	opts.ObservedRunID = &latest
	if fresh, err := ProcessQueuedTopic(ctx, restarted, "rust", opts); !fresh.Processed || !errors.Is(err, provider.err) || provider.calls.Load() != 2 {
		t.Fatal("fresh watermark did not allow retry", fresh, provider.calls.Load(), err)
	}
	opts.ObservedRunID = &observed
	if direct, err := SearchTopic(ctx, restarted, "Rust", opts); !errors.Is(err, provider.err) || direct.Status != "failed" || provider.calls.Load() != 3 {
		t.Fatal("watermark changed direct CLI policy", direct, provider.calls.Load(), err)
	}
	saved, err := reading.GetDailyReading(ctx, restarted, "rust", "2026-06-26")
	if err != nil || saved.PageID != 11 || saved.URL != "https://docs.example.org/saved" {
		t.Fatal("watermark admission changed history", saved, err)
	}
	var original int
	if err := restarted.QueryRow("SELECT count(*) FROM daily_readings WHERE id=21 AND page_id=11").Scan(&original); err != nil || original != 1 {
		t.Fatal("saved assignment identity changed", original, err)
	}
}

func TestRunWatermarkIncludesRateLimitedAdmission(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.Exec("INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','queued'),(2,'other','Other','searching'); INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at) VALUES(11,2,'fake','other','running','searching',?)", formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	provider := &revisionFailureProvider{err: errors.New("fixture source unavailable")}
	observed := int64(11)
	opts := Options{Provider: provider, ObservedRunID: &observed, MinInterval: time.Nanosecond}
	first, err := ProcessQueuedTopic(ctx, conn, "rust", opts)
	if !errors.Is(err, ErrRateLimited) || !first.Processed || !first.Result.RateLimited || provider.calls.Load() != 0 {
		t.Fatal("first rejected request lost its audit", first, provider.calls.Load(), err)
	}
	if _, err := conn.Exec("UPDATE topic_search_runs SET status='completed',stage='',completed_at=datetime('now') WHERE id=11"); err != nil {
		t.Fatal(err)
	}
	if replay, err := ProcessQueuedTopic(ctx, conn, "rust", opts); err != nil || replay.Processed || provider.calls.Load() != 0 {
		t.Fatal("replayed rejection generated after capacity freed", replay, provider.calls.Load(), err)
	}
	var runs, limited int
	if err := conn.QueryRow("SELECT count(*),sum(status='rate_limited') FROM topic_search_runs WHERE topic_id=1").Scan(&runs, &limited); err != nil || runs != 1 || limited != 1 {
		t.Fatal("replay added or changed audit history", runs, limited, err)
	}
	opts.ObservedRunID = &first.Result.RunID
	if retry, err := ProcessQueuedTopic(ctx, conn, "rust", opts); !retry.Processed || !errors.Is(err, provider.err) || provider.calls.Load() != 1 {
		t.Fatal("fresh watermark could not retry rejected admission", retry, provider.calls.Load(), err)
	}
}
