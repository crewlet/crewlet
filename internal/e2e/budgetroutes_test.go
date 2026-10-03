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
// through `/config`, a seat's through its org chart runtime half. They used to
// be written into the fixture's company document, which is the file a node
// seeds its first chart from at boot — a route no running company is ever
// changed through again — so every budget case here proved the seed and
// nothing about the two writes an operator actually makes, or about whether a
// running node picks up a ceiling moved under it. Both helpers write in the
// `{day: N}` form, the only one either surface accepts, and wait until the
// running company carries what they wrote.

// setCompanyCeilings writes the company's own `token_budget` through
// `PUT /config`, as the deployment's Tier A token, and applies the revision it
// activated the way a node's reconcile loop does.
//
// A WHOLE SETTINGS DOCUMENT, because this harness boots its node from the
// company file and stores no revision — exactly the state `crewlet config
// import` finds on a fresh deployment — and a merge patch is defined only over
// a revision that is there. The document is the running company's settings
// half with the ceilings in it, so nothing else about the company moves.
//
// APPLIED BY A TICK, because the harness runs no reconcile loop of its own: a
// write through `/config` is a revision and an activation, and the epoch moves
// only when a node's reconciler applies it.
func setCompanyCeilings(t *testing.T, n *node, ceilings map[period.Period]int) {
	t.Helper()
	company := n.engine.Company()
	if company == nil || company.Config == nil {
		t.Fatal("the node runs no company to set the ceilings of")
	}
	settings := config.SettingsOf(company.Config)
	settings.TokenBudget = config.TokenBudget{}
	for window, limit := range ceilings {
		v := limit
		switch window {
		case period.Day:
			settings.TokenBudget.Day = &v
		case period.Week:
			settings.TokenBudget.Week = &v
		case period.Month:
			settings.TokenBudget.Month = &v
		default:
			t.Fatalf("no such window as %q", window)
		}
	}
	status, body := bearer(t, n, http.MethodPut, "/config", settings,
		map[string]string{"X-Summary": "set the company's token ceilings"})
	if status != http.StatusCreated {
		t.Fatalf("PUT /config with token_budget %v answered %d: %s", ceilings, status, body)
	}
	backends := n.engine.Backends()
	reconciler, err := n.engine.NewReconciler(engine.ReconcilerOptions{
		Store: backends.Store, Fleet: backends.Fleet, Queue: backends.Queue,
		NodeID: n.id,
	})
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}
	waitFor(t, "the company's ceilings to apply", func() bool {
		if err := reconciler.Tick(t.Context()); err != nil {
			return false
		}
		company := n.engine.Company()
		if company == nil || company.Config == nil {
			return false
		}
		got := company.Config.TokenBudget.Ceilings()
		for window, want := range ceilings {
			if got[window] != want {
				return false
			}
		}
		return len(got) == len(ceilings)
	})
}

// setSeatCeilings writes one seat's `token_budget` into its org chart runtime
// half through `PATCH /chart/seats/{handle}`, and waits for this node to
// compose a company carrying it.
//
// A CONTENT WRITE STATES THE SEAT WHOLE — every field but the runtime half is
// post-state — so the seat is read first, its runtime half included, and
// written back with the one key changed. The half comes back MASKED and the
// write restores each mask from the row it patches, so a credential in it is
// neither read nor rewritten.
func setSeatCeilings(t *testing.T, n *node, handle string, ceilings map[period.Period]int) {
	t.Helper()
	status, raw := bearer(t, n, http.MethodGet,
		"/chart/seats/"+url.PathEscape(handle)+"?runtime=true", nil, nil)
	var read struct {
		Seat struct {
			Unit                 string         `json:"unit"`
			Name                 string         `json:"name"`
			Email                string         `json:"email"`
			Backstory            string         `json:"backstory"`
			Goal                 string         `json:"goal"`
			Responsibilities     []string       `json:"responsibilities"`
			BehavioralGuidelines []string       `json:"behavioral_guidelines"`
			Project              string         `json:"project"`
			Space                string         `json:"space"`
			Runtime              map[string]any `json:"runtime"`
		} `json:"seat"`
		Runtime bool `json:"runtime"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &read) != nil || !read.Runtime {
		t.Fatalf("GET /chart/seats/%s?runtime=true answered %d: %s", handle, status, raw)
	}
	seat := read.Seat
	runtime := seat.Runtime
	if runtime == nil {
		runtime = map[string]any{}
	}
	runtime["token_budget"] = ceilings
	status, raw = bearer(t, n, http.MethodPatch, "/chart/seats/"+url.PathEscape(handle),
		map[string]any{
			"unit": seat.Unit, "name": seat.Name, "email": seat.Email,
			"backstory": seat.Backstory, "goal": seat.Goal,
			"responsibilities":      seat.Responsibilities,
			"behavioral_guidelines": seat.BehavioralGuidelines,
			"project":               seat.Project, "space": seat.Space,
			"runtime": runtime,
		}, nil)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("PATCH /chart/seats/%s with token_budget %v answered %d: %s",
			handle, ceilings, status, raw)
	}
	waitFor(t, "seat "+handle+"'s ceilings to compose", func() bool {
		company := n.engine.Company()
		if company == nil || company.Org == nil {
			return false
		}
		role := company.Org.AgentSeatByHandle(handle)
		if role == nil {
			return false
		}
		for window, want := range ceilings {
			if role.TokenBudget[window] != want {
				return false
			}
		}
		return len(role.TokenBudget) == len(ceilings)
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
