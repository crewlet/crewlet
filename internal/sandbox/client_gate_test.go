package sandbox_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// clientNumber reads one of the dashboard's numeric declarations.
func clientNumber(t *testing.T, name string) int64 {
	t.Helper()
	raw, err := clientsource.Scalar(clientsource.Tree(t), name)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(raw, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("%s = %q is not a whole number: %v", name, raw, err)
	}
	return n
}

// THE DASHBOARD KNOWS EVERY OUTCOME A TAIL ANSWERS, both ways. An outcome it
// does not know is drawn as whatever its last branch is — `launching` was a
// run "no longer running" — and one it names that the engine never sends is a
// branch nothing reaches.
func TestTheDashboardKnowsEveryTailOutcome(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "SANDBOX_TAIL_OUTCOMES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	var engine []string
	for _, o := range sandbox.TailOutcomes {
		engine = append(engine, string(o))
	}
	slices.Sort(client)
	slices.Sort(engine)
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard names the outcomes %v and the engine answers %v — change "+
			"contract/sandbox.ts to the engine's set", client, engine)
	}
}

// THE LIVE VIEW HOLDS WHAT THE RECORD WILL HOLD. The owner sends a viewer at
// most this much at once — a reset is the last [sandbox.MaxRunTextBytes] — and
// answers a viewer further behind than this with a reset, so a view that held
// less would drop what it was just sent, and one that held more would hold text
// the owner can no longer prove it follows.
func TestTheDashboardHoldsWhatTheRecordWillHold(t *testing.T) {
	t.Parallel()
	if got := clientNumber(t, "LIVE_OUTPUT_MAX_BYTES"); got != sandbox.MaxRunTextBytes {
		t.Errorf("the dashboard holds %d bytes of live output and the engine bounds it at %d — "+
			"change it in contract/sandbox.ts to the engine's figure", got, sandbox.MaxRunTextBytes)
	}
}

// THE DASHBOARD POLLS PAST THE OWNER'S REUSE WINDOW AND ITS READ BUDGET. Inside
// the reuse window a single viewer would be answered from the read before its
// last one, its poll showing nothing new every other time; inside the read
// budget a poll would begin before its last one's owner could answer.
func TestTheDashboardPollsPastTheOwnersReuse(t *testing.T) {
	t.Parallel()
	poll := time.Duration(clientNumber(t, "SANDBOX_TAIL_POLL_MS")) * time.Millisecond
	if poll <= sandbox.LiveReuse || poll <= sandbox.TailReadBudget {
		t.Errorf("the dashboard polls every %v, not past the owner's reuse window (%v) and the "+
			"read budget (%v)", poll, sandbox.LiveReuse, sandbox.TailReadBudget)
	}
}
