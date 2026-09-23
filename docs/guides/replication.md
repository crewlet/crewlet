# Replication

Crewlet's own tracker and knowledge base are **replicated state machines**. One
ordered stream per domain is the write-ahead log; every node applies it into
its own SQL database; the checkpoint commits in the same transaction as the
rows it covers. That last clause is the whole design: a node's position and its
rows can never disagree, because they are written together or not at all.

This page is for operators. It covers what a write means, what an
acknowledgement promises, how far behind a node can be, and what the design
does **not** promise.

## Three numbers that are not the same number

Confusing these is the single most common misreading of this system, so they
are stated first.

| Number | Where it lives | What it is for |
|---|---|---|
| **the arbitration anchor** | `statelog_anchor` | The position of the last record this node consumed on a subject, *whatever that record then did*. It is what the broker arbitrates a new write against. |
| **`version`** | a column on every object row | The object's accepted state. It is what a caller's `if_match` compares against. |
| **the barrier comparison** | computed | `MAX(version, scoped_through)` — what a read barrier compares, because a record can change an object without being *about* it. |

The anchor and the version are equal only while every accepted record produces
rows, and an apply **gate** is by definition the rule that makes them differ.
An evicted node's append is accepted by the broker at sequence 102 and dropped
by every applier: the object's version stays at 100 on every node while the
broker's last message on that subject is 102. A writer that formed its
expectation from the version would be refused, re-read 100, and burn its whole
round budget — for at least the trim's age floor, and unbounded above while any
retention term blocks.

**`scoped_through`** is the third. A record bumps `version` only on its **own**
subject; every other row it writes carries `scoped_through` instead. A barrier
that compared only `version` would let a read past a record that had already
changed the row it was about to return.

## A write has three outcomes

Not two. `applied`, `pending`, `unknown` — and a caller does something
different with each.

- **`applied`** — the record is durable at its position **and** this node has
  applied it. Read it back and you will see it.
- **`pending`** — the record is durable at its position and this node has not
  applied it yet. **The write succeeded.** Do not retry it; retrying publishes
  a second record. What is not yet true is that you can read it back here.
- **`unknown`** — nothing can be established about the record. It may be on the
  log and it may not. This is the only outcome a retry is correct for, and the
  retry carries the same operation id so the ledger collapses a duplicate.

`pending` is the outcome an ordinary busy fleet produces most often under load:
the applier is 16 seconds into a bulk apply and a small write's five-second wait
for its own record expires. Five seconds is the applier's own stall grace
divided by twelve — many times an ordinary batch commit and its linger, and
short enough that a caller holding a request open learns "durable, unresolved
here" rather than waiting out a node that has stopped applying. The record is
safe. A caller that treated `pending` as a failure would double every write it
made during a burst.

### `applied` is not permanent

Three gates can drop a record **after** a node has applied it, and all three
are deliberate:

- **The eviction gate.** A record written by a node the fleet evicted before
  the record's own position is dropped everywhere. A node that applied it
  before learning it was evicted will drop it on replay.
- **The deletion gate.** A record about a task a purge destroyed applies
  nowhere, for ever. This is what stops a redelivery months later resurrecting
  rows an operator deliberately removed.
- **The retirement gate.** A record naming a kind the engine once published and
  no longer applies is read past rather than faulted on. It is the only one of
  the three that is about the *build* rather than about the fleet or an
  operator's decision — see [the other direction](#the-other-direction-a-kind-that-was-removed).

None is a bug being worked around. The first two are what make an eviction and
a purge mean something on a system where the log outlives the decision; the
third is what lets the engine stop writing a kind without stranding the nodes
that still have to read past one.

### A record whose scope meets a deferred scope is deferred too

A record this build cannot decode is **retained**, not dropped: the bytes are
the only copy, and a rolling upgrade puts records on the wire the older half
has never heard of. What follows is the rule that keeps that safe:

> If a record's declared scope meets a deferred record's scope, it is deferred
> too.

So an object's rows on any node are always a **prefix** of that object's own
record sequence. There is no state in which record 5 has applied and record 3
has not. That is what lets a reader say "this node is behind" rather than "this
node has a hole in the middle of this object's history".

## What an acknowledgement promises

