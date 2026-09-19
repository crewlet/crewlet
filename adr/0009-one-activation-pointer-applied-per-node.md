# ADR-0009 — Which revision is current is fleet-wide; applying it is per node

- **Status:** accepted
- **Authority:** `internal/configplane`
- **Enforced-by:** nothing
- **Cost-when-tried:** activation used to be delivered over a competing-consumer subscription. Exactly one replica applied a revision and every other node went on running the previous company indefinitely — with no error anywhere, because from each node's point of view nothing had happened.
- **Tag-status:** unreleased

## The decision

A running company takes a new configuration without a restart, and the two
halves of that are answered in different places.

**Which revision is current** is a fleet-wide fact, held as an **append-only
activation pointer** in coordination. Every node polls it and reconciles
against it, and records its own outcome — `ok` / `error` / `degraded` — so a
stalled rollout is visible rather than silent.

**Applying it** is a per-node act. Within one process an apply builds a new
**epoch**: the organization, the model registry, the tool registry and
everything else derived from the document, built and validated together and
published as one value. Nothing already published is edited. A turn pins the
epoch it started on and reads only that one until it ends, so a change reaches
a seat at its next turn, and a revision that fails to build is refused before
it is published, leaving the previous epoch serving.

The pointer is **append-only rather than a row keyed on the revision id**,
because re-activating an unchanged revision is the documented
credential-rotation gesture — so "the payload did not change" cannot be
treated as "nothing to do".

## Why the obvious alternative is wrong

The obvious alternative is to publish the new configuration as an event and let
each node apply what it receives. That is what this did, over a
competing-consumer subscription, and the failure has no symptom: one replica
consumed the message and applied, every other node never saw it, and each of
them was serving a company it believed to be current. Nothing errored, nothing
lagged, and the fleet ran two different organizations until somebody noticed a
seat answering with an old role.

Publishing a whole new epoch rather than mutating a shared one is the other
half, and it is not tidiness: turns run in genuine parallel on goroutines, so
editing the live organization is a data race with no owner.

## What this does not decide

It does not decide what a lagging node does — `serve` / `wait` / `shed` /
`isolated` / `stuck` is the control plane's own posture table, and lag alone
never sheds.

## Why Enforced-by is `nothing`

A gate would have to observe that two nodes applied the same pointer and
converged on the same epoch, which is a property of a running fleet rather than
of the source. `internal/configplane`'s own suite covers the posture arithmetic
and the reconcile cadence; what nothing checks is the shape the failure took —
a delivery mechanism that reaches one node. The nearest static formulation
would be "no configuration is applied from a consumed message", and the
candidate is a walk for a subscription over the config subjects outside
`internal/configplane`. It is written down here rather than built because the
shape it would forbid is not currently reachable: nothing subscribes to those
subjects at all.
