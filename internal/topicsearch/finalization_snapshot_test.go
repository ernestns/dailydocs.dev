package topicsearch

import (
	"context"
	"database/sql"
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
	"github.com/ernestns/daily-docs/internal/seed"
	"modernc.org/sqlite"
)

var finalizationFunctionID atomic.Uint64

type finalizationProvider func(context.Context, string, int) ([]SearchResult, error)

func (p finalizationProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return p(ctx, query, limit)
}

type finalizationOutcome struct {
	result Result
	err    error
}

func finalizationCatalog(count int) seed.TopicFile {
	fixture := seed.TopicFile{Topic: "sqlite", Name: "SQLite"}
	for i := range count {
		fixture.Pages = append(fixture.Pages, seed.PageFile{Title: fmt.Sprintf("Saved guide %d", i), URL: fmt.Sprintf("https://sqlite.org/guide-%d.html", i)})
	}
	return fixture
}

func finalizationDBs(t *testing.T, ctx context.Context, count int) (*sql.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	old, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	old.SetMaxOpenConns(1)
	importer, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = importer.Close() })
	importer.SetMaxOpenConns(1)
	if _, err := seed.ImportTopic(ctx, importer, finalizationCatalog(count)); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Exec("INSERT INTO daily_readings(id,topic_id,reading_date,page_id) VALUES(21,1,'2026-06-26',1)"); err != nil {
		t.Fatal(err)
	}
	return old, importer
}

func assertFinalizationArchive(t *testing.T, ctx context.Context, conn *sql.DB) {
	t.Helper()
	got, err := reading.GetDailyReading(ctx, conn, "sqlite", "2026-06-26")
	if err != nil || got.PageID != 1 || got.URL != "https://sqlite.org/guide-0.html" {
		t.Fatal("finalization changed the saved reading", got, err)
	}
	var assigned int
	if err := conn.QueryRowContext(ctx, "SELECT id FROM daily_readings WHERE page_id=1 AND reading_date='2026-06-26'").Scan(&assigned); err != nil || assigned != 21 {
		t.Fatal("finalization replaced the historical assignment", assigned, err)
	}
}

func TestDiscoveryFinalizationSerializesWithCatalogImport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	paused, releaseRead := make(chan struct{}), make(chan struct{})
	var once, release sync.Once
	function := fmt.Sprintf("finalization_read_%d", finalizationFunctionID.Add(1))
	if err := sqlite.RegisterScalarFunction(function, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		once.Do(func() {
			close(paused)
			select {
			case <-releaseRead:
			case <-ctx.Done():
			}
		})
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	old, importer := finalizationDBs(t, ctx, 1)
	if _, err := old.Exec("CREATE TEMP VIEW pages AS SELECT topic_id,active," + function + "(url) AS url FROM main.pages"); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	defer func() { release.Do(func() { close(releaseRead) }); jobs.Wait() }()
	providerErr := errors.New("provider unavailable")
	searchDone := make(chan finalizationOutcome, 1)
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		result, err := SearchTopic(ctx, old, "SQLite", Options{Provider: fakeProvider{err: providerErr}, Now: fixedTopicSearchTime})
		searchDone <- finalizationOutcome{result, err}
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("search did not reach its final catalog count")
	}
	importDone := make(chan error, 1)
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		_, err := seed.ImportTopic(ctx, importer, finalizationCatalog(3))
		importDone <- err
	}()
	importedEarly := false
	select {
	case err := <-importDone:
		importedEarly = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
	}
	release.Do(func() { close(releaseRead) })
	search := <-searchDone
	if !errors.Is(search.err, providerErr) || !errors.Is(search.err, ErrInsufficientResults) || search.result.Status != "failed" {
		t.Fatal("original failure/shortfall changed", search)
	}
	if !importedEarly {
		if err := <-importDone; err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := importer.QueryRowContext(ctx, "SELECT status FROM topics WHERE id=1").Scan(&status); err != nil {
		t.Fatal(err)
	}
	available, err := AvailableReadings(ctx, importer, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, readingErr := reading.GetOrCreateDailyReading(ctx, importer, "sqlite", "2026-06-27")
	if importedEarly || status != "active" || available != 3 || readingErr != nil {
		t.Fatal("old finalization overwrote the imported catalog", importedEarly, status, available, readingErr)
	}
	assertFinalizationArchive(t, ctx, importer)
}

func TestDiscoveryFinalizationRechecksImportedReadiness(t *testing.T) {
	for _, tc := range []struct {
		name               string
		before, after      int
		wantRun, wantTopic string
		shortfall          bool
	}{
		{"catalog grows", 1, 3, "completed", "active", false},
		{"catalog shrinks", 3, 1, "failed", "failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			importHeld, releaseImport := make(chan struct{}), make(chan struct{})
			finalRead, releaseRead := make(chan struct{}), make(chan struct{})
			var unblockImport, unblockRead sync.Once
			var reads atomic.Int32
			id := finalizationFunctionID.Add(1)
			importFunction, readFunction := fmt.Sprintf("finalization_import_%d", id), fmt.Sprintf("finalization_observe_%d", id)
			if err := sqlite.RegisterScalarFunction(importFunction, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
				close(importHeld)
				select {
				case <-releaseImport:
				case <-ctx.Done():
				}
				return int64(0), nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := sqlite.RegisterScalarFunction(readFunction, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				// The first group-read observes the old catalog through WAL. The
				// final count must wait for the importer before opening its snapshot.
				if reads.Add(1) == int32(tc.before+1) {
					close(finalRead)
					select {
					case <-releaseRead:
					case <-ctx.Done():
					}
				}
				return args[0], nil
			}); err != nil {
				t.Fatal(err)
			}
			old, importer := finalizationDBs(t, ctx, tc.before)
			if _, err := old.Exec("CREATE TEMP VIEW pages AS SELECT topic_id,active," + readFunction + "(url) AS url FROM main.pages"); err != nil {
				t.Fatal(err)
			}
			if _, err := importer.Exec("CREATE TEMP TRIGGER hold_import BEFORE UPDATE OF status ON main.topics BEGIN SELECT " + importFunction + "(); END"); err != nil {
				t.Fatal(err)
			}
			providerStarted, releaseProvider := make(chan struct{}), make(chan struct{})
			var unblockProvider sync.Once
			var jobs sync.WaitGroup
			defer func() {
				unblockProvider.Do(func() { close(releaseProvider) })
				unblockImport.Do(func() { close(releaseImport) })
				unblockRead.Do(func() { close(releaseRead) })
				jobs.Wait()
			}()
			searchDone := make(chan finalizationOutcome, 1)
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				provider := finalizationProvider(func(context.Context, string, int) ([]SearchResult, error) {
					close(providerStarted)
					<-releaseProvider
					return nil, nil
				})
				result, err := SearchTopic(ctx, old, "SQLite", Options{Provider: provider, Now: fixedTopicSearchTime})
				searchDone <- finalizationOutcome{result, err}
			}()
			select {
			case <-providerStarted:
			case <-ctx.Done():
				t.Fatal("search did not reach its provider")
			}
			importDone := make(chan error, 1)
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				_, err := seed.ImportTopic(ctx, importer, finalizationCatalog(tc.after))
				importDone <- err
			}()
			select {
			case <-importHeld:
			case <-ctx.Done():
				t.Fatal("import did not acquire its writer")
			}
			unblockProvider.Do(func() { close(releaseProvider) })
			readBeforeImport := false
			select {
			case <-finalRead:
				readBeforeImport = true
			case <-time.After(time.Second):
			}
			unblockImport.Do(func() { close(releaseImport) })
			if err := <-importDone; err != nil {
				t.Fatal(err)
			}
			unblockRead.Do(func() { close(releaseRead) })
			search := <-searchDone
			var status, runStatus, diagnostic string
			if err := importer.QueryRowContext(ctx, `SELECT t.status,r.status,r.error FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE r.id=?`, search.result.RunID).Scan(&status, &runStatus, &diagnostic); err != nil {
				t.Fatal(err)
			}
			if readBeforeImport || search.result.Status != tc.wantRun || runStatus != tc.wantRun || status != tc.wantTopic || errors.Is(search.err, ErrInsufficientResults) != tc.shortfall {
				t.Fatal("final outcome used pre-import readiness", readBeforeImport, search, status, runStatus, diagnostic)
			}
			if !tc.shortfall && (search.err != nil || diagnostic != "") || tc.shortfall && diagnostic != search.err.Error() {
				t.Fatal("returned and persisted outcome disagree", search.err, diagnostic)
			}
			assertFinalizationArchive(t, ctx, importer)
		})
	}
}

