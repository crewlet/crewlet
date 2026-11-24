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
// relaunch carrying the round on (ledger.Call.Late), so after that
// relaunch's brief was built. No brief of the round told it, the session of
// the job that made it may not hold it, and no save point holds it.

// EVERY CONTINUATION OF THE ROUND IS TOLD A LATE CALL, WHATEVER ITS SAVE POINT:
// even one whose save point lies past the late call's number, where every
// other call at or below it is one its session holds and stays untold.
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

// NO GUARD COUNTS A LOAD THE LOG TOOK LATE. No brief told the round the skill
// a late call loaded and the session may not hold it, yet the call's place
// (its own job's session gave it) can fall past the calls the round held when
// the next job's session opened, and its number at or below that job's save
// point. So no guard holds the skill: not a relaunch's continuing a kept
// session, nor one continuing its job's own copy in its box after its harness
// ended its own run (which holds every load its job made), nor a node's for a
// run it took over while it works. The covered tool is refused until the skill
// is loaded again, the safe direction.
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
			inBox := runner.SessionBasis{}
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
				Result: "Always thread your reply.", Late: true}, Seq: 1, Appended: 1},
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
// the round on was never told it and makes the round's submission itself. So a
// lost round whose only submission is late is carried on, and one its own job
// submitted is collected.
//
// Mutation: read a late call as the round's submission, and the lost round is
// collected on it.
func TestALateSubmissionIsNeverTheRounds(t *testing.T) {
	t.Parallel()
	lost := &runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	submitted := func(late bool) []runner.BridgedCall {
		return []runner.BridgedCall{{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args: map[string]any{"outcome": "delivered", "summary": "posted it"}, Late: late}, Seq: 1, Appended: 1}}
	}
	if !(runner.Resume{Lost: lost, Bridged: submitted(true)}).CarriesOn() {
		t.Error("a lost round whose only submission its log took late is collected on it, want it carried on")
	}
	if (runner.Resume{Lost: lost, Bridged: submitted(false)}).CarriesOn() {
		t.Error("a lost round its own job submitted is carried on, want it collected")
	}
}

// A LATE CALL IS TOLD APART, UNDER WORDS OF ITS OWN, WHATEVER THE SESSION'S END.
// It finished only after its job's harness died, gave up on it or was replaced,
// so the session may show it with no outcome or an interrupted one. Told among
// the calls after the save, a session saved where it ends reads it as a call it
// lacks, and one showing it interrupted takes it for a second call or one to
// make again. So it has a section of its own after the calls since the save,
// under the same words whether the harness ended its own run or was cut off.
//
// Mutation: tell a late call among the calls after the save, and it is told
// under words that say the session lacks it.
func TestALateCallIsToldApartUnderWordsOfItsOwn(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
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
		// HELD, so in no list of calls the session may lack, and LISTED AGAIN
		// only where it was cut off. The call after the save, told as one the
		// session may already show, names the same tool, and neither call's
		// arguments carry a digest that would let their lines read apart.
		told, again, _ := strings.Cut(since, listedAgainHeading)
		if strings.Contains(told, "the held one") || strings.Contains(apart, "the held one") {
			t.Errorf("a session (cut off: %v) is told a call it holds as one it may lack:\n%s", interrupted, brief)
		}
		if strings.Contains(again, "the held one") != interrupted {
			t.Errorf("a session (cut off: %v) lists the held call again %v, want %v:\n%s", interrupted,
				strings.Contains(again, "the held one"), interrupted, brief)
		}
	}
}

// A LATE SUBMISSION IS TOLD AS NONE OF THE ROUND'S. Every continuation after
// the first, which was never told it, is told the call with the line that it
// does not stand, so the run submits its own rather than ending on one it reads
// as done and being collected as one that submitted nothing. A late submission
// that failed is told as any failed call, without the line.
//
// Mutation: leave the line out, and a run told a late submission ran is not told
// it must make its own.
func TestALateSubmissionIsToldAsNoneOfTheRounds(t *testing.T) {
	t.Parallel()
	const notStanding = "does NOT stand as this work's submission"
	for _, failed := range []bool{false, true} {
		late := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args:   map[string]any{"outcome": "delivered", "summary": "posted it"},
			Result: "submitted", Failed: failed, Late: true}, Seq: 2, Appended: 2}
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
// HARNESS'S END. An answer or replaced sign-in continuing the box's copy tells
// it, after what carries it on, each late call once under the late calls' words
// (a submission as none of the round's) and how many the log dropped. Ended its
// own run: the copy holds every other call and the notes read, so it gets the
// late ones alone with the round's tally and no word of being cut off. Cut off:
// every call, the late ones apart. Box gone: nothing, as no launch continues
// there. It all goes in the brief the launch's floor counts, measured or not.
//
// Mutation: tell an own run's copy nothing, a late call as held, a cut-off word
// or the notes; mix a cut-off copy's late calls in; drop the submission's line
// or either end's dropped count; or tell a copy whose box is gone, and a case goes red.
func TestARunCarriedOnInItsBoxIsToldTheCallsTheLogTookLate(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
	own := numbered("slack_post", map[string]any{"text": "the job's own post"}, 2, 2)
	late := numbered("jira_create", map[string]any{"summary": "an earlier job's late issue"}, 1, 3)
	late.Late = true
	submitted := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
		Args:   map[string]any{"outcome": "delivered", "summary": "an earlier job's late submission"},
		Result: "submitted", Late: true}, Seq: 3, Appended: 4}
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

