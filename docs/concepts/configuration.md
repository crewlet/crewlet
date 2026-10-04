# Configuration

Crewlet splits configuration into **two tiers** so a founder can evolve their company at runtime — add a role, swap an LLM provider, plug in a new MCP server, rotate an integration credential, update a policy — without redeploying or restarting the engine.

---

## Two-Tier Split

| Tier | Storage | Owner | Update model | Contents |
|------|---------|-------|--------------|----------|
| **A** | `crewlet.yaml` on disk | Ops / SRE | Restart-only | The store file, the stream and coordination slots, this node's identity and roles, API host/port and auth, the secret keyring, logging (level, shape and an optional rotating log file) |
| **B** | The store (`company_config`, versioned) | Founder | Live, API-editable, validated, versioned | Everything else: name, mission, vision, policies, the company's one clock (`timezone`), providers (LLM + embeddings), turn engine, learning, MCP servers, notification transports, integrations (Jira / Confluence / Slack / GitHub / GitLab / Forge), the org chart (`roles:` and `units:`), token budgets |

**Tier A** controls *how the engine boots*. **Tier B** is *what the company is*.

The company's clock is Tier B for that reason: *where* a company keeps its hours is a fact about the company, not about the hosts running it, and every node must cut the same day from it. So it is one top-level `timezone` in the company document rather than a host's own clock — `Local` is refused — and rather than a zone per subsystem: the tracker's "today", a person's day and the hour a schedule fires on are one calendar. See [The company's clock](../getting-started/configuration.md#the-companys-clock).

### The org chart is part of the document

`roles:` and `units:` are two keys of the Tier B document like any other, so a
hire, a move and a provider change are the same kind of write: a new revision,
activated fleet-wide through [the control plane](control-plane.md), and turned
into the running organization once per applied epoch on every node. Edit it
whole (`PUT` / `PATCH /config`) or one seat or unit at a time
(`PUT /config/roles/{handle}`, `PUT /config/units/{id}`) — see
[Per-entity read and write](../reference/api-endpoints.md#per-entity-read-and-write).

**A seat's handle and a unit's key are its identity, and they never change.**
Every write mints the ones a document leaves out — a seat's `handle` from its
`name`, a unit's `id` from its name — before the revision is stored. An edit
that starts from the stored document (the org builder, `GET` then `PUT
/config`, `PATCH /config`, the per-entity routes) carries them, so correcting a
name there changes what people read and nothing else: the seat keeps its agent
id, its mailbox and its memory. A **file** is minted afresh on every import, so
correcting a name in one that leaves the `handle` or `id` out replaces that
seat or unit — declare both in any file you keep. A different handle is a
different seat. See [Handles and keys](organization-model.md#handle-based-identity).

**Removing a human seat somebody holds is refused.** A revision that takes a
human seat out of the company — or turns it into an agent seat — while the
[identity directory](identity-and-access.md) binds a person to it is refused
`409 seat_held` naming them, at any stage short of their removal: unbind or
remove them first.

### Tier A example (`crewlet.yaml`)

```yaml
logging:
  level: info           # debug, info (default), warn, error
  format: console       # console (default), text, json
  stderr: true          # default. false hands the stream to the file below,
                        #   and needs one — a node logging nowhere is refused
  file:                 # optional — a durable copy, IN ADDITION to stderr
    path: "/var/log/crewlet/${CREWLET_NODE_ID}.log"
    format: json        # empty follows logging.format
    level: debug        # empty follows logging.level
    max_size_mb: 100    # rotates here; there is no "never" (default 100)
    max_backups: 5      # `.1` (newest) … `.5`; 0 keeps none (default 5)

node:
  id: "node-0"          # optional; see below

stream:
  type: embedded        # a JetStream server inside this process; `nats` points
                        #   the same slot at an external server or cluster
  store_dir: "./crewlet-data/stream"

store:
  path: "./crewlet-data/company.db"   # ONE file, owned exclusively

coordination:
  type: local           # one node; a fleet needs `embedded-kv`

api:
  host: "0.0.0.0"
  port: 8000
  auth:
    tokens:
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"
      - id: ops
        token: "${CREWLET_API_TOKEN_OPS}"
```

#### `node.id`

Names *this process* within the company. It labels every log line and the
`/health` payload — the difference between "a config apply failed" and "the
config apply failed on `node-2`" the moment more than one process is
running, and the only way a caller behind a load balancer can tell which
process answered.

Resolution order: `node.id` (`${VAR}` references work here like anywhere
in Tier A) → the `CREWLET_NODE_ID` environment variable → `node-0`. You do
not need to set it to run a single engine. It starts with a letter or a digit
and holds only letters, digits, `.`, `_` and `-`, at most 64 characters,
because it ends up in broker consumer names and subjects — and an operator
gesture naming a node, such as `crewlet retention evict`, holds the id it is
given to the same rule.

It must be **stable across restarts**, which is why it comes from the
deployment rather than being generated per boot: anything the process
registers under its identity would otherwise be orphaned on every restart.
In Kubernetes use the pod name — a StatefulSet ordinal is ideal; under
systemd, the host name.

#### `node.roles`

What this process is willing to do. Three roles, and the default is all
three — one process running a whole company, which is every single-node
deployment:

```yaml
node:
  id: "${CREWLET_NODE_ID}"
  roles: [seats]              # a satellite: agents only
  labels:
    zone: eu
```

| Role | What it does | What a fleet loses without it |
|---|---|---|
| `ingress` | Serves the HTTP API: webhooks, the dashboard, the REST endpoints | No integration can reach the company, and there is nothing to look at |
| `seats` | Claims seat leases and runs agents, and serves their agent-mode tool bridge (`/mcp/{token}`) when `CREWLET_MCP_BRIDGE_URL` is set | Every trigger queues up unread |
| `workers` | The company-wide singleton duties: the scheduler tick, the maintenance sweep (retention and removed-seat mailbox retirement), the sandbox waiter, the integration reconcile loop, and the learning background passes (episode lifecycle, skill curation, clustering and promotion) | Nothing fires on a schedule, no sandbox run is collected, no table is swept, no integration is reconciled |

Subtracting a role subtracts it from **this node, never from the
company**, so the fleet as a whole still needs every role somewhere. That
is a shape no single node's config is wrong for, and every symptom of
getting it wrong is an absence — so the engine checks it against live
node presence and logs `fleet_role_unmanned` when nobody is doing a job.
A node that does not run seats is also left out of the denominator its
peers divide the seats by; counting it would strand the difference.

#### `node.labels`

Free-form facts about where this process runs, matched by a seat's
[`role.placement`](../guides/fleet.md#placement) selector. Values are
strings and are compared exactly — so both the key and the value are
trimmed of the whitespace around them before anything reads them, and two
keys that are one key once trimmed are refused rather than collapsed. They are advertised to peers on this
node's presence lease, so a label change takes effect one heartbeat after
the restart that made it — not at the next config activation.

Nothing here means anything to the engine on its own: the org decides
what to select on.

### Tier B example (`company.yaml`)

Everything that defines the company — see [examples/nimbus.company.yaml](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) for a complete working document, and the [configuration reference](../getting-started/configuration.md) for every field including the ones that example does not use.

---

## Bootstrap Sequence

The engine boots in this order:

1. Read `crewlet.yaml` (Tier A only — the store path, the stream, coordination, api host/port/auth, secrets, logging)
2. `logging.Configure(level, format, stderr)` — once, in `cmd/crewlet`, which is
   the only thing that sets the console destination; a later command changes how
   loud it is with `SetVerbosity` and keeps the sink already installed. The level
   and format come from the file's `logging:` block with any `-log-level` /
   `-log-format` / `-debug` flag layered on top, and only where the flag was
   actually given. Lines emitted *before* this — the file's own `${VAR}`
   warnings, a refused field — come out under the flags alone, which is the
   best a process can do about a file it has not opened yet
3. `logging.SetFile(…)` if `logging.file.path` (or `-log-file`) names one — a
   **second** destination by default: stderr keeps every line, and the file
   gets its own handler, so it can carry `json` at `debug` while the terminal
   keeps its columns at `warn`. `logging.stderr: false` hands the stream over
   to the file instead, and is refused with no file to hand it to. It is
   opened here, before the store and the stream, so the failures those can
   produce are in it. A path that cannot be opened **stops the boot**, naming
   the path: every other bad logging value resolves to a default, but a
   durable record an operator asked for and silently did not get has nothing
   pointing at why. The same ordering has a corollary — a boot that fails on
   the Tier A document itself never reaches this step, so stderr is the only
   record of it, `logging.stderr` notwithstanding
4. Open the store file and start or dial the stream
5. Run migrations — every file, in one pass. There is no lock and no phase ordering to serialize: this process owns its file, so nothing can be racing it, and no DDL depends on a value only the config knows. Embedding columns are declared as plain blobs and the vector width is validated in Go against the active revision at write time, so a schema step never has to read the config first (see [`crewlet migrate`](../reference/cli.md#crewlet-migrate)).
6. Start the **core runtime** — on every node, with a company or without one: the [state log](../guides/consistency.md) for **every** registered domain, the node gate over every identity-claiming log, the [identity estate](identity-and-access.md) with the loops that keep this node's view of it, and — on a node that publishes — the log's own trim and the identity duties. None of it depends on a company, and a node with none is exactly the one its first person has to be invited to and sign in on. It is **every** domain rather than the identity estate alone because a join replaces the whole replicated file and a snapshot names every registered domain, and because the trim counts nodes per log: a node that applied part of the register could neither adopt nor donate, and would pin the rest of the logs' trims for as long as it was up
7. Start the API inside this process, bound to `api.host:api.port`, wire up auth middleware, register `/config/*` routes
8. Start the [control plane](control-plane.md) — the reconcile loop that polls the activation pointer, plus a broadcast `crewlet.config.revision_activated` nudge that wakes it early
9. `SELECT payload FROM company_config WHERE is_active <> 0`
   - **Row present**: apply the payload, which spawns the full company — including the **native halves** the company declares: the tracker's and the knowledge base's writers and readers over the logs step 6 already applies, their lexical index, change feeds and embedding duty
   - **No row**: engine stays in the **unconfigured** state — the API keeps serving so an operator can push the first revision via `PUT /config` or `crewlet config import`, and people can be invited and sign in

**A boot that fails leaves nothing running.** Any step above can fail — an
unreachable broker, a keyring the node cannot open, a provider whose model is a
`${VAR}` nothing sets — and when one does the node unwinds everything the steps
before it started, in the order a shutdown uses: the state log's apply loops and
its snapshot donor, the shared MCP child processes, every duty loop, this node's
publish admission, and the store file and stream it opened itself. So a
supervised `crewlet run` retries onto a clean host rather than contending with
its own previous attempt — which matters most for the store, since one process
owns that file exclusively and a second open behind a leaked handle fails with
a message about locking that names neither the original failure nor the file.
The `engine_boot_abandoned` line is what says the unwind finished.

### Two equivalent bootstrap entry points

**Option 1 — bootstrap before run (CLI):**

```bash
crewlet config import company.yaml   # one-shot bootstrap of Tier B
crewlet run                          # boots from ./crewlet.yaml + the store
```

**Option 2 — run first, configure the running node:**

```bash
crewlet run                          # boots in UNCONFIGURED state
crewlet config import company.yaml -summary "initial bootstrap"
# Detects the running engine, goes through its API, and every node
# reconciles onto the new activation epoch — no restart needed.
```

The same thing by hand, which is what the CLI is doing:

```bash
curl -X PUT https://crewlet.example.com/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: initial bootstrap" \
  --data-binary @company.yaml
```

This is also how you change a company that is **already running**: the same
command, against a node that already has one.

`crewlet run` defaults `-config` to `./crewlet.yaml` and `-company` to `./company.yaml` in the working directory; naming a path is only needed when a file lives elsewhere. **A missing `./company.yaml` is not an error** — the store is authoritative, so a node with no Tier B file boots on whatever the store holds, or unconfigured when it holds nothing.

`-company` only ever **bootstraps an empty store**. Once a company exists the file is ignored, with a `company_seed_ignored` warning naming it and the active revision, so a restart cannot revert a change made live. Two flags cover the other intents:

| I want to… | Use |
|---|---|
| fill an empty store at first boot | `crewlet run -company company.yaml` (the default) |
| change a **running** fleet, no restart | `crewlet config import company.yaml` |
| make a file the company again on restart | `crewlet run -import-company company.yaml` |

`-company` and `-import-company` together are refused: they ask for opposite things, and picking a winner silently would be picking it about the flag that overwrites a running company.

---

## Unconfigured State

Until the first active row exists, the engine holds an empty `Organization` (no name, no roles, no units), an empty provider map, no running MCP processes, no integration clients, no notification transports, and no native tracker or knowledge base halves.

**What stays running:**

- The Tier A resources — the stream, the store file, the API socket — all up.
- The **core runtime** ([Bootstrap Sequence](#bootstrap-sequence) step 6): every domain's state log, the node gate and the identity estate, and on a node that publishes the trim and the identity duties. So an unconfigured node **signs people in and invites them** — its first person is invited under a Tier A token exactly as everybody after them is (`crewlet iam invite`) — and can be evicted from and readmitted to the logs it is counted on.
- The API's `/config/*` routes and the node's [reconcile loop](control-plane.md) — which is exactly what wakes an unconfigured node when the first revision lands.
- A structured log line carrying `state=unconfigured`, so the unconfigured posture is obvious in logs and on the dashboard.

**What returns degraded responses:**

| Route | Behaviour while unconfigured |
|-------|------------------------------|
| `GET /health` | `200 {"status": "unconfigured", "node": "node-0", "configured": false, ...}` — 200 because the status code is liveness; read `configured` for readiness |
| `GET /ready` | `503 {"ready": false, "configured": false, "reason": "unconfigured", ...}` — an unconfigured node cannot verify a webhook signature, so it stays out of rotation. `reason` names which of [the four](control-plane.md#posture-what-a-lagging-node-does) took it out |
| `GET /config` | `404 {"error": "no_active_revision"}` with a hint |
| `GET /config/revisions` | `200 []` |
| `PUT /config` | Accepted, and creates the first active revision, as long as the FLEET has no activation either: a node that has not caught up with its fleet answers `412 already_configured` (with `If-None-Match: *`) or `409 revision_advanced`, naming the revision the fleet is on. Send no precondition, or `If-None-Match: *` to insist nothing is configured yet; an `If-Match` names a revision to match, so it answers `412 no_active_revision` |
| `POST /config/revisions/{id}/revert` | `404` — no revisions exist yet |
| Per-entity routes (`PUT /config/roles/{handle}`, `/config/units/{id}`, `/config/llm-providers/{key}`, `/config/mcp-servers/{name}`) and `PATCH /config` | `409 Conflict` — they edit a document, and there is none; initialise via `PUT /config` first |
| `GET /agents`, `GET /tokens/breakdown` | `200` with empty lists / zero counters |
| `/auth/*`, `/iam/*` | Served exactly as on a configured node — they are the core runtime's. `GET /iam/seats` alone answers `409 no_active_revision`: it lists the running company's human seats, and there are none |
| `/work/*`, `/pages/*`, `/operator/mcp`, and the work, page and search questions | `503 no_active_revision` with a 15-second `Retry-After` — over the socket, an `unavailable` frame naming the same `refusal` with the same `retry_after` and words: the routes are **mounted** and say why they cannot serve rather than answering the `404` of a route that does not exist. They read the tracker's and the knowledge base's halves **per request**, so the first company's apply brings them up under the same API with no restart; a company that keeps its tracker or wiki on a vendor answers the `404` a route with nothing behind it answers from then on |
| `POST /webhooks/...` | Answered **before** the signature check — a node with no revision has no secrets to check against, and verifying first would answer every delivery with the no-secret refusal and its five-minute wait — as `503 no_active_revision` with a 15-second `Retry-After`, so the sender **retries**; `webhook_rejected_unconfigured` is logged at WARNING with the source and the event. A 200 here would tell the sender the delivery was accepted while discarding it — silent, unrecoverable loss the moment one process of several has simply not caught up yet. See [Webhook refusals](../reference/api-endpoints.md#webhook-refusals) |

Transition out of unconfigured: the first activation moves the pointer → the reconcile tick picks it up → the apply runs → the spawn cascade executes, including everything boot starts only for a company it already has: the native tracker's and knowledge base's halves with their tools, the projects and knowledge containers the org chart's units declare, the code sandbox's coordinator and completion poll, the reflect dispatcher and the inbound edge (see the `native`, `sandbox_runtime`, `learning`, `integrations` and `maintenance` stages below) → the engine is fully alive, with no restart. The dashboard carries the unconfigured state in always-on chrome (a caution banner saying inbound webhooks are being refused, an engine pill that says so, and the first row of the overview's attention queue), and it clears automatically on the next health tick once `/health` reports `configured: true`. See [the attention queue](../reference/dashboard-design.md#the-attention-queue).

---

## A Company With No Model Provider

An empty `providers.llm` is a valid company: an org chart written before its
credentials exist, and what a company created in the dashboard's
[org builder](../guides/org-builder.md#creating-the-company) is until somebody
adds a provider. `crewlet validate` accepts it (reporting `0 LLM providers`),
`PUT /config` and its dry run accept it, and every node applies it like any
other revision: the fleet view reports `ok`, the agent seats are placed, and
their mailboxes attach and keep what arrives.

What no seat can do is take a turn, and each node handles that the same way:

- It logs `company_has_no_models` when the revision becomes current, naming
  `providers.llm`. This is the line to look for on a company nobody has
  messaged yet.
- It holds every delivery to a seat on that seat's inbox. The inbox is paused
  and the delivery requeued, never consumed, and the node logs
  `seat_inbox_paused` for the seat, naming `providers.llm` again.
- The apply that adds a provider releases every inbox the node paused
  (`seat_inbox_resumed`), and the held work runs on the new provider. Nothing
  sent to a seat in the meantime is lost.

Schedules do not fire while the company has no provider: a fire is work on a
seat's inbox, so firing through the wait would stack every missed standup
behind the hold and run the backlog all at once. The scheduler stays disarmed
and each node logs `schedules_waiting_for_a_model`; the apply that adds a
provider arms it, and its first tick catches up at most the most recent missed
fire ([Scheduling](scheduling.md#when-the-loop-runs)).

The learning work that calls a model is not built for such a company: the
persist decider, the skill synthesizer and refiner and the counterparty
profiler that run after each turn, and the background compaction, clustering
and promotion passes. The apply that adds a provider builds all of them. The
skill curator calls no model and runs as usual.

A delivery is held rather than failed on purpose. A turn that cannot build its
runner proves nothing reached outside the engine, so the dispatcher would hand
it back to the broker, which would redeliver it until its delivery budget ran
out and then drop it: every message sent before the provider arrived would be
retried pointlessly and then lost.

---

## Live Propagation

When a new revision is activated (via `PUT /config`, `PATCH /config`, a per-entity write, a revert, or `crewlet config import`), the revision is stored and the fleet's **activation pointer** is then moved to it; the pointer's own KV sequence *is* the epoch, so the append and the flip cannot come apart. Every node polls that pointer and converges onto it; a broadcast `crewlet.config.revision_activated` event wakes the poll early but carries no work.

The two steps are **not one transaction**, and they span two stores: the node's own database and the coordination KV. That ordering is deliberate: a crash between them leaves a revision nothing points at, which is inert and replaced by the next activation, where the other order would point the fleet at bytes no node had stored. The revision is stored as history and becomes the writing node's own active revision only after the pointer has moved, so a write the fleet refused is never what that node serves or republishes; see [Control Plane](control-plane.md#the-design).

There is **no leader**, so any node's API may write. What keeps two operators from silently overwriting each other is that the flip is a **compare-and-set** against the revision the write was derived from: the loser gets a `409` naming what won, rather than a `201` for a change the fleet never took. See [Concurrent writes](../reference/api-endpoints.md#concurrent-writes).

**The nudge carries no work, and that is the load-bearing part.** A group subscription is a *work queue* — the contract says exactly one member of a group receives each message — so a revision delivered that way would be applied by whichever process won the message while every other node kept serving the previous company indefinitely. Polling a pointer inverts that: every node reads the same value and converges on it, and a lost nudge costs a poll interval rather than a revision. The full mechanism — the activation pointer and its epoch, what a lagging node does about its own traffic, and the operator surface — is [Control Plane](control-plane.md); what follows is the apply itself.

### The engine half

Converging applies the payload. `Engine.Apply` is a **straight line with no
comparison and no early return** — there is no apply lock, no payload
short-circuit and no rollback of captured state. It rebuilds the whole epoch,
in a fixed order, and names each stage it got through.

1. **`secrets`** — re-read the secret store and install a fresh resolver snapshot. **First**, because re-activating an unchanged revision is the documented [rotation gesture](secret-store.md): the payload has not moved, so the only thing that can have is what its `${VAR}` references resolve to.
2. **`company`** — validate and build the new epoch, resolving `${VAR}` where each provider is *constructed*. A refusal here changes nothing: this node keeps serving the previous epoch.
   A company with no `providers.llm` is not refused: it builds with no model registry (see [A Company With No Model Provider](#a-company-with-no-model-provider)).
   The revision's sandbox catalogue (`providers.sandbox`) is built straight after, and one that cannot be built refuses the apply there — reporting `secrets, company` and nothing else, since nothing below has touched the node yet. On **every** node, whether or not it runs a sandbox yet: the alternative publishes a company whose sandbox-enabled seats plan around a box nobody can mint.
3. **`native`** — bring up the native tracker's and knowledge base's **halves** on a node running neither, for its first company on them: their read and write sides, the lexical index, and — on a node that publishes — the embedding duty and the change feeds. Not their logs: every node has applied every domain's log since boot, company or none ([Bootstrap Sequence](#bootstrap-sequence) step 6), so what the first company brings up is the writers and readers over logs the node already holds. **Before** `tools`, because the native tools are registered only where the halves exist, and before `integrations`, whose inbound edge includes the native parsers. A start that fails refuses the apply. **Conditional:** reported only on the apply that started it — once per process, and never at all on a node that met its company at boot. The halves follow the fleet's logs rather than a revision, so a later refusal of the same apply does not take them down, and which halves run are the ones that first company declared: changing `tracker.backend` or `knowledge.backend` after that still takes a restart.
4. **`sandbox_runtime`** — bring up the [code sandbox](code-sandbox.md)'s coordinator and completion poll on a node running neither, for the first revision whose `providers.sandbox` reaches a cell, and prepare the seats this node **already** holds: each was taken before there was anything to prepare, so each now attaches its completion topic and recovers the runs recorded on it, as a seat taken afterwards does on its way in. A seat whose preparation fails is handed back (a voluntary `unprepared` release), so its next acquisition, here or on a peer, runs the whole of it. **Before** `tools` for the reason `native` is: `run_sandbox` and an agent-mode executor are offered only where the runtime exists. A start that fails refuses the apply. **Conditional:** reported only on the apply that started it — once per process, and never at all on a node that met a sandbox company at boot. Like the native halves it is a fact about the process rather than a revision, so a later refusal of the same apply does not take it down.
5. **`tools`** — equip the new epoch with this node's builtins. An epoch is published, never mutated, so each one gets its own registry; a node that equipped only its first would serve a company whose agents silently lost every builtin at the first config change.
6. **`learning`**: rebuild the reflection workers against the new org. Deliberately cannot fail the apply: reflecting against a stale org is a far smaller wrong than not reflecting. The one exception is a node's **first** company: a node that booted with none has no reflect dispatcher to swap workers into, so this stage attaches it, and an attach that fails refuses the apply for the reason it fails a boot (a company served without it learns nothing while looking healthy).
7. **`sandbox`** — swap the sandbox *manager* only. The coordinator and waiter hold this process's busy set and poll loop; rebuilding them would forget which seats are mid-run and start a second loop over the same rows. The swap carries the backend of any cell the revision dropped as *retired*, for the runs still on it, and a revision with no `providers.sandbox` keeps the last manager for the runs in flight (see [Code Sandbox](code-sandbox.md#engine-provider--providerssandbox)). It cannot fail — the catalogue was built at `company` — and it runs after every stage that can refuse a node already serving a company, so a refused apply never leaves this node launching through a catalogue its current epoch does not have. **Conditional:** only where this node runs a sandbox runtime, from its boot or from a `sandbox_runtime` start.
8. **`integrations`**: rebuild the inbound surfaces against the new epoch (Confluence, Datadog, Jira, GitLab, GitHub, and the two chat transports, Slack on every apply and Mattermost when a value it is built from moved), so work items route by the new org chart rather than the boot-time one. A third-party app the revision **retires** (its block removed, or `enabled: false` for GitHub and GitLab) has its parser unregistered, so its deliveries route to no seat; GitHub's and GitLab's webhook routes then answer `503` rather than verifying and ingesting a delivery the routing half would drop. Confluence additionally loses its searcher, or every seat would go on searching a wiki the company has removed, with the credential it revoked. Confluence and Jira re-derive a **lead map** from the org (space and project key to unit lead), which is what an unrouted page or issue falls through to. GitLab and GitHub have no lead map; theirs re-resolves the engine credential and the participants lookup that fans a thread out to the seats on it.
   On a node that booted with **no company** there is no inbound edge to rebuild: boot starts one only for a company it already has, because the inbound consumer group is fleet-wide and a node with no parsers would take deliveries its peers can route. So the node's first apply **starts** the edge here, through the same function boot runs, and every later apply reconciles it. A start that fails (the broker refuses the subscription) refuses the apply like a refused build, before the epoch is published, and takes down whatever it had brought up, so the retry starts from nothing.
9. **`epoch`** — publish the new epoch. This is the swap; everything before it built, everything after it reads the now-current company.
10. **`maintenance`** — rebuild the retention sweep, on a node's **first** company (or the apply that started the native halves). Its job list is read once, and on a node that booted unconfigured it was read before there were native halves to contribute the tracker's repairs and the inbox sweep, or a company to state the conversation horizon — so it swept none of those until a restart. Every operation ledger is swept from boot either way: the ledgers are the core runtime's. **After** the swap, because it reads those horizons off the current company. The old sweep stops, its in-flight tick waited out, before the new one starts. **Conditional:** only on that first apply, and only on a node that publishes.
11. **`learning_passes`**: hand the background learning loops (episode compaction, the skill curator, clustered synthesis and promotion) the passes this revision turns on, built from its models, credentials and knobs. **After** the swap, because the loops walk the current company's seats: handed over earlier they would run the new revision's passes over the previous company's roster, and a refusal later in the same apply would leave them there for a revision this node never served. The loops themselves are armed once per process and keep their clocks across an apply (see [Agent Learning](agent-learning.md#trigger-threshold-gated-on-a-slow-loop)), so this is also where a node that booted with no company, or a company that gained its first provider, starts running them. Reported on every apply, including one on a node with no store or no worker role, which has no loops to hand anything to. Not part of the convergence below, because nothing it builds is derived from the org chart.
12. — 19. **the convergence**: everything derived from the *published company itself*, brought up to it. These are not the apply's own stages: they are the same list a node's boot runs, through the same function, because a boot publishes a company exactly as an activation does. They are described under [What follows a published company](#what-follows-a-published-company) below, and an apply names each of them in `applied_subsystems` in that order: `parties`, `seat_identities`, `seat_tools`, `tracker_projects`, `knowledge_containers`, `mailboxes`, `scheduler`, `published`. `mailboxes` is where a company that gained its first model provider lets through the mail every seat inbox held while it had none — after the seat tools, because the first thing a released inbox does is run a turn.

Then a `config_revision_applied` event is published on
`crewlet.config.revision_applied` with `status`, the `applied_subsystems` list
and any error.

**A failure is reported by how far it got, not undone.** Every refusal above
happens before the epoch swap, so it leaves the previous epoch current and
serving — that, rather than a rollback, is what makes a failed apply safe. The
returned list is the stages that *did* complete, in the order they completed,
so "secrets, company" names both what was rebuilt and where the refusal landed.
It travels on `config_revision_applied` into the audit event log, where it
outlives the fleet view's one-minute bucket. The fleet view carries each node's
epoch, revision, status and failure text but *not* the stage list, so that
detail lives on the event rather than on the operator surfaces reading the
bucket. The active row stays active either way; the control plane records
the outcome so peers can see it (see [Control Plane](control-plane.md)).

**Read that list by name, never by number.** Five of the nineteen names are
conditional — `native`, `sandbox_runtime` and `maintenance` only on the apply
that first needs them, `sandbox` only where this node runs a sandbox runtime,
and `mailboxes` only on an engine with a node — so an ordinary apply on a node already
serving a company that runs no sandbox reports fifteen, and the swap is the
sixth of them. The numbering above is the order the code runs, not an index
into what a node reports. `published` is always last, because it is what tells
every open socket the company changed and it must not say so until everything
a socket reads has been rebuilt.

**A stopping node applies nothing.** Stopping waits for an apply already
running, which returns quickly on the cancelled context that asked for the
stop, and refuses every later one with `error`. An apply that ran on past the
teardown would start again what it had just ended: the scheduler, the
background learning passes, and on a node's first company the inbound edge.

> **"No rollback" is not "no mutation".** What the build-first ordering buys is
> that a revision which cannot be *built* changes nothing: `NewCompany`
> validates, resolves the org and constructs the providers without reaching the
> network, so stage 2 is the cheapest place to refuse and the one that costs
> nothing at all — the sandbox catalogue included, which is built there too.
> Past it the guarantee narrows. On a node that already serves a company,
> **`tools` is the last stage that can refuse** (an embeddings width it cannot
> serve, below): `learning` and `sandbox` cannot fail there, `integrations`
> returns no error, and `epoch` is the swap. By the time a `tools` refusal
> lands, two things are already mutated: the resolver snapshot (`secrets`),
> and what `tools` did before it refused — any shared MCP child whose spec
> moved, and the skill variables. So that refusal leaves this node's shared
> tool processes on the new company while it still *serves* the previous
> epoch, and reports `error`. A sandbox runtime the same apply brought up
> (`sandbox_runtime`) stays up too, and harmlessly: the previous epoch offers
> no seat `run_sandbox`, so it launches nothing, while the runs it already
> polls and the seats it prepared are the fleet's rather than the revision's.
> The reflection workers, the party index and the convergence are not among
> them: they are rebuilt after the last failure point, which is why they are
> ordered there. A node's **first** company is the one exception, on both
> sides of `tools`: `native` refuses it when the runtime cannot start,
> `learning` when the reflect dispatcher cannot attach, and `integrations`
> when the inbound edge cannot start. None of those refusals has a previous
> epoch to protect, so what it leaves behind (the shared MCP children, an
> attached dispatcher, the party index, native halves over logs the node
> already applies) serves nothing until the retry the refusal earns, which finds
> it already there. Widening that window is what would make `degraded`
> reachable, which is why everything an apply cannot un-apply stays behind the
> swap.
>
> The one thing an apply does **before** the swap that the convergence also
> does is rebuild the party index, and it is deliberately not named as a stage.
> Indexing early is a choice between two brief windows: refreshing before the
> swap leaves a seat the revision **removed** addressable for an instant, which
> costs a recorded skip; refreshing only after it leaves a seat the revision
> **added** unresolvable while the epoch carrying it is already current. During
> a rollout the new company is the one being adopted, so the window that
> favours it is the right one. An apply refused between that call and the swap
> has indexed a company nobody can reach, which costs a rebuild and nothing
> else.

One knob is refused rather than applied live, because applying it would
corrupt data rather than merely disrupt it:

- **`providers.embeddings.dimensions`** — rows already written carry vectors of the old width, and a similarity query across two widths compares nothing. The apply fails naming the declared width and the width the store already holds. Changing it means re-embedding, not a restart. Adding or removing the whole `embeddings` block *is* live in both directions: a company that drops it degrades to recency-only recall on the next turn.

**`providers.sandbox` is live in every direction.** Adding it brings the code
sandbox up on the next apply (`sandbox_runtime`), on a node that booted with no
company or with a company that had none; changing it swaps the manager
(`sandbox`) with the runs already in flight kept whole, a dropped cell's
running jobs included; and removing it stops new launches while the runs in
flight finish. A block that cannot be built is refused on every node, at
`company`. See [Code Sandbox](code-sandbox.md#engine-provider--providerssandbox).

**Token caps need no apply stage at all, because there is no cap set to
maintain.** Usage is shared and caps are not: the fleet's counter stores only
what each scope has *spent*, and the limit travels in as an argument on every
charge, read straight off the epoch the turn is pinned to. So a revision that
raises a ceiling takes effect on the next turn on every node at once, with
nothing seeded and nothing to drop when a seat goes away — a role removed,
flipped to human, or dropped to `0` (= unlimited) simply stops having a limit
passed for it. The alternative, caps replicated per node, is what makes an org
ceiling of 500 000 quietly become N × 500 000.

**Shared** MCP children are reconciled per server against the new epoch's specs,
comparing every field: a change to `url` or `headers` restarts **only that
server**, and every other child keeps running and keeps contributing the tools
it is already serving. The comparison is over *resolved* values, so a rotated
credential reads as a changed spec — see
[Rotation](control-plane.md#rotation).

A server the revision **removed** is stopped rather than merely dropped from
the catalogue, and that is a separate step because the reconcile above is
driven by the specs the *current* config names — so it never visits a name
that is gone. Without it the child kept running until the engine stopped,
holding the company's credentials, and a *rename* ran two of them. Retiring
happens before the reconcile, so a rename frees the name and the tools it
published before the replacement files its own. Taking a leaking integration
offline by deleting its `mcp_servers` entry therefore takes effect on the next
turn, which is what the rest of this section already promised.

**Per-role children are not on this path.** They belong to a seat's *lease*
rather than to the epoch: the apply-time reconcile above skips every non-shared
server, and a role's `mcp_env` — which carries the per-agent Slack/GitHub
credentials — is never part of a spec it compares. Such a child is spawned when
its seat is claimed, and it is reconciled by the `seat_tools` step of
[the convergence](#what-follows-a-published-company): an apply that adds,
removes or re-credentials a server for a seat this node holds retires the
children that are gone and starts the ones that arrived, in place, without
releasing the seat and without touching the children the revision still names.

The step rebuilds both halves because both go stale on an apply: the
**registry** is a clone of the epoch's surface, and the **children** are the
seat's `mcp_env` and its name. A child the revision still names is neither
stopped nor restarted — it never learns anything was published — so the
credential re-handshake a restart would cost is paid only by the servers that
actually changed.

### What follows a published company

A company is **published by two different paths**: a node's boot publishes the
company it starts on, and a config apply publishes the revision an operator
activated. Everything derived from a company has to follow **both**. Written as
two lists it followed one, and the failure was silent in a particular way: a
seat a revision added was in the org tree and nowhere else — not addressable,
holding no vendor account, with no tool children of its own, no project, no
mailbox, on no dashboard and firing no schedule — until something unrelated ran
the missing step, at which point all of it corrected itself at once, which is
the shape that makes a cause impossible to find.

So there is **one list**, and both paths run it. In order:

| Step | What it brings up | Why every published company needs it |
|---|---|---|
| `parties` | The party index every routing decision resolves a name through | A registry is derived from one org and answers for it permanently. Left behind, every lookup of a new seat answers "nobody matches" — the same answer a stranger gets, so nothing fails and nothing is logged |
| `seat_identities` | The vendor **account** each seat holds, re-resolved for Jira, GitLab and GitHub | A code host names a seat by an account, and which account a seat holds is read from the seat's own credential. The lookups are keyed on the token and cached, so a company whose credentials did not move spends no requests |
| `seat_tools` | Each held seat's registry, and its per-role MCP children | Both halves are described under [Shared MCP servers](#live-propagation) above |
| `tracker_projects` | The projects the org chart names, as objects, stamped with the revision's activation instant | A project belongs to a **unit**, so the revision that first names one is usually the one that adds the unit. A create takes its key from the project's own counter, so a project that is not an object refuses every task filed into it |
| `knowledge_containers` | The containers each unit's and seat's `space:` names | Same reason, one subsystem along |
| `mailboxes` | A durable subscription per seat; the release of every inbox held while the company had no model; a fresh judgement of every seat [parked on its token budget](agent-runtime.md#the-budget-park) against the ceilings now current; and the end of every [pause](agent-runtime.md#pausing-a-seat) whose seat the revision removed | Until a mailbox exists, every event published to that seat is **dropped** rather than retained. A convergence rather than a walk: it asks the broker what is missing and writes only that, so a company whose mailboxes all exist costs a comparison and no consumer proposals at all — and it runs unconditionally, because a mailbox lost under a company nobody edited is a fact about the broker that no comparison of two companies would notice. A pause is keyed by the seat's id, so it is cleared only when the seat itself is gone |
| `scheduler` | The cron loop, armed or disarmed | It reads its schedules off the current company, so it is armed after the pointer moves: armed earlier, it would fire the outgoing company's crons |
| `published` | The socket push that re-sends the roster, org tree, tool catalogue and schedules **whole** | The dashboard's company-derived screens come from the company, so no event will ever correct them and an overlay merge cannot express a seat going away |
| *the directory* — not a step of this list | The party index again, rebuilt **for the same company** when the [identity directory](identity-and-access.md#what-a-suspension-reaches-and-how-fast) moved | Not a configuration change at all, which is the point: suspending, reinstating, binding or removing the person who holds a seat is a record on the identity log, and a registry rebuilt only on a published company went on attributing a suspended person's Slack messages to their seat until somebody next activated a revision. See below |

**Every step notices for itself that there is nothing to do**, which is what
makes running the list on every apply cheap — re-activating an unchanged
revision is the credential-rotation gesture: the registry compares the company
it was built from, the identity lookups are token-keyed and cached, the mailbox
pass compares its own set against the broker's, the tracker and the containers
compare the row against what the company says, and the scheduler arms or
disarms rather than rebuilding.

**The party index has a second trigger, and it follows the same rule.** Who
holds a seat, and at what stage, is the identity directory's answer rather than
the company's: a seat whose holder is suspended, retired or removed registers
no contact identity (see [Humans in the Org
Chart](humans-in-the-org.md#a-suspended-holder-is-withdrawn-with-no-configuration-change)).
So the identity applier signals after every committed batch that moved a seat's
standing, and the node reads the directory once and — if the answer moved —
builds a whole new registry for the company it already serves and swaps it in,
exactly as `parties` does. Both triggers take one lock, so they cannot swap a
half-built registry past each other, and neither is ever a diff against the
live one. It is within one apply on every node, whatever its roles, with a
thirty-second re-read as the net, and it never appears in `applied_subsystems`,
because no apply ran.

**The order is not arbitrary.** A released inbox runs a turn immediately, and
that turn resolves parties, loads its tool surface and files work into a
project — so everything a turn reads is converged before the mailboxes are
ensured and any held mail is let through. `published` is last, because it tells
every open dashboard what this node now serves.

### The API half

**There is no second projection to keep in step.** The API answers every read
through closures over the engine's *current* epoch rather than from a cached
copy of the payload — `GET /agents`, `/org` and the dashboard's queries all
resolve against whatever `Apply` last published, and the inbound webhook
secrets are re-read per delivery the same way. So a rotated signing secret is
picked up by the epoch swap itself; there is nothing that could drift stale and
nothing to refresh.

There is one wiring, because there is one process: the API is served inside the
engine's, over the engine's own backends, and every node runs exactly one
reconciler whatever its `node.roles`. So the two halves cannot disagree about
which epoch they are on.

`configured` is derived the same way as everything else: it is true when the
engine's current epoch holds a company, and it flips the moment an apply brings
a node its first revision. A failed apply leaves the node serving the previous
epoch, which is still a configured node. What a failed apply changes is the
**posture**, and `/ready` reads that. Collapsing the two would take a
correctly-serving node out of a load balancer's rotation for being behind.

---

## Versioned Revisions

`company_config` is an append-only table — every change is a new revision. It
is swept; that is described under [what a revision is
not](#a-revision-is-immutable-as-a-configuration-not-as-an-archive-of-people)
below. The schema:

```sql
CREATE TABLE company_config (
    revision_id        TEXT    NOT NULL PRIMARY KEY,
    parent_revision_id TEXT    REFERENCES company_config(revision_id),
    created_at         INTEGER NOT NULL,          -- unix microseconds, UTC
    created_by         TEXT    NOT NULL,          -- the AUTHOR: a seat's handle for a person
                                                  -- bound to one, a login otherwise,
                                                  -- token:<id> for a Tier A token, the
                                                  -- engine's own name for its own writes
    created_by_kind    TEXT    NOT NULL DEFAULT '', -- agent | human | operator | system
    operator_id        TEXT    NOT NULL DEFAULT '', -- the credential it was written
                                                  -- through: pat:<id>, session:<lineage>,
                                                  -- a Tier A token's login; empty for a
                                                  -- write no credential made
    source             TEXT    NOT NULL,          -- "api" | "file" | "rekey" | "fleet"
    summary            TEXT    NOT NULL,          -- short human-readable change note
    payload            TEXT    NOT NULL,          -- the whole document, sealed under
                                                  -- the keyring every node holds
    is_active          INTEGER NOT NULL DEFAULT 0,
    activated_at       INTEGER
);

-- At most one active revision, enforced by the database rather than by the
-- application remembering to.
CREATE UNIQUE INDEX company_config_one_active_idx
    ON company_config (is_active) WHERE is_active <> 0;
```

The types here are the four SQLite has (`TEXT`, `INTEGER`, `REAL`, `BLOB`)
rather than `UUID` / `TIMESTAMPTZ` / `JSONB`, and a timestamp is unix microseconds
rather than a date type — Turso is SQLite-compatible in both its query language
and its file format, so that is simply what a column can be. It was also, until
recently, the intersection of two drivers' dialects; the second driver is
retired and the types are unchanged, because they were never the narrow part.

A revert creates a *new* revision whose payload equals a prior one — the audit chain stays intact via `parent_revision_id`.

### A revision is immutable as a configuration, not as an archive of people

One thing narrows "append-only", and it is deliberate.

**It is swept.** Every node keeps its own copy of every revision it has ever
met, and nothing deleted from it — one row per config write, per node, each
holding the whole document. The ordinary maintenance tick now keeps everything
inside **400 days** plus the **active revision and its whole parent chain
whatever their age**, and it runs on every node rather than under the fleet
singleton, because each node owns its own copy. The chain is kept by id rather
than by date: a revert re-activates an older revision and `crewlet config diff`
walks it, so a swept ancestor turns both into an error naming a row that used
to exist. See [Retention](../guides/retention.md#the-configuration-archive-and-the-one-thing-a-purge-cannot-reach)
for where 400 days comes from and what its floor is.

**A revision is an archive of the org chart it ran.** A human seat's `email`
and `contact` account ids are inside every revision that carried them — on
every node, in every backup — and removing the seat writes a *new* revision
rather than reaching the old ones. They leave the store when the sweep takes
their revision, and a backup when the backup is deleted.

### What a stored revision is held to

A stored revision is not a document somebody just submitted. It passed the validation of the build that wrote it, which is not necessarily the build reading it: a later build can add a rule, and during a rolling upgrade an older peer keeps activating documents that break it. So reading a revision and running one are held to different standards.

- **Reading holds a revision to no rule.** `GET /config`, a revision read, the diff, the reference index, the entity reads, `crewlet config show`, `export` and `diff`, and the prior a write restores its masks from or merges onto all decode the stored document as it is. A revision this build would refuse is exactly the one an operator needs to see and replace, so none of these may refuse it.
- **Applying holds a revision to the runnable rules.** A node's reconcile tick validates a revision before anything on the node changes, so a refused revision leaves the previous epoch serving untouched. Booting from the store validates the active revision and names it when it cannot run, with `crewlet config import` as the way out because the node's API is not up yet. A company file named with `-company` or `-import-company` is held to the runnable rules while the node only runs it, because most boots write nothing from it: it is already the active revision, or a bootstrap the store's own company outranks. `POST /config/reload` and a revert validate what they re-activate.
- **A written document is held to every rule.** `PUT`, `PATCH`, a per-entity write, a `/setup` submission that changes the document, `crewlet config import`, `crewlet validate` and a company file `crewlet run` imports as a new revision (`-company` into an empty store, `-import-company` over a different company) validate the entire document the write produces, after its masks are restored. A write over a revision this build refuses therefore succeeds exactly when it corrects it.

The difference between the last two is the **admission rules**: rules added after companies already existed, which a stored company can break and still run exactly as it did before them. Today they are [unique unit keys](organization-model.md#handles-and-keys-are-unique-names-are-not) — a display name, a seat's or a unit's, is prose and never held unique — and unique sandbox setup step names within one `setup` list (`providers.sandbox.setup`, or one seat's `sandbox.setup`): a step's `env` and `files` are credentials restored by the step's name, so two steps of one name would leave every write carrying that list refused on masks nobody edited. So is a `unit:` reference on a seat declared inside a unit it does not name: the reference places only a root seat, so there it moves nothing and reads as a placement (see [the organization model](organization-model.md#a-seats-unit-reference)). A seat's own GitHub App (`integrations.github`) on a [human seat](humans-in-the-org.md) is one too: an app is the identity an agent acts as on GitHub, a person acts as their own `contact.github_login`, and nothing creates or reconciles an app for a person, so the block would read as a setting and do nothing. Three more are **whole-document** rules: each compares one part of the document against another, or one seat against every other seat.

- **An `mcp_env` key must name a tool server.** The engine looks a seat's credentials up per *server*, so a block keyed on a name `mcp_servers` does not declare is never read by anything: it sits in the document looking configured while the server it was meant for starts with none. Six names need no server, because the engine reads those blocks itself — `github`, `gitlab`, `datadog`, and Atlassian's `atlassian`, `jira` and `confluence`.
- **Two seats may not declare one email.** The party registry keys on the address and the first seat wins, so all but one silently stop being findable and mail meant for one person resolves to another. The same rule already covered every [external account id](humans-in-the-org.md).
- **`lead:`, `unit:` and `manages:` must be shaped like what they name** — a seat's handle, a unit's key, or (for `manages:`) either. Those fields took a seat's display name once and do not any more, so `lead: QA Lead` can never resolve under any chart. Only the *shape* is refused: a well-formed reference that resolves to nothing stays a warning, because an organization is assembled in pieces and a lead naming a seat nobody has hired yet must still apply.

A seat may not be called `none` either — what `integrations.datadog.route_to` means by nobody, so a seat of that name would be silenced by its own handle. That one is a runnable rule.

A written document is refused for breaking one. A stored revision that breaks one is applied, booted on, reloaded and reverted to like any other, and each node logs `org_admission_warning` once per violation when it applies the epoch. Every other rule is a **runnable** rule, and nothing applies a revision that breaks one.

---

## Auth

**Every route requires a credential, reads included.** The only exceptions are
the handful that authenticate by other means or must be reachable to obtain a
credential at all, listed below. Tokens are listed in Tier A under
`api.auth.tokens` and resolved from environment variables at API startup. Each
revision a request produces records its **author** as `created_by` — a Tier A
token's whole login (`token:ci-pipeline`), a signed-in person's login
(`jane.doe`) or the seat the directory binds them to — with its kind as
`created_by_kind` and the **credential** it came through as `operator_id`: a
machine token's `pat:<credential id>`, a browser session's `session:<lineage>`.
So revision history carries meaningful attribution rather than generic strings,
and a revision somebody's token wrote is theirs and is never mistaken for one
they wrote themselves. Every node's copy says the same: the three travel on the
activation pointer — as `author`, `author_kind` and `operator_id`, beside the
revision's `source` and `created_at` — so a node adopting the revision from its fleet records the
writer rather than `peer`. A revision the engine wrote itself — the boot seed,
the reconcile loop — carries the kind `system` and no credential, which no
token's name could say. See [the API
reference](../reference/api-endpoints.md#the-config_audit-query).

### `config:write` is host access

The grant that writes this document is **one** grant, and it is conferred the
way shell on the engine's hosts is — because that is what it is. The
configuration runs code: an `mcp_servers` entry is a command every engine host
executes, a seat's `mcp_env` and its per-phase model keys (`llm_*`, a
`cli-agent` provider among them) decide what the seat's children run and which
credentials they are handed, a `sandbox` cell of `run_in: self` runs a coding
agent on the engine host itself, and a seat's worker grants decide which of its
tools a worker holds. So whoever holds `config:write` can run anything on every
engine host and read whatever a process there can — the keyring included — and
with it every other grant. There is deliberately no separate "runtime" grant:
it would be this one's whole practical reach under a second name, and a
`max_grants` ceiling that withheld it would withhold every settings edit too.

What that means in practice:

- Grant it to the people and pipelines you would give a shell on those hosts,
  and no one else. A machine token **can** carry it — applying a configuration
  from a deploy job is what one is for — so minting one with it is handing that
  job host access.
- `people:manage` being a separate grant does **not** bound a `config:write`
  holder. It keeps directory changes a reviewable gesture of their own; a
  holder of host access can reach the store the directory lives in regardless.

### What `allow_anonymous_read` was, and why deleting it was the only fix

Reads used to serve without a credential **by default**, on the argument that
reading is what a dashboard does and the page that prompts for a token cannot
itself require one. What that opened is worth naming: `/events`,
`/agents/{id}/memory` and `/ws/stream` carry full LLM transcripts — prompts,
tool arguments, diary entries — and the roster names everybody who works here,
to anyone who could reach the port.

It could not be repaired by changing its default, because of its *shape*: it
was a bool whose safe value was its zero, declared `omitempty`, so `false` did
not survive an export round trip. A deployment that had closed it re-opened
itself the first time its configuration went through `PUT /config`.

A deliberately public read surface is now a **named token entry holding read
grants and nothing else** — listable, revocable without a restart, and present
in the audit log, none of which a posture flag could be. The key is refused by
name if it appears, rather than ignored, because ignoring it would silently run
the opposite posture from the one the file asks for.

`api.auth.disabled` went with it. It authenticated the **empty** credential
into full operator authority with no check on the bind address anywhere, which
made one unset environment variable a total bypass. Local development uses
[`crewlet run -dev-principal <login>`](../reference/cli.md#-dev-principal-and-why-it-is-a-flag)
instead, which has the two guards that field never had: `api.host` must bind a
loopback address — the *bind*, not `api.external_url`, because the bind is what
physically stops another machine reaching this — and the binary must be a
development build, so no copied setting can carry it into a deployment that
pulled a tag. That is also why it is a flag: a field is the thing that gets
copied into an image. It grants `api.auth.max_grants` and no more, where
`disabled` granted everything regardless.

`api.auth.oidc` is retired too, and `backend: oidc` with it: the engine no
longer signs anybody in through an identity provider. A person signs in with a
password and a second factor the engine holds (`backend: local`), or the
deployment has no people and its Tier A tokens are the only credentials
(`backend: none`). The block is refused by name, so a file that still carries
it is told what happened rather than asked to check its spelling.

### What is served without a credential

Because these authenticate by other means, or must be reachable to obtain a
credential:

| Path | Why |
|------|-----|
| `/health`, `/ready` | Probes. An orchestrator has no token, and a liveness check that 401s is a liveness check that fails. A single trailing slash is tolerated (`/health/`), because the guard runs before routing — so the router's redirect to the canonical path only happens if the request gets past the guard first, and a slash must never be the difference between healthy and evicted |
| `/webhooks/*` | Each verifies its provider's HMAC before doing anything — a stronger check than a shared bearer token. Includes the Slack OAuth landing page, which a browser reaches mid-install |
| | **A route whose secret is unset has nothing to verify with, so it fails closed**: `503` + `Retry-After`, never an accepted delivery. The sender retries and the delivery flows once the secret is configured — a deployment that has not set one is stalled, not damaged, and nothing unsigned is ever recorded, published, or shown on the dashboard |
| `/otlp/*`, `/mcp/*` | The signed per-run token in the path *is* the credential. Both are reached from inside a sandbox, where the API's own token must never go |
| `/`, `/dashboard`, `/favicon.ico`, `/static/*` | The page that prompts for a credential cannot itself require one. It ships no data: every byte it renders comes from an authenticated fetch |

`/ws/stream` follows the same rule as every other route: the session cookie a
signed-in browser sends on its own — a browser cannot set a header on a
`WebSocket`, so the cookie is the whole of what the dashboard presents — or an
`Authorization` header from any other client. **Nothing is read off the URL**:
a `?token=…` authenticates nobody, here or anywhere, because a query string
lands in proxy access logs and browser history. The socket takes **the guard's own answer** for who
is calling rather than re-reading the credential itself, so it can never accept
a narrower set than the REST routes beside it — it used to read the token arm
alone, and refused every browser signed in with a cookie. A handshake
presenting nothing is refused before the upgrade, which the dashboard learns by
re-asking over plain HTTP: `401` means the credential is the problem, `426`
means the engine is fine and the protocol was wrong. A node that cannot read
its identity estate answers `503` rather than `401`, and a person whose seat
the running company no longer holds gets the guard's `403` naming the seat.

**The guard is mounted whether or not `api.auth` is configured, and it decides
who is asking — never what they may do.** Every request leaves its middleware
carrying one of three answers: **resolved** (a session cookie, a Tier A token
or a machine token matched, and the principal it names), **anonymous** (nothing
was presented, or what was presented is refused), or **unknown** (this node
could not read what it needed to tell — answered `503` with a `Retry-After`,
never the `401` that tells a browser to sign in again). What is not guarded is
exactly the list above; every other route is, whatever its method, and there is
no second list of routes that are "especially" guarded. What a resolved caller
may then *do* is each surface's own question, asked where it is enforced: every
route states the grant its verb takes, and `internal/authz`'s router decides it
against the principal — `401` for somebody unresolved, `403` for a grant they do
not hold or a proof of who they are that is too old (`step_up_required`). Tier
A supplies the *posture* — which credentials exist and the ceiling their grants
are cut to — never the existence of a check: an API built with no Tier A at all
has no token that can match, and every guarded route answers `401`. That is the
only safe reading of "an app was built without being told who may act", and it
removes the possibility of a process serving `/config` writes with nothing in
front of them.

### What Tier A must state on every node

**`secrets.keys` is required on every node**, whatever its roles and whether or
not it serves the API, and `crewlet validate` refuses a Tier A file without it.
It has no default — a key the engine invented would be one no other node holds
— and far more than the company's credentials rest on it:

- every record on every state log is signed and verified under it, because the
  broker authenticates nothing, and every node runs every state log, a
  seats-only satellite included;
- the company document a node fetches from its peers is authenticated by its
  seal, so a node refuses one that arrives unsealed (see [Secrets](#secrets));
- on a node serving the API it signs every session cookie and derives the key
  that verifies each per-run token.

Each key's `material` must be the base64 of a 32-byte key, which is what
`crewlet secrets keygen` prints. `crewlet validate` checks it on the value its
`${VAR}` resolves to, with the same decoder the node uses to build its keyring,
so a key that is not base64, or decodes to the wrong length, is refused by
`validate` naming `secrets.keys[i].material` rather than at boot.

### What Tier A must state once the API is served

Three more settings stop being optional the moment `api.port` is non-zero, and
each is refused by `crewlet validate` on a laptop rather than by a process at
bind time:

| Setting | Why it cannot be defaulted |
|---------|----------------------------|
| `api.external_url` | The session cookie's `Secure` flag and `__Host-` prefix follow its scheme, its host is the origin every write is checked against, and it is what every webhook URL is built on. The engine sits behind a TLS-terminating proxy and can read none of that off the request |
| `api.auth.max_grants` | The ceiling on what a directory record may confer. One granting everything is a ceiling that does nothing; one granting a subset silently locks out whatever it left out |
| `api.auth.tokens` | A fresh deployment's identity estate is empty, so a Tier A token is what creates the first person — and on a running one it is the way back in when an administrator has locked themselves out. Required on **every** backend, `none` included |

### What `crewlet validate` warns about

Three API postures are **valid** and worth reading before a deployment runs on
them. None can be a refusal, because each is a configuration that works exactly
as written with its consequence somewhere else:

| Warning | Why it is not a refusal |
|---------|-------------------------|
| `api.auth.local.accept_insecure` is set | It is what makes an otherwise-refused posture legal. The acknowledgement is a decision made once that everybody after inherits, so `crewlet validate` says it every time and the engine logs it on every start |
| `api.external_url` is `http://` off loopback | The session cookie cannot carry `Secure` and no `__Host-` prefix protects it, so every credential travels in the clear — but a tunnel, a staging box and an internal network genuinely look like this. The one posture it *would* be a refusal for, a password backend with an optional second factor, already is one |
| `api.external_url` is `https://` and `api.trusted_proxies` is empty | The engine never terminates TLS itself, so something in front of it does — and unless it is named, every caller's source is its address. The sign-in throttle then keys every caller's attempts at one login together, so a stranger guessing at somebody's login slows that person's own sign-in, and every audit row names the proxy. Not a refusal because one front end is right with the list empty: a balancer that passes each client's own address through as the peer rather than in a header |

`api.trusted_proxies` is a **CIDR list, never a bool**, because the question a
forwarded header poses is not "does this deployment sit behind a proxy" but "is
*this* peer the proxy". A bool set true trusts a header anybody can send, which
hands an attacker their own throttle key and their own audit row; set false
behind a real proxy it buckets the entire internet under one address. `0.0.0.0/0`
is refused for the first reason, and an `https` external URL with the list
empty is warned about for the second.

**CORS** defaults to same-origin. The dashboard is served by this process so it
needs no entry; list any other browser origin explicitly in
`api.auth.allowed_origins`. The previous `*` default let any site a logged-in
operator happened to visit read every endpoint, so `*` is now **refused at
boot** rather than honoured — name each site.

An entry is compared against the browser's `Origin` header exactly, which is
always `scheme://host[:port]`: an entry with no scheme, a trailing slash or a
path matches nothing and is refused for that reason, because an allow-list that
looks configured and never matches fails in a browser console this engine never
sees. A permitted origin's response carries `Access-Control-Allow-Origin` and
`Vary: Origin`; an unlisted one carries neither and the browser blocks it — it
is not refused with a status, because a same-origin `POST` carries an `Origin`
too and the default allow-list is empty.

Preflights are answered **before** the auth guard, and deliberately: a browser
sends the `OPTIONS` itself with no `Authorization` header, so answering it
behind the guard would be a `401` on every cross-origin write and the real
request would never be sent. Only `Authorization` and `Content-Type` are
permitted as request headers, and a preflight is cacheable for ten minutes —
short enough that removing an origin takes effect within one.

The auth middleware compares tokens in constant time (`crypto/subtle`).
Failed attempts log `api_auth_failed` at WARNING (never the candidate token
value); successes log `api_auth_ok` at DEBUG with `actor` (who a write would
be attributed to), `operator_id` (the credential it came through) and `route`.
Every write this surface logs — `config_revision_written`, `secret_written`,
`backup_taken`, `retention_gate` and the rest — carries the same pair as `by`
and `operator`.

See the [API endpoints reference](../reference/api-endpoints.md#config--live-config-management-auth-gated) for the per-route auth + status semantics.

---

## Secrets

Every node holds a **Tier A keyring** — `crewlet validate` refuses a `crewlet.yaml` without one and a node refuses to start (see [What Tier A must state on every node](#what-tier-a-must-state-on-every-node)) — so there is one secret-handling behaviour for Tier B, not two.

### `${VAR}` references

A credential in the company document is normally a `${ENV_VAR}` reference, stored verbatim and resolved where a provider, transport or integration is constructed (`internal/engine`). It resolves from the encrypted [secret store](secret-store.md) first and the process environment behind it — which is what lets a provisioner hand a minted credential straight to the engine instead of writing a file someone has to source. A reference the store does not answer resolves from the environment, so a deployment that keeps its credentials in its platform's own secret mechanism needs nothing else.

### Encrypted at rest, and authenticated

Crewlet encrypts the **entire** `company_config` payload as one opaque blob (AES-256-GCM) under the keyring before it reaches the DB:

```yaml
# crewlet.yaml (Tier A) — the keyring is the sole root of trust
secrets:
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"   # base64(32 bytes); crewlet secrets keygen
```

The whole document is stored as `{"__encrypted__": "enc:v1:<key_id>:<base64>"}` — nothing about the company's structure (its org chart, policies, model choices, integrations, or secrets) is visible in the database.

- **Encrypt on write.** Every write path (`PUT /config`, per-entity `PUT`, `crewlet config import`, `crewlet run -company` / `-import-company`) encrypts the whole document before the payload reaches the DB.
- **Decrypt at the read boundary.** The engine and the API it serves, migrations, and the CLI each decrypt the blob (`secrets.Open`, then `config.DecodeCompany`) into the plaintext structure before use, so the Tier A key is required for **every** config read. `${VAR}` references *inside* the config are kept verbatim in the blob and still resolve from the environment at construction time.
- **Fail closed.** A revision sealed under a key this node's keyring does not hold is refused rather than run as an opaque blob it can't read.
- **Authenticated, not only hidden.** The document a node fetches from its peers comes through the coordination store, which anything reaching the broker can write, so the seal is also what proves a node of this fleet wrote it. A revision stored **unsealed** is refused — never applied, adopted, shown or reverted to — with an error naming what brings it back, which depends on the revision: `crewlet config seal` for this node's active revision, importing its document again for a superseded one (nothing seals one in place), and the publishing node's own `crewlet config seal` for a body a peer sent. See [Control Plane § The design](control-plane.md#the-design).
- **One key, not N env vars.** After encrypting, the engine needs only the Tier A key in its environment — not a per-secret env var for every LLM key, MCP token, and webhook secret.

Because the key gates every read, keep it as available as the database itself: the API, dashboard, migrations, and CLI all fail closed without it.

**Threat model.** Encryption at rest defends against *data-at-rest* exposure: a leaked backup, a copied store file, a stolen volume snapshot, a coordination-store dump on a laptop — the attacker gets one opaque ciphertext blob per revision, no structure and no credentials. It also keeps config egress clean (`GET /config`, dashboard views, and revision diffs are decrypt-then-redact — no plaintext secrets) and absorbs accidental plaintext (a raw key pasted into `company.yaml` is encrypted on write, so it never lands in the DB in the clear). It does **not** defend against a compromised engine host that holds both the DB and the Tier A key (that host can decrypt — it must, to run; keep the key out of the store's backup domain — that separation is the point), a malicious operator with a valid key and API access, or in-memory extraction from the live process. The property: plaintext config exists only transiently in the encrypt/decrypt path and in the live engine's memory — never in durable storage.

### A revision stored before the keyring was required

Every write path seals, so the only plaintext revision a store can hold is one a build older than the mandatory keyring wrote. A node refuses to read it, naming the command that fixes it:

```bash
crewlet config seal                          # re-stores the active revision sealed
```

`crewlet config seal` writes a new revision holding the encrypted document — the one read of a plaintext revision the engine allows, because it is the operator vouching for the revision their own node was running. It's idempotent: a second run on an already-sealed revision is a no-op. Superseded plaintext revisions stay unreadable to every other reader, which says so and names importing the document again as the way to have it back.

### Rotation

**Rotating one secret** (e.g. a leaked LLM key) needs no host access: `PUT /config` (or a per-entity write) with the new value — the whole document is re-encrypted under the active key as a new revision.

**Rotating the master key** uses the keyring's multi-key support so there's no downtime:

```bash
crewlet secrets keygen --key-id 2026-07     # mint the new key
# add it to secrets.keys AND set active_key_id: 2026-07,
# keeping the old key in secrets.keys so its ciphertext still decrypts
crewlet config rekey                          # re-encrypt the document under 2026-07
# verify a clean boot, then drop the old key from crewlet.yaml
```

The document's envelope carries the id of the key that sealed it, so `rekey` decrypts under whichever key sealed it and re-encrypts under the active key. `crewlet config rekey --dry-run` reports whether it would re-encrypt without writing.

If you also use the [secret store](secret-store.md), run `crewlet secrets rekey` alongside `crewlet config rekey` before dropping the old key — its rows are sealed under the same keyring and would otherwise become unreadable.

### Reads and export

Every configuration read **redacts** credentials: `GET /config` (JSON and `?format=yaml`), `GET /config/revisions/{id}`, the revision diff, the entity reads (`GET /config/roles/{handle}`, `/config/units/{id}`, `/config/llm-providers/{key}`, `/config/mcp-servers/{name}`), the dashboard's `config` and `config_entities` queries, `crewlet config show` and `crewlet config diff`. The read path opens the whole document, then replaces every credential value with the literal marker `"__redacted__"`, so the caller sees the config's shape and never a credential. The `/org` view — read by every holder of `state:read` — needs no masking because it carries no credential field at all: it is an explicit projection of the charter and the organization tree (see [API endpoints](../reference/api-endpoints.md#get-org)). A revision sealed under a key the node does not hold is refused rather than served.

**What counts as a credential is structural.** A field is a credential because its Go type carries a `secret` tag (`secret:"true"`, or `secret:"content"` for a file's body, which is read whole — see [setup steps](code-sandbox.md#setup-steps--provisioning-the-box)), never because of how its value or its key reads, and a tag on a list or a map covers every element. The tagged fields today: LLM `api_keys`, the embeddings and sandbox `api_key`, a `cli-agent` provider's `cli.auth.token`, `cli.auth.credential_bundle` and `cli.env`, the integration tokens, admin tokens, API and app keys, webhook and signing secrets, a seat's `integrations.slack` bot token and signing secret, `integrations.mattermost.bot_token` and GitHub App private key and webhook secret, every `mcp_env` value on a seat or a unit, `mcp_servers[].env` and `mcp_servers[].headers`, `role.sandbox.env`, and each sandbox setup step's `env` and `files` (under `providers.sandbox.setup` and `role.sandbox.setup`). Everything else (URLs, hosts, flags, model names, the org chart) is served exactly as stored, including every toggle an operator set explicitly: a schedule kept with `enabled: false` reads as disabled, so sending the read back never re-enables it. A test fails the build when a field whose name reads like a credential, or a `map[string]string` named `env`, `headers` or `files`, is added without the tag, so a new credential field is masked by declaring it rather than by remembering to.

**Only a whole `${VAR}` reference is shown.** A credential field whose value is exactly one reference (`"${TRACKER_TOKEN}"`) is left visible: it names a credential and carries none, since the value it points at lives in the environment or the [secret store](secret-store.md) and never in this document. Every other non-empty value is masked, including one that embeds a reference beside literal text. `"Bearer sk-live-${SUFFIX}"` and `"sk-live-SECRET-${ROTATION}"` are legitimate (the resolver expands embedded references), and their literal half is a credential. A value using brace syntax the resolver ignores (`${line#host=}`, `${1}`, an unclosed `${`) is not a reference at all and is masked too. "Is a reference" is the engine's own grammar (`internal/envref`). The variable names an embedded reference carries are still listed, with their paths, by `GET /config/references`, which reads the unredacted document and answers names only.

A redacted `GET`, an edited field and a full-document `PUT` round-trip safely: the write path swaps each marker back to the currently stored value before validating, so a round trip never clobbers or exposes a credential. To *change* one, supply the new value (or a `${VAR}`) at that field.

**Members are matched by identity, not by position.** Matching by position meant a pure reorder handed each member its neighbour's credentials, silently, since the lengths still agreed and no marker was left standing to refuse. So every member that can name itself is matched by that name:

- **A seat by its handle, and a unit by its key, anywhere in the document.** One index covers the whole stored revision, so a seat moved from the root into a unit, from one unit to another, or back to the root keeps its credentials, and so does a unit moved under another unit. Reordering the roster, or adding a seat, restores every other member's credentials.
- **An MCP server, and a sandbox setup step, by name within its own list.** Both names are unique within their list: two MCP servers of one name are refused outright, and two setup steps of one name are refused on every write (an [admission rule](#what-a-stored-revision-is-held-to)). Reordering a list, or adding a member, restores every other member's credentials.
- **A list of bare credentials (`api_keys`) by position**, because it has no identity to match on. Change its length and the masks in it are refused rather than guessed.

**An identity that does not name exactly one member matches nothing.** A seat with a new handle, a unit with a new key, and an MCP server or a setup step given a new name carry no prior value of their own. An identity that is empty, or that the stored revision holds twice, is left out of the match entirely: picking either member, or falling back to position, would hand one member's credentials to another. In every one of these cases the mask stays standing and the write is refused with a validation error naming the field, so the caller writes the real value, or a `${VAR}`, there.

The untyped maps (`mcp_servers[].env` and `.headers`, `cli.env`, a sandbox step's `env` and `files`) are masked whole, every value in them, whatever its key is called. A host or a region set beside a token in one of those maps is masked with it; write it as a separate, untagged setting where one exists, or as a `${VAR}`.

`crewlet config export` runs on the host, where the keyring already is, and prints the revision as YAML with its credentials in the clear: it opens a sealed revision and renders the document, so the output is importable as it stands. That is what a restore or a migration needs, and it is the one command that prints credentials. `crewlet config export -redact` masks exactly what the reads above mask, for a dump that is safe to share.

---

## One company per engine

An engine runs exactly one company. It opens one store file, that file holds one `company_config` table, and that table has **at most one** `is_active=TRUE` row (zero in the unconfigured boot state; otherwise one). There is no tenant column and no row-level scoping: the revision you activate is simply the company the engine runs. The same rule governs the [secret store](secret-store.md): one company per coordination estate, so a variable name alone is the key.

To run a second company, run a second engine with its own database.
