package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

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
