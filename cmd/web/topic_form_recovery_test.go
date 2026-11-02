package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestInvalidTopicFormPreservesInputAndCanBeCorrected(t *testing.T) {
	for _, tc := range []struct {
		name, input, guidance string
	}{
		{"url", "https://sqlite.org/wal.html", "URLs, file paths, and instructions"},
		{"path", `C:\Users\reader\notes`, "URLs, file paths, and instructions"},
		{"markup", `<img src=x onerror="alert(1)">`, "URLs, file paths, and instructions"},
		{"blank", "   ", "Enter a topic name"},
		{"long", strings.Repeat("a", 201), "200 characters or fewer"},
		{"long_unicode", strings.Repeat("界", 201), "200 characters or fewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			importWebTopic(t, ctx, conn, "sqlite", "SQLite")
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, topicRequest(http.MethodPost, "/read", tc.input))
			body := response.Body.String()
			if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("invalid request needs an HTML correction form with 400: %d %s", response.Code, body)
			}
			for _, expected := range []string{tc.guidance, `aria-invalid="true"`, `aria-describedby="topic-error"`, `id="topic-error" role="alert"`} {
				if !strings.Contains(body, expected) {
					t.Fatalf("missing correction guidance %q: %s", expected, body)
				}
			}
			if strings.Contains(body, "<img") || strings.Contains(body, ` onerror="alert(1)"`) {
				t.Fatal("submitted markup escaped its input value")
			}
			// Parse the emitted form independently of the page's JavaScript text.
			formHTML := regexp.MustCompile(`(?s)<form\b.*?</form>`).FindString(body)
			form := parseEmittedTopicForm(t, formHTML)
			if form.method != http.MethodPost || form.action != "/read" || form.values.Get("topic") != tc.input {
				t.Fatalf("correction form lost input or action: %+v", form)
			}
			var topics, runs, readings int
			if err := conn.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM topics), (SELECT COUNT(*) FROM topic_search_runs), (SELECT COUNT(*) FROM daily_readings)`).Scan(&topics, &runs, &readings); err != nil {
				t.Fatal(err)
			}
			if topics != 1 || runs != 0 || readings != 0 || provider.calls != 0 {
				t.Fatalf("invalid request changed state: topics=%d runs=%d readings=%d calls=%d", topics, runs, readings, provider.calls)
			}
			form.values.Set("topic", "SQLite")
			corrected := httptest.NewRecorder()
			request := httptest.NewRequest(form.method, form.action, strings.NewReader(form.values.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			handler.ServeHTTP(corrected, request)
			if corrected.Code != http.StatusSeeOther || corrected.Header().Get("Location") != "/sqlite" || provider.calls != 0 {
				t.Fatalf("corrected form failed: %d %q calls=%d", corrected.Code, corrected.Header().Get("Location"), provider.calls)
			}
		})
	}
}

func TestTopicFormDoesNotRestrictUnicodeByUTF16Length(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	// Supplementary-plane letters occupy two UTF-16 units but one Go rune.
	name := strings.Repeat("𐐀", 200)
	handler := newTestHandler(conn)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, topicRequest(http.MethodPost, "/read", name))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("200 Unicode characters must remain valid: %d %s", response.Code, response.Body.String())
	}
	home := httptest.NewRecorder()
	handler.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(home.Body.String(), "maxlength=") || strings.Contains(home.Body.String(), `aria-invalid="true"`) {
		t.Fatal("fresh form must not impose a UTF-16 length limit or mark valid input invalid")
	}
}
