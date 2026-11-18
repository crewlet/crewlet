package runner_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
)

// THE WITHHELD FINISHING CORRECTIVE, on the onboarding pass (the one
// engine-driven phase whose ceiling sits above its budget; it finishes by
// calling mark_onboarded). A pass answering in prose on its budget's last round
// is missing only the call that reports its work, so it is asked again where
// its ceiling allows, without the extension judge, within one allowance.

// countingJudge grants every request and counts them: a phase missing only its
// finishing call is NOT the judge's question.
type countingJudge struct{ calls atomic.Int32 }

func (j *countingJudge) Decide(context.Context, extension.Request) (extension.Decision, error) {
	j.calls.Add(1)
	return extension.Decision{Extend: true, Reason: "still making progress"}, nil
}

// working is a round that works: one call that is not the finishing one.
func working(t *testing.T) llmtest.Response {
	t.Helper()
	return persist(t, "a", "reading")
}

// declining is a round that answers in prose where the pass had to end in its
// finishing call.
func declining() llmtest.Response { return text("I have read everything I need.") }

// finishingCorrectivesIn counts the finishing correctives one request carries.
func finishingCorrectivesIn(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		if isFinishingCorrective(m, toolname.MarkOnboarded) {
			n++
		}
	}
	return n
}

// passed runs the pass and returns its record: decision "done" is a pass that
// called mark_onboarded, anything else one that ended without it.
func passed(t *testing.T, r *runner.Runner, pub *capture) *types.AgentPhaseCompleted {
	t.Helper()
	onboard(t, r)
	return completedPhase(t, pub, "onboarding")
}

// A DECLINE ON THE BUDGET'S LAST ROUND IS ASKED AGAIN WHERE THE CEILING ALLOWS,
// as one a round earlier is: otherwise the outcome turns on which round the
// decline lands on, and the pass ends unmarked with ceiling rounds unspent. Nor
// is the judge asked: only the call reporting the work is missing.
func TestADeclineOnTheBudgetsLastRoundIsAskedAgainWithinTheCeiling(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		working(t), declining(), markDone(t, "done"),
	}}
	pub := newCapture()
	judge := &countingJudge{}
	done := passed(t, extendableRunner(t, prov, pub, judge), pub)

	if done.Decision != "done" {
		t.Fatalf("decision = %q, want the pass that marked", done.Decision)
	}
	if got := prov.roundsOf("onboarding"); got != 3 {
		t.Fatalf("the pass was asked %d times, want 3", got)
	}
	// Round 3 is a harness run of its own, opened by the declined round's corrective.
	reqs := prov.requestsFor("onboarding")
	if len(reqs) != 2 {
		t.Fatalf("the pass took %d harness runs, want the declined one and the corrected one", len(reqs))
	}
	if last := reqs[1].Messages[len(reqs[1].Messages)-1]; !isFinishingCorrective(last, toolname.MarkOnboarded) {
		t.Errorf("round 3 opened on %q, want the withheld finishing corrective", last.Content)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Errorf("the judge was asked %d times about a phase missing only its finishing call", n)
	}
	if done.RoundsUsed != 3 {
		t.Errorf("rounds_used = %d, want a finished phase of 3 rounds", done.RoundsUsed)
	}
}

// The same at the end of an EXTENSION window, where the extension nudge sends a
// phase to finish, so where a decline is likeliest.
func TestADeclineOnTheLastRoundOfAnExtensionIsAskedAgain(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		working(t), working(t), // the budget, exhausted: the judge grants 4
		working(t), working(t), working(t), declining(),
		markDone(t, "done"),
	}}
	pub := newCapture()
	judge := &countingJudge{}
	done := passed(t, extendableRunner(t, prov, pub, judge), pub)

	if done.Decision != "done" {
		t.Fatalf("decision = %q, want the pass that marked", done.Decision)
	}
	if got := prov.roundsOf("onboarding"); got != 7 {
		t.Errorf("the pass was asked %d times, want 7", got)
	}
	if n := judge.calls.Load(); n != 1 {
		t.Errorf("the judge was asked %d times, want 1 — for the exhausted budget only", n)
	}
}

