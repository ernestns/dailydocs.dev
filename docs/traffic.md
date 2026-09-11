# Private traffic reports

DailyDocs records daily pageview aggregates locally after the analytics-enabled release starts.
It does not reconstruct older traffic from topic requests or daily-reading assignments.

## View the report

Use a clean checkout containing the analytics release; an older dirty checkout intentionally left unchanged will not yet contain this recipe.
With the existing VM SSH key unlocked and host key already trusted:

```sh
REMOTE=root@dailydocs.dev just traffic 30
```

Use `7`, `30`, or `90` for common periods; any integer from 1 through 90 is supported.
The command reads the production SQLite file through the deployed `traffic-report` CLI, saves a private self-contained HTML file under `.cache/traffic/report-*/index.html`, and opens it with the available desktop opener.
It prints the path even when no opener is available.
Set `TRAFFIC_OPEN=0` to save without opening.
Keep `REMOTE` in the existing local `.env` if desired.
No token is added to a URL, browser history, or command argument; the unused `ADMIN_TOKEN` remains unchanged.
There is no public analytics endpoint or new login.

The direct equivalent works from any local directory and does not depend on the SSH account's remote working directory:

```sh
umask 077
ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10 root@dailydocs.dev \
  'DB_PATH=/opt/dailydocs/data/dailydocs.sqlite /opt/dailydocs/bin/dailydocs traffic-report --days 30 --format html' \
  > dailydocs-traffic.html
```

Open the local HTML file in a browser.
Use the account already authorized to read the app database; this feature adds no SSH or sudo permission.
Saved local reports are private snapshots and remain until the operator removes them; the server's 90-day retention does not delete downloaded copies.

## What the numbers mean

A pageview is a successful HTML document GET for the home page, topic index, topic reading/request page, or topic evaluations page.
Health checks, autocomplete, status polling, Datastar fragments, POST/HEAD requests, redirects and failed requests are excluded.
A dated reading path shares the canonical topic page.
Query strings are ignored, including search/filter parameters.

Known-bot pageviews are shown separately using a small user-agent heuristic.
The Other pageviews category can contain unrecognized or spoofed bots; it is not a human count.
There are no unique visitors, sessions, country estimates or campaign dimensions.
No IP, visitor/session identifier, cookie, raw user agent, full URL, referrer path or referrer query is stored.

Referrer domains describe the header on each page request.
Missing referrers are direct/unknown; same-site referrals are internal.
Browser policies can omit or reduce the header, and clients can forge it.
This is not attribution of an entire visit to its first source.
Only valid HTTP(S) domain names are retained; IP-valued, malformed and oversized referrers enter the other bucket.

The report shows the top 20 pages and referrers for the selected UTC date range.
Each UTC day retains at most 256 page labels and 128 referral labels, plus an other bucket for excess values.
Caps persist across process restarts; totals still include overflow pageviews.
These limits protect against scanner paths and fabricated referral domains, but deliberate flooding can reduce detail.

## Storage and failure behavior

The existing SQLite database stores daily counters, not individual request events.
One writer drains a nonblocking channel of at most 1,024 events and flushes bounded batches every 10 seconds.
Normal request handlers perform no analytics SQL and create no analytics goroutine.
The web process uses one shared SQLite pool connection so an analytics commit cannot invalidate a concurrent read-then-write daily-reading transaction; provider calls do not hold a database transaction. This serializes only in-process database work, leaves other CLI connection behavior unchanged, and does not coordinate unrelated external writers. The regression is [TestTrafficFlushCannotInvalidateAnApplicationWriteTransaction](../cmd/web/traffic_test.go).
Daily label caps also bound pending memory and database rows.
Rows older than the current UTC day plus 89 previous days are removed during flushing.

Queue overflow increments a dropped-pageview counter.
Database flush failures are retried with a bounded pending batch, counted, and logged without request data; they never change an HTTP response.
If a failed batch cannot be stored before moving to another UTC day, its pageviews become reported drops rather than growing memory indefinitely.
The report exposes persisted drops, writer errors and last successful flush; a prolonged storage failure cannot persist its own counters until storage recovers.
Graceful shutdown drains the queue and attempts a final bounded flush.
An unexpected process exit can lose queued events and all unflushed counters. Normally this is activity since the last 10-second flush, but repeated storage failures can extend that interval; channel and label caps bound memory, not the age of buffered activity. This is lightweight operational analytics, not billing-grade accounting.

`traffic-report` uses a dedicated `mode=ro` and `query_only` connection against an existing file.
It performs no migration, schema creation, pruning, provider request or credential load.
Before the first successful collection flush it exits with a history-unavailable message; there is no HTML report yet.
The HTML escapes stored labels and contains no external resources or scripts.
