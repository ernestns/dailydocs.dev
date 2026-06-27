ALTER TABLE documentation_submissions ADD COLUMN locked_at TEXT;
ALTER TABLE documentation_submissions ADD COLUMN locked_until TEXT;
ALTER TABLE documentation_submissions ADD COLUMN locked_by TEXT NOT NULL DEFAULT '';
ALTER TABLE documentation_submissions ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE documentation_submissions ADD COLUMN last_attempt_at TEXT;
ALTER TABLE documentation_submissions ADD COLUMN latest_pipeline_run_id INTEGER;

ALTER TABLE pages ADD COLUMN page_candidate_id INTEGER;
ALTER TABLE pages ADD COLUMN activated_from_pipeline_run_id INTEGER;
ALTER TABLE pages ADD COLUMN activation_reason TEXT NOT NULL DEFAULT '';

CREATE TABLE pipeline_runs (
	id INTEGER PRIMARY KEY,
	documentation_submission_id INTEGER NOT NULL REFERENCES documentation_submissions(id) ON DELETE CASCADE,
	status TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed', 'canceled')),
	crawl_policy TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL DEFAULT (datetime('now')),
	completed_at TEXT,
	discovered_count INTEGER NOT NULL DEFAULT 0,
	crawled_count INTEGER NOT NULL DEFAULT 0,
	eligible_count INTEGER NOT NULL DEFAULT 0,
	rejected_count INTEGER NOT NULL DEFAULT 0,
	failure_count INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_pipeline_runs_submission ON pipeline_runs(documentation_submission_id, started_at);

CREATE TABLE page_candidates (
	id INTEGER PRIMARY KEY,
	documentation_submission_id INTEGER NOT NULL REFERENCES documentation_submissions(id) ON DELETE CASCADE,
	pipeline_run_id INTEGER NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
	proposed_topic_slug TEXT NOT NULL DEFAULT '',
	proposed_topic_name TEXT NOT NULL DEFAULT '',
	topic_id INTEGER,
	title TEXT NOT NULL,
	h1 TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL,
	normalized_url TEXT NOT NULL,
	canonical_url TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	http_status INTEGER NOT NULL DEFAULT 0,
	extracted_excerpt TEXT NOT NULL DEFAULT '',
	word_count INTEGER NOT NULL DEFAULT 0,
	headings TEXT NOT NULL DEFAULT '[]',
	primary_classification TEXT NOT NULL DEFAULT 'Other',
	classification_tags TEXT NOT NULL DEFAULT '[]',
	classification_rules_version TEXT NOT NULL DEFAULT 'heuristic-v1',
	score INTEGER NOT NULL DEFAULT 0,
	score_components TEXT NOT NULL DEFAULT '[]',
	official INTEGER NOT NULL DEFAULT 0 CHECK (official IN (0, 1)),
	estimated_minutes INTEGER,
	reason TEXT NOT NULL DEFAULT '',
	reject_reason TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'eligible' CHECK (status IN ('eligible', 'rejected', 'activated')),
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	reviewed_at TEXT,
	UNIQUE (documentation_submission_id, normalized_url)
);

CREATE UNIQUE INDEX idx_page_candidates_canonical
ON page_candidates(documentation_submission_id, canonical_url)
WHERE canonical_url != '';
