package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Every write transaction holds the file's write lock from its BEGIN, and this
// process queues for it.
//
// # What the driver actually does
//
// Read from its source at the pinned version (turso core v0.8.0-pre.8,
// core/storage/wal.rs and core/vdbe) and measured against it, because the
// alternative is a design resting on what SQLite would do:
//
//   - ONE WRITER PER FILE. The write lock is a try-lock with no queue behind
//     it. A statement that cannot take it returns busy, and the busy handler
//     sleeps on SQLite's schedule (1 ms, growing to 100 ms) and tries again.
//     Nothing is served in order: a waiter wins only if one of its polls
//     lands in the gap between one holder's release and the next holder's
//     acquire.
//   - A PLAIN BEGIN IS DEFERRED, and the driver's BeginTx issues nothing else
//     whatever options it is handed. A deferred transaction takes its
//     snapshot at its first statement and asks for the write lock only at its
//     first WRITE, and if ANY connection committed ANYTHING to the file in
//     between, the upgrade fails with "database snapshot is stale". The
//     conflict is detected per FILE, not per row.
//
// Both of those broke the state log applier, whose occupancy model rests on
// its transactions never being aborted by a commit elsewhere in the file.
//
// The applier READS before it writes: every apply transaction opens with the
// deferred-scope probe. So a commit to a table it never touches, landing in
// the window between that read and its first write, aborted it. Measured
// against a writer committing to an untouched table in the same file: eight
// aborts in eight attempts, and the batch failed. The drain test that was
// meant to catch exactly that could not see it, because its transaction
// wrote first, and a first statement that writes takes its snapshot and the
// lock together.
//
// What that test DID catch, on macOS only, was the other half. `PRAGMA
// fullfsync` makes every commit there an F_FULLFSYNC, so a small commit holds
// the write lock for about 4 ms and leaves a gap of microseconds; a waiter
// polling on the schedule above then loses nearly every poll, and its busy
// timeout expires with the writer beside it having committed a thousand times.
// With fullfsync off the same commit holds the lock for about 0.25 ms and the
// waiter wins within a few polls, which is why Linux never showed it. The
// hazard is not the platform's, though: any writer that commits back to back
// starves a polling waiter, and a fast disk only narrows the odds.
//
// # The two halves of the fix
//
// A write transaction begins IMMEDIATE ([writeTx]): it takes the write lock
// at BEGIN, so its snapshot is taken with the lock held and nothing can commit
// to the file until it does. There is no window, so there is nothing to abort
// it. database/sql's only way to ask a driver for a different BEGIN is
// [sql.TxOptions], which this driver ignores, so [beginDriver] sits directly
// over it and gives the options a meaning.
//
// And this process's write transactions take the lock through a FIFO queue
// ([writeQueue]), one per file, handed from each holder to the writer that
// asked next. The driver's lock stays the mutual exclusion; the queue is the
// ORDER, which is the one thing the driver's busy handler cannot give. A
// writer's wait is then bounded by the work queued ahead of it rather than by
// how its polls happen to line up with somebody else's commits, which is
// what lets three domains' appliers share one file and each still drain.
//
// # What it does not cover
//
// A statement issued outside a transaction through [DB.SQL] takes the
// driver's lock on its own and is not queued. It cannot be aborted (its read
// and its write are one statement), but it waits on the driver's busy handler
// rather than in order. The replicated estate has none; the node estate's
// autocommit writers (the learning subsystem's single-statement inserts) are
// best effort by design.

// writeTx is what every write transaction in this package begins with: the
// file's write lock taken at BEGIN rather than at the first write.
//
// SERIALIZABLE is the database/sql spelling [beginDriver] maps to BEGIN
// IMMEDIATE. It is the honest word for it: a deferred transaction is
// serialized against other writers only at its first write, and aborted if one
// got there first; this one is serialized from its first statement.
var writeTx = &sql.TxOptions{Isolation: sql.LevelSerializable}

// errWritersQueued is a write transaction that waited past the busy timeout
// for the ones queued ahead of it on the same database.
//
// Retryable, exactly as the driver's own "database is locked" is: it is the
// same wait, ended by the same knob, and a retry is a longer wait rather than
// a second effect. See [retryable].
var errWritersQueued = errors.New("store: the write transactions queued ahead " +
	"of this one did not finish within the busy timeout; raise " +
	"store.busy_timeout_seconds if a transaction on this database " +
	"legitimately runs that long")

// ---- the BEGIN the options ask for ------------------------------------ //

// beginDriver wraps the driver so that BeginTx honours what it is asked for.
//
// DIRECTLY OVER THE DRIVER, beneath [Options.WrapDriver]: a fault injector
// wraps what this returns, so the transaction it intercepts is the one this
// package actually begins rather than one begun beside it.
type beginDriver struct{ inner driver.Driver }

func (d beginDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("store: driver %T cannot execute a BEGIN", d.inner)
	}
	return &beginConn{Conn: conn, exec: exec}, nil
}

// beginConn forwards every interface the driver's connection implements and
// replaces only BeginTx.
//
// FORWARDED EXPLICITLY, because an embedded driver.Conn carries none of the
// optional interfaces: database/sql picks its query path from what a
// connection implements, and hiding one moves every statement onto a
// different path than the driver's own. The set is the driver's, asserted by
// TestTheBeginAdapterHidesNoInterfaceOfTheDriver so a driver bump that adds
// one fails there rather than silently here.
type beginConn struct {
	driver.Conn
	exec driver.ExecerContext
}

