package topicsearch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type focusedProvider struct {
	calls     int
	failAt    int
	duplicate bool
	limits    []int
}

func (p *focusedProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	p.calls++
	p.limits = append(p.limits, limit)
	if p.calls == p.failAt {
		return nil, errors.New("source unavailable")
	}
	id := p.calls
	if p.duplicate {
		id = 1
	}
	return []SearchResult{{Title: fmt.Sprintf("Focused guide %d", id), URL: fmt.Sprintf("https://docs.example.org/guide-%d", id)}}, nil
}

type selectiveReviewer struct {
	calls         int
	firstAccepted int
	failAt        int
	cancel        context.CancelFunc
}

func (r *selectiveReviewer) Review(ctx context.Context, topic string, candidates []ReviewCandidate) (ReviewOutput, error) {
	r.calls++
	if r.calls == r.failAt {
		if r.cancel != nil {
			r.cancel()
			return ReviewOutput{}, ctx.Err()
		}
		return ReviewOutput{}, errors.New("review unavailable")
	}
	out := ReviewOutput{Model: "fake-reviewer", TotalTokens: 100}
	for i, c := range candidates {
		out.Results = append(out.Results, ReviewResult{Index: c.Index, DailyDocsScore: 90, PageType: "guide", ShouldStore: r.calls > 1 || i < r.firstAccepted})
	}
	return out, nil
}

func sixIntents() Planner {
	var plan PlanOutput
	for i := range 12 {
		plan.Topics = append(plan.Topics, PlannedTopic{SearchQueries: []string{fmt.Sprintf("Focused intent %d", i)}})
	}
	return fakePlanner{output: plan}
}

func TestDiscoveryStopsAtTargetOrBound(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		first                 int
		duplicate             bool
		calls, reviews, pages int
	}{
		{"target reached", 3, false, 3, 1, 3},
		{"bounded second group", 2, false, 6, 2, 3},
		{"duplicates cannot pad", 3, true, 6, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			p := &focusedProvider{duplicate: tc.duplicate}
			r := &selectiveReviewer{firstAccepted: tc.first}
			result, err := SearchTopic(ctx, conn, "Example", Options{Provider: p, Planner: sixIntents(), Reviewer: r})
			if tc.pages < 2 {
				if !errors.Is(err, ErrInsufficientResults) {
					t.Fatal(result, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			count, countErr := AvailableReadings(ctx, conn, result.TopicID)
			if countErr != nil || count != tc.pages || p.calls != tc.calls || r.calls != tc.reviews {
				t.Fatal("unexpected bounded outcome", result, count, p.calls, r.calls, countErr)
			}
			for _, limit := range p.limits {
				if limit != 3 {
					t.Fatal("unbounded requested results", p.limits)
				}
			}
		})
	}
}

func TestLaterFailureKeepsUsefulPartialResults(t *testing.T) {
	for _, stage := range []string{"search", "review", "cancellation"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			p := &focusedProvider{}
			r := &selectiveReviewer{firstAccepted: 2}
			if stage == "search" {
				p.failAt = 4
			} else {
				r.failAt = 2
			}
			if stage == "cancellation" {
				r.cancel = cancel
			}
			result, err := SearchTopic(ctx, conn, "Example", Options{Provider: p, Planner: sixIntents(), Reviewer: r, Now: fixedTopicSearchTime})
			if err == nil || errors.Is(err, ErrInvalidTopic) || result.Status != "failed" || result.StoredCount != 2 {
				t.Fatal("failure was hidden or partial output lost", result, err)
			}
			var topicStatus, runStatus string
			var stored, tokens int
			if err := conn.QueryRow("SELECT t.status,r.status,r.stored_count,r.reviewer_total_tokens FROM topics t JOIN topic_search_runs r ON r.topic_id=t.id").Scan(&topicStatus, &runStatus, &stored, &tokens); err != nil {
				t.Fatal(err)
			}
			if topicStatus != "active" || runStatus != "failed" || stored != 2 || tokens != 100 {
				t.Fatal("partial evidence not retained", topicStatus, runStatus, stored, tokens)
			}
			available, err := AvailableReadings(context.Background(), conn, result.TopicID)
			if err != nil || available != 2 {
				t.Fatal(available, err)
			}
			if stage != "search" {
				var unreviewed int
				if err := conn.QueryRow("SELECT count(*) FROM topic_search_results WHERE reviewer_score IS NULL").Scan(&unreviewed); err != nil || unreviewed != 3 {
					t.Fatal("later candidates must stay unreviewed", unreviewed, err)
				}
			}
			_, err = SearchTopic(context.Background(), conn, "Other", Options{Provider: &focusedProvider{}, Planner: sixIntents(), Reviewer: &selectiveReviewer{firstAccepted: 3}, Now: func() time.Time { return fixedTopicSearchTime().Add(6 * time.Minute) }})
			if err != nil {
				t.Fatal("failed run still occupies global running slot", err)
			}
		})
	}
}

