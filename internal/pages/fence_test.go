package pages_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE FENCE CLEARS AN EXPECTATION OF ZERO ONLY WHERE NOTHING THIS NODE MISSED
// CAN BE GONE — below the HIGHER of the published floor and the log's own
// first surviving sequence.
//
// Everything below either may have been removed, so a node that has consumed
// through the one before the higher holds everything that may be gone and an
// absent anchor really does mean an unclaimed address. One record short of it,
// the anchor may be a claim trimmed beneath this node, and publishing at zero
// takes an address somebody already holds. Each bound alone clears a node the
// other refuses: the floor is zero on a blocked trim and a stale first
// sequence trails a purge the floor already licensed. An unreadable bound
// refuses: failing open here is a lost update, which nothing recovers.
func TestTheFenceClearsZeroOnlyAtOrPastTheHigherBound(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cursor := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 100}
	unreadable := errors.New("unreadable")

	for name, tc := range map[string]struct {
		floor, first       uint64
		floorErr, firstErr error
		refused            bool
	}{
		"both bounds are the next record":     {floor: 101, first: 101},
		"both bounds are behind the cursor":   {floor: 40, first: 40},
		"nothing has ever been trimmed":       {floor: 0, first: 1},
		"a stream nobody has written":         {floor: 0, first: 0},
		"the floor is one past the next":      {floor: 102, first: 40, refused: true},
		"a purge the stream has not shown":    {floor: 150, first: 101, refused: true},
		"a blocked trim over a purged log":    {floor: 0, first: 102, refused: true},
		"a floor the counted minimum dragged": {floor: 100, first: 150, refused: true},
		"the floor could not be read":         {floorErr: unreadable, first: 40, refused: true},
		"the log could not be read":           {floor: 40, firstErr: unreadable, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			fence := pages.NewFence(db, "node-a")
			fence.Floor = func(context.Context, uint32) (uint64, error) { return tc.floor, tc.floorErr }
			fence.First = func(context.Context) (uint64, error) { return tc.first, tc.firstErr }
			err := fence.ClearForZero(t.Context(), cursor)
			if (err != nil) != tc.refused {
				t.Fatalf("ClearForZero(floor %d, first %d) = %v, want refused=%v",
					tc.floor, tc.first, err, tc.refused)
			}
			if tc.refused && tc.floorErr == nil && tc.firstErr == nil &&
				!errors.Is(err, statelog.ErrUnavailable) {
				t.Fatalf("a node below the log was refused with %v, want %v — the "+
					"caller tells `another node can serve this` from `this write "+
					"conflicted` by it", err, statelog.ErrUnavailable)
			}
		})
	}
}

// A FENCE BUILT WITHOUT EITHER BOUND REFUSES rather than clearing on the one
// it has: wiring only the floor is the shape that cleared a node below the
// log whenever the trim was blocked.
func TestAFenceMissingABoundRefusesZero(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cursor := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 100}
	low := func(context.Context) (uint64, error) { return 0, nil }

	onlyFloor := pages.NewFence(db, "node-a")
	onlyFloor.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	if err := onlyFloor.ClearForZero(t.Context(), cursor); err == nil {
		t.Fatal("a fence with no first sequence cleared an expectation of zero")
	}
	onlyFirst := pages.NewFence(db, "node-a")
	onlyFirst.First = low
	if err := onlyFirst.ClearForZero(t.Context(), cursor); err == nil {
		t.Fatal("a fence with no published floor cleared an expectation of zero")
	}
}

// THE FLOOR IS READ AT THE CURSOR'S OWN GENERATION.
//
// The cursor is the checkpoint the write's snapshot read, and an adoption can
// move this node to another generation between that snapshot and the check.
// A floor is a sequence in its generation's number space, so one read at any
// other generation says nothing about this cursor: here the cursor's own
// generation has trimmed past it and the next one has trimmed nothing, and a
// fence reading the second clears a write at zero over records it never saw.
func TestTheFenceReadsTheFloorAtTheCursorsGeneration(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cursor := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 100}
	floors := map[uint32]uint64{1: 150, 2: 0}

	fence := pages.NewFence(db, "node-a")
	fence.Floor = func(_ context.Context, generation uint32) (uint64, error) {
		return floors[generation], nil
	}
	fence.First = func(context.Context) (uint64, error) { return 1, nil }
	if err := fence.ClearForZero(t.Context(), cursor); !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a cursor at %d on generation 1, whose floor is %d, was answered %v, "+
			"want %v — the floor was read at a generation this cursor is not on",
			cursor.Seq, floors[1], err, statelog.ErrUnavailable)
	}
}
