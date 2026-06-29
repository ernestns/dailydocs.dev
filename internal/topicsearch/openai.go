package topicsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultOpenAIEndpoint = "https://api.openai.com/v1/responses"
	DefaultOpenAIModel    = "gpt-5-nano"
)

type OpenAIReviewer struct {
	APIKey   string
	Endpoint string
	Model    string
	Client   *http.Client
}

func (r OpenAIReviewer) Review(ctx context.Context, topic string, candidates []ReviewCandidate) (ReviewOutput, error) {
	if strings.TrimSpace(r.APIKey) == "" {
		return ReviewOutput{}, errors.New("OPENAI_API_KEY is required")
	}
	if len(candidates) == 0 {
		return ReviewOutput{}, nil
	}

	endpoint := strings.TrimSpace(r.Endpoint)
	if endpoint == "" {
		endpoint = DefaultOpenAIEndpoint
	}
	model := strings.TrimSpace(r.Model)
	if model == "" {
		model = DefaultOpenAIModel
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	userPayload, err := json.Marshal(openAIReviewPrompt{
		Topic:      topic,
		Candidates: candidates,
	})
	if err != nil {
		return ReviewOutput{}, fmt.Errorf("encode review prompt: %w", err)
	}

	requestBody, err := json.Marshal(openAIResponsesRequest{
		Model: model,
		Input: []openAIInputMessage{
			{Role: "system", Content: reviewSystemPrompt()},
			{Role: "user", Content: string(userPayload)},
		},
		Text: openAITextConfig{
			Format: openAITextFormat{
				Type:   "json_schema",
				Name:   "dailydocs_review",
				Strict: true,
				Schema: reviewSchema(),
			},
		},
		Store:           false,
		Reasoning:       openAIReasoningConfig{Effort: "low"},
		MaxOutputTokens: 5000,
	})
	if err != nil {
		return ReviewOutput{}, fmt.Errorf("encode openai review request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return ReviewOutput{}, fmt.Errorf("create openai review request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(r.APIKey))

	resp, err := client.Do(req)
	if err != nil {
		return ReviewOutput{}, fmt.Errorf("send openai review request: %w", err)
	}
	defer resp.Body.Close()

	var response openAIResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return ReviewOutput{}, fmt.Errorf("decode openai review response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if response.Error.Message != "" {
			return ReviewOutput{}, fmt.Errorf("openai review failed status=%d error=%s", resp.StatusCode, response.Error.Message)
		}
		return ReviewOutput{}, fmt.Errorf("openai review failed status=%d", resp.StatusCode)
	}

	output := strings.TrimSpace(response.OutputText)
	if output == "" {
		output = strings.TrimSpace(responseText(response))
	}
	if output == "" {
		return ReviewOutput{}, fmt.Errorf("openai review returned no text status=%s incomplete_reason=%s", response.Status, response.IncompleteDetails.Reason)
	}

	var review openAIReviewResponse
	if err := json.Unmarshal([]byte(output), &review); err != nil {
		return ReviewOutput{}, fmt.Errorf("decode openai review json: %w", err)
	}
	return ReviewOutput{
		Results:      review.Results,
		Model:        response.Model,
		InputTokens:  response.Usage.InputTokens,
		OutputTokens: response.Usage.OutputTokens,
		TotalTokens:  response.Usage.TotalTokens,
	}, nil
}

type openAIReviewPrompt struct {
	Topic      string            `json:"topic"`
	Candidates []ReviewCandidate `json:"candidates"`
}

type openAIReviewResponse struct {
	Results []ReviewResult `json:"results"`
}

type openAIResponsesRequest struct {
	Model           string                `json:"model"`
	Input           []openAIInputMessage  `json:"input"`
	Text            openAITextConfig      `json:"text"`
	Store           bool                  `json:"store"`
	Reasoning       openAIReasoningConfig `json:"reasoning"`
	MaxOutputTokens int                   `json:"max_output_tokens"`
}

type openAIInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAITextConfig struct {
	Format openAITextFormat `json:"format"`
}

type openAITextFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type openAIReasoningConfig struct {
	Effort string `json:"effort"`
}

type openAIResponsesResponse struct {
	OutputText        string `json:"output_text"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func reviewSystemPrompt() string {
	return strings.TrimSpace(`
You are the editor of DailyDocs.

DailyDocs recommends one documentation page each day to software engineers.

Your job is to determine how suitable each page is for that purpose.

Score each candidate from 0 to 100.

A score of 100 means this is among the best specific concept pages for daily reading.
A score of 75 means it is a worthwhile concept page but not exceptional.
A score below 40 means it should almost never be shown.

Only set should_store=true when the page itself is a good reading.
The best DailyDocs pages target a specific concept, technique, feature, or workflow.
Good examples include pages like "Generics", "Ownership", "Transactions", "Partial Indexes", "Context", or "Multi-stage Builds".
Do not store broad books, table-of-contents pages, learning hubs, topic homepages, or "getting started with the whole topic" pages.
Do not store pages whose primary value is a list of books, resource links, courses, tools, or other pages.
Resource lists and "best books/resources" pages should usually score below 50.
Landing pages, learning hubs, and entire books should usually score below 65 even when official or authoritative.

Consider:
- educational value
- evergreen content
- standalone readability
- conceptual depth
- practical usefulness
- specificity of the concept being taught
- whether an experienced engineer would recommend reading it

Do not favor API indexes, release notes, navigation pages, social posts, shallow listicles, resource lists, whole books, or generated reference material.
Write reasons in concise ASCII English only.

Return only JSON matching the schema.
`)
}

func reviewSchema() map[string]any {
	result := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"index", "dailydocs_score", "page_type", "should_store", "reason"},
		"properties": map[string]any{
			"index": map[string]any{
				"type": "integer",
			},
			"dailydocs_score": map[string]any{
				"type":    "integer",
				"minimum": 0,
				"maximum": 100,
			},
			"page_type": map[string]any{
				"type": "string",
				"enum": []string{"guide", "tutorial", "concept", "reference", "api", "blog", "listicle", "resource_list", "landing", "social", "other"},
			},
			"should_store": map[string]any{
				"type": "boolean",
			},
			"reason": map[string]any{
				"type":      "string",
				"maxLength": 240,
			},
		},
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"results"},
		"properties": map[string]any{
			"results": map[string]any{
				"type":  "array",
				"items": result,
			},
		},
	}
}

func responseText(response openAIResponsesResponse) string {
	var builder strings.Builder
	for _, output := range response.Output {
		for _, content := range output.Content {
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
}
