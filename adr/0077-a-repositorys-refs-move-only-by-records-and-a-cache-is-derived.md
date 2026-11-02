# ADR-0077: A repository's refs and packs move only by records on its own subject, and a bare repository is a cache of them

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestTheCodeDomainPassesTheStateLogSuite`, `internal/code.TestRefsChangeOnlyOnTheirRepoSubject`, `internal/code/cache.TestNothingButSyncWritesACache`, `internal/code/host.TestReceivePackNeverRunsInACache`, `internal/code/host.TestAnOkIsAnsweredOnlyFromAnAppliedRow`; and the code host suite's two checks of the same rules, each held able to fail by a topology that breaks it: `internal/code/codetest.TestTheSuiteCatchesAnOkOnPending`, `internal/code/codetest.TestTheSuiteCatchesAWriteIntoACache`
- **Measured:** an append is ≈ 1.5 ms on an idle three-member loopback cluster (a floor, not a budget — docs/guides/consistency.md); the shared write lane's published floor is ≈ 2 000 rows/s (docs/guides/replication.md, the loaded CI runner) and the applier measured unloaded at 3 861–5 056 one-row records/s, so at ≈ 4 rows a small push, one push per record would reach the floor near 500 pushes/s while a 64-push record is one apply of ≈ 200 rows; a push of one commit is one pack object of ≈ 150 KB median and, on the code log, 1 292 bytes alone in its record and 742.7 bytes a push at sixty-four at once to one repository (`BenchmarkPushLogBytes`). Measured end to end (four vCPUs of a 2.1 GHz Xeon and its one disk, which a cluster's three members share, every write fsynced, pool git 2.47.3; `internal/code`'s "What a push costs, measured" and `internal/code/host`'s measured section): the commit chain is 7.7–7.8 ms a lone push on one member and 10.5–11.1 ms on three (86–89 pushes/s), about 1 ms more through another member's router, 2 050–2 090 pushes/s at sixty-four at once in two records proposed at the committer itself and 1 270–1 390 through another member's router, and a committer's whole queue of 1 024 drained in 0.33–0.34 s (`BenchmarkCommitChain`), so a burst of one repository reaches the lane's floor before its committer does — while pushes that share no repository are a record each, 201–225 a second across three members at sixty-four in flight, which group commit cannot raise and a data node more does not, the log's record rate their bound; a person's push of one commit is 94–99 ms as git waits for it, 59–63 ms of it the receiving node's request (150–157 ms with the bundle put to an S3 at a 100 ms first byte), about 2.4 µs more a live ref and nothing more for a gigabyte of history while each commit is dated after its parent — one dated before its parent costs the request 0.37 s over that gigabyte with the commit-graph the maintainer writes first, git's own connectivity check, and 6.9 s before it (`BenchmarkPushSmall`); and it leaves 395.3 bytes of push row on every data node for the 181 days of the history horizon and 357.3 of pack row for as long as its pack lives (`BenchmarkPushHistoryBytes`).
- **Cost-when-tried:** four designs were judged against the code: running receive-pack in the serving repository left a refused push's objects in it — git migrates the quarantine BEFORE proc-receive answers (verified) — so under protocol v2 they were fetchable by id and a later push could connect against objects no row named and land a ref no other node could materialise; answering `ok` on a durable-but-unapplied (`pending`) record told a client a push had landed that an eviction gate then dropped on every node; a leader-sequence read level reintroduced the stale-leader window `statelog/barrier.go` measured at 15 s; a per-repository lease and epoch duplicated what the repo subject's compare-and-set already arbitrates
- **Tag-status:** unreleased

## The decision

A repository is ROWS in `code`, the eighth state-log domain and the sixth strict, identity-claiming one (`CREWLET_CODE_LOG`).
Its refs, its live pack set and its protection rules change ONLY by records on its own subject
(`repo.<id>`), decided from one snapshot and arbitrated by the broker's per-subject compare-and-set. A
push's bytes are uploaded first, as one object per push named by a `Required` row (ADR-0026, ADR-0074), so a
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
token is ADR-0078; seats are a later milestone's), how a repository's history is compacted or
expunged, and where a git node with no `data` sits.
