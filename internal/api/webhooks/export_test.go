package webhooks

// BodyKeyForTest exposes the payload-derived delivery key.
//
// The empty-body case has no route that can reach it — every handler refuses
// a body it could not parse long before the key is derived — and it is the
// one input where getting it wrong is a self-inflicted outage: an empty key
// that claimed would refuse every later delivery from that vendor for the
// whole TTL. So it is asserted directly rather than left to a route that
// cannot produce it.
func BodyKeyForTest(raw []byte) string { return bodyKey(raw) }
