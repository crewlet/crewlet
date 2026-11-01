# Deployment

**Crewlet requires no infrastructure services.** The engine is one
binary: its event stream is a NATS JetStream server it embeds, and its store
is a local file it creates and owns exclusively. A single host runs a whole
company with nothing else installed.

One slot changes when a deployment outgrows one node — the stream, which
becomes either a cluster of the members the nodes already embed or a NATS
cluster somebody else runs — and this page is mostly about that path. The
coordination KV is not a second address: it lives on the stream's own
broker, deliberately, so that reaching the leases is reaching the mail. On an
embedded broker it holds a second connection to it beside the queue's, and
both are watched — either one closing for good stops the node — so a node
cannot go on holding live leases over a link that still works while the one
carrying its inbox is gone, alive to its peers and deaf to its work, for
longer than a reconnect. The store never becomes shared either:
it stays one file per node, which is why everything genuinely shared between
nodes lives in the KV instead. See [Running a Fleet](fleet.md) and
[Scaling Out](../concepts/scaling.md).

---

## The single host

**A keyring is required, on one node and on every node.** Every record on every
state log is signed under it, because a record is an instruction the next node
applies and the broker's admission vouches for a connection, never a message
(ADR-0031). An embedded fleet's broker admits only keys derived from it too
(ADR-0064). `crewlet secrets keygen` mints
one and prints the export line; a node without one refuses to start, naming the
field. Every node of a fleet needs the **same** keyring — one keyed differently
would refuse every peer's records, and every browser signed in against one
would be signed out on the next request that reached another. See
[Secret store](../concepts/secret-store.md) for what else it does and for the
zero-downtime rotation runbook — adding a key and flipping `active_key_id`
logs nobody out, and **dropping a key early is what ends live sessions**.

```yaml
# crewlet.yaml (Tier A)
secrets:
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"

stream:
  type: embedded              # a JetStream server inside this process
  store_dir: "/var/lib/crewlet/stream"   # required on a broker member: every data node keeps its state logs here

store:
  path: "/var/lib/crewlet/company.db"    # ONE file, this process only

coordination:
  type: local                 # one node holding its own seat leases
```

```bash
crewlet run -config crewlet.yaml -company company.yaml
```

