package builtin_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
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
		if len(trk.opIDs) != 2 {
			t.Fatalf("two updates wrote %v", trk.opIDs)
		}
		if trk.opIDs[0] == trk.opIDs[1] {
			t.Errorf("moving an item to in_progress and then to done wrote both "+
				"under %q — the second is collapsed as the first's retry and "+
				"the item never reaches done", trk.opIDs[0])
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

// A WRITE MADE AGAIN AFTER A DIFFERENT ONE IS A NEW OPERATION, a write repeated
// with nothing different between is the same one, and a re-run reproduces
// every id.
//
// What a call asks for names WHICH write it is and not WHEN, so a run that
// moved an item to in_progress, then to done, then back to in_progress derived
// one id for the first and the third: the third was answered as the first's
// retry — `applied`, at the first's position — and the item stayed done while
// the tool said otherwise.
func TestAWriteMadeAgainAfterADifferentOneIsANewOperation(t *testing.T) {
	t.Parallel()
	started := map[string]any{"item": "ENG-1", "status": "in_progress"}
	done := map[string]any{"item": "ENG-1", "status": "done"}

	t.Run("an update", func(t *testing.T) {
		t.Parallel()
		run := func() []string {
			trk := newFakeTracker()
			surface := workSurface(t, trk, builtin.UpdateWorkItemTool)
			for _, args := range []map[string]any{started, done, started} {
				executeWork(t, surface, builtin.UpdateWorkItemTool, args)
			}
			return trk.opIDs
		}
		first := run()
		if len(first) != 3 {
			t.Fatalf("three updates wrote %v", first)
		}
		if first[2] == first[0] {
			t.Errorf("moving an item back to in_progress after done wrote under the "+
				"first move's id %q — it is answered as that move's retry and the "+
				"item stays done", first[0])
		}
		if first[2] == first[1] {
			t.Errorf("the move back wrote under the move to done's id %q", first[1])
		}
		if rerun := run(); rerun[0] != first[0] || rerun[1] != first[1] || rerun[2] != first[2] {
			t.Errorf("a re-run making the same calls wrote under %v, the first run "+
				"under %v — it would apply every one of them twice", rerun, first)
		}
	})

	t.Run("a repeat with nothing between", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		surface := workSurface(t, trk, builtin.UpdateWorkItemTool)
		executeWork(t, surface, builtin.UpdateWorkItemTool, started)
		executeWork(t, surface, builtin.UpdateWorkItemTool, map[string]any{
			"status": "in_progress", "item": "ENG-1",
		})
		if len(trk.opIDs) != 2 || trk.opIDs[0] != trk.opIDs[1] {
			t.Errorf("the same update asked twice in a row wrote under %v — an "+
				"executor that asks twice, or a retry after `unknown`, would "+
				"apply it twice", trk.opIDs)
		}
	})

	t.Run("a comment", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		surface := workSurface(t, trk, builtin.CommentOnWorkTool)
		for _, body := range []string{"blocked on review", "unblocked", "blocked on review"} {
			executeWork(t, surface, builtin.CommentOnWorkTool, map[string]any{
				"item": "ENG-1", "body": body,
			})
		}
		if len(trk.patched) != 3 {
			t.Fatalf("three comments wrote %d patches", len(trk.patched))
		}
		first, again := commentOf(t, trk.patched[0]), commentOf(t, trk.patched[2])
		if first.ID == again.ID || trk.opIDs[0] == trk.opIDs[2] {
			t.Errorf("a remark made again after a different one reused comment %q / "+
				"operation %q — it is answered as the first remark's retry",
				first.ID, trk.opIDs[0])
		}
	})
}

// workSurface is a tool surface over the work tools, bound to a turn with a
// call log of its own — the frame a real turn's calls go through.
func workSurface(t *testing.T, trk *fakeTracker, active ...string) *tools.Surface {
	t.Helper()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	turn := workTurn(t)
	turn.Calls = turnctx.NewCallLog()
	return tools.NewSurface("execute", reg.Snapshot(), active).ForTurn(turn)
}

// executeWork makes one call through a surface, as a tool loop does.
func executeWork(t *testing.T, surface *tools.Surface, name string, args map[string]any) {
	t.Helper()
	res, err := surface.Execute(t.Context(), llm.ToolCall{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.Failed {
		t.Fatalf("%s %v failed: %s", name, args, res.Output)
	}
}
