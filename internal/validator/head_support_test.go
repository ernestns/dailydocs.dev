package validator

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ernestns/daily-docs/internal/reading"
)

func TestUnsupportedHEADUsesGETBeforeDisablingReading(t *testing.T) {
	for _, tc := range []struct {
		name              string
		head, get         int
		fallback, healthy bool
	}{
		{"not implemented", http.StatusNotImplemented, http.StatusOK, true, true},
		{"method not allowed", http.StatusMethodNotAllowed, http.StatusOK, true, true},
		{"GET also broken", http.StatusNotImplemented, http.StatusNotFound, true, false},
		{"HEAD already broken", http.StatusNotFound, http.StatusOK, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openValidatorTestDB(t, ctx)
			defer conn.Close()
			const source = "https://docs.example.org/reference#transactions"
			importValidatorTopic(t, ctx, conn, source)
			if _, err := conn.Exec("INSERT INTO daily_readings(id,topic_id,reading_date,page_id) VALUES(21,1,'2026-06-26',1)"); err != nil {
				t.Fatal(err)
			}
			var heads, gets int
			client := &http.Client{Transport: validationTransport(func(r *http.Request) (*http.Response, error) {
				status := tc.head
				if r.Method == http.MethodGet {
					gets++
					status = tc.get
				} else if r.Method == http.MethodHead {
					heads++
				} else {
					t.Fatal("unexpected validation method", r.Method)
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
			})}
			for attempt := 1; attempt <= DefaultFailureThreshold; attempt++ {
				result, err := ValidateLinks(ctx, conn, client, DefaultFailureThreshold)
				if err != nil || result.Checked != 1 || result.Skipped != 0 || (result.Healthy == 1) != tc.healthy {
					t.Fatal("validation classified a method limitation as a broken reading", attempt, result, err)
				}
				if (result.Disabled == 1) != (!tc.healthy && attempt == DefaultFailureThreshold) {
					t.Fatal("failure threshold changed", attempt, result)
				}
			}
			var active, failures, archiveID int
			if err := conn.QueryRow("SELECT active,verification_failures,(SELECT id FROM daily_readings) FROM pages WHERE id=1").Scan(&active, &failures, &archiveID); err != nil {
				t.Fatal(err)
			}
			wantGets, wantFailures := 0, DefaultFailureThreshold
			if tc.fallback {
				wantGets = DefaultFailureThreshold
			}
			if tc.healthy {
				wantFailures = 0
			}
			if heads != DefaultFailureThreshold || gets != wantGets || (active == 1) != tc.healthy || failures != wantFailures || archiveID != 21 {
				t.Fatal("method fallback changed link state or history", heads, gets, active, failures, archiveID)
			}
			archive, err := reading.GetDailyReading(ctx, conn, "http", "2026-06-26")
			if err != nil || archive.PageID != 1 || archive.URL != source {
				t.Fatal("validation changed the saved reading", archive, err)
			}
			today, err := reading.GetOrCreateDailyReading(ctx, conn, "http", "2026-06-27")
			if tc.healthy && (err != nil || today.PageID != 1 || today.URL != source) || !tc.healthy && !errors.Is(err, reading.ErrNoActivePages) {
				t.Fatal("new reading availability does not match GET result", today, err)
			}
		})
	}
}
