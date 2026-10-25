package assistant_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// pastExpiry moves the rig's clock a second past p's expiry.
func pastExpiry(f *assistanttest.Rig, p assistantdomain.Proposal) {
	f.Clock.Advance(p.ExpiresAt.Sub(f.Clock.Now()) + time.Second)
}

// expiredOnce asserts turn closed expired, exactly once, and freed its
// conversation.
func expiredOnce(t *testing.T, f *assistanttest.Rig, turn uuid.UUID) {
	t.Helper()
	row, err := f.Store.Turn(t.Context(), f.Person.ID, turn)
	if err != nil || row.State != wire.TurnExpired {
		t.Fatalf("the parked turn reads %s, %v; want expired", row.State, err)
	}
	if n := f.Store.Closes(turn); n != 1 {
		t.Errorf("the turn was closed %d times, want once", n)
	}
	c, err := f.Store.Conversation(t.Context(), f.Person.ID, f.Conversation)
	if err != nil || c.OpenTurn != uuid.Nil {
		t.Errorf("the conversation still holds turn %s open (%v)", c.OpenTurn, err)
	}
}

// A PARKED TURN ENDS WITH ITS PROPOSAL'S LIFETIME, wherever it is next met:
// a reload reads it expired, a new question finds it expired rather than
// superseding it, the decision that found the proposal expired ends
// it, and the reaper ends one nobody meets again. Inside the lifetime it
// stays parked.
//
// Mutation: drop the lapse from Start, Transcript, Lapse or ReapLapsed, and
// that path leaves the conversation locked on a card nobody can decide.
func TestAParkedTurnClosesExpiredWhenItsProposalExpires(t *testing.T) {
	t.Parallel()
	paths := map[string]func(t *testing.T, f *assistanttest.Rig, p assistantdomain.Proposal){
		"a reload": func(t *testing.T, f *assistanttest.Rig, _ assistantdomain.Proposal) {
			got, err := f.Svc.Transcript(t.Context(), f.Person, f.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			if got.OpenTurn != nil {
				t.Errorf("the reload still shows open turn %+v", got.OpenTurn)
			}
		},
		"a new question": func(t *testing.T, f *assistanttest.Rig, _ assistantdomain.Proposal) {
			accepted, err := f.Ask("next", "What else is open?", nil)
			if err != nil {
				t.Fatalf("a question after the proposal expired answers %v", err)
			}
			f.Follow(accepted.TurnID, 0)
		},
		"the decision that found it expired": func(t *testing.T, f *assistanttest.Rig, p assistantdomain.Proposal) {
			if err := f.Svc.Lapse(iam.WithPrincipal(context.Background(), f.Person),
				f.Person.ID, f.Conversation, p.Turn); err != nil {
				t.Fatal(err)
			}
		},
		"the reaper": func(t *testing.T, f *assistanttest.Rig, _ assistantdomain.Proposal) {
			n, err := f.Svc.ReapLapsed(t.Context())
			if err != nil || n != 1 {
				t.Fatalf("the reaper closed %d, %v; want 1", n, err)
			}
			if n, _ := f.Svc.ReapLapsed(t.Context()); n != 0 {
				t.Errorf("a second pass closed %d more", n)
			}
		},
	}
	for name, meet := range paths {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, accepted, p := parkedRig(t)
			if n, err := f.Svc.ReapLapsed(t.Context()); err != nil || n != 0 {
				t.Fatalf("inside its lifetime the reaper closed %d, %v", n, err)
			}
			if got, err := f.Svc.Transcript(t.Context(), f.Person, f.Conversation); err != nil ||
				got.OpenTurn == nil || got.OpenTurn.State != wire.TurnWaitingForApproval {
				t.Fatalf("inside its lifetime the reload reads %+v, %v; want the turn waiting",
					got.OpenTurn, err)
			}
			pastExpiry(f, p)
			meet(t, f, p)
			expiredOnce(t, f, accepted.TurnID)
		})
	}
}
