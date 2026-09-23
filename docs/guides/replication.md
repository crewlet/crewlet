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

**Below the trim floor the log may already hold nothing — the trim has licensed
deleting it — and each node's own database file is the only copy of that
history** — N of them, independent, none replicated. Losing history below the
floor takes all N disks, and it is covered **only** by the backup gate.

That is why the trim refuses to advance past a floor no backup has reached.
The backup schedule is a **correctness input**, not hygiene. See
[Retention](retention.md).

## A node resumes from its rows, never from its reader

Each node reads each log through a durable consumer of its own on the broker,
named after the node. That consumer's position is a second, weaker number than
the checkpoint: it moves *after* the commit, and the broker keeps it when the
node's database does not. The checkpoint is what the node resumes from — but
resuming can only drop what arrives below it. Nothing on the node's side can
make the broker hand over again a record it has already delivered and been told
was applied.

So at every boot the node compares the two, and a consumer that disagrees with
the rows is deleted and created again at the checkpoint. Each rebuild is logged
as `jetstream_domain_consumer_rebuilt`, naming the `drift`:

| `drift` | What it means | What it would have cost |
|---|---|---|
| `acknowledged_past_checkpoint` (a `WARN`) | The broker was told records past the checkpoint were applied, so this node's replicated database is older than its reader. The database was deleted, or restored from a backup. | The node never hydrates, and every write on the missing objects refuses `behind`. On the vector log, where nothing checks for gaps, the skipped records' rows are just missing. |
| `delivered_past_checkpoint` | Records past the checkpoint were handed to a process that has since stopped. | The node stays behind until the 30-second ack window returns them. |
| `in_flight_for_a_gone_reader` | Deliveries at or below the checkpoint are held for a process that has since stopped. | Each one holds a slot of the in-flight ceiling for 30 seconds. When they fill it, nothing new arrives. |
| `behind_checkpoint` | The rows moved without the reader, which is what adopting a snapshot at boot does. | Every record in between is delivered only to be dropped. |

A clean restart logs none of these: a node that applied and acknowledged
everything it was handed keeps its consumer as it is.

A node below the trim floor is not ahead of its rows either, although its
reader reports records past the checkpoint as read: the broker moves a reader
over the records the trim removed, to the first one the log still holds. The
node reads the log's first sequence to tell the two apart. If it cannot read
it, it judges the raw positions and rebuilds, and the
`acknowledged_past_checkpoint` warning then says the log's first sequence was
unreadable, so it may be either case.

A rebuild is a delete and a create on the broker, and on a fleet whose broker
is not answering, either one can fail. What happens then depends on the drift:

- **`behind_checkpoint` and `in_flight_for_a_gone_reader`**: the node asks the
  broker which reader it now holds, keeps that one, and logs
  `jetstream_domain_consumer_rebuild_failed` as a `WARN`. Neither drift has
  handed over a record past the checkpoint, so keeping the reader costs what
  the table says and loses nothing. The next boot rebuilds it.
- **`acknowledged_past_checkpoint` and `delivered_past_checkpoint`**: the boot
  fails and names the domain. A node left on that reader could wait for ever.
- **Any drift where the broker holds no reader after the failure, or cannot say
  which it holds**: the boot fails too. A reader the node guessed at could be
  one that no longer exists, and every read through it would fail while the
  node reported itself up.

A rebuild touches only this node. No peer reads through its consumer, and no
retention term reads it either: the trim works from the positions each node
publishes from its own rows. What the rebuild makes safe is **losing the
replicated database while the broker keeps its estate**. A node started on an
empty `crewlet-replicated.db` replays each log from the broker, or adopts a
peer's snapshot where the trim has already removed the start of one. It no
longer waits on records its old reader already acknowledged.

## Replication lag is two positions

Not one number. Every node publishes both:

- **`seq`** — the record it has *consumed*: its checkpoint, the contiguous
  prefix it has moved over.
- **`applied_through`** — the prefix it has actually *applied*, which is lower
  whenever a record was retained rather than applied.

Folding them into one would make a node that is applying nothing while its
position advances look identical to one that is fully caught up. `crewlet
retention status` prints both, per node and per domain.

