# ADR-0057: A node boots on what it applied, and the company is the fleet's pointer

- **Status:** accepted
- **Authority:** `internal/configplane`
- **Enforced-by:** `cmd/crewlet.TestTheBootPublishDecision`, `cmd/crewlet.TestReactivatingTheBootRevisionOfflineMintsAnEpoch`, `cmd/crewlet.TestAnOfflineChangeIsNeverPublishedOverAFleetThatMovedOn`, `cmd/crewlet.TestAWriteBetweenTheBootReadAndPublishIsNotOverwritten`, `cmd/crewlet.TestARecreatedOlderPointerIsReplacedByTheNewerRevisionThisNodeSaw`, `cmd/crewlet.TestAStaleRestoreOfAnOfflineChangesBaseDoesNotPublishTheChange`, `cmd/crewlet.TestAnOfflineChangeOnAnUnconfirmedPublishIsPublishedOverIt`, `cmd/crewlet.TestACrashAfterTheBootPublishLandsConfirmsInsteadOfPublishingAgain`, `cmd/crewlet.TestANodeThatRefusedTheFleetsRevisionBootsOnWhatItServed`, `cmd/crewlet.TestAnImportStoredAndNotAnsweredNamesTheRevisionAndHowToLook`, `cmd/crewlet.TestTheBootHoldsItsStoreUntilTheEngineHasOpenedIt`, `internal/store.TestHoldKeepsTheClaimForABootWithNoHandle`, `internal/engine.TestTheBootRowFollowsWhatTheEngineApplied`, `internal/engine.TestARefusedReactivationOfTheRunningRevisionIsStillTheFleets`, `internal/engine.TestTheFleetsRowIsRepairedAfterAnApply`, `internal/engine.TestARevisionThisNodeRefusesNeverBecomesWhatItBootsOn`, `internal/engine.TestTheFleetRowSaysWhatThisNodeBootsOn`, `internal/engine.TestAScratchNodeBootsOnNothing`, `internal/engine.TestABootRowThisNodeCouldNotReadIsNeverPublishedAsNone`, `internal/engine.TestAReconcilerThatCannotReadWhatItsNodeBootsOnIsNotBuilt`, `internal/engine.TestTwoTicksOnOneEngineServeInPointerOrder`, `internal/api/configapi.TestEveryReadAfterAWriteAnswersTheWrite`, `internal/api/configapi.TestAWriteTheWriterRefusesStaysCorrectableThroughItsAPI`, `internal/api/configapi.TestAReadNeverWritesTheNodeStore`, `internal/api/configapi.TestAConfigRequestTheFleetCannotAnswerIs503`, `internal/api/configapi.TestAWriteStoredAndThenUnansweredIsAnUnknownOutcome`, `internal/api/setupapi.TestASetupWriteLandsOnTheRevisionItWasReadFrom`, `internal/api/setupapi.TestASetupWriteStoredAndThenUnansweredIsAnUnknownOutcome`, `internal/api/setupapi.TestEveryStoppedCreationTellsThePersonWhatToDo`, `internal/coord/kv.TestAnExpectationOverARemovedPointerIsARace`, `internal/store.TestTheSweepKeepsWhatTheNodeBootsOnAndWhatItSawTheFleetOn`, `internal/store.TestASweepDatesTheBootRowByTheFleetsLatestActivation`, `internal/api/queries.TestTheFleetViewSaysWhichRevisionEachNodeServes`, and the `coordtest` cases that an activation expecting a revision lands only while the pointer names it, that one expecting an epoch lands only on that activation, and that a restored pointer says so until the next write
- **Measured:** a pointer removed by hand from an external broker refused every config write on a node holding a revision with a 409 for up to 3 h 15 min (the removal marker's three-hour retention plus the sweep's 15-minute tick) on the KV backend, while the memory twin landed the same writes
- **Cost-when-tried:** one column, `is_active`, meant what a node boots on, what its fleet runs and what an offline command chose, and each reader took it for its own fact. A peer marked the fleet's revision active before validating it, so a node that refused that revision booted on it at its next start and was refused before the engine ran (#317). A write marked its revision active as the pointer moved, and a reconciler one tick behind walked the row back, so the node answered the older revision and refused the next `If-Match` as stale (#316). An offline rekey or activation was marked active "now", and the next boot published it over a fleet that had moved on, rolling every node back (#321). That boot publish carried no compare-and-set, so it replaced a write landing between its read and its publish (#319), and it skipped a re-activation of the pointer's own revision, so the offline credential-rotation gesture minted no epoch (#318). And a config write built on the node's own row expected a revision over a pointer that had been removed, which the two backends answered differently (#320)
- **Tag-status:** unreleased

## The decision

ADR-0009 put which revision is current in coordination and the apply on each
node. This record decides what the rest of the engine takes as the company, and
what a node's own store may say about it. The detail is
`internal/configplane`'s package doc, "Three facts about a company's
configuration", which names each fact's one writer.

**The company is the activation pointer.** Every config read and the base of
every config write is the revision the pointer names: `GET /config` and its
entity-tag, the revision list's `is_active`, the `If-Match` a write is judged
against, and the compare-and-set it lands with. The answer is the same on every
node, whatever that node runs. With no pointer, the company is what this node
knows of its fleet (`store.Configs.Restorable`). When coordination cannot be
read, a config request answers `503 unavailable` with a `Retry-After`, never a
local guess. No read writes.

**A node's own store keeps three facts apart** (`company_config`, node
migration 0052): the revision this node BOOTS on, the FLEET's revision as it
last saw it, runnable here or not, and an OFFER, an offline choice not yet
published, with the base it was built on. Two instants stay apart with them:
every row keeps the fleet's latest activation of its revision, which a restore
asks for and the retention sweep dates it by, and the boot row keeps the
activation this node APPLIED, which stamps the chart at the next boot. A
re-activation of the running revision that this node refuses, the
credential-rotation gesture on a node that lost the credential, is still the
fleet's newer activation, and with one instant per row it was lost. Online, only the reconciler moves the
boot row, only to the revision the engine has just applied, and one tick per
engine at a time, so a node never boots on a revision it refused and never
moves it out of pointer order. A node that refuses the fleet's revision keeps
running the one it served and boots on what its boot row holds, and says so:
each apply record carries the revision its node serves and the one it would
come back on, which
`GET /fleet` reports per node as `config_serving_revision_id` and
`config_boot_revision_id`. The second is a fact of its own rather than a
reading of the first, because the boot row's write is local and best effort:
until a tick repairs a write that failed — and every tick tries, converged or
out of retries — the node runs a revision it would not restart on, and whoever
keeps what that node needs in order to start reads the revision it would boot.
That name is only ever what a read or a write of the row found: the reconciler
reads the row before its first record and does not start when it cannot, and a
read that fails later leaves the last answer standing, so "could not read"
never reaches the fleet as "boots on nothing".

