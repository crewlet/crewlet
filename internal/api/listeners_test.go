package api_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// THE TWO LISTENERS ARE EXACT COMPLEMENTS. With api.public set, every route
// outside parties call is served on the public listener and answers 404 on
// api.port, and every other route the reverse — so the socket a deployment
// publishes carries none of the admin API, and a vendor pointed at the wrong
// one learns it from a 404 rather than from a delivery quietly accepted on the
// socket nobody published.

// publicRoutes are the routes outside parties call, one of each family: a
// vendor delivery, both vendor app landings, the sandbox telemetry edge, the
// sandbox tool bridge, a box's three git routes and a check's box's three
// reads of its commit. Each is mounted on [listenerApp], so reaching its
// handler is any answer but the mux's own 404.
var publicRoutes = []struct{ method, path string }{
	{http.MethodPost, "/webhooks/github"},
	{http.MethodPost, "/webhooks/slack/ceo"},
	{http.MethodGet, "/webhooks/slack-oauth?code=c"},
	{http.MethodGet, "/webhooks/github-app?code=c&state=s"},
	{http.MethodPost, "/otlp/not-a-token/v1/traces"},
	{http.MethodPost, mcpbridge.PathPrefix + "not-a-token"},
	{http.MethodGet, auth.BoxGitPrefix + "web.git/info/refs?service=git-upload-pack"},
	{http.MethodPost, auth.BoxGitPrefix + "web.git/git-upload-pack"},
	{http.MethodPost, auth.BoxGitPrefix + "web.git/git-receive-pack"},
	{http.MethodGet, auth.BoxCodePrefix + "web/tree"},
	{http.MethodGet, auth.BoxCodePrefix + "web/blob?path=go.mod"},
	{http.MethodGet, auth.BoxCodePrefix + "web/diff"},
}

// adminRoutes are the routes that stay on api.port: the probes, the dashboard
// shell and its assets, the REST and query reads, and a write. Each is mounted
// on [listenerApp] as well.
var adminRoutes = []struct{ method, path string }{
	{http.MethodGet, "/health"},
	{http.MethodGet, "/ready"},
	{http.MethodGet, "/"},
	{http.MethodGet, "/dashboard"},
	{http.MethodGet, "/favicon.ico"},
	{http.MethodGet, "/agents"},
	{http.MethodGet, "/query/viewer"},
	{http.MethodPost, "/backup"},
}

// listenerApp is a configured, authenticated node with every optional surface
// mounted — the webhook edge, the telemetry receiver, the tool bridge and the
// operator surface — and every code route served, and the public listener set
// or not.
func listenerApp(t *testing.T, public bool, draining bool) *api.App {
	t.Helper()
	b := closedPosture()
	b.API.Port = 8080
	if public {
		b.API.Public = config.APIPublic{Port: 8443}
	}
	return newApp(t, api.Options{
		Bootstrap: &b,
		Runtime: &fakeRuntime{state: api.RuntimeState{
			ShuttingDown: draining, Posture: "serve",
		}},
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return webhooks.Secrets{GitHub: "gh-secret"} },
			Publisher: memory.New(),
		},
		OtelReceiver: otlpReceiver(t, "http://127.0.0.1:1"),
		Bridge: bridgeFor(t, mcpbridge.Options{
			Material: runtoken.OneKey("k", "k"), BaseURL: "http://x",
		}),
		Operator: operatorSurface(t, builtin.WorkDeps{Reader: stubWorkReader{}}),
		// SERVED, not merely mounted: a git route over a node with no host
		// answers its own 404, which the walk below could not tell from a
		// route mounted on the wrong listener.
		Code:    fixedCode{git: fakeGitRoutes{}, reads: &fakeCodeReads{}},
		Sources: queries.Sources{Company: active(t)},
	})
}

