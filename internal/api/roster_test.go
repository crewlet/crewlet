package api_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
)

// The three surfaces the dashboard renders from CONFIGURATION.
//
// None of them was on the wire. Snapshot merged the live overlay onto a static
// roster of nil — an empty list whatever the projection held — and omitted the
// org tree and the tool catalogue entirely, so the Agents, Mission, Org Room
// and Tools screens had nothing to draw.
//
// It survived because the only end-to-end proof of the dashboard drives a TURN
// and asserts the `agents` OVERLAY frames it produces. Those work. The client
// appends an overlay for a role it has not seen, so a company whose model was
// answering grew its roster one seat at a time as each took its first turn —
// and a company whose model was not answering showed nothing at all, for ever.

const rosterCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
units:
  - name: Engineering
    roles:
      - name: CTO
        handle: cto
        llm: zulu
`

func rosterApp(t *testing.T, runtime api.NodeRuntime) *api.App {
	t.Helper()
	c, err := config.ParseCompany([]byte(rosterCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return newApp(t, api.Options{
		Runtime: runtime,
		Sources: queries.Sources{Company: func() *config.Company { return c }},
	})
}

// rows re-decodes a snapshot slice through JSON, which is how a client sees it.
func rows(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// THE ROSTER IS THE COMPANY'S AGENT SEATS, present before anything happens.
func TestTheSnapshotCarriesTheCompanysSeats(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, nil)

	got := rows(t, a.Stream().Snapshot()["agents"])
	if len(got) != 2 {
		t.Fatalf("roster = %v, want the two AGENT seats", got)
	}
	byHandle := map[string]map[string]any{}
	for _, row := range got {
		handle, _ := row["handle"].(string)
		byHandle[handle] = row
	}
	for _, handle := range []string{"ceo", "cto"} {
		row, ok := byHandle[handle]
		if !ok {
			t.Fatalf("seat %q is missing from the roster: %v", handle, got)
		}
		// The client keys every merge on `role` and links on `id`. A row
		// without them is a card that never receives its live overlay.
		if row["role"] == "" || row["role"] == nil {
			t.Errorf("seat %q has no role: %v", handle, row)
		}
		if id, _ := row["id"].(string); id == "" {
			t.Errorf("seat %q has no agent id: %v", handle, row)
		}
	}
	// A seat nested in a unit is still a seat. The org tree is walked, so a
	// roster built from the top-level roles alone would lose every seat in
	// a company that had units — which is every real one.
	if _, ok := byHandle["cto"]; !ok {
		t.Error("a seat inside a unit never reached the roster")
	}
	// The human is not an agent: no turn, no phase, no spend.
	for _, row := range got {
		if row["role"] == "Founder" {
			t.Error("a human seat was put on the agent roster")
		}
	}
}

// A SEAT THIS NODE HOLDS READS AS IDLE, and one it does not says nothing.
//
// The client defaults a missing state to offline, so silence is the honest
// answer for a seat this process has never seen — while claiming offline for
// one it is actively serving would report a healthy company as entirely down.
func TestOnlyHeldSeatsCarryAState(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{state: api.RuntimeState{Seats: []string{"ceo"}}})

	for _, row := range rows(t, a.Stream().Snapshot()["agents"]) {
		handle, _ := row["handle"].(string)
		state, present := row["state"]
		switch handle {
		case "ceo":
			if state != "idle" {
				t.Errorf("a held seat reads %v, want idle", state)
			}
		default:
			if present {
				t.Errorf("seat %q is not held here yet claims state %v", handle, state)
			}
		}
	}
}

// THE ORG TREE IS THE CONFIG'S OWN SHAPE. static/dashboard/js/org.js walks
// `roles` and `units` recursively and reads the config's field names off them,
// so anything reshaped here is a second definition of the wire form — and the
// one the client parses would be the one nobody edited.
func TestTheSnapshotCarriesTheOrgTreeVerbatim(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, nil)

	org, _ := a.Stream().Snapshot()["org"].(map[string]any)
	if org == nil {
		t.Fatal("the snapshot carries no org tree")
	}
	roles, _ := org["roles"].([]any)
	units, _ := org["units"].([]any)
	if len(roles) != 2 {
		t.Errorf("org roles = %d, want both top-level seats including the human", len(roles))
	}
	if len(units) != 1 {
		t.Fatalf("org units = %d, want the one unit", len(units))
	}
	unit, _ := units[0].(map[string]any)
	if unit["name"] != "Engineering" {
		t.Errorf("unit = %v, want it named", unit)
	}
	if nested, _ := unit["roles"].([]any); len(nested) != 1 {
		t.Errorf("the unit's roles = %v; the tree is what the client walks", nested)
	}
}

// THE TOOL CATALOGUE COMES FROM THE ENGINE, and only from one.
func TestTheSnapshotCarriesTheToolCatalogue(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{tools: []api.ToolInfo{
		{Name: "lookup_colleague", Description: "who is who", Source: "builtin"},
		{Name: "tracker_search", Description: "find work", Source: "tracker"},
	}})

	got := rows(t, a.Stream().Snapshot()["tools"])
	if len(got) != 2 {
		t.Fatalf("tools = %v, want both", got)
	}
	// The screen GROUPS by source, so a row without one lands in a group
	// named after the empty string.
	for _, row := range got {
		if source, _ := row["source"].(string); source == "" {
			t.Errorf("tool %v has no source", row)
		}
	}
}

// A STANDALONE API SAYS NOTHING ABOUT TOOLS rather than claiming none.
//
// It has no engine to ask. An empty catalogue is a real answer for a node that
// serves none, and this process cannot make it.
func TestAnAPIWithNoEngineClaimsNoTools(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, nil)

	if got := rows(t, a.Stream().Snapshot()["tools"]); len(got) != 0 {
		t.Errorf("tools = %v on a node with no engine", got)
	}
	// The roster and the org still come through: both are read from the
	// company document, which a standalone API has.
	if got := rows(t, a.Stream().Snapshot()["agents"]); len(got) != 2 {
		t.Errorf("a standalone API lost the roster: %v", got)
	}
}

// AND A NODE WITH NO COMPANY AT ALL ANSWERS EMPTY RATHER THAN PANICKING —
// the state a process is in before its first revision is activated.
func TestANodeWithNoCompanyAnswersEmptySurfaces(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})

	snap := a.Stream().Snapshot()
	for _, key := range []string{"agents", "org", "tools"} {
		if _, present := snap[key]; !present {
			t.Errorf("snapshot is missing %q; the client reads all three", key)
		}
	}
	if got := rows(t, snap["agents"]); len(got) != 0 {
		t.Errorf("agents = %v with no company", got)
	}
}
