package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// guard builds a guard over a Tier A shaped by the mutator.
func guard(t *testing.T, mutate func(*config.APIAuth)) *auth.Guard {
	t.Helper()
	b := config.DefaultBootstrap()
	if mutate != nil {
		mutate(&b.API.Auth)
	}
	return auth.New(&b)
}

func withTokens(tokens ...config.APIToken) func(*config.APIAuth) {
	return func(a *config.APIAuth) { a.Tokens = tokens }
}

// serve runs one request through the guard and returns the response and the
// operator id the handler saw.
func serve(t *testing.T, g *auth.Guard, method, path, header string) (*http.Response, string) {
	t.Helper()
	var seen string
	handler := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, how := iam.From(r.Context()); how == iam.Resolved {
			seen = auth.OperatorID(principal)
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result(), seen
}

// --- what the guard covers ---------------------------------------------- //

// EVERY GUARDED ROUTE NEEDS A CREDENTIAL, READS INCLUDED, and the method no
// longer enters into it.
//
// This asserted the opposite until `allow_anonymous_read` was deleted: a GET
// of /events served without one, by default, and /events and
// /agents/{id}/memory carry full LLM transcripts — prompts, tool arguments,
// diary entries.
func TestEveryGuardedRouteNeedsACredentialWhateverTheMethod(t *testing.T) {
	t.Parallel()
	// THROUGH THE MIDDLEWARE, because the method is what this case is
	// about and [auth.Unguarded] does not take one: asked of the predicate,
	// the loop below would be one tautology repeated seven times. The
	// middleware is where a verb could still be branched on, so it is the
	// only place the claim can be held.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	for _, method := range []string{
		"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE",
	} {
		for _, path := range []string{"/events", "/agents", "/agents/x/memory"} {
			res, _ := serve(t, g, method, path, "")
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s answered %d without a credential, want 401",
					method, path, res.StatusCode)
			}
		}
	}
	// The counterfactual: the credential IS served, or the case above would
	// pass on a guard that refused everything.
	if res, _ := serve(t, g, http.MethodGet, "/events", "Bearer secret"); res.StatusCode != http.StatusOK {
		t.Errorf("a valid credential answered %d", res.StatusCode)
	}
}

// THE SURFACES THAT CARRY THE COMPANY ITSELF ARE GUARDED, reads included, and
// so is everything beneath each of them.
//
// This used to be a declared list — `GuardedPrefixes` — because everything NOT
// on it followed `allow_anonymous_read` and served by default. The list is gone
// with that posture: guarded is what a route IS unless [auth.Unguarded] exempts
// it. What still has to be asserted is that none of these ever lands on the
// exemption list, because that is the one edit that would open them again, and
// each is a surface whose READ is as sensitive as its write — or, for the last
// two, whose every WRITE lands a record with an author:
//
//   - /config: the whole company document — its org chart, its integrations,
//     and every ${VAR} reference in it by name.
//   - /secrets: the fleet's credential store. Even the listing, carrying no
//     values, says which credentials a company holds and when each last
//     changed, and one route returns a value outright.
//   - /setup: the map of which credentials a company has NOT configured, which
//     is worth as much to an attacker as the configuration.
//   - /operator: the operator MCP surface, which files and moves work and
//     writes the knowledge base. Deliberately not under /mcp/, which is exempt
//     wholesale for the sandbox bridge.
//   - /chart and /company: the org chart, whose other half is a seat's model
//     chain, credentials, sandbox cell and mcp_env — the company configuration
//     under another name.
//   - /work and /pages: the human write surface over the tracker and the
//     knowledge base. A write there with nobody behind it would be a record
//     with no author, which is the one thing it exists never to write.
func TestTheCompanysOwnSurfacesAreGuardedEvenForReads(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{
		"/config", "/secrets", "/setup", "/operator", "/chart", "/company",
		"/work", "/pages",
	} {
		for _, path := range []string{
			prefix, prefix + "/", prefix + "/anything",
			prefix + "/deep/er?reveal=true",
		} {
			if auth.Unguarded(path) {
				t.Errorf("%s is exempt from the guard", path)
			}
		}
	}
}

// THE CONTROL FOR THE RULE ABOVE. If every path were guarded the assertion
// would pass for the wrong reason, so the exemptions have to still exempt —
// and they are the only thing that does.
func TestOnlyTheDeclaredExemptionsAreUnguarded(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/health", "/ready", "/", "/webhooks/slack"} {
		if !auth.Unguarded(path) {
			t.Errorf("%s is declared exempt and was guarded anyway", path)
		}
	}
	if auth.Unguarded("/events") {
		t.Error("/events is not exempt and was served without a credential")
	}
}

