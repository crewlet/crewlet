package storetest

import (
	"context"
	"database/sql/driver"
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