func (c *beginConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.exec.ExecContext(ctx, q, args)
}

func (c *beginConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *beginConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *beginConn) Ping(ctx context.Context) error {
	p, ok := c.Conn.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

// BeginTx begins the transaction the options describe, and REFUSES what it
// cannot give.
//
// The driver underneath accepts every option and issues a plain BEGIN for all
// of them, so a caller asking for a read-only or a read-committed transaction
// got a deferred one and was told nothing. Refusing is what makes an option
// that reaches this layer a guarantee rather than a hint.
func (c *beginConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		return nil, errors.New("store: this driver cannot make a transaction " +
			"read-only; use DB.Read, whose read-only-ness is the caller's discipline")
	}
	var begin string
	switch level := sql.IsolationLevel(opts.Isolation); level {
	case sql.LevelDefault:
		begin = "BEGIN"
	case sql.LevelSerializable:
		begin = "BEGIN IMMEDIATE"
	default:
		return nil, fmt.Errorf("store: isolation level %s is not one this driver "+
			"can begin; a write transaction asks for %s, a read for the default",
			level, sql.LevelSerializable)
	}
	if _, err := c.exec.ExecContext(ctx, begin, nil); err != nil {
		return nil, err
	}
	return &beginTx{exec: c.exec}, nil
}

// beginTx ends what BeginTx began, on the same connection.
//
// NOT CANCELLABLE, deliberately and as the driver's own is: database/sql
// hands Commit and Rollback no context, and a commit abandoned half way is
// the one outcome worse than either.
type beginTx struct{ exec driver.ExecerContext }

func (t *beginTx) Commit() error {
	_, err := t.exec.ExecContext(context.Background(), "COMMIT", nil)
	return err
}

func (t *beginTx) Rollback() error {
	_, err := t.exec.ExecContext(context.Background(), "ROLLBACK", nil)
	return err
}

// ---- the queue -------------------------------------------------------- //

// writeQueue orders one database's write transactions: first to ask, first to
// begin.
//
// # Why not a mutex or a channel
//
// A sync.Mutex cannot be abandoned (a writer whose context ended would stay
// in line until it reached the front), and neither it nor a blocked channel
// send PROMISES an order; the runtime happens to be close to FIFO for both,
// and a fairness property resting on an implementation detail is one a Go
// release can take away. This is explicit: waiters in a slice, served from the
// front, and the lock HANDED to the next one rather than released for anyone
// to take, so a writer arriving at the moment of a release cannot barge past
// one that has been waiting.
//
// # One per FILE, not per handle
//
// It lives on the process's claim on the file ([fileLock]), which every handle
// this process opens on that path shares. Two handles on one file are two
// pools on one lock, which the package doc calls safe, and they stay one line
// for it rather than two lines polling each other.
//
// # Bounded by the busy timeout
//
// A wait here IS the wait for the file's write lock, done in order, so it is
// bounded by the same knob as the driver's own and fails the same retryable
// way ([errWritersQueued]). Unbounded, a transaction that waited on another
// write transaction on the same database from inside its own body, which the
// driver answered with a busy timeout, would wait for ever. The bound is the
// WAITER's, passed in, because it is a property of the handle that asked.
//
// The zero value is ready.
type writeQueue struct {
	mu sync.Mutex
	// held is whether a writer has the queue. It stays true across a
	// handoff, so a waiter is only ever appended behind a holder.
	held    bool
	waiters []chan struct{}
}

// acquire returns once the caller is at the front, wait passes, or ctx ends.
// A nil error means the caller holds the queue and must release it. A wait of
// zero means [defaultBusyTimeout].
func (q *writeQueue) acquire(ctx context.Context, wait time.Duration) error {
	turn, holds := q.join()
	if holds {
		return nil
	}
	if wait <= 0 {
		wait = defaultBusyTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var err error
	select {
	case <-turn:
		return nil
	case <-timer.C:
		err = errWritersQueued
	case <-ctx.Done():
		err = ctx.Err()
	}
	if q.leave(turn) {
		q.release()
	}
	return err
}

// join takes the queue if it is free, and otherwise joins the line: the
// returned channel is closed when the caller reaches the front.
func (q *writeQueue) join() (turn chan struct{}, holds bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.held {
		q.held = true
		return nil, true
	}
	turn = make(chan struct{})
	q.waiters = append(q.waiters, turn)
	return turn, false
}

// leave takes a waiter that gave up out of the line, and reports whether it
// was too late: already handed the queue, in which case the caller HOLDS it.
//
// GIVING UP RACES THE HANDOFF, and losing that race must not lose the queue.
// A waiter dropped with the queue in its hands would stall every writer
// behind it until the busy timeout, and then again, so the caller that finds
// itself holding passes it on instead.
func (q *writeQueue) leave(turn chan struct{}) (holds bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, w := range q.waiters {
		if w == turn {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			return false
		}
	}
	return true
}

// release hands the queue to the longest waiter, or frees it when there is
// none.
func (q *writeQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiters) == 0 {
		q.held = false
		return
	}
	next := q.waiters[0]
	q.waiters[0] = nil
	q.waiters = q.waiters[1:]
	close(next)
}
