ALTER TABLE topic_search_runs ADD COLUMN reviewer_model TEXT NOT NULL DEFAULT '';
ALTER TABLE topic_search_runs ADD COLUMN reviewer_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE topic_search_runs ADD COLUMN reviewer_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE topic_search_runs ADD COLUMN reviewer_total_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE topic_search_results ADD COLUMN reviewer_score INTEGER;
ALTER TABLE topic_search_results ADD COLUMN page_type TEXT NOT NULL DEFAULT '';
ALTER TABLE topic_search_results ADD COLUMN reviewer_reason TEXT NOT NULL DEFAULT '';
