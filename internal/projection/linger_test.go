package projection

import (
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE LINGER YIELDS TO A WAITER, and this test is RED against the code it
// replaces.
//
// The bug had no crash in it and two correct halves: a partial batch waited
// its whole 250 ms window for company that was not coming, while a caller sat
// on the condition variable for a revision ALREADY IN THAT BATCH. The write
// path paid it on every write with no other traffic, which is most writes on a
// quiet company.
//
// The assertion is that lingerFor RETURNS EARLY when the waiter's revision is
// already covered, and that it still waits when nobody is waiting for anything
// this batch carries — the second half is what stops the "fix" from being
// "never linger", which would cost the batching the linger exists for.
func TestProjectionLingerYieldsToAWaiter(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		waiting  uint64
		wantFast bool
	}{
		"nobody is waiting":                     {0, false},
		"a waiter wants a revision we hold":     {7, true},
		"a waiter wants exactly the newest":     {9, true},
		"a waiter wants one we do not hold yet": {12, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &Projector{buf: newBuffer(1 << 20), waiting: tc.waiting}
			p.applied = sync.NewCond(&p.mu)

			batch := []*coord.Change{
				{Key: "a", Revision: 7},
				{Key: "b", Revision: 9},
			}

			start := time.Now()
			got := p.lingerFor(t.Context(), batch)
			elapsed := time.Since(start)

			if len(got) != len(batch) {
				t.Fatalf("the batch changed size: %d, want %d", len(got), len(batch))
			}
			// Half the window: comfortably past an immediate return and
			// comfortably short of a full linger, so neither answer is a
			// timing coincidence on a loaded runner.
			fast := elapsed < applyLinger/2
			if fast != tc.wantFast {
				t.Errorf("lingerFor took %v (fast=%v), want fast=%v: a batch "+
					"that already carries what somebody is blocked on has "+
					"nobody left to batch with, and one that does not still "+
					"wants its window", elapsed, fast, tc.wantFast)
			}
		})
	}
}

// THE POLL IS GONE: the linger is woken by the buffer's own signal, so a batch
// that fills returns the instant it fills rather than up to one poll interval
// later.
//
// The window is not what this measures — a partial batch is still entitled to
// wait its whole linger for company, which is what the linger is FOR. What it
// measures is that reaching the batch size ends the wait immediately, which is
// only true if an arrival wakes the loop.
//
// Mutation: drop the wake from Push and this test pays the full window for a
// batch that was complete 5 ms in.
func TestTheLingerIsWokenByAnArrival(t *testing.T) {
	t.Parallel()
	p := &Projector{buf: newBuffer(1 << 20)}
	p.applied = sync.NewCond(&p.mu)

	// One short of a full batch, so the arrival below completes it.
	batch := make([]*coord.Change, 0, applyBatch)
	for i := range applyBatch - 1 {
		batch = append(batch, &coord.Change{Key: "seed", Revision: uint64(i + 1)})
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		p.buf.Push(&coord.Change{Key: "late", Revision: 999})
	}()

	start := time.Now()
	got := p.lingerFor(t.Context(), batch)
	elapsed := time.Since(start)

	if len(got) != applyBatch {
		t.Fatalf("batch = %d changes, want the arrival to have completed it at %d",
			len(got), applyBatch)
	}
	if elapsed >= applyLinger/2 {
		t.Errorf("the linger took %v to notice the arrival that completed its "+
			"batch: it is waiting out a timer rather than being woken", elapsed)
	}
}
