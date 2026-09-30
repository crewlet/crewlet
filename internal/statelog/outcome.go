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

	// Unvouched says an `unknown` was answered because THIS NODE'S
	// operation ledger cannot vouch for the operation — it was minted
	// before the instant the ledger may have lost rows from (its sweep, or
	// a snapshot adopted from a donor that scrubbed it) and the ledger holds
	// no row for it — rather than because an acknowledgement was lost.
	//
	// # Why a caller has to be able to tell the two apart
	//
	// They send a retry opposite ways. A lost acknowledgement is resolved by
	// the same operation id retried HERE: the ledger writes its row when the
	// record applies, and the next attempt answers from it. An unvouched
	// operation meets the same silence on this node every time — the row it
	// needs is the one the loss took — so the same call repeated here never
	// finishes it, and the first run may well have landed. What can answer
	// it is another node, or one whose ledger lost nothing that far back.
	// Told only "unknown", a walking gesture said "call again" for ever and
	// a create said "not made" about a task its first run may have filed.
	//
	// Always false beside any outcome but unknown.
	Unvouched bool
}

// Reason says why a write was refused, and each value names a different thing
// for the caller to do.
type Reason string

const (
	// ReasonEvicted — the record's writer has been removed from the fleet:
	// this node, or — when [Unavailable.CopyWriter] names one — the other
	// node whose copy of this operation this node's append was collapsed
	// onto. Nothing an evicted node publishes is applied anywhere.
	//
	// THIS NODE'S OWN, there is no retry HERE: another node the fleet still
	// counts takes the write. A refusal that names no position was made
	// before anything was appended, and that node takes the write under the
	// same operation id. One that names a position is a record of this
	// node's that landed and applies nowhere, and it holds the operation id
	// for the log's duplicate window ([StreamSpec.Duplicates]): the same id
	// sent inside it — by any node — is collapsed onto that record and
	// refused the same way, so the other node takes the write under a fresh
	// id, or under this one once the window has passed. Neither can apply
	// twice, because the record in the way applies nowhere.
	//
	// ANOTHER NODE'S COPY says nothing about this node, which passed its own
	// fences before it appended: it takes the write itself, under the same
	// operation id once the window has passed, or under a fresh one sooner.
	ReasonEvicted Reason = "evicted"

	// ReasonReleased — the record's writer RELEASED this log before the
	// record's position and had not been readmitted: it left the log's
	// partition, and its release is its own statement that nothing it
	// publishes on the log afterwards applies anywhere ([EvictionKindRelease]).
	// A record that landed after it — a write that was in flight while the
	// node left — is dropped on every holder, which is what lets the node
	// leave without a coordination round trip anybody has to trust.
	//
	// ALWAYS A RECORD THAT LANDED, and it holds its operation id for the
	// log's duplicate window: the same id sent inside it — by any node,
	// one that serves the partition included — is collapsed onto the
	// record and refused `released` again. A node that serves the
	// partition takes the write under a fresh id, or under this one once
	// the window has passed; neither can apply twice, because the record
	// in the way applies nowhere. The node that refused is one of those
	// when [Unavailable.CopyWriter] names the record's writer: the release
	// was that node's, and this one's append was collapsed onto its copy.
	ReasonReleased Reason = "released"

	// ReasonNotHolder — this node does not serve the partition of the log the
	// write would go to ([Holding], [ErrNotHolder]): it never held the
	// partition, or it has begun to leave it. Only a partition's serving
	// holders write its logs, because they are the writers the trim counts
	// there. Another node that serves the partition takes the write, under
	// the same operation id: nothing was appended under it here.
	ReasonNotHolder Reason = "not_holder"

	// ReasonHoldingUnknown — whether this node serves the log's partition
	// could not be established ([Holding] answered an error). The third
	// value, and it BLOCKS, for [ReasonFloorUnknown]'s reason: a write
	// decided by a node the trim may not be counting is a lost update, which
	// nothing recovers. It clears when the node can tell again, and a node
	// that serves the partition can take the write meanwhile.
	ReasonHoldingUnknown Reason = "holding_unknown"

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

	// ReasonWrongPartition — the record's own domain places it in another
	// partition than the log it is on ([Domain.PartitionOf]), so it is
	// dropped on every holder of this log: applied here it would write rows
	// this partition does not own, filed under a scope no probe of the
	// partition it belongs to ever reads. Deterministic — every holder asks
	// the same function of the same envelope — so no copy diverges. Only an
	// applier's: the write authority refuses such a record before it is
	// appended ([ErrWrongPartition]), so one on a log is another build's or
	// another writer's, and nothing republishing it changes where it belongs.
	ReasonWrongPartition Reason = "wrong_partition"

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
// released, deleted, retired, abandoned, overtaken or wrong_partition — rather
// than under a generic word, because the gate is what says why: which is why
// there is no `gated` here.
func Reasons() []Reason {
	return []Reason{
		ReasonEvicted, ReasonReleased, ReasonNotHolder, ReasonHoldingUnknown,
		ReasonDeferred, ReasonBehind, ReasonBelowFloor, ReasonFloorUnknown,
		ReasonDeleted, ReasonRetired, ReasonAbandoned, ReasonOvertaken,
		ReasonWrongPartition, ReasonLogFull, ReasonSkew, ReasonOpReused,
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

	// CopyWriter names ANOTHER node when the record at Position is that
	// node's copy of this operation rather than this node's own, and the
	// gate that dropped it — an eviction, a release, a reanchor's rule, the
	// partition — is about that node's record: the broker collapsed this
	// node's append onto the copy inside the log's duplicate window, or a
	// write whose answer was lost found it newest on its subject. The
	// reason then states THE COPY'S WRITER's standing, and not the standing
	// of the node that refused — which passed its own fences and serves the
	// log's partition, and takes the write itself under the same operation
	// id once the window has let go of it.
	//
	// A FIELD rather than a sentence in Detail, because the remedy turns on
	// it: read as this node's own `evicted`, the operator was told the node
	// they ran the gesture on was evicted and sent elsewhere, although that
	// node is counted and could finish it a minute later.
	//
	// EMPTY WHEN THE REFUSAL IS ABOUT THE NODE THAT MADE IT — its own
	// record, a copy it wrote itself on an earlier attempt, or no record at
	// all — which is every refusal but that one, and what a refusal from a
	// build before this field reads as.
	CopyWriter string

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
