package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/authapi"
)

// A NODE THAT SERVES NO SIGN-IN SURFACE HAS NO SIGN-IN ROUTES, rather than
// routes mounted over a nil service.
//
// A node that started with no active company builds no sign-in surface, and
// [api.HumanSurfaces.Mount] is the one place its nil *authapi.Service meets
// the options' interface. Handed straight across, as `crewlet run` did, the
// interface is NON-NIL: [api.New] mounts every /auth route over a nil service
// and `GET /auth/config` — the first thing the dashboard reads — panics inside
// its handler, where the absent surface is documented as a 404.
//
// Mutation: make Mount assign the sign-in surface unconditionally, and the
// read below panics.
func TestANodeServingNoSignInHasNoSignInRoutes(t *testing.T) {
	t.Parallel()
	var opts api.Options
	api.HumanSurfaces{}.Mount(&opts)
	if opts.Auth != nil || opts.IAM != nil || opts.Work != nil {
		t.Fatalf("an absent surface reached the options as %#v / %#v / %#v, "+
			"which the app reads as present", opts.Auth, opts.IAM, opts.Work)
	}
	a := newApp(t, opts)

	status, err := serveRecovered(a, http.MethodGet, "/auth/config")
	if err != nil {
		t.Fatalf("GET /auth/config on a node serving no sign-in: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("GET /auth/config on a node serving no sign-in answered %d, "+
			"want 404: the surface is absent there, not failing", status)
	}
}

// AND A SURFACE THAT IS THERE IS MOUNTED — the control, without which the case
// above would pass on a Mount that dropped the sign-in surface everywhere.
func TestASignInSurfaceThatIsThereIsMounted(t *testing.T) {
	t.Parallel()
	var opts api.Options
	service := &authapi.Service{}
	api.HumanSurfaces{SignIn: service}.Mount(&opts)
	if opts.Auth != service {
		t.Errorf("Mount handed the options %#v, want the sign-in surface it holds",
			opts.Auth)
	}
}

// serveRecovered runs one request and reports a handler panic as an error, so
// a route mounted over a nil service fails the case that found it rather than
// the whole test binary.
func serveRecovered(h http.Handler, method, path string) (status int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the handler panicked: %v", r)
		}
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code, nil
}
