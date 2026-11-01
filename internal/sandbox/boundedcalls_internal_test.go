package sandbox

import (
	"fmt"
	"testing"
)

// A LONG RUN'S LOG KEEPS THE CALLS THAT ENDED IT. Past [MaxBridgeCalls] the log
// drops from its middle, and it gives up its ordinary calls first: a question
// the run parked on or a submission it corrected itself with is what the
// round's record and its reviewer read the run by, so it outlives the reads
// around it. A log of nothing but such calls is still held to the cap.
//
// Mutation: drop the middle's oldest call whatever it is, and the ask in the
// middle is lost; drop only ordinary calls, and the all-submission log grows
// past its cap.
func TestALongRunsLogKeepsTheCallsThatEndedIt(t *testing.T) {
	t.Parallel()
	var (
		calls  []BridgeCall
		elided int
	)
	add := func(name string) {
		calls, elided = appendBounded(calls, elided, BridgeCall{Name: name, Seq: len(calls) + elided + 1})
	}
	for i := range MaxBridgeCalls {
		switch i {
		case MaxBridgeCalls/2 + 10:
			add(SubmitWorkTool)
		case MaxBridgeCalls/2 + 20:
			add(AskAndWaitTool)
		default:
			add(fmt.Sprintf("read_%d", i))
		}
	}
	for range MaxBridgeCalls {
		add("read_file")
	}
	if len(calls) != MaxBridgeCalls || elided != MaxBridgeCalls {
		t.Fatalf("the log holds %d calls with %d elided, want %d and %d",
			len(calls), elided, MaxBridgeCalls, MaxBridgeCalls)
	}
	kept := map[string]bool{}
	for _, call := range calls {
		kept[call.Name] = true
	}
	if !kept[SubmitWorkTool] || !kept[AskAndWaitTool] {
		t.Errorf("the log dropped a call that ended the run: submit kept %v, ask kept %v",
			kept[SubmitWorkTool], kept[AskAndWaitTool])
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].Seq <= calls[i-1].Seq {
			t.Fatalf("the log is out of order at %d: %d after %d", i, calls[i].Seq, calls[i-1].Seq)
		}
	}

	var only []BridgeCall
	onlyElided := 0
	for range 2 * MaxBridgeCalls {
		only, onlyElided = appendBounded(only, onlyElided, BridgeCall{Name: SubmitWorkTool})
	}
	if len(only) != MaxBridgeCalls || onlyElided != MaxBridgeCalls {
		t.Errorf("a log of submissions holds %d with %d elided, want it held to %d",
			len(only), onlyElided, MaxBridgeCalls)
	}
}
