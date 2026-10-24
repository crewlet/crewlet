package statelog

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// RankOffersForTest is [collectOffers]' ordering, over offers already
// collected.
//
// EXPORTED FOR A TEST because the ordering is the part with a rule in it and
// the collection around it is a network round trip: a case that had to stand
// up a broker to assert a sort would be asserting the broker.
func RankOffersForTest(offers []Offer, seed uint64) []Offer {
	return rankOffers(offers, seed)
}

// LogTrackerTo points a tracker's transition lines at a logger a case reads.
//
// EXPORTED FOR A TEST because the two lines are half of what a tracker is for
// — when an alarm started, and how long it stood — and the package's own
// logger is the process's.
func LogTrackerTo(t *Tracker, logger *slog.Logger) { t.logger = logger }

// FetchArtefactReporting is [FetchArtefact] with its progress lines every
// report rather than every [TransferReportEvery].
//
// EXPORTED FOR A TEST because the lines are what a deployment sizes its
// startup probe from, and at the production cadence a case proving that a
// transfer reports as it goes would need a transfer longer than ten seconds.
func FetchArtefactReporting(ctx context.Context, nc *nats.Conn, offer Offer, dest string,
	logger *slog.Logger, report time.Duration) (Fetched, error) {

	return fetchArtefact(ctx, nc, offer, dest, transferPace{
		wait: TransferChunkWait, report: report, log: loggerOr(logger),
	})
}
