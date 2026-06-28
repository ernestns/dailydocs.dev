package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

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
