package tracker_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A CREATE'S KEY COMES FROM THE COUNTER, AND ITS RANK COMES FROM THE KEY.
//
// # What this pins that no unit test can
//
// A create is TWO appends on two subjects: the project's counter, then the
// task. The number the counter's own arbitration hands out is what makes two
// nodes minting at once produce two keys rather than one — and the rank is
// derived from that same number, which is what makes a create's key a pure
// integer at or above the origin and therefore provably outside every key a
// drag can mint.
//
// Both halves are only observable end to end: the counter's number is formed
// inside a decision the framework may run several times, and the rank is a
// column the applier writes.
func TestACreateTakesItsKeyAndRankFromTheCounter(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for i, id := range []string{"t-1", "t-2", "t-3"} {
		task := newTask(id)
		task.Key = ""
		result, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil)
		if err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		want := []string{"ENG-1", "ENG-2", "ENG-3"}[i]
		if result.Key != want {
			t.Fatalf("the %s create minted key %q, want %q — the number comes "+
				"from the counter's own arbitration, so consecutive creates "+
				"take consecutive numbers", id, result.Key, want)
		}
		if !result.Rank.FromCreate() {
			t.Errorf("%s's rank %q is not a key only a create can have minted "+
				"— the whole disjointness argument is about the SHAPE of the "+
				"value, so a create that mints anything else can collide with "+
				"a drag", id, result.Rank)
		}
		r.drain()
	}

	// AND NEW TASKS LAND AT THE TAIL IN CREATION ORDER, which is the
	// property the counter's monotonicity buys and a fractional mint
	// would not.
	answer := r.ask(map[string]any{"container": "project:ENG", "sort": "rank"})
	var keys []string
	for _, row := range answer.Rows {
		keys = append(keys, row.Key)
	}
	if strings.Join(keys, ",") != "ENG-1,ENG-2,ENG-3" {
		t.Fatalf("the board reads %v in rank order, and creates must land at "+
			"the tail in the order they were made", keys)
	}
}

// A CREATE INTO AN ARCHIVED PROJECT IS REFUSED BEFORE THE COUNTER MOVES.
//
// The refusal has to happen in the counter mint's own decision, not after it:
// a create refused between the two appends has already advanced the project's
// numbering for a task that will never exist.
func TestACreateIntoAnArchivedProjectMovesNoCounter(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	archiveENG(t, r)

	before := counterOf(t, r, "ENG")
	_, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil)
	if err == nil {
		t.Fatal("a create into an archived project was accepted")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("the refusal is %v and does not say the project is archived", err)
	}
	r.drain()
	if after := counterOf(t, r, "ENG"); after != before {
		t.Fatalf("the counter moved from %d to %d for a create that was "+
			"refused — a refusal after the mint burns a key number for a task "+
			"nobody will ever see", before, after)
	}
}

// A REQUIRED FIELD IS REQUIRED ON A TASK AND NOT ON A SUBTASK.
//
// ClickUp ships two toggles and so does this, and the default that matters is
// the second: one required field on a project would otherwise block every
// checklist item anybody promotes into a subtask.
func TestARequiredFieldDoesNotBlockASubtask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	requireAField(t, r)

	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err == nil {
		t.Fatal("a task missing a required field was created")
	} else if !strings.Contains(err.Error(), "requires") {
		t.Fatalf("the refusal is %v and does not name the field", err)
	}
	r.drain()

	parent := "t-parent"
	subtask := newTask("t-2")
	subtask.Parent = &parent
	if _, err := r.writer.CreateTask(t.Context(), "op-2", subtask, nil); err != nil {
		t.Fatalf("a SUBTASK missing the same field was refused: %v — the "+
			"subtask toggle defaults off precisely so a promotion is not "+
			"blocked by its project's own policy", err)
	}
}

