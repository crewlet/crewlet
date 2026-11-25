# ADR-0167: A busy seat's wake from its run's requester, about the run's work, joins the running run

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:**
  - `internal/engine.TestOnlyTheRunsRequesterJoinsItAndOnlyAboutItsWork`
  - `internal/engine.TestARequestersFollowUpJoinsTheRunOnceAndItsDeliveryIsSkippedNamingTheTurn`
  - `internal/engine.TestAWakeTheRunReadBindsItsTurnAndReadsItsConversationThrough`
  - `internal/engine.TestAnUnreadOrRefusedWakeRunsAsATurnOfItsOwn`
  - `internal/engine.TestAWakeIsClaimedByTheKeysItsDeliveryIsDroppedBy`
  - `internal/engine.TestARouterLivesWhileItsSeatIsBusyAndEndsWithTheEngine`
  - `internal/engine.TestTheRoutersOutcomesAndRulesAreClosed`
  - `internal/sandbox.TestTheSeatsRouterIsToldWhenItsRunsHoldItAndWhenTheSeatGoes`
  - `internal/queue/memory.TestConformance`
  - `internal/queue/jetstream.TestConformance`
  - `internal/coord/kv.TestFleetContract`
  - `internal/usage.TestASeatsCodeDayIsStampedByWhatItCounts`
  - `internal/store.TestTheTurnLedgerIsReadOffTheRecordsThatEndTurns`
  - `internal/engine.TestTheRequestersWakeAboutWhatTheRunWroteJoinsAndAnyoneElsesIsTriaged`
  - `internal/engine.TestAWakeIsAboutWhatARunWroteByExactIdentity`
  - `internal/engine.TestAWakeAboutARecordTheRunWroteReachesTheRunningRun`
  - `internal/sandbox.TestPendingStoreContract`
  - `internal/events/types.TestAWrittenSetHoldsEachRecordOnceAndDropsNothingAtItsBound`
  - `internal/agent/builtin.TestAWriteRecordsWhatItWroteOnceItLanded`
  - `internal/api/mcpbridge.TestABridgedCallRecordsWhatItCommitted`
  - `internal/chatlet.TestAReplyIsStampedWithTheConversationThePostRecords`
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

## Amendment: the other wakes about the run's work are triaged

Another writer's wake about the run's work, or a lead's, is no longer left alone: [ADR-0168](0168-a-busy-seats-other-wakes-are-triaged-and-the-engine-gates-the-choice.md) triages what this rule leaves and gates the choice. This rule still decides first and its joins are unchanged.

## Amendment: a wake about a record the run wrote joins too

**A third fixed rule, checked after the conversation and the work item: the requester's wake about a record the run created or wrote joins** (`wrote` on the counter), under every other condition above. The run keeps the set of those records on its bridged call log: each call's record step folds in the records its committed writes named (a work item it filed, updated or commented on; a change it opened, edited, reviewed, commented on, rebased or merged; a chat thread it posted in; a page it saved or remarked on; an artifact it remarked on), read with a write's own answer by the tool that made it, and only for a write that landed. Every opening of the log keeps the set, so it lives as long as the call log does: through a park, a relaunch, a new round and a takeover. Reads, refused and failed writes name nothing.

**Each record is named exactly as a wake about it is**: a work item by `<backend>:<id>`, the identity the wake is charged by, and every other record by the conversation identity its wakes are stamped with, from the producing domain's own function (a change's, a chat thread's at the thread grain, never the writer-split partition key; a direct conversation's line; a page's; an artifact's). Membership is an equality, never a likeness: a model's or a matcher's similarity would let a wake's own words claim a run's work, and an inexact match joins another person's thread to the run. The set holds 128 records, the bulk threshold `turnctx.MaxWritten` defends, each id at most 256 bytes, about 200 KiB of the log's rest at their worst; past it a record is refused, nothing held is dropped, and the run says so once.

**What no bridged call writes is not in it**: a push the box makes over git and a change its collection opens (the collection names a change by reference, not by the identity its wakes carry), and a held call a person approved, which the resume makes.

Another writer's wake about a written record is ADR-0168's candidate.

## Amendment: a seat runs up to its cap of runs

A seat holds up to its cap of runs, one per conversation (ADR-0169); its inbox is held once its runs hold every slot, its router reads it while any run holds, and a wake a run's steps read makes its conversation that run's.

## Amendment: the router's decisions are shared with the screen of a flowing inbox

Below the seat's cap its inbox flows (ADR-0169), so a wake reaches the dispatcher while a run holds the seat: the screen routes it to the run it relates to on this rule, as the router would, under the router's own memo and lock, so one wake read by both is filed and triaged once, and then sets it aside until the run leaves. The router still runs while any run holds the seat, reading mail the dispatcher has not taken; the backlog read skips what its client holds aside, decided already.

