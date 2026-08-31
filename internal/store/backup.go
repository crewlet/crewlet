package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Backing this file up ONLINE, and why it is VACUUM INTO.
//
// The engine owns its file exclusively for the life of the process (see
// lock.go), so an outside backup agent cannot open it — and the driver does
// not support multi-process access to a database file at all, which rules out
// every SQLite-ecosystem tool that works by being a second opener: sqlite3
// .backup, Litestream, sqlite3_rsync. Copying the file underneath a running
// engine is worse still: committed data lives in the database file AND its
// -wal, so a copy of either alone, or of both at different instants, is torn.
//
// The one sanctioned online copy is therefore one the ENGINE takes of its own
// database, from inside the process that holds the lock. Of the three ways
// SQLite offers, exactly one is reachable here: the backup API
// (sqlite3_backup_*) is stubbed in this driver, plain VACUUM is behind an
// experimental flag, and VACUUM INTO works.
//
// What it costs, measured against the pinned driver on a database under
// continuous writes: the copy runs to completion, a concurrent writer commits
// throughout with zero failures, and the result is a self-consistent
// point-in-time image of the database as of some instant during the copy.
// Crucially the copy is SELF-CONTAINED — it lands with no -wal beside it —
// so restoring a node's store is moving one file into place rather than a
// set that has to travel together.
//
// The copy is a snapshot of a MOMENT, not of the instant the call returned.
// Nothing here pauses the engine, so rows committed while the copy ran may or
// may not be in it. That is the correct trade for this store: it is the
// node's own memory and audit index, its cross-node truth lives in the
// coordination store, and a backup that stopped a company to take itself
// would not be taken.

// backupPartSuffix names the in-progress copy.
//
// The copy is written under this name and RENAMED on success, so a crash
// mid-copy leaves a file that is obviously incomplete rather than one sitting
// at the destination looking like a backup. Rename within one directory is
// atomic, which is what makes the destination's existence mean "verified".
const backupPartSuffix = ".part"

// BackupInfo describes a copy that was taken and verified.
type BackupInfo struct {
	// Path is the finished copy.
	Path string

	// Bytes is its size on disk. Reported because the first question an
	// operator asks of a backup is whether it is plausibly the whole
	// database, and the second is what the destination has to hold.
	Bytes int64

	// Migrations is the schema the copy carries, which is what a restore
	// of it would bring back. Recorded rather than assumed: a copy that
	// silently carried a different schema than the live database would
	// restore into a binary that migrates it forward again from the wrong
	// place.
	Migrations []string

	// TookFor is how long the copy ran. An operator sizing a backup
	// window needs the real number from their own database, not this
	// package's estimate of it.
	TookFor time.Duration
}

// ErrBackupExists reports a destination that is already occupied.
//
// A distinct error because overwriting is the one thing a backup command must
// never do by accident: the file in the way is, by construction, somebody's
// only copy of something.
var ErrBackupExists = errors.New("store: backup destination already exists")

// ErrBadBackupPath reports a destination this engine cannot write a copy to —
// the live database itself, or a path its VACUUM INTO mishandles.
//
// Separate from [ErrBackupExists] because they are different instructions to
// the caller ("move what is there" versus "name a different path"), and both
// are distinct from a failure of the node: a caller who named somewhere
// impossible should be sent back to their own command rather than to the
// engine's logs.
var ErrBadBackupPath = errors.New("store: unusable backup destination")

