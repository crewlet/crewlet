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
// log keeps it: arguments and output withheld (they ride the run's row
// sealed), the seq'th the bridge admitted and the appended'th the log took.
func approvedFor(tool, heldID string, seq, appended int) runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: tool, Withheld: true}, Seq: seq, Appended: appended,
		Approved: true, HeldID: heldID}
}

// heldFor is the call the run held for a person's approval, as the round's log
// keeps it: arguments withheld, recorded failed though it is the run's
// question, the seq'th the bridge admitted and the appended'th the log took.
func heldFor(tool string, seq, appended int) runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: tool, Withheld: true, Failed: true,
		Result: "held for a person's approval"}, Seq: seq, Appended: appended, Held: true}
}

// laterAttemptBrief is the brief a later attempt at an answer carries the round
// on with: an earlier attempt made the once-approved `tr_close_issue` call
// ("closed #7") and its relaunch was refused; this one reads the round's log as
// bridged. It continues the kept session basis names, else the asking job's
// box copy, whose harness ended as end says. refused, where set, is a reply
// too large to take, beside the held call.
func laterAttemptBrief(t *testing.T, basis runner.SessionBasis, end sandbox.HarnessEnd,
	bridged []runner.BridgedCall, refused *runner.RefusedReply) string {

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
				SessionID: "sess-ask", BoxKept: true, Refused: refused,
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

// A LATER ATTEMPT AT AN ANSWER TELLS THE CALL IT APPROVED ONCE, IN ITS LEAD.
// An earlier attempt made the approved call and its relaunch was refused; this
// one reads the call last in the round's log and tells what it returned in the
// lead, so neither a kept session's continuation nor the box's copy lists it
// again. The held call and a call an earlier answer approved are listed once
// each. A refused reply's lead tells the refusal alone, no held call, and the
// approved call is listed as any call is.
//
// Mutation: list the lead's approved call among a continuation's or the box
// copy's calls; leave out every approved call, or the held one; read no held
// call off the answer; or name the held call in a refused reply's lead, and a
// case goes red.
func TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnce(t *testing.T) {
	t.Parallel()
	// The round's log: posts either side of the kept capture's save, a call an
	// earlier answer approved, and, last, the call this answer approved.
	bridged := []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "posted before the save"}, 1, 1),
		approvedFor("jira_transition", "held-earlier", 2, 2),
		numbered("slack_post", map[string]any{"text": "posted after the save"}, 3, 3),
		heldFor("tr_close_issue", 4, 4),
		approvedFor("tr_close_issue", "held-1", 5, 5),
	}
	const (
		heldLine     = "- tr_close_issue(arguments sealed for a person's approval, kept on no record) → error:"
		approvedLine = "- tr_close_issue(arguments sealed for a person's approval, kept on no record) → success"
	)
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
			brief := laterAttemptBrief(t, tc.basis, tc.end, bridged, nil)
			if strings.Count(brief, "The call was made, and returned:") != 1 ||
				!strings.Contains(brief, "closed #7") {
				t.Errorf("the lead does not tell once what the approved call returned:\n%s", brief)
			}
			if strings.Contains(brief, approvedLine) || strings.Count(brief, heldLine) != 1 {
				t.Errorf("the call the lead tells is listed among the round's calls as well, or the held "+
					"call is not listed once:\n%s", brief)
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
	t.Run("a reply refused as too large, beside the held call", func(t *testing.T) {
		t.Parallel()
		brief := laterAttemptBrief(t, cutOff, sandbox.HarnessEndCutOff, bridged,
			&runner.RefusedReply{Messages: 1, Bytes: 70 << 10, MaxBytes: 64 << 10})
		if strings.Contains(brief, "The call was made, and returned:") ||
			strings.Count(brief, approvedLine) != 1 || strings.Count(brief, heldLine) != 1 {
			t.Errorf("a refused reply's relaunch does not list the approved call and the held one once each, "+
				"with no lead telling the approved one:\n%s", brief)
		}
	})
}
