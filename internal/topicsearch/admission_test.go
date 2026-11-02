package topicsearch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
)

type admissionProvider struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *admissionProvider) Search(ctx context.Context, _ string, _ int) ([]SearchResult, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []SearchResult{
		{Title: "First guide", URL: "https://docs.example.org/first"},
		{Title: "Second guide", URL: "https://docs.example.org/second"},
	}, nil
}

type admissionPlanner struct{ calls atomic.Int32 }

func (p *admissionPlanner) Plan(context.Context, string) (PlanOutput, error) {
	p.calls.Add(1)
	return PlanOutput{}, nil
}

func TestSearchAdmissionAcrossDatabaseHandles(t *testing.T) {
	for _, cancelWinner := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelWinner), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "shared.sqlite")
			var connections []*sql.DB
			for range 2 {
				conn, err := db.Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetMaxOpenConns(1)
				connections = append(connections, conn)
			}
			conn := connections[0]
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO topics(id,slug,name,status) VALUES(41,'saved','Saved','active');
				INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(51,41,'Historical guide','https://docs.example.org/history',1);
				INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(41,'2026-06-26',51);
			`); err != nil {
				t.Fatal(err)
			}
			provider := &admissionProvider{started: make(chan struct{}), release: make(chan struct{})}
			planner := &admissionPlanner{}
			workerCtx, stopWorkers := context.WithCancel(ctx)
			var workers sync.WaitGroup
			var release sync.Once
			defer func() {
				stopWorkers()
				release.Do(func() { close(provider.release) })
				workers.Wait()
			}()
			type outcome struct {
				result Result
				err    error
			}
			const attempts = 12
			done := make(chan outcome, attempts)
			start := make(chan struct{})
			for i := range attempts {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					result, err := SearchTopic(workerCtx, connections[i%2], fmt.Sprintf("Subject %d", i), Options{
						Provider: provider, Planner: planner, Now: fixedTopicSearchTime, MinInterval: time.Nanosecond,
					})
					done <- outcome{result, err}
				}()
			}
			close(start)
			select {
			case <-provider.started:
			case <-ctx.Done():
				t.Fatal("no search reached the provider")
			}
			// Every loser must return while the winner remains blocked in its
			// provider. A process-local mutex cannot protect these two handles.
			for range attempts - 1 {
				select {
				case got := <-done:
					if !errors.Is(got.err, ErrRateLimited) || !got.result.RateLimited || got.result.Status != runStatusRateLimited || got.result.RunID == 0 || got.result.TopicID == 0 {
						t.Fatalf("competing caller did not return its rate-limited identity: %+v", got)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("competing searches entered provider work: provider=%d planner=%d", provider.calls.Load(), planner.calls.Load())
				}
			}
			var running, limited int
			if err := conn.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM topic_search_runs WHERE status='running'),
				(SELECT count(*) FROM topic_search_runs WHERE status='rate_limited' AND completed_at IS NOT NULL)
			`).Scan(&running, &limited); err != nil {
				t.Fatal(err)
			}
			if running != 1 || limited != attempts-1 || provider.calls.Load() != 1 || planner.calls.Load() != 1 {
				t.Fatalf("admission was not exclusive: running=%d limited=%d provider=%d planner=%d", running, limited, provider.calls.Load(), planner.calls.Load())
			}
			// Admission must commit before provider work; another writer can
			// acquire the database while this provider is deliberately blocked.
			writeCtx, cancelWrite := context.WithTimeout(ctx, time.Second)
			writer, err := connections[1].BeginTx(writeCtx, nil)
			if err == nil {
				err = writer.Rollback()
			}
			cancelWrite()
			if err != nil {
				t.Fatal("provider work retained the admission transaction", err)
			}
			// A losing request for an existing catalog keeps its identity and
			// active state, even while another topic owns the generation slot.
			saved, err := SearchTopic(ctx, connections[1], "Saved", Options{Provider: provider, Planner: planner, Now: fixedTopicSearchTime, MinInterval: time.Nanosecond})
			if !errors.Is(err, ErrRateLimited) || saved.TopicID != 41 || saved.TopicSlug != "saved" || saved.TopicName != "Saved" || saved.RunID == 0 {
				t.Fatal("rate limit changed existing topic identity", saved, err)
			}
			if cancelWinner {
				stopWorkers()
			} else {
				release.Do(func() { close(provider.release) })
			}
			winner := <-done
			if cancelWinner {
				if !errors.Is(winner.err, context.Canceled) || winner.result.Status != runStatusFailed {
					t.Fatal("canceled winner did not finalize failure", winner)
				}
			} else if winner.err != nil || winner.result.Status != runStatusCompleted {
				t.Fatal("released winner did not complete", winner)
			}
			release.Do(func() { close(provider.release) })
			next, err := SearchTopic(ctx, connections[1], "Next Subject", Options{Provider: provider, Planner: planner, Now: func() time.Time { return fixedTopicSearchTime().Add(time.Second) }, MinInterval: time.Nanosecond})
			if err != nil || next.Status != runStatusCompleted || provider.calls.Load() != 2 || planner.calls.Load() != 2 {
				t.Fatal("finished admission did not release the next request", next, err, provider.calls.Load(), planner.calls.Load())
			}
			var status, destination string
			var pageID, assignments int
			if err := conn.QueryRowContext(ctx, `SELECT t.status,p.id,p.url,(SELECT count(*) FROM daily_readings)
				FROM topics t JOIN daily_readings dr ON dr.topic_id=t.id JOIN pages p ON p.id=dr.page_id
				WHERE t.id=41 AND dr.reading_date='2026-06-26'`).Scan(&status, &pageID, &destination, &assignments); err != nil {
				t.Fatal(err)
			}
			if status != "active" || pageID != 51 || destination != "https://docs.example.org/history" || assignments != 1 {
				t.Fatal("competing requests changed saved history", status, pageID, destination, assignments)
			}
		})
	}
}

