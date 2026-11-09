package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// A LATE CALL: one the round's log took after its job was replaced by a
// relaunch that carried the round on (runner.BridgedCall.Late), so after the
// brief of the job carrying the round on was built. No brief of the round told
// it, the session of the job that made it may not hold it, and no save point
// holds it.

// EVERY CONTINUATION OF THE ROUND IS TOLD A LATE CALL, WHATEVER ITS SAVE POINT.
// A later capture of the job that carried the round on, placed by its own read
// of the log, has a save point past the late call's number, and every other
// call at or below it is one its session holds — so the continuation of that
// capture is told the late call with its outcome all the same, and still none
// of the calls its session holds.
//
// Mutation: read a late call against the save point as any other, and it goes
// untold.
func TestEveryContinuationOfTheRoundIsToldALateCall(t *testing.T) {
	t.Parallel()
	const atRead = 3 // the calls the log had taken when the capture read it
	late := numbered("jira_create", map[string]any{"summary": "the late one"}, 2, 2)
	late.Late = true
	brief := afterSaveBrief(t, atRead, []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "before it"}, 1, 1),
		late,
		numbered("slack_post", map[string]any{"text": "after it"}, 2, 3),
	}, nil)
	for _, want := range []string{"jira_create", "the late one"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the continuation is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "slack_post") {
		t.Errorf("the continuation is told a call its session holds:\n%s", brief)
	}
}

// NO GUARD COUNTS A LOAD THE LOG TOOK LATE. No brief of the round told a skill a
// late call loaded, and the session of the job that made the call may not hold
// it, yet the call's place can fall past the calls the round held when the next
// job's session opened — its own job's session gave it — and its number at or
// below that job's save point. So neither the guard
// of a relaunch that continues a kept session nor the one a node rebuilds for
// a run it took over while it works holds the skill: the tool it covers is
// refused until the skill is loaded again, the safe direction.
//
// Mutation: count a late call's load as any other's, and the covered tool is
// let through.
func TestNoGuardCountsALoadTheLogTookLate(t *testing.T) {
	t.Parallel()
	late := loadSkill(1)
	late.Late = true
	t.Run("a relaunch continuing a kept session", func(t *testing.T) {
		t.Parallel()
		req := guarded{basis: keptBasis, bridged: []runner.BridgedCall{late}}.relaunch(t)
		bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
		if !bridged[0].Failed {
			t.Fatalf("slack_post ran (%q) on a skill a late call loaded", bridged[0].Result)
		}
	})
	t.Run("a run taken over while it works", func(t *testing.T) {
		t.Parallel()
		r, _ := guardedRunner(t, newCapture(), tools.OriginBuiltin)
		reopened, err := r.ReopenAgentRun(t.Context(), execstate.State{
			Version: execstate.Version, Round: 1,
			ActiveTools: []string{"slack_post", toolname.LoadToolSkill},
		}, []runner.BridgedCall{
			{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "chat-conventions"},
				Result: "Always thread your reply."}, Seq: 1, Appended: 1, Late: true},
		}, nil, nil, runner.Bridged{}, false)
		if err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		res, err := reopened.Surface.Execute(t.Context(), llm.ToolCall{
			Name: "slack_post", Arguments: map[string]any{"channel": "C1"},
		})
		if err != nil {
			t.Fatalf("slack_post: %v", err)
		}
		if !res.Failed {
			t.Errorf("slack_post ran (%q) on a skill a late call loaded", res.Output)
		}
	})
}

// A LATE SUBMISSION IS NEVER THE ROUND'S. A `submit_work` the round's log took
// after its job was replaced was made by a job that is over; the job carrying
// the round on was never told it and goes on to make the round's submission
// itself. So a lost job's round whose only submission is late is carried on
// rather than collected on it, and one its own job submitted is collected.
//
// Mutation: read a late call as the round's submission, and the lost round is
// collected on it.
func TestALateSubmissionIsNeverTheRounds(t *testing.T) {
	t.Parallel()
	lost := &runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	submitted := func(late bool) []runner.BridgedCall {
		return []runner.BridgedCall{{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args: map[string]any{"outcome": "delivered", "summary": "posted it"}}, Seq: 1, Appended: 1, Late: late}}
	}
	if !(runner.Resume{Lost: lost, Bridged: submitted(true)}).CarriesOn() {
		t.Error("a lost round whose only submission its log took late is collected on it, want it carried on")
	}
	if (runner.Resume{Lost: lost, Bridged: submitted(false)}).CarriesOn() {
		t.Error("a lost round its own job submitted is carried on, want it collected")
	}
}

