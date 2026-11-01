package worklet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/worklet"
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
// the second: one required field on a project would otherwise be a form to
// fill for every step somebody breaks a task into.
func TestARequiredFieldDoesNotBlockASubtask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE PARENT EXISTS FIRST, filed before the policy that would refuse it
	// landed: a subtask is filed under a real parent in its own project,
	// and one naming a parent this node does not hold is refused as such.
	if _, err := r.writer.CreateTask(t.Context(), "op-parent",
		newTask("t-parent"), nil); err != nil {
		t.Fatalf("CreateTask parent: %v", err)
	}
	r.drain()
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
			"subtask toggle defaults off precisely so breaking a task down "+
			"is not blocked by its project's own policy", err)
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
	for i := range worklet.MaxReferencesPerBody + 10 {
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
	if len(result.Caveats) == 0 || result.Caveats[0].Code() != caveat.CodeBodyLinksCapped {
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
		worklet.ProjectSubject("ENG"), "", worklet.Project{
			V: 1, Key: "ENG", Name: "Engineering", Archived: true,
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("archive the project: %v", err)
	}
	r.drain()
}

func requireAField(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-policy",
		worklet.ProjectSubject("ENG"), "", worklet.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			Fields: []worklet.FieldDef{{
				ID: "f-1", Slug: "impact", Name: "Impact",
				Type: worklet.FieldText, Required: true,
			}},
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
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

func taskOf(t *testing.T, r *roundTrip, id string) worklet.Task {
	t.Helper()
	var task worklet.Task
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
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []worklet.ChartProject{
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
// `routing_unit` while the row beside it did.
func TestACreateTellsTheLeadOfTheUnitItIsFiledInto(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	chart := fixedLeads{unit: "core-lead"}
	r.ledBy(chart)
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []worklet.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "core"},
	}); err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	r.drain()

	reachesTheLead := func(what, id string, wake *worklet.Notify) {
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
		routed := worklet.Route(worklet.Candidates(wake),
			func(string) bool { return true }, "ana")
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
	if _, err := r.writer.CreateTask(t.Context(), "op-1", task,
		worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(chart)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	reachesTheLead("a create", "t-1", r.lastWake())
}

// cardIsTheHistoryRow holds a create's notification card to the history row
// the applier wrote for the same create: the same fields, moved the same way.
//
// THE ROW IS THE REFERENCE, because it is computed from the task as it was
// FILED, in the applier's own frame ([worklet.TaskDeltas] over nothing and the
// stored row), while a card is built by a caller from the task it handed in —
// before the writer settled what only it can.
func (r *roundTrip) cardIsTheHistoryRow(what, id string, wake *worklet.Notify) {
	r.t.Helper()
	rows := r.strings(`SELECT fields_json FROM tracker_history
		WHERE subject_id = ? AND kind = ? ORDER BY log_seq`, id, string(worklet.ChangeCreated))
	if len(rows) != 1 {
		r.t.Fatalf("%s wrote %d create rows for %s, want one", what, len(rows), id)
	}
	var row map[string]worklet.Delta
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		r.t.Fatalf("decode %s's history row: %v", what, err)
	}
	if wake.FieldsOmitted != 0 || len(row) > worklet.MaxDeltas {
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
		worklet.ProjectEdit{DefaultAssignee: &bo},
		worklet.ProjectAuthority{Policy: true}); err != nil {

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
	ask := worklet.Comment{
		ID: "c-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "Which importer is this?", Ask: "cy", CreatedAt: wednesday,
	}
	notify := worklet.Wake{Kind: worklet.ChangeCreated, After: task, Comment: &ask}.Notify(nil)
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
	if snapshot.Status != worklet.StatusTodo || snapshot.StatusGroup != worklet.GroupNotStarted {
		t.Errorf("the wake routes from status %q in group %q, want the %q the task starts at",
			snapshot.Status, snapshot.StatusGroup, worklet.StatusTodo)
	}
	for _, who := range []string{"ana", "bo", "cy"} {
		if !slices.Contains(snapshot.Watchers, who) {
			t.Errorf("the wake's watchers are %v, want %s among them, as the row has",
				snapshot.Watchers, who)
		}
	}
}
