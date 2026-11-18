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
// # What the tracker's tools derive from a call's key
//
// The tracker's store is handed the ids its writes are published under, so
// its tools derive them from the call's key ([callOpKey]) rather than handing
// the key on, each under a namespace of its own ([opIDNamespace],
// [commentNamespace]) that keeps the ids apart from each other and from every
// store's derivation over the same key. A namespace is FIXED for the life of
// the format: a new one makes every retry straddling the change a second
// write.
//
// The operation id ([opIDFor]) is named `<verb>-<object>`, the object one
// grammar segment ([statelog.OpNamePart]), where the stores name theirs by the
// verb alone: under an [Actor.Operation] each write is a STEP whose name is
// its whole identity within the call, and one call writes one verb on several
// objects. A comment's id ([commentID]) and a created task's or view's
// ([createdTaskID], [createdViewID]) are UUIDv5s, not operation ids, because
// each keys its row apart from the operation writing it (a page's comment, by
// contrast, IS its operation: [statelog.KeyedObjectOpID]). So they derive from
// the key as the work began ([Actor.asBegun]): a rebase re-mints operations
// for a ledger that forgot them, and the row has not forgotten. Every id binds
// the call's digest beside the key, as the stores bind what a write says,
// since a request's key names the request rather than what it asked; in a
// turn the digest is already in the key.
//
// # The code tools
//
// The code tools (codetools.go and its siblings) read and write the company's
// Gitlet repositories on the server, never in a clone, from ONE list
// ([CodeTools]) a seat's registry and the operator catalogue share.
//
// Every read is judged where it is answered. The gate admits everybody
// (`authz.ClassEverybody`), since a repository's read grant is derived by the
// serving node from its own chart (ADR-0088) and the gate cannot ask it before
// the read. So each call names its caller ([CodeAskerOf]) and the answering
// node holds the read to that grant: a listing leaves out what the caller may
// not read, and a read of it is refused `not_permitted`. No argument names who
// reads.
//
// Every read is decided again for whoever woke the turn ([ceilingOf]): a seat
// reading in a turn somebody woke is their deputy, and would otherwise hand a
// `visibility: unit` repository to anybody who can message one of the unit's
// seats. The tool's row is decided for the requester ([authz.DecideWithin]),
// so a stranger, or a person who may not act now, reads nothing. A colleague
// seat's grant rides on every read ([gitlet.ReadQuery.CeilingSeat]) and is
// judged by the answering node ([gitlet.ErrAboveCeiling]), never on the turn's
// memo of the repository, which would keep reading for a colleague a later
// activation shut out. A person bounds nothing (every person maintains every
// repository, ADR-0085), and a call outside any turn reads for its caller
// alone. The ceiling only narrows: what is read is the seat's own read.
//
// Rows at the seat's level, objects by id. A repository and its refs are rows,
// read linearizably ([seatReadLevel]), so an answer includes every push that
// landed before the call. An object id names the same object on every node,
// so its read needs no barrier: it is asked at a SYNC level floored at this
// node's own pushes to the repository and at where each named ref last moved
// ([CodeDeps.gitFresh], [gitlet.RefsFloor]) — never at the checkpoint the
// resolve read the ref at, which sits past its own barrier and would make a
// current copy sync for nothing on every read by a ref. A turn pays one
// barrier per repository ([CodeDeps.repo]) and one per call that names refs,
// all resolved in one read ([CodeDeps.resolveRef]); a call naming commits by
// id pays none beyond the repository's.
//
// Text somebody else wrote is fenced. A file's lines (code_read, code_blame),
// a patch (code_diff) and a commit's subject (code_log, code_blame, up to
// [gitlet.MaxLogSubjectBytes]) are a committer's words a read by object id
// cannot attribute, so each goes out fenced at trust `external`, naming no
// writer (ADR-0040); every other string — a path, a ref name, a description,
// an author — is defused ([foreignRead]). The Gitlet node counts the fences
// and the defusing into a page's bytes (internal/gitlet/query's page.go).
//
// One page is one tool answer, sent as measured. A Gitlet node cuts a page at
// [gitlet.MaxGitAnswerBytes] of compact JSON, and these tools send it compact
// ([compactAnswer]): indented, a page of short entries would overrun the
// answer it was cut to fit. A row listing is cut here, whole rows, at the
// answer as sent ([foreignRead.fit]). A page the fence lengthens past the
// answer (text spelling fence markers) is refused naming a narrower read,
// never cut.
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
// NEVER A GUESS: addressing the wrong colleague pulls the wrong person into the
// work and leaves the right one unaware, so an ambiguous query returns the
// candidates, labelled as candidates.
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

// Call without a turn cannot resolve anyone (the corpus is the turn's pinned
// org); it answers a refusal rather than an error, since the model's request
// was reasonable.
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
		// NOT FOUND: the colleague is what this call is about, unlike
		// a2a_ask's `target`, where it is an argument to correct.
		return refused(tools.RefusalNotFound, fmt.Sprintf(
			"No colleague matches %q. Known handles: %s.",
			safe, strings.Join(allHandles(seats), ", "))), nil

	case len(found) == 1:
		return tools.Result{Output: describe(found[0].Seat)}, nil
	}

	// The word "ambiguous", so the model retries with a handle rather than
	// picking the first row.
	var b strings.Builder
	fmt.Fprintf(&b, "%q is ambiguous — %d colleagues match. Call "+LookupColleagueTool+" "+
		"again with one of these handles:\n", safe, len(found))
	// EVERY candidate, uncapped: a handle the model cannot see is one it
	// cannot retry with. The org chart bounds the list.
	for _, c := range found {
		fmt.Fprintf(&b, "  - %s (%s, %s)", c.Seat.Handle, c.Seat.Name, c.Seat.Kind)
		if label := c.Method.Label(); label != "" {
			fmt.Fprintf(&b, " — %s", label)
		}
		b.WriteString("\n")
	}
	return failed(b.String()), nil
}

// Corpus is every addressable seat in an org: agents AND humans, since a
// lookup answers "who do I talk to".
//
// lookup resolves a person's `${VAR}` contact ids
// ([org.HumanContact.ResolvedIdentities]); a node passes the same chain its
// notification routing reads (secret store, then environment). Nil reads the
// process environment.
//
// withheld is the identity directory's reading the party registry was built
// from: a withheld human keeps the seat, marked [colleague.Seat.Withheld], but
// loses every external id, so an id from their accounts matches nobody, as an
// inbound message from them does. Nil withholds nothing.
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
			// Every transport here is a place a message can be sent: this
			// map is both the exact-id index and what describe renders.
			// Keyed by transport, so an exact-id query matches whichever
			// platform the id came from.
			seat.External[string(id.Transport)] = id.ExternalID
		}
		out = append(out, seat)
	}
	return out
}

// describe renders one resolved colleague.
//
// A human's entry says how to reach them and that a2a_ask is not the way: an
// agent that tries it waits on a channel no person watches.
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
		// Not why: a person's standing is not the agent's business, only
		// that nothing it sends will arrive.
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
// Uncapped: a model that missed reads it for the handle it meant, and
// "...and 7 more" withholds the answer.
func allHandles(seats []colleague.Seat) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Handle)
	}
	sortStrings(out)
	return out
}
