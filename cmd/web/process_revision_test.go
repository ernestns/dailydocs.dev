package main

import (
	"bytes"
	"context"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func revisionTestApp(t *testing.T, conn *sql.DB, provider *lostResponseProvider) app {
	t.Helper()
	lifecycle := newTopicSearchLifecycle()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lifecycle.wait(ctx); err != nil {
			t.Error(err)
		}
	})
	return app{db: conn, now: time.Now, searchMu: &sync.Mutex{}, pendingSearches: &sync.Map{}, searchProvider: provider, searchLifecycle: lifecycle, asyncProcessing: true}
}

func waitRevisionJob(t *testing.T, a app) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, pending := a.pendingSearches.Load("rust"); !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("revision test worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func revisionPost(t *testing.T, a app, body string, enhanced bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/process-topic", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if enhanced {
		r.Header.Set("Datastar-Request", "true")
	}
	w := httptest.NewRecorder()
	a.processTopicHandler(w, r)
	return w
}

func TestProcessRevisionPreventsReplayAfterCompletionAndRestart(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		t.Run(strconv.FormatBool(enhanced), func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			importWebTopic(t, ctx, conn, "rust", "Rust")
			seedWebDailyReading(t, ctx, conn, "rust", "2026-06-26", "Write-Ahead Logging")
			provider := &lostResponseProvider{}
			a := revisionTestApp(t, conn, provider)
			status := httptest.NewRecorder()
			a.topicStatusHandler(status, httptest.NewRequest(http.MethodGet, "/topics/rust/status", nil), "rust")
			form := parseEmittedTopicForm(t, status.Body.String())
			if form.values.Get("expected_run_id") != "0" {
				t.Error("new process form did not carry its observed initial revision")
			}
			// Submit the same body on old and fixed code so baseline evidence
			// includes actual duplicated work, not just missing markup.
			body := "topic=rust&expected_run_id=0"
			wantStatus := http.StatusSeeOther
			if enhanced {
				wantStatus = http.StatusOK
			}
			for i := range 3 {
				if i == 2 {
					// A replacement app has no pending-map memory of the first job.
					a = revisionTestApp(t, conn, provider)
				}
				response := revisionPost(t, a, body, enhanced)
				waitRevisionJob(t, a)
				if response.Code != wantStatus {
					t.Fatal("replay did not return current status", response.Code)
				}
			}
			var runs, pages, assignments int
			if err := conn.QueryRow("SELECT (SELECT count(*) FROM topic_search_runs),(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings)").Scan(&runs, &pages, &assignments); err != nil {
				t.Fatal(err)
			}
			if provider.calls.Load() != 1 || runs != 1 || pages != 1 || assignments != 1 {
				t.Fatal("same submitted revision repeated work or changed saved data", provider.calls.Load(), runs, pages, assignments)
			}
			fresh := httptest.NewRecorder()
			a.topicStatusHandler(fresh, httptest.NewRequest(http.MethodGet, "/topics/rust/status", nil), "rust")
			form = parseEmittedTopicForm(t, fresh.Body.String())
			if form.values.Get("expected_run_id") != "1" {
				t.Fatal("fresh process form did not advance its revision", form.values)
			}
			revisionPost(t, a, form.values.Encode(), enhanced)
			waitRevisionJob(t, a)
			if provider.calls.Load() != 2 {
				t.Fatal("fresh explicit retry was blocked", provider.calls.Load())
			}
			// Legacy/manual requests without the optional field retain behavior.
			revisionPost(t, a, "topic=rust", false)
			waitRevisionJob(t, a)
			if provider.calls.Load() != 3 {
				t.Fatal("missing revision changed legacy API behavior", provider.calls.Load())
			}
			archive, err := reading.GetDailyReading(ctx, conn, "rust", "2026-06-26")
			if err != nil || archive.PageID != 1 || archive.URL != "https://sqlite.org/wal.html" {
				t.Fatal("retry changed the archived reading", archive, err)
			}
		})
	}
}

