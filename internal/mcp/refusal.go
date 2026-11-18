package mcp

import (
	"errors"
	"slices"
)

// Refusal is the machine-readable class of a tool call that did not do what it
// was asked, set ONLY by a first-party tool.
//
// The sentence is for a model; a PERSON's surface decides its status and retry
// advice (internal/api/operator's Fail, ADR-0060) from the class, not prose.
// The class is an ERROR in [Result.Cause], stated outright or beneath the
// deciding error ([Classify]) and read by [RefusalOf] with [errors.As]: one
// classifier of one failure. An MCP server's failure carries none ([RefusalOf]
// answers ""); a call refused ABOUT a down server is [RefusalUnavailable]
// (supervise.go). A named string with [Refusal.Valid], so an unknown class
// arrives as a value.
type Refusal string

// A refusal is classed, in every domain, by WHAT IT IS ABOUT and WHAT WOULD
// MAKE THE SAME REQUEST LAND: the request itself, or an argument naming
// ANOTHER object that cannot take part, is [RefusalInvalid]; a name or id
// another object holds is [RefusalExists]; the object missing or gone for good
// is [RefusalNotFound]; the object in a state refusing the gesture until
// changed (archived, trashed, frozen, its contents at their cap, a lost race)
// is [RefusalConflict]; a limit with a remedy of its own keeps its own class.
// So a room archived before a post is one class wherever met, a parent in the
// trash another, and a label another version holds a collision while every
// label in use is the artifact's state. internal/agent/builtin's
// TestEveryDomainRefusalIsClassed holds each domain sentinel to its meaning and
// class: a …Taken, …Clash or …Exists is a collision, a …Full a cap.
const (
	// RefusalInvalid — the REQUEST is wrong: an argument outside its
	// grammar, a value past a limit of its own, or an argument naming
	// another object that cannot take part, and the sentence names which.
	// The only class whose fix is "send something different", and the
	// default for a refusal that says nothing more specific. NOT a state of
	// the object the call is about — see [RefusalConflict].
	RefusalInvalid Refusal = "invalid"

	// RefusalNotFound — the object the call is ABOUT does not exist. Not
	// an argument that merely names a missing object (a parent, a
	// dependency, an assignee): that is [RefusalInvalid], because the fix
	// is the argument rather than the request.
	RefusalNotFound Refusal = "not_found"

	// RefusalForbidden — this caller may not do this here: outside a turn,
	// on somebody else's behalf, into a reserved container.
	RefusalForbidden Refusal = "forbidden"

	// RefusalVendorForbidden is the vendor a built-in reaches refusing the
	// credential the engine minted for the seat, which lacks the permission
	// the tool declares it needs (its [builtin.VendorNeed]). Said from that
	// declaration, never from the vendor's words.
	RefusalVendorForbidden Refusal = "vendor_forbidden"

	// RefusalStaleVersion — the object changed after the caller read it.
	// Nothing is wrong with the edit; the caller reads again and decides
	// from what it says now.
	RefusalStaleVersion Refusal = "stale_version"

	// RefusalConflict — the write lost its race against other writers for
	// its whole round budget, or the object the call is about is in a STATE
	// that refuses the gesture until somebody changes it — a room or a
	// project archived, an artifact in the trash or being purged, a task
	// frozen in the trash, a restore with nothing in the trash, or the
	// object's own contents at their cap, a message carrying all the emoji
	// it may — which the sentence names. Read the object again: a race
	// clears on its own, and a state is changed by whoever the sentence
	// names, after which the same request lands.
	RefusalConflict Refusal = "conflict"

	// RefusalNotReady — a change does not yet meet its target's merge gate:
	// the sentence names every unmet precondition and who it waits on, and
	// a ready_to_merge wake follows once the last of them is met. Nothing
	// about the call was wrong, which tells it from [RefusalInvalid]; and
	// neither the same call made again nor a re-read clears it, which tells
	// it from [RefusalStaleVersion] and [RefusalConflict]: the change's
	// reviewers, its checks or its author clear it, and the caller waits for
	// the wake rather than asking again (M5-D31).
	RefusalNotReady Refusal = "not_ready"

	// RefusalExists — the object this call would create already exists, or
	// a name, a title, a label, an address or an id the call gives an object
	// is another object's already: a COLLISION, which the sentence names the
	// holder of. Another value lands now, and the same one once its holder
	// gives it up — never "change an argument that is malformed"
	// ([RefusalInvalid]) and never "the object refuses this by its state"
	// ([RefusalConflict]).
	RefusalExists Refusal = "exists"

	// RefusalAlreadyAnswered — the question this answers has an answer.
	RefusalAlreadyAnswered Refusal = "already_answered"

	// RefusalReassignmentBudget — the item has been handed on as often as
	// it may be. The class that must NOT invite another attempt.
	RefusalReassignmentBudget Refusal = "reassignment_budget"

	// RefusalInboxFull — a person's inbox list is at its ceiling.
	RefusalInboxFull Refusal = "inbox_full"

	// RefusalQuotaExceeded — the write would take what is kept past a limit
	// on it: the company's storage (the artifacts' bytes, a datastore's
	// footprint), which the sentence names with the setting an operator
	// raises, or a cap on what one datastore holds (a collection's rows,
	// one participant's rows, a store's collections and documents), which
	// the sentence names. Nothing was written. NOT [RefusalForbidden]: no
	// permission lifts it, and a caller told it may not would go and ask
	// for one. It clears when somebody removes what is no longer needed or
	// raises a limit that can be raised, never by trying again.
	RefusalQuotaExceeded Refusal = "quota_exceeded"

	// RefusalNotesFull — the running turn a note is for already holds as
	// many notes as may wait unread. It clears when the turn reads them, at
	// its next step; nothing else changes it.
	RefusalNotesFull Refusal = "notes_full"

	// RefusalNotesSpent — the running turn has taken as many notes as a turn
	// takes, read or not. Like [RefusalReassignmentBudget], the class that
	// must NOT invite another attempt: nothing clears it for that turn, and
	// what is left to say belongs on the work item.
	RefusalNotesSpent Refusal = "notes_spent"

	// RefusalNotRunning — the run or turn the call addresses is not
	// running, or not waiting for what the call supplies.
	RefusalNotRunning Refusal = "not_running"

	// RefusalBudgetExhausted — the company's token budget has no room left
	// in one of its windows, so a call that would spend tokens was not
	// made. Nothing was spent. It clears when that window turns over or
	// when somebody raises its ceiling, never by trying again sooner — the
	// sentence names the window and when it resets.
	RefusalBudgetExhausted Refusal = "budget_exhausted"

	// RefusalStoreFull — the object store has no room for content the call
	// would store, so it was not stored, and nothing was recorded. Nothing
	// about the call was wrong and nothing in the node failed, which tells
	// it from [RefusalInvalid] and [RefusalInternalError]; and, which tells
	// it from [RefusalUnavailable], waiting alone does not clear it: the
	// same call is refused the same way until an operator makes room, so
	// its sentence names that room, as the store names it
	// (objstore.Full.Remedy), and never invites the retry. Read as
	// unavailable, a seat was told to try again shortly and uploaded the
	// same bytes into the same full bucket.
	RefusalStoreFull Refusal = "store_full"

	// RefusalUnavailable — this node cannot serve the call right now, or
	// this company does not run what it needs: the node is behind its log,
	// the log refused the read or the append, the coordination store did
	// not answer, the backend is not configured. A CONDITION of the node,
	// which is what tells it from [RefusalInternalError]. Never "the
	// object does not exist" — a caller told that files a duplicate.
	RefusalUnavailable Refusal = "unavailable"

	// RefusalInternalError — this node FAILED at something of its own: a
	// read of its own store that broke, a path in its own code that went
	// wrong. Nothing about the call was wrong, and, unlike
	// [RefusalUnavailable], waiting does not clear it until somebody fixes
	// the node; read as unavailable, a client was told to retry for ever.
	//
	// ITS SENTENCE IS FIXED and the error goes to the node's log: a driver
	// message, SQL fragment or database path reaches a model, an assistant
	// and a screen, none of whom can act on it.
	//
	// SPELLED AS THE ERROR ENVELOPE'S OWN 500, as are unclassified failures:
	// both say the engine broke, with no remedy the caller can apply.
	RefusalInternalError Refusal = "internal_error"
)

