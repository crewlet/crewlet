package tracker_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	if _, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask, nil); err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	r.drain()
	// THE SAME PROMOTION AGAIN, which is what a re-run after a crash
	// between the subtask and the parent commit does.
	if _, err := r.writer.PromoteItem(t.Context(), "op-3", "t-1", "i-1", subtask, nil); err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	r.drain()

	answer := r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 2 {
		t.Fatalf("the project holds %d tasks after one promotion run twice — "+
			"the subtask's id is derived from the item precisely so the "+
			"second run finds its own subtask", len(answer.Rows))
	}
}

// ONE PROJECT RUNS ONE SPRINT, AND THE POINTER IS WHERE THAT IS ARBITRATED.
//
// The sprint record keeps its own separate veto — a start moves future to
// active and refuses any other state — so an already-closed sprint can never
// be resurrected past a nil pointer.
func TestOneProjectRunsOneSprint(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedSprint(t, r, 1)
	seedSprint(t, r, 2)

	if _, err := r.writer.StartSprint(t.Context(), "op-s1", "ENG", 1); err != nil {
		t.Fatalf("StartSprint 1: %v", err)
	}
	r.drain()
	if _, err := r.writer.StartSprint(t.Context(), "op-s2", "ENG", 2); err == nil {
		t.Fatal("a second sprint started while the first was running")
	} else if !strings.Contains(err.Error(), "running sprint 1") {
		t.Fatalf("the refusal is %v and does not name the sprint that holds "+
			"the pointer", err)
	}
	r.drain()

	if _, err := r.writer.CloseSprint(t.Context(), "op-c1", "ENG", 1); err != nil {
		t.Fatalf("CloseSprint: %v", err)
	}
	r.drain()
	// AND A CLOSED SPRINT CANNOT BE RESTARTED, whatever the pointer says.
	if _, err := r.writer.StartSprint(t.Context(), "op-s3", "ENG", 1); err == nil {
		t.Fatal("a closed sprint was restarted")
	} else if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("the refusal is %v and does not name the state that refused "+
			"it — the record's own veto is separate from the pointer's", err)
	}
	r.drain()
	if _, err := r.writer.StartSprint(t.Context(), "op-s4", "ENG", 2); err != nil {
		t.Fatalf("sprint 2 after 1 closed: %v", err)
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
		tracker.TaskPatch{Status: &done}, nil)
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
		tracker.TaskPatch{Title: ptr("x")}, nil); err == nil {
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
		tracker.TaskPatch{Fields: &fields}, nil); err == nil {
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
		}, nil); err != nil {
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
		}, nil); err != nil {
		t.Fatalf("declare the field: %v", err)
	}
	r.drain()
}

func seedSprint(t *testing.T, r *roundTrip, number int) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-sprint-"+string(rune('0'+number)),
		tracker.SprintSubject("ENG", number), "", tracker.Sprint{
			V: 1, Project: "ENG", Number: number,
			Name: "Sprint", State: tracker.SprintFuture,
			StartAt: wednesday, EndAt: wednesday.AddDate(0, 0, 14),
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, nil); err != nil {
		t.Fatalf("seed sprint %d: %v", number, err)
	}
	r.drain()
}

func counterOf(t *testing.T, r *roundTrip, project string) int {
	t.Helper()
	var last int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