// A LATE CALL IS TOLD APART, UNDER WORDS OF ITS OWN, WHATEVER THE SESSION'S END.
// A call the round's log took after its job was replaced finished only after
// that job's harness died, gave up on it or was replaced, so the session may
// show it with no outcome or an interrupted one, and every continuation of the
// round tells it again. Told among the calls after the save, a session saved
// where it ends reads it as a call it lacks, and one that shows it interrupted
// takes it for a second call, or a call to make again. So it is told in a
// section of its own, after the calls since the save, under the same words
// whether the session's harness ended its own run or was cut off, and never
// among the calls after the save.
//
// Mutation: tell a late call among the calls after the save, and it is told
// under words that say the session lacks it.
func TestALateCallIsToldApartUnderWordsOfItsOwn(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls that finished after the run that made them was replaced"
	late := numbered("jira_create", map[string]any{"summary": "the late one"}, 2, 2)
	late.Late = true
	bridged := []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "the held one"}, 1, 1),
		late,
		numbered("slack_post", map[string]any{"text": "the ordinary one"}, 3, 4),
	}
	for _, interrupted := range []bool{false, true} {
		basis := keptBasis
		basis.Through, basis.Interrupted = 3, interrupted
		brief := lostRelaunch(t, basis, runner.Resume{Bridged: bridged}).Session.Brief
		since, apart, found := strings.Cut(brief, lateHeading)
		if !found {
			t.Fatalf("a session (cut off: %v) is told no late call apart:\n%s", interrupted, brief)
		}
		if !strings.Contains(apart, "the late one") || strings.Contains(since, "the late one") {
			t.Errorf("a session (cut off: %v) is told the late call among the calls after its save:\n%s",
				interrupted, brief)
		}
		if !strings.Contains(since, "the ordinary one") || strings.Contains(apart, "the ordinary one") {
			t.Errorf("a session (cut off: %v) is told the call after its save apart from them:\n%s",
				interrupted, brief)
		}
		if strings.Contains(brief, "the held one") {
			t.Errorf("a session (cut off: %v) is told a call it holds:\n%s", interrupted, brief)
		}
	}
}

// A LATE SUBMISSION IS TOLD AS NONE OF THE ROUND'S. A `submit_work` the round's
// log took late was made by a job that is over, and is never the round's
// submission: a lost round whose only submission is late is carried on, and
// every continuation after the first, which was never told it, is told the call
// with the line that it does not stand, so the run submits its own rather than
// ending on one it reads as done — and is collected as one that submitted
// nothing. A late submission that failed is told as any failed call, without
// it.
//
// Mutation: leave the line out, and a run told a late submission ran is not told
// it must make its own.
func TestALateSubmissionIsToldAsNoneOfTheRounds(t *testing.T) {
	t.Parallel()
	const notStanding = "does NOT stand as this work's submission"
	for _, failed := range []bool{false, true} {
		late := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args:   map[string]any{"outcome": "delivered", "summary": "posted it"},
			Result: "submitted", Failed: failed}, Seq: 2, Appended: 2, Late: true}
		brief := afterSaveBrief(t, 1, []runner.BridgedCall{
			numbered("slack_post", map[string]any{"text": "posted"}, 1, 1), late}, nil)
		if !strings.Contains(brief, "posted it") {
			t.Fatalf("the late submission (failed: %v) is not told:\n%s", failed, brief)
		}
		if said := strings.Contains(brief, notStanding); said == failed {
			t.Errorf("a late submission that failed (%v) is told %q: %v\n%s", failed, notStanding, said, brief)
		}
	}
}
