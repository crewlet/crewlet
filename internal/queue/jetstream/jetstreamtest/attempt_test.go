package jetstreamtest_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// ONE ATTEMPT CANNOT SPEND EVERY ATTEMPT'S BUDGET.
//
// [jetstreamtest.ClusterStartBudget] bounds the SEQUENCE of attempts. A ceiling
// with no term per attempt hands the first attempt the whole remaining budget,
// so one that hangs (a broker retrying a route to a port where nothing
// listens) consumes every retry before any runs, and the harness's remedy,
// "trying again with DIFFERENT NUMBERS", is unreachable from the failure it
// is for.
func TestOneAttemptCannotSpendTheWholeBudget(t *testing.T) {
	t.Parallel()

	start := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	ceiling := start.Add(jetstreamtest.ClusterStartBudget)

	// THE FIRST ATTEMPT IS CUT AT ITS TERM, not at the ceiling. This is the
	// assertion the defect fails: before it, the first attempt's deadline WAS
	// the ceiling.
	first := jetstreamtest.StartAttemptEnd(start, ceiling)
	if ceiling.Sub(first) <= 0 { //fixedwait:stated the ceiling and the attempt's end computed from it, both on a fixed date
		t.Errorf("the first attempt may run until %s and the whole budget "+
			"ends at %s — one hung attempt then consumes every retry",
			first.Sub(start), jetstreamtest.ClusterStartBudget) //fixedwait:stated start, a fixed date, and the attempt's end computed from it
	}
	if got := first.Sub(start); got != jetstreamtest.ClusterStartTerm { //fixedwait:stated start, a fixed date, and the attempt's end computed from it
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
			"past the budget", ceiling.Sub(late), got.Sub(late)) //fixedwait:stated the ceiling, a second before it and the end computed from them, all on a fixed date
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