// Backup writes a verified point-in-time copy of this database to dest.
//
// dest must not exist. The copy is written beside it, verified, and only then
// renamed into place — so a path that exists after this returns is a copy that
// opened, passed an integrity check, and carries the schema this binary
// applied.
//
// It does not stop the engine; see the file doc for what the resulting
// snapshot is a snapshot OF.
func (d *DB) Backup(ctx context.Context, dest string) (BackupInfo, error) {
	if d == nil || d.sql == nil {
		return BackupInfo{}, errors.New("store: backup: no open database")
	}
	if dest == "" {
		return BackupInfo{}, errors.New("store: backup: no destination path")
	}
	// AN APOSTROPHE IN THE PATH IS REFUSED, and this is a driver defect
	// rather than a quoting one. The literal below is correctly escaped —
	// a mis-escaped path is a parse error, and this is not one — but the
	// driver mishandles the path internally after parsing it. Measured at
	// v0.8.0-pre.7, and the two failures are different:
	//
	//   a directory:  I/O error ("statfs shared WAL coordination path")
	//   a filename:   VACUUM INTO REPORTS SUCCESS AND WRITES NO FILE
	//
	// The second is why this is a refusal up front rather than an error
	// left to the driver. A backup command whose happy path can return
	// success having produced nothing is worse than one that does not run:
	// the operator is told they have a copy. The size check after the
	// vacuum catches it too, and this catches it with a sentence naming
	// the fix.
	if strings.Contains(dest, "'") {
		return BackupInfo{}, fmt.Errorf("%w: %s contains an "+
			"apostrophe, which the database engine mishandles in a VACUUM INTO path "+
			"(measured: it fails outright in a directory name, and silently writes "+
			"nothing in a file name) — choose a path without one", ErrBadBackupPath, dest)
	}
	// THE SOURCE ITSELF, named explicitly. The check below would catch it
	// anyway — the live database exists, so it reads as an occupied
	// destination — but "already exists" is the wrong sentence for it, and
	// the mistake is worth naming: this process HOLDS that path's lock, and
	// the claim is refcounted per path, so a copy that got as far as
	// opening its own destination would find the lock granted rather than
	// refused and run a second pool against the live database.
	if sameFile(dest, d.path) {
		return BackupInfo{}, fmt.Errorf("%w: %s is the live database this "+
			"node is running on — name a path outside it", ErrBadBackupPath, dest)
	}
	// Checked here as well as by the driver, which refuses an existing
	// output file with a parse error naming no remedy. An operator who
	// pointed two backups at one path needs to be told that, in those
	// words.
	if _, err := os.Stat(dest); err == nil {
		return BackupInfo{}, fmt.Errorf("%w: %s — name a path that does not exist yet, "+
			"or move what is there", ErrBackupExists, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupInfo{}, fmt.Errorf("store: backup: reach %s: %w", dest, err)
	}
	// 0700 on the directory, because of what the copy CONTAINS: the sealed
	// bootstrap secret rows, every config revision, and every seat's
	// memory. The file's own mode is not ours to set at creation — the
	// driver creates it, from the process umask — so the directory is the
	// guard that holds regardless, and the mode below is the belt to its
	// braces.
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return BackupInfo{}, fmt.Errorf("store: backup: create %s: %w", filepath.Dir(dest), err)
	}

	part := dest + backupPartSuffix
	// A part file from a previous crashed attempt is debris, not data:
	// only this function writes this name, and it only ever leaves one
	// behind by dying mid-copy. Left in place it would fail every
	// subsequent backup to the same destination.
	if err := removeDatabaseFiles(part); err != nil {
		return BackupInfo{}, err
	}

	started := now()
	// THE PATH IS INTERPOLATED, and it has to be: the driver rejects a bind
	// parameter here ("VACUUM INTO requires a string literal path",
	// measured), so the obvious `VACUUM INTO ?` does not run at all. That
	// makes quoting this package's job rather than the driver's, which is
	// why it goes through one helper with its own test rather than a
	// fmt.Sprintf at the call site.
	//
	// Also why this is an ExecContext rather than the [DB.Tx] every other
	// write in this package goes through: VACUUM cannot run inside a
	// transaction.
	if _, err := d.sql.ExecContext(ctx, "VACUUM INTO "+sqlStringLiteral(part)); err != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, fmt.Errorf("store: backup %s to %s: %w", d.path, dest, err)
	}
	took := now().Sub(started)

	// THE COPY EXISTS AND HAS BYTES, asserted before anything opens it.
	// A vacuum that reported success and wrote nothing is a measured
	// failure mode of this driver (see the apostrophe refusal above), and
	// without this check the next step would CREATE the missing file as an
	// empty database and then report the confusing news that it carries no
	// schema. This is the check that says what actually happened.
	// TWO FAILURES, reported separately. Folded into one branch, an
	// unreadable destination — a permission change, a vanished mount, a
	// full filesystem — was reported as "the driver wrote nothing", which
	// sends an operator to the database rather than to the disk, and the
	// errno that names the actual fix was dropped. It was also the one
	// unwrapped error on a path where everything else carries %w.
	written, err := os.Stat(part)
	if err != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, fmt.Errorf(
			"store: backup: cannot read the copy written to %s: %w", part, err)
	}
	if written.Size() == 0 {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, fmt.Errorf("store: backup: the engine reported a successful "+
			"copy of %s but %s holds no database, so nothing was backed up", d.path, part)
	}

	migrations, err := verifyBackup(ctx, part)
	if err != nil {
		// The unverified copy is REMOVED rather than kept for
		// inspection. It is a database that failed to open or failed an
		// integrity check, its bytes say nothing a re-run would not say
		// again, and the one thing it could do is be mistaken for a
		// backup by whoever finds it.
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, err
	}

	// The sidecars verification grew, BEFORE the rename: opening a database
	// in WAL mode creates a -wal beside it, and renaming only the database
	// would leave that orphan sitting in the backup directory under the
	// part name. The artifact this produces is ONE file.
	if sidecarErr := removeSidecars(part); sidecarErr != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, sidecarErr
	}
	// 0600 BEFORE the rename, so the artifact is never readable at its
	// final name: the driver creates the copy from this process's umask,
	// which is typically 0644, and a backup of this database is every
	// credential and every seat's memory in one file.
	if chmodErr := os.Chmod(part, 0o600); chmodErr != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, fmt.Errorf("store: backup: secure %s: %w", part, chmodErr)
	}
	if renameErr := os.Rename(part, dest); renameErr != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, fmt.Errorf("store: backup: place %s: %w", dest, renameErr)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("store: backup: measure %s: %w", dest, err)
	}
	log.InfoContext(ctx, "store_backed_up",
		"source", d.path, "path", dest, "bytes", info.Size(),
		"took", took.String(), "migrations", len(migrations))
	return BackupInfo{
		Path:       dest,
		Bytes:      info.Size(),
		Migrations: migrations,
		TookFor:    took,
	}, nil
}

