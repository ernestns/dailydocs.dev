package topicname

import "testing"

func TestReservedSlugsLeaveRawIdentityUnchanged(t *testing.T) {
	for _, slug := range []string{"health", "read", "topics", "process-topic"} {
		if !IsReservedSlug(slug) {
			t.Errorf("application path %q is not reserved", slug)
		}
		if got := Slug(Display(slug)); got != slug {
			t.Errorf("raw identity changed: %q became %q", slug, got)
		}
	}
	for _, slug := range []string{"go", "health-topic", "read-topic", "topics-topic", "process-topic-topic"} {
		if IsReservedSlug(slug) {
			t.Errorf("ordinary reading slug %q is reserved", slug)
		}
	}
}
