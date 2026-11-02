package topicsearch

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
)

func staleCatalogFixture(t *testing.T, conn *sql.DB, status string, urls []string, stale, fresh bool) []reading.Reading {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(`
		INSERT INTO topics(id,slug,name,status) VALUES(1,'sqlite','SQLite',?);
		INSERT INTO pages(id,topic_id,title,url,reading_order,active) VALUES(11,1,'Archived reading','https://sqlite.org/archived.html',1,0);
		INSERT INTO topic_search_runs(id,topic_id,provider,query,status,started_at,completed_at,stored_count)
		VALUES(1,1,'fixture','original','completed','2026-01-01 00:00:00','2026-01-01 00:01:00',2);
	`, status); err != nil {
		t.Fatal(err)
	}
	for i, raw := range urls {
		if _, err := conn.Exec("INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(?,1,?,?,?)", 12+i, fmt.Sprintf("Reading %d", i), raw, i+2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO daily_readings(id,topic_id,reading_date,page_id) SELECT id,topic_id,date('2026-06-01','+' || (reading_order-1) || ' days'),id FROM pages`); err != nil {
		t.Fatal(err)
	}
	for i, enabled := range []bool{stale, fresh} {
		if !enabled {
			continue
		}
		started := fixedTopicSearchTime().Add(-time.Hour)
		if i == 1 {
			started = fixedTopicSearchTime().Add(-time.Minute)
		}
		if _, err := conn.Exec(`INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at) VALUES(?,1,'fake','retry','running','reviewing',?)`, i+2, formatTime(started)); err != nil {
			t.Fatal(err)
		}
	}
	var saved []reading.Reading
	for day := 1; day <= len(urls)+1; day++ {
		got, err := reading.GetDailyReading(ctx, conn, "sqlite", fmt.Sprintf("2026-06-%02d", day))
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, got)
	}
	return saved
}

func assertStaleCatalogHistory(t *testing.T, conn *sql.DB, saved []reading.Reading) {
	t.Helper()
	for i, before := range saved {
		after, err := reading.GetDailyReading(context.Background(), conn, before.TopicSlug, before.Date)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("cleanup changed archived reading", before, after, err)
		}
		var order, active, assignmentID int
		if err := conn.QueryRow(`SELECT p.reading_order,p.active,d.id FROM pages p JOIN daily_readings d ON d.page_id=p.id WHERE p.id=?`, before.PageID).Scan(&order, &active, &assignmentID); err != nil {
			t.Fatal(err)
		}
		wantActive := 1
		if i == 0 {
			wantActive = 0
		}
		if order != i+1 || active != wantActive || int64(assignmentID) != before.PageID {
			t.Fatal("cleanup changed order/activity/assignment identity", order, active, assignmentID)
		}
	}
	var original, pages, assignments int
	if err := conn.QueryRow(`SELECT
		(SELECT count(*) FROM topic_search_runs WHERE id=1 AND status='completed' AND completed_at='2026-01-01 00:01:00' AND stored_count=2),
		(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings)
	`).Scan(&original, &pages, &assignments); err != nil || original != 1 || pages != len(saved) || assignments != len(saved) {
		t.Fatal("cleanup changed historical rows", original, pages, assignments, err)
	}
}

func TestStaleCleanupRestoresDistinctUsefulCatalog(t *testing.T) {
	const wal, plan = "https://sqlite.org/wal.html", "https://sqlite.org/queryplanner.html"
	for _, tc := range []struct {
		name, status, wantStatus string
		urls                     []string
		stale, fresh             bool
	}{
		{"two saved", "searching", "active", []string{wal, plan}, true, false},
		{"three saved", "searching", "active", []string{wal, plan, "https://sqlite.org/eqp.html"}, true, false},
		{"inactive archive only", "searching", "failed", nil, true, false},
		{"one active and inactive archive", "searching", "failed", []string{wal}, true, false},
		{"duplicate aliases", "searching", "failed", []string{wal, "http://www.sqlite.org/wal.html/?utm_source=feed#top"}, true, false},
		{"meaningful fragments", "searching", "active", []string{wal + "#one", wal + "#two"}, true, false},
		{"orphan with useful pages", "searching", "active", []string{wal, plan}, false, false},
		{"orphan with one page", "searching", "failed", []string{wal}, false, false},
		{"already active", "active", "active", nil, true, false},
		{"disabled", "disabled", "disabled", []string{wal, plan}, true, false},
		{"newer running attempt", "searching", "searching", []string{wal, plan}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			saved := staleCatalogFixture(t, conn, tc.status, tc.urls, tc.stale, tc.fresh)
			if err := ExpireStaleRunningSearches(ctx, conn, fixedTopicSearchTime()); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := conn.QueryRow("SELECT status FROM topics WHERE id=1").Scan(&status); err != nil || status != tc.wantStatus {
				t.Fatal("stale cleanup hid or incorrectly activated catalog", status, tc.wantStatus, err)
			}
			if tc.stale {
				var finalized int
				if err := conn.QueryRow(`SELECT count(*) FROM topic_search_runs WHERE id=2 AND status='failed' AND stage='' AND completed_at=? AND error='stale running search timed out'`, formatTime(fixedTopicSearchTime())).Scan(&finalized); err != nil || finalized != 1 {
					t.Fatal("stale attempt was not finalized honestly", finalized, err)
				}
			}
			if tc.fresh {
				var running int
				if err := conn.QueryRow(`SELECT count(*) FROM topic_search_runs WHERE id=3 AND status='running' AND stage='reviewing' AND completed_at IS NULL AND error=''`).Scan(&running); err != nil || running != 1 {
					t.Fatal("cleanup changed the fresh attempt", running, err)
				}
			}
			assertStaleCatalogHistory(t, conn, saved)
		})
	}
}

func TestStaleCleanupRechecksCatalogAfterWriter(t *testing.T) {
	for _, tc := range []struct {
		name, mutation, wantStatus string
	}{
		{"disabled", "UPDATE topics SET status='disabled' WHERE id=1", "disabled"},
		{"reading deactivated", "UPDATE pages SET active=0 WHERE id=13", "failed"},
		{"new running attempt", "INSERT INTO topic_search_runs(id,topic_id,provider,query,status,stage,started_at) VALUES(3,1,'fake','new retry','running','searching','2026-06-29 11:59:00')", "searching"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(2)
			staleCatalogFixture(t, conn, "searching", []string{"https://sqlite.org/wal.html", "https://sqlite.org/queryplanner.html"}, true, false)
			writer, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(ctx, tc.mutation); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- ExpireStaleRunningSearches(ctx, conn, fixedTopicSearchTime()) }()
			deadline := time.Now().Add(time.Second)
			for conn.Stats().InUse < 2 {
				if time.Now().After(deadline) {
					t.Fatal("cleanup did not reach the held writer")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-done:
				t.Fatal("cleanup returned before the writer committed", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := writer.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var status string
			if err := conn.QueryRow("SELECT status FROM topics WHERE id=1").Scan(&status); err != nil || status != tc.wantStatus {
				t.Fatal("cleanup used stale catalog eligibility", status, tc.wantStatus, err)
			}
			archived, err := reading.GetDailyReading(ctx, conn, "sqlite", "2026-06-01")
			if err != nil || archived.PageID != 11 || archived.URL != "https://sqlite.org/archived.html" {
				t.Fatal("waiting cleanup changed archive", archived, err)
			}
		})
	}
}

func TestStaleCleanupRollsBackWhenCatalogRecoveryFails(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	saved := staleCatalogFixture(t, conn, "searching", []string{"https://sqlite.org/wal.html", "https://sqlite.org/queryplanner.html"}, true, false)
	if _, err := conn.Exec(`CREATE TRIGGER reject_catalog_recovery BEFORE UPDATE OF status ON topics WHEN NEW.status='active' BEGIN SELECT RAISE(ABORT,'injected catalog failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := ExpireStaleRunningSearches(ctx, conn, fixedTopicSearchTime()); err == nil {
		t.Fatal("catalog recovery failure was hidden")
	}
	var unfinished int
	if err := conn.QueryRow(`SELECT count(*) FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.status='searching' AND r.id=2 AND r.status='running' AND r.stage='reviewing' AND r.completed_at IS NULL AND r.error=''`).Scan(&unfinished); err != nil || unfinished != 1 {
		t.Fatal("failed catalog recovery partially finalized the attempt", unfinished, err)
	}
	assertStaleCatalogHistory(t, conn, saved)
	if _, err := conn.Exec("DROP TRIGGER reject_catalog_recovery"); err != nil {
		t.Fatal(err)
	}
	if err := ExpireStaleRunningSearches(ctx, conn, fixedTopicSearchTime()); err != nil {
		t.Fatal(err)
	}
	var recovered int
	if err := conn.QueryRow(`SELECT count(*) FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id WHERE t.status='active' AND r.id=2 AND r.status='failed' AND r.stage='' AND r.error='stale running search timed out'`).Scan(&recovered); err != nil || recovered != 1 {
		t.Fatal("recovery retry did not finalize catalog and attempt", recovered, err)
	}
	assertStaleCatalogHistory(t, conn, saved)
}
