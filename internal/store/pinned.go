package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Writer is a connection PINNED to one caller for its lifetime, and the
// transaction path a long-lived writer uses instead of [DB.Tx].
//
// # Why a pin at all
//
// The pool is bounded on purpose (see [defaultReaderConns]), and every reader
// on this node draws from it: the dashboard's queries, the coverage probes, a
// seat's tool reads. A writer that takes a pooled connection per transaction
// competes with them for one — and the competition has a direction that makes
// it worse than fair sharing. The readers are usually waiting on state THIS
// writer is about to commit, so under load they occupy every connection while
// the writer queues behind them for the one it needs to unblock them. That is
// a feedback loop rather than a deadlock: the waiters cause the latency they
// are waiting on, and nothing bounds it.
//
// A pin removes the writer from that competition entirely. It costs one
// connection for the handle's life, which is why [Options.PinnedWriters] is
// declared by the caller and added to the pool rather than taken out of it.
//
// # The pool's arithmetic, and the second thing that is not the readers'
//
// The whole bound is:
//
//	pool = defaultReaderConns() + identityReserve + PinnedWriters
//
// with the pins held by their writers, [identityReserve] held by the handle
// itself ([DB.reserve]), and what is left the readers'.
//
// [identityReserve] is a pin's SIBLING INVARIANT: one connection that ordinary
// work may not occupy, kept for the lookups that answer "who is acting". It is
// HELD for the same reason a pin is — database/sql hands connections out
// first-come-first-served, so a spare one nobody holds is taken by the first
// read burst that wants it, and adding one to the bound alone would buy
// nothing at all.
//
// THE OTHER DESIGN, an admission semaphore in front of the pool, breaks
// something a pin does not: it would bound ordinary work BELOW the pool, so
// database/sql would never reach its own limit, `sql.DBStats.WaitCount` would
// stop counting, and the `pool_starved` alarm — whose entire input is that
// counter — would go quiet for good. Holding a connection moves no queue.
// Ordinary work still waits exactly where it waited before.
//
// The failure it stops has the same shape as the one a pin stops, one layer
// up. A socket storm — N dashboards, four concurrent queries each, every one a
// scan — takes every connection; the identity read that would let those very
// requests be decided queues behind all of them; and the queue feeds itself.
//
// # It is not a second transaction implementation
//
// Tx below takes the SAME place in the SAME queue, begins the SAME transaction
// through [DB.writeOn] and carries the SAME retry loop and classifier [DB.Tx]
// does, because a second copy is how one of them comes to retry an error the
// other returns, or to take the write lock in a way the other does not. The
// only difference is which connection the transaction begins on.
//
// The pin keeps a writer out of the POOL's competition, not out of the
// QUEUE's: every write transaction on this file, pinned or pooled, is served
// in the order it asked for the lock (see writequeue.go).
//
// A Writer is NOT safe for concurrent use: it is one connection, and its
// owner is one goroutine. Two goroutines sharing one would interleave
// statements inside each other's transactions. That is also what makes the
// unguarded fields below safe: only that one goroutine reads or writes them.
type Writer struct {
	db *DB
	// conn is the pinned connection. REPLACED rather than kept when a
	// transaction on it fails to end — see Tx — and nil only when that
	// replacement could not be had.
	conn *sql.Conn
	// retire is conn's own retire switch, captured at the draw for the
	// reason [retireSwitch] gives. It travels with conn and is replaced
	// with it.
	retire func()
	// closed is the handle's own state, kept separately from conn because
	// conn is legitimately nil on a live writer. It was the closed marker
	// once, and a writer that had lost its connection then released its
	// pin twice and accepted a Tx after Close.
	closed bool
}

// ErrWriterClosed is a transaction asked of a [Writer] that has been closed.
//
// Its own sentinel because a closed writer is a CALLER's mistake with an
// obvious remedy, unlike [ErrNoEstate], which is a state a correct caller can
// legitimately be in. It used to be a nil dereference: [Writer.Close] marked
// itself closed by nilling the connection, and a connection is now
// legitimately nil on a live writer.
var ErrWriterClosed = errors.New("store: this pinned writer is closed")

// Writer pins a connection and returns a handle that owns it until Close.
//
// REFUSED PAST THE DECLARED COUNT, naming it. The pool was sized as readers
// plus [Options.PinnedWriters], so an undeclared pin is not a tight fit — it
// is a reader's connection taken with nothing reporting the loss, and the
// symptom (a dashboard that queues) appears nowhere near the cause.
func (d *DB) Writer(ctx context.Context) (*Writer, error) {
	if d == nil || d.sql == nil {
		return nil, ErrNoEstate
	}
	d.pins.mu.Lock()
	if d.pins.held >= d.pins.declared {
		declared := d.pins.declared
		d.pins.mu.Unlock()
		return nil, fmt.Errorf(
			"store: this handle declared %d pinned writer(s) and %d are held: "+
				"raise store.Options.PinnedWriters to the number of statelog "+
				"domains this node runs, so the pool is sized for them",
			declared, declared)
	}
	d.pins.held++
	d.pins.mu.Unlock()

	conn, err := d.sql.Conn(ctx)
	if err != nil {
		d.pins.mu.Lock()
		d.pins.held--
		d.pins.mu.Unlock()
		return nil, fmt.Errorf("store: pin a writer connection: %w", err)
	}
	return &Writer{db: d, conn: conn, retire: retireSwitch(conn)}, nil
}

