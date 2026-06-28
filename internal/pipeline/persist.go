package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func persistCandidate(ctx context.Context, conn *sql.DB, sub sourceSubmission, runID int64, cand candidate) error {
	headings, _ := json.Marshal(cand.Headings)
	links, _ := json.Marshal(cand.Links)
	tags, _ := json.Marshal(cand.Tags)
	components, _ := json.Marshal(cand.ScoreComponents)
	slug := slugify(firstNonEmpty(sub.SuggestedTopic, sub.SourceHost))
	if sub.TopicSlug != "" {
		slug = sub.TopicSlug
	}
	proposedName := sub.SuggestedTopic
	if sub.TopicName != "" {
		proposedName = sub.TopicName
	}

	excerpt := cand.Text
	if len(excerpt) > 500 {
		excerpt = excerpt[:500]
	}

	_, err := conn.ExecContext(ctx, `
		INSERT INTO page_candidates (
			documentation_submission_id,
			pipeline_run_id,
			topic_source_id,
			proposed_topic_slug,
			proposed_topic_name,
			topic_id,
			title,
			h1,
			url,
			normalized_url,
			canonical_url,
			source,
			http_status,
			extracted_excerpt,
			word_count,
			headings,
			meta_description,
			links,
			paragraph_count,
				link_count,
				code_block_count,
				link_density,
				gate_score,
				gate_page_type,
				reject_stage,
				primary_classification,
				classification_tags,
				classification_rules_version,
			score,
			score_components,
			official,
			estimated_minutes,
			reason,
			reject_reason,
			review_decision,
			review_confidence,
			review_model,
			review_prompt_version,
			review_input_hash,
			review_rationale,
			gate_input_tokens,
			gate_output_tokens,
			gate_reasoning_tokens,
			gate_total_tokens,
			enrichment_input_tokens,
			enrichment_output_tokens,
			enrichment_reasoning_tokens,
			enrichment_total_tokens,
			status
		)
		VALUES (
			:documentation_submission_id,
			:pipeline_run_id,
			:topic_source_id,
			:proposed_topic_slug,
			:proposed_topic_name,
			:topic_id,
			:title,
			:h1,
			:url,
			:normalized_url,
			:canonical_url,
			:source,
			:http_status,
			:extracted_excerpt,
			:word_count,
			:headings,
			:meta_description,
			:links,
			:paragraph_count,
			:link_count,
			:code_block_count,
			:link_density,
			:gate_score,
			:gate_page_type,
			:reject_stage,
			:primary_classification,
			:classification_tags,
			:classification_rules_version,
			:score,
			:score_components,
			1,
			:estimated_minutes,
			:reason,
			:reject_reason,
			:review_decision,
			:review_confidence,
			:review_model,
			:review_prompt_version,
			:review_input_hash,
			:review_rationale,
			:gate_input_tokens,
			:gate_output_tokens,
			:gate_reasoning_tokens,
			:gate_total_tokens,
			:enrichment_input_tokens,
			:enrichment_output_tokens,
			:enrichment_reasoning_tokens,
			:enrichment_total_tokens,
			:status
		)
		ON CONFLICT(documentation_submission_id, normalized_url) DO UPDATE SET
			pipeline_run_id = excluded.pipeline_run_id,
			topic_source_id = excluded.topic_source_id,
			proposed_topic_slug = excluded.proposed_topic_slug,
			proposed_topic_name = excluded.proposed_topic_name,
			topic_id = excluded.topic_id,
			title = excluded.title,
			h1 = excluded.h1,
			url = excluded.url,
			canonical_url = excluded.canonical_url,
			source = excluded.source,
			http_status = excluded.http_status,
			extracted_excerpt = excluded.extracted_excerpt,
			word_count = excluded.word_count,
			headings = excluded.headings,
			meta_description = excluded.meta_description,
			links = excluded.links,
			paragraph_count = excluded.paragraph_count,
				link_count = excluded.link_count,
				code_block_count = excluded.code_block_count,
				link_density = excluded.link_density,
				gate_score = excluded.gate_score,
				gate_page_type = excluded.gate_page_type,
				reject_stage = excluded.reject_stage,
				primary_classification = excluded.primary_classification,
			classification_tags = excluded.classification_tags,
			classification_rules_version = excluded.classification_rules_version,
			score = excluded.score,
			score_components = excluded.score_components,
			official = excluded.official,
			estimated_minutes = excluded.estimated_minutes,
			reason = excluded.reason,
			reject_reason = excluded.reject_reason,
			review_decision = excluded.review_decision,
			review_confidence = excluded.review_confidence,
			review_model = excluded.review_model,
			review_prompt_version = excluded.review_prompt_version,
			review_input_hash = excluded.review_input_hash,
			review_rationale = excluded.review_rationale,
			gate_input_tokens = excluded.gate_input_tokens,
			gate_output_tokens = excluded.gate_output_tokens,
			gate_reasoning_tokens = excluded.gate_reasoning_tokens,
			gate_total_tokens = excluded.gate_total_tokens,
			enrichment_input_tokens = excluded.enrichment_input_tokens,
			enrichment_output_tokens = excluded.enrichment_output_tokens,
			enrichment_reasoning_tokens = excluded.enrichment_reasoning_tokens,
			enrichment_total_tokens = excluded.enrichment_total_tokens,
			status = excluded.status
	`,
		sql.Named("documentation_submission_id", sub.ID),
		sql.Named("pipeline_run_id", runID),
		sql.Named("topic_source_id", nullableID(sub.TopicSourceID)),
		sql.Named("proposed_topic_slug", slug),
		sql.Named("proposed_topic_name", proposedName),
		sql.Named("topic_id", nullableID(sub.TopicID)),
		sql.Named("title", cand.Title),
		sql.Named("h1", cand.H1),
		sql.Named("url", cand.URL),
		sql.Named("normalized_url", cand.NormalizedURL),
		sql.Named("canonical_url", cand.CanonicalURL),
		sql.Named("source", sub.SourceHost),
		sql.Named("http_status", cand.HTTPStatus),
		sql.Named("extracted_excerpt", excerpt),
		sql.Named("word_count", cand.WordCount),
		sql.Named("headings", string(headings)),
		sql.Named("meta_description", cand.MetaDescription),
		sql.Named("links", string(links)),
		sql.Named("paragraph_count", cand.ParagraphCount),
		sql.Named("link_count", cand.LinkCount),
		sql.Named("code_block_count", cand.CodeBlockCount),
		sql.Named("link_density", cand.LinkDensity),
		sql.Named("gate_score", cand.Review.GateScore),
		sql.Named("gate_page_type", cand.Review.GatePageType),
		sql.Named("reject_stage", cand.Review.RejectStage),
		sql.Named("primary_classification", cand.Classification),
		sql.Named("classification_tags", string(tags)),
		sql.Named("classification_rules_version", rulesVersion),
		sql.Named("score", cand.Score),
		sql.Named("score_components", string(components)),
		sql.Named("estimated_minutes", cand.EstimatedMinutes),
		sql.Named("reason", cand.Reason),
		sql.Named("reject_reason", cand.RejectReason),
		sql.Named("review_decision", cand.Review.Decision),
		sql.Named("review_confidence", cand.Review.Confidence),
		sql.Named("review_model", cand.Review.Model),
		sql.Named("review_prompt_version", cand.Review.PromptVersion),
		sql.Named("review_input_hash", cand.Review.InputHash),
		sql.Named("review_rationale", cand.Review.Rationale),
		sql.Named("gate_input_tokens", cand.Review.GateUsage.InputTokens),
		sql.Named("gate_output_tokens", cand.Review.GateUsage.OutputTokens),
		sql.Named("gate_reasoning_tokens", cand.Review.GateUsage.ReasoningTokens),
		sql.Named("gate_total_tokens", cand.Review.GateUsage.TotalTokens),
		sql.Named("enrichment_input_tokens", cand.Review.EnrichmentUsage.InputTokens),
		sql.Named("enrichment_output_tokens", cand.Review.EnrichmentUsage.OutputTokens),
		sql.Named("enrichment_reasoning_tokens", cand.Review.EnrichmentUsage.ReasoningTokens),
		sql.Named("enrichment_total_tokens", cand.Review.EnrichmentUsage.TotalTokens),
		sql.Named("status", cand.Status),
	)
	if err != nil {
		return fmt.Errorf("persist page candidate: %w", err)
	}
	return nil
}

