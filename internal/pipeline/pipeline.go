package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ernestns/daily-docs/internal/submission"
)

const (
	DefaultMaxPages                  = 250
	DefaultMaxDiscovered             = 500
	DefaultMaxDepth                  = 3
	DefaultMaxBytes                  = 2 * 1024 * 1024
	DefaultMinScore                  = 70
	DefaultGateThreshold             = 75
	gateMaxTitleChars                = 200
	gateMaxDescriptionChars          = 500
	gateMaxHeadingChars              = 120
	gateMaxHeadings                  = 40
	gateMaxFirstParagraphChars       = 500
	gateMaxBreadcrumbChars           = 80
	gateMaxBreadcrumbs               = 8
	enrichmentMaxTitleChars          = 200
	enrichmentMaxDescriptionChars    = 1000
	enrichmentMaxHeadingChars        = 160
	enrichmentMaxHeadings            = 80
	enrichmentMaxExcerptChars        = 3000
	enrichmentMaxFirstParagraphChars = 800
	enrichmentMaxBreadcrumbChars     = 120
	enrichmentMaxBreadcrumbs         = 12
	defaultOpenAIModel               = "gpt-5-nano"
	rulesVersion                     = "heuristic-v1"
	reviewPromptVersion              = "metadata-review-v1"
	defaultRequestAgent              = "DailyDocs/0.1"
)

type DiscoveryTooBroadError struct {
	BaseURL string
	Count   int
	Limit   int
}

func (e DiscoveryTooBroadError) Error() string {
	return fmt.Sprintf("documentation source is too broad: discovered at least %d URLs for %s; submit a narrower documentation root", e.Count, e.BaseURL)
}

type Options struct {
	Client   *http.Client
	MaxPages int
	MaxDepth int
	MaxBytes int64
	MinScore int
	Reviewer PageReviewer
}

type Result struct {
	SubmissionID    int64
	TopicSourceID   int64
	PipelineRunID   int64
	DiscoveredCount int
	CrawledCount    int
	EligibleCount   int
	RejectedCount   int
	FailureCount    int
}

type sourceSubmission struct {
	ID              int64
	TopicSourceID   int64
	TopicID         int64
	TopicSlug       string
	TopicName       string
	SubmittedURL    string
	NormalizedURL   string
	SourceHost      string
	SuggestedTopic  string
	ProcessAsSource bool
}

type rawDocument struct {
	URL        string
	FinalURL   string
	HTML       string
	StatusCode int
}

type document struct {
	URL             string
	NormalizedURL   string
	CanonicalURL    string
	Title           string
	H1              string
	Headings        []string
	Breadcrumbs     []string
	Links           []string
	Text            string
	FirstParagraph  string
	WordCount       int
	ParagraphCount  int
	LinkCount       int
	CodeBlockCount  int
	CodeRatio       float64
	LinkDensity     float64
	MetaDescription string
	HTTPStatus      int
}

type DocumentMetadata struct {
	Title          string   `json:"title"`
	URL            string   `json:"url"`
	NormalizedURL  string   `json:"normalized_url"`
	CanonicalURL   string   `json:"canonical_url"`
	Description    string   `json:"description"`
	H1             string   `json:"h1"`
	Breadcrumbs    []string `json:"breadcrumbs"`
	Headings       []string `json:"headings"`
	FirstParagraph string   `json:"first_paragraph"`
	WordCount      int      `json:"word_count"`
	ParagraphCount int      `json:"paragraph_count"`
	LinkCount      int      `json:"link_count"`
	CodeBlockCount int      `json:"code_block_count"`
	CodeRatio      float64  `json:"code_ratio"`
	LinkDensity    float64  `json:"link_density"`
	HTTPStatus     int      `json:"http_status"`
}

type URLInspection struct {
	Metadata  DocumentMetadata `json:"metadata"`
	GateInput map[string]any   `json:"gate_input"`
}

type URLDiscovery struct {
	BaseURL         string   `json:"base_url"`
	NormalizedURL   string   `json:"normalized_url"`
	DiscoveredCount int      `json:"discovered_count"`
	URLs            []string `json:"urls"`
}

type GateDebugResult struct {
	Request  map[string]any  `json:"request"`
	Response json.RawMessage `json:"response"`
	Review   Review          `json:"review"`
}

type candidate struct {
	document
	Classification   string
	Tags             []string
	Score            int
	ScoreComponents  []string
	EstimatedMinutes int
	Reason           string
	RejectReason     string
	Status           string
	Review           Review
}

type Review struct {
	Decision         string     `json:"decision"`
	Classification   string     `json:"classification"`
	Confidence       float64    `json:"confidence"`
	EstimatedMinutes int        `json:"estimated_minutes"`
	Rationale        string     `json:"rationale"`
	RejectReason     string     `json:"rejection_reason"`
	GateScore        int        `json:"-"`
	GatePageType     string     `json:"-"`
	RejectStage      string     `json:"-"`
	Model            string     `json:"-"`
	PromptVersion    string     `json:"-"`
	InputHash        string     `json:"-"`
	GateUsage        TokenUsage `json:"-"`
	EnrichmentUsage  TokenUsage `json:"-"`
}

