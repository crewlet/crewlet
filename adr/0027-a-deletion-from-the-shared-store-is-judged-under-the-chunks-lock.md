# ADR-0027 — A deletion from the shared store is judged under the chunk's lock

- **Status:** accepted
- **Authority:** `internal/objstore/collect`
- **Enforced-by:** `internal/objstore/collect.TestAChunkReUsedDuringThePassIsKept`, `internal/objstore/collect.TestAnIncompleteEstateDeletesNothing`, `internal/objstore.TestARePutWaitsForTheChunksLock`, `internal/objstore.TestAFirstPutTakesNoLock`
- **Tag-status:** unreleased

## The decision

The object store's collector deletes a chunk only when three things hold, and
the last of them is what spans packages:

- it was **written more than a day ago** — the grace an upload gets between
  storing its chunks and writing the row that names them;
- **no row names it** in an estate the collector has pinned with a barrier and
  read **completely** — a node holding a record it could not apply deletes
  nothing, since that record may name any chunk;
- and its age is **read again under that chunk's lock** in the coordination
  store (`coord.ObjectStores`), which a WRITER of a chunk that already exists
  takes too, around its put.

A put of an existing chunk replaces it and resets its age, so a writer that
reaches the lock first leaves the chunk fresh and the collector keeps it, and a
collector that reaches it first deletes it before the writer stores it anew. A
first write takes no lock: there is nothing to delete. The lock is a lease aged
at a minute (`coord.ChunkLockTTL`), so a holder that dies strands nothing.

Four packages carry it: `objstore` (the store's put, and `Store.Locked`),
`objstore/collect` (the judgement), `coord` (the lock) and every backend, whose
put must move the written instant — `objstoretest` certifies that.

## Why the obvious alternatives are wrong

**The grace alone** leaves a window the grace cannot close: a file uploaded
again today with bytes first stored a month ago re-puts an old chunk, and a
collection that listed it a moment earlier, read no row naming it and deletes
it now leaves the new row naming nothing. The window is the time between the
listing and the delete, and on a store of millions of chunks that is minutes.

**A lock on every put** costs a coordination round trip per chunk of every
upload to close a race only a re-put can be in.

**Conditional deletes in the store** (delete-if-unmodified-since) are what an
S3 `If-Unmodified-Since` would give, and the JetStream object store has no
equivalent; a rule that holds on one backend only is a rule the default backend
breaks.

## What this does not decide

Which chunks exist or where they are kept — that is
[ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md).