func loadSubmission(ctx context.Context, conn *sql.DB, id int64) (sourceSubmission, error) {
	var sub sourceSubmission
	err := conn.QueryRowContext(ctx, `
		SELECT id, submitted_url, normalized_url, source_host, suggested_topic
		FROM documentation_submissions
		WHERE id = ?
	`, id).Scan(&sub.ID, &sub.SubmittedURL, &sub.NormalizedURL, &sub.SourceHost, &sub.SuggestedTopic)
	if err != nil {
		return sourceSubmission{}, fmt.Errorf("load submission: %w", err)
	}
	return sub, nil
}

func loadTopicSource(ctx context.Context, conn *sql.DB, id int64) (sourceSubmission, error) {
	var sub sourceSubmission
	err := conn.QueryRowContext(ctx, `
		SELECT
			ts.id,
			COALESCE(ts.created_from_submission_id, 0),
			ts.topic_id,
			t.slug,
			t.name,
			ts.base_url,
			ts.normalized_url,
			ts.source_host
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE ts.id = ?
			AND ts.status = 'active'
	`, id).Scan(&sub.TopicSourceID, &sub.ID, &sub.TopicID, &sub.TopicSlug, &sub.TopicName, &sub.SubmittedURL, &sub.NormalizedURL, &sub.SourceHost)
	if err != nil {
		return sourceSubmission{}, fmt.Errorf("load topic source: %w", err)
	}
	if sub.ID < 1 {
		return sourceSubmission{}, errors.New("topic source is missing created_from_submission_id")
	}
	sub.SuggestedTopic = sub.TopicName
	sub.ProcessAsSource = true
	return sub, nil
}

