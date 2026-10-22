package assistant_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// DEEP WORK (ADR-0054), driven end to end through the real orchestrator with
// a scripted model and a scripted deep-work backend: the run's every Crewlet
// call is executed by the bridge the engine would serve it, which is the
// surface handed to the backend.

// deepCompany is a company with deep work on.
func deepCompany(t *testing.T) assistant.Company {
	t.Helper()
	c, err := evals.DeepCompany("grace", "ada")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// deepRig is a rig offering the task read and the task create, on a company
// with deep work, whose runs play script.
func deepRig(t *testing.T, script *evals.DeepScript, rounds ...evals.Round) *assistanttest.Rig {
	t.Helper()
	return assistanttest.NewRig(t, evals.RigOptions{
		Company: deepCompany(t), Deep: script, Handler: task, Acts: []string{"create_work_item"},
		Tools:  []catalog.Tool{getTask(t), assistanttest.Tool(t, "create_task")},
		Rounds: rounds})
}

// startDeep is the model's escalation.
func startDeep(goal string) evals.Round {
	return evals.Call("c-deep", catalog.StartDeepWorkTool, map[string]any{"goal": goal,
		"plan": "Read the work, compare it with the teams, propose the changes."})
}

// readCall is a bridged read of ENG-7.
func readCall(id string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: "get_task", Arguments: map[string]any{"item": "ENG-7"}}
}

// steps are a stream's last frame of each step, by index.
func steps(frames []wire.Frame) map[int]wire.StepEvent {
	out := map[int]wire.StepEvent{}
	for _, f := range frames {
		if f.Kind == wire.EventStep && f.Step != nil {
			out[f.Step.Index] = *f.Step
		}
	}
	return out
}

// offered is what the model was offered in its first round, by name.
func offered(t *testing.T, f *assistanttest.Rig) []string {
	t.Helper()
	if len(f.Model.Requests()) == 0 {
		t.Fatal("the model was never asked")
	}
	var names []string
	for _, def := range f.Model.OfferedAt(0) {
		names = append(names, def.Name)
	}
	return names
}

// toolAnswer is the model's last reading of a call, by its id: the answer its
// CLI was given over the tool session, or, for a call a turn parked on, the
// answer the resumed run's conversation carries for it.
func toolAnswer(f *assistanttest.Rig, id string) (llm.Message, bool) {
	var out llm.Message
	found := false
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool && m.ToolCallID == id {
			out, found = m, true
		}
	}
	return out, found
}

// DEEP WORK IS OFFERED ONLY WHERE IT CAN RUN: a company that configures none,
// a node whose backend is not ready, and a node that keeps no files each leave
// the turn in quick mode, with no escalation tool and no word of it in the
// prompt. Mutation: offer the tool unconditionally and the first case fails.
func TestDeepWorkIsOfferedOnlyWhereItCanRun(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		company func(t *testing.T) assistant.Company
		script  *evals.DeepScript
		files   bool
		want    bool
	}{
		"no deep_work":    {func(t *testing.T) assistant.Company { return assistanttest.Company(t, true, "grace") }, evals.NewDeepScript(), true, false},
		"backend unready": {deepCompany, &evals.DeepScript{Unready: errors.New("no confinement")}, true, false},
		"ready":           {deepCompany, evals.NewDeepScript(), true, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{Company: c.company(t), Deep: c.script,
				Tools: []catalog.Tool{getTask(t)}, Rounds: []evals.Round{evals.Say("Hello.")}})
			accepted, err := f.Ask("offer", "What can you do?", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			got := slices.Contains(offered(t, f), catalog.StartDeepWorkTool)
			system := f.Model.Requests()[0].Messages[0].Content
			if got != c.want || strings.Contains(system, catalog.StartDeepWorkTool) != c.want {
				t.Fatalf("offered %v and prompted %v, want %v", got,
					strings.Contains(system, catalog.StartDeepWorkTool), c.want)
			}
		})
	}
}

