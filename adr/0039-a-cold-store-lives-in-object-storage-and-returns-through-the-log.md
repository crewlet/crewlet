# ADR-0039: A cold store will live in object storage and return through the log; the hot store keeps what that needs from its first release

- **Status:** accepted
- **Authority:** `internal/artifactdata`
- **Enforced-by:** `internal/artifactdata.TestTheStateDigestIsTheSumOfItsDocuments`, `internal/artifactdata.TestEveryHistoryRowCarriesTheDigestAfterIt`
- **Tag-status:** unreleased

## The decision

Every data node holds the whole replicated estate, and a datastore lives in
it. That is the right shape for the stores people are using and the wrong one
for the stores nobody has opened in a month, which still ride every data node's
file, every snapshot a joiner adopts and every backup, for ever. **Tiering is
the decided answer, and it is built in a later phase**; this record accepts its
design now because four of its rules constrain what this release builds, and
two of them are a re-derive of every store if they arrive late.

The design:

1. **The log stays the only way into the estate.** No applier reads an object
   store. A store that left the estate returns by being written back onto its
   own log, in chunks, so every data node rebuilds it exactly as it built it the
   first time ([ADR-0002](0002-the-stream-is-the-write-ahead-log.md)).
2. **Residency is a fleet decision made by a record.** A store is hot on every
   data node or on none, so `ClaimsIdentity`, wholesale snapshot adoption and
   the local push of changes keep working, and a node without data still
   reaches any store through one that holds it
   ([ADR-0025](0025-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md)).
3. **What leaves is an object named by a replicated row with its SHA-256**: a
   canonical checkpoint of a store's state, and, for history past the hot
   window, an immutable segment. Both are written under a random key per
   upload, verified on every read and collected by the existing lock-free rule.
4. **One canonical encoder** writes store state wherever it is written
   (checkpoints, exports, bootstrap payloads), so two writers never disagree
   about the bytes of the same state.

What this release builds for it:

- **A running state digest per store**: the sum, modulo 2^256, of the SHA-256
  of every live document's canonical encoding, updated by each write
  (subtracting the old document's hash and adding the new one), kept on the
  store's row and carried as `digest_after` on **every history row**. Any
  checkpoint at a position is then verifiable by every data node against the
  history row at that position, even after the store has moved on.
- **Compact keys**: a store, a collection and a field path are each a
  surrogate integer, so the field index is small enough to keep hot and a
  checkpoint is a rewrite of numbers rather than of every path string.
- **Footprint metered in real bytes**: what each document row, its index
  entries and its field-index entries cost in the file's B-tree pages, and
  the same for hot history and its touches (`internal/artifactdata`'s meter,
  held within 10 percent of a snapshot by
  `TestTheFootprintMeterMatchesTheFile`), which is what the per-store and
  per-company datastore quotas bound, so a quota means the same thing before
  and after tiering.
- **A hot history window** of seven days and at most 8 MiB per store, pruned in
  the write's own transaction from the record's broker time.

## Why the obvious alternatives are wrong

**Keeping every store hot and tightening quotas** cures the size of one store
and not the cost of all of them: idle stores still ride every snapshot and
every backup, and a company stays under quota only by deleting old work. The
planning model put about 1 GiB of sheet content at roughly 15.5 GB per node
and 310 GB of backups once history and every snapshot are counted; spike DS-2
replaces that estimate with a measurement.

**An applier that fetches the checkpoint itself** makes applying depend on an
external store: a node replaying the log could no longer reproduce its rows
without it, which is exactly the derivation ADR-0002 forbids breaking.

**Sharding stores across data nodes**, or materialising them lazily per node,
breaks identical copies, wholesale adoption and the rule that a node without
data asks any data node, for a fleet that holds one company on three to five
nodes.

**A database file per store** contradicts
[ADR-0004](0004-a-node-keeps-two-database-files.md), and **store state on a
compacted stream**
([ADR-0014](0014-a-compacted-changelog-is-the-fourth-answer.md)) keeps the
bytes on the broker's disks with no total order and an empty stream after a
recreate.

**Adding the digest when tiering lands** sounds like the deferred-cost option
and is the expensive one: every existing store would have to be re-read and
every existing history row rewritten, on every data node, identically.

## Amendment to an earlier record

[ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md):
an object may be state the engine derives (a checkpoint, a history segment) as
well as a file somebody uploaded, under the same rules: a random key per
upload, a row carrying its SHA-256 and size, and every read verified.

## What this does not decide

It does not decide **the eviction threshold, the checkpoint format or the
archive's retention**, which the phase that builds tiering fixes in
`internal/artifactdata` and its own record for history segments. It does not
decide **point-in-time recovery from an archive of every state log**, which is
a later record of its own. And it does not change **what a write is**, which
stays [ADR-0037](0037-a-datastore-write-is-decided-where-it-is-applied.md)'s.
