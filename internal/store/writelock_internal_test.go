package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- the queue -------------------------------------------------------- //

// THE QUEUE SERVES WRITERS IN THE ORDER THEY ASKED, and a writer arriving at
// the moment of a release cannot barge past one already waiting.
//
// Mutation: release by popping the LAST waiter, or by clearing held and
// letting the waiters race for it, and the order below comes back wrong or
// the barging writer gets in first.
func TestTheWriteQueueServesWritersInTheOrderTheyAsked(t *testing.T) {
	t.Parallel()
	q := &writeQueue{}
	ctx := t.Context()
	if err := q.acquire(ctx, time.Minute); err != nil {
		t.Fatalf("an idle queue refused its first writer: %v", err)
	}

	order := make(chan string, 2)
	var wg sync.WaitGroup
	// enqueue starts a writer and returns once it is in line. A writer
	// given a gate keeps the queue until the gate opens.
	enqueue := func(name string, ahead int, gate chan struct{}) {
		wg.Go(func() {
			if err := q.acquire(ctx, time.Minute); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			order <- name
			if gate != nil {
				<-gate
			}
			q.release()
		})
		waitForWaiters(t, q, ahead+1)
	}
	gate := make(chan struct{})
	enqueue("first", 0, gate)
	enqueue("second", 1, nil)

	// THE HANDOFF: releasing gives the queue to "first" without ever
	// freeing it, so a writer asking now waits rather than slipping in.
	q.release()
	barger, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := q.acquire(barger, time.Minute); err == nil {
		t.Error("a writer that asked after the release took the queue ahead " +
			"of two that were already waiting")
		q.release()
	}
	close(gate)
	wg.Wait()
	close(order)
	var got []string
	for name := range order {
		got = append(got, name)
	}
	if strings.Join(got, ",") != "first,second" {
		t.Errorf("writers began in the order %v, want the order they asked in "+
			"(first, second)", got)
	}
}

