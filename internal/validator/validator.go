package validator

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

const DefaultFailureThreshold = 3

type Result struct {
	Checked  int
	Healthy  int
	Failed   int
	Disabled int
}

type pageLink struct {
	ID  int64
	URL string
}

func ValidateLinks(ctx context.Context, conn *sql.DB, client *http.Client, failureThreshold int) (Result, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if failureThreshold < 1 {
		failureThreshold = DefaultFailureThreshold
	}

	pages, err := activePages(ctx, conn)
	if err != nil {
		return Result{}, err
	}

	var result Result
	for _, page := range pages {
		result.Checked++
		finalURL, err := checkURL(ctx, client, page.URL)
		if err != nil {
			disabled, recordErr := recordFailure(ctx, conn, page.ID, err, failureThreshold)
			if recordErr != nil {
				return result, recordErr
			}
			result.Failed++
			if disabled {
				result.Disabled++
			}
			continue
		}

		if err := recordSuccess(ctx, conn, page.ID, finalURL); err != nil {
			return result, err
		}
		result.Healthy++
	}

	return result, nil
}

func activePages(ctx context.Context, conn *sql.DB) ([]pageLink, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, url
		FROM pages
		WHERE active = 1
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query active pages: %w", err)
	}
	defer rows.Close()

	var pages []pageLink
	for rows.Next() {
		var page pageLink
		if err := rows.Scan(&page.ID, &page.URL); err != nil {
			return nil, fmt.Errorf("scan active page: %w", err)
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active pages: %w", err)
	}
	return pages, nil
}

func checkURL(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	status, finalURL, err := requestURL(ctx, client, http.MethodHead, rawURL)
	if err != nil {
		return "", err
	}
	if status == http.StatusMethodNotAllowed {
		status, finalURL, err = requestURL(ctx, client, http.MethodGet, rawURL)
		if err != nil {
			return "", err
		}
	}
	if status < 200 || status >= 400 {
		return "", fmt.Errorf("unexpected status %d", status)
	}
	return finalURL, nil
}

func requestURL(ctx context.Context, client *http.Client, method string, rawURL string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, "", fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("request %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	return resp.StatusCode, resp.Request.URL.String(), nil
}

func recordSuccess(ctx context.Context, conn *sql.DB, pageID int64, finalURL string) error {
	// A redirect may lead to another saved page in this topic. Keep the working
	// alias in that case: merging/deleting rows would change reading history.
	_, err := conn.ExecContext(ctx, `
		UPDATE pages AS current
		SET
			url = CASE WHEN EXISTS (
				SELECT 1 FROM pages AS destination
				WHERE destination.topic_id = current.topic_id
					AND destination.url = ? AND destination.id != current.id
			) THEN current.url ELSE ? END,
			last_verified = datetime('now'),
			verification_failures = 0,
			last_error = '',
			updated_at = datetime('now')
		WHERE id = ?
	`, finalURL, finalURL, pageID)
	if err != nil {
		return fmt.Errorf("record link success: %w", err)
	}
	return nil
}

func recordFailure(ctx context.Context, conn *sql.DB, pageID int64, linkErr error, failureThreshold int) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin link failure update: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	_, err = tx.ExecContext(ctx, `
		UPDATE pages
		SET
			last_verified = datetime('now'),
			verification_failures = verification_failures + 1,
			last_error = ?,
			updated_at = datetime('now')
		WHERE id = ?
	`, linkErr.Error(), pageID)
	if err != nil {
		return false, fmt.Errorf("record link failure: %w", err)
	}

	var failures int
	if err := tx.QueryRowContext(ctx, "SELECT verification_failures FROM pages WHERE id = ?", pageID).Scan(&failures); err != nil {
		return false, fmt.Errorf("read link failure count: %w", err)
	}

	disabled := failures >= failureThreshold
	if disabled {
		if _, err := tx.ExecContext(ctx, "UPDATE pages SET active = 0, updated_at = datetime('now') WHERE id = ?", pageID); err != nil {
			return false, fmt.Errorf("disable failed link: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit link failure update: %w", err)
	}

	return disabled, nil
}
