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
// # Nothing above this package may read one node and call it the fleet
//
// internal/api/queries declares the interface it reads history through, and
// every method on it returns a [Coverage] beside its answer. A bare
// [store.EventLog] does not satisfy it, so a read that quietly answers for one
// node does not compile.
package eventfan
