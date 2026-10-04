package statelog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FETCH WAIT IS A CEILING ON READ LATENCY, and nothing said so.
//
// A pull closes when it is FULL or when its wait expires; one record arriving
// does not end it. So a fetch of [statelog.FetchMessages]
// on a quiet log costs the WHOLE wait however fast the record got there —
// measured on a three-member cluster, an append acknowledges in about 500
// microseconds and the fetch that collects it still takes the full wait, five
// times out of five.
//
// That makes this constant the dominant term in how long a reader waits for a
// record it is waiting on. At five seconds it was longer than [ReadBudget], so
// a `linearizable` read appended its barrier and then refused `behind` before
// its own applier had collected it — every time, on every idle company. The
// three-node e2e went from failing to passing and from 45 seconds to 11 when
// this was brought under the budget.
//
// A QUARTER, so a reader can wait out one full fetch and still be served
// inside its budget with room for the apply transaction itself. The
// relationship is what matters rather than either number: whichever moves,
// this has to keep holding.
func TestTheFetchWaitFitsInsideTheReadBudget(t *testing.T) {
	t.Parallel()
	if statelog.FetchWait*4 > statelog.ReadBudget {
		t.Errorf("FetchWait is %v against a ReadBudget of %v — a reader that "+
			"arrives just after an idle fetch parks waits that long before its "+
			"record is even collected, so a level that appends a barrier "+
			"refuses `behind` on a healthy fleet",
			statelog.FetchWait, statelog.ReadBudget)
	}
	// AND THE LINGER IS NOT LONGER THAN THE WAIT. The linger is what a
	// PARTIAL run holds out for; longer than the idle wait it would make a
	// half-full batch slower to commit than an empty one, which inverts
	// the whole point of holding it.
	if statelog.ApplyLinger > statelog.FetchWait {
		t.Errorf("ApplyLinger is %v against a FetchWait of %v, so a partial "+
			"run waits longer than an empty applier does",
			statelog.ApplyLinger, statelog.FetchWait)
	}
}

// A PULL CAN ALWAYS HOLD THE LARGEST RECORD A LOG CAN CARRY.
//
// The broker refuses a pull whose byte bound is smaller than the next record,
// delivering nothing — so a bound below the largest payload the queue admits
// is an applier that stops at the first record that size and never moves
// again, on every node, with nothing wrong with the record. The margin is for
// what the broker counts beside the payload: the subject, the
// acknowledgement subject and the headers, which a single kilobyte covers
// many times over.
func TestAPullCanAlwaysHoldTheLargestRecord(t *testing.T) {
	t.Parallel()
	if statelog.FetchBytes < queue.MaxPayloadBytes+1<<10 {
		t.Errorf("FetchBytes is %d and the largest record a log carries is %d "+
			"plus its envelope — a pull bounded below it is refused with "+
			"nothing delivered, and the applier stops at that record for ever",
			statelog.FetchBytes, queue.MaxPayloadBytes)
	}
}
