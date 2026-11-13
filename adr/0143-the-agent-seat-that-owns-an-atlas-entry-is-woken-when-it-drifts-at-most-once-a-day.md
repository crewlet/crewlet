# ADR-0143: The agent seat that owns an atlas entry is woken when it drifts, at most once a day

- **Status:** accepted
- **Authority:** `internal/atlaswake`
- **Enforced-by:** `internal/atlaswake.TestOnlyAnAgentSeatThatOwnsAnEntryIsWoken`, `internal/atlaswake.TestDriftIsTheThreeChanges`, `internal/atlaswake.TestAWakeIsClaimedOnceADayAndAChangeIsToldOnce`, `internal/atlaswake.TestASecondNodeWakesNobodyTheSameDay`, `internal/atlaswake.TestTheNodeThatLosesTheClaimPublishesNothing`, `internal/atlaswake.TestASeatThatTakesNoWorkIsNotWokenAndKeepsItsDay`, `internal/atlaswake.TestAFailedPublishIsMadeAgainAsTheSameWake`, `internal/atlaswake.TestTheCompanySwitchWakesNobodyAndKeepsTheDay`, `internal/atlaswake.TestAWakeSaysNothingOutsideTheClosedWords`, `internal/atlaswake.TestTheParserWakesNoPerson`, `internal/atlaswake.TestDriftIsReadInTheOwningSeatsOwnView`, `internal/engine.TestASeatThatCannotStartATurnTakesNoAtlasWake`, `internal/engine.TestASeatNobodyCanAskAboutIsUnknown`, `internal/config.TestOwnersAreWokenUnlessTurnedOff`, `internal/coord/kv.TestFleetContract`
- **Cost-when-tried:** not tried. Until this record a drift on an entry reached a person only: the one who set a contested value, under Needs you, and every holder of the atlas in Review. An entry whose owner is an agent seat waited for a person to notice it and hand it on.
- **Tag-status:** unreleased

## The decision

**When an atlas entry drifts, the agent seat that owns it is woken about it, at most once an entry a UTC day, and only about a change it has not been told.**

1. **Drift is three changes, a closed set.** The entry's health turns to `alerting`; a value a person set or confirmed is differed from by a newer report (`contested`); two sources the engine trusts alike disagree with nobody to settle it (`disputed`). Each is read off the atlas as every read computes it, so nothing new is stored to detect one. A warning (`needs_look`) wakes nobody.
2. **Only an agent seat that owns the entry is woken.** The owner is the one the entry's row shows: a seat a person or the engine's records name, never one only suggested. A person's seat, a unit and a handle the chart no longer holds are not woken: a person is told through Review, the Inbox and the Setup Advisor. The drift is read in the atlas as the owning seat reads it, so a wake never points at an entry or a value its seat is not shown.
3. **The bound is a record in the coordination store.** One record per seat and entry holds the UTC day of the seat's last wake about the entry and a digest of each change it was told. A wake is claimed only where a change is open that the seat was not told and the record's day is not today. The record is written only at the revision it was read at, before the publish, so of two nodes that hold the duty across a handover one claims the day and the other publishes nothing, and a restart reads the same record.
4. **A change is told once.** What the seat was told and is still open wakes nobody on later days. A change that ended is forgotten, so its return is a new change. A read that missed a source forgets nothing, since what it did not show may still be open.
5. **A seat that takes no work is not woken, and keeps its day.** Before the claim the duty asks whether the seat would start a turn: not paused, held by a node, not parked on a spent budget, not parked by its holder. A seat that would not is skipped with nothing written, so the pass after it resumes tells it what is still open. An answer that cannot be read skips the seat for that pass.
6. **The wake is derived, and a pointer.** The duty publishes one delivery of source `atlas_drift` on the inbound topic, the alarms' route (ADR-0095). Its wake id is the seat, the entry and the day, and its instant the claim's, the same on every copy, so a publish that failed is made again by the next pass as the same wake (ADR-0012). The delivery carries the entry's id, its kind and the change classes, and is refused if it says anything else. No name, no value and no text a vendor, an agent or a person wrote rides it; the seat reads the entry with its own atlas tools, at the trust its writers earn. The wake names no sender, so the turn is the company's own (ADR-0109), and one entry is one conversation.
7. **The company can turn it off.** `atlas.owner_wakes: false` wakes nobody. Each record is kept through its day and dropped after it, so turning the switch off and on again inside one day buys no second wake.

The duty is a fleet singleton, `atlas-wake`, on any worker node, every five minutes: the floor of the atlas's own refresh interval, below which no integration's read moves a fact.

## Why the obvious alternative is wrong

**A wake feed on the atlas log** (ADR-0012's mechanism) sees only what is recorded there. An entry's health is joined on every read from records the log never holds (a node's beat, a tool server's sign-in, an alarm), and a contested value can come from the settings, which are never recorded. A feed would wake for some drifts and silently not for others.

**The shared day counter** (`Allow` with a limit of one) bounds the day but cannot be given back: a publish that failed after it would spend the seat's one wake of the day on nothing. It also cannot say what the seat was told, so an entry that stays in disagreement would wake its owner every day for a fact it already has.

**Publishing and letting the seat's inbox hold it**, as an access review does, spends the day's wake on mail a paused or spent seat reads days later, when the entry may long have settled, and tells it nothing about what drifted since.

**Putting the entry's name or the changed value in the wake** hands a seat a vendor's or an agent's words as its own task. A monitor is named by whoever configured it at the vendor, and a contested value is by definition one somebody else reported.

**Waking a unit's lead for an entry the unit owns** names a seat nobody named. Ownership by a unit is the unit's to route.

## What it does not do

A Gitlet repository is read only as somebody (ADR-0127), and the duty reads as nobody, so a repository only Gitlet reports is not read for drift here. An entry with more than 64 open changes is told the rest another day.
