package artifactdata_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN UNDO RESTORES WHAT ONE WRITE CHANGED, and only while nobody has changed
// it since: a teammate's later edit makes the undo a conflict rather than
// silently reverting their decision. A delete undone brings the row back with
// its creator.
func TestAnUndoRevertsOnlyWhatNobodyChangedSince(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, sam := person("jane", artifacts.RoleEditor), person("sam", artifacts.RoleEditor)
	n.applied(jane, set("tasks", "t", map[string]string{"title": `"Before"`, "keep": `1`}))
	edit := n.applied(sam, merge("tasks", "t", map[string]string{"title": `"After"`, "added": `true`}))
	res, err := n.writer.Undo(t.Context(), sam, sheet, edit.Seq)
	if err != nil || res.Verdict != artifacts.DataApplied {
		t.Fatalf("undo sam's edit: %+v, %v", res, err)
	}
	if got := fields(t, n.doc("tasks", "t")); got["title"] != `"Before"` || got["added"] != "" || got["keep"] != "1" {
		t.Fatalf("after the undo the row reads %v", got)
	}
	// A LATER EDIT BY SOMEBODY ELSE MAKES THE UNDO A CONFLICT.
	second := n.applied(sam, merge("tasks", "t", map[string]string{"title": `"Sam's"`}))
	n.applied(jane, merge("tasks", "t", map[string]string{"title": `"Jane's"`}))
	res, err = n.writer.Undo(t.Context(), sam, sheet, second.Seq)
	if err != nil || res.Verdict != artifacts.DataConflict {
		t.Fatalf("undoing a write somebody built on: %+v, %v; want a conflict", res, err)
	}
	if got := fields(t, n.doc("tasks", "t"))["title"]; got != `"Jane's"` {
		t.Fatalf("the refused undo reverted jane's edit: title %s", got)
	}
	// ONLY THE AUTHOR OR AN EDITOR UNDOES A WRITE.
	bobRow := n.applied(person("bob", artifacts.RoleEditor), set("tasks", "b", map[string]string{"x": `1`}))
	if _, err := n.writer.Undo(t.Context(), person("dana", artifacts.RoleContributor), sheet, bobRow.Seq); !errors.Is(err, artifactdata.ErrForbidden) {
		t.Fatalf("a contributor undoing bob's write: %v, want ErrForbidden", err)
	}
	// A DELETE UNDONE brings the row back as its creator's.
	gone := n.applied(jane, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "tasks", Doc: "b"})
	if res, err := n.writer.Undo(t.Context(), jane, sheet, gone.Seq); err != nil || res.Verdict != artifacts.DataApplied {
		t.Fatalf("undo a delete: %+v, %v", res, err)
	}
	if d := n.doc("tasks", "b"); d.CreatedBy.Name != "bob" {
		t.Fatalf("the restored row's creator is %q, want bob", d.CreatedBy.Name)
	}
	// A REFUSED WRITE AND A SCHEMA CHANGE ARE NOT UNDONE.
	refused := n.refused(person("dana", artifacts.RoleViewer), artifacts.DataForbidden,
		set("tasks", "d", map[string]string{"x": `1`}))
	if _, err := n.writer.Undo(t.Context(), jane, sheet, refused.Seq); !errors.Is(err, artifactdata.ErrNotUndoable) {
		t.Fatalf("undo a refused write: %v, want ErrNotUndoable", err)
	}
}

