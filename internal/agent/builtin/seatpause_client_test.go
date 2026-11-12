package builtin_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD BOUNDS A PAUSE'S REASON AT THE ENGINE'S CAP.
//
// The pause dialog asks why and holds Pause while the reason is past
// [builtin.MaxPauseReasonRunes], which `pause_seat` refuses, so the reader is
// told at the field rather than by a refusal after the press. A figure above
// the engine's sends a reason it refuses; one below refuses a reason it takes.
//
// Mutation: move either figure and this fails.
func TestTheDashboardBoundsAPauseReasonAtTheEnginesCap(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "PAUSE_REASON_MAX_RUNES")
	if err != nil {
		t.Fatal(err)
	}
	if raw != strconv.Itoa(builtin.MaxPauseReasonRunes) {
		t.Errorf("the dashboard bounds a pause's reason at %s characters and the engine at %d: "+
			"change PAUSE_REASON_MAX_RUNES in contract/actions.ts", raw, builtin.MaxPauseReasonRunes)
	}
}
