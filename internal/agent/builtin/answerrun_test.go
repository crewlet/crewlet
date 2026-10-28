package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
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

// deliveredID is the id deskFake answers every delivery with.
var deliveredID = uuid.MustParse("6f1c2b8e-3d4a-4e5f-9a0b-1c2d3e4f5a6b")

func (d *deskFake) Deliver(_ context.Context, given types.SandboxAnswerGiven) (uuid.UUID, error) {
	d.delivered = append(d.delivered, given)
	return deliveredID, d.deliverErr
}

// answerRunTool is the operator catalogue's answer_run over these fakes, called
// as the deployment's own operator.
func answerRunTool(t *testing.T, desk *deskFake) runAnswerRig {
	t.Helper()
	return newAnswerRunRig(t, desk, operatorCaller, seatLeads)
}

// runAnswerRig is answer_run called as one principal, decided by the real
// authority table over a chart. The principal rides the CALL's own context,
// so a case that cancels its context cancels the call.
type runAnswerRig struct {
	tool   tools.Callable
	caller iam.Principal
}

func (r runAnswerRig) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return r.tool.Call(iam.WithPrincipal(ctx, r.caller), args)
}

func newAnswerRunRig(t *testing.T, desk *deskFake, caller iam.Principal,
	chart authz.Chart) runAnswerRig {

	t.Helper()
	o := organization(t)
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Org:       func() *org.Organization { return o },
		Runs:      builtin.RunDeps{Desk: desk, Actor: builtin.PrincipalActor},
		Authorize: builtin.Decide(chart),
	}) {
		if tool.Name() == builtin.AnswerRunTool {
			return runAnswerRig{tool: tool, caller: caller}
		}
	}
	t.Fatal("the operator catalogue serves no answer_run with a run desk wired")
	return runAnswerRig{}
}

// parked is a run of the CTO's seat a turn the founder's message woke
// launched, naming the founder's seat by its handle.
func parked(t *testing.T, turnID, status string) sandbox.PendingRun {
	t.Helper()
	return sandbox.PendingRun{TurnID: turnID, AgentHandle: "agent-cto",
		AgentID: ctoSeat(t).String(), Requester: "founder", Status: status,
		Question: "which branch?", LaunchID: "launch-" + turnID}
}

// AN ANSWER IS DELIVERED AS THE PERSON WHO GAVE IT, naming the question it
// answers, and the caller is told `pending`: the node holding the seat records
// and resumes it, and this one cannot say what that became. The question (the
// job the run held when it asked) is what keeps an answer that reaches the
// seat late from resuming the run as the answer to a later question. And the
// caller is told the question and the answer's own id, which is what it reads
// the run's record against to learn what became of the answer: recorded on
// that question, let go of among its declined answers, or overtaken by a new
// one.
func TestAnswerRunDeliversTheAnswerAsThePersonAndAnswersPending(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusReseed)}}
	result, err := answerRunTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": " t1 ", "answer": "  use main  "})
	if err != nil || result.Failed {
		t.Fatalf("answer_run = %+v, %v", result, err)
	}
	// AS THE PERSON, in iam.ActorFor's three halves, about the run's seat
	// by its id.
	want := types.SandboxAnswerGiven{
		TurnID: "t1", Agent: ctoSeat(t).String(), AgentHandle: "agent-cto",
		Answer: "use main", AnsweredBy: "ops", AnsweredByKind: "human",
		OperatorID: "session:ops-1", LaunchID: "launch-t1",
	}
	if len(desk.delivered) != 1 || desk.delivered[0] != want {
		t.Fatalf("delivered %+v, want %+v", desk.delivered, want)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(result.Output), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["outcome"] != "pending" || answer["question"] != "which branch?" ||
		answer["launch_id"] != "launch-t1" || answer["answer_id"] != deliveredID.String() {
		t.Errorf("answer = %v, want pending with the question it answered and the "+
			"answer's own id", answer)
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
			desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", status)}}
			result, _ := answerRunTool(t, desk).Call(t.Context(),
				map[string]any{"turn_id": "t1", "answer": "use main"})
			if !result.Failed || tools.RefusalOf(result) != tools.RefusalNotRunning {
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
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalNotRunning {
		t.Fatalf("answer_run on a run with no record = %+v, want not_running", result)
	}
}

// A DELIVERY THE BROKER NEVER CONFIRMED IS `unknown`, not a refusal: it may be
// on the inbox, and a person told it was refused would answer somewhere else.
func TestAnswerRunThatMayHaveLandedAnswersUnknown(t *testing.T) {
	t.Parallel()
	desk := &deskFake{
		runs:       map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusAwaiting)},
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
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusAwaiting)}}
	result, _ := answerRunTool(t, desk).Call(t.Context(), map[string]any{
		"turn_id": "t1", "answer": strings.Repeat("a", builtin.MaxRunAnswerBytes+1),
	})
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalInvalid {
		t.Fatalf("answer_run with an oversized answer = %+v, want invalid", result)
	}
	if len(desk.delivered) != 0 {
		t.Error("an oversized answer was delivered")
	}
}

