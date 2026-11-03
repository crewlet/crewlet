package storetest

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
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

// IsValid forwards the wrapped connection's own verdict, which is how the
// store RETIRES a connection whose transaction may still be open (see
// internal/store's begin.go). Hidden here, a fault that leaves one behind
// would be handed to the next caller and the case would measure the wrong
// recovery — the same reason every other optional interface is forwarded
// explicitly above.
func (c *faultConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

// RetireSwitch forwards the wrapped connection's retire switch, which the
// store takes out at the draw. Hidden here, a connection whose
// transaction did not end would be handed to the next caller.
func (c *faultConn) RetireSwitch() func() {
	r, ok := c.Conn.(interface{ RetireSwitch() func() })
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
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
	return &txWrapDriver{inner: d, wrap: func(tx driver.Tx, _ driver.TxOptions) driver.Tx {
		return &commitFaultTx{Tx: tx, fault: f}
	}}
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
// retries a transaction that failed transiently, so a test that armed a fault
// and asserted only the outcome can pass because the fault never fired at
// all.
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

// txWrapDriver is a driver whose every transaction is handed to wrap as it
// begins, with the options it was begun under — the one place the commit
// instruments ([CommitFault], [CommitHold]) reach a transaction, so the
// connection's forwards below are written once.
type txWrapDriver struct {
	inner driver.Driver
	wrap  func(tx driver.Tx, opts driver.TxOptions) driver.Tx
}

func (d *txWrapDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &txWrapConn{Conn: conn, wrap: d.wrap}, nil
}

type txWrapConn struct {
	driver.Conn
	wrap func(tx driver.Tx, opts driver.TxOptions) driver.Tx
}

// The same four forwards faultConn makes, and for the same reason: an embedded
// driver.Conn carries none of the optional interfaces, and hiding them moves
// every query onto a different path than production takes.
func (c *txWrapConn) ExecContext(
	ctx context.Context, q string, args []driver.NamedValue,
) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *txWrapConn) QueryContext(
	ctx context.Context, q string, args []driver.NamedValue,
) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *txWrapConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

// IsValid forwards the wrapped connection's own verdict, which is how the
// store RETIRES a connection whose transaction may still be open (see
// internal/store's begin.go). Hidden here, a fault that leaves one behind
// would be handed to the next caller and the case would measure the wrong
// recovery — the same reason every other optional interface is forwarded
// explicitly above.
func (c *txWrapConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

// RetireSwitch forwards the wrapped connection's retire switch, which the
// store takes out at the draw. Hidden here, a connection whose
// transaction did not end would be handed to the next caller.
func (c *txWrapConn) RetireSwitch() func() {
	r, ok := c.Conn.(interface{ RetireSwitch() func() })
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
}

func (c *txWrapConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		tx, err := c.Conn.Begin()
		if err != nil {
			return nil, err
		}
		return c.wrap(tx, opts), nil
	}
	tx, err := bt.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return c.wrap(tx, opts), nil
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

// HoldCommits wraps a driver so that, while it is armed ([CommitHold.Arm]),
// every COMMIT of a write transaction waits until it is released: the
// transaction's statements have run and its commit has not returned, which is
// what a disk whose fsync has stalled looks like to everything waiting on that
// commit.
//
// # Why a hold is its own instrument
//
// The two faults above end a commit at once, one way or the other. A HOLD keeps
// it open for as long as the case says: it is how a case asks what the code
// around a commit does WHILE the commit has not returned — what an apply loop
// goes on doing, and what the callers waiting on it are told — which a stalling
// disk produces for seconds at a time and which no other instrument can stage.
//
// A READ'S TRANSACTION IS NEVER HELD: [store.DB.Read] begins it read-only, and
// what a read commits is nothing. Every write transaction on the handle it
// wraps is — a node's own estate and the replicated one beside it share one
// wrapper — so a case arms it at the moment the one write it is about is next.
//
// RELEASED AT CLEANUP whatever the case did, because a commit left held keeps
// its connection, the file's write lock and every goroutine behind them past
// the case's end.
func HoldCommits(t testing.TB) *CommitHold {
	h := &CommitHold{changed: make(chan struct{})}
	t.Cleanup(h.Release)
	return h
}

// CommitHold is an armable hold on write commits. See [HoldCommits].
type CommitHold struct {
	mu sync.Mutex

	// release is closed to let go the commits held since the last Arm, and
	// nil while the hold is off; fail is what a released commit reports in
	// place of committing — nil commits it.
	release chan struct{}
	fail    error

	// holding is how many commits wait at the hold now; begun and
	// committed count the write transactions begun, and committed, since
	// the last Arm.
	holding   int
	begun     int
	committed int

	// changed is closed and replaced on every change of a count, which is
	// what the Await methods wait on.
	changed chan struct{}
}

// Wrap is the [store.Options.WrapDriver] function for this hold.
func (h *CommitHold) Wrap(d driver.Driver) driver.Driver {
	return &txWrapDriver{inner: d, wrap: func(tx driver.Tx, opts driver.TxOptions) driver.Tx {
		if opts.ReadOnly {
			return tx
		}
		h.mu.Lock()
		if h.release != nil {
			h.begun++
			h.changedLocked()
		}
		h.mu.Unlock()
		return &heldTx{Tx: tx, hold: h}
	}}
}

// Arm holds every write commit from now on, and starts the counts afresh.
// Armed separately from Wrap for the reason the faults are: a hold in place at
// the open would keep the migrations from ever committing.
func (h *CommitHold) Arm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release != nil {
		return
	}
	h.release, h.fail = make(chan struct{}), nil
	h.begun, h.committed = 0, 0
	h.changedLocked()
}

// Release lets every held commit commit, and holds no more until the next
// Arm. Idempotent.
func (h *CommitHold) Release() { h.let(nil) }

// ReleaseFailing ends every held commit as one that never landed — rolled
// back, reporting err — and holds no more until the next Arm: a crash between
// a transaction's statements and its commit, with the process still running to
// be asserted against.
func (h *CommitHold) ReleaseFailing(err error) { h.let(err) }

func (h *CommitHold) let(fail error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release == nil {
		return
	}
	h.fail = fail
	close(h.release)
	h.release = nil
}

// Begun is how many write transactions began since the last Arm.
func (h *CommitHold) Begun() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.begun
}