// A PROMOTED ITEM IS A SUBTASK WITH A KEY, AND ITS PARENT IS MARKED LAST.
//
// The order is the point: marking the parent first leaves a struck-through
// line pointing at a subtask that does not exist, which no reader can tell
// from one somebody purged.
func TestAPromotedItemIsASubtaskAndItsParentPointsAtIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := newTask("t-1")
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	subtask := newTask("t-2")
	subtask.Title = "wire it"
	result, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask, nil)
	if err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	if result.Key != "ENG-2" {
		t.Fatalf("the promotion minted key %q, and a promoted item is a "+
			"subtask with a key exactly as a create is", result.Key)
	}
	r.drain()

	stored := taskOf(t, r, "t-1")
	if len(stored.Checklists) != 1 || len(stored.Checklists[0].Items) != 1 {
		t.Fatalf("the parent's checklist is %+v", stored.Checklists)
	}
	item := stored.Checklists[0].Items[0]
	if item.PromotedTo == nil || *item.PromotedTo != "t-2" {
		t.Fatalf("the item points at %v, not at the subtask it became — the "+
			"line is kept and struck through rather than deleted, which is "+
			"what records that this line became that task", item.PromotedTo)
	}
	child := taskOf(t, r, "t-2")
	if child.Parent == nil || *child.Parent != "t-1" {
		t.Fatalf("the subtask's parent is %v, want t-1", child.Parent)
	}
}

// A RE-RUN OF A PROMOTION COMPLETES IT RATHER THAN DUPLICATING IT.
//
// The subtask's id is derived from the item, so the create is refused on its
// own guarding row and the retry proceeds to the parent commit the first
// attempt did not reach. Without that, the interrupted promotion's repair
// would be a second subtask.
func TestARerunPromotionMarksTheParentItDidNotReach(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := newTask("t-1")
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	subtask := newTask("t-2")
	first, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask, nil)
	if err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	r.drain()
	// THE SAME PROMOTION AGAIN, which is what a re-run after a crash
	// between the subtask and the parent commit does.
	rerun, err := r.writer.PromoteItem(t.Context(), "op-3", "t-1", "i-1", subtask, nil)
	if err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	r.drain()
	// ANSWERED WITH THE SUBTASK IT ALREADY FILED, under the key that one
	// took — not the number the re-run's own mint spent, which is a key no
	// task holds.
	if rerun.Key != first.Key {
		t.Errorf("the re-run answered with key %q, want the subtask's own %q",
			rerun.Key, first.Key)
	}

	answer := r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 2 {
		t.Fatalf("the project holds %d tasks after one promotion run twice — "+
			"the subtask's id is derived from the item precisely so the "+
			"second run finds its own subtask", len(answer.Rows))
	}
}

// A BULK EDIT IS NOT ATOMIC AND SAYS SO PER TASK.
//
// It never was atomic: the caller re-runs the failures, and a result that
// reported one outcome for the batch would make a caller re-run tasks that
// changed.
func TestABulkEditReportsEveryTaskSeparately(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}

	done := tracker.StatusDone
	result, err := r.writer.UpdateTasks(t.Context(), "op-bulk",
		[]string{"t-1", "t-2", "t-missing"}, "ENG",
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if len(result.Applied) != 2 {
		t.Fatalf("the batch applied %v, want the two tasks that exist", result.Applied)
	}
	if _, failed := result.Failed["t-missing"]; !failed {
		t.Fatalf("the batch reported no failure for a task that is not there: "+
			"%+v — a caller that cannot tell which half landed re-runs the "+
			"half that did", result.Failed)
	}
	r.drain()
	answer := r.ask(map[string]any{"container": "project:ENG", "show_closed": "true"})
	closed := 0
	for _, row := range answer.Rows {
		if row.Status == tracker.StatusDone {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("%d of %d tasks are done after a bulk close of two: %+v",
			closed, len(answer.Rows), answer.Rows)
	}
}

// A BULK EDIT OVER ITS OWN CEILING IS REFUSED BEFORE THE FIRST APPEND.
//
// A bulk refused halfway is a caller told "failed" about tasks that changed,
// which is the one refusal that has to come before any of it lands.
func TestABulkEditIsRefusedBeforeAnythingLands(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	ids := make([]string, tracker.MaxBulkTasks+1)
	for i := range ids {
		ids[i] = "t-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bulk", ids, "ENG",
		tracker.TaskPatch{Title: ptr("x")}, tracker.ChangeFields, nil); err == nil {
		t.Fatal("a batch over the task ceiling was accepted")
	}

	// A MAXIMAL PATCH over a full batch: the ceiling is on what the batch
	// APPLIES, so the same task count with a larger patch is refused where
	// a small one is not.
	fields := map[string]json.RawMessage{}
	for i := range tracker.MaxFieldValues {
		fields[fmt.Sprintf("f%03d", i)] = json.RawMessage(
			`"` + strings.Repeat("x", tracker.MaxFieldValueBytes) + `"`)
	}
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bytes",
		ids[:tracker.MaxBulkTasks], "ENG",
		tracker.TaskPatch{Fields: &fields}, tracker.ChangeFields, nil); err == nil {
		t.Fatal("a batch over the byte ceiling was accepted")
	} else if !strings.Contains(err.Error(), "bytes of commits") {
		t.Fatalf("the refusal is %v and does not say what the ceiling is "+
			"about — the caller's remedy is fewer tasks per call, which it "+
			"cannot work out from a bare refusal", err)
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); answer.Rows[0].Title != "a task" {
		t.Fatalf("a refused batch changed a row: %q — the pre-flight runs "+
			"before the first append precisely so it cannot", answer.Rows[0].Title)
	}
}

