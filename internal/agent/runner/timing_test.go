package runner_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// WHAT A PHASE RECORD SAYS ABOUT ITS TIME AND ITS CACHE, on the wire.
//
// The tool loop measures every round and every call; these hold the runner to
// carrying those measurements onto the events a dashboard reads — folded
// across an extension exactly as the rounds themselves are, since a timeline
// whose numbers are on a different scale from the ledger beside it is a second
// ledger that disagrees with the first. The loop's cases run on the onboarding
// pass, the one engine-driven phase an extension reaches.

// cached is a round that also reports a prompt-cache share.
func cached(r llmtest.Response, read, write int) llmtest.Response {
	r.Usage.CacheRead, r.Usage.CacheWrite = read, write
	return r
}

func TestAPhaseRecordCarriesItsRoundsCallsAndCache(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		cached(persist(t, "a", "start"), 50, 10),
		cached(persist(t, "b", "again"), 55, 0),
		cached(markDone(t, "enough"), 58, 0),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, alwaysExtend{})

	before := time.Now().UTC()
	onboard(t, r)
	after := time.Now().UTC()

	done := completedPhase(t, pub, "onboarding")
	if len(done.Rounds) != 3 {
		t.Fatalf("the record carries %d timed rounds, want one per provider call (3): %+v",
			len(done.Rounds), done.Rounds)
	}
	for i, round := range done.Rounds {
		// ON THE LEDGER'S SCALE across the extension: rounds 1 and 2 ran
		// in the first invocation, round 3 in the second.
		if round.Round != i+1 {
			t.Errorf("timed round %d is numbered %d — not the scale its calls are on", i, round.Round)
		}
		if round.StartedAt.Before(before) || round.StartedAt.After(after) {
			t.Errorf("round %d started at %v, outside the phase", round.Round, round.StartedAt)
		}
		if round.InputTokens != 60 || round.OutputTokens != 40 || round.ToolCalls != 1 {
			t.Errorf("round %d = %+v, want its own completion's counts", round.Round, round)
		}
	}
	if done.CacheReadTokens != 163 || done.CacheWriteTokens != 10 {
		t.Errorf("cache = %d read / %d write, want the phase's 163 / 10 across the extension",
			done.CacheReadTokens, done.CacheWriteTokens)
	}
	if done.StartedAt.Before(before) || done.StartedAt.After(after) {
		t.Errorf("the phase started at %v, outside [%v, %v]", done.StartedAt, before, after)
	}
	// Every call is timed and attributed on the wire row.
	for _, ex := range done.ToolExecutions {
		at, _ := ex["started_at"].(string)
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			t.Errorf("%v carries started_at %q: %v", ex["name"], at, err)
		}
		if _, ok := ex["duration_ms"].(int); !ok {
			t.Errorf("%v carries no duration_ms: %v", ex["name"], ex)
		}
		if ex["origin"] != tools.OriginBuiltin {
			t.Errorf("%v names origin %v, want the builtin that answered it", ex["name"], ex["origin"])
		}
		if _, ok := ex["server"]; ok {
			t.Errorf("%v names a server although a builtin answered it", ex["name"])
		}
	}
}

func TestAnExtensionKeepsRoundNumbersAndMovesTheCap(t *testing.T) {
	t.Parallel()
	// A two-round cap, extended by the step (4) under a ceiling of 8: the
	// record states the cap the phase ENDED under, not the one it began
	// with, and the ceiling it could have reached.
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		persist(t, "a", "one"),
		persist(t, "b", "two"),
		markDone(t, "three"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, spendingJudge{})

	onboard(t, r)
	done := completedPhase(t, pub, "onboarding")
	if done.MaxRounds != 6 || done.RoundCeiling != 8 {
		t.Errorf("max_rounds %d / round_ceiling %d, want 6 (2 + the granted 4) / 8",
			done.MaxRounds, done.RoundCeiling)
	}
	// The judge sits AFTER the round the phase ran out on.
	judged := phasesOfKind(t, pub, "judge")
	if len(judged) != 1 {
		t.Fatalf("%d judge phases, want 1", len(judged))
	}
	if judged[0].HostRound != 2 {
		t.Errorf("the judge's host round = %d, want 2 — the round the phase ran out on", judged[0].HostRound)
	}
	if judged[0].StartedAt.IsZero() || judged[0].StartedAt.Before(done.Rounds[1].StartedAt) {
		t.Errorf("the judge started at %v, want after round 2 began (%v)",
			judged[0].StartedAt, done.Rounds[1].StartedAt)
	}
}

