package statelog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FETCH WAIT FITS INSIDE THE READ BUDGET, at a quarter of it: a request
// lost without an answer holds a record for [statelog.FetchWait] plus the
// queue's grace before the client asks again, and a `linearizable` read
// waiting on that record must still be served inside [statelog.ReadBudget].
// The relationship is what matters, not either number.
func TestTheFetchWaitFitsInsideTheReadBudget(t *testing.T) {
	t.Parallel()
	if statelog.FetchWait*4 > statelog.ReadBudget {
		t.Errorf("FetchWait is %v against a ReadBudget of %v — a reader that "+
			"arrives just after an idle fetch parks waits that long before its "+
			"record is even collected, so a level that appends a barrier "+
			"refuses `behind` on a healthy fleet",
			statelog.FetchWait, statelog.ReadBudget)
	}
	// AND THE LINGER IS NOT LONGER THAN THE WAIT: past it, a half-full batch
	// would commit slower than an empty one, inverting the point of holding it.
	if statelog.ApplyLinger > statelog.FetchWait {
		t.Errorf("ApplyLinger is %v against a FetchWait of %v, so a partial "+
			"run waits longer than an empty applier does",
			statelog.ApplyLinger, statelog.FetchWait)
	}
}

// A PULL CAN ALWAYS HOLD THE LARGEST RECORD A LOG CAN CARRY: the broker
// refuses, delivering nothing, a pull whose byte bound is below the next
// record, so a smaller bound stops every node's applier at the first record
// that size for ever. The kilobyte of margin covers what the broker counts
// beside the payload (subjects and headers) many times over.
func TestAPullCanAlwaysHoldTheLargestRecord(t *testing.T) {
	t.Parallel()
	if statelog.FetchBytes < queue.MaxPayloadBytes+1<<10 {
		t.Errorf("FetchBytes is %d and the largest record a log carries is %d "+
			"plus its envelope — a pull bounded below it is refused with "+
			"nothing delivered, and the applier stops at that record for ever",
			statelog.FetchBytes, queue.MaxPayloadBytes)
	}
}
