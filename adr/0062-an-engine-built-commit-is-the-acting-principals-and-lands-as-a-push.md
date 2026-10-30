# ADR-0062: An engine-built commit is the acting principal's, committed by the engine at its operation's instant, and lands as a push

- **Status:** accepted
- **Authority:** `internal/code/host`
- **Enforced-by:** `internal/code/host.TestARetriedEngineWriteIsOnePush`, `internal/code/host.TestEveryPushTakesTheOneLanding`, `internal/code/host.TestAnEngineCommitReadsNoWholeTree`, `internal/code/host.TestAPlanJudgesEachDirectoryOnce`, `internal/code/host.TestAWriteReadsItsPathsInItsBuild`, `internal/code/host.TestAWritePastTheDepthBoundRunsNoGit`, `internal/code.TestAnEngineWriteIsHeldToOneRule`, `internal/code/host.TestAnEngineCommitIsTheActingPrincipals`, `internal/code/host.TestACommitIsNeverDatedBeforeItsParent`, `internal/code/host.TestAnEngineWriteIsHeldToTheRelaysFsck`, `internal/code/host.TestAnEngineWriteNeverWritesInACache`, `internal/code/gitproc.TestOnlyCommitTreeTakesAnIdentity`, `internal/engine.TestTheSignerFallsBackToTheIdentityDomain`, `internal/code/host.TestABoxCannotPushTheEnginesCommitter`, `internal/code/host.TestABoxStoresNoCommitAsTheEngineBesideARefusedUpdate`, `internal/code/host.TestABoxMayPushBackAnEngineCommitTheRepositoryHolds`, and the code host suite's `AnEngineCommitLands`, `AnEngineWriteRetriedIsOnePush`, `AnEngineWriteCrashedAtEveryPointIsOnePush` and `AnEngineWriteWhoseHomeDiesAfterAppendIsOnePush`, with `internal/code/codetest.TestTheSuiteCatchesAWriteAnsweredOkWithoutARow` and `internal/code/codetest.TestTheSuiteCatchesAnEngineWriteIntoACache` holding the suite able to fail
- **Measured:** one write built in two git nodes' scratches is one commit, byte for byte (`TestAnEngineCommitIsTheActingPrincipals`), and a three-file commit at depth three asks git the same requests beside sibling directories of 10 files and of 10 000 (`TestAnEngineCommitReadsNoWholeTree`): what a build reads and writes follows the change, never the tree; and the read of what a write's paths are, which git walks from the root, grows with the square of their depth — a thousand paths at the 64-component bound, each with every directory above it, took 19 s of git on two E-cores of an Intel Core Ultra 7 265U, which the build's ten seconds refuse too_large. Timed (`BenchmarkEngineWrite`, `BenchmarkEngineCommit`; two runs of ten writes an arm, on two E-cores of an Intel Core Ultra 7 265U, the pinned git 2.56.0, in-process object stores only — the in-memory one and the S3 fake at a 100 and a 200 ms first byte, no real bucket): a one-file commit from a seat's tool call to its adopted pack is 44–78 ms at the p50 from memory and 239–251 ms at a 200 ms first byte, one PUT and no GET a write; sixty-four at once at most 2.9 s at the p99 at 200 ms, a twenty-eighth of the 82 s attempt, every stage inside its budget; a hundred files at depth six build in 59–145 ms at the p50, a tree of 10 000 files and one of 100 000 within a tenth of each other in each run; and the widest build, a put into a directory of 100 000 entries, packs 2.3 MB, under the 4 MiB bound
- **Cost-when-tried:** three designs were judged against the code. A write path of its own beside the relay's is a second place to check a repository's status, its protections, its blob limit and its size and to answer ok, and the relay had already shown two copies of those steps drifting apart before the code host took one landing. A whole-tree index per commit (read-tree, update-index, write-tree) is a cost that follows the repository, every engine commit into a 100 000-file tree paying for all of it. A commit stamped with the server's clock and an engine author makes a retry another commit, so one tool call lands twice or is answered stale against its own first attempt, and history names nobody
- **Tag-status:** unreleased

## The decision

A seat's or a person's change made through a tool — files committed, a commit reverted or picked, a branch
created or deleted — is built on a git node holding the repository (its home first, ADR-0059), in a scratch
beside the node's copy, by git's own plumbing over exactly the trees the change touches, and LANDED through the
same `land` every push takes: the repository's status, its protections, its blob limit and its size checked, one
bundle stored, one proposal to the repository's committer, `ok` only from the applied row, the pack adopted into
the copy by link. `host.Server.Write` is that pipeline, and the landing's second caller beside the relay. A path a
write names is at most 64 components deep (`code.MaxTreePathDepth`), refused by name before anything is read, so
what one build reads, holds and runs is bounded by the paths it names times that depth: every directory above them
judged once, one `mktree` a level, and the read of what those paths are — which git walks from the root, at a cost
growing with the square of the depth — the build's own, under its unit of the seat git-work share and charged to
its budget of git, never a check before it.

Its push id is an UNNAMED derivation of the caller's operation (`code.WritePushID`), so a retry — on the same
node, on another git node after silence, after a crash — is the same push and collapses at the committer, and a
second ask of one push id on one node joins the first rather than build again. Its commit is the same commit:
its author is the acting principal — the seat's role name and its `role.email` as the company document writes
it, or `<handle>@<code.identity_domain>` where that is no address, a person writing as the human seat their
credential is bound to — its committer the engine, `Crewlet <engine@<code.identity_domain>>`, its author date
the operation's own second, and its committer date that second or its parent's committer date, whichever is
later, so no walk by date meets a child before its parent. All of it is a pure function of the operation and
the parent, handed to `commit-tree` through the one environment the git pool admits for it, so a retried build
is byte-identical.

The committer MARKS the commits the host built in a cooperating client's history and authenticates nothing —
any git client can write it into a commit it pushes — so the push row (`via engine`, its principal and the
turn behind a seat's) is the only record of who built a commit. A coding box's push whose pack holds a
commit committed as the engine is refused (`forged_committer`), while a person's or an import's may carry
such commits: a box runs code nobody vouches for, so every commit of its pack — stored whole, the commits
of an update refused beside one that lands with it — is read for the address, compared in any case, before
anything of it is stored, sparing only a commit the repository already holds, as it holds the engine's own
commits on a branch deleted since a box checked it out. Every object a write stores is indexed by the same
`index-pack --strict` a person's push is (`receive.fsckObjects`), so an engine write stores nothing the
relay would refuse at the door.

## Why the obvious alternative is wrong

The obvious write path builds the commit in a working tree or an index and pushes it to the host like any git
client. Then the engine is a second client whose push the host cannot tell from a person's, a commit costs a
checkout or an index of the whole repository, and a retry is a new commit — the same tool call answered stale
against its own first attempt. Building over the touched trees costs what changed; deriving the push id from the
operation makes a retry the same push; stamping the operation's instant makes it the same commit; and taking the
landing everyone takes makes every rule a person's push is held to hold for the engine's too.

## What this does not decide

Who may write is the grant's (ADR-0058) and the committer's decide; which git node a write is asked of is the
home's (ADR-0059). What a coding box pushes reaches the relay as any git client's push does, and is none of
this but the engine's committer it may not claim. A further kind of engine write is a further kind of the same
`Server.Write`, never a second path to the landing.
