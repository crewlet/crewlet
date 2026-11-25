# ADR-0162: An item in progress says why it is not moving, and a stall is told, never a status change

- **Status:** accepted
- **Authority:** `internal/worklet`
- **Enforced-by:** `internal/worklet.TestAnItemInProgressSaysWhyItIsNotMoving`, `internal/worklet.TestTheFlowIsReadInThePagesOwnStatement`, `internal/worklet.TestWorkloadCountsOnlyWorkThatWantsTheSeat`, `internal/worklet.TestADueItemComesRightAfterUrgentWork`, `internal/engine.TestAWakeWhoseRelationEndedStartsNoTurn`, `internal/engine.TestAStateNoticeATurnOnItsItemReadPastStartsNoTurn`, `internal/engine.TestAPickupMovesItsItemsSeenThroughMark`, `internal/pickup.TestADueItemIsHandedToABusySeatOnItsLane`, `internal/pickup.TestALanePickupWaitsForAnEmptyLane`, `internal/worklet.TestARemovedSeatsWorkIsReturnedOrHandedToItsLead`, `internal/maintenance.TestARetirementReHomesTheSeatsWorkBeforeItDeletesItsMailbox`, `internal/api/livestate.TestAFlowReadsTheSeatsStopReasonAndItsReset`, `internal/agent/builtin.TestTheWorkToolsSayWhyAnItemIsNotMoving`, `internal/worklet.TestAFlowRecordMovesNothingTheItemOwns`, `internal/worklet.TestALateCheckIsToldOncePerStart`, `internal/worklet.TestAForgottenItemIsToldThenItsLead`, `internal/worklet.TestAHeldItemTellsTheLeadOnlyWhenItCosts`, `internal/engine.TestAFlowNoticeWhoseItemLeftItsStateStartsNoTurn`, `internal/engine.TestTheFlowDutyIsHeldWhereThePostureIsLive`, `internal/worklet.TestABackfilledFlowIsTheRecordedFlow`, `internal/worklet.TestTheBackfillRecordsThePastBeforeTheDutyOnce`, `internal/engine.TestASeatsStopsAreItsPausesAndParks`, `internal/worklet.TestTheDashboardDrawsEveryFlowStateAndCause`, `internal/worklet.TestTheBackfillLeavesTheStateTheDutyFirstReadToTheDuty`, `internal/worklet.TestADepartureTellsEachLeadOnceOfItsUnownedWork`
- **Measured:** on a company running this engine, one agent seat's background mailbox held more than 300 Worklet wakes for hours and drained at about one a minute, a model turn each; many named a dependent that had already finished, and several queued wakes about one item each cost a turn of their own. An item its agent parked for a day until a release's health check (`start_at` and `wait_reason` set) read `in_progress` on every list and counted as load on Workload; when the start passed while the seat was busy, the check ran hours late and nothing showed it or told anybody. An urgent item assigned to a seat out of its model limit read a bare `in_progress`. Items assigned to an agent seat removed from the chart kept its handle with nobody to work them.
- **Cost-when-tried:** the two shapes this replaces ran here. A passed start was handed only to an idle seat with both mailboxes empty ([ADR-0112](0112-an-idle-seat-is-handed-its-next-ready-item-once-per-item-version.md)), so a seat with a backlog never reached it. A Worklet notice about work a seat holds was an obligation [ADR-0120](0120-a-wake-a-completed-turn-already-read-past-starts-no-turn.md) never absorbs, so every queued `assignee` and `blocking` notice on an item cost a turn after a turn on that item had read past it.
- **Tag-status:** unreleased

## The decision

**An item's status never changes because no turn is running. The engine derives a read-only flow state for every item whose status is `in_progress`, in one place, serves it wherever items are read, records each change of it, and tells a person when the state means nobody is moving the item.** Four further rules make the state true: a due item is woken on the seat's priority lane, a wake about held work that asks nothing any more starts no turn, a notice is told once per item and state, and a removed seat's work is re-homed after the grace its mailbox has.

### The flow state

The first state that holds wins:

| State | Holds when | `since` / `until` |
|---|---|---|
| `working` | a turn or an executor run of the assignee is on the item now | the turn's start |
| `blocked` | the item has an open blocker | (recorded) |
| `waiting` | its start is in the future, or the assignee asked a question on the item nobody answered, or a run of the assignee is parked on the item waiting on a person | `until` the start; `reason` the wait's words |
| `held` | the assignee cannot take a turn: paused, unplaced, out of its token budget, out of its model limit, waiting on a person's answer in a run on another item, gone from the chart, or nobody | `until` the limit's or window's reset where one is known |
| `due` | its start has passed and no turn of the assignee on the item has ended since | `since` the start |
| `stuck` | nothing changed on the item for the project's window | `since` the last change |
| `moving` | none of the above | `since` the last change |

