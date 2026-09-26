# ADR-0019 — The estate names an object, and a map places its bytes

- **Status:** accepted
- **Authority:** `internal/objstore`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAChunkIsDeclared`
- **Tag-status:** unreleased

## The decision

A company's FILES are the first thing this engine stores that it does not
hold on every data node. Everything the company has to agree on about a file —
that it exists, what it is called, where it lives, which chunks make it up —
is a row in the replicated estate, written by a state log's applier like any
other. What is divided is the BYTES: a file is cut into content-addressed
chunks, each chunk belongs to one of 256 placement groups by its hash, and a
placement map in the coordination store says which data nodes hold each group.
Adding a data node adds space; the map is a pure function every node evaluates
the same way, so nobody asks where a chunk is.

Three rules hold it together, and each is stated once, at `internal/objstore`
and its packages:

- **The map is maintained by one node at a time and changed by
  compare-and-set** (`objstore/upkeep`). A member is taken out only after it
  has been gone for a grace, so a restart moves nothing, and a write past a
  silent member lands on the next member of the same ranking a reader walks,
  so an absence costs a copy in another place rather than a copy fewer.
- **The inventory is derived from the estate, never kept beside it.** Which
  chunks exist is the set of chunks the replicated rows name; which of them a
  node should hold is the map applied to that set. Repair and collection both
  compute it from this node's own tables.
- **Deleting is the only dangerous act, and it has two conditions.** A chunk no
  row names is collected once it is past a grace AND the node's view of the
  estate is current (a barrier on every domain) and complete (no record it
  could not apply). A chunk a row names but this node's map does not place
  here is deleted only when every member the map places it on says it holds
  the chunk and that its own map places it there, at the same epoch.

The cross-package half — the reason this is a record rather than a package
doc — is the list of tables that name chunks. It lives with neither the
consumer nor the collector: a consumer declares its table (and the state log
whose applier writes it), one package collects the declarations, and a test
holds that list against the replicated schema in both directions, because a
table naming chunks that nobody declared is a table whose files the collector
deletes a day after they were written. The list is also the ONLY input the
readers have: repair, collection and the backup each build their statements
from the declarations, and a domain supplies nothing but a barrier on its own
log and a read of its own rows — so there is no second statement, written
beside one reader, for the list to disagree with.

## Why the obvious alternatives are wrong

**Every data node holds every file** is what the rest of the estate does, and
it is the storage bill this design exists to avoid: a company's attachments
grow without bound and a fleet of five would pay for them five times.

**Store the bytes on the stream**, as JetStream's own object store does, keeps
them on every stream replica — the same bill, now on the broker's disk — and a
chunk that has to be trimmed is a record the log can never trim past.

**A placement kept per object** (a location row per chunk) makes every write
two writes and turns a node's failure into a rewrite of every row that named
it. A map over placement groups changes in one record and is compared in 256
entries.

**Rendezvous weights computed with a logarithm** (CRUSH's straw2) is the
standard formula and it is refused here: `math.Log` is per-architecture
assembly, and two nodes disagreeing in the last bit would place a group on
different holders. The draws are integers.

## What this does not decide

Where anything other than a file's bytes lives. The estate is still whole on
every data node; this record divides only what a row can name by hash.
