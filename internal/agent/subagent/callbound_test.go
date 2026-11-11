package subagent_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/subagent"
)

// THE DELEGATE TOOL IS BOUND BY ITS CALL'S CAP. A call of `delegate` runs its
// workers in waves, each worker under its own cap and the whole call under
// another, at which every worker still out is cancelled: that cap is the
// longest one call runs, and what the tool declares as its bound, which a
// box's harness is told to wait for a call as long as (internal/api/mcpbridge).
// One worker's cap is not it: workers that wait on one another, each finishing
// just inside its own cap, run past it.
//
// Mutation: declare one worker's cap in its place, and a call of several waves
// is given up before it could end.
func TestTheDelegateToolIsBoundByItsCallCap(t *testing.T) {
	t.Parallel()
	tool := subagent.NewTool(subagent.Config{Limits: subagent.Limits{
		TaskTimeout: 5 * time.Minute, CallTimeout: 15 * time.Minute,
	}})
	if got := tool.CallBound(); got != 15*time.Minute {
		t.Errorf("CallBound = %s, want the call's own cap, 15m0s", got)
	}
}
