package assistant_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// getTask is the catalogue's own task read, whose answer other people wrote.
func getTask(t *testing.T) catalog.Tool { return assistanttest.Tool(t, "get_task") }

// task answers ENG-7.
var task = assistanttest.Routes(map[string]exec.Response{
	"GET /work/ENG-7": assistanttest.Task("ENG-7", "Rotate keys", "Rotate the signing keys."),
})

// readTask is a round asking for ENG-7.
func readTask() evals.Round {
	return evals.Call("c1", "get_task", map[string]any{"item": "ENG-7"})
}

// A READ IS ONE REQUEST, THROUGH THE DISPATCHER, ON THE TURN'S GRANT.
//
// The assistant's every reach into the company is an ordinary request the
// API's handler chain decides as the person (ADR-0049). So a scripted turn
// that reads once sends exactly the one request its tool builds, carrying the
// grant the question derived, and the model reads the answer only as the
// engine shaped it.
//
// Mutation: serve the read some other way than the dispatcher (or on another
// grant) and the sent list goes wrong.
func TestTheAssistantReachesToolsOnlyAsRequests(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task,
		Rounds: []evals.Round{readTask(),
			evals.Say("ENG-7 is to rotate the keys.")}})
	accepted, err := f.Ask("read", "Who works here?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	sent := f.Dispatcher.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d requests, want the one the tool builds: %+v", len(sent), sent)
	}
	if got := sent[0]; got.Request.Method != http.MethodGet || got.Request.Path != "/work/ENG-7" ||
		got.Grant != f.Grant("read").ID() {
		t.Errorf("sent %+v on grant %s, want GET /work/ENG-7 on the question's grant", got.Request, got.Grant)
	}
	done := evals.Last(frames, wire.EventDone)
	if done == nil || done.Done.State != wire.TurnCompleted || done.Done.Steps != 1 {
		t.Fatalf("done = %+v, want a completed turn of one step", done)
	}
	var tool llm.Message
	for _, req := range f.Model.Requests() {
		for _, msg := range req.Messages {
			if msg.Role == llm.RoleTool {
				tool = msg
			}
		}
	}
	if tool.Role != llm.RoleTool || !strings.Contains(tool.Content, "[untrusted:") ||
		!strings.Contains(tool.Content, "source=api_answer") {
		t.Errorf("the model read the answer as %q, want it fenced as an api answer", tool.Content)
	}
	if !f.Revoker.Revoked(f.Grant("read").ID()) {
		t.Error("the turn ended and its grant is still live")
	}
}

// NOTHING SECRET REACHES A RECORD, A STREAM, AN AUDIT ROW OR A PROMPT.
//
// The assistant holds a grant's handle and never its token, so the token can
// reach nothing; and an answer carrying a credential is redacted before the
// model, the transcript, the stream or the audit see it.
//
// Mutation: hand the model the raw answer (drop redact.Secrets in the
// surface) and the key appears in the prompt.
func TestNoTokenReachesARecordALogOrAPrompt(t *testing.T) {
	t.Parallel()
	const key = "ghp_" + "Zq3vT8mN2pL6wR9xK4cB7yH1jD5fG0sA2eQ8"
	leaky := assistanttest.Routes(map[string]exec.Response{
		"GET /work/ENG-7": assistanttest.Task("ENG-7", "Rotate keys", "token "+key),
	})
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: leaky,
		Rounds: []evals.Round{readTask(),
			evals.Say("Here it is: " + key)}})
	accepted, err := f.Ask("leak", "Show me the token "+key, nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	stream, _ := json.Marshal(frames)
	audit, _ := json.Marshal(f.Audit.Events())
	for name, text := range map[string]string{
		"the prompt": f.Model.Prompts(), "the stream": string(stream),
		"the records": strings.Join(f.Store.Records(), "\n"), "the audit": string(audit),
	} {
		if strings.Contains(text, key) {
			t.Errorf("%s carries the key", name)
		}
		if strings.Contains(text, "cwl_ast_SENTINEL") {
			t.Errorf("%s carries the grant's token", name)
		}
	}
}

