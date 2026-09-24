package builtin_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN OPERATOR'S CALL IS AN OPERATION THE OPERATOR CAN BRING BACK.
//
// # What was wrong
//
// A seat's writes derive their operation ids from its turn, so the same call
// made again — a re-run, or a repeat after an `unknown` — is the same
// operation, and the ledger answers what landed. The operator's surface has no
// turn, so every call minted every id afresh: the one thing an `unknown` or a
// gesture stopped part of the way through asks for, the same operation again,
// could not be asked for at all. And the failure text told an operator's
// assistant to "call create_work_item again with exactly the same arguments" —
// which, under a fresh id, derives a fresh task id and files a second item.
//
// # What is asserted
//
// The OPERATION IDS and the DERIVED IDENTITIES, not rows: the writers here are
// fakes, so the ledger that collapses is not in this suite. Two equal ids are
// two writes any ledger collapses, which is the property.

// retrySurface is the operator's own catalogue over the fake tracker — the
// surface the op_id belongs to — with every write seam the retry cases reach.
func retrySurface(t *testing.T, trk *fakeTracker, views builtin.ViewWriter) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Dependencies: trk.depends,
			Merges: trk.merges, Actor: operatorActor,
			ViewWriter: func(builtin.Actor) builtin.ViewWriter { return views },
		},
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// answerOf decodes a tool's JSON answer.
func answerOf(t *testing.T, got tools.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
		t.Fatalf("the answer is not JSON (%v): %s", err, got.Output)
	}
	return out
}

// answeredOp is the op_id an operator's answer carried: a receipt's field, or
// the one a failed answer tells the caller to bring back — held to the rule
// the surface holds a brought-back id to.
func answeredOp(t *testing.T, got tools.Result) string {
	t.Helper()
	var receipt map[string]any
	op := ""
	if json.Unmarshal([]byte(got.Output), &receipt) == nil {
		op, _ = receipt["op_id"].(string)
	} else if named := broughtBack.FindStringSubmatch(got.Output); named != nil {
		op = named[1]
	}
	if err := statelog.CheckCallerOpID(op); err != nil {
		t.Fatalf("the answer names no op_id the surface would take back (%v): %s",
			err, got.Output)
	}
	return op
}

// broughtBack is how a failed answer names the op_id to bring back.
var broughtBack = regexp.MustCompile("`op_id` \"([^\"]+)\"")

func TestAnOperatorsCreateBroughtBackIsTheSameCreate(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := retrySurface(t, trk, nil)
	args := map[string]any{"title": "the follow-up", "project": "ENG"}

	first := callNoTurn(t, reg, builtin.CreateWorkItemTool, args)
	if first.Failed {
		t.Fatalf("the create failed: %s", first.Output)
	}
	op, _ := answerOf(t, first)["op_id"].(string)
	if err := statelog.CheckCallerOpID(op); err != nil {
		t.Fatalf("the create answered op_id %q, which the surface would refuse "+
			"back: %v", op, err)
	}

	again := map[string]any{"title": "the follow-up", "project": "ENG", "op_id": op}
	second := callNoTurn(t, reg, builtin.CreateWorkItemTool, again)
	if second.Failed {
		t.Fatalf("the create brought back failed: %s", second.Output)
	}
	if got, _ := answerOf(t, second)["op_id"].(string); got != op {
		t.Errorf("the call brought back answered op_id %q, want %q", got, op)
	}
	if trk.opIDs[1] != trk.opIDs[0] {
		t.Errorf("the create brought back wrote under %q, the first under %q — "+
			"a different operation, which the ledger decides again", trk.opIDs[1],
			trk.opIDs[0])
	}
	if trk.created[1].ID != trk.created[0].ID {
		t.Errorf("the create brought back derived task %s, the first %s — a "+
			"second item", trk.created[1].ID, trk.created[0].ID)
	}

	// AND A CALL THAT BRINGS NOTHING BACK IS A NEW OPERATION, or an
	// operator could file exactly one item per title for ever.
	callNoTurn(t, reg, builtin.CreateWorkItemTool, args)
	if trk.opIDs[2] == trk.opIDs[0] || trk.created[2].ID == trk.created[0].ID {
		t.Errorf("a call with no op_id reused the first call's operation %q", trk.opIDs[0])
	}
}

