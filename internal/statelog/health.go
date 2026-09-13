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

// ApplyRetryBudget is how long a transient apply error is retried in place
// before the applier reports itself stalled.
//
// HALF THE STALL GRACE, deliberately: a retry that outlasted the grace would
// let a node report itself healthy while it made no progress, and one much
// shorter would turn an ordinary transaction conflict into a fleet event.
const ApplyRetryBudget = StallGrace / 2

// Health is one registered domain's readiness.
//
// # THIRTEEN FIELDS, DECLARED ONCE
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

	// CaughtUp reports having drained to nothing pending at least once,
	// and not being stalled since.
	CaughtUp bool

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
	case h.Err != "" || h.Stalled:
		return RefuseStalled
	}
	return ""
}

// Healthy reports whether this domain's state permits admitting seats.
//
// # What does NOT make it false, and why
//
// Holding records this build cannot decode does not, on its own. Folding that
// into "stalled" sheds a company's seats fleet-wide on a routine rolling
// upgrade — one upgraded writer taking every un-upgraded node out of service
// at once, which is exactly the outage the retain rule exists to prevent,
// arriving through the applier instead of the codec.
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
	if h.Err != "" || h.Evicted || !h.Floor.Serves(now) {
		return false
	}
	if !h.CaughtUp || h.Stalled {
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
// looks perfectly caught up while applying nothing, for ever.
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
	if h.Position.Seq+1 < floor {
		return false, RefuseBelowFloor
	}
	if h.Lag == nil {
		// The stream's own end could not be read, so the upper half of
		// the inequality cannot be evaluated — and it is the half whose
		// failure is silent.
		return false, RefuseBrokerUnreachable
	}
	if strict && !h.CaughtUp {
		return false, RefuseBehind
	}
	return true, ""
}
