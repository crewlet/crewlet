package runner_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// namedWrite is a write named after the writes before it in its turn, the way
// the native tracker's are: the first write to an object under a base takes
// the base, and each later one the first `#n` no earlier receipt holds. It
// reads those receipts off [turnctx.Turn.Calls], which is the whole of what a
// surface owes it, and records every name it gave.
type namedWrite struct {
	mu    sync.Mutex
	named []string
}

var (
	_ tools.SeatCallable = (*namedWrite)(nil)
	_ tools.Sequenced    = (*namedWrite)(nil)
)

const namedWriteTool = "update_item"

func (w *namedWrite) Name() string        { return namedWriteTool }
func (w *namedWrite) Description() string { return "Update an item." }
func (w *namedWrite) Sequenced()          {}
func (w *namedWrite) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]any{"type": "string"},
	}}
}

func (w *namedWrite) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "no turn to name the write in", Failed: true}, nil
}

func (w *namedWrite) CallForTurn(_ context.Context, t *turnctx.Turn, args map[string]any) (tools.Result, error) {
	held := map[string]bool{}
	for _, call := range t.Calls() {
		if call.Name != namedWriteTool {
			continue
		}
		var receipt struct {
			Operations []string `json:"operations"`
		}
		if json.Unmarshal([]byte(call.Result), &receipt) == nil {
			for _, op := range receipt.Operations {
				held[op] = true
			}
		}
	}
	id, _ := args["id"].(string)
	base := t.WorkKey + "-update-" + id
	name := base
	for n := 2; held[name]; n++ {
		name = base + "#" + strconv.Itoa(n)
	}
	w.mu.Lock()
	w.named = append(w.named, name)
	w.mu.Unlock()
	out, err := json.Marshal(map[string]any{"operations": []string{name}})
	return tools.Result{Output: string(out)}, err
}

func (w *namedWrite) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.named...)
}

