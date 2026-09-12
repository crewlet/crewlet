package tracker_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// FOLDING ONE ITEM INTO ANOTHER IS THREE COMMITS AND ALL OF THEM LAND.
//
// The sequence had no caller and no test at all, which is why the shape of its
// second append — the duplicate's own subject, written again after the mark —
// was never exercised. What it has to leave behind is one state: the duplicate
// cancelled and linked to the survivor, its children under the survivor, and
// nothing under a closed parent.
func TestAMergeClosesTheDuplicateAndMovesItsChildren(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS, because a fold writes the duplicate's own subject
	// twice and its close waits for its mark. See applyWhileWriting.
	r.applyWhileWriting()
	inSprint(t, r, "keep", nil)
	inSprint(t, r, "dup", nil)

	parent := "dup"
	child := newTask("kid")
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()

	dup := r.task(t, "dup")
	if dup.Task.Status != tracker.StatusCancelled {
		t.Errorf("the duplicate is %q and a fold closes it", dup.Task.Status)
	}
	if !slices.ContainsFunc(dup.Task.Relations, func(rel tracker.Relation) bool {
		return rel.Kind == tracker.RelationDuplicates && rel.Other == "keep"
	}) {
		t.Errorf("the duplicate carries %+v and names nothing it duplicates — "+
			"a closed item with no link is one nobody can follow to the work",
			dup.Task.Relations)
	}
	// THE CHILD IS THE HALF NO PATCH CAN DO. A seat can cancel and link by
	// hand; it cannot move the subtree, and a subtask left under a closed
	// parent is work that disappears with it.
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Fatalf("the child's parent is %q, and the item it was under is "+
			"closed", got)
	}
	// AND THE MERGE FLAG IS CLEARED. It is raised by the mark and lowered
	// by the close, so an item left `merging` is a fold that stopped
	// halfway — which is exactly what a reader must be able to tell from
	// one that finished.
	if dup.Task.Merging {
		t.Error("the duplicate is still marked merging after the fold closed it")
	}
}

// AND THE CHILDREN STAY WHERE THEY ARE WHEN NOBODY ASKED TO MOVE THEM.
func TestAMergeLeavesTheChildrenWhenItIsNotAskedToReparent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS, because a fold writes the duplicate's own subject
	// twice and its close waits for its mark. See applyWhileWriting.
	r.applyWhileWriting()
	inSprint(t, r, "keep", nil)
	inSprint(t, r, "dup", nil)

	parent := "dup"
	child := newTask("kid")
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
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
// The pre-flight is what makes that answerable: both ends are read before the
// first append, so a fold that cannot complete refuses whole rather than
// leaving the duplicate marked `merging` with nothing to finish it.
func TestAMergeRefusesARemovedItemAndSaysToRestoreIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS, because a fold writes the duplicate's own subject
	// twice and its close waits for its mark. See applyWhileWriting.
	r.applyWhileWriting()
	inSprint(t, r, "keep", nil)
	inSprint(t, r, "dup", nil)

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "dup", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep", true, nil)
	if err == nil {
		t.Fatal("a removed item was folded")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Errorf("the refusal is %q and does not name the move that would "+
			"make the fold possible", err)
	}
}

// parentOf is a task's parent, or "" where it has none.
func parentOf(detail tracker.TaskDetail) string {
	if detail.Task.Parent == nil {
		return ""
	}
	return *detail.Task.Parent
}
