# ADR-0019 — A token budget is a periodic window, and the window's turnover is its only reset

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** `internal/coord/coordtest.TestAWindowRollsAtItsBoundary`, which runs the rollover case every backend's contract run runs too
- **Measured:** 32 concurrent charges on one counter key exhausted sixteen immediate compare-and-swap retries in four runs of ten on the embedded broker; with a jittered wait of 1 ms doubling to 32 ms between attempts, none failed in 200. The longest window is a calendar month, 31 days and at most an hour of clock change, so a counter's record is kept 32 days past its last write.
- **Cost-when-tried:** the counter was one lifetime figure per scope, in a bucket with no age, cleared only by an operator's `crewlet budgets reset`. A budget meant as an allowance per month or per day stopped the company for good the first time it was spent, and came back only when somebody zeroed the counter by hand, on a date nobody chose; the bucket kept a record for every seat that had ever run. When the budget became `{day, week, month}`, a counter with no calendar could only hold each scope to its tightest ceiling (`engine.counterCeiling`), which never let a window's allowance come back: a company capped at three million a day was refused for the rest of the deployment the first day it spent three million.
- **Tag-status:** unreleased

## The decision

A token budget is a set of ceilings **per calendar window** — the day, the ISO
week from Monday and the calendar month, cut on the company's one clock
(ADR-0018) — and the fleet counts spend per window. `coord.Budgets` keeps
**one record per scope** holding **a slot per period**: the label of the
window it counts (`2026-09-23`, `2026-W39`, `2026-09`), what has been spent in
it and when it last refused a charge. A charge is admitted only while every
capped window of the company and of the seat has room, and it is still one
compare-and-swap per scope: org first, compensated if the seat refuses.

**The roll is the reset.** A slot whose label is earlier than the charge's
window is rolled — label moved on, spend and refusal cleared — inside the same
compare-and-swap that counts the charge. There is no scheduled reset, no
`POST /budgets/reset` and no `crewlet budgets reset`: room is made by raising a
ceiling, or by the window turning over. A slot never rolls back, so a node
whose clock trails a peer's across a boundary counts in the window the counter
is already on — and a read behind that slot answers the later window with its
spend, never the earlier one unspent, so no reader of the counter is shown room
the next charge would be refused.

**The calendar is the caller's.** The windows travel in on every charge and
every read (`coord.Windows`), as the ceilings do; the store holds labels and
spend and never reads a clock to decide which window a charge belongs to. A
turn is charged in the windows current when each round is charged, on the clock
of the epoch it is pinned to; a detached coding run in the windows it is
collected in.

The counters live in `<prefix>_token_windows`, a bucket aged at
`coord.BudgetRetention` (32 days). The lifetime counters' bucket,
`<prefix>_budgets`, is read by nothing and deleted by the maintenance duty once
no live lease is held below `coord.WindowedCountersProtocol` — the seat-host
protocol moved to 4 for this, because a v3 node and a v4 node running seats
side by side would each charge a different counter and every cap would bind
late by what the other build spent.

## Why the obvious alternative is wrong

The obvious alternative is a SCHEDULED reset: keep one figure per scope and
zero it at midnight, on Monday and on the 1st. It needs a singleton to run it,
which moves on a lease and can miss a boundary while it moves; a reset that
runs a minute late lets a company spend a minute of tomorrow against today's
ceiling, and one that runs twice zeroes spend that already happened. And one
figure cannot answer for three windows at once — a charge on the 31st counts
toward that day, that week and that month, and the week outlives the month.
Carrying the window's label in the slot makes the roll a string comparison
inside a write that already happens, on whichever node charges next, with
nothing to schedule and nothing that can run twice.

The second is a counter per window, keyed by label. That is three records per
scope and three compare-and-swaps per charge, and the org-first compensation
then has to unwind across as many as six keys; a charge refused by the month
after the day and the week were written leaves two to take back.

## What this does not decide

It does not decide how a refusal is surfaced to a seat, whether an exhausted
seat is parked, or how the windows are drawn on a dashboard — those are the
engine's and the API's, over the answers this contract gives.

It does not make the window a retention horizon for anything else, and it
does not make the counter a spend history: a counter knows the current window
of each period and nothing before it.
