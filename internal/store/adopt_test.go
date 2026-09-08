package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// ADOPTION OVER A LIVE DATABASE WITH A HOT -wal.
//
// The shape that matters: both files have uncommitted-to-the-database pages
// sitting in a -wal when the adoption starts, which is the ordinary state of
// any database this engine has written to. What the test asserts is that the
// adopted database reads back as the DONOR's — and that no sidecar of either
// file survives, because a -wal left beside a renamed database is pages of a
// database that no longer exists.
//
// Mutation: rename without checkpointing the prepared file and the adopted
// database is missing whatever was still in its -wal; remove the live file's
// sidecars after the rename instead of before, and the new database opens
// with the OLD one's pages applied to it.
func TestAdoptFileReplacesALiveDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	prepared := filepath.Join(dir, "prepared.db")

	seed := func(path, mark string) {
		t.Helper()
		db, err := store.Open(t.Context(), path, store.Options{})
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(),
				`CREATE TABLE crewlet_adopt_probe (mark TEXT NOT NULL)`); err != nil {
				return err
			}
			_, err := tx.ExecContext(t.Context(),
				`INSERT INTO crewlet_adopt_probe (mark) VALUES (?)`, mark)
			return err
		}); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		// CLOSED, but with a hot -wal: the engine's own pragmas leave WAL
		// mode on, so the write above is in the sidecar rather than the
		// database until something checkpoints it.
		if err := db.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
	}
	seed(live, "the-old-one")
	seed(prepared, "the-donor")

	if err := store.AdoptFile(t.Context(), live, prepared); err != nil {
		t.Fatalf("AdoptFile: %v", err)
	}

	if _, err := os.Stat(prepared); !os.IsNotExist(err) {
		t.Errorf("the prepared file is still at its own name after the rename "+
			"(stat err = %v): adoption moves it, it does not copy it", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(live + suffix); !os.IsNotExist(err) {
			t.Errorf("%s survived the adoption: a sidecar beside a replaced "+
				"database holds pages of the database that is gone", live+suffix)
		}
	}

	adopted, err := store.Open(t.Context(), live, store.Options{})
	if err != nil {
		t.Fatalf("open the adopted database: %v", err)
	}
	defer func() { _ = adopted.Close() }()
	var mark string
	if err := adopted.SQL().QueryRowContext(t.Context(),
		`SELECT mark FROM crewlet_adopt_probe`).Scan(&mark); err != nil {
		t.Fatalf("read the adopted database: %v", err)
	}
	if mark != "the-donor" {
		t.Errorf("adopted database says %q, want the donor's %q", mark, "the-donor")
	}
}

// A NODE WITH NO STORE OF ITS OWN adopts too — a fresh machine joining a fleet
// below the trim floor has nothing to replace, and that is an ordinary
// adoption rather than a missing precondition.
func TestAdoptFileWithNoLiveDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live := filepath.Join(dir, "absent.db")
	prepared := filepath.Join(dir, "prepared.db")

	db, err := store.Open(t.Context(), prepared, store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.AdoptFile(t.Context(), live, prepared); err != nil {
		t.Fatalf("AdoptFile onto nothing: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the adopted database is not at the live path: %v", err)
	}
}
