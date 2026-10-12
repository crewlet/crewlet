// Package usage is what each node's seats, schedules and people did each
// company day, replicated to every data node — the state-log framework's FIFTH
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
// which is ADR-0014's fourth answer, a compacted changelog. Each data node
// derives its own day from its own event log ([store.DB.UsageForDay]), folds it
// into one record per (node, day, seat), per (node, day, schedule) and per
// (node, day, person), and publishes the record whole — signed, as every
// state-log record is (ADR-0031) — and every data node's applier writes it into
// the replicated estate. ADR-0020 is the decision.
//
// A NODE WITHOUT `data` PUBLISHES NOTHING, because it keeps no event log to
// derive a day from: its events are written by a data node that keeps them in
// custody (internal/observe, ADR-0025), so its seats' spend is counted in that
// keeper's day, under the keeper's node id — and in no other data node's, since
// the custody claim gives each batch exactly one keeper.
//
// # Why the node is part of the identity
//
// A node can only derive what its own log holds. Putting the node in the
// subject makes each object single-writer — nothing to arbitrate, no merge at
// write time — and leaves the merge to the reader, which sums across nodes. A
// seat that moved at noon is two rows for that day, and both are true.
//
// # And what names the seat, the schedule and the person
//
// Their IDENTITIES, never their names, because a day is kept for half a year
// and a name is prose somebody retypes: a seat by its agent id — derived from
// its handle (ADR-0013) — a schedule by its scope's identity (`scope_type`,
// `scope_id`) and its name within that scope, the key the dispatch ledger
// keys a fire on, and a person by the HANDLE of the human seat their spend was
// charged to — the seat the request's principal holds ([iam.RecordOwner]),
// immutable like every handle, since a human seat derives no agent id. It is
// NOT the identity estate's person id: a day records which seat spent, and the
// person holding that seat is the directory's answer, which moves when
// somebody is moved to another seat. A principal bound to no seat charges the
// company's day alone and no person's. A seat's handle rides its record as a
// LABEL only, for a seat the organisation no longer holds; a reader resolves
// a seat's id to its handle.
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
// # What it deliberately is not
//
// Not turn-level detail. A day's row cannot say which turn spent what, and
// the questions that need one turn — a trace, a phase, an event — are answered
// by asking every data node's own event log at query time, which is a
// different mechanism with a different retention.
//
// Not swept. The horizon ([History]) is applied by the applier from the
// record's own day, so the rows leave on every data node in the same
// transaction that writes the day that makes them old, and nothing but the
// applier writes the replicated estate.
package usage
