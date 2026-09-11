package main

import (
	"context"
	"database/sql"
	"encoding/xml"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

type recordingTopicPlanner struct{ topics []string }

func (p *recordingTopicPlanner) Plan(_ context.Context, topic string) (topicsearch.PlanOutput, error) {
	p.topics = append(p.topics, topic)
	return topicsearch.PlanOutput{}, nil
}

func topicIdentityHandler(conn *sql.DB, planner topicsearch.Planner, provider topicsearch.Provider) http.Handler {
	a := app{db: conn, now: func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) }, searchPlanner: planner, searchProvider: provider}
	mux := http.NewServeMux()
	mux.HandleFunc("/read", a.generateReadingHandler)
	mux.HandleFunc("/process-topic", a.processTopicHandler)
	mux.HandleFunc("/", a.routeHandler)
	return mux
}

type emittedTopicForm struct {
	method, action string
	values         url.Values
}

func parseEmittedTopicForm(t *testing.T, body string) emittedTopicForm {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(body))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose
	decoder.Entity = xml.HTMLEntity
	var forms []emittedTopicForm
	inside := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("parse emitted HTML form", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			attrs := map[string]string{}
			for _, attr := range element.Attr {
				attrs[attr.Name.Local] = attr.Value
			}
			if element.Name.Local == "form" {
				forms = append(forms, emittedTopicForm{method: strings.ToUpper(attrs["method"]), action: attrs["action"], values: url.Values{}})
				inside = true
			} else if inside && element.Name.Local == "input" && attrs["name"] != "" {
				forms[len(forms)-1].values.Add(attrs["name"], attrs["value"])
			}
		case xml.EndElement:
			if element.Name.Local == "form" {
				inside = false
			}
		}
	}
	if len(forms) != 1 {
		t.Fatalf("expected one emitted topic form, got %d", len(forms))
	}
	return forms[0]
}

func TestMissingTypedTopicSurvivesRedirectAndEmittedForm(t *testing.T) {
	for _, tc := range []struct{ legacyName, legacySlug, input string }{
		{"C++", "c", "C"},
		{".NET", "net", "NET"},
		{"Go", "go", "C++"},
		{"C++", "c", "C#"},
		{"Go", "go", ".NET"},
		{"Go", "go", "@scope/package"},
		{"C++", "c", `C++ "templates" & <T>`},
		{"Go", "go", "日本語"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			importWebTopic(t, ctx, conn, tc.legacySlug, tc.legacyName)
			seedWebDailyReading(t, ctx, conn, tc.legacySlug, "2026-06-26", "Write-Ahead Logging")
			planner, provider := &recordingTopicPlanner{}, &quantityProvider{count: 3}
			handler := topicIdentityHandler(conn, planner, provider)
			lookup := httptest.NewRecorder()
			handler.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/read?"+url.Values{"topic": {tc.input}}.Encode(), nil))
			location, err := url.Parse(lookup.Header().Get("Location"))
			if err != nil || lookup.Code != http.StatusSeeOther {
				t.Fatal("typed lookup failed", lookup.Code, lookup.Header(), err)
			}
			if location.Path == "/"+tc.legacySlug || location.Query().Get("topic") != tc.input {
				t.Fatal("redirect lost the typed subject", location)
			}
			page := httptest.NewRecorder()
			handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, location.String(), nil))
			if page.Code != http.StatusNotFound {
				t.Fatal("missing-topic page failed", page.Code, page.Body.String())
			}
			form := parseEmittedTopicForm(t, page.Body.String())
			if form.method != http.MethodPost || form.action != "/read" || form.values.Get("topic") != tc.input || len(form.values) != 1 {
				t.Fatalf("request form changed the subject: %+v", form)
			}
			for table, want := range map[string]int{"topics": 1, "pages": 1, "daily_readings": 1, "topic_search_runs": 0, "topic_search_results": 0} {
				var count int
				if err := conn.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
					t.Fatal("GET changed saved state", table, count, err)
				}
			}
			if len(planner.topics) != 0 || provider.calls != 0 {
				t.Fatal("GET invoked generation", planner.topics, provider.calls)
			}
			request := httptest.NewRequest(form.method, form.action, strings.NewReader(form.values.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			posted := httptest.NewRecorder()
			handler.ServeHTTP(posted, request)
			postedLocation, err := url.Parse(posted.Header().Get("Location"))
			if err != nil || posted.Code != http.StatusSeeOther || postedLocation.Path != location.Path || postedLocation.RawQuery != "" {
				t.Fatal("explicit request changed routing identity", posted.Code, posted.Header(), err)
			}
			if len(planner.topics) != 1 || planner.topics[0] != tc.input || provider.calls != 1 {
				t.Fatal("planner received a routing slug instead of the typed subject", planner.topics, provider.calls)
			}
			var savedName, status string
			var readings int
			if err := conn.QueryRow(`SELECT name,status,(SELECT count(*) FROM pages WHERE topic_id=topics.id) FROM topics WHERE slug=?`, strings.TrimPrefix(location.Path, "/")).Scan(&savedName, &status, &readings); err != nil || savedName != tc.input || status != "active" || readings != 3 {
				t.Fatal("requested subject not saved with useful readings", savedName, status, readings, err)
			}
			var preserved int
			if err := conn.QueryRow(`SELECT count(*) FROM topics t JOIN pages p ON p.topic_id=t.id JOIN daily_readings d ON d.topic_id=t.id AND d.page_id=p.id WHERE t.id=1 AND t.slug=? AND t.name=? AND t.status='active' AND p.id=1 AND p.url='https://sqlite.org/wal.html' AND p.active=1 AND d.reading_date='2026-06-26'`, tc.legacySlug, tc.legacyName).Scan(&preserved); err != nil || preserved != 1 {
				t.Fatal("historical catalog or assignment changed", preserved, err)
			}
		})
	}
}

