// Package memread reads a seat's memory from the node that holds the seat.
//
// # Why a read has to be routed
//
// A seat's memory — its diary, its episodes, the skills it drafted, what it
// learned about the people it works with, whether it onboarded, and the ledger
// of what it said in each conversation — is written to the store of the node
// RUNNING the seat. memsync carries every row across the fleet on a compacted
// changelog, and a node hydrates a seat's rows when placement hands it the
// seat. So every node that ever held a seat keeps a copy of its memory, and
// exactly one keeps it CURRENT: the holder. The copy on any other node is what
// the seat knew the day it left, or nothing at all.
//
// A screen that read whichever node served the request therefore described a
// seat's memory as of the last time that node happened to run it — a diary
// missing a week, a page count that went backwards on refresh — and nothing on
// the answer said so. Here a read is answered by the holder, and the answer
// names it (`held_by`).
//
// # Who answers, and what the three outcomes are
//
// The asker reads the seat's lease, which names the INCARNATION that holds it:
//
//   - nobody — the seat is on no node, so no copy is current and none is
//     shown: the answer is empty and `held_by` is [HolderNone]. The rows on
//     this node's disk are real, but they are a snapshot of unknown age, and
//     drawing them as the seat's memory is the failure this package removes;
//   - this incarnation — answered from this node's own store, once the seat is
//     ATTACHED. A seat still hydrating is not yet current here either, and the
//     read is refused as not ready rather than answered short;
//   - a peer — asked, on an ephemeral scatter ([topics.HeldRead]) that only
//     the named incarnation answers. Silence past [DefaultBudget] is an
//     UNKNOWN, never an empty memory: the holder was named and did not say.
//     Before asking, the asker reads what that incarnation's build
//     advertises ([coord.FeatureHeldRead]): a holder on a build that serves
//     no such subject is unavailable AT ONCE and says so, rather than
//     waited on for the whole budget on every poll of a rolling upgrade.
//
// A node with no broker has no peer, and its store is the only copy there is:
// it answers every read itself.
//
// # Not the queries package's, because every node answers
//
// The API is served on ingress nodes only, and a seat may be held by a node
// that serves none. So the answer is built here, below both the API and the
// engine, and the engine makes EVERY node an answerer.
package memread