// A COMMENT AND A NEW VIEW ARE IDENTIFIED BY THE CALL TOO. Each is a row whose
// id is the subject its create arbitrates on, so an id minted per call made
// the call brought back a second comment and a second view whatever its
// operation id said.
func TestAnOperatorsCommentAndViewBroughtBackAreTheSameObject(t *testing.T) {
	t.Parallel()
	t.Run("comment", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := retrySurface(t, trk, nil)
		first := callNoTurn(t, reg, builtin.CommentOnWorkTool,
			map[string]any{"item": "ENG-1", "body": "noted"})
		op, _ := answerOf(t, first)["op_id"].(string)
		callNoTurn(t, reg, builtin.CommentOnWorkTool,
			map[string]any{"item": "ENG-1", "body": "noted", "op_id": op})
		if len(trk.patched) != 2 {
			t.Fatalf("the two calls wrote %d comments", len(trk.patched))
		}
		if a, b := trk.patched[0].Comment.ID, trk.patched[1].Comment.ID; a != b {
			t.Errorf("the comment brought back is %s, the first %s — a second "+
				"comment on the thread", b, a)
		}
		if trk.opIDs[0] != trk.opIDs[1] {
			t.Errorf("the comment brought back wrote under %q, the first under %q",
				trk.opIDs[1], trk.opIDs[0])
		}
	})
	t.Run("view", func(t *testing.T) {
		t.Parallel()
		views := &viewSpy{}
		reg := retrySurface(t, newFakeTracker(), views)
		args := map[string]any{"container": "project:ENG", "name": "Mine", "type": "list"}
		first := callNoTurn(t, reg, tracker.SaveWorkViewTool, args)
		if first.Failed {
			t.Fatalf("the save failed: %s", first.Output)
		}
		op, _ := answerOf(t, first)["op_id"].(string)
		callNoTurn(t, reg, tracker.SaveWorkViewTool, map[string]any{
			"container": "project:ENG", "name": "Mine", "type": "list", "op_id": op,
		})
		if len(views.saved) != 2 || views.saved[0].ID != views.saved[1].ID {
			t.Errorf("the save brought back wrote %+v — a second view", views.saved)
		}
		if views.ops[0] != views.ops[1] {
			t.Errorf("the save brought back wrote under %q, the first under %q",
				views.ops[1], views.ops[0])
		}
	})
}

