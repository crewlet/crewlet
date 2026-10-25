package assistant_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// lostCallClaim is what the incident's turn ended on: a proposal described as
// prepared, with none made.
const lostCallClaim = "I've prepared a proposal to file the task. Nothing changes until you approve it."

// A TURN NEVER COMPLETES ON A CALL IT DID NOT MAKE.
//
// The incident: a turn asked to add a seat ended `completed` with its answer
// saying a proposal had been prepared, no proposal card and no step saying
// why. The model had made the call, outside the envelope its reply was read
// by. Read by the text-mode CLI backend's own reading
// ([cliagent.ReadReply]), each shape the call was lost in is now a reply that
// runs nothing and is never shown: the call it made is a refused step the
// person sees, the model is asked again, and its next reply's call is the
// proposal the turn waits on.
//
// Mutation: drop the stray-call check from cliagent.ParseEnvelope and each
// case completes on the claim with no proposal; drop Refused from the
// surface and the refused step is missing.
func TestATurnNeverCompletesOnACallItDidNotMake(t *testing.T) {
	t.Parallel()
	call := `{"name": "create_task", "arguments": {"title": "Rotate keys"}}`
	for name, reply := range map[string]string{
		"the call in an envelope before the claim's": "```json\n" + `{"message": "", "tool_calls": [` +
			call + `]}` + "\n```\n\n```json\n" + `{"message": "` + lostCallClaim + `", "tool_calls": []}` + "\n```",
		"the call as markup beside the claim's envelope": "\n" + `<invoke name="create_task">` +
			"\n" + `<parameter name="title">Rotate keys</parameter>` + "\n</invoke>\n```json\n" +
			`{"message": "` + lostCallClaim + `", "tool_calls": []}` + "\n```",
		"the call fenced after the claim": lostCallClaim + "\n\n```xml\n" + `<invoke name="create_task">` +
			`<parameter name="title">Rotate keys</parameter></invoke>` + "\n```",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{
				Tools: []catalog.Tool{assistanttest.Tool(t, "create_task")},
				Acts:  []string{"create_work_item"},
				Rounds: []evals.Round{
					{Text: reply, Read: cliagent.ReadReply, Input: 100, Output: 40},
					evals.Call("c1", "create_task", map[string]any{"title": "Rotate keys"}),
				}})
			accepted, err := f.Ask("lost-call", "File a task to rotate keys", nil)
			if err != nil {
				t.Fatal(err)
			}
			frames := f.Follow(accepted.TurnID, 0)

			if done := evals.Last(frames, wire.EventDone); done == nil ||
				done.Done.State != wire.TurnWaitingForApproval {
				t.Fatalf("the turn ended %+v, want it waiting on the proposal it made", done)
			}
			if proposals := f.Store.Proposals(); len(proposals) != 1 {
				t.Fatalf("the turn recorded %d proposals, want the one made through the contract",
					len(proposals))
			}
			var states []wire.StepState
			for _, fr := range frames {
				if fr.Kind == wire.EventStep && fr.Step.Tool == "create_task" &&
					fr.Step.State != wire.StepRunning {
					states = append(states, fr.Step.State)
				}
			}
			if len(states) != 2 || states[0] != wire.StepRefused || states[1] != wire.StepWaiting {
				t.Errorf("create_task steps ended %v, want the faulted reply's call refused, "+
					"then the proposal waiting", states)
			}
			for _, fr := range frames {
				if fr.Kind == wire.EventDelta && strings.Contains(fr.Delta.Text, "prepared") {
					t.Errorf("the faulted reply's claim was shown: %q", fr.Delta.Text)
				}
			}
		})
	}
}
