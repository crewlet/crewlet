# Retention

A Crewlet company's history has two shapes. The **durable tables** on every
node are the record of truth — every task, page, comment and turn, for ever.
The **log** in front of them is a replay window: how far back a node that has
been away can catch up from.

This page is about the second one. It grows, it has a ceiling, and something
has to delete from it — but deleting the wrong record loses history no backup
covers. That "something" is the trim, and most of this page is about why it
sometimes will not run.

## The one number to watch

```
crewlet retention status
```

Three figures per domain, and no others:

| Figure | What it is |
|---|---|
| `bytes` | what the log holds now |
| `max_bytes` | the ceiling the **broker** is enforcing |
| `headroom_fraction` | how much of the ceiling is unused |

`max_bytes` is read from the stream's own configuration and never from the
Tier A field, because Tier A is per node and takes effect at restart: between
an edit and a restart the field names a ceiling nothing is applying, and this
is the number you divide by.

There is deliberately **no growth rate and no projected-full date.** A
24-hour rate false-pages on the one excursion this system is designed for — a
full re-embedding, which is a step rather than a trend — and on the mutation
log a blocked term is the only path to the ceiling at all, and `blocked_by`
names it years ahead of it.

**`crewlet retention status` exits non-zero when any alarm is active** and
prints each one's measurement and remedy on stderr. That is the alarm hook:
run it from your own cron and page on the exit code. The engine deliberately
does not invent a notification channel it has no business owning. The full
alarm list is in [Alarms](../reference/alarms.md).

## The six terms

The trim may delete up to the **minimum** of six numbers. Every one of them
can only move the point **down**, which is the whole grammar: each is a floor,
never a horizon.

| Term | Permits deleting up to |
|---|---|
| `applied` | the lowest committed position over the **counted set** of nodes |
| `min_hold` | the lowest live pin — a backup or a joining node copying the log |
| `backup_floor` | what has left the host |
| `snapshot_floor` | the *k*-th highest verified snapshot position over the counted set |
| `feed_ack_floor` | how far the wake feed has scanned |
| `age_floor` | the newest sequence older than `min_age` |

A term that **could not be read** blocks, exactly as one that permits nothing
does. A term nobody could read is not a term that is satisfied, and treating it
as satisfied is how a trim advances past a node that could not report. A term
this domain does not **have** — a compacted domain has no wake feed — is
`n/a` rather than zero, which is a different thing again.

### `min_age` is a floor on trimming, and therefore a *lower bound* on retention

Raising `min_age` can only move the trim point **down**. It cannot shorten how
long the log keeps a record and it says nothing whatever about any node's own
database file. Three readers of this design got that backwards, so it is stated
at the arithmetic and again here.

Default 7 days, range 24 h to 90 d. The 24-hour floor has three reasons: a
value below a day cannot outlast a nightly backup cycle; it is the margin that
keeps a quiet object writable, because an object whose last record has been
trimmed has to be recognised as *trimmed* rather than as *absent*; and it
bounds the window a joining node's vector gap is refilled from.

### A company that never backs up never trims

This is the term that surprises people, and it is the configuration working as
asked.

The log is the only copy of what no node has applied yet. Trimming past the
newest backup is deleting the last thing that could rebuild it. So
`backup_floor` refuses until a backup exists and is younger than
`backup_max_age` (default 24 h).

`crewlet retention status` says so in prose, first, before any table:

```
Nothing is being trimmed on tracker: the newest complete backup is 3 days old
(backup_max_age is 24h).
```

Two policies decide which copies count:

- **`backup_floor: engine`** (default) — the copies this fleet's own nodes
  wrote and verified. `crewlet backup` announces each one to the fleet when its
  manifest is written.
- **`backup_floor: operator`** — an explicit acknowledgement, for a company
  whose policy is "trim only what is off-site". The engine cannot see that a
  copy left the host, so a person says so:

  ```
  crewlet retention ack -stream CREWLET_TRACKER_LOG -position 918100000
  ```

  Under this policy the trim does not advance until that acknowledgement has
  been given at least once.

