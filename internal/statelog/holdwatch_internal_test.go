package statelog

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// fixedListing is a register whose listing is fixed.
type fixedListing []coord.TrimHold

func (l fixedListing) Holds(context.Context) ([]coord.TrimHold, error) { return l, nil }

// THE WATCH FORGETS AN OWNER THE LISTING NO LONGER HOLDS, which bounds it by
// the holds standing now rather than every holder the duty ever saw.
func TestTheWatchForgetsAnOwnerTheListingNoLongerHolds(t *testing.T) {
	t.Parallel()
	hold := func(owner string) coord.TrimHold { return coord.TrimHold{Owner: owner, Revision: 7} }
	var w HoldWatch
	for _, l := range []fixedListing{{hold("a"), hold("b")}, {hold("b")}} {
		if _, _, err := w.Read(t.Context(), l); err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if _, kept := w.seen["a"]; kept || len(w.seen) != 1 {
		t.Fatalf("the watch holds %v after a listing of b alone: a released "+
			"owner's sighting is kept for the life of the duty", w.seen)
	}
}

// THE WATCH COUNTS ON A MONOTONIC CLOCK: its default reading carries one, so
// a step of the wall clock never ages a hold. In UTC it would not.
func TestTheWatchCountsOnAMonotonicClock(t *testing.T) {
	t.Parallel()
	if got := (&HoldWatch{}).clock(); got == got.Round(0) {
		t.Fatalf("the watch's default clock read %v with no monotonic reading: "+
			"a wall clock stepped forward would age every hold it watches", got)
	}
}
