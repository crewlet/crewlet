package runner

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
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
		Called: []types.CallReturned{{Tool: "delegate", Seq: 4}}, CallsReturned: true}.withReturned(
		[]BridgedCall{{Call: ledger.Call{Name: "delegate", Result: "the workers are done"}, Seq: 4}}, nil)
	lead := asked.lead()
	if lead.Answer == nil {
		t.Fatal("the answer keeps no lead")
	}
	told := answerAgain(lead.Kind, *lead.Answer, nil, answerAt{}, b)
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
	told := answerAgain(lead.Kind, *lead.Answer, nil, answerAt{}, b)
	if !strings.Contains(told, "Run `run-t` of swe ended: completed.") ||
		!strings.Contains(told, "it pushes the branch first") || strings.Contains(told, "answered") {
		t.Errorf("the wait's lead is told again as:\n%s", told)
	}
}

// AN EARLIER LEAD NOBODY ANSWERED IS NEVER INTRODUCED AS AN ANSWER. A fresh
// run, and a task told again, tell every earlier lead that carried the work
// on. Where the engine carried a run on with the calls it parked on or the run
// it waited for, the section tells what was waited for: never that a run
// stopped to ask, nor any answer, since nobody was asked anything.
//
// Mutation: introduce every earlier lead as an answer (earlierKind answering
// EarlierAnswer), and the section says a run stopped to ask.
func TestAnEarlierLeadNobodyAnsweredIsNeverIntroducedAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	called := Asked{Question: "Waiting for its call to return: delegate", Answer: "Your call to delegate returned.",
		Called: []types.CallReturned{{Tool: "delegate", Seq: 4}}, CallsReturned: true}.withReturned(
		[]BridgedCall{{Call: ledger.Call{Name: "delegate", Result: "the workers are done"}, Seq: 4}}, nil)
	waited := Asked{Question: "Waiting for another run: it pushes the branch first",
		Answer: "The run you waited for, run-t, left its seat: ended.",
		Waited: &types.RunWaited{Turn: "run-t", Seat: "swe", Left: types.RunLeftEnded, Outcome: "completed",
			Pushed: types.RunPushedNone}}
	for name, asked := range map[string]Asked{"calls returned": *called, "a run waited for": waited} {
		lead := asked.lead()
		lead.Round = 1
		for _, fresh := range []bool{true, false} {
			section := earlierAnswers([]ResumeLead{{Lead: *lead}}, 1, nil, 0, fresh, b).section.Text
			if section == "" || strings.Contains(section, "stopped to ask") ||
				strings.Contains(strings.ToLower(section), "answer") {
				t.Errorf("%s, fresh %v: the earlier lead is told as an answer:\n%s", name, fresh, section)
			}
		}
	}
}

// THE ENGINE'S ANSWER KEEPS A LEAD OF ITS OWN KIND, A PERSON'S AN ANSWER'S. The
// engine's answer that a run's parked calls returned, and its answer to a
// `wait_for_run`, are sealed under kinds of their own (sandbox.LeadKindCalled,
// sandbox.LeadKindWaited), so a build that does not know them tells them as a
// message it cannot read, as this one tells any kind it does not know, never
// as a person's answer. A person's answer with calls beside it stays an answer.
//
// Mutation: keep every lead as sandbox.LeadKindAnswer in Asked.lead, and a case
// goes red.
func TestTheEnginesAnswerKeepsALeadOfItsOwnKind(t *testing.T) {
	t.Parallel()
	called := []types.CallReturned{{Tool: "delegate", Seq: 4}}
	for name, tc := range map[string]struct {
		asked Asked
		kind  sandbox.LeadKind
	}{
		"its calls returned": {asked: Asked{Question: "Waiting for its call to return: delegate",
			Answer: "Your call to delegate returned.", Called: called, CallsReturned: true},
			kind: sandbox.LeadKindCalled},
		"a wait answered": {asked: Asked{Question: "Waiting for another run", Answer: "It left its seat.",
			Waited: &types.RunWaited{Turn: "run-t", Seat: "swe", Left: types.RunLeftEnded}},
			kind: sandbox.LeadKindWaited},
		"a person's answer beside its calls": {asked: Asked{Question: "Which branch?", Answer: "Use main.",
			Called: called}, kind: sandbox.LeadKindAnswer},
	} {
		if lead := tc.asked.lead(); lead.Kind != tc.kind || !lead.Kind.Valid() {
			t.Errorf("%s: the lead is kept as %q, want %q", name, lead.Kind, tc.kind)
		}
	}
	unknown := ResumeLead{Lead: sandbox.Lead{Kind: "verdict", Round: 1, Unread: "of a kind this build does not know"}}
	if told, _, _ := (&Runner{}).leadAgain(t.Context(), unknown, nil, nil, 1, provenance.NewBoundary()); told !=
		prompts.LeadUnread {
		t.Errorf("a lead of a kind this build does not know is told as:\n%s", told)
	}
}
