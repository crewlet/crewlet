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
// task's, comment's, view's, checklist item's, page's, room's or chat
// message's own — comes from ONE identity per call, chosen by one rule:
//
//   - IN A TURN, the turn: its work key (or its run, for a turn with no
//     ledgerable trigger), the instant that unit of work began, the tool, a
//     digest of the call's own arguments and how many different calls the
//     run made first to the same tool, or to a tool that undoes it — its
//     FAMILY, which the domain owning the pair declares (chatlet.CallFamily:
//     joining a room and leaving it) — derived ONCE, as the call's key
//     ([callOpKey], [Actor.callKey], [Actor.OperationSince],
//     turnctx.CallLog.OrdinalAmong), and every id the call's writes carry is
//     derived from that key: by the knowledge base's and chat's stores, and by
//     these tools for the tracker ([opIDFor], [commentID]). A re-run makes the
//     same calls and derives the same ids; a different call, or the same one
//     after a different one — a leave between two joins included — is a new
//     operation.
//   - OUTSIDE A TURN, the request's or the call's own operation. A transport
//     that names its requests — the act transport and the HTTP write
//     surface — carries the `Idempotency-Key` a retry repeats, held to the
//     operation grammar and scoped to the principal by internal/api/opkey,
//     as [Actor.WorkKey], put there by [Actor.Keyed] and dated by the key's
//     own instant, a key that carries none refused (and as
//     [doclet.Actor.OpKey] for a page write, [chatlet.Actor.OpKey] for a chat
//     one), so a request is a unit of work exactly as a turn's trigger is
//     and every id is derived from it as a turn's are. The operator's
//     assistant over MCP names the call's OPERATION ([Actor.Operation])
//     instead: it brings back the `op_id` its call was answered with, or is
//     minted one; that operation carries the instant it was minted and NAMES
//     THE CALL — the tool and a digest of its arguments — so it is accepted
//     only with that call: the same call made again is the same operation,
//     and anything else under it is refused before a write. That is ONE rule
//     for every family of write tools — Worklet's, the artifacts', the
//     datastore's and Gitlet's ([operations]) — and only what a turn
//     or a key derives stays each family's own.
//   - WITH NEITHER — a tool that takes no `op_id`, called over MCP — each
//     write is fresh, since nothing will repeat it. The page and chat tools
//     are such tools, and so are write_project and write_work_catalogue,
//     whose writes restate what they declare, so a repeat under a new
//     operation already changes nothing a first attempt landed.
//
// There is deliberately no second key: a request key beside the operation, a
// seed beside the turn, would be a second answer to "is this the same write",
// free to disagree with the first. And a caller is told only ITS OWN way to
// repeat: an answer carries an `op_id` only where its caller holds an
// [Actor.Operation] ([withOperation], [sameCall]).
//
// # And what it answers when nobody knows
//
// A write whose outcome is `unknown` is answered as a FAILED result naming the
// operation to make again ([unknownWrite], [failedUnknown]) — never a receipt
// carrying `outcome: unknown` beside a key or a version, which a model reads as
// the write done and a person's screen would draw.
//
// # The company's servers
//
// ssh_read and ssh_operate reach a seat's servers for the company alone, and
// ssh_operate only once a person approved that one call (ADR-0107, ssh.go).
//
// # What an agent records in the atlas
//
// atlas_record and atlas_dispute are an agent seat's alone, and each is held
// to a tool call of the same turn: the engine reads the cited call's answer
// itself, derives the record's confidence from the cited tool, and lets an
// agent remove nothing and decide no meaning (ADR-0132, atlasrecord.go).
//
// # Whose failure a condition is
//
// A failed phase or turn records a condition under a kind ([ConditionKind]):
// the environment's, never the engine's own defect. A coding sandbox's class
// is [SandboxKindPrefix] and the class ([sandbox.Why]); a deadline is
// `timeout` and an abandoned wait `canceled`, the provider layer's own words.
//
// THE STATE LOG HAS TWO KINDS. [KindStateLogUnavailable] is the log, the node
// or the broker refusing a read or a write (the node evicted, behind or
// deferring, the log full or wrong, a broker that refused it), which another
// node, a later attempt or an operator gets past. [KindStateLogWriteRefused]
// is the log refusing a write for what the write itself is or names
// ([statelog.Reason.RefusesTheWrite]: a record too large for the broker, an
// object deleted for good, an operation id that names another write, an
// operation a later record undid), which every node and every attempt meets
// alike. One kind for both put a write the work asked for and no node could
// make beside an outage, on the node's side. [stateLogSaid] picks between
// them; every refusal but a write refused for what it is is the first.
//
// EVERY KIND IS THE INFRASTRUCTURE'S OR THE WORK'S, decided once
// ([InfrastructureKind]). A failure on the infrastructure (a store, a log, a
// broker, a disk, the fleet's nodes and what they hold) is counted as the
// failing node's, never the work's: a seat's mail that failed on one is
// handed back as the node's own (internal/engine's nakFailure), and a
// delivery it spends is kept with its group's unhandled deliveries rather
// than with its poison. Every kind the package declares is the
// infrastructure's but the ones a piece of work can cause: a write the state
// log refused for what the write itself is ([KindStateLogWriteRefused]: no
// node can make it and no wait lets one, and a different write, a split
// change, a fresh operation or an object not deleted, is what gets past it);
// and a deadline and an abandoned wait, a call that ran past its own wall
// clock or that its caller gave up on, which the length of what was asked
// decides as much as the node does. Anything that is no kind of a condition
// is on neither side. A record's kind does not always name the failure its
// mail is sided by: a provider's failure that wraps a condition (a harness
// whose prompt a full disk could not take) is recorded under the provider's
// own kind (internal/agent/runner's ErrorKind), while its mail is handed back
// as the node's.
//
// A CODING SANDBOX'S CLASS IS PUT ON A SIDE CLASS BY CLASS, by whether a mail
// can cause it ([sandboxClassIsTheNodes], each class with its reason), never
// as a block: a provider that did not answer or is at its account's limit
// spent a seat's backlog into its poison when every class was the work's.
// Every class a turn or a resume fails on is met where the engine starts a
// box for the seat (the provider's create, the company's setup recipe, the
// engine's probes of the box, a relaunch), and every request and command
// there is the deployment's or the engine's own; a command the seat's
// executor runs in its box ends with its own exit status (a timeout's 124, a
// signal's), never with a class (internal/sandbox's e2b.go). So a provider
// that did not answer, at the account's limit, wanting it paid for or
// refusing the company's credentials, a setup download failing on the far
// side and a configuration the deployment cannot start are each the node's;
// a class this build does not name is not, and a class added there is
// decided on its own.
//
// OF THE INFRASTRUCTURE'S, THE ONES ONLY AN OPERATOR CLEARS are decided there
// too ([OperatorKind]): this node's disk full, mounted read-only or failing to
// read or write, its limit of open files reached, or the fleet's nodes holding
// different keys or running different builds. No wait clears one: somebody
// has to act on the host, on the keys or on the upgrade. A finished job whose
// resume fails on one waits for the operator: the resume give-up counts no
// failure of the node's (internal/sandbox's resumeGivenUp), so the run and its
// seat wait for as long as the condition lasts, and nothing ends it but a
// turn's deadline. So it is said: while one such run waits, the node holding
// the seat says so in its status record (internal/sandbox's
// Coordinator.ResumesBlocked), and every data node raises
// `executor_resume_blocked` from there. Every other kind, one that clears when
// a store, a log or a broker answers again included, is not an operator's.
//
// EVERY KIND THIS PACKAGE DECLARES, AND EVERY CLASS THE SANDBOX NAMES, IS
// NAMED in [InfrastructureKind] on one side, and
// TestEveryConditionKindIsTheNodesOrTheWorks holds every declared and every
// recorded kind to exactly one side, so a new kind or class lands on the side
// its author chose rather than on a default.
package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("agent.builtin")

