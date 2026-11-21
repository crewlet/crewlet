# ADR-0169: A seat runs up to its cap of runs, one per conversation

- **Status:** accepted
- **Authority:** `internal/sandbox`
- **Enforced-by:** `internal/sandbox.TestASeatAdmitsExactlyItsCapOfRunsAtOnce`, `internal/sandbox.TestAConversationARunWorksStartsNoSecondRun`, `internal/sandbox.TestTheInboxIsHeldAtTheCapAndReleasedBelowIt`, `internal/node.TestASeatRunsUpToItsCapOneTurnPerConversation`, `internal/engine.TestALaunchFloorCountsTheSeatsOtherRuns`, `internal/engine.TestACheckoutOfABranchAnotherRunPushesIsRefused`, `internal/config.TestASeatsRunCapDefaultsToOneAndRefusesLessThanOne`
- **Cost-when-tried:** before this record a seat whose executor run held it took no other work for the run's hours, an unrelated person's ask included, and the rule lived in several places (node admission, the coordinator's hold, the owed resume's wait), each assuming one; nothing let an operator give a seat room for two.
- **Tag-status:** unreleased

## The decision

**A seat holds up to its cap of executor runs at once, at most one per conversation.** The cap is the role's `sandbox.max_concurrent_runs`, one where unset, so a company that names none runs as before. A conversation is the run's conversation identity, its own and each one a wake its steps read came from (ADR-0167's served conversations).

**One reservation is the gate.** Each road into the holding set that starts a run (a launch's row, an answer's claim) reserves a slot in the same step, under the coordinator's lock, as it records the write; it is refused at the cap or on a conversation another of the seat's runs works, and an answer waits for the slot. A takeover counts what the store holds and is never refused. Node admission holds the engine-host turns to the same cap, one per conversation.

**The inbox is held at the cap**, not at the first run: below it a delivery runs, unless one of the seat's runs works its conversation, when it waits at the head of the held inbox until that run leaves. **A launch's budget floor counts the seat's other runs**, the static floor each, since a run's spend is charged only at its collection. **A branch is pushed by one of the seat's runs at a time**: a checkout of a branch another of its runs pushes is refused.

## Why the obvious alternatives are wrong

**A count per node only.** Node admission counts engine-host turns, which end at the launch; a run holds its seat for hours after. The cap is on runs, so the coordinator, which counts them, reserves.

**Exclusivity by partition key.** The key splits a thread by writer (ADR-0109); two writers in one thread would get two turns writing one session.

**A cap of two by default.** A delivery whose conversation is busy holds the whole inbox here, so a larger default buys little until such a delivery stops blocking the rest; one keeps every company's behaviour.

This amends ADR-0065's statement, carried in `.claude/rules/architecture.md`, that a seat takes no other work while its run works.