type TokenUsage struct {
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	TotalTokens     int
}

type PageReviewer interface {
	ReviewPage(ctx context.Context, doc document) (Review, error)
}

func ProcessSubmission(ctx context.Context, conn *sql.DB, submissionID int64, opts Options) (Result, error) {
	opts = normalizeOptions(opts)
	if opts.Reviewer == nil {
		opts.Reviewer = reviewerFromEnv(opts.Client)
	}

	sub, err := loadSubmission(ctx, conn, submissionID)
	if err != nil {
		return Result{}, err
	}

	runID, err := startRun(ctx, conn, sub, opts)
	if err != nil {
		return Result{}, err
	}

	result := Result{SubmissionID: submissionID, PipelineRunID: runID}
	if err := markSubmissionProcessing(ctx, conn, submissionID, runID); err != nil {
		_ = failRun(ctx, conn, runID, result, err)
		return result, err
	}

	if err := process(ctx, conn, sub, runID, opts, &result); err != nil {
		_ = failRun(ctx, conn, runID, result, err)
		_ = markSubmissionFailed(ctx, conn, submissionID, err)
		return result, err
	}

	if err := completeRun(ctx, conn, runID, result); err != nil {
		return result, err
	}
	if err := markSubmissionReady(ctx, conn, submissionID); err != nil {
		return result, err
	}

	return result, nil
}

func ProcessSource(ctx context.Context, conn *sql.DB, sourceID int64, opts Options) (Result, error) {
	opts = normalizeOptions(opts)
	if opts.Reviewer == nil {
		opts.Reviewer = reviewerFromEnv(opts.Client)
	}

	sub, err := loadTopicSource(ctx, conn, sourceID)
	if err != nil {
		return Result{}, err
	}

	runID, err := startRun(ctx, conn, sub, opts)
	if err != nil {
		return Result{}, err
	}

	result := Result{SubmissionID: sub.ID, TopicSourceID: sourceID, PipelineRunID: runID}
	if err := process(ctx, conn, sub, runID, opts, &result); err != nil {
		_ = failRun(ctx, conn, runID, result, err)
		_ = markTopicSourceFailed(ctx, conn, sourceID, err)
		return result, err
	}

	if err := completeRun(ctx, conn, runID, result); err != nil {
		return result, err
	}
	if err := markTopicSourceProcessed(ctx, conn, sourceID); err != nil {
		return result, err
	}

	return result, nil
}

func InspectURL(ctx context.Context, rawURL string, opts Options) (URLInspection, error) {
	opts = normalizeOptions(opts)
	raw, err := fetchDocument(ctx, opts.Client, rawURL, opts.MaxBytes)
	if err != nil {
		return URLInspection{}, err
	}
	doc, err := extractDocument(raw)
	if err != nil {
		return URLInspection{}, err
	}
	return URLInspection{
		Metadata:  metadataFromDocument(doc),
		GateInput: gateInput(doc),
	}, nil
}

func DiscoverURL(ctx context.Context, rawURL string, opts Options) (URLDiscovery, error) {
	opts = normalizeOptions(opts)
	normalized, host, err := submission.NormalizeURL(rawURL)
	if err != nil {
		return URLDiscovery{}, err
	}
	sub := sourceSubmission{
		SubmittedURL:  rawURL,
		NormalizedURL: normalized,
		SourceHost:    host,
	}
	urls, err := discoverURLs(ctx, opts.Client, sub, opts)
	if err != nil {
		return URLDiscovery{}, err
	}
	return URLDiscovery{
		BaseURL:         rawURL,
		NormalizedURL:   normalized,
		DiscoveredCount: len(urls),
		URLs:            urls,
	}, nil
}

func GateURL(ctx context.Context, rawURL string, opts Options, includeRawResponse bool) (GateDebugResult, error) {
	opts = normalizeOptions(opts)
	raw, err := fetchDocument(ctx, opts.Client, rawURL, opts.MaxBytes)
	if err != nil {
		return GateDebugResult{}, err
	}
	doc, err := extractDocument(raw)
	if err != nil {
		return GateDebugResult{}, err
	}
	reviewer := openAIReviewerFromEnv(opts.Client)
	if reviewer.apiKey == "" {
		return GateDebugResult{}, errors.New("OPENAI_API_KEY is not set")
	}
	return reviewer.GatePage(ctx, doc, includeRawResponse)
}

func metadataFromDocument(doc document) DocumentMetadata {
	return DocumentMetadata{
		Title:          doc.Title,
		URL:            doc.URL,
		NormalizedURL:  doc.NormalizedURL,
		CanonicalURL:   doc.CanonicalURL,
		Description:    doc.MetaDescription,
		H1:             doc.H1,
		Breadcrumbs:    doc.Breadcrumbs,
		Headings:       doc.Headings,
		FirstParagraph: doc.FirstParagraph,
		WordCount:      doc.WordCount,
		ParagraphCount: doc.ParagraphCount,
		LinkCount:      doc.LinkCount,
		CodeBlockCount: doc.CodeBlockCount,
		CodeRatio:      doc.CodeRatio,
		LinkDensity:    doc.LinkDensity,
		HTTPStatus:     doc.HTTPStatus,
	}
}

