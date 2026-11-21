# ADR-0167: A busy seat's wake from its run's requester, about the run's work, joins the running run

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestOnlyTheRunsRequesterJoinsItAndOnlyAboutItsWork`, `internal/engine.TestARequestersFollowUpJoinsTheRunOnceAndItsDeliveryIsSkippedNamingTheTurn`, `internal/engine.TestAWakeTheRunReadBindsItsTurnAndReadsItsConversationThrough`, `internal/engine.TestAnUnreadOrRefusedWakeRunsAsATurnOfItsOwn`, `internal/engine.TestAWakeIsClaimedByTheKeysItsDeliveryIsDroppedBy`, `internal/engine.TestARouterLivesWhileItsSeatIsBusyAndEndsWithTheEngine`, `internal/engine.TestTheRoutersOutcomesAndRulesAreClosed`, `internal/sandbox.TestTheSeatsRouterIsToldWhenItsRunsHoldItAndWhenTheSeatGoes`, `internal/queue/memory.TestConformance`, `internal/queue/jetstream.TestConformance`, `internal/coord/kv.TestFleetContract`, `internal/usage.TestASeatsCodeDayIsStampedByWhatItCounts`, `internal/store.TestTheTurnLedgerIsReadOffTheRecordsThatEndTurns`
- **Cost-when-tried:** before this record every wake reaching a seat whose executor run held it waited for the run to end, the requester's own follow-up ("use staging, not production") included, so a run of an hour worked the whole hour on what the follow-up corrected and a second turn then started over the result.
- **Tag-status:** unreleased

## The decision

**While one of a seat's executor runs holds the seat, the node holding it reads the seat's held inbox without consuming it and hands the run each wake its own requester wrote about its work.** A wake joins iff all of: its type is one the completion ledger keys (`inbox.Ledgered`) and not a delegated review or a colleague's ask or answer, each answered on a channel of its own that a run's turn answers only for its trigger; it is not a schedule's fire, an instance of its own; the ledger records none of its keys worked; its requester, credential and whom-for, composed with the functions dispatch composes a turn's requester with over that one wake, equal the run's, which names one; and its conversation is the run's, or its work item is the run's own (a non-empty one). The rule that matched (`conversation` or `work_item`) is named on the counter and the log line. No model decides anything here.

**Filed as a wake item** of the run's current job (ADR-0067's wake amendment), keyed by exactly the keys its delivery is dropped by, its words read as the turn's own ask reads them. The store dedupes by wake id, so a read made again files nothing twice; a refusal of the record (closed, spent, too large) leaves the wake queued and is never retried within that job. **The claim is the read**: once a step of the run is shown reading the wake, its keys and a join mark are recorded worked by the run's turn, the person's queued line is taken down, and when the inbox is let go its delivery is skipped `joined`, naming that turn. **A read wake binds the turn as its own trigger would**: the obligation its delivery would have put on a turn of its own (`ReplyFor`, sealed with the wake) is merged into the resumed turn's as a partition's are, so the delivery check and the review hold the turn to answering it. **And its conversation is read through it**: at the claim, the moment the run read it, the seat's seen-through mark for the wake's thread moves to the wake's record (ADR-0120), since its delivery is never a turn's to record; a later wake there at or below it that asks nothing is absorbed. A wake filed and never read is not claimed and runs as a turn of its own, as every wake did before; a relaunch's next read files it to the new job.

**One router per busy seat**, on the node holding the seat, from the moment a run holds it until none does, the seat is let go or the engine stops. It reads at once (mail already waiting), on a nudge from a best-effort stream subscription on the inbox and its lane and when the seat's dispatch goes idle, and at least every two minutes; never while a dispatch of the seat runs, since the turn that launched the run records its own triggers worked as it returns.

## Why the obvious alternatives are wrong

**Another writer's related message joining on this rule.** A run is held to one requester's apps (ADR-0109), and a partition to one writer for that reason; handing it another writer's words as its requester's would read them under the first's ceiling. Which of those may join, and as what, is ADR-0168's.

**Watching the seat's waiting channel, or deciding at the producer.** The waiting channel carries only a person's priority-lane wakes, without writer or words; the producers are six, on other nodes, and none sees mail already waiting when the run starts. The inbox is the one place every wake is, so the queue's backlog read became contract (`EventQueue.Backlog`), lane first, at most a limit, consuming and spending nothing, possibly with mail already acked, which the ledger check covers.

**Claiming at the filing.** A wake filed and never read would be dropped unworked. The claim at the read keeps the old behaviour as the fallback: whatever the run did not read runs on its own.

## Left to later pieces of the series

Objects the run created or wrote as a third match is crewlet/crewlet#1758's later piece; it needs this router and does not change its rule for the requester's own wakes.

## Amendment: the other wakes about the run's work are triaged

Another writer's wake about the run's work, or a lead's, is no longer left alone: [ADR-0168](0168-a-busy-seats-other-wakes-are-triaged-and-the-engine-gates-the-choice.md) triages what this rule leaves and gates the choice. This rule still decides first and its joins are unchanged.
