package statelog

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// LogEnds is both ends of a domain's log as ONE read of the stream reports
// them: the first sequence it still holds and the last one it wrote.
//
// One value rather than two reads, because a first from before a purge paired
// with a last from after it describes a window the log never had, and the zero
// fence asks both of the one read the write already pays for.
type LogEnds struct {
	First uint64
	Last  uint64
}

// ZeroFence is [Fence.ClearForZero] written ONCE, for every domain whose writes
// can reach an expectation of zero: a domain supplies the four reads, and each
// refusal is an [*Unavailable] under its own reason, never [ErrConflict]. The
// floor is compared with the DECISION's checkpoint (the cursor), the log's ends
// with the NODE's ([ZeroFence.Committed]): each the conservative one.
//
// It refuses `evicted` ([evictionRefusal]); `floor_unknown` when the floor or
// the ends are unread, since failing open is a lost update nothing recovers;
// `wrong_stream` when the node is past the log's end ([ErrAheadOfLog]), asked
// before the floor; and, when [Replayable] fails, `below_floor` (adopt a
// snapshot) if the log no longer holds what the node lacks, else `behind`,
// which clears on its own (package doc, "THE FLOOR THEOREM").
type ZeroFence struct {
	// Evicted is this node's own eviction, read from its own applied rows —
	// the domain's [Fence.Evicted].
	Evicted func(context.Context) (bool, error)

	// Floor is the fleet's published trim floor for this domain at the named
	// generation, the cursor's own: a floor is a sequence in its generation's
	// number space.
	Floor func(ctx context.Context, generation uint32) (uint64, error)

	// Ends is the log's two ends, read live: the one read of the log a write
	// at zero takes, so the end is checked here rather than by a second trip.
	Ends func(context.Context) (LogEnds, error)

	// Committed is this node's applier's committed checkpoint, live
	// ([Runner.Committed]). Read immediately BEFORE Ends: on a healthy log a
	// checkpoint read first never exceeds an end read after it, while one read
	// after can, when a record lands and is applied between the two reads.
	Committed func() Position
}

// ClearForZero verifies, freshly within the call, that publishing at an
// expectation of zero is safe from this node, for a write whose snapshot was
// taken at cursor.
//
// The floor and the log's ends are read AT ONCE: each is a round trip to a
// different leader, and in turn they would cost the write their sum. Nothing
// orders them: the floor theorem asks only that the floor follow the write's
// own read of its subject, and as the bound is the higher of the floor and the
// first sequence, a floor read either side of the ends bounds every record they
// show gone (the trim publishes a floor before its purge). The
// checkpoint stays immediately before the ends; the eviction is read first,
// because it makes the other two moot.
func (z ZeroFence) ClearForZero(ctx context.Context, cursor Position) error {
	if z.Evicted == nil || z.Floor == nil || z.Ends == nil || z.Committed == nil {
		// A wiring mistake, not a fleet state, so an error rather than a
		// reason: clearing on the reads it has would clear a node below the log
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
	// The floor beside the log, on a goroutine always waited for, so
	// nothing it starts outlives the call.
	var (
		floor    uint64
		floorErr error
		read     = make(chan struct{})
	)
	go func() {
		defer close(read)
		floor, floorErr = z.Floor(ctx, cursor.Generation)
	}()
	// The node's checkpoint immediately before the log ([ZeroFence.Committed]).
	at := z.Committed()
	ends, err := z.Ends(ctx)
	<-read
	// The floor's refusal first: either unread leaves the bound unknown,
	// and one refusal names one of them.
	if floorErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Unavailable{
			Reason: ReasonFloorUnknown,
			Detail: fmt.Sprintf("the published trim floor for %s could not be "+
				"read: %v — a floor that cannot be read is not a floor that is "+
				"low, and publishing at zero on the guess is a lost update nothing "+
				"recovers%s", cursor.Stream, floorErr, floorClears(floorErr)),
			Cause: floorErr,
		}
	}
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
	// A checkpoint past the end FIRST: every comparison below assumes the
	// node and the log share one sequence space, and such a checkpoint clears
	// every bound because it is past them all, so a retry at zero would land at
	// a sequence this node has already passed. Judged on the NODE's checkpoint:
	// the applier only leads the decision, so on the decision's a node that
	// applied past a restored log's end after deciding would clear.
	if pastEnd(at.Seq, ends.Last) {
		cause := aheadOfLog{at: at, last: ends.Last}.err(cursor.Stream)
		return &Unavailable{Reason: ReasonWrongStream, Detail: cause.Error(), Cause: cause}
	}
	// Everything below the higher of the two may be gone, so a decision whose
	// rows hold through the one before it has seen all of it; [Replayable] is
	// the one place that boundary is written (a node fresh from a snapshot sits
	// exactly one below it).
	held := max(floor, ends.First)
	if Replayable(cursor.Seq, held) {
		return nil
	}
	// WHICH refusal is asked of the NODE against the log's first sequence
	// alone, the split [Health.Established] and readiness make, so a node the
	// read path calls replaying is never one the write path sends to adopt: the
	// floor is published before its purge, so the log may still hold the rest.
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

// floorClears is the clause a refusal over an unread floor ends with, by what
// kept it from being read: the two causes clear in opposite ways
// ([coord.ErrFloorUnreadable]). A store that does not answer clears when it
// answers; a row that does not read as its domain's floor stays until somebody
// removes it, so naming coordination there gives the wrong remedy. Any other
// cause (a floor at a generation the cursor is not on) states itself and gets
// no remedy here.
func floorClears(err error) string {
	switch {
	case errors.Is(err, coord.ErrFloorUnreadable):
		return "; it clears once that row is removed, as the error above says, " +
			"and the next trim publishes the floor afresh — not when coordination " +
			"answers, which it already does"
	case errors.Is(err, coord.ErrUnavailable):
		return "; it clears when coordination answers"
	default:
		return ""
	}
}

// evictionRefusal is what a reading of this node's own eviction refuses with,
// or nil when it permits the write: the ONE sentence fence 0 and the zero fence
// both answer with, because it is one finding reached twice.
//
// An unread eviction BLOCKS, since publishing under it makes records every node
// drops, as [ReasonEvictionUnknown], which clears when the state reads again so
// the caller comes back rather than give up on this node. A cancelled caller
// gets its context's error: a refusal would count a shutdown as a fleet event.
// The store's own words go to the log, never the detail every surface sends
// the caller ([Reader.scopeUnread]); the error still travels as Cause.
func evictionRefusal(ctx context.Context, evicted bool, err error) error {
	switch {
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		log.WarnContext(ctx, "statelog_eviction_unread", "error", err.Error())
		return &Unavailable{
			Reason: ReasonEvictionUnknown,
			Detail: "this node's own eviction state could not be read from its " +
				"store, and publishing under an eviction nobody can see produces " +
				"records every node drops — the reason is in this node's log",
			Cause: err,
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