func TestDiscoveryFinalizationRollsBackOutcomeOnCatalogFailure(t *testing.T) {
	ctx := context.Background()
	conn, observer := finalizationDBs(t, ctx, 2)
	if _, err := conn.Exec(`UPDATE topics SET status='searching';
		INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at) VALUES(1,1,'fake','retry','running','reviewing','2026-06-29 12:00:00');
		CREATE TRIGGER fail_finalization BEFORE UPDATE OF status ON topics WHEN NEW.status='active' BEGIN SELECT RAISE(ABORT,'injected catalog failure'); END`); err != nil {
		t.Fatal(err)
	}
	providerErr := errors.New("source unavailable")
	input := Result{TopicID: 1, TopicSlug: "sqlite", TopicName: "SQLite", RunID: 1, Status: "failed", ResultCount: 4, StoredCount: 2}
	usage := ReviewOutput{Model: "fixture", TotalTokens: 100}
	if _, err := finishDiscovery(ctx, conn, input, usage, providerErr); !errors.Is(err, providerErr) || !strings.Contains(err.Error(), "injected catalog failure") {
		t.Fatal("finalization lost the provider or database failure", err)
	}
	var untouched int
	if err := observer.QueryRow(`SELECT count(*) FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.status='searching' AND r.status='running' AND r.stage='reviewing' AND r.completed_at IS NULL AND r.error='' AND r.result_count=0 AND r.stored_count=0 AND r.reviewer_total_tokens=0`).Scan(&untouched); err != nil || untouched != 1 {
		t.Fatal("catalog failure partially finalized the attempt", untouched, err)
	}
	if _, err := conn.Exec("DROP TRIGGER fail_finalization"); err != nil {
		t.Fatal(err)
	}
	result, err := finishDiscovery(ctx, conn, input, usage, providerErr)
	if !errors.Is(err, providerErr) || result.Status != "failed" || errors.Is(err, ErrInsufficientResults) {
		t.Fatal("retry changed the outcome for a useful catalog", result, err)
	}
	var finalized int
	if err := observer.QueryRow(`SELECT count(*) FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.status='active' AND r.status='failed' AND r.stage='' AND r.completed_at IS NOT NULL AND r.error=? AND r.result_count=4 AND r.stored_count=2 AND r.reviewer_total_tokens=100`, providerErr.Error()).Scan(&finalized); err != nil || finalized != 1 {
		t.Fatal("retry did not persist the consistent outcome", finalized, err)
	}
	assertFinalizationArchive(t, ctx, observer)
}
