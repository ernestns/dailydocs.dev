package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
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

	if err := markTopicSourceProcessing(ctx, conn, sourceID); err != nil {
		_ = failRun(ctx, conn, runID, Result{SubmissionID: sub.ID, TopicSourceID: sourceID, PipelineRunID: runID}, err)
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