func TestQueuedSearchAdmissionRechecksDailyCapacity(t *testing.T) {
	for _, next := range []bool{false, true} {
		t.Run(fmt.Sprintf("next=%v", next), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "shared.sqlite")
			owner, err := db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			worker, err := db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()
			if _, err := owner.ExecContext(ctx, `
				INSERT INTO topics(id,slug,name,status) VALUES(1,'queued','Queued','queued'),(2,'other','Other','active');
				INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(51,1,'Saved queued reading','https://docs.example.org/saved-queued',1);
				INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',51);
			`); err != nil {
				t.Fatal(err)
			}
			writer, err := owner.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback()
			provider := &admissionProvider{started: make(chan struct{}), release: make(chan struct{})}
			close(provider.release)
			planner := &admissionPlanner{}
			opts := Options{Provider: provider, Planner: planner, Now: fixedTopicSearchTime, MinInterval: time.Nanosecond, DailyLimit: 1}
			type outcome struct {
				result QueueResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				var result QueueResult
				var err error
				if next {
					result, err = ProcessNextQueuedTopic(ctx, worker, opts)
				} else {
					result, err = ProcessQueuedTopic(ctx, worker, "queued", opts)
				}
				done <- outcome{result, err}
			}()
			deadline := time.Now().Add(time.Second)
			for worker.Stats().InUse == 0 {
				if time.Now().After(deadline) {
					t.Fatal("queued request did not reach the held writer")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case got := <-done:
				t.Fatalf("queued request completed before writer released: %+v", got)
			case <-time.After(150 * time.Millisecond):
			}
			// The public preflight observed capacity. Consume its final slot
			// before the queued request can acquire a writer for admission.
			if _, err := writer.ExecContext(ctx, `INSERT INTO topic_search_runs(topic_id,provider,query,status,started_at,completed_at)
				VALUES(2,'fake','other','completed',?,?)`, formatTime(fixedTopicSearchTime().Add(-time.Minute)), formatTime(fixedTopicSearchTime())); err != nil {
				t.Fatal(err)
			}
			if err := writer.Commit(); err != nil {
				t.Fatal(err)
			}
			got := <-done
			var runs, pageID, assignments int
			var status, destination string
			if err := owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM topic_search_runs),t.status,p.id,p.url,(SELECT count(*) FROM daily_readings)
				FROM topics t JOIN daily_readings dr ON dr.topic_id=t.id JOIN pages p ON p.id=dr.page_id
				WHERE t.id=1 AND dr.reading_date='2026-06-26'`).Scan(&runs, &status, &pageID, &destination, &assignments); err != nil {
				t.Fatal(err)
			}
			if got.err != nil || !got.result.DailyLimitReached || got.result.Processed || provider.calls.Load() != 0 || planner.calls.Load() != 0 || runs != 1 || status != "queued" || pageID != 51 || destination != "https://docs.example.org/saved-queued" || assignments != 1 {
				t.Fatal("queued request bypassed daily capacity or changed saved state/history", got, provider.calls.Load(), planner.calls.Load(), runs, status, pageID, destination, assignments)
			}
			// Direct SearchTopic keeps its existing CLI policy: DailyLimit is
			// applied only by the queued entry points, not direct generation.
			direct, err := SearchTopic(ctx, worker, "Direct Subject", opts)
			if err != nil || direct.Status != runStatusCompleted || provider.calls.Load() != 1 || planner.calls.Load() != 1 {
				t.Fatal("queued-only cap changed direct generation policy", direct, err)
			}
		})
	}
}

func TestSearchAdmissionRollsBackTopicStateWhenRunCannotBeCreated(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(id,slug,name,status) VALUES(1,'saved','Saved','active');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(1,1,'Saved guide','https://docs.example.org/history',1);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',1);
		CREATE TRIGGER reject_running_run BEFORE INSERT ON topic_search_runs
		WHEN NEW.status='running' BEGIN SELECT RAISE(ABORT,'test run insertion failed'); END;
	`); err != nil {
		t.Fatal(err)
	}
	provider := &admissionProvider{started: make(chan struct{}), release: make(chan struct{})}
	close(provider.release)
	planner := &admissionPlanner{}
	if _, err := SearchTopic(ctx, conn, "Saved", Options{Provider: provider, Planner: planner, Now: fixedTopicSearchTime}); err == nil {
		t.Fatal("failed run creation was accepted")
	}
	var status string
	var runs, pageID int
	if err := conn.QueryRowContext(ctx, `SELECT status,(SELECT count(*) FROM topic_search_runs),
		(SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-06-26') FROM topics WHERE id=1`).Scan(&status, &runs, &pageID); err != nil {
		t.Fatal(err)
	}
	if status != "active" || runs != 0 || pageID != 1 || provider.calls.Load() != 0 || planner.calls.Load() != 0 {
		t.Fatal("failed admission changed catalog/history or spent work", status, runs, pageID, provider.calls.Load(), planner.calls.Load())
	}
}

