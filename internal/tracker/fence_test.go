package tracker_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE FENCE CLEARS AN EXPECTATION OF ZERO ONLY BELOW THE PUBLISHED FLOOR.
//
// The floor is the first sequence the trim has not licensed removing, so a
// node that has consumed through the one before it holds everything that may
// be gone and an absent anchor really does mean an empty subject. One record
// short of that, the anchor may be a record trimmed beneath this node, and
// publishing at zero overwrites it. An unreadable floor refuses: failing open
// here is a lost update, which nothing recovers.
func TestTheFenceClearsZeroOnlyAtOrPastTheFloor(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cursor := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 100}

	for name, tc := range map[string]struct {
		floor   uint64
		err     error
		refused bool
	}{
		"the floor is the next record":   {floor: 101},
		"the floor is behind the cursor": {floor: 40},
		"the floor is one past the next": {floor: 102, refused: true},
		"the floor could not be read":    {floor: 0, err: errors.New("no register"), refused: true},
		"nothing has ever been trimmed":  {floor: 0},
	} {
		t.Run(name, func(t *testing.T) {
			fence := tracker.NewFence(db, "node-a")
			fence.Cursor = func() statelog.Position { return cursor }
			fence.Floor = func(context.Context) (uint64, error) { return tc.floor, tc.err }
			err := fence.ClearForZero(t.Context(), cursor)
			if (err != nil) != tc.refused {
				t.Fatalf("ClearForZero = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}
