package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
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
	return rosterAppWith(t, runtime, nil)
}

// rosterAppWith is rosterApp over a lease table the case controls.
func rosterAppWith(t *testing.T, runtime api.NodeRuntime, leases coord.Backend) *api.App {
	t.Helper()
	c, err := config.ParseCompany([]byte(rosterCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return newApp(t, api.Options{
		Runtime: runtime,
		Sources: queries.Sources{Company: func() *config.Company { return c }, Coord: leases},
	})
}

// object re-decodes a snapshot value through JSON into an object, which is how
// a client sees it.
func object(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return out
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
	a := rosterApp(t, &fakeRuntime{})

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

// EVERY SEAT HELD ANYWHERE IN THE FLEET CARRIES A STATE — and one held nowhere
// says so.
//
// The roster used to mark only the seats THIS node held as idle and leave every
// other seat without a state, which the client drew as offline: on a fleet,
// every seat a peer ran read as down on this node's dashboard. Placement is the
// lease table's answer, which every node reads alike, and a seat no node holds
// is `stopped`/`unplaced` rather than silent.
func TestEverySeatHeldAnywhereCarriesAState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		local   []string
		peer    []string
		wantCTO map[string]any
	}{
		{
			name: "a peer holds the other seat", local: []string{"ceo"}, peer: []string{"cto"},
			wantCTO: map[string]any{"activity": "idle", "stopped_reason": nil},
		},
		{
			name: "no node holds the other seat", local: []string{"ceo"},
			wantCTO: map[string]any{"activity": "stopped", "stopped_reason": "unplaced"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leases := coordmemory.New()
			for _, handle := range tc.peer {
				lease, err := leases.TryAcquire(t.Context(), coord.SeatResource(handle),
					coord.AcquireOptions{Owner: "node-b:1", TTL: time.Minute})
				if err != nil || lease == nil {
					t.Fatalf("a peer could not claim %s: %v", handle, err)
				}
			}
			a := rosterAppWith(t, &fakeRuntime{state: api.RuntimeState{Seats: tc.local}}, leases)

			byHandle := map[string]map[string]any{}
			for _, row := range rows(t, a.Stream().Snapshot()["agents"]) {
				handle, _ := row["handle"].(string)
				byHandle[handle] = row
			}
			if got := byHandle["ceo"]; got["activity"] != "idle" || got["stopped_reason"] != nil {
				t.Errorf("the seat this node holds reads %v (%v), want idle",
					got["activity"], got["stopped_reason"])
			}
			cto := byHandle["cto"]
			for key, want := range tc.wantCTO {
				value, present := cto[key]
				if !present || value != want {
					t.Errorf("cto %s = %#v, want %#v", key, value, want)
				}
			}
			// THE ROSTER STATES NO STATE OF ITS OWN: the one word on the
			// row is the projection's.
			if _, present := cto["state"]; present {
				t.Errorf("the row still carries the retired `state` word: %v", cto)
			}
		})
	}
}

// AN UNREADABLE LEASE TABLE STOPS NOBODY. "No node holds any seat" is a claim
// about the lease table, and a read that failed has not made it.
func TestAnUnreadableLeaseTableClaimsNoSeatIsUnplaced(t *testing.T) {
	t.Parallel()
	a := rosterAppWith(t, &fakeRuntime{}, unreadableLeases{coordmemory.New()})
	for _, row := range rows(t, a.Stream().Snapshot()["agents"]) {
		if row["activity"] != "idle" {
			t.Errorf("seat %v reads %v over a lease table nobody could read, want idle",
				row["handle"], row["activity"])
		}
	}
}

// unreadableLeases is a lease table whose listing fails.
type unreadableLeases struct{ coord.Backend }

func (unreadableLeases) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, coord.ErrUnavailable
}

