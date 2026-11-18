package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/learning/memread"
)

// A SEAT THIS NODE HOLDS HAS ITS MEMORY READ HERE, and the answer says so. The
// reader finds a holder by the incarnation a lease names, so it must be armed
// with the seat host's owner, the one leases are written under. Armed with any
// other incarnation, a read of this node's own seat goes to the broker,
// addressed to nobody, and reports the holder silent.
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
		// The seat as the asker resolves it: memread re-resolves nothing.
		seat := memread.Seat{ID: placed.ID, Handle: placed.Handle}
		got, err := reads.Memory(t.Context(), seat, memread.MemoryQuery{Limit: 1})
		if err != nil {
			t.Fatalf("reading %s's memory on the node holding it: %v", seat.Handle, err)
		}
		if got.HeldBy != e.Node().ID() {
			t.Errorf("%s's memory was answered by %q, want this node %q",
				seat.Handle, got.HeldBy, e.Node().ID())
		}
	}
}
