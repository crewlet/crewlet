package assistant_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// createTask is a write tool, which a turn never sends.
var createTask = catalog.Tool{Name: "create_task", Kind: catalog.KindWrite,
	Tier: catalog.TierReversible, Act: "create_work_item",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{
		"title": map[string]any{"type": "string"}}}}

// proposalID is the one proposal the fake proposer makes.
var proposalID = assistanttest.ID("proposal/one")

// proposalCard is the proposal's card in state.
func proposalCard(state wire.ProposalState) wire.Card {
	return wire.Card{Type: wire.CardProposal, Proposal: &wire.ProposalCard{
		ProposalID: proposalID, Scope: wire.ScopeServer, Tool: "create_task",
		Title: "Create a task", Digest: "sha256:" + strings.Repeat("ab", 32),
		ExpiresAt: assistanttest.Epoch.Add(15 * time.Minute), State: state,
		ApproveLabel: "Create", Changes: []wire.Change{{Kind: wire.ChangeAdded,
			Field: "title", Label: "Title", To: "Rotate keys", New: true}}}}
}

// proposer turns every write into the one proposal.
type proposer struct{}

func (proposer) Propose(context.Context, assistant.ProposalCall) (wire.Card, error) {
	return proposalCard(wire.ProposalProposed), nil
}

// decliner records every Stop it was handed.
type decliner struct {
	mu    sync.Mutex
	stops []approval.Stop
}

func (d *decliner) DeclineTurn(_ context.Context, s approval.Stop) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops = append(d.stops, s)
	return nil
}

// parkedRig is a rig whose turn proposed a task and parked on it.
func parkedRig(t *testing.T) (*assistanttest.Rig, wire.TurnAccepted) {
	t.Helper()
	f := assistanttest.NewRig(t, assistanttest.RigOptions{Tools: []catalog.Tool{createTask},
		Proposer: proposer{}, Acts: []string{"create_work_item"},
		Rounds: []assistanttest.Round{
			assistanttest.Call("c1", "create_task", map[string]any{"title": "Rotate keys"}),
			assistanttest.Say("Created it.")}})
	accepted, err := f.Ask("propose", "File a task to rotate keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := assistanttest.Last(f.Follow(accepted.TurnID, 0), wire.EventDone)
	if done == nil || done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the proposing turn ended %+v, want waiting_for_approval", done)
	}
	return f, accepted
}

// AN APPROVAL RESUMES THE PARKED TURN where it stopped: the call it suspended
// on is answered with what became of the proposal, the card updates in place,
// and the turn finishes on the grant the approving request derived.
//
// Mutation: answer the pending call with nothing (drop resultOf) and the
// model never reads that the change was applied.
func TestAnApprovalResumesTheParkedTurn(t *testing.T) {
	t.Parallel()
	f, accepted := parkedRig(t)
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Fatalf("a proposing turn sent %+v", sent)
	}
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: proposalID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Receipt: &wire.Receipt{Summary: "Created ENG-7."},
		Card:    proposalCard(wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, resumed.AfterSeq)
	done := assistanttest.Last(frames, wire.EventDone)
	if done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the resumed turn ended %+v", done)
	}
	card := assistanttest.Last(frames, wire.EventCard)
	transcript, _ := f.Store.Transcript(t.Context(), f.Person.ID, f.Conversation)
	if card == nil || len(transcript.Cards) != 1 || card.Card.CardID != transcript.Cards[0].ID ||
		card.Card.Card.Proposal.State != wire.ProposalApplied {
		t.Errorf("the proposal card was not updated in place: %+v", card)
	}
	var tool llm.Message
	for _, req := range f.Model.Requests() {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				tool = m
			}
		}
	}
	if !strings.Contains(tool.Content, "approved the change") ||
		!strings.Contains(tool.Content, "Created ENG-7.") || tool.Failed {
		t.Errorf("the model read the approval as %+v", tool)
	}
}

// ANOTHER PERSON'S DECISION RESUMES NOTHING: a turn resumes only on its
// owner's own request.
func TestAnotherPersonCannotResumeATurn(t *testing.T) {
	t.Parallel()
	f, accepted := parkedRig(t)
	ada := assistanttest.Person("ada", "ada", iam.GrantTracker)
	_, err := f.Svc.Resume(iam.WithPrincipal(context.Background(), ada), approval.Resumption{
		Owner: f.Person.ID, Conversation: f.Conversation, Turn: accepted.TurnID,
		Proposal: proposalID, Read: f.Grant("ada"), State: wire.ProposalApplied,
		Card: proposalCard(wire.ProposalApplied)})
	if !errors.Is(err, assistant.ErrNotFound) {
		t.Fatalf("ada resuming grace's turn: err = %v, want not found", err)
	}
	if !f.Revoker.Revoked(f.Grant("ada").ID()) {
		t.Error("a refused resume left its grant live")
	}
}

// A STOP OF A PARKED TURN CLOSES IT AND DECLINES WHAT IT WAITS ON, under the
// Stop's own operation.
func TestStoppingAParkedTurnDeclinesItsProposals(t *testing.T) {
	t.Parallel()
	f, accepted := parkedRig(t)
	d := &decliner{}
	if err := f.Svc.BindDecliner(d); err != nil {
		t.Fatal(err)
	}
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	answer, err := f.Svc.Stop(ctx, f.Person.ID, accepted.TurnID, assistanttest.Key("stop"))
	if err != nil || answer.State != wire.TurnStopped {
		t.Fatalf("stop = %+v, %v", answer, err)
	}
	if len(d.stops) != 1 || d.stops[0].Turn != accepted.TurnID ||
		d.stops[0].Key != assistanttest.Key("stop") {
		t.Errorf("declined %+v, want the stopped turn under the Stop's key", d.stops)
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if row.State != wire.TurnStopped {
		t.Errorf("the turn is %s", row.State)
	}
}

// A PARKED TURN'S PAGE ACTION IS EMITTED ON ITS STREAM, and the segment closes
// still waiting, so the client's stream ends cleanly.
func TestAnEmissionClosesStillWaiting(t *testing.T) {
	t.Parallel()
	f, accepted := parkedRig(t)
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	card := proposalCard(wire.ProposalApproved)
	at, err := f.Svc.Emit(ctx, f.Person.ID, f.Conversation, accepted.TurnID,
		approval.Emission{Card: &card})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, at.AfterSeq)
	if len(frames) != 3 || frames[0].Kind != wire.EventCard ||
		frames[2].Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("emitted %v", kinds(frames))
	}
}
