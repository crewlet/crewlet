package runner_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
)

// A DROPPED LATE CALL IS TOLD PAST EVERY SAVE POINT. A long round's log drops
// from its middle, and a dropped call keeps its identity alone, so not its mark;
// the log counts the late ones it drops instead (sandbox.BridgeLog.LateElided),
// and a continuation of the round is told them among the calls dropped after
// its save point whatever that point is — here a session saved after every call
// the log took, which the tally alone would tell nothing.
//
// Mutation: hand the continuation's telling no count of the dropped late calls,
// and it is told none.
func TestADroppedLateCallIsToldPastEverySavePoint(t *testing.T) {
	t.Parallel()
	var bridged []runner.BridgedCall
	for n := 1; n <= 300; n++ {
		if n > 100 && n <= 200 {
			continue
		}
		bridged = append(bridged, numbered("jira_create", map[string]any{"summary": fmt.Sprintf("issue %d", n)},
			n, n))
	}
	basis := keptBasis
	basis.Through = 300
	brief := lostRelaunch(t, basis, runner.Resume{Bridged: bridged,
		Elided: []ledger.Elided{{Name: "jira_create", Calls: 100}}, LateElided: 1}).Session.Brief
	if !strings.Contains(brief, "(+1 further call ") {
		t.Errorf("a session saved after every call its log took is not told the late call its log dropped:\n%s",
			brief)
	}
}
