package engine

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// A SEAT THAT TAKES NO WORK IS NOT WOKEN ABOUT ITS ATLAS ENTRY (ADR-0143):
// the duty asks before it claims the day's wake, and a paused seat, a seat
// parked on a spent budget, a person's seat and a handle the chart does not
// hold all answer no.
//
// Mutation: drop the pause or the budget reading in seatTakesWork and its
// case answers yes, so the seat's one wake of the day is spent on mail it
// cannot read.
func TestASeatThatCannotStartATurnTakesNoAtlasWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		handle        string
		paused, spent bool
		want          bool
	}{
		{name: "an agent seat with room", handle: "lead", want: true},
		{name: "a paused seat", handle: "lead", paused: true},
		{name: "a seat whose budget window is spent", handle: "lead", spent: true},
		{name: "a handle the chart does not hold", handle: "nobody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newParkRig(t, "100")
			if tc.spent {
				r.spend(t, 100)
			}
			pauses := map[uuid.UUID]coord.SeatPause{}
			if tc.paused {
				pauses[seatOf("lead")] = coord.SeatPause{Seat: seatOf("lead"), By: "jane", At: parkedAt}
			}
			r.e.adoptSeatPauses(t.Context(), pauses)
			got, err := r.e.seatTakesWork(t.Context(), tc.handle)
			if err != nil || got != tc.want {
				t.Fatalf("takes work = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

// WHAT CANNOT BE READ IS NOT "TAKES WORK": a node that has not read the
// pauses answers an error, and the duty leaves the seat for its next pass.
func TestASeatNobodyCanAskAboutIsUnknown(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	if got, err := r.e.seatTakesWork(t.Context(), "lead"); !errors.Is(err, errPausesUnread) || got {
		t.Fatalf("before the pauses are read, takes work = %v (%v), want unknown", got, err)
	}
}
