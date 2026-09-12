package storetest

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync/atomic"
)

// FailReadsAfter wraps a driver so that every result set fails after n rows.
//
// It exists to reach the branch no other technique can. A fail-open read has
// two failure paths and they are NOT the same: the query failing outright, and
// the result set failing PART WAY THROUGH iteration. Closing the database
// reaches only the first. The second is what decides whether a caller gets
// "nothing is known" or a silent PARTIAL answer — some rows found, the rest
// unknown, with no way to tell which half arrived — and that is the failure
// mode fail-open exists to avoid.
//
// The wrapped driver is the real, certified one. Only iteration is
// intercepted, so the SQL, the schema and the encoding are all genuine; the
// only fiction is when the rows stop.
//
// The returned [Fault] is both the wrapper and its switch: pass Fault.Wrap to
// store.Options.WrapDriver, then Arm it once the setup writes and the CONTROL
// read have run against a healthy store. Without a control, an assertion that
// a failed read answers nothing also passes for a store that never found
// anything.
func FailReadsAfter(n int, err error) *Fault {
	return &Fault{after: n, err: err}
}

// CorruptReadsAfter wraps a driver so that every result set yields an
// UNSCANNABLE value after n rows.
//
// A third failure path, distinct from the two FailReadsAfter covers. A
// mid-iteration transport failure surfaces through rows.Err; a value the
// column cannot hold surfaces through rows.Scan, which is a separate branch
// in every reader and therefore a separate chance to return a partial answer.
// Both lines look the same and only one of them is exercised by an error on
// Next.
func CorruptReadsAfter(n int) *Fault {
	return &Fault{after: n, corrupt: true}
}

// Fault is an armable read failure.
type Fault struct {
	after   int
	err     error
	corrupt bool
	armed   atomic.Bool
}

// Wrap is the [store.Options.WrapDriver] function for this fault.
//
// A method rather than a returned closure, so the handle a test arms is the
// same object the open database is using. The closure form looked identical
// and armed a driver nobody was connected to.
func (f *Fault) Wrap(d driver.Driver) driver.Driver {
	return &faultDriver{inner: d, fault: f}
}

// Arm switches the fault on; Disarm switches it off.
//
// Armed separately from Wrap because the fault would otherwise fire during
// migration and the database would never open.
func (f *Fault) Arm() { f.armed.Store(true) }

// Disarm stops the fault firing, so a test can assert the recovery path
// on the same handle that failed.
func (f *Fault) Disarm() { f.armed.Store(false) }

type faultDriver struct {
	inner driver.Driver
	fault *Fault
}

func (d *faultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: conn, fault: d.fault}, nil
}

type faultConn struct {
	driver.Conn
	fault *Fault
}

// The connector requires ExecerContext, and database/sql picks the query path
// from the optional interfaces a conn implements — so both are forwarded
// explicitly. An embedded driver.Conn does NOT carry them: they are separate
// interfaces the concrete type satisfies, and embedding the narrow one hides
// them, which silently drops every query onto the prepared-statement path.
func (c *faultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := qr.QueryContext(ctx, q, args)
	if err != nil || !c.fault.armed.Load() {
		return rows, err
	}
	return &faultRows{Rows: rows, remaining: c.fault.after,
		err: c.fault.err, corrupt: c.fault.corrupt}, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		// The deprecated Begin is the CORRECT fallback for a driver that
		// never implemented ConnBeginTx — it is what database/sql itself
		// falls back to. A wrapper that refused it would work with fewer
		// drivers than the standard library.
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		return c.Conn.Begin()
	}
	return bt.BeginTx(ctx, opts)
}

type faultRows struct {
	driver.Rows
	remaining int
	err       error
	corrupt   bool
}

// Next fails once the allowance is spent. Returning the error rather than
// io.EOF is the point: io.EOF is a normal end of results and would look like a
// short but successful read, which is exactly the partial answer under test.
func (r *faultRows) Next(dest []driver.Value) error {
	if r.remaining > 0 {
		r.remaining--
		return r.Rows.Next(dest)
	}
	if !r.corrupt {
		return r.err
	}
	// NULL where a value is expected. database/sql refuses to convert it
	// into any non-pointer, non-Null* destination — "converting NULL to
	// string is unsupported" — so the failure lands on rows.Scan rather
	// than rows.Err, which is the branch under test.
	//
	// It is nil and not a time.Time: a time DOES convert into a string
	// destination, so the first version of this helper produced a
	// successful scan of a formatted timestamp and the test caught the
	// instrument rather than the code.
	//
	// The real row is read first so the column count is right — otherwise
	// the failure would be a shape mismatch, which fails for a different
	// reason and would pass this test while proving nothing.
	if err := r.Rows.Next(dest); err != nil {
		return err
	}
	if len(dest) > 0 {
		dest[0] = nil
	}
	return nil
}

