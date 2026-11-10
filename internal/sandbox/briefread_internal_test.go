package sandbox

import (
	"fmt"
	"slices"
	"testing"
)

// AN OPENING COUNTS LATE THE CALLS TAKEN AFTER ITS READ THAT THE MIDDLE DROPPED.
// An opening that carries the round on marks late every call the log took after
// the read its relaunch's briefs were built from ([BridgeLog.lateAfter]); one of
// those the log has dropped from its middle since keeps no field to mark, so
// the opening counts it late ([BridgeLog.LateElided]) — exactly, since every
// append is numbered: the appends past the read the log no longer lists. The
// calls the read saw are neither marked nor counted, and the log the opening
// copied is left as it was.
//
// Mutation: count none of them, and a call dropped before it could be marked
// leaves no record that it is late; mark the copy's list in place, and the log
// it was copied from is marked too.
func TestAnOpeningCountsLateTheCallsTakenAfterItsReadThatTheMiddleDropped(t *testing.T) {
	t.Parallel()
	var l BridgeLog
	for i := range MaxBridgeCalls + 3 {
		l.appendCall(BridgeCall{Name: fmt.Sprintf("read_%d", i), Seq: i + 1}, nil)
	}
	// THE MIDDLE'S OLDEST ORDINARY CALLS DROPPED: the log took 203, lists the
	// first hundred and the last, and dropped the 101st to the 103rd.
	for _, tc := range []struct{ told, counted, marked int }{
		{told: 100, counted: 3, marked: 100},
		{told: 103, counted: 0, marked: 100},
		{told: 150, counted: 0, marked: 53},
	} {
		got := l.lateAfter(tc.told)
		marked := 0
		for _, call := range got.Calls {
			if !call.Late {
				continue
			}
			marked++
			if call.Appended <= tc.told {
				t.Errorf("a read of %d marks the call the log took %d'th, which the read saw", tc.told,
					call.Appended)
			}
		}
		if got.LateElided != tc.counted || marked != tc.marked {
			t.Errorf("a read of %d marks %d listed calls and counts %d dropped ones late, want %d and %d",
				tc.told, marked, got.LateElided, tc.marked, tc.counted)
		}
	}
	if l.LateElided != 0 || slices.ContainsFunc(l.Calls, func(c BridgeCall) bool { return c.Late }) {
		t.Error("an opening's copy of the log marked the log it was copied from")
	}
}