func TestQueuedSearchAdmissionRechecksCatalogEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, status, runStatus string
		activePages             int
		allowNamedRetry         bool
	}{
		{"completed catalog", "active", "completed", 2, false},
		{"disabled catalog", "disabled", "failed", 2, false},
		{"failed attempt with saved catalog", "active", "failed", 2, true},
		{"insufficient active catalog", "active", "completed", 1, true},
		{"failed catalog", "failed", "failed", 0, true},
	} {
		for _, next := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/next=%v", tc.name, next), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				path := filepath.Join(t.TempDir(), "shared.sqlite")
				owner, err := db.Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				worker, err := db.Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer worker.Close()
				if _, err := owner.ExecContext(ctx, `
					INSERT INTO topics(id,slug,name,status) VALUES(1,'queued','Queued','queued');
					INSERT INTO pages(id,topic_id,title,url,reading_order,active) VALUES(51,1,'Archived guide','https://docs.example.org/archived',1,0);
					INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',51);
				`); err != nil {
					t.Fatal(err)
				}
				writer, err := owner.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback()
				provider := &admissionProvider{started: make(chan struct{}), release: make(chan struct{})}
				close(provider.release)
				planner := &admissionPlanner{}
				opts := Options{Provider: provider, Planner: planner, Now: fixedTopicSearchTime, MinInterval: time.Nanosecond}
				type outcome struct {
					result QueueResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					var result QueueResult
					var err error
					if next {
						result, err = ProcessNextQueuedTopic(ctx, worker, opts)
					} else {
						result, err = ProcessQueuedTopic(ctx, worker, "queued", opts)
					}
					done <- outcome{result, err}
				}()
				deadline := time.Now().Add(time.Second)
				for worker.Stats().InUse == 0 {
					if time.Now().After(deadline) {
						t.Fatal("queued request did not reach the held writer")
					}
					time.Sleep(time.Millisecond)
				}
				select {
				case got := <-done:
					t.Fatalf("queued request completed before writer released: %+v", got)
				case <-time.After(100 * time.Millisecond):
				}
				if _, err := writer.ExecContext(ctx, "UPDATE topics SET status=? WHERE id=1", tc.status); err != nil {
					t.Fatal(err)
				}
				for i := range tc.activePages {
					if _, err := writer.ExecContext(ctx, "INSERT INTO pages(topic_id,title,url,reading_order) VALUES(1,?,?,?)", fmt.Sprintf("Completed guide %d", i), fmt.Sprintf("https://docs.example.org/completed-%d", i), i+2); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := writer.ExecContext(ctx, `INSERT INTO topic_search_runs(topic_id,provider,query,status,started_at,completed_at)
					VALUES(1,'fake','previous attempt',?,?,?)`, tc.runStatus, formatTime(fixedTopicSearchTime().Add(-time.Minute)), formatTime(fixedTopicSearchTime())); err != nil {
					t.Fatal(err)
				}
				if err := writer.Commit(); err != nil {
					t.Fatal(err)
				}
				got := <-done
				wantRetry := !next && tc.allowNamedRetry
				if got.err != nil || got.result.Processed != wantRetry || got.result.DailyLimitReached {
					t.Fatal("admission did not use the current queued eligibility", got, wantRetry)
				}
				wantCalls, wantRuns, wantPages, wantStatus := int32(0), 1, tc.activePages+1, tc.status
				if wantRetry {
					wantCalls, wantRuns, wantPages, wantStatus = 1, 2, tc.activePages+3, "active"
				}
				var runs, pages, pageID, assignments int
				var status, destination string
				if err := owner.QueryRowContext(ctx, `SELECT t.status,(SELECT count(*) FROM topic_search_runs),(SELECT count(*) FROM pages),p.id,p.url,(SELECT count(*) FROM daily_readings)
					FROM topics t JOIN daily_readings dr ON dr.topic_id=t.id JOIN pages p ON p.id=dr.page_id
					WHERE t.id=1 AND dr.reading_date='2026-06-26'`).Scan(&status, &runs, &pages, &pageID, &destination, &assignments); err != nil {
					t.Fatal(err)
				}
				if provider.calls.Load() != wantCalls || planner.calls.Load() != wantCalls || runs != wantRuns || pages != wantPages || status != wantStatus || pageID != 51 || destination != "https://docs.example.org/archived" || assignments != 1 {
					t.Fatal("queued eligibility changed work, catalog state or history", provider.calls.Load(), planner.calls.Load(), runs, pages, status, pageID, destination, assignments)
				}
			})
		}
	}
}
