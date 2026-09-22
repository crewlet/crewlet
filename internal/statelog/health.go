package statelog

import (
	"fmt"
	"time"
)

// FloorState is what this node knows about the published trim floor, and
// there are THREE answers rather than two.
//
// A boolean here hid the one that matters. "This node is at or above the
// floor", "this node is below it" and "the floor could not be read" lead to
// different code, and collapsing the third into the first keeps a node serving
// reads over a hole it cannot see. The rule is the same one the trim itself
// uses: a term that cannot be read blocks.
type FloorState int

const (
	// FloorOK is at or above the published floor: every record this node
	// has not applied is still on the log.
	FloorOK FloorState = iota

	// FloorBelow is below it: records this node never applied have been
	// trimmed, so its rows are missing state no later replay can supply.
	// Reads and writes both refuse, and the node adopts a snapshot.
	FloorBelow

	// FloorUnknown is the third value, and it takes the SAME branch as
	// FloorBelow rather than the optimistic one. A floor that cannot be
	// read is not a floor that is satisfied, and the cost of guessing
	// wrong is a node serving answers with a hole in them.
	FloorUnknown
)

// String names a floor state for an operator surface.
func (f FloorState) String() string {
	switch f {
	case FloorOK:
		return "ok"
	case FloorBelow:
		return "below"
	case FloorUnknown:
		return "unknown"
	}
	return fmt.Sprintf("FloorState(%d)", int(f))
}

// Floor is what this node knows about the published trim floor AND when it
// last knew it.
//
// ONE FIELD RATHER THAN TWO, because a state without its instant ages
// silently into an assertion: a cached "at or above the floor" from four
// heartbeats ago is not a floor that is satisfied, it is a coordination path
// that has stopped answering — and the two are indistinguishable to anything
// holding only the state.
type Floor struct {
	// State is what the last successful read said.
	State FloorState

	// ReadAt is when that read happened. The zero value means never,
	// which is UNKNOWN rather than optimistic.
	ReadAt time.Time
}

// Effective is the floor state as of now, which is [Floor.State] until the
// read behind it is too old to count as a read at all.
//
// FOUR HEARTBEATS, which is the same derivation this engine already uses for
// how long a node's own status stays credible: a node may miss three beats and
// still be healthy, so a value older than four is not a slow read.
func (f Floor) Effective(now time.Time) FloorState {
	if f.ReadAt.IsZero() || now.Sub(f.ReadAt) > FloorCacheStale {
		return FloorUnknown
	}
	return f.State
}

// Serves reports whether this floor permits serving as of now.
func (f Floor) Serves(now time.Time) bool { return f.Effective(now) == FloorOK }

// Age is how long ago the floor was read, for the refusal that names it.
func (f Floor) Age(now time.Time) time.Duration {
	if f.ReadAt.IsZero() {
		return 0
	}
	return now.Sub(f.ReadAt)
}

