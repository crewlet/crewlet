package tracker_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// seedNamedSprint files one sprint in a given state.
func seedNamedSprint(t *testing.T, r *roundTrip, number int, name string,
	state tracker.SprintState) {

	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-sp-"+itoa(number),
		tracker.SprintSubject("ENG", number), "", tracker.Sprint{
			V: 1, Project: "ENG", Number: number,
			Name: name, State: state,
			StartAt: wednesday, EndAt: wednesday.AddDate(0, 0, 14),
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeMoved, nil); err != nil {
		t.Fatalf("seed sprint %d: %v", number, err)
	}
	r.drain()
}

// inSprint files a task into a sprint.
func inSprint(t *testing.T, r *roundTrip, id string, number *int) {
	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	task.Sprint = number
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// A TASK'S SPRINT MEMBERSHIP IS A ROW, and until one was written every
// sprint-scoped query answered zero.
//
// `tracker_task_sprints` is one row per STAY — what every sprint report is
// derived from — and it was DELETEd on purge, READ by the filter, and never
// INSERTed by anything. The filter, the index the DDL ships for it and the
// Sprint board view were all over an empty table.
func TestASprintFilterFindsTheTasksInThatSprint(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedNamedSprint(t, r, 4, "Kickoff", tracker.SprintActive)
	four := 4
	inSprint(t, r, "in", &four)
	inSprint(t, r, "out", nil)

	byNumber := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "4",
	}))
	if len(byNumber) != 1 || byNumber[0] != "in" {
		t.Fatalf("sprint=4 answers %v, want the one task filed into it — an "+
			"empty answer is what a filter over a table nobody writes gives",
			byNumber)
	}

	// AND THE STATE RESOLVES AGAINST THE SPRINT ROW, so `active` keeps
	// meaning "the sprint that is active now" as sprints open and close
	// rather than freezing whichever number was active when a view was
	// saved.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "active",
	})); len(got) != 1 || got[0] != "in" {
		t.Fatalf("sprint=active answers %v, want the task in the active sprint", got)
	}
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "Kickoff",
	})); len(got) != 1 || got[0] != "in" {
		t.Fatalf("sprint=<name> answers %v, want the task in that sprint", got)
	}

	// AND `none` IS THE BACKLOG, which had no spelling at all — the one
	// the implicit Backlog view needs and said in a comment it could not
	// have.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "none",
	})); len(got) != 1 || got[0] != "out" {
		t.Fatalf("sprint=none answers %v, want the task in no sprint", got)
	}
}

