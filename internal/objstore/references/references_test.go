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

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
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
// unknown standing among the reasons (ADR-0051) — a table the schema lacks, a
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
// TABLE THAT DOES — ADR-0026's enforcement, and ADR-0051's: every declaration
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
// because every default is silent: a live table read as retired or derived is
// audited by nobody and accounted for by no backup.
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
	for _, s := range []objstore.Standing{objstore.Retired, objstore.Derived} {
		unowed := tracker.FileObjectReferences
		unowed.Standing = s
		if got := faults(unowed, columns, leading); len(got) != 0 {
			t.Errorf("a %s declaration of an indexed table drew %q; a standing is not a fault", s, got)
		}
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

// EVERY REQUIRED TABLE SAYS HOW ITS LOG NAMES OBJECTS (ADR-0052). A backup
// restores its store copy and replays the declaring domain's log from the
// copy's position — and a newer build applies what the copy's applier deferred
// — so it owes every object a record of either names, and it learns how a
// record names one only from the declaration: the log's stream, the deferred
// records' table, and the domain's own reader of one record. A Required table
// missing any of them is one whose files written during a backup no backup
// carries. The deferred table is held to the schema too: one the replicated
// estate does not have, or one without the framework's position and payload,
// is a backup refused at its first read of it.
func TestEveryRequiredTableSaysHowItsLogNamesObjects(t *testing.T) {
	t.Parallel()
	columns, _ := schema(t)

	// CONTROLS: each half missing is refused, naming it — and a retired or
	// derived table, which no backup carries, needs none.
	for name, broken := range map[string]func(*objstore.ReferenceTable){
		"Log":      func(r *objstore.ReferenceTable) { r.Log = "" },
		"Deferred": func(r *objstore.ReferenceTable) { r.Deferred = "" },
		"Records":  func(r *objstore.ReferenceTable) { r.Records = nil },
	} {
		ref := tracker.FileObjectReferences
		broken(&ref)
		if err := ref.ValidateLog(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a Required declaration with no %s drew %v; want it refused naming it", name, err)
		}
	}
	for _, s := range []objstore.Standing{objstore.Retired, objstore.Derived} {
		unowed := tracker.FileObjectReferences
		unowed.Standing, unowed.Log, unowed.Deferred, unowed.Records = s, "", "", nil
		if err := unowed.ValidateLog(); err != nil {
			t.Errorf("a %s declaration naming no log was refused: %v", s, err)
		}
	}

	for _, ref := range references.All {
		if err := ref.ValidateLog(); err != nil {
			t.Error(err)
			continue
		}
		if ref.Standing != objstore.Required {
			continue
		}
		have := columns[ref.Deferred]
		if !slices.Contains(have, "position") || !slices.Contains(have, "payload") {
			t.Errorf("%s names %s as the records its log deferred, and the replicated "+
				"schema holds no such table with a position and a payload (%v)",
				ref.Table, ref.Deferred, have)
		}
	}
}

// standing is the standing one declaration states, as reviewed here, and why
// it has it.
type standing struct {
	is     objstore.Standing
	reason string
}

// standings is every declaration in references.All, keyed by its table: the
// standing it states and the reason it has that one — the one place a table's
// standing is a reviewed decision rather than a field its author filled in.
// TWO-SIDED, as [retirements] is: a declaration with no row fails, and so does
// a row no declaration matches.
var standings = map[string]standing{
	"tracker_files": {objstore.Required,
		"a project's file: bytes a person or a seat wrote, which nothing builds again"},
	"code_packs": {objstore.Required,
		"a repository's history: the pack bundle a push stored, which every git node's copy is built from"},
	"code_packs_retired": {objstore.Retired,
		"a pack a compaction replaced: nothing new reads it, and a read that began on it gets its grace before the row is swept"},
	"code_layer_parts": {objstore.Derived,
		"a published multi-pack-index layer's bytes: any git node builds the layer again from the repository's packs, and one it cannot fetch it does without"},
	"code_check_logs": {objstore.Required,
		"a failed check's log, which a reader of the failure is sent to and nothing builds again — the run is gone"},
	"artifact_objects": {objstore.Required,
		"an artifact version's file, which a retained version serves"},
	"artifact_assets": {objstore.Required,
		"an artifact's live asset, a file somebody added to it"},
	"artifact_brand_files": {objstore.Required,
		"the adopted brand's stylesheet, logos and faces, which a version that pinned it serves"},
}

// standingFaults is everything wrong with how declared's standings are
// reviewed in reviewed, as messages naming the table: a declaration that does
// not validate, one with no row, one whose standing is not its row's, a row
// that gives no reason, and a row no declaration matches.
func standingFaults(declared []objstore.ReferenceTable, reviewed map[string]standing) []string {
	var out []string
	seen := map[string]bool{}
	for _, ref := range declared {
		seen[ref.Table] = true
		if err := ref.Validate(); err != nil {
			out = append(out, fmt.Sprintf("%s: %v", ref.Table, err))
			continue
		}
		r, ok := reviewed[ref.Table]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s is declared %s and has no row in standings "+
				"(internal/objstore/references/references_test.go) — a standing is a reviewed "+
				"decision (ADR-0051): add the table, the standing it states and why", ref.Table,
				ref.Standing))
		case r.is != ref.Standing:
			out = append(out, fmt.Sprintf("%s is declared %s and reviewed as %s (%s) — a "+
				"standing changed is a promise changed: review the row with the declaration",
				ref.Table, ref.Standing, r.is, r.reason))
		case strings.TrimSpace(r.reason) == "":
			out = append(out, fmt.Sprintf("%s is reviewed as %s and gives no reason — the "+
				"reason is what a reviewer of the standing reads", ref.Table, r.is))
		}
	}
	for table := range reviewed {
		if !seen[table] {
			out = append(out, fmt.Sprintf("standings reviews %s, which references.All does not "+
				"declare — a stale row vouches for nothing", table))
		}
	}
	slices.Sort(out)
	return out
}

