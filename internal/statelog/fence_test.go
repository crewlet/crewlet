package statelog_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// zeroReads composes a zero fence from fixed answers.
type zeroReads struct {
	evicted    bool
	evictedErr error
	floor      uint64
	floorErr   error
	ends       statelog.LogEnds
	endsErr    error
}

// fence is the zero fence over these answers, with this node's applier
// standing at whatever committed says.
func (r zeroReads) fence(committed func() statelog.Position) statelog.ZeroFence {
	return statelog.ZeroFence{
		Evicted: func(context.Context) (bool, error) { return r.evicted, r.evictedErr },
		Floor: func(context.Context, uint32) (uint64, error) {
			return r.floor, r.floorErr
		},
		Ends: func(context.Context) (statelog.LogEnds, error) {
			return r.ends, r.endsErr
		},
		Committed: committed,
	}
}

// standingAt is an applier whose committed checkpoint is p, and stays there.
func standingAt(p statelog.Position) func() statelog.Position {
	return func() statelog.Position { return p }
}

// THE ZERO FENCE NAMES EVERY REFUSAL IT MAKES, in the order that makes each
// answer mean what it says, and asks each question of the position it is
// about.
//
// It is the one implementation both identity-claiming domains' fences run, so
// this is the table the rule lives in; each domain's own tests pin that its
// fence reaches it. Five properties, each a way a fence was or could be wrong:
//
//   - every refusal is an Unavailable naming its reason — the copies answered
//     an evicted node with ErrConflict and a node below the floor with prose,
//     so `below_floor` and `floor_unknown` were produced by nothing;
//   - a checkpoint PAST the log's end refuses, `wrong_stream` — the copies
//     cleared it, because a cursor past every bound clears all of them;
//   - that check comes BEFORE the floor's, because the floor comparison is
//     between sequences in one space and this is the finding that they are
//     not: a node past the end of a restored log whose floor was published
//     from the old history reads as below the floor, and would be sent to adopt
//     a snapshot rather than to the re-anchor that repairs it;
//   - the end is judged on the NODE's checkpoint rather than the decision's,
//     because it asks where an append lands against where the applier stands,
//     and the applier only leads the decision;
//   - below the floor the refusal is `below_floor` only when the node's next
//     record is gone from the LOG: a node replaying records the log still
//     holds is `behind`, which clears on its own, and is the word the read
//     path gives the same state — `below_floor` sends a caller away and an
//     operator to a snapshot the node does not need.
func TestTheZeroFenceNamesEveryRefusal(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 100}
	unreadable := errors.New("unreadable")
	onTheLog := statelog.LogEnds{First: 40, Last: 120}

	for name, tc := range map[string]struct {
		reads zeroReads
		// applied is where this node's applier stands; zero reads as the
		// decision's own checkpoint, which is the ordinary state.
		applied uint64
		want    statelog.Reason
		cause   error
		// need is the position a `behind` refusal names.
		need uint64
	}{
		"control: on the log, at the floor, not evicted": {
			reads: zeroReads{floor: 101, ends: onTheLog},
		},
		"control: a checkpoint exactly at the log's end": {
			reads: zeroReads{floor: 40, ends: statelog.LogEnds{First: 40, Last: 100}},
		},
		"evicted": {
			reads: zeroReads{evicted: true, floor: 40, ends: onTheLog},
			want:  statelog.ReasonEvicted,
		},
		"an eviction nobody could read": {
			reads: zeroReads{evictedErr: unreadable, floor: 40, ends: onTheLog},
			want:  statelog.ReasonEvicted, cause: unreadable,
		},
		"a floor nobody could read": {
			reads: zeroReads{floorErr: unreadable, ends: onTheLog},
			want:  statelog.ReasonFloorUnknown, cause: unreadable,
		},
		"a log nobody could read": {
			reads: zeroReads{floor: 40, endsErr: unreadable},
			want:  statelog.ReasonFloorUnknown, cause: unreadable,
		},
		"a checkpoint one past the log's end": {
			reads: zeroReads{floor: 40, ends: statelog.LogEnds{First: 40, Last: 99}},
			want:  statelog.ReasonWrongStream, cause: statelog.ErrAheadOfLog,
		},
		"past the end AND below a floor from the old history": {
			reads: zeroReads{floor: 150, ends: statelog.LogEnds{First: 40, Last: 90}},
			want:  statelog.ReasonWrongStream, cause: statelog.ErrAheadOfLog,
		},
		// THE DECISION IS ON THE LOG AND THE NODE IS NOT: it decided at
		// 100, applied on to 110, and the log ends at 105. Whatever it
		// appends lands at 106, which its applier has passed.
		"the node past the end though the decision it took is not": {
			reads:   zeroReads{floor: 40, ends: statelog.LogEnds{First: 40, Last: 105}},
			applied: 110,
			want:    statelog.ReasonWrongStream, cause: statelog.ErrAheadOfLog,
		},
		// THE FLOOR IS PUBLISHED BEFORE THE PURGE IT LICENSES, so the log
		// can still hold what a node below the floor lacks.
		"one record below the floor, replaying": {
			reads: zeroReads{floor: 102, ends: onTheLog},
			want:  statelog.ReasonBehind, need: 101,
		},
		"well below the floor, replaying": {
			reads: zeroReads{floor: 110, ends: onTheLog},
			want:  statelog.ReasonBehind, need: 109,
		},
		"below the log's own first sequence": {
			reads: zeroReads{floor: 0, ends: statelog.LogEnds{First: 102, Last: 120}},
			want:  statelog.ReasonBelowFloor,
		},
		"below the log and the floor above it": {
			reads: zeroReads{floor: 110, ends: statelog.LogEnds{First: 105, Last: 120}},
			want:  statelog.ReasonBelowFloor,
		},
		// THE DECISION PREDATES A RECORD THE TRIM HAS SINCE REMOVED, and
		// this node has applied it: the write is refused, because the rows
		// it decided from never saw that record, but nothing is missing
		// from the node — deciding again clears it.
		"a decision below a record the node has since applied and the log dropped": {
			reads:   zeroReads{floor: 0, ends: statelog.LogEnds{First: 105, Last: 120}},
			applied: 110,
			want:    statelog.ReasonBehind, need: 104,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := cursor
			if tc.applied != 0 {
				node.Seq = tc.applied
			}
			err := tc.reads.fence(standingAt(node)).ClearForZero(t.Context(), cursor)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ClearForZero = %v, want cleared — a fence that refuses "+
						"the healthy node proves nothing about the others", err)
				}
				return
			}
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != tc.want {
				t.Fatalf("ClearForZero = %v, want an Unavailable naming %q", err, tc.want)
			}
			if !errors.Is(err, statelog.ErrUnavailable) || errors.Is(err, statelog.ErrConflict) {
				t.Fatalf("ClearForZero = %v: a fence refusal answers ErrUnavailable "+
					"and never ErrConflict, which a caller reads as a colleague "+
					"editing and retries here", err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("ClearForZero = %v, which does not answer errors.Is(%v)",
					err, tc.cause)
			}
			switch tc.want {
			case statelog.ReasonWrongStream:
				for _, want := range []string{"at sequence", "ends at", "crewlet retention reanchor"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal %q does not name %q", err, want)
					}
				}
			case statelog.ReasonBehind:
				want := statelog.Position{Stream: cursor.Stream, Generation: cursor.Generation, Seq: tc.need}
				if refusal.Position != want {
					t.Errorf("the refusal names %s, want %s — the position the write "+
						"needs this node to have applied through, which is what a "+
						"caller retries against", refusal.Position, want)
				}
				if !strings.Contains(refusal.Detail, "clears on its own") {
					t.Errorf("the refusal %q does not say it clears on its own", refusal.Detail)
				}
			case statelog.ReasonBelowFloor:
				if !strings.Contains(refusal.Detail, "gone from the log") {
					t.Errorf("the refusal %q does not say the records are gone from the log",
						refusal.Detail)
				}
			}
		})
	}
}

