package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// deskFake is the run record and the seat inboxes answer_executor reaches.
type deskFake struct {
	runs       map[string]sandbox.PendingRun
	readErr    error
	deliverErr error
	delivered  []types.ExecutorAnswerGiven
	// seal opens a run's recorded answer for who gave it, as the node's
	// desk does; nil opens nothing.
	seal *sandbox.AnswerSeal
}

func (d *deskFake) Answerer(run sandbox.PendingRun) string {
	return sandbox.OpenedAnswerer(d.seal, run)
}

// answerSeal is a fleet keyring's answer seal, under key material of its own.
func answerSeal(t *testing.T, material string) *sandbox.AnswerSeal {
	t.Helper()
	seal, err := sandbox.NewAnswerSeal(statelog.OneKey("answers", material))
	if err != nil {
		t.Fatalf("NewAnswerSeal: %v", err)
	}
	return seal
}

// answeredBy records an answer on run as the coordinator does, sealed under
// seal: who gave it lives only inside the seal.
func answeredBy(t *testing.T, seal *sandbox.AnswerSeal, run sandbox.PendingRun, by string,
	at time.Time) sandbox.PendingRun {
	t.Helper()
	answer, err := seal.Seal(run, sandbox.AnswerWords{Text: "use main", Via: types.AnswerViaOperator,
		By: by, ByKind: "human"}, sandbox.RecordedAnswer{EventIDs: []string{"answer-1"}, RecordedAt: at})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	run.Answer = &answer
	return run
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

func (d *deskFake) Deliver(_ context.Context, given types.ExecutorAnswerGiven) (uuid.UUID, error) {
	d.delivered = append(d.delivered, given)
	return deliveredID, d.deliverErr
}

// answerExecutorTool is the operator catalogue's answer_executor over these
// fakes, called as the deployment's own operator.
func answerExecutorTool(t *testing.T, desk *deskFake) executorAnswerRig {
	t.Helper()
	return newAnswerExecutorRig(t, desk, operatorCaller, seatLeads)
}

// executorAnswerRig is answer_executor called as one principal, decided by the
// real authority table over a chart. The principal rides the CALL's own
// context, so a case that cancels its context cancels the call.
type executorAnswerRig struct {
	tool   tools.Callable
	caller iam.Principal
}

func (r executorAnswerRig) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return r.tool.Call(iam.WithPrincipal(ctx, r.caller), args)
}

func newAnswerExecutorRig(t *testing.T, desk *deskFake, caller iam.Principal,
	chart authz.Chart) executorAnswerRig {

	t.Helper()
	o := organization(t)
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Org:       func() *org.Organization { return o },
		Runs:      builtin.RunDeps{Desk: desk, Actor: builtin.PrincipalActor},
		Authorize: builtin.Decide(chart),
	}) {
		if tool.Name() == builtin.AnswerExecutorTool {
			return executorAnswerRig{tool: tool, caller: caller}
		}
	}
	t.Fatal("the operator catalogue serves no answer_executor with a run desk wired")
	return executorAnswerRig{}
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
func TestAnswerExecutorDeliversTheAnswerAsThePersonAndAnswersPending(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusReseed)}}
	result, err := answerExecutorTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": " t1 ", "launch_id": "launch-t1", "answer": "  use main  "})
	if err != nil || result.Failed {
		t.Fatalf("answer_executor = %+v, %v", result, err)
	}
	// AS THE PERSON, in iam.ActorFor's three halves, about the run's seat
	// by its id.
	want := types.ExecutorAnswerGiven{
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
	if answer["outcome"] != "pending" || answer["launch_id"] != "launch-t1" ||
		answer["answer_id"] != deliveredID.String() {
		t.Errorf("answer = %v, want pending with the question it answered and the "+
			"answer's own id", answer)
	}
	// THE QUESTION IT ANSWERED, as the run's words: fenced under the seat
	// whose coding run asked it (ADR-0040).
	if got := unfenced(t, answer["question"]); got != "which branch?" {
		t.Errorf("the answer carries the question %q, want the one it answered", got)
	}
}

// A RUN THAT IS NOT WAITING IS REFUSED `not_running`, and nothing is delivered:
// a running job, a claimed one, and a run with no record — which is what a run
// that ended has — are all runs no answer can reach.
func TestAnswerExecutorRefusesARunThatIsNotWaiting(t *testing.T) {
	t.Parallel()
	for _, status := range []string{sandbox.StatusRunning, sandbox.StatusLaunching, sandbox.StatusResumed} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", status)}}
			result, _ := answerExecutorTool(t, desk).Call(t.Context(),
				map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use main"})
			if !result.Failed || tools.RefusalOf(result) != tools.RefusalNotRunning {
				t.Fatalf("answer_executor on a %s run = %+v, want not_running", status, result)
			}
			if len(desk.delivered) != 0 {
				t.Errorf("delivered %+v to a run that is not waiting", desk.delivered)
			}
		})
	}
	desk := &deskFake{runs: map[string]sandbox.PendingRun{}}
	result, _ := answerExecutorTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "gone", "launch_id": "launch-gone", "answer": "use main"})
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalNotRunning {
		t.Fatalf("answer_executor on a run with no record = %+v, want not_running", result)
	}
}

