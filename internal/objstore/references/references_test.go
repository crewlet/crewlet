package references_test

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// schema is the freshly migrated replicated estate's columns per table, and
// the column each of its indexes leads with.
//
// READ OFF THE DATABASE rather than off the migration source, so it judges the
// schema a node actually runs.
func schema(t *testing.T) (columns map[string][]string, leading map[string][]string) {
	t.Helper()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	columns, leading = map[string][]string{}, map[string][]string{}
	firstColumn := regexp.MustCompile(`(?is)\bON\s+"?\w+"?\s*\(\s*"?(\w+)`)
	err := storetest.ReplicatedDB(t, db.Replicated()).Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT m.name, i.name FROM sqlite_master m
			 JOIN pragma_table_info(m.name) i
			 WHERE m.type = 'table'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var table, column string
			if err := rows.Scan(&table, &column); err != nil {
				_ = rows.Close()
				return err
			}
			columns[table] = append(columns[table], column)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(t.Context(),
			`SELECT tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'index'`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var table, statement string
			if err := rows.Scan(&table, &statement); err != nil {
				return err
			}
			if m := firstColumn.FindStringSubmatch(statement); m != nil {
				leading[table] = append(leading[table], m[1])
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	if len(columns) < 20 {
		t.Fatalf("read %d tables — the walk is not reaching the schema", len(columns))
	}
	return columns, leading
}

// EVERY TABLE THAT NAMES AN OBJECT IS DECLARED, AND EVERY DECLARATION IS A
// TABLE THAT DOES — ADR-0026's enforcement.
//
// The failure it exists for is silent and destroys data: a table naming
// objects that nobody declared reads, to the collector, as no references at
// all, and its files' bytes are deleted a day after they were written. And a
// declaration is held to the columns it names — the digest and the size the
// backup verifies a copy against, the owner it reports a lost object as — and
// to an index LEADING WITH ITS KEY, because the collector seeks a batch of
// keys and the audit and the backup page through them in key order, and
// without one each is every row of the table.
func TestEveryTableThatNamesAnObjectIsDeclared(t *testing.T) {
	t.Parallel()
	columns, leading := schema(t)
	declared := map[string]bool{}
	for _, ref := range references.All {
		declared[ref.Table] = true
		if err := ref.Validate(); err != nil {
			t.Errorf("%s: %v", ref.Table, err)
			continue
		}
		have := columns[ref.Table]
		if have == nil {
			t.Errorf("%s is declared as naming objects and the schema has no such table", ref.Table)
			continue
		}
		if ref.Key != references.ObjectColumn {
			t.Errorf("%s names its objects %q; every referencing table calls the column %q, "+
				"which is what lets this gate find one", ref.Table, ref.Key, references.ObjectColumn)
		}
		for _, col := range append([]string{ref.Key, ref.Hash, ref.Size}, ref.Owner...) {
			if !slices.Contains(have, col) {
				t.Errorf("%s is declared with column %q and has %v", ref.Table, col, have)
			}
		}
		if !slices.Contains(leading[ref.Table], ref.Key) {
			t.Errorf("no index on %s leads with %s (its indexes lead with %v) — every "+
				"question the collector, the audit and the backup ask of it is a scan",
				ref.Table, ref.Key, leading[ref.Table])
		}
	}
	for table, have := range columns {
		if strings.HasPrefix(table, "sqlite_") {
			continue
		}
		if slices.Contains(have, references.ObjectColumn) && !declared[table] {
			t.Errorf("%s has an %q column and is not in references.All — the collector "+
				"reads its objects as unreferenced and deletes them a day after they "+
				"are written", table, references.ObjectColumn)
		}
	}
}

// assumedClockSkew is the most the clocks of the node deciding a write and the
// node running the collector are assumed to disagree by — an hour, which no
// node an engine runs on is anywhere near (NTP keeps them within
// milliseconds), stated so the margin the constants leave is a number rather
// than a hope.
const assumedClockSkew = time.Hour

// THE GRACE OUTLASTS THE LONGEST AN UPLOAD CAN GO UNRECORDED — ADR-0027's
// inequalities, pinned here because this is the one package every constant
// they name can be imported into.
//
// A key may be named by a write decided up to objstore.RecordWithin after it
// was minted, by the deciding node's clock; the collector deletes it only
// once it was minted more than collect.PendingGrace ago, by its own. So the
// grace has to exceed the bound by more than the two clocks can disagree, or
// a write could name a key a collection had just read as nobody's. And the
// bound itself has to outlast the slowest upload the engine accepts — a file
// of tracker.MaxFileBytes at the objstore.MiBPace floor — or a legal upload
// would finish only to be refused.
func TestTheGraceOutlastsTheLongestUnrecordedUpload(t *testing.T) {
	t.Parallel()
	if objstore.RecordWithin+assumedClockSkew >= collect.PendingGrace {
		t.Errorf("RecordWithin (%v) and an assumed clock skew of %v reach the "+
			"collector's grace (%v): a write may name a key the collector deletes",
			objstore.RecordWithin, assumedClockSkew, collect.PendingGrace)
	}
	slowest := objstore.PaceFor(tracker.MaxFileBytes)
	if slowest >= objstore.RecordWithin {
		t.Errorf("the largest file at the pacing floor takes %v to upload, and a "+
			"write may name a key only %v after it was minted", slowest, objstore.RecordWithin)
	}
	t.Logf("the slowest legal upload takes %v of the %v a write may name its key in, "+
		"and the grace leaves %v for the clocks to disagree by",
		slowest, objstore.RecordWithin, collect.PendingGrace-objstore.RecordWithin)
}