An **unverified** copy satisfies neither. A file that exists and was never
opened is not a backup, and deleting the log's only copy of a record against
one is what this whole term prevents.

### `retention.backup_owner`

Set it.

```yaml
retention:
  backup_owner: platform-oncall
```

Free text — a person, a team, a scheduler's name. The engine does not run your
backups and cannot; `crewlet backup` is a node-client command because the
coordination broker binds no socket. Ownership is still a person. What this
field changes is that the fleet can name which one, in four places: `crewlet
validate` warns when it is unset under `backup_floor: engine`, `crewlet
retention status` names it in the backup term's remedy, the retention answer
renders it, and **`crewlet backup`'s manifest records it** — which is where it
stops being configuration and becomes durable evidence.

## The cross-field rule

`snapshot_interval × (SnapshotsKept + 1)` must be **less than** `min_age`, or
every snapshot ages out of the replay window before its successor exists — a
node adopting one would land below a floor the log can no longer bridge.
`crewlet validate` checks it.

With the shipped defaults that is 24 h × 2 = 48 h against 7 days. Comfortable.

## Snapshots

A node below the trim floor cannot replay its way back: the records it is
missing are gone. What it does instead is fetch a peer's snapshot of the
replicated estate, verify it, and adopt it wholesale.

```
crewlet retention snapshots
```

Its own verb rather than a block of `status`, because the repository is **per
node**: "which of my machines can donate, and how old is what they hold" is a
disk question, and it is the one you ask when a join has failed.

**`SnapshotsKept = 1`.** With N donors the fleet is the redundancy — one per
node across at least two nodes — and a recipient that fails a verification asks
the next donor.

**On a single node the loop does not run at all.** It skips with the published
reason `sole_node`, because a full copy every day buys an artefact no peer can
fetch. A solo deployment's recovery artefact is `crewlet backup`.

Every skip is published on the node's own register row, so a failed join has an
answer rather than a silence:

| Reason | What it means |
|---|---|
| `sole_node` | fewer than two counted nodes; nothing to donate to |
| `lagging` | this node is more than 1 000 records behind |
| `unhydrated` | this node has not established a complete copy of some domain |
| `deferred` | this node holds a record it cannot decode |
| `insufficient_space` | not enough disk in `store.snapshot_dir` |
| `recent` | the newest artefact is younger than `snapshot_interval` |

### Sizing the snapshot volume

**×4.2 of the replicated store**, on a separate volume. Three states: steady is
live + WAL + its own snapshot + one backup copy = ×3.2; **rotation** is that
plus the new partial file before the old one is deleted = ×4.2; adopting is
×3.2.

`rotates_out_at` on the report is three-valued — a time, "never" for a fleet
that is not rotating, and unknown where the interval cannot be established —
because a fleet whose snapshot loop is skipping has no next rotation to name.

## The join runbook

When a node reports `below_floor`:

1. **`crewlet retention snapshots`** — does any peer hold one, and how old?
2. If one does, the node fetches it on its own at boot. It takes a **hold** on
   every domain's log first, which pins the trim for the duration of the
   transfer — so a join cannot race the trim that made it necessary.
3. If none does, the skip reason says why. Fix that first: a fleet where every
   node is `lagging` has an applier problem, not a snapshot problem.
4. If the fleet genuinely holds none — a single node, or every peer skipping —
   the path is a restore from `crewlet backup`, and the log has to still reach
   the artefact's position. `crewlet retention verify --restore` is what tells
   you in advance that it does.

**`rejoin_window`** (default 30 m) is your budget for a node to become a
complete replica. `crewlet retention status` prints this node's store size, the
projected join against a conservative profile, and the window — so a fleet
whose store has outgrown its window says so before an incident does.

## Eviction

An absent node pins the `applied` term for ever: its position never advances,
so nothing above it can be deleted. That is deliberate — a node that is coming
back needs those records — and eviction is the operator's gesture for a node
that is not.

```
crewlet retention evict node-4 -confirm node-4
```

It prints the watermark before and after, and the instant the eviction takes
effect. **The evicted node stays counted for about a minute**, so a live node
is certain to have read its own tombstone before the trim passes it.

