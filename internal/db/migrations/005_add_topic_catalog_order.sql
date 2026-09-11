CREATE INDEX idx_topics_catalog_order
ON topics (name COLLATE NOCASE, id)
WHERE status != 'disabled';