// sendTo runs one request carrying the operator token through one listener's
// handler.
func sendTo(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// absent reports whether an answer is the "nothing is served here" 404, which
// is the one answer that proves a request never reached a handler: the mux's
// own, or the partition's, each of which says so in its detail. A handler's
// own 404 — an asset that is not in the bundle, a telemetry signal outside the
// closed set — carries the same code and no such detail, and is a route
// reached.
func absent(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		return false
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("a 404 that is not JSON: %v (%q)", err, rec.Body.String())
	}
	return body["error"] == string(httpjson.CodeNoRoute) &&
		(strings.HasPrefix(body["detail"], "This node serves nothing at ") ||
			strings.HasPrefix(body["detail"], "This listener serves nothing at "))
}

// publishedRoutes are the patterns the public listener serves: the whole
// surface a deployment exposes to the internet once it sets api.public.
//
// A REVIEWED LIST, held against the mounted table rather than derived from it:
// a route under a public prefix is published and exempt from the guard whatever
// it was meant to be, so a new one has to be somebody's decision in a diff, not
// a side effect of where a path happened to be mounted.
var publishedRoutes = []string{
	"POST /webhooks/github",
	"POST /webhooks/github/{handle}",
	"POST /webhooks/gitlab",
	"POST /webhooks/jira",
	"POST /webhooks/datadog",
	"POST /webhooks/confluence",
	"POST /webhooks/confluence/{event}",
	"POST /webhooks/slack/{handle}",
	"POST /webhooks/forge",
	"GET /webhooks/slack-oauth",
	"GET /webhooks/github-app",
	"POST " + auth.OTLPPrefix + "{token}/v1/{signal}",
	mcpbridge.PathPrefix + "{token}",
	host.PatternBoxInfoRefs,
	host.PatternBoxUploadPack,
	host.PatternBoxReceivePack,
	host.PatternBoxCodeTree,
	host.PatternBoxCodeBlob,
	host.PatternBoxCodeDiff,
}

// wildcard is one segment of a mux pattern that matches more than itself.
var wildcard = regexp.MustCompile(`\{[^}]*\}`)

// requestFor is a request the mux routes to pattern: its method, or GET for a
// pattern that takes every method, and its path with each wildcard filled.
func requestFor(pattern string) (method, path string) {
	method, path = http.MethodGet, pattern
	if verb, rest, ok := strings.Cut(pattern, " "); ok {
		method, path = verb, rest
	}
	return method, wildcard.ReplaceAllStringFunc(path, func(w string) string {
		if w == "{$}" {
			return ""
		}
		return "x"
	})
}

// THE WHOLE TABLE, NOT A SAMPLE. Every route the app mounts is served on the
// listener its path names and is absent from the other, every route on the
// public listener asks for no operator token — its callers hold none — and the
// set the public listener serves is exactly [publishedRoutes]. Walked from what
// was mounted, so a route added under a public prefix, or one an outside party
// calls added outside them, fails here rather than in a deployment.
func TestEveryMountedRouteIsOnTheListenerItsPathNames(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, false)
	public := a.Public()
	if public == nil {
		t.Fatal("api.public is set and the app built no public handler")
	}
	routes := a.Routes()
	if len(routes) < len(publishedRoutes)+len(adminRoutes) {
		t.Fatalf("the app mounted %d routes, fewer than this file names: %v", len(routes), routes)
	}
	var published []string
	for _, pattern := range routes {
		method, path := requestFor(pattern)
		own, other, side := http.Handler(a), public, "api.port"
		if auth.Public(path) {
			published = append(published, pattern)
			own, other, side = public, a, "the public listener"
			if !auth.Unguarded(path) {
				t.Errorf("%q is on the public listener and guarded: no caller "+
					"there holds the token it asks for", pattern)
			}
		}
		if rec := sendTo(t, own, method, path); absent(t, rec) {
			t.Errorf("%q (%s %s) is absent from %s, the listener its path names",
				pattern, method, path, side)
		}
		if rec := sendTo(t, other, method, path); !absent(t, rec) {
			t.Errorf("%q (%s %s) answered %d on the listener beside %s, want 404: "+
				"the route is served on two sockets", pattern, method, path, rec.Code, side)
		}
	}
	slices.Sort(published)
	want := slices.Sorted(slices.Values(publishedRoutes))
	if !slices.Equal(published, want) {
		t.Errorf("the public listener serves\n  %v\nwant\n  %v\nA route under a public "+
			"prefix is published and unguarded: if outside parties call it, add it to "+
			"publishedRoutes; if they do not, mount it elsewhere", published, want)
	}
}