// A DELIVERY THE BROKER NEVER CONFIRMED IS `unknown`, not a refusal: it may be
// on the inbox, and a person told it was refused would answer somewhere else.
func TestAnswerExecutorThatMayHaveLandedAnswersUnknown(t *testing.T) {
	t.Parallel()
	desk := &deskFake{
		runs:       map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusAwaiting)},
		deliverErr: errors.New("publish ack timed out"),
	}
	result, _ := answerExecutorTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use main"})
	if result.Failed || !strings.Contains(result.Output, `"outcome": "unknown"`) {
		t.Fatalf("answer_executor over an unconfirmed delivery = %+v, want outcome unknown", result)
	}
}

// AN ANSWER IS HELD TO THE PAGE ITS QUESTION IS: it rides beside the question,
// on the run's row and then in the prompt of the run it carries on, which that
// run's coding agent reads again at every step. One as long as the longest
// question is delivered whole; one a byte longer is refused `invalid`, with
// nothing delivered, and the refusal says how long it is — rounded up, so a
// byte past the bound never reads as the bound itself — what the bound is, and
// where the detail goes instead.
func TestAnAnswerIsHeldToThePageItsQuestionIs(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusAwaiting)}}
	tool := answerExecutorTool(t, desk)
	result, _ := tool.Call(t.Context(), map[string]any{
		"turn_id": "t1", "launch_id": "launch-t1", "answer": strings.Repeat("a", builtin.MaxRunAnswerBytes+1),
	})
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalInvalid {
		t.Fatalf("answer_executor with an answer a byte past a page = %+v, want invalid", result)
	}
	if len(desk.delivered) != 0 {
		t.Fatalf("an answer past the bound was delivered: %d bytes", len(desk.delivered[0].Answer))
	}
	for _, want := range []string{
		fmt.Sprintf("That answer is %d KiB", builtin.MaxRunAnswerBytes>>10+1),
		fmt.Sprintf("at most %d KiB", builtin.MaxRunAnswerBytes>>10),
		"Put the detail on a page",
	} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("the refusal says %q, want it to say %q", result.Output, want)
		}
	}

	page := strings.Repeat("a", builtin.MaxRunAnswerBytes)
	result, err := tool.Call(t.Context(), map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": page})
	if err != nil || result.Failed {
		t.Fatalf("answer_executor with an answer as long as a question may be = %+v, %v; "+
			"want it delivered", result, err)
	}
	if len(desk.delivered) != 1 || desk.delivered[0].Answer != page {
		t.Fatalf("delivered %d answers, want the one answer whole", len(desk.delivered))
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
			result, err := newAnswerExecutorRig(t, desk, tc.caller, seatLeads).Call(
				t.Context(), map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use main"})
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
//
// WHOSE, AS THE ANSWER'S SEAL SAYS: the answerer is kept nowhere else, so an
// answer whose seal does not open on this node is "someone else's" rather than
// a guess.
//
// Mutation: name the answerer off anything but the opened seal, and the
// unopened case names somebody.
func TestAnAnsweredQuestionRefusesTheNextAnswerAndSaysWhose(t *testing.T) {
	t.Parallel()
	seal := answerSeal(t, "the fleet's key material")
	run := answeredBy(t, seal, parked(t, "t1", sandbox.StatusAnswered), "miles",
		time.Date(2026, 10, 28, 0, 35, 6, 0, time.UTC))

	unopened := &deskFake{runs: map[string]sandbox.PendingRun{"t1": run},
		seal: answerSeal(t, "another fleet's key material")}
	result, err := answerExecutorTool(t, unopened).Call(t.Context(),
		map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use the release branch"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "already answered by someone else at 2026-10-28T00:35:06Z") {
		t.Fatalf("answer_executor under a seal that does not open = %+v, want it answered by someone "+
			"else", result)
	}

	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": run}, seal: seal}
	result, err = answerExecutorTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use the release branch"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalAlreadyAnswered ||
		!strings.Contains(result.Output, "already answered by miles at 2026-10-28T00:35:06Z") {
		t.Fatalf("answer_executor = %+v, want an already_answered refusal naming who answered and "+
			"when, in RFC 3339", result)
	}
	if len(desk.delivered) != 0 {
		t.Fatalf("delivered %+v for a question already answered", desk.delivered)
	}
	// A PERSON IS TOLD WHOSE IT WAS, and their own screen says when on their
	// own clock: the engine's UTC instant is the model's, never theirs.
	if person := result.PersonRefusal.Person(); !strings.Contains(person, "@miles") ||
		strings.Contains(person, "UTC") {
		t.Fatalf("a person is told %q, want who answered and no engine clock", person)
	}
}

// WHAT THE REFUSAL SAYS BECAME OF THE ANSWER IS WHAT THE RECORD SAYS: waiting
// on its seat while the run is answered, used once it was resumed, and nothing
// claimed once the run has moved on to something else.
func TestTheAnsweredRefusalSaysOnlyWhatTheRecordSays(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]string{
		sandbox.StatusAnswered: "resumes with that answer once its seat is free",
		sandbox.StatusResumed:  "was resumed with that answer",
		sandbox.StatusRunning:  "",
	} {
		seal := answerSeal(t, "the fleet's key material")
		run := answeredBy(t, seal, parked(t, "t1", status), "miles", time.Time{})
		desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": run}, seal: seal}
		result, err := answerExecutorTool(t, desk).Call(t.Context(),
			map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use the release branch"})
		if err != nil {
			t.Fatal(err)
		}
		claims := strings.Contains(result.Output, "resume")
		if (want == "") == claims || (want != "" && !strings.Contains(result.Output, want)) {
			t.Errorf("a %s run's refusal says %q, want %q", status, result.Output, want)
		}
	}
}

// decided is a parked run whose question offers a choice.
func decided(t *testing.T, turnID string) sandbox.PendingRun {
	t.Helper()
	run := parked(t, turnID, sandbox.StatusAwaiting)
	run.Decision = &decision.Decision{
		Question: run.Question, Role: decision.RoleApprover, Recommended: "keep",
		Options: []decision.Option{
			{ID: "keep", Label: "Keep the expected differences"},
			{ID: "exact", Label: "Keep style attributes byte for byte"},
		},
	}
	return run
}

// A CHOICE IS AN OPTION OF THE QUESTION IT ANSWERS, and is delivered as one: by
// its id, beside any words, so the run reads the option it named. A choice the
// question does not offer is refused before anything is delivered, naming the
// options it does, and so is a choice on an open question: the run would be
// told it chose something nobody offered. An answer is a choice or words, and
// never neither.
//
// Mutation: deliver without checking the choice against the run's decision,
// and the unoffered choice reaches the seat.
func TestAnswerExecutorDeliversAChoiceOfTheQuestionsOwnOptions(t *testing.T) {
	t.Parallel()
	desk := &deskFake{runs: map[string]sandbox.PendingRun{
		"t1": decided(t, "t1"), "t2": parked(t, "t2", sandbox.StatusAwaiting),
	}}
	tool := answerExecutorTool(t, desk)

	result, err := tool.Call(t.Context(), map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "choice": "exact",
		"answer": "and note it in the PR"})
	if err != nil || result.Failed {
		t.Fatalf("answer_executor with a choice = %+v, %v", result, err)
	}
	if len(desk.delivered) != 1 || desk.delivered[0].Choice != "exact" ||
		desk.delivered[0].Answer != "and note it in the PR" {
		t.Fatalf("delivered %+v, want the choice and the words", desk.delivered)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(result.Output), &receipt); err != nil || receipt["choice"] != "exact" {
		t.Errorf("the receipt %s does not name the choice", result.Output)
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"an unoffered choice", map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "choice": "rewrite"}, `it offers "keep" or "exact"`},
		{"a choice on an open question", map[string]any{"turn_id": "t2", "launch_id": "launch-t2", "choice": "keep"}, "no options"},
		{"neither a choice nor words", map[string]any{"turn_id": "t1", "launch_id": "launch-t1"}, "`choice`"},
	} {
		got, err := tool.Call(t.Context(), tc.args)
		if err != nil || !got.Failed || !strings.Contains(got.Output, tc.want) {
			t.Errorf("%s: answer_executor = %+v, %v; want refused naming %q", tc.name, got, err, tc.want)
		}
	}
	if len(desk.delivered) != 1 {
		t.Fatalf("%d answers delivered, want only the offered choice's", len(desk.delivered))
	}
}