// A RUN REACHES CREWLET ONLY THROUGH THE TURN'S OWN SURFACE: its read is one
// request through the dispatcher on the turn's grant, drawn as a step nested
// under the run's; the run's step is re-sent with its progress and closed with
// its outcome; its report reaches the model fenced as the run's own words, at
// the trust of text nobody in the company wrote (ADR-0054); its files are kept
// and carded; and its box is discarded. Mutation: execute the bridged call
// through anything but the turn's surface and the dispatcher sees no request
// on the turn's grant; fence the report at any trust but external and its
// label fails.
func TestADeepRunReachesCrewletOnlyThroughTheTurnsSurface(t *testing.T) {
	t.Parallel()
	script := evals.NewDeepScript(evals.DeepRunScript{
		Lines:  []string{"Reading the work of the last month", "[tool] Bash: python3 tally.py"},
		Calls:  []llm.ToolCall{readCall("b1")},
		Files:  []assistant.DeepFile{{Name: "teams.md", Content: []byte("# Proposed teams\n")}},
		Report: "Three teams cover the work; the report is in teams.md.", Input: 900, Output: 100})
	f := deepRig(t, script, startDeep("Redefine the teams"), evals.Say("Here is the analysis."))
	accepted, err := f.Ask("analyse", "Look at last month's work and propose new teams", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the turn ended %+v", done)
	}
	sent := f.Dispatcher.Sent()
	if len(sent) != 1 || sent[0].Request.Path != "/worklet/ENG-7" ||
		sent[0].Grant != f.Grant("analyse").GrantID {
		t.Fatalf("the run's read was sent as %+v, want one request on the turn's grant", sent)
	}
	got := steps(frames)
	deep, read := got[1], got[2]
	if deep.Tool != catalog.StartDeepWorkTool || deep.State != wire.StepDone || deep.Deep == nil ||
		deep.Deep.State != wire.DeepFinished || deep.Deep.Steps != 1 {
		t.Fatalf("the run's step closed as %+v / %+v", deep, deep.Deep)
	}
	if len(deep.Deep.Activity) != 0 {
		t.Errorf("the finished run's card still carries its activity %q; its report is the "+
			"answer's to say", deep.Deep.Activity)
	}
	if read.Tool != "get_task" || read.Parent != 1 || read.State != wire.StepDone {
		t.Fatalf("the run's read is %+v, want a done step nested under the run's", read)
	}
	running := false
	for _, f := range frames {
		if f.Kind == wire.EventStep && f.Step.Index == 1 && f.Step.Deep != nil &&
			f.Step.Deep.State == wire.DeepRunning &&
			slices.Contains(f.Step.Deep.Activity, "Ran a command") {
			running = true
		}
	}
	if !running {
		t.Error("the run's step was never re-sent with its progress")
	}
	card := evals.Last(frames, wire.EventCard)
	if card == nil || card.Card.Card.Type != wire.CardFile || len(card.Card.Card.File.Files) != 1 ||
		card.Card.Card.File.Files[0].Name != "teams.md" {
		t.Fatalf("the delivered file was carded as %+v", card)
	}
	answer, ok := toolAnswer(f, "c-deep")
	if !ok || !strings.Contains(answer.Content, "Three teams cover the work") ||
		!strings.Contains(answer.Content, " source=deep_work trust=external ") ||
		!strings.Contains(answer.Content, "teams.md") {
		t.Fatalf("the model read the run as %q", answer.Content)
	}
	if kept, discarded := script.Kept(), script.Discarded(); len(kept) != 0 || len(discarded) != 1 {
		t.Fatalf("boxes kept %v and discarded %v; a finished run's box is discarded", kept, discarded)
	}
	run := script.Runs()[0]
	if strings.Contains(run.System, "SENTINEL") || strings.Contains(run.Brief, "SENTINEL") {
		t.Fatal("the turn's grant reached the run's prompt")
	}
}