// THE NODE'S CHECKPOINT IS READ BEFORE THE LOG, never after.
//
// On a healthy log the end only grows and the checkpoint only follows it, so a
// checkpoint read first can never exceed an end read after it. Read the other
// way round, a record that lands and is applied between the two reads — every
// few milliseconds on a busy company — makes a healthy node look past the end,
// and it refuses its own writes as a restored broker. The staging lands that
// record inside the log read itself.
func TestTheZeroFenceReadsTheNodesCheckpointBeforeTheLog(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 100}
	var applied atomic.Uint64
	applied.Store(100)
	fence := zeroReads{floor: 40}.fence(func() statelog.Position {
		return statelog.Position{Stream: cursor.Stream, Generation: cursor.Generation,
			Seq: applied.Load()}
	})
	fence.Ends = func(context.Context) (statelog.LogEnds, error) {
		// THE LOG ENDS AT 100 WHEN ASKED, and a record lands at 101 and is
		// applied before the answer is back.
		defer applied.Store(101)
		return statelog.LogEnds{First: 40, Last: 100}, nil
	}
	if err := fence.ClearForZero(t.Context(), cursor); err != nil {
		t.Fatalf("ClearForZero = %v, want cleared — a node whose applier moved "+
			"between the two reads was judged on a checkpoint read after the end", err)
	}
}

