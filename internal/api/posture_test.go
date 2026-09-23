package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE POSTURE MATRIX: every credential shape this build can present, against
// one route per authority class.
//
// # What it is for
//
// The authority rules are each tested where they live — the registry's grant
// check, the guard's resolution, the chart surface's own split. What no one of
// them covers is the COMPOSITION: a request travels the CORS gate, the guard,
// the CSRF gate, the drain gate and the route's own rule, and a hole is
// something that opens between two of them rather than inside either. Every
// case here goes through api.App, the way a caller does.
//
// # What a row means
//
// One credential shape, one route, one expected status. A cell that is wrong
// is either a surface reachable by somebody who should not (the dangerous
// direction) or one refused to somebody who should reach it (the direction
// that gets a guard disabled by whoever is on call).
func TestThePostureMatrix(t *testing.T) {
	t.Parallel()
	const (
		wide   = "a-token-that-carries-every-grant"
		narrow = "a-token-that-carries-only-state-read"
		blind  = "a-token-that-carries-only-config-read"
	)
	b := config.DefaultBootstrap()
	b.API.Host = "127.0.0.1"
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: wide, Grants: iam.AllGrants},
		{ID: "viewer", Token: narrow, Grants: []iam.Grant{iam.GrantStateRead}},
		{ID: "auditor", Token: blind, Grants: []iam.Grant{iam.GrantConfigRead}},
	}
	dev, err := auth.NewDevPrincipal("laptop", &b)
	if err != nil {
		t.Fatalf("NewDevPrincipal: %v", err)
	}

	// TWO APPS, because the development principal is a property of the
	// PROCESS rather than of a request: there is no header that turns it
	// on, which is the whole point of it being a flag.
	//
	// THE COORDINATION SOURCE is supplied so the fleet question is
	// actually mounted. Without it the row answers 404, which is a route
	// that is not there rather than one that refused — and a matrix full
	// of 404s certifies nothing while looking like a pass.
	sources := queries.Sources{Company: active(t), Coord: coordmemory.New()}
	plain := newApp(t, api.Options{Bootstrap: &b, Sources: sources})
	devApp := newApp(t, api.Options{
		Bootstrap: &b, DevPrincipal: dev, Sources: sources,
	})

	// The routes, one per authority class, each named by what reaching it
	// discloses rather than by its grant — so a row that moves to a
	// different grant still has to be read rather than renumbered.
	//
	// THE QUESTION SURFACE for all four, because it is the one place every
	// grant class is reachable through one mount, and because it is the
	// surface where authority was a single bool until this build: any
	// credential could ask any question that was not on the config
	// surface. `/config`, `/secrets`, `/setup` and `/chart` have their own
	// suites for their own rules; what no one of those covers is the
	// COMPOSITION this asserts.
	type route struct{ name, path string }
	routes := []route{
		{"the exempt probe", "/health"},
		{"the dashboard shell", "/dashboard"},
		{"the company's working state", "/query/stream"},
		{"an agent's transcripts", "/query/events"},
		{"the map of what is not configured", "/query/integrations"},
		{"the deployment's own shape", "/query/fleet"},
		// THE SNAPSHOT'S REST MIRRORS, decided by the grant their push
		// kind takes on the socket rather than by being resolved at all.
		{"the roster mirror", "/agents"},
		{"the whole snapshot", "/stream/snapshot"},
	}

	// The credential shapes. A session and a personal access token are
	// deliberately absent: no route mints either at this commit, so a row
	// for one would assert a shape nothing can produce.
	const (
		ok    = http.StatusOK
		unath = http.StatusUnauthorized
		forbd = http.StatusForbidden
	)
	for _, tc := range []struct {
		shape  string
		app    *api.App
		header string
		want   map[string]int
	}{
		{
			shape: "nothing at all", app: plain, header: "",
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": unath, "/query/events": unath,
				"/query/integrations": unath, "/query/fleet": unath, "/agents": unath, "/stream/snapshot": unath,
			},
		},
		{
			// A CREDENTIAL THAT IS PRESENT AND WRONG IS NOT ANONYMOUS:
			// sending one says you meant to be somebody, and quietly
			// serving you as nobody is how a revoked token goes on
			// appearing to work.
			shape: "a credential this node refuses", app: plain,
			header: "Bearer not-one-of-the-two",
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": unath, "/query/events": unath,
				"/query/integrations": unath, "/query/fleet": unath, "/agents": unath, "/stream/snapshot": unath,
			},
		},
		{
			// THE NARROW TOKEN IS THE READER `allow_anonymous_read`
			// was replaced by, and the rows below it are what that
			// posture used to serve to anybody at all.
			shape: "a Tier A token carrying state:read alone", app: plain,
			header: "Bearer " + narrow,
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": ok, "/query/events": forbd,
				"/query/integrations": forbd, "/query/fleet": forbd, "/agents": ok, "/stream/snapshot": ok,
			},
		},
		{
			// RESOLVED AND ABLE TO READ NONE OF THE COMPANY'S STATE:
			// the mirrors refuse it exactly as the question does, where
			// they used to serve any resolved caller at all.
			shape: "a Tier A token carrying config:read alone", app: plain,
			header: "Bearer " + blind,
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": forbd, "/query/events": forbd,
				"/query/integrations": ok, "/query/fleet": forbd,
				"/agents": forbd, "/stream/snapshot": forbd,
			},
		},
		{
			shape: "a Tier A token carrying every grant", app: plain,
			header: "Bearer " + wide,
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": ok, "/query/events": ok,
				"/query/integrations": ok, "/query/fleet": ok, "/agents": ok, "/stream/snapshot": ok,
			},
		},
		{
			// THE DEVELOPMENT PRINCIPAL carries the deployment's
			// ceiling and no more — which here is everything, because
			// this fixture's ceiling is. `api.auth.disabled` granted
			// everything REGARDLESS, which is the difference.
			shape: "no credential, on a -dev-principal node", app: devApp,
			header: "",
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": ok, "/query/events": ok,
				"/query/integrations": ok, "/query/fleet": ok, "/agents": ok, "/stream/snapshot": ok,
			},
		},
		{
			// AND A WRONG CREDENTIAL IS STILL WRONG THERE. The
			// development principal covers an ABSENT credential only,
			// or it would hide the typo somebody is about to spend an
			// afternoon on.
			shape: "a refused credential, on a -dev-principal node", app: devApp,
			header: "Bearer not-one-of-the-two",
			want: map[string]int{
				"/health": ok, "/dashboard": ok,
				"/query/stream": unath, "/query/events": unath,
				"/query/integrations": unath, "/query/fleet": unath, "/agents": unath, "/stream/snapshot": unath,
			},
		},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			t.Parallel()
			for _, r := range routes {
				want, stated := tc.want[r.path]
				if !stated {
					t.Fatalf("%s has no expectation for %s; every route is "+
						"stated for every shape, or a hole is an omission "+
						"nobody reads as one", tc.shape, r.path)
				}
				req := httptest.NewRequest(http.MethodGet, r.path, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				rec := httptest.NewRecorder()
				tc.app.ServeHTTP(rec, req)
				if rec.Code != want {
					t.Errorf("%s reaching %s (%s): %d, want %d\n%s",
						tc.shape, r.name, r.path, rec.Code, want,
						rec.Body.String())
				}
			}
		})
	}
}

