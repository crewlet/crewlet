package assistant_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// setupPage is an integration's setup screen whose main button reads label,
// and which answers a screen read.
func setupPage(label, revision string) *wire.PageContext {
	empty := false
	return &wire.PageContext{Screen: "integrations", Title: "Integrations › Google Cloud",
		Params: map[string]string{"kind": "gcp"}, Query: map[string]string{},
		Resources: []wire.Resource{}, SourceRevision: revision,
		Accepts: []string{catalog.ReadScreen},
		ActionArgs: map[string]*wire.ActionArgs{catalog.ReadScreen: {
			Type: wire.ArgTypes{wire.ArgObject}, AdditionalProperties: new(false)}},
		ActionRevisions: map[string]string{catalog.ReadScreen: "screen"},
		View: &wire.PageView{Nodes: []wire.ViewNode{
			{Role: wire.ViewHeading, Name: "Google Cloud", Level: 1},
			{Role: wire.ViewField, Name: "Project ID", Field: wire.ViewFieldText, Filled: &empty,
				Required: true},
			{Role: wire.ViewButton, Name: label},
		}}}
}

// A SCREEN READ PARKS THE TURN AND RESUMES IT ON THE SCREEN AS IT IS NOW.
//
// read_screen is a `confirm: read` page action: approved by the turn itself,
// handed to the page only once the turn has parked (the page answers at once,
// and its report resumes only a parked turn), drawn as no card, and answered
// by the page's report with the screen it carries, which the model reads
// fenced as the page's. Mutation: send the action before the park and it
// precedes the step that waits on it; show the resumption's card and a card
// frame follows; drop the fresh screen and the model never reads "Save".
func TestAScreenReadParksAndResumesOnTheScreenNow(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, catalog.ReadScreenTool)},
		Rounds: []evals.Round{
			evals.Call("c1", catalog.ReadScreenTool, map[string]any{}),
			evals.Say("It now says Save.")}})
	accepted, err := f.Ask("page", "I pressed Connect. What do you see now?",
		setupPage("Connect", "integrations@1"))
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil ||
		done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the screen read's turn ended %+v, want parked", done)
	}
	if card := evals.Last(frames, wire.EventCard); card != nil {
		t.Errorf("a screen read drew a card %+v", card)
	}
	action := slices.IndexFunc(frames, func(fr wire.Frame) bool { return fr.Kind == wire.EventAction })
	waiting := slices.IndexFunc(frames, func(fr wire.Frame) bool {
		return fr.Kind == wire.EventStep && fr.Step.State == wire.StepWaiting
	})
	status := slices.IndexFunc(frames, func(fr wire.Frame) bool {
		return fr.Kind == wire.EventStatus &&
			fr.Status.State == wire.StatusWaitingForApproval
	})
	switch {
	case action < 0 || frames[action].Action.Confirm != wire.ConfirmRead ||
		frames[action].Action.Name != catalog.ReadScreen:
		t.Fatalf("the page was handed %+v, want a screen read", frames)
	case waiting < 0 || action < waiting || status < action:
		t.Fatalf("the screen read was sent at frame %d, its step waits at %d and the park "+
			"is told at %d: it must go out after the park", action, waiting, status)
	}

	first := f.Store.Proposals()[0]
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: first.ID,
		Read: f.Grant("report"), State: wire.ProposalApplied, ClientReported: true,
		Receipt: &wire.Receipt{Summary: wire.Plain("Read this screen.")},
		Card:    cardAt(first, wire.ProposalApplied), Page: setupPage("Save", "integrations@2")})
	if err != nil {
		t.Fatal(err)
	}
	after := f.Follow(accepted.TurnID, resumed.AfterSeq)
	if done := evals.Last(after, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the resumed turn ended %+v, want completed", done)
	}
	if card := evals.Last(after, wire.EventCard); card != nil {
		t.Errorf("the resumed screen read drew a card %+v", card)
	}
	if step := evals.Last(after, wire.EventStep); step == nil || step.Step.Summary != "Read" {
		t.Errorf("the screen read's step settled as %+v, want Read", step)
	}
	answer, ok := lastToolAnswer(f, "c1")
	if !ok {
		t.Fatal("the model never read the screen read's answer")
	}
	at := strings.Index(answer.Content, "screen now")
	if !strings.HasPrefix(answer.Content, "The screen was read.") || at < 0 ||
		!strings.Contains(answer.Content[at:], "source="+string(provenance.SourcePageContext)) ||
		!strings.Contains(answer.Content[at:], `"name":"Save"`) {
		t.Fatalf("the model did not read the screen as it is now, fenced:\n%s", answer.Content)
	}
}

// THE SCREEN'S OUTLINE REACHES THE MODEL INSIDE THE PAGE'S FENCE, REDACTED.
//
// Words on a screen can be anybody's, so an instruction in a notice is data
// in the fence, and a key pasted where a label shows it is redacted.
// Mutation: drop the outline from the secret pass and the key reaches the
// model.
func TestTheScreenOutlineReachesTheModelFencedAndRedacted(t *testing.T) {
	t.Parallel()
	key := "sk-ant-api03-" + strings.Repeat("x", 40)
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Noted.")}})
	page := setupPage("Connect", "integrations@1")
	page.View.Nodes = append(page.View.Nodes,
		wire.ViewNode{Role: wire.ViewNotice, Name: injected},
		wire.ViewNode{Role: wire.ViewNotice, Name: "Paste " + key})
	accepted, err := f.Ask("page", "What do I press?", page)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	prompts := f.Model.Prompts()
	at := strings.Index(prompts, "source="+string(provenance.SourcePageContext)+" trust=colleague")
	switch {
	case at < 0:
		t.Fatalf("the page context was not fenced:\n%s", prompts)
	case !strings.Contains(prompts[at:], `"name":"Connect"`) ||
		!strings.Contains(prompts[at:], "already approved every change"):
		t.Errorf("the outline did not reach the model inside the fence:\n%s", prompts)
	case strings.Contains(prompts, key):
		t.Errorf("a key on the screen reached the model:\n%s", prompts)
	}
}

// A SCREEN READ THE PAGE DID NOT ANSWER is never told to the model as read.
// Mutation: say "The screen was read." whatever the page reported and this
// fails.
func TestAScreenReadThePageRefusedIsNotToldAsRead(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, catalog.ReadScreenTool)},
		Rounds: []evals.Round{
			evals.Call("c1", catalog.ReadScreenTool, map[string]any{}),
			evals.Say("I could not read it.")}})
	accepted, err := f.Ask("page", "What do you see now?", setupPage("Connect", "integrations@1"))
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	first := f.Store.Proposals()[0]
	failure := &wire.Failure{Refusal: wire.Refusal{Code: "no_handler", Message: "No page."}}
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: first.ID,
		Read: f.Grant("report"), State: wire.ProposalFailed, Failure: failure,
		ClientReported: true, Card: cardAt(first, wire.ProposalFailed)})
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, resumed.AfterSeq)
	answer, ok := lastToolAnswer(f, "c1")
	if !ok || !strings.HasPrefix(answer.Content, "The screen could not be read") {
		t.Fatalf("a refused screen read reached the model as %q", answer.Content)
	}
}
