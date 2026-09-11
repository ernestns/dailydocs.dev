CREATE TABLE traffic_daily (
	day TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('total', 'page', 'referrer', 'dropped', 'errors')),
	label TEXT NOT NULL DEFAULT '',
	audience TEXT NOT NULL CHECK (audience IN ('other', 'known_bot', 'all')),
	count INTEGER NOT NULL CHECK (count >= 0),
	PRIMARY KEY (day, kind, label, audience)
);

CREATE TABLE traffic_metadata (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	started_at TEXT NOT NULL,
	last_flush_at TEXT NOT NULL
);
