// Package turnctx carries what a turn IS, as an explicit argument.
//
// A turn has five values that want to travel ambiently — the work key, the
// config pin, the LLM seat, the phase recorder and the log fields — and each
// makes the same case for itself: "the consumer is a leaf and the intermediate
// frames have no business knowing". That case is sound for a leaf value and
// wrong for everything else, and in Go it is worse than wrong: a goroutine
// SHARES whatever it captured, for as long as it runs, including after the
// turn that created it has finished. There is no copy-on-spawn to bound it, so
// what would elsewhere merely obscure a dependency is a live data race.
//
// So a turn's inputs are an argument: [Turn], which the package name already
// qualifies.
//
// # Two identities, never one
//
// A turn carries BOTH a [Turn.RunID] and a [Turn.WorkKey], and collapsing them
// is the mistake ADR-0017 exists to stop being made again. They were one value
// once — the field was called ID and documented as the work key — and the
// consequence was that a redelivered trigger re-ran under the identity the
// failed attempt already occupied: its phases, its live row and its sandbox
// run all landed on top of the previous attempt's, so the retry was invisible
// while it ran and indistinguishable from the failure afterwards.
//
// # What is still allowed to be ambient, and the bar it clears
//
// Two values, each read by a genuine leaf called from code with no turn
// concept at all, each IMMUTABLE, and each failing SAFE when absent — nothing
// branches on their presence to decide correctness:
//
//   - the log fields, which decorate a line or do not.
//   - the seat handle a model call belongs to (llm.WithSeat). Absent resolves
//     to a named "shared" rather than an empty string, because the value
//     becomes a home directory and auxiliary work — summarisation, the
//     relevance filter — legitimately arrives unbound.
//
// The work key used to be a third, on the reasoning that store writers read it
// from frames below the dispatch. They do not: the writes it guards run in the
// reflection pass, a queue consumer on whichever node wins the delivery, and
// they read the key off the event payload. The ambient channel had no
// production reader at all and is gone; the key travels here, on [Turn].
//
// The config pin is deliberately NOT one of them: a turn reading config through
// an ambient channel is how a mid-turn reload gets observed halfway, which is
// the failure immutable epochs exist to remove. Neither is the phase recorder,
// whose whole job is to attribute spend to the phase that incurred it — an
// ambient one attributes it to whichever phase last wrote the context.
package turnctx

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/org"
)

// Turn is everything a turn's own code needs, and nothing else.
//
// IMMUTABLE after construction. Derive a new one rather than mutating it — a
// tool that could rewrite the seat it runs as would make every authorization
// decision downstream a suggestion.
//
// A goroutine that captures a Turn and outlives the turn is a bug, and the one
// no linter can see. The rule that makes it checkable: a Turn is PASSED, never
// stored in a struct field that outlives a turn. Anything needing turn state
// afterwards takes a copy of the values it wants.
type Turn struct {
	// RunID names THIS EXECUTION of the turn, and nothing else. Minted
	// fresh every time the engine runs a turn, so two runs of one trigger
	// — which redelivery makes ordinary — are two different values.
	//
	// It is what every event's `turn_id` carries, what a detached sandbox
	// run and an MCP bridge session are keyed on, and what a reader means
	// when they say "open this turn". See ADR-0017.
	RunID string

	// WorkKey identifies the UNIT OF WORK this turn did — the set of
	// trigger events it was dispatched for, stable across a re-run and
	// across nodes. See [internal/workkey].
	//
	// It is what a write must be idempotent against: a re-run posts the
	// same comment and files the same tracker operation, and only a key
	// that survives the re-run collapses them. EMPTY is legitimate and
	// means "a turn with no ledgerable trigger" — a scheduled fire, a
	// sub-agent — which has no cross-run duplicate to collapse.
	WorkKey string

	// Seat is who is acting. THE authorization fact: a tool that speaks
	// for a seat — asking a colleague, marking an onboarding step, writing
	// a diary entry — reads it from here and never from its arguments,
	// which the model controls.
	Seat *org.Role

	// Org is the company this turn is running in, pinned. Read from the
	// epoch the turn started under, so a colleague lookup mid-turn cannot
	// resolve against a roster that changed underneath it.
	Org *org.Organization

	// Depth is the delegation depth this turn inherited, and Chain is who
	// it came through. Both travel so a sub-agent or an A2A ask can refuse
	// past the cap rather than discovering the loop at runtime.
	Depth int
	Chain []string

	// Calls is what this turn has ALREADY invoked, oldest first, as the
	// tool loop recorded it. Empty on the turn the engine starts with; the
	// frame that runs one tool derives the turn that call sees — see
	// [Turn.WithCalls] — so what a tool is handed is the record as of its
	// own invocation.
	//
	// A VALUE, and that is the whole design. The alternative — a closure
	// on the Turn that asks the live surface how many calls it has run —
	// reads correctly in the frame that built it and keeps answering
	// afterwards: a Turn is documented immutable precisely so a goroutine
	// that captured one cannot observe a turn moving underneath it, and a
	// function field would smuggle exactly that back in. A slice copied
	// per call cannot change after the call is over.
	//
	// It exists for [Turn.CallOrdinal] and for nothing else yet. See
	// there for what a wrong answer costs.
	Calls []Call

	// ConversationKey is the conversation this turn is serving — the Slack
	// thread, the issue, the page — or empty for a trigger that has none.
	//
	// It travels because work this turn STARTS can outlive it and still owe
	// that conversation an answer: a detached coding run is resumed in
	// another process, days later, and what it reports has to land where
	// the task came from. The resume cannot recover this by looking at the
	// trigger, which may be long gone, so the launch writes it onto the
	// run's own row and the resumed turn reads it back.
	ConversationKey string

	// Task is the ask this turn is working on, and Reply says who is
	// waiting for it — [turn.Reply]'s wire value, carried as a plain
	// string so this package does not import the turn engine it is
	// carried through.
	//
	// Both travel for the same reason ConversationKey does: work this turn
	// STARTS can outlive it. A detached coding run is resumed in another
	// process, days later, with no trigger left to re-read — so the launch
	// writes both onto the run's row, and without them the resumed turn
	// would come back with no brief and free to end in silence on a
	// request somebody is still waiting for.
	Task  string
	Reply string
}

