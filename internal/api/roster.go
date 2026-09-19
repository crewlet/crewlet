package api

import (
	"context"

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
//
// The second of them, the org tree, is an anonymous surface with its own
// explicit public shape, and lives in orgprojection.go.

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
func roster(ctx context.Context, company func() *config.Company, runtime NodeRuntime) []map[string]any {
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
	// Only this node's, because that is all a process can answer about
	// which seats it is RUNNING. On a fleet a peer's seat therefore reads
	// as offline here; the fleet view answers "who holds what" from the
	// lease table, which is the one place that knows.
	//
	// Snapshot does reach the coordination plane — for the posture, which
	// this caller does not use — so it takes a context and the read is
	// bounded. The claim that it made no coordination read was already
	// untrue when it was written.
	held := map[string]bool{}
	if runtime != nil {
		for _, handle := range runtime.Snapshot(ctx).Seats {
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
			// THE HANDLE IS THE CLIENT'S ONE IDENTIFIER FOR A SEAT. Every
			// screen that addresses one sends `row.id`, and both answers
			// behind it resolve from a handle: the diary is keyed by the
			// agent id, which is DERIVED from the handle, and the
			// episodes by the handle itself. Putting the derived uuid
			// here instead would give the seat page an identifier that
			// links to the right page and answers nothing on it.
			//
			// The agent id rides along under its own name for the callers
			// that genuinely need it — a budget scope is keyed by it.
			"id":       handle,
			"agent_id": id.String(),
			"role":     role.Name,
			"handle":   handle,
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

// toolRows renders the catalogue for the wire, or nil where this process has
// no engine to ask.
func toolRows(runtime NodeRuntime) []map[string]any {
	if runtime == nil {
		return nil
	}
	infos := runtime.Tools()
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		row := map[string]any{
			"name":        info.Name,
			"description": info.Description,
			"source":      info.Source,
			// THE HINTS AND WHERE IT LANDS. The catalogue carried three
			// strings, so the screen an operator audits a fresh MCP
			// server on could say what its tools are CALLED and nothing
			// about what they do — while the engine's own delivery fence
			// had been reading these hints all along.
			"annotations": map[string]any{
				"read_only":   info.Annotations.ReadOnly,
				"destructive": info.Annotations.Destructive,
				"idempotent":  info.Annotations.Idempotent,
				"open_world":  info.Annotations.OpenWorld,
			},
			"delivers": info.Delivers,
		}
		if info.Annotations.Title != "" {
			row["title"] = info.Annotations.Title
		}
		// ABSENT rather than an empty object when a tool takes no
		// arguments, which is a real shape: a form rendered from `{}` and
		// one rendered from a schema with no properties look identical,
		// and only the first is "this build did not send it".
		if len(info.InputSchema) > 0 {
			row["input_schema"] = info.InputSchema
		}
		out = append(out, row)
	}
	return out
}

// rosterTick reads the roster for a push tick.
//
// A CONTEXT OF ITS OWN, like [App.streamHealth]'s and for the same reason:
// the roster push is a timer, not a request, so there is nothing to inherit —
// see [tickReadBudget], whose own doc names this caller. Bounded rather than
// Background alone, because the read underneath reaches the coordination
// plane and a push tick must not outlive the interval that will fire the next
// one.
//
// A NAMED FUNCTION rather than a closure inside [New]: the two are identical
// to run, but a closure built inside a constructor reads to contextcheck as
// the constructor's own body — a background context created where the caller
// had one to pass. streamHealth is a method for the same reason.
func rosterTick(company func() *config.Company, runtime NodeRuntime) []map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), tickReadBudget)
	defer cancel()
	return roster(ctx, company, runtime)
}
