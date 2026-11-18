package statelog

import (
	"fmt"
	"time"
)

// FloorState is what this node knows about the published trim floor: four
// answers, not a bool, because "could not be read" folded into "at or above"
// keeps a node serving reads over a hole it cannot see. An unreadable term
// blocks, as it does for the trim.
//
// "Below" is two states because the floor is published BEFORE the purge it
// licenses, and a failed purge leaves the floor above the log's first record.
// Between the two a node replays what it lacks (come back soon); below the
// log's first record no replay can supply it (go elsewhere while this node
// adopts a snapshot).
type FloorState int

const (
	// FloorOK is at or above the published floor: every record this node
	// has not applied is still on the log, and none of them is one the
	// trim has licensed removing.
	FloorOK FloorState = iota

	// FloorReplaying is below the published floor but not below the log:
	// what this node lacks is still there, so it replays and clears on its
	// own. Reads refuse meanwhile ([RefuseBehind], or [RefuseStalled] once
	// the replay stops moving), and the fence refuses a write at an
	// expectation of zero until it holds the floor. It stays in service:
	// [Health.Healthy] fires on no lag, this one included.
	FloorReplaying

	// FloorBelow is below the log itself: the next record this node needs
	// has been removed, so its rows are missing state no replay can
	// supply. Reads and writes both refuse, the node takes its copy of
	// this domain out of service (its operations go to another data node,
	// and its seats stay — ADR-0079), and it adopts a peer's snapshot.
	FloorBelow

	// FloorUnknown takes FloorBelow's branch, not the optimistic one: an
	// unreadable floor is not a satisfied one.
	FloorUnknown
)

// String names a floor state for an operator surface.
func (f FloorState) String() string {
	switch f {
	case FloorOK:
		return "ok"
	case FloorReplaying:
		return "replaying"
	case FloorBelow:
		return "below"
	case FloorUnknown:
		return "unknown"
	}
	return fmt.Sprintf("FloorState(%d)", int(f))
}

// Floor is the floor state AND when it was last read, as one field: a state
// without its instant ages silently into an assertion, and a cached "at or
// above" from four heartbeats ago is a coordination path that stopped
// answering.
type Floor struct {
	// State is what the last successful read said.
	State FloorState

	// ReadAt is when that read happened. The zero value means never,
	// which is UNKNOWN rather than optimistic.
	ReadAt time.Time
}

// Effective is [Floor.State] until the read behind it is older than
// [FloorCacheStale] (four heartbeats: a node may miss three and still be
// healthy), and [FloorUnknown] after.
func (f Floor) Effective(now time.Time) FloorState {
	if f.ReadAt.IsZero() || now.Sub(f.ReadAt) > FloorCacheStale {
		return FloorUnknown
	}
	return f.State
}

// Age is how long ago the floor was read, for the refusal that names it.
func (f Floor) Age(now time.Time) time.Duration {
	if f.ReadAt.IsZero() {
		return 0
	}
	return now.Sub(f.ReadAt)
}

// Replayable reports whether a node at `checkpoint` (the last sequence
// APPLIED) can still replay a log whose lowest record is `first`: whether
// checkpoint+1 is still there, so one below `first` holds all that was
// removed. False against the stream's first sequence is [FloorBelow]; only
// against a higher published floor, [FloorReplaying]. `first` is the caller's
// choice: the higher of the two to survive the next trim. [OfferRequest.Need]
// ships first-1 to a donor, which cannot see the joiner's stream. JetStream
// reports first = 0 for a never-written stream and last+1 for one a purge
// emptied; a published floor of 0 licenses removing nothing. ONE predicate for
// every site (health, boot join, transfer, heartbeat, backup, both zero
// fences), since per-site spellings drift; `first-1 <= checkpoint` forms no
// sum, so it cannot overflow at the top of uint64.
func Replayable(checkpoint, first uint64) bool {
	return first == 0 || first-1 <= checkpoint
}

// ApplyRetryBudget is how long a transient apply error is retried quietly
// before the applier reports itself faulted, which readers treat as stalled:
// reads refuse naming the error, and the node takes its copy of that domain
// out of service until a retry succeeds. The retry never gives up
// ([Runner.Fault]).
//
// HALF THE STALL GRACE: longer would let a node report healthy while making
// no progress; much shorter turns an ordinary transaction conflict into a
// fleet event.
const ApplyRetryBudget = StallGrace / 2

