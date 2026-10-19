# Configuration

Crewlet splits configuration into **two tiers** so a founder can evolve their company at runtime — add a role, swap an LLM provider, plug in a new MCP server, rotate an integration credential, update a policy — without redeploying or restarting the engine.

---

## Two-Tier Split

| Tier | Storage | Owner | Update model | Contents |
|------|---------|-------|--------------|----------|
| **A** | `crewlet.yaml` on disk | Ops / SRE | Restart-only | The store files and the object store's backend (`store.objects`), the stream and coordination slots, this node's identity and roles, API host/port and auth, the addresses people reach it at (`api.external_url`) and vendors and sandboxes reach it at, with the optional published listener that serves them (`api.public`, `api.public.external_url`), the secret keyring, the retention estate (`store.snapshot_dir`, `stream.tracker_retention` and backup ownership, `retention.backup_owner`), logging (level, shape and an optional rotating log file) |
| **B** | The store (`company_config`, versioned) | Founder | Live, API-editable, validated, versioned | Everything else: name, mission, vision, policies, the company's one clock (`timezone`), providers (LLM + embeddings), turn engine, learning, MCP servers, delegate `workers`, `scheduling`, `skill_variables`, the `tracker`, `knowledge` and `chat` backends, notification coalescing and rate limit, integrations (Slack / Mattermost / Jira / Confluence / GitHub / GitLab / Datadog / Atlassian, plus the Forge app id that verifies relayed Cloud events), the org chart (`roles:` and `units:`), and token budgets |

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
[identity directory](identity-and-access.md) binds somebody to it, at any stage
short of their removal, or while an open invitation names it, is refused `409
seat_held` naming each holder (an invitation by its id, never its address).
Free the seat first: move a person to another human seat or remove them — a
person's seat is never cleared — unbind a service account, or cancel the
invitation; then send the write again.

