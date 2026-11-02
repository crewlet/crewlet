package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/tools"
)

// trackerTool is an MCP server's tool as a held call names it.
type trackerTool struct{}

func (trackerTool) Name() string               { return "tr_close_issue" }
func (trackerTool) Description() string        { return "Close an issue." }
func (trackerTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (trackerTool) Server() string             { return "tracker" }
func (trackerTool) RawName() string            { return "close_issue" }
func (trackerTool) Digest() string             { return "sha256:d1" }
func (trackerTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "closed"}, nil
}

// callSeal is a seal over a keyring of the test's own.
func callSeal(t *testing.T) *sandbox.CallSeal {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.NewCallSeal(cipher)
}

// A HELD CALL IS RECORDED AS THE JOB'S QUESTION, IN THE SHAPE A DECISION IS
// ASKED (ADR-0068, ADR-0077): on the job the call was made from, put to the
// seat's lead, with the three answers every held call offers and the work item
// the run is on as evidence; its arguments sealed on the held call and in no
// text the question carries; and the run told it was not called and to end its
// turn.
//
// Mutation: put the arguments in the question's context, and the case fails
// on the secret they carry.
func TestAHeldCallIsRecordedAsADecisionWithItsArgumentsSealed(t *testing.T) {
	t.Parallel()
	desk := &askDesk{outcome: sandbox.AskRecorded}
	seal := callSeal(t)
	hold := builtin.NewCallHold(desk, seal)
	turn := &turnctx.Turn{RunID: "run-1", WorkItem: &types.WorkItem{Backend: types.WorkWorklet,
		ID: "id-9", Key: "ENG-9"}}
	ctx := turnctx.WithJob(t.Context(), "launch-1")
	const secret = "hunter2-issue-body"
	got, held := hold.Hold(ctx, turn, tools.HeldCall{Name: "tr_close_issue",
		Entry: tools.Entry{Tool: trackerTool{}, Origin: tools.Origin("tracker"), NeedsApproval: true},
		Args:  map[string]any{"id": "7", "comment": secret}})
	if !held || got.Failed || !strings.Contains(got.Output, "was NOT called") ||
		!strings.Contains(got.Output, "END YOUR TURN") {
		t.Fatalf("Hold = %+v, %v, want the call held and the run told to end its turn", got, held)
	}
	if len(desk.recorded) != 1 || desk.runs[0] != "run-1" || desk.launches[0] != "launch-1" {
		t.Fatalf("recorded %d asks under %v / %v, want one under run-1 / launch-1",
			len(desk.recorded), desk.runs, desk.launches)
	}
	ask := desk.recorded[0]
	if err := ask.Validate(); err != nil {
		t.Fatalf("the held call's ask does not validate: %v", err)
	}
	h := ask.Held
	if h == nil || h.Server != "tracker" || h.Raw != "close_issue" || h.Digest != "sha256:d1" ||
		h.LaunchID != "launch-1" || h.ID == "" || ask.To != sandbox.AskToManager {
		t.Fatalf("the held call is %+v, put to %q", h, ask.To)
	}
	d := ask.Decision
	if d == nil || len(d.Options) != 3 || d.Options[0].ID != sandbox.HeldOnce ||
		len(d.Evidence) != 1 || d.Evidence[0].Ref != "id-9" {
		t.Fatalf("the decision is %+v, want once, always and deny citing the work item", d)
	}
	for _, text := range []string{ask.Question, ask.Context, h.Arguments} {
		if strings.Contains(text, secret) {
			t.Fatalf("the held call carries its arguments in the clear: %q", text)
		}
	}
	opened, err := seal.OpenArguments("run-1", *h)
	if err != nil || opened["comment"] != secret {
		t.Fatalf("the sealed arguments open to %v, %v", opened, err)
	}
	if _, err := seal.OpenArguments("run-2", *h); err == nil {
		t.Fatal("a held call's arguments opened for another run")
	}
}

// A CALL NOT MADE BY A RUN THAT CAN PARK IS NEVER HELD: a call with no job
// names nothing to park, and one made while the job already waits on a person
// is refused, since one answer carries a run on.
func TestACallIsHeldOnlyForAJobThatCanParkOnIt(t *testing.T) {
	t.Parallel()
	call := tools.HeldCall{Name: "tr_close_issue", Entry: tools.Entry{Tool: trackerTool{}}}
	desk := &askDesk{outcome: sandbox.AskRecorded}
	hold := builtin.NewCallHold(desk, callSeal(t))
	if got, held := hold.Hold(t.Context(), &turnctx.Turn{RunID: "run-1"}, call); held ||
		!got.Failed || len(desk.recorded) != 0 {
		t.Fatalf("a call with no job = %+v, %v, recorded %d", got, held, len(desk.recorded))
	}
	asked := &askDesk{outcome: sandbox.AskAlreadyAsked}
	got, held := builtin.NewCallHold(asked, callSeal(t)).Hold(turnctx.WithJob(t.Context(), "launch-1"),
		&turnctx.Turn{RunID: "run-1"}, call)
	if held || tools.RefusalOf(got) != tools.RefusalNotRunning {
		t.Fatalf("a call while the job waits = %+v, %v, want not running", got, held)
	}
	if builtin.NewCallHold(desk, nil) != nil || builtin.NewCallHold(nil, callSeal(t)) != nil {
		t.Fatal("a holder was built without its desk or its seal")
	}
}
