package runner_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// agentFixture is the shared runner over launcher, collecting resume when it
// is set.
func agentFixture(t *testing.T, launcher runner.AgentLauncher, resume *runner.Resume) *runner.Runner {
	t.Helper()
	prov := &scriptedProvider{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{launcher: launcher, resume: resume})
	return r
}

// THE EXECUTOR SPENDS NO MODEL CALL OF ITS OWN.
//
// This is the whole shape: the harness's loop drives the model, so the engine's
// job is to hand it a brief and park. A launch that also called a model here
// would run the phase twice — once on the engine's tokens and once in the box
// on the subscription's — and only the second would have the tools.
func TestTheExecutorLaunchesItsRunAndParks(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	prov := &scriptedProvider{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{launcher: launcher})

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Suspended {
		t.Fatal("the executor did not park, so nothing will collect its run")
	}
	if launcher.launched() != 1 {
		t.Fatalf("launched %d runs, want exactly 1", launcher.launched())
	}
	if n := len(prov.allRuns()); n != 0 {
		t.Errorf("the launch made %d model calls of its own, want none", n)
	}
	req := launcher.last(t)
	if req.Surface == nil {
		t.Fatal("the run was launched with no tool surface, so the bridge has nothing to dispatch against")
	}
	// The SEAT'S OWN PROMPT: the run has to carry the identity and the ask,
	// or the box gets a task with no idea whose it is.
	if !strings.Contains(req.Brief, "post the weekly summary") {
		t.Errorf("the brief does not carry the turn's ask: %q", req.Brief)
	}
	if !strings.Contains(req.System, "CTO") {
		t.Errorf("the system prompt does not carry the seat's identity: %q", req.System)
	}
	// The SUBMISSION is on the surface, because that is how the harness ends
	// its run — over the bridge. Read off the tool DEFINITIONS, which is
	// what the bridge serves to a tools/list: a submission the surface holds
	// but does not publish is one the harness can never call.
	if !slices.Contains(toolNames(req.Surface.ToolDefs()), runner.SubmitWorkTool) {
		t.Error("the bridged surface does not publish submit_work, so the run cannot say what it did")
	}
}

// THE PROMPT TRAVELS IN TWO HALVES: the seat's standing instructions as system
// text, the turn's ask as the brief.
//
// The harness appends the first to its own system prompt and runs the
// second. Joined into one brief, as they once were, the seat's identity,
// policies and memory reached the model as the opening of the user's request,
// and the harness's system prompt said nothing of whose work it was doing.
func TestTheExecutorsPromptTravelsInItsTwoHalves(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	r := agentFixture(t, launcher, nil)
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := launcher.last(t)
	if !strings.Contains(req.System, "## Your turn") || !strings.Contains(req.System, "CTO") {
		t.Errorf("the system half is not the seat's contract and identity:\n%s", req.System)
	}
	if strings.Contains(req.Brief, "CTO") || strings.Contains(req.Brief, "## Your turn") {
		t.Errorf("the seat's standing instructions ride the prompt the harness runs:\n%s", req.Brief)
	}
	if strings.Contains(req.System, "post the weekly summary") {
		t.Errorf("the turn's ask rides the system half:\n%s", req.System)
	}
}

