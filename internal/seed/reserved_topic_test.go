package seed

import (
	"context"
	"strings"
	"testing"
)

func TestImportRejectsApplicationRoutesBeforeWriting(t *testing.T) {
	for _, slug := range []string{"health", "read", "topics", "process-topic"} {
		t.Run(slug, func(t *testing.T) {
			ctx := context.Background()
			conn := openTestDB(t, ctx)
			defer conn.Close()
			_, err := ImportTopic(ctx, conn, TopicFile{
				Topic: slug, Name: slug,
				Pages: []PageFile{{Title: "Useful reading", URL: "https://docs.example.org/reading"}},
			})
			if err == nil || !strings.Contains(err.Error(), "reserved for an application route") || !strings.Contains(err.Error(), slug+"-topic") {
				t.Fatal("missing actionable reserved-slug error", err)
			}
			for _, table := range []string{"topics", "pages", "imports", "daily_readings"} {
				var count int
				if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("invalid import changed data", table, count, err)
				}
			}
		})
	}
}
