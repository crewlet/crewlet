package runner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// ONE SUITE, BOTH EXECUTOR RUNTIMES.
//
// Both run the executor as a coding CLI's own loop, and differ in where: host
// mode runs it on this host, its every call through the phase's tool session
// and the engine's own gates; box mode runs it detached in a sandbox box, its
// calls over the bridge, and collects the run afterwards from the calls it
// made. The design claim is that only WHERE the rounds happen differs: same
// prompt, same surface, same submission, same outcome vocabulary, same rescue.
// That claim is exactly the kind nobody notices breaking — the two paths are
// separate code, each has its own tests, and each passes while disagreeing
// with the other about what a turn means.
//
// So every case below is written once, against a scenario rather than against
// a runtime, and run twice: through a host-mode CLI with the model scripted,
// its calls reaching the surface over the real session, and through a
// box-mode run with the bridged calls scripted. What is asserted is the WORK,
// which is the only thing either runtime owes the turn engine — and the turn
// engine cannot tell them apart.
//
// A case that legitimately differs does not belong here. The two runtimes are
// genuinely different in one place — a host-mode pass is a loop the engine
// governs, run by run, and a box run is one the engine only launches and
// collects — and that is asserted in agentrun_test.go, not here.

// submitJSON is a response whose only content is a submission with these
// arguments — the host runtime's spelling of a bridged submit_work call.
func submitJSON(t *testing.T, name string, args map[string]any) llmtest.Response {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("conformance fixture: %v", err)
	}
	return submitCall(t, name, string(raw))
}

// scenario is one turn's worth of behaviour, expressed the way each runtime
// receives it.
type scenario struct {
	name string

	// host is what the model says, in order.
	host []llmtest.Response
	// bridged is what a box-mode run called, in order. The submission is
	// one of these, exactly as it is one of the host-mode responses' calls.
	bridged []ledger.Call
	// text is the box run's own closing prose — the host loop's equivalent
	// is its last response's.
	text string

	want turn.Work
}

func conformanceCases(t *testing.T) []scenario {
	delivered := map[string]any{
		"outcome": "delivered", "summary": "posted the weekly summary",
		"deliveries": []any{"slack_post"},
	}
	return []scenario{
		{
			name: "a delivery with a citation is delivered",
			host: []llmtest.Response{
				activate("slack_post"),
				calling(callOf(t, "slack_post", map[string]any{"channel": "C1"})),
				submitJSON(t, runner.SubmitWorkTool, delivered),
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
			host: []llmtest.Response{
				// The first submission cites a tool that never ran; the
				// decoder rejects it and the model corrects itself.
				submitJSON(t, runner.SubmitWorkTool, map[string]any{
					"outcome": "delivered", "summary": "told them",
					"deliveries": []any{"slack_post"},
				}),
				submitJSON(t, runner.SubmitWorkTool, map[string]any{
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
			name:    "an executor that never submits is rescued",
			host:    []llmtest.Response{text("I looked around and ran out of road")},
			bridged: []ledger.Call{{Name: "slack_history", Result: "read"}},
			text:    "I looked around and ran out of road",
			// THE SAME TEXT FROM BOTH. The host loop asks a phase whose run
			// ended in prose to finish twice more before the rescue — a run
			// each, which a box run the engine does not govern never gets —
			// and the model answers each corrective with its report again;
			// those answers are to the corrective, not the task, so the
			// text keeps the first — which is what a box run's one closing
			// reply is. A reviewer reads the same thing whichever runtime the
			// executor ran on.
			want: turn.Work{
				Outcome: turn.OutcomeIncomplete,
				Text:    "I looked around and ran out of road",
				Rescued: true,
			},
		},
		{
			name: "a malformed submission ends in the rescue, not an error",
			host: []llmtest.Response{
				submitJSON(t, runner.SubmitWorkTool, map[string]any{"outcome": "teleported"}),
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
	for _, tc := range conformanceCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := runHost(t, tc)
			box := runBox(t, tc)
			assertWork(t, "host", host, tc.want)
			assertWork(t, "box", box, tc.want)
			// AND THE SAME AS EACH OTHER, which is a stronger claim than
			// both matching the table: a field the table forgot to name
			// still has to agree.
			if host.Outcome != box.Outcome || host.Summary != box.Summary ||
				host.Rescued != box.Rescued {
				t.Errorf("the runtimes disagree:\n host = %+v\n box  = %+v", host, box)
			}
		})
	}
}

// runHost drives the scenario through a host-mode CLI: the engine's tool loop,
// its calls over the phase's session.
func runHost(t *testing.T, tc scenario) turn.Work {
	t.Helper()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: tc.host,
	}}}, buildOpts{})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("host Execute: %v", err)
	}
	return w
}

// runBox drives the same scenario through a box-mode run: the launch suspends,
// and the resume rebuilds the phase from the run's bridged calls.
//
// BOTH HALVES, not just the resume, because the launch is where the surface
// and the prompt are built and a suite that skipped it would certify only the
// half that is easy to reach.
func runBox(t *testing.T, tc scenario) turn.Work {
	t.Helper()
	launcher := &recordingLauncher{}
	launched, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{agentRun: launcher})
	if _, _, err := launched.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("box Execute: %v", err)
	}
	suspension, ok := launched.Suspended()
	if !ok {
		t.Fatal("the box-mode launch recorded no suspension")
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
			resume:   &runner.Resume{State: state, Answer: tc.text, Bridged: bridgedRun(tc.bridged)},
		})
	w, _, err := resumed.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("box Resume: %v", err)
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
	if want.Text != "" && got.Text != want.Text {
		t.Errorf("%s: text = %q, want %q", runtime, got.Text, want.Text)
	}
	if got.Rescued != want.Rescued {
		t.Errorf("%s: rescued = %v, want %v", runtime, got.Rescued, want.Rescued)
	}
	if want.Rescued && got.Summary != "" {
		// The executor gave no account of itself; the engine must not
		// write one for it — see the rescue path in finishWork.
		t.Errorf("%s: a rescue carries an intent nobody gave: %q", runtime, got.Summary)
	}
	if len(want.Deliveries) > 0 && len(got.Deliveries) != len(want.Deliveries) {
		t.Errorf("%s: deliveries = %v, want %v", runtime, got.Deliveries, want.Deliveries)
	}
}
