package runner_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE PHASE SEAM AND THE PHASE EVENT ARE ONE ANNOUNCEMENT.
//
// The working indicator has to say which phase a seat is in, and the frame
// that owns a turn's lifetime cannot see one: a phase opens several layers
// below the engine. This seam is what tells it — and it is invoked at the call
// that publishes agent_phase_started rather than beside it, so the words a
// person watching a chat thread reads and the phase every dashboard reads
// cannot drift. Wired anywhere else, one surface could say `review` while the
// other still said `execute`, and nothing would be wrong enough to fail.
func TestThePhaseSeamNamesThePhaseTheEventNames(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	var seen []phase.Phase
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: []llm.Completion{submitWork(t)},
		review:  []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}}}, buildOpts{
		pub:     pub,
		onPhase: func(ph phase.Phase) { seen = append(seen, ph) },
	})

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// The turn's own two passes, in the order they ran.
	if !slices.Equal(seen, []phase.Phase{phase.Execute, phase.Review}) {
		t.Fatalf("the seam reported %v, want the executor then the reviewer", seen)
	}
	// And the SAME names the events carry. Compared as the wire strings,
	// because that is what a dashboard reads and what the phrase pools are
	// keyed on: a phase whose seam said one word and whose event said
	// another would show a reader something no screen could corroborate.
	var wire []string
	for _, ph := range seen {
		wire = append(wire, ph.String())
	}
	if published := pub.startedPhases(); !slices.Equal(wire, published) {
		t.Fatalf("the seam said %v and the events said %v", wire, published)
	}
}

// AN INDICATOR IS NOT TELEMETRY, so the seam fires on a runner that publishes
// nothing at all.
//
// A node whose phases are silent — no broker wired, a test driving a runner
// directly — still has a person watching a chat thread. Gating this on the
// publisher would make what a reader sees depend on whether the engine's
// observability happened to be wired, which is a question they cannot ask.
func TestThePhaseSeamFiresWithNothingListening(t *testing.T) {
	t.Parallel()
	var seen []phase.Phase
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: []llm.Completion{submitWork(t)},
	}}}, buildOpts{onPhase: func(ph phase.Phase) { seen = append(seen, ph) }})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !slices.Equal(seen, []phase.Phase{phase.Execute}) {
		t.Fatalf("the seam reported %v on a runner with no publisher", seen)
	}
}

// NO SEAM IS THE ORDINARY CASE and must cost the phase nothing: every runner
// but a chat-triggered turn's runs with none.
func TestAPhaseRunsWithNoSeamWired(t *testing.T) {
	t.Parallel()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: []llm.Completion{submitWork(t)},
	}}}, buildOpts{})

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %q", w.Outcome)
	}
}