**Lag does not move a node's seats, at any size.** A node that is behind keeps
every seat it holds and claims no new ones until it is level — see [a copy
that is behind, and a copy that is wrong](../concepts/seat-ownership.md#a-copy-that-is-behind-and-a-copy-that-is-wrong)
for the six states that do move work, none of which is a distance. What lag
does bound is how fresh an answer a read can ask for
([Read Consistency](consistency.md)).

### The bulk-apply degradation, priced

One applier per domain, one goroutine, one totally ordered log. The measured
drain is at least 2 000 rows/s and the steady-state load of the reference
company is 0.244 rows/s — 0.012 %. What binds is not the average; it is the
burst.

That floor is deliberately conservative, and it is the number every figure
below is derived from. An applier writes a record's child rows — tags,
watchers, relations, dependency mirrors, the inverted index's postings — as
**multi-row inserts chunked to the engine's probed bind-parameter limit**,
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

Both are the system working. `crewlet retention status` publishes the longest
apply transaction and the longest batch actually observed, so this is a
measured property of your fleet rather than a surprise.

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

## The six capacity ceilings

1. **The broker's storage limit** — `stream.store_max_bytes`. Every ceiling
   below is a *reservation* checked against this one number, so it is the
   ceiling above the ceilings. Unset, the embedded broker takes three quarters
   of the free space on `stream.store_dir` when its JetStream comes up,
   measured once. **Divide it when more than one engine shares a filesystem**:
   free space bounds their sum, not each of them.
2. **Each log's byte ceiling**: `stream.tracker_log_max_bytes`,
   `stream.tracker_vectors_max_bytes` and `stream.pages_log_max_bytes`, sized
   together as [below](#how-the-byte-ceilings-are-sized). A full log
   **refuses** appends rather than shedding old records; see
   [Retention](retention.md).
3. **The trim floor** — how far back the log can be replayed from, which is
   what bounds how long a node may be away.
4. **The store's own size** — every node is a full replica, so the corpus is
   held N times.
5. **Applier occupancy** — the 16 seconds above.
6. **The snapshot repository** — one artefact per node, sized in
   [Retention](retention.md).

### How the byte ceilings are sized

A byte ceiling is a **reservation**. The broker grants it in full when it
creates the stream, before a single record is written, and refuses to create a
stream whose ceiling it could not honour. So the three logs compete for one
number, and a node sizes them together, once, when it creates their streams:

| Step | What happens |
|---|---|
| **What the broker can grant** | Read from the broker itself. An embedded broker's limit is `stream.store_max_bytes` where you set one, and otherwise three quarters of the free space on the volume holding `stream.store_dir`, counting what its own streams already hold there; an external one's is the NATS account's JetStream limit. What counts against it is the ceilings already granted, not the bytes stored. |
| **The logs' share** | Half of that, with the ceilings the logs' own streams already hold counted as theirs, so a restart divides the same half the first boot did. Where the broker states no limit, or it cannot be read, the share is half of the stream volume's free space instead, and nothing is added to it: a reservation never spends free space, so that figure already contains what the logs hold. The other half is for everything that reserves nothing: every mailbox, every coordination bucket and the snapshot a joining node reads. |
| **Each log's ask** | Its Tier A field when you set one. Unset, the mutation log asks for a quarter of the stream volume's free space (4..64 GiB), the knowledge base's log for a quarter of that (1..16 GiB), and the vector changelog for 16 GiB capped by the same quarter. |
| **The fit** | A log whose stream already exists takes the ceiling it **holds** off the logs' half first, whatever its field says now. A ceiling you set for a log being created comes off next, and is never scaled. The unset ones being created share what is left in proportion to what each asked for, none goes below 1 GiB, and none is created above what it would get if no log existed yet — the figure every later boot reports its stream against. |

**A stream that already exists keeps its ceiling.** Sizing decides what a
missing stream is created with and nothing else: a booting node never rewrites
a running stream's configuration, and the broker never re-checks a reservation
it has already granted. A log created larger than today's sizing would make it
boots as it is, and the node logs `jetstream_stream_capacity_differs` with both
numbers — its ceiling, and what this sizing would create it with if no log
existed yet. A knowledge-base log created before it joined the budget, at a
fixed 4 GiB, is the common case, and it is harmless: its reservation was
granted when it was made. To reclaim it (or to raise any log), use
[`crewlet retention set-capacity`](retention.md#changing-a-logs-ceiling).

**And it counts at that ceiling when another log is created beside it.** A log
a new version adds, or one whose stream was deleted, is sized from what the
existing logs leave of the half, not from what they would ask for today — so a
log being created fits inside what the existing logs leave of their share,
past it only by the 1 GiB floor and a ceiling you set for it. What the existing
logs already hold is not reduced: when they hold more than the share (a ceiling
set and later unset, a log created while the volume had more room, one raised
with `crewlet retention set-capacity`), the logs reserve that much past it too,
and a log created beside them gets the floor. When a log is created below what
it would have had on an empty broker, the node says so with
`statelog_ceiling_short_of_fit`, naming each log it created short, the ceiling
it got (`created`) and the one it would have had (`fit`), and what each
existing log holds (`held`); once the node is up, `crewlet retention
set-capacity` gives an existing log's reservation back and raises the new one.
It is never created above that `fit`, even where the existing logs leave more:
every later boot reports its stream against that figure, and a log created
past it would be reported as a capacity difference on every one of them.

**A boot that still cannot reserve a log says why.** When even the floors, or
a ceiling you set, do not fit, the node refuses to boot with an error naming
the stream, the bytes it needed, the bytes the broker had left and what sets
that limit, and the Tier A field the ceiling came from:

```
engine: the broker refused to reserve the pages log's ceiling:
CREWLET_PAGES_LOG needed 1073741824 bytes and the broker had 536870912 bytes
left to reserve (… of its …-byte limit already reserved), and that limit is
stream.store_max_bytes where you set one, and otherwise three quarters of the
free space on the volume holding stream.store_dir (/var/lib/crewlet/stream),
counting what the broker's streams already hold there.
stream.pages_log_max_bytes is unset, so the ceiling was derived and scaled
into what the state logs that already exist leave of their share of the
broker, and it goes no lower than 1073741824 bytes. Give the broker more room;
the state logs that already exist keep the ceilings they were created with,
and no Tier A setting changes them: …
```

The remedies are the ones it lists. Give the broker more room: raise
`stream.store_max_bytes` where you set one, or, where you did not, free space
on that volume (a first boot needs at least 4 GiB free there, three quarters
of which is the three 1 GiB floors), which the broker measures again when the
node next starts. Or, when the refused log's ceiling is
above the 1 GiB floor, set its field to a smaller ceiling, and the refusal says
so when that applies.

A log that already exists cannot be shrunk to make room from here. Its ceiling
changes only through `crewlet retention set-capacity`, which runs on a node
whose state logs are up, and every mode starts them, `maintenance` and `seal`
included: a node refused here cannot run it.

## Reading a node's state-log lines

Every line the state log writes about its own work carries
`component=statelog`, so filtering on it narrows a node's log to its
replication:

| Line | Level | What it says |
|---|---|---|
| `statelog_apply_retrying` | `WARN` | The applier hit a failure it retries in place. Written once, when the run of failures starts. |
| `statelog_apply_faulted` | `ERROR` | The same failure has outlived the retry budget (30 seconds): this node's rows have stopped moving, its reads refuse and its seats move until a retry succeeds. Written once per run of failures, when it crosses the budget — not on every retry. While it lasts, the node's status and every refused read name the current error, and `crewlet.statelog.apply.retries` counts the attempts. |
| `statelog_apply_recovered` | `INFO` | A retry succeeded and the run of failures is over, with how long it lasted (`after`) and the last error it saw. A failure after it starts a new run, written again from `statelog_apply_retrying`. |
| `statelog_applier_stopped` | `ERROR` | The applier stopped for good — a gate this build cannot read, a hole that will not close, a recreated stream — naming the stream, the position its rows froze at and why. Every read of that domain refuses from then on, and for the tracker or the knowledge base the node also stops claiming seats and the ones it holds move to a peer. What resumes it is a build that can read what this one could not, or — for a recreated stream — [`crewlet retention reanchor`](retention.md) followed by a restart. Written once per stop. |
| `statelog_adopted` | `INFO` | The node replaced its replicated database with a peer's snapshot, naming the donor, the artefact's `sha256` (the donor's `statelog_snapshot_sent` carries the same one) and when it was taken (`taken_at`), which is how old the history it installed is. |
| `statelog_record_gated` | `WARN` | A durable record applied nowhere, naming the gate. |
| `statelog_write_gated` | `WARN` | The same, seen by the write that published it. |
| `statelog_publish_unknown` | `WARN` | A write could not tell whether its record landed. The operation id is in the line; retry under that id, never a fresh one. |
| `statelog_reanchor_started`, `statelog_reanchored` | `WARN` | A generation transition. Both name the stream the operator re-anchored (`stream`) and every stream whose cursor moved (`streams`); the completion line also gives the re-anchored stream's high-water mark before the reanchor (`prev_last_seq_seen`). |

The snapshotter, the donor and the adopter write under the same component. The
lines the engine writes *around* those loops — `statelog_stream_recreated` and
`statelog_below_the_floor` among them — carry `component=engine`, because the
component names the code that wrote a line rather than what it is about.

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