// Replayable reports whether a node whose checkpoint is `checkpoint` can still
// replay forward on a log whose lowest record is `first` — whether the next
// record it needs, checkpoint+1, is still there. False is the state
// [FloorBelow] names.
//
// ONE PREDICATE, because the question is asked in more places than any one of
// them can see: [Health.Established], the health read that stamps FloorBelow,
// the boot's join, the re-check after a transfer, a running node's heartbeat,
// a backup refusing a copy the log can no longer reach, and both write fences
// that clear an expectation of zero. Written out per site it was one
// inequality in two spellings, `seq+1 < floor` and `first > seq+1`, at eight
// sites that could each drift alone — and the tracker's fence records what one
// step of drift costs: compared against the checkpoint itself, it refused a
// node ONE BELOW `first`, which has already applied everything that may be
// gone.
//
// # Why the boundary is checkpoint+1
//
// A checkpoint is the last sequence APPLIED, so what a node needs next is
// checkpoint+1. A checkpoint ONE BELOW `first` has applied everything that was
// ever removed: every record under `first` is gone and it holds all of them.
// So (first-1, first) is replayable, and first-2 is the highest checkpoint
// that is short a record.
//
// # What `first` is
//
// The lowest sequence the caller trusts the log to hold, and choosing it is
// the caller's decision rather than this function's. The stream's own first
// sequence says what IS there; the published trim floor says what the trim has
// not licensed removing; a caller that must survive the next trim asks of the
// higher of the two. [OfferRequest.Need] is this same boundary shipped to a
// donor as a number — first-1, the lowest checkpoint this accepts — because a
// donor holds no view of the joiner's stream.
//
// # The zero cases, as measured on JetStream
//
//   - A stream nobody has written reports first = 0, NOT 1, and every
//     checkpoint is replayable against it: there is nothing to have missed.
//   - A written stream reports first >= 1. A node that has applied nothing
//     has checkpoint 0, so (0, 1) is replayable — the whole log is ahead of
//     it — and (0, 2) is not: record 1 is gone and it never saw it.
//   - A stream EMPTIED by a purge reports first = last+1, one past a record
//     it no longer holds. A node that applied through `last` is exactly one
//     below that and replayable; one that stopped short of `last` is not,
//     because what it never applied went with the purge.
//   - A published floor of 0 is a trim that has licensed removing nothing,
//     and it reads exactly as the never-written stream does.
//
// # And why it is not written `first > checkpoint+1`
//
// Because that form overflows: at a checkpoint of the top of uint64 the sum
// wraps to 0, and every log with a record in it reads as having left behind a
// node that has applied every sequence there is. `first-1 <= checkpoint`
// behind a guard on zero forms no sum, so it is exact over the whole range.
// The two forms differ only at a checkpoint of exactly that top value, which
// no valid position reaches — [Position.Valid] stops at [MaxSeq] — so no
// reachable answer changed; but a boundary stated once for every caller
// should be exact for every value a caller can pass.
func Replayable(checkpoint, first uint64) bool {
	return first == 0 || first-1 <= checkpoint
}

// ApplyRetryBudget is how long a transient apply error is retried in place
// before the applier reports itself faulted — which its readers treat as
// stalled: reads refuse naming the error, and the seats move.
//
// HALF THE STALL GRACE, deliberately: a retry that outlasted the grace would
// let a node report itself healthy while it made no progress, and one much
// shorter would turn an ordinary transaction conflict into a fleet event. The
// retry itself never gives up — see [Runner.Fault]; this is the point at which
// it stops being quiet about it.
const ApplyRetryBudget = StallGrace / 2

