package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ANSWER GIVES A SEAT A DECISION ONLY WHERE A PERSON ANSWERED THE SEAT'S OWN
// QUESTION: an agent's answer, an answer to another seat's question and an id
// nothing answered with give none; of several answers that woke one turn the
// newest is the decision; the choice reads by its option's label.
//
// Mutation: count an agent's answer, ignore the asker, or name the choice by
// its id, and this goes red.
func TestAnAnswerGivesTheSeatADecisionOnlyWhereAPersonAnswered(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	at := wednesday.Add(-6 * time.Hour)
	ask := func(op, id, author, to string, hours int) {
		askOn(t, r, op, "a", worklet.Comment{ID: id, Task: "a", Author: author,
			AuthorKind: worklet.AuthorAgent, Body: "which way?", Ask: to, Decision: shipOrHold,
			CreatedAt: at.Add(time.Duration(hours) * time.Hour)})
	}
	ask("op-1", "q-1", "bo", "jane", 0)
	answerOn(t, r, "op-2", "a", "a-1", "jane", "q-1", "hold", "Hold it", at.Add(time.Hour))
	ask("op-3", "q-2", "bo", "jane", 2)
	answerOn(t, r, "op-4", "a", "a-2", "jane", "q-2", "ship", "Ship it", at.Add(3*time.Hour))
	ask("op-5", "q-3", "bo", "cy", 4)
	q3 := "q-3"
	askOn(t, r, "op-6", "a", worklet.Comment{ID: "a-3", Task: "a", Author: "cy",
		AuthorKind: worklet.AuthorAgent, Body: "Ship it", Answers: &q3, Choice: "ship",
		CreatedAt: at.Add(5 * time.Hour)})
	ask("op-7", "q-4", "cy", "jane", 6)
	answerOn(t, r, "op-8", "a", "a-4", "jane", "q-4", "ship", "Ship it", at.Add(7*time.Hour))

	read := func(answers ...string) *worklet.ActedDecision {
		t.Helper()
		got, err := r.reader.DecisionAnswered(t.Context(), answers, "bo", statelog.ReadStale)
		if err != nil {
			t.Fatalf("DecisionAnswered(%v): %v", answers, err)
		}
		return got
	}
	got := read("a-1", "a-2")
	if got == nil || got.Ask != "q-2" || got.Question != "Ship or hold?" || got.Choice != "Ship it" ||
		got.AnsweredBy != "jane" || got.Task != "a" || got.Key != "ENG-1" ||
		!got.AnsweredAt.Equal(at.Add(3*time.Hour)) {
		t.Errorf("the decision of a-1 and a-2 is %+v, want q-2 answered Ship it by jane", got)
	}
	if got := read("a-1"); got == nil || got.Ask != "q-1" || got.Choice != "Hold it" {
		t.Errorf("the decision a-1 gave is %+v, want q-1 answered Hold it", got)
	}
	for name, answers := range map[string][]string{
		"an agent's answer":                    {"a-3"},
		"an answer to another seat's question": {"a-4"},
		"an id nothing answered with":          {"q-1", "nothing"},
		"no answer at all":                     nil,
	} {
		if got := read(answers...); got != nil {
			t.Errorf("%s gives bo the decision %+v", name, got)
		}
	}
}

// THE ANSWERS THAT WOKE A TURN ARE READ OFF ITS DELIVERIES: a notice routed to
// the seat as an answer names its comment, a coalesced delivery's constituents
// included, each once; a notice routed for any other reason names none.
//
// Mutation: read a mention or a watcher's notice as an answer, or skip a
// coalesced constituent, and this goes red.
func TestTheAnswersThatWokeATurnAreReadOffItsDeliveries(t *testing.T) {
	t.Parallel()
	meta := func(via worklet.Reason, comment string) map[string]string {
		return map[string]string{worklet.MetaVia: string(via), worklet.MetaCommentID: comment}
	}
	ev := func(n types.ExternalNotification) *events.Event { return events.New(n, events.TraceContext{}) }
	got := worklet.AnswersIn([]*events.Event{
		ev(types.ExternalNotification{Metadata: meta(worklet.ReasonAnswered, "a-1")}),
		ev(types.ExternalNotification{Metadata: meta(worklet.ReasonMention, "c-9"),
			Messages: []types.CoalescedMessage{
				{Metadata: meta(worklet.ReasonAnswered, "a-2")},
				{Metadata: meta(worklet.ReasonAnswered, "a-1")},
				{Metadata: meta(worklet.ReasonWatcher, "c-8")},
			}}),
		nil,
	})
	if want := []string{"a-1", "a-2"}; !slices.Equal(got, want) {
		t.Errorf("AnswersIn = %v, want %v", got, want)
	}
}
