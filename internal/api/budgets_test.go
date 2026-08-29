package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// POST /budgets/reset exists because the counter it clears is FLEET state:
// on the default topology it lives inside the running engine, so a CLI with
// only a config file has nothing it could safely open.

// post runs one POST and returns the status and decoded body.
func post(t *testing.T, a *api.App, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return res.StatusCode, body
}

// guarded is a bootstrap with one token and anonymous reads ON — the default
// posture, and the one that makes the write/read distinction load-bearing.
func guarded() *config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = true
	b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: "t0ken"}}
	return &b
}

func TestAResetClearsTheScopeItWasGiven(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	if _, err := fleet.Charge(t.Context(), coord.AgentScope("a1"), 100, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Charge(t.Context(), coord.AgentScope("a2"), 100, 0, 0); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, api.Options{Bootstrap: guarded(), Budgets: fleet})

	status, body := post(t, a, "/budgets/reset?scope="+coord.AgentScope("a1"), "t0ken")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	// THE ANSWER NAMES WHAT IT CLEARED. A count alone leaves an operator
	// unable to tell "reset the seat I meant" from "reset a scope that
	// was already empty", against an irreversible action on a ceiling.
	scopes, _ := body["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != coord.AgentScope("a1") {
		t.Fatalf("scopes = %v, want just the one named", scopes)
	}
	if got, _ := fleet.Used(t.Context(), coord.AgentScope("a1")); got != 0 {
		t.Errorf("the named scope still holds %d", got)
	}
	// SCOPED MEANS SCOPED. Clearing a peer's counter, or the org's, would
	// re-arm a company somebody had stopped on purpose.
	if got, _ := fleet.Used(t.Context(), coord.AgentScope("a2")); got != 100 {
		t.Errorf("a scoped reset cleared another seat (now %d)", got)
	}
	if got, _ := fleet.Used(t.Context(), coord.OrgScope); got != 200 {
		t.Errorf("a scoped reset moved the org counter to %d", got)
	}
}

func TestAResetWithNoScopeClearsEverything(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	if _, err := fleet.Charge(t.Context(), coord.AgentScope("a1"), 100, 0, 0); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, api.Options{Bootstrap: guarded(), Budgets: fleet})

	status, body := post(t, a, "/budgets/reset", "t0ken")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if got, _ := fleet.Used(t.Context(), coord.OrgScope); got != 0 {
		t.Errorf("the org counter survived a full reset (%d)", got)
	}
}

// A RESET IS A WRITE, whatever the read posture allows. allow_anonymous_read
// is on by default and opens the whole read surface; clearing a company's
// spend ceiling is not a read, and a route that let it through on that
// posture would be reachable by anything that could reach the dashboard.
func TestAResetIsRefusedWithoutAToken(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	if _, err := fleet.Charge(t.Context(), coord.AgentScope("a1"), 100, 0, 0); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, api.Options{Bootstrap: guarded(), Budgets: fleet})

	status, _ := post(t, a, "/budgets/reset", "")
	if status == http.StatusOK {
		t.Fatal("an unauthenticated caller cleared the company's spend counter")
	}
	if got, _ := fleet.Used(t.Context(), coord.OrgScope); got != 100 {
		t.Errorf("a refused reset still cleared the counter (now %d)", got)
	}
}

// A NODE WITH NO COORDINATION STORE says so, and says it as 503 rather than
// 404: the route exists on this build, and a 404 sends an operator looking
// for a version mismatch that is not there.
func TestAResetWithoutACounterReportsWhyRatherThan404(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Bootstrap: guarded()})
	status, body := post(t, a, "/budgets/reset", "t0ken")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %v", status, body)
	}
	if body["error"] != "no_coordination_store" {
		t.Errorf("error = %v, want the reason", body["error"])
	}
}
