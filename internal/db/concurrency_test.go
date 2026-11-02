package db_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
)

func TestPooledConnectionPreservesReadingReferences(t *testing.T) {
	ctx := context.Background()
	conn := openReadingDB(t)
	first, err := conn.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// Keeping the first connection busy forces database/sql to open another.
	second, err := conn.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.ExecContext(ctx, "INSERT INTO daily_readings (topic_id, reading_date, page_id) VALUES (999, '2026-11-02', 999)"); err == nil {
		t.Fatal("a pooled connection accepted a reading with nonexistent topic and page")
	}
	if _, err := second.ExecContext(ctx, "DELETE FROM pages WHERE id=1"); err == nil {
		t.Fatal("a pooled connection deleted a page referenced by a historical reading")
	}
	if _, err := reading.GetDailyReading(ctx, conn, "sqlite", "2026-11-01"); err != nil {
		t.Fatalf("historical reading must remain available: %v", err)
	}
}

func TestDailyReadingWaitsForBriefWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := openReadingDB(t)
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, "UPDATE topics SET description='Updating catalog' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reading.GetOrCreateDailyReading(ctx, conn, "sqlite", "2026-11-02")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("reading did not wait for the brief writer: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reading failed after writer completed: %v", err)
	}
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings WHERE reading_date='2026-11-02'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one shared daily assignment, count=%d err=%v", count, err)
	}
}

func TestConcurrentReadersShareOneNewAssignment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := openReadingDB(t)
	const readers = 16
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(readers)
	results := make(chan reading.Reading, readers)
	errors := make(chan error, readers)
	for range readers {
		go func() {
			ready.Done()
			<-start
			got, err := reading.GetOrCreateDailyReading(ctx, conn, "sqlite", "2026-11-02")
			results <- got
			errors <- err
		}()
	}
	ready.Wait()
	close(start)
	for range readers {
		if err := <-errors; err != nil {
			t.Errorf("concurrent reading failed: %v", err)
		}
		if got := <-results; got.PageID != 1 || got.Date != "2026-11-02" {
			t.Errorf("unexpected shared reading: %+v", got)
		}
	}
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings WHERE reading_date='2026-11-02'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one shared daily assignment, count=%d err=%v", count, err)
	}
}

func TestSavedReadingsRemainAvailableDuringWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := openReadingDB(t)
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, "UPDATE topics SET description='Updating catalog' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reading.GetDailyReading(ctx, conn, "sqlite", "2026-11-01")
		if err == nil {
			_, err = reading.GetOrCreateDailyReading(ctx, conn, "sqlite", "2026-11-01")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("saved reading failed while catalog write was open: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("saved readings blocked on an unrelated writer")
	}
}

func openReadingDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "reading.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Exec(`
		INSERT INTO topics (id,slug,name) VALUES (1,'sqlite','SQLite');
		INSERT INTO pages (id,topic_id,title,url,reading_order) VALUES (1,1,'WAL','https://sqlite.org/wal.html',1);
		INSERT INTO daily_readings (topic_id,reading_date,page_id) VALUES (1,'2026-11-01',1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
