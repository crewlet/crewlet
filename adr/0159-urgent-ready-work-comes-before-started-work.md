# ADR-0159: Urgent ready work comes before started work

- **Status:** accepted
- **Authority:** `internal/worklet`
- **Enforced-by:** `internal/worklet.TestUrgentWorkComesBeforeStartedWork`, `internal/worklet.TestReadyWorkComesStartedFirstThenTheListThenTheQueue`, `internal/events/types.TestAPickupsBriefSaysWhereUrgentWorkSits`
- **Measured:** on a company running this engine, an item assigned to an agent seat as `todo` with priority `urgent` waited more than four hours while the same seat was handed, pass after pass, three `high` items it had already started; two mentions on the urgent item did not change the order.
- **Tag-status:** unreleased

## The decision

**An urgent ready item comes before every ready item that is not urgent; within the urgent items, and within the rest, the order is [ADR-0112](0112-an-idle-seat-is-handed-its-next-ready-item-once-per-item-version.md)'s.** That order is started work, then the reviews the seat holds, its own items in review nobody reviews, its waits on a person nobody asked, its dead waits, its priority list in its lead's order, then the queue by priority and due date. So a seat finishes urgent work it started before it starts another urgent item, and finishes other started work before it starts anything else that is not urgent.

An urgent item the seat has not started is handed with its own reason, `urgent`, and its brief says it comes before every item that is not, started work included. The briefs for started work and for a review say they come before unstarted work unless that work is urgent. `orderReady` in `internal/worklet` is the one place the order is computed; the pickup, its brief and the docs read it.

## Why the obvious alternatives are wrong

**Started work first for every priority** is what ADR-0112 set, and it is right below urgent: a seat that started an item keeps its work in progress bounded, and a priority edit does not make it start another. But a started item is handed back once per cooldown while the seat makes progress on it, and a cooling item gives way to the next ready item, which is the next started one. A seat holding three started items is therefore handed one of them on nearly every idle pass, and nothing it has not started is reached until all of them are cooling. `urgent` exists to say "now", and that order made it mean nothing to a seat with started work.

**Any unstarted item whose priority is above every started item's** would let `high` work interrupt `normal` work, and `normal` interrupt `low`. Each edit of a priority would then start another item and leave the last one in progress, the thrash finishing started work prevents. Urgent alone pre-empts because it is the one level whose meaning is to interrupt; a company that never marks an item urgent sees ADR-0112's order unchanged.

## What this does not decide

It does not change what is ready, the pickup's key, its cooldowns, or how an assignment or a comment wakes a seat (ADR-0112, ADR-0122).
