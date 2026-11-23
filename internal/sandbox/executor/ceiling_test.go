package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A STATED CEILING LEAVES THE BRIDGE TIME TO ANSWER BEFORE IT. The bridge
// answers a call still running by the tool loop's clock, a tenth of the
// ceiling ahead of it and at least five seconds (internal/api/mcpbridge, "A
// call longer than its harness waits"); a ceiling that clock leaves no time
// under would have every call answered the moment it starts. And at least one
// runner states one, Kimi Code's harness giving a call up at 300 seconds, so
// the check holds something.
//
// Mutation: state Kimi Code's ceiling as five seconds, and the bridge answers
// its every call at once.
func TestAStatedCeilingLeavesTheBridgeTimeToAnswerBeforeIt(t *testing.T) {
	t.Parallel()
	stated := 0
	for _, h := range executor.Registered() {
		ceiling := h.MCPCallCeiling()
		if ceiling <= 0 {
			continue
		}
		stated++
		if toolloop.ParkBefore(ceiling) <= 0 {
			t.Errorf("the %s runner states a ceiling of %v for one call, which leaves the bridge no time to "+
				"answer a call before it: every call would be answered still running as it starts",
				h.Name(), ceiling)
		}
	}
	if stated == 0 {
		t.Error("no runner states a ceiling, though Kimi Code's harness gives a call up at 300 seconds")
	}
}
