package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/panics"
)

// A PANIC ON A ROUTE SERVED WITHOUT A CREDENTIAL IS READ AS BEFORE AUTHENTICATION,
// on every other route as an ordinary one, and a handler that aborts on purpose is
// no panic at all. Each is raised again, so net/http answers as it always has.
// Not parallel: the observer hears every panic in the process.
func TestTheAPIReportsAPanicByWhetherItsRouteIsGuarded(t *testing.T) {
	var mu sync.Mutex
	var heard []*panics.Panic
	stop := panics.Observe(func(p *panics.Panic) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, p)
	})
	defer stop()
	serve := func(path string, value any) (raised any) {
		h := servedOn(nil, false, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(value) }))
		defer func() { raised = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		return nil
	}
	for path, before := range map[string]bool{"/webhooks/github": true, "/auth/login": true, "/agents": false} {
		mu.Lock()
		heard = nil
		mu.Unlock()
		if raised := serve(path, "a handler broke"); raised != "a handler broke" {
			t.Errorf("%s: raised %v, want the handler's own value", path, raised)
		}
		mu.Lock()
		if len(heard) != 1 || heard[0].Scope != panics.ScopeAPI || heard[0].Unauthenticated != before {
			t.Errorf("%s: heard %+v, want one api panic with before-authentication %v", path, heard, before)
		}
		mu.Unlock()
	}
	mu.Lock()
	heard = nil
	mu.Unlock()
	if raised, _ := serve("/agents", http.ErrAbortHandler).(error); !errors.Is(raised, http.ErrAbortHandler) {
		t.Errorf("an abort was raised as %v", raised)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 0 {
		t.Errorf("an abort was heard as a panic: %+v", heard)
	}
}
