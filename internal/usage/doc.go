// Package usage is what each node's seats, schedules and people did each
// company day, replicated to every node — the state-log framework's FOURTH
// domain, and its second COMPACTED one.
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
// one record per (node, day, seat), per (node, day, schedule) and per (node,
// day, person), and publishes the record whole; every node's applier writes it
// into the replicated estate. ADR-0020 is the decision.
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
// node's scheduler recorded. A person-day: what the seats' auxiliary model
// spent for a human seat — a question answered on the operator surface, a pass
// on a unit a person leads — by the same cells, and nothing else, since a
// person has no agent id, no turns and no reads ([KindPerson]). A record is
// always the cumulative value; an apply REPLACES.
//
// # Reading a window, a page at a time
//
// The reads a window grows ([Spend], [SeatTurns], [PersonSpend], [SeatDays],
// [PageReads]) are each the first page of a paged read asked with no budget,
// and a peer answers the same read a page at a time ([Fill]). A page pays for
// its own rows: the read walks its window one leading key at a time — a
// company day, or a page id — each by a statement that seeks the key and
// streams in the read's order, so a page stops reading where it ends.
//
// Measured with BenchmarkAUsageWindowInPages (ninety days, three nodes, 120
// seats with six spend cells and four page reads a day — 194 400 spend rows —
// on a four-vCPU 2.8 GHz Xeon), each read whole and walked at the routed
// page's budget of 2 MiB:
//
//	             one statement a window          one statement a key
//	spend        whole 4.6 s, 19 pages 11.6 s    whole 3.5 s, 19 pages 4.0 s
//	page reads   whole 2.9 s, 16 pages  7.0 s    whole 3.0 s, 16 pages 4.1 s
//	seat days    whole 1.0 s,  4 pages  1.3 s    whole 1.0 s,  4 pages 1.0 s
//
// The whole spend read got faster as well: its heads are read once a day and
// joined in memory, where the one statement probed the head table once a
// spend cell and sorted the window. What a page still costs beyond the whole
// read is the encoding it measures each row by — which the wire then carries
// rather than encoding the row again ([Page.AppendJSON]).
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
