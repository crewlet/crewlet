# adr-102 — Redelivery economics on JetStream, measured

Status: **Accepted** · Method: measurement, not inference

## What was measured

Embedded `nats-server` v2.14.5, in-process connection, file and memory stores,
Go 1.27:

| Fact | Measurement |
|---|---|
| Create a durable consumer with **no consumer attached** | **1.7 ms**, plain JetStream API |
| Mail retained on a subscription nothing is attached to | 6 of 6 |
| Attach + first fetch | sub-millisecond |
| `Nak()` → redelivery | **~0 ms, `NumDelivered` 1 → 2** |
| Defer by not acking, client stays up | waits the **full `AckWait`**, then `NumDelivered`+1 |
| Defer by **closing the client** | **does NOT release early** — nothing after 5 s with `AckWait` 30 s |
| Delete + recreate the durable | returns at `NumDelivered` 1 in 1 ms — **but resets the cursor**, so unusable |
| Ordering of redeliveries | **`[m2 m3 m0 m1]`** — redelivered messages come back *after* never-delivered ones |

## Two Pulsar properties do not carry over

The Python design rests on two measured Pulsar behaviours that JetStream does
not share:

1. **Free handoff.** On Pulsar a graceful consumer close returns unacked
   messages in ~9 ms at `redeliveryCount` 0 — a seat handoff costs nothing
   against the dead-letter budget, while a node *death* (ack-timeout) costs
   one. That asymmetry is what sized `_MAX_REDELIVER = 10`. On JetStream there
   is no free return: closing the client releases nothing, and every path back
   (`Nak`, `AckWait` expiry) increments `NumDelivered`.

2. **Order-preserving redelivery.** Pulsar replays unacked messages from the
   head, ahead of newer arrivals. JetStream returns them at the **back**.
   The Python code refuses to republish a deferred message precisely because
   that "sends the event to the topic tail while its prefetched siblings replay
   from the head, reordering the conversation" — and JetStream's *native*
   redelivery does exactly that.

## Decisions

**1. Defer uses `Nak()`.** Instant (~0 ms) versus up to a full `AckWait`. The
alternative — stop consuming and let the ack timer expire — would park a
seat's mail for the whole window on every lease movement. The cost is one
delivery count, which decision 2 absorbs.

**2. `MaxDeliver` is re-derived to 25.** On Pulsar the budget covered poison
∧ node-death. On JetStream it must also cover **handoff**, because handoffs
now spend it. 25 leaves ample headroom: a message is normally handled within
seconds, seat migrations are rate-limited to 4 claims / 2 releases per 5 s
sweep, and a message would have to be in flight across 25 of them to exhaust
the budget — a fleet thrashing that badly has a louder problem. The Python
code's own honest caveat still stands and is not solved by any cap: a fast
crash-loop is indistinguishable from poison.

**3. Within-conversation order comes from event timestamps, not from the
broker.** This is the important adaptation, and it makes the engine *more*
robust rather than less: the batch layer sorts a partition's events by their
own `timestamp` before dispatch, so a conversation reads correctly regardless
of how the broker interleaves redeliveries with fresh arrivals.

Event timestamps are already trusted for exactly this class of decision —
`OrderPartitionsOldestFirst` uses them to age a deferred conversation into
priority, and requeue preserves them by design. Depending on them here removes
a hidden dependency on one broker's replay semantics that would otherwise have
to be re-verified for every backend. Pulsar's head-replay then becomes a
performance detail rather than a correctness requirement.

**4. The dead-letter decision stays in the engine's handler wrapper**, not in
`MaxDeliver` alone: at the threshold the wrapper publishes to
`dlq-{topic}-{group}` (deliberately outside the `crewlet.*` space, so the
dashboard's `crewlet.events.>` stream cannot resurface poison as live traffic)
and `Term()`s the original. `MaxDeliver` remains configured as a backstop so a
wrapper bug cannot produce an infinite loop.

## What this deletes

The entire Pulsar admin-REST subscription-lifecycle workaround
(`src/crewlet/queue/admin.py`, ~218 lines plus its deployment requirement of a
reachable admin URL) has no JetStream equivalent: creating a durable consumer
without attaching is a normal API call that takes 1.7 ms. The
"joining a Shared subscription steals a peer's traffic" hazard — measured at
12 of 20 messages on Pulsar — does not exist here.

Pull consumers also remove the **prefetch hostage**: nothing is pushed into a
client-side queue, so a wedged node holds no mail it has not fetched, and
`Unquiesce` does not need to reclaim a prefetch. The Pulsar backend keeps both
mechanisms, because on Pulsar both are real.