func TestSearchErrorKeepsEarlierResultsInSameGroup(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	p := &focusedProvider{failAt: 2}
	r := &selectiveReviewer{firstAccepted: 3}
	result, err := SearchTopic(ctx, conn, "Example", Options{Provider: p, Planner: sixIntents(), Reviewer: r})
	if !errors.Is(err, ErrInsufficientResults) || result.StoredCount != 1 || p.calls != 2 || r.calls != 1 {
		t.Fatal(result, err, p.calls, r.calls)
	}
}

func TestPlannerInvalidityIsNotAvailabilityFailure(t *testing.T) {
	invalid := false
	for _, tc := range []struct {
		name    string
		planner Planner
		invalid bool
	}{
		{"clear non-topic", fakePlanner{output: PlanOutput{ValidTopic: &invalid, ValidityReason: "This is an instruction, not a subject."}}, true},
		{"provider unavailable", fakePlanner{err: errors.New("planner unavailable")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			p := &focusedProvider{}
			_, err := SearchTopic(ctx, conn, "Example", Options{Provider: p, Planner: tc.planner})
			if err == nil || errors.Is(err, ErrInvalidTopic) != tc.invalid || p.calls != 0 {
				t.Fatal("invalidity/availability conflated", err, p.calls)
			}
		})
	}
}

func TestSuccessfulRetryAccumulatesDistinctReadings(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	first, err := SearchTopic(ctx, conn, "Example", Options{Provider: fakeProvider{results: []SearchResult{{Title: "Guide one", URL: "https://docs.example.org/one"}}}, Now: fixedTopicSearchTime})
	if !errors.Is(err, ErrInsufficientResults) || first.StoredCount != 1 {
		t.Fatal(first, err)
	}
	var originalID int64
	if err := conn.QueryRow("SELECT id FROM pages").Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	second, err := SearchTopic(ctx, conn, "Example", Options{Provider: fakeProvider{results: []SearchResult{{Title: "Guide two", URL: "https://docs.example.org/two"}}}, Now: func() time.Time { return fixedTopicSearchTime().Add(6 * time.Minute) }})
	if err != nil || second.Status != "completed" {
		t.Fatal(second, err)
	}
	var retained int
	if err := conn.QueryRow("SELECT count(*) FROM pages WHERE id=? AND url='https://docs.example.org/one'", originalID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("original useful reading replaced", retained, err)
	}
}

func TestLaterReviewBatchFailurePreservesEarlierDecisions(t *testing.T) {
	ctx := context.Background()
	conn := openTopicSearchTestDB(t, ctx)
	defer conn.Close()
	p := fakeProvider{results: []SearchResult{
		{Title: "One", URL: "https://docs.example.org/one"},
		{Title: "Two", URL: "https://docs.example.org/two"},
		{Title: "Three", URL: "https://docs.example.org/three"},
	}}
	r := &selectiveReviewer{firstAccepted: 2, failAt: 2}
	result, err := SearchTopic(ctx, conn, "Example", Options{Provider: p, Reviewer: r, ReviewBatchSize: 2})
	if err == nil || result.StoredCount != 2 || r.calls != 2 {
		t.Fatal(result, err, r.calls)
	}
	var unreviewed, accepted int
	if err := conn.QueryRow("SELECT count(*) FROM topic_search_results WHERE reviewer_score IS NULL").Scan(&unreviewed); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT count(*) FROM topic_search_results WHERE accepted=1").Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if unreviewed != 1 || accepted != 2 {
		t.Fatal(unreviewed, accepted)
	}
}

func TestSearchResponseRespectsRequestedLimit(t *testing.T) {
	p := fakeProvider{results: []SearchResult{{Title: "One"}, {Title: "Two"}, {Title: "Three"}, {Title: "Four"}}}
	got, err := executeSearchRequests(context.Background(), p, []SearchRequest{{Query: "Example", MaxResults: 3}})
	if err != nil || len(got) != 3 {
		t.Fatal(len(got), err)
	}
}

type plannerFunc func(context.Context, string) (PlanOutput, error)

func (f plannerFunc) Plan(ctx context.Context, topic string) (PlanOutput, error) {
	return f(ctx, topic)
}