// A BODY NAMING MORE KEYS THAN THE APPLIER LINKS IS WARNED ABOUT, NOT REFUSED.
//
// THE APPLY MUST NOT DEPEND ON THE WARNING. Every node resolves the same first
// sixty-four keys in document order, so the cap is deterministic without it —
// the warning exists so the person who wrote the body learns that the rest are
// plain text, rather than discovering it on a page.
func TestAnOverReferencedBodyWarnsAndStillWrites(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	var body strings.Builder
	for i := range tracker.MaxReferencesPerBody + 10 {
		body.WriteString("see ENG-")
		body.WriteString(string(rune('0' + i%10)))
		body.WriteString(string(rune('0' + i/10)))
		body.WriteString(" ")
	}
	task := newTask("t-1")
	task.Body = body.String()
	result, err := r.writer.CreateTask(t.Context(), "op-1", task, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("a body naming more keys than the applier links produced no " +
			"warning, so the author learns nothing about the ones that are " +
			"plain text")
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 1 {
		t.Fatal("the warning refused the write; it must not")
	}
}

// --- fixtures ---------------------------------------------------------------

func archiveENG(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-archive",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering", Archived: true,
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("archive the project: %v", err)
	}
	r.drain()
}

func requireAField(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-policy",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			Fields: []tracker.FieldDef{{
				ID: "f-1", Slug: "impact", Name: "Impact",
				Type: tracker.FieldText, Required: true,
			}},
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("declare the field: %v", err)
	}
	r.drain()
}

func counterOf(t *testing.T, r *roundTrip, project string) int {
	t.Helper()
	var last int
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(t.Context(),
			`SELECT last FROM tracker_counters WHERE project_key = ?`,
			project).Scan(&last)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatalf("read %s's counter: %v", project, err)
	}
	return last
}

func taskOf(t *testing.T, r *roundTrip, id string) tracker.Task {
	t.Helper()
	var task tracker.Task
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		var body []byte
		if err := tx.QueryRowContext(t.Context(),
			`SELECT document FROM tracker_tasks WHERE id = ?`, id).
			Scan(&body); err != nil {
			return err
		}
		return json.Unmarshal(body, &task)
	}); err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return task
}