// Committed is how many write transactions committed since the last Arm, at
// once or once released.
func (h *CommitHold) Committed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.committed
}

// AwaitHeld blocks until n commits wait at the hold at once, or ctx ends.
func (h *CommitHold) AwaitHeld(ctx context.Context, n int) error {
	return h.await(ctx, func() bool { return h.holding >= n }, func() string {
		return fmt.Sprintf("%d commit(s) held, want %d", h.holding, n)
	})
}

// AwaitCommitted blocks until n write transactions have committed since the
// last Arm, or ctx ends.
func (h *CommitHold) AwaitCommitted(ctx context.Context, n int) error {
	return h.await(ctx, func() bool { return h.committed >= n }, func() string {
		return fmt.Sprintf("%d commit(s) since the hold was armed, want %d", h.committed, n)
	})
}

// await waits on changed until reached holds, both read under the lock.
func (h *CommitHold) await(ctx context.Context, reached func() bool, state func() string) error {
	for {
		h.mu.Lock()
		if reached() {
			h.mu.Unlock()
			return nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			h.mu.Lock()
			defer h.mu.Unlock()
			return fmt.Errorf("storetest: %s: %w", state(), ctx.Err())
		}
	}
}

func (h *CommitHold) changedLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// heldTx is one write transaction under a [CommitHold].
type heldTx struct {
	driver.Tx
	hold *CommitHold
}

// Commit waits while the hold is armed, then commits — or, released failing,
// rolls back and reports the failure, so nothing is durable.
func (t *heldTx) Commit() error {
	h := t.hold
	h.mu.Lock()
	release := h.release
	if release != nil {
		h.holding++
		h.changedLocked()
	}
	h.mu.Unlock()
	if release != nil {
		<-release
		h.mu.Lock()
		h.holding--
		fail := h.fail
		h.changedLocked()
		h.mu.Unlock()
		if fail != nil {
			// ROLLED BACK rather than left open, for [commitFaultTx]'s
			// reason: the fault is a commit that never landed, not a
			// transaction left wedged on its connection.
			_ = t.Tx.Rollback()
			return fail
		}
	}
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	h.mu.Lock()
	h.committed++
	h.changedLocked()
	h.mu.Unlock()
	return nil
}