// THE COLLECTION REACHES ONLY WHAT THE LAUNCH DID.
//
// A resume rebuilds the executor's surface from the registry, so it is the
// second place the narrowing has to hold: built from the whole registry, the
// collected surface could reach mark_onboarded — and a citation replayed at
// collection would be judged against a surface the run never had.
func TestACollectedRunReachesOnlyWhatItsLaunchDid(t *testing.T) {
	t.Parallel()
	marker := func(t *testing.T, reg *tools.Registry) {
		if err := reg.Register(stubTool{name: toolname.MarkOnboarded, out: "ok"}, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	got := executeRound(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{register: marker}, 1, "", nil,
		run{calls: []llm.ToolCall{{ID: "s", Name: "slack_post"}}, text: "done"}, runner.RunRecord{})
	if slices.Contains(got.surface.Catalogue, toolname.MarkOnboarded) {
		t.Error("the collected executor can reach mark_onboarded")
	}
	if slices.Contains(toolNames(got.launch.Surface.ToolDefs()), toolname.MarkOnboarded) {
		t.Error("the launched run was offered mark_onboarded")
	}
}

// A PARK THE ENGINE CAN PERSIST, carrying what the collection needs.
func TestAParkedRunCarriesWhatItsCollectionNeeds(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &playLauncher{}, nil)
	if _, _, err := r.Execute(context.Background(), 2, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, ok := r.Suspended()
	if !ok {
		t.Fatal("no state was recorded, so the engine has nothing to write")
	}
	if got.State.Round != 2 {
		t.Errorf("round = %d, want the round it was launched in", got.State.Round)
	}
	if got.State.Task != "post the weekly summary" {
		t.Errorf("task = %q", got.State.Task)
	}
	if !slices.Contains(got.State.ActiveTools, runner.SubmitWorkTool) {
		t.Errorf("active tools = %v, want the surface the run was offered", got.State.ActiveTools)
	}
	if err := got.State.Validate(); err != nil {
		t.Errorf("the recorded state is invalid, so nothing would persist it: %v", err)
	}
}

// A PARK IS COLLECTED FROM WHAT THE ENGINE PERSISTED, not from the value the
// launching runner held: the process that collects a run reads the state back
// off the wire, so a field that does not survive its encoding is one the
// collection silently loses. Launched, encoded, decoded, and collected by a
// fresh runner, the run's delivery is the turn's.
func TestAParkedRunIsCollectedFromItsPersistedForm(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, deliver(t, "posted the weekly summary"))

	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state")
	}
	blob, err := execstate.Encode(parked.State)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	state, found, err := execstate.Decode(blob)
	if err != nil || !found {
		t.Fatalf("Decode: %v (found %v)", err, found)
	}
	if !slices.Equal(state.ActiveTools, parked.State.ActiveTools) {
		t.Errorf("active tools %v came back as %v", parked.State.ActiveTools, state.ActiveTools)
	}

	collector, _ := buildWith(t, entries, buildOpts{
		resume: &runner.Resume{State: state, Answer: "posted it", Bridged: bridged},
	})
	w, _, err := collector.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeDelivered || w.Rescued {
		t.Errorf("outcome = %q rescued = %v, want the delivery the run submitted", w.Outcome, w.Rescued)
	}
	if !slices.Equal(w.Deliveries, []string{"slack_post"}) {
		t.Errorf("deliveries = %v, want the call the run cited", w.Deliveries)
	}
}

// A LAUNCH THAT FAILS FAILS THE PHASE, and CLOSES it.
//
// There is no other way to run the executor, so the error is the phase's. And
// the phase's started record was published before the launch — it carries the
// prompt the run was sent — so a launch that never happened publishes the
// phase's completion too: failed, with the error. Left open, the phase reads
// as running for ever on every screen that draws it.
//
// Mutation: drop the completion on the error path, and the record is gone.
func TestALaunchFailureFailsAndClosesThePhase(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	launcher := &playLauncher{err: errors.New("no bridge url")}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, pub: pub})

	work, _, err := r.Execute(context.Background(), 1, "", nil)
	if err == nil {
		t.Fatal("a failed launch produced a phase")
	}
	if !strings.Contains(err.Error(), "no bridge url") {
		t.Errorf("the error loses the cause: %v", err)
	}
	if _, ok := r.Suspended(); ok {
		t.Error("a failed launch recorded a state, so the engine would open a row nothing can complete")
	}
	// The counterfactual of a broken phase that had acted: nothing was
	// called, so the turn reads as having done nothing and keeps its retry.
	if len(work.Calls) != 0 {
		t.Errorf("a launch that never ran reports calls: %+v", work.Calls)
	}
	done := completedPhase(t, pub, "execute")
	if !done.Failed || !strings.Contains(done.Error, "no bridge url") {
		t.Errorf("failed = %v error = %q, want the launch failure on the record", done.Failed, done.Error)
	}
	if done.SystemPrompt == "" {
		t.Error("the failed launch's record lost the prompt it would have run with")
	}
}

// A LAUNCH A PERSON STOPPED DID NOT FAIL. The phase closes carrying the stop
// as its error and not marked failed, exactly as a phase the engine drives
// does: a turn is listed as failed when any of its rows is, and a turn
// somebody deliberately ended is not one that broke.
func TestALaunchAPersonStoppedClosesThePhaseWithoutFailingIt(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	launcher := &playLauncher{err: fmt.Errorf("the seat's fence: %w", turn.ErrStoppedByPerson)}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, pub: pub})

	_, _, err := r.Execute(context.Background(), 1, "", nil)
	if !turn.Stopped(err) {
		t.Fatalf("Execute = %v, want the stop", err)
	}
	done := completedPhase(t, pub, "execute")
	if done.Failed {
		t.Error("a stopped launch was recorded as a failure")
	}
	if done.Error == "" {
		t.Error("the record does not say why the phase ended")
	}
}

