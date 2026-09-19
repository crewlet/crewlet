# ADR-0001 — One broker carries both the stream and the coordination store

- **Status:** accepted
- **Authority:** `internal/queue`
- **Enforced-by:** `internal/queue/topics.TestNoPackageBuildsASubjectByHand`, and the `queuetest` conformance suite every backend runs
- **Measured:** creating a durable consumer with nothing attached is an ordinary API call at about 1.7 ms. The delivery budget before a message is dead-lettered is 25 rather than the ~10 a broker with a free handoff would need, because every path back to the broker — the Nak a node uses to hand a seat back included — increments the delivery count.
- **Cost-when-tried:** the Apache Pulsar backend. It has no compare-and-set, so it could not hold the coordination state at all, and every Pulsar deployment ran a second NATS estate beside it to serve one company.
- **Tag-status:** unreleased

## The decision

Messaging is a contract with one interface and two certified implementations:
an in-memory twin for tests and NATS JetStream for everything real — embedded
in the engine's own process by default, clustered or external for a fleet.
Embedded versus external is a *connection* choice rather than a second backend:
the same client code, the same subjects, the same consumer configuration.

**Nothing above `internal/queue` may branch on which backend is running**, and
that rule is what keeps the twin honest.

The load-bearing half is not the messaging. It is that **the coordination KV
rides the same NATS connection that carries this node's inbox** — seat leases,
the completion ledger, the delivery dedupe, the token counter, the activation
pointer and the company's sealed secrets all on the connection the seat's mail
arrives over.

## Why the obvious alternative is wrong

The obvious alternative is to pick the best broker for messaging and the best
store for coordination, and run both. The operational cost of two estates is
the smaller half of what that buys.

The real cost is that two connections to one company's infrastructure **fail
independently**. A node can hold live leases over a connection that still works
while the one carrying its inbox has dropped: alive to its peers, deaf to its
work, and holding every seat it is no longer serving. One connection makes that
state unrepresentable, which is worth more than any broker feature the second
estate would have brought.

A broker that cannot compare-and-set therefore cannot be a backend here,
however good its messaging is. That is not a preference; it is the reason
Pulsar was retired rather than kept as a second option.

## What this does not decide

It does not decide what the stream carries. An *ordered log* per state-log
domain is a different thing from a seat's mailbox, and the rules over it are
[ADR-0002](0002-the-stream-is-the-write-ahead-log.md)'s.

It does not make every verb durable. `Ask` and `Serve` are an ephemeral
scatter — core NATS request/reply, no stream, no consumer, no ack, no retention
— because a query is not an event, and a search fan-out on the durable verbs
would write two records and an audit row per keystroke.
