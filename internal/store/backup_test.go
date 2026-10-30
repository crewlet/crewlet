package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
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