func TestProgressCarriesTheCurrentCapAndCeiling(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{onboarding: []llmtest.Response{
		persist(t, "a", "one"),
		persist(t, "b", "two"),
		markDone(t, "three"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, alwaysExtend{})
	onboard(t, r)

	// The opening frame already states the cap, before the model has
	// answered once.
	if open := openingFrame(t, pub, "onboarding"); open.MaxRounds != 2 || open.RoundCeiling != 8 {
		t.Errorf("the opening frame states max %d / ceiling %d, want 2 / 8", open.MaxRounds, open.RoundCeiling)
	}
	frames := progressFrames(t, pub, "onboarding")
	var running []string
	announced := map[string]bool{}
	cap := 0
	for _, frame := range frames {
		round := frame.RoundNum + 1
		// THE CAP IN FORCE WHEN THE FRAME WAS PUBLISHED: the budget's 2
		// until the judge grants more, the 6 the extension moved it to
		// after — never back, and never 2 on a round past the budget.
		if frame.MaxRounds < cap {
			t.Errorf("a round-%d frame states max %d after one stated %d", round, frame.MaxRounds, cap)
		}
		cap = frame.MaxRounds
		if (frame.MaxRounds != 2 && frame.MaxRounds != 6) || frame.RoundCeiling != 8 ||
			(round > 2 && frame.MaxRounds != 6) {
			t.Errorf("a round-%d frame states max %d / ceiling %d", round, frame.MaxRounds, frame.RoundCeiling)
		}
		if frame.RoundStartedAt.IsZero() {
			t.Errorf("a round-%d frame names no start", round)
		}
		// A frame carries every round the phase has finished, on the
		// phase's scale: every one before the round it is on, and that
		// round too once its response has been read — while its request
		// is still out, the frame names it and has no timing for it yet.
		if n := len(frame.Rounds); n != round && n != round-1 {
			t.Errorf("a round-%d frame carries %d timed rounds", round, n)
		}
		for _, r := range frame.Rounds {
			if r.Round > round {
				t.Errorf("a round-%d frame carries round %d, which has not begun", round, r.Round)
			}
		}
		for _, call := range frame.RunningCalls {
			if call.Round != round {
				t.Errorf("the call running in round %d is numbered %d", round, call.Round)
			}
			if call.ID == "" {
				t.Errorf("the call running in round %d carries no id", round)
			}
			if !announced[call.ID] {
				announced[call.ID] = true
				running = append(running, call.Name)
			}
		}
	}
	if last := frames[len(frames)-1]; last.MaxRounds != 6 {
		t.Errorf("the phase's last frame states max %d, want the extended 6", last.MaxRounds)
	}
	// Every call announced WHILE it ran — the three calls this phase made,
	// in order.
	want := []string{toolname.ReflectAndPersist, toolname.ReflectAndPersist, toolname.MarkOnboarded}
	if !slices.Equal(running, want) {
		t.Errorf("running calls announced = %v, want %v", running, want)
	}
}

// A COLLECTED RUN'S RECORD STATES ITS OWN SEGMENT, BOTH SEGMENTS' TIME, AND
// ITS LAUNCH. The segment that collects a run starts when the collection does —
// not at the launch, on another node perhaps a day before — but the record is
// the phase's only completion, so its duration is the phase's: the launching
// segment's span, carried on the park, plus the collection's, and never the
// run between them, which is the run's own fact on the record of the box it ran
// in. And one turn can launch more than once, so the record names the launch it
// collected.
//
// Mutation: drop the launch's span from the park, and the record measures the
// collection alone — under a millisecond, which every reader draws as an
// executor that took no time.
func TestACollectedRunStatesItsOwnSegmentAndLaunch(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	const launchTook = 40 * time.Millisecond
	slow := &playLauncher{took: launchTook}
	launched, _ := buildWith(t, entries, buildOpts{pub: pub, launcher: slow})
	if w, _, err := launched.Execute(context.Background(), 1, "", nil); err != nil || !w.Suspended {
		t.Fatalf("Execute = suspended %v, %v; want the executor parked on its run", w.Suspended, err)
	}
	bridged := play(t, slow.last(t), deliver(t, "shipped it"))

	before := time.Now().UTC()
	collect(t, entries, buildOpts{pub: pub}, launched, nil, bridged, "the run succeeded",
		runner.RunRecord{CodingAgent: "claude-code", SandboxID: "sbx-1", LaunchID: "launch-2"})

	done := completedPhase(t, pub, "execute")
	if done.StartedAt.Before(before) {
		t.Errorf("started_at = %v, want this segment's start (after %v)", done.StartedAt, before)
	}
	if took := time.Duration(done.DurationMS) * time.Millisecond; took < launchTook {
		t.Errorf("duration_ms = %d, want at least the %v the launching segment took: the "+
			"record is the phase's only completion and covers both its segments",
			done.DurationMS, launchTook)
	}
	if done.LaunchID != "launch-2" {
		t.Errorf("launch_id = %q, want the launch the collection took", done.LaunchID)
	}
}

// A COLLECTED RUN STATES ONE TIMED ROUND, OVER THE RUN, AND PLACES ITS CALLS.
//
// The engine's whole exchange with the run is one request — the launch — and
// one answer — the collection — so the record states one round of one, from
// the launch's instant to the collection's, and every bridged call on it at
// the instant and for the span the bridge measured, naming who answered. A
// record that said "one round" and timed none was a timeline with a hole in
// it, and a call with no start is one no timeline can place. The round
// carries no tokens: the run's spend is on each launch's own
// `executor_collected` record.
//
// Mutations: leave the round untimed, state no cap, or drop the calls'
// measurement on the way to the record, and this goes red.
func TestACollectedRunStatesOneTimedRoundOverTheRun(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launcher := &playLauncher{}
	launched, _ := buildWith(t, entries, buildOpts{pub: pub, launcher: launcher})
	before := time.Now().UTC()
	if w, _, err := launched.Execute(context.Background(), 1, "", nil); err != nil || !w.Suspended {
		t.Fatalf("Execute = suspended %v, %v; want the executor parked on its run", w.Suspended, err)
	}
	afterLaunch := time.Now().UTC()
	bridged := play(t, launcher.last(t), deliver(t, "shipped it"))
	// THE RUN TAKES TIME, which is what the round has to cover.
	const ran = 30 * time.Millisecond
	time.Sleep(ran)

	collect(t, entries, buildOpts{pub: pub}, launched, nil, bridged, "shipped it",
		runner.RunRecord{CodingAgent: "claude-code", SandboxID: "sbx-1", Model: "claude-sonnet-5"})

	done := completedPhase(t, pub, "execute")
	if done.RoundsUsed != 1 || len(done.Rounds) != 1 {
		t.Fatalf("rounds_used = %d with %d timed rounds, want one round, timed",
			done.RoundsUsed, len(done.Rounds))
	}
	if done.MaxRounds != 1 || done.RoundCeiling != 1 {
		t.Errorf("max_rounds = %d, round_ceiling = %d: a phase of one round states a cap of one",
			done.MaxRounds, done.RoundCeiling)
	}
	round := done.Rounds[0]
	if round.StartedAt.Before(before) || round.StartedAt.After(afterLaunch) {
		t.Errorf("the round started at %s, want the launch's instant (%s..%s)",
			round.StartedAt, before, afterLaunch)
	}
	if took := time.Duration(round.DurationMS) * time.Millisecond; took < ran {
		t.Errorf("the round took %s, want at least the %s the run was out", took, ran)
	}
	if round.InputTokens != 0 || round.OutputTokens != 0 {
		t.Errorf("the round carries %d/%d tokens, which the run's own record already states",
			round.InputTokens, round.OutputTokens)
	}
	if round.Model != "claude-sonnet-5" || round.ToolCalls != len(bridged) {
		t.Errorf("round = %+v, want the run's model and its %d calls", round, len(bridged))
	}
	if len(done.ToolExecutions) != len(bridged) {
		t.Fatalf("%d executions, want the run's %d calls", len(done.ToolExecutions), len(bridged))
	}
	for _, ex := range done.ToolExecutions {
		at, _ := ex["started_at"].(string)
		started, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || started.Before(round.StartedAt) {
			t.Errorf("%v started at %q, want an instant inside the run's round", ex["name"], at)
		}
		if _, ok := ex["duration_ms"]; !ok {
			t.Errorf("%v carries no duration", ex["name"])
		}
		if origin, _ := ex["origin"].(string); origin == "" {
			t.Errorf("%v names no origin", ex["name"])
		}
	}
}

// A PARK THAT CARRIES NO LAUNCH INSTANT STATES ITS ROUND UNTIMED, never placed
// at the collection: a round drawn from the collection's own start would show
// a run that took no time, where "not measured" is the truth.
func TestARunWithNoLaunchInstantLeavesItsRoundUntimed(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1},
			Answer: "done",
		},
	})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := completedPhase(t, pub, "execute")
	if done.RoundsUsed != 1 || len(done.Rounds) != 0 {
		t.Errorf("rounds_used = %d with %d timed rounds, want the one round, untimed",
			done.RoundsUsed, len(done.Rounds))
	}
}

