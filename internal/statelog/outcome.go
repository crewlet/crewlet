package statelog

import (
	"errors"
	"fmt"
)

// Outcome is what a write's caller is told, and there are exactly three
// answers because there are exactly three facts.
//
// # Why not a bool, and why not a bool beside a string
//
// This is [internal/coord]'s three-valued discipline one layer down: "held",
// "definitively not held" and "the store could not be reached" are three
// different facts, and collapsing the last two into false is the single most
// incident-hardened lesson in this engine. Here the same three are a PubAck, a
// wrong-last-sequence refusal, and no answer at all.
//
// The boolean that used to sit beside this — saying whether the apply wait
// succeeded — is deliberately absent. Durability is carried by the position
// being set and the wait is carried by the enum itself, so a third field could
// only ever disagree with one of them.
type Outcome string

const (
	// OutcomeApplied says both halves at once: the record is durable at
	// its position AND this node has applied it, so the rows the caller
	// is about to read are the rows the record produced.
	//
	// Not PERMANENT, and the difference matters exactly once: a deletion
	// marker committed above this position retroactively drops it.
	OutcomeApplied Outcome = "applied"

	// OutcomePending says the record is DURABLE at its position and what
	// it produced is unresolved HERE. Every other node will apply it.
	//
	// NEVER a failure and never "applied": a caller told this has a
	// position to resolve with later, which is a different thing to do
	// than retry.
	OutcomePending Outcome = "pending"

	// OutcomeUnknown says nothing can be established about this record
	// from this node. The op id travels with it, because retrying under
	// the SAME op id is the only safe retry — a fresh one would defeat
	// the ledger that exists for exactly this case.
	OutcomeUnknown Outcome = "unknown"
)

// Outcomes is every outcome, for validation and for a test that walks them.
var Outcomes = []Outcome{OutcomeApplied, OutcomePending, OutcomeUnknown}

// Valid reports whether an outcome off the wire is one this build knows.
func (o Outcome) Valid() bool {
	for _, known := range Outcomes {
		if o == known {
			return true
		}
	}
	return false
}

// Result is what a write answers with.
type Result struct {
	// Outcome is the three-valued answer.
	Outcome Outcome

	// Position is where the record landed. Set for applied and pending,
	// zero for unknown — which is the whole content of unknown.
	Position Position

	// OpID is the operation this write belongs to, and what a caller
	// retries under.
	OpID string

	// Version is the object's new version on an applied write: the
	// position packed, which is the same number the row carries.
	Version int64

	// Rounds is how many compare-and-set rounds this write took, for the
	// instrument that says whether an object is contended.
	Rounds int
}

// Wrote reports whether this result speaks for a record a caller has to
// account for: one that was appended (a position is set) or may have been
// (`unknown`, whose position is zero by definition). A write that decided it
// had nothing to append answers neither, and folding its outcome into a
// call's answer would claim a record that does not exist.
func (r Result) Wrote() bool {
	return !r.Position.IsZero() || r.Outcome == OutcomeUnknown
}

