package artifactdata_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// reviewSheet is the motivating story's review table: an editor writes rows,
// a contributor ticks the box on a row assigned to them, the approval is a
// person's alone, and the notes only grow.
func reviewSheet() artifacts.Collection {
	five := 5.0
	return artifacts.Collection{
		Name: "review", Preset: artifacts.PresetShared,
		Columns: []artifacts.Column{
			{Key: "title", Type: artifacts.ColumnText, Required: true, Primary: true},
			{Key: "reviewer", Type: artifacts.ColumnPerson},
			{Key: "done", Type: artifacts.ColumnCheckbox, Write: artifacts.RoleContributor,
				When: artifacts.WhenAssignedToViewer, AssignedField: "reviewer"},
			{Key: "approved", Type: artifacts.ColumnCheckbox, HumansOnly: true},
			{Key: "status", Type: artifacts.ColumnStatus, Default: raw(`"todo"`), Options: []artifacts.Option{
				{ID: "todo", Label: "To review", Category: "todo"},
				{ID: "done", Label: "Done", Category: "done"},
				{ID: "old", Label: "Old", Category: "blocked", Archived: true},
			}},
			{Key: "labels", Type: artifacts.ColumnMultiSelect, Options: []artifacts.Option{
				{ID: "copy", Label: "Copy"}, {ID: "design", Label: "Design"},
			}},
			{Key: "notes", Type: artifacts.ColumnLongText},
			{Key: "log", Type: artifacts.ColumnText, Write: artifacts.RoleCommenter,
				Mode: artifacts.FieldAppend},
			{Key: "opened", Type: artifacts.ColumnDate, Immutable: true},
			{Key: "score", Type: artifacts.ColumnNumber, Max: &five},
		},
	}
}

// A WRITE LANDS, AND ITS VERDICT IS READ BACK FROM THE ROW ITS APPLY WROTE:
// the writer reports `applied` with the position of the history row, and the
// document reads back as written.
func TestTheWriterReadsItsVerdictFromItsOwnRows(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	res := n.applied(person("jane", artifacts.RoleEditor),
		set("review", "row-1", map[string]string{"title": `"Hero copy"`, "reviewer": `"bob"`}))
	if res.Seq == 0 || res.Seq != res.Result.Position.Packed() {
		t.Fatalf("the verdict names seq %d and the record landed at %d", res.Seq, res.Result.Position.Packed())
	}
	got := fields(t, n.doc("review", "row-1"))
	if got["title"] != `"Hero copy"` || got["reviewer"] != `"bob"` || got["status"] != `"todo"` {
		t.Fatalf("row-1 reads back %v", got)
	}
	// THE VERDICT IS THE ROW'S, NOT THE BROKER'S: a refused record landed
	// too, and only its history row says it changed nothing.
	refused := n.refused(person("dana", artifacts.RoleViewer), artifacts.DataForbidden,
		set("review", "row-2", map[string]string{"title": `"Not mine to add"`}))
	if refused.Result.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the refused write's record is %s, want applied: it landed and was judged", refused.Result.Outcome)
	}
	if _, err := n.reader.Doc(t.Context(), sheet, reading("dana", artifacts.RoleEditor), "review", "row-2"); !errors.Is(err, artifactdata.ErrNotFound) {
		t.Fatalf("the refused row reads back as %v, want not found", err)
	}
	if n.count(`SELECT COUNT(*) FROM artifact_data_writes WHERE verdict = 'forbidden'`) != 1 {
		t.Fatal("the refused write left no history row of its verdict")
	}
}

// A BATCH IS ALL OR NOTHING: one op the rules refuse leaves every other op of
// the batch unapplied, on every document it named.
func TestABatchAppliesWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	editor := person("jane", artifacts.RoleEditor)
	n.applied(editor, set("review", "a", map[string]string{"title": `"A"`}))
	n.refused(editor, artifacts.DataInvalid,
		merge("review", "a", map[string]string{"title": `"A changed"`}),
		set("review", "b", map[string]string{"title": `"B"`}),
		set("review", "c", map[string]string{"title": `"C"`, "status": `"nope"`}))
	if got := fields(t, n.doc("review", "a"))["title"]; got != `"A"` {
		t.Fatalf("a's title is %s after a refused batch, want it untouched", got)
	}
	if _, err := n.reader.Doc(t.Context(), sheet, reading("jane", artifacts.RoleEditor), "review", "b"); !errors.Is(err, artifactdata.ErrNotFound) {
		t.Fatalf("b exists after a refused batch: %v", err)
	}
}