// A WRITE IS MARKED UNDOABLE ONLY WHEN ITS UNDO FITS ONE WRITE: every value a
// history row keeps is bounded, but undoing a bulk delete restores every field
// of every row, so the inverse of deleting a hundred rows of twelve KiB is
// larger than a write carries. The history says so, and the undo is refused as
// not undoable before anything reaches the log, never as a record the log
// refuses.
func TestAnUndoLargerThanOneWriteIsNotOffered(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.clock.stride(time.Minute)
	jane := person("jane", artifacts.RoleEditor)
	chunk := `"` + strings.Repeat("n", 3900) + `"`
	var deletes []artifacts.DataOp
	for batch := range 4 {
		var ops []artifacts.DataOp
		for i := range artifacts.MaxBatchOps / 4 {
			id := fmt.Sprintf("r%03d", batch*artifacts.MaxBatchOps/4+i)
			ops = append(ops, set("notes", id, map[string]string{"a": chunk, "b": chunk, "c": chunk}))
			deletes = append(deletes, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "notes", Doc: id})
		}
		n.applied(jane, ops...)
	}
	small := n.applied(jane, deletes[0])
	bulk := n.applied(jane, deletes[1:]...)
	page, err := n.reader.History(t.Context(), sheet, reading("jane", artifacts.RoleEditor),
		artifactdata.HistoryFilter{Collection: "notes"}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	undoable := map[int64]bool{}
	for _, e := range page.Entries {
		undoable[e.Seq] = e.Undoable
	}
	if !undoable[small.Seq] {
		t.Fatal("deleting one row is not offered as undoable")
	}
	if undoable[bulk.Seq] {
		t.Fatal("deleting 99 rows of twelve KiB is offered as undoable, and its undo cannot fit one write")
	}
	_, err = n.writer.Undo(t.Context(), jane, sheet, bulk.Seq)
	if !errors.Is(err, artifactdata.ErrNotUndoable) || errors.Is(err, artifactdata.ErrInvalid) ||
		!strings.Contains(err.Error(), "more than one write carries") {
		t.Fatalf("undoing the bulk delete: %v, want ErrNotUndoable naming the write bound", err)
	}
	if res, err := n.writer.Undo(t.Context(), jane, sheet, small.Seq); err != nil || res.Verdict != artifacts.DataApplied {
		t.Fatalf("undoing the one-row delete: %+v, %v", res, err)
	}
}

// A SCHEMA CHANGE MIGRATES WHAT IT RETYPES AND REMOVES, BOUNDED: a removed
// column's values go, a retyped column's values are coerced (and cleared where
// they cannot be held), and a change dropping an option or retyping without a
// coercion is refused.
func TestASchemaChangeMigratesItsDocumentsWithinItsBound(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	n.configure(jane, reviewSheet())
	n.applied(jane, set("review", "a", map[string]string{"title": `"A"`, "status": `"done"`, "score": `3`}),
		set("review", "b", map[string]string{"title": `"B"`, "score": `2.5`, "notes": `"N"`}))
	next := reviewSheet()
	var cols []artifacts.Column
	for _, col := range next.Columns {
		switch col.Key {
		case "notes":
			continue // removed
		case "score":
			col.Type, col.Max = artifacts.ColumnText, nil // retyped: number to text
		case "status":
			col.Type = artifacts.ColumnMultiSelect // retyped: wrapped
			col.Default = nil
			for i := range col.Options {
				col.Options[i].Category = ""
			}
		}
		cols = append(cols, col)
	}
	next.Columns = cols
	n.configure(jane, next)
	a, b := fields(t, n.doc("review", "a")), fields(t, n.doc("review", "b"))
	if a["score"] != `"3"` || a["status"] != `["done"]` {
		t.Fatalf("row a after the migration: %v", a)
	}
	if _, held := b["notes"]; held || b["score"] != `"2.5"` {
		t.Fatalf("row b after the migration: %v", b)
	}
	// A DROPPED OPTION is refused: archive it instead.
	dropped := next
	dropped.Columns = append([]artifacts.Column(nil), next.Columns...)
	for i, col := range dropped.Columns {
		if col.Key == "labels" {
			col.Options = col.Options[:1]
			dropped.Columns[i] = col
		}
	}
	if _, err := n.writer.Configure(t.Context(), jane, sheet, []artifacts.Collection{dropped}); !errors.Is(err, artifactdata.ErrInvalid) {
		t.Fatalf("dropping an option: %v, want ErrInvalid", err)
	}
	// A RETYPE WITH NO COERCION is refused.
	bad := next
	bad.Columns = append([]artifacts.Column(nil), next.Columns...)
	for i, col := range bad.Columns {
		if col.Key == "done" {
			col.Type, col.Write, col.When, col.AssignedField = artifacts.ColumnDate, "", "", ""
			bad.Columns[i] = col
		}
	}
	if _, err := n.writer.Configure(t.Context(), jane, sheet, []artifacts.Collection{bad}); !errors.Is(err, artifactdata.ErrInvalid) {
		t.Fatalf("retyping a checkbox to a date: %v, want ErrInvalid", err)
	}
	// A COLLECTION HOLDING ROWS is not dropped.
	if _, err := n.writer.Configure(t.Context(), jane, sheet, nil); !errors.Is(err, artifactdata.ErrInvalid) {
		t.Fatalf("dropping a collection with rows: %v, want ErrInvalid", err)
	}
	computed, stored := n.digestFromRows(sheet)
	if computed != stored {
		t.Fatal("the digest after a migration is not the sum of the rows")
	}
}

