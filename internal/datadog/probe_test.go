package datadog_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// The fake keys every case types. Obvious fakes, never a real key's shape.
const (
	fakeAPIKey = "fake-api-key-0000"
	fakeAppKey = "fake-app-key-1111"
)

// The routes the staged check reads, in the order it reads them.
const (
	validatePath = "/api/v1/validate"
	orgPath      = "/api/v1/org"
	usersPath    = "/api/v2/users"
	rolesPath    = "/api/v2/roles"
	webhookPath  = "/api/v1/integration/webhooks/configuration/webhooks/crewlet"
)

// acceptingDatadog answers every stage of the check as a working pair would.
func acceptingDatadog(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case validatePath:
		if r.Header.Get("DD-API-KEY") == "" {
			forbidden(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"valid":true}`))
		return
	case orgPath:
		if r.Header.Get("DD-API-KEY") == "" || r.Header.Get("DD-APPLICATION-KEY") == "" {
			forbidden(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"orgs":[{"name":"FairSplit","public_id":"abc"}]}`))
		return
	case usersPath, rolesPath:
		_, _ = w.Write([]byte(`{"data":[]}`))
		return
	case webhookPath:
		_, _ = w.Write([]byte(`{"name":"crewlet","url":"https://example.invalid"}`))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func forbidden(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
}

func datadogVendor(t *testing.T) setuptest.Vendor {
	return setuptest.Vendor{
		App:          "Datadog",
		Requirements: datadog.Requirements(nil, nil),
		Probe: func(srv *httptest.Server) setup.Probe {
			return datadog.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})
		},
		Values: func(*httptest.Server) map[string]string {
			return map[string]string{"site": "datadoghq.eu", "api_key": fakeAPIKey, "app_key": fakeAppKey}
		},
		Accept: acceptingDatadog,
		Refuse: forbidden,
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
}

// THE SUITE every vendor's probe passes.
func TestTheDatadogProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, datadogVendor(t))
}

// fakeDatadog is an accepting Datadog with one route answered by override,
// that records every request it was sent.
type fakeDatadog struct {
	mu   sync.Mutex
	seen []*http.Request
}

func (f *fakeDatadog) serve(path string, override http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		f.mu.Unlock()
		if r.URL.Path == path && override != nil {
			override(w, r)
			return
		}
		acceptingDatadog(w, r)
	}
}

func (f *fakeDatadog) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.seen...)
}

// checkAgainst runs the one Datadog check against a fake region whose path
// answers with override, and every other route as a working pair.
func checkAgainst(
	t *testing.T, ctx context.Context, path string, override http.HandlerFunc,
) (setup.Verdict, *fakeDatadog) {
	t.Helper()
	fake := &fakeDatadog{}
	srv := httptest.NewServer(fake.serve(path, override))
	t.Cleanup(srv.Close)
	values := map[string]string{"site": "datadoghq.eu", "api_key": fakeAPIKey, "app_key": fakeAppKey}
	verdict, err := datadog.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})(ctx, setup.Check{
		Field: "app_key",
		Value: func(field string) string { return values[field] },
	})
	if err != nil {
		t.Fatalf("the probe failed rather than answering: %v", err)
	}
	return verdict, fake
}

