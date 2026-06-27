CREATE TABLE topics (
	id INTEGER PRIMARY KEY,
	slug TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE pages (
	id INTEGER PRIMARY KEY,
	topic_id INTEGER NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	url TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT '',
	official INTEGER NOT NULL DEFAULT 0 CHECK (official IN (0, 1)),
	estimated_minutes INTEGER,
	difficulty TEXT,
	evergreen_score INTEGER,
	reading_order INTEGER NOT NULL,
	active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
	last_verified TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE (topic_id, url),
	UNIQUE (topic_id, reading_order)
);

CREATE TABLE daily_readings (
	id INTEGER PRIMARY KEY,
	topic_id INTEGER NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
	reading_date TEXT NOT NULL CHECK (reading_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
	page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE RESTRICT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE (topic_id, reading_date)
);

CREATE TABLE imports (
	id INTEGER PRIMARY KEY,
	topic TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
	started_at TEXT NOT NULL DEFAULT (datetime('now')),
	completed_at TEXT,
	pages_found INTEGER NOT NULL DEFAULT 0,
	pages_imported INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_pages_topic_active_order ON pages(topic_id, active, reading_order);
CREATE INDEX idx_daily_readings_date ON daily_readings(reading_date);
CREATE INDEX idx_imports_topic_started_at ON imports(topic, started_at);
