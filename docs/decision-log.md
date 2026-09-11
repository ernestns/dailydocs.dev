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
- An explicit POST request starts processing asynchronously when allowed; ordinary URL visits do not request topics.
- Public processing and retry rules are owned by [Topic Creation](product-spec.md#topic-creation).
- Evaluated search results are stored, and accepted results become active pages.
- There is no manual activation gate in the MVP.
- Existing documentation URL submission, source, candidate, and admin activation paths are retired.

Initial pipeline:

```text
Topic
  -> Queue
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
- AI summaries, quizzes, and tagging are future features, not MVP requirements.

### Plan Topic Searches Before Retrieval

Decision: when OpenAI is configured, use a stronger planner model before Tavily to generate focused senior-level subtopics and search queries.

Reason: one broad Tavily query tends to find homepages, hubs, and shallow overview pages. Planning turns a broad topic into specific retrieval intents while Tavily remains the grounding layer for actual URLs.

Implications:

- The planner output is not trusted as page data.
- Tavily searches remain bounded per query.
- GPT-5 nano remains the cheaper validation and ranking step.

### Process Topic Requests Asynchronously

Decision: process explicit topic requests asynchronously and expose a public retry action under the [Topic Creation contract](product-spec.md#topic-creation).

Reason: processing can take several seconds. Returning a status page immediately gives a better user experience while still keeping the implementation in the web process. The manual action keeps recoverable topics visible. A daily cap directly controls cost and abuse.

Implications:

- Explicit POST topic requests enqueue, then start Tavily/OpenAI processing in the background.
- The process action also starts background Tavily/OpenAI processing.
- Admission limits, waiting/running status, retry eligibility, and cancellation cleanup are specified in [Topic Creation](product-spec.md#topic-creation).
- Per-user rate limiting can wait until there is evidence the daily cap is insufficient.

### Require An Explicit Topic Generation Request

Decision: only a submitted topic form (POST `/read`) creates a missing topic and starts discovery.
GET topic URLs and GET `/read` remain lookup paths; an unknown topic URL offers a request action without enqueueing.
Dated unknown URLs return not found.

Reason: the public catalog accumulated scanner-like URL paths because arbitrary GET requests created topics and spent the shared daily processing budget.
This preserves the simple request flow without accounts or topic-name censorship.
Existing active daily-reading assignments remain supported; [Topic Creation](product-spec.md#topic-creation) owns explicit retry behavior.
No existing catalog data is deleted by this change.

### Bound Generation And Preserve Useful Shortfalls

Decision: bound discovery, expose recoverable shortfalls, and preserve useful output. [Topic Creation](product-spec.md#topic-creation) owns result targets, budgets, identity, and retry rules; [Search Pipeline](product-spec.md#search-pipeline) owns input eligibility and provider-failure behavior.

Reason: old queue history was mistaken for current processing, and the former one-link completion rule concealed insufficient catalogs.
The [production evidence](generation-quality.md#september-11-diagnosis) distinguishes historical queue state from the behavior of actual post-deployment generation.

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
