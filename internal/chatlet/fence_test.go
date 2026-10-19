package chatlet_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// fenceStream is the log every cursor below is a position on.
var fenceStream = chatlet.Domain{}.Stream().Name

// fenceStandingAt is this node's applier, committed at p and staying there —
// the ordinary state, in which the node stands where its write's snapshot did.
func fenceStandingAt(p statelog.Position) func() statelog.Position {
	return func() statelog.Position { return p }
}

// THE CHAT FENCE REACHES THE ONE ZERO FENCE, AND EVERY REFUSAL IT MAKES NAMES
// ITS OWN REASON.
//
// [statelog.ZeroFence] is the rule, certified by its own table; what this
// domain supplies is the four reads, and the one it does not borrow is its own
// eviction — read from `chat_evictions`, this node's applied rows. So each read
// is broken here on its own, against a control that clears, and each refusal
// is held to the form a caller tells it by: an Unavailable naming its reason,
// never the conflict [chatlet.Store] translates into "the room kept changing",
// which sends a model round a loop that cannot land. A fence that failed open
// on any of them is a second room holding one address — the lost update
// nothing recovers.
func TestTheChatFenceRefusesZeroByItsOwnReads(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	unreadable := errors.New("unreadable")
	floor := func(context.Context, uint32) (uint64, error) { return 40, nil }
	ends := func(first, last uint64) func(context.Context) (statelog.LogEnds, error) {
		return func(context.Context) (statelog.LogEnds, error) {
			return statelog.LogEnds{First: first, Last: last}, nil
		}
	}
	// wired is a fence over a store, standing at the cursor, with the floor
	// and the log answering as given.
	wired := func(db store.ReplicatedHandle, floor func(context.Context, uint32) (uint64, error),
		ends func(context.Context) (statelog.LogEnds, error)) *chatlet.Fence {

		f := chatlet.NewFence(db.Reader(), "node-a")
		f.Floor, f.Ends, f.Committed = floor, ends, fenceStandingAt(cursor)
		return f
	}

	for name, tc := range map[string]struct {
		build func(t *testing.T) *chatlet.Fence
		want  statelog.Reason
		cause error
	}{
		"control: a node on the log, above the floor, not evicted": {
			build: func(t *testing.T) *chatlet.Fence {
				return wired(openFenceStore(t), floor, ends(40, 100))
			},
		},
		"this node's own eviction row": {
			build: func(t *testing.T) *chatlet.Fence {
				db := openFenceStore(t)
				evictInChat(t, db, "node-a", 0)
				return wired(db, floor, ends(40, 100))
			},
			want: statelog.ReasonEvicted,
		},
		// THE ROW IS AN INVERSE COMMIT, so a readmission is a column on
		// the eviction rather than its absence — and a node taken back is
		// cleared by that column alone.
		"an eviction a readmission took back": {
			build: func(t *testing.T) *chatlet.Fence {
				db := openFenceStore(t)
				evictInChat(t, db, "node-a", 60)
				return wired(db, floor, ends(40, 100))
			},
		},
		"another node's eviction": {
			build: func(t *testing.T) *chatlet.Fence {
				db := openFenceStore(t)
				evictInChat(t, db, "node-b", 0)
				return wired(db, floor, ends(40, 100))
			},
		},
		"an eviction that cannot be read": {
			build: func(t *testing.T) *chatlet.Fence {
				node, db := openFenceNode(t)
				if err := node.CloseReplicated(); err != nil {
					t.Fatalf("close: %v", err)
				}
				return wired(db, floor, ends(40, 100))
			},
			// ITS OWN REASON, not an eviction: nothing says this node
			// is evicted, only that nobody can see whether it is — a
			// retryable answer rather than a fence that holds.
			want: statelog.ReasonEvictionUnknown, cause: store.ErrNoEstate,
		},
		"a floor that cannot be read": {
			build: func(t *testing.T) *chatlet.Fence {
				return wired(openFenceStore(t),
					func(context.Context, uint32) (uint64, error) { return 0, unreadable },
					ends(40, 100))
			},
			want: statelog.ReasonFloorUnknown, cause: unreadable,
		},
		"a log that cannot be read": {
			build: func(t *testing.T) *chatlet.Fence {
				return wired(openFenceStore(t), floor,
					func(context.Context) (statelog.LogEnds, error) {
						return statelog.LogEnds{}, unreadable
					})
			},
			want: statelog.ReasonFloorUnknown, cause: unreadable,
		},
		"a checkpoint past the log's end": {
			build: func(t *testing.T) *chatlet.Fence {
				return wired(openFenceStore(t), floor, ends(40, 99))
			},
			want: statelog.ReasonWrongStream, cause: statelog.ErrAheadOfLog,
		},
		"a node replaying up to the floor": {
			build: func(t *testing.T) *chatlet.Fence {
				return wired(openFenceStore(t),
					func(context.Context, uint32) (uint64, error) { return 150, nil },
					ends(40, 400))
			},
			want: statelog.ReasonBehind,
		},
		"a node below the log": {
			build: func(t *testing.T) *chatlet.Fence {
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

// THE FLOOR IS READ AT THE CURSOR'S OWN GENERATION.
//
// The cursor is the checkpoint the write's snapshot read, and an adoption can
// move this node to another generation between that snapshot and the check. A
// floor is a sequence in its generation's number space, so here the cursor's
// own generation has trimmed past it and the next one has trimmed nothing, and
// a fence reading the second clears a room's create at zero over records it
// never saw.
func TestTheChatFenceReadsTheFloorAtTheCursorsGeneration(t *testing.T) {
	t.Parallel()
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	floors := map[uint32]uint64{1: 150, 2: 0}

	fence := chatlet.NewFence(openFenceStore(t).Reader(), "node-a")
	fence.Floor = func(_ context.Context, generation uint32) (uint64, error) {
		return floors[generation], nil
	}
	fence.Ends = func(context.Context) (statelog.LogEnds, error) {
		return statelog.LogEnds{First: 1, Last: 500}, nil
	}
	fence.Committed = fenceStandingAt(cursor)
	requireFenceRefusal(t, fence.ClearForZero(t.Context(), cursor), statelog.ReasonBehind)
}

// A FENCE BUILT WITHOUT ANY ONE OF ITS READS REFUSES rather than clearing on
// the ones it has: the engine sets all three after the runner exists, and a
// fence it forgot one of would otherwise clear a node below the log whenever
// the trim was blocked.
func TestAChatFenceMissingABoundRefusesZero(t *testing.T) {
	t.Parallel()
	db := openFenceStore(t)
	cursor := statelog.Position{Stream: fenceStream, Generation: 1, Seq: 100}
	floor := func(context.Context, uint32) (uint64, error) { return 0, nil }
	ends := func(context.Context) (statelog.LogEnds, error) {
		return statelog.LogEnds{Last: 100}, nil
	}
	for name, build := range map[string]func(f *chatlet.Fence){
		"no read of the log":           func(f *chatlet.Fence) { f.Floor, f.Committed = floor, fenceStandingAt(cursor) },
		"no published floor":           func(f *chatlet.Fence) { f.Ends, f.Committed = ends, fenceStandingAt(cursor) },
		"no read of this node's place": func(f *chatlet.Fence) { f.Floor, f.Ends = floor, ends },
	} {
		f := chatlet.NewFence(db.Reader(), "node-a")
		build(f)
		if err := f.ClearForZero(t.Context(), cursor); err == nil {
			t.Errorf("a fence with %s cleared an expectation of zero", name)
		}
	}
	full := chatlet.NewFence(db.Reader(), "node-a")
	full.Floor, full.Ends, full.Committed = floor, ends, fenceStandingAt(cursor)
	if err := full.ClearForZero(t.Context(), cursor); err != nil {
		t.Fatalf("control: the fully wired fence refused %v, so the cases above "+
			"show nothing about a missing read", err)
	}
}

// requireFenceRefusal is a fence refusal in every form a caller recognises it
// by: the reason a surface switches on, the sentinel a caller tells a refusal
// from a failure by, and NOT the conflict a model reads as a colleague editing.
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
		t.Fatalf("ClearForZero = %v, which answers errors.Is(%v) — the store "+
			"says that as \"the room kept changing\", and a caller retries here, "+
			"where nothing it does can land", err, statelog.ErrConflict)
	}
	if want == statelog.ReasonWrongStream &&
		!strings.Contains(err.Error(), "crewlet retention reanchor") {
		t.Fatalf("ClearForZero = %v, which does not name the verb that repairs it", err)
	}
}

// openFenceStore opens a node's store and its replicated estate in a fresh
// directory.
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

// evictInChat writes the row this node's applier writes when it applies an
// eviction — the one source the fence reads — at position 50, and the
// readmission above it where readmitted is not zero.
func evictInChat(t *testing.T, db store.ReplicatedHandle, nodeID string, readmitted int64) {
	t.Helper()
	back := sql.NullInt64{Int64: readmitted, Valid: readmitted != 0}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO chat_evictions (node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, 'operator', ?, ?, ?)`,
			nodeID, store.EncodeTime(time.Now().UTC()), int64(50), back, int64(50))
		return err
	}); err != nil {
		t.Fatalf("write the eviction row: %v", err)
	}
}
