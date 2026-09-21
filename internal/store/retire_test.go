package store_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A CONNECTION WHOSE TRANSACTION DID NOT END IS RETIRED, NOT HANDED ON.
//
// A rollback that fails leaves the transaction open on the connection it ran
// on, and database/sql hands that connection back to the pool without knowing:
// the driver reports the failure without answering ErrBadConn. The next caller
// to draw it is refused its own BEGIN ("cannot start a transaction within a
// transaction") for a transaction it never began. The retry that answered
// that error assumed its next attempt would draw a DIFFERENT connection, and
// database/sql reuses the connection it freed last, so the retry drew the same
// dirty one eight times and gave up. On a pinned writer there is no other
// connection at all: its domain stopped applying for good.
//
// Each case poisons one transaction's rollback and then asks for a clean one
// through the same path. A pool of ONE connection makes the pooled cases as
// deterministic as the pinned one, rather than dependent on which idle
// connection database/sql prefers; the pinned case needs no such help, since
// its writer only ever has the one.
//
// Mutation: hand the connection back after a failed rollback, and every case
// here fails its second transaction.
func TestAConnectionWhoseTransactionDidNotEndIsRetired(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// pinned is whether the case writes through a pinned writer.
		pinned bool
		// poison runs a transaction whose body fails, so its rollback is
		// the one the fault breaks.
		poison func(context.Context, *store.DB, *store.Writer) error
		// next runs the transaction that must begin cleanly afterwards.
		next func(context.Context, *store.DB, *store.Writer) error
	}{
		{
			name: "a pooled write",
			poison: func(ctx context.Context, db *store.DB, _ *store.Writer) error {
				return db.Tx(ctx, failAfterInsert(ctx, 1))
			},
			next: func(ctx context.Context, db *store.DB, _ *store.Writer) error {
				return db.Tx(ctx, insert(ctx, 2))
			},
		},
		{
			name: "a pooled read",
			poison: func(ctx context.Context, db *store.DB, _ *store.Writer) error {
				return db.Read(ctx, func(*sql.Tx) error { return errBodyFailed })
			},
			next: func(ctx context.Context, db *store.DB, _ *store.Writer) error {
				return db.Tx(ctx, insert(ctx, 2))
			},
		},
		{
			name:   "a pinned write",
			pinned: true,
			poison: func(ctx context.Context, _ *store.DB, w *store.Writer) error {
				return w.Tx(ctx, failAfterInsert(ctx, 1))
			},
			next: func(ctx context.Context, _ *store.DB, w *store.Writer) error {
				return w.Tx(ctx, insert(ctx, 2))
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			fault := &rollbackFault{}
			opts := store.Options{WrapDriver: fault.wrap, MaxOpenConns: 1}
			if c.pinned {
				opts = store.Options{WrapDriver: fault.wrap, PinnedWriters: 1}
			}
			node, err := store.Open(ctx, filepath.Join(t.TempDir(), "retire.db"), opts)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = node.Close() }()
			db := node.Replicated()
			if err := db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `CREATE TABLE retire_probe (id INTEGER PRIMARY KEY)`)
				return err
			}); err != nil {
				t.Fatalf("create the probe table: %v", err)
			}
			var w *store.Writer
			if c.pinned {
				if w, err = db.Writer(ctx); err != nil {
					t.Fatalf("pin a writer: %v", err)
				}
				defer func() { _ = w.Close() }()
			}

			fault.armed.Store(true)
			if err := c.poison(ctx, db, w); !errors.Is(err, errBodyFailed) {
				t.Fatalf("the poisoned transaction returned %v, want its body's own "+
					"error", err)
			}
			if !fault.fired.Load() {
				t.Fatal("the rollback fault never fired, so this case staged " +
					"nothing to recover from")
			}
			if err := c.next(ctx, db, w); err != nil {
				t.Fatalf("the transaction after a failed rollback could not "+
					"begin: %v", err)
			}
			// AND IT WAS NEVER REFUSED ONE, which is the difference
			// between retiring the connection and recovering from it.
			// A dirty connection left in the pool is caught on the
			// next BEGIN over it and retired then, so a caller that
			// only asserts it eventually succeeded passes either way
			// — having spent an attempt of its budget and told
			// nobody. Retiring it at the hand-back means the caller
			// that had nothing to do with the failure never meets it.
			if n := fault.refused.Load(); n != 0 {
				t.Errorf("%d BEGIN(s) were refused after the broken rollback: "+
					"the connection whose transaction did not end was handed "+
					"to the next caller rather than retired, and what recovered "+
					"was the retry", n)
			}

			// AND THE ABANDONED TRANSACTION DID NOT COMMIT, which a
			// connection handed on with it still open would let the next
			// caller's statements do inside it.
			var ids []int
			if err := db.Read(ctx, func(tx *sql.Tx) error {
				rows, err := tx.QueryContext(ctx, `SELECT id FROM retire_probe ORDER BY id`)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var id int
					if err := rows.Scan(&id); err != nil {
						return err
					}
					ids = append(ids, id)
				}
				return rows.Err()
			}); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if len(ids) != 1 || ids[0] != 2 {
				t.Errorf("the probe table holds %v, want only the clean "+
					"transaction's row (2)", ids)
			}
		})
	}
}