func normalizeOptions(opts Options) Options {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.MaxPages < 1 {
		opts.MaxPages = DefaultMaxPages
	}
	if opts.MaxDepth < 1 {
		opts.MaxDepth = DefaultMaxDepth
	}
	if opts.MaxBytes < 1 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MinScore < 1 {
		opts.MinScore = DefaultMinScore
	}
	return opts
}

func process(ctx context.Context, conn *sql.DB, sub sourceSubmission, runID int64, opts Options, result *Result) error {
	urls, err := discoverURLs(ctx, opts.Client, sub, opts)
	if err != nil {
		return err
	}
	result.DiscoveredCount = len(urls)

	seenCanonical := map[string]struct{}{}
	for _, candidateURL := range urls {
		raw, err := fetchDocument(ctx, opts.Client, candidateURL, opts.MaxBytes)
		if err != nil {
			result.FailureCount++
			log.Printf("candidate fetch failed submission_id=%d url=%s error=%v", sub.ID, candidateURL, err)
			continue
		}
		result.CrawledCount++

		doc, err := extractDocument(raw)
		if err != nil {
			result.FailureCount++
			log.Printf("candidate extract failed submission_id=%d url=%s error=%v", sub.ID, candidateURL, err)
			continue
		}
		if !inScope(sub.NormalizedURL, doc.NormalizedURL) {
			result.RejectedCount++
			continue
		}
		if doc.CanonicalURL != "" {
			if _, exists := seenCanonical[doc.CanonicalURL]; exists {
				result.RejectedCount++
				continue
			}
			seenCanonical[doc.CanonicalURL] = struct{}{}
		}

		cand := buildCandidate(ctx, opts.Reviewer, sub, doc, opts.MinScore)
		if err := persistCandidate(ctx, conn, sub, runID, cand); err != nil {
			return err
		}
		switch cand.Status {
		case "eligible":
			result.EligibleCount++
		default:
			result.RejectedCount++
		}
	}

	return nil
}

func discoverURLs(ctx context.Context, client *http.Client, sub sourceSubmission, opts Options) ([]string, error) {
	type discoveryItem struct {
		URL      string
		Depth    int
		Priority int
		Sequence int
	}

	maxCandidates := DefaultMaxDiscovered
	if maxCandidates < opts.MaxPages {
		maxCandidates = opts.MaxPages
	}

	seen := map[string]discoveryItem{}
	queue := []discoveryItem{}
	sequence := 0
	add := func(raw string, depth int) (discoveryItem, bool) {
		normalized, _, err := submission.NormalizeURL(raw)
		if err != nil {
			return discoveryItem{}, false
		}
		if !isDiscoverableDocumentURL(normalized) {
			return discoveryItem{}, false
		}
		if !inScope(sub.NormalizedURL, normalized) {
			return discoveryItem{}, false
		}
		if _, exists := seen[normalized]; exists {
			return discoveryItem{}, false
		}
		if len(seen) >= maxCandidates {
			return discoveryItem{}, false
		}
		item := discoveryItem{
			URL:      normalized,
			Depth:    depth,
			Priority: discoveryPriority(sub.NormalizedURL, normalized),
			Sequence: sequence,
		}
		sequence++
		seen[normalized] = item
		if depth < opts.MaxDepth {
			return item, true
		}
		return discoveryItem{}, false
	}

	if item, ok := add(sub.NormalizedURL, 0); ok {
		queue = append(queue, item)
	}

	for _, sitemapURL := range sitemapURLs(ctx, client, sub.NormalizedURL) {
		for _, loc := range fetchSitemap(ctx, client, sitemapURL, opts.MaxBytes) {
			_, _ = add(loc, opts.MaxDepth)
		}
	}

	for len(queue) > 0 {
		if len(seen) >= maxCandidates {
			return nil, DiscoveryTooBroadError{
				BaseURL: sub.NormalizedURL,
				Count:   len(seen),
				Limit:   maxCandidates,
			}
		}
		sort.SliceStable(queue, func(i, j int) bool {
			if queue[i].Priority != queue[j].Priority {
				return queue[i].Priority < queue[j].Priority
			}
			if queue[i].Depth != queue[j].Depth {
				return queue[i].Depth < queue[j].Depth
			}
			return queue[i].Sequence < queue[j].Sequence
		})
		item := queue[0]
		queue = queue[1:]
		raw, err := fetchDocument(ctx, client, item.URL, opts.MaxBytes)
		if err != nil {
			log.Printf("discovery fetch failed submission_id=%d url=%s error=%v", sub.ID, item.URL, err)
			continue
		}
		for _, link := range extractLinks(raw.HTML, raw.FinalURL) {
			if child, ok := add(link, item.Depth+1); ok {
				queue = append(queue, child)
			}
		}
	}

	items := make([]discoveryItem, 0, len(seen))
	for _, item := range seen {
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		return items[i].URL < items[j].URL
	})
	if len(items) > opts.MaxPages {
		items = items[:opts.MaxPages]
	}
	urls := make([]string, 0, len(items))
	for _, item := range items {
		urls = append(urls, item.URL)
	}
	sort.Strings(urls)
	return urls, nil
}