// A CLOSED FENCE LAUNCHES NOTHING. A run outlives the segment that launched it
// and is not fenced once it runs, so the launch is the last moment a seat that
// moved node, or one a person stopped, can be kept from starting one — and
// nothing is opened, published or parked first.
//
// Mutation: check the fence after the launch, and a run starts on a seat this
// node no longer holds.
func TestAClosedFenceLaunchesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fence error
	}{
		{"the seat moved", errors.New("seat cto is held by node-b")},
		{"a person stopped it", turn.ErrStoppedByPerson},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pub := newCapture()
			launcher := &playLauncher{}
			r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
				buildOpts{launcher: launcher, pub: pub, fence: func() error { return tc.fence }})

			_, _, err := r.Execute(context.Background(), 1, "", nil)
			if !errors.Is(err, tc.fence) {
				t.Fatalf("Execute = %v, want the fence's own answer", err)
			}
			if launcher.launched() != 0 {
				t.Errorf("launched %d runs past a closed fence", launcher.launched())
			}
			if started := pub.startedPhases(); len(started) != 0 {
				t.Errorf("published %v for a phase that never opened", started)
			}
			if _, ok := r.Suspended(); ok {
				t.Error("a launch that never happened recorded a state")
			}
		})
	}
}

// THE LAUNCH IS HANDED THE TURN'S OWN FENCE, to ask again before it opens
// anything: the prompt built since the phase opened can take a model call of
// its own. The same closure the runner was built with — a fence compares the
// grant it captured when it was built with the one held when it is called, so
// the launch is held to the turn's admission only by this one.
func TestTheLaunchIsHandedTheTurnsOwnFence(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	asked := 0
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, fence: func() error { asked++; return nil }})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := launcher.last(t)
	if req.Fence == nil {
		t.Fatal("the launch was handed no fence")
	}
	before := asked
	if err := req.Fence(); err != nil || asked != before+1 {
		t.Fatalf("the launch's fence = %v and asked the turn's %d time(s), want the turn's own",
			err, asked-before)
	}
}

// THE EXECUTOR LAUNCHES NOTHING ON A REFUSAL ALREADY CERTAIN.
//
// Its calls are made in a box and post-charged when the run is collected, so
// no round of an engine loop is there to stop them: a run launched on a
// window already past its ceiling is paid for whole, and every token of it is
// over the cap. Asked before the launch, the executor is refused as an engine
// phase is before its first round — and opens nothing.
func TestTheExecutorLaunchesNothingOnAStandingRefusal(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, pub: pub, budget: &ceilingMeter{
			countingMeter: countingMeter{seen: []int{120}}, ceiling: 100,
		}})

	_, _, err := r.Execute(context.Background(), 1, "", nil)
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Execute = %v, want the refusal the meter holds", err)
	}
	if launcher.launched() != 0 {
		t.Errorf("launched %d runs on a refusal that already stood", launcher.launched())
	}
	if started := pub.startedPhases(); len(started) != 0 {
		t.Errorf("published %v for a phase that never opened", started)
	}
	if _, ok := r.Suspended(); ok {
		t.Error("a launch that never happened recorded a state")
	}
}

// chargingModels is the seat's auxiliary seam as the engine's is for a turn:
// every rewrite it makes is charged to the turn's meter, here the very meter
// the runner judges its calls by.
type chargingModels struct {
	meter  toolloop.BudgetMeter
	tokens int
}

func (m chargingModels) Auxiliary(*org.Role, auxspend.Use) (chain.Member, error) {
	return chain.Member{Key: "cheap", Provider: chargingRewriter(m)}, nil
}

type chargingRewriter chargingModels

func (chargingRewriter) Model() string { return "cheap" }

func (r chargingRewriter) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	if _, err := r.meter.Spend(ctx, r.tokens); err != nil {
		return nil, err
	}
	return &llm.Completion{Content: "condensed: the earlier round drafted the summary"}, nil
}

