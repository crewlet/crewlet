package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
)

// THE WRITE LOCK IS TAKEN AT BEGIN, NOT AT THE FIRST WRITE, and this file is
// the whole of how.
//
// # What the driver does on its own
//
// Turso as this package configures it is a SINGLE-WRITER database with a
// database-level write lock and no MVCC — the DSN [openPool] hands the
// connector is a bare path with no `experimental=` list, so BEGIN CONCURRENT
// is unreachable. And its BeginTx DISCARDS its driver.TxOptions and execs the
// literal string "BEGIN", which is DEFERRED and takes no lock at all
// (tursogo v0.8.0-pre.11, driver_db.go:197-202). The lock is acquired by the
// transaction's FIRST WRITE STATEMENT, and it then excludes a write to EVERY
// TABLE IN THE FILE rather than to the tables this transaction touches.
//
// Two failures follow from that, and both were being paid for silently:
//
//   - A READ-THEN-WRITE transaction is ABORTED by any commit that lands
//     anywhere in the file between its read and its first write, with
//     "database snapshot is stale, rollback and retry the transaction" —
//     measured three times out of three against a commit into a table the
//     transaction never names. That is the shape of every state-log applier
//     (internal/statelog's read of the rows below the batch, then the decide,
//     then the write), so [retryTransient] was absorbing a real conflict by
//     REPLAYING a four-thousand-row batch, once per foreign commit. A write
//     statement's own internal read does NOT pin the snapshot — an upsert
//     landing on an existing row is clean — so only an explicit query is
//     exposed, which is why this looked like an occasional slow test rather
//     than a rule.
//   - A WRITE-ONLY transaction is never aborted, but it waits for the lock
//     INSIDE a transaction it has already opened, so losing the race costs
//     the full busy timeout AND a replay of everything the body did.
//
// BEGIN IMMEDIATE answers both at the root rather than absorbing either. A
// transaction that reaches its body cannot be aborted by anybody else's
// commit, and one that loses the race loses it HAVING DONE NOTHING, so its
// retry is a wait rather than a replay.
//
// # Why a driver wrapper and not hand-driven statements
//
// The obvious alternative is to stop using *sql.Tx and exec BEGIN IMMEDIATE /
// COMMIT / ROLLBACK on a *sql.Conn. It is wrong here, and measurably:
// database/sql rolls a transaction back when its context is cancelled, and
// hand-driven statements lose that. After a cancel, ROLLBACK on that same
// context returns "context canceled" WITHOUT EXECUTING, and the next BEGIN on
// the connection answers "cannot start a transaction within a transaction" —
// on a PINNED connection ([Writer]) that is the domain applying nothing for
// the life of the process. Wrapping the connection keeps every database/sql
// guarantee and changes only the word the driver was going to emit.
//
// # Why it is installed BELOW Options.WrapDriver
//
// [openPool] installs this FIRST and lets WrapDriver wrap it, so
// internal/store/storetest's commit faults still wrap the real driver.Tx this
// returns. Installed the other way round, the fault's BeginTx would hand back
// a transaction this file never sees and every commit fault would silently
// stop firing — a fault injector that no longer injects reads exactly like a
// store that no longer fails.
type beginModeDriver struct{ inner driver.Driver }

func (d *beginModeDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &beginModeConn{Conn: conn}, nil
}

// beginModeConn exists for BeginTx alone. Everything else is FORWARDED
// EXPLICITLY rather than left to the embedded interface: database/sql picks
// the exec, query and prepare paths from OPTIONAL interfaces the concrete type
// satisfies, and an embedded driver.Conn carries none of them — embedding the
// narrow interface hides the wide ones and drops every statement onto the
// prepared-statement path. storetest's own fault conn says this in the same
// words, for the same reason.
type beginModeConn struct {
	driver.Conn

	// unfit is set when a statement that ENDS a transaction on this
	// connection failed, which is the state database/sql cannot see: the
	// driver reports the failure without answering [driver.ErrBadConn], so
	// the connection goes back to the pool with a transaction possibly
	// still open on it and the next caller is refused its own BEGIN.
	//
	// ATOMIC because database/sql may end a transaction from its own
	// awaitDone goroutine — a rollback it issues when the caller's context
	// is cancelled — while the caller is still inside the call that began
	// it.
	unfit atomic.Bool
}

// RetireSwitch hands out the function that says this connection's
// transaction may still be open, so it must not be handed to another caller.
// See [beginModeConn.IsValid] for what acts on it, and [retireSwitch] for why
// the switch is taken out rather than the connection reached back into.
func (c *beginModeConn) RetireSwitch() func() { return func() { c.unfit.Store(true) } }

