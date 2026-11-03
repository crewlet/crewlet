package statelog_test

import (
	"testing"
)

// THE ROWS CHANGE WITH EVERY RECORD BUT A BARRIER, and nothing else moves where
// they last did ([statelog.Runner.Changed]).
//
// A barrier is every linearizable read in the fleet, applied by every data
// node, and it writes no row: a reading of where this node's rows last changed
// that moved with it — as the reached position and the committed checkpoint
// both do — retires a cache keyed on it at every read. A barrier this node
// RETAINS is a row of the deferral index, though, and a write is a write. And
// a load of the rows — a restart over the same file, which reprocesses what an
// earlier build retained and moves no position — is a reading of its own.
func TestTheRowsChangeWithEveryRecordButABarrier(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "o1", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "o2", "op-2", 1))
	loop := h.looping(t)
	until(t, "the two writes", func() bool { return h.runner.Committed().Seq >= 2 })
	written := h.runner.Changed()
	if written.At.Seq != 2 {
		t.Fatalf("the rows last changed at %s after two writes, want 2", written.At)
	}

	for seq := uint64(3); seq <= 5; seq++ {
		h.fetch.offer(seq, barrierEnv())
	}
	until(t, "the barriers committed", func() bool { return h.runner.Committed().Seq >= 5 })
	if got := h.runner.Changed(); got != written {
		t.Fatalf("three barriers moved where the rows last changed from %s to %s — "+
			"every read in the fleet retires a cache keyed on it", written, got)
	}

	h.fetch.offerFramed(6, sealedUnder(t, "k-lacked", barrierEnv()))
	until(t, "the barrier under a lacked key retained", func() bool {
		return h.runner.Committed().Seq >= 6 && h.retainedCount() == 1
	})
	if got := h.runner.Changed().At.Seq; got != 6 {
		t.Errorf("the rows last changed at %d after a barrier was retained, want 6 — "+
			"the retained record is a row", got)
	}
	h.fetch.offer(7, env(7, "edit", "o7", "op-7", 1))
	until(t, "the write after it", func() bool { return h.runner.Committed().Seq >= 7 })
	if got := h.runner.Changed().At.Seq; got != 7 {
		t.Errorf("the rows last changed at %d after a write at 7", got)
	}
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}

	before := h.runner.Changed()
	if err := h.boot(1); err != nil {
		t.Fatalf("boot the runner again: %v", err)
	}
	if after := h.runner.Changed(); after.String() == before.String() {
		t.Fatalf("a load of the rows read %s as before it — what a load reprocesses "+
			"changes rows no position names", after)
	}
}
