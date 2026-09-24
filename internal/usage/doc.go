// Package usage is what each node's seats and schedules did each company day,
// replicated to every node — the state-log framework's FOURTH domain, and its
// second COMPACTED one.
//
// # The question it answers, and who has to agree on it
//
// "How much did this company spend last month, and on what" was answered from
// `crewlet_events`, the node estate's audit log. That log is this node's
// alone, so every such answer described the node that happened to answer it: a
// three-node fleet showed a third of its spend on whichever node the dashboard
// reached, a ninety-day window drew thirty days of data (the audit log's own
// horizon), and a node that left the fleet took its history with it.
//
// Every node has to agree on a node's day, and only its CURRENT value matters —
// which is ADR-0014's fourth answer, a compacted changelog. Each node derives
// its own day from its own event log ([store.DB.UsageForDay]), folds it into
// one record per (node, day, seat) and per (node, day, schedule), and
// publishes the record whole; every node's applier writes it into the
// replicated estate. ADR-0020 is the decision.
//
// # Why the node is part of the identity
//
// A node can only derive what its own log holds. Putting the node in the
// subject makes each object single-writer — nothing to arbitrate, no merge at
// write time — and leaves the merge to the reader, which sums across nodes. A
// seat that moved at noon is two rows for that day, and both are true.
//
// # What a record holds
//
// A seat-day: its spend by (phase, worker, model, provider key) with the cache
// counts, its ended turns (count, failed, reviewed, first-pass, send-backs and
// a mergeable duration [Hist]), and the pages it read — capped at
// [ReadsPerSeatDay] with the remainder counted. A schedule-day: every fire the
// node's scheduler recorded. A record is always the cumulative value; an apply
// REPLACES.
//
// # What it deliberately is not
//
// Not turn-level detail. A day's row cannot say which turn spent what, and
// the questions that need one turn — a trace, a phase, an event — are answered
// by asking every node's own event log at query time, which is a different
// mechanism with a different retention.
//
// Not swept. The horizon ([History]) is applied by the applier from the
// record's own day, so the rows leave on every node in the same transaction
// that writes the day that makes them old, and nothing but the applier writes
// the replicated estate.
package usage
