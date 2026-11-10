package runner_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
)

// A RELAUNCH HANDS ITS LAUNCH THE READ ITS BRIEFS WERE BUILT FROM. A relaunch
// that carries the round on builds every brief from one read of the round's
// log, then readies its launch — its probe, any rewrite of a long call, the
// call a person approved — before the launch's opening moves the log on: a call
// of the job before landing in between is told by none of them. So the launch
// is handed how many calls the log had taken at that read
// (runner.Continuation.BriefRead), past which its opening marks the round's
// calls late (sandbox.BridgeLog's lateAfter).
//
// Mutation: hand the launch no read, and the opening marks nothing.
func TestARelaunchHandsItsLaunchTheReadItsBriefsWereBuiltFrom(t *testing.T) {
	t.Parallel()
	bridged := []runner.BridgedCall{numbered("slack_post", map[string]any{"text": "posted"}, 1, 1)}
	req := lostRelaunch(t, keptBasis, runner.Resume{Bridged: bridged, BridgedRead: 7})
	if req.Continue == nil || req.Continue.BriefRead == nil || *req.Continue.BriefRead != 7 {
		t.Fatalf("the relaunch carrying the round on hands its launch %+v, want the read of 7 calls its "+
			"briefs were built from", req.Continue)
	}
}
