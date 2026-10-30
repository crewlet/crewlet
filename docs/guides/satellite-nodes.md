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

A satellite comes in two shapes, and the difference is the `data` role:

| | `roles: [seats]` — **stateless** | `roles: [data, seats]` — **holds data** |
|---|---|---|
| Its store | Scratch: deleted at every boot, and no copy of the tracker, the knowledge base or chat | Its own full copy of the replicated estate |
| Its broker | A **leaf** of the members' — no JetStream, no replica, no vote | A cluster **member**, holding replicas (a data node on a leaf is refused) |
| Disk it needs | Its seats' memory and working rows, nothing else | The whole company's history |
| Its seats' tools | Read and write through a data node, over the broker | Read and write its own copy, through the same router |
| Replacing it | Delete it and start another — it keeps nothing | A member leaving and joining |

Which of the two its broker is — its **broker kind** — comes from its `stream`
block, never from its roles: `stream.leaf.urls` makes it a leaf, and every node
advertises the kind it runs on its presence (`crewlet fleet broker list`, or
the Broker column on **Settings › Nodes**). See
[The broker: members and leaves](fleet.md#the-broker-members-and-leaves).

**Stateless is what you want for an agent host you intend to keep small and
disposable**: it can be rebuilt from its Tier A file alone, holds no raft
state, and restarts in seconds whatever the company's size. Its price is that
its seats need a data node to answer — see
[what a satellite still needs](#what-a-satellite-still-needs).

> A satellite is a normal `crewlet run` with roles subtracted, not a
> lighter agent-only binary. Either shape still needs to reach the fleet's
> broker. If that is not possible from where you want the agent, this is
> not the mechanism you want.

---

## What actually moves

Pinning a seat moves the whole seat, which is the point — an agent is
not a request handler you can route, it is a place where things run:

| Moves to the satellite | Stays on the core |
|---|---|
| The agent instance, its state and its turns | The HTTP API, the dashboard, every integration's webhooks |
| **Its per-role MCP servers**, spawned as child processes of the node that claimed the seat | Every [singleton duty](../concepts/seat-ownership.md#singleton-duties) — the scheduler tick, the sandbox waiter, the maintenance sweep, the identity sweep, the trim, the embeddings, the learning passes, the integration reconcile, the object store's collector, the trash reaper and Chatlet's message horizon — on whichever core node holds `workers` |
| Its LLM calls, its knowledge searches, its tool calls | Every other seat's agents and MCP servers |
| Its executor runs' launches, and a `direct` or `container` box itself, on the satellite's own host | The company config, the leases and the ledgers — all shared, in the coordination slot |

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

Whether a satellite is also a **replica** is the `data` role's answer and
nothing else's. A satellite that holds data applies every one of the
company's shared state logs (Worklet, Doclet, the
search vectors, the identity directory, every data node's usage history, the
artifacts, their datastores, the Dashboard Assistant's conversations and
Chatlet) into its own database, exactly as a core node does. A stateless one applies
none of them and asks a data node instead. See
[what a satellite holds](#what-a-satellite-holds) before you choose a host
for either.

```mermaid
flowchart LR
    subgraph core["Core network"]
        C1["node-a<br/><i>data · ingress · seats · workers</i>"]
        C2["node-b<br/><i>data · ingress · seats · workers</i>"]
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
prompts, providers, integrations) comes from the fleet, so there is no
company YAML to copy or keep in sync.

A **stateless** satellite on an embedded fleet:

```yaml
# crewlet.yaml, on the satellite host
node:
  id: "${CREWLET_NODE_ID}"        # distinct and stable — sat-eu-1
  roles: [seats]                  # agents only: no data, no API, no duties
  labels:
    zone: eu                      # what a role will select on
  max_concurrent: 4               # this host runs one or two seats, not a
                                  #   company's worth — see below

store:
  path: "/var/lib/crewlet/sat-eu-1.db"
  scratch: true                   # deleted at every boot — required without
                                  #   the data role, and it says so twice

stream:
  leaf:
    urls:                         # any member's leaf listener will do
      - "nats-leaf://node-a.internal:7422"
      - "nats-leaf://node-b.internal:7422"

coordination:
  type: embedded-kv               # the fleet's replicated KV, reached over
                                  #   the same link

secrets:                          # REQUIRED on every node, this one too
  active_key_id: k1
  keys:
    - {id: k1, material: "${CREWLET_SECRET_KEY_K1}"}   # the SAME keys
                                  #   every other node of the fleet holds
```

There is no `stream.store_dir` here, and there must not be: a leaf runs no
JetStream, so it has no stream store to keep, and Tier A refuses one beside
`stream.leaf.urls`. Every stream the satellite's seats use lives on the
members, in theirs — so the durability rule is theirs too: `crewlet validate`
refuses a member with no `stream.store_dir`, and asks nothing of a leaf's
stream.

The members open that listener with `stream.leaf.port`, and a member that
opens one must persist (`stream.store_dir`), as every member of a fleet must:
the nodes that join it keep nothing, so it keeps everything they do.

```yaml
# on node-a and node-b — alongside their existing cluster block
stream:
  store_dir: /var/lib/crewlet/stream
  leaf:
    port: 7422                    # where stateless nodes join
```

A leaf port something else already holds is refused when the member starts,
by name, as a taken route port is (see [Every node embeds a member of one
cluster](deployment.md#every-node-embeds-a-member-of-one-cluster)). NATS
does not fail when it cannot bind the listener — it logs the error and the
member never becomes ready — so the engine probes the port first:

```
leaf listener port taken: stream.leaf.port 7422 is already in use on
every interface, so no stateless node could join the fleet through this
member — free that port or give this node a different one
```

On an **external** NATS cluster there is no leaf to configure: a stateless
node dials the cluster as every node does, and keeps `store.scratch: true`
and `roles: [seats]`.

A satellite that **holds data** is the same file with `roles: [data, seats]`,
a durable store (no `scratch`) and a cluster member's `stream.cluster` block
and `stream.store_dir` instead of `stream.leaf` — see
[Running a Fleet](fleet.md). It joins the broker as a voter: mind the
[five-member ceiling](fleet.md#the-broker-members-and-leaves) before adding
one to a fleet that already has five.

The `secrets:` block is not optional on a satellite either, although it holds
no estate, serves no API and signs no session cookie. The company document it
fetches from its peers is sealed under the fleet keyring and authenticated by
that seal; every request it sends a data node's estate service is signed
under it, because the data node runs a write as the actor the request names
and the broker authenticates nothing; and every seat's executor run is handed
a per-run token signed under it. `crewlet validate` refuses a Tier A file
without it, naming `secrets.keys`; generate a key once with `crewlet secrets
keygen` and give every node the same material.

`${VAR}` references are resolved in `node.labels` and `node.id` like
anywhere else, so an orchestrator injects both from the environment
without templating the file. There is a `-roles` flag for the same
reason; labels have no flag, because `${VAR}` already covers it.

`api.port` can stay set, and one config file works for both shapes: a node
without the `ingress` role binds it for its [probes](#probe-it) —
`/health` and `/ready` — and serves the network no API, no dashboard and no
webhook on it (`api_probes_listening`). Every other path answers `404`, or
`401` for a write or an always-guarded prefix, exactly as an unknown path does
on an ingress node. A seat pinned here adds no route the network can reach
either. Its executor's box calls the seat's tools back over
[`/mcp/{token}`](../concepts/llm-backends.md#the-tool-bridge): a `direct` box,
a process of the satellite's own host, dials the satellite's `api.port` on
loopback, where the satellite serves the bridge to its own host alone — so a
satellite with no address the rest of the fleet can reach runs its `direct`
seats with nothing to set, provided `api.host` takes every interface or a
loopback address. A box in any other cell dials the fleet's bridge address —
the ingress nodes — and the ingress node a call reaches relays it to the
satellite over the broker connection the satellite already dialled, so give
the satellite the same `CREWLET_MCP_BRIDGE_URL` as every other node wherever
such a box runs. With `api.port: 0` the satellite binds nothing at all, which
is the shape for a host that must accept no connection of any kind — at the
price of no probe, and of its `direct` boxes needing that address too; its
seats' executors still reach their tools.

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

On a **stateless** satellite there is one command:

```bash
crewlet run                     # roles come from the file
```

Its store is created fresh, at the binary's own schema, every time it starts,
so there is nothing to migrate — `crewlet migrate` refuses a scratch store and
says so, and so do the offline `config` and `search eval` commands, because
anything they wrote into it would be gone at the next boot. `crewlet secrets`,
`crewlet llm`, the provisioners and every other command that resolves the
company's `${VAR}`s refuse it too, for another reason: the company's secrets
live on the fleet's store, never in a node's own file, and a command reaches
that store itself only from a node that holds data — so from this one, name a
running node that does with `-api`, with one of its Tier A tokens in
`CREWLET_API_TOKEN`. Configure the company through the API of a node that
holds data.

A satellite that **holds data** is migrated on the satellite first:

```bash
crewlet migrate                 # this host's own store file
crewlet run
```

`crewlet migrate` is **per node, not per fleet**. There is no shared database
to migrate from elsewhere: it applies the pending schema migrations to the two
local store files its Tier A config names — the node's own and the replicated
estate beside it — and every node owns its files exclusively. A satellite
without `data` has nothing to migrate: its store is scratch, and `migrate`
refuses it.

Or, if you would rather not put roles in the file:

```bash
crewlet run -roles seats
```

---

## Verify it landed

The **Settings › Nodes** screen in the dashboard is the direct answer: it reads the
lease table, so it gives the same picture from whichever node you happen
to reach. Look for the satellite in *Nodes* with its roles and labels,
and for the pinned handle in *Seat ownership* against that node.

From the logs on the satellite:

```
seat_claimed    seat=eu-support epoch=3
inbox_attached  seat=eu-support epoch=3 elapsed_ms=5.1
```

### Probe it

A satellite with `api.port` set answers the same two probes every node
does, so an orchestrator can restart one that wedged and wait for one to
join before it replaces the next:

```bash
curl -s http://sat-eu-1.internal:8000/ready
# {"ready":true,"node":"sat-eu-1","configured":true,"draining":false,"posture":"serve"}
```

`/health` is liveness, and answers `200` for as long as the process is
alive — through a drain, and through a broker link that is down — with the
node's half of the [health envelope](../reference/api-endpoints.md#probes-on-a-node-without-ingress):
its id, its roles, its posture and applied epoch, the seats it holds, and
`shutting_down`. Before that, while it boots, it answers as every starting node
does ([While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting)):
`503` with `"phase": "recovering"` until it has linked to a member of the
fleet and provisioned what it uses on it — a satellite holds no store, so that
is the whole of its recovery. The engine bounds every step of it: a minute for
the link, and five minutes for each of the bring-ups that provision what it
uses — the event streams, the coordination buckets and the object store's
bucket, each a sequence of its own — past any of which it exits. Then it
answers `200` with `"phase": "preparing"` while its company's tools come up.

`/ready` answers whether the node is **doing its work**, because a
satellite takes no traffic for a probe to steer. It is `503` until all of
these hold, naming the first that does not in `reason`:

| `reason` | What it means here |
|---|---|
| `starting` | The node has not started yet: its listener is bound and answers the boot's answers. |
| `draining` | The node is stopping: its turns finish and it claims nothing new. |
| `broker_unlinked` | Its leaf link to every member is down (or, on an external cluster, its connection is reconnecting); `detail` says which. Its seats can neither receive nor publish anything. |
| `unconfigured` | No company revision has reached it yet. |
| `shed` or `stuck` | Its [config posture](../concepts/control-plane.md#operator-surface): it cannot apply the revision its peers run. |
| `no_presence` | It does not hold its presence lease, so its peers do not count it and nothing routes to it. |
| `admission_withheld` | No data node has yet answered that a copy of the estate admits a seat — or the node cannot serve the seats it holds — so it claims nothing. This is the state a fresh satellite sits in until it is admitted, and the one it returns to while no data node is reachable. |

Two failures to know by sight:

- **`seats_unplaceable`** — no live node matches the selector, or the
  only one that does cannot serve its seats right now and has withdrawn
  (`seats_shed_unserviceable` on that node says why). Usually a typo
  (`zone: EU` does not match `zone: eu`; comparison is exact) or a
  satellite that has not started. The seat is not being served, and
  every other node's sweep looks perfectly healthy, which is why this is
  logged rather than left to be noticed.
- **The label change did not take.** Labels are advertised on the node's
  presence lease, so a change takes effect one heartbeat after the
  **restart** that made it — editing the file is not enough.

---

## What a satellite holds

Worklet, Doclet, the search vectors, the identity
directory, every data node's usage history, the artifacts, their datastores,
the Dashboard Assistant's conversations and Chatlet are [replicated state
machines](replication.md).
Each is one ordered log the whole fleet shares, and every **data** node
applies every log into its own copy of the rows — so it answers questions
about each one locally, and so it can donate a snapshot of all of them to a
data node that fell behind. There is no key that narrows it: a data node
applies every log, whatever its other roles.

**A stateless satellite holds none of it.** Its store is scratch, and every
question its seats ask about the estate is a request to a data node — the
identity directory's included. A satellite consumes inbound deliveries and
runs seats, so whose Slack member or Jira account counts as a colleague
depends on who holds each human seat and whether they are suspended; it asks
a data node that, floored on the identity log, and reads it again every
thirty seconds. Until it has read the directory once it withholds its human
seats from routing rather than route by the org chart alone, because the
chart alone would attribute a suspended person's word to their seat.

**A satellite that holds data holds all of it**, the identity directory
included: the people in your company, the addresses and logins they are
known by, the verifiers of their passwords, recovery codes and machine
tokens, their seat bindings, their grants, the invitations that enrolled
them and the sessions they are signed in with. Be clear about what that puts
on the host, and why it is not a new exposure:

- **Names and addresses are sealed** under the fleet keyring, and a
  password, a recovery code or a machine token is stored only as a
  verifier. Logins, grants, seat bindings and session rows are in the
  clear.
- **The host already holds the fleet keyring**, which no node runs
  without — a stateless one included. The same keyring opens the company
  document, the company's [secret store](../concepts/secret-store.md),
  every vendor credential in it, and every person's sealed name and address.
  A satellite's host could decrypt every company secret whether or not it
  held a copy of the directory, and keeping the directory off it never took
  the directory out of that host's reach.

So choose a satellite's host as you would any node's: somewhere you would
trust with the company's secrets. If a seat needs to run somewhere you
would not, a satellite is not the mechanism — the host is a full member
of the fleet, whatever roles you give it.

Two rules follow for a satellite that holds data, and both are worth knowing
before you need them:

- **Every domain, or none.** A data node that could not start one of the
  domains refuses to start at all. It does not come up serving the ones
  that did start: a node applying half the logs serves rows derived from
  one while another's records pile up unapplied, and nothing above it can
  tell that apart from a node that is merely behind.
- **Every live data node counts for every log's trim.** A live data node
  that has published no position for a log yet is counted at position zero,
  and holds that log's floor until its row lands. A stateless satellite is
  never counted. (See the counted set in
  [Retention](retention.md#the-six-terms).)

At boot every data node logs the logs it started, whether or not it serves
HTTP — which on a `roles: [data, seats]` satellite is the one place to look,
because such a node serves no API on its port:

```
statelog_started  node=sat-eu-1 domains="[tracker vectors pages iam usage artifacts artifactdata assistant chat]"
```

---

## What a satellite still needs

It is a full engine process with roles subtracted, so be honest about
the dependency surface before choosing a host for it:

- **Outbound reach to the coordination slot and to the stream.** Seat
  leases, the activation pointer, the ledgers and the seat's inbox all
  live there. A network so restricted that neither is reachable cannot
  host a satellite. Its own store file is local, so that one costs
  nothing.
- **For a stateless satellite, a data node that answers.** Its seats' tracker,
  knowledge-base and chat tools, their searches and its tool-skill walks
  are each a request to a data node over the broker — no inbound port, the
  same link. It is admitted to claim seats only once a data node answers
  that its own copy is level with its logs, and while none answers its tools
  fail naming the estate nobody served rather than answering from nothing. What it publishes about its
  own turns is handed to a data node's event log (the audit trail cannot live
  in a store deleted at its next boot), so `GET /events` on that data node
  shows it; its seats' memory rides the compacted changelog every seat's
  memory does and is rehydrated after a restart.
- **The coding CLI of every model its seats name, installed and signed in
  on that host.** Each [`providers.llm` entry](../concepts/llm-backends.md)
  is a CLI the node runs, and it calls its own vendor over outbound HTTPS. A
  network with no egress at all can still work if the role's models are
  OpenCode entries whose model names an on-premises server.
- **The MCP servers its role declares, installed on that host** — the
  whole reason the seat is there.
- **The same keyring** every other node holds. The satellite opens the
  company document and the [secret store](../concepts/secret-store.md) with
  it and signs every request it sends the estate service; without it the
  node refuses to start — see [what a satellite holds](#what-a-satellite-holds)
  for what that means for the host.
- **Nothing inbound from the fleet or the outside world.** No ingress
  rule, no public URL: the satellite dials out to the broker and
  coordination slot and is never dialled back. That is what makes this
  shape workable in a zone the rest of the fleet is not in. What reaches
  `api.port` is the orchestrator's probes, and nothing else: a seat pinned
  here has its executor box's tool calls relayed to it over the broker by
  an ingress node, so the box never dials this node. `api.port: 0`
  closes the probes too.

---

## What it costs

**A pinned seat is unserved whenever nothing matches it.** The engine
will not widen a selector to keep a seat running — widening it is
exactly what you asked it not to do. So when the satellite is down, that
agent is down, and the fleet says so (`seats_unplaceable`) rather than
quietly running it somewhere it does not belong. If that is not
acceptable, run **two** satellites carrying the same label: the seat
moves between them, and the fair share is computed per placement group,
so the pinned seat does not eat into anything else's capacity — and
nothing else eats into the pinned seat's: a satellite's share of the
unpinned seats is a separate bound from its share of its own, so it
keeps room for the seat it exists for and claims it first. A seat whose
teardown failed on the satellite is charged against the unpinned seats
before the pinned one, for the same reason.

**A satellite is eligible for unpinned seats too.** It runs seats, so it
takes its share of the ones nobody pinned — on top of its pinned seat,
never instead of it — which is a real property to be deliberate about,
because those seats' MCP servers may not work in a restricted network.
There is no "only take pinned seats" switch. To keep general work off
it, pin the general work to the core, which is one label and a YAML
anchor rather than a block per role:

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

## Agent seats and their boxes

Every agent seat's executor runs in a [box](../concepts/code-sandbox.md),
so a seat pinned here brings three facts the engine cannot check
for you:

- The satellite **launches** the run. For an `e2b` cell it must reach the
  sandbox provider (E2B cloud, or your self-hosted `domain`); a `direct` or
  `container` cell runs on the satellite's own host, where `direct` needs
  the coding CLI installed, and `container` the container runtime and an
  image with the coding CLI in it.
- Whichever node holds the `sandbox-waiter` duty **polls** an `e2b`
  box. On a satellite (`roles: [seats]`) that duty is on a core node by
  construction — so check that node's reachability too, not just the
  satellite's. A `direct` or `container` box is the satellite's own: it
  polls, collects and reclaims it itself, with no duty, and a seat that
  moves to another node asks the satellite to read the box back (see
  [Code Sandbox](../concepts/code-sandbox.md#durability-completion-and-observability)).
- `CREWLET_SANDBOX_OTEL_RECEIVER_URL` must name an address the *sandbox*
  can reach, which means an ingress node. It is explicit config and is
  never derived from whichever node launched the run, so a satellite
  never advertises itself by accident. `CREWLET_MCP_BRIDGE_URL`, which
  every seat's executor box off the satellite's host dials, is the same: an
  ingress node's address, the same value on every node, and the ingress node
  a box's call reaches relays it to the satellite over the broker. The one
  address a satellite derives is its own listener on loopback, which only its
  own `direct` boxes are handed — for the bridge, and for telemetry where the
  satellite forwards to `OTEL_EXPORTER_OTLP_ENDPOINT` — and which it serves to
  its own host alone, so it advertises nothing to the network.

Pinning an agent seat to a node that cannot reach its executor's sandbox
provider gives you a seat that claims cleanly and fails every run. The engine
knows what a node *says it is*, never what it can talk to.

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
