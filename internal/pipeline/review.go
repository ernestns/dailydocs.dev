package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func buildCandidate(ctx context.Context, reviewer PageReviewer, sub sourceSubmission, doc document, _ int) candidate {
	classification, tags := classify(doc)
	if isCollectionIndex(sub, doc) {
		classification = "Index"
		tags = append(tags, "index")
	}
	score, components := scoreDocument(doc, classification)

	reason := strings.Join(components, "; ")
	estimated := int(math.Ceil(float64(doc.WordCount) / 200.0))
	if estimated < 1 {
		estimated = 1
	}

	review := prefilterReview(doc, classification, estimated)
	if review.Decision == "" {
		var err error
		review, err = reviewer.ReviewPage(ctx, doc)
		if err != nil {
			review = heuristicReview(doc, classification, score, estimated, fmt.Sprintf("review failed: %v", err))
		}
	}
	if strings.TrimSpace(review.Classification) != "" {
		classification = review.Classification
	}
	if review.EstimatedMinutes > 0 {
		estimated = review.EstimatedMinutes
	}

	status := "rejected"
	rejectReason := review.RejectReason
	if review.Decision == "include" {
		status = "eligible"
		rejectReason = ""
	}
	if review.Model == "heuristic" && score < DefaultMinScore {
		status = "rejected"
		rejectReason = "Quality score below threshold."
		if review.RejectStage == "" {
			review.RejectStage = "heuristic_gate"
		}
	}
	if rejectReason == "" && status == "rejected" {
		rejectReason = firstNonEmpty(review.Rationale, "Review did not include this page.")
	}

	return candidate{
		document:         doc,
		Classification:   classification,
		Tags:             tags,
		Score:            score,
		ScoreComponents:  components,
		EstimatedMinutes: estimated,
		Reason:           reason,
		RejectReason:     rejectReason,
		Status:           status,
		Review:           review,
	}
}

func classify(doc document) (string, []string) {
	value := strings.ToLower(doc.Title + " " + doc.URL)
	switch {
	case strings.Contains(value, "release notes") || strings.Contains(value, "releases"):
		return "Release Notes", []string{"release-notes"}
	case strings.Contains(value, "changelog"):
		return "Release Notes", []string{"changelog"}
	case strings.Contains(value, "archive"):
		return "Archive", []string{"archive"}
	case strings.Contains(value, "api") || isGeneratedAPIReference(doc):
		return "API", []string{"api"}
	case strings.Contains(value, "tutorial"):
		return "Tutorial", []string{"tutorial"}
	case strings.Contains(value, "guide") || strings.Contains(value, "/guide"):
		return "Guide", []string{"guide"}
	case strings.Contains(value, "concept") || strings.Contains(value, "/concept"):
		return "Concept", []string{"concept"}
	case strings.Contains(value, "example"):
		return "Example", []string{"example"}
	case strings.Contains(value, "migration"):
		return "Migration", []string{"migration"}
	case strings.Contains(value, "faq"):
		return "FAQ", []string{"faq"}
	default:
		return "Concept", []string{"concept"}
	}
}

