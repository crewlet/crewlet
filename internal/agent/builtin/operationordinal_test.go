package builtin_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY WRITE A TURN MAKES TWICE TO ONE OBJECT IS NAMED TWICE, AND A
// REDELIVERY NAMES EACH THE SAME WAY AGAIN.
//
// operation_test.go holds the rule for an update, a comment and a create. These
// hold it for every other write that derives its name from the calls before it
// in the turn — a merge, a project's tags and policy, a dependency and a label
// declaration — each through the surface a phase runs it on, because that is
// the frame that hands a call the calls before it. A write named like an
// earlier one in its turn is answered `applied` by the ledger and dropped.

// twice runs calls on a surface bound to one attempt at the unit of work
// "wk-1", failing the test on a refusal unless the case expects one.
func twice(t *testing.T, reg *tools.Registry, runID, tool string, calls ...map[string]any) {
	t.Helper()
	s := onSurface(reg, attemptOf(t, runID))
	for i, args := range calls {
		if got := execute(t, s, tool, args); got.Failed {
			t.Fatalf("%s call %d failed: %s", tool, i+1, got.Output)
		}
	}
}

// sameAgain asserts a redelivery named its writes as its first attempt did.
func sameAgain(t *testing.T, first, again []string) {
	t.Helper()
	if !slices.Equal(again, first) {
		t.Errorf("the redelivery wrote under %v where its first attempt wrote "+
			"under %v — each write lands twice", again, first)
	}
}

// Two merges of one item in one turn.
//
// Mutation: name a merge its base whatever came before it in the turn, and
// the second is named like the first.
func TestTwoMergesOfOneItemInOneTurnAreTwoWrites(t *testing.T) {
	t.Parallel()
	merged := func(runID string) []string {
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Merges: trk.merges})
		twice(t, reg, runID, builtin.MergeWorkItemTool,
			map[string]any{"item": "ENG-1", "into": "ENG-2"},
			map[string]any{"item": "ENG-1", "into": "ENG-3"})
		return trk.mergeOps
	}
	first := merged("run-1")
	if want := []string{"wk-1-merge-i1", "wk-1-merge-i1#2"}; !slices.Equal(first, want) {
		t.Errorf("two merges wrote under %v, want %v", first, want)
	}
	sameAgain(t, first, merged("run-2"))
}

// A WRITE THAT NAMED ITS OPERATION AND DID NOT LAND IS COUNTED, on the writes
// that name one operation as on those that name several: its record may have
// landed on an earlier attempt that lost the answer, and a redelivery of that
// attempt names its next write after it.
//
// Mutation: answer a failed merge, or a failed tag edit, without the operation
// it named, and the next one is named like it.
func TestAFailedMergeOrProjectWriteIsCounted(t *testing.T) {
	t.Parallel()
	t.Run("a merge", func(t *testing.T) {
		trk := newFakeTracker()
		s := onSurface(workRegistry(t, builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Merges: trk.merges,
		}), attemptOf(t, "run-1"))
		trk.writeErr = errors.New("no response from stream")
		if got := execute(t, s, builtin.MergeWorkItemTool,
			map[string]any{"item": "ENG-1", "into": "ENG-2"}); !got.Failed {
			t.Fatalf("the failing merge reported success: %s", got.Output)
		}
		trk.writeErr = nil
		if got := execute(t, s, builtin.MergeWorkItemTool,
			map[string]any{"item": "ENG-1", "into": "ENG-2"}); got.Failed {
			t.Fatalf("merge failed: %s", got.Output)
		}
		if want := []string{"wk-1-merge-i1", "wk-1-merge-i1#2"}; !slices.Equal(trk.mergeOps, want) {
			t.Errorf("the merges were handed %v, want %v", trk.mergeOps, want)
		}
	})
	t.Run("a tag edit", func(t *testing.T) {
		trk := newFakeTracker()
		s := onSurface(projectRegistryIn(t, trk, leadAlways, ""), attemptOf(t, "run-1"))
		tag := map[string]any{"project": "ENG",
			"tags_add": []any{map[string]any{"slug": "regression"}}}
		trk.writeErr = errors.New("no response from stream")
		if got := execute(t, s, tracker.WriteProjectTool, tag); !got.Failed {
			t.Fatalf("the failing tag edit reported success: %s", got.Output)
		}
		trk.writeErr = nil
		if got := execute(t, s, tracker.WriteProjectTool, tag); got.Failed {
			t.Fatalf("tag edit failed: %s", got.Output)
		}
		if want := []string{"wk-1-tags-ENG#2"}; !slices.Equal(trk.opIDs, want) {
			t.Errorf("the tag edit after a failed one landed under %v, want %v",
				trk.opIDs, want)
		}
	})
}

