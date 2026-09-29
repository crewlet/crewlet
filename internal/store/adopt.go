package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// AdoptFile replaces the database at live with the one at prepared, in the ONE
// order that leaves no torn state behind.
//
// A node that has fallen below the log's trim floor cannot replay its way
// back: the records it is missing are gone. What it does instead is take a
// peer's snapshot, verify it, and adopt it wholesale — this call is the last
// step of that, and it is the only place in the engine that replaces a live
// database file.
//
// # The order, and what each step is for
//
// A SQLite-family database in WAL mode is not one file: it is the database
// plus a -wal holding committed pages the database itself does not have yet,
// plus a -shm indexing that -wal. Renaming the database alone leaves the OLD
// -wal beside the NEW database — a file whose pages belong to a database that
// is gone, which the next open will happily apply. That is
// internal/store/backup.go's torn-copy hazard from the other direction, and
// backup.go:222-226 removes a prepared artefact's sidecars before ITS rename
// for exactly this reason.
//
//  1. Checkpoint and close the PREPARED file, so every page it holds is in
//     the file itself.
//  2. Remove the prepared file's sidecars, so nothing follows it across.
//  3. Checkpoint and close the LIVE file, so no page of the database being
//     replaced is left in a -wal that outlives it.
//  4. Remove the live file's sidecars.
//  5. Rename prepared over live — one atomic operation on the same
//     filesystem, which is what makes an interrupted adoption leave either
//     the old database or the new one and never a mixture.
//
// The checkpoints are EXPLICIT rather than left to a clean close, because the
// case that matters is the one where the close was not clean: storetest's
// fault injectors arm exactly that, and a truncating checkpoint is what makes
// the file self-contained whether or not the handle closed tidily.
//
// # What it does not do
//
// It does not open, verify or migrate either file, and it takes no handle:
// both databases must already be CLOSED by their owners. A caller holding a
// live *DB has to Close it first — the process's own advisory lock is on the
// path, not the inode, so an open handle would be writing into a file that is
// no longer at that name.
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

// adoptSteps is the order, AS A VALUE.
//
// The order IS the crash matrix — every one of the five steps exists because
// the state a crash after it leaves is one the next open can make sense of,
// and no other arrangement has that property. A straight line of five calls
// says so only in a comment; a table is something a test can walk, cut at each
// step, and check the invariant against. See TestTheAdoptsStepsRunInTheOrder
// ItsCrashMatrixAssumes and TestAnInterruptedAdoptionLeavesOneDatabaseOrThe
// Other.
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
// # Why a reader needs this at all
//
// Opening a SQLite database creates a -wal and a -shm beside it, even for a
// read, and this package's own advisory lock adds a third. That is invisible
// while a process owns a live database and a problem the moment somebody READS
// A COPY: a backup artefact is a set of files whose meaning depends on being
// that set, so a sidecar left behind is debris carrying the reader's own umask
// rather than the directory's deliberate 0700, and a restore script looking
// for named files finds one it does not know.
//
// # Why the name says COPY
//
// The lock sidecar is deliberately never removed from a live database — see
// [fileLock.release]: unlinking it races a peer that has already opened it and
// is about to lock, and two processes would then believe they hold it. A copy
// has no peer: nothing else will ever open it in place, because a restore
// moves it first. So the constraint is in the name rather than in a comment
// somebody reads afterwards, and the file must already be CLOSED by whoever
// opened it.
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
	pool, err := openPrepared(ctx, path, Options{MaxOpenConns: 1}, nil)
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
