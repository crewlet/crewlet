package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// heldBudget bounds every wait on a loop holding a commit: [wiredOpBudget],
// since what a held commit leaves the loop doing takes milliseconds, so
// running it out means the loop stopped, not that it is slow.
const heldBudget = wiredOpBudget

// barrierEnv is the barrier a read index appends ([statelog.ReadIndex.Read]):
// the framework's envelope, no writer, no operation id.
func barrierEnv() statelog.Envelope {
	return statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: 1,
		Scope: statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}},
	}
}

// batches is how many transactions the probe applier has seen commit
// ([statelog.Applier.Committed]).
func (a *probeApplier) batches() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.commits
}

// runningLoop is an applier a case runs beside it.
type runningLoop struct {
	// cancel ends the loop's context and returns at once.
	cancel func()

	// stop cancels the loop, waits for it, and answers how it ended if not
	// by cancellation; it runs once however often called.
	stop func() error
}

// looping runs h's applier until stopped, or the case's cleanup. A case holding
// a commit releases it first: the loop waits for its transaction beside it.
func (h *applyHarness) looping(t *testing.T) *runningLoop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	stopped := make(chan error, 1)
	go func() { stopped <- h.runner.Run(ctx) }()
	l := &runningLoop{cancel: cancel}
	l.stop = sync.OnceValue(func() error {
		cancel()
		if err := <-stopped; !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	t.Cleanup(func() {
		if err := l.stop(); err != nil {
			t.Errorf("the applier stopped with %v", err)
		}
	})
	return l
}

// until polls cond until it holds, failing t with what was awaited if it does
// not within [heldBudget].
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(heldBudget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, heldBudget)
		}
		time.Sleep(time.Millisecond)
	}
}

// ONE CHECKPOINT TRANSACTION IS IN FLIGHT, BESIDE THE LOOP.
//
// With a run of barriers' commit held (a stalled fsync), the loop keeps
// decoding and reaches every barrier behind it; nothing else begins on the
// writer; the next transaction carries all decoded since; a run with rows
// waits for the one in flight; and nothing is acknowledged before its commit.
func TestOneCheckpointTransactionIsInFlight(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	h.fetch.offer(1, env(1, "edit", "o1", "op-1", 1))
	loop := h.looping(t)
	until(t, "the write before the hold", func() bool { return h.runner.Committed().Seq >= 1 })
	before := h.applier.batches()

	hold.Arm()
	// RELEASED ON EVERY WAY OUT, before the loop stops: it waits for the
	// transaction beside it, which would wait on the hold for ever.
	defer hold.Release()
	h.fetch.offer(2, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barrier's checkpoint never reached its commit: %v", err)
	}
	h.fetch.offer(3, barrierEnv())
	h.fetch.offer(4, barrierEnv())
	until(t, "the loop reaching the barriers behind a held checkpoint", func() bool {
		return h.runner.Reached().Seq == 4
	})
	if begun := hold.Begun(); begun != 1 {
		t.Errorf("%d write transactions began while one checkpoint was held, want 1 — "+
			"the writer has one transaction on it at a time", begun)
	}
	if got := h.runner.Committed().Seq; got != 1 {
		t.Errorf("the committed checkpoint is %d while the barriers' commit is held, want 1", got)
	}
	for seq := uint64(2); seq <= 4; seq++ {
		if n := h.fetch.ackCount(seq); n != 0 {
			t.Errorf("the barrier at %d was acknowledged %d time(s) before any commit "+
				"checkpointed it — a restart now would never be handed it again", seq, n)
		}
	}

	hold.Release()
	until(t, "the checkpoint catching up once released", func() bool {
		return h.runner.Committed().Seq == 4
	})
	if got := h.applier.batches() - before; got != 2 {
		t.Errorf("%d transactions committed barriers 2 to 4, want 2 — the held one, "+
			"and ONE after it carrying both barriers decoded meanwhile", got)
	}
	for seq := uint64(2); seq <= 4; seq++ {
		until(t, fmt.Sprintf("the barrier at %d acknowledged once committed", seq),
			func() bool { return h.fetch.ackCount(seq) == 1 })
	}

	// A RUN WITH ROWS WAITS FOR THE ONE IN FLIGHT.
	before = h.applier.batches()
	hold.Arm()
	h.fetch.offer(5, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the second barrier's checkpoint never reached its commit: %v", err)
	}
	h.fetch.offer(6, env(6, "edit", "o6", "op-6", 1))
	until(t, "the loop taking the write behind the held checkpoint", func() bool {
		pending, _ := h.fetch.Pending(t.Context())
		return pending == 0
	})
	// A WHILE, for a loop that did not wait to begin: one that waits shows
	// nothing.
	time.Sleep(50 * time.Millisecond)
	if slices.Contains(h.applier.seen(), statelog.Position{Stream: probeStream, Generation: 1, Seq: 6}) {
		t.Errorf("the write at 6 was applied while the checkpoint before it was held — " +
			"a transaction began on the writer beside the one in flight")
	}
	if got := h.runner.Reached().Seq; got != 5 {
		t.Errorf("the loop reached %d while the write at 6 was uncommitted, want 5", got)
	}
	hold.Release()
	until(t, "the write committed once the checkpoint before it returned", func() bool {
		return h.runner.Committed().Seq == 6
	})
	if got := h.applier.batches() - before; got != 2 {
		t.Errorf("%d transactions committed the barrier at 5 and the write at 6, "+
			"want 2: the checkpoint, then the loop's own", got)
	}
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}
	for seq := uint64(5); seq <= 6; seq++ {
		if n := h.fetch.ackCount(seq); n != 1 {
			t.Errorf("the record at %d was acknowledged %d time(s), want once", seq, n)
		}
	}
}

