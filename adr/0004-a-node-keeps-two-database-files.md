# ADR-0004 — A node keeps two database files, and nothing spans them

- **Status:** accepted
- **Authority:** `internal/store`
- **Enforced-by:** `internal/store.TestNoStatementNamesBothEstates`, `internal/store.TestNoTableIsDeclaredInBothEstates`
- **Measured:** 16 tables in the node estate, 60 in the replicated one
- **Cost-when-tried:** taken from a single file, a snapshot is a copy of everything followed by a delete — and with no in-place `VACUUM`, the deleted pages ride along in the artefact, the transfer, the checksum and the integrity check anyway.
- **Tag-status:** unreleased

## The decision

One `Open` brings up **two** databases: the NODE estate (the audit event log,
learning memory, config revisions, the lexical search index, the bootstrap half
of the secret store) and the REPLICATED estate beside it, which is everything a
state log's applier writes.

Two rules make that real, and both are enforced by a static walk rather than
held as a convention: **no transaction spans the two, and no read joins across
them.** A cross-estate comparison is a batch from each side, never a `JOIN`.

## Why the obvious alternative is wrong

The obvious alternative is one file. It is what every other single-node design
does, and it is wrong here for one operational reason: **a snapshot is a copy
of one estate.** A node too far behind to replay the log fetches a peer's
replicated file and installs it wholesale, and that file must not carry the
donor's audit log, its agents' diaries or the bootstrap half of its secret
store.

From a single file that snapshot is a copy of everything followed by a delete.
There is no in-place `VACUUM` available here, so the deleted pages stay in the
artefact: they are transferred, checksummed and integrity-checked, and the
recipient installs a file that still contains another node's private rows.

Both failures the two rules prevent look the same from the caller's side — not
an error it can handle, but the driver refusing a table it cannot see, from a
query nobody thought was crossing a boundary, because in one file it was not.

## What this does not decide

Which estate a given table belongs in. That is
[ADR-0003](0003-fleet-agreement-state-lives-in-coordination.md)'s question, and
its gate is what makes each answer written down.

It does not make the replicated handle a second general database: what may
write it is [ADR-0002](0002-the-stream-is-the-write-ahead-log.md).
