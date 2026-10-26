package api_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// wildcardName is one `{name}` or `{name...}` of a route pattern.
var wildcardName = regexp.MustCompile(`\{([^}.$]+)(\.\.\.)?\}`)

// routeRequest is a request on pattern with every wildcard named "x", as the
// mux would hand the route's policy one.
func routeRequest(pattern string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	r := httptest.NewRequest(method, wildcardName.ReplaceAllString(path, "x"), nil)
	for _, m := range wildcardName.FindAllStringSubmatch(path, -1) {
		r.SetPathValue(m[1], "x")
	}
	return r
}

// THE ASSISTANT IS ADMITTED TO EVERY ROUTE IT MAY NAME EXACTLY AS ITS PERSON IS,
// BUT FOR THE HUMAN-ONLY LIST (ADR-0052).
//
// The route-level half of the parity walk, in its strong form, over every
// route the App binds for the assistant's generic tools (App.AssistantRoutes)
// and five people (an administrator, and a person holding only the tracker,
// only the knowledge base, only artifacts, or nothing but the overview), each
// freshly proved. Each route is asked as the grant that would carry it: a read
// grant for a GET or a HEAD, an approved exec grant for every other method, on
// the object the route's own policy builds from the matched request. The
// assistant's admission equals the person's on every route, presence rows
// included (an approval is the person at the keyboard), and differs on exactly
// the human-only list, where it is refused naming the assistant. A route the
// table binds with no verb must be one that decides inside its handler, and
// one of the four kinds that do (a question, a named read, an overview read,
// the act route), each held to parity by its own walk; any other is a route
// added with no decision, and fails here. Mutation: keep a presence row from an
// exec grant, mount a GET on a write verb, let a verb leave the human-only
// list, or bind an unguarded route, and this names the route.
func TestEveryGuardedRouteAdmitsTheAssistantAsItsPerson(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	policies := app.Policies()
	decidedInside := map[string]bool{"GET /query/{what}": true, catalog.ActRoute: true}
	for _, r := range app.OverviewRoutes() {
		decidedInside[r] = true
	}
	for r := range api.NamedReadRoutes() {
		decidedInside[r] = true
	}
	people := map[string][]iam.Grant{
		"an administrator": {iam.GrantAdmin}, "a tracker-only person": {iam.GrantWorklet},
		"a knowledge-only person": {iam.GrantDoclet}, "an artifacts-only person": {iam.GrantArtifacts},
		"an overview-only person": {},
	}
	now := time.Now()
	routes := app.AssistantRoutes()
	if len(routes) < 150 {
		t.Fatalf("the App binds %d routes for the assistant, so the walk is not reading the "+
			"table a node serves", len(routes))
	}
	neverRefused := 0
	for name, grants := range people {
		keyboard := iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson,
			Seat: "cto", Stage: iam.StageActive, Grants: grants, ReauthAt: now.Add(time.Hour)}
		admitted := 0
		for _, route := range routes {
			if route.Action == "" {
				if !decidedInside[route.Pattern] {
					t.Errorf("%s is bound for the assistant and decided by nothing: mount it "+
						"on a verb", route.Pattern)
				}
				continue
			}
			policy := policies[route.Pattern]
			if policy.Action != route.Action {
				t.Errorf("%s is bound on %s and mounted on %s", route.Pattern, route.Action,
					policy.Action)
				continue
			}
			var object authz.Object
			if policy.Object != nil {
				object = policy.Object(routeRequest(route.Pattern))
			}
			method, _, _ := strings.Cut(route.Pattern, " ")
			scope := iam.AssistScopeExec
			if method == http.MethodGet || method == http.MethodHead {
				scope = iam.AssistScopeRead
			}
			assisted := keyboard
			assisted.Via, assisted.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), scope
			want := authz.Decide(t.Context(), keyboard, policy.Action, object, authz.NoChart{}, now)
			got := authz.Decide(t.Context(), assisted, policy.Action, object, authz.NoChart{}, now)
			if class, _ := authz.AssistOf(policy.Action); class == authz.AssistNever {
				if got.Allowed || (got.Reason != authz.ReasonAssistRefused && !got.NotFound() &&
					got.Reason != authz.ReasonSeatOnly) {
					t.Errorf("%s (%s) for %s through a %s grant decided %+v; it is on the "+
						"human-only list", route.Pattern, policy.Action, name, scope, got)
				}
				if want.Allowed {
					neverRefused++
				}
				continue
			}
			if !sameDecision(got, want) {
				t.Errorf("%s (%s) for %s decided %+v through a %s grant and %+v at the "+
					"keyboard", route.Pattern, policy.Action, name, got, scope, want)
			}
			if got.Allowed {
				admitted++
			}
		}
		if admitted == 0 {
			t.Errorf("%s was admitted to no route through the assistant", name)
		}
	}
	if neverRefused == 0 {
		t.Error("no human-only verb was refused where it admits the person, so the walk " +
			"certifies nothing about the list")
	}
}

// sameDecision reports two decisions that say the same thing to a caller.
func sameDecision(a, b authz.Decision) bool {
	return a.Allowed == b.Allowed && a.Reason == b.Reason && a.Unknown() == b.Unknown() &&
		slices.Equal(a.Grants, b.Grants)
}
