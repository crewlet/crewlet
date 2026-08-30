# Scaling Out

**One node is the whole company, and more than one is supported.** A single
`crewlet run` holds every seat, serves the API, and runs every company-wide
duty. That is the design's degenerate case, not a lesser path — a fleet takes
exactly the same code down exactly the same paths, with the leases held by more
than one process.

So the standing advice is **scale up before you scale out**. Agent handlers are
goroutines, so a single engine's practical ceiling is LLM provider rate
limits and host memory, not process count. A fleet buys availability, traffic
separation, and placement — not throughput. [Running a Fleet](../guides/fleet.md)
is the operator guide; this page is the model underneath it.

---

## What a node is

A node is one process that has declared what it is willing to do. Everything
else about it — which seats it runs, which duties it holds, which config
revision it is on — is discovered at runtime from shared state, never
configured per node.

```yaml
# Tier A, per node
node:
  id: "${CREWLET_NODE_ID}"           # distinct and stable, per process
  roles: [ingress, seats, workers]   # the default; omit the key
  labels: {zone: eu}                 # optional, matched by role.placement
  max_concurrent: 32                 # agent turns this process runs at once
```

| Role | What it does |
|---|---|
| `ingress` | Serves the HTTP API — every integration's webhooks, the dashboard, the REST and WebSocket read surface |
| `seats` | Claims seat leases, spawns the agents, consumes their inboxes, runs turns |
| `workers` | The company-wide [singleton duties](seat-ownership.md#singleton-duties) — scheduler tick, sandbox waiter, retention sweep, skill clustering and curation, seat-subscription creation. These read their work list from the **org**, never from the node's own seats: a `workers` node runs no seats at all, so a duty that iterated the local agent pool would cover nothing |

```mermaid
flowchart TB
    subgraph fleet["The fleet"]
        N1["node-a<br/><i>ingress · seats · workers</i>"]
        N2["node-b<br/><i>ingress · seats · workers</i>"]
        N3["sat-eu<br/><i>seats</i> · zone=eu"]
    end
    subgraph shared["Shared state — the company · one NATS estate"]
        KV[("Coordination KV<br/>leases · config epochs<br/>counters · ledgers")]
        MQ[("JetStream streams<br/>one durable consumer<br/>per seat inbox")]
    end
    N1 --- KV
    N2 --- KV
    N3 --- KV
    N1 --- MQ
    N2 --- MQ
    N3 --- MQ
```

That shared box is a *logical* one. In the default fleet shape it lives inside
the nodes themselves — each embeds a member of one NATS cluster and the streams
replicate between them (`stream.cluster.*`, `stream.replicas: 3`) — and
`stream.type: nats` is the same picture with the estate moved out to a cluster
somebody else runs. One estate either way, carrying both slots — see
[what the fleet shares](#what-the-fleet-shares).

**The node id must be distinct and stable across restarts.** It comes from the
deployment (`CREWLET_NODE_ID`, or `node.id` in the Tier A file) rather than
being generated, because a fresh value per boot orphans whatever the previous
incarnation registered under the old one. Two nodes sharing an id miscount the
fleet and each compute too small a share of the seats. On a clustered embedded
stream it is also this member's NATS server name, and JetStream places stream
replicas **by server name**: a node that comes back under a new one is a new
peer, its old replicas orphaned on a member that no longer exists, and the
stream left short of quorum waiting for a server that will never return. It is
never generated for you either: an unset id is `node-0` on *every* node, which
is the two-nodes-sharing-an-id failure with an extra symptom — NATS refuses a
route from a member whose name it already knows.

A role is subtracted from **this node, not from the company**, so a fleet can be
assembled node by node into a shape where a whole job is done by nobody while no
single node's config is wrong. The engine checks the assembled shape against
live node presence and logs `fleet_role_unmanned` when a role has nobody doing
it. See [the fleet guide](../guides/fleet.md#node-roles).

---

## What had to be true for this to work

Running two engines against one company is not a matter of adding a lock. The
couplings that made it unsafe fall into five kinds, and **only one of them is
what a lock fixes**:

| | The coupling | What resolves it |
|---|---|---|
| **1** | **Control plane** — config activation, secret rotation, identity maps were delivered over a competing-consumer subscription, so exactly one replica applied a revision and the rest ran the previous company forever | An append-only activation epoch every node polls — see [Control Plane](control-plane.md) |
| **2** | **Process-bound resources** — a seat's stdio MCP servers are child processes of the engine holding its credentials. A seat's tools live where its subprocesses live, so "any node can serve any seat" is false unless placement decides tools *and* routing together | Seat leases: claiming a seat is what spawns its MCP children — see [Seat Ownership](seat-ownership.md) |
| **3** | **Per-seat exclusion** — two nodes attached to one seat's durable consumer split its traffic, running one agent's conversation as two interleaved turn streams that neither can see | A TTL lease with an epoch fencing token. **This is the class a lock fixes**, and it is one of five |
| **4** | **Shared mutable counters** — budgets, concurrency, webhook dedupe, credential cooldowns were per-process, so an org cap of 500 k tokens silently became N × 500 k | Shared storage, not exclusion — see [what the fleet shares](#what-the-fleet-shares) below |
| **5** | **Boot walks with external side effects** — schema migration, sandbox recovery, skill clustering all ran unconditionally at boot, where "abandoned by a dead engine" is a valid inference only when there is exactly one engine | Singleton duty leases, an advisory lock on `migrate()`, and per-seat recovery inside the acquire hook |

Class 3 is the one everybody expects and the only one a mutex addresses. The
other four are why "just take a lock and run N replicas" does not work, and why
the answer is a node model rather than a lock.

---

## What the fleet shares

Everything that must be true for the *company* rather than for a process lives
in the coordination slot — a fleet-shared key/value store, distinct from the
node's own database. A fleet is not configured; it is discovered from these
slots, which is why adding a node is starting a process and removing one is
stopping it.

| Slot | Answers | Documented in |
|---|---|---|
| `leases` | Which node runs which seat, which node holds which duty, and which nodes are alive at all | [Seat Ownership](seat-ownership.md#the-lease) |
| `config` · `status` | Which company revision is current, and which nodes have reached it | [Control Plane](control-plane.md) |
| `ledger` | Has this trigger already been worked — read before a turn, written after one | [The completion ledger](seat-ownership.md#the-completion-ledger) |
| `claims` | Has this inbound delivery been seen — the dedupe that used to be a per-process map, and that GitHub and GitLab did not have at all | [Event System](event-system.md) |
| `cooldowns` | Which provider key is cooling after a 429. Per-process monotonic values are not even *comparable* across nodes | [Deployment](../guides/deployment.md) |
| `rate` | The notification valve | [Event System](event-system.md) |
| `budgets` | Org and per-seat spend against the cap. Caps stay config-derived in memory; only *usage* is shared | [Deployment § Token budgets](../guides/deployment.md#token-budgets) |

The full list, what each retention is sized from, and what deliberately stays
node-local are in [Coordination](coordination.md).

The stream carries the other half: one durable consumer per seat inbox,
attached only by the node holding that seat's lease. That consumer **is** the
mailbox — it holds an unowned seat's mail until somebody claims it, and it is
created with no inactivity threshold precisely so nothing reaps it while a seat
is between owners. It also has to exist *before* anything publishes: the agent
and notification streams keep a message only while some durable consumer that
has not acked it exists, so a subject no consumer covers drops what is
published to it. See [a seat's mailbox](event-system.md#a-seats-mailbox-exists-before-the-seat-is-running)
and [the fleet guide](../guides/fleet.md#what-a-fleet-needs).

**Both halves ride one connection**, deliberately. The coordination KV is
JetStream KV on the stream's own NATS connection rather than an estate of its
own: two connections to one broker fail independently, so a node could hold
live leases over a connection that still works while the one carrying its inbox
has dropped — alive to its peers, deaf to its work. One connection makes
"reachable" a single fact about a node rather than two that can disagree.

### What stays per-process, deliberately

- **`max_concurrent`.** Tier A's `node.max_concurrent` (default 32) is the gate
  every agent turn passes through, and it is per node — so an org's ceiling is
  N × the configured value. Size it per node, not per company. This is the one
  knob a fleet genuinely changes the meaning of. (A cli-agent provider's own
  `max_concurrent`, under `providers.llm.<name>.cli`, is a different knob: it
  caps that provider's subprocesses.)
- **A seat's MCP subprocesses.** They are children of the node that claimed the
  seat, and they die with the release.
- **The tool-skill registry.** It warms a local cache rather than producing
  shared state, so every node runs the boot walk — a node that skipped it would
  have agents with no tool skills at all. The test for whether periodic work is
  a [singleton duty](seat-ownership.md#singleton-duties) is exactly this: shared
  state, or a local cache?
- **The dashboard's live-state projection.** Each ingress node builds its own
  from the event stream, so any node can answer without a fan-out.

---

## Where the constants come from

The seat-handover constants are set by what the shipped backend actually does,
and the shipped backend is **NATS JetStream** — embedded in each node or dialled
as an external cluster, the same client code either way
(`internal/queue/jetstream`, where each number carries its reasoning at its
definition). The behaviours they rest on are held by the ONE conformance suite
every backend runs (`internal/queue/queuetest`), and it is worth being exact
about how: it asserts the **behaviour** each number describes — that a durable
consumer retains mail with nothing attached, that re-attaching replays it in
order, that a successor receives what its predecessor never acked, that one
client's detach leaves its peers attached — and it asserts no timing. A broker
that got slower would not fail the build; a broker that stopped behaving this
way would.

| What a handover rests on | What the backend does |
|---|---|
| Creating a seat's mailbox | A durable consumer created with **nothing attached**, at `DeliverAll` — **1.7 ms**, a plain client call, which is what makes it affordable for every node to create every seat's mailbox at boot |
| Handing a seat over cleanly | The loser NAKs its unfinished partition (a `Defer`); the successor sees it in **about a millisecond** |
| Losing a node with no handoff | Nothing NAKs, so those deliveries wait out the ack window — **30 minutes** — before they are redelivered |
| Prefetch a consumer can hold | **None.** Pull consumers fetch one message, or one drain's worth, when they are ready to run it |
| Delivery budget | **25**, then the dead-letter stream. A handoff spends one of them, because a NAK counts as a delivery |

What each one decides:

- **One durable consumer per seat, attached by whoever holds the lease, is
  sound.** The cursor belongs to the *consumer*, not to the connection that was
  reading it, so a change of owner replays nothing and loses nothing: what the
  loser never acked is exactly what the successor is handed. A consumer per node
  instead would either replay the whole subject on every handoff or lose
  whatever arrived while a seat was unowned. The suite asserts it rather than
  taking the broker's word for it.
- **The broker imposes no floor on the lease TTL** — so the **45-second TTL**
  is not a number any broker measurement sets. Creating a mailbox costs
  ~1.7 ms and a clean handoff returns the mail in about a millisecond, so a
  successor is productive essentially immediately. What
  bounds the TTL is heartbeat reliability: 45 s is three **15-second** heartbeat
  intervals, which tolerates two consecutive missed renewals — a GC pause, a
  store blip, a scheduling hiccup — with a full interval left to recover in,
  plus headroom for the skew between two machines' opinions of the time. Shorter
  drops a healthy node's seats on ordinary jitter, and every spurious handoff
  costs a real MCP respawn; longer is time a dead node's seats sit dark, because
  nothing can claim them until the TTL runs out.
- **The claim-rate limit is not about attaching.** At a millisecond, attaching
  is free. The real cost of a takeover is spawning that seat's MCP children,
  which is what the limiter is sized against: four claims per five-second sweep
  — twenty seats absorbed in ~25 s, and never more than four subprocess trees
  forked in one tick — against two releases, since giving a seat up interrupts a
  live agent while claiming one only starts it.
- **A wedged-but-alive node holds only what it already fetched.** There is no
  prefetch to hold hostage: a pull consumer asks for work when it is ready to
  run it, so a loop that has stopped turning is holding at most the batch in its
  hands. It also stops asking without being told to — admission reads the
  *freshness* of this node's last renew, so within one heartbeat interval the
  next delivery here is deferred, which NAKs it straight back and quiesces the
  consumer. What that does **not** cover is the delivery a wedged handler is
  still sitting on: nothing NAKs it, the ack clock is server-side and per
  message, and ending the process does not shorten it — that batch returns to
  the successor when the 30-minute window expires, not when the corpse falls
  over. The successor serves everything published after it claims the seat
  normally; it is the already-fetched batch that waits. So the
  [event-loop watchdog](seat-ownership.md#the-wedged-node-and-why-it-leaves)
  **ends the process** for blunter reasons than releasing mail: a wedged node
  neither works nor dies. It keeps its MCP children, its credentials and any
  turn already past the ownership check — and a turn's external side effects, a
  chat post or a work-item comment, are not something an epoch fence can reach —
  while still answering a liveness probe, so nothing restarts it. In a fleet
  its leases lapse and peers absorb the seats, so what is lost is that node's
  capacity; on a single node there is no peer, and the seats stay dark until a
  person notices. Nothing can be signalled out of that state either, because
  the code that would handle the signal is the code that is stuck;
  `os.Exit(75)` is the only unilateral move, and the distinct exit code is what
  makes a supervisor's restart log say what happened. It is also
  why correctness against zombies comes from epoch fencing rather than from
  waiting for anything to time out.

---

## What the design does not promise

Stated plainly, because each of these is a real window rather than a
theoretical one:

- **Exactly-once external side effects do not exist.** Not here and not in any
  design that calls non-transactional external APIs at-least-once. The
  [completion ledger](seat-ownership.md#the-completion-ledger) and the per-round
  fence *bound* the duplicate window; they cannot close it. A node dying
  mid-turn may repeat a Slack post — the same window a single engine has on
  force-stop and broker redelivery, now named.
- **A wedged-alive zombie can act for up to one LLM round plus one heartbeat
  interval** after losing its lease. Fencing bounds the damage to that window;
  it does not prevent the window.
- **Two seat-scoped writes still duplicate, deliberately.** A
  differently-worded `agent_diary` entry (identical content already
  collapses on write) and a `token_usage` row. Nothing can key a reworded
  memory to its twin — that needs the duplicate *turn* not to happen, which
  is the completion ledger's job — and `token_usage` is observability on a
  high-volume path swept on a TTL, where a guard costs more than the skew.
  Budget *enforcement* is unaffected: it reads the fleet's shared
  counter. `episodes` and the counterparty interaction
  count are collapsed against the reader that matters — both live in the
  node's own database, which only that node reads, so the duplicate the
  work key removes is the one *that* node would otherwise write twice — and
  onboarding was already exclusive; see
  [Keying a write on the work](seat-ownership.md#keying-a-write-on-the-work).
- **Per-company singletons remain singletons.** They sit behind leases so any
  node can host them, but the scheduler tick, the curator, clustering and the
  sandbox waiter are each one logical instance at a time. A fleet does not
  parallelise them.
- **A rolling upgrade across a protocol bump has a visible outage window**, and
  a rollback across one needs a full drain. See
  [Mixed-version fleets](seat-ownership.md#mixed-version-fleets).

---

## See also

- [Running a Fleet](../guides/fleet.md) — the operator guide: node roles, seat placement, draining and rolling upgrades
- [Running One Agent Somewhere Else](../guides/satellite-nodes.md) — the satellite shape, walked end to end
- [Seat Ownership](seat-ownership.md) — leases, epoch fencing, admission, the completion ledger, singleton duties
- [Control Plane](control-plane.md) — how a config revision reaches every node, and the posture a lagging one takes
- [Deployment](../guides/deployment.md) — processes, database, broker settings, probes
