# ADR-0120: A wake a completed turn already read past starts no turn

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestAWakeACompletedTurnReadPastIsAbsorbed`, `internal/agent/ledger/ledgerstore.TestASeenThroughMarkMovesForwardOnly`, `internal/coord/kv.TestFleetContract`, `internal/engine.TestAHeldConversationStartsNoTurnUntilAPersonWritesOnIt`, `internal/store.TestTheTurnLedgerIsReadOffTheRecordsThatEndTurns`
- **Cost-when-tried:** not tried here. Before this record a wake was dropped only when a turn had been woken for that very event, or (in a chat thread) when a later turn's thread block showed the same writer's message waiting. A tracker notice that came round after a newer turn on the same item had run, a redelivery or a slow drain, started a turn of its own over a change the item's newer turn had already read, and the turn ledger counted it as started work.
- **Tag-status:** unreleased

## The decision

**When a turn completes, the newest record it was woken for marks how far its
seat has read that conversation, and a later wake there at or below the mark
that asks the seat for nothing starts no turn.**

1. **The position is the record's own.** Every first-party wake carries the
   log position of the record it was derived from (`trigger_position`, the
   same token the woken turn raises its read floor to). A vendor's wake
   carries none and is never absorbed.
2. **The mark is per seat, conversation and log, forward only.** Keyed on the
   conversation's stamped partition key, the thread and never the thread
   split by writer, so a colleague's later turn and a person's both move it;
   and on the log a position belongs to, since positions on two logs do not
   compare and one must never stand in for the other.
   It is written when a turn completes, by compare-and-set: a mark another
   node moved further is kept, a later position or generation of the log
   moves it. It lives in the
   completion ledger's bucket and ages with it, since a mark matters only to
   a redelivery the ledger itself would outlive.
3. **An obligation is never absorbed.** A wake addressed to the seat, owing a
   chat surface, or putting a question to the room runs whatever its position.
   The test is the source's own routing decision carried on the wake, not a
   rule of this record's.
4. **The ledger counts it.** An absorbed wake is recorded as a
   `TurnTriggerSkipped` with the disposition `absorbed` and its wake key, and a
   wake a held conversation settled unrun
   ([ADR-0117](0117-a-failure-a-retry-cannot-cure-pauses-its-seat.md)) with
   `breaker_held`; the turn ledger counts both beside `started` and `held`.

## Why the obvious alternatives are wrong

**A wall clock** ("drop wakes older than the last turn") compares a writer's
clock with a reader's, and a minute of skew absorbs a change nobody read.

**Absorbing on the completion ledger's keys alone** is what was there: it
recognises the same event, not an older change in the same conversation.

**Moving the mark on every read a turn makes** would absorb what the turn
actually saw during its run, and needs every read path to report the position
it was served at. It is the natural next step and is not decided here.

## What this does not decide

How the dashboard shows absorbed wakes, which detectors read them, or whether
a chat surface's own message sequence becomes a position.
