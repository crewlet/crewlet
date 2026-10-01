# ADR-0001 — One broker carries both the stream and the coordination store

- **Status:** accepted
- **Authority:** `internal/queue`
- **Enforced-by:** `internal/queue/topics.TestNoPackageBuildsASubjectByHand`, the `queuetest` conformance suite every backend runs, and `internal/queue/jetstream.TestEveryConnectionANodeDependsOnIsWatched`, which holds that every connection a node runs on is watched on both topologies
- **Measured:** creating a durable consumer with nothing attached is an ordinary API call at about 1.7 ms. The delivery budget before a message is dead-lettered is 25 rather than the ~10 a broker with a free handoff would need, because every path back to the broker — the Nak a node uses to hand a seat back included — increments the delivery count.
- **Cost-when-tried:** the Apache Pulsar backend. It has no compare-and-set, so it could not hold the coordination state at all, and every Pulsar deployment ran a second NATS estate beside it to serve one company. And a second connection to the one broker, dialled UNWATCHED: the embedded broker's coordination connection was opened with no closed handler, so NATS closing it for good left a node consuming work over the queue's connection while every lease renewal failed, reported by nothing.
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
lives on the same broker that carries this node's inbox** — seat leases, the
completion ledger, the delivery dedupe, the token counter, the activation
pointer and the company's sealed secrets all on the servers the seat's mail
arrives from — **and that a node runs on that broker only while every
connection it holds to it is open**.

How many connections that is depends on the topology:

- **On an external broker, one.** The coordination store rides the queue's own
  connection.
- **On the embedded broker — the default — two.** The coordination store has a
  second connection to the in-process server
  (`jetstream.Queue.DialWatched`), and the seat-memory replay and the backup's
  copies of the streams ride it too. That is deliberate: a seat's memory replay
  is an ordered consumer over its whole history and a backup copies whole
  streams, and on the queue's own connection either would sit on the read loop
  every mailbox consumes through.

**Both connections are watched.** NATS closing either one for good — rather
than reconnecting — stops the node: it drains, tears down and exits non-zero
for whatever supervises it to restart it (`internal/engine`'s `Engine.Fatal`).
So the split the obvious alternative risks — a node holding live leases over
one connection while the other, carrying its inbox, is gone — lasts no longer
than a reconnect: while the client is still reconnecting it expects both back,
and once it gives up on either, the node leaves.

## Why the obvious alternative is wrong

The obvious alternative is to pick the best broker for messaging and the best
store for coordination, and run both. The operational cost of two estates is
the smaller half of what that buys.

The real cost is that two estates **fail independently**, and in a way no one
node can see. Every connection a node holds can be open while the two estates
disagree about it — the coordination cluster sees its leases renewed while the
stream cluster has lost its inbox. Alive to its peers, deaf to its work, holding
every seat it is no longer serving, and nothing on the node closed for it to
notice. One broker makes that state unrepresentable: the leases and the mail
are on the same servers, so reaching one is reaching the other, which is worth
more than any broker feature the second estate would have brought.

What one broker leaves is narrower: a connection to it can close on its own,
and on the embedded broker a node holds two. That residue is closed by
WATCHING rather than by sharing — sharing one connection would put the replay
and the backup's bulk reads on the mailboxes' read loop, and a watched second
connection costs the split nothing but the length of a reconnect.

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
