package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// deskFake is the run record and the seat inboxes answer_run reaches.
type deskFake struct {
	runs       map[string]sandbox.PendingRun
	readErr    error
	deliverErr error
	delivered  []types.SandboxAnswerGiven
}

func (d *deskFake) Run(_ context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	if d.readErr != nil {
		return sandbox.PendingRun{}, false, d.readErr
	}
	run, ok := d.runs[turnID]
	return run, ok, nil
}

func (d *deskFake) Deliver(_ context.Context, given types.SandboxAnswerGiven) error {
	d.delivered = append(d.delivered, given)
	return d.deliverErr
}

// fleetFake answers the feature gate for the node holding each seat.
type fleetFake struct {
	lacks map[string]bool
	err   error
}

func (f fleetFake) SeatFeature(_ context.Context, handle string, feature coord.Feature) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return feature == coord.FeatureAnswerRunByTurn && !f.lacks[handle], nil
}

func (f fleetFake) AllLiveHave(context.Context, coord.Feature) (bool, error) { return true, nil }

// answerRunTool is the operator catalogue's answer_run over these fakes.
func answerRunTool(t *testing.T, desk *deskFake, fleet builtin.Fleet) tools.Callable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Fleet: fleet,
		Runs: builtin.RunDeps{Desk: desk, Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
			return builtin.Actor{Handle: "founder-token", Kind: tracker.AuthorOperator, Seat: "founder"}, nil
		}},
	}) {
		if tool.Name() == builtin.AnswerRunTool {
			return tool
		}
	}
	t.Fatal("the operator catalogue serves no answer_run with a run desk wired")
	return nil
}

func parked(turnID, status string) sandbox.PendingRun {
	return sandbox.PendingRun{TurnID: turnID, AgentHandle: "swe", Status: status, Question: "which branch?"}
}

// AN ANSWER IS PUT ON THE SEAT'S INBOX AS THE PERSON WHO GAVE IT, and the
// caller is told `pending`: the node holding the seat resumes the run, and this
// one cannot say what that became.
func TestAnswerRunDeliversTheAnswerAsThePersonAndAnswersPending(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked("t1", sandbox.StatusReseed)}}
	result, err := answerRunTool(t, desk, fleetFake{}).Call(t.Context(),
		map[string]any{"turn_id": " t1 ", "answer": "  use main  "})
	if err != nil || result.Failed {
		t.Fatalf("answer_run = %+v, %v", result, err)
	}
	want := types.SandboxAnswerGiven{
		TurnID: "t1", AgentHandle: "swe", Answer: "use main",
		AnsweredBy: "founder-token", AnsweredBySeat: "founder",
	}
	if len(desk.delivered) != 1 || desk.delivered[0] != want {
		t.Fatalf("delivered %+v, want %+v", desk.delivered, want)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(result.Output), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["outcome"] != "pending" || answer["question"] != "which branch?" {
		t.Errorf("answer = %v, want pending with the question it answered", answer)
	}
}

// A RUN THAT IS NOT WAITING IS REFUSED `not_running`, and nothing is delivered:
// a running job, a claimed one, and a run with no record — which is what a run
// that ended has — are all runs no answer can reach.
func TestAnswerRunRefusesARunThatIsNotWaiting(t *testing.T) {
	t.Parallel()
	for _, status := range []string{sandbox.StatusRunning, sandbox.StatusLaunching, sandbox.StatusResumed} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked("t1", status)}}
			result, _ := answerRunTool(t, desk, fleetFake{}).Call(t.Context(),
				map[string]any{"turn_id": "t1", "answer": "use main"})
			if !result.Failed || result.Refusal != tools.RefusalNotRunning {
				t.Fatalf("answer_run on a %s run = %+v, want not_running", status, result)
			}
			if len(desk.delivered) != 0 {
				t.Errorf("delivered %+v to a run that is not waiting", desk.delivered)
			}
		})
	}
	desk := &deskFake{runs: map[string]sandbox.PendingRun{}}
	result, _ := answerRunTool(t, desk, fleetFake{}).Call(t.Context(),
		map[string]any{"turn_id": "gone", "answer": "use main"})
	if !result.Failed || result.Refusal != tools.RefusalNotRunning {
		t.Fatalf("answer_run on a run with no record = %+v, want not_running", result)
	}
}

// AN OWNER THAT CANNOT READ THE EVENT IS REFUSED `peer_upgrading` — an older
// build would take the answer for an ordinary wake and run a turn about nothing
// while the run waited on — and a fleet that could not be read is
// `unavailable`, never either answer.
func TestAnswerRunRefusesAnOwnerThatCannotReadTheEvent(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked("t1", sandbox.StatusAwaiting)}}
	result, _ := answerRunTool(t, desk, fleetFake{lacks: map[string]bool{"swe": true}}).Call(
		t.Context(), map[string]any{"turn_id": "t1", "answer": "use main"})
	if !result.Failed || result.Refusal != tools.RefusalPeerUpgrading {
		t.Fatalf("answer_run to an older owner = %+v, want peer_upgrading", result)
	}
	result, _ = answerRunTool(t, desk, fleetFake{err: errors.New("the lease table is down")}).Call(
		t.Context(), map[string]any{"turn_id": "t1", "answer": "use main"})
	if !result.Failed || result.Refusal != tools.RefusalUnavailable {
		t.Fatalf("answer_run over an unreadable fleet = %+v, want unavailable", result)
	}
	if len(desk.delivered) != 0 {
		t.Errorf("delivered %+v past the feature gate", desk.delivered)
	}
}

// A DELIVERY THE BROKER NEVER CONFIRMED IS `unknown`, not a refusal: it may be
// on the inbox, and a person told it was refused would answer somewhere else.
func TestAnswerRunThatMayHaveLandedAnswersUnknown(t *testing.T) {
	t.Parallel()
	desk := &deskFake{
		runs:       map[string]sandbox.PendingRun{"t1": parked("t1", sandbox.StatusAwaiting)},
		deliverErr: errors.New("publish ack timed out"),
	}
	result, _ := answerRunTool(t, desk, fleetFake{}).Call(t.Context(),
		map[string]any{"turn_id": "t1", "answer": "use main"})
	if result.Failed || !strings.Contains(result.Output, `"outcome": "unknown"`) {
		t.Fatalf("answer_run over an unconfirmed delivery = %+v, want outcome unknown", result)
	}
}

// AN ANSWER IS BOUNDED, and the refusal names the bound: it is spliced into the
// run as one tool reply.
func TestAnswerRunRefusesAnAnswerTooLongToSplice(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked("t1", sandbox.StatusAwaiting)}}
	result, _ := answerRunTool(t, desk, fleetFake{}).Call(t.Context(), map[string]any{
		"turn_id": "t1", "answer": strings.Repeat("a", builtin.MaxRunAnswerBytes+1),
	})
	if !result.Failed || result.Refusal != tools.RefusalInvalid {
		t.Fatalf("answer_run with an oversized answer = %+v, want invalid", result)
	}
	if len(desk.delivered) != 0 {
		t.Error("an oversized answer was delivered")
	}
}
