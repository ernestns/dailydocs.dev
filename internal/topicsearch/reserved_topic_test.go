package topicsearch

import (
	"context"
	"fmt"
	"testing"

	"github.com/ernestns/daily-docs/internal/topicname"
)

func TestReservedTopicDoesNotCaptureOccupiedFallbackNames(t *testing.T) {
	for _, requested := range []string{"Health", "Read", "Topics", "Process Topic"} {
		for _, fallbackOccupants := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d_fallbacks", requested, fallbackOccupants), func(t *testing.T) {
				ctx := context.Background()
				conn := openTopicSearchTestDB(t, ctx)
				defer conn.Close()
				conn.SetMaxOpenConns(1)
				stored := map[string]string{}
				store := func(name string) string {
					t.Helper()
					slug, actualName, err := ResolveTopic(ctx, conn, name)
					if err != nil || actualName != name {
						t.Fatal("could not resolve distinct subject", name, slug, actualName, err)
					}
					if _, exists := stored[slug]; exists {
						t.Fatal("distinct subject reused occupied slug", name, slug)
					}
					if _, err := conn.Exec("INSERT INTO topics(slug,name) VALUES(?,?)", slug, name); err != nil {
						t.Fatal(err)
					}
					stored[slug] = name
					return slug
				}
				store(requested + " Topic")
				for range fallbackOccupants {
					candidate, _, err := ResolveTopic(ctx, conn, requested)
					if err != nil {
						t.Fatal(err)
					}
					if _, exists := stored[candidate]; exists {
						t.Fatal("resolver selected a fallback already owned by another subject", requested, candidate)
					}
					// A valid human topic can spell the generated fallback literally.
					if got := store(topicname.Display(candidate)); got != candidate {
						t.Fatal("fixture did not occupy the generated fallback", got, candidate)
					}
				}
				if _, err := conn.Exec("PRAGMA query_only=ON"); err != nil {
					t.Fatal(err)
				}
				slug, name, err := ResolveTopic(ctx, conn, requested)
				if err != nil || name != requested {
					t.Fatal("read-only resolution failed", slug, name, err)
				}
				if owner, exists := stored[slug]; exists {
					t.Fatalf("%q captured fallback %q belonging to %q", requested, slug, owner)
				}
				if repeated, _, err := ResolveTopic(ctx, conn, requested); err != nil || repeated != slug {
					t.Fatal("unwritten resolution was not deterministic", repeated, slug, err)
				}
				if _, err := conn.Exec("PRAGMA query_only=OFF"); err != nil {
					t.Fatal(err)
				}
				if got := store(requested); got != slug {
					t.Fatal("subject's final identity changed", got, slug)
				}
				for wantSlug, wantName := range stored {
					gotSlug, gotName, err := ResolveTopic(ctx, conn, wantName)
					if err != nil || gotSlug != wantSlug || gotName != wantName {
						t.Fatal("stored subject identity changed", wantName, gotSlug, gotName, err)
					}
				}
			})
		}
	}
}

func TestReservedTopicSuffixDoesNotMergeDistinctNames(t *testing.T) {
	for _, names := range [][2]string{
		{"Health", "Health Topic"}, {"Health Topic", "Health"},
		{"Read", "Read Topic"}, {"Read Topic", "Read"},
		{"Topics", "Topics Topic"}, {"Topics Topic", "Topics"},
		{"Process Topic", "Process Topic Topic"}, {"Process Topic Topic", "Process Topic"},
	} {
		t.Run(names[0]+" then "+names[1], func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			var slugs [2]string
			for i, name := range names {
				slug, storedName, err := ResolveTopic(ctx, conn, name)
				if err != nil {
					t.Fatal(err)
				}
				if topicname.IsReservedSlug(slug) || storedName != name || i > 0 && slug == slugs[0] {
					t.Fatal("distinct topic captured another catalog", name, slug, storedName, slugs)
				}
				slugs[i] = slug
				if _, err := conn.Exec("INSERT INTO topics(slug,name) VALUES(?,?)", slug, storedName); err != nil {
					t.Fatal(err)
				}
			}
			for i, name := range names {
				slug, _, err := ResolveTopic(ctx, conn, name)
				if err != nil || slug != slugs[i] {
					t.Fatal("repeat lookup changed topic identity", name, slug, err)
				}
			}
		})
	}
}

func TestReservedLegacyNameKeepsStoredIdentity(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	if _, err := conn.Exec(`
		INSERT INTO topics(id,slug,name) VALUES(1,'health','Health');
		INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES(1,1,'Legacy reading','https://docs.example.org/legacy',1);
		INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-09-10',1);
	`); err != nil {
		t.Fatal(err)
	}
	slug, name, err := ResolveTopic(ctx, conn, "Health")
	if err != nil || slug != "health" || name != "Health" {
		t.Fatal("legacy identity was rewritten", slug, name, err)
	}
	var topicID, pageID int
	if err := conn.QueryRow("SELECT topic_id,page_id FROM daily_readings").Scan(&topicID, &pageID); err != nil || topicID != 1 || pageID != 1 {
		t.Fatal("legacy reading reference changed", topicID, pageID, err)
	}
}