func discoveryPriority(baseRaw string, candidateRaw string) int {
	base, _ := url.Parse(baseRaw)
	candidate, err := url.Parse(candidateRaw)
	if err != nil {
		return 100
	}
	if base != nil && strings.TrimRight(candidate.Path, "/") == strings.TrimRight(base.Path, "/") {
		return 0
	}

	path := strings.ToLower(candidate.Path)
	score := 50
	switch {
	case strings.Contains(path, "toc.html"),
		strings.Contains(path, "sidebar"),
		strings.Contains(path, "nav"),
		strings.HasSuffix(path, "/index.html"):
		score -= 30
	}
	switch {
	case strings.Contains(path, "/book/"),
		strings.Contains(path, "/tutorial"),
		strings.Contains(path, "/guide"),
		strings.Contains(path, "/learn"),
		strings.Contains(path, "/doc/"),
		strings.Contains(path, "/docs/"):
		score -= 20
	}
	switch {
	case strings.Contains(path, "/std/"),
		strings.Contains(path, "/core/"),
		strings.Contains(path, "/alloc/"),
		strings.Contains(path, "/src/"),
		strings.Contains(path, "/api/"):
		score += 60
	}
	if strings.Contains(path, "/reference/") {
		score += 20
	}
	for _, marker := range []string{"/struct.", "/trait.", "/enum.", "/fn.", "/macro.", "/type.", "/constant.", "/attr.", "/derive.", "/all.html"} {
		if strings.Contains(path, marker) {
			score += 80
			break
		}
	}
	for _, marker := range []string{"release", "changelog", "changes", "migration", "archive", "deprecated", "print.html"} {
		if strings.Contains(path, marker) {
			score += 80
			break
		}
	}
	if score < 0 {
		return 0
	}
	return score
}

func fetchDocument(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) (rawDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return rawDocument{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", defaultRequestAgent)

	resp, err := client.Do(req)
	if err != nil {
		return rawDocument{}, fmt.Errorf("request url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return rawDocument{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "text/html") && !strings.Contains(contentType, "xml") {
		return rawDocument{}, fmt.Errorf("unsupported content type %q", contentType)
	}

	limited := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return rawDocument{}, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return rawDocument{}, errors.New("response too large")
	}

	return rawDocument{
		URL:        rawURL,
		FinalURL:   resp.Request.URL.String(),
		HTML:       string(body),
		StatusCode: resp.StatusCode,
	}, nil
}

var (
	hrefPattern             = regexp.MustCompile(`(?is)<a[^>]+href\s*=\s*["']([^"']+)["']`)
	iframeSrcPattern        = regexp.MustCompile(`(?is)<iframe[^>]+src\s*=\s*["']([^"']+)["']`)
	titlePattern            = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	h1Pattern               = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	headingPattern          = regexp.MustCompile(`(?is)<h[1-3][^>]*>(.*?)</h[1-3]>`)
	paragraphPattern        = regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
	codePattern             = regexp.MustCompile(`(?is)<(pre|code)[^>]*>.*?</(pre|code)>`)
	breadcrumbPattern       = regexp.MustCompile(`(?is)<[^>]+(?:aria-label|class|id)\s*=\s*["'][^"']*breadcrumb[^"']*["'][^>]*>(.*?)</[^>]+>`)
	jsonLDBreadcrumbPattern = regexp.MustCompile(`(?is)<script[^>]+type\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script>`)
	jsonLDNamePattern       = regexp.MustCompile(`(?is)"name"\s*:\s*"([^"]+)"`)
	canonicalPattern        = regexp.MustCompile(`(?is)<link[^>]+rel\s*=\s*["'][^"']*canonical[^"']*["'][^>]+href\s*=\s*["']([^"']+)["']`)
	descriptionPattern      = regexp.MustCompile(`(?is)<meta[^>]+name\s*=\s*["']description["'][^>]+content\s*=\s*["']([^"']+)["']`)
	scriptPattern           = regexp.MustCompile(`(?is)<(script|style|nav|footer)[^>]*>.*?</(script|style|nav|footer)>`)
	tagPattern              = regexp.MustCompile(`(?is)<[^>]+>`)
	spacePattern            = regexp.MustCompile(`\s+`)
)

func extractLinks(body string, base string) []string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil
	}

	rawLinks := []string{}
	for _, pattern := range []*regexp.Regexp{hrefPattern, iframeSrcPattern} {
		for _, match := range pattern.FindAllStringSubmatch(body, -1) {
			if len(match) >= 2 {
				rawLinks = append(rawLinks, match[1])
			}
		}
	}

	links := make([]string, 0, len(rawLinks))
	for _, rawLink := range rawLinks {
		link, err := url.Parse(html.UnescapeString(rawLink))
		if err != nil {
			continue
		}
		links = append(links, baseURL.ResolveReference(link).String())
	}
	return links
}

