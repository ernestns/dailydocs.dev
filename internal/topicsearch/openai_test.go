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
	properties := request.Text.Format.Schema["properties"].(map[string]any)
	resultsSchema := properties["results"].(map[string]any)
	if resultsSchema["minItems"] != float64(1) || resultsSchema["maxItems"] != float64(1) {
		t.Fatalf("review must request one decision per candidate: %+v", resultsSchema)
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

func TestOpenAITopicPlannerSendsStructuredPlanRequest(t *testing.T) {
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
			"model": "gpt-5.5",
			"usage": {"input_tokens": 100, "output_tokens": 80, "total_tokens": 180},
			"output_text": "{\"topics\":[{\"name\":\"Pinning\",\"category\":\"language\",\"reason\":\"Requires memory movement understanding.\",\"search_queries\":[\"Rust Pin Unpin official documentation\"],\"include_domains\":[\"doc.rust-lang.org\"],\"expected_terms\":[\"Pin\",\"Unpin\"]}]}"
		}`))
	}))
	defer server.Close()

	planner := OpenAITopicPlanner{
		APIKey:   "test-key",
		Endpoint: server.URL,
		Client:   server.Client(),
	}
	output, err := planner.Plan(context.Background(), "Rust")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if authHeader != "Bearer test-key" {
		t.Fatalf("unexpected auth header %q", authHeader)
	}
	if request.Model != DefaultOpenAIPlannerModel {
		t.Fatalf("expected planner model %q, got %q", DefaultOpenAIPlannerModel, request.Model)
	}
	if request.Text.Format.Type != "json_schema" || request.Text.Format.Name != "dailydocs_topic_plan" || !request.Text.Format.Strict {
		t.Fatalf("unexpected text format: %+v", request.Text.Format)
	}
	if request.Reasoning.Effort != "high" {
		t.Fatalf("expected high reasoning effort, got %q", request.Reasoning.Effort)
	}
	if request.Store {
		t.Fatalf("expected store=false")
	}
	if len(output.Topics) != 1 || output.Topics[0].Name != "Pinning" || output.TotalTokens != 180 {
		t.Fatalf("unexpected plan output: %+v", output)
	}
}

func TestOpenAITopicPlannerRequiresAPIKey(t *testing.T) {
	_, err := OpenAITopicPlanner{}.Plan(context.Background(), "Rust")
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("expected api key error, got %v", err)
	}
}