The honest worst case for that window is **zero**: a node three heartbeats late
reads its tombstone exactly when the trim may pass it. So the window is a
convenience rather than the safety property. The safety property is the
applier's own **eviction gate**, which drops an evicted node's records wherever
they land, depends on nothing but the log's own order, and holds when
coordination cannot be reached at all.

Readmission is the inverse commit rather than a delete, so the whole history
survives a replay:

```
crewlet retention readmit node-4 -confirm node-4
```

It can be refused, and the refusal names the reason: a node whose own position
is below the current trim floor cannot simply resume — it has to adopt a
snapshot first, which is why the readmission prints its position beside the
floor.

## A full log refuses; it does not shed

At the ceiling the log **refuses appends**, naming `log_full`, the field, and
the retention block. It does not silently delete old records to make room:
shedding a record no node has applied is exactly the loss the whole gate
exists to prevent.

A full log costs `linearizable` reads, because those append a barrier. `stale`
and `session` keep answering. See [Read consistency](consistency.md).

The refusal carries **no retry hint**, deliberately: the only thing that frees
a byte is a fifteen-minute gated job, and the log is full precisely because
that gate is closed. A number here would be a promise the mechanism does not
make.

## Changing a log's ceiling

`stream.max_bytes` is not a live setting. Raising it is a **fleet-wide
maintenance window**, and the reason is not caution:

- A resize is decided against the usage the log is at, and a publisher makes
  that a moving quantity.
- What retires a configuration request the broker has already queued is the
  **broker process restarting**, not a client closing its connection. So an
  apply whose outcome is unknown can only be resolved by everything restarting.

The window costs **three fleet-wide restarts** on the happy path, and two more
per retry:

```
# 1. every node
crewlet run -mode maintenance

# 2. from any one of them
crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592

# 3. every node
crewlet run -mode seal

# 4. from any one of them
crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592

# 5. every node, back to service
crewlet run
```

### The three modes

| `-mode` | Starts publishers | May write stream configuration |
|---|---|---|
| *(unset)* — `normal` | yes | no |
| `maintenance` | no | **yes** |
| `seal` | no | **no** |

`seal` exists precisely because it *cannot* write configuration. A node
acknowledging from it is evidence that the process making the claim is not the
one holding the request being retired — which is what a maintenance-mode
acknowledgement could never establish about itself.

### What excludes a publisher

On the default embedded topology, **nothing outside these processes can reach
the broker at all** — it binds no socket — so a fleet in a maintenance mode is
structurally excluded. There is no check to pass.

On `stream.type: nats`, the engine does not run the broker and cannot know who
else holds a connection to it. `set-capacity` refuses there unless you pass
`-i-have-excluded-all-publishers`, which is your assertion in your own words. A
check that quietly proved nothing would be worse than the refusal.

A node that boots into `normal` mode writes an **admission** before it starts
anything, then re-reads the operation and withdraws if one is open. A
coordinator refuses to take the exclusion while any admission is held. Either
order is then safe: a node that slipped between a check and its own start has
left a durable record, where a silence would have let the coordinator proceed.

### Where it stands, and what is holding it

```
crewlet retention maintenance status -stream CREWLET_TRACKER_LOG
```

One page: the operation and its phase and attempt, every participant's
baselined incarnation against what it acknowledged as, which acknowledgements
are missing, any unresolved write attempt, and any admission still blocking
activation.

Two gestures act on what it shows:

| | |
|---|---|
| `crewlet retention maintenance exclude -stream NAME -node ID -confirm ID` | Your assertion that a participant's **process is stopped** and holds no outstanding request. The only thing that waives an acknowledgement — an eviction does not, because that is about whose records apply and this is about whose process is running. |
| `crewlet retention maintenance abandon -stream NAME -confirm OPERATION-ID` | Changes what the operation is trying to reach; **never** the barrier it must cross. From `opened` it clears outright, because no request was ever issued. From anywhere else it still enters the seal: a paused coordinator's request is outstanding whether or not a person has read a status page. |

### Why the target cannot be changed mid-window

