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

**`linearizable` is for a decision, which is why it is what an agent reads
at.** Use it when the answer *decides* something — an admission check, a gate,
a number somebody is about to act on irreversibly. It is the only level that
establishes a position at the log's current end before answering. Every seat
tool read is one of these: a create that refuses a project the company does
not have, a hand-off that names a colleague, a turn that reports what it
found. A seat has no screen on which to notice it was reading a stale copy, so
it never reads one.

**`stale` is what the dashboard and the read API use.** A board that is two
hundred milliseconds behind is a board, and the answer carries its own lag so
a reader can tell. A tile that took a barrier append to redraw would put the
fleet's whole read rate on the log to remove a staleness the next redraw
removes anyway.

**`session` is the write path's level, and no read surface offers it.** It
waits for *your own* high-water mark, which you have to supply — and nothing
outside the engine can. An HTTP request holds no position, and a seat's tools
carry none either. A surface that accepted `session` would wait for the zero
position, serve whatever that node happened to hold, and label the answer
`session`: a wrong label rather than a weaker answer. So `read_level=session`
is **refused** by the read grammar, naming the two honest asks — `linearizable`,
or `stale` with `max_lag_seq`. Inside the engine the level is real and used:
a write waits for its own last write to be applied before it opens the snapshot
it decides from.

That wait is **per log, not per object**. A node that has just published a bulk
update waits for it to apply locally before its next write on that log — any
subject — and may be refused `behind`, naming its own record. That is what
read-your-writes means on the write path: the alternative is a write that
cannot see the write before it. It costs nothing when the node is caught up,
which is every ordinary write.

**`consistent_prefix` is the default for nobody, and it is either asked for or
resolved to.** It promises a *named prefix* of the log — everything up to a
stated position, with nothing from after it — and makes **no statement about
age**. That makes it weaker than `session` in a way the answer cannot show, so
nothing defaults to it: a surface that quietly downgraded to it would give
every reader that did not know to ask for more an answer they could not tell
apart. The dashboard may ask for it, and a replication answer resolves to it
when the lag is unmeasurable — which is not a downgrade but the honest name
for what is left when there is no age to claim, and the answer says so.

## Which surface reads at which level

The level is a property of the **surface asking**, never of the caller who
happened to omit the key. There are four:

| Surface | Default | May the caller choose? |
|---|---|---|
| A seat's own tools, inside a turn | `linearizable` | No |
| The operator MCP, about tracker content | `linearizable` | No |
| The dashboard and the REST read path | `stale` | Yes — `linearizable`, `stale` or `consistent_prefix` |
| Any answer **about replication** — the retention report, the Fleet screen's lag, whether a purge landed | `stale`, weakening to `consistent_prefix` | No — it is derived, not chosen |

**Only the screen chooses**, and the reason is that only the screen can see
what it got: the level and the lag are rendered beside the rows, so a person
who asks for a weaker answer is shown the one they were given.

**A replication answer is the one row where the SUBJECT decides the level, not
the caller**, and it holds on every surface including the operator MCP.
`linearizable` means *every mutation committed anywhere in the company before
this read was issued is in the answer*, and it is established by appending a
barrier and waiting through its position. But the question these answers ask is
**how far behind that same log this node is** — the barrier is the instrument
and its health is the subject. A node cannot produce a `linearizable` answer to
a question about its own replication, so that is not a stronger answer costing
more; it is a level that cannot be served, refusing in precisely the incident
somebody opened the page for.

It weakens one step further when this node could not measure its own lag at
all — the broker unreachable, or coordination — because `stale` is a claim
about *age* and there is then no age to claim. `crewlet retention status` and
the Fleet screen say so in a sentence rather than printing the same figures
under the stronger name.

An agent cannot choose because the level is not a model's to pick — a tool
argument for it would be a model trading correctness for latency it cannot
perceive. The operator's reads cannot choose because nothing is wrong and the
person is deciding something about their own company: a knob that only ever
weakens the answer is one somebody turns once, forgets, and then reads a stale
board from for a year.

Every answer reports the level it was **actually** read at, so a caller that
asked for one and got another can tell.

## Bounding staleness

`stale` on its own accepts an answer of any age. A caller that will not says so
with `max_lag_seconds`, `max_lag_seq`, or both — and the read refuses
`too_stale` past **whichever is reached first**. They are two readings of one
distance rather than two distances: the record count is what the broker
actually answers, and the duration is derived from it through this node's own
drain rate, so a caller who can say "at most 250 records behind" is naming the
measured quantity instead of an estimate made from it.

Both are refused at every other level, because they are a staleness bound and
nothing else is — asking for `linearizable&max_lag_seq=250` is a caller who
believes they asked for something they did not.

A zero bound is not a bound: it accepts anything, which is what makes
declaring one the caller's own decision rather than a default somebody
inherits.

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
| `log_full` | The log is at its byte ceiling and refuses appends, so no barrier can be written. | Raise the ceiling with `crewlet retention set-capacity`, or unblock the trim — `crewlet retention status` names the term. **`stale` keeps answering**, so a full log costs `linearizable` reads — every seat tool read among them — rather than every read. |
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
then read at.

So a seat's `linearizable` reads are not what makes it see its own trigger; the
floor was already established before the turn opened. What they buy is the
other half: that an answer the turn *decides* on is not one from before the
read arrived.

## See also

- **[Replication](replication.md)** — what the two positions in every answer
  mean, and what the log does and does not promise.
- **[Retention](retention.md)** — the trim, the terms that block it, and what a
  full log costs.
- **[API endpoints](../reference/api-endpoints.md)** — the level header and the
  refusal shape on the wire.