// LookupColleagueTool is the tool's wire name.
const LookupColleagueTool = toolname.LookupColleague

// lookupColleague resolves a free-text query to one colleague, or to the list
// it might be.
//
// NEVER A GUESS. An agent that silently addressed the wrong colleague is worse
// than one that asked which — the wrong person gets pulled into work that is
// not theirs, and the right one never hears about it. So an ambiguous query
// returns the candidates and says they are candidates.
//
// env resolves the chart's `${VAR}` contacts ([Deps.Env]).
type lookupColleague struct{ env org.EnvLookup }

var _ tools.SeatCallable = (*lookupColleague)(nil)

func (t *lookupColleague) Name() string { return LookupColleagueTool }

func (t *lookupColleague) Description() string {
	return "Look up a colleague — AI agent or human teammate — by any " +
		"identifier: handle, role name, Slack user ID, Jira/Confluence " +
		"account ID, GitHub or GitLab username. Returns the canonical " +
		"handle, the seat kind (agent | human), and every known " +
		"cross-platform identity; human results add what they own and how " +
		"to reach them (a mention and an asynchronous reply — never " +
		A2AAskTool + ", which addresses agents only). Matching is " +
		"case-insensitive and falls back to partial and fuzzy matching, so " +
		"'ceo' finds the role 'Agent CEO'; when more than one colleague " +
		"matches, the candidate list is returned instead of a guess. Use " +
		"this before " + A2AAskTool + " or before @mentioning anyone."
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
		return failed(LookupColleagueTool + " needs a `query`: a handle, a role name, or an id on any connected platform."), nil
	}
	if turn == nil || turn.Org == nil {
		return refusedBy(tools.RefusalUnavailable, waitless(),
			"No organization is in scope, so there is nobody to look up."), nil
	}

	seats := Corpus(turn.Org, t.env, turn.WithholdsContacts)
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
	fmt.Fprintf(&b, "%q is ambiguous — %d colleagues match. Call "+LookupColleagueTool+" "+
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
//
// lookup resolves a person's `${VAR}` contact ids
// ([org.HumanContact.ResolvedIdentities]); a node passes its own chain, the
// secret store and then the environment it was handed, which is what the
// notification routing that mentions these same people reads. Nil reads the
// process environment.
//
// withheld is the identity directory's reading, the one the party registry was
// built from: a human seat it withholds keeps its place and loses every
// external id, so an id copied from that person's accounts matches nobody —
// exactly as an inbound message from them resolves to nobody — and the seat is
// marked [colleague.Seat.Withheld]. Nil withholds nothing.
func Corpus(o *org.Organization, lookup org.EnvLookup, withheld func(handle string) bool) []colleague.Seat {
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
		if role.IsHuman() && withheld != nil && withheld(seat.Handle) {
			seat.Withheld = true
			out = append(out, seat)
			continue
		}
		for _, id := range role.Contact.ResolvedIdentities(lookup) {
			// EVERY TRANSPORT HERE IS A PLACE A MESSAGE CAN BE SENT.
			// It was not always: a `crewlet` transport carried the
			// operator id a Tier A token bound a seat with, and it had
			// to be filtered out of this map — which is both the
			// exact-id index and what describe renders under a person's
			// name — or an attribution key appeared beside their Slack
			// id as though an agent could mention them there. The
			// identity estate holds that binding now and the transport
			// is gone, so there is nothing left to filter.
			//
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
	switch {
	case s.Kind == string(org.KindHuman) && s.Withheld:
		// NOT WHY: a person's standing is not an agent's business, and
		// what the agent needs is only that nothing it sends will arrive.
		b.WriteString("  reach them: they cannot be reached on any chat or " +
			"code-host account right now, so there is nobody to mention or " +
			"message. Work for this seat goes in the PM tool, assigned to " +
			"the seat. " + A2AAskTool + " addresses AGENTS only and will not reach them.\n")
	case s.Kind == string(org.KindHuman):
		b.WriteString("  reach them: mention them on a shared surface and " +
			"continue without waiting — a person answers asynchronously. " +
			A2AAskTool + " addresses AGENTS only and will not reach them.\n")
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
