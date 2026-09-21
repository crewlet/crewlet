package sandbox_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE RESERVE IS PER MESSAGE, AND ONE MESSAGE WITH ROOM IS ENOUGH.
//
// Driven here rather than only through the dispatcher because it is a pure
// fold over values: a rule reachable only through a queue, a screening and a
// turn is one nobody re-measures. The boundary is the assertion — one
// delivery either side of the reserve decides whether a parked run is offered
// the message at all.
//
// The case that matters most is the mixed partition. A conversation whose
// earlier message has been handed back until it is inside the reserve keeps
// collecting replies, and each arrives with a whole budget; folding those two
// into the SMALLEST refuses the fresh reply and keeps refusing every later
// one, which spends a person's answer on an ordinary turn while a coding run
// is still parked on the question it answers.
func TestOneMessageWithDeliveriesToSpareKeepsTheOfferOpen(t *testing.T) {
	t.Parallel()
	const reserve = sandbox.AnswerDeliveryReserve
	known := func(left int) sandbox.AnswerHeadroom {
		return sandbox.AnswerHeadroom{Left: left, Known: true}
	}
	unstated := sandbox.AnswerHeadroom{}

	for name, tc := range map[string]struct {
		perMessage []sandbox.AnswerHeadroom
		want       bool
	}{
		"one delivery outside the reserve": {[]sandbox.AnswerHeadroom{known(reserve + 1)}, true},
		"at the reserve":                   {[]sandbox.AnswerHeadroom{known(reserve)}, false},
		"one delivery left":                {[]sandbox.AnswerHeadroom{known(1)}, false},
		"none left at all":                 {[]sandbox.AnswerHeadroom{known(0)}, false},
		"a full budget":                    {[]sandbox.AnswerHeadroom{known(24)}, true},

		// A FRESH REPLY BESIDE A SPENT SIBLING is the shape the fold
		// exists for, in both orders so nothing depends on which one a
		// backend listed first.
		"a fresh reply after a spent sibling": {[]sandbox.AnswerHeadroom{known(1), known(24)}, true},
		"a fresh reply before a spent one":    {[]sandbox.AnswerHeadroom{known(24), known(1)}, true},

		// AND WHEN EVERY MESSAGE IS SPENT THE ROUTE LETS GO. "Any" is
		// not "always": without this the per-message read would simply
		// have removed the bound.
		"every message inside the reserve": {[]sandbox.AnswerHeadroom{known(reserve), known(2)}, false},

		// AN ABSENT COUNT IS NOT A SPENT ONE. A transport that did not
		// say leaves the route as it was before this clause existed,
		// bounded by the per-process attempt count alone — reading
		// silence as "no headroom" spends the reply on the exact
		// failure the route exists to prevent.
		"nothing stated at all":        {[]sandbox.AnswerHeadroom{unstated}, true},
		"no messages at all":           {nil, true},
		"one unstated beside a spent":  {[]sandbox.AnswerHeadroom{known(1), unstated}, true},
		"an unstated zero is unstated": {[]sandbox.AnswerHeadroom{{Left: 0, Known: false}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := sandbox.MayOfferAnswer(tc.perMessage); got != tc.want {
				t.Errorf("MayOfferAnswer(%v) = %v, want %v", tc.perMessage, got, tc.want)
			}
		})
	}
}
