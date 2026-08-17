# Crewlet Node — Implementation Plan

**Status:** planned, not started. The companion analysis — why the current
engine is a singleton, the target architecture, and the adversarial review
that shaped it — is [`SCALING.md`](SCALING.md). This file is the *how*: the
phase-by-phase work breakdown, with file-level anchors, schema changes,
tests, and exit criteria.

**Audience:** Crewlet maintainers.

---

## Ground rules

These hold for every phase:

1. **Every phase ships independently and leaves `main` releasable.** Phases
   1–4 are pure wins for today's single-node topology; nothing multi-node is
   *required* until phase 5.
2. **Single-node stays the default and stays fully supported — forever.** One
   node holding every lease is the design's degenerate case
   ([`SCALING.md` § Honest limits](SCALING.md#honest-limits)); the quickstart
   never changes. Multi-node is opt-in until the phase-5 chaos suite is green.
3. **Memory backends are semantic twins.** Every new primitive (leases,
   claims, owner-only subscription, per-topic pause) lands in the memory
   backend with the same semantics, and the same contract-test suite runs
   against both. A no-PG / no-broker configuration must refuse multi-node
   operation loudly, not silently degrade (the existing
   `MemoryPendingSandboxRunStore` fallback at `engine.py:4715` is the trap
   this rule exists to prevent).
4. **Docs and tests ship in the same change as the code** (per `CLAUDE.md`);
   the per-phase docs targets are listed below.
5. **Breaking changes are allowed** and are called out in PR titles (the
   release notes). We are pre-1.0 (`0.1.0`); each phase bumps the minor
   version. CLI changes get a deprecation window of one minor release.
6. **Two review checkpoints are gates, not suggestions** (see
   [`SCALING.md` § Open questions](SCALING.md#open-questions-before-implementation),
   item 7): a second adversarial pass on the config-gate semantics **before
   phase 4 implementation**, and on the handoff interleavings (deferral ×
   coalescing × turn-claim) **before phase 5 implementation**.

---

## Phase 1 — Foundations

Everything later depends on these four primitives. No behavior change for
operators except the migrator.

### 1.1 Node identity

- Tier A field `node.id` on `BootstrapConfig` (`config.py:440`), env fallback
  `CREWLET_NODE_ID`, default `"node-0"` for single-node installs. Must be
  stable across restarts (deployment-provided: pod name, StatefulSet ordinal,
  or the explicit field) — never self-generated (`uuid4` per boot reproduces
  the `hook-{id(callback)}` orphan-subscription leak, `engine.py:3453`).
- Bind `node=` into structlog context at boot; stamp it on
  `ConfigRevisionApplied` and health payloads.
- Docs: `docs/getting-started/configuration.md`,
  `docs/reference/environment-variables.md`.

### 1.2 Held-connection database API

- `Database.acquire()` (context-managed raw connection) and
  `Database.transaction()` on `db/client.py` (today: only per-statement
  `execute`/`fetchrow`/`fetchval`, `db/client.py:54-68`). Without this, every
  advisory-lock idea is a silent no-op — asyncpg's pool reset runs
  `pg_advisory_unlock_all()` on connection release — and no migration file
  can be transactional.
- Migrate `db/company_config.py`'s direct `pool.acquire()` onto the new API.
- Tests: `tests/test_db/` — prove an advisory lock held across statements
  survives, and that the per-statement helpers still round-trip.

### 1.3 `crewlet.db.leases`

- Migration `017_leases.sql`:

  ```sql
  CREATE TABLE leases (
      resource   text PRIMARY KEY,       -- 'seat:{handle}' | 'worker:{duty}' | 'node:{id}'
      owner      text NOT NULL,          -- node id
      epoch      bigint NOT NULL,        -- increments on EVERY ownership change
      expires_at timestamptz NOT NULL,
      preferred  text NOT NULL DEFAULT '',
      protocol   int NOT NULL DEFAULT 1  -- mixed-version fleet gate (phase 5)
  );
  ```

- Module `crewlet/db/leases.py`, modelled on
  `OnboardingMarkerStore.try_claim_pass` (`learning/onboarding_markers.py:136`)
  with its two defects fixed: `release(resource, owner)` is owner-predicated,
  and `renew(resource, owner, ttl)` exists. `try_acquire` returns the new
  epoch (the fencing token) or `None`.
- Heartbeat runner: a dedicated OS thread with its own connection and event
  loop, self-fencing (renewal failure or observed main-loop stall ⇒ the node
  kills its own claimed work rather than trusting a lease it cannot prove).
- Tests: contract suite against PG and a memory twin; a two-owner race test;
  an expiry-takeover test asserting the epoch increments.

### 1.4 Migrator rework (fixes live bugs #1 and #2)

- New CLI: `crewlet migrate <config>` — the explicit, recommended step.
- `migrate()` (`db/migrator.py:49`) takes `pg_advisory_lock` on a **held
  connection** (the one place session-scoped release-on-disconnect is the
  *right* property — a SIGKILLed migrator must not wedge future boots), and
  applies each file inside one transaction.
- **Deferral replaces the 1536 fallback** (`cli.py:796`): the pgvector-width
  files (`002_episodes.sql`, `007_agent_diary.sql`) run **only when a config
  declares the embedding width**, and the run *stops* rather than skipping
  ahead (later migrations touch objects they create). Applies to every path
  that can supply a width — active revision, `--company`, `config import` —
  so a company with no `providers.embeddings` block defers too instead of
  baking 1536. Both processes may still auto-migrate; the lock makes that
  safe, and nothing can guess a width any more. `Engine.apply_config`
  completes the deferred tail once a config declares a width, so the
  unconfigured-bootstrap flow ends with a full schema and no restart.
- Repair migration `018_learning_health_repair.sql`: re-issues the canonical
  `learning_health` view definition, healing databases where the
  `005`/`009` `CREATE OR REPLACE VIEW` race applied them out of order.
- Engine/API boot verifies schema version and fails with a clear "run
  `crewlet migrate`" message; `crewlet run` keeps a convenience auto-migrate
  (now lock-protected) for the single-node dev flow.
- Fix `_split_sql`'s silent hazards (`db/migrator.py:40-46`): reject
  dollar-quoting and `$n` literals loudly until the splitter supports them.
- Tests: N processes racing `migrate()` (green = one applies, rest wait);
  API-first cold boot followed by a 3072-dim engine import succeeds.
- Docs: `docs/guides/deployment.md` (migrate step),
  `docs/reference/cli.md`.

**Exit criteria:** concurrent-boot fuzz green; the pgvector cold-start test
green; leases contract suite green on PG + memory.

---

## Phase 2 — Kill the wiring fork (fixes live bug #3) — DONE

One API wiring — the broadcast one — regardless of where the API runs.
This is the phase that permanently retires the bug class behind all three
live bugs.

Deviations from the plan as written, both discovered in implementation:

- The `NodeStatus` provider became `NodeRuntime` and also carries the live
  MCP tool surface, since `_build_tools_data()` was the fourth thing only
  a co-located engine could answer. Still one injection point.
- An engine with no config store (a programmatic embed) primes the app
  through the *same* `_apply_payload_to_app` the refresh path uses, from
  its in-memory config. Without it, dropping the parameters would have
  left that deployment with no webhook secrets at all — one derivation,
  two sources, rather than two derivations.

### 2.1 Rewire the embedded API to the standalone shape

- The engine stops passing `engine=self` and the boot-time
  `agent_roles`/`org_data`/`tools_data` snapshots into `create_app`
  (`engine.py:2713-2740`); the embedded app gets exactly what `cmd_api`
  builds (`cli.py:1587-1602`): queue, event store, database, stream,
  bootstrap, config store — then `attach_config_refresh` +
  `subscribe_stream("crewlet.events.>", stream.ingest)`.
- The `add_publish_listener` feed into `StreamService` (`engine.py:2710`)
  is deleted. (The event-store writer keeps its publish listener — its
  `ON CONFLICT (event_time, event_id) DO NOTHING` write,
  `timescaledb/repository.py:89`, is idempotent and publisher-side
  persistence is correct with N writers.)
- `/health` splits: `/health` = process liveness (identical shape both
  modes, stamped with `node.id`); `/ready` = new readiness endpoint (used by
  the LB and by drain, phase 5). Engine-specific fields (`in_flight`,
  `shutting_down`) move behind a narrow `NodeStatus` provider protocol that
  the embedded mode registers — one injection point instead of four
  embedded-only `create_app` parameters. Per the review's warning: if the
  embedded-only parameters survive "for compatibility", the merge's entire
  payoff is forfeited — they must be gone, not deprecated.

### 2.2 Stateless OTLP tokens

- Per-run sandbox OTLP tokens become HMAC-signed values (key derived from
  the Tier A keyring), verified statelessly by any process — replacing the
  per-process dict (`sandbox/otel.py:40`) and fixing the standalone API's
  unconditional 503 (`api/routes/webhooks.py:550`). The receiver is
  constructed in the shared `create_app` path.

### 2.3 Webhooks fail closed

- The unconfigured 200-drop (`api/routes/webhooks.py:152`) becomes a 503 so
  upstreams retry; HMAC-verification failure stays 401. Documented per
  source (Slack retries on 5xx and disables after sustained failure — which
  is the *correct* pressure toward fixing the outage, not a reason to lie
  with a 200).

### 2.4 Full-surface auth + CORS

- Bearer auth (already implemented for `/config/*`) extends to every REST
  and WS route; webhooks authenticate by HMAC, OTLP by signed token. CORS
  default tightens to same-origin. The dashboard gets a token entry flow.
- Escape hatch: explicit `api.auth.allow_anonymous_read: true` for
  firewalled dev setups — opt-in, loudly logged.
- After this ships: revisit the frank auth-gap paragraph in `SCALING.md`,
  and note the posture change in `SECURITY.md`.

**Exit criteria:** a parity test asserting embedded and standalone construct
byte-identical app wiring (introspect `app.state` and route table); OTLP
export lands through the standalone API; webhook fail-closed tests; auth
denial tests on every route.

---

## Phase 3 — Shared counters to PG — DONE

Everything in-memory that a lock cannot fix ([`SCALING.md` § class 4](SCALING.md#4-shared-mutable-counters)).

Deviation from the plan as written: budgets did **not** become one table
of caps + usage. A cap is config — every process derives the same number
from the same revision — so only *usage* is shared. Caps stay in memory,
which also kept the setters synchronous and the engine's config-apply
path unchanged. `BudgetManager` keeps a local usage *mirror* for the
advisory readers (sub-agent slice sizing, the reflect engine's
exhausted-check); enforcement never reads it.

### 3.1 Budgets

- Migration `019_budgets.sql`: one row per scope (`org`, `agent:{handle}`),
  consumed by a single conditional-`UPDATE` CTE covering org + agent
  atomically (replacing the in-memory check-then-rollback,
  `concurrency.py:188-212`). `BudgetManager` becomes a façade over the table.
- **Deliberate behavior change:** budgets become durable across restarts
  (today they reset per engine run — meaningless in a fleet). New
  `crewlet budgets reset` CLI; docs updated
  (`docs/getting-started/quickstart.md` § token budgets,
  `docs/concepts/configuration.md`).

### 3.2 Webhook delivery dedupe

- Migration `020_webhook_deliveries.sql`: `INSERT … ON CONFLICT DO NOTHING`
  claims keyed on provider delivery ids (`X-GitHub-Delivery`,
  `X-Gitlab-Event-UUID`, Slack retry headers + event id, Jira/Confluence/
  Plane delivery ids). Replaces the four in-memory rings
  (`transports/slack.py:175` et al.) and gives GitHub/GitLab dedupe for the
  first time (`notifications/service.py:631` has none today). TTL sweep via
  the phase-5 worker host (until then, an interval task).

### 3.3 Credential cooldowns

- `CredentialPool` cooldowns move from `time.monotonic()`
  (`providers/credential.py:82-89`) to wall-clock timestamps in PG, so a 429
  cools a key fleet-wide and replicas stop diverging onto different models
  through the fallback chain.

### 3.4 Rate limits

- `notification_rate_limit` (`config.py:2344`) becomes a PG token bucket;
  volumes are low, contention is negligible. `ConcurrencyController` stays
  per-node by design — its config value is re-documented as per-node.

**Exit criteria:** two-process contract tests proving single-spend /
single-claim on each counter, against PG and the memory twins.

---

## Phase 4 — Control plane

**Gate: second adversarial review of the gate semantics first** (partial
apply — a revision succeeding on some nodes and failing on others).

### 4.1 Activation epochs

- Migration `021_config_activation.sql`: a monotonically increasing
  `activation_seq` that bumps on **every** activation — including
  re-activation of an unchanged revision, which is the documented
  secret-rotation gesture (`engine.py:594-598`). Epoch ≠ revision id, or
  rotation never propagates. Plus `config_apply_status(node_id, epoch,
  status, error)` fed by each node's apply outcome.

### 4.2 Delivery: authoritative poll + Reader fast path

- Each node runs a ~15 s reconcile poll comparing its applied epoch to the
  store's — the authoritative mechanism — plus a non-durable Pulsar
  **Reader** on the config topic for latency (no broker-side state to
  orphan on crash, unlike per-node durable subscriptions). The
  competing-consumer groups `engine-config` (`engine.py:1144`) and
  `api-config` (`api/config_refresh.py:361`) are both retired — the review
  was explicit that fixing only one of them re-creates the bug.

### 4.3 Fail-closed, gated on fleet-applied

- A node whose applied epoch lags refuses to *start new turns* and reports
  divergence; the comparison target is the latest epoch that **successfully
  applied** (from `config_apply_status`), not the raw store pointer — a
  revision that fails apply on all nodes must leave the fleet serving the
  prior config (today's rollback behavior, `engine.py:706-716`), not 503ing
  everything.
- Webhook ingress fails closed only on what it depends on: HMAC verification
  checks current **and prior** secret during a bounded overlap window;
  verified events are enqueued regardless of config staleness (a publish is
  epoch-independent).

### 4.4 Non-tearing applies

- Turns pin an immutable config snapshot (org + resolved tool surface) in
  `TurnContext` at turn start; `apply_config`'s restart-required subsystems
  (MCP servers, transports) apply behind a per-seat drain; applies are
  jittered across nodes; a node restarts per-role MCP servers only for seats
  it owns (phase 5 tightens this to lease ownership).
- MCP restart diffing moves from raw `${VAR}` spec comparison
  (`engine.py:3917-3926`) to a hash of the **resolved** env/headers, so
  rotation actually restarts children that captured the old secret at spawn
  (`mcp/client.py:73`).

### 4.5 Secret snapshot

- `refresh_secret_snapshot` triggers on epoch change (not revision change);
  maximum staleness = poll interval + apply time (~seconds), acceptable for
  revocation.

**Exit criteria:** chaos tests — node offline during activation converges via
poll within 15 s; bad-revision test keeps the fleet on the prior config with
divergence visible; rotation test proves an MCP child restarts when its
resolved credential changes; mid-apply turn test proves a running turn sees
a consistent snapshot end-to-end.

---

## Phase 5 — Seat host

The largest phase.

**Entry gates:** (a) the broker measurement harness has produced numbers for
session-death timing, cursor continuity on owner handoff, and prefetch
behavior — and `receiver_queue_size` is set explicitly per consumer (today it
silently defaults to 1000); (b) the mixed-version design is decided — the
lease table's `protocol` column gates claims (a node refuses to claim while
an older-protocol node holds any lease), with strictly additive schemas as
the default evolution rule; (c) the second adversarial review of the handoff
interleavings has run.

### 5.1 Seat leases

- One lease per agent seat (`seat:{handle}`), greedy claim up to
  `ceil(seats/N)`, claim-rate limit (prevents the MCP spawn storm on
  takeover), `preferred` stickiness across deploys.

### 5.2 Owner-only subscriptions

- The seat's inbox consumer (`engine.py:2120-2127`) is created on lease
  acquire and closed on release — same durable subscription name
  (`agent-{handle}`), so the cursor survives handoff; type stays **Shared**
  (owner-only membership; Exclusive was rejected — DLQ policy inert, wedged
  zombies hold the slot; see [`SCALING.md` § rejected](SCALING.md#rejected-mechanisms-and-alternatives)).
- Broker-side `unsubscribe` (the destructive kind, `engine.py:5116`) happens
  **only** on role decommission, coordinated via the control plane so every
  node first releases/ignores the seat.

### 5.3 The takeover pipeline (boot = takeover)

- Acquire lease → scan `pending_sandbox_run` for the seat → reconstruct
  pause state from rows (`sandbox_id` non-empty ⇔ paused) and
  `AWAITING_SANDBOX` → attach the inbox consumer **last**.
  `SandboxCoordinator.recover()` (`coordinator.py:498`) is rewritten to this
  owned-seats-only shape.

### 5.4 Turn claims

- Migration `022_turn_claims.sql` — the state machine from
  [`SCALING.md` § turn claims](SCALING.md#turn-claims--a-state-machine-not-an-insert):
  short-circuit only on `completed`; dead-epoch `in_progress` superseded;
  claim deleted in the same `finally` that defers the delivery; **sandbox
  resumes never take one** (the pending-row claim is already the
  at-most-once flip).
- Lease-blocked deliveries defer by **republish-then-ack** (the identity-
  preserving requeue machinery at `queue/pulsar.py:930`), never NAK
  (3 × 1 s dead-letters healthy events, `pulsar.py:93-101`).

### 5.5 Epoch fencing on writes

- `pending_sandbox_run` mutations, turn-claim transitions, and budget
  consumption carry `WHERE owner_epoch = $current`. The waiter/reaper's
  unguarded writes (`waiter.py:159`; `set_status` without a status
  precondition; `attach_sandbox`/`release_box`,
  `pending_store.py:367`) gain compare-and-set preconditions.

### 5.6 Owner-routed sandbox control

- Primary variant: **the DB claim** — the waiter (a worker-host singleton)
  writes `completed` onto the pending row; each seat host claims completions
  for its own seats (`WHERE seat IN <my leases> AND owner_epoch = $mine`)
  on its poll tick. Chosen over the per-seat control topic because the
  waiter already *is* a poll loop, it adds no topics/consumers, and latency
  is bounded by a tick that already exists. The engine-wide
  `sandbox-coordinator` Shared group (`coordinator.py:58-60`) is retired.
- `_dispatch_resume_execute` **raises** when the agent is not local
  (`coordinator.py:433-447` returns today), so any misrouted claim reverts
  via the existing un-claim path instead of settling the run to `done`.

### 5.7 In-turn fencing

- The tool loop (`agent/llm_loop.py`) checks local lease validity before
  each LLM round and before each side-effect-bearing tool call (using the
  capability classification in `tools/capabilities.py`), bounding the zombie
  window to one round.

### 5.8 Drain and readiness

- `/ready` flips off → stop claiming → **release each seat's lease as it
  goes idle** (peers pick seats up one by one); a seat in
  `AWAITING_SANDBOX` releases immediately (its state is the PG row). Node
  death is the same flow via TTL expiry. Integrates with the existing
  signal-handling design (`engine.py:2803+`).

### 5.9 Workers behind singleton leases

- Scheduler tick (epoch-gated: skip while the node's applied epoch lags —
  fire identity is org-derived, `schedule/scheduler.py:184`),
  `SkillClusteringScheduler` (`learning/skill_scheduler.py:117`),
  `SkillCuratorWorker`, the sandbox waiter, the tool-skills boot walk, and
  the dedupe-TTL sweep each claim `worker:{duty}`.

**Exit criteria (the chaos suite):** `kill -9` the owner mid-turn → takeover
within TTL + attach, trigger redelivered, claim superseded, no lost trigger,
duplicates only within the documented one-round window; node death during a
detached sandbox run → resume on the new owner with the suspended
conversation intact; 3-node rolling deploy under continuous webhook load →
zero lost events; the memory twins pass the identical suite.

---

## Phase 6 — A2A on the durable queue

- The opening brief rides the (already durable) inbox wake
  (`a2a/service.py:105`); replies ride a durable response subject. The
  in-memory queue survives only as a same-node fast-path optimization behind
  the same interface — never a correctness dependency.
- Migration `023_a2a_channels.sql`: channel bookkeeping (participants,
  requester, state) replaces the process-local `A2AService._channels`
  (`a2a/service.py:40`), so send/close authorization works cross-node; TTL
  close for abandoned channels.
- `_handle_a2a` (`engine.py:2448`) stops silently swallowing unknown
  channels — with state in PG, "unknown" now genuinely means closed/expired
  and is surfaced to the requester as a tool failure.

**Exit criteria:** cross-node `a2a_ask` integration test; a
satellite-profile test (participant reachable only via broker + PG); leak
test proving channel close tears down all resources.

---

## Phase 7 — Placement, satellites, CLI convergence

- Tier A `node.roles: [ingress, seats, workers]` (default: all).
- `role.placement` on seats: pin to a node id or a label selector;
  `node.labels` on the node. Lease claims filter on placement. Validation:
  sandbox-enabled seats may not be satellite-pinned until the
  waiter-reachability question is resolved
  ([`SCALING.md` § open questions](SCALING.md#open-questions-before-implementation),
  item 4) — the validator enforces the current answer either way.
- CLI: `crewlet run` *is* the node. `crewlet run api` becomes a deprecated
  alias for `crewlet run --roles ingress` (one minor release), then is
  removed.
- Fleet observability: a dashboard view over the lease table + per-node
  apply status (per-node in-flight, seat ownership, epoch divergence).
- Docs, the big flip: `docs/guides/deployment.md`'s
  [Replica count](docs/guides/deployment.md#replica-count) section is
  replaced with fleet guidance; `docs/concepts/overview.md`'s single-instance
  statement is updated; a new `docs/guides/fleet.md` covers topology,
  satellites, drain, and upgrade order; `SCALING.md` is updated to
  "implemented" status with the plan's deviations recorded.

**Exit criteria:** a 3-node + 1-satellite example runs the Nimbus company
end-to-end in the integration harness; the deployment docs describe (and
only describe) topologies the chaos suite covers.

---

## Cross-cutting workstreams

### Broker measurement harness (before phase 5)

A `tests/test_integration/`-style suite against the compose broker
measuring: consumer-session death timing after `SIGKILL` (broker keepalive),
cursor continuity when the sole Shared-group member detaches and another
attaches, prefetch inventory in a killed consumer (and the effect of an
explicit `receiver_queue_size`), Reader catch-up semantics, and
republish-then-ack latency under load. Output: the constants file
(`lease TTL`, heartbeat interval, claim-rate limit) with measured
justifications, per `CLAUDE.md`'s tuning-knob rule.

### Contract tests (every phase)

One suite per primitive (leases, claims, budgets, dedupe, owner-only
subscription, pause, A2A), parameterized over the real backend and its
memory twin. A twin that cannot express a semantic is a failing test, not a
skip.

### Release mapping

| Phase | Version | Headline (the PR-title / release-note view) |
|---|---|---|
| 1 | 0.2.0 | `crewlet migrate`, locked migrations, pgvector width fix |
| 2 | 0.3.0 | One API wiring, full-surface auth, webhooks fail closed, OTLP fix |
| 3 | 0.4.0 | Durable budgets + fleet-safe dedupe and cooldowns |
| 4 | 0.5.0 | Config activation epochs + non-tearing applies |
| 5 | 0.6.0 | Seat leases: multi-node engine (opt-in) |
| 6 | 0.7.0 | Cross-node A2A |
| 7 | 0.8.0 | Fleet topology GA: roles, satellites, CLI convergence |
