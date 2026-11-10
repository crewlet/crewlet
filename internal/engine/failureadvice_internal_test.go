package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

// EVERY FAILURE THAT PAUSES A SEAT HAS THE ADVISOR'S FIX: the owner is told what
// to change for each kind in the table (ADR-0117), never left with a paused agent.
func TestEveryFailurePauseHasTheAdvisorsFix(t *testing.T) {
	t.Parallel()
	for kind := range failureRules {
		if _, ok := advisor.PausedFailureClass(string(kind)); !ok {
			t.Errorf("a seat paused for %q is given no fix by the advisor", kind)
		}
	}
}