A verify compares the ceiling the broker reports against the operation's
target. If the target could move, a mismatch would be unreadable — nobody could
tell an unapplied request from a changed mind. Repeat the number in `-confirm`
and it is fixed for the life of the operation; to choose a different one,
abandon and open a new one.

A mismatch at verify is a **numbered attempt**, not a re-apply: every node is
in `seal` mode, which refuses configuration writes, so there is nowhere legal
for a re-apply to run. Three attempts, then the operation reports blocked and
waits for you.

## Re-anchoring a recreated stream

If a stream is genuinely recreated — deleted and remade, restored from a
broker-level backup, rebuilt by hand — its sequences restart at 1. Every
position the fleet holds then names a number space that no longer exists: a
stored version, a consumer cursor, an arbitration anchor. Nodes refuse to
serve, which is correct.

The engine detects this from the stream's own **creation instant**, and
`reanchor` is the response:

```
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
# prints the live stream's created_at, and refuses

crewlet retention reanchor -stream CREWLET_TRACKER_LOG -confirm 2031-04-02T03:00:00Z
```

The confirmation is the stream's own `created_at`, and the verb **prints it and
refuses** rather than reading it and feeding it straight back — otherwise it
would be confirming against its own output.

What a reanchor says is: *these rows are what they are; follow the new stream
from its head.* The durable tables are the record of truth and the stream is a
replay window, so the rows survive and the window is replaced. The generation
is what makes an old position **comparable and safely stale** rather than
indistinguishable from a current one — a stored version below it forms
`expect = 0` on its next write, and a client cursor below it is refused by name.

It does **not** recover records that were on the old stream and were never
applied here.

It refuses while any peer is hydrated on the live stream, naming the peer:
adopting that peer's snapshot recovers the history a reanchor discards, so it
is strictly the better recovery. `-force` is for the case where the peer cannot
be reached.

## Proving a restore

```
crewlet retention verify --restore -dir /var/backups/crewlet
```

The one verb here that talks to **no node**. It restores the newest artefact
under `-dir` and opens the copy: the point is to establish that the artefact
alone is enough, and running it through a running engine would be asking the
thing under test to test itself. It writes nothing to the live store and takes
no lock on it.

It prints what the artefact holds — when it was taken and by which node, each
estate's size and migration count, and every domain's generation and sequence —
and then **exits non-zero past its cadence**, which defaults to 30 days
(`-cadence`). That default is derived rather than chosen: `min_age` is 7 days,
so a restore path broken for longer than one replay window means the log can no
longer bridge the gap between an artefact and the present. Monthly gives four
of those windows of margin.

Two refusals are worth knowing:

- **A directory with no manifest is debris**, not a partial backup — the
  manifest is written last, so its absence means the run did not finish.
- **An artefact naming no domain position cannot be verified**, whatever else
  it contains: a restore replays from that sequence, and there is none.

Put it in cron. A lapsed restore test that exits zero is a paragraph in a
runbook nobody read.

## Removal, deletion and what a purge does not reach

Three different things:

- **Removing** a task hides it. The rows stay; a restore brings it back.
- **Deleting** writes a marker. Every node drops every record about that task,
  for ever — which is what stops a redelivery months later resurrecting it.
- **Purging** removes the rows. Its report has **three groups**: what was
  purged, what could not be reached, and what is stale.

The report deliberately gives **no time guarantee**. An offline or evicted
disk retains its copy until replay, adoption, replacement or destruction, and
there is no duration to state. Saying "within 24 hours" would be a promise the
architecture cannot keep.

## Old data is not cold data

There is no tiering here, and the absence is a decision rather than an
omission.

Every node holds the whole corpus in one SQL database. A task filed three years
ago is read at the same cost as one filed this morning, is searched by the same
index, and is replicated exactly as many times. The log in front of it is
trimmed; the tables are not.

That is what makes the storage forecast a function of how much a company has
ever done rather than of how much it is doing — and it is why the numbers below
are worth reading before the fleet is large.