// AN ANSWER IS TO THE QUESTION THE PERSON READ. It names that question by the
// job that asked it, and a run that has moved on to another question since
// refuses it, in words that say to read the question again, rather than taking
// it (a choice by id above all) as the answer to a question nobody showed them.
// One naming no question is refused before anything is read.
//
// Mutation: deliver against the row's own launch whatever the call named, and
// the answer to the old question resumes the run on the new one.
func TestAnAnswerIsToTheQuestionItsPersonRead(t *testing.T) {
	t.Parallel()
	moved := decided(t, "t1")
	moved.LaunchID = "launch-after"
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": moved}}
	tool := answerExecutorTool(t, desk)
	got, err := tool.Call(t.Context(), map[string]any{"turn_id": "t1", "launch_id": "launch-t1",
		"choice": "keep"})
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalStaleVersion ||
		!strings.Contains(got.Output, "moved on to another question") {
		t.Fatalf("an answer to a question the run moved past = %+v, %v; want a conflict in words", got, err)
	}
	if said := got.PersonRefusal.Person(); !strings.Contains(said, "Reload") {
		t.Errorf("the person is told %q, want to reload", said)
	}
	unnamed, err := tool.Call(t.Context(), map[string]any{"turn_id": "t1", "choice": "keep"})
	if err != nil || !unnamed.Failed || !strings.Contains(unnamed.Output, "`launch_id`") {
		t.Errorf("an answer naming no question = %+v, %v; want it refused naming launch_id", unnamed, err)
	}
	if len(desk.delivered) != 0 {
		t.Fatalf("delivered %+v, want nothing", desk.delivered)
	}
}
