package assistant_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// cardAt is a proposal's own card in state.
func cardAt(p assistantdomain.Proposal, state wire.ProposalState) wire.Card {
	card := p.Card
	proposal := *card.Proposal
	proposal.State = state
	card.Proposal = &proposal
	return card
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

// parkedRig is a rig whose turn proposed a task through the catalogue's own
// create_task, and parked on the proposal, which it answers.
func parkedRig(t *testing.T) (*assistanttest.Rig, wire.TurnAccepted, assistantdomain.Proposal) {
	t.Helper()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "create_task")},
		Acts:  []string{"create_work_item"},
		Rounds: []evals.Round{
			evals.Call("c1", "create_task", map[string]any{"title": "Rotate keys"}),
			evals.Say("Created it.")}})
	accepted, err := f.Ask("propose", "File a task to rotate keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone)
	if done == nil || done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the proposing turn ended %+v, want waiting_for_approval", done)
	}
	proposals := f.Store.Proposals()
	if len(proposals) != 1 {
		t.Fatalf("the turn recorded %d proposals, want 1", len(proposals))
	}
	return f, accepted, proposals[0]
}

// AN APPROVAL RESUMES THE PARKED TURN where it stopped: the call it suspended
// on is answered with what became of the proposal, the card updates in place,
// and the turn finishes on the grant the approving request derived. The
// route's answer to the write is the engine's own records, which the model
// reads fenced as an API answer at a colleague's trust, by the one rule for a
// surface the engine hosts (ADR-0040).
//
// Mutation: answer the pending call with nothing (drop resultOf) and the
// model never reads that the change was applied; fence the route's answer at
// another trust, or not at all, and its label fails.
func TestAnApprovalResumesTheParkedTurn(t *testing.T) {
	t.Parallel()
	f, accepted, p := parkedRig(t)
	// A PROPOSING TURN WRITES NOTHING: its preview reads what the change
	// would touch, and nothing else is sent until the approval.
	for _, sent := range f.Dispatcher.Sent() {
		if sent.Request.Method != http.MethodGet {
			t.Fatalf("a proposing turn sent %+v", sent)
		}
	}
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Receipt: &wire.Receipt{Summary: "Created ENG-7."},
		Answer:  []byte(`{"key":"ENG-7","title":"Rotate keys"}`),
		Card:    cardAt(p, wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, resumed.AfterSeq)
	done := evals.Last(frames, wire.EventDone)
	if done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the resumed turn ended %+v", done)
	}
	card := evals.Last(frames, wire.EventCard)
	transcript, _ := f.Store.Transcript(t.Context(), f.Person.ID, f.Conversation)
	if card == nil || len(transcript.Cards) != 1 || card.Card.CardID != transcript.Cards[0].ID ||
		card.Card.Card.Proposal.State != wire.ProposalApplied {
		t.Errorf("the proposal card was not updated in place: %+v", card)
	}
	var tool llm.Message
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool {
			tool = m
		}
	}
	if !strings.Contains(tool.Content, "approved the change") ||
		!strings.Contains(tool.Content, "Created ENG-7.") || tool.Failed {
		t.Errorf("the model read the approval as %+v", tool)
	}
	if at := strings.Index(tool.Content, " source=api_answer trust=colleague "); at < 0 ||
		!strings.Contains(tool.Content[at:], `"key":"ENG-7"`) {
		t.Errorf("the route's answer is not fenced as the engine's own records:\n%s", tool.Content)
	}
	if at := strings.Index(tool.Content, " source=api_answer trust=colleague "); at < 0 ||
		!strings.Contains(tool.Content[at:], `"key":"ENG-7"`) {
		t.Errorf("the route's answer is not fenced as the engine's own records:\n%s", tool.Content)
	}
}

