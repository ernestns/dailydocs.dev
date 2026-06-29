package topicsearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTavilyClientSendsSearchRequest(t *testing.T) {
	var authHeader string
	var request tavilySearchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Rust Book","url":"https://doc.rust-lang.org/book/","content":"Learn Rust","score":0.9}]}`))
	}))
	defer server.Close()

	client := TavilyClient{
		APIKey:   "test-key",
		Endpoint: server.URL,
		Client:   server.Client(),
	}
	results, err := client.Search(context.Background(), "Rust specific concept tutorial guide deep dive documentation", 7)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if authHeader != "Bearer test-key" {
		t.Fatalf("unexpected auth header %q", authHeader)
	}
	if request.Query != "Rust specific concept tutorial guide deep dive documentation" || request.MaxResults != 7 {
		t.Fatalf("unexpected request: %+v", request)
	}
	if len(request.ExcludeDomains) == 0 {
		t.Fatalf("expected excluded domains in request")
	}
	if len(results) != 1 || results[0].Title != "Rust Book" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestTavilyClientRequiresAPIKey(t *testing.T) {
	_, err := TavilyClient{}.Search(context.Background(), "Rust", 10)
	if err == nil || !strings.Contains(err.Error(), "TAVILY_API_KEY") {
		t.Fatalf("expected api key error, got %v", err)
	}
}
