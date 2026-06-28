package submission

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ernestns/daily-docs/internal/db"
)

func TestNormalizeURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		normalized string
		host       string
	}{
		{
			name:       "lowercases host and removes fragment",
			input:      "https://SQLite.org/docs.html#top",
			normalized: "https://sqlite.org/docs.html",
			host:       "sqlite.org",
		},
		{
			name:       "removes default port and trailing slash",
			input:      "https://example.com:443/docs/",
			normalized: "https://example.com/docs",
			host:       "example.com",
		},
		{
			name:       "drops query string",
			input:      "https://go.dev/doc/?utm_source=test",
			normalized: "https://go.dev/doc",
			host:       "go.dev",
		},
		{
			name:       "keeps non-default port",
			input:      "http://localhost:8080/docs",
			normalized: "http://localhost:8080/docs",
			host:       "localhost:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, host, err := NormalizeURL(tt.input)
			if err != nil {
				t.Fatalf("normalize url: %v", err)
			}
			if normalized != tt.normalized {
				t.Fatalf("expected normalized %q, got %q", tt.normalized, normalized)
			}
			if host != tt.host {
				t.Fatalf("expected host %q, got %q", tt.host, host)
			}
		})
	}
}

func TestNormalizeURLRejectsInvalidURLs(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"",
		"ftp://example.com/docs",
		"https:///docs",
		"https://user@example.com/docs",
	} {
		_, _, err := NormalizeURL(input)
		if !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("expected ErrInvalidURL for %q, got %v", input, err)
		}
	}
}

func TestCreateDeduplicatesNormalizedURL(t *testing.T) {
	ctx := context.Background()
	conn := openSubmissionTestDB(t, ctx)
	defer conn.Close()

	first, err := Create(ctx, conn, CreateInput{
		URL:            "https://SQLite.org/docs.html#top",
		SuggestedTopic: "SQLite",
		SubmitterIP:    "203.0.113.1",
		IPHashSalt:     "test",
	})
	if err != nil {
		t.Fatalf("create first submission: %v", err)
	}

	second, err := Create(ctx, conn, CreateInput{
		URL:            "https://sqlite.org/docs.html",
		SuggestedTopic: "Ignored",
		SubmitterIP:    "203.0.113.1",
		IPHashSalt:     "test",
	})
	if err != nil {
		t.Fatalf("create duplicate submission: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("expected duplicate to return same id, got %d and %d", first.ID, second.ID)
	}
	if second.RequestCount != 2 {
		t.Fatalf("expected request count 2, got %d", second.RequestCount)
	}
	if second.SuggestedTopic != "SQLite" {
		t.Fatalf("expected original suggested topic to be preserved, got %q", second.SuggestedTopic)
	}
	if second.SubmitterIPHash == "" {
		t.Fatal("expected submitter IP hash")
	}
}

func TestListPublicExcludesHiddenSubmissions(t *testing.T) {
	ctx := context.Background()
	conn := openSubmissionTestDB(t, ctx)
	defer conn.Close()

	visible, err := Create(ctx, conn, CreateInput{URL: "https://go.dev/doc/", SuggestedTopic: "Go"})
	if err != nil {
		t.Fatalf("create visible submission: %v", err)
	}
	hidden, err := Create(ctx, conn, CreateInput{URL: "https://example.com/docs", SuggestedTopic: "Example"})
	if err != nil {
		t.Fatalf("create hidden submission: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET visibility = 'hidden' WHERE id = ?", hidden.ID); err != nil {
		t.Fatalf("hide submission: %v", err)
	}

	submissions, err := ListPublic(ctx, conn, 50)
	if err != nil {
		t.Fatalf("list public submissions: %v", err)
	}
	if len(submissions) != 1 {
		t.Fatalf("expected one public submission, got %d", len(submissions))
	}
	if submissions[0].ID != visible.ID {
		t.Fatalf("expected visible submission %d, got %d", visible.ID, submissions[0].ID)
	}
}

func TestPublicStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sub  Submission
		want string
	}{
		{
			name: "pending submission",
			sub:  Submission{Status: "pending"},
			want: "Submitted",
		},
		{
			name: "ready source",
			sub:  Submission{Status: "active", SourceStatus: "ready_to_process"},
			want: "Discovered",
		},
		{
			name: "needs scope",
			sub:  Submission{Status: "active", SourceStatus: "needs_scope"},
			want: "Needs narrower URL",
		},
		{
			name: "candidates ready",
			sub:  Submission{Status: "active", SourceStatus: "candidates_ready"},
			want: "Ready for review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sub.PublicStatus(); got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func openSubmissionTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	conn, err := db.Open(ctx, filepath.Join(t.TempDir(), "dailydocs.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return conn
}
