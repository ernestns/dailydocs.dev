# DailyDocs Product Specification

Version: 0.2 (MVP)

## Vision

DailyDocs helps individuals and teams increase their depth of knowledge by reading for a few minutes each day.

Each reading teaches something new about a topic they care about and is sourced from documentation and durable technical references.

A DailyDocs reading is simply a URL.

Example:

```text
https://dailydocs.dev/go
```

The common URL shows today's reading. A dated URL shows a specific historical reading.

Every visitor receives the same reading for a topic on a given day. Tomorrow the common URL changes.

No accounts. No setup. Bookmark the reading and read.

## Product Philosophy

DailyDocs has a limited scope.

It is not:

- a documentation search engine
- a documentation mirror
- a learning management system
- another social network

It is:

> A deterministic daily reading from software documentation.

The application is designed for repeated daily use.

## Core Principles

### Deterministic

Given a topic and date, DailyDocs always returns the same reading.

This enables teams to read together, share discussions, cache responses, and revisit historical readings.

### Stateless

Version 1 stores no user state.

No accounts, sessions, cookies, or local storage are required for the reader experience.

The URL is the reading.

### Useful

Each topic should have a small catalog of roughly 10 to 50 useful documentation links. More links are only useful when they improve the daily reading experience.

Identifying useful documentation is the central product challenge.

Quality signals include:

- Foundational: understanding this unlocks many other topics.
- Practical impact: the knowledge improves how people build or debug real systems.
- Canonicality: the source is authoritative or widely accepted.
- Uniqueness: the page provides insight that is not repeated everywhere else.

### Interesting First

DailyDocs should recommend pages worth reading.

Official documentation is a strong positive signal, but it is not the only signal. A great canonical guide, deep explanation, or practical reference can be better than a generic official landing page.

## Goals

- Provide one reading per topic per day
- Promote useful documentation and durable technical references
- Reduce the need to search for documentation to read
- Enable teams to learn together
- Preserve historical daily readings

## MVP User Flow

User visits `dailydocs.dev`, searches for a topic, then clicks `View Reading`.

If the topic exists, DailyDocs shows today's reading.

If the topic does not exist, DailyDocs creates an enqueued topic request and starts a bounded search immediately.

```text
Topic
  -> Search
  -> Store
  -> Display
```

The generated reading URL is bookmarkable:

```text
https://dailydocs.dev/sqlite
```

The user visits every morning and reads that day's recommended documentation.

## Daily Reading

Each topic/date pair produces exactly one reading.

Example:

```text
Today's Reading

SQLite
Partial Indexes
12 min
Read
```

The documentation itself is never hosted by DailyDocs. Users are always sent to the source documentation.

## URL Format

Daily readings use path-based URLs:

```text
/{topic}
/{topic}/{date}
```

Examples:

```text
/go
/go/2026-06-26
/sqlite
/sqlite/2026-06-26
```

The topic-only URL is the common bookmarkable URL and resolves to today's reading.

The dated URL is the stable archive URL for a specific reading date.

The topic path segment uses the topic slug. The date path segment uses `YYYY-MM-DD`.

## Reading Selection

Each topic has a stable reading order.

Daily readings are stored in a `daily_readings` assignment table rather than recomputed forever from the current page list. This preserves historical accuracy when documentation pages are added, removed, disabled, or reordered.

The application may lazily create a daily assignment on first request for a topic/date pair.

## Topic Creation

Missing topics are requested by topic name only. Users are not asked to provide a documentation URL.

When a missing topic is requested:

1. Normalize the topic into a slug.
2. Create or reuse a topic request record.
3. Show that the request is enqueued.
4. If the global search limit allows it, run the search immediately.
5. Store accepted search results as active pages.
6. Display the first available reading once pages exist.

Initial rate limit:

- one topic search at a time globally
- at most one topic search every five minutes

The MVP has no manual activation gate.

## Search Pipeline

The first automated pipeline is intentionally simple:

```text
Topic
  -> Search provider
  -> Normalize results
  -> Review candidates
  -> Store evaluated candidates
  -> Store accepted pages
  -> Display daily reading
```

