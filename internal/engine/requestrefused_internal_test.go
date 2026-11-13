package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN WHOSE CONTINUED SESSION ITS PROVIDER REFUSED IS RELAUNCHED FRESH, ONCE.
// The session is what was refused, so the round is carried on in one of its
// own. Only then: a job refused in a session it started, or one that is itself
// that relaunch, is collected and ends the turn on the provider's message; an
// ending that is no refusal is collected as ever; and a run a person's answer
// or a lost host already carries on is left to that.
//
// Mutation: drop any one of those conditions from requestRefused, or hand a
// carried round the message too, and a row here fails.
func TestARunWhoseContinuedSessionWasRefusedIsRelaunchedFreshOnce(t *testing.T) {
	t.Parallel()
	const said = "API Error: 400 tools.88.custom.name: String should match pattern '^[a-zA-Z0-9_-]{1,128}$'"
	refused := sandbox.RunEnding{Error: said, Refused: true, At: time.Now()}
	failed := sandbox.RunEnding{Error: "exit 1", At: time.Now()}
	run := func(start sandbox.SessionStart, followUp sandbox.FollowUpReason) sandbox.PendingRun {
		return sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe", SessionID: "sess-1", SandboxID: "box-1",
			Launch: sandbox.LaunchRecord{ProviderKey: "plan", SessionStart: start, FollowUp: followUp}}
	}
	continued, fresh := sandbox.SessionStartContinued, sandbox.SessionStartFresh
	for name, tc := range map[string]struct {
		in       resumeInput
		relaunch bool
		ends     bool
	}{
		"refused in a session it continued": {resumeInput{Run: run(continued, ""), Ended: refused}, true, false},
		"refused in a session it started":   {resumeInput{Run: run(fresh, ""), Ended: refused}, false, true},
		"refused where its box never said":  {resumeInput{Run: run("", ""), Ended: refused}, false, true},
		"refused again after the relaunch": {resumeInput{Run: run(continued, sandbox.FollowUpRequestRefused),
			Ended: refused}, false, true},
		"failed another way": {resumeInput{Run: run(continued, ""), Ended: failed}, false, false},
		"a person's answer carries it on": {resumeInput{Run: run(continued, ""), Ended: refused,
			Asked: &sandbox.AskedRun{}}, false, false},
		"a lost host carries it on already": {resumeInput{Run: run(continued, ""), Ended: refused,
			Lost: &sandbox.LostRun{}}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := tc.in
			in.Refused = requestRefused(t.Context(), in)
			if (in.Refused != nil) != tc.relaunch {
				t.Fatalf("requestRefused = %+v, want relaunch %v", in.Refused, tc.relaunch)
			}
			if in.Refused != nil && (*in.Refused != runner.Refused{Said: said, BoxKept: true}) {
				t.Errorf("the carried round = %+v, want the provider's message and its kept box", *in.Refused)
			}
			if got := refusedSaid(in); (got != "") != tc.ends || (tc.ends && !strings.Contains(got, said)) {
				t.Errorf("the round's ending says %q, want the provider's message: %v", got, tc.ends)
			}
		})
	}
}
