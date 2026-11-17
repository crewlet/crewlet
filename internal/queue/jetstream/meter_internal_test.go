package jetstream

import (
	"math"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// fedMeter is a meter that reads no socket, whose readings a case feeds by
// hand: a reading every [meterPeriod] from an instant the case states.
type fedMeter struct {
	m  *LinkMeter
	at time.Time
}

// newFedMeter is a fed meter of a link whose floor carries floor bytes a
// second.
func newFedMeter(floor float64) *fedMeter {
	return &fedMeter{
		m: &LinkMeter{floor: floor, readings: make([]reading, meterKeeps),
			sampled: make(chan struct{})},
		at: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC),
	}
}

// feed records n more readings, a [meterPeriod] apart, each count grown by
// what it carries in a period each way.
func (f *fedMeter) feed(n int, ackedPerPeriod, readPerPeriod uint64) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	last := reading{at: f.at}
	if f.m.taken > 0 {
		last = f.m.readings[(f.m.next+len(f.m.readings)-1)%len(f.m.readings)]
	}
	for range n {
		last = reading{at: last.at.Add(meterPeriod), acked: last.acked + ackedPerPeriod,
			read: last.read + readPerPeriod}
		f.m.pushLocked(last)
	}
}

// perPeriod is what share of the floor carries in a [meterPeriod], rounded
// up to a whole byte.
func perPeriod(floor, share float64) uint64 {
	return uint64(math.Ceil(share * floor * meterPeriod.Seconds()))
}

// A LINK IS MOVING AT SEVEN TENTHS OF THE FLOOR, ONE WAY: the line sits midway
// between the background a quiet link carries — presence reads up to 40% of
// the floor, pings and heartbeats — and a link draining bulk at the floor, so
// the floor itself moves, the background does not, and the line is
// [movingShare] of what the floor carries over the span asked about, in
// EITHER direction alone: two directions each under it are not added up to
// pass it.
//
// Mutant: Moving true on any byte — the background reads as moving. Mutant:
// the two directions summed — two halves under the line pass it. Mutant: the
// line at half the floor — the background at 0.6 passes.
func TestALinkIsMovingAtSeventyPercentOfTheFloorOneWay(t *testing.T) {
	t.Parallel()
	floor := float64(1<<20) / objstore.MiBPace.Seconds()
	const d = 5 * time.Second
	span := int(d/meterPeriod) + 1
	for _, c := range []struct {
		name        string
		acked, read float64
		want        bool
	}{
		{"the floor, written", 1, 0, true},
		{"the floor, read", 0, 1, true},
		{"seven tenths written", 0.7, 0, true},
		{"seven tenths read", 0, 0.7, true},
		{"just under seven tenths written", 0.69, 0, false},
		{"just under seven tenths read", 0, 0.69, false},
		{"the background's ceiling, both ways", 0.4, 0.4, false},
		{"background above the old half-floor line", 0.6, 0, false},
		{"a quiet link's trickle", 0, 1024 / floor, false},
		{"nothing", 0, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFedMeter(floor)
			f.feed(span, perPeriod(floor, c.acked), perPeriod(floor, c.read))
			if got := f.m.Moving(d); got != c.want {
				t.Errorf("a link carrying %.2f of the floor written and %.2f read over %v reads "+
					"moving=%t, want %t", c.acked, c.read, d, got, c.want)
			}
		})
	}
}

// A LINK'S MOVEMENT IS JUDGED OVER THE SPAN ASKED ABOUT, from its newest
// reading: a link that carried at the floor until a moment ago and nothing
// since is moving over a span reaching back into the bulk and still over one
// that does not; a meter keeps [objstore.Stall] of readings, past its ring's
// wrap; and one that has read no span — made a moment ago — is not moving.
//
// Mutant: the oldest reading kept rather than the newest one d back — a link
// that stopped a span ago reads as moving. Mutant: the ring read in the order
// it is stored — red past the wrap.
func TestALinksMovementIsJudgedOverTheSpanAskedAbout(t *testing.T) {
	t.Parallel()
	floor := float64(1<<20) / objstore.MiBPace.Seconds()
	const d = 5 * time.Second
	span := int(d / meterPeriod)

	f := newFedMeter(floor)
	if f.m.Moving(d) {
		t.Fatal("a meter with no reading reads moving")
	}
	f.feed(1, perPeriod(floor, 1), 0)
	if f.m.Moving(d) {
		t.Fatal("a meter with one reading, and so no span, reads moving")
	}
	// PAST THE RING'S WRAP: more readings at the floor than it keeps.
	f.feed(meterKeeps+span, perPeriod(floor, 1), 0)
	if !f.m.Moving(d) {
		t.Fatal("a link at the floor, its readings past the ring's wrap, reads still")
	}
	if !f.m.Moving(2 * objstore.Stall) {
		t.Fatal("a link at the floor for longer than the meter keeps reads still over a span past it")
	}
	// STOPPED: a span's readings of nothing.
	f.feed(span+1, 0, 0)
	if f.m.Moving(d) {
		t.Fatalf("a link that carried nothing for %v reads moving over %v", d+meterPeriod, d)
	}
	if !f.m.Moving(4 * d) {
		t.Fatalf("a link that carried the floor until %v ago reads still over %v", d+meterPeriod, 4*d)
	}

	// A YOUNG METER: three readings at the floor, judged over the half
	// second they span whatever span is asked.
	young := newFedMeter(floor)
	young.feed(3, perPeriod(floor, 1), 0)
	if !young.m.Moving(d) {
		t.Fatal("a meter that has read the floor over all it has read reads still")
	}
}