`stream.sync` decides it, and the default is the strong value.

| Setting | An acknowledged write has reached |
|---|---|
| `always` (default) | the disk, on every replica that acknowledged. |
| a duration, e.g. `30s` | the replicas' memory, and their disks within that window. |

The five failure classes, and which the design survives at each topology:

| Failure | Single node | Clustered, one host | Clustered across hosts | External NATS |
|---|---|---|---|---|
| **F1** the process is killed | survived | survived | survived | survived |
| **F2** the host loses power | survived only with `sync: always` | survived for any one host | survived for any one host | whatever that cluster promises |
| **F3** the disk is lost | not survived | survived | survived | whatever that cluster promises |
| **F4** the filesystem corrupts | not survived | survived for any one host's disk | survived for any one host's disk | whatever that cluster promises |
| **F5** the site is lost | not survived | not survived | **only if the hosts are in different sites, which the engine does not know and does not claim** | ask your NATS operator |

On `stream.type: nats` the engine **refuses** `stream.sync` rather than
warning: it is a server option on a server the engine does not run, and a field
that named a mechanism it cannot reach would let an operator hold a durability
belief nothing delivers. Ask the NATS operator for `sync_interval: always`
instead; `crewlet validate` says it cannot check it.

## Above the floor and below it — two different regimes

This is the sentence the backup schedule hangs on.

**Above the trim floor**, the log holds every record on R replicas and every
node holds the applied rows. Losing a node loses nothing.

**Below the trim floor the log holds nothing, and each node's own database file
is the only copy of that history** — N of them, independent, none replicated.
Losing history below the floor takes all N disks, and it is covered **only** by
the backup gate.

That is why the trim refuses to advance past a floor no backup has reached.
The backup schedule is a **correctness input**, not hygiene. See
[Retention](retention.md).

## Replication lag is two positions

Not one number. Every node publishes both:

- **`seq`** — the record it has *consumed*: its checkpoint, the contiguous
  prefix it has moved over.
- **`applied_through`** — the prefix it has actually *applied*, which is lower
  whenever a record was retained rather than applied.

Folding them into one would make a node that is applying nothing while its
position advances look identical to one that is fully caught up. `crewlet
retention status` prints both, per node and per domain.

### The bulk-apply degradation, priced

One applier per domain, one goroutine, one totally ordered log. The measured
drain is at least 2 000 rows/s. What binds is not a company's average load
against it; it is the burst.

**Rows here are database rows, not records.** Every figure in this section
counts the rows an apply writes. The `crewlet.statelog.drain.records_per_second`
gauge in [Metrics](../reference/metrics.md) counts the log's **records**
instead — the unit a node's backlog is counted in, and so the rate its lag in
seconds and its retry hints are derived from — and a record writes as many
rows as its apply needs, so the gauge and these figures are different numbers
that cannot be compared.

The 2 000 rows/s floor is deliberately conservative, and it is the number
every figure below is derived from. An applier writes a record's child rows —
tags, watchers, relations, dependency mirrors, the inverted index's postings —
as **multi-row inserts chunked to the engine's probed bind-parameter limit**,
rather than one statement per row. On the pinned driver that limit is 2 000
parameters, so a seven-column row batches 285 to a statement and a
three-column row 666: an 8 000-row apply is 22 statements rather than 8 000.
Measured unloaded that shape drains about four times faster than one statement
per row; measured on a loaded CI runner under the race detector, closer to
1.5 times. The floor above is the loaded, contended figure, so a fleet sized
against it has margin rather than a number it has to hope for.

One maximal bulk update is about 31 800 rows, which is **about 16 seconds of
applier occupancy on every peer**. For those 16 seconds two things degrade
fleet-wide without tripping any alarm:

- an unrelated single-object write's five-second wait expires and returns
  **`pending`** with its position and its lag — correct, and designed for;
- a barriered read answers `behind` with a computed `retry_after_seconds` of
  about 16.

Both are the system working. The `crewlet.statelog.apply.tx.duration` and
`crewlet.statelog.apply.batch.rows` histograms in
[Metrics](../reference/metrics.md) record every apply transaction and the rows
it wrote, so this is a measured property of your fleet rather than a surprise.