// Tx runs fn inside a transaction on the pinned connection, with [DB.Tx]'s
// retry and rollback semantics exactly — including that fn MAY RUN MORE THAN
// ONCE, so anything with an effect outside the transaction belongs after Tx
// returns rather than inside it.
func (w *Writer) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return retryTransient(ctx, budget(w.db.busy), func() (err error) {
		conn, err := w.pinned(ctx)
		if err != nil {
			return err
		}
		// REPLACED ON EVERY EXIT THAT IS NOT A CLEAN ONE, a body that
		// panics included: [txOn] re-panics rather than returning its
		// verdict, so nobody is left to say whether its rollback
		// happened. Keeping a connection that may still carry an open
		// transaction is the failure this path exists to end, and on a
		// pinned writer it is permanent — every later BEGIN on the one
		// connection this applier has is refused, and the domain stops
		// applying for good.
		fit := false
		defer func() {
			if !fit {
				w.replace(ctx, conn)
			}
		}()
		fit, err = w.db.writeOn(ctx, conn, fn)
		return err
	})
}

// replace retires the writer's connection and pins a fresh one in its place,
// under the same declared pin.
//
// RE-PINNED NOW rather than at the next Tx, so [Writer.Conn] keeps answering
// a live connection, and drawn WITHOUT the caller's cancellation: a
// replacement is cleanup, and the failure that made it necessary is often the
// cancellation itself (CLAUDE.md's rule that a teardown takes
// context.WithoutCancel). If it cannot be had, conn stays nil and the next Tx
// tries again and says why.
//
// BOUNDED BY THE BUSY TIMEOUT all the same, because a context with the
// cancellation taken off it has no deadline either. The bound is that knob
// rather than a constant of its own for the reason every other wait here
// takes it: it is the one number an operator already sets for "how long this
// store may block me". A pool wait here is short by construction — the
// connection just handed back is a free slot — but this also runs on the way
// out of a PANIC, and an unbounded wait there would hold the panic inside the
// store with nothing to say so.
func (w *Writer) replace(ctx context.Context, conn *sql.Conn) {
	giveBack(conn, w.retire, false)
	w.retire = nil
	w.conn = nil
	bounded, stop := context.WithTimeout(context.WithoutCancel(ctx), w.db.busy)
	defer stop()
	// The error is the NEXT Tx's to report, through [Writer.pinned]: this
	// runs from a defer, including one unwinding a panic, where there is
	// no caller left to hand it to.
	_, _ = w.pinned(bounded)
}

// pinned is the writer's connection, drawn afresh if the last was retired.
func (w *Writer) pinned(ctx context.Context) (*sql.Conn, error) {
	if w == nil || w.closed {
		return nil, ErrWriterClosed
	}
	if w.db == nil || w.db.sql == nil {
		return nil, ErrNoEstate
	}
	if w.conn == nil {
		conn, err := w.db.sql.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("store: re-pin a writer connection: %w", err)
		}
		w.conn, w.retire = conn, retireSwitch(conn)
	}
	return w.conn, nil
}

// Conn exposes the pinned connection for statements that are not transactions
// — a PRAGMA, a single read.
//
// It is the SAME connection every Tx runs on, until a transaction fails to
// end and Tx replaces it: ask again after a failed Tx rather than holding the
// old one. It is NIL only when that replacement could not be had, and the
// next Tx reports why — so a caller reaching for it directly must check,
// exactly as one reaching for [DB.Replicated] must. There is deliberately NO
// prepared-statement cache on it, and the reason is a measurement rather than
// a preference: on this driver, executing an applier-shaped upsert 4 000 times
// through a statement prepared once on this connection is not faster than
// passing the SQL text each time — 549 ms against 543 ms, and the ordering
// flips between runs. The driver implements ExecerContext, so an "unprepared"
// exec is already one round trip with its arguments, and the parse it saves is
// not what the time is spent on. The shape that DOES pay is the multi-row
// insert — 4 000 rows in 137 ms against 573 ms, four times faster — which is
// [RowsPerInsert] and [Chunks] behind [InsertRows], and BenchmarkLogApplyDrain
// is the record of both numbers.
//
// THAT LAST SENTENCE WAS FALSE FOR THE WHOLE OF THIS PACKAGE'S LIFE UNTIL
// [InsertRows] EXISTED. RowsPerInsert and Chunks had no callers outside their
// own tests: every applier in the tree wrote its child rows one ExecContext
// per row, so the shape this comment called the one that pays was measured by
// a benchmark and shipped nowhere. It is worth recording because nothing
// caught it — a doc comment naming two exported functions reads exactly like a
// doc comment describing what the engine does.
func (w *Writer) Conn() *sql.Conn { return w.conn }

