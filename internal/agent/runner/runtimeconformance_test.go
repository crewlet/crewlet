package runner_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// ONE SUITE, BOTH EXECUTOR RUNTIMES.
//
// Agent mode's whole design claim is that only the ROUNDS happen elsewhere:
// same prompt, same surface, same submission, same outcome vocabulary, same
// rescue. That claim is exactly the kind nobody notices breaking — the two
// paths are separate code, each has its own tests, and each passes while
// disagreeing with the other about what a turn means.
//
// So every case below is written once, against a scenario rather than against
// a runtime, and run twice: through the native tool loop with the model
// scripted, and through an agent-mode run with the bridged calls scripted.
// What is asserted is the WORK, which is the only thing either runtime owes
// the turn engine — and the turn engine cannot tell them apart.
//
// A case that legitimately differs does not belong here. The two runtimes are
// genuinely different in one place — a native pass makes model calls and an
// agent run makes none — and that is asserted in agentrun_test.go, not here.

// submitJSON is a completion whose only content is a submission with these
// arguments — the native runtime's spelling of a bridged submit_work call.
func submitJSON(name string, args map[string]any) llm.Completion {
	return llm.Completion{
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: name, Arguments: args}},
	}
}

// scenario is one turn's worth of behaviour, expressed the way each runtime
// receives it.
type scenario struct {
	name string

	// native is what the model says, in order.
	native []llm.Completion
	// bridged is what an agent-mode run called, in order. The submission
	// is one of these, exactly as it is one of the native completions.
	bridged []ledger.Call
	// text is the run's own closing prose, which only the agent runtime
	// has — the native loop's equivalent is the last completion's content.
	text string

	want turn.Work
}

func conformanceCases() []scenario {
	delivered := map[string]any{
		"outcome": "delivered", "summary": "posted the weekly summary",
		"deliveries": []any{"slack_post"},
	}
	return []scenario{
		{
			name: "a delivery with a citation is delivered",
			native: []llm.Completion{
				activate("slack_post"),
				{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post",
					Arguments: map[string]any{"channel": "C1"}}}},
				submitJSON(runner.SubmitWorkTool, delivered),
			},
			bridged: []ledger.Call{
				{Name: "slack_post", Result: "posted"},
				{Name: runner.SubmitWorkTool, Args: delivered},
			},
			want: turn.Work{
				Outcome: turn.OutcomeDelivered, Summary: "posted the weekly summary",
				Deliveries: []string{"slack_post"},
			},
		},
		{
			name: "a delivery nothing did is refused, then corrected",
			native: []llm.Completion{
				// The first submission cites a tool that never ran; the
				// decoder rejects it and the model corrects itself.
				submitJSON(runner.SubmitWorkTool, map[string]any{
					"outcome": "delivered", "summary": "told them",
					"deliveries": []any{"slack_post"},
				}),
				submitJSON(runner.SubmitWorkTool, map[string]any{
					"outcome": "blocked", "summary": "no channel to post in",
					"evidence": "the seat has no write tool on its surface",
				}),
			},
			bridged: []ledger.Call{
				{Name: runner.SubmitWorkTool, Args: map[string]any{
					"outcome": "delivered", "summary": "told them",
					"deliveries": []any{"slack_post"},
				}},
				{Name: runner.SubmitWorkTool, Args: map[string]any{
					"outcome": "blocked", "summary": "no channel to post in",
					"evidence": "the seat has no write tool on its surface",
				}},
			},
			want: turn.Work{Outcome: turn.OutcomeBlocked, Summary: "no channel to post in"},
		},
		{
			name: "an executor that never submits is rescued",
			native: []llm.Completion{
				{Content: "I looked around and ran out of road"},
			},
			bridged: []ledger.Call{{Name: "slack_history", Result: "read"}},
			text:    "I looked around and ran out of road",
			want: turn.Work{
				Outcome: turn.OutcomeIncomplete,
				Summary: "I looked around and ran out of road",
				Rescued: true,
			},
		},
		{
			name: "a malformed submission ends in the rescue, not an error",
			native: []llm.Completion{
				submitJSON(runner.SubmitWorkTool, map[string]any{"outcome": "teleported"}),
			},
			bridged: []ledger.Call{
				{Name: runner.SubmitWorkTool, Args: map[string]any{"outcome": "teleported"}},
			},
			want: turn.Work{Outcome: turn.OutcomeIncomplete, Rescued: true},
		},
	}
}

func TestBothExecutorRuntimesAgreeAboutWhatATurnDid(t *testing.T) {
	t.Parallel()
	for _, tc := range conformanceCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			native := runNative(t, tc)
			agent := runAgent(t, tc)
			assertWork(t, "native", native, tc.want)
			assertWork(t, "agent", agent, tc.want)
			// AND THE SAME AS EACH OTHER, which is a stronger claim than
			// both matching the table: a field the table forgot to name
			// still has to agree.
			if native.Outcome != agent.Outcome || native.Summary != agent.Summary ||
				native.Rescued != agent.Rescued {
				t.Errorf("the runtimes disagree:\n native = %+v\n agent  = %+v", native, agent)
			}
		})
	}
}

// runNative drives the scenario through the engine's own tool loop.
func runNative(t *testing.T, tc scenario) turn.Work {
	t.Helper()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: tc.native,
	}}}, buildOpts{})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("native Execute: %v", err)
	}
	return w
}

// runAgent drives the same scenario through an agent-mode run: the launch
// suspends, and the resume rebuilds the phase from the run's bridged calls.
//
// BOTH HALVES, not just the resume, because the launch is where the surface
// and the prompt are built and a suite that skipped it would certify only the
// half that is easy to reach.
func runAgent(t *testing.T, tc scenario) turn.Work {
	t.Helper()
	launcher := &recordingLauncher{}
	launched, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{agentRun: launcher})
	if _, _, err := launched.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("agent Execute: %v", err)
	}
	suspension, ok := launched.Suspended()
	if !ok {
		t.Fatal("the agent-mode launch recorded no suspension")
	}

	// THE STATE THE ENGINE WOULD HAVE PERSISTED, round-tripped through the
	// wire format, because a resume in another process reads it back rather
	// than holding it.
	blob, err := execstate.Encode(suspension.State)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	state, found, err := execstate.Decode(blob)
	if err != nil || !found {
		t.Fatalf("Decode: %v (found %v)", err, found)
	}

	resumed, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{
			agentRun: &recordingLauncher{},
			resume:   &runner.Resume{State: state, Answer: tc.text, Bridged: tc.bridged},
		})
	w, _, err := resumed.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("agent Resume: %v", err)
	}
	return w
}

func assertWork(t *testing.T, runtime string, got, want turn.Work) {
	t.Helper()
	if got.Outcome != want.Outcome {
		t.Errorf("%s: outcome = %q, want %q", runtime, got.Outcome, want.Outcome)
	}
	if want.Summary != "" && got.Summary != want.Summary {
		t.Errorf("%s: summary = %q, want %q", runtime, got.Summary, want.Summary)
	}
	if got.Rescued != want.Rescued {
		t.Errorf("%s: rescued = %v, want %v", runtime, got.Rescued, want.Rescued)
	}
	if len(want.Deliveries) > 0 && len(got.Deliveries) != len(want.Deliveries) {
		t.Errorf("%s: deliveries = %v, want %v", runtime, got.Deliveries, want.Deliveries)
	}
}