That is the deployment. Point a reverse proxy at the API port for inbound
webhooks and the dashboard, and there is nothing else to operate. When the
webhooks have to come from the internet and the dashboard must not, give them
a listener of their own instead: see
[Exposing webhooks without the admin API](#exposing-webhooks-without-the-admin-api).

Give that proxy at least **two minutes** of silence in either direction on
the API port's REST and file routes. Requests to `/git/` on the same port
have a sizing of their own, which can be longer — a cold cache sync before a
request's first byte, sized by your largest repository, and three minutes for
a push from a client that asks for no side-band — so give `/git/` its own
location with what [The code host](#the-code-host) asks for. Nearly every
other request is answered in well under a second; the slowest answer a REST
or file route gives is a file upload whose object store fails or stops
answering — about a minute and a half: a minute, then thirty seconds
abandoning an upload made in parts (95 s measured against an S3 fake whose
bucket hung the abandon too) — and the node stops reading the upload's body
while it waits, so the silence runs both ways. nginx's `proxy_read_timeout`
and `proxy_send_timeout`, both sixty seconds by default, each cut that answer
to a `504`; the upload is not written either way, and the object the store
may hold is collected a day later. A store that keeps throttling an upload
holds it to the upload's own deadline — a throttle slows a request down and
is never given up
([Object store](../concepts/object-store.md#an-s3-bucket--s3)) — which a
two-minute proxy also cuts: still not written, and the person uploads again.
A node eviction or readmission ([Retention](retention.md#eviction)) may take
up to fifty seconds — twenty to judge it and thirty to write every log — and
`crewlet retention evict` and the dashboard's evict dialog both wait a minute
for its answer; a proxy set below fifty cuts those answers off with a 504.
Nothing is lost when it does — the node finishes the gesture whatever happens
to the connection, and both clients read an answer the engine did not write
as one that never arrived, keeping the operation id to finish it with — but
the operator then has to finish it to see what it did. The one route whose
work grows with what it reads — a check of a git node's copy of a repository
(`POST /code/repos/{name}/verify`), about nine seconds a gigabyte — needs no
longer timeout: it runs on the node's own time and answers within a quarter
of a second — `200` with a small copy's report, `202` for a check still
running — and a running check's outcome is read with
`GET /code/repos/{name}/verify/{id}` until it ends
([API reference](../reference/api-endpoints.md#post-codereposnameverify)).

### Every machine's clock

**Keep every machine's clock synchronised** — every node's, and every server's
of an external NATS cluster. Two of the engine's deletions judge an instant
one machine stamped by another machine's clock, and both assume every
machine's clock keeps within an hour of the fleet's. The
[object collector](../concepts/object-store.md#collection-and-audit) deletes an
object no row names only once its key, minted by the node that uploaded it,
and the store's own record of when it was written are both more than a day old
by the clock of the node running the collector; and the code host's
`code-retention` duty removes a
[retired pack's](../concepts/object-store.md#live-retired-and-derived-references) row
once the instant the broker stamped on its retirement is a day old by the
clock of the node holding the duty. Each day leaves a margin past the longest
upload or read it has to outlast — twelve hours for the collector, two hours
and thirty-nine minutes for a retired pack — and a clock running ahead of the
other by more than that deletes an object a write or a read still needs. NTP
keeps machines within milliseconds of each other; a host without it drifts,
and a virtual machine restored from a snapshot can come back hours off.

Every data node reads the fleet's clocks on its position heartbeat, every ten
seconds, off the position register in the coordination store, which it reads
on that beat anyway. Each data node's row there carries the instant the node
stamped on it and the instant the broker stored it at, so each row says how
far that node's clock stands from the one broker member leading the register,
which stores every row, and the median of them is the **fleet's clock**: the
median of every data node's. A node whose own clock stands more than an hour
from the fleet's raises [`clock_skew`](../reference/alarms.md) about itself,
and every node raises it about the broker member once that member's clock does
— on an embedded broker one of the data nodes, which then raises it about its
own clock too; on an external NATS cluster the server leading the register's
stream. So while no node raises it, every data node and that member keep
within an hour of the fleet's clock, and any two of them — every member of an
embedded broker among them — within two hours of each other, inside both
margins. With two data nodes neither clock outvotes the other, so both are
named once they stand more than two hours apart. The alarm names a clock that
has already drifted rather than keeping one from drifting, and it cannot see
the other servers of an external NATS cluster disagree with the one it reads,
so run an NTP client (chrony, systemd-timesyncd) everywhere the engine or its
broker runs. [Watching a fleet](fleet.md#watching-a-fleet) says how to read the
alarm across the nodes.

### The room the stream's volume needs

The engine keeps eight ordered logs on the stream: the tracker's mutation
log, the vector changelog, the knowledge base's log, the code host's log, the
identity estate's log, the usage log, the artifacts log and the artifact
datastore's log. Each log's byte ceiling is **reserved** on the volume holding
`stream.store_dir` when its stream is created: the embedded broker grants a
ceiling in full, up front, or refuses to create the stream at all. Its limit is
`stream.store_max_bytes` where you set one, and otherwise three quarters of
that volume's free space.

The node sizes the ceilings together to fit five eighths of what that limit
leaves once the object store's `nats` bucket (below) has its reservation. A log
derived from the disk that would be scaled below 1 GiB is **held at 1 GiB**
instead, and a log whose own default is smaller than that is held at what it
asked for, so a fresh node has a floor it cannot go below.

#### The minimum room for a fresh node

With nothing set, a node starting on an empty stream directory reserves:

| What reserves it | At its smallest | Tier A field |
|---|---|---|
| The tracker's mutation log | 1 GiB | `stream.tracker_log_max_bytes` |
| The vector changelog | 1 GiB | `stream.tracker_vectors_max_bytes` |
| The knowledge base's log | 1 GiB | `stream.pages_log_max_bytes` |
| The artifacts log | 1 GiB | `stream.artifacts_log_max_bytes` |
| The artifact datastore's log | 1 GiB | `stream.artifact_data_log_max_bytes` |
| The code host's log | 1 GiB | `stream.code_log_max_bytes` |
| The usage log (a fixed default) | 1 GiB | `stream.usage_log_max_bytes` |
| The identity estate's log (a fixed default) | 512 MiB | `stream.iam_log_max_bytes` |
| **The eight logs together** | **7.50 GiB** | |
| The object store's `nats` bucket, reserved before the logs | a quarter of the broker's limit, 2.50 GiB at the minimum | `store.objects.nats_max_bytes` |

So the broker needs a limit of at least **10.00 GiB**, which an embedded
broker sizing itself at three quarters of the volume reaches with **13.33 GiB
free** on the volume holding `stream.store_dir` (about 14.3 GB). With the
objects in S3 (`store.objects.backend: s3`) no bucket is reserved, and the
logs' 7.50 GiB alone needs 10.00 GiB free. These are the least that **boots**:
the mailboxes, the coordination buckets and a joining node's snapshot reserve
nothing and grow into whatever is left, so give a production host far more.

Below the minimum the node refuses to boot with an error naming the log it
could not reserve, the bytes it needed, the bytes the broker had left, the
Tier A field that sets that ceiling, and the least a fresh node of its
configuration reserves, split into the logs and the bucket.

**On a small host**, two settings decide the figure:

- `store.objects.nats_max_bytes` sets the bucket's reservation instead of a
  quarter of the broker's limit. At its smallest, 1 GiB, a fresh node needs a
  limit of **8.50 GiB**, which three quarters of **11.33 GiB free** reaches.
  The bucket then holds at most 1 GiB of the company's files; see
  [Object store](../concepts/object-store.md#its-byte-reservation).
- `stream.store_max_bytes` sets the broker's limit outright, rather than three
  quarters of the free space at boot. It accepts values from 6 GiB, below
  what a fresh node needs, so `crewlet validate` (and the advisories a node
  logs at every start) warn when a data node's embedded broker is given less
  than the minimum for its configuration.

The fixed-default logs' fields also go lower: `stream.iam_log_max_bytes` and
`stream.usage_log_max_bytes` accept 64 MiB. The other six fields accept no
less than 1 GiB, so setting them only moves the figure up.
- **A floor is per log, not universal.** The identity estate's and the
  usage log's fields go down to 64 MiB, far below the others, because the
  identity log grows with headcount and sign-ins, and the usage log is a census of
  node-days — about 12 MB for a ten-seat node's 181 days — and a ceiling is
  granted in full at create time, so a log at a disk-sized floor costs free
  space a node needs before it can start, for a log that will not fill it.
- **More room buys longer logs, up to a point.** Unset, the mutation log and
  the vector changelog each ask for a quarter of the free space (4..64 GiB),
  the knowledge base's log, the artifacts log and the artifact datastore's
  log each for a quarter of the mutation log's, the code log for two thirds
  of the mutation log's, the identity estate's log for a flat 512 MiB and the
  usage log for a flat 1 GiB — neither of the last two is derived from the
  disk at all, because the identity log grows with headcount and the usage
  log's size is a count of node-days rather than a rate, so more disk buys
  either nothing. They are scaled down together whenever they ask for more
  than the logs' share — five eighths of what the broker's limit leaves once
  the `nats` bucket has taken its quarter of it, about 35 % of the free space
  on a first boot — which is every volume with less than about 626 GiB
  free (with the mutation log at its 64 GiB clamp the eight ask for about
  220 GiB: 64 + 64 + 16 + 16 + 16 + 42⅔ + 1 + ½); from there up each log gets
  what it asked for.
- **The code log's ask is paid whether or not anything pushes.** Below about
  626 GiB free it comes out of the other logs' share: at 100 GiB free the
  mutation log and the vector changelog are created at 9.9 GiB each rather
  than 12.2, at 256 GiB at 25.9 rather than 32.2 — windows about a fifth
  shorter for a blocked trim — and the knowledge base's, the artifacts and
  the artifact datastore's logs by the same proportion. A company that hosts
  no code and is short of room sets `stream.code_log_max_bytes: 1073741824`
  before the first boot; the
  [replication guide](replication.md#what-the-code-logs-reservation-costs)
  has the table and what to do once the streams exist.
- **The ceilings are fixed when the streams are created.** Moving the node to
  a bigger volume, or setting `stream.tracker_log_max_bytes`,
  `stream.tracker_vectors_max_bytes`, `stream.pages_log_max_bytes`,
  `stream.code_log_max_bytes`, `stream.artifacts_log_max_bytes`,
  `stream.artifact_data_log_max_bytes`, `stream.iam_log_max_bytes` or
  `stream.usage_log_max_bytes` later, changes nothing about streams that
  already exist; `crewlet retention set-capacity` is what changes a running
  log's ceiling. A log derived from the disk that is created later — one a new
  version adds — is sized from what the existing logs' ceilings leave of that
  share, so logs that already hold all of it leave it the 1 GiB floor.

[Replication](replication.md#how-the-byte-ceilings-are-sized) has the whole
arithmetic and the refusal's text.

---

## The compose stack starts nothing

`docker-compose.yml` in a repo checkout is for the things *around* the
engine — the local integration loops, and nothing else. There is no broker
service in it, because there is nothing to run: the engine embeds its own.
Every service is behind a profile, so a bare `docker compose up` brings up
nothing at all:

```bash
cp .env.example .env                              # first time only
docker compose --profile gitlab up -d             # GitLab (code host)
docker compose --profile mattermost up -d --wait  # Mattermost (chat)
```

| Service | Port | Details |
|---------|------|---------|
| GitLab | 8929 / 2424 | `gitlab/gitlab-ee`, configured `external_url http://gitlab.local:8929` — the published port and the URL GitLab builds its own links from are deliberately the same number. 2424 is git-over-SSH and optional; HTTPS plus a token is the path the engine takes |
| Mattermost | 8065 | `${MATTERMOST_LISTEN_PORT:-8065}`. Mattermost accepts a websocket upgrade only from a browser whose `Origin` matches `MM_SERVICESETTINGS_SITEURL` exactly, host and scheme, so move the port and the site URL together or the page loads with the event stream silently dead |

---

## The stream beyond one host

The stream slot has a default and one alternative, and they differ in who
runs the broker rather than in what the engine speaks: `embedded` starts a
NATS JetStream server inside this process, `nats` dials one somebody else
runs. The client code is identical either way — one implementation, with the
connection as the only branch — so nothing above the queue can tell which is
in use, and no company config mentions it.

A single host needs neither shape below. A fleet needs exactly one of them,
because two nodes each embedding a *solo* server are two companies that
cannot see each other: the solo server binds no socket at all, which is a
security property as much as a convenience.

### Every node embeds a member of one cluster

The fleet still ships as one binary and there is still no broker to deploy.
Three nodes, each naming itself, its route port and the other two:

```yaml
# crewlet.yaml (Tier A) on the first node. The other two differ only in
# node.id and in which peers they name.
node:
  id: crewlet-1

stream:
  type: embedded
  store_dir: "/var/lib/crewlet/stream"
  cluster:
    name: crewlet                      # identical on every member
    port: 6222                         # this member's route port
    peers:                             # the others' route URLs
      - "nats://crewlet-2.internal:6222"
      - "nats://crewlet-3.internal:6222"
    host: 10.0.0.11                    # bind the route port to the private
                                       #   interface, not to all of them
  replicas: 3                          # a publish is committed by a quorum
                                       #   before Publish returns

coordination:
  type: embedded-kv                    # the leases live on the stream's own
                                       #   broker; nothing else to set

secrets:                               # the SAME keys on every member: each
  active_key_id: "2026-01"             #   one verifies what the others sign,
  keys:                                #   and the broker admits only a peer
    - id: "2026-01"                    #   presenting a key derived from them
      material: "${CREWLET_SECRET_KEY_2026_01}"
```

**Bind the route port to the network the peers are on.** `cluster.host` is the
interface the route listener binds, and leaving it unset binds every
interface. The route port admits only a member presenting a credential derived
from `secrets.keys` ([below](#the-fleets-broker-admits-only-its-own-nodes)),
but that credential crosses the network in clear once per connection — a NATS
route carries a password or a certificate, never a signature — and so does
every byte after it: the company's whole event history and every lease. On a
host with a public interface an unset `host` publishes both. Set it to the
private address, or keep the port off the public interface with a firewall; the
engine cannot tell which of a host's addresses is the private one, so it does
not guess.

**A cluster block without a name is refused, because it would do nothing.**
The embedded server takes its route port, its bind interface, its advertise
address and its peer list only from a *named* cluster, so anything under
`cluster:` with no `cluster.name` starts a solo node that binds no route
listener and forms no cluster — while every other reading of the same file
(the provisioning budgets, the topology validation) calls it clustered. Tier A
names the missing field instead.

**A route port something else already holds is refused at startup, by name.**
The broker binds its route listener last, after it has recovered its whole
store, so the engine probes the port before it starts the broker and refuses
in microseconds rather than after a recovery that may take minutes:

```
stream.cluster.port 6222 is already in use on 10.0.0.11, so this member's
route listener cannot bind and it could never form a route to a peer — free
that port or give this node a different one
```

A port taken in the instant between that probe and the bind is refused too,
as soon as the bind fails, with the operating system's own error on the end of
the same sentence. `stream.leaf.port` is probed and refused the same way.

**`cluster.advertise` is for when what a member binds is not what its peers can
dial.** Members learn about each other from the members they already have: when
node 1 accepts a route from node 2 it tells node 3 where to find node 2, and
node 3 dials that address itself. With nothing configured that address is
derived from the connection's own remote address, which is correct on a flat
network and wrong wherever the address a member is seen from is not one anybody
else can use — a container with a mapped port, a NAT, a member behind a load
balancer. There, set `advertise` to the address peers should dial (host and
port, or a bare host to keep this member's own route port):

```yaml
stream:
  cluster:
    host: 0.0.0.0                      # inside the container, bind everything
    advertise: "crewlet-1.internal:6222"  # outside it, this is the address
```

**`node.id` is this member's identity in the cluster, and it has to survive a
restart.** The engine passes it as the NATS server name, which must be unique
— a route from a server whose name the cluster already knows is rejected —
and stable, because **JetStream places replicas by server name**. A node that
comes back under a fresh name is a new peer: its old replicas are orphaned on
a member that no longer exists, and the stream sits short of quorum waiting
for a server that will never return. That is why a clustered member with no
name is refused at startup rather than given a generated one — a generated
name is unique, which is only half the requirement.

**It is the resolved id, so the environment is enough.** The name follows the
same precedence as everything else that identifies this node — `node.id` in
the file, then `CREWLET_NODE_ID`, then the default — so an orchestrator
injecting a pod name needs no `node:` block at all. Writing
`node.id: "${CREWLET_NODE_ID}"` (or `"${HOSTNAME}"`) works too, and means the
same thing: Tier A expands `${VAR}` references before it decodes. Into a text
field like this one a reference expands anywhere in the value; into a number or
a boolean — a port, `stream.replicas` — it must be the whole value, and what it
resolves to is read as if it were written there (see [Environment Variable References](../getting-started/configuration.md#environment-variable-references)).

**One node or three, never two.** Two embedded-KV members have no quorum
without each other, so the fleet stops serving the moment either restarts —
and a rolling upgrade restarts them one at a time, which makes the outage
certain rather than unlucky. Tier A refuses a two-member config by name,
counting this node and the **other** members its `stream.cluster.peers`
names: an entry recognisably this node's own route, or a repeat, is left out
of the count with a warning, and an alias of this host it cannot recognise
is counted as a member — see [Running a Fleet](fleet.md#what-a-fleet-needs).

**A fresh cluster takes seconds to form, and the engine waits it out rather
than hanging.** Accepting connections is not the same as being able to serve
JetStream: a member accepts connections as soon as it is listening, while
the metadata group takes seconds to elect a leader — measured at around eight
on a quiet three-member cluster — and until it has one, creating a replicated
stream *blocks* instead of failing.

There are therefore three waits at boot, in order, and they fail for
different reasons:

| Wait | Budget | What is happening |
|---|---|---|
| **Starting** | none — reported every 10s | The broker recovers its file store: as long as the store is large, and on a fresh one milliseconds |
| **Listeners** | 30s | The broker opens its listeners — its route listener, in a cluster — once it has started. The same budget then bounds the handshake of every connection the engine opens to its own member — the queue's, the coordination store's, a snapshot donor's — which used the NATS client's two-second default and failed a loaded host's boot against a server that was already up |
| **JetStream current** | 60s | The metadata group elects a leader and this member catches up with it |

Then placement retries for as long as the cluster answers "no suitable
peers", inside the per-create provisioning budget — **30 seconds** on a solo
node and **2 minutes** on a member with peers, because the two creates are not
the same call underneath. See
[A clustered node is given longer to create them](#a-clustered-node-is-given-longer-to-create-them)
below.

**The store recovery is waited out, not bounded.** It takes as long as the
store is large — measured at roughly a second per GiB after a clean stop and
a second and a half after a crash, on a fast local disk, and longer on a
slower one — and the store holds the company's files as well as its streams.
A budget there could only fail a boot that was working: the broker cannot
stop a recovery in progress, so a boot that gave up waited for the recovery
anyway, shut the recovered broker down, and the next boot recovered the same
store again, so a store larger than the budget could never start at all.
Instead the boot says what it is doing every ten seconds:

```
INFO queue.jetstream  embedded_broker_starting server=node-1 store_dir=/var/lib/crewlet/stream elapsed_seconds=20 detail="the embedded broker is still starting — recovering its file store, …"
```

**`/health` answers `503` until the recovery is over.** The node binds its
HTTP listener before any of this, and while it recovers what it holds —
its store, this broker, the replicated estate — `/health` answers
`503 {"status": "starting", "phase": "recovering"}`; it turns `200` the moment
the recovery ends, before anything the company configures starts (see
[While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting)).
So a node under an orchestrator needs a startup probe sized for the largest
store it may have to recover: [Sizing the startup probe](#sizing-the-startup-probe)
has the arithmetic. A liveness probe on its own counts every second of the
recovery as a failure, restarts the node partway through it, and the next boot
recovers the same store again. A broker that cannot start at all, such as one
with a store directory it cannot use or a port somebody holds, does not wait:
it [declares itself dead](#a-broker-that-declares-itself-dead) and the boot
fails at once with its reason — and so does a node whose own `api.port` or
`api.public` is taken, which now fails before the recovery rather than after
it.

**The listeners keep a budget, and only they do.** Once the broker has
started, opening its listeners is a bind each — measured at under 40 ms with
sixteen members starting at once — so thirty seconds fails a boot only when
the broker stops reporting itself ready for a reason nobody predicted, and
names that rather than hanging.

Every wait is cancellable: Ctrl-C ends it at once, unless the member is still
recovering its file store. The broker cannot stop a recovery in progress, so
the boot waits for it to finish and logs `embedded_start_cancel_waits` while
it does; a second Ctrl-C exits immediately (see
[Force stop](../concepts/agent-runtime.md#graceful-shutdown)). Every other error is returned at once: a bad subject or
a conflicting retention does not clear by waiting, and retrying would turn a
config mistake into a half-minute hang with the same message at the end.

**`store_dir` is required on every member, and the engine refuses to boot
one without it.** Empty selects an in-memory member: a restart loses that
member's replicas, and the same server holds the KV buckets carrying the
fleet's shared records — the token counters, the completion ledger, open
agent-to-agent asks, claimed scheduled fires, detached (and billed) sandbox
runs. The rule is asked of the broker, not of the roles: every **member** —
every `data` node on the embedded broker — sets `store_dir` whatever else it
does, a single node included, because an in-memory member creates every
stream it provisions in memory. A leaf and a client of an external cluster
hold no streams of their own, so both are refused a `store_dir`; see
[Satellite nodes](satellite-nodes.md).

**And the company's own records live there, whatever backends it names.**
Every data node keeps the company's **identity estate** — its people, their
credentials and sessions — on a state log, and a domain's log is a stream. With
`tracker.backend: native` or `knowledge.backend: native` (the defaults), its
work items and its pages are there too, and so are the pushes of a company
that hosts its repositories in the engine (`code.native`). An unset `store_dir` means the first restart recreates those logs
empty, and a node whose rows are ahead of a log that restarted from nothing
stops serving for good. So the engine refuses to boot without one, and
`crewlet validate` refuses it, naming `stream.store_dir`.

> This rule used to ask which backends a company ran, and a company on Jira
> and Confluence was allowed an in-memory member. That company still has
> people who sign in — so it kept its identity log in memory and lost every
> person and session on its first restart, with nothing having warned. There
> is no company the rule can correctly let past.

**One broker per `store_dir`.** An embedded member takes a lock file,
`crewlet-broker.lock`, inside its `store_dir` before its broker is built and
lets it go once the broker has stopped; a second broker on the same directory
is refused before it reads anything, naming the holder
(`pid N on HOST since T`). nats-server takes no lock of its own: it
**accepts** a second server on one directory, and two brokers writing one
directory corrupt it, with nothing said until a read meets the damage.
Whatever starts the broker holds the lock — the engine, or a `crewlet` command that reaches
the fleet's secrets itself while this node's engine is stopped
([Secret Store § Which store the CLI writes](../concepts/secret-store.md#which-store-the-cli-writes))
— so a second engine on the directory is refused, so is an engine started
while such a command holds it, and so is a command that meets an engine part
way through starting (once the engine holds its database, a command goes
through its API instead). Two engines on one host therefore each name their
own `store_dir`. The file is inside the directory rather than beside it
because a `store_dir` can be the root of its own volume, whose parent a
non-root engine on a read-only root filesystem cannot write. The kernel
releases it when its holder exits, however it exits, so there is no stale
lock to clear by hand.

**Divide `store_max_bytes` when several engines share a filesystem.** Every
stream ceiling on the embedded broker is a *reservation*: the broker refuses to
create a stream whose ceiling it cannot back, and the number it compares
against is this limit. Unset, the broker sizes itself from the free space on
`store_dir` when its JetStream comes up — three quarters of it, measured once —
which is right when it is the volume's only tenant and wrong the moment it is
not, because free space bounds the *sum* of the engines on a disk rather than
each of them. Two engines each taking what they can see over-commit the volume;
three over-commit it by half again. The failure is not a disk-full message: on
a single node it is `insufficient storage resources available`, and on a fleet
— where the limit is applied by the metadata leader placing the stream rather
than by the member creating it — it is `no suitable peers for placement,
insufficient storage`. Either way it names whichever stream that node happened
to provision last, which reads as a problem with that subsystem. So on
a host running N engines against one filesystem — a test runner, a
multi-tenant box, several companies on one machine — give each of them its own
share:

```yaml
stream:
  store_dir: "/var/lib/crewlet/acme/stream"
  store_max_bytes: 68719476736   # 64 GiB of the volume, this engine's share
```

It is measured once, at boot, on both paths: a volume that later grows or
shrinks does not move the limit, and a node that should see a resized disk is
restarted. Five eighths of whatever is in force is what the state logs'
derived ceilings may reserve between them; the other three eighths are for the
streams that reserve nothing and simply grow against it — the seats'
mailboxes, the event log, the dead-letter stream, the memory changelog and
every coordination bucket. A refusal names the ceiling that did not fit and the Tier A field
that sets it on either topology; it adds the limit in force and what is already
reserved only where this node can read them, which is the standalone one. On a
fleet the room that refused is another member's disk, and no member can read
another's.

#### The fleet's broker admits only its own nodes

Every connection to an embedded broker presents a key derived from
`secrets.keys`, and the broker admits exactly four kinds of connection
([ADR-0064](../../adr/0064-the-fleets-broker-admits-only-a-key-derived-from-secrets-keys.md)):

- a **route** from another member, presenting the route credential derived
  from a key of its keyring;
- a **leaf link** from a node without `data`, presenting the nkey derived from a
  key of its keyring and signing the member's challenge;
- an **operator** on a member's own host, on the member's loopback client
  listener, presenting the operator key `crewlet fleet broker credential`
  writes ([below](#reaching-an-embedded-fleets-broker-with-the-nats-tool));
- the node's **own connections**, in process.

Every node accepts every key of its keyring and presents the active one, so the
keyring's [rotation runbook](../concepts/secret-store.md#rotating-the-keyring-with-nothing-in-flight-lost)
moves the broker with no step of its own. A clustered member listens on two
ports: the route port on the configured interface, and a client port on
loopback that NATS opens before it routes — the engine's own connections never
dial it (they are in process), and no client is told where it is.

A refused connection is counted in `crewlet.broker.peer.refusals`, by the
listener it reached and what it presented — `none`, `unknown` (a key this node
does not hold) or `invalid` (a key it holds, with a proof its material does not
produce) — and logged as `broker_peer_refused`: a class's first refusal at once,
then at most once a minute carrying the count since, naming the key presented
and where it came from. `unknown` while a rotation is under way is a node whose
keyring differs from this one's; `none` is a process that is not a node of this
fleet. Every broker logs `broker_keys` when it starts: the key it presents, the
keys it accepts and the listeners it opened.

**What it does not do is encrypt.** `credentials`, `token` and `stream.tls` are
**dial-side** options for an external cluster; an embedded broker's links carry
no TLS. A route's credential crosses the network in clear once per connection —
a NATS route carries a password or a certificate, never a signature — and every
byte of the fleet's traffic after it: every event, every lease, every record.
Neither end of a route or a link authenticates what answers at the address it
dials either, so a route's credential goes to whatever holds a peer's address.
A private subnet, a security group or a WireGuard mesh between the nodes is
still what makes this shape safe, and no configuration substitutes for one;
what an impostor on that network could forge is bounded by the signature every
record and every instruction carries
([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)).

**A coding box on a member's host network can read that traffic.** A
[container box](../concepts/code-sandbox.md#local-sandboxes) joins the Docker
network `providers.sandbox.local.network` names (the runtime's default bridge
when unset), and from there it reaches the broker's ports like any process
outside the fleet — and is refused. Given the HOST's network instead
(`providers.sandbox.local.network: host`, or a `--network host` run argument)
on a member's host, it shares the member's network namespace: with Docker's
default capabilities it can read the host's traffic, a route's credential at
connect time included, and reach the loopback client listener, which admits
only the operator's key. On a fleet node, keep coding boxes off the host
network, or run them in `e2b`. A `direct` box is outside every network rule: it
runs as the engine's own user
([Code Sandbox → Security model](../concepts/code-sandbox.md#security-model)).

### Reaching an embedded fleet's broker with the nats tool

A few broker gestures have no engine verb — above all resizing every stream and
bucket before a raised `stream.replicas` lets a node start
([Replica count](#replica-count)) — and they are made with the
[`nats` tool](https://github.com/nats-io/natscli) on a member's own host,
through the member's loopback client listener, with the operator key:

```bash
# on a clustered member's host, with the token of a principal that holds admin
export CREWLET_API_TOKEN=...
crewlet fleet broker credential -o ./operator.nk
# Wrote the operator key derived from secrets.keys id "2026-01" to ./operator.nk (0600).
#
#   nats --server nats://127.0.0.1:41234 --nkey ./operator.nk <command>

nats --server nats://127.0.0.1:41234 --nkey ./operator.nk stream ls
```

The command derives the key from this host's own Tier A — the seed is written
to a file created `0600`, never printed, and never over a file that exists —
and asks this host's node, over `GET /fleet/broker` (which takes `admin`), for
its client listener: its port is the OS's, chosen at every start, and on
loopback it is reachable from that host alone. It refuses an answer from any
other node (a `-url` naming a balancer or a peer), and a node with no client
listener — a solo member, a member with only a leaf listener, a leaf — says so.
A member that serves no API (`api.port: 0`) is asked nothing: the invocation is
printed with the address left for you, and the node's `broker_keys` line names
it. The key reaches every stream and bucket and never the system account —
removing a member goes through
[`crewlet fleet broker remove`](../reference/cli.md#crewlet-fleet-broker) — and
it grants nothing its holder lacks: whoever reads this node's Tier A holds the
keyring, which already decrypts the company's document and secrets. It is
admitted until its key leaves `secrets.keys`; write it again after a rotation
drops it. A member serves the plain JetStream API beside the fleet's domain, so
the tool needs no `--js-domain`.

### An external NATS server

The other multi-node shape, and the one to reach for when the broker has to
be secured, operated, or shared on a schedule of its own:

```yaml
# crewlet.yaml (Tier A)
stream:
  type: nats
  url: "nats://nats-1.internal:4222,nats://nats-2.internal:4222,nats://nats-3.internal:4222"
  credentials: "/etc/crewlet/engine.creds"   # an NKey/JWT creds file — one of
  # token: "${CREWLET_NATS_TOKEN}"           #   credentials, token or tls.cert
                                             #   (or a credential in url) is
                                             #   required
  tls:
    ca: /etc/crewlet/ca.pem                  # the private CA to trust
    cert: /etc/crewlet/client.pem            # this engine's own certificate
    key: /etc/crewlet/client.key

coordination:
  type: embedded-kv

secrets:                                     # required on every node, and the
  active_key_id: "2026-01"                   #   same keys on each of them
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"
```

**The URL goes to the NATS client verbatim**, so a comma-separated list of a
cluster's members is one value as far as this config is concerned and the
client fails over between them. `store_dir` is refused here by name: it is
where an *embedded* server persists, and an external cluster keeps its own
storage. A URL may carry a credential as its userinfo — `user:password@`, or
a bare `token@`, which the client reads as a bearer token — and nothing the
engine writes repeats it: a failed dial, a refusal at boot and a log line each
name a server by its scheme, host and port alone, and an address that does
not parse is named as one rather than quoted.

**Authentication is required, on both sides.** This node must present a
credential: `credentials`, a path to a NATS credentials file (the NKey/JWT
pair a NATS account setup issues per user); `token`, a bearer token, which
takes a `${VAR}` reference so the secret stays out of the file and out of the
config revision history; `tls.cert`, a client certificate; or a credential in
any `url` entry's userinfo, which the client sends. Set whichever the broker
asks for; each is a dial option, and the engine stores none of them. A Tier A
that presents none of the four is refused at `stream.credentials`, because a
node presenting nothing reaches only a server that authenticates nobody.

And the server must authenticate its clients. Anything a NATS server admits
reads every event this company publishes and writes on every subject it rides
— a seat's mail, a lease, a state-log record (whose signature keeps a forged
one from being applied, and nothing else) — so a server that admits anybody
makes everything that can reach its port, a [container coding
box](../concepts/code-sandbox.md#local-sandboxes) on a node's host among them,
a reader and a writer of this company. The dial reads what the server says in
the `INFO` line it sends first, and admits it only where that says
`auth_required` — the server holds any authorization: a token, users, nkeys,
an operator — or `tls_required` with `tls_verify`, where every client must
present a certificate the server verifies before it says anything. Anything
else refuses the boot, naming the server by host and port:

```text
engine: stream: connect nats: jetstream: an external NATS server must authenticate its clients: the NATS server at nats://nats-1.internal:4222 requires no authentication (it admits anonymous clients, or binds them to a no_auth_user), so anything that reaches it reads and writes this company: configure authorization, or tls { verify: true } without allow_non_tls, on every server of that cluster
```

A certificate this node presents does not count on its own: a server holding
`tls { }` without `verify` never asks for one, and admits a client that
presents nothing beside it. Neither does `verify` with `allow_non_tls`, which
admits a plaintext client that presents no certificate at all. And a token the
server does not require is no authentication either — the server admits
everybody else as well — which is why the server's own word is read rather
than this node's. The check judges the server each connection the node opens
is on when its dial completes — the coordination store's and every second
connection included — and not one a connection reconnects to later, so
configure every server of the cluster alike: a client fails over to whichever
answers.

**`stream.tls` is the transport underneath that authentication**, and it is a
separate question from who you are: a broker configured `tls { verify: true }`
— the hardened default every NATS guide recommends — refuses a connection
presenting no client certificate, whatever credentials would have followed.
`ca` is the bundle the server's certificate is verified against, and empty
means the host's root pool, which is right for a public CA and wrong for the
self-signed certificate most internal estates use. `cert` and `key` are this
engine's own certificate: both or neither, because validation refuses half a
keypair rather than letting it dial and be rejected by the broker with an
error naming neither file. There is deliberately **no way to skip
verification** — that switch is set once during a bring-up and never unset,
and the connection it leaves behind carries every event this company
publishes to whoever answers on that address.

**Those files are opened before the dial, and the error names the field.** A
missing `ca` reports `tls.ca /etc/crewlet/ca.pem: no such file or directory`,
not a connection failure. Left to the NATS client, an unreadable certificate
surfaces as a dial error that reads exactly like *the broker is
unreachable* — and sends an operator to debug a network path that is fine,
for a file that is simply not there.

**A broker blip is not a node restart.** The engine dials with unlimited
reconnects and a one-second wait between attempts, and never gives up on the
URL: the coordination layer already distinguishes "unreachable" from "not
mine", and a node that keeps its seats through a two-second outage is the
entire point of that distinction. The coordination KV rides this same
connection on purpose — one connection, one fate. An outage that outlasts the
lease TTL (45 s unless `coordination.lease_ttl_seconds` says otherwise) does
hand this node's seats to a peer, and that is the intended behaviour rather
than something a reconnect policy should paper over. Each attempt gets **2
seconds** to connect, and 2 more for TLS and the NATS handshake, not the
embedded member's listener budget: the loop tries the URLs in turn, and a
member that silently drops packets holds every attempt at it for that long
before a member that answers is asked, which is time the node spends not
renewing its leases. A URL whose name resolves to several addresses — a
headless Kubernetes service naming every pod of the cluster — shares those 2
seconds among them, tried in a random order so a fleet's connections spread
across the members, rather than taking 2 seconds for each.

**A connection the client gives up on is a node restart.** Reconnecting for
ever covers a server that goes away and comes back. It does not cover the
cases where the NATS client itself closes the connection *for good* and stops
asking:

- a message larger than a `max_payload` that was lowered under the running
  node (see [below](#the-largest-message-every-server-must-carry));
- the server refusing this node's credentials, and refusing them again on the
  reconnect after it — `credentials` or `token` changed, revoked or expired,
  or the server's own authorization reloaded so it no longer admits them;
- any other error the server sends that the client does not treat as
  transient.

A node in that state has nothing to publish, consume or renew a lease over,
and it used to stay up anyway — answering its liveness probe, its seats lapsing
to peers, until somebody restarted it. It now **stops itself**: it logs
`jetstream_connection_closed_for_good` and `engine_fatal`, drains and tears
down in the order it does on `SIGTERM`, and exits with status **1**, printing on
stderr what closed, why, and what to change. The one difference from a
`SIGTERM` is the drain over a lost connection to the stream: every delivery is
acknowledged over it, so no turn still running can be, and whichever node
takes its seat runs it again — the drain cancels those turns rather than
finishing them twice, and ends at once (`drain_cut`). A node still **starting**
when it happens fails its boot the same way, exit status 1 with the same
sentence — `crewlet: engine: stream: jetstream: … closed this node's connection
for good …` — followed by `; the start failed on it at:` and the step that met
the closed connection, rather than that step's bare `nats: connection closed`.
Run it under something that
restarts a process which exits non-zero — a systemd unit with
`Restart=on-failure`, a container restart policy, a Kubernetes deployment —
and the node comes back by itself once the cause is fixed. The same applies
to the second connection the coordination store holds on an embedded broker —
whose loss alone leaves every acknowledgement landing, so that drain waits for
the running turns as `SIGTERM`'s does — and to this node's own embedded broker
going away under it: a connection to it tries
again once a second for as long as a boot gives that broker's listeners once
it has started — 30 seconds, solo or clustered — and nothing but a restart
brings a broker in this process back. It tries that member and no other: a
cluster member advertises its peers' client addresses, and a node's
connections ignore them, so a node whose own member has stopped exits for its
supervisor to restart the two together rather than running on through a peer
with its member gone and the cluster a replica short. A snapshot donor's
connection is the one exception: it serves *peers*, and a node whose donor is
down still serves its own company, so that connection closing never stops the
node — the donor [dials again](retention.md) by itself.

**The account needs more than publish and subscribe.** A node creates what it
uses, on every start and idempotently: the seven engine streams
(`CREWLET_AGENT`, `CREWLET_EVENTS`, `CREWLET_NOTIFICATIONS`,
`CREWLET_CONFIG`, `CREWLET_MEMORY`, `CREWLET_DLQ`, `CREWLET_CUSTODY`), the eight state-log
domain streams (`CREWLET_TRACKER_LOG`, `CREWLET_TRACKER_VECTORS`,
`CREWLET_PAGES_LOG`, `CREWLET_IAM_LOG`, `CREWLET_USAGE_LOG`, `CREWLET_CODE_LOG`,
`CREWLET_ARTIFACTS_LOG`, `CREWLET_ARTIFACT_DATA_LOG`), a stream per extra subject namespace a company
publishes under, one durable consumer per seat mailbox (an ordinary API
call, measured at 1.7 ms), the twenty-one `crewlet_*` KV buckets:
three in the lease store, holding the seat and presence leases, the duty
leases and the fencing epochs, and eighteen in the fleet store holding the
shared records — the object store's backend record and its collector's report
among them — and, on the default `nats` object store, the `crewlet_files`
object store bucket (its stream is `OBJ_crewlet_files`). A credential
scoped to publishing and consuming fails at boot, on the first stream it
tries to create.

**A coordination read costs one ordered pass certified against the stream's
key index, and an account needs the consumer, stream-info, message-get and
purge APIs.** A node lists coordination records constantly — several
fifteen-second duty loops on every tick, and an operator's gestures such as a
readmission or the retention report once each — but no state-log read and no
seat's write waits on a listing: the state-log write fence, which reads the
published trim floor on every write at an expectation of zero, a subject's
first write among them, reads its own domain's floor by its key, one
`$JS.API.STREAM.MSG.GET` to the `positions` stream's leader, and creates no
consumer. Each listing is one pass over a temporary
consumer, which on a replicated bucket is two metadata-raft proposals, and
beside it the stream's info narrowed to the listing's keys — one request for
each hundred thousand keys — which names every key that has a message. A pass alone cannot tell that it is complete — a
key rewritten under it can land behind the marker that ends it, and every renew
is such a rewrite — so a key the index names that the pass missed, or delivered
only as a delete marker, is read by itself from the **stream leader**
(`$JS.API.STREAM.MSG.GET`), never through the bucket's direct get, which any
replica — one that has fallen behind included — may answer: a member that had
not yet applied an acknowledged write answered from before it (see
[Coordination § Every read is the leader's](../concepts/coordination.md#every-read-is-the-leaders)).
A single key is read with that same leader request. A marker needs that
read because on a replicated bucket the pass may be served by a replica that
applied a key's delete and not yet its re-creation. On a quiet bucket the
certification therefore costs the index read and one leader read per recent
removal the pass meets, and under writes one more per key the pass lost. What
keeps "recent" recent is the maintenance duty, which sweeps the removal markers
older than three hours out of every shared bucket with no age, each with a
leader-side `$JS.API.STREAM.PURGE` bounded at the marker's own revision so a
key written again since is untouched (see
[Coordination](../concepts/coordination.md#removal-markers-are-swept)). While a
bucket's stream is **electing a leader**, a listing of it answers unavailable
rather than a short list, since every member answers the index from its own
store until one leads — the answer every caller already treats as a store
that did not answer, never as "no records". A read only a leader answers — a
stream's info, one of its messages, a consumer's info, a page of a listing — is
asked again while nobody answers it: one more request every two seconds,
whether a member that has not yet noticed its leader is gone dropped the first
or the leader is only slow, and the broker processes each one. It is asked for
as long as its caller waits — the caller's own deadline, or the client's five
seconds where it set none, as it was when it was one request — and never past
thirty seconds, fifteen requests nobody answers at most (see
[Coordination](../concepts/coordination.md#the-three-valued-answer)). An
election measured on a fleet of three cost a read about two seconds and one
more request, not its caller's whole budget. Past the broker's page size for
the key index, **100,000 keys under one filter**, the index is read a page at a
time, each page its own read and the pages overlapping, so a key removed while
it is read cannot hide another; see
[Coordination](../concepts/coordination.md#the-three-valued-answer).
The engine deliberately does **not** use the batched direct get that would
avoid the consumer: it is served by any replica, and this estate has reads
whose answer is acted on with nothing to arbitrate them. So a credential
scoped only to publishing and consuming is not enough; the account needs the
consumer API, `$JS.API.STREAM.INFO`, `$JS.API.STREAM.MSG.GET` and
`$JS.API.STREAM.PURGE` alongside the rest of `$JS.API`. If the broker's own
debug logging is on, that consumer churn is what produces a steady stream of
`JetStream connection closed: Client Closed` lines — see `stream.debug`, which
is off by default for exactly this reason.

#### A clustered node is given longer to create them

Every one of those creates is a local file-store setup on a solo node and a raft
round trip on a member of a cluster, against a metadata group whose peers are
themselves still booting — so the budget branches: **30 seconds** per create
solo, **2 minutes** clustered, with the whole coordination bring-up bounded at
**2 minutes** and **5 minutes** respectively. The flat 30 seconds these replaced was
measured failing: a fleet booting together would lose one create, and because
each object discovers a slow cluster independently the failure landed on a
different stream or bucket every time. A node that exhausts the budget fails
to start rather than running against a group it cannot reach, and the error
names the object it was creating.

**A request the broker never answers is asked again, not waited on.** A
clustered metadata request does not always come back late — sometimes the
reply is never sent at all. nats-server drops a routed request outright in
more than one ordinary situation during a bring-up: a member that is not the
metadata leader and holds no assignment for the object returns without
replying, and the routed API queue is discarded wholesale when it reaches its
limit. A node waiting on one of those is waiting for something nobody will
send, so a longer budget buys nothing — measured buying nothing, at two
minutes a time, on a different object every attempt.

So the engine now distinguishes three answers to "does this object exist",
not two: it is there, it is *not* there, and **nobody said**. The first two
are answers and are acted on; the third is silence, and silence is re-asked —
at a new leader, or past a queue that has drained. An existence probe that
goes unanswered for its whole term also simply falls through to the create,
which settles the question either way: absent and it is made, present and it
comes back as a peer having won the race. A node no longer fails to start
because it could not hear.

**A dropped stream or consumer lookup is asked again within two seconds; a
create after sixteen.** The broker decides a read — does this stream, this consumer exist —
when it processes it: it answers, or it returns without a word, and nothing
answers that request later. Every fleet booting together meets the second
case. An object another node has just asked for is *in flight*, assigned by
the metadata leader but not yet applied by the member chosen to lead it, and
until that member has applied it every other member drops a lookup of it. So
a lookup of one of the engine's streams or consumers, of a coordination
bucket, or of the object store's bucket, waits one second for its answer and
is asked again a second later — every two seconds, for up to thirty in all. A
create is the opposite case: the broker keeps its reply until the object it
made has a leader, which can take an election, so a create waits fifteen
seconds for its answer and is re-sent a second after that. A lookup used to wait the create's fifteen too, and a fresh
three-member fleet was measured idling sixteen seconds of its boot on one
dropped stream lookup.

**A create that is taking a while says so while it is happening.** Provisioning
was otherwise silent — a node opens every coordination bucket and several
streams in a row and logged nothing between them, so one that hung emitted
nothing at all until its budget expired and the log could not say which object
it was on. Any create still running after 10 seconds now writes one `WARN`
naming it (`coord_kv_bucket_slow`, `natsobj_bucket_slow`,
`jetstream_stream_slow`, `jetstream_consumer_slow`), and so does the lookup
that precedes it (`jetstream_stream_lookup_slow`,
`jetstream_consumer_lookup_slow`) — that lookup is the first call to reach the
metadata group, so a member stalled against a group that has not settled
waits there, where nothing used to report it at all. The read of each state
log's ceiling that sizes the logs before any of them is created is the same
kind of lookup and says so the same way (`statelog_ceiling_read_slow`); the
five are asked at once, so a broker that answers none of them costs one
lookup ceiling rather than five in a row. The object store's
bucket is looked up and created as one step, and writes one line over the two
(`natsobj_bucket_slow`). A probe that went unanswered is named as such
(`jetstream_stream_lookup_unanswered`, `jetstream_consumer_lookup_unanswered`,
`coord_kv_bucket_lookup_unanswered`, `natsobj_bucket_lookup_unanswered`)
rather than failing the boot. One line per object, deliberately: whether more
lines follow is what tells a slow bring-up from a wedged one.

**Every broker line names the member that emitted it.** More than one
embedded broker can run in one process — a fleet test does exactly that — and
without the name every `queue.nats.server` line from either of them was
indistinguishable, which is the one question a reader has about a fleet that
did not form. Lines carry `server=` from `stream.cluster.name`'s member
identity; a solo broker has no name to carry and the attribute is empty.

**And it needs room for the state logs.** Every state log reserves its byte
ceiling against the account's JetStream storage limit when its stream is
created, and the node sizes them to five eighths of what that limit has left.
An untiered limit counts every replica, so a `replicas: 3` fleet needs three
times the bytes; a tiered one needs its `R3` tier. An account that states no
limit leaves the node nothing to size against but its own disk, and a server's
own cap then refuses what does not fit, by name. See
[Replication](replication.md#how-the-byte-ceilings-are-sized).

#### The largest message every server must carry

**A `max_payload` of at least 8 MiB, on every server.** That is the largest
single message the engine sends — an event, a webhook delivery, a state-log
record at the largest its log declares (the tracker's, the knowledge base's,
the vector changelog's, the usage log's and the code log's are 8 MiB less
4 KiB, which is 8 MiB on the wire once the record is signed) — and the
embedded broker is configured at exactly that; nats-server's own default is
1 MiB. Set `max_payload: 8MB` in the
configuration of every server in the cluster (nats-server reads `MB` as
mebibytes, so that is exactly the 8388608 bytes the engine needs). A server
holds a connection to the **lowest** of its own `max_payload` and the payload
limits of the account the engine signs in to and of its user — an account's
`limits { max_payload }` in the server's file, or the `payload` limit in an
account's or a user's JWT on an operator-mode deployment — so where either
states one, raise it to 8 MiB or leave it unlimited too.

**A node refuses to start against a server that holds it to less.** A server
below it would take every small write and refuse the first large one — a node
that booted, reported itself healthy, served, and then refused a webhook
delivery, a long phase's telemetry or a state-log record (`record_too_large`)
past the server's limit — so the check is made when the node connects, before
anything is provisioned, and the refusal names the setting, the value the
server announced and the value this build needs:

```text
engine: stream: connect nats: jetstream: the NATS server at nats://nats-1.internal:4222 announces a max_payload of 1048576 bytes for this node's connection, below the 8388608 bytes this build needs: that is the largest message it sends — an event, a webhook delivery, a state-log record at the largest its log declares — and a server that carries less takes every smaller write and refuses the first large one. Set max_payload: 8MB in the configuration of every server stream.url reaches, and wherever the account this node signs in to, or its user, states a payload limit of its own, raise that too: a server holds a connection to the lowest of them
```

What it reads is the limit the server **announces** for this connection once
it has signed in — the account's and the user's included, which the server
states only after authenticating it — on the member the client reached at
boot. That is why the setting is needed on every server rather than the first
one a node happens to dial: a member configured apart from the others is met
later, when the client reconnects to it, and so is a server restarted with a
lower limit under a running node. The node logs
`jetstream_max_payload_below_contract` at `ERROR` on the reconnect, naming
that server and its value, and a message past that server's limit is refused
when it is written — a state-log write answers `record_too_large`, naming the
server's `max_payload`.

**Never lower it with a live reload under running nodes.** nats-server applies
a reloaded `max_payload` to the connections it already holds without telling
their clients, so a node goes on believing its server carries 8 MiB, and the
first message past the new limit makes the server close the connection with
`Maximum Payload Violation`. The NATS client treats that refusal as final and
does not reconnect, so the node
[stops itself](#an-external-nats-server) and exits with status 1, naming the
setting:

```text
crewlet: the node stopped itself: engine: stream: jetstream: the NATS server stream.url reaches (nats://nats-1.internal:4222) closed this node's connection for good (nats: Maximum Payload Violation): a message this node sent was larger than the max_payload the server now holds the connection to, a limit lowered under the running node — a live reload, which nats-server applies to the connections it holds without telling them — below the 8388608 bytes this build sends at most. Set max_payload: 8MB in the configuration of every server stream.url reaches, and wherever the account this node signs in to, or its user, states a payload limit of its own, raise that too; a node restarted against a server still below it refuses to start, naming the setting
```

Restarted against a server still below 8 MiB, it refuses to start with the
boot refusal above. So a reload that lowers the limit takes down every node
whose next large message reaches it, and they stay down until the limit is
raised again — which is the outcome to want from a broker that can no longer
carry what they send, and the reason not to lower it.

**Replication is asked for, not assumed.** `stream.replicas` is the replica
count the engine requests for each of those streams and buckets, and it
applies to an external cluster exactly as it does to an embedded one — set it
to `3` against a cluster of three or more, or the engine asks for one copy and
gets what it asked for: streams and a lease bucket that survive a process
restart but not the loss of the single server holding them.

Tier A cannot check this number for you here. It refuses `replicas` above 1
on an *embedded* stream that names no peers, because that file contradicts
itself — but `stream.url` names an address rather than a member list, so how
many servers answer behind it is yours to know. Asking for more replicas than
the cluster has members fails at boot, on the first stream the engine tries to
create.

**On an external cluster `stream.replicas` also picks which storage limit the
engine is held to.** There is no `store_max_bytes` on that topology — the limit
is the *account's*, and the engine reads it back — and a NATS account states
that limit in one of two shapes, never both. An ordinary account has a single
limit, which the server charges `replicas × ceiling` against, so the engine
divides it by `stream.replicas` before it sizes anything. A **tiered** account
states one limit per replica class (`R1`, `R3`, `R5`, …), already counting
replication, so the engine takes the tier for `stream.replicas` whole.

A tiered account that has **no tier for the class you asked for** — `R1` and
`R5` declared while `stream.replicas: 3` — is neither, and neither is a tier
that *is* declared but carries no storage limit: the account's report lists
every class it holds objects in, whether or not a limit was ever set for one,
so `R3` being present is not `R3` being declared. The engine reports a budget
of **zero** under its own source — `account_no_tier` or
`account_tier_no_limit` — rather than as an account that is merely full: the
two carry the same number and the opposite instruction, since one clears by
somebody freeing room and these only by a change of configuration. A tier
declared **unlimited** is neither again: it states its limit as a negative, the
broker creates against it, and the engine sizes from three quarters of its own
free disk, the cap a server running its defaults takes, as it does for any
broker that states no limit.

The broker refuses every stream, consumer and bucket create either way, but
**not with the same words**, so the engine's own refusal carries the ones you
will actually see. A class the account's limit table has no entry for is never
resolved at all — `no JetStream default or applicable tiered limit present`,
before a single byte is compared — and that covers both the missing tier and a
tier the report carries only because the account holds objects in that class. A
class it really does declare with no disk, a memory-only tier, resolves
normally and is refused by the byte comparison instead: `insufficient storage
resources available` on every create that reserves bytes.

What that does to a node depends on **whether its objects already exist**, and
the two cases look nothing alike.

*A node that has not provisioned them does not start.* `crewlet run` creates
its streams while it is opening its broker client, and its coordination buckets
straight after — all of it before anything sizes a ceiling — so the first
create is refused and the process exits. The first of those two refusals names
the class the account carries no limit for and the two levers that move it: the
engine classifies it rather than reading it as the object having failed to
appear, so what you get is `R3` and `stream.replicas` and not `(and it is not
there: stream not found)` appended to the broker's bare text.

*A node whose objects are all already there boots, on numbers the broker never
agreed to.* Nothing is created, so nothing is refused. The
`statelog_broker_states_no_limit` warning says which shape it is and which
lever to move, and the `statelog_ceilings` line below it carries
`broker_source=account_no_tier` (or `account_tier_no_limit`) with
`broker_limit=0` — so the logs are sized against nothing but what they already
hold between them, floored at a gibibyte each. It keeps working until it has to
make something new: a state-log stream a new version adds, the mailbox for a
seat you just added, a coordination bucket. That create is refused as above.

Either way: set `stream.replicas` to a class the account carries a limit for,
or have the cluster's operator declare one for the class you asked for.

---

## Running the Engine + API

### Single Process (embedded API — the single-host default)

Any `api.port > 0` in the Tier A YAML makes `crewlet run` start an **embedded API server inside the engine process** — one process runs the engine, the dashboard, and every webhook route:

```yaml
api:
  port: 80       # 0 (the default) disables the embedded API
  external_url: "https://crewlet.example.com"   # REQUIRED once port is set
  auth:
    admin: serve             # serve | refuse, required once port is set
    tokens: [{id: founder, token: "${CREWLET_API_TOKEN_FOUNDER}",
              grants: [admin]}]   # at least one
secrets:
  active_key_id: k1          # REQUIRED on EVERY node, serving or not
  keys: [{id: k1, material: "${CREWLET_SECRET_KEY_K1}"}]
```

**`secrets.keys` is required on every node**, whether or not it serves the
API: every record on every state log is signed and verified under the keyring,
the company document a node fetches from its peers is authenticated by its
seal, and — once a port is set — it signs every session cookie and per-run
token too. **Three more settings stop being optional the moment `api.port` is
non-zero** — `api.external_url`, `api.auth.admin` and at least one
`api.auth.tokens` entry. `crewlet validate` refuses each by name, so a
deployment finds out on a laptop rather than at bind time. See
[the Tier A example](../getting-started/configuration.md#tier-a) and
[Configuration § Auth](../concepts/configuration.md#auth) for what each one
decides.

**`api.external_url` is where a BROWSER reaches this deployment**, which behind
a load balancer or a TLS-terminating proxy is not what `api.host` and
`api.port` bind. The engine cannot read the scheme off the request it receives —
the proxy has already terminated TLS — so the session cookie's `Secure` flag,
its `__Host-` prefix, the origin every write is checked against and the base
every webhook URL is built on all come from this one value. Behind a proxy,
name the proxy's own block in `api.trusted_proxies` too, or every caller shares
the proxy's address — one sign-in throttle curve per login for everybody,
so a stranger guessing at somebody's login slows that person's own sign-in,
and one client in every audit row. `crewlet validate` warns about an `https` external
URL with no proxy trusted, and every start of a node serving the API logs the
same warning as `config_advisory`, because the engine never terminates TLS
itself.

```bash
crewlet run -config crewlet.yaml    # engine + embedded API on :80
```

(`-api-port 8000` on the command line does the same.) This is the shape every single-host walkthrough in these docs uses, and that embedded server **is** the webhook target the integrations register (e.g. `http://host.docker.internal:8000/webhooks/gitlab`). **Port 80 buys exactly one thing**: a webhook URL with no `:port` suffix, which matters when the address is pasted into a vendor's UI by hand or has to survive a proxy that rewrites ports. It costs a privileged bind — as a non-root process on Linux that needs `sudo sysctl net.ipv4.ip_unprivileged_port_start=80` (persist in `/etc/sysctl.d/`) or `CAP_NET_BIND_SERVICE`.

The two bundled examples land on either side of that trade, which is the clearest way to read it. `examples/nimbus.config.yaml` pays for port 80: its company registers GitLab webhooks, so the address gets pasted into a vendor's UI. `examples/nimbus-claude-cli.config.yaml` takes `api.port: 8000`, because a chat-only company has no inbound webhook at all and gets nothing for the privileged bind — its port only has to match the `CREWLET_MCP_BRIDGE_URL` its own seats dial back on. Make sure nothing else already owns the port you pick.

Do **not** also start a second node on the same host with such a file — both read the same `api.port`, and the second binder hits `EADDRINUSE` and fails its boot at once, naming the address, before it has recovered anything.

**The listener comes up before the boot does, and serves before the seats do.** `crewlet run` binds `api.port` — and `api.public` and `api.artifacts`, where this node serves them — as soon as it has read its Tier A, before it opens its store. Until the node has started, that socket answers a starting node's answers: `/health` `503` while the node recovers what it holds and `200` once it has, `/ready` `503`, and every route it will serve `503 starting` with a `Retry-After`, so a vendor or a client meets a node about to serve rather than a refused connection (see [While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting)). Then the node's own surface takes the same socket over — logged as `api_listening` — and only after that does it start claiming seats, so `/dashboard` and every webhook route are served before the first seat is claimed, even on a company whose agents take much longer to come up. That ordering matters because claiming a seat starts that seat's per-role MCP servers (one subprocess per server per seat, each a spawn, a handshake and a `tools/list`), and a company with seven seats and three vendors is twenty-one children. Serving after them made the whole inbound edge dark for as long as the slowest vendor took, which reads exactly like a hung process.

While seats are still being claimed the node reports what is true rather than pretending: `/health` lists the seats it holds so far, and nothing is lost in the meantime because every seat's mailbox is created before any claiming (see [Event System](../concepts/event-system.md#a-seats-mailbox-exists-before-the-seat-is-running)). A seat's own children still start before its mailbox is attached, so a turn never begins without its tools — that ordering is unchanged; what changed is that they start **concurrently** rather than one after another, so a seat attaches in the time its slowest server takes rather than the sum of all of them.

### Exposing webhooks without the admin API

Every route an outside party calls shares `api.port` with everything else by
default: the vendor webhooks, the admin API (`/config`, `/secrets`, `/setup`,
`/operator`, the fleet and retention gestures), the read surface and the
dashboard, and the sign-in routes under `/auth`. A deployment that must accept
GitLab or Slack deliveries from the internet but keep the rest private would
then have to publish that one port
and filter it **by path** in a proxy — an allow-list kept outside the engine,
where one forgotten route or one prefix matched too widely puts `/config` on
the internet and nothing here can see it.

`api.public` splits the two onto separate sockets instead:

```yaml
# crewlet.yaml (Tier A)
api:
  host: "10.0.0.5"     # private: the dashboard, the REST API, the probes
  port: 8000
  external_url: "https://crewlet.internal.example.com"   # where people reach api.port
  public:
    port: 8443         # published: webhooks and the sandbox endpoints only
                       #   (host defaults to every interface)
    external_url: "https://hooks.example.com"   # where vendors reach it
```

| Route | Served on |
|---|---|
| `/webhooks/*` — every vendor delivery, the Slack OAuth landing, the GitHub App return | `api.public` only |
| `/otlp/{token}/v1/{signal}` — sandbox telemetry | `api.public` only |
| `/mcp/{token}` — the agent-mode tool bridge | `api.public` only |
| `/box/git/*` — a box's git, a coding box's or a check's, on a node whose roles name `git` | `api.public` only |
| `/box/code/*` — a check's box's reads of its commit, on a node whose roles name `git` | `api.public` only |
| everything else: `/health`, `/ready`, `/dashboard` and its assets, the sign-in routes under `/auth`, every REST read, `/ws/stream`, `/config`, `/secrets`, `/setup`, `/iam/*`, `/operator/*`, `/backup`, `/fleet/*`, `/work/*` | `api.port` only |

`api.port` answers a public route with `404 no_route` and a hint naming
`api.public` — a vendor pointed at the wrong port learns it from the first
delivery rather than from a delivery quietly accepted on the socket nobody
published. The public listener answers every other route exactly as it answers
a path nothing serves: the same `404`, the same generic hint, nothing that says
an admin port exists behind it. It requires no credential and decides
before the guard runs, so `/config` there is that same `404` whatever
credential is sent. The split
is the engine's own, from one list (the `/webhooks/`, `/otlp/`, `/mcp/`,
`/box/git/` and `/box/code/` prefixes, the routes that authenticate by a
provider signature or a signed per-run or per-check token rather than by a
person's or a token's credential), so there is no allow-list to keep in step
with the routes a release adds.

**Why the sandbox endpoints are public.** A remote sandbox (E2B's cloud) runs
on somebody else's network and reaches only what you publish; it calls
`/otlp/{token}` and `/mcp/{token}` with the signed per-run token in the path,
and a box reaches the code host's `/box/git/*` — a coding box with its run's
token as git's Basic password, a check's box with its check's — never holding
a credential of anybody's; a check's box reads the one commit its check
measures at `/box/code/*` with the same credential. Leaving them on `api.port`
would force that port public for every company running remote sandboxes.
Point `CREWLET_SANDBOX_OTEL_RECEIVER_URL` and `CREWLET_MCP_BRIDGE_URL` at the
public address. **`/box/git/*` and `/box/code/*` are mounted only on a node
whose roles name `git`**, so on a fleet the balancer in front of the public
listener routes both prefixes to the git nodes, as it routes a person's
`/git/` on `api.port` (see [Code Host → Coding boxes](../concepts/code-host.md#coding-boxes)
and [Check boxes](../concepts/code-host.md#check-boxes)); a node without the
role answers them the mux's `404`.
`CREWLET_BOX_GIT_URL` is that route as a box reaches it — the public address
with `/box/git` on the end, naming a git node or the balancer over them, since
any git node serves any box — and every node that runs seats sets it when the
company hosts code: it is what a launch hands its box as the base of every
clone, and a node without it refuses a `run_sandbox` call naming repositories
(`sandbox_git_url_unset`) rather than give a box a repository it cannot reach.
A run's git token rides each of those requests as git's Basic password, so the
URL is `https://` wherever it leaves the box's own host (see
[Code Sandbox → Security model](../concepts/code-sandbox.md#security-model)).

**Why the probes are not.** `/health` describes the node — its version, its
seats, its posture — for whoever runs it, and an orchestrator or a load balancer
can probe a port other than the one it routes traffic to. Point the public load
balancer's health check, and the orchestrator's liveness and readiness probes,
at `api.port`'s `/health` and `/ready`; on Kubernetes that is the pod's own
address, which the kubelet reaches whether or not the port is in the public
Service. Readiness is per process, so a draining node leaves rotation on both
sockets at once.

What to point where:

- **`api.public.external_url`** is where vendors and sandboxes reach this
  deployment: the **public** listener's address as your proxy or load balancer
  publishes it, required once `api.public.port` is set on an `ingress` node.
  Every webhook the engine registers, every vendor app it provisions and every
  per-run sandbox endpoint it hands out is built on it.
- **`api.external_url`** is where people reach the dashboard — `api.port`'s
  address as your private network or VPN reaches it. It names the session
  cookie and is the only `Origin` a write is accepted from, and every link an
  agent composes for a person (`${crewlet_base_url}` in a tool skill, a page
  change's `url`, an invitation or a password link) is built on it; the public
  listener could serve none of them. On a deployment with no public listener it
  is the vendors' address too; the two are two fields because here they
  cannot be one. Both are Tier A, because both are facts about where the
  operator published this deployment's sockets.
- **The dashboard and the CLI** stay on `api.port`: `crewlet` commands that
  read a node's own Tier A file dial `api.host:api.port`, which is unchanged.
- **A node without the `ingress` role** serves its
  [probes](../reference/api-endpoints.md#probes-on-a-node-without-ingress) on
  `api.port` and one route beside them, its seats' tool bridge, and with
  `api.public` set it binds the public port for the bridge alone — so one
  Tier A file still serves every role, and every node's
  `CREWLET_MCP_BRIDGE_URL` names its own public port. Its `api.port: 0` (or
  `-api-port 0`) is still the hard off switch: it binds nothing, the public
  port included, whatever the shared file says about `api.public`.

`api.host` may stay `0.0.0.0` where the network keeps `api.port` private — a
container whose public Service or load balancer forwards only `api.public.port`
— or name a private interface where the host itself is reachable.
`crewlet validate` refuses a public listener beside `api.port: 0` on a node with
the `ingress` role (no probe would answer) and a public port that is also
`api.port`, `stream.cluster.port` or
`stream.leaf.port` on the same address or with either binding every interface.

**Draining.** Both sockets go through the one drain gate: from the first moment
of a drain the webhook routes answer `503` with a `Retry-After` on the public
listener, while the sandbox endpoints keep serving the runs already in flight,
and `api.port` keeps `/health` at `200`. Once the drain completes the public
listener closes first and `api.port` after it, each waiting up to five seconds
for its requests, so the probes answer until the very end — an open bridge
session can hold the public listener for its whole five seconds, and a
liveness probe must not fail while it does.

### Separate processes (a split deployment)

Run ingress as its own node when you want the webhook receiver to stay up
across engine restarts, or the two on separate hosts.

**Two processes need a stream they can both reach**, which a solo embedded
one is not — it binds no socket, so each would have its own. Either shape
from [the section above](#the-stream-beyond-one-host) works: a *clustered*
embedded stream (`stream.cluster`), or an external NATS server
(`stream.type: nats` plus `stream.url`). They also need shared coordination:
Tier A refuses `coordination.type: local` alongside either of them, by name,
rather than letting two nodes each claim every seat.

Both nodes can read the same Tier A file: against an external NATS server
nothing in it is per-node except `node.id`, which `CREWLET_NODE_ID` injects
without templating anything, and `store.path`, the node's own database, which
the engine locks for as long as it runs — so two processes on one host each
name their own through a `${VAR}` set per process, as the public port below
is. A clustered embedded stream is the one exception — each member also names
its own route port, its own peers and, on one host, its own
`stream.store_dir`, because a second broker on one directory is
[refused](#every-node-embeds-a-member-of-one-cluster). So
give each node its roles at the command line, and its own `-api-port`: on one
host two listeners cannot share a port, and only the ingress node's carries
the API — the other's carries its [probes](#probes) and nothing else:

```bash
# Terminal 1: the agents and the fleet duties — /health and /ready only
crewlet run -config crewlet.yaml -roles data,seats,workers -api-port 8001

# Terminal 2: the webhook receiver and the dashboard
crewlet run -config crewlet.yaml -roles data,ingress -api-host 0.0.0.0 -api-port 8000
```

`-api-port 0` on the first binds nothing at all, which is fine on a host where
nothing probes it.

Give each node a distinct `node.id` (or `CREWLET_NODE_ID`) — two nodes sharing an id miscount the fleet. See [Running a Fleet](fleet.md).

If any seat runs in [agent mode](../concepts/llm-backends.md#agent-mode), the seats node needs its port, and its `CREWLET_MCP_BRIDGE_URL` set to that port as a sandbox reaches it. Without the `ingress` role that listener serves the `/mcp/{token}` tool bridge beside the probes and nothing else (`api_probes_listening … tool_bridge=true`); with `-api-port 0` the node refuses every agent-mode launch, naming `api.port`.

**With a [public listener](#exposing-webhooks-without-the-admin-api) on one host,** the two processes would both bind `api.public.port` — the ingress node for its webhooks, an agent-mode seats node for its bridge — and the second fails at boot. Give each process its own public port through a whole `${VAR}`, which a Tier A number accepts:

```yaml
api:
  port: 8000
  public:
    port: ${CREWLET_PUBLIC_PORT}
```

```bash
CREWLET_PUBLIC_PORT=8443 crewlet run -config crewlet.yaml -roles data,ingress -api-host 0.0.0.0 -api-port 8000
CREWLET_PUBLIC_PORT=8444 crewlet run -config crewlet.yaml -roles data,seats,workers
```

The seats node binds only its public port, for its bridge; one started with `-api-port 0` binds nothing, so it needs no public port of its own.

`crewlet migrate` is idempotent and safe to re-run. Each node also
auto-migrates its own store file on boot, and two nodes starting together
cannot race, because they are not migrating the same file — every node owns
its own. Running the explicit step first turns a schema change into an
observable step rather than a side effect of startup.

Both take the **Tier A** bootstrap file (`crewlet.yaml`) — the founder-owned company YAML is seeded separately (`crewlet config import`, or `crewlet run -company`).

`crewlet config import` writes that file as a new active revision. Against a
running node it goes through `PUT /config`, and every node converges with no
restart. Against a **stopped** one it writes this node's store, and the node
publishes the activation when it next starts. Either way the file is one
gesture; only the timing differs.

- **`data`** holds the company's durable state — a copy of the replicated estate, the identity directory's included, and the node's own event log; `ingress` and `workers` need it, and a node without it is [stateless](fleet.md#nodes-that-hold-no-data)
- **`seats`** runs the agents — claims seat leases, boots the instances, processes their turns
- **`ingress`** serves the REST API — receives webhooks (Slack, GitLab, Jira, GitHub, Confluence) and publishes them to the event queue
- **`workers`** runs the company-wide [singleton duties](../concepts/seat-ownership.md#singleton-duties) — the scheduler tick, the maintenance sweep, the sandbox waiter, the integration reconcile, the learning passes, the state logs' trim, the embedding duty, the object store's collector and the identity sweep

They are one command, and they build the **same** application: every node learns the company from the active config revision and the live picture from the broadcast event stream. Point `CREWLET_SANDBOX_OTEL_RECEIVER_URL` at whichever node is externally reachable: an `ingress` one, which serves the `/otlp/{token}/v1/{signal}` receiver. Its tokens are per-run and signed, so the node that mints and the node that verifies need no shared memory: signing uses the Tier A keyring, which every node already holds — the SAME keyring on each, or every token one process mints is forged as far as the other is concerned. Rotating that keyring costs nothing in flight: a token names the key that signed it, so a run holding one keeps working until the key is dropped — the runbook is in [Secret store](../concepts/secret-store.md). `CREWLET_MCP_BRIDGE_URL`, if any seat runs in [agent mode](../concepts/llm-backends.md#agent-mode), is the opposite: a bridge session lives in the process that opened it, so each `seats` node sets it to **its own** address and serves `/mcp/{token}` itself, on its own `-api-port`, even without the `ingress` role. `CREWLET_BOX_GIT_URL`, if the company hosts code, is the receiver's case again: each `seats` node sets it to the git nodes' public `/box/git` — any of them, or the balancer over them — because a run's git token is signed with the same keyring and admitted by the run's row, which every git node reads.

Point liveness probes at `/health` (stays `200` through a drain) and load-balancer readiness at `/ready` (`503` while draining or before the first config revision applies, with the cause in its `reason` field), and give the boot a startup probe on `/health`, which answers `503` until the node has recovered what it holds and `200` from then on, while the rest of the boot runs and `/ready` stays `503`. Every node answers both, whatever its roles — see [Probes](#probes) for what `/ready` means on a node that takes no traffic and how long the startup probe has to wait. A draining node keeps its listener until the drain completes, so both probes answer throughout, and it refuses any request that would start new work with `503` and a `Retry-After`; see [During a drain](../reference/api-endpoints.md#during-a-drain). A node with nothing in flight drains in milliseconds, which is also the whole of an `ingress` node's drain unless it is serving git (a git request already running is finished first; a new one is refused). So give such a pod a `preStop` sleep of a few seconds if you need its load balancer to have stopped sending it requests before the listener goes: Kubernetes takes a terminating pod out of its Service's endpoints as the hook begins, and sends the SIGTERM that starts the drain only once the hook has ended, so the sleep is the time that change has to reach every proxy in front of the pod. (A balancer that probes `/ready` itself sees the `503` only once the drain has begun, and acts on it after its own period and threshold.) For git the same window is the one the drain waits through: a git client sent to the pod once its drain has begun is refused at its first request and retries against a peer rather than being cut part way, but the upload `POST` of a fetch begun on a peer is served there, and waited for, for as long as such requests still arrive. The engine will not sleep on its own: a delay long enough to matter would eat the `terminationGracePeriodSeconds` the drain itself has to finish inside, and only the deployment knows how much of that grace its longest turn needs.

Both communicate through the stream, and through the coordination KV on the
same broker — never with each other. Both accept `-debug` for verbose
logging.

### Probes

Every node with an `api.port` answers `/health` and `/ready` on it, whatever
its roles, so an orchestrator can probe each one the same way. What the two
answer is fixed; what `/ready` *means* follows from whether the node takes
traffic:

| | `/health` (liveness) | `/ready` |
|---|---|---|
| A node with `ingress` | `200` while the process is alive, from the moment the node has recovered what it holds — through the rest of its boot, its serving life and a drain — and `503` before that | Should traffic come here: `503` until the node serves, and then `503` while draining, unconfigured, on a `shed` or `stuck` posture, or while its own copy of the estate is out of service and it cannot name a data node to route to (`estate_behind`) |
| A node without it | The same, with the node's half of the health envelope | Is it doing its work: also `503` while its broker link is down, it does not hold its presence lease, or — running seats — it has not been admitted to claim them |

A node without `ingress` serves **nothing else** on that port — no API, no
dashboard, no webhook — and a seats node's agent-mode tool bridge only when
`CREWLET_MCP_BRIDGE_URL` is set. The reasons, their precedence and both bodies
are in [Probes on a node without ingress](../reference/api-endpoints.md#probes-on-a-node-without-ingress).

**While a node starts**, both kinds answer their probes the same way, because
neither serves anything of its own yet: `/health` is `503 {"status": "starting",
"phase": "recovering"}` while the node recovers what it holds and
`200 {"status": "starting", "phase": "preparing"}` while it brings up what its
company configures, and `/ready` is `503` with `reason: "starting"`. Every
route the node will serve is `503 starting` with a `Retry-After` — on a node
without `ingress`, that is its tool bridge alone — and every other path is the
`404` it answers once it serves. The bodies are in
[While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting).

On Kubernetes that is one probe block for every node, with `api.host: 0.0.0.0`
so the kubelet can reach the pod's address, and `api.port: 8000` — the port the
image's `EXPOSE` documents:

```yaml
# the engine container of any node — ingress, seats, satellite
ports:
  - {name: http, containerPort: 8000}
startupProbe:
  httpGet: {path: /health, port: http}
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 60     # ten minutes for the node to recover what it
                           #   holds; see "Sizing the startup probe"
livenessProbe:
  httpGet: {path: /health, port: http}
  periodSeconds: 10
  timeoutSeconds: 5        # /health reads the coordination plane under a
  failureThreshold: 6      #   budget well inside this; a minute of silence
                           #   is a wedged process, not a slow broker
readinessProbe:
  httpGet: {path: /ready, port: http}
  periodSeconds: 5
  timeoutSeconds: 5
```

Liveness tolerates a minute because the engine ends a wedged process itself:
the [watchdog](../concepts/seat-ownership.md#the-wedged-node-and-why-it-leaves)
exits once its watched duty stalls for a seat lease TTL (45 s), so a liveness
failure is the backstop for a process that cannot even do that. Readiness on a
node without `ingress` is what a rolling update waits on before it replaces
the next pod, so a satellite that cannot reach its fleet holds the rollout
rather than letting it take the next one down too.

Give every pod a `terminationGracePeriodSeconds` of at least its longest turn
plus 60 s, plus any `preStop` sleep you give it — Kubernetes starts the grace
countdown before the hook runs, so the sleep is spent out of the same grace.
The drain waits for the turns already running — and, on a pod that serves the
engine's own git host, for the clones and pushes already running, which it
finishes rather than cuts — and has no bound of the engine's own, so the
longest turn (or the longest clone or push the pod should finish) is the part
of the grace it needs; what follows the
drain — giving its leases back, closing the HTTP listeners (five seconds each,
which an open sandbox bridge session spends whole), flushing the auxiliary
spend, the reads of unit repositories and the day's usage, closing the store —
takes milliseconds on a healthy
fleet but up to about 46 s on a member that has lost both its event stream and
its coordination store, and about 50 s on a node without `data`, each step
waiting out its own bound (see
[Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown)). A grace of the longest turn
alone kills such a member before its store closes; the extra 60 s covers that
with room for the process to exit. A deployment whose turns finish in seconds
still needs 60 s, not Kubernetes' 30 s default — and an `ingress` pod with the
`preStop` sleep described above needs 60 s plus that sleep.

#### Sizing the startup probe

**`/health` answers `503` until the node has recovered what it holds.** The
node binds its listener before its boot, so the port answers from the first
second, but until the node has recovered what it holds itself — its own store
and that release's schema migrations, the broker's file store, the broker's
listeners, JetStream catching up with its peers (on a node without `data`, its
leaf link to a member), the streams and buckets it provisions, the replicated
estate and its state logs, and a joining node's snapshot — `/health` answers
`503` with `"phase": "recovering"`. Applying those logs is not part of it: it
catches up in the background from the end of the recovery, and what waits for
it is the node's first seat, never `/health`. Kubernetes runs no liveness or
readiness probe until a startup probe has succeeded once, so the startup probe
is what bounds the recovery, and it fails the container after
`failureThreshold × periodSeconds`. Without one, the liveness probe above
counts the recovery as a wedged process: six failures, one minute, and the
kubelet restarts the node partway through its recovery. The next boot then
recovers the same store again, so a store whose recovery takes longer than a
minute never boots.

The listener does not answer `200` during the recovery, on purpose. A `200`
then could say only that the process exists, and a recovery that never
finished, on a disk that stopped answering, would have nothing to restart it.
Bounding the recovery belongs to the startup probe, because only the
deployment knows how large a store it gave the node.

**Nothing the company configures is part of it.** The moment the recovery is
over the node logs `engine_recovered` and `/health` turns `200`, with
`"phase": "preparing"`, while the node brings up what its company configures:
its shared MCP servers — each bounded by its own
[`startup_timeout_seconds`](../getting-started/configuration.md#mcp-servers),
for its start and again for its first tool listing, with no ceiling on the
setting — its sandboxes and native halves, the first apply of its active
revision, and the dashboard's projection. A probe sized by the store knows
nothing about any of them, so none of them is allowed to hold it: the startup
probe passes, the liveness probe takes over and is answered `200`, and
`/ready` stays `503` with `reason: "starting"` until the node serves, so
neither traffic nor a rollout counts the node before then.

**What no probe catches is a part of that which never returns**, and it is the
price of keeping the company's part out of the probe. A part that waits on
something else is bounded by its own setting — a tool server that runs out of
time costs only its own tools, and the boot carries on — but the first apply
as a whole has no deadline beyond its parts', since one cut short would publish
a company missing whatever had not come up yet. So a part that hangs leaves
the node `preparing`: `/health` `200`, `/ready` `503 starting`, and no restart.
A restart would not cure it — the next boot meets the same company and waits on
the same part — so what a deployment needs is to see it. A rollout stops
behind such a pod (a Deployment marks it with `ProgressDeadlineExceeded` after
its `progressDeadlineSeconds`), and an alert on a pod whose `/ready` has named
`starting` for longer than the startup probe's window plus twice the slowest
shared tool server's `startup_timeout_seconds` names the node. Its log says
where it stopped: `engine_recovered`, and no `api_listening` after it.

**The arithmetic:** `failureThreshold × periodSeconds` has to cover the
recovery of the largest store the node may hold, plus the rest of the node's
own bring-up.

- **The recovery** was measured at about a second and a half per GiB after a
  crash on a fast local disk (see the boot's three waits under
  [Every node embeds a member of one cluster](#every-node-embeds-a-member-of-one-cluster)).
  A slower disk is slower in proportion.
- **The rest of the node's own bring-up** takes seconds on a healthy fleet.
  Allow two minutes for it. That covers the whole of the broker's two bounded
  waits — 30 seconds for the listeners and 60 for JetStream to catch up — and
  the seconds provisioning takes on a cluster that can place what it creates.
  Provisioning on a cluster that cannot is held to budgets of its own, up to
  five minutes for the coordination bring-up on a member (see
  [A clustered node is given longer to create them](#a-clustered-node-is-given-longer-to-create-them)),
  so add those minutes if your members routinely boot while their peers are
  still forming.

The block above allows ten minutes (60 × 10 s). Less the two minutes, that
leaves eight minutes, or 480 seconds, of recovery: about 320 GiB at 1.5 s per
GiB, or about 80 GiB on a disk four times slower. For a larger store, raise
`failureThreshold` and leave `periodSeconds` alone, because the period is
also how long a finished recovery can wait before the kubelet sees it.

**Measure your own.** `api_bound` is the moment the port began answering. A
recovery longer than ten seconds logs `embedded_broker_starting` every ten
seconds with its `elapsed_seconds`. The time from the process's first log line
to `engine_recovered` is the recovery the startup probe has to cover; from
there to `api_listening` (`api_probes_listening` on a node without `ingress`)
is the company's part, which it does not. Time a node's first boot on a new
release as well: each node applies that release's schema migrations to its own
store as it opens it, inside the recovery. The store holds the company's files
as well as its streams on the default `nats` object store, which has no byte
ceiling, so revisit the number as the store grows.

**Err long.** A probe that is too short is a restart loop that never ends. A
probe that is too long delays only the restart of a recovery that is genuinely
stuck. A boot that fails outright, such as one with an unusable store
directory, a taken port or a bad config, exits at once with its reason
whatever the probe says.

A StatefulSet of [clustered broker members](fleet.md#the-broker-members-and-leaves)
needs `podManagementPolicy: Parallel`. The default, `OrderedReady`, starts a
member only once the one before it is ready, and no member can be ready alone:
the company revision it applies and the presence lease it holds are both in a
coordination store that writes through a quorum of the members — so the first
waits for peers the StatefulSet will not start until it is ready, and the set
never comes up.

### Replica count

**Run one `crewlet run`, and scale up before you scale out.** A single
engine handles many concurrent turns — agent handlers are
goroutines, so the practical ceiling is your model plans' and keys' rate
limits and host memory — every model call is a coding CLI process of 200–400 MB,
up to each `providers.llm` entry's `max_concurrent` (4) at once — not the
engine's own process count. One node is the design's degenerate case, not
a lesser path: it holds every lease, and everything a fleet does works
exactly the same way.

Multi-node is supported and certified by a chaos suite that kills nodes
mid-turn under load. Reach for it when a node's failure is not acceptable
downtime, when you need to terminate traffic separately from running
agents, or when some seats have to run somewhere specific — not as a
throughput lever, because `max_concurrent` is per process and N nodes is
N × that ceiling whether you wanted it or not.

**[Running a Fleet](fleet.md)** is the guide: node roles, seat placement,
draining, and rolling upgrades. The two things that bite hardest:

> **A fleet needs shared coordination.**
>
> Seat leases live in the coordination slot. `coordination.type: local` is a
> per-process store, so every node would believe it owns the whole company,
> which is why a Tier A file that pairs it with a clustered or external stream
> is refused at load on `coordination.type`. A fleet needs
> `coordination.type: embedded-kv`; see [Running a Fleet](fleet.md). The slot
> governs the *leases* only — the fleet's shared records are on the KV
> regardless, because they have to survive a restart as much as a peer, and
> the KV lives on the stream's own broker whichever value the slot holds.
>
> **And, when the nodes *are* the broker, a quorum to keep it on.**
>
> One node or three, never two: two embedded members have no quorum without
> each other, so the fleet stops serving the moment either restarts, and Tier
> A refuses that config by name, counting `stream.cluster.peers`.
> `stream.replicas: 3` is the other half — one replica count covers the
> engine's streams *and* the coordination buckets, because both live on the
> same broker, and at 1 the loss of a member takes a seat's mailbox or the
> fleet's leases with it. Against an external NATS cluster the quorum is that
> cluster's to provide rather than the engine's to count; see
> [An external NATS server](#an-external-nats-server).
>
> **Raising it on a fleet that already ran needs the existing objects resized.**
> Nothing the engine provisions is ever rewritten by a booting node — a
> shared stream's configuration has one writer and it is not whichever node
> started last — so a rolling restart after raising `stream.replicas` finds
> every stream and bucket already there at the old count and adopts it. A node
> that is short refuses to start and says so, naming both counts: a stream
> because an acknowledged publish would be proving fewer copies than
> `stream.replicas` promises, and a coordination bucket because the leases,
> the fencing epochs and the company's secrets would be on fewer disks than
> the config claims. Resize the objects deliberately (`nats stream update
> --replicas=3`, which covers the buckets too — a bucket *is* a stream), or
> stand the fleet up fresh. On an embedded fleet the order is: bring the fleet
> up at the old count, resize every stream and bucket through one member's
> client listener with the operator key
> ([Reaching an embedded fleet's broker with the nats tool](#reaching-an-embedded-fleets-broker-with-the-nats-tool)),
> then raise `stream.replicas` and restart — a node refuses to start while any
> object is short, so the resize comes first.

### What an acknowledged publish has reached

**`stream.sync` decides, and it defaults to `always` at every replica count.**
Every write is fsynced before the broker acknowledges it, so a publish that
returned is on the disk of the member that took it — which is what the
`EventQueue` contract's "durable" means, and what the company's own records
depend on. The cost is one fsync per write: **1–3 ms on NVMe**, and 15–40 ms
at the 99th percentile on a network-attached volume.

**It is deliberately not inferred from `replicas`.** The tempting inference —
a replicated member has a quorum instead of a disk, so it can skip the fsync —
is true of *one* failure class and there are five:

| What fails | Does a quorum survive it? |
|---|---|
| One host loses power | Yes — the other two hold the write |
| The process is killed, or panics | Yes — the page cache is the kernel's, and the kernel lives |
| An orderly shutdown | Yes — the store is flushed on the way out |
| A rack or an availability zone loses power | **No** — a majority can go together |
| Correlated power loss across every member | **No** — three copies of one unflushed page cache is one copy |

A three-node fleet in one rack, which is what a first production deployment
usually looks like, is exposed to the bottom two rows by construction.

**Declining the fsync is a legitimate trade and it is made explicitly.** Set
`sync` to a duration — `30s` — and that duration is the window: the most an
acknowledged write may be behind the disk. Tier A refuses the value in the
four places where it would be recorded and then not honoured:

- **against `stream.type: nats`**, because the field configures the embedded
  server's file store and an external cluster stores its own data (set
  `sync_interval` on that cluster instead);
- **on a leaf** (`stream.leaf.urls`), because a leaf's broker runs no
  JetStream and has no file store at all — the members it joins decide what
  an acknowledged write has reached, in their own `stream.sync`;
- **below `replicas: 3`**, because the disk being traded away is the only copy
  there is, so the window buys nothing;
- **on a cluster whose peers are all on this host**, because the majority the
  window trades for shares one power supply and one page cache.

Give each node a distinct id — `node.id` in the Tier A file, or the
`CREWLET_NODE_ID` environment variable, which is how a container orchestrator
injects a pod name without templating the config. Two nodes sharing an id
miscount the fleet and each compute too small a share.

Each node migrates its **own** store file at boot; there is no shared schema
to bring up first, and no migration lock, because no two processes share a
file. [`crewlet migrate`](../reference/cli.md#crewlet-migrate) applies them
ahead of time when you would rather not do it on the startup path.

What a fleet gets right, each of which was a real defect before:

- *Duplicate Slack posts, duplicate Jira comments, two contradictory plans for one webhook.* A seat's inbox is attached only by the node holding its lease, admission is gated on a renew fresh enough to prove exclusivity, and the turn loop re-checks the seat fence at the top of every round and again before each of that round's tool calls — so a node that loses the seat mid-turn stops before its next call rather than running out the turn beside the seat's new owner. A turn that finished but whose delivery was never acked is not re-run, because the [completion ledger](../concepts/seat-ownership.md#the-completion-ledger) records what shipped.
- *Live coding sandboxes torn down mid-run.* Recovery is a per-seat step inside the acquire hook, fenced on the claiming node's epoch, instead of a fleet-wide scan that treated every in-flight run as abandoned.
- *Config activation.* Delivered by the [control plane](../concepts/control-plane.md) — a shared activation pointer whose own revision is the epoch, polled by every node — rather than the competing-consumer subscription that used to let exactly one replica apply a revision while the rest ran the previous company.
- *Token budgets.* Shared counters in the coordination slot, one slot per calendar window, so an org cap of 500 k a day is 500 k a day across the fleet — and they cover **every** completion the engine makes on a seat's behalf, the turn loop (the round-cap extension judge included), the coding sandbox, the turn-start context assembly and the auxiliary learning passes alike.
- *Duplicate auto-drafted skill pages and N× LLM spend on synthesis.* Skill clustering, skill curation and episode compaction are [singleton duties](../concepts/seat-ownership.md#singleton-duties) (they share one `worker:` lease, so a fleet runs each of them on exactly one node), along with the scheduler tick, the sandbox waiter, the integration reconcile, the maintenance sweep, the state logs' trim, the embedding duty, the object store's collector and the identity sweep. Each lease is claimed per tick: a node that stops gracefully gives its duties back as it exits, and one that dies mid-duty hands them back by lapsing, which for the longer duties takes up to their TTL (45 minutes for the maintenance sweep and the trim, three hours for the learning passes).
- *Unbounded table growth.* `scheduled_runs` and `conversation_sessions` both answer a short-horizon question and are written on every event that asks it. The migrations always said they were swept on a TTL; the sweep exists, behind the `maintenance` duty. Most fleet-shared records — the delivery dedupe, the rate valve, the completion ledger and each node's apply status — are not swept here at all: each lives in a [coordination](../concepts/coordination.md) bucket whose own age is its retention, so the broker expires them. Agent-to-agent channels are the exception and *are* swept by the duty, because a bucket age cannot tell an open ask from an answered one — and so are the removal markers every bucket with no age keeps, which the duty sweeps after three hours so a listing never re-reads every record the company has ever removed. The apply status is the one that hides: it is keyed by *node* rather than by event, so it does not look short-horizon — but a node that is scaled in, redeployed or crashed would leave its last report behind, which under generated pod names is one per pod that ever ran, and the bucket's one-minute age is what makes that node *vanish* instead.

Two things are still per-process. `max_concurrent`: Tier A's
`node.max_concurrent` (default 32) is the gate every agent turn takes a slot
from, and it is per node — so an org's ceiling becomes N × the configured
value. Size it per node, not per company. And a spent plan: an entry whose CLI
reported its vendor's rate limit sits out on the node that heard it — until
the reset the CLI named, an hour at most, or for a minute — while every other
node learns the same thing from its own calls once per window.

For the model underneath all of this — what a node is, what the fleet shares,
and where the constants come from — see
[Scaling Out](../concepts/scaling.md).

---

## The store

One local file per node, opened by **Turso** — the only driver, so there is no
setting that chooses one. The file is in the SQLite format, so any
SQLite-compatible client can read it.

**Turso keeps a native library cache, and the engine prepares it before the
first query.** The driver is pure Go in the sense that matters — no cgo, no C
toolchain — but its engine ships as a ~20 MB native library embedded in the
driver, extracted on first use into `$TURSO_GO_CACHE_DIR` (default
`~/.cache/turso-go`) and loaded from there. That cache is shared by every
process on the host and is written without a rename, so two engines starting at
once could leave a half-written file behind that fails verification for good.
Crewlet therefore extracts under a lock in `<cache>/turso-go/`, and clears and
re-extracts a cache entry that will not verify. Two consequences worth knowing:

- **Point `TURSO_GO_CACHE_DIR` at a writable, persistent path** in an ephemeral
  container. A read-only or per-restart cache costs a 20 MB extraction on every
  start; a cache root that cannot be created at all fails the store open with an
  error naming the directory.
- **A cache that cannot be repaired names the way out**, and there is no
  second driver to fall back to: delete that directory by hand, or
  point `TURSO_GO_CACHE_DIR` at a writable directory of its own.
- **The linux binaries need glibc, and there is no musl build.** The database
  engine is a native library loaded with `dlopen`, which makes the binary
  dynamically linked against `libc.so.6` even though it is pure Go and built
  with `CGO_ENABLED=0`. On Alpine and other musl systems it fails at `execve`,
  reported as `no such file or directory` about a file that plainly exists.
  Use a glibc base image — the published one is `debian:trixie-slim` for
  exactly this reason — or run the engine on a glibc host. macOS is
  unaffected.

**The engine owns the file exclusively.** A second process pointed at the same
path is not a degraded configuration, it is corruption waiting for a schedule
to collide — so nothing that genuinely needs to be shared between nodes lives
here. Seat leases, the activation pointer and per-node apply status, the
completion ledger, webhook dedupe and the rate valve are
all in the [coordination slot](../concepts/coordination.md) instead.

### The connection pool, and the one connection ordinary work cannot take

`store.max_open_conns` sets each file's **ordinary readers**. Left at `0` it is
**derived**, and either way what ordinary work may not occupy is added on top:

```
pool  = readers                 (max(8, GOMAXPROCS) unset; the setting when set —
                                 never fewer than 2 on the replicated estate)
      + 1  reserved for identity work
      + one per pinned writer   (the replicated estate only — one per
                                 state-log domain this node applies)
```

**`max(8, GOMAXPROCS)`.** A read on this store is CPU-bound work inside this
process — the database engine is embedded, so a query is not a round trip to a
server — and a connection that cannot get a core buys queue depth rather than
concurrency. GOMAXPROCS is therefore the honest ceiling, and 8 is the floor
because the dashboard's socket admits four concurrent queries **per socket**
and a company is routinely watched from more than one tab — so the floor is
two full dashboards. It was a flat `4` before: a number describing one socket,
on a host of any size, so three tabs offered twelve concurrent scans to a pool
of four and the engine's own reads queued behind whichever four arrived first —
on a 16-core node exactly as on a laptop. Nothing said so, and the only fix was
to set this field by hand on every deployment that outgrew one tab.

The **unit** is the socket, which is what the floor counts: a person with three
tabs open offers twelve concurrent queries, which wait in the pool's own queue
past its size, while the one read that must never wait behind them — resolving
who is acting — runs on the connection reserved for it below. See
[Scaling Out § The socket's admission semaphore](../concepts/scaling.md#the-sockets-admission-semaphore).

**The reserved connection is held, not merely counted.** A free connection goes
to whoever asks for one first, so a spare one nobody holds is taken by the
first read burst that wants it. The store therefore **draws that connection at
open and keeps it**, the same move a pinned writer makes, and only identity work
ever runs on it — the one arrangement under which a socket storm cannot starve
the lookups that answer who is acting. See
[Scaling Out § The admission semaphore, and the connection it cannot take](../concepts/scaling.md#the-admission-semaphore-and-the-connection-it-cannot-take)
for why it is held rather than defended by a semaphore: a gate in front of the
pool would stop `database/sql` ever reaching its own limit, and the
`pool_starved` alarm below reads exactly that.

**Setting the field sets the readers, and nothing else.** An explicit value
replaces `max(8, GOMAXPROCS)`; the reserved connection and the pinned writers
are added on top of it exactly as they are to the derived figure, so the number
you give is always the number of connections ordinary reads may draw. Taken out
of it instead, a figure at or below what is held would leave the readers
nothing at all, and a read burst would queue behind a writer with nothing
naming the loss. Raise it if the `pool_starved` alarm fires — see
[Alarms](../reference/alarms.md) — which means reads are queuing before they
start.

The load-bearing tables:

- **`agent_diary`** — each agent's private observation log, each note with the vector of the model it came from (a per-agent scan the database ranks; there is no vector index). Written by the reflect path, which embeds content on write; the node holding the seat fills any note left without a vector of the current model. The `## Personal memory` prefetch reads it via hybrid candidate selection (vector top-50 ∪ recency top-50, deduped by row id) handed to an aux-LLM relevance filter. Shared knowledge is **not** stored here — natively it is rows in the replicated estate beside the vectors derived from them, and a Confluence knowledge base has no local copy at all; see [knowledge system](../concepts/knowledge-system.md).
- **`episodes`** — one row per completed turn, raw and LLM-compacted shapes in the same table; a raw turn carries the vector of the model it came from and a compacted row none (a per-seat scan the database ranks; there is no vector index). Drained by the episode-lifecycle duty.
- **`synthesized_skills`** + **`synthesized_skill_versions`** — auto-drafted skills the agent can load, plus their refinement history.
- **`counterparty_profiles`** — per-`(observer, subject, platform)` profiles built from observed interactions.
- **`agent_onboarding_markers`** — onboarding bookkeeping, one row per agent.
- **`crewlet_events`** — the observability event store. A phase completion's token counts are promoted out of its payload into columns, so the spend rollup reads nine narrow values a row instead of hauling every prompt and response across the driver — which is what lets it fold the whole window rather than a capped prefix of it.
- **`crewlet_event_parties`** — which agents each event involves, one row per pair. It is an *index* of the table above rather than state of its own: the dashboard's per-seat activity filter matches on it, and it exists because the engine's planner does no OR-optimization, so the same predicate spread across five columns would scan the log instead of seeking. Swept on the same horizon as the events it points at.
- **`conversation_sessions`** — the [conversation ledger](../concepts/conversation-sessions.md): what this seat already said in one thread, rendered back into that conversation's next turn.
- **`company_config`** — the revision payloads. Which one is *current* is the fleet's business, and lives in coordination; see the [control plane](../concepts/control-plane.md).

Migrations are **forward-only**: each file in `internal/store/schema/` is applied once and recorded by filename, and there are no downgrade scripts. Downgrading the binary below the schema it already migrated is not supported; restore a [backup](backup.md) instead. There is no migration lock and no advisory-lock protocol, because one process owns the file — the whole idiom disappears.

Everything else is either:

- **YAML config** — the org structure and every seat's definition
- **In-memory** — agent runtime state, the execution tracker
- **An external tool** — task state (Jira, GitLab issues)
- **The event stream** — routing, with a durable per-subscription backlog

## The code host

A node with the `git` [role](../concepts/configuration.md#noderoles), for a
company that sets [`code.native`](../concepts/code-host.md), serves the
company's repositories on its API listener under `/git/`, from a disposable
cache on its own disk. What such a node needs from its host:

- **git 2.55 or newer on its `PATH`** — the node floor. The node runs the
  host's own git as a subprocess per request and checks its version at boot:
  an older one refuses the boot, naming the version and the path it found. The
  code host's clients — a person's git, a CI runner's, a sandbox box's — need
  only 2.40 or newer (the client floor). The published image does
  not install Debian's git: it builds the newest git release from a pinned,
  checksummed tarball ([`scripts/git-pin.env`](https://github.com/crewlet/crewlet/blob/main/scripts/git-pin.env)),
  cross-compiled for each platform, with its HTTPS transport (libcurl, the
  GnuTLS flavour Debian's own git uses) and zlib and nothing else — no
  OpenSSL of its own (libcurl's own libssh2 and OpenLDAP still bring
  OpenSSL 3, Debian's `libssl3t64`, into the image and into every
  `git-remote-https`, as they do beside Debian's git), no PCRE
  (`git grep -P`, `git log -P`) and no Perl, Python or Tcl programs
  (`git send-email`, `git p4`, `gitk`). So a git security fix reaches a git
  node only as an engine image built from a bumped pin — never as an update to
  the base image's own packages — and the project checks the pin against
  git's releases every week, verifying each release's signature, so the
  engine release that carries a git fix is the one to take. It reads its
  system configuration from `/etc/gitconfig` and `/etc/gitattributes`, where
  Debian's git did, so an image you derive configures it there (`git var
  GIT_CONFIG_SYSTEM` prints the path); the code host's own processes read
  neither, whatever they hold. A local sandbox box
  that runs as the engine (`run_in: direct`) on a node from that image runs this
  git too, so a coding agent's `git grep -P` fails there; a `container` or
  `e2b` box runs its own image's git. The node serves a clone with git's
  single-pack reuse — the bitmap's preferred pack sent as it is stored, every
  other object as git packs any object, a stored delta reused and a whole copy
  searched for a delta — rather than multi-pack reuse, which every git
  at the node floor could serve (2.44 to 2.47 sent a pack the client
  refuses) but which costs a clone bytes: the cache keeps a pack a push, each
  completed by receive-pack with whole copies of the objects the push's
  deltas were made against, and multi-pack reuse sends as they are stored the
  whole copies the multi-pack-index keeps — 8.5–9% more over a history of
  pushes that add files and 48–57% more over one of pushes that edit them, for
  more of the node's CPU too (measured over copies the cache made,
  `internal/code/cache`'s `BenchmarkPackReuse`; the spread is which copy of
  each edited object a copy's index happened to keep).
- **A volume for `git.cache_dir`.** The cache holds every repository the node
  serves, whole, and a cold sync writes everything it downloads at the speed
  the object store delivers it — so a volume of its own is worth it: its own
  filesystem with its own space, never a btrfs subvolume or a ZFS dataset
  drawing on the store's pool, which shares that pool's space and is read as
  shared. Its derived budget is then seven eighths of the volume (see
  [`git`](../concepts/configuration.md#git)). On a volume it shares, the cache
  first leaves an embedded broker storing its streams there everything that
  broker may still write under its limit — three quarters of the volume's free
  space unless `stream.store_max_bytes` says otherwise — and takes half of what
  is left, so on the single-volume layout (the store, the streams and the cache
  under one directory, as [the example](../getting-started/configuration.md)
  has them) a first boot gives the cache an eighth of the free space, settling
  near a fifth as the broker re-derives its own limit from what the cache has
  since filled. Size for that, or set `stream.store_max_bytes` and
  `git.cache_max_bytes` to divide the volume yourself; the node says what it
  chose, and every figure it chose from, as `code_cache_sized`, at warning
  level when the budget comes out under 1 GiB. Where the broker's claim and the
  request spools (64 MiB for each of `git.max_transfers`) leave no room at all
  — on that layout, a cold cache whose quarter of the free space is no more
  than the spools, as under 16 GiB free on a sixteen-core node — the node
  refuses to host code rather than fill the volume the store lives on, and the
  error names the figures and each setting that gives the cache room. Behind
  the serving, the node also reads every byte of each repository it serves
  against its checksums — once after it restarts, then once a day while the
  repository is used, one repository at a time — which is what finds a pack or
  an index the disk damaged after it was written. It reads 1.13 times the
  repository's pack bytes — about a second a gigabyte from the page cache, and
  at the volume's own throughput read cold — so for 1 TB of packs all used
  every day, at most 2.5 hours of a gp3 volume's 125 MB/s baseline a day
  (10.5% of it) for the share read cold, and nothing for a repository nobody
  reads. It is
  never backed up, and a backup tool of your own skips it with
  `--exclude-caches`.
- **Nothing to set for `vm.max_map_count`.** A git node reads the runs of its
  indexes of which push introduced each commit through memory it maps from
  their files, and the kernel counts every mapping of the engine's process
  against `vm.max_map_count`, one setting of the whole host — a pod cannot set
  its own — past which the process can map nothing, its own heap included,
  and stops. So the node holds at most a quarter of the count mapped (16 382
  runs at Linux's default 65 530), reads the count at boot and names its
  bound as `mapped_runs` on `code_cache_opened`; a host that raised the count
  for something else raises the bound with it, and one that lowered it below
  the default lowers what every process on it may map, this one's bound
  included ([Which push landed a commit](../concepts/code-host.md#which-push-landed-a-commit)).
- **A writable `$TMPDIR` short enough for a socket.** Every push's git hands its
  ref updates back to the engine over a unix socket made under `$TMPDIR`, whose
  path is held to 100 bytes (darwin takes 104, Linux 108): a node whose
  `$TMPDIR` is longer refuses to start the host, and says to set a shorter one.
  The engine passes the socket's path and a one-time token to the hook as
  `CREWLET_HOOK_SOCKET` and `CREWLET_HOOK_TOKEN`
  ([Environment variables](../reference/environment-variables.md#set-by-the-engine-never-by-you)).
- **A balancer that does not cut a quiet git request.** git keeps a transfer
  alive by itself while `upload-pack` counts, and the engine writes a
  keepalive every 5 s on a push from its hook onward — so an idle timeout of a
  minute or more on `/git/` is enough for both. Neither keepalive covers the
  silence **before a request's first byte**, which has two parts. The first is
  a cache sync: warm, a few
  milliseconds; on a node that has never served the repository, or whose budget
  evicted it, a **cold** sync downloads one object for every push the
  repository ever took that carried objects — at most 2 048 of them, the live
  packs a repository may hold
  ([the pack bound](../concepts/code-host.md#limits)), since 10 000 would take
  about 70 to 130 s from S3, which answers each request in about 100 to
  200 ms — and on a node bringing up
  hundreds of repositories at once it first waits its repository's turn to
  read the rows, eight a node at a time
  ([many repositories at once](../concepts/code-host.md#the-cache-on-a-git-node)):
  on a node joining with fresh caches the downloads outlast it once its
  repositories hold more than a handful of packs, and on a node restarted over
  the cache it kept, which downloads nothing, it is the whole sync — about four
  seconds for 500 repositories asked at once, each a full page of refs from a
  data node of eight cores, which scans a full page in about 60 ms. The first
  sync of each
  repository after the node restarts also holds that repository's index files
  — about a quarter of its pack bytes — to their checksums before it answers:
  0.2 s a gigabyte of packs from the page cache, and at the volume's own
  throughput read cold — 39 s for the largest repository the defaults admit
  (20 GiB) at a gp3 volume's 125 MB/s baseline, shorter in proportion on a
  volume provisioned faster. The second is the hold on a
  fetch's answer until its pack begins: the host holds upload-pack's answer so
  that a cache its `pack-objects` finds damaged can be repaired and the fetch
  served, rather than failing it past the first byte. For a client using
  side-band (every git since 1.6) that wait is at most a progress line (about a
  second) or one upload-pack keepalive (5 s) — pack-objects' first word, unless
  that word is an error it reports, which holds the answer until the process
  ends — and a client that asks for no side-band waits until the raw pack's
  first byte.
  Either size the idle timeout to the cold sync of your largest repository, or
  route `/git/<name>` by repository — a balancer hashing the path to a node,
  with the next one in line when it is down — so each repository stays warm on
  one or two nodes. Routing by repository is never required for correctness.
  All of it holds for a coding box's `/box/git/<name>` on the public
  listener's balancer as well: a box's clone meets the same cold sync and
  the same holds, so give that balancer the same idle timeout, or route the
  box prefix by repository the same way.
  A fetch large enough to spool — a partial clone's checkout asking for more
  than a mebibyte of objects — that finds the node's room for such requests in
  use is silent too: it waits up to 55 s for its turn, a keepalive interval
  inside the minute, and is answered 503 if its turn has not come, so an idle
  timeout of a minute lets the node's own answer through rather than cutting
  the request with the balancer's.
  A client that does not ask for side-band (every git since 1.6 asks, so this
  is a hand-built client) hears nothing during a push from the end of its
  upload to its report: git's own keepalive (`receive.keepAlive`) and the
  engine's travel in side-band only. That stretch is receive-pack indexing and
  checking the pack, then the engine scanning it, storing it, proposing the
  push and waiting for its row. For such clients keep the idle timeout at 3
  minutes or more, whatever the push's size: the proposal is given two minutes
  — a minute's write attempt at the repository's committer and another at its
  runner-up, should the committer stop answering — and a row that resolved
  pending a minute more. A large push adds what its pack's own work may take
  before the engine gives it up: 10 s and a second a mebibyte for git's
  indexing and for each of the engine's three scans of the pack (a coding
  box's push four: its commits' committers are read too), and a minute and
  30 s a mebibyte (the floor every transfer is held to) for the object store's
  put — about 34 s a mebibyte in all, 35 s for a box's. A push into a large
  repository adds what walking its history may take: a commit dated before
  its parent — a clock that is behind — walks every commit of the repository in
  receive-pack's own connectivity check, and, until the node has written the
  repository's commit-graph, in each of the engine's listings and each branch
  update's force check too, and each such walk is given 11 µs an object of the
  repository: 33 s at three million objects, a gigabyte of source history,
  which such a walk takes about two seconds to finish.
- **A token for each CI runner.** A fetch large enough to spool — a blobless
  checkout's lazy fetch, past a mebibyte of wants — holds its room from its
  first spooled byte until git has read it and started answering, its sync
  included, and one credential's may hold a quarter of the node's room at
  once: 256 MiB on a four-core node, each up to 64 MiB, about 1.3 million
  objects. Fetches past that quarter wait their turn behind the token's own,
  up to 55 s each, and a lane's width of git transfer slots of them may wait
  at once — 16 on a four-core node, past which the excess is answered 503 at
  once. How long they wait is how long the ones ahead of them hold their
  room: a moment on a node that holds the repository, and on one that does
  not the cold sync above with each fetch's pack-objects — which can outlast
  the wait, and the wait stays 55 s whatever idle timeout the balancer is
  given, so on a cold node where it does the fetches past a token's quarter
  are answered 503, and git does not retry a lazy fetch, so those checkouts
  fail. N runners behind one token, each asking for
  B bytes at once, meet that once N × B passes the quarter: thirteen lazy
  fetches of 20 MB fill a four-core node's 256 MiB, and the fourteenth and
  every one after it fail on such a cold sync. Give each runner a token of its
  own, and the limit is the node's whole room — fifty-three such fetches on a
  four-core node's gibibyte, which a cold sync can fill too; routing by
  repository, above, keeps the syncs warm. A fetch whose body is still
  arriving holds no turn: it holds what its client has sent, and the node
  keeps one fetch's 64 MiB free of those behind the oldest still arriving, so
  a slow client holds others up only once the fetches arriving hold all but
  that of the room — 960 MiB of a four-core node's gibibyte, 192 MiB of a
  token's quarter. `crewlet.code.host.spool.waits` by `outcome` shows the
  waiting ([Metrics](../reference/metrics.md)), and a
  `code_fetch_spool_unserved` line names the token behind any that was not
  served and what held it back — git itself prints only
  `error: RPC failed; HTTP 503`.
- **Memory for its transfers, cores for its reads.** Each clone, fetch or push
  holds a few hundred MiB while it runs (224 MiB at the defaults, plus about
  240 bytes for every object of a repository it clones whole) and a core only
  now and then, so `git.max_transfers` defaults to four a core and no more than
  one for every 512 MiB of the node's memory; reads and background indexing
  take a core each, `git.max_processes` of them. `crewlet.code.git.wait` by
  `class` is the signal: a `transfer` wait climbing is a node serving as many
  transfers as its memory backs, a `read` wait one out of cores
  ([Metrics](../reference/metrics.md)). `crewlet.code.git.share_wait` by
  `share` says whose transfers wait: `machine` climbing while `person` stays
  flat is CI waiting behind CI, which the node's
  [fair share](../concepts/code-host.md#fair-share) does on purpose, and both
  climbing is a lane too narrow for its traffic. `crewlet.code.git.cpu` by
  `class` is what each lane's work costs — every git process's CPU, its
  children's included, counted whole as the process ends. A clone can run for
  hours and a background index for a quarter of an hour, each landing in the
  minute it ends in, so read its sum over hours, as the cost of a lane's work,
  and never as how busy the node is now: that is the container's own CPU usage.
  A transfer lane full while the node's CPU is low is transfers waiting on
  something other than cores — clients slow to read, pushes waiting on the
  code log or the object store — and one full with the CPU high is a node
  short of cores.

### Sizing git nodes

Size ONE git node for publishing, and every other for serving. A repository's
bitmapped index is written once, by its
[maintainer](../concepts/code-host.md#who-maintains-a-repository), and
installed by every other git node in a few MiB of memory: the maintainer is the
first git node of the repository's order whose background budget — what the
node can give its background work, which every git node advertises on its
presence heartbeat — holds every step of the repository's maintenance: about
840 bytes of memory an object of the repository for its publication (600 for
the bitmap, 240 for a clone beside it;
[Memory](../concepts/code-host.md#the-cache-on-a-git-node)), and under some six
million objects more for a merge of every object — 790 bytes an object beside
its delta window, six of the largest blob `max_blob_bytes` admits, 300 MiB by
default. On a fleet of
alike git nodes that is each repository's home, and a fleet whose repositories
every node can publish needs nothing more. Where one repository's publication
is past every node of ordinary size, give one git node the memory for it: every
repository only that node can hold is published there, whatever its home, and
the rest stay with their homes. A repository at the default `repo_max_bytes`,
20 GiB or some 60 million objects of a source repository's history, needs
about 50 GB: the LARGE git node — 96 GiB of memory and 16 cores, whose
background budget at the default lanes is about 68 GiB once it keeps 224 MiB
for each of its 64 transfers and 896 MiB for each core — publishes it with
room to spare, and each routine publication of it, a layer over a base
charged 350 bytes an object rather than the root's 600, reserves about 35 GB
of that budget while it builds, and one such node is all the fleet needs for a bitmap of it
everywhere. A repository NO git node can hold is still
served, without a bitmap for what was pushed since its last publication, and
its home raises [`code_maintenance_refused`](../reference/alarms.md), its
`code_cache_maintenance_refused` line naming the repository and the charge.

The node that publishes also holds:

- **Every repository it maintains, in its cache.** Each merge and publication
  syncs the repository first, so give that node a cache budget
  ([`git`](../concepts/configuration.md#git)) that keeps the repositories it
  maintains warm; otherwise each publication begins with a download of the
  whole repository — 20 GiB at the default `repo_max_bytes`.
- **About ten large repositories at their duty cycle.** It publishes one
  repository at a time, each at most every ten times its last build, so past
  about ten repositories whose builds take minutes each waits for the others,
  its unpublished tail growing meanwhile
  (`crewlet.code.publish.duration` against how often each is due). Give a
  second git node the same memory, and the repositories spread over the two,
  each in its own order.
- **Its uploads' buffers.** A publication uploads up to sixteen parts of its
  layer and sixteen outputs of its repack at once, and a merge beside it up to
  sixteen outputs: on an `s3` store an 8 MiB first part each, 384 MiB at most,
  256 MiB of them one publication's — out of what the node keeps back for
  serving and the store, not its background budget.

### Running checks on git nodes

Every git node runs the company's [checks](../concepts/code-host.md#who-runs-a-check):
each claims the queued attempts its repositories' git order ranks it first
for, and a box of the company's sandbox catalogue runs each. A `container`
check is a container on the git node's own machine — a repository's build or
its tests, beside the code host's git — so **`git.max_checks`** bounds how many
run there at once:

```yaml
git:
  max_checks: 4      # unset: a quarter of the node's cores, at least one; 0: none
```

- **Unset**, a node runs one container check for every four cores
  (GOMAXPROCS), and at least one: a check runs as many processes as it is let
  — `go test ./...`, `make -j` — so four cores a check leave the node's own git
  lanes (`max_processes`, a core a slot) three quarters of the machine, and a
  parallel build room to be parallel. A node whose checks are most of its work
  raises it; a node that should serve git and nothing else sets **`0`**, and
  the fleet's other git nodes run its repositories' container checks two
  seconds later, as the next of each repository's order.
- **An `e2b` check runs on E2B's machines** and takes none of these; what a
  company lets its remote checks spend is its own configuration's.
- **A node needs `CREWLET_BOX_GIT_URL`** to run any check — the public
  listener with `/box/git` on the end, where a check's box clones its commit,
  and whose `/box/code` sibling a `checkout: none` check reads it at. A node
  without it says `code_checks_not_run` once and runs none.
- **`crewlet.code.checks.running{placement="container"}` pinned at
  `git.max_checks`** while [`code_checks_backlog`](../reference/alarms.md)
  stands is a fleet whose container cells are too few for its checks: raise
  `max_checks` where the cores allow it, or add a git node.

---

## Observability

### The event store

Crewlet persists every engine event (LLM invocations, task lifecycle, agent
states) to the `crewlet_events` table in the same file as everything else.

There is **nothing to set up**: the table is created by the engine's own
migrations on first start, on whichever path `store.path` names. No extension
to enable, no managed service to configure, no separate retention system.

The engine registers the event-store writer as a **publish listener** on the
event queue. Every event is written at publish time, inline on the node that
published it — no queue round-trip and no consumer group, which is precisely
why two nodes can never write the same row and a group rebalance can never
lose one. Events land with dedicated columns for the common filterable
dimensions (`event_type`, `source`, `category`, `agent_id`, `agent_role`,
`task_id`, `channel_id`, `sender`, `trace_id`) plus a JSON column for
everything else.

That inline write is also why a fleet's event store is *per node*: each holds
what it published. A history read is therefore asked of **every live node**
at query time — the node serving the dashboard reads its own store and
scatters the same question to its peers, merges what comes back, and names
any node that did not answer in the answer's `coverage` (see
[Reading the fleet's history](../concepts/event-system.md#reading-the-fleets-history)).
A node that has LEFT the fleet takes its turn-level detail with it; the
aggregates — spend, turn counts, page reads — survive it in the replicated
`usage` domain. A deployment that wants to keep every node's detail past its
departure exports to an external sink over OTLP rather than pointing the
nodes at one database, which the exclusive file ownership rules out by
construction.

#### Custody: the rows of a node without `data`

A node without the `data` role writes none of its own. Its store is deleted
at every boot, so rows written there would be gone at its next restart with
nothing able to read them in between (the API runs only where the data is).
So it hands every event it would have written to the data nodes, and **exactly
one** of them keeps each one:

1. **It publishes them durably.** Events are buffered for at most a second —
   a turn never waits on its own audit trail — and published in batches of up
   to 256 events or 1 MiB onto the custody topic, `crewlet.custody.records`,
   on the `CREWLET_CUSTODY` stream. Once the broker acknowledges a batch it
   survives the node that published it; a batch it could not publish is sent
   again under the same id, which the broker collapses. The buffer holds 4 096
   events while the broker is unreachable, and an event that finds it full is
   dropped and counted (`event_custody_dropped`) rather than holding a turn.
2. **The data nodes take them from one group,** `event-custody`, and each
   writes the batch it takes into its own event log, where its `GET /events`
   and the fleet's history reads find it.
3. **One data node keeps each batch.** The group delivers a batch whose
   acknowledgement was lost again, to whichever data node asks next — so after
   writing a batch a data node *claims* it in the coordination store,
   create-only. The claim's winner keeps the batch; any other node that wrote
   it deletes its copy. A batch is acknowledged only once its keeper is
   decided, and a data node that crashed between writing and claiming settles
   its copy the same way at its next boot.

That last step is what keeps every figure derived from a node's own log
honest: the `usage` domain sums each node's day, so a batch two data nodes
kept would bill a stateless node's spend twice. And a node's day counts only
what it keeps: a batch it has written and not settled counts in none of its
days, and one it settles as kept is counted from then on — on the day its
rows fall, which the node publishes again however far back that is, within
the event log's 30-day read floor
([Replication](replication.md#two-compacted-domains-the-embeddings-and-each-nodes-day)).

Until the node that lost the claim has settled its copy — at once when its
claim is answered, and otherwise at its next pass, a minute later or as soon
as it can reach the coordination store again — both logs hold the batch's
rows. The fleet's history reads hold such a row once, wherever they list rows
and wherever they count them — the event axis's bars and category counts, a
trace's or a turn's total, a page of turns' tokens, and the integrations' drop
and merge counts: each data node counts the rows it keeps and names the ones it
has not settled, and the node you asked counts each named row once
([Reading the fleet's history](../concepts/event-system.md#reading-the-fleets-history)).

**Upgrade the data nodes first.** A data node writes each event in a batch by
its own build's category map, and leaves out a type its build does not place —
so a node without `data` on a newer build can hand a data node still on this
one events it cannot keep, and they reach no event log. With every data node
upgraded before the nodes without `data`, nothing is left out.

#### What gets stored, and under which category

`category` is the one column with a closed vocabulary, and it is what the
dashboard's filter and `GET /events?category=` group by. It is a property of
the **event type**, fixed in `internal/events`, and this table is generated
from that map — a guard test fails if the two drift.

| Category | Event types |
|---|---|
| `a2a` | `a2a_channel_closed`, `a2a_channel_opened`, `a2a_message_sent` |
| `auth` | `iam_credential_minted`, `iam_credential_revoked`, `iam_grants_changed`, `iam_invitation_cancelled`, `iam_login_failures`, `iam_mfa_reset`, `iam_password_changed`, `iam_password_reset`, `iam_password_reset_issued`, `iam_recovery_code_used`, `iam_second_factor_throttled`, `iam_session_ended`, `iam_session_generation_bumped`, `iam_session_started`, `iam_stepup_completed`, `statelog_record_tampered`, `statelog_record_unverifiable` |
| `decision` | `contribution_received`, `contribution_requested`, `decision_requested`, `decision_resolved` |
| `learning` | `compaction_completed`, `compaction_requested`, `counterparty_profile_updated`, `episode_written`, `knowledge_read`, `persist_decider_completed`, `prefetch_summary`, `reflection_completed`, `skill_archived`, `skill_promoted`, `skill_refined`, `skill_revived`, `skill_staled`, `skill_synthesized`, `skill_used`, `turn_completed` |
| `lifecycle` | `backup_requested`, `config_revision_activated`, `config_revision_applied`, `operator_acted`, `org_started`, `org_stopped`, `seat_paused`, `seat_resumed` |
| `notification` | `external_notification`, `notification_skipped`, `notifications_coalesced`, `turn_trigger_skipped` |
| `system` | `agent_phase_completed`, `agent_phase_started`, `agent_turn_completed`, `agent_turn_started`, `agent_turn_steered`, `agent_turn_stopped`, `auxiliary_spend`, `budget_exhausted`, `change_merged`, `change_opened`, `code_check_finished`, `code_fetched`, `code_queue_ejected`, `code_target_broken`, `llm_unavailable`, `phase.tool_skill_blocked`, `prompt.size`, `provider_fallback`, `review_submitted`, `skill_telemetry_write_failed`, `subagent_batched`, `turn.guard_breach` |
| `task` | `sandbox_clarification_requested`, `sandbox_run_answered`, `sandbox_run_completed`, `sandbox_run_failed`, `sandbox_run_started`, `scheduled_task_fired`, `task_assigned` |
| `webhook` | `inbound_delivery` — one row per delivery presented to one seat, from a [webhook route](../reference/api-endpoints.md) or a [Mattermost socket](../integrations/mattermost.md#running-on-a-fleet). The row is filed under the delivery's own label (`webhook:<event>`, `forge:<event>`, `socket:posted`) rather than under `inbound_delivery`, with the provider's exact bytes as its payload |

**The map is also the admission list.** A type that is not in it is not written
and does not reach the activity feed — so the exclusions below are
deliberate and each one says why, and a *new* type that nobody placed fails a
test rather than vanishing quietly.

Each exclusion carries a **cause** as well as a reason. The reason is for you,
when you come looking for a type and do not find it; the cause is the same
decision as a value, so the rule below can be checked rather than remembered.

| Excluded type | Cause | Why |
|---|---|---|
| `agent_turn_progress` | Intermediate state | Fires as each LLM round opens, answers and runs its tools, as a live-only signal; the matching `agent_phase_completed` is its durable record, so persisting this would fill the log with intermediate states of rows it also holds finished. It still drives the live projection. |
| `agent_spawned` | Placement | Placement moves a seat between nodes on every rebalance, so a durable row per claim would fill the log with a fact about **scheduling** rather than about the company. It still drives the live projection, which is what asks "is this seat running, and where". |
| `agent_terminated` | Placement | The counterpart, excluded for the same reason. It is what takes a released instance's call off a live screen rather than leaving it showing whatever it last did; whether the seat still runs anywhere is the seat leases' to say. |
| `raw_webhook` | Already recorded | The delivery is **already** a row (`inbound_delivery`, in the `webhook` category above). This event is the wake an inbound edge publishes for the transports to route, so categorising it too would store every delivery twice — once as what arrived and once as what was forwarded. |
| `a2a_request` | Already recorded | The ask is **already** a row: `a2a_channel_opened` and `a2a_message_sent` record the same exchange under the ids the audit trail is keyed on. This event is the wake it puts on the target seat's inbox — same reason as `raw_webhook`. |
| `a2a_message` | Already recorded | The answer is **already** a row (`a2a_message_sent`). This event is the wake it puts on the requester's inbox. |
| `sandbox_answer_given` | Already recorded | The wake an [answer by turn](../concepts/code-sandbox.md#answering-a-parked-run) puts on the seat's inbox, and never a turn. What the answer became is **already** a row (`sandbox_run_answered`), and that a person gave it is their `operator_acted` row — same reason as `a2a_request`. |
| `tool_skill_page_changed` | Already recorded | A **nudge** between nodes that one tool-skill page moved, so every node's registry re-reads it rather than only the node that won the webhook. The delivery that caused it is **already** a row (the `webhook` category above), and what the change did is a log line on each node, so a durable row would record one wiki edit once more per member of the fleet. |
| `operator_tools_changed` | Already recorded | A **nudge** between nodes that what one node's [operator catalogue](../reference/api-endpoints.md#operatormcp--your-own-assistant) is built from changed, so every node tells the MCP clients listening on it to list their tools again — the listing may reach any node, and the last nudge follows the last node's change. The apply that moved the catalogue is **already** a row (`config_revision_applied`), so a durable row per node per change would record one revision once more per member of the fleet. |
| `reflection_due` | Already recorded | The **wake** that puts a finished turn in front of [post-turn reflection](../concepts/agent-learning.md) on the seat's holder. The turn is **already** a row (`turn_completed`), and what reflecting on it did is its own (`reflection_completed`) — same reason as `a2a_request`. |
| `custody_batch` | Already recorded | A **carrier**, not an event: a node without the `data` role keeps no event log, so it publishes its events in batches and one data node writes each event inside as the row it is ([custody](#custody-the-rows-of-a-node-without-data)). A row for the batch would describe the transport and repeat every event in it. |
| `budget_meters` | Periodic snapshot | A **snapshot** of the shared token counters, published by every node on a 15-second tick and at once when a budget window first refuses a charge, so a durable row per report is about two million a year per node to answer a question the live projection and `GET /budgets` answer for free. What the audit log holds instead is the spend the counter is charged with, recorded per phase in the `agent_phase_completed` rows and per auxiliary purpose in the `auxiliary_spend` rows every spend query folds, so "what did we spend last month" is answerable and "what was the counter reading at 14:03:15" is not a question anybody asks. It still drives the live projection. |

**Nothing an anonymous caller can pace is written here.** Every categorised
type also records who authors its rate — this company's own work (`engine`), an
admitted caller such as an operator's token, a seat or a signed webhook
delivery (`authenticated`), or anybody who can reach the listener with no
credential at all (`anonymous`). Nothing is in that third group, and the rule
is that nothing may be: this file is on a disk you sized, and every backup,
every snapshot a lagging node installs and every integrity check taken before
either counts is a copy of the whole of it. A failed login is the case to hold
in mind — anyone who can reach the API can author millions a day for free — so
the per-attempt fact is the `crewlet.auth.attempts.failed` counter on the
[metrics](../reference/metrics.md) exporter, and what becomes a row is
`iam_login_failures`: one per node per minute, carrying a count per source and
never what was presented, published by the engine's own flush loop and therefore
paced by the engine. A type kept out for this reason is excluded with the cause
`anonymous_rate`. The rule is enforced by a test over the taxonomy, not by
review.

Two facts the identity design names as deliberately unrecorded have no event
type at all rather than an excluded one: a per-request **authorization
decision** (orders of magnitude more voluminous than everything else here, and
a fact about a poll rather than about the company — a Tier A token refused by
a route is a WARN log line on the node it reached instead) and a **session
touch** (a clock rather than an event; a re-issue moves a deadline inside the
cookie's own signature, so an hour of use writes nothing anywhere). The
exclusion table lists types something publishes, and nothing publishes either
of these.

**Stored is not always fed.** One class of type is written like every other row
and kept out of the dashboard's activity feed — a ring of the whole company's
last few hundred events, since every node's feed is fed fleet-wide. It is in
`GET /events`, a turn's history and a trace like any row, and filterable by its
category.

| Stored, not in the activity feed | Why |
|---|---|
| `auxiliary_spend` | **Accounting, not activity**: what the auxiliary model cost for one key, coalesced per flush ([Budgets and spend § Auxiliary spend](budgets-and-spend.md#auxiliary-spend)). A turn writes several beside its own phases, and a compaction a burst, so in the feed they would push the turns, failures and deliveries a reader is watching out of the ring. |
| `code_check_finished` | **Counted, not watched**: one attempt of a check of a commit that ended, as its row on the code log keeps the ending, published once by the data node that wrote it ([Code Host § Who records a check](../concepts/code-host.md#who-records-a-check)). Every push of every change ends each check its repository defines, faster than anybody reads a feed. How a change's checks ended is read where it is asked — the change's checks, the repository's. |
| `code_fetched` | **An audit, not activity**: one reader's reads of a `visibility: unit` repository's contents — fetches whose answers carried a pack, or a code tool's answers — coalesced per fifteen-second flush, with how many and their bytes ([Code Host § Who read it](../concepts/code-host.md#who-read-it)). A busy company's readers — a CI token fetching on every build, seats reading files a line range at a time — write a record each every flush, faster than anybody reads a feed. Who read a repository is answered by `GET /code/repos/{name}/readers` and by the operator catalogue's readers tool, summed across the fleet. |

#### Querying events

The dashboard's [Event log](../reference/dashboard-design.md#information-architecture) is the
intended reader — filters, traces and event detail, over the same
`/ws/stream` query channel the REST routes use, so both surfaces answer from
one implementation.

For ad-hoc SQL, point any SQLite-compatible client at `store.path` while the
engine is stopped, or use the read-only endpoints under
[`/events`](../reference/api-endpoints.md) while it runs. Do **not** open the
file with a second writer against a running engine.

### Tracing

Crewlet uses **OpenTelemetry** for distributed tracing. Every event carries W3C Trace Context fields (`trace_id`, `span_id`, `parent_span_id`) that propagate automatically through the system.

#### How Traces Flow

```mermaid
flowchart TD
    A["<b>webhook.receive</b><br/>the delivery arrives, and roots the trace"]
    B["the wake is published to the seat's inbox<br/><i>trace rides in the event envelope</i>"]
    C["<b>agent.turn</b><br/>the dispatcher restores the trigger's trace"]
    D["<b>agent.turn.onboarding / .execute / .review / .judge</b><br/>one span per phase"]
    E["<b>llm.round</b><br/>one per model round trip"]
    F["<b>tool.call</b><br/>one per call, including the refused ones"]
    G["<b>agent.turn.resume</b><br/>a suspended run re-entering, days later"]
    A --> B --> C --> D
    D --> E
    D --> F
    C -.->|"suspend, then resume"| G
```

**Five span names, and that is the whole set.** `webhook.receive`,
`agent.turn` (plus `agent.turn.resume`), `agent.turn.<phase>`, `llm.round` and
`tool.call`, alongside `schedule.fire` for cron-started work.

Span attributes are deliberately thin: the seat, the phase, the model, the
round, the tool and its outcome, and token counts. Everything about what a turn
*did* — prompts and responses verbatim, tool arguments and results, the
decision — is already in the [event store](#the-event-store), and a span
carries what no event does, which is **duration**.

Only the LLM *round* is spanned, not the fallback chain and each member's
backend beneath it — that would nest a span per member tried and tell you
nothing you could act on. Which member answered is on the phase event.

**A suspended run is two spans, not one.** A [code sandbox](../concepts/code-sandbox.md)
run detaches: the phase returns, the process may exit, the seat may move node,
and the resume can be days later. A live span cannot survive that, so the
suspending span ends and the resume opens a new one under a reconstructed
parent — the wait shows up as the gap it actually is.

#### Dashboard Trace View

The dashboard groups a trace's events into a tree — reach one from any row that carries a `trace_id`, or paste the id into the search box:

- Root event (e.g., webhook) shown as the trace header
- Child events nested underneath with connecting lines
- Click `inspect →` on LLM turn events to view the full prompt/response
- Notification skip reasons shown inline (e.g., "not following this thread")

#### OTLP Export

To export traces to Jaeger, Grafana Tempo, or any OTLP-compatible backend:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```

That is the collector's **base** URL — the engine appends `/v1/traces` itself.
Do not include the signal path here; use `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`
when you need to give the full URL. Getting this wrong also reaches the sandbox
forwarder, which appends a signal path of its own, and the collector sees
`/v1/traces/v1/traces`.

When either is set the engine installs a batching exporter at startup and
flushes it during shutdown, after the drain, so the spans a shutdown itself
produces are exported rather than dropped. Without it, spans are still created
and their ids still reach every event, the event store and the dashboard's
trace view — there is simply nothing shipping them anywhere.

The full set of variables, including the protocol, the service name and the
sampling ratio, is in
[Environment Variables](../reference/environment-variables.md#opentelemetry-optional).
The engine's exporter and the sandbox OTLP forwarder read the **same** endpoint
and headers on purpose: a coding agent's spans land in the same backend as the
turn that started them, nested underneath it.

#### Correlating logs with traces

Every log line emitted inside a span carries `trace_id` and `span_id`, in all
three [log formats](#logging). So a span you are looking at in Jaeger and the
lines the engine wrote while it was open are joined by the same identifier:

```bash
crewlet run -log-format json | jq 'select(.trace_id == "4bf92f35…")'
```

Lines emitted outside any span carry neither field rather than carrying an
empty one, so a shipper indexing `trace_id` never sees a placeholder.

#### Querying Traces in the Event Store

The `crewlet_events` table stores `trace_id`, `span_id`, and `parent_span_id` as first-class columns, so trace queries are a direct column filter:

```sql
-- All events in a specific trace
SELECT event_time, event_type, source, summary, payload
FROM crewlet_events
WHERE trace_id = '<trace-id>'
ORDER BY event_time ASC;
```

The dashboard API also provides `GET /events/trace/{trace_id}` which returns all events in a trace ordered by timestamp.

### Logging

How loud a node is, in what shape, and where it writes, is Tier A:

```yaml
# crewlet.yaml (Tier A)
logging:
  level: info       # debug, info (default), warn, error
  format: console   # console (default), text, json
  stderr: true      # default. false needs a file below — see "The log file"
  file:             # optional: a durable copy, IN ADDITION to stderr
    path: "/var/log/crewlet/crewlet.log"
    format: json    # empty follows logging.format
    level: debug    # empty follows logging.level
    max_size_mb: 100    # rotate at this size (default 100)
    max_backups: 5      # rotated files kept beside the live one (default 5)
```

That block is the only way the file says it.

The same settings, on the command line, for one run:

```bash
crewlet run -debug                             # shorthand for -log-level debug
crewlet run -log-level debug                   # what -debug is shorthand for
crewlet run -log-level info -log-format json   # for a log shipper
crewlet run -log-file /var/log/crewlet/crewlet.log   # a durable copy
crewlet run -log-file ""                       # and no file, for one run,
                                               #   whatever the Tier A says
```

**A flag overrides the file only when it is actually given.** A flag carries
its default whether or not anyone typed it, so `crewlet run` distinguishes
"the operator asked for `info`" from "nobody said anything" — otherwise
`logging.level: warn` in a file would be dead on arrival behind the flag's own
default. `-debug` only ever *raises*: to quieten a node whose file says
`logging.level: debug`, pass `-log-level info`. `-log-file` obeys the same
rule in both directions: it replaces `logging.file.path` for one run, and an
explicit `-log-file ""` is how a node with a file configured is asked to write
none. It moves only the *path* — the shape and the rotation caps describe the
disk this deployment runs on rather than this invocation, so they stay the
file's. Each flag given is folded into the document the node runs, so the
level, the file and the [advisories every start
logs](../reference/cli.md#every-start-says-what-its-tier-a-is-worth-knowing)
all read one answer: a node started with `-debug` is not told the embedded
broker's debug output has nowhere to go — unless `logging.stderr: false` hands
the stream to a log file that keeps a quieter `logging.file.level` of its own,
which `-debug` does not raise, and then the line names that level rather than
`-debug`.

**The first lines of a run come out in the flag's shape, not the file's — and
before any log file exists.** The `${VAR}` warnings a Tier A document produces
are emitted while it is being read, so a node configured `format: json` writes
those few lines as `console`, on stderr only, before switching. The log file is
named *by* the document that is still being read, so it cannot be open yet.
That is the best a process can do about a file it has not opened, and it is
the right way round: `-debug` is turned on most often to watch the config load
itself fail, so the flags have to take effect first. If a boot fails on the
document itself, stderr is the only place it is recorded — start there before
the log file.

**What the document is worth knowing comes out after the file is open.** A
Tier A that is valid and carries a consequence — no backup owner, a declined
fsync, an acknowledged insecure sign-in posture — is logged at `WARN` on every
start as one `config_advisory` line per warning, with `config=bootstrap`, the
`path` the remedy is written at and the whole message as `detail`: the same
list `crewlet validate` prints, for every deployment nobody ran `validate` on.
Unlike the `${VAR}` lines above they come out once the document, with the
run's flags folded in, has passed every Tier A rule and the log file is
attached, so they are in it, and they are judged on that same document. A
deployment that has said everything logs none. See
[`crewlet run`](../reference/cli.md#every-start-says-what-its-tier-a-is-worth-knowing).

A value the build does not recognise is treated differently in the two
places, on purpose. In a **flag** it resolves to the default — a bad log level
must never be why a company will not boot. In the **file** it is refused, with
the field path, by `crewlet validate` and at boot: a flag is typed by someone
watching the process start, and a file is written once and deployed for
months, so a misspelled level there would run quietly at `info` for as long as
nobody looked. Either way the fallback is never *silent*: an unrecognised
`-log-level` / `-log-format`, or `$CREWLET_LOG_LEVEL` / `$CREWLET_LOG_FORMAT`,
logs a `log_level_unrecognised` / `log_format_unrecognised` warning naming what
was written, what the build used instead, and what it accepts.

**A log file is the one logging value that does not fall back at all.** A
level is an enum with a sane default; a path is not. A node that could not
open the file it was told to write *refuses to start*, naming the path and the
error, rather than running on stderr alone — an operator who configured a
durable record and silently did not get one has nothing anywhere pointing at
why. The same
applies to `$CREWLET_LOG_FILE` on the other commands. Once the node is up, a
file that *becomes* unwritable — a full disk, a volume pulled away — is the
opposite case and is handled the opposite way: the failure is announced on
stderr once, the console sink keeps every line, and the engine keeps running.
It is announced again when the file starts taking writes, so the gap has two
ends.

#### The three formats

| Format | For | Shape |
|---|---|---|
| `console` (default) | A person watching a terminal | Fixed columns — time, level, component, event — with attributes dimmed, and ANSI colour when the stream is a live terminal |
| `text` | Grepping without a parser | slog's `key=value`: `time=… level=INFO msg=seat_claimed component=seat.host seat=eng.alice` |
| `json` | A log shipper | One JSON object per line |

`console` adapts to its sink. Colour appears only on a live terminal, so a
redirected stream carries no escape codes it cannot render — and because a
redirected stream is read *later*, its lines carry the full date where a
terminal's carry the wall-clock time alone. `CREWLET_LOG_COLOR=always|never`
overrides the detection (for a CI viewer that renders ANSI without being a
terminal), and `NO_COLOR` suppresses it the way it does for every other tool.
A **log file is never a terminal**, so a `console`-format file is never
coloured and always carries the full date, whatever `CREWLET_LOG_COLOR` says —
the variable describes the screen someone is looking at, and nobody is looking
at a file.

#### The log file

`logging.file.path` adds a durable copy of the log. It is a **second
destination, not a redirect**: stderr keeps every line it had. That is
deliberate — stderr is the only sink that exists before the document naming
the file has been read, it is what a container platform captures, and it is
where a boot failure and the [watchdog's exit
notice](../concepts/seat-ownership.md) are written. A node that fell silent
there the moment a path was configured would look exactly like one that had
stopped. A deployment that genuinely wants the file alone redirects stderr in
its unit file or its container spec.

Because the two are separate destinations rather than one stream tee'd in two,
each carries its own shape — which is the point:

```yaml
logging:
  format: console     # columns and colour, for whoever is watching
  file:
    path: "/var/log/crewlet/crewlet.log"
    format: json      # one object per line, for the shipper
```

Leave `file.format` out and the file follows `logging.format`, so a node that
says nothing writes one log in two places.

**The level splits the same way, and both directions are real.** `file.level`
is how loud the *file* is; unset, it follows `logging.level`, so `-log-level`
and `-debug` move both destinations at once.

```yaml
logging:
  level: warn         # what a person watching sees
  file:
    path: "/var/log/crewlet/crewlet.log"
    level: debug      # what the incident is reconstructed from
```

A `debug` file behind a `warn` console keeps the detail an incident needs
without burying whoever is watching; a `warn` file behind a `debug` console
keeps the durable record small while somebody works. The process admits the
**louder** of the two and each destination filters, so `log.Enabled(…, debug)`
answers "will this be recorded anywhere" — meaning a `debug` file costs the
work at every debug call site whatever the console says. That is the price of
asking for a debug file, and it is paid whichever destination reads it.

#### Turning stderr off

`logging.stderr: false` silences the ordinary log stream on stderr once a file
has taken it over:

```yaml
logging:
  stderr: false
  file:
    path: "/var/log/crewlet/crewlet.log"
```

Use it where the platform already captures stderr **and** you keep a file —
journald plus a log file, or a container with a log driver plus a mounted
volume — because there every line is otherwise stored twice. Without it the
default stands: a file never silences stderr.

**The tradeoff it buys you:** with stderr off the file is the node's *only*
destination, so a file that becomes unwritable loses log lines rather than
diverting them. The engine still says so on stderr — the notice names the
file, the error, and that the lines are lost rather than continuing
elsewhere — but the lines themselves are gone until the file takes writes
again. Leave stderr on if a gap in the record is worse for you than storing
it twice.

**It is not `2>/dev/null`, and the difference is the point.** Three kinds of
line reach stderr without passing through the configured handler, and this
field keeps all three while a shell redirect throws them away:

| What | Why it bypasses the handler |
|---|---|
| Everything before the Tier A document is read | The log file is named *by* that document, so it cannot be open yet |
| The [seat watchdog's](../concepts/seat-ownership.md) exit notice | It writes to stderr directly and calls `os.Exit(75)`; a wedged process has not earned a configured handler. A broker that declared itself dead leaves by the same exit with code `69`, after an `embedded_broker_died` line that does go through the handler |
| "log file *X*: no space left on device" | A sink cannot report its own failure through itself |

**A terminal that is about to go quiet says so.** Before the switch takes
effect the engine writes one plain line to stderr naming the file it is
handing over to — not through the logger, and so not subject to
`logging.level`. That matters: the structured `log_file_opened` record is
ordinary telemetry at `info`, so on a `logging.level: warn` node it is
filtered, and without the plain line `crewlet run` would print *nothing at
all* with no way to discover the log was in a file.

**A node with neither destination is refused**, by name: `logging.stderr:
false` with no `logging.file.path` fails validation and fails the boot, and
so does a `-log-file ""` that takes the file away from a document that had
switched stderr off. Silence is never what configuring logging meant.

The other commands are unaffected — they read no `logging:` block, so their
stderr always stays on.

**Rotation is built in, and it cannot be turned off.** A log file with no
ceiling fills the disk the store is on, and it does it on exactly the
deployments nobody is watching — so there is no "never rotate" spelling, only
a size you will not reach. The live file rotates at `max_size_mb` (default
100) and the rotated ones are kept as `crewlet.log.1` (newest) through
`crewlet.log.N`, `max_backups` of them (default 5). Together the defaults
bound the estate at roughly 600 MB. `max_backups: 0` is a setting rather than
an absence: it keeps no history at all, which is what a small disk with a
shipper already tailing the live file wants.

The size is checked *before* the record that would cross it, never after, so a
record is never split across two files — half a JSON object at the end of one
file and half at the start of the next is a parse error in whatever is
shipping it. A single record larger than the whole cap is written whole into
an empty file rather than rotating forever around something that can never
fit.

Restarting **appends**; it does not rotate. A restart loop is precisely when
the previous incarnation's last lines are the evidence, and rotating on every
boot would push the first failure off the end of the stack by morning.

Missing directories are created, `0700`, and the file is `0600`. A log line is
redacted but it is not a public document, so a shipper running as another user
needs a `chmod` you make deliberately.

Already running `logrotate(8)`? Point it at the same path with
`copytruncate` — which keeps the descriptor this process holds — and give
`max_size_mb` a value this node will never reach. Both caps are bounded above
as well as below (`max_size_mb` at 1 073 741 824, a pebibyte; `max_backups` at
1000) and a value past either is refused by name: the size is held in bytes,
so a larger one wraps and would rotate on *every line* — the exact inverse of
what a huge number asks for — and the backup count is a rename per rotation,
so a huge one stalls the rotation instead of keeping more history. A rename-based logrotate
rule moves the file out from under the engine's open descriptor, and there is
no reopen signal to send it: the engine owns its signals for the graceful
drain (see [`crewlet run`](../reference/cli.md#crewlet-run)), and a third tier
of signal handling is not worth a mechanism this file already has.

**One file per node.** Two processes on one host — a split `ingress` / `seats`
deployment, or a node beside a `crewlet migrate` — pointed at one path will
interleave their lines and rotate each other's file, and nothing detects it.
Put the node id in the path, which resolves like any other Tier A `${VAR}`:

```yaml
logging:
  file:
    path: "/var/log/crewlet/${CREWLET_NODE_ID}.log"
```

Every line is structured whichever format is installed, and carries a
`component` attribute naming the subsystem that emitted it (`agent.turn`,
`mcp.client`, `seat.host`) — the field `console` promotes into its own column
— so a debug run stays filterable rather than becoming a wall.

The operator commands are quiet by default: they open a store, which logs a
line per migration, and that is noise on a one-shot command whose output is
meant to be piped or diffed. They take no logging flags — only `crewlet run`
does — so `CREWLET_LOG_LEVEL`, `CREWLET_LOG_FORMAT` and `CREWLET_LOG_FILE` are
their levers: the third appends a `crewlet migrate` or a `crewlet validate` to
the same durable record the node writes, which is the reason a CI step wants
any of them. It writes only what the command *logs*; whatever the command
prints for its caller still goes to stdout, so a piped or diffed output is
untouched. `crewlet run` ignores all three — its level, shape and file come
from Tier A and its own flags. See
[Environment Variables](../reference/environment-variables.md#logging).
Nothing silences a warning.

#### The embedded broker's own logs

The NATS server the engine embeds logs through the engine's logger, under the
component `queue.nats.server`, so what the **broker** said is always
distinguishable from what the engine said about it. Anything it reports as
wrong — a JetStream write error, a slow consumer, stream recovery after an
unclean shutdown, cluster election trouble — keeps its own severity and
reaches the log whatever else is configured. Its boot narration ("Starting
nats-server", the JetStream storage line, "Server is ready") is `debug`: a
dozen lines describing infrastructure you deliberately did not deploy.

One error line is `debug` too: the broker's own report of a connection it
refused for its credential (`authentication error - User "…"`), which it
writes once per attempt, carrying whatever name the peer sent. The engine
reports every such refusal itself — counted in `crewlet.broker.peer.refusals`,
and logged as `broker_peer_refused` once a minute a class, naming the key
([The fleet's broker admits only its own nodes](#the-fleets-broker-admits-only-its-own-nodes))
— so the broker's copy would be the same event twice, at whatever rate a peer
chooses. The dialling side keeps its level: a member failing to join a peer
(`Route Error 'Authorization Violation'`) is this node's own problem, and its
operator needs it.

Its **own debug output is a separate switch**, `stream.debug`, and it is off
by default:

```yaml
stream:
  debug: true     # only when the BROKER is what you are diagnosing
```

`logging.level: debug` and `-debug` say how loud the *engine* is. They are
what you want to watch a turn — the prompt, the tool calls, the review — and
they deliberately do not turn this on, because nats-server's debug output is
per *internal client* rather than per event, and the engine's own coordination
reads manufacture those continuously. Every coordination key listing is an
ordered consumer created and then deleted, and deleting one writes two lines
like:

```
DEBUG queue.nats.server  nats_server detail="JETSTREAM - JetStream connection closed: Client Closed"
```

Two of a node's fifteen-second duty loops list keys on every tick, so that is
a constant background stream on a node doing nothing at all. `Client Closed`
is the *graceful* close reason and nothing is leaking; it is simply the
broker narrating its own housekeeping.

Both switches have to agree for these to appear: `stream.debug` decides
whether nats-server produces them, and a destination at `debug` decides
whether anything records them. `crewlet validate` warns when the first is set
and the second is not, and so does every start, as `config_advisory` — judged
with the run's own `-debug` / `-log-level` folded in, so a node started with
`-debug` is not warned unless `logging.stderr: false` hands the stream to a log
file that keeps a quieter level of its own. The line names the cause it found
and only the changes that would record the broker: there, `logging.file.level`,
never `-debug`. `stream.debug` is **refused** for `stream.type: nats` —
an external cluster logs wherever its own operator configured it to, so a flag
here would reach nothing.

**The NATS client's own errors go through the same logger**, on every
topology, embedded or external. What the client reports outside any call —
a slow consumer, a permissions violation, a server refusing this node's
credentials on a reconnect — is `jetstream_client_error` at `WARN` under
`queue.jetstream`, carrying the client's own error and nothing read off the
subscription it came from: the client rewrites a JetStream consumer's
subscription under a lock of its own on every reset, and what that field names
there is a random inbox anyway. A permissions violation names the subject the
server refused in the error itself. Left to the client's default it was a bare
line written straight to stderr, outside `logging.file`, outside any level and
in the middle of a `json` stream; the credentials refusal that comes before a
[connection closed for good](#an-external-nats-server) was said only there.

#### A broker that declares itself dead

nats-server has one more severity, **fatal**, for a failure it cannot go on
from: JetStream refusing its store directory, a listener that cannot bind, an
advertise address it cannot read. A standalone nats-server exits the process
there. The embedded one does not — the engine owns the process — so the engine
decides what it means, and it means the same thing a standalone server would:

- **During the boot, the boot fails at once with the broker's own reason.**
  Nothing waits for a server that has already given up, and the reason is
  nats-server's own sentence rather than a summary of it:

  ```
  engine: stream: start embedded nats: embedded nats server could not start:
  nats-server: Can't start JetStream: storage directory is not a directory
  ```

  A clustered member is not told its route port was taken unless it was: a
  bind that failed on `stream.cluster.port` or `stream.leaf.port` because
  something else holds it is the one fatal reported as a taken port.
- **After the boot, the node ends.** It logs `embedded_broker_died` at
  `error`, with the broker's reason, through the ordinary log, then leaves the
  same way the [seat watchdog](../concepts/seat-ownership.md#a-broker-that-declares-itself-dead)
  does: a notice on stderr, and exit code **69** rather than the wedge's 75,
  so an orchestrator's restart log tells the two apart. It does not drain, as
  a node whose [connection closed for good](#an-external-nats-server) does,
  because every step of a drain — acknowledging a turn, publishing what it
  did, giving a seat's lease back — rides the broker that has just declared
  itself dead, and a declaration closes no connection for those steps to fail
  on at once. A node left running on it would answer `/health` while nothing
  it did could land; ended, it is restarted, and the restart recovers the
  broker from its store. The same reason on every restart is a setting or a
  disk to fix.

### Per-Agent Token Tracking

Every LLM completion records prompt, completion and total tokens plus a call
count, per agent and per model, with cache reads and writes broken out so
cached prefixes are visible rather than folded into the total.

Read them from the dashboard's **Settings › Token spend** and **Company › Budgets**, or
over the socket's query channel — `tokens` for the rollup and `budgets` for the
caps beside the durable counters the engine enforces against. Both are the same
functions the REST routes call, so the two surfaces cannot disagree.

```bash
crewlet budgets show      # the durable counters, read from a running node
```

It talks to a node rather than to a file: the counters are the fleet's, and on
the default topology they live inside the running engine. `-url` names another
node, and without it the address is taken from the `api` block of the config
on the command line, which is not read at all when `-url` is given; the
credential is `CREWLET_API_TOKEN`, never a flag.
There is no reset command: a window's allowance comes back when the window
turns over.

### Token Budgets

Set budgets at two levels, each a mapping of ceilings per calendar window —
`day`, `week` and `month`, each optional — cut on the company's
[clock](../getting-started/configuration.md#the-companys-clock):

- **Org-wide** — the company's `token_budget`, a setting changed by a
  revision through `/config`
- **Per seat** — an agent seat's `token_budget`, changed with the rest of the
  seat through `/config` — `PUT /config/roles/{handle}` for one seat

```yaml
token_budget: {day: 3000000, month: 40000000}
```

An absent window is uncapped, and a ceiling of `0` is refused rather than read
as unlimited. See [Configuration § Token budgets](../getting-started/configuration.md#token-budgets)
for the rules and for the ceilings `crewlet validate` warns can never bind.

Every model round is charged against both the moment its reply arrives (its
size is known no sooner) and before any tool it asked for runs, in every
window at once — the day, the week and the month it falls in on the company's
clock — and it is admitted only while every capped window of both had room for
it. A round that does not fit is refused: its tool calls do not run, the turn
stops, and the engine publishes a `budget_exhausted` event naming the scope that
refused, the window (`period`, `window`, `resets_at`) and its figures, beside
the turn's own `agent_turn_completed`. The refused round is **counted all the
same**, on the seat's counter and the company's, because the vendor has
already billed it: the refusing window reads past its ceiling by the round that
crossed it, and every later round is refused against that figure until the
window turns over or the ceiling is raised. Each scope's check is atomic — and
a node's charges on one counter are one write, decided in the order they
arrived, so the company's counter is contended by the nodes charging it rather
than by every round in flight — and nothing a charge counted is taken back: a
seat write that fails after the company's landed stops the round as an outage
and leaves it on the company,
whose record of a billed round is true, so only that seat's own counter is
short of it — logged as `coord_kv_budget_spend_uncounted` — until the window
turns over. In a fleet the counters live in the coordination
slot, so an org cap of 500 k is 500 k across every node rather than per
process.

A window's allowance comes back when the window turns over — at local
midnight, on Monday, on the 1st — rolled inside the first charge after the
boundary, so nothing has to run for it and no node has to be up at midnight.
There is no reset: room before a window turns over is made by raising its
ceiling, which takes effect on the next turn. See
[Coordination § Token budgets are windows](../concepts/coordination.md#token-budgets-are-windows).

**A seat out of room waits; its mail is not lost.** Before a delivery is
handed to a seat, the node asks whether one of that seat's capped windows —
its own or the company's — is refusing. If one is, the seat is **parked**:
its inbox is held, the delivery goes back to the broker for one of its
deliveries, and it is delivered again when the window turns over (the one
that ends last, where several refuse) or at once when a change to the
ceilings applies on that node — the revision that changed them. The node logs
`seat_budget_parked` with the window and when it resets, and
`seat_budget_park_released` when the mail flows again. A
turn refused part-way through is parked the same way, unless it had already
written outside the engine, in which case it is recorded and not run again.
See [Agent Runtime § The budget park](../concepts/agent-runtime.md#the-budget-park).

**Every seat is counted, capped or not.** A company that sets no ceiling
still has its spend on the counters, in every window, so a ceiling added
part-way through a day judges what the day has already spent rather than
starting from zero, and `GET /budgets` shows an uncapped company's spend
rather than nothing.

A refusal is also recorded beside the counter, as when the gate last turned a
call away in that window (`refused_at` on [`GET /budgets`](../reference/api-endpoints.md#get-budgets)
and on the [live token meter](../reference/api-endpoints.md#the-live-token-meter)),
and the next charge the scope admits clears it, as does the window turning
over. The gate turns work away in two ways, and both are recorded: it refuses
a round's charge, or — once a window is already full, because a context
assembly, a rewrite, a coding run, a person's answers or an earlier round took
it there — the work is turned away before its first call, since that call
would be billed and then refused. That second way is every refusal of a full
window the engine can see coming: a turn stops its next call, the
[budget park](../concepts/agent-runtime.md#the-budget-park) defers a seat's
delivery, a person's `answer_knowledge` question is refused, and the
reflection stage declines a pass and a conversation entry's rewrites. Each is
recorded on the scope a charge would be refused by (the company's before the
seat's), a turn's once per window for each part of the turn, so a window that
is refusing everything sent its way never reads as one that has refused
nothing. Because the counter carries every round it refused, `GET /budgets`,
the live meter and the park all read a window that refused a round **over** its
ceiling by the round that crossed it, never just short of it; `refused_at` is
when the gate last said no. A window filled with nothing asked of it since — a
background pass, a coding run whose turn has not resumed, and no delivery,
question or pass for that scope after it — carries no `refused_at` until
something is, though its `state` is already `refusing`.

No model call can be checked by its own size first, because its size is known
only once it has happened. A turn's round is judged when it is charged, as
above, because a verdict still has something to stop: the tools it asked for
and every model call of the turn after it, each refused before it is sent
rather than billed and refused in turn. Four other spends have nothing left to
stop by the time their size is known, so each is **post-charged** — added to
the counters in the windows it is recorded in, without a verdict — behind a
gate that reads the room left *before* it starts:

- **A coding run.** Its box spends while the turn is suspended, so its tokens
  are known only when the run is collected, and they reach both the seat's
  counter and the company's in the windows the run is collected in. A run that
  takes a counter past its cap is logged as `sandbox_spend_over_budget`.
- **An auxiliary pass** — the reflection, profiling, compaction and summary
  calls the [learning subsystem](../concepts/agent-learning.md) makes on a
  seat's behalf. A pass does not start for a seat or company with no room
  left, and each completion it makes is recorded in full on the seat's counter
  and the company's, past the ceiling included. The same gate stands in front
  of the rewrite of a [conversation entry's](../concepts/conversation-sessions.md)
  long tool payloads, made after a turn under the same reflection stage: with
  no room left the entry is still written, each such payload named by its size
  and digest.
- **A turn's own auxiliary calls** — the memory filter, the knowledge query and
  the episode summary the [turn-start prefetch](../concepts/agent-learning.md)
  asks the seat's auxiliary model for before the first phase opens, and every
  rewrite the turn's conversation block, ledgers, judge and tools need. Their
  gate is the turn's own: a delivery to a seat whose window has no room left is
  [parked](../concepts/agent-runtime.md#the-budget-park) before it runs, so no
  prefetch starts for it; each completion the turn does make is recorded on the
  seat's counter and the company's through the turn's own meter, which then
  holds any window the completion filled; and the turn makes no further call,
  auxiliary or not, while that window is full — except its task card's
  rewrite, the turn's record, made after its last round.
- **A person's knowledge answer** — the dashboard's ⌘K answer
  ([`answer_knowledge`](../reference/api-endpoints.md#answering-a-question-from-the-companys-knowledge)).
  It is a person's question rather than a seat's work, so it is gated on the
  **company's** windows alone — refused `budget_exhausted` before any model
  call when one of them has no room — and recorded on the company's counter
  alone, although it runs on the auxiliary model of the seat the asker is
  bound to (which is why a credential bound to no seat is refused it).

In each case the next round the seat or the company attempts is refused
against the recorded figure — and for a turn's own auxiliary calls, and for a
coding run its resumed turn collects, that round is refused before it is sent:
the turn's meter holds what the post-charge answered, or reads it once before
the resumed turn's first call.

### Structured Logging

Every significant operation emits structured log entries:

```json
{
  "time": "2026-03-12T10:30:00.412Z",
  "level": "INFO",
  "msg": "onboarding_phase_complete",
  "component": "agent.onboarding",
  "agent": "sarah-chen",
  "turn_id": "9f3c1e70-…",
  "marked": true,
  "rounds": 6,
  "chain": "…",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id": "00f067aa0ba902b7"
}
```

`trace_id` and `span_id` are present on any line emitted inside a span — which
is every line a turn produces — and absent entirely on lines that are not, so a
shipper indexing them never sees an empty placeholder. They are the same ids the
[tracing](#tracing) section exports, which is what lets you pivot from a slow
span in Jaeger to the lines the engine wrote while it was open.

The JSON key for the message is `msg`, slog's own, and its *value* is the
short, machine-parsable event name every line carries in place of a sentence.

### Reacting to events

Every state change in the engine is an event on the stream, so anything that
wants to react — dashboards, alerting, an external audit sink — subscribes
rather than polls. `/ws/stream` is the read surface for a client; nothing runs
inside the process to hook them, because the engine loads no plugins.

---

## Security Boundaries

- **Scope isolation** — agents can only access knowledge within their permitted scopes
- **Tool availability** — all registered tools available; per-role MCP tools carry role-specific credentials
- **Communication permissions** — agents can only post to channels they're members of
- **Manager handoffs** — agents identify their manager from their identity prompt and reach them through the colleague-surface tools (Slack/Jira/Confluence/A2A); engine-detected failures surface to the operator dashboard as the seat's `last_error`, and an unreachable provider as the seat state `stopped`/`provider`
- **LLM sandboxing** — tool execution results are validated before returning to the agent
