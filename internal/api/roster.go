package api

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
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
// CONFIGURATION ONLY. What a seat is doing — `activity`, and why a stopped one
// is stopped — is the projection's to say, over its turn, its runs, its pause,
// its budget and its placement (livestate's activity.go); a state written here
// as well is how a seat came to have two. The roster used to mark the seats
// THIS node held as idle and leave every other seat stateless, which the client
// drew as offline — so on a fleet, every seat a peer ran read as down.
//
// Human seats are left out. They have no turn, no phase and no spend, and the
// agent screen is about what is running; the org tree below carries them, which
// is where a reader looks for who to talk to.
func roster(company func() *config.Company) []map[string]any {
	organization := agentOrganization(company)
	if organization == nil {
		return nil
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
		out = append(out, map[string]any{
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
		})
	}
	return out
}

// agentOrganization is the active company's org, or nil when there is none to
// read seats from.
func agentOrganization(company func() *config.Company) *org.Organization {
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
	return organization
}

// placement reads which of the company's agent seats some node in the fleet
// holds, keyed by role — the input the seat-state vocabulary's
// `stopped`/`unplaced` is computed from.
//
// FROM THE SEAT LEASES, which every node reads alike, rather than from which
// seats THIS node runs: a seat a peer holds is placed. This node's own seats
// are added to what the listing returns, because a prefix listing is free to
// lag a claim (coord's own contract) and a seat this process is serving right
// now must never read as held by nobody on its own dashboard.
//
// An unreadable lease table is an error, never an empty answer: "no node holds
// any seat" would stop every seat on every screen over a store blip.
func placement(ctx context.Context, company func() *config.Company, leases coord.Backend,
	runtime NodeRuntime,
) (map[string]bool, error) {
	organization := agentOrganization(company)
	if organization == nil {
		return map[string]bool{}, nil
	}
	live, err := leases.ListLive(ctx, coord.ClassSeat)
	if err != nil {
		return nil, fmt.Errorf("list the seat leases: %w", err)
	}
	held := map[string]bool{}
	for _, lease := range live {
		if handle, ok := coord.SeatHandle(lease.Resource); ok {
			held[handle] = true
		}
	}
	for _, handle := range runtime.Snapshot(ctx).Seats {
		held[handle] = true
	}
	out := map[string]bool{}
	for role := range organization.AllRoles() {
		if _, ok := organization.AgentIDFor(role); ok {
			out[role.Name] = held[role.Handle()]
		}
	}
	return out, nil
}

// toolRows renders the engine's catalogue for the wire.
func toolRows(runtime NodeRuntime) []map[string]any {
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

// placementTick reads the placement for the live channel.
//
// A CONTEXT OF ITS OWN, like [App.streamHealth]'s and for the same reason: the
// read is made on the shared tick and on a socket's snapshot, neither of which
// is a request with a context to inherit — see [tickReadBudget], whose own doc
// names this caller. Bounded rather than Background alone, because the read
// reaches the coordination plane and a push tick must not outlive the interval
// that will fire the next one.
//
// A NAMED FUNCTION rather than a closure inside [New]: the two are identical
// to run, but a closure built inside a constructor reads to contextcheck as
// the constructor's own body — a background context created where the caller
// had one to pass. streamHealth is a method for the same reason.
func placementTick(company func() *config.Company, leases coord.Backend,
	runtime NodeRuntime,
) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tickReadBudget)
	defer cancel()
	return placement(ctx, company, leases, runtime)
}
