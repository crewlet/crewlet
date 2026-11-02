package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A CHECKSUM ENDS WHEN ITS CALLER GIVES UP.
//
// The files hashed are whole estates — a backup's copy, a donor's snapshot —
// and reading one takes as long as the disk does. With no context, a snapshot
// loop told to stop by a node's drain read to the end of the file first and
// the stop waited for it, and a backup whose request had gone away finished a
// checksum nobody would read. So the digest is the file's while its caller
// waits, and the caller's own ending once it does not.
func TestAChecksumEndsWhenItsCallerGivesUp(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("a donor's snapshot "), 1<<16)
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	want := sha256.Sum256(content)
	got, err := store.FileDigest(t.Context(), path)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("FileDigest = (%s, %v), want the file's own %x", got, err, want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := store.FileDigest(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("a checksum whose caller had given up answered (%s, %v), want "+
			"the cancellation — it read the whole file for nobody", got, err)
	}
}

// A path with a space in it is ordinary and must work: the destination
// reaches the driver as an interpolated SQL literal (VACUUM INTO refuses a
// bind parameter), so this is the case that proves the quoting is real rather
// than accidentally unnecessary.
func TestABackupPathWithASpaceSurvivesQuoting(t *testing.T) {
	t.Parallel()
	db := open(t)

	dest := filepath.Join(t.TempDir(), "nightly backups", "the company.db")
	if _, err := db.Backup(t.Context(), dest); err != nil {
		t.Fatalf("backup to a path with a space: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the backup did not land at the path it was given: %v", err)
	}
}

// An apostrophe is refused UP FRONT, and the refusal is not fussiness about
// quoting — the literal is escaped correctly, and a mis-escaped one would be
// a parse error rather than what actually happens. It is a measured driver
// defect with a silent-success arm, which is the one failure mode a backup
// command must never have. See DB.Backup.
func TestABackupPathWithAnApostropheIsRefusedRatherThanSilentlyLost(t *testing.T) {
	t.Parallel()
	db := open(t)

	dest := filepath.Join(t.TempDir(), "it's a copy.db")
	_, err := db.Backup(t.Context(), dest)
	if err == nil {
		t.Fatal("a path the engine cannot honour was accepted")
	}
	if !strings.Contains(err.Error(), "apostrophe") {
		t.Fatalf("the refusal does not name the problem: %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a refused backup left a file at the destination")
	}
}

// The tripwire for the defect above. It records what the driver DOES today,
// so the day a pin bump fixes it this test fails and the refusal in
// DB.Backup can go — rather than the workaround outliving its reason by
// years, which is how a band-aid becomes permanent.
func TestTheEngineStillMishandlesAnApostropheInAVacuumPath(t *testing.T) {
	t.Parallel()
	db := open(t)

	dest := filepath.Join(t.TempDir(), "quoted'path.db")
	// Escaped exactly as DB.Backup escapes it: doubling the quote is the
	// whole rule for a SQL string literal in this dialect.
	literal := "'" + strings.ReplaceAll(dest, "'", "''") + "'"
	_, execErr := db.SQL().ExecContext(t.Context(), "VACUUM INTO "+literal)
	_, statErr := os.Stat(dest)

	switch {
	case execErr == nil && statErr == nil:
		t.Fatalf("the engine now honours an apostrophe in a VACUUM INTO path. "+
			"Drop the refusal in DB.Backup and this test: %s was written", dest)
	case execErr != nil:
		t.Logf("still refused outright, as measured: %v", execErr)
	default:
		t.Logf("still the silent arm, as measured: the engine reported success "+
			"and wrote no file at %s", dest)
	}
}

// Backing up ONTO the live database must be refused by name. It would
// otherwise read as an ordinary occupied destination, and the danger is not
// the occupancy: this process already holds that path's lock, the claim is
// refcounted per path, and a copy that got as far as opening its own
// destination would find the lock granted rather than refused.
func TestABackupOntoTheLiveDatabaseIsRefused(t *testing.T) {
	t.Parallel()
	db := open(t)

	_, err := db.Backup(t.Context(), db.Path())
	if err == nil {
		t.Fatal("a backup onto the live database was accepted")
	}
	if errors.Is(err, store.ErrBackupExists) {
		t.Fatalf("refused as a merely occupied path, which hides what went wrong: %v", err)
	}
	// Relative spellings of the same file are the same file. A string
	// comparison would wave this one through.
	rel, relErr := filepath.Rel(t.TempDir(), db.Path())
	if relErr == nil {
		if _, err := db.Backup(t.Context(), filepath.Join(t.TempDir(), rel)); err == nil {
			t.Fatal("a relative spelling of the live database was accepted")
		}
	}
}

