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
	if err := checkpointAndClose(ctx, prepared); err != nil {
		return fmt.Errorf("store: adopt: prepare %s: %w", prepared, err)
	}
	if err := removeSidecars(prepared); err != nil {
		return fmt.Errorf("store: adopt: %w", err)
	}
	// The live file may not exist — a node adopting before it ever opened
	// a store of its own — and that is an ordinary adoption rather than an
	// error, so both steps below tolerate its absence.
	if _, err := os.Stat(live); err == nil {
		if err := checkpointAndClose(ctx, live); err != nil {
			return fmt.Errorf("store: adopt: quiesce %s: %w", live, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: adopt: stat %s: %w", live, err)
	}
	if err := removeSidecars(live); err != nil {
		return fmt.Errorf("store: adopt: %w", err)
	}
	if err := os.Rename(prepared, live); err != nil {
		return fmt.Errorf("store: adopt: rename %s over %s: %w", prepared, live, err)
	}
	return nil
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