// Health is one registered domain's readiness.
//
// # FIFTEEN FIELDS, DECLARED ONCE
//
// This struct is cited from the framework's contracts, from the readiness
// gate, from the operator surface and from the register's own heartbeat, and
// it is written out in exactly one place. The reason is mechanical rather than
// aesthetic: written out per reader it becomes three lists that disagree —
// and the fields most likely to be dropped are the nilable ones, which are
// precisely the ones carrying "this is unknown" rather than "this is zero".
//
// [Health.AppliedThrough] is a RAW sequence within [Health.Position]'s own
// generation, where Position is composed. That is why the two have different
// shapes, and why every barrier compares the first against a read index taken
// from the same generation.
type Health struct {
	// Position is (stream, generation, seq) committed in THIS node's own
	// transaction — not a consumer's acknowledgement floor, which moves
	// for reasons that have nothing to do with what this node holds.
	Position Position

	// AppliedThrough is the prefix whose DERIVED CONSEQUENCES this node
	// holds: the lowest deferred sequence minus one, or Position.Seq when
	// it holds none.
	//
	// THE NODE-WIDE VALUE, for the register and for an operator's screen.
	// A read's own coverage is a separate probe over the objects that
	// read is about, decided before any barrier is appended and never
	// waited on — a wait on this number could not terminate, because a
	// deferred scope pins it below the checkpoint while a barrier is
	// appended above it.
	AppliedThrough uint64

	// Deferred is how many records this node holds and cannot decode.
	Deferred uint64

	// DeferredFrom is the lowest of their sequences, which is where a
	// build that can read them would resume.
	DeferredFrom uint64

	// Drained reports having applied this domain's log to nothing pending
	// at least once, and not having stalled since.
	//
	// A HISTORY, NOT AN INSTANT, and the distinction is the whole reason
	// the field exists beside [Health.Lag]. Whether this node is behind
	// RIGHT NOW is the lag; whether its rows have ever been a whole state
	// is this. The two were one bool once, assigned `lag == 0` on every
	// heartbeat, and the readers that wanted the history got the instant:
	// on a single node every write to the tracker put one record on the
	// log the applier had not yet consumed, so the bool went false for one
	// heartbeat and [Health.Healthy] read the node as a WRONG copy — seven
	// seats released, reclaimed five seconds later, six times in eight
	// minutes, on a company doing nothing but filing work items.
	//
	// IT IS THE APPLIER'S OWN OBSERVATION rather than a sampled lag, for
	// the reason a sample cannot answer a question about a series: a
	// heartbeat that looks every ten seconds can miss every moment a busy
	// log is empty, and a latch that never latches is a refusal nothing
	// clears. The applier sees each one — the fetch the broker answered
	// with nothing is the drain — and [Runner.Drained] is that record.
	//
	// ONE READER, AND IT IS THE DONOR GATE: may this node hand a peer a
	// copy of its rows. That question is about whether the rows were ever
	// whole, and the separate question of how far they have since fallen
	// behind is [Health.Lag] against [SnapshotLagSlack]. Neither readiness
	// gate reads this — admission wants the instant and the shed wants a
	// fault, and neither is a history.
	Drained bool

	// Stalled reports an applied prefix that has not moved for the stall
	// grace. A first-class state rather than a symptom: a stalled node's
	// rows are frozen, so every expectation it forms is stale and every
	// write it makes burns its whole round budget to a conflict a model
	// reads as a colleague editing the same object.
	Stalled bool

	// Floor is the three-valued floor state and when it was last read.
	// ONE FIELD, because a state whose age nobody carries ages silently
	// into an assertion.
	Floor Floor

	// Evicted reports an eviction tombstone for this node.
	//
	// COORDINATION-DERIVED: it is this process's copy of the cached
	// tombstone, refreshed on the same loop, so it is NOT an independent
	// input. That is exactly why the write path's fence checks a third
	// source — this node's own applied eviction rows, fresh to its
	// applied prefix and the only one still fresh when coordination is
	// wedged, which is a precondition of an eviction being permitted.
	Evicted bool

	// Lag is the stream's last sequence minus this node's position, and
	// NIL when the broker could not be reached.
	//
	// A POINTER because a zero-because-unknown lag serves a bounded
	// stale read without any bound at all: the read asks "how far behind
	// may this answer be", and an unreadable broker answers "not at all".
	Lag *uint64

	// FirstSeq is the stream's first surviving sequence as last read, and
	// NIL when it could not be.
	//
	// ADVISORY, and it may only RAISE the floor. It arrives on the same
	// stream info from the same possibly-non-authoritative member, and a
	// stale one is LOWER than the truth — so a maximum that trusts it
	// under-fires and a node below the real floor keeps serving.
	FirstSeq *uint64

	// TrimFloor is the register's published floor for this domain as last
	// read, and NIL when it could not be. AUTHORITATIVE where FirstSeq is
	// advisory.
	TrimFloor *uint64

	// LastSeq is the stream's own last sequence as last read, and NIL when
	// it could not be.
	//
	// ITS OWN FIELD BESIDE Lag, because Lag is clamped at zero: a
	// checkpoint PAST the log's end reads as caught up through it, and
	// that is the one state the number exists to refuse. A checkpoint
	// above the end is a position on a stream that is not this one — a
	// recreated stream, or a broker restored from an older copy — and a
	// consumer created there waits for a sequence that never arrives while
	// reporting nothing pending.
	LastSeq *uint64

	// StreamRecreated is a live stream that is not the one this node's applier
	// started against — a delete and a rebuild under the same name.
	//
	// # Why it is observed rather than derived
	//
	// Because nothing this node holds can show it. The generation does not move
	// (a rebuilt stream comes back at 0), the sequences count from 1 again, and
	// once the new stream has published past this node's checkpoint even
	// [Health.AheadOfLog] goes quiet — the checkpoint is no longer past an end
	// that has caught up with it. What is left is a node applying a DIFFERENT
	// history into rows keyed by the old one, reporting itself caught up.
	//
	// The only thing that separates the two streams is the broker's own creation
	// instant, and the only place that sees the live one while a node runs is the
	// position heartbeat, which reads the stream's state every ten seconds anyway.
	// So the heartbeat compares and sets this, and the refusal it produces is the
	// same one the boot's own identity check produces — with the same remedy, an
	// operator's re-anchor.
	StreamRecreated bool

	// Coverage is COMPACTED domains only: the fraction of rows present
	// against rows expected. A gap here is the compaction policy working
	// rather than a fault, which is why it is a number and not a bool.
	Coverage float64

	// Err is the stuck sequence and its error, when this domain is
	// stalled or has stopped.
	Err string
}

