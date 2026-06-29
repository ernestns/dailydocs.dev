# DailyDocs Implementation Strategy

## Approach

Implement DailyDocs in thin vertical slices.

The core product risk is not rendering a reading page. The core risk is whether a requested topic can quickly produce useful documentation links.

The current MVP pipeline is:

```text
Topic
  -> Search
  -> Store
  -> Display
```

No manual activation gate is required for the MVP.

## Completed

0. Public hello-world Go app
1. SQLite schema and migrations
2. Topic/page seed importer
3. Daily reading assignment logic with tests
4. Basic reading page rendering
5. Topic search and reading URL generation UI
6. Link validator
7. Backup and restore scripts
8. Documentation URL submission queue
9. Candidate discovery pipeline
10. Manual activation
11. Queue runner
12. Pipeline inspection commands
13. Topic index page
14. Missing-topic submission fallback
15. Topic lookup combobox
16. Protected admin UI
17. Topic sources and source lifecycle statuses
18. Source discovery preview and discovery history
19. Candidate filters and pipeline telemetry in admin
20. Admin source action guardrails
21. Duplicate-submit protection

These completed items reflect the first pipeline direction. The architecture has since been simplified. Existing code can be removed or retired as the new topic-search flow replaces it.

## Next

22. Add topic request records and statuses.
23. Add Tavily search provider integration.
24. Store search runs and search results.
25. Convert stored search results into active pages.
26. Trigger topic search inline when a missing topic is requested.
27. Show queued/searching/failed topic states in the reader UI.
28. Add global search throttling: one search at a time, at most once every five minutes.
29. Remove retired documentation URL submission, source, candidate, GPT review, and admin activation paths.

## Backlog

- User feedback on active readings
- Deactivate active pages
- Edit active topic and page metadata
- Scheduled offsite backups
- Search provider fallback
- Better result quality review
- Abuse controls beyond the initial global rate limit

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

## Data Model Changes

Add or adapt tables for:

- `topics`
- `pages`
- `daily_readings`
- `topic_search_runs`
- `topic_search_results`

Suggested `topics.status` values:

- `active`
- `queued`
- `searching`
- `failed`

Suggested `topic_search_runs.status` values:

- `running`
- `completed`
- `failed`
- `rate_limited`

`pages` should keep enough provenance to know whether a page came from search:

- source
- discovered_at
- search_run_id or equivalent provenance field

Historical `daily_readings` rows must not be deleted.

## Web Application

Build a small Go monolith.

Routes:

```text
GET /                         topic picker
GET /{topic}                  today's reading page or queued state
GET /{topic}/{date}           daily reading page
GET /topics/search?q=go       autocomplete endpoint
GET /topics                   topic index
```

The topic-only route is the common product URL:

```text
/sqlite
```

The topic/date route is the stable archive URL:

```text
/sqlite/2026-06-26
```

The URL is the reader state.

## UI Scope

Use server-rendered Go templates with small, targeted JavaScript for interactions such as autocomplete, selecting one topic, and submit locking.

Do not turn the app into a complex single-page application.

## Link Validator

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

Broken links should be detected before broad traffic depends on a topic.

## Operational Commands

Keep operational commands small and explicit.

Useful commands:

```sh
dailydocs import-file topics/sqlite.yaml
dailydocs validate-links
dailydocs search-topic rust
```

The web app can call the same topic-search application code inline. The command exists for local testing and production repair, not as the primary user path.

## Deployment

Deploy as one Go binary with SQLite behind Caddy.

Application startup:

1. Open database.
2. Apply migrations.
3. Serve HTTP.

Operational scripts:

- `bootstrap.sh`
- `backup-sqlite.sh`
- `restore-sqlite.sh`
- `deploy-remote.sh`

## MVP Content Bar

Each supported topic should aim for roughly 10 to 50 useful documentation links.

The first automated version may store fewer than 10 results if the provider returns too few usable links, but the UI should make the topic state visible rather than hiding the failure.
