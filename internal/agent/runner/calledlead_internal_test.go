package runner

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A PARKED CALL'S LEAD IS TOLD AGAIN AS THE CALL, NEVER AS AN ANSWER. The
// answer that carried a run parked on its own call on is kept as its lead, so
// a later brief to a session that may lack it tells it again: as the call the
// run waited for, which ran once, with what it returned, never as a person
// answering a question the run never put to anybody.
//
// Mutation: keep the lead without its call (Called dropped from Asked.lead),
// and the later brief says the person it asked answered.
func TestAParkedCallsLeadIsToldAgainAsTheCallNeverAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	asked := Asked{Question: "Waiting for its call to return: delegate", Answer: "Your call to delegate returned.",
		Called: []types.CallReturned{{Tool: "delegate", Seq: 4}}, CallsReturned: true}
	asked.findReturned([]BridgedCall{{Call: ledger.Call{Name: "delegate", Result: "the workers are done"}, Seq: 4}},
		nil)
	lead := asked.lead()
	if lead.Answer == nil {
		t.Fatal("the answer keeps no lead")
	}
	told := answerAgain(*lead.Answer, nil, answerAt{}, b)
	if !strings.Contains(told, "called `delegate`") || !strings.Contains(told, "ran once") ||
		!strings.Contains(told, "the workers are done") ||
		strings.Contains(told, "answered") {
		t.Errorf("the parked call's lead is told again as:\n%s", told)
	}
}

// A WAIT'S LEAD IS TOLD AGAIN AS THE RUN IT WAITED FOR, NEVER AS AN ANSWER. The
// engine answers a `wait_for_run` with how the run waited for left its seat
// (ADR-0176), and a later brief to a session that may lack it tells that lead
// again: as that run's account, never as a person answering a question the
// run put to nobody.
//
// Mutation: keep the lead without its account (Waited dropped from
// Asked.lead), and the later brief says the person it asked answered.
func TestAWaitsLeadIsToldAgainAsTheRunItWaitedForNeverAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	asked := Asked{Question: "Waiting for another run: it pushes the branch first",
		Answer: "The run you waited for, run-t, left its seat: ended.",
		Waited: &types.RunWaited{Turn: "run-t", Seat: "swe", Left: types.RunLeftEnded, Outcome: "completed",
			Pushed: types.RunPushedNone}}
	lead := asked.lead()
	if lead.Answer == nil {
		t.Fatal("the answer keeps no lead")
	}
	told := answerAgain(*lead.Answer, nil, answerAt{}, b)
	if !strings.Contains(told, "Run `run-t` of swe ended: completed.") ||
		!strings.Contains(told, "it pushes the branch first") || strings.Contains(told, "answered") {
		t.Errorf("the wait's lead is told again as:\n%s", told)
	}
}