// WITHOUT api.public EVERY ROUTE IS ON api.port, as every deployment before
// the listener existed runs — and there is no second handler to bind.
func TestWithoutAPublicListenerEveryRouteIsOnTheAPIPort(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, false, false)
	if a.Public() != nil {
		t.Fatal("no api.public, and the app built a public handler to bind")
	}
	for _, req := range append(publicRoutes[:len(publicRoutes):len(publicRoutes)], adminRoutes...) {
		if rec := sendTo(t, a, req.method, req.path); absent(t, rec) {
			t.Errorf("%s %s is absent from the only listener", req.method, req.path)
		}
	}
}

// EACH LISTENER SAYS WHETHER IT IS HTTPS BY THE ADDRESS IT IS PUBLISHED AT.
//
// The two sockets are published apart, and the natural shape of the split is
// an admin side on a private http address beside a public side behind TLS —
// which serves the GitHub App return and the Slack OAuth landing, pages a
// browser renders. One flag decided from `api.external_url` for both sent no
// Strict-Transport-Security on exactly those pages, and sent it from a plain
// http admin socket in the reverse arrangement. Both arrangements are held, so
// a build that hands either listener the other's answer fails one of them.
func TestEachListenerSendsHSTSByItsOwnAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, admin, public string
		adminHSTS, pubHSTS  bool
	}{
		{"a private http admin side beside a public one behind TLS",
			"http://127.0.0.1:8080", "https://hooks.example.com", false, true},
		{"an admin side behind TLS beside a plain http public one",
			"https://crewlet.example.com", "http://10.0.0.5:8443", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := closedPosture()
			b.API.Port = 8080
			b.API.ExternalURL = tc.admin
			b.API.Public = config.APIPublic{Port: 8443, ExternalURL: tc.public}
			a := newApp(t, api.Options{
				Bootstrap: &b,
				Runtime:   &fakeRuntime{state: api.RuntimeState{Posture: "serve"}},
				Sources:   queries.Sources{Company: active(t)},
			})
			for _, side := range []struct {
				name string
				h    http.Handler
				path string
				want bool
			}{
				{"api.port", a, "/nope", tc.adminHSTS},
				{"the public listener", a.Public(), "/webhooks/nope", tc.pubHSTS},
			} {
				got := sendTo(t, side.h, http.MethodGet, side.path).
					Header().Get("Strict-Transport-Security")
				if want := map[bool]string{true: pagepolicy.HSTS}[side.want]; got != want {
					t.Errorf("%s: Strict-Transport-Security = %q, want %q", side.name, got, want)
				}
			}
		})
	}
}

// AND THE BRIDGE-ONLY LISTENER IS THE PUBLIC ONE. A seats node without the
// ingress role binds it on api.public, so it answers by the public listener's
// address — never api.port's, which is another socket under another scheme.
func TestTheBridgeOnlyListenerSendsHSTSByThePublicAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		admin, public string
		want          string
	}{
		{"http://127.0.0.1:8080", "https://hooks.example.com", pagepolicy.HSTS},
		{"https://crewlet.example.com", "http://10.0.0.5:8443", ""},
	} {
		b := closedPosture()
		b.API.ExternalURL = tc.admin
		b.API.Public = config.APIPublic{Port: 8443, ExternalURL: tc.public}
		h := api.BridgeOnly(&b, bridgeFor(t, mcpbridge.Options{
			Material: runtoken.OneKey("k", "k"), BaseURL: "http://x",
		}))
		got := sendTo(t, h, http.MethodGet, "/nope").Header().Get("Strict-Transport-Security")
		if got != tc.want {
			t.Errorf("published at %s beside an admin side at %s: "+
				"Strict-Transport-Security = %q, want %q", tc.public, tc.admin, got, tc.want)
		}
	}
}