// runnerWithTurn is a runner bound to a turn, with the named write registered
// beside the fixture's tools, over prov.
func runnerWithTurn(t *testing.T, prov llm.Provider, write *namedWrite, opts runner.Config) *runner.Runner {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(write, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range []string{runner.ReflectAndPersistTool, runner.MarkOnboardedTool} {
		if err := reg.Register(stubTool{name: name, out: "ok"}, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	opts.Seat = prompts.Seat{Org: organization, Role: role}
	opts.Registry, opts.Models = reg, models
	opts.Caps = runner.Caps{ExecutorRounds: 6}
	opts.Task = "fix ENG-1"
	opts.Reply = turn.ToolReply("")
	opts.Turn = runner.Turn{RunID: "t-1", WorkKey: "wk-1", AgentID: "a-1",
		Context: &turnctx.Turn{RunID: "t-1", WorkKey: "wk-1", Seat: role, Org: organization}}
	r, err := runner.New(opts)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// suspendedAfterAWrite is an executor round that wrote ENG-1 and then launched
// a coding run, suspending on it.
func suspendedAfterAWrite() execstate.State {
	state := suspendedAfterTwoRounds()
	state.ActiveTools = []string{namedWriteTool}
	state.ToolExecutions = []types.ToolExecution{{
		"name": namedWriteTool, "arguments": `{"id":"ENG-1"}`,
		"result": `{"operations":["wk-1-update-ENG-1"]}`, "success": true, "round": 1,
	}}
	return state
}

// A RESUMED PHASE REMEMBERS WHAT IT WROTE BEFORE IT SUSPENDED.
//
// The round a resume re-enters is one no closed round holds, so its calls from
// before the suspend reach a later write only through the Turn the resumed
// surface is bound to. Bound without them, "update ENG-1; run_sandbox;
// [resume] update ENG-1" named the second update as the first was named, and
// the operation ledger answered it applied and dropped it. Two writes to one
// item are two names.
func TestAWriteAfterAResumeIsNamedAfterTheWritesBeforeTheSuspend(t *testing.T) {
	t.Parallel()
	write := &namedWrite{}
	prov := &scriptedProvider{execute: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "again", Name: namedWriteTool,
			Arguments: map[string]any{"id": "ENG-1"}}}},
		submitWork(t),
	}}
	r := runnerWithTurn(t, prov, write, runner.Config{
		Resume: &runner.Resume{State: suspendedAfterAWrite(), Answer: "the run succeeded"},
	})

	if _, _, err := r.Resume(context.Background(), 1, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := write.names(); len(got) != 1 || got[0] != "wk-1-update-ENG-1#2" {
		t.Errorf("the write after the resume was named %v, want the second write to ENG-1: "+
			"named as the first, the ledger answers it applied without writing it", got)
	}
}

// THE ONBOARDING PASS CANNOT REACH A WRITE NAMED BY ITS PLACE IN THE TURN.
//
// The pass's calls are in no round the turn's phases are bound to, and a
// redelivered turn does not onboard again — so a write it made would take the
// name the turn's own first write to that object derives, on this attempt or
// on a redelivery, and the ledger would collapse the turn's write into it.
// Such a write is not discoverable from the pass, nor callable by name — and
// the pass's prompt does not list it among its tools either, since a model
// told it has a tool calls it, and every such call fails.
func TestTheOnboardingPassCannotReachAWriteNamedByItsPlaceInTheTurn(t *testing.T) {
	t.Parallel()
	write := &namedWrite{}
	prov := &scriptedProvider{onboarding: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "act", Name: runner.ActivateTool,
			Arguments: map[string]any{"name": namedWriteTool}}}},
		{ToolCalls: []llm.ToolCall{{ID: "w", Name: namedWriteTool,
			Arguments: map[string]any{"id": "ENG-1"}}}},
		{ToolCalls: []llm.ToolCall{{ID: "m", Name: runner.MarkOnboardedTool,
			Arguments: map[string]any{"notes": "read the pages"}}}},
	}}
	r := runnerWithTurn(t, prov, write, runner.Config{
		Onboarding: runner.Onboarding{Markers: &markers{claimHeld: true}, Latch: runner.NewLatch(),
			Rounds: 4},
	})

	if _, err := r.Onboard(context.Background()); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if got := write.names(); len(got) != 0 {
		t.Errorf("the onboarding pass wrote %v, under names the turn's own writes derive", got)
	}
	requests := prov.requestsFor("onboarding")
	if len(requests) == 0 {
		t.Fatal("the onboarding pass sent no request")
	}
	for _, req := range requests {
		for _, def := range req.Tools {
			if def.Name == namedWriteTool {
				t.Fatalf("the onboarding pass was offered %s", namedWriteTool)
			}
		}
		var catalogue string
		for _, m := range req.Messages {
			if m.Role == llm.RoleSystem {
				_, catalogue, _ = strings.Cut(m.Content, "## Available tools")
			}
		}
		if !strings.Contains(catalogue, runner.MarkOnboardedTool) {
			t.Fatalf("the pass's prompt lists no catalogue with the tools it may call: %q", catalogue)
		}
		if strings.Contains(catalogue, namedWriteTool) {
			t.Fatalf("the pass's prompt lists %s among its tools, which it cannot call", namedWriteTool)
		}
	}
}

// A RESUMED ROUND IS FILED UNDER THE LOOP'S NUMBER, NOT THE STATE'S.
//
// The loop numbers the round it re-enters on from the turn's closed rounds,
// and the round's review is filed under that number; a record filed under the
// round the state names would sit apart from its own review whenever the two
// differ — here the state says round 1 and the turn has closed three.
func TestAResumedRoundIsFiledUnderTheLoopsNumber(t *testing.T) {
	t.Parallel()
	if suspendedAfterTwoRounds().Round == 4 {
		t.Fatal("the premise: the state names the round the loop does, so this proves nothing")
	}
	history := []ledger.Iteration{{Iteration: 1}, {Iteration: 2}, {Iteration: 3}}
	for _, tc := range []struct {
		name     string
		provider llm.Provider
		fails    bool
	}{
		{"a round that submits", &scriptedProvider{execute: []llm.Completion{submitWork(t)}}, false},
		{"a round whose provider fails", unavailable{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pub := newCapture()
			r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: tc.provider}}, buildOpts{
				pub:    pub,
				resume: &runner.Resume{State: suspendedAfterTwoRounds(), Answer: "the run succeeded"},
			})
			if _, _, err := r.Resume(context.Background(), 4, history); (err != nil) != tc.fails {
				t.Fatalf("Resume = %v, want failed=%v", err, tc.fails)
			}
			records := phasesOfKind(t, pub, "execute")
			var got []int
			for _, rec := range records {
				got = append(got, rec.Iteration)
			}
			if len(got) != 1 || got[0] != 4 {
				t.Errorf("the resumed round's records are filed under %v, want the loop's round 4", got)
			}
		})
	}
}