// EVERY DECLARATION STATES ITS STANDING, AND THE STANDING IS A REVIEWED
// DECISION (ADR-0051). Validate refuses a declaration that states none, but
// any of the three passes it, and each is a different promise: a Required
// table's objects are audited and carried by every backup, a Retired one's are
// kept for its domain's grace and then let go, and a Derived one's are kept
// while named and never audited or carried, because the engine builds them
// again. Each wrong one is silent: a live table declared derived is audited by
// nobody and in no backup, a rebuildable one declared required pages somebody
// about a loss nobody has to repair, and a retired one declared derived lets
// its object go under a reader still fetching it. So every entry of the list
// is held to a row naming its standing and why, and a new declaration lands
// with its row.
func TestEveryDeclarationStatesItsStanding(t *testing.T) {
	t.Parallel()
	// CONTROLS FIRST, for TestEveryTableThatNamesAnObjectIsDeclared's reason:
	// every way a review can be wrong draws a fault naming it, and a sound one
	// draws none.
	file := tracker.FileObjectReferences
	asDerived := file
	asDerived.Standing = objstore.Derived
	unstated := file
	unstated.Standing = ""
	reviewed := map[string]standing{file.Table: {objstore.Required, "a project's file"}}
	for _, c := range []struct {
		name     string
		declared []objstore.ReferenceTable
		reviewed map[string]standing
		want     string // a fault naming this, or "" for none
	}{
		{"a declaration nobody reviewed", []objstore.ReferenceTable{file}, nil, "no row in standings"},
		{"a row nothing declares", nil, reviewed, "stale"},
		{"a standing its row does not hold", []objstore.ReferenceTable{asDerived}, reviewed,
			"declared derived and reviewed as required"},
		{"a row that gives no reason", []objstore.ReferenceTable{file},
			map[string]standing{file.Table: {objstore.Required, " "}}, "gives no reason"},
		{"a declaration stating no standing", []objstore.ReferenceTable{unstated}, reviewed, "Standing"},
		{"a sound review", []objstore.ReferenceTable{file}, reviewed, ""},
	} {
		got := standingFaults(c.declared, c.reviewed)
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s drew %q; want no fault", c.name, got)
		case c.want != "" && !strings.Contains(strings.Join(got, "\n"), c.want):
			t.Errorf("%s drew %q; want a fault naming %q", c.name, got, c.want)
		}
	}

	for _, fault := range standingFaults(references.All, standings) {
		t.Error(fault)
	}
	t.Logf("%d declarations reviewed", len(standings))
}

