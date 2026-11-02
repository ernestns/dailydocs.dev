package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

// Run the production command entrypoint in its own process so real operating
// system signals cannot affect the test runner or unrelated services.
func TestDailyDocsCLISignalProcessHelper(t *testing.T) {
	if os.Getenv("DAILYDOCS_CLI_SIGNAL_TEST_PROCESS") != "1" {
		return
	}
	os.Args = []string{"dailydocs", "search-topic", "Rust"}
	main()
	os.Exit(0)
}

func TestCLISignalsFinalizeSearchAndReleaseAdmission(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "cli.sqlite")
			conn, err := db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = conn.Exec(`
				INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','active');
				INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(1,1,'Saved Rust','https://docs.example.org/saved-rust',1);
				INSERT INTO daily_readings(id,topic_id,reading_date,page_id) VALUES(21,1,'2026-01-01',1);
			`)
			if closeErr := conn.Close(); err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var startedOnce, canceledOnce sync.Once
			var providerCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				startedOnce.Do(func() { close(started) })
				select {
				case <-r.Context().Done():
					canceledOnce.Do(func() { close(canceled) })
				case <-release:
				}
			}))
			defer provider.Close()
			defer close(release)
			command := exec.Command(os.Args[0], "-test.run=^TestDailyDocsCLISignalProcessHelper$")
			// Inherit no environment or credentials. The child can only contact
			// this loopback fixture with a synthetic token; OpenAI is disabled.
			command.Env = []string{
				"DAILYDOCS_CLI_SIGNAL_TEST_PROCESS=1", "DAILYDOCS_LIVE_QUALITY=0", "DB_PATH=" + path,
				"TAVILY_API_KEY=local-test-only", "TAVILY_ENDPOINT=" + provider.URL, "OPENAI_API_KEY=",
				"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "NO_PROXY=*",
			}
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var exitErr error
			go func() { exitErr = command.Wait(); close(done) }()
			defer func() { _ = command.Process.Kill(); <-done }()
			select {
			case <-started:
			case <-done:
				t.Fatalf("CLI exited before the provider request: %v %s", exitErr, output.String())
			case <-time.After(5 * time.Second):
				t.Fatal("CLI did not reach the local provider")
			}
			if err := command.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("signal did not cancel the provider request")
			}
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Fatal("CLI did not complete bounded cleanup after signal")
			}
			var exit *exec.ExitError
			if !errors.As(exitErr, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("canceled command must report a nonzero normal exit: %v %s", exitErr, output.String())
			}
			if providerCalls.Load() != 1 {
				t.Fatal("interruption retried provider work", providerCalls.Load())
			}
			conn, err = db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var status, stage, diagnostic string
			var completed bool
			if err := conn.QueryRow(`SELECT status,stage,error,completed_at IS NOT NULL FROM topic_search_runs WHERE id=1`).Scan(&status, &stage, &diagnostic, &completed); err != nil {
				t.Fatal(err)
			}
			canceledDiagnostic := strings.Contains(diagnostic, context.Canceled.Error()) || strings.Contains(diagnostic, sig.String()+" signal received")
			if status != "failed" || stage != "" || !completed || !canceledDiagnostic {
				t.Fatalf("signal left generation unfinished: status=%q stage=%q completed=%v error=%q", status, stage, completed, diagnostic)
			}
			// Releasing the running slot must not bypass the existing default
			// global minimum interval between generation attempts.
			retryProvider := &countingWebProvider{}
			retry, err := topicsearch.SearchTopic(ctx, conn, "Rust", topicsearch.Options{Provider: retryProvider})
			if !errors.Is(err, topicsearch.ErrRateLimited) || !retry.RateLimited || retryProvider.calls != 0 {
				t.Fatal("interruption changed the default global interval", retry, retryProvider.calls, err)
			}
			// Isolate the running guard from that separate minimum interval.
			nextProvider := &countingWebProvider{}
			next, err := topicsearch.SearchTopic(ctx, conn, "Go", topicsearch.Options{Provider: nextProvider, MinInterval: time.Nanosecond})
			if err != nil || next.Status != "completed" || nextProvider.calls != 1 {
				t.Fatal("interrupted CLI still blocks unrelated generation", next, nextProvider.calls, err)
			}
			archive, err := reading.GetDailyReading(ctx, conn, "rust", "2026-01-01")
			if err != nil || archive.PageID != 1 || archive.URL != "https://docs.example.org/saved-rust" || archive.Title != "Saved Rust" {
				t.Fatal("interruption changed the historical reading", archive, err)
			}
			var assignmentID int
			if err := conn.QueryRow(`SELECT id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-01-01'`).Scan(&assignmentID); err != nil || assignmentID != 21 {
				t.Fatal("interruption changed the saved assignment", assignmentID, err)
			}
		})
	}
}
