package runner_test

import (
	"context"
	"slices"
	"sync"
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
	"github.com/crewlet/crewlet/internal/tools"
)

// WHAT A PHASE RECORD SAYS ABOUT ITS TIME AND ITS CACHE, on the wire.
//
// The tool loop measures every round and every call; these hold the runner to
// carrying those measurements onto the events a dashboard reads — folded
// across an extension and a resume exactly as the rounds themselves are, since
// a timeline whose numbers are on a different scale from the ledger beside it
// is a second ledger that disagrees with the first.

// cached is a round that also reports a prompt-cache share.
func cached(c llm.Completion, read, write int) llm.Completion {
	c.CacheRead, c.CacheWrite = read, write
	return c
}

func TestAPhaseRecordCarriesItsRoundsCallsAndCache(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		cached(thinkAndCall(t, "read_file", `{"path":"/a"}`, "start"), 50, 10),
		cached(thinkAndCall(t, "read_file", `{"path":"/b"}`, "again"), 55, 0),
		cached(thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "enough"), 58, 0),
		text("done"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, alwaysExtend{})

	before := time.Now().UTC()
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	after := time.Now().UTC()

	done := completedPhase(t, pub, "execute")
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
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "one"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "two"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "three"),
		text("done"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, spendingJudge{})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	done := completedPhase(t, pub, "execute")
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
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "one"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "two"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "three"),
		text("done"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, alwaysExtend{})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The opening frame already states the cap, before the model has
	// answered once.
	if open := openingFrame(t, pub, "execute"); open.MaxRounds != 2 || open.RoundCeiling != 8 {
		t.Errorf("the opening frame states max %d / ceiling %d, want 2 / 8", open.MaxRounds, open.RoundCeiling)
	}
	var running []string
	for _, frame := range progressFrames(t, pub, "execute") {
		round := frame.RoundNum + 1
		want := 2
		if round > 2 {
			want = 6 // the extension moved it
		}
		if frame.MaxRounds != want || frame.RoundCeiling != 8 {
			t.Errorf("a round-%d frame states max %d / ceiling %d, want %d / 8",
				round, frame.MaxRounds, frame.RoundCeiling, want)
		}
		if frame.RoundStartedAt.IsZero() {
			t.Errorf("a round-%d frame names no round start", round)
		}
		if len(frame.Rounds) != round {
			t.Errorf("a round-%d frame carries %d timed rounds", round, len(frame.Rounds))
		}
		if call := frame.RunningCall; call != nil {
			if call.Round != round {
				t.Errorf("the call running in round %d is numbered %d", round, call.Round)
			}
			running = append(running, call.Name)
		}
	}
	// One announcement per call, each BEFORE it ran — the three calls this
	// phase made, in order.
	want := []string{"read_file", "read_file", runner.SubmitWorkTool}
	if !slices.Equal(running, want) {
		t.Errorf("running calls announced = %v, want %v", running, want)
	}
}

