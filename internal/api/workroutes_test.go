package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/pages"
)

// workStub is a write surface with one route, recording whether a request
// reached it.
type workStub struct{ reached int }

func (s *workStub) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, func(*http.Request, authz.Policy) authz.Decision {
		return authz.Decision{Allowed: true, Reason: authz.ReasonGrant}
	})
	return router.Handle("POST /work/items",
		authz.Policy{Action: authz.ActionWorkCreate},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			s.reached++
			w.WriteHeader(http.StatusOK)
		}))
}

// THE WRITE SURFACE IS MOUNTED BEHIND THE CREDENTIAL GUARD.
//
// Its own suite holds every route it mounts against the exemption list; what
// only the app can show is the COMPOSITION — that the mount happens on the mux
// the guard wraps rather than beside it. A route mounted beside it would be
// decided by its own policy with nobody on the request, and a surface that
// files work would file it as nobody.
//
// The control is the same request with the fixture's credential, which
// reaches the handler: without it this case would pass on a route that was
// never mounted at all.
func TestTheWriteSurfaceIsMountedBehindTheGuard(t *testing.T) {
	t.Parallel()
	stub := &workStub{}
	a := newApp(t, api.Options{Work: stub})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/work/items", nil))
	if rec.Code != http.StatusUnauthorized || stub.reached != 0 {
		t.Fatalf("an uncredentialled write answered %d and reached the "+
			"surface %d times", rec.Code, stub.reached)
	}
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, "/work/items", nil)))
	if rec.Code != http.StatusOK || stub.reached != 1 {
		t.Fatalf("the control answered %d and reached the surface %d times — "+
			"the route is not mounted, so the refusal above proves nothing",
			rec.Code, stub.reached)
	}
}

// unreadWork and unreadPages satisfy the write surface's seams and are never
// called: this case mounts the surface and serves nothing through it.
type (
	unreadWork  struct{ builtin.WorkReader }
	unreadPages struct {
		builtin.PageReader
		builtin.PageWriter
		workapi.PageStore
	}
)

// Rename is ambiguous between the two embedded seams it satisfies, so it is
// named once here — which is also why it is never called.
func (unreadPages) Rename(context.Context, pages.Actor, string, string, bool) (
	pages.Written, error) {

	return pages.Written{}, nil
}

// THE REAL WRITE SURFACE MOUNTS BESIDE EVERY READ THE APP ALREADY SERVES.
//
// Go's mux PANICS at registration on two patterns that match the same request
// with neither more specific, and the write surface shares its prefixes with
// the read routes — `/work/items/{key}` beside `/work/{id}`, `PUT /pages/{id}`
// beside `GET /pages/{id}`. A collision would be a node that panics at boot
// with the API on, and no unit suite of either surface alone can see it,
// because each mounts on a mux of its own. So this builds the app with the
// real surface on it — and then asks it something only a MOUNTED route could
// answer: a purge naming no confirmation is refused `400` by the route's own
// handler, before it reads a row, so nothing here reaches the seams above.
// Unmounted it is a 404, and unguarded a 401 would have come first.
func TestTheRealWriteSurfaceMountsBesideTheReads(t *testing.T) {
	t.Parallel()
	surface, err := workapi.New(workapi.Options{
		Work: builtin.WorkDeps{
			Reader: unreadWork{},
			Writer: func(builtin.Actor) builtin.WorkWriter { return nil },
		},
		Pages: builtin.PageDeps{Reader: unreadPages{}, Writer: unreadPages{}},
		Tracker: func(builtin.Actor) workapi.TrackerWriter {
			return nil
		},
		PageStore: unreadPages{},
		Chart:     authz.NoChart{},
	})
	if err != nil || surface == nil {
		t.Fatalf("workapi.New: %v", err)
	}
	a := newApp(t, api.Options{Work: surface})
	for _, target := range []string{"/work/items/ENG-1/purge", "/pages/p-1/purge"} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with no credential answered %d, want the guard's "+
				"401", target, rec.Code)
		}
		rec = httptest.NewRecorder()
		a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, target, nil)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with no confirmation answered %d, want the "+
				"route's own 400 — the write surface is not mounted", target, rec.Code)
		}
	}
}