// THE PUBLIC LISTENER NEVER LOOKS AT AN OPERATOR TOKEN. A guarded route there
// is absent before any credential is compared, so the socket a deployment
// publishes cannot tell a valid token from an invalid one: a missing token, a
// wrong one and the right one all get the same 404.
func TestThePublicListenerIsNoOracleForAnOperatorToken(t *testing.T) {
	t.Parallel()
	public := listenerApp(t, true, false).Public()
	for _, bearer := range []string{"", "Bearer wrong", "Bearer secret"} {
		for _, path := range []string{"/config", "/secrets", "/operator/mcp", "/agents"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if bearer != "" {
				r.Header.Set("Authorization", bearer)
			}
			rec := httptest.NewRecorder()
			public.ServeHTTP(rec, r)
			if !absent(t, rec) {
				t.Errorf("GET %s with %q answered %d on the public listener, want "+
					"the same 404 whatever the credential", path, bearer, rec.Code)
			}
		}
	}
}

// THE PUBLIC LISTENER'S REFUSAL IS THE MUX'S OWN, byte for byte: the dashboard
// asked of the published socket answers exactly as a path nothing serves under
// a public prefix does, so a scanner learns nothing of an admin port behind it.
// api.port's refusal is the opposite — the caller is on the operator's network,
// most likely a vendor configured with the wrong port, and it names the
// listener that serves the route. Both carry the security headers.
func TestOnlyTheAPIPortSaysWhichListenerIsRight(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, false)
	unmounted := sendTo(t, a.Public(), http.MethodGet, "/webhooks/nope")
	if !absent(t, unmounted) {
		t.Fatalf("GET /webhooks/nope answered %d on the public listener, want the mux's 404",
			unmounted.Code)
	}
	for _, path := range []string{"/dashboard", "/config", "/nope"} {
		rec := sendTo(t, a.Public(), http.MethodGet, path)
		if rec.Code != unmounted.Code || !slices.Equal(headerNames(rec), headerNames(unmounted)) {
			t.Errorf("GET %s on the public listener: %d %v, want the mux's own %d %v",
				path, rec.Code, rec.Header(), unmounted.Code, unmounted.Header())
		}
		got := strings.ReplaceAll(rec.Body.String(), path, "/webhooks/nope")
		if got != unmounted.Body.String() {
			t.Errorf("GET %s on the public listener answered\n  %s\nnot the mux's own\n  %s",
				path, rec.Body.String(), unmounted.Body.String())
		}
	}

	rec := sendTo(t, a, http.MethodPost, "/webhooks/github")
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !absent(t, rec) || !strings.Contains(body["hint"], "api.public") {
		t.Errorf("a webhook on api.port answered %d %v, want a 404 naming api.public",
			rec.Code, body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the refusal carries no security headers: %v", rec.Header())
	}
}

// headerNames is every header an answer carries, with its values, in one order.
func headerNames(rec *httptest.ResponseRecorder) []string {
	var out []string
	for name, values := range rec.Header() {
		if name == "Content-Length" {
			continue
		}
		out = append(out, name+": "+strings.Join(values, ", "))
	}
	slices.Sort(out)
	return out
}

