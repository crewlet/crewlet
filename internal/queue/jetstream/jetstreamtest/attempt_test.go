package jetstreamtest_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// ONE ATTEMPT CANNOT SPEND EVERY ATTEMPT'S BUDGET.
//
// [jetstreamtest.ClusterStartBudget] bounds the SEQUENCE, because "n attempts
// at an unbounded cost each is a product nobody declared". A ceiling on the
// sequence with no term per attempt is that same defect wearing the other
// face: the first attempt is handed the whole remaining budget, so one that
// hangs consumes every retry before any of them runs.
//
// Measured on this repository's own CI, which is why this is a test rather
// than a tidy: `TestAFleetAgreesAboutOneCompany` failed after 181.05s with
// "no cluster came up within 3m0s (1 of 4 attempts)". Member 0's broker spent
// the entire ceiling retrying a route to a port where nothing was listening,
// and the three attempts that exist to absorb exactly that never ran — the
// harness's own remedy, "trying again with DIFFERENT NUMBERS", unreachable
// from inside the failure it is for.
func TestOneAttemptCannotSpendTheWholeBudget(t *testing.T) {
	t.Parallel()

	start := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	ceiling := start.Add(jetstreamtest.ClusterStartBudget)

	// THE FIRST ATTEMPT IS CUT AT ITS TERM, not at the ceiling. This is the
	// assertion the defect fails: before it, the first attempt's deadline WAS
	// the ceiling.
	first := jetstreamtest.StartAttemptEnd(start, ceiling)
	if !first.Before(ceiling) {
		t.Errorf("the first attempt may run until %s and the whole budget "+
			"ends at %s — one hung attempt then consumes every retry",
			first.Sub(start), jetstreamtest.ClusterStartBudget)
	}
	if got := first.Sub(start); got != jetstreamtest.ClusterStartTerm {
		t.Errorf("the first attempt's term is %s, want %s", got,
			jetstreamtest.ClusterStartTerm)
	}

	// AND EVERY ATTEMPT FITS. The term is the budget divided by the attempts,
	// so the four of them are exactly the ceiling and no try is nominal.
	if got := jetstreamtest.ClusterStartTerm * jetstreamtest.ClusterStartAttempts; got != jetstreamtest.ClusterStartBudget {
		t.Errorf("%d attempts of %s is %s, and the budget is %s — a term that "+
			"does not divide the budget either wastes tries or overruns the "+
			"ceiling", jetstreamtest.ClusterStartAttempts,
			jetstreamtest.ClusterStartTerm, got, jetstreamtest.ClusterStartBudget)
	}

	// AND THE CEILING STILL WINS AT THE END. The last attempt must not run
	// past the budget just because its term has room: the ceiling is the
	// promise to the package timeout, and the term is only how the tries are
	// shared out inside it.
	late := ceiling.Add(-time.Second)
	if got := jetstreamtest.StartAttemptEnd(late, ceiling); !got.Equal(ceiling) {
		t.Errorf("an attempt starting %s before the ceiling may run until %s, "+
			"past the budget", ceiling.Sub(late), got.Sub(late))
	}

	// AND AN EXPIRED BUDGET GRANTS NOTHING. The loop checks the ceiling after
	// an attempt, so this is reached only on a clock that moved under it —
	// and a deadline in the past is what makes that attempt fail immediately
	// rather than run a full term past the bound.
	past := ceiling.Add(time.Second)
	if got := jetstreamtest.StartAttemptEnd(past, ceiling); !got.Equal(ceiling) {
		t.Errorf("an attempt starting after the ceiling was granted until %s",
			got)
	}
}
