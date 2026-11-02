package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func readRevisionPost(a app, values url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.generateReadingHandler(w, r)
	return w
}

func readRevisionHomeForm(t *testing.T, a app) emittedTopicForm {
	t.Helper()
	w := httptest.NewRecorder()
	a.homeHandler(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatal("home form unavailable", w.Code, w.Body.String())
	}
	return parseEmittedTopicForm(t, regexp.MustCompile(`(?s)<form\b.*?</form>`).FindString(w.Body.String()))
}

func TestReadRevisionPreventsReplayAfterCompletionAndRestart(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			initialRuns := 0
			observed := "0"
			if existing {
				importWebTopic(t, ctx, conn, "rust", "Rust")
				seedWebDailyReading(t, ctx, conn, "rust", "2026-06-26", "Write-Ahead Logging")
				if _, err := conn.Exec(`UPDATE topics SET status='failed';
					INSERT INTO topic_search_runs(id,topic_id,provider,query,status) SELECT 7,id,'fixture','previous Rust attempt','failed' FROM topics;
					INSERT INTO topics(slug,name,status) VALUES('go','Go','failed');
					INSERT INTO topic_search_runs(id,topic_id,provider,query,status) SELECT 11,id,'fixture','unrelated attempt','failed' FROM topics WHERE slug='go'`); err != nil {
					t.Fatal(err)
				}
				initialRuns, observed = 2, "11"
			}
			provider := &lostResponseProvider{}
			a := revisionTestApp(t, conn, provider)
			form := readRevisionHomeForm(t, a)
			if form.values.Get("observed_run_id") != observed {
				t.Error("home form omitted its current global run watermark", form.values)
			}
			// Keep the baseline executable even before markup and parsing exist.
			form.values.Set("observed_run_id", observed)
			form.values.Set("topic", "Rust")
			for i := range 3 {
				if i == 2 {
					a = revisionTestApp(t, conn, provider)
				}
				w := readRevisionPost(a, form.values)
				waitRevisionJob(t, a)
				if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rust" {
					t.Fatal("native request did not retain its stable URL", w.Code, w.Header())
				}
			}
			var runs int
			if err := conn.QueryRow("SELECT count(*) FROM topic_search_runs").Scan(&runs); err != nil {
				t.Fatal(err)
			}
			if provider.calls.Load() != 1 || runs != initialRuns+1 {
				t.Fatal("replayed native form repeated generation", provider.calls.Load(), runs, initialRuns)
			}
			fresh := readRevisionHomeForm(t, a)
			if fresh.values.Get("observed_run_id") == observed || fresh.values.Get("observed_run_id") == "" {
				t.Fatal("fresh form did not advance its observed watermark", fresh.values)
			}
			fresh.values.Set("topic", "Rust")
			readRevisionPost(a, fresh.values)
			waitRevisionJob(t, a)
			if provider.calls.Load() != 2 {
				t.Fatal("fresh one-submit retry was lost", provider.calls.Load())
			}
			readRevisionPost(a, url.Values{"topic": {"Rust"}})
			waitRevisionJob(t, a)
			if provider.calls.Load() != 3 {
				t.Fatal("omitted watermark changed legacy callers", provider.calls.Load())
			}
			if existing {
				archive, err := reading.GetDailyReading(ctx, conn, "rust", "2026-06-26")
				if err != nil || archive.PageID != 1 || archive.URL != "https://sqlite.org/wal.html" {
					t.Fatal("native replay/retry changed saved history", archive, err)
				}
				var pages, assignments int
				if err := conn.QueryRow("SELECT (SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings)").Scan(&pages, &assignments); err != nil || pages != 1 || assignments != 1 {
					t.Fatal("native retry replaced saved data", pages, assignments, err)
				}
			}
		})
	}
}

func TestReadRevisionAllowsUnrelatedActivityAfterRender(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	form := readRevisionHomeForm(t, a)
	form.values.Set("observed_run_id", "0")
	form.values.Set("topic", "Rust")
	if result, err := topicsearch.SearchTopic(ctx, conn, "Go", topicsearch.Options{Provider: provider, MinInterval: time.Nanosecond}); err == nil || result.Status != "failed" {
		t.Fatal("unrelated fixture attempt did not finish", result, err)
	}
	w := readRevisionPost(a, form.values)
	waitRevisionJob(t, a)
	var rustRuns int
	if err := conn.QueryRow("SELECT count(*) FROM topic_search_runs WHERE topic_id=(SELECT id FROM topics WHERE slug='rust')").Scan(&rustRuns); err != nil || rustRuns != 1 || provider.calls.Load() != 2 || w.Code != http.StatusSeeOther {
		t.Fatal("unrelated activity invalidated the requested topic", rustRuns, provider.calls.Load(), w.Code, err)
	}
}

