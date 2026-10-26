package authz_test

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
)

// keeper is a table that keeps every policy it is told.
type keeper struct {
	*http.ServeMux
	policies map[string]authz.Policy
}

func (k *keeper) MountedWith(pattern string, p authz.Policy) { k.policies[pattern] = p }

// A ROUTER TELLS A TABLE THAT KEEPS POLICIES EVERY VERB IT MOUNTS, directly
// or through a surface's own sub-mux, so a walk over the guarded routes reads
// each one's verb from the table rather than from a second list. A table that
// keeps none is handed nothing and still mounts the route. Mutation: drop the
// report in Router.Handle, or let Within hide the outer table, and this goes
// red.
func TestARouterReportsEveryPolicyToATableThatKeepsThem(t *testing.T) {
	t.Parallel()
	outer := &keeper{ServeMux: http.NewServeMux(), policies: map[string]authz.Policy{}}
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	direct := authz.NewRouter(outer, nil)
	if err := direct.Handle("GET /worklet", authz.Policy{Action: authz.ActionWorkList}, ok); err != nil {
		t.Fatal(err)
	}
	inner := http.NewServeMux()
	sub := authz.NewRouter(authz.Within(inner, outer), nil)
	if err := sub.Handle("GET /config", authz.Policy{Action: authz.ActionConfigRead}, ok); err != nil {
		t.Fatal(err)
	}
	for pattern, want := range map[string]authz.Action{
		"GET /worklet": authz.ActionWorkList, "GET /config": authz.ActionConfigRead,
	} {
		if got := outer.policies[pattern].Action; got != want {
			t.Errorf("the table keeps %q for %s, want %q", got, pattern, want)
		}
	}
	if _, pattern := inner.Handler(mustRequest(t, "/config")); pattern != "GET /config" {
		t.Errorf("the sub-mux serves %q for /config, want the route mounted on it", pattern)
	}
	if got := authz.Within(inner, http.NewServeMux()); got != authz.Mux(inner) {
		t.Errorf("Within over a table that keeps no policies is %T, want the inner mux", got)
	}
}

func mustRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