// A BUDGET REFUSAL STOPS BEFORE A ROUND, AND BEFORE ANYTHING IS RECORDED.
//
// A company whose window is already full refuses the question before a model
// is called, a turn is opened or a grant is kept live.
//
// Mutation: drop the budget gate in Start and the model is called.
func TestABudgetRefusalStopsBeforeARound(t *testing.T) {
	t.Parallel()
	resets := evals.Epoch.Add(3 * time.Hour)
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("never")},
		Budget: evals.Budget{M: assistanttest.FullMeter{ResetsAt: resets}}})
	_, err := f.Ask("budget", "How much did we spend?", nil)
	refused, ok := errorAs[*assistant.BudgetRefused](err)
	if !ok || !refused.ResetsAt.Equal(resets) {
		t.Fatalf("err = %v, want a budget refusal naming when the window resets", err)
	}
	if n := len(f.Model.Requests()); n != 0 {
		t.Errorf("the model was asked %d rounds", n)
	}
	if records := f.Store.Records(); len(records) != 0 {
		t.Errorf("a refused question recorded %v", records)
	}
	if !f.Revoker.Revoked(f.Grant("budget").ID()) {
		t.Error("a refused question left its grant live")
	}
}

// ONE TURN RUNS PER CONVERSATION, AND THE SAME KEY IS THE SAME TURN.
//
// Mutation: drop the open-turn check and the second question starts beside the
// first.
func TestOneTurnRunsPerConversation(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("done")}})
	f.Model.Gate = make(chan struct{})
	first, err := f.Ask("first", "One?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Ask("second", "Two?", nil); !errors.Is(err, assistant.ErrTurnRunning) {
		t.Errorf("a second question while one runs: err = %v, want ErrTurnRunning", err)
	}
	again, err := f.Ask("first", "One?", nil)
	if err != nil || again.TurnID != first.TurnID {
		t.Errorf("a retry of the first question = %+v, %v; want the same turn", again, err)
	}
	close(f.Model.Gate)
	if done := evals.Last(f.Follow(first.TurnID, 0), wire.EventDone); done == nil ||
		done.Done.State != wire.TurnCompleted {
		t.Errorf("done = %+v", done)
	}
}

// A STOPPED TURN CHANGES NOTHING: nothing more is sent, it ends stopped, and
// its grant is ended.
//
// Mutation: ignore the Stop (drop the cancel in turn.stop) and the turn runs
// its tool call.
func TestAStoppedTurnChangesNothing(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task,
		Rounds: []evals.Round{readTask(),
			evals.Say("Two.")}})
	f.Model.Gate = make(chan struct{})
	accepted, err := f.Ask("stop", "Who works here?", nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.Svc.Stop(t.Context(), f.Person.ID, accepted.TurnID, evals.Key("stopping"))
	if err != nil || answer.State != wire.TurnStopped {
		t.Fatalf("stop = %+v, %v", answer, err)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("a stopped turn sent %+v", sent)
	}
	row, err := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if err != nil || row.State != wire.TurnStopped {
		t.Errorf("the turn is %s (%v), want stopped", row.State, err)
	}
	if !f.Revoker.Revoked(f.Grant("stop").ID()) {
		t.Error("a stopped turn left its grant live")
	}
}

// A GRANT ENDING MID-TURN ENDS THE TURN, as grant_ended: a 401 answered to the
// turn's own grant is the person's session ending, never a refusal of one
// read for the model to work around.
//
// Mutation: answer a 401 as an ordinary refusal and the model goes on.
func TestAGrantEndingMidTurnEndsTheTurn(t *testing.T) {
	t.Parallel()
	signedOut := func(exec.Request) exec.Response {
		return exec.Response{Status: http.StatusUnauthorized,
			Body: []byte(`{"error":"invalid_token","message":"Sign in again."}`)}
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: signedOut,
		Rounds: []evals.Round{readTask(),
			evals.Say("never")}})
	accepted, err := f.Ask("signout", "Who works here?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	failure := evals.Last(frames, wire.EventError)
	if failure == nil || failure.Error.Code != wire.ErrorGrantEnded {
		t.Fatalf("error = %+v, want grant_ended", failure)
	}
	if n := len(f.Model.Requests()); n != 1 {
		t.Errorf("the model was asked %d rounds after the grant ended", n)
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if row.State != wire.TurnFailed || row.ErrorCode != string(wire.ErrorGrantEnded) {
		t.Errorf("the turn closed %s (%s)", row.State, row.ErrorCode)
	}
}

// A GRANT ABOUT TO END SENDS NOTHING: the fence closes before the request,
// so no call rides a grant that lapses while the route serves it.
func TestAGrantAboutToEndSendsNothing(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task,
		Rounds: []evals.Round{readTask()}})
	short := f.Grant("short")
	short.ExpiresAt = f.Clock.Now().Add(assistant.GrantMargin / 2)
	accepted, err := f.Svc.Start(t.Context(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: evals.Key("short"), Input: "Who?", Read: short})
	if err != nil {
		t.Fatal(err)
	}
	failure := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventError)
	if failure == nil || failure.Error.Code != wire.ErrorGrantEnded {
		t.Fatalf("error = %+v, want grant_ended", failure)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("sent %+v on a grant about to end", sent)
	}
}