**A company with no human seat can admit nobody.** Every person holds a human
seat, and an invitation or a create names the one they will hold, so a company
that declares no `kind: human` seat has nowhere to put anybody. That is right
for a company its agents run alone, administered through the deployment's Tier
A tokens, so it is never refused: `crewlet validate` and every `/config` write
name it in an `advisory` warning at `roles`, and every node logs that warning
when it applies the company ([`config_advisory`](#what-an-applied-company-is-worth-knowing)).

### Mission, vision and policies

The company's **charter** is `mission`, `vision` and `policies`: what the
company is for, where it is going, and the standing rules it runs by. Every
seat is handed all three word for word at the top of its prompt, on every
round of every turn, so a seat never needs a knowledge lookup to know them.

```yaml
mission: Help friends split shared costs fairly.
vision: Splitting a bill is never the awkward part of a trip again.
policies:
  - Every amount is exact to the cent.
  - Work is tracked on the board, and nothing ships without acceptance criteria.
```

An administrator edits them in place on **Company › Principles**, which writes
through [`PUT /config/charter`](../reference/api-endpoints.md#the-charter-mission-vision-and-policies):
a new revision of the whole document like any other write, conditional on the
charter itself rather than on the revision, so a draft is refused only when
somebody changed the charter meanwhile.

Because every character reaches every prompt, the charter is bounded, in
characters: the mission at 1024, the vision at 2048, at most 25 policies of at
most 1024 each. The mission and each policy are one line, since a seat reads
the mission inline and each policy as one item of a list; the vision may be
written in paragraphs, and the prompt quotes it line by line so no line of it
reads as the prompt's own. A policy may not be blank or repeat another word for
word. Every bound is counted on the value as it is stored, with the space
around it trimmed.

These rules judge what an edit CHANGES, against the stored revision it
replaces, so an old fault never locks a company out:

- **An edit through the API** (`PUT /config/charter`, `PATCH /config`, `PUT
  /config` from the org builder or a script, a per-entity write, `/setup`) is
  refused only for a part it changes and leaves at fault: a mission or vision it
  edits, a policy it adds or edits, a policy count it raises past the bound, a
  repeat it adds. Reordering policies changes none of them. A write that leaves
  a faulty part untouched (a budget, a seat, another policy) is admitted, with a
  warning naming the part in its answer.
- **An import never refuses for the charter**: `crewlet config import` into a
  stopped node, `-company` and `-import-company` at boot, a restore and a
  migration. A file that ran under an earlier build imports with its faults
  printed as warnings, and every node logs them as `org_admission_warning` when
  it applies the company, because refusing there would leave an upgraded node
  unable to boot on its own file. An import sent to a running node is too: the
  CLI sends it as `PUT /config?import=true`, which judges the charter
  warn-only, is `admin`'s alone and records the revision as imported from a
  file. The dashboard never sends it.
- **`crewlet validate`** prints the faults as warnings and exits 0: it cannot
  know what the store holds, so it cannot know whether a write of the file
  would edit the faulty part.

A stored revision that breaks one still applies.

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

Resolution order: `node.id` (a `${VAR}` reference works here, as in any
Tier A text field, and the id it resolves to is judged by the rules below —
see [Environment Variable References](../getting-started/configuration.md#environment-variable-references)
for how one reads in a number or a boolean field) → the `CREWLET_NODE_ID` environment
variable → `node-0`. You do
not need to set it to run a single engine. It starts with a letter or a digit
and holds only letters, digits, `.`, `_` and `-`, at most 64 characters,
because it ends up in broker consumer names and subjects — and an operator
gesture naming a node, such as `crewlet retention evict`, holds the id it is
given to the same rule. One id of that shape is **reserved**: `operator`,
which the fleet's backup register files the operator's
[acknowledgements](../guides/retention.md#a-company-that-never-backs-up-never-trims)
under, so a node of that name would have its own backups read as one.

It must be **stable across restarts**, which is why it comes from the
deployment rather than being generated per boot: anything the process
registers under its identity would otherwise be orphaned on every restart.
In Kubernetes use the pod name — a StatefulSet ordinal is ideal; under
systemd, the host name.

#### `node.roles`

What this process is willing to do. Four roles, and the default is all
four — one process running a whole company, which is every single-node
deployment:

```yaml
node:
  id: "${CREWLET_NODE_ID}"
  roles: [seats]              # a stateless satellite: agents only, no data
  labels:
    zone: eu
```

| Role | What it does | What a fleet loses without it |
|---|---|---|
| `data` | Keeps the company's durable state on this node's disk: a copy of the replicated estate and the event log. The company's files are not under it: they are in the [object store](object-store.md), which on the default `nats` backend is a stream the broker's members keep. It says nothing about the broker (see below). `ingress` and `workers` require it | Every seat's tracker, knowledge and chat tools, which a node without `data` answers through one that has it |
| `ingress` | Serves the HTTP API: webhooks, the dashboard, the REST endpoints, and the tool bridge (`/mcp/{token}`) every seat's executor reaches its tools through from its box, which relays each call to the node holding the run's seat — served where the node is given `CREWLET_MCP_BRIDGE_URL`, or, for its own `direct` boxes alone, where it runs `seats` too ([Running a Fleet](../guides/fleet.md#what-a-fleet-needs)). A node without it serves the network only its [probes](../reference/api-endpoints.md#probes-on-a-node-without-ingress) on `api.port` | No integration can reach the company, no executor run can reach its tools, and there is nothing to look at |
| `seats` | Claims seat leases and runs agents, and answers their executor runs' tool calls: a box off the node's host reaches it through an `ingress` node's tool bridge at the fleet's `CREWLET_MCP_BRIDGE_URL`, and a `direct` box dials its own node's listener on loopback — on a node without `ingress`, `api.port`, where the node serves its own host the bridge beside its probes | Every trigger queues up unread |
| `workers` | The company-wide [singleton duties](seat-ownership.md#singleton-duties): the scheduler tick, the maintenance sweep (retention, removed-seat mailbox retirement, and forgetting the chat read state of a human seat the company no longer has), the state logs' trim, the embedding duty, the sandbox waiter, the integration reconcile loop, the object store's collector, the trash reaper, the identity sweep, Chatlet's message horizon, and the learning background passes (episode lifecycle, skill curation, clustering and promotion) | Nothing fires on a schedule, no `e2b` sandbox run is collected and no node that left for good has its local runs ended (a `direct` or `container` box is its launching node's own, polled there under no duty), no table is swept, no state log is trimmed (each grows until its ordinary writes are refused `log_full`), no knowledge is embedded for semantic search, no integration is reconciled, no unnamed object is collected, nothing in a trash is purged past its term, the identity estate's retention never runs, and no chat message is pruned at its room's horizon |

Subtracting a role subtracts it from **this node, never from the
company**, so the fleet as a whole still needs every role somewhere. That
is a shape no single node's config is wrong for, and every symptom of
getting it wrong is an absence — so the engine checks it against live
node presence and logs `fleet_role_unmanned` when nobody is doing a job.
A node that does not run seats is also left out of the denominator its
peers divide the seats by; counting it would strand the difference.

**`data` is the one role that is a promise about the disk rather than about
work.** A node without it keeps nothing that has to outlive it, and Tier A
holds it to that whatever its broker: `store.scratch: true` is required (its
store is deleted at every boot), and `store.replicated_path` and
`store.snapshot_dir` are refused. `store.objects` is not: it names the one store the whole fleet keeps its files in, and a node without `data` uploads and downloads through it directly. See
[Running a Fleet](../guides/fleet.md#nodes-that-hold-no-data).

**The broker is not a role.** How a node's broker takes part in the fleet's
is its *broker kind*, derived from the `stream` block and from nothing else:

| Broker kind | The `stream` block that makes it | What the broker is |
|---|---|---|
| `member` | embedded, with no `stream.leaf.urls` (a solo node included) | runs JetStream in this process, holds stream replicas, votes in the metadata group |
| `leaf` | embedded, with `stream.leaf.urls` | JetStream off: reaches the members' across a leaf link, holds nothing, votes in nothing |
| `client` | `type: nats` | a plain client of an external cluster somebody else runs |

Every node advertises its kind on its presence lease (`crewlet fleet broker
list` and the dashboard's **Settings › Nodes** screen show it), and a node advertising a
kind this build does not know — a newer build's — shows as `unknown`. The rules that are about the broker
are asked of the kind, whatever the roles: a member — a solo node included —
must set `stream.store_dir`, because it holds the fleet's streams for every
node that reaches it, its own state logs among them, and an in-memory stream
is a node whose first restart leaves it unable to serve; a leaf carries no
cluster block, no store directory, no leaf listener and no declined
`stream.sync` — it runs no JetStream, so it has no file store to keep.

Three pairings of roles and broker are **refused**, and each refusal names
both ways out: a data node on a leaf, a broker member without `data`, and
`ingress` or `workers` without `data`. Every data node holds the whole estate
as a member of the broker, and a node without `data` reaches the estate
through one — so a node without `data` on an embedded stream joins the fleet
as a leaf through `stream.leaf.urls`. Two valid shapes are warned about, by
`crewlet validate` and by every start of the node, which logs them as
`config_advisory`: a member whose peer list names more than four other
members (no stream keeps more than five copies, so a sixth member is a voter
holding nothing — a fleet grows by adding leaves), and a fleet node that left
`node.roles` undeclared and so runs every role. The start judges the roles it
runs with, so a fleet node given `-roles` is not told they are undeclared.

#### `node.labels`

Free-form facts about where this process runs, matched by a seat's
[`role.placement`](../guides/fleet.md#placement) selector. Values are
strings and are compared exactly — so both the key and the value are
trimmed of the whitespace around them before anything reads them, and two
keys that are one key once trimmed are refused rather than collapsed. A
key is at most 63 bytes — Kubernetes' limit on a label name — and holds no
whitespace or unprintable character anywhere, because it is matched
exactly wherever it is named: a key with a space inside it is refused
rather than accepted as a label nothing could name. Labels are advertised
to peers on this node's presence lease, so a label change takes effect
one heartbeat after the restart that made it — not at the next config
activation.

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
   `-log-format` / `-debug` flag folded into it, and only where the flag was
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
4. Hold the Tier A this process is about to run to Tier A's rules once more —
   the file with every run flag applied: `-roles`, `-api-host`, `-api-port`
   and the four logging flags. The file was validated as it loaded, but a flag
   reaches every rule that reads what it moved — `-api-port` on a file with no
   `api:` block turns on an API that has to name `api.external_url` — so
   `crewlet run` asks as soon as the flags are in, before it binds or opens
   anything, and its refusal names them. Once that has passed, every advisory
   Tier A raises is logged, one `config_advisory` line at `WARN` each — after
   the file is open, so they are in it, and judged on that same document (see
   [what every start says](../reference/cli.md#every-start-says-what-its-tier-a-is-worth-knowing))
5. Bind `api.host:api.port` — and `api.public.port` and `api.artifacts.port`
   on an `ingress` node that sets them — and log `api_bound`. A port somebody holds fails the boot
   here, before anything slow has run. From now until step 13 the sockets
   answer what a starting node answers: `/health` `503` with
   `"phase": "recovering"` until step 11 and `200` with
   `"phase": "preparing"` from step 11 to step 13, `/ready` `503 starting`
   throughout, and every route the node will serve `503 starting` with a
   `Retry-After` (see
   [While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting))
6. Decide, once, what the Tier B file on the command line is for and what the
   engine starts on. The engine's start and the import in step 12 both take
   this one decision, so the engine can never start on a file the import then
   ignores. On a data node, read the revision its own store boots on
   (`is_active`) and the fleet's revision as the node last saw it
   (`is_fleets`), which opens and migrates that store and closes it again: the
   engine opens its own. The process **keeps the store's lock** from this read
   until the engine has opened the same file in step 8, so nothing can change
   what the engine starts on in between. From here on a command that opens
   this node's store meets the starting node exactly as it meets a running
   one: `crewlet config activate`, `rekey` or `show`, `crewlet migrate` and
   the rest are refused, naming it; `crewlet config import` and the commands
   that reach the fleet's secret store (`crewlet secrets`, `crewlet llm`, the
   provisioners) go to its API instead, which answers `503 starting` until
   step 13 — run them again once the node serves. A start with
   `-import-company` reads no store here and takes no lock until step 8. The
   revision it boots on is the last one its engine
   applied, or a change an offline command made on it (see [Versioned
   Revisions](#versioned-revisions)), and that is what the engine starts on —
   at the instant of the activation it applied, or at no instant for an
   offline change the fleet has not taken yet, which the first apply of the
   activation that publishes it stamps. So a node that refused the fleet's
   revision starts on what its boot row names — the one it was serving,
   unless the write moving the row to that one was still being retried when
   the node stopped, which leaves the one before — and never on the one it
   refused. A
   `-company` file beside it is either that revision byte for byte or one the
   store's company outranks, which is logged as `company_seed_ignored`. A node
   that boots on nothing but has seen the fleet run a company — it joined and
   refused that company's revision, or never finished applying it — starts
   with no company, and a `-company` file is not a bootstrap there either: it
   is ignored the same way, since a company exists. With neither, or with
   `-import-company`, the engine starts with no company and the file is
   considered for import in step 12, once the fleet's pointer can be read.
   `-import-company` reads
   nothing from the store, so a stored revision this build cannot run does not
   refuse the boot that replaces it. A node without `data` reads nothing here,
   since its store is scratch, and starts with no company. On a
   [managed](#managed-configuration) deployment a `-company` file is not read
   at all, and the node decides as one given none. **The engine never starts
   on a Tier B file**: building a company starts its tool servers, restores
   its coding CLIs' logins and opens the store at its embedding width, and
   none of that is undone by the reconcile that would replace it
7. The engine holds Tier A to its rules again as it starts, before it starts
   the stream or opens a store of its own, because it is the one thing that
   runs Tier A: a bootstrap a tool built in code reaches it having passed no
   rule
8. Start or dial the stream, then open the store file. An embedded member
   first takes `stream.store_dir`'s lock (`crewlet-broker.lock`, inside the
   directory), because nats-server takes none and would accept a second
   broker writing the same store: a directory another broker holds — another
   engine's, or a `crewlet` command's reaching the fleet's store while this
   engine was stopped — refuses the start, naming its holder. The broker then
   recovers its file store, which takes as long as the store is large. A node
   without `data` opens a scratch store, deleted at every boot, and joins the
   members' broker as a leaf
9. Run migrations — every file, in one pass. There is no lock and no phase ordering to serialize: this process owns its file, so nothing can be racing it, and no DDL depends on a value only the config knows. Embedding columns are declared as plain blobs and the vector width is validated in Go against the company the engine runs at write time, so a schema step never has to read the config first (see [`crewlet migrate`](../reference/cli.md#crewlet-migrate)).
10. Start the **core runtime** — on every data node, with a company or without one: the [state log](../guides/consistency.md) for **every** registered domain, the node gate over every identity-claiming log, the [identity estate](identity-and-access.md) with the loops that keep this node's view of it, and — on a node that publishes — the log's own trim and the identity duties. None of it depends on a company, and a node with none is exactly the one that has to sign a Tier A token's session in — `ingress` requires `data`, so the node anybody signs in on always has it — because that session is what the dashboard creates the company through; the company's first person is invited or created once it declares the human seat they will hold. It is **every** domain rather than the identity estate alone because a join replaces the whole replicated file and a snapshot names every registered domain, and because the trim counts nodes per log: a node that applied part of the register could neither adopt nor donate, and would pin the rest of the logs' trims for as long as it was up. A node without `data` starts none of it: it applies no log, and reaches the estate — the identity directory included — through a node that holds it ([A node that holds no data](scaling.md#a-node-that-holds-no-data))
11. **The node has recovered what it holds.** It logs `engine_recovered`, and
    `/health` turns `200` with `"phase": "preparing"`: everything above is the
    node's own and is bounded by the deployment's startup probe — a data
    node's join in step 10 included, when a fleet's logs can no longer bring
    its rows level, which is why that probe is sized by
    `stream.tracker_retention.rejoin_window` on a data node of a fleet — and
    everything below is the company's, which no probe bounds — a part that
    waits on something else has a bound of its own, and the apply has none
    beyond its parts' (see
    [Sizing the startup probe](../guides/deployment.md#sizing-the-startup-probe)).
    The state logs step 10 started catch up in the background from here, and
    hold the node's seats rather than its probes. Then, with the revision
    step 6 decided to start on:
    - **A company**: apply it, which spawns the full company: the sandbox backends it configures, the **Worklet, Doclet and Chatlet halves** it declares (their writers and readers over the logs step 10 already applies, their lexical index and chat's own keyword index, change feeds, embedding duty and chat's message horizon), and its seats' tools, the shared MCP servers among them, each within its own `startup_timeout_seconds`
    - **No company**: the engine stays in the **unconfigured** state — the API serves from step 13 so an operator can push the first revision via `PUT /config` or `crewlet config import`, and a Tier A token's session signs in — which is how the dashboard creates the company. Nobody can be invited or created yet: every person holds a human seat the company declares, so that waits for the first revision. A node whose file step 6 decided to import builds that company in step 12, before any seat is claimed
12. Start the [control plane](control-plane.md): import the Tier B file where step 6 decided to — `-company` only where the fleet runs no company either, create-only, so of two nodes starting a fleet's first company one wins and the other defers to it; `-import-company` where it differs from the revision the fleet runs, unconditionally, the one activation that expects nothing of the pointer — or otherwise publish what this node knows, by a compare-and-set against what it was built on: a change an offline command made on this node, only while the fleet still runs the revision that change was built on (or the earlier offline change it replaced, where a start published that one and could not confirm it); the company this node knows, create-only, where the fleet's pointer is gone; and the fleet's revision this node last saw, where the pointer is a **restore** — put back by a node after the coordination store lost it — of a revision this node holds that was activated before it: a pointer recreated over a fleet that had moved on. A pointer somebody chose is the company, however old its instant reads. A change the fleet moved on from is not published, and every boot says so at warn as `offline_revision_not_published`, for as long as the node boots on it (see [Control Plane § What a booting node publishes](control-plane.md#what-a-booting-node-publishes)). A file about to be imported is first held to every rule a written document meets and **built as this node's apply would build it** (the company over this node's `${VAR}` chain, and the sandbox catalogue it configures), because nothing else builds it before the whole fleet has been pointed at it: a file this node cannot build — a `${VAR}` that resolves to nothing where a backend refuses an empty value, a coding CLI whose profile cannot be loaded, a sandbox cell this host cannot run — refuses the boot with nothing stored or activated. Then one reconcile against the activation pointer — on a node that booted with no company and finds one active, the file it has just imported included, this is where the company of step 11 is built — before the loop that polls the pointer, and a broadcast `crewlet.config.revision_activated` nudge that wakes it early, run from step 14 on. That reconcile failing does not fail the boot: `initial_reconcile_failed` says whether the node is still serving the company it started on or, having started on none, serves no company until a later tick applies the fleet's revision — a part of the apply that could not start (the inbound edge, the native halves) is started again by that tick
13. Hand the listeners the API inside this process — auth middleware, the `/config/*` routes and the rest — and, when `api.public.port` is set, the routes outside parties call (`/webhooks/*`, `/otlp/{token}`, `/mcp/{token}`) on that second listener instead (see [Deployment → Exposing webhooks without the admin API](../guides/deployment.md#exposing-webhooks-without-the-admin-api)). A node without the `ingress` role serves the network only its `/health` and `/ready` probes on `api.port`, and beside them, to its own host's loopback alone, the tool bridge and telemetry receiver its own `direct` boxes dial: an executor run in a box off its host reaches its tools through an `ingress` node's bridge, which relays each call over the broker. Logged as `api_listening` (`api_probes_listening`)
14. Start the engine — claim this node's seats, each with its per-role MCP children — and the reconcile loop

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

`crewlet run` defaults `-config` to `./crewlet.yaml` and `-company` to `./company.yaml` in the working directory; naming a path is only needed when a file lives elsewhere. **A missing `./company.yaml` is not an error** — what a node starts on is its store's to say, never the file's, so a node with no Tier B file boots on the revision its store boots on, or unconfigured when it holds none.

`-company` only ever **bootstraps a company that does not exist yet**: none the node's store boots on or has seen the fleet run, and none the fleet runs. Once a company exists the file is ignored, with a `company_seed_ignored` warning naming it and the revision that outranks it, and the node starts on the company that exists — the revision its store boots on, or with none until its first reconcile applies the fleet's — so a restart cannot revert a change made live, not even for the moments before that reconcile. An empty store is not a company with none: a fresh node joining a running fleet has one, and so does every node without `data` at every boot, and each defers to the fleet's company (silently, when the file is the very revision the fleet runs). The warning's hint names the other intents, which the table gives:

| I want to… | Use |
|---|---|
| start a company at its first boot | `crewlet run -company company.yaml` (the default) |
| change a **running** fleet, no restart | `crewlet config import company.yaml` while this node's engine is up, or with `-api` naming any node that is: it goes through that node's `PUT /config` |
| change the company from a **stopped** node | `crewlet config import company.yaml`, then start the node: it boots on the import and publishes it only while the fleet still runs the revision the import was built on ([below](#offline-commands-make-an-offer)) |
| make a file the company again on restart, whatever the fleet runs | `crewlet run -import-company company.yaml` |

`-company` and `-import-company` together are refused: they ask for opposite things, and picking a winner silently would be picking it about the flag that overwrites a running company.

---

## Unconfigured State

Until a node's engine has a company — none in its store to boot on, and none applied since it started — the engine holds an empty `Organization` (no name, no roles, no units), an empty provider map, no running MCP processes, no integration clients, no notification transports, and no Worklet, Doclet or Chatlet halves.

**What stays running:**

- The Tier A resources — the stream, the store's files, the API socket — all up.
- The **core runtime**, on a data node ([Bootstrap Sequence](#bootstrap-sequence) step 10): every domain's state log, the node gate and the identity estate, and on a node that publishes the trim and the identity duties. So an unconfigured node **signs a Tier A token's session in** — which is how the dashboard creates its company — and can be evicted from and readmitted to the logs it is counted on. What it cannot do yet is put anybody on a seat: every person holds a human seat the company declares, so an invitation or a create naming a seat answers `409 no_active_revision`, as `GET /iam/seats` does, until a company declaring one is applied. Then its first person is invited or created onto that seat under a Tier A token, exactly as everybody after them is (`crewlet iam invite -seat`, `crewlet iam create -seat`).
- The API's `/config/*` routes and the node's [reconcile loop](control-plane.md) — which is exactly what wakes an unconfigured node when the first revision lands.
- A structured log line carrying `state=unconfigured`, so the unconfigured posture is obvious in logs and on the dashboard.

**What returns degraded responses:**

| Route | Behaviour while unconfigured |
|-------|------------------------------|
| `GET /health` | `200 {"status": "unconfigured", "node": "node-0", "configured": false, ...}` — 200 because the status code is liveness; read `configured` for readiness |
| `GET /ready` | `503 {"ready": false, "configured": false, "reason": "unconfigured", ...}` — an unconfigured node cannot verify a webhook signature, so it stays out of rotation. `reason` names what took it out — `unconfigured` here; every reason `/ready` can name, in its order of precedence, is in [the routes table](../reference/api-endpoints.md#routes) |
| `GET /config` | `404 {"error": "no_active_revision"}` with a hint, while the fleet has no activation pointer and this node has never seen one name a revision. Every `/config` answer is the fleet's ([Live Propagation](#live-propagation)), so a node that is unconfigured beside a fleet that runs one — it joined and has not applied it yet, or refused it — answers that company like every other node, and one that cannot read the coordination store answers `503 unavailable` with a `Retry-After` |
| `GET /config/revisions` | `200 []` from a store that holds no revision |
| `PUT /config` | Accepted, and creates the first revision, as long as the FLEET has no company either: the write is create-only at the pointer, so if another node's first company lands first it is refused `409 revision_advanced` naming that company (`412 already_configured` with `If-None-Match: *`), its own revision kept as history. Where the fleet runs a company, this node answers it, so a write is built on it like any other and `If-None-Match: *` answers `412 already_configured` naming it. Send no precondition, or `If-None-Match: *` to insist nothing is configured yet; an `If-Match` names a revision to match, so with no company it answers `412 no_active_revision` |
| `POST /config/revisions/{id}/revert` | `404` — no revisions exist yet |
| Per-entity routes (`PUT /config/roles/{handle}`, `/config/units/{id}`, `/config/llm-providers/{key}`, `/config/mcp-servers/{name}`) and `PATCH /config` | `409 Conflict` while the fleet runs no company — they edit a document, and there is none; initialise via `PUT /config` first. Beside a fleet that runs one, they edit that company's document |
| `GET /agents`, `GET /tokens/breakdown` | `200` with empty lists / zero counters |
| `/auth/*`, `/iam/*` | Served exactly as on a configured node — they are the core runtime's. What names a seat answers `409 no_active_revision`, because the running company's human seats are what it is checked against and there are none: `GET /iam/seats`, an invitation, a person's create, and a bind |
| `/worklet/*`, `/doclet/pages/*`, `/chatlet/*`, `/operator/mcp`, and the work, page, chat and search questions | `503 no_active_revision` with a 15-second `Retry-After`; over the socket, an `unavailable` frame naming the same `refusal` with the same `retry_after` and words: the routes are **mounted** and say why they cannot serve rather than answering the `404` of a route that does not exist. They read Worklet's, Doclet's and Chatlet's halves **per request**, so the first company's apply brings them up under the same API with no restart; a company that keeps its tracker, wiki or chat on a vendor answers that half's routes (`/worklet/*`, `/doclet/pages/*` or `/chatlet/*`) with the `404` a route with nothing behind it answers from then on, while `/operator/mcp` goes on serving whatever it keeps natively beside the seat controls every node serves |
| `POST /webhooks/...` | Answered **before** the signature check — a node with no revision has no secrets to check against, and verifying first would answer every delivery with the no-secret refusal and its five-minute wait — as `503 no_active_revision` with a 15-second `Retry-After`, so the sender **retries**; `webhook_rejected_unconfigured` is logged at WARNING with the source and the event. A 200 here would tell the sender the delivery was accepted while discarding it — silent, unrecoverable loss the moment one process of several has simply not caught up yet. See [Webhook refusals](../reference/api-endpoints.md#webhook-refusals) |

Transition out of unconfigured: the first activation moves the pointer → the reconcile tick picks it up → the apply runs → the spawn cascade executes, including everything boot starts only for a company it already has: Worklet's, Doclet's and Chatlet's halves with their tools, the projects, knowledge containers and rooms the org chart's units declare, the code sandbox's coordinator and completion poll, the reflect dispatcher and the inbound edge (see the `native`, `sandbox_runtime`, `learning`, `integrations` and `maintenance` stages below) → the engine is fully alive, with no restart. The dashboard carries the unconfigured state in always-on chrome (a caution banner saying inbound webhooks are being refused, an engine pill that says so, and the first row of the overview's attention queue), and it clears automatically on the next health tick once `/health` reports `configured: true`. See [the attention queue](../reference/dashboard-design.md#the-attention-queue).

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
- The revision that adds the first provider to a company with an agent seat
  must configure `providers.sandbox` in the same edit — every agent seat's
  executor runs in one of its cells — or it is refused at `providers.sandbox`,
  naming the first such seat. `local: {}` with `default_run_in: direct` is the
  engine host itself (see [Code Sandbox](code-sandbox.md#configuring-it)).

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

When a new revision is activated (via `PUT /config`, `PATCH /config`, a per-entity write, a revert, or `crewlet config import` through a running node), the revision is stored and the fleet's **activation pointer** is then moved to it; the pointer's own KV sequence *is* the epoch, so the append and the flip cannot come apart. Every node polls that pointer and converges onto it; a broadcast `crewlet.config.revision_activated` event wakes the poll early but carries no work. A `crewlet config` command run while the engine is stopped cannot move the pointer, and what it writes is an [offer](#offline-commands-make-an-offer) the node's next start publishes.

The two steps are **not one transaction**, and they span two stores: the node's own database and the coordination KV. That ordering is deliberate: a crash between them leaves a revision nothing points at, which is inert and replaced by the next activation, where the other order would point the fleet at bytes no node had stored. The revision is stored as history, and it becomes what a node boots on — the writing node included — only once that node's engine has applied it: a write that lost its compare-and-set stays inert history, and a revision a node refused to apply is never what that node starts on. See [Control Plane § The company, and what each node keeps of it](control-plane.md#the-company-and-what-each-node-keeps-of-it).

There is **no leader**, so any node's API may write. What keeps two operators from silently overwriting each other is that the flip is a **compare-and-set** against the revision the write was derived from: the loser gets a `409` naming what won, rather than a `201` for a change the fleet never took. See [Concurrent writes](../reference/api-endpoints.md#concurrent-writes).

**That revision is the fleet's, on whichever node serves the write.** The company is the pointer, so `GET /config` and its entity-tag, the revision list's `is_active`, a diff against `active` and the base every write is built on all answer the revision the pointer names, never the one the serving node happens to run. Every node answers the same: one that has not reconciled since the pointer moved answers from the body the pointer carries, and one that refused the fleet's revision still answers it, so the read after a write answers the write and an `If-Match` taken from one node's entity-tag lands on any other. What each node runs is a separate fact, which [`GET /fleet`](../reference/api-endpoints.md#get-fleet) gives per node. When the coordination store cannot be read, a `/config` request that needs the fleet's revision — every read above, and every write — answers `503 unavailable` with `Retry-After: 2` rather than a guess, since "the fleet has no company" is the guess that sends a create-only write to replace the company it has. See [Control Plane § Every config answer is the pointer's](control-plane.md#every-config-answer-is-the-pointers).

**The nudge carries no work, and that is the load-bearing part.** A group subscription is a *work queue* — the contract says exactly one member of a group receives each message — so a revision delivered that way would be applied by whichever process won the message while every other node kept serving the previous company indefinitely. Polling a pointer inverts that: every node reads the same value and converges on it, and a lost nudge costs a poll interval rather than a revision. The full mechanism — the activation pointer and its epoch, what a lagging node does about its own traffic, and the operator surface — is [Control Plane](control-plane.md); what follows is the apply itself.

### The engine half

Converging applies the payload. `Engine.Apply` is a **straight line with no
comparison and no early return** — there is no apply lock, no payload
short-circuit and no rollback of captured state. It rebuilds the whole epoch,
in a fixed order, and names each stage it got through.

1. **`secrets`** — re-read the secret store and install a fresh resolver snapshot. **First**, because re-activating an unchanged revision is the documented [rotation gesture](secret-store.md): the payload has not moved, so the only thing that can have is what its `${VAR}` references resolve to.
2. **`company`** — validate and build the new epoch, resolving `${VAR}` where each provider is *constructed*. A refusal here changes nothing: this node keeps serving the previous epoch.
   A company with no `providers.llm` is not refused: it builds with no model registry (see [A Company With No Model Provider](#a-company-with-no-model-provider)).
   The revision's sandbox catalogue (`providers.sandbox`) is built straight after, and one that cannot be built refuses the apply there — reporting `secrets, company` and nothing else, since nothing below has touched the node yet. On **every** node, whether or not it runs a sandbox yet: the alternative publishes a company whose agent seats' executors launch into a box nobody can mint.
3. **`native`**: bring up Worklet's, Doclet's and Chatlet's **halves** on a node running none, for its first company on them: their read and write sides, the lexical index, chat's own keyword index, and (on a node that publishes) the embedding duty, chat's prune duty and the change feeds. Not their logs: every data node has applied every domain's log since boot, company or none ([Bootstrap Sequence](#bootstrap-sequence) step 10), so what the first company brings up is the writers and readers over logs the node already holds; a node without `data` builds none of it, and its seats' tools reach every half through a data node. **Before** `tools`, because Worklet's and Doclet's tools are registered only where the halves exist, and before `integrations`, whose inbound edge includes their parsers. A start that fails refuses the apply. **Conditional:** reported only on the apply that started it (once per process), and never at all on a node that met its company at boot. The halves follow the fleet's logs rather than a revision, so a later refusal of the same apply does not take them down, and which halves run are the ones that first company declared: changing `tracker.backend`, `knowledge.backend` or `chat.backend` after that still takes a restart.
4. **`sandbox_runtime`**: bring up the [code sandbox](code-sandbox.md)'s coordinator and completion poll on a node running neither, for the first revision whose `providers.sandbox` reaches a cell, and prepare the seats this node **already** holds: each was taken before there was anything to prepare, so each now attaches its completion topic and recovers the runs recorded on it, as a seat taken afterwards does on its way in. A seat whose preparation fails is handed back (a voluntary `unprepared` release), so its next acquisition, here or on a peer, runs the whole of it. **Before** `tools` for the reason `native` is: an executor run launches only where the runtime exists. A start that fails refuses the apply. **Conditional:** reported only on the apply that started it (once per process), and never at all on a node that met a sandbox company at boot. Like Worklet's and Doclet's halves it is a fact about the process rather than a revision, so a later refusal of the same apply does not take it down.
5. **`tools`** — equip the new epoch with this node's builtins. An epoch is published, never mutated, so each one gets its own registry; a node that equipped only its first would serve a company whose agents silently lost every builtin at the first config change. (The one part of a published epoch that does change is its shared MCP servers' tools: a server that comes up after its start failed, or comes back from a crash with a different tool list, is refiled into the current epoch's registry without an apply. See [A server that dies, or will not start](../guides/tools-and-mcp.md#a-server-that-dies-or-will-not-start).)
6. **`learning`**: rebuild the reflection workers against the new org — built here, and swapped into the running dispatcher only at `epoch`, once the revision's configured credential bundles are restored, so no reflection pass runs the revision's models before its login is in place or for a revision a later stage refuses. Deliberately cannot fail the apply: reflecting against a stale org is a far smaller wrong than not reflecting. The one exception is a node's **first** company: a node that booted with none has no reflect dispatcher to swap workers into, so this stage builds it, and a build that fails refuses the apply for the reason it fails a boot (a company served without it learns nothing while looking healthy). What feeds the dispatcher is not this stage's: each seat's reflection subject is attached by the node that acquires the seat, beside its mailbox.
7. **`sandbox`** — swap the sandbox *manager* only. The coordinator and waiter hold this process's busy set and poll loop; rebuilding them would forget which seats are mid-run and start a second loop over the same rows. The swap carries the backend of any cell the revision dropped as *retired*, for the runs still on it, and a revision with no `providers.sandbox` keeps the last manager for the runs in flight (see [Code Sandbox](code-sandbox.md#engine-provider--providerssandbox)). It cannot fail — the catalogue was built at `company` — and it runs after every stage that can refuse a node already serving a company, so a refused apply never leaves this node launching through a catalogue its current epoch does not have. **Conditional:** only where this node runs a sandbox runtime, from its boot or from a `sandbox_runtime` start.
8. **`integrations`**: rebuild the inbound surfaces against the new epoch (Confluence, Datadog, Jira, GitLab, GitHub, and the two chat transports, Slack on every apply and Mattermost when a value it is built from moved), so work items route by the new org chart rather than the boot-time one. A third-party app the revision **retires** (its block removed, or `enabled: false` for GitHub and GitLab) has its parser unregistered, so its deliveries route to no seat; GitHub's and GitLab's webhook routes then answer `503` rather than verifying and ingesting a delivery the routing half would drop. Confluence additionally loses its searcher, or every seat would go on searching a wiki the company has removed, with the credential it revoked. Confluence and Jira re-derive a **lead map** from the org (space and project key to unit lead), which is what an unrouted page or issue falls through to. GitLab and GitHub have no lead map; theirs re-resolves the engine credential and the participants lookup that fans a thread out to the seats on it.
   On a node that booted with **no company** there is no inbound edge to rebuild: boot starts one only for a company it already has, because the inbound consumer group is fleet-wide and a node with no parsers would take deliveries its peers can route. So the node's first apply **starts** the edge here, through the same function boot runs, and every later apply reconciles it. A start that fails (the broker refuses the subscription) refuses the apply like a refused build, before the epoch is published, and takes down whatever it had brought up, so the retry starts from nothing.
9. **`epoch`** — publish the new epoch. Immediately before the swap, with every refusal behind it, each [`providers.llm`](llm-backends.md) entry's configured credential bundle is restored into its credential directory where that holds no login (and a [logout](llm-backends.md#1-broker-the-vendors-own-login-any-cli) has not withheld it), and the reflection workers built at `learning` are swapped in — so only a revision about to be published writes a login, and its first call finds it. This is the swap; everything before it built, everything after it reads the now-current company.
10. **`maintenance`**: rebuild the retention sweep, on a node's **first** company (or the apply that started Worklet's, Doclet's and Chatlet's halves). Its job list is read once, and on a node that booted unconfigured it was read before there were Worklet and Doclet halves to contribute Worklet's repairs and the inbox sweep, or a company to state the conversation horizon, so it swept none of those until a restart. Every operation ledger is swept from boot either way: the ledgers are the core runtime's. **After** the swap, because it reads those horizons off the current company. The old sweep stops, its in-flight tick waited out, before the new one starts. **Conditional:** only on that first apply, and only on a node that publishes.
11. **`learning_passes`**: hand the background learning loops (episode compaction, the skill curator, clustered synthesis and promotion) the passes this revision turns on, built from its models, credentials and knobs. **After** the swap, because the loops walk the current company's seats: handed over earlier they would run the new revision's passes over the previous company's roster, and a refusal later in the same apply would leave them there for a revision this node never served. The loops themselves are armed once per process and keep their clocks across an apply (see [Agent Learning](agent-learning.md#trigger-threshold-gated-on-a-slow-loop)), so this is also where a node that booted with no company, or a company that gained its first provider, starts running them. Reported on every apply, including one on a node with no store or no worker role, which has no loops to hand anything to. Not part of the convergence below, because nothing it builds is derived from the org chart.
12. — 20. **the convergence**: everything derived from the *published company itself*, brought up to it. These are not the apply's own stages: they are the same list a node's boot runs, through the same function, because a boot publishes a company exactly as an activation does. They are described under [What follows a published company](#what-follows-a-published-company) below, and an apply names each of them in `applied_subsystems` in that order: `parties`, `seat_identities`, `seat_tools`, `tracker_projects`, `knowledge_containers`, `chat_unit_rooms`, `mailboxes`, `scheduler`, `published`. `seat_tools` hands each held seat's per-role children their reconcile and does **not wait** for it: a start is a subprocess handshake and a first tool listing, each of which can take a whole `startup_timeout_seconds`, and with the new company already current every step after it would otherwise wait behind it. `mailboxes` is where a company that gained its first model provider lets through the mail every seat inbox held while it had none — after the seat tools, because the first thing a released inbox does is run a turn.

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
epoch, the revision that epoch named, the status and failure text, the epoch
the node last applied (`config_applied_epoch`), the revision it serves
(`config_serving_revision_id`) and the one it would boot on
(`config_boot_revision_id`), but *not* the stage list, so that detail lives on
the event rather than on the operator surfaces reading the bucket. A refusal
moves nothing in the node's own store but its record of the fleet's revision:
the node goes on serving the revision it ran before, and if it restarts comes
back on the revision its boot row names — that same one, except while a write
to the boot row is being retried, and none on a node whose own database is
discarded at every start — never on the revision it refused. The control plane records the outcome — the revision refused and the
one still served — so peers and operators can see it (see [Control
Plane](control-plane.md#a-nodes-own-store-keeps-three-facts-apart)).

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
> (`sandbox_runtime`) stays up too, and harmlessly: the previous epoch has no
> catalogue — which a company validates without only when it has no agent seat
> or no `providers.llm` entry — and a launch reads its turn's pinned epoch's
> catalogue, so it launches nothing, while the runs it already
> polls and the seats it prepared are the fleet's rather than the revision's.
> The reflection workers, the party index and the convergence are not among
> them: they are rebuilt after the last failure point, which is why they are
> ordered there. A node's **first** company is the one exception, on both
> sides of `tools`: `native` refuses it when the runtime cannot start,
> `learning` when the reflect dispatcher cannot be built, and `integrations`
> when the inbound edge cannot start. None of those refusals has a previous
> epoch to protect, so what it leaves behind (the shared MCP children, a
> built dispatcher, the party index, Worklet's, Doclet's and Chatlet's halves over logs the node
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

- **`providers.embeddings.dimensions`** — rows already written carry vectors of the old width, and a similarity query across two widths compares nothing. The apply fails naming the declared width and the width the store already holds. Moving a store from one width to another is a **restart**, because its vector columns are sized when it opens; after it, the knowledge corpus re-embeds itself, since its vectors are kept per model and width, and the node holding each seat re-fills that seat's diary and its raw episodes, which store the whole text their vectors are of. Similarity recall compares only vectors of the query's model and width, so until a row is filled it is not reached by similarity: personal memory reaches a note through its recent half, `## Similar prior work` leaves a turn out, and `query_episodes` reaches it with `conversation` or by recency — and says how many turns its search could not reach. A change of `providers.embeddings.model` at the same width *is* live, and for the same reason it needs no refusal: every stored vector names the model it came from, recall compares only rows of the query's model, and the diary and the raw episodes are re-filled in the new space the same way, never ranked across two spaces meanwhile. Adding or removing the whole `embeddings` block *is* live in both directions: a company that drops it degrades on the next turn to keyword-only search, personal memory chosen from each seat's recent notes alone, and no recall of similar prior work, since a seat's recent turns are not similar work.

**`providers.sandbox` is live in every direction.** Adding it brings the code
sandbox up on the next apply (`sandbox_runtime`), on a node that booted with no
company or with a company that had none; changing it swaps the manager
(`sandbox`) with the runs already in flight kept whole, a dropped cell's
running jobs included; and removing it — which validation admits only for a
company with no agent seat or no `providers.llm` entry, since every agent seat's
executor runs in one of its cells — stops new launches while the runs in
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
[Rotation](control-plane.md#rotation). A server the reconcile finds **down**
(its process died, or its start failed, and the engine is bringing it back) is
not restarted and not waited for: it is asked to try again at once, and its
tools are filed into the new epoch where they would be if it were up, refusing
until it is back — see
[A server that dies, or will not start](../guides/tools-and-mcp.md#a-server-that-dies-or-will-not-start).

A server the revision **removed** is stopped rather than merely dropped from
the catalogue, and that is a separate step because the reconcile above is
driven by the specs the *current* config names — so it never visits a name
that is gone. Without it the child kept running until the engine stopped,
holding the company's credentials, and a *rename* ran two of them. Retiring
happens before the reconcile, so a rename frees the name and the tools it
published before the replacement files its own. Taking a leaking integration
offline by deleting its `mcp_servers` entry therefore takes effect on the next
turn, which is what the rest of this section already promised.

**Per-role children are reconciled too, but by the seat rather than by the
epoch.** They belong to a seat's *lease*: spawned when the seat is claimed and
torn down when it is released, and only ever on the node holding it — so the
reconcile above, over the epoch's specs, skips every non-shared server, and
each node instead reconciles the children of the seats it holds, in the
`seat_tools` step of [the convergence](#what-follows-a-published-company), once
the new company is current. The spec compared there is the template with the
seat's `mcp_env` laid over it — which carries the per-agent Slack, GitHub or
Mattermost credential — resolved through the snapshot this apply re-read. So an
apply that stops declaring a server for a seat retires that child, one that
starts declaring one starts it, and an `mcp_env` edit, or a credential sealed
or rotated since the child started, restarts **only that seat's child**, on the
apply that carried it — in place, without releasing the seat — and every other
child keeps running. A child whose seat's own credential does not resolve is
not started at all, and one already running whose credential stopped resolving
is stopped: it waits, unstarted, for a credential a provisioning pass is
minting, and is reported failed for one nothing will mint or a pass cannot mint
until somebody acts (see
[Tools & MCP](../guides/tools-and-mcp.md#shared-vs-per-role-servers)). A seat
the revision removes altogether loses all its
children in the same step, while this node still holds it: placement releases
the seat afterwards, up to a sweep interval later, and a child left running
until then would go on acting at the vendor for an agent the company no longer
has.

The same step rebuilds each held seat's **registry**. The registry is a clone
of the epoch's surface and goes stale the moment a new epoch is published,
while a child is a running process holding that seat's credentials and must
not be restarted for a config edit that did not name it. So an apply re-files
each seat's live children into a fresh clone of the new epoch's builtins and
shared servers first, and only then reconciles the children themselves; a
child whose resolved spec did not move never learns the apply happened, so the
credential re-handshake a restart would cost is paid only by the servers that
actually changed.

Unlike the shared servers, which are reconciled *before* the swap and so hold
the apply until each has started, a seat's children are reconciled after it
and the apply does not wait for them. A turn that starts in between runs on
the seat's refiled registry, which still lists the children as the apply found
them, and a call to one being restarted is refused, classed `unavailable`, as
a server no longer running on this node; the seat refiles its registry again
once its reconcile is done, so its next turn sees the restarted children. A
child whose start failed is retried on its own as well, while the seat is
held, and one that dies after starting is restarted; an apply asks a child it
finds down to try again at once rather than reconciling it. A child held back
for its credential is not retried on a timer — nothing about it changes until
the secret snapshot does — and the pass that starts it is the one the next
change to the snapshot runs: an apply, or the pass a provisioning run asks for
on its own node as soon as it has sealed what it minted. No reconcile, retry
or restart starts a child for a seat this node can no longer prove it holds.
See
[Tools & MCP](../guides/tools-and-mcp.md#shared-vs-per-role-servers).

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
| `seat_tools` | Each held seat's registry, and its per-role MCP children | Both halves are described under [Shared MCP servers](#live-propagation) above. The registry is refiled before the step returns; the children are each seat's own reconcile, which the step hands over and does not wait for, because a start's handshake and first tool listing can each take a whole `startup_timeout_seconds` |
| `tracker_projects` | The projects the org chart names, as objects, stamped with the revision's activation instant | A project belongs to a **unit**, so the revision that first names one is usually the one that adds the unit. A create takes its key from the project's own counter, so a project that is not an object refuses every task filed into it |
| `knowledge_containers` | The containers each unit's and seat's `space:` names | Same reason, one subsystem along |
| `chat_unit_rooms` | On Chatlet, the room each unit's own `channel:` names, its members the unit's subtree and its lead, stamped with the revision's activation instant; a room whose unit stopped declaring it is released, staffed by nobody | A unit's seats talk in its room from their first turn, and only the chart writes a unit room's membership, so a revision that adds a unit or moves a seat is the only thing that can open or restaff one. A room is opened only for a unit that declares its channel, never one it inherits. No lease: a create arbitrates on the room's name, so two nodes running the step race at the broker and the loser is told the name is held, which is this step succeeding. A node in a capacity window opens none, because it publishes nothing |
| `mailboxes` | A durable subscription per seat; the release of every inbox held while the company had no model; a fresh judgement of every seat [parked on its token budget](agent-runtime.md#the-budget-park) against the ceilings now current; and the end of every [pause](agent-runtime.md#pausing-a-seat) whose seat the revision removed | Until a mailbox exists, every event published to that seat is **dropped** rather than retained. A convergence rather than a walk: it asks the broker what is missing and writes only that, so a company whose mailboxes all exist costs a comparison and no consumer proposals at all — and it runs unconditionally, because a mailbox lost under a company nobody edited is a fact about the broker that no comparison of two companies would notice. A pause is keyed by the seat's id, so it is cleared only when the seat itself is gone |
| `scheduler` | The cron loop, armed or disarmed | It reads its schedules off the current company, so it is armed after the pointer moves: armed earlier, it would fire the outgoing company's crons |
| `published` | The socket push that re-sends the roster, org tree, tool catalogue and schedules **whole** | The dashboard's company-derived screens come from the company, so no event will ever correct them and an overlay merge cannot express a seat going away |
| *the directory* — not a step of this list | The party index again, rebuilt **for the same company** when the [identity directory](identity-and-access.md#what-a-suspension-reaches-and-how-fast) moved | Not a configuration change at all, which is the point: suspending, reinstating, binding or removing the person who holds a seat is a record on the identity log, and a registry rebuilt only on a published company went on attributing a suspended person's Slack messages to their seat until somebody next activated a revision. See below |

**Every step notices for itself that there is nothing to do**, which is what
makes running the list on every apply cheap — re-activating an unchanged
revision is the credential-rotation gesture: the registry compares the company
it was built from, the identity lookups are token-keyed and cached, the mailbox
pass compares its own set against the broker's, the tracker, the containers and the unit rooms
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
live one. It is within one apply on every data node, with a thirty-second
re-read as the net — and on a node holding no data, which applies no log and
asks a data node for its directory reading, the re-read is the trigger, and it never appears in `applied_subsystems`,
because no apply ran.

**The order is not arbitrary.** A released inbox runs a turn immediately, and
that turn resolves parties, loads its tool surface and files work into a
project — so everything a turn reads is converged before the mailboxes are
ensured and any held mail is let through, except a seat's per-role children,
which `seat_tools` hands their reconcile and does not wait for (see above).
`published` is last, because it tells every open dashboard what this node now
serves.

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
    created_by         TEXT    NOT NULL,          -- the AUTHOR: the seat's handle for a person
                                                  -- or a credential bound to one, a login
                                                  -- for a credential bound to none —
                                                  -- token:<id> for a Tier A token — the
                                                  -- engine's own name for its own writes
    created_by_kind    TEXT    NOT NULL DEFAULT '', -- agent | human | operator | system
    operator_id        TEXT    NOT NULL DEFAULT '', -- the credential it was written
                                                  -- through: pat:<id>, session:<lineage>,
                                                  -- a Tier A token's login; empty for a
                                                  -- write no credential made
    source             TEXT    NOT NULL,          -- "api" | "file" | "rekey"
    summary            TEXT    NOT NULL,          -- short human-readable change note
    payload            TEXT    NOT NULL,          -- the whole document, sealed under
                                                  -- the keyring every node holds
    is_active          INTEGER NOT NULL DEFAULT 0, -- this node BOOTS on it
    activated_at       INTEGER,                    -- an activation instant: see below
    is_fleets          INTEGER NOT NULL DEFAULT 0, -- the fleet's revision as this
                                                   -- node last saw it
    offered            INTEGER NOT NULL DEFAULT 0, -- the boot row is an offline
                                                   -- offer the fleet has not taken
    offered_over       TEXT,                       -- the revision the offer was built
                                                   -- on; NULL for none
    offered_unconfirmed TEXT,                      -- the offer this one replaced, which
                                                   -- a start published and could not
                                                   -- confirm; NULL for none
    offered_at         INTEGER,                    -- the instant the boot publish
                                                   -- planned for the offer
    served_at          INTEGER                     -- the boot row's: the activation
                                                   -- this node applied
);

-- At most one row this node boots on, and at most one it records as the
-- fleet's, each enforced by the database rather than by the application
-- remembering to.
CREATE UNIQUE INDEX company_config_one_active_idx
    ON company_config (is_active) WHERE is_active <> 0;
CREATE UNIQUE INDEX company_config_one_fleets_idx
    ON company_config (is_fleets) WHERE is_fleets <> 0;
```

The types here are the four SQLite has (`TEXT`, `INTEGER`, `REAL`, `BLOB`)
rather than `UUID` / `TIMESTAMPTZ` / `JSONB`, and a timestamp is unix microseconds
rather than a date type — Turso is SQLite-compatible in both its query language
and its file format, so that is simply what a column can be. It was also, until
recently, the intersection of two drivers' dialects; the second driver is
retired and the types are unchanged, because they were never the narrow part.

A revert creates a *new* revision whose payload equals a prior one — the audit chain stays intact via `parent_revision_id`.

**Which revision is the company is not a column here.** It is the fleet's
activation pointer, in the coordination store, and every node answers it
([Live Propagation](#live-propagation)). What the table marks are three facts
only this node can know:

- **`is_active` is the revision this node boots on.** Online, only the
  reconciler moves it, and only to the revision its engine has just applied,
  so a node never starts on a revision it refused, and an engine's reconcile
  ticks move it in the order they read the pointer. Offline, a `crewlet
  config` command chooses it ([below](#offline-commands-make-an-offer)).
- **`is_fleets` is the fleet's revision as this node last saw it**, recorded by
  the reconciler for every pointer revision whose seal opens under the node's
  keyring, before validating it — refused here or not.
- **`offered`, `offered_over`, `offered_unconfirmed` and `offered_at` mark an
  offer**: a boot row an offline command chose that the fleet has not taken,
  the revision it was built on, the earlier offer it replaced where a start
  published that one and could not confirm it, and the instant the boot
  publish planned for it.

With no pointer — one lost under a running fleet — the company is the one this
node knows: an offer built on the fleet's revision it last saw, or built while
it had seen none; otherwise that fleet revision, even one this build refused,
because every node that could run it does and the older revision this node
runs would roll them back; otherwise the revision it boots on. That is what
`GET /config` answers while the pointer is gone, what the next boot restores,
and what an offline command builds on.

Two instants stay apart. **`activated_at`**, on every row the boot row's
included, is the latest instant this node knows the fleet activated that
revision at, raised and never lowered: what a node restoring a lost pointer
asks for, and what the sweep below dates a re-activated revision by.
**`served_at`** is the boot row's alone: the instant of the activation this
node applied, what the engine stamps the org chart and the knowledge spaces
with when it boots on the revision, and it moves only when the node applies an
activation. An offer has none, and starts the engine at no instant, because the
fleet has not taken it yet. They were one column, and a re-activation of the
running revision that this node refused — the [rotation](#rotation) gesture on
a node that lost the credential — could raise neither: the boot row's instant
had to stay the one it applied, so the fleet's newer activation was lost.

These used to be one flag, `is_active`, and each reader took it for the fact it
needed: a node marked the fleet's revision active as it fetched it, before
validating it, and was refused at its next start; a write marked its revision
active as the pointer moved, and a reconciler a moment behind walked it back;
and an offline command marked its revision active "now", which the next start
published over whatever the fleet had moved on to. See [Control Plane § A
node's own store keeps three facts
apart](control-plane.md#a-nodes-own-store-keeps-three-facts-apart).

### Offline commands make an offer

`crewlet config import`, `activate` and `rekey` run while the engine is stopped
do not open the coordination store — on an embedded broker it lives inside the
running engine — so they cannot move the pointer. What each writes is an
**offer**: the revision this node boots on at its next start, built on the
company this node knows (above), which each command names. "Stopped" means
not starting either: a start holds the store from the read it decides on
(step 6 of the [Bootstrap Sequence](#bootstrap-sequence)), so the offer it
publishes is the one it read, and a command run while it starts is refused
or sent to its API as it would be beside the running engine.

- `import` writes the file as a new revision built on that company, with a
  redacted file's masks restored from it. A file equal to it imports nothing.
- `activate <id>` makes a stored revision the offer, holding it to the
  runnable rules first, because the next start boots on it: a revision this
  build cannot run is refused with nothing written. Activating the fleet's own
  revision is the offline [rotation](#rotation) gesture: it is published over
  itself, as a new epoch.
- `rekey` re-seals the revision this node boots on — the one its next start
  must open, which on a node that refused the fleet's revision is not the
  fleet's — and offers the re-sealed copy over that revision, or, where the
  revision it re-seals is itself an offer, over the revision that offer was
  built on. On a node that refused the fleet's revision the copy is therefore
  withheld, as below, and the fleet's own document is re-sealed through a
  running node with `POST /config/reload`.

The next start publishes the offer by a compare-and-set against its base: only
while the pointer still names the revision it was built on — expecting the very
activation of it the start read, so a publish planned at one instant lands at
exactly that instant or not at all — at an instant later than the pointer's, so
it always mints a new epoch. Where the company this node knew was itself an
offer an earlier start published and could not confirm, because it crashed
between the publish and the record of it, whether the fleet took that one is
not known offline: the new offer is built on both it and the revision under
it, and is published over whichever the pointer names. Where the pointer is gone,
it is published create-only if its base is the fleet's revision this node last
saw, or if this node saw none; built on no company, it is published only while
the fleet has none. **An offer the fleet moved on from is
withheld, never published over the fleet.** The node boots on it, says
`offline_revision_not_published` at warn on every boot — naming the offer, what
it was built on and what the fleet runs — and runs it until it applies the
fleet's revision, which then becomes what it boots on, with the offline change
left in its history. Making such a change the company as it stands would undo
what the fleet changed since, so the warning's hint names the change instead:
`GET /config/revisions/<revision>/diff?against=<built_on>` on that node shows
what it did, to make the same change on the fleet's document (through the
dashboard, or `crewlet config import -api`), and
`POST /config/revisions/<revision>/revert` on that node puts it up as it
stands, for an operator who means that.
`crewlet run -import-company` is the one way to replace the company
unconditionally. The whole decision, restoring a lost pointer included, is
[What a booting node publishes](control-plane.md#what-a-booting-node-publishes).

`crewlet config revisions` says which row is which in its STATE column:
`boots`, `fleet's`, and `offered over <id>`, or `offered over <id> or the
unconfirmed <offer>` (see [the CLI
reference](../reference/cli.md#crewlet-config-revisions)).

### A revision is immutable as a configuration, not as an archive of people

One thing narrows "append-only", and it is deliberate.

**It is swept.** Every node keeps its own copy of every revision it has ever
met, and nothing deleted from it — one row per config write, per node, each
holding the whole document. The ordinary maintenance tick now keeps every
revision created or activated inside **400 days** — a row is dated by the later
of the two, so an old revision the fleet re-activated yesterday stays — plus,
whatever their age, **the revision this node boots on, the fleet's revision as
it last saw it, and the whole parent chain behind each**, and it runs on every
node rather than under the fleet singleton, because each node owns its own
copy. The two marked rows are often different revisions: a node that refused
the fleet's revision boots on an older one, and needs both — one to start on,
the other to restore a lost pointer from. The chain is kept by id rather than
by date: a revert re-activates an older revision and `crewlet config diff`
walks it, so a swept ancestor turns both into an error naming a row that used
to exist. See [Retention](../guides/retention.md#the-configuration-archive-and-the-one-thing-a-purge-cannot-reach)
for where 400 days comes from and what its floor is.

**A revision is an archive of the org chart it ran.** A human seat's `email`
and `contact` account ids are inside every revision that carried them — on
every node, in every backup — and removing the seat writes a *new* revision
rather than reaching the old ones. They leave the store when the sweep takes
their revision, and a backup when the backup is deleted.

### What a stored revision is held to

A stored revision is not a document somebody just submitted. It passed the validation of the build that wrote it, which is not necessarily the build reading it: during a rolling upgrade a newer peer can store a document this build's rules would refuse. So reading a revision and running one are held to different standards.

- **Reading holds a revision to no rule.** `GET /config`, a revision read, the diff, the reference index, the entity reads, `crewlet config show`, `export` and `diff`, and the prior a write restores its masks from or merges onto all decode the stored document as it is. A revision this build would refuse is exactly the one an operator needs to see and replace, so none of these may refuse it.
- **Applying holds a revision to the runnable rules.** A node's reconcile tick validates a revision before anything on the node changes, so a refused revision leaves the previous epoch serving untouched — and leaves it what the node boots on: the tick records the refused revision only as the fleet's, so the node's next start runs what it ran before rather than being refused before its engine starts. Booting from the store validates the revision the node boots on and names it when it cannot run. Online that cannot happen, because a node boots only on a revision its engine applied or one an offline command of the same build held to these rules; a boot row this build refuses was written by another build or another store — a binary downgraded after the node applied it, an offline command run with a newer build's CLI, or a store restored, copied or edited by hand — so the refusal names the build that wrote it first. Its other way out has to reach this node's own store: the boot reads nothing else, and a correction written through another node fixes the fleet but reaches this store only through a reconciler a refused boot never starts. What writes the store directly is an offline `crewlet config import` of a corrected document, which the next start boots on and [publishes](#offline-commands-make-an-offer) only while the fleet still runs the revision it was built on (or, built on no company, only if the fleet has none), so it never replaces a revision the fleet has moved on to; where the fleet has, the import is this node's alone until it applies the fleet's revision. The refusal does not name `-import-company`, which would replace whatever the fleet runs — a newer company included — to start one node. On a [managed](#managed-configuration) deployment the offline import is refused, so the refusal names the way that reaches the store: take the document back on this node for one start — remove `api.auth.company_writers` from its Tier A, import the managing system's current document with `crewlet config import`, and start the node, which publishes the import only while the fleet still runs the revision it was built on and so never replaces a newer one — then restore `api.auth.company_writers` and restart. A company file named with `-company` or `-import-company` is held to the runnable rules as it is read, and to the rest only where it is written, because most boots write nothing from it: it is already the revision the node boots on (for `-import-company`, the one the fleet runs), or a bootstrap a company that exists outranks — one this node boots on or has seen the fleet run, or the fleet's. `POST /config/reload` and a revert validate what they re-activate.
- **A written document is held to every rule.** `PUT`, `PATCH`, a per-entity write, a `/setup` submission that changes the document, `crewlet config import`, `crewlet validate` and a company file `crewlet run` imports as a new revision (`-company` where no company exists yet, `-import-company` over a different company) validate the entire document the write produces, after its masks are restored. A write over a revision this build refuses therefore succeeds exactly when it corrects it. The company file `crewlet run` imports is also **built** as the node's apply would build it before anything is stored, because nothing else builds it before the fleet is pointed at it (see [Bootstrap Sequence](#bootstrap-sequence) step 12).

The difference between the last two is the **admission rules**: authoring hygiene that nothing about running a company depends on, and the part of the rule set a later build may add to or relax. A revision that breaks one still runs, so an apply does not refuse it: during a rolling upgrade a newer peer may activate a revision it admitted under rules this build does not share, and refusing it would split the fleet's epoch. Today they are [unique unit keys](organization-model.md#handles-and-keys-are-unique-names-are-not) — a display name, a seat's or a unit's, is prose and never held unique — and unique sandbox setup step names within one `setup` list (`providers.sandbox.setup`, or one seat's `sandbox.setup`): a step's `env` and `files` are credentials restored by the step's name, so two steps of one name would leave every write carrying that list refused on masks nobody edited. So is a `unit:` reference on a seat declared inside a unit it does not name: the reference places only a root seat, so there it moves nothing and reads as a placement (see [the organization model](organization-model.md#a-seats-unit-reference)). A seat's own GitHub App (`integrations.github`) on a [human seat](humans-in-the-org.md) is one too: an app is the identity an agent acts as on GitHub, a person acts as their own `contact.github_login`, and nothing creates or reconciles an app for a person, so the block would read as a setting and do nothing. So is a schedule's `cron` that has five fields and still breaks the [cron grammar](scheduling.md#cron-syntax) (`61 * * * *`): a company carrying one runs with that schedule never firing and every other schedule untouched. So is a seat's `avatar` naming a character or a colour this build does not draw, or written on a human seat: nothing that runs a seat reads it, and the set of characters will grow, so a revision a newer peer admitted with a character this build does not know still applies here, and the dashboard draws that seat as the original Crewlet until it is upgraded. So is a sandbox setup step writing a file a box some seat runs in would refuse — `/usr/local/bin/…` for a seat that runs `direct`, say, or the home itself in any cell: the company runs, and only the boxes that step reaches fail at setup (see [Code Sandbox](code-sandbox.md#setup-steps--provisioning-the-box)). A `providers.llm` entry's credential configured where its CLI never reads it, or handed over under a variable name no store can hold (an `overrides.token_env` or `overrides.api_key_env` outside the reference grammar), is the same kind of rule, and so is an `opencode` entry's `model` with no provider segment (see [LLM Backends](llm-backends.md#configuration-reference)). So are two units that declare one `channel` on [Chatlet](chatlet.md#what-a-company-gets): a unit's room there belongs to one unit at a time, so the second unit would be left out of a room its seats are told is theirs — the engine opens it for the first and reports the other at every apply. So is a coding run's `optional_keys` entry — on a seat's `sandbox` or on a setup step — that leaves nothing out: a key or path its block does not write, a variable's name where its key belongs, or a key whose values reference nothing, each leaving the key it was meant for expected, so the run's start logs it as a gap (`sandbox_env_unresolved`) wherever its value resolves to nothing (see [Code Sandbox](code-sandbox.md#credentials-and-the-run-environment)). Five more are **whole-document** rules: each compares one part of the document against another, or one seat against every other seat.

- **An `mcp_env` key must name a tool server.** The engine looks a seat's credentials up per *server*, so a block keyed on a name `mcp_servers` does not declare is never read by anything: it sits in the document looking configured while the server it was meant for starts with none. Six names need no server, because the engine reads those blocks itself — `github`, `gitlab`, `datadog`, and Atlassian's `atlassian`, `jira` and `confluence`.
- **A tool server's `optional_keys` entry must leave something out.** It names a key of the server's own `env` or `headers`, or of a seat's or unit's `mcp_env` block for it, whose value carries a `${VAR}` reference; a variable's name where its key belongs, a variable only the `url` references, a key nothing writes and a key whose values reference nothing are refused, because each leaves the key it was meant for required and the server not started wherever its value resolves to nothing (see [Tools & MCP](../guides/tools-and-mcp.md#a-key-that-may-go-without-its-value)).
- **An http header is spelled one way where nothing says which is sent.** HTTP does not tell `authorization` from `Authorization`, so two spellings of one header in one map, or in a unit's `mcp_env` block and one of its members', would send whichever came last out of the map. A seat's spelling over its template's is not refused: the seat's replaces the template's however either is written.
- **Two seats may not declare one email.** The party registry keys on the address and the first seat wins, so all but one silently stop being findable and mail meant for one person resolves to another. The same rule already covered every [external account id](humans-in-the-org.md).
- **`lead:`, `unit:` and `manages:` must be shaped like what they name** — a seat's handle, a unit's key, or (for `manages:`) either. Those fields took a seat's display name once and do not any more, so `lead: QA Lead` can never resolve under any chart. Only the *shape* is refused: a well-formed reference that resolves to nothing stays a warning, because an organization is assembled in pieces and a lead naming a seat nobody has hired yet must still apply.

A seat may not be called `none` either — what `integrations.datadog.route_to` means by nobody, so a seat of that name would be silenced by its own handle. That one is a runnable rule.

A written document is refused for breaking one. A revision being applied that breaks one is applied, booted on, reloaded and reverted to like any other, and each node logs `org_admission_warning` once per violation when it applies the epoch. Every other rule is a **runnable** rule, and nothing applies a revision that breaks one.

The [charter's rules](#mission-vision-and-policies) are judged on an edit's **change** rather than on the document it leaves: an API write is refused only for a fault in a part of the charter it changes, judged against the stored revision it replaces, and an import (offline, at boot, to a running node, a restore) is never refused for one. Every fault an edit leaves untouched, and every fault an import carries, is warned about like an admission rule, on the write's answer, in the import's output and as `org_admission_warning` on every apply.

### What an applied company is worth knowing

A revision can also be valid and carry a consequence: a company with no human
seat, which can admit nobody; a human seat with no account on the Mattermost or
Slack workspace the company talks on, which nobody can mention there; a token
ceiling another ceiling leaves unable to refuse a turn; a `-e NAME` in
`providers.sandbox.local.run_args` that nothing supplies; an agent seat whose `llm` chain mixes a `claude-code` or `opencode`
entry with another CLI, which a launch skips (it still serves the reviewer,
workers and side passes). `crewlet validate`, a
`/config` write and a dry run answer these to whoever asked, as `advisory`
warnings. **Every node also logs them, once for each epoch it applies**, one
`config_advisory` line at `WARN` each, and never for a revision it refused:

| Field | Meaning |
|---|---|
| `config` | `company`: which tier. A node's start logs its Tier A's advisories under the same event with `config=bootstrap` (see [`crewlet run`](../reference/cli.md#every-start-says-what-its-tier-a-is-worth-knowing)), and so does an apply for the one advisory only the two tiers together raise: on a Linux node with both the `ingress` and the `seats` role, a company that reaches the `container` cell while the listener a box dials binds loopback alone, which no container box on that host can reach ([Local sandboxes](code-sandbox.md#local-sandboxes)) |
| `revision`, `epoch` | The revision and the activation the node applied |
| `path` | Where in the document the remedy is written |
| `seat`, `unit` | The seat's handle or the unit's name the advisory is about — present only when it is about one |
| `detail` | The whole message: what will happen, then what to do instead |

The log is the only place some companies are told. A `/config` answer goes to
the person or the system that wrote, but a company a node imports from
`-company` or `-import-company` is answered to nobody, so before this line it
ran silent unless somebody ran `crewlet validate` on the file. The two other
kinds of warning keep their own lines at apply — `org_dangling_reference` for a
[reference that resolves to nothing](organization-model.md#dangling-references)
and `org_admission_warning` above — so no fact is logged twice.

---

## Auth

**Every route requires a credential, reads included.** The only exceptions are
the handful that authenticate by other means or must be reachable to obtain a
credential at all, listed below. Tokens are listed in Tier A under
`api.auth.tokens` and resolved from environment variables at API startup. Each
revision a request produces records its **author** as `created_by` — a Tier A
token's whole login (`token:ci-pipeline`) — or the seat it is bound to — or
the seat a signed-in person holds — with its kind as
`created_by_kind` and the **credential** it came through as `operator_id`: a
machine token's `pat:<credential id>`, a browser session's `session:<lineage>`.
So revision history carries meaningful attribution rather than generic strings,
and a revision somebody's token wrote is theirs and is never mistaken for one
they wrote themselves. Every node's copy says the same: the three travel on the
activation pointer — as `author`, `author_kind` and `operator_id`, beside the
revision's `source` and `created_at` — so a node keeping the revision from its fleet records the
writer rather than `peer`. A revision the engine wrote itself — the boot seed,
the reconcile loop — carries the kind `system` and no credential, which no
token's name could say. See [the API
reference](../reference/api-endpoints.md#the-config_audit-query).

### `admin` is host access

The permission that writes this document is **one** permission — `admin`,
which also runs the company's credentials, people and deployment — and it is
conferred the way shell on the engine's hosts is, because that is what it is. The
configuration runs code: an `mcp_servers` entry is a command every engine host
executes, a seat's `mcp_env` and its per-phase model keys (`llm_*`, each
naming a coding CLI the engine host runs) decide what the seat's children run and which
credentials they are handed, every agent seat's executor is its coding CLI's
own fully permissioned run, which a `providers.llm` entry's `run_in: direct` (or
`providers.sandbox.default_run_in: direct`) runs on the engine host itself as
the engine's user, and a seat's worker grants decide which of its
tools a worker holds. So whoever holds `admin` can run anything on every
engine host and read whatever a process there can — the keyring included.
There is deliberately no separate "runtime" permission: it would be this one's
whole practical reach under a second name. What a node can do is refuse admin
altogether — `api.auth.admin: refuse` clamps it away on that node and keeps
every app — which is how a node published to the people who use the company is
kept from being one anybody runs it through.

What that means in practice:

- Grant it to the people and pipelines you would give a shell on those hosts,
  and no one else. A machine token **can** carry it — applying a configuration
  from a deploy job is what one is for — so minting one with it is handing that
  job host access. What it cannot do through a token is what needs a person
  present: reveal a secret, change who may do anything, or end everybody's
  sessions ([two locks](identity-and-access.md#some-gestures-ask-how-recently-you-proved-who-you-are)).
- Everything a company does *with* its apps — filing work, writing pages,
  making artifacts, talking in its own chat — needs no admin: that is
  `worklet`, `doclet`, `artifacts` and `chatlet`, which is what most people
  hold.

The org chart inside this document is also written by its **leads**, who need
no permission for it: a person who leads a unit may change the seats and units
inside it, judged change by change on both sides of the write, and never a
setting, a credential, a `${VAR}` in any field, or a project, space, channel,
address or contact id ([A lead edits their own
team](identity-and-access.md#a-lead-edits-their-own-team)). What that reaches
is bounded by what the settings offer, which stay admin's: a lead may
point a seat in their team at any model chain the company declares, and so at
the cell each of those entries' `run_in` (or the default) names — so every
`providers.llm` entry, each a CLI on the engine host, and every cell an entry
places its runs in on the engine host is one a lead can hand to their own
seats. Configure those only where every lead may use them.

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

A script that only reads what the company is doing now holds **a service
account's token with no permission at all** — it reads the overview and
nothing else — listable, revocable without a restart, and present in the audit
log, none of which a posture flag could be. A file that still
carries the key is refused — Tier A is decoded strictly, so a key it does not
know is an error rather than something ignored, which is what keeps it from
silently running the opposite posture from the one the file asks for — and the
refusal names it as removed and says to give a read-only client an
`api.auth.tokens` entry with only the apps it reads, rather than asking for a
spelling no spelling of it passes.

`api.auth.disabled` went with it. It authenticated the **empty** credential
into full operator authority with no check on the bind address anywhere, which
made one unset environment variable a total bypass. Local development uses
[`crewlet run -dev-principal <login>`](../reference/cli.md#-dev-principal-and-why-it-is-a-flag)
instead, which has the two guards that field never had: `api.host` must bind a
loopback address — the *bind*, not `api.external_url`, because the bind is what
physically stops another machine reaching this — and the binary must be a
development build, so no copied setting can carry it into a deployment that
pulled a tag. That is also why it is a flag: a field is the thing that gets
copied into an image. It grants every app, and `admin` only where
`api.auth.admin` is `serve`, where `disabled` granted everything regardless.

`api.auth.oidc` is retired too: the engine no longer signs anybody in through
an identity provider. A person signs in with a login, a password and — unless
`api.auth.totp` says `optional` — a second factor the engine holds. A file that
still carries the block is refused, and the refusal says it was removed and
that people sign in with a password and two-step verification instead.

There is no switch for password sign-in, either: it is always served. A
deployment nobody has been invited to already signs nobody in, because nobody
holds a password to present, so a setting that turned the routes off would
change nothing a stranger could reach. What a deployment does state is what a
password sign-in asks for — `api.auth.totp` (unset is `required`, the secure
value, so `optional` is always somebody's explicit choice),
`api.auth.accept_insecure` and `api.auth.min_password_length`.

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
| `/auth/config`, `/auth/login`, `/auth/invite/*`, `/auth/reset/*` | A login cannot require a login: these are how somebody obtains a credential, and an invitation's link and a password reset link are each the credential. Exact paths plus the two prefixes, never `/auth/` — the same surface ends sessions and enrols second factors. The sign-in throttle and the origin check stand in for the guard |
| `/auth/logout` | Signing out of this session clears the cookie whatever the node can read, and verifies every bearer it ends itself. Signing out everywhere and ending a named session stay guarded |

**The public listener serves only what an outside party calls.** With
`api.public.port` set, `/webhooks/*`, `/otlp/*` and `/mcp/*` are served on it
and nowhere else, behind the same guard, and every other route — every `/auth/`
route included — only on `api.port`, so the address a vendor is given never
reaches a sign-in form. A node without `ingress` serves its probes on
`api.port`, behind the guard as well: `/health` answers, `/dashboard` is `404`
and every guarded route is `401` (see [probes on a node without
ingress](../reference/api-endpoints.md#probes-on-a-node-without-ingress)).

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

**`api.auth.company_writers`** names the Tier A tokens that alone may *change* the company document; unset, whoever the authority table admits may. See [Managed configuration](#managed-configuration).

### What Tier A must state on every node

**`secrets.keys` is required on every node**, whatever its roles and whether or
not it serves the API, and `crewlet validate` refuses a Tier A file without it.
It has no default — a key the engine invented would be one no other node holds
— and far more than the company's credentials rest on it:

- every record on every state log is signed and verified under it, because the
  broker authenticates nothing and every data node applies every log — and
  every request a node without `data` sends through the estate router, and
  every answer it gets, is signed under it too;
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

Three more settings stop being optional the moment a node serves the API —
`api.port` non-zero on a node running `ingress`; a port on a node without it
serves only probes and needs none of them — and each is refused by `crewlet
validate` on a laptop rather than by a process at bind time:

| Setting | Why it cannot be defaulted |
|---------|----------------------------|
| `api.external_url` | The session cookie's `Secure` flag and `__Host-` prefix follow its scheme, its host is the origin every write is checked against, and it is every link a person follows — an invitation, a password link, a page's address in a notification. Without a public listener it is also what every webhook URL is built on. The engine sits behind a TLS-terminating proxy and can read none of that off the request |
| `api.auth.admin` | Whether this node grants `admin` to those who hold it (`serve`) or clamps it away and keeps every app (`refuse`). Admin is host access, so whether a node publishes it is the operator's decision, never a default |
| `api.auth.tokens` | A fresh deployment's identity estate is empty, so a Tier A token is what creates the company and then puts its first person on their seat — and on a running one it is the way back in when an administrator has locked themselves out |

With `api.public.port` set on an `ingress` node, **`api.public.external_url`** is required as well:
the address vendors and sandboxes reach the public listener at, which every
webhook URL is built on. A box's per-run OTLP and tool-bridge endpoints are
built on `CREWLET_SANDBOX_OTEL_RECEIVER_URL` and `CREWLET_MCP_BRIDGE_URL`,
which name that same address — but a `direct` box's, which dial its own node
on loopback (see
[Deployment](../guides/deployment.md#exposing-webhooks-without-the-admin-api)). The two
are separate settings because they are separate sockets — a person reaches
`api.port` and a vendor the public one — and neither is ever read off a request.
Without a public listener the two are the same address and only
`api.external_url` is stated.

### The API postures Tier A warns about

Three API postures are **valid** and worth reading before a deployment runs on
them. None can be a refusal, because each is a configuration that works exactly
as written with its consequence somewhere else. `crewlet validate` prints each,
and every start of a node serving the API logs each as a `config_advisory`
line at `WARN`:

| Warning | Why it is not a refusal |
|---------|-------------------------|
| `api.auth.accept_insecure` is set | It is what makes an otherwise-refused posture legal — `totp: optional` on an address a browser reaches over the network. The acknowledgement is a decision made once that everybody after inherits, so `crewlet validate` says it every time and every start logs it |
| `api.external_url` is `http://` off loopback | The session cookie cannot carry `Secure` and no `__Host-` prefix protects it, so every credential travels in the clear — but a tunnel, a staging box and an internal network genuinely look like this. The one posture it *would* be a refusal for, an optional second factor, already is one |
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

An entry is compared against the browser's `Origin` header, which is always
`scheme://host[:port]`, in the spelling a browser gives it: the scheme and host
in lower case, an IP address in its canonical form and no port where it is the
scheme's default, so `https://Ops.Example.com:443` permits the
`https://ops.example.com` a browser sends. An entry is refused when no
browser could send it — no scheme, no host, a port outside 1–65535 — because
an allow-list that looks configured and never matches fails in a browser
console this engine never sees; and when it carries more than an origin — a
path, a trailing slash, a query, a fragment or a user — because all of that is
dropped before the comparison, so `https://ops.example.com/dashboard` would
admit every page of the host. Three hosts a browser rewrites before it sends
them are refused too, naming the form to write instead: one outside ASCII,
which a browser sends in its punycode form (`xn--bcher-kva.example`); an IPv4
address written other than as four decimal octets (`127.1`, `0x7f.0.0.1`, a
leading zero, which a browser reads as octal); and an IPv6 zone, which no
browser can address. `api.external_url` is held to the same three. A permitted
origin's response carries `Access-Control-Allow-Origin`,
`Access-Control-Expose-Headers` and `Vary: Origin`; an unlisted one carries
only the `Vary` and the browser blocks it — it is not refused with a status,
because a same-origin `POST` carries an `Origin` too and the default allow-list
is empty.

Preflights are answered **before** the auth guard, and deliberately: a browser
sends the `OPTIONS` itself with no `Authorization` header, so answering it
behind the guard would be a `401` on every cross-origin write and the real
request would never be sent. The request headers a permitted origin may send
are a closed set — exactly the ones the routes read: `Authorization`,
`Content-Type`, `Idempotency-Key`, `If-Match`, `If-None-Match`, `X-Summary` and
the two link secrets — never an echo of what the browser asked for, and an
answer to a permitted origin exposes exactly the headers a client acts on:
`ETag`, `Retry-After`, `Accept-Patch`, `Allow` and `Content-Disposition`.
Leaving one out does not strip it; the browser refuses the whole request, or
hides the header from the page, so a route reading a header a permitted origin
cannot send is a route that origin can never use. The Dashboard Assistant's
`Last-Event-ID` is not offered, since the one route that reads it, the
assistant's stream, answers nothing but a person's own browser session, and
no answer here allows another origin credentials. The dashboard's
`X-Crewlet-Reader` is not offered, since only the dashboard sends it and it is
same-origin. The methods are likewise exactly the ones the routes serve —
`OPTIONS` among them, for `OPTIONS /config`. The
[API reference](../reference/api-endpoints.md#routes) says which route reads
each one. A preflight is cacheable for ten minutes —
short enough that removing an origin takes effect within one.

The auth middleware compares tokens in constant time (`crypto/subtle`).
A credential presented and refused logs `api_auth_failed` at WARNING (never
the candidate token value), while a request that presented none — a signed-out
tab, an invitation's page — logs `api_auth_anonymous` at DEBUG; successes log `api_auth_ok` at DEBUG with `actor` (who a write would
be attributed to), `operator_id` (the credential it came through) and `route`.
Every write this surface logs — `config_revision_written`, `secret_written`,
`backup_taken`, `retention_gate` and the rest — carries the same pair as `by`
and `operator`.

See the [API endpoints reference](../reference/api-endpoints.md#config--live-config-management-auth-gated) for the per-route auth + status semantics.

### Managed configuration

Some deployments do not edit the company document by hand at all: a GitOps
pipeline applies it from a repository, or external tooling such as a
Kubernetes operator renders it from custom resources and writes it on every
reconcile. Such a system replaces the fleet's revision with its own each time
it runs, so an edit a person makes through the dashboard or the API lasts only
until then — and nothing tells them. Naming the system's token as the
document's only writer turns that silent loss into a refusal at the moment of
the edit:

```yaml
api:
  auth:
    tokens:
      - {id: gitops, token: "${CREWLET_API_TOKEN_GITOPS}"}
      - {id: founder, token: "${CREWLET_API_TOKEN_FOUNDER}"}
    company_writers: [gitops]   # only this token may change the company document
```

With `company_writers` set, every write that **changes** the document refuses
any other credential with `403 config_managed`, naming the writers and what to
do instead. **The credential is what is judged, never the author**: a listed
token writes because the request came through it (`operator_id` is its login,
`token:<id>`), so a person's session, a machine token acting as its owner, a
session exchanged from a listed Tier A token and a lead writing their own team
are each refused — every one of them is an edit the managing system's next
reconcile would overwrite. It is judged after the route's own grant and before
the body is read, so a refusal costs no parse. Reads are unchanged: every
administrator still reads `/config`, its history and its diffs.

| Refused for a credential not listed | Still open to whoever the authority table admits |
|---|---|
| `PUT` and `PATCH /config`, every per-entity `PUT /config/{kind}/{id}`, `POST /config/revisions/{id}/revert`, and the `?dry_run=true` check of each — a check answers what the write would, and the write would be refused | `POST /config/reload`, which re-publishes the fleet's document's own bytes |
| A `/setup` connect that writes a pointer, a disconnect, and an agent's GitHub App — each refused **before** anything is sealed, queued or created at the vendor | A `/setup` submission that only rotates a value the document already names, and the provisioning pass and its check |
| An offline `crewlet config import`, an offline `crewlet config activate` of any revision but the company's own, and `crewlet run -import-company`; a `-company` bootstrap seed is not read at all — neither imported nor run, so a stray `./company.yaml` neither becomes the company nor fails the boot — and a `company_seed_ignored` warning names it | `/secrets`, `crewlet secrets` and `crewlet config rekey`, and `crewlet config activate` of the company's own revision (the one `crewlet config show` prints), the offline re-publish a rotated credential needs |

**Why the secret store and a rotation stay open.** A leaked credential has to
be replaced now, by whoever holds the new one, and the managing system cannot
know it is needed. Writing the new value and re-publishing the unchanged
document — `PUT /secrets/{name}` then `POST /config/reload`, or one rotation
through Company › Integrations, changes nothing the managing system renders,
so it is the break-glass a person keeps. If the managing system also syncs the
secret store, it will overwrite a rotated name it owns at its next sync; fix
the value at its source as well.

**The engine's own writes are not judged by the list.** A reconcile pass that
records what it discovered at a vendor writes as the engine itself (kind
`system`), not as a credential; a managing system that re-renders the document should carry those
values (they are in the revision `GET /config` answers) or set them itself.
That includes a provisioning pass any operator starts on demand from `/setup`
or Company › Integrations: where `integrations.jira` or
`integrations.confluence` has no `cloud_id`, the Atlassian pass records the
`cloud_id` and `site_url` of the site it found, and the revision is credited to
the engine (`reconcile loop`, kind `system`), not to the person who pressed
the button.

**It prevents drift; it is not a privilege boundary.** `company_writers`
stops a person's edit from being silently overwritten, and nothing more. Every
administrator still writes the secret store and can reload, so a credential
that is not a writer can
still change what the managed document *does* by rewriting a value it
references — a model's API key, a webhook secret, or any URL or endpoint kept
as a `${VAR}` — and publishing it with `POST /config/reload`. A deployment that
needs to keep somebody away from the company's behaviour must not grant them
`admin`.

**The dashboard shows the document as managed.** The [`viewer`](../reference/api-endpoints.md#queries)
answer says whether the caller's credential may change the document
(`config_writer`) and who manages it (`config_managed_by`); every control that
writes the document — a lead's edit of their own team included — is drawn
disabled with that sentence, and the org builder opens read-only.

`crewlet validate` warns about a list that is valid and almost certainly wrong,
and so does every start of the node, as `config_advisory`:
an id that names no token in `api.auth.tokens` (it lets nobody write), and a
list naming no configured token at all (nothing may change the document
through the API — a frozen document). An entry outside the token-id grammar
every `api.auth.tokens[].id` is held to, or a repeated one, is refused
outright. The list is per
node, like the rest of `api.auth`: give every node the same one.

To take the document back, remove `company_writers` from every node's Tier A
and restart. The decision and its boundaries are
[ADR-0030](https://github.com/crewlet/crewlet/blob/main/adr/0030-a-managed-company-document-names-its-writers.md).

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
- **Authenticated, not only hidden.** The document a node fetches from its peers comes through the coordination store, which anything reaching the broker can write, so the seal is also what proves a node of this fleet wrote it. A revision stored **unsealed** is refused — never applied, kept, shown or reverted to — with an error naming the way to have its document back: importing it again from your own copy (`crewlet config import` or `PUT /config`), which stores it sealed. See [Control Plane § The design](control-plane.md#the-design).
- **One key, not N env vars.** After encrypting, the engine needs only the Tier A key in its environment — not a per-secret env var for every LLM key, MCP token, and webhook secret.

Because the key gates every read, keep it as available as the database itself: the API, dashboard, migrations, and CLI all fail closed without it.

**Threat model.** Encryption at rest defends against *data-at-rest* exposure: a leaked backup, a copied store file, a stolen volume snapshot, a coordination-store dump on a laptop — the attacker gets one opaque ciphertext blob per revision, no structure and no credentials. It also keeps config egress clean (`GET /config`, dashboard views, and revision diffs are decrypt-then-redact — no plaintext secrets) and absorbs accidental plaintext (a raw key pasted into `company.yaml` is encrypted on write, so it never lands in the DB in the clear). It does **not** defend against a compromised engine host that holds both the DB and the Tier A key (that host can decrypt — it must, to run; keep the key out of the store's backup domain — that separation is the point), a malicious operator with a valid key and API access, or in-memory extraction from the live process. The property: plaintext config exists only transiently in the encrypt/decrypt path and in the live engine's memory — never in durable storage.

### Rotation

**Rotating one secret** (e.g. a leaked LLM key) needs no host access: `PUT /config` (or a per-entity write) with the new value — the whole document is re-encrypted under the active key as a new revision.

**Rotating the master key** uses the keyring's multi-key support so there's no downtime. [The rotation runbook](secret-store.md#rotating-the-keyring-with-nothing-in-flight-lost) is the whole procedure, and in outline:

```bash
crewlet secrets keygen -key-id 2026-07      # mint the new key
# 1. add it to secrets.keys on every node, leaving active_key_id alone; restart
# 2. set active_key_id: 2026-07 on every node; restart again
crewlet secrets rekey                         # 3. the secret store and every person's sealed values, through the running node
curl -X POST "$CREWLET_URL/config/reload" \
  -H "Authorization: Bearer $CREWLET_API_TOKEN"   #    and the company document
# 4. drop the old key only once the runbook's conditions hold
```

**Both re-seals are required.** The company document, the secret store (the engine's own blind-index key included, once anybody has been added) and every person's name, address and second factor in the identity estate are all sealed under this keyring, so a key dropped while any of them is still under it leaves that thing unreadable. A reload stores the fleet's document again under the node's active key and activates it. On a stopped node, `crewlet config rekey` re-seals the revision that node boots on — the one its next start must open — and its next start publishes the re-sealed copy only while the fleet still runs the revision it was built on ([offline commands make an offer](#offline-commands-make-an-offer)); `crewlet config rekey -dry-run` reports whether it would re-encrypt without writing. The document's envelope carries the id of the key that sealed it, so either way it is decrypted under whichever key sealed it and re-encrypted under the active key. `crewlet secrets rekey` moves every person's sealed values only through a running node, whose engine holds the identity estate: run with the node's engine stopped, it reaches the fleet's store itself and re-seals that store's rows — the engine's own keys among them — and says to run it again through a running node for the rest.

On a single node, steps 1 and 2 can be one restart. On a fleet they cannot: a node that flips first signs records, cookies and requests under a key its peers do not hold yet.

### Reads and export

Every configuration read **redacts** credentials: `GET /config` (JSON and `?format=yaml`), `GET /config/revisions/{id}`, the revision diff, the entity reads (`GET /config/roles/{handle}`, `/config/units/{id}`, `/config/llm-providers/{key}`, `/config/mcp-servers/{name}`), the dashboard's `config` and `config_entities` queries, `crewlet config show` and `crewlet config diff`. The read path opens the whole document, then replaces every credential value with the literal marker `"__redacted__"`, so the caller sees the config's shape and never a credential. The `/org` view — read by everybody signed in, as the overview — needs no masking because it carries no credential field at all: it is an explicit projection of the charter and the organization tree (see [API endpoints](../reference/api-endpoints.md#get-org)). A revision sealed under a key the node does not hold is refused rather than served.

**What counts as a credential is structural.** A field is a credential because its Go type carries a `secret` tag (`secret:"true"`, or `secret:"content"` for a file's body, which is read whole — see [setup steps](code-sandbox.md#setup-steps--provisioning-the-box)), never because of how its value or its key reads, and a tag on a list or a map covers every element. The tagged company fields today: a `providers.llm` entry's `auth.token`, `auth.api_key`, `auth.credential_bundle` and `env`, the embeddings and sandbox `api_key`, the integration tokens, admin tokens, API and app keys, webhook and signing secrets, a seat's `integrations.slack` bot token and signing secret, `integrations.mattermost.bot_token` and GitHub App private key and webhook secret, every `mcp_env` value on a seat or a unit, `mcp_servers[].env` and `mcp_servers[].headers`, `role.sandbox.env`, and each sandbox setup step's `env` and `files` (under `providers.sandbox.setup` and `role.sandbox.setup`). Everything else (URLs, hosts, flags, model names, the org chart) is served exactly as stored, including every toggle an operator set explicitly: a schedule kept with `enabled: false` reads as disabled, so sending the read back never re-enables it. A test fails the build when a field whose name reads like a credential, or a `map[string]string` named `env`, `headers` or `files`, is added without the tag — in either tier, since Tier A's credentials carry it too (`secrets.keys[].material`, `api.auth.tokens[].token`, `stream.token` and `store.objects.s3.secret_access_key`, listed under [credential positions](../reference/cli.md#credential-positions-x-crewlet-secret)) — so a new credential field is masked by declaring it rather than by remembering to. The same tag marks each of these positions `"x-crewlet-secret": true` in the [generated JSON Schema](../reference/cli.md#credential-positions-x-crewlet-secret), held by a second test to exactly the set a read masks, so tooling that writes a company without the engine (an editor, a CI linter, a Kubernetes operator rendering the document from its own `Secret`s) knows where a `${VAR}` belongs instead of a value.

**Only a whole `${VAR}` reference is shown.** A credential field whose value is exactly one reference (`"${TRACKER_TOKEN}"`) is left visible: it names a credential and carries none, since the value it points at lives in the environment or the [secret store](secret-store.md) and never in this document. Every other non-empty value is masked, including one that embeds a reference beside literal text. `"Bearer sk-live-${SUFFIX}"` and `"sk-live-SECRET-${ROTATION}"` are legitimate (the resolver expands embedded references), and their literal half is a credential. A value using brace syntax the resolver ignores (`${line#host=}`, `${1}`, an unclosed `${`) is not a reference at all and is masked too. "Is a reference" is the engine's own grammar (`internal/envref`). The variable names an embedded reference carries are still listed, with their paths, by `GET /config/references`, which reads the unredacted document and answers names only.

A redacted `GET`, an edited field and a full-document `PUT` round-trip safely: the write path swaps each marker back to the currently stored value before validating, so a round trip never clobbers or exposes a credential. To *change* one, supply the new value (or a `${VAR}`) at that field.

**Members are matched by identity, not by position.** Matching by position meant a pure reorder handed each member its neighbour's credentials, silently, since the lengths still agreed and no marker was left standing to refuse. So every member that can name itself is matched by that name:

- **A seat by its handle, and a unit by its key, anywhere in the document.** One index covers the whole stored revision, so a seat moved from the root into a unit, from one unit to another, or back to the root keeps its credentials, and so does a unit moved under another unit. Reordering the roster, or adding a seat, restores every other member's credentials.
- **An MCP server, and a sandbox setup step, by name within its own list.** Both names are unique within their list: two MCP servers of one name are refused outright, and two setup steps of one name are refused on every write (an [admission rule](#what-a-stored-revision-is-held-to)). Reordering a list, or adding a member, restores every other member's credentials.

**An identity that does not name exactly one member matches nothing.** A seat with a new handle, a unit with a new key, and an MCP server or a setup step given a new name carry no prior value of their own. An identity that is empty, or that the stored revision holds twice, is left out of the match entirely: picking either member, or falling back to position, would hand one member's credentials to another. In every one of these cases the mask stays standing and the write is refused with a validation error naming the field, so the caller writes the real value, or a `${VAR}`, there.

The untyped maps (`mcp_servers[].env` and `.headers`, a `providers.llm` entry's `env`, a sandbox step's `env` and `files`) are masked whole, every value in them, whatever its key is called. A host or a region set beside a token in one of those maps is masked with it; write it as a separate, untagged setting where one exists, or as a `${VAR}`.

`crewlet config export` runs on the host, where the keyring already is, and prints the revision as YAML with its credentials in the clear: it opens a sealed revision and renders the document, so the output is importable as it stands. That is what a restore or a migration needs, and it is the one command that prints credentials. `crewlet config export -redact` masks exactly what the reads above mask, for a dump that is safe to share.

---

## One company per engine

An engine runs exactly one company. It opens one store file, that file holds one `company_config` table, and that table marks **at most one** row the node boots on (`is_active`; none in the unconfigured boot state) and at most one as the fleet's revision (`is_fleets`). There is no tenant column and no row-level scoping: the revision the fleet's pointer names is simply the company, and each engine runs it once it has applied it. The same rule governs the [secret store](secret-store.md): one company per coordination estate, so a variable name alone is the key.

To run a second company, run a second engine with its own database.
