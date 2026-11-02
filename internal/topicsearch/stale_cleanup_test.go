package topicsearch

import (
	"context"
	"testing"
	"time"
)

func TestExpireStaleRunningSearchesKeepsCleanupCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, topicStatus, runStatus, wantTopic, wantRun string
	}{
		{"stale active topic", "active", "running", "active", "failed"},
		{"stale searching topic", "searching", "running", "failed", "failed"},
		{"orphan without run", "searching", "", "failed", ""},
		{"orphan with completed run", "searching", "completed", "failed", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			now := fixedTopicSearchTime()
			if _, err := conn.ExecContext(ctx, `INSERT INTO topics(slug,name,status) VALUES('rust','Rust',?)`, tc.topicStatus); err != nil {
				t.Fatal(err)
			}
			if tc.runStatus != "" {
				if _, err := conn.ExecContext(ctx, `INSERT INTO topic_search_runs(topic_id,provider,query,status,stage,started_at)
					VALUES(1,'fake','Rust',?,'reviewing',?)`, tc.runStatus, formatTime(now.Add(-time.Hour))); err != nil {
					t.Fatal(err)
				}
			}
			if err := ExpireStaleRunningSearches(ctx, conn, now); err != nil {
				t.Fatal(err)
			}
			var topicStatus, runStatus, stage, diagnostic, completed string
			if err := conn.QueryRowContext(ctx, `SELECT status,
				COALESCE((SELECT status FROM topic_search_runs),''),
				COALESCE((SELECT stage FROM topic_search_runs),''),
				COALESCE((SELECT error FROM topic_search_runs),''),
				COALESCE((SELECT completed_at FROM topic_search_runs),'')
				FROM topics WHERE id=1`).Scan(&topicStatus, &runStatus, &stage, &diagnostic, &completed); err != nil {
				t.Fatal(err)
			}
			if topicStatus != tc.wantTopic || runStatus != tc.wantRun {
				t.Fatalf("cleanup skipped stale/orphan state: topic=%s run=%s", topicStatus, runStatus)
			}
			if tc.runStatus == "running" && (stage != "" || diagnostic != "stale running search timed out" || completed != formatTime(now)) {
				t.Fatalf("stale run was not finalized: stage=%s error=%s completed=%s", stage, diagnostic, completed)
			}
			if tc.runStatus == "completed" && (stage != "reviewing" || diagnostic != "" || completed != "") {
				t.Fatal("orphan topic cleanup changed its completed run")
			}
		})
	}
}

func TestExpireStaleRunningSearchesDoesNotLockAtCutoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	now := fixedTopicSearchTime()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(slug,name,status) VALUES('rust','Rust','searching');
		INSERT INTO topic_search_runs(topic_id,provider,query,status,stage,started_at)
		VALUES(1,'fake','Rust','running','reviewing',?)
	`, formatTime(now.Add(-StaleRunTimeout))); err != nil {
		t.Fatal(err)
	}
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	done := make(chan error, 1)
	go func() { done <- ExpireStaleRunningSearches(ctx, conn, now) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = writer.Rollback()
		<-done
		t.Fatal("a run exactly at the cutoff incorrectly required cleanup")
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM topic_search_runs WHERE id=1`).Scan(&status); err != nil || status != "running" {
		t.Fatal("boundary run was changed", status, err)
	}
}

func TestExpireStaleRunningSearchesRechecksAfterWaitingForWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(2)
	now := fixedTopicSearchTime()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO topics(slug,name,status) VALUES('rust','Rust','searching');
		INSERT INTO topic_search_runs(topic_id,provider,query,status,stage,started_at)
		VALUES(1,'fake','Rust','running','reviewing',?)
	`, formatTime(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `
		UPDATE topics SET status='active',updated_at=? WHERE id=1;
		UPDATE topic_search_runs SET status='completed',stage='',completed_at=?,error='',stored_count=3 WHERE id=1
	`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	// The preflight can read the committed stale state through WAL, while the
	// completion remains uncommitted and keeps cleanup's write transaction out.
	done := make(chan error, 1)
	go func() { done <- ExpireStaleRunningSearches(ctx, conn, now) }()
	deadline := time.Now().Add(time.Second)
	for conn.Stats().InUse < 2 {
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not reach the held writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("stale cleanup returned before acquiring the writer: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var topicStatus, runStatus, stage, diagnostic, completed string
	var stored int
	if err := conn.QueryRowContext(ctx, `SELECT t.status,sr.status,sr.stage,sr.error,sr.completed_at,sr.stored_count
		FROM topics t JOIN topic_search_runs sr ON sr.topic_id=t.id WHERE t.id=1`).Scan(&topicStatus, &runStatus, &stage, &diagnostic, &completed, &stored); err != nil {
		t.Fatal(err)
	}
	if topicStatus != "active" || runStatus != "completed" || stage != "" || diagnostic != "" || completed != formatTime(now) || stored != 3 {
		t.Fatalf("waiting cleanup overwrote completion: topic=%s run=%s stage=%s error=%s completed=%s stored=%d", topicStatus, runStatus, stage, diagnostic, completed, stored)
	}
}
