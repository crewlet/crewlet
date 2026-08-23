# q — `a_deferral_spends_no_dead_letter_budget` races any asynchronous backend

Raised by: the Pulsar backend, which is the first backend to declare
`Capabilities.FreeDeferral` *and* dispatch asynchronously. Measured flake rate:
**3 of 12 full-suite runs** against a real Pulsar 4.0.6, and **16 of 20** when
the case is run alone (where the polling loop is fastest).

I do not own `queuetest/`, so this is a report plus a one-line fix, not a
patch.

## The case

`queuetest/negative.go`:

```go
publish(t, q, "topic.budget", newEvent("e0"))
awaitState(t, "the deferral to quiesce the attachment", func() bool {
    return len(backlog(s, q, "topic.budget", "grp")) == 1
})
if _, err := q.Unquiesce(ctx, "topic.budget", "grp"); err != nil { … }
naks.await(t, "the full retry budget to survive the deferral", …)
```

## Why the wait does not wait for what it names

The condition is `backlog == 1`. That is true **from the moment the publish is
acked** — before the broker has dispatched anything, and therefore before the
handler has run, let alone deferred. So `awaitState` can return at once,
`Unquiesce` finds nothing quiesced and is a no-op, the deferral lands a
millisecond later, and the attachment is quiesced with nothing left to resume
it. The handler is never invoked again and the case times out with
`NOTHING was delivered`.

The comment says what was meant — "the deferral to quiesce the attachment" —
and the backlog is simply not that signal.

**It cannot fail on the backend it was written against.** The in-memory twin
declares `InlineDispatch`: `Publish` drains before returning, so by the time
`publish()` returns the handler has already deferred and the window does not
exist. That is precisely the trap this suite's own docs warn about — "a
conformance suite must not require the happy path of the backend it was
written against" — and `runNegativePaths`'s header says the four cases there
were checked by asking what a plausible wrong backend gets away with, rather
than by running an asynchronous one.

JetStream never sees it because it skips the case (`FreeDeferral` false).

## The fix, one line

The suite already defines the exact observable and this backend already
supplies it:

```go
// Capabilities.Quiescing — "reports whether this client has stopped taking
// work on a subscription."
```

```go
awaitState(t, "the deferral to quiesce the attachment", func() bool {
    return s.caps.Quiescing != nil && s.caps.Quiescing(q, "topic.budget", "grp")
})
```

with a `needQuiescing(t)` skip when the capability is absent, in the same shape
as `needBacklog`. That waits for the thing the case is about, and it is
deterministic on an inline backend and an asynchronous one alike.

(If `Quiescing` should not become a requirement of this case, waiting for
`Backlog` to report the event *after* it has been handed back works too — but
only for a backend whose deferral makes the message observable again, which is
a backend-specific promise the suite should not encode.)

## What I did meanwhile

Nothing that hides it. This backend declares `FreeDeferral: true` because the
property is measured and true — a graceful close returns the unacked message
in 1.8 ms at `redeliveryCount` 0, and a re-attach receives it again 8.6 ms
later still at 0 (`rewrite/decisions/104-pulsar-redelivery-economics.md`).
Declaring it false to make the case skip would be a false statement about the
backend.

I did narrow the window while implementing `Backlog` to its documented words —
"retains and **has not delivered**" — by subtracting the subscription's
`unackedMessages` from `msgBacklog`. That is a correctness fix in its own
right (it stops "the mailbox is filling up" and "the seat is busy" reading
identically), and it cut the window from "the whole handler run" to
"publish-acked until broker-dispatched". It cannot close it: no broker
statistic distinguishes *not yet dispatched* from *nothing to dispatch*.

**Until the suite fix lands, the `pulsar conformance` CI job will fail roughly
one run in four on this case alone.**

## A second, smaller one in the same family

`EventQueue/members_of_a_group_compete` (non-strict branch) requires two
members to each receive at least one of four events. Pulsar dispatches to a
Shared subscription by available PERMITS — it hands one consumer as many
entries as it has room for before moving on — so at the production prefetch
(64) one member legitimately takes all four. Each event still reaches exactly
one member, which is all "competing consumers" owes; the *sharing* of a
four-message burst is a stronger property that only holds when the prefetch is
smaller than the burst.

Measured at 1 in 10 full-suite runs before I sized the conformance config's
`ReceiverQueueSize` to 2, which makes it deterministic. That is a test-scale
value and it is documented as such at the definition, but the suite is
asserting round-robin-per-message, which is the twin's dispatch strategy
rather than a broker requirement. Worth a `StrictRoundRobin`-style capability
or a weaker assertion (each event to exactly one member, over a longer run).
