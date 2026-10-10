package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE ROUTE TABLE NAMES A REACH FOR EVERY ROUTE, and this gate holds each to
// the decision ADR-0031's withholding rule makes for it — walked from what the
// app actually mounted, the real /config, /secrets and /setup surfaces
// included, so a route added anywhere is judged here rather than in a
// deployment.
//
// That a route HAS a reach is the compiler's ([auth.Router] takes none
// without one) and that the reach is one this build knows is the guard's
// ([auth.Guard.Require] panics at mount). What only a walk can say is that
// each is the RIGHT one.

// reachedRoutes are the mounted routes that are not a registry question's,
// and the reach each must be mounted at. A REVIEWED LIST, held against the
// table in both directions: a new route is a decision somebody makes in a
// diff, never a side effect of where it was mounted.
var reachedRoutes = map[string]auth.Reach{
	// The probes and the dashboard shell: an orchestrator and a browser
	// that has not signed in hold no key.
	"GET /health": auth.ReachOpen, "GET /ready": auth.ReachOpen,
	"GET /{$}": auth.ReachOpen, "GET /dashboard": auth.ReachOpen,
	"GET /favicon.ico": auth.ReachOpen, "GET /static/": auth.ReachOpen,
	// Many questions behind one pattern, each judged by the registry.
	"GET /query/{what}": auth.ReachOpen,
	// What a socket is sent is decided per question.
	auth.SocketPath: auth.ReachOpen,
	// The company's public face.
	"GET /org": auth.ReachPublic,
	// The company's own work, and the person's own transports onto it.
	"GET /work/files/{project}/{path...}":    auth.ReachMember,
	"PUT /work/files/{project}/{path...}":    auth.ReachMember,
	"DELETE /work/files/{project}/{path...}": auth.ReachMember,
	operator.MCPPath:                         auth.ReachMember,
	operator.ActPattern:                      auth.ReachMember,
	// What the machine processes and how it is run.
	"GET /agents": auth.ReachAdmin, "GET /tools": auth.ReachAdmin,
	"GET /stream/snapshot":  auth.ReachAdmin,
	"POST /backup":          auth.ReachAdmin,
	"POST /work/{id}/purge": auth.ReachAdmin,
}

// reachApp is a node with EVERY surface mounted — the real config, secrets and
// setup services among them, since their reach is the most consequential in
// the table — on a posture whose keys never come up: the gate reads the table,
// and the cases that exercise it at the door are below.
func reachApp(t *testing.T) *api.App {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "reach.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	b := closedPosture()
	boot := config.Bootstrap{Stream: config.Stream{StoreDir: t.TempDir()}}
	cfg, err := configapi.New(configapi.Options{Store: db, Plane: coordmemory.NewFleet(), Bootstrap: &boot})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	secrets, err := secretsapi.New(secretsapi.Options{Fleet: coordmemory.NewFleet()})
	if err != nil {
		t.Fatalf("secretsapi.New: %v", err)
	}
	setupSurface, err := setupapi.New(setupapi.Options{
		Company: func() *config.Company { return nil }, Config: cfg,
		Secrets: noSecrets{}, Resolve: func(string) (string, bool) { return "", false },
		Passes: setup.NewRunner(nil, nil, nil),
		Sink:   func(string) (provision.TokenSink, error) { return nil, nil },
		Status: noStatus{}, SlackApps: func() map[string]string { return nil },
		StateClaims: coordmemory.NewFleet(),
	})
	if err != nil {
		t.Fatalf("setupapi.New: %v", err)
	}
	return newApp(t, api.Options{
		Bootstrap: &b,
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return webhooks.Secrets{} },
			Publisher: memory.New(),
		},
		OtelReceiver: otlpReceiver(t, "http://127.0.0.1:1"),
		Bridge:       mcpbridge.New(mcpbridge.Options{Key: []byte("k"), BaseURL: "http://x"}),
		Operator: operatorSurface(t, operator.Options{
			Work: builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)},
		}),
		Files:   newFakeFiles(t),
		Purger:  &fakePurger{},
		Config:  cfg,
		Secrets: secrets,
		Setup:   setupSurface,
		// EVERY QUESTION SERVED UNDER AN ADMIN PREFIX IS REGISTERED, or its
		// route would be mounted open answering `unknown_query` and the
		// prefix rule below would be judging an absent question.
		Sources: queries.Sources{
			Company:   active(),
			Retention: func(context.Context) any { return nil },
		},
	})
}

// noSecrets and noStatus are the setup surface's two stores, never written:
// the gate reads the routes and calls none.
type noSecrets struct{}

func (noSecrets) Set(context.Context, string, string, string, string, time.Time) error { return nil }

type noStatus struct{}

func (noStatus) SaveIntegration(context.Context, integration.State) error      { return nil }
func (noStatus) ForgetIntegration(context.Context, integration.Kind) error     { return nil }
func (noStatus) LoadIntegrations(context.Context) ([]integration.State, error) { return nil, nil }