// THE ARGUMENT IS OFFERED WHERE A CALLER HOLDS ONE, AND NOWHERE ELSE. A seat's
// turn is its identity, so a seat schema offering `op_id` would let a model
// name another write's operation — answered as that write, whatever it asked.
func TestOpIDIsOfferedOnlyOnTheOperatorsWrites(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	// EVERY WRITE SEAM WIRED — the move included, which this fixture lacked,
	// so nothing asserted the one tool the API reference lists as taking an
	// op_id that no case here ever registered.
	operator := builtin.OperatorTools(builtin.OperatorDeps{Work: builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends, Merges: trk.merges,
		Moves: trk.moves,
		Inbox: trk, TrashWriter: func(builtin.Actor) builtin.TrashWriter { return nil },
		PersonWriter: func(builtin.Actor) builtin.PersonWriter { return nil },
		ViewWriter:   func(builtin.Actor) builtin.ViewWriter { return nil },
		Actor:        operatorActor,
	}})
	writes := []string{
		builtin.CreateWorkItemTool, builtin.UpdateWorkItemTool, builtin.CommentOnWorkTool,
		tracker.MergeWorkItemTool, tracker.MoveWorkItemTool,
		tracker.RemoveWorkItemTool, tracker.RestoreWorkItemTool,
		tracker.SetPrioritiesTool, tracker.SetPinsTool, tracker.MarkInboxTool,
		tracker.SaveWorkViewTool,
	}
	offered := map[string]bool{}
	for _, tool := range operator {
		props, _ := tool.Parameters()["properties"].(map[string]any)
		_, held := props["op_id"]
		offered[tool.Name()] = held
	}
	for _, name := range writes {
		if !offered[name] {
			t.Errorf("the operator's %s offers no op_id, so an `unknown` from "+
				"it can never be finished", name)
		}
	}
	for name, held := range offered {
		if held && !slices.Contains(writes, name) {
			t.Errorf("the operator's %s offers an op_id it does nothing with", name)
		}
	}

	seat := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as,
		Dependencies: trk.depends, Merges: trk.merges, Moves: trk.moves})
	for _, name := range []string{builtin.CreateWorkItemTool, builtin.UpdateWorkItemTool,
		builtin.CommentOnWorkTool, tracker.MergeWorkItemTool, tracker.MoveWorkItemTool} {

		entry, _ := seat.Lookup(name)
		props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
		if _, held := props["op_id"]; held {
			t.Errorf("a seat's %s offers op_id", name)
		}
	}
}

// AN OP_ID IS HELD TO THE RULE EVERY SURFACE HOLDS ONE TO, and a turn's call
// is refused one outright rather than silently ignoring it.
func TestABroughtBackOpIDIsCheckedAndATurnIsRefusedOne(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		"not an id this engine minted": "retry-1",
		"a space in it":                "01a0d089-d0ba-7d43-baf4-49cd5d66df39.x y",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			got := callNoTurn(t, retrySurface(t, trk, nil), builtin.CreateWorkItemTool,
				map[string]any{"title": "x", "project": "ENG", "op_id": bad})
			if !got.Failed || !strings.Contains(got.Output, "op_id") {
				t.Errorf("op_id %q was accepted: %s", bad, got.Output)
			}
			if len(trk.created) != 0 {
				t.Errorf("a refused op_id still wrote %d item(s)", len(trk.created))
			}
		})
	}
	t.Run("inside a turn", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		got := callWork(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
			builtin.CreateWorkItemTool, map[string]any{
				"title": "x", "project": "ENG",
				"op_id": statelog.NewOpID(time.Now(), "create_work_item"),
			})
		if !got.Failed || !strings.Contains(got.Output, "takes no `op_id`") {
			t.Errorf("a turn's call with an op_id gave %q", got.Output)
		}
		if len(trk.created) != 0 {
			t.Errorf("a refused op_id still wrote %d item(s)", len(trk.created))
		}
	})
	// AN EMPTY ONE IS NONE, which is what a model filling an optional field
	// writes — refusing it would refuse half of every assistant's creates.
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		got := callNoTurn(t, retrySurface(t, trk, nil), builtin.CreateWorkItemTool,
			map[string]any{"title": "x", "project": "ENG", "op_id": ""})
		if got.Failed {
			t.Errorf("an empty op_id was refused: %s", got.Output)
		}
	})
}