// Health is one registered domain's readiness.
//
// SEVENTEEN FIELDS, DECLARED ONCE: the framework's contracts, the readiness
// gate, the operator surface and the register's heartbeat all cite this one
// struct, because per-reader copies drift, dropping first the nilable fields
// that carry "unknown" rather than "zero".
// TestHealthCarriesEveryFieldItsContractsCite asserts the count, so a new
// field moves this line and that test together.
//
// [Health.AppliedThrough] is a RAW sequence within [Health.Position]'s own
// generation, so every barrier compares it against a read index taken from
// the same generation.
type Health struct {
	// Position is (stream, generation, seq) THIS node's rows are current
	// through: its reached position ([Runner.Reached]), every record at or
	// below it committed in this node's own transaction or a barrier that
	// writes no row. Not a consumer's ack floor (it moves for reasons
	// unrelated to what this node holds), nor the committed checkpoint (a
	// stalled fsync holds it back over rows already whole).
	Position Position

	// AppliedThrough is the prefix whose DERIVED CONSEQUENCES this node
	// holds: the lowest deferred sequence minus one, or Position.Seq when
	// it holds none. Node-wide, for the register and an operator: a read's
	// own coverage is a separate probe, taken before any barrier is appended
	// and never a wait on this number, which could not terminate — a
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
	// A HISTORY, NOT AN INSTANT (that is [Health.Lag]): read as `lag == 0`
	// per heartbeat, every tracker write on a single node made [Health.Healthy]
	// call the copy WRONG for a beat. It is the applier's own observation
	// ([Runner.Drained]: a fetch the broker answered empty), since a sampled
	// lag can miss every moment a busy log is empty and never latch.
	//
	// Read by the donor gate and [Health.Serving], each also bounding
	// [Health.Lag] by [SnapshotLagSlack]; neither readiness gate reads it,
	// since admission wants the instant and a copy's fault wants a fault.
	Drained bool

	// Stalled reports an applied prefix that has not moved for the stall
	// grace: a first-class state, because a stalled node's rows are frozen,
	// so every expectation it forms is stale and every write burns its whole
	// round budget on a conflict a model reads as a colleague's edit.
	Stalled bool

	// Floor is the four-valued floor state and when it was last read
	// ([Floor]).
	Floor Floor

	// Evicted reports an eviction tombstone for this node: this process's
	// cached copy, refreshed on the coordination loop, so NOT an independent
	// input. The write fence therefore also checks this node's own applied
	// eviction rows, the one source still fresh when coordination is wedged,
	// which an eviction presupposes.
	Evicted bool

	// Lag is the stream's last sequence minus this node's position, and
	// NIL when the broker could not be reached: a zero-because-unknown lag
	// would serve a bounded stale read with no bound at all.
	//
	// As of the last read of the log's ends, which a read's health may serve
	// from memory for a few seconds at most ([ReaderDeps.Health]); ends
	// unrefreshed for longer serve no health at all.
	Lag *uint64

	// FirstSeq is the stream's first surviving sequence as last read, NIL
	// when it could not be. ADVISORY, and it may only RAISE the floor: it
	// comes on the same stream info from a possibly non-authoritative member,
	// and a stale one is LOWER than the truth.
	//
	// Below the published floor it picks [FloorBelow] (`below_floor`, out of
	// service, adopt) or [FloorReplaying] (retryable `behind`), so a stale-low
	// value calls a node replaying records already gone, for seconds only:
	// just a leaderless replica answers stale, every heartbeat, verdict and
	// admission asks afresh, and a read's copy ages no further than
	// [ReaderDeps.Health]. A later heartbeat reading requests the rejoin;
	// meanwhile a strict applier clamped past the hole faults, as `stalled`.
	FirstSeq *uint64

	// TrimFloor is the register's published floor for this domain as last
	// read, and NIL when it could not be. AUTHORITATIVE where FirstSeq is
	// advisory.
	TrimFloor *uint64

	// LastSeq is the stream's own last sequence as last read, and NIL when
	// it could not be. Its own field because Lag is clamped at zero: a
	// checkpoint PAST the end is a position on another stream (recreated, or
	// a broker restored from an older copy), where a consumer waits forever
	// reporting nothing pending — the one state this exists to refuse.
	LastSeq *uint64

	// StreamRecreated is a live stream that is not the one this node's rows
	// are keyed to — a delete and a rebuild under the same name — found at
	// boot against the checkpoint or while running against the applier's start.
	//
	// Observed, because nothing this node holds shows it: the generation does
	// not move, sequences restart at 1, and [Health.AheadOfLog] goes quiet once
	// the new stream passes the checkpoint. Only the creation instant
	// ([StreamCreatedAt]) tells them apart; the boot, every [PositionHeartbeat]
	// and the zero fence land it in [Runner.StreamIdentity], and this is
	// errors.Is [ErrStreamRecreated] on that same answer the write path refuses
	// on, so reads and writes cannot disagree. The checkpoint-past-end half is
	// [Health.AheadOfLog]. Either way the remedy is an operator's re-anchor.
	StreamRecreated bool

	// GenerationPassed is a domain a peer re-anchored past this node's rows
	// ([ErrGenerationPassed]): the log continues from that peer's rows, so
	// these are a history the fleet has left. Observed like the recreation:
	// the heartbeat's positions-register reading, or a record from a
	// generation this node never entered, lands in [Runner.StreamIdentity],
	// and this is errors.Is on that answer. It refuses where a recreation
	// does; the remedy is this node adopting a peer's snapshot.
	GenerationPassed bool

	// LogDiverged is a log whose record at this node's checkpoint is not the
	// one it consumed there ([ErrLogDiverged]): a broker restored from an
	// older copy and written past these rows since. Observed by the applier
	// before it applies past the checkpoint and by every heartbeat, errors.Is
	// the write path's same answer, and refused where a recreation is: the
	// state [Health.AheadOfLog] loses once the log is written back past the
	// checkpoint.
	LogDiverged bool

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

// Age is how long the oldest undecodable record has been held as of now, and
// zero when none is — or when nothing has dated one yet. The one arithmetic for
// [Health.Healthy] (out of service past the grace), the `deferred_old` alarm
// and the gauge, so the three cannot disagree.
func (d DeferredSince) Age(now time.Time) time.Duration {
	if !d.Held || d.Since.IsZero() {
		return 0
	}
	return now.Sub(d.Since)
}

// Refusal is why this domain is not serving as of now, or empty when it is.
//
// A node replaying up to the floor is refused LAST, as [RefuseBehind]: it
// clears on its own only while its applier moves on the stream its rows are
// keyed to, so a stall or a wrong stream is the truer answer.
func (h Health) Refusal(now time.Time) ReadRefusal {
	switch {
	case h.Evicted:
		return RefuseEvicted
	case h.Floor.Effective(now) == FloorUnknown:
		return RefuseFloorUnknown
	case h.Floor.Effective(now) == FloorBelow:
		return RefuseBelowFloor
	case h.AheadOfLog() || h.StreamRecreated || h.GenerationPassed || h.LogDiverged:
		return RefuseWrongStream
	case h.Err != "" || h.Stalled:
		return RefuseStalled
	case h.Floor.Effective(now) == FloorReplaying:
		return RefuseBehind
	}
	return ""
}

// AheadOfLog reports a checkpoint past the stream's own last sequence: a
// position on another stream (recreated, or a broker restored from an older
// copy) that nothing can reconcile, so it refuses on every path rather than
// reading as a clamped lag of zero. The write path refuses on the same fact
// with the same word ([Runner.ObserveEnd], [ZeroFence]).
func (h Health) AheadOfLog() bool {
	return h.LastSeq != nil && pastEnd(h.Position.Seq, *h.LastSeq)
}

// Healthy reports whether this copy of the domain's log is SOUND — one its
// node may keep serving that DOMAIN's operations from. One that is not goes
// out of service for that domain alone (ADR-0079); the node's seats stay.
//
// It answers "is this copy WRONG", never "BEHIND" ([Health.Established] gates
// admission on that): a copy behind catches up, so taking it out of service is
// pure loss. No lag makes it false, nor a first drain not yet reached, nor
// [FloorReplaying]; a prefix that stops moving ([Health.Stalled]) does.
// Undecodable records alone do not (that would take every copy out on a
// rolling upgrade) until held past [DeferralGrace], when the node cannot run
// this company's records. Nor does a compacted domain's coverage: its gaps
// are the compaction policy working. [Health.Fault] lists the terms.
func (h Health) Healthy(now time.Time, deferredSince DeferredSince) bool {
	return h.Fault(now, deferredSince) == ""
}

// Fault is WHY this copy is wrong ([Health.Healthy] false), or empty while it
// is sound: the refusal its reads answer for the same state, in
// [Health.Refusal]'s order, and [RefuseDeferred] for a record held past the
// deferral grace, which no read refuses on by itself. The out-of-service line
// names it, since which fault holds is what an operator acts on. ONE LIST,
// the one [Health.Healthy] reads, so reason and predicate cannot disagree.
func (h Health) Fault(now time.Time, deferredSince DeferredSince) ReadRefusal {
	floor := h.Floor.Effective(now)
	switch {
	case h.Evicted:
		return RefuseEvicted
	case floor == FloorUnknown:
		return RefuseFloorUnknown
	case floor == FloorBelow:
		return RefuseBelowFloor
	case h.AheadOfLog() || h.StreamRecreated || h.GenerationPassed || h.LogDiverged:
		return RefuseWrongStream
	// A FROZEN PREFIX, the one term a lag resembles and is not: a node
	// owing progress that made none for the stall grace answers from rows
	// that stopped. A stopped applier, or one faulted past its budget, is
	// the same report.
	case h.Err != "" || h.Stalled:
		return RefuseStalled
	case h.Deferred > 0 && deferredSince.Age(now) > DeferralGrace:
		return RefuseDeferred
	}
	return ""
}

// Established is the two-sided inequality plus a drain, RE-EVALUATED ON EVERY
// HEARTBEAT, not only at boot: a live consumer below the stream's first
// sequence is CLAMPED UPWARD silently and reports itself caught up over a
// hole, an evicted node keeps running while the trim passes it (the counted
// set cannot cover it), and a slow node can cross the floor mid-flight.
//
// Above the stream's last sequence is a refusal: at a non-zero start sequence
// the server does not clamp downward, so a consumer there waits forever
// looking caught up. Read from [Health.LastSeq], since Lag is clamped at zero.
// strict is the INSTANT: a seat about to attach must not read rows that are
// behind and answer "no such item" about work it was just handed. What a node
// may KEEP is not decided on a lag ([Health.Healthy]).
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
		// BELOW THE FLOOR; the first sequence says which side of the log,
		// since the floor is published before its purge. Unread, it cannot
		// show the log holds what this node lacks.
		//
		// In [Health.Refusal]'s order, the retryable answer LAST: a rebuilt
		// stream keeps the old one's floor (only a reanchor touches it), so
		// a node below it there replays nothing that clears.
		switch {
		case h.FirstSeq == nil:
			return false, RefuseBrokerUnreachable
		case !Replayable(h.Position.Seq, *h.FirstSeq):
			return false, RefuseBelowFloor
		case h.AheadOfLog() || h.StreamRecreated || h.LogDiverged:
			return false, RefuseWrongStream
		}
		return false, RefuseBehind
	}
	if h.Lag == nil || h.LastSeq == nil {
		// The end is unread, so the inequality's upper half, whose failure
		// is silent, cannot be evaluated.
		return false, RefuseBrokerUnreachable
	}
	if h.AheadOfLog() || h.StreamRecreated || h.GenerationPassed || h.LogDiverged {
		return false, RefuseWrongStream
	}
	// THE LAG ITSELF, non-nil by the check above: a node with records left
	// to apply holds only a PREFIX of the log.
	if strict && *h.Lag > 0 {
		return false, RefuseBehind
	}
	return true, ""
}