// pathOf is a pattern's path, its method dropped.
func pathOf(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

// EVERY ROUTE DECLARES THE REACH IT SERVES, and it is the one the withholding
// rule gives it (ADR-0031).
func TestEveryRouteDeclaresTheReachItServes(t *testing.T) {
	t.Parallel()
	a := reachApp(t)
	routes := a.Routes()
	mounted := map[string]bool{}
	for _, route := range routes {
		mounted[route.Pattern] = true
		path := pathOf(route.Pattern)
		switch {
		// THE ROUTES OUTSIDE PARTIES CALL hold no key of this engine:
		// each authenticates by a vendor's signature or a per-run token.
		case auth.Public(path):
			if route.Reach != auth.ReachOpen {
				t.Errorf("%q is under a public prefix and mounted at %q: no caller there "+
					"holds a key", route.Pattern, route.Reach)
			}
		// HOW THE ENGINE IS RUN, reads included.
		case path == "/config" || strings.HasPrefix(path, "/config/") ||
			path == "/secrets" || strings.HasPrefix(path, "/secrets/") ||
			path == "/setup" || strings.HasPrefix(path, "/setup/") ||
			path == "/fleet" || strings.HasPrefix(path, "/fleet/") ||
			strings.HasPrefix(path, "/work/retention"):
			if route.Reach != auth.ReachAdmin {
				t.Errorf("%q runs the engine and is mounted at %q, want admin",
					route.Pattern, route.Reach)
			}
		}
		// A QUESTION'S ROUTE IS AT ITS QUESTION'S REACH, the registry
		// being the one source — and a question this node does not
		// register answers nothing but `unknown_query`, so its route is
		// open.
		if what, isQuestion := api.QuestionOf(route.Pattern); isQuestion {
			want := a.Queries().ReachOf(what)
			if want == "" {
				want = auth.ReachOpen
			}
			if route.Reach != want {
				t.Errorf("%q answers %q, registered at %q, and is mounted at %q",
					route.Pattern, what, want, route.Reach)
			}
			continue
		}
		if auth.Public(path) || strings.HasPrefix(path, "/config") ||
			strings.HasPrefix(path, "/secrets") || strings.HasPrefix(path, "/setup") ||
			strings.HasPrefix(path, "/fleet/") || strings.HasPrefix(path, "/work/retention/") {
			continue
		}
		want, listed := reachedRoutes[route.Pattern]
		if !listed {
			t.Errorf("%q is mounted at %q and nothing here classifies it: list it in "+
				"reachedRoutes with the reach the withholding rule gives it",
				route.Pattern, route.Reach)
			continue
		}
		if route.Reach != want {
			t.Errorf("%q is mounted at %q, want %q", route.Pattern, route.Reach, want)
		}
	}
	for pattern := range reachedRoutes {
		if !mounted[pattern] {
			t.Errorf("%q is classified and this app mounts no such route: the list has "+
				"drifted from the table", pattern)
		}
	}
	// THE SURFACES THIS GATE EXISTS FOR ARE ACTUALLY IN THE TABLE, or the
	// walk above asserted nothing about them.
	for _, prefix := range []string{"/config", "/secrets", "/setup/", "/fleet/broker/", "/webhooks/", "/work/retention/"} {
		found := false
		for _, route := range routes {
			found = found || strings.HasPrefix(pathOf(route.Pattern), prefix)
		}
		if !found {
			t.Errorf("no route under %s is mounted, so this gate certified nothing about it", prefix)
		}
	}
}

// AT THE DOOR, THE TABLE IS WHAT IS ENFORCED: one route of each reach, asked
// by each kind of caller, through the whole app. A member reads the company's
// work and is forbidden the machine; nobody is told to sign in.
func TestEachReachIsEnforcedAtTheDoor(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.Auth.Anonymous = config.AnonymousPublic
	b.API.Auth.Tokens = append(b.API.Auth.Tokens,
		config.APIToken{ID: "ada", Role: config.RoleMember, Token: "member-secret"})
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Config:    stubSurface{"GET /config"},
		Sources:   queries.Sources{Company: active()},
	})
	for _, tc := range []struct {
		path        string
		key         string
		want        int
		description string
	}{
		{"/health", "", http.StatusOK, "an open route, to nobody"},
		{"/org", "", http.StatusOK, "the public face, to nobody under anonymous: public"},
		{"/schedules", "", http.StatusUnauthorized, "a member route, to nobody"},
		{"/schedules", "member-secret", http.StatusOK, "a member route, to a member"},
		{"/agents", "member-secret", http.StatusForbidden, "an admin route, to a member"},
		{"/config", "member-secret", http.StatusForbidden, "configuration, to a member"},
		{"/budgets", "member-secret", http.StatusForbidden, "spend, to a member"},
		{"/agents", "secret", http.StatusOK, "an admin route, to an admin"},
		{"/config", "secret", http.StatusNoContent, "configuration, to an admin"},
	} {
		if got := sendAs(t, a, tc.path, tc.key); got != tc.want {
			t.Errorf("%s: GET %s = %d, want %d", tc.description, tc.path, got, tc.want)
		}
	}
}

// sendAs runs one GET presenting key ("" presents none) and answers its
// status.
func sendAs(t *testing.T, a *api.App, path, key string) int {
	t.Helper()
	rec := sendTo(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" {
			r.Header.Del("Authorization")
		} else {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		a.ServeHTTP(w, r)
	}), http.MethodGet, path)
	return rec.Code
}
