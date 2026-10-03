# ADR-0020 — A node's own day is a compacted domain, and every node writes only its own

- **Status:** accepted
- **Authority:** `internal/usage`
- **Enforced-by:** `internal/usage.TestADepartedNodesSpendIsStillAnswered`
- **Measured:** the stream holds one message per (node, company day, seat or schedule) for 181 days, so its size is a census rather than a rate: a three-node fleet of forty seats and twenty schedules is about 32 600 messages, and at a busy seat-day's ≈ 6.5 KiB that is about 217 MB against the 1 GiB default ceiling. A seat-day's page reads are capped at 256 (page, via) entries — about 35 KiB — so no record approaches the broker's 1 MiB message limit.
- **Cost-when-tried:** spend, turn and read history was a query over `crewlet_events`, the NODE estate's own audit log, so every answer described the node that happened to serve it. A three-node fleet showed a third of its spend on whichever node the dashboard reached; a "90 days" chart was drawn over the thirty the audit log keeps, with the previous window silently empty at 30 and 90 days; and a node that left the fleet took its share of the company's history with it, so every window after its departure under-reported by exactly what it had spent.
- **Tag-status:** unreleased

## The decision

What each node's seats and schedules did each company day is the state-log
framework's **fourth domain**, `usage`, and it is **compacted**: stream
`CREWLET_USAGE_LOG`, one message per subject, a 181-day age bound. Its answer to
"who has to agree on it?" is *every node, and only the current value* — the
compacted-changelog answer of ADR-0014, applied to aggregates rather than to a
seat's memory.

**The node is part of the subject**, and that is the whole design. A node can
only derive what its own event log holds, so each node publishes its own days
and nothing else: one record per (node, day, seat) and per (node, day,
schedule), re-derived from that node's records and republished WHOLE every time
it moves. Every object therefore has exactly one writer, so nothing arbitrates
and nothing merges at write time; the reader sums across nodes. An apply
REPLACES the object's rows under a monotone position guard. Every node's
applier writes every node's days into its replicated estate, so any node
answers for the fleet — including for a node that has gone, and a node that
joins later replays the days from the stream alone.

**The horizon is the applier's.** A record for day D deletes every row older
than D minus 181 days in the transaction that writes D, which is a pure function
of the record — so the history ages out identically on every node and nothing
but the applier ever writes the replicated estate (ADR-0002).

## Why the obvious alternative is wrong

The obvious alternative is to ask every node at query time — scatter the spend
query and sum the answers. It is the right shape for turn-level DETAIL, where a
missing node can be named, but for aggregates it has no answer to the case this
exists for: a node that has left cannot be asked, so its history is simply gone,
and the ninety-day comparison is only as long as the shortest audit log in the
fleet. It also costs every chart a round trip to every node.

The second is a strictly ordered, arbitrated domain holding one row per
(day, seat) that every node adds its deltas into. That puts N writers on one
subject for every seat that ran on more than one node, makes a lost or repeated
delta a permanent miscount, and turns the stream into a history nobody needs —
the only value anybody reads is the current total, which a compacted subject
holds by construction.

The third is a fleet-singleton duty that reads everyone's spend and publishes
it. No node can read another's event log, so there is nothing for it to read.

## What this does not decide

It does not decide which answers are served from these rows or what shapes they
take — those are the API's, over the rows this domain writes. It does not carry
turn-level detail: which turn spent what is answered from each node's own
event log, which is a different mechanism with a different retention.
