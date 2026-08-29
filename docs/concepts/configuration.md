# Configuration

Crewlet splits configuration into **two tiers** so a founder can evolve their company at runtime — add a role, swap an LLM provider, plug in a new MCP server, rotate an integration credential, update a policy — without redeploying or restarting the engine.

---

## Two-Tier Split

| Tier | Storage | Owner | Update model | Contents |
|------|---------|-------|--------------|----------|
| **A** | `crewlet.yaml` on disk | Ops / SRE | Restart-only | The store file, the stream and coordination slots, this node's identity and roles, API host/port and auth, the secret keyring, logging |
| **B** | The store (`company_config`, versioned) | Founder | Live, API-editable, validated, versioned | Everything else: name, mission, vision, policies, providers (LLM + embeddings), turn engine, learning, MCP servers, notification transports, integrations (Jira / Confluence / Slack / GitHub / GitLab / Forge), org roles & units, token budgets |

**Tier A** controls *how the engine boots*. **Tier B** is *what the company is*.

### Tier A example (`crewlet.yaml`)

```yaml
logging:
  level: info           # debug, info (default), warn, error
  format: console       # console (default), text, json

node:
  id: "node-0"          # optional; see below

stream:
  type: embedded        # a JetStream server inside this process; `nats` or
                        #   `pulsar` point the same slot at an external one
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
not need to set it to run a single engine.

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
| `seats` | Claims seat leases and runs agents | Every trigger queues up unread |
| `workers` | The company-wide singleton duties — scheduler tick, retention sweep, sandbox waiter, skill clustering and curation, seat-subscription creation | Nothing fires on a schedule, no sandbox run is collected, no table is swept |

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
strings and are compared exactly. They are advertised to peers on this
node's presence lease, so a label change takes effect one heartbeat after
the restart that made it — not at the next config activation.

Nothing here means anything to the engine on its own: the org decides
what to select on.

### Tier B example (`company.yaml`)

Everything that defines the company — see [examples/nimbus.company.yaml](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) for a complete reference.

---

## Bootstrap Sequence

The engine boots in this order:

1. Read `crewlet.yaml` (Tier A only — the store path, the stream, coordination, api host/port/auth, secrets, logging)
2. `logging.Configure(level, format, stderr)` — once, in `cmd/crewlet`, which is
   the only thing that sets the destination; a later command changes how loud it
   is with `SetVerbosity` and keeps the sink already installed. The level and
   format come from the file's `logging:` block with any `-log-level` /
   `-log-format` / `-debug` flag layered on top, and only where the flag was
   actually given. Lines emitted *before* this — the file's own `${VAR}`
   warnings, a refused field — come out under the flags alone, which is the
   best a process can do about a file it has not opened yet
3. Open the store file and start or dial the stream
4. Run migrations — every file, in one pass. There is no lock and no phase ordering to serialize: this process owns its file, so nothing can be racing it, and no DDL depends on a value only the config knows. Embedding columns are declared as plain blobs and the vector width is validated in Go against the active revision at write time, so a schema step never has to read the config first (see [`crewlet migrate`](../reference/cli.md#crewlet-migrate)).
5. Start the API process (or embedded API) bound to `api.host:api.port`, wire up auth middleware, register `/config/*` routes
6. Start the [control plane](control-plane.md) — the reconcile loop that polls the activation pointer, plus a broadcast `crewlet.config.revision_activated` nudge that wakes it early
7. `SELECT payload FROM company_config WHERE is_active <> 0`
   - **Row present**: apply the payload, which spawns the full company
   - **No row**: engine stays in the **unconfigured** state — the API keeps serving so an operator can push the first revision via `PUT /config` or `crewlet config import`

### Two equivalent bootstrap entry points

**Option 1 — bootstrap before run (CLI):**

```bash
crewlet config import company.yaml   # one-shot bootstrap of Tier B
crewlet run                          # boots from ./crewlet.yaml + the store
```

**Option 2 — run first, configure over the API:**

```bash
crewlet run                          # boots in UNCONFIGURED state
curl -X PUT https://crewlet.example.com/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: initial bootstrap" \
  --data-binary @company.yaml
# Every node reconciles onto the new activation epoch and spawns the
# company in place — no restart needed.
```

`crewlet run` defaults `-config` to `./crewlet.yaml` and `-company` to `./company.yaml` in the working directory; naming a path is only needed when a file lives elsewhere.

---

## Unconfigured State

Until the first active row exists, the engine holds an empty `Organization` (no name, no roles, no units), an empty provider map, no running MCP processes, no integration clients, and no notification transports.

**What stays running:**

- The Tier A resources — the stream, the store file, the API socket — all up.
- The API's `/config/*` routes and the node's [reconcile loop](control-plane.md) — which is exactly what wakes an unconfigured node when the first revision lands.
- A structured log line carrying `state=unconfigured`, so the unconfigured posture is obvious in logs and on the dashboard.

**What returns degraded responses:**

| Route | Behaviour while unconfigured |
|-------|------------------------------|
| `GET /health` | `200 {"status": "unconfigured", "node": "node-0", "configured": false, ...}` — 200 because the status code is liveness; read `configured` for readiness |
| `GET /ready` | `503 {"ready": false, "configured": false}` — an unconfigured node cannot verify a webhook signature, so it stays out of rotation |
| `GET /config` | `404 {"error": "no_active_revision"}` with a hint |
| `GET /config/revisions` | `200 []` |
| `PUT /config` | Accepted — creates the first active revision. `If-Match` not required when nothing to match against; if supplied must equal `"none"` else `412 Precondition Failed` |
| `POST /config/revisions/{id}/revert` | `404` — no revisions exist yet |
| Per-entity routes (`PUT /config/roles/{handle}`, etc.) and `PATCH /config` | `409 Conflict` — they edit a document, and there is none; initialise via `PUT /config` first |
| `GET /agents`, `GET /tokens/breakdown` | `200` with empty lists / zero counters |
| `POST /webhooks/...` | Signature check still runs (a forgery is rejected as a forgery); body logged at WARNING; returns `503 {"status": "unavailable", "reason": "unconfigured"}` with `Retry-After` so the sender **retries**. A 200 here would tell the sender the delivery was accepted while discarding it — silent, unrecoverable loss the moment one process of several has simply not caught up yet |

Transition out of unconfigured: the first activation moves the pointer → the reconcile tick picks it up → the apply runs → the spawn cascade executes → the engine is fully alive. The dashboard carries the unconfigured state in always-on chrome — an amber live dot and a banner saying inbound webhooks are being dropped — and it clears automatically on the next health tick once `/health` reports `configured: true`. See [Health](../reference/dashboard-design.md#health).

---

## Live Propagation

When a new revision is activated (via `PUT /config`, `PATCH /config`, a per-entity write, a revert, or `crewlet config import`), the revision is stored and the fleet's **activation pointer** is then moved to it; the pointer's own KV sequence *is* the epoch, so the append and the flip cannot come apart. Every node polls that pointer and converges onto it; a broadcast `crewlet.config.revision_activated` event wakes the poll early but carries no work.

The two steps are **not one transaction**, and they span two stores — the node's own database and the coordination KV. That ordering is deliberate: a crash between them leaves a revision nothing points at, which is inert and replaced by the next activation, where the other order would point the fleet at bytes no node had stored.

There is **no leader**, so any node's API may write. What keeps two operators from silently overwriting each other is that the flip is a **compare-and-set** against the revision the write was derived from: the loser gets a `409` naming what won, rather than a `201` for a change the fleet never took. See [Concurrent writes](../reference/api-endpoints.md#concurrent-writes).

This replaced a pair of Pulsar **competing-consumer** subscriptions (`engine-config`, `api-config`) under which exactly one process applied any given revision and the rest ran the previous company indefinitely. The full mechanism — the activation pointer and its epoch, what a lagging node does about its own traffic, and the operator surface — is [Control Plane](control-plane.md); what follows is the apply itself.

### The engine half

Converging applies the payload. `Engine.Apply` is a **straight line with no
comparison and no early return** — there is no apply lock, no payload
short-circuit and no rollback of captured state. It rebuilds the whole epoch,
in a fixed order, and names each stage it got through:

1. **`secrets`** — re-read the secret store and install a fresh resolver snapshot. **First**, because re-activating an unchanged revision is the documented [rotation gesture](secret-store.md): the payload has not moved, so the only thing that can have is what its `${VAR}` references resolve to.
2. **`company`** — validate and build the new epoch, resolving `${VAR}` where each provider is *constructed*. A refusal here changes nothing: this node keeps serving the previous epoch.
3. **`tools`** — equip the new epoch with this node's builtins. An epoch is published, never mutated, so each one gets its own registry; a node that equipped only its first would serve a company whose agents silently lost every builtin at the first config change.
4. **`learning`** — rebuild the reflection workers against the new org. Deliberately cannot fail the apply: reflecting against a stale org is a far smaller wrong than not reflecting.
5. **`sandbox`** — swap the sandbox *manager* only. The coordinator and waiter hold this process's busy set and poll loop; rebuilding them would forget which seats are mid-run and start a second loop over the same rows. **Conditional:** only where this node booted with a sandbox coordinator (see below).
6. **`parties`** — rebuild the party index *before* the epoch is published, so a seat the revision **adds** is addressable the instant the epoch carrying it is current.
7. **`integrations`** — rebuild the four trackers against the new epoch, so work items route by the new chart rather than the boot-time one. Confluence and Jira re-derive a **lead map** from the org — space and project key to unit lead — which is what an unrouted page or issue falls through to. GitLab and GitHub have no lead map; theirs re-resolves the engine credential and the participants lookup that fans a thread out to the seats on it.
8. **`epoch`** — publish the new epoch. This is the swap; everything before it built, everything after it reads the now-current company.
9. **`mailboxes`** — ensure a mailbox exists for every seat. **After** the swap, because it reads the seat list off the current company, and until something creates a new role's mailbox every event published to it is dropped rather than retained. **Conditional:** only where the engine has a node — `crewlet validate` applies to nothing.
10. **`scheduler`** — re-arm the cron loop. After the swap too, and for a sharper version of the same reason: the tick reads schedules off the current company, so arming early would open a window in which the loop fires the outgoing company's crons.

Then `crewlet.config.revision_applied` is published with `status`, the
`applied_subsystems` list and any error.

**A failure is reported by how far it got, not undone.** Every refusal above
happens before the epoch swap, so it leaves the previous epoch current and
serving — that, rather than a rollback, is what makes a failed apply safe. The
returned list is the stages that *did* complete, in the order they completed,
so "secrets, company" names both what was rebuilt and where the refusal landed.
It travels on `ConfigRevisionApplied` into the audit event log, where it
outlives the fleet view's one-minute bucket. The fleet view carries each node's
epoch, revision, status and failure text but *not* the stage list, so that
detail lives on the event rather than on the operator surfaces reading the
bucket. The active row stays active either way; the control plane records
the outcome so peers can see it (see [Control Plane](control-plane.md)).

**Read that list by name, never by number.** Two of the ten stages are
conditional, so a successful apply on a node that booted without a sandbox
reports nine names and the swap is the seventh of them. The numbering above is
the order the code runs, not an index into what a node reports.

> **"No rollback" is not "no mutation".** What the build-first ordering buys is
> that a revision which cannot be *built* changes nothing: `NewCompany`
> validates, resolves the org and constructs the providers without reaching the
> network, so stage 2 is the cheapest place to refuse and the one that costs
> nothing at all. Past it the guarantee narrows. **Stage 5 is the last stage
> that can refuse** — stages 6 and 7 return no error, and stage 8 is the swap —
> and by the time it runs, three things are already mutated: the resolver
> snapshot (stage 1), any shared MCP child whose spec moved plus the skill
> variables (stage 3), and the reflection workers (stage 4). So a sandbox-build
> refusal leaves this node's tool surface and learning workers on the new
> company while it still *serves* the previous epoch, and reports `error`. The
> party index and the trackers are not among them: they are rebuilt after the
> last failure point, which is why they are ordered there. Widening that window
> is what would make `degraded` reachable, which is why everything an apply
> cannot un-apply stays behind the swap.

Two knobs are refused rather than applied live, because applying them would
corrupt data rather than merely disrupt it:

- **`providers.embeddings.dimensions`** — rows already written carry vectors of the old width, and a similarity query across two widths compares nothing. The apply fails naming the declared width and the width the store already holds. Changing it means re-embedding, not a restart. Adding or removing the whole `embeddings` block *is* live in both directions: a company that drops it degrades to recency-only recall on the next turn.
- **`providers.sandbox`** — on a node that booted with a sandbox, a revision whose sandbox block cannot be built is refused rather than published, because the alternative serves a company whose sandbox-enabled seats plan around a box that will never be minted. The coordinator itself is built once, at boot, and only where the booting company had a workable block — so **adding** `providers.sandbox` to a company that started without one is not live in either direction: no coordinator is minted, no `run_sandbox` tool appears, and a broken block is published rather than refused, until the process restarts.

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

**Per-role children are not on this path.** They belong to a seat's *lease*
rather than to the epoch: the apply-time reconcile skips every non-shared
server, and a role's `mcp_env` — which carries the per-agent Slack/GitHub
credentials — is never part of a spec an apply compares. Such a child is
spawned when its seat is claimed and torn down when it is released, so an
`mcp_env` change reaches it when the seat next changes hands, not on the apply
that carried it.

### The API half

**There is no second projection to keep in step.** The API answers every read
through closures over the engine's *current* epoch rather than from a cached
copy of the payload — `GET /agents`, `/org` and the dashboard's queries all
resolve against whatever `Apply` last published, and the inbound webhook
secrets are re-read per delivery the same way. So a rotated signing secret is
picked up by the epoch swap itself; there is nothing that could drift stale and
nothing to refresh.

That is the point of there being one wiring for the embedded and standalone
topologies: what differs between them is only what the node can *see*, through
a single seam, never how many loops are chasing the pointer. Every node runs
exactly one reconciler whatever its `node.roles`, so the two halves cannot
disagree about which epoch they are on.

`configured` is the one field that is not derived per read. It is set **true at
construction and stays true for the life of the process**: the engine only
exists because a company config parsed, validated and built an epoch, and a
failed apply leaves the node serving the previous epoch — which is still a
configured node. What a failed apply changes is the **posture**, and `/ready`
reads that. Collapsing the two would take a correctly-serving node out of a
load balancer's rotation for being behind.

---

## Versioned Revisions

`company_config` is an append-only table — every change is a new revision. The schema:

```sql
CREATE TABLE company_config (
    revision_id        TEXT    NOT NULL PRIMARY KEY,
    parent_revision_id TEXT    REFERENCES company_config(revision_id),
    created_at         INTEGER NOT NULL,          -- unix seconds, UTC
    created_by         TEXT    NOT NULL,          -- token id, e.g. "founder"
    source             TEXT    NOT NULL,          -- "api" | "cli" | "api.revert" | "api.entity"
    summary            TEXT    NOT NULL,          -- short human-readable change note
    payload            TEXT    NOT NULL,          -- the whole document as JSON, or the
                                                  -- sealed envelope when a keyring is set
    is_active          INTEGER NOT NULL DEFAULT 0,
    activated_at       INTEGER
);

-- At most one active revision, enforced by the database rather than by the
-- application remembering to.
CREATE UNIQUE INDEX company_config_one_active_idx
    ON company_config (is_active) WHERE is_active <> 0;
```

One dialect, two certified drivers: every statement has to parse on both Turso
and mainline SQLite, which is why the types here are the four SQLite has
(`TEXT`, `INTEGER`, `REAL`, `BLOB`) rather than `UUID` / `TIMESTAMPTZ` /
`JSONB`, and why a timestamp is unix seconds rather than a date type.

A revert creates a *new* revision whose payload equals a prior one — the audit chain stays intact via `parent_revision_id`.

---

## Auth

**Writes and the whole `/config` surface require `Authorization: Bearer
<token>`. Reads serve without one by default.** Tokens are listed in Tier A
under `api.auth.tokens` and resolved from env vars at API startup. The matched
token's `id` is recorded as `created_by` on each revision the request produces,
so revision history carries meaningful attribution (`alice`, `ci-pipeline`,
`ops`) rather than generic strings.

Reading is what a dashboard does, and the page that would prompt for a token is
itself served unauthenticated — the page that asks for a credential cannot
require one — so a guarded-by-default read surface puts a modal in front of
every first load. Be clear-eyed about what open reads expose, though: `/events`,
`/agents/{id}/memory` and `/ws/stream` carry full LLM transcripts — prompts,
tool arguments, diary entries — to anyone who can reach the port. One line
closes them:

```yaml
api:
  auth:
    allow_anonymous_read: false   # every route needs a token
    tokens:
      - {id: founder, token: "${CREWLET_API_TOKEN_FOUNDER}"}
```

With reads closed the dashboard authenticates its own socket and prompts for a
token when the engine refuses it — including a banner that says *refused*
rather than *disconnected*, since a rejected credential is not an outage that
resolves itself.

The API states which posture it took at startup, on `api_anonymous_read_enabled`
— at `WARNING` when `api.host` is not loopback, at `INFO` when it is. A laptop
and an internet-facing bind are not the same decision, and a warning that fires
identically for both is one nobody reads by the third deployment.

**The guard is mounted whether or not `api.auth` is configured.** It applies one
rule — `requires_token` — and what Tier A supplies is the *posture*, not the
existence of a check. An API built with no Tier A at all therefore has no token
that can match, which means reads serve and every write plus the whole `/config`
surface answers `401`. That is the only safe reading of "an app was built
without being told who may write to it", and it removes the possibility of a
process that serves `/config` writes with nothing in front of them.

Served **without** a token in either posture, because they authenticate by other
means or must be reachable to obtain one:

| Path | Why |
|------|-----|
| `/health`, `/ready` | Probes. An orchestrator has no token, and a liveness check that 401s is a liveness check that fails. A single trailing slash is tolerated (`/health/`), because the guard runs before routing — so the router's redirect to the canonical path only happens if the request gets past the guard first, and a slash must never be the difference between healthy and evicted |
| `/webhooks/*` | Each verifies its provider's HMAC before doing anything — a stronger check than a shared bearer token. Includes the Slack OAuth landing page, which a browser reaches mid-install |
| | **A route whose secret is unset has nothing to verify with, so it fails closed**: `503` + `Retry-After`, never an accepted delivery. The sender retries and the delivery flows once the secret is configured — a deployment that has not set one is stalled, not damaged, and nothing unsigned is ever recorded, published, or shown on the dashboard |
| `/otlp/*` | The signed per-run token in the path *is* the credential |
| `/`, `/dashboard`, `/static/*` | The page that prompts for a token cannot itself require one. It ships no data — every byte it renders comes from an authenticated fetch |

`/ws/stream` follows the same rule as every other read. When reads are closed it
needs a credential like anything else — and browsers can't set headers on a
`WebSocket`, so it accepts `?token=…` as well as the `Authorization` header.
Prefer the header where a client can send one, since query strings tend to land
in proxy access logs.

| Setting | Effect |
|---------|--------|
| `api.auth.tokens` | The accepted bearer tokens. Needed for writes and `/config`, whatever the read posture is |
| `api.auth.allow_anonymous_read: true` *(default)* | `GET`/`HEAD` outside `/config` serve without a token; writes and the whole `/config` surface still require one |
| `api.auth.allow_anonymous_read: false` | Every route needs a token, `/ws/stream` included. The lockdown posture for a deployment that terminates traffic somewhere reachable |
| `api.auth.disabled: true` | Local development only. Everything serves unauthenticated **including writes**, attribution becomes `"anonymous"`, loud `WARNING` at startup |

Two combinations are worth calling out:

- **No tokens at all** is a legitimate posture, not a misconfiguration: reads
  serve and writes are refused outright, because no token can ever match an
  empty list. A deployment that never writes config through the API therefore
  has no credential to manage — strictly safer than minting one it will not use.
- **`allow_anonymous_read: false` with no tokens** is refused at boot. It guards
  every route behind a credential that does not exist, which is not a strict
  posture but an outage whose only symptom is a uniform `401` that reads exactly
  like a wrong token.

**CORS** defaults to same-origin. The dashboard is served by this process so it
needs no entry; list any other browser origin explicitly in
`api.auth.allowed_origins`. The previous `*` default let any site a logged-in
operator happened to visit read every endpoint.

The auth middleware uses `hmac.compare_digest` for constant-time comparison.
Failed attempts log at WARNING (never the candidate token value); successes log
at DEBUG with `operator_id` + `route`.

See the [API endpoints reference](../reference/api-endpoints.md#config--live-config-management-auth-gated) for the per-route auth + status semantics.

---

## Secrets

Crewlet has two secret-handling behaviours for Tier B. Which one is in effect depends solely on whether a **Tier A encryption keyring** is configured.

### Default: `${VAR}` references (no keyring)

With no `secrets:` block in `crewlet.yaml`, the DB stores `${ENV_VAR}` reference strings verbatim and resolution happens at provider / transport / integration construction time (`internal/engine`). The `company_config` table never holds a real secret; the environment is the source of truth. Safe to back up / export, but every deployment must re-provision the referenced env vars, and rotating a key means editing the env + restarting.

A configured keyring also unlocks a second, independent place a `${VAR}` can resolve from: the encrypted [secret store](secret-store.md), consulted ahead of the environment. That is what lets a provisioner hand a minted credential straight to the engine instead of writing a file someone has to source. It is opt-in and inert until a secret is actually stored.

### Encrypted at rest (Tier A keyring configured)

Add a keyring to `crewlet.yaml` and Crewlet encrypts the **entire** `company_config` payload as one opaque blob (AES-256-GCM) before it reaches the DB:

```yaml
# config.yaml (Tier A) — the keyring is the sole root of trust
secrets:
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"   # base64(32 bytes); crewlet secrets keygen
```

The whole document is stored as `{"__encrypted__": "enc:v1:<key_id>:<base64>"}` — nothing about the config's structure (org chart, policies, model choices, or secrets) is visible in the database. A stolen DB reveals nothing.

- **Encrypt on write.** Every write path (`PUT /config`, per-entity `PUT`, `crewlet config import`, `crewlet run -company`) encrypts the whole document before the payload reaches the DB.
- **Decrypt at the read boundary.** The engine, API process, migrations, and CLI each decrypt the blob (`load_config`) into the plaintext structure before use — the Tier A key is required for **every** config read. `${VAR}` references *inside* the config are kept verbatim in the blob and still resolve from the environment at construction time.
- **Fail closed.** If an activated revision is stored encrypted but no keyring is configured (or the key is missing), the engine refuses to boot rather than run with an opaque blob it can't read.
- **One key, not N env vars.** After encrypting, the engine needs only the Tier A key in its environment — not a per-secret env var for every LLM key, MCP token, and webhook secret.

Because the key gates every read, keep it as available as the database itself: the API, dashboard, migrations, and CLI all fail closed without it.

**Threat model.** Encryption at rest defends against *data-at-rest* exposure: a leaked backup, a read replica, a stolen snapshot, an over-broad `SELECT` grant, a `pg_dump` on a laptop — the attacker gets one opaque ciphertext blob per revision, no structure and no credentials. It also keeps config egress clean (`GET /config`, dashboard views, and revision diffs are decrypt-then-redact — no plaintext secrets) and absorbs accidental plaintext (a raw key pasted into `company.yaml` is encrypted on write, so it never lands in the DB in the clear). It does **not** defend against a compromised engine host that holds both the DB and the Tier A key (that host can decrypt — it must, to run; keep the key out of the DB host's backup domain — that separation is the point), a malicious operator with a valid key and API access, or in-memory extraction from the live process. The property: plaintext config exists only transiently in the encrypt/decrypt path and in the live engine's memory — never in durable storage.

### Migrating from `${VAR}` to encrypted

Encryption is opt-in and backward-compatible — a plaintext config boots with or without a keyring. To migrate an existing deployment:

```bash
crewlet secrets keygen --key-id 2026-01     # prints a key + the config.yaml snippet
# add the secrets: block to config.yaml, export CREWLET_SECRET_KEY_2026_01
crewlet config seal                          # encrypts the active revision as one document
```

`crewlet config seal` writes a new revision holding the encrypted document; afterwards the per-secret env vars are no longer needed at runtime (only the Tier A key). It's idempotent — a second run on an already-sealed revision is a no-op.

### Rotation

**Rotating one secret** (e.g. a leaked LLM key) needs no host access: `PUT /config` (or a per-entity write) with the new value — the whole document is re-encrypted under the active key as a new revision.

**Rotating the master key** uses the keyring's multi-key support so there's no downtime:

```bash
crewlet secrets keygen --key-id 2026-07     # mint the new key
# add it to secrets.keys AND set active_key_id: 2026-07,
# keeping the old key in secrets.keys so its ciphertext still decrypts
crewlet config rekey                          # re-encrypt the document under 2026-07
# verify a clean boot, then drop the old key from config.yaml
```

The document's envelope carries the id of the key that sealed it, so `rekey` decrypts under whichever key sealed it and re-encrypts under the active key. `crewlet config rekey --dry-run` reports whether it would re-encrypt without writing.

If you also use the [secret store](secret-store.md), run `crewlet secrets rekey` alongside `crewlet config rekey` before dropping the old key — its rows are sealed under the same keyring and would otherwise become unreadable.

### Reads and export

Every HTTP read path **redacts** secrets behind a `{"encrypted": true, "key_id": null}` marker — `GET /config` (JSON + YAML), revision reads, the revision diff, the entity reads (`GET /query/config_entities?kind=roles|units|llm-providers|mcp-servers&id=...` — the entity paths under `/config` are write-only and answer `405` to a `GET`), and the dashboard `/org` view. The read path decrypts the whole document, then masks every secret leaf, so the caller sees the config's shape but never a secret value — no ciphertext or plaintext egresses. Without the keyring these paths return an opaque `{"__encrypted__": {"encrypted": true}}` rather than leaking.

What counts as a secret leaf: LLM `api_keys`, embeddings / sandbox `api_key`, Jira/Confluence/GitHub/GitLab tokens + webhook/signing secrets, every per-agent `mcp_env` value and per-role Slack cred, and — for the shared `mcp_servers[].env` / `.headers` dicts — any value whose key name signals a secret (a `*_TOKEN` env var, an `Authorization` header). (Org-level `integrations.slack` carries no secrets — it is an empty enable-marker.) Non-secret structure (URLs, hosts, ports, flags, model names) stays visible. The key-name match errs toward **over**-masking — a non-secret key that happens to contain `token`/`authorization` (e.g. `max_tokens`) is masked too — deliberately, so a real secret is never missed. A value that is still a `${VAR}` reference is left **visible** on reads: it is an inert pointer (the real secret lives in the environment or the encrypted [secret store](secret-store.md), never in this payload), so it is neither ciphertext nor a secret-at-rest — masking it would hide the useful variable name and falsely flag it as encrypted. "Is a reference" uses the engine's own grammar (`internal/envref`), i.e. *substitution would actually change this value*; a literal secret merely containing brace syntax the resolver ignores (`${line#host=}`) is a literal, and is masked.

A redacted `GET` → edit a field → full-doc `PUT` round-trips safely: the write path swaps each marker back to the currently-stored (decrypted) value before validating (keep-existing), so a round-trip never clobbers or exposes a secret. To *change* a secret, supply the new value (or a `${VAR}`) at that field.

What counts as a secret leaf is structural, and it covers the untyped surfaces too — `mcp_servers[].env` and `.headers`, and a `cli-agent` provider's `cli.auth.token` / `cli.auth.credential_bundle` / `cli.env`. On those, only keys whose *name* signals a credential are masked, so a host, a region or a URL beside the token stays readable in the config view. A field carrying a credential as a literal rather than a `${VAR}` is masked exactly the same way — `${VAR}` is the convention, not what makes redaction work.

`crewlet config export` runs on the host (you already hold the key) and emits the stored payload verbatim — a plaintext `${VAR}` config when unencrypted, or the inert `{"__encrypted__": "enc:v1:…"}` document blob when encrypted (DR-friendly and round-trippable: re-importing decrypts and re-stores it). `crewlet config export --redact` decrypts the structure but masks every secret for a share-safe dump.

---

## One company per engine

An engine runs exactly one company. It opens one store file, that file holds one `company_config` table, and that table has **at most one** `is_active=TRUE` row (zero in the unconfigured boot state; otherwise one). There is no `tenant_id` column and no row-level scoping — the revision you activate is simply the company the engine runs. The same rule governs the [secret store](secret-store.md): one company per coordination estate, so a variable name alone is the key.

To run a second company, run a second engine with its own database.
