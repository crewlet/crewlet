package sandbox

import (
	"fmt"
	"testing"
)

// A LONG RUN'S LOG KEEPS A CALL NOBODY WAITED FOR AS IT KEEPS A LATE ONE, AND
// COUNTS ONE IT DROPS. Past [MaxBridgeCalls] the log drops from its middle, and
// a call it drops keeps nothing but its identity — so not the mark that says
// its harness had stopped waiting for it ([BridgeCall.Abandoned]), which no
// save point holds, while a save point past its number would hold the call
// dropped. So the middle gives such a call up only once no ordinary call is
// left there, as it gives up a late one, and counts each it does give up
// ([BridgeLog.LateElided]), which every continuation of the round is told past
// whatever its save point.
//
// Mutation: read a call nobody waited for as an ordinary one where the middle
// drops, and it is the first dropped and is not counted.
func TestALongRunsLogKeepsACallNobodyWaitedForAndCountsOneItDrops(t *testing.T) {
	t.Parallel()
	t.Run("an ordinary call left in the middle", func(t *testing.T) {
		t.Parallel()
		var l BridgeLog
		for i := range 2 * MaxBridgeCalls {
			call := BridgeCall{Name: fmt.Sprintf("read_%d", i), Seq: i + 1}
			if i == MaxBridgeCalls/2+10 {
				call = BridgeCall{Name: "delegate", Seq: i + 1, Abandoned: true}
			}
			l.appendCall(call, nil)
		}
		kept := false
		for _, call := range l.Calls {
			kept = kept || (call.Name == "delegate" && call.Abandoned)
		}
		if !kept || l.Elided != MaxBridgeCalls || l.LateElided != 0 {
			t.Errorf("the log kept the call nobody waited for: %v, with %d calls dropped and %d of them counted "+
				"as held by no save point; want it kept, %d dropped and none counted", kept, l.Elided,
				l.LateElided, MaxBridgeCalls)
		}
	})
	t.Run("a middle of such calls alone", func(t *testing.T) {
		t.Parallel()
		var l BridgeLog
		for i := range MaxBridgeCalls + 1 {
			l.appendCall(BridgeCall{Name: fmt.Sprintf("read_%d", i), Seq: i + 1, Abandoned: i >= MaxBridgeCalls/2},
				nil)
		}
		if l.Elided != 1 || l.LateElided != 1 {
			t.Errorf("the log dropped %d calls and counts %d of them as held by no save point, want 1 and 1",
				l.Elided, l.LateElided)
		}
	})
}
