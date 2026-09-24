# ADR-0022 — A turn is charged to one work item, per segment, and never split

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestTaskSpendNeverExceedsTheRollup`
- **Cost-when-tried:** `tracker.Writer.RecordTurn` existed with the applier behind it and had no caller, so every task's `spend_*` columns read zero for every task in every company while the docs promised "spend is on the task". The turn records that should have named the item declared a `task_id` nothing assigned. And the applier gated a purge's marker for the task's own records only: a turn decided while its task existed and landing after the task's purge found no row, which the applier reads as a writer's bug and stops on — on every node, since every node applies the same log in the same order.
- **Tag-status:** unreleased

## The decision

A turn is charged to **one work item or to none**, and never split between
two. The item is resolved by four rules, tried in order — `trigger`,
`asked_by`, `resume`, and at completion only `sole_write` — in
`internal/engine/worksubject.go`, and every turn-level record carries the
answer as `work_item` and `work_item_basis`.

The charge is made **per completed segment**, not per turn. A turn that
detaches a coding run completes once when it parks and again each time a run
it launched is collected, so each segment charges what it spent under an
operation id naming the segment — `turn/<run>/dispatch` or
`turn/<run>/resume/<launch>` — which is also the id of the turn's row on the
task. Only the dispatch segment counts a turn. A segment's tokens are its own
phases as their records state them, its delegated workers, the round-cap judge
and, on a resumed segment, the coding run it collected; learning after the turn
is not charged to the item. A parked segment charged to nothing hands what it
spent on with the suspended conversation, and the segment that finishes the
turn pays it when a sole write charges the turn.

Only a **native** item is charged: the counters are rows in the engine's own
tracker. The write is `tracker.Writer.RecordTurn`, the one additive record,
which refuses a task the writer cannot see or one that was purged and charges a
tombstoned one; a turn that lands after its task's purge is read past by the
deletion gate, never applied.

## Why the obvious alternatives are wrong

**Splitting a turn across the items it touched** needs a ratio, and nothing in
a turn states one: a turn that read three tasks and updated one did not spend a
third of itself on each. Any ratio the engine invented would be a number no
person could check against the turn that produced it.

**Charging once per turn, at its end,** loses the half of every parked turn
that ran on another node, and cannot be made idempotent without a record of
which segments were already summed — which is the per-segment id this uses
instead.

**Summing the turn's completion events into the task at read time** puts a
scan of the event log behind every board sort by spend, and every node's event
log holds only what that node ran.

## What this does not decide

It does not decide how a task's turns are listed or numbered on a screen, nor
which answers read the counters — those are the tracker reader's and the API's.
It does not decide what a seat's or the company's budget counts: those are
ADR-0019's windows over the same spend, charged per call rather than per item.
