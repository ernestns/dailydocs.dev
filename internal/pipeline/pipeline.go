package pipeline

import (
	"context"
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
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ernestns/daily-docs/internal/submission"
)

const (
	DefaultMaxPages     = 250
	DefaultMaxBytes     = 2 * 1024 * 1024
	DefaultMinScore     = 70
	rulesVersion        = "heuristic-v1"
	defaultRequestAgent = "DailyDocs/0.1"
)

type Options struct {
	Client   *http.Client
	MaxPages int
	MaxBytes int64
	MinScore int
}

type Result struct {
	SubmissionID    int64
	PipelineRunID   int64
	DiscoveredCount int
	CrawledCount    int
	EligibleCount   int
	RejectedCount   int
	FailureCount    int
}

type sourceSubmission struct {
	ID             int64
	SubmittedURL   string
	NormalizedURL  string
	SourceHost     string
	SuggestedTopic string
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
	Text            string
	WordCount       int
	MetaDescription string
	HTTPStatus      int
}

type candidate struct {
	document
	Classification   string
	Tags             []string
	Score            int
	ScoreComponents  []string
	EstimatedMinutes int
	Reason           string
}

func ProcessSubmission(ctx context.Context, conn *sql.DB, submissionID int64, opts Options) (Result, error) {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.MaxPages < 1 {
		opts.MaxPages = DefaultMaxPages
	}
	if opts.MaxBytes < 1 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MinScore < 1 {
		opts.MinScore = DefaultMinScore
	}

	sub, err := loadSubmission(ctx, conn, submissionID)
	if err != nil {
		return Result{}, err
	}

	runID, err := startRun(ctx, conn, submissionID, opts)
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

		cand, eligible := buildCandidate(sub, doc, opts.MinScore)
		if !eligible {
			result.RejectedCount++
			continue
		}
		if err := persistCandidate(ctx, conn, sub, runID, cand); err != nil {
			return err
		}
		result.EligibleCount++
	}

	return nil
}

func discoverURLs(ctx context.Context, client *http.Client, sub sourceSubmission, opts Options) ([]string, error) {
	seen := map[string]struct{}{}
	add := func(raw string) {
		normalized, _, err := submission.NormalizeURL(raw)
		if err != nil {
			return
		}
		if !inScope(sub.NormalizedURL, normalized) {
			return
		}
		if _, exists := seen[normalized]; exists {
			return
		}
		if len(seen) >= opts.MaxPages {
			return
		}
		seen[normalized] = struct{}{}
	}

	add(sub.NormalizedURL)

	if raw, err := fetchDocument(ctx, client, sub.NormalizedURL, opts.MaxBytes); err == nil {
		for _, link := range extractLinks(raw.HTML, raw.FinalURL) {
			add(link)
		}
	}

	for _, sitemapURL := range sitemapURLs(ctx, client, sub.NormalizedURL) {
		for _, loc := range fetchSitemap(ctx, client, sitemapURL, opts.MaxBytes) {
			add(loc)
		}
	}

	urls := make([]string, 0, len(seen))
	for u := range seen {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	if len(urls) > opts.MaxPages {
		urls = urls[:opts.MaxPages]
	}
	return urls, nil
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
	hrefPattern        = regexp.MustCompile(`(?is)<a[^>]+href\s*=\s*["']([^"']+)["']`)
	titlePattern       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	h1Pattern          = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	headingPattern     = regexp.MustCompile(`(?is)<h[1-3][^>]*>(.*?)</h[1-3]>`)
	canonicalPattern   = regexp.MustCompile(`(?is)<link[^>]+rel\s*=\s*["'][^"']*canonical[^"']*["'][^>]+href\s*=\s*["']([^"']+)["']`)
	descriptionPattern = regexp.MustCompile(`(?is)<meta[^>]+name\s*=\s*["']description["'][^>]+content\s*=\s*["']([^"']+)["']`)
	scriptPattern      = regexp.MustCompile(`(?is)<(script|style|nav|footer)[^>]*>.*?</(script|style|nav|footer)>`)
	tagPattern         = regexp.MustCompile(`(?is)<[^>]+>`)
	spacePattern       = regexp.MustCompile(`\s+`)
)

func extractLinks(body string, base string) []string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil
	}

	matches := hrefPattern.FindAllStringSubmatch(body, -1)
	links := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		link, err := url.Parse(html.UnescapeString(match[1]))
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

	return document{
		URL:             raw.FinalURL,
		NormalizedURL:   normalized,
		CanonicalURL:    canonical,
		Title:           title,
		H1:              h1,
		Headings:        headings,
		Text:            text,
		WordCount:       len(words),
		MetaDescription: cleanText(firstMatch(descriptionPattern, raw.HTML)),
		HTTPStatus:      raw.StatusCode,
	}, nil
}

