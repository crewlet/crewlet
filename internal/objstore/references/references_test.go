package references_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/store"
)

// EVERY TABLE THAT NAMES A CHUNK IS DECLARED, AND EVERY DECLARATION IS A TABLE
// THAT DOES — ADR-0019's enforcement.
//
// Read off a freshly migrated replicated estate rather than off the migration
// source, so it judges the schema a node actually runs. The failure it exists
// for is silent and destroys data: a table naming chunks that nobody declared
// reads, to the collector, as no references at all, and its files' bytes are
// deleted a day after they were written.
func TestEveryTableThatNamesAChunkIsDeclared(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	columns := map[string][]string{}
	rows, err := db.Replicated().SQL().QueryContext(t.Context(),
		`SELECT m.name, i.name FROM sqlite_master m
		 JOIN pragma_table_info(m.name) i
		 WHERE m.type = 'table'`)
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		columns[table] = append(columns[table], column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(columns) < 20 {
		t.Fatalf("read %d tables — the walk is not reaching the schema", len(columns))
	}

	declared := map[string]bool{}
	for _, ref := range references.All {
		declared[ref.Table] = true
		have := columns[ref.Table]
		switch {
		case have == nil:
			t.Errorf("%s is declared as naming chunks and the schema has no such table", ref.Table)
		case ref.Column != references.ChunkColumn:
			t.Errorf("%s names its chunks %q; every referencing table calls the column %q, "+
				"which is what lets this gate find one", ref.Table, ref.Column, references.ChunkColumn)
		case !slices.Contains(have, ref.Column) || !slices.Contains(have, ref.Group):
			t.Errorf("%s is declared with columns %q and %q and has %v",
				ref.Table, ref.Column, ref.Group, have)
		}
	}
	for table, have := range columns {
		if strings.HasPrefix(table, "sqlite_") {
			continue
		}
		if slices.Contains(have, references.ChunkColumn) && !declared[table] {
			t.Errorf("%s has a %q column and is not in references.All — the collector "+
				"reads its chunks as unreferenced and deletes them a day after they "+
				"are written", table, references.ChunkColumn)
		}
	}
}
