package statelog

import (
	"context"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/store"
)

// writesNoRow reports whether rec is a record whose whole transaction would be
// the checkpoint: a BARRIER this node authenticated, at the version every
// build reads, declaring the framework's scope, on a subject the log arbitrates
// nothing on. Such a record changes no row on any node ([FrameworkScope]), so
// this node's rows are as current past it the moment it is decoded as they are
// once its checkpoint commits ([Runner.Reached]).
//
// EVERY TERM IS A ROW ITS TRANSACTION WOULD OTHERWISE WRITE, which is why each
// is asked rather than the kind alone:
//
//   - A record under a key this node does not hold is RETAINED, a row in the
//     deferral index ([Runner.applyRun]); so is one above the version this
//     build reads. [BarrierVersion] never moves, and a barrier at any other
//     version is not one this framework appended.
//   - The trimmed record is the other framework record, and it removes this
//     log's anchors below its floor in its own transaction (ADR-0050).
//   - A scope other than the framework's is a claim the record makes some
//     object stale, and a subject the log arbitrates on advances an anchor.
//     The framework appends neither on a barrier ([ReadIndex.Read]), so a
//     record that carries one is not a barrier this predicate may vouch for.
//
// Whether the node holds a retained record at all is the caller's term
// ([Runner.reach]): a barrier's scope meets a retained barrier's, and a record
// on stale rows is retained too.
func writesNoRow(rec Record, arbitrated []string) bool {
	return rec.Kind == BarrierKind && rec.verdict == Verified && rec.V == BarrierVersion &&
		slices.Equal(rec.Scope.Paths, []string{FrameworkScope}) &&
		!arbitrates(arbitrated, rec.Subject.Kind)
}

// reachedOver is how far the records run carries move a reached position:
// over every record next in the log after it that writes no row
// ([writesNoRow]), and no further.
//
// # Why it stops where it does
//
// The reached position's whole claim is that every record at or below it is
// committed or writes no row, so it moves one sequence at a time and STOPS:
//
//   - AT A RECORD THAT WRITES A ROW, which is reached only by its commit
//     ([Runner.advance]). A read released before the commit would be served
//     from rows the transaction then rolls back or has not written.
//   - AT A HOLE. The next sequence not in hand is a record nobody has
//     decoded, which may write anything. On a strict log the reorder buffer
//     keeps whatever arrived above one out of the run; on a compacted log a
//     missing sequence is ordinary and indistinguishable from one not yet
//     delivered, so the position moves there only with a commit.
//   - AT A GENERATION IT IS NOT IN, which no record of this loop's carries:
//     every record is placed at the checkpoint's generation ([Runner.decode]).
//
// A record AT OR BELOW the position is a redelivery of one already committed
// or reached, and is stepped over rather than stopped at — a run carries those
// on purpose, so the loop acknowledges them. The scan is in run order, which
// is how the records next in the log arrive: each fetch's records are admitted
// past the ones before them, in order ([reorderBuffer.admit]).
//
// A PURE FUNCTION of its arguments, so each rule above is a case of its own
// test rather than a branch reachable only through a running loop.
func reachedOver(reached Position, run []Record, arbitrated []string) Position {
	for _, rec := range run {
		switch at := rec.Position; {
		case at.Packed() <= reached.Packed():
		case at.Stream == reached.Stream && at.Generation == reached.Generation &&
			at.Seq == reached.Seq+1 && writesNoRow(rec, arbitrated):
			reached = at
		default:
			return reached
		}
	}
	return reached
}

