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

WHAT                     FILE                                  SIZE       CONTENTS
store (node)             store.db                              252.0 KiB  20 migrations
store (replicated)       store-replicated.db                   1.2 MiB    3 migrations
stream CREWLET_AGENT     streams/CREWLET_AGENT.snapshot        1.1 KiB    5 messages
bucket crewlet_budgets   streams/KV_crewlet_budgets.snapshot   512 B      3 messages
…
```

The path is on the **engine's host**, not yours — this writes files where the
node runs and downloads nothing. Run it against a node with `seats` or
`workers` in its roles: an ingress-only node holds neither estate and says so
rather than writing a backup of nothing.

## What state exists, and where

A deployment's durable state lives in four estates:

| Estate | Where | What it holds |
|---|---|---|
| **The node's store files** | `store.path` and `store.replicated_path`, each with its `-wal` sidecar | The seat's memory — diary, episodes, counterparty profiles, synthesized skills, onboarding markers, the [conversation ledger](../concepts/conversation-sessions.md) — which is also [replicated onto the stream](../concepts/seat-ownership.md#a-seats-memory-follows-it), so this file is a cache of it rather than its only copy; and, held here **only**: the audit event log (30 days), scheduled-run history, the company-config revision history, the [secret store's](../concepts/secret-store.md) bootstrap rows |
| **The stream estate** | `stream.store_dir` per embedded member, or the external NATS cluster | Agent mailboxes (unacked in-flight work), the shared event and config streams, and every [coordination](../concepts/coordination.md) KV bucket: seat leases and fencing epochs, the activation pointer with the current company payload, the completion ledger, delivery dedupe, budget counters, scheduled-fire claims, detached sandbox-run records, the sealed credentials |
| **Tier A, on disk** | `crewlet.yaml` and the environment it reads | The keyring (`CREWLET_SECRET_KEY_*`) — the sole root of trust for everything sealed — plus API tokens and any NATS credential/TLS files |
| **cli-agent homes** | Per-seat state directories on the engine host | Subscription CLI logins (portable via `crewlet llm export`) |

Classify before you size the job:

- **Rebuildable, safe to lose:** every TTL'd coordination bucket — the rate
  valve, delivery dedupe and node status regenerate, credential cooldowns
  re-learn at the cost of some rate-limit errors — and the leases and epochs
  *provided the whole fleet cold-starts together* (they re-form from nothing).
- **Held twice:** the learning tables and the conversation ledger. Each row is
  also on the memory changelog, which is how a seat's memory follows it to a
  new node — so a store file lost with the stream estate intact costs at most
  the last sync cycle, and the seat re-hydrates the rest on its next
  acquisition.
- **Authoritative, with no other copy:** the event log's history, the config
  revision history, the sealed credential bucket, the budget counters, and
  each detached sandbox-run record, which is the only thing that knows a
  billed box exists.

## What `crewlet backup` produces

One directory, and the **manifest is the claim**: a directory holding
`manifest.json` is a complete backup, one without it is the debris of a run
that did not finish. Nothing else in the directory says so, which is exactly
why the manifest is written last.

```
2026-08-30T18-00/
├── manifest.json                          what was captured, from which node
├── store.db                               the node estate, self-contained
├── store-replicated.db                    the replicated estate, self-contained
└── streams/
    ├── CREWLET_AGENT.snapshot             a mailbox stream
    ├── KV_crewlet_secrets.snapshot        a coordination bucket
    └── …                                  one per stream and bucket found
