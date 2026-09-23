package statelog

import (
	"context"
	"fmt"
)

// LogEnds is both ends of a domain's log as ONE read of the stream reports
// them: the first sequence it still holds and the last one it wrote.
//
// ONE VALUE rather than two readers, because a first sequence from before a
// purge paired with a last sequence from after it describes a window the log
// never had — and because the zero fence asks both questions of the one read
// the write already pays for.
type LogEnds struct {
	First uint64
	Last  uint64
}

// ZeroFence is [Fence.ClearForZero] written ONCE, for every domain whose writes
// can reach an expectation of zero.
//
// # Why one implementation
//
// The tracker's fence and the pages fence were two copies of this check, and
// the copies had already agreed on the wrong answer twice: both refused an
// evicted node with [ErrConflict], which a caller reads as a colleague editing
// the same object and a model is told to re-read and retry; and both refused a
// node below the floor by wrapping [ErrUnavailable] in prose, so the reasons
// [ReasonBelowFloor] and [ReasonFloorUnknown] were declared, documented and
// produced by nothing — the refusal counter recorded every one of them as
// `error`. A domain supplies the four reads; the refusals are decided here.
//
// # Two positions, because two questions are asked
//
// The DECISION's checkpoint — the cursor the caller passes, [Snap.Checkpoint] —
// is what the floor theorem is about: its conclusion is that a trimmed record
// on this subject is already in the rows the decision read, so it is compared
// with the floor at its own generation, and a later position would be the
// permissive answer. The NODE's checkpoint — [ZeroFence.Committed], read here
// immediately before the log — is what the other two comparisons are about,
// because both are properties of where this node's applier stands rather than
// of one decision: whether what it appends lands at a sequence its applier has
// already passed, and whether what it lacks is still on the log for it to
// replay. Each position is the conservative one for the question it answers: a
// decision only trails its node, so the floor refuses more against it; the
// node only leads its decision, so the end refuses more against it.
//
// # What it refuses, each with its own reason
//
//   - `evicted`: this node's own eviction row says so, or cannot be read —
//     the one sentence fence 0 refuses with, because it is the same finding.
//   - `floor_unknown`: the published floor or the log's ends could not be
//     read. Either is half of the bound F the floor theorem is stated over, so
//     either unread leaves F unknown — and A READ THAT ANSWERS UNKNOWN MUST
//     REFUSE: failing open here is a lost update, which nothing recovers.
//   - `wrong_stream`: this node's checkpoint is past the log's end, so it is a
//     position in a history the log does not hold ([ErrAheadOfLog]). Asked
//     BEFORE the floor, because the floor comparison is between sequences in
//     one space and this is the finding that there is not one.
//   - `below_floor`: [Replayable] fails against the higher of the floor and
//     the log's first sequence, so an absent anchor may be a record trimmed
//     beneath this node rather than an object never written — AND the record
//     this node's applier needs next is gone from the log itself, so no replay
//     can supply it and the node adopts a peer's snapshot.
//   - `behind`: the same comparison fails, and the log still holds every
//     record this node lacks — it is REPLAYING up to the floor, the state
//     [FloorReplaying] names, or it has already applied past a decision that
//     was taken below it. The write is refused all the same, because the floor
//     theorem holds only once the decision's rows reach the floor; but it
//     clears on its own, which is what `behind` says on the write path and on
//     the read path alike, and `below_floor` would send the caller away and an
//     operator to a snapshot this node does not need.
//
// Every refusal is an [*Unavailable], so errors.Is(err, ErrUnavailable) holds
// and none of them is [ErrConflict]: each says this node cannot make this write
// now, and none says somebody else did.
type ZeroFence struct {
	// Evicted is this node's own eviction, read from its own applied rows —
	// the domain's [Fence.Evicted].
	Evicted func(context.Context) (bool, error)

	// Floor is the fleet's published trim floor for this domain, at the
	// generation named — the cursor's own, because a floor is a sequence in
	// its generation's number space and says nothing about a cursor on
	// another.
	Floor func(ctx context.Context, generation uint32) (uint64, error)

	// Ends is the log's two ends, read live. It is the one read of the log
	// an expectation of zero takes, which is why the end is checked here
	// rather than by a second round trip.
	Ends func(context.Context) (LogEnds, error)

	// Committed is this node's applier's committed checkpoint, live — the
	// runner's own [Runner.Committed]. Read immediately BEFORE Ends, which
	// is the only order in which comparing it with the end is sound: on a
	// healthy log a checkpoint read first can never exceed an end read after
	// it, while one read after can, whenever a record lands and is applied
	// between the two reads.
	Committed func() Position
}