// twoClocks is the most the clocks of two machines may disagree by while no
// node raises `clock_skew`: each within objstore.AssumedClockSkew of the
// fleet's clock, the median of the data nodes' clocks, so the two within twice
// it — the reading every inequality here compares two machines' clocks under
// (see objstore.AssumedClockSkew).
const twoClocks = 2 * objstore.AssumedClockSkew

// A HOLD RECORD COVERS EVERY PASS IN FLIGHT (M3-decisions M3-O61). A turn that
// takes its hold on a seat's scratch repository RECORDS it on the repository's
// subject only where the repository has been idle code.HoldRecordAfter or more,
// by the holding node's clock read after the hold; under that it keeps the hold
// in memory, or on its run row, alone — which the code-retention pass reads only
// AFTER it has fixed its cutoff. That is safe only while a repository idle less
// than the threshold is past no in-flight pass's cutoff, the pass's clock
// against the same broker-stamped activity: the threshold and the two clocks'
// disagreement together must not reach the shortest idle time a company may
// set (config.CodeScratchIdleDaysFloor days). Otherwise a pass already deciding
// could expire a repository a turn has just named and not recorded.
//
// Mutation: HoldRecordAfter at 23 h — red.
func TestAHoldRecordCoversEveryPassInFlight(t *testing.T) {
	t.Parallel()
	shortest := 24 * time.Hour * time.Duration(config.CodeScratchIdleDaysFloor)
	if reach := code.HoldRecordAfter + twoClocks; reach > shortest {
		t.Errorf("an unrecorded hold may be taken on a repository idle up to %v (code.HoldRecordAfter) by a "+
			"clock up to %v (twice objstore.AssumedClockSkew) off the pass's, %v in all, past the shortest "+
			"idle time a pass expires at (%v): a pass in flight could expire a repository a turn holds",
			code.HoldRecordAfter, twoClocks, reach, shortest)
	}
}

// THE GRACE OUTLASTS THE LONGEST AN UPLOAD CAN GO UNRECORDED — ADR-0027's
// inequalities, pinned here because this is the one package every constant
// they name can be imported into.
//
// A key may be named by a write decided up to objstore.RecordWithin after it
// was minted, by the deciding node's clock; the collector deletes it only
// once it was minted more than collect.PendingGrace ago, by its own. So the
// grace has to exceed the bound by more than the two nodes' clocks can
// disagree, or a write could name a key a collection had just read as
// nobody's — and two nodes may disagree by twice objstore.AssumedClockSkew
// while neither raises `clock_skew`. And the bound itself has to outlast the
// slowest upload the engine accepts — the largest object any declaring domain
// stores (a file of tracker.MaxFileBytes, a pack bundle of code.MaxPackBytes,
// a part of a published layer of code.LayerPartBytes, a failed check's log of
// code.MaxCheckLogBytes, an artifact's file or asset of
// artifacts.MaxTextFileBytes) at the objstore.MiBPace floor — or a legal
// upload would finish only to be refused.
//
// The controls show the first inequality is held to two nodes' clocks: a grace
// past the bound by one node's skew and not two is refused.
func TestTheGraceOutlastsTheLongestUnrecordedUpload(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		grace time.Duration
		fault bool
	}{
		{"a grace past the bound and two nodes' skew", objstore.RecordWithin + twoClocks + time.Minute, false},
		{"a grace at the bound and two nodes' skew", objstore.RecordWithin + twoClocks, true},
		{"a grace past the bound and one node's skew, not two",
			objstore.RecordWithin + objstore.AssumedClockSkew + time.Minute, true},
	} {
		if got := uploadFault(objstore.RecordWithin, c.grace); (got != "") != c.fault {
			t.Errorf("%s drew %q; want a fault: %v", c.name, got, c.fault)
		}
	}
	if fault := uploadFault(objstore.RecordWithin, collect.PendingGrace); fault != "" {
		t.Error(fault)
	}
	for _, largest := range []struct {
		name  string
		bytes int64
	}{
		{"tracker.MaxFileBytes", tracker.MaxFileBytes},
		{"code.MaxPackBytes", code.MaxPackBytes},
		{"code.LayerPartBytes", code.LayerPartBytes},
		{"code.MaxCheckLogBytes", code.MaxCheckLogBytes},
		{"artifacts.MaxTextFileBytes", artifacts.MaxTextFileBytes},
	} {
		slowest := objstore.PaceFor(largest.bytes)
		if slowest >= objstore.RecordWithin {
			t.Errorf("an object of %s (%d bytes) takes %v to upload at the pacing floor, "+
				"and a write may name a key only %v after it was minted", largest.name,
				largest.bytes, slowest, objstore.RecordWithin)
		}
		t.Logf("the slowest legal upload of %s takes %v of the %v a write may name its "+
			"key in, and the grace leaves %v for the clocks to disagree by, against "+
			"the %v they may", largest.name, slowest, objstore.RecordWithin,
			collect.PendingGrace-objstore.RecordWithin, twoClocks)
	}
}

