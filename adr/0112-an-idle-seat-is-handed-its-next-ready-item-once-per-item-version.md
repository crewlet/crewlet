# ADR-0112: An idle agent seat is handed its next ready item, once per item version

- **Status:** accepted
- **Authority:** `internal/pickup`
- **Enforced-by:** `internal/pickup.TestAPickupIsKeyedOnTheItemsVersionBySomebodyElse`, `internal/pickup.TestASeatThatIsNotIdleIsHandedNothing`, `internal/worklet.TestReadyWorkComesStartedFirstThenTheListThenTheQueue`, `internal/worklet.TestReadyWorkLeavesOutWhatCannotStartNow`, `internal/pickup.TestAnIdleSeatIsHandedItsReadyItemOnce`, `internal/pickup.TestAPickupThatCannotBeSentIsTriedAgain`, `internal/pickup.TestADeclinedItemGivesWayToTheNext`, `internal/pickup.TestThePickupReadsPastTheFirstPageAndRemembersAnAllOfferedSet`, `internal/engine.TestAPickupIsWorkedForWhomItsItemIsFor`, `internal/agent/builtin.TestAPickupTurnIsHeldAsTheAssignmentTurnIs`
- **Measured:** on a company running this engine, over about five hours: 200 turns, a median of 20 seconds and none over five minutes, while 28 items assigned to agents, none of them waiting on an open blocker, sat untouched for 20 to 260 hours, and several items in progress had no run behind them.
- **Cost-when-tried:** the workaround that company ran, a schedule per role whose task text said "pick up your next ready item", fired on a clock whether or not the seat was busy, could not tell which item it meant, and woke a seat with nothing ready as often as one with ten.
- **Tag-status:** unreleased

## The decision

**The holder of an agent seat derives one wake for it, a pickup, when the seat is idle and holds ready work.** Every node that publishes runs a loop over the agent seats it holds, once a minute. A seat is idle when no turn of it is dispatching, no executor run holds it or waits on a person's answer, nothing waits in its inbox, it is not paused, no budget or provider park holds it, no budget window or provider limit would refuse its next launch, and the node is neither shedding nor draining, and all of that held on the previous pass too. An idle seat is handed its first ready item: work it already started, then its priority list in its lead's order, then the rest by priority and due date. Ready is assigned to the seat, `in_progress` or `todo`, waiting on no open blocker, past its start date, and not held on a question the seat itself asked on it.

**A pickup is made once per (seat, item, the last change somebody else made to the item, status, and for started work the seat's own last progress on it other than a comment).** Its event id is derived from that key, so the fleet's completion ledger refuses a pickup the seat already had its turn on, whichever node holds the seat and across restarts, for the ledger's retention of seven days. A fleet claim on the same key keeps two nodes that both believe they hold the seat from publishing it twice. A seat that declines an item, records a blocker on it or comments on it is not handed it again until somebody else changes it or its status moves; started work the seat makes progress on is handed back when the seat is idle again, and a turn that changed nothing does not re-arm it. The ready order is read page by page, so item twenty-one onwards is offered once the first twenty were, and a seat whose whole ready set was already handed over costs no ledger read until the set changes.

**The cooldown spaces declines, not work.** A fleet claim on the seat, `scheduling.work_pickup.cooldown_seconds`, is taken only when a pickup passes over an item the seat was already handed and left unchanged, so a seat that finishes items is handed the next at once.

**The pickup's turn works for whom the item is for (ADR-0109).** The wake carries the writer of the item's last assignment by somebody else, else its reporter, and a credential with the apps it wrote under, re-narrowed on the node running the turn, so the turn is held exactly as the assignment's own turn was.

**On by default**, with a ten-minute cooldown (`scheduling.work_pickup`). The wake is a `task_assigned` carrying the item, its status, its key and why it was chosen, stamped with the instant of the change its key names and filed under the item's own conversation, so the turn continues the item's session; its brief names the item and the four honest ways to answer it. `crewlet.work.pickups{outcome}` and the `work_pickup_woken` and `work_pickup_skipped` lines are what an operator reads.

## Why the obvious alternative is wrong

**The obvious alternative is to wake the assignee again from the change feed**, on a timer per assignment, as [ADR-0012](0012-a-wake-is-derived-not-published.md) derives every other wake from a committed record. It is wrong because the record that should cause the wake does not exist: what went wrong was that the seat's turn did not start the work, and nothing is written when a turn decides to do something else. A timer per item wakes a busy seat as readily as an idle one, and a seat with ten ready items ten times.

**Re-waking until the item moves** is the other half of the obvious answer, and it is the loop this replaces: a seat that has a reason not to start an item says so every time it is asked. Keying on the version somebody else last wrote makes "no" an answer that holds until the facts change, and makes the seat's own comment on the item not count as a change.

## What this does not decide

It does not decide what a seat does with the item; the brief names the choices and the delivery obligation of a `task_assigned` holds the turn to a tool. It does not change how an assignment, a status change or the unblocked repair wakes a seat: those still wake from the change feed, and the pickup is the backstop for the turn that did not act on them. It does not cover a seat on a vendor tracker, which the engine does not read for ready work.
