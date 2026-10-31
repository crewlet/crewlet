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

**Every `crewlet retention` command that talks to a node needs
`admin`** — all of them but `verify --restore`, which opens a snapshot
directory on disk — reads included:
the `retention` question and every `/work/retention*` route take it, because a
map of which machine holds what and the value a reanchor must echo describe the
deployment rather than the company. The commands authenticate with
`CREWLET_API_TOKEN`, and a credential without the grant is refused `403`
naming it.

Five figures per domain, and no others:

| Figure | What it is |
|---|---|
| `bytes` | what the log holds now |
| `max_bytes` | the ceiling the **broker** is enforcing |
| `reserve_bytes` | on every log that claims identity — the tracker's, the knowledge base's, the identity estate's, the artifacts log's, the artifact datastore's and the code host's — the top of that ceiling kept for gate records; see [the gate reserve](#the-gate-reserve) |
| `headroom_fraction` | how much of the ceiling **ordinary writes** are held to is unused: `max_bytes` less `reserve_bytes` |
| `bytes_per_day` | what the log took in over the trailing day (`PER DAY` in the table) |

`max_bytes` is read from the stream's own configuration and never from the
Tier A field, because Tier A is per node and is only the value a stream is
created with: once the stream exists, an edit to the field names a ceiling
nothing is applying, restart or not, and this is the number you divide by.

### The one rate, and what it is held against

The trim never removes a record younger than `min_age`, so a log's ceiling has
to hold `min_age` of its own intake. A log whose ceiling is smaller fills and
refuses appends **with every trim term satisfied** — nothing is blocked, so
`blocked_by` says nothing, and unblocking a term could not help anyway. That is
the one path to a full log the six terms cannot name, and `log_ceiling_short`
is the alarm for it: each node measures every log's intake at boot and on
every trim tick, compares each log's `max_bytes` against `min_age` ×
`bytes_per_day` on every alarm heartbeat (ten seconds), and fires when the
window no longer fits. Its detail says how long the ceiling holds at that rate;
the remedy is a bigger ceiling (see [Changing a log's
ceiling](#changing-a-logs-ceiling)) or a shorter `min_age`.

It matters most on the **identity log**. The mutation log and the knowledge
base grow with a corpus you can forecast; the identity log grows with how often people sign in, which is the rate an
operator cannot predict — and its ceiling is not derived from the disk.

The rate is **measured from the log itself**, not sampled: every record of the
last day is still in the log (`min_age` is at least a day), each carries the
broker's own stored instant, so the day's intake is the records at or after
now − 24 h — found by the same binary search the age term runs — at the log's
own average record size. Every node reading the same stream derives the same
number, and a node that has just restarted measures on its first tick.

It is deliberately absent — and the alarm silent — in three places, and absent
is not zero:

- **A compacted log**, which is the vector log. Its size follows how many
  subjects it holds rather than how long it has been written, and a full
  re-embedding — the one excursion this system is designed for — republishes
  every vector in a day while replacing what the log held rather than adding
  to it. Holding its ceiling against a week of that day would page for a log
  that is not growing.
- **A log in its first two days** — a fresh deployment, or one re-anchored
  under a running fleet. Its first day is the one a company imports into, and
  a day extrapolated from an import is an alarm for a log that settles an
  order of magnitude lower — so the day measured has to lie wholly after the
  first one, which a log merely a day old does not give: the day behind it is
  its first.
- **A node whose trim has not ticked yet.**

A measured `0` is a log that took in nothing yesterday, which holds any window.

There is still **no projected-full date.** The comparison is against the
window the operator set, not a date extrapolated from a trend. One day busy
enough that a window of such days would not fit — a login storm on the
identity log — raises the alarm while it is inside the trailing day, because
its first day is indistinguishable from a new steady state; if it was a step,
it clears the day after.

**`crewlet retention status` exits non-zero when any alarm is active** and
prints each one's measurement and remedy on stderr. That is the alarm hook:
run it from your own cron and page on the exit code. The engine deliberately
does not invent a notification channel it has no business owning. The full
alarm list is in [Alarms](../reference/alarms.md).

**An alarm is one per kind, however many logs it stands on.** A condition
that is a property of a log (`log_headroom`, `log_ceiling_short`,
`trim_blocked`, `deferred_old`, `floor_unknown`, `census_drift`) lists every
log it stands on in its `domains`, each with that log's own measurement, so
four held logs are one `trim_blocked` on the health count, the gauge and the
dashboard rather than four; `crewlet retention status` prints them under the
alarm, one log in its headline and several as a line each. The `alarm_raised`
and `alarm_cleared` log lines are still written per log, carrying it as
`domain`, so a second log joining an alarm that is already up, and the first
one leaving it while the second stands, are each said with their own start.

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
| `feed_ack_floor` | how far **that log's own** wake feed has acknowledged |
| `age_floor` | the newest sequence older than `min_age` |

The **counted set** of a log is every node the trim waits for there: each node
that has reported a position on the log, and each **live data node** even
before its first report, when it counts at position zero because it is about
to replay what the trim would otherwise delete. The live data nodes are listed
afresh from the presence leases on every tick rather than read from a watched
view, because a view up to a heartbeat old may miss a node that has just
booted — the one node the trim must not pass. A node evicted from the log stops
being counted about a minute after that record lands.

**The estate's logs are trimmed by one node**: the holder of the trim duty,
`worker:retention`. Only a data node may claim it, since the trim reads the
estate it holds — and not one whose copy of the tracker, the knowledge base or
the code log is out of service, since the evictions and the arbitration anchors
it counts are in those rows: such a node
declines the duty (`retention_duty_declined`), gives its lease back if it holds
it (`retention_duty_given_back`), and one whose copies serve takes it at its
next tick ([ADR-0049](https://github.com/crewlet/crewlet/blob/main/adr/0049-a-domains-fault-takes-only-its-own-copy-out-of-service.md)).
Left to lapse, the duty's 45-minute lease would leave every log untrimmed for
up to an hour.
Every node still evaluates its own alarms on every tick, whatever duty it holds.

`feed_ack_floor` is the acknowledgement floor of the durable consumer that each
log's own change feed opens — the feed's **group** `crewlet-tracker-feed` on
`CREWLET_TRACKER_LOG`, and its group `crewlet-pages-feed` on
`CREWLET_PAGES_LOG` — because a record that feed has not acknowledged is one
nobody has been woken for yet. The code log is trimmed by the same six terms
and declares no wake feed, so its feed term is absent rather than zero: no
wake is derived from a code record in this release. So a feed that has stopped holds back its own
log and no other, and `blocked_by: feed_ack_floor` on a domain names that
domain's feed.

A group name is not the broker's consumer name. The durable consumer is named
from the group **and** the stream, as `<group>__<stream>__<digest>` — for
example `crewlet-pages-feed__CREWLET_PAGES_LOG__e5825de86642` — so
`nats consumer ls <stream>` is what finds it, on an external cluster or
through an embedded fleet's member with the
[operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool), and a lookup by
the group name alone finds nothing.

A term that **could not be read** blocks, exactly as one that permits nothing
does. A term nobody could read is not a term that is satisfied, and treating it
as satisfied is how a trim advances past a node that could not report. A term
this domain does not **have** — the vector log, the code log, the identity
estate's and the usage log's have no wake feed — is `n/a`
rather than zero, which is a different thing again. And a term that was
read and **binds nothing** — no hold is pinning the log, or a solo fleet takes
no snapshots — reads `unbounded` rather than carrying a sequence. Inside the
engine its value is the largest there is, because it is the identity for the
minimum the trim takes across the six; that is a number chosen to lose a
comparison, not a position, so the screen and the API say the word instead.

### A blocked trim is ordinary until it has kept something

Every fresh deployment's trim is blocked — on its first backup, on its first
snapshot donors, and for as long as a log nobody writes to stays empty, since
every node sits at position zero on it. `crewlet retention status` shows the
block and the term holding it from the first tick, because that is what the
screen is for. The node holding the trim says it in its log once rather than
every tick, both at `WARN` so a filter that shows one shows the other:
`retention_trim_blocked` when a block starts or the term holding it changes,
and `retention_trim_unblocked` when the trim advances again, carrying how long
it was blocked. The **`trim_blocked` alarm** is quieter still, and fires only
when both of these hold:

- the trim has been blocked for longer than `min_age` **plus one trim tick**
  (fifteen minutes). `min_age` is the number the configuration already holds
  every sanctioned block under: a joining node inside its rejoin window (whose
  24-hour ceiling is `min_age`'s floor), a young fleet waiting for its donors
  (the [cross-field rule](#the-cross-field-rule) keeps the snapshot cadence
  below it), a first backup (which `backup_age` raises on its own — at once
  where none has been taken, at the policy's age where one has gone stale). The
  tick is the block's resolution: `blocked_since` moves only when the trim runs,
  so a block that ended is seen to end up to a tick later;
- **and** the log holds records older than `min_age` — the ones the age term
  would release and the blocked trim is keeping. That is read off the log
  itself, the published age term's sequence against the log's first, so a young
  log and an empty one stay silent however long the block has lasted.

Its detail names the log, the blocking term and up to how many records it is
keeping — exactly that many on a strict log, at most that many on the vectors'
compacted one, whose superseded records are already gone; the term's row in
`crewlet retention status` says what it has and what it wants.

### `min_age` is a floor on trimming, and therefore a *lower bound* on retention

Raising `min_age` can only move the trim point **down**. It cannot shorten how
long the log keeps a record and it says nothing whatever about any node's own
database file. Three readers of this design got that backwards, so it is stated
at the arithmetic and again here.

Default 7 days, range 24 h to 90 d. The 24-hour floor has three reasons: a
value below a day cannot outlast a nightly backup cycle; it is the margin that
keeps a quiet object writable, because an object whose last record has been
trimmed has to be recognised as *trimmed* rather than as *absent* — and its
arbitration anchor is then removed, see
[Anchors below the log are removed by a record](#anchors-below-the-log-are-removed-by-a-record);
and it bounds the window a joining node's vector gap is refilled from.

### A company that never backs up never trims

This is the term that surprises people, and it is the configuration working as
asked.

The log is the only copy of what no node has applied yet. Trimming past the
newest backup is deleting the last thing that could rebuild it. So
`backup_floor` refuses until a backup exists and is younger than
`backup_max_age` (default 24 h).

`crewlet retention status` says so in prose, first, before any table:

```
Nothing is being trimmed on tracker: the newest backup is 72h0m0s old, past the 24h0m0s this deployment accepts.
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
  been given at least once, and `crewlet validate` warns about it on every
  node that holds data — the nodes that run the trim — as each of those nodes
  does on every start, in a `config_advisory` line. A node without `data`
  runs no trim and reads none of `stream.tracker_retention`, so it is not
  warned.

The **backup-age alarm** (`backup_age`) ages the same point the trim reads,
under the same policy: under `operator` a node's nightly copies are fresh
while the trim waits on an acknowledgement, and the alarm fires on the missing
acknowledgement rather than staying green beside a log that cannot be trimmed.
It names every log whose trim the backup term has held for longer than that
log's `min_age` replay window plus a trim tick while the log keeps records older
than the window — exactly the logs that would otherwise raise `trim_blocked`
([above](#a-blocked-trim-is-ordinary-until-it-has-kept-something)) — and those
logs raise no `trim_blocked` of their own: the backup is the one thing to do
about all of them. A log the backup term has held for less than that, or that
keeps nothing past the window, is listed under no alarm; `crewlet retention
status` still shows its block from the first tick. **Settings › Backups &
retention** marks that point **counted · newest**.

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
validate` warns whenever it is unset (under either `backup_floor`), and so does
every start of every node, in a `config_advisory` line; `crewlet retention
status` names it in the backup term's remedy; the retention answer renders it;
and **`crewlet backup`'s manifest records it** — which is where it stops being
configuration and becomes durable evidence.

## The trim floor

`crewlet retention status` prints a **trim floor** per domain. Everything below
it may already have been deleted from the log, so a node that has applied
every record up to the one just before it holds everything that may be
missing. A node further behind replays what the log still holds, and adopts a
snapshot only when what it lacks is gone from the log itself. Two properties
make the floor something the fleet can rely on:

- **It is published before the purge it licenses.** The trim writes its
  conclusion to the fleet first and deletes second. A tick whose floor could
  not be published deletes nothing. A tick that published and then failed to
  delete leaves the floor ahead of the log until a later tick licenses
  deleting at least that far — and a tick that is blocked, or whose lowest
  counted node is lower, deletes less or nothing, so that can be a while. It
  is the safe direction: the records between the log's first record and the
  floor are still there, so a node below the floor and above the log reports
  `behind` while it replays them and needs no snapshot.
- **It never moves down within a generation.** A tick that is blocked, or whose
  lowest counted node is lower than last time — a readmitted node, one
  restored from an old backup, one that came up on its own history because no
  peer could donate — leaves the floor where it was. Records an earlier tick
  deleted do not come back because a later tick concluded less.

What the last tick itself concluded is `trim_to` on the retention answer:
zero while the trim is blocked, and below the floor whenever the lowest counted
node is. The dashboard's retention screen shows it beside the floor when the
two differ.

**A floor belongs to one generation of its log.** Right after a
[reanchor](#re-anchoring-a-recreated-or-restored-log) the published floor, its terms and
its blocking term describe the stream that was left, so the status shows that
domain with **no conclusion yet** until the trim's first tick on the adopted
stream — exactly as the write fence and readiness read it. `crewlet retention
status` prints `-` for its `TRIM FLOOR` and says, under its watermarks, that
the trim has concluded nothing about that generation yet; the dashboard tags it
*no conclusion yet* rather than *advancing*; and the retention answer carries
`trim_floor_state: none_at_generation` with an empty `terms` list. A domain
whose floor nobody could read is shown as that (`unreadable`), never as an
empty one, and the other domains' rows are unaffected — see
[An unreadable trim floor](#an-unreadable-trim-floor).

The floor is load-bearing for **writes**, not only for recovery. A write to an
object whose last record has been deleted from the log cannot compare against
that record any more, so it is retried as "this object's history on the log is
empty" — which is only true on a node that has applied everything that may be
gone. So every such write first checks, freshly, that the rows it decided
from — as they stood when it decided, not wherever the node has got to since —
hold every record up to the one just before the **higher of the floor and the
log's own first record**, and a write that does not is refused `unavailable`
rather than allowed to overwrite a change it never saw. Freshly means after
the write has read its object on the log, and never from a floor the node
remembers: one read before it can miss the floor that licensed deleting the
object's last record. That read is the write's own domain's floor alone, by
its key — one request to the coordination store's leader — never a listing
of every domain's floor, and it is made at the same time as the read of the
log's ends, so a write waits for the slower of the two rather than both. The
refusal names its reason, and makes the same split reads do:

- `behind` when the log still holds every record the node lacks — it is
  replaying up to the floor, or it has already applied past the state the
  write decided from. It clears on its own; retry on the same node.
- `below_floor` when the node's next record is gone from the log itself. No
  replay can supply it, so the node [adopts a snapshot](#the-join-runbook);
  another node can make the write meanwhile.
- `floor_unknown` when this domain's floor or the log could not be read. An
  unknown bound refuses rather than guessing, and only for the domain whose
  bound it is.
- `evicted` for a node the fleet has removed.
- `wrong_stream` when the node's own checkpoint is past the log's end — judged
  on where the node's applier stands rather than on the state the write
  decided from, because whatever it appends lands relative to the former — or
  when the log holds, at that checkpoint, another record than the one the node
  consumed there. See
  [Re-anchoring a recreated or restored log](#re-anchoring-a-recreated-or-restored-log).
- `log_truncated` when a **peer's** rows hold records the log lost — a broker
  restored from an older copy, with a peer whose rows are newer. This node's
  own rows are the log's history, so only its writes refuse, until the
  operator decides which history the fleet keeps; an eviction is still
  written. See the same section.

None of them is a conflict: a conflict tells a caller somebody else is editing
and to re-read, and every one of these is about this node rather than the
object. Reads make the same comparison: a node below the floor whose missing
records the log still holds refuses them as `behind` while it replays them,
and a node whose missing records are gone from the log itself refuses them as
`below_floor` and adopts a snapshot.

### An unreadable trim floor

Each domain's floor is a key of its own, `floor.<domain>` in the
`crewlet_statelog_positions` bucket, and every reader of it asks that key
alone. So a floor that cannot be read stops **that domain** and nothing else:
its reads refuse `floor_unknown` once the floor has gone unread for four
heartbeats (a minute), its writes at an expectation of zero refuse at once,
its trim neither publishes nor purges, and the retention answer shows its floor
`unreadable` — while every other domain reads, writes and trims as before. The
`floor_unknown` alarm names each domain whose reads it refuses — those whose
floor has gone unread past the four heartbeats — with how long that domain's
floor has gone unread; a domain whose floor began failing more recently is not
named yet, since its reads are still served. That age is this node's own
observation, taken on its ten-second alarm heartbeat by the **floor watch**
through the read path's own two steps — each domain's floor read by its key,
then held to the generation its applier is on — from the first beat that
failed to the latest, so the alarm names exactly the floors this node's reads
refuse on, whichever node holds the trim, and forgets the age the moment a
read succeeds. A floor published at a generation this node has not reached is
as unusable as one that cannot be read, and the alarm says which it was.

There are two ways a floor cannot be read, and they read differently:

- **Coordination does not answer.** Every domain's floor is unreadable at
  once, because every key is in the one bucket. Nothing is wrong with the
  rows; the refusals clear by themselves when the store answers again. Check
  coordination — the node's connection to the broker, and the bucket's
  stream having a leader.
- **The row does not read as its floor.** Its bytes do not decode, or it is
  filed under one domain's key and names another. Only that domain refuses,
  and the refusal — on the trim's `retention_trim_failed` line and on every
  write it refuses — names the key and the bucket, and says it clears once the
  key is removed rather than when coordination answers. Nothing in the engine
  rewrites such a row: the trim carries each floor forward from the row it
  reads, so it never publishes over one it cannot read. The remedy is to
  remove the key, after which the next trim tick publishes the floor afresh
  from what every node has applied. On an external NATS cluster
  (`stream.type: nats`) that is
  `nats kv purge crewlet_statelog_positions floor.<domain>`, and so it is on
  an embedded fleet, through a clustered member's loopback client listener
  with the [operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool). A broker that opens no
  client listener — a solo member's, or one serving only leaves — listens on
  no socket an outside tool can reach, and no `crewlet` command removes the
  key. Neither shape is one the engine
  writes — every floor it publishes is validated and filed under its own
  domain's key — so a row like this was written by something other than the
  engine, and it stays until a client of the bucket removes it.

### Anchors below the log are removed by a record

Every node keeps, per object, the position of the last record it consumed on
that object's subject, whatever that record then did — the **arbitration
anchor** a new write's expectation is formed from
([replication](replication.md)). Once the log no longer holds an
object's records, its anchor names a record that is gone: all it can do is send
the object's next write to the broker to be refused before the write asks the
log what the object holds. Kept, the anchors would grow by every object the log
ever carried, on every data node, for the life of the deployment.

So after each tick's purges, on each log that writes anchors — the tracker's,
the knowledge base's and the code log — the trim holder counts the anchors its
own copy holds below the log's **first surviving record** and appends one
`trimmed` record per 250 of them, at most 128 a tick. Every node's applier removes up to 250
anchors per record, lowest first, in the transaction that applies it — the
same rows on every node, because it is a record on the log rather than a sweep
each node runs on its own schedule
([ADR-0050](https://github.com/crewlet/crewlet/blob/main/adr/0050-an-anchor-below-the-trim-floor-is-removed-by-a-record.md)).

- **A tick with nothing below appends nothing**, so an idle fleet's log gains no
  record. A tick whose purge was blocked still counts below the log's own first
  record, so a backlog the cap of 128 or the time budget left — or one whose
  sweep failed — goes on the following ticks without waiting for another purge.
- **The holder counts only once its own copy has applied the whole log.** It
  reads the log's end and waits for its applier to commit it — up to five
  seconds, and never past the interval the tick's sweeps share; a copy that is
  behind — catching up after adopting a snapshot, or with a backlog — would
  count anchors that the records already on the log remove, so it appends
  nothing that tick and says so.
- **One record at a time.** Each record is appended only once the holder's own
  applier has committed the one before it (again waiting up to five seconds),
  so a seat's write or a read's barrier that lands behind the sweep on the
  holder waits for one record's apply, never for the whole run.
- **Within a time budget.** The trim holder evaluates its own alarms on the
  same loop that sweeps, so a tick's sweeps share one alarm interval (ten
  seconds), counts included. Every log is counted first, and the interval is
  then shared among the logs that have anchors below their floor **that
  tick** — each handed an equal share of what is left, so what one leaves
  unspent goes to the logs after it. A log with nothing below costs the others
  its count and nothing more: a log draining a backlog on its own is given the
  whole ten seconds, and three logs draining at once a third each. Each
  stops appending when its share is spent, leaving the rest to the next tick.
  That bounds two things. The holder's alarms are evaluated at most one
  interval late, however slow its applier is. And a data node that applies the
  records more slowly than the holder, which builds the difference up in front
  of its own writes and reads, is left at most about (k − 1) budgets behind
  them when it is k times slower — about one budget at half the holder's speed
  — and never more than 128 records behind.
- **The floor is the log's first surviving record, not the published floor.**
  The published floor can run ahead of the log after a purge that failed, and
  an anchor of a record the log still holds is one the object's next write uses.
- **What it means for writes:** the next write to such an object asks the log
  what the object holds, finds nothing, and passes the same zero fence it
  always did — one refused append cheaper. Removing an anchor never lets a
  write past that fence; it only changes which way the write reaches it.
- **What it costs:** a record is a few hundred bytes on the log and is trimmed
  like any other. Applying one holds each node's writer for 21 ms at the 99th
  percentile with 20,000 anchors on the log, 56 ms with 300,000 and 75 ms with
  a million (measured on a 4-vCPU container's virtio disk —
  `go test ./internal/statelog -run '^$' -bench BenchmarkATrimmedRecordsSweep -benchtime 200x`
  re-measures it on yours). A backlog drains at up to 128 records — 32,000
  anchors — per log per tick where records apply quickly. On a large table the
  time budget can bind first: at a million anchors a record's apply and append
  take about 45 ms, so a log sweeping on its own still reaches the 128 inside
  its ten seconds — three million anchors a day — while each of three logs
  sweeping at once appends about seventy into its third, 1.7 million a day.
- **What to watch:** the duty logs `retention_anchors_swept` (`domain`,
  `generation`, `below`, `records`, and `more_remain` when the cap or the time
  budget stopped it with anchors still below) when it appends, and
  `retention_anchor_sweep_failed` (`WARN`) when it could not, its own copy
  being behind the log included — the purge and the floor have landed by then,
  and the next tick retries.
  Each node counts what its applier removed as `crewlet.statelog.anchors.swept`.
  An evicted node appends none.

## The cross-field rule

`snapshot_interval × (SnapshotsKept + 1)` must be **less than** `min_age`, or
every snapshot ages out of the replay window before its successor exists — a
node adopting one would land below a floor the log can no longer bridge.
`crewlet validate` checks it.

With the shipped defaults that is 24 h × 2 = 48 h against 7 days. Comfortable.

## Snapshots

A node below the log — whose next record has been deleted — cannot replay its
way back: the records it is missing are gone. What it does instead is fetch a
peer's snapshot of the replicated estate, verify it, and adopt it wholesale. (A
node that is only below the published trim floor, while the log still holds
what it lacks, replays it instead and needs none of this.)

**A snapshot is a copy of the replicated estate's file**, never of the node's
own. It is taken on `snapshot_interval` and judged against the file's size, and
the write-ahead log a copy holds back while it runs (below), for free space.
Every data node takes its own and offers it, whether or not it may
write the estate: an evicted machine, back with its files, keeps applying its
logs and may hold the only copy a joiner can fetch. A take the node declines is
retried rather than left for an interval: one declined for a reason a boot
settles by itself — `unhydrated`, `sole_node`, `lagging` — a second later,
doubling to thirty seconds, and the moment each log first catches up; any
other decline, or a take that failed, every thirty seconds; and `recent` once
the artefact it found has aged past `snapshot_interval`. A node that stays in
one of them — a single node is `sole_node` for good — settles on the
thirty-second retry within a minute. The register row naming a new artefact is
published as soon as it is taken, not on the position heartbeat's next beat.
Its manifest names every log it carries: a joiner refuses an
artefact naming a log it does not run, or missing one it does, before a byte
moves. Artefacts live in `store.snapshot_dir` itself.

**Each snapshot is one moment** of the file: as it stood when the take began,
with every record the node had applied by then and nothing it applied while
the copy ran, and the positions its manifest names are read from that copy.

**A take needs room for more than the copy.** Keeping the copy one moment
stops every checkpoint at that moment for the copy's whole run, so the live
file's `-wal` grows by what the node writes meanwhile, on the volume the
applier commits to: its write rate times the copy's run, which is the file's
size over the rate the copy reads it. Each copy measures how far the log grew
under it, and a take is declined `insufficient_space` unless
`store.snapshot_dir`'s volume has room for the copy, a tenth of it to spare,
and the log the last copy measured, scaled to the file as it stands — and,
where the snapshots live on a volume of their own, unless the replicated
estate's volume has room for that log. Until a copy has measured it since the
node started, the tenth is what stands for the log, which covers a node
writing its log at up to a tenth of the rate the copy reads the file.

```
crewlet retention snapshots
```

Its own verb rather than a block of `status`, because the repository is **per
node**: "which of my machines can donate, and how old is what they hold" is a
disk question, and it is the one you ask when a join has failed.

**`SnapshotsKept = 1`.** With N donors the fleet is the redundancy — one per
data node across at least two — and a recipient that fails a verification asks
the next donor. The trim's `snapshot_floor` asks for two of a log's counted
nodes to hold an artefact; a fleet of one data node is satisfied by
construction, its recovery artefact being a backup.

**A donor streams at most four artefacts at once, and refuses the rest at
once.** Every transfer is a sequential read of one large file with a credit
window of chunks in flight, and the fetch subject is a fan-in every joiner can
reach — so a fleet restarting together would otherwise spawn one goroutine per
joiner on whichever node holds the newest artefact. What is over the bound is
*terminated with a reason* rather than queued, because a joiner told no asks
the next donor immediately, while one waiting on a queue it cannot see spends
its whole collection window on a node that was never going to answer. For the
same reason a joiner shuffles the offers that are *equally* good before ranking
them: the best artefact still comes first, but which of several equally good
donors it asks is that joiner's own choice rather than the same on every node.

**A donor whose connection goes away dials again.** The donor serves over a
connection of its own, and it is the one carrying the largest messages a node
sends, so it is the first a server can close for good under it — a chunk past
a `max_payload` lowered under the node, a credential the server stopped
accepting. That stops the donor and nothing else: it serves peers, and a node
whose donor is down still serves its own company, so the node does not
[stop itself](deployment.md#an-external-nats-server) over it the way it does
over the connections it cannot run without. The donor logs
`statelog_donor_stopped` (`ERROR`, `component=engine`), naming why and when it
dials again, and dials again a second later, doubling to thirty seconds while
the dial is refused — the snapshot loop's own retry, since both ask whether
this node can serve a peer that fell behind. A refusal repeated on every redial
is said once; a donor that was serving and lost its connection again is said
again.

**The manifest names the position the file keeps.** The checkpoint commits with
the rows, so the position inside the copy is the only one that describes it,
and the donor reads it back out of the copy after the scrub rather than from
its own live applier — which has moved on by however many records landed while
the copy was taken. A recipient verifies the two agree and refuses an artefact
where they do not. A domain nobody has written to yet is at position zero in
both, and adoptable.

**On a single node the loop does not run at all.** It skips with the published
reason `sole_node` — the fleet counts no data node but this one — because a
full copy every day buys an artefact no peer can fetch. A solo deployment's
recovery artefact is `crewlet backup`. Who counts is the estate's counted set,
as the trim counts it: every node whose row names one of its logs, and every
live data node — so a node joining the fleet is counted before its first
report, and the lone data node already there takes the artefact that joiner
needs.

Every skip is published on the node's own register row, so a failed join has an
answer rather than a silence:

| Reason | What it means |
|---|---|
| `sole_node` | no counted node but this one; nothing to donate to. This node need not be counted itself: one an eviction removed, back with its files, still donates to a joiner beside it. Counted as the trim counts: every node whose row names one of the estate's logs, every live data node — so a joiner that has not published a row yet is somebody to donate to — less nodes evicted longer ago than the fence window. A view of the data nodes this node cannot confirm leaves them out, and the rows alone are counted |
| `lagging` | this node is more than 1 000 records behind |
| `unhydrated` | this node has not established a complete copy of some domain |
| `deferred` | this node holds a record it cannot decode |
| `insufficient_space` | not enough disk in `store.snapshot_dir` for the copy and the write-ahead log it holds back, or on the replicated estate's own volume for that log |
| `ahead_of_log` | this node's checkpoint is past the log's end, so its rows are keyed to a sequence space the stream no longer has |
| `log_diverged` | the log holds, at this node's checkpoint, another record than the one it consumed there — a broker restored from an older copy and written past these rows — so they are a history the log does not continue |
| `recent` | the newest artefact is younger than `snapshot_interval` **and names every domain this node runs, at the generation this node is on**; a young artefact short of a domain is replaced at once (`statelog_snapshot_recent_but_short`), because no joiner can adopt it — see [Running a Fleet](fleet.md#draining-and-rolling-upgrades). And see below |
| `failed` | the copy was attempted and errored; the engine log carries the error |

`recent` is the one reason a healthy node reaches, and it is therefore **not**
published: a node whose newest artefact is inside the interval is a node that
can donate, so its row carries the artefact and an empty reason. What the
column answers is "why can this node not donate", and a skip is only ever the
answer to that when there is no current artefact behind it.

A node that holds an older artefact and cannot refresh it publishes **both** —
the position it can still donate from, and the reason it has stopped
refreshing. A skip never erases what is already on disk.

**A reanchor or an adoption refreshes it at once.** A joiner asks for an
artefact at the generation each domain is on and refuses any other, so an
artefact from before a [reanchor](#re-anchoring-a-recreated-or-restored-log) is
one nobody can adopt however young it is. It does not count as `recent`, and a
completed reanchor or adoption wakes the loop — as does reopening a database a
failed adoption left closed, since the file it reopens may be the artefact that
adoption installed — so the node takes a new one as soon as its gate allows,
rather than up to `snapshot_interval` later, while every peer the reanchor left
behind waits with nothing to adopt.

### Sizing the snapshot volume

**×4.2 of the replicated store**, on a separate volume. Three states: steady is
live + WAL + its own snapshot + one backup copy = ×3.2; **rotation** is that
plus the new partial file before the old one is deleted = ×4.2; adopting is
×3.2.

`rotates_out_at` on the report is three-valued — a time, "never" for a fleet
that is not rotating, and unknown where the interval cannot be established —
because a fleet whose snapshot loop is skipping has no next rotation to name.

## The join runbook

A node that reports `below_floor` has lost its next record from the log. (One
below the published floor whose missing records the log still holds reports
`behind` instead, replays them, and needs none of this.) When a node reports
`below_floor`:

1. **`crewlet retention snapshots`** — does any peer hold one, and how old?
2. If one does, the node fetches it on its own — at boot, or the moment its
   own heartbeat finds it below the log while running: it pauses its
   appliers, adopts, moves its consumers to the artefact's position and
   resumes, with no restart. It takes a **hold** on every domain's log first,
   which pins the trim for the duration of the transfer — so a join cannot
   race the trim that made it necessary. It collects offers until every node
   that can donate has answered, or for five seconds at most. That is every
   live data node — each runs a donor, and one holding no artefact answers
   that it has none — and every node whose row in the positions register was
   written within the last forty seconds and names an artefact. The second
   half is how a **draining** node is waited for: it gives up its presence as
   its drain begins, so its peers stop counting it for seats, but it goes on
   serving its donor until it stops, and in a rolling upgrade it is the
   likeliest holder of the newest artefact. The asking node counts itself only
   once its own donor is serving, which at boot it is not yet. A node named
   that never answers — one that died within its row's forty seconds — costs
   the five seconds and no more. Who is live and what the register says are
   read while the offers arrive, so a coordination store slow to say costs
   nothing past those five seconds, and one that cannot say leaves the five
   seconds to decide.
   A running node that finds no donor stays as it is, refusing, and asks again
   on an interval that doubles up to five minutes. Stopping a node mid-join —
   a signal during its boot, or a shutdown while it is asking or fetching —
   gives the join up at once rather than waiting out the five-second offer
   window, and is never reported as a
   fleet with nothing to donate: the next start decides afresh. Nor is a join
   that loses the node's **own database** — an install that failed and whose
   live file then could not be reopened, or an artefact installed and then not
   opened. (A failed install deletes the artefact it fetched before it reopens
   the live file, so the reopen never competes with it for room.) At boot the
   node stops, naming both failures. While running it logs
   `statelog_estate_lost` at error level, naming the file: the node answers no
   tracker, page, usage or search read from its own copy until the file opens — its
   seats stay, and read the estate from the other data nodes.
   It reopens the file on its next heartbeat rather than on the doubling
   interval, because reopening its own file asks nobody — but it asks the
   fleet again only on that interval, and a join that fetched an artefact
   before it lost the file doubles it like any other failed ask, so a failure
   the transfer itself causes, such as a full disk, never becomes a transfer
   every heartbeat. If the line repeats, its error names what to fix — the
   disk, the file's permissions — and the node reopens the file on the next
   heartbeat after that. The file it reopens may be the artefact the failed
   join installed, so the reopen re-keys every domain's applier to it and
   wakes the snapshot loop, exactly as a completed adoption does; an applier
   whose log could not be read at that moment judges the file again when it
   resumes, and if that still leaves it refusing rows that are keyed to the
   stream the broker serves, the next heartbeat notices and a join re-keys it
   — `statelog_runner_not_rekeyed` is the line that says it happened.
3. If none does, the skip reason says why. Fix that first: a fleet where every
   node is `lagging` has an applier problem, not a snapshot problem.
4. If the fleet genuinely holds none — a single node, or every peer skipping —
   the path is a restore from `crewlet backup`, and the log has to still reach
   the artefact's position. `crewlet retention verify --restore` is what tells
   you in advance that it does.

**A donor on a newer schema is refused before anything moves.** An artefact
whose file carries a migration this node's binary does not have holds rows
shaped by code the node does not run, so the node cannot take it — and the
manifest lists the file's migrations, so the offer is refused from that list
rather than fetched whole and refused once it arrives (the file is still held
to its own list when it does). During a rolling upgrade that adds a migration,
a node still on the older build adopts from a peer still on it too, or is
upgraded first.

**So is a donor whose rows hold a record this build cannot read.** Each log's
entry in the manifest states the highest record version its rows were
*applied* from — kept beside the checkpoint and raised in the transaction that
applied each record — and a node whose build reads less refuses it, since it
would arrive past a record it could never apply. A build that merely *reads* a
newer version does not make its snapshots unadoptable: until something
publishes a record at that version the rows hold none, and a node still on the
older build adopts them.

**An adoption carries the operation ledger with it.** The ledger — the table
that says which operations have already been applied — travels inside the
snapshot, so the adopted node answers a retry of anything its donor applied
from it, and files work that was queued before the join like any other. The
point before which the ledger may have lost rows to its thirty-day sweep — and,
for the identity estate's sessions, to their hourly one — travels with it, so
the joining node answers a retry exactly as its donor would have.
See [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted).

### A node a peer re-anchored past

When one node [re-anchors](#re-anchoring-a-recreated-or-restored-log) the
tracker's, the knowledge base's or the code log, it opens the next generation
from its own rows — and every other node's rows are then a history the log no longer
continues from. Nothing on the log can bring them level, so each of those nodes
adopts a snapshot from a node in the new generation, **on its own**:

- It learns of the new generation from the re-anchoring node's position row,
  which that node publishes the moment the reanchor completes, on its next
  heartbeat — or sooner, from the reanchor's own record arriving on its log,
  which stops its applier before anything of the new generation is applied.
- From then until the adoption lands it refuses that domain's reads and writes
  as `wrong_stream`, logs `statelog_generation_passed` naming both generations,
  and takes its copy of that domain out of service — its seats stay, and that
  domain's operations go to the other data nodes — whether or not its own
  readings of the log look wrong. On a broker restored from an older copy, a node whose checkpoint was
  below the restored end sees nothing wrong at all, which is why the fleet's
  generation is what decides it rather than the log.
- It adopts through the join above, asking for an artefact at the new
  generation (the join logs `statelog_behind_a_reanchor`). Its applier is then
  re-keyed to the checkpoint it adopted and resumes with no restart.
- The re-anchoring node wakes its snapshot loop when the reanchor completes, so
  it can donate as soon as its gate allows. Until some node in the new
  generation holds an artefact, the others ask again on the doubling interval.

The vectors are exempt: each node re-anchors its own copy, so a peer ahead there
is a recovery in progress rather than a history this node has lost.

**If the node that re-anchored is gone for good** — decommissioned, or its disk
lost, before any peer adopted from it — nothing can donate the new generation:
its history was on that one machine. Every other node keeps refusing the
domain and asking for a donor, and a reanchor on any of them refuses, naming
the peer that "has already re-anchored" — force does not override that, because
while the peer might come back its rows are the fleet's history. The remedy is
to say it will not come back:

```
crewlet retention evict node-4 -confirm node-4
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
```

1. **Evict the peer**, from any remaining node. The eviction is the one write a
   node the peer re-anchored past still makes, so it lands even though that
   node refuses everything else; the command reports the stranded logs as
   `pending`, because the node's own applier is stopped and applies the record
   only after step 2.
2. From then on an evicted node's position row — and any trim floor it
   published — counts toward neither the generation the fleet is on nor the
   reanchor guards. Every node reads the peer's standing off the log itself,
   since a stranded node's applier never reaches the eviction record; so the
   nodes stop asking for a donor at the peer's generation.
3. **Re-anchor the most caught-up remaining node.** `reanchor` names the
   `abandoned` case: the log is the one these rows are keyed to and still holds
   everything they are missing, but it continues in a generation only the
   evicted peer held. The reanchor opens the generation **after** the peer's —
   its number was used, and its record holds that generation's subject — and
   follows the log from this node's own checkpoint. Every record written in the
   generation it skips is **void**: consumed, and applied into no row, because
   it was decided from rows nobody holds. Records the other nodes wrote in this
   node's own generation before they learned of the peer's move are kept.
4. Every other node then adopts from the re-anchored one, as above.

What the evicted peer applied that nobody else did — the tail it re-anchored
from, and anything written in its generation — is lost with its disk; the
reanchor keeps everything else.

**The refusal says which remedy applies**, and says it again as that changes.
While a live peer holds the new generation, or nothing yet shows that none does,
it names the adoption and, beside it, the eviction and reanchor above for a peer
that is gone. Once the only nodes that held that generation are evicted — the
record's writer is, or no un-evicted peer's row or trim floor stands at it any
more — it names the reanchor alone, since there is no snapshot left to adopt;
every heartbeat judges this again, so a peer evicted after the refusal began, or
readmitted, moves it (a beat that could not read whether the writer is evicted
logs `statelog_generation_holder_unread` and leaves it as it was). And a record
opening the next generation that **this node's own** reanchor appended before it
failed — the command gave up, or the node restarted part-way — is refused
naming that: nothing is adopted, and running the same `reanchor` again completes
it from that record.

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

It is also what stops an absent node's position counting anywhere else: an
evicted node's row — and any trim floor it published — counts toward neither
the generation the fleet is on nor a reanchor's guards, which is how a fleet
stranded by a node that re-anchored and then vanished is released ([a node a
peer re-anchored past](#a-node-a-peer-re-anchored-past)). The row itself
stays: a readmission is judged by it.

The trim counts nodes **per log**, so an eviction is a record on every log it
counts nodes on — every log that claims identity: the tracker's, the knowledge
base's, the identity estate's, the artifacts log's, the artifact datastore's
and the code host's. (The vector log and the usage log count nothing and get
none: a node behind on the vectors is a coverage figure, and an evicted node's
usage is still what its seats did.) Every node running the state log can make
the gesture, whichever backends the
company uses: every data node runs every one of those logs, and a company on an
external tracker still runs all six.

**The node you run the gesture on writes every log itself**: every data node
holds the whole estate, so one gesture on any data node reaches every log. A
readmission is **judged** on every log before any is written, against the
bound this node's own write fence holds a node to — the published floor and
the log's first surviving sequence — and an input it cannot read (the
positions register, a floor, a log's first sequence) refuses the readmission
rather than passing it unjudged: `readmission_unjudged`, with nothing written.
The same gesture, here once the read answers or through another node, finishes
it.

It is **judged once, before anything is written**: a node that still holds a
live presence lease is refused, because it is still reaching the fleet and
almost certainly running — an eviction would drop everything it writes and
take its copies of the logs it is evicted from out of service. Stop it and wait
for its `LIVE` column in
`crewlet retention status` to read `no`; `-force` overrides the refusal for a node wedged in a way
that still renews its lease. A refusal writes nothing to any log. If this
node cannot read the presence leases at all — a coordination fault — the
eviction is refused as one nobody could judge, and says so; `-force` takes it
past that too, since the leases are the only thing the judgement reads, and
the node's log records that the eviction was forced unjudged. A node id no node
could run under (the [`node.id`](../concepts/configuration.md#nodeid) rule:
alphanumeric first, then letters, digits, `.`, `_` or `-`, at most 64
characters) is refused before anything is judged.

Past the judgement the record goes to every log at once, and each answers on
its own line — the tracker's log, the pages log, the code log and the identity estate's — with the [three-valued outcome](replication.md#a-write-has-three-outcomes)
every write has — `applied`, `pending` or `unknown` — or `not written` with the
reason that stopped that log. A log that answered holds its record whatever
the others did, and the gesture runs to its end **whatever happens to the
command**: once the first record is about to be written the node finishes the
gesture under its own budget — thirty seconds for the logs — so a dropped
connection or a client timeout does not leave the node evicted on one log and
counted on another.

When **not every log holds it**, the command exits non-zero, and under each
log it did not finish prints what to do. Where running the gesture again can
finish that log — an `unknown` outcome, a node still catching up, a lost race —
it prints the command: the same one with `-op-id <id>` (and `-force` if the
first run had it). Each log's record is published under an id derived from the
gesture's, its sign, the log and the node, and each log's snapshot reads that
id's [ledger row](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted)
before anything is decided, judging it against the node's own row in the same
transaction: a log whose record is already the gate in force answers at the
position it already has, and only a missing log is written — even through a
node whose applier had not reached the first record yet, and however long
after the first run the retry comes. A fresh id would be a second gesture
rather than this one finished. The command mints the id **before** it asks —
in the engine's own grammar, carrying its instant, since the node refuses an id
that carries none — so a gesture the node never answered — the connection
dropped, the wait ran out, or what came back was not the node's own answer (a
proxy's 504 page, any status with no engine error code, a 200 cut off part way
through) — still prints the `-op-id` that finishes it.

Where running it again **cannot** finish a log straight away, it says what
has to happen first — and in every case but two, the gesture survives that
remedy, so it prints the same `-op-id` to finish it with afterwards. A fresh id
there would be a second gesture: every log that already holds the first one's
record would be written again, its eviction re-dated and its fence window
restarted, and the first id would answer `superseded` to anyone finishing it.

- `log_full` — that log is full to its broker ceiling, past even the
  [reserve kept for gate records](#the-gate-reserve), so no retry makes room:
  raise its ceiling with [`crewlet retention set-capacity`](#changing-a-logs-ceiling),
  then run the gesture again with the same `-op-id`. A log full only for
  ordinary writes does not refuse an eviction at all.
- `evicted` — the node you ran it on is itself evicted and writes nothing: run
  the gesture, under the same `-op-id`, through a node the fleet still counts
  (`-url`). Where the hint says the **gesture's record** on that log landed and
  applies nowhere, it holds the operation id there for the log's duplicate
  window (two minutes) from when it landed: the same `-op-id` sent sooner —
  through any node — is collapsed onto that record and answered `evicted`
  again, so wait out the window first. A fresh id is not the way round it, for
  the reason above. Where the hint instead names **another operation's** record
  — the newest on that log's subject when the gesture's answer was lost, which
  may be a healthy peer's — the refusal is only about the node you ran it on:
  run the gesture through another node at once, and if this node's own record
  did land, that node answers with the copy case below.
- `overtaken` or `abandoned` — the node you ran it on wrote that log's record
  from rows a reanchor left behind — after a restored reanchor it had not yet
  learned of, or in a generation a reanchor abandoned — so it landed and
  applies nowhere. It holds the operation id for the log's duplicate window
  (two minutes) from when it landed: once that has passed, run the gesture,
  under the same `-op-id`, through a node on the log's current generation
  (`-url`).
- **Another node's copy** — `evicted`, `overtaken` or `abandoned`,
  with a hint naming another node as the writer of the record at that position.
  The `-op-id` you sent had already been written under by that node — you ran
  the gesture through it first, and by the time its record landed it had been
  evicted, or it had written the record from
  rows a reanchor left behind (overtaken or abandoned) — and the node you ran it
  on this time had its append collapsed onto that node's record, which applies
  nowhere. The refusal is about that node, not this one, which was counted when
  it tried: once the duplicate window (two minutes from when the record landed)
  has passed, run the gesture again with the same `-op-id` through the same
  node.
- `unknown` that **this node cannot tell** — its operation ledger may have
  lost the row the operation needs, because the id was minted before the
  ledger's sweep reached it. The node
  published nothing and answers the same gesture the same way every time, so
  it is not offered as a retry: run it, under the same `-op-id`, through a node
  whose ledger reaches back that far (`-url`). The dashboard says the same and
  sends you to another node's dashboard.
- `wrong_stream` — the log was rebuilt under this node:
  [re-anchor it](#re-anchoring-a-recreated-or-restored-log) first, then run the
  gesture again with the same `-op-id`.

The two where the operation itself is over, and a new gesture is the only way
on:

- `superseded` — the operation's record landed and a later gate record on the
  same node has undone it since (an eviction retried after a readmission took
  the node back). Run a new gesture, without `-op-id`, if the node should
  change again.
- `op_reused` — the operation id already names a record on another object:
  run the gesture without `-op-id`.

What to do about each log comes from the node as an **action** — the same
gesture again, a new one, force, another node, a reanchor, a larger ceiling,
a wait or a restore ([the full set](../reference/api-endpoints.md#the-three-retention-gestures-that-write))
— beside a sentence that names no flag, and the command prints each action as
the flags it has. The dashboard renders the same actions as its own controls.

### From the dashboard

**Settings › Backups & retention** offers **Readmit…** on a node row its logs
hold evicted and **Evict…** on every other, and the dialog behind them is the
same gesture as the command, answered the same way: one row per log with its
outcome and position — no position for `unknown` — or the reason it was not
written and what to do.

- The dialog **mints the operation id in the browser before its first
  request**, in the engine's grammar and on the browser's clock, and keeps it
  for the whole gesture. So a request that timed out or dropped still holds
  the id — the dialog waits a minute, past the fifty seconds the node takes at
  most to answer one: twenty to judge it and thirty to write every log — and
  the dialog offers **Finish this gesture**, which sends the same request under
  the same id and reads every log's answer. So does an
  answer the node did not write: a reverse proxy's 504 page, any status with no
  engine error code in it, or a 200 cut off part way through. Only a refusal
  carrying the engine's own code is read as one.
- Where a log answers that the same gesture can finish it (`unknown`, a lost
  race, a node catching up), **Finish** sends it again under the same id, with
  force carried over if the gesture used it.
- Where a log needs something done first, **Finish** stays, for once it is
  done: a full log (`log_full`) needs a larger ceiling and a rebuilt one
  (`wrong_stream`) a re-anchor — neither has a control here, so each names its
  command — and a node that is itself evicted (`evicted`) cannot write, so the
  gesture is finished from the dashboard on a node the fleet still counts,
  under the same id (**Finish a gesture started elsewhere** takes it, and stays
  on the form until something answers — a refused id can be corrected, and one
  refused is not taken up as the gesture's own). The same goes for an `unknown`
  this node cannot tell about. The
  gesture's own id is what finishes each of them; a fresh one would write every
  log that already holds the record again and re-date each eviction.
- Only an operation no request can finish — `superseded`, `op_reused` — offers
  **Start a new gesture** instead of Finish.
- Closing the dialog does not lose the gesture: reopening it for the same node
  and the same sign offers **Finish** rather than starting afresh, and a node
  whose last eviction did not reach every log reads **Finish eviction…** on its
  row. A gesture that **did** reach every log is kept too, until the report
  behind the dialog shows it — the screen asks the report again as soon as the
  answer arrives, and meanwhile the row reads **Eviction sent…** (or
  **Readmission sent…**) and reopens that answer, never a fresh gesture that
  would write a second record on every log. It is let go of too once the node
  serving the report has applied every record the gesture wrote and the report
  still disagrees — somebody readmitted the node from the command line straight
  after, say — because that is a later change rather than a report behind; the
  row then offers the gesture the node's current state calls for. Not while
  that node could not read its evictions, though: a log whose evictions it
  could not read shows every node as not evicted whatever happened (the report
  marks that log's row `evictions_unreadable`, and the screen says so above the
  node block), so the gesture stays on the row until a report whose evictions
  were read.
- A **Finish** that is refused — it re-runs the judgement, so a lease listing
  this node cannot read or a node still catching up can refuse it — wrote
  nothing, and the answer the gesture already had stays on screen beside the
  refusal, with its operation id and **Finish**, and **Force eviction** where
  the refusal offers it. A Finish nobody answered keeps that answer too, below
  its **No answer** notice.
- A node still holding a live presence lease is marked **live** beside its
  **Evict…**, and its eviction is refused. The refusal — and the one for a
  lease listing this node cannot read — offers **Force eviction**, behind a
  second typed confirmation, which sends `force=true` and carries it into every
  Finish of that gesture.

It prints the watermark before and after, and the instant the eviction takes
effect. **The evicted node stays counted for about a minute** on each log after
that log's record lands, so a live node is certain to have read its own
tombstone before the trim passes it. `crewlet retention status` shows the node
as evicted only once every log holds its tombstone, and dates it from the
latest of them.

The honest worst case for that window is **zero**: a node three heartbeats late
reads its tombstone exactly when the trim may pass it. So the window is a
convenience rather than the safety property. The safety property is each log's
applier's own **eviction gate**, which drops an evicted node's records on that
log wherever they land, depends on nothing but the log's own order, and holds
when coordination cannot be reached at all.

Readmission is the inverse commit on every one of those logs rather than a
delete, so the whole history survives a replay:

```
crewlet retention readmit node-4 -confirm node-4
```

It is **refused while the node is below a trim floor**, and the refusal prints
its position beside the floor, because that inequality is the reason. A
readmission makes the trim count the node again, so readmitting one the floor
has passed — typically a machine that is still switched off — puts back
exactly the pin the eviction lifted, while the node itself is missing records
the log has already lost or is licensed to lose.

The comparison is the one the node's own [write fence](#the-trim-floor) makes,
made once for every log before any is written: its last published position
in each of the six logs, against the higher of each log's published floor and
its first surviving sequence. A node that has never published a position is
judged as holding nothing. Nothing is written to any log on a refusal, and the node needs nothing from you but time: start it if it
is not running, and it [catches up on its own](#the-join-runbook) — replaying
what the log still holds, adopting a snapshot where it does not. Readmit it
once it has applied every record up to the one just before that bound: in
`crewlet retention status`, its `SEQ` for each of those domains has reached one
less than the higher of the domain's `TRIM FLOOR` and `FIRST`. The refusal's
own `floor`, `first_seq` and `generation` are the numbers it compared — right
after a [re-anchor](#re-anchoring-a-recreated-or-restored-log) the domain shows `-`
in `TRIM FLOOR` until the trim's first tick on the adopted stream, and the bound
is `FIRST` alone — and running the readmission again is always safe, since a
refusal writes nothing. The node's `GEN` column says whether its position is
at the log's current generation at all: a position from one the log has left
prints `left gen N` in place of a lag, because its sequence is in a number
space that no longer exists and a readmission is refused on it whatever it
reads. A readmission that reaches some logs and not others is finished the
way an eviction is, with `-op-id`.

The refusal is not what keeps the fleet's data safe, and being refused is not
an emergency: a node below the floor is refused every write that assumes a
history it does not hold, by its own fence, whether it is readmitted or not.

## A full log refuses; it does not shed

At its ceiling the log **refuses appends**, naming `log_full`, the ceiling, and
the verb that raises it. It does not silently delete old records to make room:
shedding a record no node has applied is exactly the loss the whole gate
exists to prevent.

A single record larger than its log takes is a different refusal,
`record_too_large`, on a log with room to spare, and no ceiling or trim
changes it. Every log declares the largest record it publishes — 8 MiB less
4 KiB on the tracker's, the knowledge base's, the code host's, the vector
changelog's and the usage log's, which is the largest the embedded broker's
8 MiB messages carry once a record is signed, 2 MiB on the artifacts log's,
1 MiB plus 64 KiB on the artifact datastore's and 128 KiB on the identity
estate's — and a record past it is refused before it is sent; the stream's
own `max_msg_size` is that declaration plus 4 KiB for what a stored record
carries beside it, so the broker holds a peer on another build to it too. Two
more limits can refuse a record, and the detail says which it was: the file store's per-record limit,
and a NATS server's `max_payload` — never that of the server a node started
against, since a node refuses to start against one that holds it to less than
8 MiB, but that of one it has reconnected to since: a cluster member
configured apart from the others, or a server restarted with a smaller limit.
A limit lowered by a live reload is never such a refusal, because the client
is not told of it: the server closes the connection over the record, and the
node [stops itself](deployment.md#an-external-nats-server). See
[Read consistency](consistency.md#a-writes-refusals).

A full log costs `linearizable` reads, because those append a barrier — which
is every seat tool read. `stale` keeps answering, so the dashboard and the read
API are unaffected. See [Read consistency](consistency.md).

The **usage log** refuses the same way, and what it costs is different: no read
appends to it, so nothing a seat does is refused — what stops is the
replication of each node's days. Every data node keeps re-deriving its day and
retrying on every tick, nothing is lost from any node's own event log, and the
`log_headroom` alarm names the domain (`log_headroom on usage`) long before the ceiling. Its
size is a count of node-days rather than a rate — one message per (node, day,
and seat, person, schedule, or `visibility: unit` repository somebody read that
day) for 181 days — so a log that fills has outgrown its census: raise
`stream.usage_log_max_bytes` as below. A unit repository's day is one message
for each node that served its reads, naming every reader in under two hundred
bytes each ([Who read it](../concepts/code-host.md#who-read-it)): ten unit
repositories read every day on three nodes by a dozen readers each are about
13 MB over the 181 days, some 1 % of the default gibibyte.

The refusal carries **no retry hint**, deliberately: the only thing that frees
a byte is a fifteen-minute gated job, and the log is full precisely because
that gate is closed. A number here would be a promise the mechanism does not
make.

### The gate reserve

What fills a log is almost always a trim that cannot advance, and the
commonest reason is a node that is gone and still counted, pinning the
applied term. The gesture that unpins it — [`crewlet retention
evict`](#eviction) — is itself a record on that log, so a
log that refused it like any other append could never be emptied: the
eviction was refused, run again, and refused again.

So on the six logs that carry gate records — every log that claims identity:
the tracker's, the knowledge base's, the identity estate's, the artifacts log's, the artifact datastore's and the code host's — **ordinary writes are refused at a soft ceiling** below the
broker's, and the top of the ceiling — the **gate reserve**, `reserve_bytes`
in `crewlet retention status` — takes only the records that install or lift a
gate: an eviction and the readmission that inverts it. A log full for
ordinary writes still takes an eviction, and once the evicted node's minute
has passed the trim moves again. It takes it whatever else is refusing this
node's ordinary writes — a peer holding what the log lost (`log_truncated`,
below) or a generation the fleet moved past — because the eviction is the way
out of each, and a log nothing can write to is a log nothing trims. A purge is
not a gate record here: it frees nothing until the trim runs, and nothing
bounds how many are run. The node holds that at the write: a record that asks
for the reserve and is not a node's eviction or readmission — a purge included
— is refused before anything is sent, and passes none of the fences an
eviction is excused.

| | the logs that claim identity | vector changelog, usage log |
|---|---|---|
| ordinary writes and barriers refused at | the ceiling less the reserve | the ceiling |
| gate records refused at | the ceiling | — (they carry none) |
| `headroom_fraction` measured against | the ceiling less the reserve | the ceiling |

The reserve is sized so it holds however the fleet's writes race. Each node
reads the log's usage **after** it starts an append and counts its own
appends still in flight, so what can land past the soft ceiling is only what
other nodes have in flight at that instant — at most one of **that log's**
largest records each, plus 4 KiB for what a stored record carries beside it.
So the reserve is the larger of two terms:

- the **fleet term** — seven other nodes' largest appends, and a mebibyte for
  the gate records themselves — which holds at every ceiling a log may have,
  down to its floor;
- **a sixteenth of the ceiling**, which grows with the ceiling while the fleet
  term does not, so a larger log absorbs a larger fleet.

| log | largest record | floor | reserve at the floor | ordinary writes at the floor |
|---|---|---|---|---|
| tracker, knowledge base, code host | 8 MiB less 4 KiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| artifacts | 2 MiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| artifact datastore | 1 MiB plus 64 KiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| identity estate | 128 KiB | 64 MiB | 4 MiB (a sixteenth) | 60 MiB |

While a rolling upgrade runs, a node on an older build keeps no reserve, so
the reserve holds once every node counts its own.

If a gate record is refused even there, the refusal says so and points at
`crewlet retention set-capacity` rather than suggesting a retry; the
[`log_headroom`](../reference/alarms.md) alarm, which fires at a tenth of the
soft ceiling left, is there so that never happens.

## Changing a log's ceiling

A log's Tier A ceiling (`stream.tracker_log_max_bytes`,
`stream.tracker_vectors_max_bytes`, `stream.pages_log_max_bytes`,
`stream.code_log_max_bytes`, `stream.artifacts_log_max_bytes`,
`stream.artifact_data_log_max_bytes`, `stream.iam_log_max_bytes`,
`stream.usage_log_max_bytes`) is not a live setting: it is the value the log's
stream is created with, sized with the
other logs inside what the broker can grant (see
[Replication](replication.md#how-the-byte-ceilings-are-sized)). Changing a
running log's ceiling, raising it or lowering it, is a **fleet-wide
maintenance window**, and the reason is not caution:

- A resize is decided against the usage the log is at, and a publisher makes
  that a moving quantity. The verb decides it before the window opens: a
  target whose soft ceiling — the target less its
  [gate reserve](#the-gate-reserve), on the six logs that claim identity; the
  whole target on the vector changelog and the usage log — is at or below what
  the log already holds is refused, naming both and the least target that
  would do, because that ceiling would refuse every ordinary append the moment
  it applied. So is a target under the floor Tier A holds that log's field to —
  a gibibyte on the tracker's, the knowledge base's, the code host's, the
  artifacts log's, the artifact datastore's and the vector changelog's, 64 MiB
  on the identity estate's and the usage log's —
  naming the field. Anything else is fair, including a target under the
  current ceiling, which is how a log created larger than its budget gives
  the reservation back.
- A raise is a reservation too, and the broker refuses one it cannot honour.
  Where the node can read the limit the broker holds an update to (a lone
  embedded node, or a NATS account's own JetStream limit on any topology), a
  raise past it is refused before the window opens, naming what it reserves
  and what the broker has left. A clustered member cannot read that limit,
  because the member leading the cluster checks an update against its own
  reservations, so there the broker refuses such a raise when the window
  applies it, and the seal retires the attempt.
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

A node in either maintenance mode **keeps its presence lease** — the process is
running, and `crewlet fleet broker list`, the fleet view and the participant set
below all read it — while it claims **no seat** and hands **no writer** to any
surface, whether or not it holds data (a write a stateless node routes to a
data node is still one it starts): the operator's tools — the dashboard's and
an assistant's over MCP alike — offer no work-item or page write, the purge
route and a project file's upload and removal are not served, the chart's
projects, the containers and the repositories `code.repos` declares are
applied at the next normal boot, and a data node
in the mode answers "not here" to the writes a stateless node still in normal
mode routes to it. What appends to no log is still offered: a parked coding
run's answer (it waits on the seat's mailbox until a node holds the seat
again), a seat's pause and resume, a note to a running turn, and an answer
from the company's knowledge. Its reads are still served — the operator's read
tools and a project file's download among them — at every level but
`linearizable`, which proves the log's end by appending a barrier to it and is
refused [`maintenance`](consistency.md#the-fourteen-refusals). The operator gestures
that append a record are refused the same way: an eviction and a readmission
answer `409 not_publishing`, and a reanchor `409 reanchor_refused` naming the
mode. None of them is what a window needs — a participant that will not
acknowledge is excluded from the operation, not evicted from the logs — and
each would move the usage the window is measuring.

A company revision **activated during the window** is still applied — the
node serves the new epoch's reads and reports `ok` — and it starts none of
what the mode keeps off: no inbound edge (the node routes no webhook or chat
message to a seat, exactly as when it booted in that mode) and no scheduler
(a schedule the revision adds starts firing at the next normal boot).

The [object store](../concepts/object-store.md)'s **collection and audit do
not run** in either mode: the `object-collector` duty is a duty, and a node in
a window claims none — and each pass first pins the estate with a
`linearizable` read, which is exactly the barrier the mode refuses. They lose
nothing they would have done: no write lands a file whose object a collection
would free, and a pass that is due runs on the duty's next turn once a node is
back in normal mode. Nor does the `code-retention` duty purge a
[repository](../concepts/code-host.md#deletion-and-purge): each step is a
record on the code log, which a node in a window does not publish, so a purge
that came due waits for the window to close.

### Who has to acknowledge

The seal's proof is that every **broker** process restarted — a request the
broker already queued is retired by the process holding it going away — and
that every **publisher** was admitted. So the operation's participants are:

- every node the fleet holds a position for;
- every live node whose presence names the **`data`** role — or whose roles
  cannot be read, since leaving out a node that may hold data could pass a seal
  it should hold — because a data node publishes records;
- every live **broker member**, whatever its roles, because its broker holds
  queued requests whether or not the node keeps data;
- every live node whose presence advertises no broker kind this build knows —
  a newer build's — because leaving out a node that may
  be a member could pass a seal it should hold, while waiting on one that is
  not costs only an acknowledgement you can `exclude`;
- and the coordinator itself.

A **leaf** or a client of an external cluster that holds no data is not asked:
its broker queues nothing, and its seats publish through a data node, which is.
Each node's broker kind is on `crewlet fleet broker list`.

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

**You do not have to go looking for it.** While an operation is open, both
`crewlet retention status` and **Settings › Backups & retention** lead with
it — the stream, the phase, the attempt, how long it has been open, who ran
the verb, and who is still outstanding — because every number underneath describes a fleet in which
nothing is running, and a blocked trim read without knowing that sends you
after the wrong thing. **No acknowledgement outstanding is not progress**: it
means the operation is waiting on its operator, and both surfaces say so
rather than printing an empty list.

Two gestures act on what it shows:

| | |
|---|---|
| `crewlet retention maintenance exclude -stream NAME -node ID -confirm ID` | Your assertion that a participant's **process is stopped** and holds no outstanding request. The only thing that waives an acknowledgement — an eviction does not, because that is about whose records apply and this is about whose process is running. |
| `crewlet retention maintenance abandon -stream NAME -confirm OPERATION-ID` | Changes what the operation is trying to reach; **never** the barrier it must cross. From `opened` it clears outright, because no request was ever issued. From anywhere else it still enters the seal: a paused coordinator's request is outstanding whether or not a person has read a status page. |

Both are recorded on the window with who made them — the status prints each
excluded participant as `excluded by` its author and the credential they used,
and an abandoned window's `ABANDONED` line the same way — because an exclusion
is the one fact the seal takes on somebody's word.

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

## Re-anchoring a recreated or restored log

If a stream is genuinely recreated — deleted and remade, rebuilt by hand, or
provisioned afresh on a broker that lost its store — its sequences restart at
1. Every position the fleet holds then names a number space that no longer
exists: a stored version, a consumer cursor, an arbitration anchor. Nodes
refuse to serve, which is correct.

The engine detects this from the stream's own **creation instant**, which
every applier compares at boot against the instant its checkpoint was
committed under. It is the instant the node whose create made the stream
stamped into the stream's configuration, as the metadata key
`crewlet.created_at` (`nats stream info` lists it under the stream's
metadata, on an embedded fleet through a member's client listener with the
[operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool)) — not the broker's own creation time, which a snapshot restore
resets to the moment of the restore, and which is read only for a stream
that carries no stamp, one an operator created by hand. A stream restored
from a snapshot is handed back the configuration it was taken with, stamp
and all, so it is the **same** stream: it holds every record at its own
sequence, and is judged exactly as the restored broker below is. A stream
the engine provisions again after it was lost is stamped anew, which is the
recreation. A stream deleted and remade from a configuration that still
carries `crewlet.created_at` — the configuration `nats stream info --json`
printed, passed to `nats stream add --config`, which is the usual way to
recreate one identically — is the **same** stream to every node too: the copied
instant declares it the same history, so it is never refused as recreated and
is judged only by where it ends and what it holds at each node's checkpoint
(below). Its re-anchor is then the restored case, followed from its end, so
records written into it since need `-discard`; as a recreation it would be
followed from its first surviving record. To make a deliberate rebuild read as
a recreation, remove that metadata key from the configuration before creating
the stream. On a difference the applier **stops** rather
than resuming — the log line names both instants and this verb — the node's
reads and writes refuse `wrong_stream` with that reason, it takes its copy of
that domain out of service — that domain's operations go to a peer, and its
seats stay — and `crewlet retention status` leads with `NOT READY <domain>:
wrong_stream (recreated): …` carrying the same sentence, both instants
included (the dashboard shows the same refusal above the domain's block). A checkpoint past the log's end is caught as `wrong_stream` too,
because a position the log has never reached is a position on another stream.

**A broker restored from an older copy is the case the instant cannot see.**
Bringing back the broker's store directory from a disk snapshot — or, for the
embedded broker, restarting the engine on yesterday's volume — keeps the stream
it had, creation instant and all, and so does restoring its streams from their
snapshots (a [`crewlet backup`](backup.md#restoring), or `nats stream
restore` against an external cluster), which hands each one back the configuration that carries its
instant; so every identity check passes. What differs
is where the log **ends**: below the checkpoint of a node whose own database is
newer. Such a node refuses every read and every write of that domain with
`wrong_stream` from the moment it boots, logs `statelog_ahead_of_log` with both
numbers, and `crewlet retention status` on that node leads with `NOT READY
<domain>: wrong_stream (ahead_of_log): …` naming both. Its writes have
to refuse, not only its reads: whatever it appended would land at the log's
next sequence, which is one its own applier has already passed and will never
apply.

That refusal of the end lifts when the end reaches the checkpoint again — an
end read from a stream member that has not caught up looks exactly the same,
and a refusal nothing could lift would take a healthy node out over one leader
election — and the node logs `statelog_ahead_of_log_cleared`. **What decides
whether it carries on is the record, not the end.** Every checkpoint names the
record it stands on, by the broker's own storage instant for it, and before the
node applies anything past its checkpoint it reads the record the log now holds
at that sequence. The same record is the stale member it looked like, and the
node carries on. Another record means the restored log has been written past
this node's rows — by a node whose own database was not ahead of the copy — so
the log continues a history these rows are not. The node then logs
`statelog_log_diverged`, applies nothing past its checkpoint, refuses every
read and every write of that domain as `wrong_stream`, donates no snapshot of
it (`log_diverged`), and says so on its position row (`log_diverged`) —
which is what every other node's `crewlet retention status` marks as `LOG
DIVERGED` on that node's line, and its own leads with `NOT READY <domain>:
wrong_stream (log_diverged)`. That
refusal does **not** lift on its own: a member that has not caught up answers
that it holds no record at a sequence, never with another one. The node finds
it the same way when it **boots** on a log already written past its rows — its
checkpoint is then not past the end at all, and the record is the only thing
that shows it — and when a broker is restored under it while it runs. Then
whatever the node held past its checkpoint when it met the restore — a failed
fetch, or the log's end read below what it held — goes back to the broker
unapplied first (`statelog_apply_unwound`): linearizable reads' barriers it had
reached and not yet committed, records waiting above a hole or for the next
transaction. So the record compared is the one its last commit left at the
checkpoint, and nothing the old history delivered is applied after the
restore. It is compared for equality, so no clock is ever ordered against
another.

A checkpoint a reanchor placed where the log held no record — below a rebuilt
stream's first record, or at a sequence the log skipped — names nothing, and
there is nothing on the log to compare it with: the node carries on, and the
first batch it commits names its own record. It is never named from the log's
record, which is exactly the thing in question. Past the log's end it is
treated like any other checkpoint there, and nothing past it is applied until
the log reaches it. The one thing such a checkpoint cannot show is a broker
restored afterwards from a copy that put a record back at its sequence; that
goes unnoticed until the next batch names a record, so after restoring a broker
behind a node that has not committed since its reanchor, re-anchor or replace
it rather than waiting for that batch.

**The rest of the fleet stops writing to it too.** A node whose rows were the
copy's age sees a log that is its own history, so nothing about its own log
refuses it — yet every write it makes after the restore is one the restored
reanchor of a newer node would apply nowhere, acknowledged to a caller who then
loses it. So every node reads the positions register on its heartbeat, and a
peer on the same stream, in the same generation and not evicted, whose position
is past the log's end — confirmed against the log, which must not hold the
record there — or whose row says `log_diverged`, stops this node's **writes**
of that domain: each refuses `log_truncated`, naming the peer, and the node
logs `statelog_log_truncated`. Its **reads** go on, being the log's own history.
The eviction of that peer is still written, because it is one of the ways out —
into the [gate reserve](#the-gate-reserve) if the log has filled meanwhile.
The refusal lifts on its own (`statelog_log_truncated_cleared`) once the peer
re-anchors, is rebuilt from a peer's snapshot, or is evicted. What this cannot
see is a newer node that has not published since the restore — after a whole
broker restore the register is the copy's too — so writes made before it boots
still land.

**After restoring a broker, re-anchor the most caught-up node whose checkpoint
was past the restored log's end, before anything else writes to it** — the verb
below treats it as the *restored* case and follows the log from its end, and it
treats a node the log has diverged from the same way — and every other node,
ahead of the restored end or not, then adopts from it on its own: see
[a node a peer re-anchored past](#a-node-a-peer-re-anchored-past).

**If something wrote the restored log first, the reanchor asks you what to
keep.** A restored reanchor follows the log from its end, so any record a node
whose rows were the copy's age wrote after the restore sits below that end and
would be applied on no node — lost to whoever it was acknowledged to. Before it
runs, the verb walks the log back from its end to the newest record that writes
rows (a linearizable read's barrier writes none and is stepped over) and asks
whether this node applied that very record: the same operation, at the same
position, stored by the broker at the same instant. If it did, nothing written
after the restore wrote rows, and the reanchor runs. If it did not, the verb
names that record — its kind, subject, writer, operation and sequence — prints
it before the command it would run, and **refuses unless you pass `-discard`**.
There are two ways out, and only you can choose between them:

- **Keep this node's rows** — what they hold past the copy — by re-running with
  `-discard`. The records written after the restore are applied on no node;
  the generation record says so, naming the newest, and every other node adopts
  this node's snapshot.
- **Keep what was written after the restore** by not re-anchoring this node:
  stop it, move its replicated database aside and start it again. It replays
  the log from the broker, or adopts a peer's snapshot where the log has been
  trimmed, and what only its old rows held is given up. Its old position row is
  replaced by its new one on its first heartbeat, which also lifts every other
  node's `log_truncated`.

The walk vouches for a record through the operation ledger, which travels
inside every snapshot — so a record this node holds because the peer it adopted
from applied it counts as held, exactly as one it applied itself. It cannot
vouch where the ledger has lost the record's row to its thirty-day sweep —
this node's, or the donor's it inherited with an adopted snapshot. The ledger
records how far back it may have lost rows, and when the
named record's operation is older than that the refusal says so — the rows may
hold the record after all. A record a gate dropped writes no row and reads as
not held too. In each of those cases the verb can refuse when nothing was
written after the restore at all, and `-discard` then discards nothing, because
the record it names is one the rows already hold. What it never does is follow
the log past a record it cannot vouch for without saying so.

**The checkpoint goes one below the verb's own generation record**, not at the
end it read: the verb appends that record first, so anything written between
its reading and its append lands below the record too. If a node whose rows
were the copy's age writes the log in that moment, the verb walks the log again
up to its own record, names what landed, and refuses with the generation already
open — re-run it with `-discard` to finish, or keep the record by replacing this
node's rows with a peer's and evicting this node, which abandons the generation
it opened.

**What the rest of the fleet writes after the reanchor is not applied here.**
The other nodes learn of a restored reanchor when their appliers reach its
generation record or their heartbeat reads the fleet's new generation, and a
node whose rows were the copy's age can write once more in the old generation
before either happens. Such a record is decided from the rows the reanchor did
not keep, so wherever the re-anchored checkpoint is followed from — this node,
and every peer that adopts its snapshot — a record positioned after the
reanchor's own record and written in a lower generation is void: consumed and
applied on no node, logged `statelog_record_gated` naming `overtaken`.

**A reanchor that stops part-way is re-run.** Once its generation record is on
the log, the verb rebuilds this node's consumer and commits the checkpoint on
the node's own time rather than the caller's, and the CLI waits up to six
minutes for it. If it stops anyway — the broker blinked, the node restarted —
re-run the same command: it finds its own record, follows the log from one
below it, and still names every record written after the restore beneath it.

**A rebuild under a node that never restarts is caught too**, wherever the node
reads the stream's state: the position heartbeat does every ten seconds, and a
write that would publish at an expectation of zero does within the write. The
creation instant arrives in both answers. Nothing else can see it — a rebuilt
stream comes back at generation 0 counting from 1, so once it has published
past the node's checkpoint every sequence term reads healthy while the node
applies a different history into rows keyed by the old one. The node logs
`statelog_stream_recreated` with both instants, takes its copy of that domain
out of service — its seats stay, and that domain's operations go to the other
data nodes — and refuses
every read and every write of that domain with `wrong_stream` until that stream
is re-anchored, and it applies nothing from the rebuilt stream into rows keyed
to the old one.

**Writes refuse, not only reads**, and every kind of write rather than only the
retry at zero. Every expectation a node forms is a sequence from the stream its
rows came from, and the rebuilt stream — same name, same generation — would
arbitrate it as a sequence about itself: an expectation of zero would land on a
subject the old history had written, and an ordinary one would be accepted
wherever the rebuilt stream's own history happened to end at the same number.
Anything that landed would be applied by every data node once the stream is
followed from its head. A refused write is an `unavailable` answer naming
`wrong_stream`, both creation instants and the `reanchor` command below.

`reanchor` is the response:

```
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
# prints the live stream's created_at and which case it is, and refuses

crewlet retention reanchor -stream CREWLET_TRACKER_LOG -confirm 2031-04-02T03:00:00.418226517Z
```

The confirmation is the **live** stream's own `created_at` — the instant the
`wrong_stream` refusals name as the live stream's, its `crewlet.created_at` —
exactly as the verb prints it,
nanoseconds and all. The verb **prints it and refuses** rather than reading it
and feeding it straight back — otherwise it would be confirming against its own
output. An instant that differs at the microsecond is another stream, and is
refused.

What a reanchor says is: *these rows are what they are; follow the live stream
from here.* The durable tables are the record of truth and the stream is a
replay window, so the rows survive and the window is replaced. The generation
is what makes an old position **comparable and safely stale** rather than
indistinguishable from a current one — an arbitration anchor below it forms
`expect = 0` on its next write, and a client cursor below it is refused by name.

**Three cases, and they differ in where the log is followed from.** The verb
decides which from the same reading of the stream it keys the checkpoint to,
prints it before you confirm, and names it in its answer and in the
`statelog_reanchored` line:

| Case | What the broker holds | Where the new checkpoint goes |
|---|---|---|
| `recreated` | Another stream: its creation instant is not the one this node's rows are keyed to. It holds nothing the rows came from. | One below its **first surviving record**, so the domain applies everything it still holds. |
| `restored` | The **same** stream — creation instant and all — brought back from an older copy, ending below this node's checkpoint — or written past it since, so it holds another record there. What it holds up to the copy is a prefix of the history the rows came from. | At the log's **end**, so none of those records is applied again. |
| `abandoned` | The **same** stream, holding everything the rows are missing — but continuing in a generation only an [evicted peer](#a-node-a-peer-re-anchored-past) held. | At this node's **own checkpoint**, in the generation after the evicted peer's, with every record of the generation it skips void. |

**An evicted peer's generation only ever raises the number.** Whichever case it
is, a reanchor opens the generation after every one this domain has used —
this node's own, and any a peer the fleet has evicted opened, found from its
position row, a floor it published, or its generation record on the log — and
the records of any generation it skips are void wherever its checkpoint is
followed from.

The difference is not cosmetic. A restored copy replayed from its first record
would be applied in a generation that outranks every row, so each object would
roll back to the state it had when the copy was taken, and whatever the rows
gained since would be written over. A same-stream log that ends at or past the
checkpoint, holding there the record the checkpoint names, is neither case — it
still holds every record the rows are missing — and the verb refuses it as
having nothing to re-anchor.

**It moves one log.** Each domain — the tracker, the knowledge base, the
identity estate, the vectors — has its own stream and its own generation, and a reanchor moves only
the one you named: that domain's checkpoint goes to the next generation, where
its case puts it, keyed to the live instant. Every other domain keeps its
checkpoint, its generation and its stream exactly as they were. If more than
one log was rebuilt, re-anchor each of them.

**It needs no restart.** The domain's apply loop is paused for the length of
the transition and resumed on the adopted stream when it completes: its reads
and writes are served again, its seats can come back, and nothing else on the
node pauses. A reanchor that is refused puts the loop back as it was.

In order, it: checks the confirmation against the live stream; refuses while
the fleet guards below say so, and on a node evicted from that domain; appends
the domain's own generation record to the adopted stream (every log that
claims identity keeps one, applied as `tracker_log_generations`,
`pages_log_generations`, `code_log_generations` and `iam_log_generations`;
the vectors and the usage log keep none); reads the stream's instant
again, refusing if it was rebuilt again meanwhile; moves this node's consumer;
and commits the checkpoint. Every step before the checkpoint can be repeated,
so a reanchor that failed part-way is finished by running it again: it derives
the same generation and finds its own record already there.

It rewrites no row. Nothing needs it: a version or an anchor from before the
reanchor sits below every position the adopted stream will produce, which is
the order every comparison wants. After a restored reanchor an object whose last
record the copy kept is written against that record — the rows already hold it
— so nothing is held up waiting for an applier that will never re-read it.

For a recreated log it does **not** recover records that were on the old stream
and were never applied here. For a restored one it keeps what the rows hold
past the copy, which is on no log any more — so no other node can replay it,
and each of them adopts a snapshot from a node in the new generation on its
own ([a node a peer re-anchored past](#a-node-a-peer-re-anchored-past)).

For every log that keeps a generation record — the tracker's, the knowledge
base's, the code host's and the identity estate's — it refuses while any peer
has already re-anchored the stream — a peer at a later generation of it — naming the peer:
that peer's rows are the fleet's history in the new generation, and a second
reanchor from another node's rows would open the same generation over a
different prefix of what was lost, which nothing could reconcile. This node
adopts that peer's snapshot instead, and does so on its own ([a node a peer
re-anchored past](#a-node-a-peer-re-anchored-past)). A peer the fleet has
**evicted** is not counted, in this rule or the next: its generation is
abandoned rather than the fleet's. Otherwise only
the most caught-up node on the stream its rows came from may re-anchor — among
the peers holding history the log does **not**: a peer whose checkpoint record
the log still holds is on the log, so nothing it applied is lost by a reanchor
that follows it, and it is not weighed (whether what it wrote after a restore
is kept is the `-discard` question, not this one). So a node the log diverged
from can re-anchor although the copy-age nodes that wrote the log past it
stand further along the log than its checkpoint. Every data node publishes, beside
its position, the creation instant of the stream its rows are keyed to and the
broker's instant for its checkpoint's record, so a peer still on the lost
stream, or past a restored log's end, is compared with this node and one that
came up on the rebuilt stream with no rows is not: it is further along nothing
a reanchor discards. A refusal names the peer that is further along. `-force` overrides the
most-caught-up rule for the case where the fleet cannot be asked, and never the
first. The
vectors are exempt from both: their coverage legitimately differs node to node,
so every node re-anchors its own copy.

**And one generation is only ever opened once.** Two nodes that re-anchor the
same log at the same moment — each before the other's position row says so, or
both forced while the register could not be read — derive the same next
generation, and exactly one generation record lands. The other reanchor reads
back whose record it is, refuses naming the node that opened it, and commits
nothing: two openings of one generation would put two histories under one
number, which no force makes safe. That node adopts from the one that got there
first. A reanchor that failed part-way still finishes when it is run again,
because the record it finds there is its own.

**It moves that log and nothing else.** The new cursor, the generation and the
reset are all computed from the one recreated stream, so the reanchor moves
that domain's cursor alone; it used to move every domain's, to a position in
the tracker's sequence space that none of their own logs has. And it records
who ran it: the generation record every data node applies into its audit row names
the operator and the credential they used, where it named the node that served
the request.

**Today that log is the work tracker's**, `CREWLET_TRACKER_LOG`. A reanchor is
three steps only the log's own domain can take — reset its rows' versions,
publish its generation record, write its audit row — and the knowledge base,
the identity estate, the vectors and the usage log have none of
their own yet, so naming one of their logs is refused before anything moves.
It used to run the tracker's steps for them, resetting the tracker's rows for a
log that was not the tracker's. Recover one of those logs by adopting the snapshot of a
peer hydrated on the live stream, or from a backup.

> **The verb could not have completed before this build.** Its reset named
> three tracker tables with no `version` column and failed on the first, and
> past that it handed the framework a store handle that answered "not open"
> when the cursor was moved. Nothing moved in either case, so no estate was
> left half re-anchored.

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
store copy (its estate, as `crewlet backup` names it) with its size and migration count, and every domain's generation and
sequence —
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
- **Purging** removes the rows, on every data node as each one reaches the record.

```
crewlet work purge <item> -reason "why" -confirm <item-key> [-op-id ID]
```

`<item>` is the task's key or its id. The purge removes every row the task's own records wrote, not only the task's: its
comments and body revisions, every inbox notice, turn record and
dependency-mirror row about it, and the content of every history row — a
history row carries its record's whole change, so leaving it would leave the
text the purge was run to destroy. One account survives, and it is the purge's
own: its history row and the project lead's `purged` notice, which say that it
happened, to which key, by whom and why. Beside it stays a **skeleton** of the
task's history and nothing more: the rows that moved a count — its creation, a
removal or restore, and each change of its status, assignee or project — each
cut to its kind, its instant and those three changes, because the work flow
answers the past by walking them backward and a purge takes the task out of
that series only from the moment it happened.

The confirmation is the task's **key**,
and the node checks it against the task `<item>` resolves to, destroying
nothing on a mismatch: the id may be what is on the command line, so repeating
it would confirm nothing, while the key has to be looked up — which is the point
of asking. There is no project to name: the task's own row says which project
the record is filed under. The **reason is required** because it is the
only thing that survives: the rows are destroyed, and the deletion marker's
reason is the entire account of what used to be at that key. A purge is an
**operator gesture** — `admin`, held by a person or a token (a person's
session asks for a recent proof first), never an agent and never the engine — because nothing else can be asked to confirm it.

It answers the same three-valued outcome every write here has. `pending` means
the record is on the log and each node's rows go as it reaches them; do not run
it again. `unknown` is the one to retry, and the printed operation id goes back
in `-op-id` so the retry cannot append a second purge of a task the first one
may already have destroyed — and when it did, the retry answers `applied`, at
the first purge's position, rather than finding the task gone and refusing.
Pass it back exactly as printed: the id carries the
instant it was minted, which is what a node judges the retry by once its
operation ledger may have lost the first purge's row to the ledger's
thirty-day sweep, and there it answers `unknown` again rather than purging twice. An id of your
own making is refused (`op_id_invalid`): it carries no instant, so no node
could tell whether it already ran. So is a printed one altered on the way back
— trimmed, spaced, or grown past 128 bytes — because the broker carries the id
in a header that trims its ends and rewrites a line break, and would
deduplicate it as another operation.

A purge names **one** task, so its **children are moved, not destroyed** —
each direct child re-parents onto the purged task's own parent, or becomes a
root when the purged task was one, and the subtree's depths and ancestry are
rebuilt with it. Destroying the subtree would destroy work nobody confirmed,
and leaving it alone would leave every child pointing at an id that resolves to
nothing.

What it prints deliberately gives **no time guarantee**. An offline or evicted
disk retains its copy until replay, adoption, replacement or destruction, and
there is no duration to state. Saying "within 24 hours" would be a promise the
architecture cannot keep.

### A repository

A repository the company [hosts](../concepts/code-host.md) is removed by the
configuration rather than by a command, and its three stages are timed:

- **Deleting** is removing it from `code.repos`. At that activation it takes
  no push, and its refs and packs stay; declaring it again under its name
  restores it as it was.
- **Purging** follows `code.deleted_retention_days` after the deletion (unset,
  7 days). The `code-retention` duty removes the repository's rows in bounded
  steps, each one record every data node applies, paced to a third of each
  node's write lane, and the last leaves only its deletion marker, which drops
  every later record about the repository on every node, for ever. A purge
  once begun is finished whatever the retention says, since it has no undo,
  and the name is free again once it is. One begins only on a node serving
  the revision the fleet is on, so a node that failed to apply a longer
  retention never purges by the shorter one it replaced.
- **The bytes** go a day after that: once no row names a pack, the
  [object collector](../concepts/object-store.md#collection-and-audit)
  deletes it like any other object nothing names.

A repository a tool made is deleted by the tools rather than the
configuration, and purged the same way. A seat's own **scratch** repository
is also deleted by the `code-retention` duty once it has sat idle — no push,
rename, settings change or restore — for `code.scratch.idle_days` (unset,
7 days), unless a live turn holds it; it is then restorable and purged like
any deletion. A renamed repository's **old name** keeps redirecting for a
month and is released by the same duty after it, one small record a name
([Repositories made by tools](../concepts/code-host.md#repositories-made-by-tools)).
And a branch a seat set in its own namespace (`refs/heads/seats/<handle>/`)
that nobody has set for **14 days** is deleted by the same duty, through a
push of the engine's own whose row keeps the commit to restore it at — unless
a live turn holds it, it is its repository's default branch, or a live change
proposes from or into it ([The ref lifecycle](../concepts/code-host.md#the-ref-lifecycle)).

Inside a repository that is not deleted, three horizons are kept by the
records that apply to it, never by a sweep: a push row expires **181 days**
after its push, the same half year as the [usage history](#old-data-is-not-cold-data),
a deleted ref's tombstone after **48 hours**, or sooner, oldest first, once
the repository holds 100 000 of them, and the record of the object a forced
push or a deletion left after **14 days**, by the repository's next push
whose record has room for it (see
[ref tombstones and ref drops](../concepts/code-host.md#retention-and-history)). A
[change](../concepts/code-host.md#changes) and its revisions have no horizon of
their own: they are kept as long as their repository is, and its purge removes
them with the rest of its rows. Nor do a repository's published layers, and
they need none: `code_layers` holds at most 32 rows a repository and
`code_layer_parts` at most 16 a layer, each publication replacing the rows it
supersedes in place, so neither grows with pushes — at most 544 rows a
repository, which its purge removes with the rest. The code log itself is
trimmed by the same six terms as every other log; a push record names its pack
and never carries it, so trimming the log removes no repository's bytes.

## The configuration archive, and the one thing a purge cannot reach

Every node keeps its **own copy of every configuration revision it has ever
met**, in `company_config`, and until this release nothing deleted from it.
One row per config write, per node, each holding the whole document.

**The revision sweep** bounds it. It is part of the ordinary maintenance tick and runs on
**every node**, not under the fleet singleton: each node owns its own copy, so
a singleton would tidy the node holding the duty and let the table grow for
ever on every other — which looks exactly like a sweep that works, to whoever
checks the node it ran on. It keeps everything inside **400 days**, plus the
**active revision and its whole parent chain whatever their age**. The chain
is kept by id rather than by date because a revert re-activates an older
revision and `crewlet config diff` walks it, so a swept ancestor turns both
into an error naming a row that used to exist — and a company that has not
changed its configuration for a year has an active revision older than any
horizon worth setting.

Four hundred days is an annual cycle plus five weeks. The gestures that reach
furthest back into configuration history are annual — a reorganisation
repeated each year, a compliance review, a renewal — and exactly 365 days
makes finding last year's revision a coin flip on the day the gesture is
repeated. The floor is the **audit log's own horizon** (31 days): an audit row
naming a revision must still be able to open it, so this can never be set
below that.

**And a revision is a copy of the org chart it ran.** A human seat's `email`
and `contact` account ids are in every revision that carried them — on every
node, in every backup — and removing the seat writes a *new* revision rather
than reaching the old ones. They leave the store when the sweep above takes
their revision, and a backup when the backup is deleted.

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

The **usage history** is the one replicated table set with a horizon of its
own: each node's company days — spend, turns, page reads, schedule fires — are
kept **181 days**, which is a ninety-day window, the ninety days it is compared
with, and the day a moved clock can touch. Nothing sweeps them: every record for
a day deletes the rows older than that day minus 181 in the same transaction
that writes it, on every node, and the stream's own age bound forgets the same
days. See [Replication](replication.md#two-compacted-domains-the-embeddings-and-each-nodes-day).

Two tables are the exception, and both are swept **per node** rather than once
across the fleet — each node applies the log into its own copy, so a fleet
singleton would tidy one node and let the table grow for ever on every other,
which looks exactly like a sweep that works to whoever checks the node it ran
on.

Each domain's **operation ledger** — the table that answers "did the operation
I published land here?" — is swept at **30 days**. The horizon comes from the
client that actually re-asks: a machine retry lives inside a five-second wait,
but a seat carries an operation id forward and re-asks on its next wake, hours
or a weekend later. An operation id older than that resolves `unknown` rather
than `applied`, which is the honest answer once the row is gone — and it is
never applied a second time: each pass records the cutoff it deleted before,
and a write whose operation was minted earlier than that, with no row left to
say whether it already landed, is answered `unknown` without being published
(see [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted)).

That horizon belongs to the **domain** rather than to the framework, and a
domain may declare a **shorter** one for a subject kind whose writers never
re-ask late. Every domain takes 30 days for its ledger as a whole, because the
client that re-asks is the same for all of them — and the identity domain keeps
its **session** subjects' rows for **one hour**: a row per sign-in and per
sign-out, whose operation ids nobody re-asks after the request that wrote them
(a sign-in is answered inside its request; a sign-out and an administrator
ending somebody's session each re-decide rather than re-ask). Kept 30 days they were the busiest thing that domain wrote, for a
question nobody asks after an hour. The hour is the publisher's own
five-second resolve budget with a wide margin, and it is its own maintenance
job, `iam_ops_session`, so what it deletes is counted apart.

A strict log's **arbitration anchors** — one row per subject any writer has
ever contended on, holding the position of the last record consumed on it —
are removed below the log's **first surviving record**, and never on a clock:
the trim holder appends a `trimmed` record and every node's applier removes the
same rows in the transaction that applies it (see
[Anchors below the log are removed by a record](#anchors-below-the-log-are-removed-by-a-record)).
Below that record there is nothing left to replay, so nothing left for the
anchor to be the anchor of; an anchor above it names a record the log still
holds, and removing it would cost that object's next write a read of the log
for nothing. Two nodes reading their own wall clocks would delete different
rows, and so would two nodes each deleting below a floor it read itself, which
is why this term is a position carried by a record rather than a duration or a
local delete.

The **authentication trail** — `iam_history`, the identity domain's record of
who did what to whom — is the one table in any domain with **two** horizons,
and a `class` column the engine derives from the operation is what separates
them. A **change** (somebody suspended, an access granted, a person removed) is
an audit asked a year later, and in several jurisdictions one that must be
answerable; a **session** (who signed in on Tuesday) answers an investigation
that is days old, and is a record of a person's working hours for as long as it
is kept. Keeping the second as long as the first would be storing more about
people than there is a reason to.

The trail's sweep is, like the anchors' above, **a record on the log**
rather than a local delete. The rows are identity-claimed, so a node
sweeping on its own clock would hold different bytes from its peers, and "N
byte-identical copies" would quietly become a claim about how synchronised
their clocks were. The record names *positions* the publisher resolved once —
"every change below this one, every session record below that one" — and one
instant, also read once, against which the same record collects the rows that
are **over** rather than old: sessions that ended or passed their absolute
deadline; invitations that expired unredeemed **or were redeemed** (one carrying no expiry at all counts as expired, since no reader treats it as open); and credentials — passwords, second factors, machine tokens — that
were **revoked or passed their own expiry**. Each is kept for a week (168
hours) after it stopped being presentable, so the sessions and credentials
screens can still say what ended and why; after that the trail row, kept for
the change or session horizon, is the durable account of it. It runs
**per bucket**: the estate is divided into 64 partitions by a hash of the
person's id, so one horizon's worth of deletions is 64 bounded transactions of
at most 4 000 rows each rather than one unbounded one. Two nodes whose clocks
disagree delete exactly the same rows, because neither reads its own clock.

A credential is collected from **two** places at once. The person's own row
carries their credentials whole, and the rows the credentials screen reads are
derived from it — so a sweep that deleted only those rows would be undone by the
next change to that person's credentials, which republishes the whole set. The
sweep rewrites the person's row without the collected credentials in the same
transaction. How somebody joined outlives the redeemed invitation too: the
redemption is on the change trail, kept for its own horizon.

The identity domain's **operation ledger** keeps the 30-day horizon every
other domain's does, and one hour for its session subjects, which is not a
contradiction with the two above: a ledger answers "did my write land" and is
measured against the longest a client will retry, where a trail answers "what
happened" and is measured against an audit obligation. It is swept like every
other domain's — per node, by the maintenance sweep — because each node owns
its own copy of it. The hourly sweep of the session subjects records how far
back it has lost **that kind** of operation on its own, beside the watermark the
month's sweep keeps for the whole ledger, so a session operation retried after
the hour is answered `unknown` while every other identity operation is still
vouched for over the month (see
[Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted)).

### A removed person's values stay on the identity log until the trim

A removal erases every sealed value of the person's from every node's rows in
the transaction that removes them — their name, their address and any second
factor's seed, from their own rows, from an invitation addressed to them and
from the trail rows that outlive them (see [Identity and
Access](../concepts/identity-and-access.md#removing-somebody-erases-what-is-theirs-from-every-nodes-rows)).
What it cannot rewrite is the **log**: the records that wrote those values stay
on `CREWLET_IAM_LOG`, sealed under the fleet keyring, until the trim passes
them — never before `min_age`, and later for as long as any of the six terms
holds the identity log. Every backup taken meanwhile carries them in its stream
snapshot.

So the trim is part of an erasure, and a blocked one is an erasure that has not
finished: the identity log named by an alarm holding its trim — under
`trim_blocked`, or under `backup_age` while the backup term is what holds it,
since that alarm lists the logs it holds and those raise no `trim_blocked` of
their own ([the backup term](#a-company-that-never-backs-up-never-trims)) — is
also the answer to "is that person's name still on disk". The remedy is the
term's own — a backup that never left the host, a pin nobody released — and
never a shorter `min_age` for its own sake, which only moves when a record
*may* go.

### The identity duties

Two things keep the identity estate's retention, and one of them is a fleet
singleton on its own lease. It runs once as soon as a node claims it, and then
on its interval.

| Duty | Interval | What it does |
|---|---|---|
| `iam_sweep` | 1 hour | Resolves `api.auth.audit.changes` (400 days) and `api.auth.audit.sessions` (90 days) to positions and publishes one sweep record for each bucket that is **due** — one holding something at least a day past its horizon. The day of slack is what bounds the log: a bucket is swept at most about once a day, so the sweep adds at most 64 records a day however often it runs. |

The other is the operation ledger's sweep, which runs in the ordinary
maintenance tick on every node.

A node arms the duty only if it runs the `workers` role: every data node applies
the identity log, but a node without `workers` claims no singleton, so it arms none
rather than running loops its roles would refuse on every tick.
`identity_duty_seconds` on [`GET /health`](../reference/api-endpoints.md#the-health-envelope)
names the duty a node armed and its interval — the only way to tell a duty
that is running and finding nothing from one that was never armed.

A person's **inbox** — one row per routed change per recipient — is swept at
the company's `inbox.retention_days`, **365 days** by default and settable
between 30 and 3650, and changed live: the sweep reads it off the running
company at every tick, as the applier does at every batch. It is the company's
setting rather than the tracker's because a person's inbox is every native
domain's notices under one horizon. It is the one horizon here that deletes
something a person reads, and it deletes a *pointer* rather than the thing
pointed at: the history row behind every notice is never swept, so "what was I
told about in 2024" is still a `work_activity` question at any age. A month is
the floor because below it an inbox stops being one — somebody away for four
weeks would come back to nothing.

A change's **notices** — one row per recipient of every record about a
[change](../concepts/code-host.md#who-a-change-wakes): a review asked of
somebody, a change of theirs merged or ready, a mention — keep two horizons.
A person's are that inbox's, `inbox.retention_days`. A seat's are kept **30
days**, the horizon of a turn's own detail in the event log: a seat is woken
through the code log, never from its row, so past the turn the wake started
the row names a delivery nothing can show any more — and seats are most of a
change's audience, so a year of their rows would be most of the table for
nothing. Each node sweeps its own rows of each kind in the ordinary
maintenance tick (`code_notifications`, `code_notifications_agents`): what a
node holds is its own applier's — the rows a record writes depend on when the
node applied it and the horizon its company stated — so no two nodes need to
agree, and none is deleted by another. The applier writes no notice older
than its recipient's horizon when it applies the record, so a node that
replays the code log from far back rebuilds none the sweep would delete; and
a repository's [purge](#a-repository) removes its changes' notices with its
changes.

## The storage forecast

For the reference company (100 000 tasks a year, 300 000 comments, 1 000 edits
a day, 3 000 turns a day, 50 projects):

| | Year 1 | Year 3 | Year 5 |
|---|---|---|---|
| replicated store, per node | 9.7 GB | 26.6 GB | 43.8 GB |
| snapshot volume at ×4.2 | 40.8 GB | 111.8 GB | 183.9 GB |
| plus, hosting code at 3 000 pushes a day: the code host's rows, per node | 0.4 GB | 0.4 GB | 0.4 GB |
| and their snapshot volume at ×4.2 | 1.6 GB | 1.6 GB | 1.6 GB |

The code host's line is for a company whose seats push — 3 000 pushes a day,
the rate the code host is built for, every one of them a seat's (a hundred
seats pushing thirty times a day); in this release pushes are people's and
their CI's, and the line scales with them. A seat's push leaves a push row of
about 680 bytes, naming the seat and the turn, work item and model behind it
(a person's is about 623), kept the 181 days of the push-history horizon
(369 MB once the horizon is reached, in the first year), and its pack's row
of about 588, kept while the pack is live. A repository's home git node looks
at it every minute and merges its small packs once sixteen have gathered — a
busy repository's at every look, the minute's pushes taking it past them
([Compaction](../concepts/code-host.md#compaction)) — so a repository holds at
most fifty live pack rows plus a minute's pushes, and a row more for each
0.9 GB its history fills: one pushed now and then sits near the sixteen, and
one pushed three hundred times a minute held 304 at the most, measured —
some 29 KB at fifty and 179 KB at 304. A merged pack's row is kept one more
day, as a retired row of about 608 bytes: at most sixteen for every fifteen
pushes, about 2.0 MB at this rate. So the line stops growing once the push history
reaches its horizon — where it grew by about 644 MB of pack rows a year while nothing
merged a repository's packs — measured in `internal/code`'s "What a push
costs, measured", and set out with the code log's share, a seat's one counter
row and a larger company's figures under
[What every data node holds for the code host](../concepts/scaling.md#git-nodes).
The bytes of the packs are not here: they are the
[object store's](../concepts/object-store.md). Nor is a git node's cache: its
copies of the repositories, the index of which push introduced each commit
and the memo of its seats' read answers — at most a sixteenth of the budget
([The memo](../concepts/code-host.md#the-memo)) — are disposable files inside
`git.cache_max_bytes`, derived again from the rows and the object store, which
no retention keeps and no backup copies.

The mutation log itself, on a healthily-trimming fleet, holds **about
85 MB** — the `min_age` window's worth of records, not a year's: seven days,
`min_age`'s default, of the 12.1 MB a day the reference company writes to it.
`min_age` is the term that binds on a fleet that trims, because the trim
removes only what all six terms permit and every other one sits nearer the
log's head: the backup floor at the newest verified backup, which is at most a
day old (`backup_max_age`) or the trim would be blocked, the snapshot floor
within a day, and the rest where the nodes and the wake feed already are. The
day is 9.9 MB of records and 2.2 MB of read barriers:

| In a day | Each | Of the log |
|---|---|---|
| 274 tasks filed, at a 2 KiB description | 5 266 bytes, in two records | 1.44 MB |
| 822 comments, at 1 KiB | 3 037 bytes | 2.50 MB |
| 1 000 edits, each a status change | 1 157 bytes | 1.16 MB |
| 3 000 turns charged to a task, a summary at its 600-byte cap | 1 608 bytes | 4.82 MB |
| 12 525 read barriers: the seats' 12 500 and the object collector's 25 | 176 bytes | 2.20 MB |

The records are measured on a real broker, written as a seat's tools write
them: a task filed into the team the org chart gives its project, and every
change with the notification the engine attaches, which names the project's
lead and that team's lead for the change to fall back on
(`internal/tracker`'s "What a day costs the log, measured"), each in the signed
frame every state-log record is appended in
([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)): about forty
bytes a record — the frame's own head and a 32-byte MAC around the active
key's id, measured with a two-byte id, so each character more of yours adds a
byte to every record. Their keys are a
new project's, one or two digits, where a project of the reference company's
reaches four in its first year and five by its fifth: three bytes more by then
wherever a key is written — three times in a filed task, once in a comment or
an edit — about 8 kB of the day, under a tenth of one percent. A record
carries a description or a comment whole and its notification's excerpt of it
again, so the text a company writes is part of the figure: with half of it — a
1 KiB description, a 512-byte comment — the records come to 9.1 MB.
`blocked_by` is the field that says whether this fleet is one that trims; if
it is set, the log is growing without bound and the forecast above does not
apply to it.

The read barriers add about **800 MB a year** to the log's throughput — 12 500
a day at 176 bytes, 18 % of what the log takes. That figure is a term in the
log's own size, so it is printed with its derivation rather than assumed: it
comes from an assumed 125 linearizable reads a day per agent seat, which is
12 500 for the reference company's 100 seats, at 176 bytes a barrier — the
code log's `code.BarrierRecordBytes`, which a mutation-log barrier costs too
from the log's second generation to its tenth (168 bytes at its first). On a
fleet that trims they are about 15 MB of the 85. On one whose trim is blocked
the log keeps the whole day, about 4.4 GB a year, so a log created at 16 GiB
fills in nearly four years — nearly five on its records alone — and one
created at 4 GiB in about a year. The [`census_drift`](../reference/alarms.md)
alarm holds every log to that assumption: each log is expected to take 125 reads a day per agent seat (a
company with none is counted as one), plus what the engine reads on its own —
the object store's collector, one in the fleet, pins the log of every domain
whose rows name files, the tracker's, the artifacts log's and the code host's,
25 times a day each
(24 collections and one audit) — and the alarm fires on the log furthest past
twice its census. The code log's census also counts its git clients — people
and their CI, who read no seat's tools — as the barriers that fill a tenth of
the ceiling its stream holds over the whole of `min_age`, at 176 bytes a
barrier in its signed frame (about 87 000 a day at the 1 GiB floor and the
default seven days). A seat's own reads of the code log — a ref resolved for
it, and a read of a repository's content by object id that looks once at the
log's head for an object its home's copy lacks — are counted in its 125 a day,
as every log's are, and are no part of that term; nor are its coding boxes',
which read at their run's floor. The ones a seat's work can repeat at will —
a box's read past its run's floor, a tool's look at the head, an engine
write's second judgement there — are held to that 125 on each git node, 16 at
once and one every 11.5 minutes after, and answered at the seat's floor past
it ([a box's re-syncs](../concepts/code-host.md#a-boxs-re-syncs-and-requests-in-flight)).
That term belongs to the log rather than to the company's current
configuration, so it is counted whether or not `code.native` is set: the log
keeps its ceiling either way, and the rate it is held against is every node's
over the last day — which still holds a day of git reads after the host is
turned off, and the peers' reads while a rolling restart turns it on. A busier
git host resizes the log — `crewlet retention set-capacity CREWLET_CODE_LOG`, as in
[Changing a log's ceiling](#changing-a-logs-ceiling) — and the census moves
with the ceiling the stream then holds; `stream.code_log_max_bytes` sizes only
a code log not yet created, so raising it under one that exists changes neither
the log nor its census.
The rate is the log's, counted where every data node applies it, and each barrier is
counted in the hour it was **committed**: a node back from days away, or
adopting a snapshot a day old, replays days of barriers in minutes, and they
count toward the day they happened in rather than toward today.

Two excursions are designed for. The second never alarms; the first alarms
only on a small volume:

1. **A full re-embedding.** A width change republishes every vector, so for
   the week after it the vector log holds the company's whole corpus — about
   17 MB per agent seat per year of history, 8.46 GB (7.88 GiB) for the
   reference company in its fifth year. What that leaves free depends on the
   ceiling a first boot created the vector log with, every field unset: about
   88 % of the 64 GiB clamp, which a volume of about 626 GiB free or more
   gets; about 61 % of the 20.1 GiB a 200 GiB volume gets; about 38 % of the
   12.7 GiB a 128 GiB volume gets; about 20 % of the 9.9 GiB a 100 GiB volume
   gets; and on a 64 GiB volume, whose 6.1 GiB is less than the corpus, it
   does not fit at all — there the eight logs' asks are scaled down to the
   share they divide, five eighths of what the broker can grant once the
   object store's `nats` bucket has its quarter. The alarm fires within a
   tenth of the ceiling, so **on a volume of less than about 89 GiB free, a
   year-five re-embedding raises `log_headroom`**, and below about 81 GiB the
   log refuses it. The line would be about 73 GiB without the code log's share
   of the same budget
   ([what the code log's reservation costs](replication.md#what-the-code-logs-reservation-costs)).
   Below it, set `stream.tracker_vectors_max_bytes` at about twice the corpus
   — 34 MB per agent seat per year of history — which the log is created with
   whole; or, for a company that hosts no code, set `stream.code_log_max_bytes`
   to its 1 GiB floor, which moves the line to about 76 GiB and leaves a
   100 GiB volume's year-five re-embedding about 34 % free. Either takes effect
   at the first boot; once the streams exist, the same trade is
   [`crewlet retention set-capacity`](#changing-a-logs-ceiling). A larger or
   older company sets `stream.tracker_vectors_max_bytes` the same way,
   whatever its volume.
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
| broker storage | `stream.store_max_bytes` **unset** on a host running one engine; **divided** where several share a filesystem | Unset, the embedded broker takes three quarters of `stream.store_dir`'s free space, measured once at boot — right when it is the only tenant. Free space bounds the *sum* of the engines on a volume, so two that each size themselves from what they can see over-commit it, and the failure is `insufficient storage resources available` — or, on a fleet, `no suitable peers for placement, insufficient storage` — on whichever stream was provisioned last. The engine names the ceiling that did not fit and this field beside either of them, and adds the limit in force and what is already reserved on the standalone one, where this node can read them. Five eighths of whatever is in force is what the state logs may reserve between them, a log that already exists counting at the ceiling it holds and the derived ceilings of the ones being created sharing what that leaves; the other three eighths are for the streams that reserve nothing and grow — the mailboxes, the event log, the dead-letter stream, the memory changelog and every coordination bucket. |

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
- **[Read consistency](consistency.md)** — what a full log costs, the
  fourteen read refusals and every write refusal.
- **[Backups & restore](backup.md)** — the artefact and the runbook.
- **[CLI reference](../reference/cli.md#crewlet-retention)** — every verb's
  flags and refusals.
- **[Alarms](../reference/alarms.md)** — every condition and its remedy.
- **[Metrics](../reference/metrics.md)** — the instruments a dashboard is built
  from.
