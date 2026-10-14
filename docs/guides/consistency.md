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

**`session` is the write path's level, and a read surface offers it only
with a position.** It waits for *your own* high-water mark, which you have to
supply. A surface that accepted the level bare would wait for the zero
position, serve whatever that node happened to hold, and label the answer
`session`: a wrong label rather than a weaker answer. So the read grammar
accepts `read_level=session` **only beside `min_position`** — the position the
write you are reading back answered with — and refuses it without one, naming
the key and the two other honest asks, `linearizable` or `stale` with
`max_lag_seq`. A seat's tools carry no position and choose no level, so there
the ask is unreachable. Inside the engine the level is real and used: a write
waits for its own last write to be applied before it opens the snapshot it
decides from.

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
happened to omit the key:

| Surface | Default | May the caller choose? |
|---|---|---|
| A seat's own tools, inside a turn | `linearizable`, floored at its node's own writes and at the change that woke the turn ([below](#read-your-trigger-is-a-floor-not-the-mechanism)) | No |
| The operator MCP, about tracker content | `linearizable` | No |
| The dashboard and the REST read path | `stale` | Yes — `linearizable`, `stale`, `consistent_prefix`, or `session` beside a `min_position` |
| Any answer **about replication** — the retention report, Settings › Backups & retention's lag, whether a purge landed | `stale`, weakening to `consistent_prefix` | No — it is derived, not chosen |
| A person's git client, for every answer that names a ref — the refs a clone, a fetch or a push is shown, a protocol v2 `ls-refs`, a fetch by `want-ref` | `linearizable` | No |
| A git fetch of objects by id | none — an object id names the same bytes on every node; a want this node has not seen takes one `linearizable` sync first | No |
| The [code host](../concepts/code-host.md)'s REST reads, `/code/repos…` | `stale`, as the REST read path's | Yes — `linearizable`, `stale` or `consistent_prefix`; `session` is refused `400`, since none of the caller's writes lands on the code log through this surface |

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
Settings › Backups & retention say so in a sentence rather than printing the same figures
under the stronger name.

An agent cannot choose because the level is not a model's to pick — a tool
argument for it would be a model trading correctness for latency it cannot
perceive. The operator's reads cannot choose because nothing is wrong and the
person is deciding something about their own company: a knob that only ever
weakens the answer is one somebody turns once, forgets, and then reads a stale
board from for a year.

Every answer reports the level it was **actually** read at, so a caller that
asked for one and got another can tell.

**git has no level to choose, and needs none.** Every answer git is given
that names a ref is read at the log's head, so a node cut off from the fleet
answers `503` rather than an old head; an object fetch names ids, which mean
the same bytes on every node, so it is served from what the node holds — and
when the client asks for an object it learned of from a node further ahead,
the node reads the log's head once before it answers. A push is answered `ok`
for a ref only once the push's own record has been **applied**, never on a
record that is durable and not yet applied: see [A push, end to
end](../concepts/code-host.md#a-push-end-to-end).

**A client that must not conclude "not declared" from an old copy** — a
script about to create or import a repository — reads `/code/repos/{name}` at
`read_level=linearizable`: at the default `stale`, a repository declared a
moment ago may not be on this node's copy yet, and is answered `404`.

## Bounding staleness

`stale` on its own accepts an answer of any age. A caller that will not says so
with `max_lag_seconds`, `max_lag_seq`, or both — and the read refuses
`too_stale` past **whichever is reached first**. They are two readings of one
distance rather than two distances: the record count is what the broker
actually answers, and the duration is derived from it through this node's own
drain rate, so a caller who can say "at most 250 records behind" is naming the
measured quantity instead of an estimate made from it.

The lag a bound is checked against is this node's reading of its log's ends,
which a read is served from memory rather than asked of the broker per read: it
is re-read at most once a second, off the read's path, and **never more than
three seconds old** — a second's recheck plus the two seconds that re-read may
take. So a node may be up to three seconds' worth of the log's writes further
behind than the lag it reports. Ends nobody could re-read for those three
seconds are not served at all: the read is refused rather than checked against
a lag nobody has measured since.

Both are refused at every other level, because they are a staleness bound and
nothing else is — asking for `linearizable&max_lag_seq=250` is a caller who
believes they asked for something they did not.

A zero bound is not a bound: it accepts anything, which is what makes
declaring one the caller's own decision rather than a default somebody
inherits.

## Reading your own write back

Every write answers with the **position** its record landed at —
`<stream>@<generation>:<sequence>` — on the tool answer, the operator MCP's
answer and the retention routes alike, and every read takes it back as
`min_position`. The answer is then served from no earlier than that position,
**at whatever level was asked**:

- At `linearizable` it costs nothing: the barrier the read appends is already
  past any position a write on the same log answered with.
- At `session` it *is* the high-water mark the level waits for, which is what
  makes the level offerable to a caller outside the engine at all.
- At `stale` and `consistent_prefix` it turns "whatever this node holds" into
  "whatever this node holds, from here on". The lag is still checked against
  any bound and still reported on the answer; what changes is that a node
  which has not reached the position within the read budget refuses `behind`
  — with the same derived retry hint — rather than serving rows from before
  the write.

A position on another domain's log is refused `wrong_stream`, not waited for.

Two spellings, one triple: inside an answer a position is the object
`{stream, generation, seq}` (`seen_through`, `incomplete.from`, a listing's
`position`), and as a parameter — `min_position`, a cursor, `since` — it is the
token `<stream>@<generation>:<sequence>`, because a query string carries no
object. A tool answer's `position` is already the token, since that is what
its reader pastes back.

This is the wire's half of read-your-writes. A seat needs none of it, because
its reads are `linearizable`; a person's assistant filing an item over the
operator MCP and redrawing a board over the REST route holds nothing else it
could ask that board to include.

## How `linearizable` actually works

It is a **barrier append**, not a field check.

The reader publishes one record onto the domain's `barrier` subject and waits
for its own applier to reach it. The acknowledgement is what carries the
proof: at `replicas: 3` a `PubAck` means a majority of the raft group has the
record durably, so any position at or below it is one the whole group agrees
on. At `replicas: 1` there is no group and the acknowledgement proves only
that the one member has it — which is exactly as strong as that deployment is.

The barrier is a real record with a real cost, and every `linearizable` read
waits for one to come back — but concurrent reads on one node **share** them.
A read may use only a barrier that started after the read arrived: one already
in flight was proposed before the read existed, so it proves nothing about the
writes the read must see. A read that arrives while a barrier is in flight
waits that one out and joins the next, which serves every read that arrived in
the meantime. So a lone read costs one append, and a read on a busy node waits
for at most two and appends none of its own.

Measured by `make bench` on one four-vCPU machine holding all three members,
with `stream.sync: always`:

| | solo | three replicas |
|---|---|---|
| the barrier append alone (median) | 0.6 ms (0.2 ms with the fsync declined) | 3.5–3.9 ms (0.9–1.3 ms declined) |
| one reader: the whole read, median / p99 | 1.7–1.8 / 4.3–4.6 ms | 4.8–5.5 / 10–12.5 ms |
| one reader: reads a second | 535–555 | 171–197 |
| sixteen readers at once: barriers appended per read | 0.22 | 0.14–0.20 |
| sixteen readers at once: reads a second, together | 4,600–4,700 | 1,120–1,610 |
| sixteen readers at once: the whole read, median / p99 | 3.3 / 8.3–8.5 ms | 9.6–12.5 / 19–31 ms |

Sixteen readers calling back to back get at most about eight reads out of a
barrier rather than sixteen, because the readers one barrier served call again
while the next is already in flight and wait for the one after it: measured,
each barrier alone served eight, and whole reads — whose waits for the applier
spread the readers out — four to seven. Treat all of it as a floor rather than
a budget: real members add a network hop between them and do not share one
machine.

The reason it is an append and not a `STREAM.INFO` field is that a field read
can be served by a member that has been partitioned away from its own group:
an isolated former leader answers with a last sequence it believes and the
majority has moved past. An append cannot be served that way, because there is
nothing to commit against.

## The thirteen refusals

A read that cannot be served at the level asked for is **refused with a code**
rather than downgraded. Each code names a different thing to do.

| Code | What happened | What to do |
|---|---|---|
| `behind` | This node has not reached the position the read needs — including a node below the published trim floor whose missing records the log still holds, which it is replaying. | Wait — the answer carries a retry hint derived from this node's measured drain. It clears on its own. |
| `too_stale` | This node's lag is past what the read said it would accept. | Same, or accept more staleness. |
| `stalled` | This node's applied prefix has stopped moving — its applier halted, or has been retrying a failure it cannot get past for longer than the retry budget. | Its rows are frozen, so a short answer would be wrong rather than old. Check the applier — `crewlet retention status` names the domain, its position and the error it is retrying. A retried failure clears on its own the moment an attempt succeeds. A `consistent_prefix` read, which makes no statement about age, is still answered by a stalled node at or above the published trim floor — a frozen prefix is still a coherent one — and refused by one below it. |
| `no_quorum` | The barrier did not commit in time: the broker answered and a majority did not agree within the barrier's own five seconds — or, on a read another node asked for, within what that node's attempt left, less a second for the answer to reach it. | Retry after the hint (4 s, the broker's own minimum election timeout). If it persists, a member is down or partitioned. |
| `broker_unreachable` | The broker did not answer at all — the barrier a linearizable read appends, or this node's own reading of its log's ends (not re-read for three seconds) or its eviction row. | Retry. Not the same as `no_quorum`, and the difference is where to look. |
| `log_full` | The log is at the byte ceiling its ordinary appends are held to — on a log that claims identity (the tracker, pages and code logs) a sixteenth below the broker's, the rest being [kept for gate records](retention.md#the-gate-reserve) — so no barrier can be written. | Raise the ceiling with `crewlet retention set-capacity`, or unblock the trim — `crewlet retention status` names the term. **`stale` keeps answering**, so a full log costs `linearizable` reads — every seat tool read among them — rather than every read. |
| `deferred` | This node holds a record it cannot decode covering what this read is about. | Ask another node, or upgrade this one. No amount of waiting changes it. |
| `deferred_scope_unknown` | The deferred record's own scope could not be read, so nothing can be said about what it covers. | It blocks the whole domain, which is why it is a different code. Upgrade the node that is behind on the record version. |
| `below_floor` | Records this node never applied are gone from the log. | Its rows are missing state no replay can supply. The node adopts a peer's snapshot on its own; ask another node meanwhile. See [Retention](retention.md#the-join-runbook). |
| `floor_unknown` | The published trim floor has not been read for four heartbeats (a minute). A read is served the floor this node last read — refreshed at most once a second, off the read's path, so a read asks the broker nothing — and a blip of coordination therefore refuses nothing; a floor nobody could refresh for the minute is no answer, and the refusal names how long ago it was last read. | An unreadable floor blocks: guessing here keeps a node serving over a hole it cannot see. Check coordination. |
| `evicted` | This node has been removed from the fleet. | Nothing it holds is authoritative. Readmit it, or route elsewhere. |
| `maintenance` | This node runs in a [capacity window](retention.md#changing-a-logs-ceiling) (`-mode maintenance` or `-mode seal`), which publishes nothing — and a `linearizable` read proves the log's end by appending a barrier to it. | Ask again once the fleet is back in normal mode. Every other level appends nothing and is still answered. Every node of a fleet in a window is in one, so no other node answers it either. |
| `wrong_stream` | The position this read was asked to reach is on another stream — including a `min_position` naming another domain's log, which is refused at every level rather than quietly dropped. Or this node's own log is not the one its rows are keyed to: its checkpoint is past the log's end — what a broker restored from an older copy looks like, since it keeps the stream's creation instant — or the log holds, at that checkpoint, another record than the one it consumed there, which is the same restore written past this node's rows, or the stream was deleted and rebuilt — found at boot against the checkpoint, or under a running node by the position heartbeat and by any write at an expectation of zero, from the stream's own creation instant — or a peer re-anchored the log past this node's generation, so it continues from that peer's rows rather than this node's. Every one of these findings refuses the node's **writes** with the same word. | A caller bug, a cursor from before a reanchor, a recreated stream or a restored broker; see [Retention](retention.md#re-anchoring-a-recreated-or-restored-log). A peer's reanchor clears on its own, once this node has [adopted](retention.md#a-node-a-peer-re-anchored-past) a snapshot from the new generation. |

Four of them are worth coming back to **this** node for — `behind`,
`no_quorum`, `broker_unreachable` and `stalled`. The rest are not, and the
distinction is in the code rather than in a retry loop's guesswork: a caller
that retried `deferred` would loop forever.

Every refusal is about **one domain's copy**. A node whose tracker copy refuses
`stalled` or `below_floor` still answers knowledge-base reads from its own
pages, and the estate's router sends a seat's call on the tracker to a data
node whose copy of the tracker is in service, while the node's other logs keep
answering here
([ADR-0031](https://github.com/crewlet/crewlet/blob/main/adr/0031-a-domains-fault-takes-only-its-own-copy-out-of-service.md);
see [how a request reaches the estate](../concepts/scaling.md#how-a-request-reaches-the-estate)).

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

An agent woken by a change to the engine's own tracker or knowledge base sees
that change. That is guaranteed by the *wake* carrying the record's own
position — `trigger_position`, where on its log the change was committed (see
[the event system](../concepts/event-system.md)) — and not by the level the
turn's tools then read at:

1. Before the turn's first read, the node hands that position to its
   **read-your-writes floors**: one table per node, the same one every write
   the node makes raises.
2. Every tracker or knowledge-base read the turn makes carries the floor on its
   own domain's log to whichever node answers it — a tracker read the tracker
   log's, a page read the knowledge base's — this node's own copy included.
3. That holder waits up to two seconds to have applied it, or answers `behind`
   and the next holder is asked. A holder asked by another node waits only as
   long as that node's attempt leaves it, less a second for the answer's trip
   back, so its `behind` arrives while the asker is still listening — never as
   the silence of a node that is gone.

The floor travels with the request rather than being waited for before the
turn opens, because the node running the turn may hold no data at all and have
no applier of its own to wait on. A floor on one domain's log never holds up a
read of the other: a tracker read does not depend on the knowledge base's rows.

**What the floor does not reach.** The ranked searches — `search_knowledge`,
`search_work_items` and the turn-start knowledge block — read an index each
node builds behind its own rows, so no log position describes them and they
carry no floor: a seat woken by a page created a moment ago may not find it by
search yet, and finds it by listing or by its id. A vendor's wake — a Slack
message, a Jira issue — carries no position at all, because what changed is in
another company's system.

So a seat's `linearizable` reads are not what makes it see its own trigger.
What they buy is the other half: that an answer the turn *decides* on is not
one from before the read arrived. The operator's reads are `linearizable` too
and are not settable: the person asking is deciding something about their own
company, and operators are few.

## See also

- **[Replication](replication.md)** — what the two positions in every answer
  mean, and what the log does and does not promise.
- **[Retention](retention.md)** — the trim, the terms that block it, and what a
  full log costs.
- **[API endpoints](../reference/api-endpoints.md)** — the level header and the
  refusal shape on the wire.
