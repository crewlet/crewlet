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
// Readers wait on state THIS writer is about to commit, so under load they hold
// every pooled connection while it queues for the one that unblocks them. The
// pin costs one connection, declared by the caller ([DB.OpenReplicated]) and
// added to the pool: pool = readers + identityReserve + pins. identityReserve is HELD like a pin, since database/sql gives a free
// connection to whoever asks first; an admission semaphore instead would starve
// `pool_starved`, whose input is sql.DBStats.WaitCount. Tx shares [DB.Tx]'s
// queue place and retry loop; a second copy would drift. A Writer is NOT safe
// for concurrent use: goroutines would interleave statements inside each
// other's transactions.
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
// plus the logs [DB.OpenReplicated] was given, so an undeclared pin is not a
// tight fit — it is a reader's connection taken with nothing reporting the
// loss, and the symptom (a dashboard that queues) appears nowhere near the
// cause.
func (d *DB) Writer(ctx context.Context) (*Writer, error) {
	if !d.isOpen() {
		return nil, ErrNoEstate
	}
	d.pins.mu.Lock()
	if d.pins.held >= d.pins.declared {
		declared := d.pins.declared
		d.pins.mu.Unlock()
		return nil, fmt.Errorf(
			"store: this handle declared %d pinned writer(s) and %d are held: "+
				"the replicated estate's pins are one per log it carries — "+
				"open it with DB.OpenReplicated for the number of logs whose "+
				"apply loops run on it, so the pool is sized for them",
			declared, declared)
	}
	d.pins.held++
	d.pins.mu.Unlock()

	conn, err := d.conn(ctx)
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
	return retryTransient(ctx, budget(w.db.busy, w.db.pool), func() (err error) {
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
// RE-PINNED NOW rather than at the next Tx, so [Writer.Conn] keeps answering a
// live connection, and drawn WITHOUT the caller's cancellation: the failure that
// made it necessary is often the cancellation itself. If it cannot be had, conn
// stays nil and the next Tx tries again and says why.
//
// BOUNDED BY THE BUSY TIMEOUT, since a context without cancellation has no
// deadline either, and this runs on the way out of a PANIC, where an unbounded
// wait would hold the panic inside the store. It is that knob, as every wait
// here is: the one number an operator sets for how long the store may block.
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
	if !w.db.isOpen() {
		return nil, ErrNoEstate
	}
	if w.conn == nil {
		conn, err := w.db.conn(ctx)
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
// It is the SAME connection every Tx runs on until a failed Tx replaces it, so
// ask again after one rather than holding the old. It is NIL only when that
// replacement could not be had, so a direct caller must check.
//
// There is deliberately NO prepared-statement cache: on this driver 4 000
// upserts through one prepared statement took 549 ms against 543 ms unprepared
// (ExecerContext is already one round trip). What pays is the multi-row insert,
// 137 ms against 573 ms: [InsertRows] over [RowsPerInsert] and [Chunks], with
// BenchmarkLogApplyDrain the record of both.
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
// A multi-statement read has to see ONE snapshot, or a count and the list after
// it disagree under an applier's commits.
//
// IT PASSES sql.TxOptions{ReadOnly: true}, which [beginModeDriver] reads twice:
// the DEFERRED begin (no write lock, so a dashboard query does not exclude the
// engine's writes), and `query_only` on for fn, so a write inside fn is REFUSED
// by the engine, the promise [ReplicatedReader] rests on (see
// [beginModeConn.queryOnlyFor]). It carries [DB.Tx]'s retry, so a lost snapshot
// race does not surface as "database snapshot is stale".
func (d *DB) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	// BEFORE `d.busy`, which is the whole of this bug. The guard lived in
	// [DB.txOpts] alone, and `budget(d.busy, d.pool)` below is an argument —
	// evaluated first, dereferencing the nil handle the guard was put there
	// to refuse.
	// See [ErrNoEstate]: this is the second time a maintenance tick racing a
	// shutdown has taken the engine down through this exact path.
	if !d.isOpen() {
		return ErrNoEstate
	}
	// IDENTITY WORK GOES TO THE RESERVED CONNECTION. This is the read path
	// a socket storm arrives on, and the reserve is what keeps one
	// connection out of that competition — see [Identity] and [DB.reserve].
	if isIdentity(ctx) {
		if err := d.readReserved(ctx, fn); !errors.Is(err, errNoReserve) {
			return err
		}
		// A reserve that could not be drawn just now serves identity work
		// from the pool like everybody else. It is where those reads were
		// before the reserve existed, and it is better than refusing a
		// question this handle can answer.
	}
	return retryTransient(ctx, budget(d.busy, d.pool), func() (err error) {
		conn, err := d.conn(ctx)
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
// IT MEANS THE RESERVED CONNECTION COULD NOT BE DRAWN JUST NOW (at the open, or
// after the last was retired), which is no reason to refuse a question the pool
// can answer. Every handle holds a reserve, added to the pool ([Options.poolSize]),
// so no bound is too small to keep one.
//
// Never returned to a caller: a control answer between two functions here, a
// sentinel because every other path already answers with an error.
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
	return retryTransient(ctx, budget(d.busy, d.pool), func() (err error) {
		// A HANDLE CLOSED UNDER THE CALLER answers as one that was never
		// open, exactly as [DB.conn] does for the pool: Close may have
		// released the reserve between [DB.Read]'s guard and this lock.
		if d.closed.Load() {
			return fmt.Errorf("%w: %s was closed while this call was in flight",
				ErrNoEstate, d.path)
		}
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
