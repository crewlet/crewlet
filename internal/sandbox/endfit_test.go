package sandbox

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A RECORDED END'S FAILURE IS FIT: the end the executor log is asked to record
// carries its failure as the log keeps it ([executorkeep.Excerpt]), within
// FailureMaxBytes and a credential straddling the cut redacted, however long
// the error a frame handed it.
//
// Mutation: redact the failure whole instead of fitting it, and the end is
// asked with a failure past its bound.
func TestARecordedEndsFailureIsFit(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		failure := strings.Repeat("the box said no ", 63) + "ghp_" + strings.Repeat("c", 36) + " and then " +
			strings.Repeat("more output ", 4<<10)
		if err := rig.coordinator.EndTurn(t.Context(), TurnEnd{Handle: "swe", TurnID: "t-gone", WorkKey: "wk-1",
			WorkSince: rig.now, Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
				Failure: failure}}); err != nil {
			t.Fatalf("EndTurn: %v", err)
		}
		ends := writes.endedRecords()
		want := executorkeep.Excerpt(failure, executorkeep.FailureMaxBytes)
		if len(ends) != 1 {
			t.Fatalf("asked the log to record %d ends, want 1", len(ends))
		}
		if got := ends[0].Ending.Failure; got != want || len(got) > executorkeep.FailureMaxBytes {
			t.Fatalf("the end's failure is %d bytes ending %q, want %q", len(got), got[max(0, len(got)-60):], want)
		}
	})
}
