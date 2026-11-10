package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// approvedFor is the call a person approved for held call heldID, as the round's
// log keeps it: named its tool, its arguments and output withheld, which ride
// the run's row sealed, the seq'th the bridge admitted and the appended'th the
// log took.
func approvedFor(tool, heldID string, seq, appended int) runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: tool, Withheld: true}, Seq: seq, Appended: appended,
		Approved: true, HeldID: heldID}
}

// laterAttemptBrief is the brief a later attempt at an answer carries the round
// on with: the answer allowed a call to `tr_close_issue` once, an earlier
// attempt marked and made it, recording what it returned ("closed #7"), and its
// relaunch was refused after; this attempt reads the round's log as bridged. The
// relaunch continues the kept session basis names, and the brief is that
// continuation's; where basis names none, it continues the copy in the asking
// job's own box, whose harness ended as end says.
func laterAttemptBrief(t *testing.T, basis runner.SessionBasis, end sandbox.HarnessEnd,
	bridged []runner.BridgedCall) string {

	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	made := &firstCommit{marked: true, result: &sandbox.HeldResult{Output: "closed #7"}}
	relauncher := &probingLauncher{basis: basis}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Bridged: bridged, HarnessEnd: end,
			Asked: &runner.Asked{Question: "close #7?", Answer: "yes", By: "founder",
				SessionID: "sess-ask", BoxKept: true,
				Held: &runner.HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:close",
					Choice: sandbox.HeldOnce, HeldID: "held-1", Commit: made}}}})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if basis.Continues() {
		if again.Session == nil {
			t.Fatalf("the relaunch continues no kept session, in place %+v", again.Continue)
		}
		return again.Session.Brief
	}
	if again.Session != nil || again.Continue == nil || again.Continue.SessionID != "sess-ask" {
		t.Fatalf("the relaunch continues kept %+v and in place %+v, want the asking job's own session in its "+
			"box", again.Session, again.Continue)
	}
	return again.Continue.Brief
}

// A LATER ATTEMPT AT AN ANSWER TELLS THE CALL IT APPROVED ONCE, IN ITS LEAD. The
// first attempt made the call a person allowed and recorded what it returned,
// and its relaunch was refused after, so the answer is attempted again. This
// attempt reads the call in the round's log, placed after every call the log
// took, so past any save point, and answers it from its mark: its lead says what
// the call returned. So neither a kept session's continuation, whatever its
// save point, nor the copy in the asking job's own box, which may have been cut
// off and is told every call of the round, lists that call again; while a call
// an earlier answer of the round approved, which no lead of this relaunch
// tells, is listed as any call is, once.
//
// Mutation: list the lead's approved call among the round's calls of a kept
// session's continuation, or of the copy in its box, or of both; leave out every
// call a person approved; or read no held call off the answer, and a case goes
// red.
func TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnce(t *testing.T) {
	t.Parallel()
	// THE ROUND'S LOG AS THE LATER ATTEMPT READS IT: the asking job's posts
	// either side of its kept capture's save, a call an earlier answer of the
	// round approved, and the call this answer approved, which the first
	// attempt made and the log took last.
	bridged := []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "posted before the save"}, 1, 1),
		approvedFor("jira_transition", "held-earlier", 2, 2),
		numbered("slack_post", map[string]any{"text": "posted after the save"}, 3, 3),
		approvedFor("tr_close_issue", "held-1", 4, 4),
	}
	saved := keptBasis
	saved.Through = 1
	cutOff := keptBasis
	cutOff.Through, cutOff.Interrupted = 0, true
	for name, tc := range map[string]struct {
		basis runner.SessionBasis
		end   sandbox.HarnessEnd
		// listed is the header the round's calls are told under.
		listed string
	}{
		"a kept session saved after the round's first call": {basis: saved, end: sandbox.HarnessEndOwnRun,
			listed: "## Calls made after the session above was saved"},
		"a kept session cut off": {basis: cutOff, end: sandbox.HarnessEndCutOff,
			listed: cutOffHeader},
		"the copy in the asking job's box, cut off": {end: sandbox.HarnessEndCutOff,
			listed: cutOffHeader},
		"the copy in the asking job's box, its record saying nothing": {
			listed: cutOffHeader},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			brief := laterAttemptBrief(t, tc.basis, tc.end, bridged)
			if strings.Count(brief, "The call was made, and returned:") != 1 ||
				!strings.Contains(brief, "closed #7") {
				t.Errorf("the lead does not tell once what the approved call returned:\n%s", brief)
			}
			if strings.Contains(brief, "- tr_close_issue(") {
				t.Errorf("the call the lead tells is listed among the round's calls as well:\n%s", brief)
			}
			at := strings.Index(brief, tc.listed)
			if at < 0 || strings.Count(brief, "- jira_transition(") != 1 ||
				!strings.Contains(brief[at:], "- jira_transition(") ||
				!strings.Contains(brief[at:], "posted after the save") {
				t.Errorf("the round's calls are not told under %q, the call an earlier answer approved once "+
					"among them:\n%s", tc.listed, brief)
			}
		})
	}
}
