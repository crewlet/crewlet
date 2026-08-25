package api

import (
	"encoding/json"

	"github.com/crewlet/crewlet/internal/config"
)

// The three surfaces the dashboard renders from CONFIGURATION rather than from
// anything that has happened yet: who the seats are, how they are organised,
// and what tools they can reach.
//
// They were the gap that made a freshly started company look empty. The
// projection only ever holds what has HAPPENED — a turn's phase, a live call,
// a spend — so it can say what a seat is doing and not that the seat exists.
// Snapshot asked it for the agent list anyway, merging the live overlay onto a
// static roster of nil, which is an empty list every time. The roster arrived
// only as a side effect of a turn running, because an `agents` overlay for an
// unknown role is appended by the client — so a company whose model was not
// answering had no agents on screen at all, permanently, and one that was
// working grew its roster a seat at a time as each happened to take a turn.

// roster is the static seat rows the live overlay is merged onto.
//
// EVERY AGENT SEAT IN THE COMPANY, not the ones this node runs: the dashboard
// is a view of the company, and a fleet's seats are spread across nodes. A
// roster limited to local seats would show a different company depending on
// which node the browser reached.
//
// Human seats are left out. They have no turn, no phase and no spend, and the
// agent screen is about what is running; the org tree below carries them, which
// is where a reader looks for who to talk to.
func roster(company func() *config.Company, runtime NodeRuntime) []map[string]any {
	if company == nil {
		return nil
	}
	c := company()
	if c == nil {
		return nil
	}
	organization, err := c.Organization()
	if err != nil {
		// A company that will not resolve into an org is one the engine
		// refused to apply, so this is a config no node is running. An
		// empty roster is the honest answer and the screen says so.
		return nil
	}
	// WHICH SEATS THIS NODE IS ACTUALLY SERVING. A held seat is attached,
	// its mailbox is open and it is waiting for work — which is "idle",
	// not "offline", and the difference is the whole first impression a
	// booted company gives.
	//
	// Only this node's, because that is all a process can answer without
	// a coordination read, and Snapshot is documented to make none. On a
	// fleet a peer's seat therefore reads as offline here; the fleet view
	// answers "who holds what" from the lease table, which is the one
	// place that knows.
	held := map[string]bool{}
	if runtime != nil {
		for _, handle := range runtime.Snapshot().Seats {
			held[handle] = true
		}
	}

	var out []map[string]any
	for role := range organization.AllRoles() {
		// ONE CHECK, and it is the id lookup. AgentIDFor refuses a
		// non-agent by contract, so this both filters the human seats and
		// produces the id the row needs. A separate IsAgent guard in
		// front of it reads as defence and is unreachable — a mutation
		// deleting it passed the whole suite, which is the definition of
		// a claim rather than a check.
		id, ok := organization.AgentIDFor(role)
		if !ok {
			continue
		}
		handle := role.Handle()
		row := map[string]any{
			// id is the DERIVED agent id, the same one every node
			// computes, so a row links to the same seat page everywhere.
			"id":     id.String(),
			"role":   role.Name,
			"handle": handle,
		}
		// SET ONLY WHERE IT IS KNOWN. The client already reads a missing
		// state as offline (state.js, effectiveAgentState), so a seat
		// this node does not hold needs no claim from here — and writing
		// one would be this process asserting something about a seat it
		// has never seen.
		if held[handle] {
			row["state"] = "idle"
		}
		out = append(out, row)
	}
	return out
}

// orgTree is the company's role and unit tree, verbatim.
//
// VERBATIM, and that is the contract the client is written to: static/dashboard
// js/org.js walks `roles` and `units` recursively and reads the config's own
// field names off them — token_budget, mcp_env, contact, manages. Reshaping it
// here would mean two definitions of the org's wire form, and the one the
// client actually parses would be the one nobody edited.
//
// Marshalled through JSON rather than hand-built for the same reason: the tags
// on config.Company ARE the wire names, so a field added to a role reaches the
// dashboard without a second place to remember.
func orgTree(company func() *config.Company) map[string]any {
	if company == nil {
		return map[string]any{}
	}
	c := company()
	if c == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(struct {
		Roles any `json:"roles"`
		Units any `json:"units"`
	}{Roles: c.Roles, Units: c.Units})
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// toolRows renders the catalogue for the wire, or nil where this process has
// no engine to ask.
func toolRows(runtime NodeRuntime) []map[string]any {
	if runtime == nil {
		return nil
	}
	infos := runtime.Tools()
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, map[string]any{
			"name":        info.Name,
			"description": info.Description,
			"source":      info.Source,
		})
	}
	return out
}
