package reading

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidDate     = errors.New("invalid reading date")
	ErrNoActivePages   = errors.New("topic has no active pages")
	ErrReadingNotFound = errors.New("reading not found")
	ErrTopicNotFound   = errors.New("topic not found")
)

type Reading struct {
	TopicSlug        string
	TopicName        string
	Date             string
	PageID           int64
	Title            string
	URL              string
	Source           string
	Official         bool
	EstimatedMinutes *int
}

func GetDailyReading(ctx context.Context, conn *sql.DB, topicSlug string, date string) (Reading, error) {
	if _, err := dayNumber(date); err != nil {
		return Reading{}, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Reading{}, fmt.Errorf("begin daily reading: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	assigned, found, err := findAssignedReading(ctx, tx, topicSlug, date)
	if err != nil {
		return Reading{}, err
	}
	if !found {
		if err := requireActiveTopic(ctx, tx, topicSlug); err != nil {
			return Reading{}, err
		}
		return Reading{}, ErrReadingNotFound
	}

	if err := tx.Commit(); err != nil {
		return Reading{}, fmt.Errorf("commit daily reading: %w", err)
	}
	return assigned, nil
}

func GetOrCreateDailyReading(ctx context.Context, conn *sql.DB, topicSlug string, date string) (Reading, error) {
	dayNumber, err := dayNumber(date)
	if err != nil {
		return Reading{}, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Reading{}, fmt.Errorf("begin daily reading: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if existing, found, err := findAssignedReading(ctx, tx, topicSlug, date); err != nil {
		return Reading{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return Reading{}, fmt.Errorf("commit daily reading: %w", err)
		}
		return existing, nil
	}

	topicID, err := findActiveTopicID(ctx, tx, topicSlug)
	if err != nil {
		return Reading{}, err
	}
	pageID, err := selectPageID(ctx, tx, topicID, dayNumber)
	if err != nil {
		return Reading{}, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO daily_readings (topic_id, reading_date, page_id) VALUES (?, ?, ?)`, topicID, date, pageID); err != nil {
		return Reading{}, fmt.Errorf("insert daily reading: %w", err)
	}

	assigned, found, err := findAssignedReading(ctx, tx, topicSlug, date)
	if err != nil {
		return Reading{}, err
	}
	if !found {
		return Reading{}, fmt.Errorf("daily reading assignment was not created")
	}

	if err := tx.Commit(); err != nil {
		return Reading{}, fmt.Errorf("commit daily reading: %w", err)
	}
	return assigned, nil
}

func dayNumber(date string) (int, error) {
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalidDate, date)
	}
	return int(parsed.UTC().Unix() / 86400), nil
}

func findAssignedReading(ctx context.Context, tx *sql.Tx, topicSlug string, date string) (Reading, bool, error) {
	var reading Reading
	var official int

	err := tx.QueryRowContext(ctx, `
		SELECT
			t.slug,
			t.name,
			dr.reading_date,
			p.id,
			p.title,
			p.url,
			p.source,
			p.official,
			p.estimated_minutes
		FROM daily_readings dr
		JOIN topics t ON t.id = dr.topic_id
		JOIN pages p ON p.id = dr.page_id
		WHERE t.slug = ? AND dr.reading_date = ?
	`, topicSlug, date).Scan(
		&reading.TopicSlug,
		&reading.TopicName,
		&reading.Date,
		&reading.PageID,
		&reading.Title,
		&reading.URL,
		&reading.Source,
		&official,
		&reading.EstimatedMinutes,
	)
	if err == nil {
		reading.Official = official == 1
		return reading, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Reading{}, false, nil
	}
	return Reading{}, false, fmt.Errorf("find assigned reading: %w", err)
}

func requireActiveTopic(ctx context.Context, tx *sql.Tx, topicSlug string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM topics
		WHERE slug = ? AND status = 'active'
	`, topicSlug).Scan(&exists)
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTopicNotFound
	}
	return fmt.Errorf("find topic: %w", err)
}

func findActiveTopicID(ctx context.Context, tx *sql.Tx, topicSlug string) (int64, error) {
	var topicID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM topics WHERE slug = ? AND status = 'active'`, topicSlug).Scan(&topicID)
	if err == nil {
		return topicID, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrTopicNotFound
	}
	return 0, fmt.Errorf("find topic: %w", err)
}

func selectPageID(ctx context.Context, tx *sql.Tx, topicID int64, dayNumber int) (int64, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages WHERE topic_id = ? AND active = 1`, topicID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active pages: %w", err)
	}
	if count == 0 {
		return 0, ErrNoActivePages
	}

	offset := dayNumber % count
	if offset < 0 {
		offset += count
	}

	var pageID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM pages WHERE topic_id = ? AND active = 1 ORDER BY reading_order ASC, id ASC LIMIT 1 OFFSET ?`, topicID, offset).Scan(&pageID); err != nil {
		return 0, fmt.Errorf("select page: %w", err)
	}
	return pageID, nil
}
