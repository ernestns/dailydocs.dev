package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
)

// This helper runs the real server entrypoint in an isolated test process so
// SIGTERM exercises the production signal, worker, traffic, and SQLite defers.
func TestDailyDocsShutdownProcessHelper(t *testing.T) {
	if os.Getenv("DAILYDOCS_SHUTDOWN_TEST_PROCESS") != "1" {
		return
	}
	os.Args = []string{"dailydocs"}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestServerSIGTERMDrainsGenerationBeforeClosingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shutdown.sqlite")
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(`
		INSERT INTO topics(id,slug,name,status) VALUES(1,'rust','Rust','active');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(1,1,'Saved Rust reading','https://doc.rust-lang.org/saved-reading',1);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-09-10',1);
	`)
	if closeErr := conn.Close(); err != nil || closeErr != nil {
		t.Fatal("seed database", err, closeErr)
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

	// Reserve an available loopback port, releasing it just before the child
	// binds. The child reports any bind failure rather than contacting a host.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDailyDocsShutdownProcessHelper$")
	command.Env = append(os.Environ(),
		"DAILYDOCS_SHUTDOWN_TEST_PROCESS=1", "ADDR="+addr, "DB_PATH="+path,
		"TAVILY_API_KEY=local-test-only", "TAVILY_ENDPOINT="+provider.URL,
		"OPENAI_API_KEY=", "OPENAI_ENDPOINT=", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "NO_PROXY=*",
	)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = command.Wait()
		close(done)
	}()
	defer func() {
		_ = command.Process.Kill()
		<-done
	}()
	client := &http.Client{
		Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	origin := "http://" + addr
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		select {
		case <-done:
			t.Fatalf("server exited before ready: %v\n%s", exitErr, output.String())
		default:
		}
		response, err := client.Get(origin + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("child server did not become ready")
	}
	response, err := client.PostForm(origin+"/process-topic", url.Values{"topic": {"rust"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("generation POST returned %d", response.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("local provider did not receive generation request")
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("SIGTERM did not cancel provider request")
	}
	select {
	case <-done:
		if exitErr != nil {
			t.Fatalf("server did not exit cleanly: %v\n%s", exitErr, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not drain and exit after SIGTERM")
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("unexpected provider work: %d calls", providerCalls.Load())
	}

	conn, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var status, diagnostic string
	var completed int
	if err := conn.QueryRow("SELECT status,error,completed_at IS NOT NULL FROM topic_search_runs").Scan(&status, &diagnostic, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || completed != 1 || !strings.Contains(diagnostic, context.Canceled.Error()) {
		t.Fatalf("process exited before persisting canceled generation: status=%s completed=%d error=%s\n%s", status, completed, diagnostic, output.String())
	}
	got, err := reading.GetDailyReading(ctx, conn, "rust", "2026-09-10")
	if err != nil || got.PageID != 1 || got.URL != "https://doc.rust-lang.org/saved-reading" {
		t.Fatal("shutdown changed historical reading", got, err)
	}
}
