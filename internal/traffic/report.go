package traffic

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Daily struct {
	Day                          string
	Other, Bots, Dropped, Errors int64
}
type Ranked struct {
	Label       string
	Other, Bots int64
}
type Report struct {
	Days                             int
	Since, Until, Started, LastFlush string
	Other, Bots, Dropped, Errors     int64
	Daily                            []Daily
	Pages, Referrers                 []Ranked
}

// OpenReadOnly never creates a database, applies migrations, or changes pragmas
// on a writer connection. Even an accidental write on the returned handle fails.
func OpenReadOnly(path string) (*sql.DB, error) {
	if path == "" {
		path = "data/dailydocs.sqlite"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("traffic report requires an existing regular SQLite file")
	}
	u := url.URL{Scheme: "file", Path: abs}
	values := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)"}}
	u.RawQuery = values.Encode()
	conn, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

func ReadReport(ctx context.Context, conn *sql.DB, days int, now time.Time) (Report, error) {
	if days < 1 || days > retentionDays {
		return Report{}, fmt.Errorf("days must be between 1 and %d", retentionDays)
	}
	now = now.UTC()
	r := Report{Days: days, Since: now.AddDate(0, 0, -(days - 1)).Format(time.DateOnly), Until: now.Format(time.DateOnly)}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return r, err
	}
	defer func() { _ = tx.Rollback() }()
	var hasTable int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='traffic_metadata'`).Scan(&hasTable); err != nil {
		return r, err
	}
	if hasTable == 0 {
		return r, errNoTraffic
	}
	err = tx.QueryRowContext(ctx, `SELECT started_at,last_flush_at FROM traffic_metadata WHERE id=1`).Scan(&r.Started, &r.LastFlush)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNoTraffic
	}
	if err != nil {
		return r, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT day,
		coalesce(sum(CASE WHEN kind='total' AND audience='other' THEN count ELSE 0 END),0),
		coalesce(sum(CASE WHEN kind='total' AND audience='known_bot' THEN count ELSE 0 END),0),
		coalesce(sum(CASE WHEN kind='dropped' THEN count ELSE 0 END),0),
		coalesce(sum(CASE WHEN kind='errors' THEN count ELSE 0 END),0)
		FROM traffic_daily WHERE day BETWEEN ? AND ? AND kind IN ('total','dropped','errors') GROUP BY day ORDER BY day DESC`, r.Since, r.Until)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var d Daily
		if err = rows.Scan(&d.Day, &d.Other, &d.Bots, &d.Dropped, &d.Errors); err != nil {
			_ = rows.Close()
			return r, err
		}
		r.Daily = append(r.Daily, d)
		r.Other += d.Other
		r.Bots += d.Bots
		r.Dropped += d.Dropped
		r.Errors += d.Errors
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return r, err
	}
	if err = rows.Close(); err != nil {
		return r, err
	}
	rank := func(kind string) ([]Ranked, error) {
		rows, err := tx.QueryContext(ctx, `SELECT label,
		coalesce(sum(CASE WHEN audience='other' THEN count ELSE 0 END),0),
		coalesce(sum(CASE WHEN audience='known_bot' THEN count ELSE 0 END),0)
		FROM traffic_daily WHERE day BETWEEN ? AND ? AND kind=? GROUP BY label ORDER BY sum(count) DESC,label LIMIT 20`, r.Since, r.Until, kind)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var values []Ranked
		for rows.Next() {
			var v Ranked
			if err = rows.Scan(&v.Label, &v.Other, &v.Bots); err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return values, rows.Err()
	}
	if r.Pages, err = rank("page"); err != nil {
		return r, err
	}
	if r.Referrers, err = rank("referrer"); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// RunReport writes escaped, self-contained HTML without exposing a web endpoint.
func RunReport(ctx context.Context, args []string, path string, output io.Writer) error {
	fs := flag.NewFlagSet("traffic-report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	days := fs.Int("days", 30, "days to include, 1–90")
	format := fs.String("format", "html", "output format (html)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("traffic-report: %w", err)
	}
	if fs.NArg() != 0 || *format != "html" {
		return errors.New("usage: dailydocs traffic-report --days 30 --format html")
	}
	conn, err := OpenReadOnly(path)
	if err != nil {
		return fmt.Errorf("open traffic report database: %w", err)
	}
	defer conn.Close()
	r, err := ReadReport(ctx, conn, *days, time.Now())
	if err != nil {
		return err
	}
	return reportTemplate.Execute(output, r)
}

var reportTemplate = template.Must(template.New("traffic").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'">
<title>DailyDocs private traffic report</title><style>
body{margin:0;background:#f7f8fa;color:#1f2933;font:16px system-ui,sans-serif}main{max-width:960px;margin:auto;padding:32px 20px}h1{margin-bottom:8px}h2{margin-top:32px;font-size:21px}p{line-height:1.6;color:#52606d}.cards{display:flex;gap:16px;flex-wrap:wrap}.card{background:white;border:1px solid #d9e2ec;border-radius:8px;padding:18px;flex:1;min-width:170px}.number{font-size:32px;font-weight:700} .table-scroll{overflow-x:auto}table{width:100%;border-collapse:collapse;background:white}th,td{text-align:left;border-bottom:1px solid #d9e2ec;padding:12px;overflow-wrap:anywhere}th{font-size:13px;color:#52606d}code{overflow-wrap:anywhere}.notice{border-left:4px solid #8d6c13;padding-left:14px}footer{margin-top:32px;font-size:14px}
</style></head><body><main><h1>DailyDocs traffic</h1><p>Private report · {{.Since}} to {{.Until}} UTC · {{.Days}} days</p>
<div class="cards"><div class="card"><div class="number">{{.Other}}</div>Other pageviews</div><div class="card"><div class="number">{{.Bots}}</div>Known-bot pageviews</div><div class="card"><div class="number">{{.Dropped}}</div>Dropped pageviews</div></div>
<p>These are page requests, not unique people or sessions. “Other” includes unrecognized bots. No country, campaign, cookie, IP address or visitor identifier is collected.</p>
<p>Collection started {{.Started}}. Last successful flush {{.LastFlush}}. Historical traffic before collection is unavailable.</p>
{{if or .Dropped .Errors}}<p class="notice">Collection is incomplete: {{.Dropped}} pageviews dropped and {{.Errors}} writer errors recorded in this period. Temporary failures are retried; data that could not be retained is excluded from pageview totals.</p>{{end}}
<h2>Daily pageviews</h2><div class="table-scroll" tabindex="0" role="region" aria-label="Daily pageviews"><table><thead><tr><th>UTC day</th><th>Other</th><th>Known bots</th><th>Dropped</th><th>Writer errors</th></tr></thead><tbody>{{range .Daily}}<tr><td>{{.Day}}</td><td>{{.Other}}</td><td>{{.Bots}}</td><td>{{.Dropped}}</td><td>{{.Errors}}</td></tr>{{else}}<tr><td colspan="5">No recorded pageviews in this period.</td></tr>{{end}}</tbody></table></div>
<h2>Top pages</h2><div class="table-scroll" tabindex="0" role="region" aria-label="Top pages"><table><thead><tr><th>Canonical page</th><th>Other</th><th>Known bots</th></tr></thead><tbody>{{range .Pages}}<tr><td><code>{{.Label}}</code></td><td>{{.Other}}</td><td>{{.Bots}}</td></tr>{{else}}<tr><td colspan="3">No page data.</td></tr>{{end}}</tbody></table></div>
<h2>Referral domains</h2><div class="table-scroll" tabindex="0" role="region" aria-label="Referral domains"><table><thead><tr><th>Domain</th><th>Other</th><th>Known bots</th></tr></thead><tbody>{{range .Referrers}}<tr><td>{{.Label}}</td><td>{{.Other}}</td><td>{{.Bots}}</td></tr>{{else}}<tr><td colspan="3">No referral data.</td></tr>{{end}}</tbody></table></div>
<p>Referrals describe the header on each page request. Direct/unknown includes missing or suppressed referrers; internal means navigation within DailyDocs. This is not session acquisition attribution. Browser policies and bots can omit or forge headers.</p>
<footer>Only the top 20 rows are shown. At most 256 page labels and 128 referral labels are retained per UTC day, with excess or rejected values grouped as (other). Date-specific reading URLs share their topic page. Retention is 90 days. Up to 10 seconds of buffered activity can be lost on an unexpected process exit; graceful shutdown flushes accepted events. Re-run <code>just traffic 7</code>, <code>just traffic 30</code>, or <code>just traffic 90</code> with REMOTE set to change the period.</footer>
</main></body></html>`))