// A RUN'S WORKERS NEST UNDER THE ROUND ITS CALLS ARE ON.
//
// The run's delegate call arrives over the bridge, where no engine loop is
// iterating to say which round it is in; the run's record puts every call it
// made on its one round, so a worker it spawned has to name that round, or a
// reader places it under no call at all. And under the ITERATION it was
// launched in, since a self-iterating turn launches once per round.
//
// Mutation: leave the spawner's round unstated, and both the worker and the
// batch name round 0.
func TestARunsWorkersNestUnderTheRunsRound(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: workerProvider{}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file", out: "contents"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7"}
	launcher := &playLauncher{}
	r, err := runner.New(runner.Config{
		Seat:      prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry:  reg,
		Models:    models,
		Launcher:  launcher,
		Task:      "fan this out",
		Publisher: pub,
		Subagent:  &runner.SubagentConfig{Limits: shipped()},
		Turn: runner.Turn{RunID: "t-fan", AgentID: "agent-1",
			Context: &turnctx.Turn{WorkItem: item}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if _, _, err := r.Execute(context.Background(), 2, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	play(t, launcher.last(t), run{calls: []llm.ToolCall{
		activate("read_file"),
		call(t, "read_file", `{"path":"/a"}`),
		{ID: "d", Name: subagent.ToolName, Arguments: map[string]any{"tasks": []any{map[string]any{
			"id": "research", "prompt": "look", "system_prompt": "you research things"}}}},
	}})

	workers := phasesOfKind(t, pub, "subagent")
	if len(workers) != 1 {
		t.Fatalf("%d worker records, want 1", len(workers))
	}
	w := workers[0]
	if w.HostRound != 1 || w.Iteration != 2 || w.HostIteration != 2 {
		t.Errorf("the worker nests at round %d of iteration %d (host iteration %d), want "+
			"round 1 of the iteration its run was launched in (2)", w.HostRound, w.Iteration, w.HostIteration)
	}
	if w.StartedAt.IsZero() || len(w.Rounds) != 1 || w.MaxRounds != shipped().MaxTurns {
		t.Errorf("the worker's own timeline: started %v, rounds %+v, max %d", w.StartedAt, w.Rounds, w.MaxRounds)
	}
	if w.CacheReadTokens != 7 {
		t.Errorf("the worker's cache share = %d, want its own 7", w.CacheReadTokens)
	}
	if w.WorkItem == nil || w.WorkItem.Key != "ENG-7" {
		t.Errorf("the worker record names item %+v, want the turn's ENG-7", w.WorkItem)
	}
	batch := batched(t, pub)
	if batch.Round != 1 || batch.StartedAt.IsZero() || batch.StartedAt.After(w.StartedAt) {
		t.Errorf("the call's summary names round %d, started %v (worker started %v)",
			batch.Round, batch.StartedAt, w.StartedAt)
	}
}

// workerProvider plays a delegated worker that submits at once — the one
// model a run's delegate call reaches through the engine, since the run's own
// rounds are its CLI's. Anything else asking it is a fixture wired wrong, and
// is refused rather than handed a worker's answer.
type workerProvider struct{}

func (workerProvider) Model() string { return "scripted" }

func (workerProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if req.Tools == nil {
		return nil, errors.New("workerProvider: asked with no tools, so not by a worker")
	}
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(c *llmtest.Conn) (llmtest.Response, error) {
		if !slices.Contains(c.OfferedNames(), subagent.SubmitTool) {
			return llmtest.Response{}, errors.New("workerProvider: asked by something that is not a worker")
		}
		return llmtest.Response{
			Calls: []llmtest.Call{{Name: subagent.SubmitTool, Args: `{"result":"found it"}`}},
			Usage: llm.Usage{Input: 30, Output: 5, CacheRead: 7},
		}, nil
	})
}

// batched is the one delegate call's summary.
func batched(t *testing.T, c *capture) *types.SubagentBatched {
	t.Helper()
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.SubagentBatched](ev); ok {
			return got
		}
	}
	t.Fatal("no subagent_batched was published")
	return nil
}
