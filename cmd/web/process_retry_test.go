package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

type lostResponseProvider struct{ calls atomic.Int32 }

func (p *lostResponseProvider) Search(context.Context, string, int) ([]topicsearch.SearchResult, error) {
	p.calls.Add(1)
	return nil, errors.New("fixture source unavailable")
}

func TestProcessTopicDoesNotReplayAfterLostResponse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to execute the pinned browser transport behavior")
	}
	for _, tc := range []struct {
		mode string
		want int
	}{{"form", 1}, {"defaults", 2}, {"never-only", 2}} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
			provider := &lostResponseProvider{}
			lifecycle := newTopicSearchLifecycle()
			defer func() {
				lifecycle.stop()
				if err := lifecycle.wait(ctx); err != nil {
					t.Error(err)
				}
			}()
			a := app{db: conn, now: time.Now, searchMu: &sync.Mutex{}, pendingSearches: &sync.Map{}, searchProvider: provider, searchLifecycle: lifecycle, asyncProcessing: true}
			page := httptest.NewRecorder()
			a.routeHandler(page, httptest.NewRequest(http.MethodGet, "/rust", nil))
			body := page.Body.String()
			if page.Code != http.StatusOK || !strings.Contains(body, "datastar@v1.0.2/bundles/datastar.js") || !strings.Contains(body, `method="post"`) || !strings.Contains(body, `action="/process-topic"`) || !strings.Contains(body, `name="topic" value="rust"`) {
				t.Fatal("process form or pinned runtime changed; update the transport fixture deliberately", page.Code)
			}
			match := regexp.MustCompile(`data-on:submit__prevent="@post\('/process-topic', ([^"]+)\)"`).FindStringSubmatch(body)
			if len(match) != 2 {
				t.Fatal("rendered process form options not found")
			}
			command := exec.CommandContext(ctx, node, "testdata/process-topic-retry.cjs", html.UnescapeString(match[1]), tc.mode)
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				stdin.Close()
				if !waited {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			}()
			waitForAttempt := func() {
				t.Helper()
				for {
					if _, pending := a.pendingSearches.Load("rust"); !pending {
						return
					}
					select {
					case <-ctx.Done():
						t.Fatal("fake attempt did not finish", ctx.Err())
					case <-time.After(time.Millisecond):
					}
				}
			}
			requests, reportedAttempts := 0, 0
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				var message struct {
					Type, URL, Method, Body string
					Attempt, Attempts       int
				}
				if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
					t.Fatal(err)
				}
				if message.Type == "done" {
					reportedAttempts = message.Attempts
					continue
				}
				requests++
				if message.Type != "request" || requests > 2 || message.Attempt != requests || message.URL != "/process-topic" || message.Method != http.MethodPost || message.Body != "topic=rust" {
					t.Fatal("unexpected transport request", message)
				}
				request := httptest.NewRequest(message.Method, message.URL, strings.NewReader(message.Body))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				request.Header.Set("Datastar-Request", "true")
				response := httptest.NewRecorder()
				a.processTopicHandler(response, request)
				if response.Code != http.StatusOK {
					t.Fatal("process request was not accepted", response.Code, response.Body.String())
				}
				// The source fails before the network reports the lost response.
				// A transport replay can therefore pass pending-job protection.
				waitForAttempt()
				if err := json.NewEncoder(stdin).Encode(map[string]any{"drop": requests == 1, "status": response.Code, "body": response.Body.String()}); err != nil {
					t.Fatal(err)
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			waited = true
			if err != nil {
				t.Fatalf("browser transport: %v\n%s", err, stderr.String())
			}
			var runs, pages, assignments int
			if err := conn.QueryRow("SELECT (SELECT count(*) FROM topic_search_runs),(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings)").Scan(&runs, &pages, &assignments); err != nil {
				t.Fatal(err)
			}
			t.Logf("single click: mode=%s requests=%d provider calls=%d runs=%d", tc.mode, requests, provider.calls.Load(), runs)
			if requests != tc.want || reportedAttempts != tc.want || int(provider.calls.Load()) != tc.want || runs != tc.want || pages != 0 || assignments != 0 {
				t.Fatal("one click repeated provider work or changed saved state", requests, reportedAttempts, provider.calls.Load(), runs, pages, assignments)
			}
			// A later explicit submission still works without JavaScript and keeps
			// the normal POST/redirect fallback, including after an uncertain result.
			explicit := httptest.NewRecorder()
			a.processTopicHandler(explicit, topicRequest(http.MethodPost, "/process-topic", "rust"))
			waitForAttempt()
			if explicit.Code != http.StatusSeeOther || explicit.Header().Get("Location") != "/rust" || int(provider.calls.Load()) != tc.want+1 {
				t.Fatal("transport retry policy prevented an explicit later retry", explicit.Code, explicit.Header(), provider.calls.Load())
			}
		})
	}
}
