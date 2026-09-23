package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SUBTASK IS FILED IN ITS PARENT'S PROJECT, and under a parent that is not
// in the trash.
//
// A board draws a project's roots and lets their subtrees ride along, with the
// container on the outer row too — so a subtask in another project from its
// root is on NEITHER board. Nothing refused it: the create checked the project
// it was filed in and never looked at the parent, and the create tool
// defaulted a subtask to the CALLER's team, so a seat breaking down a
// colleague's item scattered its pieces into its own project. And a child
// filed under a removed parent is a live row under a parent nobody can see,
// which a restore does not bring back with it.
func TestASubtaskIsFiledInItsParentsProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	if _, err := r.writer.CreateTask(t.Context(), "op-root",
		newTask("p-root"), nil); err != nil {
		t.Fatalf("CreateTask root: %v", err)
	}
	r.drain()

	parent := "p-root"
	elsewhere := newTask("p-elsewhere")
	elsewhere.Project, elsewhere.Parent = "OPS", &parent
	_, err := r.writer.CreateTask(t.Context(), "op-elsewhere", elsewhere, nil)
	switch {
	case err == nil:
		t.Fatal("a subtask of an ENG item was filed under OPS, where neither " +
			"project's board draws it")
	case !strings.Contains(err.Error(), "ENG"):
		t.Errorf("the refusal %q does not name the parent's project, which is "+
			"the one the caller has to file under", err)
	}
	r.drain()

	home := newTask("p-home")
	home.Parent = &parent
	if _, err := r.writer.CreateTask(t.Context(), "op-home", home, nil); err != nil {
		t.Fatalf("a subtask filed in its parent's project was refused: %v", err)
	}
	r.drain()
	if got := oneTask(t, r, "p-home"); got.Parent == nil || *got.Parent != parent {
		t.Errorf("the subtask's parent is %v, want p-root", got.Parent)
	}

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "p-root", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	late := newTask("p-late")
	late.Parent = &parent
	if _, err := r.writer.CreateTask(t.Context(), "op-late", late, nil); err == nil ||
		!strings.Contains(err.Error(), "restore") {
		t.Errorf("a subtask filed under a parent in the trash = %v, want a "+
			"refusal saying to restore the parent first", err)
	}
}

// A MERGE ACROSS PROJECTS THAT WOULD MOVE THE SUBTASKS IS REFUSED BEFORE THE
// MARK.
//
// Every re-parent it would publish is refused on its own subject, since a
// subtask lives in its parent's project — so a merge that marked the
// duplicate first would leave a mark nothing can finish, and the duty that
// completes an abandoned merge would retry it on every tick for ever. Without
// moving the subtasks the fold is fine: they stay where they are.
func TestAMergeAcrossProjectsCannotMoveTheSubtasks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// A MERGE'S CLOSE WAITS FOR ITS MARK, so this node has to apply while
	// the sequence writes. See applyWhileWriting.
	r.applyWhileWriting()
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	keep := newTask("m-keep")
	keep.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-keep", keep, nil); err != nil {
		t.Fatalf("CreateTask keep: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-dup", newTask("m-dup"), nil); err != nil {
		t.Fatalf("CreateTask dup: %v", err)
	}
	r.drain()
	dup := "m-dup"
	kid := newTask("m-kid")
	kid.Parent = &dup
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "m-dup", "m-keep",
		true, nil)
	if err == nil || !strings.Contains(err.Error(), "subtasks") {
		t.Fatalf("a cross-project merge that moves the subtasks = %v, want a "+
			"refusal naming the subtasks", err)
	}
	r.drain()
	if got := oneTask(t, r, "m-dup"); got.Merging {
		t.Error("the refused merge marked the duplicate anyway, and nothing " +
			"can ever finish that mark")
	}

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge-stay", "m-dup",
		"m-keep", false, nil); err != nil {
		t.Fatalf("a cross-project merge that leaves the subtasks = %v", err)
	}
	r.drain()
	if got := parentOf(r.task(t, "m-kid")); got != "m-dup" {
		t.Errorf("the subtask's parent is %q after a merge told to leave it", got)
	}
}

// A RE-PARENT IS HELD TO THE SAME RULE, at the write — which is the only other
// gesture that sets a parent.
func TestAReparentIntoAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	other := newTask("r-other")
	other.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-other", other, nil); err != nil {
		t.Fatalf("CreateTask other: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-mine", newTask("r-mine"), nil); err != nil {
		t.Fatalf("CreateTask mine: %v", err)
	}
	r.drain()

	onto := "r-other"
	_, err := r.writer.UpdateTask(t.Context(), "op-reparent", "r-mine", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &onto},
		tracker.ChangeReparented, nil)
	if err == nil {
		t.Fatal("an ENG item was re-parented under an OPS one")
	}
	if errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("the refusal %v says to come back, and waiting changes "+
			"nothing about which project either item is in", err)
	}
	r.drain()
	if got := oneTask(t, r, "r-mine"); got.Parent != nil {
		t.Errorf("the item took parent %v across projects", *got.Parent)
	}
}

// A RECORD THAT CARRIES THE SHAPE ANYWAY IS APPLIED AND FLAGGED.
//
// The writer refuses it, and the applier cannot: a record the broker committed
// is one every node must consume, and refusing it at apply stalls the log on
// every node over one task. `inconsistent_project` is the attention flag that
// names it, and until now nothing ever raised it.
func TestASubtaskInAnotherProjectFromItsRootIsFlagged(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	root := newTask("f-root")
	if _, err := h.apply(taskRecord("f-root", tracker.OpCreate, root, nil), at); err != nil {
		t.Fatalf("create the root: %v", err)
	}
	parent := "f-root"
	for _, c := range []struct{ id, project string }{
		{"f-home", "ENG"}, {"f-away", "OPS"},
	} {
		child := newTask(c.id)
		child.Key, child.Project, child.Parent, child.Depth = c.project+"-9", c.project, &parent, 1
		if _, err := h.apply(taskRecord(c.id, tracker.OpCreate, child, nil), at); err != nil {
			t.Fatalf("create %s: %v", c.id, err)
		}
	}
	flag := func(id string) int64 {
		t.Helper()
		return h.value(`SELECT inconsistent_project FROM tracker_tasks WHERE id = ?`, id)
	}
	if flag("f-away") != 1 {
		t.Error("a subtask in OPS under an ENG root carries no " +
			"inconsistent_project, so the attention set never names it")
	}
	if flag("f-home") != 0 || flag("f-root") != 0 {
		t.Error("a subtask in its root's project, or the root itself, is " +
			"flagged inconsistent")
	}
}
