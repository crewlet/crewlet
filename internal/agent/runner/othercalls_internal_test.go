package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// A FRESH RUN IS TOLD NO OTHER CALLS. A run that starts a session of its own has
// no session above, so nothing it may show is said to be none of the listed
// calls, however the turn began: its brief's late section says each call is
// ONE call, which elsewhere brings the section, and the turn continued an
// earlier turn's session. prompts.CalledBefore renders no such section either,
// so it is held here, where the value is decided.
//
// Mutation: ignore toldAs.fresh, and the fresh run's telling carries the
// earlier turns' part.
func TestAFreshRunIsToldNoOtherCalls(t *testing.T) {
	t.Parallel()
	r := &Runner{cfg: Config{Resume: &Resume{Base: &BaseBasis{Turn: "t-before"}}}}
	// A LATE SECTION OF ONE DROPPED CALL ALONE: it claims a call may be shown
	// twice, and lists nothing the fresh run's surface would have to read.
	round := roundCalls{of: 2, elided: []ledger.Elided{{Name: "slack_post", Calls: 1}}, lateElided: 1}
	for name, tc := range map[string]struct {
		as   toldAs
		want prompts.OtherCalls
	}{
		"a run of its own":            {as: toldAs{fresh: true}},
		"a session that continued it": {as: toldAs{}, want: prompts.OtherCalls{EarlierTurns: true}},
	} {
		out, _ := r.calledAfter(t.Context(), round, new(0), nil, nil, tc.as, nil, "")
		if out.LateDropped != 1 {
			t.Fatalf("%s: the dropped late call is not counted, so the case tests nothing: %+v", name, out)
		}
		if out.Other.EarlierTurns != tc.want.EarlierTurns || out.Other.Fire || out.Other.Unlisted.Unnamed {
			t.Errorf("%s: the other calls told are %+v, want %+v", name, out.Other, tc.want)
		}
	}
}
