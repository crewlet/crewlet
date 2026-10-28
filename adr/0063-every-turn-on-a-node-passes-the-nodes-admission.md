# ADR-0063: Every turn on a node passes the node's admission

- **Status:** accepted
- **Authority:** `internal/node`
- **Enforced-by:** `internal/node.TestEveryTurnStartIsAdmitted`, `internal/node.TestTheMailboxTurnIsAdmittedBeforeItRuns`, the unexported `admitted` parameter of `sandbox.Coordinator.resumeAndSettle`, `internal/engine.TestADrainingNodeLeavesACompletionForTheSeatsNextHolder`, `internal/node.TestADrainingNodeAdmitsNoTurn`, `internal/node.TestADrainWaitsForATurnNoDeliveryStarted`
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
leaves the work unspent for the seat's next holder: a delivery deferred, a
completion handed back (deferred on a draining node, which quiesces the control
topic for the rest of the drain), an answer kept recorded on its run and waiting
without spending an attempt. A turn inside an admitted turn (the inline answer
resume, on the delivery that carried it) reuses that admission while it is held.

**The drain waits for the gate's turns, not only the queue's handlers.** A turn
holds a slot whatever started it, so the slot count is the one that says
nothing is running.

The roads are pinned rather than listed: `turn.Run` is called from two frames,
each reachable only through an admitted one, and the coordinator's resumer only
from a frame that takes an admission no other frame can mint.

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
