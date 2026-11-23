# ADR-0063: Every turn on a node passes the node's admission

- **Status:** accepted
- **Authority:** `internal/node`
- **Enforced-by:** `internal/node.TestEveryTurnStartIsAdmitted` and `internal/node.TestTheAdmissionGuardFindsEveryRoad` (a syntactic guard: every non-test Go file in the module is parsed, the root package included; `turn.Run` is matched under whatever name a file imports the turn package by and refused through a dot import; every other edge of the two chains to a turn is an unexported name matched on any operand; and the coordinator's `admitted` type may be named, in a signature or a body, only by `admitTurn`, which makes it, and the three frames that take it, each of which is called only from its one admitting caller; it does not see a guarded function carried out of its pinned frames as a stored method value, or a call made through reflection, which review holds), `internal/node.TestTheMailboxTurnIsAdmittedBeforeItRuns`, the compiler, outside the two packages (the mailbox's turn is the unexported `engine.Engine.mailboxTurn`, and `sandbox.Coordinator.resumeAndSettle` takes the unexported `admitted`; inside `internal/sandbox` only the guard holds who makes one), `internal/engine.TestADrainingNodeLeavesACompletionForTheSeatsNextHolder`, `internal/engine.TestARefusedCompletionIsHandedBackByItsCondition`, `internal/engine.TestACompletionRefusedOnAWaitResumesOnceItClears`, `internal/sandbox.TestARefusedCompletionIsReFiredWhenItsConditionClears`, `internal/sandbox.TestARefusalWhileTheClockOnItsHostRunsIsHandedBack`, `internal/sandbox.TestAJobNoWaiterAnnouncesIsRenewedByItsRow`, `internal/sandbox.TestAWaiterAnnouncementMadeSinceTheClockEndsIt`, `internal/sandbox.TestAnAnnouncementFromAReadBeforeTheClockEndsNothing`, `internal/sandbox.TestAnAnnouncementSaysWhenItsRowWasRead`, `internal/sandbox.TestEveryRoadsAnnouncementSaysWhenItsStoreWasAsked`, `internal/sandbox.TestAnUnclaimedCompletionIsAnnouncedAgain`, `internal/sandbox.TestAnAcknowledgedRefusalOutlivesItsHolder`, `internal/sandbox.TestAnAcknowledgedRefusalIsRecordedOnItsJob`, `internal/sandbox.TestASettledCompletionOwesNoRefusal`, `internal/sandbox.TestARefusalAcknowledgedUnderAnotherAttemptsClaimIsOwed`, `internal/sandbox.TestADrainsRefusalIsHandedOnOwingNothing`, `internal/sandbox.TestAKeptCompletionIsReFiredWithoutASignal`, `internal/sandbox.TestACompletionHeldHereIsReFiredAtItsInstantAndNeverSooner`, `internal/sandbox.TestACompletionTheBudgetHeldOnItsRowIsKeptForTheHold`, `internal/sandbox.TestAHeldCompletionPastItsDeadlineEndsWithinTheBackstop`, `internal/sandbox.TestARefireTheStoreDidNotAnswerIsKeptForItsBackstop`, `internal/sandbox.TestASignalledRefireTheStoreDidNotAnswerIsHeldNoLonger`, `internal/sandbox.TestAKeptCompletionsRefireGoesWithIt`, `internal/sandbox.TestASeatsRecoveryAndTheClaimsOfItsRunsExcludeEachOther`, the `internal/sandbox/sandboxtest` contract cases `AnOwedRefusalIsRecordedOnceAndSettledByThePark` and `AnOwedRefusalIsRecordedOnAClaimOfItsCompletion`, `internal/engine.TestAResumedTurnTakesASlotUnderTheNodesCeiling`, `internal/node.TestADrainingNodeAdmitsNoTurn`, `internal/node.TestADrainWaitsForATurnNoDeliveryStarted`
- **Measured:** a draining engine (2026-10-27): `engine_draining` with two turns in flight, then two `sandbox_waiter_fired` completions 6 and 9 minutes into the drain, each resuming a full turn (the in-flight count rose from 1 to 2, then 3) on a node whose every mailbox was quiesced and whose API answered `api_refused_while_draining`. Both resumed turns ended `sandbox_resume_abandoned`, and the drain completed 16 minutes after it began, waiting on turns it should never have started.
- **Cost-when-tried:** the admission lived inside the mailbox subscription's handler (`node.runTurn`), so it was a property of one road rather than of a turn. The sandbox coordinator re-entered suspended turns from the seat's control topic and from its own answer-retry timer, and neither road passed the ownership check, the `node.max_concurrent` slot or the drain's closed gate. A recorded answer's retry was not even counted by the drain, which waited only for the queue's handlers.
- **Tag-status:** unreleased

