package pipeline

import (
	"context"
	"errors"

	"github.com/ernestns/daily-docs/internal/submission"
)

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
