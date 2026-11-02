package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestAutocompleteKeepsExactAndPrefixMatchesAheadOfSubstringMatches(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	for i := range 12 {
		importWebTopic(t, ctx, conn, fmt.Sprintf("a-go-%d", i), fmt.Sprintf("A Go reference %02d", i))
	}
	importWebTopic(t, ctx, conn, "go", "Go")
	importWebTopic(t, ctx, conn, "godot", "Godot")
	response := httptest.NewRecorder()
	newTestHandler(conn).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/search?q=Go", nil))
	var topics []topicOption
	if err := json.Unmarshal(response.Body.Bytes(), &topics); err != nil {
		t.Fatal(err)
	}
	if len(topics) != 10 || topics[0].Name != "Go" || topics[1].Name != "Godot" {
		t.Fatalf("exact and prefix topics must remain discoverable in bounded results: %+v", topics)
	}
}

func TestAutocompleteTreatsSearchWildcardsLiterally(t *testing.T) {
	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	importWebTopic(t, ctx, conn, "go", "Go")
	for _, query := range []string{"%", "_", `\`} {
		response := httptest.NewRecorder()
		newTestHandler(conn).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/topics/search?q="+url.QueryEscape(query), nil))
		var topics []topicOption
		if response.Code != http.StatusOK {
			t.Fatalf("query %q: %d %s", query, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &topics); err != nil {
			t.Fatal(err)
		}
		if len(topics) != 0 {
			t.Fatalf("query %q unexpectedly matched %+v", query, topics)
		}
	}
}
