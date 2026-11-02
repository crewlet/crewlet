package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// EVERY ANSWER THE API WRITES SAYS IT IS THE ENGINE'S ([httpjson.EngineHeader]).
//
// A client tells what the engine did from what something in front of it did
// by this header alone: a proxy can answer JSON with an `error` field, and
// read as the engine's its 403 asked a person for a grant and its 504 said
// the engine failed. So every answer carries it, whichever handler wrote it:
// every mounted route on both listeners, served and while the node starts, a
// caller's credential presented or not (so the guard's own 401 too), and the
// answers no route writes (the mux's 404 and 405, the canonical-path refusal,
// the partition's 404).
func TestEveryAnswerTheAPIWritesCarriesTheEnginesMark(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, false)
	b := closedPosture()
	b.API.Port = 8080
	b.API.Public = config.APIPublic{Port: 8443}
	boot := bootFor(t, b)
	handlers := []struct {
		name string
		h    http.Handler
	}{
		{"api.port", a},
		{"the public listener", a.Public()},
		{"api.port while starting", boot.Handler()},
		{"the public listener while starting", boot.Public()},
	}
	routes := a.Routes()
	if len(routes) < len(publishedRoutes)+len(adminRoutes) {
		t.Fatalf("the app mounted %d routes, fewer than the listener cases name", len(routes))
	}
	requests := make([][2]string, 0, len(routes)+4)
	for _, pattern := range routes {
		method, path := requestFor(pattern)
		requests = append(requests, [2]string{method, path})
	}
	requests = append(requests,
		[2]string{http.MethodGet, "/health"},
		[2]string{http.MethodGet, "/no/such/route"},
		[2]string{http.MethodDelete, "/health"},
		[2]string{http.MethodGet, "/webhooks/../config"},
	)
	for _, side := range handlers {
		for _, req := range requests {
			for _, credential := range []bool{true, false} {
				r := httptest.NewRequest(req[0], req[1], strings.NewReader("{}"))
				if credential {
					r.Header.Set("Authorization", "Bearer secret")
				}
				rec := httptest.NewRecorder()
				side.h.ServeHTTP(rec, r)
				if got := rec.Header().Get(httpjson.EngineHeader); got != httpjson.EngineHeaderValue {
					t.Errorf("%s %s on %s (credential %v) answered %d with %s %q, want %q",
						req[0], req[1], side.name, credential, rec.Code,
						httpjson.EngineHeader, got, httpjson.EngineHeaderValue)
				}
			}
		}
	}
	// FLOOR: the requests without a credential reached the guard's refusal,
	// so its answer is among those held above.
	r := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /config without a credential answered %d, want the guard's 401", rec.Code)
	}
}

// THE DASHBOARD READS THE ENGINE'S MARK BY THE ENGINE'S OWN NAME AND VALUE
// (dashboard/src/contract/engine.ts). A copy that drifted would read every
// answer the engine writes as something in front of it: every refusal a write
// whose outcome nobody knows.
func TestTheDashboardKnowsTheEnginesOwnAnswers(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, c := range []struct{ name, want string }{
		{"ENGINE_HEADER", httpjson.EngineHeader},
		{"ENGINE_HEADER_VALUE", httpjson.EngineHeaderValue},
	} {
		got, err := clientsource.Scalar(tree, c.name)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("the dashboard's %s is %q, the engine's %q", c.name, got, c.want)
		}
	}
}