// THE EXECUTOR LAUNCHES NOTHING WHEN ITS OWN BRIEF FILLS THE WINDOW.
//
// The phase asks the meter before it builds anything, and the answer is room;
// building the brief then rewrites a prior round's long output with the seat's
// auxiliary model, charged to the same counters, and that rewrite takes the
// window past its ceiling. A run launched now is paid for whole past it, so the
// executor asks again once the brief exists and is refused.
func TestTheExecutorLaunchesNothingWhenItsBriefFillsTheWindow(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	meter := &ceilingMeter{ceiling: 100}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, budget: meter,
			compact: compact.New(chargingModels{meter: meter, tokens: 150}, nil).
				For(nil, auxspend.Use{Stage: types.AuxStageTurn})})
	history := []ledger.Iteration{{Text: strings.Repeat("drafting the weekly summary. ", 400)}}

	_, _, err := r.Execute(context.Background(), 2, "", history)
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Execute = %v, want the refusal the brief's own rewrite made certain", err)
	}
	if got := meter.charges(); len(got) != 1 || got[0] != 150 {
		t.Fatalf("charges = %v, want the one rewrite this case is about", got)
	}
	if launcher.launched() != 0 {
		t.Errorf("launched %d runs on a window the brief had already filled", launcher.launched())
	}
	if _, ok := r.Suspended(); ok {
		t.Error("a launch that never happened recorded a suspension")
	}
}

// THE RUN'S OWN SUBMISSION IS THE TURN'S ANSWER, replayed from the durable
// bridged-call log rather than from memory: the process collecting the run may
// not be the one that launched it.
func TestAResumedAgentRunReportsWhatTheRunSubmitted(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &playLauncher{}, &runner.Resume{
		State:  execstate.State{Version: execstate.Version, Round: 1},
		Answer: "cloned the repo, fixed the failing test, opened the merge request",
		Bridged: []runner.BridgedCall{
			{Call: ledger.Call{Name: "slack_post", Result: "posted"}},
			{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
				"outcome":    "delivered",
				"summary":    "fixed the failing test and opened the MR",
				"deliveries": []any{"slack_post"},
			}}},
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

// AN ABSENT SUBMISSION IS NOT A VALUE.
//
// A run that stopped without saying what it did is rescued as incomplete and
// judged on its record. Reading the prose it happened to end with as a
// delivery would put a claim in its mouth on the one question that matters.
func TestAnAgentRunThatNeverSubmittedIsRescued(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &playLauncher{}, &runner.Resume{
		State:   execstate.State{Version: execstate.Version, Round: 1},
		Answer:  "I had a look around and then ran out of turns",
		Bridged: []runner.BridgedCall{{Call: ledger.Call{Name: "slack_history", Result: "read"}}},
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
	r := agentFixture(t, &playLauncher{}, &runner.Resume{
		State:  execstate.State{Version: execstate.Version, Round: 1},
		Answer: "done",
		Bridged: []runner.BridgedCall{
			{Call: ledger.Call{Name: "slack_post", Result: "posted"}},
			{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
				"outcome": "blocked", "summary": "could not reach the repo",
			}}},
			{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
				"outcome": "delivered", "summary": "posted after all",
				"deliveries": []any{"slack_post"},
			}}},
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

// THE CORRECTION IS THE CALL MADE LAST, NOT THE ONE THAT FINISHED LAST. The
// row holds a run's calls in the order they finished — each is appended once
// it returns — and a harness that makes calls at once finishes them in whatever
// order their tools take. So the collection reads them in the order the bridge
// admitted them, and states each with when it ran, for how long and who
// answered, as the phase record of any other phase does.
//
// Mutation: replay and record the row's own order, and the draft the model
// replaced is the turn's answer; drop the timing from the executions, and the
// phase card cannot place the run's calls on a timeline.
func TestARunIsRecordedInTheOrderItMadeItsCalls(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	started := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	draft := map[string]any{"outcome": "delivered", "summary": "posted the first draft",
		"deliveries": []any{"slack_post"}}
	correction := map[string]any{"outcome": "delivered", "summary": "posted after all",
		"deliveries": []any{"slack_post"}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1},
			Answer: "done",
			// AS THE ROW HOLDS THEM: the correction returned first.
			Bridged: []runner.BridgedCall{
				{Call: ledger.Call{Name: "slack_post", Result: "posted"}, Seq: 1,
					StartedAt: started, Duration: 1500 * time.Millisecond,
					Origin: tools.OriginMCPPrefix + "slack", Server: "slack"},
				{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: correction, Result: "submitted"},
					Seq: 3, StartedAt: started.Add(3 * time.Second), Duration: 10 * time.Millisecond,
					Origin: tools.OriginBuiltin},
				{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: draft, Result: "submitted"},
					Seq: 2, StartedAt: started.Add(2 * time.Second), Duration: 2 * time.Second,
					Origin: tools.OriginBuiltin},
			},
		},
	})

	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Outcome != turn.OutcomeDelivered || w.Summary != "posted after all" {
		t.Errorf("outcome = %q, summary = %q: the draft made before the correction won "+
			"because it finished after it", w.Outcome, w.Summary)
	}
	if len(w.Calls) != 3 || w.Calls[2].Args["summary"] != "posted after all" {
		t.Errorf("calls = %+v, want the run's calls in the order it made them", w.Calls)
	}

	done := completedPhase(t, pub, "execute")
	if len(done.ToolExecutions) != 3 {
		t.Fatalf("tool_executions = %+v, want the run's three calls", done.ToolExecutions)
	}
	post, draftRow := done.ToolExecutions[0], done.ToolExecutions[1]
	if post["name"] != "slack_post" || post["server"] != "slack" ||
		post["origin"] != tools.OriginMCPPrefix+"slack" {
		t.Errorf("the first execution is %v, want slack_post answered by the slack server", post)
	}
	if post["started_at"] != started.Format(time.RFC3339Nano) || post["duration_ms"] != 1500 {
		t.Errorf("the first execution is timed %v for %v, want %s for 1500ms",
			post["started_at"], post["duration_ms"], started.Format(time.RFC3339Nano))
	}
	if !strings.Contains(fmt.Sprint(draftRow["arguments"]), "first draft") ||
		draftRow["duration_ms"] != 2000 {
		t.Errorf("the second execution is %v, want the draft the run made second", draftRow)
	}
}

