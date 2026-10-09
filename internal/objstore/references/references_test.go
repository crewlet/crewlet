package references_test

import (
	"database/sql"
	"fmt"
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

// faults is everything the gate finds wrong with one declaration against the
// schema, as messages naming it: one that does not validate — a missing or
// unknown standing among the reasons (ADR-0033) — a table the schema lacks, a
// key column not called [references.ObjectColumn], a declared column the table
// does not have, and no index leading with the key.
//
// A declaration that does not validate answers that alone: every other check
// reads fields Validate vouches for.
func faults(ref objstore.ReferenceTable, columns, leading map[string][]string) []string {
	if err := ref.Validate(); err != nil {
		return []string{fmt.Sprintf("%s: %v", ref.Table, err)}
	}
	have := columns[ref.Table]
	if have == nil {
		return []string{fmt.Sprintf("%s is declared as naming objects and the schema has no such table", ref.Table)}
	}
	var out []string
	if ref.Key != references.ObjectColumn {
		out = append(out, fmt.Sprintf("%s names its objects %q; every referencing table calls the column %q, "+
			"which is what lets this gate find one", ref.Table, ref.Key, references.ObjectColumn))
	}
	for _, col := range append([]string{ref.Key, ref.Hash, ref.Size}, ref.Owner...) {
		if !slices.Contains(have, col) {
			out = append(out, fmt.Sprintf("%s is declared with column %q and has %v", ref.Table, col, have))
		}
	}
	if !slices.Contains(leading[ref.Table], ref.Key) {
		out = append(out, fmt.Sprintf("no index on %s leads with %s (its indexes lead with %v) — every "+
			"question the collector, the audit and the backup ask of it is a scan",
			ref.Table, ref.Key, leading[ref.Table]))
	}
	return out
}

// EVERY TABLE THAT NAMES AN OBJECT IS DECLARED, AND EVERY DECLARATION IS A
// TABLE THAT DOES — ADR-0026's enforcement, and ADR-0033's: every declaration
// states its standing.
//
// The failure it exists for is silent and destroys data: a table naming
// objects that nobody declared reads, to the collector, as no references at
// all, and its files' bytes are deleted a day after they were written. And a
// declaration is held to the columns it names — the digest and the size the
// backup verifies a copy against, the owner it reports a lost object as — and
// to an index LEADING WITH ITS KEY, because the collector seeks a batch of
// keys and the audit and the backup page through them in key order, and
// without one each is every row of the table. And it is held to a STANDING,
// because either default is silent: a live table read as retired is audited
// by nobody and accounted for by no backup.
func TestEveryTableThatNamesAnObjectIsDeclared(t *testing.T) {
	t.Parallel()
	columns, leading := schema(t)

	// CONTROLS FIRST: a guard asserting an absence of faults passes
	// identically when the list is right and when the guard has gone inert,
	// so it is shown a declaration it must refuse — one stating no
	// standing, one naming a table the schema lacks — and one it must not.
	undeclared := tracker.FileObjectReferences
	undeclared.Standing = ""
	if got := faults(undeclared, columns, leading); len(got) == 0 ||
		!strings.Contains(strings.Join(got, "\n"), "Standing") {
		t.Errorf("a declaration stating no standing drew %q; want it refused for that", got)
	}
	absent := tracker.FileObjectReferences
	absent.Table = "tracker_files_never"
	if got := faults(absent, columns, leading); len(got) == 0 {
		t.Error("a declaration of a table the schema lacks drew no fault")
	}
	retired := tracker.FileObjectReferences
	retired.Standing = objstore.Retired
	if got := faults(retired, columns, leading); len(got) != 0 {
		t.Errorf("a retired declaration of an indexed table drew %q; retiring is a standing, not a fault", got)
	}

	declared := map[string]bool{}
	required := 0
	for _, ref := range references.All {
		declared[ref.Table] = true
		if ref.Standing == objstore.Required {
			required++
		}
		for _, fault := range faults(ref, columns, leading) {
			t.Error(fault)
		}
	}
	t.Logf("%d declared tables, %d of them required", len(references.All), required)
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

// retirement is what a RETIRED declaration owes (ADR-0033), reviewed here,
// beside the gate that holds it: a retired table nobody sweeps keeps its
// objects for ever, and one whose grace is shorter than the longest read of
// what its rows name deletes the object under that read. Each field is the
// declaring domain's own constant, never a copy of one, so the inequality the
// gate checks is the one the domain runs.
type retirement struct {
	// grace is how long the domain keeps a retired row — the whole time the
	// row keeps its object from collection.
	grace time.Duration

	// largest is the largest object a row of the table can name, which
	// bounds the longest read the grace has to outlast.
	largest int64

	// sweep is the job that deletes a retired row once its grace has
	// passed, as the engine's sweep list
	// (internal/engine.TestTheEngineSweepsEveryShortHorizonTable) names it.
	sweep string
}

// retirements is every RETIRED declaration in references.All, keyed by its
// table: the one place a retirement's grace and sweep are reviewed.
//
// EMPTY while no domain retires a reference. The gate is two-sided, as
// skipgate's Always entries are: a retired declaration with no entry here
// fails, and so does an entry no retired declaration matches, so the list can
// be neither short nor stale.
var retirements = map[string]retirement{}

// retirementFaults is everything wrong with how declared's RETIRED tables are
// reviewed in reviewed, as messages naming the table: a retired table with no
// entry, an entry for a table that is not declared retired, and an entry whose
// sweep is unnamed, whose largest object is unstated or whose grace does not
// outlast the longest read of that object by more than the clocks stamping
// and sweeping the row can disagree.
func retirementFaults(declared []objstore.ReferenceTable, reviewed map[string]retirement) []string {
	var out []string
	retired := map[string]bool{}
	for _, ref := range declared {
		if ref.Standing != objstore.Retired {
			continue
		}
		retired[ref.Table] = true
		r, ok := reviewed[ref.Table]
		if !ok {
			out = append(out, fmt.Sprintf("%s is declared objstore.Retired and has no entry in "+
				"retirements (internal/objstore/references/references_test.go) — ADR-0033 holds "+
				"a retired table to a sweep that deletes its rows and a grace that outlasts the "+
				"longest read of what they name: add its domain's grace, the largest object a "+
				"row names and the job that sweeps the rows, and list that job in "+
				"internal/engine.TestTheEngineSweepsEveryShortHorizonTable", ref.Table))
			continue
		}
		if strings.TrimSpace(r.sweep) == "" {
			out = append(out, fmt.Sprintf("%s is retired and names no job that sweeps its rows — "+
				"a retired row nobody deletes keeps its object for ever (ADR-0033)", ref.Table))
		}
		if r.largest <= 0 {
			out = append(out, fmt.Sprintf("%s is retired and states no largest object (%d bytes), "+
				"so nothing bounds the read its grace must outlast (ADR-0033)", ref.Table, r.largest))
			continue
		}
		if longest := objstore.ReadBudget(r.largest); r.grace <= longest+assumedClockSkew {
			out = append(out, fmt.Sprintf("%s keeps a retired row for %v, and a read of its "+
				"largest object (%d bytes) may take objstore.ReadBudget = %v, plus an assumed "+
				"clock skew of %v between the node stamping the row and the node sweeping it — "+
				"the object can be deleted under a read still fetching it (ADR-0033)",
				ref.Table, r.grace, r.largest, longest, assumedClockSkew))
		}
	}
	for table := range reviewed {
		if !retired[table] {
			out = append(out, fmt.Sprintf("retirements reviews %s, which no declaration in "+
				"references.All retires — a stale entry vouches for nothing", table))
		}
	}
	slices.Sort(out)
	return out
}

// A RETIRED TABLE IS SWEPT, AND OUTLIVES ITS LONGEST READ — the two
// obligations ADR-0033 places on a retired declaration, which no other gate
// can see: the collector deletes an object the moment nothing names it, so a
// retired row nobody deletes keeps its object for ever, and one deleted before
// the longest read of its object ends deletes the object under that read.
//
// A TRIPWIRE before it is a measurement: while nothing retires a reference the
// list is empty and so is every fault, so the controls below are what show the
// gate would fire on the first retired declaration to land without a reviewed
// entry, and on every way such an entry can be wrong.
func TestARetiredTableIsSweptAndOutlivesItsLongestRead(t *testing.T) {
	t.Parallel()
	retired := tracker.FileObjectReferences
	retired.Table, retired.Standing = "test_retired_objects", objstore.Retired
	longest := objstore.ReadBudget(tracker.MaxFileBytes)
	sound := retirement{grace: longest + assumedClockSkew + time.Minute,
		largest: tracker.MaxFileBytes, sweep: "test_retired_objects"}
	with := func(edit func(*retirement)) map[string]retirement {
		r := sound
		edit(&r)
		return map[string]retirement{retired.Table: r}
	}
	for _, c := range []struct {
		name     string
		declared []objstore.ReferenceTable
		reviewed map[string]retirement
		want     string // a fault naming this, or "" for none
	}{
		{"a retired table nobody reviewed", []objstore.ReferenceTable{retired}, nil, "no entry in retirements"},
		{"an entry nothing retires", []objstore.ReferenceTable{tracker.FileObjectReferences},
			with(func(*retirement) {}), "stale"},
		{"a required table reviewed as retired",
			[]objstore.ReferenceTable{tracker.FileObjectReferences},
			map[string]retirement{tracker.FileObjectReferences.Table: sound}, "stale"},
		{"a retirement nothing sweeps", []objstore.ReferenceTable{retired},
			with(func(r *retirement) { r.sweep = " " }), "sweeps its rows"},
		{"a retirement stating no largest object", []objstore.ReferenceTable{retired},
			with(func(r *retirement) { r.largest = 0 }), "largest object"},
		{"a grace the longest read outlasts", []objstore.ReferenceTable{retired},
			with(func(r *retirement) { r.grace = longest }), "ReadBudget"},
		{"a grace the clocks can eat", []objstore.ReferenceTable{retired},
			with(func(r *retirement) { r.grace = longest + assumedClockSkew }), "clock skew"},
		{"a sound retirement", []objstore.ReferenceTable{retired}, with(func(*retirement) {}), ""},
	} {
		got := retirementFaults(c.declared, c.reviewed)
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s drew %q; want no fault", c.name, got)
		case c.want != "" && !strings.Contains(strings.Join(got, "\n"), c.want):
			t.Errorf("%s drew %q; want a fault naming %q", c.name, got, c.want)
		}
	}

	for _, fault := range retirementFaults(references.All, retirements) {
		t.Error(fault)
	}
	t.Logf("%d retired tables reviewed", len(retirements))
}
