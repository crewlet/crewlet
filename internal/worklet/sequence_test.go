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
// A create appends twice: the project's counter, then the task. The counter's
// own arbitration gives two nodes minting at once two keys, and the rank,
// derived from that number, is a pure integer at or above the origin, outside
// every key a drag can mint. Both show only end to end: the number is formed
// in a decision the framework may rerun, and the rank is an applier's column.
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

	// AND NEW TASKS LAND AT THE TAIL IN CREATION ORDER, which the counter's
	// monotonicity buys and a fractional mint would not.
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
// Refused in the counter mint's own decision: a refusal between the two
// appends has already advanced the numbering for a task that will never exist.
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
// The subtask toggle defaults off: one required field on a project would
// otherwise be a form to fill for every step somebody breaks a task into.
func TestARequiredFieldDoesNotBlockASubtask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE PARENT IS FILED BEFORE THE POLICY that would refuse it: a subtask
	// naming a parent this node does not hold is refused as such.
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
// THE APPLY MUST NOT DEPEND ON THE WARNING: every node resolves the same first
// [worklet.MaxReferencesPerBody] keys in document order, so the cap is
// deterministic without it. The warning tells the author the rest are plain text.
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
// `filed_unit` is the unit the work BELONGS to (what `unit=` filters, a board's
// `unit` axis groups and the item page labels "Filed into"), and the chart
// apply wrote the project's owning unit on its row. It is derived at the write,
// where every writer reaches it, not from the filing seat's team, which an
// operator or a root-level seat does not have.
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

	// AN EXPLICIT UNIT IS LEFT ALONE: it says what the project cannot, work
	// belonging to another team than the project's owner.
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

	// AND A PROJECT THE CHART GAVE NO UNIT FILES INTO NONE: an empty unit is
	// a fact about the chart, not a default to invent.
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
// The writer files a create naming no unit into its project's own inside its
// snapshot, after the caller built the notification from a task with no
// routing unit; the wake must still name the unit's lead, the fallback when a
// change names nobody else. The case is a project the chart resolves to no
// lead of its own (a seat's own project inside a unit, which
// [org.Organization.LeadsBy] gives none) under a unit that has one.
//
// AND THE CARD SAYS SO, as the history row does: the applier writes a create's
// row from the task as filed, the settled unit included.
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
// THE ROW IS THE REFERENCE: it is computed from the task as FILED
// ([worklet.TaskDeltas] over nothing and the stored row), while a caller builds
// a card from the task it handed in, before the writer settled what only it can.
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
// A caller builds the notification from the task it hands in; the writer then
// settles the tags' slugs and the default type before the key is minted, and,
// in the create's snapshot, the project's default assignee and their watch,
// the starting status and priority, and the person a question is put to, who
// follows the item from its first record. The card and its snapshot carry what
// was settled, as the history row, written from the task as FILED, does.
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

	// EVERYTHING LEFT FOR THE WRITER TO SETTLE: no type, status, priority or
	// assignee, a tag as a person types it, and a question to a non-watcher.
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
