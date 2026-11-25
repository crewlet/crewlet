# ADR-0159: Urgent ready work comes before started work

- **Status:** accepted
- **Authority:** `internal/worklet`
- **Enforced-by:**
  - `internal/worklet.TestUrgentWorkComesBeforeStartedWork`
  - `internal/worklet.TestReadyWorkComesStartedFirstThenTheListThenTheQueue`
  - `internal/events/types.TestAPickupsBriefSaysWhereUrgentWorkSits`
  - `internal/pickup.TestAnUrgentItemIsHandedToABusySeatOnItsPriorityLane`
  - `internal/pickup.TestAnUrgentItemWaitsForWhatHoldsTheSeat`
  - `internal/pickup.TestAnUrgentItemIsHandedWhileTheSeatsExecutorRuns`
  - `internal/pickup.TestAnUrgentPickupIsTheBusySeatsNextTurn`, the `queuetest` conformance suite every backend runs (`Lane/a_priority_event_published_mid_drain_is_the_next_partition`), `internal/notify.TestASourcesLaneRuleKeepsAPersonsChangeOffTheLane`
  - `internal/worklet.TestOnlyAPersonsCommentRidesTheLane`
- **Measured:** on a company running this engine, an item assigned to an agent seat as `todo` with priority `urgent` waited more than four hours while the same seat was handed, pass after pass, three `high` items it had already started; two mentions on the urgent item did not change the order. Once the seat had started it, its next turn was an older queued assignment wake for a `high` item, one of hundreds the seat had queued. Marked for the lane, it still waited hours: a drain read the lane only when it opened and then ran up to twenty queued partitions, each a turn, and the lane itself held dozens of tasks people had filed or assigned (one seat showed 79 such wakes against 9 background ones), each ahead of the pickup and of a person's later mention. On a seat whose every turn runs in an executor box, an urgent item filed while a run held the seat was handed nothing for more than two hours, through one run of almost an hour and then dozens of short ones: the pass read each run as a hold, so it could hand the item only in the moments between two runs, and its once-a-minute pass landed in none of them.
- **Tag-status:** unreleased

## The decision

**An urgent ready item comes before every ready item that is not urgent; within the urgent items, and within the rest, the order is [ADR-0112](0112-an-idle-seat-is-handed-its-next-ready-item-once-per-item-version.md)'s.** That order is started work, then the reviews the seat holds, its own items in review nobody reviews, its waits on a person nobody asked, its dead waits, its priority list in its lead's order, then the queue by priority and due date. So a seat finishes urgent work it started before it starts another urgent item, and finishes other started work before it starts anything else that is not urgent.

**An urgent ready item does not wait for the seat to be idle, and rides its priority lane.** A seat's background wakes (an assignment, a status change, a colleague's comment) drain in arrival order, and a seat carrying hundreds of them is never idle with an empty inbox, so a pickup that waits for both never comes. So when a seat's first ready item is urgent, the pass hands it even while a turn or its executor's run is in flight, while mail waits or before the settle, and the pickup is marked for the inbox's priority lane (`queue.Prioritize`). A drain takes the lane first and asks it again before each partition it runs, so a lane wake that lands while a turn runs is the seat's next turn, ahead of what the drain still holds: the seat's next turn is the urgent one. The lane holds a person's words and urgent pickups; a task a person files, assigns or moves for the seat is its work, which this order hands, so its wake rides with the background (`notify.LaneRule`). A run in its box is a turn in flight: the inbox holds its mail, lane included, until the run settles, so the urgent pickup waits there as it would behind a turn. Every other hold stands: a seat not held here, paused, without a model, whose run waits on a person's answer (the seat's next delivery is offered as that answer), parked or refused is handed nothing. An urgent pickup handed while the seat is idle rides the lane too. The pickup's claims, ledger and cooldowns are unchanged, so an urgent item a seat makes progress on comes back at most once per cooldown, as started work does.

An urgent item the seat has not started is handed with its own reason, `urgent`, and its brief says it comes before every item that is not, started work included. The briefs for started work and for a review say they come before unstarted work unless that work is urgent. `orderReady` in `internal/worklet` is the one place the order is computed; the pickup, its brief and the docs read it.

## Why the obvious alternatives are wrong

**Started work first for every priority** is what ADR-0112 set, and it is right below urgent: a seat that started an item keeps its work in progress bounded, and a priority edit does not make it start another. But a started item is handed back once per cooldown while the seat makes progress on it, and a cooling item gives way to the next ready item, which is the next started one. A seat holding three started items is therefore handed one of them on nearly every idle pass, and nothing it has not started is reached until all of them are cooling. `urgent` exists to say "now", and that order made it mean nothing to a seat with started work.

**Any unstarted item whose priority is above every started item's** would let `high` work interrupt `normal` work, and `normal` interrupt `low`. Each edit of a priority would then start another item and leave the last one in progress, the thrash finishing started work prevents. Urgent alone pre-empts because it is the one level whose meaning is to interrupt; a company that never marks an item urgent sees ADR-0112's order unchanged.

## What this does not decide

It does not change what is ready, the pickup's key, its cooldowns, or whether an assignment or a comment wakes a seat (ADR-0112, ADR-0122). It does not stop a turn in flight: the urgent pickup is taken by the seat's next turn. The lane keeps its own age order, so an urgent pickup follows a person's message already waiting there.