// A GUARDED WRITE IS A CONFLICT ONCE ITS DOCUMENT HAS MOVED, and the answer
// carries the documents as they stand, which is everything a merge needs.
func TestAMovedGuardIsAConflictCarryingTheCurrentDocuments(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	editor := person("jane", artifacts.RoleEditor)
	first := n.applied(editor, set("tasks", "t1", map[string]string{"title": `"One"`}))
	n.applied(editor, merge("tasks", "t1", map[string]string{"title": `"One, revised"`}))
	res := n.refused(editor, artifacts.DataConflict,
		guarded(merge("tasks", "t1", map[string]string{"title": `"Stale edit"`}), first.Seq))
	if len(res.Conflict) != 1 || res.Conflict[0].ID != "t1" ||
		!strings.Contains(string(res.Conflict[0].Fields), "One, revised") {
		t.Fatalf("the conflict carries %+v, want t1 as it stands", res.Conflict)
	}
	// THE CURRENT VERSION IS THE GUARD THAT WINS.
	n.applied(editor, guarded(merge("tasks", "t1", map[string]string{"title": `"Merged"`}),
		res.Conflict[0].Version))
}

// LONG TEXT IS A GUARDED WHOLE-FIELD WRITE PLUS APPEND (D18): replacing it
// blind is refused, a guarded replace and an append both land.
func TestLongTextIsWrittenGuardedOrAppended(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	editor := person("jane", artifacts.RoleEditor)
	created := n.applied(editor, set("review", "r", map[string]string{"title": `"R"`, "notes": `"Draft."`}))
	n.refused(editor, artifacts.DataInvalid, merge("review", "r", map[string]string{"notes": `"Blind."`}))
	n.applied(editor, field(artifacts.DataAppend, "review", "r", "notes", `" More."`))
	n.refused(editor, artifacts.DataConflict,
		guarded(merge("review", "r", map[string]string{"notes": `"Rewritten."`}), created.Seq))
	current := n.doc("review", "r")
	n.applied(editor, guarded(merge("review", "r", map[string]string{"notes": `"Rewritten."`}), current.Version))
	if got := fields(t, n.doc("review", "r"))["notes"]; got != `"Rewritten."` {
		t.Fatalf("notes = %s", got)
	}
}

// THE APPLIER DECIDES FROM THE STAMPED AUTHORITY ALONE: the same op is admitted
// or refused by the role and the actor kind the record carries, with nothing
// read from another domain, so a stamp is the whole of who may write.
func TestTheApplierDecidesFromTheStampedAuthorityAlone(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	n.applied(person("jane", artifacts.RoleEditor),
		set("review", "r1", map[string]string{"title": `"Assigned to bob"`, "reviewer": `"bob"`}),
		set("review", "r2", map[string]string{"title": `"Assigned to sam"`, "reviewer": `"sam"`}))

	// A CONTRIBUTOR ticks the box on the row assigned to them, and on no
	// other; they write no other column of a row they did not create.
	bob := person("bob", artifacts.RoleContributor)
	n.applied(bob, merge("review", "r1", map[string]string{"done": `true`}))
	n.refused(bob, artifacts.DataForbidden, merge("review", "r2", map[string]string{"done": `true`}))
	n.refused(bob, artifacts.DataForbidden, merge("review", "r1", map[string]string{"title": `"Mine now"`}))
	// THE SAME SEAT STAMPED A VIEWER is refused the box it ticked a
	// moment ago: the role is the stamp's, never re-read.
	n.refused(person("bob", artifacts.RoleViewer), artifacts.DataForbidden,
		merge("review", "r1", map[string]string{"done": `false`}))

	// HUMANS ONLY: an agent at owner is refused the approval a person at
	// editor makes.
	n.refused(agent("agent-iris", artifacts.RoleOwner), artifacts.DataForbidden,
		merge("review", "r1", map[string]string{"approved": `true`}))
	n.applied(person("jane", artifacts.RoleEditor), merge("review", "r1", map[string]string{"approved": `true`}))

	// APPEND ONLY: a commenter grows the log and never replaces it.
	dana := person("dana", artifacts.RoleCommenter)
	n.applied(dana, field(artifacts.DataAppend, "review", "r1", "log", `"dana: looks good. "`))
	n.refused(dana, artifacts.DataForbidden, merge("review", "r1", map[string]string{"log": `"erased"`}))

	// IMMUTABLE: set when a row is created, never changed after.
	editor := person("jane", artifacts.RoleEditor)
	n.applied(editor, set("review", "r3", map[string]string{"title": `"T"`, "opened": `"2031-04-07"`}))
	n.refused(editor, artifacts.DataForbidden, merge("review", "r3", map[string]string{"opened": `"2031-05-01"`}))
}

