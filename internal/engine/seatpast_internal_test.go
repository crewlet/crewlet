package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A SEAT'S STOPS ARE ITS PAUSES AND PARKS, EACH FROM ITS FIRST EVENT TO ITS END
// (ADR-0162).
//
// The flow backfill reads a seat's past from the events the fleet keeps: a
// park is announced again at each probe that meets the limit, which extends
// the stop rather than beginning one; a stop still on reads with no end; and
// an end whose beginning has aged out of the history ends a stop that began
// before the floor, rather than leaving the seat read as free.
func TestASeatsStopsAreItsPausesAndParks(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return floor.Add(time.Duration(h) * time.Hour) }
	marks := []seatMark{
		{at: at(9), cause: worklet.FlowCauseLimit, stops: true},
		{at: at(2), cause: worklet.FlowCausePaused, stops: false},
		{at: at(5), cause: worklet.FlowCauseLimit, stops: true},
		{at: at(3), cause: worklet.FlowCauseLimit, stops: true},
		{at: at(6), cause: worklet.FlowCauseLimit, stops: false},
		{at: at(7), cause: worklet.FlowCausePaused, stops: true},
	}
	got := seatStops(marks, floor)
	want := []worklet.SeatStop{
		{Cause: worklet.FlowCausePaused, From: floor, To: at(2)},
		{Cause: worklet.FlowCauseLimit, From: at(3), To: at(6)},
		{Cause: worklet.FlowCausePaused, From: at(7)},
		{Cause: worklet.FlowCauseLimit, From: at(9)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the seat's stops are %+v, want %+v", got, want)
	}
}
