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

**Gate outcome: the design as written FAILED review** (one lens
`redesign-needed`, two `implement-with-changes`). Three independent lenses
found the same fatal flaw, plus three more. The revised design below is what
gets built; the original is kept only where the gate endorsed it.

**Status: SHIPPED.**

What landed, with the deviations:

- Migration `023_config_plane.sql` + `db/config_plane.py` — the epoch log,
  per-node apply status, `decide_posture`, and a memory twin.
- The activation append runs **inside `CompanyConfigStore`'s own
  transaction** (`ACTIVATION_INSERT_SQL`, one statement shared by both
  activation paths), not at the API route. A crash between the `is_active`
  flip and the epoch append would otherwise leave a fleet converged on a
  revision nobody asked for. It also covers `crewlet config import`, which
  a route-level append would have missed entirely.
- **Both** competing groups retired. `revision_activated` survives as a
  broadcast **nudge** on `subscribe_stream`; the poll is authoritative
  because an ephemeral stream consumer starts at the latest message.
- The API's cached projection became `ConfigStateRefresher` — one tick
  (`refresh_if_changed`) plus an optional loop. A merged node passes
  `poll=False` and the engine drives the tick from its own reconcile loop,
  so one process polls the pointer once rather than twice.
- **Deviation from 4.C:** no new `pause_subscription(topic, group)`. The
  existing `pause_topic` gained a `reason` instead, so the config shed and
  the sandbox busy gate hold the same topics without either releasing the
  other's hold. Reversible, no broker-side subscription is touched, and it
  reuses machinery both callers already needed.
- The engine's control plane falls back to `MemoryConfigPlaneStore` without
  a database — the correct plane for that shape rather than a stub, since
  without a shared database there is also no shared config store and so
  exactly one process.

- 4.E/4.F landed together, because they only make sense together. The
  pin (`agent/turn_pin.py`) is a `ContextVar` read *through the existing
  accessors* — `TurnEngineSettings.get()`, a new `TurnEngine._llm_providers`
  property, `_role_mcp_for()`, and `AgentInstance.definition` — so no read
  site changed and sub-agent tasks inherit it for free. `live()` /
  `live_definition` are the escape hatches the apply path itself uses.
  The per-seat counter drains in `_apply_org_diff`, `_respawn_role_mcp`
  and `_decommission_role_live`; the cap is `SEAT_DRAIN_TIMEOUT_SECONDS`
  = 10 s, justified at its definition as the tail of one LLM round.

**Incidental bug fixed:** `CompanyConfigStore.activate()` deactivated the
current revision and then silently matched zero rows when the target
revision did not exist — leaving the company with **no** active revision,
which reads to every consumer as "unconfigured". It now raises inside the
transaction, so the deactivate rolls back with it.

**Both live bugs the gate found are fixed.** `ToolRegistry.unregister`
landed with the rotation fix in the prior commit. `_rollback` became
**async** and now routes the transport restore through
`_apply_notification_transports_live` — the same machinery the apply
uses — so a failed apply stops the failed revision's transports and
*restarts* the snapshot's, instead of reinstalling objects it had already
stopped and leaving the node silently deaf while reporting a healthy
epoch.

Rollback still does not respawn per-role MCP children: re-running the
spawn sequence for every role inside an already-failing apply trades one
failure for a longer, less predictable one. That residue is exactly what
`degraded` records, and why such a node fails readiness.

### What the gate endorsed, unchanged

- **4.1 activation epoch, not revision id.** Confirmed necessary: the
  documented rotation gesture re-activates an *unchanged* revision, so a
  reconcile keyed on revision id could never propagate it.
- **Retiring BOTH competing groups** (`engine-config`, `api-config`).
  Non-negotiable, and fixing only one leaves the two processes with
  different rotation semantics — the current bug in miniature.
- **Webhook rule**: verify HMAC, then enqueue regardless of config
  staleness. Already matches the code's ordering.
- **Not pinning the `PromptSkillRegistry`.** Its lock-free read contract and
  mid-turn gating are deliberate; "completing" the pin would silently
  disable the required-skill guard.

### 4.A — The rotation path (fatal; also a LIVE bug)

`apply_config` early-outs on `old.model_dump() == new.model_dump()`
(`engine.py:647`) — which is *by definition* what re-activating an unchanged
revision is. So the documented rotation gesture only swaps the secret
snapshot: MCP children keep the credential they captured at spawn, LLM
providers keep the revoked key, transports keep the old token. The API side
re-resolves unconditionally, so the same gesture works there and is inert on
the engine — which is why it went unnoticed.

Fix: a **resolution fingerprint** — a process-local *keyed* digest
(`blake2b` with a per-process random key) over the resolved value of every
`${VAR}` the payload references. Stored beside `_active_config`; the
early-out fires only when the raw payload **and** the fingerprint match.
When only the fingerprint moved, the credential-bearing subsystems rebuild.

The key is per-process and never persisted or logged: a bare SHA-256 of a
short credential in a log line or a DB row is offline-brute-forceable, which
would turn the fix into a leak.

### 4.B — The gate: target ≠ action (fatal)

"Lag behind the latest epoch applied *somewhere*" makes every **successful**
rollout a fleet-wide outage: the first node to apply advances the target, so
every peer is instantly "lagging" and sheds until its own apply finishes.
The faster one node is, the longer everyone else is down.

Revised:

- **Target** = the store's activation pointer, not any peer's status.
- **Action** on a confirmed lag, where "confirmed" means this node recorded
  `error` for that epoch *or* the lag persisted beyond `k` poll intervals
  (k=3): peers healthy → **shed**; nobody applied it anywhere → **keep
  serving** the prior epoch and raise divergence loudly; nobody has reported
  yet → **wait**, never shed.
- A node that has never applied anything (fresh/unconfigured) is a distinct
  state from one that applied N and failed N+1.
- Retry is **bounded**; a node that exhausts it goes `stuck` and fails
  `/ready` rather than re-applying — and restarting MCP servers — every 15s
  forever.

### 4.C — The gate's seam is the QUEUE, not the turn (fatal)

