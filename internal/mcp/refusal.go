package mcp

import "slices"

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
// with an empty Refusal, and a reader that needs a class treats it as the
// server's own failure rather than guessing one.
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

	// RefusalUnavailable — this node cannot serve the call right now, or
	// this company does not run what it needs: the read failed, the log
	// refused the append, the backend is not configured. Never "the object
	// does not exist" — a caller told that files a duplicate.
	RefusalUnavailable Refusal = "unavailable"

	// RefusalPeerUpgrading — the fleet has a node too old to carry this
	// gesture, so it is refused everywhere until the upgrade finishes.
	RefusalPeerUpgrading Refusal = "peer_upgrading"
)

// Refusals is every class this build knows, in the order a reader's table
// lists them.
var Refusals = []Refusal{
	RefusalInvalid, RefusalNotFound, RefusalForbidden,
	RefusalStaleVersion, RefusalConflict, RefusalExists,
	RefusalAlreadyAnswered, RefusalReassignmentBudget, RefusalInboxFull,
	RefusalNotRunning, RefusalSteerUnsupported,
	RefusalUnavailable, RefusalPeerUpgrading,
}

// Valid reports whether a class off the wire is one this build knows. The
// empty class is not: it means "unclassified", which is a different fact.
func (r Refusal) Valid() bool { return slices.Contains(Refusals, r) }
