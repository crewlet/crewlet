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
// The engine owns its file exclusively (lock.go) and the driver has no
// multi-process access, so no second opener (sqlite3 .backup, Litestream,
// sqlite3_rsync) works, and copying the file under a running engine tears:
// data lives in the database AND its -wal. The engine copies its own database
// from inside the lock holder; of SQLite's three ways only VACUUM INTO is
// reachable (the backup API is stubbed, plain VACUUM is experimental). The
// copy is SELF-CONTAINED, with no -wal beside it, and a concurrent writer
// commits throughout with zero failures.

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
// renamed into place, so a path that exists after this returns is a copy that
// opened, passed an integrity check, and carries the schema this binary
// applied.
//
// The copy is ONE MOMENT, the one it began at ([DB.vacuumInto]): every commit
// returned before the call and nothing committed while it runs. A donor offers
// its snapshot at the copy's checkpoint, and a fleet backup pins the log just
// before the copy began, so an older copy would leave records pinned by
// nothing. Nothing pauses the engine.
func (d *DB) Backup(ctx context.Context, dest string) (BackupInfo, error) {
	if !d.isOpen() {
		return BackupInfo{}, fmt.Errorf("store: backup: %w", ErrNoEstate)
	}
	if dest == "" {
		return BackupInfo{}, errors.New("store: backup: no destination path")
	}
	// AN APOSTROPHE IN THE PATH IS REFUSED: a driver defect, not a quoting one
	// (the literal below is escaped correctly). Measured at v0.8.0-pre.7, the
	// driver fails differently by place:
	//
	//   a directory:  I/O error ("statfs shared WAL coordination path")
	//   a filename:   VACUUM INTO REPORTS SUCCESS AND WRITES NO FILE
	//
	// The second is why this refuses up front: a backup that can succeed
	// having produced nothing tells the operator they have a copy. The size
	// check after the vacuum catches it too; this names the fix.
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
// path is exercised before it is needed: it reads the copy through the same
// driver, session pragmas and schema accessor a restored node would.
//
// It uses openPrepared, not [OpenEstate]: an open MIGRATES, which would mutate
// the artifact being verified, and takes the exclusive lock, a claim on a file
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

// vacuumInto writes the copy to part as ONE POINT IN TIME holding every commit
// that returned before it began.
// The driver's VACUUM INTO holds no view: its read transaction is a DEFERRED
// `BEGIN`, which takes no snapshot (tursogo v0.8.3, op_vacuum_into).
// Measured under a writer, a copy MISSED earlier commits (a pooled connection's
// older view: 143 of 300) or was TORN (a checkpoint backfilling under it: 6 of
// 150); a fresh read cured only the first.
//
// Hold, then take, then copy ([DB.holdViewFor] does both in that order): the
// hold's read mark stops backfill past it, then the copying connection's view
// is taken, held or newer. The hold costs the log its checkpoints, so the copy
// is LISTED ([DB.Copies]) and its growth MEASURED.
func (d *DB) vacuumInto(ctx context.Context, part string) (held int64, err error) {
	// ONE CONNECTION DRAWN FROM THIS HANDLE ([DB.conn]), not through the pool,
	// so a close after [DB.Backup]'s guard (an adoption closing the replicated
	// estate) answers [ErrNoEstate], not database/sql's "database is closed".
	// It is drawn before the hold, so a closed handle opens nothing.
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
// ends the hold: a read transaction on a connection opened OUTSIDE this
// handle's pool, THEN conn's own view taken under it (order: [DB.vacuumInto]).
//
// Outside the pool because one copy would otherwise need two readers: with one
// configured it waits for ever on its own hold, and two copies at the
// replicated estate's floor of two each hold one and wait for the other
// (TestACopyTakesOneConnectionFromItsPool). It answers once its view is
// ESTABLISHED: a deferred BEGIN takes no snapshot, so it reads once
// ([takeView]). It OUTLIVES ctx's cancellation ([context.WithoutCancel]):
// database/sql rolls back when its context ends, and a hold ended mid-copy is
// the torn copy. The release ends it once the copy has returned.
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
// This package's connections keep temporary storage in memory ([openPool]),
// but VACUUM INTO builds the copy on one the driver opens itself, which no
// session pragma reaches: a temporary database in a fresh directory under
// TURSO_TMPDIR, then SQLITE_TMPDIR, then TMPDIR, and a sort spill under
// TMPDIR. A missing directory fails every copy, peers' snapshots included.
// MATCHED ON THE MESSAGE: the driver gives the failure no status, only the
// generic error carrying `I/O error (tempdir)`; the refusal then names the
// variables and directories ([temporaryDirectories]).
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
// with its transfer's digest over the same bytes (statelog's FetchArtefact); a
// second implementation is how one hashes the file and the other its path.
// statelog's TestAnArtefactArrivesByteForByte holds the two to this one.
//
// IT ENDS WHEN ITS CALLER GIVES UP, between reads. The files are whole
// estates, tens of gigabytes: with no context, a drain's stop waited for a
// snapshot loop to finish reading, and a backup whose request had gone away
// finished a checksum nobody would read.
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
