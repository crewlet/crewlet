package store_test

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A read-then-write that loses a race is retried, not lost.
//
// The driver's BeginTx ignores its options and always issues a plain BEGIN, so
// a transaction that reads a row another writer has since advanced past does
// not wait out a busy timeout — it fails IMMEDIATELY with "database snapshot
// is stale". Twelve callers in this tree do read-then-write inside Tx and
// exactly one of them carried a private retry loop, so for the other eleven
// that error surfaced as a lost write on a database with no writer but this
// process: a conversation entry, a memory row, a config revision, gone with a
// log line.
//
// The counter here is the sharpest shape of it — every goroutine reads the
// same row and writes it back — and the assertion is on the FINAL VALUE rather
// than on any error, because a lost update is silent by construction. Remove
// the retry from store.Tx and this goes red with a count short of the writes.
func TestTxRetriesAConflictedReadThenWrite(t *testing.T) {
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
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				err := db.Tx(ctx, func(tx *sql.Tx) error {
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
		`SELECT n FROM crewlet_tx_probe WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}

	// THE INVARIANT, and the one a lost update breaks: every increment either
	// landed or came back as an error. A bounded retry can still be
	// exhausted — that is what bounded means, and the caller is told — but
	// nothing may disappear between the two.
	if got+failed != writers*each {
		t.Errorf("counter = %d with %d reported failures, want them to sum to %d: "+
			"%d increments were lost silently", got, failed, writers*each,
			writers*each-got-failed)
	}

	// And the retry has to be doing its job, not merely accounting for its
	// absence. Measured over twenty runs of this exact contention — four
	// writers, one row, twelve increments each — the budget was never
	// exhausted; without the retry the first conflict fails immediately and
	// this count is a double-digit fraction of the writes.
	if failed > writers {
		t.Errorf("%d of %d transactions exhausted their retry budget: the retry "+
			"is not absorbing ordinary contention", failed, writers*each)
	}
}
