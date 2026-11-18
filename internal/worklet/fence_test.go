package worklet_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fenceStream is the log every cursor below is a position on.
var fenceStream = worklet.Domain{}.Stream().Name

// standingAt is this node's applier, committed at p and staying there — the
// ordinary state, in which the node stands where its write's snapshot did.
func standingAt(p statelog.Position) func() statelog.Position {
	return func() statelog.Position { return p }
}

// THE FENCE CLEARS AN EXPECTATION OF ZERO ONLY WHERE NOTHING THIS NODE MISSED
// CAN BE GONE — below the HIGHER of the published floor and the log's own
// first surviving sequence. One record short, the absent anchor may have been
// trimmed beneath this node, and publishing at zero overwrites it.
//
// Each bound alone clears a node the other refuses: the floor is zero on a
// blocked trim, and a stale first sequence trails a purge the floor licensed.
// An unreadable bound refuses, since failing open is an unrecoverable lost
// update. The log's first sequence picks the reason: a node whose next record
// the log still holds is `behind`; one whose next record is gone `below_floor`.
func TestTheFenceClearsZeroOnlyAtOrPastTheHigherBound(t *testing.T) {
	t.Parallel()
	db := openFenceStore(t)
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	unreadable := errors.New("unreadable")

	for name, tc := range map[string]struct {
		floor, first       uint64
		floorErr, firstErr error
		want               statelog.Reason
	}{
		"both bounds are the next record":     {floor: 101, first: 101},
		"both bounds are behind the cursor":   {floor: 40, first: 40},
		"nothing has ever been trimmed":       {floor: 0, first: 1},
		"a stream nobody has written":         {floor: 0, first: 0},
		"the floor is one past the next":      {floor: 102, first: 40, want: statelog.ReasonBehind},
		"a purge the stream has not shown":    {floor: 150, first: 101, want: statelog.ReasonBehind},
		"a blocked trim over a purged log":    {floor: 0, first: 102, want: statelog.ReasonBelowFloor},
		"a floor the counted minimum dragged": {floor: 100, first: 150, want: statelog.ReasonBelowFloor},
		"the floor could not be read":         {floorErr: unreadable, first: 40, want: statelog.ReasonFloorUnknown},
		"the log could not be read":           {floor: 40, firstErr: unreadable, want: statelog.ReasonFloorUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			fence := worklet.NewFence(db.Reader(), "node-a")
			fence.Floor = func(context.Context, uint32) (uint64, error) { return tc.floor, tc.floorErr }
			// The log ends well past the cursor: every case is about the
			// floor.
			fence.Ends = func(context.Context) (statelog.LogEnds, error) {
				return statelog.LogEnds{First: tc.first, Last: 500}, tc.firstErr
			}
			fence.Committed = standingAt(cursor)
			err := fence.ClearForZero(t.Context(), cursor)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ClearForZero(floor %d, first %d) = %v, want cleared",
						tc.floor, tc.first, err)
				}
				return
			}
			requireFenceRefusal(t, err, tc.want)
		})
	}
}