// DeferredSince is how long this node has been holding records it cannot
// decode, given the instant the oldest arrived.
type DeferredSince struct {
	Since time.Time
	Held  bool
}

// Serving reports whether this domain may answer a read at all.
//
// The order is the cheap local refusals first, so a doomed read never reaches
// the broker: eviction, then the floor, then a stall.
func (h Health) Serving(now time.Time) bool {
	return h.Refusal(now) == ""
}

// Refusal is why this domain is not serving as of now, or empty when it is.
func (h Health) Refusal(now time.Time) ReadRefusal {
	switch {
	case h.Evicted:
		return RefuseEvicted
	case h.Floor.Effective(now) == FloorUnknown:
		return RefuseFloorUnknown
	case h.Floor.Effective(now) == FloorBelow:
		return RefuseBelowFloor
	case h.AheadOfLog() || h.StreamRecreated:
		return RefuseWrongStream
	case h.Err != "" || h.Stalled:
		return RefuseStalled
	}
	return ""
}

// AheadOfLog reports a checkpoint past the stream's own last sequence.
//
// A position the log has never reached is a position on another stream: the
// stream was recreated, or the broker was restored from a copy older than
// this node's rows. Nothing this node holds above the end can be reconciled
// with what the log will now produce, so it is a refusal on every path rather
// than a lag of zero — which is exactly what a clamped lag would report.
func (h Health) AheadOfLog() bool {
	return h.LastSeq != nil && h.Position.Seq > *h.LastSeq
}

