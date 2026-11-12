package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// A LATE CALL: one the round's log took after its job was replaced by a
// relaunch that carried the round on (runner.BridgedCall.Late), so after the
// brief of the job carrying the round on was built. No brief of the round had
// told it when the log took it, the session of the job that made it may not
// hold it, and no save point holds it.

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

// NO GUARD COUNTS A LOAD THE LOG TOOK LATE. No brief of the round had told a
// skill a late call loaded when the log took the call, and the session of the
// job that made it may not hold it, yet the call's place can fall past the calls the round held when the next
// job's session opened — its own job's session gave it — and its number at or
// below that job's save point. So neither the guard of a relaunch that
// continues a kept session, nor the one of a relaunch that continues its job's
// own copy in its box after its harness ended its own run, which holds every
// load its job made, nor the one a node rebuilds for a run it took over while
// it works holds the skill: the tool it covers is refused until the skill is
// loaded again, the safe direction.
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
	t.Run("a relaunch continuing its job's copy in its box, its harness having ended its own run",
		func(t *testing.T) {
			t.Parallel()
			inBox := runner.SessionBasis{Why: "the newest capture of the job was not kept: refused"}
			active := guarded{basis: inBox}.relaunch(t).Surface.Active()
			req := guarded{basis: inBox, state: execstate.State{ActiveTools: active},
				bridged: []runner.BridgedCall{late}, carry: func(r *runner.Resume) {
					r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: true}
					r.HarnessEnd = sandbox.HarnessEndOwnRun
				}}.relaunch(t)
			if req.Session != nil || req.Continue == nil || req.Continue.SessionID != "sess-1" {
				t.Fatalf("the relaunch continues kept %+v and in place %+v, want the job's own session in its box",
					req.Session, req.Continue)
			}
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
		}, nil, nil, nil, runner.Bridged{}, false)
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
	const lateHeading = "## Calls that finished after the run that made them stopped waiting for them"
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

// A RUN CARRIED ON IN ITS BOX IS TOLD THE CALLS THE LOG TOOK LATE, WHATEVER ITS
// HARNESS'S END. A person's answer or a replaced sign-in that continues the copy
// of the session its job's box holds tells it, after what carries it on, every
// call of the round the log took after the job that made it was replaced —
// here an earlier job's worker that finished after this job opened, which this
// job's brief never told — under the late calls' own words, a submission among
// them as none of the round's, once each, and how many of them the log dropped
// from its middle, whatever the harness's end. Where the job's harness ended its
// own run, its copy holds every other call of the round — here its job's own,
// and the rest its job's brief told — so it is told the late ones alone, with
// the round's tally, and nothing that says it was or may have been cut off, nor
// the notes the turn has read, which it holds; where it was cut off, every call
// of the round, the late ones apart from the rest. Where the job's box is gone,
// which no launch continues in, nothing of it is built, whatever its harness's
// end. All of it is in the brief the launch continues the copy with, which the
// floor of that launch counts where a capture measured the copy.
//
// Mutation: tell an own run's copy nothing, read a late call as one it holds,
// tell a cut-off copy's late calls among the round's other calls, leave the
// late submission's line out, leave the dropped late calls uncounted or count
// them under the late calls' words only for an own run's copy, tell an own
// run's copy it was or may have been cut off or the notes the turn has read, or
// build an own run's telling where its box is gone, and a case goes red.
func TestARunCarriedOnInItsBoxIsToldTheCallsTheLogTookLate(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls that finished after the run that made them stopped waiting for them"
	own := numbered("slack_post", map[string]any{"text": "the job's own post"}, 2, 2)
	late := numbered("jira_create", map[string]any{"summary": "an earlier job's late issue"}, 1, 3)
	late.Late = true
	submitted := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
		Args:   map[string]any{"outcome": "delivered", "summary": "an earlier job's late submission"},
		Result: "submitted"}, Seq: 3, Appended: 4, Late: true}
	// A NOTE THE TURN HAS READ, which a copy whose harness ended its own run
	// holds, and one cut off is told again.
	read := steer.Read{Note: steer.Note{ID: "read", Text: "a note the job read", By: "founder"},
		Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1,
			Boundary: types.SteerBoundaryToolBatch, Launch: "job-1", Order: 1}}
	for name, tc := range map[string]struct {
		end     sandbox.HarnessEnd
		boxGone bool
	}{
		"its harness ended its own run":               {end: sandbox.HarnessEndOwnRun},
		"its harness was cut off":                     {end: sandbox.HarnessEndCutOff},
		"its harness ended its own run, its box gone": {end: sandbox.HarnessEndOwnRun, boxGone: true},
	} {
		for _, carry := range inBoxCarries {
			t.Run(carry.name+", "+name, func(t *testing.T) {
				t.Parallel()
				brief := inPlaceBrief(t, carry, !tc.boxGone, 1, nil, func(r *runner.Resume) {
					r.Bridged, r.HarnessEnd = []runner.BridgedCall{own, late, submitted}, tc.end
					r.Elided, r.LateElided = []ledger.Elided{{Name: "slack_read", Calls: 3}}, 1
					r.Steered, r.Run = []steer.Read{read}, runner.RunRecord{LaunchID: "job-1"}
				})
				at := strings.Index(brief, lateHeading)
				if tc.boxGone {
					if at >= 0 || strings.Contains(brief, "an earlier job's late issue") {
						t.Errorf("a copy whose box is gone, which no launch continues in, is told the calls the "+
							"log took late:\n%s", brief)
					}
					if carry.lead == runner.RotatedContinuation && brief != carry.lead {
						t.Errorf("the copy is told more than what carries it on:\n%s", brief)
					}
					return
				}
				if at < 0 {
					t.Fatalf("the copy is told no call the log took late, under its own words:\n%s", brief)
				}
				apart := brief[at:]
				for _, want := range []string{"an earlier job's late issue", "an earlier job's late submission",
					"does NOT stand as this work's submission", "(+1 further such call,"} {
					if !strings.Contains(apart, want) || strings.Count(brief, want) != 1 {
						t.Errorf("the copy is not told %q once, apart under the late calls' words:\n%s", want, brief)
					}
				}
				if tc.end.EndedOwnRun() {
					for _, not := range []string{"the job's own post", cutOffHeader, prompts.InterruptedCopy,
						prompts.MayBeInterruptedCopy, notesAgainHeader} {
						if strings.Contains(brief, not) {
							t.Errorf("a copy whose harness ended its own run is told %q, which it holds or which is "+
								"untrue of it:\n%s", not, brief)
						}
					}
					if !strings.Contains(apart, "slack_read") {
						t.Errorf("a copy whose harness ended its own run is not told the round's tally beside the "+
							"late call its log dropped:\n%s", brief)
					}
					return
				}
				if since := strings.Index(brief, cutOffHeader); since < 0 || since > at ||
					!strings.Contains(brief[since:at], "the job's own post") {
					t.Errorf("a copy cut off is not told the round's other calls before the late ones:\n%s", brief)
				}
			})
		}
	}
}
