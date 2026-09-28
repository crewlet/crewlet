package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RESUMED PHASE CARRIES THE USAGE OF THE RUN IT COLLECTED, from the resume
// request to the published record.
//
// The run's usage reaches the Tokens view only on this record: the coordinator
// hands it to the resume, the resumer copies it into the turn it re-enters, and
// the turn maps it onto the executor's run record — and each of those three
// steps is a field assignment that, dropped, leaves every other suite green
// while no phase anywhere says what a coding run spent. So this drives the
// resumer from a real request, through the real turn, to the event the engine
// publishes.
func TestAResumedPhaseCarriesTheUsageOfTheRunItCollected(t *testing.T) {
	e, _ := indicatingWith(t, notify.StatusOff, scripted{})
	var (
		mu     sync.Mutex
		phases []types.AgentPhaseCompleted
	)
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if ev.Type != (types.AgentPhaseCompleted{}).EventType() {
			return
		}
		p, ok := events.DataAs[*types.AgentPhaseCompleted](ev)
		if !ok {
			t.Errorf("a phase record carries %T, not its registered type", ev.Data)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		phases = append(phases, *p)
	})

	// The Execute loop as it suspended: its run_sandbox call unanswered.
	state, err := execstate.Encode(execstate.State{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "fix the failing test"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "call-1", Name: "run_sandbox", Arguments: map[string]any{},
			}}},
		},
		PendingCallID: "call-1", PendingCallName: "run_sandbox",
	})
	if err != nil {
		t.Fatalf("encode the suspended loop: %v", err)
	}
	usage := sandbox.RunUsage{LaunchID: "launch-7", InputTokens: 5000, OutputTokens: 700, CostUSD: 0.5}
	if err := (&resumer{engine: e}).Resume(t.Context(), sandbox.ResumeRequest{
		Run: sandbox.PendingRun{
			TurnID: "wk-1", AgentHandle: "swe", LaunchID: "launch-7",
			CodingAgent: "claude-code", SandboxID: "box-1",
			TaskDescription: "fix the failing test", ExecuteState: state,
		},
		Answer:  "the tests pass now",
		Success: true,
		Usage:   usage,
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var collected *types.AgentPhaseCompleted
	for i := range phases {
		if phases[i].Backend == types.BackendSandbox {
			collected = &phases[i]
		}
	}
	if collected == nil {
		t.Fatalf("no sandbox phase was published among %d phase records", len(phases))
	}
	if collected.LaunchID != usage.LaunchID || collected.RunInputTokens != usage.InputTokens ||
		collected.RunOutputTokens != usage.OutputTokens || collected.CostUSD != usage.CostUSD {
		t.Errorf("the resumed phase carries launch %q, run tokens %d/%d and $%v, want the "+
			"request's %q, %d/%d and $%v", collected.LaunchID, collected.RunInputTokens,
			collected.RunOutputTokens, collected.CostUSD, usage.LaunchID, usage.InputTokens,
			usage.OutputTokens, usage.CostUSD)
	}
	if collected.CodingAgent != "claude-code" {
		t.Errorf("the resumed phase names coding agent %q, want the run's", collected.CodingAgent)
	}
}
