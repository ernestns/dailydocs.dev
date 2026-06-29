# DailyDocs Product Specification

Version: 0.1 (MVP)

## Vision

DailyDocs helps individuals and teams increase their depth of knowledge by reading for a few minutes each day.

Each reading teaches something new about a topic they care about and is sourced from official documentation whenever possible.

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

This enables:

- teams reading together
- shared discussions
- cache-friendly infrastructure
- reproducible URLs

### Stateless

Version 1 stores no user state.

No accounts, sessions, cookies, or local storage.

The URL is the reading.

### Useful

The application recommends documentation links selected from known documentation sources.

Each topic should have a small catalog of roughly 10 to 50 high-quality documentation links. More links are only useful when they improve the daily reading experience.

Identifying high-quality documentation is the central product challenge.

Quality signals include:

- Foundational: understanding this unlocks many other topics.
- Practical impact: the knowledge improves how people build or debug real systems.
- Canonicality: the source is authoritative or widely accepted.
- Uniqueness: the page provides insight that is not repeated everywhere else.

### Official First

Whenever possible, recommendations should come from official documentation.

Community resources are only used when an official source does not exist.

## Goals

- Encourage continuous learning
- Provide one reading per topic per day
- Promote official documentation
- Reduce the need to search for documentation to read
- Enable teams to learn together

## MVP User Flow

User visits `dailydocs.dev`, searches for a topic, then clicks `View Reading`.

Example topics:

- Go
- SQLite
- Docker

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
/docker
/docker/2026-06-26
```

The topic-only URL is the common bookmarkable URL and resolves to today's reading.

The dated URL is the stable archive URL for a specific reading date.

The topic path segment uses the topic slug. The date path segment uses `YYYY-MM-DD`.

The homepage may redirect a selected topic to the topic-only URL.

## Reading Selection

Each topic has a stable reading order.

During import:

1. Discover documentation pages
2. Extract metadata
3. Review quality
4. Filter poor candidates
5. Store reading order

Example:

```text
SQLite

1 WAL Mode
2 Partial Indexes
3 VACUUM
4 Transactions
5 Query Planner
```

Daily readings are stored in a `daily_readings` assignment table rather than recomputed forever from the current page list. This preserves historical accuracy when documentation pages are added, removed, disabled, or reordered.

The application may lazily create a daily assignment on first request for a topic/date pair.

## Functional Requirements

### Search Topics

Autocomplete is supported.

Users search existing topics.

If a topic does not exist, offer documentation URL submission.

For future topic creation, the lower-friction action is documentation link submission rather than topic submission. A user can submit a documentation base URL, optionally provide a topic name, and the system can infer or propose the topic during processing.

### View Reading

Support one topic per reading URL.

Produce a bookmarkable URL.

### Daily Reading

Display:

- title
- estimated reading time
- source
- official badge
- read button

## Import System

The importer is a separate command mode in the DailyDocs binary.

Purpose: turn a topic into a reading list.
The target output is 10 to 50 high-quality documentation links for a topic, not an exhaustive mirror of the documentation site.

Example:

```text
Import "SQLite"
  -> Discover official documentation
  -> Extract pages
  -> Normalize URLs
  -> Remove duplicates
  -> Estimate reading time
  -> Assign metadata
  -> Review quality
  -> Store
```

Initially, imports are manually started.

## Documentation Link Submission

Future topic expansion should start from submitted documentation links.

If a user searches for a topic that does not exist, offer a way to submit a documentation URL. The topic name is optional at submission time.

Example submission:

```text
url: https://sqlite.org/docs.html
topic: SQLite
```

The submitted URL is enqueued for processing. It does not immediately create an active topic.

Submission status values:

- pending
- processing
- candidates_ready
- active
- rejected
- failed

Public submission pages should show status, source host, suggested topic, request count, and last submission time. They should not expose internal errors, raw crawl output, rejected candidate URLs, or score components. Public queue pages should include `noindex`.

Submission safety:

- accept only `http` and `https`
- normalize and deduplicate URLs before insert
- hash submitter IPs for rate limiting
- include basic bot friction such as a honeypot field
- make submitted URLs non-clickable until processed
- keep admin/debug details out of the public queue

Processing:

```text
submitted documentation URL
  -> infer or confirm topic
  -> discover candidate documentation pages
  -> crawl pages
  -> extract structured metadata
  -> review quality
  -> filter low-scoring pages
  -> deduplicate URLs and canonicals
  -> persist eligible candidates