// A SCHEMA CHANGE REACHES A ROW WHOSE LIST IS EMPTY: removing the last chip
// leaves `[]`, which indexes no scalar, and the change still finds the row
// (through its presence marker), so a removed column leaves its body and a
// retyped one is cleared where its new type cannot hold it. Were it missed,
// the row would keep a field its schema no longer declares, and every
// whole-row write of it would be refused as naming that field.
func TestASchemaChangeReachesARowHoldingAnEmptyList(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	options := []artifacts.Option{{ID: "copy", Label: "Copy"}, {ID: "design", Label: "Design"}}
	spec := artifacts.Collection{Name: "chips", Preset: artifacts.PresetShared, Columns: []artifacts.Column{
		{Key: "title", Type: artifacts.ColumnText},
		{Key: "tags", Type: artifacts.ColumnMultiSelect, Options: options},
		{Key: "topics", Type: artifacts.ColumnMultiSelect, Options: options},
	}}
	n.configure(jane, spec)
	n.applied(jane, set("chips", "r1", map[string]string{"title": `"One"`, "tags": `["copy"]`,
		"topics": `["design"]`}))
	n.applied(jane, field(artifacts.DataRemove, "chips", "r1", "tags", `"copy"`),
		field(artifacts.DataRemove, "chips", "r1", "topics", `"design"`))
	if got := fields(t, n.doc("chips", "r1")); got["tags"] != `[]` || got["topics"] != `[]` {
		t.Fatalf("after removing the last chips the row reads %v, want both lists empty", got)
	}
	next := spec
	next.Columns = []artifacts.Column{
		{Key: "title", Type: artifacts.ColumnText},
		{Key: "topics", Type: artifacts.ColumnSelect, Options: options},
	}
	n.configure(jane, next)
	d := n.doc("chips", "r1")
	if got := fields(t, d); len(got) != 1 || got["title"] != `"One"` {
		t.Fatalf("after removing tags and retyping topics the row reads %v, want only its title", got)
	}
	if len(d.Invalid) != 0 {
		t.Fatalf("the migrated row still holds fields its schema refuses: %v", d.Invalid)
	}
	n.applied(jane, set("chips", "r1", map[string]string{"title": `"Renamed"`}))
	if got := fields(t, n.doc("chips", "r1")); got["title"] != `"Renamed"` {
		t.Fatalf("a whole-row write after the migration left %v", got)
	}
	computed, stored := n.digestFromRows(sheet)
	if computed != stored {
		t.Fatal("the digest after the migration is not the sum of the rows")
	}
}

