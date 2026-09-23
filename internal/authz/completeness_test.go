package authz_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY VERB THIS BUILD KNOWS HAS A RULE, AND EVERY RULE NAMES A CLASS.
//
// The walk that makes [authz.Decide]'s unknown-action arm a build failure
// rather than a production one. A verb added to the table with a class the
// switch does not handle, or a class declared and never used, is a gate
// somebody wrote half of — and both halves look finished from the other side.
func TestEveryActionResolvesToADeclaredClass(t *testing.T) {
	t.Parallel()
	actions := authz.Actions()
	if len(actions) == 0 {
		t.Fatal("the table is empty, so every walk below certifies nothing")
	}
	for _, a := range actions {
		class, known := authz.ClassOf(a)
		if !known {
			t.Errorf("%q is in the table and resolves to no class", a)
			continue
		}
		if !slices.Contains(authz.Classes, class) {
			t.Errorf("%q names the class %q, which is not one this build "+
				"declares", a, class)
		}
	}
}

// AND EVERY DECLARED CLASS IS REACHED BY AT LEAST ONE VERB.
//
// THE OTHER DIRECTION, and it is the half that decays quietly: a class nobody
// uses is a rule nothing exercises, so the day a verb is moved onto it the
// first exercise it ever gets is in production. It is the same two-sided
// shape internal/skipgate uses for a structural skip that stopped firing.
func TestEveryDeclaredClassHasAVerb(t *testing.T) {
	t.Parallel()
	used := map[authz.Class]bool{}
	for _, a := range authz.Actions() {
		if class, ok := authz.ClassOf(a); ok {
			used[class] = true
		}
	}
	for _, c := range authz.Classes {
		if !used[c] {
			t.Errorf("the class %q is declared and no verb is decided by it, "+
				"so nothing exercises the rule", c)
		}
	}
}

// EVERY MOUNTED PATTERN HAS A POLICY, BECAUSE THERE IS NO OTHER WAY TO MOUNT.
//
// The route walk. It is worth being exact about what it certifies: the
// router is the only thing that can register a pattern AND record it, so the
// walk cannot find a pattern with no policy — what it actually proves is
// that a route the caller believes it mounted is one the router is guarding.
// A route registered straight on the mux is invisible here, which is why
// internal/api keeps no other reference to its own.
func TestEveryMountedPatternHasAPolicy(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	router := authz.NewRouter(mux, allow)
	for _, pattern := range []string{
		"GET /work/items", "PATCH /work/items/{key}", "GET /config",
	} {
		if err := router.Handle(pattern, policyFor(pattern), ok200()); err != nil {
			t.Fatalf("mount %s: %v", pattern, err)
		}
	}
	patterns := router.Patterns()
	if len(patterns) != 3 {
		t.Fatalf("mounted %v, want three", patterns)
	}
	for _, pattern := range patterns {
		p, found := router.PolicyFor(pattern)
		if !found {
			t.Errorf("%s is mounted and carries no policy", pattern)
			continue
		}
		if _, known := authz.ClassOf(p.Action); !known {
			t.Errorf("%s names the action %q, which has no rule", pattern, p.Action)
		}
	}
}