func buildCandidate(sub sourceSubmission, doc document, minScore int) (candidate, bool) {
	classification, tags := classify(doc)
	score, components := scoreDocument(doc, classification)
	if score < minScore {
		return candidate{}, false
	}

	reason := strings.Join(components, "; ")
	estimated := int(math.Ceil(float64(doc.WordCount) / 200.0))
	if estimated < 1 {
		estimated = 1
	}

	return candidate{
		document:         doc,
		Classification:   classification,
		Tags:             tags,
		Score:            score,
		ScoreComponents:  components,
		EstimatedMinutes: estimated,
		Reason:           reason,
	}, true
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
	case strings.Contains(value, "api") || strings.Contains(value, "/reference/"):
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
		score -= 20
		components = append(components, "-20 api reference")
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
	return score, components
}

func persistCandidate(ctx context.Context, conn *sql.DB, sub sourceSubmission, runID int64, cand candidate) error {
	headings, _ := json.Marshal(cand.Headings)
	tags, _ := json.Marshal(cand.Tags)
	components, _ := json.Marshal(cand.ScoreComponents)
	slug := slugify(firstNonEmpty(sub.SuggestedTopic, sub.SourceHost))

	excerpt := cand.Text
	if len(excerpt) > 500 {
		excerpt = excerpt[:500]
	}

	_, err := conn.ExecContext(ctx, `
		INSERT INTO page_candidates (
			documentation_submission_id,
			pipeline_run_id,
			proposed_topic_slug,
			proposed_topic_name,
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
			primary_classification,
			classification_tags,
			classification_rules_version,
			score,
			score_components,
			official,
			estimated_minutes,
			reason,
			status
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 'eligible')
		ON CONFLICT(documentation_submission_id, normalized_url) DO UPDATE SET
			pipeline_run_id = excluded.pipeline_run_id,
			proposed_topic_slug = excluded.proposed_topic_slug,
			proposed_topic_name = excluded.proposed_topic_name,
			title = excluded.title,
			h1 = excluded.h1,
			url = excluded.url,
			canonical_url = excluded.canonical_url,
			source = excluded.source,
			http_status = excluded.http_status,
			extracted_excerpt = excluded.extracted_excerpt,
			word_count = excluded.word_count,
			headings = excluded.headings,
			primary_classification = excluded.primary_classification,
			classification_tags = excluded.classification_tags,
			classification_rules_version = excluded.classification_rules_version,
			score = excluded.score,
			score_components = excluded.score_components,
			official = excluded.official,
			estimated_minutes = excluded.estimated_minutes,
			reason = excluded.reason,
			status = 'eligible'
	`, sub.ID, runID, slug, sub.SuggestedTopic, cand.Title, cand.H1, cand.URL, cand.NormalizedURL, cand.CanonicalURL, sub.SourceHost, cand.HTTPStatus, excerpt, cand.WordCount, string(headings), cand.Classification, string(tags), rulesVersion, cand.Score, string(components), cand.EstimatedMinutes, cand.Reason)
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

func startRun(ctx context.Context, conn *sql.DB, submissionID int64, opts Options) (int64, error) {
	policy, _ := json.Marshal(map[string]any{
		"max_pages": opts.MaxPages,
		"max_bytes": opts.MaxBytes,
		"min_score": opts.MinScore,
	})
	result, err := conn.ExecContext(ctx, `
		INSERT INTO pipeline_runs (documentation_submission_id, status, crawl_policy)
		VALUES (?, 'running', ?)
	`, submissionID, string(policy))
	if err != nil {
		return 0, fmt.Errorf("start pipeline run: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read pipeline run id: %w", err)
	}
	return id, nil
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
		return nil
	}
	locs := make([]string, 0, len(parsed.URLs))
	for _, entry := range parsed.URLs {
		locs = append(locs, strings.TrimSpace(entry.Loc))
	}
	return locs
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
	prefix := base.Path
	if prefix == "" || prefix == "/" {
		return true
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return candidate.Path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(candidate.Path, prefix)
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
