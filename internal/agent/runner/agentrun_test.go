package runner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// recordingLauncher stands in for the engine's detached-run seam.
type recordingLauncher struct {
	req  runner.AgentRunRequest
	runs int
	err  error
}

func (l *recordingLauncher) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) error {
	l.runs++
	l.req = req
	return l.err
}

// agentFixture is the shared runner with an agent-mode executor.
func agentFixture(t *testing.T, launcher runner.AgentLauncher, resume *runner.Resume) *runner.Runner {
	t.Helper()
	prov := &scriptedProvider{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{agentRun: launcher, resume: resume})
	return r
}

// AN AGENT-MODE EXECUTOR SPENDS NO MODEL CALL OF ITS OWN.
//
// This is the whole shape: the CLI's loop drives the model, so the engine's
// job is to hand it a brief and stop. A branch that fell through to the native
// loop would run the phase twice — once here on the engine's tokens and once
// in the box on the subscription's — and only the second would have the tools.
func TestAnAgentModeExecutorLaunchesAndSuspends(t *testing.T) {
	t.Parallel()
	launcher := &recordingLauncher{}
	r := agentFixture(t, launcher, nil)

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Suspended {
		t.Fatal("an agent-mode executor did not suspend, so nothing will collect its run")
	}
	if launcher.runs != 1 {
		t.Fatalf("launched %d runs, want exactly 1", launcher.runs)
	}
	if launcher.req.Surface == nil {
		t.Fatal("the run was launched with no tool surface, so the bridge has nothing to dispatch against")
	}
	// The SEAT'S OWN PROMPT, not a second one written for the CLI: the
	// brief has to carry the identity and the ask, or the box gets a task
	// with no idea whose it is.
	if !strings.Contains(launcher.req.Brief, "post the weekly summary") {
		t.Errorf("the brief does not carry the turn's ask: %q", launcher.req.Brief)
	}
	if !strings.Contains(launcher.req.Brief, "CTO") {
		t.Errorf("the brief does not carry the seat's identity: %q", launcher.req.Brief)
	}
	// The SUBMISSION is on the surface, because that is how the CLI ends
	// its run — over the bridge, exactly as a native loop ends locally.
	// Read off the tool DEFINITIONS, which is what the bridge serves to a
	// tools/list: a submission the surface holds but does not publish is
	// one the CLI can never call.
	published := false
	for _, def := range launcher.req.Surface.ToolDefs() {
		if def.Name == runner.SubmitWorkTool {
			published = true
		}
	}
	if !published {
		t.Error("the bridged surface does not publish submit_work, so the run cannot say what it did")
	}
}

// A SUSPENSION THE ENGINE CAN PERSIST, and one that says which shape it is.
//
// The flag is what the resume reads, and it has to be on the STATE rather than
// re-derived from config: a run parked for days comes back to whatever the
// company was applied to since, and an agent run collected as a native one
// rebuilds a conversation that never existed.
func TestAnAgentRunSuspensionCarriesWhatAResumeNeeds(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &recordingLauncher{}, nil)
	if _, _, err := r.Execute(context.Background(), 2, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, ok := r.Suspended()
	if !ok {
		t.Fatal("no suspension was recorded, so the engine has nothing to write")
	}
	if !got.State.AgentRun {
		t.Error("the state does not say it is an agent run")
	}
	if len(got.State.Messages) > 0 {
		t.Error("an agent run recorded an engine conversation it does not have")
	}
	if got.State.Round != 2 {
		t.Errorf("round = %d, want the round it suspended in", got.State.Round)
	}
	if got.State.Task != "post the weekly summary" {
		t.Errorf("task = %q", got.State.Task)
	}
	if err := got.State.Validate(); err != nil {
		t.Errorf("the recorded state is invalid, so nothing would persist it: %v", err)
	}
}

// A LAUNCH THAT FAILS FAILS THE PHASE, rather than falling through to the
// native loop.
//
// A seat configured for agent mode whose run cannot start has a configuration
// problem — no sandbox, no bridge URL, no runner for its CLI — and quietly
// running the executor a different way hides it behind a turn that merely
// looks expensive, on a model the operator did not choose for the work.
func TestALaunchFailureFailsThePhase(t *testing.T) {
	t.Parallel()
	launcher := &recordingLauncher{err: errors.New("no bridge url")}
	r := agentFixture(t, launcher, nil)

	_, _, err := r.Execute(context.Background(), 1, "", nil)
	if err == nil {
		t.Fatal("a failed launch produced a phase, so the executor silently ran somewhere else")
	}
	if !strings.Contains(err.Error(), "no bridge url") {
		t.Errorf("the error loses the cause: %v", err)
	}
	if _, ok := r.Suspended(); ok {
		t.Error("a failed launch recorded a suspension, so the engine would open a row nothing can complete")
	}
}

