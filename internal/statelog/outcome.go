package statelog

import (
	"errors"
	"fmt"
	"slices"
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
	// never stored: Position is the earlier copy's, found in this node's
	// ledger before a decision was taken ([Snap.Held]), in the broker's
	// duplicate acknowledgement, or in the ledger when the publish is
	// resolved — wherever the row the ledger names cannot be proven to be
	// this call's own record. Only an acknowledged append whose position
	// the ledger names is; an ambiguous publish answered from the ledger
	// is collapsed even when the record happens to be this call's, since
	// nothing here can tell the two apart and building on another copy's
	// answer is the failure this field exists to prevent.
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

	// ReasonOvertaken — the record was written after a RESTORED reanchor's
	// generation record, in a generation below the one that reanchor opened:
	// by a node the move had overtaken before it learned of it, from the rows
	// the reanchor did not keep — the copy's age. It produces rows nowhere the
	// reanchor's checkpoint is followed from — see [ReanchorPlan.StaleAfter].
	ReasonOvertaken Reason = "overtaken"

	// ReasonLogFull — the log is at its byte ceiling and refuses
	// appends rather than dropping records. An operator raises the
	// ceiling or unblocks the trim. On a log that keeps a gate reserve
	// an ordinary write meets this at the ceiling less the reserve
	// ([OrdinaryCeiling]), and a gate record only at the broker's own.
	ReasonLogFull Reason = "log_full"

	// ReasonSkew — the broker answered a last sequence BELOW an
	// expectation this node formed, which cannot happen on a healthy
	// stream and means a store or stream was restored out of step.
	ReasonSkew Reason = "skew"

	// ReasonOpReused — the write's operation id already names a record on
	// another object in this node's ledger: an operation id names one
	// write, and the caller sent it with a different one. Nothing was
	// written for this write, and a fresh id is what it needs.
	ReasonOpReused Reason = "op_reused"

	// ReasonLogTruncated — the log lost records a peer's rows hold
	// ([ErrLogTruncated]): a broker restored from an older copy, and a peer
	// whose rows are newer than the copy. This node's own rows are the log's
	// history, so only its WRITES refuse, and they do until the operator
	// settles which history the fleet keeps — a write made before that is one
	// the restored reanchor of the peer would apply nowhere.
	ReasonLogTruncated Reason = "log_truncated"

	// ReasonWrongStream — the log under this domain's name is not the
	// one this node's rows were derived from. Each finding has its own
	// cause a caller can recognise without switching on the reason: the
	// log was deleted and rebuilt, so it counts from 1 again at the same
	// generation ([ErrStreamRecreated]); this node's checkpoint is past the
	// log's end ([ErrAheadOfLog]) — the broker was restored from a copy
	// older than this node's rows, which keeps the stream's creation
	// instant, so only the end shows it; the log holds, at that checkpoint,
	// another record than the one this node consumed there
	// ([ErrLogDiverged]) — the same restore, written past this node's rows,
	// where the end shows nothing and the record does; or a peer
	// re-anchored the log past this node's generation
	// ([ErrGenerationPassed]). Every expectation this node could form is a
	// sequence from a history the broker does not hold, and whatever it
	// appends lands where its own applier will never apply it. Waiting
	// does not clear it, and the remedy is an operator's re-anchor — or,
	// for a passed generation, whichever the refusal names: this node's own
	// adoption where a live peer holds the generation, a re-anchor where
	// only evicted nodes do, and a re-run of this node's own reanchor where
	// that is what opened it. The same word as the read refusal for the
	// same facts, so a surface that meets both reads one.
	ReasonWrongStream Reason = "wrong_stream"

	// ReasonSuperseded — this operation's record landed, and a later record
	// on the same object has undone or replaced it since, so a retry of the
	// operation is not a new write of it and cannot be answered as though
	// its record were still the one in force. A caller that still wants the
	// effect starts a NEW operation, under a fresh id. Produced by a write's
	// [Request.Standing] — the node gate's ([GateStanding]): an eviction
	// retried under its id after a readmission has taken the node back.
	ReasonSuperseded Reason = "superseded"
)

// Reasons is every [Reason] this build names, in declaration order.
//
// THE ENUMERATION IS WHAT A SURFACE IS HELD TO. A refusal's reason is a label
// on the refusal counter and a field in every write route's answer, and each
// value has its own remedy — so a reference that lists some of them reads as
// complete and is not, and the operator writing a collector rule from it has
// no row for the value that fires. The metrics catalogue's refusal instrument
// is checked against this list ([TestEveryRefusalReasonIsInTheMetricsReference]).
//
// A RECORD A GATE DROPPED is refused under the gate that dropped it — evicted,
// deleted, retired, abandoned or overtaken — rather than under a generic word,
// because the gate is what says why: which is why there is no `gated` here.
func Reasons() []Reason {
	return []Reason{
		ReasonEvicted, ReasonDeferred, ReasonBehind, ReasonBelowFloor,
		ReasonFloorUnknown, ReasonDeleted, ReasonRetired, ReasonAbandoned,
		ReasonOvertaken, ReasonLogFull, ReasonSkew, ReasonOpReused,
		ReasonLogTruncated, ReasonWrongStream, ReasonSuperseded,
	}
}

// Valid reports whether r is a reason this build names — an unknown one off
// the wire is a value to show rather than one to switch on.
func (r Reason) Valid() bool { return slices.Contains(Reasons(), r) }

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