// A WRITER THAT GIVES UP LEAVES THE LINE, whether its busy timeout ran out or
// its context ended, and leaves the queue free behind it.
//
// Mutation: leave the waiter in the slice, and the release below hands the
// queue to a writer that has already returned, which nobody then releases.
func TestAWriterThatGivesUpLeavesTheLine(t *testing.T) {
	t.Parallel()
	q := &writeQueue{}
	ctx := t.Context()
	const wait = 20 * time.Millisecond
	if err := q.acquire(ctx, wait); err != nil {
		t.Fatalf("an idle queue refused its first writer: %v", err)
	}
	if err := q.acquire(ctx, wait); !errors.Is(err, errWritersQueued) {
		t.Errorf("a writer that waited past the busy timeout got %v, want %v, "+
			"which the retry classifier knows by identity", err, errWritersQueued)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := q.acquire(cancelled, wait); !errors.Is(err, context.Canceled) {
		t.Errorf("a writer whose context ended got %v, want %v", err, context.Canceled)
	}
	waitForWaiters(t, q, 0)

	q.release()
	quick, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := q.acquire(quick, wait); err != nil {
		t.Fatalf("the queue was not free after its only holder released it "+
			"and every waiter gave up: %v", err)
	}
	q.release()
}

// A GIVE-UP THAT LOSES THE RACE TO THE HANDOFF PASSES THE QUEUE ON.
//
// The race cannot be staged through acquire, because which case a select
// takes when both are ready is the runtime's choice; so its two halves are
// driven directly. A waiter handed the queue before it could leave the line
// holds it, and must be told so, or the queue stays held by nobody.
//
// Mutation: have leave report false unconditionally, and the queue below is
// never freed.
func TestAGiveUpThatLosesTheRaceToTheHandoffPassesTheQueueOn(t *testing.T) {
	t.Parallel()
	q := &writeQueue{}
	if _, holds := q.join(); !holds {
		t.Fatal("an idle queue did not hand its first writer the queue")
	}
	turn, holds := q.join()
	if holds {
		t.Fatal("a second writer took a queue that was held")
	}
	q.release() // the handoff, before the waiter notices

	if !q.leave(turn) {
		t.Fatal("a waiter that was handed the queue was told it was still in " +
			"line, so it drops the queue and every writer behind it stalls")
	}
	q.release()
	if _, holds := q.join(); !holds {
		t.Error("the queue is still held after the late waiter passed it on")
	}
}

// waitForWaiters blocks until q has exactly n writers in line.
func waitForWaiters(t *testing.T, q *writeQueue, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		q.mu.Lock()
		got := len(q.waiters)
		q.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d writer(s) in line, want %d: a writer that should be "+
				"waiting in this database's queue is not in it", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- the queue, through the store ------------------------------------- //

// WRITERS BEGIN IN THE ORDER THEY ASKED, pooled and pinned alike and from
// every handle on the file: the fact the applier's drain rests on, because the
// driver's own lock is a try-lock with a polling busy handler and serves nobody
// in order.
//
// A transaction holds the lock, the applier's pinned writer asks next, a
// pooled writer on a SECOND handle to the same file asks after it, and the
// holder lets go: the applier must go first. Before the queue, both would have
// polled the driver's lock and whichever happened to wake in the gap won.
//
// Mutation: take the lock without the queue, in either write path, or give
// each handle a queue of its own, and a writer never appears in line; release
// the waiters in any other order, and the rows land in it.
func TestWritersBeginInTheOrderTheyAsked(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := Open(ctx, filepath.Join(t.TempDir(), "order.db"),
		Options{PinnedWriters: 1, BusyTimeout: time.Minute})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	other, err := Open(ctx, node.Path(), Options{BusyTimeout: time.Minute})
	if err != nil {
		t.Fatalf("open a second handle on the same file: %v", err)
	}
	defer func() { _ = other.Close() }()
	db := node.Replicated()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE order_probe (
			seq INTEGER PRIMARY KEY AUTOINCREMENT, who TEXT NOT NULL)`)
		return err
	}); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	record := func(who string) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO order_probe (who) VALUES (?)`, who)
			return err
		}
	}
	hold, holding := make(chan struct{}), make(chan struct{})
	var once sync.Once
	done := make(chan error, 3)
	go func() {
		done <- db.Tx(ctx, func(tx *sql.Tx) error {
			once.Do(func() { close(holding) })
			<-hold
			return record("holder")(tx)
		})
	}()
	<-holding
	go func() { done <- w.Tx(ctx, record("applier")) }()
	waitForWaiters(t, db.writes, 1)
	go func() { done <- other.Replicated().Tx(ctx, record("pooled")) }()
	waitForWaiters(t, db.writes, 2)
	close(hold)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatalf("a writer failed: %v", err)
		}
	}

	var got []string
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT who FROM order_probe ORDER BY seq`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var who string
			if err := rows.Scan(&who); err != nil {
				return err
			}
			got = append(got, who)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the order back: %v", err)
	}
	if strings.Join(got, ",") != "holder,applier,pooled" {
		t.Errorf("writers committed in the order %v, want the order they asked "+
			"in (holder, applier, pooled)", got)
	}
}

// ---- the BEGIN -------------------------------------------------------- //

// A WRITE TRANSACTION HOLDS THE LOCK FROM ITS BEGIN, before it has read or
// written anything, and a read transaction holds nothing.
//
// This is the half of writelock.go that makes an abort impossible rather than
// merely unlikely: a transaction that holds the lock from BEGIN has no window
// between its snapshot and its first write for another commit to land in. The
// probe here is a statement outside any transaction, which does not queue and
// meets only the driver's lock, so it sees exactly what BEGIN took.
//
// Mutation: map the write option to a plain BEGIN, as the driver does for
// every option, and the statement below commits straight past the "write"
// transaction.
func TestAWriteTransactionHoldsTheLockFromItsBegin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "begin.db"),
		Options{BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE begin_probe (n INTEGER NOT NULL)`)
		return err
	}); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}

	for _, c := range []struct {
		name   string
		opts   *sql.TxOptions
		locked bool
	}{
		{"a write transaction", writeTx, true},
		{"a read transaction", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, err := db.sql.Conn(ctx)
			if err != nil {
				t.Fatalf("draw a connection: %v", err)
			}
			defer func() { _ = conn.Close() }()
			tx, err := conn.BeginTx(ctx, c.opts)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			_, err = db.sql.ExecContext(ctx, `INSERT INTO begin_probe (n) VALUES (1)`)
			locked := err != nil && strings.Contains(err.Error(), "database is locked")
			if err != nil && !locked {
				t.Fatalf("the probe statement failed for another reason: %v", err)
			}
			if locked != c.locked {
				t.Errorf("a statement beside %s that had done nothing yet was "+
					"locked out: %v, want %v", c.name, locked, c.locked)
			}
		})
	}
}

