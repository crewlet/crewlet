package worklet_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Two atlas entries by id, and a third no task names.
const (
	entryDB   = "7f3c2a10-5b1e-5c3d-9a4f-0d2e6b8c1a57"
	entryAPI  = "0b9d4c1e-2f6a-5e7b-8c3d-4a5f6e7d8c9b"
	entryNone = "c4e1f2a3-9b8d-5a6c-b7e0-1d2f3a4b5c6d"
)

// entryTasks files four tasks, links three to atlas entries and finishes one.
func entryTasks(t *testing.T) *roundTrip {
	t.Helper()
	r := newRoundTrip(t)
	for _, id := range []string{"t-a", "t-b", "t-c", "t-d"} {
		task := newTask(id)
		task.Key = ""
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	r.relate("t-a", entryDB, worklet.RelationEntry)
	r.relate("t-b", entryDB, worklet.RelationEntry)
	r.relate("t-c", entryAPI, worklet.RelationEntry)
	// A PAGE EDGE whose other end spells an entry's id marks no entry.
	r.relate("t-d", entryAPI, worklet.RelationPage)
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done-b", "t-b", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("finish t-b: %v", err)
	}
	r.drain()
	return r
}

// THE OPEN WORK ON EACH ENTRY IS COUNTED FROM THE TASKS THAT NAME IT: a
// finished task, a task naming another entry and an edge of another kind
// count for nothing.
//
// Mutation: drop the open term and the finished task counts; drop the kind
// term and the page edge marks the entry.
func TestTheOpenWorkOnEachAtlasEntryIsCounted(t *testing.T) {
	t.Parallel()
	r := entryTasks(t)
	marks, err := r.reader.EntryMarks(t.Context(), statelog.ReadStale)
	if err != nil {
		t.Fatalf("EntryMarks: %v", err)
	}
	if len(marks.Open) != 2 || marks.Open[entryDB] != 1 || marks.Open[entryAPI] != 1 || marks.Capped {
		t.Errorf("the open work reads %+v, want one open task on each of the two entries", marks)
	}
}

// AN ENTRY'S WORK IS ITS OPEN TASKS, THEN THE ONES FINISHED LATELY, asked by
// every id the entry answers to; a task finished longer ago than
// [worklet.EntryWorkRecent] has left the list.
func TestAnAtlasEntrysWorkIsItsOpenAndRecentlyFinishedTasks(t *testing.T) {
	t.Parallel()
	r := entryTasks(t)
	read := func(now time.Time, entries ...string) worklet.EntryWork {
		t.Helper()
		out, err := r.reader.EntryWork(t.Context(), worklet.EntryWorkQuery{Entries: entries,
			Level: statelog.ReadStale}, now)
		if err != nil {
			t.Fatalf("EntryWork(%v): %v", entries, err)
		}
		return out
	}
	now := time.Now()
	got := read(now, entryDB)
	if open, recent := taskIDs(got.Open), taskIDs(got.Recent); !slices.Equal(open, []string{"t-a"}) ||
		!slices.Equal(recent, []string{"t-b"}) || got.OpenTotal.Total != 1 || got.RecentTotal.Total != 1 {
		t.Errorf("the database's work reads open %v and recent %v, want t-a and t-b", open, recent)
	}
	both := read(now, entryDB, entryAPI)
	if open := taskIDs(both.Open); !slices.Equal(open, []string{"t-a", "t-c"}) {
		t.Errorf("two ids of one entry read open %v, want t-a and t-c", open)
	}
	if later := read(now.Add(worklet.EntryWorkRecent+time.Hour), entryDB); len(later.Recent) != 0 ||
		len(later.Open) != 1 {
		t.Errorf("a month on the finished task is still listed: %+v", later)
	}
	if none := read(now, entryNone); none.Open == nil || none.Recent == nil || len(none.Open)+len(none.Recent) != 0 {
		t.Errorf("an entry no task names reads %+v, want two empty lists", none)
	}
	for name, q := range map[string]worklet.EntryWorkQuery{
		"no entry":  {Level: statelog.ReadStale},
		"not an id": {Entries: []string{"orders-db"}, Level: statelog.ReadStale},
		"no level":  {Entries: []string{entryDB}},
		"too many":  {Entries: slices.Repeat([]string{entryDB}, worklet.MaxEntryWorkEntries+1), Level: statelog.ReadStale},
	} {
		if _, err := r.reader.EntryWork(t.Context(), q, now); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("a read naming %s answered %v, want a refusal", name, err)
		}
	}
}

// taskIDs is the ids of rows, sorted.
func taskIDs(rows []worklet.TaskRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	slices.Sort(out)
	return out
}

