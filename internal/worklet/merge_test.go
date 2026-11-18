package worklet_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// FOLDING ONE ITEM INTO ANOTHER IS THREE COMMITS AND ALL OF THEM LAND.
//
// What it leaves is one state: the duplicate cancelled and linked to the
// survivor, its children under the survivor, nothing under a closed parent.
func TestAMergeClosesTheDuplicateAndMovesItsChildren(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS: a fold's close waits for its mark (applyWhileWriting).
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	parent := "dup"
	child := newTask("kid")
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	under := "kid"
	grand := newTask("grandkid")
	grand.Parent, grand.Depth = &under, 2
	if _, err := r.writer.CreateTask(t.Context(), "op-grand", grand, nil); err != nil {
		t.Fatalf("CreateTask grandkid: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()

	dup := r.task(t, "dup")
	if dup.Task.Status != worklet.StatusCancelled {
		t.Errorf("the duplicate is %q and a fold closes it", dup.Task.Status)
	}
	if !slices.ContainsFunc(dup.Task.Relations, func(rel worklet.Relation) bool {
		return rel.Kind == worklet.RelationDuplicates && rel.Other == "keep"
	}) {
		t.Errorf("the duplicate carries %+v and names nothing it duplicates — "+
			"a closed item with no link is one nobody can follow to the work",
			dup.Task.Relations)
	}
	// THE CHILD IS THE HALF NO PATCH CAN DO: a subtask left under a closed
	// parent disappears with it.
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Fatalf("the child's parent is %q, and the item it was under is "+
			"closed", got)
	}
	// AND ONLY THE SUBTASKS MOVE, as `move_subtasks` promises: a grandchild
	// stays under its parent, since flattening the subtree cannot be undone.
	if got := parentOf(r.task(t, "grandkid")); got != "kid" {
		t.Errorf("the grandchild's parent is %q, want \"kid\" — the merge "+
			"flattened the subtree onto the survivor rather than moving the "+
			"subtasks it was asked to move", got)
	}
	// AND THE MERGE FLAG IS CLEARED: an item left `merging` is a fold that
	// stopped halfway, which a reader must tell from one that finished.
	if dup.Task.Merging {
		t.Error("the duplicate is still marked merging after the fold closed it")
	}
	// AND SO IS THE INTENT IT CARRIED: it describes a running walk, and the
	// close names only the marker, so the two go down together or drift on
	// the first completed merge.
	if dup.Task.MergeReparent {
		t.Error("the finished merge left its re-parent intent standing on the " +
			"task, which no later marker on that task is the author of")
	}
}

// AND THE CHILDREN STAY WHERE THEY ARE WHEN NOBODY ASKED TO MOVE THEM.
func TestAMergeLeavesTheChildrenWhenItIsNotAskedToReparent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS: a fold's close waits for its mark (applyWhileWriting).
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	parent := "dup"
	child := newTask("kid")
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	under := "kid"
	grand := newTask("grandkid")
	grand.Parent, grand.Depth = &under, 2
	if _, err := r.writer.CreateTask(t.Context(), "op-grand", grand, nil); err != nil {
		t.Fatalf("CreateTask grandkid: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		false, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()

	if got := parentOf(r.task(t, "kid")); got != "dup" {
		t.Errorf("the child moved to %q although the fold was told to leave it",
			got)
	}
}

// A REMOVED ITEM IS NOT FOLDED, and the refusal says what to do about it.
//
// Both ends are read before the first append, so a fold that cannot complete
// refuses whole rather than leaving the duplicate marked `merging`.
func TestAMergeRefusesARemovedItemAndSaysToRestoreIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS: a fold's close waits for its mark (applyWhileWriting).
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "dup", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep", true, nil)
	if err == nil {
		t.Fatal("a removed item was folded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "restore") {
		t.Errorf("the refusal is %q and does not name the move that would "+
			"make the fold possible", err)
	}
}

// A MERGE WITH A SUBTASK IN THE TRASH FINISHES, and leaves that subtask where
// a restore will look for it.
//
// A tombstoned task refuses every write, so a walk that selects it would stop
// there on every re-run, leaving the duplicate open and marked for good.
func TestAMergeWithASubtaskInTheTrashFinishes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS: a fold's close waits for its mark (applyWhileWriting).
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	for _, id := range []string{"kid-live", "kid-trashed"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "kid-trashed", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("a merge with a subtask in the trash: %v", err)
	}
	r.drain()
	if dup := r.task(t, "dup"); dup.Task.Merging ||
		dup.Task.Status != worklet.StatusCancelled {
		t.Errorf("the duplicate is %q, merging %v", dup.Task.Status, dup.Task.Merging)
	}
	if got := parentOf(r.task(t, "kid-live")); got != "keep" {
		t.Errorf("the live subtask's parent is %q, want keep", got)
	}
	if got := parentOf(r.task(t, "kid-trashed")); got != "dup" {
		t.Errorf("the removed subtask's parent is %q — a frozen task was "+
			"written to", got)
	}
}

// parentOf is a task's parent, or "" where it has none.
func parentOf(detail worklet.TaskDetail) string {
	if detail.Task.Parent == nil {
		return ""
	}
	return *detail.Task.Parent
}
