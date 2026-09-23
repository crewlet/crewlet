package builtin_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
)

// TWO DIFFERENT WRITES IN ONE TURN ARE TWO OPERATIONS, and the same write asked
// twice is one.
//
// An operation id derived only from the turn, the verb and the object named
// WHICH write this was and not which of two: a turn that moved an item to
// in_progress and later to done, or commented on it twice, wrote both under
// one id. The second was then the first's retry to everything downstream —
// inside the log's duplicate window the broker acknowledged it as the first
// record and the tool answered `applied` for a change that never landed.
func TestTwoDifferentWritesInOneTurnAreTwoOperations(t *testing.T) {
	t.Parallel()

	t.Run("two updates of one item", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "status": "in_progress",
		})
		callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "status": "done",
		})
		// AND THE FIRST ONE AGAIN, which is the executor that asks twice
		// and the re-run: the same call is the same operation.
		callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"status": "in_progress", "item": "ENG-1",
		})
		if len(trk.opIDs) != 3 {
			t.Fatalf("three updates wrote %v", trk.opIDs)
		}
		if trk.opIDs[0] == trk.opIDs[1] {
			t.Errorf("moving an item to in_progress and then to done wrote both "+
				"under %q — the second is collapsed as the first's retry and "+
				"the item never reaches done", trk.opIDs[0])
		}
		if trk.opIDs[0] != trk.opIDs[2] {
			t.Errorf("the same update asked twice wrote under %q and %q — a "+
				"re-run of it would apply it twice", trk.opIDs[0], trk.opIDs[2])
		}
	})

	t.Run("two comments on one item", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		for _, body := range []string{"started on it", "done, see the PR"} {
			if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
				"item": "ENG-1", "body": body,
			}); got.Failed {
				t.Fatalf("comment %q failed: %s", body, got.Output)
			}
		}
		if len(trk.patched) != 2 || len(trk.opIDs) != 2 {
			t.Fatalf("two comments wrote %d patches under %v", len(trk.patched), trk.opIDs)
		}
		first, second := commentOf(t, trk.patched[0]), commentOf(t, trk.patched[1])
		if first.ID == second.ID || trk.opIDs[0] == trk.opIDs[1] {
			t.Errorf("two different remarks share comment %q / operation %q — "+
				"the second upserts the first's row, or never lands at all",
				first.ID, trk.opIDs[0])
		}
	})

	t.Run("two queues set in one turn", func(t *testing.T) {
		t.Parallel()
		person := &personSpy{}
		reg := personSurface(t, newFakeTracker(), person, nil, nil)
		var ids []string
		for _, items := range [][]any{{"ENG-1"}, {"ENG-2", "ENG-1"}} {
			if got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
				"items": items,
			}); got.Failed {
				t.Fatalf("set_priorities %v failed: %s", items, got.Output)
			}
			ids = append(ids, person.opID)
		}
		if ids[0] == ids[1] {
			t.Errorf("a queue and its correction were both written under %q — "+
				"the correction is dropped as a retry of the first", ids[0])
		}
	})
}

// commentOf is the comment a patch carries.
func commentOf(t *testing.T, patch tracker.TaskPatch) *tracker.Comment {
	t.Helper()
	if patch.Comment == nil {
		t.Fatal("the patch carries no comment")
	}
	return patch.Comment
}

// A RE-RUN OF ONE CREATE ADDRESSES THE TASK THE FIRST RUN FILED, and two
// different creates file two.
//
// The new task's id is the subject its create arbitrates on, and it was a
// fresh uuid per call — which was also part of the operation's name — so a
// turn redelivered after it had filed its item filed a second one under a
// second key, and no retry of a create could ever reach the first copy.
func TestARerunCreateAddressesTheTaskItFiled(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	for _, args := range []map[string]any{
		{"title": "wire the applier", "project": "ENG"},
		// THE SAME CALL AGAIN, as the re-run makes it: the arguments a
		// model wrote in whatever order it wrote them.
		{"project": "ENG", "title": "wire the applier"},
		{"title": "write the docs", "project": "ENG"},
	} {
		if got := callWork(t, reg, builtin.CreateWorkItemTool, args); got.Failed {
			t.Fatalf("create %v failed: %s", args, got.Output)
		}
	}
	if len(trk.created) != 3 || len(trk.opIDs) != 3 {
		t.Fatalf("three creates reached the tracker as %d under %v",
			len(trk.created), trk.opIDs)
	}
	first, rerun, other := trk.created[0], trk.created[1], trk.created[2]
	if first.ID != rerun.ID || trk.opIDs[0] != trk.opIDs[1] {
		t.Errorf("a re-run filed task %q under %q after %q under %q — one "+
			"request, two items", rerun.ID, trk.opIDs[1], first.ID, trk.opIDs[0])
	}
	if first.ID == other.ID || trk.opIDs[0] == trk.opIDs[2] {
		t.Errorf("two different creates share task %q / operation %q", first.ID,
			trk.opIDs[0])
	}
	if _, err := uuid.Parse(first.ID); err != nil {
		t.Errorf("the derived task id %q is not a uuid: %v", first.ID, err)
	}
}
