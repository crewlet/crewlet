package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A purge statement holds the node's one writer for as long as it runs, and
// an inline event Append waits behind it no longer than the busy timeout. So
// what one statement deletes is bounded by payload bytes as well as rows: a
// log's rows are up to one whole event each, and a statement of five hundred
// of those would hold the writer for seconds.

// purgeLog opens a log for a purge case.
func purgeLog(t *testing.T) (*DB, *EventLog) {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "purge.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, db.Events()
}

// stale appends a row past retention whose payload weighs size bytes, and
// returns its id.
func stale(t *testing.T, log *EventLog, i, size int) string {
	t.Helper()
	id := fmt.Sprintf("stale-%04d", i)
	payload := `{"x":"` + strings.Repeat("x", size-8) + `"}`
	if err := log.Append(t.Context(), EventRecord{
		ID: id, Type: "task_assigned", Category: "task",
		Time:    now().Add(-EventRetention - time.Hour).Add(time.Duration(i) * time.Millisecond),
		Payload: json.RawMessage(payload),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
	return id
}

// staleRows inserts n rows past retention, each carrying size bytes of
// payload, in one transaction: a case that needs more rows than a batch holds
// is seeded without a commit for each.
func staleRows(t *testing.T, db *DB, from, n, size int) {
	t.Helper()
	payload := `{"x":"` + strings.Repeat("x", size-8) + `"}`
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := from; i < from+n; i++ {
			at := now().Add(-EventRetention - time.Hour).Add(time.Duration(i) * time.Millisecond)
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO crewlet_events (event_time, event_id, event_type, payload) VALUES (?, ?, ?, ?)`,
				EncodeTime(at), fmt.Sprintf("stale-%04d", i), "task_assigned", payload); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// A BATCH STOPS AT WHICHEVER BOUND IT REACHES FIRST, AND TAKES ONE ROW AT LEAST.
//
// Oldest first: the rows that fit the byte bound go together, a row larger
// than the bound on its own goes in a batch of its own rather than never, and
// the row bound holds however small the rows are.
func TestAPurgeBatchStopsAtWhicheverBoundItReachesFirst(t *testing.T) {
	t.Parallel()
	_, log := purgeLog(t)
	const kb = 1 << 10
	for i, size := range []int{10 * kb, 10 * kb, 30 * kb, 10 * kb, 100 * kb, 10 * kb, 1 * kb, 1 * kb, 1 * kb} {
		stale(t, log, i, size)
	}
	if err := log.Append(t.Context(), EventRecord{ID: "recent", Type: "task_assigned",
		Category: "task", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cutoff := EncodeTime(now().Add(-EventRetention))

	// Rows per batch, with a bound of 25 KiB of payload and three rows.
	for i, want := range []struct {
		rows int
		more bool
	}{
		{2, true}, // 10 + 10; the 30 would cross the bound
		{1, true}, // the 30 alone: larger than the bound, and one row at least
		{1, true}, // 10; the 100 would cross the bound
		{1, true}, // the 100 alone
		{3, true}, // 10 + 1 + 1: the row bound, with a row still to come
		{1, false},
	} {
		batch, more, err := log.nextPurgeBatch(t.Context(), cutoff, 3, 25*kb)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.rowids) != want.rows || more != want.more {
			t.Fatalf("batch %d is %d rows (%d bytes), more %v; want %d rows, more %v",
				i, len(batch.rowids), batch.bytes, more, want.rows, want.more)
		}
		if len(batch.rowids) > 1 && batch.bytes > 25*kb {
			t.Fatalf("batch %d is %d rows weighing %d bytes, past the bound", i, len(batch.rowids), batch.bytes)
		}
		if _, err := log.deleteBatch(t.Context(), cutoff, batch); err != nil {
			t.Fatal(err)
		}
	}
	if batch, _, err := log.nextPurgeBatch(t.Context(), cutoff, 3, 25*kb); err != nil || len(batch.rowids) != 0 {
		t.Fatalf("a batch of %d rows after the overhang was deleted (%v)", len(batch.rowids), err)
	}
	if _, err := log.ByID(t.Context(), "recent"); err != nil {
		t.Errorf("a row inside retention is gone: %v", err)
	}
}

// purgeWatch measures the DELETE statements a store runs, as its driver runs
// them: for each one on the event log, how many rows and how many bytes of
// payload it removed, and for each one on the party index, how many rows. It
// is installed as [Options.WrapDriver], so what it measures is what Purge
// itself sent, whatever shape its statements take.
//
// The event log's are measured by what is GONE after each statement rather
// than by the statement's text, so one DELETE over every row past the cutoff
// would be measured the same way as a batch of them.
type purgeWatch struct {
	mu sync.Mutex
	// armed is set once the rows are seeded, so the seeding and the schema's
	// own statements are not measured.
	armed bool
	// sizes is every event row's payload, in bytes, by rowid, and left the
	// rowids still in the table after the last statement measured.
	sizes map[int64]int64
	left  map[int64]bool

	events  []purgeStatement
	parties []int64
}

// purgeStatement is one DELETE on the event log.
type purgeStatement struct {
	rows  int
	bytes int64
}

// arm reads every event row's size and starts measuring.
func (w *purgeWatch) arm(t *testing.T, db *DB) {
	t.Helper()
	rows, err := db.SQL().QueryContext(t.Context(), `SELECT rowid, octet_length(payload) FROM crewlet_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sizes, w.left = map[int64]int64{}, map[int64]bool{}
	for rows.Next() {
		var rowid, size int64
		if err := rows.Scan(&rowid, &size); err != nil {
			t.Fatal(err)
		}
		w.sizes[rowid], w.left[rowid] = size, true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	w.armed = true
}

// saw measures one statement conn just ran.
func (w *purgeWatch) saw(ctx context.Context, conn driver.Conn, query string, res driver.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed {
		return nil
	}
	switch {
	case strings.HasPrefix(query, "DELETE FROM crewlet_event_parties"):
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		w.parties = append(w.parties, n)
	case strings.HasPrefix(query, "DELETE FROM crewlet_events"):
		// What is left, read on the connection that ran the delete.
		rows, err := conn.(driver.QueryerContext).QueryContext(ctx, `SELECT rowid FROM crewlet_events`, nil)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		still := map[int64]bool{}
		dest := make([]driver.Value, 1)
		for {
			if err := rows.Next(dest); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			rowid, _ := dest[0].(int64)
			still[rowid] = true
		}
		var gone purgeStatement
		for rowid := range w.left {
			if !still[rowid] {
				gone.rows++
				gone.bytes += w.sizes[rowid]
			}
		}
		w.events, w.left = append(w.events, gone), still
	}
	return nil
}

func (w *purgeWatch) wrap(d driver.Driver) driver.Driver { return watchedDriver{inner: d, watch: w} }

type watchedDriver struct {
	inner driver.Driver
	watch *purgeWatch
}

func (d watchedDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &watchedConn{Conn: conn, watch: d.watch}, nil
}

// watchedConn forwards every optional interface the store's own connection
// satisfies, explicitly: an embedded driver.Conn carries none of them, and a
// wrapper that hid one would move every statement onto another path.
type watchedConn struct {
	driver.Conn
	watch *purgeWatch
}

func (c *watchedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	res, err := ex.ExecContext(ctx, q, args)
	if err != nil {
		return res, err
	}
	if err := c.watch.saw(ctx, c.Conn, q, res); err != nil {
		return nil, fmt.Errorf("measure %q: %w", q, err)
	}
	return res, nil
}

func (c *watchedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c *watchedConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c *watchedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, fmt.Errorf("the store's connection %T begins no transaction with options", c.Conn)
	}
	return bt.BeginTx(ctx, opts)
}

func (c *watchedConn) Ping(ctx context.Context) error {
	p, ok := c.Conn.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

func (c *watchedConn) IsValid() bool {
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

func (c *watchedConn) RetireSwitch() func() {
	r, ok := c.Conn.(retirer)
	if !ok {
		return func() {}
	}
	return r.RetireSwitch()
}

// watchedLog opens a log whose statements w measures.
func watchedLog(t *testing.T, w *purgeWatch) (*DB, *EventLog) {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "purge.db"), Options{WrapDriver: w.wrap})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, db.Events()
}

// A PURGE NEVER HOLDS ONE STATEMENT PAST EITHER BOUND, AND TAKES EVERY ROW.
//
// Measured on the statements Purge itself runs: an overhang with more rows
// than the row bound, and then rows the size of the largest event the
// transport carries less a mebibyte — what a phase record's parts come close
// to — more of them together than the byte bound. Every statement is within
// [EventPurgeBatch] rows, and within [EventPurgeBytes] of payload unless it is
// one row; and they all go, while a row inside retention stays.
func TestAPurgeNeverHoldsAStatementPastEitherBound(t *testing.T) {
	t.Parallel()
	watch := &purgeWatch{}
	db, log := watchedLog(t, watch)
	const small, large = EventPurgeBatch + 100, 5
	staleRows(t, db, 0, small, 1<<10)
	for i := range large {
		stale(t, log, small+i, 7<<20)
	}
	if large*(7<<20) <= EventPurgeBytes {
		t.Fatalf("the large rows weigh %d bytes together; the case needs more than the %d-byte bound",
			large*(7<<20), EventPurgeBytes)
	}
	if err := log.Append(t.Context(), EventRecord{ID: "recent", Type: "task_assigned",
		Category: "task", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	watch.arm(t, db)

	deleted, err := log.Purge(t.Context())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != small+large {
		t.Errorf("the purge reports %d rows deleted of the %d past retention", deleted, small+large)
	}
	gone := 0
	for i, s := range watch.events {
		gone += s.rows
		if s.rows > EventPurgeBatch {
			t.Errorf("statement %d deleted %d rows, past the %d-row bound", i, s.rows, EventPurgeBatch)
		}
		if s.rows > 1 && s.bytes > EventPurgeBytes {
			t.Errorf("statement %d deleted %d rows weighing %d bytes, past the %d-byte bound",
				i, s.rows, s.bytes, EventPurgeBytes)
		}
	}
	if gone != small+large {
		t.Errorf("the statements measured deleted %d rows of the %d past retention", gone, small+large)
	}
	if _, err := log.ByID(t.Context(), "recent"); err != nil {
		t.Errorf("a row inside retention is gone: %v", err)
	}
}

// THE PARTY INDEX IS PURGED IN BATCHES, AND ALL OF IT.
//
// Its rows are small, but a multi-day overhang holds several for every event,
// and one statement over all of them would hold the writer for the whole
// overhang, which is what batching the events' own delete exists to stop. A
// loop that stopped after its first batch would leave the rest pointing at
// events the sweep then deletes. Measured on the statements Purge runs: none
// removes more than [eventPartyPurgeBatch] rows, and together they remove
// every row past retention.
func TestThePartyIndexIsPurgedInBatchesAndAllOfIt(t *testing.T) {
	t.Parallel()
	watch := &purgeWatch{}
	db, log := watchedLog(t, watch)
	old := now().Add(-EventRetention - time.Hour)
	overhang := 2*eventPartyPurgeBatch + 5
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range overhang {
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES (?, ?, ?)`,
				"lead", EncodeTime(old.Add(time.Duration(i)*time.Microsecond)), uuid.NewString()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES ('lead', ?, 'recent')`,
			EncodeTime(now().Add(-time.Hour)))
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	watch.arm(t, db)

	if _, err := log.Purge(t.Context()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	removed := int64(0)
	for i, n := range watch.parties {
		removed += n
		if n > eventPartyPurgeBatch {
			t.Errorf("statement %d removed %d party rows, past the %d-row bound", i, n, eventPartyPurgeBatch)
		}
	}
	if removed != int64(overhang) {
		t.Errorf("the statements measured removed %d party rows of the %d past retention", removed, overhang)
	}
	var left int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM crewlet_event_parties`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("%d party rows remain of the %d past retention and the one inside it; want the one",
			left, overhang)
	}
}