// A PROPOSAL PARKS THE RUN AND THE APPROVAL CONTINUES IT, in the same box and
// CLI session: the team redefinition. The run proposes a change and is told it
// waits; a further call is refused; the turn parks on the run's step with the
// box kept; the person's approval resumes the turn, which continues the run
// with the decision as its next message, and only the run's end answers the
// model. Mutation: answer the parked call with the decision (skip the
// continuation) and the second run never starts.
func TestAProposalParksTheRunAndTheApprovalContinuesIt(t *testing.T) {
	t.Parallel()
	script := evals.NewDeepScript(
		evals.DeepRunScript{Calls: []llm.ToolCall{readCall("b1"),
			{ID: "b2", Name: "create_task", Arguments: map[string]any{"title": "Form the platform team"}},
			readCall("b3")},
			Report: "Proposed the platform team; the roles are next."},
		evals.DeepRunScript{Calls: []llm.ToolCall{readCall("b4")},
			Report: "The platform team was created; nothing is left."})
	f := deepRig(t, script, startDeep("Redefine the teams"), evals.Say("The teams are redefined."))
	accepted, err := f.Ask("teams", "Redefine our teams from last month's work", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the proposing run's turn ended %+v, want parked", done)
	}
	answers := script.Answers()[0]
	if len(answers) != 3 || !strings.Contains(answers[1].Output, "waiting for their approval") &&
		!strings.Contains(answers[1].Output, "approval") || !answers[2].Failed {
		t.Fatalf("the run's calls were answered %+v: the proposal must say it waits and the "+
			"call after it must be refused", answers)
	}
	if kept := script.Kept(); len(kept) != 1 {
		t.Fatalf("the parked run's box was not kept: %v", kept)
	}
	got := steps(frames)
	if got[1].State != wire.StepWaiting || got[1].Deep.State != wire.DeepWaiting {
		t.Fatalf("the run's step parked as %+v", got[1])
	}
	proposals := f.Store.Proposals()
	if len(proposals) != 1 {
		t.Fatalf("%d proposals; the run's one change is one proposal", len(proposals))
	}

	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: proposals[0].ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Receipt: &wire.Receipt{Summary: "Created ENG-8."}, Card: cardAt(proposals[0], wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	after := f.Follow(accepted.TurnID, resumed.AfterSeq)
	if done := evals.Last(after, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the resumed turn ended %+v", done)
	}
	runs := script.Runs()
	if len(runs) != 2 || runs[1].Continue == nil || *runs[1].Continue != script.Kept()[0] ||
		!strings.Contains(runs[1].Brief, "approved the change") {
		t.Fatalf("the approval did not continue the run in its box with the decision: %+v", runs)
	}
	answer, ok := toolAnswer(f, "c-deep")
	if !ok || !strings.Contains(answer.Content, "nothing is left") {
		t.Fatalf("the model read %q for the parked call, want the continued run's end", answer.Content)
	}
	if steps(after)[1].Deep == nil || steps(after)[1].Deep.State != wire.DeepFinished {
		t.Fatalf("the continued run's step ended %+v", steps(after)[1])
	}
	if discarded := script.Discarded(); len(discarded) != 1 {
		t.Fatalf("the finished task's box was not discarded: %v", discarded)
	}
}

// A TASK'S END CARRIES THE ENGINE'S RECORD OF EVERY CHANGE IT PROPOSED: a task
// that proposes twice, each approved and applied, ends with a report that
// claims nothing it can prove, and the turn's model reads both receipts from
// the engine's own records beside it, the first kept across the second park.
// Mutation: answer the parked call with the run's end alone and the model
// reads neither receipt, which is how a person was told "I could not confirm
// that" about two changes the engine had applied.
func TestATasksEndCarriesTheRecordOfEveryChangeItProposed(t *testing.T) {
	t.Parallel()
	script := evals.NewDeepScript(
		evals.DeepRunScript{Calls: []llm.ToolCall{
			{ID: "d1", Name: "create_task", Arguments: map[string]any{"title": "Write the role page"}}},
			Report: "Proposed the role page."},
		evals.DeepRunScript{Calls: []llm.ToolCall{
			{ID: "d2", Name: "create_task", Arguments: map[string]any{"title": "Give QA its first task"}}},
			Report: "Proposed the QA task."},
		evals.DeepRunScript{Report: "Both changes may have been applied; I cannot confirm it."})
	f := deepRig(t, script, startDeep("Redefine the teams"), evals.Say("The teams are redefined."))
	accepted, err := f.Ask("teams", "Redefine our teams from last month's work", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	receipts := []string{"Created ENG-8.", "Created ENG-9."}
	for i, receipt := range receipts {
		proposals := f.Store.Proposals()
		if len(proposals) != i+1 {
			t.Fatalf("%d proposals after %d parks, want %d", len(proposals), i+1, i+1)
		}
		p := proposals[i]
		resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
			Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
			Read: f.Grant("approve"), State: wire.ProposalApplied,
			Receipt: &wire.Receipt{Summary: receipt}, Card: cardAt(p, wire.ProposalApplied)})
		if err != nil {
			t.Fatal(err)
		}
		want := wire.TurnWaitingForApproval
		if i == len(receipts)-1 {
			want = wire.TurnCompleted
		}
		if done := evals.Last(f.Follow(accepted.TurnID, resumed.AfterSeq), wire.EventDone); done == nil ||
			done.Done.State != want {
			t.Fatalf("resume %d ended %+v, want %s", i+1, done, want)
		}
	}
	answer, ok := lastToolAnswer(f, "c-deep")
	if !ok {
		t.Fatal("the model never read the task's end")
	}
	if !strings.Contains(answer.Content, "I cannot confirm it") {
		t.Fatalf("the model read %q, want the run's own report", answer.Content)
	}
	records := answer.Content[strings.Index(answer.Content, "I cannot confirm it"):]
	for _, want := range []string{"from the engine's own records", "1. Create task: The person " +
		"approved the change and it was applied. Created ENG-8.", "2. Create task: The person " +
		"approved the change and it was applied. Created ENG-9."} {
		if !strings.Contains(records, want) {
			t.Fatalf("the task's end, after the run's report, lacks %q:\n%s", want, records)
		}
	}
}

