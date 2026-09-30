package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN `op_id` BROUGHT BACK WITH ANOTHER CALL IS REFUSED BEFORE ANY WRITE.
//
// Each step of an operator's call was named by its verb and its object, so a
// create in OPS under the op_id a create in ENG was answered with derived
// `X.create-OPS` beside `X.create-ENG` — two operations, two task ids, and on a
// real ledger two items: the duplicate the op_id's own contract says cannot
// happen. The same held for an update, a merge, a move, a removal or a restore
// naming another item, another person's priorities and another view. And
// naming the object alone would not have closed it: with the same object and
// other arguments, the steps that matched were answered as the first call's
// while the ones that did not — a dependency mirror on a blocker the first call
// never named — landed as a write nothing had authored.
//
// So the op_id names the whole call it was answered for, and with any other it
// is refused naming `op_id`, before the writer is reached at all.
func TestAnOpIDBroughtBackWithAnotherCallIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	view := func(id string) map[string]any {
		return map[string]any{"id": id, "container": "project:ENG",
			"name": "Mine", "type": "list"}
	}
	type call struct {
		tool string
		args map[string]any
	}
	for name, tc := range map[string]struct{ first, second call }{
		"a create in another project": {
			call{builtin.CreateWorkItemTool, map[string]any{"title": "the follow-up", "project": "ENG"}},
			call{builtin.CreateWorkItemTool, map[string]any{"title": "the follow-up", "project": "OPS"}}},
		"a create with another title": {
			call{builtin.CreateWorkItemTool, map[string]any{"title": "the follow-up", "project": "ENG"}},
			call{builtin.CreateWorkItemTool, map[string]any{"title": "a follow-up", "project": "ENG"}}},
		"an update of another item": {
			call{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "status": "done"}},
			call{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-2", "status": "done"}}},
		"an update waiting on another blocker": {
			call{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "waiting_on": map[string]any{"add": []any{"ENG-2"}}}},
			call{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "waiting_on": map[string]any{"add": []any{"ENG-3"}}}}},
		"a comment on another item": {
			call{builtin.CommentOnWorkTool, map[string]any{"item": "ENG-1", "body": "see the PR"}},
			call{builtin.CommentOnWorkTool, map[string]any{"item": "ENG-2", "body": "see the PR"}}},
		"a merge into another item": {
			call{tracker.MergeWorkItemTool, map[string]any{"item": "ENG-2", "into": "ENG-1"}},
			call{tracker.MergeWorkItemTool, map[string]any{"item": "ENG-2", "into": "ENG-3"}}},
		"a move of another item": {
			call{tracker.MoveWorkItemTool, map[string]any{"item": "ENG-1", "project": "OPS"}},
			call{tracker.MoveWorkItemTool, map[string]any{"item": "ENG-2", "project": "OPS"}}},
		"a removal of another item": {
			call{tracker.RemoveWorkItemTool, map[string]any{"item": "ENG-1"}},
			call{tracker.RemoveWorkItemTool, map[string]any{"item": "ENG-2"}}},
		"a restore of another item": {
			call{tracker.RestoreWorkItemTool, map[string]any{"item": "ENG-1"}},
			call{tracker.RestoreWorkItemTool, map[string]any{"item": "ENG-2"}}},
		"another person's priorities": {
			call{tracker.SetPrioritiesTool, map[string]any{"handle": "ana", "items": []any{"ENG-1"}}},
			call{tracker.SetPrioritiesTool, map[string]any{"handle": "bo", "items": []any{"ENG-1"}}}},
		"another view": {
			call{tracker.SaveWorkViewTool, view("v-1")},
			call{tracker.SaveWorkViewTool, view("v-2")}},
		"another tool": {
			call{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "status": "done"}},
			call{builtin.CommentOnWorkTool, map[string]any{"item": "ENG-1", "body": "done"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := &unknownWriter{fakeTracker: newFakeTracker()}
			reg := u.operatorSurface(t)
			op := answeredOp(t, callNoTurn(t, reg, tc.first.tool, tc.first.args))
			before := writesOf(u)

			second := map[string]any{"op_id": op}
			for k, v := range tc.second.args {
				second[k] = v
			}
			got := callNoTurn(t, reg, tc.second.tool, second)
			if !got.Failed || !strings.Contains(got.Output, "refused that `op_id`") ||
				!strings.Contains(got.Output, "nothing was written") ||
				!strings.Contains(got.Output, "leave `op_id` out") {
				t.Errorf("op_id %s brought back with another call answered %q",
					op, got.Output)
			}
			if after := writesOf(u); after != before {
				t.Errorf("the refused call reached the writer %d time(s)", after-before)
			}
		})
	}

	// A STEP'S OWN ID HANDED BACK IS REFUSED TOO: as an op_id it would be a
	// NEW call's operation, deriving new objects under it.
	t.Run("a step's id", func(t *testing.T) {
		t.Parallel()
		u := &unknownWriter{fakeTracker: newFakeTracker()}
		reg := u.operatorSurface(t)
		args := map[string]any{"item": "ENG-1", "status": "done"}
		callNoTurn(t, reg, builtin.UpdateWorkItemTool, args)
		step := u.ops[0]
		got := callNoTurn(t, reg, builtin.UpdateWorkItemTool,
			map[string]any{"item": "ENG-1", "status": "done", "op_id": step})
		if !got.Failed || len(u.ops) != 1 {
			t.Errorf("the step id %s was taken back as an op_id: %s", step, got.Output)
		}
	})
}

