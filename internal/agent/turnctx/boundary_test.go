package turnctx_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
)

// TestEveryFenceOfATurnCarriesItsOneBoundary: the turn's token, through every
// copy a phase or a worker derives, so the prompt's thread and each tool answer
// of the segment are fenced alike.
func TestEveryFenceOfATurnCarriesItsOneBoundary(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	turn := &turnctx.Turn{Boundary: b}
	for name, derived := range map[string]*turnctx.Turn{
		"the turn":         turn,
		"a phase":          turn.InPhase(types.PhaseExecute),
		"a worker's calls": turn.WithCalls(turnctx.NewCallLog()),
		"another's tally":  turn.WithAuxSpend(nil),
	} {
		if got := derived.FenceBoundary(); got != b {
			t.Fatalf("%s fences with %q, want the turn's %q", name, got, b)
		}
	}
}

// TestAFenceOutsideATurnStillCarriesAnUnguessableToken: no turn, or a turn
// built without a token, fences each read with a fresh one rather than with an
// empty token any guess would close.
func TestAFenceOutsideATurnStillCarriesAnUnguessableToken(t *testing.T) {
	t.Parallel()
	var none *turnctx.Turn
	for name, turn := range map[string]*turnctx.Turn{
		"no turn":         none,
		"a turn without":  {},
		"a malformed one": {Boundary: "not-a-token"},
	} {
		first, second := turn.FenceBoundary(), turn.FenceBoundary()
		if !first.Valid() || !second.Valid() {
			t.Fatalf("%s fences with %q and %q", name, first, second)
		}
		if first == second {
			t.Fatalf("%s reused %q across two reads with no turn to hold it", name, first)
		}
	}
}
