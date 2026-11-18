package statelog

import (
	"context"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/store"
)

// writesNoRow reports whether rec's transaction is only the checkpoint, so rows
// are current past it once decoded ([Runner.Reached]). Each term excludes a row:
//   - a record not verified, or above [BarrierVersion], is RETAINED in the
//     deferral index ([Runner.applyRun]); a barrier at another version is not
//     this framework's;
//   - a kind other than barrier: the trimmed record removes anchors (ADR-0080);
//   - a scope other than [FrameworkScope] claims an object stale, and an
//     arbitrated subject advances an anchor; the framework's barriers carry
//     neither ([ReadIndex.Read]).
//
// Whether the node retains anything is [Runner.reach]'s term: a barrier's scope
// meets a retained barrier's, and a record on stale rows is retained too.
func writesNoRow(rec Record, arbitrated []string) bool {
	return rec.Kind == BarrierKind && rec.verdict == recordseal.Verified && rec.V == BarrierVersion &&
		slices.Equal(rec.Scope.Paths, []string{FrameworkScope}) &&
		!arbitrates(arbitrated, rec.Subject.Kind)
}

// reachedOver moves a reached position over each next record that writes no
// row ([writesNoRow]), one sequence at a time, and STOPS:
//   - at a record that writes a row, reached only by its commit
//     ([Runner.advance]): a read released earlier is served without it;
//   - at a hole, a record nobody decoded that may write anything (on a compacted
//     log a missing sequence looks like one not yet delivered, so only a commit
//     moves past it);
//   - at another generation, which no record of this loop's carries
//     ([Runner.decode]).
//
// A record at or below the position is a redelivery, stepped over. A run is in
// log order ([reorderBuffer.admit]). Pure, so each rule is a test case.
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

// checkpoints is the ONE checkpoint-only transaction an apply loop runs beside
// it, and the reached barriers awaiting it ([Runner.Run]). Such a run writes no
// row ([writesNoRow]), so the loop reaches it at once ([Runner.reach]): inline,
// a stalled fsync held every record behind it undecoded, `behind` at [ReadBudget].
//
// At most one runs, on the loop's pinned writer, the next carrying all arrived
// meanwhile: coalescing with no constant. The loop waits for it only before its
// own transaction ([checkpoints.settle]) or [Runner.unwind]. It verifies
// nothing: the loop settles a doubt with none in flight
// ([Runner.verifyBeforeApplying]). Durability is the loop's ([Runner.applyRun]):
// an uncommitted barrier stays unacknowledged for a restart to re-deliver
// ([FetchMessages] bounds them); a failure takes the loop's regime too.
type checkpoints struct {
	r *Runner

	mu sync.Mutex

	// pending is every reached record no transaction has taken, in hand-over
	// order.
	pending []Record

	// done is closed when the goroutine running a transaction exits, and nil
	// while none runs: what keeps two transactions off the writer.
	done chan struct{}

	// halt is set while the loop wants the writer (its own transaction, or
	// ending), so the goroutine takes no further batch.
	halt bool

	// failed is a failure the loop has not yet taken; no goroutine starts
	// until it has.
	failed error

	// committed is whether a transaction committed since the loop last asked
	// ([checkpoints.outcome]).
	committed bool
}

// offer hands run, every record of it reached, to the next transaction beside
// the loop, starting one if none runs. w is the loop's pinned writer and ctx
// its own, so a transaction here ends with the loop's context.
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
// the loop wants the writer, a transaction fails, or an unsettled doubt covers
// it ([Runner.mayCommitBeside]), which the loop gives back ([Runner.unwind]).
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

		// NO VERIFICATION HERE: a doubt is the loop's to settle once this
		// transaction finishes. Run here, it would read the checkpoint from
		// before the commit in flight and clear a doubt the batches it then
		// committed were never compared under.
		consumed, err := c.r.applyRun(ctx, w, batch)

		c.mu.Lock()
		// What it did not consume (all of it, on failure) goes back IN FRONT
		// of what arrived meanwhile, which is behind it on the log.
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

// settle waits for the transaction in flight and takes everything pending, for
// the loop's own transaction to carry ahead of its run, with the last one's
// failure for the loop's regime. UNBOUNDED BY ANY CONTEXT: the loop may not
// begin a transaction beside another, and that one ends with the loop's context.
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
// failure, with every record pending for the loop to take back (the failed
// goroutine has exited and starts no other), or whether one committed.
func (c *checkpoints) outcome() (taken []Record, committed bool, failed error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed != nil {
		failed, taken = c.failed, c.pending
		c.failed, c.pending = nil, nil
	}
	committed, c.committed = c.committed, false
	return taken, committed, failed
}

// holds reports whether anything is pending or in flight here: records the loop
// holds uncommitted, as far as an empty fetch can tell.
func (c *checkpoints) holds() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done != nil || len(c.pending) > 0 || c.failed != nil
}

// stop waits for the transaction in flight and takes no further batch: the
// loop is ending and its writer is about to close.
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
// it last trusted the log ([Runner.doubtLocked]): its own, the reorder buffer's,
// and those waiting beside it once the transaction in flight there returns,
// leaving the reached position at its checkpoint and answering its failure.
//
// EVERYTHING: a verification vouches only up to the checkpoint's record, and the
// top held record vouches for none below it. A restored log may hold other
// records at the held sequences, dropped as redeliveries of kept ones and
// mixing two histories' rows undetectably. The transaction beside is awaited,
// as it cannot be called back and the verification compares the checkpoint it
// leaves; a batch it had not taken goes back with the rest, which
// [Runner.mayCommitBeside] keeps it from taking meanwhile.
func (r *Runner) unwind(ctx context.Context, beside *checkpoints, held []Record,
	buffer *reorderBuffer) error {

	taken, failed := beside.settle()
	back := slices.Concat(taken, held, buffer.held)
	*buffer = reorderBuffer{}
	r.mu.Lock()
	// AT THE CHECKPOINT THE TRANSACTION LEFT: a commit since the doubt
	// moved both together.
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
// A FAILED HAND-BACK IS A REDELIVERY AT THE ACK WINDOW, waited out behind the
// hole it leaves on a strict log: one line naming how many, never a stop, never
// a line a record, since it fails when the connection that failed a fetch is
// still down, which is every record at once.
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
