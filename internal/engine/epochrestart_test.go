package engine_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SEAT'S FENCING EPOCH OUTLIVES A GRACEFUL RESTART ON LOCAL COORDINATION, and
// so does every run the seat parked before it.
//
// The leases of a single node are an in-process table, and its fencing counter
// used to be one too, so every boot minted epoch 1 while the fleet store kept
// the runs the previous process had stamped with a later token. A seat that
// had been re-claimed once in a process (a lapse, a re-placement) parked its
// runs at epoch 2; after a graceful restart its new holder claimed at epoch 1
// and was refused as OLDER than its own previous self on all of them: the
// person's answer, given on the seat's control topic, was handed back on every
// delivery until it was dead-lettered, and the run waited on its question for
// ever.
func TestAnAnswerToARunParkedBeforeAGracefulRestartIsRecordedAfterIt(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "stream")
	boot := func() *engine.Backends {
		t.Helper()
		b := bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, dir)
			b.Coordination.Type = config.CoordinationLocal
		})
		back, err := openBackends(t, b)
		if err != nil {
			t.Fatalf("OpenBackends: %v", err)
		}
		return back
	}
	seat := coord.SeatResource(fixtureSeat("swe"))
	claim := func(back *engine.Backends, owner string) *coord.Lease {
		t.Helper()
		lease, refused, err := back.Coord.TryAcquire(t.Context(), seat,
			coord.AcquireOptions{Owner: owner, TTL: time.Minute})
		if err != nil || lease == nil {
			t.Fatalf("TryAcquire(%s) = %v, %q, %v", owner, lease, refused, err)
		}
		return lease
	}
	release := func(back *engine.Backends, lease *coord.Lease) {
		t.Helper()
		if ok, err := back.Coord.Release(t.Context(), seat, lease.Owner, lease.Epoch); err != nil || !ok {
			t.Fatalf("Release(%s at %d) = %v, %v", lease.Owner, lease.Epoch, ok, err)
		}
	}

	// THE FIRST PROCESS claims the seat, gives it up and claims it again,
	// which is the second tenure a lapse gives a seat mid-process, and parks
	// a run under that second tenure's token.
	first := boot()
	release(first, claim(first, "node-0/first"))
	before := claim(first, "node-0/first")
	w := newSpentWorld(t)
	w.store = sandbox.NewCoordStore(first.Fleet)
	old := w.holder(t, sandbox.Fence{Owner: before.Owner, Epoch: before.Epoch}, &takingResumer{})
	run := w.parked(t, old)
	if run.OwnerEpoch != before.Epoch {
		t.Fatalf("the run was parked at epoch %d, want the tenure's %d", run.OwnerEpoch, before.Epoch)
	}
	// A GRACEFUL drain: the seat is handed back, and the process stops.
	old.coordinator.Stop()
	release(first, before)
	first.Close(context.WithoutCancel(t.Context()))

	// THE NEXT PROCESS claims the seat on the same disk.
	second := boot()
	t.Cleanup(func() { second.Close(context.WithoutCancel(t.Context())) })
	after := claim(second, "node-0/second")
	if after.Epoch <= before.Epoch {
		// Errorf: what the person sees is the answer below, so let it say so.
		t.Errorf("the seat was re-claimed after the restart at epoch %d, not above the %d "+
			"its runs were stamped with: the new holder is older than its own past self",
			after.Epoch, before.Epoch)
	}
	w.store = sandbox.NewCoordStore(second.Fleet)
	w.ledger = ledgerstore.NewMemoryCompletions()
	resumer := &takingResumer{}
	next := w.holder(t, sandbox.Fence{Owner: after.Owner, Epoch: after.Epoch}, resumer)
	if err := next.coordinator.RecoverSeat(t.Context(), "swe", after.Owner, after.Epoch); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}

	// The person answers from the Inbox, on the seat's control topic.
	next.deliver(t, answerAgainst(run, "use main"))
	next.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the run parked before the restart was resumed %d times, want once", resumer.count())
	}
}
