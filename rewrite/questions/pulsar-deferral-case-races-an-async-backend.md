# q — two `queuetest` cases assume the twin's dispatch model

Raised by: the Pulsar backend, which is the first backend to declare
`Capabilities.FreeDeferral` *and* dispatch asynchronously. Both cases below
cost real debugging time and both were, in the end, closed from the backend
side — but the assumptions are still in the suite and the next asynchronous
backend will meet them. I do not own `queuetest/`, so this is a report.

## 1. `NegativePaths/a_deferral_spends_no_dead_letter_budget`

```go
publish(t, q, "topic.budget", newEvent("e0"))
awaitState(t, "the deferral to quiesce the attachment", func() bool {
    return len(backlog(s, q, "topic.budget", "grp")) == 1
})
if _, err := q.Unquiesce(ctx, "topic.budget", "grp"); err != nil { … }
naks.await(t, "the full retry budget to survive the deferral", …)
```

**The wait does not wait for what it names.** The condition is `backlog == 1`.
On a backend whose backlog counts everything unacked, that is true **from the
moment the publish is acked** — before the broker has dispatched anything, so
before the handler has run, let alone deferred. `awaitState` returns at once,
`Unquiesce` finds nothing quiesced and no-ops, the deferral lands a
millisecond later, and the attachment is quiesced with nothing left to resume
it. The handler is never invoked again and the case times out with
`NOTHING was delivered`.

Measured on a real Pulsar 4.0.6 while `Backlog` counted unacked messages:
**16 of 20 isolated runs failed**, and **7 of 10 full-suite runs**.

It cannot fail on the backend it was written against. The in-memory twin
declares `InlineDispatch` — `Publish` drains before returning — so by the time
`publish()` returns the handler has already deferred and the window does not
exist. That is exactly the trap this suite's own doc warns about ("a
conformance suite must not require the happy path of the backend it was
written against"), and `runNegativePaths`'s header says these four cases were
checked by asking what a plausible wrong backend gets away with, rather than
by running an asynchronous one. JetStream never sees it because it skips the
case (`FreeDeferral` false).

**How this backend closed it, and why that was the right fix anyway.**
`Capabilities.Backlog` is documented as "the events a subscription retains and
has not delivered — the mail an unowned seat is holding". Read literally, two
kinds of message are not backlog:

- one a consumer already **holds** (Pulsar's `unackedMessages`) — work in
  progress;
- one the broker is **about to send**, because a connected consumer has an
  outstanding flow permit covering it — in flight, and nobody else can have it.

`internal/queue/pulsar` now subtracts both. That is a correctness fix on its
own — counting them makes "the mailbox is filling up" and "the seat is busy"
the same reading — and it happens to remove the window entirely, because a
just-published message is always covered by a permit while a live consumer has
credit. A genuinely backlogged seat still reports one: a consumer working
through a full prefetch has no spare permits, so everything behind it counts.
**0 of 25 isolated runs and 0 of 20 full-suite runs after the change.**

**What is still worth doing in the suite.** The case's wait is still not the
property it names, and the next backend to declare `FreeDeferral` will only be
safe by accident. The suite already defines the exact observable:

```go
// Capabilities.Quiescing — "reports whether this client has stopped taking
// work on a subscription."
awaitState(t, "the deferral to quiesce the attachment", func() bool {
    return s.caps.Quiescing != nil && s.caps.Quiescing(q, "topic.budget", "grp")
})
```

with a `needQuiescing(t)` skip in the shape of `needBacklog`. This backend
supplies `Quiescing`; JetStream does not declare it yet.

It is also worth deciding whether `Backlog`'s doc — "retains and **has not
delivered**" — is a requirement or a description. JetStream computes
`NumPending + NumAckPending`, which counts delivered-unacked and would hit
this window the moment it declared `FreeDeferral`.

## 2. `EventQueue/members_of_a_group_compete`, non-strict branch

The non-strict branch requires two members to each receive at least one of
four events. Pulsar dispatches to a Shared subscription by available PERMITS —
it hands one consumer as many entries as it has room for before moving on — so
at the production prefetch (64) one member legitimately takes all four. Each
event still reaches exactly one member, which is all "competing consumers"
owes; the *sharing* of a four-message burst is a stronger property that only
holds when the prefetch is smaller than the burst.

Measured at 1 in 10 full-suite runs before the conformance config's
`ReceiverQueueSize` was sized to 2, which makes it deterministic. That value
is documented as test-scale at its definition, and the production number is
chosen for an unrelated reason (bounding how much of a seat's mail one node
can hold hostage).

Worth a capability, or a weaker assertion: "each event reaches exactly one
member, and over a long enough run both members are used" is the property
every broker owes. Round-robin per message is the twin's dispatch strategy.