// A CRASH PAST THE COMMITTED CHECKPOINT LOSES NOTHING.
//
// The reached position runs ahead of the checkpoint over uncommitted barriers,
// and nothing durable may read it: a node stopping there restarts at the
// checkpoint, is handed the unacknowledged barriers again, and ends with the
// rows of a node that never stopped. Its stance ([statelog.Runner.Stance],
// which the heartbeat publishes and the trim counts) never passes the
// checkpoint, or a trim could purge the barriers the restart needs.
func TestACrashPastTheCommittedCheckpointLosesNothing(t *testing.T) {
	t.Parallel()
	offerAll := func(h *applyHarness, from, to uint64) {
		for seq := from; seq <= to; seq++ {
			if seq <= 3 {
				h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq), fmt.Sprintf("op-%d", seq), 1))
				continue
			}
			h.fetch.offer(seq, barrierEnv())
		}
	}
	// THE NODE THAT NEVER STOPS, whose rows the other's must match.
	control := newApplyHarness(t, probeDomain{})
	offerAll(control, 1, 5)
	if err := control.run(5); err != nil {
		t.Fatalf("the control node: %v", err)
	}

	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	offerAll(h, 1, 3)
	loop := h.looping(t)
	until(t, "the writes before the hold", func() bool { return h.runner.Committed().Seq >= 3 })
	hold.Arm()
	defer hold.Release()
	offerAll(h, 4, 5)
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barriers' checkpoint never reached its commit: %v", err)
	}
	until(t, "the loop reaching both barriers", func() bool { return h.runner.Reached().Seq == 5 })
	durable, found, err := statelog.CheckpointOf(t.Context(), h.estate, probeStream)
	if err != nil || !found {
		t.Fatalf("read the checkpoint row: (%v, %v)", found, err)
	}
	if stance := h.runner.Stance(); stance.At != durable.At || stance.At.Seq != 3 {
		t.Fatalf("the runner's stance is %s while its checkpoint row holds %s, want both "+
			"at 3 — what the heartbeat publishes past the checkpoint is a position a "+
			"restart does not resume at", stance.At, durable.At)
	}

	// THE CRASH: the loop ends and its commit never lands. CANCELLED FIRST,
	// so the failed commit is never retried: the process that would is dead.
	loop.cancel()
	hold.ReleaseFailing(errors.New("the process died before the commit returned"))
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}
	for seq := uint64(4); seq <= 5; seq++ {
		if n := h.fetch.ackCount(seq); n != 0 {
			t.Fatalf("the barrier at %d was acknowledged %d time(s) and never committed — "+
				"a restart is never handed it again", seq, n)
		}
	}

	// THE RESTART, at the committed checkpoint, handed the barriers again.
	h.rebuild(probeDomain{}, time.Time{})
	if got := h.runner.Reached(); got.Seq != 3 {
		t.Fatalf("the restarted runner stands at %s, want the committed checkpoint 3", got)
	}
	offerAll(h, 4, 5)
	if err := h.run(5); err != nil {
		t.Fatalf("the restarted node: %v", err)
	}
	if got, want := probeRows(t, h), probeRows(t, control); !slices.Equal(got, want) {
		t.Errorf("the node that crashed past its checkpoint holds rows at %v, and the "+
			"node that never stopped at %v", got, want)
	}
}

