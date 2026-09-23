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

// floorReading is what one watch puts in front of the alarm table for one
// domain — the per-domain half the report evaluates.
func floorReading(w *floorWatch, domain string) statelog.Reading {
	var out statelog.Reading
	out.FloorUnknownFor, out.FloorUnknownCause = w.of(domain)
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
	if got := floorReading(w, "tracker"); got.FloorUnknownFor != 0 ||
		!strings.Contains(got.FloorUnknownCause, "coordination unreachable") {
		t.Fatalf("a first failing beat reads %+v; want an age of zero and the cause", got)
	}
	// AND THE CAUSE SAYS WHAT COULD NOT BE READ, in the read path's own
	// words, rather than handing on a transport's error as the whole of it.
	if got := floorReading(w, "tracker"); !strings.HasPrefix(got.FloorUnknownCause,
		"engine: read the fleet's published trim floors: ") {
		t.Errorf("the cause reads %q; want it to name the floors it could not list",
			got.FloorUnknownCause)
	}
	if fired(statelog.Evaluate(floorReading(w, "tracker")), statelog.KindFloorUnknown) {
		t.Fatal("one failing beat raised floor_unknown")
	}

	// FOUR HEARTBEATS OF IT is the threshold, and is not past it.
	w.observe(t.Context(), t0.Add(statelog.FloorCacheStale), subjects)
	if fired(statelog.Evaluate(floorReading(w, "tracker")), statelog.KindFloorUnknown) {
		t.Fatal("a floor unreadable for exactly the threshold raised floor_unknown")
	}
	// ONE MORE BEAT IS — for EVERY log the listing failed for, each on its
	// own clock, because a coordination outage refuses every log's reads.
	w.observe(t.Context(), t0.Add(statelog.FloorCacheStale+statelog.AlarmInterval), subjects)
	for _, domain := range []string{"tracker", "pages"} {
		reading := floorReading(w, domain)
		if reading.FloorUnknownFor != statelog.FloorCacheStale+statelog.AlarmInterval {
			t.Errorf("%s's age is %s, want first failing beat to latest",
				domain, reading.FloorUnknownFor)
		}
		if !fired(statelog.Evaluate(reading), statelog.KindFloorUnknown) {
			t.Errorf("%s's floor, unreadable a beat past four heartbeats, raised "+
				"no floor_unknown", domain)
		}
	}

	// A READ THAT SUCCEEDS CLEARS IT at once, and the next outage starts a
	// clock of its own rather than inheriting this one's age.
	source.err = nil
	w.observe(t.Context(), t0.Add(time.Hour), subjects)
	if got := floorReading(w, "tracker"); got.FloorUnknownFor != 0 || got.FloorUnknownCause != "" {
		t.Fatalf("a readable floor still reads %+v", got)
	}
	source.err = errors.New("coordination unreachable again")
	w.observe(t.Context(), t0.Add(2*time.Hour), subjects)
	if got := floorReading(w, "tracker"); got.FloorUnknownFor != 0 {
		t.Errorf("a new outage inherited an age of %s", got.FloorUnknownFor)
	}
}

// A FLOOR AHEAD OF THIS NODE IS UNKNOWN, ONE BEHIND IT IS NOT — the read path's
// own rule, so the alarm names exactly the floors every read refuses on.
//
// A floor published at a LATER generation means the fleet re-anchored the log
// and this node did not, and its positions name a sequence space nobody else
// is in: every read of that log refuses. One at an EARLIER generation is a
// floor the trim has not re-published since the re-anchor, which [floorFor]
// reads as nothing licensed yet and serves.
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

	ahead := floorReading(w, "tracker")
	if !strings.Contains(ahead.FloorUnknownCause, "generation 3") {
		t.Errorf("the cause is %q; want the tracker's floor, ahead at generation 3",
			ahead.FloorUnknownCause)
	}
	if ahead.FloorUnknownFor != time.Hour {
		t.Errorf("the age is %s, want an hour", ahead.FloorUnknownFor)
	}
	if behind := floorReading(w, "pages"); behind.FloorUnknownFor != 0 ||
		behind.FloorUnknownCause != "" {
		t.Errorf("a floor BEHIND this node reads %+v; the read path serves it, "+
			"so the alarm must not name it", behind)
	}
}

// EVERY LOG WHOSE FLOOR THIS NODE CANNOT USE IS ANSWERED ON ITS OWN, with its
// own age and its own cause.
//
// Two logs re-anchored while this node was not are two refusals and two
// re-anchors. The form this replaced reduced the watch to the domain unreadable
// longest, so the second log was named only once the first was repaired — under
// an alarm that had never cleared, and with an age that had been the first
// log's all along.
func TestEveryUnreadableFloorIsAnsweredOnItsOwn(t *testing.T) {
	t.Parallel()
	source := &scriptedFloors{rows: []coord.TrimFloor{{Domain: "tracker", Generation: 3}}}
	w := newFloorWatch(source.read)
	t0 := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	subjects := []floorSubject{{domain: "tracker", generation: 2}, {domain: "pages", generation: 2}}
	w.observe(t.Context(), t0, subjects)

	// HALF AN HOUR LATER the knowledge base's log is re-anchored too.
	source.rows = append(source.rows, coord.TrimFloor{Domain: "pages", Generation: 5})
	w.observe(t.Context(), t0.Add(30*time.Minute), subjects)
	w.observe(t.Context(), t0.Add(time.Hour), subjects)

	for domain, want := range map[string]struct {
		age        time.Duration
		generation string
	}{
		"tracker": {time.Hour, "generation 3"},
		"pages":   {30 * time.Minute, "generation 5"},
	} {
		got := floorReading(w, domain)
		if got.FloorUnknownFor != want.age || !strings.Contains(got.FloorUnknownCause, want.generation) {
			t.Errorf("%s reads %s: %q; want %s and its own floor at %s",
				domain, got.FloorUnknownFor, got.FloorUnknownCause, want.age, want.generation)
		}
	}

	// REPAIRING ONE clears that one and leaves the other exactly as it was.
	source.rows[0].Generation = 2
	w.observe(t.Context(), t0.Add(time.Hour+statelog.AlarmInterval), subjects)
	if got := floorReading(w, "tracker"); got.FloorUnknownCause != "" {
		t.Errorf("the repaired log still reads %+v", got)
	}
	if got := floorReading(w, "pages"); got.FloorUnknownFor != 30*time.Minute+statelog.AlarmInterval {
		t.Errorf("the other log's age became %s when its neighbour was repaired",
			got.FloorUnknownFor)
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
	if got := floorReading(w, "tracker"); got.FloorUnknownCause != "" {
		t.Fatalf("a stopping beat started a clock: %+v", got)
	}

	source.err = errors.New("coordination unreachable")
	w.observe(t.Context(), t0, subjects)
	source.err = context.Canceled
	w.observe(stopped, t0.Add(time.Hour), subjects)
	if got := floorReading(w, "tracker"); got.FloorUnknownCause == "" {
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