func TestReadRevisionTravelsWithWaitingWorker(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	a.searchMu.Lock()
	locked := true
	defer func() {
		if locked {
			a.searchMu.Unlock()
		}
	}()
	readRevisionPost(a, url.Values{"topic": {"Rust"}, "observed_run_id": {"0"}})
	if _, pending := a.pendingSearches.Load("rust"); !pending {
		t.Fatal("fixture did not queue the native request")
	}
	if result, err := topicsearch.SearchTopic(ctx, conn, "Rust", topicsearch.Options{Provider: provider, MinInterval: time.Nanosecond}); err == nil || result.Status != "failed" {
		t.Fatal("competing attempt did not finish", result, err)
	}
	a.searchMu.Unlock()
	locked = false
	waitRevisionJob(t, a)
	var runs int
	if err := conn.QueryRow("SELECT count(*) FROM topic_search_runs").Scan(&runs); err != nil || runs != 1 || provider.calls.Load() != 1 {
		t.Fatal("waiting worker lost its submitted watermark", runs, provider.calls.Load(), err)
	}
}

func TestReadRevisionMissingFormsAndCorrection(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	if _, err := topicsearch.SearchTopic(ctx, conn, "Go", topicsearch.Options{Provider: provider, MinInterval: time.Nanosecond}); err == nil {
		t.Fatal("unrelated fixture did not fail")
	}
	for _, path := range []string{"/rust", "/rust?topic=Rust"} {
		w := httptest.NewRecorder()
		a.routeHandler(w, httptest.NewRequest(http.MethodGet, path, nil))
		form := parseEmittedTopicForm(t, w.Body.String())
		if w.Code != http.StatusNotFound || form.values.Get("observed_run_id") != "1" {
			t.Fatal("missing subject did not carry the current global watermark", path, w.Code, form.values)
		}
	}
	readRevisionPost(a, url.Values{"topic": {"Rust"}, "observed_run_id": {"0"}})
	waitRevisionJob(t, a)
	readRevisionPost(a, url.Values{"topic": {"Rust"}, "observed_run_id": {"0"}})
	waitRevisionJob(t, a)
	if provider.calls.Load() != 2 {
		t.Fatal("missing subject replay was not suppressed", provider.calls.Load())
	}
	w := readRevisionPost(a, url.Values{"topic": {"https://invalid.example/"}, "observed_run_id": {"0"}})
	form := parseEmittedTopicForm(t, regexp.MustCompile(`(?s)<form\b.*?</form>`).FindString(w.Body.String()))
	if w.Code != http.StatusBadRequest || form.values.Get("observed_run_id") != "2" || provider.calls.Load() != 2 {
		t.Fatal("invalid-name correction did not refresh watermark without generation", w.Code, form.values, provider.calls.Load())
	}
	form.values.Set("topic", "Rust")
	readRevisionPost(a, form.values)
	waitRevisionJob(t, a)
	if provider.calls.Load() != 3 {
		t.Fatal("corrected form could not retry", provider.calls.Load())
	}
}

