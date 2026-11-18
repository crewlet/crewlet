package builtin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ARGUMENT A BUILTIN DOES NOT READ IS REFUSED BY NAME, ON EVERY PATH A CALL
// TAKES — before the tool runs, and before anybody's authority is asked.
//
// A builtin reads only what its schema declares, so an undeclared argument
// would be dropped and the call answered as though less were asked (a retired
// `owner` saving a personal view as a shared tab; a misspelling never named).
// A seat's turn, a sandbox's bridge and the operator's assistant reach the
// tools with no HTTP route in front, so the gate every builtin is wrapped in
// at registration checks it; this covers its two paths: a seat's turn through
// the phase surface, and the operator catalogue's plain call.
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
		got, err := surface[worklet.SaveWorkViewTool].Call(everyGrant(),
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
}
