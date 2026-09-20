package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- the queue itself ------------------------------------------------- //

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
// The timeout's error is asserted by IDENTITY rather than by its text,
// because [classify] reads it that way: a queue timeout that stopped being
// errWritersQueued would stop being retried and nothing else would notice.
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
	if got := classify(errWritersQueued); got != causeLockTimeout {
		t.Errorf("the queue's own timeout classified as %s, want %s: it is the "+
			"same wait for the same lock ended by the same knob",
			causeName(got), causeName(causeLockTimeout))
	}
	// A WAIT THAT CANNOT FIRE, deliberately not the short one above. The
	// context is already done on entry, so both cases of acquire's select
	// are ready the moment a 20 ms timer expires — and Go picks a ready
	// case at random. A goroutine descheduled for 20 ms between the timer
	// and the select (a GC pause, a preemption under the detector on a
	// saturated runner) would make this a coin flip that re-runs green.
	// The short wait belongs to the case above, where the timer IS what is
	// under test.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := q.acquire(cancelled, time.Minute); !errors.Is(err, context.Canceled) {
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
// driver's own lock is a try-lock with a polling busy handler and serves
// nobody in order.
//
// A transaction holds the lock, the applier's pinned writer asks next, a
// pooled writer on a SECOND handle to the same file asks after it, and the
// holder lets go: the applier must go first. Before the queue, both would have
// polled the driver's lock and whichever happened to wake in the gap won.
//
// NO WALL-CLOCK ASSERTION ANYWHERE IN IT. Each writer is observed to be IN
// LINE before the next one is started, so the order under test is the order
// they asked in by construction rather than by having slept long enough — the
// property this file exists for is exactly the one a timing-dependent test
// could not hold.
//
// Mutation: take the lock without the queue in either write path, or give
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
	if db.writes != other.Replicated().writes {
		t.Fatal("two handles on one file took two queues: they share one " +
			"driver-level write lock, so they have to share one line for it")
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

// A READ QUEUES BEHIND NOTHING, which is the other half of the queue being
// worth having: it orders the writers, and a dashboard read that took a place
// in their line would be paying for a correctness fix it does not need.
//
// Mutation: acquire the queue in [DB.Read] as well, and the read below blocks
// until the writer lets go.
func TestAReadTakesNoPlaceInTheWriteQueue(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "readfree.db"),
		Options{BusyTimeout: time.Minute})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	hold, holding := make(chan struct{}), make(chan struct{})
	var once sync.Once
	wrote := make(chan error, 1)
	go func() {
		wrote <- db.Tx(ctx, func(*sql.Tx) error {
			once.Do(func() { close(holding) })
			<-hold
			return nil
		})
	}()
	<-holding

	read, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := db.Read(read, func(tx *sql.Tx) error {
		var n int
		return tx.QueryRowContext(read, `SELECT count(*) FROM schema_migrations`).Scan(&n)
	}); err != nil {
		t.Errorf("a read waited on a write transaction that holds the queue: %v", err)
	}
	close(hold)
	if err := <-wrote; err != nil {
		t.Fatalf("the holding write failed: %v", err)
	}
}