// WHERE THE PHASE MAY NOT RUN PAST ITS BUDGET, NOTHING CHANGES: the extension
// switch off, or a ceiling equal to the budget, keeps the cap hard, and the
// pass ends on the declined round without a corrective nobody reads.
func TestADeclineOnTheLastRoundEndsThePassWhereTheCapIsHard(t *testing.T) {
	t.Parallel()
	for name, caps := range map[string]loopCaps{
		"extension off":     {rounds: 2, ceiling: 8, step: 4},
		"ceiling at budget": {rounds: 2, ceiling: 2, extensionOn: true, step: 4},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := &scriptedProvider{onboarding: []llmtest.Response{
				working(t), declining(), markDone(t, "done"),
			}}
			pub := newCapture()
			done := passed(t, cappedRunner(t, prov, pub, &countingJudge{}, caps), pub)
			if done.Decision == "done" {
				t.Error("the pass ran past a hard cap to mark")
			}
			if got := prov.roundsOf("onboarding"); got != 2 {
				t.Errorf("the pass was asked %d times, want 2", got)
			}
		})
	}
}

// ONE ALLOWANCE FOR THE PHASE, NOT ONE PER INVOCATION: declines that had a
// corrective inside the budget get only one more past it, two in all.
func TestAContinuationSharesTheFinishingAllowance(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		declining(), declining(), declining(), declining(),
	}}
	pub := newCapture()
	done := passed(t, extendableRunner(t, prov, pub, &countingJudge{}), pub)
	if done.Decision == "done" {
		t.Error("a pass that never called its finishing tool reads as marked")
	}
	reqs := prov.requestsFor("onboarding")
	if len(reqs) != 3 {
		t.Fatalf("the pass was asked %d times, want 3 — one answer and two correctives", len(reqs))
	}
	if got := finishingCorrectivesIn(reqs[2]); got != 2 {
		t.Errorf("the last request carries %d finishing correctives, want 2", got)
	}
}

// A CONTINUATION IS AS LONG AS THE CORRECTIVES IT MAY STILL READ. One that
// turns back to WORK is no longer missing only its finishing call: its rounds
// running out mid-call are an exhausted budget, and the judge decides.
func TestAContinuationThatGoesBackToWorkIsTheJudgesQuestion(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		declining(), declining(), // one corrective inside the budget
		working(t),          // the one round past it, spent working
		markDone(t, "done"), // in the window the judge granted
	}}
	pub := newCapture()
	judge := &countingJudge{}
	done := passed(t, extendableRunner(t, prov, pub, judge), pub)
	if done.Decision != "done" {
		t.Error("the pass did not mark in the window the judge granted")
	}
	if n := judge.calls.Load(); n != 1 {
		t.Errorf("the judge was asked %d times, want 1 — the continuation ran past "+
			"the corrective it was for", n)
	}
}

// AND NEVER AN EXTENSION BY THE BACK DOOR: rounds past the budget without the
// judge are capped at that same allowance, or a phase working and declining by
// turns runs on to its ceiling on nobody's decision.
func TestRoundsPastTheBudgetWithoutTheJudgeAreBounded(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		working(t), declining(), // the budget: declines on its last round
		working(t), declining(), // two rounds past it: works, declines again
		markDone(t, "done"),
	}}
	pub := newCapture()
	judge := &countingJudge{}
	done := passed(t, extendableRunner(t, prov, pub, judge), pub)
	if done.Decision == "done" {
		t.Error("the pass ran past its budget a second time without the judge")
	}
	if got := prov.roundsOf("onboarding"); got != 4 {
		t.Errorf("the pass was asked %d times, want 4", got)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Errorf("the judge was asked %d times, want none", n)
	}
}

// A PHASE THAT FINISHED ON ITS BUDGET'S LAST ROUND IS FINISHED, not exhausted,
// though its last message is a call as an exhausted phase's is: otherwise the
// judge is asked, and charged, and its grant runs priced rounds past the
// finishing call.
func TestAFinishingCallOnTheLastRoundIsNotExhaustion(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		working(t), markDone(t, "done"), text("anything further is a wasted round"),
	}}
	pub := newCapture()
	judge := &countingJudge{}
	done := passed(t, extendableRunner(t, prov, pub, judge), pub)
	if done.Decision != "done" || done.ExhaustedRounds {
		t.Errorf("decision = %q exhausted = %v, want a finished phase", done.Decision, done.ExhaustedRounds)
	}
	if got := prov.roundsOf("onboarding"); got != 2 {
		t.Errorf("the pass was asked %d times, want 2", got)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Errorf("the judge was asked %d times about a phase that had finished", n)
	}
}
