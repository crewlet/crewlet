package artifactsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
)

// A ROUTED REQUEST IS DECIDED AT ITS ROUTE'S INSTANT, AND A CALL NO ROUTE
// DECIDED AT THE GATE'S CLOCK.
//
// The purge route admits on a recent proof and the handler decides the purge
// again once it has read the artifact's head; at the gate's own clock a proof
// that lapsed in between refused, as stale, the purge the route had just
// admitted. A tool reaches the same gate with no route in front of it, and
// there the gate's clock — the one a test sets — is the instant. Mutation:
// decide at g.now() whatever the context carries and the routed half fails;
// decide at authz.DecidedAt and the unrouted half does.
func TestAGateDecidesARoutedRequestAtItsRoutesInstant(t *testing.T) {
	t.Parallel()
	clock := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g := Gate{Now: func() time.Time { return clock }}
	if at := g.decidedAt(context.Background()); !at.Equal(clock) {
		t.Errorf("a call no route decided is decided at %v, want the gate's %v", at, clock)
	}

	mux := http.NewServeMux()
	router := authz.NewRouter(mux, func(*http.Request, authz.Policy) authz.Decision {
		return authz.Decision{Allowed: true, Reason: authz.ReasonGrant}
	})
	var routed, decided time.Time
	var stamped bool
	if err := router.Handle("POST /artifacts/{id}/purge",
		authz.Policy{Action: authz.ActionArtifactPurge},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			routed, stamped = authz.RouteInstant(r.Context())
			decided = g.decidedAt(r.Context())
		})); err != nil {
		t.Fatal(err)
	}
	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/artifacts/a/purge", nil))
	if !stamped {
		t.Fatal("the route stamped no instant on the request it admitted")
	}
	if !decided.Equal(routed) {
		t.Errorf("a routed purge is decided at %v, want the route's %v", decided, routed)
	}
}