Gating "new turns" is too late twice over:

- A stale node still **consumes** inbound messages off the competing
  `notification-svc-inbound` group. Slack HMAC verification happens
  *consume-side* against that node's cached secret, and failure is a skip +
  ack — so a rotated signing secret means the stale node silently eats half
  the fleet's inbound messages.
- Gating `run_turn` **permanently wedges** a seat whose sandbox run just
  completed: the pending row is already flipped to `resumed`, the box
  collected, the agent `AWAITING_SANDBOX` with its inbox paused, and nothing
  reaps a `resumed` row in-process.

Revised: gate at **trigger admission** (inbox / notification / task-assigned
handlers and the scheduler tick), refusing by **republish-then-ack**, never
NAK (3 × 1 s dead-letters a healthy event). Sandbox-driven turns — anything
with `resume_state`, plus the clarification post — **bypass the gate
unconditionally**: they are the tail of a turn this node already started, and
refusing them destroys durable state rather than deferring it.

This needs a new reversible `pause_subscription(topic, group)` on
`EventQueue`: `pause_delivery` is process-wide and shutdown-only, and
`unsubscribe` deletes the *broker-side* subscription and drops the whole
group's backlog.

### 4.D — Apply honesty (serious)

`_rollback` is synchronous and cannot respawn MCP subprocesses; it also
reinstalls **stopped** transport objects. So a node that fails an apply after
the restart-required phase reports the prior epoch as fine while its tool
surface is amputated and its inbound path is dead.

Fix: a third outcome, **`degraded`** — recorded in `config_apply_status` and
treated by the gate as *not* converged — for any failure after a
restart-required subsystem was mutated.

### 4.E — Per-seat drain belongs to phase 5

There is no per-seat in-flight registry, and draining on `AgentState` is
actively wrong: a seat parked on a detached sandbox stays `AWAITING_SANDBOX`
for the whole run plus up to a 30-minute clarification pause, so one agent's
pending question would block the apply and gate the entire node.

Phase 4 ships a per-seat in-flight **counter** (incremented in `run_turn`,
decremented in its existing `finally`) and drains on that with a hard 10s
cap. "Only for seats it owns" moves verbatim to phase 5.1, where leases
exist.

### 4.F — Turn config pinning, scoped by what is actually pinnable

Pin: the `TurnEngineConfig` snapshot (~18 accessors re-read it *per access*,
so Plan can run under one round cap and Execute under another), the LLM
provider maps (mutated in place by `clear()`+`update()`), `_role_mcp_tools`
(read twice per turn from different places), and the `AgentDefinition`
(reassigned in place by the org diff).

Do **not** pin: the `PromptSkillRegistry` (see above), or the tool dispatch
objects — pinning a catalogue does not pin a capability, since a pinned
wrapper whose client was stopped just fails softly. Pinning buys consistent
naming and limits across a turn, never guaranteed capability. That is what
makes drain-before-stop load-bearing rather than optional.

### 4.G — Corrections to the smaller clauses

- **Jitter on applies is dropped** — it compounds 4.B's brownout. Jitter
  stays only on the *poll interval* (±20%), to break lock-step after a
  synchronized fleet restart.
- **The dual-secret HMAC overlap is struck**: the prior revision's secret is
  not reachable at verification time. Replaced with an operator procedure.
- **Raw-vs-resolved diffing is systemic**, not MCP-specific: LLM providers,
  sandbox, four integrations, extensions and per-role `mcp_env` all gate on
  raw config while their builders resolve. The fingerprint covers them
  uniformly.
- **4.5's staleness bound is false for sandboxes** by orders of magnitude: a
  live box holds resolved credentials for the run duration plus pause TTL.
  Either drive the existing `reseed` path on a credential change, or record
  the honest bound.

### Live bugs found by the gate (pre-existing, fixed with this phase)

1. **`ToolRegistry` has no `unregister`.** A shared MCP server removed by a
   live config edit leaves its tools advertised in every later turn's
   catalogue, dispatching to a dead client forever.
2. **Rollback reinstalls stopped transports** — the node goes silently deaf
   while reporting a healthy epoch.

### Exit criteria (revised)

The four original chaos tests miss every interleaving above. Add: rotation
with an *unchanged* revision proves an MCP child restarts; epoch N succeeds
on node A and fails on node B, so B gates, retries a bounded number of times,
then fails `/ready` while A keeps serving; a sandbox completion on a gated
node still resumes; a node that applied and then died does not gate its peers
behind its stale row.

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

### Gate (a) — broker measurement — CLOSED