func extractDocument(raw rawDocument) (document, error) {
	normalized, _, err := submission.NormalizeURL(raw.FinalURL)
	if err != nil {
		return document{}, err
	}

	title := cleanText(firstMatch(titlePattern, raw.HTML))
	h1 := cleanText(firstMatch(h1Pattern, raw.HTML))
	if title == "" {
		title = h1
	}
	if title == "" {
		return document{}, errors.New("missing title")
	}

	headings := []string{}
	for _, match := range headingPattern.FindAllStringSubmatch(raw.HTML, -1) {
		if len(match) < 2 {
			continue
		}
		heading := cleanText(match[1])
		if heading != "" {
			headings = append(headings, heading)
		}
	}

	canonical := ""
	if rawCanonical := firstMatch(canonicalPattern, raw.HTML); rawCanonical != "" {
		baseURL, _ := url.Parse(raw.FinalURL)
		canonicalURL, err := url.Parse(html.UnescapeString(rawCanonical))
		if err == nil {
			canonical, _, _ = submission.NormalizeURL(baseURL.ResolveReference(canonicalURL).String())
		}
	}

	text := extractText(raw.HTML)
	words := strings.Fields(text)
	links := extractLinks(raw.HTML, raw.FinalURL)
	linkDensity := 0.0
	if len(words) > 0 {
		linkDensity = float64(len(links)) / float64(len(words))
	}
	codeBytes := 0
	for _, match := range codePattern.FindAllString(raw.HTML, -1) {
		codeBytes += len(match)
	}
	codeRatio := 0.0
	if len(raw.HTML) > 0 {
		codeRatio = float64(codeBytes) / float64(len(raw.HTML))
	}

	return document{
		URL:             raw.FinalURL,
		NormalizedURL:   normalized,
		CanonicalURL:    canonical,
		Title:           title,
		H1:              h1,
		Headings:        headings,
		Breadcrumbs:     extractBreadcrumbs(raw.HTML),
		Links:           links,
		Text:            text,
		FirstParagraph:  firstParagraph(raw.HTML),
		WordCount:       len(words),
		ParagraphCount:  len(paragraphPattern.FindAllStringSubmatch(raw.HTML, -1)),
		LinkCount:       len(links),
		CodeBlockCount:  len(codePattern.FindAllStringSubmatch(raw.HTML, -1)),
		CodeRatio:       codeRatio,
		LinkDensity:     linkDensity,
		MetaDescription: cleanText(firstMatch(descriptionPattern, raw.HTML)),
		HTTPStatus:      raw.StatusCode,
	}, nil
}

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

