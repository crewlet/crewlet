package builtin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A TOOL A ROUTED REQUEST CALLS IS DECIDED AT THE INSTANT ITS ROUTE WAS.
//
// The work surface's tool-backed routes are [authz.Router] routes, and each
// then dispatches into the tool gate on the request's own context — the last
// check of a routed request, so it is judged at the instant the route stamped
// on it ([authz.DecidedAt]) rather than at its own clock. A route that admits
// a person whose proof lapses one nanosecond after it decided proves the two
// clocks apart: the gate, asked a step-up verb on the routed context once the
// proof has lapsed, still admits, and the SAME principal on a context no route
// decided is refused for a stale proof.
//
// Mutation: decide [builtin.Decide] at `time.Now()` again and the routed call
// is refused step_up.
func TestAToolGateDecidesARoutedRequestAtItsRoutesInstant(t *testing.T) {
	t.Parallel()
	gate := builtin.Decide(authz.NoChart{})
	admin := func(reauth time.Time) iam.Principal {
		return iam.Principal{
			ID: uuid.Must(uuid.NewV7()), Kind: iam.KindPerson, Login: "jane.doe",
			Seat: "jane", Stage: iam.StageActive,
			Grants: []iam.Grant{iam.GrantAdmin}, ReauthAt: reauth,
		}
	}
	var routed time.Time
	var person iam.Principal
	var routedErr error
	mux := http.NewServeMux()
	router := authz.NewRouter(mux, func(r *http.Request, _ authz.Policy) authz.Decision {
		routed = authz.DecidedAt(r.Context())
		return authz.Decision{Allowed: true, Reason: authz.ReasonEverybody}
	})
	err := router.Handle("POST /worklet/items", authz.Policy{Action: authz.ActionConfigWrite},
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			person = admin(routed.Add(time.Nanosecond))
			// UNTIL THE PROOF HAS LAPSED on the wall clock, which is
			// the event the case is about rather than a bound on it.
			for !time.Now().After(person.ReauthAt) {
				time.Sleep(time.Microsecond)
			}
			routedErr = gate(iam.WithPrincipal(r.Context(), person),
				authz.ActionConfigWrite, authz.Object{})
		}))
	if err != nil {
		t.Fatal(err)
	}
	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/worklet/items", nil))
	if routed.IsZero() {
		t.Fatal("the route never decided the request, so nothing was asked")
	}
	if routedErr != nil {
		t.Errorf("a step-up verb asked of the tool gate on a request routed while "+
			"its proof was fresh = %v, want the admission its route made", routedErr)
	}
	unroutedErr := gate(iam.WithPrincipal(context.Background(), person),
		authz.ActionConfigWrite, authz.Object{})
	var refused *builtin.DecisionRefused
	if !errors.As(unroutedErr, &refused) || refused.Decision.Reason != authz.ReasonStepUp {
		t.Errorf("the same lapsed proof on a context no route decided = %v, want "+
			"a refusal for a stale proof — without it this case cannot tell the "+
			"route's instant from the gate's own", unroutedErr)
	}
}
