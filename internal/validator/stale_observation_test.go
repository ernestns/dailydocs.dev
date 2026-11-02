package validator

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type validationTransport func(*http.Request) (*http.Response, error)

func (f validationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func validationResponse(r *http.Request, status int, location string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: r}
}

func TestValidationSkipsObservationsForChangedPages(t *testing.T) {
	for _, change := range []string{"redirected", "deactivated", "deleted"} {
		for _, observation := range []string{"healthy-redirect", "failure"} {
			t.Run(change+"/"+observation, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				conn := openValidatorTestDB(t, ctx)
				defer conn.Close()
				const oldURL = "https://docs.example.org/old"
				const currentURL = "https://docs.example.org/current"
				const obsoleteURL = "https://docs.example.org/obsolete"
				importValidatorTopic(t, ctx, conn, oldURL)
				var pageID, topicID int64
				if err := conn.QueryRow(`SELECT id,topic_id FROM pages`).Scan(&pageID, &topicID); err != nil {
					t.Fatal(err)
				}
				var laterID int64
				if err := conn.QueryRow(`INSERT INTO pages(topic_id,title,url,reading_order) VALUES(?,'Later guide','https://docs.example.org/later',2) RETURNING id`, topicID).Scan(&laterID); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Exec(`INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(?,'2026-01-02',?)`, topicID, laterID); err != nil {
					t.Fatal(err)
				}
				// Only an unassigned page can be deleted. Redirect/deactivation cases
				// also exercise history that must retain the first page's identity.
				if change != "deleted" {
					if _, err := conn.Exec(`INSERT INTO daily_readings(topic_id,reading_date,page_id) VALUES(?,'2026-01-01',?)`, topicID, pageID); err != nil {
						t.Fatal(err)
					}
				}
				started := make(chan struct{})
				release := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				slow := &http.Client{Transport: validationTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() == oldURL {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						if observation == "failure" {
							return validationResponse(r, http.StatusNotFound, ""), nil
						}
						return validationResponse(r, http.StatusMovedPermanently, obsoleteURL), nil
					}
					return validationResponse(r, http.StatusOK, ""), nil
				})}
				type outcome struct {
					result Result
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := ValidateLinks(ctx, conn, slow, 1)
					done <- outcome{result, err}
				}()
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("first validator did not start")
				}
				switch change {
				case "redirected":
					// A second real validation finishes while the first HTTP request
					// waits, saving a newer healthy destination for the same page ID.
					current := &http.Client{Transport: validationTransport(func(r *http.Request) (*http.Response, error) {
						if r.URL.String() == oldURL {
							return validationResponse(r, http.StatusMovedPermanently, currentURL), nil
						}
						return validationResponse(r, http.StatusOK, ""), nil
					})}
					result, err := ValidateLinks(ctx, conn, current, 1)
					if err != nil || result != (Result{Checked: 2, Healthy: 2}) {
						t.Fatalf("new validator: %+v %v", result, err)
					}
				case "deactivated":
					if _, err := conn.Exec(`UPDATE pages SET active=0,verification_failures=2,last_error='retired',updated_at='2020-01-01 00:00:00' WHERE id=?`, pageID); err != nil {
						t.Fatal(err)
					}
				case "deleted":
					if _, err := conn.Exec(`DELETE FROM pages WHERE id=?`, pageID); err != nil {
						t.Fatal(err)
					}
				}
				var expected validationPage
				if change != "deleted" {
					expected = readValidationPage(t, conn, pageID)
					if change == "redirected" && expected.url != currentURL {
						t.Fatal("second validator did not save the new destination", expected)
					}
				}
				close(release)
				finished := <-done
				if finished.err != nil || finished.result != (Result{Checked: 2, Healthy: 1, Skipped: 1}) {
					t.Fatalf("stale observation must be skipped and later page checked: %+v %v", finished.result, finished.err)
				}
				if change != "deleted" {
					if got := readValidationPage(t, conn, pageID); got != expected {
						t.Fatalf("stale result changed current page: got=%+v want=%+v", got, expected)
					}
					var assignedID int64
					if err := conn.QueryRow(`SELECT page_id FROM daily_readings WHERE reading_date='2026-01-01'`).Scan(&assignedID); err != nil || assignedID != pageID {
						t.Fatal("historical first-page assignment changed", assignedID, err)
					}
				} else {
					var remaining int
					if err := conn.QueryRow(`SELECT count(*) FROM pages WHERE id=?`, pageID).Scan(&remaining); err != nil || remaining != 0 {
						t.Fatal("deleted page reappeared", remaining, err)
					}
				}
				later := readValidationPage(t, conn, laterID)
				if later.active != 1 || later.failures != 0 || later.verified == "" {
					t.Fatalf("later page was not verified: %+v", later)
				}
				var assignedID int64
				if err := conn.QueryRow(`SELECT page_id FROM daily_readings WHERE reading_date='2026-01-02'`).Scan(&assignedID); err != nil || assignedID != laterID {
					t.Fatal("historical later-page assignment changed", assignedID, err)
				}
			})
		}
	}
}

type validationPage struct {
	url, verified, lastError, updated string
	active, failures                  int
}

func readValidationPage(t *testing.T, conn *sql.DB, id int64) validationPage {
	t.Helper()
	var page validationPage
	if err := conn.QueryRow(`SELECT url,active,verification_failures,coalesce(last_verified,''),last_error,updated_at FROM pages WHERE id=?`, id).Scan(&page.url, &page.active, &page.failures, &page.verified, &page.lastError, &page.updated); err != nil {
		t.Fatal(err)
	}
	return page
}
