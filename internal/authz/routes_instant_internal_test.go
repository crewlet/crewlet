package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A REQUEST IS DECIDED AT THE INSTANT ITS ROUTE WAS.
//
// The route asks how recently the caller proved who they are, and a handler
// that decides more once the body is read — [Admit], a configuration write's
// parts — asks again. Each used to ask at its own `time.Now()`, so a proof
// that lapsed while the body was read refused, as stale, the request the
// route had just admitted on it. A request stamped with its route's instant is
// decided at that instant by every check the guard makes of it, here a
// step-up verb whose proof was fresh then and has lapsed since. Mutation:
// decide [ContextGuard]'s requests at `time.Now()` again and this fails.
func TestEveryCheckOfARequestIsDecidedAtItsRoutesInstant(t *testing.T) {
	t.Parallel()
	now := time.Now()
	routed := now.Add(-time.Hour)
	admin := iam.Principal{
		ID: uuid.Must(uuid.NewV7()), Kind: iam.KindPerson, Login: "jane.doe",
		Seat: "jane", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: now.Add(-30 * time.Minute),
	}
	ctx := iam.WithPrincipal(context.Background(), admin)
	ctx = context.WithValue(ctx, decidedAtKey{}, routed)
	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/config", nil)
	d := ContextGuard(NoChart{})(r, Policy{Action: ActionConfigWrite})
	if !d.Allowed {
		t.Fatalf("a step-up verb asked of a request routed while its proof was "+
			"fresh was decided %+v, want the admission the route made", d)
	}
	if !DecidedAt(ctx).Equal(routed) {
		t.Errorf("DecidedAt = %v, want the route's %v", DecidedAt(ctx), routed)
	}
	unrouted := iam.WithPrincipal(context.Background(), admin)
	if at := DecidedAt(unrouted); at.Before(now) {
		t.Errorf("a context no route decided is decided at %v, before now (%v)", at, now)
	}
}

// A SURFACE BEHIND ANOTHER ROUTER IS THE SAME REQUEST.
//
// A route's wrapper stamps the request with the instant it is decided at, and
// one mounted behind another router's route keeps the instant the first one
// stamped, so the handler and every guard on the way decide the request at
// one instant. Mutation: stamp the request unconditionally and the inner
// route's instant is its own.
func TestARouteBehindAnotherKeepsTheFirstInstant(t *testing.T) {
	t.Parallel()
	var outer, inner, handler time.Time
	allow := Decision{Allowed: true, Reason: ReasonEverybody}
	final := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		handler = DecidedAt(r.Context())
	})
	innerRouter := NewRouter(http.NewServeMux(), func(r *http.Request, _ Policy) Decision {
		inner = DecidedAt(r.Context())
		return allow
	})
	outerRouter := NewRouter(http.NewServeMux(), func(r *http.Request, _ Policy) Decision {
		outer = DecidedAt(r.Context())
		// LONG ENOUGH that an instant taken after it is another one.
		time.Sleep(2 * time.Millisecond)
		return allow
	})
	p := Policy{Action: ActionConfigRead}
	wrapped := outerRouter.wrap(p, innerRouter.wrap(p, final))
	wrapped.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/config", nil))
	if outer.IsZero() || !inner.Equal(outer) || !handler.Equal(outer) {
		t.Errorf("the outer route decided at %v, the inner at %v and the "+
			"handler read %v: one request, three instants", outer, inner, handler)
	}
}
