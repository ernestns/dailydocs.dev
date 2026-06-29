package topicsearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIReviewerSendsStructuredReviewRequest(t *testing.T) {
	var authHeader string
	var request openAIResponsesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output_text": "{\"results\":[{\"index\":1,\"dailydocs_score\":91,\"page_type\":\"guide\",\"should_store\":true,\"reason\":\"Strong conceptual guide.\"}]}"
		}`))
	}))
	defer server.Close()

	reviewer := OpenAIReviewer{
		APIKey:   "test-key",
		Endpoint: server.URL,
		Client:   server.Client(),
	}
	output, err := reviewer.Review(context.Background(), "Rust", []ReviewCandidate{
		{Index: 1, Title: "Rust Book", URL: "https://doc.rust-lang.org/book", Source: "doc.rust-lang.org", Snippet: "Learn Rust.", ProviderRank: 1},
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}

	if authHeader != "Bearer test-key" {
		t.Fatalf("unexpected auth header %q", authHeader)
	}
	if request.Model != DefaultOpenAIModel {
		t.Fatalf("expected default model %q, got %q", DefaultOpenAIModel, request.Model)
	}
	if request.Text.Format.Type != "json_schema" || request.Text.Format.Name != "dailydocs_review" || !request.Text.Format.Strict {
		t.Fatalf("unexpected text format: %+v", request.Text.Format)
	}
	if request.Store {
		t.Fatalf("expected store=false")
	}
	if request.Reasoning.Effort != "low" {
		t.Fatalf("expected low reasoning effort, got %q", request.Reasoning.Effort)
	}
	if len(request.Input) != 2 || request.Input[0].Role != "system" || request.Input[1].Role != "user" {
		t.Fatalf("unexpected input messages: %+v", request.Input)
	}
	if len(output.Results) != 1 || output.Results[0].DailyDocsScore != 91 || !output.Results[0].ShouldStore {
		t.Fatalf("unexpected review results: %+v", output.Results)
	}
}

func TestOpenAIReviewerRequiresAPIKey(t *testing.T) {
	_, err := OpenAIReviewer{}.Review(context.Background(), "Rust", []ReviewCandidate{{Index: 1}})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("expected api key error, got %v", err)
	}
}
