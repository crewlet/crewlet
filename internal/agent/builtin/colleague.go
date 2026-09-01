// Package builtin holds the tools the engine itself ships.
//
// Every one of them speaks FOR a seat — asking a colleague, loading a skill
// this agent synthesized, recalling its own episodes, marking its own
// onboarding — which is why they take the turn rather than reading a handle
// out of their arguments. The seat comes from the surface the runner built
// (tools.SeatCallable); a model that spelled a different handle cannot become
// somebody else by asking.
//
// One registration per epoch, not per seat: the catalogue a planner is shown
// comes from the registry, so a per-seat registration would put N copies of
// every builtin in it — and the fact that varies per call is the CALLER, not
// the tool.
package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("agent.builtin")

// LookupColleagueTool is the tool's wire name.
const LookupColleagueTool = "lookup_colleague"

// lookupColleague resolves a free-text query to one colleague, or to the list
// it might be.
//
// NEVER A GUESS. An agent that silently addressed the wrong colleague is worse
// than one that asked which — the wrong person gets pulled into work that is
// not theirs, and the right one never hears about it. So an ambiguous query
// returns the candidates and says they are candidates.
type lookupColleague struct{}

var _ tools.SeatCallable = (*lookupColleague)(nil)

func (t *lookupColleague) Name() string { return LookupColleagueTool }

func (t *lookupColleague) Description() string {
	return "Look up a colleague — AI agent or human teammate — by any " +
		"identifier: handle, role name, Slack user ID, Jira/Confluence " +
		"account ID, GitHub or GitLab username. Returns the canonical " +
		"handle, the seat kind (agent | human), and every known " +
		"cross-platform identity; human results add what they own and how " +
		"to reach them (a mention and an asynchronous reply — never " +
		"a2a_ask, which addresses agents only). Matching is " +
		"case-insensitive and falls back to partial and fuzzy matching, so " +
		"'ceo' finds the role 'Agent CEO'; when more than one colleague " +
		"matches, the candidate list is returned instead of a guess. Use " +
		"this before a2a_ask or before @mentioning anyone."
}

func (t *lookupColleague) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "Any colleague identifier: handle, role name " +
					"(or part of one), or an id on any connected platform",
			},
		},
		"required": []any{"query"},
	}
}

// Call without a turn cannot resolve anyone: the corpus is the turn's pinned
// org. Reported as a failed result rather than an error, because the model
// asked for something reasonable in a context that cannot serve it.
func (t *lookupColleague) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *lookupColleague) CallForTurn(_ context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return failed("lookup_colleague needs a `query`: a handle, a role name, or an id on any connected platform."), nil
	}
	if turn == nil || turn.Org == nil {
		return failed("No organization is in scope, so there is nobody to look up."), nil
	}

	seats := Corpus(turn.Org)
	found := colleague.Resolve(query, seats)
	safe := clip(query)

	switch {
	case len(found) == 0:
		log.Debug("lookup_colleague_no_match", "query", safe, "corpus", len(seats))
		return failed(fmt.Sprintf(
			"No colleague matches %q. Known handles: %s.",
			safe, strings.Join(allHandles(seats), ", "))), nil

	case len(found) == 1:
		return tools.Result{Output: describe(found[0].Seat)}, nil
	}

	// AMBIGUOUS. The list, and the word "ambiguous", so the model asks
	// again with a handle rather than picking the first row.
	var b strings.Builder
	fmt.Fprintf(&b, "%q is ambiguous — %d colleagues match. Call lookup_colleague "+
		"again with one of these handles:\n", safe, len(found))
	// EVERY candidate. This message's whole job is to let the model retry
	// with an exact handle, and a handle it cannot see is one it cannot
	// name — a cap here answers "who might you mean" with a list that may
	// not contain the answer. A company's seats are its org chart, so the
	// list is bounded by the config a founder wrote.
	for _, c := range found {
		fmt.Fprintf(&b, "  - %s (%s, %s)", c.Seat.Handle, c.Seat.Name, c.Seat.Kind)
		if label := c.Method.Label(); label != "" {
			fmt.Fprintf(&b, " — %s", label)
		}
		b.WriteString("\n")
	}
	return failed(b.String()), nil
}

// Corpus is every addressable seat in an org: agents AND humans.
//
// Both, because the question a lookup answers is "who do I talk to", and a
// human seat is addressable — it just cannot be reached the same way. Leaving
// humans out would make the tool silently unable to find the people an agent
// most often needs.
func Corpus(o *org.Organization) []colleague.Seat {
	if o == nil {
		return nil
	}
	var out []colleague.Seat
	for role := range o.AllRoles() {
		seat := colleague.Seat{
			Handle: role.Handle(), Name: role.Name, Kind: string(role.Kind),
			External: map[string]string{},
		}
		if seat.Kind == "" {
			seat.Kind = string(org.KindAgent)
		}
		for _, id := range role.Contact.ResolvedIdentities(nil) {
			// Keyed by transport, so an exact-id query matches whichever
			// platform the id was copied from without the tool having to
			// be told which.
			seat.External[string(id.Transport)] = id.ExternalID
		}
		out = append(out, seat)
	}
	return out
}

// describe renders one resolved colleague.
//
// A human's entry says how to reach them and says explicitly that a2a_ask is
// not the way: an agent that tries it gets a refusal at best, and at worst
// waits for an answer from a channel no person is watching.
func describe(s colleague.Seat) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n  handle: %s\n  kind: %s\n", s.Name, s.Kind, s.Handle, s.Kind)
	if len(s.External) > 0 {
		for _, transport := range sortedKeys(s.External) {
			fmt.Fprintf(&b, "  %s: %s\n", transport, s.External[transport])
		}
	}
	if s.Kind == string(org.KindHuman) {
		b.WriteString("  reach them: mention them on a shared surface and " +
			"continue without waiting — a person answers asynchronously. " +
			"a2a_ask addresses AGENTS only and will not reach them.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// allHandles is every seat's handle, sorted.
//
// EVERY one, with no cap. This list is what a model that missed reads to find
// the handle it should have used; "...and 7 more" tells it the answer exists
// and withholds it.
func allHandles(seats []colleague.Seat) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Handle)
	}
	sortStrings(out)
	return out
}
