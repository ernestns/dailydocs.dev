package validator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestValidationRedirectsPreserveReadingSections(t *testing.T) {
	for _, tc := range []struct {
		name, start, first, second, want string
		fallback                         bool
	}{
		{"inherited section", "/old#transactions", "/new", "", "/new#transactions", false},
		{"escaped section", "/old#part%2Fone", "/new", "", "/new#part%2Fone", false},
		{"replacement section", "/old#transactions", "/new#locking", "", "/new#locking", false},
		{"cleared section", "/old#transactions", "/new#", "", "/new", false},
		{"bare cleared section", "/old?edition=2#part%2Fone", "#", "", "/old?edition=2", false},
		{"bare replacement section", "/old?edition=2#transactions", "#locking", "", "/old?edition=2#locking", false},
		{"two inherited hops", "/old#transactions", "/middle", "/new", "/new#transactions", false},
		{"replacement then inheritance", "/old#transactions", "/middle#locking", "/new", "/new#locking", false},
		{"clear then inheritance", "/old#transactions", "/middle#", "/new", "/new", false},
		{"introduced then inheritance", "/old", "/middle#locking", "/new", "/new#locking", false},
		{"encoded hash in path", "/old#transactions", "/new%23part", "", "/new%23part#transactions", false},
		{"HEAD fallback", "/old#transactions", "/new", "", "/new#transactions", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openValidatorTestDB(t, ctx)
			defer conn.Close()
			const origin = "https://docs.example.org"
			importValidatorTopic(t, ctx, conn, origin+tc.start)
			if _, err := conn.Exec("INSERT INTO daily_readings(topic_id,reading_date,page_id) SELECT topic_id,'2026-06-26',id FROM pages"); err != nil {
				t.Fatal(err)
			}
			var getCalls int
			hops := make(map[string]int)
			client := &http.Client{Transport: validationTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					getCalls++
				}
				status, headers := http.StatusOK, http.Header{}
				hop := hops[r.Method]
				hops[r.Method]++
				switch {
				case hop == 0:
					status = http.StatusMovedPermanently
					headers.Set("Location", tc.first)
				case hop == 1 && tc.second != "":
					status = http.StatusTemporaryRedirect
					headers.Set("Location", tc.second)
				default:
					if tc.fallback && r.Method == http.MethodHead {
						status = http.StatusMethodNotAllowed
					}
				}
				return &http.Response{StatusCode: status, Header: headers, Body: http.NoBody, Request: r}, nil
			})}
			result, err := ValidateLinks(ctx, conn, client, 3)
			if err != nil || result.Healthy != 1 || result.Failed != 0 || result.Skipped != 0 {
				t.Fatal("redirect validation failed", result, err)
			}
			var destination string
			var pageID, assignments, failures int
			if err := conn.QueryRow(`SELECT p.id,p.url,p.verification_failures,(SELECT count(*) FROM daily_readings)
				FROM pages p JOIN daily_readings d ON d.page_id=p.id`).Scan(&pageID, &destination, &failures, &assignments); err != nil {
				t.Fatal(err)
			}
			if pageID != 1 || assignments != 1 || failures != 0 || destination != origin+tc.want {
				t.Fatal("validation changed the saved section or assignment", pageID, destination, failures, assignments)
			}
			if tc.fallback != (getCalls > 0) || client.CheckRedirect != nil {
				t.Fatal("changed fallback behavior or mutated supplied client", getCalls)
			}
		})
	}
}

func TestReadingRedirectPolicyAndLimitRemainIntact(t *testing.T) {
	ctx := context.Background()
	redirect := validationTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"/again"}}, Body: http.NoBody, Request: r}, nil
	})
	policyErr := errors.New("fixture redirect policy")
	called := 0
	client := &http.Client{Transport: redirect, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		called++
		if next.URL.Fragment != "section" || len(via) != 1 {
			t.Error("caller policy did not see the reading destination", next.URL, len(via))
		}
		return policyErr
	}}
	if _, err := checkURL(ctx, client, "https://docs.example.org/start#section"); !errors.Is(err, policyErr) || called != 1 {
		t.Fatal("caller redirect policy was ignored", called, err)
	}
	lastResponse := &http.Client{Transport: redirect, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if got, err := checkURL(ctx, lastResponse, "https://docs.example.org/start#section"); err != nil || got != "https://docs.example.org/start#section" {
		t.Fatal("last-response policy changed", got, err)
	}
	var requests int
	limited := &http.Client{Transport: validationTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return redirect.RoundTrip(r)
	})}
	if _, err := checkURL(ctx, limited, "https://docs.example.org/start#section"); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") || requests != 10 {
		t.Fatal("default redirect limit changed", requests, err)
	}
}
