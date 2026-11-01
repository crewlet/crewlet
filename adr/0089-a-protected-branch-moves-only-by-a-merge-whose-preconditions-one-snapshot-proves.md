# ADR-0089: A protected branch moves only by a merge whose preconditions one snapshot proves

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestAMergeIsDecidedFromOneSnapshot`, `internal/code.TestAMergeMovesTargetDeletesSourceInOneRecord`, `internal/code.TestAnOverrideIsAPersons`, `internal/code.TestAProtectedRefMovesOnlyByAGatedRecord`, `internal/code.TestTheGateReadsNoDecision`
- **Tag-status:** unreleased

## The decision

A change merges into its target by ONE MERGE RECORD on its repository's own
subject, decided by the repository's committer alone (`code.Merge`,
`Committer.ProposeMerge`). Its decide reads one snapshot and proves every
precondition in it: the repository active; the change open, not a draft, into
the target and at the head the merge was built from; the target at the tip the
merge was built onto, read from the snapshot's refs and never from the
proposal; and the change's merge gate (`code.JudgeGate`) met with the record's
principal as the merger — every precondition but `mergeable`, which the merge's
own build proved against that tip. The record then does everything a merge is,
in one apply: the change marked merged first, the target moved onto the merge's
commit as a push `via merge`, the source branch deleted where the merge asks
it, the protected target's landing row, and its green row where its gate
requires no check.

So a protected branch moves only by such a record, or by a PERSON'S OWN PUSH
under `direct_push: persons` — which is that person's recorded override: their
push row names them, and only a person may make one (P8: "a person … may
override"). No seat's push moves a protected branch: every protection refuses
it. A person may also merge a change past its gate by a merge record carrying
their reason (`override`), which the change's row and the record keep; a seat
may not, and a target whose gate requires a check merges through its queue, so
only a person's override merges into one directly.

The gate counts the change's own REVIEW OBJECTS — its approvals, its findings,
its checks — never a decision of the tracker's: ADR-0023's decisions are
untouched, and a decision recorded on a work item gates no merge.

Between the snapshot and the record the race is the broker's, and it is the
deliberate one: the record is arbitrated on the repository's subject, so any
record that landed after the snapshot — a push moving either branch, a review,
a setting — fails the append and the decide runs again on the rows it left. A
verdict that changes after the record landed is a verdict about a branch that
has already moved.

## Why the obvious alternative is wrong

The obvious design is a merge as a PUSH: judge the gate in the tool, then hand
the target's move to the push path the engine already has. It splits one
decision across two snapshots. The gate is judged on the rows the tool read;
the push is decided on the rows its committer reads later, which the gate
never saw — an approval withdrawn, a blocker raised, a check failed, the
target moved by somebody else's push. The push decide cannot judge the gate
again, because a push carries no change and no merger, and the record it
writes says nothing of the change it merged, so the change's own merge is a
second record that may land, or not, after the branch moved. It also leaves
the change to M4's merge by push and close on a deleted source, which take the
change for live: deleting a seat's branch in the same push closes the change
it just merged.

The second obvious design is a lock or a lease per repository taken across
the gate and the push. The committer already serialises every write of a
repository on one goroutine, and the broker already arbitrates its subject; a
lease is a third mechanism with its own expiry, its own fencing and its own
failure modes, guarding what one arbitrated record already makes atomic.

## What this does not decide

- N-of-M approvals, approval timers and escalation: the gate asks one counted
  approval at most, as M5's `approvals` closed set says.
- CODEOWNERS files: path owners are rows on the repository's subject (M4's
  `path_owners`), matched by its home, never a file in the tree.
- How a merge is built — the merge commit, the squash, the rebase — and who
  builds it: the repository's home git node's, under ADR-0082 and ADR-0085.
- The merge queue's landings: its candidates and its `land` record prove each
  entry's gate again from their own snapshot, as this record does.