// A MIGRATION PAST ONE RECORD'S BOUND IS REFUSED, naming the bound, before it
// is appended (review A7): an apply holds the estate's only writer.
func TestAMigrationPastItsBoundIsRefused(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.clock.stride(10 * time.Second)
	jane := person("jane", artifacts.RoleOwner)
	spec := artifacts.Collection{Name: "bulk", Preset: artifacts.PresetShared, Columns: []artifacts.Column{
		{Key: "n", Type: artifacts.ColumnNumber}, {Key: "gone", Type: artifacts.ColumnText},
	}}
	n.configure(jane, spec)
	for batch := range (artifacts.MaxMigrationDocs + 1 + artifacts.MaxBatchOps - 1) / artifacts.MaxBatchOps {
		var ops []artifacts.DataOp
		for i := range artifacts.MaxBatchOps {
			id := batch*artifacts.MaxBatchOps + i
			if id > artifacts.MaxMigrationDocs {
				break
			}
			ops = append(ops, set("bulk", fmt.Sprintf("d%05d", id), map[string]string{"n": `1`, "gone": `"x"`}))
		}
		if len(ops) > 0 {
			n.applied(jane, ops...)
		}
	}
	trimmed := spec
	trimmed.Columns = spec.Columns[:1]
	if _, err := n.writer.Configure(t.Context(), jane, sheet, []artifacts.Collection{trimmed}); !errors.Is(err, artifactdata.ErrMigrationTooLarge) {
		t.Fatalf("a change rewriting %d documents: %v, want ErrMigrationTooLarge",
			artifacts.MaxMigrationDocs+1, err)
	}
}

// AN AGENT TIGHTENS A SCHEMA AND NEVER LOOSENS ONE (03 section 4.2): it may add
// `humans_only` and never remove it, nor drop the protected column to declare
// it again without.
func TestAnAgentNeverLoosensASchema(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	iris := agent("agent-iris", artifacts.RoleOwner)
	tighter := reviewSheet()
	for i, col := range tighter.Columns {
		if col.Key == "score" {
			col.HumansOnly = true
			tighter.Columns[i] = col
		}
	}
	n.configure(iris, tighter)
	for name, change := range map[string]func(c *artifacts.Collection){
		"lifting humans only": func(c *artifacts.Collection) {
			for i, col := range c.Columns {
				if col.Key == "approved" {
					col.HumansOnly = false
					c.Columns[i] = col
				}
			}
		},
		"dropping the approval column": func(c *artifacts.Collection) {
			var cols []artifacts.Column
			for _, col := range c.Columns {
				if col.Key != "approved" {
					cols = append(cols, col)
				}
			}
			c.Columns = cols
		},
		"opening the collection to contributors": func(c *artifacts.Collection) {
			c.Preset = artifacts.PresetContribute
		},
		// THE ASSIGNEE REPOINTED: `done` admits whoever `reviewer` names;
		// pointing it at a person column any contributor fills hands the
		// write to anybody who puts their own handle there.
		"repointing the assignee of an assigned column": func(c *artifacts.Collection) {
			c.Columns = append(c.Columns, artifacts.Column{Key: "helper",
				Type: artifacts.ColumnPerson, Write: artifacts.RoleContributor})
			for i, col := range c.Columns {
				if col.Key == "done" {
					col.AssignedField = "helper"
					c.Columns[i] = col
				}
			}
		},
	} {
		loose := tighter
		loose.Columns = append([]artifacts.Column(nil), tighter.Columns...)
		change(&loose)
		if _, err := n.writer.Configure(t.Context(), iris, sheet, []artifacts.Collection{loose}); !errors.Is(err, artifactdata.ErrForbidden) {
			t.Errorf("an agent %s: %v, want ErrForbidden", name, err)
		}
		// A PERSON MAY.
		n.configure(person("jane", artifacts.RoleOwner), loose)
		n.configure(person("jane", artifacts.RoleOwner), tighter)
	}
}