`waiting` comes before `held` because a hold matters only to an item that wants a turn now, so an item parked until tomorrow on a seat out of its limit until noon is waiting, and the same item is held once its start passes and the limit has not lifted. `held` therefore never counts an item that would not have moved anyway, and a notice about a hold names work the hold is actually delaying. A seat's load is `working + moving + due + stuck`: the in-progress items that want the seat now. `moving` is the ordinary case and draws no badge.

The flow is **one SQL expression built by one function in `internal/worklet`**, evaluated inside the statement each reader already runs: a list page's arms, the detail read, `my_work` and ready work, Workload's one `GROUP BY` (a bucket per state, plus `load`), and the flow duty below. The estate's facts are columns and index-served correlated subqueries limited to in-progress rows (the open blocker, the start, the wait's words, the assignee's open ask, the assignee's last turn segment on the item in `tracker_turns`, the chart's seat places, the project's window). The live facts enter as bound parameters from the seat posture: which items have a turn or run on them, and which seats are stopped and why. The posture is the one ranking of a seat's stop reasons the engine already has (`livestate.stateOf`: paused, then unplaced, then budget, then provider), read through an interface `internal/worklet` declares. A node whose projection has not been seeded evaluates the expression with no live facts and its answer says so (`flow_live: false`), rather than reading every stopped seat as moving.

The answer is `flow: {state, cause, since, until, reason}` on every row and detail of an item in progress, and `flow: null` on any other; `reason` carries somebody's words and is declared `untrusted:"name"`. The dashboard draws what it is sent: the closed sets of states and causes live in `dashboard/src/contract`, held to the Go constants by the client gate, and no screen compares a start with its own clock. Every new wire field passes the gate that refuses `omitempty` on a field whose zero value is meaningful.

### A due item is woken on the seat's priority lane

The work pickup hands an in-progress item whose start has passed, and whose pickup for that start has not been made, to a seat that is busy, on its priority lane, exactly as it hands urgent work ([ADR-0159](0159-urgent-ready-work-comes-before-started-work.md)): the same predicate decides when a seat may be jumped, and every hold that stops an urgent pickup stops this one. In the ready order a due item comes after urgent work and before every other item. Its brief says the wait ended, the date and the wait's words. An engine pickup, urgent or due, is put on the lane only while the lane is empty, so at most one waits there and a person's message waits behind at most one such turn.

### A wake about held work that asks nothing starts no turn

Every Worklet wake already carries its item, the position of the change it tells (`trigger_position`), its reason, its change kind and its conversation. A `blocking` wake gains the dependents it names, which its record holds and the wake drops today. At dispatch, before any model call, a state notice (a wake under `assignee`, `blocking`, `unblocked` or `flow` that carries no comment) is:

