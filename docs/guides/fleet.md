# Running a Fleet

One `crewlet run` is a whole company. Everything below is about running
more than one, and the honest summary is: you probably do not need to.
A single engine handles many concurrent turns — agent handlers are
goroutines, so the practical ceiling is LLM provider rate limits and
host memory, not process count. **Scale up before you scale out.**

Run a fleet when one of these is true:

- **A node's failure is not acceptable downtime.** A second node takes
  over a dead one's seats within a lease TTL (45 s), and a rolling deploy
  hands them over gracefully.
- **You need to terminate traffic separately from running agents.** An
  ingress node in a DMZ, agents on a private host.
- **Some seats have to run somewhere specific.** A seat that needs a
  network zone, a filesystem, or a GPU that only one host has.

It is not a throughput lever on its own: `node.max_concurrent` is per process,
so N nodes is N × that ceiling whether you wanted it or not. Size it per
node.

## What a fleet needs

**Shared coordination.** Seat leases live in the `coordination` slot.
`coordination.type: local` holds them in this process, so every node
believes it owns the whole company; the engine logs
`seat_placement_is_process_local` at boot. It is *only* the leases: the
fleet's shared records — the token counter, the completion ledger, the
delivery dedupe, agent-to-agent channels, scheduled-fire claims, detached
sandbox runs — are on the KV whatever this setting says, because each of
them has to outlive the process rather than merely be visible to a peer. A fleet needs
`coordination.type: embedded-kv` — and Tier A refuses `local` beside a
clustered or external stream by name, because this is the one
misconfiguration that would otherwise silently give two processes the
same agents.

**One node or three, never two.** Two embedded-KV members have no quorum
without each other, so the fleet stops serving the moment either
restarts — and a rolling upgrade restarts them one at a time, which makes
the outage certain rather than unlucky. Tier A refuses a two-member
config by name.

**The right broker settings, on Pulsar.** An unowned seat's subscription
has no connected consumer, and two Pulsar reapers delete exactly that: set
`subscriptionExpirationTimeMinutes` to `0` and
`brokerDeleteInactiveTopicsEnabled` to `false` (or
`brokerDeleteInactiveTopicsMode` to
`delete_when_subscriptions_caught_up`). The repo's `docker-compose.yml`
ships these values under its `pulsar` profile, and CI certifies the
backend against a broker configured this way. Nothing in the engine can detect a broker that is
quietly deleting a quiet seat's mail.

**A distinct, stable id per node.** `node.id` in the Tier A file, or
`CREWLET_NODE_ID`, which is how an orchestrator injects a pod name
without templating the config. Two nodes sharing an id miscount the fleet
and each compute too small a share.

