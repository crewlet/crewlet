package worklet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// seedTree files a root and a subtask under it, with the statuses given.
func seedTree(t *testing.T, r *roundTrip, root, sub string,
	rootStatus, subStatus worklet.Status) {

	t.Helper()
	parent := newTask(root)
	parent.Status, parent.StatusGroup = rootStatus, rootStatus.Group()
	if _, err := r.writer.CreateTask(t.Context(), "op-"+root, parent, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", root, err)
	}
	r.drain()

	child := newTask(sub)
	child.Status, child.StatusGroup = subStatus, subStatus.Group()
	child.Parent = &root
	child.Depth = 1
	if _, err := r.writer.CreateTask(t.Context(), "op-"+sub, child, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", sub, err)
	}
	r.drain()
}

// A SUBTREE RIDES ALONG WITH ITS ROOT, and that is what `collapsed` means.
//
// [SubtaskMode]'s own contract: collapsed and expanded filter ROOT tasks and
// let their subtrees ride along unfiltered; separate filters every task on its
// own. It was parsed, defaulted and compiled by nothing, so every query in the
// engine has always behaved as `separate` while the grammar said otherwise.
func TestASubtreeRidesAlongWithItsRoot(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// A TODO ROOT WITH A DONE SUBTASK. Under `collapsed` a filter for
	// todo work brings both; under `separate` it brings the root alone.
	seedTree(t, r, "root-a", "sub-a", worklet.StatusTodo, worklet.StatusDone)

	collapsed := ids(r.ask(map[string]any{
		"container": "project:ENG", "status": "todo", "show_closed": "true",
	}))
	assertIDs(t, collapsed, []string{"root-a", "sub-a"})

	separate := ids(r.ask(map[string]any{
		"container": "project:ENG", "status": "todo",
		"subtasks": "separate", "show_closed": "true",
	}))
	assertIDs(t, separate, []string{"root-a"})

	// EXPANDED IS THE SAME SET as collapsed — the difference between them
	// is a RENDER hint, and a caller folds or does not.
	expanded := ids(r.ask(map[string]any{
		"container": "project:ENG", "status": "todo",
		"subtasks": "expanded", "show_closed": "true",
	}))
	assertIDs(t, expanded, []string{"root-a", "sub-a"})
}

// A SUBTASK WHOSE ROOT DOES NOT MATCH IS OUT, which is the other half of
// "filter ROOT tasks": the mode is a predicate on the tree, not on the row.
func TestASubtaskWhoseRootDoesNotMatchIsOut(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// A DONE ROOT WITH A TODO SUBTASK — the mirror of the case above.
	seedTree(t, r, "root-b", "sub-b", worklet.StatusDone, worklet.StatusTodo)

	collapsed := ids(r.ask(map[string]any{
		"container": "project:ENG", "status": "todo", "show_closed": "true",
	}))
	if len(collapsed) != 0 {
		t.Fatalf("a todo subtask under a done root is in a collapsed answer: "+
			"%v — the mode filters ROOTS, so its tree did not match", collapsed)
	}
	// AND `separate` IS WHERE IT COMES BACK, because that mode filters
	// every task on its own.
	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "status": "todo",
		"subtasks": "separate", "show_closed": "true",
	})), []string{"sub-b"})
}

// ASKING FOR A SUBTREE TURNS THE MODE OFF.
//
// `parent` and `root` are questions ABOUT subtasks, so filtering their roots
// would answer the parent's siblings instead of its children.
func TestAskingForASubtreeTurnsTheModeOff(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedTree(t, r, "root-c", "sub-c", worklet.StatusDone, worklet.StatusTodo)

	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "parent": "root-c", "show_closed": "true",
	})), []string{"sub-c"})

	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "root": "root-c", "show_closed": "true",
	})), []string{"root-c", "sub-c"})
}

// A REMOVED SUBTASK DOES NOT RIDE ALONG.
//
// The tree predicate tests the ROOT's tombstone, so the row's own has to be
// tested out here — or removing a subtask would leave it on every board its
// parent is on.
func TestARemovedSubtaskDoesNotRideAlong(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedTree(t, r, "root-d", "sub-d", worklet.StatusTodo, worklet.StatusTodo)

	// A SECOND SUBTASK, already carrying its tombstone. The write path is
	// what stamps one in production; what is under test here is the
	// PREDICATE that reads it, and the tree predicate tests the ROOT's
	// tombstone — so the row's own has to be tested out here, or removing
	// a subtask would leave it on every board its parent is on.
	parentID := "root-d"
	removed := newTask("sub-gone")
	removed.Key = "ENG-gone"
	removed.Parent = &parentID
	removed.Depth = 1
	removed.Removed = &worklet.Tombstone{
		By: "ana", Kind: worklet.AuthorHuman, At: wednesday,
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-gone", removed, nil); err != nil {
		t.Fatalf("seed the removed subtask: %v", err)
	}
	r.drain()

	assertIDs(t, ids(r.ask(map[string]any{"container": "project:ENG"})),
		[]string{"root-d", "sub-d"})
	// AND THE TRASH STILL REACHES IT, because a removal hides a task
	// rather than destroying it — in every subtask mode, since its
	// tombstone names no other task's removal and so it is a removal of its
	// own, listed on its own in the default mode too.
	for _, mode := range []string{"", "collapsed", "separate"} {
		params := map[string]any{"container": "project:ENG", "removed": "true"}
		if mode != "" {
			params["subtasks"] = mode
		}
		if got := ids(r.ask(params)); !containsString(got, "sub-gone") {
			t.Fatalf("the trash in mode %q is %v and does not hold the removed "+
				"subtask", mode, got)
		}
	}
}

// THE TOTALS AND THE HINT FOLLOW THE MODE.
//
// A header that counted the roots while the board showed their subtrees would
// be a number nobody can reconcile with what is in front of them.
func TestTheHintAndTheTotalsFollowTheSubtaskMode(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedTree(t, r, "root-e", "sub-e", worklet.StatusTodo, worklet.StatusDone)

	collapsed := r.ask(map[string]any{
		"container": "project:ENG", "status": "todo",
		"totals": "tasks:count", "show_closed": "true",
	})
	if collapsed.TotalHint != 2 {
		t.Fatalf("the collapsed hint is %d, want the 2 rows it answered",
			collapsed.TotalHint)
	}
	if len(collapsed.Totals) != 1 || collapsed.Totals[0].Value == nil ||
		*collapsed.Totals[0].Value != 2 {
		t.Fatalf("the collapsed total is %v, want 2", collapsed.Totals)
	}

	separate := r.ask(map[string]any{
		"container": "project:ENG", "status": "todo", "subtasks": "separate",
		"totals": "tasks:count", "show_closed": "true",
	})
	if separate.TotalHint != 1 {
		t.Fatalf("the separate hint is %d, want 1", separate.TotalHint)
	}
}
