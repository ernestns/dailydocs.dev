package topicsearch

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
	"modernc.org/sqlite"
)

var cleanupBarrierID atomic.Uint64

func TestFailureCleanupKeepsAdmissionUntilCatalogFinalized(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readingStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var pause, release sync.Once
	defer release.Do(func() { close(releaseRead) })
	function := fmt.Sprintf("cleanup_barrier_%d", cleanupBarrierID.Add(1))
	if err := sqlite.RegisterScalarFunction(function, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		pause.Do(func() {
			close(readingStarted)
			select {
			case <-releaseRead:
			case <-ctx.Done():
			}
		})
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	old, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	old.SetMaxOpenConns(1)
	next, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := old.ExecContext(ctx, `
  INSERT INTO topics(id,slug,name,status) VALUES(1,'sqlite','SQLite','active');
  INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(11,1,'Historical guide','https://docs.example.org/history',1);
  INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',11);
 `); err != nil {
		t.Fatal(err)
	}
	// A connection-local TEMP VIEW pauses this exact cleanup boundary without
	// altering the shared schema. The other connection uses the real pages table,
	// including all indexes and foreign keys. The scalar keeps the read snapshot
	// alive while an independent caller attempts to reserve the next run.
	if _, err := old.ExecContext(ctx, "CREATE TEMP VIEW pages AS SELECT topic_id,active,"+function+"(url) AS url FROM main.pages"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Result
		err    error
	}
	oldDone := make(chan outcome, 1)
	nextDone := make(chan outcome, 1)
	var jobs sync.WaitGroup
	defer func() { release.Do(func() { close(releaseRead) }); jobs.Wait() }()
	plannerErr := errors.New("planner unavailable")
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		result, err := SearchTopic(ctx, old, "SQLite", Options{Provider: fakeProvider{}, Planner: fakePlanner{err: plannerErr}, Now: fixedTopicSearchTime, MinInterval: time.Nanosecond})
		oldDone <- outcome{result, err}
	}()
	select {
	case <-readingStarted:
	case <-ctx.Done():
		t.Fatal("failure cleanup did not reach the catalog read")
	}
	var visibleRun string
	if err := next.QueryRowContext(ctx, "SELECT status FROM topic_search_runs WHERE id=1").Scan(&visibleRun); err != nil {
		t.Fatal(err)
	}
	t.Log("while old cleanup is paused, committed run status:", visibleRun)
	provider := &admissionProvider{started: make(chan struct{}), release: make(chan struct{})}
	close(provider.release)
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		result, err := SearchTopic(ctx, next, "SQLite", Options{Provider: provider, Now: func() time.Time { return fixedTopicSearchTime().Add(time.Minute) }, MinInterval: time.Nanosecond})
		nextDone <- outcome{result, err}
	}()
	var newer outcome
	completedEarly := false
	select {
	case newer = <-nextDone:
		completedEarly = true
	case <-time.After(time.Second):
	}
	t.Logf("new search before old cleanup finishes: completed=%v provider calls=%d", completedEarly, provider.calls.Load())
	release.Do(func() { close(releaseRead) })
	older := <-oldDone
	if !errors.Is(older.err, plannerErr) {
		t.Fatal("old failure changed", older)
	}
	if !completedEarly {
		newer = <-nextDone
	}
	if newer.err != nil || newer.result.Status != "completed" {
		t.Fatal("new search failed", newer)
	}
	var status, latest string
	var pages, archive int
	if err := next.QueryRowContext(ctx, `SELECT status,(SELECT status FROM topic_search_runs ORDER BY id DESC LIMIT 1),(SELECT count(*) FROM pages),(SELECT page_id FROM daily_readings WHERE reading_date='2026-06-26') FROM topics WHERE id=1`).Scan(&status, &latest, &pages, &archive); err != nil {
		t.Fatal(err)
	}
	_, readingErr := reading.GetOrCreateDailyReading(ctx, next, "sqlite", "2026-06-27")
	t.Logf("after both: topic=%s latest run=%s pages=%d archive=%d today's reading error=%v", status, latest, pages, archive, readingErr)
	if visibleRun != "running" || completedEarly || status != "active" || readingErr != nil {
		t.Fatal("old failure released admission early and overwrote a successful new catalog")
	}
	if pages != 3 || archive != 11 || provider.calls.Load() != 1 {
		t.Fatal("cleanup or new generation changed saved history", pages, archive, provider.calls.Load())
	}
	var originalURL, originalFailure string
	if err := next.QueryRowContext(ctx, "SELECT (SELECT url FROM pages WHERE id=11),(SELECT error FROM topic_search_runs WHERE id=1)").Scan(&originalURL, &originalFailure); err != nil {
		t.Fatal(err)
	}
	if originalURL != "https://docs.example.org/history" || originalFailure != plannerErr.Error() {
		t.Fatal("cleanup changed source identity or failure diagnostic", originalURL, originalFailure)
	}
}

