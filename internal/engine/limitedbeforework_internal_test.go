package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN THAT MET ITS LIMIT BEFORE ANY WORK IS RELAUNCHED, ONCE, ON THE NEXT
// MEMBER: its ending sat its entry out, it made no call and wrote nothing, and
// it is not itself that relaunch. A run that worked first, one limited again
// after the relaunch, and one a person's answer, a lost host, a replaced
// sign-in or a refused session already carries on are left to that.
//
// Mutation: drop any one of those conditions from limitedBeforeWork, and a row
// here fails.
func TestARunLimitedBeforeAnyWorkIsRelaunchedOnTheNextMemberOnce(t *testing.T) {
	t.Parallel()
	run := func(followUp sandbox.FollowUpReason) sandbox.PendingRun {
		return sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe", SandboxID: "box-1",
			Launch: sandbox.LaunchRecord{ProviderKey: "spent", FollowUp: followUp}}
	}
	worked := sandbox.BridgeLog{Calls: []sandbox.BridgeCall{{}}}
	for name, tc := range map[string]struct {
		in       resumeInput
		spent    bool
		relaunch bool
	}{
		"limited before any work":           {in: resumeInput{Run: run("")}, spent: true, relaunch: true},
		"its ending sat nothing out":        {in: resumeInput{Run: run("")}},
		"it made a call first":              {in: resumeInput{Run: run(""), Bridged: worked}, spent: true},
		"it wrote output first":             {in: resumeInput{Run: run(""), OutputTokens: 12}, spent: true},
		"limited again after the relaunch":  {in: resumeInput{Run: run(sandbox.FollowUpLimitedBeforeWork)}, spent: true},
		"a person's answer carries it on":   {in: resumeInput{Run: run(""), Asked: &sandbox.AskedRun{}}, spent: true},
		"a lost host carries it on already": {in: resumeInput{Run: run(""), Lost: &sandbox.LostRun{}}, spent: true},
		"a refused session carries it on":   {in: resumeInput{Run: run(""), Refused: &runner.Refused{}}, spent: true},
	} {
		got := limitedBeforeWork(t.Context(), tc.in, tc.spent)
		if (got != nil) != tc.relaunch {
			t.Errorf("%s: limitedBeforeWork = %+v, want relaunch %v", name, got, tc.relaunch)
		}
		if got != nil && !got.BoxKept {
			t.Errorf("%s: the carried round forgot its kept box", name)
		}
	}
}
