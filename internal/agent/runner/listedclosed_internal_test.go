package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// A FRESH RUN LISTS NOTHING AGAIN. A run starting a session of its own
// (prompts.CalledBefore) has no session above that could show a closed round's
// call, and renders closed rounds as work already done; a session that
// continued through them lists an alike closed call again.
//
// Mutation: ignore toldAs.fresh, and the fresh run lists the closed post again.
func TestAFreshRunListsNothingAgain(t *testing.T) {
	t.Parallel()
	args := map[string]any{"channel": "C1", "text": "hi"}
	closed := ledger.Call{Name: "slack_post", Args: args, Result: "done", Digest: turnctx.ArgsDigest(args)}
	history := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{closed}}}
	late := closed
	late.Late = true
	told := []BridgedCall{{Call: late, Seq: 1, Appended: 1}}
	opts := ledger.Format(nil, nil)
	r := &Runner{}
	for name, tc := range map[string]struct {
		as   toldAs
		want int
	}{
		"a run of its own":            {as: toldAs{fresh: true}, want: 0},
		"a session that continued it": {as: toldAs{}, want: 1},
	} {
		again, closedN := listedAgain(r.closedShown(history, 2, tc.as), nil, told, opts)
		if len(again) != tc.want || closedN != tc.want {
			t.Errorf("%s: %d closed calls listed again (%d counted closed), want %d", name, len(again), closedN,
				tc.want)
		}
	}
}

// A CLOSED WITHHELD CALL OF ANOTHER ROUND THAN A LEAD'S IS STILL LISTED AGAIN.
// A lead leaves out the withheld calls of its round and tool, since a closed
// list keeps no held call's id; a withheld call of the tool in another round
// is another held call, whose fate no lead tells and the session may show.
//
// Mutation: leave out a withheld call of the lead's tool whatever its round,
// and round 2's is no longer set beside the told calls.
func TestAClosedWithheldCallOfAnotherRoundThanALeadsIsStillListedAgain(t *testing.T) {
	t.Parallel()
	held := ledger.Call{Name: "vendor_post", Withheld: true, Failed: true, Result: "held for a person's approval"}
	history := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{held}},
		{Iteration: 2, Calls: []ledger.Call{held}}}
	got := (&Runner{}).closedShown(history, 3, toldAs{leads: []toldHeld{{id: "held-1", tool: "vendor_post",
		round: 1}}})
	if len(got) != 1 {
		t.Errorf("%d closed withheld calls are set beside the told ones, want round 2's alone: %+v", len(got), got)
	}
}
