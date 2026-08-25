# Configuration

Crewlet splits configuration into **two tiers** so a founder can evolve their company at runtime — add a role, swap an LLM provider, plug in a new MCP server, rotate an integration credential, update a policy — without redeploying or restarting the engine.

---

## Two-Tier Split

| Tier | Storage | Owner | Update model | Contents |
|------|---------|-------|--------------|----------|
| **A** | `config.yaml` on disk | Ops / SRE | Restart-only | The store file, the stream and coordination slots, this node's identity and roles, API host/port and auth, the secret keyring, debug |
| **B** | The store (`company_config`, versioned) | Founder | Live, API-editable, validated, versioned | Everything else: name, mission, vision, policies, providers (LLM + embeddings), turn engine, learning, MCP servers, notification transports, integrations (Jira / Confluence / Slack / GitHub / GitLab / Plane / Forge), org roles & units, extensions, token budgets |

**Tier A** controls *how the engine boots*. **Tier B** is *what the company is*.

### Tier A example (`config.yaml`)

```yaml
debug: false

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

1. Read `config.yaml` (Tier A only — DSN, queue URL, api host/port/auth, debug)
2. `configure_logging(level)`
3. Open the store file and start or dial the stream
4. Run migrations — every file, in one pass. There is no lock and no phase ordering to serialize: this process owns its file, so nothing can be racing it, and no DDL depends on a value only the config knows. Embedding columns are declared as plain blobs and the vector width is validated in Go against the active revision at write time, so a schema step never has to read the config first (see [`crewlet migrate`](../reference/cli.md#crewlet-migrate)).
5. Start the API process (or embedded API) bound to `api.host:api.port`, wire up auth middleware, register `/config/*` routes
6. Start the [control plane](control-plane.md) — the reconcile loop that polls the activation pointer, plus a broadcast `crewlet.config.revision_activated` nudge that wakes it early
7. `SELECT payload FROM company_config WHERE is_active = TRUE`
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

Until the first `is_active=TRUE` row exists, the engine holds an empty `Organization` (no name, no roles, no units), an empty provider map, no running MCP processes, no integration clients, and no notification transports.

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
| Per-entity routes (`POST /config/roles`, etc.) | `409 Conflict` — operator must initialise via `PUT /config` first |
| `GET /agents`, `GET /tokens/breakdown` | `200` with empty lists / zero counters |
| `POST /webhooks/...` | Signature check still runs (a forgery is rejected as a forgery); body logged at WARNING; returns `503 {"status": "unavailable", "reason": "unconfigured"}` with `Retry-After` so the sender **retries**. A 200 here would tell the sender the delivery was accepted while discarding it — silent, unrecoverable loss the moment one process of several has simply not caught up yet |

Transition out of unconfigured: the first activation moves the pointer → the reconcile tick picks it up → the apply runs → the spawn cascade executes → the engine is fully alive. The dashboard carries the unconfigured state in always-on chrome — an amber live dot and a banner saying inbound webhooks are being dropped — and it clears automatically on the next health tick once `/health` reports `configured: true`. See [Health](../reference/dashboard-design.md#health).

---

## Live Propagation

When a new revision is activated (via `PUT /config`, per-entity write, revert, or `crewlet config import`), the write appends an **activation epoch** in the same transaction. Every node polls that pointer and converges onto it; a broadcast `crewlet.config.revision_activated` event wakes the poll early but carries no work.

This replaced a pair of Pulsar **competing-consumer** subscriptions (`engine-config`, `api-config`) under which exactly one process applied any given revision and the rest ran the previous company indefinitely. The full mechanism — the epoch log, what a lagging node does about its own traffic, and the operator surface — is [Control Plane](control-plane.md); what follows is the apply itself.

### The engine half

Converging applies the payload, which:

1. Takes the apply lock, so the CLI path, the reconcile loop and tests serialise against one another.
2. Re-reads the secret store, then validates the payload as `CompanyConfig` (defence in depth).
3. **No-op short-circuit:** if the new payload equals the current active config **and** its [resolution fingerprint](control-plane.md#rotation) is unchanged, returns `[]` immediately — no snapshot capture, no per-subsystem comparison passes. Same payload with a *moved* fingerprint is a credential rotation, not a no-op: the credential-bearing subsystems (LLM providers, shared and per-role MCP servers, notification transports) rebuild and the rest is skipped.
4. Snapshots in-memory state for rollback (including `_scheduling_config` so a rollback after `_apply_scheduling_live` restores the prior scheduler settings).
5. Dispatches per-subsystem diff handlers in order:
   - **`org`** — spawn new roles, terminate removed (stopping their per-role MCP subprocesses), swap `AgentDefinition` for changed roles, re-derive the per-seat `token_budget` caps from the new org, and re-seed the running notification transports' fall-through routing maps (Jira project / Confluence space / Plane project → unit lead) from the new org — the freshly built map is always pushed, so removing the last `integrations.*` identity *clears* live routing rather than leaving the stale map until restart
   - **`budgets`** — update org `token_budget` (per-role caps are applied by the `org` branch above, since they live on `Role`)

   > **Per-seat caps are a projection of the active org, not an accumulation.** Every org swap re-derives the whole cap set: each agent seat with a positive `token_budget` gets its cap (usage history preserved), and every cap whose seat is gone — role removed, flipped to human, budget dropped to `0` (= unlimited) — is dropped. Crucially the caps cover **every seat in the company on every node**, not just the seats a node happens to be running: caps are config while only *usage* is shared, and a missing local cap is read as "unlimited", so a node that seeded selectively would run a seat with no cap the moment it took that seat over.
   - **`turn_engine`** — push new settings into `TurnEngineSettings` cell; in-flight turns finish on the prior snapshot
   - **`providers`** — LLM providers are rebuilt and swapped in place. The **embeddings** provider is rebuilt with them — model, key and base URL are all live — with **one exception that is refused rather than applied**: `dimensions`. Rows already written carry vectors of the old width, and a similarity query across two widths compares nothing; the apply fails with an error naming the declared width and the width this store already holds. Changing it means re-embedding, not a restart. Adding or removing the whole `embeddings` block *is* live, in both directions: a company that drops it degrades to recency-only recall on the next turn rather than at the next restart.
   - **`scalars`** — `integrations.forge_app_id`, `notification_rate_limit` (the rate limit is propagated onto the running `NotificationService` so it takes effect on the next notification), and `notification_coalesce_window_seconds` / `notification_coalesce_max_batch` (mutated in place on the shared `BatchOptions` the inbox batch consume loops read every cycle — takes effect on the next batch, no re-subscription; see [Event System — Inbox batching](event-system.md#inbox-batching--coalescing))
   - **`restart_required`** — MCP server start/stop/restart for both stdio  and remote http (`restart_http_server`, triggered by a `url` / `headers` change), per-role MCP respawn when a role's `mcp_env` changes (`_respawn_role_mcp` — this carries the per-agent Slack/GitHub credentials too), notification transport dict swap with routing re-seed (Slack apps + Jira/Confluence project/space key→lead maps), integration handle-registry refresh, extension `unregister`/`register`. **Learning** is the one subsystem that does NOT live-restart — the new `learning:` config is stored for the next engine restart and a WARNING is logged; the running `ReflectEngine` / `EpisodeLifecycleWorker` / `SkillCuratorWorker` keep the prior config until then.
6. Refreshes derived state (`DelegationHandler`).
7. Publishes `crewlet.config.revision_applied` with `status`, `applied_subsystems`, optional `error`.

On any mid-apply failure the rollback restores all captured state — and, after the org and the transports are back, re-derives both the per-seat token caps and the running transports' routing maps from the rolled-back org, so a failed apply never leaves live spend limits or webhook routing derived from a revision that was never activated. The error carries which subsystem failed and which ones had already applied. The active row stays active either way — the dashboard banner surfaces divergence. The converge path carries that partial list onto the `config_revision_applied` event so the dashboard can render "applied: org, budgets; failed at: providers" rather than an empty list, and records the outcome in the fleet's [apply status](control-plane.md) so peers can see it.

Rollback **restarts** the transports it restores, routing them through the same swap the apply used, so a failed apply cannot leave the node with a live config and a dead inbound path.

What it still cannot undo is per-role MCP respawn: the failed revision's children are already running, and re-running the spawn sequence for every role inside an already-failing apply trades one failure for a longer, less predictable one. The apply error therefore carries a `degraded` flag, set when the failure came *after* a restart-required subsystem was mutated. Such a node reports the prior epoch while its tool surface may be amputated, so the control plane records it as `degraded`, never counts it as converged, and fails its readiness probe — see [Control Plane](control-plane.md).

### The API half

The API's cached projection refreshes `app.state.org_data`, `agent_roles`, `tools_data`, `github_webhook_secret`, `forge_app_id`, `configured` from the new payload. Without this, `GET /agents` / `/org` / `/health` would drift stale — and, far worse, a rotated webhook signing secret would never be picked up, so inbound HMAC verification would fail against every delivery.

It follows the same activation pointer, and deliberately follows the **pointer rather than the local apply outcome**: these fields decide whether inbound verification succeeds, and refusing deliveries because an apply failed would drop events the queue could otherwise have held. Keeping a stale node from *processing* work is the posture gate's job, not the receiver's.

A merged node (one that runs both `ingress` and `seats`) drives that refresh from the engine's own reconcile tick rather than a second loop, so the two halves can never disagree about which epoch they are on. An ingress-only node runs the loop itself.

---

## Versioned Revisions

`company_config` is an append-only table — every change is a new revision. The schema:

```sql
CREATE TABLE company_config (
    revision_id        UUID         PRIMARY KEY,
    parent_revision_id UUID         REFERENCES company_config(revision_id),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_by         TEXT         NOT NULL,  -- token id, e.g. "founder"
    source             TEXT         NOT NULL,  -- "api" | "cli" | "api.revert" | "api.entity"
    summary            TEXT         NOT NULL,  -- short human-readable change note
    payload            JSONB        NOT NULL,  -- full CompanyConfig snapshot
    is_active          BOOLEAN      NOT NULL DEFAULT FALSE,
    activated_at       TIMESTAMPTZ
);

-- Exactly at most one active revision at any time:
CREATE UNIQUE INDEX company_config_one_active
    ON company_config (is_active) WHERE is_active;
```

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

With no `secrets:` block in `config.yaml`, the DB stores `${ENV_VAR}` reference strings verbatim and resolution happens at provider / transport / integration construction time (`crewlet.engine_builders._resolved_for_runtime`). The `company_config` table never holds a real secret; the environment is the source of truth. Safe to back up / export, but every deployment must re-provision the referenced env vars, and rotating a key means editing the env + restarting.

A configured keyring also unlocks a second, independent place a `${VAR}` can resolve from: the encrypted [secret store](secret-store.md) (`secret_values`), consulted ahead of the environment. That is what lets a provisioner hand a minted credential straight to the engine instead of writing a file someone has to source. It is opt-in and inert until a secret is actually stored.

### Encrypted at rest (Tier A keyring configured)

Add a keyring to `config.yaml` and Crewlet encrypts the **entire** `company_config` payload as one opaque blob (AES-256-GCM) before it reaches the DB:

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

Every HTTP read path **redacts** secrets behind a `{"encrypted": true, "key_id": null}` marker — `GET /config` (JSON + YAML), revision reads, the revision diff, the entity GETs (`/config/llm-providers/{key}`, `/config/roles/{handle}`, `/config/units/{name}`), and the dashboard `/org` view. The read path decrypts the whole document, then masks every secret leaf, so the caller sees the config's shape but never a secret value — no ciphertext or plaintext egresses. Without the keyring these paths return an opaque `{"__encrypted__": {"encrypted": true}}` rather than leaking.

What counts as a secret leaf: LLM `api_keys`, embeddings / sandbox `api_key`, Jira/Confluence/GitHub/GitLab/Plane tokens + webhook/signing secrets, every per-agent `mcp_env` value and per-role Slack cred, and — for the shared `mcp_servers[].env` / `.headers` dicts — any value whose key name signals a secret (a `*_TOKEN` env var, an `Authorization` header). (Org-level `integrations.slack` carries no secrets — it is an empty enable-marker.) Non-secret structure (URLs, hosts, ports, flags, model names) stays visible. The key-name match errs toward **over**-masking — a non-secret key that happens to contain `token`/`authorization` (e.g. `max_tokens`) is masked too — deliberately, so a real secret is never missed. A value that is still a `${VAR}` reference is left **visible** on reads: it is an inert pointer (the real secret lives in the environment or the encrypted [secret store](secret-store.md), never in this payload), so it is neither ciphertext nor a secret-at-rest — masking it would hide the useful variable name and falsely flag it as encrypted. "Is a reference" uses the engine's own grammar (`crewlet.env_refs`), i.e. *substitution would actually change this value*; a literal secret merely containing brace syntax the resolver ignores (`${line#host=}`) is a literal, and is masked.

A redacted `GET` → edit a field → full-doc `PUT` round-trips safely: the write path swaps each marker back to the currently-stored (decrypted) value before validating (keep-existing), so a round-trip never clobbers or exposes a secret. To *change* a secret, supply the new value (or a `${VAR}`) at that field.

What counts as a secret leaf is structural, and it covers the untyped surfaces too — `mcp_servers[].env` and `.headers`, `integrations.transports[]`, and a `cli-agent` provider's `cli.auth.token` / `cli.auth.credential_bundle` / `cli.env`. On those, only keys whose *name* signals a credential are masked, so a host, a region or a URL beside the token stays readable in the config view. A field carrying a credential as a literal rather than a `${VAR}` is masked exactly the same way — `${VAR}` is the convention, not what makes redaction work.

`crewlet config export` runs on the host (you already hold the key) and emits the stored payload verbatim — a plaintext `${VAR}` config when unencrypted, or the inert `{"__encrypted__": "enc:v1:…"}` document blob when encrypted (DR-friendly and round-trippable: re-importing decrypts and re-stores it). `crewlet config export --redact` decrypts the structure but masks every secret for a share-safe dump.

---

## One company per engine

An engine runs exactly one company. It opens one store file, that file holds one `company_config` table, and that table has **at most one** `is_active=TRUE` row (zero in the unconfigured boot state; otherwise one). There is no `tenant_id` column and no row-level scoping — the revision you activate is simply the company the engine runs. The same rule governs [`secret_values`](secret-store.md): one company per database, so a variable name alone is the primary key.

To run a second company, run a second engine with its own database.
