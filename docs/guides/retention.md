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
Tier A field, because Tier A is per node and is only the value a stream is
created with: once the stream exists, an edit to the field names a ceiling
nothing is applying, restart or not, and this is the number you divide by.

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
| `feed_ack_floor` | how far **that log's own** wake feed has acknowledged |
| `age_floor` | the newest sequence older than `min_age` |

`feed_ack_floor` is the acknowledgement floor of the durable consumer that each
log's own change feed opens — the feed's **group** `crewlet-tracker-feed` on
`CREWLET_TRACKER_LOG`, and its group `crewlet-pages-feed` on
`CREWLET_PAGES_LOG` — because a record that feed has not acknowledged is one
nobody has been woken for yet. So a feed that has stopped holds back its own
log and no other, and `blocked_by: feed_ack_floor` on a domain names that
domain's feed.

A group name is not the broker's consumer name. The durable consumer is named
from the group **and** the stream, as `<group>__<stream>__<digest>` — for
example `crewlet-pages-feed__CREWLET_PAGES_LOG__e5825de86642` — so on an
external cluster `nats consumer ls <stream>` is what finds it, and a lookup by
the group name alone finds nothing.

A term that **could not be read** blocks, exactly as one that permits nothing
does. A term nobody could read is not a term that is satisfied, and treating it
as satisfied is how a trim advances past a node that could not report. A term
this domain does not **have** — the vector log has no wake feed — is `n/a`
rather than zero, which is a different thing again. And a term that was
read and **binds nothing** — no hold is pinning the log, or a solo fleet takes
no snapshots — reads `unbounded` rather than carrying a sequence. Inside the
engine its value is the largest there is, because it is the identity for the
minimum the trim takes across the six; that is a number chosen to lose a
comparison, not a position, so the screen and the API say the word instead.

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

The floor is load-bearing for **writes**, not only for recovery. A write to an
object whose last record has been deleted from the log cannot compare against
that record any more, so it is retried as "this object's history on the log is
empty" — which is only true on a node that has applied everything that may be
gone. So every such write first checks, freshly, that the rows it decided
from — as they stood when it decided, not wherever the node has got to since —
hold every record up to the one just before the **higher of the floor and the
log's own first record**, and a write that does not is refused `unavailable`
rather than allowed to overwrite a change it never saw. The refusal names its
reason, and makes the same split reads do:

- `behind` when the log still holds every record the node lacks — it is
  replaying up to the floor, or it has already applied past the state the
  write decided from. It clears on its own; retry on the same node.