// AN OPTION THE DRIVER CANNOT GIVE IS REFUSED, where the driver itself accepts
// every option and begins the same deferred transaction for all of them.
//
// Mutation: fall through to a plain BEGIN for an unknown option, and a caller
// asking for a read-only transaction gets one that writes.
func TestTheBeginRefusesWhatItCannotGive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "refuse.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for name, opts := range map[string]*sql.TxOptions{
		"read-only":      {ReadOnly: true},
		"read committed": {Isolation: sql.LevelReadCommitted},
		"linearizable":   {Isolation: sql.LevelLinearizable},
	} {
		tx, err := db.sql.BeginTx(ctx, opts)
		if err == nil {
			_ = tx.Rollback()
			t.Errorf("a %s transaction was begun, as a plain deferred one", name)
		}
	}
}

// THE BEGIN ADAPTER HIDES NO INTERFACE OF THE DRIVER'S CONNECTION.
//
// database/sql chooses its query path from the optional interfaces a
// connection implements, and a wrapper carries only the ones it declares. So a
// driver bump that adds one (a value checker, a session resetter) would be
// silently dropped by the adapter, and every statement would take a different
// path than the driver meant. This is the tripwire for that.
func TestTheBeginAdapterHidesNoInterfaceOfTheDriver(t *testing.T) {
	t.Parallel()
	if err := prepareTursoLibrary(); err != nil {
		t.Fatalf("prepare the driver's library: %v", err)
	}
	probe, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("resolve the driver: %v", err)
	}
	drv := probe.Driver()
	_ = probe.Close()

	raw, err := drv.Open(":memory:")
	if err != nil {
		t.Fatalf("open a raw connection: %v", err)
	}
	defer func() { _ = raw.Close() }()
	wrapped, err := beginDriver{inner: drv}.Open(":memory:")
	if err != nil {
		t.Fatalf("open a wrapped connection: %v", err)
	}
	defer func() { _ = wrapped.Close() }()

	for name, has := range map[string]func(driver.Conn) bool{
		"ExecerContext":      func(c driver.Conn) bool { _, ok := c.(driver.ExecerContext); return ok },
		"QueryerContext":     func(c driver.Conn) bool { _, ok := c.(driver.QueryerContext); return ok },
		"ConnPrepareContext": func(c driver.Conn) bool { _, ok := c.(driver.ConnPrepareContext); return ok },
		"ConnBeginTx":        func(c driver.Conn) bool { _, ok := c.(driver.ConnBeginTx); return ok },
		"Pinger":             func(c driver.Conn) bool { _, ok := c.(driver.Pinger); return ok },
		"NamedValueChecker":  func(c driver.Conn) bool { _, ok := c.(driver.NamedValueChecker); return ok },
		"SessionResetter":    func(c driver.Conn) bool { _, ok := c.(driver.SessionResetter); return ok },
		"Validator":          func(c driver.Conn) bool { _, ok := c.(driver.Validator); return ok },
	} {
		if has(raw) && !has(wrapped) {
			t.Errorf("the driver's connection implements driver.%s and the begin "+
				"adapter hides it: forward it in writelock.go", name)
		}
	}
}