func TestTheProbesAndTheShellAreNeverGuarded(t *testing.T) {
	t.Parallel()
	// An orchestrator has no token, and a liveness check that 401s is a
	// liveness check that fails. The page that prompts for a token cannot
	// itself require one.
	for _, path := range []string{
		"/", "/health", "/ready", "/dashboard", "/favicon.ico",
		"/health/", "/ready/", "/dashboard/",
		"/static/dashboard/app.js", "/webhooks/slack", "/otlp/tok/v1/traces",
		// The MCP bridge: a coding agent inside a sandbox holds no API
		// token, and giving it one would hand a box the credential that
		// reads the whole company. Its per-run signed token IS the check.
		"/mcp/tok",
	} {
		if !auth.Unguarded(path) {
			t.Errorf("%s was guarded", path)
		}
	}
}

func TestASiblingOfAProbeIsGuarded(t *testing.T) {
	t.Parallel()
	// The exact/prefix split is the point. As prefixes, /health and /ready
	// would silently have exempted any future route merely starting with
	// those letters, on the day it was added.
	for _, path := range []string{"/health-admin", "/readyz-reset", "/healthz", "/dashboards"} {
		if auth.Unguarded(path) {
			t.Errorf("%s was exempted as if it were a probe", path)
		}
	}
}

func TestOnlyOneTrailingSlashIsNormalised(t *testing.T) {
	t.Parallel()
	// A load balancer probing /health/ must not be 401'd — the guard runs
	// before routing, so the mux's own redirect never happens. But the
	// normalisation must not widen anything beyond that one spelling.
	if !auth.Unguarded("/health/") {
		t.Error("/health/ was guarded, so a probe with a trailing slash 401s")
	}
	for _, path := range []string{"/health//", "/health/sub", "//health"} {
		if auth.Unguarded(path) {
			t.Errorf("%s was exempted by the trailing-slash normalisation", path)
		}
	}
	// The root is one character and must not be normalised into "".
	if !auth.Unguarded("/") {
		t.Error("/ was guarded")
	}
}

// --- the token comparison ------------------------------------------------ //

func TestAValidTokenAuthenticatesAsItsOperator(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(
		config.APIToken{ID: "founder", Token: "secret-a"},
		config.APIToken{ID: "ci", Token: "secret-b"},
	))
	for id, token := range map[string]string{"founder": "secret-a", "ci": "secret-b"} {
		got, ok := g.Operator(token)
		if !ok || got != id {
			t.Errorf("token for %q resolved to %q/%v", id, got, ok)
		}
	}
}

func TestAWrongOrMissingTokenAuthenticatesAsNobody(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	for _, candidate := range []string{"", "wrong", "secre", "secrett", "SECRET"} {
		if _, ok := g.Operator(candidate); ok {
			t.Errorf("%q authenticated", candidate)
		}
	}
}

func TestTheBearerSchemeIsCaseInsensitiveAndTrimmed(t *testing.T) {
	t.Parallel()
	// Clients spell it every way, and a scheme comparison that was
	// case-sensitive would reject a conforming client for its
	// capitalisation.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	for _, header := range []string{"Bearer secret", "bearer secret", "BEARER secret", "Bearer   secret  "} {
		res, seen := serve(t, g, "POST", "/agents", header)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%q: status = %d", header, res.StatusCode)
		}
		if seen != "founder" {
			t.Errorf("%q: operator = %q", header, seen)
		}
	}
}

func TestANonBearerHeaderIsRefused(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	for _, header := range []string{"secret", "Basic c2VjcmV0", "Bearer", "bearertoken"} {
		res, _ := serve(t, g, "POST", "/agents", header)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", header, res.StatusCode)
		}
	}
}

// --- the postures -------------------------------------------------------- //

func TestNoTokensRefusesEveryGuardedRoute(t *testing.T) {
	t.Parallel()
	// NO LONGER A POSTURE ANYBODY SHOULD BE IN, which config refuses once
	// the API is served — but the guard must not fail, so what it does
	// here is still worth pinning: no candidate can match, so everything
	// but the exemptions answers 401. It used to serve every read.
	g := guard(t, nil)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/events"}, {"POST", "/agents"},
		{"GET", "/config"}, {"POST", "/config/revisions"},
	} {
		res, _ := serve(t, g, tc.method, tc.path, "Bearer anything")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401: no token can match", tc.method, tc.path, res.StatusCode)
		}
	}
	// The exemptions still serve, or the assertion above would pass on a
	// guard that refused literally everything.
	if res, _ := serve(t, g, "GET", "/health", ""); res.StatusCode != http.StatusOK {
		t.Errorf("the liveness probe = %d, want it served", res.StatusCode)
	}
}

