package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
