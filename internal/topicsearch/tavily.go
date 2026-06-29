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

const DefaultTavilyEndpoint = "https://api.tavily.com/search"

type TavilyClient struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
}

func (c TavilyClient) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New("TAVILY_API_KEY is required")
	}
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		endpoint = DefaultTavilyEndpoint
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if maxResults < 1 {
		maxResults = DefaultMaxResults
	}

	body, err := json.Marshal(tavilySearchRequest{
		Query:             query,
		SearchDepth:       "basic",
		Topic:             "general",
		MaxResults:        maxResults,
		IncludeAnswer:     false,
		IncludeRawContent: false,
		IncludeImages:     false,
		IncludeFavicon:    false,
		IncludeUsage:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("encode tavily request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send tavily request: %w", err)
	}
	defer resp.Body.Close()

	var searchResp tavilySearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("decode tavily response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if searchResp.Error != "" {
			return nil, fmt.Errorf("tavily search failed status=%d error=%s", resp.StatusCode, searchResp.Error)
		}
		return nil, fmt.Errorf("tavily search failed status=%d", resp.StatusCode)
	}

	results := make([]SearchResult, 0, len(searchResp.Results))
	for _, result := range searchResp.Results {
		results = append(results, SearchResult{
			Title:   result.Title,
			URL:     result.URL,
			Content: result.Content,
			Score:   result.Score,
		})
	}
	return results, nil
}

type tavilySearchRequest struct {
	Query             string `json:"query"`
	SearchDepth       string `json:"search_depth"`
	Topic             string `json:"topic"`
	MaxResults        int    `json:"max_results"`
	IncludeAnswer     bool   `json:"include_answer"`
	IncludeRawContent bool   `json:"include_raw_content"`
	IncludeImages     bool   `json:"include_images"`
	IncludeFavicon    bool   `json:"include_favicon"`
	IncludeUsage      bool   `json:"include_usage"`
}

type tavilySearchResponse struct {
	Results []tavilySearchResult `json:"results"`
	Error   string               `json:"error"`
}

type tavilySearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}
