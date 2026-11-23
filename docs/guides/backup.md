# Backups & Restore

What durable state a Crewlet deployment holds, where it lives, how to back all of it up so a restore works, and which losses are survivable without one.

**`crewlet backup` takes a verified copy of a running node**, without stopping it. It goes through the engine because the store's files are locked to that process and the embedded broker binds no socket. The cold runbook further down remains the fallback.

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

**It needs `admin`.** The command authenticates with `CREWLET_API_TOKEN` like every command that talks to a running node, and `POST /backup` refuses a credential without it with `403` naming it: copying the node's whole durable state (every credential and every seat's memory) to a directory the caller names operates the deployment.

The path is on the **engine's host**, not yours; nothing is downloaded. Any node that serves the API produces one: every node with `ingress`, so a data node (it holds both store files and, on the embedded topology, its own broker). A node that dialled an **external** NATS cluster says so: its copy carries the store estates alone, and the stream half is backed up at the cluster (`nats account backup`) from the same moment.

## What state exists, and where

Durable state lives in six places:

| Estate | Where | What it holds |
|---|---|---|
| **The node's own store file** | `store.path`, with its `-wal` sidecar | The seat's memory (diary, episodes, counterparty profiles, synthesized skills, onboarding markers), also [replicated onto the stream](../concepts/seat-ownership.md#a-seats-memory-follows-it): this file is a cache of what the memory sync has carried there and the only copy of what it has not (see **Held twice** below). Held here **only**: the audit event log (30 days), scheduled-run history, the company-config revision history, the [secret store's](../concepts/secret-store.md) bootstrap rows, and this node's record of any snapshot it has adopted |
| **The replicated estate** | `store.replicated_path`, with its `-wal` sidecar | Everything a state log's applier derives from the fleet's records (Worklet, Doclet's pages, the embeddings that search them, the [identity directory](../concepts/identity-and-access.md), the company's [git repositories](../concepts/gitlet.md), every data node's [daily usage](replication.md#two-compacted-domains-the-embeddings-and-each-data-nodes-day), the artifacts and their datastores, Ask Crewlet's conversations, the engine's own chat and the rows naming each seat's executor sessions), plus the checkpoint of how far this node has applied. Derivable by replay **only while the log still holds the records**: past the trim floor, a node with no copy of this file adopts a peer's snapshot |
| **The object store** | `store.objects`: the `OBJ_crewlet_files` stream on the broker's members (`nats`, the default), or an S3-compatible bucket (`s3`) | The bytes of the company's [files](../concepts/object-store.md), one object per upload, and of each seat's sealed executor sessions, one per capture. **One store the whole fleet shares**, not any node's directory. The rows naming the files are in the replicated estate |
| **The stream estate** | `stream.store_dir` per embedded member, or the external NATS cluster | Agent mailboxes (unacked in-flight work), the shared event and config streams, one ordered **log per state-log domain** (the record of truth the replicated estate is derived from), and every [coordination](../concepts/coordination.md) KV bucket: seat, presence and duty leases and fencing epochs, the activation pointer and the current company document it names, the completion ledger, delivery dedupe, budget counters, scheduled-fire claims, the executors' records, each human seat's chat read state and Home board, the sealed credentials |
| **Tier A, on disk** | `crewlet.yaml` and the environment it reads | The keyring (`CREWLET_SECRET_KEY_*`), the sole root of trust for everything sealed, plus API tokens and any NATS credential/TLS files |
| **LLM harness homes** | Each `providers.llm` entry's state directory on the engine host (`state_dir`, or one per key under `CREWLET_LLM_HARNESS_HOME`), each harness's files under `<state_dir>/<harness>` | The harnesses' logins and per-seat homes (a login is portable via `crewlet llm export`) |

Classify before you size the job:

- **Rebuildable, safe to lose:** every TTL'd coordination bucket. The rate valve, delivery dedupe and node status regenerate; the [rebase records](../concepts/coordination.md#what-the-fleet-shares) re-form at the next attempt (a turn whose work began more than twenty-nine days ago, retried after the loss, writes again what its earlier attempt wrote); the leases and epochs too, *provided the whole fleet cold-starts together*.
- **Held twice:** the learning tables. Each row is also on the memory changelog once the node has carried it there, which is how a seat's memory follows it to a new node. A store file lost with the stream estate intact costs what a crash costs: the rows not yet carried (about the last sync cycle's writes, more while a seat's sync is behind; [what a crash and a handoff lose](../concepts/seat-ownership.md#a-seats-memory-follows-it)). The seat re-hydrates the rest on its next acquisition.
- **Kept by the store, as many copies as it keeps:** the object store's objects. On `nats` they are a stream at `stream.replicas` copies, surviving what the logs survive, and the broker catches up a failed member; on `s3` they have the bucket provider's durability. An object lost or changed anyway is gone, and the collector's daily audit raises `objects_missing` naming the file. Only a backup covers that case.
- **Derived, and rebuildable *only within the replay window*:** everything in the replicated estate. A node that loses that file replays the domain logs from the beginning to exactly the same rows, if the logs still hold them. Past the trim floor it fetches a peer's verified snapshot automatically at boot. That needs a **peer**: a single-node company that loses this file with trimmed logs has lost the trimmed history, which `retention.backup_max_age` exists to prevent.
- **Authoritative, with no other copy:** the event log's history, the config revision history, the sealed credential bucket, the budget counters, and each executor's record (the only record of a run's parked turn, its answer and its stop; a box whose record is gone is reclaimed by its host).

## What `crewlet backup` produces

**The manifest names every state-log domain the build registers**, each at the position its copy holds. A domain whose log nobody has written has no checkpoint in the copy and is named at position zero: a restore replays its log from the first record. The backup is refused rather than written when it cannot place a domain: the copy holds a checkpoint on a log this build does not register, or a log no longer holds the record after the copy's position (the assertion below, answered `log_gap`). So every finished backup covers every registered domain, and its `backup_age` alarm clears with it.

One directory, and the **manifest is the claim**: a directory holding `manifest.json` is a complete backup; one without it is the debris of an unfinished run. That is why the manifest is written last.

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

- **Each store copy is taken with `VACUUM INTO` and then verified** (reopened, integrity-checked, schema compared against its source, sha256 recorded in the manifest) before it is renamed into place. A copy that will not open fails the backup; the digest tells a copy truncated in transit from one bad when made. Each is self-contained: no `-wal` travels with it.
- **Each store copy is one moment**: every write committed when it began is in it, nothing after is (the engine holds a read transaction open across the copy, since `VACUUM INTO` holds none). While it runs the write-ahead log is not checkpointed past that moment, so the live `-wal` grows by the node's write rate times the copy's run (file size over about 104 MB/s: about seven minutes for the year-five estate of ≈ 44 GB below), and the first checkpoint after the copy folds it back. A node writing 1 MB/s holds back about 420 MB; one writing 2.5 MB/s a gibibyte, where [`wal_large`](../reference/alarms.md) fires. **Before it copies anything a backup checks** that each database's own volume has room for what the last copy of that file held back, scaled to the file as it stands, and refuses otherwise (the node would stop committing once the volume filled). `volume_low` counts the same log in the room it wants beside a second copy. The first copy after the node starts has nothing measured, and a burst past the last copy's rate is not foreseen: `wal_large` and `volume_low` name either while it happens.
- **`VACUUM INTO` needs a writable temporary directory**: a new directory under `TURSO_TMPDIR`, `SQLITE_TMPDIR` or `TMPDIR` (the first one set, else `/tmp`), and another under `TMPDIR` for sorting a large index. Both are removed when the copy ends; a process killed mid-copy leaves only those. A backup and the [snapshot](retention.md#snapshots) every data node takes for its peers are the only things the store writes there (every other connection keeps temporary storage in memory; see [Deployment](deployment.md#the-store)). A node whose temporary directory is missing or read-only fails every backup and snapshot, and the error names the variable and the directory.
- **A node is two databases, and a backup carries both.** The node estate holds the audit log, memory, config revisions and the secret bootstrap; the replicated estate holds everything a state log's applier writes. They are separate because a snapshot for a joining node copies the second alone and must not carry the donor's audit log or bootstrap half of its secret store. Restoring one without the other gives halves from different moments. Every node with the `data` role **holds** the replicated estate from start, whatever the company runs, and the backup copies what the node holds. An estate it holds that is closed at that instant (an adoption replacing its file, or a failed reopen) **refuses the backup, naming it**; retry once the node reports the estate serving. A node without `data` holds none and is never asked: the route is `ingress`'s, which requires `data`.
- **The objects come from the object store, not this node's disk.** The rows naming them are the declared live references: a Worklet project's files (`tracker_files`), an artifact's objects, assets and brand files (`artifact_objects`, `artifact_assets`, `artifact_brand_files`), the files attached to an Ask Crewlet conversation (`assistant_files`), the pack bundles the company's [git repositories'](../concepts/gitlet.md) pushes stored (`code_packs`) and the logs of their checks (`code_check_logs`). A repository's other rows (refs, settings) are in the replicated store copy, and its packs ride the backup as these objects.
  - On `nats` they are already a stream, `OBJ_crewlet_files`, and the stream snapshot carries them; no object is copied on its own, and the manifest's `objects.stream` names the stream. After the snapshot, the backup asks the stream's leader about every object a live row of the replicated copy names, **thirty-two at a time** (the collector's audit asks at the same depth), and `objects.objects` and `objects.bytes` count the ones it holds. A key is never reused, so an object the store holds after the snapshot finished is one the snapshot holds too.
  - On `s3` the backup reads every object a **live** row of the replicated copy names (read from the copy itself, for the position's reason below) from the bucket, **thirty-two at a time**. A first copy is bound by each request's wait for its first byte (S3 answers in about 100 to 200 ms), not the link: thirty-two out keep a 1 Gbit/s link busy (sixteen left it a third idle; the large model's first copy, about 2.1 million objects and 1.15 TB, took about three hours against five), and stay under a twentieth of what S3 serves one prefix, so the company's own downloads keep the rest. Each is streamed to disk and checked against its row's size and SHA-256 as written, and lands under `objects/files/<key>`, first under a temporary name, renamed once it checks out.
  - Either way the copy's rows are read **a page at a time**, in key order across every table, while the previous page's objects are fetched: the backup never holds the company's whole inventory. An object only a [retired or derived](../concepts/object-store.md#live-retired-and-derived-references) row names is neither copied nor asked after: a restore brings the row back without it, and nothing is owed it (a retired object was let go; the engine rebuilds a derived one).
- **The files written while the backup ran come too.** A restore brings the store copy back and replays each log from the copy's position to the last record its snapshot captured (a copy of the stream as it stood when asked for, however much is appended during transfer), so a file uploaded after the copy was taken is in the restored company. Once the streams are snapshotted, the backup reads each log that writes a table of files from the copy's position to that last record, plus the records the copy's applier held back, and carries every object those records name: on `s3` copied like the rest, thirty-two at a time; on `nats` asked after like the rest, since `OBJ_crewlet_files` is snapshotted **after every other stream**. `objects.replayed` counts the ones only those records name. Refusals:
  - A record this build cannot read that may write a file (a newer build's during a rolling upgrade: a file record, a kind this build has never heard of, or one whose writer said it may touch a whole project) **refuses the backup** (`409 record_unreadable`): take it from a node running the newer build. A newer record whose writer said it touches no file (a task's, say) refuses nothing.
  - A record missing from the log between the copy and the snapshot: `503 log_gap`, take it again.
  - A copy whose position is on an earlier instance of a since-recreated log: `409 log_recreated`, take it once this node has re-keyed to the live log.
  - Every record is read as the appliers read it, its signature checked under this node's `secrets.keys` first ([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)). One signed under a key this node does not hold refuses the backup (`409 record_unverified`): add the key and restart the node, or back up from a node that holds it. One whose signature does not verify refuses it as bytes no node signed (`500 backup_failed`, the reason in this node's log).
- **A backup takes what its previous one holds** (`s3`). A key is minted for one upload and never names other bytes, so an object this node's previous backup holds is the same object. When that backup's directory is still on this host (the `dir` this node last announced), each object in it is **linked** into the new backup (copied where the filesystem refuses a link, another mount among them) and read back against its row's size and SHA-256 before it counts, so a copy that rotted earlier is fetched again. Only what is new is read from the bucket. Each is a complete file of the new backup, never a reference into the old: shipping or deleting either directory leaves the other whole. The manifest's `objects.reused` and `objects.reused_from` say how many and from where.
- **An object the store has lost is recorded, an object it could not answer for fails the backup.**
  - An object the store *answered* it does not hold, or holds at another size or digest than its file records, is gone whatever the backup does. The backup carries everything else, lists it in the manifest's `objects.lost` as `{"object", "named_by"}` (the key and what names it, its row's owner joined by `/`: `PROJECT/path` for a project's file, `<seat>/<turn>` for a seat's executor session, which no one replaces: its conversation's next turn starts fresh), logs `backup_objects_lost`, and announces like any other. Refusing would refuse every later backup too, and with them the trim of every log in the fleet, over one lost file. The `objects_missing` alarm sends somebody to replace the file. Only what a restore names is lost: a file the copy names that a later record replaced or removed, whose old object the collector has since deleted, is owed by nothing the restore brings back, so it is not listed (`backup_objects_removed_since_copy` counts them).
  - An object the store did not *answer* about is **asked again** up to four times, a second, two and four apart, within the time one read is given, since a reset or stall in one of millions of reads is no reason to refuse hours of copying (`backup_objects_retried` counts those that needed it). On `s3` each read already retries a refused request until it cannot begin within a minute, so a bucket that fails one object every time spends about two minutes on a small file on two such reads rather than four.
  - One the store still does not answer for may be intact there, so it **fails the backup** (`POST /backup` answers 503 `objects_unreachable`), naming it: take it again once the store answers. The **first** such object stops the backup with nothing left in flight, so a store that has stopped answering is not sent the rest of the company's files.
  - The manifest's `objects` records `dir` or `stream`, `objects`, `bytes`, `replayed`, `reused`, `reused_from` and `lost`; a backup owing no file has none.
- **One backup at a time per node.** A backup asked for while another runs on the node is refused (`POST /backup` answers `409 backup_running`) and writes nothing, its directory included: the trim hold is keyed on the node, so two at once would share one pin and the first to finish would release the pin the other's replay needs. Schedule them so one finishes before the next starts.
- **Streams are enumerated, not listed.** A namespace stream is created on first publish and a coordination bucket's name depends on a configurable prefix, so what is captured is what is there.
- **A stream is copied with its consumers, and one that ends mid-copy costs a retake, not the backup.** The broker copies a stream's consumers (every seat's delivery position) before its messages and abandons the snapshot when one has ended by the time it reads it. A working engine ends consumers constantly (every listing of a coordination bucket is one). When the broker names a consumer it no longer holds, that stream is snapshotted again (`backup_stream_retaken`), five attempts in all; losing all five means something churns that stream's consumers far faster than the engine does, and the backup fails naming the consumer. Every other abandoned snapshot fails the backup at once.
- **Every estate or none.** A failure anywhere leaves the directory without a manifest. A backup missing an estate is unrestorable: the store alone loses every lease, ledger and credential; the streams alone lose this node's audit log, scheduled-run history and config revision history, everything the store holds that the memory changelog does not carry.

It is a copy of a **moment**, not an instant: the engine keeps working, and the pieces are separated by the copy's duration. The store is copied **first**, and that order is mandatory.

The store carries a **position**: the replicated rows are derived from an ordered log by an applier that commits its checkpoint in the same transaction as the rows, so a store copy claims what has been applied.

- **Store first** leaves a store at position *P* beside a log that has moved past it. A restore replays the difference: a bounded gap of a few minutes of work applied twice, which is free because the applier's guard is monotone in the position. The replay starts from the restored rows, not the broker's record of what the node read: the node's reader of each log was copied with the streams, *after* the store, and may have acknowledged past *P*. The node rebuilds such a reader at *P* when it boots and logs `jetstream_domain_consumer_rebuilt` (see [Replication](replication.md#a-node-resumes-from-its-rows-never-from-its-reader)).
- **Store last** would leave a store at position *P* beside a log whose newest record is *below* it. Every later record lands at a sequence the store has marked applied, and the version-guarded write drops it **silently**: a permanent hole nothing reports.

**The gap is only replayable while the log still has the records**, and the fleet's trim deletes a record once every counted node has committed past it. A backup is not a counted node, so two things close that window:

- **A trim hold** is taken before the first byte is copied, at the position this node's appliers stand at, and released when the manifest is written. It is *heartbeated*: a pin that outlived its owner would stop the trim for ever, so the fleet's trim reads past a hold it has watched go a minute unrenewed. A node that cannot write the hold refuses the backup.
- **An assertion**: the log's first surviving sequence must be at or below the copy's position plus one, checked per domain after the stream snapshots, from bounds those snapshots captured. It makes "restorable" a *checkable inequality*. A backup that cannot assert it writes **no manifest**.

The manifest records the position **read from the copy itself**, not the live database: the checkpoint commits with the rows, so the position inside a file is the only one describing that file, and the applier ran throughout the copy. The hold and the assertion cover every registered domain, a domain at position zero included.

**Every backup taken over `POST /backup` is audited.** Once the copy begins, the node writes a `backup_requested` event to its own event store: who asked (the token's name and the person it is bound to), which node, which directory, and whether it finished, a failed run included since it can leave files behind. A directory refused before the copy began (relative, not empty, or one the host cannot create or write because of the path itself) wrote nothing, answers `400`, and is not recorded. A disk that fails or fills while the directory is prepared is the node's failure: it answers `500` and is recorded, as is a backup refused because a database's own volume has no room for the log its copy would hold back, and a refusal once part of the copy is in the directory. Filter the event log on `source=operator` to see it beside every other change a person made through the engine; see [the runtime audit](../reference/api-endpoints.md#the-runtime-audit-sourceoperator).

### From the dashboard

**Settings › Backups & retention** takes one too: **Take a backup** asks the node serving the page (`POST /backup`) for a directory on **that node's host** (nothing is downloaded), offering a fresh directory beside that node's last copy when it has one. The copy can take minutes on a large store; closing the dialog does not stop it, and the outcome arrives as a notice either way.

The same screen shows what the fleet has backed up ([`GET /backups`](../reference/api-endpoints.md#get-backups)): each node's newest copy and the operator's newest acknowledgement of each log (directory, size, reach, whether the trim counts it, with every point some log's trim reads marked **newest**), and the **backup history**: every backup a person asked any node for over the event log's 30 days, failures included, each with the host that holds it. The register keeps only each node's newest point and each log's newest acknowledgement, so the history is where a failed night shows. A node you [evict](retention.md#eviction) loses its point once the eviction has taken effect on every log, so no log is trimmed against it and the screen stops listing it. The directory itself is untouched: keep or delete it.

### One node, or every node?

`crewlet backup` runs against **one** node and copies what that node can reach, which is not what it owns.

The **stream estate is the fleet's**, so any node's copy of it is the whole company's: the mailboxes, leases and epochs, activation pointer and the company document it names, completion ledger, budget counters, sealed credentials, and (since a seat's memory is [replicated onto it](../concepts/seat-ownership.md#a-seats-memory-follows-it)) every seat's diary, episodes, profiles and skills, whichever node wrote them. On a clustered embedded stream one member's snapshot carries what its peers hold.

A node is **two** database files, and they answer differently.

The **replicated estate** (listed in the table above, plus the org chart's units and seats) is derived from ordered logs by an applier that runs identically on every data node, so any healthy data node's copy is the company's.

The **node estate** is that node's alone: its audit event log (on a data node, also the events it keeps in [custody](deployment.md#custody-the-rows-of-a-node-without-data) for nodes without `data`), its scheduled-run history, its share of the config revision history. No peer's backup contains them. A node without `data` keeps none: its node estate is scratch, deleted at every boot, and each of its events is in the data node keeping it.

So:

- **For the company's state, one data node is enough.** A restore needs the stream estate and the replicated estate, and a single data node's backup captures both. Every backup is a data node's: `crewlet backup` asks a node's API, only a node with `ingress` serves one, and `ingress` requires `data`.
- **For the complete audit trail, take one per data node**, on the same schedule, and keep them together. The event log is per-node history with no second copy, so a fleet-wide trail is the union of every data node's. A node without `data` has its events kept in exactly one data node's ([custody](deployment.md#custody-the-rows-of-a-node-without-data)), so that node's backup is where they are. A data node without `ingress` serves no API for `crewlet backup`, so its trail is backed up only if it runs `ingress` too.

There is no fleet-wide barrier, deliberately: each node's copy is its own moment, and the stream estate, the one that must be internally consistent, is captured as one set within a single run.

### Where to put it, and how often

The backup lands on the engine's host, so it is not a backup until it leaves: ship the directory to object storage or another machine as a second step. Schedule it like anything else against a node (cron, a systemd timer, your orchestrator), one directory per run named by timestamp, since a destination that already holds something is refused rather than merged.

**The schedule is yours, and so is its cost.** Every run is a full copy:

```
backup storage = copies retained x replicated estate bytes
```

A flat *every 6 hours, retained 14 days* is 56 copies, **≈ 2.4 TB** at a year-five estate of ≈ 44 GB, plus a full `VACUUM INTO` four times a day on a live node, competing with the applier's commits for the same disk. The shipped guidance is **tiered**:

| Tier | Kept | Covers |
|---|---|---|
| every 6 hours | 8 copies | the last two days, at the granularity an incident needs |
| daily | 14 copies | the fortnight, at the granularity a discovered problem needs |

Twenty copies rather than fifty-six: **≈ 875 GB** at the same estate, for a recovery point worse by nothing anybody has needed. [`crewlet.backup.duration`](../reference/metrics.md) measures the copy's cost against your hardware (the window the trim hold covers and the I/O competing with the applier); start from the table and move it once you have that number.

**How stale is too stale is a separate setting.** `retention.backup_max_age` is what the trim reads, deliberately not derived from the schedule: a company that never backs up never trims, loudly and by design, so the engine must know what "recent enough" means to *you*. The age the trim and the `backup_age` alarm read is the **register's**: per log, the newest verified backup your policy counts (the announcement below, or under `backup_floor: operator` your acknowledgement of that log), aged from when the copy was taken, because the node holding the trim duty can never see a directory on another host.

[`crewlet.backup.age`](../reference/metrics.md) differs: the node that took the copy publishes it from its newest complete **manifest** on disk. The register records that a copy was announced, the disk that it still exists; so for a copy deleted, a volume never mounted, or a schedule pointing at a path nobody ships from, the register (and so the trim and alarm) still counts the copy while the gauge reports it gone. A node that has never taken one publishes nothing rather than a zero.

**The backup interval IS the recovery point for history below the trim floor.** Above it the log holds every record on R replicas and every data node holds the applied rows. Below it each data node's replicated estate is the only copy of that history (independent, none replicated). A six-hour schedule is a six-hour RPO for it, whatever the replica count. See [Retention](retention.md).

**A finished backup announces itself to the fleet.** When the manifest is written, the taker publishes what the copy reaches (per stream, with the generation, for every domain the manifest names) so the trim's backup term and the `backup_age` alarm can see it from whichever node holds the duty. A failure to announce is logged, and self-correcting: the log keeps a longer window than needed, and the next backup announces again.

The announcement, the trim hold and the manifest's `node_id` are keyed on the node's **resolved** id: `node.id` when the file sets it, else `CREWLET_NODE_ID`, else the default. A node named only through the variable (the container-orchestrator shape) would otherwise share one blank key with every such node.

**A pin that outlives its owner is visible.** `crewlet.backup.holds` is how many trim holds the fleet carries; a count that does not return to zero is a backup that crashed mid-copy. The trim reads past that pin once it has watched it go a minute unrenewed: at its second look, within half an hour of the crash, and a quarter of an hour more after its duty moved. Until then the trim does not advance past it. The pin stays in the register, and the count stays up, until its node replaces or releases it.

**Name the owner.** `retention.backup_owner` is free text (a person, a team, a scheduler's name) that `crewlet backup` records in the manifest as durable evidence of who was responsible for the artefact somebody is restoring. `crewlet validate` warns when it is unset, and every node logs the same warning at every start (`config_advisory`).

Two rules carry over from the cold runbook: the directory holds every credential the company has, so treat it as you treat the secret store; and the **keyring must not travel with it**, or the sealing is undone.

### A purge and the backups before it

A backup is never rewritten. When a person purges a repository's unreachable objects (the commit a leaked secret was force-pushed away from, the history a deleted branch left; [Expunge](../concepts/gitlet.md#expunge)), the purge takes them out of the repository, every Gitlet node's cache and, a day later, the object store, but not out of a backup that already holds them:

- **A backup taken before the purge commits holds them, for as long as it is kept.** Its replicated copy names the packs that held them as live, and its objects carry those packs: on `s3` in its `objects/`, on `nats` in the bucket's stream snapshot.
- **On `nats`, so does a backup taken after the commit and before the store deletes them** (a day after the commit, then the collector's next hourly pass): its stream snapshot carries every byte the bucket holds, retired packs included, uncounted. On `s3` a backup taken after the commit copies only what a live row names, so it holds none.
- **Restoring a backup taken before the commit brings them back.** Its rows name the packs as the repository's own again, served to every clone. On `s3`, copy the backup's `objects/` into the bucket as [Restoring](#restoring) says even where the bucket otherwise needs nothing (the collector has deleted those packs, and without them the next audit raises `objects_missing`), then purge the repository again once the fleet runs.

So a purge is for tidiness after the credential has been **rotated**, never instead of it: expire the backups taken before it on your own schedule, and treat each as holding the old secret until it goes.
`code_get_repo`'s `expunge.expunged_at` — and `crewlet gitlet status -repo
<name>`'s `expunge:` line — is the instant the purge committed: a backup
whose manifest's `taken_at` is earlier holds the purged objects.
## Moving the files to a bucket

`crewlet objects migrate --to s3` moves the company's files from the fleet's
NATS bucket to an S3-compatible bucket ([the
procedure](../concepts/object-store.md#moving-the-files-to-a-bucket)). Take a
backup on each side of it:

- **Take a backup before the final pass.** Until the move, the objects ride
  the `OBJ_crewlet_files` stream snapshot, so that backup holds every file.
- **After the move, the next backup copies the objects into `objects/`**, as
  every backup on `s3` does; the sealed `OBJ_crewlet_files` snapshot it still
  takes no longer holds the files. Restoring a pre-move backup restores the
  fleet onto the NATS bucket, and its nodes must start with the old
  `store.objects` block.
- **Delete the old bucket only after the first backup on the bucket** has
  finished.

The move never deletes anything: a stopped migration resumes by running it
again, and the fleet keeps working on the NATS bucket until the final pass.

## The cold backup runbook

The one whose restore exercises no recovery code. Use it for a copy involving
no running engine: before a risky upgrade, or when taking the deployment down
anyway.

1. **Export subscription logins first**, if any seats run on a harness, while
   the fleet is up: `crewlet llm export <key>` packs each into one portable
   bundle. It reads the company's values through the running engine, with one
   of its Tier A tokens in `CREWLET_API_TOKEN`, so do it before the drain: with
   every node stopped, a cluster member is refused ([where a command reads the
   company's values](../reference/cli.md#where-a-command-reads-the-companys-values)).
2. **Drain and stop every node**: SIGTERM or Ctrl+C once, and let the drain
   converge; see [graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).
3. **Copy, per node:** `store.path` and, on a node with the `data` role,
   `store.replicated_path`, each **with its `-wal` sidecar** (`-shm` and
   `.lock` are transient), and `stream.store_dir` for every embedded member,
   all from one instant so local, replicated and shared state tell one story.
   On the default `nats` object store the files are a stream, so
   `stream.store_dir` carries them.
4. **Copy the bucket, on `s3`:** the objects under the configured prefix's
   `files/` with the provider's tool (`aws s3 sync
   s3://acme-files/crewlet/files/ objects/files/`). Without it the backup has
   rows naming files it cannot restore.
5. **Copy Tier A:** `crewlet.yaml` and any NATS credential/TLS files it names,
   and record where the keyring material comes from. **Keep the keyring out of
   the data's backup domain** ([Secret Store § Backups](../concepts/secret-store.md)):
   a backup with both ciphertext and key has undone the sealing.
6. **Start the fleet again.**

A `stream.store_dir` left empty selects an in-memory stream server, which
keeps nothing; the engine refuses to boot without one.

## Restoring

Restore is an operator procedure against a **stopped** fleet, not a command:
the hazards are ordering and identity, which one verb would hide. These steps
move what `crewlet backup` produces.

**Store half:** put `store.db` at the node's `store.path` and
`store-replicated.db` at its `store.replicated_path` (default
`crewlet-replicated.db` beside `store.path`), with no `-wal` beside either: a
stale sidecar from the old database would corrupt it. **Both, from the same
backup set**, or the audit log and tracker come from different moments.

**Object half:**

- **`nats`:** nothing beyond the streams; the objects are the
  `OBJ_crewlet_files` snapshot.
- **`s3`:** usually nothing; the bucket kept its own copies. If it lost data,
  or you restore into a [new bucket](#restoring-into-a-new-bucket), sync the
  backup's `objects/` (laid out as the bucket is, each object at
  `files/<key>`; a key never names two objects) into the bucket under the
  configured prefix, merging into what is there:

```sh
aws s3 sync objects/ s3://acme-files/crewlet/
```

  Bucket and prefix are in `store.objects.s3`; use the provider's tool or
  `--endpoint-url` for a non-Amazon store.
- **Missing objects:** one a live row of the restored copy names and the store
  lacks is reported by the collector's next audit (`objects_missing`);
  `crewlet objects status` lists the files. A backup from before a repository's
  purge restores what the purge removed, and its packs are gone from the
  bucket: copy them back, then purge again ([A purge and the backups before
  it](#a-purge-and-the-backups-before-it)). One only a
  [retired or derived](../concepts/object-store.md#live-retired-and-derived-references)
  row names is never reported: a retired row goes once its grace passes, and
  the engine rebuilds a derived object.
- **Derived objects** are Gitlet's [published
  layers](../concepts/gitlet.md#when-a-layer-is-missing). On `s3` a restore
  brings none back; on `nats` the snapshot brings back whatever parts it held.
  A Gitlet node installs a part the store holds once it passes the layer
  checks, and refuses a layer with a missing part as `missing`, serving from
  lower layers or its own index (slower, never wrong) until the repository's
  [maintainer](../concepts/gitlet.md#who-maintains-a-repository) publishes
  again: one walk of the history for a chain lost whole, about half a minute
  per gigabyte of packs (measured), about a quarter of an hour for the largest
  repository the default `repo_max_bytes` admits (extrapolated).
  `crewlet.code.cache.layers{outcome=missing}` counts each refused layer and
  `crewlet.code.cache.chained` climbs back as maintainers publish; nothing
  alarms meanwhile.

**Stream half:** for an external cluster, `nats stream restore` per snapshot;
for embedded, restore into a fresh `stream.store_dir` on a node started for
that purpose. Restore every snapshot, except the one a restore into a [new
bucket](#restoring-into-a-new-bucket) leaves out. Then:

- **Restore whole estates together, then cold-start the whole fleet.** Fencing
  epochs and the activation pointer must never move backwards while any live
  node remembers newer values: gaps are harmless, resets are not. A KV estate
  restored under a running fleet hands out epochs live leaseholders outrank.
  With every node down, restore store files and stream estate from the *same*
  backup set, then start everything.
- **Keep node identity.** An embedded member's replicas are placed by server
  name, the node's resolved id (`node.id`, else `CREWLET_NODE_ID`). A node
  restored under a fresh name is a new peer, its old replicas are orphaned, and
  the stream waits for a server that never returns, short of quorum.
- **Keep each stream's configuration as its snapshot recorded it.** A node's
  rows are keyed to each log's creation instant as stamped in the stream's
  configuration (`crewlet.created_at`), not the broker's creation time. The
  restore request hands the recorded configuration back, so the stamp
  survives; each log is then the stream the rows came from, a node restored
  from the same set resumes every log at its copy's position and replays past
  it, and the memory changelog hydrates every seat. A configuration edited on
  the way back (stamp dropped or changed) is a recreated stream: every node
  refuses that log as `wrong_stream` until it is
  [re-anchored](retention.md#re-anchoring-a-recreated-or-restored-log).
  Conversely, a stream deleted and remade outside a restore from a
  configuration still carrying `crewlet.created_at` (the one
  `nats stream info --json` printed, passed to `nats stream add --config`, on
  an embedded fleet through a member's client listener with the [operator
  key](deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool))
  is the same stream to every node, judged by where it ends and what it holds
  at each node's checkpoint, and re-anchored as restored (followed from its
  end). To make a deliberate rebuild read as a recreation, remove that metadata
  key from the configuration first.
- **Expect bounded duplicates, not loss.** Mailboxes hold exactly the unacked
  backlog, and a restored completion ledger and dedupe window are older than
  the outside world, so some handled triggers re-run: the at-least-once posture
  after any crash. Avoid the reverse skew: a ledger *newer* than the mailboxes
  it acquits writes off work that never ran, which is why the embedded
  one-directory, one-instant copy is the paved path.
  - A restored log's **duplicate window** (the broker collapsing an operation
    id appended twice; [replication](replication.md)) is rebuilt from its
    records' ids by the restore itself before it is answered (nats-server
    2.15.3, solo restore, measured), and whenever its broker starts on it, so a
    repeated operation collapses from the moment the restore ends.
  - An external cluster's servers older than 2.15.3 rebuild the window only at
    their next start, so a stream restored into one while it runs has none until
    restart. That matters only for a backup restored within the window (two
    minutes) of being taken.
  - On an external NATS cluster, `nats stream backup` / `nats account backup`
    are the equivalent, taken across streams and KV buckets as one session. Such
    a node snapshots no stream, so its backup has no last record to read the
    logs to and carries only the objects its copy names: copy the bucket
    (`aws s3 sync`) when the cluster's backup is taken, as the cold path does.
- **A node's usage history never shrinks under a restore.** A fleet restore
  puts one node's store beside another node's streams, so a restored event log
  can stop before records its usage publisher sent after that copy, and the
  first start derives again the days its watermark names (most often today or
  yesterday). No day is published below what the fleet already holds: each
  figure goes out at the larger of the node's log count and the fleet's, the
  node logs `usage_rederive_short` once, and no day is derived until the usage
  applier has replayed the usage log to its end. A node started with an empty
  store gets the same protection, for the day it runs in and a day a
  redelivered custody batch names. What the log lost is not recovered: a figure
  the fleet held more of counts the node's new rows only once its own count
  passes it. See
  [Replication](replication.md#two-compacted-domains-the-embeddings-and-each-data-nodes-day).
- **Config converges on its own.** The current revision's payload rides the
  coordination store beside the activation pointer, so a node restored with a
  stale store picks up the live revision; `crewlet config export` from any
  running node round-trips the document, sealed or not.
- **A store file lost with the stream estate intact is nearly free.** Learning
  tables re-hydrate from the memory changelog when the seat is next acquired.
  Lost: that node's audit log, scheduled-run history, config revision history
  and the seat memory the changelog had not acknowledged (see **Held twice**
  above). Start with an empty store; it migrates fresh and seats arrive
  remembering what the changelog holds, a refined skill's versions included. A
  lost replicated database is re-derived as [above](#what-state-exists-and-where)
  describes, and the node rebuilds each log's reader at the checkpoint its new
  rows hold, so records its old reader acknowledged are delivered again (see
  [Replication](replication.md#a-node-resumes-from-its-rows-never-from-its-reader)).
- **Total loss of the stream estate without a backup is survivable by
  re-provisioning**: secrets resolve store-first-env-second so a new node
  starts from the environment, and every stream, bucket and mailbox is created
  idempotently at boot. The price is the non-rebuildables above: token counters
  lose the current day's, week's and month's spend (every capped window re-arms
  silently, giving a company or seat that spent its ceiling the whole allowance
  until the window turns over), executors' records vanish (a held box stays
  until its host reclaims it), and the completion ledger forgets (bounded duplicate
  turns).

### Restoring into a new bucket

A restore whose `store.objects.s3` names another store than the backed-up
fleet (another endpoint, bucket or prefix) restores every stream
**except** `KV_crewlet_objects` (`streams/KV_crewlet_objects.snapshot`). That
coordination bucket records which store holds the files; restored, it names
the old one, and every node refuses to boot against the new one, naming both
(see [One store per fleet](../concepts/object-store.md#one-store-per-fleet)).
Left out, the first node to boot records the new store. The object collector's
last report, which described the old store, goes with it, so its first two
passes are a collection and an
[audit](../concepts/object-store.md#collection-and-audit) of the new one, and
an object the sync missed is reported as `objects_missing`.

Restoring into the **same** bucket restores everything, the record included.

Two ways of restoring put every stream back at once:

- **An external cluster restored whole** (`nats account restore`): delete the
  record after the restore and before any node starts:
  `nats kv del crewlet_objects backend`, the gesture a
  [move](../concepts/object-store.md#one-store-per-fleet) makes. The
  collector sets aside the report beside it, which names the old store.
- **A cold copy** of an embedded member's `stream.store_dir` carries the bucket
  in the broker's own files, so for a restore into a new bucket use a
  `crewlet backup` instead.

### The last step is `crewlet iam invalidate-all`

A restore rolls identity backwards too, and one rollback hands somebody a
working credential:

- **A session ended after the artefact was taken comes back alive**: a revoked
  or signed-out bearer validates again.
- **A revocation performed after the artefact was taken is undone**: a
  person's revocation epoch is a number in the restored rows, so a leaver cut
  off yesterday holds an epoch the estate accepts.

The artefact cannot say who, so push the **fleet-wide session generation**
forward:

```bash
crewlet iam invalidate-all -api http://127.0.0.1:8000
```

Every bearer minted before it (cookie, session, machine token) is invalid:
everybody signs in again once, and every pipeline holding a token minted with
`crewlet iam token` needs a new one (a token revoked after the artefact was
taken is live again, and nothing says which). The Tier A tokens in the
configuration file are not in the estate and are untouched; run this command,
and re-mint the pipelines' tokens, with them.

Run it **after** the fleet is up and **before** anybody is told the restore is
finished. It takes `admin`, a person present and a proof of identity inside
`step_up`. A Tier A token holding `admin` (as the break-glass one does)
satisfies this by construction; a machine token never can, whatever it
carries: the authority table refuses it on this row, and the identity domain
refuses the record.

**Before you run it, put back what took access away.** The generation ends
*bearers* only. Every other identity-directory change made after the artefact
was taken is rolled back, and several give somebody a way in no bearer
carries, so signing in again once lets them straight back:

- **Somebody removed or suspended since is enrolled and active again**, with
  the password and second factor held when the artefact was taken; a removal
  comes back whole (row, credentials, name and address sealed under the
  keyring).
- **A credential withdrawn since is back.** A password or second factor
  revoked, a second factor reset and recovery codes regenerated since are as
  they were: a factor reset after a lost phone works again, with the recovery
  codes it replaced, and a password changed because it leaked signs in once
  more. (A reset link the artefact holds is not among them: the generation ends
  it, as it does a machine token.)
- **An invitation cancelled since is open again** until its week runs out:
  cancelling deletes it, so the artefact's copy returns with its link working,
  and whoever holds it (the stranger an address typo sent it to) can redeem it
  and be enrolled with what it carries, on the seat it names. While open it
  holds that seat: nobody else can be invited or created on it until it is
  cancelled again.
- **A permission taken away since is held again** (`admin` counting only on a
  node whose `api.auth.admin` serves it, which is Tier A, not in the artefact),
  and so is a **seat**: a person moved to another seat since is back on the old
  one, and a service account unbound since is bound again.

The restored estate cannot list any of these: its own trail
([`crewlet iam audit`](../reference/cli.md#crewlet-iam)) and every data node's
event log stop when the artefact was taken. Re-apply each from a record kept
**outside** the estate (your off-boarding and access-change records, or the
engine's logs if shipped off the host) with `crewlet iam remove`, `suspend`,
`grant`, `bind` (a person's move to another seat), `unbind` (a service
account's), `revoke-credential`, `reset-mfa` and `cancel-invite`. Removals
first: a removed person is the one the restore handed everything back to.
Anybody whose password changed since needs a new reset link
(`reset-password`), issued **after** `invalidate-all`: the generation ends an
earlier link, so it is `410 reset_spent` when its person opens it.

Then read `crewlet iam people`: a restore under a different keyring shows every
person as *can't be read: this node's keyring is missing its key* rather than
as somebody with no name; putting that key back on the ring ends it. Somebody
removed **before** the artefact was taken stays removed: its rows hold their
tombstone and none of their values, though its stream snapshot still holds the
identity log's records that wrote them if the trim had not passed them, and the
restored log trims them on its ordinary schedule. Somebody removed after it is
the first case above, and the reason a backup is what an erasure has to
outlast: until every backup taken before the trim passed a removed person's
records is deleted, whoever holds the keyring can read, and restore, the
person from it. See [what a
removal reaches](../concepts/identity-and-access.md#removing-somebody-erases-what-is-theirs-from-every-data-nodes-rows).

## What not to do

- **Do not copy the store's files while the engine runs.** A live WAL database
  copied mid-write is torn; there is no safe second opener. `crewlet backup`
  is the supported way to copy a running node: it asks the engine to copy its
  own database.
- **Do not treat a directory without a `manifest.json` as a backup.** It is an
  unfinished attempt.
- **Do not point `sqlite3`, Litestream, or any other SQLite tooling at the
  live file.** The format is SQLite's but the live coordination is not, and the
  engine does not support mixed-tool multi-process access. Reading a *cold
  copy* with `sqlite3` is fine; writing one is not a supported path back.
- **Do not restore a KV estate into a running fleet** (the epoch rewind
  above).
- **Do not back the keyring up beside the data it seals.**

## Filesystem snapshots — the other online option

An **atomic** volume or filesystem snapshot (LVM, ZFS, btrfs, EBS) capturing
`store.path`, `store.replicated_path` (each with its `-wal`) and
`stream.store_dir` at one instant is a crash image: restoring it recovers as
after a power loss (WAL replay on the store, unacked work redelivered from the
mailboxes). A non-atomic copy of a live tree is **not** this and gets no such
guarantee.

Treat snapshots as defense in depth: restoring one is crash recovery, the
least-proven surface of a pre-1.0 database engine, and the store's vendor
recommends independent backups. `crewlet backup` is the copy to trust
(verified when taken), and the cold runbook's restore exercises no recovery
code at all.
