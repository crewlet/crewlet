package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
	hold := builtin.NewCallHold(desk, seal, nil, nil)
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
	hold := builtin.NewCallHold(desk, callSeal(t), nil, nil)
	if got, held := hold.Hold(t.Context(), &turnctx.Turn{RunID: "run-1"}, call); held ||
		!got.Failed || len(desk.recorded) != 0 {
		t.Fatalf("a call with no job = %+v, %v, recorded %d", got, held, len(desk.recorded))
	}
	asked := &askDesk{outcome: sandbox.AskAlreadyAsked}
	got, held := builtin.NewCallHold(asked, callSeal(t), nil, nil).Hold(turnctx.WithJob(t.Context(), "launch-1"),
		&turnctx.Turn{RunID: "run-1"}, call)
	if held || tools.RefusalOf(got) != tools.RefusalNotRunning {
		t.Fatalf("a call while the job waits = %+v, %v, want not running", got, held)
	}
	if builtin.NewCallHold(desk, nil, nil, nil) != nil || builtin.NewCallHold(nil, callSeal(t), nil, nil) != nil {
		t.Fatal("a holder was built without its desk or its seal")
	}
}

// A CALL TOO LARGE TO HOLD IS REFUSED BEFORE ANYTHING IS ASKED, ITS SIZE NAMED. A
// call held for approval never reaches its server, so nothing but the run's row
// bounds what it carries, and the row carries its arguments sealed — a
// megabyte file of markup three and a half times over. The holder refuses such
// a call while the run can still split or shorten it, saying how large it is
// and how large one may be, and records no question; a call within the share is
// held as ever.
//
// Mutation: hold the call whatever it carries, and the store refuses the
// question, which the run is told to make again, refused every time.
func TestACallTooLargeToHoldIsRefusedBeforeAnythingIsAsked(t *testing.T) {
	t.Parallel()
	desk := &askDesk{outcome: sandbox.AskRecorded}
	hold := builtin.NewCallHold(desk, callSeal(t), nil, nil)
	turn := &turnctx.Turn{RunID: "run-1"}
	ctx := turnctx.WithJob(t.Context(), "launch-1")
	entry := tools.Entry{Tool: trackerTool{}, Origin: tools.Origin("tracker"), NeedsApproval: true}
	got, held := hold.Hold(ctx, turn, tools.HeldCall{Name: "tr_close_issue", Entry: entry,
		Args: map[string]any{"id": "7", "attachment": strings.Repeat("<p>", 1<<20/3)}})
	if held || !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, "at most 256 KiB") || !strings.Contains(got.Output, "Split it") {
		t.Fatalf("Hold = %+v, %v; want the call refused as invalid, its size and the bound named", got, held)
	}
	if len(desk.recorded) != 0 {
		t.Fatalf("a call too large to hold put %d questions to a person", len(desk.recorded))
	}
	if _, held := hold.Hold(ctx, turn, tools.HeldCall{Name: "tr_close_issue", Entry: entry,
		Args: map[string]any{"id": "7", "comment": "close it, it is a duplicate"}}); !held {
		t.Fatal("a call within the share was not held")
	}
}

// actedOn answers one decision for the answers it is asked about, or fails,
// and records what it was asked.
type actedOn struct {
	act   *sandbox.HeldAct
	err   error
	asked *[][]string
}

func (a actedOn) DecisionAnswered(_ context.Context, answers []string, _ string) (*sandbox.HeldAct, error) {
	if a.asked != nil {
		*a.asked = append(*a.asked, answers)
	}
	if a.act == nil {
		return nil, a.err
	}
	copied := *a.act
	return &copied, a.err
}

// A HELD CALL NAMES THE DECISION IT CARRIES OUT ONLY ON THE TURN THAT DECISION'S
// ANSWER WOKE: a person's answer to the seat's question, delivered to the
// turn, rides the held call and its context, so an approver reads a step of an
// answer already given. A later turn on the item, woken by anything else,
// names none and does not read for one; a read that fails holds the call
// naming none.
//
// Mutation: name a decision on a turn no answer woke, drop it from the held
// call or its context, or refuse a call whose decision could not be read, and
// this goes red.
func TestAHeldCallNamesTheDecisionItsTurnWasWokenFor(t *testing.T) {
	t.Parallel()
	act := &sandbox.HeldAct{Item: "id-9", Key: "ENG-9", Ask: "q-2", Question: "Ship the migration?",
		Choice: "Ship it", AnsweredBy: "jane", AnsweredAt: time.Unix(1_700_000_000, 0).UTC()}
	call := tools.HeldCall{Name: "tr_close_issue",
		Entry: tools.Entry{Tool: trackerTool{}, Origin: tools.Origin("tracker"), NeedsApproval: true},
		Args:  map[string]any{"id": "7"}}
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "id-9", Key: "ENG-9"}
	hold := func(acting builtin.ActedDecisions, answered ...string) sandbox.Ask {
		t.Helper()
		desk := &askDesk{outcome: sandbox.AskRecorded}
		turn := turnFor(t, "agent-ceo")
		turn.WorkItem, turn.Answered = item, answered
		if _, held := builtin.NewCallHold(desk, callSeal(t), acting, nil).Hold(
			turnctx.WithJob(t.Context(), "launch-1"), turn, call); !held || len(desk.recorded) != 1 {
			t.Fatalf("the call was not held (%v, %d asks)", held, len(desk.recorded))
		}
		return desk.recorded[0]
	}

	var asked [][]string
	ask := hold(actedOn{act: act, asked: &asked}, "a-2")
	got := ask.Held.Acting
	if got == nil || got.Item != "id-9" || got.Key != "ENG-9" || got.Ask != "q-2" || got.Choice != "Ship it" {
		t.Fatalf("the held call carries out %+v, want q-2 on ENG-9", got)
	}
	if len(asked) != 1 || !slices.Equal(asked[0], []string{"a-2"}) {
		t.Errorf("the decision was read for %v, want the answer that woke the turn", asked)
	}
	if want := `It carries out a decision on ENG-9: jane answered "Ship the migration?" with "Ship it".`; !strings.Contains(ask.Context, want) {
		t.Errorf("the context says\n%s\nwant it to say\n%s", ask.Context, want)
	}
	if err := ask.Validate(); err != nil {
		t.Errorf("an ask naming its decision does not validate: %v", err)
	}
	words := *act
	words.Choice = ""
	if ask := hold(actedOn{act: &words}, "a-2"); !strings.Contains(ask.Context, `answered "Ship the migration?" in words.`) {
		t.Errorf("an answer in words reads\n%s", ask.Context)
	}

	asked = nil
	later := hold(actedOn{act: act, asked: &asked})
	if later.Held.Acting != nil || strings.Contains(later.Context, "carries out a decision") || len(asked) != 0 {
		t.Errorf("a turn no answer woke names %+v (read for %v)", later.Held.Acting, asked)
	}
	for name, a := range map[string]sandbox.Ask{
		"a read that failed": hold(actedOn{err: errors.New("the tracker did not answer")}, "a-2"),
		"no decision given":  hold(actedOn{}, "a-2"),
	} {
		if a.Held.Acting != nil || strings.Contains(a.Context, "carries out a decision") {
			t.Errorf("%s: the call names %+v", name, a.Held.Acting)
		}
	}
}
