# ADR-0060: A compaction merges packs without judging reachability, and a retired pack outlives its longest read

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code/host.TestTheHostPassesTheSuite` (its cases `C2_CompactionDuringPush`, `C3_CompactionDuringFetch` and `C4_ARetiredPackOutlivesItsLongestRead`), held able to fail by `internal/code/codetest.TestTheSuiteCatchesARetirementWithNoGrace`; `internal/objstore/references.TestARetiredTableIsSweptAndOutlivesItsLongestRead`; `internal/code/cache.TestASwapKeepsEveryPackAReaderHolds`; `internal/code/cache.TestAMergeCompactionKeepsTheCommitGraph`; `internal/code/compact.TestACompactionKeepsEveryObjectOfItsInputs`; `internal/code/compact.TestTheBuildReadsItsInputsThroughTheAlternate`; `internal/code.TestACompactionOfAPackNoLongerLiveIsStale`
- **Cost-when-tried:** the code host's first build compacted nothing: every push stored one pack, kept live for the repository's life, so a cold sync's length was its pack count — 2 000 packs at the S3 fake's 200 ms first byte took about 26 s at one sync's width (`BenchmarkColdSync`), and the 10 000 of a 10 000-push repository about 130 s (its `bucket=fake/firstbyte=200ms/packs=10000/fetch=16` arm) — every request's pack lookup searched every pack the round's index did not cover, and the code estate's pack rows grew with every push for ever
- **Tag-status:** unreleased

## The decision

A repository's small packs are merged by its HOME git node — the first of its
rendezvous order over the live git nodes
([ADR-0059](0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md)), in
`internal/code/compact` — into one or a few pack bundles, each stored as one
object, and the merge is decided by the repository's committer as a push is:
one record on `repo.<id>` (`OpCompact`), one snapshot, alone. Three rules make it
safe with no fence:

1. **A merge makes no reachability judgement.** Its outputs hold every object
   of its inputs — one `pack-objects --stdin-packs` over the inputs with NO
   excluded pack — so a push decided against the inputs can never land a ref
   whose closure the merge removed. Excluding the objects another live pack
   holds would write less, and is wrong: two compactions decided from
   different snapshots may each retire the pack the other relied on, and both
   land. Dropping objects is an expunge's, the one rewrite that needs a fence.
   And the build reads its inputs from a store holding EXACTLY them — hard
   links of the home's copy of each — so the walk `--stdin-packs` makes from
   their commits to fill in its name hashes ends at their edge: excluding
   nothing, over the whole copy that walk ran the repository's history, and a
   merge of a few small packs cost what the history weighs rather than what it
   merges.
2. **Only packs that are live and not FULL are inputs**, held to the rows in
   the decide; a plan made from an older snapshot is refused `stale` and made
   again. A full pack (`FullPackBytes`, or its bundle at `FullBundleBytes`) is
   never merged again.
3. **A retired pack outlives its longest read.** Its row moves into
   `code_packs_retired`, the first retired table
   ([ADR-0051](0051-a-retired-reference-keeps-its-object-for-a-grace.md)), and
   keeps its object from the collector for `RetireGrace` — 24 h, past
   `objstore.ReadBudget(MaxPackBytes)` plus the clock skew the references gate
   assumes — and only then does a `retire_sweep` record of the code-retention
   duty remove it. On every git node the pack leaves a copy by a SWAP: the next
   directory is built from hard links of every file kept, and a reader holding
   the old one keeps every file it may open until its last release. git opens
   a pack lazily, part way through a request, so no pin of single files could
   know which ones a running process will touch; a directory can.

Backpressure is the bound's, not the compactor's: a push carrying a pack into a
repository holding `MaxPacksPerRepo` live packs is refused `compaction_behind`,
undecided, and the home raises `code_compaction_behind` for as long as it is.

## Why the obvious alternatives are wrong

**Compact in place on each git node** (`git repack -d` in the cache). Every node
would hold different packs, so no row could name what a node holds, a cold node
would download the uncompacted history, and a cache would stop being a function
of the rows ([ADR-0054](0054-a-repositorys-refs-move-only-by-records-and-a-cache-is-derived.md)).

**A fence before a compaction**, as an expunge needs. A merge removes no object,
so there is nothing for a fence to protect; it would serialise every compaction
against every push for no failure it prevents.

**Delete a retired object at once, or after the collector's own `PendingGrace`
from its upload.** A clone of a large repository legitimately reads a bundle for
up to `ReadBudget(MaxPackBytes)`, and an object deleted under it fails the read
at its end. `PendingGrace` dates the upload, not the retirement: an object a year
old would go at the next pass.

**Pin files rather than directories.** A pin needs to know which files a process
will open, and git does not say.

## What this does not decide

Which git node is a repository's home
([ADR-0059](0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md)). How a
layer is published over full packs. Expunge. How long a person may wait on a
refused push: `MaxPacksPerRepo` is derived from a cold sync's share of
`host.Timing.SpoolWait`, measured in `internal/code`'s doc.

## Amendment — the maintainer merges, and a published pack is no input

[ADR-0061](0061-a-derived-object-is-published-kept-and-never-audited.md)
moves the merge from the home to the repository's MAINTAINER — the first git
node of its order
([ADR-0059](0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md))
whose advertised background budget holds its publication, the home on a fleet
of alike nodes — so the repository's two writers of packs, compaction and
publication, run on one node, never at once there; and the maintainer raises
`code_compaction_behind`. It adds a fourth rule: **a pack a published layer
names is never a compaction's input**, unless the compaction is the
publication that replaces that layer, since a node's chain loads only over
every pack it names. A published repository's small packs are merged by its
publications' repacks instead.
