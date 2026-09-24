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
	"time"

	"github.com/crewlet/crewlet/internal/org"
)

// Turn is everything a turn's own code needs, and nothing else.
//
// IMMUTABLE after construction. Derive a new one rather than mutating it — a
// tool that could rewrite the seat it runs as would make every authorization
// decision downstream a suggestion. The one exception is [Turn.Calls], a log
// that only grows and that nothing authorizes on.
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

	// WorkSince is when that unit of work BEGAN: the earliest instant at
	// which any trigger event the key is derived from was created. Zero
	// exactly when WorkKey is empty.
	//
	// It travels with the key because it is half of the same identity. An
	// idempotent write derived from the key carries this instant as its
	// mint time, and the state log answers `unknown` rather than deciding
	// again an operation minted before its node's operation ledger may
	// have lost rows — to the ledger's own sweep, or to a snapshot adopted
	// from a donor that scrubbed it — so a
	// re-run must reproduce the instant exactly, as it reproduces
	// the key, and neither may move with the run. It is derived from the
	// SAME events the key is, for that reason (inbox.WorkSinceFor).
	WorkSince time.Time

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
	// it came through. Both travel so an A2A ask can refuse past the cap
	// rather than discovering the loop at runtime. A delegated worker needs
	// neither: it is a leaf that contacts nobody (see agent/subagent).
	Depth int
	Chain []string

	// ConversationKey is the conversation this turn is serving — the Slack
	// thread, the issue, the page — or empty for a trigger that has none.
	//
	// It travels because work this turn STARTS can outlive it and still owe
	// that conversation an answer: a detached coding run is resumed in
	// another process, days later, and what it reports has to land where
	// the task came from. The resume cannot recover this by looking at the
	// trigger, which may be long gone, so the launch writes it onto the
	// run's own row and the resumed turn reads it back.
	//
	// THE DURABLE IDENTITY, which for a direct message is the whole DM
	// channel rather than whichever thread the trigger arrived in.
	ConversationKey string

	// PartitionKey is the inbox partition the trigger arrived in, which is
	// the identity above or a finer cut of it.
	//
	// It travels for the same reason and no other: a detached run's row
	// states the batch it was launched from beside the conversation it is
	// answered on, and the launch is the only frame that can put either on
	// the row. Carried as a second field rather than collapsed into one,
	// because the two differ for exactly the surface a clarification is
	// most often asked on — a direct message — and one value answering
	// both questions is the defect this pair was split out of: filed under
	// the batch, a DM's coding work landed in a ledger row the next turn
	// never read, and matched on it the person's answer reached nobody.
	PartitionKey string

	// Task is the ask this turn is working on, and Reply says who is
	// waiting for it — [turn.Reply]'s wire value, carried as a plain
	// string so this package does not import the turn engine it is
	// carried through.
	//
	// Both travel for the same reason the two conversation values do: work
	// this turn STARTS can outlive it. A detached coding run is resumed in another
	// process, days later, with no trigger left to re-read — so the launch
	// writes both onto the run's row, and without them the resumed turn
	// would come back with no brief and free to end in silence on a
	// request somebody is still waiting for.
	Task  string
	Reply string

	// Calls is what this run has called so far, which a derived operation
	// id reads its repeat count from — see [CallLog].
	//
	// THE ONE PART OF A TURN THAT CHANGES, and only by growing: the tool
	// surface appends each call it made, and nothing can rewrite or drop an
	// entry, so no authorization decision reads it and nothing a model says
	// reaches it but the calls it actually made.
	Calls *CallLog
}

// CallLog is this run's call log, or nil outside a turn — see [Turn.Calls].
func (t *Turn) CallLog() *CallLog {
	if t == nil {
		return nil
	}
	return t.Calls
}

// WithCalls is this turn with calls as its log — a delegated worker's view of
// the run, on a fork of the run's own log ([CallLog.Fork]).
//
// A DERIVED TURN, never this one changed: a worker's calls must not reach the
// parent's log until its wave is done.
func (t *Turn) WithCalls(calls *CallLog) *Turn {
	if t == nil {
		return nil
	}
	derived := *t
	derived.Calls = calls
	return &derived
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