1. **dropped as moot** when what it is about no longer asks the seat anything: the item was finished, removed or reassigned (#1552's rule); every dependent a `blocking` wake names is finished, removed or no longer waits on the item; an `unblocked` item has an open blocker again; a flow notice's item has left the state it reported;
2. **absorbed** when it sits at or below the seat's seen-through mark for the item's conversation (ADR-0120). Its obligation is the item, which the pickup owns, so a completed turn on the item discharges it. `Reason.Addressed` keeps its meaning for prompts and the working indicator; absorption asks one question of its own;
3. otherwise **delivered**, coalesced with its partition as today.

Words are never dropped or absorbed by state. A pickup stamps the item's Worklet conversation and the position its key names, so a pickup's turn moves the same mark. A backlog of notices about one item costs at most one turn, and a notice about finished work costs none.

### Notices

The flow duty, a fleet singleton every minute, records and tells. It reads every item in progress on each tick, one statement per page on one node: a flow state changes with time and with seats as well as with writes, and nothing written marks those changes, so its cost is the company's work in progress rather than its trouble. It tells:

- **Due**, once the start is 30 minutes past. The grace is borrowed ([ADR-0015](0015-an-alarm-borrows-its-threshold.md)): it is the inbox's acknowledgement window, which the engine already sizes as a wait plus a worst-case turn, so a lane pickup handed at the start has had every turn the engine expects to run first.
- **Stuck**, when it becomes stuck.
- **Held**, to the lead only, and only when the hold costs something: the item is urgent, or it would be due past the grace or quiet past the window were it not held. A hold that ends on its own on an item nothing waits for asks nobody to act.

A person assignee is told first and the project lead one threshold later if the item is still in the same state since the same instant. An agent assignee's notice is its pickup, which carries the typed-answer contract a notice cannot, so the first person told about an agent's item is its lead. A notice is a flow record carrying a tracker notification under the reason `flow`: an Inbox row for a person, a background wake for an agent lead. Its id is derived from the item, the state, the instant it entered it and the recipient, so it is told once per item and transition, and an item that leaves the state and enters it again may be told again.

### A removed seat's work

An item whose assignee is not in the chart reads `held` with the cause `departed` at once, from the roster's seat places. When the seat's retirement runs (absent from the active revision for 24 hours, judged as its mailbox is, unknown never absent, under the seat's lease), each open item it holds is re-homed by an engine write with a history note naming the seat: an item in progress with a live side effect goes to the project lead with its status kept; any other item in progress goes back to `todo`, unassigned; an item in review goes to the lead; an item to do is unassigned. **A live side effect** is something outside the item's own fields that will come back to it: a wait with a date (waiting or due), the seat's open question on the item, a blocker that is not a work item (a pull request it waits on), or a run of the seat on the item that the retirement ended. With no project lead, the routing unit's lead takes it; with neither, it is returned unassigned and the retirement logs it. A seat restored within the grace keeps its work.

### What is recorded

Each change of an item's flow state, `working` aside, is a `flow` record the duty publishes on a subject of its own per item, applied into `tracker_flow`. It never touches the item's subject, version or `updated_at`, so recording a state does not change the state it records or wake anybody. A row carries the state, its cause, the instant the facts give for it (the start, the last change, the hold's start; never the tick's), `until`, the seat, the notice step and whether it was backfilled. Working time is the turns' own record in `tracker_turns` and is not copied. The item's history view merges both. A backfill job, gated on its own cursor and idempotent through the derived ids, evaluates the same expression over each item's past from its history, its turn segments and the seat events the fleet's event stores still retain, and says which facts it had.

Each project carries `stuck_after_hours`, 1 to 168, absent meaning 8.

### What it costs the store and the log

No reader adds a statement per row: the expression's subqueries are index seeks evaluated only for rows in progress, the posture is at most a parameter per seat, and a plan test holds each reader to its index. The replicated estate gains `tracker_flow` (by item and instant) and a partial index over items in progress for the duty; `tracker_turns` gains an index on item, seat and instant only where the plan test shows the existing one cannot serve. The project field and the `flow` record's payload are version-gated fields, so the record version moves (to 19, and to 20 if they land apart) and an older data node retains those records rather than applying them half. The pickup event gains the date it waited until, the wait's words and the position its key names, and a `blocking` wake the dependents it names, both additive. The reason `flow` is one more routing reason, in the precedence after `thread`.

## Why the obvious alternatives are wrong

**The engine moving the status** (to blocked, to `todo`) rewrites what the assignee said about the item with what a clock inferred, wakes everybody the status change wakes, and moves the pickup's key, so the item is handed again for the engine's own write.

**Deriving the state where it is drawn** is what the item page does today, comparing a start with the browser's clock. The agents' tools and the lead's Workload would each need a copy, and the copies would disagree at every boundary.

**Storing the state on the task row** needs a writer for transitions nobody writes (a start passing, a seat's limit lifting), moves the row's version and `updated_at` so the stuck clock resets itself, and is only as fresh as its last sweep.

**Superseding a wake in the queue** (one undelivered wake per item and reason) changes the contract of both queue backends for what absorption at dispatch already achieves with a fact every wake carries.

**Telling an agent assignee with a notice** gives it a second, weaker way to hear about work its pickup already hands it, without the typed answer.

**Re-homing on the apply that removed the seat** loses work to every reverted edit; the retirement's grace exists because removals are often mistakes.

## What this does not decide

It changes no status except a removed seat's re-homing, which is a write with a history note. It does not restart a paused seat, warn when every seat shares one model account, page anybody outside the Inbox and the agents' inboxes, or judge whether a moving item's work is good. Items to do or in review have no flow state. It does not change the pickup's daily recheck ([ADR-0122](0122-a-pickup-is-answered-with-a-typed-outcome-and-a-wait-on-a-person-is-an-ask.md)), the lane's age order, or what a Worklet wake wakes.

## Amendment: a departure tells each lead once of the work it left without an owner

Work left without an owner reaches its lead, and a seat's departure must not wake that lead once an item. The re-homing writes name no lead to fall back on. For each lead (the project's, else the routing unit's) of the items a departure leaves unassigned, the last such item's record names that lead as an ordinary `lead_fallback` candidate, and its history note lists the items. A seat leaving with many items wakes each lead once. Items handed to a lead still reach it as their assignee, as before.