func TestMissingTopicRejectsInvalidOrMismatchedNameHints(t *testing.T) {
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	importWebTopic(t, context.Background(), conn, "c", "C++")
	planner, provider := &recordingTopicPlanner{}, &quantityProvider{count: 3}
	handler := topicIdentityHandler(conn, planner, provider)
	lookup := httptest.NewRecorder()
	handler.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/read?topic=C", nil))
	location, err := url.Parse(lookup.Header().Get("Location"))
	if err != nil || lookup.Code != http.StatusSeeOther {
		t.Fatal(lookup.Code, err)
	}
	for _, input := range []string{"", " ", "C\nInjected", "C\r\nInjected", "C\x00", "<script>alert(1)</script>", `C:\Users\alice`, "https://example.org/"} {
		for _, path := range []string{"/read", location.Path} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+"?"+url.Values{"topic": {input}}.Encode(), nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid input %q on %s returned %d", input, path, response.Code)
			}
		}
	}
	for _, input := range []string{"NET", "C++"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, location.Path+"?"+url.Values{"topic": {input}}.Encode(), nil))
		if response.Code != http.StatusBadRequest {
			t.Fatal("unrelated name reinterpreted a missing route", input, response.Code)
		}
	}
	var topics, runs int
	if err := conn.QueryRow(`SELECT (SELECT count(*) FROM topics),(SELECT count(*) FROM topic_search_runs)`).Scan(&topics, &runs); err != nil || topics != 1 || runs != 0 || len(planner.topics) != 0 || provider.calls != 0 {
		t.Fatal("invalid name hints changed state or spent provider work", topics, runs, planner.topics, provider.calls, err)
	}
}

func TestNameHintsDoNotReinterpretExistingCatalogs(t *testing.T) {
	for _, status := range []string{"active", "queued", "failed"} {
		t.Run(status, func(t *testing.T) {
			conn := openWebTestDB(t, context.Background())
			defer conn.Close()
			importWebTopic(t, context.Background(), conn, "c", "C++")
			if _, err := conn.Exec(`UPDATE topics SET status=? WHERE slug='c'`, status); err != nil {
				t.Fatal(err)
			}
			planner, provider := &recordingTopicPlanner{}, &quantityProvider{count: 3}
			handler := topicIdentityHandler(conn, planner, provider)
			lookup := httptest.NewRecorder()
			handler.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/read?"+url.Values{"topic": {"C++"}}.Encode(), nil))
			if lookup.Code != http.StatusSeeOther || lookup.Header().Get("Location") != "/c" {
				t.Fatal("existing name lost its canonical URL", lookup.Code, lookup.Header())
			}
			for _, path := range []string{"/c", "/c?topic=C", "/c?topic=C%0AInjected"} {
				page := httptest.NewRecorder()
				handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, path, nil))
				if page.Code != http.StatusOK || !strings.Contains(html.UnescapeString(page.Body.String()), "C++") {
					t.Fatal("hint changed existing catalog", path, page.Code, page.Body.String())
				}
				form := parseEmittedTopicForm(t, page.Body.String())
				if form.method != http.MethodPost || form.action != "/process-topic" || form.values.Get("topic") != "c" {
					t.Fatalf("hint changed selected-catalog retry: %+v", form)
				}
			}
			var name, savedStatus string
			if err := conn.QueryRow(`SELECT name,status FROM topics WHERE slug='c'`).Scan(&name, &savedStatus); err != nil || name != "C++" || savedStatus != status || len(planner.topics) != 0 || provider.calls != 0 {
				t.Fatal("view changed existing identity or invoked generation", name, savedStatus, planner.topics, provider.calls, err)
			}
		})
	}
}