**The occupancy is per domain, not per node.** Every domain's applier writes
the same replicated database, and a node hands that database's write lock to
its writers in the order they asked for it. A bulk update commits one
transaction at a time — at most 4 000 rows, about two seconds at the measured
drain — and a waiting writer takes the lock as soon as the one in front of it
commits. So a bulk update in the work tracker delays the knowledge base's
apply by the transactions already queued ahead of it, at most one per other
writer on that node, never by the whole 16 seconds.

A writer that does not reach the front within `store.busy_timeout_seconds`
fails retryably and rejoins the line, logged as `store_tx_retry` naming the
knob. With three domains applying and a bulk update in flight, the default
five seconds is close to the three transactions a fourth writer can
legitimately wait behind — so that log line on a node doing bulk work is the
signal to raise it rather than a fault. Raise the knob before you widen
anything else: a longer per-waiter bound lets one stuck holder block the line
for longer, and this way the line still drains in order.

**No apply transaction is ever aborted by a commit elsewhere in the database,
and none is ever re-run because of one.** Every write transaction takes the
lock when it begins rather than at its first write, so there is no window
between an applier's read and its write for another commit to land in. The
`crewlet.statelog.apply.tx.aborts` counter in
[Metrics](../reference/metrics.md) reads zero on a healthy node for that
reason, and a non-zero count is the retry budget being spent on something
else.

## What a rolling upgrade blocks

A record at a version this build cannot decode is retained, and everything
whose scope meets it is retained too. That is bounded rather than open-ended:
the **deferral grace** is 30 minutes, past which the alarm fires and names the
position and the scope.

So a rolling upgrade should finish inside that window. An upgrade that stalls
half-done leaves the old nodes holding records they cannot apply and refusing
reads about the objects those records touched — with `deferred` naming exactly
what to do, which is finish the upgrade.

**The upgraded node applies what it retained at its next boot**, before its
applier consumes anything new: every retained record it can now read, in log
order, each released in the transaction that applied it. A record whose scope
met a retained one is applied after it, which is what makes the objects' rows
a prefix of their history again rather than a hole. A record still above the
new build's version stays retained, with everything it covers, until a build
that reads it boots.

### The other direction: a kind that was removed

The deferral above handles a **newer** peer's records, and it is keyed on the
record *version*. The mirror case is a record from an **older** peer naming a
kind the new build has removed, and the version gate cannot see it: the record
is at a version the new build reads perfectly, and it is the *kind* that is
gone. It would reach the applier's dispatch, match nothing, and fault.

So a removed kind is **retired** rather than deleted. It stops being
publishable — nothing mints a subject for it and no table is classified for
it — and it goes on being consumable, producing no rows, which is what the
retirement gate does. Without that, one old peer publishing during an upgrade,
or one old record still inside the log's retention window, stops the *newest*
node's applier at that position for as long as the record is in the log:
`stream.tracker_retention` bounds that, and nothing does where the trim cannot
advance.

A retired record is reported like any other gated one — a
`statelog_record_gated` line naming `retired`, and the
`crewlet.statelog.records_gated` counter under the same `gate` value. A kind
that was never published is not retired and still faults, which is what keeps
this from hiding a writer publishing a kind it never declared. Sprints, removed
from the work tracker, are the first retired kind.

## The five capacity ceilings

