package pipeline

import (
	"errors"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/ernestns/daily-docs/internal/submission"
)

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