Measured against **Apache Pulsar 4.2.4 standalone** (`--no-functions-worker
--no-stream-storage`, `PULSAR_MEM="-Xms512m -Xmx1g
-XX:MaxDirectMemorySize=512m"` — the compose stack's own configuration),
via `tests/test_queue/test_broker_behavior.py`:

| Measurement | Result |
|---|---|
| Redelivery after a **graceful close** | **9 ms**; all held messages recovered |
| Redelivery from a **wedged** consumer (never closes) | **12.0 s** against an 11 s ack timeout |
| **Cursor continuity** on owner handoff | owner acked `[0,1,2]`, successor saw `[3,4,5]`, **replayed `[]`** |
| **Attach latency** to an existing subscription | **4.9 ms** |
| **Prefetch hostage** with `receiver_queue_size=64` | **64** of 256 (the 1000 default would have held all 256) |

What the numbers decide:

- **Owner-only Shared is sound.** The shared cursor survives a change of
  owner with no replay and no loss. This was the entire case against
  Exclusive and it is now measured rather than asserted.
- **The broker imposes no floor on the lease TTL.** A graceful close
  releases in 9 ms and attach costs 5 ms, so a successor is productive
  essentially immediately. The TTL is therefore bounded by *heartbeat
  reliability*, not by the broker — 45 s = 15 s interval × 2 tolerated
  consecutive misses, plus headroom for clock skew.
- **The claim-rate limit is not about attach.** At 5 ms, attaching is
  free; the cost of a takeover is spawning that seat's MCP children. The
  limiter stays where it was, for that reason.
- **The wedged-node window is the ack timeout, confirmed.** 12.0 s against
  an 11 s timeout scales to **~30 minutes** at the engine's real
  `_INBOX_ACK_TIMEOUT_MS`. Nothing in the broker shortens it, which is
  precisely why correctness against zombies comes from epoch fencing
  (5.5, 5.7) and why the prefetch cap matters — the cap is the difference
  between a wedged node holding 64 of a seat's messages for half an hour
  and holding a thousand.

Two things the run corrected in the harness itself, both of which would
have quietly voided the numbers:

- the Pulsar client **refuses an unacked-message timeout below 10 s**;
- a **new subscription starts at `Latest`**, so a consumer that attaches
  after publishing sees nothing at all. Harmless for the engine (its inbox
  subscriptions exist before traffic flows) and fatal for a measurement
  that publishes first.

The prefetch cap and the harness themselves:

**`receiver_queue_size` is now set explicitly**
(`_RECEIVER_QUEUE_SIZE = 64` for durable consumers, scaled to `2 ×
max_batch` for batch subscriptions; `_STREAM_RECEIVER_QUEUE_SIZE = 200`
for the dashboard's broadcast consumers). The client default of 1000 was
not a throughput win but a liability: prefetched messages are
delivered-but-unacked, `_INBOX_ACK_TIMEOUT_MS` is thirty minutes, and a
*wedged-alive* node therefore sat on up to a thousand of its seat's
messages for half an hour. Turns are serialized per seat, so prefetch
beyond one batch buys nothing; 64 covers the default `max_batch` of 20
three times over and cuts the worst case ~15×.

**Closed:** the harness exists — `tests/test_queue/test_broker_behavior.py`,
six measurements: redelivery after a graceful close, redelivery from a
wedged consumer that never closes, cursor continuity across owner
handoff, attach latency, prefetch hostage size, and the explicit-prefetch
regression guard. Each asserts the property the design rests on and
prints the number to tune to.

Re-run them any time with `pytest
tests/test_queue/test_broker_behavior.py -m integration -s` against a
broker; they skip without one, and skipping is not passing.

### Gate (b) — mixed-version fleets — CLOSED

`PROTOCOL_VERSION = 1` in `crewlet.db.leases`, enforced *inside*
`try_acquire`'s statement rather than as a read-then-claim pair — the
pair loses the race it exists to prevent. A node refuses to claim
**anything** while any live lease is held at a lower protocol.

The rule is asymmetric, and that is the point: older nodes keep working
(they cannot run a check that postdates them), newer ones wait visibly
until the last old lease lapses or is released. A rolling deploy
converges because draining the old is what a rolling deploy does.
`fleet_protocol_floor()` is the observability half — without it a node
stalled by the gate looks exactly like one whose peers hold every seat.

Two consequences, documented at the constant: schema evolution here is
**additive-only**, and a **downgrade across a protocol bump requires a
full fleet drain** — an older build has no check to run, so nothing in
the table can stop it taking over a newer node's expired leases.

### Gate (c) — adversarial review of the handoff interleavings — CLOSED

Re-attacked the interaction SCALING.md § open question 7 names:
republish-deferral × coalescing × turn-claim inside an ownership handoff.
Six findings, each changing the design below.

**C1 — coalescing destroys the trigger identity the claim is keyed on
(fatal for 5.4 as written).** The inbox batcher merges N same-conversation
notifications into ONE digest with a *fresh* event id. A claim keyed on
that id can never match a redelivery, because a redelivery after node
death coalesces a possibly-different set into a different merged id. The
claim would silently never short-circuit for the majority of inbox
traffic — dead weight that reads like protection.

*Resolution:* the claim is keyed on the **constituent** event ids, not
the merged one. A batch claims all of them in one statement, drops any
already `completed` from the batch, and coalesces what remains. Node died
after completing → every constituent is `completed`, nothing remains, ack
and skip. Died mid-turn → constituents are `in_progress` at a dead epoch
→ superseded → re-run.

**C2 — a deferral that runs after a claim leaks the claim.** Republish-then-ack
on a path that already took a claim leaves `in_progress` rows owned by a
node that is not running them, at a *live* epoch — so the new owner
cannot supersede them and the trigger stalls until the claim expires.

*Resolution:* **no path may claim and then defer.** Every deferral
condition (no turn engine, `AWAITING_SANDBOX`, config shed, not the lease
owner) is evaluated before the claim is taken, and any post-claim failure
deletes the claim in the same `finally` that defers.

**C3 — republish-then-ack feeds itself during release (fatal).** The
deferral republishes to the seat's own inbox topic. During a lease
release the owner's consumer is still the only member of that Shared
subscription, so it immediately receives its own republished copies and
defers them again — a hot loop that ends only when the consumer detaches.

*Resolution:* **quiesce the consume loop before deferring.** Release
order is: stop the loop from dispatching → defer the in-flight partition
(republish + ack, consumer still open so the ack lands) → close the
consumer → release the lease. Deferring after the close is not an
alternative: the ack would fail and the message would be both redelivered
*and* republished.

**C4 — handoff redeliveries spend the dead-letter budget.** Closing a
consumer releases its unacked messages immediately (harness measurement
1), which is convenient — but every redelivery increments
`redeliveryCount`, and `_INBOX_MAX_REDELIVER` is 3. A message in flight
across three seat handoffs is dead-lettered, and a rolling 3-node deploy
can plausibly move a seat that often.

*Resolution:* this is precisely why the deferral is republish-then-ack
rather than "just close and let the broker redeliver" — a republished
message is a *new* message with a zero redelivery count. C3's ordering is
what makes it usable. The broker's own redelivery remains the fallback
for the un-graceful path (`kill -9`), where three handoffs of one message
is not a realistic shape.

**C5 — the heartbeat's failure direction, and what a watchdog thread can
actually do.** SCALING.md calls for the heartbeat on a dedicated OS
thread that "kills its own seat work" when the loop stalls. It cannot: a
stalled event loop does not process `call_soon_threadsafe` either, so no
cross-thread signal can stop the work.

*Resolution:* the heartbeat is an asyncio task, whose failure mode is
already the safe one — a stalled loop stops renewing, leases lapse, peers
take over. What the dedicated thread genuinely adds is *detection*, and
the one action it can take unilaterally: a watchdog thread that observes
event-loop lag and **hard-exits the process** when the loop is stalled
past the lease TTL. A node that cannot prove it is alive removes itself
rather than becoming a zombie. The residual window between lapse and exit
is bounded by 5.7's in-turn fencing.

**C6 — `preferred` must not be read as ownership.** The stickiness hint
survives release by design, so after a node dies its id sits in
`preferred` on every seat it held. Any placement pass that treats a
matching `preferred` as a reason to *wait* would stall those seats until
the dead node returns.

*Resolution:* `preferred` biases the order a node tries seats in, and
nothing else. Never a claim precondition, never a reason to skip.

**Unchanged by the review:** the owner-only Shared subscription (the
cursor argument holds), the turn-claim state machine's supersede rule,
sandbox resumes never taking a turn claim, and the DB-claim variant of
owner-routed sandbox control.

### 5.1 Seat leases — DONE

`crewlet/seat/host.py` (`SeatHost`) + `crewlet/seat/watchdog.py`
(`EventLoopWatchdog`), with a contract suite that runs against the memory
lease twin and — when `CREWLET_TEST_DSN` is set — against real
PostgreSQL.

Delivered as planned: one lease per seat (`seat:{handle}`), greedy claim
to `ceil(seats/N)`, claim-rate limit, `preferred` stickiness.

Decisions the implementation had to make that the plan left open:

- **`N` comes from `node:{id}` presence leases**, renewed on the same
  heartbeat. There is no membership service, and inferring the node count
  from *seat* ownership cannot work — a fleet where nobody has claimed
  anything yet reads as zero nodes, and every node then believes it
  should take every seat. The resource shape was already reserved in
  `017_leases.sql`'s comment.
- **`preferred` orders the attempt and never gates it.** Stated in the
  plan; worth restating because the failure is silent and permanent — the
  hint survives the node that set it, so treating a foreign hint as a
  reason to wait would strand every seat a dead node used to hold.
- **`renew() == False` drops the seat immediately; `LeaseError` does
  not.** The lease module already drew this distinction; the seat host is
  the first caller that has to act on it, and getting it backwards tears
  a healthy node's whole company down over a two-second database blip
  during which no peer could claim the seats anyway.
- **A failed `on_acquire` releases the seat.** A takeover whose pipeline
  raised would otherwise read as owned to the entire fleet while nothing
  runs it — the seat simply goes dark until the process restarts.
- **The watchdog's beat and poll intervals are scaled to the
  threshold.** Found by a test: at a 0.5 s threshold with a fixed 1 s
  beat, a *healthy* loop reports itself a second behind and shoots
  itself. Invisible at production values (45 s vs 1 s) and lethal to
  anyone who lowers the lease TTL, so it is enforced in the constructor
  rather than documented as a caveat.

Constants, from the gate (a) measurements rather than from reasoning:
`SEAT_LEASE_TTL_SECONDS = 45` (the broker imposes no floor — 9 ms to
release, 5 ms to attach — so the TTL is bounded by heartbeat reliability:
three intervals, tolerating two consecutive misses),
`SEAT_HEARTBEAT_INTERVAL_SECONDS = 15`, `SEAT_SWEEP_INTERVAL_SECONDS = 5`,
`SEAT_CLAIM_LIMIT_PER_SWEEP = 4` (MCP spawn, not attach, is the cost of a
takeover).

Not yet wired into `Engine` — that is 5.2, where owning a seat starts to
mean something.

### 5.2 Owner-only subscriptions — DESIGN FAILED REVIEW

A pre-implementation review (4 lenses over the mapped seams) returned
**`redesign-needed` from three of four**. The design as written is not
what gets built. Two of its fatal findings were then **overturned by
measurement** against the live broker, and the rest stand.

#### Measured, and they change the design

- **A durable subscription retains its backlog with NO consumer
  attached.** Published 5 messages while nothing was subscribed; the
  successor received all 5. So the reviewed "unowned seats are a black
  hole" hazard is wrong *provided the subscription is never
  `unsubscribe()`d* — which makes a non-destructive detach load-bearing
  rather than merely tidy.
- **A close-driven handoff does NOT increment `redeliveryCount`.**
  Prefetched-but-unacked messages returned to the successor at count 0.
  This kills four findings at once — the "63 prefetched messages
  dead-letter after three handoffs" fatal, the conversation-reordering
  fatal that followed from it, and gate (c)'s own C3/C4, which invented
  republish-then-ack to avoid a DLQ cost that does not exist on this
  path. **A plain close is a safe deferral.** What *does* increment the
  counter is a NAK, so the cancellation path is the thing to avoid, not
  the close.

#### Standing, and fatal

1. **Detach is the wrong safety property.** The prescribed ordering
   (quiesce → defer → detach → release) describes only the *voluntary*
   path. The dominant path is lease **loss**, which inverts it by
   construction: the row lapsed, so a peer may already hold the seat
   before this node notices. Nothing anywhere tests ownership on the
   turn-start path — `SeatHost.epoch_for` had no callers outside its own
   module. The safety property has to be an **epoch-fenced ownership
   check where a turn starts**, with detach demoted to cleanup.
2. **`pause_topic` cannot quiesce.** Verified in the code: the per-topic
   gate is checked at the top of `_receive_one` only. The post-receive
   gate tests the *global* pause, and `_collect_batch` never checks the
   topic pause at all. So a message already fetched is still dispatched,
   and a linger window keeps fetching more. Quiesce needs a
   per-`_Subscription` flag checked at four points, not a topic pause.
   (This also means today's sandbox busy gate keeps filling a batch
   after the pause lands — a live, if minor, bug.)
3. **Spawning agents on every node and gating only the inbox is
   unsound.** `AgentInstance.state` is process-local and never persisted,
   so a non-owner's instance can be stranded in `AWAITING_SANDBOX`
   permanently and the seat comes home poisoned. It also contradicts
   5.1's own constants, which justify the claim-rate limit by MCP spawn
   cost that spawn-everywhere would eliminate. `on_acquire` must
   establish a *known* seat state.
4. **Pause holds are keyed by topic and survive teardown**, so a
   release/re-acquire on one node re-attaches into a still-paused topic
   and goes silently deaf — and `preferred` stickiness makes
   flap-and-return the *normal* case. Holds must be keyed by
   `(topic, group)` and dropped on detach.
5. **The sandbox busy gate lands on the wrong node** (N−1)/N of the time:
   `SandboxCoordinator` subscribes both control topics under one
   fleet-wide Shared group. `recover()` is likewise a fleet-wide
   unfiltered scan that would poison every node's instances.
6. **The memory twin has no backlog**, so owner-gating turns into
   unconditional message loss in single-process and test runs.

#### The structural conclusion

**5.2, 5.3 and 5.6 are one change, not three.** A seat's inbox cannot be
owned independently of its sandbox control and its recovery scan: gating
the consumer while completions still ride a fleet-wide group, and while
`recover()` scans every seat on every node, produces a node that owns the
mail and not the work. They will be implemented and reviewed together.

#### Fixed in 5.1 as a result

- **The `LeaseError` grace was unbounded.** `heartbeat` retried forever
  on an unreachable store while the module docstring claimed it retried
  "until the TTL genuinely runs out" — a deadline the code never
  computed. One unreachable database therefore became two nodes serving
  one seat. Now bounded by the TTL, measured from the last *successful*
  renew.
- **The heartbeat and sweep were unsynchronised.** A per-seat
  `asyncio.Lock` is now held across the whole acquire and release hook,
  with an epoch re-check inside it, so a heartbeat carrying a stale lease
  cannot tear down a claim a sweep made while it was awaiting.

### 5.2a — Org-derived resolution — IN PROGRESS

Split out of the revised 5.2 and shipped first, because it is not
actually multi-node work. Six paths resolve a recipient through the live
`AgentInstance` when all they need is the **org**, and that is a latent
fragility today: a seat that exists but has not been spawned — during the
boot cascade, after a failed `_spawn_role_live`, mid-config-apply — is
invisible to routing that goes through the pool. Owner-only seats turn a
latent fragility into a guaranteed one, which is how the review found it.

The rule: **routing needs a handle and an agent id, both derivable from
the org. The live instance is an execution detail and must stop being a
routing one.**

**Landed:** `HandleRegistry` resolves agent seats from the org, with the
live instance attached when this process has one. `ResolvedParty` gains
`is_local` (the execution question), `agent_id` (derived via
`derive_agent_id(org.name, handle)`, so every process computes the same
id with no database and no instance) and `inbox_topic`. `all_parties()`
enumerates the **org** rather than the pool — the colleague surface
builds its fuzzy-match corpus from it, and a missing colleague does not
merely fail to match, it lets the match land on the wrong one. Pool-only
construction (no org reference at all) still falls back to the pool.

`agent is None` now has two causes and callers must not conflate them: a
human seat, which never has an instance, and an agent seat that is not
running here. `kind` distinguishes them; `is_local` answers the
execution question.

**Remaining in 5.2a:** the notification-ingress recipient resolution, the
internal task routing in `events/subscriptions.py`, the scheduler's
`_fire`, the A2A `request_channel` guard, and seeding budget caps for
every org seat on every node.

### 5.2 — Seat ownership: inbox, takeover, and sandbox control (REVISED, one phase)

Replaces the original 5.2, 5.3 and 5.6. They were three phases on paper
and one change in fact: a node that owns a seat's mail but not its
sandbox control, or that recovers seats it does not own, is not a seat
owner — it is a node holding half of one. Splitting them ships a state
we already know is wrong.

#### The five decisions the review forced

**(a) Subscription existence is an org invariant, not an ownership one.**

Every node ensures the durable subscription exists for *every* seat at
boot — `subscribe` then immediately `detach`, ~14 ms per seat by the
measured attach/release numbers — and ownership thereafter governs only
*attachment*. This is what makes an unclaimed seat safe: measured, a
durable subscription retains its backlog with no consumer attached, so
messages published during a lease gap, a claim ramp, or a full fleet
restart are held rather than discarded. Without the invariant, the first
publish to a never-subscribed seat is silently dropped — no DLQ, no
producer error.

It also decouples decommission. Deleting a subscription becomes an
explicit `EventQueue.delete_subscription(topic, group)` that does not
require a local consumer, so role removal cannot depend on which node
happens to hold the seat.

**(b) The safety property is an epoch-fenced ownership check where a turn
starts — not detach.**

The prescribed release ordering only ever described the *voluntary* path.
The dominant path is lease **loss**, which inverts it by construction:
the row lapsed, so a peer may already be running the agent before this
node notices. No amount of careful detaching closes that window, because
the window opens before detach is reached.

So the inbox handler gains an ownership branch beside `admits_triggers()`:
if this node does not hold the seat's lease, requeue-and-ack (never NAK).
`SeatHost.epoch_for` stops being decorative and becomes the token every
seat-scoped write carries. Detach is demoted to what it actually is —
cleanup that stops *new* deliveries, not the thing that makes concurrency
impossible.

**(c) Two release modes, because loss and drain are opposites.**

- **voluntary** (drain, capacity rebalance, role gone): quiesce → let the
  in-flight handler finish under a bounded wait → detach → release lease.
- **fenced** (renew returned `False`, the TTL grace expired, an acquire
  hook failed, posture went SHED/STUCK): **detach first**, abandon
  in-flight work, and never republish. A peer may already be running this
  seat; republishing hands it a second copy of work it is already doing.

`on_release(handle, lease, *, reason)` carries which. `role_gone` must
not defer at all — the events are for a role that no longer exists.

**Republish-then-ack is dropped from the handoff path entirely.** Measured:
a close-driven handoff does not increment `redeliveryCount`; unacked
messages return to the successor at count 0, in order. Gate (c)'s C3 and
C4 invented republish-then-ack to avoid a dead-letter cost that does not
exist here, and it was itself the source of the conversation-reordering
fatal (republished messages go to the topic tail while prefetched
siblings replay from the head). A plain close is the correct deferral.
What *does* burn the counter is a NAK, so the rule inverts: **quiesce
must avoid cancelling a handler mid-flight**, because the cancellation
path NAKs.

**(d) Quiesce is a per-subscription flag, not a topic pause.**

Verified in the code: `_paused_topics` is consulted only at the top of
`_receive_one`, before the blocking receive. The post-receive gate tests
the *global* pause, and `_collect_batch` never checks the topic pause at
all. A pause therefore stops the *next* fetch and nothing else — a
message already fetched still dispatches, and a positive linger keeps
collecting more.

`_Subscription` gains a `quiescing` flag, checked at four points: loop
top, immediately after `_receive_one` returns, inside `_collect_batch`'s
loop, and immediately before dispatch. Collected-but-undispatched
messages are left unacked for the successor rather than dispatched or
NAK'd.

(That same gap is a live bug today: the sandbox busy gate keeps filling a
batch after the pause lands. It is fixed by the same flag.)

**(e) Sandbox control is owner-routed, and a seat is established, not
assumed.**

`SandboxCoordinator` subscribes both control topics under one fleet-wide
Shared group, so a completion lands on a non-owner (N−1)/N of the time.
Replaced by a **per-seat control topic** — `crewlet.agent.{handle}.control`,
group `agent-{handle}-control` — attached and detached alongside the
inbox. Routing then emerges from who subscribes, exactly as it does for
the inbox, rather than from any "which node" computation. It cannot ride
the inbox itself: while a seat is `AWAITING_SANDBOX` the inbox is paused,
and a completion riding it would queue behind the very pause it exists to
lift.

`pending_sandbox_run` gains `owner` and `owner_epoch`. `recover()` stops
being a fleet-wide scan and becomes a per-seat step inside `on_acquire`,
fenced on the claiming node's epoch.

And **agents are no longer spawned on every node.** That was the original
point 5, and it is unsound: `AgentInstance.state` is process-local and
never persisted, so a non-owner's instance strands in `AWAITING_SANDBOX`
and the seat comes home poisoned. It also contradicts the constants 5.1
shipped, which size the claim-rate limit by MCP spawn cost that
spawn-everywhere would eliminate. `on_acquire` establishes the seat in a
known state — instance, budget, per-role MCP children, pending-run
recovery — and attaches the consumer **last**, the ordering boot already
uses and `_spawn_role_live` currently inverts.

#### Work items

1. **`EventQueue` protocol**: `detach(topic, group)` (non-destructive,
   idempotent, returns whether an attachment existed),
   `delete_subscription(topic, group)` (no local consumer required),
   `quiesce(topic, group)`. `unsubscribe` is removed in favour of the
   explicit pair — its name never said which one it was.
2. **Pause holds keyed by `(topic, group)`**, dropped on detach, with a
   dedicated `"seat"` reason. Today they are topic-keyed, process-local
   and survive teardown, so a release/re-acquire on one node re-attaches
   into a still-paused topic and goes silently deaf — and `preferred`
   stickiness makes flap-and-return the *normal* case.
3. **`MemoryEventQueue` gets a real durable subscription**: a
   per-`(topic, group)` backlog created on first subscribe, appended to
   by `publish` when no attachment exists, replayed on re-attach; plus
   round-robin delivery across members of a group. Without both, the twin
   inverts the design's load-bearing property and the double-consumer
   split-brain is structurally untestable — CI would read green on
   exactly the failure this phase exists to prevent.
4. **A fake-broker unit suite** at the `_create_durable_consumer` seam
   (the shape `test_pulsar_unit.py` already uses), asserting the real
   detach body: close called, `unsubscribe` *not* called, executor shut
   down, registry entry removed.
5. **`SeatHost`**: `on_release` reason; fail-closed release (a detach
   that cannot be proven complete does **not** release the lease — keep
   renewing and retry, and if it cannot be closed within the TTL, take
   the watchdog's remedy); acquire-hook failure gets backoff and negative
   stickiness rather than an immediate re-claim; `_notify_release` stops
   swallowing exceptions silently.
6. **Seat identity**: `SeatHost` keys by handle, every engine drain and
   config-apply primitive keys by role name, and `Role.handle` is
   mutable. Make `handle` an identity field — a handle change
   decommissions the old seat and spawns a new one instead of taking the
   update branch — and give the hooks an explicit handle↔role mapping.
7. **`_subscribed_inboxes` → `dict[handle, epoch]`**, written and cleared
   only by the hooks, cleared in `stop()` and `_force_stop()`, and
   `_subscribe_agent_inbox` raises rather than silently skipping when
   asked to attach something it believes attached.
8. **Drain ordering**: `Engine.stop()` begins with `begin_drain()` and
   releases seats one at a time through the voluntary path *before*
   `pause_delivery()`. Today's order blackholes the node's own seats for
   the whole drain while its heartbeat keeps renewing them.
9. **Posture couples to ownership**: SHED/STUCK drives `begin_drain()` +
   `release_all()` and drops the `node:` presence lease, so the fair
   share recomputes over serving nodes only. Otherwise a diverged node
   holds seats it refuses to serve and reserves capacity for itself.
10. **Observability**: a `seats` accessor on `NodeRuntime`, rendered in
    the health envelope (held, capacity, live nodes, last claimed, last
    lost, protocol block); `SweepResult.lost` actually populated — it is
    documented "for logs, tests and /health" and is hardcoded empty;
    `inbox_attached` / `inbox_detached` at INFO with seat, epoch and
    elapsed ms. Every failure mode in this phase is currently diagnosable
    only by reading three processes' logs at DEBUG.

#### Measurements this design rests on

| Question | Answer | Consequence |
|---|---|---|
| Does a durable subscription retain its backlog with no consumer? | **Yes**, all messages recovered | (a) is safe; detach must never `unsubscribe` |
| Does a close-driven handoff increment `redeliveryCount`? | **No**, count 0 at the successor | republish-then-ack dropped; C3/C4 retired |
| Does the cursor survive a change of owner? | **Yes**, no replay, no loss | owner-only Shared confirmed |
| Attach / release cost | 4.9 ms / 9 ms | the boot pre-create in (a) is affordable |
| Does ack-timeout redelivery increment the counter? | **Yes**, 0 → 1 | `_INBOX_MAX_REDELIVER = 3` becomes "three node deaths per message"; raised to 10 (item 12) |
| Do the broker's reapers delete an unattached subscription? | **Both reapers are live**; not caught within 150 s at a 1-minute setting | the conservative broker config is required, not optional (item 11) |

11. **Broker reaper configuration becomes load-bearing, and today's
    compose config contradicts decision (a).**

    Two reapers are enabled by default and both delete the thing (a)
    depends on. `brokerDeleteInactiveTopicsEnabled=true` with a 60 s
    sweep removes an idle topic outright — observed in the broker log
    doing exactly that to a test topic ("Topic deleted successfully due
    to inactivity"). And `subscriptionExpirationTimeMinutes` deletes a
    subscription whose last-active is older than the threshold —
    also observed ("The subscription was deleted due to expiration").

    The repo's own `docker-compose.yml` sets that to **30 minutes**,
    deliberately, and justifies it in a comment: *"Non-lossy for live
    work: the engine's own subscriptions have a connected consumer the
    whole time it runs."* **That premise is exactly what this phase
    breaks.** Under owner-only attachment a seat's subscription has no
    connected consumer for as long as the seat is unowned, so the
    setting silently becomes lossy — a seat nobody claims for half an
    hour loses its subscription and, with it, the backlog decision (a)
    exists to preserve.

    So the phase must ship the operator configuration alongside the
    code: subscription expiry off (or far above any credible unowned
    window) and inactive-topic deletion off or set to
    `delete_when_subscriptions_caught_up`, with the compose comment
    rewritten to say why. An operator who upgrades the engine without
    changing the broker gets a fleet that loses a quiet seat's mail, and
    nothing in the engine can detect it.

    *Measurement status:* with expiry forced to 1 minute, a detached
    subscription — empty and backlogged alike — still held its message
    after 150 s, so the reaper is not as eager as the setting suggests.
    That is **not** evidence the 30-minute window is safe; it only means
    the two tests did not catch it. The conservative configuration is
    required either way, and a long-running test belongs in the chaos
    suite rather than the harness.

12. **`_INBOX_MAX_REDELIVER`: 3 → 10.** Measured: an ack-timeout
    redelivery — the `kill -9`, wedged-node and watchdog-`os._exit` path
    — increments the counter. Three was sized for a single-node world
    where a redelivery means "the handler failed", i.e. a poison
    message. In a fleet it also means "a node died holding this", so the
    budget silently became three node deaths per message, after which a
    perfectly healthy event is dead-lettered having never been handled.

    Ten, because the two causes separate by **rate**, not by count. A
    genuine poison message is NAK'd on handler failure and redelivered
    after `_NEG_ACK_REDELIVERY_DELAY_MS` (1 s), so it still burns the
    whole budget in about ten seconds — poison detection goes from 3 s to
    10 s, which nothing depends on. Handoff-driven redeliveries are
    minutes to half an hour apart (the ack timeout is 30 minutes), so ten
    covers a 3-node rolling deploy plus several genuine crashes over a
    message's life. Raising the count is close to free for the case the
    cap exists for, and buys the case a fleet actually produces.

#### Review outcome (pre-implementation, 5 lenses + completeness critic)

**Five `implement-with-changes`, one `redesign-needed`** (the per-seat
control topic). A marked improvement on the previous round's
three-of-four `redesign-needed`, and the core reframings were endorsed
independently by several lenses: subscription existence as an org
invariant, the pause-gap diagnosis in (d) — verified in code by three
reviewers rather than taken on trust — retiring the fleet-wide
`recover()` scan, splitting `unsubscribe` into `detach` +
`delete_subscription`, and "agents are no longer spawned on every node"
for the reason given.

What has to change before implementation:

**(a) pre-create — three corrections, one of them fatal.**

- *Pre-creating by subscribing joins a Shared subscription a peer
  actively owns*, and the joining node then takes a share of that seat's
  live traffic into its own prefetch before detaching. Decision (a) as
  drafted has every node do this for every seat at every boot — it
  manufactures the double-consumer state the whole phase exists to
  prevent. Creation must happen **once, behind the `worker:{duty}`
  singleton lease**, not N times from every node.
- *`_create_durable_consumer` never passes `initial_position`* (verified),
  so every subscription it creates starts at `Latest`. A pre-created
  subscription would therefore skip anything already published — the
  invariant would exist and still lose messages. Pre-create must be
  `Earliest`.
- *`delete_subscription` without a local consumer is not implementable on
  this client*, and the subscribe-then-unsubscribe workaround has the same
  join problem. Decommission goes behind the same singleton, after the
  seat lease is released and tombstoned.

**(b) the ownership check — it cannot carry the safety property alone.**

`SeatHost.owns()` reads `self._held` (verified), a snapshot refreshed on
a 15 s heartbeat against a 45 s TTL. The check can therefore be a full
TTL stale, which is precisely the window it exists to close, so it cannot
meet its own exit criterion. It is an **optimization**; correctness has
to come from epoch-fenced writes — which are scheduled for 5.5. **5.5
moves into this phase.** Two further corrections: the branch must not
*requeue* (that reorders, re-creating the fatal (c) declares retired —
it should leave the delivery unacked and detach), and it needs a pause or
backoff behind it or it hot-loops when no node owns the seat — unbounded
inline recursion on the memory twin, which dispatches inside `publish`.

**(e) the control topic — `redesign-needed`.**

The draft dropped superseded-5.6's actual fix: `_dispatch_resume_execute`
must **raise** when the agent is not local, so a misrouted completion
reverts instead of settling the run to `done` and discarding the
suspended Execute conversation. Beyond restoring it: `recover()` inside
`on_acquire` reaps a live run's sandbox (the reap's own precondition is
destroyed by the move); `resumed` rows lose their reconciler; the waiter
and pause reaper are not seat-owned, which contradicts epoch-fenced row
writes; and moving the sandbox lifecycle events off `crewlet.events.*`
blanks the dashboard's Running-sandboxes panel.

**Item 12 (`_INBOX_MAX_REDELIVER` 3 → 10) — narrower than written.**

The constant governs **every durable subscription in the process**, not
the inbox, and the design traced none of the others. The memory twin
carries its own `max_redeliveries: int = 3` with different semantics
(verified), so raising one silently diverges the backends. And the rate
argument is false at the edges: a node that dies *fast* produces NAKs a
second apart, indistinguishable from poison. The raise stands, scoped and
with the twin moved in lockstep.

**Exit criteria are ungated.** They lead with "the chaos suite", which
does not exist and which no work item builds. Either a work item builds
it or the criteria name tests that exist.

**The completeness critic found the largest thing in the review, and it
is one gap wearing six costumes.**

Decision (e) — agents exist only on the seat's owner — is right, and it
breaks every handler that resolves a recipient through the **local agent
pool** while consuming from a **fleet-wide competing-consumer group**.
There are at least six, and three of them are fatal:

- **Notification ingress.** `crewlet.notifications.inbound` has one
  fleet-wide group. A Slack mention of `engineer` is won by whichever
  node happens to take it; that node resolves the recipient through its
  own pool, finds nothing, and the delivery is *terminally dropped*.
  With N nodes that is (N−1)/N of all inbound. This is the primary
  ingress path.
- **Internal task routing** (`events/subscriptions.py`) has the same
  shape and needs **two** local seats to succeed — the completing agent
  and its manager — so it fails more often than ingress, and one of its
  three drop paths is silent.
- **The scheduler** cannot fire for a seat this node does not own, and
  the miss is unrecoverable by construction: the at-most-once
  `scheduled_runs` claim is taken *before* the publish, so a non-owner
  marks the fire done and nothing runs it.

And three more that are serious rather than fatal: cross-node `a2a_ask`
is refused by a guard whose error message asserts something false; the
colleague surface shrinks to the node's own seats, so an agent stops
seeing its own org chart and can fuzzy-match onto the wrong colleague;
and the learning workers' reflection budget gate silently becomes a
no-op.

**The unifying fix is one sentence: resolution must be org-derived, not
instance-derived.** Routing needs only `handle → (inbox topic,
agent_id)`, and both are already derivable from the org that every node
has — `agent_id` deterministically, via `derive_agent_id(org.name,
handle)`. The live `AgentInstance` is an *execution* detail and must stop
being a *routing* one. That is a single work item, it blocks the phase,
and it is the largest one in it.

Two further gaps worth naming because they are not fixed by that:

- **Placement reads a per-node, possibly-stale org** while the control
  plane deliberately keeps a lagging node `SERVING` (that is phase 4's
  central rule). So during a rollout the fleet disagrees about the seat
  *set*, and therefore about capacity. Placement must become
  epoch-aware: refuse to **claim** under a stale epoch, but never refuse
  to **release**.
- **`ctx.agent_pool` changes meaning for extensions** — a documented
  public API that silently becomes "this node's seats", typically empty
  at `on_engine_start`. At minimum this needs saying in the design and
  in `docs/`.

**Two corrections to (c) that the first lens made concrete, and that
change what the phase can honestly claim:**

- **"Never republish, never NAK" is not expressible by a handler today.**
  The queue offers exactly two outcomes: ack on return, NAK on raise.
  A third is needed — a `DeferDelivery` sentinel meaning *leave unacked,
  stop this subscription* — or the fenced path cannot be written.
- **Fencing protects database state, never outbound effects.** The
  fenced path's property is **bounded duplication**, not no duplication,
  and the plan must say so. `run_sandbox` makes it vivid: it acquires a
  real billed E2B box *before* the pending row is written, so no
  epoch-fenced insert can undo a box that is already pushing commits.
  What bounds the window is the in-turn fence — checked before each
  round, before each write-capable tool, and inside the sub-agent loop —
  so **5.7 moves into this phase too**, because nothing else bounds it
  at all.

And the concrete fix for the stale ownership check: make it
**freshness-based rather than membership-based**. A successful renew at
time *t* proves exclusivity through *t + ttl*, so admit a turn only when
`now - renewed_at <= heartbeat_seconds`. Every started turn is then
certified owned for at least 30 s, and the `LeaseError` grace stops
admitting *new* turns at the first failed renew while still keeping the
seat so in-flight work can finish. Exposed as `may_start(handle) -> int
| None` returning the epoch — not as `owns()`.

**Net effect on the phase.** It absorbs 5.5 (epoch-fenced writes) and 5.7
(in-turn fencing), and gains an org-derived resolution layer as its
largest work item. That is four original sub-phases in one, which is a
large phase — but every attempt to draw the line elsewhere has produced a
state the reviews call fatal, and the reason is always the same: a seat
is not a thing you can half-own.

#### Exit criteria

The chaos suite, plus: a seat handed off mid-conversation preserves
order; a node that loses its lease starts no new turn even while its
consumer is still attached; a completion for a seat reaches its owner and
only its owner; an unclaimed seat's messages survive until someone claims
it; and the same suite passes on the memory twin.

---

### 5.2 Owner-only subscriptions — SUPERSEDED (original plan text, kept for the record)

- The seat's inbox consumer (`engine.py:2120-2127`) is created on lease
  acquire and closed on release — same durable subscription name
  (`agent-{handle}`), so the cursor survives handoff; type stays **Shared**
  (owner-only membership; Exclusive was rejected — DLQ policy inert, wedged
  zombies hold the slot; see [`SCALING.md` § rejected](SCALING.md#rejected-mechanisms-and-alternatives)).
- Broker-side `unsubscribe` (the destructive kind, `engine.py:5116`) happens
  **only** on role decommission, coordinated via the control plane so every
  node first releases/ignores the seat.

### 5.3 The takeover pipeline — SUPERSEDED, folded into the revised 5.2

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

### 5.6 Owner-routed sandbox control — SUPERSEDED, folded into the revised 5.2

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