// The artifact carries every credential the secret store bootstrapped, every
// config revision and every seat's memory. It must not land world-readable
// because the process umask says so.
func TestABackupIsNotReadableByTheWholeHost(t *testing.T) {
	t.Parallel()
	db := open(t)

	dir := filepath.Join(t.TempDir(), "vault")
	dest := filepath.Join(dir, "copy.db")
	if _, err := db.Backup(t.Context(), dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	file, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat the copy: %v", err)
	}
	if mode := file.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the backup is mode %04o; anything outside the owner can read it", mode)
	}
	parent, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the directory: %v", err)
	}
	if mode := parent.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the backup directory is mode %04o", mode)
	}
}

// A COPY HOLDS EVERY COMMIT THAT RETURNED BEFORE IT BEGAN, whichever
// connection it draws.
//
// The driver's VACUUM INTO reads at the view its connection's PREVIOUS read
// left behind (see DB.vacuumInto), so this stands the pool where a busy node
// stands it: every connection a copy can draw last read before the commit,
// which then returns on the pinned writer's own connection — an applier's
// shape — before the copy is asked for. A copy that does not take its own view
// holds the database as it stood before that commit, every time; it is the
// restored-reanchor case's failure ("the copy's checkpoint is at 1, want the
// write's own 2") made deterministic.
func TestACopyHoldsEveryCommitThatReturnedBeforeItBegan(t *testing.T) {
	t.Parallel()
	p := openCopyProbe(t)
	p.commit(t, 1)
	p.staleEveryReader(t)
	p.commit(t, 2)

	dest := filepath.Join(t.TempDir(), "copy.db")
	if _, err := p.db.Backup(t.Context(), dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if a, b := readCopyProbe(t, dest); a != 2 || b != 2 {
		t.Fatalf("the copy holds %d and %d, want the 2 whose commit returned before "+
			"the copy began — it copied the view its connection's last read took", a, b)
	}
}

// THE TRIPWIRE for the driver behaviour DB.vacuumInto exists for: a bare
// VACUUM INTO on a connection whose last read came before a commit still
// copies the database as it stood at that read. The day a pin bump makes it
// take a view of its own, this fails and the hold in DB.vacuumInto is
// revisited — though not simply removed: a copy is one moment only while a
// read mark guards its view against checkpoints, so the hold goes only once
// the driver's VACUUM INTO holds a read transaction for its whole run.
func TestTheDriversVacuumIntoStillCopiesItsConnectionsLastView(t *testing.T) {
	t.Parallel()
	p := openCopyProbe(t)
	ctx := t.Context()
	p.commit(t, 1)
	p.staleEveryReader(t)
	p.commit(t, 2)

	dest := filepath.Join(t.TempDir(), "bare.db")
	conn, err := p.db.SQL().Conn(ctx)
	if err != nil {
		t.Fatalf("draw a connection: %v", err)
	}
	literal := "'" + strings.ReplaceAll(dest, "'", "''") + "'"
	_, err = conn.ExecContext(ctx, "VACUUM INTO "+literal)
	_ = conn.Close()
	if err != nil {
		t.Fatalf("a bare VACUUM INTO: %v", err)
	}
	if a, b := readCopyProbe(t, dest); a != 1 || b != 1 {
		t.Fatalf("a bare VACUUM INTO on a connection that last read 1 copied %d and %d: "+
			"the driver now takes a view of its own — revisit the hold in "+
			"DB.vacuumInto, which may go once the driver also holds a read lock "+
			"for the copy's whole run", a, b)
	}
}

// EVERY COPY TAKEN UNDER WRITES AND CHECKPOINTS IS ONE MOMENT, at or after
// every commit that returned before it began.
//
// The writer commits the two probe tables to the same value in ONE
// transaction, without pause, and checkpoints are forced beside it as fast as
// they complete — each one free, while nothing holds a read mark, to backfill
// newer frames into the file under a copy or to restart the log. A copy whose
// view nothing holds reads a page from the file as the file is NOW, so it can
// hold the two tables at different values, a state the database never held;
// the table between them makes the copy read them far apart.
func TestACopyTakenUnderWritesAndCheckpointsIsOneMoment(t *testing.T) {
	t.Parallel()
	p := openCopyProbe(t)
	ctx := t.Context()
	p.commit(t, 0)

	var returned atomic.Int64
	stop := make(chan struct{})
	var bg sync.WaitGroup
	// STOPPED ON EVERY WAY OUT, a failed read of a copy included: a writer
	// left running past the case would commit into a store its cleanup is
	// closing and report it on a test that has ended.
	quiesce := sync.OnceFunc(func() { close(stop); bg.Wait() })
	defer quiesce()
	bg.Go(func() {
		for n := int64(1); ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := p.tryCommit(ctx, n); err != nil {
				t.Errorf("commit %d: %v", n, err)
				return
			}
			returned.Store(n)
		}
	})
	var checkpoints atomic.Int64
	bg.Go(func() {
		for {
			// A MILLISECOND APART: as often as one can complete, with
			// the pool's other connection free between attempts for the
			// copy to draw.
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if p.checkpoint(ctx) {
				checkpoints.Add(1)
			}
		}
	})

	dir := t.TempDir()
	for i := range copyUnderWrites {
		before := returned.Load()
		dest := filepath.Join(dir, fmt.Sprintf("copy-%d.db", i))
		if _, err := p.db.Backup(ctx, dest); err != nil {
			t.Errorf("copy %d: %v", i, err)
			break
		}
		a, b := readCopyProbe(t, dest)
		if a != b {
			t.Errorf("copy %d holds the two tables one transaction writes at %d and %d: "+
				"it is not one moment", i, a, b)
		}
		if min(a, b) < before {
			t.Errorf("copy %d holds %d, older than the %d whose commit returned before "+
				"it began", i, min(a, b), before)
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-tshm"} {
			_ = os.Remove(dest + suffix)
		}
	}
	quiesce()
	// THE CASE HAS TO HAVE BEEN ONE: copies taken while nothing committed,
	// or while no checkpoint completed, would pass whatever a copy holds.
	if n := returned.Load(); n < copyUnderWrites {
		t.Errorf("the writer committed %d times across %d copies, which tests "+
			"nothing about a copy under writes", n, copyUnderWrites)
	}
	if checkpoints.Load() == 0 {
		t.Error("no checkpoint completed beside the copies, so nothing moved the " +
			"file under any of them")
	}
}