func persistCandidate(ctx context.Context, conn *sql.DB, sub sourceSubmission, runID int64, cand candidate) error {
	headings, _ := json.Marshal(cand.Headings)
	links, _ := json.Marshal(cand.Links)
	tags, _ := json.Marshal(cand.Tags)
	components, _ := json.Marshal(cand.ScoreComponents)
	slug := slugify(firstNonEmpty(sub.SuggestedTopic, sub.SourceHost))
	if sub.TopicSlug != "" {
		slug = sub.TopicSlug
	}
	proposedName := sub.SuggestedTopic
	if sub.TopicName != "" {
		proposedName = sub.TopicName
	}

	excerpt := cand.Text
	if len(excerpt) > 500 {
		excerpt = excerpt[:500]
	}

	_, err := conn.ExecContext(ctx, `
		INSERT INTO page_candidates (
			documentation_submission_id,
			pipeline_run_id,
			topic_source_id,
			proposed_topic_slug,
			proposed_topic_name,
			topic_id,
			title,
			h1,
			url,
			normalized_url,
			canonical_url,
			source,
			http_status,
			extracted_excerpt,
			word_count,
			headings,
			meta_description,
			links,
			paragraph_count,
				link_count,
				code_block_count,
				link_density,
				gate_score,
				gate_page_type,
				reject_stage,
				primary_classification,
				classification_tags,
				classification_rules_version,
			score,
			score_components,
			official,
			estimated_minutes,
			reason,
			reject_reason,
			review_decision,
			review_confidence,
			review_model,
			review_prompt_version,
			review_input_hash,
			review_rationale,
			gate_input_tokens,
			gate_output_tokens,
			gate_reasoning_tokens,
			gate_total_tokens,
			enrichment_input_tokens,
			enrichment_output_tokens,
			enrichment_reasoning_tokens,
			enrichment_total_tokens,
			status
		)
		VALUES (
			:documentation_submission_id,
			:pipeline_run_id,
			:topic_source_id,
			:proposed_topic_slug,
			:proposed_topic_name,
			:topic_id,
			:title,
			:h1,
			:url,
			:normalized_url,
			:canonical_url,
			:source,
			:http_status,
			:extracted_excerpt,
			:word_count,
			:headings,
			:meta_description,
			:links,
			:paragraph_count,
			:link_count,
			:code_block_count,
			:link_density,
			:gate_score,
			:gate_page_type,
			:reject_stage,
			:primary_classification,
			:classification_tags,
			:classification_rules_version,
			:score,
			:score_components,
			1,
			:estimated_minutes,
			:reason,
			:reject_reason,
			:review_decision,
			:review_confidence,
			:review_model,
			:review_prompt_version,
			:review_input_hash,
			:review_rationale,
			:gate_input_tokens,
			:gate_output_tokens,
			:gate_reasoning_tokens,
			:gate_total_tokens,
			:enrichment_input_tokens,
			:enrichment_output_tokens,
			:enrichment_reasoning_tokens,
			:enrichment_total_tokens,
			:status
		)
		ON CONFLICT(documentation_submission_id, normalized_url) DO UPDATE SET
			pipeline_run_id = excluded.pipeline_run_id,
			topic_source_id = excluded.topic_source_id,
			proposed_topic_slug = excluded.proposed_topic_slug,
			proposed_topic_name = excluded.proposed_topic_name,
			topic_id = excluded.topic_id,
			title = excluded.title,
			h1 = excluded.h1,
			url = excluded.url,
			canonical_url = excluded.canonical_url,
			source = excluded.source,
			http_status = excluded.http_status,
			extracted_excerpt = excluded.extracted_excerpt,
			word_count = excluded.word_count,
			headings = excluded.headings,
			meta_description = excluded.meta_description,
			links = excluded.links,
			paragraph_count = excluded.paragraph_count,
				link_count = excluded.link_count,
				code_block_count = excluded.code_block_count,
				link_density = excluded.link_density,
				gate_score = excluded.gate_score,
				gate_page_type = excluded.gate_page_type,
				reject_stage = excluded.reject_stage,
				primary_classification = excluded.primary_classification,
			classification_tags = excluded.classification_tags,
			classification_rules_version = excluded.classification_rules_version,
			score = excluded.score,
			score_components = excluded.score_components,
			official = excluded.official,
			estimated_minutes = excluded.estimated_minutes,
			reason = excluded.reason,
			reject_reason = excluded.reject_reason,
			review_decision = excluded.review_decision,
			review_confidence = excluded.review_confidence,
			review_model = excluded.review_model,
			review_prompt_version = excluded.review_prompt_version,
			review_input_hash = excluded.review_input_hash,
			review_rationale = excluded.review_rationale,
			gate_input_tokens = excluded.gate_input_tokens,
			gate_output_tokens = excluded.gate_output_tokens,
			gate_reasoning_tokens = excluded.gate_reasoning_tokens,
			gate_total_tokens = excluded.gate_total_tokens,
			enrichment_input_tokens = excluded.enrichment_input_tokens,
			enrichment_output_tokens = excluded.enrichment_output_tokens,
			enrichment_reasoning_tokens = excluded.enrichment_reasoning_tokens,
			enrichment_total_tokens = excluded.enrichment_total_tokens,
			status = excluded.status
	`,
		sql.Named("documentation_submission_id", sub.ID),
		sql.Named("pipeline_run_id", runID),
		sql.Named("topic_source_id", nullableID(sub.TopicSourceID)),
		sql.Named("proposed_topic_slug", slug),
		sql.Named("proposed_topic_name", proposedName),
		sql.Named("topic_id", nullableID(sub.TopicID)),
		sql.Named("title", cand.Title),
		sql.Named("h1", cand.H1),
		sql.Named("url", cand.URL),
		sql.Named("normalized_url", cand.NormalizedURL),
		sql.Named("canonical_url", cand.CanonicalURL),
		sql.Named("source", sub.SourceHost),
		sql.Named("http_status", cand.HTTPStatus),
		sql.Named("extracted_excerpt", excerpt),
		sql.Named("word_count", cand.WordCount),
		sql.Named("headings", string(headings)),
		sql.Named("meta_description", cand.MetaDescription),
		sql.Named("links", string(links)),
		sql.Named("paragraph_count", cand.ParagraphCount),
		sql.Named("link_count", cand.LinkCount),
		sql.Named("code_block_count", cand.CodeBlockCount),
		sql.Named("link_density", cand.LinkDensity),
		sql.Named("gate_score", cand.Review.GateScore),
		sql.Named("gate_page_type", cand.Review.GatePageType),
		sql.Named("reject_stage", cand.Review.RejectStage),
		sql.Named("primary_classification", cand.Classification),
		sql.Named("classification_tags", string(tags)),
		sql.Named("classification_rules_version", rulesVersion),
		sql.Named("score", cand.Score),
		sql.Named("score_components", string(components)),
		sql.Named("estimated_minutes", cand.EstimatedMinutes),
		sql.Named("reason", cand.Reason),
		sql.Named("reject_reason", cand.RejectReason),
		sql.Named("review_decision", cand.Review.Decision),
		sql.Named("review_confidence", cand.Review.Confidence),
		sql.Named("review_model", cand.Review.Model),
		sql.Named("review_prompt_version", cand.Review.PromptVersion),
		sql.Named("review_input_hash", cand.Review.InputHash),
		sql.Named("review_rationale", cand.Review.Rationale),
		sql.Named("gate_input_tokens", cand.Review.GateUsage.InputTokens),
		sql.Named("gate_output_tokens", cand.Review.GateUsage.OutputTokens),
		sql.Named("gate_reasoning_tokens", cand.Review.GateUsage.ReasoningTokens),
		sql.Named("gate_total_tokens", cand.Review.GateUsage.TotalTokens),
		sql.Named("enrichment_input_tokens", cand.Review.EnrichmentUsage.InputTokens),
		sql.Named("enrichment_output_tokens", cand.Review.EnrichmentUsage.OutputTokens),
		sql.Named("enrichment_reasoning_tokens", cand.Review.EnrichmentUsage.ReasoningTokens),
		sql.Named("enrichment_total_tokens", cand.Review.EnrichmentUsage.TotalTokens),
		sql.Named("status", cand.Status),
	)
	if err != nil {
		return fmt.Errorf("persist page candidate: %w", err)
	}
	return nil
}

