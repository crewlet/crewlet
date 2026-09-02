package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
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
		seen, _ = auth.OperatorFrom(r.Context())
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

func TestWritesNeedATokenAndReadsDoNotByDefault(t *testing.T) {
	t.Parallel()
	// allow_anonymous_read defaults on, and what it opens is worth naming:
	// /events and /agents/{id}/memory carry full LLM transcripts.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		if g.Requires("/events", method) {
			t.Errorf("%s /events required a token under anonymous read", method)
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if !g.Requires("/agents", method) {
			t.Errorf("%s /agents served without a token", method)
		}
	}
}

func TestConfigIsGuardedEvenForReads(t *testing.T) {
	t.Parallel()
	// Reading it exposes the whole company document, and writing it
	// changes the company — so it is never eligible for anonymous read.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	for _, path := range []string{"/config", "/config/", "/config/revisions", "/config/audit"} {
		if !g.Requires(path, "GET") {
			t.Errorf("GET %s served without a token", path)
		}
	}
}

func TestSecretsIsGuardedEvenForReads(t *testing.T) {
	t.Parallel()
	// The fleet's credential store. Even the listing, which carries no
	// values, says which credentials a company holds and when each last
	// changed — and one route returns a value outright. Anonymous read is
	// ON by default, so leaving this off the list would serve the whole
	// inventory to anyone who could reach the port.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	for _, path := range []string{
		"/secrets", "/secrets/", "/secrets/GITLAB_TOKEN",
		"/secrets/GITLAB_TOKEN?reveal=true",
	} {
		if !g.Requires(path, "GET") {
			t.Errorf("GET %s served without a token", path)
		}
	}
}

// EVERY ALWAYS-GUARDED PREFIX IS ACTUALLY CONSULTED.
//
// The list and the check are one function apart, and the failure mode of a
// prefix that is declared but not consulted is silent: the surface serves
// anonymously in the default posture and nothing anywhere reports it.
func TestEveryAlwaysGuardedPrefixIsEnforced(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	if len(auth.GuardedPrefixes) == 0 {
		t.Fatal("no prefix is always guarded, so this asserts nothing")
	}
	for _, prefix := range auth.GuardedPrefixes {
		if !g.Requires(prefix, "GET") {
			t.Errorf("%s is declared always-guarded and served anyway", prefix)
		}
		if !auth.AlwaysGuarded(prefix + "/anything") {
			t.Errorf("%s does not cover what is beneath it", prefix)
		}
	}
}

func TestClosingAnonymousReadClosesReadsToo(t *testing.T) {
	t.Parallel()
	g := guard(t, func(a *config.APIAuth) {
		a.AllowAnonymousRead = false
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	})
	if !g.Requires("/events", "GET") {
		t.Error("a closed posture still served reads")
	}
}

func TestTheProbesAndTheShellAreNeverGuarded(t *testing.T) {
	t.Parallel()
	// An orchestrator has no token, and a liveness check that 401s is a
	// liveness check that fails. The page that prompts for a token cannot
	// itself require one.
	g := guard(t, func(a *config.APIAuth) {
		a.AllowAnonymousRead = false
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	})
	for _, path := range []string{
		"/", "/health", "/ready", "/dashboard", "/favicon.ico",
		"/health/", "/ready/", "/dashboard/",
		"/static/dashboard/app.js", "/webhooks/slack", "/otlp/tok/v1/traces",
		// The MCP bridge: a coding agent inside a sandbox holds no API
		// token, and giving it one would hand a box the credential that
		// reads the whole company. Its per-run signed token IS the check.
		"/mcp/tok",
	} {
		if g.Requires(path, "GET") {
			t.Errorf("%s was guarded", path)
		}
	}
}