The one exception is each domain's **operation ledger** — the table that
answers "did the operation I published land here?" — which is swept per node at
**30 days**. The horizon comes from the client that actually re-asks: a machine
retry lives inside a two-second wait, but a seat carries an operation id
forward and re-asks on its next wake, hours or a weekend later. An operation id
older than that resolves `unknown` rather than `applied`, which is the honest
answer once the row is gone.

## The storage forecast

For the reference company (100 000 tasks a year, 300 000 comments, 1 000 edits
a day, 3 000 turns a day, 50 projects):

| | Year 1 | Year 3 | Year 5 |
|---|---|---|---|
| replicated store, per node | 9.7 GB | 26.6 GB | 43.8 GB |
| snapshot volume at ×4.2 | 40.8 GB | 111.8 GB | 183.9 GB |

The log itself, on a healthily-trimming fleet, holds **about 156 MB** — the
`min_age` window's worth of records, not a year's. `blocked_by` is the field
that says whether this fleet is one that trims; if it is set, the log is
growing without bound and the forecast above does not apply to it.

The read barriers add about **639 MB a year** to the log's throughput, which is
inside the ceiling by a factor of five at year five. That figure is a term in
the log's own size, so it is printed with its derivation rather than assumed:
it comes from an assumed 12 500 linearizable reads a day.

Two excursions are designed for and do not alarm:

1. **A full re-embedding.** A width change republishes every vector, which at
   year five bottoms the vector log's headroom at about 51 %. The alarm
   threshold is 10 %, which is the first decile clear of it.
2. **A bulk gesture.** One maximal bulk update is 16 seconds of applier
   occupancy on every peer; see [Replication](replication.md).

## The recovery-operations profile

What a production fleet should actually be set to:

| | Value | Why |
|---|---|---|
| replicas | **3 nodes**, `stream.replicas: 3`, `stream.sync: always`, distinct hosts in independent power domains | Two gives no quorum. Three is the minimum at which two donors can be satisfied while one node has lost its store. |
| disks | store dir and JetStream store dir on **local NVMe** | Anchored to a published p99 fsync budget of under 10 ms, against 1–3 ms measured on NVMe and 15–40 ms on network volumes. |
| backups | **every 6 hours**, retained **14 days** | 6 h is `backup_max_age ÷ 4`, so three consecutive failures pass before the trim blocks. 14 d is 2 × `min_age`, so a restore is always into a window the log can still cover. |
| restore test | **monthly**, `crewlet retention verify --restore` | Justified against `min_age = 7d`: a restore path broken for longer than one replay window means the log can no longer bridge the gap. Monthly gives four windows of margin. |
| snapshot volume | **×4.2** of the store, on its own volume | The rotation state above. |
| rejoin window | **30 m** | Against a conservative projection of 5.1 / 12.8 / 20.7 minutes at years 1 / 3 / 5. |

**Full replay with no snapshot is 5.2 hours at year five** — 10 to 16 hours
accounting for index depth. That is 15 to 46× the snapshot path, which is what
makes the snapshot tier a requirement rather than an optimisation.

## Measured, derived, projected

The numbers on this page are not all the same kind of number, and it matters
which:

- **MEASURED** — the apply drain, the barrier's latency, the publish-to-
  deliverable time, the quantisation recall, the fsync rates. These came off a
  benchmark.
- **DERIVED** — the join projections, the storage forecast, the barrier's
  annual bytes. These are arithmetic over measured inputs and a stated corpus.
  Change the corpus and they change.
- **PROJECTED** — the supported search corpus at your concurrency, and the
  under-load search capacity. These need a benchmark on your hardware, and
  until it runs they are estimates with a stated basis rather than facts.

## See also

- **[Replication](replication.md)** — the two regimes, the write outcomes and
  what the design does not promise.
- **[Read consistency](consistency.md)** — what a full log costs, and the
  twelve refusals.
- **[Backups & restore](backup.md)** — the artefact and the runbook.
- **[CLI reference](../reference/cli.md#crewlet-retention)** — every verb's
  flags and refusals.
- **[Alarms](../reference/alarms.md)** — every condition and its remedy.
- **[Metrics](../reference/metrics.md)** — the instruments a dashboard is built
  from.
