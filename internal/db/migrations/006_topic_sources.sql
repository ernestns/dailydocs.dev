CREATE TABLE topic_sources (
	id INTEGER PRIMARY KEY,
	topic_id INTEGER NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
	base_url TEXT NOT NULL,
	normalized_url TEXT NOT NULL,
	source_host TEXT NOT NULL,
	source_type TEXT NOT NULL DEFAULT 'documentation',
	status TEXT NOT NULL DEFAULT 'pending_discovery' CHECK (status IN ('pending_discovery', 'ready_to_process', 'processing', 'candidates_ready', 'needs_scope', 'discovery_failed', 'disabled')),
	created_from_submission_id INTEGER REFERENCES documentation_submissions(id) ON DELETE SET NULL,
	last_processed_at TEXT,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE (topic_id, normalized_url)
);

CREATE INDEX idx_topic_sources_topic_status ON topic_sources(topic_id, status);
CREATE INDEX idx_topic_sources_submission ON topic_sources(created_from_submission_id);

ALTER TABLE pipeline_runs ADD COLUMN topic_source_id INTEGER REFERENCES topic_sources(id) ON DELETE SET NULL;
CREATE INDEX idx_pipeline_runs_source ON pipeline_runs(topic_source_id, started_at);

ALTER TABLE page_candidates ADD COLUMN topic_source_id INTEGER REFERENCES topic_sources(id) ON DELETE SET NULL;
CREATE INDEX idx_page_candidates_source ON page_candidates(topic_source_id, status);