// AND A ROUTE MOUNTED WITHOUT ONE IS REFUSED WHERE IT IS WRITTEN.
//
// The control, and the reason the check is at mount rather than in the walk:
// a test sees only the mux it builds, so a route mounted in production alone
// is invisible to it. Refusing at registration is what covers that — it fails
// wherever the route table runs, including in production, before a request
// has been served.
func TestARouteWithNoPolicyIsRefusedAtTheMount(t *testing.T) {
	t.Parallel()
	router := authz.NewRouter(http.NewServeMux(), allow)
	for _, c := range []struct {
		name    string
		pattern string
		policy  authz.Policy
		want    string
	}{
		{"no action", "GET /work/items", authz.Policy{}, "names no action"},
		{"a verb with no rule", "GET /work/items",
			authz.Policy{Action: "invent_a_verb"}, "no rule for"},
		{"no pattern", "", authz.Policy{Action: authz.ActionWorkList}, "needs a pattern"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := authz.NewRouter(http.NewServeMux(), allow).
				Handle(c.pattern, c.policy, ok200())
			if err == nil {
				t.Fatalf("%q mounted with %+v", c.pattern, c.policy)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
	// AND THE SAME PATTERN TWICE, which ServeMux panics on at runtime and
	// which is reported here instead, naming the pattern.
	if err := router.Handle("GET /work/items",
		authz.Policy{Action: authz.ActionWorkList}, ok200()); err != nil {
		t.Fatalf("the first mount failed: %v", err)
	}
	if err := router.Handle("GET /work/items",
		authz.Policy{Action: authz.ActionWorkList}, ok200()); err == nil {
		t.Error("the same pattern mounted twice was accepted, so which handler " +
			"runs is whichever ServeMux kept")
	}
}

// EVERY GRANT THE VOCABULARY DECLARES IS ASKED FOR BY SOMETHING.
//
// A capability nothing consults is a capability an operator can be given and
// which opens nothing, which reads to them as a gate that is broken. Walked
// from [iam.AllGrants] rather than from this package's table, so the day the
// vocabulary grows an eleventh the walk fails until a verb asks for it.
func TestEveryGrantIsAskedForBySomeVerb(t *testing.T) {
	t.Parallel()
	asked := map[iam.Grant]bool{}
	for _, a := range authz.Actions() {
		for _, g := range iam.AllGrants {
			d := authz.Decide(t.Context(), grantedOnly(g), a,
				authz.Object{Kind: authz.KindCompany}, authz.NoChart{})
			if d.Allowed && d.Reason == authz.ReasonGrant {
				asked[g] = true
			}
		}
	}
	// THE WRITE GRANTS ARE ASKED THROUGH AN OBJECT, so they are walked
	// again over the kinds that carry them: the company object above
	// reaches only the operator rows.
	for _, kind := range authz.ObjectKinds {
		for _, a := range authz.Actions() {
			for _, g := range iam.AllGrants {
				d := authz.Decide(t.Context(), grantedOnly(g), a,
					authz.Object{Kind: kind}, authz.NoChart{})
				if d.Allowed && d.Reason == authz.ReasonGrant {
					asked[g] = true
				}
			}
		}
	}
	for _, g := range iam.AllGrants {
		if !asked[g] {
			t.Errorf("no verb in this build opens anything for %q, so a "+
				"principal holding it gains nothing and reads it as broken", g)
		}
	}
}

// A REQUEST WHOSE CLEANED PATH DIFFERS FROM ITS RAW ONE IS REFUSED.
//
// A gate is worth exactly as much as the agreement between what it matched
// and what a reader thinks it matched. `/work/items/../config` reaches
// ServeMux as one pattern and reads to a person as another.
//
// REFUSED, NOT REDIRECTED: ServeMux's own answer is a redirect, which every
// client follows — so the request arrives a second time with the decision
// still unmade, and the access log shows only the clean path.
//
// AND THEREFORE OUTSIDE THE MUX. Written inside the per-route wrapper, beside
// the decision it protects, every case here came back 307: ServeMux cleans
// and redirects BEFORE it matches, so the guard was never entered at all.
// That is what put [authz.CanonicalPath] in the outer middleware.
func TestACleanedPathDifferingFromTheRawOneIsRefused(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	router := authz.NewRouter(mux, allow)
	if err := router.Handle("GET /work/items",
		authz.Policy{Action: authz.ActionWorkList}, ok200()); err != nil {
		t.Fatalf("mount: %v", err)
	}
	guarded := authz.CanonicalPath(mux)
	for _, c := range []struct {
		path string
		want int
	}{
		{"/work/items", http.StatusOK},
		{"/work/items/../work/items", http.StatusBadRequest},
		{"/work//items", http.StatusBadRequest},
		{"/./work/items", http.StatusBadRequest},
		// ENCODED, so the escaped path is already clean and only the
		// decoded one — which ServeMux cleans and redirects — is not.
		{"/work/items/%2e%2e/work/items", http.StatusBadRequest},
	} {
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://x"+c.path, nil)
			guarded.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s answered %d, want %d", c.path, rec.Code, c.want)
			}
			if c.want == http.StatusBadRequest {
				body := envelope(t, rec)
				if body["error"] != string(httpjson.CodeNonCanonicalPath) {
					t.Errorf("%s refused as %v, want the envelope's own code",
						c.path, body["error"])
				}
			}
		})
	}
}

