package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// openPinned is the fixture the cases below share: a store sized for n pinned
// writers, with one probe table.
//
// IT RETURNS THE REPLICATED ESTATE, which is where a pin belongs: a pinned
// writer is an applier's, an applier writes the replicated tables, and the
// node estate is sized for readers alone.
func openPinned(t *testing.T, pins int) *store.DB {
	t.Helper()
	node, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "pinned.db"), store.Options{PinnedWriters: pins})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })
	db := node.Replicated()
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`CREATE TABLE crewlet_pin_probe (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// THE APPLIER IS NEVER QUEUED BEHIND READERS, and this is the shape that
// proves it: every reader connection the pool has is held open in a
// transaction, and the writer commits anyway.
//
// Without the pin the writer's transaction cannot begin at all until a reader
// releases — and the readers on a real node are usually waiting for exactly
// the record this writer is about to commit, which is a feedback loop with no
// floor rather than fair sharing.
//
// The assertion is on COMPLETION under a deadline, not on latency: a timing
// assertion here would be flaky on a loaded runner, while "did it commit at
// all while every other connection was occupied" is the property.
func TestTheApplierIsNeverQueuedBehindReaders(t *testing.T) {
	t.Parallel()
	db := openPinned(t, 1)

	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	// SATURATE THE WHOLE POOL, which is what makes the mutation reachable:
	// five readers are started against a pool of five (four readers plus
	// the one declared pin) and only four can get a connection while the
	// pin holds the fifth. Remove the pin — take the writer's connection
	// from the pool per transaction — and all five readers get one, so the
	// write has nothing left to begin on and this test goes red.
	//
	// Waiting for four rather than five is the point: the fifth is EXPECTED
	// to be blocked in the correct implementation.
	held := make(chan struct{})
	defer close(held)
	var up sync.WaitGroup
	up.Add(4)
	var once sync.Once
	var got int64
	for range 5 {
		go func() {
			_ = db.Read(t.Context(), func(tx *sql.Tx) error {
				var n int
				_ = tx.QueryRowContext(t.Context(),
					`SELECT count(*) FROM crewlet_pin_probe`).Scan(&n)
				if atomic.AddInt64(&got, 1) <= 4 {
					up.Done()
				} else {
					once.Do(func() {})
				}
				<-held
				return nil
			})
		}()
	}
	up.Wait()

	done := make(chan error, 1)
	go func() {
		done <- w.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(),
				`INSERT INTO crewlet_pin_probe (id, n) VALUES (1, 1)`)
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the pinned write failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pinned writer did not commit while every reader " +
			"connection was held: its connection is coming from the shared " +
			"pool, so it queues behind the readers waiting for it")
	}
}

// A pin past the declared count is REFUSED NAMING THE COUNT rather than taken
// from the readers' share.
//
// Mutation: hand out the connection anyway and the pool silently narrows —
// a node running three domains against a handle sized for two serves its
// dashboard from three connections instead of four, with nothing anywhere
// reporting it.
func TestAPinPastTheDeclaredCountIsRefused(t *testing.T) {
	t.Parallel()
	db := openPinned(t, 1)

	first, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("first Writer: %v", err)
	}
	defer func() { _ = first.Close() }()

	if _, err := db.Writer(t.Context()); err == nil {
		t.Fatal("a second writer was pinned against a handle that declared one")
	} else if !contains(err.Error(), "PinnedWriters") {
		t.Errorf("refusal = %q, want it to name store.Options.PinnedWriters, "+
			"which is the field the caller has to change", err)
	}

	// AND CLOSING RETURNS THE BUDGET: a domain that restarts its applier
	// must be able to re-pin, which is the re-pin-on-driver-error path.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	again, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("re-pin after Close: %v", err)
	}
	_ = again.Close()
}

// Writer.Tx and DB.Tx share ONE retry loop, so a conflicted read-then-write on
// the pinned connection is retried exactly as it is on a pooled one.
//
// The counter is the sharpest shape of it — pooled writers and the pinned one
// all read and write the same row — and the assertion is on the FINAL VALUE,
// because a lost update is silent by construction.
func TestWriterAndTxShareOneRetryLoop(t *testing.T) {
	t.Parallel()
	db := openPinned(t, 1)
	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO crewlet_pin_probe (id, n) VALUES (1, 0)`)
		return err
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	const each = 12
	bump := func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT n FROM crewlet_pin_probe WHERE id = 1`).Scan(&n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE crewlet_pin_probe SET n = ? WHERE id = 1`, n+1)
		return err
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3*each)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range each {
			if err := w.Tx(ctx, bump); err != nil {
				errs <- err
			}
		}
	}()
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if err := db.Tx(ctx, bump); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		failed++
		t.Logf("a transaction exhausted its retry budget: %v", err)
	}

	var got int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT n FROM crewlet_pin_probe WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got+failed != 3*each {
		t.Errorf("counter = %d with %d reported failures, want them to sum to %d: "+
			"%d increments were lost silently, which means the pinned path is "+
			"not retrying what the pooled one does", got, failed, 3*each,
			3*each-got-failed)
	}
}

// DB.Read gives fn ONE snapshot, which is the reason it is a transaction
// rather than two queries: a count and a listing taken separately against a
// store being written report a total the page under it does not match.
func TestReadSeesOneSnapshot(t *testing.T) {
	t.Parallel()
	db := openPinned(t, 0)
	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO crewlet_pin_probe (id, n) VALUES (1, 1)`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var first, second int
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM crewlet_pin_probe`).Scan(&first); err != nil {
			return err
		}
		// A COMMITTED WRITE LANDS BETWEEN THE TWO STATEMENTS.
		if err := db.Tx(ctx, func(w *sql.Tx) error {
			_, err := w.ExecContext(ctx,
				`INSERT INTO crewlet_pin_probe (id, n) VALUES (2, 2)`)
			return err
		}); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`SELECT count(*) FROM crewlet_pin_probe`).Scan(&second)
	}); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("Read: %v", err)
	}
	if first != second {
		t.Errorf("one transaction saw %d rows and then %d: a read that changes "+
			"under itself is what DB.Read exists to prevent", first, second)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
