package assistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A NEW QUESTION WITHDRAWS NOTHING (05-ux-design.md, 3.2 "Waiting for your
// approval"): asked while a turn waits on a proposal, it is answered; the
// parked turn closes superseded and its stream says so; and the proposal
// stays approvable, untouched.
//
// Mutation: refuse the question while a turn is parked (the old 409
// assistant_turn_running) and the person is locked out of their own
// conversation until they decide the card or press Stop.
func TestANewQuestionSupersedesAParkedTurnAndWithdrawsNothing(t *testing.T) {
	t.Parallel()
	f, parked, p := parkedRig(t)
	accepted, err := f.Ask("next", "What else is open?", nil)
	if err != nil {
		t.Fatalf("a question beside a waiting proposal answers %v, want it taken", err)
	}
	done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone)
	if done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the new question ended %+v", done)
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, parked.TurnID)
	if row.State != wire.TurnSuperseded {
		t.Fatalf("the parked turn is %s, want superseded", row.State)
	}
	if got := f.Store.Proposals(); len(got) != 1 || got[0].ID != p.ID ||
		got[0].State != wire.ProposalProposed {
		t.Errorf("the proposal is %+v, want it still proposed", got)
	}
	ended := evals.Last(f.Follow(parked.TurnID, 0), wire.EventDone)
	if ended == nil || ended.Done.State != wire.TurnSuperseded {
		t.Errorf("a stream of the parked turn ends %+v, want superseded", ended)
	}
	if again, err := f.Ask("next", "What else is open?", nil); err != nil ||
		again.TurnID != accepted.TurnID || f.Store.Closes(parked.TurnID) != 1 {
		t.Errorf("a retried question answers %+v, %v and closed the parked turn %d times",
			again, err, f.Store.Closes(parked.TurnID))
	}
}

// A PROPOSAL OF A SUPERSEDED TURN, APPROVED LATER, IS SHOWN WITHOUT ITS LOOP:
// the card updates in place with its receipt and the screens are told where
// the write landed, while the model is never woken in a conversation that has
// moved on.
//
// Mutation: resume the superseded loop as a parked one and the model answers
// into the middle of the later question's thread.
func TestApprovingASupersededTurnsProposalShowsItWithoutTheLoop(t *testing.T) {
	t.Parallel()
	f, parked, p := parkedRig(t)
	accepted, err := f.Ask("next", "What else is open?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	at, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: parked.TurnID, Proposal: p.ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Receipt:    &wire.Receipt{Summary: "Created ENG-7."},
		Invalidate: &wire.Invalidate{Domain: "CREWLET_WORK", Position: "CREWLET_WORK@4"},
		Card:       cardAt(p, wire.ProposalApplied)})
	if err != nil {
		t.Fatalf("approving the superseded turn's proposal answers %v", err)
	}
	frames := f.Follow(parked.TurnID, at.AfterSeq)
	if got := kinds(frames); len(got) != 4 || got[0] != wire.EventStep ||
		got[1] != wire.EventCard || got[2] != wire.EventAction || got[3] != wire.EventDone {
		t.Fatalf("the approval told %v, want the settled step, the card, the invalidation "+
			"and the end", got)
	}
	if frames[0].Step.State != wire.StepDone || frames[1].Card.Card.Proposal.State != wire.ProposalApplied ||
		frames[3].Done.State != wire.TurnSuperseded {
		t.Errorf("the step reads %s, the card %s and the turn ends %s", frames[0].Step.State,
			frames[1].Card.Card.Proposal.State, frames[3].Done.State)
	}
	if err := f.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "approved the change") {
			t.Fatalf("the approval woke the superseded loop: the model read %q", m.Content)
		}
	}
	if !f.Revoker.Revoked(f.Grant("approve").ID()) {
		t.Error("the decision's read grant was left live with no runner to take it")
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, parked.TurnID)
	if row.State != wire.TurnSuperseded {
		t.Errorf("the superseded turn is %s after the approval", row.State)
	}
}
