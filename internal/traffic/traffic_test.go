package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
)

func testDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traffic.sqlite")
	conn, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, path
}
func mustExec(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func sum(t *testing.T, conn *sql.DB, kind string) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(`SELECT coalesce(sum(count),0) FROM traffic_daily WHERE kind=?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMiddlewareCapturesDocumentsWithoutSensitiveData(t *testing.T) {
	conn, _ := testDB(t)
	now := func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	c := newCollector(conn, now, time.Hour, 100)
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/missing" {
			w.WriteHeader(404)
		} else if r.URL.Path == "/redirect" {
			w.WriteHeader(303)
		}
		_, _ = w.Write([]byte("<p>hello</p>"))
	}))
	for _, p := range []string{"/", "/topics?q=secret-search", "/sqlite/2026-09-11?password=secret", "/topics/sqlite/evaluations"} {
		r := httptest.NewRequest("GET", "https://dailydocs.dev"+p, nil)
		r.Header.Set("Referer", "https://user:password@news.example/article?token=ref-secret#private")
		r.Header.Set("Cookie", "session=private-cookie")
		r.Header.Set("Authorization", "Bearer private-authorization")
		r.Header.Set("User-Agent", "ordinary-private-agent")
		r.RemoteAddr = "192.0.2.3:1111"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	for _, p := range []string{"/health", "/read?topic=secret", "/process-topic", "/topics/search?q=secret", "/topics/sqlite/status", "/missing", "/redirect", "/very/long/scanner/path"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://dailydocs.dev"+p, nil))
	}
	for _, method := range []string{"HEAD", "POST"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "https://dailydocs.dev/", nil))
	}
	r := httptest.NewRequest("GET", "https://dailydocs.dev/", nil)
	r.Header.Set("Datastar-Request", "true")
	h.ServeHTTP(httptest.NewRecorder(), r)
	r = httptest.NewRequest("GET", "https://dailydocs.dev/", nil)
	r.Header.Set("User-Agent", "ExampleBot/1.0")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := sum(t, conn, "total"); n != 5 {
		t.Fatalf("pageviews=%d, want 5", n)
	}
	rows, err := conn.Query(`SELECT kind,label,audience FROM traffic_daily`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var saved strings.Builder
	for rows.Next() {
		var k, l, a string
		if err := rows.Scan(&k, &l, &a); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&saved, k, l, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "password", "private", "192.0.2.3", "2026-09-11?", "cookie", "authorization"} {
		if strings.Contains(saved.String(), secret) {
			t.Fatalf("sensitive data persisted: %q", secret)
		}
	}
	if !strings.Contains(saved.String(), "news.example") || !strings.Contains(saved.String(), "known_bot") {
		t.Fatal(saved.String())
	}
}

func TestReferrerNormalization(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"", "(direct/unknown)"}, {"https://dailydocs.dev/page?q=private", "(internal)"},
		{"https://News.Example.:443/path?secret=value", "news.example"},
		{"https://192.0.2.1/path", otherLabel}, {"https://[2001:db8::1]/", otherLabel},
		{"javascript:alert(1)", otherLabel}, {"https://localhost", otherLabel}, {"https://bad_host.example/", otherLabel},
		{"https://bad..example/", otherLabel}, {"https://bad.-label.example/", otherLabel},
		{strings.Repeat("x", 2049), otherLabel},
	} {
		if got := referral(tt.input, "dailydocs.dev:443"); got != tt.want {
			t.Errorf("referral(%q)=%q want %q", tt.input, got, tt.want)
		}
	}
}

func TestPersistentCapsSurviveRestartAndOverflowKeepsTotals(t *testing.T) {
	conn, _ := testDB(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for restart := 0; restart < 2; restart++ {
		w := &writer{db: conn}
		for i := 0; i < 600; i++ {
			w.add(event{day: "2026-09-11", page: fmt.Sprintf("/page-%d-%d", restart, i), referrer: fmt.Sprintf("ref-%d-%d.example", restart, i), audience: "other"})
		}
		if len(w.pending) > 2*(pageLimit+referrerLimit+2)+5 {
			t.Fatalf("unbounded memory: %d", len(w.pending))
		}
		if err := w.flush(now); err != nil {
			t.Fatal(err)
		}
	}
	for kind, want := range map[string]int{"page": pageLimit + 1, "referrer": referrerLimit + 1} {
		var n int
		if err := conn.QueryRow(`SELECT count(DISTINCT label) FROM traffic_daily WHERE kind=?`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s labels=%d want %d", kind, n, want)
		}
		if got := sum(t, conn, kind); got != 1200 {
			t.Fatalf("%s total=%d", kind, got)
		}
	}
	if n := sum(t, conn, "total"); n != 1200 {
		t.Fatalf("total=%d", n)
	}
}

func TestFlushFailureRetriedAndReportedWithoutLosingCounts(t *testing.T) {
	conn, _ := testDB(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	w := &writer{db: conn}
	w.add(event{day: "2026-09-11", page: "/", referrer: "(direct/unknown)", audience: "other"})
	mustExec(t, conn, `ALTER TABLE traffic_daily RENAME TO temporarily_unavailable`)
	if err := w.flush(now); err == nil {
		t.Fatal("expected unavailable table")
	}
	if w.errors != 1 || len(w.pending) == 0 {
		t.Fatal("failure counters/batch lost")
	}
	mustExec(t, conn, `ALTER TABLE temporarily_unavailable RENAME TO traffic_daily`)
	if err := w.flush(now); err != nil {
		t.Fatal(err)
	}
	if sum(t, conn, "total") != 1 || sum(t, conn, "errors") != 1 {
		t.Fatal("retry did not preserve counts/errors")
	}
	if err := w.flush(now); err != nil {
		t.Fatal(err)
	}
	if sum(t, conn, "total") != 1 || sum(t, conn, "errors") != 1 {
		t.Fatal("retry double counted")
	}
}

func TestFailedOldDayIsBoundedAndAccountsForDroppedViews(t *testing.T) {
	conn, _ := testDB(t)
	w := &writer{db: conn}
	w.add(event{day: "2026-09-10", page: "/", referrer: "(direct/unknown)", audience: "other"})
	mustExec(t, conn, `ALTER TABLE traffic_daily RENAME TO temporarily_unavailable`)
	w.add(event{day: "2026-09-11", page: "/", referrer: "(direct/unknown)", audience: "other"})
	mustExec(t, conn, `ALTER TABLE temporarily_unavailable RENAME TO traffic_daily`)
	if err := w.flush(time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if sum(t, conn, "total") != 1 || sum(t, conn, "dropped") != 1 || sum(t, conn, "errors") != 2 {
		t.Fatal("old-day loss not accounted")
	}
}

func TestRetentionAndShutdownFlush(t *testing.T) {
	conn, _ := testDB(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	mustExec(t, conn, `INSERT INTO traffic_daily VALUES(?,?,?,?,?)`, now.AddDate(0, 0, -90).Format(time.DateOnly), "total", "", "other", 100)
	mustExec(t, conn, `INSERT INTO traffic_daily VALUES(?,?,?,?,?)`, now.AddDate(0, 0, -89).Format(time.DateOnly), "total", "", "other", 2)
	c := newCollector(conn, func() time.Time { return now }, time.Hour, 10)
	c.events <- event{day: now.Format(time.DateOnly), page: "/", referrer: "(direct/unknown)", audience: "other"}
	c.dropped.Add(3)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sum(t, conn, "total") != 3 || sum(t, conn, "dropped") != 3 {
		t.Fatal("retention/shutdown/drop counts incorrect")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOverfullChannelDoesNotBlockResponse(t *testing.T) {
	c := &Collector{events: make(chan event, 1), now: time.Now}
	c.events <- event{}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("ok"))
	}))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest("GET", "http://dailydocs.dev/", nil))
	if response.Code != 200 || response.Body.String() != "ok" || c.dropped.Load() != 1 {
		t.Fatal("overflow changed response or failed to count drop")
	}
}

func TestRestartRetainsAdmittedLabelsBeforeNewTraffic(t *testing.T) {
	for _, initial := range []int{1, pageLimit} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			conn, _ := testDB(t)
			now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			first := &writer{db: conn}
			for i := range initial {
				first.add(event{day: "2026-09-11", page: fmt.Sprintf("/saved-%d", i), referrer: fmt.Sprintf("saved-%d.example", i%referrerLimit), audience: "other"})
			}
			if err := first.flush(now); err != nil {
				t.Fatal(err)
			}
			restarted := &writer{db: conn}
			for i := range 600 {
				restarted.add(event{day: "2026-09-11", page: fmt.Sprintf("/new-%d", i), referrer: fmt.Sprintf("new-%d.example", i), audience: "other"})
			}
			restarted.add(event{day: "2026-09-11", page: "/saved-0", referrer: "saved-0.example", audience: "known_bot"})
			if err := restarted.flush(now); err != nil {
				t.Fatal(err)
			}
			for kind, label := range map[string]string{"page": "/saved-0", "referrer": "saved-0.example"} {
				var botViews int
				if err := conn.QueryRow(`SELECT count FROM traffic_daily WHERE kind=? AND label=? AND audience='known_bot'`, kind, label).Scan(&botViews); err != nil || botViews != 1 {
					t.Fatalf("retained %s label lost on restart: count=%d err=%v", kind, botViews, err)
				}
				if got := sum(t, conn, kind); got != int64(initial+601) {
					t.Fatalf("%s total=%d want=%d", kind, got, initial+601)
				}
				limit := pageLimit
				if kind == "referrer" {
					limit = referrerLimit
				}
				var labels int
				if err := conn.QueryRow(`SELECT count(DISTINCT label) FROM traffic_daily WHERE kind=? AND label != ?`, kind, otherLabel).Scan(&labels); err != nil || labels != limit {
					t.Fatal("daily admission cap changed", kind, labels, err)
				}
			}
		})
	}
}

func TestUnavailableAdmissionLabelsKeepBoundedTotalsAndRecover(t *testing.T) {
	conn, _ := testDB(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	mustExec(t, conn, `INSERT INTO traffic_daily VALUES('2026-09-11','page','/saved','other',1)`)
	mustExec(t, conn, `ALTER TABLE traffic_daily RENAME TO temporarily_unavailable`)
	w := &writer{db: conn}
	for i := range 600 {
		w.add(event{day: "2026-09-11", page: fmt.Sprintf("/new-%d", i), referrer: fmt.Sprintf("new-%d.example", i), audience: "other"})
	}
	if len(w.pending) > 4 || w.errors != 1 {
		t.Fatal("unavailable admission grew memory or retried for each event", len(w.pending), w.errors)
	}
	mustExec(t, conn, `ALTER TABLE temporarily_unavailable RENAME TO traffic_daily`)
	if err := w.flush(now); err != nil {
		t.Fatal(err)
	}
	w.add(event{day: "2026-09-11", page: "/saved", referrer: "saved.example", audience: "other"})
	if err := w.flush(now); err != nil {
		t.Fatal(err)
	}
	var saved, overflow int
	if err := conn.QueryRow(`SELECT count FROM traffic_daily WHERE kind='page' AND label='/saved'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`SELECT count FROM traffic_daily WHERE kind='page' AND label=?`, otherLabel).Scan(&overflow); err != nil {
		t.Fatal(err)
	}
	if saved != 2 || overflow != 600 || sum(t, conn, "total") != 601 || sum(t, conn, "errors") != 1 {
		t.Fatal("admission recovery lost counts or labels", saved, overflow)
	}
}
