# Running One Agent Somewhere Else

Sometimes a single seat needs to be somewhere the rest of the company is
not: a support agent that must reach an internal API only one VPC can
see, an engineer whose MCP server drives a licensed binary installed on
one host, a seat that needs a GPU, a lab network, or a database behind a
jump host.

You do not move the company for that. You start one more Crewlet process
on that host, tell it what it is, and pin that one role to it. Everything
else — the API, the webhooks, the scheduler, the other agents — stays
exactly where it is. This shape is called a **satellite**: a node that
runs agents, holds no company-wide duty, and terminates no inbound
traffic.

> A satellite is a normal `crewlet run` with roles subtracted, not a
> lighter agent-only binary. It still needs to reach the database and
> the broker. If that is not possible from where you want the agent,
> this is not the mechanism you want — see
> [what a satellite still needs](#what-a-satellite-still-needs).

---

## What actually moves

Pinning a seat moves the whole seat, which is the point — an agent is
not a request handler you can route, it is a place where things run:

| Moves to the satellite | Stays on the core |
|---|---|
| The agent instance, its state and its turns | The HTTP API, the dashboard, every integration's webhooks |
| **Its per-role MCP servers**, spawned as child processes of the node that claimed the seat | The scheduler tick, the retention sweep, the sandbox waiter, skill clustering and curation |
| Its LLM calls, its knowledge searches, its tool calls | Every other seat's agents and MCP servers |
| Its sandbox launches, if the role is sandboxed | The company config, the leases and the ledgers — all shared, in the coordination slot |

The MCP row is the one that makes this feature what it is. A seat's
stdio MCP servers are children of the process holding its lease, so
"pin the seat" and "run its tools over there" are the same act. That is
also why the mechanism cannot be a routing trick: the tools are not
somewhere the request can be forwarded to, they are somewhere the
*agent* has to be.

Work still reaches it normally. A trigger is published to the seat's
inbox topic, and the node holding that seat's lease is the one consuming
it — so nothing that publishes has to know where the agent is, and a
Slack message routed to that agent works exactly as before.

```mermaid
flowchart LR
    subgraph core["Core network"]
        C1["node-a<br/><i>ingress · seats · workers</i>"]
        C2["node-b<br/><i>ingress · seats · workers</i>"]
        KV[("Coordination<br/>leases · ledgers")]
        MQ[("Event stream")]
    end
    subgraph zone["Restricted network"]
        S["sat-eu<br/><i>seats</i> · zone=eu"]
        API["internal API<br/>only reachable here"]
        S -->|its MCP server| API
    end
    C1 --- KV
    C2 --- KV
    C1 --- MQ
    C2 --- MQ
    S -->|outbound only| KV
    S -->|outbound only| MQ
```

---

## Set it up

Three steps: say what the node is, say where the role belongs, start it.

### 1. Give the satellite a Tier A config

Only the Tier A file lives on the satellite. The company itself (roles,
prompts, providers, integrations) comes from the database, so there is
no company YAML to copy or keep in sync.

```yaml
# config.yaml, on the satellite host
node:
  id: "${CREWLET_NODE_ID}"        # distinct and stable — sat-eu-1
  roles: [seats]                  # agents only: no API, no duties
  labels:
    zone: eu                      # what a role will select on
  max_concurrent: 4               # this host runs one or two seats, not a
                                  #   company's worth — see below

store:
  path: "/var/lib/crewlet/sat-eu-1.db"   # this node's own file, not shared

stream:
  type: nats
  url: "${CREWLET_STREAM_URL}"

coordination:
  type: embedded-kv               # the fleet's replicated KV, reached over
                                  #   the same NATS cluster
```

`${VAR}` references are resolved in `node.labels` and `node.id` like
anywhere else, so an orchestrator injects both from the environment
without templating the file. There is a `--roles` flag for the same
reason; labels have no flag, because `${VAR}` already covers it.

`api.port` can stay set — a node without the `ingress` role does not
bind it, and logs `embedded_api_not_started` saying why. That means one
config file works for both shapes.

### 2. Pin the role

In the company config (Tier B), on the role that has to run there:

```yaml
roles:
  - name: EU Support
    handle: eu-support
    goal: "Answer in-zone customer requests"
    placement:
      labels: {zone: eu}
```

`labels` requires **every** pair to be present and equal on the node.
`node: sat-eu-1` pins to one node id instead. Give both and both must
hold — a placement only ever narrows.

Prefer a **label** over a node id. A label is a statement about what the
host *is* ("this one is in the EU zone"), so a replacement host with the
same label picks the seat up; a node id is a statement about which
process, and it strands the seat when that particular process is gone.

### 3. Start it

```bash
crewlet migrate                 # once, from anywhere that reaches the DB
crewlet run                     # on the satellite; roles come from the file
```

Or, if you would rather not put roles in the file:

```bash
crewlet run --roles seats
```

---

## Verify it landed

The **Fleet** screen in the dashboard is the direct answer: it reads the
lease table, so it gives the same picture from whichever node you happen
to reach. Look for the satellite in *Nodes* with its roles and labels,
and for the pinned handle in *Seat ownership* against that node.

From the logs on the satellite:

```
seat_claimed    seat=eu-support epoch=3
inbox_attached  seat=eu-support epoch=3 elapsed_ms=5.1
```

Two failures to know by sight:

- **`seats_unplaceable`** — no live node matches the selector. Usually a
  typo (`zone: EU` does not match `zone: eu`; comparison is exact) or a
  satellite that has not started. The seat is not being served, and
  every other node's sweep looks perfectly healthy, which is why this is
  logged rather than left to be noticed.
- **The label change did not take.** Labels are advertised on the node's
  presence lease, so a change takes effect one heartbeat after the
  **restart** that made it — editing the file is not enough.

---

## What a satellite still needs

It is a full engine process with roles subtracted, so be honest about
the dependency surface before choosing a host for it:

- **Outbound reach to the coordination slot and to the stream.** Seat
  leases, the activation pointer, the ledgers and the seat's inbox all
  live there. A network so restricted that neither is reachable cannot
  host a satellite. Its own store file is local, so that one costs
  nothing.
- **Whatever its LLM provider needs.** Usually outbound HTTPS to the
  provider. A network with no egress at all can still work if the role
  uses a [subscription CLI backend](../concepts/subscription-llm-backends.md)
  or an on-premises OpenAI-compatible endpoint.
- **The MCP servers its role declares, installed on that host** — the
  whole reason the seat is there.
- **The same keyring**, if company-config encryption is on. The
  satellite decrypts the company document and the
  [secret store](../concepts/secret-store.md) itself; without the
  keyring it cannot read the config at all.
- **Nothing inbound.** No port, no ingress rule, no public URL. That is
  what makes this shape workable in a zone the rest of the fleet is not
  in.

---

## What it costs

**A pinned seat is unserved whenever nothing matches it.** The engine
will not widen a selector to keep a seat running — widening it is
exactly what you asked it not to do. So when the satellite is down, that
agent is down, and the fleet says so (`seats_unplaceable`) rather than
quietly running it somewhere it does not belong. If that is not
acceptable, run **two** satellites carrying the same label: the seat
moves between them, and the fair share is computed per placement group,
so the pinned seat does not eat into anything else's capacity.

**A satellite is eligible for unpinned seats too.** It runs seats, so it
takes its share of the ones nobody pinned — which is a real property to
be deliberate about, because those seats' MCP servers may not work in a
restricted network. There is no "only take pinned seats" switch. To keep
general work off it, pin the general work to the core, which is one
label and a YAML anchor rather than a block per role:

```yaml
roles:
  - name: CEO
    handle: ceo
    goal: "Set direction"
    placement: &core {labels: {tier: core}}       # anchor, on first use

  - {name: Engineer, handle: eng, goal: "Build", placement: *core}
  - {name: Designer, handle: design, goal: "Design", placement: *core}

  - name: EU Support
    handle: eu-support
    goal: "Answer in-zone customer requests"
    placement: {labels: {zone: eu}}
```

Give the core nodes `labels: {tier: core}` and the satellite only
`{zone: eu}`, and each group stays on its own side. (The anchor has to
be declared on a real `placement` field as above — a top-level holder
key is rejected, since the company schema forbids unknown fields.)

**Two more, inherited from running a fleet at all:**

- `node.max_concurrent` is per process, so the satellite has its own ceiling.
  Size it for one agent, not for the company — the default of 32 is sized for
  a node holding a company's worth of seats, and a satellite running one seat
  wants a much smaller number.
- A rolling upgrade across a lease-protocol bump makes new nodes wait
  for old ones. Upgrade the satellite in the same rollout as the core —
  see [mixed-version fleets](../concepts/seat-ownership.md#mixed-version-fleets).

---

## Sandboxed seats

A [sandboxed](../concepts/code-sandbox.md) seat can be pinned like any
other, with three network facts the engine cannot check for you:

- The satellite **launches** the sandbox, so it must reach the sandbox
  provider (E2B cloud, or your self-hosted `domain`).
- Whichever node holds the `sandbox-waiter` duty **polls** it. On a
  satellite (`roles: [seats]`) that duty is on a core node by
  construction — so check that node's reachability too, not just the
  satellite's.
- `CREWLET_SANDBOX_OTEL_RECEIVER_URL` must name an address the *sandbox*
  can reach, which means an ingress node. It is explicit config and is
  never derived from whichever node launched the run, so a satellite
  never advertises itself by accident.

Pinning a sandboxed seat to a node that cannot reach the provider gives
you a seat that claims cleanly and fails every run. The engine knows
what a node *says it is*, never what it can talk to.

---

## See also

- [Running a Fleet](fleet.md) — the general case: node roles, fair-share
  placement, draining and rolling upgrades
- [Scaling Out](../concepts/scaling.md) — what a node is, what the fleet
  shares, and what the design does not promise
- [Seat Ownership](../concepts/seat-ownership.md) — how the lease makes
  "only this node runs that agent" true
- [Tools & MCP](tools-and-mcp.md) — declaring the MCP servers a role's
  seat will spawn wherever it runs
