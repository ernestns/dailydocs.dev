package traffic

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportConnectionReallyReadOnlyAndNeverCreates(t *testing.T) {
	_, path := testDB(t)
	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.Exec(`CREATE TABLE forbidden(id)`); err == nil {
		t.Fatal("read-only report allowed write")
	}
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err = OpenReadOnly(missing); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("report created missing database")
	}
}

func TestReportEscapesLabelsAndHasNoExternalResources(t *testing.T) {
	conn, path := testDB(t)
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	mustExec(t, conn, `INSERT INTO traffic_metadata VALUES(1,?,?)`, now.Format(time.RFC3339), now.Format(time.RFC3339))
	for _, row := range []struct {
		k, l, a string
		n       int
	}{{"total", "", "other", 5}, {"total", "", "known_bot", 2}, {"dropped", "", "all", 1}, {"errors", "", "all", 3}, {"page", "/<script>alert('x')</script>", "other", 5}, {"referrer", "<img src=x onerror=alert(1)>", "known_bot", 2}} {
		mustExec(t, conn, `INSERT INTO traffic_daily VALUES(?,?,?,?,?)`, day, row.k, row.l, row.a, row.n)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	r, err := ReadReport(context.Background(), ro, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Other != 5 || r.Bots != 2 || r.Dropped != 1 || r.Errors != 3 {
		t.Fatalf("wrong totals: %+v", r)
	}
	var buf bytes.Buffer
	if err = RunReport(context.Background(), []string{"--days", "30", "--format", "html"}, path, &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, forbidden := range []string{"<script>", "<img ", "src=\"http", "href=\"http"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("unsafe content %q", forbidden)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "not unique people", "Collection is incomplete", "Private report", "Top pages", "Referral domains"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestReportMissingHistoryAndInvalidDays(t *testing.T) {
	conn, _ := testDB(t)
	if _, err := ReadReport(context.Background(), conn, 30, time.Now()); !errors.Is(err, errNoTraffic) {
		t.Fatalf("missing history error=%v", err)
	}
	for _, days := range []int{0, -1, 91} {
		if _, err := ReadReport(context.Background(), conn, days, time.Now()); err == nil {
			t.Fatalf("accepted days=%d", days)
		}
	}
	mustExec(t, conn, `DROP TABLE traffic_metadata`)
	if _, err := ReadReport(context.Background(), conn, 30, time.Now()); !errors.Is(err, errNoTraffic) {
		t.Fatalf("unmigrated db error=%v", err)
	}
}