// AN ENTRY IS NAMED BY ITS ID AND NOTHING ELSE, and a task names at most
// [worklet.MaxEntries]: a name, a key or an id in another spelling is refused
// at the write, so no atlas name can enter the tracker's log.
func TestATaskNamesAnAtlasEntryOnlyByItsID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.createTask("deploy")
	relate := func(op string, others ...string) error {
		relations := make([]worklet.Relation, 0, len(others))
		for _, other := range others {
			relations = append(relations, worklet.Relation{Kind: worklet.RelationEntry, Other: other})
		}
		_, err := r.writer.UpdateTask(t.Context(), op, "t-deploy", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Relations: &relations}, worklet.ChangeRelations, nil)
		return err
	}
	for name, other := range map[string]string{
		"a key":       "hostname:api.acme.example",
		"a name":      "orders-db",
		"an upper id": strings.ToUpper(entryDB),
		"a braced id": "{" + entryDB + "}",
		"an address":  "https://api.acme.example/v1",
	} {
		if err := relate("op-bad-"+name, other); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("an entry named by %s was written: %v", name, err)
		}
	}
	many := make([]string, 0, worklet.MaxEntries+1)
	for i := range worklet.MaxEntries + 1 {
		many = append(many, entryDB[:len(entryDB)-2]+string("0123456789abcdef"[i/16])+string("0123456789abcdef"[i%16]))
	}
	if err := relate("op-many", many...); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a task naming %d entries was written: %v", len(many), err)
	}
	if err := relate("op-full", many[:worklet.MaxEntries]...); err != nil {
		t.Errorf("a task naming %d entries was refused: %v", worklet.MaxEntries, err)
	}
}

// A HISTORY ROW SAYS HOW MANY ENTRIES A TASK NAMES AND NEVER WHICH: an
// entry's id is derived from its kind and key, and the row is read by people
// who may not read the atlas.
//
// Mutation: record the set as every other kind's is and the ids are in the row.
func TestAHistoryRowCountsTheAtlasEntriesAndNamesNone(t *testing.T) {
	t.Parallel()
	before := worklet.Task{Relations: []worklet.Relation{{Kind: worklet.RelationEntry, Other: entryDB}}}
	after := worklet.Task{Relations: []worklet.Relation{{Kind: worklet.RelationEntry, Other: entryDB},
		{Kind: worklet.RelationEntry, Other: entryAPI}}}
	moved := worklet.TaskDeltas(before, after, nil)
	got := moved[string(worklet.RelationEntry)]
	if got.From != "1" || got.To != "2" || len(got.Added)+len(got.Removed) != 0 {
		t.Errorf("the row recorded %+v, want a count from 1 to 2", got)
	}
	swapped := worklet.Task{Relations: []worklet.Relation{{Kind: worklet.RelationEntry, Other: entryAPI}}}
	if _, recorded := worklet.TaskDeltas(before, swapped, nil)[string(worklet.RelationEntry)]; !recorded {
		t.Error("one entry swapped for another recorded nothing")
	}
	encoded, err := json.Marshal(moved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), entryDB) || strings.Contains(string(encoded), entryAPI) {
		t.Errorf("the row names an entry: %s", encoded)
	}
}

// A READER WITHOUT THE ATLAS IS TOLD HOW MANY ENTRIES A TASK NAMES AND HANDED
// NONE: the record's relations and the answer's links both lose them, and
// every other edge stays.
func TestADetailWithoutEntriesCountsThemAndKeepsEveryOtherEdge(t *testing.T) {
	t.Parallel()
	relations := []worklet.Relation{{Kind: worklet.RelationEntry, Other: entryDB},
		{Kind: worklet.RelationPage, Other: "s-1"}, {Kind: worklet.RelationEntry, Other: entryAPI}}
	detail := worklet.TaskDetail{Task: worklet.Task{ID: "t-a", Relations: relations},
		Links: []worklet.DetailLink{{Kind: worklet.RelationEntry, Other: entryDB},
			{Kind: worklet.RelationPage, Other: "s-1"}, {Kind: worklet.RelationEntry, Other: entryAPI}}}
	got := detail.WithoutEntries()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.EntriesWithheld != 2 || len(got.Links) != 1 || len(got.Task.Relations) != 1 ||
		strings.Contains(string(encoded), entryDB) || strings.Contains(string(encoded), entryAPI) {
		t.Errorf("the answer reads %s, want two entries counted and neither named", encoded)
	}
	if len(detail.Task.Relations) != 3 || len(detail.Links) != 3 {
		t.Error("the answer it was made from lost its entries")
	}
}
