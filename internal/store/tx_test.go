package store_test

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A READ-THEN-WRITE CANNOT LOSE A RACE, and its body runs once.
//
// The driver's default BEGIN is deferred and its conflict detection is per
// file: a transaction that reads a row another writer has since advanced past
// is refused its first write with "database snapshot is stale". Twelve callers
// in this tree do read-then-write inside Tx, and for years that refusal was
// either a lost write or a retry that ran the body again. A write transaction
// now holds the lock from its BEGIN (see writelock.go), so the second writer
// reads only after the first has committed, and there is no race left to lose
// or to retry.
//
// The counter is the sharpest shape of it: every goroutine reads the same row
// and writes it back. The assertions are on the FINAL VALUE, because a lost
// update is silent by construction, and on how many times the bodies RAN,
// because a store that recovered from the race by re-running them would pass
// the first assertion and still be paying for the race.
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
		t.Errorf("a read-then-write failed under contention: %v", err)
	}

	var got int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT n FROM crewlet_tx_probe WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != writers*each {
		t.Errorf("counter = %d, want %d: %d increments were lost", got,
			writers*each, writers*each-got)
	}
	if n := ran.Load(); n != writers*each {
		t.Errorf("the bodies ran %d times for %d transactions: a read-then-write "+
			"lost a race and was re-run, so a write transaction is not holding "+
			"the lock from its BEGIN", n, writers*each)
	}
}
