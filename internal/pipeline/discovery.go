package pipeline

import (
	"context"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/ernestns/daily-docs/internal/submission"
)

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