// verifyBackup opens the copy, integrity-checks it, and reports the schema it
// carries.
//
// A COPY NOBODY OPENED IS NOT A BACKUP. This is the only place the restore
// path is ever exercised before it is needed: it reads the copy through the
// same driver, the same session pragmas and the same schema accessor a
// restored node would, so a copy that cannot be opened is a failed backup
// rather than a surprise on the worst day of the deployment's life.
//
// It goes through openPrepared rather than [Open] deliberately, for two
// reasons that both matter: Open MIGRATES, which would mutate the artifact
// being verified, and Open takes the exclusive lock, which is a claim on a
// file this process is about to rename.
func verifyBackup(ctx context.Context, path string) ([]string, error) {
	pool, err := openPrepared(ctx, path, Options{MaxOpenConns: 1})
	if err != nil {
		return nil, fmt.Errorf("store: backup verify: the copy at %s will not open: %w", path, err)
	}
	defer func() { _ = pool.Close() }()

	if checkErr := integrityCheck(ctx, pool); checkErr != nil {
		return nil, checkErr
	}

	rows, err := pool.QueryContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: backup verify: read schema_migrations in the copy: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var applied []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: backup verify: scan schema_migrations: %w", err)
		}
		applied = append(applied, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: backup verify: read schema_migrations in the copy: %w", err)
	}
	if len(applied) == 0 {
		return nil, fmt.Errorf("store: backup verify: the copy at %s records no schema at all, "+
			"so it is not a copy of a migrated database", path)
	}
	return applied, nil
}

// integrityCheck runs the driver's own structural check over a database.
//
// EVERY ROW, not the first: a healthy database answers with the single row
// "ok", and a damaged one answers with up to a hundred rows describing what
// is wrong. Reading only the first would report the first fault and hide the
// rest — and, worse, a check that returned no rows at all would look like a
// pass.
func integrityCheck(ctx context.Context, pool *sql.DB) error {
	rows, err := pool.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("store: integrity check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var faults []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("store: integrity check: %w", err)
		}
		if line != "ok" {
			faults = append(faults, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: integrity check: %w", err)
	}
	if len(faults) > 0 {
		return fmt.Errorf("store: integrity check failed: %v", faults)
	}
	return nil
}

// databaseSidecars are the files a database grows beside itself. Named once,
// because a list that is written twice is a list that stops matching.
var databaseSidecars = []string{"-wal", "-shm", "-tshm"}

// removeDatabaseFiles deletes a database path and every sidecar of it.
func removeDatabaseFiles(path string) error {
	if err := remove(path); err != nil {
		return err
	}
	return removeSidecars(path)
}

// removeSidecars deletes a database's sidecars, leaving the database.
//
// Verification opens the copy, and opening a database in WAL mode creates a
// -wal beside it — so without this the finished backup would ship with an
// empty sidecar that means nothing and invites an operator to copy or omit it
// on restore. The copy VACUUM INTO produces is self-contained; this keeps it
// that way.
func removeSidecars(path string) error {
	for _, suffix := range databaseSidecars {
		if err := remove(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

func remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: clear %s: %w", path, err)
	}
	return nil
}

// sqlStringLiteral renders s as a SQL string literal.
//
// One rule, and it is the whole rule for this dialect: a literal is single
// quotes around the text with every embedded single quote DOUBLED. There is
// no backslash escape to get wrong, and no other character is special inside
// one — so a path containing quotes, spaces or backslashes survives intact.
//
// It exists because [DB.Backup] cannot use a bind parameter (see there), and
// a hand-rolled Sprintf at that call site is how a path with an apostrophe in
// it becomes a syntax error at best.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sameFile reports whether two paths name one file.
//
// By IDENTITY where both exist — os.SameFile compares device and inode, so a
// symlink, a bind mount or a relative spelling of the live database is caught
// where a string comparison would wave it through. String equality is the
// fallback for a destination that does not exist yet, which is the ordinary
// case and cannot be the live database anyway.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
