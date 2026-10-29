# ADR-0059: A repository's git work is asked of its home git node

- **Status:** accepted
- **Authority:** `internal/estate`
- **Enforced-by:** `internal/estate.TestAGitOperationIsAskedOfItsRepositorysHome`, `internal/estate.TestAGitNodeThatIsSyncingSendsTheAskerToTheRunnerUp`, `internal/estate.TestOnlyTheEffectiveHomeIsAskedToFill`, `internal/estate.TestAHomeThatJudgesNoSeatHandsTheFillToItsRunnerUp`, `internal/estate.TestTheFillMovesOnlyPastANodeBringingNoCopyUp`, `internal/code.TestAReasonPassesTheFillOnlyWhereNoCopyIsComing`, `internal/code/query.TestOnlyAFailureOfTheNodesOwnLetsAnotherNodeFill`, `internal/estate.TestAGitNodeAtItsShareIsHeardInsideItsAskersAttempt`, `internal/estate.TestAGitReadInProcessIsHeldToTheAttemptARemoteOneIs`, `internal/code/query.TestAReadWhoseAnswerFallsDueSaysSoWhileItsAskerListens`, `internal/code/query.TestAReadsOwnGitHasOneBudgetItsProcessesShare`, `internal/estate.TestANodeWithNoCodeHostRunsNoGitRead`, `internal/estate.TestEveryOperationNamesTheCopyItReads`, `internal/code/query.TestANodeThatHoldsNoCopyFillsOnlyAsHome`, `internal/code/cache.TestACacheHoldsACopyOnlyOnceItHasOneOrIsFillingOne`, `internal/code/codecluster.TestEveryMemberRoutesAGitReadToOneHome`, `internal/engine.TestTheGitHalfIsServedOnceAnApplyBuildsTheCodeHost`, `internal/rendezvous.TestNoPackageOrdersByAHashOfItsOwn`, `internal/estate.TestAnEngineWriteThatRanNothingMovesOnWithTheSameWrite`, `internal/estate.TestAGitWriteIsHeldToItsOwnAttempt`, `internal/estate.TestProductionsGitWriteAttemptHoldsAWritesWaits`, and the code host fleet suite's `EveryMemberRoutesAnEngineWriteToTheHome`
- **Measured:** `BenchmarkCodeRead` (`internal/code/query`), once over a 32 MB history of 14 080 commits under a 10 000-file tree on git 2.56.0: every kind a warm home answers in 3–231 ms, more than an order of magnitude inside the 10 s of git run time a read is given, and a node holding no copy, asked as the home, syncs the repository whole and answers in 118 ms from the in-process store (`memobj`), 604 ms from the S3 fake answering every request 100 ms late and 1.1 s at 200 ms (no real bucket) — so the 17 s attempt a router gives a git read is a sum of whole budgets (the share's wait, two read budgets, the read's own git and the reply's margin), never a figure a healthy home approaches; the read lane's queue and the short checks of what a read names are left out of it, and come out of the attempt
- **Cost-when-tried:** not tried here: before this record no seat read a repository server-side at all. The cost it avoids is the one the cold sync measures — 1.4 s for a repository of two hundred pushes and 13.4–13.8 s for one of two thousand against an object store answering every request 100 ms late — paid on whichever git node a read landed on, and on every one of them over time
- **Tag-status:** unreleased

## The decision

An operation on a repository's git OBJECTS — a server-side read by object id, in the estate's `code.git_read`,
and an engine write built beside a copy of them (ADR-0062), in its `code.git_write` — is answered by a node with
the `git` role from its own cache, never by a data node's copy.
The estate's router asks it of the GIT nodes (`RouterOptions.Git`, every live node with the role) in
`rendezvous.Order(repository id, every live git node)`: the repository's HOME first, then its runner-up, with
the demotions every keyed order has — a node that ran nothing for it (`no_backend`: its code host not built)
asked last for thirty seconds, a silent one last of all — no sticky node, and this node at its place, in
process, where its git half is built (`LocalGit`, resolved per request, since a node builds its code host at
boot or at the first apply that hosts code, after its router exists). The home is a PREFERENCE, never
ownership: no lease and no epoch, and two nodes that both believe they are home answer the same immutable
read.

A git node that cannot answer inside its budget for a reason about ITSELF — its copy still syncing, its share
of the read lane full, no chart or a configuration behind the fleet's to judge a seat by, a failure of its
own — answers `behind` with that reason beside it (`behind_config` where it is the node's configuration, the
node's posture as every operation says it) and is not demoted, so its askers move on and come back once it
is warm. ONE node a walk FILLS a copy at a time: the walk stamps the attempt it makes of its effective home
(its first node after the demotions, `code.GitQuery.Home`), and every other git node that holds no copy
answers at once that it holds none (`not_held`) having started nothing — so a read of a repository nobody
holds costs the fleet one download, never one a git node. The fill moves on to the next node of the same
walk only past a node bringing no copy up for the read — behind its configuration for a seat's read, a
failure of its own, no code host (`code.UnreadyReason.PassesFill`) — and never past one that may be
filling: a home kept as the walk's one filler while it served no seat left a repository it alone held
unreadable to every seat for as long as it stayed stuck. Nor past a home at its share, which holds its copy
and is only busy: the share is the NODE'S, spent by every repository it serves, and passed on for it the
fill would answer a fleet busy everywhere with a download on each busy node and no capacity added. So one
repository's seat reads are served by the git nodes that hold a copy of it — its home, and any node a
person's fetch, a push or an earlier fill has warmed — and are bounded by those nodes' shares. Such an
operation names the git half rather than a domain's copy (`GitCopy`), so no copy verdict, no dispatch floor
and no lag gate holds it back: its floor travels in its arguments and is the cache's sync level. It is
answered on a lane of its own, which a git node registers from boot and answers `no_backend` on until its
code host is built, and every request on it is a signed estate request like any other.

Every attempt — asked of a peer, or of this node's own git half in process — is held to the router's git
read attempt and SERVED UNTIL ITS ANSWER IS DUE, a reply's margin before the attempt ends
(`code.AnswerBy`): every wait the read makes ends there and is answered unready while the asker still
listens, so a busy node is never heard as a gone one, and what a read is answered never depends on which
node asked it. The read's own git has ONE budget of run time, shared by every process of its kind; only a
read whose own git ran the whole of it is too large to answer, and a read the answer's due instant cut
short is unready, since a node with the time would have answered it.

An ENGINE WRITE is asked again as a read is — of the next git node after one that ran nothing or went silent —
because it is idempotent by its push id, derived from its caller's operation: the next node builds the same push,
the same commit byte for byte, and is answered from the push's row where the silent node landed it, and two nodes
that each took themselves for its home build one push, which the repository's committer lands once. It is answered
on a lane of its own, apart from the reads, since it holds its slot through its upload, its proposal and the wait
for its row. Its attempt on one git node is the sum of its stages' waits at their whole budgets, which the git node
fits the write into — answering busy, before its upload, a write whose proposal would no longer fit — so the
attempt is the router's own whatever deadline the write's caller gives: a turn's minutes waited out on a wedged
home would leave its runner-up never asked.

## Why the obvious alternative is wrong

Any data node, as every other operation is asked: seats run on nodes without the git role, and a read asked
of whichever node the ordinary walk prefers would have every git node cache every repository a seat ever
read, a cold sync paid per node per repository and each node's cache budget evicting warm repositories to
make the room. Every git node free to fill whatever it was asked: a cold round runs on after its read stops
waiting, so one read of a repository no git node held — the first after its home's death — began a whole
download of it on every git node the walk reached, at the default `repo_max_bytes` up to 20 GiB a node, for a
read one node answers. A per-repository lease — an owner: ownership needs an epoch, a fence and a handover,
for work that is idempotent or arbitrated anyway.

## Amendments to earlier records

[ADR-0025](0025-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md): the router's walk has a
second placement beside the data nodes — the git nodes, keyed by repository, for an operation on a
repository's git objects — whose operations read no domain's copy and so take no copy verdict, no dispatch
floor and no lag gate.

## What this does not decide

Who compacts a repository, what an engine-built write builds and how it lands
([ADR-0062](0062-an-engine-built-commit-is-the-acting-principals-and-lands-as-a-push.md)), git nodes that hold
no data, and whether a load balancer hashes `/git/` by repository — recommended for a cache's locality, never
required for correctness. Nor who may read or write: the git node that answers judges the asker's grant from
its own chart ([ADR-0058](0058-a-repository-grant-is-derived-from-the-chart.md)).

## Amendment — who compacts and publishes

[ADR-0061](0061-a-derived-object-is-published-kept-and-never-audited.md)
decides what this record left open: a repository's packs are compacted, and its
layers published, by its MAINTAINER — the first git node of this same
rendezvous order whose advertised background budget holds the publication, and
the home where none does. On a fleet of alike nodes it is the home. The home
stays the node a repository's reads are asked of first.
