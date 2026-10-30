# ADR-0020: A node's own day is a compacted domain, and every node writes only its own

- **Status:** accepted
- **Authority:** `internal/usage`
- **Enforced-by:** `internal/usage.TestADepartedNodesSpendIsStillAnswered`
- **Measured:** the stream holds one message per (node, company day, seat, schedule or person) for 181 days, so its size is a census rather than a rate: a three-node fleet of forty seats and twenty schedules is about 32 600 messages, and at a busy seat-day's ≈ 6.5 KiB that is about 217 MB against the 1 GiB default ceiling. A person's day is a handful of spend cells — well under 1 KiB — and only for the people who asked a question that day, so it moves neither figure. A seat-day's page reads are capped at 256 (page, via) entries — under 200 KiB with a full-length search query in every entry, and under 1 MiB however those queries are spelled — so no record approaches the largest the log takes (`statelog.MaxTransportRecordBytes`, the transport's 8 MiB message ceiling less what an append adds to a record).
- **Cost-when-tried:** spend, turn and read history was a query over `crewlet_events`, the NODE estate's own audit log, so every answer described the node that happened to serve it. A three-node fleet showed a third of its spend on whichever node the dashboard reached; a "90 days" chart was drawn over the thirty the audit log keeps, with the previous window silently empty at 30 and 90 days; and a node that left the fleet took its share of the company's history with it, so every window after its departure under-reported by exactly what it had spent.
- **Tag-status:** unreleased

## The decision

What each node's seats and schedules did each company day is the state-log
framework's **fifth domain**, `usage`, and its second **compacted** one: stream
`CREWLET_USAGE_LOG`, one message per subject, a 181-day age bound. Its answer to
"who has to agree on it?" is *every node, and only the current value* — the
compacted-changelog answer of ADR-0014, applied to aggregates rather than to a
seat's memory.

**The node is part of the subject**, and that is the whole design. A node can
only derive what its own event log holds, so each node publishes its own days
and nothing else: one record per (node, day, seat), per (node, day, schedule)
and per (node, day, person) — a human seat the auxiliary model spent for, which
has no agent id to be a seat's — re-derived from that node's records and
republished WHOLE every time it moves. Every object therefore has exactly one writer, so nothing arbitrates
and nothing merges at write time; the reader sums across nodes. An apply
REPLACES the object's rows under a monotone position guard. Every node's
applier writes every node's days into its replicated estate, so any node
answers for the fleet — including for a node that has gone, and a node that
joins later replays the days from the stream alone.

**A node's day is the rows it keeps.** A data node also writes the custody
batches of nodes without `data` (ADR-0025), and until a batch's claim settles
two data nodes can hold it — so a day counts only what its node keeps, and the
fleet's sum holds every row once. A turn's figures are read off the ONE record
that ends it, which states the whole turn — its reviews, its send-backs, every
segment's duration — because its other records reach the data nodes a custody
batch at a time, kept by whichever node won each claim, and a resumed turn
leaves its first segment on another node: summed from what each node held,
the turn was counted short on one node and nowhere on the rest. So a row
changes only the day it falls on — but for a turn whose end states no tally —
one a build before that record ended, or one this build ended after resuming a
park such a build made — which is read off its records as it always was — and
a batch settled as kept changes the days its rows fall on, however far back;
the node derives every such day again, except one older than its event log is
read back, which keeps the value it was last published at rather than be
replaced by a derivation of rows partly swept. A day derived again keeps the instants it
was first cut on, so a moved company clock never moves rows between days
already published, and no day — today and yesterday included — is ever
published below what the fleet holds of it: inside the read floor a day only
grows, so a smaller derivation proves the node's own log lost rows — a
restore, an emptied store, a parked turn's earliest records swept — rather
than that the day shrank, and what goes out is the derivation JOINED with the
fleet's record, every figure at the larger of the two, which on an ordinary
day is the derivation itself. `internal/usage`'s publisher says how.

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

## Amendment — a node without `data` publishes no day

ADR-0025 left a node without `data` no event log and no state log, so every
"every node" above reads "every data node", on both sides: each data node
publishes its own days and applies every data node's, and a node without
`data` publishes none. Its seats' spend is counted in the day of the data node
that keeps its events in custody — which is what **A node's day is the rows it
keeps** above already decides, so the node a subject names is the node that
kept the events, not necessarily the node whose seat spent them. Nothing else
here moves: one writer per object, nothing arbitrated, and a reader summing
across the nodes that have days.
