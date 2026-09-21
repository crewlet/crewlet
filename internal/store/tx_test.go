package store_test

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A READ-THEN-WRITE CANNOT LOSE A RACE, AND ITS BODY RUNS ONCE.
//
// The driver's default BEGIN is deferred and its conflict detection is per
// FILE: a transaction that reads a row another writer has since advanced past
// is refused its first write with "database snapshot is stale". Twelve callers
// in this tree do read-then-write inside Tx, and for as long as that was the
// begin this package used, the refusal was either a lost write — a
// conversation entry, a memory row, a config revision, gone with a log line —
// or a retry that ran the whole body again. A write transaction now holds the
// lock from its BEGIN (begin.go) and queues for it in order (writequeue.go),
// so the second writer reads only after the first has committed and there is
// no race left to lose or to recover from.
//
// The counter is the sharpest shape of it: every goroutine reads the same row
// and writes it back. Two assertions, and the second is what the first cannot
// say on its own — the FINAL VALUE, because a lost update is silent by
// construction, and how many times the bodies RAN, because a store that
// recovered from the race by re-running them would pass the first and still
// be paying for the race on every contended write.
func TestAReadThenWriteCannotLoseARace(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "tx.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`CREATE TABLE crewlet_tx_probe (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO crewlet_tx_probe (id, n) VALUES (1, 0)`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		writers = 4
		each    = 12
	)
	var ran atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for range writers {
		wg.Go(func() {
			for range each {
				err := db.Tx(ctx, func(tx *sql.Tx) error {
					ran.Add(1)
					// READ then WRITE, in that order and in one
					// transaction: the shape that conflicts.
					var n int
					if err := tx.QueryRowContext(ctx,
						`SELECT n FROM crewlet_tx_probe WHERE id = 1`).Scan(&n); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx,
						`UPDATE crewlet_tx_probe SET n = ? WHERE id = 1`, n+1)
					return err
				})
				if err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		// NOT A LOGGED TOLERANCE ANY MORE. This used to count failures
		// and allow up to one per writer, because a bounded retry can
		// legitimately be exhausted by a race. There is no race: four
		// writers on one row queue for the lock and take it in turn, so
		// a failure here is a failure.
		t.Errorf("a read-then-write failed under contention: %v", err)
	}

	var got int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT n FROM crewlet_tx_probe WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != writers*each {
		t.Errorf("counter = %d, want %d: %d increments were lost silently, "+
			"which is what a read-then-write losing a race looks like",
			got, writers*each, writers*each-got)
	}

	// AND NOTHING RAN TWICE, which is the assertion the count above cannot
	// make: a store that absorbed the race by re-running the body would
	// reach the right total and still be replaying whatever the body did.
	if n := ran.Load(); n != int64(writers*each) {
		t.Errorf("the bodies ran %d times for %d transactions: %d of them were "+
			"aborted and replayed, which a transaction holding the lock from "+
			"its BEGIN cannot be", n, writers*each, n-int64(writers*each))
	}
}
