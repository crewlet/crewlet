package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// AdoptFile replaces the database at live with the one at prepared, in the ONE
// order ([adoptSteps]) that leaves no torn state behind. It is the only place
// that replaces a live database file: the last step of adopting a peer's
// verified snapshot, for a node below the log's trim floor.
//
// A WAL-mode database is the file plus a -wal and a -shm. Renaming the
// database alone leaves the OLD -wal beside the NEW database, whose pages the
// next open would apply (backup.go removes a prepared artefact's sidecars for
// the same reason). It verifies and migrates nothing, and takes no handle:
// both databases must already be CLOSED, since the advisory lock is on the
// path, not the inode.
func AdoptFile(ctx context.Context, live, prepared string) error {
	for _, step := range adoptSteps(live, prepared) {
		if err := step.run(ctx); err != nil {
			return err
		}
	}
	return nil
}

// adoptStep is one step of the install, with the name the doc above gives it.
type adoptStep struct {
	name string
	run  func(ctx context.Context) error
}

// adoptSteps is the order, AS A VALUE: checkpoint and close the prepared file,
// remove its sidecars, checkpoint and close the live file (so no replaced page
// outlives it in a -wal), remove its sidecars, rename prepared over live.
//
// The rename is atomic on one filesystem, so an interrupted adoption leaves the
// old database or the new one, never a mixture. The checkpoints are EXPLICIT,
// not left to a clean close, because an unclean close is the case that matters
// (storetest's fault injectors arm it). The order IS the crash matrix, and a
// table is something a test can walk, cut at each step. See
// TestTheAdoptsStepsRunInTheOrderItsCrashMatrixAssumes and
// TestAnInterruptedAdoptionLeavesOneDatabaseOrTheOther.
func adoptSteps(live, prepared string) []adoptStep {
	return []adoptStep{
		{"checkpoint the prepared file", func(ctx context.Context) error {
			if err := checkpointAndClose(ctx, prepared); err != nil {
				return fmt.Errorf("store: adopt: prepare %s: %w", prepared, err)
			}
			return nil
		}},
		{"remove the prepared file's sidecars", func(context.Context) error {
			if err := removeSidecars(prepared); err != nil {
				return fmt.Errorf("store: adopt: %w", err)
			}
			return nil
		}},
		{"checkpoint the live file", func(ctx context.Context) error {
			// The live file may not exist — a node adopting before it
			// ever opened a store of its own — and that is an ordinary
			// adoption rather than an error, so this step and the next
			// tolerate its absence.
			if _, err := os.Stat(live); err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("store: adopt: stat %s: %w", live, err)
			}
			if err := checkpointAndClose(ctx, live); err != nil {
				return fmt.Errorf("store: adopt: quiesce %s: %w", live, err)
			}
			return nil
		}},
		{"remove the live file's sidecars", func(context.Context) error {
			if err := removeSidecars(live); err != nil {
				return fmt.Errorf("store: adopt: %w", err)
			}
			return nil
		}},
		{"rename prepared over live", func(context.Context) error {
			if err := os.Rename(prepared, live); err != nil {
				return fmt.Errorf("store: adopt: rename %s over %s: %w",
					prepared, live, err)
			}
			return nil
		}},
	}
}

// QuiesceCopy makes a COPY of a database self-contained again: it folds the
// -wal back in and removes every sidecar, including the lock.
//
// Opening a database, even to read, creates a -wal and -shm beside it, and
// this package's advisory lock adds a third. Left beside a backup artefact they
// are debris with the reader's umask rather than the directory's 0700, and a
// restore script finds files it does not know.
//
// The name says COPY because the lock sidecar is never removed from a live
// database (internal/flock's package doc: unlinking races a peer about to
// lock). A copy has no peer, since a restore moves it first. The file must
// already be CLOSED by whoever opened it.
func QuiesceCopy(ctx context.Context, path string) error {
	if err := checkpointAndClose(ctx, path); err != nil {
		return fmt.Errorf("store: quiesce %s: %w", path, err)
	}
	if err := removeSidecars(path); err != nil {
		return err
	}
	return remove(path + lockSuffix)
}

// RemoveCopy deletes a COPY of a database: the file, its sidecars and its lock.
//
// The lock goes with it for the reason [QuiesceCopy] gives: a copy has no
// peer that could be racing for the lock, because nothing else ever opens it
// in place. It is what a caller writing a copy under a part name clears a
// crashed attempt with, and what it discards a failed one with — a stale -wal
// beside a fresh copy is applied to it on the next open, so the sidecars have
// to go with the file and the list of them is this package's.
func RemoveCopy(path string) error {
	if err := removeDatabaseFiles(path); err != nil {
		return err
	}
	return remove(path + lockSuffix)
}

// checkpointAndClose opens path, folds its -wal into it, and closes.
//
// TRUNCATE rather than PASSIVE: passive leaves the -wal file in place with
// its pages already applied, and the whole point here is that the database is
// self-contained the instant this returns.
func checkpointAndClose(ctx context.Context, path string) error {
	pool, err := openPrepared(ctx, path, Options{MaxOpenConns: 1})
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	// The three columns a checkpoint answers with are read and discarded:
	// a busy checkpoint reports how much it could not fold, and this
	// process is the only opener, so anything other than "everything" is a
	// bug in the layer below rather than a condition to handle.
	var busy, logFrames, checkpointed sql.NullInt64
	if err := pool.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("store: checkpoint %s: %w", path, err)
	}
	return nil
}
