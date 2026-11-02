package seed

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/ernestns/daily-docs/internal/topicname"
	"gopkg.in/yaml.v3"
)

var topicSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type TopicFile struct {
	Topic       string     `yaml:"topic"`
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Pages       []PageFile `yaml:"pages"`
}

type PageFile struct {
	Title            string `yaml:"title"`
	URL              string `yaml:"url"`
	Source           string `yaml:"source"`
	Official         bool   `yaml:"official"`
	EstimatedMinutes *int   `yaml:"estimated_minutes"`
	Difficulty       string `yaml:"difficulty"`
	EvergreenScore   *int   `yaml:"evergreen_score"`
}

type Result struct {
	TopicSlug     string
	PagesFound    int
	PagesImported int
}

func ImportFile(ctx context.Context, conn *sql.DB, path string) (Result, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("read seed file: %w", err)
	}

	var topicFile TopicFile
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(&topicFile); err != nil {
		return Result{}, fmt.Errorf("parse seed file: %w", err)
	}
	// A seed file replaces one topic's active catalog. Reject trailing documents
	// before that replacement rather than silently importing only the first one.
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Result{}, fmt.Errorf("parse seed file: %w", err)
		}
		return Result{}, errors.New("seed file must contain exactly one topic document")
	}

	if err := topicFile.Validate(); err != nil {
		return Result{}, err
	}

	return ImportTopic(ctx, conn, topicFile)
}

func ImportTopic(ctx context.Context, conn *sql.DB, topicFile TopicFile) (Result, error) {
	if err := topicFile.Validate(); err != nil {
		return Result{}, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin import: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	importID, err := createImport(ctx, tx, topicFile.Topic, len(topicFile.Pages))
	if err != nil {
		return Result{}, err
	}

	topicID, err := upsertTopic(ctx, tx, topicFile)
	if err != nil {
		_ = failImport(ctx, tx, importID, err)
		return Result{}, err
	}

	if err := moveExistingReadingOrder(ctx, tx, topicID); err != nil {
		_ = failImport(ctx, tx, importID, err)
		return Result{}, err
	}

	imported := 0
	activeURLs := make([]string, 0, len(topicFile.Pages))
	for i, page := range topicFile.Pages {
		activeURLs = append(activeURLs, page.URL)
		if err := upsertPage(ctx, tx, topicID, page, i+1); err != nil {
			_ = failImport(ctx, tx, importID, err)
			return Result{}, err
		}
		imported++
	}

	if err := deactivateMissingPages(ctx, tx, topicID, activeURLs); err != nil {
		_ = failImport(ctx, tx, importID, err)
		return Result{}, err
	}

	if err := completeImport(ctx, tx, importID, imported); err != nil {
		return Result{}, err
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit import: %w", err)
	}

	return Result{
		TopicSlug:     topicFile.Topic,
		PagesFound:    len(topicFile.Pages),
		PagesImported: imported,
	}, nil
}

func (tf TopicFile) Validate() error {
	if strings.TrimSpace(tf.Topic) == "" {
		return errors.New("topic is required")
	}
	if !topicSlugPattern.MatchString(tf.Topic) {
		return fmt.Errorf("topic %q must be a lowercase slug", tf.Topic)
	}
	if topicname.IsReservedSlug(tf.Topic) {
		return fmt.Errorf("topic %q is reserved for an application route; choose a different slug such as %q", tf.Topic, tf.Topic+"-topic")
	}
	if strings.TrimSpace(tf.Name) == "" {
		return errors.New("name is required")
	}
	if len(tf.Pages) == 0 {
		return errors.New("at least one page is required")
	}

	seenURLs := map[string]struct{}{}
	for i, page := range tf.Pages {
		if err := page.Validate(); err != nil {
			return fmt.Errorf("page %d: %w", i+1, err)
		}
		if _, exists := seenURLs[page.URL]; exists {
			return fmt.Errorf("page %d: duplicate url %q", i+1, page.URL)
		}
		seenURLs[page.URL] = struct{}{}
	}

	return nil
}

func (pf PageFile) Validate() error {
	if strings.TrimSpace(pf.Title) == "" {
		return errors.New("title is required")
	}
	if strings.TrimSpace(pf.URL) == "" {
		return errors.New("url is required")
	}
	parsed, err := url.Parse(pf.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("url %q must be absolute", pf.URL)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("url %q must use http or https", pf.URL)
	}
	if pf.EstimatedMinutes != nil && *pf.EstimatedMinutes < 1 {
		return errors.New("estimated_minutes must be positive")
	}
	if pf.EvergreenScore != nil && (*pf.EvergreenScore < 0 || *pf.EvergreenScore > 100) {
		return errors.New("evergreen_score must be between 0 and 100")
	}
	return nil
}

func createImport(ctx context.Context, tx *sql.Tx, topic string, pagesFound int) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO imports (topic, status, pages_found)
		VALUES (?, 'running', ?)
	`, topic, pagesFound)
	if err != nil {
		return 0, fmt.Errorf("record import start: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read import id: %w", err)
	}
	return id, nil
}

func failImport(ctx context.Context, tx *sql.Tx, importID int64, importErr error) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE imports
		SET status = 'failed', completed_at = datetime('now'), error = ?
		WHERE id = ?
	`, importErr.Error(), importID)
	return err
}

