package store_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// THE DONOR SCRUBS, and a name it cannot find is REFUSED rather than skipped.
//
// The artefact says what it emptied and a recipient verifies that list, so a
// name nobody can find is either a table that was renamed — in which case the
// real one is travelling while the manifest claims it was emptied — or a list
// that has drifted from the schema. Skipping it turns both into a snapshot
// that ships one node's private state under a claim that it did not.
func TestAScrubEmptiesWhatItNamesAndRefusesWhatItCannotFind(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "artefact.db")
	seed(t, path, `
		CREATE TABLE fleet_rows (id INTEGER PRIMARY KEY);
		CREATE TABLE local_rows (id INTEGER PRIMARY KEY);
		INSERT INTO fleet_rows (id) VALUES (1), (2), (3);
		INSERT INTO local_rows (id) VALUES (1), (2);`)

	scrubbed, err := store.ScrubFile(t.Context(), path, []string{"local_rows"})
	if err != nil {
		t.Fatalf("ScrubFile: %v", err)
	}
	if len(scrubbed) != 1 || scrubbed[0] != "local_rows" {
		t.Fatalf("scrubbed %v, want [local_rows]", scrubbed)
	}
	if got := count(t, path, "local_rows"); got != 0 {
		t.Errorf("local_rows holds %d row(s) after a scrub", got)
	}
	if got := count(t, path, "fleet_rows"); got != 3 {
		t.Errorf("fleet_rows holds %d row(s) — a scrub empties what it names "+
			"and nothing else", got)
	}

	// AND A NAME NOBODY CAN FIND STOPS THE WHOLE THING.
	_, err = store.ScrubFile(t.Context(), path, []string{"renamed_away"})
	if !errors.Is(err, store.ErrScrubTable) {
		t.Fatalf("a scrub of a table that is not there = %v, want ErrScrubTable", err)
	}
	if !strings.Contains(err.Error(), "renamed_away") {
		t.Errorf("the refusal does not name the table: %v", err)
	}
}

// A RECIPIENT VERIFIES THE CLAIM RATHER THAN TRUSTING IT.
//
// The manifest is a list of what the donor says it emptied, and the whole
// safety argument for accepting an artefact at all is that everything still in
// it is fleet-visible. A claim nobody checks is a claim.
func TestARecipientCanCheckWhatADonorSaysItEmptied(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "artefact.db")
	seed(t, path, `
		CREATE TABLE fleet_rows (id INTEGER PRIMARY KEY);
		CREATE TABLE local_rows (id INTEGER PRIMARY KEY);
		INSERT INTO fleet_rows (id) VALUES (1);
		INSERT INTO local_rows (id) VALUES (1);`)

	empty, err := store.EmptyTables(t.Context(), path, []string{"fleet_rows", "local_rows"})
	if err != nil {
		t.Fatalf("EmptyTables: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("EmptyTables on an unscrubbed file = %v, want none", empty)
	}

	if _, err := store.ScrubFile(t.Context(), path, []string{"local_rows"}); err != nil {
		t.Fatalf("ScrubFile: %v", err)
	}
	empty, err = store.EmptyTables(t.Context(), path, []string{"fleet_rows", "local_rows"})
	if err != nil {
		t.Fatalf("EmptyTables: %v", err)
	}
	if len(empty) != 1 || empty[0] != "local_rows" {
		t.Fatalf("EmptyTables = %v, want [local_rows] — a recipient that could "+
			"not tell would have to trust the donor's own list", empty)
	}
}

// A SCRUB LEAVES NO SIDECAR BEHIND, because the deletions have to be in the
// file that travels.
//
// Committed data lives in the database AND its write-ahead log, so a copy of
// either alone is torn — and an artefact whose deletions were still in a
// sidecar would arrive with the rows it claims to have emptied.
func TestAScrubLeavesItsDeletionsInTheFileThatTravels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "artefact.db")
	seed(t, path, `
		CREATE TABLE local_rows (id INTEGER PRIMARY KEY);
		INSERT INTO local_rows (id) VALUES (1), (2), (3);`)

	if _, err := store.ScrubFile(t.Context(), path, []string{"local_rows"}); err != nil {
		t.Fatalf("ScrubFile: %v", err)
	}
	for _, sidecar := range []string{"-wal", "-shm", "-tshm"} {
		if _, err := os.Stat(path + sidecar); err == nil {
			t.Errorf("%s survives the scrub — the artefact travels as one file, "+
				"so a deletion left in a sidecar arrives undone", path+sidecar)
		}
	}
	// And the rows are gone as seen by a fresh open of the file alone.
	if got := count(t, path, "local_rows"); got != 0 {
		t.Fatalf("local_rows holds %d row(s) when the file is opened on its own",
			got)
	}
}

// A TABLE NAME IS INTERPOLATED, so the set it may come from is narrow.
func TestAScrubRefusesANameItWouldHaveToQuote(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "artefact.db")
	seed(t, path, `CREATE TABLE rows_here (id INTEGER PRIMARY KEY);`)
	for _, name := range []string{
		"", "Rows", "rows here", `rows"; DROP TABLE rows_here; --`,
		"sqlite_master", "1rows",
	} {
		if _, err := store.ScrubFile(t.Context(), path, []string{name}); !errors.Is(err, store.ErrScrubTable) {
			t.Errorf("a scrub of %q = %v, want ErrScrubTable", name, err)
		}
	}
}

// seed creates a database file with the given schema and rows.
func seed(t *testing.T, path, ddl string) {
	t.Helper()
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), ddl)
		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// count reads one table's row count through a fresh open of the file alone.
func count(t *testing.T, path, table string) int64 {
	t.Helper()
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()
	var n int64
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
