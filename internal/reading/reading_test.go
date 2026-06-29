package reading

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/seed"
)

func TestGetDailyReadingReturnsStoredAssignment(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()
	importTopic(t, ctx, conn, "go", []seed.PageFile{
		{Title: "First", URL: "https://go.dev/first"},
		{Title: "Second", URL: "https://go.dev/second", EstimatedMinutes: intPtr(8), Official: true},
		{Title: "Third", URL: "https://go.dev/third"},
	})
	seedDailyReading(t, ctx, conn, "go", "1970-01-02", "Second")

	got, err := GetDailyReading(ctx, conn, "go", "1970-01-02")
	if err != nil {
		t.Fatalf("get daily reading: %v", err)
	}
	if got.Title != "Second" {
		t.Fatalf("expected second page, got %q", got.Title)
	}
	if got.EstimatedMinutes == nil || *got.EstimatedMinutes != 8 {
		t.Fatalf("expected estimated minutes 8, got %#v", got.EstimatedMinutes)
	}
	if !got.Official {
		t.Fatal("expected official page")
	}

	var assignments int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings").Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 1 {
		t.Fatalf("expected one assignment, got %d", assignments)
	}
}

func TestGetDailyReadingDoesNotCreateMissingAssignment(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()
	importTopic(t, ctx, conn, "go", []seed.PageFile{
		{Title: "First", URL: "https://go.dev/first"},
		{Title: "Second", URL: "https://go.dev/second"},
	})

	if _, err := GetDailyReading(ctx, conn, "go", "1970-01-02"); !errors.Is(err, ErrReadingNotFound) {
		t.Fatalf("expected ErrReadingNotFound, got %v", err)
	}
}

func TestGetOrCreateDailyReadingCreatesStableAssignment(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()
	importTopic(t, ctx, conn, "go", []seed.PageFile{
		{Title: "First", URL: "https://go.dev/first"},
		{Title: "Second", URL: "https://go.dev/second", EstimatedMinutes: intPtr(8), Official: true},
		{Title: "Third", URL: "https://go.dev/third"},
	})

	got, err := GetOrCreateDailyReading(ctx, conn, "go", "1970-01-02")
	if err != nil {
		t.Fatalf("get daily reading: %v", err)
	}
	if got.Title != "Second" {
		t.Fatalf("expected second page, got %q", got.Title)
	}

	again, err := GetOrCreateDailyReading(ctx, conn, "go", "1970-01-02")
	if err != nil {
		t.Fatalf("get daily reading again: %v", err)
	}
	if again.PageID != got.PageID {
		t.Fatalf("expected same page id, got %d then %d", got.PageID, again.PageID)
	}

	var assignments int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings").Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 1 {
		t.Fatalf("expected one assignment, got %d", assignments)
	}
}

func TestGetDailyReadingPreservesHistoricalAssignmentAfterPagesChange(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()
	importTopic(t, ctx, conn, "sqlite", []seed.PageFile{
		{Title: "WAL", URL: "https://sqlite.org/wal.html"},
		{Title: "Indexes", URL: "https://sqlite.org/partialindex.html"},
	})
	seedDailyReading(t, ctx, conn, "sqlite", "1970-01-01", "WAL")

	first, err := GetDailyReading(ctx, conn, "sqlite", "1970-01-01")
	if err != nil {
		t.Fatalf("get first reading: %v", err)
	}
	if first.Title != "WAL" {
		t.Fatalf("expected WAL, got %q", first.Title)
	}

	importTopic(t, ctx, conn, "sqlite", []seed.PageFile{
		{Title: "Indexes", URL: "https://sqlite.org/partialindex.html"},
		{Title: "Vacuum", URL: "https://sqlite.org/lang_vacuum.html"},
	})

	afterChange, err := GetDailyReading(ctx, conn, "sqlite", "1970-01-01")
	if err != nil {
		t.Fatalf("get reading after page change: %v", err)
	}
	if afterChange.PageID != first.PageID || afterChange.Title != "WAL" {
		t.Fatalf("expected historical WAL assignment, got %+v", afterChange)
	}
}

func TestGetDailyReadingRejectsMissingTopicAndInvalidDate(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()

	if _, err := GetDailyReading(ctx, conn, "missing", "2026-06-27"); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("expected ErrTopicNotFound, got %v", err)
	}

	if _, err := GetDailyReading(ctx, conn, "missing", "2026-6-27"); !errors.Is(err, ErrInvalidDate) {
		t.Fatalf("expected ErrInvalidDate, got %v", err)
	}
}

func TestGetDailyReadingRejectsActiveTopicWithoutAssignment(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name) VALUES ('rust', 'Rust')"); err != nil {
		t.Fatalf("insert topic: %v", err)
	}

	if _, err := GetDailyReading(ctx, conn, "rust", "2026-06-27"); !errors.Is(err, ErrReadingNotFound) {
		t.Fatalf("expected ErrReadingNotFound, got %v", err)
	}
}

func TestGetOrCreateDailyReadingRejectsTopicWithoutActivePages(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "INSERT INTO topics (slug, name) VALUES ('rust', 'Rust')"); err != nil {
		t.Fatalf("insert topic: %v", err)
	}

	if _, err := GetOrCreateDailyReading(ctx, conn, "rust", "2026-06-27"); !errors.Is(err, ErrNoActivePages) {
		t.Fatalf("expected ErrNoActivePages, got %v", err)
	}
}

func openTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}

func importTopic(t *testing.T, ctx context.Context, conn *sql.DB, slug string, pages []seed.PageFile) {
	t.Helper()

	if _, err := seed.ImportTopic(ctx, conn, seed.TopicFile{
		Topic: slug,
		Name:  slug,
		Pages: pages,
	}); err != nil {
		t.Fatalf("import topic: %v", err)
	}
}

func seedDailyReading(t *testing.T, ctx context.Context, conn *sql.DB, slug string, date string, title string) {
	t.Helper()

	_, err := conn.ExecContext(ctx, `INSERT INTO daily_readings (topic_id, reading_date, page_id) SELECT t.id, ?, p.id FROM topics t JOIN pages p ON p.topic_id = t.id WHERE t.slug = ? AND p.title = ?`, date, slug, title)
	if err != nil {
		t.Fatalf("seed daily reading: %v", err)
	}
}

func intPtr(value int) *int {
	return &value
}
