// Package objectives holds the service objectives the engine is held to, as
// named constants with their reasons: publish success and latency, the first
// byte of a served document, the browser-reported time from a ticket to a ready
// frame, the datastore round trip and its resolution, change visibility, wake
// latency and presence latency.
//
// # One place for a threshold
//
// An alarm borrows its threshold from a decision made elsewhere rather than
// inventing one (ADR-0015), so the numbers an operator's alarms, panels and
// recording sites read are these. Every objective is an EVENT objective: a
// fraction of events, over a thirty-day [Period], that must be good, and a
// good event is one answered without fault on this engine's side within the
// objective's bound ([Objective.Good]). A latency percentile is the same
// shape, stated the other way round: "p95 under 250 ms" is "95 percent of
// operations answered within 250 ms", which is what lets a percentile carry an
// error budget and a burn rate like any availability objective.
//
// # Measured where the event happens, counted once
//
// A recording site (the publish pipeline, the artifact origin, the datastore
// writer, the ready report, the socket's push of a committed change, the
// change feed's wakes and the presence hub) calls [Meter.Record] with what it
// measured — or [Meter.RecordSince] with the instant another clock stamped its
// event's start, the broker's or a peer's, which the meter ends on its own
// clock and clamps at zero against the two clocks' skew — and the meter
// classifies the observation against the objective and counts it twice from
// that one call: into the engine's recorder, as
// `crewlet.objectives.events{objective, outcome}` for an exporter and the
// operator's 24-hour window, and into its own minute ledger, which is what the
// burn-rate alarm reads. The ledger exists because the recorder's window is
// hourly and a burn rate is judged over five minutes as well as six hours; it
// is a second VIEW of the same call, never a second counting path.
//
// # Burn rates, not thresholds
//
// An objective alarms when it spends its error budget faster than the month
// allows, over two windows at once ([BurnWindows], the pairs of the SRE
// workbook): fourteen point four times over the last hour and the last five
// minutes, which spends a fiftieth of the month's budget in an hour, and six
// times over the last six hours and the last thirty minutes, which spends a
// twentieth in six. The long window says the burn is real; the short window
// says it is still happening, so the alarm clears within minutes of the fault
// ending rather than an hour later. internal/statelog raises the result as the
// `objective_burn` alarm.
//
// PER NODE, like every alarm this engine raises: each node judges what it
// measured. A self-hosted install gets the same constants as reference
// objectives, with the counter an exporter aggregates across the fleet.
//
// # The browser-reported ready time
//
// Server metrics cannot see an outage that keeps requests from arriving (the
// artifact domain's DNS, its certificate, an edge or a corporate filter), so
// the dashboard reports how long each frame took from its ticket to the page's
// bridge hello. [Beacon] admits those reports: one per frame, bound to a frame
// this fleet minted for the reporting credential, never longer than the frame
// has existed, and at most a sustained one a second per credential.
package objectives
