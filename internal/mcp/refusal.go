package mcp

import (
	"errors"
	"slices"
)

// Refusal is the machine-readable class of a tool call that did not do what it
// was asked, set ONLY by a first-party tool.
//
// # Why a class beside the sentence
//
// Because the sentence has one reader and the class has another. A model reads
// [Result.Output] and is expected to act on its words — that is why the output
// is the whole story for it, and why the refusal text names the argument to
// change. A PERSON's surface cannot: the dashboard has to decide whether to
// show the tool's sentence at all (it names a field only for [RefusalInvalid]
// and [RefusalForbidden]), which HTTP status the write surface answers, and
// whether a retry could ever help — and every one of those decisions, made by
// matching the prose, is a decision that changes when somebody rewords a
// refusal for the model's benefit. The prompt text is tuned against observed
// model behaviour; it cannot also be a wire contract.
//
// # Why an MCP server's failure carries none
//
// Because nothing it says can be classified honestly. A third-party server
// reports failure as a bare isError with prose, and inferring a class from
// that prose would be the same prose-matching this type exists to end, applied
// to text the engine did not write. An unclassified failure is [Result.Failed]
// with no class in its [Result.Cause] — [RefusalOf] answers "" — and a reader
// that needs a class treats it as the server's own failure rather than
// guessing one.
//
// # Why it is an ERROR, carried in the cause
//
// Because the class is a fact about WHY the call failed, and [Result.Cause]
// is where why lives. A tool states its class as the cause outright (`Cause:
// RefusalInvalid` for an argument it refuses with nothing underneath), or
// beneath the error that decided it ([Classify]), and [RefusalOf] reads it
// back with [errors.As]. A class field beside the cause was the other shape,
// and it was two classifiers of one failure: a write surface branching on
// the cause and an audit row recording the field could tell one caller two
// different stories about the same call.
//
// # Why it is a named string with [Refusal.Valid]
//
// The value travels: a newer build may classify a refusal this build has no
// constant for, and an unknown class must arrive as a VALUE a reader can
// branch on (and fall back from) rather than a decode error. See
// CLAUDE.md's rule for enums.
type Refusal string

const (
	// RefusalInvalid — an argument is wrong, and the sentence names which
	// one. The only class whose fix is "send something different", and the
	// default for a refusal that says nothing more specific.
	RefusalInvalid Refusal = "invalid"

	// RefusalNotFound — the object the call is ABOUT does not exist. Not
	// an argument that merely names a missing object (a parent, a
	// dependency, an assignee): that is [RefusalInvalid], because the fix
	// is the argument rather than the request.
	RefusalNotFound Refusal = "not_found"

	// RefusalForbidden — this caller may not do this here: outside a turn,
	// on somebody else's behalf, into a reserved container.
	RefusalForbidden Refusal = "forbidden"

	// RefusalStaleVersion — the object changed after the caller read it.
	// Nothing is wrong with the edit; the caller reads again and decides
	// from what it says now.
	RefusalStaleVersion Refusal = "stale_version"

	// RefusalConflict — the write lost its race against other writers for
	// its whole round budget, or another bulk gesture holds the object, or
	// the running turn a note is for already holds as many unread notes as
	// it takes. Each clears on its own: try again shortly.
	RefusalConflict Refusal = "conflict"

	// RefusalExists — the object this call would create already exists.
	RefusalExists Refusal = "exists"

	// RefusalAlreadyAnswered — the question this answers has an answer.
	RefusalAlreadyAnswered Refusal = "already_answered"

	// RefusalReassignmentBudget — the item has been handed on as often as
	// it may be. The class that must NOT invite another attempt.
	RefusalReassignmentBudget Refusal = "reassignment_budget"

	// RefusalInboxFull — a person's inbox list is at its ceiling.
	RefusalInboxFull Refusal = "inbox_full"

	// RefusalNotRunning — the run or turn the call addresses is not
	// running, or not waiting for what the call supplies.
	RefusalNotRunning Refusal = "not_running"

	// RefusalSteerUnsupported — the running turn's runtime cannot take a
	// note mid-turn.
	RefusalSteerUnsupported Refusal = "steer_unsupported"

	// RefusalBudgetExhausted — the company's token budget has no room left
	// in one of its windows, so a call that would spend tokens was not
	// made. Nothing was spent. It clears when that window turns over or
	// when somebody raises its ceiling, never by trying again sooner — the
	// sentence names the window and when it resets.
	RefusalBudgetExhausted Refusal = "budget_exhausted"

	// RefusalUnavailable — this node cannot serve the call right now, or
	// this company does not run what it needs: the node is behind its log,
	// the log refused the read or the append, the coordination store did
	// not answer, the backend is not configured. A CONDITION of the node,
	// which is what tells it from [RefusalInternalError]. Never "the
	// object does not exist" — a caller told that files a duplicate.
	RefusalUnavailable Refusal = "unavailable"

	// RefusalPeerUpgrading — the fleet has a node too old to carry this
	// gesture, so it is refused everywhere until the upgrade finishes.
	RefusalPeerUpgrading Refusal = "peer_upgrading"

	// RefusalInternalError — this node FAILED at something of its own: a
	// read of its own store that broke, a database that would not answer,
	// a path in its own code that went wrong. Nothing about the call was
	// wrong, and — which is what tells it from [RefusalUnavailable] —
	// waiting does not clear it: a node behind its log catches up and a
	// coordination store that blinked answers again, while a store that
	// cannot be read stays that way until somebody fixes the node. Read as
	// unavailable, a client was told to come back in two seconds, for
	// ever.
	//
	// ITS SENTENCE IS FIXED and its error is in the node's log, never in
	// the sentence: a store's error is a driver's message, a SQL fragment
	// or a database path, and the sentence reaches a model's prompt, an
	// operator's assistant and a person's screen — none of whom can act
	// on it, while whoever runs the node reads the log.
	//
	// SPELLED AS THE ERROR ENVELOPE'S OWN 500, because that is what a
	// person's surface answers it with, the classes are the codes, and an
	// unclassified failure — a first-party tool that forgot to classify —
	// is answered the same: both say the engine broke, and neither has a
	// remedy the caller can apply.
	RefusalInternalError Refusal = "internal_error"
)