**The schema, applied first.** [`crewlet migrate`](../reference/cli.md#crewlet-migrate)
before starting any node.

## Node roles

Every process declares what it is willing to do. The default is all three
— that is the single-node deployment, and no existing config changes.

```yaml
# Tier A, per node
node:
  id: "${CREWLET_NODE_ID}"
  roles: [ingress, seats, workers]   # the default; omit the key
```

| Role | What it does |
|---|---|
| `ingress` | Serves the HTTP API: webhooks from every integration, the dashboard, the REST endpoints |
| `seats` | Claims seat leases, spawns the agents, consumes their inboxes, runs turns |
| `workers` | The company-wide singleton duties — scheduler tick, retention sweep, sandbox waiter, skill clustering and curation, seat-subscription creation |

A role is subtracted from **this node, not from the company**. That means
a fleet can be assembled, node by node, into a shape where a whole job is
done by nobody while no single node's config is wrong — and every symptom
is an absence: nothing fires on a schedule, no webhook arrives, no
sandbox run is ever collected. Nothing raises. So the engine checks the
shape against live node presence and logs `fleet_role_unmanned` when a
role has nobody doing it, and `fleet_role_manned` when it comes back.

A node that does not run seats is also excluded from the denominator its
peers divide seats by. Counting an ingress-only node would shrink every
other node's share and strand the difference.

### How `workers` is enforced

Two independent things decide who runs a singleton duty, and both are
needed:

- **The `worker:{duty}` lease** decides *which* node among the ones asking.
  Without it, two willing nodes both run the sweep.
- **The `workers` role** decides *whether this node asks at all*. Without
  it, a node explicitly configured `roles: [seats]` competes for every
  duty — and wins some of them.

A node without the role **refuses** the duty rather than abstaining from
the claim, which is a distinction with teeth: internally, "no duty gate"
means "there is no fleet, so this is always mine" — the single-node case.
An ingress-only node that abstained would therefore run every duty. The
refusal is silent, because the node is doing exactly what it was told to.

### Common shapes

```yaml
# Three interchangeable nodes. Simplest fleet there is.
node: {id: "${CREWLET_NODE_ID}"}          # all roles, on each
```

```yaml
# Ingress split out: two API nodes behind a load balancer, three
# engines. The API nodes still need the database and the broker.
node: {id: "${CREWLET_NODE_ID}", roles: [ingress]}
node: {id: "${CREWLET_NODE_ID}", roles: [seats, workers]}
```

```yaml
# A satellite: agents only, no duties, no inbound traffic. Runs seats
# pinned to it, in a network zone the rest of the fleet is not in.
node: {id: sat-eu-1, roles: [seats], labels: {zone: eu}}
```

That last one is the shape to reach for when a *single* agent needs to
be somewhere specific — a host that can see an internal API, a licensed
binary, a GPU — and the rest of the company should stay put.
**[Running One Agent Somewhere Else](satellite-nodes.md)** walks it end
to end.

## Placement

By default any node that runs seats may hold any seat, and the fleet
converges on a fair share. `role.placement` narrows that:

```yaml
# Tier B, on a role
roles:
  - name: EU Support
    placement:
      labels: {zone: eu}        # any node carrying this label

  - name: Build Engineer
    placement:
      node: builder-1           # exactly this node
```

Give both and both must hold — a placement only ever narrows. Labels come
from `node.labels` in each node's Tier A file and are compared exactly;
they are advertised on the node's presence lease, so a label change takes
effect one heartbeat after the restart that made it.

**The share is computed per placement group.** Nine seats pinned to one
node and one seat free, across three nodes: a single fleet-wide
`ceil(10/3)` would let the pinned node take four of its nine and leave
five claimable by nobody — stranded, while every node reported a healthy
sweep. Each group's share is `ceil(group size / nodes eligible for it)`,
and a node's capacity is the sum over the groups it belongs to.

**A seat no live node matches is not served.** The engine will not widen
a selector to place a seat — widening it is exactly what the operator
asked it not to do — so it logs `seats_unplaceable` with the handles and
leaves them. Expect this after a pinned node dies: the pin is a
constraint, and a constraint has a cost.

**A seat that stops matching is handed back.** Narrow a selector under a
node that holds the seat, or change that node's labels, and it releases
the seat voluntarily at the next sweep — the in-flight turn finishes,
then an eligible peer picks it up.

**A placement narrows a seat, not a node.** A satellite that runs seats
is eligible for every *unpinned* seat too — it will take its share of
them like any other node. If a seat needs something only the core has,
pin it to the core; do not assume a satellite will leave it alone.

### Placement and sandboxes

A [sandboxed](../concepts/code-sandbox.md) seat may be pinned like any
other, with one thing to get right that the engine cannot check for you:

- The node holding the seat **launches** the sandbox, so it must reach
  the sandbox provider (E2B cloud, or your self-hosted `domain`).
- Whichever node holds the `sandbox-waiter` duty **polls** it, so that
  node must reach the same provider. On a satellite (`roles: [seats]`)
  the duty is on a core node by construction — check that one too.
- `CREWLET_SANDBOX_OTEL_RECEIVER_URL` must point at an address the
  *sandbox* can reach, which means an ingress node. It is explicit
  config, never derived from the node that happens to launch the run, so
  a satellite never advertises itself by accident.

Pinning a sandbox seat to a node that cannot reach the provider produces
a seat that claims fine and fails every run. That is a network fact about
your deployment; the engine knows only what a node says it is, not what
it can talk to.

## Draining and rolling upgrades

A node stops by draining: it stops taking new work, lets in-flight turns
finish, releases each seat as it goes idle, and exits. Peers pick the
seats up. Point load-balancer readiness at `/ready` (`503` while
draining) and liveness at `/health` (stays `200` through a drain), and
give the orchestrator a termination grace period longer than your longest
turn — the engine does not impose its own cutoff, because that would be a
guess at yours.

**Upgrade one node at a time, and let each one finish.** Seat leases
carry a protocol version, and a node refuses to claim seats while any
live lease is held at an older one. The rule is asymmetric on purpose:
older nodes keep working, newer ones wait — visibly, with
`seat_claims_blocked_by_older_protocol` — until the last old lease lapses
or is released. A rolling deploy converges because that is what a rolling
deploy does.

Two consequences worth stating plainly:

- **A stalled rollout stalls placement.** If you leave one old node
  running, the new ones hold nothing. The log line says so; watch for it.
- **Rolling *back* across a protocol bump needs a full stop.** An older
  build has no protocol check at all, so it will happily take over a
  newer node's expired leases. Nothing in the table can stop it.

## Watching a fleet

- **`fleet_role_unmanned`** — a job nobody is doing. Fix the roles.
- **`seats_unplaceable`** — a seat nobody may run. Fix the selector, or
  start a node that matches.
- **`seat_claims_blocked_by_older_protocol`** — an unfinished upgrade.
- **`seat_placement_is_process_local`** — no database. Every node thinks
  it owns everything.
- **`/health`** carries this node's seats, its in-flight count and its
  config posture; the dashboard's **Fleet** view puts every node's
  side by side, with seat ownership and per-node config epoch.

A node whose applied config epoch lags the fleet's is not an error on its
own — every rollout produces lag. See
[the control plane](../concepts/control-plane.md) for when lag becomes a
posture change.

## See also

- [Running one agent somewhere else](satellite-nodes.md) — the
  satellite shape end to end: pinning one seat to a host that can reach
  what it needs, and what that pin costs
- [Scaling out](../concepts/scaling.md) — the model underneath this guide:
  what a node is, what the fleet shares, and where the constants above
  (the 45-second TTL, the prefetch cap) were measured
- [Seat ownership](../concepts/seat-ownership.md) — leases, fencing,
  admission, and what a takeover actually does
- [Control plane](../concepts/control-plane.md) — how a config revision
  reaches every node
- [Deployment](deployment.md) — processes, database, broker, probes
