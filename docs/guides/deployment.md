# Deployment

Crewlet requires two infrastructure services: **Apache Pulsar** and **PostgreSQL** with the **TimescaleDB** and **pgvector** extensions. One database server holds everything — operational state (`token_usage`, `chat_thread_follows`), the per-agent diary vector store (`agent_diary`, pgvector), the episodic vector store (`episodes`, pgvector), and the event store (`crewlet_events` TimescaleDB hypertable).

---

## Docker Compose (Recommended)

The included `docker-compose.yml` provides all services:

```bash
cp .env.example .env    # copy default env vars (first time only)
docker compose up -d    # start all services
docker compose down     # stop and remove containers
```

| Service | Port | Details |
|---------|------|---------|
| Apache Pulsar | 6650 / 8080 | `apachepulsar/pulsar:latest` standalone — 6650 binary protocol (the engine connects here), 8080 admin/REST |
| Dekaf (Pulsar UI) | 8090 | Pulsar web UI — topics, subscriptions, backlog, message browse |
| PostgreSQL | 5432 | `timescale/timescaledb:latest-pg18` image — TimescaleDB + pgvector preloaded. User/pass: `crewlet/crewlet` |
| pgweb | 8150 | PostgreSQL web UI, auto-connected |

The Pulsar web UI is [Dekaf](https://pulsar.apache.org/docs/next/administration-dekaf-ui/) (the UI the Pulsar docs recommend — Apache-2.0, no account/license), auto-wired to the broker. The CLI works too, e.g. `docker compose exec pulsar bin/pulsar-admin topics list public/default`. Dekaf is just a UI; remove the `dekaf` service from `docker-compose.yml` if you don't want it.

**Local vs remote access.** Dekaf renders an absolute `<base href>` from `DEKAF_PUBLIC_BASE_URL` (default `http://localhost:8090`), so the SPA's own API calls target that URL. Locally, just open <http://localhost:8090>. **When Dekaf runs on a remote server**, set `DEKAF_PUBLIC_BASE_URL` to the address you actually open in the browser — e.g. in `.env`:

```bash
DEKAF_PUBLIC_BASE_URL=http://<server-ip-or-host>:8090
```

Otherwise the page loads but every API call goes to the *browser machine's* `localhost:8090`, which fails.

**`upstream connect error ... connection failure ... 111`.** That's Dekaf's Envoy (on 8090) reporting its backend was unreachable. Most often it's the remote-access case above (calls hitting the wrong host); it can also mean you opened the UI before Dekaf finished starting, or its backend was starved on a RAM-pressured host. The service has a healthcheck — wait until `docker compose ps` shows `dekaf` as `healthy` (or use `docker compose up --wait`), make sure the broker/host is healthy (see the JVM-memory note above), and if it persists check `docker logs crewlet-dekaf-1` and `docker stats`.

**JVM memory (most important).** The `apachepulsar/pulsar` image defaults to `PULSAR_MEM="-Xms2g -Xmx2g -XX:MaxDirectMemorySize=4g"` — the standalone JVM commits 2 GB of heap *at startup* and can grow direct memory to 4 GB (~6 GB total). On a host without that much free RAM (alongside Postgres and the engine), the box starts **swapping**, which presents as a sustained 100+ MB/s disk-read storm and high load right when you start the project, and can make the host itself unresponsive (you can lose SSH). No disk/retention setting fixes this — it's a memory problem. The bundled `docker-compose.yml` caps it to `-Xms512m -Xmx1g -XX:MaxDirectMemorySize=512m` (~800 MB idle, stays bounded under load) and sets `mem_limit: 2g` as a hard container ceiling so it can never swap the host. Raise these if you run a heavier workload and have the RAM. (Pulsar's own [docker-compose guide](https://pulsar.apache.org/docs/next/getting-started-docker-compose/) assumes ≥ 8 GB for a full cluster; the capped standalone here needs far less.)

**Orphaned subscriptions.** Pulsar keeps a message until *every* subscription has acked it, so a durable subscription left behind by an unclean shutdown — e.g. one of the dashboard's per-tab broadcast consumers after a crash — would otherwise pin events on disk indefinitely. The bundled `docker-compose.yml` sets `subscriptionExpirationTimeMinutes=30`, which reaps a subscription that has had no connected consumer for 30 minutes (releasing its backlog). This is non-lossy for live work — the engine's own subscriptions have a connected consumer the whole time it runs. We deliberately do **not** set an aggressive message TTL or backlog-eviction quota here: those apply uniformly to the engine's durable inboxes too and would silently drop legitimately-queued tasks (e.g. for an agent that's briefly down). Genuine send/queue pressure is handled at the application layer (the `PulsarEventQueue` retries transient publish failures with backoff) and the JVM memory cap is what actually protects the host.

If a broker has already bloated (e.g. from repeated unclean kills on a broker running without the JVM memory cap), clear the accumulated state: `docker compose down -v` (wipes the `pulsar-data` volume) then `docker compose up -d`. To wipe **only** Pulsar (keeping Postgres): `docker compose rm -sf pulsar && docker volume rm crewlet_pulsar-data`. If you want acknowledged events kept for longer queue-side replay, set a namespace retention policy, e.g. `docker compose exec pulsar bin/pulsar-admin namespaces set-retention public/default --time 7d --size -1`.

**Custom tenant / namespace.** By default all crewlet topics live under Pulsar's built-in `public/default`, which exists on every broker. To isolate crewlet on a shared cluster, set `providers.queue.tenant` / `providers.queue.namespace` in the Tier A config — topics then become `persistent://<tenant>/<namespace>/<subject>`. The engine is deliberately **data-plane only**: it never calls the Pulsar admin API, so it cannot (and will not) create tenants or namespaces. Pulsar auto-creates *topics*, never tenants/namespaces — so provision them out-of-band before starting the engine:

```bash
# one-time, by the operator (or your IaC):
docker compose exec pulsar bin/pulsar-admin tenants create crewlet
docker compose exec pulsar bin/pulsar-admin namespaces create crewlet/prod
```

```yaml
# config.yaml (Tier A)
providers:
  queue:
    type: pulsar
    url: "pulsar://localhost:6650"
    tenant: crewlet
    namespace: prod
```

If the tenant/namespace doesn't exist, the engine fails at startup with the broker's "namespace/tenant not found" error on its first subscription — create it and restart.

**Authentication.** The bundled compose runs the broker **unauthenticated**, which is fine on a laptop and wrong anywhere else. For a broker reachable beyond localhost, enable Pulsar's token authentication and authorization: the engine authenticates as its own *role* (the JWT's `sub` claim), and you grant that role produce/consume on its namespace only, so a stolen or stray client gets `AuthorizationError` instead of your agents' inboxes.

