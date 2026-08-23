# d-202 — Where a Pulsar topology keeps its leases

**Status:** DECIDED and BUILT. Derivable from d-201 and the two-slot model; no
operator input needed.

## The question

The two-slot model lets the stream and the coordination store be chosen
independently, and `Bootstrap.validateTopology` already refuses the incoherent
pairings — including `stream.type: pulsar` with `coordination.type: local`,
because Pulsar has no compare-and-set and a lease needs one (d-201).

That leaves exactly one supported Pulsar topology: `pulsar` + `embedded-kv`.
And it could not run. `openPulsar` refused every call with "a pulsar stream
needs a coordination cluster this build does not start yet", because the
coordination store is a NATS KV and there was no NATS connection to open it on
— `coordination:` had two fields, a type and a TTL, and neither says where a
NATS estate is.

So a slot that config accepted, validated and documented was unreachable at
runtime. Every Pulsar deployment was refused at boot.

## What was decided

**A Pulsar topology runs two estates: Pulsar for the stream, NATS for the
leases.** The coordination block gains the same two shapes the stream slot
already has — dial one an operator runs, or embed a member of a cluster the
nodes form among themselves:

```yaml
stream:
  type: pulsar
  url: pulsar://broker:6650
  tenant: acme
  namespace: default
coordination:
  type: embedded-kv
  nats:
    store_dir: /var/lib/crewlet/coord   # embedded; or url: nats://coord:4222
    cluster:
      name: crewlet-coord
      peers: [nats://b:6222, nats://c:6222]
    replicas: 3
```

Embedded is the shape that keeps the deployment count down. The estate carries
leases and nothing else — a few keys, written on a heartbeat — so it is small
enough to live inside the processes that use it, and an operator adding Pulsar
does not also acquire a NATS cluster to operate.

## Why the block is refused everywhere else

On a NATS or embedded stream, `coordination.nats` is a validation error rather
than an ignored field. The coordination store there rides **the stream's own
connection**, deliberately (d-201): two connections to one broker fail
independently, so a node could hold live leases over a connection that still
works while the one carrying its inbox has dropped — alive to its peers, deaf
to its work. Allowing the block on those topologies would let an operator
configure exactly that.

## Two things this exposed

**The quorum check was reading the wrong cluster.** `validateTopology` counts
`stream.cluster.peers` to refuse a two-node fleet — two embedded KV members
have no quorum without each other, so the fleet stops serving the moment either
restarts. On a Pulsar topology the stream block describes a cluster that does
not hold the leases, so a two-member *lease* cluster counted as one node and
passed. The count now comes from whichever block actually holds them.

**An empty estate is refused at open, not just at validate.** `OpenBackends`
deliberately does not re-validate the topology, but this one is not a duplicated
rule: "embed a member with defaults" and "nothing was said about leases" are the
same value, and reading it the first way gives every node in a fleet its own
private in-memory lease table. Every node then claims every seat, and nothing
anywhere reports a problem — which is the failure the whole slot exists to
prevent.

## Ordering

The coordination estate opens **before** the Pulsar client, and its failure
tears the estate down. A node that reached its broker and then found it had
nowhere to keep leases would sit attached to topics it must not take work from,
while the operator read a lease error and looked at a healthy broker.

The cleanup is covered by a goroutine-count assertion rather than a
reachability one, because `pulsar.NewClient` connects **lazily**: a dial at a
dead port succeeds and fails at the first attach, so an unreachable broker is
not a failure `OpenBackends` can observe. The test uses a stream URL the Pulsar
client refuses synchronously instead.

## What is still not covered

A live Pulsar broker is not reachable from the engine package's tests, so the
assembled topology is exercised as far as the lease store — acquire, and refuse
a second owner — and no further. The stream half is certified by
`internal/queue/pulsar`'s conformance run against a real broker in CI (gate
G3b).
