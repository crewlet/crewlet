# ADR-0003 — Fleet-agreement state lives in coordination, never a node's own file

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** `internal/store.TestEveryNodeTableSaysWhoHasToAgreeOnIt`
- **Measured:** nine tables moved, across five migrations — `internal/store/schema/node/0010` took five, `0011`, `0012` and `0013` one each, and `0028` the ninth
- **Cost-when-tried:** five separate repair migrations, each discovering the previous one was incomplete. `0012` says so in its own text: `a2a_channels` "was the last of the tables migration 0010 should have taken". `0013` said it again about the next one, and `0028` about the one after that.
- **Tag-status:** unreleased

## The decision

The node's store is **one file, one process, exclusively owned**. A fact the
whole company has to agree on therefore cannot live in it, because every node
would read its own copy and draw a fleet of one. Such a fact goes in the
coordination KV, which rides the stream's own connection and is a
compare-and-set store the broker already provides.

The test is a single question, asked of every new table: **who has to agree on
this?** Three answers are legitimate and one is not:

- *this node alone* — the store. Its own audit log, a seat's memory, an index
  it rebuilds locally, an observation about shared infrastructure that two
  nodes may legitimately differ on.
- *every node, identically, derived from an ordered log* — the replicated
  estate, which is [ADR-0002](0002-the-stream-is-the-write-ahead-log.md).
- *the whole company, now* — coordination.
- *"it has always been in the store"* — not an answer, and the one that
  produced every incident below.

## Why the obvious alternative is wrong

The obvious alternative is that the store is already there, already durable,
already has a migration path, and one more table costs nothing. What it costs
is invisible on a single node and invisible on the node you check:

- `webhook_deliveries` — a vendor retrying a delivery reached whichever ingress
  node the load balancer picked, found no claim, and woke the same seat twice.
- `rate_limits` — four nodes ran four rate valves, so a seat capped at five a
  second emitted twenty.
- `config_activations` / `config_apply_status` — each node read its own row and
  drew a fleet of one, so a stalled rollout looked complete from everywhere.
- `turn_completions` — a redelivery that landed on a peer found no completion
  row and ran the turn a second time, firing every side effect again.
- `a2a_channels` — the channel state lived on the node that opened it while the
  wake was delivered to whichever node owned the target's seat, so the target
  woke to a channel it could not see.

Every one of those tables shipped with a migration comment describing shared
state and a placement that was per node. Nothing compared the two, because the
rule was prose and prose is not compared to anything. That is why the gate
named above does not try to judge which estate a table belongs in — no walk
over DDL can — and instead requires every node-estate table to carry a written
answer, checked in both directions, so the judgement is made in a diff somebody
reviews rather than skipped.

## What this does not decide

It does not make coordination a general database. Retention there is a
*bucket's age*, never a per-write TTL; the records are bounded, mutable and
short-lived; and coordination is deliberately **not** itself a replicated log —
`internal/coord` gives five reasons, the first being that the framework's
central property is the one a lease must not have.

## The table this record was written for

`chat_thread_follows` was company-wide chat routing state in the node's own
file, written and read from the fleet-wide `notify-inbound` consumer group. On
more than one node a reply that is not a mention reached the node holding the
follow row only by chance — the same shape `a2a_channels` was moved out for in
`0012`. Its only written justification was an exemption string in a
memory-sync test, arguing about seat movement rather than about per-delivery
node fan-out, which is a different question and the one it needed to answer.

It moved, in node migration `0028`, to a coordination bucket whose own age is
the retention. That is what makes this record's `Enforced-by:` mean something:
the gate made the table state its case, the case did not hold, and the table
went where the case pointed.