1. Generate a secret key and tokens, and copy the key to the host
   `./pulsar-keys/` directory that step 2 mounts — generating it straight
   into `/pulsar/keys/` would strand it in the container's ephemeral
   filesystem, and the broker recreated in step 2 (with the mount) would
   come up without its signing key. These are credentials; the repo's
   default `.gitignore` covers `pulsar-keys/`:

   ```bash
   docker compose exec pulsar bin/pulsar tokens create-secret-key --output /pulsar/secret.key
   docker compose exec pulsar bin/pulsar tokens create --secret-key file:///pulsar/secret.key --subject admin   # operator
   docker compose exec pulsar bin/pulsar tokens create --secret-key file:///pulsar/secret.key --subject engine  # the engine
   mkdir -p pulsar-keys
   docker compose cp pulsar:/pulsar/secret.key pulsar-keys/secret.key
   ```

2. Enable auth on the broker (compose `pulsar` service; mount the key dir and update the healthcheck, which must now authenticate too):

   ```yaml
   environment:
     PULSAR_PREFIX_authenticationEnabled: "true"
     PULSAR_PREFIX_authenticationProviders: "org.apache.pulsar.broker.authentication.AuthenticationProviderToken"
     PULSAR_PREFIX_authorizationEnabled: "true"
     PULSAR_PREFIX_superUserRoles: "admin"
     PULSAR_PREFIX_tokenSecretKey: "file:///pulsar/keys/secret.key"
     PULSAR_PREFIX_brokerClientAuthenticationPlugin: "org.apache.pulsar.client.impl.auth.AuthenticationToken"
     PULSAR_PREFIX_brokerClientAuthenticationParameters: "token:${PULSAR_ADMIN_TOKEN}"
   volumes:
     - ./pulsar-keys:/pulsar/keys:ro
   healthcheck:
     test: ["CMD-SHELL", "curl -fsS -H \"Authorization: Bearer ${PULSAR_ADMIN_TOKEN}\" http://localhost:8080/admin/v2/brokers/health"]
   ```