// A RUN'S QUESTION IS ITS REQUESTER'S TO ANSWER, or its seat's lead's — and
// nobody else's. The requester leads nobody and is admitted as themselves (the
// run's row names their seat by its handle); the CTO's
// lead is admitted as its lead; a colleague who is neither is refused with the
// authority's own answer, and nothing reaches the seat's inbox.
func TestARunIsAnsweredByItsRequesterOrItsSeatsLead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		caller  iam.Principal
		allowed bool
	}{
		{"the person who asked for the work", boundTo("founder"), true},
		{"the run's seat's lead", boundTo("jane"), true},
		{"a colleague who is neither", boundTo("sam", iam.Apps...), false},
		{"a seat, however granted", iam.Principal{ID: uuid.New(), Kind: iam.KindSeat,
			Login: "agent-ceo", Seat: "agent-ceo", Stage: iam.StageActive,
			Grants: iam.AllGrants}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			desk := &deskFake{runs: map[string]sandbox.PendingRun{
				"t1": parked(t, "t1", sandbox.StatusAwaiting)}}
			result, err := newAnswerRunRig(t, desk, tc.caller, seatLeads).Call(
				t.Context(), map[string]any{"turn_id": "t1", "answer": "use main"})
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.allowed && (result.Failed || len(desk.delivered) != 1):
				t.Fatalf("refused %s: %+v", tc.name, result)
			case !tc.allowed && (!result.Failed || !errors.Is(result.Cause, builtin.ErrRefused)):
				t.Fatalf("answered %s with %+v, want the authority's refusal", tc.name, result)
			case !tc.allowed && len(desk.delivered) != 0:
				t.Errorf("a refused answer was delivered: %+v", desk.delivered)
			}
		})
	}
}

// A QUESTION ANSWERED ALREADY REFUSES THE NEXT ANSWER, AND SAYS WHOSE IT WAS.
// The first recorded answer wins; a person answering after it is told it was
// answered, by whom and when, and nothing is delivered. Four people answering
// one run used to be told `pending` four times while three of the answers went
// nowhere they could see.
func TestAnAnsweredQuestionRefusesTheNextAnswerAndSaysWhose(t *testing.T) {
	t.Parallel()
	run := parked(t, "t1", sandbox.StatusAnswered)
	run.Answer = &sandbox.RecordedAnswer{Text: "use main", By: "miles",
		RecordedAt: time.Date(2026, 10, 28, 0, 35, 6, 0, time.UTC)}
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": run}}
	result, err := answerRunTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "t1", "answer": "use the release branch"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalNotRunning ||
		!strings.Contains(result.Output, "already answered by miles at 00:35 UTC on 28 Oct") {
		t.Fatalf("answer_run = %+v, want a not_running refusal naming who answered and when", result)
	}
	if len(desk.delivered) != 0 {
		t.Fatalf("delivered %+v for a question already answered", desk.delivered)
	}
}