func TestProcessRevisionTravelsWithWaitingWorker(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	a.searchMu.Lock()
	locked := true
	defer func() {
		if locked {
			a.searchMu.Unlock()
		}
	}()
	revisionPost(t, a, "topic=rust&expected_run_id=0", true)
	if _, pending := a.pendingSearches.Load("rust"); !pending {
		t.Fatal("fixture did not queue the observed request")
	}
	// Another worker finishes after HTTP admission but before this job acquires
	// its local worker. The observed revision must reach SQL admission.
	if result, err := topicsearch.SearchTopic(ctx, conn, "Rust", topicsearch.Options{Provider: provider, MinInterval: time.Nanosecond}); err == nil || result.Status != "failed" {
		t.Fatal("competing fake attempt did not fail", result, err)
	}
	a.searchMu.Unlock()
	locked = false
	waitRevisionJob(t, a)
	var runs int
	if err := conn.QueryRow("SELECT count(*) FROM topic_search_runs").Scan(&runs); err != nil || runs != 1 || provider.calls.Load() != 1 {
		t.Fatal("queued request lost its observed revision", runs, provider.calls.Load(), err)
	}
}

func TestProcessRevisionRejectsMalformedValuesBeforeWork(t *testing.T) {
	for _, value := range []string{"", "-1", "+1", "1.0", " 1", "1 ", "no", "9223372036854775808", "1&expected_run_id=1"} {
		t.Run(value, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
			provider := &lostResponseProvider{}
			a := revisionTestApp(t, conn, provider)
			encoded := url.QueryEscape(value)
			if strings.Contains(value, "&") {
				encoded = value
			}
			response := revisionPost(t, a, "topic=rust&expected_run_id="+encoded, true)
			waitRevisionJob(t, a)
			var runs int
			if err := conn.QueryRow("SELECT count(*) FROM topic_search_runs").Scan(&runs); err != nil || response.Code != http.StatusBadRequest || runs != 0 || provider.calls.Load() != 0 {
				t.Fatal("bad revision caused work or was accepted", response.Code, runs, provider.calls.Load(), err)
			}
		})
	}
}

func TestProcessRevisionProtectsUnparsedMultipartForms(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust", "Rust")
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	post := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, entries := range values {
			for _, value := range entries {
				if err := writer.WriteField(name, value); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/process-topic", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		r.Header.Set("Datastar-Request", "true")
		w := httptest.NewRecorder()
		// No FormValue/ParseForm call before the production handler.
		a.processTopicHandler(w, r)
		waitRevisionJob(t, a)
		return w
	}
	for range 2 {
		if w := post(url.Values{"topic": {"rust"}, "expected_run_id": {"0"}}); w.Code != http.StatusOK {
			t.Fatal("multipart process form failed", w.Code, w.Body.String())
		}
	}
	if provider.calls.Load() != 1 {
		t.Fatal("multipart replay repeated provider work", provider.calls.Load())
	}
	for _, revision := range [][]string{{""}, {"-1"}, {"1", "1"}, {"9223372036854775808"}} {
		if w := post(url.Values{"topic": {"rust"}, "expected_run_id": revision}); w.Code != http.StatusBadRequest {
			t.Fatal("multipart malformed revision was accepted", revision, w.Code)
		}
	}
	if provider.calls.Load() != 1 {
		t.Fatal("invalid multipart revision spent work", provider.calls.Load())
	}
	if w := post(url.Values{"topic": {"rust"}, "expected_run_id": {"1"}}); w.Code != http.StatusOK || provider.calls.Load() != 2 {
		t.Fatal("fresh multipart revision could not retry", w.Code, provider.calls.Load())
	}
	for _, contentType := range []string{"multipart/form-data; boundary=missing", "application/x-www-form-urlencoded"} {
		r := httptest.NewRequest(http.MethodPost, "/process-topic", strings.NewReader("topic=rust&expected_run_id=%ZZ"))
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		a.processTopicHandler(w, r)
		if w.Code != http.StatusBadRequest || provider.calls.Load() != 2 {
			t.Fatal("malformed form body was accepted", contentType, w.Code, provider.calls.Load())
		}
	}
}
