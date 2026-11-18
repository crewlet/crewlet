package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// A FRESH RUN IS TOLD NO OTHER CALLS: with no session above it, nothing it may
// show is said to be none of the listed calls, even where its late section and
// a continued earlier turn's session would bring that section elsewhere.
// prompts.CalledBefore renders none either, so it is held here, where the value
// is decided.
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
		out, _, _ := r.calledAfter(t.Context(), round, new(0), nil, nil, tc.as, nil, "")
		if out.LateDropped != 1 {
			t.Fatalf("%s: the dropped late call is not counted, so the case tests nothing: %+v", name, out)
		}
		if out.Other.EarlierTurns != tc.want.EarlierTurns || out.Other.Fire || out.Other.Unlisted.Unnamed {
			t.Errorf("%s: the other calls told are %+v, want %+v", name, out.Other, tc.want)
		}
	}
}

// A SPAN HOLDING THE TOLD ROUND IS NO PARTNER OF ITS CALLS. The park's last
// tier folds every closed round, the told one included, into one span; its
// tally counts the round's own calls, so read as work already done it would
// mark every told call against itself. A span of earlier rounds alone is work
// already done like any other.
//
// Mutation: read a span as standing for its first round alone, or take the
// entry standing for the round as a partner, and the first case is marked.
func TestASpanHoldingTheToldRoundIsNoPartnerOfItsCalls(t *testing.T) {
	t.Parallel()
	span := func(through int) ledger.Iteration {
		return ledger.Iteration{Iteration: 1, Through: through, Elided: []ledger.Elided{{Name: "slack_post",
			Calls: 3}}}
	}
	told := BridgedCall{Call: ledger.Call{Name: "slack_post", Args: map[string]any{"text": "hi"}}, Late: true}
	opts := ledger.Format(nil, nil)
	r := &Runner{}
	for name, tc := range map[string]struct {
		rendered []ledger.Iteration
		want     bool
	}{
		"a span holding the round": {rendered: []ledger.Iteration{span(2)}},
		"a span of earlier rounds": {rendered: []ledger.Iteration{span(1)}, want: true},
		"no rendered round":        {},
	} {
		if got := r.partnersOf(2, toldAs{rendered: tc.rendered}).couldBeTakenFor(told, opts); got != tc.want {
			t.Errorf("%s: a partner could be taken for the told post: %t, want %t", name, got, tc.want)
		}
	}
}
