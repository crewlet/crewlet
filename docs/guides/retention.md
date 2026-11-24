# Retention

A company's history has two shapes. The **durable tables** on every node are
the record of truth: every task, page, comment and turn, for ever. The **log**
in front of them is a replay window, how far back a node that has been away can
catch up from.

This page is about the log. It grows, it has a ceiling, and the trim deletes
from it; deleting the wrong record loses history no backup covers, so most of
this page is about why the trim sometimes will not run.

## The one number to watch

```
crewlet retention status
```

**Every `crewlet retention` command that talks to a node needs
`admin`** (all but `verify --restore`, which opens a snapshot directory on
disk), reads included: the `retention` question and every `/worklet/retention*`
route take it, because the map of which machine holds what and the value a
reanchor must echo describe the deployment, not the company. Commands
authenticate with `CREWLET_API_TOKEN`; a credential without the grant is
refused `403` naming it.

Five figures per domain, and no others:

| Figure | What it is |
|---|---|
| `bytes` | what the log holds now |
| `max_bytes` | the ceiling the **broker** is enforcing |
| `reserve_bytes` | on every log that claims identity — the tracker's, the knowledge base's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, chat's, the executor log's and Gitlet's — the top of that ceiling kept for gate records; see [the gate reserve](#the-gate-reserve) |
| `headroom_fraction` | the unused part of what **ordinary writes** are held to: `max_bytes` less `reserve_bytes` |
| `bytes_per_day` | what the log took in over the trailing day (`PER DAY` in the table) |

`max_bytes` is read from the stream's own configuration, never from the Tier A
field: Tier A is per node and only the value a stream is created with, so once
the stream exists an edit to the field names a ceiling nothing applies, restart
or not. This is the number you divide by.

### The one rate, and what it is held against