func startRun(ctx context.Context, conn *sql.DB, sub sourceSubmission, opts Options) (int64, error) {
	policy, _ := json.Marshal(map[string]any{
		"max_pages": opts.MaxPages,
		"max_depth": opts.MaxDepth,
		"max_bytes": opts.MaxBytes,
		"min_score": opts.MinScore,
	})
	result, err := conn.ExecContext(ctx, `
		INSERT INTO pipeline_runs (documentation_submission_id, topic_source_id, status, crawl_policy)
		VALUES (?, ?, 'running', ?)
	`, sub.ID, nullableID(sub.TopicSourceID), string(policy))
	if err != nil {
		return 0, fmt.Errorf("start pipeline run: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read pipeline run id: %w", err)
	}
	return id, nil
}

func nullableID(id int64) any {
	if id < 1 {
		return nil
	}
	return id
}

func markSubmissionProcessing(ctx context.Context, conn *sql.DB, submissionID int64, runID int64) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE documentation_submissions
		SET status = 'processing',
			latest_pipeline_run_id = ?,
			attempt_count = attempt_count + 1,
			last_attempt_at = datetime('now'),
			last_error = ''
		WHERE id = ?
	`, runID, submissionID)
	return err
}

func markSubmissionReady(ctx context.Context, conn *sql.DB, submissionID int64) error {
	_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'candidates_ready', last_error = '' WHERE id = ?", submissionID)
	return err
}

func markSubmissionFailed(ctx context.Context, conn *sql.DB, submissionID int64, runErr error) error {
	_, err := conn.ExecContext(ctx, "UPDATE documentation_submissions SET status = 'failed', last_error = ? WHERE id = ?", runErr.Error(), submissionID)
	return err
}

func markTopicSourceProcessed(ctx context.Context, conn *sql.DB, sourceID int64) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE topic_sources
		SET last_processed_at = datetime('now'),
			last_error = '',
			updated_at = datetime('now')
		WHERE id = ?
	`, sourceID)
	return err
}

func markTopicSourceFailed(ctx context.Context, conn *sql.DB, sourceID int64, runErr error) error {
	status := "active"
	var tooBroad DiscoveryTooBroadError
	if errors.As(runErr, &tooBroad) {
		status = "needs_scope"
	}
	_, err := conn.ExecContext(ctx, `
		UPDATE topic_sources
		SET status = ?,
			last_error = ?,
			updated_at = datetime('now')
		WHERE id = ?
	`, status, runErr.Error(), sourceID)
	return err
}

func completeRun(ctx context.Context, conn *sql.DB, runID int64, result Result) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE pipeline_runs
		SET status = 'completed',
			completed_at = datetime('now'),
			discovered_count = ?,
			crawled_count = ?,
			eligible_count = ?,
			rejected_count = ?,
			failure_count = ?
		WHERE id = ?
	`, result.DiscoveredCount, result.CrawledCount, result.EligibleCount, result.RejectedCount, result.FailureCount, runID)
	return err
}

func failRun(ctx context.Context, conn *sql.DB, runID int64, result Result, runErr error) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE pipeline_runs
		SET status = 'failed',
			completed_at = datetime('now'),
			discovered_count = ?,
			crawled_count = ?,
			eligible_count = ?,
			rejected_count = ?,
			failure_count = ?,
			error = ?
		WHERE id = ?
	`, result.DiscoveredCount, result.CrawledCount, result.EligibleCount, result.RejectedCount, result.FailureCount, runErr.Error(), runID)
	return err
}