func isGeneratedAPIReference(doc document) bool {
	raw, err := url.Parse(doc.NormalizedURL)
	if err != nil {
		return false
	}
	path := strings.ToLower(raw.Path)
	if strings.Contains(path, "/std/") {
		return true
	}
	generatedMarkers := []string{
		"/struct.",
		"/trait.",
		"/enum.",
		"/fn.",
		"/macro.",
		"/primitive.",
		"/keyword.",
		"/type.",
	}
	for _, marker := range generatedMarkers {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return strings.HasSuffix(path, "/all.html") || strings.HasSuffix(path, "/print.html")
}

func isCollectionIndex(sub sourceSubmission, doc document) bool {
	base, err := url.Parse(sub.NormalizedURL)
	if err != nil {
		return false
	}
	candidate, err := url.Parse(doc.NormalizedURL)
	if err != nil {
		return false
	}
	if !strings.EqualFold(base.Host, candidate.Host) {
		return false
	}

	basePath := strings.TrimSuffix(base.Path, "/")
	candidatePath := strings.TrimSuffix(candidate.Path, "/")
	if candidatePath == basePath {
		return true
	}

	prefix := basePath
	if prefix == "" {
		prefix = "/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !strings.HasPrefix(candidate.Path, prefix) {
		return false
	}

	relative := strings.Trim(strings.TrimPrefix(candidate.Path, prefix), "/")
	if relative == "" {
		return true
	}
	parts := strings.Split(relative, "/")
	if len(parts) == 1 && strings.HasSuffix(originalPath(doc), "/") {
		return true
	}
	return len(parts) == 2 && parts[1] == "index.html"
}

func originalPath(doc document) string {
	raw, err := url.Parse(doc.URL)
	if err != nil {
		return ""
	}
	return raw.Path
}

func scoreDocument(doc document, classification string) (int, []string) {
	score := 50
	components := []string{"+50 official documentation source"}

	switch classification {
	case "Tutorial", "Guide", "Concept":
		score += 20
		components = append(components, "+20 tutorial/guide/concept")
	case "Release Notes":
		score -= 40
		components = append(components, "-40 release notes")
	case "Migration":
		score -= 40
		components = append(components, "-40 migration")
	case "Archive":
		score -= 30
		components = append(components, "-30 archive")
	case "API":
		score -= 40
		components = append(components, "-40 api reference")
	case "Index":
		score -= 40
		components = append(components, "-40 documentation index")
	}

	if doc.WordCount >= 500 && doc.WordCount <= 3000 {
		score += 15
		components = append(components, "+15 word count")
	}
	if len(doc.Headings) >= 2 {
		score += 10
		components = append(components, "+10 headings")
	}
	if doc.WordCount < 100 {
		score -= 10
		components = append(components, "-10 very short")
	}
	if doc.ParagraphCount >= 3 {
		score += 10
		components = append(components, "+10 paragraphs")
	}
	if doc.LinkDensity > 0.2 {
		score -= 20
		components = append(components, "-20 high link density")
	}
	if doc.LinkDensity > 0.5 {
		score -= 30
		components = append(components, "-30 listing-like link density")
	}
	return score, components
}

func reviewerFromEnv(client *http.Client) PageReviewer {
	reviewer := openAIReviewerFromEnv(client)
	if reviewer.apiKey == "" {
		return heuristicReviewer{}
	}
	return reviewer
}

func openAIReviewerFromEnv(client *http.Client) openAIReviewer {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if apiKey == "" {
		return openAIReviewer{}
	}
	if model == "" {
		model = defaultOpenAIModel
	}
	return openAIReviewer{
		client:   client,
		apiKey:   apiKey,
		model:    model,
		endpoint: "https://api.openai.com/v1/responses",
	}
}

type heuristicReviewer struct{}

func (heuristicReviewer) ReviewPage(_ context.Context, doc document) (Review, error) {
	classification, _ := classify(doc)
	score, _ := scoreDocument(doc, classification)
	estimated := int(math.Ceil(float64(doc.WordCount) / 200.0))
	if estimated < 1 {
		estimated = 1
	}
	return heuristicReview(doc, classification, score, estimated, ""), nil
}

func prefilterReview(doc document, classification string, estimated int) Review {
	if doc.WordCount >= 50 {
		return Review{}
	}
	return Review{
		Decision:         "exclude",
		Classification:   classification,
		Confidence:       1,
		EstimatedMinutes: estimated,
		Rationale:        "Very little extracted text.",
		RejectReason:     "Very little extracted text.",
		GateScore:        0,
		GatePageType:     "too_thin",
		RejectStage:      "prefilter",
		Model:            "prefilter",
		PromptVersion:    reviewPromptVersion,
		InputHash:        reviewInputHash(doc),
	}
}

func heuristicReview(doc document, classification string, score int, estimated int, fallbackReason string) Review {
	decision := "include"
	rationale := "Standalone documentation page with enough extracted content."
	rejectReason := ""
	confidence := 0.65
	gateScore := 80
	gatePageType := "standalone_doc"
	rejectStage := ""

	switch {
	case classification == "Index":
		decision = "exclude"
		rejectReason = "Documentation index or landing page."
		confidence = 0.85
		gateScore = 20
		gatePageType = "index_page"
		rejectStage = "heuristic_gate"
	case classification == "Release Notes":
		decision = "exclude"
		rejectReason = "Release notes are not durable daily reading material."
		confidence = 0.8
		gateScore = 30
		gatePageType = "release_notes"
		rejectStage = "heuristic_gate"
	case classification == "API":
		decision = "exclude"
		rejectReason = "Generated or API reference shaped page."
		confidence = 0.75
		gateScore = 40
		gatePageType = "api_reference"
		rejectStage = "heuristic_gate"
	case doc.WordCount < 100:
		decision = "exclude"
		rejectReason = "Very little extracted text."
		confidence = 0.7
		gateScore = 20
		gatePageType = "too_thin"
		rejectStage = "heuristic_gate"
	case doc.LinkDensity > 0.5:
		decision = "exclude"
		rejectReason = "Page appears to be mostly links."
		confidence = 0.75
		gateScore = 30
		gatePageType = "mostly_links"
		rejectStage = "heuristic_gate"
	case score < DefaultMinScore:
		decision = "exclude"
		rejectReason = "Quality score below threshold."
		confidence = 0.6
		gateScore = 50
		gatePageType = "other"
		rejectStage = "heuristic_gate"
	}
	if fallbackReason != "" {
		rationale = fallbackReason
		if rejectReason == "" {
			rejectReason = fallbackReason
		}
	}
	if rejectReason != "" {
		rationale = rejectReason
	}
	return Review{
		Decision:         decision,
		Classification:   classification,
		Confidence:       confidence,
		EstimatedMinutes: estimated,
		Rationale:        rationale,
		RejectReason:     rejectReason,
		GateScore:        gateScore,
		GatePageType:     gatePageType,
		RejectStage:      rejectStage,
		Model:            "heuristic",
		PromptVersion:    reviewPromptVersion,
		InputHash:        reviewInputHash(doc),
	}
}

type openAIReviewer struct {
	client   *http.Client
	apiKey   string
	model    string
	endpoint string
}

type gateReview struct {
	DailyDocsScore int    `json:"dailydocs_score"`
	PageType       string `json:"page_type"`
}

func gateSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"dailydocs_score": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"maximum":     100,
				"description": "0 means definitely reject; 100 means among the best pages in the documentation set for daily reading.",
			},
			"page_type": map[string]any{
				"type": "string",
				"enum": []string{"tutorial", "guide", "concept", "reference_concept", "api_reference", "index_page", "navigation_page", "release_notes", "changelog", "exercise_or_quiz", "playground", "product_page", "too_thin", "mostly_links", "other"},
			},
		},
		"required": []string{"dailydocs_score", "page_type"},
	}
}

