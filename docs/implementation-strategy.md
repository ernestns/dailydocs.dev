# DailyDocs Implementation Strategy

## Approach

Implement DailyDocs in thin vertical slices.

The core product risk is not rendering a reading page. The core risk is incorrect, stale, or broken links.

## Implementation Order

Completed:

0. Public hello-world Go app
1. SQLite schema and migrations
2. Topic/page seed importer
3. Daily reading assignment logic with tests
4. Basic reading page rendering
5. Topic search and reading URL generation UI
6. Link validator
7. Backup and restore scripts

Next:

8. Documentation submission queue
9. Minimal candidate pipeline
10. Manual activation
11. Queue runner

Backlog:

- Feedback and moderation
- Optional AI review layer
- Scheduled offsite backups

## Step Zero: Public Hello World

Before adding SQLite, Datastar, migrations, importer logic, or topic routes, prove the deployment path with the smallest useful Go application.

Bare minimum repository pieces:

- `go.mod`
- `cmd/web/main.go`
- `scripts/build.sh` or documented build commands
- deployment notes for systemd and Caddy

Initial routes:

```text
GET /        returns a simple DailyDocs page
GET /health  returns ok
```

Initial configuration:

```text
ADDR=:8080
```

Definition of done:

- `https://dailydocs.dev` loads publicly
- `https://dailydocs.dev/health` returns `ok`
- Caddy terminates TLS and proxies to the Go app
- the app runs under systemd or an equivalent supervisor
- the repository documents enough steps to rebuild the deployment on a fresh VPS

Do not include SQLite, Datastar, migrations, importer commands, validator logic, or topic routes in this milestone.

## Core Domain First

Define the database and selection rules before building much UI.

Initial tables:

- `topics`
- `pages`
- `daily_readings`
- `imports`
- `schema_migrations`

Key constraints:

- `topics.slug` is unique
- `pages(topic_id, url)` is unique
- `daily_readings(topic_id, reading_date)` is unique
- only active pages are eligible for new readings
- historical `daily_readings` rows are preserved

## Daily Assignment

The web app should have one core domain operation:

```text
GetDailyReading(topic, date) -> page
```

Behavior:

1. Check `daily_readings` for the topic/date pair.
2. If present, return the assigned page.
3. If missing, select from active pages.
4. Insert the assignment.
5. Return the assigned page.

This logic should be heavily tested because it is the product.

## Seed Data Before Automation

Do not begin with a complex crawler.

Start with a simple human-readable import format:

```yaml
topic: sqlite
name: SQLite
pages:
  - title: Write-Ahead Logging
    url: https://sqlite.org/wal.html
    source: SQLite Documentation
    official: true
    estimated_minutes: 12
```

Build a command:

```sh
dailydocs import-file topics/sqlite.yaml
```

This lets the product launch with documentation links immediately. Documentation submission processing can follow after the shape of good data is clearer.

## Web Application

Build a small Go monolith.

Routes:

```text
GET /                         topic picker
GET /{topic}                  today's reading page
GET /{topic}/{date}           daily reading page
GET /topics/search?q=go       autocomplete endpoint
```

The topic-only route is the common product URL:

```text
/sqlite
```

The topic/date route is the stable archive URL:

```text
/sqlite/2026-06-26
```

The homepage shows the topic picker and can send the user to the topic-only URL for the selected topic.

## Datastar Scope

Use Datastar modestly for:

- autocomplete
- selecting one topic
- generating the bookmarkable URL

Do not turn the app into a complex single-page application. The URL is the state.

## Link Validator

The validator is more important than the automated importer.

Command:

```sh
dailydocs validate-links
```

Responsibilities:

- check active pages
- follow redirects
- mark repeated failures
- update `last_verified`
- optionally propose URL updates

Broken links should be detected before broad importer automation.

## Documentation Submission Queue

This replaces the older idea of starting from a topic-only discovery command.

