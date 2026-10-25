package assistant_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
)

// pageRig is a rig whose turn proposes action on the Worklet screen, which
// accepts it, then answers.
func pageRig(t *testing.T, action string) (*assistanttest.Rig, []wire.Frame) {
	t.Helper()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "propose_page_change")},
		Rounds: []evals.Round{
			evals.Call("c1", "propose_page_change", map[string]any{
				"action": action, "args": map[string]any{"status": "todo"}}),
			evals.Say("Done.")}})
	page := &wire.PageContext{Screen: "work", Title: "Worklet › Tasks",
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
	f, frames := pageRig(t, catalog.DraftSet)
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

// THE OPEN DRAFT REACHES THE MODEL, FENCED, AND A CREDENTIAL PASTED INTO IT
// NEVER DOES.
//
// An unsaved draft exists nowhere a tool could read it, so the page context
// is how the assistant knows what the person is editing: its step and values
// reach the question's message inside the page context's fence. A key pasted
// into a field that does not say it holds one is caught by its shape on the
// way, by the engine's one secret pass. Mutation: drop redactedPage from
// Service.Start and the key reaches the prompt; drop the draft from the page
// context and the colour does not.
func TestTheOpenDraftReachesTheModelFencedAndRedacted(t *testing.T) {
	t.Parallel()
	key := "sk-ant-api03-" + strings.Repeat("x", 40)
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Noted.")}})
	page := &wire.PageContext{Screen: "brand-setup", Title: "Brand › Set up",
		Params: map[string]string{}, Query: map[string]string{}, Resources: []wire.Resource{},
		SourceRevision: "brand-setup@1", Accepts: []string{catalog.DraftSet},
		Draft: &wire.PageDraft{Kind: wire.DraftClient, ID: "kit-1", Revision: "3", Step: "type",
			Help: "Propose colours, type and voice; logos are uploaded by the person.",
			Fields: []wire.DraftField{
				{Path: "colors.primary", Label: "Primary", Value: "#5B21B6", Editable: true},
				{Path: "voice.summary", Label: "Voice", Value: "Plain and warm. " + key,
					Editable: true},
			}}}
	accepted, err := f.Ask("page", "Check my draft", page)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	prompts := f.Model.Prompts()
	fence := "source=" + string(provenance.SourcePageContext) + " trust=colleague"
	at := strings.Index(prompts, fence)
	switch {
	case at < 0:
		t.Fatalf("the page context was not fenced:\n%s", prompts)
	case !strings.Contains(prompts[at:], `"path":"colors.primary"`) ||
		!strings.Contains(prompts[at:], "#5B21B6") || !strings.Contains(prompts[at:], `"step":"type"`):
		t.Errorf("the open draft did not reach the model inside the fence:\n%s", prompts)
	case strings.Contains(prompts, key):
		t.Errorf("a key pasted into the draft reached the model:\n%s", prompts)
	}
}