**Every publish is a compare-and-set over what it was built on.** `Expect`
lands only while the pointer names it, on every backend, and a removed pointer
is a race; a writer that read no pointer passes `ExpectAbsent`; and
`ExpectEpoch` pins a publish to the very activation it read, so one planned at
an instant lands at exactly that instant or not at all, and a crash after it
landed is told from the pointer. An offline command (`crewlet config import`,
`activate`, `rekey`) makes an offer, and the next boot publishes it only over
its base, or over an offer an earlier start sent and never confirmed, which
the fleet may run already: the rules are the table in
`cmd/crewlet/bootpublish.go`, 1a to 1e for an offer and 2a to 2c for a boot row
that is not one. An offer whose base the fleet has moved past is withheld,
logged at warn on every boot (`offline_revision_not_published`), and replaced
once the node applies the fleet's revision. A boot publish that loses its race
reads the pointer again and decides again. The boot holds its node's store
from the read it decides on until the engine has opened that store, so no
offline command changes what it decided on in between: one run in that window
is refused, naming the starting node. `crewlet run -import-company` is the
one unconditional activation, because replacing the company outright is what
it is for, and it is a choice made at that node's clock, never at the instant
the revision last activated.

**A publish says whether it restores or chooses.** A pointer a node put back
because the coordination store lost it is a RESTORE, and a peer that remembers
the fleet moving past that revision replaces it with the newer one (rule 2b),
pinned to that very restore. A pointer somebody chose is the company however
old its instant reads: instants come from writers' clocks, and an offline
activation or an override on a node whose clock runs behind would otherwise be
reverted by the first peer to boot. The flag rides the pointer until the next
write.

**A write stored and then unanswered is an unknown outcome.** The config
surface stores a revision before it moves the pointer, and an activation the
coordination store did not answer may have landed. It is answered `503` with
`outcome: unknown` and the stored revision's id, never "nothing was written",
whichever surface the write came through, and a page a person reads, the
GitHub App return, says the same in words. The setup surface reads the fleet's
company once per request, with the revision it is, so what each field holds
decides a write that lands on that revision or is refused as stale.

## Why the obvious alternatives are wrong

**One column for every fact** is what this replaced, and the header lists what
it cost. Each repair that kept the column moved the failure rather than
removing it: a reader asking "what do I boot on" and a reader asking "what is
the company" get different right answers the moment a node refuses a revision,
and one column can give only one of them.

**Answering `GET /config` with what the node serves** was the fix #317
proposed. On a node that refused R and serves P, the entity-tag would be P while
the pointer names R: every `If-Match` taken from it would expect P and lose the
race, a write without one would be built on P and lose it too, and a reload
would store P over R, a rollback through the API. What a node runs is a real
fact, and it is answered per node on `/fleet`.

**Ordering the boot row by activation instant** was the fix #316 proposed.
Instants order activations only within one pointer's lifetime: a pointer
created again takes the instant it is asked for, so a guard on "later" strands a
node behind an older instant after a recreation. It also keeps the root cause,
one column for two facts. The order comes from construction instead: one tick
per engine, writing what it applied, in the order it read the pointer.

**Publishing the newest offline revision by its instant**, the old boot rule,
published an offline change over whatever the fleet had moved on to. An offline
instant says when somebody typed a command, not what they saw; the base says
what they saw, and it is the only thing that tells an edit from a rollback.

**Answering from this node's own record when coordination cannot be read**
hands out exactly the stale entity-tag that refuses every write built on it,
and "the fleet has no company" sends a create-only write to replace a running
one. A 503 tells the client to come back in seconds.

**An expectation that lands over a missing pointer** was the contract, and the
KV backend broke it over a removal marker while the twin kept it (#320). Made
lenient on both, it would land a write built on a company nobody runs. Strict
on both, a writer that read no pointer passes `ExpectAbsent`, which creates the
pointer over a marker.

## What this leaves open

A restore and a choice are told apart, but a choice made over a stale restore
is not: an edit made on a node that knew no better is a choice, and a peer that
remembers a newer revision leaves it alone. A rebuilt coordination store is
restored by the first nodes to boot, so the fleet converges on the newest
revision any of them remembers only while they boot ahead of the first write.

## What this does not decide

The posture a lagging node takes, which is ADR-0009's and
`internal/configplane`'s own. Who may write a managed document, which is
ADR-0030. And an activation's identity across pointer lifetimes: an epoch a
recreated pointer reuses (#322), and chart stamps left stale by a pointer
recreated at an older instant (#323), are issues of their own. This record
depends on neither, because the boot row is written from what the engine
applied, never from the pointer's revision at the applied epoch.