// LessCertain is the weaker of two write outcomes: `unknown` over `pending`
// over `applied`, and an empty outcome — nothing appended — under all three,
// since it claims nothing a caller must wait for.
//
// # Why a call that appended several records needs it
//
// A tool call, a dependency sequence, a save followed by a rename: each is ONE
// answer over several records, and that answer is read as "may I stop looking
// at this write?". It has to be the outcome a caller can rely on for EVERY
// record the call made — `applied` over a record the broker never confirmed
// tells a caller it is done with a write nobody can vouch for, and the rule
// every transport documents is that an unknown write is never reported
// applied. Taking the first record's outcome, or the last's, breaks that the
// moment the records disagree.
func LessCertain(a, b Outcome) Outcome {
	rank := func(o Outcome) int {
		switch o {
		case OutcomeUnknown:
			return 3
		case OutcomePending:
			return 2
		case OutcomeApplied:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Later is the later of two positions on one stream, the zero position losing
// to any other. It is the position a call that appended several records must
// answer, because a caller barriers on it before its next read: the earlier
// one is a floor BELOW a record the call made, and a read at it can come back
// without that record.
func Later(a, b Position) Position {
	if b.Packed() > a.Packed() {
		return b
	}
	return a
}

// Reason says why a write was refused, and each value names a different thing
// for the caller to do.
type Reason string

const (
	// ReasonEvicted — this node has been removed from the fleet. Nothing
	// it publishes will be applied anywhere. There is no retry.
	ReasonEvicted Reason = "evicted"

	// ReasonDeferred — this node holds a record it cannot decode whose
	// scope covers this object, so its rows are stale and any decision
	// taken from them is unsafe. Another node can serve this write.
	ReasonDeferred Reason = "deferred"

	// ReasonBehind — this node has not yet applied the caller's own
	// previous write, so a snapshot here would decide from a state the
	// caller has already moved. It clears on its own.
	ReasonBehind Reason = "behind"

	// ReasonBelowFloor — this node is below the trim floor, so a
	// retry-at-zero here could overwrite a peer's committed write. It
	// clears when the node catches up or adopts a snapshot.
	ReasonBelowFloor Reason = "below_floor"

	// ReasonFloorUnknown — the trim floor could not be read, which is
	// the third value and it BLOCKS. Failing open here is a lost update,
	// which is not recoverable; failing open where a claim is
	// deduplicated is a duplicate delivery, which is.
	ReasonFloorUnknown Reason = "floor_unknown"

	// ReasonDeleted — this object carries a permanent deletion marker.
	// It stays deleted; a guarding row's absence below the trim floor is
	// not permission to recreate it.
	ReasonDeleted Reason = "deleted"

	// ReasonGated — the record is durable and produced rows on NO node,
	// because a gate dropped it. Never re-decide: republishing produces
	// another durable record nothing applies.
	ReasonGated Reason = "gated"

	// ReasonRetired — the record names a kind the domain once published
	// and no longer applies, so it produces no rows anywhere.
	//
	// THE VERSION GATE CANNOT CATCH THIS ONE, which is why it is a reason
	// of its own: a retired kind arrives at a record version this build
	// reads perfectly, so nothing defers it, and the kind is simply gone
	// from the applier's dispatch. A rolling upgrade makes an older
	// peer's records ordinary traffic for as long as one takes, and
	// faulting on them wedges the newest node in the fleet.
	ReasonRetired Reason = "retired"

	// ReasonLogFull — the log is at its byte ceiling and refuses
	// appends rather than dropping records. An operator raises the
	// ceiling or unblocks the trim.
	ReasonLogFull Reason = "log_full"

	// ReasonSkew — the broker answered a last sequence BELOW an
	// expectation this node formed, which cannot happen on a healthy
	// stream and means a store or stream was restored out of step.
	ReasonSkew Reason = "skew"
)

// ErrUnavailable is what a refusal wraps, so a caller can tell a refusal from
// a conflict with errors.Is before it looks at the reason.
var ErrUnavailable = errors.New("statelog: unavailable")

// ErrConflict reports a write that lost its race for the whole round budget.
// It is the refusal a model reads as "somebody else is editing this", and it
// is the same refusal the tracker's own writes give today.
var ErrConflict = errors.New("statelog: conflict")

// Unavailable is a refusal naming what it is about.
//
// A REASON AND A DETAIL rather than a message, because two different readers
// need it: a surface switches on the reason to decide whether to retry
// elsewhere, and a person reads the detail to find out what to change.
type Unavailable struct {
	Reason   Reason
	Detail   string
	Position Position
	OpID     string
}

func (u *Unavailable) Error() string {
	if u.Detail == "" {
		return fmt.Sprintf("statelog: unavailable (%s)", u.Reason)
	}
	return fmt.Sprintf("statelog: unavailable (%s): %s", u.Reason, u.Detail)
}

// Unwrap makes every refusal answer errors.Is(err, ErrUnavailable).
func (u *Unavailable) Unwrap() error { return ErrUnavailable }