// AND THE MATRIX ABOVE IS NOT ONE-SIDED. A cell asserting a refusal proves
// nothing unless the same route answers somebody — which is what the
// every-grant row is — and a cell asserting an answer proves nothing unless
// the route is genuinely guarded. This states the second half as a claim of
// its own, so neither can drift into a matrix of one repeated status.
func TestThePostureMatrixIsNotAllOneAnswer(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: "a-token-that-carries-every-grant", Grants: iam.AllGrants},
	}
	a := newApp(t, api.Options{Bootstrap: &b})

	seen := map[int]string{}
	for _, probe := range []struct{ path, header string }{
		{"/health", ""}, // exempt
		{"/config", ""}, // guarded, no credential
		{"/config", "Bearer a-token-that-carries-every-grant"}, // guarded, allowed
	} {
		req := httptest.NewRequest(http.MethodGet, probe.path, nil)
		if probe.header != "" {
			req.Header.Set("Authorization", probe.header)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		seen[rec.Code] = fmt.Sprintf("%s %q", probe.path, probe.header)
	}
	if len(seen) < 2 {
		t.Fatalf("every probe answered the same status (%v); the matrix is "+
			"asserting one answer repeated rather than a decision", seen)
	}
}

// THE SNAPSHOT MIRROR IS THE CALLER'S AUDIENCE, key by key.
//
// A reader holding `state:read` alone receives the roster and not the event
// feed — every phase's prompt and response, which the `events` question
// refuses without `audit:read` — so neither channel is a way round the other.
func TestTheSnapshotMirrorIsTheCallersAudience(t *testing.T) {
	t.Parallel()
	const (
		wide   = "a-token-that-carries-every-grant"
		narrow = "a-token-that-carries-only-state-read"
	)
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: wide, Grants: iam.AllGrants},
		{ID: "viewer", Token: narrow, Grants: []iam.Grant{iam.GrantStateRead}},
	}
	a := newApp(t, api.Options{Bootstrap: &b})
	keys := func(token string) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/stream/snapshot", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /stream/snapshot = %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	if got := keys(narrow); got["events"] != nil || got["agents"] == nil {
		t.Errorf("a state:read-only snapshot: events present = %v, agents present = %v",
			got["events"] != nil, got["agents"] != nil)
	}
	if got := keys(wide); got["events"] == nil {
		t.Error("an audit:read holder's snapshot carries no event feed")
	}
}
