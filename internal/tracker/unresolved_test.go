package tracker_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A MOVE STOPS AT A DESCENDANT WHOSE STEP IS UNKNOWN: the ones after it stay
// where they are, the root keeps its mark, and the caller is told the walk
// stopped under which operation — never that it succeeded.
//
// The step's append lands and its answer is lost, and so is the probe's, so
// the write answers `unknown` with a nil error. The walk checked only the
// error: it carried on to the next descendant, took the mark down over a task
// that might still be in the old project, and reported the move done.
func TestAMoveStopsAtAnUnknownDescendant(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b", "m-kid-c")
	lossy, lost := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "move")

	lost.dropFor("m-kid-b")
	_, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if !errors.Is(err, tracker.ErrStepUnresolved) || !strings.Contains(err.Error(), op) {
		t.Fatalf("a move with an unknown descendant step = %v, want an error "+
			"wrapping ErrStepUnresolved that names operation %s", err, op)
	}
	r.drain()
	if oneTask(t, r, "m-kid-c").Project != "ENG" {
		t.Error("the walk carried on past the unknown step and moved m-kid-c")
	}
	if !oneTask(t, r, "m-root").Moving {
		t.Fatal("the mark came down over a walk that stopped at an unresolved step")
	}

	// THE RE-RUN UNDER THE SAME OPERATION finishes it: m-kid-b's step is
	// answered by what landed, and the rest follows.
	retry, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the re-run of the stopped move: %v", err)
	}
	r.drain()
	if retry.Outcome != statelog.OutcomeApplied {
		t.Errorf("the re-run answered %q, want applied", retry.Outcome)
	}
	for _, id := range []string{"m-kid-a", "m-kid-b", "m-kid-c"} {
		if got := oneTask(t, r, id); got.Project != "OPS" {
			t.Errorf("%s is in %q after the re-run, want OPS", id, got.Project)
		}
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("the re-run finished the walk and left the mark up")
	}
}

// A MOVE WHOSE ROOT STEP IS UNKNOWN MOVES NO DESCENDANT.
func TestAMoveStopsAtAnUnknownRoot(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a")
	lossy, lost := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "move")

	lost.dropFor("m-root")
	_, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if !errors.Is(err, tracker.ErrStepUnresolved) {
		t.Fatalf("a move with an unknown root step = %v, want ErrStepUnresolved", err)
	}
	r.drain()
	if oneTask(t, r, "m-kid-a").Project != "ENG" {
		t.Error("a descendant followed a root whose own move is unknown")
	}
}

// A MERGE STOPS AT A SUBTASK WHOSE RE-PARENT IS UNKNOWN, and never closes the
// duplicate over it: the close takes the merge marker down, and a marker taken
// down over a subtask still under the duplicate is a merge nothing finishes.
func TestAMergeStopsAtAnUnknownReparent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	for _, id := range []string{"kid-a", "kid-b"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
	}
	lossy, lost := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "merge")

	lost.dropFor("kid-a")
	_, err := lossy.MergeDuplicates(t.Context(), op, "dup", "keep", true, nil)
	if !errors.Is(err, tracker.ErrStepUnresolved) {
		t.Fatalf("a merge with an unknown re-parent = %v, want ErrStepUnresolved", err)
	}
	r.drain()
	dup := r.task(t, "dup")
	if !dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
		t.Errorf("the duplicate is %q, merging %v — closed over a re-parent "+
			"nobody can vouch for", dup.Task.Status, dup.Task.Merging)
	}
	if kid := oneTask(t, r, "kid-b"); kid.Parent == nil || *kid.Parent != "dup" {
		t.Error("the walk carried on past the unknown re-parent")
	}
}

// A DEPENDENCY WHOSE AUTHORED EDGE IS UNKNOWN WRITES NO MIRROR. A mirror over
// an edge that may not exist is a blocker listing a dependent whose own edge is
// missing — the one residue no scan can find.
func TestADependencyStopsBeforeItsMirrorWhenTheEdgeIsUnknown(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	lossy, lost := r.lossyWriter(t)

	lost.dropFor("dep")
	_, err := lossy.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"})
	if !errors.Is(err, tracker.ErrStepUnresolved) {
		t.Fatalf("a dependency with an unknown authored edge = %v, want "+
			"ErrStepUnresolved", err)
	}
	r.drain()
	if blk := r.task(t, "blk"); slices.Contains(blk.Task.Dependents, "dep") {
		t.Error("the mirror was written over an authored edge nobody can vouch for")
	}
}

// A MIRROR WHOSE OUTCOME IS UNKNOWN IS REPORTED ONE-SIDED, not mirrored.
func TestAnUnknownMirrorIsReportedOneSided(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	lossy, lost := r.lossyWriter(t)

	lost.dropFor("blk")
	got, err := lossy.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"})
	if err != nil {
		t.Fatalf("Depend: %v", err)
	}
	if slices.Contains(got.Mirrored, "blk") || !slices.Contains(got.OneSided, "blk") {
		t.Errorf("an unknown mirror reported mirrored %v, one-sided %v — want "+
			"blk one-sided", got.Mirrored, got.OneSided)
	}
}

// THE MARK'S REMOVAL IS A STEP LIKE ANY OTHER: a move whose last append is
// unknown is not a move that finished.
func TestAMoveWhoseMarkRemovalIsUnknownDoesNotReportSuccess(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a")
	lossy, lost := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "move")

	// THE ROOT'S NEXT APPEND AFTER ITS CHILD MOVED is the mark coming down.
	lost.afterAppendTo("m-kid-a", func() { lost.dropFor("m-root") })
	if _, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil); !errors.Is(err,
		tracker.ErrStepUnresolved) {
		t.Fatalf("a move whose mark removal is unknown = %v, want "+
			"ErrStepUnresolved", err)
	}
}

// A PROMOTION WHOSE SUBTASK IS UNKNOWN MARKS NO ITEM: an item pointing at a
// subtask that may not exist is the struck-through line pointing at nothing
// that the promotion's order exists to prevent.
func TestAPromotionWithAnUnknownSubtaskMarksNoItem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	promotionParent(t, r, "parent-p", "item-1")
	lossy, lost := r.lossyWriter(t)

	lost.dropFor("sub-s")
	_, err := lossy.PromoteItem(t.Context(), statelog.NewOpID(time.Now(), "promote"),
		"parent-p", "item-1", newTask("sub-s"), nil)
	if !errors.Is(err, tracker.ErrStepUnresolved) {
		t.Fatalf("a promotion with an unknown subtask = %v, want ErrStepUnresolved", err)
	}
	r.drain()
	if got := promotedTo(t, r, "parent-p", "item-1"); got != "" {
		t.Errorf("the item points at %q over a subtask nobody can vouch for", got)
	}
}

// THE DUTY DOES NOT COUNT A MERGE WHOSE CLOSE IS UNKNOWN as one it finished.
func TestTheDutyDoesNotCountAnUnknownClose(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"keep", "dup"} {
		filedTask(t, r, id)
	}
	markMerge(t, r, "dup", "keep")
	lossy, lost := r.lossyWriter(t)
	lost.dropFor("dup")
	worker, err := maintenance.New(maintenance.Options{
		Jobs: tracker.Jobs(tracker.DutyDeps{DB: r.db, Writer: lossy, NodeID: "node-a"}),
		Now:  func() time.Time { return wednesday },
	})
	if err != nil {
		t.Fatalf("build the worker: %v", err)
	}
	swept, err := worker.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_merges"]; got != 0 {
		t.Errorf("the duty counted %d merge(s) finished over a close it cannot "+
			"vouch for, want 0", got)
	}
}