// lastToolAnswer is the model's latest reading of a call, by its id: a parked
// call is read again, with its newer answer, by every round after a resume.
func lastToolAnswer(f *assistanttest.Rig, id string) (llm.Message, bool) {
	var out llm.Message
	found := false
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool && m.ToolCallID == id {
			out, found = m, true
		}
	}
	return out, found
}

// A STOP KILLS THE RUN: the person's Stop ends the run's context, the turn ends
// stopped, a cancellation and never a failure, and the box is discarded.
func TestAStopKillsTheRun(t *testing.T) {
	t.Parallel()
	script := evals.NewDeepScript(evals.DeepRunScript{Hold: true})
	script.Started = make(chan string, 1)
	f := deepRig(t, script, startDeep("Redesign the brand"))
	accepted, err := f.Ask("stop", "Redesign the brand", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-script.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never started")
	}
	if _, err := f.Svc.Stop(context.Background(), f.Person.ID, accepted.TurnID, evals.Key("stop-1")); err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnStopped {
		t.Fatalf("the stopped turn ended %+v", done)
	}
	if steps(frames)[1].Deep.State != wire.DeepStopped {
		t.Fatalf("the run's step ended %+v", steps(frames)[1].Deep)
	}
	if len(script.Discarded()) != 1 {
		t.Fatal("the stopped run's box was not discarded")
	}
}

