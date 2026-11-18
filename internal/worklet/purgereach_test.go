package worklet_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A PURGE IS HELD BACK BY A RECORD DEFERRED ON ANY TASK ITS APPLY REWRITES.
//
// A purge's apply writes rows keyed on other tasks (dependents, blockers'
// mirrors, relations, references, children), so its scope must name them: a
// record deferred on one of them would otherwise be overtaken by the purge on
// the deferring node, applied there in the opposite order to the fleet.
func TestAPurgeIsHeldBackByARecordDeferredOnATaskItRewrites(t *testing.T) {
	t.Parallel()
	for name, stage := range map[string]func(r *roundTrip, purged, other worklet.Task){
		"a dependent in another project": func(r *roundTrip, purged, other worklet.Task) {
			if _, err := r.writer.Depend(r.t.Context(), "op-depend", worklet.DependencyChange{
				Task: other.ID, Project: other.Project, WaitingOnAdd: []string{purged.ID},
			}, fixedLeads{project: "eng-lead"}); err != nil {
				r.t.Fatalf("depend: %v", err)
			}
		},
		"a blocker it waits on": func(r *roundTrip, purged, other worklet.Task) {
			if _, err := r.writer.Depend(r.t.Context(), "op-depend", worklet.DependencyChange{
				Task: purged.ID, Project: purged.Project, WaitingOnAdd: []string{other.ID},
			}, fixedLeads{project: "eng-lead"}); err != nil {
				r.t.Fatalf("depend: %v", err)
			}
		},
		"a task linked to it": func(r *roundTrip, purged, other worklet.Task) {
			r.relate(other.ID, purged.ID, worklet.RelationLinked)
		},
		"a task referencing it": func(r *roundTrip, purged, other worklet.Task) {
			body := "see " + purged.Key + " for the history"
			if _, err := r.writer.UpdateTask(r.t.Context(), "op-body", other.ID, other.Project,
				worklet.NoIfMatch, worklet.TaskPatch{Body: &body}, worklet.ChangeFields, nil); err != nil {
				r.t.Fatalf("reference: %v", err)
			}
		},
		"its child": func(r *roundTrip, purged, other worklet.Task) {
			if _, err := r.writer.UpdateTask(r.t.Context(), "op-parent", other.ID, other.Project,
				worklet.NoIfMatch, worklet.TaskPatch{Parent: &purged.ID}, worklet.ChangeReparented, nil); err != nil {
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
				// Filed in another project, where the scope must name it.
				seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
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

			operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
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
// Every task's derived rows are re-derived from its own document on every
// record about it, so a purge must fix the documents, not only the rows: else
// the next edit restores a purged blocker (open for ever, since a blocker
// with no row reads as open), a parent pointer to nothing, or the purged task
// among a blocker's dependents.
func TestAPurgesEffectOnAnotherTaskOutlivesThatTasksNextEdit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	purged := r.createTask("to be purged")
	dependent := r.createTask("waits on it")
	blocker := r.createTask("it waits on")
	child := r.createTask("under it")
	for _, change := range []worklet.DependencyChange{
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
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &purged.ID}, worklet.ChangeReparented, nil); err != nil {
		t.Fatalf("parent: %v", err)
	}
	r.drain()

	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", purged.ID, purged.Project,
		"a duplicate import"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	// The next edit to each re-derives its rows from its document.
	for _, id := range []string{dependent.ID, blocker.ID, child.ID} {
		title := "edited after the purge"
		if _, err := r.writer.UpdateTask(t.Context(), "op-edit-"+id, id, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil); err != nil {
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
		func(rel worklet.Relation) bool { return rel.Other == purged.ID }) {
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

// A PURGE REWRITES A TASK IT REACHES TWICE ONCE, WITH BOTH EFFECTS.
//
// A task can be reached by two effects (a subtask its epic waits on is child
// and blocker). Each rewrite stamps the task at the purge's position, and a
// second rewrite from the same record reads that stamp as a redelivery and
// writes nothing — so rewriting effect by effect drops the second effect.
func TestAPurgeRewritesATaskItReachesTwiceOnceWithBothEffects(t *testing.T) {
	t.Parallel()
	for name, stage := range map[string]func(r *roundTrip, purged, other worklet.Task){
		"a subtask its epic waits on": func(r *roundTrip, purged, other worklet.Task) {
			r.fileUnder(other, purged)
			r.waitOn(purged, other)
		},
		"a blocker linked to the task that waits on it": func(r *roundTrip, purged, other worklet.Task) {
			r.waitOn(purged, other)
			r.relate(other.ID, purged.ID, worklet.RelationLinked)
		},
		"a child linked to its parent": func(r *roundTrip, purged, other worklet.Task) {
			r.fileUnder(other, purged)
			r.relate(other.ID, purged.ID, worklet.RelationLinked)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			purged := r.createTask("to be purged")
			other := r.createTask("reached twice")
			stage(r, purged, other)

			operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
			if _, err := operator.PurgeTask(t.Context(), "op-purge", purged.ID, purged.Project,
				"a duplicate import"); err != nil {
				t.Fatalf("purge: %v", err)
			}
			r.drain()
			r.assertFreeOf(other.ID, purged.ID, "after the purge")

			// And after its next edit, which re-derives every row from
			// the document.
			title := "edited after the purge"
			if _, err := r.writer.UpdateTask(t.Context(), "op-edit", other.ID, other.Project,
				worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil); err != nil {
				t.Fatalf("edit: %v", err)
			}
			r.drain()
			r.assertFreeOf(other.ID, purged.ID, "after its next edit")
		})
	}
}

// fileUnder makes child a direct child of parent.
func (r *roundTrip) fileUnder(child, parent worklet.Task) {
	r.t.Helper()
	if _, err := r.writer.UpdateTask(r.t.Context(), "op-parent-"+child.ID, child.ID, child.Project,
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &parent.ID}, worklet.ChangeReparented, nil); err != nil {
		r.t.Fatalf("file %s under %s: %v", child.ID, parent.ID, err)
	}
	r.drain()
}

// waitOn makes dependent wait on blocker, both halves of the edge.
func (r *roundTrip) waitOn(dependent, blocker worklet.Task) {
	r.t.Helper()
	if _, err := r.writer.Depend(r.t.Context(), "op-depend-"+dependent.ID+"-"+blocker.ID,
		worklet.DependencyChange{
			Task: dependent.ID, Project: dependent.Project, WaitingOnAdd: []string{blocker.ID},
		}, fixedLeads{project: "eng-lead"}); err != nil {
		r.t.Fatalf("%s waits on %s: %v", dependent.ID, blocker.ID, err)
	}
	r.drain()
}

// assertFreeOf fails unless task names gone nowhere — not in its document's
// parent, relations or dependents, and not in any row derived from them.
func (r *roundTrip) assertFreeOf(task, gone, when string) {
	r.t.Helper()
	held := oneTask(r.t, r, task)
	if held.Parent != nil {
		r.t.Errorf("%s the task's document names parent %q, want the purged task's "+
			"own parent (none)", when, *held.Parent)
	}
	if slices.ContainsFunc(held.Relations, func(rel worklet.Relation) bool { return rel.Other == gone }) {
		r.t.Errorf("%s the task's document still relates to the purged task: %+v",
			when, held.Relations)
	}
	if slices.Contains(held.Dependents, gone) {
		r.t.Errorf("%s the task's document still lists the purged task among its "+
			"dependents: %v", when, held.Dependents)
	}
	for _, query := range []string{
		`SELECT COALESCE(parent_id, '') FROM tracker_tasks WHERE id = ? AND parent_id IS NOT NULL`,
		`SELECT other_id FROM tracker_relations WHERE task_id = ?`,
		`SELECT blocker_id FROM tracker_task_deps WHERE task_id = ?`,
		`SELECT dependent_id FROM tracker_task_dependents WHERE task_id = ?`,
		`SELECT ancestor_id FROM tracker_task_closure WHERE descendant_id = ? AND distance > 0`,
	} {
		if got := r.strings(query, task); len(got) != 0 {
			r.t.Errorf("%s %q answers %v for the task, want nothing", when, query, got)
		}
	}
}

// A PURGE NAMING ANOTHER PROJECT IS REFUSED, AND PURGES NOTHING.
//
// Its record is filed and probed under the project the caller named, so a
// wrong one escapes a record deferred on the task's real project.
func TestAPurgeNamingAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	here := r.createTask("filed in ENG")
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})

	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	_, err := operator.PurgeTask(t.Context(), "op-purge-ops", here.ID, "OPS", "a duplicate import")
	if err == nil || !strings.Contains(err.Error(), "is in project ENG, not OPS") {
		t.Fatalf("a purge naming OPS for a task in ENG answered %v, want it refused "+
			"naming the project the task is in", err)
	}
	r.drain()
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE id = ?`, here.ID); len(got) != 1 {
		t.Fatal("a purge refused for naming the wrong project purged the task")
	}
}

// A PURGE WHOSE REACH GREW AFTER ITS SCOPE WAS READ IS REFUSED, NAMING THE TASK.
//
// The scope is read before the request, since the publisher probes with it,
// but the decide may run on newer rows (the retry after a peer's write on the
// purged task's subject). A dependent that arrived in between is unnamed, so
// the decide must refuse rather than widen a scope already probed with.
func TestAPurgeWhoseReachGrewAfterItsScopeWasReadIsRefused(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	purged := a.createTask("to be purged")
	dependent := a.createTask("waits on it")

	// A caught-up peer on the same log makes the dependent wait — both
	// halves, the second on the purged task's own subject.
	peerDBNode, peerDB := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "peer.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = peerDBNode.Close() })
	peer := newRoundTripOn(t, a.broker, a.log, peerDBNode, peerDB, "node-b")
	peer.drain()
	peer.applyWhileWriting()
	peer.waitOn(dependent, purged)

	// This node has not applied it, so its scope names the purged task
	// alone; its decide waits for the peer's append and then sees it.
	if got := a.strings(`SELECT task_id FROM tracker_task_deps WHERE blocker_id = ?`,
		purged.ID); len(got) != 0 {
		t.Fatalf("the premise: this node already holds the dependent %v", got)
	}
	a.applyWhileWriting()
	operator := a.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	_, err := operator.PurgeTask(t.Context(), "op-purge", purged.ID, purged.Project,
		"a duplicate import")
	if !errors.Is(err, statelog.ErrConflict) || !strings.Contains(err.Error(), dependent.ID) {
		t.Fatalf("a purge whose reach grew to %s after its scope was read answered %v, "+
			"want a conflict naming it", dependent.ID, err)
	}
	a.drain()
	if got := a.strings(`SELECT id FROM tracker_tasks WHERE id = ?`, purged.ID); len(got) != 1 {
		t.Fatal("the refused purge purged the task")
	}
}
