package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A WRITE THAT NEVER GOT THE LOCK IS BUSY, AND A WRITE THAT FAILED IS NOT.
//
// Staged the way production reaches it: one transaction holds the file's write
// lock and does not let go, and a second asks for it on a store whose busy
// timeout is short. The second waits its line out, waits it out again on its
// one retry, and gives up — and that give-up is the one failure here a caller
// clears by trying again a moment later, so it must carry [store.ErrBusy] for
// a tool to answer it as a condition rather than as this node's fault. The
// driver's own words stay beneath it for the log.
//
// The control is a statement that fails with the lock HELD — a table that does
// not exist — which no wait changes and which must reach its caller unmarked.
//
// Mutation: return the give-up's error unwrapped from retryTransient, and the
// first half fails; wrap every exhausted cause, or every failure, and the
// control does.
func TestAWriteThatNeverGotTheLockIsBusy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := store.Open(ctx, filepath.Join(t.TempDir(), "busy.db"), store.Options{
		// SHORT, so the two waits the give-up costs are a fifth of a
		// second rather than the default ten.
		BusyTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	db := node.Replicated()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE busy_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}

	holding, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO busy_probe (id) VALUES (1)`); err != nil {
				return err
			}
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	waited := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO busy_probe (id) VALUES (2)`)
		return err
	})
	close(release)
	if err := <-held; err != nil {
		t.Fatalf("the transaction holding the lock failed: %v", err)
	}
	if !errors.Is(waited, store.ErrBusy) {
		t.Fatalf("a write that never got the lock answered %v, want store.ErrBusy: "+
			"unmarked, a caller above the store reads lock contention — which "+
			"clears the moment the holder commits — as this node's own fault", waited)
	}
	if waited.Error() == store.ErrBusy.Error() {
		t.Errorf("the give-up carries only %q; the driver's own words, which say "+
			"what the lock wait ended on, are not beneath it", waited)
	}

	// AND IT CLEARED BY WAITING, which is the claim the mark makes: the
	// holder has committed, so the same write lands now.
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO busy_probe (id) VALUES (2)`)
		return err
	}); err != nil {
		t.Fatalf("the write that was busy failed again once the lock was free: %v", err)
	}

	failed := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO no_such_table (id) VALUES (1)`)
		return err
	})
	if failed == nil {
		t.Fatal("a statement against a table that does not exist succeeded")
	}
	if errors.Is(failed, store.ErrBusy) {
		t.Errorf("a statement that failed with the lock held answered %v, marked "+
			"busy: no wait changes it, and a caller told to try again would try "+
			"for ever", failed)
	}
}

// A CANCELLED WAIT IS THE CALLER'S, NOT BUSY: a writer whose context ends
// while it is still in line for the lock answers the context's error, which
// says whose decision ended the wait.
//
// Mutation: wrap every error retryTransient returns in ErrBusy, and this fails.
func TestACancelledLockWaitIsNotBusy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := store.Open(ctx, filepath.Join(t.TempDir(), "cancelled.db"), store.Options{
		// LONG, so the cancellation and never the timeout is what ends
		// the wait.
		BusyTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	db := node.Replicated()

	holding, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- db.Tx(ctx, func(*sql.Tx) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	defer func() {
		close(release)
		<-held
	}()

	waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = db.Tx(waiting, func(*sql.Tx) error { return nil })
	if err == nil {
		t.Fatal("a write behind a held lock succeeded while the lock was held")
	}
	if errors.Is(err, store.ErrBusy) {
		t.Errorf("a wait its caller's own deadline ended answered %v, marked busy", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a wait its caller's own deadline ended answered %v, want the "+
			"context's error", err)
	}
}
