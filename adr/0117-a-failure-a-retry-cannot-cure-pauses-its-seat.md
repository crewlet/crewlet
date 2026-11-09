# ADR-0117: A failure a retry cannot cure pauses its seat, once

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestTheSecondIdenticalFailurePausesTheSeat`, `internal/agent/builtin.TestAPersonsPauseSupersedesTheEnginesFailurePause`, `internal/engine.TestOnlyARunOfOneFailurePausesTheSeat`, `internal/engine.TestAChangedSettingLiftsTheFailurePause`, `internal/engine.TestEveryFailureKindIsClassified`, `internal/engine.TestAChainThatRefusedEveryCredentialIsAFailureThatWillRepeat`, `internal/queue/jetstream.TestARedeliveryIsSpreadAroundItsSpacing`
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

## Why the obvious alternative is wrong

A park of its own beside the budget and provider parks would be this node's
alone, decided again on the first delivery after a restart or a move: the loop
it exists to stop would come back once per restart, and no screen would say
why, since a park is not a pause a person can lift. Counting failures in the
queue's redelivery would not tell a failure that will repeat from one that
will not, and a longer backoff only spends the same 25 deliveries more slowly.

## What this does not decide

A failure tied to one conversation rather than the seat (a context that does
not fit) is not in the table. The first failure of a run still costs one
retry: the count is each node's, so a restart between the two failures costs
one more. A pull the local runtime cannot tell apart from a refused image is
classed as the configuration's, so a pull that fails twice on a network blip
pauses the seat until a person resumes it.