// FailCommitAfter wraps a driver so that the (n+1)th COMMIT fails with err.
//
// # Why a commit fault is its own instrument
//
// The read faults above reach a caller that got an incomplete answer. This one
// reaches a caller that does not know what happened to a WRITE, and the two
// have opposite remedies: an incomplete read is retried freely, while a
// failed-looking write may or may not have landed. A rolled-back transaction
// leaves rows, op ids and checkpoints consistent with each other, and the
// applier contract's whole claim is that they move together — so the way to
// prove it is to fail the commit and read all three back.
//
// It counts commits rather than firing on all of them so a test can let its
// setup and its control transaction through and arm the fault at the one
// commit under examination.
func FailCommitAfter(n int, err error) *CommitFault {
	return &CommitFault{after: n, err: err}
}

// LoseCommitAck wraps a driver so that the (n+1)th COMMIT SUCCEEDS and then
// reports failure to the caller.
//
// # The only way to reach committed-but-unknown
//
// This is the branch nothing else can produce. The transaction is durable —
// the pages are on disk, a reader sees them, a restart finds them — and the
// caller was told it failed. Every other fault produces a state the caller's
// belief matches; this one produces the state where it does not, which is the
// state the three-valued write outcome exists for. A caller that treats it as
// a failure and retries writes twice; one that treats it as a success reports
// a durability it did not observe. The only correct answer is "unknown", and
// without this injector no test can ever hand a caller that fact.
//
// A CRASH BETWEEN COMMIT AND ACK is the real-world shape of it, and it cannot
// be produced in-process: the process is gone. This is that shape with the
// process still running to be asserted against.
func LoseCommitAck(n int) *CommitFault {
	return &CommitFault{after: n, lose: true}
}

// CommitFault is an armable commit failure. See [FailCommitAfter] and
// [LoseCommitAck].
type CommitFault struct {
	after int
	err   error
	lose  bool

	armed atomic.Bool
	seen  atomic.Int64
	fired atomic.Bool
}

// Wrap is the [store.Options.WrapDriver] function for this fault.
func (f *CommitFault) Wrap(d driver.Driver) driver.Driver {
	return &commitFaultDriver{inner: d, fault: f}
}

// Arm switches the fault on; Disarm switches it off. Armed separately for the
// same reason the read faults are: a fault that fired during migration would
// stop the database opening at all.
func (f *CommitFault) Arm() { f.armed.Store(true) }

// Disarm stops the fault firing, so a test can assert the recovery path on the
// same handle that failed.
func (f *CommitFault) Disarm() { f.armed.Store(false) }

// Fired reports whether the fault actually reached its commit.
//
// An assertion that the injector RAN, and it is not ceremony: the store
// retries a conflicted transaction, so a test that armed a fault and asserted
// only the outcome can pass because the fault never fired at all.
func (f *CommitFault) Fired() bool { return f.fired.Load() }

// commitErr decides what this commit does. It is called once per commit while
// armed, and consumes the allowance.
func (f *CommitFault) commitErr() (fail bool, lose bool, err error) {
	if !f.armed.Load() {
		return false, false, nil
	}
	if f.seen.Add(1) <= int64(f.after) {
		return false, false, nil
	}
	f.fired.Store(true)
	if f.lose {
		return false, true, errLostAck
	}
	return true, false, f.err
}

// errLostAck is what a lost acknowledgement surfaces as. A distinct sentinel
// rather than the caller's own error, because the point of the injector is
// that the caller cannot tell this apart from a real failure by its error.
var errLostAck = errors.New("storetest: the commit's acknowledgement was lost")

// ErrLostAck is the error a [LoseCommitAck] transaction reports. Exported so a
// test can assert the failure it saw was the injected one rather than a real
// fault the fixture produced by accident.
var ErrLostAck = errLostAck

type commitFaultDriver struct {
	inner driver.Driver
	fault *CommitFault
}

func (d *commitFaultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &commitFaultConn{Conn: conn, fault: d.fault}, nil
}

type commitFaultConn struct {
	driver.Conn
	fault *CommitFault
}

// The same four forwards faultConn makes, and for the same reason: an embedded
// driver.Conn carries none of the optional interfaces, and hiding them moves
// every query onto a different path than production takes.
func (c *commitFaultConn) ExecContext(
	ctx context.Context, q string, args []driver.NamedValue,
) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *commitFaultConn) QueryContext(
	ctx context.Context, q string, args []driver.NamedValue,
) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *commitFaultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *commitFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		tx, err := c.Conn.Begin()
		if err != nil {
			return nil, err
		}
		return &commitFaultTx{Tx: tx, fault: c.fault}, nil
	}
	tx, err := bt.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &commitFaultTx{Tx: tx, fault: c.fault}, nil
}

type commitFaultTx struct {
	driver.Tx
	fault *CommitFault
}

// Commit is where the two injectors differ, and the difference is the whole
// point: FailCommitAfter never reaches the real Commit, so nothing is durable;
// LoseCommitAck reaches it FIRST and reports the failure afterwards, so
// everything is durable and the caller was told otherwise.
func (t *commitFaultTx) Commit() error {
	fail, lose, err := t.fault.commitErr()
	switch {
	case fail:
		// Rolled back rather than left open: a driver that returned an
		// error from Commit without ending the transaction would wedge
		// the connection, and the fault under test is a failed commit,
		// not a leaked one.
		_ = t.Tx.Rollback()
		return err
	case lose:
		if commitErr := t.Tx.Commit(); commitErr != nil {
			return commitErr
		}
		return err
	default:
		return t.Tx.Commit()
	}
}
