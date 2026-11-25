package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A LAUNCH IS HANDED THE LEADS ITS FRESH BRIEF OPENED WITH. A turn's first
// launch, and a send-back, carry no continuation to name what their fresh brief
// opened with, the turn's task, which the launch keeps for its job so that a
// later continuation of a session its run starts on that brief, which may be a
// summary, is told the task again.
//
// Mutation: hand the launch no fresh leads, and this goes red.
func TestALaunchIsHandedTheLeadsItsFreshBriefOpenedWith(t *testing.T) {
	t.Parallel()
	fresh := []sandbox.Lead{{Kind: sandbox.LeadKindFresh, Round: 1, Task: &sandbox.LeadTask{}}}
	got := withRunsOwn(runner.AgentRunRequest{FreshOwn: fresh}, nil, sandbox.LaunchRequest{Model: "stub/m"})
	if len(got.FreshOwn) != 1 || got.FreshOwn[0].Kind != sandbox.LeadKindFresh || got.Model != "stub/m" {
		t.Errorf("the launch is handed %+v for %q, want the fresh brief's task for stub/m", got.FreshOwn, got.Model)
	}
}