// THE COLUMN TYPES HOLD THEIR VALUES: an archived option, an unknown field, a
// value past a bound and a person who is not a seat are each refused.
func TestEveryColumnTypeHoldsItsValues(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	editor := person("jane", artifacts.RoleEditor)
	n.applied(editor, set("review", "ok", map[string]string{
		"title": `"T"`, "status": `"done"`, "labels": `["copy","design"]`, "score": `4.5`,
		"opened": `"2031-04-07T09:00:00Z"`,
	}))
	for name, f := range map[string]map[string]string{
		"an archived option":         {"title": `"T"`, "status": `"old"`},
		"an option nobody offers":    {"title": `"T"`, "labels": `["copy","nope"]`},
		"a chip chosen twice":        {"title": `"T"`, "labels": `["copy","copy"]`},
		"a field nobody declared":    {"title": `"T"`, "typo": `"x"`},
		"a number past its maximum":  {"title": `"T"`, "score": `9`},
		"a multi-line title":         {"title": `"two\nlines"`},
		"a date out of range":        {"title": `"T"`, "opened": `"1800-01-01"`},
		"a checkbox that is no bool": {"title": `"T"`, "done": `"yes"`},
		"a missing required field":   {"status": `"done"`},
	} {
		res := n.write(editor, set("review", "bad", f))
		if res.Verdict != artifacts.DataInvalid {
			t.Errorf("%s: verdict %s (%s), want invalid", name, res.Verdict, res.Reason)
		}
	}
	// A PERSON VALUE NAMING NO SEAT is refused before it is appended.
	_, err := n.writer.Write(t.Context(), editor, sheet, artifacts.DataWrite{Ops: []artifacts.DataOp{
		set("review", "who", map[string]string{"title": `"T"`, "reviewer": `"nobody"`}),
	}})
	if !errors.Is(err, artifactdata.ErrInvalid) {
		t.Fatalf("a person who is not a seat: %v, want ErrInvalid", err)
	}
}

// AN INCREMENT IS EXACT, ON DECIMAL TEXT: a float would make 0.1 + 0.2 a
// counter's value for ever.
func TestAnIncrementIsExactDecimalArithmetic(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	editor := person("jane", artifacts.RoleEditor)
	n.applied(editor, field(artifacts.DataIncrement, "votes", "poll", "total", `0.1`))
	n.applied(editor, field(artifacts.DataIncrement, "votes", "poll", "total", `0.2`))
	n.applied(editor, field(artifacts.DataIncrement, "votes", "poll", "big", `9007199254740993`),
		field(artifacts.DataIncrement, "votes", "poll", "big", `1`))
	got := fields(t, n.doc("votes", "poll"))
	if got["total"] != "0.3" || got["big"] != "9007199254740994" {
		t.Fatalf("the counters read %v, want total 0.3 and big 9007199254740994", got)
	}
}

