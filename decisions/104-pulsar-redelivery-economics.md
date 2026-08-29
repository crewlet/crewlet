# d-104 — Redelivery economics on Pulsar, measured

Status: **decided** · Method: measurement, not inference
(broker: Apache Pulsar 4.0.6 standalone, `apache/pulsar-client-go` v0.21.0,
Go 1.27; harness: `internal/queue/pulsar/conformance_test.go` plus the direct
probe transcribed below)

Companion to `102-jetstream-redelivery.md`, which measured the same questions
on JetStream and named Pulsar as the contrast. This is the Pulsar column,
re-measured on the Go client rather than inherited from the Python engine's
numbers — because the client is not the same client, and two of its
differences are load-bearing.

## What was measured

| Fact | Measurement |
|---|---|
| Create a subscription with **no consumer attached** | admin REST `PUT …/subscription/{name}` with body `"earliest"`, 204 |
| Mail retained on a subscription nothing is attached to | retained (`msgBacklog` 1, `unackedMessages` 0) |
| Attach + first receive | sub-millisecond |
| Close a consumer holding an unacked message | **1.8 ms**, message returns |
| Re-attach and receive it again | **8.6 ms** after the close, **`redeliveryCount` 0** |
| `Nack()` → redelivery | next tick of the client's nack tracker, `delay/3` |
| Client-side ack timeout | **does not exist** — no `ConsumerOptions.AckTimeout` |
| `RedeliverUnacknowledgedMessages` on the Consumer interface | **not exported** |
| Subscription stats distinguish held from waiting | yes — `msgBacklog` vs `unackedMessages` |

The Python engine's own harness (`tests/test_queue/test_broker_behavior.py`,
Pulsar 4.2.4, C++ client) measured the same free handoff — "redelivery after a
graceful close: 9 ms, nothing lost; `redeliveryCount` after a close-driven
handoff: **0** — free" — and the prefetch hostage at exactly
`receiver_queue_size`. Both carry.

## The two client differences that changed the design

**1. There is no ack timeout.** The C++ client tracks unacked messages and
redelivers them after `unacked_messages_timeout_ms`; the Go client has no such
option, and Pulsar has no broker-side equivalent for a *connected* consumer. A
fetched message is that consumer's until it acks, naks, or closes.

Consequences, both acted on:

- **The batch dispatch budget is deleted.** `_BATCH_DISPATCH_BUDGET_MS = 60 s`
  and its requeue-by-republish path existed only because every drained
  message's 30-minute ack clock started at receive, so a long tail of
  multi-minute turns would breach it mid-drain. There is no clock, so the
  budget has nothing left to protect and is *deleted* outright. This also
  removes the republish that d-101 §1 forbids anyway.
- **A wedged-but-alive node holds its prefetch until its connection dies**,
  not for 30 minutes. Keepalives keep answering while the consume goroutine is
  stuck, so nothing releases it. That is an argument for the watchdog, not
  against the prefetch cap, and it makes a *small* prefetch more important
  here than it was on the C++ client.

**2. There is no redeliver-unacknowledged command.** The Python `unquiesce`
called `redeliver_unacknowledged_messages()` to reclaim what a quiesced
consumer was sitting on. The Go client does not export it, so the only
mechanism is to **close the consumer** — which on Pulsar is free.

## Decisions

**1. A blocked attachment holds nothing.** The consume loop closes its
consumer whenever it is blocked — quiesced, detached, draining, or held — and
opens a fresh one when it is not. One mechanism for all four reasons, because
on Pulsar all four are the same operation: closing returns everything unacked
*and everything prefetched*, in order, at `redeliveryCount` 0.

This is what makes `Unquiesce` reclaim the prefetch as d-101 §3 requires.
It also fixes a real starvation, measured against the conformance suite: a
node that merely stopped reading a paused subscription kept its prefetched
share of a fleet-wide group, and its peers could reach only three of four
messages — permanently, since nothing would ever return them.

The close happens **in the loop**, not in `Quiesce`. The loop is one
goroutine, so it reaches the close only after the handler it was running has
applied its outcome to a live consumer — which is how a quiesce can both stop
new work and let a running handler finish.

**2. `Defer` leaves the message unacked. It is never a NAK.** This is the
opposite of d-102 decision 1, and for the measured reason that decision gives:
on JetStream nothing is released by closing, so a deferral has to spend a
delivery count; on Pulsar the close returns it at `redeliveryCount` 0. The
backend declares `queuetest.Capabilities.FreeDeferral`.

**3. `MaxDeliveries` stays at 10.** JetStream re-derived it upward to 25
because handoffs there spend the budget. Here they do not, so the Python
value's original reasoning is intact: the budget covers poison ∧ node-death,
and ten is sized for a fleet where an ack-timeout redelivery (a node that
*died* holding the message) also increments it.

Counting convention, stated because this repo holds both: Pulsar's
`DLQPolicy.MaxDeliveries` counts TOTAL deliveries — the client's own router
says "the user specifies that wants to process a message up to 10 times". NATS
`MaxDeliver` counts the same way; the in-memory twin counts redeliveries after
the first. `queuetest` asks backends for total ATTEMPTS precisely so the suite
never has to guess which.

**4. Dead-lettering uses the client's own DLQ router, not a handler wrapper.**
d-102 decision 4 puts the decision in the wrapper because JetStream ships no
router. Pulsar's client does, it reads the BROKER's `redeliveryCount` — which
is authoritative across every node that ever held the message, where a local
counter is not — and it routes before the message is handed over again. The
dead-letter topic is `topics.DeadLetter(topic, group)`, deliberately outside
`crewlet.*` so the dashboard's `crewlet.events.>` feed cannot resurface
poison; Pulsar's own default name (`<topic>-<sub>-DLQ`) would sit inside it.

The router needs one thing the Python engine did not give it:
`InitialSubscriptionName`. Without a subscription the dead-letter topic
retains nothing, and Pulsar deletes a message published where no subscription
covers it — destroying the poison message the budget just spent ten deliveries
establishing.

**5. Within-conversation order still comes from event timestamps.** Pulsar's
broker replays from the head, so this backend *could* have leaned on the
broker. It does not, and the flag is deliberately NOT declared: the client
PREFETCHES, so by the time a handler naks a message its never-delivered
siblings are already in the local receiver queue and are served first. The
property `HeadReplayOnNak` asserts is what the handler observes, and that is
not it. d-102 decision 3 already removed the dependency.

## What Pulsar keeps that JetStream deleted

The whole admin-REST subscription lifecycle. Creating a subscription by
subscribing joins a Shared subscription a peer may be serving and takes a
share of that seat's traffic into this process — 12 of 20 messages, measured
in the Python harness — and `Consumer.Unsubscribe()` needs a local consumer,
which would make role decommission depend on which node ran the seat. Both
operations therefore go through `admin/v2`, and a reachable admin endpoint is
a deployment requirement.
