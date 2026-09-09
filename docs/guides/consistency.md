# Read Consistency

Every node of a Crewlet fleet holds its own copy of the company's work. The
copies are built by applying one ordered log, so they converge — but at any
instant they are at different points along it. This page is about the one
question that follows: **how fresh does this particular answer have to be?**

There are four answers, and picking one is the whole of it.

## The four levels

| Level | What it promises | What it costs |
|---|---|---|
| `linearizable` | No answer from before this read arrived. | An append to the log and a wait for it to come back. |
| `session` | No answer from before **your own** last write. | Nothing at all when you are caught up, which is the common case. |
| `stale` | Whatever this node holds, with its lag reported. | No broker call. |
| `consistent_prefix` | A coherent point in the log's own order, possibly behind. | No broker call. |

**`session` is what an agent's tools use.** A seat that files a task and then
lists its project must see the task it just filed — that is the only
consistency property agent behaviour actually depends on, and it costs nothing
when the seat is caught up.

**`stale` is what the dashboard and the read API use.** A board that is two
hundred milliseconds behind is a board, and the answer carries its own lag so
a reader can tell.

**`linearizable` is for a decision.** Use it when the answer *decides*
something — an admission check, a gate, a number somebody is about to act on
irreversibly. It is the only level that establishes a position at the log's
current end before answering.

**`consistent_prefix` is the default for nobody, and it has to be asked for.**
It promises a *named prefix* of the log — everything up to a stated position,
with nothing from after it — and makes **no statement about age**. That makes
it weaker than `session` in a way the answer cannot show, so nothing defaults
to it: a surface that quietly downgraded to it would give every reader that
did not know to ask for more an answer they could not tell apart.

## How `linearizable` actually works

It is a **barrier append**, not a field check.

The reader publishes one record onto the domain's `barrier` subject and waits
for its own applier to reach it. The acknowledgement is what carries the
proof: at `replicas: 3` a `PubAck` means a majority of the raft group has the
record durably, so any position at or below it is one the whole group agrees
on. At `replicas: 1` there is no group and the acknowledgement proves only
that the one member has it — which is exactly as strong as that deployment is.

The barrier is a real record with a real cost. It is not free for most reads
and cheap for a few: **every** linearizable read waits for exactly one append
to come back. Measured on an idle loopback cluster that is about 1.5 ms from a
follower at three replicas, and about 0.4 ms solo with `stream.sync: always` —
plus whatever the applier takes to reach it. Treat those as a floor rather
than a budget.

The reason it is an append and not a `STREAM.INFO` field is that a field read
can be served by a member that has been partitioned away from its own group:
an isolated former leader answers with a last sequence it believes and the
majority has moved past. An append cannot be served that way, because there is
nothing to commit against.

## The twelve refusals

A read that cannot be served at the level asked for is **refused with a code**
rather than downgraded. Each code names a different thing to do.

| Code | What happened | What to do |
|---|---|---|
| `behind` | This node has not reached the position the read needs. | Wait — the answer carries a retry hint derived from this node's measured drain. It clears on its own. |
| `too_stale` | This node's lag is past what the read said it would accept. | Same, or accept more staleness. |
| `stalled` | This node's applied prefix has stopped moving. | Its rows are frozen, so a short answer would be wrong rather than old. Check the applier — `crewlet retention status` names the domain and its position. |
| `no_quorum` | The barrier did not commit: the broker answered and a majority did not agree. | Retry after the hint (4 s, the broker's own minimum election timeout). If it persists, a member is down or partitioned. |
| `broker_unreachable` | The broker did not answer at all. | Retry. Not the same as `no_quorum`, and the difference is where to look. |
| `log_full` | The log is at its byte ceiling and refuses appends, so no barrier can be written. | Raise the ceiling with `crewlet retention set-capacity`, or unblock the trim — `crewlet retention status` names the term. **`stale` and `session` keep answering**, so a full log costs `linearizable` rather than reads. |
| `deferred` | This node holds a record it cannot decode covering what this read is about. | Ask another node, or upgrade this one. No amount of waiting changes it. |
| `deferred_scope_unknown` | The deferred record's own scope could not be read, so nothing can be said about what it covers. | It blocks the whole domain, which is why it is a different code. Upgrade the node that is behind on the record version. |
| `below_floor` | Records this node never applied have been trimmed. | Its rows are missing state no replay can supply. The node has to adopt a peer's snapshot; see [Retention](retention.md). |
| `floor_unknown` | The published trim floor could not be read. | The third value blocks: guessing here keeps a node serving over a hole it cannot see. Check coordination. |
| `evicted` | This node has been removed from the fleet. | Nothing it holds is authoritative. Readmit it, or route elsewhere. |
| `wrong_stream` | The position this read was asked to reach is on another stream. | A caller bug, or a cursor from before a reanchor. |

Four of them are worth coming back to **this** node for — `behind`,
`no_quorum`, `broker_unreachable` and `stalled`. The rest are not, and the
distinction is in the code rather than in a retry loop's guesswork: a caller
that retried `deferred` would loop forever.

## Completeness is a different fact from freshness

Every set answer carries **`complete`** beside its level, and the two fail
independently.

An answer can be perfectly fresh and incomplete. A record this node cannot
decode is retained rather than applied, and if it touched something the read is
about, a row that would have entered the set has no local trace. The level says
nothing about that — so `complete: false` is its own field, with an
`incomplete` block saying how many records could intersect, the lowest position
among them, and the scope they declared.

It deliberately does **not** name the objects. Enumerating them would disclose
neither the direction of the difference — a row that would have entered the set
or one that would have left it — nor reliably the right identifiers, and it
would truncate. `direction` is always `"unknown"`, and it is a field rather
than an omission so a reader meets the fact instead of inferring it.

## What no level can strengthen

Three things are outside this vocabulary entirely, and asking for a stronger
level does not touch them:

1. **An apply bug.** N nodes applying one ordered log deterministically produce
   N identical copies *including of a bug*. Every level agrees; they agree on
   the wrong thing.
2. **A record this build cannot decode.** It is retained, not applied.
   `linearizable` establishes a position past it and still cannot show you what
   it would have written.
3. **What another company's system did.** A level is a statement about this
   log. A Jira issue that changed a second ago is not in it.

## Read-your-trigger is a floor, not the mechanism

An agent woken by a change sees that change. That is guaranteed, and it is
guaranteed by the *wake* carrying the record's own position — the turn waits
for its own applier to reach it — rather than by the level the turn's tools
then read at. `session` is what those tools use, and it is enough because the
trigger has already established the floor.

Do not read a stronger level to "make sure the trigger landed". It already did.

## See also

- **[Replication](replication.md)** — what the two positions in every answer
  mean, and what the log does and does not promise.
- **[Retention](retention.md)** — the trim, the terms that block it, and what a
  full log costs.
- **[API endpoints](../reference/api-endpoints.md)** — the level header and the
  refusal shape on the wire.
