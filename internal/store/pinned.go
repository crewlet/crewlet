package store

import (
	"context"
	"database/sql"
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
// Tx below carries the SAME retry loop and the SAME stale-snapshot classifier
// [DB.Tx] does, through retryStale, because a second copy is how one of them
// comes to retry an error the other returns. The only difference is which
// connection the transaction begins on.
//
// A Writer is NOT safe for concurrent use: it is one connection, and its
// owner is one goroutine. Two goroutines sharing one would interleave
// statements inside each other's transactions.
type Writer struct {
	db   *DB
	conn *sql.Conn
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

// Tx runs fn inside a transaction on the pinned connection, with [DB.Tx]'s
// retry and rollback semantics exactly — including that fn MAY RUN MORE THAN
// ONCE, so anything with an effect outside the transaction belongs after Tx
// returns rather than inside it.
func (w *Writer) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return retryStale(ctx, func() error { return w.tx(ctx, fn) })
}

// tx is one attempt: begin, run, commit or roll back.
func (w *Writer) tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := w.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Conn exposes the pinned connection for statements that are not transactions
// — a prepared-statement cache, a PRAGMA, a single read.
//
// It is the SAME connection every Tx runs on, which is what makes a statement
// prepared through it reusable: database/sql caches a prepared statement per
// connection, so preparing through the pool would re-prepare on whichever
// connection answered.
func (w *Writer) Conn() *sql.Conn { return w.conn }

// Close releases the pinned connection back to the pool.
func (w *Writer) Close() error {
	if w == nil || w.conn == nil {
		return nil
	}
	err := w.conn.Close()
	w.conn = nil
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
// deliberately not passed: the driver's BeginTx IGNORES its options and always
// issues a plain BEGIN (see the note on DB.Tx). Passing one would read as a
// guarantee the driver does not make — a caller could believe a write inside
// fn is refused, and it is not. The read-only-ness here is the caller's
// discipline and this doc, which is the honest description of what the layer
// underneath actually provides.
//
// It carries the same retry as [DB.Tx]: a read transaction can lose a snapshot
// race too, and a reader that surfaced "database snapshot is stale" to a
// dashboard would be reporting the store's internals as the answer.
func (d *DB) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return retryStale(ctx, func() error {
		return d.tx(ctx, func(tx *sql.Tx) error { return fn(tx) })
	})
}
