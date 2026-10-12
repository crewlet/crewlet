package runner_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// countingJudge grants every request and counts them: what these cases assert
// is that a phase missing only its submission is NOT the judge's question.
type countingJudge struct{ calls atomic.Int32 }

func (j *countingJudge) Decide(context.Context, extension.Request) (extension.Decision, error) {
	j.calls.Add(1)
	return extension.Decision{Extend: true, Reason: "still making progress"}, nil
}

// readFile is a working round: one call that is not the submission.
func readFile(t *testing.T) llm.Completion {
	t.Helper()
	return submitCall(t, "read_file", `{"path":"/a"}`)
}

// finishingCorrectivesIn counts the finishing correctives one request carries.
func finishingCorrectivesIn(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		if isFinishingCorrective(m, runner.SubmitWorkTool) {
			n++
		}
	}
	return n
}

// A DECLINE ON THE BUDGET'S LAST ROUND IS ASKED AGAIN WHERE THE CEILING ALLOWS.
// The round the budget ends on is the one a phase most naturally submits on,
// and the same fenced JSON one round earlier was re-asked and submitted — so
// the outcome turned on which round the decline landed on, and the ceiling's
// rounds went unspent while the phase was rescued as incomplete. Nor is the
// judge asked: the phase's work is done and only the call reporting it is
// missing.
func TestADeclineOnTheBudgetsLastRoundIsAskedAgainWithinTheCeiling(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		readFile(t), writtenSubmission(), submitWork(t),
	}}
	pub := newCapture()
	judge := &countingJudge{}
	r := extendableRunner(t, prov, pub, judge)

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Rescued || w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %s rescued = %v, want the submitted blocked", w.Outcome, w.Rescued)
	}
	exec := prov.requestsFor("execute")
	if len(exec) != 3 {
		t.Fatalf("the executor was asked %d times, want 3", len(exec))
	}
	if last := exec[2].Messages[len(exec[2].Messages)-1]; !isFinishingCorrective(last, runner.SubmitWorkTool) {
		t.Errorf("round 3 opened on %q, want the withheld finishing corrective", last.Content)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Errorf("the judge was asked %d times about a phase missing only its submission", n)
	}
	done := completedPhase(t, pub, "execute")
	if done.RescueFired || done.RoundsUsed != 3 {
		t.Errorf("rescue_fired = %v rounds_used = %d, want a submitted phase of 3 rounds",
			done.RescueFired, done.RoundsUsed)
	}
}

// The same at the end of an EXTENSION window, which is where the extension
// nudge sends a phase to submit — "if you cannot finish in this window, call
// submit_work" — and so where a written-out submission is likeliest.
func TestADeclineOnTheLastRoundOfAnExtensionIsAskedAgain(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		readFile(t), readFile(t), // the budget, exhausted: the judge grants 4
		readFile(t), readFile(t), readFile(t), writtenSubmission(),
		submitWork(t),
	}}
	judge := &countingJudge{}
	r := extendableRunner(t, prov, newCapture(), judge)

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Rescued || w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %s rescued = %v, want the submitted blocked", w.Outcome, w.Rescued)
	}
	if got := len(prov.requestsFor("execute")); got != 7 {
		t.Errorf("the executor was asked %d times, want 7", got)
	}
	if n := judge.calls.Load(); n != 1 {
		t.Errorf("the judge was asked %d times, want 1 — for the exhausted budget only", n)
	}
}

// WHERE THE PHASE MAY NOT RUN PAST ITS BUDGET, NOTHING CHANGES: the extension
// switch off, or a ceiling equal to the budget, keeps the cap hard, and the
// phase is rescued on the declined round without a corrective nobody reads.
func TestADeclineOnTheLastRoundIsRescuedWhereTheCapIsHard(t *testing.T) {
	t.Parallel()
	for name, caps := range map[string]runner.Caps{
		"extension off":     {ExecutorRounds: 2, ExecutorCeiling: 8, ExtensionStep: 4},
		"ceiling at budget": {ExecutorRounds: 2, ExecutorCeiling: 2, ExtensionOn: true, ExtensionStep: 4},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := &scriptedProvider{execute: []llm.Completion{
				readFile(t), writtenSubmission(), submitWork(t),
			}}
			r := cappedRunner(t, prov, newCapture(), &countingJudge{}, caps)
			w, _, err := r.Execute(context.Background(), 1, "", nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !w.Rescued || w.Outcome != turn.OutcomeIncomplete {
				t.Errorf("outcome = %s rescued = %v, want the rescue", w.Outcome, w.Rescued)
			}
			exec := prov.requestsFor("execute")
			if len(exec) != 2 {
				t.Errorf("the executor was asked %d times, want 2", len(exec))
			}
		})
	}
}