func TestRunCleanupCoversPostPlanAndFinalizationFailures(t *testing.T) {
	for _, failure := range []string{"cancellation", "query update", "completion update"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			if _, err := conn.Exec(`
				INSERT INTO topics(id,slug,name,status) VALUES(1,'example','Example','active');
				INSERT INTO pages(topic_id,title,url,reading_order) VALUES
				(1,'Saved one','https://docs.example.org/saved-one',1),
				(1,'Saved two','https://docs.example.org/saved-two',2);
			`); err != nil {
				t.Fatal(err)
			}
			p := &focusedProvider{}
			planner := plannerFunc(func(context.Context, string) (PlanOutput, error) {
				if failure == "cancellation" {
					cancel()
				}
				return PlanOutput{}, nil
			})
			if failure != "cancellation" {
				trigger := `CREATE TRIGGER fail_update BEFORE UPDATE OF query ON topic_search_runs BEGIN SELECT RAISE(FAIL,'query unavailable'); END`
				if failure == "completion update" {
					trigger = `CREATE TRIGGER fail_update BEFORE UPDATE ON topic_search_runs WHEN NEW.status='completed' BEGIN SELECT RAISE(FAIL,'completion unavailable'); END`
				}
				if _, err := conn.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			result, err := SearchTopic(ctx, conn, "Example", Options{Provider: p, Planner: planner, Now: fixedTopicSearchTime})
			if err == nil || result.Status != "failed" || result.TopicID != 1 || result.RunID == 0 {
				t.Fatalf("failure lost its run identity: %+v %v", result, err)
			}
			if failure == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("original cancellation lost: %v", err)
			}
			if failure != "completion update" && p.calls != 0 {
				t.Fatalf("provider ran after failed planning transition: %d", p.calls)
			}
			var status, stage, diagnostic, topicStatus string
			var completed bool
			if err := conn.QueryRow(`SELECT r.status,r.stage,r.error,r.completed_at IS NOT NULL,t.status FROM topic_search_runs r JOIN topics t ON t.id=r.topic_id WHERE r.id=?`, result.RunID).Scan(&status, &stage, &diagnostic, &completed, &topicStatus); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || stage != "" || diagnostic == "" || !completed || topicStatus != "active" {
				t.Fatal("run not finalized or saved catalog hidden", status, stage, diagnostic, completed, topicStatus)
			}
			if failure != "cancellation" {
				if _, err := conn.Exec("DROP TRIGGER fail_update"); err != nil {
					t.Fatal(err)
				}
			}
			_, err = SearchTopic(context.Background(), conn, "Other", Options{Provider: &focusedProvider{}, Planner: sixIntents(), Now: func() time.Time { return fixedTopicSearchTime().Add(6 * time.Minute) }})
			if err != nil {
				t.Fatal("failure retained global running restriction", err)
			}
		})
	}
}

func TestProcessingPreservesSelectedDuplicateNameCatalog(t *testing.T) {
	for _, entry := range []string{"explicit active", "explicit failed", "next queued", "typed name"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			conn := openTopicSearchTestDB(t, ctx)
			defer conn.Close()
			status := "active"
			if entry == "explicit failed" {
				status = "failed"
			} else if entry == "next queued" {
				status = "queued"
			}
			if _, err := conn.Exec(`
				INSERT INTO topics(id,slug,name,status) VALUES(1,'first','Example','active'),(2,'second','Example',?);
				INSERT INTO pages(id,topic_id,title,url,reading_order) VALUES
				(11,1,'First saved reading','https://docs.example.org/first',1),
				(21,2,'Second saved reading','https://docs.example.org/second',1);
				INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(1,'2026-06-27',11),(2,'2026-06-27',21);
			`, status); err != nil {
				t.Fatal(err)
			}
			opts := Options{Provider: fakeProvider{results: []SearchResult{
				{Title: "Guide one", URL: "https://docs.example.org/new-one"},
				{Title: "Guide two", URL: "https://docs.example.org/new-two"},
			}}, Now: fixedTopicSearchTime}
			var result Result
			var err error
			wantID, wantSlug := int64(2), "second"
			switch entry {
			case "next queued":
				var queued QueueResult
				queued, err = ProcessNextQueuedTopic(ctx, conn, opts)
				result = queued.Result
			case "typed name":
				result, err = SearchTopic(ctx, conn, "Example", opts)
				wantID, wantSlug = 1, "first"
			default:
				var queued QueueResult
				queued, err = ProcessQueuedTopic(ctx, conn, "second", opts)
				result = queued.Result
			}
			if err != nil || result.TopicID != wantID || result.TopicSlug != wantSlug {
				t.Fatalf("processed a different catalog: %+v %v", result, err)
			}
			for _, id := range []int64{1, 2} {
				want := 1
				if id == wantID {
					want = 3
				}
				if got, err := AvailableReadings(ctx, conn, id); err != nil || got != want {
					t.Fatalf("catalog %d readings=%d want=%d err=%v", id, got, want, err)
				}
			}
			var unchanged int
			if err := conn.QueryRow(`SELECT count(*) FROM daily_readings WHERE (topic_id=1 AND page_id=11) OR (topic_id=2 AND page_id=21)`).Scan(&unchanged); err != nil || unchanged != 2 {
				t.Fatal("historical assignments changed", unchanged, err)
			}
		})
	}
}