func TestAResumedPhaseStatesItsOwnSegmentStart(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "slack_post", `{"text":"the fix is up"}`, "tell the requester"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"shipped it","deliveries":["slack_post"]}`,
			"the box did the work"),
		text("done"),
	}}
	// Parked a day ago, on another node, after an hour of work.
	state := suspendedAfterTwoRounds()
	parkedAt := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	state.ElapsedMS = int(time.Hour / time.Millisecond)
	state.Rounds = []types.PhaseRound{
		{Round: 1, StartedAt: parkedAt, DurationMS: 1200, Model: "earlier", InputTokens: 200, OutputTokens: 50},
		{Round: 2, StartedAt: parkedAt.Add(time.Minute), DurationMS: 900, Model: "earlier",
			InputTokens: 200, OutputTokens: 50, CacheReadTokens: 150, ToolCalls: 1},
	}
	state.CacheReadTokens = 150
	state.ToolExecutions[1]["started_at"] = parkedAt.Add(2 * time.Minute).Format(time.RFC3339Nano)
	state.ToolExecutions[1]["duration_ms"] = 3100
	state.ToolExecutions[1]["origin"] = tools.OriginBuiltin

	// Through JSON, the way the pending-run row hands it back: every number
	// in a loose map is a float64 on the other side.
	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	parked, _, err := execstate.Decode(blob)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{State: parked, Answer: "the run succeeded", Run: runner.RunRecord{
			CodingAgent: "claude-code", SandboxID: "sbx-1", LaunchID: "launch-2",
		}},
	})
	before := time.Now().UTC()
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	// THIS SEGMENT's start — not the published instant minus a duration
	// that covers an hour of work a day ago on another node.
	if done.StartedAt.Before(before) {
		t.Errorf("started_at = %v, want this segment's start (after %v)", done.StartedAt, before)
	}
	// WHICH LAUNCH this segment collected: one turn can launch more than
	// once, and the segment is identified by it.
	if done.LaunchID != "launch-2" {
		t.Errorf("launch_id = %q, want the launch the resume collected", done.LaunchID)
	}
	if done.DurationMS < int(time.Hour/time.Millisecond) {
		t.Errorf("duration_ms = %d, want the whole phase (at least the hour before the suspend)", done.DurationMS)
	}
	// The pre-suspend rounds keep THEIR timing and their cache share.
	if len(done.Rounds) != 4 {
		t.Fatalf("rounds = %+v, want the two parked rounds and the two resumed ones", done.Rounds)
	}
	if !done.Rounds[0].StartedAt.Equal(parkedAt) || done.Rounds[1].DurationMS != 900 || done.Rounds[2].Round != 3 {
		t.Errorf("the timeline across the suspend is %+v", done.Rounds)
	}
	if done.CacheReadTokens != 150 {
		t.Errorf("cache_read_tokens = %d, want the parked half's 150 carried", done.CacheReadTokens)
	}
	parkedCall := done.ToolExecutions[1]
	if parkedCall["name"] != "run_sandbox" || parkedCall["duration_ms"] != 3100 || parkedCall["origin"] != tools.OriginBuiltin {
		t.Errorf("the call that parked the phase lost its timing: %v", parkedCall)
	}
	// And a row an OLDER build wrote stays untimed rather than acquiring a
	// zero: absent is "not recorded", a zero is "instant".
	if _, timed := done.ToolExecutions[0]["duration_ms"]; timed {
		t.Errorf("an untimed pre-suspend call acquired a duration: %v", done.ToolExecutions[0])
	}
}

func TestAWorkerNamesTheRoundThatSpawnedIt(t *testing.T) {
	t.Parallel()
	prov := &delegatingProvider{}
	pub := newCapture()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file", out: "contents"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	item := &types.WorkItem{Backend: types.WorkNative, ID: "task-7", Key: "ENG-7"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models,
		// A ONE-ROUND cap, extended: the delegate call is made in the
		// SECOND invocation of the loop, which numbers its own rounds from
		// 1 — so a worker that reports the phase's round 2 here is one that
		// heard the phase's scale rather than the invocation's.
		Caps: runner.Caps{
			ExecutorRounds: 1, ExecutorCeiling: 6,
			ExtensionOn: true, ExtensionStep: 4,
		},
		Judge:     alwaysExtend{},
		Task:      "fan this out",
		Publisher: pub,
		Subagent:  &runner.SubagentConfig{Limits: shipped()},
		Turn: runner.Turn{RunID: "t-fan", AgentID: "agent-1",
			Context: &turnctx.Turn{WorkItem: item}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	workers := phasesOfKind(t, pub, "subagent")
	if len(workers) != 1 {
		t.Fatalf("%d worker records, want 1", len(workers))
	}
	w := workers[0]
	// Round 2 made the delegate call; round 1 read a file first.
	if w.HostRound != 2 {
		t.Errorf("the worker's host round = %d, want 2 — the round whose delegate call spawned it", w.HostRound)
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
	if batch.Round != 2 || batch.StartedAt.IsZero() || batch.StartedAt.After(w.StartedAt) {
		t.Errorf("the call's summary names round %d, started %v (worker started %v)",
			batch.Round, batch.StartedAt, w.StartedAt)
	}

	// A worker's tokens are the turn's DELEGATED spend, split by direction
	// and kept out of the turn's own totals; the turn's own cache share is
	// its phases', which is the executor's two rounds here.
	spend := r.Spend()
	if spend.Workers != 1 || spend.WorkerInput != 30 || spend.WorkerOutput != 5 || spend.WorkerTokens() != 35 {
		t.Errorf("delegated tally = %d workers, %d in / %d out", spend.Workers, spend.WorkerInput, spend.WorkerOutput)
	}
	if spend.CacheRead != 3*11 {
		t.Errorf("the turn's own cache share = %d, want the executor's 33", spend.CacheRead)
	}
}

// delegatingProvider plays an executor that reads, delegates, and submits, and
// a worker that submits at once. Told apart by the submission tool each is
// offered; guarded because the worker runs on its own goroutine.
type delegatingProvider struct {
	mu       sync.Mutex
	executor int
}

func (p *delegatingProvider) Model() string { return "scripted" }

func (p *delegatingProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slices.Contains(toolNames(req.Tools), subagent.SubmitTool) {
		return &llm.Completion{
			ToolCalls:   []llm.ToolCall{{ID: "w1", Name: subagent.SubmitTool, Arguments: map[string]any{"result": "found it"}}},
			InputTokens: 30, OutputTokens: 5, CacheRead: 7,
		}, nil
	}
	p.executor++
	var c llm.Completion
	switch p.executor {
	case 1:
		c = llm.Completion{ToolCalls: []llm.ToolCall{{ID: "e1", Name: "read_file", Arguments: map[string]any{"path": "/a"}}}}
	case 2:
		c = llm.Completion{ToolCalls: []llm.ToolCall{{ID: "e2", Name: subagent.ToolName, Arguments: map[string]any{
			"tasks": []any{map[string]any{"id": "research", "prompt": "look", "system_prompt": "you research things"}},
		}}}}
	default:
		c = llm.Completion{ToolCalls: []llm.ToolCall{{ID: "e3", Name: runner.SubmitWorkTool, Arguments: map[string]any{
			"outcome": "no_action", "summary": "delegated the research",
		}}}}
	}
	c.InputTokens, c.OutputTokens, c.CacheRead = 100, 10, 11
	return &c, nil
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
