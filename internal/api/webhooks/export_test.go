package webhooks

// BodyKeyForTest exposes the payload-derived delivery key.
//
// The empty-body case has no route that can reach it — every handler refuses
// a body it could not parse long before the key is derived — and it is the
// one input where getting it wrong is a self-inflicted outage: an empty key
// that claimed would refuse every later delivery from that third-party app for the
// whole TTL. So it is asserted directly rather than left to a route that
// cannot produce it.
func BodyKeyForTest(raw []byte) string { return bodyKey(raw) }

// GitHubSecretForTest exposes which credential a delivery to one path is
// checked against.
//
// Asserted directly because the two deployments it decides between cannot be
// told apart from outside: an agent's own app and one organization app
// pointed at a seat both POST to the same route, and picking the wrong
// credential refuses every delivery with a 503 the third-party app's own
// settings page reports as healthy.
func GitHubSecretForTest(s Secrets, handle string) string { return githubSecret(s, handle) }

// DatadogSummaryForTest exposes the gloss one alert gets in the feed.
//
// Asserted directly because the two things that were wrong with it are
// invisible from a route: the priority is read by this function and by
// [datadog.decode], and they disagreed about whether the wire value carries
// its own "P" — which a delivery test cannot see, because both readings
// accept the same payload.
func DatadogSummaryForTest(body map[string]any) string { return datadogSummary(body) }