// A MALFORMED SUBMISSION IS THE "SAID NOTHING" CASE, not a failed resume.
//
// The run happened and its other calls happened; losing the whole turn over a
// call the decoder rejects would throw away work that was really done.
func TestAMalformedSubmissionRescuesRatherThanFailing(t *testing.T) {
	t.Parallel()
	r := agentFixture(t, &playLauncher{}, &runner.Resume{
		State:  execstate.State{Version: execstate.Version, Round: 1},
		Answer: "did some things",
		Bridged: []runner.BridgedCall{
			{Call: ledger.Call{Name: "slack_post", Result: "posted"}},
			{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{"outcome": "teleported"}}},
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

// THE PROMPT REACHES THE RECORD, published at the launch.
//
// Nothing else will: the collection's own record deliberately carries none (it
// collects a run rather than opening a conversation). Without this an operator
// reading a turn on the dashboard sees a run that finished and no sign of what
// it was asked — and the prompt is the whole of the engine's instruction to
// the agent.
func TestAnAgentRunPublishesThePromptItWasLaunchedWith(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	prompts := pub.promptsFor("execute")
	if len(prompts) == 0 {
		t.Fatal("a launch published no prompt, so the turn's record has no ask in it")
	}
	if !strings.Contains(prompts[0], "post the weekly summary") {
		t.Errorf("the published prompt does not carry the ask: %q", prompts[0])
	}
}

// THE EXECUTOR'S PROMPT IS MEASURED WITH ITS TOOL ARRAY, although neither
// half of the prompt contains it.
//
// The launcher is handed the system and user text and the surface separately:
// the definitions reach the harness over the MCP bridge, not in the prompt.
// The harness's own model is billed for every one of them all the same, so a
// meter that counted only the text would report the engine's most tool-heavy
// phase as its slimmest — and the argument for leaving them out ("they are not
// in the prompt") is exactly the one a reader will make when deleting the
// term, which is why the case is here.
func TestAnAgentRunMeasuresTheToolsItAdvertisedOverTheBridge(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := pub.sizes()
	if len(got) != 1 {
		t.Fatalf("published %d prompt.size events for one launch, want 1", len(got))
	}
	m := got[0]
	defs := launcher.last(t).Surface.ToolDefs()
	if len(defs) == 0 {
		t.Fatal("the launcher was handed no tools, so this case cannot tell a fix from a bug")
	}
	if m.ToolCount != len(defs) || m.ToolBytes != toolArrayBytes(t, defs) {
		t.Errorf("measured %d tools at %d chars, the run was advertised %d at %d",
			m.ToolCount, m.ToolBytes, len(defs), toolArrayBytes(t, defs))
	}
	// And both halves of the prompt itself.
	if m.SystemBytes == 0 || m.UserBytes == 0 {
		t.Errorf("measured %d/%d system/user bytes for a launch that sends both",
			m.SystemBytes, m.UserBytes)
	}
}

// A RUN'S EVENT SAYS WHAT IT DID, which box it did it in, and which member of
// the seat's executor chain ran it.
//
// The resume used to hand the record builder a zero result while holding the
// whole account in the bridged log, so the published event carried no
// response, no tool calls, no rounds and no model — and stamped `native` on a
// run that had not run here. The card rendered EMPTY: an empty ledger and an
// empty response leave no transcript to fall back on, and the "composing its
// first round" placeholder only shows on a LIVE phase. An operator saw a
// decision word and a blank card for a run that made real tool calls.
//
// THE MEMBER IS THE LAUNCH'S, not the chain's head: a launch that fell back
// to the second entry ran on it, and a record naming the head — or nobody —
// charges the run's spend to an entry that never served it.
func TestAResumedAgentRunPublishesWhatTheRunDid(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1},
			Answer: "cloned the repo, fixed the failing test, opened the merge request",
			Bridged: []runner.BridgedCall{
				{Call: ledger.Call{Name: "slack_post", Result: "posted"}},
				{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
					"outcome":    "delivered",
					"summary":    "fixed the failing test and opened the MR",
					"deliveries": []any{"slack_post"},
				}}},
			},
			Run: runner.RunRecord{
				Harness: "claude-code", SandboxID: "box-7",
				ProviderKey: "fallback-plan", Model: "claude-sonnet-5",
				DeliveredRefs: []string{"https://example.com/pr/9"},
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
	// AND WHICH BOX: the harness that did the work and the box it ran in, which
	// only the run knows and nothing on the resuming side could re-derive.
	if done.Harness != "claude-code" || done.SandboxID != "box-7" {
		t.Errorf("the box is unnamed: agent=%q id=%q", done.Harness, done.SandboxID)
	}
	if len(done.DeliveredRefs) != 1 {
		t.Errorf("delivered_refs = %v, want what the run produced", done.DeliveredRefs)
	}
	if done.ProviderKey != "fallback-plan" || done.Model != "claude-sonnet-5" {
		t.Errorf("provider_key = %q model = %q, want the member the launch picked",
			done.ProviderKey, done.Model)
	}
}

// A COLLECTED RUN LISTS EVERY TOOL IT HELD, AND NOTHING LEFT TO DISCOVER. The
// round's record states what the run held (tools_available): every tool its
// launch recorded, which is every tool its seat holds, MCP servers' included.
// It used to list beside that what the run could still have discovered
// (tool_catalogue), and an executor has nothing left to discover — its harness
// keeps a large server's tools out of its requests by itself — so the record
// carries no such list rather than one a reader would take for tools the run
// never had.
//
// Mutation: put a catalogue back on the executor's record, or rebuild the
// collection's surface without the launch's record, and this goes red.
func TestACollectedRunListsEveryToolItHeld(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	held := []string{"jira_create", "lookup_colleague", "reflect", "slack_history", "slack_post"}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1, ActiveTools: held},
			Answer: "filed the flaky test",
			Bridged: []runner.BridgedCall{
				{Call: ledger.Call{Name: "jira_create", Args: map[string]any{"summary": "flaky test"},
					Result: "created"}, Seq: 1},
				{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
					"outcome": "delivered", "summary": "filed the flaky test",
					"deliveries": []any{"jira_create"},
				}}, Seq: 2},
			},
		},
	})

	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	for _, tool := range held {
		if !slices.Contains(done.ToolsAvailable, tool) {
			t.Errorf("tools_available = %v, want every tool the launch recorded, %s among them",
				done.ToolsAvailable, tool)
		}
	}
	if len(done.ToolCatalogue) != 0 {
		t.Errorf("tool_catalogue = %v, on a run that held every tool it could reach",
			done.ToolCatalogue)
	}
}

// A PHASE THAT RAN HERE NAMES NO BOX. The harness and the box are facts about the
// run an executor collected, not something the collection stamps on every
// record it touches: the reviewer runs in this process's own loop.
func TestAPhaseThatRanHereNamesNoBox(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{review: []llmtest.Response{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	done := completedPhase(t, pub, "review")
	if done.Harness != "" || done.SandboxID != "" {
		t.Errorf("a phase that ran here named a box: agent=%q id=%q", done.Harness, done.SandboxID)
	}
}