// A LATE SUBMISSION IS TOLD ONCE, UNDER ITS OWN WORDS: in the late calls'
// section, with the line that it does not stand because the run that made it
// had stopped waiting for it. The calls after the save hold no submission, so
// their section carries no line saying the work was carried on after one.
//
// Mutation: say a late submission among the calls after the save as well, and
// the case goes red.
func TestALateSubmissionIsToldOnceUnderItsOwnWords(t *testing.T) {
	t.Parallel()
	const (
		lateHeading  = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
		notStanding  = "does NOT stand as this work's submission"
		lateWords    = "the run that made it had stopped waiting for it before it returned"
		earlierWords = "the work was carried on after it"
	)
	late := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
		Args:   map[string]any{"outcome": "delivered", "summary": "the late submission"},
		Result: "submitted", Late: true}, Seq: 3, Appended: 3}
	brief := afterSaveBrief(t, 1, []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "held by the session"}, 1, 1),
		numbered("jira_create", map[string]any{"summary": "the call after the save"}, 2, 2),
		late,
	}, nil)
	since, apart, found := strings.Cut(brief, lateHeading)
	if !found {
		t.Fatalf("the late submission is not told apart, under the late calls' words:\n%s", brief)
	}
	if !strings.Contains(since, "the call after the save") {
		t.Fatalf("the call after the save is not told before the late calls:\n%s", brief)
	}
	if strings.Contains(since, earlierWords) {
		t.Errorf("the calls after the save, which hold no submission, are told one among them does not stand:\n%s",
			brief)
	}
	if !strings.Contains(apart, "the late submission") || !strings.Contains(apart, lateWords) {
		t.Errorf("the late submission is not told under the late calls' words, with the line that it does not "+
			"stand:\n%s", brief)
	}
	if n := strings.Count(brief, notStanding); n != 1 {
		t.Errorf("the session is told %d times that a submission does not stand, want once:\n%s", n, brief)
	}
}

// A LATE CALL WITH NO PLACE IS TOLD BY THE NEXT ROUND'S CONTINUATION. A call of
// an earlier round that the next round's log took late has no place
// (sandbox.BridgeCall.Seq zero): a continuation of that round is told it under
// the late calls' words whatever its save point, and a late `submit_work` with
// no place is never the round's submission.
//
// Mutation: skip the calls with no place in the continuation's walk, and the
// earlier round's call goes untold.
func TestALateCallWithNoPlaceIsToldByTheNextRoundsContinuation(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
	earlier := runner.BridgedCall{Call: ledger.Call{Name: "jira_create",
		Args: map[string]any{"summary": "the earlier round's"}, Result: "done", Late: true}, Appended: 2}
	brief := afterSaveBrief(t, 3, []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "held by the session"}, 1, 1),
		earlier,
		numbered("slack_post", map[string]any{"text": "held too"}, 2, 3),
	}, nil)
	if _, apart, found := strings.Cut(brief, lateHeading); !found || !strings.Contains(apart, "the earlier round's") {
		t.Errorf("the continuation is not told the earlier round's call under the late calls' words:\n%s", brief)
	}
	lost := &runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	submitted := []runner.BridgedCall{{Call: ledger.Call{Name: runner.SubmitWorkTool,
		Args: map[string]any{"outcome": "delivered", "summary": "posted it"}, Late: true}, Appended: 1}}
	if !(runner.Resume{Lost: lost, Bridged: submitted}).CarriesOn() {
		t.Error("a lost round whose only submission is an earlier round's, with no place, is collected on it, " +
			"want it carried on")
	}
}
