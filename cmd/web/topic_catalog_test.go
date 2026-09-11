package main

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func seedCatalog(t *testing.T, conn *sql.DB, count int) {
	t.Helper()
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < count; i++ {
		status := "queued"
		if i%2 == 0 {
			status = "failed"
		}
		if _, err := tx.Exec("INSERT INTO topics(slug,name,status) VALUES(?,?,?)", fmt.Sprintf("topic-%03d", i), fmt.Sprintf("Topic %03d", i), status); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func catalogResponse(t *testing.T, conn *sql.DB, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	newTestHandler(conn).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func catalogRows(body string) int {
	_, table, ok := strings.Cut(body, "<tbody>")
	if !ok {
		return 0
	}
	table, _, _ = strings.Cut(table, "</tbody>")
	return strings.Count(table, "<tr")
}

func TestTopicCatalogBoundsRowsAndKeepsStablePages(t *testing.T) {
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	seedCatalog(t, conn, 123)
	for _, tc := range []struct {
		query                string
		wantRows             int
		first, last, summary string
	}{{"", 50, "topic-000", "topic-049", "1–50 of 123 topics"}, {"?page=2", 50, "topic-050", "topic-099", "51–100 of 123 topics"}, {"?page=3", 23, "topic-100", "topic-122", "101–123 of 123 topics"}, {"?page=999999999", 23, "topic-100", "topic-122", "101–123 of 123 topics"}} {
		w := catalogResponse(t, conn, "/topics"+tc.query)
		body := w.Body.String()
		rows := catalogRows(body)
		t.Logf("request=%s status=%d rendered_rows=%d response_bytes=%d", tc.query, w.Code, rows, len(body))
		if w.Code != 200 || rows != tc.wantRows {
			t.Fatalf("unbounded or incorrect page: status=%d rows=%d want=%d", w.Code, rows, tc.wantRows)
		}
		for _, want := range []string{`href="/` + tc.first + `"`, `href="/` + tc.last + `"`, tc.summary, "No readings yet"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	}
}

func TestTopicCatalogFiltersAndPaginationCompose(t *testing.T) {
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	seedCatalog(t, conn, 123)
	w := catalogResponse(t, conn, "/topics?q=Topic&status=failed")
	body := html.UnescapeString(w.Body.String())
	if w.Code != 200 || catalogRows(body) != 50 || !strings.Contains(body, "1–50 of 62 topics") {
		t.Fatal("wrong filtered page", w.Code, body)
	}
	if !strings.Contains(body, `href="/topics?page=2&q=Topic&status=failed"`) {
		t.Fatal("next link lost filters", body)
	}
	w = catalogResponse(t, conn, "/topics?q=Topic&status=failed&page=2")
	body = html.UnescapeString(w.Body.String())
	if catalogRows(body) != 12 || !strings.Contains(body, "51–62 of 62 topics") || !strings.Contains(body, `href="/topics?page=1&q=Topic&status=failed"`) {
		t.Fatal("second page or previous link wrong", body)
	}
	if strings.Contains(body, `href="/topic-101"`) {
		t.Fatal("status filter included queued topic")
	}
	for _, q := range []string{"%", "_", "\\"} {
		w := catalogResponse(t, conn, "/topics?q="+url.QueryEscape(q))
		if w.Code != 200 || catalogRows(w.Body.String()) != 0 {
			t.Fatal("search wildcard was not literal", q, w.Body.String())
		}
	}
}

func TestTopicCatalogEmptyInvalidAndCounts(t *testing.T) {
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	if w := catalogResponse(t, conn, "/topics?page=9"); w.Code != 200 || !strings.Contains(w.Body.String(), "No topics yet.") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, q := range []string{"?page=0", "?page=-2", "?page=nope", "?page=999999999999999999999999", "?status=invalid"} {
		if w := catalogResponse(t, conn, "/topics"+q); w.Code != 400 {
			t.Fatal(q, w.Code)
		}
	}
	importWebTopic(t, context.Background(), conn, "sqlite", "SQLite")
	if _, err := conn.Exec("INSERT INTO topics(slug,name,status) VALUES('hidden','Hidden','disabled')"); err != nil {
		t.Fatal(err)
	}
	w := catalogResponse(t, conn, "/topics")
	body := w.Body.String()
	if !strings.Contains(body, "1–1 of 1 topics") || strings.Contains(body, `href="/hidden"`) || strings.Contains(body, "No readings yet") || !strings.Contains(body, "Needs more readings") {
		t.Fatal("active/disabled reading counts wrong", body)
	}
	if w := catalogResponse(t, conn, "/topics?q=missing"); w.Code != 200 || !strings.Contains(w.Body.String(), "No topics match these filters.") || !strings.Contains(w.Body.String(), `href="/topics"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, topic := range []string{"", "   ", "\t\n"} {
		request := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(url.Values{"topic": {topic}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		newTestHandler(conn).ServeHTTP(w, request)
		if w.Code != 400 {
			t.Fatal("server blank validation changed", topic, w.Code)
		}
	}
}

func TestTopicCatalogTiedNamesStayOrderedAndPunctuationIsSearchable(t *testing.T) {
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	seedCatalog(t, conn, 105)
	if _, err := conn.Exec("UPDATE topics SET name='Same name'"); err != nil {
		t.Fatal(err)
	}
	for _, page := range []int{1, 2, 3} {
		filter := topicCatalogFilter{Page: page}
		result, err := listRequestedTopics(context.Background(), conn, filter)
		if err != nil {
			t.Fatal(err)
		}
		for i, topic := range result.Topics {
			want := fmt.Sprintf("topic-%03d", (page-1)*50+i)
			if topic.Slug != want {
				t.Fatalf("unstable tied-name order: %s want %s", topic.Slug, want)
			}
		}
	}
	for i, name := range []string{"Go", "R", "C++", "C#", ".NET"} {
		if _, err := conn.Exec("INSERT INTO topics(slug,name,status) VALUES(?,?,'queued')", fmt.Sprintf("short-%d", i), name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Go", "C++", "C#", ".NET"} {
		page, err := listRequestedTopics(context.Background(), conn, topicCatalogFilter{Query: name, Page: 1})
		if err != nil || page.Total != 1 || len(page.Topics) != 1 || page.Topics[0].Name != name {
			t.Fatalf("literal short search %q failed: %+v %v", name, page, err)
		}
	}
}
