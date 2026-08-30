# Backups & Restore

What durable state a Crewlet deployment holds, where each piece lives, how to
back all of it up so a later restore actually works, and which losses are
survivable without one.

The short version: **there is no online backup yet.** The engine owns its
store file exclusively while it runs, the embedded broker listens on no
socket, and no `crewlet backup` command exists — so a complete backup today is
a **cold** backup: drain, stop, copy, start. Everything below is the detail
that makes that copy restorable.

## What state exists, and where

A deployment's durable state lives in four estates:

| Estate | Where | What it holds |
|---|---|---|
| **The node's store file** | `store.path`, plus its `-wal` sidecar | The seat's memory — diary, episodes, counterparty profiles, synthesized skills, onboarding markers; the audit event log (30 days); the [conversation ledger](../concepts/conversation-sessions.md); scheduled-run history; the company-config revision history; the [secret store's](../concepts/secret-store.md) bootstrap rows |
| **The stream estate** | `stream.store_dir` per embedded member, or the external NATS cluster | Agent mailboxes (unacked in-flight work), the shared event and config streams, and every [coordination](../concepts/coordination.md) KV bucket: seat leases and fencing epochs, the activation pointer with the current company payload, the completion ledger, delivery dedupe, budget counters, scheduled-fire claims, detached sandbox-run records, the sealed credentials |
| **Tier A, on disk** | `crewlet.yaml` and the environment it reads | The keyring (`CREWLET_SECRET_KEY_*`) — the sole root of trust for everything sealed — plus API tokens and any NATS credential/TLS files |
| **cli-agent homes** | Per-seat state directories on the engine host | Subscription CLI logins (portable via `crewlet llm export`) |

Classify before you size the job:

- **Rebuildable, safe to lose:** every TTL'd coordination bucket — the rate
  valve, delivery dedupe and node status regenerate, credential cooldowns
  re-learn at the cost of some rate-limit errors — and the leases and epochs
  *provided the whole fleet cold-starts together* (they re-form from nothing).
- **Authoritative, with no other copy:** the learning tables — a seat that
  loses them keeps working and has simply lost its memory — the event log's
  history, the config revision history, the sealed credential bucket, the
  budget counters, and each detached sandbox-run record, which is the only
  thing that knows a billed box exists.

## The cold backup runbook

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
  opener.
- **Do not point `sqlite3`, Litestream, or any other SQLite tooling at the
  live file.** The file format is SQLite's, but the live coordination is not:
  the store's engine does not support mixed-tool multi-process access.
  Reading a *cold copy* with `sqlite3` is fine; writing one is not a
  supported path back.
- **Do not restore a KV estate into a running fleet** — the epoch rewind
  above.
- **Do not back the keyring up beside the data it seals.**

## Filesystem snapshots — the one online option

An **atomic** volume or filesystem snapshot (LVM, ZFS, btrfs, EBS) that
captures the store file, its `-wal`, and `stream.store_dir` at one instant is
a crash image: restoring it recovers exactly as if the node had lost power at
that moment — WAL replay on the store, unacked work redelivered from the
mailboxes. A non-atomic copy of a live tree is **not** this, and gets no such
guarantee.

Treat snapshots as defense in depth rather than the copy you must be able to
trust: crash recovery is the least-proven surface of a pre-1.0 database
engine, and the store's own vendor recommends keeping independent backups.
The cold runbook above is the one whose restore exercises no recovery code at
all.