// writesOf counts every write the fake received, its own creates and
// dependency changes included.
func writesOf(u *unknownWriter) int {
	return len(u.ops) + len(u.created) + len(u.depended)
}

// AN OPERATION THAT ALREADY WROTE TO SOMETHING ELSE IS ANSWERED WITH ITS
// REMEDY: leave `op_id` out. Brought back with its own call, a step can still
// meet another object — the key a move aliases is the item's current one, and
// somebody may have moved it since — and the state log refuses it `op_reused`.
// The generic "the change was NOT made" is true, and gives an operator's
// assistant nothing to try but the same call, which meets the same record.
func TestAnOperationThatWroteElsewhereIsAnsweredWithItsRemedy(t *testing.T) {
	t.Parallel()
	reused := &statelog.Unavailable{Reason: statelog.ReasonOpReused,
		Detail: "operation landed on alias/ENG-1, and this write is to alias/FIN-2"}

	t.Run("the write itself", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		trk.writeErr = reused
		checkReusedAnswer(t, callNoTurn(t, retrySurface(t, trk, nil),
			builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-2", "status": "done"}))
	})
	t.Run("its label declaration", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		trk.ensureErr = reused
		checkReusedAnswer(t, callNoTurn(t, labelSurface(t, trk, true),
			builtin.CreateWorkItemTool, map[string]any{"title": "x", "project": "OPS",
				"labels": []any{"regression"}, "labels_create_missing": true}))
		if len(trk.created) != 0 {
			t.Error("the create carried on past its refused declaration")
		}
	})
	t.Run("a seat, which has no op_id to leave out", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		trk.writeErr = reused
		got := callWork(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
			builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-2", "status": "done"})
		if !got.Failed || strings.Contains(got.Output, "op_id") ||
			!strings.Contains(got.Output, "NOT made") {
			t.Errorf("a seat's refused write answered %q", got.Output)
		}
	})
}

func checkReusedAnswer(t *testing.T, got tools.Result) {
	t.Helper()
	op := answeredOp(t, got)
	for _, want := range []string{"NOT made", "leave `op_id` out", op, "op_reused"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refused write's answer lacks %q: %s", want, got.Output)
		}
	}
	if !got.Failed {
		t.Error("a refused write answered as a success")
	}
}
