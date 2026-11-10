package worklet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE DECISION A SEAT'S TURN ACTS ON IS ITS NEWEST QUESTION ON THE TASK THAT A
// PERSON ANSWERED: an older answered one, a newer one still open and another
// seat's answered one are not it; the choice reads by its option's label, and
// a seat that asked nothing answered there acts on none.
//
// Mutation: take the newest ask whatever its answer, ignore the asker, or name
// the choice by its id, and this goes red.
func TestTheDecisionATurnActsOnIsItsNewestAnswered(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	at := wednesday.Add(-6 * time.Hour)
	ask := func(op, id, author string, hours int) {
		askOn(t, r, op, "a", worklet.Comment{ID: id, Task: "a", Author: author,
			AuthorKind: worklet.AuthorAgent, Body: "which way?", Ask: "jane", Decision: shipOrHold,
			CreatedAt: at.Add(time.Duration(hours) * time.Hour)})
	}
	ask("op-1", "q-1", "bo", 0)
	answerOn(t, r, "op-2", "a", "a-1", "jane", "q-1", "hold", "Hold it", at.Add(time.Hour))
	ask("op-3", "q-2", "bo", 2)
	answerOn(t, r, "op-4", "a", "a-2", "jane", "q-2", "ship", "Ship it", at.Add(3*time.Hour))
	ask("op-5", "q-3", "bo", 4)
	ask("op-6", "q-4", "cy", 4)
	answerOn(t, r, "op-7", "a", "a-4", "jane", "q-4", "hold", "Hold it", at.Add(5*time.Hour))

	got, err := r.reader.DecisionActedOn(t.Context(), "a", "bo", statelog.ReadStale)
	if err != nil || got == nil {
		t.Fatalf("DecisionActedOn = %+v (%v), want bo's newest answered question", got, err)
	}
	if got.Ask != "q-2" || got.Question != "Ship or hold?" || got.Choice != "Ship it" ||
		got.AnsweredBy != "jane" || !got.AnsweredAt.Equal(at.Add(3*time.Hour)) {
		t.Errorf("the decision bo acts on is %+v, want q-2 answered Ship it by jane", got)
	}
	none, err := r.reader.DecisionActedOn(t.Context(), "a", "dee", statelog.ReadStale)
	if err != nil || none != nil {
		t.Errorf("a seat that asked nothing acts on %+v (%v), want none", none, err)
	}
}
