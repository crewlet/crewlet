package assistant_test

import (
	"context"
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

// auditWait is how long a suite waits for a turn's audit record, which the
// runner publishes right after the turn's done frame: far past any delay a
// loaded race runner puts between the two, and reached only by a turn that
// never publishes one.
const auditWait = 10 * time.Second

// getTask is the catalogue's own task read, whose answer other people wrote.
func getTask(t *testing.T) catalog.Tool { return assistanttest.Tool(t, "get_task") }

// task answers ENG-7.
var task = assistanttest.Routes(map[string]exec.Response{
	"GET /worklet/ENG-7": assistanttest.Task("ENG-7", "Rotate keys", "Rotate the signing keys."),
})

// readTask is a round asking for ENG-7.
func readTask() evals.Round {
	return evals.Call("c1", "get_task", map[string]any{"item": "ENG-7"})
}

// A READ IS ONE REQUEST, THROUGH THE DISPATCHER, ON THE TURN'S GRANT.
//
// The assistant's every reach into the company is an ordinary request the
// API's handler chain decides as the person (ADR-0050). So a scripted turn
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
	if got := sent[0]; got.Request.Method != http.MethodGet || got.Request.Path != "/worklet/ENG-7" ||
		got.Grant != f.Grant("read").ID() {
		t.Errorf("sent %+v on grant %s, want GET /worklet/ENG-7 on the question's grant", got.Request, got.Grant)
	}
	done := evals.Last(frames, wire.EventDone)
	if done == nil || done.Done.State != wire.TurnCompleted || done.Done.Steps != 1 {
		t.Fatalf("done = %+v, want a completed turn of one step", done)
	}
	var tool llm.Message
	for _, msg := range f.Model.Seen() {
		if msg.Role == llm.RoleTool {
			tool = msg
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
		"GET /worklet/ENG-7": assistanttest.Task("ENG-7", "Rotate keys", "token "+key),
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
	published, ok := f.Audit.Await(1, auditWait)
	if !ok {
		t.Fatal("the turn published no audit record")
	}
	audit, _ := json.Marshal(published)
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

// A READ OF THE ASSISTANT'S OWN RECORDS THAT FAILED IS THE STORE'S OWN ERROR,
// never the assistant's Unavailable: the HTTP surface tells it as the
// condition of the node or the fault it is (internal/api/assistantapi). Told
// as Unavailable, every such failure carried a wait of two seconds, so a full
// log was asked again every two seconds and a broken store was said to be
// coming back.
//
// Mutation: answer readFailure's store error as an Unavailable again and the
// transcript's read answers unavailable.
func TestAFailedReadOfItsRecordsIsTheStoresOwnError(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{})
	f.Store.FailConversations(1)
	_, err := f.Svc.Transcript(t.Context(), f.Person, f.Conversation)
	if err == nil || errors.Is(err, assistant.ErrUnavailable) ||
		!strings.Contains(err.Error(), "the store is away") {
		t.Errorf("a transcript the store could not read answered %v, want the "+
			"store's own error", err)
	}
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
	// The question, the empty answer the silent first round's card hangs
	// on, and the second round's prose below it.
	if len(snap.Messages) != 3 || len(snap.Steps) != 1 || len(snap.Cards) != 1 {
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

// A STOP ANSWERS WHAT THE TURN TRULY CAME TO: a runner that has not recorded
// its end within the Stop's wait is still running, and the Stop says so
// rather than claiming a stop the record does not hold.
//
// Mutation: answer stopped unconditionally (as before) and the Stop reports a
// state no record says.
func TestAStopAnswersTheStateTheTurnTrulyReached(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Hi.")}})
	f.Model.Gate = make(chan struct{})
	accepted, err := f.Ask("stuck", "Hello?", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The close never lands within the wait: the runner retries it behind
	// the Stop's answer.
	f.Store.FailCloses(1 << 20)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	answer, err := f.Svc.Stop(ctx, f.Person.ID, accepted.TurnID, evals.Key("stopping"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if answer.State != row.State {
		t.Errorf("the Stop answered %s while the record says %s", answer.State, row.State)
	}
}

// A TURN THAT HAS TOLD ITS END HOLDS NO RUNNING SLOT: a person who reads done
// and asks again at once is answered, even while the runners before are still
// tearing down (their audit records unwritten).
//
// Mutation: give the slot back only when the runner returns (as before) and
// the third question is refused assistant_busy behind two finished turns.
func TestAFinishedTurnHoldsNoRunningSlot(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Say("One."), evals.Say("Two."), evals.Say("Three.")}})
	f.Store.Seed(f.Person.ID, f.Conversation, "Titled")
	release := make(chan struct{})
	defer close(release)
	f.Audit.Hold(release)
	for i, key := range []string{"first", "second", "third"} {
		accepted, err := f.Ask(key, "Question "+key, nil)
		if err != nil {
			t.Fatalf("question %d after %d finished turns: %v", i+1, i, err)
		}
		if done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone); done == nil {
			t.Fatalf("question %d never ended", i+1)
		}
	}
}

// A SEARCH THAT FINDS NOTHING DRAWS NO CARD: its step ends "No matches" and
// shows the request it made (`GET /query/knowledge`, never the pattern
// `/query/{what}`), no card frame is sent, and the model reads that the
// search came back empty with the phrase it asked. The turn's audit record
// keeps the pattern alone. Mutation: card the empty answer again, show the
// route's pattern on the step, or audit the sent path, and this fails.
func TestASearchThatFindsNothingDrawsNoCard(t *testing.T) {
	t.Parallel()
	empty := assistanttest.Routes(map[string]exec.Response{
		"GET /query/knowledge": {Status: http.StatusOK, Body: []byte(
			`{"available":true,"backend":"native","hits":[],"query":"mission"}`)},
	})
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools:   []catalog.Tool{assistanttest.Tool(t, "search_knowledge")},
		Handler: empty, Rounds: []evals.Round{
			evals.Call("c1", "search_knowledge", map[string]any{"q": "mission"}),
			evals.Say("Nothing in the knowledge base mentions the mission.")}})
	accepted, err := f.Ask("empty", "Find the mission", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	step := evals.Last(frames, wire.EventStep)
	if step == nil || step.Step.State != wire.StepDone || step.Step.Summary != "No matches" ||
		step.Step.Request == nil || step.Step.Request.Route != "/query/knowledge" {
		t.Errorf("step = %+v, want done, No matches, on /query/knowledge", step)
	}
	if card := evals.Last(frames, wire.EventCard); card != nil {
		t.Errorf("an empty search drew a card: %+v", card.Card)
	}
	// THE AUDIT KEEPS THE PATTERN: the company-wide trail names the route the
	// step matched, never a path value the person's own step shows.
	published, ok := f.Audit.Await(1, auditWait)
	if !ok {
		t.Fatal("the turn published no audit record")
	}
	audit, _ := json.Marshal(published)
	if !strings.Contains(string(audit), `/query/{what}`) ||
		strings.Contains(string(audit), `/query/knowledge`) {
		t.Errorf("the turn's audit records %s, want the route pattern alone", audit)
	}
	var tool llm.Message
	for _, msg := range f.Model.Seen() {
		if msg.Role == llm.RoleTool {
			tool = msg
		}
	}
	if !strings.Contains(tool.Content, `\"empty\":true`) || !strings.Contains(tool.Content, "mission") {
		t.Errorf("the model read the empty search as %q", tool.Content)
	}
}