func gatePrompt() string {
	return `You are the editor of DailyDocs.

DailyDocs recommends one documentation page each day to software engineers.

Your job is to determine how suitable this page is for that purpose.

A DailyDocs score of 100 means this is among the best pages in the documentation set for daily reading.
A score of 75 means it is worthwhile but not exceptional.
A score below 40 means it should almost never be shown.

Consider:

* educational value
* evergreen content
* standalone readability
* conceptual depth
* practical usefulness
* whether it is self-contained
* whether it teaches a concept
* whether it can be read in one sitting
* whether an experienced engineer would recommend reading it

Do not favor API indexes, release notes, navigation pages, or generated reference material.

Return only JSON matching the schema.`
}

func enrichmentSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"decision": map[string]any{
				"type": "string",
				"enum": []string{"include", "exclude", "needs_review"},
			},
			"classification": map[string]any{
				"type": "string",
				"enum": []string{"Tutorial", "Guide", "Concept", "Reference", "API Reference", "Index", "Release Notes", "Exercise", "Playground", "Product Page", "Other"},
			},
			"confidence": map[string]any{
				"type": "number",
			},
			"estimated_minutes": map[string]any{
				"type": "integer",
			},
			"rationale": map[string]any{
				"type": "string",
			},
			"rejection_reason": map[string]any{
				"type": "string",
			},
		},
		"required": []string{"decision", "classification", "confidence", "estimated_minutes", "rationale", "rejection_reason"},
	}
}

