# ADR-0021 — The fleet's turn-level detail is read from every node at query time, and every answer says which nodes it covers

- **Status:** accepted
- **Authority:** `internal/eventfan`
- **Enforced-by:** `internal/api/queries.TestEventReadsGoThroughTheFleet`, plus the compile-time `queries.FleetEvents` interface, every method of which answers a coverage — so a bare `*store.EventLog` does not fit in `queries.Sources`
- **Cost-when-tried:** every history read was a query over the serving node's own `crewlet_events`. A three-node fleet's turn list was a third of the company's turns on whichever node the dashboard reached; a turn resumed on another node after a restart showed half its phases and read as never finished; a trace whose inbound webhook landed on one node and whose agent work ran on another was two unrelated half-traces; and a seat's history restarted at every placement move. None of it said so — a short answer looked exactly like a quiet company.
- **Tag-status:** unreleased

## The decision

Turn-level detail — the event log, one event, the log's time axis, a trace, a
turn, the list of turns, the phase records and a seat's history — stays where
it is written, in the event store of the node that published it, and is read
by **asking every live node at query time**. The node serving the question
reads its own store directly and scatters the same question to its peers over
the broker's ephemeral request/reply (`crewlet.observe.read`), waits at most
`eventfan.FleetReadBudget` (two seconds), and merges the answers with merges
that are exact for disjoint stores.

**Every answer carries one shape saying which nodes it covers**,
`coverage{nodes:[{id,answered,error}],complete}`, and names every node that did
not answer and why. The API reads history only through `queries.FleetEvents`,
whose every method returns that coverage beside its answer.

The aggregates are the other mechanism and not this one: spend, turn counts
and page reads are the replicated `usage` domain (ADR-0020), because they must
survive the node that recorded them.

## Why the obvious alternative is wrong

The obvious alternative is to replicate the detail — every node applying every
node's events, so any node can answer alone. That puts every prompt and every
response of every phase on every node's disk, which is the one table in the
deployment whose size is a function of how much the company talks, multiplied
by the fleet, to answer a question asked a few times a minute. It also turns
the audit log into a second replicated estate with its own log, trim, snapshot
and catch-up story, for rows nobody reads after thirty days.

The second alternative is to keep reading one node and document it. That is
what shipped, and it was wrong in the way that matters most: silently. A screen
that shows a third of a fleet's turns without saying so is worse than one that
shows none.

## What this does not decide

It does not keep a departed node's detail. A node that has left the fleet
cannot be asked, and its turns, phases and events are gone with it — stated on
every answer by the node's absence from `coverage` and by the `history_partial`
alarm, and accepted: an operator who needs that detail past a node's life
exports it to an OTLP sink. It does not decide the aggregates (ADR-0020), and it
does not change how events are written: still once, inline, on the publishing
node.
