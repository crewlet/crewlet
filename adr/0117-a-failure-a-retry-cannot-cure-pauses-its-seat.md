# ADR-0117: A failure a retry cannot cure pauses its seat or holds its conversation, once

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestTheSecondIdenticalFailurePausesTheSeat`, `internal/agent/builtin.TestAPersonsPauseSupersedesTheEnginesFailurePause`, `internal/engine.TestTheSecondContextThatDoesNotFitHoldsTheConversation`, `internal/engine.TestAHeldConversationStartsNoTurnUntilAPersonWritesOnIt`, `internal/engine.TestNewModelsLiftAConversationHold`, `internal/engine.TestARemovedSeatLeavesNoConversationHeld`, `internal/notice.TestAHeldConversationIsANotice`, `internal/engine.TestOnlyARunOfOneFailurePausesTheSeat`, `internal/engine.TestAChangedSettingLiftsTheFailurePause`, `internal/engine.TestEveryFailureKindIsClassified`, `internal/engine.TestAChainThatRefusedEveryCredentialIsAFailureThatWillRepeat`, `internal/queue/jetstream.TestARedeliveryIsSpreadAroundItsSpacing`, `internal/sandbox.TestAContainerThatWouldNotStartIsClassedByWhetherItsImageIsHere`, `internal/engine.TestTheSecondRunKilledForMemoryHoldsTheConversation`, `internal/engine.TestAChangedSandboxLiftsAMemoryHold`, `internal/sandbox/executor.TestAHarnessKilledForMemoryEndedAtItsCeiling`, `internal/sandbox.TestACollectedRecordNamesARunKilledForMemory`, `internal/notice.TestAHeldConversationIsANotice`
- **Cost-when-tried:** not tried here. Before this record such a failure was NAKed like any other, so a seat whose sandbox image could not start, or whose models all refused their credential, ran the same failing turn on every redelivery until the delivery's budget ran out, rebuilding the turn's whole context each time, and its agents then asked a person about the failure as if it were a business decision.
- **Tag-status:** unreleased

## The decision

**A turn failure the engine knows will happen again until a person changes
something pauses its seat on its second time in a row.** The failures are a
closed table: the sandbox runtime refusing the box as configured, the sandbox
provider refusing the engine's credential or its billing, and every model of
the seat's chain refusing its credential. Every other failure, including a
sandbox provider that is down or limited, is left to the redelivery, whose
spacing is now spread by a fifth so seats that failed together do not come
back on one second.

The pause is a seat pause like a person's: one record per seat in the
coordination store, taken by the engine (`by: engine`, kind `system`), with the
failure's kind and a fingerprint of the settings it depends on (the seat's
sandbox and the company's, or the seat's models and the company's). So it holds
the seat's inbox on whichever node holds the seat, survives a restart and a
move, says why on every screen that shows a paused seat, and drops nothing:
the delivery that tripped it waits at the head of the inbox.

It ends in one of two ways. A person resumes the seat, or an apply changes the
settings the failure depended on, which lifts the pause as the engine. A
person who pauses a seat the engine paused makes the pause theirs: it loses its
failure, takes their stop if they ask for one, and ends only when somebody
resumes it, since a person's pause is never lifted by an apply. A
failure that still holds after either pauses the seat again on its second time.

**A conversation whose context does not fit is held, not its seat.** A run
that ran out of context after the conversation's previous run did, the second
time on a fresh session (so the brief alone does not fit, not a long session),
holds that conversation: one record per (seat, conversation) in the
coordination store. Its wakes are settled without a turn and recorded as
skipped, and the seat's other conversations go on. A person writing on the
conversation lifts the hold and that delivery runs, and so does a change of
the seat's models, which a hold is judged against the next time it is read. A
seat the company removes takes its holds with it.

**A conversation whose work does not fit its box's memory is held the same
way.** A run whose harness the kernel killed for memory (`out_of_memory`: a kill
counted in the job's memory domain between the job script's reads of it before
and after the harness, and the harness ended by SIGKILL) after the
conversation's previous run's was, on whatever session, holds that
conversation. A fresh session does not shrink the work, so nothing about the
session is asked. The hold is judged against the seat's sandbox settings
rather than its models, so a person writing on the conversation, or a change of
the box (its size among them), lifts it. Every collected run says the kills its
job saw and its domain's ceiling, a run that survived a killed command included.

**A held conversation is told, and is never released by a timer.** Each hold is
a platform notice of the engine's cause (ADR-0116, class `conversation.held`,
with its `cause`: `context_exhausted` or `out_of_memory`),
which reaches every administrator while the hold stands and leaves with it, and
the item a held conversation is about says "Held by the engine" on its page.
There is nothing to decide: the notice says what releases it. A timed release
would run the same brief into the same models again, twice, which is the loop
the hold ends; a person who reads the notice and writes on the conversation is
the release that can change the answer.

## Why the obvious alternative is wrong

A park of its own beside the budget and provider parks would be this node's
alone, decided again on the first delivery after a restart or a move: the loop
it exists to stop would come back once per restart, and no screen would say
why, since a park is not a pause a person can lift. Counting failures in the
queue's redelivery would not tell a failure that will repeat from one that
will not, and a longer backoff only spends the same 25 deliveries more slowly.

## What this does not decide

The first failure of a run still costs one
retry: the count is each node's, so a restart or a move between the two
failures costs one more, for a seat pause and a conversation hold alike. A held
conversation's wakes are recorded as worked, not as held turns in the usage
ledger, and the turn a person's message releases carries that message; the
conversation's thread still holds every message the hold settled. A container that would not start is the configuration's only when the runtime
holds its image; one it does not hold failed to arrive, and is left to the
retry like any provider that did not answer.
