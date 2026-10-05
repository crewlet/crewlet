// Package eventfan answers the fleet's turn-level history by asking every node
// its own event store at query time, and says which nodes answered.
//
// # Why this is a scatter and not a table (ADR-0021)
//
// Every event is written ONCE, to the event store of the node that published
// it: the publish listener is inline on the publishing node and there is no
// consumer group behind it, so no two nodes ever write one row of their own
// (see internal/observe). A node without `data` keeps no store, and hands its
// events to ONE data node in custody batches instead — one, once the fleet has
// settled which, and for the moments before that possibly two (below). That is
// what makes the audit log cheap and exact — and it is also why a read of it
// answered for ONE node. A three-node fleet's turn
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
// The merges are pure functions over values ([MergeListing], [MergeSeries],
// [MergeTurnPartials], [MergeOutcomes], [FirstFound]), and they rest on each
// node's store being disjoint from every other's — which every row of a node's
// own is, and a stateless node's row is once its custody batch is settled. A
// batch whose claim failed is written by a second keeper before the first has
// learned it is not its own, so until the first settles it — a pass a minute,
// and for as long as that node cannot reach coordination — two logs hold the
// same rows. A merge that unions rows by identity holds such a row once (a
// listing, a trace's or a turn's rows, the spend records by event id), and the
// outcome counts name such rows rather than count them and count each once
// ([MergeOutcomes]); the axis's bars, a trace's or a turn's total and a page of
// turns' folded sums add each node's part, and count such a row once per node
// holding it until its batch is settled. Each merge is otherwise exact for a
// reason it states:
// a keyset page is merged k-way on (time, id) and cut at the newest position
// any FULL page stopped at, a histogram's bars are summed over one pinned
// window that every build cuts alike — the partial bar a window the history
// clips begins with is dropped only after the sum, so a node on an earlier
// build is summed rather than named (short by the rows between the asker's
// horizon and its own when its clock runs ahead — see [Protocol]) — a turn's
// aggregate is re-folded from
// each node's partial with the SQL's own aggregates, the integrations' outcome
// counts are summed over a window whose both edges the asker named, and a
// list of turns is TWO
// scatters — the candidates, then every node's share of exactly those turns,
// because the node a turn was selected on is not always the only node that
// holds it — paged where a node's page lists each turn, since a cursor can
// resume only from there ([Fleet.Turns]).
//
// # A count is asked over a window, never taken from a page
//
// A page of the newest rows spans whatever it spans, so a number derived from
// one is a count over a stretch of time nobody named — and stated beside a
// window the answer DOES name, it reads as that window's. The integrations
// answer did exactly that with what became of its deliveries, until the
// outcome counts became a question of their own ([Fleet.NotificationOutcomes]):
// every node counts `[since, at)` from its own store, grouped in SQL, and the
// asker sums — each node the rows it keeps, and the asker each row a custody
// batch still in flight puts on two nodes once.
//
// # Every question is asked at one instant
//
// Exact for disjoint stores, and only if every store answers the same
// question — and every one of these is floored at the thirty-day history
// horizon, which is a function of WHEN it is asked. So the asker reads its
// [Fleet.Clock] once per question — to the microsecond, the store's own
// resolution, or the floor a statement applies and the horizon the asker holds
// rows to are two edges a tick apart — and sends the instant with it, and every
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
