package tracker

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE RESET TOUCHES EXACTLY THE TABLES WHOSE `version` IS A LOG POSITION.
//
// It was a list typed beside a comment claiming it was derived, and it named
// three tables with no `version` column at all — so every reanchor failed at
// its fourth step with "no such column" before it moved anything — and one
// whose `version` is not a position: a body revision's is the BODY's own
// version and half of its primary key, which a reset to the generation's floor
// would collapse into one row per task. Held against the schema in both
// directions: every listed table carries a `version`, and every table this
// domain holds as its own rows that carries one is listed, or named below as
// not a position.
// Mutation: put a column-less table back, drop a positioned one, or list the
// body revisions, and a row goes red.
func TestTheResetTouchesExactlyTheTablesVersionedByPosition(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	// NOT POSITIONS, each with why, so an addition here is a decision a
	// reviewer reads rather than a table that quietly stops being reset.
	notPositions := map[string]string{
		"tracker_body_revisions": "the body's own version, half of the primary key",
	}
	versioned := func(table string) bool {
		t.Helper()
		var has bool
		if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `
				SELECT COUNT(*) > 0 FROM pragma_table_info(?) WHERE name = 'version'`,
				table).Scan(&has)
		}); err != nil {
			t.Fatalf("read %s's columns: %v", table, err)
		}
		return has
	}

	for _, table := range versionedTables {
		if !versioned(table) {
			t.Errorf("%s is reset and has no version column: every reanchor "+
				"fails on it", table)
		}
		if why, not := notPositions[table]; not {
			t.Errorf("%s is reset and its version is not a position (%s)", table, why)
		}
	}
	for table, class := range (Domain{}).Tables() {
		// THE DOMAIN'S OWN ROWS: the log's machinery (the ledger, the
		// deferred records) is this node's alone and holds no object a
		// writer forms an expectation from.
		if class == statelog.Local || !versioned(table) ||
			slices.Contains(versionedTables, table) {
			continue
		}
		if _, not := notPositions[table]; !not {
			t.Errorf("%s carries a version and the reset leaves it at the old "+
				"generation: list it, or name it as not a position", table)
		}
	}
}
