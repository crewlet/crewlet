# Backups & Restore

What durable state a Crewlet deployment holds, where each piece lives, how to
back all of it up so a later restore actually works, and which losses are
survivable without one.

The short version: **`crewlet backup` takes a verified copy of a running
node**, without stopping it. It goes through the engine because it has to —
the store file is locked to that process and the embedded broker binds no
socket, so nothing outside the engine can read either estate. The cold
runbook further down remains the belt to that braces.

```bash
crewlet backup -dir /var/backups/crewlet/2026-08-30T18-00
```

```
Backup written to /var/backups/crewlet/2026-08-30T18-00 on node-0 in 1.412s

WHAT                          FILE                                       SIZE       CONTENTS
store (node)                  store.db                                   252.0 KiB  20 migrations
store (replicated)            store-replicated.db                        1.2 MiB    3 migrations
stream CREWLET_AGENT          streams/CREWLET_AGENT.snapshot             1.1 KiB    5 messages
bucket crewlet_token_windows  streams/KV_crewlet_token_windows.snapshot  512 B      3 messages
…
```

**It needs `admin`.** The command authenticates with
`CREWLET_API_TOKEN` like every command that talks to a running node, and
`POST /backup` refuses a credential without it with `403` naming it —
copying the node's whole durable state, every credential and every seat's
memory included, to a directory the caller names operates the deployment
rather than reading the company.

The path is on the **engine's host**, not yours — this writes files where the
node runs and downloads nothing. Any node produces one, whatever its roles:
every node holds its own store and, on the embedded topology, its own broker.
A node that dialled an **external** NATS cluster is the exception, and it says
so: its copy carries the store estates alone, and the stream half is backed up
at the cluster (`nats account backup`) from the same moment.

## What state exists, and where

A deployment's durable state lives in six places:

| Estate | Where | What it holds |
|---|---|---|
| **The node's own store file** | `store.path`, with its `-wal` sidecar | The seat's memory — diary, episodes, counterparty profiles, synthesized skills, onboarding markers, the [conversation ledger](../concepts/conversation-sessions.md) — which is also [replicated onto the stream](../concepts/seat-ownership.md#a-seats-memory-follows-it), so this file is a cache of it rather than its only copy; and, held here **only**: the audit event log (30 days), scheduled-run history, the company-config revision history, the [secret store's](../concepts/secret-store.md) bootstrap rows, and this node's own record of any snapshot it has adopted |
| **The replicated estate** | `store.replicated_path`, with its `-wal` sidecar | Everything a state log's applier derives from the fleet's own records — the work tracker, the knowledge base's pages, the embeddings that search them, the [identity directory](../concepts/identity-and-access.md), the company's [git repositories](../concepts/code-host.md) and every node's [daily usage](replication.md#two-compacted-domains-the-embeddings-and-each-nodes-day) — together with the checkpoint that says how far this node has applied. Derivable by replay **only while the log still holds the records**: past the trim floor, a node with no copy of this file adopts a peer's snapshot instead |
| **The object store** | `store.objects`: the `OBJ_crewlet_files` stream on the broker's members (`nats`, the default), or an S3-compatible bucket (`s3`) | The bytes of the company's [files](../concepts/object-store.md), one object per upload. **One store the whole fleet shares**, not a directory of any node's: on `nats` a stream like any other, on `s3` somebody else's bucket. The rows naming the files are in the replicated estate |
| **The stream estate** | `stream.store_dir` per embedded member, or the external NATS cluster | Agent mailboxes (unacked in-flight work), the shared event and config streams, one ordered **log per state-log domain** — which is the record of truth the file above is derived from — and every [coordination](../concepts/coordination.md) KV bucket: seat, presence and duty leases and fencing epochs, the activation pointer with the current company payload, the completion ledger, delivery dedupe, budget counters, scheduled-fire claims, detached sandbox-run records, the sealed credentials |
| **Tier A, on disk** | `crewlet.yaml` and the environment it reads | The keyring (`CREWLET_SECRET_KEY_*`) — the sole root of trust for everything sealed — plus API tokens and any NATS credential/TLS files |
| **LLM CLI homes** | Each `providers.llm` entry's state directory on the engine host (`state_dir`, or one per key under `CREWLET_LLM_CLI_HOME`), each CLI's files under `<state_dir>/<agent>` | The coding CLIs' logins and per-seat homes (a login is portable via `crewlet llm export`) |

Classify before you size the job:

- **Rebuildable, safe to lose:** every TTL'd coordination bucket — the rate
  valve, delivery dedupe and node status regenerate, and the
  [rebase records](../concepts/coordination.md#what-the-fleet-shares)
  re-form at the next attempt, at the cost that a turn whose work began more
  than twenty-nine days ago, retried after the loss, writes again what its
  earlier attempt wrote — and the leases and epochs
  *provided the whole fleet cold-starts together* (they re-form from nothing).
- **Held twice:** the learning tables and the conversation ledger. Each row is
  also on the memory changelog, which is how a seat's memory follows it to a
  new node — so a store file lost with the stream estate intact costs at most
  the last sync cycle, and the seat re-hydrates the rest on its next
  acquisition.
- **Kept by the store, as many copies as it keeps:** the object store's
  objects. On `nats` they are a stream at `stream.replicas` copies, so they
  survive exactly what the logs survive, and a member that fails is caught up
  by the broker; on `s3` they have whatever durability the bucket's provider
  promises. An object the store has lost or changed anyway is gone, and the
  collector's daily audit raises `objects_missing` naming the file. Only a
  backup covers that case.
- **Derived, and rebuildable *only within the replay window*:** everything in
  the replicated estate. A node that loses that file replays the domain logs
  from the beginning and arrives at exactly the same rows — but only if the
  logs still hold them. Past the trim floor the records are gone, and the node
  fetches a peer's verified snapshot instead, which it does automatically at
  boot. That fallback needs a **peer**: a single-node company that loses this
  file and whose logs have been trimmed has lost the trimmed history, which is
  the case `retention.backup_max_age` exists to keep from arising.
- **Authoritative, with no other copy:** the event log's history, the config
  revision history, the sealed credential bucket, the budget counters, and
  each detached sandbox-run record, which is the only thing that knows a
  billed box exists.

## What `crewlet backup` produces

A snapshot artefact is adopted **wholesale**, and so is a backup read back:
the manifest names a position per domain, and a build that registers a domain
the artefact never named refuses it rather than concluding it is caught up on
that domain from a checkpoint the file has no rows for.

One directory, and the **manifest is the claim**: a directory holding
`manifest.json` is a complete backup, one without it is the debris of a run
that did not finish. Nothing else in the directory says so, which is exactly
why the manifest is written last.

```
2026-08-30T18-00/
├── manifest.json                          what was captured, from which node
├── store.db                               the node estate, self-contained
├── store-replicated.db                    the replicated estate, self-contained
├── objects/                               s3 only: every object a live row of that
│   └── files/                               copy names, and every object a record a
│       └── 0199a3c2-…                       restore replays from it names — one file
│                                            per object, under its key: the bucket's
│                                            layout under its prefix
└── streams/
    ├── CREWLET_AGENT.snapshot             a mailbox stream
    ├── KV_crewlet_secrets.snapshot        a coordination bucket
    ├── OBJ_crewlet_files.snapshot         nats only: the company's file objects
    └── …                                  one per stream and bucket found
```

What is worth knowing about it:

- **Each store copy is taken with `VACUUM INTO` and then verified** — reopened,
  integrity-checked, its schema compared against the database it came from,
  and a sha256 of the finished file recorded in the manifest — before it is
  renamed into place. A copy that will not open is a failed backup rather than
  a surprise on the worst day of the deployment's life; the digest is what
  tells a copy that was truncated in transit from one that was bad when it was
  made. Each is self-contained: no `-wal` travels with it.
- **Each store copy is one moment**: the database as it stood when its copy
  began — every write that had committed by then is in it, and nothing
  committed while it ran is. The engine holds a read transaction open across
  the copy to make it so, because the database engine's own `VACUUM INTO`
  holds none: without it a copy could miss writes that had already committed,
  or hold two halves of one write. While a copy runs, the store's write-ahead
  log is not checkpointed past its moment, so the `-wal` beside the live file
  grows by whatever the node commits meanwhile and is folded back in by the
  first checkpoint after the copy ends. **How much** is the node's write rate
  times the copy's run, and the run is the file's size over the rate the copy
  reads it — measured at about 104 MB/s on the engine's own driver, so about
  seven minutes for the year-five estate of ≈ 44 GB below, over which a node
  writing 1 MB/s to its log holds back about 420 MB of it, and one writing
  2.5 MB/s a gibibyte, where [`wal_large`](../reference/alarms.md) fires: it
  names the copy holding the log and says the copy's end folds it back in.
  Each copy measures what it held back, and **before it copies anything a
  backup checks** that each database's own volume has room for what the last
  copy of that file held back, scaled to the file as it stands, and refuses
  otherwise, because that log grows on the volume the node commits to and the
  node would stop committing once it filled. `volume_low` counts the same log
  in the room it wants beside a second copy. The first copy after the node
  starts has nothing measured to go on, and nothing foresees a burst of
  writes past the rate the last copy saw: `wal_large` and `volume_low` name
  either while it happens.
- **A node is two databases, and a backup carries both.** The node estate holds
  the audit log, memory, the config revisions and the secret bootstrap; the
  replicated estate holds everything a state log's applier writes. They are
  separate files because a snapshot for a joining node is a copy of the second
  one alone, and it must not carry the donor's audit log or the bootstrap half
  of its secret store. Restoring one without the other gives a company whose
  halves are from different moments. The replicated estate is something the
  node **holds** — every node with the `data` role holds it from the moment it
  starts, whether or not its company runs the native tracker or knowledge base,
  because the rows its log derived stay on disk either way — and the backup
  copies what the node holds rather than whatever happens to be open. An
  estate the node holds that is closed at that instant (an adoption
  replacing its file, or one whose reopen failed) **refuses the backup, naming
  it**, rather than writing a manifest without it; take it again once the node
  reports the estate serving. A node without `data` holds none and backs up
  its own file alone.
- **The objects come from the object store, not from this node's disk.** The
  rows that name them are the declared ones — a project's files
  (`tracker_files`) and the pack bundles the company's
  [git repositories'](../concepts/code-host.md) pushes stored
  (`code_packs`), both live references — a repository's rows, its refs and
  settings among them, are in the replicated store copy like every other
  replicated row, and its packs ride the backup as these objects. On
  `nats` they are already a stream — `OBJ_crewlet_files` — and the stream
  snapshot the backup takes anyway carries them, so no object is copied on its
  own: the manifest's `objects.stream` names the stream. Once the snapshot is
  taken, the backup asks the stream's leader about every object a live row of
  the replicated copy names, **thirty-two at a time** — the collector's audit
  asks the same question at the same depth — and `objects.objects` and
  `objects.bytes` count the ones it holds: a key is never reused, so an object
  the store holds after the snapshot finished is one the snapshot holds too. On
  `s3` the backup reads every object a **live** row of the replicated copy names
  — read from the copy itself, for the position's reason below — from the
  bucket, **thirty-two at a time**. A first copy is bound by how long each
  request waits for its first byte rather than by the link: S3 answers in about
  100 to 200 ms, and thirty-two out keep a 1 Gbit/s link busy at the lower end —
  sixteen left it a third idle, and the large model's first copy (about 2.1
  million objects, 1.15 TB) took about five hours where thirty-two took about
  three on the host it was measured on — and about three fifths of it at the
  upper end, where that first copy takes about four hours (worked out rather
  than measured). Thirty-two out stay under a twentieth of what S3 serves one
  prefix, so the company's own downloads keep the rest. Each is streamed
  to disk and checked against its row's size and SHA-256 as it is written,
  and lands under `objects/files/<key>` — the
  bucket's own layout under its prefix — first under a temporary name, renamed
  only once it has checked out. Either way the copy's rows are read **a page at
  a time**, in key order across every table, while the objects of the page
  before are fetched: the backup holds a page of references, never the
  company's whole inventory. An object only a
  [retired or derived](../concepts/object-store.md#live-retired-and-derived-references)
  row names is neither copied nor asked after: a restore brings the row back
  without it, and nothing is owed it — a retired object was let go, and the
  engine builds a derived one again.
- **The files written while the backup ran come too.** A restore brings the
  store copy back and replays each log from the copy's position to the last
  record its snapshot captured — the snapshot is a copy of the stream as it
  stood when it was asked for, however much is appended while it transfers — so
  a file uploaded and named after the copy was taken is a file the restored
  company has. Once the streams are snapshotted, the backup reads each log that
  writes a table of files from the copy's position to that last record, and
  the records the copy's applier had held back to apply later, and carries
  every object those records name: on `s3` copied like the rest, thirty-two at
  a time; on `nats` asked after like the rest, since the `OBJ_crewlet_files`
  stream is snapshotted **after every other stream** and so holds every object
  a captured record names. `objects.replayed` counts the ones only those
  records name. A record this build cannot read that may write a file — a
  newer build's, during a rolling upgrade: a file record, one of a kind this
  build has never heard of, or one whose writer said it may touch a whole
  project — **refuses the backup** (`409 record_unreadable`), since it may
  name a file the backup would not carry: take it from a node running the
  newer build. So does a record missing from the log between the copy and the
  snapshot (`503 log_gap`: take it again), and a copy whose position is on an
  earlier instance of a log that has since been recreated (`409
  log_recreated`: take it once this node has re-keyed to the live log). A
  newer record whose writer said it touches no file — a task's, say — refuses
  nothing. Every record is read as the appliers read it, its signature
  checked under this node's `secrets.keys` first
  ([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)): one signed
  under a key this node does not hold refuses the backup (`409
  record_unverified`) — add the key and restart the node, or take the backup
  from a node that holds it — and one whose signature does not verify refuses
  it as bytes no node of the fleet signed (`500 backup_failed`, the reason in
  this node's log).
- **A backup takes what its previous one holds** (`s3`). A key is minted for
  one upload and never names other bytes, so an object this node's previous
  backup already holds is the same object: when that backup's directory is
  still on this host (the `dir` this node last announced), each object in it
  is **linked** into the new backup — copied where the filesystem refuses a
  link, another mount among them — and read back against its row's size and
  SHA-256 before it counts, so a copy that rotted in the earlier artefact is
  fetched again rather than carried forward. Only what is new since is read
  from the bucket. Each one is a complete file of the new backup, never a
  reference into the old one: shipping or deleting either directory leaves the
  other whole. The manifest's `objects.reused` and `objects.reused_from` say
  how many and from where.
- **An object the store has lost is recorded, an object it could not answer
  for fails the backup.** An object the store *answered* it does not hold, or
  holds at another size or under another digest than its file records, is
  gone whatever the backup does: the backup carries everything else, lists it
  in the manifest's `objects.lost` as `{"object", "named_by"}` — the key and
  the file, `PROJECT/path` — logs `backup_objects_lost`, and is announced like
  any other. Refusing it would refuse every later backup too, and with them
  the trim of every log in the fleet, over one lost file. The
  `objects_missing` alarm is what sends somebody to replace the file. Only
  what a restore names is lost: a file the copy names that a record after the
  copy replaced or removed — and whose old object the collector has since
  deleted — is owed by nothing the restore brings back, so it is not listed
  (`backup_objects_removed_since_copy` counts them). An
  object the store did not *answer* about is **asked again** — up to four
  times, a second, two and four apart, within the time one read of it is given
  — since a reset or a stall part way through one of millions of reads is no
  reason to refuse hours of copying (`backup_objects_retried` counts the ones
  that needed it). On `s3` each of those reads already asks a request the
  bucket refused again until the read cannot begin within a minute, so a
  bucket that fails one object every time spends that object's whole read
  time — about two minutes for a small file — on two such reads rather than
  four. One the store still does not answer for may be intact there,
  so it **fails the backup** (`POST /backup` answers 503
  `objects_unreachable`), naming it: take it again once the store answers. The
  **first** such object stops the backup, with nothing left in flight — it is
  refused for that one whatever the rest do, and a store that has stopped
  answering is not sent the rest of the company's files. The manifest's `objects` records
  `dir` or `stream`, `objects`, `bytes`, `replayed`, `reused`, `reused_from`
  and `lost`; a backup owing no file has none.
- **One backup at a time per node.** A backup asked for while another runs on
  the same node is refused (`POST /backup` answers `409 backup_running`) and
  writes nothing, its directory included: the trim hold a backup takes is
  keyed on the node, so two at once would share one pin, and the first to
  finish would release the pin the other's replay still needs. Schedule them
  so one finishes before the next starts.
- **Streams are enumerated, not listed.** A namespace stream is created on
  first publish and a coordination bucket's name depends on a configurable
  prefix, so what gets captured is what is actually there.
- **Every estate or none.** A failure anywhere leaves the directory without a
  manifest. A backup missing an estate is not a partial backup, it is an
  unrestorable one: the store alone loses every lease, ledger and credential;
  the streams alone lose this node's audit log, its scheduled-run history and
  the config revision history — everything the store holds that the memory
  changelog does not carry.

It is a copy of a **moment**, not an instant — the engine keeps working
throughout, and the pieces are separated by however long the copy took. The
store is copied **first**, and that order is now mandatory rather than
preferable.

The store carries a **position**: the tracker's rows are derived from an
ordered log by an applier that commits its checkpoint in the same transaction
as the rows, so a store copy is a claim about what has already been applied.

- **Store first** leaves the artefact holding a store at position *P* beside a
  log that has since moved past it. A restore replays the difference. The gap
  is bounded and replayable, and it costs a few minutes of work being applied
  twice — which is free, because the applier's guard is monotone in the
  position. The replay starts from the restored rows, not from the broker's
  record of what the node read: the node's reader of each log was copied with
  the streams, *after* the store, and may already have acknowledged past *P*.
  The node rebuilds such a reader at *P* when it boots and logs
  `jetstream_domain_consumer_rebuilt` (see
  [Replication](replication.md#a-node-resumes-from-its-rows-never-from-its-reader)).
- **Store last** would leave a store at position *P* beside a log whose newest
  record is *below* it. Every subsequent record then lands at a sequence the
  store has already marked applied, and the version-guarded write drops it
  **silently**. That is not a gap, it is a permanent hole nothing reports — a
  restored company quietly missing whatever was written during the copy.

So the order trades a bounded, replayable gap against a permanent, silent one.

**The gap is only replayable while the log still has the records**, and the
fleet's own trim deletes a record once every counted node has committed past
it. A backup is not a counted node, so two things close that window and they
are different kinds of thing:

- **A trim hold** is taken before the first byte is copied, at the position
  this node's appliers stand at, and released when the manifest is written. It
  is what makes the race not happen. It is *heartbeated*: a pin that outlived
  its owner would stop the trim for ever and the log would grow to its ceiling,
  so the fleet ignores a hold nobody has renewed. A node that cannot write the
  hold refuses the backup rather than taking one whose gap may be trimmed away
  while it runs.
- **An assertion** — the log's first surviving sequence must be at or below the
  copy's position plus one, checked per domain after the stream snapshots from
  bounds those snapshots already captured. It is what makes "restorable" a
  *checkable inequality* rather than a hope. A backup that cannot assert it
  writes **no manifest**, which is how a reader tells debris from a backup.

The manifest records the position **read from the copy itself**, not from the
live database: the checkpoint commits with the rows, so the position inside a
file is the only one that describes that file, and the applier ran throughout
the copy.

**Every backup taken over `POST /backup` is audited.** Once the copy begins,
the node writes a `backup_requested` event to its own event store — who asked
(the token's name, and the person it is bound to), which node, which directory,
and whether it finished — a failed run included, since it can leave files
behind. A directory refused before the copy began (relative, not empty, or one
the host cannot create or write because of the path itself) wrote nothing,
answers `400`, and is not recorded. A disk that fails or fills while the
directory is prepared is the node's failure rather than the path's: it answers
`500` and is recorded, and so is a backup refused because a database's own
volume has no room for the log its copy would hold back, and a refusal that
arrives once part of the copy is already in the directory. Filter the event log on `source=operator` to see it beside every other
change a person made through the engine; see
[the runtime audit](../reference/api-endpoints.md#the-runtime-audit-sourceoperator).

### From the dashboard

**Settings › Backups & retention** takes one too: **Take a backup** asks the
node serving the page (`POST /backup`) for a directory on **that node's host**
— nothing is downloaded — offering a fresh directory beside that node's last
copy when it has one. The copy can take minutes on a large store; closing the
dialog does not stop it, and the outcome arrives as a notice either way.

The same screen shows what the fleet has backed up
([`GET /backups`](../reference/api-endpoints.md#get-backups)): each owner's
newest copy — its directory, size, reach and whether the trim counts it, with
the one the trim reads marked **newest** — and the **backup history**, every
backup a person asked any node for over the event log's 30 days, failures
included and each with the host that holds it. The register keeps only each
owner's newest point, so the history is where a failed night shows.

### One node, or every node?

`crewlet backup` runs against **one** node and copies what that node can
reach — which is not the same as what that node owns.

The **stream estate is the fleet's**, so any node's copy of it is the whole
company's: the mailboxes, the leases and epochs, the activation pointer with
the current company payload, the completion ledger, the budget counters, the
sealed credentials — and, since a seat's memory is
[replicated onto it](../concepts/seat-ownership.md#a-seats-memory-follows-it),
every seat's diary, episodes, profiles, skills and conversation ledger, no
matter which node wrote them. On a clustered embedded stream you are
snapshotting a replicated stream, so one member's snapshot carries what its
peers hold too.

A node is **two** database files, and they answer differently.

The **replicated estate** is a copy of state every data node holds: the tracker's
projects, tasks, comments and history, the knowledge base's pages and
revisions, the vectors search reads, the org chart's units and seats, the
identity directory's people, credentials and sessions, the code host's
repositories, refs and pack lists, and every node's usage
history — each derived from its
own ordered log by an applier that runs identically everywhere. Any healthy node's copy of it is the
company's, in the same sense the stream estate is.

The **node estate** is that node's alone, and what only lives there is what
only *that node* did: its audit event log, its scheduled-run history, its share
of the config revision history. Those exist nowhere else and no peer's backup
contains them.

So:

- **For the company's state — one node is enough.** Everything a restore needs
  to bring the company back is on the stream estate and the replicated estate,
  and a single node's backup captures both.
- **For the complete audit trail — take one per node**, on the same schedule,
  and keep them together. The event log is per-node history with no second
  copy, so a fleet-wide audit trail is the union of every node's.

There is no fleet-wide barrier and deliberately no attempt at one: each node's
copy is its own moment, and the estate that has to be internally consistent —
the stream estate — is captured as one set within a single run.

### Where to put it, and how often

The backup lands on the engine's host, which is not a backup until it leaves
that host: ship the directory to object storage or another machine as a
second step. Schedule it the way you schedule anything else against a node
(cron, a systemd timer, your orchestrator) — one directory per run, named by
timestamp, since a destination that already holds something is refused rather
than merged.

**The schedule is yours, and so is its cost.** Every run is a full copy, so the
footprint is arithmetic rather than a judgement:

```
backup storage = copies retained x replicated estate bytes
```

A flat *every 6 hours, retained 14 days* is 56 copies, which at a year-five
estate of ≈ 44 GB is **≈ 2.4 TB** — plus a full `VACUUM INTO` of that estate
four times a day on a live node, competing with the applier's own commits for
the same disk. That is a real cost nobody quotes, so the shipped guidance is
**tiered**:

| Tier | Kept | Covers |
|---|---|---|
| every 6 hours | 8 copies | the last two days, at the granularity an incident needs |
| daily | 14 copies | the fortnight, at the granularity a discovered problem needs |

Twenty copies rather than fifty-six — **≈ 875 GB** at the same year-five
estate, for a recovery point that is worse by nothing anybody has ever
needed. [`crewlet.backup.duration`](../reference/metrics.md) is what measures
the copy's own cost against your hardware — the window the trim hold covers and
the I/O the copy spends competing with the applier's own commits; start from the
table and move it once you have that number.

**How stale is too stale is a separate setting.** `retention.backup_max_age`
is what the trim reads, and it is deliberately not derived from the schedule:
a company that never backs up never trims, loudly and by design, so the engine
has to know what "recent enough" means to *you* rather than inferring it from
how often a cron happened to fire. The age itself is read from the newest
complete **manifest** on disk rather than from a counter the engine keeps —
a counter records that a process believed it took a backup, and the disk
records that one exists. They differ in exactly the cases the alarm is for — a
copy taken and then deleted, a volume never mounted, a schedule pointing at a
path nobody ships from — and in every one of them the counter says the fleet is
protected. It is published as `crewlet.backup.age`, by the node that holds the
artefact, because that is the only process whose disk the manifests are on; a
node that has never taken one publishes nothing rather than a zero, since zero
is the freshest backup imaginable.

**The backup interval IS the recovery point for history below the trim
floor.** Above that floor the log holds every record on R replicas and every
node holds the applied rows, so losing a node loses nothing. Below it the log
holds nothing, and each node's own database file is the only copy of that
history — N of them, independent, none replicated. A schedule of six hours is
therefore a six-hour RPO for that half of the company's past, and no replica
count changes it. See [Retention](retention.md).

**A finished backup announces itself to the fleet.** When the manifest is
written, the taker publishes what the copy reaches — per stream, with the
generation — so the trim's backup term can see it from whichever node holds the
duty. That node is often not the one that took the copy, and can never see a
directory on another host. A backup that ran and announced nothing leaves a
fleet with a working nightly schedule whose log grows for ever, so a failure to
announce is logged rather than silent — and it is bounded and self-correcting:
the log keeps a longer window than it needed to, and the next backup announces
again.

The announcement, the trim hold and the manifest's `node_id` are all keyed on
the node's **resolved** id: `node.id` when the file sets it, else
`CREWLET_NODE_ID`, else the default. A node named only through the variable
is the shape a container orchestrator runs, and keying on the raw field would
give every such node one shared, blank key.

**A pin that outlives its owner is visible.** A backup takes a trim hold before
the first byte is copied and releases it when the copy ends; `crewlet.backup.holds`
is how many the fleet is carrying, and a count that does not return to zero is a
backup that crashed mid-copy. Until the stale bound expires that pin the trim
does not advance, which has no other symptom at all until the log walks into its
ceiling.

**Name the owner.** `retention.backup_owner` is free text — a person, a team, a
scheduler's name — and `crewlet backup` records it in the manifest, which is
where it stops being configuration and becomes durable evidence of who was
responsible for the artefact somebody is now restoring. `crewlet validate`
warns when it is unset, and every node logs the same warning at every start
(`config_advisory`).

Two rules carry over from the cold runbook and are worth repeating because
this path makes them easier to forget: the directory holds every credential
the company has, so treat it exactly as you treat the secret store; and the
**keyring must not travel with it**, or the sealing is undone.

### A purge and the backups before it

A backup is never rewritten. When a person purges a repository's
unreachable objects — the commit a leaked secret was force-pushed away from,
the history a deleted branch left ([Expunge](../concepts/code-host.md#expunge))
— the purge takes them out of the repository, every git node's cache and,
a day later, the object store, but not out of a backup that already holds
them:

- **A backup taken before the purge commits holds them, for as long as it is
  kept.** Its replicated copy names the packs that held them as live, and
  its objects carry those packs: on `s3` in its `objects/`, on `nats` in the
  bucket's stream snapshot.
- **On `nats`, so does a backup taken after the commit and before the store
  deletes them** — a day after the commit, then the collector's next hourly
  pass. Its stream snapshot carries every byte the bucket holds, the retired
  packs' among them, uncounted. On `s3` a backup taken after the commit
  copies only what a live row names, so it holds none of them.
- **Restoring a backup taken before the commit brings them back.** Its rows name the
  packs as the repository's own again, served to every clone. On `s3`, copy
  the backup's `objects/` into the bucket as [Restoring](#restoring) says
  even where the bucket otherwise needs nothing — the collector has deleted
  those packs, and without them the next audit raises `objects_missing` —
  then purge the repository again once the fleet runs.

So what a purge is for is tidiness after the credential has been
**rotated**, never instead of it: expire the backups taken before it on
your own schedule, and treat each as holding the old secret until it goes.
`code_get_repo`'s `expunge.expunged_at` — and `crewlet code status -repo
<name>`'s `expunge:` line — is the instant the purge committed: a backup
whose manifest's `taken_at` is earlier holds the purged objects.

## Moving the files to a bucket

`crewlet objects migrate --to s3` moves the company's files from the fleet's
NATS bucket to an S3-compatible bucket ([the
procedure](../concepts/object-store.md#moving-the-files-to-a-bucket)). It
changes what a backup carries, so take one on each side of it:

- **Take a backup before the final pass.** Until the move, the objects ride
  the `OBJ_crewlet_files` stream snapshot; a backup taken then holds every
  file the old store holds.
- **After the move, the next backup copies the objects into `objects/`**, the
  way every backup on `s3` does, and the `OBJ_crewlet_files` snapshot it
  still takes (the stream is sealed, not deleted) is no longer where the files
  are. A restore of a backup taken before the move restores the fleet onto the
  NATS bucket, and its nodes must be started with the old `store.objects`
  block.
- **Delete the old bucket only after the first backup on the bucket** has
  finished: that backup is the first that holds the files without it.

The move never deletes anything, so a migration that stops part of the way is
resumed by running it again, and the fleet keeps working on the NATS bucket
until the final pass.

## The cold backup runbook

Still the belt to the online path's braces, and the one whose restore
exercises no recovery code at all. Use it when you want a copy that involves
no running engine — before a risky upgrade, or when taking the deployment
down anyway.

1. **Drain and stop every node** — SIGTERM or Ctrl+C once, and let the drain
   converge; see [graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).
2. **Copy, per node:** both store files — `store.path` and, on a node with
   the `data` role, `store.replicated_path` — each **together with its `-wal`
   sidecar** — committed data lives in both, while the `-shm` and `.lock`
   sidecars are transient — and `stream.store_dir` for every embedded member.
   Copying them all out of one instant is what keeps the node's local state,
   the replicated estate and the fleet's shared state telling one story. On
   the default `nats` object store the company's files are a stream, so
   `stream.store_dir` already carries them.
3. **Copy the bucket, on `s3`:** the objects under the configured prefix's
   `files/`, with the provider's own tool (`aws s3 sync
   s3://acme-files/crewlet/files/ objects/files/`). Nothing writes them while
   the fleet is stopped, so this copy is of the same instant as the rest; a
   bucket left out is a backup whose rows name files it cannot restore.
4. **Copy Tier A:** `crewlet.yaml` and any NATS credential/TLS files it
   names — and record where the keyring material comes from. **Keep the
   keyring out of the data's backup domain**
   ([Secret Store § Backups](../concepts/secret-store.md)): a backup that
   carries both the ciphertext and its key has undone the sealing.
5. **Export subscription logins**, if any seats run on a coding CLI:
   `crewlet llm export <key>` packs each into one portable bundle.
6. **Start the fleet again.**

A `stream.store_dir` left empty selects an in-memory stream server: nothing
survives a restart and there is nothing to back up. The engine refuses to boot
without one, so a fleet that reaches this procedure has already set it.

## Restoring

Restore is an operator procedure against a **stopped** fleet, not a command:
every hazard below is about ordering and identity, and a tool that hid them
behind one verb would be hiding exactly what has to be got right. What
`crewlet backup` produces is what these steps move.

The store half is two file copies: put `store.db` at the node's `store.path`
and `store-replicated.db` at its `store.replicated_path` (by default
`crewlet-replicated.db` beside `store.path`), with no `-wal` beside either —
each copy is self-contained, and a stale sidecar from the old database is the
one thing that would corrupt it. **Both, from the same backup set**: they are
one node's state, and a restore holding one of them has an audit log and a
tracker from different moments.
The object half depends on the backend. On **`nats`** there is nothing to do
apart from the streams: the objects are the `OBJ_crewlet_files` snapshot and
restore with every other stream below. On **`s3`** the bucket is not part of
the fleet and usually needs nothing at all — it kept its own copies while the
fleet was down. If it lost data, or the company is restoring into a
[new bucket](#restoring-into-a-new-bucket), copy the backup's `objects/`
directory into the bucket under the configured prefix: it is laid out as the
bucket is, each object at `files/<key>`, and a key never names two different
objects, so it is one sync, merged into whatever is there —

```sh
aws s3 sync objects/ s3://acme-files/crewlet/
```

— with the bucket and prefix from `store.objects.s3`, and the provider's own
tool or `--endpoint-url` for a store that is not Amazon's. An object a live
row of the restored copy names and the store does not hold is reported by the
collector's next audit (`objects_missing`); `crewlet objects status` lists the
files. A backup taken before a repository's purge restores what the purge
took out, and its packs are among those the bucket no longer holds: copy
them back, then purge again ([A purge and the backups before
it](#a-purge-and-the-backups-before-it)). One only a
[retired or derived](../concepts/object-store.md#live-retired-and-derived-references)
row names is never reported: the backup never carried it and nothing is owed
it — a retired row goes once its grace has passed, and the engine builds a
derived object again.
The derived objects are the code host's
[published layers](../concepts/code-host.md#when-a-layer-is-missing). On
`s3` a restore brings none back, and on `nats` the bucket's snapshot brings
back whatever parts it held, uncounted: every git node installs a part the
store holds once it passes the checks any layer is held to, and refuses a
layer with a part it lacks as `missing`, serving from the layers below it — or
from its own index, slower and never wrong — until the repository's
[maintainer](../concepts/code-host.md#who-maintains-a-repository) publishes
again: one walk of the repository's history, once, for a chain the store lost
whole — about half a minute for a gigabyte of a source repository's packs,
measured, and about a quarter of an hour, extrapolated, for the largest
repository the default `repo_max_bytes` admits.
`crewlet.code.cache.layers{outcome=missing}` counts each layer a node
refused, and `crewlet.code.cache.chained` climbs back to the repositories held
as their maintainers publish; nothing alarms meanwhile.
The stream half is restored into a broker with `nats stream restore` per
snapshot for an external cluster; for the embedded topology, restore into a
fresh `stream.store_dir` on a node started for that purpose — every snapshot,
except the one a restore into a [new bucket](#restoring-into-a-new-bucket)
leaves out. Then:

- **Restore whole estates together, then cold-start the whole fleet.** The
  fencing epochs and the activation pointer must never move backwards while
  any live node remembers newer values — gaps in those counters are harmless,
  resets are not. A KV estate restored under a running fleet hands out epochs
  that live leaseholders outrank. Every node down → restore store files and
  the stream estate from the *same* backup set → start everything.
- **Keep node identity.** A clustered embedded member's replicas are placed by
  server name, which is the node's resolved id (`node.id`, else
  `CREWLET_NODE_ID`): a node restored under a fresh
  name is a new peer, its old replicas are orphaned, and the stream sits short
  of quorum waiting for a server that will never return.
- **Keep each stream's configuration as its snapshot recorded it.** What a
  node's rows are keyed to is each log's creation instant as the engine
  stamped it into the stream's configuration when it created the stream
  (`crewlet.created_at`), not the broker's own creation time: a restore dates
  every stream to the moment of the restore, and the stamp survives because
  the restore request hands the recorded configuration back. Restored that
  way, each log is the stream the rows came from, so a node restored from the
  same backup set resumes every log at its copy's position and replays the
  records past it, and the memory changelog hydrates every seat. A
  configuration edited on the way back — the stamp dropped or changed — is a
  recreated stream to every node, which then refuses that log as
  `wrong_stream` until it is
  [re-anchored](retention.md#re-anchoring-a-recreated-or-restored-log).
  The converse holds for a stream deleted and remade outside a restore: one
  created from a configuration that still carries `crewlet.created_at` — the
  configuration `nats stream info --json` printed, passed to `nats stream add
  --config`, on an embedded fleet through a member's client listener with the
  [operator key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool) — is the same stream to every node, judged only by where it ends
  and what it holds at each node's checkpoint, and re-anchored as the restored
  case (followed from its end) rather than the recreated one. To make a
  deliberate rebuild read as a recreation, remove that metadata key from the
  configuration before creating the stream.
- **Expect bounded duplicates, not loss.** Mailboxes hold exactly the unacked
  backlog, and a restored completion ledger and dedupe window are older than
  the outside world — so some already-handled triggers re-run. That is the
  same at-least-once posture the engine holds after any crash. The reverse
  skew is the one to avoid: a ledger *newer* than the mailboxes it acquits
  writes off work that never ran, which is why the embedded topology's
  one-directory, one-instant copy is the paved path. A restored log's
  **duplicate window** — the broker collapsing an operation id appended twice
  ([replication](replication.md)) — is rebuilt from its records' ids when its
  broker next starts, not by the restore itself (nats-server 2.15.0, solo
  restore, measured): the embedded path starts its broker again with the node,
  and a stream restored into a running external cluster may serve without one
  until its servers restart. That matters only for a backup restored inside the
  window (two minutes) of being taken, since the window holds only records the
  broker stored that recently. On an external NATS
  cluster, `nats stream backup` / `nats account backup` against the cluster
  are the equivalent, taken across the streams and KV buckets as one session.
  A node on an external cluster snapshots no stream, so its backup has no last
  record to read the logs to: it carries the objects its copy names and none
  written after the copy — copy the bucket (`aws s3 sync`) at the moment the
  cluster's backup is taken, as the cold path does.
- **A node's usage history never shrinks under a restore.** A backup copies
  the store before the streams, and a fleet restore puts one node's store
  beside another node's streams, so a restored node's event log can stop
  before records its usage publisher sent after that copy — and its first
  start derives again the days its restored watermark names, most often today
  or yesterday. No day, today and yesterday included, is published below what
  the fleet already holds of it: each figure of the day goes out at the larger
  of what the node's log now counts and what the fleet held, the node logs
  `usage_rederive_short` once, and no day is derived at all until the node's
  usage applier has replayed the usage log to its end. A node started with an
  empty store gets the same protection, for the day it is running in and for
  a day a redelivered custody batch names. What the restored log lost is not
  recovered: a figure the fleet held more of counts the node's new rows only
  once the node's own count passes it. See [Replication](replication.md#two-compacted-domains-the-embeddings-and-each-nodes-day).
- **Config converges on its own.** The current revision's payload rides the
  coordination store beside the activation pointer, so a node restored with a
  stale store picks up the live revision; `crewlet config export` from any
  running node round-trips the document, sealed or not.
- **A store file lost with the stream estate intact is nearly free.** The
  learning tables and the conversation ledger re-hydrate from the memory
  changelog when the seat is next acquired, so what is actually lost is that
  node's audit log, its scheduled-run history and its config revision
  history. Start the node with an empty store; it migrates fresh and its
  seats arrive remembering. A lost replicated database is re-derived as
  [above](#what-state-exists-and-where) describes, and the node rebuilds its
  reader of each log at the checkpoint its new rows hold, so the records its
  old reader acknowledged are delivered again (see
  [Replication](replication.md#a-node-resumes-from-its-rows-never-from-its-reader)).
- **Total loss of the stream estate without a backup is survivable by
  re-provisioning** — secrets resolve store-first-env-second so a brand-new
  node starts from the environment, and every stream, bucket and mailbox is
  created idempotently at boot — at the price of the non-rebuildables above:
  the token counters lose the current day's, week's and month's spend (every
  capped window re-arms silently — a company or seat that had spent its
  ceiling gets the whole allowance back until that window turns over),
  sandbox-run records vanish (a billed box leaks until its own TTL), and the
  completion ledger forgets (bounded duplicate turns).

### Restoring into a new bucket

A restore whose `store.objects.s3` names another store than the backed-up
fleet did — another endpoint, bucket or prefix — restores every stream
**except** `KV_crewlet_objects` (`streams/KV_crewlet_objects.snapshot`). That
coordination bucket records which store the fleet's files are in, and restored
it names the old one: every node compares its own `store.objects` against it
at boot and refuses to start against the new one, naming both (see
[One store per fleet](../concepts/object-store.md#one-store-per-fleet)). Left
out, the first node to boot records the new store. The bucket's other record,
the object collector's last report, described the old store and goes with it,
so the collector's first two passes are a collection and an
[audit](../concepts/object-store.md#collection-and-audit) of the new one — and
an object the sync above missed is reported by that audit, as
`objects_missing`.

Restoring into the **same** bucket — it lost data, or it was emptied —
restores everything, the record included: it names the store every node is
configured with.

Two ways of restoring put every stream back at once, with no snapshot to leave
out:

- **An external cluster restored whole** (`nats account restore`): delete the
  record after the restore and before any node starts —
  `nats kv del crewlet_objects backend`, the gesture a
  [move](../concepts/object-store.md#one-store-per-fleet) makes. The
  collector's report left beside it names the old store, and the collector
  sets it aside rather than picking it up.
- **A cold copy** of an embedded member's `stream.store_dir`: it carries the
  bucket in the broker's own files rather than as a snapshot to leave out, so
  a restore into a new bucket is taken from a `crewlet backup` instead.

### The last step is `crewlet iam invalidate-all`

A restore rolls identity backwards as surely as it rolls everything else
backwards, and one of those rollbacks hands somebody a working credential:

- **A session ended after the artefact was taken comes back alive.** The row
  that ended it is in the artefact as an open session, so a bearer that was
  revoked — or that somebody signed out of — validates again.
- **A revocation performed after the artefact was taken is undone.** A
  person's revocation epoch is a number in the restored rows, so a leaver
  whose access was cut yesterday holds an epoch the estate now accepts.

Neither is fixable by finding the affected people, because the artefact does
not know who they are: whatever ended those sessions happened *after* it was
written. The **fleet-wide session generation** is the one number that can be
pushed forward without knowing:

```bash
crewlet iam invalidate-all -api http://127.0.0.1:8000
```

Every bearer minted before it — every cookie, every session and every
machine token — is invalid by construction: everybody signs in again once, and
every pipeline holding a token minted with `crewlet iam token` needs a new one.
Tokens are included for the same reason sessions are: a token revoked after the
artefact was taken is live again in the restored rows, and nothing can say
which ones were. The Tier A tokens in the configuration file are not in the
estate and are untouched, which is what you run this command — and re-mint the
pipelines' tokens — with.

Run it **after** the fleet is up and **before** anybody is told the restore is
finished. It takes `admin`, a person present and a proof of identity inside
`step_up` — it ends every person's authority at once — which a Tier A token
holding `admin` (as the break-glass one does) satisfies by construction, since
it is its own credential rather than a machine token and is fresh by
construction. A machine token never can, whatever it carries: the authority
table refuses it on this row, and the identity domain refuses the record.

**Before you run it, put back what took access away.** The generation ends
*bearers* — cookies, sessions, machine tokens — and nothing else. Every other
change to the identity directory made after the artefact was taken is rolled
back with it, and several of those hand somebody a way in that no bearer
carries, so everybody signing in again once lets them straight back:

- **Somebody removed or suspended since is enrolled and active again**, with
  the password and second factor they held when the artefact
  was taken. A removal comes back whole — their row, their credentials, and
  their name and address, sealed under the keyring exactly as they were.
- **A credential withdrawn since is back.** A password or a second factor
  revoked, a second factor reset and recovery codes
  regenerated since are all as they were when the artefact was taken — so a
  factor you reset because a phone was lost works again, and so do the
  recovery codes it replaced. A password changed since — by its person, or
  from a reset link — is the one before it again, so a password changed
  because it leaked signs in once more. (A reset link the artefact holds is
  not among them: it is ended by the generation, as a machine token is.)
- **An invitation cancelled since is open again** until its week runs out:
  cancelling one deletes it, so the artefact's copy comes back with its link
  working, and whoever holds the link — the stranger an address typo sent it
  to — can redeem it and be enrolled with what it carries, on the seat it
  names. While it is open it holds that seat again too: nobody else can be
  invited onto it or created on it until it is cancelled once more.
- **A permission taken away since is held again** — `admin` counting only on a
  node whose `api.auth.admin` serves it, which is Tier A and not in the
  artefact — and so is a **seat**: a person moved to another seat since is back on
  the one they held, and a service account unbound since is bound again.

The restored estate cannot list any of these: its own trail
([`crewlet iam audit`](../reference/cli.md#crewlet-iam)) and every node's event
log stop at the moment the artefact was taken, exactly like the rows they
describe. So re-apply each one from a record kept **outside** the estate — your
own off-boarding and access-change records, or the engine's logs if you ship
them off the host — with `crewlet iam remove`, `suspend`, `grant`, `bind`
(a person's move to another seat), `unbind` (a service account's),
`revoke-credential`, `reset-mfa` and `cancel-invite`. Removals first: a
removed person is the one the restore handed everything back to. Anybody whose
password changed since needs a new reset link (`reset-password`), and that one
comes **after** `invalidate-all`, not before it: a reset link is ended by the
generation as a machine token is, so a link issued before the generation
moves is `410 reset_spent` by the time its person opens it.

Then read `crewlet iam people`: a restore under a different keyring shows every
person as *can't be read: this node's keyring is missing its key* rather than as
somebody with no name, and putting that key back on the ring ends it. Somebody
removed **before** the artefact was taken stays removed: its rows hold their
tombstone and none of their values — though its stream snapshot still holds
the identity log's records that wrote them, if the trim had not passed them
when the artefact was taken, and the restored log trims them on its ordinary
schedule. Somebody removed after it was taken is the first case above, and the
reason a backup is what an erasure has to outlast: until every backup taken
before the trim passed a removed person's records is deleted, the person can be
read — and restored — from it by whoever holds the keyring. See
[what a removal reaches](../concepts/identity-and-access.md#removing-somebody-erases-what-is-theirs-from-every-nodes-rows).

## What not to do

- **Do not copy the store file while the engine runs.** A live WAL database
  copied mid-write is a torn copy; the engine's exclusive lock and the
  driver's one-process rule exist precisely because there is no safe second
  opener. `crewlet backup` is the supported way to copy a running node,
  and it works by asking the engine to copy its own database.
- **Do not treat a directory without a `manifest.json` as a backup.** It is
  an attempt that did not finish, and the manifest's absence is the only
  thing that says so.
- **Do not point `sqlite3`, Litestream, or any other SQLite tooling at the
  live file.** The file format is SQLite's, but the live coordination is not:
  the store's engine does not support mixed-tool multi-process access.
  Reading a *cold copy* with `sqlite3` is fine; writing one is not a
  supported path back.
- **Do not restore a KV estate into a running fleet** — the epoch rewind
  above.
- **Do not back the keyring up beside the data it seals.**

## Filesystem snapshots — the other online option

An **atomic** volume or filesystem snapshot (LVM, ZFS, btrfs, EBS) that
captures the store file, its `-wal`, and `stream.store_dir` at one instant is
a crash image: restoring it recovers exactly as if the node had lost power at
that moment — WAL replay on the store, unacked work redelivered from the
mailboxes. A non-atomic copy of a live tree is **not** this, and gets no such
guarantee.

Treat snapshots as defense in depth rather than the copy you must be able to
trust: restoring one is a crash recovery, which is the least-proven surface of
a pre-1.0 database engine, and the store's own vendor recommends keeping
independent backups. `crewlet backup` is the copy to trust — it is verified at
the moment it is taken — and the cold runbook is the one whose restore
exercises no recovery code at all.
