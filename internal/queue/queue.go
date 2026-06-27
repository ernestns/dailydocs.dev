package queue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/ernestns/daily-docs/internal/pipeline"
)

type Processor func(context.Context, *sql.DB, int64) error

type Options struct {
	Limit        int
	LockDuration time.Duration
	WorkerID     string
	Processor    Processor
}

type Result struct {
	Claimed   int
	Processed int
	Failed    int
}

func ProcessPending(ctx context.Context, conn *sql.DB, opts Options) (Result, error) {
	if opts.Limit < 1 {
		opts.Limit = 5
	}
	if opts.LockDuration <= 0 {
		opts.LockDuration = 15 * time.Minute
	}
	if opts.WorkerID == "" {
		opts.WorkerID = randomWorkerID()
	}
	if opts.Processor == nil {
		opts.Processor = func(ctx context.Context, conn *sql.DB, submissionID int64) error {
			_, err := pipeline.ProcessSubmission(ctx, conn, submissionID, pipeline.Options{})
			return err
		}
	}

	var result Result
	for result.Claimed < opts.Limit {
		submissionID, ok, err := claimNext(ctx, conn, opts.WorkerID, opts.LockDuration)
		if err != nil {
			return result, err
		}
		if !ok {
			return result, nil
		}

		result.Claimed++
		if err := opts.Processor(ctx, conn, submissionID); err != nil {
			result.Failed++
			if recordErr := markFailed(ctx, conn, submissionID, err, opts.LockDuration); recordErr != nil {
				return result, recordErr
			}
			continue
		}
		if err := releaseLock(ctx, conn, submissionID); err != nil {
			return result, err
		}
		result.Processed++
	}

	return result, nil
}

func claimNext(ctx context.Context, conn *sql.DB, workerID string, lockDuration time.Duration) (int64, bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin submission claim: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var submissionID int64
	err = tx.QueryRowContext(ctx, `
		SELECT id
		FROM documentation_submissions
		WHERE status IN ('pending', 'failed')
			AND (locked_until IS NULL OR locked_until <= datetime('now'))
		ORDER BY last_submitted_at ASC, id ASC
		LIMIT 1
	`).Scan(&submissionID)
	if err == sql.ErrNoRows {
		if err := tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("commit empty claim: %w", err)
		}
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("select claim submission: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE documentation_submissions
		SET locked_at = datetime('now'),
			locked_until = datetime('now', ?),
			locked_by = ?,
			status = 'processing'
		WHERE id = ?
			AND status IN ('pending', 'failed')
			AND (locked_until IS NULL OR locked_until <= datetime('now'))
	`, fmt.Sprintf("+%d seconds", int(lockDuration.Seconds())), workerID, submissionID)
	if err != nil {
		return 0, false, fmt.Errorf("claim submission: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("read claim affected rows: %w", err)
	}
	if affected == 0 {
		if err := tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("commit missed claim: %w", err)
		}
		return 0, false, nil
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit claim: %w", err)
	}
	return submissionID, true, nil
}

func releaseLock(ctx context.Context, conn *sql.DB, submissionID int64) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE documentation_submissions
		SET locked_at = NULL,
			locked_until = NULL,
			locked_by = ''
		WHERE id = ?
	`, submissionID)
	if err != nil {
		return fmt.Errorf("release submission lock: %w", err)
	}
	return nil
}

func markFailed(ctx context.Context, conn *sql.DB, submissionID int64, processErr error, retryDelay time.Duration) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE documentation_submissions
		SET status = 'failed',
			last_error = ?,
			locked_at = NULL,
			locked_until = datetime('now', ?),
			locked_by = ''
		WHERE id = ?
	`, processErr.Error(), fmt.Sprintf("+%d seconds", int(retryDelay.Seconds())), submissionID)
	if err != nil {
		return fmt.Errorf("mark queued submission failed: %w", err)
	}
	return nil
}

func randomWorkerID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	return "worker-" + hex.EncodeToString(bytes[:])
}