The lower-friction user action is submitting a documentation URL, not manually naming a topic. Topic name can be optional and inferred from the submitted source.

User flow:

```text
search missing topic
  -> submit documentation URL
  -> optional topic name
  -> submission appears in public queue
  -> processing pipeline discovers candidate pages
  -> deterministic extraction, classification, scoring, filtering, and deduplication
  -> eligible candidates can be activated
```

Definition of done:

- Add `documentation_submissions` migration.
- Let users submit a documentation homepage URL.
- Let users optionally provide a topic name.
- Normalize and deduplicate submitted URLs.
- Increment `request_count` for duplicate submissions.
- Show a public `/submissions` queue page.
- Add `noindex` to `/submissions`.
- Show only source host, suggested topic, status, request count, and last submitted time publicly.
- Keep raw errors, rejected URLs, score components, and internal debug data out of public pages.
- Accept only `http` and `https`.
- Store `submitter_ip_hash` for rate limiting.
- Include basic bot friction such as a honeypot field.
- Make submitted URLs non-clickable until processed.
- Do not crawl, process, create topics, or activate pages yet.
- Add tests for URL validation, deduplication, request counts, and public queue rendering.

## Minimal Candidate Pipeline

The first processing pipeline should persist candidates, not every intermediate crawl artifact.

Command:

```sh
dailydocs process-submission <submission-id>
```

The pipeline is deterministic and does not use AI.

Pipeline:

```text
submitted
  -> discovering
  -> crawling
  -> extracting
  -> classifying
  -> scoring
  -> filtering
  -> deduplicating
  -> persist page_candidates
  -> candidates_ready | failed
```

Initial stages:

```text
1. Discover candidate URLs from sitemap.xml, robots.txt sitemap declarations, navigation menus, sidebars, breadcrumbs, and internal documentation links.
2. Normalize, scope-check, and deduplicate the crawl frontier before fetching.
3. Crawl candidate URLs and keep URL, HTML, HTTP status, and headers in memory for extraction.
4. Extract title, H1, headings, plain text, word count, links, canonical URL, and meta description.
5. Classify with deterministic heuristics.
6. Apply hard exclusions before scoring.
7. Score remaining pages with explainable score components.
8. Filter pages below the minimum score.
9. Deduplicate redirects, normalized URLs, and trusted canonical URLs.
10. Persist eligible candidates to `page_candidates`.
```

The pipeline must be bounded:

- same host only by default
- submitted path prefix by default
- strip fragments
- drop query strings unless allowlisted
- max pages: 250
- max depth: 3
- max bytes per page: 2 MB
- request timeout: 10 seconds
- concurrency: 1 per host
- per-host delay
- respect robots.txt disallow rules
- extract sitemap declarations from robots.txt

The first implementation should not use AI. AI classification or scoring may replace or augment deterministic heuristics later.

Candidate activation should be a separate step from processing so candidates can be inspected before becoming active pages.

Canonical URL rule:

```text
Trust canonical URLs only when they stay inside the allowed host and path scope.
Otherwise use the final fetched URL as the candidate identity.
```

Hard exclusions before scoring:

- non-HTML responses
- login-required pages
- release notes
- archive pages
- changelogs
- download pages
- search pages
- tag/category index pages

Failure statuses should be structured per URL:

- robots_disallowed
- out_of_scope
- timeout
- too_large
- unsupported_content_type
- http_error
- redirect_out_of_scope
- parse_failed
- duplicate_normalized_url
- duplicate_canonical_url

Definition of done:

- Add `pipeline_runs`.
- Add `page_candidates`.
- Do not add `raw_documents` or `discovered_urls` tables yet.
- Discover candidate URLs from the submitted documentation homepage.
- Normalize, scope-check, and deduplicate the frontier in memory before fetching.
- Crawl bounded candidate pages.
- Extract structured metadata.
- Classify with deterministic heuristics.
- Score with explainable score components.
- Filter by minimum score.
- Deduplicate normalized URLs and canonical URLs.
- Persist eligible candidates.
- Persist run summary and bounded error summaries on `pipeline_runs`.
- Discard raw HTML after extraction.
- Make reruns idempotent.
- Do not activate candidates.
- Add tests for each stage using local test servers and fixtures.

