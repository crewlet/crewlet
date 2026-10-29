package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/structured"
)

// A SUBMISSION MADE BEFORE THE RUN ASKED IS NOT THE CARRIED-ON RUN'S. A run
// carried on by a person's answer keeps the calls it made before its question
// (ADR-0068), and a `submit_work` among them was made before it knew the
// answer: replayed, the turn would report that submission for the work the run
// did after it, or for a run that never submitted again. A submission made
// after the question is the run's own, and stands.
//
// Mutation: replay every submission whatever came after it, and the first case
// reports the work submitted before the question.
func TestASubmissionBeforeTheRunAskedIsNotReplayed(t *testing.T) {
	t.Parallel()
	const asked = "ask_and_wait"
	submit := func(summary string) ledger.Call {
		return ledger.Call{Name: SubmitWorkTool, Args: map[string]any{"summary": summary}}
	}
	ask := ledger.Call{Name: asked, Args: map[string]any{"question": "Which branch?"}}
	refusedAsk := ledger.Call{Name: asked, Failed: true}
	for _, tc := range []struct {
		name   string
		calls  []ledger.Call
		want   string
		answer bool
	}{
		{"submitted, then asked", []ledger.Call{submit("before"), ask}, "", false},
		{"asked, then submitted", []ledger.Call{submit("before"), ask, submit("after")}, "after", true},
		{"a refused ask ends nothing", []ledger.Call{submit("before"), refusedAsk}, "before", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
				func(args map[string]any) (workPayload, error) {
					summary, _ := args["summary"].(string)
					return workPayload{Summary: summary}, nil
				})
			replaySubmission(t.Context(), tool, tc.calls, asked)
			got, submitted := tool.Value()
			if submitted != tc.answer || got.Summary != tc.want {
				t.Fatalf("replayed %q (submitted %v), want %q (submitted %v)",
					got.Summary, submitted, tc.want, tc.answer)
			}
		})
	}
}