func TestNoBootstrapAtAllStillGuardsWrites(t *testing.T) {
	t.Parallel()
	// Tier A supplies the POSTURE, never the existence of a check. The
	// hole is a middleware mounted only when Tier A is present while the
	// /config write surface is gated on a store being configured — two
	// independent conditions deciding one security property.
	g := auth.New(nil)
	res, _ := serve(t, g, "POST", "/agents", "Bearer anything")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no Tier A at all", res.StatusCode)
	}
	if res, _ := serve(t, g, "GET", "/events", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("reads = %d, want 401: with no Tier A nobody has said who "+
			"may act, and the read surface carries every LLM transcript this "+
			"company has produced", res.StatusCode)
	}
}

func TestARefusalSaysSoInJSONAndNothingElse(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	res, seen := serve(t, g, "POST", "/agents", "Bearer wrong")

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if seen != "" {
		t.Error("the handler ran despite the refusal")
	}
}

func TestAnUnguardedRouteReachesTheHandlerWithNoOperator(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	res, seen := serve(t, g, "GET", "/health", "")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d", res.StatusCode)
	}
	if seen != "" {
		t.Errorf("operator = %q, want none attached on an unguarded route", seen)
	}
}

func TestAValidTokenIsAttributedEvenWhereItIsNotRequired(t *testing.T) {
	t.Parallel()
	// Attribution and authorization are different questions. A route that
	// does not REQUIRE a token can still be told who presented one, which
	// is what lets an operator-only query be answered on a surface the
	// anonymous-read posture lets through.
	//
	// Resolving only on guarded routes made that unreachable: the query
	// arrived with a valid token, no operator attached, and came back
	// unauthorized to a caller holding the right credential.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	// An unguarded route under an open read posture.
	if _, seen := serve(t, g, "GET", "/events", "Bearer secret"); seen != "founder" {
		t.Errorf("operator = %q on an unguarded read, want founder", seen)
	}
	// And an exempt one.
	if _, seen := serve(t, g, "GET", "/health", "Bearer secret"); seen != "founder" {
		t.Errorf("operator = %q on an exempt route, want founder", seen)
	}
}

func TestNobodyIsReportedAsAbsentRatherThanAsAnEmptyName(t *testing.T) {
	t.Parallel()
	// A FAILED RESOLUTION IS A FINDING, never a principal with an empty
	// name: attached as one, a caller asking "is somebody there" — which
	// is the whole point of the resolution — is told yes.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	var present bool
	handler := g.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, how := iam.From(r.Context())
		present = how == iam.Resolved
	}))
	req := httptest.NewRequest("GET", "/events", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if present {
		t.Error("a rejected credential was attached as an operator")
	}
}

func TestAWrongTokenIsNotAttributedOnAnUnguardedRoute(t *testing.T) {
	t.Parallel()
	// The counterfactual, and the half that matters: resolving the
	// credential everywhere must not mean accepting it everywhere. A
	// route that does not require a token still serves — that is what
	// unguarded means — but the caller is nobody.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	res, seen := serve(t, g, "GET", "/health", "Bearer wrong")

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d: a bad token closed an unguarded route", res.StatusCode)
	}
	if seen != "" {
		t.Errorf("operator = %q, want nobody", seen)
	}
}

// --- the bind posture ---------------------------------------------------- //

