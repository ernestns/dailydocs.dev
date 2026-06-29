ALTER TABLE topic_search_results ADD COLUMN accepted INTEGER NOT NULL DEFAULT 0 CHECK (accepted IN (0, 1));