// A TASK THAT NAMES NO UNIT IS FILED INTO ITS PROJECT'S OWN UNIT.
//
// `filed_unit` is the unit the work BELONGS to — it is what `unit=` filters
// on, what a board's `unit` axis groups by, and what the item page labels
// "Filed into" — and a project already knows which unit owns it, because the
// chart apply wrote it on the project's own row.
//
// Nothing derived it. The one caller that filled the field stamped the FILING
// SEAT'S team, so an operator — who holds no seat, and therefore no team —
// filed into ENG and got "Filed into: no unit" on an item whose project said
// Core, and so did every root-level seat, which belongs to no unit either.
// This case is the derivation, at the write, where every writer reaches it.
func TestATaskNamingNoUnitIsFiledIntoItsProjectsOwnUnit(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	if _, err := r.writer.ApplyChart(t.Context(), activation(0), []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "core"},
		// AND ONE THE CHART GAVE NO UNIT, which is a real shape: a
		// project on a root-level seat belongs to no team.
		{Key: "LAB", Name: "Research"},
	}); err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	r.drain()

	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	filed := taskOf(t, r, "t-1")
	if filed.FiledUnit != "core" {
		t.Errorf("a task filed into ENG carries filed_unit %q, want core — "+
			"ENG is owned by core and nobody named anything else",
			filed.FiledUnit)
	}
	// AND THE ROUTING HALF FOLLOWS IT, or the unit's lead is no fallback
	// for work filed into the unit.
	if filed.RoutingUnit != "core" {
		t.Errorf("routing_unit = %q, want core", filed.RoutingUnit)
	}
	// AND THE COLUMN CARRIES IT, not only the document: `unit=` filters on
	// the column, so a value that reached the record and not the row is
	// one no query can find.
	if got := r.strings(
		`SELECT filed_unit FROM tracker_tasks WHERE id = ?`, "t-1"); len(got) != 1 ||
		got[0] != "core" {
		t.Errorf("the applied row's filed_unit is %v, want [core]", got)
	}

	// AN EXPLICIT UNIT IS LEFT ALONE. It is the one thing the project
	// cannot say: work belonging to another team than the one owning the
	// project it sits in is what the argument is for.
	named := newTask("t-2")
	named.FiledUnit, named.RoutingUnit = "platform", "platform"
	if _, err := r.writer.CreateTask(t.Context(), "op-2", named, nil); err != nil {
		t.Fatalf("CreateTask naming a unit: %v", err)
	}
	r.drain()
	if got := taskOf(t, r, "t-2"); got.FiledUnit != "platform" {
		t.Errorf("filed_unit = %q, want the platform the writer named — the "+
			"project's own unit overwrote a stated one", got.FiledUnit)
	}

	// AND A PROJECT THE CHART GAVE NO UNIT FILES INTO NONE, which is
	// honest: an empty unit is a fact about the chart rather than a
	// default to invent.
	unowned := newTask("t-3")
	unowned.Project, unowned.Key = "LAB", ""
	if _, err := r.writer.CreateTask(t.Context(), "op-3", unowned, nil); err != nil {
		t.Fatalf("CreateTask into an unowned project: %v", err)
	}
	r.drain()
	if got := taskOf(t, r, "t-3"); got.FiledUnit != "" {
		t.Errorf("filed_unit = %q for a project the chart gave no unit",
			got.FiledUnit)
	}
}

// AND SO IS A PROMOTED CHECKLIST ITEM, which is a task in a project like any
// other — and was filed into no unit at all, because the one caller that
// stamped the field never ran on this path.
func TestAPromotedSubtaskIsFiledIntoItsProjectsOwnUnit(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	if _, err := r.writer.ApplyChart(t.Context(), activation(0), []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "core"},
	}); err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	r.drain()

	parent := newTask("t-1")
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	subtask := newTask("t-2")
	subtask.Title = "wire it"
	if _, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1",
		subtask, nil); err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	r.drain()

	if got := taskOf(t, r, "t-2"); got.FiledUnit != "core" ||
		got.RoutingUnit != "core" {
		t.Errorf("the promoted subtask is filed into %q and routed to %q, "+
			"want core for both", got.FiledUnit, got.RoutingUnit)
	}
}