func completeImport(ctx context.Context, tx *sql.Tx, importID int64, imported int) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE imports
		SET status = 'completed', completed_at = datetime('now'), pages_imported = ?
		WHERE id = ?
	`, imported, importID)
	if err != nil {
		return fmt.Errorf("record import completion: %w", err)
	}
	return nil
}

func upsertTopic(ctx context.Context, tx *sql.Tx, topicFile TopicFile) (int64, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO topics (slug, name, description, status)
		VALUES (?, ?, ?, 'active')
		ON CONFLICT(slug) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			status = 'active'
	`, topicFile.Topic, topicFile.Name, topicFile.Description)
	if err != nil {
		return 0, fmt.Errorf("upsert topic: %w", err)
	}

	var topicID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM topics WHERE slug = ?", topicFile.Topic).Scan(&topicID); err != nil {
		return 0, fmt.Errorf("read topic id: %w", err)
	}
	return topicID, nil
}

func moveExistingReadingOrder(ctx context.Context, tx *sql.Tx, topicID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE pages
		SET reading_order = -id
		WHERE topic_id = ?
	`, topicID)
	if err != nil {
		return fmt.Errorf("prepare reading order update: %w", err)
	}
	return nil
}

func upsertPage(ctx context.Context, tx *sql.Tx, topicID int64, page PageFile, readingOrder int) error {
	official := 0
	if page.Official {
		official = 1
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO pages (
			topic_id,
			title,
			url,
			source,
			official,
			estimated_minutes,
			difficulty,
			evergreen_score,
			reading_order,
			active,
			updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, datetime('now'))
		ON CONFLICT(topic_id, url) DO UPDATE SET
			title = excluded.title,
			source = excluded.source,
			official = excluded.official,
			estimated_minutes = excluded.estimated_minutes,
			difficulty = excluded.difficulty,
			evergreen_score = excluded.evergreen_score,
			reading_order = excluded.reading_order,
			active = 1,
			updated_at = datetime('now')
	`, topicID, page.Title, page.URL, page.Source, official, page.EstimatedMinutes, nullIfEmpty(page.Difficulty), page.EvergreenScore, readingOrder)
	if err != nil {
		return fmt.Errorf("upsert page %q: %w", page.URL, err)
	}
	return nil
}

func deactivateMissingPages(ctx context.Context, tx *sql.Tx, topicID int64, activeURLs []string) error {
	if len(activeURLs) == 0 {
		return nil
	}

	placeholders := make([]string, len(activeURLs))
	args := make([]any, 0, len(activeURLs)+1)
	args = append(args, topicID)
	for i, activeURL := range activeURLs {
		placeholders[i] = "?"
		args = append(args, activeURL)
	}

	query := fmt.Sprintf(`
		UPDATE pages
		SET active = 0, updated_at = datetime('now')
		WHERE topic_id = ? AND url NOT IN (%s)
	`, strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("deactivate omitted pages: %w", err)
	}
	return nil
}

func nullIfEmpty(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
