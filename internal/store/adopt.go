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

// CloseReplicated and ReopenReplicated bracket a join's install, keeping the
// handle every caller holds valid across it.
//
// # Why a bracket and not a reconstruction
//
// A join arrives at a node that is already assembled: the engine holds one
// *DB and hands it to the applier, the write authority, the read seam, the
// projector and the backup. Returning a NEW handle would mean re-threading
// every one of them, and the failure of missing one is silent — a subsystem
// still reading the file that was replaced, which opens, answers, and answers
// from the state this node adopted its way out of.
//
// So the outer handle is stable and its PEER is what changes. A caller that
// took [DB.Replicated] before the swap holds the old one, which is why this is
// only ever called at boot, before an applier, a projector or a seat exists.
//
// # Why the close is separate from the rename
//
// [AdoptFile] takes no handle and both files must already be closed when it
// runs: this process's claim is on the PATH rather than the inode, so an open
// handle would go on writing into a file that is no longer at that name. The
// join owns the rename between these two calls, and it is the join that knows
// whether it reached it.
func (d *DB) CloseReplicated() error {
	switch {
	case d == nil || d.sql == nil:
		return fmt.Errorf("store: no handle to close a replicated estate on")
	case d.estate != EstateNode:
		return fmt.Errorf("store: a %s handle has no replicated peer — the "+
			"bracket is the node handle's, because that is the one every "+
			"caller reaches the replicated estate through", d.estate)
	case d.replicated == nil:
		// ALREADY CLOSED IS NOT AN ERROR: a join that failed between
		// the close and the rename unwinds by reopening, and an unwind
		// that had to know how far it got would be a second state
		// machine beside the phase the adoption row already records.
		return nil
	}
	err := d.replicated.Close()
	d.replicated = nil
	if err != nil {
		return fmt.Errorf("store: close the replicated estate: %w", err)
	}
	return nil
}

// ReopenReplicated brings the peer back up at the same path, with the same
// options, running the migrator: an artefact from a peer on an OLDER build is
// one this node brings forward, and one from a newer build was refused before
// the transfer started.
//
// A failure here leaves the node with NO replicated estate rather than with
// the old one, and says so: after the rename the old database is gone, and a
// handle that quietly went on answering from a file the caller cannot name is
// the failure the whole sequence is against.
func (d *DB) ReopenReplicated(ctx context.Context) error {
	switch {
	case d == nil || d.sql == nil:
		return fmt.Errorf("store: no handle to reopen a replicated estate on")
	case d.estate != EstateNode:
		return fmt.Errorf("store: a %s handle has no replicated peer to reopen",
			d.estate)
	case d.replicated != nil:
		return nil
	}
	path := ReplicatedPath(d.path, d.opened.ReplicatedPath)
	replicated, err := openEstate(ctx, EstateReplicated, path, d.opened)
	if err != nil {
		return fmt.Errorf("store: reopen the replicated estate at %s — this "+
			"node has none open and cannot serve without one: %w", path, err)
	}
	// THE PROBE IS NOT REPEATED, for [Open]'s reason: it answers a
	// question about the driver compiled into this process, and a file
	// arriving from a peer did not change which driver that is.
	replicated.caps = d.caps
	d.replicated = replicated
	return nil
}
