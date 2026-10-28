package kv

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// A SINGLE NODE'S FENCING EPOCH OUTLIVES ITS PROCESS. Its leases are an
// in-process table that dies with the process, and its counter used to be one
// too: every boot minted 1 again, under the sandbox runs the previous process
// had stamped with a later token in the fleet store. Here two processes are
// two lease tables over one broker, each with its own handle on the counter.
func TestAnInProcessLeaseTableMintsFromTheCounterThatOutlivesIt(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	cfg := EpochsConfig{BucketPrefix: fmt.Sprintf("t%d", bucketSeq.Add(1))}
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
	// bucket, so a move from local coordination to a fleet keeps the tokens
	// the single node's runs were stamped with below the fleet's.
	store, err := Open(t.Context(), jsOf(nc), Config{TTL: time.Minute, BucketPrefix: cfg.BucketPrefix})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fleet := claim(store, "fleet-node")
	if fleet.Epoch <= after.Epoch {
		t.Fatalf("the fleet's first claim minted %d, not above the single node's %d", fleet.Epoch, after.Epoch)
	}
}

// The in-process lease table a single node runs is certified by the one
// contract suite on the counter it actually mints from, not only on its own
// in-process count: the claim that mints outside the lock is a protocol of its
// own, and this is where the suite's stampedes and lapses reach it.
func TestContractInProcessLeasesOnThePersistentCounter(t *testing.T) {
	nc := embeddedNATS(t)
	coordtest.Run(t, func(t *testing.T) coord.Backend {
		counter, err := OpenEpochs(t.Context(), jsOf(nc), EpochsConfig{BucketPrefix: fmt.Sprintf("t%d", bucketSeq.Add(1))})
		if err != nil {
			t.Fatalf("OpenEpochs: %v", err)
		}
		return &memory.Backend{Epochs: counter}
	})
}