// THE UNIT A CREATE IS FILED INTO HEARS ABOUT IT FROM THE CREATE ITSELF.
//
// A create naming no unit is filed into its project's own by the writer,
// inside its snapshot — after the caller built the notification, from a task
// that had no routing unit yet. So the wake carried no unit lead: the
// fallback that reaches a unit's lead when a change names nobody else had only
// the project's lead to offer on the one record that files the work, while
// every later change to the same task, snapshotted from a row that holds the
// unit, offered the unit's lead first. Where the chart resolves the project to
// no lead of its own — a seat's own project inside a unit, which
// [org.Organization.LeadsBy] gives none because a member declares where it
// writes rather than who owns it — an unassigned task filed there reached
// nobody at all. The case is that project: the unit has a lead and the
// project resolves to none.
//
// AND THE CARD SAYS SO, as the history row does: the applier writes a create's
// row from every field the task was filed with, the unit the writer settled
// among them, so a card built from the task the caller handed in named no
// `routing_unit` while the row beside it did — and a promotion's named no
// `parent` either, which the writer sets too.
func TestACreateTellsTheLeadOfTheUnitItIsFiledInto(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	chart := fixedLeads{unit: "core-lead"}
	r.ledBy(chart)
	if _, err := r.writer.ApplyChart(t.Context(), activation(0), []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "core"},
	}); err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	r.drain()

	reachesTheLead := func(what, id string, wake *tracker.Notify) {
		t.Helper()
		if wake == nil {
			t.Fatalf("%s published no notification, so this case tests nothing", what)
		}
		if wake.Snapshot.RoutingUnitLead != "core-lead" {
			t.Errorf("%s files the work into core and its notification names %q as "+
				"core's lead, want core-lead", what, wake.Snapshot.RoutingUnitLead)
		}
		if moved := wake.Fields["routing_unit"]; moved.From != "" || moved.To != "core" {
			t.Errorf("%s files the work into core and its card's routing_unit is %+v, "+
				"want it moved from nothing to core", what, moved)
		}
		r.cardIsTheHistoryRow(what, id, wake)
		routed := tracker.Route(tracker.Candidates(wake, false),
			func(string) bool { return true }, tracker.PartyOf("ana"))
		var reached []string
		for _, c := range routed {
			reached = append(reached, c.Handle)
		}
		if len(reached) != 1 || reached[0] != "core-lead" {
			t.Errorf("%s of unassigned work into core reached %v, want core's lead "+
				"and nobody else", what, reached)
		}
	}

	// AN UNASSIGNED TASK, filed by the person who watches it: the actor is
	// dropped, so the fallback is the whole of who hears about it.
	task := newTask("t-1")
	task.Reporter, task.Watchers = "ana", []string{"ana"}
	task.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", task,
		tracker.Wake{Kind: tracker.ChangeCreated, After: task}.Notify(chart)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	reachesTheLead("a create", "t-1", r.lastWake())

	// AND A PROMOTED CHECKLIST ITEM, the other create the writer files.
	subtask := newTask("t-2")
	subtask.Title, subtask.Reporter, subtask.Watchers = "wire it", "ana", []string{"ana"}
	if _, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask,
		tracker.Wake{Kind: tracker.ChangeCreated, After: subtask}.Notify(chart)); err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	r.drain()
	wake := r.wakeOf("t-2")
	reachesTheLead("a promotion", "t-2", wake)
	if moved := wake.Fields["parent"]; moved.To != "t-1" {
		t.Errorf("a promotion out of t-1 carries a card whose parent is %+v, want t-1", moved)
	}
}

// cardIsTheHistoryRow holds a create's notification card to the history row
// the applier wrote for the same create: the same fields, moved the same way.
//
// THE ROW IS THE REFERENCE, because it is computed from the task as it was
// FILED, in the applier's own frame ([tracker.TaskDeltas] over nothing and the
// stored row), while a card is built by a caller from the task it handed in —
// before the writer settled what only it can.
func (r *roundTrip) cardIsTheHistoryRow(what, id string, wake *tracker.Notify) {
	r.t.Helper()
	rows := r.strings(`SELECT fields_json FROM tracker_history
		WHERE subject_id = ? AND kind = ? ORDER BY log_seq`, id, string(tracker.ChangeCreated))
	if len(rows) != 1 {
		r.t.Fatalf("%s wrote %d create rows for %s, want one", what, len(rows), id)
	}
	var row map[string]tracker.Delta
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		r.t.Fatalf("decode %s's history row: %v", what, err)
	}
	if wake.FieldsOmitted != 0 || len(row) > tracker.MaxDeltas {
		r.t.Fatalf("the premise: %s's card leaves %d fields off and its row holds %d, so "+
			"the two cannot be compared whole", what, wake.FieldsOmitted, len(row))
	}
	for field, moved := range row {
		if shown, held := wake.Fields[field]; !held {
			r.t.Errorf("%s's history row says %s moved (%+v) and its card does not", what,
				field, moved)
		} else if !slices.Equal(shown.Added, moved.Added) ||
			!slices.Equal(shown.Removed, moved.Removed) ||
			shown.From != moved.From || shown.To != moved.To {

			r.t.Errorf("%s's card says %s moved %+v and its history row %+v", what, field,
				shown, moved)
		}
	}
	for field, shown := range wake.Fields {
		if _, held := row[field]; !held {
			r.t.Errorf("%s's card says %s moved (%+v) and its history row does not", what,
				field, shown)
		}
	}
}