## The decision

**A turn starts only under `node.Node.AdmitTurn`.** It is the node's one
admission: the seat owned fresh, a concurrency slot (refused at once while the
node drains), and the drain's cut on the admitted context. Every road to a turn
goes through it: the mailbox's handler, and the sandbox coordinator's resume on
both of its routes (a completion, and a recorded answer, inline or retried),
which asks through `sandbox.CoordinatorOptions.Gate` **before it claims the
run** and holds the admission until the resume and its settle return. A refusal
leaves the work undone for the seat's next holder: a delivery deferred, a
completion's run left unclaimed, an answer kept recorded on its run and waiting
without spending an attempt.

**A refused completion is handed back by the condition its refusal names**
(`sandbox.TurnRefused.Condition`), never by the error behind it. A draining
node defers it, which quiesces the control topic for the rest of the drain and
costs the one delivery every handoff costs. A condition that clears by waiting
(the seat's ownership not yet proven again, or any limit a later change adds)
acknowledges it, spending none of its deliveries and quiescing nothing, because
the waiter that drives the run's box announces a finished run again until a
node claims it, on a schedule that doubles from one poll interval to twenty: the
box host's own
([ADR-0065](0065-a-local-box-is-driven-by-the-node-that-launched-it.md)), save
where that waiter cannot (below).
Only a failure the refusal does not explain is a Nak. A new condition is
handed back by the rule for its kind with no change to the hand-back.

**An acknowledged completion resumes when its condition clears, at the
waiter's next announcement of it or the coordinator's own re-fire at worst.** The coordinator keeps each
completion its gate refused on a condition, and re-fires it when the engine
signals that condition cleared for the seat (`Engine.readmitResumes`, the same
`sandbox.Coordinator.Readmit` that wakes an answer waiting on it, and the hook
whatever ends a new condition calls). That waiter is the backstop for a
clearing nothing signals: it lists the active runs on every pass and polls each
one it drives, whatever its seat is doing, and announces a finished run nobody
has claimed again on its schedule, each announcement due at most twenty poll
intervals after the last (`sandbox.DefaultPollInterval`, 15 seconds, so five
minutes), which bounds the wait. And the coordinator re-fires what it keeps
itself, at the instant the refusal names for its lifting on the clock and
never later than five minutes (`sandbox.Coordinator.keepLocked`), cancelled
when the keep goes: a condition that lifts on the clock with no edge (a lease
merely stale, a provider's limit at its instant) is signalled by nothing, and
while the coordination store does not answer the box's host its waiter
announces nothing either, so the holder that kept the seat waited for neither
for as long as that silence lasted. A completion it holds off itself after its
claim waits for its instant instead (`sandbox.Coordinator.keptRefireIn`): a
re-fire drops the hold, and one at five minutes claimed the job and ran its
review into the same limit every five minutes until that instant. So does a
completion whose resume its seat's budget held on the run's row
(`sandbox.Coordinator.keepHeldOnRow`): its delivery is spent by the hold, and
the claim refuses a held row, so before the instant nothing carried the job
but that host's waiter, and a re-fire then claimed nothing and dropped the
keep; it is kept unheld, re-fired at the hold's instant and never sooner, and
on its condition's signal once the lift of its hold has landed, a signal since
what was read before the resume was admitted, so one that landed during the
resume, with no hold yet to lift, re-fires it too; and the seat's recovery
keeps every held row it finds the same way, and re-arms one whose hold a lift
cleared before its re-fire claimed the job, since the release that writes a
hold records the completion as owed on the job's record too
(`sandbox.PendingStore.ReleaseClaim`); and every lift of a hold a signal
makes re-fires the job too (`sandbox.Coordinator.refireLifted`), since a
lift that found nothing kept, as one racing the seat's recovery can, left the
job to that host's waiter, unless the node carries the job already, kept for
anything but that hold or a re-fire of it on its way, as the seat's recovery
keeps no row whose job it does: neither starts a second re-fire beside one
(`sandbox.Coordinator.carriedLocked`), since a second asked the gate, claimed
and read the row for a job the first carried already; but a keep for the hold
the lift cleared is re-fired in its place, and a re-fire on its way that
leaves the job kept for it is followed by one as it returns
(`sandbox.Coordinator.refired`), since that keep, made against a reading of
the row from before the lift and of the signals from after the signal, waited
for the hold's instant once the hold was gone.
A delivery of it that the hold refuses leaves it kept, and a claim the hold
refuses with nothing kept for the job, whatever was kept in its place having
re-fired into the hold, keeps it again from the row
(`sandbox.Coordinator.keepHeldClaim`). The timer of a completion held here
still fires every five minutes before its instant, and there does only what an
announcement of the job does (`sandbox.Coordinator.recheckHeld`): past the
turn's deadline the run is ended, and before it the hold is kept and the next
backstop armed, so a turn whose deadline passes while no waiter announces the
job ends within five minutes of it, rather than at the hold's instant; and a
hold on the run's row is written no later than the turn's deadline
(`sandbox.Coordinator.unclaimHeld`), since nothing reaches a held row to end
it, so the hold lapses there and its keep re-fires the job, whose resume past
the deadline ends the run. The claim is at-most-once, so any of them arriving
together resume the run once. A seat this node lets go of drops its kept
completions, for the waiter that drives each run's box to announce to the
seat's next holder.

**An acknowledged refusal is an obligation the seat's holder owes, recorded on
the run's row.** The acknowledgement stays: it spends none of the completion's
deliveries. But the waiter that backs it reads the run's row through its node's
coordination store, and a silence of the store's runs alone has no bound (that
node's lease and presence are another stream, so no watchdog ends it and no peer
finds it gone), while the kept completion is the holder's memory, which a
restart or a seat move forgets. So before the control topic acknowledges a
refused completion, the holder records it on the job's record
(`sandbox.LaunchRecord.RefusalOwed`, by `sandbox.PendingStore.OweRefusal`): one
write, the condition and when, taken only where the record owes none, so a
re-announcement refused again writes nothing; the gate's refusal, the holder's
own hold after a claim and a resume reverted on a wait alike, wherever the box
is hosted. A record the store does not take is no acknowledgement: the delivery
is handed back as the store's silence. A draining node's refusal
(`sandbox.Draining`, named beside `sandbox.SeatLost` as the two conditions the
control topic hands on) is never recorded: its delivery is never acknowledged,
so it leaves nothing owed. The write that settles the completion clears it (the park on
the job's question; the next launch and the run's end remove it with the job's
record), and a claim does not: one handed back puts the completion back as it
was, so a refusal acknowledged while another attempt holds the job's claim is
recorded on the claimed row. The seat's recovery, after a restart and on the
next holder alike, re-arms each one it finds for its condition's signal and
offers the completion to its own gate at once, since the recorded condition is
the last holder's and its signal an edge on that node
([ADR-0064](0064-a-model-at-its-limit-parks-the-seats-it-holds-and-a-seat-says-one-steady-thing.md)):
a refusal there is kept for this holder's own condition, whose signal carries
it. The record is an obligation, never a hold: nothing refuses or holds off an
announcement for it.

**The waiter is the backstop while the store answers the box's host, and the
broker is while it does not.** A `direct` or `container` box is driven by its
host's waiter alone (ADR-0065), and while the coordinator keeps a clock on the
store of that host (`sandbox.Coordinator.hostClocked`: the host last answered a
collection that its store did not answer its read of the run's row), or the
run's row says what such a clock would (`sandbox.LaunchRecord.HostStoreSilentSince`,
which every claim's release writes, or the job lost with that host and its
round not yet carried on), that waiter reads the row through the silent store
and announces nothing, and a kept completion is this node's memory alone, which
a restart or a seat let go of forgets and a clearing nothing signals never
re-fires. So a refusal then is
still kept for the signalled re-fire, and its delivery is not acknowledged: the
coordinator marks it `sandbox.ErrUnannounced`, the control topic NAKs it
whatever its condition (a drain's and a seat let go of still defer it), and the
coordinator renews it before its last delivery. A duplicate is harmless, since
the claim is made once, and a re-fire announces nothing afresh while that
delivery is in flight. That waiter's own announcement says when it asked its
store for the row it was made from, which a renewal, reading no row, never
says, so a delivery that is that announcement, made from a read asked after the clock or
the record began, ends the clock, a claim of it clears the record, and a
refusal of it is acknowledged as above: the waiter read the row through the
store they say is silent, and announces the job again from there. One whose
read was asked before ends nothing, however late it was made, since the
waiter polls the box between its read and its announcement.

**A seat's recovery pass is exclusive with every claim of the seat's runs on
the node.** The pass (`sandbox.Coordinator.RecoverSeat`) reads a claim it finds
as one whose node stopped, and reaps it or revives its answer, which is right
only where no attempt of its own node holds the claim. The admission made that
so at the acquisition, which admits no turn of a seat still establishing
(`seat.Host.MayStart`), but not on every road: a completion past its turn's
deadline is claimed to be ended without the admission, and an apply that
brings the sandbox up on a seat already held recovers its runs while the seat
admits turns, so a delivery, a signal's re-fire or an owed answer's attempt
claimed inside the pass, which reaped this node's own live claim under the
review running it. So the acquisition's rule is carried to every claim, at one
place in the claim path (`sandbox.Coordinator.claimGateLocked`, recoverygate.go):
while the seat's pass runs on the node, a claim is refused
(`sandbox.ErrSeatRecovering`), the gate closed before the seat's control topic
is attached; and every claim the gate lets through is counted as the node's
live attempt until it returns, which the reap refuses to touch. A refused
claim is never lost: a delivery is handed back to the queue as the node's
failure, and a re-fire, a held completion's backstop and an owed answer's
attempt are carried by the pass when it is done. It is a gate of the
coordinator's, beside the admission rather than inside it, because the
deadline's claim asks no admission and must still meet it.

**A turn inside an admitted turn reuses that admission, and skips the
ownership check for the inner turn.** The inline answer resume runs on the
delivery that carried the answer, inside the turn that delivery's admission
already covers, on the same seat and node. The ownership check is an
optimization against starting minutes of work on a seat a peer may hold, and
that work has already started: refusing the inner half of a running turn would
not stop it, only strand the answer mid-turn, and taking a second slot for the
one turn would wedge a node whose ceiling is one. Correctness still rests where
it always did, on the lease epoch every claim and write is fenced by, and the
drain's cut still ends the outer turn's context, which the inner one runs
under. The reuse holds only while the outer admission is held, so a context
that outlives its turn admits nothing.

**The drain waits for the gate's turns, not only the queue's handlers.** A turn
holds a slot whatever started it, so the slot count is the one that says
nothing is running.

The roads are pinned rather than listed: `turn.Run` is called from two frames,
each reachable only through an admitted one, and the coordinator's resumer only
from a frame that takes an admission no other frame can mint. The engine's
mailbox turn is unexported, so nothing outside the engine can reach it, and the
guard pins who inside the engine does. Inside `internal/sandbox` the compiler
does not stop a frame declaring an admission and filling it in, so the guard
does: only `admitTurn` may name the type in a value, and only the three frames
that take one may name it in a signature. The guard reads syntax, not types: it
proves that no non-test Go file in the module names a guarded edge outside its
frames, by any import name or through any copy of a receiver, and it does not
prove the absence of a guarded function value handed out of those frames or a
call through reflection.

## Why the obvious alternative is wrong

**Quiescing the control topic in the drain** stops the completion road and no
other: the answer retry runs on a timer, not a delivery, and the next road
somebody adds is a third. It also leaves the resumed turns outside the
concurrency ceiling the node is sized by, drain or no drain.

**Checking the drain flag in the resumer** answers after the claim and the
collection, so every redelivery during a drain would claim the run, read its
box and give the claim back. It is also a second admission beside the node's,
which is how the first one came to cover a single road.

## What this does not decide

Whether a seat's own conditions (a person's pause, a budget, the config
posture) apply to a completion's resume: they apply to an answer's retry, which
runs outside the delivery that would have screened it, and this record leaves
both as they were. The dashboard assistant's turns are a person's, not a seat's,
and are refused at the API's drain gate; they are outside this admission.