// THE RUN'S OWN SUBMISSION IS THE TURN'S ANSWER, replayed from the durable
// bridged-call log rather than from memory: the process collecting the run may
// not be the one that launched it.
func TestAResumedAgentRunReportsWhatTheRunSubmitted(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &recordingLauncher{}, &runner.Resume{
		State:  execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
		Answer: "cloned the repo, fixed the failing test, opened the merge request",
		Bridged: []ledger.Call{
			{Name: "slack_post", Result: "posted"},
			{Name: runner.SubmitWorkTool, Args: map[string]any{
				"outcome":    "delivered",
				"summary":    "fixed the failing test and opened the MR",
				"deliveries": []any{"slack_post"},
			}},
		},
	})

	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeDelivered {
		t.Fatalf("outcome = %q, want the one the run submitted", w.Outcome)
	}
	if w.Rescued {
		t.Error("a run that submitted was rescued, so the reviewer judges a turn that in fact delivered")
	}
	if w.Summary != "fixed the failing test and opened the MR" {
		t.Errorf("summary = %q", w.Summary)
	}
	// THE RUN'S CALLS, not this process's surface's — the delivery check,
	// the citations and the ledger all read them, and a fresh surface has
	// executed nothing.
	if len(w.Calls) != 2 || w.Calls[0].Name != "slack_post" {
		t.Errorf("calls = %+v, want the run's own log", w.Calls)
	}
}

// AN ABSENT SUBMISSION IS NOT A VALUE — the same rule a native pass follows.
//
// A run that stopped without saying what it did is rescued as incomplete and
// judged on its record. Reading the prose it happened to end with as a
// delivery would put a claim in its mouth on the one question that matters.
func TestAnAgentRunThatNeverSubmittedIsRescued(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &recordingLauncher{}, &runner.Resume{
		State:   execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
		Answer:  "I had a look around and then ran out of turns",
		Bridged: []ledger.Call{{Name: "slack_history", Result: "read"}},
	})

	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeIncomplete {
		t.Errorf("outcome = %q, want the engine's own word", w.Outcome)
	}
	if !w.Rescued {
		t.Error("the work is not marked rescued, so a fast path could act on a claim nobody made")
	}
	if w.Text != "I had a look around and then ran out of turns" {
		t.Errorf("the rescue lost the run's own text: %q", w.Text)
	}
	if w.Summary != "" {
		t.Errorf("a rescue wrote an intent the run never gave: %q", w.Summary)
	}
}

// A SECOND SUBMISSION IS A CORRECTION, which is the submission contract
// everywhere else and has to hold here too: the replay is in call order, so
// the last write wins.
func TestTheLastSubmissionAnAgentRunMadeWins(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &recordingLauncher{}, &runner.Resume{
		State:  execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
		Answer: "done",
		Bridged: []ledger.Call{
			{Name: "slack_post", Result: "posted"},
			{Name: runner.SubmitWorkTool, Args: map[string]any{
				"outcome": "blocked", "summary": "could not reach the repo",
			}},
			{Name: runner.SubmitWorkTool, Args: map[string]any{
				"outcome": "delivered", "summary": "posted after all",
				"deliveries": []any{"slack_post"},
			}},
		},
	})

	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeDelivered || w.Summary != "posted after all" {
		t.Errorf("outcome = %q, summary = %q — the correction did not win", w.Outcome, w.Summary)
	}
}

// A MALFORMED SUBMISSION IS THE "SAID NOTHING" CASE, not a failed resume.
//
// The run happened and its other calls happened; losing the whole turn over a
// call the decoder rejects would throw away work that was really done.
func TestAMalformedSubmissionRescuesRatherThanFailing(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &recordingLauncher{}, &runner.Resume{
		State:  execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
		Answer: "did some things",
		Bridged: []ledger.Call{
			{Name: "slack_post", Result: "posted"},
			{Name: runner.SubmitWorkTool, Args: map[string]any{"outcome": "teleported"}},
		},
	})

	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeIncomplete || !w.Rescued {
		t.Errorf("outcome = %q rescued = %v, want the rescue path", w.Outcome, w.Rescued)
	}
}

// THE STATE DECIDES WHICH RESUME RUNS, not the config.
//
// A native suspension resumed on a runner configured for agent mode must still
// re-enter its conversation: the two are different shapes, and picking by
// config would rebuild whichever one the company happens to say today.
func TestANativeSuspensionResumesNativelyEvenInAgentMode(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, runner.SubmitWorkTool, `{
			"outcome":"delivered","summary":"posted it","deliveries":["slack_post"]}`),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		agentRun: &recordingLauncher{},
		resume: &runner.Resume{
			State: execstate.State{
				Version:         execstate.Version,
				Messages:        suspendedConversation(),
				PendingCallID:   "call-1",
				PendingCallName: "run_sandbox",
				Round:           1,
				// The pre-suspend rounds' calls, which is what makes a
				// delivery made before the suspend citable after it.
				ToolExecutions: []map[string]any{
					{"name": "slack_post", "success": true, "result": "posted"},
				},
			},
			Answer: "the sandbox finished",
		},
	})

	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeDelivered {
		t.Fatalf("outcome = %q — the native conversation was not re-entered", w.Outcome)
	}
	if len(prov.requestsFor("execute")) == 0 {
		t.Error("no model call was made, so the native loop did not run")
	}
}

