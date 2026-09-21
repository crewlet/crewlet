package statelog

// RankOffersForTest is [collectOffers]' ordering, over offers already
// collected.
//
// EXPORTED FOR A TEST because the ordering is the part with a rule in it and
// the collection around it is a network round trip: a case that had to stand
// up a broker to assert a sort would be asserting the broker.
func RankOffersForTest(offers []Offer, seed uint64) []Offer {
	return rankOffers(offers, seed)
}