func (r openAIReviewer) ReviewPage(ctx context.Context, doc document) (Review, error) {
	gateResult, err := r.GatePage(ctx, doc, false)
	if err != nil {
		return Review{}, err
	}
	if gateResult.Review.Decision == "exclude" {
		return gateResult.Review, nil
	}

	input := reviewInput(doc)
	var review Review
	enrichmentUsage, err := r.callOpenAIJSON(ctx, "daily_docs_page_enrichment", enrichmentSchema(), "You review documentation page metadata for DailyDocs. Include standalone tutorials, guides, and concept pages. Exclude landing pages, indexes, generated API references, release notes, changelogs, quizzes, playgrounds, and product pages. Return only valid JSON matching the schema.", input, &review, nil)
	if err != nil {
		return Review{}, err
	}
	review.GateScore = gateResult.Review.GateScore
	review.GatePageType = gateResult.Review.GatePageType
	review.GateUsage = gateResult.Review.GateUsage
	review.EnrichmentUsage = enrichmentUsage
	if review.Decision != "include" {
		review.RejectStage = "ai_enrichment"
	}
	review.Model = r.model
	review.PromptVersion = reviewPromptVersion
	review.InputHash = reviewInputHash(doc)
	return review, nil
}

func (r openAIReviewer) GatePage(ctx context.Context, doc document, includeRawResponse bool) (GateDebugResult, error) {
	var gate gateReview
	var raw json.RawMessage
	var rawPtr *json.RawMessage
	if includeRawResponse {
		rawPtr = &raw
	}
	request := r.requestBody("daily_docs_page_gate", gateSchema(), gatePrompt(), gateInput(doc))
	usage, err := r.callOpenAIJSON(ctx, "daily_docs_page_gate", gateSchema(), gatePrompt(), gateInput(doc), &gate, rawPtr)
	if err != nil {
		return GateDebugResult{Request: request}, err
	}

	estimated := int(math.Ceil(float64(doc.WordCount) / 200.0))
	if estimated < 1 {
		estimated = 1
	}
	review := Review{
		Decision:         "include",
		Classification:   "Other",
		Confidence:       float64(gate.DailyDocsScore) / 100,
		EstimatedMinutes: estimated,
		Rationale:        fmt.Sprintf("AI gate score %d met threshold.", gate.DailyDocsScore),
		GateScore:        gate.DailyDocsScore,
		GatePageType:     gate.PageType,
		Model:            r.model,
		PromptVersion:    reviewPromptVersion,
		InputHash:        reviewInputHash(doc),
		GateUsage:        usage,
	}
	if gate.DailyDocsScore < DefaultGateThreshold {
		review.Decision = "exclude"
		review.Rationale = fmt.Sprintf("AI gate score %d below threshold.", gate.DailyDocsScore)
		review.RejectReason = review.Rationale
		review.RejectStage = "ai_gate"
	}
	return GateDebugResult{
		Request:  request,
		Response: raw,
		Review:   review,
	}, nil
}

func (r openAIReviewer) requestBody(schemaName string, schema map[string]any, systemPrompt string, input map[string]any) map[string]any {
	body := map[string]any{
		"model": r.model,
		"reasoning": map[string]any{
			"effort": "low",
		},
		"input": []map[string]string{
			{
				"role":    "system",
				"content": systemPrompt,
			},
			{
				"role":    "user",
				"content": mustJSON(input),
			},
		},
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   schemaName,
				"schema": schema,
				"strict": true,
			},
		},
	}
	return body
}

func (r openAIReviewer) callOpenAIJSON(ctx context.Context, schemaName string, schema map[string]any, systemPrompt string, input map[string]any, output any, rawResponse *json.RawMessage) (TokenUsage, error) {
	body := r.requestBody(schemaName, schema, systemPrompt, input)
	encoded, err := json.Marshal(body)
	if err != nil {
		return TokenUsage{}, err
	}
	endpoint := r.endpoint
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/responses"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return TokenUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return TokenUsage{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return TokenUsage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenUsage{}, fmt.Errorf("openai review status %d: %s", resp.StatusCode, shortenText(string(respBody), 500))
	}
	if rawResponse != nil {
		*rawResponse = append((*rawResponse)[:0], respBody...)
	}

	text := responseOutputText(respBody)
	if text == "" {
		return TokenUsage{}, errors.New("openai review missing output text")
	}
	if err := json.Unmarshal([]byte(text), output); err != nil {
		return TokenUsage{}, fmt.Errorf("decode openai review: %w", err)
	}
	return responseTokenUsage(respBody), nil
}