// Close releases the pinned connection back to the pool and gives up the pin.
//
// IDEMPOTENT ON ITS OWN FLAG, not on the connection. A nil connection was the
// closed marker once, and it is now a live writer's legitimate state: a
// writer whose replacement could not be drawn released its pin on the first
// Close, again on the second, and accepted a Tx afterwards that dereferenced
// nothing.
func (w *Writer) Close() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.conn != nil {
		err = w.conn.Close()
		w.conn, w.retire = nil, nil
	}
	w.db.pins.mu.Lock()
	w.db.pins.held--
	w.db.pins.mu.Unlock()
	return err
}

// Read runs fn inside a transaction that only reads.
//
// # Why it exists, and why it takes no ReadOnly option
//
// A multi-statement read has to see ONE snapshot: a board that counts its
// rows in one statement and lists them in the next, against a store an
// applier is committing to, otherwise reports a total that does not match the
// page under it. A transaction is what makes the two statements one answer.
//
// IT PASSES sql.TxOptions{ReadOnly: true}, and that option now MEANS
// something here — but not what its name suggests. [beginModeDriver] reads it
// as "take the DEFERRED begin": a snapshot with no write lock, which is what
// a multi-statement read wants and what keeps a dashboard query from
// excluding the engine's writes for its duration. It does NOT make the driver
// refuse a write inside fn. The read-only-ness is still the caller's
// discipline and this doc; what the option buys is the right begin, not an
// enforcement.
//
// It carries the same retry as [DB.Tx]: a read transaction can lose a snapshot
// race too, and a reader that surfaced "database snapshot is stale" to a
// dashboard would be reporting the store's internals as the answer.
func (d *DB) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	// BEFORE `d.busy`, which is the whole of this bug. The guard lived in
	// [DB.txOpts] alone, and `budget(d.busy)` below is an argument — evaluated
	// first, dereferencing the nil handle the guard was put there to refuse.
	// See [ErrNoEstate]: this is the second time a maintenance tick racing a
	// shutdown has taken the engine down through this exact path.
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	// IDENTITY WORK GOES TO THE RESERVED CONNECTION. This is the read path
	// a socket storm arrives on, and the reserve is what keeps one
	// connection out of that competition — see [Identity] and [DB.reserve].
	if isIdentity(ctx) {
		if err := d.readReserved(ctx, fn); !errors.Is(err, errNoReserve) {
			return err
		}
		// A handle with no room for a reserve (an explicit pool of one)
		// serves identity work from the pool like everybody else. It is
		// where those reads were before the reserve existed, and it is
		// better than refusing a question this handle can answer.
	}
	return retryTransient(ctx, budget(d.busy), func() (err error) {
		conn, err := d.sql.Conn(ctx)
		if err != nil {
			return fmt.Errorf("store: begin: %w", err)
		}
		// GIVEN BACK ON EVERY EXIT, for the reason [DB.Tx] carries: a
		// body that panics unwinds past the return path, and a
		// connection not handed back there is lost to the pool for good.
		// A read's rollback can fail too, and the connection it leaves
		// behind poisons the next caller exactly as a write's would —
		// which [beginModeConn.IsValid] is what stops.
		retire := retireSwitch(conn)
		fit := false
		defer func() { giveBack(conn, retire, fit) }()
		fit, err = txOn(ctx, conn.BeginTx, readTx, fn)
		return err
	})
}

// errNoReserve is [DB.readReserved] reporting that there is no reserved
// connection to run on, so the caller should fall through to the pool.
//
// TWO STATES ANSWER IT and both call for the same thing: a handle with no room
// to hold one (an explicit pool of one), and a handle whose reserved
// connection was retired and could not be re-drawn just now. Neither is a
// reason to refuse a question this handle can answer from the pool — which is
// where these reads were before the reserve existed.
//
// Unexported and never returned to a caller: it is a control answer between
// two functions in this file, and a sentinel rather than a second return value
// because every other path here already answers with an error.
var errNoReserve = errors.New("store: this handle holds no identity reserve")

// readReserved runs a read transaction on the connection [DB.reserve] holds.
//
// SERIALISED, because it is ONE connection: concurrent identity reads take
// their turn rather than competing with the readers the reserve exists to
// protect them from. That is what "one connection's worth" means, and it is
// enough because an identity read is a keyed lookup rather than a scan.
//
// The connection is REPLACED rather than given back when a transaction leaves
// it unfit, exactly as [Writer.Tx] replaces a pinned writer's: this connection
// is held for the handle's life, so one left carrying an open transaction
// would refuse every later identity read for good.
func (d *DB) readReserved(ctx context.Context, fn func(*sql.Tx) error) error {
	d.reserve.mu.Lock()
	defer d.reserve.mu.Unlock()
	return retryTransient(ctx, budget(d.busy), func() (err error) {
		conn := d.identityConn(ctx)
		if conn == nil {
			return errNoReserve
		}
		fit := false
		defer func() {
			if !fit {
				giveBack(conn, d.reserve.retire, false)
				d.reserve.conn, d.reserve.retire = nil, nil
			}
		}()
		fit, err = txOn(ctx, conn.BeginTx, readTx, fn)
		return err
	})
}