// THE ORG TREE IS ON THE SNAPSHOT, nested the way the company is.
//
// The client walks `roles` and `units` recursively, so a seat nested in a unit
// has to arrive nested, and a human seat has to arrive at all: /agents leaves
// humans out, and the org tree is the one surface that names them.
func TestTheSnapshotCarriesTheOrgTree(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{})

	org := object(t, a.Stream().Snapshot()["org"])
	if org["name"] != "Acme" {
		t.Errorf("org name = %v, want the company's own", org["name"])
	}
	roles, _ := org["roles"].([]any)
	units, _ := org["units"].([]any)
	if len(roles) != 2 {
		t.Errorf("org roles = %d, want both top-level seats including the human", len(roles))
	}
	humans := 0
	for _, raw := range roles {
		if seat, _ := raw.(map[string]any); seat["kind"] == "human" {
			humans++
		}
	}
	if humans != 1 {
		t.Errorf("org roles carry %d human seats, want the founder: %v", humans, roles)
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

// AND IT CARRIES WHAT EACH TOOL DOES, not only what it is called.
//
// The delivery fence, the operator MCP surface and the sandbox bridge have all
// read these hints since registration; the catalogue on the wire carried three
// strings, so the one screen an operator audits a fresh MCP server on could
// say what its tools are NAMED and nothing about which of them can write.
func TestTheCatalogueCarriesWhatEachToolDoes(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{tools: []api.ToolInfo{
		{
			Name: "post_message", Description: "say something", Source: "slack",
			Annotations: api.ToolAnnotations{
				ReadOnly: "no", Destructive: "no",
				Idempotent: "unknown", OpenWorld: "yes",
			},
			Delivers:    "slack",
			InputSchema: map[string]any{"type": "object"},
		},
		{
			Name: "lookup_colleague", Description: "who is who", Source: "builtin",
			Annotations: api.ToolAnnotations{
				ReadOnly: "yes", Destructive: "unknown",
				Idempotent: "unknown", OpenWorld: "unknown",
			},
		},
	}})

	got := rows(t, a.Stream().Snapshot()["tools"])
	if len(got) != 2 {
		t.Fatalf("tools = %v", got)
	}
	ann, _ := got[0]["annotations"].(map[string]any)
	if ann["read_only"] != "no" || ann["open_world"] != "yes" {
		t.Errorf("annotations = %v, want the hints the registry recorded", ann)
	}
	// EVERY HINT IS A WORD, including the unadvertised one. A bool cannot
	// hold "the server said nothing", and an absent hint arriving as
	// `false` reads as a positive denial — which would make a fresh
	// server's unannotated tools look like proven reads.
	if ann["idempotent"] != "unknown" {
		t.Errorf("an unadvertised hint came through as %#v, not a word", ann["idempotent"])
	}
	if got[0]["delivers"] != "slack" {
		t.Errorf("delivers = %#v, want where the call lands", got[0]["delivers"])
	}
	if _, ok := got[0]["input_schema"].(map[string]any); !ok {
		t.Errorf("no input_schema on %v", got[0])
	}
	// A tool that reaches nobody says so with an empty surface rather than
	// by leaving the field out, which a client cannot tell from a build
	// that does not send it.
	if delivers, present := got[1]["delivers"]; !present || delivers != "" {
		t.Errorf("delivers = %#v on a read-only builtin", delivers)
	}
	// AND A TOOL THAT TAKES NO ARGUMENTS SENDS NO SCHEMA, because a form
	// rendered from `{}` and one rendered from a schema with no properties
	// look identical and only the first means "not sent".
	if _, present := got[1]["input_schema"]; present {
		t.Errorf("a tool with no schema carried one: %v", got[1])
	}
}

// AN ENGINE SERVING NO TOOLS YET STILL SERVES THE ROSTER.
//
// The catalogue comes from the engine and the roster from the company
// document, so an engine whose catalogue is empty (its MCP servers not up, or
// no revision equipped yet) renders an empty tool screen and the whole roster,
// rather than letting one absence blank the other.
func TestAnEngineWithNoToolsStillServesTheRoster(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{})

	if got := rows(t, a.Stream().Snapshot()["tools"]); len(got) != 0 {
		t.Errorf("tools = %v from an engine serving none", got)
	}
	if got := rows(t, a.Stream().Snapshot()["agents"]); len(got) != 2 {
		t.Errorf("an engine with no tools lost the roster: %v", got)
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
	// An empty OBJECT, not null and not an object of empty strings: the
	// client reads `{}` as "nothing loaded" and indexes into it freely.
	if got := object(t, snap["org"]); len(got) != 0 {
		t.Errorf("org = %v with no company, want {}", got)
	}
}

// THE CONFIGURED SCHEDULES ARE ON THE SNAPSHOT.
//
// The schedules screen renders its table from this slice and fetches only the
// dispatch ledger itself. The slice was never sent, and the client reads a
// missing one as "not here yet" — which was permanently true, so the screen
// sat on five skeleton rows for ever, with nothing in the console or the logs
// because nothing had failed.
//
// The CONFIGURED half only: the ledger is a store read and the snapshot is
// built without one.
func TestTheSnapshotCarriesTheConfiguredSchedules(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{})

	snap := a.Stream().Snapshot()
	if _, present := snap["schedules"]; !present {
		t.Fatal("the snapshot carries no schedules slice, so the screen " +
			"cannot tell 'none configured' from 'not loaded yet'")
	}
	// This company declares none, and an empty ARRAY is the answer that
	// says so — null would leave the screen on its skeleton.
	if got := rows(t, snap["schedules"]); got == nil {
		t.Error("schedules = null; the client reads that as still loading")
	}
}

// AND THE PUSH KEEPS THE LEDGER THE SCREEN ALREADY FETCHED.
//
// applySchedules assigns each half only when present, so a re-send that
// carried an empty recent_runs would blank the history a reader was looking
// at every time an unrelated config field changed.
func TestTheSchedulesPushCarriesOnlyTheConfiguredHalf(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{})

	push := a.Stream().Schedules()
	if _, present := push["schedules"]; !present {
		t.Error("the push carries no configured rows")
	}
	if _, present := push["recent_runs"]; present {
		t.Error("the push carries recent_runs; a re-send would blank the " +
			"ledger the screen fetched for itself")
	}
}