// A GESTURE STOPPED PART OF THE WAY THROUGH TELLS EACH CALLER HOW IT FINISHES:
// a seat by repeating its arguments before anything else, an operator by
// bringing its op_id back — and never an operator by a bare repeat, which is a
// new operation.
func TestAStoppedGestureTellsEachCallerHowToFinishIt(t *testing.T) {
	t.Parallel()
	stopped := fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved)

	trk := newFakeTracker()
	trk.writeErr = stopped
	reg := retrySurface(t, trk, nil)
	args := map[string]any{"item": "ENG-1", "priority": "urgent"}
	got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, args)
	if !got.Failed {
		t.Fatalf("an operator's stopped gesture answered as a receipt: %s", got.Output)
	}
	op := answeredOp(t, got)
	if strings.Contains(got.Output, "before calling it with any others") {
		t.Errorf("an operator was told a seat's repeat: %s", got.Output)
	}
	// AND BRINGING IT BACK IS THE SAME OPERATION: every write it makes
	// derives the id it derived the first time.
	trk.writeErr = nil
	if again := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "priority": "urgent", "op_id": op,
	}); again.Failed {
		t.Fatalf("the op_id brought back with its own call was refused: %s", again.Output)
	}
	if len(trk.opIDs) != 1 || !strings.HasPrefix(trk.opIDs[0], op+".") {
		t.Errorf("the brought-back call wrote under %v, want a step of %s",
			trk.opIDs, op)
	}

	seat := newFakeTracker()
	seat.writeErr = stopped
	got = callWork(t, workRegistry(t, builtin.WorkDeps{Reader: seat, Writer: seat.as}),
		builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "priority": "urgent"})
	if !got.Failed || !strings.Contains(got.Output, "before calling it with any others") ||
		strings.Contains(got.Output, "op_id") {
		t.Errorf("a seat's stopped gesture gave %q", got.Output)
	}
}

// A CREATE WHOSE DEPENDENCIES STOPPED NAMES THE ITEM IT FILED, and the tool
// that finishes them on it — never a repeat of the create, which is a second
// item for every caller whose repeat is a new operation.
func TestACreateWhoseDependenciesStoppedNamesTheItemItFiled(t *testing.T) {
	t.Parallel()
	for name, operator := range map[string]bool{"seat": false, "operator": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.dependErr = fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved)
			args := map[string]any{"title": "x", "project": "ENG", "waiting_on": []any{"ENG-2"}}
			var got tools.Result
			if operator {
				got = callNoTurn(t, retrySurface(t, trk, nil), builtin.CreateWorkItemTool, args)
			} else {
				got = callWork(t, workRegistry(t, builtin.WorkDeps{
					Reader: trk, Writer: trk.as, Dependencies: trk.depends,
				}), builtin.CreateWorkItemTool, args)
			}
			if got.Failed {
				t.Fatalf("a filed item was reported as a failed call: %s", got.Output)
			}
			failure, _ := answerOf(t, got)["dependencies_failed"].(string)
			for _, want := range []string{"ENG-9", "update_work_item",
				"Do NOT call create_work_item again"} {

				if !strings.Contains(failure, want) {
					t.Errorf("dependencies_failed lacks %q: %s", want, failure)
				}
			}
			for _, bad := range []string{"exactly the same arguments", "NOT made"} {
				if strings.Contains(failure, bad) {
					t.Errorf("dependencies_failed says %q about an item that "+
						"was filed: %s", bad, failure)
				}
			}
		})
	}
}

// AN OPERATOR'S UNKNOWN CREATE NAMES THE OP_ID TO BRING BACK, and only that
// one. The create's own write id is a step of the call's operation; handed
// back as an op_id it would be a NEW call's operation, deriving a new task.
func TestAnOperatorsUnknownCreateNamesTheOperationToBringBack(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.createAnswer = &tracker.WriteResult{
		Key:    "ENG-9",
		Result: statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op.task"},
	}
	got := callNoTurn(t, retrySurface(t, trk, nil), builtin.CreateWorkItemTool,
		map[string]any{"title": "x", "project": "ENG"})
	if !got.Failed || !strings.Contains(got.Output, "Do not reword it") {
		t.Errorf("an operator's unknown create does not say to bring back its "+
			"op_id unchanged: %q", got.Output)
	}
	if op := answeredOp(t, got); !strings.HasPrefix(trk.opIDs[0], op+".") {
		t.Errorf("the answer names op_id %s, and the create wrote under %s, "+
			"which is not a step of it", op, trk.opIDs[0])
	}
	if strings.Contains(got.Output, trk.opIDs[0]) {
		t.Errorf("the answer names the create's own write id %s, which brought "+
			"back as an op_id files a second item: %q", trk.opIDs[0], got.Output)
	}
}