// A PURGE REMOVES THE STORE FOR GOOD, and a write about it that arrives later
// produces no rows on any node: the writer is told the store is gone.
func TestAPurgedStoreStaysPurged(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	n.applied(jane, set("tasks", "t", map[string]string{"x": `1`}))
	if err := n.writer.PurgeArtifactState(t.Context(), jane, statelog.StepOpID(statelog.NewOpID(monday, "purge"), "dependent-0"), sheet); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, table := range []string{"artifact_data_stores", "artifact_data_docs", "artifact_data_fields",
		"artifact_data_writes", "artifact_data_collections", "artifact_data_touches", "artifact_data_paths"} {
		if c := n.count(`SELECT COUNT(*) FROM ` + table); c != 0 {
			t.Errorf("%s holds %d rows after the purge", table, c)
		}
	}
	if fp := n.count(`SELECT footprint FROM artifact_data_usage`); fp != 0 {
		t.Errorf("the company's footprint is %d after its only store was purged", fp)
	}
	if _, err := n.writer.Write(t.Context(), jane, sheet, artifacts.DataWrite{Ops: []artifacts.DataOp{
		set("tasks", "t", map[string]string{"x": `2`})}}); !errors.Is(err, artifactdata.ErrGone) {
		t.Fatalf("a write after the purge: %v, want ErrGone", err)
	}
	if _, err := n.reader.Docs(t.Context(), sheet, reading("jane", artifacts.RoleOwner), "tasks", 0, "", 0); !errors.Is(err, artifactdata.ErrGone) {
		t.Fatalf("a read after the purge: %v, want ErrGone", err)
	}
	// A SECOND PURGE STEP, a retry of the artifact's purge, is a no-op.
	if err := n.writer.PurgeArtifactState(t.Context(), jane, "another-step", sheet); err != nil {
		t.Fatalf("a second purge: %v", err)
	}
	// A WRITE RECORD THAT RACED THE PURGE is gated on every data node: applied
	// after the marker, it writes nothing.
	n.holdApplier()
	late, err := artifactdata.Encode(artifactdata.MutationRecord{
		RecordEnvelope: artifactdata.RecordEnvelope{OpID: "late-write", Subject: artifactdata.StoreSubject(sheet),
			Op: artifactdata.OpWrite, Scope: artifactdata.ScopeSet{Subject: true}},
		Mutation: mustJSON(t, artifactdata.WritePayload{V: 1, Authority: jane.Authority,
			Write: artifacts.DataWrite{Ops: []artifacts.DataOp{set("tasks", "t", map[string]string{"x": `3`})}}}),
		Actor: "jane", ActorKind: jane.Authority.Actor.Kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	n.applyBody(artifactdata.Domain{}.Stream(), 999, late, monday)
	if c := n.count(`SELECT COUNT(*) FROM artifact_data_docs`); c != 0 {
		t.Fatalf("a write after the purge recreated %d documents", c)
	}
	if c := n.count(`SELECT rejects FROM artifactdata_deletions`); c != 1 {
		t.Fatalf("the marker counted %d rejects, want 1", c)
	}
}

// mustJSON is v's JSON.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// THE HOT HISTORY KEEPS SEVEN DAYS AND ITS BYTE BOUND, pruned from broker time
// in each record's own transaction; a delete older than the floor is
// forgotten, and a client catching up from below it is told to read whole.
func TestTheHotHistoryKeepsItsWindowAndTellsAStaleClientToResync(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleEditor)
	// EVERY RECORD UP TO THE FOURTH IS A WEEK AND A DAY OLD by broker time.
	week := artifacts.DataHistoryHotWindow + 24*time.Hour
	n.storedAt = func(seq uint64, broker time.Time) time.Time {
		if seq <= 4 {
			return broker.Add(-week)
		}
		return broker
	}
	n.applied(jane, set("tasks", "a", map[string]string{"x": `1`}))
	n.applied(jane, set("tasks", "b", map[string]string{"x": `1`}))
	old := n.applied(jane, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "tasks", Doc: "b"})
	n.applied(jane, merge("tasks", "a", map[string]string{"x": `2`}))
	n.applied(jane, merge("tasks", "a", map[string]string{"x": `3`}))
	if c := n.count(`SELECT COUNT(*) FROM artifact_data_writes`); c != 1 {
		t.Fatalf("%d history rows after a write a week past four others, want only the new one", c)
	}
	if c := n.count(`SELECT COUNT(*) FROM artifact_data_docs WHERE body IS NULL`); c != 0 {
		t.Fatalf("%d tombstones survive past the history floor", c)
	}
	if sum, held := n.count(`SELECT COALESCE(SUM(metered), 0) FROM artifact_data_docs`),
		n.count(`SELECT doc_bytes FROM artifact_data_stores`); sum != held {
		t.Fatalf("the rows left meter %d bytes and the store still counts %d: a pruned "+
			"tombstone kept its bytes", sum, held)
	}
	page, err := n.reader.Docs(t.Context(), sheet, reading("jane", artifacts.RoleEditor), "tasks", old.Seq-1, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Resync {
		t.Fatal("a client catching up from below the forgotten delete was not told to read whole")
	}
	info, err := n.reader.Store(t.Context(), sheet)
	if err != nil {
		t.Fatal(err)
	}
	if info.HistoryFloor == 0 || info.TombstonesFloor < old.Seq {
		t.Fatalf("the floors are %d and %d after the prune", info.HistoryFloor, info.TombstonesFloor)
	}
	// AND THE BYTE BOUND: a store stamped small keeps a quarter of it.
	small := person("jane", artifacts.RoleEditor)
	small.Authority.Limits.DataStoreBytes = 1 << 20
	n.storedAt = nil
	big := `"` + strings.Repeat("y", 2000) + `"`
	for i := range 300 {
		n.applied(small, merge("tasks", "a", map[string]string{"note": big, "i": fmt.Sprint(i)}))
	}
	if hb := n.count(`SELECT history_bytes FROM artifact_data_stores`); hb > (1<<20)/4+8<<10 {
		t.Fatalf("history holds %d bytes past a quarter of the stamped 1 MiB", hb)
	}
	if sum := n.count(`SELECT SUM(bytes) FROM artifact_data_writes`); sum != n.count(`SELECT history_bytes FROM artifact_data_stores`) {
		t.Fatalf("the history rows sum to %d and the store counts %d", sum,
			n.count(`SELECT history_bytes FROM artifact_data_stores`))
	}
}

// WHAT A BATCH MOVED IS TOLD AFTER THE COMMIT, from the rows it wrote: each
// document with the facts a push is shaped by, and every record's verdict.
func TestWhatABatchMovedIsToldWithItsShapingFacts(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), artifacts.Collection{Name: "votes", Preset: artifacts.PresetPrivate})
	n.moved = nil
	n.applied(person("bob", artifacts.RoleViewer), set("votes", "bob-vote", map[string]string{"choice": `"a"`}))
	n.refused(person("dana", artifacts.RoleViewer), artifacts.DataForbidden,
		merge("votes", "bob-vote", map[string]string{"choice": `"b"`}))
	if len(n.moved) != 2 {
		t.Fatalf("%d movements, want one per record", len(n.moved))
	}
	first := n.moved[0]
	if first.Artifact != sheet || len(first.Docs) != 1 || first.Docs[0].Read != artifacts.ReadPrivate ||
		first.Docs[0].CreatedBy != "bob" || !strings.Contains(string(first.Docs[0].Body), `"a"`) {
		t.Fatalf("the first movement is %+v", first)
	}
	if len(n.moved[1].Docs) != 0 || n.moved[1].Writes[0].Verdict != artifacts.DataForbidden {
		t.Fatalf("the refused write moved %+v", n.moved[1])
	}
}
