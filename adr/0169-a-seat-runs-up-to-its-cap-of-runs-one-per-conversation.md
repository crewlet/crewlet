# ADR-0169: A seat runs up to its cap of runs, one per conversation

- **Status:** accepted
- **Authority:** `internal/sandbox`
- **Enforced-by:**
  - `internal/sandbox.TestASeatAdmitsExactlyItsCapOfRunsAtOnce`
  - `internal/sandbox.TestAConversationARunWorksStartsNoSecondRun`
  - `internal/sandbox.TestTheInboxIsHeldAtTheCapAndReleasedBelowIt`
  - `internal/node.TestASeatRunsUpToItsCapOneTurnPerConversation`
  - `internal/engine.TestALaunchFloorCountsTheSeatsOtherRuns`
  - `internal/engine.TestACheckoutOfABranchAnotherRunPushesIsRefused`
  - `internal/config.TestASeatsRunCapDefaultsToTwoAndRefusesLessThanOne`
  - `internal/engine.TestAnUnsetSeatAdmitsTwoRunsOfDifferentConversations`
  - `internal/queue/memory.TestConformance`
  - `internal/queue/jetstream.TestConformance`
  - `internal/queue/jetstream.TestAnAsideDeliveryIsKeptPastTheAckWindow`
  - `internal/sandbox.TestARunLeavingReadmitsWhatWaitedOnItAndNeverWhatAnotherHolds`
  - `internal/sandbox.TestAnAnsweredFoldInReadmitsTheWakeItDecides`
  - `internal/engine.TestADeliveryBelowTheCapWaitsAsideOnTheRunItRelatesToOrStartsBeside`
  - `internal/engine.TestABusySeatsWakeIsRoutedSetAsideAndReadmittedAsTheRunLeaves`
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

## Amendment: mail waiting on a run waits aside, and the cap is two by default

**A delivery that waits on one of the seat's runs is set aside in flight, not held at the inbox's head.** Below the cap, one of a conversation a run works, or one the router relates to a run (its requester's about the run's work, or a triage candidate: about its item, a record it wrote, or a lead's), is routed to that run as at the cap (ADR-0167, ADR-0168) and then answered `queue.OutcomeAside` under a key: the conversation, or the run's own key. The delivery stays unacknowledged, its ack window restarted while it waits and its delivery count untouched, and the consumer goes on past it, so one busy conversation no longer holds every unrelated wake. A delivery related to no run starts a turn beside them with no model call; a request related to a run waits, since related work must not run beside the run doing it, and two unrelated turns judge any collision themselves.

**The run leaving readmits exactly what waited on it.** Each transition that takes a run out of `Holding` (settling, parking on a person, failing, being collected, a recovery finding it gone) frees its keys, and the coordinator readmits each no other run of the seat still holds, at the head of the inbox's next drain, partitioned there as any drain's mail so one writer's waiting wakes are one turn (ADR-0109). A joined wake the run read is skipped as worked; an unread one runs on its own. A readmit landing while the handler that sets a delivery aside still runs readmits that delivery, so the race between a run leaving and its mail being set aside loses nothing. A release or a stop hands aside mail back at one delivery, a crash after the ack window. An inbox holds at most 64 aside (`queue.MaxAside`: a slice of its consumer's 1000 unacknowledged slots, and what the router reads at once); past that it is held, as at the cap, until a readmit hands some back.

**A request another writer left waits aside on its fold-in** (ADR-0109's amendment "another writer's request joins only on approval"): put to the run's approvers as at the cap, it is set aside under the run it relates to, and the answer, recorded on the run's row and told to the seat's holder on its control topic, readmits it under that run's key and its conversation's, so the screen reads the answer at once: an approval files it into the running work and drops its delivery as folded, a refusal sets it aside again. Unanswered when its job ends, it waits on, readmitted when the run leaves. Its writer is told why once, on the one memory the fold-in tells by, and a reason the fold-in told stands over the aside's.

**The default cap is two**: with a busy conversation no longer blocking the rest, a second run keeps a seat answering an unrelated request, and a seat at rest holds no box, so the second slot costs nothing until it is used. This supersedes "A cap of two by default" below the decision and the head-of-inbox wait above it.

**Why not the obvious ways to keep the mail.** A deferral costs a delivery and returns at the head, so the busy message is the head again on the next fetch: the whole inbox blocked and the budget spent in minutes. A Nak or a pass returns it behind waiting mail spaced up to thirty seconds, about ten minutes to a dead letter, against a run of hours. A republish lands at the tail, resets the count and loops while the inbox flows. A side store (the coordination store, a list on the run's row) needs a second road into a turn with no delivery behind it, and a hand-back racing the run's leaving. The broker already keeps an unacknowledged message durably; setting it aside in flight keeps it there.

## Amendment: a seat's runs see and message each other

A run launched beside the seat's other runs is told of them and may message one of the same requester or wait for one to leave its slot ([ADR-0176](0176-a-seats-runs-see-and-message-each-other.md)). A run parked so keeps its keys and no slot: its mail waits aside behind it until it resumes or ends.