// Answers reports whether this copy of a log may answer a REQUEST now — a
// read, or a write decided from its rows: it is [Health.Serving], or it is
// established at a lag of zero this instant ([Health.Established] strict).
//
// Neither half alone: Serving refuses a level copy whose drained latch is not
// set yet (up to a fetch wait), so a seat admitted at lag zero would be
// refused by its own node; strict refuses a busy copy on most instants, a
// record being in flight. What a request must see of its own node's writes is
// the session floors' job, not either half's. Refused, it answers Serving's
// refusal, which says what the copy is waiting for.
func (h Health) Answers() (bool, ReadRefusal) {
	ok, refusal := h.Serving()
	if ok {
		return true, ""
	}
	if level, _ := h.Established(true); level {
		return true, ""
	}
	return false, refusal
}

// Serving reports whether this copy of a log may answer for the estate rather
// than a peer's: ESTABLISHED on the log's own terms (at or above the floor, on
// the stream its rows are keyed to), DRAINED since its applier started, and
// within [SnapshotLagSlack] of the end. Otherwise it answers the refusal that
// holds, [RefuseBehind] for a copy never drained or past the slack.
//
// A history and a distance, never the instant: [Health.Established] strict
// refuses at a lag of one, which a busy log has on most heartbeats. The slack
// is a donor's, since a copy close enough to hand a joiner is close enough to
// answer for the estate.
func (h Health) Serving() (bool, ReadRefusal) {
	if ok, refusal := h.Established(false); !ok {
		return false, refusal
	}
	// The lag is non-nil here: an unread end is refused above.
	if !h.Drained || *h.Lag > SnapshotLagSlack {
		return false, RefuseBehind
	}
	return true, ""
}
