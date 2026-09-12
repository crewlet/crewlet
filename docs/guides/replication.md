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
the applier is 16 seconds into a bulk apply and a small write's two-second wait
for its own record expires. The record is safe. A caller that treated `pending`
as a failure would double every write it made during a burst.

### `applied` is not permanent

Two gates can drop a record **after** a node has applied it, and both are
deliberate:

- **The eviction gate.** A record written by a node the fleet evicted before
  the record's own position is dropped everywhere. A node that applied it
  before learning it was evicted will drop it on replay.
- **The deletion gate.** A record about a task a purge destroyed applies
  nowhere, for ever. This is what stops a redelivery months later resurrecting
  rows an operator deliberately removed.

Neither is a bug being worked around. They are what make an eviction and a
purge mean something on a system where the log outlives the decision.

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
drain is at least 2 000 rows/s and the steady-state load of the reference
company is 0.244 rows/s — 0.012 %. What binds is not the average; it is the
burst.

One maximal bulk update is about 31 800 rows, which is **about 16 seconds of
applier occupancy on every peer**. For those 16 seconds two things degrade
fleet-wide without tripping any alarm:

- an unrelated single-object write's two-second wait expires and returns
  **`pending`** with its position and its lag — correct, and designed for;
- a barriered read answers `behind` with a computed `retry_after_seconds` of
  about 16.

Both are the system working. `crewlet retention status` publishes the longest
apply transaction and the longest batch actually observed, so this is a
measured property of your fleet rather than a surprise.

## What a rolling upgrade blocks

A record at a version this build cannot decode is retained, and everything
whose scope meets it is retained too. That is bounded rather than open-ended:
the **deferral grace** is 30 minutes, past which the alarm fires and names the
position and the scope.

So a rolling upgrade should finish inside that window. An upgrade that stalls
half-done leaves the old nodes holding records they cannot apply and refusing
reads about the objects those records touched — with `deferred` naming exactly
what to do, which is finish the upgrade.

## The five capacity ceilings

1. **The log's byte ceiling** — `stream.tracker_log_max_bytes`. A full log
   **refuses** appends rather than shedding old records; see
   [Retention](retention.md).
2. **The trim floor** — how far back the log can be replayed from, which is
   what bounds how long a node may be away.
3. **The store's own size** — every node is a full replica, so the corpus is
   held N times.
4. **Applier occupancy** — the 16 seconds above.
5. **The snapshot repository** — one artefact per node, sized in
   [Retention](retention.md).

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
