# ADR-0073: An anchor below the trim floor is removed by a record

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog/statelogtest.TrimmedAnchorsGoAndAZeroWriteIsFenced`, run by every domain's certification — `internal/worklet.TestTheTrackerIsACertifiedDomain`, `internal/doclet.TestThePagesDomainIsACertifiedDomain`, `internal/search.TestTheVectorDomainIsACertifiedDomain`, `internal/usage.TestTheUsageDomainIsACertifiedDomain`, `internal/assistantdomain.TestTheAssistantDomainIsACertifiedDomain`, `internal/chatlet.TestTheChatDomainIsACertifiedDomain` — and held able to fail by `internal/statelog/statelogtest.TestTheSuiteCatchesADomainWhoseTrimmedRecordTheFrameworkCannotUse`; `internal/statelog.TestATrimmedRecordRemovesOnlyTheAnchorsBelowItsFloor`, `internal/statelog.TestATrimmedRecordRemovesAtMostItsBoundLowestFirst`, `internal/statelog.TestASweepAppendsOneRecordPerBoundAndNoneWhenNothingIsBelow`, `internal/statelog.TestASweepAppendsEachRecordOnlyOnceTheOneBeforeHasCommittedHere`, `internal/statelog.TestASweepFromACopyBehindTheLogAppendsNothing`, `internal/statelog.TestASweepStopsAppendingOnceItsBudgetIsSpent`, `internal/statelog.TestASweepAppendsNoRecordPastItsBudget`; `internal/engine.TestEveryLogThatWritesAnchorsHasATrimmedRecord`, `internal/engine.TestATickRemovesTheAnchorsBelowWhatItPurged`, `internal/engine.TestATickThatPurgesNothingSweepsBelowTheLogsFirstRecord`, `internal/engine.TestATicksAnchorSweepsShareOneAlarmInterval`, `internal/engine.TestAnIdleLogLeavesItsShareToTheOthers`; and the one writer of the replicated estate, `internal/store.TestOnlyTheApplierWritesTheReplicatedEstate`
- **Measured:** on the pinned driver under the store's own pragmas (`internal/statelog.BenchmarkATrimmedRecordsSweep`, a 4-vCPU container's virtio disk, a table laid out as production lays it out — anchors removed in an order unrelated to the order they are stored in, another log's interleaved), one record's committed transaction removing 250 anchors takes 21 ms at the 99th percentile from a table of 20 000, 56 ms from 300 000 and 75 ms from 1 000 000 — under a third of the applier's 250 ms transaction budget, which is checked only between records. At 1 000 anchors a record it took 49, 137 and 159 ms, and the slowest of two hundred took 273 ms.
- **Cost-when-tried:** the replicated estate's first migration shipped `statelog_anchor_swept_idx` and a comment promising "a range delete over anchors below the trim floor", and nothing ever deleted a row: every subject a log ever carried kept an anchor on every data node for the life of the deployment — the native code host's model mints about seventy thousand subjects a day — and the next write to any quiet object first sent an expectation the broker could only refuse. The first design that came to hand, a node-local maintenance job range-deleting below the published floor, is a second writer of the replicated estate, which ADR-0002 forbids and `TestOnlyTheApplierWritesTheReplicatedEstate` refuses.
- **Tag-status:** unreleased

## The decision

After each tick's purge, the trim appends to every log that writes arbitration
anchors a FRAMEWORK record, `trimmed` (`statelog.TrimmedKind`), whose subject
names the log's first surviving sequence (`statelog.TrimmedSubject`), and every
node's applier removes that log's anchors below it in the transaction that
applies the record — at most `statelog.AnchorsPerTrimmedRecord` a record,
lowest first. The trim appends one record per that many anchors its own copy
holds below the floor, at most `statelog.TrimmedRecordsPerSweep` a tick, and
none when there are none (`statelog.PlanSweep` counts them and
`statelog.SweepPlan.Sweep` appends them, called by the engine's
`retention.sweepAnchors` with the domain's encoder from `trimmedEncoder`). It
counts only once its own copy has applied the log's end, and appends each
record only once its own applier has committed the one before, each wait
bounded by `statelog.DefaultResolveBudget`; and the appends are bounded in
time (the budget `statelog.SweepPlan.Sweep` is handed) — no record appended
once the budget is spent, no wait outlasting it, and running out the designed
stop rather than a failure. The engine bounds a tick's sweeps, counts and
appends together, by one alarm interval (`anchorSweepDeadline`), because the
sweeps run on the goroutine that evaluates the holder's alarms: every log is
counted first, and the interval is shared among the logs with anchors below
their floor that tick, each handed an equal share of what is left
(`runSweeps`) — so an idle log costs the others its count and nothing more.
So a holder whose copy is behind appends nothing; a write or a read queued
behind a sweep on the holder waits one record's apply; on a node that applies
the records k times slower than the holder it waits at most about k − 1
budgets, and never more than the cap's records; and a tick's sweeps together
delay the holder's alarms by at most one interval. The record is version 1 for ever, declares the framework's
scope sentinel, carries no operation id and names its writer, so an eviction
gate drops an evicted node's. A log that arbitrates nothing — the compacted
vectors and usage — writes no anchor and has no such record.

## Why the obvious alternative is wrong

A sweep beside the applier — a maintenance job deleting below the published
floor — is a second writer of replicated state: N nodes deleting on N
schedules from N readings of a floor, which is exactly what the applier's
exclusivity exists to rule out. One record with one unbounded delete holds
every node's only writer for as long as the backlog takes, at once; the bound
is what keeps a week of blocked trim from becoming tens of seconds of stalled
appliers fleet-wide. Bounded records appended back to back are the same stall
in pieces: every record on a log is applied in order on every node, so a seat's
write or a linearizable read's barrier appended behind a sweep's run waits for
the whole run — at the cap, seconds — which is why each record waits for the
one before it to apply. Pacing alone bounds that wait only on the node that
sets the pace, and bounds the sweep's own length not at all: on a holder whose
applier is slow but still moving, every wait succeeds just inside its bound and
the cap's records take minutes of the goroutine that evaluates the holder's
alarms — at the moment `apply_lag` matters most — while a data node applying
them slower than the holder still queues the difference in front of its own
writes. A time budget is the one bound that limits both, which is why the
sweep has one as well as a cap. A count taken from a copy that has not
applied the records already on the log counts anchors they remove, and a
holder that stayed behind would append the cap again every tick. And keeping
the anchors grows a table by every subject ever written and sends every quiet
object's next write a doomed append.

Removal is safe at any bound, which is why the bound is chosen for cost: an
anchor is never what prevents a lost update — the broker's arbitration is —
and a write to a subject with no anchor asks the log what the subject holds
and reaches the expectation-zero fence by that road, as one whose anchor named
a trimmed record reaches it after the broker refuses the anchor (the floor
theorem in `go doc ./internal/statelog`). The floor is the log's first
surviving sequence rather than the published floor because an anchor at or
above it names a record the log still holds, and removing it would cost that
object's next write a leader read for nothing.

## What this does not decide

The trim's six terms and the published floor are unchanged; the record's floor
is what the log no longer holds, never the published one. The broker's own
per-subject state is not this record's to manage — a trimmed record's subject
is removed by a later purge like any record's. And it says nothing about the
compacted logs, whose per-subject retention is the broker's.