- `below_floor` when the node's next record is gone from the log itself. No
  replay can supply it, so the node [adopts a snapshot](#the-join-runbook);
  another node can make the write meanwhile.
- `floor_unknown` when the floor or the log could not be read. An unknown
  bound refuses rather than guessing.
- `evicted` for a node the fleet has removed.
- `wrong_stream` when the node's own checkpoint is past the log's end — judged
  on where the node's applier stands rather than on the state the write
  decided from, because whatever it appends lands relative to the former. See
  [Re-anchoring a recreated stream](#re-anchoring-a-recreated-stream).

None of them is a conflict: a conflict tells a caller somebody else is editing
and to re-read, and every one of these is about this node rather than the
object. Reads make the same comparison: a node below the floor whose missing
records the log still holds refuses them as `behind` while it replays them,
and a node whose missing records are gone from the log itself refuses them as
`below_floor` and adopts a snapshot.

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

```
crewlet retention snapshots
```

Its own verb rather than a block of `status`, because the repository is **per
node**: "which of my machines can donate, and how old is what they hold" is a
disk question, and it is the one you ask when a join has failed.

**`SnapshotsKept = 1`.** With N donors the fleet is the redundancy — one per
node across at least two nodes — and a recipient that fails a verification asks
the next donor.

**The manifest names the position the file keeps.** The checkpoint commits with
the rows, so the position inside the copy is the only one that describes it,
and the donor reads it back out of the copy after the scrub rather than from
its own live applier — which has moved on by however many records landed while
the copy was taken. A recipient verifies the two agree and refuses an artefact
where they do not. A domain nobody has written to yet is at position zero in
both, and adoptable.

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
| `ahead_of_log` | this node's checkpoint is past the log's end, so its rows are keyed to a sequence space the stream no longer has |
| `recent` | the newest artefact is younger than `snapshot_interval` — but see below |
| `failed` | the copy was attempted and errored; the engine log carries the error |

`recent` is the one reason a healthy node reaches, and it is therefore **not**
published: a node whose newest artefact is inside the interval is a node that
can donate, so its row carries the artefact and an empty reason. What the
column answers is "why can this node not donate", and a skip is only ever the
answer to that when there is no current artefact behind it.

A node that holds an older artefact and cannot refresh it publishes **both** —
the position it can still donate from, and the reason it has stopped
refreshing. A skip never erases what is already on disk.

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
   race the trim that made it necessary. A running node that finds no donor
   stays as it is, refusing, and asks again on an interval that doubles up to
   five minutes. Stopping a node mid-join — a signal during its boot, or a
   shutdown while it is asking or fetching — gives the join up at once rather
   than waiting out the five-second offer window, and is never reported as a
   fleet with nothing to donate: the next start decides afresh. Nor is a join
   that loses the node's **own database** — an install that failed and whose
   live file then could not be reopened, or an artefact installed and then not
   opened. (A failed install deletes the artefact it fetched before it reopens
   the live file, so the reopen never competes with it for room.) At boot the
   node stops, naming both failures. While running it logs
   `statelog_estate_lost` at error level, naming the file: the node serves no
   tracker, page or search read and gives up its seats until the file opens.
   It reopens the file on its next heartbeat rather than on the doubling
   interval, because reopening its own file asks nobody — but it asks the
   fleet again only on that interval, and a join that fetched an artefact
   before it lost the file doubles it like any other failed ask, so a failure
   the transfer itself causes, such as a full disk, never becomes a transfer
   every heartbeat. If the line repeats, its error names what to fix — the
   disk, the file's permissions — and the node reopens the file on the next
   heartbeat after that.
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

It is **refused while the node is below a trim floor**, and the refusal prints
its position beside the floor, because that inequality is the reason. A
readmission makes the trim count the node again, so readmitting one the floor
has passed — typically a machine that is still switched off — puts back
exactly the pin the eviction lifted, while the node itself is missing records
the log has already lost or is licensed to lose.

The comparison is the one the node's own [write fence](#the-trim-floor) makes:
its last published position in the tracker's log and the pages log, against
the higher of each log's published floor and its first surviving sequence. A
node that has never published a position is judged as holding nothing. Nothing
is written on a refusal, and the node needs nothing from you but time: start it
if it is not running, and it [catches up on its own](#the-join-runbook) —
replaying what the log still holds, adopting a snapshot where it does not.
Readmit it once it has applied every record up to the one just before that
bound: in `crewlet retention status`, its `SEQ` for each of those domains has
reached one less than the higher of the domain's `TRIM FLOOR` and `FIRST`. The
refusal's own `floor`, `first_seq` and `generation` are the numbers it
compared — right after a [re-anchor](#re-anchoring-a-recreated-stream) the
`TRIM FLOOR` column can still show the old generation's floor, which the
refusal no longer reads — and running the readmission again is always safe,
since a refusal writes nothing.

The refusal is not what keeps the fleet's data safe, and being refused is not
an emergency: a node below the floor is refused every write that assumes a
history it does not hold, by its own fence, whether it is readmitted or not.

## A full log refuses; it does not shed

At the ceiling the log **refuses appends**, naming `log_full`, the field, and
the retention block. It does not silently delete old records to make room:
shedding a record no node has applied is exactly the loss the whole gate
exists to prevent.

A full log costs `linearizable` reads, because those append a barrier — which
is every seat tool read. `stale` keeps answering, so the dashboard and the read
API are unaffected. See [Read consistency](consistency.md).

The refusal carries **no retry hint**, deliberately: the only thing that frees
a byte is a fifteen-minute gated job, and the log is full precisely because
that gate is closed. A number here would be a promise the mechanism does not
make.

## Changing a log's ceiling

A log's Tier A ceiling (`stream.tracker_log_max_bytes`,
`stream.tracker_vectors_max_bytes`, `stream.pages_log_max_bytes`) is not a live
setting: it is the value the log's stream is created with, sized with the
other logs inside what the broker can grant (see
[Replication](replication.md#how-the-byte-ceilings-are-sized)). Changing a
running log's ceiling, raising it or lowering it, is a **fleet-wide
maintenance window**, and the reason is not caution:

- A resize is decided against the usage the log is at, and a publisher makes
  that a moving quantity. The verb decides it before the window opens: a
  target at or below what the log already holds is refused, naming both,
  because that ceiling would refuse every append the moment it applied.
  Anything above the usage is fair, including a target under the current
  ceiling, which is how a log created larger than its budget gives the
  reservation back.
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
`crewlet retention status` and the Fleet screen lead with it — the stream, the
phase, the attempt, how long it has been open, who ran the verb, and who is
still outstanding — because every number underneath describes a fleet in which
nothing is running, and a blocked trim read without knowing that sends you
after the wrong thing. **No acknowledgement outstanding is not progress**: it
means the operation is waiting on its operator, and both surfaces say so
rather than printing an empty list.

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

The engine detects this from the stream's own **creation instant**, which the
broker reports and every applier compares at boot against the instant its
checkpoint was committed under. On a difference the applier **stops** rather
than resuming — the log line names both instants and this verb — the node's
reads and writes refuse `wrong_stream` with that reason, its seats move to a
peer, and `crewlet retention status` shows the domain as not ready, naming the
recreation. A checkpoint past the log's end is caught as `wrong_stream` too,
because a position the log has never reached is a position on another stream.

**A broker restored from an older copy is the case the instant cannot see.**
Bringing back the broker's store directory from a disk snapshot — or, for the
embedded broker, restarting the engine on yesterday's volume — keeps the stream
it had, creation instant and all, so every identity check passes. What differs
is where the log **ends**: below the checkpoint of a node whose own database is
newer. Such a node refuses every read and every write of that domain with
`wrong_stream` from the moment it boots, logs `statelog_ahead_of_log` with both
numbers, and shows as not ready in `crewlet retention status`. Its writes have
to refuse, not only its reads: whatever it appended would land at the log's
next sequence, which is one its own applier has already passed and will never
apply.

That refusal lasts only while the log ends below the node's checkpoint — an end
read from a stream member that has not caught up looks exactly the same, and a
refusal nothing could lift would take a healthy node out over one leader
election. So if anything writes the restored log past that checkpoint — a node
whose own database was not ahead of it — the refusal lifts and the node carries
on from rows the log does not contain. The engine cannot tell that from the
harmless case, so it logs `statelog_ahead_of_log_cleared` saying both. **After
restoring a broker, deal with every node whose checkpoint was past the restored
log's end before anything else writes to it** — re-anchor it with the verb
below, or have it adopt a peer's snapshot through [the join
runbook](#the-join-runbook).

**A rebuild under a node that never restarts is caught too**, wherever the node
reads the stream's state: the position heartbeat does every ten seconds, and a
write that would publish at an expectation of zero does within the write. The
creation instant arrives in both answers. Nothing else can see it — a rebuilt
stream comes back at generation 0 counting from 1, so once it has published
past the node's checkpoint every sequence term reads healthy while the node
applies a different history into rows keyed by the old one. The node logs
`statelog_stream_recreated` with both instants, gives up its seats, and refuses
every read and every write of that domain with `wrong_stream` for as long as
the process runs against the rebuilt stream.

**Writes refuse, not only reads**, and every kind of write rather than only the
retry at zero. Every expectation a node forms is a sequence from the stream its
rows came from, and the rebuilt stream — same name, same generation — would
arbitrate it as a sequence about itself: an expectation of zero would land on a
subject the old history had written, and an ordinary one would be accepted
wherever the rebuilt stream's own history happened to end at the same number.
Anything that landed would be applied by every node once the stream is
followed from its head. A refused write is an `unavailable` answer naming
`wrong_stream`, both creation instants and the `reanchor` command below.

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

```
crewlet work purge <task-id> -project KEY -reason "why" -confirm <task-key>
```

The confirmation is the task's **key**, not its id: the id is already on the
command line, so repeating it confirms nothing, while the key has to be looked
up — which is the point of asking. The **reason is required** because it is the
only thing that survives: the rows are destroyed, and the deletion marker's
reason is the entire account of what used to be at that key. A purge is an
**operator gesture** — a person or an operator token, never an agent and never
the engine — because nothing else can be asked to confirm it.

It answers the same three-valued outcome every write here has. `pending` means
the record is on the log and each node's rows go as it reaches them; do not run
it again. `unknown` is the one to retry, and the printed operation id goes back
in `-op-id` so the retry cannot append a second purge of a task the first one
may already have destroyed.

A purge names **one** task, so its **children are moved, not destroyed** —
each direct child re-parents onto the purged task's own parent, or becomes a
root when the purged task was one, and the subtree's depths and ancestry are
rebuilt with it. Destroying the subtree would destroy work nobody confirmed,
and leaving it alone would leave every child pointing at an id that resolves to
nothing.

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
than `applied`, which is the honest answer once the row is gone.

A person's **inbox** — one row per routed change per recipient — is swept at
`tracker.native.inbox_retention_days`, **365 days** by default and settable
between 30 and 3650. It is the one horizon here that deletes something a person
reads, and it deletes a *pointer* rather than the thing pointed at: the history
row behind every notice is never swept, so "what was I told about in 2024" is
still a `work_activity` question at any age. A month is the floor because below
it an inbox stops being one — somebody away for four weeks would come back to
nothing.

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
| broker storage | `stream.store_max_bytes` **unset** on a host running one engine; **divided** where several share a filesystem | Unset, the embedded broker takes three quarters of `stream.store_dir`'s free space, measured once at boot — right when it is the only tenant. Free space bounds the *sum* of the engines on a volume, so two that each size themselves from what they can see over-commit it, and the failure is `insufficient storage resources available` — or, on a fleet, `no suitable peers for placement, insufficient storage` — on whichever stream was provisioned last. The engine names the ceiling that did not fit and this field beside either of them, and adds the limit in force and what is already reserved on the standalone one, where this node can read them. Half of whatever is in force is what the state logs may reserve between them, a log that already exists counting at the ceiling it holds and the derived ceilings of the ones being created sharing what that leaves; the other half is for the streams that reserve nothing and grow — the mailboxes, the event log, the dead-letter stream, the memory changelog and every coordination bucket. |

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
