# Running a Fleet

One `crewlet run` is a whole company. You probably do not need more than one.
A single engine runs many concurrent turns (agent handlers are goroutines);
the ceiling is your model plans' and keys' rate limits and host memory (every
model call is a harness process). **Scale up before you scale out.**

Run a fleet when:

- **A node's failure is not acceptable downtime.** A second node takes over a
  dead one's seats within a lease TTL (45 s); a rolling deploy hands them over
  gracefully.
- **You need to terminate traffic separately from running agents.** An ingress
  node in a DMZ, agents on a private host.
- **Some seats have to run somewhere specific.** A network zone, a filesystem
  or a GPU that only one host has.

It is not a throughput lever: `node.max_concurrent` is per process, so N nodes
is N × that ceiling. Size it per node.

**One thing divides by itself.** Above 10 000 indexed documents a fleet splits
the knowledge search's 64 buckets between its live data nodes, so each scans a
share. Nothing is configured or rebuilt; adding a data node divides the buckets
again on the next search, and an answer a node did not come back for is
labelled partial rather than silently short. See [Search](search.md).

## What a fleet needs

**Shared coordination.** Seat leases live in the `coordination` slot.
`coordination.type: local` holds them in this process, so every node would
believe it owns the whole company. It covers *only* the leases: the fleet's
shared records (token counter, completion ledger, delivery dedupe,
agent-to-agent channels, scheduled-fire claims, executors' records) are on the
KV whatever this says, because each must outlive the process. A fleet needs
`coordination.type: embedded-kv`; Tier A refuses `local` beside a clustered or
external stream by name.

Nothing else to deploy: the KV lives on the **stream's own NATS broker**, on
every topology, over the queue's own connection on an external cluster and over
a second connection to the same server on an embedded one; the node's holds
ride a lease connection of their own on both. Every one of them is watched: the
NATS client closing any for good stops the node for its supervisor to
restart.

**One stream every node can reach.** The default embedded server binds no
socket, so two nodes started side by side are two private brokers, sharing
neither stream nor coordination bucket, and nothing says so. Either:

- cluster the embedded servers: the same `stream.cluster.name` on every node,
  a `stream.cluster.port` to route on, and the other members' route URLs in
  `stream.cluster.peers`; or
- point all at an external cluster with `stream.type: nats` and `stream.url`.
  Its servers must all set `max_payload: 8MB` (events and node answers may be
  that large; company files cross in 128 KiB messages). Refused at connect, by name: a server at nats-server's 1 MiB
  default, one whose `jetstream { limits { duplicate_window } }` is below the
  dead letters' 15 minutes, and one that authenticates nobody (every node must
  present a credential it checks;
  [An external NATS server](deployment.md#an-external-nats-server)).

It is the same client code either way. The choice is strictly either/or:
`stream.cluster` configures the embedded server's own membership, so writing
one against `stream.type: nats` is refused.

**`stream.replicas: 3` on a clustered fleet.** A publish is then a quorum
commit before it returns: "published" means "survives losing this node".
**It does not cover a lost disk or site while the members share one host**:
replicas on one machine survive its process, not its hardware, and the engine
cannot tell whether your hosts are separate failure domains, so it does not
claim they are. Failure matrix: [Replication](replication.md).

Tier A refuses `replicas` above 1 when no peers are configured. Expect a boot
to pause the first time a cluster forms: a member waits for the metadata group
to elect a leader before provisioning (about eight seconds on a quiet
three-member cluster, up to sixty), because creating a replicated stream
against a leaderless group blocks rather than failing. It waits until that
leader **answers it**, not until the member believes itself caught up, since a
create sent to a leaderless group is dropped, not refused. If the other members have not
arrived, the stream cannot be placed and the node says so
(`jetstream_stream_awaiting_peers`) while it retries inside the per-create
provisioning deadline: **two minutes** on a member with peers, thirty seconds
on a solo node (the two creates differ underneath). See
[Deployment](deployment.md#a-clustered-node-is-given-longer-to-create-them).

**Credentials go through a node that is running.** The
[secret store](../concepts/secret-store.md#which-store-the-cli-writes) is on
the KV and nowhere else, so `crewlet
secrets set` against a live node reaches every node, through that node's
authenticated API: the coordination broker is inside the engine's process with
no address a command is given, and a second broker on its store directory is
refused. With this node's engine **stopped**, the command reaches the store
itself where it can: a lone member (one whose other nodes are its leaves)
starts its own broker on `stream.store_dir`, with no listener, and stops it
again; a node on an external broker dials it. A **cluster member** cannot
(started alone it never reaches quorum, and beside running peers it would
rejoin the cluster as this node for one command), so it is refused and told to
name a running member with `-api URL`, or to start its engine and re-run. Rotate on a live fleet through a running node: that write is stamped with
the authenticating credential and logged on the node; a direct one records only
the shell's user.
`-secret-store` on a provisioner follows the same rule, and both take
`-api URL`. The process environment remains the fallback every node resolves
from, so a platform secret projected as env still works.

**Every `seats` node runs its seats' models, so every one needs their harnesses.**
A [`providers.llm` entry](../concepts/llm-backends.md) is a harness the node
holding the seat starts, and its login lives in a state directory on that
node's own disk. Install each harness on every `seats` node and sign it in
there. The sign-ins that reach every node at once travel through the secret
store: a headless token (`crewlet llm login <key>
-capture-token` writes it into the store every node reads), an
`auth.mode: api-key` entry's key, or a credential bundle (`crewlet llm export`,
set as `auth.credential_bundle` or `CREWLET_LLM_CLI_<KEY>_CREDENTIALS`), which
each node restores into an empty credential directory when it builds the
company. A plain `crewlet llm login` signs in only the host it ran on. A
`seats` node holds no data, and a cluster member whose engine is stopped cannot
reach the store alone, so `crewlet llm` on either names a running node that
holds data with `-api URL` (stored values are read through it, a captured token
is stored there), with one of Tier A's tokens in `CREWLET_API_TOKEN`. See
[LLM Backends § Authentication](../concepts/llm-backends.md#authentication).

**One `CREWLET_MCP_BRIDGE_URL` on every node.** Every agent seat's executor is
its harness run in a box, and the box reaches the seat's tools over the
[tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box):
it dials the fleet's bridge address, whichever node a call lands on relays it
over the broker to the node holding the seat. So give every node, `ingress`
nodes included, the same `CREWLET_MCP_BRIDGE_URL`: an `ingress` node's address,
or a load balancer in front of several, as a box reaches it. A node builds its
bridge, and serves `/mcp/{token}`, only from that variable, or, for its own
`direct` boxes, from its own listener where it runs seats **and** binds
`api.port` (on a node without `ingress`, beside its probes and on its host's
loopback alone). So an `ingress` node that runs no seats and has no variable
serves no bridge route: every box call landing on it finds nothing, and nothing
warns, since only a node that runs seats reports a cell its boxes cannot reach
(`executor_bridge_unreachable`). The one fleet needing no variable has all
seats' executors `direct` on nodes that bind `api.port`: each box dials its own
node's loopback listener, which relays a call whose seat has moved. A
`container` box always dials the variable; a loopback value reaches a
`container` box under the runtime's name for the engine host, which on Linux
needs the API to listen beyond loopback.

**One keyring on every node.** Every node holds the same `secrets.keys`:
records are signed under it, the company document is sealed under it, and an
embedded fleet's broker admits a route or leaf link only on a key derived from
it, so a node on another keyring is refused by every member it dials
([The broker: members and leaves](#the-broker-members-and-leaves)).

**And one `api.external_url` on every node that serves the API.** A browser
behind a load balancer signs in on whichever node it reaches and presents that
cookie to every other. Each node verifies it under the fleet keyring and
resolves it against its own copy of the identity directory (a node serving the
API holds `data`), which reaches it only through the identity log. So a node
that has not yet applied a sign-in serves that cookie's reads and holds its
first write for up to five seconds while it catches up, and never answers `401`
in between, which a browser would read as "signed out" and discard a good
cookie (the whole table is in
[Identity and Access](../concepts/identity-and-access.md#validation-is-three-valued-twice)).
A sign-out everywhere made on one node is refused on every other from the
moment each applies it. `api.external_url` is the address the load balancer
answers on, the same on every node: it names the cookie and is the only
`Origin` a write or live-socket handshake carrying that cookie is accepted
from.

**One node or three, never two.** Two embedded-KV members have no quorum
without each other, so the fleet stops serving when either restarts, and a
rolling upgrade restarts them one at a time. Tier A refuses a two-member config
by name, counting the **stream's** members: the KV lives on the stream's
broker, so the coordination quorum *is* the stream cluster's.

**`stream.cluster.peers` lists the *other* members**: the members are this node
plus the entries in its list, which is what that rule and the one allowing at
most one copy per member count. Each entry is a route URL with a host and port
(`nats://node-b.internal:6222`); one the embedded server could not dial is
refused. An entry **recognisably this node's own route** is not counted: same host and
port as this node's route listener (`cluster.host` with `cluster.port`, when
bound to one address) or its `cluster.advertise` address (whose bare host keeps
`cluster.port`), scheme ignored, host compared case-insensitively and IPs in
canonical form. An entry repeating one already listed is not counted twice.
`crewlet validate` warns naming every entry it discounted. It **cannot**
recognise another name for this host (a DNS alias, `localhost` for an
advertised `10.0.0.11`, or any entry at all when the route listener binds every
interface and advertises nothing); such an entry is counted as a member it is
not, so a two-node fleet written that way passes as three. Leave this node out
of its own list. A list naming *only* this node is refused: a member that seeds
a cluster lists no peers.

**A distinct, stable id per node.** `node.id` in the Tier A file, or
`CREWLET_NODE_ID` (how an orchestrator injects a pod name without templating
the config). Nodes sharing an id miscount the fleet; on clustered embedded servers
NATS also rejects a route from a server whose name it knows (the id is the
member's server name). The id is *refused* rather than generated when
clustering: JetStream places replicas by server name, so a name minted at boot
makes every restart a new peer, orphaning the old one's replicas.

**The schema, applied first.** [`crewlet migrate`](../reference/cli.md#crewlet-migrate)
before starting any node.

## The broker: members and leaves

A node's **broker kind** is how its broker takes part in the fleet's. It comes
from the node's `stream` block, never from its roles:

| Broker kind | The `stream` block that makes it | What its broker does |
|---|---|---|
| `member` | embedded, with no `stream.leaf.urls` | Runs JetStream in the process, holds stream replicas, votes in the metadata group that places every stream and consumer |
| `leaf` | embedded, with `stream.leaf.urls` | Runs with JetStream off and reaches the members' across a leaf link. Holds nothing, votes in nothing |
| `client` | `stream.type: nats` | A plain client of an external cluster somebody else runs |

Every node advertises its kind on its presence lease, beside roles and labels;
**Settings › Nodes** and `crewlet fleet broker list` show it. A kind this build
does not know (a newer build's) shows as `unknown` and is counted as a member
wherever that is the safe reading.

**The broker is a fixed few members.** Three survive one member lost; five
survive two, and five is recommended for a fleet whose company runs Worklet at
scale. No stream keeps more than five copies (`stream.replicas` stops at five,
JetStream's ceiling), so a sixth member holds no copy anybody asked for and
only adds a voter every election and create waits on. `crewlet validate` warns
about a peer list naming more than four other members, and so does every start
of such a member, as `config_advisory`. **Beyond five, grow by adding leaves,
not members.**

**A member of a fleet persists.** A member that names a cluster, lists peers or
opens a leaf listener holds the fleet's streams for every node reaching it
(every seat's mailbox, every tracker, knowledge-base and chat record, every
coordination bucket), so Tier A requires `stream.store_dir` on it whatever its
roles: in memory it loses all of them at its next restart.

**Which roles pair with which broker.** A node without `data` joins an
embedded fleet as a leaf; a node with `data` is a member (or a client of an
external cluster). Three pairings are **refused**, each refusal naming both
ways out: a data node on a leaf, a broker member that holds no data, and
`ingress` or `workers` on a node without `data`. Every data node holds the
whole estate as a broker member; a node without data reaches it through one.

**The broker admits only the fleet's own nodes.** A member's route port and
leaf listener admit only a route or leaf link presenting a key derived from
`secrets.keys` (every key of the keyring accepted, the active one presented).
A node on another keyring is refused, counted in
`crewlet.broker.peer.refusals` and named by key in the members'
`broker_peer_refused` lines; a leaf a member turns away fails its boot naming
the key it presented. Keyring rotation moves the broker with no step of its
own. Nothing is encrypted: keep the ports on a private network
([The fleet's broker admits only its own nodes](deployment.md#the-fleets-broker-admits-only-its-own-nodes)).

**A capacity seal counts every broker.** Changing a log's byte ceiling restarts
the fleet into maintenance mode, and the seal proving no queued request
survived needs every data node and every broker member to acknowledge, whatever
its roles
([who has to acknowledge](retention.md#who-has-to-acknowledge)).

### A member that is gone for good

Two records say who the members are and can disagree: the presence leases, and
the metadata group's own list of voters. A member whose host died loses its
presence within a lease TTL, but the group keeps counting it in every election
and create until it is removed, so a three-member fleet that lost two has no
quorum to create anything with, and nothing on the presence side says why.

```
crewlet fleet broker list
```

puts what each node advertises beside how the group counts it, and names every
disagreement: a **dead member** (a voter no live node is, or whose node came
back as a leaf or a client), a node advertising a member the group does not
count, and a node that does not say. Once a dead member is not coming back:

```
crewlet fleet broker remove node-c -confirm node-c
```

The group counts a voter by its raft **peer id**, derived from its node id, and
a member hears another's *name* only from that server itself. So once the
survivors have restarted since the member died (a rolling upgrade does it, and
so do the restarts a capacity seal takes), none can name it: `list` shows it as
`(name unknown)` with its peer id. Removing by the node id you know still
works, since the removal goes by the id the node id hashes to; if you no longer
know which node it was, remove it by the peer id `list` shows:

```
crewlet fleet broker remove -peer 9iReXzcw -confirm 9iReXzcw
```

The node you ask forwards the removal to a live member, never the one being
removed (a member cannot see itself dropped). It is refused while
the voter's node still holds a live presence lease **as a member** (a running
member removed from the group rejoins as a voter at its next restart);
`-force` overrides that for a member wedged in a way that still renews its
lease. A node that came back as a **leaf** or **client** under the voter's name
is removed without `-force`: its broker never rejoins. The dead member is often
the group's **leader**, and only a leader answers a membership change, so the
removal asks again every second while the survivors elect another, and returns
once the member carrying it no longer counts the dead one. The whole removal is
bounded by one wait (two minutes and five seconds): nobody answering as leader
for all of it is a group without quorum, which cannot change its own
membership, so bring enough members back first; a carrying member that goes
silent ends it as an outcome nobody knows, so read `list` before asking again.
A member carries each signed removal once: it keeps the request's id in its own
store until the wait ends, so a copy replayed later removes nothing, even after
that member restarts and the voter has rejoined.
The **Broker members** panel on **Settings › Nodes** offers the same removal,
by node id or peer id.

## Node roles

Every process declares what it is willing to do. The default is all five, the
single-node deployment.

```yaml
# Tier A, per node
node:
  id: "${CREWLET_NODE_ID}"
  roles: [data, gitlet, ingress, seats, workers]   # the default; omit the key
```

| Role | What it does |
|---|---|
| `data` | Keeps the company's durable state on this node's disk: a full copy of the replicated estate (the tracker, the knowledge base, chat, the vectors, the identity directory, the usage days, the artifacts, their datastores, Ask Crewlet's conversations, the seats' executor sessions and Gitlet's repositories) and the event log. The company's files are not under it: they are in the [object store](../concepts/object-store.md), which on the default `nats` backend is a stream the broker's members keep. The one role that is a promise about the **disk** rather than about work — see [Nodes that hold no data](#nodes-that-hold-no-data). It says nothing about the broker, which is the node's [broker kind](#the-broker-members-and-leaves) |
| `gitlet` | Serves the company's git repositories, where the company sets [`code.backend: gitlet`](../concepts/gitlet.md): a disposable [cache](../concepts/gitlet.md#the-cache-on-a-gitlet-node) of them on this node's disk, sized by its Tier A [`gitlet` block](../concepts/configuration.md#gitlet), and git itself run for their clones, fetches and pushes. Needs `ingress`, whose listener its routes are served on — and so, in this release, `data` too. Does nothing for a company that hosts no code |
| `ingress` | Serves the HTTP API: webhooks from every integration, the dashboard, the REST endpoints, and the tool bridge (`/mcp/{token}`) every seat's executor reaches its tools through from its box, which relays each call to the node holding the run's seat — served where the node is given `CREWLET_MCP_BRIDGE_URL`, or, for its own `direct` boxes alone, where it runs `seats` too (see [What a fleet needs](#what-a-fleet-needs)). A node without it serves the network only its [probes](../reference/api-endpoints.md#probes-on-a-node-without-ingress) on `api.port` |
| `seats` | Claims seat leases, spawns the agents, consumes their inboxes, runs turns. Answers its own seats' executor runs' tool calls — relayed to it over the broker by whichever `ingress` node a box's call reached, or, for a `direct` box, taken on its own listener on loopback, which a node without `ingress` serves on `api.port` to its own host alone — because a bridged session lives in the process holding the seat |
| `workers` | The company-wide [singleton duties](../concepts/seat-ownership.md#singleton-duties): the scheduler tick, the maintenance sweep (retention, removed-seat mailbox retirement, and forgetting the chat read state and Home board of a human seat the company no longer has), the state-log trim, the embedding duty, the object store's collector, the identity sweep, the trash reaper, Chatlet's message horizon, the code-retention duty, the delegated decisions sweep, the launch-node watch, the integration reconcile loop, the atlas collect duty, the atlas wake duty, and the learning passes (skill clustering, curation, episode compaction, promotion) on one lease |

Roles decide what a node serves and which duties it holds. Only `data` decides
**which state logs it applies**: a data node applies all eleven (the tracker,
the vectors, the knowledge base, the identity directory, the usage history, the
artifacts, their datastores, Ask Crewlet's conversations, chat, the seats'
executor sessions and Gitlet), whatever its other roles and whether or not a
company has been published. A node without `data` applies none. So every data
node holds your people, the verifiers of their credentials and their sessions,
and decides a sign-in from its own rows with nothing cached; a seats-only
satellite holds none of it and reads who holds each seat from a data node. See
[what a satellite holds](satellite-nodes.md#what-a-satellite-holds) for what
that means for its host.

A role is subtracted from **this node, not from the company**. A fleet can end
up with a whole job done by nobody while no node's config is wrong, and every
symptom is an absence: nothing fires on a schedule, no webhook arrives, and no
node that left for good has its runs judged gone. Nothing raises. So the engine checks the shape against live
node presence and logs `fleet_role_unmanned` when a role has nobody doing it,
and `fleet_role_manned` when it comes back.

`gitlet` is the one role a company may not need: a company that does not set
`code.backend: gitlet` hosts nothing to serve, so the role is reported unmanned
only where the company hosts code. A role that stops being needed while nobody
held it (code turned off) is logged once as `fleet_role_unneeded`, never as a
Gitlet node coming back.

**Which nodes to give `gitlet`.** Every ingress node a load balancer sends `/git/`
to. A node that **lists** its roles serves git only if the list names it (a
fleet that wrote `[data, ingress, …]` before it hosted code serves none until
it does). A node that declares no roles has every role, so a fleet that never
wrote `node.roles` gives every node a cache, and each syncs each repository it
is asked for: correct, but a cold sync of every repository on every node. Route
`/git/<name>` by repository (a balancer hashing the path to a node, with the
next in line when it is down) to keep each repository warm on one or two nodes.
See [Deployment](deployment.md#gitlet) and
[Scaling Out](../concepts/scaling.md#gitlet-nodes).

A node that does not run seats is also excluded from the denominator its peers
divide seats by; counting an ingress-only node would strand the difference.

### Nodes that hold no data

A node without `data` keeps nothing that has to outlive it. Its store is
**scratch**: deleted at every boot, opened with no copy of the replicated
estate. On an embedded stream its broker joins the fleet as a **leaf** of the
members': no JetStream, no replica, no vote in any quorum. It suits a small,
disposable agent host; [Running One Agent Somewhere Else](satellite-nodes.md)
walks through one.

It can run `seats` alone. `ingress` and `workers` read and write a node's own
copy of the estate directly (the API's retention report, capacity, reanchor,
eviction and backup surfaces, the scheduler, the trim), so Tier A refuses
either without `data`, naming the field and why. The API's tracker,
knowledge-base and chat routes and the operator's MCP do not need a local copy:
they go through the router a seat's tools use, so a data node whose copy of
those is out of service answers its operator from a peer's copy. A stateless
node's seats use the same tools as a data node's, each asking a data node over
the broker:

- **Reads and writes** go to any data node (every one holds the whole estate),
  picked by the asking node, moving to the next if it does not answer or ran
  nothing. The exception is a knowledge-base or chat write whose caller named
  no operation: a repeat could not be collapsed, so it is reported as unknown
  rather than sent twice (a seat's own writes always carry one). A data node's
  own seats use the same router and are answered from its own copy
  ([how a request reaches the estate](../concepts/scaling.md#how-a-request-reaches-the-estate)).
- **Who holds each seat is read from a data node too.** Which person the
  identity directory binds to each human seat (what attributes an inbound chat
  message, and what a lead's tool resolves a colleague's login to) is an estate
  read floored on the identity log and re-read every thirty seconds. Until the
  node has read it once, its human seats are withheld from routing rather than
  routed by the org chart alone.
- **Its seats' executor sessions are recorded through a data node.** The
  engine's record of what each seat's box did (the session it leaves, how its
  turn ended), and the next launch's read of the conversation it continues, go
  to the [executor log](../concepts/conversation-sessions.md#where-a-session-lives)
  through the same router. These writes are the engine's alone; no tool or
  person can make one.
- **Its own writes are visible to its next read** on whichever data node
  answers: every request carries the furthest position the node has been told
  landed, and a data node that has not applied that far says so rather than
  answer from before it. When no data node can serve the call, the tool fails
  naming the estate nobody served.
- **Its audit trail is kept by a data node.** What it publishes about its turns
  goes to a data node's event log, where `GET /events` on that node shows it.
- **Its files go straight to the object store.** Bytes travel to and from the
  one [object store](../concepts/object-store.md) the fleet shares (the
  broker's bucket across its leaf link, or the S3 bucket directly), so it
  carries the same `store.objects` block as every other node; none are kept on
  the stateless node.
- **It is never counted as a copy.** The trim waits on data nodes' positions
  only, and the search fan-out divides its buckets between data nodes only (a
  stateless node publishes to no state log). A capacity operation asks the data
  nodes and every broker **member** to acknowledge, whatever their roles; a
  leaf's broker queues nothing, so a stateless leaf is not asked
  ([who has to acknowledge](retention.md#who-has-to-acknowledge)).

Seat admission on a stateless node asks a data node whether its copies of the
tracker, the knowledge base and chat are level with their logs, so it claims no
seat until they are. While no data node answers, its seats' tracker, knowledge
and chat tools fail saying so, and `fleet_role_unmanned` names `data` if no
live node holds it.

The members that stateless nodes join open a leaf listener
(`stream.leaf.port`) and must persist (`stream.store_dir`), as every member of a
fleet must. Every node of such a fleet runs `coordination.type:
embedded-kv`: the leases a stateless node holds are the fleet's, reached over
the same link.

### How `workers` is enforced

Two independent things decide who runs a singleton duty, and both are needed:

- **The `worker:{duty}` lease** decides *which* node among the ones asking.
  Without it, two willing nodes both run the sweep.
- **The `workers` role** decides *whether this node asks at all*. Without it, a
  node configured `roles: [seats]` competes for every duty and wins some.

A node without the role **refuses** the duty rather than abstaining from the
claim: internally "no duty gate" means "there is no fleet, so this is always
mine", so an ingress-only node that abstained would run every duty. The
refusal is silent, because the node is doing what it was told.

**A `worker:` lease is not always a duty**, and one deliberately ignores the
role. `worker:setup-provision-<kind>` is mutual exclusion around provisioning
*one integration*, taken by whoever writes at that surface (an operator's pass
from the dashboard, a tick of the reconcile loop, a disconnect's teardown). It
is work a node has been asked to do, at whichever node serves the API, so
refusing it on the role would make it impossible: on `-roles data,ingress`
every Connect would answer *"another pass for this integration is running"* and
every Disconnect *"being provisioned right now; try again in a moment"*,
permanently. It gates on the coordination store alone.

**The integration reconcile loop has a third gate**, which the other duties do
not: it declines while this node's [config posture](../concepts/control-plane.md)
is `shed` or `stuck`, since every reconciler reads the live company document
and a node the fleet has moved past would converge third-party apps to a
replaced revision. It declines *before* claiming, so its lease lapses and a
peer on the current revision takes the loop over, and it logs
`integration_reconcile_shed` on the way in: the one refusal here that is not
silent, because a stalled reconcile loop looks like a converged company.

That gate is deliberately **not** applied to every duty. Roles and posture are
decided by subsystems that do not consult each other: a peer counted healthy by
the posture rule may be ingress-only and never claim a singleton. Gate them all
and an `ingress` + `seats,workers` fleet whose worker node fails one apply ends
with *no* node running *any* duty, with `/ready` green on the survivor and
nothing logged.

### Common shapes

```yaml
# Three interchangeable nodes. Simplest fleet there is.
node: {id: "${CREWLET_NODE_ID}"}          # all roles, on each
```

```yaml
# Ingress split out: two ingress nodes behind a load balancer, three
# running the seats and the duties. Every node runs the engine, so the
# ingress nodes hold the stream and a store of their own like the rest.
# Every node, the ingress ones included, also gets the same
# CREWLET_MCP_BRIDGE_URL (the load balancer's address, as a box reaches
# it): a container box dials it, and the ingress nodes run no
# seats, so they serve the bridge only with the variable set. A seats
# node's own direct boxes dial its api.port on loopback instead.
# The ingress nodes serve git too, where the company hosts code.
node: {id: "${CREWLET_NODE_ID}", roles: [data, gitlet, ingress]}
node: {id: "${CREWLET_NODE_ID}", roles: [data, seats, workers]}
```

```yaml
# A satellite: agents only, no duties, no inbound traffic, and no data —
# a scratch store and a leaf of the members' broker (see the satellite
# guide for the rest of its file). Runs seats pinned to it, in a network
# zone the rest of the fleet is not in.
node: {id: sat-eu-1, roles: [seats], labels: {zone: eu}}
```

The last is the shape for a *single* agent that must be somewhere specific (a
host that sees an internal API, a licensed binary, a GPU) while the rest of the
company stays put. **[Running One Agent Somewhere Else](satellite-nodes.md)**
walks it end to end.

## Placement

By default any node that runs seats may hold any seat, and the fleet converges
on a fair share. `role.placement` narrows that:

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

Give both and both must hold; a placement only narrows. Labels come from
`node.labels` in each node's Tier A file, are compared exactly, and are
advertised on the presence lease, so a change takes effect one heartbeat after
the restart that made it. Only an agent seat takes a placement: a
[human seat](../concepts/humans-in-the-org.md) is never claimed, so one
carrying a placement is refused.

**A selector is held to what a node can advertise.** A selector no node could
carry is refused at company validation rather than left unserved:

| Part | Rule |
|---|---|
| `node` | A node id under `node.id`'s rule: starts with a letter or digit, then letters, digits, `.`, `_` or `-`, at most 64 characters. A `${VAR}` is **not** resolved (Tier B compares the pin as written), so write the node id itself. The published schema carries the same pattern |
| label key | 1 to 63 bytes of UTF-8, no whitespace or unprintable character. Dots, slashes and non-ASCII letters are fine (`topology.example.com/zone`). Same grammar as `node.labels` in Tier A |
| label value | At most 64 bytes of printable characters, compared exactly: interior spaces and the empty string are values; a tab, control character or line separator is refused, as in `node.labels`. Whitespace around a value is refused, not trimmed (nodes' values are trimmed at load, so `" eu"` could never match, and the difference is invisible in the file) |
| labels | At most 16, as many as a node can carry under `node.labels`: a seat is placed only on a node carrying every one |

A document with several bad keys reports every one, in the same order on every
run, on both sides of the match.

**The share is computed per placement group.** With nine seats pinned to one
node and one free, across three nodes, a fleet-wide `ceil(10/3)` would let the
pinned node take four of its nine and strand five, while every node reported a
healthy sweep. Each group's share is `ceil(group size / nodes eligible for it)`,
and a node's capacity is the sum over the groups it belongs to.

**Each share is spent on its own group only.** A node holds at most its share
*of each group*; room in one group is never room in another. A satellite
labelled for one pinned seat also matches the unpinned seats, so with two cores
beside it its capacity is 2: one for its pinned seat, one for its third of three
unpinned ones. With a single number, a satellite that swept first could fill it
with two unpinned seats and never claim the pinned one, which no other node may
run. A node over its share of a group gives the surplus back even when its total
is within capacity, and the groups with the fewest eligible nodes are claimed
first.

**A seat whose teardown failed still counts.** A node that could not prove a
seat torn down keeps renewing its lease (see
[Seat Ownership](../concepts/seat-ownership.md#establishing-a-seat-and-giving-it-back)),
and the seat counts against its capacity wherever it sits. It is charged first,
and the rest goes to the node's groups most constrained first, so the cost
falls on its unpinned seats (which go to a peer) before the pinned seat only it
may run.

**A node that cannot serve its seats steps out.** A node whose seats' calls
cannot reach the estate gives every seat back (`seats_shed_unserviceable`) and
says so on its presence row. Peers stop counting it in any share until it
recovers and take up the seats, and a seat only it matches is reported
unplaceable rather than left waiting.

**A seat no live node matches is not served.** The engine will not widen a
selector, so it logs `seats_unplaceable` with the handles and leaves them.
Expect this after a pinned node dies: the pin is a constraint, and a constraint
has a cost. The warning is computed from the same shares the claims are bounded
by, so it covers every group no live node placing seats matches. Two states
leave a share unclaimed for a while without it. A node that is not ready yet
(catching up, `seat_claims_withheld`) still counts, since it keeps serving what
it holds and takes its share once level. A node whose teardown keeps failing
holds its share down by the stuck seats, and re-raises that alarm until the
teardown succeeds or it is restarted.

**A seat that stops matching is handed back.** Narrow a selector under a node
holding the seat, or change that node's labels, and it releases the seat at the
next sweep: the in-flight turn finishes, then an eligible peer picks it up.

**A placement narrows a seat, not a node.** A satellite that runs seats is
eligible for every *unpinned* seat too and takes its share of them. If a seat
needs something only the core has, pin it to the core.

### Placement and sandboxes

Every agent seat's executor runs in a [box](../concepts/code-sandbox.md),
launched by the node holding the seat, so pinning any agent seat has these to
get right, which the engine cannot check:

- The node holding the seat **launches** the run, on its own host: `direct`
  needs the harness installed there, `container` needs the container runtime
  and an image with the harness in it.
- The run's box **dials the tool bridge**; the address depends on its cell. A
  `direct` box dials the launching node's own listener on loopback wherever
  that node binds `api.port` (on a node without `ingress`, served to that host
  alone), and `CREWLET_MCP_BRIDGE_URL` on a node with `api.port: 0`. A
  `container` box is not on the node's loopback, so it always dials
  `CREWLET_MCP_BRIDGE_URL`, the fleet's address as that box reaches it,
  whatever the node's roles. A launch with no address to dial is refused,
  naming what to set (see
  [Code Sandbox](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)).
- A box is polled, collected and reclaimed on the host of the node that
  launched it, whoever holds the seat afterwards. A node that leaves for good
  has its runs' jobs lost with it (`launch_node_gone`) once the `workers` node
  holding the `launch-node-watch` duty has found it away for
  `host_grace_seconds`, and their rounds are carried on in fresh boxes by the
  seats' next holders (see
  [Code Sandbox](../concepts/code-sandbox.md#durability-completion-and-observability)).
- `CREWLET_SANDBOX_OTEL_RECEIVER_URL` must point at an address the *sandbox*
  can reach, which means an ingress node. It is explicit config, never derived
  from the launching node, so a satellite never advertises itself by accident.
  A `direct` box needs none: a node that forwards to
  `OTEL_EXPORTER_OTLP_ENDPOINT` and binds `api.port` hands its own boxes its
  loopback listener, as for the bridge.

Pinning an agent seat to a node that cannot run its executor's cell gives a
seat that claims fine and fails every run. The engine knows
only what a node says it is, not what it can talk to.

## Draining and rolling upgrades

A node stops by draining: it stops taking new work, lets in-flight turns
finish, releases each seat as it goes idle, gives back every fleet duty once
its duty loops have finished their last tick, and exits. An executor's run that
finishes during the drain, or a person's answer to one, is not resumed here; it
is left for the seat's next holder, as is every turn the drain refuses. Peers
pick up the seats and duties. A node that is killed releases nothing: its seats
move after the lease TTL, and its duties after their own TTL (45 minutes for the
retention sweep, three hours for the learning passes).

Point load-balancer readiness at `/ready` (`503` while draining) and liveness
at `/health` (stays `200` through a drain), and give the orchestrator a
termination grace period longer than your longest turn; the engine imposes no
cutoff of its own. A node without `ingress` answers both on `api.port` too, and
its `/ready` is what a rollout should wait on before replacing the next node: it
turns `200` only once the node has linked to the broker, holds its presence
lease and, running seats, has been admitted to claim (see
[Probes on a node without ingress](../reference/api-endpoints.md#probes-on-a-node-without-ingress)).

A node coming up answers both from its first seconds: `/ready` is `503 starting` until it serves, which holds the rollout, and `/health` is `503` only
while the node recovers what it holds (the startup probe's window, sized by the
store and, on a data node of a fleet, by the join it may have to make; see
[Sizing the startup probe](deployment.md#sizing-the-startup-probe)), then `200`
while its company's tools come up, so a slow tool server never gets a starting
node restarted (see
[While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting)).

The node keeps its listener for the whole drain, so both probes answer until
the last turn has finished. A request that still reaches it meanwhile (a
webhook or config write the load balancer sent before its readiness probe
caught up) is refused with `503` and a `Retry-After`, which sends it to a peer;
reads and the dashboard keep answering. See
[During a drain](../reference/api-endpoints.md#during-a-drain).

**A data node's drain moves no files.** The company's files are in the
[object store](../concepts/object-store.md), not on any one node. On `nats` the
objects are a stream at `stream.replicas` copies on the broker's members, with
the same quorum arithmetic as the logs (three members at three replicas keep
writing files with one down; two members at two replicas cannot write with
either down), and a returning member is caught up by the broker. On `s3` no node
holds an object. So between one data node and the next, wait for what the logs
need and nothing more.

### Removing a data node for good

There is nothing to drain for the files. On `nats` the node is a broker member:
stop it, then remove it from the broker's membership as
[A member that is gone for good](#a-member-that-is-gone-for-good) describes, and
the broker re-places its copies of every stream (the files' included) on the
remaining members, where there are enough. On `s3` it held no object. See
[Taking a data node away](../concepts/object-store.md#taking-a-data-node-away).

**Upgrade one node at a time, and let each one finish.** Leases carry a
protocol version, and a node refuses to claim seats while any live presence or
seat lease is held at a lower one. The current version is **1**. The rule is
asymmetric on purpose: lower-protocol nodes keep working, higher ones wait,
visibly, with `seat_claims_blocked_by_older_protocol`, until the last lower
lease lapses or is released. A rolling deploy converges that way.

- **A stalled rollout stalls placement.** If one old node stays running, the
  new ones hold nothing. The log line says so.
- **An old node that crashes holds nothing up for long.** Its presence and seat
  leases lapse within a lease TTL, and the duties it held do not count, though
  they stay live for up to three hours. See
  [Mixed-version fleets](../concepts/seat-ownership.md#mixed-version-fleets).
- **Rolling *back* across a protocol bump needs a full stop.** The check only
  looks down, so a lower-protocol build is never refused and takes over a higher
  node's expired leases unchecked.
- **A company revision an older node cannot run leaves it running.** A revision
  written on a newer node may carry something the older build refuses. The older
  node reports the refusal (`config_status: error`) and keeps running the
  revision it served before. A node starts on the last revision its own engine
  applied, never one it refused, so restarting it (onto the new build or not)
  brings it back on that one, and once it runs a build that takes the fleet's
  revision, it applies it. While peers on the newer build run the revision, the
  older node sheds (refuses new work and fails `/ready`, so its work goes to
  them), and once its three attempts are spent it reports `stuck` and stops
  retrying (see
  [Posture](../concepts/control-plane.md#posture-what-a-lagging-node-does)).
  Every config read and write through it answers the fleet's revision like any
  other node's. **Settings › Nodes** counts it as behind on config and names the
  revision it runs, which [`GET /fleet`](../reference/api-endpoints.md#get-fleet)
  gives as `config_serving_revision_id`.
- **A node that leaves takes its turn-level history with it.** Every data node's
  event store holds the events it published and those it keeps in custody for a
  node without `data`, and the dashboard's turns, traces and event log are read
  from every live data node at query time. After a data node is drained for good
  or its volume discarded, its turns, phases and events (and those it kept for a
  node without `data`) can no longer be shown on any screen; the spend, turn
  counts and page reads it recorded survive in the replicated `usage` domain.
  Export to an OTLP sink first if you need that detail. See
  [Reading the fleet's history](../concepts/event-system.md#reading-the-fleets-history).
- **Mid-rollout, a history read can name a node as speaking another
  protocol.** The history scatter carries a version, and a node on a build that
  reshaped it answers with its own version and nothing else, which the answer's
  `coverage` names rather than merging rows it cannot read.
- **Mid-rollout, a request that uses a newer field waits for a newer data
  node.** A data node refuses a request carrying a field its build does not
  declare (a write, or a read asking by a newer argument) rather than run it
  without the field; the asking node tries the next data node. Where none is on
  the newer build yet, a seat's tool or a person's request answers
  `fleet_builds_differ`; the engine's record of how a turn ended stays owed (the
  seat's mail waits on it); a coding run whose change's open uses one comes back
  with the change not opened, for its seat to open again with
  `code_open_change`; and the refusing data node logs `estate_request_refused`
  at `WARN` with `disposition=unknown_keys`, naming the keys, once per request
  refused. A request that does not use the field is served as before. **Upgrade
  the data nodes first**, so no data node refuses a request. That order costs
  something too: once a newer node writes a newer field, a node not yet upgraded
  is refused the answers it would carry into another request (a publish keeping
  a version's files, a view save and the views listed to make one, the
  catalogue, a project's fields or a datastore's schema sent back whole, a
  list's query, a Gitlet node's sync of a repository, the queue work a
  repository's home Gitlet node lands its merge queue from) and answers
  `fleet_builds_differ` for those until upgraded too, while such a home's merge
  queues wait, losing nothing. See
  [A build reads what it is asked whole](../concepts/scaling.md#a-build-reads-what-it-is-asked-whole-or-runs-none-of-it).
- **A roll finishes within the hour, or its wakes are dead-lettered.** Every
  node pulls each domain's wake feed from one durable consumer, so a node on the
  older build that meets a record the newer build wrote (or a node not yet
  restarted onto a key a keyring rotation added) hands it back for a peer that
  can read it, waiting two to thirty seconds between attempts, rather than
  acknowledge it for the whole fleet (`crewlet.tracker.feed.deferred` counts
  each). A record no node has read within an hour of being stored is kept whole,
  its sealed frame inside, as a dead letter in `CREWLET_DLQ`, and let go, and
  `feed_dead_lettered` is raised from the first one. The first node whose feed
  starts able to read an entry replays it, derives its wake once and drops it,
  so a stalled roll is finished by restarting the remaining nodes onto the new
  build.
  [`crewlet feeds dead-letters`](../reference/cli.md#crewlet-feeds-dead-letters)
  lists every entry by id, with its record, why no node read it and how long it
  has left. An entry no node will ever read (a record sealed under a key retired
  from every node, one no build translates) is abandoned with
  [`crewlet feeds discard <id> -confirm <id>`](../reference/cli.md#crewlet-feeds-discard),
  which loses its wake for good
  (`crewlet.tracker.feed.dead_letter_discarded`). An entry nothing replays or
  discards is kept for `stream.event_retention_hours` and then expires, and its
  wake with it (`crewlet.tracker.feed.dead_letter_expired`). See
  [Dead letters](../concepts/event-system.md#dead-letters).
- **An older node hands a newer record's wake back to the fleet, and a node of
  the newer build delivers what the fleet did not.** Every node takes the
  notification service's deliveries from one consumer group, `notify-inbound`,
  so a wake whose record the newer build wrote can reach an older node, which
  hands it back rather than acknowledge it for the whole fleet
  (`inbound_deferred` and `delivery_passed`, both at `INFO`). So does a
  delivery from a first-party source the newer build added (a domain given a
  wake feed, say) that the older node cannot parse (`reason=newer_source`); a
  vendor's webhook nothing parses is acknowledged as before. Handed-back
  deliveries return on the redelivery backoff (a second doubling to thirty) for
  the group's 25 deliveries, about ten minutes, and nothing steers them to a
  newer node, so early in a roll every one can go to an older node.
  - A delivery no node took by then is dead-lettered with its copy among the
    group's unhandled deliveries,
    `dlq.crewlet.notifications.inbound.notify-inbound.unhandled.{digest}`
    (`dead_lettered` at `ERROR`, with `returned=pass` and
    `reason=newer_record`). So is one whose last delivery a node that sheds or
    stops handed back after the passes, or reached a newer node that could not
    publish its wake (its broker did not answer, or its stop ended the
    handler): that node's failure, never the wake's (`handler_failed` with
    `fault=node`).
  - Every node of the newer build replays that subject: a running one within
    ten seconds of the keep, a starting one at its start. It delivers each
    copy's wake and drops the copy (`inbound_dead_letters_replayed`), so a roll
    longer than ten minutes loses no wake to its length.
  - A copy whose keep fails (refused, or cut by a node that stopped or was
    killed mid-crossing) is kept by the message's next delivery
    (`dead_letter_failed` at `WARN`), since the broker delivers a message five
    times past its budget for that. Two losses remain: a copy whose keep fails
    at every one of those deliveries (`dead_letter_failed` at `ERROR`), and one
    no node of a build that reads it runs to replay before the retention lets
    it go.
  - Each waiting copy holds one of the group's acknowledgement slots for up to
    ten minutes, out of the broker's 1000, so only a sustained 1.7 such wakes a
    second early in a roll would delay the group's other mail.
  - While any such wake waits, `inbound_dead_lettered` is raised on every data
    node, naming how many wait and how soon the first expires: one that stays
    is one no running node reads, so finish the roll, or start a node of the
    newer build if it has paused, and it is delivered. One that expires first is
    a lost wake (`crewlet.notify.inbound.dead_letter_expired`).
  - `crewlet feeds
    dead-letters` does not list these: they are the notification service's,
    not a wake feed's.

  See [Delivery semantics](../concepts/event-system.md#delivery-semantics) and
  [Dead letters](../concepts/event-system.md#dead-letters).

**Adding a state-log domain is a coordinated upgrade**, for the same reason as
the seat-protocol rule: the fleet briefly runs two builds that disagree about
what a node must hold.

A snapshot artefact's manifest names a position per domain, and a joiner
refuses one that names no position for a domain **it runs**. So on the day a
domain is added, every artefact already on disk is unadoptable by the new
build. A node whose newest artefact is short of a domain it runs takes a
complete one immediately instead of waiting for the interval
(`statelog_snapshot_recent_but_short`), and until it has one it reports itself
as holding **no** snapshot at all (not even for the domains the old artefact
covers), so the trim stops counting it as a donor and keeps the records a
joiner would still need.

So **upgrade every node before letting any node fall below the trim floor**:
the ordinary rolling deploy plus one check. Watch **Settings › Nodes** until
every data node reports a snapshot again before restoring, rebuilding or adding
nodes.

## Watching a fleet

- **`fleet_role_unmanned`**: a job nobody is doing. Fix the roles. For
  `gitlet`, said only where the company sets `code.backend: gitlet`;
  `fleet_role_unneeded` is the role no longer being needed.
- **`seats_unplaceable`**: a seat nobody may run. Fix the selector, start a node
  that matches, or clear what made the only matching node withdraw
  (`seats_shed_unserviceable` on that node).
- **`seat_claims_blocked_by_older_protocol`**: an unfinished upgrade.
- **`objects_missing`**: the object store's collector audited the company's
  files and found some whose bytes the store does not hold, or holds at the
  wrong size or digest, so they cannot be downloaded. Check the store's health
  and restore them from a backup, or upload them again; `crewlet
  objects status` names them.
- **`history_partial`**: history reads are coming back without a node that did
  not answer inside the fleet read budget. Every such answer names the node in
  its `coverage`.
- **`clock_skew`**: a clock has left the fleet's by more than an hour. The
  fleet's clock is the median of every data node's, which each node reads off
  the position register on its heartbeat, so read the alarm across nodes: their
  rows on `GET /fleet` (`alarms`), and each node's detail on its `alarm_raised`
  line. A node whose detail names **this node's clock** is the machine whose
  clock moved: run NTP there. When every node raises it naming **the clock of
  the broker member leading the position register**, that member's clock moved
  and is the one to fix: on an embedded broker one of the data nodes (the one
  whose detail names its own clock as well), on an external NATS cluster the
  server leading `KV_crewlet_statelog_positions`. With two data nodes neither
  clock outvotes the other, so both are named once they stand more than two
  hours apart. What rests on the clocks is in
  [Every machine's clock](deployment.md#every-machines-clock).
- **`offline_revision_not_published`**: at warn, from the node that logs it. A
  `crewlet config import`, `activate` or `rekey` made there while it was
  stopped was built on a revision the fleet has since moved on from. It is
  withheld rather than published over the fleet's newer company; the node starts
  on it, says so on every start while it stands, and moves to the fleet's
  revision once it applies it, which it says once more. The change stays in the
  node's history under the `revision` the line names. Put up as it stands, it
  would undo what the fleet changed since, so read the change with
  `GET /config/revisions/<revision>/diff?against=<built_on>` on that node and
  make it again on the fleet's document (through the dashboard, or
  `crewlet config import -api`); `POST /config/revisions/<revision>/revert`
  there puts it up as it stands, for an operator who means that (see [What a
  booting node publishes](../concepts/control-plane.md#what-a-booting-node-publishes)).
- **`/health`** carries this node's seats, in-flight count and config posture;
  the dashboard's **Settings › Nodes** screen puts every node's side by side,
  with seat ownership, the config epoch each last attempted and the revision
  each runs. It counts as behind on config every node that does not run the
  activated revision: one on an older epoch, one that refused it or applied it
  only in part, one on another revision, and one that has reported nothing.
- **Each node's status record** carries how each of its MCP servers started
  (one row per server, counting the instances that started and failed, with one
  failure's reason) beside its models, its runners and what it runs on. See
  [Its status record](../concepts/coordination.md#its-status-record).
- **What is firing on each node** rides its heartbeat: every
  [alarm](../reference/alarms.md) the node's own table has up, by kind and with
  when it was raised, longest-standing first. `GET /fleet` lists them on each
  node's row (`alarms`), so one read says what is wrong anywhere, where
  `/health` counts only the alarms of the node that answered. A node that
  evaluated its table and found nothing lists none (`[]`); a node with no table
  to evaluate (any node without `data`, or one whose first evaluation has not
  run yet) carries no `alarms` at all, which reads as not saying, not as
  healthy. Read the measurement and remedy on that node's own `alarm_raised`
  line.

A node whose reported config epoch lags the fleet's is not an error on its own;
every rollout produces lag. See [the control plane](../concepts/control-plane.md)
for when lag becomes a posture change. A node that reports the fleet's epoch
with `config_status:
error` has refused the revision rather than lagged it: it runs the revision its
`config_serving_revision_id` names, and
[Reading a stuck node](../concepts/control-plane.md#reading-a-stuck-node) tells
the revision's fault from the node's.
### The health tools

Two read-only tools give an assistant or agent the same view:

| Tool | What it answers |
|---|---|
| `fleet_health` | Every live node as its heartbeat states it (roles, broker participation, running turns, draining, config posture, seats held) and every alarm its own table has firing, by kind and since, longest first. A node with no table it evaluated says `alarms_evaluated: false`, never an empty list. Nodes by id, paged (`limit` 1 to 100, 25 by default; `next` back as `cursor`), a page cut whole-row where it would outgrow one tool answer. `firing`: only nodes with an alarm firing, plus a count of nodes that cannot say (`unevaluated`) instead of their rows. `node`: that one node. |
| `code_status` | The Gitlet nodes, each as it states itself: cache bytes against budget, repositories held, copies evicted and repaired over the last hour and day, and the fleet's hottest repositories (each node's own count served and its copy there; the hottest twenty of the hour and of the day). `repo`: one repository on every Gitlet node. Paged by node id as `fleet_health` is (`limit` 1 to 100, 25 by default; `next` back as `cursor`); the hot list rides the first page alone; with `node`, the answer is that Gitlet node with its own hot list. A page asks only its own Gitlet nodes (read off the presence leases) and waits only for them; the first page asks every Gitlet node, for the hot list. A node that does not answer holds up only the pages that list it. A page's `coverage` names each of its nodes that did not answer inside the fleet read budget; `complete` says whether every node it lists answered (on the first page, every Gitlet node). A fleet whose presence cannot be read is refused `unavailable`, as `fleet_health` refuses it. Offered where the company hosts code. |

**They set no thresholds.** What is wrong is the [alarm table's](../reference/alarms.md) to say: `fleet_health` reports each node's alarms as the node raised them, beside its heartbeat facts, and judges nothing else (many turns or draining with nothing firing is reported with nothing firing). `code_status` reports what nodes counted; a cache past its budget is two numbers, never a flag.

**Who may read them.** A person holding `admin` reads both through their own assistant, as they read `GET /fleet`. An agent seat reads them only where its role carries the grant, in the company document:

```yaml
roles:
  - name: SRE
    handle: sre
    health_tools: true   # fleet_health and code_status, an administrator's reads
```

Every seat's registry holds both tools, but the authority table refuses a seat whose role lacks the grant before anything is named, and the same decision leaves them off its surfaces. Setting or clearing `health_tools` takes `admin`, even inside a team a person leads: a lead who could give a seat the administrator's view would read it through that seat's trail. The grant opens the deployment's view, never a repository: `code_status` holds a seat to its own [repository grant](../concepts/gitlet.md). Its answer leaves out repositories the seat may not read and counts those hotter than the list's cut (`withheld`); a repository named with `repo` that the seat may not read is refused, as the code tools refuse it. The list is judged hottest first and only as far as its cut, so a call reads the twenty of each window it is answered plus the hotter ones it may not read, however many Gitlet nodes there are; only the first page reads the list. The first read that fails answers the call `unavailable` at once.

**Who they are read for.** In a turn somebody woke, a granted seat reads the deployment for them, and both tools hold it to the company:

- A colleague seat or person of the company is answered the seat's whole view; they need not hold `admin`.
- A turn woken by somebody the company does not know (a chat sender no seat or person maps to), by a person who may not act in the company now, or by a credential acting as no seat (a pipeline's token or service account, whose reach is the apps its permissions name) is answered nothing, so a seat never relays the deployment outside the company or past a token's pin.
- `fleet_health` decides on `fleet.health.read`, which everybody of the company who may act holds. `code_status` decides on Gitlet's read verb, which takes the Gitlet permission: a turn a person without Gitlet woke gets nothing from `code_status` and the seat's whole view from `fleet_health`.
- `code_status`'s repository reads are code reads made for that somebody ([Reads made for somebody else](../concepts/gitlet.md#reads-made-for-somebody-else)), so in a colleague seat's turn it also leaves out what the colleague may not read.
- An alarm's wake is nobody's errand: in the turn it starts, both tools read on the seat's own grant.

**A granted seat is also woken by the alarms.** Each time an alarm fires or clears on a node, or a node's first evaluation since start finds one firing, every agent seat whose role carries `health_tools` is notified once, with the alarm, node, what was measured and the table's remedy, and is sent to `fleet_health` with `firing: true` before it acts (the notification is one change on one node; only the heartbeats say what is firing now). One alarm is one conversation, so copies pending when a seat's turn starts are one turn. With `notification_coalesce_window_seconds` at its default `0`, one fleet-wide condition raised by every data node costs each granted seat two turns (the first copy's, then one for copies that arrived during it); a window of ten seconds, the alarm interval, makes it one. Nothing holds a transition back, so an alarm that fires and clears more slowly than a turn runs wakes every granted seat once per fire and once per clear while it flaps. A seat may look and do nothing. See [Alarm wakes](../concepts/event-system.md#alarm-wakes) for how a transition reaches the seats, and why a node that dies mid-alarm loses at most a notification, never the state `fleet_health` reads.

## How a node that fell behind catches up

Worklet, Doclet's pages and embeddings, the identity directory, the artifacts and their datastores, Ask Crewlet's conversations, chat, the usage history, the seats' executor sessions and Gitlet's repositories are each derived on every data node from an ordered log the fleet shares. A node replays each log from where its rows say it stopped, which works only while the log still **holds** those records. A record is trimmed once every data node has applied past it, a backup covers it and it is at least a week old.

A node down long enough, or never run, can wake below what the log holds; waiting produces nothing to replay. It asks the fleet for a **snapshot** (a copy of another member's replicated estate), verifies it against its own requirements and a checksum, and installs it wholesale before anything reads from it. This is automatic, at boot, before the node serves anything, inside its recovery, so an orchestrator's startup probe must allow for it: see [Sizing the startup probe](deployment.md#sizing-the-startup-probe), which sizes it by `stream.tracker_retention.rejoin_window`. The transfer logs `statelog_join_progress` every ten seconds. A fleet with one data node never transfers anything: nobody can donate.

Three things make that work, all per data node (a node without `data` holds no copy to fall behind with):

- **Every data node publishes its own position**, every ten seconds and at once when it takes a snapshot. The trim reads it; a node that publishes nothing is invisible, and the log is trimmed past records it still needs.
- **Every data node takes snapshots**, into `store.snapshot_dir` (default `snapshots/` beside the store file), no more often than `stream.tracker_retention.snapshot_interval` (default 24h). It declines while catching up, holding a record it cannot decode, short of disk, or counting no other data node, and retries shortly rather than waiting out the interval.
- **Every data node serves them.** There is no designated donor: one that was down would leave nothing to give.

### Who can donate to whom

**Any data node to any other.** Every data node runs every state-log domain whatever its other roles, so a node holding `data` alone is as good a donor as one running every role. A node without `data` neither donates nor adopts.

The one refusal is a **donor that runs fewer domains than the recipient**: with every data node running all of them, a donor on an older build during a rolling upgrade. It is refused from its manifest alone, before any byte moves, because an artefact naming no position for a domain the recipient runs would leave the recipient believing it was caught up, with a checkpoint and no rows. The joiner moves to the next offer, logging `statelog_adoption_refused` naming the donor, and only if **every** offer was unusable says so, with `statelog_no_snapshot_offered`.

A node that found no usable offer does not stop. It comes up on the history it has, its reads report the coverage they could not account for, the domains that gate seat admission keep it from claiming work it cannot answer for, and it asks again on a widening interval.

A refusal is only about the **offer** (its transfer, bytes, schema, positions, or a trim that passed it in flight). Two other endings are never logged against a donor:

- **A fault of the joining node's own** before anything is installed: its disk refusing the copy, its store refusing a write, the coordination store refusing the pin on the log's tail. Every other offer would meet the same fault, so the join ends, logging `statelog_join_unable` at error with the fault named. Nothing was installed and the database is untouched, so the node carries on as when nobody could donate: a booting node comes up on the history it has, and a node behind keeps asking on its widening interval, meeting the same fault until what the line names is fixed.
- **Anything after the install.** Once the artefact is renamed into place it is the node's database and the join is an adoption whatever fails next; it never fetches another offer over it. If the node's record of the adoption (its `statelog_adoption` row) cannot be advanced, it logs `statelog_adoption_unrecorded` at error beside `statelog_adopted`, serves on the artefact, and the row stays incomplete. If the artefact cannot be opened, the node has no replicated database open: see `statelog_estate_lost` in [the join runbook](retention.md#the-join-runbook).

**A fleet with no successful backups eventually stops trimming**, deliberately (see [Backup and restore](backup.md)); until it trims, nothing falls below the floor and this path is never needed.

Watch for **`statelog_no_snapshot_yet`** (a warning): a node has never successfully taken a snapshot, and the line says why. A fleet where every data node logs it has no recovery path.

It is logged once per change of reason, not per retry: its appearing means the answer moved, and no repeat does not mean recovery. `crewlet retention snapshots` says what each node holds now.

It concerns the artefact the node **holds**, not what this process has taken, so a restart is silent: a node that snapshotted yesterday and restarted skips for `recent`, still holds a copy a peer can adopt, and has nothing to report.

A **single node** is not a warning. It skips for `sole_node` and says so once, at info, as **`statelog_snapshot_sole_node`**. Nothing is wrong: a solo deployment's recovery artefact is [`crewlet backup`](backup.md), and the skip ends when a second node joins.

## See also

- [Running one agent somewhere else](satellite-nodes.md): pinning one seat to a host that can reach what it needs, and what that costs
- [Scaling out](../concepts/scaling.md): what a node is, what the fleet shares, and where the constants above (the 45-second TTL, the 25-delivery budget a handoff spends one of) were measured
- [Seat ownership](../concepts/seat-ownership.md): leases, fencing, admission, takeover
- [Control plane](../concepts/control-plane.md): how a config revision reaches every node
- [Deployment](deployment.md): processes, database, broker, probes
