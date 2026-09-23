package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// scriptedFloors is a listing of the published floors a case controls.
type scriptedFloors struct {
	rows []coord.TrimFloor
	err  error
}

func (s *scriptedFloors) read(context.Context) ([]coord.TrimFloor, error) {
	return s.rows, s.err
}

// floorReading is what one watch puts in front of the alarm table.
func floorReading(w *floorWatch) statelog.Reading {
	var out statelog.Reading
	w.fill(&out)
	return out
}

// AN UNREADABLE FLOOR IS AGED FROM ITS FIRST FAILING BEAT TO ITS LATEST, and
// `floor_unknown` fires once that has passed four heartbeats — not on the first
// failure, and not never.
//
// The form this replaced could not fire: the report stood the threshold itself
// in for the age, and only on a branch a failed floor read never reached.
func TestAnUnreadableFloorIsAgedFromItsFirstFailingBeat(t *testing.T) {
	t.Parallel()
	source := &scriptedFloors{err: errors.New("coordination unreachable")}
	w := newFloorWatch(source.read)
	subjects := []floorSubject{{domain: "tracker", generation: 1}, {domain: "pages", generation: 1}}
	t0 := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)

	w.observe(t.Context(), t0, subjects)
	if got := floorReading(w); got.FloorUnknownFor != 0 ||
		!strings.Contains(got.FloorUnknownCause, "coordination unreachable") {
		t.Fatalf("a first failing beat reads %+v; want an age of zero and the cause", got)
	}
	if fired(statelog.Evaluate(floorReading(w)), statelog.KindFloorUnknown) {
		t.Fatal("one failing beat raised floor_unknown")
	}

	// FOUR HEARTBEATS OF IT is the threshold, and is not past it.
	w.observe(t.Context(), t0.Add(statelog.FloorCacheStale), subjects)
	if fired(statelog.Evaluate(floorReading(w)), statelog.KindFloorUnknown) {
		t.Fatal("a floor unreadable for exactly the threshold raised floor_unknown")
	}
	// ONE MORE BEAT IS.
	w.observe(t.Context(), t0.Add(statelog.FloorCacheStale+statelog.AlarmInterval), subjects)
	reading := floorReading(w)
	if reading.FloorUnknownFor != statelog.FloorCacheStale+statelog.AlarmInterval {
		t.Errorf("the age is %s, want first failing beat to latest", reading.FloorUnknownFor)
	}
	// IN NAME ORDER when every domain failed together, so the detail names
	// the same log on every beat.
	if !strings.HasPrefix(reading.FloorUnknownCause, "pages: ") {
		t.Errorf("the cause is %q; one outage should name one log, the first by name",
			reading.FloorUnknownCause)
	}
	if !fired(statelog.Evaluate(reading), statelog.KindFloorUnknown) {
		t.Fatal("a floor unreadable a beat past four heartbeats raised no floor_unknown")
	}

	// A READ THAT SUCCEEDS CLEARS IT at once, and the next outage starts a
	// clock of its own rather than inheriting this one's age.
	source.err = nil
	w.observe(t.Context(), t0.Add(time.Hour), subjects)
	if got := floorReading(w); got.FloorUnknownFor != 0 || got.FloorUnknownCause != "" {
		t.Fatalf("a readable floor still reads %+v", got)
	}
	source.err = errors.New("coordination unreachable again")
	w.observe(t.Context(), t0.Add(2*time.Hour), subjects)
	if got := floorReading(w); got.FloorUnknownFor != 0 {
		t.Errorf("a new outage inherited an age of %s", got.FloorUnknownFor)
	}
}

// A FLOOR AHEAD OF THIS NODE IS UNKNOWN, ONE BEHIND IT IS NOT — the read path's
// own rule, so the alarm names exactly the floors every read refuses on.
//
// A floor published at a LATER generation means the fleet re-anchored the log
// and this node did not, and its positions name a sequence space nobody else
// is in: every read refuses. One at an EARLIER generation is a floor the trim
// has not re-published since the re-anchor, which [floorFor] reads as nothing
// licensed yet and serves.
func TestAFloorAheadOfThisNodeIsUnknownAndOneBehindIsNot(t *testing.T) {
	t.Parallel()
	source := &scriptedFloors{rows: []coord.TrimFloor{
		{Domain: "tracker", Generation: 3},
		{Domain: "pages", Generation: 1},
	}}
	w := newFloorWatch(source.read)
	t0 := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	subjects := []floorSubject{{domain: "tracker", generation: 2}, {domain: "pages", generation: 2}}
	w.observe(t.Context(), t0, subjects)
	w.observe(t.Context(), t0.Add(time.Hour), subjects)

	reading := floorReading(w)
	if !strings.HasPrefix(reading.FloorUnknownCause, "tracker: ") ||
		!strings.Contains(reading.FloorUnknownCause, "generation 3") {
		t.Errorf("the cause is %q; want the tracker's floor, ahead at generation 3",
			reading.FloorUnknownCause)
	}
	if reading.FloorUnknownFor != time.Hour {
		t.Errorf("the age is %s, want an hour", reading.FloorUnknownFor)
	}
}

// A SHUTDOWN IS NOT AN OUTAGE. A listing that failed because the beat's own
// context ended says nothing about coordination, so it starts no clock — and
// clears none, since a firing alarm must not be cleared by the node stopping.
func TestAStoppingBeatNeitherStartsNorClearsTheClock(t *testing.T) {
	t.Parallel()
	source := &scriptedFloors{err: errors.New("coordination unreachable")}
	w := newFloorWatch(source.read)
	subjects := []floorSubject{{domain: "tracker", generation: 1}}
	t0 := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)

	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	source.err = context.Canceled
	w.observe(stopped, t0, subjects)
	if got := floorReading(w); got.FloorUnknownCause != "" {
		t.Fatalf("a stopping beat started a clock: %+v", got)
	}

	source.err = errors.New("coordination unreachable")
	w.observe(t.Context(), t0, subjects)
	source.err = context.Canceled
	w.observe(stopped, t0.Add(time.Hour), subjects)
	if got := floorReading(w); got.FloorUnknownCause == "" {
		t.Fatal("a stopping beat cleared an outage it could say nothing about")
	}
}

// fired reports whether a table's answer carries an alarm of one kind.
func fired(alarms []statelog.Alarm, kind statelog.Kind) bool {
	for _, a := range alarms {
		if a.Kind == kind {
			return true
		}
	}
	return false
}