// A REFUSAL'S TOOL SENTENCE REACHES THE MODEL AND NOTHING THE BROWSER READS
// (ADR-0060). The approval path hands the resumed model the route's answer
// with the tool's own sentence beside it (`tool_detail`), because it is the
// one reader that can act on it; the stream the drawer follows, its snapshot
// and the stored transcript the drawer reloads carry the person's refusal and
// never that sentence. Mutation: emit the decision's text as a step or a
// message and the stream half goes red.
func TestARefusalsToolSentenceNeverReachesTheBrowsersStream(t *testing.T) {
	t.Parallel()
	f, accepted, p := parkedRig(t)
	said := "create_work_item needs a `title`: one line saying what the work is."
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
		Read: f.Grant("approve"), State: wire.ProposalFailed,
		Failure: &wire.Failure{Refusal: wire.Refusal{Code: "invalid",
			Message: "A work item needs a title. Add one, then try again."}},
		Answer: []byte(`{"error":"invalid","message":"That change was refused, and ` +
			`nothing was written.","tool_detail":` + strconv.Quote(said) + `}`),
		Card: cardAt(p, wire.ProposalFailed)})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, resumed.AfterSeq)
	if done := evals.Last(frames, wire.EventDone); done == nil {
		t.Fatal("the resumed turn never ended")
	}
	var tool llm.Message
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool {
			tool = m
		}
	}
	if !strings.Contains(tool.Content, "needs a `title`") {
		t.Errorf("the model read the refusal as %q, without the tool's sentence", tool.Content)
	}
	streamed, _ := json.Marshal(f.Follow(accepted.TurnID, 0))
	stored, _ := f.Store.Transcript(t.Context(), f.Person.ID, f.Conversation)
	kept, _ := json.Marshal(stored)
	for what, body := range map[string][]byte{"the stream": streamed, "the transcript": kept} {
		if strings.Contains(string(body), "needs a `title`") ||
			strings.Contains(string(body), "tool_detail") {
			t.Errorf("%s the browser reads carries the tool's sentence", what)
		}
	}
}

// ANOTHER PERSON'S DECISION RESUMES NOTHING: a turn resumes only on its
// owner's own request.
func TestAnotherPersonCannotResumeATurn(t *testing.T) {
	t.Parallel()
	f, accepted, p := parkedRig(t)
	ada := evals.Person("ada", "ada", iam.GrantWorklet)
	_, err := f.Svc.Resume(iam.WithPrincipal(context.Background(), ada), approval.Resumption{
		Owner: f.Person.ID, Conversation: f.Conversation, Turn: accepted.TurnID,
		Proposal: p.ID, Read: f.Grant("ada"), State: wire.ProposalApplied,
		Card: cardAt(p, wire.ProposalApplied)})
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
	f, accepted, _ := parkedRig(t)
	d := &decliner{}
	if err := f.Svc.BindDecliner(d); err != nil {
		t.Fatal(err)
	}
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	answer, err := f.Svc.Stop(ctx, f.Person.ID, accepted.TurnID, evals.Key("stop"))
	if err != nil || answer.State != wire.TurnStopped {
		t.Fatalf("stop = %+v, %v", answer, err)
	}
	if len(d.stops) != 1 || d.stops[0].Turn != accepted.TurnID ||
		d.stops[0].Key != evals.Key("stop") {
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
	f, accepted, p := parkedRig(t)
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	card := cardAt(p, wire.ProposalApproved)
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

// A LOOP PARKED AT ANOTHER VERSION IS REFUSED BY NAME: resuming it says which
// version it was parked at and which this build reads, and nothing runs.
//
// Mutation: fold the version check back into the decode error (wrapping a nil
// error) and the refusal reads "%!w(<nil>)" with no version this build reads.
func TestALoopParkedAtAnotherVersionIsRefusedByName(t *testing.T) {
	t.Parallel()
	f, accepted, p := parkedRig(t)
	f.Store.Repark(accepted.TurnID, []byte(`{"v":99}`))
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	_, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Card: cardAt(p, wire.ProposalApplied)})
	if err == nil || strings.Contains(err.Error(), "%!") || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("resuming a loop parked at version 99 answered %v", err)
	}
	if rounds := len(f.Model.Requests()); rounds != 1 {
		t.Errorf("the refused resume asked the model %d rounds in all, want the proposing 1", rounds)
	}
}

// A RESUME THAT CANNOT START LEAVES THE TURN PARKED: everything that can fail
// is read before the resume record, so a failure answers the approval and
// leaves the turn waiting on it, never running with no runner.
//
// Mutation: read the conversation after the resume record (where it was) and
// the turn is left running.
func TestAResumeThatCannotStartLeavesTheTurnParked(t *testing.T) {
	t.Parallel()
	f, accepted, p := parkedRig(t)
	f.Store.FailConversations(1)
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	if _, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Card: cardAt(p, wire.ProposalApplied)}); err == nil {
		t.Fatal("a resume whose conversation could not be read started")
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if row.State != wire.TurnWaitingForApproval {
		t.Fatalf("the turn is %s after a resume that could not start, want still parked",
			row.State)
	}
}
