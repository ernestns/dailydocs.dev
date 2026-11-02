package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ernestns/daily-docs/internal/topicname"
)

func TestReservedTopicRequestsReachTheirDailyReadings(t *testing.T) {
	for _, name := range []string{"Health", "Read", "Topics", "Process Topic"} {
		t.Run(name, func(t *testing.T) {
			conn := openWebTestDB(t, context.Background())
			defer conn.Close()
			provider := &quantityProvider{count: 3}
			handler := newTestHandlerWithProvider(conn, provider)
			requested := httptest.NewRecorder()
			handler.ServeHTTP(requested, topicRequest(http.MethodPost, "/read", name))
			wantPath := "/" + topicname.Slug(name) + "-topic"
			if requested.Code != http.StatusSeeOther || requested.Header().Get("Location") != wantPath {
				t.Fatal("topic request did not use a reachable path", requested.Code, requested.Header(), requested.Body.String())
			}
			page := httptest.NewRecorder()
			handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, wantPath, nil))
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Useful guide") || !strings.Contains(page.Body.String(), "<h1>"+name+"</h1>") {
				t.Fatal("redirect did not show the requested reading", page.Code, page.Body.String())
			}
			again := httptest.NewRecorder()
			handler.ServeHTTP(again, topicRequest(http.MethodPost, "/read", name))
			if again.Header().Get("Location") != wantPath || provider.calls != 1 {
				t.Fatal("repeat request changed identity or regenerated catalog", again.Header(), provider.calls)
			}
			health := httptest.NewRecorder()
			handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
			if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
				t.Fatal("application health route changed")
			}
		})
	}
}
