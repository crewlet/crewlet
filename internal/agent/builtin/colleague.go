// Package builtin holds the tools the engine itself ships.
//
// Every one of them speaks FOR a seat — asking a colleague, loading a skill
// this agent synthesized, recalling its own episodes, marking its own
// onboarding — which is why they take the turn rather than reading a handle
// out of their arguments. The seat comes from the surface the runner built
// (tools.SeatCallable); a model that spelled a different handle cannot become
// somebody else by asking.
//
// One registration per epoch, not per seat: the catalogue an agent is shown
// comes from the registry, so a per-seat registration would put N copies of
// every builtin in it — and the fact that varies per call is the CALLER, not
// the tool.
//
// # How a write is made once
//
// Every write these tools make is sent again sometimes — a turn redelivered
// after a crash, a call repeated after an `unknown`, a person pressing retry —
// and the state log collapses a repetition only when it arrives under the SAME
// operation id. So every id a write derives — the operation's, and a created
// task's, comment's, view's, checklist item's or page's own — comes from ONE
// identity per call, chosen by one rule:
//
//   - IN A TURN, the turn: its work key (or its run, for a turn with no
//     ledgerable trigger), the instant that unit of work began, a digest of
//     the call's own arguments and how many different calls to the same tool
//     the run made first ([opIDFor], [Actor.OperationSince],
//     turnctx.CallLog). A re-run makes the same calls and derives the same
//     ids; a different call, or the same one after a different one, is a new
//     operation.
//   - OUTSIDE A TURN, the call's OPERATION ([Actor.Operation]) — the one key
//     there, and every write the call makes is a step of it. The caller names
//     it, on the transport it came by: an operator's assistant over MCP
//     brings back the `op_id` its call was answered with (or is minted one
//     for a new call, and answered it); the dashboard's act transport names
//     it from the request id its retry repeats ([RequestOperation]), scoped
//     to the credential that sent it. Either way the operation carries the
//     instant it was minted and NAMES THE CALL — the tool and a digest of its
//     arguments — so it is accepted only with that call
//     ([WorkDeps.bindOperation], and [PageDeps.foreignOperation] for the
//     page tools): the same call made again is the same operation, and
//     anything else under it is refused before a write.
//   - WITH NEITHER — a tool that takes no `op_id`, called over MCP — each
//     write is fresh, since nothing will repeat it. The page tools are such
//     tools, and so are write_project and write_work_catalogue, whose writes
//     restate what they declare, so a repeat under a new operation already
//     changes nothing a first attempt landed ([bindRequest]).
//
// There is deliberately no second key: a request key beside the operation, a
// seed beside the turn, would be a second answer to "is this the same write",
// free to disagree with the first. And a caller is told only ITS OWN way to
// repeat: an answer to a request carries no `op_id` and says "send the same
// request again" ([withOperation], [sameCall]), because an `op_id` beside a
// request is refused.
//
// # And what it answers when nobody knows
//
// A write whose outcome is `unknown` is answered as a FAILED result naming the
// operation to make again ([unknownWrite], [unknownOutcome]) — never a receipt
// carrying `outcome: unknown` beside a key or a version, which a model reads as
// the write done and a person's screen would draw.
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
	"github.com/crewlet/crewlet/internal/tracker"
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
		return refused(tools.RefusalUnavailable,
			"No organization is in scope, so there is nobody to look up."), nil
	}

	seats := Corpus(turn.Org)
	found := colleague.Resolve(query, seats)
	safe := clip(query)

	switch {
	case len(found) == 0:
		log.Debug("lookup_colleague_no_match", "query", safe, "corpus", len(seats))
		// NOT FOUND, because the colleague IS what this call is about —
		// unlike a2a_ask's `target`, where an unmatched spelling is an
		// argument to correct before the ask can be sent.
		return refused(tools.RefusalNotFound, fmt.Sprintf(
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
			// UNREACHABLE TRANSPORTS ARE LEFT OUT. This map is both the
			// exact-id index and what describe renders under a person's
			// name, so an operator id would appear beside their Slack id as
			// though it were somewhere an agent could mention them. It is
			// an attribution key on the engine's own surface, and the seat
			// handle already addresses that seat.
			if !id.Transport.Reachable() {
				continue
			}
			// Keyed by transport, so an exact-id query matches whichever
			// platform the id was copied from without the tool having to
			// be told which.
			seat.External[string(id.Transport)] = id.ExternalID
		}
		out = append(out, seat)
	}
	return out
}

// Parties resolves a handle to that person's two identities, from the chart —
// what [WorkDeps.Party] is wired with.
//
// HERE RATHER THAN IN [Corpus], because the roster deliberately leaves an
// operator id out: that map is the exact-id index AND what `lookup_colleague`
// renders, so a credential in it would appear beside somebody's Slack id as
// though it were somewhere an agent could mention them. This is the same fact
// asked for by name instead.
//
// A HANDLE NO SEAT HOLDS IS STILL A PARTY — somebody who has left, a handle on
// old rows — because the question is about the ROWS and this lookup only ever
// ADDS an alias.
//
// # It is the other direction of the same lookup, and cannot disagree with it
//
// `operator.WorkActor` walks credential → seat and this walks seat → credential
// — as does the dashboard's own `queries.Sources.partyOf` — and all of them go
// through the one resolution in `org`: see [org.Organization.SeatByOperatorID],
// whose own doc says why a `${VAR}` has to be resolved rather than compared. A
// person bound for one direction and unbound for the other is a dashboard that
// knows who they are and shows them nothing.
//
// A nil chart, or one this build has not loaded, answers the handle alone.
func Parties(chart func() *org.Organization) func(string) tracker.Party {
	return func(handle string) tracker.Party {
		party := tracker.PartyOf(handle)
		if chart == nil {
			return party
		}
		o := chart()
		if o == nil {
			return party
		}
		// NIL LOOKUP, so a `${VAR}` binding resolves against this
		// process's own environment, which is where every other
		// consumer of `contact` resolves one.
		party.OperatorID = o.SeatByHandle(handle).ResolvedOperatorID(nil)
		return party
	}
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
