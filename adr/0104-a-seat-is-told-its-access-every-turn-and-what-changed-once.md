# ADR-0104: A seat is told its access on every turn, and what changed in it once

- **Status:** accepted
- **Authority:** `internal/capability`
- **Enforced-by:** `internal/capability.TestYourAccessIsInEverySystemPrompt`, `internal/capability.TestYourAccessIsByteStableWhileAccessIs`, `internal/capability.TestASnapshotHoldsNoCredential`, `internal/capability.TestARevertIsToldAsAChangeBack`, `internal/capability.TestARotationIsNoChange`, `internal/engine.TestABaselineTurnCarriesNoBlock`, `internal/engine.TestASeatIsToldAChangeOnce`, `internal/engine.TestManyRevisionsAreToldAsOne`, `internal/engine.TestATurnThatNeverLaunchedToldNothing`, `internal/agent/runner.TestARelaunchCarriesTheSameBlock`, `internal/agent/runner.TestAContinuedConversationIsToldTheChange`, `internal/agent/prefetch.TestANoteOlderThanAChangeNamingItIsMarked`, `internal/capability.TestADefaultToolLevelChangeIsTold`, `internal/capability.TestTheLargestSnapshotFitsItsRecord`, `internal/capability.TestAShortenedSnapshotIsComparedAtItsOwnLevel`, `internal/engine.TestATurnsLaunchWritesItsToldRecordOnce`, `internal/engine.TestAnIncompleteBaselineTellsOnlyTheAccess`, `internal/engine.TestASnapshotPastItsBoundIsKeptAsItsDigest`, `internal/capability.TestEachTurnStatesTheBoxItsRunsUse`, `internal/capability.TestABoxChangeIsToldAsWhatMoved`, `internal/capability.TestABaselineFromBeforeBoxesTellsNoBoxChange`, `internal/engine.TestASeatIsToldItsBoxAsARunLastFoundIt`, `internal/sandbox.TestAFreshBoxIsReadOnceAndTheLaunchSaysWhatItFound`, `internal/sandbox.TestABoxIsReadIntoClosedValuesOnly`
- **Cost-when-tried:** a seat whose access to production was granted kept answering from an earlier turn of its continued session that agents cannot touch production, and a seat asked about a failing commit status had no way to know which GitHub permissions it held. The policy and the grant were both current in the system prompt; nothing said they had changed.
- **Tag-status:** unreleased

## The decision

**Every turn's system prompt states the seat's access, and the first turn after
a change is told the difference, measured against what the seat was last
told.** A turn's dispatch takes a pure snapshot of the seat's access (the
integrations it holds and at what level, GitHub permissions, scope, waiting
raises, tool states, its tool servers' levels, the company's policies, the
seat's setup steps and the boxes its runs use; never a credential or a value),
renders "## Your access"
from it, reads the seat's last told record off the access log at
`linearizable`, and renders "## What changed since your last turn" where the
two differ by a change on the closed list. Both are frozen on the turn. The
turn's first launch to reach a run writes the told record, once per turn, and
that record is the next turn's baseline. Notes written before a told change and
naming it are marked as such for 90 days, never edited or deleted. A snapshot
is bounded by design for any company: every name and word to its own bound,
every list to its cap with the rest counted and digested, and shortened in two
fixed steps past 48 KiB, so a told record is never refused for its size; two
snapshots shortened differently are compared at the coarser level.

**The boxes a seat's runs use are part of its access.** Each cell its executor
chain places a run in is told with the memory the box is held to and whether
its image has a browser and a container runtime. A fresh box is read once
after its setup, into closed values only, and the reading is kept per seat and
cell in the coordination store under a fingerprint of what made the box (its
cell, image or template, run arguments and setup steps), so a reading is told
only while the document still makes the box that way and a box nobody has read
yet is told as such. A box change is told only between two facts both sides
know, and a baseline written before boxes were told (snapshot version 1) is no
box change: "Your access" states the box.

## Why the obvious alternative is wrong

Telling a seat at apply time (a notice, a chat message, a wake) happens per node
and per revision: several revisions before a seat's next turn would each be
told, a revert would be told as two changes, a credential rotation as one, and a
wake costs a turn nobody asked for. Diffing against the previous revision has
the same faults on a smaller scale. And a statement in the system prompt alone
does not displace a contradicting statement the continued session already
holds; a dated correction, placed last, that says it replaces what came before,
does.
