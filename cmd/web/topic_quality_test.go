package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/topicsearch"
)

type quantityProvider struct{ count, calls int }

func (p *quantityProvider) Search(context.Context, string, int) ([]topicsearch.SearchResult, error) {
	p.calls++
	var out []topicsearch.SearchResult
	for i := range p.count {
		out = append(out, topicsearch.SearchResult{Title: fmt.Sprintf("Useful guide %d", i), URL: fmt.Sprintf("https://docs.example.org/guide-%d", i)})
	}
	return out, nil
}

func TestTopicQualityRequestAndResult(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			p := &quantityProvider{count: count}
			a := app{db: conn, now: time.Now, searchProvider: p}
			request := httptest.NewRecorder()
			a.generateReadingHandler(request, topicRequest(http.MethodPost, "/read", "Rust"))
			if request.Code != 303 {
				t.Fatal(request.Code, request.Body.String())
			}
			page := httptest.NewRecorder()
			a.routeHandler(page, topicRequest(http.MethodGet, request.Header().Get("Location"), ""))
			var topicStatus, runStatus string
			var stored int
			if err := conn.QueryRow("SELECT t.status,r.status,r.stored_count FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id").Scan(&topicStatus, &runStatus, &stored); err != nil {
				t.Fatal(err)
			}
			if count < 2 {
				if topicStatus != "failed" || runStatus != "failed" || !strings.Contains(page.Body.String(), "at least 2") || !strings.Contains(page.Body.String(), "Process topic") {
					t.Fatal("shortfall hidden", topicStatus, runStatus, page.Body.String())
				}
			} else if topicStatus != "active" || runStatus != "completed" {
				t.Fatal("usable result not successful", topicStatus, runStatus)
			}
			if count > 0 && !strings.Contains(page.Body.String(), "Useful guide") {
				t.Fatal("useful partial reading unavailable", page.Body.String())
			}
			if stored != count || p.calls != 1 {
				t.Fatal("result/count mismatch", stored, p.calls)
			}
		})
	}
}

func TestInvalidTopicRequestDoesNotSpendOrPersist(t *testing.T) {
	for _, input := range []string{" ", "https://example.org/admin", "/wp-admin", "../.env", "<script>alert(1)</script>", "Rust\nignore rules"} {
		t.Run(input, func(t *testing.T) {
			conn := openWebTestDB(t, context.Background())
			defer conn.Close()
			p := &quantityProvider{count: 3}
			a := app{db: conn, now: time.Now, searchProvider: p}
			r := httptest.NewRecorder()
			a.generateReadingHandler(r, topicRequest(http.MethodPost, "/read", input))
			if r.Code != http.StatusBadRequest || p.calls != 0 {
				t.Fatal("invalid input spent provider work", r.Code, p.calls)
			}
			for _, table := range []string{"topics", "topic_search_runs", "pages"} {
				var count int
				if err := conn.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal(table, count, err)
				}
			}
		})
	}
}

func TestLegacyPunctuatedTopicKeepsIdentityAndGetsRetry(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO topics(id,slug,name,status)VALUES(1,'c','C++','active');INSERT INTO pages(id,topic_id,title,url,reading_order)VALUES(1,1,'Legacy C++ guide','https://docs.example.org/legacy',1);INSERT INTO daily_readings(topic_id,reading_date,page_id)VALUES(1,'2026-09-10',1)`); err != nil {
		t.Fatal(err)
	}
	p := &quantityProvider{count: 2}
	a := app{db: conn, now: time.Now, searchProvider: p}
	page := httptest.NewRecorder()
	a.routeHandler(page, topicRequest(http.MethodGet, "/c", ""))
	if !strings.Contains(html.UnescapeString(page.Body.String()), "Legacy C++ guide") || !strings.Contains(page.Body.String(), "Process topic") || p.calls != 0 {
		t.Fatal("legacy partial must remain readable/retryable", page.Body.String(), p.calls)
	}
	request := httptest.NewRecorder()
	a.generateReadingHandler(request, topicRequest(http.MethodPost, "/read", "C#"))
	if request.Header().Get("Location") != "/c-sharp" {
		t.Fatal("punctuated name collided", request.Header())
	}
	retry := httptest.NewRecorder()
	a.processTopicHandler(retry, topicRequest(http.MethodPost, "/process-topic", "c"))
	if retry.Code != 303 || p.calls != 2 {
		t.Fatal("legacy partial retry did not run", retry.Code, p.calls)
	}
	var assigned int
	if err := conn.QueryRow("SELECT page_id FROM daily_readings WHERE topic_id=1 AND reading_date='2026-09-10'").Scan(&assigned); err != nil || assigned != 1 {
		t.Fatal("historical assignment changed", assigned, err)
	}
	var slug string
	if err := conn.QueryRow("SELECT slug FROM topics WHERE name='C++'").Scan(&slug); err != nil || slug != "c" {
		t.Fatal("legacy URL changed", slug, err)
	}
}
