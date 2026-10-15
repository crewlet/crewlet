package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/period"
)

// A COMPANY'S TOKEN CEILINGS ARE SET WHERE A PERSON SETS THEM: the company's
// through `PUT /config`, a seat's through `PUT /config/roles/{handle}`. They
// used to be written into the fixture's company document, which is the file a
// node seeds its first revision from at boot — a route no running company is
// ever changed through again — so every budget case here proved the seed and
// nothing about the two writes an operator actually makes, or about whether a
// running node picks up a ceiling moved under it. Both helpers write in the
// `{day: N}` form and wait until the running company carries what they wrote.

// setCompanyCeilings writes the company's own `token_budget` through
// `PUT /config`, as the deployment's Tier A token, and applies the revision it
// activated the way a node's reconcile loop does.
//
// A WHOLE DOCUMENT, because this harness boots its node from the company file
// and stores no revision — exactly the state `crewlet config import` finds on
// a fresh deployment — and a merge patch is defined only over a revision that
// is there. The document is the running company with the ceilings in it, so
// nothing else about the company moves.
func setCompanyCeilings(t *testing.T, n *node, ceilings map[period.Period]int) {
	t.Helper()
	company := n.engine.Company()
	if company == nil || company.Config == nil {
		t.Fatal("the node runs no company to set the ceilings of")
	}
	document := *company.Config
	document.TokenBudget = budgetOf(t, ceilings)
	status, body := bearer(t, n, http.MethodPut, "/config", document,
		map[string]string{"X-Summary": "set the company's token ceilings"})
	if status != http.StatusCreated {
		t.Fatalf("PUT /config with token_budget %v answered %d: %s", ceilings, status, body)
	}
	applyByTick(t, n, "the company's ceilings", func(c *engine.Company) bool {
		return sameCeilings(c.Config.TokenBudget.Ceilings(), ceilings)
	})
}

// setSeatCeilings writes one seat's `token_budget` through
// `PUT /config/roles/{handle}`, and applies the revision it activated.
//
// THE SEAT IS READ FIRST AND WRITTEN BACK WHOLE with the one key changed,
// because the entity route replaces the seat. A credential comes back masked,
// and the write restores each mask from the revision it splices into, so a
// credential is neither read nor rewritten.
func setSeatCeilings(t *testing.T, n *node, handle string, ceilings map[period.Period]int) {
	t.Helper()
	path := "/config/roles/" + url.PathEscape(handle)
	status, raw := bearer(t, n, http.MethodGet, path, nil, nil)
	var seat map[string]any
	if status != http.StatusOK || json.Unmarshal(raw, &seat) != nil {
		t.Fatalf("GET %s answered %d: %s", path, status, raw)
	}
	seat["token_budget"] = ceilings
	status, raw = bearer(t, n, http.MethodPut, path, seat,
		map[string]string{"X-Summary": "set " + handle + "'s token ceilings"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("PUT %s with token_budget %v answered %d: %s", path, ceilings, status, raw)
	}
	applyByTick(t, n, "seat "+handle+"'s ceilings", func(c *engine.Company) bool {
		role := c.Org.AgentSeatByHandle(handle)
		return role != nil && sameCeilings(role.TokenBudget, ceilings)
	})
}

// budgetOf is ceilings as the document's block.
func budgetOf(t *testing.T, ceilings map[period.Period]int) config.TokenBudget {
	t.Helper()
	var budget config.TokenBudget
	for window, limit := range ceilings {
		v := limit
		switch window {
		case period.Day:
			budget.Day = &v
		case period.Week:
			budget.Week = &v
		case period.Month:
			budget.Month = &v
		default:
			t.Fatalf("no such window as %q", window)
		}
	}
	return budget
}

// sameCeilings reports got holding exactly want.
func sameCeilings(got, want map[period.Period]int) bool {
	for window, limit := range want {
		if got[window] != limit {
			return false
		}
	}
	return len(got) == len(want)
}

// applyByTick applies the activation a write through `/config` made, the way a
// node's reconcile loop does, and waits for the running company to satisfy
// done.
//
// BY A TICK, because the harness runs no reconcile loop of its own: a write
// through `/config` is a revision and an activation, and the epoch moves only
// when a node's reconciler applies it.
func applyByTick(t *testing.T, n *node, what string, done func(*engine.Company) bool) {
	t.Helper()
	backends := n.engine.Backends()
	reconciler, err := n.engine.NewReconciler(engine.ReconcilerOptions{
		Store: backends.Store, Fleet: backends.Fleet, Queue: backends.Queue,
		NodeID: n.id,
	})
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}
	waitFor(t, what+" to apply", func() bool {
		if err := reconciler.Tick(t.Context()); err != nil {
			return false
		}
		company := n.engine.Company()
		return company != nil && company.Config != nil && company.Org != nil &&
			done(company)
	})
}

// bearer makes one request to one node as the deployment's Tier A token — a
// script, as `crewlet` and a CI job write — and answers its status and raw
// body.
func bearer(t *testing.T, n *node, method, path string, body any,
	header map[string]string) (int, []byte) {

	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the body for %s %s: %v", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, n.server.URL+path, payload)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := n.server.Client().Do(present(req))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", method, path, err)
	}
	return res.StatusCode, raw
}
