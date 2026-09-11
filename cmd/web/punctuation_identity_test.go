package main

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

func TestLegacyPunctuationCannotCapturePlainSubject(t *testing.T) {
	for _, tc := range []struct{ legacyName, legacySlug, input string }{{"C++", "c", "C"}, {".NET", "net", "NET"}, {"C++", "c", "C++"}, {"Go", "go", "Go"}} {
		t.Run(tc.legacyName+" to "+tc.input, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			if _, err := conn.Exec("INSERT INTO topics(id,slug,name,status)VALUES(1,?,?,'active')", tc.legacySlug, tc.legacyName); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec("INSERT INTO pages(topic_id,title,url,reading_order)VALUES(1,'Legacy reading','https://docs.example.org/legacy',1)"); err != nil {
				t.Fatal(err)
			}
			resolved, _, err := topicsearch.ResolveTopic(ctx, conn, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			p := &quantityProvider{count: 3}
			a := app{db: conn, now: time.Now, searchProvider: p}
			request := httptest.NewRecorder()
			a.generateReadingHandler(request, topicRequest(http.MethodPost, "/read", tc.input))
			var names int
			if err := conn.QueryRow("SELECT count(*) FROM topics WHERE lower(name)=lower(?)", tc.input).Scan(&names); err != nil {
				t.Fatal(err)
			}
			t.Logf("legacy_name=%q input=%q resolver=%q actual_redirect=%q provider_calls=%d exact_name_topics=%d", tc.legacyName, tc.input, resolved, request.Header().Get("Location"), p.calls, names)
			if tc.input == tc.legacyName && p.calls != 0 {
				t.Error("viewing an existing named catalog implicitly retried generation")
			}
			if request.Header().Get("Location") != "/"+resolved {
				t.Errorf("web POST bypassed shared distinct-name resolver")
			}
		})
	}
}

func TestTypedGetLookupPreservesNamedAndHistoricalIdentities(t *testing.T) {
	for _, tc := range []struct{ legacyName, legacySlug, plainName, plainSlug string }{
		{"C++", "c", "C", "c-language"},
		{".NET", "net", "NET", "net-plain"},
	} {
		t.Run(tc.plainName, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			importWebTopic(t, ctx, conn, tc.legacySlug, tc.legacyName)
			p := &quantityProvider{count: 3}
			h := newTestHandlerWithProvider(conn, p)
			missing := httptest.NewRecorder()
			h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/read?topic="+url.QueryEscape(tc.plainName), nil))
			if missing.Code != http.StatusSeeOther || missing.Header().Get("Location") == "/"+tc.legacySlug {
				t.Fatal("missing typed name captured by historical slug", missing.Code, missing.Header())
			}
			var topics int
			if err := conn.QueryRow("SELECT count(*) FROM topics").Scan(&topics); err != nil || topics != 1 {
				t.Fatal("GET created a topic", topics, err)
			}
			importWebTopic(t, ctx, conn, tc.plainSlug, tc.plainName)
			for _, lookup := range []struct{ name, slug string }{{tc.plainName, tc.plainSlug}, {tc.legacyName, tc.legacySlug}} {
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read?topic="+url.QueryEscape(lookup.name), nil))
				if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/"+lookup.slug {
					t.Fatalf("typed name %q resolved to %q", lookup.name, response.Header().Get("Location"))
				}
			}
			for _, table := range []string{"topic_search_runs", "daily_readings"} {
				var count int
				if err := conn.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("typed GET changed saved state", table, count, err)
				}
			}
			direct := httptest.NewRecorder()
			h.ServeHTTP(direct, httptest.NewRequest(http.MethodGet, "/"+tc.legacySlug, nil))
			if direct.Code != http.StatusOK || !strings.Contains(html.UnescapeString(direct.Body.String()), tc.legacyName) || p.calls != 0 {
				t.Fatal("historical URL changed or lookup invoked provider", direct.Code, p.calls, direct.Body.String())
			}
		})
	}
}
