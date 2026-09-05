package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// Applier turns one document change into rows, inside the projector's own
// transaction.
//
// DECLARED HERE, implemented by the packages that own the documents:
// [internal/work] knows what an item's classes mean and [internal/pages]
// knows what a page's do, and neither belongs in a package whose subject is
// keeping a local copy in step. What this package guarantees in return is
// the part an applier must not have to think about — exactly-once by
// revision, one writer, a transaction per batch, and a cursor that advances
// only after a commit.
//
// Two rules bind every implementation, because the plumbing above depends on
// them:
//
//   - APPLY IS IDEMPOTENT. The same change may arrive twice — a crash between
//     the batch commit and the cursor write replays it, and a boot reconcile
//     re-applies whatever it re-fetched. An applier that appended rather than
//     upserted would double a comment thread on an ordinary restart.
//   - APPLY IS A PURE FUNCTION OF (row, change). It may read this
//     transaction's own tables and nothing else — no network, no clock it
//     stores, no coordination read. A projector rebuilding from scratch
//     replays the same changes in the same order and must reach the same
//     rows, or a rebuild is a different projection.
type Applier interface {
	// Family is which family this applier serves.
	Family() Family

	// Apply writes one change. A [coord.OpPurge] change removes the record
	// and everything derived from it; a [coord.OpPut] upserts.
	//
	// A change whose key this applier does not recognise is IGNORED, not an
	// error: a newer node writes classes this build has no rule for, and a
	// rolling upgrade must not wedge the older half's projector.
	Apply(ctx context.Context, tx *sql.Tx, change coord.Change) error

	// Reset drops every row this applier owns, for a rebuild.
	Reset(ctx context.Context, tx *sql.Tx) error

	// Committed runs after a batch's transaction has COMMITTED, for the
	// side effects an apply cannot take inside one.
	//
	// The projector is the only thing that knows a batch landed, and some
	// consequences of a change are not rows: the tool-skill registry has
	// to re-read its container when a skill page moves, and nothing else
	// sees that happen — the change feed deliberately drops those changes
	// rather than waking a team about a procedure written for one phase of
	// one turn.
	//
	// AFTER THE COMMIT, never inside it, for two reasons that both bite: a
	// callback running mid-transaction reads rows the batch has not
	// committed, and a slow one holds the projection's write lock while
	// every other change queues behind it.
	//
	// It is called on EVERY committed batch, not only interesting ones —
	// deciding what is interesting is the applier's own business, and a
	// projector that tried would need the applier's grammar.
	Committed(ctx context.Context)

	// Order ranks a key for a batch, lower first.
	//
	// # Why the projector cannot decide this itself
	//
	// A boot reconcile enumerates the bucket's keys in MAP ORDER, and a
	// family's records have parents: a comment's row references its item's,
	// a revision's references its page's. An applier that skipped a child
	// whose parent was not there yet would drop it PERMANENTLY — the key
	// set records the child as applied at that revision, so no later
	// reconcile re-fetches it and nothing anywhere says a thread is short.
	// That is measured, not hypothetical: a twenty-comment item projected
	// twelve of them on a fresh node before this existed.
	//
	// The projector cannot fix that on its own, because which key is whose
	// parent is the applier's own grammar. So the applier ranks, and the
	// projector sorts each batch by (rank, revision) before applying —
	// which makes an apply's precondition "my parent is either already
	// here or earlier in this same transaction".
	//
	// A rank is a small integer and ties keep revision order, so an applier
	// with no hierarchy returns a constant and loses nothing.
	Order(key string) int
}

// Documents is the coordination surface a projector reads.
//
// A NARROWED [coord.Documents]: the projector watches, fetches one key at an
// exact revision, and does nothing else. Declaring the three methods it uses
// rather than taking the whole interface is what makes the fake in this
// package's tests three methods instead of nine.
type Documents interface {
	DocumentAt(ctx context.Context, family coord.Family, key string, revision uint64) (coord.Record, bool, error)
	WatchDocuments(ctx context.Context, family coord.Family, from uint64) (coord.Watcher, error)
}

