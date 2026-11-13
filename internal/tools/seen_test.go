package tools_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// probeTool answers what its `say` argument asks, or fails where `fail` is set.
type probeTool struct{}

func (probeTool) Name() string               { return "probe" }
func (probeTool) Description() string        { return "probes a system" }
func (probeTool) Parameters() map[string]any { return nil }
func (probeTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	say, _ := args["say"].(string)
	fail, _ := args["fail"].(bool)
	return tools.Result{Output: say, Failed: fail}, nil
}

// AN ANSWERED CALL IS KEPT FOR THE TURN, AND A FAILED ONE IS NOT (ADR-0132):
// the surface notes on the turn what every call that answered showed the
// model, with its tool, where the tool came from and the arguments it ran
// with, which is the only text a later call of the turn can cite. A call that
// failed, and one that named no tool, showed nothing to cite.
//
// Mutation: note failed calls too, and a refusal's own wording becomes
// evidence; drop the note, and no record an agent makes can cite anything.
func TestAnAnsweredCallIsKeptForTheTurnAndAFailedOneIsNot(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	mustRegister(t, r, probeTool{}, tools.OriginBuiltin)
	turn := &turnctx.Turn{Calls: turnctx.NewCallLog(), Seen: turnctx.NewSeen()}
	s := tools.NewSurface("execute", r.Snapshot(), []string{"probe"}).ForTurn(turn)
	run := func(name string, args map[string]any) {
		t.Helper()
		if _, err := s.Admit(name, args)(t.Context(), llm.ToolCall{ID: "c", Name: name, Arguments: args}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	run("probe", map[string]any{"say": "orders-db is up"})
	run("probe", map[string]any{"say": "permission denied", "fail": true})
	run("no_such_tool", map[string]any{"say": "orders-db"})
	run("probe", map[string]any{"say": "web-1 is up"})

	answers, dropped := turn.Answers().Of("probe")
	if len(answers) != 2 || dropped != 0 {
		t.Fatalf("the turn holds %d answers of the tool (%d dropped), want the two that answered", len(answers), dropped)
	}
	if answers[0].Output != "web-1 is up" || answers[1].Output != "orders-db is up" {
		t.Errorf("the answers read %q then %q, want the newest first", answers[0].Output, answers[1].Output)
	}
	if answers[1].Origin != tools.OriginBuiltin || answers[1].Args["say"] != "orders-db is up" {
		t.Errorf("an answer is kept as %+v, want its origin and its arguments beside it", answers[1])
	}
	if unknown, _ := turn.Answers().Of("no_such_tool"); len(unknown) != 0 {
		t.Errorf("a call that named no tool was kept: %+v", unknown)
	}
}
