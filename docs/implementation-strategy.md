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

1. Test Tavily searches with real topics locally.
2. Deploy topic search to production.
3. Observe real search results and adjust query wording if needed.
4. Decide whether search-only quality is sufficient before adding feedback or review.

## Core Domain

The web app has one core reader operation:

```text
GetDailyReading(topic, date) -> page
```

Behavior:

1. Check `daily_readings` for the topic/date pair.
2. If present, return the assigned page.
3. If missing, select from active pages.
4. Insert the assignment.
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
  -> create queued topic
  -> attempt processing when allowed
  -> show reading or queued state
```

The UI should make it clear when the request remains queued. A process action is available for queued topics.

Processing flow:

```text
request or POST /process-topic
  -> find oldest queued topic
  -> stop if 20 topics have been processed today
  -> run search pipeline
  -> mark active or failed
```

## Search Pipeline

Initial pipeline:

```text
topic name
  -> Tavily search
  -> normalize result URLs
  -> deduplicate by topic and URL
  -> review candidates with GPT-5 nano when configured
  -> store search run
  -> store evaluated search results
  -> create active pages for accepted results
```

Tavily query goals:

- prefer interesting documentation-like pages
- rank official documentation as a positive signal
- prefer standalone documentation pages
- avoid generic marketing pages when possible
- return enough results to seed the first daily rotation

GPT-5 nano reviews search candidate metadata when `OPENAI_API_KEY` is configured. Without the key, the pipeline uses deterministic ranking and filtering so local development still works.

Every reviewed candidate is stored in `topic_search_results`. Only accepted candidates become active `pages`.

## Search Limits

Initial processing limits:

- one topic search at a time globally
- process at most 20 topics per UTC day
- bounded provider timeout
- bounded provider result count

If `TAVILY_API_KEY` is missing, topics remain queued.

## Data Model

Current tables:

- `topics`
- `pages`
- `daily_readings`
- `imports`
- `topic_search_runs`
- `topic_search_results`

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
GET /{topic}                  today's reading page or queued state
GET /{topic}/{date}           daily reading page
GET /topics/search?q=go       autocomplete endpoint
GET /topics                   topic index
```

The URL is the reader state.

## UI Scope

Use server-rendered Go templates with small, targeted JavaScript for interactions such as autocomplete and selecting one topic.

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