// A FENCE MISSING A READ IS A WIRING MISTAKE, and it refuses as one: it cannot
// establish what an absent anchor means, and clearing on the reads it has is
// the shape that cleared a node below the log whenever the trim was blocked.
// It is NOT a reason, because no state of the fleet produces it.
func TestAZeroFenceMissingARead(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 100}
	full := zeroReads{floor: 40, ends: statelog.LogEnds{First: 40, Last: 120}}.
		fence(standingAt(cursor))
	for name, fence := range map[string]statelog.ZeroFence{
		"no eviction read": {Floor: full.Floor, Ends: full.Ends, Committed: full.Committed},
		"no floor":         {Evicted: full.Evicted, Ends: full.Ends, Committed: full.Committed},
		"no log":           {Evicted: full.Evicted, Floor: full.Floor, Committed: full.Committed},
		"no checkpoint":    {Evicted: full.Evicted, Floor: full.Floor, Ends: full.Ends},
	} {
		err := fence.ClearForZero(t.Context(), cursor)
		var refusal *statelog.Unavailable
		if err == nil || errors.As(err, &refusal) {
			t.Errorf("%s: ClearForZero = %v, want a wiring error", name, err)
		}
	}
	if err := full.ClearForZero(t.Context(), cursor); err != nil {
		t.Fatalf("control: the full fence refused %v, so the cases above show "+
			"nothing about a missing read", err)
	}
}

// A CALLER THAT LEFT IS NOT A FLEET EVENT. A read that failed because the
// caller's context ended answers the context's own error, not a refusal: a
// shutdown counted as `floor_unknown` would read on the refusal counter as a
// coordination outage.
func TestAZeroFenceAnswersACancelledCallerWithItsOwnError(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 100}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for name, reads := range map[string]zeroReads{
		"the eviction read": {evictedErr: context.Canceled},
		"the floor read":    {floorErr: context.Canceled},
		"the log read":      {endsErr: context.Canceled},
	} {
		err := reads.fence(standingAt(cursor)).ClearForZero(ctx, cursor)
		var refusal *statelog.Unavailable
		if !errors.Is(err, context.Canceled) || errors.As(err, &refusal) {
			t.Errorf("%s cancelled: ClearForZero = %v, want the context's own error", name, err)
		}
	}
}

