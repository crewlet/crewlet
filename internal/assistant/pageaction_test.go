package assistant_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// pageRig is a rig whose turn proposes action on the Work screen, which
// accepts it, then answers.
func pageRig(t *testing.T, action string) (*assistanttest.Rig, []wire.Frame) {
	t.Helper()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "propose_page_change")},
		Rounds: []evals.Round{
			evals.Call("c1", "propose_page_change", map[string]any{
				"action": action, "args": map[string]any{"status": "todo"}}),
			evals.Say("Done.")}})
	page := &wire.PageContext{Screen: "work", Title: "Work › Tasks",
		Params: map[string]string{}, Query: map[string]string{}, Resources: []wire.Resource{},
		SourceRevision: "work@3", Accepts: []string{action}}
	accepted, err := f.Ask("page", "Show only what is still to do", page)
	if err != nil {
		t.Fatal(err)
	}
	return f, f.Follow(accepted.TurnID, 0)
}

// A VIEW CHANGE APPLIES AT ONCE (D11): a `confirm: none` page action is
// recorded as a proposal the turn approves itself, handed to the page as an
// action frame with no card, and the turn carries on to its answer without
// parking.
//
// Mutation: park every page action (as before) and a filter waits behind
// "Needs your approval" instead of applying with an Undo.
func TestAViewChangeIsAppliedAtOnceWithoutParking(t *testing.T) {
	t.Parallel()
	f, frames := pageRig(t, "work.filter.apply")
	done := evals.Last(frames, wire.EventDone)
	if done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the view change's turn ended %+v, want completed", done)
	}
	action := evals.Last(frames, wire.EventAction)
	if action == nil || action.Action.Confirm != wire.ConfirmNone ||
		action.Action.ProposalID == nil || action.Action.ActionID != *action.Action.ProposalID ||
		action.Action.SourceRevision != "work@3" {
		t.Fatalf("the page was handed %+v, want the action applied at once", action)
	}
	if card := evals.Last(frames, wire.EventCard); card != nil {
		t.Errorf("a view change drew a card %+v", card)
	}
	proposals := f.Store.Proposals()
	if len(proposals) != 1 || proposals[0].State != wire.ProposalApproved ||
		proposals[0].ID != action.Action.ActionID {
		t.Errorf("the view change was recorded as %+v, want one approved proposal", proposals)
	}
}

// A DRAFT CHANGE STILL WAITS FOR THE PERSON: a `confirm: approve` page action
// parks the turn on its proposal card, exactly as before.
func TestADraftChangeStillWaitsForApproval(t *testing.T) {
	t.Parallel()
	f, frames := pageRig(t, "page.draft.replace")
	if done := evals.Last(frames, wire.EventDone); done == nil ||
		done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the draft change's turn ended %+v, want waiting for approval", done)
	}
	if evals.Last(frames, wire.EventAction) != nil {
		t.Error("a draft change was handed to the page before the person approved it")
	}
	if proposals := f.Store.Proposals(); len(proposals) != 1 ||
		proposals[0].State != wire.ProposalProposed {
		t.Errorf("the draft change was recorded as %+v, want proposed", proposals)
	}
}