func loadSubmission(ctx context.Context, conn *sql.DB, id int64) (sourceSubmission, error) {
	var sub sourceSubmission
	err := conn.QueryRowContext(ctx, `
		SELECT id, submitted_url, normalized_url, source_host, suggested_topic
		FROM documentation_submissions
		WHERE id = ?
	`, id).Scan(&sub.ID, &sub.SubmittedURL, &sub.NormalizedURL, &sub.SourceHost, &sub.SuggestedTopic)
	if err != nil {
		return sourceSubmission{}, fmt.Errorf("load submission: %w", err)
	}
	return sub, nil
}

func loadTopicSource(ctx context.Context, conn *sql.DB, id int64) (sourceSubmission, error) {
	var sub sourceSubmission
	err := conn.QueryRowContext(ctx, `
		SELECT
			ts.id,
			COALESCE(ts.created_from_submission_id, 0),
			ts.topic_id,
			t.slug,
			t.name,
			ts.base_url,
			ts.normalized_url,
			ts.source_host
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE ts.id = ?
			AND ts.status = 'active'
	`, id).Scan(&sub.TopicSourceID, &sub.ID, &sub.TopicID, &sub.TopicSlug, &sub.TopicName, &sub.SubmittedURL, &sub.NormalizedURL, &sub.SourceHost)
	if err != nil {
		return sourceSubmission{}, fmt.Errorf("load topic source: %w", err)
	}
	if sub.ID < 1 {
		return sourceSubmission{}, errors.New("topic source is missing created_from_submission_id")
	}
	sub.SuggestedTopic = sub.TopicName
	sub.ProcessAsSource = true
	return sub, nil
}

func startRun(ctx context.Context, conn *sql.DB, sub sourceSubmission, opts Options) (int64, error) {
	policy, _ := json.Marshal(map[string]any{
		"max_pages": opts.MaxPages,
		"max_depth": opts.MaxDepth,
		"max_bytes": opts.MaxBytes,
		"min_score": opts.MinScore,
	})
	result, err := conn.ExecContext(ctx, `
		INSERT INTO pipeline_runs (documentation_submission_id, topic_source_id, status, crawl_policy)
		VALUES (?, ?, 'running', ?)
	`, sub.ID, nullableID(sub.TopicSourceID), string(policy))
	if err != nil {
		return 0, fmt.Errorf("start pipeline run: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read pipeline run id: %w", err)
	}
	return id, nil
}

func nullableID(id int64) any {
	if id < 1 {
		return nil
	}
	return id
}

func markSubmissionProcessing(ctx context.Context, conn *sql.DB, submissionID int64, runID int64) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE documentation_submissions
		SET status = 'processing',
			latest_pipeline_run_id = ?,
			attempt_count = attempt_count + 1,
			last_attempt_at = datetime('now'),
			last_error = ''
		WHERE id = ?
	`, runID, submissionID)
	return err
}

func markSubmissionReady(ctx context.Context, conn *sql.DB, submissionID int64) error {
	_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'candidates_ready', last_error = '' WHERE id = ?", submissionID)
	return err
}

func markSubmissionFailed(ctx context.Context, conn *sql.DB, submissionID int64, runErr error) error {
	_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'failed', last_error = ? WHERE id = ?", runErr.Error(), submissionID)
	return err
}

func markTopicSourceProcessed(ctx context.Context, conn *sql.DB, sourceID int64) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE topic_sources
		SET last_processed_at = datetime('now'),
			last_error = '',
			updated_at = datetime('now')
		WHERE id = ?
	`, sourceID)
	return err
}

func markTopicSourceFailed(ctx context.Context, conn *sql.DB, sourceID int64, runErr error) error {
	status := "active"
	var tooBroad DiscoveryTooBroadError
	if errors.As(runErr, &tooBroad) {
		status = "needs_scope"
	}
	_, err := conn.ExecContext(ctx, `
		UPDATE topic_sources
		SET status = ?,
			last_error = ?,
			updated_at = datetime('now')
		WHERE id = ?
	`, status, runErr.Error(), sourceID)
	return err
}

func completeRun(ctx context.Context, conn *sql.DB, runID int64, result Result) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE pipeline_runs
		SET status = 'completed',
			completed_at = datetime('now'),
			discovered_count = ?,
			crawled_count = ?,
			eligible_count = ?,
			rejected_count = ?,
			failure_count = ?
		WHERE id = ?
	`, result.DiscoveredCount, result.CrawledCount, result.EligibleCount, result.RejectedCount, result.FailureCount, runID)
	return err
}

func failRun(ctx context.Context, conn *sql.DB, runID int64, result Result, runErr error) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE pipeline_runs
		SET status = 'failed',
			completed_at = datetime('now'),
			discovered_count = ?,
			crawled_count = ?,
			eligible_count = ?,
			rejected_count = ?,
			failure_count = ?,
			error = ?
		WHERE id = ?
	`, result.DiscoveredCount, result.CrawledCount, result.EligibleCount, result.RejectedCount, result.FailureCount, runErr.Error(), runID)
	return err
}

