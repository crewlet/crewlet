package builtin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN ARGUMENT A BUILTIN DOES NOT READ IS REFUSED BY NAME, ON EVERY PATH A CALL
// TAKES — before the tool runs, and before anybody's authority is asked.
//
// A builtin reads what its schema declares and nothing else, so an undeclared
// argument was DROPPED, and the call answered as though somebody had asked for
// less. The HTTP write surface refused one at its routes and nothing else did:
// a seat's turn, a sandbox's bridge and the operator's assistant reach the
// same tools through the same gate with no route in front of them, so an
// assistant still sending `save_work_view`'s retired `owner` had a PERSONAL
// view saved as a SHARED tab under a success, and a model that misspelt an
// argument was never told which one. The gate every builtin is wrapped in at
// registration is the one place every call passes, so it is checked there —
// here on its three paths: a seat's turn through the phase surface, the
// operator catalogue's plain call, and the detached arm, which must not
// suspend a loop for a run it refused to start.
func TestAnArgumentABuiltinDoesNotReadIsRefusedOnEveryPath(t *testing.T) {
	t.Parallel()

	refusedByName := func(t *testing.T, res tools.Result, named ...string) {
		t.Helper()
		if !res.Failed || !errors.Is(res.Cause, builtin.ErrUndeclaredArgument) {
			t.Fatalf("answered %q (cause %v), want the argument refused by name",
				res.Output, res.Cause)
		}
		for _, name := range named {
			if !strings.Contains(res.Output, name) {
				t.Errorf("the refusal does not name %s: %s", name, res.Output)
			}
		}
	}

	t.Run("a seat's turn", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		surface := tools.NewSurface("execute", reg.Snapshot(),
			[]string{builtin.CreateWorkItemTool}).ForTurn(workTurn(t))
		got, err := surface.Execute(everyGrant(), llm.ToolCall{ID: "c1",
			Name: builtin.CreateWorkItemTool, Arguments: map[string]any{
				"title": "rotate the key", "project": "ENG", "assigne": "ana",
			}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !got.Failed || !strings.Contains(got.Output, `"assigne"`) ||
			!strings.Contains(got.Output, "assignee") {
			t.Errorf("a misspelt argument answered %q, want it named beside "+
				"what the tool reads", got.Output)
		}
		if len(trk.actors) != 0 {
			t.Errorf("a create carrying an argument nothing reads was written as %v",
				trk.actors)
		}
	})

	t.Run("the operator's assistant", func(t *testing.T) {
		t.Parallel()
		views := &viewStore{}
		surface := ownRecordTools(t, &personSpy{}, views)
		got, err := surface[tracker.SaveWorkViewTool].Call(everyGrant(),
			map[string]any{"owner": "ana", "container": "project:ENG",
				"name": "Mine", "type": "list"})
		if err != nil {
			t.Fatalf("save_work_view: %v", err)
		}
		refusedByName(t, got, `"owner"`)
		if len(views.saved) != 0 {
			t.Errorf("the retired shape was saved as %+v", views.saved)
		}
	})

	t.Run("a detached run", func(t *testing.T) {
		t.Parallel()
		spy := &launchSpy{}
		surface := sandboxSurface(t, spy)
		got, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1",
			Name: builtin.RunSandboxTool, Arguments: map[string]any{
				"brief": "fix the failing test", "repo": "example.com/acme/api",
			}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got.Suspend {
			t.Error("a refused launch suspended the loop, which waits for a run " +
				"that never started")
		}
		if !got.Failed || !strings.Contains(got.Output, `"repo"`) {
			t.Errorf("a launch carrying an argument nothing reads answered %q", got.Output)
		}
		if len(spy.briefs) != 0 {
			t.Errorf("the refused run was launched: %v", spy.briefs)
		}
	})
}
