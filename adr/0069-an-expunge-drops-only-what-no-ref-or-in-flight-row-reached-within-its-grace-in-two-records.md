# ADR-0069: An expunge drops only what no ref or in-flight row reached within its grace, in two records

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestAPushCheckedBeforeABeginIsRefusedExpunging`; `internal/code/cache.TestOnlyAnExpungeDropsTheCommitGraph`; `internal/code/cache.TestEveryInterleavingOfAnExpungeKeepsWhatARefReaches` (every interleaving of a push, a begin, a rescue and a commit over two copies, each copy held to real `git fsck`); `internal/code/host.TestTheHostPassesTheSuite` (its cases `F48_ExpungeVsAPushCheckedBeforeIt`, `F49_ExpungeUnderAPushEvery100ms` and `F50_ExpungeRetiredPackOutlivesItsLongestRead`), held able to fail by `internal/code/codetest.TestTheSuiteCatchesAnExpungeThatDeletesWhatItDrops`; `internal/code.TestAnExpungeBeginKeepsEveryObjectOfItsInputs`; `internal/code.TestAnExpungeCommitNeedsOnlyItsDroppingPacks`; `internal/code/compact.TestAnOpenChangesOldRevisionSurvivesAPurge`; `internal/code/compact.TestAnObjectYoungerThanTheGraceIsKept`
- **Measured:** `BenchmarkApplyACompaction`'s expunge arms, one bounded run on two E-cores (internal/code's doc, # Measured): 75 ms for the widest begin — 1 024 inputs into 32 outputs, a chain of 32 layers dropped against 2 048 live packs, 2 625 rows, the widest record the code domain forms — under the third of `statelog.ApplyTxTimeBudget` a compaction's apply is held to; 13.5 ms for the widest commit, 97 rows
- **Cost-when-tried:** the code host's first build dropped nothing: a force-push or a deletion left its history in the repository's packs for the repository's life, fetchable by its id from every git node, a leaked secret included, and counted against `code.limits.repo_max_bytes`, which a push is refused `repo_size` on — so a busy repository, a seat's scratch one most of all, walked towards its quota on bytes no ref reached
- **Tag-status:** unreleased

## The decision

An EXPUNGE is the one rewrite that drops objects from a repository's packs.
It is two records of its own on `repo.<id>`, each decided alone by the
repository's committer in its one queue (`Committer.ProposeExpunge`), as a
compaction is:

1. **The BEGIN keeps every object.** It rewrites some live packs — a full
   pack among them, the only rewrite that takes one — into KEPT packs, live,
   holding what the maintainer's scan found reachable, and DROPPING packs
   holding the rest: still rows naming their objects, still served to every
   reader, never a pack a push may build on. To every reader the begin is a
   merge, so it needs no fence of its own. It moves the repository's DROP
   EPOCH, and removes the published layers over its inputs.
2. **Every record that moves a ref onto objects its build did not bring
   carries the drop epoch its build read** — a push (`Proposal.DropEpoch`,
   engine writes and box pushes inheriting it through the one landing), a
   pull mirror's sync, and a merge, a queue's candidate and its landing — and one
   read before the begin is refused `expunging`, whole and unrecorded, and
   built again. A push received after the begin cannot rely on a dropping
   object: its scratch reads the copy's own packs and never the dropping view
   (`gitproc.Cmd.Dropping`, set only for a serving git process).
3. **The COMMIT retires the dropping packs and keeps a RESCUE.** Its one
   precondition is its expunge, open at its epoch with exactly those dropping
   packs — no ref — so a repository pushed to all day still commits. Whatever
   of them a push landed between the scan and the begin made reachable again
   stays live as a rescue pack. The dropping packs move into
   `code_packs_retired`, so a reader already on one finishes
   ([ADR-0060](0060-a-compaction-merges-packs-without-judging-reachability.md)'s
   third rule) and the object store deletes their bytes only after
   `RetireGrace` and the collector's next pass.

What a scan keeps is exact, and every part of it is indexed: the closure of
the repository's current tips, every object a forced update or a deletion
left (`code_ref_drops`) within the GRACE, and every object a row still names
for work in flight — each revision head of a draft or open change, each live
queue entry's head, each merge candidate's tip and each green row's commit —
plus, on the schedule, every pack that ARRIVED within the grace, kept whole
(`code_packs.arrived_at`). The grace is `PruneGrace` (14 days, git's
`gc.pruneExpire`) on the maintainer's own WEEKLY schedule
(`ExpungeInterval`), which expunges every active or archived repository —
scratch ones included — whose unreachable bytes are more than a quarter of its
live pack bytes (`ExpungeShare`) and at least `ExpungeMinBytes`; and NONE for a
person's PURGE (`code_purge_unreachable`, an `expunge_request` record answered
at once), whose cycles take only the packs whose content dates from before the
request. The rows in flight are kept by both. The repository's MAINTAINER
runs it, in `internal/code/compact`'s third loop beside compaction and
publication, one rewrite of a repository at a time.

When the commit applies, each git node's copy drops the dropping packs by its
next swap, and with them the derived files that may name or quote what
left: the commit-graph — the one drop of it any pack change makes — the
node's tip graph over a published chain, the answers of seats' reads and the
code index (`cache.EventPackExpunged`). The multi-pack index, the push
provenance and the installed layers stay, none of them covering a dropping
pack.

## Why the obvious alternatives are wrong

**One record with a ref precondition** — drop what no ref reaches, provided
the refs are still as scanned. A repository pushed to every few seconds moves
a ref between every scan and its decide, so the expunge starves for ever,
exactly on the busy repository whose garbage grows fastest; the two records
split it so that neither waits on a ref.

**Make the unreachable objects' packs live first and mark them dropping in a
later record.** That is a third rewrite record, with its own window in which
a push may build on the cruft as on any live pack.

**Judge reachability by an object's age** — git's own `gc` keeps loose
objects by their file's mtime. A cache keeps no mtimes that mean anything (a
sync downloads every pack as a new file), and the rows are the authority.
**Scan the push history for what was dropped** costs a walk of 181 days of
push rows a cycle; `code_ref_drops` is the same answer as one indexed seek.
**Keep only what the refs reach** drops the tip a pending landing moves its
target onto (a candidate is rows, never a ref) and an open change's
force-pushed revisions its reviewers compare.

**Serve the dropping packs through `objects/info/alternates`.** git follows
alternates recursively, so every push's scratch — whose one alternate is the
copy — would see them, and a push could land a ref on an object the commit
then takes out of every node.

**Delete the dropping packs' rows at the commit.** A lagging reader's download
of one fails at its end; retiring them keeps their objects for the longest
read, as a compaction's retirement does.

## What this does not decide

Which git node maintains a repository
([ADR-0061](0061-a-derived-object-is-published-kept-and-never-audited.md)).
How a compaction merges packs, which drops nothing
([ADR-0060](0060-a-compaction-merges-packs-without-judging-reachability.md)).
How a deleted repository is purged whole — the `code-retention` duty's steps.
What a backup taken before a purge keeps: nothing rewrites a backup, so a
secret that was ever pushed is a secret to rotate, and the purge is tidiness
after the rotation. `ExpungeMinBytes` is PROVISIONAL until
`BenchmarkExpunge` decides it (internal/code's limits).

## Amendments to earlier records

[ADR-0060](0060-a-compaction-merges-packs-without-judging-reachability.md):
its "What this does not decide" names Expunge; this record decides it. A full
pack, which a compaction never merges again, is an expunge's input; and a
compaction of live packs is decided as ever while an expunge is open, since
the dropping packs are no live pack's and a merge drops nothing.

[ADR-0056](0056-a-node-local-derived-file-is-declared.md): a declaration
answers one more change, a pack an expunge's commit takes out
(`OnPackExpunged`), which is the one change that drops the commit-graph.
