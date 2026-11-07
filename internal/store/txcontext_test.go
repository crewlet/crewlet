package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A TRANSACTION WHOSE CONTEXT ENDED REPORTS THE CONTEXT, not sql.ErrTxDone.
//
// database/sql rolls a transaction back when its context ends, so the commit
// that follows reports only that the transaction is done: a socket closing
// under a read was logged as a store fault ("store: commit: sql: transaction
// has already been committed or rolled back") that no caller could tell from
// one. The case waits for that rollback before returning, so the commit takes
// the ErrTxDone path every time.
//
// Mutation: return the commit's own error and both rows are red.
func TestACommitOnAnEndedContextReportsTheContext(t *testing.T) {
	t.Parallel()
	node := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	for name, run := range map[string]func(context.Context, func(*sql.Tx) error) error{
		"a read":  node.Read,
		"a write": node.Tx,
	} {
		ctx, cancel := context.WithCancel(t.Context())
		err := run(ctx, func(tx *sql.Tx) error {
			cancel()
			// UNTIL database/sql HAS ROLLED IT BACK, which it does on a
			// goroutine of its own once the context ends.
			for deadline := time.Now().Add(10 * time.Second); ; {
				var one int
				err := tx.QueryRowContext(context.Background(), `SELECT 1`).Scan(&one)
				if errors.Is(err, sql.ErrTxDone) {
					return nil
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s: the transaction was not rolled back after its "+
						"context ended (last: %v)", name, err)
				}
				time.Sleep(time.Millisecond)
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s on an ended context answered %v, want context.Canceled", name, err)
		}
	}
}