// Healthy reports whether this domain's state permits KEEPING the seats this
// node already holds.
//
// # It answers "is this copy WRONG", never "is this copy BEHIND"
//
// That is the whole line between it and [Health.Established], which gates
// admission. A copy that is behind catches up on its own, so withholding
// claims is the entire remedy and dropping work in hand would be pure loss; a
// copy that is wrong cannot catch up, so the work has to move. Every term
// below is of the second kind.
//
// # What does NOT make it false, and why
//
// BEING BEHIND DOES NOT — not by a record, not by ten thousand, and not
// because this node has yet to drain the log a first time. The reading that
// conflated the two was `lag == 0` recomputed per heartbeat: on a single node
// every tracker write put one record on the log the applier had not consumed
// yet, so this went false for one heartbeat and the sweep released all seven
// of the company's seats, reclaiming them about five seconds later — six
// times in eight minutes, with a real model that is every turn on the node
// interrupted by somebody filing a work item. A prefix that stops MOVING is a
// different fact and it is [Health.Stalled], which is below.
//
// Holding records this build cannot decode does not either, on its own.
// Folding that into "stalled" sheds a company's seats fleet-wide on a routine
// rolling upgrade — one upgraded writer taking every un-upgraded node out of
// service at once, which is exactly the outage the retain rule exists to
// prevent, arriving through the applier instead of the codec.
//
// # And what does
//
// Holding them PAST THE DEFERRAL GRACE. At that point the honest reading is
// "this node cannot run this company's records" rather than "this node is
// briefly behind", and a node that fails every call about a growing set of
// objects while keeping its seats is the same outage in a slower form. Under
// the grace nothing changes, which covers every rolling upgrade this design
// describes — a config apply and a seat re-placement are one heartbeat each.
//
// A compacted domain's coverage does not make it false either: a derived row's
// gaps are the compaction policy working.
func (h Health) Healthy(now time.Time, deferredSince DeferredSince) bool {
	if h.Err != "" || h.Evicted || !h.Floor.Serves(now) ||
		h.AheadOfLog() || h.StreamRecreated {
		return false
	}
	// A FROZEN PREFIX, which is the one term here that a lag resembles and
	// is not: a node that owes progress and has made none for the stall
	// grace answers every expectation out of rows that stopped, where a
	// node that is merely behind is applying its way out of it.
	if h.Stalled {
		return false
	}
	if h.Deferred > 0 && deferredSince.Held && now.Sub(deferredSince.Since) > DeferralGrace {
		return false
	}
	return true
}

// Established is the two-sided inequality plus a drain, and it is
// RE-EVALUATED ON EVERY HEARTBEAT rather than only at boot.
//
// The continuity is load-bearing three ways, and each one is a state a node
// reaches without restarting:
//
//   - A live consumer whose position falls below the stream's first sequence
//     is CLAMPED UPWARD with no error at all, and then reports itself caught
//     up over a hole.
//   - An evicted node keeps running while the trim advances past it, so the
//     counted-set term the floor rests on cannot cover it.
//   - A merely slow node can cross the floor mid-flight on a busy fleet.
//
// ABOVE THE STREAM'S LAST SEQUENCE IS A REFUSAL, not a clamp. With a non-zero
// start sequence the server does not clamp downward, so a consumer created
// there waits for a sequence that never arrives, reports nothing pending, and
// looks perfectly caught up while applying nothing, for ever. It is decided
// from [Health.LastSeq] rather than from Lag, because Lag is clamped at zero
// and reads that state as caught up.
//
// STRICT IS THE INSTANT, NOT THE HISTORY. It asks whether this node is behind
// RIGHT NOW, because it gates a seat about to attach and act on these rows —
// a node inside the trim floor still serves rows that are behind, and a seat
// reading one answers "there is no such item" about work it was just handed.
// [Health.Drained] is the opposite question and belongs to the opposite gate:
// what a node may KEEP is not decided on a lag (see [Health.Healthy]).
func (h Health) Established(strict bool) (bool, ReadRefusal) {
	// THE AUTHORITATIVE TERM IS THE PUBLISHED FLOOR. The stream's own
	// first sequence may only raise it.
	if h.TrimFloor == nil {
		return false, RefuseFloorUnknown
	}
	floor := *h.TrimFloor
	if h.FirstSeq != nil && *h.FirstSeq > floor {
		floor = *h.FirstSeq
	}
	if !Replayable(h.Position.Seq, floor) {
		return false, RefuseBelowFloor
	}
	if h.Lag == nil || h.LastSeq == nil {
		// The stream's own end could not be read, so the upper half of
		// the inequality cannot be evaluated — and it is the half whose
		// failure is silent.
		return false, RefuseBrokerUnreachable
	}
	if h.AheadOfLog() || h.StreamRecreated {
		return false, RefuseWrongStream
	}
	// THE LAG ITSELF, and it is non-nil by the check above: a node with
	// records left to apply holds a PREFIX of what the log says, whether
	// or not it has ever held the whole of it.
	if strict && *h.Lag > 0 {
		return false, RefuseBehind
	}
	return true, ""
}