// EVERY REFUSAL THE FENCE MAKES NAMES ITS OWN REASON, and none of them is a
// conflict: the work tools read statelog.ErrConflict as "somebody else is
// editing — read it again", a loop that never lands for an evicted node, and
// the publisher's refusal counter records the reason. A checkpoint past the
// log's end is refused though it clears every bound: a retry at zero would
// append at a sequence this node's applier has already passed.
func TestEveryZeroFenceRefusalNamesItsReason(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	unreadable := errors.New("unreadable")
	floor := func(context.Context, uint32) (uint64, error) { return 40, nil }
	ends := func(first, last uint64) func(context.Context) (statelog.LogEnds, error) {
		return func(context.Context) (statelog.LogEnds, error) {
			return statelog.LogEnds{First: first, Last: last}, nil
		}
	}
	// wired is a fence over db, standing at the cursor, with the floor and
	// the log answering as given.
	wired := func(db store.ReplicatedHandle, floor func(context.Context, uint32) (uint64, error),
		ends func(context.Context) (statelog.LogEnds, error)) *worklet.Fence {

		f := worklet.NewFence(db.Reader(), "node-a")
		f.Floor, f.Ends, f.Committed = floor, ends, standingAt(cursor)
		return f
	}

	for name, tc := range map[string]struct {
		build func(t *testing.T) *worklet.Fence
		want  statelog.Reason
		cause error
	}{
		"control: a node on the log, above the floor, not evicted": {
			build: func(t *testing.T) *worklet.Fence {
				return wired(openFenceStore(t), floor, ends(40, 100))
			},
		},
		"this node's own eviction row": {
			build: func(t *testing.T) *worklet.Fence {
				db := openFenceStore(t)
				evictInTracker(t, db, "node-a")
				return wired(db, floor, ends(40, 100))
			},
			want: statelog.ReasonEvicted,
		},
		"an eviction that cannot be read": {
			build: func(t *testing.T) *worklet.Fence {
				node, db := openFenceNode(t)
				if err := node.CloseReplicated(); err != nil {
					t.Fatalf("close: %v", err)
				}
				return wired(db, floor, ends(40, 100))
			},
			want: statelog.ReasonEvictionUnknown, cause: store.ErrNoEstate,
		},
		"a floor that cannot be read": {
			build: func(t *testing.T) *worklet.Fence {
				return wired(openFenceStore(t),
					func(context.Context, uint32) (uint64, error) { return 0, unreadable },
					ends(40, 100))
			},
			want: statelog.ReasonFloorUnknown, cause: unreadable,
		},
		"a log that cannot be read": {
			build: func(t *testing.T) *worklet.Fence {
				return wired(openFenceStore(t), floor,
					func(context.Context) (statelog.LogEnds, error) {
						return statelog.LogEnds{}, unreadable
					})
			},
			want: statelog.ReasonFloorUnknown, cause: unreadable,
		},
		"a checkpoint past the log's end": {
			build: func(t *testing.T) *worklet.Fence {
				return wired(openFenceStore(t), floor, ends(40, 99))
			},
			want: statelog.ReasonWrongStream, cause: statelog.ErrAheadOfLog,
		},
		"a node replaying up to the floor": {
			build: func(t *testing.T) *worklet.Fence {
				return wired(openFenceStore(t),
					func(context.Context, uint32) (uint64, error) { return 150, nil },
					ends(40, 400))
			},
			want: statelog.ReasonBehind,
		},
		"a node below the log": {
			build: func(t *testing.T) *worklet.Fence {
				return wired(openFenceStore(t), floor, ends(150, 400))
			},
			want: statelog.ReasonBelowFloor,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.build(t).ClearForZero(t.Context(), cursor)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ClearForZero = %v, want cleared — a fence that refuses "+
						"the healthy node proves nothing about the others", err)
				}
				return
			}
			requireFenceRefusal(t, err, tc.want)
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("ClearForZero = %v, which does not answer errors.Is(%v) — "+
					"the refusal carries what it was concluded from", err, tc.cause)
			}
		})
	}
}

// A CHECKPOINT EXACTLY AT THE LOG'S END IS ON THE LOG: refusing it would
// refuse every caught-up node once the fleet went quiet. THIS NODE's
// checkpoint is compared, not the write's: a node that applied past the end
// after deciding is past it.
func TestACheckpointAtTheLogsEndIsNotPastIt(t *testing.T) {
	t.Parallel()
	fence := worklet.NewFence(openFenceStore(t).Reader(), "node-a")
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(context.Context) (statelog.LogEnds, error) {
		return statelog.LogEnds{First: 1, Last: 100}, nil
	}
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	fence.Committed = standingAt(cursor)
	if err := fence.ClearForZero(t.Context(), cursor); err != nil {
		t.Fatalf("a node that has applied the log's last record was refused: %v", err)
	}
	past := cursor
	past.Seq = 101
	fence.Committed = standingAt(past)
	requireFenceRefusal(t, fence.ClearForZero(t.Context(), cursor), statelog.ReasonWrongStream)
}

