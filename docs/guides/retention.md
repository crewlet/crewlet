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
`fleet:operate`** — all of them but `verify --restore`, which opens a snapshot
directory on disk — reads included:
the `retention` question and every `/work/retention*` route take it, because a
map of which machine holds what and the value a reanchor must echo describe the
deployment rather than the company. The commands authenticate with
`CREWLET_API_TOKEN`, and a credential without the grant is refused `403`
naming it.

Four figures per domain, and no others:

| Figure | What it is |
|---|---|
| `bytes` | what the log holds now |
| `max_bytes` | the ceiling the **broker** is enforcing |
| `headroom_fraction` | how much of the ceiling is unused |
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
`bytes_per_day` on every alarm heartbeat (fifteen seconds), and fires when the
window no longer fits. Its detail says how long the ceiling holds at that rate;
the remedy is a bigger ceiling (see [Changing a log's
ceiling](#changing-a-logs-ceiling)) or a shorter `min_age`.

It matters most on the **identity log**. The mutation log and the knowledge
base grow with a corpus you can forecast, and the chart with headcount; the
identity log grows with how often people sign in, which is the rate an
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
`n/a` rather than zero, which is a different thing again. And a term that was
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

When a node reports `below_floor`:

1. **`crewlet retention snapshots`** — does any peer hold one, and how old?
2. If one does, the node fetches it on its own — at boot, or the moment its
   own heartbeat finds it below the floor while running: it pauses its
   appliers, adopts, moves its consumers to the artefact's position and
   resumes, with no restart. It takes a **hold** on every domain's log first,
   which pins the trim for the duration of the transfer — so a join cannot
   race the trim that made it necessary. A running node that finds no donor
   stays as it is, refusing, and asks again on an interval that doubles up to
   five minutes.
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

A full log costs `linearizable` reads, because those append a barrier — which
is every seat tool read. `stale` keeps answering, so the dashboard and the read
API are unaffected. See [Read consistency](consistency.md).

The refusal carries **no retry hint**, deliberately: the only thing that frees
a byte is a fifteen-minute gated job, and the log is full precisely because
that gate is closed. A number here would be a promise the mechanism does not
make.

## Changing a log's ceiling

A log's Tier A ceiling (`stream.tracker_log_max_bytes`,
`stream.tracker_vectors_max_bytes`, `stream.pages_log_max_bytes`,
`stream.chart_log_max_bytes`, `stream.iam_log_max_bytes`) is not a live
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
reads refuse `stalled` with that reason, its seats move to a peer, and
`crewlet retention status` shows the domain as stopped. A checkpoint past the
log's end is caught the same way, as `wrong_stream`, because a position the
log has never reached is a position on another stream.

**A rebuild under a node that never restarts is caught too**, on the position
heartbeat: it reads the stream's state every ten seconds anyway, and the
creation instant arrives in that same answer. Nothing else can see it — a
rebuilt stream comes back at generation 0 counting from 1, so once it has
published past the node's checkpoint every sequence term reads healthy while
the node applies a different history into rows keyed by the old one. The node
refuses `wrong_stream`, gives up its seats, and logs both instants.

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
- **Purging** removes the rows, on every node as each one reaches the record.

```
crewlet work purge <item> -reason "why" -confirm <item-key> [-op-id ID]
```

`<item>` is the task's key or its id. The confirmation is the task's **key**,
and the node checks it against the task `<item>` resolves to, destroying
nothing on a mismatch: the id may be what is on the command line, so repeating
it would confirm nothing, while the key has to be looked up — which is the point
of asking. There is no project to name: the task's own row says which project
the record is filed under. The **reason is required** because it is the
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

What it prints deliberately gives **no time guarantee**. An offline or evicted
disk retains its copy until replay, adoption, replacement or destruction, and
there is no duration to state. Saying "within 24 hours" would be a promise the
architecture cannot keep.

## The configuration archive, and the one thing a purge cannot reach

Every node keeps its **own copy of every configuration revision it has ever
met**, in `company_config`, and until this release nothing deleted from it.
One row per config write, per node, each holding the whole document.

Two things now bound it.

**The revision sweep** is part of the ordinary maintenance tick and runs on
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

**`crewlet config scrub`** is the one-time erasure for what is already in the
archive. The org chart used to live inside the company document, so a human
seat's `email` and `contact` account ids are in every revision that ever
carried them — on every node, in every backup, in a table nothing deleted
from. Removing the seat never reached it: the removal writes a *new* revision
and every older one still holds them. Revisions written after the [chart moved
onto its own log](../concepts/configuration.md) carry no chart at all, so
nothing new enters the archive.

```
crewlet config scrub -dry-run     # which revisions hold personal data
crewlet config scrub              # erase it from every superseded revision
crewlet config scrub <revision>   # or from one
```

Three things about it are not negotiable:

- **It refuses the active revision.** The fleet is serving that document and
  every node is holding it; rewriting it underneath them would be a
  configuration change nothing activated — no epoch, no apply, no event. To
  take an address out of the *live* company, edit the company; that writes a
  revision the scrub can then reach.
- **It is this node's copy only.** Run it on every node. Backups taken before
  the run still hold the original revisions, and nothing here reaches them.
- **It is not reversible.** The field is replaced with `__scrubbed__`, which
  is deliberately not the `__redacted__` a config read writes over a
  credential: that one means "this value exists and you may not see it" and is
  restored from the row behind it, and this one means the value is gone.

A revision stays immutable as a *configuration* and stops being immutable as a
copy of somebody's personal data. So **a `crewlet config diff` across a scrub
shows the tombstone**, which is a change rather than damage; the row carries a
`scrubbed_at` stamp and the run writes a `config_revision_scrubbed` audit
event recording the revision and how many fields went — never which, and never
what they held.

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

That horizon belongs to the **domain** rather than to the framework, and a
domain may declare a **shorter** one for a subject kind whose writers never
re-ask late. Every domain takes 30 days for its ledger as a whole, because the
client that re-asks is the same for all of them — and the identity domain keeps
its **session** subjects' rows for **one hour**: a row per sign-in and per
sign-out, whose operation ids nobody re-asks after the request that wrote them
(a sign-in is answered inside its request; a sign-out, an administrator ending
somebody's session and the deactivation probe each re-decide rather than
re-ask). Kept 30 days they were the busiest thing that domain wrote, for a
question nobody asks after an hour. The hour is the publisher's own
five-second resolve budget with a wide margin, and it is its own maintenance
job, `iam_ops_session`, so what it deletes is counted apart.

Each domain's **arbitration anchors** — one row per subject any writer has ever
contended on, holding what that subject's last record expects — are swept at
the log's own **published trim floor**, and never on a clock. An anchor above
the floor has to stay however old it is: read as absent, it hands the next
writer an expectation of "this subject holds nothing", which the broker refuses
for ever against a subject that does. Below the floor there is no record left
to replay, so there is nothing left for the anchor to be the anchor of. Two
nodes reading their own wall clocks would delete different rows, which is why
this one term is a position and not a duration.

The **authentication trail** — `iam_history`, the identity domain's record of
who did what to whom — is the one table in any domain with **two** horizons,
and a `class` column the engine derives from the operation is what separates
them. A **change** (somebody suspended, an access granted, a person removed) is
an audit asked a year later, and in several jurisdictions one that must be
answerable; a **session** (who signed in on Tuesday) answers an investigation
that is days old, and is a record of a person's working hours for as long as it
is kept. Keeping the second as long as the first would be storing more about
people than there is a reason to.

The trail's sweep is the one sweep in the engine that is **a record on the
log** rather than a local delete. The rows are identity-claimed, so a node
sweeping on its own clock would hold different bytes from its peers, and "N
byte-identical copies" would quietly become a claim about how synchronised
their clocks were. The record names *positions* the publisher resolved once —
"every change below this one, every session record below that one" — and one
instant, also read once, against which the same record collects the rows that
are **over** rather than old: sessions that ended or passed their absolute
deadline; invitations and bootstrap codes that expired unredeemed **or were
redeemed**; and credentials — passwords, provider links, machine tokens — that
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

**A sweep record carries a version, and an older node waits for it.** Collecting
what was spent is the second version of the sweep record. A sweep deletes by a
rule every node evaluates for itself, so a node running a build that did not
know the new clauses would delete less than its peers from the same record —
and the copies would stay different after the upgrade, because a record is
never applied twice. So an older node **defers** a version-2 sweep, holds back
later identity records for the same bucket behind it, and applies all of them
once it is upgraded. During a rolling upgrade that is a bucket's worth of
people whose changes reach that node late, and never a node that disagrees.

The identity domain's **operation ledger** keeps the 30-day horizon every
other domain's does, and one hour for its session subjects, which is not a
contradiction with the two above: a ledger answers "did my write land" and is
measured against the longest a client will retry, where a trail answers "what
happened" and is measured against an audit obligation. It is swept like every
other domain's — per node, by the maintenance sweep — because each node owns
its own copy of it.

### The identity duties

Five things keep the identity estate honest, and four of them are fleet
singletons, each on its own lease so that a flap on one costs that one an
interval rather than all of them. Each of the four runs once as soon as a node
claims it — so a restored node names a duplicate the moment it is back rather
than an hour later — and then on its interval.

| Duty | Interval | What it does |
|---|---|---|
| `iam_sweep` | 1 hour | Resolves `api.auth.audit.changes` (400 days) and `api.auth.audit.sessions` (90 days) to positions and publishes one sweep record for each bucket that is **due** — one holding something at least a day past its horizon. The day of slack is what bounds the log: a bucket is swept at most about once a day, so the sweep adds at most 64 records a day however often it runs. |
| `iam_deactivation_probe` | `oidc.deactivation_probe` (1 hour) | Asks the identity provider about every live provider session, with the refresh token kept when the person signed in. Only on a deployment with an `oidc` block. |
| `iam_key_shred` | 15 minutes | Destroys the key of anybody removed whose key outlived the removal, and every key nobody owns once it is an hour old; collects the refresh token of every provider session that is over. |
| `iam_claims` | 1 hour | Logs every duplicated claim and every orphaned reservation at WARN, every tick it stands, naming each holder by id. A login or a seat is logged as it is; an address by its kind alone, never by its keyed blind, which would be a stable pseudonym for it in every system your logs are shipped to. |

The fifth is the operation ledger's sweep, which runs in the ordinary
maintenance tick on every node.

A node arms these only if it runs the identity domain **and** the `workers`
role: an ingress-only node applies the identity log but claims no singleton, so
it arms none rather than running loops its roles would refuse on every tick.
`identity_duty_seconds` on [`GET /health`](../reference/api-endpoints.md#the-health-envelope)
names the ones a node armed and each interval — the only way to tell a duty
that is running and finding nothing from one that was never armed.

**The key duty exists because a removal is a key deletion.** The removal's rows
commit first and the person's key is destroyed after, on every node that
applies it — so a coordination store that blinks at that instant leaves a row
that says *removed* and a key that still exists. Until the key goes, the
person's name and address are readable from every backup taken before the
removal. Fifteen minutes is the maintenance sweep's own interval: a pending key
exists only because a write failed, so the useful retry is "soon after the store
is back", and every minute is a minute somebody off-boarded is still readable.
`crewlet iam check` names each one as `removal_key_live` while it waits.

**It also destroys a key nobody owns.** An enrolment mints a person's key before
it claims their address, and an invitation mints its own before it publishes —
so an enrolment or an invitation refused on its address leaves a key for
somebody who never existed, and an invitation the sweep collects leaves its
key behind. Nothing else would ever name those keys, and what they sealed would
stay readable from every backup for the life of the deployment. The duty
destroys one only once it is **an hour old** — the gestures that mint a key
finish within one request, so an hour is long past any of them — and only on a
node whose rows have **applied everything the identity log held** when it asked,
because on a node that has not, somebody whose enrolment has not been applied
owns nothing there either, and destroying their key would be an irreversible
shred of a person nobody removed. *Applied* is the word that matters: a node
holding a record it cannot apply yet — a newer build's during a rollout, or one
signed under a keyring key it was not restarted with during a key rotation —
has consumed the log past that record while its rows lack it, and it judges no
unowned key until it can apply it. Such a node, and one that is simply behind,
says so (`iam_keys_unjudged`) and leaves them for a pass that can; a removal's
key does not wait for that, because a removal is definitive wherever it has been
applied. `crewlet iam check` names
each unowned key past the hour as `key_unowned`.

**And it collects every refresh token whose session is over** — by logout,
expiry, a revocation or a session invalidation — whether or not a provider is
still configured. That used to be the probe's, and the probe runs only while an
`oidc` block does: a deployment that dropped its provider kept every token, a
live credential at that provider, for ever. A token whose session this node has
not applied is kept — its row is missing because the session has not arrived
there, or because it arrived in a record this node set aside (a newer build's,
or one signed under a keyring key it was not restarted with) — since collecting
it would leave the probe nothing to ask with while the session is still live.

**The probe needs the refresh token, so a provider sign-in keeps it.** It is
sealed into the company's secret store beside the session it belongs to, and a
sign-in whose token cannot be kept there is refused with a 503 rather than
admitted: a session the probe cannot ask about is one nobody can end from the
provider before its absolute deadline. `invalid_grant` ends the session as
`idp_revoked`; every other failure — an unreachable provider, a 5xx, a timeout
— is *unknown* and ends nothing, because reading an outage as a deactivation
would sign the whole company out during somebody else's incident. A rotated
refresh token is recorded, or the next pass would present one the provider had
retired and read the refusal as an off-boarding. The probe drops the token of
a session it ended itself; a token whose session ended any other way is the key
duty's to collect.

**One session the probe cannot handle is one session, not the pass.** A token
it cannot read — one a newer node wrote in the middle of a rolling upgrade — is
logged by name (`oidc_probe_session_skipped`) and left alone, and every other
session is still asked about; a close that did not land, or a rotated token that could not be
recorded, is logged the same way and tried again next pass. Such a pass ends
with `iam_probe_pass_partial` at WARN, counting what it checked, ended,
skipped and failed. Only a pass that could do nothing at all — no provider
metadata, a directory it could not read — logs `iam_probe_failed`.

**The probe's interval can exceed what a lease may live.** A lease is capped at
three hours and every singleton keeps three claims to a lease, so a duty whose
interval is longer than an hour claims hourly and runs every so many claims —
a `deactivation_probe` of `90m` is two claims of 45 minutes, and the interval
the operator set is the interval kept.

**The claim report never repairs.** The broker cannot put two people on one
address, one login or one seat, but a restore or a reanchor can, and the
estate has no unique index to refuse it with — a violation inside an apply
would stop that node's log for good. So a duplicate is *reported*, with every
person holding it, and an operator decides who keeps it. An orphaned
reservation — claims an enrolment took before it stopped, older than an hour —
is released by removing its id.

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
   threshold is 10 %, which is the first decile clear of it — and the vector
   log is compacted, so `log_ceiling_short` measures no rate for it at all.
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
| broker storage | `stream.store_max_bytes` **unset** on a host running one engine; **divided** where several share a filesystem | Unset, the embedded broker takes three quarters of `stream.store_dir`'s free space, measured once at boot — right when it is the only tenant. Free space bounds the *sum* of the engines on a volume, so two that each size themselves from what they can see over-commit it, and the failure is `insufficient storage resources available` — or, on a fleet, `no suitable peers for placement, insufficient storage` — on whichever stream was provisioned last. The engine names the ceiling that did not fit and this field beside either of them, and adds the limit in force and what is already reserved on the standalone one, where this node can read them. Half of whatever is in force is what the state logs' derived ceilings may reserve between them; the other half is for the streams that reserve nothing and grow — the mailboxes, the event log, the dead-letter stream, the memory changelog and every coordination bucket. |

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
