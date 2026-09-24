package pages_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE FENCE CLEARS AN EXPECTATION OF ZERO ONLY BELOW THE PUBLISHED FLOOR.
//
// The floor is the first sequence the trim has not licensed removing, so a
// node that has consumed through the one before it holds everything that may
// be gone and an absent anchor really does mean an empty subject. One record
// short of that, the anchor may be a record trimmed beneath this node, and
// publishing at zero overwrites it. An unreadable floor refuses: failing open
// here is a lost update, which nothing recovers. Every refusal names its
// reason, because each has a different remedy.
func TestTheFenceClearsZeroOnlyAtOrPastTheFloor(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cursor := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 100}

	for name, tc := range map[string]struct {
		floor  uint64
		err    error
		absent bool
		reason statelog.Reason
	}{
		"the floor is the next record":   {floor: 101},
		"the floor is behind the cursor": {floor: 40},
		"nothing has ever been trimmed":  {floor: 0},
		"the floor is one past the next": {floor: 102, reason: statelog.ReasonBelowFloor},
		"the floor could not be read": {
			err: errors.New("no register"), reason: statelog.ReasonFloorUnknown,
		},
		"no floor is wired at all": {absent: true, reason: statelog.ReasonFloorUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			fence := pages.NewFence(db, "node-a")
			fence.Cursor = func() statelog.Position { return cursor }
			if !tc.absent {
				fence.Floor = func(context.Context) (uint64, error) { return tc.floor, tc.err }
			}
			err := fence.ClearForZero(t.Context(), cursor)
			assertZeroRefusal(t, err, tc.reason)
			// THE POSITION A BELOW-FLOOR NODE MUST REACH, which is what
			// a caller told `below_floor` has to wait for.
			var refusal *statelog.Unavailable
			if tc.reason == statelog.ReasonBelowFloor && errors.As(err, &refusal) &&
				refusal.Position.Seq != tc.floor-1 {
				t.Errorf("the refusal names %s, want the position one below the "+
					"floor, %d", refusal.Position, tc.floor-1)
			}
		})
	}

	// AN EVICTED NODE IS REFUSED AS EVICTED, whatever the floor says: its
	// records apply nowhere, and a refusal read as a conflict tells a caller
	// a colleague is editing when no write from here will ever land.
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO pages_evictions (node_id, at, from_position, version)
			VALUES ('node-a', 0, 7, 7)`)
		return err
	}); err != nil {
		t.Fatalf("seed the eviction: %v", err)
	}
	fence := pages.NewFence(db, "node-a")
	fence.Floor = func(context.Context) (uint64, error) { return 0, nil }
	err = fence.ClearForZero(t.Context(), cursor)
	assertZeroRefusal(t, err, statelog.ReasonEvicted)
	if errors.Is(err, statelog.ErrConflict) {
		t.Errorf("an evicted node's refusal reads as a conflict: %v", err)
	}
}

// assertZeroRefusal holds a fence's answer to the refusal it should be: none
// for an empty reason, and otherwise a [statelog.Unavailable] naming exactly
// that reason — the value a caller switches on to decide what to do next.
func assertZeroRefusal(t *testing.T, err error, reason statelog.Reason) {
	t.Helper()
	if reason == "" {
		if err != nil {
			t.Fatalf("ClearForZero = %v, want it cleared", err)
		}
		return
	}
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Fatalf("ClearForZero = %v, want a %s refusal", err, reason)
	}
}