func TestFailureCleanupRollsBackRunWhenCatalogUpdateFails(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(id,slug,name,status) VALUES(1,'sqlite','SQLite','searching');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(11,1,'Saved first','https://docs.example.org/saved-first',1),(12,1,'Saved second','https://docs.example.org/saved-second',2);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-26',11);
		INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at) VALUES(1,1,'fake','SQLite','running','planning','2026-06-27 10:00:00');
		CREATE TRIGGER fail_catalog_cleanup BEFORE UPDATE OF status ON topics
		WHEN OLD.id=1 AND NEW.status='active' BEGIN SELECT RAISE(ABORT,'fixture catalog unavailable'); END;
	`); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	originalErr := fmt.Errorf("planner stopped: %w", context.Canceled)
	err := failRunAndTopic(canceled, conn, 1, 1, originalErr)
	if err == nil || !strings.Contains(err.Error(), "fixture catalog unavailable") {
		t.Fatal("cleanup did not report the injected catalog update failure", err)
	}
	var runStatus, stage, diagnostic, topicStatus string
	var finished bool
	if err := conn.QueryRowContext(ctx, `SELECT r.status,r.stage,r.error,r.completed_at IS NOT NULL,t.status
		FROM topic_search_runs r JOIN topics t ON t.id=r.topic_id WHERE r.id=1`).Scan(&runStatus, &stage, &diagnostic, &finished, &topicStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || stage != "planning" || diagnostic != "" || finished || topicStatus != "searching" {
		t.Fatal("failed cleanup partially released admission or changed the catalog", runStatus, stage, diagnostic, finished, topicStatus)
	}
	if _, err := conn.ExecContext(ctx, "DROP TRIGGER fail_catalog_cleanup"); err != nil {
		t.Fatal(err)
	}
	// Cleanup is retryable after the database error; the canceled provider
	// context must not prevent its existing bounded persistence operation.
	if err := failRunAndTopic(canceled, conn, 1, 1, originalErr); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT r.status,r.stage,r.error,r.completed_at IS NOT NULL,t.status
		FROM topic_search_runs r JOIN topics t ON t.id=r.topic_id WHERE r.id=1`).Scan(&runStatus, &stage, &diagnostic, &finished, &topicStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" || stage != "" || diagnostic != originalErr.Error() || !finished || topicStatus != "active" {
		t.Fatal("retry did not finalize failure and retain the usable catalog", runStatus, stage, diagnostic, finished, topicStatus)
	}
	result, err := SearchTopic(ctx, conn, "SQLite", Options{
		Provider: fakeProvider{results: []SearchResult{{Title: "New first", URL: "https://docs.example.org/new-first"}, {Title: "New second", URL: "https://docs.example.org/new-second"}}},
		Now:      fixedTopicSearchTime, MinInterval: time.Nanosecond,
	})
	if err != nil || result.Status != runStatusCompleted {
		t.Fatal("successful cleanup did not release generation admission", result, err)
	}
	// Repeated cleanup of an already-finalized older run must remain a no-op.
	if err := failRunAndTopic(canceled, conn, 1, 1, originalErr); err != nil {
		t.Fatal(err)
	}
	archived, err := reading.GetDailyReading(ctx, conn, "sqlite", "2026-06-26")
	if err != nil || archived.PageID != 11 || archived.URL != "https://docs.example.org/saved-first" {
		t.Fatal("failure cleanup changed the saved reading", archived, err)
	}
	if _, err := reading.GetOrCreateDailyReading(ctx, conn, "sqlite", "2026-06-27"); err != nil {
		t.Fatal("older cleanup hid the newly completed catalog", err)
	}
}
