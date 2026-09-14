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
// The pool is small on purpose (see defaultReaderConns), and every reader on
// this node draws from it: the dashboard's queries, the coverage probes, a
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
// # It is not a second transaction implementation
//
// Tx below takes the SAME place in the SAME queue, begins the SAME IMMEDIATE
// transaction and carries the SAME retry loop [DB.Tx] does, because a second
// copy is how one of them comes to retry an error the other returns, or to
// take the write lock the way the other does not. The only difference is which
// connection the transaction begins on.
//
// The pin keeps a writer out of the POOL's competition, not out of the
// queue's: every write transaction on this file, pinned or pooled, is served
// in the order it asked for the lock (see writelock.go).
//
// A Writer is NOT safe for concurrent use: it is one connection, and its
// owner is one goroutine. Two goroutines sharing one would interleave
// statements inside each other's transactions.
type Writer struct {
	db *DB
	// conn is the pinned connection. REPLACED, not kept, when a
	// transaction on it fails to end: see Tx.
	conn   *sql.Conn
	closed bool
}

// Writer pins a connection and returns a handle that owns it until Close.
//
// REFUSED PAST THE DECLARED COUNT, naming it. The pool was sized as readers
// plus [Options.PinnedWriters], so an undeclared pin is not a tight fit — it
// is a reader's connection taken with nothing reporting the loss, and the
// symptom (a dashboard that queues) appears nowhere near the cause.
func (d *DB) Writer(ctx context.Context) (*Writer, error) {
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
	return &Writer{db: d, conn: conn}, nil
}

// Tx runs fn inside a write transaction on the pinned connection, with
// [DB.Tx]'s lock, retry and rollback semantics exactly, including that fn MAY
// RUN MORE THAN ONCE, so anything with an effect outside the transaction
// belongs after Tx returns rather than inside it.
//
// A CONNECTION THAT CANNOT BE TRUSTED IS REPLACED. When an attempt leaves its
// transaction possibly open ([attempt] reports it unfit, or fn panicked and
// nothing reported anything), the pinned connection is retired and a fresh one
// pinned in its place, under the same declared pin. Keeping it would refuse
// every later BEGIN on the one connection this writer uses, which stopped a
// domain applying for good.
func (w *Writer) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return retryTransient(ctx, func() (err error) {
		conn, err := w.pinned(ctx)
		if err != nil {
			return err
		}
		// REPLACED ON EVERY EXIT THAT IS NOT A CLEAN ONE, a body that
		// panics included: [attempt] re-panics rather than returning its
		// verdict, and nobody is left to say whether its rollback
		// happened. Keeping a connection that may still carry an open
		// transaction is the failure this whole path exists to end, and on
		// a pinned writer it is permanent.
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
// RE-PINNED NOW rather than at the next Tx, so [Writer.Conn] keeps answering a
// live connection, and drawn WITHOUT the caller's cancellation: a replacement
// is cleanup, and the failure that made it necessary is often the
// cancellation itself. If it cannot be had, the next Tx tries again and says
// why.
func (w *Writer) replace(ctx context.Context, conn *sql.Conn) {
	giveBack(conn, false)
	w.conn = nil
	_, _ = w.pinned(context.WithoutCancel(ctx))
}

// pinned is the writer's connection, drawn afresh if the last was retired.
func (w *Writer) pinned(ctx context.Context) (*sql.Conn, error) {
	if w.closed {
		return nil, errors.New("store: this writer is closed")
	}
	if w.conn == nil {
		conn, err := w.db.sql.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("store: re-pin a writer connection: %w", err)
		}
		w.conn = conn
	}
	return w.conn, nil
}

// Conn exposes the pinned connection for statements that are not
// transactions: a PRAGMA, a single read.
//
// It is the SAME connection every Tx runs on, until a transaction fails to end
// and Tx replaces it: ask again after a failed Tx rather than holding the old
// one. It is nil only when that replacement could not be had, which the next
// Tx reports. There is deliberately NO
// prepared-statement cache on it, and the reason is a measurement rather than
// a preference: on this driver, executing an applier-shaped upsert 4 000 times
// through a statement prepared once on this connection is not faster than
// passing the SQL text each time — 549 ms against 543 ms, and the ordering
// flips between runs. The driver implements ExecerContext, so an "unprepared"
// exec is already one round trip with its arguments, and the parse it saves is
// not what the time is spent on. The shape that DOES pay is the multi-row
// insert — 4 000 rows in 137 ms against 573 ms, four times faster — which is
// [RowsPerInsert] and [Chunks], and BenchmarkLogApplyDrain is the record of
// both numbers.
func (w *Writer) Conn() *sql.Conn { return w.conn }

// Close releases the pinned connection back to the pool.
func (w *Writer) Close() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.conn != nil {
		err = w.conn.Close()
		w.conn = nil
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
// database/sql has sql.TxOptions{ReadOnly: true} for exactly this and it is
// deliberately not passed: the driver cannot make a transaction read-only, and
// this package's BEGIN refuses the option rather than ignoring it the way the
// driver does (see writelock.go). The read-only-ness here is the caller's
// discipline and this doc, which is the honest description of what the layer
// underneath actually provides.
//
// IT TAKES NO LOCK AND QUEUES BEHIND NOTHING: a plain BEGIN is deferred, the
// snapshot is taken at the first statement, and under the write-ahead log a
// reader and the one writer never wait on each other. That is also why it
// must not write. A write inside it would upgrade a snapshot another commit
// may already have advanced past, which the driver refuses, and which
// [retryable] deliberately does not retry.
//
// It carries the same retry as [DB.Tx] for the failures a read can meet: a
// connection some other caller left dirty, and the driver's lock held past the
// busy timeout while a snapshot is being established.
func (d *DB) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return retryTransient(ctx, func() error {
		if d == nil || d.sql == nil {
			return ErrNoEstate
		}
		conn, err := d.sql.Conn(ctx)
		if err != nil {
			return fmt.Errorf("store: begin: %w", err)
		}
		// GIVEN BACK ON EVERY EXIT, for the reason [DB.Tx] carries: a body
		// that panics unwinds past the return path, and a connection not
		// handed back there is lost to the pool for good.
		fit := false
		defer func() { giveBack(conn, fit) }()
		fit, err = attempt(ctx, conn.BeginTx, nil, fn)
		return err
	})
}