func TestASiblingOfAProbeIsGuarded(t *testing.T) {
	t.Parallel()
	// The exact/prefix split is the point. As prefixes, /health and /ready
	// would silently have exempted any future route merely starting with
	// those letters, on the day it was added.
	g := guard(t, func(a *config.APIAuth) {
		a.AllowAnonymousRead = false
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	})
	for _, path := range []string{"/health-admin", "/readyz-reset", "/healthz", "/dashboards"} {
		if !g.Requires(path, "GET") {
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

func TestNoTokensRefusesEveryWriteAndAllOfConfig(t *testing.T) {
	t.Parallel()
	// A real posture, not an oversight: a read-only deployment has no
	// credential to manage, which is strictly safer than being made to
	// mint one it will never use.
	g := guard(t, nil)

	res, _ := serve(t, g, "GET", "/events", "")
	if res.StatusCode != http.StatusOK {
		t.Errorf("a read was refused with no tokens configured: %d", res.StatusCode)
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/agents"}, {"GET", "/config"}, {"POST", "/config/revisions"},
	} {
		res, _ := serve(t, g, tc.method, tc.path, "Bearer anything")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401: no token can match", tc.method, tc.path, res.StatusCode)
		}
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
	if res, _ := serve(t, g, "GET", "/events", ""); res.StatusCode != http.StatusOK {
		t.Errorf("reads = %d, want them served", res.StatusCode)
	}
}

func TestDisabledServesEverythingAsAnonymous(t *testing.T) {
	t.Parallel()
	g := guard(t, func(a *config.APIAuth) { a.Disabled = true })

	res, seen := serve(t, g, "POST", "/config/revisions", "")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want a disabled guard to serve", res.StatusCode)
	}
	// The explicit label is what keeps a disabled-mode write
	// distinguishable in an audit row from a real operator's.
	if seen != auth.AnonymousOperator {
		t.Errorf("operator = %q, want %q", seen, auth.AnonymousOperator)
	}
	if got, ok := g.Operator(""); !ok || got != auth.AnonymousOperator {
		t.Errorf("Operator(\"\") = %q/%v", got, ok)
	}
}

func TestTheReservedIDIsTheOneTheAPIStamps(t *testing.T) {
	t.Parallel()
	// Config refuses it as a token id and the API stamps it. Two copies
	// would disagree silently, each side staying self-consistent while the
	// reservation stopped covering what is actually written.
	if auth.AnonymousOperator != config.ReservedOperatorID {
		t.Errorf("the API stamps %q but config reserves %q",
			auth.AnonymousOperator, config.ReservedOperatorID)
	}
}

// --- the middleware ------------------------------------------------------ //

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
	// OperatorFrom answers (id, ok), and the two halves have to agree. A
	// failed resolution attached as an empty NAME reads as ok=true with no
	// id — so a caller that checks ok, which is the whole point of
	// returning it, is told somebody is there.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	var present bool
	handler := g.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, present = auth.OperatorFrom(r.Context())
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
	res, seen := serve(t, g, "GET", "/events", "Bearer wrong")

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d: a bad token closed an unguarded read", res.StatusCode)
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
	// assuming it.
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	if g.Tokens() != 1 || !g.AnonymousRead() || g.Disabled() {
		t.Errorf("posture = tokens %d / read %v / disabled %v",
			g.Tokens(), g.AnonymousRead(), g.Disabled())
	}
}

func TestASocketCanAttachItsOwnOperator(t *testing.T) {
	t.Parallel()
	// The dashboard's socket carries its credential as a query parameter
	// rather than a header, so the stream handler authenticates it itself
	// and hands the id down the same way the middleware does.
	ctx := auth.WithOperator(t.Context(), "founder")
	if got, ok := auth.OperatorFrom(ctx); !ok || got != "founder" {
		t.Errorf("operator = %q/%v", got, ok)
	}
	if _, ok := auth.OperatorFrom(t.Context()); ok {
		t.Error("a bare context reported an operator")
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

func TestConfigRefusesTheShapesThatWouldLockEveryoneOut(t *testing.T) {
	t.Parallel()
	// Checked in config rather than at API startup, so `crewlet validate`
	// catches them on a laptop rather than a deployment catching them at
	// bind time.
	for _, tc := range []struct {
		name string
		auth config.APIAuth
		want string
	}{
		{
			// Every route guarded by a token that does not exist: a
			// process that starts cleanly, binds its port, and answers
			// 401 to everything including its own dashboard.
			name: "no tokens with reads closed",
			auth: config.APIAuth{AllowAnonymousRead: false},
			want: "nothing is reachable",
		},
		{
			name: "the reserved attribution as a token id",
			auth: config.APIAuth{
				AllowAnonymousRead: true,
				Tokens:             []config.APIToken{{ID: config.ReservedOperatorID, Token: "t"}},
			},
			want: "reserved",
		},
	} {
		b := config.DefaultBootstrap()
		b.API.Auth = tc.auth
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

func TestTheOrdinaryPosturesStillValidate(t *testing.T) {
	t.Parallel()
	// The counterfactual to the two refusals above: no tokens WITH reads
	// open is a real read-only deployment, and it must not be refused.
	for _, tc := range []struct {
		name string
		auth config.APIAuth
	}{
		{"read-only, no credential to manage", config.APIAuth{AllowAnonymousRead: true}},
		{"reads closed with a token", config.APIAuth{
			Tokens: []config.APIToken{{ID: "founder", Token: "secret"}},
		}},
	} {
		b := config.DefaultBootstrap()
		b.API.Auth = tc.auth
		if err := b.Validate(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