// A NODE THAT RETAINS A RECORD REACHES ONLY WHAT IT COMMITS.
//
// Every barrier declares the framework's one scope, so a barrier meets any
// retained barrier's scope and is retained too, which is a row: while this node
// retains anything its reached position moves only with commits. Here a
// barrier under a lacked key is retained, and the readable one after it is
// retained behind it in a transaction held at its commit.
func TestANodeThatRetainsARecordReachesOnlyWhatItCommits(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	h.fetch.offer(1, env(1, "edit", "o1", "op-1", 1))
	h.fetch.offerFramed(2, sealedUnder(t, "k-lacked", barrierEnv()))
	loop := h.looping(t)
	until(t, "the barrier under a lacked key retained", func() bool {
		return h.runner.Committed().Seq >= 2 && h.retainedCount() == 1
	})

	hold.Arm()
	defer hold.Release()
	h.fetch.offer(3, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barrier behind the retained one never reached its commit: %v", err)
	}
	if got := h.runner.Reached().Seq; got != 2 {
		t.Errorf("the reached position is %d while the transaction retaining the barrier "+
			"at 3 is held, want 2", got)
	}
	hold.Release()
	until(t, "the barrier behind the retained one committed", func() bool {
		return h.runner.Committed().Seq == 3
	})
	if n := h.retainedCount(); n != 2 {
		t.Errorf("this node retains %d record(s), want 2 — the barrier it reads was "+
			"retained behind the one it does not, which is the row its transaction wrote", n)
	}
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}
}

// NO READ WAITS ON A COMMIT THAT WRITES NO ROW.
//
// Three linearizable reads back to back on a real broker and store, while the
// checkpoint of the first read's barrier is held at its commit: the second's
// barrier follows the first's, the third's one that arrived during the hold.
// Each is served with every row and none waits on the held commit, which would
// leave it `behind` at [statelog.ReadBudget]; the checkpoint stays behind, and
// catches up with all three once it returns.
func TestNoReadWaitsOnACommitThatWritesNoRow(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	n := running(newWiredNodeWith(t, soloBroker(t, js.Config{SyncAlways: true}), "node-r",
		store.Options{WrapDriver: hold.Wrap}))
	ctx, cancel := context.WithTimeout(t.Context(), wiredOpBudget)
	defer cancel()
	const writes = 3
	var written statelog.Position
	for i := range writes {
		res, err := n.write(ctx, probeSubject(fmt.Sprintf("row-%d", i)), statelog.Position{})
		if err != nil {
			t.Fatalf("write %d before the hold: %v", i+1, err)
		}
		written = res.Position
	}

	hold.Arm()
	// RELEASED ON EVERY WAY OUT, before the node's cleanup stops an applier
	// that would wait on the held commit for ever.
	defer hold.Release()
	reader := n.reader(t, n.runner)
	for i := range 3 {
		var rows int
		started := time.Now()
		answer, err := reader.Read(ctx, statelog.Query{
			Level: statelog.ReadLinearizable,
			Scope: statelog.ScopeSet{Paths: []string{probeSubject("row-0").String()}},
		}, func(tx *sql.Tx) error {
			var err error
			rows, err = objects(ctx, tx)
			return err
		})
		took := time.Since(started)
		if err != nil {
			t.Fatalf("read %d of three, behind a checkpoint held at its commit, after %s: %v",
				i+1, took.Round(time.Millisecond), err)
		}
		// THE FIGURE [statelog.ReadBudget]'s comment quotes: a read behind a
		// commit held for as long as the case runs.
		t.Logf("read %d of three served in %s behind the held checkpoint", i+1, took)
		if rows != writes {
			t.Errorf("read %d saw %d writes, want all %d", i+1, rows, writes)
		}
		if answer.Position.Packed() < written.Packed() {
			t.Errorf("read %d was answered at %s, below the last write at %s",
				i+1, answer.Position, written)
		}
		if i == 0 {
			if err := hold.AwaitHeld(ctx, 1); err != nil {
				t.Fatalf("the first read's barrier never reached its commit: %v", err)
			}
		}
	}
	if got := n.runner.Committed(); got.Packed() > written.Packed() {
		t.Errorf("the committed checkpoint moved to %s past the last write at %s while "+
			"the only commit after it is held", got, written)
	}
	_, last, err := n.log.Bounds(ctx)
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if last != written.Seq+3 {
		t.Fatalf("the log ends at %d, want the last write at %d and one barrier for each "+
			"read after it", last, written.Seq)
	}

	hold.Release()
	until(t, "the checkpoint reaching the third read's barrier once released", func() bool {
		return n.runner.Committed().Seq == last
	})
}