// Call is one tool invocation a turn has already made.
//
// THE TWO FIELDS AN ORDINAL IS DERIVED FROM, and no more. The loop's own
// record (`tools.Call`, `ledger.Call`) also carries the arguments and the
// output, and copying those per call would put a turn's whole transcript on
// every Turn every tool is handed — a cost that grows with the round count on
// a value whose only question is "how many of these have run".
type Call struct {
	Name string

	// Failed marks a call the tool reported failure for. Recorded rather
	// than inferred, exactly as the loop records it: see
	// [Turn.CallOrdinal] for why a failed call must not advance a
	// counter.
	Failed bool
}

// WithCalls derives the turn ONE call runs under: this turn, with the record
// of what ran before it.
//
// DERIVED RATHER THAN MUTATED, on this type's own rule — a Turn is immutable
// after construction, so the frame that invokes a tool builds the turn that
// call sees instead of rewriting the one it holds. The copy is shallow and
// deliberately so: every other field is either a scalar or a slice nothing
// appends to in place.
//
// # Who calls this, and what happens if nobody does
//
// The frame that runs a tool on behalf of a turn — `tools.Surface.invoke`,
// which already holds both the turn and the loop's record. A resumed phase
// has a SECOND source: the calls a suspended run made before it parked live
// on its `execstate` row rather than in this process's surface (see
// `runner.resumedCalls`), so a resume composes the parked record with the
// live one, oldest first.
//
// With nobody calling it every turn reports an ordinal of 0 for every call,
// which is SILENT: a seat that posts twice in one turn derives one message id
// twice, the applier's insert declines the second, and the turn believes it
// said two things. That is the failure [Turn.CallOrdinal] exists to prevent,
// so a build wiring a tool whose id is derived from the ordinal must wire
// this too.
func (t *Turn) WithCalls(calls []Call) *Turn {
	if t == nil {
		return nil
	}
	out := *t
	// CLONED, because the caller's slice is the loop's own live record and
	// it appends to it after this call returns. Sharing the backing array
	// would let a turn already in flight observe calls made after it.
	out.Calls = append([]Call(nil), calls...)
	return &out
}

