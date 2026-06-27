# DailyDocs Decision Log

## Accepted Decisions

### Store Daily Reading Assignments

Decision: add a `daily_readings` table that records the selected page for each topic/date pair.

Reason: documentation page lists will change. Pages may be added, removed, disabled, or reordered. A stored assignment preserves what DailyDocs recommended on a given day without storing the documentation contents.

Implications:

- Historical reading results remain stable.
- New readings are generated from currently active pages.
- Past assignments are not automatically changed when page metadata changes.
- Admin repair tooling may later replace a broken current-day assignment if needed.

### Remove `Another` From MVP

Decision: exclude the `Another` feature from MVP.

Reason: the MVP supports one reading per topic per day. Offering alternate readings adds product and URL complexity without strengthening the core behavior.

### Support Single-Topic Reading URLs

Decision: MVP supports one topic per reading URL using path-based routes:

```text
/{topic}
/{topic}/{date}
```

Example:

```text
/sqlite
/sqlite/2026-06-26
```

Reason: single-topic URLs make the product easier to understand and implement. The topic-only URL is the common bookmark for today's reading, while the dated URL gives DailyDocs a stable historical address. The daily assignment model is naturally keyed by one topic and one date, and multi-topic bundles can be deferred until there is evidence users need them.

### Start With Reviewed Seed Files

Decision: use reviewed seed files before building automated discovery.

Reason: reviewed seed files define the initial link set without depending on scraping heuristics.

### Build Validator Before Full Importer Automation

Decision: implement link validation before a broad automated importer.

Reason: broken links are worse than a smaller topic catalog.

### Replace Discovery Importer With Documentation Submissions

Decision: future topic expansion starts from submitted documentation URLs rather than direct topic requests.

Reason: a documentation URL is lower friction and more actionable than a topic name alone. The system can infer or propose the topic from the submitted source, then discover candidate pages from that source.

Implications:

- Missing-topic search should offer documentation URL submission.
- Topic name is optional at submission time.
- Submitted sources are visible in a pending queue.
- Initial processing is deterministic: discovery, crawl, extraction, heuristic classification, explainable scoring, filtering, deduplication, and persistence.
- AI review is a later optional layer, not the first implementation.
- Candidate activation is separate from candidate processing.
- The old `dailydocs discover <topic> <url>` concept is replaced by `process-submission`.

Initial deterministic pipeline stages:

- Discover candidate URLs.
- Crawl pages and keep raw responses in memory for extraction.
- Extract structured metadata.
- Classify with deterministic heuristics.
- Score with explainable components.
- Filter by minimum score.
- Deduplicate normalized and canonical URLs.
- Persist eligible candidates.

Initial score threshold:

- `score >= 70`: eligible candidate

### Keep Submission Processing Bounded and Auditable

Decision: documentation processing must use explicit crawl bounds, two-phase deduplication, in-memory raw HTML handling, and structured per-URL failure statuses.

Reason: public documentation homepages can contain large navigation graphs, duplicate URL forms, redirects, query strings, archives, release notes, and non-document pages. The pipeline must be predictable before it can be scheduled.

Initial crawl defaults:

- same host only
- submitted path prefix by default
- strip fragments
- drop query strings unless allowlisted
- max pages: 250
- max depth: 3
- max bytes per page: 2 MB
- request timeout: 10 seconds
- concurrency: 1 per host
- respect robots.txt

Deduplication happens twice:

1. Before crawl: normalize, scope-check, and dedupe the frontier.
2. After crawl: dedupe redirects, final URLs, and trusted same-scope canonical URLs.

Raw HTML is temporary and should not be persisted in the first implementation. Long-term records should keep candidate metadata, bounded excerpts, and score explanations.

### Separate Public Queue State From Pipeline Attempts

Decision: `documentation_submissions.status` is the public lifecycle, while `pipeline_runs.status` is the processing attempt lifecycle.

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

Processing claims use leases so a crashed run does not leave submissions stuck forever.

### Keep Public Queue Boring

Decision: `/submissions` can be public, but should expose only safe summary fields.

Public fields:

- source host
- suggested topic
- status
- request count
- last submitted time

Do not expose raw crawler errors, rejected URLs, raw submitted links as clickable links, score components, or internal debug details on public pages. Add `noindex`.

### Deprioritize Scheduled Backups

Decision: keep manual backup and restore scripts, but move scheduled offsite backups to the backlog.

Reason: the app currently has little production data, and near-term submission processing will be manual. Scheduled offsite backups matter more once the database contains meaningful user submissions or automated queue processing runs regularly.

Implications:

- Manual SQLite backup and restore scripts remain available.
- Scheduled backups should not block the submission queue or manual processing pipeline.
- Before regular scheduled processing, revisit offsite backup cadence, storage provider, retention, and restore testing.

## Open Decisions

### Canonical Day Boundary

Question: should DailyDocs use UTC or a configured product timezone for the meaning of "today"?

Recommendation: use UTC for MVP unless the product needs a configured local date boundary.

### Initial Topic Set

Question: which 5-10 topics should launch first?

Recommendation: choose technologies with official documentation and common developer use, such as Go, SQLite, Docker, PostgreSQL, Git, Python, TypeScript, Kubernetes, Redis, and HTTP.

### Import Review Format

Question: should seed/review files use YAML, JSON, or Markdown frontmatter?

Recommendation: use YAML for human-edited topic files unless the Go implementation strongly favors another format.

### Submission Feedback Scope

Question: should user feedback on pending submissions and active readings be part of the first processing pipeline version?

Recommendation: make pending submissions publicly visible first. Add upvotes, duplicate flags, source edits, and reading-level feedback after the basic queue and processing pipeline exist.
