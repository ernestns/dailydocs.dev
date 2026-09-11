package topicsearch

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestLiveSQLiteQuality is opt-in because it spends provider credits.
// See docs/generation-quality.md for evaluation scope and interpretation.
func TestLiveSQLiteQuality(t *testing.T) {
	if os.Getenv("DAILYDOCS_LIVE_QUALITY") != "1" {
		t.Skip("set DAILYDOCS_LIVE_QUALITY=1 explicitly to spend provider credits")
	}
	if os.Getenv("OPENAI_API_KEY") == "" || os.Getenv("TAVILY_API_KEY") == "" {
		t.Fatal("existing OpenAI and Tavily credentials are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	planner := &qualityPlanner{OpenAITopicPlanner: OpenAITopicPlanner{
		APIKey: os.Getenv("OPENAI_API_KEY"), Model: os.Getenv("OPENAI_PLANNER_MODEL"),
		ReasoningEffort: os.Getenv("OPENAI_PLANNER_REASONING_EFFORT"),
	}}
	provider := &qualityProvider{TavilyClient: TavilyClient{APIKey: os.Getenv("TAVILY_API_KEY")}}
	reviewer := &qualityReviewer{OpenAIReviewer: OpenAIReviewer{APIKey: os.Getenv("OPENAI_API_KEY"), Model: os.Getenv("OPENAI_MODEL")}}
	start := time.Now()
	result, err := SearchTopic(ctx, conn, "SQLite", Options{
		Provider: provider, Planner: planner, Reviewer: reviewer,
		MaxPlannedSearches: 4, MaxResultsPerPlannedSearch: 3, ReviewBatchSize: 20,
	})
	record := struct {
		ElapsedSeconds float64
		Plan           PlanOutput
		Searches       []qualitySearch
		Review         ReviewOutput
		Result         Result
		Error          string
	}{ElapsedSeconds: time.Since(start).Seconds(), Plan: planner.output, Searches: provider.searches, Review: reviewer.output, Result: result}
	if err != nil {
		record.Error = err.Error()
	}
	encoded, marshalErr := json.MarshalIndent(record, "", "  ")
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if path := os.Getenv("DAILYDOCS_LIVE_REPORT"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("SQLite generation: elapsed=%.1fs searches=%d accepted=%d result=%s", record.ElapsedSeconds, len(provider.searches), result.StoredCount, result.Status)
	if err != nil {
		t.Fatal(err)
	}
}

type qualityPlanner struct {
	OpenAITopicPlanner
	output PlanOutput
}

func (p *qualityPlanner) Plan(ctx context.Context, topic string) (PlanOutput, error) {
	output, err := p.OpenAITopicPlanner.Plan(ctx, topic)
	p.output = output
	return output, err
}

type qualitySearch struct {
	Request SearchRequest
	Results []SearchResult
}
type qualityProvider struct {
	TavilyClient
	searches []qualitySearch
}

func (p *qualityProvider) SearchWithRequest(ctx context.Context, request SearchRequest) ([]SearchResult, error) {
	results, err := p.TavilyClient.SearchWithRequest(ctx, request)
	p.searches = append(p.searches, qualitySearch{request, results})
	return results, err
}

type qualityReviewer struct {
	OpenAIReviewer
	output ReviewOutput
}

func (r *qualityReviewer) Review(ctx context.Context, topic string, candidates []ReviewCandidate) (ReviewOutput, error) {
	output, err := r.OpenAIReviewer.Review(ctx, topic, candidates)
	r.output = output
	return output, err
}
