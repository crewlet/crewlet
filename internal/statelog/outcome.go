package statelog

import (
	"errors"
	"fmt"
	"time"
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
// SUCCEEDED — is deliberately absent, and stays absent. Durability is carried
// by the position being set and success by the enum itself, so that field
// could only ever disagree with one of them.
//
// [Result.Waited] is not it, and the distinction is exactly the one that makes
// the removed field redundant and this one not. "The wait succeeded" is
// derivable — it is `applied`. "A wait was attempted at all" is not, once
// [Request.NoWait] exists: the two pendings it produces carry the same
// outcome and mean opposite things about this node.
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

	// Waited says an apply wait was ATTEMPTED, which is what tells the
	// two pendings apart.
	//
	// NOT "the wait succeeded" — that is derivable and is `applied`. This
	// says whether anybody asked, and it exists because [Request.NoWait]
	// made the question askable: a pending that was waited for says this
	// NODE is behind, and a pending that was not says the caller declined
	// to find out. Reported as one, a deliberate skip reads to an
	// operator as a node falling behind under load, which is a health
	// signal they will go and act on.
	//
	// FALSE ON EVERY OUTCOME THAT DID NOT REACH THE WAIT — a refusal, a
	// round that lost its race — which is honest rather than absent
	// information: no wait happened there either.
	Waited bool

	// Version is the object's new version on an applied write: the
	// position packed, which is the same number the row carries.
	Version int64

	// Rounds is how many compare-and-set rounds this write took, for the
	// instrument that says whether an object is contended.
	Rounds int
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

// Retryable reports whether this refusal clears on THIS node without anybody
// doing anything — the write-side twin of [ReadRefusal.Retryable], and for its
// reason: a node that is behind catches up, while an evicted node, a deleted
// object or a full log refuses the same write however long the caller waits.
//
// THREE CLEAR ON THEIR OWN, each by its own constant's words: a node BEHIND the
// caller's previous write applies it, a node BELOW the trim floor catches up
// or adopts a snapshot, and a floor that could not be read is read again.
// [ReasonDeferred] is deliberately not one of them: ANOTHER node can serve the
// write, and this one cannot until it is upgraded — so "come back here" is the
// wrong instruction however soon it is given.
func (r Reason) Retryable() bool {
	switch r {
	case ReasonBehind, ReasonBelowFloor, ReasonFloorUnknown:
		return true
	}
	return false
}

// RetryAfter is how long a caller should wait before bringing the request err
// refused back to THIS node: ZERO when waiting cannot change the answer, the
// refusal's own derived hint where it has one, and otherwise when it is
// retryable and derived nothing — or is not a refusal this package made at
// all, which is the caller's to judge rather than this package's.
//
// ONE RULE FOR EVERY SURFACE that answers a refusal in a status code. Written
// at each one, it drifted: /chart turned a refusal waiting cannot clear — a
// node holding a record it cannot decode, an evicted one — into `Retry-After:
// 2`, and the work surface did the same to every write refusal, so a client
// polled every two seconds a node that would not answer until somebody
// upgraded or readmitted it. And the zero is the only honest hint there is:
// [Refused.RetryAfter] already says so for a read, and [Reason.Retryable]
// says it for a write, which carries no hint of its own.
func RetryAfter(err error, otherwise time.Duration) time.Duration {
	var refused *Refused
	if errors.As(err, &refused) {
		switch {
		case !refused.Code.Retryable():
			return 0
		case refused.RetryAfter > 0:
			return refused.RetryAfter
		}
		return otherwise
	}
	var unavailable *Unavailable
	if errors.As(err, &unavailable) && !unavailable.Reason.Retryable() {
		return 0
	}
	return otherwise
}

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
