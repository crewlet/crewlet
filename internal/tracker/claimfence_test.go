package tracker_test

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A MERGE WHOSE LEASE LAPSES MID-WALK WRITES NOTHING AFTER IT.
//
// The log knows nothing of a lease: an append from a holder whose lease has
// lapsed is accepted like any other, while the duty takes the claim over and
// finishes the same merge beside it. So the walk checks its claim inside each
// append's decide, and a holder that can no longer vouch for its lease stops
// before its next append — here, the second subtask's move and the close —
// says so, and leaves the marker for the duty.
//
// The store's clock and the holder's move together, so the lease runs out on
// both without the case sleeping through it.
//
// Mutation: drop the fence check from Writer.publish and the second subtask
// moves and the duplicate closes.
func TestAMergeWhoseLeaseLapsesMidWalkWritesNothingAfter(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	mergeFixture(t, r)
	claims, clock := memory.New(), newCaseClock()
	holder := writerOverClaims(t, r, claims, clock)
	hooked.arm(func(_, opID string) bool { return opID == "op-merge.c/kid-1" },
		func() { lapse(claims, clock) })

	_, err := holder.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil)
	if !hooked.didFire() {
		t.Fatal("the lease never lapsed inside the walk, so this case is not " +
			"the shape it names")
	}
	var stopped *tracker.PartialError
	if !errors.Is(err, tracker.ErrClaimLost) || !errors.As(err, &stopped) ||
		stopped.Rerun {
		t.Fatalf("a merge whose lease lapsed mid-walk answered %v, want it to "+
			"say it lost its claim, partial and not re-run — its mark landed", err)
	}
	r.drain()
	assertStoppedAtTheLapse(t, r)
}

// A SWEEP WHOSE LEASE LAPSES MID-WALK WRITES NOTHING AFTER IT EITHER.
//
// The duty finishing an abandoned merge is a walk under the same claim, and it
// is fenced the same way: once it cannot vouch for its lease it stops before
// its next append, the merge keeps its marker, and the failure is the tick's.
//
// Mutation: make finishAbandoned append through d.deps.Writer rather than the
// writer fenced on its claim, and the second subtask moves and the duplicate
// closes.
func TestASweepWhoseLeaseLapsesMidWalkWritesNothingAfter(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	mergeFixture(t, r)
	merging, reparent, keep := true, true, "keep"
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: keep,
			}}},
			Merging: &merging, MergeReparent: &reparent, MergeInto: &keep,
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	claims, clock := memory.New(), newCaseClock()
	sweeper := writerOverClaims(t, r, claims, clock)
	hooked.arm(func(_, opID string) bool { return strings.HasSuffix(opID, ".c/kid-1") },
		func() { lapse(claims, clock) })

	swept, err := trackerWorkerWriting(t, r, slog.New(slog.DiscardHandler),
		sweeper).Tick(t.Context())
	if !hooked.didFire() {
		t.Fatal("the lease never lapsed inside the sweep's walk, so this case " +
			"is not the shape it names")
	}
	if !errors.Is(err, tracker.ErrClaimLost) {
		t.Fatalf("a sweep whose lease lapsed mid-walk answered %v, want the "+
			"tick to report the lost claim", err)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Errorf("the sweep counted %d merge(s) finished after losing its claim", n)
	}
	r.drain()
	assertStoppedAtTheLapse(t, r)
}

// mergeFixture files the item a merge folds into and a duplicate with two
// subtasks, which a walk moves in id order.
func mergeFixture(t *testing.T, r *roundTrip) {
	t.Helper()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	for _, id := range []string{"kid-1", "kid-2"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
}

// assertStoppedAtTheLapse holds the rows to what a walk that stopped after
// moving the first subtask leaves: that one moved, the second not, and the
// duplicate still marked and open for whoever takes the claim next.
func assertStoppedAtTheLapse(t *testing.T, r *roundTrip) {
	t.Helper()
	if got := parentOf(r.task(t, "kid-1")); got != "keep" {
		t.Errorf("the subtask moved before the lapse is under %q, want keep", got)
	}
	if got := parentOf(r.task(t, "kid-2")); got != "dup" {
		t.Errorf("a subtask was moved onto %q after the walk's lease lapsed", got)
	}
	dup := r.task(t, "dup")
	if !dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
		t.Errorf("the duplicate reads merging=%v and status %q — a walk whose "+
			"lease had lapsed closed it", dup.Task.Merging, dup.Task.Status)
	}
}

// writerOverClaims is a writer on this harness's log and store whose claims
// come from the case's own coordination store, measured on the case's clock.
func writerOverClaims(t *testing.T, r *roundTrip, claims *memory.Backend,
	clock *caseClock) *tracker.Writer {
	t.Helper()
	w, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: r.publisher, DB: r.db, NodeID: "node-a", Claims: claims,
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now:        func() time.Time { return r.at },
		ClaimClock: clock.now,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w
}

// lapse runs one whole claim TTL out on the store's clock and the holder's.
func lapse(claims *memory.Backend, clock *caseClock) {
	claims.Advance(tracker.ClaimTTL)
	clock.advance(tracker.ClaimTTL)
}

// caseClock is a clock a case moves by hand.
type caseClock struct {
	mu sync.Mutex
	at time.Time
}

func newCaseClock() *caseClock {
	return &caseClock{at: time.Unix(1_700_000_000, 0)}
}

func (c *caseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *caseClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}
