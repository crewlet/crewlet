package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// scriptedRead answers its rows in order and then, where fail is set, fails
// instead of ending.
type scriptedRead struct {
	rows []spendRow
	fail error
	pos  int
}

func (r *scriptedRead) next(context.Context) (spendRow, bool, error) {
	if r.pos < len(r.rows) {
		r.pos++
		return r.rows[r.pos-1], true, nil
	}
	if r.fail != nil {
		return spendRow{}, false, r.fail
	}
	return spendRow{}, false, nil
}

// rowsAt is a read's rows at the given instants, newest first, each id naming its
// instant.
func rowsAt(instants ...int64) []spendRow {
	rows := make([]spendRow, len(instants))
	for i, t := range instants {
		rows[i] = spendRow{at: t, rec: tokens.Record{EventID: fmt.Sprintf("t%02d", t)}}
	}
	return rows
}

func recordIDs(recs []tokens.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.EventID
	}
	return out
}

// THE MERGE TAKES THE NEWEST OF EVERY READ, and one past the limit as the
// evidence that there is more.
func TestTakeNewestKeepsTheNewestOfEveryRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		limit int
		want  []string
		more  bool
	}{
		{5, []string{"t10", "t09", "t08", "t07", "t04"}, true},
		{7, []string{"t10", "t09", "t08", "t07", "t04", "t02", "t01"}, false},
	} {
		got, more, err := takeNewest(t.Context(), []spendRead{
			&scriptedRead{rows: rowsAt(10, 7, 4, 1)},
			&scriptedRead{rows: rowsAt(9, 8, 2)},
		}, c.limit)
		if err != nil || !slices.Equal(recordIDs(got), c.want) || more != c.more {
			t.Errorf("limit %d: %v (more=%v), %v; want %v (more=%v)", c.limit, recordIDs(got), more, err, c.want, c.more)
		}
	}
}

// A READ THAT FAILS PART-WAY KEEPS WHAT THE MERGE TOOK, which is the newest of
// the window: never a record it had in hand but had not taken, since the read
// that failed could have held one newer. This is what lets the live
// projection's seed keep the newest records a slow store gave it before its
// deadline rather than none.
func TestTakeNewestKeepsWhatItTookWhenAReadFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("the deadline passed")
	got, more, err := takeNewest(t.Context(), []spendRead{
		&scriptedRead{rows: rowsAt(10, 7, 4, 1)},
		// Two records and then the failure: whether a third would have come
		// before t07 is exactly what nobody knows.
		&scriptedRead{rows: rowsAt(9, 8), fail: boom},
	}, 5)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the read's own", err)
	}
	if want := []string{"t10", "t09", "t08"}; !slices.Equal(recordIDs(got), want) || !more {
		t.Errorf("taken %v (more=%v), want %v and more: t07 was in hand, but the read "+
			"that failed could have held one newer", recordIDs(got), more, want)
	}
}

// statementWatch counts, for every result set a store reads once armed, the
// rows it answered, and the result sets closed before their end.
type statementWatch struct {
	mu        sync.Mutex
	armed     bool
	rows      int
	abandoned int
}

func (w *statementWatch) wrap(d driver.Driver) driver.Driver {
	return statementDriver{inner: d, watch: w}
}

func (w *statementWatch) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = true
}

func (w *statementWatch) counted() (rows, abandoned int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rows, w.abandoned
}

type statementDriver struct {
	inner driver.Driver
	watch *statementWatch
}

func (d statementDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &statementConn{Conn: conn, watch: d.watch}, nil
}

// statementConn forwards every optional interface the store's own connection
// satisfies, explicitly: an embedded driver.Conn carries none of them, and a
// wrapper that hid one would move every statement onto another path.
type statementConn struct {
	driver.Conn
	watch *statementWatch
}

func (c *statementConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ex.ExecContext(ctx, q, args)
}

func (c *statementConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := qr.QueryContext(ctx, q, args)
	if err != nil {
		return rows, err
	}
	c.watch.mu.Lock()
	armed := c.watch.armed
	c.watch.mu.Unlock()
	if !armed {
		return rows, nil
	}
	return &statementRows{Rows: rows, watch: c.watch}, nil
}

func (c *statementConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *statementConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, fmt.Errorf("the store's connection %T begins no transaction with options", c.Conn)
	}
	return bt.BeginTx(ctx, opts)
}

func (c *statementConn) Ping(ctx context.Context) error {
	p, ok := c.Conn.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

func (c *statementConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

func (c *statementConn) RetireSwitch() func() {
	r, ok := c.Conn.(retirer)
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
}

type statementRows struct {
	driver.Rows
	watch *statementWatch
	ended bool
}

func (r *statementRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	switch {
	case errors.Is(err, io.EOF):
		r.ended = true
	case err == nil:
		r.watch.mu.Lock()
		r.watch.rows++
		r.watch.mu.Unlock()
	}
	return err
}

func (r *statementRows) Close() error {
	if !r.ended {
		r.watch.mu.Lock()
		r.watch.abandoned++
		r.watch.mu.Unlock()
	}
	return r.Rows.Close()
}

// THE TAIL READS EVERY STATEMENT TO ITS END, AND NO FURTHER THAN IT NEEDS.
//
// This driver finishes a statement closed part-way by stepping through the
// rest of its rows, so a read that left one open for its merge and stopped
// early would pay for every row it did not take — the whole remainder of the
// window — and a statement read whole to spare the close would read that
// remainder outright. Either way the cost of the seed would be the window's
// rather than the tail's. So: no statement closed before its end, and no more
// rows read than the tail, its evidence record, and one statement's worth
// beyond them for each type.
func TestATailReadsItsStatementsToTheirEndAndNoFurther(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	watch := &statementWatch{}
	db, err := Open(ctx, filepath.Join(t.TempDir(), "tail.db"), Options{WrapDriver: watch.wrap})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)
	held := 4 * spendChunk
	for i := range held {
		eventType := "agent_phase_completed"
		if i%4 == 1 {
			eventType = "auxiliary_call_completed"
		}
		if err := log.Append(ctx, EventRecord{
			ID: fmt.Sprintf("r-%05d", i), Type: eventType, Category: "lifecycle",
			Time: base.Add(time.Duration(i) * time.Millisecond), Payload: []byte(`{"total_tokens":1}`),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	watch.arm()
	limit := spendChunk + spendChunk/2
	tail, more, err := log.PhaseTokenTail(ctx, PhaseTokenQuery{SinceDays: 1}, limit)
	if err != nil || len(tail) != limit || !more {
		t.Fatalf("tail = %d records (more=%v), %v; want %d and more", len(tail), more, err, limit)
	}
	rows, abandoned := watch.counted()
	if abandoned != 0 {
		t.Errorf("%d statements were closed before their end", abandoned)
	}
	if ceiling := limit + 1 + len(spendEventTypes)*spendChunk; rows > ceiling {
		t.Errorf("the tail read %d rows of %d for %d records, want at most %d", rows, held, limit, ceiling)
	}
}
