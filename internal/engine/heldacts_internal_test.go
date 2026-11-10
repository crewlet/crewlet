package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ANSWER THAT WOKE A TURN IS STILL ITS ANSWER WHEN IT RESUMES. A person's
// answer to the seat's question, delivered to the turn, reaches its context at
// dispatch, the run's row its launch writes, and the context a resumed segment
// rebuilds from that row: a call a later job of the same turn holds still names
// the decision. A turn woken by anything else carries none.
//
// Mutation: leave Answered off the dispatch, the launch record or the resume,
// and this goes red.
func TestTheAnswerThatWokeATurnIsStillItsAnswerWhenItResumes(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	body := "Ship it"
	wake := events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, SourceEventType: "comment", Sender: "jane",
		Body: body, SalientBody: &body,
		Metadata: map[string]string{worklet.MetaVia: string(worklet.ReasonAnswered),
			worklet.MetaCommentID: "a-2"},
	}, events.TraceContext{})
	tel, err := e.describeTurn(t.Context(), company, Request{
		RunID: "run-1", Handle: "swe", Events: []*events.Event{wake},
	})
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	dispatched := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context
	want := []string{"a-2"}
	if !slices.Equal(dispatched.Answered, want) {
		t.Fatalf("the dispatched turn was woken by answers %v, want %v", dispatched.Answered, want)
	}
	ref := sandboxTurnRef(t.Context(), dispatched, "SWE", nil)
	if !slices.Equal(ref.Answered, want) {
		t.Fatalf("the launch records answers %v, want %v", ref.Answered, want)
	}
	run := sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Answered: ref.Answered}
	resumed, err := e.describeResume(t.Context(), company, resumeInput{
		Run:  run,
		Turn: resumedTurn(run, company.Org.AgentSeatByHandle("swe"), company.Org, turnctx.NewHolds(run.CodeHolds)),
	})
	if err != nil {
		t.Fatalf("describeResume: %v", err)
	}
	if again := resumed.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context; !slices.Equal(again.Answered, want) {
		t.Errorf("the resumed turn was woken by answers %v, want the row's %v", again.Answered, want)
	}

	plain := events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, SourceEventType: "comment", Sender: "jane",
		Body: body, SalientBody: &body,
		Metadata: map[string]string{worklet.MetaVia: string(worklet.ReasonWatcher),
			worklet.MetaCommentID: "c-7"},
	}, events.TraceContext{})
	other, err := e.describeTurn(t.Context(), company, Request{
		RunID: "run-2", Handle: "swe", Events: []*events.Event{plain},
	})
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	if got := other.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context.Answered; len(got) != 0 {
		t.Errorf("a turn a watcher's notice woke carries answers %v", got)
	}
}