func TestReadRevisionParsesNativeAndMultipartBeforeWork(t *testing.T) {
	for _, multipartForm := range []bool{false, true} {
		for _, value := range []string{"", "-1", "+1", "1.0", " 1", "1 ", "no", "9223372036854775808", "duplicate"} {
			t.Run(strconv.FormatBool(multipartForm)+"/"+value, func(t *testing.T) {
				conn := openWebTestDB(t, context.Background())
				defer conn.Close()
				provider := &lostResponseProvider{}
				a := revisionTestApp(t, conn, provider)
				values := url.Values{"topic": {"Rust"}, "observed_run_id": {value}}
				if value == "duplicate" {
					values["observed_run_id"] = []string{"0", "0"}
				}
				w := httptest.NewRecorder()
				if multipartForm {
					var body bytes.Buffer
					writer := multipart.NewWriter(&body)
					for name, entries := range values {
						for _, entry := range entries {
							if err := writer.WriteField(name, entry); err != nil {
								t.Fatal(err)
							}
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					r := httptest.NewRequest(http.MethodPost, "/read", &body)
					r.Header.Set("Content-Type", writer.FormDataContentType())
					a.generateReadingHandler(w, r)
				} else {
					w = readRevisionPost(a, values)
				}
				waitRevisionJob(t, a)
				var topics, runs int
				if err := conn.QueryRow("SELECT (SELECT count(*) FROM topics),(SELECT count(*) FROM topic_search_runs)").Scan(&topics, &runs); err != nil || w.Code != http.StatusBadRequest || topics != 0 || runs != 0 || provider.calls.Load() != 0 {
					t.Fatal("invalid watermark performed work", w.Code, topics, runs, provider.calls.Load(), err)
				}
			})
		}
	}
}

func TestReadRevisionMultipartReplayAndMalformedBodies(t *testing.T) {
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	a.asyncProcessing = false
	for range 2 {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("topic", "Rust"); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("observed_run_id", "0"); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/read", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		a.generateReadingHandler(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatal("unparsed multipart request failed", w.Code, w.Body.String())
		}
	}
	if provider.calls.Load() != 1 {
		t.Fatal("multipart replay repeated generation", provider.calls.Load())
	}
	for _, tc := range []struct{ contentType, body string }{
		{"application/x-www-form-urlencoded", "topic=Rust&observed_run_id=%ZZ"},
		{"multipart/form-data; boundary=bad", "--bad\r\nContent-Disposition: form-data; name=\"topic\"\r\n\r\nRust"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		w := httptest.NewRecorder()
		a.generateReadingHandler(w, r)
		if w.Code != http.StatusBadRequest || provider.calls.Load() != 1 {
			t.Fatal("malformed body performed work", w.Code, provider.calls.Load())
		}
	}
}

func TestReadRevisionMissingURLRetainsLegacyNamedRetry(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	seedQueuedWebTopic(t, ctx, conn, "rust-legacy", "Rust")
	if _, err := conn.Exec(`UPDATE topics SET status='failed'; INSERT INTO topic_search_runs(topic_id,provider,query,status) SELECT id,'fixture','Rust','failed' FROM topics`); err != nil {
		t.Fatal(err)
	}
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	a.asyncProcessing = false
	w := httptest.NewRecorder()
	a.routeHandler(w, httptest.NewRequest(http.MethodGet, "/rust", nil))
	form := parseEmittedTopicForm(t, w.Body.String())
	if w.Code != http.StatusNotFound || form.values.Get("observed_run_id") != "1" {
		t.Fatal("missing URL ignored existing named subject revision", w.Code, form.values)
	}
	w = readRevisionPost(a, form.values)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rust-legacy" || provider.calls.Load() != 1 {
		t.Fatal("fresh missing-URL form lost existing named retry", w.Code, w.Header(), provider.calls.Load())
	}
	var topics int
	if err := conn.QueryRow("SELECT count(*) FROM topics").Scan(&topics); err != nil || topics != 1 {
		t.Fatal("legacy named request duplicated catalog", topics, err)
	}
}

func TestReadRevisionActiveLookupRemainsReadOnlyDuringWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(2)
	importWebTopic(t, ctx, conn, "rust-legacy", "Rust")
	seedWebDailyReading(t, ctx, conn, "rust-legacy", "2026-06-26", "Write-Ahead Logging")
	if _, err := conn.Exec(`UPDATE topics SET updated_at='2020-01-01 00:00:00'; INSERT INTO topic_search_runs(topic_id,provider,query,status) SELECT id,'fixture','Rust','completed' FROM topics; INSERT INTO topics(slug,name,status) VALUES('other','Other','queued')`); err != nil {
		t.Fatal(err)
	}
	writer, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, "UPDATE topics SET description='unrelated writer' WHERE slug='other'"); err != nil {
		t.Fatal(err)
	}
	provider := &lostResponseProvider{}
	a := revisionTestApp(t, conn, provider)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- readRevisionPost(a, url.Values{"topic": {"Rust"}, "observed_run_id": {"0"}}) }()
	select {
	case w := <-done:
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rust-legacy" {
			t.Fatal("observed form lost active lookup", w.Code, w.Header())
		}
	case <-time.After(time.Second):
		_ = writer.Rollback()
		<-done
		t.Fatal("observed active lookup waited for unrelated writer")
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	var status, updated string
	var pages, assignments, runs int
	if err := conn.QueryRow(`SELECT status,updated_at,(SELECT count(*) FROM pages),(SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs) FROM topics WHERE slug='rust-legacy'`).Scan(&status, &updated, &pages, &assignments, &runs); err != nil || status != "active" || updated != "2020-01-01 00:00:00" || pages != 1 || assignments != 1 || runs != 1 || provider.calls.Load() != 0 {
		t.Fatal("observed active lookup changed history/work", status, updated, pages, assignments, runs, provider.calls.Load(), err)
	}
}
