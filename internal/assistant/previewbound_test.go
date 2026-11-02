package assistant_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// busyAgent is GET /agents/pm for a seat that has worked a while: its live
// overlay and the fifty finished phases the route answers, each with the
// detail a phase carries, which together run past what one step reads.
func busyAgent() exec.Response {
	history := make([]any, 0, 50)
	for i := range 50 {
		history = append(history, map[string]any{"phase": "execute", "model": "claude-sonnet-5",
			"turn_id": fmt.Sprintf("turn-%02d", i), "summary": strings.Repeat("Reviewed the "+
				"settle-up tests and commented on the work item. ", 30)})
	}
	return evals.JSON(map[string]any{"handle": "pm", "role": "Product manager",
		"live": map[string]any{"activity": "working"}, "llm_history": history})
}

// A WRITE'S PREVIEW READS THE RECORD IT DIFFS WHOLE, HOWEVER LONG.
//
// The QA run (X1): every attempt to pause the QA Engineer was refused "the
// answer is larger than one step reads", because the pause's preview read the
// agent through a step's bound, and an agent with fifty phases of history is
// past it. A preview decodes what it reads and draws the fields that move,
// never handing the answer to the model, so its read takes the preview's own
// bound (exec.PreviewMaxResponseBytes), and the pause is a card. Mutation:
// drop the bound from the surface's preview read and the pause is refused
// before any card.
func TestAPreviewReadsALongRecordWhole(t *testing.T) {
	t.Parallel()
	agent := busyAgent()
	if len(agent.Body) <= exec.MaxResponseBytes {
		t.Fatalf("the agent is %d bytes, not past a step's %d", len(agent.Body),
			exec.MaxResponseBytes)
	}
	for _, tool := range []string{"pause_agent", "resume_agent"} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{
				Tools: []catalog.Tool{assistanttest.Tool(t, tool)},
				Handler: func(r exec.Request) exec.Response {
					if r.Path == "/agents/pm" {
						return agent
					}
					return assistanttest.Answer(assistanttest.World(), r)
				},
				Rounds: []evals.Round{
					evals.Call("c1", tool, assistanttest.ProposalArgs[tool]),
					evals.Say("Done.")}})
			accepted, err := f.Ask("page", "Pause the product manager", nil)
			if err != nil {
				t.Fatal(err)
			}
			frames := f.Follow(accepted.TurnID, 0)
			if proposals := f.Store.Proposals(); len(proposals) != 1 {
				res, _ := f.Model.Answer("c1")
				t.Fatalf("recorded %d proposals; the call was answered %q", len(proposals),
					res.Output)
			}
			if card := evals.Last(frames, wire.EventCard); card == nil ||
				card.Card.Card.Type != wire.CardProposal {
				t.Errorf("no proposal card was drawn: %+v", card)
			}
		})
	}
}