func refuseWith(reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["` + reason + `"]}`))
	}
}

// A WORKING PAIR NAMES ITS ORGANIZATION, its region and what it was proved
// able to read, and claims nothing more.
func TestADatadogCheckNamesTheOrganization(t *testing.T) {
	t.Parallel()
	got, _ := checkAgainst(t, t.Context(), "", nil)
	want := "Works. Both keys are valid for the FairSplit organization in datadoghq.eu, " +
		"and can read users, roles and webhooks."
	if got.Outcome != setup.VerifyWorks || got.Detail != want {
		t.Errorf("answered %+v, want %q", got, want)
	}
}

// EACH STAGE'S REFUSAL NAMES THE KEY TO REPLACE, or the permission the
// application key lacks, in Datadog's own words.
func TestADatadogRefusalNamesWhatFailed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, path, want string
	}{
		{"the API key", validatePath, "Datadog refused the API key: Forbidden."},
		{"the application key", orgPath,
			"The API key is valid, but Datadog refused the application key: Forbidden."},
		{"reading users", usersPath, "Both keys are valid, but the application key lacks " +
			"a permission Crewlet needs: reading users (Forbidden)."},
		{"reading roles", rolesPath, "Both keys are valid, but the application key lacks " +
			"a permission Crewlet needs: reading roles (Forbidden)."},
		{"reading webhooks", webhookPath, "Both keys are valid, but the application key " +
			"lacks a permission Crewlet needs: reading webhooks (Forbidden)."},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, _ := checkAgainst(t, t.Context(), c.path, forbidden)
			if got.Outcome != setup.VerifyRefused || got.Detail != c.want {
				t.Errorf("answered %+v, want refused %q", got, c.want)
			}
		})
	}
}

// A REFUSAL WITHOUT A REASON STILL NAMES THE KEY, and a reason that would
// repeat a key, or run past a sentence, is left out whole.
func TestADatadogRefusalKeepsItsReasonFitToShow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		path   string
		refuse http.HandlerFunc
		want   string
	}{
		{"no reason", orgPath, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}, "The API key is valid, but Datadog refused the application key."},
		{"a reason repeating the API key", validatePath,
			refuseWith("key " + fakeAPIKey + " is unknown"), "Datadog refused the API key."},
		{"a reason repeating the application key", usersPath,
			refuseWith("key " + fakeAppKey + " lacks user_access_read"),
			"Both keys are valid, but the application key lacks a permission Crewlet " +
				"needs: reading users."},
		{"a reason too long to repeat", rolesPath, refuseWith(strings.Repeat("denied ", 60)),
			"Both keys are valid, but the application key lacks a permission Crewlet " +
				"needs: reading roles."},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, _ := checkAgainst(t, t.Context(), c.path, c.refuse)
			if got.Outcome != setup.VerifyRefused || got.Detail != c.want {
				t.Errorf("answered %+v, want refused %q", got, c.want)
			}
		})
	}
}

// AN ABSENT WEBHOOK IS A READ THAT WORKED: the first reconcile pass creates
// it, so a new organization is not a missing permission.
func TestADatadogCheckAllowsAnAbsentWebhook(t *testing.T) {
	t.Parallel()
	got, _ := checkAgainst(t, t.Context(), webhookPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if got.Outcome != setup.VerifyWorks {
		t.Errorf("answered %+v, want works", got)
	}
}

// AN OUTAGE IS NEVER A REFUSAL, at whichever stage it happens.
func TestADatadogOutageAtAnyStageIsUnreachable(t *testing.T) {
	t.Parallel()
	for _, path := range []string{validatePath, orgPath, usersPath, rolesPath, webhookPath} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			got, _ := checkAgainst(t, t.Context(), path, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			})
			want := "Datadog answered 502 Bad Gateway, so it could not say whether this " +
				"works. Nothing was stored."
			if got.Outcome != setup.VerifyUnreachable || got.Detail != want {
				t.Errorf("answered %+v, want unreachable %q", got, want)
			}
		})
	}
}

// A STAGE THAT RUNS OUT OF TIME SAYS WHICH STAGE IT WAS, and is not a
// refusal: the check's deadline is the request's or its own, whichever ends
// first.
func TestADatadogStageOutOfTimeSaysWhich(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	got, _ := checkAgainst(t, ctx, orgPath, func(_ http.ResponseWriter, r *http.Request) {
		// Held until the check gives up and drops the connection.
		<-r.Context().Done()
	})
	want := "Datadog did not answer in time while checking the application key. " +
		"Nothing was stored."
	if got.Outcome != setup.VerifyUnreachable || got.Detail != want {
		t.Errorf("answered %+v, want unreachable %q", got, want)
	}
}

// EVERY CALL IS A READ, the keys travel only as headers, and the API key's
// own stage carries no application key.
func TestADatadogCheckOnlyReadsAndKeepsTheKeysInHeaders(t *testing.T) {
	t.Parallel()
	_, fake := checkAgainst(t, t.Context(), "", nil)
	reqs := fake.requests()
	var paths []string
	for _, r := range reqs {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: the check wrote at Datadog", r.Method, r.URL.Path)
		}
		if u := r.URL.String(); strings.Contains(u, fakeAPIKey) || strings.Contains(u, fakeAppKey) {
			t.Errorf("a key travelled in the URL: %s", u)
		}
		if r.URL.Path == validatePath && r.Header.Get("DD-APPLICATION-KEY") != "" {
			t.Error("the API key's own stage carried the application key")
		}
	}
	want := []string{validatePath, orgPath, usersPath, rolesPath, webhookPath}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("the stages read %v, want %v", paths, want)
	}
}

// A REFUSAL STOPS THE CHECK: nothing after the stage that failed is asked.
func TestADatadogRefusalStopsTheCheck(t *testing.T) {
	t.Parallel()
	_, fake := checkAgainst(t, t.Context(), validatePath, forbidden)
	if n := len(fake.requests()); n != 1 {
		t.Errorf("Datadog was asked %d times after refusing the API key, want once", n)
	}
}

// THE KEYS ARE CHECKED TOGETHER, so a check without both, or without a
// region Datadog serves, is a check this cannot make, said as such, and
// never a refusal sent to Datadog. And the API key is not checked on its
// own: the surface answers it from its note.
func TestADatadogCheckWithoutBothKeysIsUnusable(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	t.Cleanup(srv.Close)
	probe := datadog.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})
	for _, c := range []struct {
		values map[string]string
		says   string
	}{
		{map[string]string{"app_key": "b", "site": "datadoghq.com"}, "Enter both"},
		{map[string]string{"api_key": "a", "app_key": " ", "site": "datadoghq.com"}, "Enter both"},
		{map[string]string{"api_key": "a", "app_key": "b", "site": "datadog.example"}, "region"},
	} {
		_, err := probe(t.Context(), setup.Check{
			Field: "app_key", Value: func(f string) string { return c.values[f] },
		})
		if !errors.Is(err, setup.ErrUnusableInput) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("with %v: %v", c.values, err)
		}
	}
	_, err := probe(t.Context(), setup.Check{
		Field: "api_key", Value: func(string) string { return "a" },
	})
	if !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("the API key on its own: %v, want ErrNotCheckable", err)
	}
	if called.Load() {
		t.Error("Datadog was asked about a pair that was not whole")
	}
}

// THE ONE TEST IS DECLARED ON THE APPLICATION KEY, with the fields it is made
// with, so a form draws one control after it and none beside the API key.
func TestTheDatadogKeysDeclareOneCheck(t *testing.T) {
	t.Parallel()
	byField := map[string]setup.Requirement{}
	for _, r := range datadog.Requirements(nil, nil) {
		byField[r.Field] = r
	}
	app, api := byField["app_key"], byField["api_key"]
	if !app.Checkable || strings.Join(app.CheckWith, ",") != "site,api_key" {
		t.Errorf("app_key: checkable %v, check_with %v", app.Checkable, app.CheckWith)
	}
	if api.Checkable || api.CheckNote == "" {
		t.Errorf("api_key: checkable %v, note %q; it is tested with the application key",
			api.Checkable, api.CheckNote)
	}
}