Required uniqueness/idempotency constraints:

```text
documentation_submissions(normalized_url)
page_candidates(documentation_submission_id, normalized_url)
page_candidates(documentation_submission_id, canonical_url) where canonical_url is present
pages(topic_id, url)
```

## Manual Activation

Manual activation promotes eligible `page_candidates` into active `pages`.

Command:

```sh
dailydocs activate-candidates <submission-id>
```

Definition of done:

- Activate eligible candidates into `pages`.
- Preserve existing page IDs when possible.
- Do not mutate historical `daily_readings`.
- Keep rejected or low-scoring candidates out of active pages.
- Create a topic if needed.
- Resolve topic slug conflicts explicitly.
- Assign `reading_order` deterministically.
- Prefer all-or-nothing activation per submission.
- Update existing pages by URL, deactivate omitted pages only when explicitly requested.
- Store provenance from page to candidate and pipeline run on `pages`.
- Add tests for idempotency and page preservation.

## Queue Runner

The first queue runner should be a one-off batch command, not a long-running worker. It should be tested locally first, then run manually in production. Production scheduling waits until manual production runs are stable.

Command:

```sh
dailydocs process-pending-submissions --limit 5
```

Definition of done:

- Find pending submissions.
- Claim one submission at a time using a lease.
- Run `process-submission`.
- Mark success or failure.
- Continue up to the limit.
- Be safe to rerun.
- Recover stuck processing jobs after lease expiry.
- Add tests for claim behavior and failure handling.

Claim fields:

- locked_at
- locked_until
- locked_by
- attempt_count
- last_attempt_at
- last_error

Claim rule:

```text
Only claim pending or failed submissions whose lock is empty or expired.
Check affected row count after updating the lock.
```

## Observability Commands

Small-system operations should start with CLI inspection commands and logs.

Commands:

```sh
dailydocs list-submissions
dailydocs show-submission <id>
dailydocs list-runs <submission-id>
dailydocs list-candidates <submission-id>
```

Log per pipeline run:

- discovered count
- fetched count
- failed count
- eligible count
- rejected count
- duration

## Backlog: Feedback and Moderation

Feedback should come after the queue and candidate pipeline exist.

Possible features:

- upvote pending submissions
- flag duplicate submissions
- suggest topic name edits
- suggest better source URLs
- mark active readings as wrong topic, not documentation, broken, or duplicate

Definition of done is intentionally deferred.

## Backlog: Scheduled Offsite Backups

Scheduled backups are useful but not on the critical path while the app has little production data and queue processing is manual.

Current state:

- manual SQLite backup script exists
- manual SQLite restore script exists

Backlog scope:

- choose an object storage provider
- add systemd timer/unit examples or bootstrap support
- upload compressed SQLite backups off the VPS
- define retention for daily, weekly, and monthly backups
- document how to inspect timer logs and restore from an uploaded backup

## Backlog: Optional AI Review Layer

AI can later augment deterministic classification or scoring, but should not be foundational to the first pipeline.

Good AI boundary:

```text
candidate metadata + bounded excerpt -> structured review decision
```

Bad AI boundary:

```text
find all good docs for this topic
```

Definition of done is intentionally deferred.

## Deployment

Deploy as one Go binary with SQLite behind Caddy.

Application startup:

1. Open database.
2. Apply migrations.
3. Serve HTTP.

Operational scripts:

- `bootstrap.sh`
- `backup.sh`
- `restore.sh`
- `validate-links`
- `import-file`
- `process-submission`
- `process-pending-submissions`
- `activate-candidates`

## MVP Content Bar

Ship with 5-10 supported topics and documentation links.

This validates the daily reading flow before investing in the submission processing pipeline.
