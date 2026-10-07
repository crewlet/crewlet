package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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

// answerRunTool is the operator catalogue's answer_run over these fakes.
func answerRunTool(t *testing.T, desk *deskFake) tools.Callable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
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
	return sandbox.PendingRun{TurnID: turnID, AgentHandle: "swe", Status: status, Question: "which branch?",
		LaunchID: "launch-" + turnID}
}

// AN ANSWER IS PUT ON THE SEAT'S INBOX AS THE PERSON WHO GAVE IT, naming the
// question it answers, and the caller is told `pending`: the node holding the
// seat resumes the run, and this one cannot say what that became. The question
// — the job the run held when it asked — is what keeps an answer
// that reaches the seat late from resuming the run as the answer to a later
// question.
func TestAnswerRunDeliversTheAnswerAsThePersonAndAnswersPending(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked("t1", sandbox.StatusReseed)}}
	result, err := answerRunTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": " t1 ", "answer": "  use main  "})
	if err != nil || result.Failed {
		t.Fatalf("answer_run = %+v, %v", result, err)
	}
	want := types.SandboxAnswerGiven{
		TurnID: "t1", AgentHandle: "swe", Answer: "use main",
		AnsweredBy: "founder-token", AnsweredBySeat: "founder",
		LaunchID: "launch-t1",
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
			result, _ := answerRunTool(t, desk).Call(t.Context(),
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
	result, _ := answerRunTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "gone", "answer": "use main"})
	if !result.Failed || result.Refusal != tools.RefusalNotRunning {
		t.Fatalf("answer_run on a run with no record = %+v, want not_running", result)
	}
}

// A RUN NAMING NO JOB HAS NO QUESTION AN ANSWER CAN NAME, and the node holding
// the seat spends an answer naming none without resuming the run — so the
// answer is refused here, where the person can be told, rather than delivered
// to be spent unread.
func TestAnswerRunRefusesARunWhoseQuestionItCannotName(t *testing.T) {
	t.Parallel()
	unnamed := parked("t1", sandbox.StatusAwaiting)
	unnamed.LaunchID = ""
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": unnamed}}
	result, _ := answerRunTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "t1", "answer": "use main"})
	if !result.Failed || result.Refusal != tools.RefusalNotRunning {
		t.Fatalf("answer_run on a run naming no job = %+v, want not_running", result)
	}
	if len(desk.delivered) != 0 {
		t.Errorf("delivered %+v, an answer that names no question", desk.delivered)
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
	result, _ := answerRunTool(t, desk).Call(t.Context(),
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
	result, _ := answerRunTool(t, desk).Call(t.Context(), map[string]any{
		"turn_id": "t1", "answer": strings.Repeat("a", builtin.MaxRunAnswerBytes+1),
	})
	if !result.Failed || result.Refusal != tools.RefusalInvalid {
		t.Fatalf("answer_run with an oversized answer = %+v, want invalid", result)
	}
	if len(desk.delivered) != 0 {
		t.Error("an oversized answer was delivered")
	}
}