// A SET'S CHIPS COMMUTE: two people adding different chips at once both land,
// and removing one leaves the other.
func TestAddingAndRemovingChipsNeverLosesAnotherWritersChip(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	editor := person("jane", artifacts.RoleEditor)
	n.applied(editor, set("review", "r", map[string]string{"title": `"R"`}))
	n.applied(editor, field(artifacts.DataAdd, "review", "r", "labels", `"copy"`))
	n.applied(person("sam", artifacts.RoleEditor), field(artifacts.DataAdd, "review", "r", "labels", `"design"`))
	n.applied(editor, field(artifacts.DataAdd, "review", "r", "labels", `"copy"`))
	if got := fields(t, n.doc("review", "r"))["labels"]; got != `["copy","design"]` {
		t.Fatalf("labels = %s", got)
	}
	n.applied(editor, field(artifacts.DataRemove, "review", "r", "labels", `"copy"`))
	if got := fields(t, n.doc("review", "r"))["labels"]; got != `["design"]` {
		t.Fatalf("labels after a removal = %s", got)
	}
}

// A CONTRIBUTE COLLECTION IS A SIGN-UP SHEET: a viewer adds a row and changes
// their own, never somebody else's, and keeps one row when the collection
// says so.
func TestAContributeCollectionKeepsEachRowItsWriters(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), artifacts.Collection{
		Name: "signups", Preset: artifacts.PresetContribute, OncePerPrincipal: true,
	})
	bob, dana := person("bob", artifacts.RoleViewer), person("dana", artifacts.RoleViewer)
	n.applied(bob, set("signups", "bob-row", map[string]string{"name": `"Bob"`}))
	n.refused(bob, artifacts.DataForbidden, set("signups", "bob-again", map[string]string{"name": `"Bob"`}))
	n.refused(dana, artifacts.DataForbidden, merge("signups", "bob-row", map[string]string{"name": `"Dana"`}))
	n.refused(dana, artifacts.DataForbidden, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "signups", Doc: "bob-row"})
	n.applied(bob, merge("signups", "bob-row", map[string]string{"name": `"Robert"`}))
	// DELETING ONE'S OWN ROW AND ADDING ANOTHER IN ONE BATCH keeps one.
	n.applied(bob, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "signups", Doc: "bob-row"},
		set("signups", "bob-new", map[string]string{"name": `"Bob"`}))
	// AN EDITOR changes anybody's row.
	n.applied(person("jane", artifacts.RoleEditor), merge("signups", "bob-new", map[string]string{"name": `"Bob B."`}))
}

// THE STAMPED LIMITS BOUND A STORE'S REAL FOOTPRINT, and a write that frees
// bytes always lands so a full store can be emptied.
func TestTheStampedLimitBoundsTheStoresFootprint(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	tight := person("jane", artifacts.RoleEditor)
	tight.Authority.Limits.DataStoreBytes = 64 << 10
	big := `"` + strings.Repeat("x", 3000) + `"`
	landed := 0
	for i := range 40 {
		res := n.write(tight, set("notes", "n"+string(rune('a'+i%26))+string(rune('a'+i/26)),
			map[string]string{"body": big}))
		if res.Verdict == artifacts.DataQuota {
			break
		}
		if res.Verdict != artifacts.DataApplied {
			t.Fatalf("write %d: %s (%s)", i, res.Verdict, res.Reason)
		}
		landed++
	}
	if landed == 0 || landed == 40 {
		t.Fatalf("%d of 40 writes of 3 KB landed under a 64 KiB footprint: the limit bound nothing", landed)
	}
	info, err := n.reader.Store(t.Context(), sheet)
	if err != nil {
		t.Fatal(err)
	}
	if info.Footprint > tight.Authority.Limits.DataStoreBytes {
		t.Fatalf("the footprint is %d past the stamped %d", info.Footprint, tight.Authority.Limits.DataStoreBytes)
	}
	if want := info.BodyBytes + 48*info.FieldEntries + info.HistoryBytes; info.Footprint != want {
		t.Fatalf("the footprint is %d, and its parts sum to %d", info.Footprint, want)
	}
	n.applied(tight, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "notes", Doc: "naa"})
}