// Refusals is every class this build knows, in the order a reader's table
// lists them.
var Refusals = []Refusal{
	RefusalInvalid, RefusalNotFound, RefusalForbidden,
	RefusalStaleVersion, RefusalConflict, RefusalNotReady, RefusalExists,
	RefusalAlreadyAnswered, RefusalReassignmentBudget, RefusalInboxFull,
	RefusalQuotaExceeded, RefusalNotesFull, RefusalNotesSpent,
	RefusalNotRunning, RefusalBudgetExhausted,
	RefusalStoreFull, RefusalUnavailable, RefusalInternalError, RefusalVendorForbidden,
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
// before it applied, or an MCP server's session ended with the call in flight
// (supervise.go). [UnknownOf] is exactly [errors.Is] against it.
//
// DEFINED HERE, beneath every tool, so readers of a result can ask without
// importing the tools above: a tool's own unknown-outcome error wraps this.
//
// IT IS [RefusalUnavailable] TOO, and wraps it: the class says the node cannot
// serve the call now, this says the caller must find out what happened first.
// A surface reading the class alone would say "nothing happened" about a write
// that may have landed, and it would be made twice.
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
// The class handed WINS over any class err carries: it is the tool's
// statement about this call, the other about a part of it. A nil err is the
// class alone. The message is err's: a log line reads the reason.
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