// checkpoints is the ONE checkpoint-only transaction an apply loop keeps beside
// it, and the reached barriers waiting for the next one ([Runner.Run]).
//
// # Why a run of barriers is not committed on the loop
//
// Its transaction writes no row: its whole effect is the checkpoint and the
// commit's fsync ([writesNoRow]). Committed inline, that fsync stood between
// every record behind it and its DECODE — on a disk whose fsyncs stall for
// seconds, every barrier appended during one waited, undecoded, for a commit
// that changed nothing a read reads, and the reads resting on them were
// refused `behind` at [ReadBudget]. So the loop reaches such a run at once
// ([Runner.reach]) and hands it here.
//
// # One at a time, and the next carries everything since
//
// AT MOST ONE transaction runs, on the loop's own pinned writer: barriers
// handed over while it runs wait for the next one, which starts the moment it
// returns and carries all of them — about one transaction per commit's
// duration however many barriers arrive, a few milliseconds apart on a healthy
// disk and one per stall on a stalled one. That is the coalescing, and it needs
// no constant of its own. Or the loop's next transaction with rows carries them
// ([checkpoints.settle]): the loop waits for the one in flight ONLY before a
// transaction of its own — or before it gives back what it holds after a doubt
// ([Runner.unwind]) — so the writer has one writer at a time and the loop goes
// on decoding meanwhile.
//
// # It verifies nothing
//
// What a batch here needs verified the loop verified before it handed the
// batch over; a doubt since is the loop's to settle, with no transaction here
// in flight ([Runner.verifyBeforeApplying]), and a batch past the checkpoint
// waits for that rather than committing under it ([Runner.mayCommitBeside]).
//
// # What it never changes
//
// Durability. A transaction here is the apply loop's own ([Runner.applyRun]):
// the records' apply, the checkpoint in the same transaction, the commit under
// the store's `synchronous = FULL`, and the acknowledgements only after it. A
// barrier reached and not yet committed is unacknowledged, so a restart
// re-delivers it and the loop reaches it again; the broker's in-flight ceiling
// ([FetchMessages]) bounds how many there are. A failure is the loop's to
// handle, through its retry regime, as a failed transaction of its own is
// ([checkpoints.outcome]).
type checkpoints struct {
	r *Runner

	mu sync.Mutex

	// pending is every reached record no transaction has taken yet, in the
	// order the loop handed them over.
	pending []Record

	// done is closed when the goroutine running this transaction exits, and
	// is nil while none runs: the one fact that keeps two transactions off
	// the writer.
	done chan struct{}

	// halt is set while the loop wants the writer — for its own transaction,
	// or because it is ending — so the goroutine takes no further batch.
	halt bool

	// failed is a transaction's failure the loop has not yet taken. The
	// goroutine exits on one and starts no other until the loop has.
	failed error

	// committed is whether a transaction committed since the loop last
	// asked ([checkpoints.outcome]).
	committed bool
}

// offer hands run — every record of it reached — to the transaction beside the
// loop: to the next one, and to one started now if none runs.
//
// w is the loop's pinned writer, and ctx the loop's own: a transaction here
// ends when the loop's context does, as one on the loop would.
func (c *checkpoints) offer(ctx context.Context, w *store.Writer, run []Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, run...)
	if c.done == nil && c.failed == nil && !c.halt {
		c.done = make(chan struct{})
		go c.drain(ctx, w, c.done)
	}
}

// drain commits what is pending, one transaction at a time, until nothing is,
// the loop wants the writer, a transaction fails, or a doubt the loop has yet
// to settle covers what is pending ([Runner.mayCommitBeside]): the loop gives
// that back itself ([Runner.unwind]).
func (c *checkpoints) drain(ctx context.Context, w *store.Writer, done chan struct{}) {
	defer close(done)
	for {
		c.mu.Lock()
		if c.halt || len(c.pending) == 0 || !c.r.mayCommitBeside(c.pending) {
			c.done = nil
			c.mu.Unlock()
			return
		}
		batch := c.pending
		c.pending = nil
		c.mu.Unlock()

		// NO VERIFICATION HERE, deliberately: a batch is handed over only
		// once the loop has settled any it owed, and one owed since is the
		// loop's to settle, with this transaction finished — run here, it
		// read the checkpoint from before the commit in flight and cleared
		// a doubt the batches it then committed were never compared under.
		consumed, err := c.r.applyRun(ctx, w, batch)

		c.mu.Lock()
		// WHAT THE TRANSACTION DID NOT CONSUME — all of it, where it
		// failed — GOES BACK IN FRONT of what arrived meanwhile, which is
		// behind it on the log.
		c.pending = slices.Concat(batch[len(consumed):], c.pending)
		if err != nil {
			c.failed, c.done = err, nil
			c.mu.Unlock()
			return
		}
		c.committed = true
		c.mu.Unlock()
	}
}

// settle waits for the transaction in flight, if one is, and takes everything
// still pending — for the loop's own transaction to carry, ahead of its run —
// with the failure of the last one, which goes through the loop's regime.
//
// UNBOUNDED BY ANY CONTEXT, deliberately: the loop may not begin a transaction
// on the writer while another is on it, and the one in flight ends when the
// loop's context does.
func (c *checkpoints) settle() ([]Record, error) {
	c.mu.Lock()
	c.halt = true
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halt = false
	taken, failed := c.pending, c.failed
	c.pending, c.failed, c.committed = nil, nil, false
	return taken, failed
}

