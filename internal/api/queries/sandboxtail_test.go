package queries_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// recordingTails answers every request with the outcome it is told to, and
// remembers what it was asked.
type recordingTails struct {
	asked  [][2]string
	answer sandbox.TailAnswer
}

func (r *recordingTails) Tail(_ context.Context, turnID, launchID string) (sandbox.TailAnswer, error) {
	r.asked = append(r.asked, [2]string{turnID, launchID})
	return r.answer, nil
}

// A TAIL NAMES ITS JOB, not only its turn: a turn may launch more than one job,
// and a request naming only the turn would show whichever the run's record
// holds now — a different job from the span a person opened, the moment a
// second launch replaces the first.
func TestASandboxTailNeedsTheTurnAndTheJob(t *testing.T) {
	t.Parallel()
	tails := &recordingTails{answer: sandbox.TailAnswer{
		Outcome: sandbox.TailOwnerSilent, TurnID: "t1", LaunchID: "l1", Node: "n2",
	}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{SandboxTail: tails})

	for _, params := range []map[string]any{
		{"turn_id": "t1"}, {"launch_id": "l1"}, {"turn_id": " ", "launch_id": "l1"},
	} {
		if _, err := r.Answer(t.Context(), "sandbox_tail", params, "operator"); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("sandbox_tail %v = %v; want a bad-params refusal", params, err)
		}
	}
	got, err := r.Answer(t.Context(), "sandbox_tail", map[string]any{"turn_id": "t1", "launch_id": "l1"}, "operator")
	if err != nil {
		t.Fatalf("sandbox_tail: %v", err)
	}
	answer, ok := got.(sandbox.TailAnswer)
	if !ok || answer.Outcome != sandbox.TailOwnerSilent || answer.Node != "n2" {
		t.Errorf("answer = %#v; want the reader's own, the silent owner named", got)
	}
	if len(tails.asked) != 1 || tails.asked[0] != [2]string{"t1", "l1"} {
		t.Errorf("the reader was asked %v; want exactly (t1, l1)", tails.asked)
	}
}
