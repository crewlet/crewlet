package kv

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// A SINGLE NODE'S FENCING EPOCH OUTLIVES ITS PROCESS. Its leases die with the
// process; a counter that did too would mint 1 again, under rows the previous
// process stamped with a later token in the fleet store. Two processes here
// are two lease tables over one broker, each with its own counter handle.
func TestAnInProcessLeaseTableMintsFromTheCounterThatOutlivesIt(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	cfg := EpochsConfig{BucketPrefix: fmt.Sprintf("t%d", bucketSeq.Add(1)), Reach: InProcess}
	process := func() *memory.Backend {
		t.Helper()
		counter, err := OpenEpochs(t.Context(), jsOf(nc), cfg)
		if err != nil {
			t.Fatalf("OpenEpochs: %v", err)
		}
		return &memory.Backend{Epochs: counter}
	}
	const seat = "seat:backend-engineer"
	claim := func(b coord.Backend, owner string) *coord.Lease {
		t.Helper()
		lease, refused, err := b.TryAcquire(t.Context(), seat, coord.AcquireOptions{Owner: owner, TTL: time.Minute})
		if err != nil || lease == nil {
			t.Fatalf("TryAcquire(%s) = %v, %q, %v", owner, lease, refused, err)
		}
		return lease
	}

	first := process()
	held := claim(first, "first")
	if ok, err := first.Release(t.Context(), seat, held.Owner, held.Epoch); err != nil || !ok {
		t.Fatalf("Release = %v, %v", ok, err)
	}
	before := claim(first, "first")
	if before.Epoch != held.Epoch+1 {
		t.Fatalf("a re-claim in the same process minted %d after %d, want the next token", before.Epoch, held.Epoch)
	}
	if ok, err := first.Release(t.Context(), seat, before.Owner, before.Epoch); err != nil || !ok {
		t.Fatalf("Release = %v, %v", ok, err)
	}

	after := claim(process(), "second")
	if after.Epoch <= before.Epoch {
		t.Fatalf("the next process claimed at epoch %d, not above the %d the last one "+
			"released: every row that tenure stamped now outranks the seat's holder",
			after.Epoch, before.Epoch)
	}

	// AND A FLEET CARRIES ON FROM IT: the KV lease store mints from the same
	// bucket, so the single node's tokens stay below the fleet's.
	store, err := Open(t.Context(), jsOf(nc), Config{TTL: time.Minute, BucketPrefix: cfg.BucketPrefix, Reach: InProcess})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fleet := claim(store, "fleet-node")
	if fleet.Epoch <= after.Epoch {
		t.Fatalf("the fleet's first claim minted %d, not above the single node's %d", fleet.Epoch, after.Epoch)
	}
}

// The in-process lease table runs the contract suite on the counter it really
// mints from: its claim mints outside the lock, a protocol of its own, and
// this is where the suite's stampedes and lapses reach it.
func TestContractInProcessLeasesOnThePersistentCounter(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	coordtest.Run(t, func(t *testing.T) coord.Backend {
		counter, err := OpenEpochs(t.Context(), jsOf(nc), EpochsConfig{BucketPrefix: fmt.Sprintf("t%d", bucketSeq.Add(1)), Reach: InProcess})
		if err != nil {
			t.Fatalf("OpenEpochs: %v", err)
		}
		return &memory.Backend{Epochs: counter}
	})
}

// A FLOOR ONLY EVER RAISES: it creates a missing record, lifts one below it
// and leaves one at or above it alone, so every node can floor at boot and agree.
func TestAFloorRaisesTheCounterAndNeverLowersIt(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	counter, err := OpenEpochs(t.Context(), jsOf(nc), EpochsConfig{BucketPrefix: fmt.Sprintf("t%d", bucketSeq.Add(1)), Reach: InProcess})
	if err != nil {
		t.Fatalf("OpenEpochs: %v", err)
	}
	const seat = "seat:a"
	for _, step := range []struct {
		floor  int64
		raised bool
	}{
		{floor: 5, raised: true},  // a resource the counter never minted for
		{floor: 3, raised: false}, // below: never lowered
		{floor: 5, raised: false}, // at: nothing to do
		{floor: 7, raised: true},  // above: lifted
	} {
		raised, err := counter.Floor(t.Context(), seat, step.floor)
		if err != nil || raised != step.raised {
			t.Fatalf("Floor(%d) = %v, %v; want %v", step.floor, raised, err, step.raised)
		}
	}
	if next, err := counter.Next(t.Context(), seat); err != nil || next != 8 {
		t.Fatalf("Next after a floor of 7 = %d, %v; want 8", next, err)
	}
}