```

The pipeline should be deterministic and idempotent around discovery, extraction, deduplication, and persistence so the same documentation homepage can be processed repeatedly without duplicating candidates.

Quality review uses page metadata and bounded excerpts. The reviewer should favor pages that are foundational, practically useful, canonical, and unique.

Candidate pages should be persisted before activation. A separate activation step can promote eligible candidates into active `pages`.

Initial bounded stages:

1. Discover candidate URLs from sitemap.xml, robots.txt sitemap declarations, navigation menus, sidebars, breadcrumbs, and internal documentation links.
2. Normalize, scope-check, and deduplicate the crawl frontier before fetching.
3. Crawl candidate URLs and keep URL, HTML, HTTP status, and headers in memory for extraction.
4. Extract title, H1, headings, plain text, word count, links, canonical URL, and meta description.
5. Apply hard exclusions before review.
6. Review page quality.
7. Filter pages below the minimum score.
8. Deduplicate redirects, normalized URLs, and trusted canonical URLs.
9. Persist eligible candidates.

Default crawl policy:

- same host only
- submitted path prefix by default
- strip fragments
- drop query strings unless allowlisted
- max pages: 250
- max depth: 3
- max bytes per page: 2 MB
- request timeout: 10 seconds
- concurrency: 1 per host
- respect robots.txt disallow rules
- extract sitemap declarations from robots.txt

Canonical URLs are trusted only when they stay inside the allowed host and path scope. Otherwise, the final fetched URL remains the candidate identity.

Hard exclusions:

- non-HTML responses
- login-required pages
- release notes
- archive pages
- changelogs
- download pages
- search pages
- tag/category index pages

Possible page types:

- Tutorial
- Guide
- Concept
- Reference
- API
- Example
- Migration
- Release Notes
- FAQ
- Archive
- Other

Quality rubric:

- Foundational: does understanding this unlock many other topics?
- Practical impact: will this knowledge improve how people build or debug real systems?
- Canonicality: is this the authoritative or widely accepted source?
- Uniqueness: does it provide insights that are not repeated elsewhere?

Example threshold:

- `score >= 70`: eligible candidate

Users should be able to see pending submissions so they know a documentation source has been enqueued. A public queue page can list submitted/pending sources and their processing status.

Possible future feedback on the queue:

- upvote a pending submission
- flag duplicates
- suggest topic name edits
- suggest better source URLs

Possible future feedback on active readings:

- good link
- wrong topic
- not documentation
- broken link
- duplicate

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

### Web Application

Responsibilities:

- topic search
- reading generation
- deterministic selection
- rendering

Technology:

- Go
- SQLite
- Caddy

Single monolith.

### Importer

Separate Go command mode.

Responsibilities:

- documentation link processing
- topic inference
- candidate discovery
- scraping
- parsing
- deterministic classification
- explainable scoring
- metadata generation
- deduplication
- reading order generation

Runs manually.

### Validator

Separate Go command mode.

Responsibilities:

- link verification
- health checks
- redirect updates

Runs manually, scheduled later if desired.

## Data Model

### topics

- id
- slug
- name
- description
- status
- created_at

### pages

- id
- topic_id
- page_candidate_id
- activated_from_pipeline_run_id
- title
- url
- source
- official
- estimated_minutes
- difficulty
- evergreen_score
- reading_order
- active
- activation_reason
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

### imports

- id
- topic
- status
- started_at
- completed_at
- pages_found
- pages_imported
- error

The `imports` table is for seed-file imports only. Pipeline processing history belongs in `documentation_submissions`, `pipeline_runs`, and `page_candidates`.

### documentation_submissions

- id
- submitted_url
- normalized_url
- source_host
- inferred_topic
- suggested_topic
- status
- visibility
- allowed_hosts
- allowed_path_prefixes
- locked_at
- locked_until
- locked_by
- latest_pipeline_run_id
- request_count
- attempt_count
- last_attempt_at
- submitter_ip_hash
- rejection_reason
- last_error
- first_submitted_at
- last_submitted_at
- error

Unique constraint:

- normalized_url

### pipeline_runs

- id
- documentation_submission_id
- status
- crawl_policy
- started_at
- completed_at
- discovered_count
- crawled_count
- eligible_count
- rejected_count
- failure_count
- error

`documentation_submissions.status` is the public lifecycle. `pipeline_runs.status` is the processing attempt lifecycle.

Submission statuses:

- pending
- processing
- candidates_ready
- active
- rejected
- failed

Pipeline run statuses:

- running
- completed
- failed
- canceled

The first implementation does not persist discovered URL rows or raw HTML rows. Discovery, crawl, and extraction artifacts stay in memory during a run. `pipeline_runs` stores aggregate counts and bounded error summaries; `page_candidates` stores the eligible output.

### page_candidates

- id
- documentation_submission_id
- pipeline_run_id
- proposed_topic_slug
- proposed_topic_name
- topic_id
- title
- h1
- url
- normalized_url
- canonical_url
- source
- http_status
- extracted_excerpt
- word_count
- headings
- primary_classification
- classification_tags
- classification_rules_version
- score
- score_components
- official
- estimated_minutes
- reason
- reject_reason
- status
- created_at
- reviewed_at

`headings`, `classification_tags`, and `score_components` are JSON stored as text in SQLite until querying them requires a different shape.

Unique constraints:

- documentation_submission_id, normalized_url
- documentation_submission_id, canonical_url when canonical_url is present

Activation history can be added later if moderation needs it. For MVP, provenance fields on `pages` are enough.

## Deployment Philosophy

Infrastructure should fit on a single VPS.

Single Hetzner VPS, SQLite database, and one Go binary with web, import, validation, and processing command modes.

The repository is the source of truth.

A brand-new VPS should be recoverable by:

```text
Install Git
  -> Clone repository
  -> Run bootstrap.sh
  -> Restore SQLite backup
  -> Application online
```

Application startup automatically performs database migrations.

## Backups

SQLite operates in WAL mode.

Manual backups use SQLite's backup mechanism.

Scheduled offsite backups are a backlog item. When added, backups should be:

- compressed
- uploaded to object storage
- retained daily, weekly, and monthly

Recovery should be documented and tested before scheduled processing becomes important.

## Future Features

- User accounts
- Saved reading lists
- Reading history
- Read status
- Comments
- Page deactivation
- Topic and page metadata editing
- User feedback on pending submissions and active readings
- Moderation
- Optional AI summaries, quizzes, difficulty estimation, or tagging

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
- successful imports

## Guiding Principle

Every design decision should answer one question:

> Does this help someone read one documentation page today?

If the answer is no, it probably does not belong in DailyDocs.