// A ZERO-FENCE REFUSAL REACHES THE CALLER AND THE COUNTER UNDER ITS OWN REASON,
// through the publisher, and nothing lands.
//
// The refusal counter is an operator's only view of which remedy a fleet
// needs, and before this every one of these arrived as `error` or — for an
// evicted node — as `conflict`, which also bumped the per-kind conflict counter
// that says an object is contended. Driven through a real publisher over a real
// broker, with the fence's own implementation producing each refusal.
func TestAZeroFenceRefusalIsCountedUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("unreadable")
	onTheLog := statelog.LogEnds{First: 1, Last: 10}

	for name, tc := range map[string]struct {
		reads zeroReads
		want  statelog.Reason
	}{
		"control: the fence clears and the write lands at zero": {
			reads: zeroReads{ends: onTheLog},
		},
		// Fence 0 read the eviction as clear; it landed before the zero
		// fence's own fresh read, which is what that read is for.
		"evicted between fence 0 and the zero fence": {
			reads: zeroReads{evicted: true, ends: onTheLog},
			want:  statelog.ReasonEvicted,
		},
		"replaying up to the floor": {
			reads: zeroReads{floor: 50, ends: statelog.LogEnds{First: 1, Last: 60}},
			want:  statelog.ReasonBehind,
		},
		"below the log": {
			reads: zeroReads{floor: 50, ends: statelog.LogEnds{First: 50, Last: 60}},
			want:  statelog.ReasonBelowFloor,
		},
		"a floor nobody could read": {
			reads: zeroReads{floorErr: unreadable, ends: onTheLog},
			want:  statelog.ReasonFloorUnknown,
		},
		"a log nobody could read": {
			reads: zeroReads{endsErr: unreadable},
			want:  statelog.ReasonFloorUnknown,
		},
		"a checkpoint past the log's end": {
			reads: zeroReads{ends: statelog.LogEnds{First: 1, Last: 5}},
			want:  statelog.ReasonWrongStream,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			// THIS NODE HAS APPLIED THROUGH 10 of a log whose own ends the
			// case composes — the subject below was never written, so the
			// write reaches the zero fence with no append attempted.
			h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 10})
			zero := tc.reads.fence(h.applier.Committed)
			h.fence.zero = &zero

			res, err := h.write(probeSubject("fresh"), "op-1", "hello")
			if tc.want == "" {
				if err != nil || res.Outcome != statelog.OutcomeApplied {
					t.Fatalf("write = %+v, %v; want applied", res, err)
				}
				if got := refusals(h.metrics); len(got) != 0 {
					t.Fatalf("a write that landed counted refusals %v", got)
				}
				return
			}
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != tc.want {
				t.Fatalf("write = %v, want an Unavailable naming %q", err, tc.want)
			}
			if errors.Is(err, statelog.ErrConflict) {
				t.Fatalf("write = %v answers ErrConflict — the page and work tools "+
					"tell a model somebody else is editing", err)
			}
			if got := h.appends.appends.Load(); got != 0 {
				t.Fatalf("appended %d time(s) past a fence that refused", got)
			}
			got := refusals(h.metrics)
			if len(got) != 1 || got[string(tc.want)] != 1 {
				t.Fatalf("the refusal counter holds %v, want exactly {%s: 1} — the "+
					"reason label is what tells an operator which remedy applies",
					got, tc.want)
			}
			for _, s := range h.metrics.Read() {
				if s.Name == metrics.StatelogPublishConflicts {
					t.Fatalf("a fence refusal was counted as a conflict on %v — that "+
						"counter says an object is contended", s.Attrs)
				}
			}
		})
	}
}

// A REBUILD OUTRANKS THE ZERO FENCE'S OWN END CHECK.
//
// A log rebuilt under a node counts from 1 again, so the fence's read that
// establishes the rebuild also finds the checkpoint past the rebuilt log's end,
// and the fence refuses on the end before the publisher asks the identity.
// Answered in that order, a rebuild was reported as a restored broker — the
// same word, `wrong_stream`, but the wrong cause, and the one an operator acts
// on differently. Measured on a real engine before the order was fixed.
func TestARebuildOutranksTheZeroFencesEndCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 10})
	zero := zeroReads{ends: statelog.LogEnds{First: 0, Last: 0}}.fence(h.applier.Committed)
	h.fence.zero = &zero
	h.fence.reads = h.applier.rebuilt

	_, err := h.write(probeSubject("fresh"), "op-1", "hello")
	requireWrongStream(t, err)
	if errors.Is(err, statelog.ErrAheadOfLog) {
		t.Fatalf("write = %v names the end rather than the rebuild that put it "+
			"there", err)
	}
	if got := h.appends.appends.Load(); got != 0 {
		t.Fatalf("appended %d time(s) onto a rebuilt log", got)
	}

	// AND WHERE THE IDENTITY FINDS NOTHING, THE FENCE'S OWN REFUSAL
	// STANDS: a concurrent reading may have cleared the shared verdict, and
	// the end check is what keeps the zero branch honest within the call.
	h2 := newHarness(t)
	h2.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 10})
	zero2 := zeroReads{ends: statelog.LogEnds{First: 0, Last: 0}}.fence(h2.applier.Committed)
	h2.fence.zero = &zero2
	_, err = h2.write(probeSubject("fresh"), "op-1", "hello")
	if !errors.Is(err, statelog.ErrAheadOfLog) {
		t.Fatalf("write = %v, want the fence's own %v", err, statelog.ErrAheadOfLog)
	}
}

// refusals is the publisher's refusal counter, by reason.
func refusals(rec *metrics.Recorder) map[string]uint64 {
	out := map[string]uint64{}
	for _, s := range rec.Read() {
		if s.Name == metrics.StatelogPublishRefusals && s.Total > 0 {
			out[s.Attrs["reason"]] += s.Total
		}
	}
	return out
}
