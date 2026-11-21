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
//
// And the store's own consequence: each data node's store holds what that node
// published and the custody batches it keeps, and nothing else, so a READ of
// history is not this package's — it is asked of every live data node at query
// time by internal/eventfan, and a read of one node's store alone is a third
// of a three-node fleet (ADR-0021).
//
// A node without `data` writes no rows of its own, so [Custody] hands its
// events to the data nodes and one fleet-wide group of [Keeper]s keeps each
// batch once (custody.go). The boot seeds fill the projection from the fleet,
// each read until it lands, and a late one reaches every open socket
// (seeding.go).
//
// # A custody batch sent back is this node's failure, not the batch's
//
// A data node's [Keeper] that could not write or settle a custody batch sends
// it back for a retry ([sendBack]): as the batch's own failure ([queue.Nak])
// where the store refused it ([store.ErrCustodyRefused]), and as THIS NODE'S
// OWN ([queue.NakNode]) otherwise. Both come back on the same backoff and
// spend the same budget; what differs is where a batch whose budget is spent
// is kept ([queue.RetiredTo]). Counted as the batch's, an outage, this node's
// leaving or its own store failing put a stateless node's healthy events with
// the custody group's poison; as the node's, they are kept with its unhandled
// deliveries, as a batch no member handled.
//
// THE BATCH'S IS THE ONE FAILURE A BATCH CAN CAUSE, so it is the one told by a
// mark: a batch no node can write, one naming no id or origin or holding a
// record that cannot be stored, which the store refuses before it writes
// anything. Everything else the keeper meets names nothing of a batch but its
// id, and is this node's:
//
//   - what the queue tells as its node's ([queue.NodeFault]: a broker that did
//     not answer, a queue no longer live, a delivery whose context ended,
//     which the keeper's leaving the group does to a batch in hand), the test
//     internal/notify's wakes and internal/engine's seat mailbox and control
//     topic ask too, and which wins over the mark: a refusal met as the keeper
//     left is the leaving's;
//   - a claim that named no keeper: every error a claim answers is UNKNOWN by
//     its contract ([coord.Custody]), the store could not say, save one
//     wrapping [coord.ErrCustodyUndecidable], the store's definite answer that
//     the record cannot be read, which the keeper acknowledges before anything
//     is sent back; told by the step that met it and never by what a backend
//     wrapped it in, since [coord.ErrUnavailable] is the KV backend's wrapping
//     and an error a backend left unwrapped is no less unknown;
//   - this node's own store failing: the write's transaction after every
//     record validated, and the settle, which takes only the batch's id and
//     the claim's answer, whether the store was closed under the keeper or
//     failed with an error it left unmarked. A full disk or a busy store fails
//     a healthy batch that a peer, or this node a moment later, writes whole.
//
// # A batch this node already settled is not written again
//
// A redelivery of a batch this node settled, kept or not, writes nothing: the
// store answers from its `custody_settled` rows, swept with the event log.
// Written again, its kept rows would be in flight a second time and leave every
// usage count until the claim answered again.
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