// uploadFault is what is wrong with a collector's grace against the bound on
// the write that names a key, or "" when the grace outlasts the bound by more
// than two nodes' clocks may disagree.
func uploadFault(recordWithin, grace time.Duration) string {
	if recordWithin+twoClocks < grace {
		return ""
	}
	return fmt.Sprintf("RecordWithin (%v) and the %v two nodes' clocks may disagree by "+
		"(twice objstore.AssumedClockSkew) reach the collector's grace (%v): a write may "+
		"name a key the collector deletes", recordWithin, twoClocks, grace)
}

// retirement is what a RETIRED declaration owes (ADR-0051), reviewed here,
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

	// sweep is what deletes a retired row once its grace has passed: a job
	// of the engine's maintenance worker, as its sweep list
	// (internal/engine.TestTheEngineSweepsEveryShortHorizonTable) names it,
	// or a duty whose records delete the rows, as the duty's own engine test
	// holds it — a record-driven sweep is no job of that worker, and that
	// list asserts the worker's jobs exactly.
	sweep string
}

// retirements is every RETIRED declaration in references.All, keyed by its
// table: the one place a retirement's grace and sweep are reviewed.
//
// The gate is two-sided, as skipgate's Always entries are: a retired
// declaration with no entry here fails, and so does an entry no retired
// declaration matches, so the list can be neither short nor stale.
var retirements = map[string]retirement{
	// THE CODE HOST'S RETIRED PACKS: a day, over the 21 h 21 min a clone of
	// the largest bundle a row can name may read it (ADR-0060), removed by
	// the code-retention duty's retire_sweep records and held to that by
	// internal/engine.TestARetirementIsSweptByARecordTheEngineRuns.
	code.RetiredPackReferences.Table: {grace: code.RetireGrace, largest: code.MaxPackBytes,
		sweep: "the code-retention duty's retire_sweep records (code.Store.RetireSweep)"},
}

