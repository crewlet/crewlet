package sandbox_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE RESERVE IS PER MESSAGE, AND ONE MESSAGE WITH ROOM IS ENOUGH. The
// boundary is the assertion: one delivery either side of the reserve decides
// whether a parked run is offered the message at all. The case that matters
// most is the mixed partition: a conversation whose earlier message is inside
// the reserve keeps collecting replies with whole budgets, and folding to the
// SMALLEST refuses every one, spending a person's answer on an ordinary turn
// while an executor is still parked on the question it answers.
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

		// A FRESH REPLY BESIDE A SPENT SIBLING, in both orders so nothing
		// depends on which a backend lists first.
		"a fresh reply after a spent sibling": {[]sandbox.AnswerHeadroom{known(1), known(24)}, true},
		"a fresh reply before a spent one":    {[]sandbox.AnswerHeadroom{known(24), known(1)}, true},

		// WHEN EVERY MESSAGE IS SPENT THE ROUTE LETS GO: "any" is not
		// "always", or the per-message read would remove the bound.
		"every message inside the reserve": {[]sandbox.AnswerHeadroom{known(reserve), known(2)}, false},

		// AN ABSENT COUNT IS NOT A SPENT ONE: read as spent, the reply is
		// spent on an ordinary turn; the per-process attempt count alone
		// bounds the route.
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
