package topicsearch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ernestns/daily-docs/internal/topicname"
)

// ResolveTopic preserves existing named topics and their historical URL assignments.
func ResolveTopic(ctx context.Context, conn *sql.DB, value string) (string, string, error) {
	if err := topicname.Validate(value); err != nil {
		return "", "", err
	}
	name := displayTopicName(value, value)
	slug := topicname.Slug(value)
	var existingSlug, existingName string
	err := conn.QueryRowContext(ctx, "SELECT slug,name FROM topics WHERE lower(name)=lower(?) ORDER BY id LIMIT 1", name).Scan(&existingSlug, &existingName)
	if err == nil {
		return existingSlug, existingName, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	err = conn.QueryRowContext(ctx, "SELECT name FROM topics WHERE slug=?", slug).Scan(&existingName)
	if err == nil {
		if topicname.Slug(existingName) == slug {
			return slug, existingName, nil
		}
		// An older lossy slug may already belong to a different punctuated name.
		hash := sha256.Sum256([]byte(strings.ToLower(name)))
		slug = fmt.Sprintf("%s-%x", slug, hash[:4])
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	return slug, name, nil
}

// AvailableReadings counts the same distinct destinations used by discovery.
func AvailableReadings(ctx context.Context, conn *sql.DB, topicID int64) (int, error) {
	rows, err := conn.QueryContext(ctx, "SELECT url FROM pages WHERE topic_id=? AND active=1", topicID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return 0, err
		}
		seen[readingURLKey(u)] = true
	}
	return len(seen), rows.Err()
}

func discoverReadings(ctx context.Context, conn *sql.DB, topicID, runID int64, slug, name string, requests []SearchRequest, opts Options, maxResults, minScore, batchSize int) (Result, error) {
	result := Result{TopicID: topicID, TopicSlug: slug, TopicName: name, RunID: runID, Status: runStatusFailed}
	var usage ReviewOutput
	var failure error
	seen := map[string]bool{}
	// Review each small group before spending on the next one; never retry a failed provider automatically.
	for start := 0; start < len(requests); start += 3 {
		end := min(start+3, len(requests))
		raw, searchErr := executeSearchRequests(ctx, opts.Provider, requests[start:end])
		result.ResultCount += len(raw)
		var candidates []storedResult
		for _, candidate := range normalizeResults(raw) {
			key := readingURLKey(candidate.URL)
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, candidate)
			}
		}
		if len(candidates) > 0 {
			if err := saveCandidates(ctx, conn, topicID, runID, candidates); err != nil {
				failure = err
				break
			}
			var reviewed = candidates
			var reviewErr error
			if opts.Reviewer != nil {
				if ctx.Err() != nil {
					reviewed = unreviewedResults(candidates)
					reviewErr = ctx.Err()
				} else {
					if err := updateSearchRunStage(ctx, conn, runID, runStageReviewing); err != nil {
						failure = err
						break
					}
					var output ReviewOutput
					reviewed, output, reviewErr = reviewResults(ctx, name, opts.Reviewer, candidates, minScore, batchSize)
					mergeReviewUsage(&usage, output)
				}
			}
			remaining := maxResults - result.StoredCount
			if remaining <= 0 {
				for i := range reviewed {
					reviewed[i].Accepted = false
				}
			} else {
				reviewed = capAcceptedResults(reviewed, remaining)
			}
			storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			stored, err := storeReviewedResults(storeCtx, conn, topicID, runID, reviewed)
			cancel()
			result.StoredCount += stored
			failure = errors.Join(searchErr, reviewErr, err)
		} else {
			failure = searchErr
		}
		if failure != nil {
			break
		}
		available, err := AvailableReadings(ctx, conn, topicID)
		if err != nil {
			failure = err
			break
		}
		if available >= DefaultMaxResults || result.StoredCount >= maxResults {
			break
		}
		if end < len(requests) {
			if err := updateSearchRunStage(ctx, conn, runID, runStageSearching); err != nil {
				failure = err
				break
			}
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	available, err := AvailableReadings(cleanup, conn, topicID)
	if err != nil {
		return result, errors.Join(failure, err)
	}
	if available < MinimumUsefulResults {
		failure = errors.Join(failure, fmt.Errorf("%w: found %d; need at least %d, target %d", ErrInsufficientResults, available, MinimumUsefulResults, DefaultMaxResults))
	}
	if failure == nil {
		result.Status = runStatusCompleted
	}
	if err := finishDiscovery(cleanup, conn, result, available, usage, failure); err != nil {
		return result, errors.Join(failure, err)
	}
	return result, failure
}

func saveCandidates(ctx context.Context, conn *sql.DB, topicID, runID int64, candidates []storedResult) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return storeSearchCandidates(cleanup, conn, topicID, runID, candidates)
}

func finishDiscovery(ctx context.Context, conn *sql.DB, result Result, available int, usage ReviewOutput, failure error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	diagnostic := ""
	if failure != nil {
		diagnostic = failure.Error()
	}
	_, err = tx.ExecContext(ctx, `UPDATE topic_search_runs SET status=?, stage='', completed_at=datetime('now'), result_count=?, stored_count=?, reviewer_model=?, reviewer_input_tokens=?, reviewer_output_tokens=?, reviewer_total_tokens=?, error=? WHERE id=?`, result.Status, result.ResultCount, result.StoredCount, usage.Model, usage.InputTokens, usage.OutputTokens, usage.TotalTokens, diagnostic, result.RunID)
	if err != nil {
		return err
	}
	status := "failed"
	if available >= MinimumUsefulResults {
		status = "active"
	}
	if _, err = tx.ExecContext(ctx, "UPDATE topics SET status=?,updated_at=datetime('now') WHERE id=?", status, result.TopicID); err != nil {
		return err
	}
	return tx.Commit()
}

func mergeReviewUsage(total *ReviewOutput, part ReviewOutput) {
	total.InputTokens += part.InputTokens
	total.OutputTokens += part.OutputTokens
	total.TotalTokens += part.TotalTokens
	if total.Model == "" {
		total.Model = part.Model
	} else if part.Model != "" && part.Model != total.Model {
		total.Model = "multiple"
	}
}

func unreviewedResults(results []storedResult) []storedResult {
	copy := append([]storedResult(nil), results...)
	for i := range copy {
		copy[i].Accepted = false
		copy[i].Reviewed = false
		copy[i].Score = 0
		copy[i].PageType = ""
		copy[i].Reason = ""
	}
	return copy
}
