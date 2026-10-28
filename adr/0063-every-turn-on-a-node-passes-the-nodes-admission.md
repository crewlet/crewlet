# ADR-0063: Every turn on a node passes the node's admission

- **Status:** accepted
- **Authority:** `internal/node`
- **Enforced-by:** `internal/node.TestEveryTurnStartIsAdmitted` and `internal/node.TestTheAdmissionGuardFindsEveryRoad` (a syntactic guard: every non-test Go file in the module is parsed, the root package included; `turn.Run` is matched under whatever name a file imports the turn package by and refused through a dot import; every other edge of the two chains to a turn is an unexported name matched on any operand; and the coordinator's `admitted` type may be named, in a signature or a body, only by `admitTurn`, which makes it, and the three frames that take it, each of which is called only from its one admitting caller; it does not see a guarded function carried out of its pinned frames as a stored method value, or a call made through reflection, which review holds), `internal/node.TestTheMailboxTurnIsAdmittedBeforeItRuns`, the compiler, outside the two packages (the mailbox's turn is the unexported `engine.Engine.mailboxTurn`, and `sandbox.Coordinator.resumeAndSettle` takes the unexported `admitted`; inside `internal/sandbox` only the guard holds who makes one), `internal/engine.TestADrainingNodeLeavesACompletionForTheSeatsNextHolder`, `internal/engine.TestARefusedCompletionIsHandedBackByItsCondition`, `internal/engine.TestACompletionRefusedOnAWaitResumesOnceItClears`, `internal/sandbox.TestARefusedCompletionIsReFiredWhenItsConditionClears`, `internal/sandbox.TestAnUnclaimedCompletionIsAnnouncedAgain`, `internal/engine.TestAResumedTurnTakesASlotUnderTheNodesCeiling`, `internal/node.TestADrainingNodeAdmitsNoTurn`, `internal/node.TestADrainWaitsForATurnNoDeliveryStarted`
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
the waiter that drives the run's box announces a finished run again on every
poll until a node claims it: the fleet's for an `e2b` box, the box host's own
for a `direct` or `container` one
([ADR-0064](0064-a-local-box-is-driven-by-the-node-that-launched-it.md)).
Only a failure the refusal does not explain is a Nak. A new condition is
handed back by the rule for its kind with no change to the hand-back.

**An acknowledged completion resumes when its condition clears, within one
poll at worst.** The coordinator keeps each completion its gate refused on a
condition, and re-fires it when the engine signals that condition cleared for
the seat (`Engine.readmitResumes`, the same `sandbox.Coordinator.Readmit` that
wakes an answer waiting on it, and the hook whatever ends a new condition
calls). That waiter is the backstop for a clearing nothing signals: it lists
the active runs on every pass and polls each one it drives, whatever its seat
is doing, so the wait is bounded by its poll interval (`sandbox.DefaultPollInterval`, 15 seconds). The
claim is at-most-once, so the two arriving together resume the run once. A
seat this node lets go of drops its kept completions, for the waiter that
drives each run's box to announce to the seat's next holder.

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
