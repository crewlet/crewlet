package sandbox

import (
	"testing"
)

// A WINDOW CARRIES A SESSION WHOSE FIRST REQUEST (its context plus the brief
// continuing it) FITS BENEATH ITS REPLY RESERVE.
//
// Mutation: compare the context alone, or against the whole window, and a row
// goes red.
func TestAWindowCarriesWhatFitsBeneathItsReplyReserve(t *testing.T) {
	t.Parallel()
	w := ExecutorWindow{Tokens: 200_000, Known: true, Reserve: 20_000}
	for name, c := range map[string]struct {
		context int64
		brief   int
		carries bool
	}{
		"well within":            {context: 150_000, brief: 5_000, carries: true},
		"at the reserve":         {context: 175_000, brief: 5_000, carries: true},
		"past it with the brief": {context: 178_000, brief: 5_000},
		"past the whole window":  {context: 210_000, brief: 1_000},
	} {
		if got := w.Carries(c.context, c.brief); got != c.carries {
			t.Errorf("%s: Carries(%d, %d) = %v, want %v", name, c.context, c.brief, got, c.carries)
		}
	}
}

// THE LEAST OF TWO WINDOWS TELLS MORE: the lower standard window, Known only
// where both are, the larger headroom and the heavier charge, and an unknown
// window's fallback standard never lowers a known one's.
//
// Mutation: take the larger standard window, keep Known from either, or take
// the lighter charge, and a case goes red.
func TestTheLeastOfTwoWindowsTellsMore(t *testing.T) {
	t.Parallel()
	wide := ExecutorWindow{Tokens: 1_000_000, Standard: 1_000_000, Known: true, Reserve: 20_000, Buffer: 13_000,
		Attachments: 20_000, Charge: 4}
	narrow := ExecutorWindow{Tokens: 1_000_000, Standard: 200_000, Known: true, Reserve: 30_000, Buffer: 10_000,
		Attachments: 5_000, Charge: 2}
	got := wide.Least(narrow)
	want := ExecutorWindow{Tokens: 1_000_000, Standard: 200_000, Known: true, Reserve: 30_000, Buffer: 13_000,
		Attachments: 20_000, Charge: 2}
	if got != want {
		t.Errorf("the least of %+v and %+v is %+v, want %+v", wide, narrow, got, want)
	}
	unknown := ExecutorWindow{Tokens: 200_000, Standard: 100_000}
	if got := wide.Least(unknown); got.Known || got.Standard != wide.Standard {
		t.Errorf("an unknown window lowered the standard or kept Known: %+v", got)
	}
}

// A CHARGE ROUNDS UP and counts nothing of nothing.
//
// Mutation: round down, or charge a negative size, and a case goes red.
func TestAChargeRoundsUp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		charge Charge
		n      int64
		want   int64
	}{{2, 10, 5}, {2, 11, 6}, {4, 1, 1}, {4, 0, 0}, {4, -3, 0}, {0, 10, 0}} {
		if got := c.charge.Tokens(c.n); got != c.want {
			t.Errorf("%d bytes at %d a token is %d, want %d", c.n, c.charge, got, c.want)
		}
	}
}
