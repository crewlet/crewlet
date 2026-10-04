package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/learning/memread"
)

// A SEAT THIS NODE HOLDS HAS ITS MEMORY READ HERE, and the answer says so.
//
// The reader finds a seat's holder by the incarnation its lease names, so the
// reader has to be armed with the SEAT HOST's owner — the one the leases are
// written under — and not with any other incarnation this process mints. Armed
// with the wrong one, every read of a seat this node holds is sent to the
// broker addressed to an incarnation nobody answers as, and a node reading its
// own seats' memory is told the holder is silent.
func TestASeatThisNodeHoldsHasItsMemoryReadHere(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(e.Node().Attached()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	attached := e.Node().Attached()
	if len(attached) < 2 {
		t.Fatalf("attached %v after 10s, want both seats", attached)
	}
	reads := e.MemoryReads()
	if reads == nil {
		t.Fatal("an engine with a store built no memory reader")
	}
	company := e.Company()
	for _, placed := range company.Seats() {
		// THE SEAT AS THE ASKER RESOLVES IT: its id and its handle —
		// memread re-resolves nothing.
		seat := memread.Seat{ID: placed.ID, Handle: placed.Handle}
		got, err := reads.Memory(t.Context(), seat, 1)
		if err != nil {
			t.Fatalf("reading %s's memory on the node holding it: %v", seat.Handle, err)
		}
		if got.HeldBy != e.Node().ID() {
			t.Errorf("%s's memory was answered by %q, want this node %q",
				seat.Handle, got.HeldBy, e.Node().ID())
		}
		threads, err := reads.Threads(t.Context(), seat, "", 0)
		if err != nil || threads.HeldBy != e.Node().ID() {
			t.Errorf("%s's threads: held_by %q, err %v", seat.Handle, threads.HeldBy, err)
		}
	}
}
