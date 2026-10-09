package store

import (
	"context"
	"database/sql/driver"
)

// driverHooks wraps a driver so a case can act on what a pool sends it before
// the driver sees it: hold a BEGIN, or answer a query in the driver's place.
// Either hook may be nil, and what neither intercepts reaches the real driver
// on the real file.
type driverHooks struct {
	// begin runs before every BEGIN; an error refuses it.
	begin func(ctx context.Context) error
	// query runs before every query; an error is the query's answer.
	query func(q string) error
}

// wrap is the [Options.WrapDriver] function for these hooks.
func (h *driverHooks) wrap(d driver.Driver) driver.Driver {
	return &hookedDriver{inner: d, hooks: h}
}

type hookedDriver struct {
	inner driver.Driver
	hooks *driverHooks
}

func (d *hookedDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &hookedConn{Conn: conn, hooks: d.hooks}, nil
}

// hookedConn forwards every optional interface the store's connections carry,
// for the reason storetest's fault conn gives: an embedded driver.Conn carries
// none of them, and hiding them moves every statement onto a path production
// never takes.
type hookedConn struct {
	driver.Conn
	hooks *driverHooks
}

func (c *hookedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *hookedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.hooks.query != nil {
		if err := c.hooks.query(q); err != nil {
			return nil, err
		}
	}
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *hookedConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *hookedConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

func (c *hookedConn) RetireSwitch() func() {
	r, ok := c.Conn.(interface{ RetireSwitch() func() })
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
}

func (c *hookedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.hooks.begin != nil {
		if err := c.hooks.begin(ctx); err != nil {
			return nil, err
		}
	}
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		return c.Conn.Begin()
	}
	return bt.BeginTx(ctx, opts)
}