Tavily is the preferred search provider.

GPT-5 nano reviews candidate metadata when `OPENAI_API_KEY` is configured. The reviewer scores each candidate against the DailyDocs quality rubric. Every reviewed candidate is stored for observability, while only accepted candidates are stored as pages for the topic rotation.

If model review is unavailable, DailyDocs falls back to deterministic ranking and filtering.

Stored search results must include:

- topic
- title
- URL
- source/domain
- snippet or description when available
- result rank
- reviewer score when available
- page type when available
- reviewer reason when available
- accepted decision
- date stored

If the search provider is unavailable or returns no usable results, the topic remains visible as enqueued or failed rather than silently disappearing.

### Public Observability

DailyDocs publicly shows:

- requested topics
- topic status
- accepted page count
- evaluated candidate count
- evaluated webpages for each topic
- reviewer score, page type, reason, and accepted/rejected decision

## Functional Requirements

### Search Topics

Autocomplete is supported for existing topics.

If a topic does not exist, the UI should clearly offer to request the topic.

### View Reading

Support one topic per reading URL.

Produce a bookmarkable URL.

### Daily Reading

Display:

- title
- estimated reading time when known
- source
- official badge when known
- read button

### Requested Topics

Users should be able to tell that a missing topic has been enqueued.

A topic request should not require an account.

## Link Validation

The validator is a separate command mode in the DailyDocs binary.

Responsibilities:

- HEAD requests
- redirect handling
- broken link detection
- update `last_checked`
- disable consistently failing links

Broken links should never appear in new recommendations.

## Architecture

DailyDocs is a single Go web application backed by SQLite and deployed behind Caddy.

Responsibilities:

- topic search
- topic request enqueueing
- bounded search provider calls
- daily reading assignment
- rendering
- link validation command mode

Application startup automatically performs database migrations.

## Data Model

### topics

- id
- slug
- name
- description
- status
- created_at
- updated_at

Topic statuses:

- active
- queued
- searching
- failed

### pages

- id
- topic_id
- title
- url
- source
- official
- estimated_minutes
- reading_order
- active
- discovered_at
- last_verified
- created_at
- updated_at

Pages referenced by `daily_readings` must not be deleted. Removal from the reading pool means `active = 0`.

### daily_readings

- id
- topic_id
- reading_date
- page_id
- created_at

Unique constraint:

- topic_id
- reading_date

### topic_search_runs

- id
- topic_id
- provider
- query
- status
- started_at
- completed_at
- result_count
- stored_count
- reviewer_model
- reviewer_input_tokens
- reviewer_output_tokens
- reviewer_total_tokens
- error

### topic_search_results

- id
- topic_id
- search_run_id
- title
- url
- source
- snippet
- rank
- reviewer_score
- page_type
- reviewer_reason
- accepted
- stored_as_page_id
- created_at

## Deployment Philosophy

Infrastructure should fit on a single VPS.

Single Hetzner VPS, SQLite database, and one Go binary with web, validation, and search command modes.

The repository is the source of truth.

A brand-new VPS should be recoverable by:

```text
Install Git
  -> Clone repository
  -> Run scripts/bootstrap-ubuntu.sh
  -> Restore SQLite backup
  -> Application online
```

## Backups

SQLite operates in WAL mode.

Manual backups use SQLite's backup mechanism.

Scheduled offsite backups are a backlog item.

## Future Features

- User accounts
- Saved reading lists
- Reading history
- Read status
- Comments
- Page deactivation
- Topic and page metadata editing
- User feedback on requested topics and active readings
- Moderation
- Better result quality review
- Search provider fallback

AI-generated reading summaries, quizzes, and tagging are not required for the core reading experience.

## Success Metrics

Primary:

- returning daily visitors
- reading bookmarks
- documentation click-through rate

Secondary:

- supported topics
- indexed pages
- broken link rate
- successful topic searches

## Guiding Principle

Every design decision should answer one question:

> Does this help someone read one documentation page today?

If the answer is no, it probably does not belong in DailyDocs.
