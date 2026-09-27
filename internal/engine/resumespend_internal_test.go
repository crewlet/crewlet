package engine

import (
	"errors"
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// parkedOnARun is an executor suspended on its run_sandbox call after rounds
// that billed 500 tokens on one model, as the pending-run row carries it.
func parkedOnARun(t *testing.T) map[string]any {
	t.Helper()
	blob, err := execstate.Encode(execstate.State{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you are the engineer"},
			{Role: llm.RoleUser, Content: "fix the flake"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "run_sandbox", Arguments: map[string]any{"task": "fix it"}},
			}},
		},
		PendingCallID: "call-1", PendingCallName: "run_sandbox",
		Round: 1, RoundsUsed: 1, InputTokens: 400, OutputTokens: 100,
		Model:  "exec-model",
		Models: []types.ModelSpend{{Model: "exec-model", InputTokens: 400, OutputTokens: 100}},
		Task:   "fix the flake",
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return blob
}

// A CODING RUN'S OWN SPEND REACHES THE RECORD OF THE PHASE ITS RESUME RE-ENTERS.
//
// The coordinator hands the resume what the run's agent reported — its tokens
// by model, and whether they are the run's whole — and the resumed Execute
// phase's record is the one account of that spend a rollup can read: the agent
// called its models inside the box, where no round of this process billed
// them. Driven through the coordinator's seam, [resumer.Resume], because the
// hand-over from the request to the phase is the whole of what is under test:
// dropped anywhere on the way, the record prices the run at what its agent
// said and sizes it at the rounds this process ran around it.
//
// The resumed phase's model fails, so its record is the failure record — which
// counts the carried spend like any first record of a resume — and the resume
// is handed back for a retry. The retry is told the spend was counted, and its
// record counts none of it.
func TestARunsOwnSpendReachesTheResumedPhasesRecord(t *testing.T) {
	t.Parallel()
	run := types.RunSpend{Collected: true, Models: []types.ModelSpend{
		{Model: "claude-sonnet", InputTokens: 600, OutputTokens: 60, CostUSD: 0.9},
	}}
	for _, tc := range []struct {
		name       string
		counted    bool
		tokens     int
		models     map[string][2]int
		unreported bool
		costUSD    float64
		handedBack error
	}{
		{
			name: "the first attempt", tokens: 1160,
			models:     map[string][2]int{"exec-model": {400, 100}, "claude-sonnet": {600, 60}},
			unreported: true, costUSD: 0.9, handedBack: sandbox.ErrCarriedCounted,
		},
		{
			name: "a retry told the spend was counted", counted: true, tokens: 0,
			models: map[string][2]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			company, seat := resumableCompany(t, unavailableModel{}, 0)
			q := memory.New()
			if err := q.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			e, _ := resumingEngine(t)
			e.backends = &Backends{Queue: q}
			e.epoch.current.Store(company)

			err := (&resumer{engine: e}).Resume(t.Context(), sandbox.ResumeRequest{
				Run: sandbox.PendingRun{
					TurnID: "wk-1", AgentHandle: seat.Handle(), ConversationKey: "slack:C1",
					CodingAgent: "claude-code", SandboxID: "box-1",
					ExecuteState: parkedOnARun(t), CarriedCounted: tc.counted,
				},
				Answer: "the run finished", Success: true,
				CostUSD: 0.9, RunSpend: run,
			})
			if err == nil || errors.Is(err, sandbox.ErrResumeAbandoned) {
				t.Fatalf("Resume = %v, want the failed resume handed back for a retry", err)
			}
			if tc.handedBack != nil && !errors.Is(err, tc.handedBack) {
				t.Errorf("Resume = %v, want it to say its record counted the carried spend", err)
			}

			var rec *types.AgentPhaseCompleted
			for _, ev := range q.History() {
				if got, ok := ev.Data.(*types.AgentPhaseCompleted); ok && got.Phase == types.PhaseExecute {
					rec = got
				}
			}
			if rec == nil {
				t.Fatal("the resumed phase published no record")
			}
			split := map[string][2]int{}
			for _, m := range rec.Models {
				split[m.Model] = [2]int{m.InputTokens, m.OutputTokens}
			}
			if rec.TotalTokens != tc.tokens || rec.RunSpendUnreported != tc.unreported ||
				rec.CostUSD != tc.costUSD || rec.Backend != types.BackendSandbox {
				t.Errorf("record = %d tokens, unreported %v, $%v on %q; want %d, %v, $%v on the box",
					rec.TotalTokens, rec.RunSpendUnreported, rec.CostUSD, rec.Backend,
					tc.tokens, tc.unreported, tc.costUSD)
			}
			if !maps.Equal(split, tc.models) {
				t.Errorf("models = %+v, want %v", rec.Models, tc.models)
			}
		})
	}
}
