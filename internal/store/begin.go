package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
)

// THE WRITE LOCK IS TAKEN AT BEGIN, NOT AT THE FIRST WRITE; this file is how.
//
// Turso here is single-writer with a database-level write lock and no MVCC.
// Its BeginTx DISCARDS driver.TxOptions and execs a DEFERRED "BEGIN" (tursogo
// v0.8.1, driver_db.go:197-202), so the lock is taken by the first write and
// excludes writes to EVERY table. A READ-THEN-WRITE transaction (every
// state-log applier) is then ABORTED by any commit anywhere in the file
// ("database snapshot is stale"), and [retryTransient] REPLAYED a four-thousand-
// row batch per foreign commit; a WRITE-ONLY one waits for the lock inside an
// opened transaction, costing the busy timeout AND a replay. BEGIN IMMEDIATE
// fixes both: a loser has done nothing, so its retry is a wait.

// beginModeDriver is a driver wrapper, not hand-driven BEGIN/COMMIT on a
// *sql.Conn: database/sql rolls back on context cancel and hand-driven
// statements lose that (ROLLBACK on the cancelled context returns "context
// canceled" WITHOUT EXECUTING, and the next BEGIN fails "cannot start a
// transaction within a transaction", leaving a PINNED [Writer] applying
// nothing for the life of the process).
//
// It is installed BELOW Options.WrapDriver ([openPool] does it first), so
// storetest's commit faults wrap the real driver.Tx; the other way round their
// BeginTx hands back a transaction this file never sees and they stop firing.
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

	// inTx is whether a transaction is open on this connection, which a
	// `query_only` change must never land inside. A plain field that needs
	// no guard: database/sql serialises every call on a driver connection
	// behind its own lock, the rollback its awaitDone goroutine issues
	// included.
	inTx bool

	// queryOnly is whether `query_only` is on for this connection now:
	// on for a read transaction, off for everything else. See
	// [beginModeConn.queryOnlyFor].
	queryOnly bool
}

// queryOnlyFor sets this connection's `query_only` to what its next statement
// needs (ON for a read transaction's BEGIN, OFF otherwise) when it differs.
//
// A read is ENFORCED, not promised: [DB.Read]'s deferred begin lets any
// statement through, yet [ReplicatedReader] is handed out on the strength of
// being unable to write (measured: an INSERT through Read committed).
//
// Only on a change, and never inside a transaction: pools hand connections to
// reads and writes alike, so a VACUUM INTO or migration on one a read left ON
// would be refused. `= 1` and `= 0`, because Turso REFUSES `ON`/`OFF` here.
// A FAILED CHANGE RETIRES THE CONNECTION: left ON it refuses every later
// write, left OFF it lets a read write.
func (c *beginModeConn) queryOnlyFor(ctx context.Context, on bool) error {
	if c.inTx || c.queryOnly == on {
		return nil
	}
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return errNoExecer
	}
	value := "0"
	if on {
		value = "1"
	}
	if _, err := ex.ExecContext(ctx, "PRAGMA query_only = "+value, nil); err != nil {
		c.unfit.Store(true)
		return fmt.Errorf("store: set query_only to %s: %w", value, err)
	}
	c.queryOnly = on
	return nil
}

// RetireSwitch hands out the function that says this connection's
// transaction may still be open, so it must not be handed to another caller.
// See [beginModeConn.IsValid] for what acts on it, and [retireSwitch] for why
// the switch is taken out rather than the connection reached back into.
func (c *beginModeConn) RetireSwitch() func() { return func() { c.unfit.Store(true) } }

// IsValid is how a retired connection is DISCARDED rather than pooled: it is
// [driver.Validator], which database/sql consults on every hand-back and
// closes the connection on.
//
// A flag, not sql.Conn.Raw answering [driver.ErrBadConn] at the hand-back:
// Raw reads c.done BEFORE taking c.closemu, so racing database/sql closing the
// same *sql.Conn (which it does when a transaction's context is cancelled, via
// the rollback in awaitDone, still in flight after tx.Rollback answered
// ErrTxDone) dereferences a nil driverConn. Measured: a SIGSEGV in the
// retention loop during an ordinary drain. A flag database/sql ASKS, at a point
// it has already serialised, has no window; [DB.retireSwitch] reaches it while
// the connection is provably still the store's own.
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
// AND IT IS AN ENFORCEMENT: a read-only transaction runs with `query_only`
// on, so the engine refuses any write inside it — see
// [beginModeConn.queryOnlyFor].
func (c *beginModeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (tx driver.Tx, err error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, errNoExecer
	}
	stmt := "BEGIN IMMEDIATE"
	if opts.ReadOnly {
		stmt = "BEGIN"
	}
	if err = c.queryOnlyFor(ctx, opts.ReadOnly); err != nil {
		return nil, err
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
	c.inTx = true
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
	if err := c.queryOnlyFor(ctx, false); err != nil {
		return nil, err
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *beginModeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.queryOnlyFor(ctx, false); err != nil {
		return nil, err
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *beginModeConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if err := c.queryOnlyFor(ctx, false); err != nil {
		return nil, err
	}
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
	// CLOSED WHATEVER THE ANSWER, as far as a `query_only` change is
	// concerned: a connection whose end failed is retired rather than used
	// again (see below), so no change will ever run on it either way.
	t.conn.inTx = false
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

// readTx selects the deferred begin, with writes refused. Only [DB.Read]
// passes it.
var readTx = &sql.TxOptions{ReadOnly: true}