// ClearForZero verifies, freshly and within the call, that publishing at an
// expectation of zero is safe from this node, for a write whose snapshot was
// taken at cursor — see [ZeroFence] for which comparison each position is
// asked.
func (z ZeroFence) ClearForZero(ctx context.Context, cursor Position) error {
	if z.Evicted == nil || z.Floor == nil || z.Ends == nil || z.Committed == nil {
		// A WIRING MISTAKE, not a state of the fleet, so it is an error
		// rather than a reason: a fence that cannot read one of its four
		// inputs cannot establish that an absent anchor means an empty
		// subject rather than a record trimmed beneath it, and clearing on
		// the inputs it has is the shape that cleared a node below the log
		// whenever the trim was blocked.
		return fmt.Errorf("statelog: the zero fence for %s is missing a read "+
			"(eviction %t, floor %t, log ends %t, checkpoint %t) — it cannot "+
			"establish that an absent anchor means an empty subject rather than "+
			"a record trimmed beneath this node", cursor.Stream,
			z.Evicted != nil, z.Floor != nil, z.Ends != nil, z.Committed != nil)
	}
	evicted, err := z.Evicted(ctx)
	if refusal := evictionRefusal(ctx, evicted, err); refusal != nil {
		return refusal
	}
	floor, err := z.Floor(ctx, cursor.Generation)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Unavailable{
			Reason: ReasonFloorUnknown,
			Detail: fmt.Sprintf("the published trim floor for %s could not be "+
				"read: %v — a floor that cannot be read is not a floor that is "+
				"low, and publishing at zero on the guess is a lost update nothing "+
				"recovers; it clears when coordination answers", cursor.Stream, err),
			Cause: err,
		}
	}
	// THE NODE'S CHECKPOINT BEFORE THE LOG, and immediately before it —
	// see [ZeroFence.Committed].
	at := z.Committed()
	ends, err := z.Ends(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Unavailable{
			Reason: ReasonFloorUnknown,
			Detail: fmt.Sprintf("the ends of %s could not be read: %v — a log "+
				"that cannot be read is not one that has lost nothing, and "+
				"publishing at zero on the guess is a lost update nothing "+
				"recovers; it clears when the broker answers", cursor.Stream, err),
			Cause: err,
		}
	}
	// A CHECKPOINT PAST THE END FIRST. Every term below is a sequence, and
	// comparing them is meaningful only while this node and the log are in
	// one sequence space — which this is the finding that they are not. It
	// is the only branch here that sees it: a checkpoint past the end clears
	// every bound below BECAUSE it is past all of them, so without this
	// check the fence waved a retry at zero onto a log that will append it
	// at a sequence this node has already passed.
	//
	// THE NODE'S, NOT THE DECISION'S. Where the record lands against where
	// the applier stands is what makes it unanswerable, and the applier only
	// leads the decision: judged on the snapshot's checkpoint instead, a
	// node that applied past a restored log's end after deciding would
	// clear.
	if pastEnd(at.Seq, ends.Last) {
		cause := aheadOfLog{at: at, last: ends.Last}.err(cursor.Stream)
		return &Unavailable{Reason: ReasonWrongStream, Detail: cause.Error(), Cause: cause}
	}
	// EVERYTHING BELOW THE HIGHER OF THE TWO MAY BE GONE, so a decision
	// whose rows hold everything through the one before it has seen
	// everything that may be gone. Compared against the cursor itself the
	// check refused a node ONE BELOW that bound although it holds everything
	// it needs — which is where a node that has just adopted a snapshot
	// sits, since the snapshot term licenses trimming through the artefact's
	// own position — and [Replayable] is the one place that boundary is
	// written.
	held := max(floor, ends.First)
	if Replayable(cursor.Seq, held) {
		return nil
	}
	// AND WHICH REFUSAL is asked of the NODE against the log's first
	// sequence alone — the split [Health.Established] and readiness make,
	// so a node the read path calls replaying is never one the write path
	// sends to adopt. The floor is published before the purge it licenses,
	// so the log may still hold everything the node lacks.
	if !Replayable(at.Seq, ends.First) {
		return &Unavailable{
			Reason: ReasonBelowFloor,
			Detail: fmt.Sprintf("records of %s below %d are gone from the log "+
				"(first surviving sequence %d, published floor %d) and this node "+
				"has applied through %d, so an absent anchor may be a record "+
				"trimmed beneath it rather than an object never written, and no "+
				"replay can supply what it lacks — it adopts a peer's snapshot, "+
				"and another node can make this write meanwhile",
				cursor.Stream, ends.First, ends.First, floor, at.Seq),
		}
	}
	need := Position{Stream: cursor.Stream, Generation: cursor.Generation, Seq: held - 1}
	return &Unavailable{
		Reason: ReasonBehind,
		Detail: fmt.Sprintf("records of %s below %d may be removed from the log "+
			"(published floor %d, first surviving sequence %d) and this write was "+
			"decided from rows through %d, so an absent anchor may be a record the "+
			"trim licensed removing rather than an object never written; the log "+
			"still holds every record this node lacks (it has applied through %d), "+
			"so this clears on its own once it has applied through %d and the "+
			"write is decided again",
			cursor.Stream, held, floor, ends.First, cursor.Seq, at.Seq, need.Seq),
		Position: need,
	}
}

// evictionRefusal is what a reading of this node's own eviction refuses with,
// or nil when it permits the write — the ONE sentence fence 0 and the zero fence
// both answer with, because it is one finding reached twice.
//
// THE THIRD VALUE BLOCKS. An eviction that cannot be read is not an eviction
// that did not happen, and publishing under it produces durable records every
// node drops. A cancelled caller is the exception, and only because it is not a
// reading at all: it is the caller leaving, and naming it `evicted` would put a
// shutdown on the refusal counter as a fleet event.
func evictionRefusal(ctx context.Context, evicted bool, err error) error {
	switch {
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return &Unavailable{
			Reason: ReasonEvicted,
			Detail: fmt.Sprintf("this node's own eviction state could not be read: %v", err),
			Cause:  err,
		}
	case evicted:
		return &Unavailable{
			Reason: ReasonEvicted,
			Detail: "this node has been removed from the fleet; nothing it " +
				"publishes will be applied anywhere. An operator readmits it",
		}
	}
	return nil
}
