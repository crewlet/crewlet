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

Four figures per domain, and no others:

| Figure | What it is |
|---|---|
| `bytes` | what the log holds now |
| `max_bytes` | the ceiling the **broker** is enforcing |
| `reserve_bytes` | on the tracker and pages logs, the top of that ceiling kept for gate records — see [the gate reserve](#the-gate-reserve) |
| `headroom_fraction` | how much of the ceiling **ordinary writes** are held to is unused: `max_bytes` less `reserve_bytes` |

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

**A floor belongs to one generation of its log.** Right after a
[reanchor](#re-anchoring-a-recreated-or-restored-log) the published floor, its terms and
its blocking term describe the stream that was left, so the status shows that
domain with no floor, no terms and not blocked until the trim's first tick on
the adopted stream — exactly as the write fence and readiness read it. The
other domains' rows are unaffected.

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
| `log_diverged` | the log holds, at this node's checkpoint, another record than the one it consumed there — a broker restored from an older copy and written past these rows — so they are a history the log does not continue |
| `recent` | the newest artefact is younger than `snapshot_interval` and names every domain at the generation this node is on — but see below |
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

**An adoption carries the operation ledger with it.** The ledger — the table
that says which operations have already been applied — travels inside the
snapshot, so the adopted node answers a retry of anything its donor applied
from it, and files work that was queued before the join like any other. The one
exception is a donor on an **older build**, which scrubbed its ledger out of the
snapshot: the joining node records the join as the point before which its
ledger may have lost rows, so a retry of older work there — a turn re-run
whose work began before the join, an operator repeating an older `-op-id` —
answers `unknown` (and logs `statelog_write_unvouched`) rather than risk
applying it twice, until it is retried on a node that did not adopt from the
older peer. Upgrading the fleet ends it. See [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted).

### A node a peer re-anchored past

When one node [re-anchors](#re-anchoring-a-recreated-or-restored-log) the
tracker's or the knowledge base's log, it opens the next generation from its
own rows — and every other node's rows are then a history the log no longer
continues from. Nothing on the log can bring them level, so each of those nodes
adopts a snapshot from a node in the new generation, **on its own**:

- It learns of the new generation from the re-anchoring node's position row,
  which that node publishes the moment the reanchor completes, on its next
  heartbeat — or sooner, from the reanchor's own record arriving on its log,
  which stops its applier before anything of the new generation is applied.
- From then until the adoption lands it refuses that domain's reads and writes
  as `wrong_stream`, logs `statelog_generation_passed` naming both generations,
  and gives up its seats — whether or not its own readings of the log look
  wrong. On a broker restored from an older copy, a node whose checkpoint was
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
peer re-anchored past](#a-node-a-peer-re-anchored-past)). The row itself stays:
a readmission is judged by it.

The trim counts nodes **per log**, so an eviction is a record on every log it
counts nodes on: the tracker's log and the pages log. (The vector log counts
nothing — a node behind on it is a coverage figure — and gets none.) Every
node running the state log can make the gesture, whichever backends the
company uses: a company on an external tracker still runs both logs.

It is **judged once, before anything is written**: a node that still holds a
live presence lease is refused, because it is still reaching the fleet and
almost certainly running — an eviction would drop everything it writes and
move its seats. Stop it and wait for its `LIVE` column in `crewlet retention
status` to read `no`; `-force` overrides the refusal for a node wedged in a way
that still renews its lease. A refusal writes nothing to either log. If this
node cannot read the presence leases at all — a coordination fault — the
eviction is refused as one nobody could judge, and says so; `-force` takes it
past that too, since the leases are the only thing the judgement reads, and
the node's log records that the eviction was forced unjudged. A node id no node
could run under (the [`node.id`](../concepts/configuration.md#nodeid) rule:
alphanumeric first, then letters, digits, `.`, `_` or `-`, at most 64
characters) is refused before anything is judged.

Past the judgement the record goes to each log in turn, and each answers on
its own line with the [three-valued outcome](replication.md#a-write-has-three-outcomes)
every write has — `applied`, `pending` or `unknown` — or `not written` with the
reason that stopped that log. A log that answered holds its record whatever
the other did, and the gesture runs to its end **whatever happens to the
command**: once the first record is about to be written the node finishes the
gesture under its own one-minute budget, so a dropped connection or a client
timeout does not leave the node evicted on one log and counted on the other.

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
dropped, the wait ran out — still prints the `-op-id` that finishes it.

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
  (`-url`).
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
made once for every log before either is written: its last published position
in the tracker's log and the pages log, against the higher of each log's
published floor and its first surviving sequence. A node that has never
published a position is judged as holding nothing. Nothing is written to either
log on a refusal, and the node needs nothing from you but time: start it if it
is not running, and it [catches up on its own](#the-join-runbook) — replaying
what the log still holds, adopting a snapshot where it does not. Readmit it
once it has applied every record up to the one just before that bound: in
`crewlet retention status`, its `SEQ` for each of those domains has reached one
less than the higher of the domain's `TRIM FLOOR` and `FIRST`. The refusal's
own `floor`, `first_seq` and `generation` are the numbers it compared — right
after a [re-anchor](#re-anchoring-a-recreated-or-restored-log) the domain shows no
`TRIM FLOOR` until the trim's first tick on the adopted stream, and the bound
is `FIRST` alone — and running the readmission again is always safe, since a
refusal writes nothing. A readmission that reaches one log and not the other is finished the
way an eviction is, with `-op-id`.

The refusal is not what keeps the fleet's data safe, and being refused is not
an emergency: a node below the floor is refused every write that assumes a
history it does not hold, by its own fence, whether it is readmitted or not.

## A full log refuses; it does not shed

At its ceiling the log **refuses appends**, naming `log_full`, the ceiling, and
the verb that raises it. It does not silently delete old records to make room:
shedding a record no node has applied is exactly the loss the whole gate
exists to prevent.

A full log costs `linearizable` reads, because those append a barrier — which
is every seat tool read. `stale` keeps answering, so the dashboard and the read
API are unaffected. See [Read consistency](consistency.md).

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

So on the two logs that carry gate records, the tracker's and the knowledge
base's, **ordinary writes are refused at a soft ceiling** a sixteenth below
the broker's, and the top sixteenth — the **gate reserve**, `reserve_bytes`
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

| | tracker and pages logs | vector changelog |
|---|---|---|
| ordinary writes and barriers refused at | the ceiling less the reserve | the ceiling |
| gate records refused at | the ceiling | — (it carries none) |
| `headroom_fraction` measured against | the ceiling less the reserve | the ceiling |

A sixteenth is sized so the reserve holds however the fleet's writes race:
each node reads the log's usage **after** it starts an append and counts its
own appends still in flight, so what can land past the soft ceiling is only
what other nodes have in flight at that instant — at most one maximum record
(8 MiB) each. At the smallest ceiling a log may have, a gibibyte, the reserve
is 64 MiB: seven other nodes' maximum records with room to spare for the gate
records themselves, and every larger ceiling holds more. While a rolling
upgrade runs, a node on an older build keeps no reserve, so the reserve
holds once every node counts its own.

If a gate record is refused even there, the refusal says so and points at
`crewlet retention set-capacity` rather than suggesting a retry; the
[`log_headroom`](../reference/alarms.md) alarm, which fires at a tenth of the
soft ceiling left, is there so that never happens.

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
  target whose soft ceiling — the target less its
  [gate reserve](#the-gate-reserve), on the tracker and pages logs; the whole
  target on the vector changelog — is at or below what the log already holds
  is refused, naming both and the least target that would do, because that
  ceiling would refuse every ordinary append the moment it applied. So is a
  target under a gibibyte, the floor Tier A holds every log to and the one the
  reserve is sized against. Anything else is fair, including a target under
  the current ceiling, which is how a log created larger than its budget gives
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

## Re-anchoring a recreated or restored log

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
it (`log_diverged`), and says so on its position row (`log_diverged`). That
refusal does **not** lift on its own: a member that has not caught up answers
that it holds no record at a sequence, never with another one. The node finds
it the same way when it **boots** on a log already written past its rows — its
checkpoint is then not past the end at all, and the record is the only thing
that shows it — and when a broker is restored under it while it runs. It is
compared for equality, so no clock is ever ordered against another.

A checkpoint written before checkpoints named their record — or placed by a
reanchor where the log held none — names nothing, and the next batch the node
commits names its own. An **idle** domain commits no batch, so the node names
such a checkpoint itself, at boot, from what it kept when it consumed that
record and **never** from the log's record, which is exactly the thing in
question: its operation ledger's row at the checkpoint (by the record's own
instant, or — for a row older than that — by the operation being the one the
log's record carries), or its retained copy of a record it could not decode. It
writes the name back and logs `statelog_checkpoint_named` with the evidence
(`ledger_instant`, `ledger_operation` or `retained`), and compares from then on
like any other. A ledger row there naming **another** operation than the log's
record carries is a divergence, logged as `statelog_checkpoint_other_operation`
and then refused like one. Where nothing names it — the ledger's retention swept
the row, or the record there wrote none (a read barrier, a repeated operation, a
record a gate kept out of the rows) — the node logs
`statelog_checkpoint_unnamed` (`WARN`) once and carries on: refusing would stop
every idle domain on its first boot of this build over a question that almost
always has the ordinary answer, but it is the one state in which a restored log
written past these rows goes unnoticed until the next batch names a record. If
the broker behind such a node was restored from an older copy, re-anchor or
replace it rather than waiting for that batch. A checkpoint the log holds no
record at is neither named nor said, since there is nothing to compare it with.

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
vouch where the ledger has lost the record's row: to the ledger's thirty-day
sweep, or with a snapshot from a peer on an older build, which arrived without
its ledger. The ledger records how far back it may have lost rows, and when the
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
`statelog_stream_recreated` with both instants, gives up its seats, and refuses
every read and every write of that domain with `wrong_stream` until that stream
is re-anchored, and it applies nothing from the rebuilt stream into rows keyed
to the old one.

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
# prints the live stream's created_at and which case it is, and refuses

crewlet retention reanchor -stream CREWLET_TRACKER_LOG -confirm 2031-04-02T03:00:00.418226517Z
```

The confirmation is the **live** stream's own `created_at` — the instant the
`wrong_stream` refusals name as the broker's — exactly as the verb prints it,
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
vectors — has its own stream and its own generation, and a reanchor moves only
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
the domain's own generation record to the adopted stream (the tracker's and the
knowledge base's each keep one, applied as `tracker_log_generations` and
`pages_log_generations`; the vectors keep none); reads the stream's instant
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

For the tracker and the knowledge base it refuses while any peer has already
re-anchored the stream — a peer at a later generation of it — naming the peer:
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
stand further along the log than its checkpoint. Every node publishes, beside
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
may already have destroyed — and when it did, the retry answers `applied`, at
the first purge's position, rather than finding the task gone and refusing.
Pass it back exactly as printed: the id carries the
instant it was minted, which is what a node judges the retry by once its
operation ledger may have lost the first purge's row — to the ledger's
thirty-day sweep, or to a snapshot adopted from a peer on an older build —
and there it answers `unknown` again rather than purging twice. An id of your
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
than `applied`, which is the honest answer once the row is gone — and it is
never applied a second time: each pass records the cutoff it deleted before,
and a write whose operation was minted earlier than that, with no row left to
say whether it already landed, is answered `unknown` without being published
(see [Replication](replication.md#what-a-retry-is-judged-by-the-instant-its-operation-was-minted)).

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
