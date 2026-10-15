package statelog

import "log/slog"

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