func TestLoopbackBindsAreRecognised(t *testing.T) {
	t.Parallel()
	// Anonymous reads on one of these are a laptop; on anything else they
	// are a decision somebody may not have made deliberately.
	for _, host := range []string{"127.0.0.1", "::1", "[::1]", "localhost", "LOCALHOST", " localhost "} {
		if !auth.BindIsLoopback(host) {
			t.Errorf("%q was not recognised as loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "", "10.0.0.1", "example.com", "localhost.evil.com"} {
		if auth.BindIsLoopback(host) {
			t.Errorf("%q was treated as loopback", host)
		}
	}
}

func TestTheGuardReportsItsOwnPosture(t *testing.T) {
	t.Parallel()
	// The startup line has to be able to state what was loaded, which is
	// the difference between an operator knowing their posture and
	// assuming it. One number now rather than three: there is no read
	// posture and no disabled posture left to state.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	if g.Tokens() != 1 {
		t.Errorf("posture = tokens %d, want 1", g.Tokens())
	}
	if auth.New(nil).Tokens() != 0 {
		t.Error("a guard built from no Tier A reported credentials it does not hold")
	}
}

func TestASocketCanAttachItsOwnOperator(t *testing.T) {
	t.Parallel()
	// The dashboard's socket carries its credential as a query parameter
	// rather than a header, so the stream handler authenticates it itself
	// and hands the id down the same way the middleware does.
	ctx := auth.WithOperator(t.Context(), "founder")
	principal, how := iam.From(ctx)
	if how != iam.Resolved || auth.OperatorID(principal) != "founder" {
		t.Errorf("operator = %q/%v", auth.OperatorID(principal), how)
	}
	// AND IT CARRIES NO AUTHORITY, which is what makes attaching one
	// outside the guard safe: it names a writer and opens nothing.
	if len(principal.Grants) != 0 {
		t.Errorf("an attributed principal carries grants: %v", principal.Grants)
	}
	// A BARE CONTEXT IS UNKNOWN, not anonymous: a handler nobody wired
	// through the guard looks exactly like one whose resolver found
	// nobody, and reading either as the other is how a surface decides it
	// is safe to serve because nothing told it otherwise.
	if _, how := iam.From(t.Context()); how != iam.Unknown {
		t.Errorf("a bare context resolved as %q, want unknown", how)
	}
}

func TestAnEmptyConfiguredTokenIsNotABypass(t *testing.T) {
	t.Parallel()
	// Config refuses an empty token value, but Bootstrap is an exported
	// struct an embedder can build directly — and a token that resolved to
	// "" because its environment variable was unset would otherwise match
	// a request presenting no credential at all.
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: ""}}
	g := auth.New(&b)

	if _, ok := g.Operator(""); ok {
		t.Error("an empty candidate authenticated against an empty token")
	}
	res, seen := serve(t, g, "POST", "/config/revisions", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
	if seen != "" {
		t.Errorf("operator = %q", seen)
	}
	// And the counterfactual: the empty value is still refused when it is
	// presented explicitly.
	if res, _ := serve(t, g, "POST", "/agents", "Bearer "); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an explicit empty bearer = %d, want 401", res.StatusCode)
	}
}

