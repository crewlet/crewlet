package statelog

import (
	"log/slog"
	"time"
)

// SpokenForTest is d as an alarm's detail says a duration ([spoken]).
//
// EXPORTED FOR A TEST because a detail that names a window by the constant it
// borrows, rather than by a literal of its own, is a rule a case can hold
// only by rendering the constant the same way.
func SpokenForTest(d time.Duration) string { return spoken(d) }

// RankOffersForTest is [collectOffers]' ordering, over offers already
// collected.
//
// EXPORTED FOR A TEST because the ordering is the part with a rule in it and
// the collection around it is a network round trip: a case that had to stand
// up a broker to assert a sort would be asserting the broker.
func RankOffersForTest(offers []Offer, seed uint64) []Offer {
	return rankOffers(offers, seed)
}

// MeasureVolumesWith has s read what a volume has free from fn rather than
// from the volume.
//
// EXPORTED FOR A TEST because the gate's arithmetic is the part with a rule in
// it, and what the machine running a case has free is no condition the case
// can state.
func MeasureVolumesWith(s *Snapshotter, fn func(dir string) (int64, error)) { s.volumeFree = fn }

// WaitsThrough is what a linearizable read resting on the read index at waits
// for ([waitsThrough]).
//
// EXPORTED FOR A TEST so the wired node's read — which the benchmarks measure
// a linearizable read through — waits for what a read does, by the same
// derivation rather than a copy of it.
func WaitsThrough(at Position) Position { return waitsThrough(at) }

// StanceAt is a runner standing at at, with nothing reached past it, as a
// reading of the log's end pairs it ([Runner.ObserveEnd]).
//
// EXPORTED FOR A TEST because a case about the pairing names positions a
// runner does not stand at — the zero it reports before its loop loads the
// row, a checkpoint from before a reanchor — and a stance is otherwise only
// ever read off a runner.
func StanceAt(at Position) Stance { return Stance{At: at, reached: at} }

// LogTrackerTo points a tracker's transition lines at a logger a case reads.
//
// EXPORTED FOR A TEST because the two lines are half of what a tracker is for
// — when an alarm started, and how long it stood — and the package's own
// logger is the process's.
func LogTrackerTo(t *Tracker, logger *slog.Logger) { t.logger = logger }
