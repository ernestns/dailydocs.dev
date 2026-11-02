package validator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const DefaultFailureThreshold = 3

type Result struct {
	Checked  int
	Healthy  int
	Failed   int
	Disabled int
	Skipped  int
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
			applied, disabled, recordErr := recordFailure(ctx, conn, page, err, failureThreshold)
			if recordErr != nil {
				return result, recordErr
			}
			if !applied {
				result.Skipped++
				continue
			}
			result.Failed++
			if disabled {
				result.Disabled++
			}
			continue
		}

		applied, err := recordSuccess(ctx, conn, page, finalURL)
		if err != nil {
			return result, err
		}
		if !applied {
			result.Skipped++
			continue
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

	// A reading can identify a section, not just a document. HTTP redirects
	// inherit the previous fragment unless Location explicitly supplies one
	// (including an empty #). Go's URL resolution alone drops that section.
	// Copy the client so concurrent validations and caller policy stay intact.
	requestClient := *client
	requestClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.Response != nil && len(via) > 0 {
			_, fragment, explicit := strings.Cut(next.Response.Header.Get("Location"), "#")
			if !explicit {
				previous := via[len(via)-1].URL
				next.URL.Fragment, next.URL.RawFragment = previous.Fragment, previous.RawFragment
			} else if fragment == "" {
				// URL resolution inherits for a bare #; HTTP explicitly clears it.
				next.URL.Fragment, next.URL.RawFragment = "", ""
			}
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(next, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("request %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	return resp.StatusCode, resp.Request.URL.String(), nil
}

func recordSuccess(ctx context.Context, conn *sql.DB, page pageLink, finalURL string) (bool, error) {
	// A redirect may lead to another saved page in this topic. Keep the working
	// alias in that case: merging/deleting rows would change reading history.
	// The checked URL may have changed while the HTTP request was in flight.
	result, err := conn.ExecContext(ctx, `
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
		WHERE id = ? AND url = ? AND active = 1
	`, finalURL, finalURL, page.ID, page.URL)
	if err != nil {
		return false, fmt.Errorf("record link success: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read link success update count: %w", err)
	}
	return rows != 0, nil
}

func recordFailure(ctx context.Context, conn *sql.DB, page pageLink, linkErr error, failureThreshold int) (applied, disabled bool, err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, false, fmt.Errorf("begin link failure update: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	result, err := tx.ExecContext(ctx, `
		UPDATE pages
		SET
			last_verified = datetime('now'),
			verification_failures = verification_failures + 1,
			last_error = ?,
			updated_at = datetime('now')
		WHERE id = ? AND url = ? AND active = 1
	`, linkErr.Error(), page.ID, page.URL)
	if err != nil {
		return false, false, fmt.Errorf("record link failure: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("read link failure update count: %w", err)
	}
	if rows == 0 {
		return false, false, nil
	}

	var failures int
	if err := tx.QueryRowContext(ctx, "SELECT verification_failures FROM pages WHERE id = ?", page.ID).Scan(&failures); err != nil {
		return false, false, fmt.Errorf("read link failure count: %w", err)
	}

	disabled = failures >= failureThreshold
	if disabled {
		if _, err := tx.ExecContext(ctx, "UPDATE pages SET active = 0, updated_at = datetime('now') WHERE id = ?", page.ID); err != nil {
			return false, false, fmt.Errorf("disable failed link: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, false, fmt.Errorf("commit link failure update: %w", err)
	}

	return true, disabled, nil
}