// THE PER-VIEWER STATE IS THE VIEWER'S ALONE (D15): a key one viewer keeps is
// not another's, and writing into somebody else's state is refused.
func TestPerViewerStateIsEachViewersOwn(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	bobKey, err := artifactdata.StateDocID("bob", "theme")
	if err != nil {
		t.Fatal(err)
	}
	danaKey, _ := artifactdata.StateDocID("dana", "theme")
	n.applied(person("bob", artifacts.RoleViewer), set(artifactdata.StateCollection, bobKey,
		map[string]string{artifactdata.StateField: `"dark"`}))
	n.applied(person("dana", artifacts.RoleViewer), set(artifactdata.StateCollection, danaKey,
		map[string]string{artifactdata.StateField: `"light"`}))
	n.refused(person("dana", artifacts.RoleOwner), artifacts.DataForbidden,
		set(artifactdata.StateCollection, bobKey, map[string]string{artifactdata.StateField: `"pink"`}))
	got, err := n.reader.State(t.Context(), sheet, reading("bob", artifacts.RoleViewer))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "theme" || string(got[0].Value) != `"dark"` {
		t.Fatalf("bob's state reads %+v", got)
	}
	schema, err := n.reader.Schema(t.Context(), sheet)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range schema {
		if c.Name == artifactdata.StateCollection {
			t.Fatal("the schema lists the engine's state collection")
		}
	}
}

// A WRITE NAMING SOMETHING NO STORE HOLDS IS REFUSED BEFORE IT IS APPENDED:
// nothing reaches the log.
func TestAWriteOutsideTheGrammarNeverReachesTheLog(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	editor := person("jane", artifacts.RoleEditor)
	for name, op := range map[string]artifacts.DataOp{
		"a reserved collection": set("_hidden", "a", map[string]string{"x": `1`}),
		"a route's own word":    set("schema", "a", map[string]string{"x": `1`}),
		"an upper-case name":    set("Tasks", "a", map[string]string{"x": `1`}),
		"a document id slash":   set("tasks", "a/b", map[string]string{"x": `1`}),
		"an engine field":       set("tasks", "a", map[string]string{"_meta": `1`}),
		"an op nobody applies":  {Op: "rename", Collection: "tasks", Doc: "a"},
	} {
		_, err := n.writer.Write(t.Context(), editor, sheet, artifacts.DataWrite{Ops: []artifacts.DataOp{op}})
		if !errors.Is(err, artifactdata.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if end, err := n.log.End(t.Context()); err != nil || end != 0 {
		t.Fatalf("the log ends at %d (%v): a refused write was appended", end, err)
	}
	ops := make([]artifacts.DataOp, artifacts.MaxBatchOps+1)
	for i := range ops {
		ops[i] = set("tasks", "a", map[string]string{"x": `1`})
	}
	if _, err := n.writer.Write(t.Context(), editor, sheet, artifacts.DataWrite{Ops: ops}); !errors.Is(err, artifactdata.ErrInvalid) {
		t.Fatalf("a batch of %d ops: %v, want ErrInvalid", len(ops), err)
	}
}

// THE WRITE BUDGET IS WEIGHED BY COST AND ANSWERS WHEN TO COME BACK: a client
// looping on one store is refused with a Retry-After, never locked out.
func TestTheWriteBudgetIsWeighedAndSaysWhenToComeBack(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	editor := person("jane", artifacts.RoleEditor)
	wide := map[string]string{}
	for i := range 40 {
		wide["f"+string(rune('a'+i%26))+string(rune('a'+i/26))] = `1`
	}
	var limited *artifactdata.RateLimited
	for i := range 200 {
		_, err := n.writer.Write(t.Context(), editor, sheet, artifacts.DataWrite{Ops: []artifacts.DataOp{
			set("wide", "w", wide),
		}})
		if errors.As(err, &limited) {
			if i == 0 {
				t.Fatal("the first write was refused")
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if limited == nil {
		t.Fatal("200 writes of 40 fields each at one instant were never limited")
	}
	if limited.RetryAfter <= 0 || limited.Scope != "principal" {
		t.Fatalf("the refusal is %+v, want the principal's bucket and a wait", limited)
	}
	// ANOTHER PRINCIPAL is not held by the first one's budget.
	if _, err := n.writer.Write(t.Context(), person("sam", artifacts.RoleEditor), sheet,
		artifacts.DataWrite{Ops: []artifacts.DataOp{set("wide", "s", map[string]string{"a": `1`})}}); err != nil {
		t.Fatalf("another principal's write: %v", err)
	}
	if got := artifactdata.Cost([]artifacts.DataOp{set("wide", "w", wide)}); got != 3 {
		t.Fatalf("a 40-field set costs %d, want 3 units of 16 rows", got)
	}
}
