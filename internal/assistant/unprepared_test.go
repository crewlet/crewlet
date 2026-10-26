package assistant_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A CHANGE THAT CANNOT BE PREPARED IS NEVER A CARD.
//
// A generic change whose preview read the route refuses (here a 403), or
// whose PATCH or DELETE finds nothing, never reached the person: no proposal
// is recorded and no proposal card is drawn, so nothing reads as a proposal
// the engine rejected. The step is refused in the person's words (its code and
// the engine's sentence for its kind, never the route's account, which names
// what only the model corrects), the model is handed the route's own words as
// the step's result so it can prepare a change that can apply, and the turn
// goes on. Mutation: draw the failed preview as a card, show the route's words
// on the step, or hand the model "tell the person it failed" in place of the
// route's refusal, and this fails.
func TestAChangeThatCannotBePreparedIsNeverACard(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable([]catalog.Route{
		{Pattern: "GET /worklet/projects/{key}", Action: authz.ActionProjectRead},
		{Pattern: "PUT /worklet/projects/{key}", Action: authz.ActionProjectWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	person := evals.Person("grace", "grace", iam.GrantAdmin)
	refused := func(exec.Request) exec.Response {
		return exec.Response{Status: http.StatusForbidden,
			Body: []byte(`{"error":"forbidden","message":"Only the project's lead edits it.",` +
				`"hint":"Ask the lead of ENG."}`)}
	}
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: catalog.GenericTools(table, person, nil), Handler: refused, Person: &person,
		Rounds: []evals.Round{
			evals.Call("c1", catalog.ProposeAPIChangeTool, map[string]any{
				"route": "PUT /worklet/projects/{key}", "path": map[string]any{"key": "ENG"},
				"body": map[string]any{"key": "ENG", "name": "Engine room"}}),
			evals.Say("Only the lead of ENG can rename it."),
		}})
	accepted, err := f.Ask("unprepared", "Rename ENG to Engine room", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)

	for _, fr := range frames {
		if fr.Kind == wire.EventCard && fr.Card != nil && fr.Card.Card.Type == wire.CardProposal {
			t.Fatalf("a change that was never prepared drew a proposal card: %+v", fr.Card.Card)
		}
	}
	if proposals := f.Store.Proposals(); len(proposals) != 0 {
		t.Errorf("a change that was never prepared was recorded: %+v", proposals)
	}
	var step *wire.StepEvent
	for _, fr := range frames {
		if fr.Kind == wire.EventStep && fr.Step.Tool == catalog.ProposeAPIChangeTool {
			step = fr.Step
		}
	}
	if step == nil || step.State != wire.StepRefused || step.Refusal == nil ||
		step.Refusal.Code != "forbidden" || step.Refusal.Message == "" ||
		strings.Contains(step.Refusal.Message, "lead") || step.Refusal.Hint != "" {
		t.Fatalf("the change's step reads %+v, want refused in the person's words", step)
	}
	// THE ROUND AFTER THE CALL is the one that reads its result; the title
	// call the finished turn makes after it carries no tool message.
	result := ""
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool {
			result = m.Content
		}
	}
	if !strings.Contains(result, "Only the project's lead edits it.") ||
		!strings.Contains(result, "Ask the lead of ENG.") {
		t.Errorf("the model was handed %q, not the route's refusal", result)
	}
	if done := evals.Last(frames, wire.EventDone); done == nil ||
		done.Done.State != wire.TurnCompleted {
		t.Errorf("the turn ended %+v, want it to go on and complete", done)
	}
}
