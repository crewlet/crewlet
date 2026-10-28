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

// restartRig is one single node's disk, booted as many times as a case needs,
// with the swe seat's coding run carried across the boots.
type restartRig struct {
	dir  string
	seat string
	w    *spentWorld
}

func newRestartRig(t *testing.T) *restartRig {
	t.Helper()
	return &restartRig{
		dir:  filepath.Join(t.TempDir(), "stream"),
		seat: coord.SeatResource(fixtureSeat("swe")),
		w:    newSpentWorld(t),
	}
}

// boot opens the node's backends on local coordination, the default, and
// points the run's store at the fleet store this boot opened.
func (r *restartRig) boot(t *testing.T) *engine.Backends {
	t.Helper()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, r.dir)
		b.Coordination.Type = config.CoordinationLocal
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	r.w.store = sandbox.NewCoordStore(back.Fleet)
	r.w.ledger = ledgerstore.NewMemoryCompletions()
	return back
}

func (r *restartRig) claim(t *testing.T, back *engine.Backends, owner string) *coord.Lease {
	t.Helper()
	lease, refused, err := back.Coord.TryAcquire(t.Context(), r.seat,
		coord.AcquireOptions{Owner: owner, TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("TryAcquire(%s) = %v, %q, %v", owner, lease, refused, err)
	}
	return lease
}

func (r *restartRig) release(t *testing.T, back *engine.Backends, lease *coord.Lease) {
	t.Helper()
	if ok, err := back.Coord.Release(t.Context(), r.seat, lease.Owner, lease.Epoch); err != nil || !ok {
		t.Fatalf("Release(%s at %d) = %v, %v", lease.Owner, lease.Epoch, ok, err)
	}
}

// parkUnder launches the run under fence and parks it on its question.
func (r *restartRig) parkUnder(t *testing.T, fence sandbox.Fence) (sandbox.PendingRun, *spentHolder) {
	t.Helper()
	holder := r.w.holder(t, fence, &takingResumer{})
	run := r.w.parked(t, holder)
	if run.OwnerEpoch != fence.Epoch {
		t.Fatalf("the run was parked at epoch %d, want the tenure's %d", run.OwnerEpoch, fence.Epoch)
	}
	return run, holder
}

// answeredAfter is the seat's claim on the next boot: strictly above every
// epoch the run carries, the run recovered under it, and the person's answer,
// given on the seat's control topic, recorded and resuming the run.
func (r *restartRig) answeredAfter(t *testing.T, run sandbox.PendingRun) *coord.Lease {
	t.Helper()
	back := r.boot(t)
	t.Cleanup(func() { back.Close(context.WithoutCancel(t.Context())) })
	after := r.claim(t, back, "node-0/next")
	if after.Epoch <= run.OwnerEpoch {
		// Errorf: what the person sees is the answer below, so let it say so.
		t.Errorf("the seat was claimed after the restart at epoch %d, not above the %d "+
			"its run was stamped with: the new holder is older than its own past self",
			after.Epoch, run.OwnerEpoch)
	}
	resumer := &takingResumer{}
	next := r.w.holder(t, sandbox.Fence{Owner: after.Owner, Epoch: after.Epoch}, resumer)
	if err := next.coordinator.RecoverSeat(t.Context(), "swe", after.Owner, after.Epoch); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	next.deliver(t, answerAgainst(run, "use main"))
	next.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the run parked before the restart was resumed %d times, want once", resumer.count())
	}
	return after
}

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
	r := newRestartRig(t)

	// THE FIRST PROCESS claims the seat, gives it up and claims it again,
	// which is the second tenure a lapse gives a seat mid-process, and parks
	// a run under that second tenure's token.
	first := r.boot(t)
	r.release(t, first, r.claim(t, first, "node-0/first"))
	before := r.claim(t, first, "node-0/first")
	run, old := r.parkUnder(t, sandbox.Fence{Owner: before.Owner, Epoch: before.Epoch})
	// A GRACEFUL drain: the seat is handed back, and the process stops.
	old.coordinator.Stop()
	r.release(t, first, before)
	first.Close(context.WithoutCancel(t.Context()))

	_ = r.answeredAfter(t, run)
}

// AN UPGRADE OWNS THE RUNS ITS PREDECESSOR'S COUNTER STAMPED. A node that ran a
// build whose counter lived in its process left runs stamped with epochs the
// persistent counter never minted, and an operator who deletes the epochs
// bucket leaves the same: rows above an empty counter. The boot floors each
// seat's counter at the highest epoch its runs carry, so the seat's first
// claim already outranks them, with no repair by hand.
func TestAnUpgradeOwnsTheRunsItsPredecessorsCounterStamped(t *testing.T) {
	t.Parallel()
	r := newRestartRig(t)

	// Stamped at 2 by a counter this deployment's bucket never held, as the
	// live runs were: no claim is made on this boot, so the bucket knows
	// nothing of the seat.
	first := r.boot(t)
	run, old := r.parkUnder(t, sandbox.Fence{Owner: "node-0/old-build", Epoch: 2})
	old.coordinator.Stop()
	first.Close(context.WithoutCancel(t.Context()))

	if after := r.answeredAfter(t, run); after.Epoch != 3 {
		t.Errorf("the first claim over runs at epoch 2 minted %d, want 3: the counter "+
			"starts just above the highest epoch the seat's rows carry", after.Epoch)
	}
}

// ONE RUN RECORD NOTHING CAN READ DOES NOT STOP THE BOOT. The floor skips it,
// logged, and still floors every seat whose runs it can read: failing the
// boot over it stopped every node of the company starting.
func TestAnUnreadableRunRecordDoesNotStopTheBoot(t *testing.T) {
	t.Parallel()
	r := newRestartRig(t)
	first := r.boot(t)
	_, old := r.parkUnder(t, sandbox.Fence{Owner: "node-0/old-build", Epoch: 2})
	old.coordinator.Stop()
	if _, err := first.Fleet.CreateSandboxRun(t.Context(), "t-garbled", []byte("{not json")); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	first.Close(context.WithoutCancel(t.Context()))

	second := r.boot(t)
	t.Cleanup(func() { second.Close(context.WithoutCancel(t.Context())) })
	if after := r.claim(t, second, "node-0/next"); after.Epoch != 3 {
		t.Fatalf("the first claim beside an unreadable record minted %d, want 3: the "+
			"readable run's epoch still floors the seat", after.Epoch)
	}
}
