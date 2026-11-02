package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMissingPreviewCannotCaptureConcurrentlyClaimedTopic(t *testing.T) {
	for _, status := range []string{"queued", "active"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			lookup := httptest.NewRecorder()
			handler.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/read?topic=Health", nil))
			location := lookup.Header().Get("Location")
			if lookup.Code != http.StatusSeeOther || location != "/health-topic?topic=Health" {
				t.Fatal("unexpected missing-topic preview", lookup.Code, location)
			}
			// Another explicit request/import claims the candidate before this
			// reader follows the redirect. Its history must not become Health's.
			importWebTopic(t, ctx, conn, "health-topic", "Health Topic")
			seedWebDailyReading(t, ctx, conn, "health-topic", "2026-06-26", "Write-Ahead Logging")
			if _, err := conn.ExecContext(ctx, `UPDATE topics SET status=?`, status); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, location, nil))
			body := response.Body.String()
			if response.Code != http.StatusBadRequest || !strings.Contains(body, "Topic preview unavailable") {
				t.Fatalf("claimed preview did not offer recovery: %d %s", response.Code, body)
			}
			for _, wrong := range []string{"<h1>Health Topic</h1>", "Write-Ahead Logging", "Process topic", `action="/process-topic"`} {
				if strings.Contains(body, wrong) {
					t.Fatal("preview exposed the competing subject", wrong)
				}
			}
			recoveryURL := datedRecoveryLink(t, body, "Review requested topic")
			if recoveryURL != "/read?topic=Health" {
				t.Fatal("recovery changed the requested name", recoveryURL)
			}
			recovered := httptest.NewRecorder()
			handler.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, recoveryURL, nil))
			next, err := url.Parse(recovered.Header().Get("Location"))
			if err != nil || recovered.Code != http.StatusSeeOther || next.Path == "/health-topic" || next.Query().Get("topic") != "Health" {
				t.Fatal("recovery did not resolve the original subject", recovered.Code, next, err)
			}
			preview := httptest.NewRecorder()
			handler.ServeHTTP(preview, httptest.NewRequest(http.MethodGet, next.String(), nil))
			if preview.Code != http.StatusNotFound {
				t.Fatal("recovered missing preview lost its 404", preview.Code)
			}
			form := parseEmittedTopicForm(t, preview.Body.String())
			if form.method != http.MethodPost || form.action != "/read" || form.values.Get("topic") != "Health" {
				t.Fatalf("recovery offered a different topic: %+v", form)
			}
			var topics, pages, assignments, runs, historicalPage int
			if err := conn.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM topics), (SELECT count(*) FROM pages),
				(SELECT count(*) FROM daily_readings), (SELECT count(*) FROM topic_search_runs),
				(SELECT page_id FROM daily_readings WHERE reading_date='2026-06-26')
			`).Scan(&topics, &pages, &assignments, &runs, &historicalPage); err != nil {
				t.Fatal(err)
			}
			if topics != 1 || pages != 1 || assignments != 1 || runs != 0 || historicalPage != 1 || provider.calls != 0 {
				t.Fatal("preview changed data or generated", topics, pages, assignments, runs, historicalPage, provider.calls)
			}
		})
	}
}

func TestTopicPreviewReadsNamesWithoutCreatingAssignments(t *testing.T) {
	for _, status := range []string{"queued", "failed", "active"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			importWebTopic(t, ctx, conn, "c", "C++")
			if _, err := conn.ExecContext(ctx, `UPDATE topics SET status=?`, status); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
				t.Fatal(err)
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			for _, path := range []string{"/read?topic=C%2B%2B", "/c?topic=C%2B%2B"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/c" {
					t.Fatal("matching preview did not use the clean historical URL", path, response.Code, response.Header())
				}
			}
			var assignments, runs int
			if err := conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs)`).Scan(&assignments, &runs); err != nil || assignments != 0 || runs != 0 || provider.calls != 0 {
				t.Fatal("name lookup wrote or generated", assignments, runs, provider.calls, err)
			}
		})
	}
}

func TestTopicPreviewRecoveryEscapesAndPreservesRequestedName(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	provider := &countingWebProvider{}
	handler := newTestHandlerWithProvider(conn, provider)
	for _, input := range []string{"", "  ", "C\nInjected", "https://example.org/path", "javascript:alert(1)", `<img src=x onerror="alert(1)">`, `C++ "templates" & <T>`} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rust?"+url.Values{"topic": {input}}.Encode(), nil))
		body := response.Body.String()
		if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(body, "Topic preview unavailable") {
			t.Fatal("invalid/mismatched preview lacks HTML recovery", response.Code, body)
		}
		if strings.Contains(body, "<img") || strings.Contains(body, `onerror="`) || strings.Contains(body, "<T>") {
			t.Fatal("requested topic escaped the recovery link")
		}
		recovery, err := url.Parse(datedRecoveryLink(t, body, "Review requested topic"))
		if err != nil || recovery.Scheme != "" || recovery.Host != "" || recovery.Path != "/read" || recovery.Query().Get("topic") != input {
			t.Fatal("recovery link changed the requested name", recovery, err)
		}
		if datedRecoveryLink(t, body, "Search another topic") != "/" {
			t.Fatal("preview recovery lost home navigation")
		}
		if input == `C++ "templates" & <T>` && strings.Contains(body, "valid topic name") {
			t.Fatal("a stale URL mislabeled valid input as invalid")
		}
	}
	var topics, assignments, runs int
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM topics),(SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs)`).Scan(&topics, &assignments, &runs); err != nil || topics != 0 || assignments != 0 || runs != 0 || provider.calls != 0 {
		t.Fatal("rejected preview wrote or generated", topics, assignments, runs, provider.calls, err)
	}
}

func TestDatedReadingIgnoresNamePreviewHints(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	importWebTopic(t, ctx, conn, "c", "C++")
	seedWebDailyReading(t, ctx, conn, "c", "2026-06-26", "Write-Ahead Logging")
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	provider := &countingWebProvider{}
	handler := newTestHandlerWithProvider(conn, provider)
	for _, tc := range []struct {
		path, content string
		status        int
	}{
		{"/c/2026-06-26?topic=C", "Write-Ahead Logging", http.StatusOK},
		{"/c/2026-06-26?topic=C%0AInjected", "Write-Ahead Logging", http.StatusOK},
		{"/c/2026-06-25?topic=C", "No saved reading for this date", http.StatusNotFound},
		{"/c/not-a-date?topic=C", "Use a valid date", http.StatusBadRequest},
		{"/unknown/2026-06-26?topic=C", "No saved reading for this date", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.content) || strings.Contains(response.Body.String(), "Topic preview unavailable") {
			t.Fatal("name hint reinterpreted dated URL", tc.path, response.Code, response.Body.String())
		}
	}
	var assignments, runs int
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs)`).Scan(&assignments, &runs); err != nil || assignments != 1 || runs != 0 || provider.calls != 0 {
		t.Fatal("dated hint changed history or generated", assignments, runs, provider.calls, err)
	}
}