type sitemap struct {
	URLs []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

func sitemapURLs(ctx context.Context, client *http.Client, raw string) []string {
	base, err := url.Parse(raw)
	if err != nil {
		return nil
	}

	var urls []string
	robots := *base
	robots.Path = "/robots.txt"
	robots.RawQuery = ""
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, robots.String(), nil); err == nil {
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
			_ = resp.Body.Close()
			for _, line := range strings.Split(string(body), "\n") {
				key, value, ok := strings.Cut(line, ":")
				if ok && strings.EqualFold(strings.TrimSpace(key), "sitemap") {
					urls = append(urls, strings.TrimSpace(value))
				}
			}
		}
	}

	defaultSitemap := *base
	defaultSitemap.Path = "/sitemap.xml"
	defaultSitemap.RawQuery = ""
	urls = append(urls, defaultSitemap.String())
	return urls
}

func fetchSitemap(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) []string {
	return fetchSitemapDepth(ctx, client, rawURL, maxBytes, 0, map[string]struct{}{})
}

func fetchSitemapDepth(ctx context.Context, client *http.Client, rawURL string, maxBytes int64, depth int, seen map[string]struct{}) []string {
	if depth > 2 {
		return nil
	}
	normalized, _, err := submission.NormalizeURL(rawURL)
	if err != nil {
		normalized = rawURL
	}
	if _, exists := seen[normalized]; exists {
		return nil
	}
	seen[normalized] = struct{}{}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil
	}
	var parsed sitemap
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return sitemapTextURLs(string(body))
	}
	locs := make([]string, 0, len(parsed.URLs))
	for _, entry := range parsed.URLs {
		locs = append(locs, strings.TrimSpace(entry.Loc))
	}
	for _, entry := range parsed.Sitemaps {
		child := strings.TrimSpace(entry.Loc)
		if child == "" {
			continue
		}
		locs = append(locs, fetchSitemapDepth(ctx, client, child, maxBytes, depth+1, seen)...)
	}
	return locs
}

func sitemapTextURLs(body string) []string {
	urls := []string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			urls = append(urls, line)
		}
	}
	return urls
}

func isDiscoverableDocumentURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	switch strings.ToLower(path.Ext(parsed.Path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".ico", ".css", ".js", ".mjs", ".map", ".pdf", ".zip", ".tar", ".gz", ".tgz", ".mp4", ".webm", ".mp3", ".woff", ".woff2", ".ttf", ".eot":
		return false
	default:
		return true
	}
}

func inScope(baseRaw string, candidateRaw string) bool {
	base, err := url.Parse(baseRaw)
	if err != nil {
		return false
	}
	candidate, err := url.Parse(candidateRaw)
	if err != nil {
		return false
	}
	if !strings.EqualFold(base.Host, candidate.Host) {
		return false
	}
	prefix := scopePath(base.Path)
	if prefix == "" || prefix == "/" {
		return true
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return candidate.Path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(candidate.Path, prefix)
}

func scopePath(rawPath string) string {
	if rawPath == "" || rawPath == "/" {
		return rawPath
	}
	if strings.HasSuffix(rawPath, "/") {
		return rawPath
	}
	if path.Ext(rawPath) != "" {
		dir := path.Dir(rawPath)
		if dir == "." {
			return "/"
		}
		return dir
	}
	return rawPath
}

func firstMatch(pattern *regexp.Regexp, value string) string {
	match := pattern.FindStringSubmatch(value)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func cleanText(value string) string {
	value = tagPattern.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	value = strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
	return value
}

func extractText(body string) string {
	body = scriptPattern.ReplaceAllString(body, " ")
	body = tagPattern.ReplaceAllString(body, " ")
	body = html.UnescapeString(body)
	return strings.TrimSpace(spacePattern.ReplaceAllString(body, " "))
}

func firstParagraph(body string) string {
	for _, match := range paragraphPattern.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		value := cleanText(match[1])
		if value != "" {
			return value
		}
	}
	return ""
}

func extractBreadcrumbs(body string) []string {
	values := []string{}
	for _, match := range breadcrumbPattern.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		for _, crumb := range strings.Split(cleanText(match[1]), ">") {
			crumb = strings.TrimSpace(crumb)
			if crumb != "" {
				values = append(values, crumb)
			}
		}
	}
	for _, block := range jsonLDBreadcrumbPattern.FindAllStringSubmatch(body, -1) {
		if len(block) < 2 || !strings.Contains(strings.ToLower(block[1]), "breadcrumblist") {
			continue
		}
		for _, match := range jsonLDNamePattern.FindAllStringSubmatch(block[1], -1) {
			if len(match) < 2 {
				continue
			}
			value := strings.TrimSpace(html.UnescapeString(match[1]))
			if value != "" {
				values = append(values, value)
			}
		}
	}
	return uniqueStrings(values, 10)
}

func uniqueStrings(values []string, limit int) []string {
	seen := map[string]struct{}{}
	unique := []string{}
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, strings.TrimSpace(value))
		if limit > 0 && len(unique) >= limit {
			break
		}
	}
	return unique
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastDash = false
		case !lastDash:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
