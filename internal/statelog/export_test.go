package statelog

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// SpokenForTest is d as an alarm's detail says a duration ([spoken]): a
// detail names a window by the constant it borrows, so a case holds that rule
// only by rendering the constant the same way.
func SpokenForTest(d time.Duration) string { return spoken(d) }

// WaitWithinFrom is [WaitWithin] with the clock read at now ([waitWithin]):
// the bound must survive a stall between reading the clock and arming the
// wait, and no case can stall a goroutine there; a reading from before now is
// that stall.
func WaitWithinFrom(ctx context.Context, budget time.Duration, now time.Time) (
	context.Context, context.CancelFunc, WaitBound) {
	return waitWithin(ctx, budget, now)
}

// RankOffersForTest is [collectOffers]' ordering over offers already
// collected: the ordering holds the rule, and the collection around it is a
// broker round trip a case should not stand up to assert a sort.
func RankOffersForTest(offers []Offer, seed uint64) []Offer {
	return rankOffers(offers, seed)
}

// MeasureVolumesWith has s read what a volume has free from fn rather than
// from the volume: the gate's arithmetic holds the rule, and what the case's
// machine has free is no condition a case can state.
func MeasureVolumesWith(s *Snapshotter, fn func(dir string) (int64, error)) { s.volumeFree = fn }

// WaitsThrough is what a linearizable read resting on the read index at waits
// for ([waitsThrough]), exported so the wired node's read, which the
// benchmarks measure, waits by the same derivation rather than a copy.
func WaitsThrough(at Position) Position { return waitsThrough(at) }

// CurrentThrough is the position a read transaction's rows are current
// through, from the checkpoint row it read and the position the read was
// released at ([currentThrough]), exported so each rule of the carry is a case
// of its own rather than a branch reachable only through a loop.
func CurrentThrough(at, released Position) Position { return currentThrough(at, released) }

// StanceAt is a runner standing at at, with nothing reached past it, as a
// reading of the log's end pairs it ([Runner.ObserveEnd]): a case names
// positions no runner stands at (the zero before its loop loads the row, a
// checkpoint from before a reanchor).
func StanceAt(at Position) Stance { return Stance{At: at, reached: at} }

// LogTrackerTo points a tracker's transition lines at a logger a case reads:
// the lines (when an alarm started, how long it stood) are half of what a
// tracker is for, and the package's own logger is the process's.
func LogTrackerTo(t *Tracker, logger *slog.Logger) { t.logger = logger }

// FetchArtefactReporting is [FetchArtefact] with its progress lines every
// report rather than every [TransferReportEvery]: a deployment sizes its
// startup probe from those lines, and at the production cadence a case would
// need a transfer longer than ten seconds.
func FetchArtefactReporting(ctx context.Context, nc *nats.Conn, offer Offer, dest string,
	logger *slog.Logger, report time.Duration) (Fetched, error) {

	return fetchArtefact(ctx, nc, offer, dest, transferPace{
		wait: TransferChunkWait, report: report, log: loggerOr(logger),
	})
}
