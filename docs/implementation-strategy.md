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

The main product risk is whether a requested topic can quickly produce useful documentation links. The reader flow, daily assignment logic, migrations, seed importer, validator, and deployment path already exist.

## Current Structure

```text
cmd/web                 web server and command entrypoint
cmd/web/templates       server-rendered HTML
internal/db             SQLite connection and migrations
internal/reading        deterministic daily reading assignment
internal/seed           seed-file importer
internal/validator      active-link validation
scripts                 build, deploy, bootstrap, backup, restore
```

Avoid adding broad layers until there is a concrete need. New behavior should live behind focused packages.

## Next Work

1. Add `internal/topicsearch`.
2. Add Tavily search provider integration.
3. Store search runs and search results.
4. Convert stored search results into active pages.
5. Trigger topic search inline when a missing topic is requested.
6. Add global search throttling: one search at a time, at most once every five minutes.
7. Add `dailydocs search-topic <topic>` for local testing and production repair.

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
  -> show queued state
  -> if global throttle allows, search immediately
  -> store results as pages
  -> render reading page if pages now exist
```

The UI should make it clear that the request is enqueued even when search begins inline.

## Search Pipeline

Initial pipeline:

```text
topic name
  -> Tavily search
  -> normalize result URLs
  -> deduplicate by topic and URL
  -> store search run
  -> store search results
  -> create active pages
```

Tavily query goals:

- prefer official documentation
- prefer standalone documentation pages
- avoid generic marketing pages when possible
- return enough results to seed the first daily rotation

The MVP does not use GPT or manual review in the pipeline.

## Search Limits

Initial limits:

- one topic search at a time globally
- at most one topic search every five minutes
- bounded provider timeout
- bounded provider result count

If the limit is active, the missing topic remains queued and can be searched by a later request or command.

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
- Abuse controls beyond the initial global rate limit
