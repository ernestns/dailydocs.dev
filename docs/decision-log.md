# DailyDocs Decision Log

## Current Direction

DailyDocs uses topic requests, not documentation URL submissions.

The MVP topic pipeline is:

```text
Topic
  -> Tavily Search
  -> GPT Review
  -> Store
  -> Display
```

There is no manual activation gate in the MVP.

The content strategy is Interesting First: official documentation is preferred when it is useful, but durable technical references can also qualify.

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

### Replace Documentation URL Submissions With Topic Requests

Decision: missing-topic expansion starts from a topic name, not a documentation URL.

Reason: asking for only a topic is lower friction and keeps the product focused on "I want to read about Rust" rather than "I know which documentation homepage to submit."

Implications:

- Missing-topic search should offer a topic request.
- The request is visible as queued.
- The system starts a bounded search immediately when allowed by the global rate limit.
- Search results are stored directly as active pages for the MVP.
- There is no manual activation gate in the MVP.
- Existing documentation URL submission, source, candidate, and admin activation paths are retired.

Initial pipeline:

```text
Topic
  -> Search
  -> Store
  -> Display
```

### Use Tavily Search With Optional GPT Review

Decision: use Tavily as the search provider and GPT-5 nano as an optional candidate reviewer.

Reason: search-only results produced too many pages about documentation, listicles, and noisy sources. A small structured review pass gives the product a better way to apply the DailyDocs quality rubric while keeping the pipeline simple.

Implications:

- Store the search run and reviewed candidate results.
- Convert accepted reviewed results into active pages.
- Store reviewer score, page type, reason, and accepted/rejected decision when available.
- Expose requested topics and evaluated candidates publicly for observability.
- Fall back to deterministic ranking when `OPENAI_API_KEY` is not configured.
- AI summaries, quizzes, tagging, and quality review are future features, not MVP requirements.

### Keep Topic Search Globally Bounded

Decision: the MVP runs at most one topic search at a time globally, and no more than one every five minutes.

Reason: inline search keeps the product simple, but provider calls need a basic cost and abuse guard before being exposed publicly.

Implications:

- If a topic is requested while the limit is active, keep it queued.
- The UI should show that the request has been enqueued.
- A later request or command can process queued topics.
- Per-user rate limiting can wait until there is evidence the global limit is insufficient.

### Deprioritize Scheduled Backups

Decision: keep manual backup and restore scripts, but move scheduled offsite backups to the backlog.

Reason: the app currently has little production data. Scheduled offsite backups matter more once the database contains meaningful topic requests or automated search runs regularly.

Implications:

- Manual SQLite backup and restore scripts remain available.
- Scheduled backups should not block topic requests or automated search.
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

### Topic Feedback Scope

Question: should user feedback on queued topics and active readings be part of the first topic-search pipeline version?

Recommendation: make queued topics visible first. Add upvotes, duplicate flags, source edits, and reading-level feedback after the basic topic-search pipeline exists.