// bufferBytes bounds one family's in-flight change buffer.
//
// BOUNDED BY BYTES, not by entries, and that is the whole reason it is a
// constant worth explaining: a page body is capped at 512 KiB, so a
// thousand-entry buffer is anywhere from 100 KB to half a gigabyte depending
// on what happens to be flowing through it. Sixty-four mebibytes holds a
// hundred and twenty-eight worst-case pages or something like a hundred
// thousand ordinary changes, which is far more than the drain can fall behind
// by while a batch commits.
//
// Overflowing it is not a gap to tolerate. The drain records the drop and the
// projector rebuilds, because a projection missing writes it cannot name is a
// board with items silently absent from it.
const bufferBytes = 64 << 20

// Projector keeps one family's projection in step with its bucket.
//
// One per family, because the write side is one goroutine per family: the
// driver's BeginTx issues a plain BEGIN, so two writers racing over one row
// take two snapshots and one loses — and [store.DB.Tx] retrying that would
// re-run an apply against a row the other writer has since moved. A single
// writer is what makes an apply a pure function of (row, change).
type Projector struct {
	docs    Documents
	db      *store.DB
	applier Applier
	family  Family

	// buf is the drain's handoff to the writer. The drain NEVER touches the
	// store: a full watcher channel blocks the read loop of this node's one
	// NATS connection, and with it every seat's mailbox.
	buf *buffer

	mu       sync.Mutex
	hydrated bool
	cursor   uint64
	lastErr  error
	lastAt   time.Time

	// applied broadcasts a cursor advance to WaitApplied. A condition
	// variable rather than a channel per waiter: waiters are unbounded (one
	// per in-flight write) and each wants the same edge, which is what a
	// broadcast is.
	applied *sync.Cond

	stop  context.CancelFunc
	done  chan struct{}
	watch coord.Watcher
}

// Options configure a projector.
type Options struct {
	Documents Documents
	DB        *store.DB
	Applier   Applier
}

// New builds a projector. It reads nothing and starts nothing: [Projector.Run]
// does the boot reconcile, and a caller that has not called it must not read.
func New(opts Options) (*Projector, error) {
	switch {
	case opts.Documents == nil:
		return nil, errors.New("projection: a document store is required")
	case opts.DB == nil:
		return nil, errors.New("projection: a store is required")
	case opts.Applier == nil:
		return nil, errors.New("projection: an applier is required")
	}
	family := opts.Applier.Family()
	if !family.Valid() {
		return nil, coord.ErrUnknownFamily(family)
	}
	p := &Projector{
		docs:    opts.Documents,
		db:      opts.DB,
		applier: opts.Applier,
		family:  family,
		buf:     newBuffer(bufferBytes),
		done:    make(chan struct{}),
	}
	p.applied = sync.NewCond(&p.mu)
	return p, nil
}

// Family is which family this projector serves.
func (p *Projector) Family() Family { return p.family }

// Status is what this projector reports to the node status.
func (p *Projector) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{
		Family:      p.family,
		Hydrated:    p.hydrated,
		Revision:    p.cursor,
		Pending:     p.buf.Len(),
		Dropped:     p.buf.Dropped(),
		LastApplied: p.lastAt,
	}
	if p.lastErr != nil {
		s.Err = p.lastErr.Error()
	}
	return s
}

// Hydrated reports whether the boot reconcile has finished.
//
// THE FACT A MAILBOX WAITS ON, and deliberately not the cursor: see the
// package doc for the three ways a plausible cursor sits over an empty
// projection for ever.
func (p *Projector) Hydrated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hydrated
}

// Cursor is the last revision applied here.
func (p *Projector) Cursor() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor
}

