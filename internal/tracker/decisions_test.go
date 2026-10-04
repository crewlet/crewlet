package tracker_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) decisions(handle string) tracker.DecisionsAnswer {
	r.t.Helper()
	answer, err := r.reader.Decisions(r.t.Context(), tracker.DecisionsQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday, time.UTC)
	if err != nil {
		r.t.Fatalf("Decisions: %v", err)
	}
	return answer
}

// WHAT WAITS ON A PERSON IS EVERY ASK PUT TO THEM, the oldest one's instant
// beside the count, and an answered ask leaves.
func TestDecisionsCountsEveryAskPutToThePerson(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	older := wednesday.Add(-3 * time.Hour)
	// TWO PUT TO JANE, one to somebody else.
	askOn(t, r, "op-1", "a", tracker.Comment{ID: "c-1", Task: "a", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "ship?", Ask: "jane", CreatedAt: wednesday})
	askOn(t, r, "op-2", "a", tracker.Comment{ID: "c-2", Task: "a", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "hold?", Ask: "jane", CreatedAt: older})
	askOn(t, r, "op-3", "a", tracker.Comment{ID: "c-3", Task: "a", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "yours?", Ask: "ana", CreatedAt: older})

	got := r.decisions("jane")
	if got.Total.Total != 2 || len(got.Asks) != 2 {
		t.Fatalf("jane has %d asks (total %+v), want her two", len(got.Asks),
			got.Total)
	}
	if got.OldestAt == nil || !got.OldestAt.Equal(older) {
		t.Errorf("oldest_at = %v, want %v", got.OldestAt, older)
	}

	answers := "c-2"
	askOn(t, r, "op-4", "a", tracker.Comment{ID: "c-4", Task: "a", Author: "jane",
		AuthorKind: tracker.AuthorHuman, Body: "hold", Answers: &answers, CreatedAt: wednesday})
	after := r.decisions("jane")
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
	got := r.decisions("jane")
	if len(got.Asks) != tracker.MaxDecisions || got.Total.Total != asks {
		t.Errorf("decisions carries %d of total %d, want the page of %d and all %d",
			len(got.Asks), got.Total.Total, tracker.MaxDecisions, asks)
	}
}

// AN ASK A PERSON PUT THROUGH THEIR OWN TOKEN NAMES THE PERSON. Bound to a seat
// they write AS it (iam.ActorFor), so the asker is the seat and the token
// rides beside it as the operator id — a screen drawing the asker draws a
// person rather than a credential, with no second field to reconcile.
func TestAnAskWrittenThroughAPersonsTokenNamesThePerson(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	maya := r.writer.As("maya", tracker.AuthorHuman, tracker.Provenance{
		OperatorID: "pat:maya-ci",
	})
	comment := tracker.Comment{ID: "c-1", Task: "a", Author: "maya",
		AuthorKind: tracker.AuthorHuman, Body: "ship?", Ask: "jane", CreatedAt: wednesday}
	if _, err := maya.UpdateTask(t.Context(), "op-ask", "a", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Comment: &comment}, tracker.ChangeComment,
		// AS EVERY ASK IS WRITTEN: it wakes the person asked, and the
		// notification names the comment the history row resolves to.
		&tracker.Notify{Kind: tracker.ChangeComment, CommentID: "c-1"}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	got := r.decisions("jane")
	if len(got.Asks) != 1 || got.Asks[0].AskedBy != "maya" {
		t.Errorf("the ask is %+v, want maya as its asker", got.Asks)
	}
}