// A CREATE'S CARD SAYS WHAT THE WRITER SETTLED, AS ITS HISTORY ROW DOES.
//
// A caller builds a create's notification from the task it hands in, and the
// writer then settles what only the writer can: the tags as the slugs they
// normalise to and the default type, before the key is minted; and inside the
// create's own snapshot the project's default assignee and the watch they then
// hold, the status and priority a create naming none starts at, and the
// person a question is put to, who follows the item from its first record.
// The applier writes the create's history row from the task as FILED, so a
// card left at what the caller handed in disagreed with the row beside it on
// each of them — and its snapshot routed from an assignee, a status and a
// watcher list the record does not carry.
func TestACreatesCardSaysWhatTheWriterSettled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bo := "bo"
	if _, err := r.writer.WriteProject(t.Context(), "op-default", "ENG",
		tracker.ProjectEdit{DefaultAssignee: &bo},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("set the default assignee: %v", err)
	}
	r.drain()
	r.declareTags("api")

	// EVERYTHING THE WRITER SETTLES LEFT FOR IT TO SETTLE: no type, status,
	// priority or assignee, a tag spelled as a person types it, and a
	// question put to somebody who is not watching yet.
	task := newTask("t-1")
	task.Key, task.Type, task.Status, task.StatusGroup, task.Priority = "", "", "", "", ""
	task.Tags, task.Reporter, task.Watchers = []string{"API"}, "ana", []string{"ana"}
	ask := tracker.Comment{
		ID: "c-1", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "Which importer is this?", Ask: "cy", CreatedAt: wednesday,
	}
	notify := tracker.Wake{Kind: tracker.ChangeCreated, After: task, Comment: &ask}.Notify(nil)
	if _, err := r.writer.CreateTaskAsking(t.Context(), "op-1", task, ask, notify); err != nil {
		t.Fatalf("CreateTaskAsking: %v", err)
	}
	r.drain()

	wake := r.lastWake()
	r.cardIsTheHistoryRow("a create the writer settled", "t-1", wake)
	if moved := wake.Fields["tags"]; !slices.Equal(moved.Added, []string{"api"}) {
		t.Errorf("the card's tags are %+v, want the slug the task was filed under", moved)
	}
	snapshot := wake.Snapshot
	if snapshot.Assignee != "bo" {
		t.Errorf("the wake routes from assignee %q, want the project's default bo",
			snapshot.Assignee)
	}
	if snapshot.Status != tracker.StatusTodo || snapshot.StatusGroup != tracker.GroupNotStarted {
		t.Errorf("the wake routes from status %q in group %q, want the %q the task starts at",
			snapshot.Status, snapshot.StatusGroup, tracker.StatusTodo)
	}
	for _, who := range []string{"ana", "bo", "cy"} {
		if !slices.Contains(snapshot.Watchers, who) {
			t.Errorf("the wake's watchers are %v, want %s among them, as the row has",
				snapshot.Watchers, who)
		}
	}
}

// wakeOf reads the notification off the newest record on task id's own
// subject — off the log, for [roundTrip.lastWake]'s reason, and by subject
// because a sequence's last record need not be the one about this task.
func (r *roundTrip) wakeOf(id string) *tracker.Notify {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	for seq := last; seq > 0; seq-- {
		_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil || !ok {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		record, err := tracker.Decode(payload)
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		if record.Subject == tracker.TaskSubject(id) {
			return record.Notify
		}
	}
	r.t.Fatalf("no record on the log is about task %s", id)
	return nil
}
