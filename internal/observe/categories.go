// Package observe is the engine's observability edge: what becomes a durable
// event-store row, what reaches a live dashboard, and how one event turns into
// each.
//
// It exists as its own package because those two consumers are reached by
// deliberately different routes, and the reasons are not obvious:
//
//   - THE STORE IS WRITTEN BY A PUBLISH LISTENER, inline on the node that
//     published. That node definitely has the event, so there is no round trip
//     and no consumer group — and therefore no way for two nodes to write the
//     same row, and no way for a group's rebalance to lose one.
//   - THE PROJECTION IS FED BY SubscribeStream, an ephemeral per-caller
//     broadcast. It has to be: a dashboard served by node B must show turns
//     that ran on node A, and a competing-consumer group would hand each event
//     to exactly one node's projection — so which turns a browser saw would
//     depend on which node answered its socket.
//
// A publish listener for the projection would have the same defect from the
// other side: it only ever sees what its own node published.
package observe

import "github.com/crewlet/crewlet/internal/events"

// The taxonomy is [events]'s, and these three are the thinnest possible view
// of it.
//
// It used to be a map HERE and an identical map in internal/store, with
// nothing asserting they agreed: a type placed in one and forgotten in the
// other would be written to the store and never shown, or shown and never
// written, and no test anywhere could see it. One map, in the package that
// owns the type registry, is what makes that impossible rather than merely
// unlikely.

// Category names an event type's dashboard category, or "" for a type that is
// neither persisted nor fed to the activity feed.
func Category(eventType string) string {
	category, _ := events.Category(eventType)
	return category
}

// LiveOnly reports whether a type is excluded from the store while still
// driving the live projection.
func LiveOnly(eventType string) bool { return events.LiveOnly(eventType) }

// Excluded reports why a type is kept out of the event store, or "" if it is
// not deliberately excluded — which, for a type with no category either, means
// nobody has placed it.
func Excluded(eventType string) string { return events.Excluded(eventType) }
