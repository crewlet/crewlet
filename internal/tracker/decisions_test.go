package tracker_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) decisions(who tracker.Party) tracker.DecisionsAnswer {
	r.t.Helper()
	answer, err := r.reader.Decisions(r.t.Context(), tracker.DecisionsQuery{
		Who: who, Level: statelog.ReadStale,
	}, wednesday, time.UTC)
	if err != nil {
		r.t.Fatalf("Decisions: %v", err)
	}
	return answer
}

// WHAT WAITS ON A PERSON IS EVERY ASK PUT TO ANY OF THEIR NAMES, the oldest
// one's instant beside the count, and an answered ask leaves.
func TestDecisionsCountsEveryNameOfTheParty(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	older := wednesday.Add(-3 * time.Hour)
	// ONE PUT TO THE SEAT, ONE TO THE TOKEN BOUND TO IT, one to somebody
	// else — the same person's two names are one party.
	askOn(t, r, "op-1", "a", tracker.Comment{ID: "c-1", Task: "a", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "ship?", Ask: "jane", CreatedAt: wednesday})
	askOn(t, r, "op-2", "a", tracker.Comment{ID: "c-2", Task: "a", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "hold?", Ask: "founder", CreatedAt: older})
	askOn(t, r, "op-3", "a", tracker.Comment{ID: "c-3", Task: "a", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "yours?", Ask: "ana", CreatedAt: older})

	jane := tracker.Party{Handle: "jane", OperatorID: "founder"}
	got := r.decisions(jane)
	if got.Total.Total != 2 || len(got.Asks) != 2 {
		t.Fatalf("jane has %d asks (total %+v), want both of her names' two",
			len(got.Asks), got.Total)
	}
	if got.OldestAt == nil || !got.OldestAt.Equal(older) {
		t.Errorf("oldest_at = %v, want %v", got.OldestAt, older)
	}

	answers := "c-2"
	askOn(t, r, "op-4", "a", tracker.Comment{ID: "c-4", Task: "a", Author: "jane",
		AuthorKind: tracker.AuthorHuman, Body: "hold", Answers: &answers, CreatedAt: wednesday})
	after := r.decisions(jane)
	if after.Total.Total != 1 || after.OldestAt == nil || !after.OldestAt.Equal(wednesday) {
		t.Errorf("after answering the older ask, decisions = %d waiting since %v, "+
			"want 1 since %v", after.Total.Total, after.OldestAt, wednesday)
	}
}

// THE TOTAL IS EVERY ASK, NOT THE PAGE.
func TestDecisionsTotalExceedsThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	const asks = tracker.MaxDecisions + 3
	for i := range asks {
		askOn(t, r, fmt.Sprintf("op-%02d", i), "a", tracker.Comment{
			ID: fmt.Sprintf("c-%02d", i), Task: "a", Author: "bo",
			AuthorKind: tracker.AuthorAgent, Body: "?", Ask: "jane", CreatedAt: wednesday,
		})
	}
	got := r.decisions(tracker.PartyOf("jane"))
	if len(got.Asks) != tracker.MaxDecisions || got.Total.Total != asks {
		t.Errorf("decisions carries %d of total %d, want the page of %d and all %d",
			len(got.Asks), got.Total.Total, tracker.MaxDecisions, asks)
	}
}

// AN ASK A PERSON'S TOKEN WROTE NAMES THE PERSON. The author stays the token —
// the audit trail — and the seat it was bound to rides beside it, because a
// screen drawing the asker must draw a person rather than a credential.
func TestAnAskWrittenThroughABoundTokenNamesThePerson(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	token := r.writer.As("ops-maya", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "ops-maya", Seat: "maya",
	})
	comment := tracker.Comment{ID: "c-1", Task: "a", Author: "ops-maya",
		AuthorKind: tracker.AuthorOperator, Body: "ship?", Ask: "jane", CreatedAt: wednesday}
	if _, err := token.UpdateTask(t.Context(), "op-ask", "a", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Comment: &comment}, tracker.ChangeComment,
		// AS EVERY ASK IS WRITTEN: it wakes the person asked, and the
		// notification names the comment the history row resolves to.
		&tracker.Notify{Kind: tracker.ChangeComment, CommentID: "c-1"}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	got := r.decisions(tracker.PartyOf("jane"))
	if len(got.Asks) != 1 || got.Asks[0].AskedBy != "ops-maya" || got.Asks[0].AskedBySeat != "maya" {
		t.Errorf("the ask is %+v, want the token as author and maya as the person", got.Asks)
	}
}