// Refusals is every class this build knows, in the order a reader's table
// lists them.
var Refusals = []Refusal{
	RefusalInvalid, RefusalNotFound, RefusalForbidden,
	RefusalStaleVersion, RefusalConflict, RefusalExists,
	RefusalAlreadyAnswered, RefusalReassignmentBudget, RefusalInboxFull,
	RefusalNotRunning, RefusalSteerUnsupported, RefusalBudgetExhausted,
	RefusalUnavailable, RefusalPeerUpgrading, RefusalInternalError,
}

// Valid reports whether a class off the wire is one this build knows. The
// empty class is not: it means "unclassified", which is a different fact.
func (r Refusal) Valid() bool { return slices.Contains(Refusals, r) }

// Error makes a class a cause a tool can state outright: a refusal with
// nothing underneath it — an argument the tool itself found wrong — carries
// its class as [Result.Cause], and one decided by something underneath
// carries it beneath that error through [Classify].
func (r Refusal) Error() string { return "refused: " + string(r) }

// ErrOutcomeUnknown is the cause of a failed write whose outcome NOBODY CAN
// VOUCH FOR: the append's acknowledgement was lost, or the node answered
// before it applied, so the write may have landed. [UnknownOf] is exactly
// [errors.Is] against it.
//
// DEFINED HERE, beneath every tool, because the tools that make writes sit
// above this package and the readers of a result — the audit, the loop's
// record, a person's write surface — must be able to ask without importing
// them: a tool's own unknown-outcome error wraps this one.
//
// IT IS [RefusalUnavailable] TOO, and wraps it, because the class answers
// "can this node serve the call now" — and the answer is still no — while
// this answers a different question: whether the caller has to find out what
// happened before doing anything else. A person's surface that read the class
// alone would tell somebody "nothing happened" about a write that may have
// landed, and they would make it a second time. A model reads
// [Result.Output], which says all of this in words.
var ErrOutcomeUnknown error = &unknownOutcome{}

// unknownOutcome is [ErrOutcomeUnknown]'s type: a sentinel that is also
// [RefusalUnavailable], so [RefusalOf] reads it as that class and no caller
// has to state both.
type unknownOutcome struct{}

func (*unknownOutcome) Error() string { return "whether the write landed is unknown" }

func (*unknownOutcome) Unwrap() error { return RefusalUnavailable }

// Classify states a refusal's class BENEATH the error that decided it, so a
// caller branching on the domain's own error ([errors.Is] against a tracker or
// knowledge-base sentinel) and a reader asking [RefusalOf] read one value.
//
// The class it is handed WINS over any class err already carries: it is the
// tool's statement about this call, made with what the tool knows about the
// gesture, and the class underneath was somebody's statement about a part of
// it. A nil err is the class alone. The message is err's, because the class is
// already a value and a log line reads the reason.
func Classify(class Refusal, err error) error {
	if err == nil {
		return class
	}
	return &classified{class: class, err: err}
}

// classified is [Classify]'s error: the class first, so [errors.As] meets it
// before anything err carries.
type classified struct {
	class Refusal
	err   error
}

func (c *classified) Error() string { return c.err.Error() }

func (c *classified) Unwrap() []error { return []error{c.class, c.err} }

// RefusalOf is the class of a failed result, read out of its [Result.Cause]:
// the first [Refusal] the cause's chain carries, or "" for a result that is
// not failed or whose cause states none — an MCP server's failure, which this
// engine will not classify by guessing.
//
// A SUCCESS HAS NO CLASS, whatever its cause says: the class describes a call
// that did not do what it was asked, and the caller of a successful one was
// answered with what it did.
func RefusalOf(r Result) Refusal {
	if !r.Failed || r.Cause == nil {
		return ""
	}
	var class Refusal
	if errors.As(r.Cause, &class) {
		return class
	}
	return ""
}

// UnknownOf reports whether a failed result's write MAY HAVE LANDED — whether
// its cause is [ErrOutcomeUnknown]. Every such result is also
// [RefusalUnavailable] by [RefusalOf], since the sentinel wraps that class.
//
// A SUCCESS IS NEVER UNKNOWN, for [RefusalOf]'s reason: its caller was told
// what it did.
func UnknownOf(r Result) bool {
	return r.Failed && errors.Is(r.Cause, ErrOutcomeUnknown)
}