// A FENCE BUILT WITHOUT ANY ONE OF ITS READS REFUSES rather than clearing on
// the ones it has: the floor alone clears a node below the log whenever the
// trim is blocked, and with no checkpoint it cannot say whether this node is
// on the log at all.
func TestAFenceMissingABoundRefusesZero(t *testing.T) {
	t.Parallel()
	db := openFenceStore(t)
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	floor := func(context.Context, uint32) (uint64, error) { return 0, nil }
	ends := func(context.Context) (statelog.LogEnds, error) {
		return statelog.LogEnds{Last: 100}, nil
	}

	onlyFloor := worklet.NewFence(db.Reader(), "node-a")
	onlyFloor.Floor, onlyFloor.Committed = floor, standingAt(cursor)
	if err := onlyFloor.ClearForZero(t.Context(), cursor); err == nil {
		t.Fatal("a fence with no read of the log cleared an expectation of zero")
	}
	onlyEnds := worklet.NewFence(db.Reader(), "node-a")
	onlyEnds.Ends, onlyEnds.Committed = ends, standingAt(cursor)
	if err := onlyEnds.ClearForZero(t.Context(), cursor); err == nil {
		t.Fatal("a fence with no published floor cleared an expectation of zero")
	}
	noCheckpoint := worklet.NewFence(db.Reader(), "node-a")
	noCheckpoint.Floor, noCheckpoint.Ends = floor, ends
	if err := noCheckpoint.ClearForZero(t.Context(), cursor); err == nil {
		t.Fatal("a fence with no read of this node's checkpoint cleared an expectation of zero")
	}
	full := worklet.NewFence(db.Reader(), "node-a")
	full.Floor, full.Ends, full.Committed = floor, ends, standingAt(cursor)
	if err := full.ClearForZero(t.Context(), cursor); err != nil {
		t.Fatalf("control: the fully wired fence refused %v, so the cases above "+
			"show nothing about a missing read", err)
	}
}

// requireFenceRefusal requires a fence refusal in every form a caller knows it
// by: the reason, the ErrUnavailable sentinel, and NOT the conflict a model
// reads as a colleague editing.
func requireFenceRefusal(t *testing.T, err error, want statelog.Reason) {
	t.Helper()
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != want {
		t.Fatalf("ClearForZero = %v, want an Unavailable naming %q", err, want)
	}
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("ClearForZero = %v, which does not answer errors.Is(%v)",
			err, statelog.ErrUnavailable)
	}
	if errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("ClearForZero = %v, which answers errors.Is(%v) — a caller reads "+
			"that as somebody else editing and retries here, where nothing it "+
			"does can land", err, statelog.ErrConflict)
	}
	if want == statelog.ReasonWrongStream &&
		!strings.Contains(err.Error(), "crewlet retention reanchor") {
		t.Fatalf("ClearForZero = %v, which does not name the verb that repairs it", err)
	}
}

// openFenceStore opens a node's store and its replicated estate in a fresh directory.
func openFenceStore(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	_, db := openFenceNode(t)
	return db
}

// openFenceNode is [openFenceStore] with the node's own handle beside it.
func openFenceNode(t *testing.T) (*store.DB, store.ReplicatedHandle) {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	return node, db
}

// evictInTracker writes the row this node's applier writes on its own
// eviction, the one source the fence reads.
func evictInTracker(t *testing.T, db store.ReplicatedHandle, nodeID string) {
	t.Helper()
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_evictions (node_id, log_stream, from_position, at, by)
			VALUES (?, ?, ?, ?, 'operator')`,
			nodeID, fenceStream, int64(50), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("write the eviction row: %v", err)
	}
}

// THE FLOOR IS READ AT THE CURSOR'S OWN GENERATION: an adoption can move this
// node to another generation between the write's snapshot and the check, and a
// floor is a sequence in its own generation's number space. Here generation 1
// has trimmed past the cursor and 2 nothing; reading 2 would clear a write at
// zero over records the node never saw.
func TestTheFenceReadsTheFloorAtTheCursorsGeneration(t *testing.T) {
	t.Parallel()
	db := openFenceStore(t)
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	floors := map[uint32]uint64{1: 150, 2: 0}

	fence := worklet.NewFence(db.Reader(), "node-a")
	fence.Floor = func(_ context.Context, generation uint32) (uint64, error) {
		return floors[generation], nil
	}
	fence.Ends = func(context.Context) (statelog.LogEnds, error) {
		return statelog.LogEnds{First: 1, Last: 500}, nil
	}
	fence.Committed = standingAt(cursor)
	err := fence.ClearForZero(t.Context(), cursor)
	if err == nil {
		t.Fatalf("a cursor at %d on generation 1, whose floor is %d, was cleared — "+
			"the floor was read at a generation this cursor is not on",
			cursor.Seq, floors[1])
	}
	requireFenceRefusal(t, err, statelog.ReasonBehind)
}