```

Three properties worth knowing:

- **Each store copy is taken with `VACUUM INTO` and then verified** — reopened,
  integrity-checked, its schema compared against the database it came from,
  and a sha256 of the finished file recorded in the manifest — before it is
  renamed into place. A copy that will not open is a failed backup rather than
  a surprise on the worst day of the deployment's life; the digest is what
  tells a copy that was truncated in transit from one that was bad when it was
  made. Each is self-contained: no `-wal` travels with it.
- **A node is two databases, and a backup carries both.** The node estate holds
  the audit log, memory, the config revisions and the secret bootstrap; the
  replicated estate holds everything a state log's applier writes. They are
  separate files because a snapshot for a joining node is a copy of the second
  one alone — see [state log](../concepts/coordination.md). Restoring one
  without the other gives a company whose halves are from different moments.
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
  position.
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

The **replicated estate** is a copy of state every node holds: the tracker's
projects, tasks, comments and history, derived from an ordered log by an
applier that runs identically everywhere. Any healthy node's copy of it is the
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
needed. `snapshot.duration` is what measures the copy's own cost against your
hardware; start from the table and move it once you have that number.

**How stale is too stale is a separate setting.** `retention.backup_max_age`
is what the trim reads, and it is deliberately not derived from the schedule:
a company that never backs up never trims, loudly and by design, so the engine
has to know what "recent enough" means to *you* rather than inferring it from
how often a cron happened to fire. The age itself is read from the newest
complete **manifest** on disk rather than from a counter the engine keeps —
a counter records that a process believed it took a backup, and the disk
records that one exists. They differ in exactly the cases the alarm is for.

Two rules carry over from the cold runbook and are worth repeating because
this path makes them easier to forget: the directory holds every credential
the company has, so treat it exactly as you treat the secret store; and the
**keyring must not travel with it**, or the sealing is undone.

## The cold backup runbook

Still the belt to the online path's braces, and the one whose restore
exercises no recovery code at all. Use it when you want a copy that involves
no running engine — before a risky upgrade, or when taking the deployment
down anyway.

1. **Drain and stop every node** — SIGTERM or Ctrl+C once, and let the drain
   converge; see [graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).
2. **Copy, per node:** the store file **together with its `-wal` sidecar** —
   committed data lives in both, while the `-shm` and `.lock` sidecars are
   transient — and `stream.store_dir` for every embedded member. Copying both
   out of one instant is what keeps the node's local state and the fleet's
   shared state telling one story.
3. **Copy Tier A:** `crewlet.yaml` and any NATS credential/TLS files it
   names — and record where the keyring material comes from. **Keep the
   keyring out of the data's backup domain**
   ([Secret Store § Backups](../concepts/secret-store.md)): a backup that
   carries both the ciphertext and its key has undone the sealing.
4. **Export subscription logins**, if any seats run on a coding CLI:
   `crewlet llm export <key>` packs each into one portable bundle.
5. **Start the fleet again.**

A `stream.store_dir` left empty selects an in-memory stream server: nothing
survives a restart and there is nothing to back up. Set it before backups are
worth discussing at all.

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
The stream half is restored into a broker with `nats stream restore` per
snapshot for an external cluster; for the embedded topology, restore into a
fresh `stream.store_dir` on a node started for that purpose. Then:

- **Restore whole estates together, then cold-start the whole fleet.** The
  fencing epochs and the activation pointer must never move backwards while
  any live node remembers newer values — gaps in those counters are harmless,
  resets are not. A KV estate restored under a running fleet hands out epochs
  that live leaseholders outrank. Every node down → restore store files and
  the stream estate from the *same* backup set → start everything.
- **Keep node identity.** A clustered embedded member's replicas are placed by
  server name, which is the node's `node.id`: a node restored under a fresh
  name is a new peer, its old replicas are orphaned, and the stream sits short
  of quorum waiting for a server that will never return.
- **Expect bounded duplicates, not loss.** Mailboxes hold exactly the unacked
  backlog, and a restored completion ledger and dedupe window are older than
  the outside world — so some already-handled triggers re-run. That is the
  same at-least-once posture the engine holds after any crash. The reverse
  skew is the one to avoid: a ledger *newer* than the mailboxes it acquits
  writes off work that never ran, which is why the embedded topology's
  one-directory, one-instant copy is the paved path. On an external NATS
  cluster, `nats stream backup` / `nats account backup` against the cluster
  are the equivalent, taken across the streams and KV buckets as one session.
- **Config converges on its own.** The current revision's payload rides the
  coordination store beside the activation pointer, so a node restored with a
  stale store picks up the live revision; `crewlet config export` from any
  running node round-trips the document, sealed or not.
- **A store file lost with the stream estate intact is nearly free.** The
  learning tables and the conversation ledger re-hydrate from the memory
  changelog when the seat is next acquired, so what is actually lost is that
  node's audit log, its scheduled-run history and its config revision
  history. Start the node with an empty store; it migrates fresh and its
  seats arrive remembering.
- **Total loss of the stream estate without a backup is survivable by
  re-provisioning** — secrets resolve store-first-env-second so a brand-new
  node starts from the environment, and every stream, bucket and mailbox is
  created idempotently at boot — at the price of the non-rebuildables above:
  budget counters reset (a company somebody stopped on purpose re-arms
  silently), sandbox-run records vanish (a billed box leaks until its own
  TTL), and the completion ledger forgets (bounded duplicate turns).

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
