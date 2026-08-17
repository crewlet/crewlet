# Configuration

Crewlet splits configuration into **two tiers** so a founder can evolve their company at runtime — add a role, swap an LLM provider, plug in a new MCP server, rotate an integration credential, update a policy — without redeploying or restarting the engine.

---

## Two-Tier Split

| Tier | Storage | Owner | Update model | Contents |
|------|---------|-------|--------------|----------|
| **A** | `config.yaml` on disk | Ops / SRE | Restart-only | DB DSN, queue (Pulsar) URL, API host/port, API auth tokens, debug, knowledge backend |
| **B** | PostgreSQL (`company_config` table, JSONB, versioned) | Founder | Live, API-editable, validated, versioned | Everything else: name, mission, vision, policies, providers (LLM + embeddings), turn engine, learning, MCP servers, notification transports, integrations (Jira / Confluence / Slack / GitHub / GitLab / Plane / Forge), org roles & units, extensions, token budgets |

**Tier A** controls *how the engine boots*. **Tier B** is *what the company is*.

### Tier A example (`config.yaml`)

```yaml
debug: false

node:
  id: "node-0"          # optional; see below

providers:
  queue:
    type: pulsar
    url: "pulsar://localhost:6650"
  database:
    dsn: "postgresql://crewlet:crewlet@localhost:5432/crewlet"
  knowledge:
    type: pgvector

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

### Tier B example (`company.yaml`)

Everything that defines the company — see [examples/nimbus.company.yaml](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) for a complete reference.

---

## Bootstrap Sequence

The engine boots in this order:

1. Read `config.yaml` (Tier A only — DSN, queue URL, api host/port/auth, debug)
2. `configure_logging(level)`
3. Connect to Pulsar + PostgreSQL
4. Run migrations in two phases, serialized behind a PostgreSQL advisory lock so concurrent processes wait rather than race: first apply the self-contained bootstrap tables (`company_config`, `secret_values`, `leases`), read the active revision's `providers.embeddings.dimensions`, then apply the rest with that value so the pgvector columns (`episodes`, `agent_diary`) are sized to the configured embedding model. The width is **never guessed** — with no active revision the run stops before those migrations and they apply later, when a config declares one (see [`crewlet migrate`](../reference/cli.md#crewlet-migrate)). A company bootstrapped through the unconfigured state gets them applied as part of its first `apply_config`.
5. Start the API process (or embedded API) bound to `api.host:api.port`, wire up auth middleware, register `/config/*` routes
6. Subscribe the engine to `crewlet.config.revision_activated` on Pulsar
7. `SELECT payload FROM company_config WHERE is_active = TRUE`
   - **Row present**: call `apply_config(payload)` which spawns the full company
   - **No row**: engine stays in the **unconfigured** state — the API keeps serving so an operator can push the first revision via `PUT /config` or `crewlet config import`

### Two equivalent bootstrap entry points

**Option 1 — bootstrap before run (CLI):**

```bash
crewlet config import company.yaml   # one-shot bootstrap of Tier B
crewlet run                          # boots from ./config.yaml + DB
```

**Option 2 — run first, configure over the API:**

```bash
crewlet run                          # boots in UNCONFIGURED state
curl -X PUT https://crewlet.example.com/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: initial bootstrap" \
  --data-binary @company.yaml
# Engine receives crewlet.config.revision_activated and spawns the
# company in place — no restart needed.
```

`crewlet run` defaults its config path to `./config.yaml` in the current working directory; passing an explicit path is only needed when the file lives elsewhere.

---

## Unconfigured State

Until the first `is_active=TRUE` row exists, the engine holds an empty `Organization` (no name, no roles, no units), an empty provider map, no running MCP processes, no integration clients, and no notification transports.

**What stays running:**

- The Tier A connections — Pulsar, PostgreSQL, the API socket — all up.
- The API's `/config/*` routes and the engine's `revision_activated` subscriber.
- Structlog with `state=unconfigured` so the unconfigured posture is obvious in logs and on the dashboard.

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

Transition out of unconfigured: the first `crewlet.config.revision_activated` arrives → `apply_config` runs → spawn cascade executes → engine is fully alive. The dashboard carries the unconfigured state in always-on chrome — an amber live dot and a banner saying inbound webhooks are being dropped — and it clears automatically on the next health tick once `/health` reports `configured: true`. See [Health](../reference/dashboard-design.md#health).

---

## Live Propagation

When a new revision is activated (via `PUT /config`, per-entity write, revert, or `crewlet config import`), the API publishes `crewlet.config.revision_activated` on Pulsar. Both the engine (consumer group `engine-config`) and the API process (consumer group `api-config`) subscribe; each handler does independent work.

### Engine subscriber

The engine handler runs `await self.apply_config(payload)`, which:

1. Acquires `self._apply_lock` (serialises CLI + Pulsar callers).
2. Validates the payload as `CompanyConfig` (defence in depth).
3. **No-op short-circuit:** if the new payload equals the current active config, returns `[]` immediately — no snapshot capture, no per-subsystem comparison passes.
4. Snapshots in-memory state for rollback (including `_scheduling_config` so a rollback after `_apply_scheduling_live` restores the prior scheduler settings).
5. Dispatches per-subsystem diff handlers in order:
   - **`org`** — spawn new roles (seeding the per-role `token_budget`), terminate removed (dropping their budget + stopping their per-role MCP subprocesses), swap `AgentDefinition` for changed roles, apply a changed role's per-role `token_budget` in place, and re-seed the running notification transports' fall-through routing maps (Jira project / Confluence space / Plane project → unit lead) from the new org — the freshly built map is always pushed, so removing the last `integrations.*` identity *clears* live routing rather than leaving the stale map until restart
   - **`budgets`** — update org `token_budget` (per-role caps are applied by the `org` branch above, since they live on `Role`)
   - **`turn_engine`** — push new settings into `TurnEngineSettings` cell; in-flight turns finish on the prior snapshot
   - **`providers`** — re-instantiate LLM providers and swap dict entries in place (preserves dict identity so `TurnEngine` sees the swap). The **embeddings** provider is *not* live-rewired: it is wired deeply into the diary / episode store / reflect engine at boot (and fixes the pgvector column width at migration time), so a change stores the new provider and logs a restart-required WARNING — the running learning subsystem keeps the prior provider until the next restart.
   - **`scalars`** — `integrations.forge_app_id`, `notification_rate_limit` (the rate limit is propagated onto the running `NotificationService` so it takes effect on the next notification), and `notification_coalesce_window_seconds` / `notification_coalesce_max_batch` (mutated in place on the shared `BatchOptions` the inbox batch consume loops read every cycle — takes effect on the next batch, no re-subscription; see [Event System — Inbox batching](event-system.md#inbox-batching--coalescing))
   - **`restart_required`** — MCP server start/stop/restart for both stdio (`MCPToolBridge.restart_server`) and remote http (`restart_http_server`, triggered by a `url` / `headers` change), per-role MCP respawn when a role's `mcp_env` changes (`_respawn_role_mcp` — this carries the per-agent Slack/GitHub credentials too), notification transport dict swap with routing re-seed (Slack apps + Jira/Confluence project/space key→lead maps), integration handle-registry refresh, extension `unregister`/`register`. **Learning** is the one subsystem that does NOT live-restart — the new `learning:` config is stored for the next engine restart and a WARNING is logged; the running `ReflectEngine` / `EpisodeLifecycleWorker` / `SkillCuratorWorker` keep the prior config until then.
6. Refreshes derived state (`DelegationHandler`).
7. Publishes `crewlet.config.revision_applied` with `status`, `applied_subsystems`, optional `error`.

On any mid-apply failure: `_rollback(snapshot)` restores all captured state — and, after the org and transports dict are back, re-seeds the running transports' routing maps from the rolled-back org, so a failed apply never leaves live webhook routing derived from a revision that was never activated — and `ConfigApplyError(subsystem, original, applied_before_failure)` is raised. The DB row stays `is_active=TRUE` either way — the dashboard banner surfaces divergence. The Pulsar `revision_activated` handler unpacks `applied_before_failure` from the exception onto `ConfigRevisionApplied.applied_subsystems` so the dashboard can render "applied: org, budgets; failed at: providers" rather than an empty list.

### API subscriber

The API handler refreshes `app.state.org_data`, `agent_roles`, `tools_data`, `github_webhook_secret`, `forge_app_id`, `configured` from the new payload. Without this, `GET /agents` / `/org` / `/health` would drift stale until the API restarts.

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

Every `/config/*` route requires `Authorization: Bearer <token>`. Tokens are listed in Tier A under `api.auth.tokens` and resolved from env vars at API startup. The matched token's `id` is recorded as `created_by` on each revision the request produces, so revision history carries meaningful attribution (`alice`, `ci-pipeline`, `ops`) rather than generic strings.

The auth middleware uses `hmac.compare_digest` for constant-time comparison. Failed attempts log at WARNING (never the candidate token value); successes log at DEBUG with `operator_id` + `route`.

For local development: `api.auth.disabled: true` opts out (loud WARNING at startup, attribution becomes `"anonymous"`). Never use in production.

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

- **Encrypt on write.** Every write path (`PUT /config`, per-entity `PATCH`, `crewlet config import`, `crewlet run --import-company`) encrypts the whole document before the payload reaches the DB.
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

`crewlet config export` runs on the host (you already hold the key) and emits the stored payload verbatim — a plaintext `${VAR}` config when unencrypted, or the inert `{"__encrypted__": "enc:v1:…"}` document blob when encrypted (DR-friendly and round-trippable: re-importing decrypts and re-stores it). `crewlet config export --redact` decrypts the structure but masks every secret for a share-safe dump.

---

## One company per engine

An engine runs exactly one company. It connects to one PostgreSQL database, that database holds one `company_config` table, and that table has **at most one** `is_active=TRUE` row (zero in the unconfigured boot state; otherwise one). There is no `tenant_id` column and no row-level scoping — the revision you activate is simply the company the engine runs. The same rule governs [`secret_values`](secret-store.md): one company per database, so a variable name alone is the primary key.

To run a second company, run a second engine with its own database.
