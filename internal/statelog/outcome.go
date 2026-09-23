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

	// Collapsed says this call was a RETRY of an operation that had
	// already landed under the same op id, and THIS call's decision was
	// never stored: Position is the earlier copy's, found either in this
	// node's ledger before a decision was taken ([Snap.Held]) or in the
	// broker's duplicate acknowledgement.
	//
	// The outcome is still the operation's own — it did apply — and a
	// caller that only reports it needs nothing more. A caller whose
	// ANSWER is computed inside its decision must not build on that
	// answer: it describes a decision nothing published, or — where the
	// ledger answered — one that was never taken at all. A counter a
	// create minted from is the sharp case: the number the earlier copy
	// took is on the log, and the one this call would have taken is not.
	Collapsed bool
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

	// ReasonBehind — a snapshot here would decide from a state below a
	// position the write needs: the caller's own previous write, a peer's
	// write the broker has already accepted, or — at an expectation of
	// zero ([ZeroFence]) — the published trim floor, while the log still
	// holds every record this node lacks and it is replaying them, the
	// state [FloorReplaying] names on the read path. Position is the
	// position the write needs this node to have applied through. It
	// clears on its own.
	ReasonBehind Reason = "behind"

	// ReasonBelowFloor — the record this node's applier needs next is gone
	// from the log, so a retry-at-zero here could overwrite a peer's
	// committed write and no replay can supply what the node lacks. It
	// clears when the node adopts a peer's snapshot; another node can make
	// the write meanwhile. Produced by [ZeroFence], on the one branch where
	// being below the floor is a lost update rather than a stale read — and
	// only when the node is below the LOG: below the published floor alone
	// it is replaying, and the refusal is [ReasonBehind].
	ReasonBelowFloor Reason = "below_floor"

	// ReasonFloorUnknown — the bound an expectation of zero is cleared
	// against could not be read: the published trim floor, or the log's
	// own ends, which carry its first surviving sequence. Either is half
	// of that bound, and an unknown half is the third value, and it
	// BLOCKS. Failing open here is a lost update, which is not
	// recoverable; failing open where a claim is deduplicated is a
	// duplicate delivery, which is.
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

	// ReasonAbandoned — the record was written in a generation a reanchor
	// ABANDONED: one only a node the fleet has since evicted held, whose
	// history is on no disk the fleet still has. It was decided from rows
	// nobody holds, so it produces rows nowhere the reanchor's checkpoint is
	// followed from — see [ReanchorPlan.From].
	ReasonAbandoned Reason = "abandoned"

	// ReasonLogFull — the log is at its byte ceiling and refuses
	// appends rather than dropping records. An operator raises the
	// ceiling or unblocks the trim.
	ReasonLogFull Reason = "log_full"

	// ReasonSkew — the broker answered a last sequence BELOW an
	// expectation this node formed, which cannot happen on a healthy
	// stream and means a store or stream was restored out of step.
	ReasonSkew Reason = "skew"

	// ReasonWrongStream — the log under this domain's name is not the
	// one this node's rows were derived from. Two findings, each with its
	// own cause a caller can recognise without switching on the reason:
	// the log was deleted and rebuilt, so it counts from 1 again at the
	// same generation ([ErrStreamRecreated]); or this node's checkpoint is
	// past the log's end ([ErrAheadOfLog]) — the broker was restored from a
	// copy older than this node's rows, which keeps the stream's creation
	// instant, so only the end shows it. Either way every expectation this
	// node could form is a sequence from a history the broker does not
	// hold, and whatever it appends lands where its own applier will never
	// apply it. Waiting does not clear it — a restored log written past
	// the checkpoint stops LOOKING wrong without becoming this node's
	// history — and the remedy is an operator's re-anchor. The same word
	// as the read refusal for the same facts, so a surface that meets both
	// reads one.
	ReasonWrongStream Reason = "wrong_stream"
)

// ErrUnavailable is what a refusal wraps, so a caller can tell a refusal from
// a conflict with errors.Is before it looks at the reason.
var ErrUnavailable = errors.New("statelog: unavailable")

// ErrConflict reports a write that lost its race for the whole round budget.
// It is the refusal a model reads as "somebody else is editing this", and it
// is the same refusal the tracker's own writes give today.
//
// NOTHING BUT A LOST RACE MAY RETURN IT. A fence that refuses this node — an
// eviction, a floor it is below, a log it is past the end of — returns an
// [*Unavailable] instead, because the two send a caller opposite ways: a
// conflict says re-read and try again here, and every fence refusal says this
// node cannot make this write however often it re-reads. Both fences once
// refused an evicted node with this, so a model was told a colleague was
// editing an object on a node the fleet had removed.
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

	// Cause is the error a refusal was concluded from, when there is one
	// a caller may want to recognise without switching on the reason —
	// [ErrStreamRecreated] or [ErrAheadOfLog] behind `wrong_stream`, which
	// are the same sentences the applier's stop and a read's refusal
	// carry, and the unreadable read behind `floor_unknown` or an
	// unreadable `evicted`, so a store's or a broker's own sentinel still
	// answers errors.Is through the refusal. Nil for the reasons that are
	// their own whole answer.
	Cause error
}

func (u *Unavailable) Error() string {
	if u.Detail == "" {
		return fmt.Sprintf("statelog: unavailable (%s)", u.Reason)
	}
	return fmt.Sprintf("statelog: unavailable (%s): %s", u.Reason, u.Detail)
}

// Unwrap makes every refusal answer errors.Is(err, ErrUnavailable), and one
// with a cause answer for the cause too.
func (u *Unavailable) Unwrap() []error {
	if u.Cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{ErrUnavailable, u.Cause}
}