// A READ PAST ANOTHER NODE'S BARRIER IS SERVED.
//
// Every data node applies every node's barriers, so the record before a read's
// barrier is often another node's: node B appends two while node A's
// checkpoint of the first is held, and A's read, its barrier after B's second,
// is served. Passing only a node's own barriers would leave A on that fsync.
func TestAReadPastAnotherNodesBarrierIsServed(t *testing.T) {
	t.Parallel()
	q := soloBroker(t, js.Config{SyncAlways: true})
	hold := storetest.HoldCommits(t)
	a := running(newWiredNodeWith(t, q, "node-a", store.Options{WrapDriver: hold.Wrap}))
	b := running(newWiredNode(t, q, "node-b"))
	ctx, cancel := context.WithTimeout(t.Context(), wiredOpBudget)
	defer cancel()
	subject := probeSubject("seen")
	written, err := a.write(ctx, subject, statelog.Position{})
	if err != nil {
		t.Fatalf("a write before the hold: %v", err)
	}

	hold.Arm()
	defer hold.Release()
	first, err := b.barrier(ctx)
	if err != nil {
		t.Fatalf("node B's first barrier: %v", err)
	}
	if err := hold.AwaitHeld(ctx, 1); err != nil {
		t.Fatalf("node A's checkpoint of B's barrier never reached its commit: %v", err)
	}
	second, err := b.barrier(ctx)
	if err != nil {
		t.Fatalf("node B's second barrier: %v", err)
	}
	var rows int
	if _, err := a.reader(t, a.runner).Read(ctx, statelog.Query{
		Level: statelog.ReadLinearizable,
		Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
	}, func(tx *sql.Tx) error {
		var err error
		rows, err = objects(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("node A's read, its barrier after B's while A's checkpoint of B's is "+
			"held: %v", err)
	}
	if rows != 1 {
		t.Errorf("node A's read saw %d writes, want the one", rows)
	}
	_, last, err := a.log.Bounds(ctx)
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if first.Seq != written.Position.Seq+1 || last != second.Seq+1 {
		t.Fatalf("the log holds the write at %d, B's barriers at %d and %d and ends at "+
			"%d: the read's barrier does not follow B's second", written.Position.Seq,
			first.Seq, second.Seq, last)
	}
	if got := a.runner.Committed(); got.Packed() > written.Position.Packed() {
		t.Errorf("node A's checkpoint moved to %s past its write at %s while its "+
			"commit of B's barrier is held", got, written.Position)
	}
}