// CallOrdinal is how many of the named tools this turn has already run
// successfully — the number the NEXT such call takes.
//
// # What it is for
//
// A turn may legitimately do the same thing twice: a seat that answers a
// question and then reports what it did has said two things, and both are
// real. Native chat derives a message's id from (turn, channel, ordinal)
// precisely so that the second remark is a second message while a RE-RUN of
// the whole turn — which redelivery makes ordinary — derives the same ids and
// writes each message once. Without the ordinal a turn's second post
// overwrites its first on every node, silently.
//
// # Why several names, and why success only
//
// SEVERAL NAMES, because the ordinal numbers the GESTURE rather than the
// tool: chat's three posting tools all derive ids in one space, so a room
// post and a thread reply in one turn must not both be call number zero.
// A caller passes the set that shares an id space — `chat.WriteTools` — and
// never a single name.
//
// SUCCESS ONLY, because a failed call wrote nothing. A model whose post was
// refused for a body over the cap retries it, and that retry is still the
// turn's FIRST message; counting the refusal would leave a hole in the
// numbering and — worse — make the ids depend on how many times the model
// got it wrong, which a re-run reproduces only by failing identically.
func (t *Turn) CallOrdinal(names ...string) int {
	if t == nil || len(names) == 0 {
		return 0
	}
	n := 0
	for _, call := range t.Calls {
		if call.Failed {
			continue
		}
		for _, name := range names {
			if call.Name == name {
				n++
				break
			}
		}
	}
	return n
}

// Handle is the acting seat's handle, or "" when there is no seat.
//
// The nil check is not defensive noise: a tool surface built outside a turn —
// a validate command, a test driving a runner directly — legitimately has no
// seat, and a tool that speaks for one must refuse rather than panic.
func (t *Turn) Handle() string {
	if t == nil || t.Seat == nil {
		return ""
	}
	return t.Seat.Handle()
}

// Role is the acting seat's role name, or "".
func (t *Turn) Role() string {
	if t == nil || t.Seat == nil {
		return ""
	}
	return t.Seat.Name
}

// AgentID is the acting seat's derived agent id, or "".
//
// Derived here rather than carried, because the derivation is a pure function
// of two values this type already pins — the org's name and the seat's handle
// — and a carried copy is a value that can disagree with the seat beside it.
// Empty for a human seat, which has no agent id at all rather than a zero one
// (see [org.Organization.AgentIDFor]), and for a surface built outside a turn.
//
// It exists because an event addressed by agent id is published from both the
// turn engine and the sub-agent fan-out, and only the first had the id to
// hand: a `provider_fallback` from inside a delegate call went out with the
// promoted column empty, so a row whose whole doc says it is addressed like a
// phase event could not be resolved to the seat that emitted it.
func (t *Turn) AgentID() string {
	if t == nil || t.Seat == nil || t.Org == nil {
		return ""
	}
	id, ok := t.Org.AgentIDFor(t.Seat)
	if !ok {
		return ""
	}
	return id.String()
}

// ErrNoSeat is what a seat-scoped tool returns when it was called outside a
// turn. Its own error rather than a string, so a caller can tell "this tool is
// unusable here" from "this tool ran and failed".
var ErrNoSeat = fmt.Errorf("turnctx: no acting seat")

// RequireSeat reports the acting seat, or ErrNoSeat.
func (t *Turn) RequireSeat() (*org.Role, error) {
	if t == nil || t.Seat == nil {
		return nil, ErrNoSeat
	}
	return t.Seat, nil
}

// ForSubagent derives the context an ephemeral sub-agent runs under.
//
// It KEEPS the org (a sub-agent must see the same company its parent does) and
// EXTENDS the delegation chain, refusing past the cap. The seat becomes the
// child's own: a sub-agent acting as its parent would make the delegation cap
// unenforceable, because nothing downstream could tell the two apart.
//
// THE CALL RECORD DOES NOT TRAVEL. A child runs its own loop and records its
// own calls, so carrying the parent's would number the child's first call
// after work it did not do. What makes that safe rather than merely tidy is
// that parent and child SHARE a RunID: two loops numbering from separate
// records would collide on any id derived from (run, ordinal), and the only
// reason they cannot is that every tool whose id is so derived writes to a
// shared surface, which the sub-agent guard denies a worker outright. A tool
// that changes is a tool that needs a seed of its own here.
func (t *Turn) ForSubagent(seat *org.Role, limit int) (*Turn, error) {
	if t == nil {
		return nil, ErrNoSeat
	}
	depth := t.Depth + 1
	if limit > 0 && depth > limit {
		return nil, fmt.Errorf("turnctx: delegation depth %d exceeds the limit of %d "+
			"(chain: %v)", depth, limit, t.Chain)
	}
	// Copied, not appended in place: append can share a backing array, and
	// two sub-agents derived from one parent would then write over each
	// other's chain.
	chain := make([]string, len(t.Chain), len(t.Chain)+1)
	copy(chain, t.Chain)
	if h := t.Handle(); h != "" {
		chain = append(chain, h)
	}
	return &Turn{
		RunID: t.RunID, WorkKey: t.WorkKey,
		Seat: seat, Org: t.Org, Depth: depth, Chain: chain,
	}, nil
}
