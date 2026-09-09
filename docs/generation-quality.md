# Generation quality evidence

DailyDocs generates a catalog of reading links, not original articles.
This change integrates the existing unfinished subtopic planner and adds explicit generation requests, duplicate protection, and honest review status.
SQLite remains the catalog and shared daily-reading store; no existing data is deleted.

## Observed baseline, 2026-09-09

The public `/topics` page contained 2,288 topics: 1,476 queued, 145 failed, and 667 active.
Names such as `wp-admin`, `phpinfo`, and `webmail` suggested scanner traffic; request origins were not verified from logs.
The original handler and its tests confirmed that an arbitrary unknown GET could create a topic and invoke the provider.
An explicit form submission is now required, so URL visits no longer consume topic-generation capacity.
Existing active reading URLs still create their intended daily assignment when needed.

The existing [SQLite catalog](https://dailydocs.dev/topics/sqlite/evaluations) had one accepted link among 17 candidates: a [Medium full-text-search tutorial](https://medium.com/@johnidouglasmarangon/full-text-search-in-sqlite-a-practical-guide-80a69c3f42a4).
No sqlite.org candidate was present.
The existing Rust and Python catalogs had three accepted readings from eight and eighteen candidates respectively.
This historical observation is not a controlled benchmark or a claim that nonofficial articles are inherently poor.

## Bounded planner sample

One local SQLite generation used a disposable database, the existing provider credentials, four focused searches with three results each, and a single candidate-review call.
No live application data or deployment changed.

| Step | Configuration and observed usage |
| --- | --- |
| Planner | `gpt-5.5-2026-04-23`, high reasoning, 2,514 total tokens |
| Search | Four Tavily basic requests: WAL, locking, query planner, EXPLAIN QUERY PLAN; each constrained to sqlite.org |
| Review | `gpt-5-nano-2025-08-07`, low reasoning, 2,838 total tokens |
| Duration | 46.7 seconds |
| Raw output | 12 candidates, all from sqlite.org or www.sqlite.org |
| Original acceptance | Five entries, representing only three distinct articles |

The useful accepted readings were:

- [EXPLAIN QUERY PLAN](https://sqlite.org/eqp.html): interpreting query plans and index usage; reviewer score 85.
- [The Next-Generation Query Planner](https://www.sqlite.org/queryplanner-ng.html): understanding query-planning internals; reviewer score 75.
- [The SQLite Query Optimizer Overview](https://sqlite.org/optoverview.html#manual_control_of_query_plans_using_sqlite_stat_tables): optimizer background and practical query-plan controls; reviewer score 70.

This provides evidence that focused retrieval can find canonical material missing from the historical catalog.
It also exposed duplicate URL aliases and incomplete model reviews.
The WAL query returned search pages, so the sample does not establish adequate subtopic coverage or an ideal complete rotation.
The reviewer sees metadata and snippets, not complete source pages; these scores remain model judgments rather than independently established reading quality.

## Corrections and deterministic replay

The captured response is represented by `internal/topicsearch/testdata/sqlite-quality.json` and exercised by `TestSQLiteQualityReplay`.
The corrected pipeline publishes three distinct useful readings from those same recorded decisions, rather than five duplicate entries.
It retains all ten distinct candidate destinations; seven omitted review decisions are explicitly unreviewed rather than invented zero-score rejections.
That replay is deterministic and makes no external requests; it is not a second live generation sample.

URL equivalence is intentionally narrow.
SQLite's HTTP query-planner URL was verified to redirect to HTTPS, and the www/non-www HTTPS responses had matching ETag and content length.
Only those SQLite host/scheme aliases are treated as equivalent; unrelated www hosts and HTTP destinations are not assumed interchangeable.
A returned HTTPS alias is preferred without inventing a new destination.
Meaningful query parameters and section fragments remain part of reading identity; known tracking parameters and the conventional `#top` anchor are removed.

The review request now requires one result per candidate in its structured schema and prompt.
If a response nevertheless omits candidates, the available valid decisions remain usable and missing decisions retain NULL scores.
Duplicate or unknown review indices fail the review and leave candidates unreviewed, instead of publishing ambiguous decisions.
The public evaluation table distinguishes Not reviewed from a genuine zero-score rejection, and counts discovered candidates explicitly.

## Repeating a bounded evaluation

Normal `go test ./...` uses fake/local providers and skips live generation.
To deliberately spend provider credits on one bounded SQLite sample, load the existing environment through `scripts/with-env.sh` and run:

```sh
DAILYDOCS_LIVE_QUALITY=1 DAILYDOCS_LIVE_REPORT=/tmp/sqlite-quality.json \
  ./scripts/with-env.sh go test ./internal/topicsearch -run '^TestLiveSQLiteQuality$' -count=1 -v
```

The live test uses a temporary SQLite database and the standard OpenAI/Tavily endpoints.
It records public topic/search output, model usage, and run outcome, never credentials.
Do not infer broad quality, accessibility, or latency guarantees from this one sample.

## Validation

The new request-boundary regressions fail against the original handler for unknown topics, scanner-like paths, dated unknown URLs, GET `/read`, and queued-topic GETs.
The captured duplicate/ambiguous-review regressions also fail against the inherited pipeline.
They pass against the corrected implementation, alongside the existing Go test suite.
Browser tooling was unavailable during this change; functional handler/template tests ran, but an interactive visual review is not claimed.

The initial vulnerability scan on the installed Go 1.26.4 found reachable standard-library advisories
[GO-2026-6218](https://pkg.go.dev/vuln/GO-2026-6218),
[GO-2026-6091](https://pkg.go.dev/vuln/GO-2026-6091),
[GO-2026-6090](https://pkg.go.dev/vuln/GO-2026-6090),
[GO-2026-6089](https://pkg.go.dev/vuln/GO-2026-6089),
[GO-2026-5972](https://pkg.go.dev/vuln/GO-2026-5972),
[GO-2026-5856](https://pkg.go.dev/vuln/GO-2026-5856), and
[GO-2026-5026](https://pkg.go.dev/vuln/GO-2026-5026).
The Go 1.26 line is fixed at patch 1.26.6 for these advisories (1.26.5 for GO-2026-5856), consistent with the [official release history](https://go.dev/doc/devel/release#go1.26.6).
The existing `go.mod` minimum now selects Go 1.26.6 or newer through normal Go toolchain selection; no global Go installation or deployment script is replaced.
With `GOTOOLCHAIN=go1.26.6`, the vulnerability scan reports no vulnerabilities, the race-enabled Go suite passes, and the built binary reports Go 1.26.6.
