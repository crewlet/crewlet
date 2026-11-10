package runner_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
)

// A DROPPED LATE CALL IS TOLD PAST EVERY SAVE POINT, UNDER THE LATE CALLS'
// WORDS. A long round's log drops from its middle, and a dropped call keeps its
// identity alone, so neither its mark nor its number; the log counts the late
// ones it drops instead (sandbox.BridgeLog.LateElided), and a continuation of
// the round is told how many under the late calls' own words whatever its save
// point — here a session saved after every call the log took, which the tally
// alone would tell nothing — and never as calls its own run made after it was
// saved, which no late call is. A session saved before the dropped middle is
// told the calls dropped past its point under those words as well, a dropped
// late call among them by its number: an over-count, never an under-count.
//
// Mutation: hand the continuation's telling no count of the dropped late calls,
// or count them among the calls dropped after the save, and a case goes red.
func TestADroppedLateCallIsToldPastEverySavePoint(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls that finished after the run that made them stopped waiting for them"
	var bridged []runner.BridgedCall
	for n := 1; n <= 300; n++ {
		if n > 100 && n <= 200 {
			continue
		}
		bridged = append(bridged, numbered("jira_create", map[string]any{"summary": fmt.Sprintf("issue %d", n)},
			n, n))
	}
	for name, tc := range map[string]struct {
		through int
		// afterSave is the count told under the words of calls after the
		// save, "" for none.
		afterSave string
	}{
		"a session saved after every call the log took": {through: 300},
		"a session saved before the dropped middle": {through: 100,
			afterSave: "(+100 further calls this run made after the session above was saved,"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			basis := keptBasis
			basis.Through = tc.through
			brief := lostRelaunch(t, basis, runner.Resume{Bridged: bridged,
				Elided: []ledger.Elided{{Name: "jira_create", Calls: 100}}, LateElided: 1}).Session.Brief
			at := strings.Index(brief, lateHeading)
			if at < 0 || !strings.Contains(brief[at:], "(+1 further such call,") {
				t.Errorf("the late call the log dropped is not counted under the late calls' words:\n%s", brief)
			}
			if strings.Contains(brief, "(+1 further call this run made") {
				t.Errorf("the late call the log dropped is counted as a call the session's own run made after "+
					"it was saved:\n%s", brief)
			}
			if tc.afterSave != "" && !strings.Contains(brief, tc.afterSave) {
				t.Errorf("the session is not told %q:\n%s", tc.afterSave, brief)
			}
		})
	}
}