// Two project writes in one turn, each with a tag half and a policy half: each
// half is its own write on its own subject, counted under its own verb.
//
// Mutation: name either half its base whatever came before it, or count the
// two halves under one verb, and a name repeats or skips.
func TestTwoProjectWritesInOneTurnAreTwoOfEachHalf(t *testing.T) {
	t.Parallel()
	written := func(runID string) []string {
		trk := newFakeTracker()
		twice(t, projectRegistryIn(t, trk, leadAlways, ""), runID, tracker.WriteProjectTool,
			map[string]any{"project": "ENG", "archived": false,
				"tags_add": []any{map[string]any{"slug": "regression"}}},
			map[string]any{"project": "ENG", "archived": true,
				"tags_add": []any{map[string]any{"slug": "flaky"}}})
		return trk.opIDs
	}
	first := written("run-1")
	want := []string{"wk-1-tags-ENG", "wk-1-policy-ENG", "wk-1-tags-ENG#2", "wk-1-policy-ENG#2"}
	if !slices.Equal(first, want) {
		t.Errorf("two project writes wrote under %v, want %v", first, want)
	}
	sameAgain(t, first, written("run-2"))
}

// Two dependency changes to one item in one turn, and a dependency each of two
// creates writes on the task it filed.
//
// Mutation: name a dependency its base whatever came before it, and the
// update's second is named like its first.
func TestTwoDependencyWritesInOneTurnAreTwoWrites(t *testing.T) {
	t.Parallel()
	t.Run("an update", func(t *testing.T) {
		depended := func(runID string) []string {
			trk := newFakeTracker()
			twice(t, workRegistry(t, builtin.WorkDeps{
				Reader: trk, Writer: trk.as, Dependencies: trk.depends,
			}), runID, builtin.UpdateWorkItemTool,
				map[string]any{"item": "ENG-1",
					"waiting_on": map[string]any{"add": []any{"ENG-2"}}},
				map[string]any{"item": "i1",
					"waiting_on": map[string]any{"add": []any{"ENG-3"}}})
			return trk.dependOps
		}
		first := depended("run-1")
		if want := []string{"wk-1-depend-i1", "wk-1-depend-i1#2"}; !slices.Equal(first, want) {
			t.Errorf("two dependency changes wrote under %v, want %v", first, want)
		}
		sameAgain(t, first, depended("run-2"))
	})
	t.Run("a create", func(t *testing.T) {
		filed := func(runID string) (depends, tasks []string) {
			trk := newFakeTracker()
			twice(t, workRegistry(t, builtin.WorkDeps{
				Reader: trk, Writer: trk.as, Dependencies: trk.depends,
			}), runID, builtin.CreateWorkItemTool,
				map[string]any{"title": "follow up", "project": "ENG",
					"waiting_on": []any{"ENG-2"}},
				map[string]any{"title": "follow up", "project": "ENG",
					"waiting_on": []any{"ENG-2"}})
			for _, task := range trk.created {
				tasks = append(tasks, task.ID)
			}
			return trk.dependOps, tasks
		}
		first, tasks := filed("run-1")
		if len(tasks) != 2 || len(first) != 2 {
			t.Fatalf("two creates filed %v with dependencies %v", tasks, first)
		}
		for i, op := range first {
			if op != "wk-1-depend-"+tasks[i] {
				t.Errorf("create %d's dependency was written under %q, want "+
					"wk-1-depend-%s", i+1, op, tasks[i])
			}
		}
		again, _ := filed("run-2")
		sameAgain(t, first, again)
	})
}

// Two creates in one turn that each declare a label in one project.
//
// Mutation: name a declaration its base whatever came before it, and the
// second is named like the first.
func TestTwoLabelDeclarationsInOneTurnAreTwoWrites(t *testing.T) {
	t.Parallel()
	declared := func(runID string) []string {
		trk := newFakeTracker()
		twice(t, projectRegistryIn(t, trk, nil, "ENG"), runID, builtin.CreateWorkItemTool,
			map[string]any{"title": "A task", "labels": []any{"regression"},
				"labels_create_missing": true},
			map[string]any{"title": "Another", "labels": []any{"flaky"},
				"labels_create_missing": true})
		var labels []string
		for _, op := range trk.opIDs {
			if strings.Contains(op, "-labels-") {
				labels = append(labels, op)
			}
		}
		return labels
	}
	first := declared("run-1")
	if want := []string{"wk-1-labels-ENG", "wk-1-labels-ENG#2"}; !slices.Equal(first, want) {
		t.Errorf("two label declarations wrote under %v, want %v", first, want)
	}
	sameAgain(t, first, declared("run-2"))
}