// ONE ALLOWANCE FOR THE PHASE, NOT ONE PER INVOCATION. A run of declines that
// already had a corrective inside the budget gets only the one it has left
// past it: two in all, exactly as if the budget had been one round longer.
func TestAContinuationSharesTheFinishingAllowance(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		writtenSubmission(), writtenSubmission(), writtenSubmission(), writtenSubmission(),
	}}
	r := extendableRunner(t, prov, newCapture(), &countingJudge{})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Rescued {
		t.Error("a phase that never submitted was not rescued")
	}
	exec := prov.requestsFor("execute")
	if len(exec) != 3 {
		t.Fatalf("the executor was asked %d times, want 3 — one answer and two correctives",
			len(exec))
	}
	if got := finishingCorrectivesIn(exec[2]); got != 2 {
		t.Errorf("the last request carries %d finishing correctives, want 2", got)
	}
}

// A CONTINUATION IS AS LONG AS THE CORRECTIVES IT MAY STILL READ, and no
// longer. One that turns back to WORK has stopped being a phase missing only
// its submission: when its rounds run out with the model still calling tools,
// that is an exhausted budget, and the judge decides whether it deserves more.
func TestAContinuationThatGoesBackToWorkIsTheJudgesQuestion(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		writtenSubmission(), writtenSubmission(), // one corrective inside the budget
		readFile(t),   // the one round past it, spent working
		submitWork(t), // in the window the judge granted
	}}
	judge := &countingJudge{}
	r := extendableRunner(t, prov, newCapture(), judge)
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Rescued {
		t.Error("the phase was rescued after it submitted")
	}
	if n := judge.calls.Load(); n != 1 {
		t.Errorf("the judge was asked %d times, want 1 — the continuation ran past "+
			"the corrective it was for", n)
	}
}

// AND NEVER AN EXTENSION BY THE BACK DOOR. A phase that works and declines by
// turns would otherwise earn a fresh pair past every budget it reaches, and
// run on to its ceiling on nobody's decision: the rounds a phase runs past its
// budget without the judge are capped at that same allowance.
func TestRoundsPastTheBudgetWithoutTheJudgeAreBounded(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		readFile(t), writtenSubmission(), // the budget: declines on its last round
		readFile(t), writtenSubmission(), // two rounds past it: works, declines again
		submitWork(t),
	}}
	judge := &countingJudge{}
	r := extendableRunner(t, prov, newCapture(), judge)
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Rescued {
		t.Error("the phase ran past its budget a second time without the judge")
	}
	if got := len(prov.requestsFor("execute")); got != 4 {
		t.Errorf("the executor was asked %d times, want 4", got)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Errorf("the judge was asked %d times, want none", n)
	}
}

// A PHASE THAT SUBMITTED ON ITS BUDGET'S LAST ROUND IS FINISHED, not
// exhausted. Its last message is still a call, which is what an exhausted
// phase's looks like, and it was read as one: the judge was asked, and
// charged, about a phase with nothing left to do, and its grant ran more
// priced rounds after the submission.
func TestASubmissionOnTheLastRoundIsNotExhaustion(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		readFile(t), submitWork(t), text("anything further is a wasted round"),
	}}
	pub := newCapture()
	judge := &countingJudge{}
	r := extendableRunner(t, prov, pub, judge)
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Rescued || w.ExhaustedRounds {
		t.Errorf("rescued = %v exhausted = %v, want a finished phase", w.Rescued, w.ExhaustedRounds)
	}
	if got := len(prov.requestsFor("execute")); got != 2 {
		t.Errorf("the executor was asked %d times, want 2", got)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Errorf("the judge was asked %d times about a phase that had submitted", n)
	}
	if done := completedPhase(t, pub, "execute"); done.ExhaustedRounds {
		t.Error("the phase record calls a submitted phase exhausted")
	}
}
