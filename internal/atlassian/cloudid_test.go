package atlassian_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
)

const siteID = "0f3a6c1e-0000-4000-8000-00000000c0de"

// clock is a settable now.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// site serves /_edge/tenant_info with answer, counting the asks.
func site(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var asks atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_edge/tenant_info" {
			http.NotFound(w, r)
			return
		}
		asks.Add(1)
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &asks
}

// A SITE'S CLOUD ID IS READ ONCE FOR THE PROCESS: it never changes, so one
// lookup serves every event. Mutation: drop the kept success and asks goes
// to 3.
func TestASitesCloudIDIsReadOnce(t *testing.T) {
	t.Parallel()
	srv, asks := site(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cloudId":"` + strings.ToUpper(siteID) + `"}`))
	})
	ids := atlassian.NewCloudIDs(atlassian.CloudIDOptions{Client: srv.Client()})
	for _, address := range []string{srv.URL, srv.URL + "/wiki", srv.URL + "/"} {
		id, err := ids.Resolve(t.Context(), address)
		if err != nil || id != siteID {
			t.Fatalf("Resolve(%s) = %q, %v; want %q", address, id, err, siteID)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("%d lookups for one site, want 1", n)
	}
}

// A FAILURE IS RETRIED, BUT NOT BY EVERY EVENT: inside the floor the last
// error answers, past it the site is asked again. Mutation: keep a failure
// for good, or drop the floor, and a check goes red.
func TestAFailedLookupIsRetriedAfterTheFloor(t *testing.T) {
	t.Parallel()
	var down atomic.Bool
	down.Store(true)
	srv, asks := site(t, func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"cloudId":"` + siteID + `"}`))
	})
	now := &clock{at: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	ids := atlassian.NewCloudIDs(atlassian.CloudIDOptions{Client: srv.Client(), Now: now.now})

	if _, err := ids.Resolve(t.Context(), srv.URL); err == nil {
		t.Fatal("a 502 answered a cloud id")
	}
	down.Store(false)
	if _, err := ids.Resolve(t.Context(), srv.URL); err == nil || asks.Load() != 1 {
		t.Errorf("inside the floor: err %v after %d asks, want the last error and 1 ask", err, asks.Load())
	}
	now.advance(atlassian.TenantInfoRetryFloor)
	if id, err := ids.Resolve(t.Context(), srv.URL); err != nil || id != siteID {
		t.Errorf("past the floor: %q, %v; want the site asked again", id, err)
	}
}

// THE ANSWER IS BOUNDED AND VALIDATED: only a JSON object naming a cloud id
// of the right shape, within the size cap, in time, is one. Mutation: drop
// the cap, the shape check or the empty check and a case goes red.
func TestOnlyAWellFormedAnswerIsACloudID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, cause string
		status            int
	}{
		{"oversized", `{"cloudId":"` + siteID + `","pad":"` + strings.Repeat("x", atlassian.TenantInfoMaxBytes) + `"}`,
			"over 4096 bytes", http.StatusOK},
		{"not an object", `["` + siteID + `"]`, "not a JSON object", http.StatusOK},
		{"a number", `{"cloudId":7}`, "not a JSON object", http.StatusOK},
		{"no cloud id", `{"other":"x"}`, "names no cloudId", http.StatusOK},
		{"an empty id", `{"cloudId":"  "}`, "names no cloudId", http.StatusOK},
		{"not a cloud id", `{"cloudId":"../../etc"}`, "is not a cloud id", http.StatusOK},
		{"an error status", `{"cloudId":"` + siteID + `"}`, "HTTP 404", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := site(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			ids := atlassian.NewCloudIDs(atlassian.CloudIDOptions{Client: srv.Client()})
			id, err := ids.Resolve(t.Context(), srv.URL)
			var lookup *atlassian.TenantInfoError
			if err == nil || !errors.As(err, &lookup) || !strings.Contains(lookup.Cause, tc.cause) {
				t.Fatalf("Resolve = %q, %v; want a refusal naming %q", id, err, tc.cause)
			}
			if !strings.Contains(err.Error(), srv.URL+"/_edge/tenant_info") {
				t.Errorf("%v does not name the address asked", err)
			}
		})
	}
	t.Run("no answer in time", func(t *testing.T) {
		t.Parallel()
		srv, _ := site(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		client := srv.Client()
		client.Timeout = time.Millisecond
		ids := atlassian.NewCloudIDs(atlassian.CloudIDOptions{Client: client})
		if _, err := ids.Resolve(t.Context(), srv.URL); err == nil || !strings.Contains(err.Error(), "no answer within") {
			t.Fatalf("Resolve err = %v, want a timeout named", err)
		}
	})
}

// A REDIRECT YIELDS NO CLOUD ID: the id must come from the site itself, so a
// 3xx is a refusal and wherever it points is never asked. Mutation: let the
// client follow redirects and the second server is asked and its id kept.
func TestARedirectedLookupYieldsNoCloudID(t *testing.T) {
	t.Parallel()
	elsewhere, asked := site(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cloudId":"` + siteID + `"}`))
	})
	srv, _ := site(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/_edge/tenant_info", http.StatusFound)
	})
	ids := atlassian.NewCloudIDs(atlassian.CloudIDOptions{Client: srv.Client()})
	id, err := ids.Resolve(t.Context(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Resolve = %q, %v; want a refusal naming HTTP 302", id, err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the redirect's target was asked %d times, want 0", n)
	}
}

// transport answers each request with answer, counting them.
type transport struct {
	calls  atomic.Int64
	answer func(n int64, r *http.Request) (*http.Response, error)
}

func (f *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.answer(f.calls.Add(1), r)
}

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

const acmeSite = "https://acme.atlassian.net"

// A KEPT CLOUD ID IS NEVER ASKED AGAIN NOR OVERWRITTEN by a later lookup of
// the same site: a lookup that finds one kept answers it, and one that fails
// while another stored a success leaves that success. Flight 1 is held open
// on a gate while a second lookup stores the id. Mutation: drop either
// check in fly and a check goes red.
func TestAKeptCloudIDIsNeverOverwritten(t *testing.T) {
	t.Parallel()
	arrived, gate := make(chan struct{}), make(chan struct{})
	rt := &transport{answer: func(n int64, _ *http.Request) (*http.Response, error) {
		if n == 1 {
			close(arrived)
			<-gate
			return reply(http.StatusBadGateway, "busy"), nil
		}
		return reply(http.StatusOK, `{"cloudId":"`+siteID+`"}`), nil
	}}
	ids := atlassian.NewCloudIDs(atlassian.CloudIDOptions{Client: &http.Client{Transport: rt}})

	done := make(chan error, 1)
	go func() {
		_, err := atlassian.FlyForTest(ids, context.Background(), acmeSite)
		done <- err
	}()
	<-arrived
	if id, err := ids.Resolve(t.Context(), acmeSite); err != nil || id != siteID {
		t.Fatalf("Resolve = %q, %v while flight 1 is held", id, err)
	}
	close(gate)
	<-done
	if id, err := ids.Resolve(t.Context(), acmeSite); err != nil || id != siteID {
		t.Errorf("after flight 1 failed: %q, %v; want the kept id", id, err)
	}

	before := rt.calls.Load()
	if id, err := atlassian.FlyForTest(ids, t.Context(), acmeSite); err != nil || id != siteID {
		t.Errorf("a lookup with an id kept = %q, %v; want the kept id", id, err)
	}
	if rt.calls.Load() != before {
		t.Error("a lookup asked the site for an id already kept")
	}
}