var errBodyFailed = errors.New("the transaction's body failed")

func insert(ctx context.Context, id int) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO retire_probe (id) VALUES (?)`, id)
		return err
	}
}

// failAfterInsert writes a row and then fails, so the rollback that follows
// has something to undo.
func failAfterInsert(ctx context.Context, id int) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if err := insert(ctx, id)(tx); err != nil {
			return err
		}
		return errBodyFailed
	}
}

// rollbackFault breaks ONE rollback once armed: it reports a failure and never
// reaches the driver, so the transaction stays open on its connection, which
// is what a rollback the driver refused leaves behind.
type rollbackFault struct {
	armed, fired atomic.Bool
	// refused counts the BEGINs the driver turned down, which on this
	// path means one and only one thing: a connection with a transaction
	// still open on it reached a caller that had nothing to do with it.
	refused atomic.Int64
	// reachedLate is set when the store reached into a connection AFTER a
	// transaction had been begun on it, which is the window that
	// segfaults.
	reachedLate atomic.Bool
}

func (f *rollbackFault) wrap(d driver.Driver) driver.Driver {
	return &rollbackFaultDriver{inner: d, fault: f}
}

type rollbackFaultDriver struct {
	inner driver.Driver
	fault *rollbackFault
}

func (d *rollbackFaultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &rollbackFaultConn{Conn: conn, fault: d.fault}, nil
}

// rollbackFaultConn forwards what the store's connection path uses, for the
// reason storetest's injectors spell out: an embedded driver.Conn carries none
// of the optional interfaces.
type rollbackFaultConn struct {
	driver.Conn
	fault *rollbackFault
	// began is whether a transaction has been begun on this connection
	// SINCE IT WAS LAST HANDED OUT. A pooled connection is reused, so the
	// window this records is the checkout's rather than the connection's:
	// database/sql resets a connection it is about to reuse, which is
	// where the flag is cleared.
	began atomic.Bool
}

// ResetSession is called by database/sql before it hands this connection to
// another caller, which is what makes [rollbackFaultConn.began] a fact about
// one checkout rather than about the connection's whole life.
func (c *rollbackFaultConn) ResetSession(ctx context.Context) error {
	c.began.Store(false)
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *rollbackFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c *rollbackFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *rollbackFaultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

// IsValid forwards the store's own verdict on this connection, which is how
// a connection whose transaction may still be open is retired rather than
// pooled. Hidden here, these cases would measure a recovery the store does
// not actually have.
func (c *rollbackFaultConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

// RetireSwitch forwards the wrapped connection's retire switch, which the
// store takes out at the draw. Hidden here, a connection whose
// transaction did not end would be handed to the next caller.
func (c *rollbackFaultConn) RetireSwitch() func() {
	if c.began.Load() {
		c.fault.reachedLate.Store(true)
	}
	r, ok := c.Conn.(interface{ RetireSwitch() func() })
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
}

func (c *rollbackFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.began.Store(true)
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		c.fault.refused.Add(1)
		return nil, err
	}
	return &rollbackFaultTx{Tx: tx, fault: c.fault}, nil
}

type rollbackFaultTx struct {
	driver.Tx
	fault *rollbackFault
}

func (t *rollbackFaultTx) Rollback() error {
	if t.fault.armed.CompareAndSwap(true, false) {
		t.fault.fired.Store(true)
		return errors.New("injected: the rollback did not happen")
	}
	return t.Tx.Rollback()
}

// A BODY THAT PANICS DOES NOT COST THE HANDLE ITS CONNECTION.
//
// Every transaction here draws a connection and hands it back when the attempt
// is over. A panic does not end an attempt by returning: it unwinds through
// the runtime, past anything written on the return path. What that costs
// depends on which path it passed through, and both are fatal to the handle:
//
//   - a POOLED connection never handed back is one database/sql has lost for
//     the life of the process. Measured on the default pool of four, a fifth
//     transaction after four panicking bodies waited in the pool for ever.
//   - a PINNED connection is the only one its writer will ever have, and a
//     panic reports nothing about whether its rollback happened, so keeping it
//     is keeping a connection that may still carry an open transaction.
//
// Each case panics through one path and then asks that path for a connection
// it can use. The pooled cases run on a pool of ONE, so a single lost
// connection is the whole pool, and under a deadline, so a pool that has lost
// it fails here rather than hanging. The pinned case breaks the rollback as
// well, which is what makes its connection unusable rather than merely
// suspect.
//
// Mutation: hand the connection back on the return path rather than on the way
// out of the attempt, and the pooled cases fail with a deadline expired inside
// database/sql and the pinned case with "cannot start a transaction within a
// transaction".
func TestABodyThatPanicsDoesNotCostTheHandleItsConnection(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// breakRollback arms the fault, so the panic's own rollback fails
		// and leaves the transaction open on the connection.
		breakRollback bool
		opts          store.Options
		panicking     func(context.Context, *store.DB, *store.Writer)
		next          func(context.Context, *store.DB, *store.Writer) error
	}{
		{
			name: "a pooled write",
			opts: store.Options{MaxOpenConns: 1},
			panicking: func(ctx context.Context, db *store.DB, _ *store.Writer) {
				_ = db.Tx(ctx, func(*sql.Tx) error { panic(errBodyPanicked) })
			},
			next: func(ctx context.Context, db *store.DB, _ *store.Writer) error {
				return db.Tx(ctx, insert(ctx, 2))
			},
		},
		{
			name: "a pooled read",
			opts: store.Options{MaxOpenConns: 1},
			panicking: func(ctx context.Context, db *store.DB, _ *store.Writer) {
				_ = db.Read(ctx, func(*sql.Tx) error { panic(errBodyPanicked) })
			},
			next: func(ctx context.Context, db *store.DB, _ *store.Writer) error {
				return db.Tx(ctx, insert(ctx, 2))
			},
		},
		{
			name:          "a pinned write",
			breakRollback: true,
			opts:          store.Options{PinnedWriters: 1},
			panicking: func(ctx context.Context, _ *store.DB, w *store.Writer) {
				_ = w.Tx(ctx, func(tx *sql.Tx) error {
					if err := insert(ctx, 1)(tx); err != nil {
						return err
					}
					panic(errBodyPanicked)
				})
			},
			// THROUGH Writer.Conn RATHER THAN Writer.Tx. Tx's own retry
			// retires the connection on the BEGIN that meets the open
			// transaction and draws another, so a transaction through Tx
			// recovers whether or not the panic replaced anything, and
			// says nothing about which. What a caller reaching for the
			// pinned connection ITSELF gets is the whole question here,
			// and [store.Writer.Conn] promises a live one.
			next: func(ctx context.Context, _ *store.DB, w *store.Writer) error {
				conn := w.Conn()
				if conn == nil {
					return errors.New("the writer was left with no pinned " +
						"connection, so Writer.Conn answers nil")
				}
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				return tx.Rollback()
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			fault := &rollbackFault{}
			opts := c.opts
			opts.WrapDriver = fault.wrap
			node, err := store.Open(ctx, filepath.Join(t.TempDir(), "panic.db"), opts)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = node.Close() }()
			db := node.Replicated()
			if err := db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `CREATE TABLE retire_probe (id INTEGER PRIMARY KEY)`)
				return err
			}); err != nil {
				t.Fatalf("create the probe table: %v", err)
			}
			var w *store.Writer
			if c.opts.PinnedWriters > 0 {
				if w, err = db.Writer(ctx); err != nil {
					t.Fatalf("pin a writer: %v", err)
				}
				defer func() { _ = w.Close() }()
			}

			fault.armed.Store(c.breakRollback)
			got := panicValue(func() { c.panicking(ctx, db, w) })
			if err, ok := got.(error); !ok || !errors.Is(err, errBodyPanicked) {
				t.Fatalf("the transaction returned %v where its body panicked "+
					"with %v: a panic must reach the caller rather than be "+
					"swallowed by the store", got, errBodyPanicked)
			}
			if c.breakRollback && !fault.fired.Load() {
				t.Fatal("the rollback fault never fired, so this case staged " +
					"nothing to recover from")
			}

			// UNDER A DEADLINE: a connection the panic lost is one this
			// call waits for inside database/sql with nothing left to
			// free it, and a test that hung would report the same
			// failure as a timeout nobody can attribute.
			bounded, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			if err := c.next(bounded, db, w); err != nil {
				t.Fatalf("the transaction after a panicking body could not "+
					"begin: %v", err)
			}
		})
	}
}

// errBodyPanicked is what a panicking body panics with, so the test can tell
// its own panic from any other.
var errBodyPanicked = errors.New("the transaction's body panicked")

// panicValue runs fn and reports what it panicked with, or nil.
func panicValue(fn func()) (v any) {
	defer func() { v = recover() }()
	fn()
	return nil
}

// A CLOSED WRITER RELEASES ITS PIN ONCE AND REFUSES EVERY LATER TRANSACTION,
// EVEN WHEN IT HAD ALREADY LOST ITS CONNECTION.
//
// A nil connection was Close's own "already closed" marker, and it stopped
// being one when a failed transaction began REPLACING the connection rather
// than keeping it: a writer whose replacement could not be drawn is LIVE with
// no connection. Under the old marker that writer's Close released nothing,
// so its pin was held for the life of the process and the next Writer on the
// handle was refused; and a Tx after Close was accepted and dereferenced a
// nil.
//
// The state is staged rather than asserted about: a body panics with its
// rollback broken, so the attempt reports its connection unfit, and the
// driver is then refusing to open any more, so the replacement cannot be had.
// That is exactly the state, reached the only way production reaches it.
//
// Mutation: guard Close on `w.conn == nil` again, and the pin is never
// released (the second Writer is refused) and the Tx after Close is accepted.
func TestAWriterThatLostItsConnectionStillGivesBackItsPin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fault := &rollbackFault{}
	closedDoor := &openFault{}
	node, err := store.Open(ctx, filepath.Join(t.TempDir(), "lost.db"), store.Options{
		WrapDriver: func(d driver.Driver) driver.Driver {
			return closedDoor.wrap(fault.wrap(d))
		},
		PinnedWriters: 1,
		MaxOpenConns:  1,
		// SHORT, so a bounded replacement draw that the pool could
		// somehow serve does not hold this case for the default five
		// seconds. It is not what the case turns on: the door below is
		// shut, so the draw fails rather than waits.
		BusyTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	db := node.Replicated()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE retire_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}

	fault.armed.Store(true)
	closedDoor.shut.Store(true)
	if got := panicValue(func() {
		_ = w.Tx(ctx, func(tx *sql.Tx) error {
			if err := insert(ctx, 1)(tx); err != nil {
				return err
			}
			panic(errBodyPanicked)
		})
	}); !errors.Is(got.(error), errBodyPanicked) {
		t.Fatalf("the transaction returned %v, want its body's own panic", got)
	}
	if !fault.fired.Load() {
		t.Fatal("the rollback fault never fired, so the connection was never " +
			"reported unfit and this case staged nothing")
	}
	if w.Conn() != nil {
		t.Fatal("the writer still holds a connection: the door was shut, so " +
			"the replacement cannot have been drawn, and this case is not the " +
			"state it is named for")
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close a writer that has no connection: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("a second close reported %v, want nil: it is a no-op", err)
	}
	if err := w.Tx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrWriterClosed) {
		t.Errorf("a transaction on a closed writer got %v, want %v", err,
			store.ErrWriterClosed)
	}

	// THE PIN CAME BACK, and exactly once: one is declared, so a second
	// Writer proves both that Close released it and that the double Close
	// did not spend it twice.
	closedDoor.shut.Store(false)
	again, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("the pin a closed writer gave back could not be taken again: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("close the second writer: %v", err)
	}
}

// openFault refuses to open new driver connections while it is shut, which is
// how a test reaches the state of a pinned writer whose replacement could not
// be drawn.
type openFault struct{ shut atomic.Bool }

func (f *openFault) wrap(d driver.Driver) driver.Driver {
	return &openFaultDriver{inner: d, fault: f}
}

type openFaultDriver struct {
	inner driver.Driver
	fault *openFault
}

func (d *openFaultDriver) Open(name string) (driver.Conn, error) {
	if d.fault.shut.Load() {
		return nil, errors.New("injected: no more connections")
	}
	return d.inner.Open(name)
}

// A CANCELLED TRANSACTION DOES NOT TAKE THE PROCESS WITH IT.
//
// When a transaction's context is cancelled, database/sql rolls it back from
// its own awaitDone goroutine and DISCARDS the connection — which closes the
// *sql.Conn the store is holding, concurrently with the store's own
// hand-back. That close is why the retirement is a flag on the driver's
// connection rather than a [sql.Conn.Raw] callback at the hand-back:
// Conn.Raw reads the conn's done flag BEFORE taking the lock that close
// holds, so a close landing in that window leaves it dereferencing a nil
// driverConn.
//
// It is not a theoretical window. The engine's retention loop reads on a
// context the drain cancels, and the panic took the whole process down in the
// middle of an ordinary shutdown:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	database/sql.(*Conn).Raw(...)  sql.go:2087
//	store.giveBack(...)            store.go:1298
//	store.(*DB).Read.func1.1()     pinned.go:268
//
// tx.Rollback answering ErrTxDone is what makes it reachable: it means
// awaitDone got there first, and awaitDone is STILL INSIDE its own rollback
// and close when the caller is already unwinding. "The transaction has ended"
// is not the same fact as "nothing is closing this connection".
//
// So the cancellation is forced from INSIDE the body, which is the ordering
// that produced the crash, and repeated, because the window is narrow. Put
// the Raw call back in giveBack and this panics rather than failing.
func TestACancelledTransactionDoesNotCrashTheHandBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := store.Open(ctx, filepath.Join(t.TempDir(), "cancel.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	db := node.Replicated()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE retire_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	for _, c := range []struct {
		name string
		run  func(context.Context, context.CancelFunc) error
	}{
		{"a pooled read", func(c context.Context, cancel context.CancelFunc) error {
			return db.Read(c, func(tx *sql.Tx) error {
				cancel()
				var n int
				return tx.QueryRowContext(c,
					`SELECT count(*) FROM retire_probe`).Scan(&n)
			})
		}},
		{"a pooled write", func(c context.Context, cancel context.CancelFunc) error {
			return db.Tx(c, func(tx *sql.Tx) error {
				cancel()
				return insert(c, 1)(tx)
			})
		}},
		{"a pinned write", func(c context.Context, cancel context.CancelFunc) error {
			return w.Tx(c, func(tx *sql.Tx) error {
				cancel()
				return insert(c, 1)(tx)
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// NOT PARALLEL, and not a subtest that shares the writer
			// with another: a Writer is one connection and one owner.
			for range 50 {
				cancelled, cancel := context.WithCancel(ctx)
				// The error is whatever the cancellation produced and
				// is not the subject: what is asserted is that the
				// hand-back returned at all.
				_ = c.run(cancelled, cancel)
				cancel()
			}
		})
	}
}

// THE RETIRE SWITCH IS TAKEN OUT BEFORE ANY TRANSACTION EXISTS ON THE
// CONNECTION, which is the whole of why the crash above cannot come back.
//
// [sql.Conn.Raw] is the only way into a pooled connection from outside
// database/sql, and it races a concurrent close of that same *sql.Conn. The
// only thing that closes one underneath its holder is the awaitDone goroutine
// of a transaction on it, so the window opens at this CHECKOUT's first
// BeginTx — not before, where this goroutine is the connection's only
// reference and there is no transaction to cancel. It is a fact about the
// checkout rather than about the connection, which is reused; database/sql
// resets a connection before handing it on, and that is where the fault
// connection clears its record.
//
// So the store reaches in once, at the draw, takes the switch out, and throws
// it later without touching database/sql at all. This is that ordering,
// asserted rather than described: the fault connection records when a
// transaction is begun on it and when its switch is taken, and reports a
// switch taken after a begin.
//
// Mutation: move the retireSwitch call from the draw to the hand-back — which
// is where sql.Conn.Raw naturally wants to be, and where it crashed — and
// every case here reports it.
func TestTheRetireSwitchIsTakenBeforeAnyTransactionExists(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fault := &rollbackFault{}
	node, err := store.Open(ctx, filepath.Join(t.TempDir(), "order.db"),
		store.Options{WrapDriver: fault.wrap, PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	db := node.Replicated()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE retire_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	if err := db.Tx(ctx, insert(ctx, 1)); err != nil {
		t.Fatalf("a pooled write: %v", err)
	}
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		var n int
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM retire_probe`).Scan(&n)
	}); err != nil {
		t.Fatalf("a pooled read: %v", err)
	}
	if err := w.Tx(ctx, insert(ctx, 2)); err != nil {
		t.Fatalf("a pinned write: %v", err)
	}

	if fault.reachedLate.Load() {
		t.Error("the store reached into a connection after a transaction had " +
			"been begun on it: that is the window database/sql closes the " +
			"connection in when a context is cancelled, and reaching in there " +
			"dereferences a nil driver connection")
	}
}