// IsValid is how a retired connection is DISCARDED rather than pooled: it is
// [driver.Validator], which database/sql consults on every hand-back
// (validateConnection, from putConn) and closes the connection on.
//
// # Why the answer is a flag here rather than sql.Conn.Raw at the hand-back
//
// Raw answering [driver.ErrBadConn] is database/sql's documented door for
// "this connection must not be reused", and calling it where the hand-back
// happens is RACY — against database/sql closing that same *sql.Conn, which
// is exactly what it does when a transaction's context is cancelled, because
// the rollback its awaitDone goroutine then issues discards the connection.
// Conn.Raw reads c.done BEFORE taking c.closemu, so a close landing in that
// window leaves it dereferencing a nil driverConn. Measured, and not a
// theoretical window: a SIGSEGV in the retention loop's read during an
// ordinary engine drain, which takes the whole process with it. The
// cancellation that opens the window is the ordinary shape of a shutdown,
// and awaitDone's rollback is still in flight when tx.Rollback has already
// answered ErrTxDone to the caller — so "the transaction has ended" is not
// the same fact as "nothing is closing this connection".
//
// A flag the driver's connection carries has no window at all: database/sql
// ASKS it, on its own terms, at a point it has already serialised.
// [DB.retireSwitch] is how the store reaches it, and reaches it while the
// connection is provably still its own.
func (c *beginModeConn) IsValid() bool { return !c.unfit.Load() }

// BeginTx issues the begin this package actually wants.
//
// THE MODE IS CARRIED BY sql.TxOptions.ReadOnly, which is the standard
// vocabulary for "this will not write" and which the driver otherwise
// discards. ReadOnly selects the DEFERRED begin — a snapshot with no lock,
// which is what a multi-statement dashboard read needs and what [DB.Read]
// passes. Everything else, including the nil options database/sql passes by
// default, is a WRITE and takes the lock now.
//
// NOTHING HERE MAKES THE DRIVER REFUSE A WRITE inside a read-only
// transaction. The option selects a begin; it is not an enforcement, and
// [DB.Read]'s doc stays honest about that.
func (c *beginModeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (tx driver.Tx, err error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, errNoExecer
	}
	stmt := "BEGIN IMMEDIATE"
	if opts.ReadOnly {
		stmt = "BEGIN"
	}
	defer func() {
		if err != nil {
			// A BEGIN THE DRIVER REFUSED, whose usual reason is a
			// transaction already open on this connection. Retiring
			// one that was in fact clean costs a reconnect on a path
			// that is already failing; keeping a dirty one costs
			// every later caller that draws it.
			c.unfit.Store(true)
		}
	}()
	if _, err = ex.ExecContext(ctx, stmt, nil); err != nil {
		return nil, err
	}
	return &beginModeTx{ex: ex, conn: c}, nil
}

// Begin is the path database/sql falls back to for a driver with no
// ConnBeginTx. It cannot express the mode, so it takes the WRITE one: this
// package's transactions are writes unless a caller said otherwise, and a
// fallback that quietly took the deferred begin would reintroduce exactly the
// shape above on whichever path reached it.
//
//nolint:staticcheck // SA1019: the method database/sql's interface still requires.
func (c *beginModeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *beginModeConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *beginModeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *beginModeConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *beginModeConn) Ping(ctx context.Context) error {
	p, ok := c.Conn.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

// beginModeTx ends the transaction this connection began.
//
// ON context.Background(), which is what the driver's own transaction does and
// is the only correct choice here: database/sql calls Rollback from its
// awaitDone goroutine precisely BECAUSE the caller's context died, so a
// rollback bound to that context would refuse to run and leave the transaction
// open on the connection. This is CLAUDE.md's "a teardown takes
// context.WithoutCancel" in the one place the interface offers no context to
// strip.
type beginModeTx struct {
	ex   driver.ExecerContext
	conn *beginModeConn
	done bool
}

func (t *beginModeTx) Commit() error   { return t.end("COMMIT") }
func (t *beginModeTx) Rollback() error { return t.end("ROLLBACK") }

// end is not guarded by a mutex, and does not need one: database/sql
// serialises every call on a driver connection behind its own lock, and a
// driver.Tx is reachable only from the connection that produced it. The flag
// is here for the double-end database/sql itself can make — a Rollback from
// awaitDone racing the caller's Commit is ordered by that same lock, and the
// second one must answer ErrTxDone rather than emit a stray statement.
func (t *beginModeTx) end(stmt string) error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	_, err := t.ex.ExecContext(context.Background(), stmt, nil)
	if err != nil {
		// THE TRANSACTION MAY STILL BE OPEN. A COMMIT the driver
		// refused leaves one, as SQLite does for a deferred
		// constraint, and so does a ROLLBACK it refused — and in
		// neither case does it answer ErrBadConn, so nothing else
		// would stop this connection being pooled. See
		// [beginModeConn.IsValid].
		t.conn.unfit.Store(true)
	}
	return err
}

// errNoExecer is a driver whose connection cannot execute a bare statement,
// which this package requires twice over: for the session pragmas and for the
// begin mode.
var errNoExecer = errors.New(
	"store: the driver's connection cannot execute statements, so the session " +
		"pragmas and the immediate-begin mode are both unreachable")

// readTx selects the deferred begin. Only [DB.Read] passes it.
var readTx = &sql.TxOptions{ReadOnly: true}