The trim never removes a record younger than `min_age`, so a log's ceiling must
hold `min_age` of its own intake. A smaller ceiling fills and refuses appends
**with every trim term satisfied**: nothing is blocked, `blocked_by` says
nothing, and unblocking a term could not help. That is the one path to a full
log the six terms cannot name; `log_ceiling_short` is its alarm. Each node
measures every log's intake at boot and on every trim tick, compares each log's
`max_bytes` against `min_age` × `bytes_per_day` on every alarm heartbeat (ten
seconds), and fires when the window no longer fits. Its detail says how long the
ceiling holds at that rate; the remedy is a bigger ceiling (see [Changing a log's
ceiling](#changing-a-logs-ceiling)) or a shorter `min_age`.

It matters most on the **identity log**, which grows with sign-ins and whose
ceiling is not derived from the disk.

The rate is **measured from the log itself**: the records at or after now − 24 h
at the log's average record size. Every node derives the same number; a
restarted node measures on its first tick.

The rate is absent, and the alarm silent, in three places; absent is not zero:

- **A compacted log** (the vector log and the usage log, one message per
  subject). Its size follows its subject count, and a full re-embedding
  replaces rather than adds, so the alarm would page for a log not growing.
- **A log in its first two days** (a fresh deployment, or one re-anchored under
  a running fleet). The first day is the import day and overstates the settled
  rate, so the measured day must lie wholly after it.
- **A node whose trim has not ticked yet.**

A measured `0` is a log that took in nothing yesterday, which holds any window.

There is **no projected-full date**: the comparison is against the window the
operator set. One day busy enough that a window of such days would not fit (a
login storm on the identity log) raises the alarm while inside the trailing day,
since its first day is indistinguishable from a new steady state; if it was a
step, it clears the day after.

**`crewlet retention status` exits non-zero when any alarm is active** and
prints each one's measurement and remedy on stderr. That is the alarm hook: run
it from your own cron and page on the exit code; the engine owns no notification
channel. The full alarm list is in [Alarms](../reference/alarms.md).

**An alarm is one per kind, however many logs it stands on.** A condition that
is a property of a log (`log_headroom`, `log_ceiling_short`, `backup_age`,
`trim_blocked`, `deferred_old`, `floor_unknown`, `census_drift`) lists every log
it stands on in its `domains`, each with its own measurement; a measurement in
the same words on several logs no term holds is given once, naming them. So four
held logs are one `trim_blocked` on the health count, the gauge and the
dashboard. `crewlet retention status` prints them under the alarm, one
measurement in its headline and several as a line each. The `alarm_raised` and
`alarm_cleared` log lines are still written per log, carrying it as `domain`.

## The six terms

The trim may delete up to the **minimum** of six numbers. Each can only move the
point **down**: each is a floor, never a horizon.

| Term | Permits deleting up to |
|---|---|
| `applied` | the lowest committed position over the **counted set** of nodes |
| `min_hold` | the lowest live pin — a backup copying the log, or a joining node fetching a snapshot and replaying its tail |
| `backup_floor` | what has left the host |
| `snapshot_floor` | the *k*-th highest verified snapshot position over the counted set |
| `feed_ack_floor` | how far **that log's own** wake feed has acknowledged |
| `age_floor` | the newest sequence older than `min_age` |

The **counted set** of a log is every node the trim waits for there: each node
that has reported a position on the log, and each **live data node** even before
its report lands (it counts at position zero, because it applies the log all the
same). Live data nodes are listed afresh from the presence leases on every tick,
not from a watched view, which can be a heartbeat old and miss a node that just
came up. A node evicted from the log stops being counted about a minute after
that record lands.

A node **at boot** is counted by neither until its first position lands, which
it publishes as its logs start, before it takes a presence lease; until then the
position an earlier run left stands for it, or nothing if it never ran. A node
**joining at boot** is in that interval for its whole transfer. Its own
`min_hold` pin keeps the tail it will replay: renewed every fifteen seconds for
the whole transfer (the trim reads past a pin it has watched go a minute
unrenewed, at its second look at it), and kept after the install until its
published position counts that tail. A backup's pin is renewed the same way
while the copy runs.

**The estate's logs are trimmed by one node**: the holder of the trim duty,
`worker:retention`. Only a data node may claim it, since the trim reads the
estate it holds, and not one whose copy of any log that claims identity or
writes anchors is out of service (every log but the vector changelog and the
usage log, since the evictions and arbitration anchors it counts are in those
rows). Such a node declines the duty (`retention_duty_declined`) and gives its
lease back if it holds it (`retention_duty_given_back`); one whose copies serve
takes it at its next tick
([ADR-0079](https://github.com/crewlet/crewlet/blob/main/adr/0079-a-domains-fault-takes-only-its-own-copy-out-of-service.md)).
A lapsed 45-minute lease would leave every log untrimmed for up to an hour. Every node still evaluates its own alarms on every tick, whatever duty
it holds.

`feed_ack_floor` is the acknowledgement floor of the durable consumer a log's
own change feed opens: the feed's **group** `crewlet-tracker-feed` on
`CREWLET_TRACKER_LOG`, `crewlet-pages-feed` on `CREWLET_PAGES_LOG`,
`crewlet-chat-feed` on `CREWLET_CHAT_LOG`, and `crewlet-code-feed` on
`CREWLET_CODE_LOG`, which wakes the seats a change's records tell
([How a seat is woken](../concepts/gitlet.md#how-a-seat-is-woken)). A record the
feed has not acknowledged is one nobody has been woken for, so a stopped feed
holds back its own log only, and `blocked_by: feed_ack_floor` on a domain names
that domain's feed. A record no node could read yet (written by a newer build
during a roll) is handed back, not acknowledged, so it holds its log's floor too,
until its first delivery after it is an hour old: the feed then keeps it whole
as a dead letter and acknowledges it
([Dead letters](../concepts/event-system.md#dead-letters)), and the log can trim
it.

A feed whose consumer fails (a missed broker heartbeat, a slept host, a consumer
the broker refuses) reopens its group on its own, waiting one second and
doubling up to thirty between attempts, and is handed every unacknowledged
record, so wakes are late, not lost. Each failure is a `changefeed_interrupted`
or `changefeed_open_failed` line, and the return a `changefeed_reopened` line
carrying how long it was down. A feed with no consumer for longer than the
one-minute stall grace raises [`feed_stopped`](../reference/alarms.md) on its
node until it holds one again.

A group name is not the broker's consumer name. The durable consumer is named
`<group>__<stream>__<digest>`, for example
`crewlet-pages-feed__CREWLET_PAGES_LOG__e5825de86642`, so `nats consumer ls <stream>` finds it (on an external cluster, or through an embedded fleet's member
with the
[operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool));
a lookup by group name alone finds nothing.

A term that **could not be read** blocks, exactly as one that permits nothing
does; treating it as satisfied is how a trim passes a node that could not
report. A term this domain does not **have** is `n/a`, not zero: the vector log,
the identity estate's, the artifacts log's, the artifact datastore's, Ask
Crewlet's, the executor log's and the usage log's have no wake feed. A term that
was read and **binds nothing** (no hold pins the log, or a solo fleet takes no
snapshots) reads `unbounded`: internally the largest value, the identity for the
minimum, chosen to lose a comparison and not a position, so the screen and API
say the word.

### A blocked trim is ordinary until it has kept something

Every fresh deployment's trim is blocked: on its first backup, on its first
snapshot donors, and for as long as an unwritten log stays empty (every node
sits at position zero). `crewlet retention status` shows the block and its term
from the first tick. The node holding the trim logs it once, both at `WARN`:
`retention_trim_blocked` when a block starts or its term changes, and
`retention_trim_unblocked` when the trim advances again, carrying how long it
was blocked. The **`trim_blocked` alarm** is quieter, and fires only when both
hold:

- the trim has been blocked longer than `min_age` **plus one trim tick**
  (fifteen minutes). `min_age` is what the configuration already holds every
  sanctioned block under: a joining node inside its rejoin window (whose 24-hour
  ceiling is `min_age`'s floor), a young fleet waiting for donors (the
  [cross-field rule](#the-cross-field-rule) keeps the snapshot cadence below it),
  a first backup (which `backup_age` raises on its own: at once where none has
  been taken, at the policy's age where one has gone stale). The tick is the
  block's resolution: `blocked_since` moves only when the trim runs, so a block
  that ended is seen to end up to a tick later;
- **and** the log holds records older than `min_age`, which the age term would
  release and the blocked trim is keeping. This is read from the log (the age
  term's sequence against the log's first), so a young or empty log stays silent
  however long the block lasts.

Its detail names the log, the blocking term and up to how many records it keeps:
exactly that many on a strict log, at most that many on a compacted one (the
vector log or the usage log), whose superseded records are already gone. The
term's row in `crewlet retention status` says what it has and wants.

### `min_age` is a floor on trimming, and therefore a *lower bound* on retention

Raising `min_age` can only move the trim point **down**. It cannot shorten how
long the log keeps a record, and says nothing about any node's own database
file.

Default 7 days, range 24 h to 90 d. The 24-hour floor has three reasons: a value
below a day cannot outlast a nightly backup cycle; it keeps a quiet object
writable, because an object whose last record has been trimmed must be
recognised as *trimmed* rather than *absent*, and its arbitration anchor is then
removed, see
[Anchors below the log are removed by a record](#anchors-below-the-log-are-removed-by-a-record);
and it bounds the window a joining node's vector gap is refilled from.

### A company that never backs up never trims

The log is the only copy of what no node has applied yet, and trimming past the
newest backup deletes the last thing that could rebuild it. So `backup_floor` refuses until a backup exists and is
younger than `backup_max_age` (default 24 h).

`crewlet retention status` says so in prose, before any table:

```
Nothing is being trimmed on tracker: the newest backup is 72h0m0s old, past the 24h0m0s this deployment accepts.
```

Two policies decide which copies count:

- **`backup_floor: engine`** (default): the copies this fleet's own nodes wrote
  and verified. `crewlet backup` announces each one to the fleet when its
  manifest is written.
- **`backup_floor: operator`**: an explicit acknowledgement, for a company whose
  policy is "trim only what is off-site". The engine cannot see that a copy left
  the host, so a person says so:

  ```
  crewlet retention ack -stream CREWLET_TRACKER_LOG -position 918100000 \
    -taken-at 2026-11-17T03:00:00Z
  ```

  `-taken-at` is when the copy was taken, by the node's clock: the backup's age
  is measured from it, so a late acknowledgement still ages the copy.

  An acknowledgement names ONE stream and is kept per stream; acknowledging the
  next log leaves the last one standing. Every log is trimmed only as far as an
  acknowledgement of that log says, so give one per log. A log never
  acknowledged is never trimmed: the ordered logs, `CREWLET_TRACKER_LOG`,
  `CREWLET_PAGES_LOG`, `CREWLET_IAM_LOG`, `CREWLET_ARTIFACTS_LOG`,
  `CREWLET_ARTIFACT_DATA_LOG`, `CREWLET_ASSISTANT_LOG`, `CREWLET_CHAT_LOG`,
  `CREWLET_EXECUTOR_LOG`, `CREWLET_CODE_LOG`, `CREWLET_ACCESS_LOG`,
  `CREWLET_ATLAS_LOG` and `CREWLET_ADVISOR_LOG`, each grow until their own
  ceiling refuses writes. A full identity log refuses every sign-in, a full Ask
  Crewlet log every question, a full chat log every message, a full executor log
  every session a turn would keep for its conversation's next, a full code log
  every push and every record of a change, a full access log every SSH command
  and every change of access a seat would be told, a full atlas log every edit to
  the atlas, and a full advisor log every finding the Advisor would record and
  every decision a person makes on one. The two compacted logs,
  `CREWLET_TRACKER_VECTORS` and `CREWLET_USAGE_LOG`, age their records out on the
  broker whatever the trim decides; their trim and alarms wait on an
  acknowledgement just the same. `crewlet validate` warns about this policy on
  every node that holds data (the nodes that run the trim), as each of them does
  on every start in a `config_advisory` line. A node without `data` runs no trim
  and reads none of `stream.tracker_retention`, so it is not warned.

Each log's trim reads **the newest counted backup that covers that log**: under
`engine`, a node's copy that leaves a log out does not stand in for it, and an
older copy of another node's that covers it does. The **backup-age alarm**
(`backup_age`) ages the same point per log under the same policy: under
`operator` a node's nightly copies are fresh while each trim waits on its
acknowledgement, so the alarm fires on each log's own missing or stale
acknowledgement. It is one alarm naming every such log, leading with the one
whose backup is missing or oldest. A log it names whose trim the backup term has
held longer than that log's `min_age` replay window plus a trim tick, while the
log keeps records older than the window (exactly a log that would otherwise
raise `trim_blocked`, [above](#a-blocked-trim-is-ordinary-until-it-has-kept-something)),
raises no `trim_blocked` of its own: its entry under `backup_age` carries the
term, the instant the trim stopped and what the hold is keeping, because the
backup is the one thing to do. A younger hold, or one that keeps nothing past the
window, is listed under no alarm; `crewlet retention status` still shows its
block from the first tick. A log the backup term holds while its own backup is
inside the policy (a copy taken on an earlier generation of the log) stays under
`trim_blocked`. The retention report carries each log's point as that domain's
`backup_at`, and **Settings › Backups & retention** marks every point some log's
trim reads **counted · newest**.

An **unverified** copy satisfies neither: a file that exists and was never
opened is not a backup.

### `retention.backup_owner`

Set it.

```yaml
retention:
  backup_owner: platform-oncall
```

Free text: a person, a team, a scheduler's name. The engine does not run your
backups (`crewlet backup` is a node-client command because the coordination
broker binds no socket), but the fleet can name who owns them in four places: `crewlet
validate` warns whenever it is unset (under either `backup_floor`), and
so does every start of every node in a `config_advisory` line; `crewlet retention
status` names it in the backup term's remedy; the retention answer
renders it; and **`crewlet backup`'s manifest records it**, which makes it
durable evidence.

## The trim floor

`crewlet retention status` prints a **trim floor** per domain. Everything below
it may already be deleted from the log, so a node that has applied every record
up to the one just before it holds everything that may be missing. A node
further behind replays what the log still holds, and adopts a snapshot only when
what it lacks is gone from the log. Two properties make the floor reliable:

- **It is published before the purge it licenses.** The trim writes its
  conclusion to the fleet first and deletes second. A tick whose floor could not
  be published deletes nothing. A tick that published and then failed to delete
  leaves the floor ahead of the log until a later tick licenses deleting at
  least that far (possibly a while, if blocked or the lowest counted node is
  lower). That is the safe direction: the records between the log's first and
  the floor are still there, so a node below the floor and above the log reports
  `behind` while it replays them and needs no snapshot.
- **It never moves down within a generation.** A tick that is blocked, or whose
  lowest counted node is lower than last time (a readmitted node, one restored
  from an old backup, one that came up on its own history because no peer could
  donate) leaves the floor where it was. Deleted records do not come back.

What the last tick itself concluded is `trim_to` on the retention answer: zero
while the trim is blocked, and below the floor whenever the lowest counted node
is. The dashboard's retention screen shows it beside the floor when they differ.

**A floor belongs to one generation of its log.** Right after a
[reanchor](#re-anchoring-a-recreated-or-restored-log) the published floor, its
terms and its blocking term describe the stream that was left, so the status
shows that domain with **no conclusion yet** until the trim's first tick on the
adopted stream, as the write fence and readiness do. `crewlet retention
status` prints `-` for its `TRIM FLOOR` and says under its watermarks that the trim has
concluded nothing about that generation yet; the dashboard tags it *no
conclusion yet* rather than *advancing*; and the retention answer carries
`trim_floor_state: none_at_generation` with an empty `terms` list. A domain whose
floor nobody could read is shown as `unreadable`, never as an empty one, and
other domains' rows are unaffected; see
[An unreadable trim floor](#an-unreadable-trim-floor).

The floor is load-bearing for **writes**, not only recovery. A write to an
object whose last record was deleted cannot compare against it, so it is retried
as "this object's history on the log is empty", which is only true on a node that
has applied everything that may be gone. So every such write first checks,
freshly, that the rows it decided from (as they stood when it decided) hold every
record up to the one just before the **higher of the floor and the log's own
first record**; a write that does not is refused `unavailable` rather than
allowed to overwrite a change it never saw. Freshly means after the write has
read its object on the log, never from a remembered floor, which can miss the
floor that licensed deleting the object's last record. The read is the write's
own domain's floor alone, by its key (one request to the coordination store's
leader), made at the same time as the read of the log's ends, so a write waits
for the slower of the two. The refusal names its reason, as reads do:

- `behind` when the log still holds every record the node lacks: it is replaying
  up to the floor, or has applied past the state the write decided from. It
  clears on its own; retry on the same node.
- `below_floor` when the node's next record is gone from the log. No replay can
  supply it, so the node [adopts a snapshot](#the-join-runbook); another node can
  make the write meanwhile.
- `floor_unknown` when this domain's floor or the log could not be read. An
  unknown bound refuses rather than guessing, and only for the domain whose
  bound it is.
- `evicted` for a node the fleet has removed.
- `wrong_stream` when the node's own checkpoint is past the log's end (judged on
  where the node's applier stands, not the state the write decided from, because
  what it appends lands relative to the former), or when the log holds, at that
  checkpoint, another record than the one the node consumed there. See
  [Re-anchoring a recreated or restored log](#re-anchoring-a-recreated-or-restored-log).
- `log_truncated` when a **peer's** rows hold records the log lost: a broker
  restored from an older copy, with a peer whose rows are newer. This node's own
  rows are the log's history, so only its writes refuse, until the operator
  decides which history the fleet keeps; an eviction is still written. See the
  same section.

None is a conflict (which tells a caller somebody else is editing and to
re-read); each is about this node, not the object. Reads make the same
comparison: a node below the floor whose missing records the log still holds
refuses them as `behind` while it replays, and one whose missing records are
gone refuses them as `below_floor` and adopts a snapshot.

### An unreadable trim floor

Each domain's floor is a key of its own, `floor.<domain>` in the
`crewlet_statelog_positions` bucket, and every reader asks that key alone. A
floor that cannot be read stops **that domain** only: its reads refuse
`floor_unknown` once the floor has gone unread for four heartbeats (a minute),
its writes at an expectation of zero refuse at once, its trim neither publishes
nor purges, and the retention answer shows its floor `unreadable`; every other
domain reads, writes and trims as before. The `floor_unknown` alarm names each
domain whose reads it refuses (floor unread past the four heartbeats) with how
long it has gone unread; a domain that began failing more recently is not named
yet, since its reads are still served. That age is this node's own observation,
taken on its ten-second alarm heartbeat by the **floor watch** through the read
path's own two steps (each domain's floor read by its key, then held to the
generation its applier is on), from the first failed beat to the latest, so the
alarm names exactly the floors this node's reads refuse on, whichever node holds
the trim, and forgets the age the moment a read succeeds. A floor published at a
generation this node has not reached is as unusable as an unreadable one, and
the alarm says which it was.

There are two ways a floor cannot be read:

- **Coordination does not answer.** Every domain's floor is unreadable at once,
  because every key is in the one bucket. Nothing is wrong with the rows; the
  refusals clear when the store answers again. Check the node's connection to
  the broker, and that the bucket's stream has a leader.
- **The row does not read as its floor.** Its bytes do not decode, or it is
  filed under one domain's key and names another. Only that domain refuses, and
  the refusal (on the trim's `retention_trim_failed` line and on every write it
  refuses) names the key and the bucket, and says it clears once the key is
  removed, not when coordination answers. The remedy is to remove the key; the next
  trim tick publishes the floor afresh from what every node has applied. On an
  external NATS cluster (`stream.type: nats`) that is
  `nats kv purge crewlet_statelog_positions floor.<domain>`, and so it is on an
  embedded fleet, through a clustered member's loopback client listener with the
  [operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool).
  A broker that opens no client listener (a solo member's, or one serving only
  leaves) has no socket an outside tool can reach, and no `crewlet` command
  removes the key. The engine writes neither shape, so something else wrote the
  row, and it stays until a client of the bucket removes it.

### Anchors below the log are removed by a record

Every node keeps, per object, the position of the last record it consumed on
that object's subject: the **arbitration anchor** a new write's expectation is
formed from ([replication](replication.md)). Once the log no longer holds an
object's records, its anchor names a gone record, and can only send the object's
next write to the broker to be refused. Kept, anchors would grow by every object
the log ever carried, on every data node, for the life of the deployment.

So after each tick's purges, on each log that writes anchors (the tracker's, the
knowledge base's, the executor log and the code log) the trim holder counts the
anchors its own copy holds below the log's **first surviving record** and
appends one `trimmed` record per 250 of them, at most 128 a tick. Every node's
applier removes up to 250 anchors per record, lowest first, in the transaction
that applies it: the same rows on every node, because it is a record on the log
rather than a per-node sweep
([ADR-0080](https://github.com/crewlet/crewlet/blob/main/adr/0080-an-anchor-below-the-trim-floor-is-removed-by-a-record.md)).

- **A tick with nothing below appends nothing**, so an idle fleet's log gains no
  record. A tick whose purge was blocked still counts below the log's own first
  record, so a backlog left by the cap of 128 or the time budget, or a failed
  sweep, goes on the following ticks without waiting for another purge.
- **The holder counts only once its own copy has applied the whole log.** It
  reads the log's end and waits for its applier to commit it, up to five seconds
  and never past the interval the tick's sweeps share. A copy that is behind
  (catching up after a snapshot, or with a backlog) would count anchors the
  records already on the log remove, so it appends nothing that tick and says so.
- **One record at a time.** Each record is appended only once the holder's
  applier has committed the one before it (again up to five seconds), so a seat's
  write or a read's barrier behind the sweep on the holder waits for one record's
  apply, never the whole run.
- **Within a time budget.** The holder evaluates its alarms on the same loop that
  sweeps, so a tick's sweeps, counts included, share one alarm interval (ten
  seconds). Every log is counted first, then the interval is shared among the
  logs with anchors below their floor **that tick**, each handed an equal share
  of what is left (unspent time goes to the logs after it). A log with nothing
  below costs the others its count only: one log draining a backlog gets the
  whole ten seconds, three at once a third each. Each stops appending when its
  share is spent, leaving the rest to the next tick. So the holder's alarms are
  at most one interval late, and a data node k times slower than the holder is
  left at most about (k − 1) budgets behind, never more than 128 records.
- **The floor is the log's first surviving record, not the published floor**,
  which can run ahead of the log after a failed purge.
- **What it means for writes:** the next write to such an object finds nothing on
  the log and passes the same zero fence as before, one refused append cheaper.
- **What it costs:** a record is a few hundred bytes on the log and is trimmed
  like any other. Applying one holds each node's writer for 21 ms at the 99th
  percentile with 20,000 anchors on the log, 56 ms with 300,000 and 75 ms with a
  million (measured on a 4-vCPU container's virtio disk;
  `go test ./internal/statelog -run '^$' -bench BenchmarkATrimmedRecordsSweep -benchtime 200x`
  re-measures it on yours). A backlog drains at up to 128 records (32,000
  anchors) per log per tick where records apply quickly. On a large table the
  time budget can bind first: at a million anchors a record's apply and append
  take about 45 ms, so a log sweeping alone still reaches the 128 inside its ten
  seconds (three million anchors a day), while each of three logs sweeping at
  once appends about seventy into its third (1.7 million a day).
- **What to watch:** the duty logs `retention_anchors_swept` (`domain`,
  `generation`, `below`, `records`, and `more_remain` when the cap or time budget
  stopped it with anchors still below) when it appends, and
  `retention_anchor_sweep_failed` (`WARN`) when it could not, its own copy being
  behind the log included; the purge and floor have landed by then, and the next
  tick retries. Each node counts what its applier removed as
  `crewlet.statelog.anchors.swept`. An evicted node appends none.

## The cross-field rule

`snapshot_interval × (SnapshotsKept + 1)` must be **less than** `min_age`, or
every snapshot ages out of the replay window before its successor exists, and a
node adopting one would land below a floor the log can no longer bridge.
`crewlet validate` checks it. With the shipped defaults that is 24 h × 2 = 48 h
against 7 days.
## Snapshots

A node below the log (its next record is deleted) cannot replay. It fetches a peer's snapshot of the replicated estate, verifies it and adopts it wholesale. A node only below the published trim floor, while the log still holds what it lacks, replays instead and needs none of this.

**A snapshot is a copy of the replicated estate's file**, never of the node's own. It is taken on `snapshot_interval`, and judged for free space against the file's size and the write-ahead log the copy holds back while it runs.
Every data node takes and offers its own, whether or not it may write the estate: an evicted machine, back with its files, keeps applying its logs and may hold the only copy a joiner can fetch.

A declined take is retried, not left for an interval:

- A decline a boot settles by itself (`unhydrated`, `sole_node`, `lagging`): retried after a second, doubling to thirty seconds, and the moment each log first catches up.
- Any other decline, or a failed take: every thirty seconds.
- `recent`, once the artefact found has aged past `snapshot_interval`.

A node that stays in one of them (a single node is `sole_node` for good) settles on the thirty-second retry within a minute. The register row naming a new artefact is published as soon as it is taken, not on the position heartbeat's next beat.

The manifest names every log the artefact carries: a joiner refuses one naming a log it does not run, or missing one it does, before a byte moves. Artefacts live in `store.snapshot_dir` itself. The copy is taken with `VACUUM INTO`, as a backup's is, so it needs a writable temporary directory while it runs (see [Deployment](deployment.md#the-store)).

**Each snapshot is one moment** of the file: every record the node had applied when the take began, nothing applied while the copy ran. The positions its manifest names are read from that copy.

**A take needs room for more than the copy.** Holding the copy at one moment stops every checkpoint for the copy's whole run, so the live file's `-wal` grows by what the node writes meanwhile, on the volume the applier commits to: its write rate times the copy's run (file size over the rate the copy reads it). Each copy measures how far the log grew under it. A take is declined `insufficient_space` unless `store.snapshot_dir`'s volume has room for the copy, a tenth of it to spare, and the log the last copy measured scaled to the file as it stands. Where snapshots live on a volume of their own, the replicated estate's volume must also have room for that log. Until a copy has measured it since the node started, a tenth stands for the log, which covers a node writing its log at up to a tenth of the rate the copy reads the file.

```
crewlet retention snapshots
```

Its own verb rather than a block of `status`, because the repository is **per node**: "which of my machines can donate, and how old is what they hold" is a disk question, asked when a join has failed.

**`SnapshotsKept = 1`.** With N donors the fleet is the redundancy (one per data node across at least two), and a recipient that fails a verification asks the next donor. The trim's `snapshot_floor` asks two of a log's counted nodes to hold an artefact; a fleet of one data node is satisfied by construction, its recovery artefact being a backup.

**A donor streams at most four artefacts at once and refuses the rest at once.** Each transfer is a sequential read of one large file with a credit window of chunks in flight, and the fetch subject is a fan-in every joiner reaches, so a fleet restarting together would otherwise spawn one goroutine per joiner on the node holding the newest artefact. The excess is *terminated with a reason*, not queued: a joiner told no asks the next donor immediately, while one waiting on a queue it cannot see spends its whole collection window on a node that was never going to answer. For the same reason a joiner shuffles *equally* good offers before ranking: the best artefact still comes first, but which of several equal donors it asks is its own choice.

**A donor whose connection goes away dials again.** The donor serves over its own connection, which carries the largest messages a node sends, so it is the first a server can close for good (a chunk past a lowered `max_payload`, a credential the server stopped accepting). That stops the donor only: a node whose donor is down still serves its own company, so it does not [stop itself](deployment.md#an-external-nats-server) as it does over connections it cannot run without. The donor logs `statelog_donor_stopped` (`ERROR`, `component=engine`), naming why and when it dials again, and redials after a second, doubling to thirty seconds while refused (the snapshot loop's own retry). A refusal repeated on every redial is said once; a donor that was serving and lost its connection again is said again.

**The manifest names the position the file keeps.** The checkpoint commits with the rows, so the position inside the copy is the only one that describes it. The donor reads it back out of the copy after the scrub, not from its live applier, which has moved on. A recipient verifies the two agree and refuses an artefact where they do not. A domain nobody has written to is at position zero in both, and adoptable.

**On a single node the loop does not run.** It skips with the published reason `sole_node` (the fleet counts no data node but this one), because a daily full copy buys an artefact no peer can fetch. A solo deployment's recovery artefact is `crewlet backup`. Who counts is the estate's counted set, as the trim counts it: every node whose row names one of its logs and every live data node, so a joining node is counted before its first report and the lone data node already there takes the artefact the joiner needs.

Every skip is published on the node's own register row, so a failed join has an answer rather than a silence:

| Reason | What it means |
|---|---|
| `sole_node` | no counted node but this one; nothing to donate to. This node need not be counted itself: one an eviction removed, back with its files, still donates to a joiner beside it. Counted as the trim counts: every node whose row names one of the estate's logs, every live data node (so a joiner with no row yet is somebody to donate to), less nodes evicted longer ago than the fence window. A view of the data nodes this node cannot confirm leaves them out, and the rows alone are counted |
| `lagging` | this node is more than 1 000 records behind |
| `unhydrated` | this node has not established a complete copy of some domain |
| `deferred` | this node holds a record it cannot decode |
| `insufficient_space` | not enough disk in `store.snapshot_dir` for the copy and the write-ahead log it holds back, or on the replicated estate's own volume for that log |
| `ahead_of_log` | this node's checkpoint is past the log's end, so its rows are keyed to a sequence space the stream no longer has |
| `log_diverged` | the log holds, at this node's checkpoint, another record than the one it consumed there (a broker restored from an older copy and written past these rows), so they are a history the log does not continue |
| `recent` | the newest artefact is younger than `snapshot_interval` **and names every domain this node runs, at the generation this node is on**; a young artefact short of a domain is replaced at once (`statelog_snapshot_recent_but_short`), because no joiner can adopt it (see [Running a Fleet](fleet.md#draining-and-rolling-upgrades)). And see below |
| `failed` | the copy was attempted and errored; the engine log carries the error. A temporary directory the node can't write is one cause, and the error names the variable and the directory |

`recent` is the one reason a healthy node reaches, so it is **not** published: a node whose newest artefact is inside the interval can donate, so its row carries the artefact and an empty reason. The column answers "why can this node not donate", and a skip answers that only when no current artefact stands behind it.

A node holding an older artefact it cannot refresh publishes **both**: the position it can still donate from and the reason it stopped refreshing. A skip never erases what is on disk.

**A reanchor or an adoption refreshes it at once.** A joiner asks for an artefact at the generation each domain is on and refuses any other, so an artefact from before a [reanchor](#re-anchoring-a-recreated-or-restored-log) is one nobody can adopt however young. It does not count as `recent`. A completed reanchor or adoption wakes the loop, as does reopening a database a failed adoption left closed (the file it reopens may be the artefact that adoption installed), so the node takes a new artefact as soon as its gate allows, not up to `snapshot_interval` later while every peer the reanchor left behind has nothing to adopt.

### Sizing the snapshot volume

**×4.2 of the replicated store**, on a separate volume. Three states: steady is live + WAL + its own snapshot + one backup copy = ×3.2; **rotation** is that plus the new partial file before the old one is deleted = ×4.2; adopting is ×3.2.

`rotates_out_at` on the report is three-valued: a time, "never" for a fleet that is not rotating, and unknown where the interval cannot be established (a fleet whose snapshot loop is skipping has no next rotation to name).

## The join runbook

A node that reports `below_floor` has lost its next record from the log. (One below the published floor whose missing records the log still holds reports `behind` instead, replays them and needs none of this.) When a node reports `below_floor`:

1. **`crewlet retention snapshots`**: does any peer hold one, and how old?
2. If one does, the node fetches it on its own, at boot or the moment its heartbeat finds it below the log while running: it pauses its appliers, adopts, moves its consumers to the artefact's position and resumes, with no restart.
   - **Hold.** It first takes a **hold** on every domain's log, which pins the trim for the transfer and, once the snapshot is installed, until the node's own published position counts the tail it replays, so a join cannot race the trim that made it necessary. The hold is renewed every fifteen seconds; the trim reads past one it has watched go a minute unrenewed, at its second look at it.
   - **Offers.** It collects offers until every node that can donate has answered, or five seconds at most. That is every live data node (each runs a donor; one holding no artefact answers that it has none) and every node whose positions-register row the coordination store took within the last two hours and forty seconds, by the asking node's clock, and names an artefact. The second half is how a **draining** node is waited for: it gives up its presence as its drain begins, so peers stop counting it for seats, but it serves its donor until it stops, and in a rolling upgrade it is the likeliest holder of the newest artefact. The asking node counts itself only once its own donor is serving, which at boot it is not yet. A named node that never answers (one that stopped within those) costs the five seconds and no more. Liveness and the register are read while offers arrive, so a slow coordination store costs nothing past the five seconds, and one that cannot say leaves the five seconds to decide.
   - **No donor.** A running node that finds none stays as it is, refusing, and asks again on an interval doubling up to five minutes.
   - **Stopped mid-join** (a signal during boot, or a shutdown while asking or fetching): the join is given up at once rather than waiting out the offer window, and never reported as a fleet with nothing to donate; the next start decides afresh.
   - **Losing the node's own database.** A join that loses it is not reported that way either: an install that failed and whose live file could not be reopened, or an artefact installed and then not opened. (A failed install deletes the artefact it fetched before reopening the live file, so the reopen never competes with it for room.) At boot the node stops, naming both failures. While running it logs `statelog_estate_lost` at error level, naming the file: the node answers no tracker, page, usage or search read from its own copy until the file opens; its seats stay, and read the estate from the other data nodes. It reopens the file on its next heartbeat, since reopening asks nobody, but asks the fleet again only on the doubling interval; a join that fetched an artefact before losing the file doubles it like any failed ask, so a failure the transfer itself causes, such as a full disk, never becomes a transfer every heartbeat. If the line repeats, its error names what to fix (the disk, the file's permissions) and the node reopens on the next heartbeat after that. The file it reopens may be the artefact the failed join installed, so the reopen re-keys every domain's applier to it and wakes the snapshot loop, as a completed adoption does. An applier whose log could not be read at that moment judges the file again when it resumes; if it still refuses rows keyed to the stream the broker serves, the next heartbeat notices and a join re-keys it (`statelog_runner_not_rekeyed` says it happened).
   - **A fault of the node's own before the install** (its disk refusing the copy, its store refusing a write, the coordination store refusing the hold) ends the join at that offer rather than fetching every other one to meet it again, and is never logged against a donor. The node logs `statelog_join_unable` at error naming the fault, comes up (or stays up) on the rows it has, and asks again on the doubling interval. Fix what the line names; the next ask then adopts.
   - **After the install** the join is an adoption whatever fails next: a node whose own `statelog_adoption` row could not be advanced logs `statelog_adoption_unrecorded` at error level beside `statelog_adopted`, and serves on the artefact like any adoption.
3. If none does, the skip reason says why. Fix that first: a fleet where every node is `lagging` has an applier problem, not a snapshot problem.
4. If the fleet genuinely holds none (a single node, or every peer skipping), the path is a restore from `crewlet backup`, and the log has to still reach the artefact's position. `crewlet retention verify --restore` tells you in advance that it does.

**A donor on a newer schema is refused before anything moves.** An artefact whose file carries a migration this binary lacks holds rows shaped by code the node does not run. The manifest lists the file's migrations, so the offer is refused from that list rather than fetched whole (the file is still held to its own list when it arrives). During a rolling upgrade that adds a migration, a node still on the older build adopts from a peer still on it, or is upgraded first.

**So is a donor whose rows hold a record this build cannot read.** Each log's manifest entry states the highest record version its rows were *applied* from (kept beside the checkpoint, raised in the transaction that applied each record), and a node whose build reads less refuses it, since it would arrive past a record it could never apply. A build that merely *reads* a newer version does not make its snapshots unadoptable: until something publishes a record at that version the rows hold none, and a node on the older build adopts them.

**An adoption carries the operation ledger with it.** The ledger (the table of which operations have been applied) travels inside the snapshot, so the adopted node answers a retry of anything its donor applied, and files work queued before the join like any other. The point before which the ledger may have lost rows to its thirty-day sweep (for the identity estate's sessions, their hourly one) travels with it, so the joiner answers a retry exactly as its donor would. See [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted).

### A node a peer re-anchored past

When one node [re-anchors](#re-anchoring-a-recreated-or-restored-log) a log that claims identity (the tracker's, the knowledge base's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, chat's, the executor log's or Gitlet's), it opens the next generation from its own rows, and every other node's rows are then a history the log no longer continues from. Nothing on the log can bring them level, so each of those nodes adopts a snapshot from a node in the new generation, **on its own**:

- It learns of the new generation from the re-anchoring node's position row, published the moment the reanchor completes (on its next heartbeat), or sooner from the reanchor's own record arriving on its log, which stops its applier before anything of the new generation is applied.
- Until the adoption lands it refuses that domain's reads and writes as `wrong_stream`, logs `statelog_generation_passed` naming both generations, and takes its copy of that domain out of service (its seats stay, and that domain's operations go to the other data nodes), whether or not its own readings of the log look wrong. On a broker restored from an older copy, a node whose checkpoint was below the restored end sees nothing wrong, which is why the fleet's generation decides it rather than the log.
- It adopts through the join above, asking for an artefact at the new generation (the join logs `statelog_behind_a_reanchor`). Its applier is then re-keyed to the adopted checkpoint and resumes with no restart.
- The re-anchoring node wakes its snapshot loop when the reanchor completes, so it can donate as soon as its gate allows. Until some node in the new generation holds an artefact, the others ask again on the doubling interval.

The vectors and the usage days are exempt, as every log that claims no identity is: each data node re-anchors its own copy, so a peer ahead there is a recovery in progress, not a history this node has lost.

**If the node that re-anchored is gone for good** (decommissioned, or its disk lost, before any peer adopted from it), nothing can donate the new generation: its history was on that one machine. Every other node keeps refusing the domain and asking for a donor, and a reanchor on any of them refuses, naming the peer that "has already re-anchored"; force does not override that, because while the peer might come back its rows are the fleet's history. The remedy is to say it will not come back:

```
crewlet retention evict node-4 -confirm node-4
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
```

1. **Evict the peer**, from any remaining node. The eviction is the one write a node the peer re-anchored past still makes, so it lands even though that node refuses everything else. The command reports the stranded logs as `pending`, because the node's applier is stopped and applies the record only after step 2.
2. From then on an evicted node's position row, and any trim floor it published, counts toward neither the generation the fleet is on nor the reanchor guards. Every data node reads the peer's standing off the log itself (a stranded node's applier never reaches the eviction record), so the nodes stop asking for a donor at the peer's generation.
3. **Re-anchor the most caught-up remaining node.** `reanchor` names the `abandoned` case: the log is the one these rows are keyed to and still holds everything they are missing, but it continues in a generation only the evicted peer held. The reanchor opens the generation **after** the peer's (its number was used, and its record holds that generation's subject) and follows the log from this node's own checkpoint. Every record written in the generation it skips is **void**: consumed, applied into no row, because it was decided from rows nobody holds. Records other nodes wrote in this node's own generation before learning of the peer's move are kept.
4. Every other node then adopts from the re-anchored one, as above.

What the evicted peer applied that nobody else did (the tail it re-anchored from, anything written in its generation) is lost with its disk; the reanchor keeps everything else.

**The refusal says which remedy applies**, and again as that changes. While a live peer holds the new generation, or nothing yet shows none does, it names the adoption and, beside it, the eviction and reanchor above for a peer that is gone. Once the only nodes that held that generation are evicted (the record's writer is, or no un-evicted peer's row or trim floor stands at it) it names the reanchor alone, since no snapshot is left to adopt. Every heartbeat judges this again, so a peer evicted after the refusal began, or readmitted, moves it (a beat that could not read whether the writer is evicted logs `statelog_generation_holder_unread` and leaves it as it was). A record opening the next generation that **this node's own** reanchor appended before it failed (the command gave up, or the node restarted part-way) is refused naming that: nothing is adopted, and running the same `reanchor` again completes it from that record.

**`rejoin_window`** (default 30 m) is your budget for a node to become a complete replica. `crewlet retention status` prints this node's store size, the projected join against a conservative profile, and the window, so a fleet whose store has outgrown its window says so before an incident does.

It is also a term of every data node's **startup probe** in a fleet of more than one. A join at boot runs inside the node's recovery, before `/health` turns `200`, so a probe that allows the store's recovery and not the window kills a joining node partway through its transfer, and the next boot restarts it from the first byte. Size the probe as [Sizing the startup probe](deployment.md#sizing-the-startup-probe) says, and raise it with the window. A lone data node never transfers, so its probe needs no such term. While a join runs, `statelog_join_progress` every ten seconds says how far it has got and at what rate, which checks a window against this fleet's own disks and network rather than the projection.

## Eviction

An absent node pins the `applied` term for ever: its position never advances, so nothing above it can be deleted. That is deliberate (a node coming back needs those records); eviction is the operator's gesture for a node that is not.

```
crewlet retention evict node-4 -confirm node-4
```

It also stops an absent node's position counting anywhere else: an evicted node's row, and any trim floor it published, counts toward neither the generation the fleet is on nor a reanchor's guards, which releases a fleet stranded by a node that re-anchored and then vanished ([a node a peer re-anchored past](#a-node-a-peer-re-anchored-past)). The row itself stays for the life of the deployment: a readmission is judged by it, and a node id can be readmitted whenever it is evicted. The register therefore holds one row per node id ever configured (a machine rebuilt under its old id writes over its old row; only an id retired for good leaves one behind). The node's **backup** does not stay: once the eviction has taken effect on every log that counts nodes, the trim's next tick forgets the node's newest backup point, so under `backup_floor: engine` no log is trimmed against a copy on a host the fleet has let go, and [`GET /backups`](../reference/api-endpoints.md#get-backups) stops listing it. A log that copy alone covered blocks on the `backup_floor` term, and raises `backup_age`, until a live node backs up; a node readmitted later records its next backup afresh.

The trim counts nodes **per log**, so an eviction is a record on every log it counts nodes on: every log that claims identity (the tracker's, the knowledge base's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, chat's, the executor log's and Gitlet's). The vector log and the usage log count nothing and get none: a node behind on the vectors is a coverage figure, and an evicted node's usage is still what its seats did. Every node running the state log can make the gesture, whichever backends the company uses: every data node runs every one of those logs, even a company on an external tracker or a vendor's chat.

**The node you run the gesture on writes every log itself**: every data node holds the whole estate, so one gesture on any data node reaches every log. A readmission is **judged** on every log before any is written, against the bound this node's own write fence holds a node to (the published floor and the log's first surviving sequence). An input it cannot read (the positions register, a floor, a log's first sequence) refuses the readmission rather than passing it unjudged: `readmission_unjudged`, with nothing written. The same gesture, once the read answers or through another node, finishes it.

It is **judged once, before anything is written**: a node that still holds a live presence lease is refused, because it is still reaching the fleet and almost certainly running; an eviction would drop everything it writes and take its copies of the logs it is evicted from out of service. Stop it and wait for its `LIVE` column in `crewlet retention status` to read `no`; `-force` overrides the refusal for a node wedged in a way that still renews its lease. A refusal writes nothing to any log. If this node cannot read the presence leases at all (a coordination fault), the eviction is refused as one nobody could judge, and says so; `-force` takes it past that too, since the leases are all the judgement reads, and the node's log records that the eviction was forced unjudged. A node id no node could run under (the [`node.id`](../concepts/configuration.md#nodeid) rule: alphanumeric first, then letters, digits, `.`, `_` or `-`, at most 64 characters) is refused before anything is judged.

Past the judgement the record goes to every log at once, and each answers on its own line (the tracker's log, the pages log, the identity estate's, the artifacts log, the artifact datastore's, Ask Crewlet's, chat's, the executor log and the code log) with the [three-valued outcome](replication.md#a-write-has-three-outcomes) every write has (`applied`, `pending` or `unknown`) or `not written` with the reason that stopped that log. A log that answered holds its record whatever the others did, and the gesture runs to its end **whatever happens to the command**: once the first record is about to be written the node finishes under its own budget (thirty seconds for the logs), so a dropped connection or a client timeout does not leave the node evicted on one log and counted on another.

When **not every log holds it**, the command exits non-zero and under each unfinished log prints what to do. Where running the gesture again can finish that log (an `unknown` outcome, a node still catching up, a lost race) it prints the command: the same one with `-op-id <id>` (and `-force` if the first run had it). Each log's record is published under an id derived from the gesture's, its sign, the log and the node, and each log's snapshot reads that id's [ledger row](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted) before anything is decided, judging it against the node's own row in the same transaction: a log whose record is already the gate in force answers at the position it already has, and only a missing log is written, even through a node whose applier had not reached the first record yet, and however long after the first run the retry comes. A fresh id would be a second gesture rather than this one finished. The command mints the id **before** it asks, in the engine's own grammar carrying its instant (the node refuses an id that carries none), so a gesture the node never answered (connection dropped, wait ran out, or what came back was not the node's own answer: a proxy's 504 page, any status with no engine error code, a 200 cut off part way) still prints the `-op-id` that finishes it.

Where running it again **cannot** finish a log straight away, it says what has to happen first, and in every case but two the gesture survives that remedy, so it prints the same `-op-id` to finish it with afterwards. A fresh id there would be a second gesture: every log already holding the first one's record would be written again, its eviction re-dated and its fence window restarted, and the first id would answer `superseded` to anyone finishing it.

- `log_full`: that log is full to its broker ceiling, past even the [reserve kept for gate records](#the-gate-reserve), so no retry makes room: raise its ceiling with [`crewlet retention set-capacity`](#changing-a-logs-ceiling), then run the gesture again with the same `-op-id`. A log full only for ordinary writes does not refuse an eviction at all.
- `evicted`: the node you ran it on is itself evicted and writes nothing: run the gesture, under the same `-op-id`, through a node the fleet still counts (`-api`). Where the hint says the **gesture's record** on that log landed and applies nowhere, the log holds the operation id for its duplicate window (two minutes) from when it landed: the same `-op-id` sent sooner, through any node, is collapsed onto that record and answered `evicted` again, so wait out the window first. A fresh id is not the way round it, for the reason above. Where the hint instead names **another operation's** record (the newest on that log's subject when the gesture's answer was lost, which may be a healthy peer's), the refusal is only about the node you ran it on: run the gesture through another node at once, and if this node's own record did land, that node answers with the copy case below.
- `overtaken` or `abandoned`: the node you ran it on wrote that log's record from rows a reanchor left behind (after a restored reanchor it had not yet learned of, or in a generation a reanchor abandoned), so it landed and applies nowhere. The log holds the operation id for its duplicate window (two minutes) from when it landed: once that has passed, run the gesture, under the same `-op-id`, through a node on the log's current generation (`-api`).
- **Another node's copy**: `evicted`, `overtaken` or `abandoned`, with a hint naming another node as the writer of the record at that position. The `-op-id` you sent had already been written under by that node (you ran the gesture through it first, and by the time its record landed it had been evicted, or it wrote from rows a reanchor left behind), and the node you ran it on this time had its append collapsed onto that node's record, which applies nowhere. The refusal is about that node, not this one, which was counted when it tried: once the duplicate window (two minutes from when the record landed) has passed, run the gesture again with the same `-op-id` through the same node.
- `unknown` that **this node cannot tell**: its operation ledger may have lost the row the operation needs, because the id was minted before the ledger's sweep reached it. The node published nothing and answers the same gesture the same way every time, so it is not offered as a retry: run it, under the same `-op-id`, through a node whose ledger reaches back that far (`-api`). The dashboard says the same and sends you to another node's dashboard.
- `wrong_stream`: the log was rebuilt under this node: [re-anchor it](#re-anchoring-a-recreated-or-restored-log) first, then run the gesture again with the same `-op-id`.

The two where the operation itself is over, and a new gesture is the only way on:

- `superseded`: the operation's record landed and a later gate record on the same node has undone it since (an eviction retried after a readmission took the node back). Run a new gesture, without `-op-id`, if the node should change again.
- `op_reused`: the operation id already names a record on another object: run the gesture without `-op-id`.

What to do about each log comes from the node as an **action** (the same gesture again, a new one, force, another node, a reanchor, a larger ceiling, a wait or a restore; [the full set](../reference/api-endpoints.md#the-three-retention-gestures-that-write)) beside a sentence that names no flag, and the command prints each action as the flags it has. The dashboard renders the same actions as its own controls.

### From the dashboard

**Settings › Backups & retention** offers **Readmit…** on a node row its logs
hold evicted and **Evict…** on every other. The dialog is the same gesture as
the command and answers the same way: one row per log with its outcome and
position (none for `unknown`), or the reason it was not written and what to do.

- The dialog **mints the operation id in the browser before its first
  request**, in the engine's grammar and on the browser's clock, and keeps it
  for the whole gesture. A request that timed out or dropped still holds the
  id: the dialog waits a minute, past the fifty seconds the node takes at most
  to answer (twenty to judge, thirty to write every log), then offers
  **Finish this gesture**, which resends the same request under the same id and
  reads every log's answer. It does the same for an answer the node did not
  write: a reverse proxy's 504 page, any status with no engine error code, or a
  200 cut off part way. Only a refusal carrying the engine's own code is read as
  one.
- Where the same gesture can finish a log (`unknown`, a lost race, a node
  catching up), **Finish** sends it again under the same id, with force carried
  over if the gesture used it.
- Where a log needs something done first, **Finish** stays for once it is done:
  a full log (`log_full`) needs a larger ceiling and a rebuilt one
  (`wrong_stream`) a re-anchor (no control here; each names its command). A node
  that is itself evicted (`evicted`) cannot write, so finish from the dashboard
  of a node the fleet still counts, under the same id: **Finish a gesture
  started elsewhere** takes it and stays on the form until something answers (a
  refused id can be corrected and is not taken up as the gesture's own). The
  same goes for an `unknown` this node cannot tell about. Always finish under
  the gesture's own id; a fresh one would rewrite every log that already holds
  the record and re-date each eviction.
- Only an operation no request can finish (`superseded`, `op_reused`) offers
  **Start a new gesture** instead of Finish.
- Closing the dialog does not lose the gesture: reopening it for the same node
  and sign offers **Finish**, and a node whose last eviction did not reach every
  log reads **Finish eviction…** on its row. A gesture that **did** reach every
  log is kept until the report shows it: the screen re-asks the report as soon
  as the answer arrives, and meanwhile the row reads **Eviction sent…** (or
  **Readmission sent…**) and reopens that answer, never a fresh gesture that
  would write a second record on every log. It is let go once the node serving
  the report has applied every record the gesture wrote and the report still
  disagrees (say, somebody readmitted the node from the command line); that is
  a later change, and the row offers the gesture the node's current state calls
  for. Not while that node could not read its evictions: such a log shows every
  node as not evicted (the report marks its row `evictions_unreadable`, and the
  screen says so above the node block), so the gesture stays until a report
  whose evictions were read.
- A refused **Finish** (it re-runs the judgement, so an unreadable lease
  listing or a node still catching up can refuse it) wrote nothing. The
  gesture's earlier answer stays on screen beside the refusal, with its
  operation id, **Finish**, and **Force eviction** where the refusal offers it.
  A Finish nobody answered keeps that answer below its **No answer** notice.
- A node holding a live presence lease is marked **live** beside **Evict…**, and
  its eviction is refused. That refusal, and the one for an unreadable lease
  listing, offers **Force eviction** behind a second typed confirmation; it
  sends `force=true` and carries it into every Finish of that gesture.

It prints the watermark before and after, and the instant the eviction takes
effect. **The evicted node stays counted for about a minute** on each log after
that log's record lands, so a live node has read its own tombstone before the
trim passes it. `crewlet retention status` shows the node as evicted only once
every log holds its tombstone, and dates it from the latest.

The worst case for that window is **zero**: a node three heartbeats late reads
its tombstone exactly when the trim may pass it. So the window is a
convenience. The safety property is each log's applier's **eviction gate**: it
drops an evicted node's records on that log wherever they land, depends only on
the log's own order, and holds when coordination cannot be reached.

Readmission is the inverse commit on every one of those logs, not a delete, so
the whole history survives a replay:

```
crewlet retention readmit node-4 -confirm node-4
```

It is **refused while the node is below a trim floor**, and the refusal prints
its position beside the floor. Readmitting a node the floor has passed
(typically a machine still switched off) makes the trim count it again, putting
back the pin the eviction lifted while the node is missing records the log has
lost or may lose.

The comparison is the one the node's own [write fence](#the-trim-floor) makes,
made once for every log before any is written: the node's last published
position in each log against the higher of that log's published floor and its
first surviving sequence. A node that never published a position is judged as
holding nothing. A refusal writes nothing, and the node needs only time: start
it if it is not running, and it [catches up on its own](#the-join-runbook),
replaying what the log holds and adopting a snapshot where it does not. Readmit
once it has applied every record up to the one just before that bound: in
`crewlet retention status`, its `SEQ` for each domain has reached one less than
the higher of the domain's `TRIM FLOOR` and `FIRST`. The refusal's `floor`,
`first_seq` and `generation` are the numbers it compared. Right after a
[re-anchor](#re-anchoring-a-recreated-or-restored-log) the domain shows `-` in
`TRIM FLOOR` until the trim's first tick on the adopted stream, and the bound is
`FIRST` alone. Rerunning a readmission is always safe. The node's `GEN` column
says whether its position is at the log's current generation: a position from a
generation the log has left prints `left gen N` in place of a lag, because its
sequence is in a number space that no longer exists, and a readmission is
refused on it whatever it reads. A readmission that reaches some logs and not
others is finished like an eviction, with `-op-id`.

The refusal is not what keeps the data safe, and is not an emergency: a node
below the floor is refused every write that assumes a history it does not hold,
by its own fence, readmitted or not.

## A full log refuses; it does not shed

At its ceiling the log **refuses appends**, naming `log_full`, the ceiling, and
the verb that raises it. It never silently deletes old records to make room:
shedding a record no node has applied is the loss the gate exists to prevent.

A record larger than its log takes is a different refusal, `record_too_large`,
on a log with room to spare; no ceiling or trim changes it. Every log declares
the largest record it publishes, and a record past it is refused before it is
sent:

- 8 MiB less 4 KiB on the tracker's, the knowledge base's, Gitlet's, the vector
  changelog's and the usage log's, the largest the embedded broker's 8 MiB
  messages carry once a record is signed
- 128 KiB on the identity estate's
- 2 MiB on the artifacts log's
- 1 MiB plus 64 KiB on the artifact datastore's
- 2 MiB on Ask Crewlet's
- 512 KiB on chat's
- 128 KiB on the executor log's

The stream's own `max_msg_size` is that declaration plus 4 KiB for what a
stored record carries beside it, so the broker holds a peer on another build to
it too. Two more limits can refuse a record, and the detail says which: the file
store's per-record limit, and a NATS server's `max_payload`. The latter is never
that of the server a node started against (a node refuses to start against one
that holds it to less than 8 MiB) but that of one it reconnected to since: a
cluster member configured apart from the others, or a server restarted with a
smaller limit. A limit lowered by a live reload is not told to the client: the
server closes the connection over the record, and the node
[stops itself](deployment.md#an-external-nats-server), unless the client
reconnected without reading why, in which case it reads the lowered limit on
the reconnect and the refusals that follow are this one. See
[Read consistency](consistency.md#a-writes-refusals).

A full log costs `linearizable` reads, because those append a barrier, which
is every seat tool read. `stale` keeps answering, so the dashboard and the read
API are unaffected. See [Read consistency](consistency.md).

The **usage log** refuses the same way, but no read appends to it, so nothing a
seat does is refused; what stops is the replication of each data node's days.
Every data node keeps re-deriving its day and retrying each tick, nothing is
lost from any data node's own event log, and the `log_headroom` alarm names the
domain (`log_headroom on usage`) long before the ceiling. Its size is a count
of node-days, not a rate: one message per (node, day, and seat, schedule,
person, or `visibility: unit` repository somebody read that day) for 181 days.
A log that fills has outgrown its census: raise `stream.usage_log_max_bytes` as
below. A unit repository's day is one message per node that served its reads,
naming every reader in under two hundred bytes each
([Who read it](../concepts/gitlet.md#who-read-it)): ten unit repositories read
every day on three nodes by a dozen readers each are about 13 MB over the 181
days, some 1 % of the default gibibyte.

The **chat log** refuses the same way, a sixteenth short of its ceiling
([its gate reserve](#the-gate-reserve)), and what is refused is a message
somebody typed: every post, reply, DM, edit, delete, reaction and room change
is refused `log_full`, from a seat and a person alike. A seat's chat reads are
`linearizable` and go with them, so a seat woken in a room can neither read the
thread nor answer. A person's message is refused, not lost: the dashboard's
composer keeps it with the engine's sentence and a Retry, and the dashboard's
`stale` reads keep answering. The prune is refused too and would free nothing:
it deletes rows, not log records, so only the trim frees this log. The ceiling
does not follow `stream.tracker_log_max_bytes`, so a fleet sized for a long
blocked trim on the tracker's log reaches this one first: raise
`stream.chat_log_max_bytes` as below, or clear whatever `blocked_by` names.
`log_headroom` names the domain (`log_headroom on chat`) long before the
ceiling.

The refusal carries **no retry hint**: the only thing that frees a byte is a
fifteen-minute gated job, and the log is full because that gate is closed, so a
number would promise what the mechanism does not.

### The gate reserve

A log fills almost always because a trim cannot advance, most often a node that
is gone and still counted, pinning the applied term. The gesture that unpins it,
[`crewlet retention evict`](#eviction), is itself a record on that log, so a log
that refused it like any append could never be emptied.

So on the logs that carry gate records (every log that claims identity: the
tracker's, the knowledge base's, the identity estate's, the artifacts log's,
the artifact datastore's, Ask Crewlet's, chat's, the executor log's and
Gitlet's) **ordinary writes are refused at a soft ceiling** below the broker's.
The top of the ceiling, the **gate reserve** (`reserve_bytes` in `crewlet
retention status`), takes only the records that install or lift a gate: an
eviction and the readmission that inverts it. A log full for ordinary writes
still takes an eviction, and once the evicted node's minute has passed the trim
moves again. It takes it whatever else refuses this node's ordinary writes (a
peer holding what the log lost, `log_truncated` below, or a generation the fleet
moved past), because the eviction is the way out of each. A purge is not a gate
record: it frees nothing until the trim runs, and nothing bounds how many are
run. The node holds that at the write: a record asking for the reserve that is
not a node's eviction or readmission, a purge included, is refused before
anything is sent and passes none of the fences an eviction is excused.

| | the logs that claim identity | vector changelog, usage log |
|---|---|---|
| ordinary writes and barriers refused at | the ceiling less the reserve | the ceiling |
| gate records refused at | the ceiling | — (they carry none) |
| `headroom_fraction` measured against | the ceiling less the reserve | the ceiling |

The reserve holds however the fleet's writes race. Each node reads the log's
usage **after** it starts an append and counts its own appends in flight, so
what can land past the soft ceiling is only what other nodes have in flight at
that instant: at most one of **that log's** largest records each, plus 4 KiB
for what a stored record carries beside it. The reserve is the larger of:

- the **fleet term**: seven other nodes' largest appends plus a mebibyte for
  the gate records, which holds at every ceiling a log may have, down to its
  floor;
- **a sixteenth of the ceiling**, which grows with the ceiling while the fleet
  term does not, so a larger log absorbs a larger fleet.
| log | largest record | floor | reserve at the floor | ordinary writes at the floor |
|---|---|---|---|---|
| tracker, knowledge base, Gitlet | 8 MiB less 4 KiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| artifacts | 2 MiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| artifact datastore | 1 MiB plus 64 KiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| identity estate | 128 KiB | 64 MiB | 4 MiB (a sixteenth) | 60 MiB |
| Ask Crewlet | 2 MiB | 64 MiB | 15 MiB plus 28 KiB (the fleet term) | 49 MiB less 28 KiB |
| chat | 512 KiB | 1 GiB | 64 MiB (a sixteenth) | 960 MiB |
| executor | 128 KiB | 64 MiB | 4 MiB (a sixteenth) | 60 MiB |
| access | 256 KiB | 64 MiB | 4 MiB (a sixteenth) | 60 MiB |
| atlas | 256 KiB | 64 MiB | 4 MiB (a sixteenth) | 60 MiB |
| advisor | 256 KiB | 64 MiB | 4 MiB (a sixteenth) | 60 MiB |

During a rolling upgrade a node on an older build keeps no reserve, so the reserve holds once every node counts its own. If a gate record is refused even there, the refusal points at `crewlet retention set-capacity`, not a retry. The [`log_headroom`](../reference/alarms.md) alarm, at a tenth of the soft ceiling left, exists so that never happens.

## Changing a log's ceiling

A log's Tier A ceiling (`stream.tracker_log_max_bytes`, `stream.tracker_vectors_max_bytes`, `stream.pages_log_max_bytes`, `stream.iam_log_max_bytes`, `stream.usage_log_max_bytes`, `stream.code_log_max_bytes`, `stream.artifacts_log_max_bytes`, `stream.artifact_data_log_max_bytes`, `stream.assistant_log_max_bytes`, `stream.chat_log_max_bytes`, `stream.executor_log_max_bytes`, `stream.access_log_max_bytes`, `stream.atlas_log_max_bytes`, `stream.advisor_log_max_bytes`) is not live: it is the value the log's stream is created with, sized with the other logs inside what the broker can grant (see [Replication](replication.md#how-the-byte-ceilings-are-sized)). Changing it, up or down, is a **fleet-wide maintenance window**, because:

- A resize is decided against usage, which publishers keep moving. The verb decides before the window opens. It refuses a target whose soft ceiling is at or below what the log holds (naming both and the least target that would do), since that ceiling would refuse every append. The soft ceiling is the target less its [gate reserve](#the-gate-reserve) on the logs that claim identity, the whole target on the vector changelog and the usage log. It also refuses a target under the Tier A floor, naming the field: a gibibyte on the tracker's, the knowledge base's, the vector changelog's, Gitlet's, the artifacts log's, the artifact datastore's and chat's; 64 MiB on the identity estate's, the usage log's, Ask Crewlet's and the executor log's. Anything else is fair, including a target under the current ceiling, which gives a reservation back.
- A raise is a reservation, and the broker refuses one it cannot honour. Where the node can read the limit (a lone embedded node, or a NATS account's own JetStream limit on any topology), a raise past it is refused before the window opens, naming what it reserves and what is left. A clustered member cannot read it, so there the broker refuses when the window applies it, and the seal retires the attempt.
- Only the **broker process restarting** retires a request the broker has queued, not a client disconnecting. So an apply with unknown outcome is resolved only by everything restarting.

The window costs **three fleet-wide restarts** on the happy path, two more per retry:

```
# 1. every node
crewlet run -mode maintenance

# 2. from any one of them
crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592

# 3. every node
crewlet run -mode seal

# 4. from any one of them
crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592

# 5. once every log's window is closed, every node, back to service
crewlet run
```

For several logs, run step 2 for each log before the seal restart and step 4 for each after it. Each log has its own window and acknowledgements. A node won't start in service while any window is open, so the closing verb says to restart in service only once none is; otherwise it names each open window and the command that finishes it.

### The three modes

| `-mode` | Starts publishers | May write stream configuration |
|---|---|---|
| *(unset)* — `normal` | yes | no |
| `maintenance` | no | **yes** |
| `seal` | no | **no** |

`seal` cannot write configuration, so a node acknowledging from it proves it is not the process holding the request being retired, which a maintenance-mode acknowledgement could never prove.

A node in either maintenance mode **keeps its presence lease** (`crewlet fleet broker list`, the fleet view and the participant set below read it) but claims **no seat** and hands **no writer** to any surface, data node or not:

- The operator's MCP endpoint serves none of the tracker's, knowledge base's or chat's room tools, reads included.
- Kept, because they append to no state log: knowledge search and answers, the coordination store's gestures (pausing and resuming a seat, steering a running turn, answering a parked run) and, on Chatlet, a person's own read state (`mark_chat_read`).
- Not served: the purge route, a project file's upload and removal, chat's erase, prune and horizon routes. The chart's projects and containers and the repositories `code.repos` declares apply at the next normal boot. A data node answers "not here" to writes a normal-mode stateless node routes to it.
- Reads are served (a project file's download too) at every level but `linearizable`, which appends a barrier and is refused [`maintenance`](consistency.md#the-fifteen-refusals).
- Operator gestures that append a record are refused: eviction and readmission answer `409 not_publishing`, a reanchor `409 reanchor_refused` naming the mode. A window needs none (a participant that won't acknowledge is excluded from the operation, not evicted), and each would move the usage being measured.

A company revision **activated during the window** is applied (the node serves the new epoch's reads and reports `ok`) but starts nothing the mode keeps off: no inbound edge (no webhook or chat message routed to a seat) and no scheduler (a schedule it adds fires from the next normal boot).

The [object store](../concepts/object-store.md)'s **collection and audit do not run** in either mode: a node in a window claims no `object-collector` duty, and each pass starts with a `linearizable` read, which the mode refuses. Nothing is lost: no write lands a file a collection would free, and a due pass runs once a node is back in normal mode. The `code-retention` duty does not purge a [repository](../concepts/gitlet.md#deletion-and-purge) either, since each step is a record on the code log; a due purge waits for the window to close.

### Who has to acknowledge

The seal proves every **broker** process restarted and every **publisher** was admitted. Participants:

- every node the fleet holds a position for;
- every live node whose presence names the **`data`** role, or whose roles cannot be read (leaving out a possible data node could pass a seal it should hold), because a data node publishes;
- every live **broker member**, whatever its roles, because its broker holds queued requests;
- every live node advertising no broker kind this build knows (a newer build's): leaving out a possible member could pass a bad seal, while waiting on a non-member costs only an acknowledgement you can `exclude`;
- every node whose broker the fleet's metadata group hears from (a voter not offline), read from a member: a booting node's broker joins before it acknowledges or claims presence, and can hold a request from then on. A request is not sent while the group can't be read;
- the coordinator itself.

A **leaf** or a client of an external cluster that holds no data is not asked: its broker queues nothing and its seats publish through a data node. Each node's broker kind is on `crewlet fleet broker list`.

Each participant records which process it restarted as it boots into either mode, once for every log. Where a log has a window open, that record **acknowledges** the window and attempt. Where it has none yet (the first restart), it names no window and the baseline still reads it, so the fleet restarts into `maintenance` once before you run the verb. A restart recorded for one log never answers another's window. The seal takes only an acknowledgement made by booting into `seal` while this window's attempt was open.

**A node id gone for good is retired, once, for the whole fleet.** The register keeps a row for every node id ever configured, so a fleet scaled down, or hosts rebuilt under new ids, leaves rows whose nodes will never run again, and each would be asked to acknowledge every window. `crewlet retention maintenance retire -node ID -confirm ID` records your assertion that no process will run under the id again, with who made it. From then on no window counts the id while it runs nothing (no presence, no broker the metadata group hears) and writes no row: the node running again under its id takes part again, so a retirement made in error undoes itself. A running node is refused, as is the node you ask. **In an open window a retirement is judged every time the window is read** (by a status, a request and the seal alike), never written into it: while it stands the window neither counts the participant against its 16 nor waits for its acknowledgement, and `status` prints it as `retired`; the moment the node holds a presence or stores its row again, it is a participant again and the seal waits for it. A node retired before a window's request is not recorded by that request; if it runs again later, the window's next request records it. A retirement is not an [eviction](#eviction): an evicted node may still run and queue a request, nor an [exclusion](#where-it-stands-and-what-is-holding-it), which waives a node in one window until it runs again as a request leaves.

**A window names at most 16 participants** (`coord.MaxMaintenanceParticipants`). Every data node's boot reads each log's window at the read term, so its record is held to a small value (16 KiB), and sixteen participants counted (it records up to 32, those a standing retirement waives included), as many excluded, each named by whoever excluded it, and its journal fill it. Every data node holds the whole estate and every member queues the request, so a fleet runs a handful of them. On a fleet that counts more, the verb opens no window: it names each node counted only for its row (no presence), to retire if it is gone for good, and only where the live member and data nodes are themselves more than 16 says a window cannot serve the fleet. The window's journal keeps its 8 newest requests (`coord.MaxMaintenanceJournal`): the oldest resolved ones leave first and an unresolved one never does, and a retired request's evidence is cut to 256 bytes, its ceiling first.

The set is read when the window opens and **again right before every request is sent**; each read only adds to the last. A window can wait at `opened`, or at `baselined` once its coordinator has stopped, so a node that came up meanwhile is asked too, and a named node stays named until it acknowledges or you exclude it.

The same write records each participant's running process as the request leaves, and the seal takes an acknowledgement only from a later process. A coordinator resuming a baselined window records it again before its request leaves. So a node restarted while the window waited, into `seal` or not, must restart once more before the seal holds. Run the `seal` restart after the verb has answered from `maintenance`, not before.

### What excludes a publisher

On the default embedded topology **nothing outside these processes can reach the broker** (it binds no socket), so a fleet in a maintenance mode is excluded structurally; there is no check.

On `stream.type: nats` the engine does not run the broker and cannot know who else is connected. `set-capacity` refuses there unless you pass `-i-have-excluded-all-publishers`, your assertion. A check that quietly proved nothing would be worse.

A node booting into `normal` mode writes an **admission** before starting anything, then re-reads the operation and withdraws if one is open, refusing to start and naming the mode to restart in: `maintenance` at `opened`, `seal` from `baselined` on. The coordinator writes the operation first, then reads the admissions, and sends no request while any is held. Whichever record lands second has its writer read the first, so either the node finds the window and doesn't start, or the window finds the node and waits.

What the window does depends on whether the admission's process is **running**, which fleet presence says (a running process renews its presence lease in every mode):

- **Running** (holds its node's presence): it may be publishing, so the window is **closed again** as soon as it opens, nothing is sent, and the verb is refused naming the node. Restart it with `-mode maintenance` and rerun the verb. An open window would turn away every node that started in service meanwhile. The same happens when the admissions can't be read.
- **Not running** (nobody holds its presence): it stopped without giving its admission back. The window stays at `opened`, nothing sent, and the first request is refused naming the node, the restart and the exclusion. It turns away nodes starting in service, so the refusal also names the abandonment that closes it.
- **Replaced** (another process of the node holds its presence): the named process is gone, and the window withdraws the admission itself, logging `admission_superseded`. That is a node restarted into maintenance before its crashed process's lease lapsed.

An admission names its process and never expires. A node gives it back when it stops cleanly, after publishers drain. A crashed process never does, so the node's **next boot into a maintenance mode** (every window's first step) withdraws it before starting anything, logging `admission_superseded`. It does so only once presence shows the old process isn't running: its lease lapsed, or another process of the node holds it. A second process under a live node's id leaves the admission and logs `admission_not_superseded`. (A boot in service needs no such step: a data node's own admission replaces the old one, and a node without `data` publishes nothing.) So a node that crashed in service only needs restarting with the window's `-mode`. Restarted within the coordination lease TTL (`coordination.lease_ttl_seconds`) of the crash, its boot still sees the old lease and leaves the admission, and the window withdraws it once the new process holds the presence. A node that will never boot again is **excluded** (below), which withdraws its admission and is the one way past a dead node's.

### Where it stands, and what is holding it

```
crewlet retention maintenance status -stream CREWLET_TRACKER_LOG
```

One page: the operation, phase and attempt; the process each participant was running as the newest request left against what it acknowledged as; any unresolved request and when it was sent; what the next step waits on, with the next commands in full. At `opened` the next step is the baseline, so it names participants with no maintenance-mode restart recorded and any admission stopping the first request; from the baseline on, what the seal waits for. With no window open on a node in a maintenance mode, it names any node still holding an admission.

While an operation is open, both `crewlet retention status` and **Settings › Backups & retention** lead with it: stream, phase, attempt, age, who ran the verb, any admission holding it at `opened`, who is outstanding and, where it needs a person, why in words and who abandoned it. The fleet is held meanwhile (no node starts in service, a restarted node publishes nothing). Only a node in service answers either surface, so neither says nothing is publishing. Several open windows each get their own, oldest first. Outstanding means: at `opened`, no maintenance-mode restart recorded; from baseline to seal, every node the seal waits on (no acknowledgement from `seal`, or still the process running when the request left). Once sealed, nobody is outstanding; the verify and release wait on no node. **Nothing outstanding is not progress**: unless an admission holds the window, it is waiting on its operator, and both surfaces say so instead of printing an empty list.

**A window the node couldn't read is said, never left out.** The read is a coordination read, and its outage is when people ask why the company went quiet. Each log whose window couldn't be read is named as that (the report's `maintenance_unread`, a harness-page line, a screen banner), never shown as having no window; likewise when acknowledgements or admissions couldn't be read, both surfaces say so rather than naming every node or nobody. When which participants are retired couldn't be judged, `maintenance status` says that and leaves out every list that waives them, and a gesture's answer is marked `retired_unread` with a sentence that names nobody to restart; the report counts each as owing what any participant owes, and the node logs `capacity_window_part_unreadable` naming `retirements`. A data node's boot won't start in service while it can't read a log's window. The `maintenance_open` alarm judges such a log on what was last read: a window last read open stays up **as old as it was when that read answered**, its detail saying when and that the window can't be finished or abandoned through this node until it can read again; a log never read with a window raises nothing. So a blip never clears or raises the alarm: a window last read at 59 minutes stays under the hour however long the blip lasts. The node logs `capacity_window_unreadable` with the reason when reads start failing and `capacity_window_readable` when they stop.

Three gestures act on what it shows:

| | |
|---|---|
| `crewlet retention maintenance exclude -stream NAME -node ID -confirm ID` | Your assertion that a participant's **process is stopped** and holds no outstanding request. The only thing that waives an acknowledgement (an eviction does not: that is about whose records apply). It also withdraws the node's admission, so this is how a fleet gets past a node that crashed and will never boot again. Any node can be excluded, participant or not; an excluded node is no longer a participant of that window. Refused for a running node (one holding a live presence lease, one whose broker the metadata group hears, or the node you ask), for a window's last participant once a request has left (its seal then needs that node's restart, or its retirement), past the 16 exclusions a window keeps, and when the window moved while it was being judged. A clustered member whose own JetStream is off (as nats-server leaves one whose store filled or can't be written) can't read the group, and asks another member; with none answering, the gesture and the request refuse rather than guess. **An exclusion lapses** if the node is running, as judged the same way, when the window's next request leaves, the node sending it first: that process can hold the request, so the window records it again (sending nothing until it has acknowledged) and its seal waits for its restart. |
| `crewlet retention maintenance retire -node ID -confirm ID` | Your assertion, for every window to come, that **no process will run under the id again** ([above](#who-has-to-acknowledge)). Refused while the node is running: it holds a live presence lease, or the metadata group hears its broker. |
| `crewlet retention maintenance abandon -stream NAME -confirm OPERATION-ID` | Changes what the operation is trying to reach, **never** the barrier it must cross. The node checks the confirmation against the open operation and refuses one naming another. From `opened` it clears outright, since no request was issued. From anywhere else it still enters the seal: a paused coordinator's request is outstanding regardless. Once the seal holds, `set-capacity` run again closes the window at whatever ceiling the log has then, sending nothing more. |

Each is recorded with who made it: the status prints each excluded node, after the participants, as `excluded by` its author and credential, and an abandoned window's `ABANDONED` line the same way, because an exclusion is the one fact the seal takes on somebody's word.

### Why the target cannot be changed mid-window

A verify compares the broker's ceiling against the operation's target. If the target could move, nobody could tell an unapplied request from a changed mind. Repeating the number in `-confirm` fixes it for the operation's life; to choose another, abandon and open a new one.

If the ceiling reads back as anything but the target right after the request, the verb sends no other request and says what it read. That request may still land, so it stays for the `seal` restart to retire, and the verify checks again.

A mismatch at verify is a **numbered attempt**, not a re-apply: every node is in `seal` mode, which refuses configuration writes. After three attempts the operation reports blocked and waits for you.

**A window that needs a person says why in words.** The operation carries a script code, `blocked` (`abandoned_by_operator`, `capacity_operation_unsealed`, `capacity_fleet_too_large`, `capacity_window_records_too_many` or `capacity_journal_full`), and every surface shows the sentence beside it, `blocked_detail`: what blocked it and what clears it, with every command as the CLI takes it. Who abandoned it is printed beside the sentence with their credential. A test holds each code's sentence.

- An **abandoned** window closes at whatever ceiling the log has, by its seal: abandoning puts a window back at `sealing`, so the sentence reads the acknowledgements, not the phase. If the seal already holds (fleet still in `seal` mode), one `crewlet retention set-capacity <log> <target bytes> -confirm <target bytes>` closes it, no restart. Otherwise it names each node still to restart with `crewlet run -mode seal`.
- A window **out of attempts** gets no further request: after the third failed verify, `set-capacity` is blocked every time. It says what the broker still reports, and that abandoning it (`crewlet retention maintenance abandon -stream <log> -confirm <operation id>`) and then running `set-capacity` from a node in `seal` mode closes it at that ceiling. If the log still needs the target, find out why the broker won't take it before opening a new window. The verb's own refusal says the same.
- A window whose **fleet outgrew it** (`capacity_fleet_too_large`: more than 16 nodes would take part in its next request, counted in `outgrown`) sends nothing more. Retire each node id among them that is gone for good, then run `set-capacity` again; otherwise abandon it. The verb's own refusal names each node counted only for its row. The node logs `capacity_request_blocked` at error.
- A window that **records too many** (`capacity_window_records_too_many`: its next request would record more than 32 participants, counted in `outgrown`, though it counts no more than 16) sends nothing more. A participant stays recorded once a request names it, retired or not, so retiring more ids won't lower it: exclude retired participants (`status` prints them as `retired`) until it records no more than 32, then run `set-capacity` again; otherwise abandon it. Both sentences prescribe exclusions only where the window has room for as many as it takes (16 in all), and abandonment otherwise; the verb's own refusal also names the retired ones.
- A window with **eight unresolved requests** (`capacity_journal_full`) sends no ninth: restart every node with `crewlet run -mode seal` so its seal retires them, then run `set-capacity` again, or abandon it.
- The seal lifts any of these three blocks as it retires the window's requests: the next attempt's request judges the fleet afresh and blocks again only if it still must.
## Re-anchoring a recreated or restored log

If a stream is genuinely recreated (deleted and remade, rebuilt by hand, or provisioned afresh on a broker that lost its store), its sequences restart at 1. Every position the fleet holds (stored version, consumer cursor, arbitration anchor) then names a number space that no longer exists, and nodes refuse to serve, which is correct.

The engine detects this from the stream's **creation instant**, which every applier compares at boot against the instant its checkpoint was committed under. It is the instant the node whose create made the stream stamped into the stream's configuration as the metadata key `crewlet.created_at` (`nats stream info` lists it under the stream's metadata; on an embedded fleet, through a member's client listener with the [operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool)). It is not the broker's own creation time, which a snapshot restore resets; that is read only for a stream with no stamp (one an operator created by hand).

- A stream restored from a snapshot gets back its configuration, stamp included: the **same** stream, holding every record at its own sequence, judged as the restored broker below is.
- A stream the engine provisions again after it was lost is stamped anew: the recreation.
- A stream deleted and remade from a configuration that still carries `crewlet.created_at` (the configuration `nats stream info --json` printed, passed to `nats stream add --config`) is the **same** stream to every node: it is never refused as recreated, only judged by where it ends and what it holds at each node's checkpoint (below). Its re-anchor is the restored case, followed from its end, so records written into it since need `-discard`; as a recreation it would be followed from its first surviving record. To make a deliberate rebuild read as a recreation, remove that key from the configuration before creating the stream.

On a difference the applier **stops**. The log line names both instants and this verb; the node's reads and writes refuse `wrong_stream` with that reason; it takes its copy of that domain out of service (that domain's operations go to a peer, its seats stay); and `crewlet retention status` leads with `NOT READY <domain>: wrong_stream (recreated): …` carrying the same sentence, both instants included (the dashboard shows the same refusal above the domain's block). A checkpoint past the log's end is caught as `wrong_stream` too: a position the log has never reached is a position on another stream.

**A broker restored from an older copy is the case the instant cannot see.** Restoring the broker's store directory from a disk snapshot, restarting the embedded broker's engine on yesterday's volume, or restoring its streams from snapshots (a [`crewlet backup`](backup.md#restoring), or `nats stream restore` against an external cluster) keeps each stream's creation instant, so every identity check passes. What differs is where the log **ends**: below the checkpoint of a node whose own database is newer. Such a node refuses every read and write of that domain with `wrong_stream` from boot, logs `statelog_ahead_of_log` with both numbers, and `crewlet retention status` on that node leads with `NOT READY <domain>: wrong_stream (ahead_of_log): …` naming both. Writes refuse too: whatever it appended would land at a sequence its applier has already passed and will never apply.

That refusal lifts when the end reaches the checkpoint again (an end read from a stream member that has not caught up looks the same, and a refusal nothing could lift would take a healthy node out over one leader election), and the node logs `statelog_ahead_of_log_cleared`. **What decides whether it carries on is the record, not the end.** Every checkpoint names the record it stands on, by the broker's storage instant for it. Before applying anything past its checkpoint, the node reads the record the log now holds at that sequence:

- The same record: the stale member it looked like; the node carries on.
- Another record: the restored log has been written past this node's rows (by a node whose database was not ahead of the copy). The node logs `statelog_log_diverged`, applies nothing past its checkpoint, refuses every read and write of that domain as `wrong_stream`, donates no snapshot of it (`log_diverged`), and says so on its position row (`log_diverged`). Every other node's `crewlet retention status` marks it as `LOG
DIVERGED` on that node's line, and its own leads with `NOT READY <domain>: wrong_stream (log_diverged)`. This does **not** lift on its own: a member that has not caught up answers that it holds no record at a sequence, never another one.

The node finds divergence the same way when it **boots** on a log already written past its rows (its checkpoint is then not past the end; the record is the only sign) and when a broker is restored under it while it runs. Then whatever it held past its checkpoint when it met the restore (a failed fetch, or the log's end read below what it held) goes back to the broker unapplied first (`statelog_apply_unwound`): linearizable reads' barriers reached and not yet committed, records waiting above a hole or for the next transaction. So the record compared is the one its last commit left at the checkpoint, and nothing the old history delivered is applied after the restore. The comparison is for equality, so no clock is ever ordered against another.

A checkpoint a reanchor placed where the log held no record (below a rebuilt stream's first record, or at a skipped sequence) names nothing to compare: the node carries on, and its first committed batch names its own record. Past the log's end it is treated like any other checkpoint there. It cannot show a broker restored afterwards from a copy that put a record back at its sequence; that goes unnoticed until the next batch names a record, so after restoring a broker behind a node that has not committed since its reanchor, re-anchor or replace it rather than waiting.

**The rest of the fleet stops writing to it too.** A node whose rows were the copy's age sees a log that is its own history, yet its writes after the restore would be applied nowhere by a newer node's reanchor, though acknowledged. So every data node reads the positions register on its heartbeat. A peer on the same stream, in the same generation and not evicted, whose position is past the log's end (confirmed against the log, which must not hold the record there) or whose row says `log_diverged`, stops this node's **writes** of that domain: each refuses `log_truncated`, naming the peer, and the node logs `statelog_log_truncated`. Its **reads** go on. The peer's eviction is still written, being one of the ways out, into the [gate reserve](#the-gate-reserve) if the log has filled meanwhile. The refusal lifts on its own (`statelog_log_truncated_cleared`) once the peer re-anchors, is rebuilt from a peer's snapshot, or is evicted. It cannot see a newer node that has not published since the restore (after a whole broker restore the register is the copy's too), so writes made before that node boots still land.

**After restoring a broker, re-anchor the most caught-up node whose checkpoint was past the restored log's end, before anything else writes to it.** The verb below treats it as the *restored* case and follows the log from its end (a node the log has diverged from is treated the same way); every other node then adopts from it on its own: see [a node a peer re-anchored past](#a-node-a-peer-re-anchored-past).

**If something wrote the restored log first, the reanchor asks you what to keep.** A restored reanchor follows the log from its end, so any record written after the restore by a node whose rows were the copy's age sits below that end and would be applied on no node. Before it runs, the verb walks the log back from its end to the newest record that writes rows (a linearizable read's barrier writes none and is stepped over) and asks whether this node applied that very record: same operation, position and broker instant. If so, nothing written after the restore wrote rows, and the reanchor runs. If not, the verb prints that record (kind, subject, writer, operation, sequence) before the command it would run, and **refuses unless you pass `-discard`**. Two ways out:

- **Keep this node's rows** (what they hold past the copy) by re-running with `-discard`. The records written after the restore are applied on no node; the generation record says so, naming the newest, and every other node adopts this node's snapshot.
- **Keep what was written after the restore** by not re-anchoring this node: stop it, move its replicated database aside and start it again. It replays the log from the broker, or adopts a peer's snapshot where the log has been trimmed; what only its old rows held is given up. Its old position row is replaced on its first heartbeat, which also lifts every other node's `log_truncated`.

The walk vouches for a record through the operation ledger, which travels inside every snapshot, so a record held because the adopted-from peer applied it counts. It cannot vouch where the ledger has lost the record's row to its thirty-day sweep (this node's, or the donor's). The ledger records how far back it may have lost rows, and when the named record's operation is older than that, the refusal says so: the rows may hold the record after all. A record a gate dropped writes no row and reads as not held too. In each case the verb can refuse when nothing was written after the restore, and `-discard` then discards nothing. It never follows the log past a record it cannot vouch for without saying so.

**The checkpoint goes one below the verb's own generation record**, not at the end it read: the verb appends that record first, so anything written between its reading and its append lands below it too. If a copy-age node writes the log in that moment, the verb walks the log again up to its own record, names what landed, and refuses with the generation already open. Re-run it with `-discard` to finish, or keep the record by replacing this node's rows with a peer's and evicting this node, which abandons the generation it opened.

**What the rest of the fleet writes after the reanchor is not applied here.** Other nodes learn of a restored reanchor when their appliers reach its generation record or their heartbeat reads the fleet's new generation, and a copy-age node can write once more in the old generation before either. Wherever the re-anchored checkpoint is followed from (this node and every peer that adopts its snapshot), a record positioned after the reanchor's own record and written in a lower generation is void: consumed, applied on no node, logged `statelog_record_gated` naming `overtaken`.

**A reanchor that stops part-way is re-run.** Once its generation record is on the log, the verb rebuilds this node's consumer and commits the checkpoint on the node's own time rather than the caller's, and the harness waits up to six minutes for it. If it stops anyway (broker blinked, node restarted), re-run the same command: it finds its own record, follows the log from one below it, and still names every record written after the restore beneath it.

**A rebuild under a node that never restarts is caught too**: the position heartbeat reads the stream's state every ten seconds, and a write that would publish at an expectation of zero reads it within the write; the creation instant arrives in both answers. Nothing else can see it: a rebuilt stream comes back at generation 0 counting from 1, so once it has published past the node's checkpoint every sequence term reads healthy while the node applies a different history into rows keyed by the old one. The node logs `statelog_stream_recreated` with both instants, takes its copy of that domain out of service (seats stay; operations go to the other data nodes), refuses every read and write of that domain with `wrong_stream` until that stream is re-anchored, and applies nothing from the rebuilt stream.

**Writes refuse, not only reads**, every kind rather than only the retry at zero: every expectation a node forms is a sequence from the stream its rows came from, and the rebuilt stream (same name, same generation) would arbitrate it as one about itself, so zero would land on a subject the old history wrote and an ordinary one would be accepted wherever the new history ended at the same number. A refused write is an `unavailable` answer naming `wrong_stream`, both creation instants and the `reanchor` command below.

`reanchor` is the response:

```
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
# prints the live stream's created_at and which case it is, and refuses

crewlet retention reanchor -stream CREWLET_TRACKER_LOG -confirm 2031-04-02T03:00:00.418226517Z
```

The confirmation is the **live** stream's own `created_at` (the instant the `wrong_stream` refusals name as the live stream's, its `crewlet.created_at`) exactly as the verb prints it, nanoseconds and all. The verb **prints it and refuses** rather than reading it and feeding it back, which would confirm against its own output. An instant that differs at the microsecond is another stream, and is refused.

A reanchor says: *these rows are what they are; follow the live stream from here.* The durable tables are the record of truth and the stream is a replay window, so the rows survive and the window is replaced. The generation makes an old position **comparable and safely stale**: an arbitration anchor below it forms `expect = 0` on its next write, and a client cursor below it is refused by name.

**Three cases, differing in where the log is followed from.** The verb decides which from the same reading of the stream it keys the checkpoint to, prints it before you confirm, and names it in its answer and in the `statelog_reanchored` line:

| Case | What the broker holds | Where the new checkpoint goes |
|---|---|---|
| `recreated` | Another stream: its creation instant is not the one this node's rows are keyed to. It holds nothing the rows came from. | One below its **first surviving record**, so the domain applies everything it still holds. |
| `restored` | The **same** stream brought back from an older copy, ending below this node's checkpoint, or written past it since, so it holds another record there. What it holds up to the copy is a prefix of the history the rows came from. | At the log's **end**, so none of those records is applied again. |
| `abandoned` | The **same** stream, holding everything the rows are missing, but continuing in a generation only an [evicted peer](#a-node-a-peer-re-anchored-past) held. | At this node's **own checkpoint**, in the generation after the evicted peer's, with every record of the generation it skips void. |

**An evicted peer's generation only ever raises the number.** In every case a reanchor opens the generation after every one this domain has used: this node's own, and any a peer the fleet has evicted opened (found from its position row, a floor it published, or its generation record on the log). Records of any generation it skips are void wherever its checkpoint is followed from.

The difference matters: a restored copy replayed from its first record would be applied in a generation that outranks every row, rolling each object back to its state when the copy was taken and overwriting what the rows gained since. A same-stream log that ends at or past the checkpoint, holding there the record the checkpoint names, is neither case (it holds every record the rows are missing), and the verb refuses it as having nothing to re-anchor.

**It moves one log.** Each domain (the tracker, the knowledge base, the identity estate, the vectors, the usage days, the artifacts, their datastore, Ask Crewlet's conversations, chat, the seats' executor sessions and Gitlet) has its own stream and generation. A reanchor moves only the one you named: that domain's checkpoint goes to the next generation, where its case puts it, keyed to the live instant. Every other domain is untouched. If more than one log was rebuilt, re-anchor each. The new cursor, generation and reset are all computed from the one stream you named.

**It needs no restart.** The domain's apply loop is paused for the transition and resumed on the adopted stream; reads and writes are served again, its seats can come back, nothing else on the node pauses. A refused reanchor puts the loop back as it was.

In order, it: checks the confirmation against the live stream; refuses while the fleet guards below say so, and on a node evicted from that domain; appends the domain's own generation record to the adopted stream (every log that claims identity keeps one, applied as `tracker_log_generations`, `pages_log_generations`, `code_log_generations`, `iam_log_generations`, `artifacts_log_generations`, `artifactdata_log_generations`, `assistant_log_generations` and `chat_log_generations`; the vectors and the usage days keep none, because they claim no identity a second opening could violate); reads the stream's instant again, refusing if it was rebuilt meanwhile; moves this node's consumer; and commits the checkpoint. Every step before the checkpoint can be repeated, so a part-way failure is finished by running it again: it derives the same generation and finds its own record there.

It rewrites no row: a version or anchor from before the reanchor sits below every position the adopted stream will produce, which is the order every comparison wants. After a restored reanchor an object whose last record the copy kept is written against that record (the rows already hold it), so nothing waits for an applier that will never re-read it. For a recreated log it does **not** recover records that were on the old stream and never applied here. For a restored one it keeps what the rows hold past the copy, which is on no log any more, so no other node can replay it; each adopts a snapshot from a node in the new generation on its own ([a node a peer re-anchored past](#a-node-a-peer-re-anchored-past)).

For every log that keeps a generation record (the tracker's, the knowledge base's, Gitlet's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, chat's and the executor log's), it refuses while any peer has already re-anchored the stream (a peer at a later generation of it), naming the peer: that peer's rows are the fleet's history in the new generation, and a second reanchor from other rows would open the same generation over a different prefix of what was lost, which nothing could reconcile. This node adopts that peer's snapshot instead, on its own ([a node a peer re-anchored past](#a-node-a-peer-re-anchored-past)). A peer the fleet has **evicted** is not counted, in this rule or the next: its generation is abandoned rather than the fleet's.

Otherwise only the most caught-up node on the stream its rows came from may re-anchor, among the peers holding history the log does **not**: a peer whose checkpoint record the log still holds is on the log, so a reanchor that follows it loses nothing it applied, and it is not weighed (whether what it wrote after a restore is kept is the `-discard` question). So a node the log diverged from can re-anchor although copy-age nodes that wrote the log past it stand further along the log. Every data node publishes, beside its position, the creation instant of the stream its rows are keyed to and the broker's instant for its checkpoint's record, so a peer still on the lost stream, or past a restored log's end, is compared with this node, and one that came up on the rebuilt stream with no rows is not. A refusal names the peer that is further along. `-force` overrides the most-caught-up rule for when the fleet cannot be asked, never the first. The vectors and the usage days are exempt from both, and every data node re-anchors its own copy of each: the vectors' coverage legitimately differs node to node, and each node's usage days are written by that node alone.

**A reanchor of the usage log is followed by every data node publishing every day again.** A restored usage log is followed from its end, so the node that re-anchors it never applies what the data nodes published after the restore and before the reanchor (a late turn of yesterday, a custody batch settled onto an older day), and a day not moved since would stay at its pre-restore value on that node for the 181 days the history keeps it. So each data node watches the usage log's generation on every data node in the positions register, and when one rises past what it last answered, it publishes every day its own event log still holds again (30 days back), under operation ids no earlier copy carries, so the broker's two-minute duplicate window cannot collapse an unchanged day onto the copy the reanchor skipped. The re-anchoring node does so as soon as its usage applier resumes, every other data node within a heartbeat and a publish tick (`usage_republish_started`, then `usage_republish_settled` once every day has landed). The generations a node answered are kept in its own `usage_republished` row, so a node down while a peer re-anchored answers the rise when it starts; a data node's very first start logs the pair once too, its `detail` naming no reanchor, because its row records no pass yet. A day the tail held that is now more than the event log's 30 days old is not derived again, so re-anchor a restored usage log within that window. Re-anchor each data node's copy in turn: each reanchor is a rise the others answer again.

**One generation is only ever opened once.** Two nodes that re-anchor the same log at the same moment (each before the other's position row says so, or both forced while the register could not be read —) derive the same next generation, and exactly one generation record lands. The other reanchor reads back whose record it is, refuses naming the node that opened it, and commits nothing: two openings of one generation would put two histories under one number, which no force makes safe. That node adopts from the one that got there first. A part-way failure still finishes when run again, because the record it finds is its own.

**It records who ran it.** On a log that keeps a generation record, the record every data node applies into its audit row names the operator and the credential they used; on the vector changelog and the usage log, which keep none, the serving node's `reanchored` line names them.

**Every log can be re-anchored, by its own domain.** A log that claims identity (Worklet's, Doclet's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, Chatlet's, the executor log's and the code log's) appends its own generation record, which is the audit row above and the first-writer-wins between two operators; the vectors and the usage days declare that they keep none. A domain that cannot say is refused when the node boots, never when an operator first names its log.

## Proving a restore

```
crewlet retention verify --restore -dir /var/backups/crewlet
```

The one verb here that talks to **no node**: running it through a running engine would have the thing under test test itself. It restores the newest artefact under `-dir` and opens the copy, to establish that the artefact alone is enough. It writes nothing to the live store and takes no lock on it.

It prints what the artefact holds (when it was taken and by which node, each store copy, its estate as `crewlet backup` names it, with its size and migration count, and every domain's generation and sequence) and then **exits non-zero past its cadence**, default 30 days (`-cadence`). The default is derived: `min_age` is 7 days, so a restore path broken for longer than one replay window means the log can no longer bridge the gap between an artefact and the present; monthly gives four windows of margin.

Two refusals:

- **A directory with no manifest is debris**, not a partial backup: the manifest is written last, so its absence means the run did not finish.
- **An artefact naming no domain position cannot be verified**: a restore replays from that sequence, and there is none.

Put it in cron.

## Removal, deletion and what a purge does not reach

Three different things:

- **Removing** a task hides it. The rows stay; a restore brings it back.
- **Deleting** writes a marker. Every data node drops every record about that task, for ever, which stops a redelivery months later resurrecting it.
- **Purging** removes the rows, on every data node as each one reaches the record.

```
crewlet worklet purge <item> -reason "why" -confirm <item-key> [-op-id ID]
```

`<item>` is the task's key or its id. The purge removes every row the task's own records wrote: comments, body revisions, every inbox notice, turn record and dependency-mirror row about it, and the content of every history row (a history row carries its record's whole change). One account survives, the purge's own: its history row and the project lead's `purged` notice, which say that it happened, to which key, by whom and why. Beside it stays a **skeleton** of the task's history: the rows that moved a count (creation, a removal or restore, each change of status, assignee or project), each cut to its kind, its instant and those three changes, because the work flow answers the past by walking them backward and a purge takes the task out of that series only from the moment it happened.

The confirmation is the task's **key**, checked against the task `<item>` resolves to; nothing is destroyed on a mismatch. The id may be what is on the command line, so repeating it would confirm nothing, while the key has to be looked up. There is no project to name: the task's own row says which project the record is filed under. The **reason is required** because it is the only thing that survives: the deletion marker's reason is the entire account of what used to be at that key. A purge is an **operator gesture**: `admin`, held by a person or a token (a person's session asks for a recent proof first), never an agent and never the engine, because nothing else can be asked to confirm it.

It answers the same three-valued outcome every write here has:

- `pending`: the record is on the log and each data node's rows go as it reaches them; do not run it again.
- `unknown`: retry, passing the printed operation id back in `-op-id` so the retry cannot append a second purge. If the first had landed, the retry answers `applied`, at the first purge's position, rather than finding the task gone and refusing. Pass it back exactly as printed: the id carries the instant it was minted, which a node judges the retry by once its operation ledger may have lost the first purge's row to the thirty-day sweep, and there it answers `unknown` again rather than purging twice.
- An id of your own making is refused (`op_id_invalid`): it carries no instant, so no node could tell whether it already ran. So is a printed one altered on the way back (trimmed, spaced, or grown past 128 bytes), because the broker carries the id in a header that trims its ends and rewrites a line break, and would deduplicate it as another operation.

A purge names **one** task, so its **children are moved, not destroyed**: each direct child re-parents onto the purged task's own parent, or becomes a root when the purged task was one, and the subtree's depths and ancestry are rebuilt. Destroying the subtree would destroy work nobody confirmed; leaving it would leave children pointing at an id that resolves to nothing.

What it prints deliberately gives **no time guarantee**: an offline or evicted disk retains its copy until replay, adoption, replacement or destruction.

### A repository

A repository the company [hosts](../concepts/gitlet.md) is removed by the configuration, not a command, in three timed stages:

- **Deleting** is removing it from `code.repos`. At that activation it takes no push, and its refs and packs stay; declaring it again under its name restores it as it was.
- **Purging** follows `code.deleted_retention_days` after the deletion (unset, 7 days). The `code-retention` duty removes the repository's rows in bounded steps, each one record every data node applies, paced to a third of each node's write lane; the last leaves only its deletion marker, which drops every later record about the repository on every node, for ever. The last step also deletes the key the viewed marks of its changes were digested under: those marks are each person's own, which no purge reaches, and name no file once the key is gone and the code log is trimmed past its record ([Viewed files](../concepts/gitlet.md#viewed-files)). A purge once begun is finished whatever the retention says, since it has no undo, and the name is free again once it is. One begins only on a node serving the revision the fleet is on, so a node that failed to apply a longer retention never purges by the shorter one it replaced.
- **The bytes** go a day after that: once no row names a pack, the [object collector](../concepts/object-store.md#collection-and-audit) deletes it like any other object nothing names.

A repository a tool made is deleted by the tools rather than the configuration, and purged the same way. The `code-retention` duty also deletes:

- A seat's own **scratch** repository, once idle (no push, rename, settings change or restore) for `code.scratch.idle_days` (unset, 7 days), unless a live turn holds it; it is then restorable and purged like any deletion.
- A renamed repository's **old name**, which keeps redirecting for a month and is released by the duty after it, one small record a name ([Repositories made by tools](../concepts/gitlet.md#repositories-made-by-tools)).
- A branch a seat set in its own namespace (`refs/heads/seats/<handle>/`) that nobody has set for **14 days**, through a push of the engine's own whose row keeps the commit to restore it at, unless a live turn holds it, it is its repository's default branch, or a live change proposes from or into it ([The ref lifecycle](../concepts/gitlet.md#the-ref-lifecycle)).

Inside a repository that is not deleted, three horizons are kept by the records that apply to it, never by a sweep:

- A push row expires **181 days** after its push, the same half year as the [usage history](#old-data-is-not-cold-data).
- A deleted ref's tombstone expires after **48 hours**, or sooner, oldest first, once the repository holds 100 000 of them.
- The record of the object a forced push or a deletion left expires after **14 days**, by the repository's next push whose record has room for it (see [ref tombstones and ref drops](../concepts/gitlet.md#retention-and-history)).

A [change](../concepts/gitlet.md#changes) and its revisions have no horizon of their own: they are kept as long as their repository is, and its purge removes them. Each data node's index of their words drops them within one rotation after the purge's last step ([Searching changes by their words](../concepts/gitlet.md#searching-changes-by-their-words)). Published layers need no horizon: `code_layers` holds at most 32 rows a repository and `code_layer_parts` at most 16 a layer, each publication replacing what it supersedes in place (at most 544 rows a repository, removed by its purge). The code log is trimmed by the same six terms as every other log; a push record names its pack and never carries it, so trimming the log removes no repository's bytes.

## The configuration archive, and the one thing a purge cannot reach

Every node keeps its **own copy of every configuration revision it has ever met**, in `company_config`: one row per config write, per node, each holding the whole document.

**The revision sweep** bounds it. It is part of the ordinary maintenance tick and runs on **every node**, not under the fleet singleton: each node owns its own copy, so a singleton would tidy only the node holding the duty. It keeps everything created or activated inside **400 days** (a row is dated by the later of the two, activation being the fleet's latest one this node knows of, so a revision the fleet re-activated yesterday stays, even on a node that refused that re-activation) plus, **whatever their age**, the revision this node boots on, the fleet's revision as it last saw it, and the whole parent chain behind each. Those are two rows on a node that refused the fleet's revision and runs the one before (see [what a node's own store keeps](../concepts/control-plane.md#a-nodes-own-store-keeps-three-facts-apart)): what it starts on, and what it restores when the fleet's pointer is gone. Chains are kept by id rather than date because a revert re-activates an older revision and `crewlet config diff` walks it, so a swept ancestor turns both into an error naming a vanished row, and a company that has not changed its configuration for a year boots on a revision older than any horizon worth setting.

Four hundred days is an annual cycle plus five weeks: the gestures that reach furthest back into configuration history are annual (a repeated reorganisation, a compliance review, a renewal), and exactly 365 days makes finding last year's revision a coin flip. The floor is the **audit log's own horizon** (31 days): an audit row naming a revision must still be able to open it, so this can never be set below that.

**A revision is a copy of the org chart it ran.** A human seat's `email` and `contact` account ids are in every revision that carried them, on every node and in every backup, and removing the seat writes a *new* revision rather than reaching the old ones. They leave the store when the sweep takes their revision, and a backup when the backup is deleted.
## Old data is not cold data

There is no tiering, by decision. Every data node holds the whole corpus in one SQL database: a task filed three years ago is read at the same cost, searched by the same index and replicated as many times as one filed this morning. The log in front of it is trimmed; the tables are not, except the usage history, Ask Crewlet's conversations, the chat transcript and the seats' executor sessions (each with its own horizon) and the bookkeeping tables each node sweeps, all below.

So the storage forecast follows how much a company has ever done, not how much it is doing now. Read the numbers below before the fleet is large.

Four replicated table sets keep their own horizon.

- The **usage history** (each data node's company days: spend, turns, page reads, schedule fires) is kept **181 days**: a ninety-day window, the ninety days it is compared with, and the day a moved clock can touch. Nothing sweeps it: every record for a day deletes the rows older than that day minus 181 in the same transaction that writes it, on every data node, and the stream's own age bound forgets the same days. See [Replication](replication.md#two-compacted-domains-the-embeddings-and-each-data-nodes-day).
- Ask Crewlet's conversations: one nobody has touched for 30 days is purged by the trash reaper through a record every data node applies (see [Running Ask Crewlet](assistant.md#retention-and-erasure)).
- The chat transcript's horizon is the company's to set: see [the chat transcript's horizon](#the-chat-transcripts-horizon) below.
- The seats' **executor sessions**: one row per turn, naming the sealed object its harness's session is kept in. Every way a row goes is a record every data node applies.
  - A turn's end deletes exactly the ended turns of its conversation that its first launch was handed (the session it continued, the endings it was told and the sessions it was offered beside it), since its own session and account now hold them. A turn its launch was not shown (one that ended beside it, or one past the sixteen endings a launch is told) stays, and the next turn is shown it.
  - Past a seat's `turn_engine.conversation_session.max_bytes_per_seat` (64 MiB by default) a capture releases the seat's oldest ended sessions, keeping how each turn ended and dropping only its object. The same figure is the allowance of the seat's saves of its running sessions, whose replaced objects the [object collector](../concepts/object-store.md#collection-and-audit) deletes a day and more later, so a seat's objects reach up to twice it ([saved as it works](../concepts/code-sandbox.md#a-running-jobs-session-saved-as-it-works)).
  - The trash reaper deletes a row untouched for the company's `turn_engine.conversation_session.retention_days` (30 days by default), ended or not, never while its turn's run is live, and takes the row's tombstone with it (no record decided before the delete can land that late).
  - It also ends a turn as `lost` when the turn never ended, a day has passed since its last record (its newest capture, or the `begin` that opened the row of a turn that captured nothing) and no run of it is live. Until ended, the session holds the seat's budget (which only releases ended turns' sessions) and its begin holds the session it was handed against every release but a [retirement](../concepts/conversation-sessions.md#a-session-that-ran-out-of-context-is-retired).
  - A seat that leaves the company takes its rows with it once its [mailbox is retired](../concepts/seat-ownership.md#the-removed-seat), by one record every node applies in order, which also refuses a capture or an end decided before it and landing after. That record goes with the same retention.
  - A person's removal does not reach the sessions: what they said to a seat stays in the sessions of the turns that read it until each row goes by one of the ways above, the retention at the latest.

The **access history** (what each seat was told about its own access, every SSH certificate the engine signed and command it ran or a person denied, every verdict on a [delegated request](delegated-decisions.md), and every step of a growth act: a call made, a held call's ask, answer and outcome, on `CREWLET_ACCESS_LOG`) is kept for the company's `access_history.retention_days`, **400 days** by default, settable from 90 to 3650. The trash reaper expires it an hour at a time, by one record each hour that every node applies by the broker instants its rows hold, so every copy deletes the same rows. Each seat's newest told record stays whatever its age: the seat's next turn is diffed against it. A removed seat's told records are forgotten at the reaper's next pass, by a record that also keeps a turn racing the removal from being told into it; its certificates, commands, verdicts and growth steps stay to their retention, as the audit trail.

The **atlas's history** (what people recorded about what the company runs on, on `CREWLET_ATLAS_LOG`) is kept for the company's `atlas.retention_days`, **400 days** by default, settable from 90 to 3650, the access history's horizon for its reason: a yearly review finds the whole year it reviews. Only history goes: a value a newer value from the same person replaced, once replaced for that long, and an entry a person retired that nothing has reported since. What the atlas holds now never expires. The trash reaper expires it an hour at a time, by one record each hour that every node applies by the broker instants its rows hold. An administrator's [purge](../concepts/atlas.md#the-atlass-own-log) is the other way something leaves it, at once and history included.

The **Advisor's history** (findings with their history, runs, weekly reports and alarm episodes, on `CREWLET_ADVISOR_LOG`) has fixed horizons: a closed finding and its history **90 days**, a run and a cleared alarm episode **30 days**, a weekly report **26 weeks**. An open finding never expires, and a finding's key outlives its episodes while a dismissal still holds it quiet or a fix that did not take is remembered against it. A day's expiry is one record every node applies by the instants its rows hold, and a day is expired once.

Two tables are swept **per node** rather than once across the fleet: each data node applies the log into its own copy, so a fleet singleton would tidy one node and let every other grow for ever.

Each domain's **operation ledger** (the table that answers "did the operation I published land here?") is swept at **30 days**. A machine retry lives inside a five-second wait, but a seat carries an operation id forward and re-asks on its next wake, hours or a weekend later. An operation id older than the horizon resolves `unknown` rather than `applied`, and is never applied a second time: each pass records the cutoff it deleted before, and a write whose operation was minted earlier than that, with no row left to say whether it landed, is answered `unknown` without being published (see [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted)).

The horizon belongs to the **domain**, which may declare a **shorter** one for a subject kind whose writers never re-ask late. Every domain takes 30 days for its ledger as a whole; the identity domain keeps its **session** subjects' rows (one per sign-in and sign-out, whose operation ids nobody re-asks after the request that wrote them) for **one hour**: the publisher's five-second resolve budget with a wide margin. It is its own maintenance job, `iam_ops_session`, so what it deletes is counted apart.

A strict log's **arbitration anchors** (one row per subject any writer has contended on, holding the position of the last record consumed on it) are removed below the log's **first surviving record**, never on a clock: the trim holder appends a `trimmed` record and every node's applier removes the same rows in the transaction that applies it (see [Anchors below the log are removed by a record](#anchors-below-the-log-are-removed-by-a-record)). An anchor above it names a record the log still holds, and removing it would cost that object's next write a read of the log.

The **authentication trail** (`iam_history`, the identity domain's record of who did what to whom) is the one table in any domain with **two** horizons, split by a `class` column the engine derives from the operation. A **change** (somebody suspended, an access granted, a person removed) is an audit asked a year later, and in several jurisdictions must be answerable. A **session** (who signed in on Tuesday) answers an investigation that is days old, and is a record of a person's working hours; keeping it as long as a change would store more about people than there is reason to.

The trail's sweep is **a record on the log**, not a local delete: the rows are identity-claimed, so a node sweeping on its own clock would hold different bytes from its peers. The record names *positions* the publisher resolved once ("every change below this one, every session record below that one") and one instant, also read once, against which it collects the rows that are **over** rather than old:

- sessions that ended or passed their absolute deadline;
- invitations that expired unredeemed **or were redeemed** (one with no expiry counts as expired, since no reader treats it as open);
- credentials (passwords, second factors, machine tokens) that were **revoked or passed their own expiry**.

Each is kept a week (168 hours) after it stopped being presentable, so the sessions and credentials screens can say what ended and why; after that the trail row, kept for the change or session horizon, is the durable account. It runs **per bucket**: the estate is divided into 64 partitions by a hash of the person's id, so one horizon's deletions are 64 bounded transactions of at most 4 000 rows each.

A credential is collected from **two** places: the person's own row carries their credentials whole, and the rows the credentials screen reads are derived from it. Deleting only the derived rows would be undone by the next change to that person's credentials, which republishes the whole set, so the sweep rewrites the person's row without the collected credentials in the same transaction. How somebody joined outlives the redeemed invitation: the redemption is on the change trail, kept for its own horizon.

The identity domain's **operation ledger** keeps the 30-day horizon and one hour for its session subjects. It is swept per node by the maintenance sweep, since each node owns its copy. The hourly sweep of the session subjects records how far back it has lost **that kind** of operation, beside the watermark the month's sweep keeps for the whole ledger, so a session operation retried after the hour is answered `unknown` while every other identity operation is still vouched for over the month (see [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted)).

### A removed person's values stay on the identity log until the trim

A removal erases every sealed value of the person's from every data node's rows in the transaction that removes them: name, address and any second factor's seed, from their own rows, from an invitation addressed to them and from the trail rows that outlive them (see [Identity and Access](../concepts/identity-and-access.md#removing-somebody-erases-what-is-theirs-from-every-data-nodes-rows)). It cannot rewrite the **log**: the records that wrote those values stay on `CREWLET_IAM_LOG`, sealed under the fleet keyring, until the trim passes them, never before `min_age` and later for as long as any of the six terms holds the identity log. Every backup taken meanwhile carries them in its stream snapshot.

So a blocked trim is an erasure that has not finished: the identity log named by an alarm holding its trim, under `trim_blocked`, or under `backup_age` with the `backup_floor` term on its entry (the backup term holds it while the identity log's own backup is missing or stale, and a hold folded there raises no `trim_blocked` of its own; [the backup term](#a-company-that-never-backs-up-never-trims)), also answers "is that person's name still on disk". The remedy is the term's own (a backup that never left the host, a pin nobody released), never a shorter `min_age` for its own sake, which only moves when a record *may* go.

### The identity duties

Two things keep the identity estate's retention; one is a fleet singleton on its own lease, run once as soon as a node claims it and then on its interval.

| Duty | Interval | What it does |
|---|---|---|
| `iam_sweep` | 1 hour | Resolves `api.auth.audit.changes` (400 days) and `api.auth.audit.sessions` (90 days) to positions and publishes one sweep record for each bucket that is **due**: one holding something at least a day past its horizon. A bucket is swept at most about once a day, so the sweep adds at most 64 records a day however often it runs. |

The other is the operation ledger's sweep, in the ordinary maintenance tick on every node.

A node arms the duty only if it runs the `workers` role: every data node applies the identity log, but a node without `workers` claims no singleton. `identity_duty_seconds` on [`GET /health`](../reference/api-endpoints.md#the-health-envelope) names the duty a node armed and its interval, the only way to tell a duty that is running and finding nothing from one never armed.

A person's **inbox** (one row per routed change per recipient) is swept at the company's `inbox.retention_days`, **365 days** by default, settable between 30 and 3650 (an administrator changes it in place on Company › Integrations › Worklet › Settings), and changed live: the sweep reads it off the running company at every tick, as the applier does at every batch. It deletes a *pointer*, not the thing pointed at: the history row behind every notice is never swept, so "what was I told about in 2024" is still a `work_activity` question at any age. A month is the floor because below it an inbox stops being one: somebody away four weeks would come back to nothing.

A change's **notices** (one row per recipient of every record about a [change](../concepts/gitlet.md#who-a-change-wakes): a review asked, a change merged or ready, a mention) keep two horizons. A person's is `inbox.retention_days`. A seat's is **30 days**, the horizon of a turn's own detail in the event log: a seat is woken through the code log, never from its row, so past the turn the row names a delivery nothing can show, and seats are most of a change's audience. Each node sweeps its own rows of each kind in the ordinary maintenance tick (`code_notifications`, `code_notifications_agents`); no two nodes need to agree, since the rows a record writes depend on when the node applied it and the horizon its company stated. The applier writes no notice older than its recipient's horizon, so a node replaying the code log from far back rebuilds none the sweep would delete. A repository's [purge](#a-repository) removes its changes' notices with its changes, within the same bounded parts as their rows: every change keeps a count of the notices its records could have written, the same on every node whatever each one's horizon kept, and a part charges a change's notices by that count, so no single write deletes more rows than its bound on any node.

### The chat transcript's horizon

The tables above are swept per node because each is that node's own bookkeeping. The transcript is replicated content, so it is not swept: it is **pruned by a record**, one more change on the chat log that every data node applies. A message is the only copy of what somebody said, so `chat.chatlet.message_retention_days` (**365 days** by default, settable between 30 and 3650, and **`0` to keep everything for ever**) is a policy decision. `0` reads opposite to `inbox.retention_days`, where it means "take the default": an inbox row is derived from history that answers for ever, a message is derived from nothing. The range is 0, or 30 to 3650: Chatlet's page under Company › Integrations shows the horizon in force beside its default, changes it in place, and says a value in the hole beside the field before anything is sent.

The prune is a record: a message's instant is the BROKER's, stamped when the record was stored, so one fleet-singleton duty (`chat-prune`) publishing one cutoff makes every data node delete exactly the same rows; nodes reading their own clocks would diverge permanently, with no read that repairs it. A room may carry a horizon of its own, which beats the company's, set by an administrator on the human write surface (the dashboard's Room details, About › Messages are kept, [`PUT /chatlet/channels/{channel_id}/retention`](../reference/api-endpoints.md#a-chat-erase-prune-or-horizon), or [`crewlet chatlet retention`](../reference/cli.md#crewlet-chatlet)). Each node's chat keyword index follows the same range afterwards, because a forward-only walk cannot see a row that vanished. See [Chatlet § Retention](../concepts/chatlet.md#retention).

The duty prunes by the `message_retention_days` of the revision the fleet's activation pointer names, read only on a node that runs that very revision. A node serving any other (it refused the fleet's revision, booted on an offline import the fleet has not taken, or has not yet applied a revision activated a moment ago) neither publishes a cutoff nor claims the duty, and logs `chat_prune_declined` naming the activation it serves and the fleet's epoch; a peer running the fleet's revision takes the duty once the lease lapses. A lagging node makes a horizon late, never early, the direction a prune can afford: an hour missed is pruned on the next tick, and a message pruned by a horizon the company has since raised cannot be brought back.

## The storage forecast

For the reference company (100 000 tasks a year, 300 000 comments, 1 000 edits a day, 3 000 turns a day, 50 projects):

| | Year 1 | Year 3 | Year 5 |
|---|---|---|---|
| replicated store, per node | 9.7 GB | 26.6 GB | 43.8 GB |
| snapshot volume at ×4.2 | 40.8 GB | 111.8 GB | 183.9 GB |
| plus, hosting code at 3 000 pushes a day: Gitlet's rows, per node | 0.4 GB | 0.4 GB | 0.4 GB |
| and their snapshot volume at ×4.2 | 1.7 GB | 1.7 GB | 1.7 GB |

Gitlet's line is for a company whose seats push 3 000 pushes a day (a hundred seats pushing thirty times a day). In this release pushes are people's, their CI's and their seats' (through the code tools and from coding boxes), and the line scales with them.

- A seat's push leaves a push row of about 680 bytes where its code tools built it (naming the seat and the turn, work item and model behind it) and about 755 from its coding box (which names the launch and the harness besides); a person's is about 623. Rows are kept the 181 days of the push-history horizon: 410 MB once the horizon is reached, in the first year, every push priced as a box's, the most a seat's push leaves, since which seats push from boxes and which through tools is no figure the reference company has (369 MB priced as a tool's).
- Its pack's row is about 588 bytes, kept while the pack is live. A repository's home Gitlet node looks at it every minute and merges its small packs once sixteen have gathered (a busy repository's at every look, the minute's pushes taking it past them; [Compaction](../concepts/gitlet.md#compaction)). So a repository holds at most fifty live pack rows plus a minute's pushes, and a row more for each 0.9 GB its history fills: one pushed now and then sits near the sixteen, and one pushed three hundred times a minute held 304 at the most, measured, some 29 KB at fifty and 179 KB at 304.
- A merged pack's row is kept one more day, as a retired row of about 608 bytes: at most sixteen for every fifteen pushes, about 2.0 MB at this rate.

So the line stops growing once the push history reaches its horizon (it grew by about 644 MB of pack rows a year while nothing merged a repository's packs), measured in `internal/gitlet`'s "What a push costs, measured" and set out with the code log's share, a seat's one counter row and a larger company's figures under [What every data node holds for Gitlet](../concepts/scaling.md#gitlet-nodes). The bytes of the packs are the [object store's](../concepts/object-store.md). A Gitlet node's cache is not here: its copies of the repositories, the index of which push introduced each commit and the memo of its seats' read answers (at most a sixteenth of the budget; [The memo](../concepts/gitlet.md#the-memo)) are disposable files inside `gitlet.cache_max_bytes`, derived again from the rows and the object store, which no retention keeps and no backup copies.

**Chat is forecast separately, and it is the reason the horizon exists.** At the declared census of 20 000 messages a day, a year is 7.3 M message rows: roughly **7 GB per data node, per year** at this schema's row-and-index width, comparable to everything else the reference company writes in its first year. Under the default 365-day horizon that is a STEADY STATE, not a slope: the prune removes about as much as the company writes, on every data node, in every snapshot and in every join transfer. With `message_retention_days: 0` it is a slope, and the snapshot volume should be sized for it. It is DERIVED over a declared census and changes proportionally with it.

The mutation log itself, on a healthily-trimming fleet, holds **about 85 MB** — the `min_age` window's worth of records, not a year's: seven days, `min_age`'s default, of the 12.1 MB a day the reference company writes to it. `min_age` binds on a fleet that trims, because the trim removes only what all six terms permit and every other one sits nearer the log's head: the backup floor at the newest verified backup, at most a day old (`backup_max_age`) or the trim would be blocked, the snapshot floor within a day, and the rest where the nodes and the wake feed already are. The day is 9.9 MB of records and 2.2 MB of read barriers:

| In a day | Each | Of the log |
|---|---|---|
| 274 tasks filed, at a 2 KiB description | 5 266 bytes, in two records | 1.44 MB |
| 822 comments, at 1 KiB | 3 037 bytes | 2.50 MB |
| 1 000 edits, each a status change | 1 181 bytes | 1.18 MB |
| 3 000 turns charged to a task, a summary at its 600-byte cap | 1 608 bytes | 4.82 MB |
| 12 525 read barriers: the seats' 12 500 and the object collector's 25 | 176 bytes | 2.20 MB |

The records are measured on a real broker, written as a seat's tools write them, each change with the notification the engine attaches (`internal/worklet`'s "What a day costs the log, measured"), each in the signed frame every state-log record is appended in ([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)): about forty bytes a record, the frame's head and a 32-byte MAC around the active key's id, measured with a two-byte id, so each character more adds a byte to every record. Project keys reach four digits in the reference company's first year and five by its fifth: three bytes more wherever a key is written (three times in a filed task, once in a comment or an edit) — about 8 kB of the day, under a tenth of one percent. A record carries a description or a comment whole and its notification's excerpt of it again, so the text a company writes is part of the figure: with half of it — a 1 KiB description, a 512-byte comment — the records come to 9.2 MB. The chat log holds about 230 MB at the census (a week at its 32 MB a day). `blocked_by` says whether this fleet trims; if set on a log, that log is growing without bound and the forecast does not apply to it.

The read barriers add about **800 MB a year** to the log's throughput — 12 500 a day at 176 bytes, 18 % of what the log takes. It comes from an assumed 125 linearizable reads a day per agent seat, which is 12 500 for the reference company's 100 seats, at 176 bytes a barrier — the code log's `gitlet.BarrierRecordBytes`, which a mutation-log barrier costs too from the log's second generation to its tenth (168 bytes at its first). On a fleet that trims they are about 15 MB of the 85. On one whose trim is blocked the log keeps the whole day, about 4.4 GB a year, so a log created at 16 GiB fills in nearly four years — nearly five on its records alone — and one created at 4 GiB in about a year.

**Each log's read rate is its domain's own declaration.** A barrier lands on the log of the domain the read is about, so the engine's register states, per domain, how many linearizable reads a day each agent seat puts on its log, and a domain that grants `linearizable` cannot be registered without stating it.

- The tracker's, the knowledge base's, the artifacts log's, the artifact datastore's and Gitlet's logs each declare **125**: a seat's tools read all five at `linearizable`, and the census says how many reads a company makes, not how they split, so each declares all of them.
- Chat's declares **120**: the four chat reads each of a seat's thirty turns a day is declared to make, at the seat's own level. Its ceiling, `stream.chat_log_max_bytes`, carries a barrier term of 120 × 100 × 365 × 176 B, about 771 MB a year at the reference company.
- The identity estate's declares **none**: no read of it takes a barrier, because every identity answer is a node's own rows with their lag beside them, and a session a node has not applied yet is waited for at the position its bearer states. Its ceiling, `stream.iam_log_max_bytes`, has no barrier term: its 512 MiB default is sized from sessions (0 × 100 × 365 × 176 B of barriers).
- Ask Crewlet's declares **none**: no seat reads it, and a person reads their own conversation through the node their writes waited on, so its one barrier is the collector's pin below, counted as the engine's own.
- The executor log declares **150**, five reads a turn over the census's thirty turns a seat a day: a turn's dispatch reads it at the seat's level once, for the head of the conversation it continues; its first launch reads that session's row once more, to take it up only while it still names what the read froze; each relaunch (a send-back, an answered question, a lost job carried on) reads the turn's own row twice (as its runner asks which session it continues and as its launch takes it up), at a mean of one relaunch a turn; and the seat's executor reads its sessions through `list_sessions` and `read_session`, each call one read, at a mean of one call a turn. Its ceiling, `stream.executor_log_max_bytes`, carries a barrier term of 150 × 100 × 365 × 140 B, about 767 MB a year at the reference company, beside the about 32 MB a day its begins, captures and endings write.

The [`census_drift`](../reference/alarms.md) alarm holds every log to its own census: its domain's rate per agent seat (a company with none is counted as one), plus what the engine reads on its own: the object store's collector, one in the fleet, pins the log of every domain whose rows name a file 25 times a day (24 collections and one audit). The alarm names every log past twice its census, and a log whose domain declares none on its first barrier, each with the Tier A field that sets that log's ceiling: `stream.tracker_log_max_bytes` on the mutation log, `stream.chat_log_max_bytes` on chat's, `stream.executor_log_max_bytes` on the executor log's, `stream.iam_log_max_bytes` on the identity estate's. Every read past the census is a barrier record on that log, so it is that field's sizing the extra reads have outgrown, whatever else the field is sized from.

The code log's census also counts its git clients — people and their CI, who read no seat's tools — as the barriers that fill a tenth of the ceiling its stream holds over the whole of `min_age`, at 176 bytes a barrier in its signed frame (about 87 000 a day at the 1 GiB floor and the default seven days).

- A seat's own reads of the code log (a ref resolved for it, and a read of a repository's content by object id that looks once at the log's head for an object its home's copy lacks) are counted in its 125 a day, as every log's are, and are no part of that term; nor are its coding boxes', which read at their run's floor.
- The ones a seat's work can repeat at will (a box's read past its run's floor, a tool's look at the head, an engine write's second judgement there) are held to that 125 on each Gitlet node, together, 16 at once and one every 11.5 minutes after: one bucket of the seat on each Gitlet node, which its boxes and its turns' tools both pay. Past it they are answered at the seat's floor ([a box's re-syncs](../concepts/gitlet.md#a-boxs-re-syncs-and-requests-in-flight)).
- A check's box's re-syncs are bounded by its repository's checks rather than by a seat: 16 at once and 125 a day for each repository on each Gitlet node, its transfers and its reads together. They are in neither term; a looping checks file adds at most that to the log.

That term belongs to the log, not to the company's current configuration, so it is counted whether or not `code.backend: gitlet` is set: the log keeps its ceiling either way, and the rate it is held against is every node's over the last day, which still holds a day of git reads after Gitlet is turned off, and the peers' reads while a rolling restart turns it on. Where Gitlet is busier, resize the log with `crewlet retention set-capacity CREWLET_CODE_LOG`, as in [Changing a log's ceiling](#changing-a-logs-ceiling); the census moves with the ceiling the stream then holds. `stream.code_log_max_bytes` sizes only a code log not yet created, so raising it under one that exists changes neither the log nor its census.

The rate is the log's, counted where every data node applies it, and each barrier is counted in the hour it was **committed**: a node back from days away, or adopting a snapshot a day old, replays days of barriers in minutes, and they count toward the day they happened in, not today.

Two excursions are designed for. The second never alarms; the first alarms only on a small volume:

1. **A full re-embedding.** A width change republishes every vector, so for the week after it the vector log holds the company's whole corpus: about 17 MB per agent seat per year of history, 8.46 GB (7.88 GiB) for the reference company in its fifth year. That bottoms the vector log's headroom at about 51 % at the 16 GiB a 64 GiB volume asks for, and at about 88 % at the 64 GiB a volume of about 653 GiB or more gets. The alarm threshold is 10 %, clear of each. But the one budget scales every log to fit the broker: the thirteen logs' asks are scaled down to the share they divide, five eighths of what the broker can grant once the object store's `nats` bucket has its quarter. So a first boot with every field unset leaves about 59 % of the 19.1 GiB a 200 GiB volume creates it with, about 33 % of the 11.7 GiB a 128 GiB volume does and about 13 % of the 9.0 GiB a 100 GiB volume does; and a 64 GiB volume creates the log at 5.6 GiB, too small to hold that re-embedding at all: below about 98 GiB free, set `stream.tracker_vectors_max_bytes` to at least 9 GiB. The alarm fires within a tenth of the ceiling, so **on a volume of less than about 98 GiB free, a year-five re-embedding raises `log_headroom`**, and below about 88 GiB the log refuses it. The line would be about 81 GiB without the code log's share of the same budget ([what the code log's reservation costs](replication.md#what-the-code-logs-reservation-costs)). Below it, set `stream.tracker_vectors_max_bytes` as above, which the log is created with whole; or, for a company that hosts no code, set `stream.code_log_max_bytes` to its 1 GiB floor, which moves the line to about 84 GiB and leaves a 100 GiB volume's year-five re-embedding about 26 % free. Either takes effect at the first boot; once the streams exist, the same trade is [`crewlet retention set-capacity`](#changing-a-logs-ceiling). The corpus is the company's whole history, so a larger or older company sets `stream.tracker_vectors_max_bytes` at about twice it, 34 MB per agent seat per year of history, whatever its volume. That is also why an unset chat log asks for a sixteenth of the free space rather than its whole 8 GiB on a small volume: every derived ask is scaled into the one budget in proportion, so what the chat log asks for comes out of the vector changelog's room.
2. **A burst.** A cross-project move re-keys a subtree of up to 1 000 tasks, one record each, and every 2 000 rows its records write is about a second of applier occupancy on every data node; see [Replication](replication.md#the-burst-degradation-priced).

## The recovery-operations profile

What a production fleet should be set to:

| | Value | Why |
|---|---|---|
| replicas | **3 nodes**, `stream.replicas: 3`, `stream.sync: always`, distinct hosts in independent power domains | Two gives no quorum. Three is the minimum at which two donors can be satisfied while one node has lost its store. |
| disks | store dir and JetStream store dir on **local NVMe** | Anchored to a published p99 fsync budget under 10 ms, against 1–3 ms measured on NVMe and 15–40 ms on network volumes. |
| backups | **every 6 hours**, retained **14 days** | 6 h is `backup_max_age ÷ 4`, so three consecutive failures pass before the trim blocks. 14 d is 2 × `min_age`, so a restore is always into a window the log can still cover. |
| restore test | **monthly**, `crewlet retention verify --restore` | A restore path broken for longer than one replay window (`min_age = 7d`) means the log can no longer bridge the gap. Monthly gives four windows of margin. |
| snapshot volume | **×4.2** of the store, on its own volume | The rotation state above. |
| rejoin window | **30 m** | Against a conservative projection of 5.1 / 12.8 / 20.7 minutes at years 1 / 3 / 5. |
| broker storage | `stream.store_max_bytes` **unset** on a host running one engine; **divided** where several share a filesystem | Unset, the embedded broker takes three quarters of `stream.store_dir`'s free space, measured once at boot: right when it is the only tenant. Free space bounds the *sum* of the engines on a volume, so two that each size themselves from what they can see over-commit it, and the failure is `insufficient storage resources available` (on a fleet, `no suitable peers for placement, insufficient storage`) on whichever stream was provisioned last. The engine names the ceiling that did not fit and this field, and on the standalone one adds the limit in force and what is already reserved, where this node can read them. Five eighths of whatever is in force is what the state logs may reserve between them, a log that exists counting at the ceiling it holds and the derived ceilings of the ones being created sharing what that leaves; the other three eighths are for the streams that reserve nothing and grow: the mailboxes, the event log, the dead-letter stream, the memory changelog and every coordination bucket. |

**Full replay with no snapshot is 5.2 hours at year five**, 10 to 16 hours accounting for index depth: 15 to 46× the snapshot path, which makes the snapshot tier a requirement rather than an optimisation.

## Measured, derived, projected

The numbers here are of three kinds:

- **MEASURED**: the apply drain, the barrier's latency, the publish-to-deliverable time, the quantisation recall, the fsync rates. These came off a benchmark.
- **DERIVED**: the join projections, the storage forecast, the barrier's annual bytes, and the chat transcript's 7 GB a node a year. Arithmetic over measured inputs and a stated corpus; change the corpus and they change. The chat figure is linear in the declared census: a company that talks twice as much pays twice as much.
- **PROJECTED**: the supported search corpus at your concurrency, and the under-load search capacity. These need a benchmark on your hardware; until it runs they are estimates with a stated basis, not facts.
## See also

- **[Replication](replication.md)**: the two regimes, the write outcomes, what the design does not promise.
- **[Read consistency](consistency.md)**: what a full log costs, the fifteen read refusals, every write refusal.
- **[Backups & restore](backup.md)**: the artefact and the runbook.
- **[CLI reference](../reference/cli.md#crewlet-retention)**: every verb's flags and refusals.
- **[Alarms](../reference/alarms.md)**: every condition and its remedy.
- **[Metrics](../reference/metrics.md)**: the instruments for a dashboard.