// WaitApplied blocks until this node has applied revision, or the budget
// expires.
//
// THE READ-YOUR-WRITES PRIMITIVE, and it is used on the WRITE path only: a
// REST call or a tool call waits for its own revision before answering, so a
// person who files an item and lands on the board sees it. A WAKE NEVER
// WAITS — a wait inside a seat's serialised inbox handler would block every
// other conversation on that mailbox while one record caught up, and a seat
// whose projector had wedged would stop answering anybody. The wake path
// carries its revision instead and reads through to coordination for that one
// record.
//
// It promises "revision N or newer", never a bound: a coordination direct get
// is not read-your-writes on a replicated stream, so no path here may turn
// this into an assertion that a subsequent read will see the value.
func (p *Projector) WaitApplied(ctx context.Context, revision uint64) error {
	if revision == 0 {
		return nil
	}
	ctx, cancel := waitCtx(ctx)
	defer cancel()

	// The waiter is woken by a broadcast OR by the context ending, and a
	// condition variable knows nothing about contexts — so a watcher
	// goroutine turns the deadline into a broadcast. It exits with the
	// wait, so a burst of waiters costs a goroutine each only while they
	// are actually waiting.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.applied.Broadcast()
			p.mu.Unlock()
		case <-stopped:
		}
	}()

	p.mu.Lock()
	defer p.mu.Unlock()
	for p.cursor < revision {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %s is at revision %d, waiting for %d",
				ErrRevisionTooNew, p.family, p.cursor, revision)
		}
		p.applied.Wait()
	}
	return nil
}

// Run reconciles this node against the bucket, then follows it until ctx ends.
//
// IT RETURNS ONLY ON FAILURE OR SHUTDOWN. A caller starts it in a goroutine
// and gates seat acquisition on [Projector.Hydrated] rather than on this
// returning, because it does not return while it is working.
func (p *Projector) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.stop = cancel
	p.mu.Unlock()
	defer cancel()
	defer close(p.done)

	for {
		err := p.cycle(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			// A clean end with a live context means the watch closed
			// under us — a broker reconnect, a bucket recreated. Re-enter
			// through the reconcile rather than resuming the cursor: the
			// bucket may be a different one now, and resuming into it is
			// exactly the silent-freeze case.
			err = errors.New("projection: the document watch closed")
		}
		p.fail(err)
		log.WarnContext(ctx, "projection_cycle_restart", "family", string(p.family),
			"error", err.Error(),
			"detail", "re-running the boot reconcile; a resumed cursor cannot "+
				"tell a reconnected bucket from a recreated one")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cycleBackoff):
		}
	}
}

// cycleBackoff is the pause between a failed follow and the next reconcile.
//
// Two seconds: long enough that a broker election or a reconnect completes
// inside one pause rather than being retried through, short enough that a
// node is not serving stale reads for a noticeable time after a blip. The
// reconcile that follows is idempotent, so the cost of an early retry is a
// metadata pass rather than a wrong answer.
const cycleBackoff = 2 * time.Second

// Stop ends the projector and waits for it.
func (p *Projector) Stop() {
	p.mu.Lock()
	stop := p.stop
	p.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-p.done
}

// cycle is one reconcile-then-follow pass.
func (p *Projector) cycle(ctx context.Context) error {
	if err := p.reconcile(ctx); err != nil {
		return err
	}
	return p.follow(ctx)
}

// fail records a failure for the status surface and drops hydration.
//
// HYDRATION DROPS WITH IT, which is the load-bearing half: a projector that
// has stopped following is a projector whose rows are going stale, and a
// mailbox that attached on the strength of an earlier hydration would keep
// answering from them. Dropping it stops new seats being claimed against a
// projection nobody is maintaining.
func (p *Projector) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastErr = err
	p.hydrated = false
}

// advance records a committed batch and wakes anyone waiting for it.
func (p *Projector) advance(revision uint64, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if revision > p.cursor {
		p.cursor = revision
	}
	p.lastAt = at
	p.lastErr = nil
	p.applied.Broadcast()
}

// markHydrated flips the fact a mailbox waits on.
func (p *Projector) markHydrated(cursor uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hydrated = true
	if cursor > p.cursor {
		p.cursor = cursor
	}
	p.lastErr = nil
	p.applied.Broadcast()
}