func reviewInput(doc document) map[string]any {
	return map[string]any{
		"title":            truncateText(doc.Title, enrichmentMaxTitleChars),
		"url":              doc.NormalizedURL,
		"canonical_url":    doc.CanonicalURL,
		"description":      truncateText(doc.MetaDescription, enrichmentMaxDescriptionChars),
		"h1":               truncateText(doc.H1, enrichmentMaxTitleChars),
		"breadcrumbs":      sampleStrings(doc.Breadcrumbs, enrichmentMaxBreadcrumbs, enrichmentMaxBreadcrumbChars),
		"headings_sample":  sampleStrings(doc.Headings, enrichmentMaxHeadings, enrichmentMaxHeadingChars),
		"heading_count":    len(doc.Headings),
		"first_paragraph":  truncateText(doc.FirstParagraph, enrichmentMaxFirstParagraphChars),
		"word_count":       doc.WordCount,
		"paragraph_count":  doc.ParagraphCount,
		"link_count":       doc.LinkCount,
		"code_block_count": doc.CodeBlockCount,
		"code_ratio":       doc.CodeRatio,
		"link_density":     doc.LinkDensity,
		"text_excerpt":     truncateText(doc.Text, enrichmentMaxExcerptChars),
	}
}

func gateInput(doc document) map[string]any {
	return map[string]any{
		"title":           truncateText(doc.Title, gateMaxTitleChars),
		"url":             doc.NormalizedURL,
		"canonical_url":   doc.CanonicalURL,
		"description":     truncateText(doc.MetaDescription, gateMaxDescriptionChars),
		"h1":              truncateText(doc.H1, gateMaxTitleChars),
		"breadcrumbs":     sampleStrings(doc.Breadcrumbs, gateMaxBreadcrumbs, gateMaxBreadcrumbChars),
		"headings_sample": sampleStrings(doc.Headings, gateMaxHeadings, gateMaxHeadingChars),
		"heading_count":   len(doc.Headings),
		"first_paragraph": truncateText(doc.FirstParagraph, gateMaxFirstParagraphChars),
		"word_count":      doc.WordCount,
		"paragraph_count": doc.ParagraphCount,
		"link_count":      doc.LinkCount,
		"code_ratio":      doc.CodeRatio,
		"link_density":    doc.LinkDensity,
	}
}

func sampleStrings(values []string, limit int, maxChars int) []string {
	if limit < 0 {
		limit = 0
	}
	if len(values) < limit {
		limit = len(values)
	}
	sample := make([]string, 0, limit)
	for _, value := range values[:limit] {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		sample = append(sample, truncateText(value, maxChars))
	}
	return sample
}

func truncateText(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	if maxChars < 0 || len(value) <= maxChars {
		return value
	}
	if maxChars <= 3 {
		return value[:maxChars]
	}
	return value[:maxChars-3] + "..."
}

func reviewInputHash(doc document) string {
	sum := sha256.Sum256([]byte(mustJSON(reviewInput(doc))))
	return fmt.Sprintf("%x", sum[:])
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func responseOutputText(body []byte) string {
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if parsed.OutputText != "" {
		return parsed.OutputText
	}
	for _, output := range parsed.Output {
		for _, content := range output.Content {
			if content.Text != "" {
				return content.Text
			}
		}
	}
	return ""
}

func responseTokenUsage(body []byte) TokenUsage {
	var parsed struct {
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			TotalTokens         int `json:"total_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:     parsed.Usage.InputTokens,
		OutputTokens:    parsed.Usage.OutputTokens,
		ReasoningTokens: parsed.Usage.OutputTokensDetails.ReasoningTokens,
		TotalTokens:     parsed.Usage.TotalTokens,
	}
}

func shortenText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}