// A PERSON'S LOAD IS BOUNDED: two turns running at once, and a turn rate.
func TestAPersonsTurnsAreRateLimited(t *testing.T) {
	t.Parallel()
	var rounds []evals.Round
	for range assistant.TurnBurst {
		rounds = append(rounds, evals.Say("ok"))
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: rounds})
	for i := range assistant.TurnBurst {
		accepted, err := f.Ask("burst"+string(rune('a'+i)), "Hi", nil)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		f.Follow(accepted.TurnID, 0)
	}
	_, err := f.Ask("over", "Hi", nil)
	busy, ok := errorAs[*assistant.Busy](err)
	if !ok || busy.RetryAfter <= 0 {
		t.Fatalf("a turn past the burst: err = %v, want Busy with a wait", err)
	}
}

// AN IDENTITY ARGUMENT IS REFUSED BEFORE ANYTHING IS SENT (the catalogue's
// own check), and shows as a refused step.
func TestAnIdentityArgumentIsRefusedBeforeSending(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: []evals.Round{
			evals.Call("c1", "get_task", map[string]any{"item": "ENG-7", "actor": "ada"}),
			evals.Say("I cannot.")}})
	accepted, err := f.Ask("identity", "Act as Ada", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	step := evals.Last(frames, wire.EventStep)
	if step == nil || step.Step.State != wire.StepRefused || step.Step.Refusal == nil ||
		step.Step.Refusal.Code != "invalid_input" {
		t.Errorf("step = %+v, want refused invalid_input", step)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("sent %+v", sent)
	}
}

// AN OFF ASSISTANT AND A PRINCIPAL THAT IS NOT A PERSON ARE REFUSED, each with
// the reason the viewer gives, before anything else.
func TestTheAssistantRefusesWhoItMayNotServe(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Off: true})
	if _, err := f.Ask("off", "Hi", nil); !isReason(err, wire.ViewerDisabled) {
		t.Errorf("an assistant that is Off: err = %v", err)
	}
	g := assistanttest.NewRig(t, evals.RigOptions{})
	g.Person.Via = "pat:" + evals.ID("token").String()
	if _, err := g.Ask("token", "Hi", nil); !isReason(err, wire.ViewerNotAPerson) {
		t.Errorf("a machine token: err = %v", err)
	}
	h := assistanttest.NewRig(t, evals.RigOptions{Models: &evals.Models{Err: errors.New("no model")}})
	if _, err := h.Ask("model", "Hi", nil); !errors.Is(err, assistant.ErrUnavailable) {
		t.Errorf("no model: err = %v", err)
	}
}

func isReason(err error, reason wire.ViewerReason) bool {
	refused, ok := errorAs[*assistant.NotAvailable](err)
	return ok && refused.Reason == reason
}

// A RESUMED STREAM REPLAYS ONLY WHAT IT MISSED, and one whose frames are gone
// is answered from the transcript.
func TestAResumedStreamReplaysOnlyWhatItMissed(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task,
		Rounds: []evals.Round{readTask(),
			evals.Say("Two.")}})
	accepted, err := f.Ask("resume", "Who?", nil)
	if err != nil {
		t.Fatal(err)
	}
	whole := f.Follow(accepted.TurnID, 0)
	resumed := f.Follow(accepted.TurnID, 3)
	if len(resumed) != len(whole)-3 || resumed[0].Seq != 4 {
		t.Fatalf("resumed after 3: %d frames from seq %d, want %d from 4",
			len(resumed), resumed[0].Seq, len(whole)-3)
	}
	f.Clock.Advance(10 * time.Minute)
	gone := f.Follow(accepted.TurnID, 3)
	if len(gone) != 2 || gone[0].Kind != wire.EventSnapshot || gone[1].Kind != wire.EventDone ||
		gone[0].Seq != 4 {
		t.Fatalf("frames past retention = %v, want a snapshot at 4 then done", kinds(gone))
	}
	snap := gone[0].Snapshot
	if len(snap.Messages) != 2 || len(snap.Steps) != 1 || len(snap.Cards) != 1 {
		t.Errorf("snapshot = %d messages, %d steps, %d cards", len(snap.Messages),
			len(snap.Steps), len(snap.Cards))
	}
	for _, s := range snap.Steps {
		if s.TurnID != accepted.TurnID {
			t.Errorf("a snapshot step names turn %s", s.TurnID)
		}
	}
}

func kinds(frames []wire.Frame) []wire.EventKind {
	out := make([]wire.EventKind, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Kind)
	}
	return out
}
