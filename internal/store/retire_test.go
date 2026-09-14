package store_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

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

func (c *rollbackFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
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