// A COPY TAKES ONE CONNECTION FROM ITS POOL, so it finishes on the smallest
// pool either file can be configured with, and two at once on the smallest the
// replicated estate keeps.
//
// The read transaction that keeps a copy one moment is a second connection,
// opened OUTSIDE the pool (DB.vacuumInto). Drawn from the pool instead — the
// obvious simplification — one copy needs two of its readers at once: a node
// file opened with `store.max_open_conns: 1` keeps one, so its copy would wait
// for ever on its own hold, and two copies at once on the replicated estate's
// floor of two would each draw one and wait for the other. The wait is for
// ever in earnest, since the hold begins under context.WithoutCancel, so no
// deadline ends it and a fleet backup stuck in it keeps its running flag and
// its trim hold for the life of the process. Every other case copies through a
// pool of two or more readers, one copy at a time, and passes either way.
//
// So all three copies are stood at the same point before any is let through:
// each has drawn its connection, holds its view and waits at its VACUUM INTO.
// A copy short of a connection never reaches that point, and the case says so
// within a bound rather than hanging the binary.
func TestACopyTakesOneConnectionFromItsPool(t *testing.T) {
	t.Parallel()
	hold := storetest.NewCopyHold()
	node, replicated := openReplicated(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{MaxOpenConns: 1, WrapDriver: hold.Wrap}, 1)
	ctx := t.Context()
	// THE REPLICATED ESTATE AT ITS FLOOR: two readers beside the identity
	// reserve the handle holds and the writer its log pins, pinned here as
	// the applier pins it — left unpinned, its connection is one more a copy
	// could draw, and two copies would never be short of one.
	w, err := replicated.Writer(ctx)
	if err != nil {
		t.Fatalf("pin the applier's writer: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	type copied struct {
		name string
		err  error
	}
	hold.Arm()
	// RELEASED ON EVERY WAY OUT, so a copy the case gave up on fails at its
	// VACUUM INTO rather than waiting on a gate nobody will open.
	defer hold.Release(errors.New("the case ended before it let the copies through"))
	dir := t.TempDir()
	copies := []*store.DB{node, replicated, replicated}
	done := make(chan copied, len(copies))
	for i, db := range copies {
		name := fmt.Sprintf("copy %d, of the %s estate", i, db.Estate())
		go func() {
			_, err := db.Backup(ctx, filepath.Join(dir, fmt.Sprintf("copy-%d.db", i)))
			done <- copied{name: name, err: err}
		}()
	}
	if got := hold.Wait(len(copies), storetest.CopyHoldWait); got < len(copies) {
		t.Fatalf("%d of %d copies reached their VACUUM INTO within %s; the rest wait on a "+
			"connection their pool cannot give — a copy that draws its hold from the pool "+
			"needs two readers at once, where a node file at `store.max_open_conns: 1` "+
			"keeps one, and two such copies at once need four of the replicated estate's two",
			got, len(copies), storetest.CopyHoldWait)
	}
	// AND EACH FILE LISTS THE COPIES HOLDING ITS LOG, the one holding it
	// longest first: the node file its one, the replicated estate both of
	// its own.
	if held := node.Copies(); len(held) != 1 {
		t.Errorf("the node file lists %d copies holding its log, want its 1: %+v", len(held), held)
	}
	if held := replicated.Copies(); len(held) != 2 || held[1].Since.Before(held[0].Since) {
		t.Errorf("the replicated estate lists %+v holding its log, want its 2, the one "+
			"holding it longest first", held)
	}
	hold.Release(nil)
	timeout := time.After(storetest.CopyHoldWait)
	for range copies {
		select {
		case c := <-done:
			if c.err != nil {
				t.Errorf("%s: %v", c.name, c.err)
			}
		case <-timeout:
			t.Fatalf("a copy let through to its VACUUM INTO did not return within %s",
				storetest.CopyHoldWait)
		}
	}
}

const (
	// copyUnderWrites is how many copies the case under writes and
	// checkpoints takes, in about ten seconds under the race detector on
	// two cores. Measured there: a copy whose view nothing held came out torn
	// 20 times in 120, so forty copies catch it every run; one whose view was
	// taken before its hold, once in 120 — which is why DB.holdViewFor takes
	// both steps rather than leaving their order to a caller.
	copyUnderWrites = 40

	// copyProbeReaders is the readers the probe's pool is opened with: a
	// small pool, so staleEveryReader can hold every one of them at once.
	copyProbeReaders = 2

	// copyProbeBetweenRows is how many rows the table between the two probe
	// tables holds, a kibibyte each: enough pages that a copy reads the two
	// tables far apart, which is when a checkpoint can land between them.
	copyProbeBetweenRows = 1_000
)

// copyProbe is a replicated estate whose pool has copyProbeReaders readers,
// with one pinned writer and the tables a copy is judged by: crewlet_copy_a
// and crewlet_copy_b, written to the same value in ONE transaction, and
// crewlet_copy_between, created between them so a copy reads it in between.
type copyProbe struct {
	db *store.DB
	w  *store.Writer
}

func openCopyProbe(t *testing.T) *copyProbe {
	t.Helper()
	_, db := openReplicated(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{MaxOpenConns: copyProbeReaders}, 1)
	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE crewlet_copy_a (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`,
			`CREATE TABLE crewlet_copy_between (id INTEGER PRIMARY KEY, pad BLOB NOT NULL)`,
			`CREATE TABLE crewlet_copy_b (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`,
			`INSERT INTO crewlet_copy_a (id, n) VALUES (1, -1)`,
			`INSERT INTO crewlet_copy_b (id, n) VALUES (1, -1)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		for i := range copyProbeBetweenRows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO crewlet_copy_between (id, pad)
				VALUES (?, zeroblob(1024))`, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the probe tables: %v", err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("pin the writer: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return &copyProbe{db: db, w: w}
}

// commit writes n to both probe tables in one transaction on the pinned
// writer's connection, which no copy can draw.
func (p *copyProbe) commit(t *testing.T, n int64) {
	t.Helper()
	if err := p.tryCommit(t.Context(), n); err != nil {
		t.Fatalf("commit %d: %v", n, err)
	}
}

func (p *copyProbe) tryCommit(ctx context.Context, n int64) error {
	return p.w.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE crewlet_copy_a SET n = ? WHERE id = 1`, n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE crewlet_copy_b SET n = ? WHERE id = 1`, n)
		return err
	})
}

// staleEveryReader reads once on every connection a copy can draw, all at once
// so each read takes a connection of its own, and hands them back: each then
// carries the view its read took into whatever is asked of it next.
func (p *copyProbe) staleEveryReader(t *testing.T) {
	t.Helper()
	var read, release, done sync.WaitGroup
	read.Add(copyProbeReaders)
	release.Add(1)
	errs := make(chan error, copyProbeReaders)
	for range copyProbeReaders {
		done.Go(func() {
			// ONCE, and on every way out: a read that fails before its
			// body runs must still let the others be released rather
			// than hang the case, and one the store retries runs its
			// body twice.
			var once sync.Once
			signal := func() { once.Do(read.Done) }
			defer signal()
			errs <- p.db.Read(t.Context(), func(tx *sql.Tx) error {
				var n int64
				err := tx.QueryRowContext(t.Context(),
					`SELECT n FROM crewlet_copy_a WHERE id = 1`).Scan(&n)
				signal()
				release.Wait()
				return err
			})
		})
	}
	read.Wait()
	release.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("read on every connection: %v", err)
		}
	}
}

// checkpoint asks for a TRUNCATE checkpoint and reports whether it completed:
// one is refused rather than waited for while a reader holds a read mark.
func (p *copyProbe) checkpoint(ctx context.Context) bool {
	conn, err := p.db.SQL().Conn(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	var busy, frames, backfilled int64
	err = conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &frames, &backfilled)
	return err == nil && busy == 0
}

// readCopyProbe is the two probe tables as the copy at path holds them.
func readCopyProbe(t *testing.T, path string) (a, b int64) {
	t.Helper()
	ctx := t.Context()
	copied, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open the copy at %s: %v", path, err)
	}
	defer func() { _ = copied.Close() }()
	if err := copied.Read(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT n FROM crewlet_copy_a WHERE id = 1`).Scan(&a); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT n FROM crewlet_copy_b WHERE id = 1`).Scan(&b)
	}); err != nil {
		t.Fatalf("read the copy at %s: %v", path, err)
	}
	return a, b
}

// A BACKUP THAT CANNOT MAKE ITS TEMPORARY FILES NAMES WHERE THEY GO: the
// variable that places them and the directory it names. The driver builds the
// copy on a connection of its own, which keeps a temporary database under the
// first of TURSO_TMPDIR and SQLITE_TMPDIR that is set, else TMPDIR, for as
// long as the copy runs, so a directory there that is missing or read-only
// fails every backup, and the driver's words for it, "I/O error (tempdir):
// entity not found", name no variable and no path.
//
// THE CHECK IS THE VARIABLE AND ITS DIRECTORY TOGETHER, because every
// t.TempDir path carries the test's name, so the variable's name alone would
// be found there whatever the refusal said.
func TestABackupThatCannotMakeItsTemporaryFilesNamesWhereTheyGo(t *testing.T) {
	// One case per variable the driver can take the directory from, each
	// with the one it reads missing and every other one writable or unset:
	// the error must name the variable that placed the temporary database
	// and its directory, since the driver's own label is the same for all.
	for _, refused := range []string{"TMPDIR", "TURSO_TMPDIR", "SQLITE_TMPDIR"} {
		t.Run(refused, func(t *testing.T) {
			db := open(t)
			missing := filepath.Join(t.TempDir(), "missing")
			writable := t.TempDir()
			t.Setenv("TMPDIR", writable)
			for _, name := range []string{"TURSO_TMPDIR", "SQLITE_TMPDIR"} {
				unsetEnv(t, name)
			}
			t.Setenv(refused, missing)

			_, err := db.Backup(t.Context(), filepath.Join(t.TempDir(), "copy.db"))
			if err == nil {
				t.Fatalf("a backup succeeded with %s naming a directory that "+
					"does not exist, so this case no longer stages the failure "+
					"it is about", refused)
			}
			if want := refused + " (" + missing + ")"; !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q, so it does not say "+
					"what to change: %v", want, err)
			}
		})
	}
}

// unsetEnv unsets name for the rest of the test and puts it back after,
// which is what t.Setenv does for a value: an unset variable and one set to
// nothing are different things to the driver, which takes a set
// TURSO_TMPDIR or SQLITE_TMPDIR as the directory whatever it holds.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}
