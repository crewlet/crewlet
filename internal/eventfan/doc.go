// Package eventfan answers the fleet's turn-level history by asking every node
// its own event store at query time, and says which nodes answered.
//
// # Why this is a scatter and not a table (ADR-0021)
//
// Every event is written ONCE, to the event store of the node that published
// it: the publish listener is inline on the publishing node and there is no
// consumer group behind it, so no two nodes ever write one row (see
// internal/observe). That is what makes the audit log cheap and exact — and it
// is also why a read of it answered for ONE node. A three-node fleet's turn
// list was a third of the company's turns on whichever node the dashboard
// reached; a turn resumed on another node after a restart showed half its
// phases; a trace whose inbound webhook landed on node B and whose agent work
// ran on node A was two unrelated half-traces on two screens.
//
// Replicating the detail would put every prompt and every response of every
// phase on every node's disk — the one table in the deployment whose size is a
// function of how much the company talks — to answer a question that is asked
// a few times a minute. So the question travels instead: a read is scattered
// to every live node on [topics.ObserveRead] over the queue's EPHEMERAL verbs
// ([queue.EventQueue.Ask] and Serve — no stream, no consumer, no record, since
// a read of the event log that wrote an event would grow the log every time
// somebody looked at it), every node answers from its own store, and the asker
// merges. Aggregates — spend, turn counts, page reads — are the other
// mechanism and live in the replicated `usage` domain (ADR-0020); this package
// is only the detail, and only the detail a node still holds.
//
// # A gone node's detail is gone
//
// This is the cost the decision accepts, and it is stated on every answer
// rather than hidden: a node that has left the fleet, or that did not answer
// inside [FleetReadBudget], is NAMED in the answer's [Coverage], and the answer
// is complete only when every live node answered. A short answer that did not
// say so would be indistinguishable from a quiet company.
//
// # The merges are exact, and each says why
//
// Each node's store is disjoint from every other's, so the merges are pure
// functions over values ([MergeListing], [MergeSeries], [MergeTurnPartials],
// [FirstFound]) and each one is exact for a reason it states: a keyset page is
// merged k-way on (time, id) and cut at the newest position any FULL page
// stopped at, a histogram's bars are summed over one pinned window, a turn's
// aggregate is re-folded from each node's partial with the SQL's own
// aggregates, and a list of turns is TWO scatters — the candidates, then every
// node's share of exactly those turns, because the node a turn was selected
// on is not always the only node that holds it.
//
// # Every question is asked at one instant
//
// Exact for disjoint stores, and only if every store answers the same
// question — and every one of these is floored at the thirty-day history
// horizon, which is a function of WHEN it is asked. So the asker reads its
// [Fleet.Clock] once per question and sends the instant with it, and every
// node floors at that instant rather than at its own clock; and one node's
// part of an answer is read at that one instant throughout (see parts.go), so
// a count is never floored a moment later than the rows it counts. Floored at
// each node's own clock, a merged answer was a union of horizons, and a node
// that answered a second late dropped that second's rows from an axis the
// answer said covered them. The instant raises no protocol version, so a node
// on an earlier build answers as of its own clock — and its lookup of one
// event by id was not floored at all — which is why the asker also HOLDS
// every row that comes back to its own horizon before it merges anything
// ([heldTo]); [Protocol] says what that cannot reach.
//
// # Nothing above this package may read one node and call it the fleet
//
// internal/api/queries declares the interface it reads history through, and
// every method on it returns a [Coverage] beside its answer. A bare
// [store.EventLog] does not satisfy it, so a read that quietly answers for one
// node does not compile.
package eventfan
