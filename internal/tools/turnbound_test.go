package tools_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
)

// turnTold is a turn-bound tool that records how each call reached it.
type turnTold struct {
	turn          *turnctx.Turn
	plain, inTurn int
}

func (*turnTold) Name() string        { return "told" }
func (*turnTold) Description() string { return "records how it was called" }
func (*turnTold) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *turnTold) Call(context.Context, map[string]any) (tools.Result, error) {
	t.plain++
	return tools.Result{Output: "plain"}, nil
}

func (t *turnTold) CallInTurn(_ context.Context, turn *turnctx.Turn, _ map[string]any) (tools.Result, error) {
	t.inTurn++
	t.turn = turn
	return tools.Result{Output: "in turn"}, nil
}

// A SEAT'S CALL TELLS A TURN-BOUND TOOL ITS TURN, and any other call does not:
// builtin's gate binds the requester ceiling from the turn a seat's call is
// made in (ADR-0105), for the read-only builtins as for the seat-scoped ones,
// so every surface a seat calls through — the turn's loop, a delegate worker,
// the executor's bridge, each of which invokes as the seat — hands it over,
// and the operator's surface and the HTTP one, which call as their own
// principal, reach the plain call.
//
// Mutation: drop the TurnBound arm of tools' call — the seat's call reaches
// the plain Call and no turn is told.
func TestASeatsCallTellsATurnBoundToolItsTurn(t *testing.T) {
	t.Parallel()
	tool := &turnTold{}
	turn := &turnctx.Turn{RunID: "run-1"}
	if _, _, err := tools.Invoke(t.Context(), tools.Invocation{Name: tool.Name(), Tool: tool,
		Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: turn}); err != nil {
		t.Fatal(err)
	}
	if tool.inTurn != 1 || tool.turn != turn || tool.plain != 0 {
		t.Errorf("a seat's call reached the tool %d times in its turn (%v) and %d plainly, want once, in "+
			"turn run-1", tool.inTurn, tool.turn, tool.plain)
	}
	if _, _, err := tools.Invoke(t.Context(), tools.Invocation{Name: tool.Name(), Tool: tool,
		Args: tools.ArgsOf(tool, false, map[string]any{})}); err != nil {
		t.Fatal(err)
	}
	if tool.plain != 1 || tool.inTurn != 1 {
		t.Errorf("a call not made as a seat reached the tool plainly %d times and in a turn %d, want one "+
			"plain call", tool.plain, tool.inTurn)
	}
}