// BOTH LISTENERS DRAIN BY THE ONE RULE. The webhook edge on the public listener
// is refused from a drain's first moment like any delivery, the sandbox edges
// beside it are still served for the runs the drain cannot wait for, and
// api.port keeps its probes up while refusing its writes.
func TestBothListenersDrain(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, true)
	public := a.Public()
	for _, path := range []string{"/webhooks/github", "/webhooks/slack/ceo"} {
		if rec := sendTo(t, public, http.MethodPost, path); !refusedForDraining(t, rec) {
			t.Errorf("POST %s on the public listener answered %d during a drain, "+
				"want the draining 503", path, rec.Code)
		}
	}
	for _, path := range []string{mcpbridge.PathPrefix + "not-a-token", "/otlp/not-a-token/v1/traces"} {
		if rec := sendTo(t, public, http.MethodPost, path); refusedForDraining(t, rec) {
			t.Errorf("POST %s on the public listener was refused for draining: an "+
				"in-flight run lost its edge", path)
		}
	}
	if rec := sendTo(t, a, http.MethodGet, "/health"); rec.Code != http.StatusOK {
		t.Errorf("/health answered %d on api.port during a drain, want 200", rec.Code)
	}
	if rec := sendTo(t, a, http.MethodPost, "/backup"); !refusedForDraining(t, rec) {
		t.Errorf("POST /backup on api.port answered %d during a drain", rec.Code)
	}
}

// EVERY PUBLIC ROUTE IS EXEMPT FROM THE GUARD. The public listener's callers
// hold no operator token by definition, so a public prefix the guard did not
// exempt would answer every one of them 401.
func TestEveryPublicRouteIsUnguarded(t *testing.T) {
	t.Parallel()
	for _, prefix := range auth.PublicPrefixes() {
		if !auth.Unguarded(prefix + "x") {
			t.Errorf("%s is public and guarded: no caller of the public listener "+
				"holds the token it asks for", prefix)
		}
	}
	for _, req := range publicRoutes {
		if path, _, _ := strings.Cut(req.path, "?"); !auth.Public(path) {
			t.Errorf("%s is listed as a public route and auth.Public does not say so", path)
		}
	}
	for _, req := range adminRoutes {
		if auth.Public(req.path) {
			t.Errorf("%s is an admin route and auth.Public calls it public", req.path)
		}
	}
}

// GIT AND THE CODE READS ARE NEVER PUBLISHED. A person reaches a repository
// with their own bound token (ADR-0055), and api.public is the socket whose
// premise is that it serves only outside parties and asks no operator token —
// so /git/ and /code/ answer the mux's own 404 there, before any credential is
// read, whether it arrives as git's Basic password or as a bearer, and reach
// the code host on api.port. The published routes are unchanged:
// [TestEveryMountedRouteIsOnTheListenerItsPathNames] holds them to
// publishedRoutes, which names neither prefix.
//
// Mutation: add /git/ to the public prefixes, and the public half goes red.
func TestGitRoutesAreNotPublished(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, false)
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack"},
		{http.MethodPost, "/git/r.git/git-upload-pack"},
		{http.MethodPost, "/git/r.git/git-receive-pack"},
		{http.MethodGet, "/code/repos"},
		{http.MethodGet, "/code/repos/r"},
		{http.MethodPost, "/code/repos/r/verify"},
	} {
		for _, credential := range []string{"Basic " + basicAuth("crewlet", "secret"), "Bearer secret"} {
			r := httptest.NewRequest(req.method, req.path, nil)
			r.Header.Set("Authorization", credential)
			rec := httptest.NewRecorder()
			a.Public().ServeHTTP(rec, r)
			if !absent(t, rec) {
				t.Errorf("%s %s with %s answered %d on the public listener, want the mux's own 404",
					req.method, req.path, strings.Fields(credential)[0], rec.Code)
			}
		}
		// 200, and a verify's 202: the code host's own answers.
		if rec := sendTo(t, a, req.method, req.path); rec.Code/100 != 2 {
			t.Errorf("%s %s answered %d %q on api.port, want the code host's own answer",
				req.method, req.path, rec.Code, rec.Body.String())
		}
	}
}

// basicAuth is an Authorization header's Basic credential.
func basicAuth(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}
