package storetest

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync"
	"time"
)

// CopyHoldWait is how long a case waits for held copies to reach their
// VACUUM INTO, or for a copy it let through to return, before it fails rather
// than hangs.
//
// A HANG DETECTOR, NOT A BUDGET: a copy of a test's estate reaches its VACUUM
// INTO, and returns once let through, in well under a second on two cores
// under the race detector. What it has to tell apart from that is a copy that
// never gets there — waiting on a connection its pool cannot give it, which
// it does for ever — and half a minute does so under any load a test binary
// meets while failing well inside the binary's own ten-minute timeout, which
// would otherwise be the first thing to notice.
const CopyHoldWait = 30 * time.Second

// CopyHold holds every copy's VACUUM INTO back while it is armed: a copy it
// holds has drawn its connection from the pool, taken its view and holds its
// file's write-ahead log, and waits there until [CopyHold.Release]. It is how
// a case stands several copies at the same point at once, sees a copy in
// flight, or writes while one holds the log.
//
// Pass [CopyHold.Wrap] to store.Options.WrapDriver, then [CopyHold.Arm] once
// the setup has run: nothing is held until then.
type CopyHold struct {
	mu sync.Mutex

	// gate is what the copies held now wait on, nil while disarmed.
	gate *copyGate

	// held is how many copies wait on a gate now, and changed is closed —
	// and replaced — every time that number moves, so [CopyHold.Wait]
	// sleeps on it rather than polling.
	held    int
	changed chan struct{}
}

// copyGate is one arming's release: closed by [CopyHold.Release], with the
// error the copies it held answer, nil to let them run.
type copyGate struct {
	open chan struct{}
	err  error
}

// NewCopyHold is a hold, disarmed.
func NewCopyHold() *CopyHold { return &CopyHold{changed: make(chan struct{})} }

// Wrap is the store.Options.WrapDriver function for this hold.
//
// A method rather than a returned closure, for [Fault.Wrap]'s reason: the
// handle a case arms is then the one the open database is using.
func (h *CopyHold) Wrap(d driver.Driver) driver.Driver {
	return &copyHoldDriver{inner: d, hold: h}
}

// Arm holds every copy that reaches its VACUUM INTO from now until the next
// [CopyHold.Release].
func (h *CopyHold) Arm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gate = &copyGate{open: make(chan struct{})}
}

// Release lets every copy held now go on — to its VACUUM INTO, or, with a
// non-nil err, to fail with err in its place — and disarms the hold, so the
// copies that come after run unheld.
func (h *CopyHold) Release(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gate == nil {
		return
	}
	h.gate.err = err
	close(h.gate.open)
	h.gate = nil
}

// Wait waits up to within for n copies to be held at once, and answers how
// many are when it returns: n once they are, fewer when within ran out first.
func (h *CopyHold) Wait(n int, within time.Duration) int {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		h.mu.Lock()
		held, changed := h.held, h.changed
		h.mu.Unlock()
		if held >= n {
			return held
		}
		select {
		case <-changed:
		case <-timer.C:
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.held
		}
	}
}

// hold is what a copy's VACUUM INTO does first: wait on the gate while the
// hold is armed, or on ctx, which a case's own context ends with the case.
func (h *CopyHold) hold(ctx context.Context) error {
	h.mu.Lock()
	gate := h.gate
	if gate == nil {
		h.mu.Unlock()
		return nil
	}
	h.count(1)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.count(-1)
		h.mu.Unlock()
	}()
	select {
	case <-gate.open:
		return gate.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// count moves the number of copies held and wakes every [CopyHold.Wait].
// Caller holds h.mu.
func (h *CopyHold) count(by int) {
	h.held += by
	close(h.changed)
	h.changed = make(chan struct{})
}

type copyHoldDriver struct {
	inner driver.Driver
	hold  *CopyHold
}

func (d *copyHoldDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &copyHoldConn{Conn: conn, hold: d.hold}, nil
}

// copyHoldConn holds a VACUUM INTO and forwards everything else, every optional
// interface explicitly, for faultConn's reason: an embedded driver.Conn carries
// none of them, and hiding them moves every statement onto a path production
// never takes.
type copyHoldConn struct {
	driver.Conn
	hold *CopyHold
}

// ExecContext is where a copy is held: the store issues its VACUUM INTO here,
// on the connection it drew from the pool, once the view the copy is taken at
// is held — and nothing else it sends begins with those words.
func (c *copyHoldConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(q, "VACUUM INTO ") {
		if err := c.hold.hold(ctx); err != nil {
			return nil, err
		}
	}
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *copyHoldConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *copyHoldConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

// IsValid forwards the wrapped connection's own verdict, for faultConn's
// reason: it is how the store retires a connection whose transaction may still
// be open.
func (c *copyHoldConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

// RetireSwitch forwards the wrapped connection's retire switch, which the store
// takes out at the draw.
func (c *copyHoldConn) RetireSwitch() func() {
	r, ok := c.Conn.(interface{ RetireSwitch() func() })
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
}

func (c *copyHoldConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		return c.Conn.Begin()
	}
	return bt.BeginTx(ctx, opts)
}