// suspendedConversation is the minimum a native suspension needs: one
// assistant turn whose tool call is still unanswered.
func suspendedConversation() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "you are the CTO"},
		{Role: llm.RoleUser, Content: "post the weekly summary"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "run_sandbox", Arguments: map[string]any{"brief": "fix it"}},
		}},
	}
}

// THE PROMPT REACHES THE RECORD, published at the launch.
//
// Nothing else will: a native pass publishes it from inside the tool loop,
// which agent mode does not enter, and the resume's own record deliberately
// carries none (it re-entered a phase rather than opening one). Without this
// an operator reading an agent-mode turn on the dashboard sees a run that
// finished and no sign of what it was asked — on the one runtime where the
// prompt is the whole of the engine's instruction to the agent.
func TestAnAgentRunPublishesThePromptItWasLaunchedWith(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{agentRun: &recordingLauncher{}, pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	prompts := pub.promptsFor("execute")
	if len(prompts) == 0 {
		t.Fatal("an agent-mode launch published no prompt, so the turn's record has no ask in it")
	}
	if !strings.Contains(prompts[0], "post the weekly summary") {
		t.Errorf("the published prompt does not carry the ask: %q", prompts[0])
	}
}

// AN AGENT-MODE RUN'S EVENT SAYS WHAT IT DID, and which box it did it in.
//
// The resume used to hand the record builder a zero result while holding the
// whole account in the bridged log, so the published event carried no
// response, no tool calls, no rounds and no model — and stamped `native` on a
// run that had not run here. The card rendered EMPTY: an empty ledger and an
// empty response leave no transcript to fall back on, and the "composing its
// first round" placeholder only shows on a LIVE phase. An operator saw a
// decision word and a blank card for a run that made real tool calls.
func TestAResumedAgentRunPublishesWhatTheRunDid(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		agentRun: &recordingLauncher{},
		pub:      pub,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
			Answer: "cloned the repo, fixed the failing test, opened the merge request",
			Bridged: []ledger.Call{
				{Name: "slack_post", Result: "posted"},
				{Name: runner.SubmitWorkTool, Args: map[string]any{
					"outcome":    "delivered",
					"summary":    "fixed the failing test and opened the MR",
					"deliveries": []any{"slack_post"},
				}},
			},
			Run: runner.RunRecord{
				CodingAgent: "claude-code", SandboxID: "box-7",
				CostUSD: 0.42, DeliveredRefs: []string{"https://example.com/pr/9"},
			},
		},
	})

	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.Response == "" {
		t.Error("the run's own account is missing, so the card has nothing to render")
	}
	if len(done.ToolExecutions) != 2 {
		t.Errorf("tool_executions = %+v, want the run's own two calls", done.ToolExecutions)
	}
	// ONE ROUND, and every call on it — the engine made one request, the
	// launch, and got one answer. Splitting the log into a round per call
	// would claim a structure nobody observed.
	if done.RoundsUsed != 1 {
		t.Errorf("rounds_used = %d, want the one round the engine actually drove", done.RoundsUsed)
	}
	for _, ex := range done.ToolExecutions {
		if round, _ := ex["round"].(int); round != 1 {
			t.Errorf("%v is on round %d, want 1", ex["name"], round)
		}
	}
	if len(done.RoundNarration) != 1 {
		t.Fatalf("round_narration = %+v, want the run's answer on its own round", done.RoundNarration)
	}
	if done.RoundNarration[0]["content"] == "" {
		t.Error("the round carries no content, so its tool rows have nothing above them")
	}
	// AND WHICH BOX. `native` on a detached coding run is what made a
	// twenty-minute remote job and three local rounds indistinguishable.
	if done.Backend != types.BackendSandbox {
		t.Errorf("backend = %q, want sandbox", done.Backend)
	}
	if done.CodingAgent != "claude-code" || done.SandboxID != "box-7" {
		t.Errorf("the box is unnamed: agent=%q id=%q", done.CodingAgent, done.SandboxID)
	}
	if done.CostUSD != 0.42 {
		t.Errorf("cost_usd = %v — a subscription CLI's spend is reported nowhere else", done.CostUSD)
	}
	if len(done.DeliveredRefs) != 1 {
		t.Errorf("delivered_refs = %v, want what the run produced", done.DeliveredRefs)
	}
}

// A NATIVE PHASE IS STILL NATIVE. The backend is a fact about where a phase
// ran, not a flag the resume path sets for everything it touches.
func TestAPhaseThatRanHereReportsTheNativeBackend(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, runner.SubmitWorkTool, `{"outcome":"no_action","summary":"nothing to do"}`),
		text("done"),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{reply: turn.ReplyNone, pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	done := completedPhase(t, pub, "execute")
	if done.Backend != types.BackendNative {
		t.Errorf("backend = %q, want native", done.Backend)
	}
	if done.CodingAgent != "" || done.SandboxID != "" {
		t.Errorf("a native phase named a box: agent=%q id=%q", done.CodingAgent, done.SandboxID)
	}
}
