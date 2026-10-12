package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// rowidAliases are the three names SQLite gives a table's rowid.
var rowidAliases = []string{"oid", "rowid", "_rowid_"}

// NO COLUMN IS NAMED FOR THE ROWID, in either estate.
//
// SQLite lets a declared column shadow the rowid's alias, and every statement
// that names the column then reads the column — except where the pinned driver
// does not: in an upsert, `excluded.oid` resolves to the excluded row's ROWID,
// so `ON CONFLICT … DO UPDATE SET oid = excluded.oid` stores a small integer
// where the statement wrote a value. Measured on code_refs, whose object
// column was named `oid` until this gate: every ref an upsert moved — a branch
// advanced by a push, a deletion's tombstone — held "2" or "4", while a ref an
// insert created held its object id, so the bug read as correct on every fresh
// row. A name is cheap to change and a stored rowid is silent, so the names
// are refused outright rather than the statements that would trip over them.
func TestNoColumnIsNamedForTheRowid(t *testing.T) {
	t.Parallel()
	for _, estate := range []store.Estate{store.EstateNode, store.EstateReplicated} {
		columns := columnsIn(t, estate)
		if len(columns) != len(tablesIn(t, estate)) {
			t.Fatalf("read the columns of %d of the %s estate's %d tables", len(columns),
				estate, len(tablesIn(t, estate)))
		}
		for table, names := range columns {
			for _, column := range names {
				if slices.Contains(rowidAliases, strings.ToLower(column)) {
					t.Errorf("%s.%s (the %s estate) is named for the rowid — the pinned "+
						"driver reads `excluded.%s` in an upsert as the rowid; name the "+
						"column for what it holds", table, column, estate, column)
				}
			}
		}
	}
}

// columnsIn is every column of every table of one estate, as its migrations
// built them — each table read and none answering nothing.
func columnsIn(t *testing.T, estate store.Estate) map[string][]string {
	t.Helper()
	db, err := store.OpenEstate(context.Background(), estate,
		filepath.Join(t.TempDir(), string(estate)+".db"), store.Options{})
	if err != nil {
		t.Fatalf("open the %s estate: %v", estate, err)
	}
	defer func() { _ = db.Close() }()
	out := map[string][]string{}
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		for table := range tablesIn(t, estate) {
			rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
			if err != nil {
				return err
			}
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					_ = rows.Close()
					return err
				}
				out[table] = append(out[table], name)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if len(out[table]) == 0 {
				t.Errorf("%s has no columns in the %s estate — the read answered "+
					"nothing, so this gate would pass having read nothing", table, estate)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the %s estate's columns: %v", estate, err)
	}
	return out
}