// outcome is what the transactions beside the loop did since it last asked: a
// failure — with every record pending, which the loop takes back, since the
// goroutine that failed has exited and starts no other — or whether one
// committed.
func (c *checkpoints) outcome() (failed error, taken []Record, committed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed != nil {
		failed, taken = c.failed, c.pending
		c.failed, c.pending = nil, nil
	}
	committed, c.committed = c.committed, false
	return failed, taken, committed
}

// holds reports whether anything is pending or in flight here — whether the
// loop holds records it has not committed, as far as an empty fetch can tell.
func (c *checkpoints) holds() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done != nil || len(c.pending) > 0 || c.failed != nil
}

// stop waits for the transaction in flight and takes no further batch: the
// loop is ending, and the writer it runs on is about to be closed.
func (c *checkpoints) stop() {
	c.mu.Lock()
	c.halt = true
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// unwind gives back to the broker every record this loop holds past the moment
// it last trusted the log ([Runner.doubtLocked]) — held, the records in its own
// hands; the reorder buffer's; and those waiting beside it, once the
// checkpoint transaction in flight there has returned — and leaves the reached
// position at the checkpoint that transaction left. It answers that
// transaction's failure, if it failed, for the loop's regime; its records are
// given back with the rest.
//
// # Why everything, and not only what a verification finds wrong
//
// A verification compares ONE record, the checkpoint's, and what it can vouch
// for is that the log is this node's history UP TO there. The records held past
// it were delivered before the doubt, and a restored log may hold others at
// their sequences: kept, they were applied past a verification that never
// looked at them, and the log's own records at the same sequences were passed
// through as redeliveries of them and dropped — rows from two histories in one
// copy, which no later reading can find. Nothing cheaper vouches for them
// either: the record at the top of what the loop holds proves nothing about
// one below it from another delivery. So all of it goes back, and the broker,
// as it now is, delivers its own records again, which the next verification is
// compared before — at the cost, after a fetch that only blipped, of handing
// over again what it held.
//
// # Why the transaction beside the loop is waited for first
//
// It is committing a batch the loop handed over before the doubt, and cannot
// be called back: the checkpoint it leaves is the one the verification has to
// compare, or a barrier committed from the old history becomes the base the
// new one is admitted against. A batch it had not yet taken is given back with
// the rest, which [Runner.mayCommitBeside] keeps it from taking meanwhile.
func (r *Runner) unwind(ctx context.Context, beside *checkpoints, held []Record,
	buffer *reorderBuffer) error {

	taken, failed := beside.settle()
	back := slices.Concat(taken, held, buffer.held)
	*buffer = reorderBuffer{}
	r.mu.Lock()
	// AT THE CHECKPOINT THE TRANSACTION LEFT, which may have moved it: the
	// doubt put the reached position back at the checkpoint as it stood, and
	// a commit since moves both together.
	r.reached = r.cursor
	at := r.cursor
	r.mu.Unlock()
	if len(back) > 0 {
		r.logger.InfoContext(ctx, "statelog_apply_unwound",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"position", at.String(), "records", len(back),
			"detail", "the records this node held past its checkpoint went back to "+
				"the broker unapplied, because the log may since be another "+
				"history — a fetch failed, or the log's end was read below what "+
				"the node held; the broker delivers them again, and the record at "+
				"the checkpoint is compared with the log's before anything past it "+
				"is applied")
	}
	r.giveBack(ctx, back)
	return failed
}

// giveBack hands each record back to the broker unconsumed ([Message.Release]).
//
// A FAILED HAND-BACK IS A REDELIVERY AT THE ACK WINDOW, which the loop waits
// out behind the hole it leaves on a strict log; worth one line naming how
// many, never a stop, and never one line a record — a hand-back fails when the
// connection that just failed a fetch has not come back, and that is every
// record at once.
func (r *Runner) giveBack(ctx context.Context, records []Record) {
	failed := 0
	var first error
	var at Position
	for _, rec := range records {
		if rec.release == nil {
			continue
		}
		if err := rec.release(); err != nil {
			if failed++; first == nil {
				first, at = err, rec.Position
			}
		}
	}
	if first != nil {
		r.logger.WarnContext(ctx, "statelog_release_failed",
			"domain", r.domain.Name(), "stream", r.spec.Name, "records", failed,
			"position", at.String(), "error", first.Error(),
			"detail", "records given back to the broker unapplied could not be "+
				"handed back, so it delivers them again only once their "+
				"acknowledgement window passes; position is the first of them")
	}
}
