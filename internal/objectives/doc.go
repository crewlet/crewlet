// Package objectives holds the service objectives artifacts are held to, as
// named constants with their reasons: publish success and latency, the first
// byte of a served document, the browser-reported time from a ticket to a ready
// frame, the datastore round trip, change visibility and presence latency.
//
// ONE PLACE FOR A THRESHOLD. An alarm borrows its threshold from a decision
// made elsewhere rather than inventing one, so the numbers an operator's alarms
// and dashboards read are these, and the measurements that feed them classify
// each observation as good or bad against the same constants.
package objectives
