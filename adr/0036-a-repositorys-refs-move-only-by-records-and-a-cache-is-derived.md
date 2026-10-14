# ADR-0036 — A repository's refs and packs move only by records on its own subject, and a bare repository is a cache of them

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestTheCodeDomainPassesTheStateLogSuite`, `internal/code.TestRefsChangeOnlyOnTheirRepoSubject`, `internal/code/cache.TestNothingButSyncWritesACache`, `internal/code/host.TestReceivePackNeverRunsInACache`, `internal/code/host.TestAnOkIsAnsweredOnlyFromAnAppliedRow`; and the code host suite's two checks of the same rules, each held able to fail by a topology that breaks it: `internal/code/codetest.TestTheSuiteCatchesAnOkOnPending`, `internal/code/codetest.TestTheSuiteCatchesAWriteIntoACache`
- **Measured:** an append is ≈ 1.5 ms on an idle three-member loopback cluster (a floor, not a budget — docs/guides/consistency.md); the shared write lane's published floor is ≈ 2 000 rows/s (docs/guides/replication.md, the loaded CI runner) and the applier measured unloaded at 3 861–5 056 one-row records/s, so at ≈ 4 rows a small push, one push per record would reach the floor near 500 pushes/s while a 64-push record is one apply of ≈ 200 rows; a push of one commit is one pack object of ≈ 150 KB median and one record of ≈ 1.5 KiB.
- **Cost-when-tried:** four designs were judged against the code: running receive-pack in the serving repository left a refused push's objects in it — git migrates the quarantine BEFORE proc-receive answers (verified) — so under protocol v2 they were fetchable by id and a later push could connect against objects no row named and land a ref no other node could materialise; answering `ok` on a durable-but-unapplied (`pending`) record told a client a push had landed that an eviction gate then dropped on every node; a leader-sequence read level reintroduced the stale-leader window `statelog/barrier.go` measured at 15 s; a per-repository lease and epoch duplicated what the repo subject's compare-and-set already arbitrates
- **Tag-status:** unreleased

## The decision

A repository is ROWS in `code`, the fifth strict, identity-claiming state-log domain (`CREWLET_CODE_LOG`).
Its refs, its live pack set and its protection rules change ONLY by records on its own subject
(`repo.<id>`), decided from one snapshot and arbitrated by the broker's per-subject compare-and-set. A
push's bytes are uploaded first, as one object per push named by a `Required` row (ADR-0026, ADR-0033), so a
record carries a name rather than bytes. `receive-pack` runs in a throwaway scratch repository whose only
alternate is the node's cache, and one hook (`proc-receive`) hands every ref update to the engine; the
engine answers a git client `ok` only from the push's own applied row, waiting at most `StallGrace` for it.
A bare repository on a git node is a CACHE: a function of the rows at a position, written only by
`cache.Sync`, which converges a directory to the rows rather than replaying anything, and which may be
deleted at any time.

## Why the obvious alternative is wrong

The obvious host is a bare repository per node with `receive-pack` and `update-ref` moving its refs and a log
replicating what happened. Then the repository is the truth and the log a report of it, and every node's
repository is a second opinion about the same push: two nodes accept two pushes to one branch and both say
`ok`. Arbitrating on the repo subject makes exactly one of them land; deriving the caches from rows makes
every node agree on what landed; and keeping `receive-pack` out of the cache is what makes the derivation
true, because otherwise git itself writes objects the rows never named.

## What this does not decide

Who may push which ref beyond the protection rules on the row (how a person reaches git with their own
token is a record of its own; seats are a later milestone's), how a repository's history is compacted or
expunged, and where a git node with no `data` sits.
