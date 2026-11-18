package runner_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// THE PHASE SEAM AND THE PHASE EVENT ARE ONE ANNOUNCEMENT. The seam tells
// the working indicator which phase a seat is in, and is invoked at the call
// that publishes agent_phase_started, so a chat thread and a dashboard
// cannot disagree (one saying `review` while the other says `execute`).
func TestThePhaseSeamNamesThePhaseTheEventNames(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	var seen []phase.Phase
	// One seam across launch and collect: a chat thread has one indicator
	// per turn.
	round := executeRound(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}}}, buildOpts{
		pub:     pub,
		onPhase: func(ph phase.Phase) { seen = append(seen, ph) },
	}, 1, "", nil, run{calls: []llm.ToolCall{submitWork(t)}}, runner.RunRecord{})
	if _, err := round.collector.Review(context.Background(), 1, round.work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	if !slices.Equal(seen, []phase.Phase{phase.Execute, phase.Review}) {
		t.Fatalf("the seam reported %v, want the executor then the reviewer", seen)
	}
	// Compared as wire strings: what a dashboard reads and the phrase pools
	// are keyed on.
	var wire []string
	for _, ph := range seen {
		wire = append(wire, ph.String())
	}
	if published := pub.startedPhases(); !slices.Equal(wire, published) {
		t.Fatalf("the seam said %v and the events said %v", wire, published)
	}
}

// AN INDICATOR IS NOT TELEMETRY, so the seam fires on a runner that publishes
// nothing: a node with no broker wired still has a person watching a chat
// thread.
func TestThePhaseSeamFiresWithNothingListening(t *testing.T) {
	t.Parallel()
	var seen []phase.Phase
	launch(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{onPhase: func(ph phase.Phase) { seen = append(seen, ph) }}, 1, "", nil)
	if !slices.Equal(seen, []phase.Phase{phase.Execute}) {
		t.Fatalf("the seam reported %v on a runner with no publisher", seen)
	}
}

// NO SEAM IS THE ORDINARY CASE and must cost the phase nothing: every runner
// but a chat-triggered turn's runs with none.
func TestAPhaseRunsWithNoSeamWired(t *testing.T) {
	t.Parallel()
	round := executeRound(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{}, 1, "", nil, run{calls: []llm.ToolCall{submitWork(t)}}, runner.RunRecord{})
	if round.work.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %q", round.work.Outcome)
	}
}
