// Package traffic records bounded daily pageview aggregates, never visitor identifiers.
package traffic

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	retentionDays = 90
	pageLimit     = 256
	referrerLimit = 128
	queueSize     = 1024
	otherLabel    = "(other)"
)

type event struct{ day, page, referrer, audience string }
type key struct{ kind, label, audience string }

type Collector struct {
	events  chan event
	stop    chan struct{}
	done    chan struct{}
	closing atomic.Bool
	dropped atomic.Int64
	once    sync.Once
	now     func() time.Time
	writer  *writer
}

type writer struct {
	db               *sql.DB
	day              string
	pending          map[key]int64
	pages, referrers map[string]bool
	errors           int64
	lastError        error
}

// New starts one writer. Requests only submit to a bounded nonblocking channel.
func New(db *sql.DB) *Collector { return newCollector(db, time.Now, 10*time.Second, queueSize) }

func newCollector(db *sql.DB, now func() time.Time, interval time.Duration, capacity int) *Collector {
	c := &Collector{events: make(chan event, capacity), stop: make(chan struct{}), done: make(chan struct{}), now: now, writer: &writer{db: db}}
	go c.run(interval)
	return c
}

func (c *Collector) run(interval time.Duration) {
	defer close(c.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	flush := func() {
		now := c.now().UTC()
		c.writer.rotate(now.Format(time.DateOnly))
		c.writer.pending[key{kind: "dropped", audience: "all"}] += c.dropped.Swap(0)
		if err := c.writer.flush(now); err != nil {
			// Never log request data or database error text, which may contain paths.
			log.Print("traffic: aggregate flush failed; bounded counters retained for retry")
		}
	}
	for {
		select {
		case e := <-c.events:
			c.writer.add(e)
		case <-ticker.C:
			flush()
		case <-c.stop:
			for {
				select {
				case e := <-c.events:
					c.writer.add(e)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close drains accepted events and makes a final bounded flush after HTTP shutdown.
func (c *Collector) Close(ctx context.Context) error {
	c.once.Do(func() { c.closing.Store(true); close(c.stop) })
	select {
	case <-c.done:
		return c.writer.lastError
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *writer) rotate(day string) {
	if w.day == day {
		return
	}
	var dropped int64
	if w.day != "" {
		// A failed previous-day batch cannot grow memory indefinitely. Account for
		// its lost pageviews on the new day, including earlier queue drops.
		_ = w.flush(timeForDay(w.day))
		for k, n := range w.pending {
			if k.kind == "total" || k.kind == "dropped" {
				dropped += n
			}
		}
	}
	w.day = day
	w.pending = make(map[key]int64)
	w.pages = make(map[string]bool)
	w.referrers = make(map[string]bool)
	w.pending[key{kind: "dropped", audience: "all"}] = dropped
}

func timeForDay(day string) time.Time { t, _ := time.Parse(time.DateOnly, day); return t }

func boundedLabel(label string, seen map[string]bool, limit int) string {
	if label == otherLabel || seen[label] {
		return label
	}
	if len(seen) >= limit {
		return otherLabel
	}
	seen[label] = true
	return label
}

func (w *writer) add(e event) {
	w.rotate(e.day)
	w.pending[key{kind: "total", audience: e.audience}]++
	page := boundedLabel(e.page, w.pages, pageLimit)
	referrer := boundedLabel(e.referrer, w.referrers, referrerLimit)
	w.pending[key{kind: "page", label: page, audience: e.audience}]++
	w.pending[key{kind: "referrer", label: referrer, audience: e.audience}]++
}

func (w *writer) flush(now time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer func() {
		w.lastError = err
		if err != nil {
			w.errors++
		}
	}()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Load admitted labels inside the same transaction, so caps survive restarts.
	pages, refs := make(map[string]bool), make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT kind, label FROM traffic_daily WHERE day = ? AND kind IN ('page','referrer') AND label != ?`, w.day, otherLabel)
	if err != nil {
		return err
	}
	for rows.Next() {
		var kind, label string
		if err = rows.Scan(&kind, &label); err != nil {
			_ = rows.Close()
			return err
		}
		if kind == "page" {
			pages[label] = true
		} else {
			refs[label] = true
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	batch := make(map[key]int64)
	for k, n := range w.pending {
		if n == 0 {
			continue
		}
		switch k.kind {
		case "page":
			k.label = boundedLabel(k.label, pages, pageLimit)
		case "referrer":
			k.label = boundedLabel(k.label, refs, referrerLimit)
		}
		batch[k] += n
	}
	batch[key{kind: "errors", audience: "all"}] += w.errors
	for k, n := range batch {
		if n == 0 {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO traffic_daily(day,kind,label,audience,count) VALUES(?,?,?,?,?) ON CONFLICT(day,kind,label,audience) DO UPDATE SET count = count + excluded.count`, w.day, k.kind, k.label, k.audience, n)
		if err != nil {
			return err
		}
	}
	stamp := now.UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_metadata(id,started_at,last_flush_at) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET last_flush_at=excluded.last_flush_at`, stamp, stamp)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM traffic_daily WHERE day < ?`, now.UTC().AddDate(0, 0, -(retentionDays-1)).Format(time.DateOnly))
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	w.pending = make(map[key]int64)
	w.pages, w.referrers = pages, refs
	w.errors = 0
	return nil
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)
var domainLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validDomain(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !domainLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func canonicalPage(path string) string {
	if path == "/" || path == "/topics" {
		return path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 3 && parts[0] == "topics" && parts[2] == "evaluations" && slugPattern.MatchString(parts[1]) {
		return "/topics/" + parts[1] + "/evaluations"
	}
	if len(parts) < 1 || len(parts) > 2 || !slugPattern.MatchString(parts[0]) {
		return ""
	}
	switch parts[0] {
	case "health", "read", "process-topic", "topics":
		return ""
	}
	if len(parts) == 2 {
		if _, err := time.Parse(time.DateOnly, parts[1]); err != nil {
			return ""
		}
	}
	return "/" + parts[0]
}

func referral(raw, requestHost string) string {
	if raw == "" {
		return "(direct/unknown)"
	}
	if len(raw) > 2048 {
		return otherLabel
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return otherLabel
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || net.ParseIP(host) != nil || !validDomain(host) {
		return otherLabel
	}
	current := strings.ToLower(requestHost)
	if h, _, err := net.SplitHostPort(current); err == nil {
		current = h
	}
	if host == strings.TrimSuffix(current, ".") {
		return "(internal)"
	}
	return host
}

func audience(ua string) string {
	if len(ua) > 512 {
		ua = ua[:512]
	}
	ua = strings.ToLower(ua)
	for _, token := range []string{"bot", "crawler", "spider", "headless", "curl/", "wget/", "python-requests/"} {
		if strings.Contains(ua, token) {
			return "known_bot"
		}
	}
	return "other"
}

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseStatus) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseStatus) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (c *Collector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := canonicalPage(r.URL.Path)
		if r.Method != http.MethodGet || page == "" || r.Header.Get("Datastar-Request") != "" {
			next.ServeHTTP(w, r)
			return
		}
		sw := &responseStatus{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || c.closing.Load() {
			return
		}
		e := event{day: c.now().UTC().Format(time.DateOnly), page: page, referrer: referral(r.Referer(), r.Host), audience: audience(r.UserAgent())}
		select {
		case c.events <- e:
		default:
			c.dropped.Add(1)
		}
	})
}

var errNoTraffic = errors.New("traffic history is unavailable; collection begins after the analytics release starts")
