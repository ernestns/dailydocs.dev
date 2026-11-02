package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestUnicodeAutocompleteRanksExactNameBeforeLimit(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	for i := range 12 {
		importWebTopic(t, ctx, conn, fmt.Sprintf("reference-%d", i), fmt.Sprintf("A École SDK reference %02d", i))
	}
	importWebTopic(t, ctx, conn, "saved-school-guide", "École SDK")
	importWebTopic(t, ctx, conn, "saved-school-reference", "École SDK Reference")
	for _, query := range []string{"École SDK", "école sdk", "ÉCOLE SDK"} {
		response := httptest.NewRecorder()
		newTestHandler(conn).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/search?q="+url.QueryEscape(query), nil))
		var topics []topicOption
		if err := json.Unmarshal(response.Body.Bytes(), &topics); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || len(topics) != 10 || topics[0].Slug != "saved-school-guide" || topics[0].Name != "École SDK" || topics[1].Slug != "saved-school-reference" {
			t.Fatalf("Unicode exact/prefix matches hidden by bounded suggestions for %q: status=%d %+v", query, response.Code, topics)
		}
	}
}

func TestUnicodeNamedCatalogRetainsIdentityAndSavedReadings(t *testing.T) {
	for _, slug := range []string{"sdk-école", "saved-unicode-guide"} {
		t.Run(slug, func(t *testing.T) {
			ctx := context.Background()
			conn := openWebTestDB(t, ctx)
			defer conn.Close()
			importWebTopic(t, ctx, conn, "saved-unicode-guide", "SDK École")
			// Generated topics can have Unicode slugs; imports retain historical
			// ASCII slugs. Exercise both stored layouts without generating work.
			if _, err := conn.Exec("UPDATE topics SET slug=?", slug); err != nil {
				t.Fatal(err)
			}
			for _, date := range []string{"2026-06-26", "2026-06-27"} {
				seedWebDailyReading(t, ctx, conn, slug, date, "Write-Ahead Logging")
			}
			provider := &countingWebProvider{}
			handler := newTestHandlerWithProvider(conn, provider)
			for _, query := range []string{"SDK École", "sdk école", "SDK ÉCOLE"} {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					request := httptest.NewRequest(method, "/read?topic="+url.QueryEscape(query), nil)
					if method == http.MethodPost {
						request = httptest.NewRequest(method, "/read", strings.NewReader(url.Values{"topic": {query}}.Encode()))
						request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					location, err := url.Parse(response.Header().Get("Location"))
					if err != nil || response.Code != http.StatusSeeOther || location.Path != "/"+slug || location.RawQuery != "" {
						t.Fatalf("%s %q did not reuse the saved catalog: status=%d location=%q error=%v", method, query, response.Code, response.Header().Get("Location"), err)
					}
				}
			}
			for _, path := range []string{"/" + slug, "/" + slug + "/2026-06-26"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "<h1>SDK École</h1>") || !strings.Contains(response.Body.String(), `class="reading-link" href="https://sqlite.org/wal.html"`) {
					t.Fatal("named lookup changed current or saved reading", path, response.Code)
				}
			}
			var topics, pages, assignments, runs, assignedPages int
			var storedName, storedSlug string
			if err := conn.QueryRow(`SELECT name,slug,
				(SELECT count(*) FROM topics),(SELECT count(*) FROM pages),
				(SELECT count(*) FROM daily_readings),(SELECT count(*) FROM topic_search_runs),
				(SELECT count(*) FROM daily_readings WHERE page_id=1) FROM topics WHERE id=1`).Scan(&storedName, &storedSlug, &topics, &pages, &assignments, &runs, &assignedPages); err != nil {
				t.Fatal(err)
			}
			if storedName != "SDK École" || storedSlug != slug || topics != 1 || pages != 1 || assignments != 2 || assignedPages != 2 || runs != 0 || provider.calls != 0 {
				t.Fatal("Unicode name variant changed saved identity or generated work", storedName, storedSlug, topics, pages, assignments, runs, assignedPages, provider.calls)
			}
		})
	}
}

func TestUnicodeCatalogFiltersKeepPaginationAndLiteralWildcards(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	for i := range 53 {
		if _, err := conn.Exec("INSERT INTO topics(slug,name,status) VALUES(?,?,'active')", fmt.Sprintf("school-%03d", i), fmt.Sprintf("École SDK %03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec("INSERT INTO topics(slug,name,status) VALUES('queued-school','École SDK queued','queued')"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"École SDK", "école sdk", "ÉCOLE SDK"} {
		for pageNumber, wantRows := range map[int]int{1: 50, 2: 3} {
			filter := topicCatalogFilter{Query: query, Status: "active", Page: pageNumber}
			page, err := listRequestedTopics(ctx, conn, filter)
			if err != nil || page.Total != 53 || page.TotalPages != 2 || len(page.Topics) != wantRows {
				t.Fatalf("Unicode filter lost its bounded page: %+v %v", page, err)
			}
			for i, topic := range page.Topics {
				if want := fmt.Sprintf("school-%03d", (pageNumber-1)*50+i); topic.Slug != want {
					t.Fatalf("Unicode matching changed catalog order: got %s want %s", topic.Slug, want)
				}
			}
			link := page.NextURL
			if pageNumber == 2 {
				link = page.PreviousURL
			}
			parsed, err := url.Parse(link)
			if err != nil || parsed.Query().Get("q") != query || parsed.Query().Get("status") != "active" {
				t.Fatal("pagination changed the visible Unicode filter", link, err)
			}
			response := catalogResponse(t, conn, topicCatalogURL(filter, pageNumber))
			if response.Code != http.StatusOK || catalogRows(response.Body.String()) != wantRows {
				t.Fatal("Unicode catalog response lost matching rows", response.Code)
			}
		}
	}
	for i, literal := range []string{"%", "_", `\`} {
		slug := fmt.Sprintf("literal-%d", i)
		if _, err := conn.Exec("INSERT INTO topics(slug,name,status) VALUES(?,?,'active')", slug, "École "+literal+" Reference"); err != nil {
			t.Fatal(err)
		}
		query := "école " + literal
		page, err := listRequestedTopics(ctx, conn, topicCatalogFilter{Query: query, Page: 1})
		if err != nil || page.Total != 1 || len(page.Topics) != 1 || page.Topics[0].Slug != slug {
			t.Fatal("Unicode catalog filtering treated literal punctuation as wildcards", query, page, err)
		}
		matches, err := listTopics(ctx, conn, query, 10)
		if err != nil || len(matches) != 1 || matches[0].Slug != slug {
			t.Fatal("Unicode autocomplete treated literal punctuation as wildcards", query, matches, err)
		}
	}
}
