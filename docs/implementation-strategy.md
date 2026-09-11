# DailyDocs Implementation Strategy

## Current Approach

Build DailyDocs as a small Go monolith with SQLite.

The current MVP pipeline is:

```text
Topic
  -> Search
  -> Store
  -> Display
```

The main product risk is whether a requested topic can quickly produce useful documentation links. The reader flow, daily assignment logic, migrations, seed importer, validator, topic search package, and deployment path already exist.

## Current Structure

```text
cmd/web                 web server and command entrypoint
cmd/web/templates       server-rendered HTML
internal/db             SQLite connection and migrations
internal/reading        deterministic daily reading assignment
internal/seed           seed-file importer
internal/topicsearch    Tavily search and result persistence
internal/validator      active-link validation
scripts                 build, deploy, bootstrap, backup, restore
```

Avoid adding broad layers until there is a concrete need. New behavior should live behind focused packages.

## Next Work

The deployed discovery baseline, measured results, and opt-in evaluation procedure are recorded in [generation-quality.md](generation-quality.md). Use that evidence when choosing further quality work; planned features remain in the [Backlog](#backlog).

## Core Domain

The web app has two reader operations:

```text
GetDailyReading(topic, date) -> page
GetOrCreateDailyReading(topic, today) -> page
```

Archive lookup behavior:

1. Check `daily_readings` for the topic/date pair.
2. If present, return the assigned page.
3. If missing, return not found.

Topic-only behavior:

1. Resolve today in UTC.
2. Check `daily_readings` for the topic/today pair.
3. If present, return the assigned page.
4. If missing, select from active pages and store the assignment.
5. Return the assigned page.

This logic should stay heavily tested because it is the product.

## Topic Request Flow

When a user requests an existing topic:

```text
GET /{topic}
  -> find active topic
  -> get or create today's daily reading
  -> render reading page
```

When a user requests a missing topic:

```text
GET /{topic}
  -> show an explicit request action without creating data
POST /read (topic form)
  -> create queued topic
  -> start processing when allowed
  -> redirect to reading or status state
```

The [Topic Creation contract](product-spec.md#topic-creation) owns retry eligibility, typed-name identity, pending-work polling, and the distinction between catalog status and the latest attempt.

## Search Pipeline

Initial pipeline:

```text
topic name
  -> plan senior-level subtopics with a strong OpenAI model when configured
  -> Tavily searches for focused official-documentation queries
  -> normalize result URLs
  -> deduplicate by topic and URL
  -> review candidates in batches with GPT-5 nano when configured
  -> store search run
  -> store evaluated search results
  -> create active pages for accepted results
```

The planner generates retrieval intents, not accepted facts. Its output is used to split a broad topic into specific features, APIs, frameworks, internals, or capabilities that should have standalone documentation. Tavily remains the grounding layer for URLs.

Tavily query goals:

- prefer interesting documentation-like pages
- rank official documentation as a positive signal
- prefer standalone documentation pages
- avoid generic marketing pages when possible
- return enough results to seed the first daily rotation

OpenAI topic planning uses a stronger model by default, configured with `OPENAI_PLANNER_MODEL`. GPT-5 nano reviews search candidate metadata when `OPENAI_API_KEY` is configured. Without the key, the pipeline uses deterministic ranking and filtering so local development still works.

All candidates are stored in `topic_search_results`.
Only accepted candidates become active `pages`.
See [generation-quality.md](generation-quality.md#corrections-and-deterministic-replay) for authoritative URL/review identity rules and partial-decision handling.

## Search Limits

The [Topic Creation contract](product-spec.md#topic-creation) owns discovery budgets, admission limits, shortfalls, and retry behavior; [Search Pipeline](product-spec.md#search-pipeline) describes behavior when providers are unavailable or unconfigured.

## Data Model

The [SQLite migrations](../internal/db/migrations/) own the schema. See [traffic.md](traffic.md#storage-and-failure-behavior) for aggregate storage and reporting constraints.

`topics.status` values:

- `active`
- `queued`
- `searching`
- `failed`
- `disabled`

`topic_search_runs.status` values:

- `running`
- `completed`
- `failed`
- `rate_limited`

`topic_search_runs.stage` values while status is `running`:

- `searching`
- `reviewing`
- `storing`

Search candidates are stored after Tavily returns and before GPT review. Review metadata is written back to those rows after GPT returns. Only accepted reviewed candidates become active `pages`.

Historical `daily_readings` rows must not be deleted.

## Web Routes

```text
GET /                         topic picker
GET /{topic}                  today's reading, status, or explicit request action
GET /read?topic=...            lookup and redirect only
POST /read                    explicitly request/generate a topic
GET /{topic}/{date}           archived daily reading page
GET /topics/search?q=go       autocomplete endpoint
GET /topics                   topic index
```

The URL is the reader state.

## UI Scope

Use server-rendered Go templates with small, targeted JavaScript for interactions such as autocomplete and selecting one topic. Datastar is used for reactive topic status updates while background processing runs.

Do not turn the app into a complex single-page application.

## Operational Commands

Current commands:

```sh
dailydocs import-file topics/sqlite.yaml
dailydocs validate-links
```

Current topic-search command:

```sh
dailydocs search-topic rust
```

For the private reporting command and SSH recipe, see [traffic.md](traffic.md#view-the-report).

## Deployment

Deploy as one Go binary with SQLite behind Caddy.

Application startup:

1. Open database.
2. Apply migrations.
3. Serve HTTP.

Operational scripts:

- `scripts/bootstrap-ubuntu.sh`
- `scripts/backup-sqlite.sh`
- `scripts/restore-sqlite.sh`
- `scripts/deploy-remote.sh`

## Backlog

- User feedback on active readings
- Deactivate active pages
- Edit active topic and page metadata
- Scheduled offsite backups
- Search provider fallback
- Better result quality review
- Abuse controls beyond the initial daily processing cap