1. **Each log's byte ceiling**: `stream.tracker_log_max_bytes`,
   `stream.tracker_vectors_max_bytes` and `stream.pages_log_max_bytes`, sized
   together as [below](#how-the-byte-ceilings-are-sized). A full log
   **refuses** appends rather than shedding old records; see
   [Retention](retention.md).
2. **The trim floor** — how far back the log can be replayed from, which is
   what bounds how long a node may be away.
3. **The store's own size** — every node is a full replica, so the corpus is
   held N times.
4. **Applier occupancy** — the 16 seconds above.
5. **The snapshot repository** — one artefact per node, sized in
   [Retention](retention.md).

### How the byte ceilings are sized

A byte ceiling is a **reservation**. The broker grants it in full when it
creates the stream, before a single record is written, and refuses to create a
stream whose ceiling it could not honour. So the three logs compete for one
number, and a node sizes them together, once, when it creates their streams:

| Step | What happens |
|---|---|
| **What the broker can grant** | Read from the broker itself. An embedded broker's limit is three quarters of the free space on the volume holding `stream.store_dir`, counting what its own streams already hold there; an external one's is the NATS account's JetStream limit. What counts against it is the ceilings already granted, not the bytes stored. |
| **The logs' share** | Half of that, with the ceilings the logs' own streams already hold counted as theirs. The other half is for everything that reserves nothing: every mailbox, every coordination bucket and the snapshot a joining node reads. |
| **Each log's ask** | Its Tier A field when you set one. Unset, the mutation log asks for a quarter of the stream volume's free space (4..64 GiB), the knowledge base's log for a quarter of that (1..16 GiB), and the vector changelog for 16 GiB capped by the same quarter. |
| **The fit** | A ceiling you set is never scaled. The unset ones share what is left of the logs' half in proportion to what each asked for, and none goes below 1 GiB. |

**A stream that already exists keeps its ceiling.** Sizing decides what a
missing stream is created with and nothing else: a booting node never rewrites
a running stream's configuration, and the broker never re-checks a reservation
it has already granted. A log created larger than today's sizing would make it
boots as it is, and the node logs `jetstream_stream_capacity_differs` with both
numbers. A knowledge-base log created before it joined the budget, at a fixed
4 GiB, is the common case, and it is harmless: its reservation was granted
when it was made. To reclaim it (or to raise any log), use
[`crewlet retention set-capacity`](retention.md#changing-a-logs-ceiling).

**A boot that still cannot reserve a log says why.** When even the floors, or
a ceiling you set, do not fit, the node refuses to boot with an error naming
the stream, the bytes it needed, the bytes the broker had left and what sets
that limit, and the Tier A field the ceiling came from:

```
engine: the broker refused to reserve the pages log's ceiling:
CREWLET_PAGES_LOG needed 1073741824 bytes and the broker had 536870912 bytes
left to reserve (… of its …-byte limit already reserved), and that limit is
three quarters of the free space on the volume holding stream.store_dir
(/var/lib/crewlet/stream), counting what the broker's streams already hold
there. stream.pages_log_max_bytes is unset, so the ceiling was derived and
scaled into the state logs' share of the broker, and it goes no lower than
1073741824 bytes. Give the broker more room; the state logs that already exist
keep the ceilings they were created with, and no Tier A setting changes them: …
```

The remedies are the ones it lists. Give the broker more room: on the embedded
topology, free space on that volume (a first boot needs at least 4 GiB free
there, three quarters of which is the three 1 GiB floors), which the broker
measures again when the node next starts. Or, when the refused log's ceiling is
above the 1 GiB floor, set its field to a smaller ceiling, and the refusal says
so when that applies.

A log that already exists cannot be shrunk to make room from here. Its ceiling
changes only through `crewlet retention set-capacity`, which runs on a node
whose state logs are up, and every mode starts them, `maintenance` and `seal`
included: a node refused here cannot run it.

## Three things CI cannot prove

Stated here because the alternative is implying a guarantee nobody measured:

1. **Durability under real power loss.** The fsync counting and the
   fault-injection suites test the code path; a real machine losing power at a
   real disk's worst moment is not reproducible in CI.
2. **Behaviour of an external NATS cluster.** The engine does not run it and
   cannot assert its settings.
3. **Search capacity under production load.** The scan benchmark has a
   concurrency axis, but the supported corpus at your concurrency is a
   projection until it runs on your hardware.

## What the design does not promise

N nodes applying one ordered log deterministically produce N identical copies
**including of an apply bug**. A replica set protects against machine loss and
against nothing else.

The residual is real and is not engineered away: a bug that corrupts silently
and is noticed after the backup window has rolled costs history. That is why
the trim refuses to advance past a floor no backup has reached — the backup
gate is not ceremony — and why the restore test has a cadence rather than being
a thing somebody did once.

## See also

- **[Read consistency](consistency.md)** — the four levels and the twelve
  refusals.
- **[Retention](retention.md)** — the trim, the six terms, snapshots and the
  join runbook.
- **[Backups & restore](backup.md)** — the artefact, and what its interval
  actually means.
- **[Running a fleet](fleet.md)** — topologies, seat placement, rolling
  upgrades.