// A CARRY-OVER IS IN BOTH SPRINTS, which is why membership is a stay rather
// than a column: a column could name only where the task is now, and every
// sprint report is about where it has been.
func TestACarryOverIsInBothSprints(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedNamedSprint(t, r, 4, "Four", tracker.SprintClosed)
	seedNamedSprint(t, r, 5, "Five", tracker.SprintActive)
	four, five := 4, 5
	inSprint(t, r, "rolled", &four)

	if _, err := r.writer.UpdateTask(t.Context(), "op-roll", "rolled", "ENG", 0,
		tracker.TaskPatch{Sprint: &five}, tracker.ChangeSprint, nil); err != nil {
		t.Fatalf("carry it over: %v", err)
	}
	r.drain()

	for _, sprint := range []string{"4", "5"} {
		if got := ids(r.ask(map[string]any{
			"container": "project:ENG", "sprint": sprint,
		})); len(got) != 1 {
			t.Errorf("sprint=%s answers %v, want the carried-over task: it was "+
				"in four and is in five, and a report about either is about it",
				sprint, got)
		}
	}
	// AND IT IS NOT IN THE BACKLOG: the stay it left is closed, and the
	// one it is in is open. A backlog that showed it would show the person
	// planning the next sprint work that is already in one.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "none",
	})); len(got) != 0 {
		t.Errorf("sprint=none answers %v, and a carried-over task is not in "+
			"the backlog", got)
	}

	// AND A TASK PULLED BACK OUT OF A SPRINT IS IN THE BACKLOG AGAIN.
	// That is why the backlog is an absence of an OPEN stay rather than of
	// every stay: this task has a stay on four and a stay on five, both
	// closed, and it is unplanned work somebody has to schedule.
	// A ZERO IS THE CLEAR — the patch's own spelling for "no sprint",
	// since an absent field means "leave it alone".
	if _, err := r.writer.UpdateTask(t.Context(), "op-pull", "rolled", "ENG", 0,
		tracker.TaskPatch{Sprint: new(int)}, tracker.ChangeSprint, nil); err != nil {
		t.Fatalf("pull it out of the sprint: %v", err)
	}
	r.drain()
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "none",
	})); len(got) != 1 || got[0] != "rolled" {
		t.Errorf("sprint=none answers %v after the task was pulled out of "+
			"every sprint, want it back in the backlog — a backlog spelled "+
			"as 'no stay at all' would never show a task that had been in one",
			got)
	}

	// THE CLOSED STAY SAYS WHERE THE WORK WENT, which is what tells a
	// report work that rolled forward from work that was dropped.
	var rolledTo sql.NullInt64
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT rolled_to FROM tracker_task_sprints
			 WHERE task_id = 'rolled' AND sprint = 4`).Scan(&rolledTo)
	}); err != nil {
		t.Fatalf("read the closed stay: %v", err)
	}
	if !rolledTo.Valid || rolledTo.Int64 != 5 {
		t.Errorf("the stay on sprint 4 rolled to %v, want 5", rolledTo)
	}
}

// A SPRINT IS NUMBERED AND NAMED PER PROJECT, so naming one at company scope
// names one in every project — refused rather than answered about all of them.
func TestANamedSprintNeedsItsProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, value := range []string{"4", "active", "Kickoff"} {
		if err := r.askErr(map[string]any{
			"container": "workspace", "sprint": value,
		}); err == nil {
			t.Errorf("sprint=%s was accepted at company scope, where it names "+
				"a different sprint in every project", value)
		}
	}
	// `none` IS THE EXCEPTION and needs no project: "in no sprint" means
	// the same thing everywhere.
	if err := r.askErr(map[string]any{
		"container": "workspace", "sprint": "none",
	}); err != nil {
		t.Errorf("sprint=none was refused at company scope: %v", err)
	}
}

// A TASK IS FILED INTO A SPRINT THE PROJECT MINTED, OR INTO NONE.
//
// The schedule tool's schema says `sprint` is a number `sprint_report` lists,
// and nothing checked it: any positive integer was stored, and the task then
// pointed at a membership that does not exist. `sprint_report` and the
// burndown both refuse that sprint with [tracker.ErrNoSprint], so the work was
// filed where no report can show it while the seat was told the write worked.
//
// A REFUSAL RATHER THAN AN UNAVAILABLE, which is the answer an undeclared TAG
// gets: a sprint number comes from a model reading a schema that only
// describes it, so absent is overwhelmingly a number nobody minted, and
// "retry" would be advice that never comes good.
func TestATaskCannotBeFiledIntoASprintNobodyMinted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// A CREATE naming a sprint the project has not minted.
	unminted := newTask("ghost")
	unminted.Key = "ENG-ghost"
	unminted.Sprint = ptr(99)
	_, err := r.writer.CreateTask(t.Context(), "op-ghost", unminted, nil)
	if err == nil {
		t.Fatal("a task was filed into sprint 99, which nobody minted — no " +
			"report can show it and the caller was told it worked")
	}
	if !strings.Contains(err.Error(), "sprint 99") {
		t.Errorf("the refusal does not name the sprint: %v", err)
	}
	// AND IT SAYS WHERE TO LOOK, which is the whole difference between a
	// refusal somebody can act on and one they can only retry.
	if !strings.Contains(err.Error(), "sprint_report") {
		t.Errorf("the refusal names no way to find the real ones: %v", err)
	}

	// AN UPDATE takes the same gate.
	inSprint(t, r, "real", nil)
	if _, err := r.writer.UpdateTask(t.Context(), "op-move", "real", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Sprint: ptr(99)},
		tracker.ChangeSprint, nil); err == nil {
		t.Error("a task was MOVED into sprint 99, which nobody minted")
	}

	// AND A MINTED ONE IS ACCEPTED, so the gate refuses the unminted rather
	// than the sprint field.
	seedSprintWindow(t, r, 1, tracker.SprintActive,
		time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(13*24*time.Hour), nil)
	if _, err := r.writer.UpdateTask(t.Context(), "op-real", "real", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Sprint: ptr(1)},
		tracker.ChangeSprint, nil); err != nil {
		t.Errorf("a minted sprint was refused: %v", err)
	}

	// AND TAKING A TASK OUT names no sprint, so it passes whatever the
	// project has minted.
	r.drain()
	if _, err := r.writer.UpdateTask(t.Context(), "op-out", "real", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Sprint: ptr(0)},
		tracker.ChangeSprint, nil); err != nil {
		t.Errorf("clearing a sprint was refused: %v", err)
	}
}
