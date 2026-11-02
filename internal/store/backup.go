package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/words"

	turso "turso.tech/database/tursogo"
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
// continuous writes: the copy runs to completion and a concurrent writer
// commits throughout with zero failures. Crucially the copy is
// SELF-CONTAINED — it lands with no -wal beside it — so restoring a node's
// store is moving one file into place rather than a set that has to travel
// together.
//
// The copy is ONE MOMENT, the one it began at: it holds every commit that
// returned before [DB.Backup] was called, and nothing a writer commits while
// it runs. That is this package's doing and not the driver's: its VACUUM INTO
// holds no view of its own, and it copied either an older view or a torn one
// until [DB.vacuumInto] held one for it. Every caller leans on it. A donor
// offers its snapshot at the checkpoint the copy holds, having judged before
// the copy that nothing was deferred; and a fleet backup pins the log at the
// position its node stood at just before its copy began, so a copy older than
// that position would leave the records between the two pinned by nothing.
//
// Nothing here pauses the engine: a write made while the copy runs commits as
// usual and lands after the copy's moment. That is the correct trade for this
// store: it is the node's own memory and audit index, its cross-node truth
// lives in the coordination store, and a backup that stopped a company to
// take itself would not be taken.

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

	// SHA256 is the hex digest of the finished copy.
	//
	// Taken HERE rather than by whoever ships the file, because this is the
	// last moment the bytes are known to be the ones that passed the
	// integrity check: a digest computed later cannot tell a good copy that
	// was truncated in transit from a bad copy that was faithfully carried.
	SHA256 string

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
	if !d.isOpen() {
		return BackupInfo{}, fmt.Errorf("store: backup: %w", ErrNoEstate)
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
	held, err := d.vacuumInto(ctx, part)
	if err != nil {
		_ = removeDatabaseFiles(part)
		if tempDirRefused(err) {
			return BackupInfo{}, fmt.Errorf("store: backup %s to %s: couldn't make "+
				"a directory for the copy's temporary files. %s: %w",
				d.path, dest, temporaryDirectories(), err)
		}
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

	// The source's own applied set, read before the verify so the copy is
	// compared with what it was made from.
	source, err := d.appliedVersions(ctx)
	if err != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, err
	}
	migrations, err := verifyBackup(ctx, part, source)
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
	// THE DIGEST BEFORE THE RENAME, over the file the rename will place —
	// after the sidecars are gone and the mode is set, so what is hashed is
	// byte-for-byte what an operator will ship.
	digest, err := FileDigest(ctx, part)
	if err != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, err
	}
	if renameErr := os.Rename(part, dest); renameErr != nil {
		_ = removeDatabaseFiles(part)
		return BackupInfo{}, fmt.Errorf("store: backup: place %s: %w", dest, renameErr)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("store: backup: measure %s: %w", dest, err)
	}
	// HELD_LOG IS WHAT THE COPY HELD BACK on the live file's volume while it
	// ran (copies.go): beside `took`, the two numbers an operator sizes that
	// volume's room for the next copy against.
	log.InfoContext(ctx, "store_backed_up",
		"estate", string(d.estate),
		"source", d.path, "path", dest, "bytes", info.Size(),
		"took", took.String(), "held_log", held,
		"migrations", len(migrations), "sha256", digest)
	return BackupInfo{
		Path:       dest,
		SHA256:     digest,
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
// It goes through openPrepared rather than [OpenEstate] deliberately, for two
// reasons that both matter: an open MIGRATES, which would mutate the artifact
// being verified, and it takes the exclusive lock, which is a claim on a file
// this process is about to rename.
func verifyBackup(ctx context.Context, path string, want []string) ([]string, error) {
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
	// AGAINST THE SOURCE'S OWN SCHEMA, not against "more than none".
	//
	// The rule this replaces — a copy with no migrations is not a copy of a
	// migrated database — was a proxy for the real question and stopped
	// being true the day a node grew a second estate: the replicated
	// estate's sequence is empty until the state log's tables land, so its
	// backup would have been refused for faithfully reproducing an empty
	// sequence. Comparing against the source answers the actual question,
	// and is strictly stronger everywhere the old rule held: a copy that
	// carries FEWER migrations than the live database is one a restore
	// would migrate forward from the wrong place, whether that number is
	// zero or twelve.
	if !slices.Equal(applied, want) {
		return nil, fmt.Errorf("store: backup verify: the copy at %s records schema %s "+
			"but the database it was copied from has applied %s, so a restore of it "+
			"would migrate forward from the wrong place", path, words.And(applied), words.And(want))
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
		return fmt.Errorf("store: integrity check failed: %s", words.And(faults))
	}
	return nil
}

// databaseSidecars are the files a database grows beside itself. Named once,
// because a list that is written twice is a list that stops matching.
var databaseSidecars = []string{walSidecar, "-shm", "-tshm"}

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

// vacuumInto writes the copy to part as ONE POINT IN TIME that holds every
// commit which returned before it began.
//
// # The driver's VACUUM INTO holds no view of its own
//
// Its "implicit read transaction" is a DEFERRED `BEGIN`, which takes no
// snapshot, and every read of the source after it is a nested statement,
// which never begins one (tursogo v0.8.3, whose database engine is Turso
// v0.8.3: op_vacuum_into in core/vdbe/execute.rs, where Turso's own test finds
// the source pager holding no read lock after that BEGIN). So the copy is read
// at whatever view the connection's PREVIOUS read transaction left behind, and
// nothing holds that view while the copy runs. Two failures follow, both
// measured against the pinned driver on the replicated estate, with a writer
// committing on its pinned connection throughout:
//
//   - A copy MISSED commits that returned before it began. A pooled
//     connection last used for a read taken before a commit carried that
//     view into the copy: with four readers cycling the pool, 143 of 300
//     copies held a view older than a commit that had returned before they
//     began — each the view its connection's previous read had taken — while
//     0 of 300 657 reads on the same pool did. That is the restored-reanchor
//     case's "the copy's checkpoint is at 1, want the write's own 2", caught
//     under load with the node's runner unmoved and its live checkpoint at 2
//     across the copy; a donor's snapshot or a fleet backup taken just after a
//     write misses it the same way.
//   - A copy was TORN. With nothing holding the view, a checkpoint is free
//     to backfill newer frames into the database file, or to restart the log,
//     under it, and a page the view has no frame for is read as the file holds
//     it now: with a TRUNCATE checkpoint forced every 20 ms, 6 of 150 copies
//     held two tables written in ONE transaction at different values — a state
//     the database never held, which a donor would have offered as its rows
//     and its checkpoint. A read on the copying connection first, giving the
//     copy a fresh view, cured the first failure and not this one: 0 of 150
//     stale, 6 torn.
//
// # Hold, then take, then copy
//
// A read transaction is begun on a connection of its own and held until the
// copy has returned: its read mark is what stops a checkpoint backfilling past
// it or restarting the log, so a page the copy reads from the file is one that
// no frame of its view supersedes. Only then is the copying connection's view
// taken, so that view is the held one or newer and every frame it reads past
// the held mark stays in the log for the whole copy ([DB.holdViewFor] does
// both, in that order). Measured the same way: 0 of 150 copies stale, 0 torn.
// THE ORDER IS THE RULE: a view taken before the hold may be older than the
// held mark, and a checkpoint may then backfill the frames between the two
// under it — which is why one function takes both steps rather than its
// caller.
//
// THE HOLD IS OUTSIDE THE POOL ([DB.connector]), on a connection opened for it
// and closed after it. Drawn from the pool beside the copying connection, one
// copy would need two of the readers at once: on a node configured with one
// it would wait for ever on its own hold, and two copies at once on the
// replicated estate's floor of two would each hold one and wait for the other
// — for ever in earnest, since the hold begins under [context.WithoutCancel].
// TestACopyTakesOneConnectionFromItsPool stands both at their VACUUM INTO at
// once.
//
// IT COSTS THE LOG ITS CHECKPOINTS for the copy's run: nothing past the held
// mark is backfilled while it stands, so the write-ahead log grows by what the
// writers commit during the copy and is folded in by the first checkpoint
// after it. That is the price of any online copy that is one point in time;
// mainline SQLite's own VACUUM INTO pays it the same way. So the copy is
// LISTED while it holds the log ([DB.Copies]) and MEASURES how far the log grew
// under it, before the hold ends ([DB.CopyHoldsBack]), which it answers as
// held: copies.go says who reads each, and why.
//
// # Through one connection drawn from this handle
//
// The copy runs on ONE CONNECTION DRAWN FROM THIS HANDLE ([DB.conn]) rather
// than through the pool, so a close that lands after [DB.Backup]'s guard — an
// adoption closing the replicated estate a snapshot is being taken of —
// answers [ErrNoEstate], the state every caller branches on, rather than
// database/sql's "database is closed", which none does. It is drawn before
// the hold, so a handle already closed opens nothing.
func (d *DB) vacuumInto(ctx context.Context, part string) (held int64, err error) {
	conn, err := d.conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	release, err := d.holdViewFor(ctx, conn)
	if err != nil {
		return 0, err
	}
	defer release()
	copying := d.copies.begin(d.path, part)
	_, err = conn.ExecContext(ctx, "VACUUM INTO "+sqlStringLiteral(part))
	// MEASURED HERE, before the deferred release ends the hold and lets a
	// checkpoint fold the log back in ([copies.end]).
	return d.copies.end(d.path, copying, err == nil), err
}

// holdViewFor holds a view for a copy conn is about to take, and returns what
// ends the hold: a read transaction begun on a connection opened for it,
// outside this handle's pool, and THEN conn's own view taken under it — see
// [DB.vacuumInto] for what each step is for, why in this order, and why the
// hold is outside the pool.
//
// The hold answers once its view is ESTABLISHED rather than merely begun: a
// deferred BEGIN takes no snapshot, so the transaction reads once
// ([takeView]), and its read mark stands from that read.
//
// THE TRANSACTION OUTLIVES ctx's CANCELLATION, for the reason a teardown takes
// [context.WithoutCancel]: database/sql rolls a transaction back the moment
// its context ends, and a hold ended while the copy it guards is still reading
// leaves that copy reading under nothing, which is the torn copy the hold
// exists to prevent. The release ends it, once the copy has returned.
func (d *DB) holdViewFor(ctx context.Context, conn *sql.Conn) (release func(), err error) {
	side := sql.OpenDB(d.connector)
	side.SetMaxOpenConns(1)
	tx, err := side.BeginTx(context.WithoutCancel(ctx), readTx)
	if err != nil {
		_ = side.Close()
		return nil, fmt.Errorf("store: hold the copy's view of %s: %w", d.path, err)
	}
	release = func() {
		_ = tx.Rollback()
		_ = side.Close()
	}
	if holdErr := takeView(ctx, tx); holdErr != nil {
		release()
		return nil, fmt.Errorf("store: hold the copy's view of %s: %w", d.path, holdErr)
	}
	if takeErr := takeView(ctx, conn); takeErr != nil {
		release()
		return nil, fmt.Errorf("store: take the copy's view of %s: %w", d.path, takeErr)
	}
	return release, nil
}

// viewQuery is the read that makes a connection's view the newest there is: a
// statement over a TABLE, so the driver begins a read transaction for it — of
// sqlite_schema, which every database has, so any file a handle opens can take
// one.
const viewQuery = `SELECT COUNT(*) FROM sqlite_schema`

// takeView makes q's view the newest there is, by reading through it.
func takeView(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) error {
	var n int64
	return q.QueryRowContext(ctx, viewQuery).Scan(&n)
}

// tempDirRefused reports a VACUUM INTO that failed because the driver could
// not make a directory for its temporary files.
//
// THE ONE STATEMENT HERE THAT STILL NEEDS ONE, and two things run it: a
// backup, and the state-log snapshot every data node takes for its peers.
// Every connection this package opens keeps its temporary storage in memory
// (see [openPool]), but VACUUM INTO builds the copy on a connection the driver
// opens for itself, which no session pragma reaches: it keeps a temporary
// database in a fresh directory under TURSO_TMPDIR, then SQLITE_TMPDIR, then
// TMPDIR (/tmp unset), and sorts past its buffer into another under TMPDIR,
// removing both when the copy ends. Measured at the pinned driver, a temporary
// directory that does not exist fails every copy, a one-table copy included.
//
// MATCHED ON THE MESSAGE, because the driver gives an I/O failure no status of
// its own: it arrives as the generic error carrying `I/O error (tempdir)`,
// the label upstream gives both of those directories. Matched, the refusal
// names the variables to change and the directories they hold
// ([temporaryDirectories]), where the driver's own words name neither.
func tempDirRefused(err error) bool {
	return errors.Is(err, turso.ErrTursoGeneric) && strings.Contains(err.Error(), "(tempdir)")
}

// temporaryDirectories says where the driver puts a copy's temporary files,
// resolved the way the driver resolves them: the temporary database under the
// first of TURSO_TMPDIR and SQLITE_TMPDIR that is set, else TMPDIR, and a
// large sort under TMPDIR whatever the others say.
//
// RESOLVED, NEVER TMPDIR ALONE: the driver labels both directories
// "(tempdir)", so the error cannot say which one refused, and naming TMPDIR's
// directory where TURSO_TMPDIR's was the missing one pointed an operator at a
// directory that was fine. So both are named when they differ.
func temporaryDirectories() string {
	spill := os.TempDir()
	for _, name := range []string{"TURSO_TMPDIR", "SQLITE_TMPDIR"} {
		dir, set := os.LookupEnv(name)
		if !set {
			continue
		}
		if dir == spill {
			return fmt.Sprintf("They go under %s and TMPDIR (%s). Make sure "+
				"the engine's user can write there", name, dir)
		}
		return fmt.Sprintf("The temporary database goes under %s (%s) and a "+
			"large sort under TMPDIR (%s). Make sure the engine's user can "+
			"write to both", name, dir, spill)
	}
	return fmt.Sprintf("They go under TMPDIR (%s). Make sure the engine's "+
		"user can write there", spill)
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

// FileDigest is the SHA-256 of a file, streamed rather than read whole.
//
// EXPORTED because a snapshot's manifest carries one and a joiner compares it
// with the digest its transfer took of the bytes as it wrote them (statelog's
// FetchArtefact), and the two must be the same digest over the same bytes — a
// second implementation is how one comes to hash the file and the other its
// path, which is why statelog's TestAnArtefactArrivesByteForByte holds the
// transfer's digest to this one.
//
// IT ENDS WHEN ITS CALLER GIVES UP, between one read and the next. The files
// it hashes are a whole estate — a backup's copy, a donor's snapshot — and a
// read of tens of gigabytes takes as long as the disk takes: with no context,
// a snapshot loop asked to stop by a node's drain went on reading until the
// end of the file, and the stop waited for it, and a backup whose request had
// gone away finished a checksum nobody would read.
func FileDigest(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("store: backup: read %s to checksum it: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, readUntilDone{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("store: backup: checksum %s: %w", path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// readUntilDone is r, refusing every read once ctx has ended. A wrapper rather
// than the file itself also keeps [io.Copy] on its own buffered loop — an
// [os.File] copies through its own WriteTo, which a context cannot reach.
type readUntilDone struct {
	ctx context.Context
	r   io.Reader
}

func (u readUntilDone) Read(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	return u.r.Read(p)
}