// A RUN'S CREWLET CALLS ARE CAPPED by its steps: the deep company's twenty,
// and the twenty-first is refused without being sent. Mutation: drop the cap
// and twenty-one requests are sent. Each call reads another task, since one
// read made twice in a run is answered from the first (the surface's memo).
func TestARunsCrewletCallsAreCapped(t *testing.T) {
	t.Parallel()
	var calls []llm.ToolCall
	tasks := map[string]exec.Response{}
	for i := range 21 {
		key := fmt.Sprintf("ENG-%d", i+1)
		call := readCall("b" + string(rune('a'+i)))
		call.Arguments = map[string]any{"item": key}
		calls = append(calls, call)
		tasks["GET /worklet/"+key] = assistanttest.Task(key, "Rotate keys", "Rotate the keys.")
	}
	script := evals.NewDeepScript(evals.DeepRunScript{Calls: calls, Report: "Read it all."})
	f := assistanttest.NewRig(t, evals.RigOptions{
		Company: deepCompany(t), Deep: script, Handler: assistanttest.Routes(tasks),
		Acts:   []string{"create_work_item"},
		Tools:  []catalog.Tool{getTask(t), assistanttest.Tool(t, "create_task")},
		Rounds: []evals.Round{startDeep("Read everything"), evals.Say("Done.")}})
	accepted, err := f.Ask("cap", "Read everything", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	if sent := len(f.Dispatcher.Sent()); sent != 20 {
		t.Fatalf("%d requests sent, want the cap of 20", sent)
	}
	answers := script.Answers()[0]
	if last := answers[len(answers)-1]; !last.Failed || !strings.Contains(last.Output, "every Crewlet call") {
		t.Fatalf("the call past the cap was answered %+v", last)
	}
}

// A RUN NEVER OUTLIVES THE TURN'S GRANT: a turn whose grant has too little
// left refuses to start one, rather than start a run whose calls would meet a
// grant that ended. Mutation: ignore the grant's expiry and the run starts.
func TestARunNeverOutlivesTheTurnsGrant(t *testing.T) {
	t.Parallel()
	script := evals.NewDeepScript(evals.DeepRunScript{Report: "x"})
	f := deepRig(t, script, startDeep("Redefine the teams"), evals.Say("I could not start it."))
	short := f.Grant("short")
	short.ExpiresAt = f.Clock.Now().Add(80 * time.Second)
	accepted, err := f.AskWith("short", "Redefine the teams", nil, short)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if len(script.Runs()) != 0 {
		t.Fatal("a run started on a grant with less than a minute of it left")
	}
	if step := steps(frames)[1]; step.State != wire.StepRefused || step.Refusal == nil {
		t.Fatalf("the refused run's step is %+v", step)
	}
}

// meter counts what a turn charged it.
type meter struct {
	mu    sync.Mutex
	spent int
}

func (m *meter) Spend(_ context.Context, n int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spent += n
	return toolloop.SpendOutcome{OK: true}, nil
}

func (m *meter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	return toolloop.SpendOutcome{}, false
}

// A RUN'S TOKENS ARE THE TURN'S: charged to the meter the turn's rounds are,
// once, when the run ends. Mutation: skip the charge and the meter holds only
// the model's rounds.
func TestARunsTokensAreChargedToTheTurn(t *testing.T) {
	t.Parallel()
	m := &meter{}
	script := evals.NewDeepScript(evals.DeepRunScript{Report: "Done.", Input: 4000, Output: 1000})
	f := assistanttest.NewRig(t, evals.RigOptions{Company: deepCompany(t), Deep: script,
		Budget: evals.Budget{M: m}, Tools: []catalog.Tool{getTask(t)},
		Rounds: []evals.Round{startDeep("Analyse"), evals.Say("Done.")}})
	accepted, err := f.Ask("tokens", "Analyse the work", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	// evals.Call and evals.Say each report their own tokens: 110 and 120.
	if want := 5000 + 110 + 120; m.spent != want {
		t.Fatalf("the meter was charged %d, want the run's 5000 and the rounds' 230", m.spent)
	}
}

// THE BRAND REDESIGN: the person attaches a screenshot on the brand screen,
// whose open draft the run reads through the page context in its brief; the
// screenshot is in its inputs; it delivers a logo (and an HTML page, which is
// refused); and the assistant proposes the logo into the draft by the file id
// the card showed, a proposal the person approves. Mutation: drop the page
// from the brief and the run never sees the draft; keep an HTML file and the
// card lists it.
func TestTheBrandRedesignDeliversAssetsAndProposesThemIntoTheDraft(t *testing.T) {
	t.Parallel()
	logo := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`)
	script := evals.NewDeepScript(evals.DeepRunScript{
		Files: []assistant.DeepFile{{Name: "logo.svg", Content: logo},
			{Name: "preview.html", Content: []byte("<html><script>x()</script></html>")}},
		Report: "logo.svg is the new primary logo."})
	files := evals.NewFiles(evals.NewClock().Now)
	key := evals.Key("brand")
	turn := assistant.TurnID(key)
	logoID := assistant.DeliveredFileID(turn, "assistant-"+turn.String()+"-1-0", "logo.svg")
	f := assistanttest.NewRig(t, evals.RigOptions{Company: deepCompany(t), Deep: script, Files: files,
		Tools: []catalog.Tool{assistanttest.Tool(t, "propose_page_change")},
		Rounds: []evals.Round{startDeep("Redesign the brand assets"),
			evals.Call("c-draft", "propose_page_change", map[string]any{"action": catalog.DraftSet,
				"args": map[string]any{"values": map[string]any{"logos.primary": logoID.String()}}}),
			evals.Say("The new logo is ready for your approval.")}})
	shot, err := files.Attach(f.Person.ID, f.Conversation, "screenshot.png",
		append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := f.Svc.Start(context.Background(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: key, Input: "Redesign my brand assets",
		Page: draftPage(), Read: f.Grant("brand"), Attachments: []uuid.UUID{shot.FileID}})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	transcript, err := f.Store.Transcript(t.Context(), f.Person.ID, f.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	if asked := transcript.Messages[0]; len(asked.Attachments) != 1 || asked.Attachments[0].FileID != shot.FileID {
		t.Fatalf("the question was recorded with attachments %+v", asked.Attachments)
	}
	run := script.Runs()[0]
	if !strings.Contains(run.Brief, "logos.primary") || !strings.Contains(run.Brief, "screenshot.png") {
		t.Fatalf("the run's brief does not carry the draft and the attachment:\n%s", run.Brief)
	}
	if len(run.Inputs) != 1 || run.Inputs[0].Name != "screenshot.png" {
		t.Fatalf("the run's inputs are %+v", run.Inputs)
	}
	var card *wire.FileCard
	for _, fr := range frames {
		if fr.Kind == wire.EventCard && fr.Card.Card.Type == wire.CardFile {
			card = fr.Card.Card.File
		}
	}
	if card == nil || len(card.Files) != 1 || card.Files[0].FileID != logoID ||
		card.Files[0].MediaType != "image/svg+xml" {
		t.Fatalf("the delivered files are %+v; the logo alone is kept, under its derived id %s (run %s)",
			card, logoID, run.ID)
	}
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the turn ended %+v, want parked on the draft change", done)
	}
	proposals := f.Store.Proposals()
	if len(proposals) != 1 || proposals[0].Tool != "propose_page_change" ||
		!strings.Contains(string(proposals[0].Body), logoID.String()) ||
		!strings.Contains(string(proposals[0].Body), f.Conversation.String()) {
		t.Fatalf("the logo was proposed as %+v, want its reference resolved in this "+
			"conversation (%s)", proposals, f.Conversation)
	}
}

// A RUN'S OWN DRAFT CHANGE NAMES A FILE BY ITS ID ALONE TOO: the bridge reaches
// the turn's surface, so a file the person attached in this conversation is
// resolved into the whole reference the page takes, and one of another
// conversation is refused before any card. Mutation: resolve files only for
// the quick-mode model and the run's change carries a bare id.
func TestARunsDraftChangeResolvesItsFileOnTheBridge(t *testing.T) {
	t.Parallel()
	set := func(id, file string) llm.ToolCall {
		return llm.ToolCall{ID: id, Name: "propose_page_change", Arguments: map[string]any{
			"action": catalog.DraftSet, "args": map[string]any{"values": map[string]any{
				"logos.primary": file}}}}
	}
	for name, c := range map[string]struct {
		file     func(shot uuid.UUID) string
		proposed bool
	}{
		"a file of this conversation": {file: func(shot uuid.UUID) string { return shot.String() },
			proposed: true},
		"a file nobody holds here": {file: func(uuid.UUID) string { return uuid.NewString() }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := evals.NewFiles(evals.NewClock().Now)
			shot, err := files.Attach(grace, conversation, "logo.png",
				append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
			if err != nil {
				t.Fatal(err)
			}
			script := evals.NewDeepScript(evals.DeepRunScript{Report: "Proposed the logo.",
				Calls: []llm.ToolCall{set("p1", c.file(shot.FileID))}})
			f := assistanttest.NewRig(t, evals.RigOptions{Company: deepCompany(t), Deep: script,
				Files: files, Tools: []catalog.Tool{assistanttest.Tool(t, "propose_page_change")},
				Rounds: []evals.Round{startDeep("Set the logo"), evals.Say("Done.")}})
			if f.Person.ID != grace || f.Conversation != conversation {
				t.Fatalf("the rig's person or conversation moved: %v %v", f.Person.ID, f.Conversation)
			}
			accepted, err := f.Ask("logo", "Use the logo I attached", draftPage())
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			proposals := f.Store.Proposals()
			if c.proposed != (len(proposals) == 1) {
				t.Fatalf("recorded %d proposals, want proposed %v", len(proposals), c.proposed)
			}
			if c.proposed && !strings.Contains(string(proposals[0].Body), f.Conversation.String()) {
				t.Errorf("the run's change carries %s, not the resolved reference",
					proposals[0].Body)
			}
		})
	}
}

// A CONTINUED RUN WORKS ON THE SCREEN AS THE DECISION LEFT IT: a run that
// proposed a draft change is continued after the person applied it, told the
// screen as it is now, and its next draft change is proposed through the
// bridge, not refused as an action the screen does not accept. Mutation:
// resume the turn with no page and the run's second change is refused.
func TestAContinuedRunWorksOnTheScreenAsTheDecisionLeftIt(t *testing.T) {
	t.Parallel()
	set := func(id, path, value string) llm.ToolCall {
		return llm.ToolCall{ID: id, Name: "propose_page_change", Arguments: map[string]any{
			"action": catalog.DraftSet, "args": map[string]any{"values": map[string]any{path: value}}}}
	}
	script := evals.NewDeepScript(
		evals.DeepRunScript{Calls: []llm.ToolCall{set("p1", "voice.summary", "Plain and warm.")},
			Report: "Proposed the voice."},
		evals.DeepRunScript{Calls: []llm.ToolCall{set("p2", "name", "FairSplit")},
			Report: "Proposed the name."})
	f := assistanttest.NewRig(t, evals.RigOptions{
		Company: deepCompany(t), Deep: script,
		Tools:  []catalog.Tool{assistanttest.Tool(t, "propose_page_change")},
		Rounds: []evals.Round{startDeep("Redesign my brand"), evals.Say("Done.")}})
	page := draftPage()
	page.Draft.Fields[3].Editable = true
	accepted, err := f.Ask("brand", "Redesign my brand from the draft", page)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	first := f.Store.Proposals()[0]
	now := draftPage()
	now.Draft.Fields[3].Editable = true
	now.SourceRevision = "brand-setup@2"
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: first.ID,
		Read: f.Grant("report"), State: wire.ProposalApplied, ClientReported: true,
		Receipt: &wire.Receipt{Summary: "Applied to Brand › Set up"},
		Card:    cardAt(first, wire.ProposalApplied), Page: now})
	if err != nil {
		t.Fatal(err)
	}
	after := f.Follow(accepted.TurnID, resumed.AfterSeq)
	if done := evals.Last(after, wire.EventDone); done == nil ||
		done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the continued run's turn ended %+v, want parked on its second change", done)
	}
	if n := len(f.Store.Proposals()); n != 2 {
		t.Fatalf("%d proposals, want the continued run's second change proposed", n)
	}
	runs := script.Runs()
	if len(runs) != 2 || !strings.Contains(runs[1].Brief, "screen now") ||
		!strings.Contains(runs[1].Brief, "brand-setup@2") {
		t.Fatalf("the continued run was not told the screen as it is now: %+v", runs)
	}
}

// NO HOST PATH OR RAW TOOL LINE REACHES A PERSON from deep work: a run whose
// transcript names its box's paths, its CLI's tools and Crewlet's MCP tools,
// and whose report says where it wrote a file, shows the person plain lines
// (no "[tool]", no absolute path, no Crewlet call, which its nested steps
// already list), and hands the model a report with every path cut to the file's
// name. Mutation: keep the transcript verbatim, or fence the report as the run
// wrote it, and this fails.
func TestNoHostPathReachesAPersonFromDeepWork(t *testing.T) {
	t.Parallel()
	box := "/private/tmp/claude-501/review-engine/data/assistant-boxes/7f3a/outbox"
	script := evals.NewDeepScript(evals.DeepRunScript{
		Lines: []string{"[tool] mcp__crewlet__list_artifacts", "[tool] Write: " + box + "/report.md",
			"[tool] ToolSearch: select:Read,Write", "I saved the draft in " + box + "/report.md.",
			"[tool] Bash: rsvg-convert logo.svg -o logo.png → error: not found"},
		Files:  []assistant.DeepFile{{Name: "report.md", Content: []byte("# Report\n")}},
		Report: "I wrote the full report to " + box + "/report.md.", Input: 900, Output: 100})
	f := deepRig(t, script, startDeep("Review the brand"), evals.Say("Done."))
	accepted, err := f.Ask("brand", "Review our brand", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var shown []string
	for _, fr := range frames {
		if fr.Kind == wire.EventStep && fr.Step.Deep != nil {
			shown = append(shown, fr.Step.Deep.Activity...)
			shown = append(shown, fr.Step.Deep.Reason)
		}
	}
	if len(shown) == 0 {
		t.Fatal("the run showed no activity at all")
	}
	for _, line := range shown {
		for _, internal := range []string{"[tool]", "/private/", "assistant-boxes", "mcp__",
			"ToolSearch", "select:"} {
			if strings.Contains(line, internal) {
				t.Errorf("a person reads %q, which carries %q", line, internal)
			}
		}
	}
	for _, want := range []string{"Wrote report.md", "I saved the draft in report.md.",
		"Ran a command, which failed", "Looked up a tool"} {
		if !slices.Contains(shown, want) {
			t.Errorf("the activity %q does not say %q", shown, want)
		}
	}
	answer, ok := toolAnswer(f, "c-deep")
	if !ok || strings.Contains(answer.Content, "/private/") ||
		!strings.Contains(answer.Content, "I wrote the full report to report.md.") {
		t.Errorf("the model read the run as %q", answer.Content)
	}
}