// THE GUARD DOES NOT FAIL, SO CONFIG HAS TO, and this is where the two meet:
// every posture that would leave this surface unreachable is refused by
// `crewlet validate` on a laptop rather than by a process at bind time.
//
// The list moved wholesale when `allow_anonymous_read` went. It used to hold
// one shape — no tokens with reads closed — because with reads open a
// credential-less deployment was a working read-only one. There is no read
// posture now, so a served API with no credential is unreachable full stop,
// and three more facts became load-bearing at the same time: the ceiling the
// directory is clamped by, the address the cookie's flags come from, and the
// keyring that signs it.
func TestConfigRefusesEveryPostureThisSurfaceCannotBeReachedUnder(t *testing.T) {
	t.Parallel()
	complete := func() config.Bootstrap {
		b := config.DefaultBootstrap()
		b.API.Port = 8000
		b.API.ExternalURL = "http://localhost:8000"
		b.API.Auth.MaxGrants = iam.AllGrants
		b.API.Auth.Tokens = []config.APIToken{{
			ID: "founder", Token: strings.Repeat("k", 32),
			Grants: []iam.Grant{iam.GrantConfigWrite},
		}}
		b.Secrets = config.Secrets{
			ActiveKeyID: "k1",
			Keys: []config.SecretKey{{
				ID: "k1", Material: base64.StdEncoding.EncodeToString(make([]byte, 32)),
			}},
		}
		return b
	}
	// THE CONTROL FIRST. Without it every case below could be passing on
	// some unrelated refusal, which is how a suite ends up asserting that
	// validation fails rather than that it fails for this reason.
	control := complete()
	if err := control.Validate(); err != nil {
		t.Fatalf("the complete posture does not validate, so every case below "+
			"may be failing on something else: %v", err)
	}
	for _, tc := range []struct {
		name string
		make func(*config.Bootstrap)
		want string
	}{
		{"no credential at all", func(b *config.Bootstrap) {
			b.API.Auth.Tokens = nil
		}, "at least one token is required"},
		{"no ceiling on what the directory may confer", func(b *config.Bootstrap) {
			b.API.Auth.MaxGrants = nil
		}, "required once `api.port` is set"},
		{"no address a browser reaches this on", func(b *config.Bootstrap) {
			b.API.ExternalURL = ""
		}, "required once `api.port` is set"},
		{"no keyring to sign a session with", func(b *config.Bootstrap) {
			b.Secrets = config.Secrets{}
		}, "signs every session cookie"},
		{"a credential short enough to guess", func(b *config.Bootstrap) {
			b.API.Auth.Tokens[0].Token = "short"
		}, "at least"},
		{"a credential whose blast radius nobody stated", func(b *config.Bootstrap) {
			b.API.Auth.Tokens[0].Grants = nil
		}, "stated where it is pinned"},
	} {
		b := complete()
		tc.make(&b)
		err := b.Validate()
		if err == nil {
			t.Errorf("%s: validated", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error does not say why: %v", tc.name, err)
		}
	}
}

// THE COUNTERFACTUAL. A node that serves no HTTP at all needs none of it —
// every rule above is gated on `api.port`, and a worker node that had to
// declare a ceiling, an address and a token for a surface it does not bind
// would be four settings of ceremony for nothing.
func TestANodeServingNoApiNeedsNoneOfIt(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	if b.API.Port != 0 {
		t.Fatal("the default binds a port, so this case is not the one it names")
	}
	if err := b.Validate(); err != nil {
		t.Errorf("a node serving no API was refused: %v", err)
	}
}

// --- the socket's query token -------------------------------------------- //

// THE SOCKET PATH TAKES ITS TOKEN FROM THE QUERY, AND NOTHING ELSE DOES.
//
// A browser cannot set a header on a WebSocket constructor, so the dashboard
// sends its token as ?token= on /ws/stream. The middleware used to read the
// header alone and leave the query to the stream handler, and under a closed
// posture it answered 401 before that handler ever ran: the dashboard could
// not connect with a valid token on the one posture whose point is that the
// token is required.
func TestTheSocketPathTakesItsTokenFromTheQuery(t *testing.T) {
	t.Parallel()
	g := guard(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	})

	res, seen := serve(t, g, http.MethodGet, auth.SocketPath+"?token=secret", "")
	if res.StatusCode != http.StatusOK || seen != "founder" {
		t.Fatalf("a valid query token on the socket path = %d as %q, want 200 as founder",
			res.StatusCode, seen)
	}
	res, _ = serve(t, g, http.MethodGet, auth.SocketPath+"?token=wrong", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong query token on the socket path = %d, want 401", res.StatusCode)
	}
	// The header still works there, and wins over a stale query.
	res, seen = serve(t, g, http.MethodGet, auth.SocketPath+"?token=stale", "Bearer secret")
	if res.StatusCode != http.StatusOK || seen != "founder" {
		t.Errorf("a header beside a stale query = %d as %q, want 200 as founder", res.StatusCode, seen)
	}
	// And a token in the URL of any OTHER route authenticates nobody: a URL
	// lands in proxy logs and browser history, which is a price paid for
	// exactly one route that has no alternative.
	for _, path := range []string{"/events?token=secret", "/config?token=secret", "/ws/streams?token=secret"} {
		res, seen := serve(t, g, http.MethodGet, path, "")
		if res.StatusCode != http.StatusUnauthorized || seen != "" {
			t.Errorf("%s = %d as %q, want 401 as nobody", path, res.StatusCode, seen)
		}
	}
}

// THE GUARD REFUSES IN THE ENVELOPE EVERY OTHER SURFACE ANSWERS WITH.
//
// It used to write the body by hand — a Content-Type, a WriteHeader and a JSON
// literal, three lines that say nothing about which vocabulary the code comes
// from or where the sentence beside it lives. A caller refused HERE, before any
// route runs, and a caller refused BY a route must read one shape, because the
// dashboard's token gate branches on both: a stale credential is refused at
// this middleware and the screen that offers to forget it renders the same
// `message` it would for any other refusal.
func TestTheGuardsRefusalIsTheSharedEnvelope(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	res, _ := serve(t, g, "POST", "/config", "Bearer stale-from-last-deployment")

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "" {
		t.Errorf("X-Content-Type-Options = %q: nosniff beside a JSON body is the "+
			"one pairing that stops a strict client parsing it", got)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}
	if body["error"] != string(httpjson.CodeInvalidToken) {
		t.Errorf("error = %v, want %q", body["error"], httpjson.CodeInvalidToken)
	}
	if body["message"] != httpjson.CodeInvalidToken.Message() {
		t.Errorf("message = %v, want the code's own sentence — this is what a "+
			"person is shown when their token stops working", body["message"])
	}
}