// retirementFaults is everything wrong with how declared's RETIRED tables are
// reviewed in reviewed, as messages naming the table: a retired table with no
// entry, an entry for a table that is not declared retired, and an entry whose
// sweep is unnamed, whose largest object is unstated or whose grace does not
// outlast the longest read of that object by more than the clocks stamping
// and sweeping the row can disagree.
//
// TWO MACHINES' CLOCKS, never one's against a reference: a retirement is
// stamped by the broker member leading its domain's log and swept by the
// clock of whichever node holds the sweep, and `clock_skew` holds each of
// them to objstore.AssumedClockSkew of a third — the fleet's clock, the median
// of the data nodes' clocks, on an embedded broker the stamping member's among
// them.
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
				"retirements (internal/objstore/references/references_test.go) — ADR-0051 holds "+
				"a retired table to a sweep that deletes its rows and a grace that outlasts the "+
				"longest read of what they name: add its domain's grace, the largest object a "+
				"row names and what sweeps the rows — a maintenance job, listed in "+
				"internal/engine.TestTheEngineSweepsEveryShortHorizonTable, or the duty whose "+
				"records delete them, held by an engine test of its own that the duty the "+
				"engine runs publishes them", ref.Table))
			continue
		}
		if strings.TrimSpace(r.sweep) == "" {
			out = append(out, fmt.Sprintf("%s is retired and names no job that sweeps its rows — "+
				"a retired row nobody deletes keeps its object for ever (ADR-0051)", ref.Table))
		}
		if r.largest <= 0 {
			out = append(out, fmt.Sprintf("%s is retired and states no largest object (%d bytes), "+
				"so nothing bounds the read its grace must outlast (ADR-0051)", ref.Table, r.largest))
			continue
		}
		if longest := objstore.ReadBudget(r.largest); r.grace <= longest+twoClocks {
			out = append(out, fmt.Sprintf("%s keeps a retired row for %v, and a read of its "+
				"largest object (%d bytes) may take objstore.ReadBudget = %v, plus a clock skew "+
				"of up to %v (twice objstore.AssumedClockSkew) between the broker stamping the "+
				"row and the node sweeping it — the object can be deleted under a read still "+
				"fetching it (ADR-0051)", ref.Table, r.grace, r.largest, longest, twoClocks))
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
// obligations ADR-0051 places on a retired declaration, which no other gate
// can see: the collector deletes an object the moment nothing names it, so a
// retired row nobody deletes keeps its object for ever, and one deleted before
// the longest read of its object ends deletes the object under that read.
//
// THE CODE HOST'S RETIRED PACKS ARE THE FIRST, and their entry is measured
// here: code.RetireGrace (24 h) against objstore.ReadBudget(code.MaxPackBytes)
// (21 h 21 min) plus the two hours the stamping broker and the sweeping node
// may disagree by (twice objstore.AssumedClockSkew), which leaves 39 min. The
// controls below show the gate fires on a retired declaration that lands
// without a reviewed entry, and on every way such an entry can be wrong —
// one only one machine's skew past the longest read included — whatever the
// shipped list holds.
func TestARetiredTableIsSweptAndOutlivesItsLongestRead(t *testing.T) {
	t.Parallel()
	retired := tracker.FileObjectReferences
	retired.Table, retired.Standing = "test_retired_objects", objstore.Retired
	// A DERIVED TABLE OWES NO ENTRY: no grace protects a reader of a
	// derived object, since its domain owes every reader a way on without it.
	derived := retired
	derived.Table, derived.Standing = "test_derived_objects", objstore.Derived
	longest := objstore.ReadBudget(tracker.MaxFileBytes)
	sound := retirement{grace: longest + twoClocks + time.Minute,
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
			with(func(r *retirement) { r.grace = longest + twoClocks }), "clock skew"},
		// ONE MACHINE'S SKEW IS NOT THE BOUND: the broker that stamped the
		// row and the node sweeping it are two clocks, each held to the
		// assumed skew of a third.
		{"a grace past one clock's skew and not two machines'", []objstore.ReferenceTable{retired},
			with(func(r *retirement) { r.grace = longest + objstore.AssumedClockSkew + time.Minute }),
			"clock skew"},
		{"a sound retirement", []objstore.ReferenceTable{retired}, with(func(*retirement) {}), ""},
		{"a derived table, which owes no grace", []objstore.ReferenceTable{derived}, nil, ""},
		{"a derived table reviewed as retired",
			[]objstore.ReferenceTable{derived},
			map[string]retirement{derived.Table: sound}, "stale"},
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
	for table, r := range retirements {
		longest := objstore.ReadBudget(r.largest)
		t.Logf("%s keeps a retired row %v: the longest read of its largest object takes %v, "+
			"and with the %v two machines' clocks may disagree by that leaves %v", table,
			r.grace, longest, twoClocks, r.grace-longest-twoClocks)
	}
}
