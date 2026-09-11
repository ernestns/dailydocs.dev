// Package topicname keeps topic input hygiene and URL identity consistent across entry points.
package topicname

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrInvalid = errors.New("enter a topic name, not a URL, path, or instruction")

var slugPattern = regexp.MustCompile(`^[\pL\pN][\pL\pN-]*$`)

func IsSlug(value string) bool { return slugPattern.MatchString(value) }

// Validate rejects clear non-topic syntax, not unfamiliar subjects or short names.
// Semantic uncertainty belongs to the planner; a provider failure proves nothing about validity.
func Validate(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 200 || !utf8.ValidString(value) {
		return ErrInvalid
	}
	lower := strings.ToLower(value)
	if len(lower) >= 3 && lower[0] >= 'a' && lower[0] <= 'z' && lower[1] == ':' && (lower[2] == '\\' || lower[2] == '/') {
		return ErrInvalid
	}
	if strings.Contains(lower, "://") || strings.HasPrefix(value, "<") {
		return ErrInvalid
	}
	for _, prefix := range []string{"/", "\\", "./", "../", "~/", "ignore previous instructions", "ignore all instructions"} {
		if strings.HasPrefix(lower, prefix) {
			return ErrInvalid
		}
	}
	meaningful := false
	for _, r := range value {
		if unicode.IsControl(r) {
			return ErrInvalid
		}
		meaningful = meaningful || unicode.IsLetter(r) || unicode.IsNumber(r)
	}
	if !meaningful {
		return ErrInvalid
	}
	return nil
}

func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(value, ".") {
		value = "dot-" + strings.TrimPrefix(value, ".")
	}
	value = strings.NewReplacer("+", "-plus-", "#", "-sharp-").Replace(value)
	var out strings.Builder
	dash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			out.WriteRune(r)
			dash = false
		} else if out.Len() > 0 && !dash {
			out.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func Display(value string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		runes := []rune(part)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
		}
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}