3. Create the namespace and grant the engine's role — and only that role — access (operator, one-time):

   ```bash
   A="--auth-plugin org.apache.pulsar.client.impl.auth.AuthenticationToken --auth-params token:$PULSAR_ADMIN_TOKEN"
   docker compose exec pulsar bin/pulsar-admin $A tenants create crewlet
   docker compose exec pulsar bin/pulsar-admin $A namespaces create crewlet/prod
   docker compose exec pulsar bin/pulsar-admin $A namespaces grant-permission crewlet/prod --role engine --actions produce,consume
   ```

4. Point the engine at that namespace with its token (Tier A config; tokens are bearer secrets — use `pulsar+ssl://` plus `tls_trust_certs_path` whenever the broker isn't on localhost):

   ```yaml
   providers:
     queue:
       type: pulsar
       url: "pulsar://broker:6650"
       tenant: crewlet
       namespace: prod
       auth_token: "${CREWLET_PULSAR_TOKEN}"
   ```

5. Dekaf needs credentials too once auth is on — give it a token via `DEKAF_DEFAULT_PULSAR_AUTH: '{"type":"jwt","token":"<token>"}'`.

Verified end-to-end: with the grant in place the engine completes a full publish/subscribe roundtrip in its own namespace, is refused with `AuthorizationError` on any namespace it was not granted, and anonymous connections are rejected outright.

### Plane (optional profile)

A local [Plane](../integrations/plane.md) fork instance ships in the main `docker-compose.yml` under the **`plane` profile** — one compose file for everything, profile-gated like GitLab so a plain `docker compose up` leaves the thirteen-service stack out:

```bash
docker compose --profile plane up -d   # no --wait: the migrator is a one-shot job
scripts/plane-dev-bootstrap.sh
```

On a **remote host**, set `PLANE_PUBLIC_URL` (e.g. `http://<server-ip>:8091`) on both commands — it feeds Plane's `WEB_URL`/CORS (where redirects and shared links come from) and is written into `.env.plane` as `${PLANE_URL}`, the reference the shipped company config resolves. The UI lands on `http://localhost:8091` otherwise; budget **~2–2.5 GB RAM** for the whole stack (no per-service `mem_limit`s — none of these small services has the multi-GB-single-process pathology the Pulsar/GitLab caps exist for). The S3 store is [RustFS](https://github.com/rustfs/rustfs) (MinIO's community image is de-facto deprecated), behind the `plane-minio` network alias the proxy's baked Caddyfile expects; RabbitMQ 4.x runs with a compose-shipped permit for the deprecated transient queues Celery's control plane still declares (without it the worker crash-loops and webhook deliveries never run). The bootstrap steps, each idempotent: **(1)** polls the API healthy (migrations included), **(2)** creates the founder (instance admin) plus their personal API token and writes `PLANE_FOUNDER_USER_ID` + `PLANE_URL` into `.env.plane`, **(3)** creates the `nimbus` workspace, **(4)** archives the workspace-named demo project Plane seeds (a decoy agents otherwise wander into), **(5–6)** with `COMPANY=` set runs `crewlet plane provision --create-projects --webhook-url …` then `crewlet plane import` for the example docs + tool skills, and **(7)** prints the engine next steps (the engine's [embedded API](#single-process-embedded-api--the-single-host-default) is the webhook receiver). The full walkthrough and the end-to-end webhook loop live in [Plane § Local testing](../integrations/plane.md#local-testing); it is not duplicated here.

---

## Running the Engine + API

### Single Process (embedded API — the single-host default)

Any `api.port > 0` in the Tier A YAML makes `crewlet run` start an **embedded API server inside the engine process** — one process runs the engine, the dashboard, and every webhook route:

```yaml
api:
  port: 80       # 0 (the default) disables the embedded API
```

```bash
crewlet run config.yaml            # engine + embedded API on :80
```

(`--api-port 80` on the command line does the same.) This is the shape every single-host walkthrough in these docs uses — the bundled `examples/nimbus.config.yaml` ships `api.port: 80`, and that embedded server **is** the webhook target the integrations register (e.g. `http://host.docker.internal:80/webhooks/plane`). Binding 80 as a non-root process needs privileged-port access on Linux: `sudo sysctl net.ipv4.ip_unprivileged_port_start=80` (persist in `/etc/sysctl.d/`) or grant the binary `CAP_NET_BIND_SERVICE`. Avoid 8080 (Pulsar's admin port in the bundled `docker-compose.yml`) and make sure nothing else owns 80 — keep Plane's `PLANE_LISTEN_PORT` at its 8091 default.

Do **not** also start a second node on the same host with such a file — both read the same `api.port`, and the second binder hits `EADDRINUSE` and kills whichever server came second.

### Separate Processes (split deployment, Pulsar backend)

Run ingress as its own node when you want the webhook receiver to stay up across engine restarts, or the two on separate hosts. They connect through Apache Pulsar. Both read the same Tier A file, so give each one its roles — and its own `--api-port`, since only the ingress node should bind one:

```bash
# Once, first: bring the schema up (safe to re-run; takes an advisory lock)
crewlet migrate config.yaml

# Terminal 1: the agents and the fleet duties, no HTTP
crewlet run config.yaml --roles seats,workers --api-port 0

# Terminal 2: the webhook receiver and the dashboard
crewlet run config.yaml --roles ingress --api-host 0.0.0.0 --api-port 8000
```

Give each node a distinct `node.id` (or `CREWLET_NODE_ID`) — two nodes sharing an id miscount the fleet. See [Running a Fleet](fleet.md).

`crewlet migrate` is idempotent and safe to re-run. Both long-lived
processes also still auto-migrate on boot, and starting them together is no
longer a race: the run takes a PostgreSQL advisory lock, each migration file
applies in its own transaction, and the embedding width is read from the
active revision rather than guessed. Running the explicit step first is
still recommended — it turns a schema change into an observable step instead
of a side effect of whichever process started first. On a database with no
company config yet, the run stops before the `vector(N)` migrations rather
than guessing the width — see
[the CLI reference](../reference/cli.md#the-embedding-width-and-why-a-run-can-stop-early).

Both take the **Tier A** bootstrap file (`config.yaml`) — the founder-owned company YAML is imported into the database separately (`crewlet config import` or `crewlet run --import-company`).

- **`--roles seats`** runs the agents — claims seat leases, boots the instances, processes their turns
- **`--roles ingress`** serves the REST API — receives webhooks (Slack, Plane, GitLab, Jira, GitHub, Confluence) and publishes them to the event queue
- **`--roles workers`** runs the company-wide duties — the scheduler tick, the retention sweeps, the sandbox waiter

They are one command, and they build the **same** application: every node learns the company from the active config revision and the live picture from the broadcast event stream. Point `CREWLET_SANDBOX_OTEL_RECEIVER_URL` at whichever node is externally reachable — an `ingress` one, which serves the `/otlp/{token}/v1/{signal}` receiver (sandbox tokens are signed, so the node that mints and the node that verifies need no shared memory). Signing uses the Tier A keyring, so a split deployment needs one configured (`crewlet secrets keygen`); without it each process signs with an ephemeral key and logs a warning.

Point liveness probes at `/health` (stays `200` through a drain) and load-balancer readiness at `/ready` (`503` while draining or before the first config revision applies).

Both communicate through Pulsar. Both accept `--debug` for verbose logging.

### Replica count

**Run one `crewlet run`, and scale up before you scale out.** A single
engine handles many concurrent turns — agent handlers are `asyncio`
tasks, so the practical ceiling is LLM provider rate limits and host
memory, not process count. One node is the design's degenerate case, not
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

> **A fleet needs both a database and the right broker settings.**
>
> Seat leases live in PostgreSQL. With no `providers.database.dsn` they fall
> back to an in-process store, and every node then believes it owns the whole
> company — the engine logs `seat_placement_is_process_local` at boot.
>
> Pulsar's two reapers must also be turned off, because an unowned seat's
> subscription has no connected consumer and both reapers delete exactly that:
> set `subscriptionExpirationTimeMinutes` to `0` and
> `brokerDeleteInactiveTopicsEnabled` to `false` (or
> `brokerDeleteInactiveTopicsMode` to `delete_when_subscriptions_caught_up`).
> The repo's `docker-compose.yml` ships these values. Nothing in the engine can
> detect a broker that is quietly deleting a quiet seat's mail.

Give each node a distinct id — `node.id` in the Tier A file, or the
`CREWLET_NODE_ID` environment variable, which is how a container orchestrator
injects a pod name without templating the config. Two nodes sharing an id
miscount the fleet and each compute too small a share.

Bring the schema up **before** starting any node, with
[`crewlet migrate`](../reference/cli.md#crewlet-migrate).

What a fleet gets right, each of which was a real defect before:

- *Duplicate Slack posts, duplicate Jira comments, two contradictory plans for one webhook.* A seat's inbox is attached only by the node holding its lease, admission is gated on a renew fresh enough to prove exclusivity, and the turn loop re-checks the fence before every round and every write-capable tool. A turn that finished but whose delivery was never acked is not re-run, because the [completion ledger](../concepts/seat-ownership.md#the-completion-ledger) records what shipped.
- *Live coding sandboxes torn down mid-run.* Recovery is a per-seat step inside the acquire hook, fenced on the claiming node's epoch, instead of a fleet-wide scan that treated every in-flight run as abandoned.
- *Config activation.* Delivered by the [control plane](../concepts/control-plane.md) — an append-only epoch log every node polls — rather than the competing-consumer subscription that used to let exactly one replica apply a revision while the rest ran the previous company.
- *Token budgets.* A shared PostgreSQL counter, so an org cap of 500 k is 500 k across the fleet.
- *Duplicate auto-drafted skill pages and N× LLM spend on synthesis.* Skill clustering and curation are [singleton duties](../concepts/seat-ownership.md#singleton-duties) now, along with the scheduler tick, the sandbox waiter, the seat-subscription walk and the retention sweeps — six `worker:{duty}` leases, each claimed per tick so a node that dies mid-duty hands it back by lapsing.
- *Unbounded table growth.* `webhook_deliveries`, `rate_limits`, `scheduled_runs`, `turn_completions` and `a2a_channels` all answer a short-horizon question and are written on every event that asks it. The migrations always said they were swept on a TTL; the sweep exists, behind the `maintenance` duty.

The one thing that is still per-process: `max_concurrent`. The concurrency
gate is per node, so an org's ceiling becomes N × the configured value.
Size it per node, not per company.

For the model underneath all of this — what a node is, what the fleet shares,
and where the constants come from — see
[Scaling Out](../concepts/scaling.md).

---

## Database

PostgreSQL via `asyncpg`. The TimescaleDB and pgvector extensions must be available in the server — the `timescale/timescaledb` image used by the bundled `docker-compose.yml` ships with both. All tables live in the same database:

- **`token_usage`** — per-agent cumulative token consumption (`agent_handle TEXT PK`, `tokens_used BIGINT`, `updated_at TIMESTAMPTZ`). The shared `run_tool_loop` (used by every phase of the [Turn Engine](../concepts/turn-engine.md) — Plan, Execute, Review, sub-agent) upserts into this table after every LLM completion that passes the in-memory budget check, providing durable audit totals. Persists across engine restarts; does **not** currently rehydrate the in-memory budget state on startup.
- **`agent_diary`** — pgvector-backed table for each agent's private observation log. Written by `reflect_and_persist` and the post-turn `PersistDecider`, which embed content on write. The `## Personal memory` prefetch reads it via hybrid candidate selection (vector top-K ∪ recency top-K, deduped, capped at 100) handed to an aux-LLM relevance filter. (Shared knowledge is **not** stored in the database — Confluence is searched live at query time; see [knowledge system](../concepts/knowledge-system.md).)
- **`episodes`** — TimescaleDB hypertable for one-row-per-completed-turn records, with both raw and LLM-compacted shapes. Drained by the `EpisodeLifecycleWorker`.
- **`synthesized_skills`** + **`synthesized_skill_versions`** — auto-drafted skills the agent can load via `use_skill`, plus their refinement history.
- **`counterparty_profiles`** — per-`(observer, subject, platform)` profiles built by the `CounterpartyProfiler`.
- **`agent_onboarding_markers`** — `mark_onboarded` bookkeeping (one row per agent, UPSERT-keyed).
- **`crewlet_events`** — TimescaleDB hypertable for the observability event store.
- **`chat_thread_follows`** — per-agent chat thread-follow state, keyed by backend (regular table; survives restarts).

The `crewlet_events` hypertable is created by the initial migration `001_initial.sql`. The full migration list is in `src/crewlet/db/migrations/`. Migrations are **forward-only**: each file is applied once and recorded by filename in `schema_migrations`, and there are no downgrade scripts. Downgrading the package below the schema it already migrated is not supported; restore a backup instead.

Everything else is either:

- **YAML config** — agent definitions, org structure
- **In-memory** — agent runtime state, execution tracker
- **External PM tool** — task state (Jira, Plane, GitLab/GitHub issues)
- **Apache Pulsar** — event routing (durable per-subscription backlog; optional time-based retention via a namespace policy)

---

## Observability

### TimescaleDB Event Store

Crewlet persists every engine event (LLM invocations, task lifecycle, agent states) to a **TimescaleDB hypertable** called `crewlet_events` in the same PostgreSQL instance that holds the per-agent diary vector store, the episodic store, and operational tables. One database for everything.

#### Setup

1. Start the stack via Docker Compose:

```bash
docker compose up -d  # postgres (with TimescaleDB + pgvector) starts with all services
```

The bundled compose image (`timescale/timescaledb:latest-pg18`) ships with TimescaleDB and pgvector preloaded. The engine's migration runner creates `crewlet_events` as a hypertable on first startup (see `src/crewlet/db/migrations/001_initial.sql`).

##### Managed Postgres services

The migration runs `CREATE EXTENSION IF NOT EXISTS timescaledb`, which requires either a superuser role or a pre-enabled extension. Managed services handle this differently:

- **AWS RDS / Aurora Postgres**: add `timescaledb` to `shared_preload_libraries` via the parameter group and grant the invoking role `rds_superuser` so the migration can run `CREATE EXTENSION timescaledb`. TimescaleDB needs the preload entry because it registers planner hooks at server startup; `pgvector` is a regular extension that only needs `CREATE EXTENSION vector` (the initial migration handles that for you).
- **Google Cloud SQL for PostgreSQL**: enable both extensions via `cloudsql.enable_pgvector` and `cloudsql.enable_timescaledb` flags before running the migration.
- **Timescale Cloud**: TimescaleDB is pre-enabled. You still need to install `pgvector` from the extensions menu.
- **Supabase**: both `timescaledb` and `vector` are available from the Database → Extensions panel.
- **Self-hosted vanilla Postgres**: install the TimescaleDB apt/yum package, then either run the migration as a superuser or pre-run `CREATE EXTENSION timescaledb;` out of band.

`IF NOT EXISTS` makes the migration a no-op once the extension is present, so subsequent restarts don't need elevated privileges.

2. The event store uses the same database as everything else — no extra config is required beyond `providers.database.dsn`:

```yaml
providers:
  database:
    dsn: "postgresql://crewlet:crewlet@localhost:5432/crewlet"
```

The engine registers an `EventStoreWriter` as a **publish listener** on the EventQueue. Every event is written directly to PostgreSQL at publish time — no queue round-trip, no consumer groups, no race conditions. Events land in the `crewlet_events` hypertable with dedicated columns for the common filterable dimensions (`event_type`, `source`, `category`, `agent_id`, `agent_role`, `task_id`, `channel_id`, `sender`, `trace_id`) plus a `tags JSONB` column for everything else.

#### What Gets Stored

| Category | Event Types | Use Case |
|----------|-------------|----------|
| `lifecycle` | `agent_spawned`, `agent_terminated`, `org_started/stopped`, `role_updated` | Agent uptime tracking |
| `task` | `task_created/started/completed/failed/delegated` | Task throughput metrics |
| `system` | `agent_turn_completed`, `agent_phase_completed`, `budget_exhausted`, `turn.guard_breach`, `llm_unavailable` | LLM invocation tracking, per-stage token usage, AFK dashboard signals |
| `communication` | `message_sent` | Communication volume |
| `decision` | `decision_requested/resolved`, `contribution_requested/received` | Decision pipeline |
| `knowledge` | `document_created/updated` | Knowledge activity |
| `notification` | `external_notification`, `notification_skipped` | Notification delivery and drop reasons |

Each event includes a JSON payload with full details (token counts, model used, tool executions, trace context, etc.). `agent_turn_progress` events are published on the EventQueue for real-time subscribers but not persisted to the store. Two different events carry the durable state, at two different levels of aggregation: `agent_phase_completed` is the per-phase record — it is what replaces the in-flight row on the dashboard when a phase ends — while `agent_turn_completed` is the whole-turn aggregate the rollups read. Each progress event carries the turn coordinates (`turn_id`, `phase`, `iteration`, `role`) plus the accumulated response — reasoning included — and tool executions so far; the dashboard's agent page uses them to render the in-flight LLM call live, then swaps in that persisted per-phase record when the phase finishes. A round publishes two of them: one as soon as the model answers, one once that round's tools return (see [Turn Engine § What streams during a turn](../concepts/turn-engine.md#what-streams-during-a-turn)).

The dashboard's **Tokens** view rolls these `agent_phase_completed` rows up by phase / model / auxiliary worker / agent / turn so operators can see *where* the LLM bill is going (Plan vs Execute vs Review vs Auxiliary, default vs `llm_auxiliary` model, which learning worker, which agent, which turn). The rollup is served by `GET /tokens/breakdown` — see [API Endpoints](../reference/api-endpoints.md#token-spend-breakdown) for the response shape and query parameters.

#### Querying Events

The included **pgweb** web interface at `http://localhost:8150` gives you a
schema browser, a SQL query editor, and result tables — it's auto-connected to
the local PostgreSQL with the credentials from `.env`.  The standard `psql`
CLI works too.

Example queries against the `crewlet_events` hypertable:

```sql
-- Recent LLM invocations by agent
SELECT event_time, agent_role, summary, payload
FROM crewlet_events
WHERE event_type = 'agent_turn_completed'
ORDER BY event_time DESC
LIMIT 50;

-- Task throughput by agent
SELECT agent_role, event_type, count(*) AS cnt
FROM crewlet_events
WHERE event_type IN ('task_started', 'task_completed', 'task_failed')
  AND event_time >= now() - INTERVAL '7 days'
GROUP BY agent_role, event_type;

-- Token usage from payload
SELECT event_time, agent_role, payload
FROM crewlet_events
WHERE event_type = 'agent_turn_completed'
  AND event_time >= now() - INTERVAL '24 hours'
ORDER BY event_time DESC;
```

You can also connect **Grafana** to PostgreSQL using the built-in PostgreSQL
data source for persistent dashboards — TimescaleDB is query-compatible with
plain SQL.

#### Architecture

```mermaid
flowchart TD
    P["EventQueue.publish()"]
    PULSAR["Apache Pulsar<br/>(event routing to agents)"]
    W["EventStoreWriter<br/>(publish listener, categorizes, extracts tags)"]
    TS["TimescaleDBEventStore<br/>(asyncpg INSERT/SELECT against<br/>the crewlet_events hypertable)"]
    G["pgweb / Grafana"]
    P --> PULSAR
    P --> W --> TS --> G
```

The writer is registered as a publish listener via `event_queue.add_publish_listener(writer.on_publish)`. It runs inline during every `publish()` call, so events are written to PostgreSQL at the same time they're published to Pulsar — no consumer groups, no subscription timing issues, no message loss. If the database is temporarily unavailable, the write error is logged and the event still reaches Pulsar normally.

#### In-Memory Satellite

Even with TimescaleDB configured, a `MemoryEventStore` (capped at 1000 events) runs as a second leg of a `CompositeEventStore`. This gives queries an instant-read path for events that haven't yet round-tripped through PostgreSQL — useful for the dashboard when a batch of events was just published.

### Tracing

Crewlet uses **OpenTelemetry** for distributed tracing. Every event carries W3C Trace Context fields (`trace_id`, `span_id`, `parent_span_id`) that propagate automatically through the system.

#### How Traces Flow

```mermaid
flowchart TD
    A["Webhook arrives (OTel span created)"]
    B["NotificationService (restores trace context)"]
    C["ExternalNotification → agent inbox (trace preserved)"]
    D["Executor (creates child span 'agent.turn')"]
    E["TaskStarted, TaskCompleted, etc."]
    F["Tool calls (messages, decisions)"]
    G["AgentTurnCompleted"]
    A --> B --> C --> D
    D --> E
    D --> F
    D --> G
```

Each Event's `trace_id`/`span_id` fields are auto-populated from the active OTel span at creation time. When events cross async boundaries (EventQueue → handler), the receiving component calls `restore_context()` to reconnect the trace.

#### Dashboard Trace View

The dashboard (`/dashboard` → Traces) groups events by `trace_id` into collapsible trace trees:

- Root event (e.g., webhook) shown as the trace header
- Child events nested underneath with connecting lines
- Click `inspect →` on LLM turn events to view the full prompt/response
- Notification skip reasons shown inline (e.g., "not following this thread")

#### OTLP Export

To export traces to Jaeger, Grafana Tempo, or any OTLP-compatible backend:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318/v1/traces
```

When this env var is set, the engine configures a `BatchSpanProcessor` with an `OTLPSpanExporter` at startup. Without it, spans are still created (for trace context propagation) but not exported.

#### Querying Traces in the Event Store

The `crewlet_events` hypertable stores `trace_id`, `span_id`, and `parent_span_id` as first-class columns, so trace queries are a direct column filter:

```sql
-- All events in a specific trace
SELECT event_time, event_type, source, summary, payload
FROM crewlet_events
WHERE trace_id = '<trace-id>'
ORDER BY event_time ASC;
```

The dashboard API also provides `GET /events/trace/{trace_id}` which returns all events in a trace ordered by timestamp.

### Debug Mode

Set `debug: true` in the Tier A bootstrap file (`config.yaml`) to enable DEBUG-level logging (or pass `--debug` on `crewlet run`):

```yaml
# config.yaml (Tier A)
debug: true
```

Or pass it directly:

```python
engine = Engine(organization=org, debug=True)
```

When debug mode is enabled, the `crewlet` root logger is set to `DEBUG`, propagating to all child loggers (`crewlet.engine`, `crewlet.mcp.client`, `crewlet.events.bus`, etc.).

### Per-Agent Token Tracking

The `ObservabilityManager` tracks token usage per-agent with full input/output breakdown:

```python
class TokenUsage(BaseModel):
    input_tokens: int = 0    # prompt tokens
    output_tokens: int = 0   # completion tokens
    total_tokens: int = 0    # input + output
    call_count: int = 0      # number of LLM calls
```

Query metrics at runtime:

```python
summary = engine.observability.get_summary()
# summary["agents"]["abc-123"]["tokens"]["input_tokens"]
# summary["engine"]["total_tokens"]["total_tokens"]
```

### Token Budgets

Set budgets at two levels:

- **Org-wide** — `token_budget` in the top-level YAML config
- **Per-agent** — `token_budget` on each Role definition

When exceeded, the shared tool-call loop emits a `BudgetExhausted` event, raises `RuntimeError` to stop the agent turn, and marks the task as failed. Budget checking is atomic — if the agent budget fails, org-level consumption is rolled back.

### Structured Logging

Every significant operation emits structured log entries:

```json
{
  "timestamp": "2026-03-12T10:30:00Z",
  "level": "info",
  "component": "agent.turn",
  "agent_id": "abc-123",
  "role": "Senior Engineer",
  "event": "turn_completed",
  "task_id": "def-456",
  "llm_input_tokens": 1200,
  "llm_output_tokens": 647,
  "duration_ms": 3200
}
```

### Hooks

```python
await engine.on_task_state_change(callback)  # task lifecycle → dashboards / alerting
await engine.on_agent_spawn(callback)        # agent_spawned events
```

Both subscribe the callback to the relevant event topics. For per-turn detail (LLM invocations, phase completions), subscribe to the event stream directly — see [Extensions](extensions.md).

---

## Security Boundaries

- **Scope isolation** — agents can only access knowledge within their permitted scopes
- **Tool availability** — all registered tools available; per-role MCP tools carry role-specific credentials
- **Communication permissions** — agents can only post to channels they're members of
- **Manager handoffs** — agents identify their manager from their identity prompt and reach them through the colleague-surface tools (Slack/Jira/Confluence/A2A); engine-detected failures surface to the operator dashboard as `afk` state
- **LLM sandboxing** — tool execution results are validated before returning to the agent