// AN UNDECIDABLE REQUEST ANSWERS 503, AND A REFUSED ONE 403.
//
// The two outcomes a surface must not fold together: 403 tells somebody they
// may not, and sends them to ask for an authority they may already hold.
func TestAnUndecidableRequestIsNotAForbiddenOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		guard authz.Guard
		want  int
	}{
		{"allowed", allow, http.StatusOK},
		{"refused", func(*http.Request, authz.Policy) authz.Decision {
			return authz.Decision{Reason: authz.ReasonNoGrant}
		}, http.StatusForbidden},
		{"undecidable", func(*http.Request, authz.Policy) authz.Decision {
			return authz.Decision{Err: authz.ErrNoChart}
		}, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			if err := authz.NewRouter(mux, c.guard).Handle("GET /work/items",
				authz.Policy{Action: authz.ActionWorkList}, ok200()); err != nil {
				t.Fatalf("mount: %v", err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(
				http.MethodGet, "http://x/work/items", nil))
			if rec.Code != c.want {
				t.Errorf("answered %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// THE ROUTER'S OWN REFUSAL IS THE ENGINE'S ENVELOPE, not net/http's plain text.
//
// The /iam and /chart surfaces state no wording of their own, so every
// refusal they make is this one — and it was `http.Error`: `text/plain` with
// `nosniff`, a sentence rather than a code, and a 503 with no Retry-After.
// A client of the rest of this API could not branch on it, could not name the
// grant it lacked, and could not tell "try again" from "this node is gone".
// So both halves are asserted on the WIRE: the content type, the code, the
// sentence, the reason and grants on a refusal, and the Retry-After on the
// undecidable arm.
func TestTheRoutersOwnRefusalIsTheEnvelope(t *testing.T) {
	t.Parallel()
	refused := authz.Decide(t.Context(), grantedOnly(iam.GrantStateRead),
		authz.ActionConfigWrite, authz.Object{Kind: authz.KindCompany}, authz.NoChart{})
	if refused.Allowed || refused.Unknown() {
		t.Fatalf("the fixture decision was %+v, want a plain refusal", refused)
	}
	for _, c := range []struct {
		name   string
		d      authz.Decision
		status int
		code   httpjson.Code
	}{
		{"refused", refused, http.StatusForbidden, httpjson.CodeUnauthorized},
		{"undecidable", authz.Decision{Err: authz.ErrNoChart},
			http.StatusServiceUnavailable, httpjson.CodeUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			guard := func(*http.Request, authz.Policy) authz.Decision { return c.d }
			if err := authz.NewRouter(mux, guard).Handle("PATCH /config",
				authz.Policy{Action: authz.ActionConfigWrite}, ok200()); err != nil {
				t.Fatalf("mount: %v", err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "http://x/config", nil))
			if rec.Code != c.status {
				t.Fatalf("answered %d, want %d", rec.Code, c.status)
			}
			body := envelope(t, rec)
			if body["error"] != string(c.code) || body["message"] != c.code.Message() {
				t.Errorf("answered %v, want %s with its sentence", body, c.code)
			}
			if c.status == http.StatusServiceUnavailable {
				if rec.Header().Get("Retry-After") !=
					strconv.Itoa(authz.RetryUndecidedSeconds) {
					t.Errorf("Retry-After = %q, want %d: a 503 without one reads "+
						"as a node that is down for good",
						rec.Header().Get("Retry-After"), authz.RetryUndecidedSeconds)
				}
				return
			}
			if body[authz.DetailReason] != string(authz.ReasonNoGrant) {
				t.Errorf("reason = %v, want the rule's own", body[authz.DetailReason])
			}
			grants, _ := body[authz.DetailGrants].([]any)
			if len(grants) != 1 || grants[0] != string(iam.GrantConfigWrite) {
				t.Errorf("grants = %v, want the grant the rule consulted",
					body[authz.DetailGrants])
			}
		})
	}
}

// envelope reads a refusal and insists it is the engine's JSON envelope rather
// than text some other writer produced.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json:\n%s", got, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v\n%s", err, rec.Body.String())
	}
	return body
}

// A SURFACE'S OWN REFUSAL RENDERS WHAT THE ROUTER DECIDED, and nothing else.
//
// The seam exists so a surface whose verbs are also tools can answer in the
// tools' own sentence; what it must never become is a second decision. So the
// renderer sees every refusal and every undecidable request, and an ADMITTED
// request never reaches it — a renderer that could be handed an allowed
// request would be one that could turn it into a refusal.
func TestARefusalRendererRendersOnlyWhatTheRouterRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		guard    authz.Guard
		rendered bool
	}{
		{"allowed", allow, false},
		{"refused", func(*http.Request, authz.Policy) authz.Decision {
			return authz.Decision{Reason: authz.ReasonNoGrant}
		}, true},
		{"undecidable", func(*http.Request, authz.Policy) authz.Decision {
			return authz.Decision{Err: authz.ErrNoChart}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var seen *authz.Decision
			mux := http.NewServeMux()
			router := authz.NewRouter(mux, c.guard).Refusing(
				func(w http.ResponseWriter, _ *http.Request, p authz.Policy,
					d authz.Decision) {
					seen = &d
					if p.Action != authz.ActionWorkList {
						t.Errorf("the renderer was handed %q, not the route's own "+
							"policy", p.Action)
					}
					w.WriteHeader(http.StatusTeapot)
				})
			if err := router.Handle("GET /work/items",
				authz.Policy{Action: authz.ActionWorkList}, ok200()); err != nil {
				t.Fatalf("mount: %v", err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(
				http.MethodGet, "http://x/work/items", nil))
			switch {
			case c.rendered && (seen == nil || rec.Code != http.StatusTeapot):
				t.Errorf("a refusal answered %d without the surface's renderer",
					rec.Code)
			case !c.rendered && (seen != nil || rec.Code != http.StatusOK):
				t.Errorf("an admitted request reached the renderer and answered %d",
					rec.Code)
			}
		})
	}
}

// --- fixtures -------------------------------------------------------- //

// allow is a guard that permits everything, for the cases that are about
// registration and the request path rather than about the decision.
func allow(*http.Request, authz.Policy) authz.Decision {
	return authz.Decision{Allowed: true, Reason: authz.ReasonGrant}
}

func ok200() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func policyFor(pattern string) authz.Policy {
	switch {
	case strings.Contains(pattern, "/config"):
		return authz.Policy{Action: authz.ActionConfigRead}
	case strings.Contains(pattern, "PATCH"):
		return authz.Policy{Action: authz.ActionWorkUpdate}
	}
	return authz.Policy{Action: authz.ActionWorkList}
}

// grantedOnly is an active person carrying exactly one capability.
func grantedOnly(g iam.Grant) iam.Principal {
	p := person("jane.doe", g)
	return p
}

// THE SHARED GUARD DECIDES A CALLER IT COULD NOT RESOLVE AS UNKNOWN.
//
// It is the clause four surfaces each wrote for themselves and the one that is
// easy to drop: decided as the zero principal, an unreadable identity estate is
// a 403 naming a grant the caller may very well hold, on every request, for as
// long as the estate is down. Three resolutions, three answers — and the
// resolved caller is decided on the object the policy builds, so a guard that
// dropped the object would refuse every relation rule.
func TestTheContextGuardDecidesAnUnresolvedCallerAsUnknown(t *testing.T) {
	t.Parallel()
	guard := authz.ContextGuard(authz.NoChart{})
	policy := authz.Policy{Action: authz.ActionInboxMark,
		Object: func(*http.Request) authz.Object {
			return authz.Object{Kind: authz.KindPerson, Owner: "jane.doe"}
		}}
	request := func(ctx func(*http.Request) *http.Request) *http.Request {
		return ctx(httptest.NewRequest(http.MethodPut, "http://x/work/people/jane.doe/inbox", nil))
	}

	unknown := guard(request(func(r *http.Request) *http.Request {
		return r.WithContext(iam.WithUnresolved(r.Context(), errors.New("estate unreadable")))
	}), policy)
	if !unknown.Unknown() {
		t.Errorf("an unresolved caller decided %+v, want UNKNOWN", unknown)
	}

	anonymous := guard(request(func(r *http.Request) *http.Request {
		return r.WithContext(iam.WithAnonymous(r.Context()))
	}), policy)
	if anonymous.Unknown() || anonymous.Allowed {
		t.Errorf("an anonymous caller decided %+v, want a refusal", anonymous)
	}

	self := guard(request(func(r *http.Request) *http.Request {
		return r.WithContext(iam.WithPrincipal(r.Context(), person("jane.doe")))
	}), policy)
	if !self.Allowed || self.Reason != authz.ReasonSelf {
		t.Errorf("the owner of the record decided %+v, want admitted as self", self)
	}
}

// ADMIT ASKS THE SAME GUARD AND ANSWERS THE ROUTER'S REFUSAL.
func TestAdmitAnswersTheRoutersOwnRefusal(t *testing.T) {
	t.Parallel()
	refuse := func(*http.Request, authz.Policy) authz.Decision {
		return authz.Decision{Reason: authz.ReasonNoGrant,
			Grants: []iam.Grant{iam.GrantSecretRead}}
	}
	rec := httptest.NewRecorder()
	if authz.Admit(rec, httptest.NewRequest(http.MethodGet, "http://x/secrets/A", nil),
		refuse, authz.Policy{Action: authz.ActionSecretReveal}) {
		t.Fatal("a refused decision was admitted")
	}
	body := envelope(t, rec)
	if rec.Code != http.StatusForbidden || body[authz.DetailReason] != string(authz.ReasonNoGrant) {
		t.Errorf("answered %d %v, want the router's 403", rec.Code, body)
	}
	if !authz.Admit(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/", nil),
		allow, authz.Policy{Action: authz.ActionSecretReveal}) {
		t.Error("an admitted decision was refused")
	}
}
