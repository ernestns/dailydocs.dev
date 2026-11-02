package main

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestMissingDatedReadingOffersTodayWithoutCreatingHistory(t *testing.T) {
	for _, date := range []string{"1970-01-01", "2099-01-01"} {
		t.Run(date, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			importWebTopic(t, ctx, conn, "sqlite", "SQLite")
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sqlite/"+date, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("expected missing reading to remain 404: %d %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("expected HTML recovery page, got %q", got)
			}
			body := response.Body.String()
			if !strings.Contains(body, "No saved reading for this date") || !strings.Contains(body, `<time datetime="`+date+`">`+date+`</time>`) {
				t.Fatalf("expected explanation of missing saved date: %s", body)
			}
			var count int
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_readings").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("recovery page created %d reading assignments", count)
			}
			todayPath := datedRecoveryLink(t, body, "Today's reading")
			if todayPath != "/sqlite" || datedRecoveryLink(t, body, "Search another topic") != "/" {
				t.Fatal("recovery page did not offer the current topic and home")
			}
			today := httptest.NewRecorder()
			handler.ServeHTTP(today, httptest.NewRequest(http.MethodGet, todayPath, nil))
			if today.Code != http.StatusOK || !strings.Contains(today.Body.String(), "Write-Ahead Logging") {
				t.Fatalf("recovery link did not reach today's reading: %d %s", today.Code, today.Body.String())
			}
			var storedDate string
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*), MIN(reading_date) FROM daily_readings").Scan(&count, &storedDate); err != nil {
				t.Fatal(err)
			}
			if count != 1 || storedDate != "2026-06-27" {
				t.Fatalf("navigation must only assign today: count=%d date=%q", count, storedDate)
			}
			if provider.calls != 0 {
				t.Fatalf("reading recovery called provider %d times", provider.calls)
			}
		})
	}
}

func TestInvalidDatedReadingExplainsFormatWithoutReflectingInput(t *testing.T) {
	for _, topic := range []string{"sqlite", "unknown"} {
		for _, date := range []string{"yesterday", "2026-02-30", `untrusted"><img src=x onerror=alert(1)>`} {
			t.Run(topic+"/"+date, func(t *testing.T) {
				ctx := context.Background()
				conn := openWebTestDB(t, ctx)
				defer conn.Close()
				importWebTopic(t, ctx, conn, "sqlite", "SQLite")
				provider := &countingWebProvider{}
				handler := newTestHandlerWithProvider(conn, provider)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+topic+"/"+url.PathEscape(date), nil))
				body := response.Body.String()
				if response.Code != http.StatusBadRequest || !strings.Contains(body, "Use a valid date in YYYY-MM-DD format") {
					t.Fatalf("expected explanatory 400: %d %s", response.Code, body)
				}
				if strings.Contains(body, date) || strings.Contains(body, "untrusted") {
					t.Fatal("invalid date input must not be reflected in the page")
				}
				if datedRecoveryLink(t, body, "Today's reading") != "/"+topic || datedRecoveryLink(t, body, "Search another topic") != "/" {
					t.Fatal("invalid date recovery links are incorrect")
				}
				for _, table := range []string{"daily_readings", "topic_search_runs", "topic_search_results"} {
					var count int
					if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("invalid dated visit created %d rows in %s", count, table)
					}
				}
				if provider.calls != 0 {
					t.Fatalf("invalid date called provider %d times", provider.calls)
				}
			})
		}
	}
}

func TestUnknownDatedReadingRecoveryRequiresExplicitTopicRequest(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	provider := &countingWebProvider{}
	handler := newTestHandlerWithProvider(conn, provider)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unknown/2026-06-26", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected unknown dated topic to remain 404: %d", response.Code)
	}
	todayPath := datedRecoveryLink(t, response.Body.String(), "Today's reading")
	today := httptest.NewRecorder()
	handler.ServeHTTP(today, httptest.NewRequest(http.MethodGet, todayPath, nil))
	if today.Code != http.StatusNotFound || !strings.Contains(today.Body.String(), "Request Topic") {
		t.Fatalf("unknown topic must offer an explicit request: %d %s", today.Code, today.Body.String())
	}
	if provider.calls != 0 {
		t.Fatalf("unknown topic recovery called provider %d times", provider.calls)
	}
	for _, table := range []string{"topics", "pages", "daily_readings", "topic_search_runs", "topic_search_results"} {
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("unknown topic recovery created %d rows in %s", count, table)
		}
	}
}

func datedRecoveryLink(t *testing.T, body, label string) string {
	t.Helper()
	pattern := regexp.MustCompile(`<a[^>]*href="([^"]+)"[^>]*>` + regexp.QuoteMeta(label) + `</a>`)
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("recovery page has no %q link", label)
	}
	return html.UnescapeString(match[1])
}
