package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// AN OWED READ AND A LATE CALL LEFT OUT, BOTH DROPPED SINCE, ARE EACH COUNTED
// ONCE. An earlier brief counted, without listing them, a read at place 3 (owed)
// and a late call at place 5 (apart, [omittedReads]); the log has since dropped
// both from its middle, the late one counted late (sandbox.BridgeLog.LateElided). The
// brief says one owed read was dropped and one late call was, EXACTLY the two
// calls the log dropped: the late call is never counted among the owed reads
// too, as it was when the two were one set.
//
// Mutation: count the late calls left out among the owed reads dropped
// (owed-gone-counts-late), and the brief counts three calls dropped for two.
func TestAnOwedReadAndALateCallTheLogDroppedAreEachCountedOnce(t *testing.T) {
	t.Parallel()
	r := &Runner{cfg: Config{Resume: &Resume{}}}
	round := roundCalls{of: 1, elided: []ledger.Elided{{Name: "slack_history", Calls: 2}}, lateElided: 1}
	out, _, _ := r.calledAfter(t.Context(), round, nil, []int{3}, nil, toldAs{lateOwed: []int{5}}, nil, "")
	if out.OwedDropped != 1 || out.LateDropped != 1 {
		t.Errorf("the brief counts %d owed reads and %d late calls dropped, want one each: the log dropped "+
			"%d calls", out.OwedDropped, out.LateDropped, ledger.Count(round.elided))
	}
}
