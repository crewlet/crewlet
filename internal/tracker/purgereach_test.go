package tracker_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PURGE IS HELD BACK BY A RECORD DEFERRED ON ANY TASK ITS APPLY REWRITES.
//
// A record's scope is the complete set of objects its apply may write, and a
// purge's apply writes rows keyed on other tasks: its dependents' edges, the
// mirror its blockers keep, the relations other tasks hold to it, the
// references to it, and its children's pointers. Its scope named the purged
// task alone, so a record this node could not decode about any of those tasks
// — a newer peer's, deferred here until the upgrade reaches this node — did not
// stop the purge that rewrites that task's rows, and on the node that deferred
// it the purge was applied first and the deferred record later, over rows the
// rest of the fleet had changed in the other order.
func TestAPurgeIsHeldBackByARecordDeferredOnATaskItRewrites(t *testing.T) {
	t.Parallel()
	for name, stage := range map[string]func(r *roundTrip, purged, other tracker.Task){
		"a dependent in another project": func(r *roundTrip, purged, other tracker.Task) {
			if _, err := r.writer.Depend(r.t.Context(), "op-depend", tracker.DependencyChange{
				Task: other.ID, Project: other.Project, WaitingOnAdd: []string{purged.ID},
			}, fixedLeads{project: "eng-lead"}); err != nil {
				r.t.Fatalf("depend: %v", err)
			}
		},
		"a blocker it waits on": func(r *roundTrip, purged, other tracker.Task) {
			if _, err := r.writer.Depend(r.t.Context(), "op-depend", tracker.DependencyChange{
				Task: purged.ID, Project: purged.Project, WaitingOnAdd: []string{other.ID},
			}, fixedLeads{project: "eng-lead"}); err != nil {
				r.t.Fatalf("depend: %v", err)
			}
		},
		"a task linked to it": func(r *roundTrip, purged, other tracker.Task) {
			r.relate(other.ID, purged.ID, tracker.RelationLinked)
		},
		"a task referencing it": func(r *roundTrip, purged, other tracker.Task) {
			body := "see " + purged.Key + " for the history"
			if _, err := r.writer.UpdateTask(r.t.Context(), "op-body", other.ID, other.Project,
				tracker.NoIfMatch, tracker.TaskPatch{Body: &body}, tracker.ChangeFields, nil); err != nil {
				r.t.Fatalf("reference: %v", err)
			}
		},
		"its child": func(r *roundTrip, purged, other tracker.Task) {
			if _, err := r.writer.UpdateTask(r.t.Context(), "op-parent", other.ID, other.Project,
				tracker.NoIfMatch, tracker.TaskPatch{Parent: &purged.ID}, tracker.ChangeReparented, nil); err != nil {
				r.t.Fatalf("parent: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			purged := r.createTask("to be purged")
			other := r.createTask("the other task")
			if name == "a dependent in another project" {
				// Filed elsewhere, where its records are filed and where a
				// scope has to name it.
				seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
				waiting := newTask("t-waits-from-ops")
				waiting.Project, waiting.Key, waiting.Title = "OPS", "", "waits across projects"
				if _, err := r.writer.CreateTask(t.Context(), "op-"+waiting.ID, waiting, nil); err != nil {
					t.Fatalf("file the dependent: %v", err)
				}
				r.drain()
				other = oneTask(t, r, waiting.ID)
			}
			stage(r, purged, other)
			r.drain()
			r.deferRecordOn(other.ID, other.Project)

			operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
			_, err := operator.PurgeTask(t.Context(), "op-purge", purged.ID, purged.Project,
				"a duplicate import")
			var unavailable *statelog.Unavailable
			if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonDeferred {
				t.Fatalf("a purge rewriting %s, which a deferred record covers = %v, want it "+
					"refused as deferred", name, err)
			}
		})
	}
}

// A PURGE'S EFFECT ON ANOTHER TASK OUTLIVES THAT TASK'S NEXT EDIT.
//
// Every task's relations, dependency edges, mirror and parent pointer are
// derived from its own document on every record about it, and the purge fixed
// only the rows: the next edit to a dependent put the purged blocker back —
// OPEN, since a blocker with no row reads as open — and blocked it for ever on
// a task that no longer existed; the next edit to a child put the purged task
// back as its parent, a pointer to nothing; and the next edit to a blocker put
// the purged task back among the tasks it would tell it was unblocked.
func TestAPurgesEffectOnAnotherTaskOutlivesThatTasksNextEdit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	purged := r.createTask("to be purged")
	dependent := r.createTask("waits on it")
	blocker := r.createTask("it waits on")
	child := r.createTask("under it")
	for _, change := range []tracker.DependencyChange{
		{Task: dependent.ID, Project: dependent.Project, WaitingOnAdd: []string{purged.ID}},
		{Task: purged.ID, Project: purged.Project, WaitingOnAdd: []string{blocker.ID}},
	} {
		if _, err := r.writer.Depend(t.Context(), "op-depend-"+change.Task, change,
			fixedLeads{project: "eng-lead"}); err != nil {
			t.Fatalf("depend: %v", err)
		}
		r.drain()
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-parent", child.ID, child.Project,
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &purged.ID}, tracker.ChangeReparented, nil); err != nil {
		t.Fatalf("parent: %v", err)
	}
	r.drain()

	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", purged.ID, purged.Project,
		"a duplicate import"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	// THE NEXT EDIT TO EACH, which is what re-derived the rows before.
	for _, id := range []string{dependent.ID, blocker.ID, child.ID} {
		title := "edited after the purge"
		if _, err := r.writer.UpdateTask(t.Context(), "op-edit-"+id, id, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil); err != nil {
			t.Fatalf("edit %s: %v", id, err)
		}
		r.drain()
	}

	if got := r.strings(`SELECT blocker_id FROM tracker_task_deps WHERE task_id = ?`,
		dependent.ID); len(got) != 0 {
		t.Errorf("the dependent waits on %v after its next edit — a purged blocker, "+
			"open for ever", got)
	}
	if held := oneTask(t, r, dependent.ID); slices.ContainsFunc(held.Relations,
		func(rel tracker.Relation) bool { return rel.Other == purged.ID }) {
		t.Errorf("the dependent's document still relates to the purged task: %+v", held.Relations)
	}
	if held := oneTask(t, r, blocker.ID); slices.Contains(held.Dependents, purged.ID) {
		t.Errorf("the blocker still lists the purged task among its dependents: %v",
			held.Dependents)
	}
	if got := r.strings(`SELECT dependent_id FROM tracker_task_dependents WHERE task_id = ?`,
		blocker.ID); len(got) != 0 {
		t.Errorf("the blocker's mirror rows are back after its next edit: %v", got)
	}
	held := oneTask(t, r, child.ID)
	if held.Parent != nil {
		t.Errorf("the child's parent is %q after its next edit, want the purged task's "+
			"own parent (none) — a pointer to a task that no longer exists", *held.Parent)
	}
	if got := r.strings(`SELECT COALESCE(parent_id, '') FROM tracker_tasks WHERE id = ?`,
		child.ID); len(got) != 1 || got[0] != "" {
		t.Errorf("the child's parent pointer is %v after its next edit", got)
	}
}
