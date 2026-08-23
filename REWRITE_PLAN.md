# Crewlet Single-Binary Rewrite — Implementation Plan

Status: **approved plan, execution not started** · Target: Go · This document is the
execution contract for the rewrite. The Python tree in this same repository **is the
specification** — its tests encode measured broker behavior and incident postmortems,
its `docs/concepts/` pages describe intent. When this plan and the observed behavior of
the Python code disagree, the Python code wins and the disagreement gets escalated
(see §2), never silently resolved.

---

## 1. How to read this plan (execution model)

Work items carry one of four markers:

- **[DECIDED]** — settled during the assessment. Do not re-litigate. Rationale is
  recorded here or in the referenced doc; changing one requires going back to the
  human owner.
- **[DECIDE]** — a genuine design judgment that must be made by the **architect
  tier** (the strongest available model, or a human) *before* the dependent build
  work starts. Each DECIDE lists what the decision must cover and the constraints it
  must satisfy. The output of a DECIDE is a short written design note committed next
  to this plan (`rewrite/decisions/NNN-<slug>.md`).
- **[BUILD]** — mechanical implementation. Deliberately shaped so a cheaper/faster
  executor can do it: the spec is a named Python file plus a named test suite to port
  first. Hints are included; they are hints, not constraints.
- **[GATE]** — an executable exit criterion. **Do not start work that lists a gate as
  a prerequisite until the gate is green in CI.** Gates are how this plan avoids
  building the tower on sand.

Phases are ordered by dependency, not importance. Sizes are relative
(S/M/L/XL) — they size the *build* work, not the decisions.

## 2. Ground rules for every executor (any tier)

1. **Tests first, always.** For every module, port the named Python test suite to Go
   *before* implementing, then implement until green. The suites are the spec; the
   prose here is the map.
2. **Twins and contract suites are the architecture.** Every backend pair (memory
   twin + real implementation) runs under ONE shared contract suite, exactly as the
   Python repo does (`tests/test_queue/test_protocol.py`,
   `tests/test_db/` contract suites). A backend that isn't certified by the suite
   does not exist.
3. **Constants are either carried or re-derived — never invented.** §14 lists which.
   A re-derived constant ships with the harness measurement that produced it.
4. **Never "fix" a listed deliberate behavior.** §15 (fail-open/fail-closed table) and
   §16 (non-promises) enumerate behaviors that look like bugs and are not. Changing
   one is a DECIDE, not a cleanup.
5. **Escalate divergence.** If the Python code does something this plan doesn't
   predict, stop, write it up in `rewrite/questions/`, and get an architect-tier
   answer. Guessing is how incident-hardened semantics get lost.
6. **Byte-stable prompts and additive-only events.** The per-phase system prompts must
   stay byte-stable across rounds (provider prompt caching depends on it), and the
   event schema evolves additively with unknown-type fallback (rolling upgrades
   depend on it).
7. **Stage by path, never `git add -A`, while other executors share the checkout.**
   A blanket add takes whatever is on disk at that instant — which may be another
   executor's half-written file — and files it under a subject that does not
   describe it. This has already happened once here (`9510102` swept queue and
   coordination guard work into a prompts commit). On the RECORD question it
   raised: this branch squash-merges into ONE commit whose subject is the pull
   request title, so per-commit scope buys reviewable history, not release notes
   — the PR title is the release note and must cover the whole branch. That is
   why the mis-scoped commit is not being rewritten: published history on a
   branch other executors are working against is not worth disturbing for a
   subject line, and the practice, not the commit, was the defect.
8. No TODOs, no stubs, no deferred halves inside a phase — a phase is done when its
   gate is green and its docs section exists.

## 3. Decisions already made [DECIDED]

| # | Decision | Essence |
|---|---|---|
| D1 | **Language: Go** | Embedded NATS JetStream exists only in Go; official MCP (`modelcontextprotocol/go-sdk`), Anthropic (`anthropic-sdk-go`), OpenAI (`openai-go`) SDKs; fast iteration loop. Rust reconsidered only if the broker decision changes. |
| D2 | **Broker: embedded NATS JetStream** as the default stream+coordination substrate; the nats client is one code path for embedded and external servers. |
| D3 | **Pulsar is kept** as a certified *external stream* backend (multi-tenant estates: one tenant per company, `persistent://{tenant}/{ns}/{subject}` — the existing grammar). Pulsar never fills the coordination slot (no CAS). Client: `apache/pulsar-client-go`. |
| D4 | **Database: Turso** (`tursodatabase/turso-go`, purego, no CGO) behind `database/sql` + store contracts, with **mainline SQLite as the certified fallback driver** (dual-driver CI). Turso-specific SQL (vector `F32_BLOB`/`vector_top_k`, Tantivy FTS `fts_match`/`fts_score`) is confined to one narrow query function per feature so the fallback degrades to brute-force, not redesign. Single-process access only: the engine owns its DB file exclusively; humans inspect backup snapshots, never the live file. |
| D5 | **Two-slot deployment model.** Stream slot: `embedded-jetstream \| external-nats \| external-pulsar`. Coordination slot: `local \| embedded-kv`. Tier A config picks; boot validation refuses incoherent combos with a named reason. |
| D6 | **v1 topologies: 1 node, or 3+ nodes.** Two-node fleets are **rejected at config validation** with an error naming the constraint (embedded coordination needs a quorum; external coordination backends are post-v1). This deletes the external-SQL coordination backend from v1 scope entirely. |
| D7 | **Oxia is not built in v1.** Documented as a future external coordination backend for self-hosted Pulsar 5.0 estates; unreachable on managed Pulsar Cloud, which is the driving deployment. |
| D8 | **Sync truth, async cache.** Every durable write is quorum-acked into the stream/KV layer before returning; each node's Turso file is a rebuildable materialized index (idempotent applies keyed by stream sequence, durable per-node cursor). Coordination state never lives in Turso in fleet mode. Stream-first writes apply in solo mode too, so solo→fleet is config, not migration. |
| D9 | **Coordination scope is the company.** One engine/fleet serves one company; nothing coordinates across tenants. Per-tenant: 1 node → local coordination; 3+ → that tenant's embedded quorum. |
| D10 | **Dashboard survives verbatim.** The 16.6k-line zero-build ES-module dashboard is copied as-is and embedded via `embed.FS`; the WS wire protocol is frozen as a compatibility contract (§9). Google Fonts get vendored. |
| D11 | **Python extension system + Python API are not ported.** MCP is the primary extension seam (unchanged). In-process hooks, if any, come later via a DECIDE (Starlark/WASM). |
| D12 | **External integrations remain first-class** behind the existing seams (Transport, KnowledgeSearcher, MentionGrammar, StatusPoster, NotificationPrompt, PromotionPageWriter). Built-in chat/tracker/knowledge surfaces are **post-v1** (§12) and slot in as additional backends behind the same seams. |
| D13 | **Event store retention becomes explicit** (the stream's MaxAge is the authority; the Turso sweep mirrors it). The current unbounded `crewlet_events` is not carried. |
| D14 | **Repo layout: `go/` is temporary scaffolding, not a home.** During execution the Go module lives in `go/` so the Python tree stays adjacent as the living specification in one checkout. The terminal state is a **complete replacement**: Phase 9 (§17) moves the Go tree to the repository root and deletes the Python implementation. |
| D15 | **Clean break from Python deployments.** Breaking changes are accepted wholesale: no data migration, no state export, no compatibility shims with running Python companies. Python-era constants (e.g. the agent-id uuid5 namespace) need not be byte-preserved — what must survive is the *feature* (deterministic seat-id derivation from org + handle, so any node recovers any seat's id with no lookup), not the bytes. |

## 4. Phase 0 — Workspace, CI, and skeletons (S) [BUILD]

- `go/` module (`github.com/crewlet/crewlet/go` — final module path is a trivial
  DECIDE for the architect at kickoff). Layout hint:
  `cmd/crewlet/`, `internal/{queue,coord,store,materialize,engine,agent,org,events,
  schedule,notify,knowledge,mcp,tools,providers,sandbox,api,config,secrets}`,
  `web/` (dashboard copy).
- CI: `golangci-lint`, `go test ./...` (race detector on), a dual-driver store job
  (Turso + mainline SQLite), and an integration job that runs the real-broker suites
  (embedded NATS is in-process — free; Pulsar via testcontainers).
- Version in exactly one place; `go:embed` for the dashboard; `log/slog` with the
  same snake_case event-name discipline as `src/crewlet/_logging.py`.
- Port the schema artifacts pipeline shape: `crewlet schema <tier>` emitting JSON
  Schema, with a parity test (spec: `schema/*.json`, `tests/test_config*`).

**[GATE G0]** CI green on an empty-but-structured module; lint + race + dual-driver
jobs all wired and failing-loudly on purpose once each (prove the jobs actually run).

## 5. Phase 1 — The queue spine (M/L)

The single most load-bearing contract in the system. Spec:
`src/crewlet/queue/protocol.py` (the contract), `src/crewlet/queue/memory.py`
(871 lines — an executable specification: note the deliberate **broker/client
split**), `src/crewlet/queue/topics.py` (the one topic grammar),
`src/crewlet/events/` (types + registry + subscriptions). Suites to port FIRST:
`tests/test_queue/test_protocol.py` (backend-agnostic conformance),
`tests/test_queue/test_topics.py` (fails the build on hand-built topic strings),
`tests/test_events/`.

- **[DECIDE d-101] The Go contract shape.** Must cover: handler outcome as an explicit
  enum (`Ack | Nak | Defer` — Python uses a control-flow exception for Defer; do not
  port that), `subscribe_batch` with live-mutable batch options, the four-verb
  attachment lifecycle (`quiesce/unquiesce/detach/delete_subscription` — four verbs
  with distinct destructiveness, keep the names), pause holds keyed by
  *(topic, group)* pair with reason-scoped sets, publish-persist-before-return,
  broadcast streams with `*`/`>` wildcards, and requeue that preserves event
  id/timestamp/trace verbatim. Constraint: the memory twin and every real backend
  implement this one interface; nothing engine-side may know which backend runs.
- **[BUILD] Memory twin + conformance suite.** Port `memory.py` faithfully including
  the incident behaviors: NAK-to-front ordering, mid-batch-quiesce splice scanning the
  leading run (not a length delta), DLQ topic named outside the `crewlet.*` subject
  space, one-node's-detach-must-not-drop-a-peer's-consumer (the broker/client split).
- **[BUILD] Embedded JetStream backend.** Hints: durable consumers created via API
  (no admin-REST workaround needed — delete that whole concept), pull consumers with
  `Fetch(n, expires)` for the batch drain (this *removes* the prefetch-hostage
  problem and the ack-budget requeue subsystem — confirm via the harness before
  deleting), `Nak(delay)`, `MaxDeliver` + advisory-capture for DLQ (assemble the DLQ
  stream from `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>`).
- **[BUILD] Broker-behavior harness.** Port `tests/test_queue/test_broker_behavior.py`
  (663 lines — measurements, not unit tests; they print the numbers the design tunes
  to and fail if the broker stops behaving). Run it against embedded JetStream and
  record the JetStream constants table (§14B) as a committed doc.
- **[DECIDE d-102] Redelivery economics on JetStream.** Pulsar's measured asymmetry
  (graceful-close handoff redelivers at count 0; ack-timeout redelivery increments)
  sets `_MAX_REDELIVER=10` today. Measure JetStream's actual behavior with the
  harness and re-derive the redelivery budget + ack-wait + DLQ policy. Constraint:
  a seat handoff must not spend poison budget.

**[GATE G1]** Conformance suite green on memory twin AND embedded JetStream; harness
constants doc committed; topic-grammar test enforcing single definition in Go.

## 6. Phase 2 — Coordination + storage (L)

Spec: `src/crewlet/db/leases.py` (714), `src/crewlet/db/config_plane.py` (470),
`src/crewlet/db/{turn_completions,budgets,deliveries,rate_limits,credentials}.py`,
`src/crewlet/db/client.py`, `src/crewlet/secrets/`, migrations under
`src/crewlet/db/migrations/`. Suites to port FIRST: `tests/test_db/` (≈3.9k,
contract suites over both backends), `tests/test_secrets/` (1.3k).

- **[DECIDE d-201] The coordination contract, written semantically.** Must express:
  TTL lease + **monotonic fencing epoch that survives release** (Python: release
  expires the row in place, never deletes — on KV this means ownership and epoch are
  SEPARATE keys: ephemeral/TTL ownership + persistent CAS-bumped epoch counter),
  owner = process incarnation, tri-state answers (held / definitively-lost /
  **store-unreachable** — `LeaseError` semantics: unknown is not lost; this
  distinction is the single most incident-hardened thing in the codebase),
  the mixed-version protocol gate (cross-key predicate — on KV this becomes a
  deliberate claim protocol; design it, don't approximate it), `preferred` ordering
  hints, presence-as-membership with NodeProfile meta, per-key TTL via the store's
  clock (JetStream per-key TTL; note MaxAge-beats-per-key-TTL — bucket MaxAge must be
  unset/infinite on lease buckets).
- **[DECIDE d-202] Budget atomicity without a transaction.** Today: agent + org
  charged in ONE SQL transaction so a seat-refused turn never charges the org.
  Options: sequenced CAS with compensation, or a per-scope single-writer duty.
  Constraint: refusal must name its own scope; failure polarity stays fail-closed.
- **[BUILD] KV-backed stores** for: leases, presence, config activations (an
  append-only stream — epoch = stream sequence; the append must be atomic with the
  activation flip, design note in d-201), apply-status (TTL-fresh keys),
  turn-completions ledger (first-writer-wins, both directions fail OPEN), webhook
  dedupe (TTL re-claim), rate valve, credential cooldowns. Memory twins for all.
- **[BUILD] Turso store layer.** Fresh consolidated schema (do NOT port 31
  migrations — write the end-state schema; the Python migration list is the
  inventory). Hints: episodes gets a real `UNIQUE(agent_handle, work_key) WHERE
  work_key <> ''` (the advisory-lock dance existed only because TimescaleDB forbade
  it — delete the whole idiom); vector column dimension is a runtime property, not
  DDL templating; `crewlet_events` gets promoted filter columns + keyset-paging
  indexes `(…, event_time DESC, event_id DESC)` + an explicit retention sweep.
  Vector recall and FTS live behind exactly one store query function each (D4).
- **[BUILD] Materializer framework.** Stream → Turso appliers: idempotent by stream
  sequence, durable per-node cursor, per-seat state streams with compaction
  snapshots. **[DECIDE d-203] snapshot/compaction format + cadence** (constraint:
  seat-handoff replay must be bounded; snapshot + tail, snapshot stored in the
  stream or KV — architect picks).
- **[BUILD] Secrets.** Port `secrets/` behavior: AES-256-GCM envelope `enc:v1:` with
  multi-key keyring (stdlib `crypto/aes` + `cipher.NewGCM`), per-row AAD binding the
  var name, whole-config blob mode, resolution order STORE-then-env (a stale .env
  must not shadow a rotated secret), Tier A resolves with the store OFF, no plaintext
  mode for `secret_values`, uniform decrypt errors (no oracle), names-only logging.

**[GATE G2]** Store contract suites green on: memory twins, embedded KV, Turso AND
mainline-SQLite drivers. Lease suite proves: epoch survives release; tri-state
answers; protocol gate refuses under race (concurrent claim test).

## 7. Phase 3 — Fleet proof (M) — the go/no-go gate for the architecture

Spec: `src/crewlet/seat/` (host 1.5k, placement, watchdog), `docs/concepts/scaling.md`,
`docs/concepts/seat-ownership.md`, `docs/concepts/control-plane.md`,
`docs/guides/fleet.md` (~950 lines total — these docs ARE the spec). Suites:
`tests/test_seat/` (2.1k), `tests/test_fleet/` (1.5k).

- **[BUILD] Placement math** — pure functions; port `placement.py` with its invariant:
  share is per placement GROUP over eligible nodes, summed (a fleet-wide
  `ceil(total/nodes)` strands pinned seats — the test suite encodes this).
- **[BUILD] SeatHost** — converge in BOTH directions (claim up to share, shed excess),
  claim/release rate limits (4/2 per 5s sweep — sized by MCP spawn cost), per-seat
  asyncio-lock discipline → per-seat mutex held across whole acquire/release,
  `may_start` freshness (admission is an optimization; correctness is the epoch fence),
  release reasons voluntary-vs-fenced (fenced: detach FIRST, abandon in-flight, never
  republish), undead-teardown retry (a teardown that cannot be proven keeps the lease),
  presence suppressed while draining, last-known membership reused on store blips.
- **[DECIDE d-301] The watchdog, generalized.** Python's watchdog detects a stalled
  event loop; Go has no single loop. Define the liveness beat ("application logic
  cannot make progress while the broker client still answers keepalives") and keep
  the invariant: hard-exit with code 75, never graceful shutdown from a wedged state;
  threshold tied to lease TTL, not a config knob; stand down for a STOPPED engine vs
  a wedged one; disarmed during graceful shutdown.
- **[BUILD] Config plane + posture.** `decide_posture` is a pure function — port it
  with its rules verbatim (lag alone NEVER sheds; ISOLATED outranks exhausted
  retries; degraded never counts as converged; peer freshness bound; retry budget
  per epoch). Shed refuses by Defer at trigger admission; un-quiesce is a per-tick
  convergence, not an edge.
- **[BUILD] Fleet suite** — port the shape of `tests/test_fleet/`: TWO engines, one
  broker, one coordination store, parametrized over memory twins AND the real
  embedded cluster. Exit criteria (verbatim from the Python suite): handoff preserves
  order; a node that lost its lease starts no turn while still attached; a completion
  reaches only the owner; an unclaimed seat's mail survives full fleet restart; no
  trigger is worked by both nodes.

**[GATE G3]** Fleet suite green on twins and on a real 3-node embedded cluster.
**This gate is the architecture's proof.** Nothing in phases 4+ may merge until G3.

**Status: met.** `go/internal/node/fleet_test.go`, three substrates — `twin`
(memory queue + memory coordination), `embedded` (one embedded JetStream server +
its KV), `cluster` (a real 3-member embedded NATS cluster with R3 streams and R3
KV buckets, each node's client on a different member; started by
`internal/queue/jetstream/jetstreamtest`). Five criteria, listed above.

What the gate found, none of which was visible from one node or one server:

- **Events changed Go type at the wire.** A payload built locally was a value and
  a decoded one a pointer, so `DataAs[*T]` answered differently on the publishing
  node. Fixed at the contract (d-103); the memory twin was hiding it by never
  serializing at all, which it now does.
- **A data race in the seat host.** `Sweep` published a pointer to the value it
  returned, and a heartbeat appended through it. It also overwrote the sweep's
  `Lost` list rather than appending, so a node that shed two seats and then lost a
  third reported one.
- **A seat-thrash livelock.** A node whose renews fail while its claims succeed
  re-took a dropped seat ~100 ms later and lost it again every TTL, forever,
  respawning that seat's runtime each cycle while a healthy peer never won a race.
- **Every embedded server was named `crewlet`.** NATS requires server names unique
  per cluster and places JetStream replicas BY name, so a real fleet would have
  refused its own routes — and a node restarting under a generated name would
  orphan its replicas. `Config.ServerName` is now required when clustering and must
  be the node's stable id.
- **The embedded broker shared one default store directory** with every other
  crewlet process and test binary on the machine.

## 8. Phase 3b — The Pulsar stream backend (M) [BUILD]

Deliberately scheduled immediately after G3: certifying a second real broker early
prevents the queue contract from silently encoding JetStream-isms.

- Spec: `src/crewlet/queue/pulsar.py` (1.5k) + `admin.py` (the admin-REST
  subscription lifecycle — REQUIRED on Pulsar: creating a subscription by subscribing
  steals a live seat's traffic, measured), the tuned constants in §14B.
- `apache/pulsar-client-go`; tenant/namespace from config; the operational
  requirements doc (subscriptionExpirationTimeMinutes=0,
  brokerDeleteInactiveTopicsEnabled=false) carries over verbatim.
- Run the broker harness against real Pulsar (testcontainers); commit its constants
  table beside JetStream's. The managed-cloud checklist ships in docs: verify the
  plan exposes tenant-scoped admin REST (subscription create/delete) before
  committing to a provider.

**[GATE G3b]** Conformance + fleet suites green with Pulsar streams + embedded-KV
coordination (the multi-tenant topology), and the harness table committed.

**Status: MET.** The backend is `go/internal/queue/pulsar`, and the conformance
suite passes against a real Apache Pulsar 4.0.6 standalone broker — **121
subtests, 4 documented capability skips, 0 failures under `-race`**, verified
directly rather than only in CI. (Docker is unavailable in the execution
environment; the broker runs on the JDK, which is what made this gate
certifiable here at all.) The CI job runs the same suite against a service
container; the suite skips cleanly without `$CREWLET_TEST_PULSAR_URL` rather
than faking a broker. The redelivery economics measured for this broker are
`decisions/104`, beside JetStream's in `102`.

What the third backend was scheduled to catch, and did:

- **A seat-killing bug in JetStream.** A pause hold landing during a batch's
  linger window ended the consume loop permanently — the seat stayed attached,
  kept renewing its lease, and read nothing for the rest of the process's life.
  Reachable in production: the sandbox busy gate pauses a seat's inbox exactly
  while a detached run is in flight.
- **Two conformance cases that asserted the twin's dispatch model** rather than
  the contract — a deferral case waiting on an observable that is true before
  the handler has run, and a competing-consumers case requiring both members to
  share a burst, which Pulsar legitimately does not do.

## 9. Phase 4 — Engine core: turn engine, providers, MCP, config (XL)

The largest phase. Spec: `src/crewlet/agent/` (turn 2.5k, llm_loop, execute,
plan, review, subagent, onboarding, skills/), `src/crewlet/engine.py` (7.5k — the
entanglement point; its inbox handler at ~2915 and apply_config at ~771 are the two
passages to study line-by-line), `src/crewlet/config.py`, `src/crewlet/work_key.py`,
`src/crewlet/tools/`, `src/crewlet/a2a/`, `src/crewlet/schedule/`,
`src/crewlet/learning/`, `src/crewlet/mcp/`, `src/crewlet/providers/`,
`docs/concepts/turn-engine.md`. Suites: `tests/test_agent` + `test_engine` +
`test_tools` + `test_config*` (~30k+), `tests/test_mcp` (2.1k),
`tests/test_providers`, `tests/test_learning`, `tests/test_a2a`, `tests/test_schedule`.

Architect-tier decisions, all before build starts:

- **[DECIDE d-401] Context threading.** Replaces five contextvar channels (work_key,
  TurnPin, llm scope, phase-progress, log fields) with an explicit `TurnContext`
  passed through phase/tool/provider signatures + `context.Context` values where
  ambient is genuinely right. Constraint: spawned goroutines (sub-agents) must
  inherit it deliberately; the TurnPin becomes an immutable per-turn config snapshot
  (it already is one in spirit).
- **[DECIDE d-402] Suspend/resume serialization.** The `execute_state` schema (the
  suspended Execute conversation) is a cross-subsystem contract with the sandbox.
  Keep serialize-state-and-re-enter (NEVER a parked goroutine — the run must survive
  restarts and node handoffs). Semantics to preserve verbatim: exactly one dangling
  tool_use; a repeat call of the pending tool refused BEFORE execution; resume = same
  turn_id, skip Plan, replay activations + skill-guard state; post-resume phase
  events show only the post-resume slice; agent flips WORKING→AWAITING_SANDBOX in
  the suspending turn's finally, never through IDLE.
- **[DECIDE d-403] Config models + validation + schema generation.** ~100 Pydantic
  models with cross-field validators → Go structs + imperative validators + JSON
  Schema generation with a parity test against `schema/*.json`. Decide codegen vs
  hand-written; constraint: `${VAR}` resolution order (store before env) is code the
  validators call, and the resolution FINGERPRINT (rotation gesture detection,
  `config_resolution.py`) is preserved.
- **[DECIDE d-404] Hot reload as immutable epochs.** In-place-mutation-with-identity
  dies; design epoch-swapped immutable snapshots with per-turn pinning. Constraints:
  diff/rollback semantics, `degraded` (failed after a restart-required subsystem
  mutated) never counts converged, drain-seat-before-mutating-its-tools ordering,
  per-subsystem apply order (org→budgets→turn_engine→providers→scalars→restart-required).
- **[DECIDE d-405] Event type system.** ~70 event types; registry with explicit
  registration (no reflection scan), additive-only evolution, unknown-type fallback
  to a base envelope, trace context captured at construction.

Build items (each: port the suite, then implement):

- **[BUILD] run_tool_loop** — budget check-and-increment per round; tool_choice
  =required corrective re-prompt (max 2); progress published twice per round with ONE
  response builder shared between live and durable records; telemetry publishes
  swallowed-on-failure.
- **[BUILD] Turn engine** — Plan/Execute/Review with the delivery gates (phantom-tool
  rules: 'delivered' = called ∩ catalogue-resolved tools_needed; intent keys off RAW
  tools_needed), prior-work ledger with per-VALUE elision, 'direct' skips Review with
  the Execute-only forced-Review net, round caps + extension judge, stall detection,
  per-agent turn serialization (busy agent QUEUES, never NAKs).
- **[BUILT] Inbox handler** — the ordering is load-bearing and enumerated:
  same-id dedupe FIRST → ownership Defer → park (requeue+ack+pause) → posture shed
  (Defer) → completion-ledger drop → coalesce → bind work_key → dispatch → record.
  Ported with the engine tests that pin it, in `internal/agent/inbox` (the decision,
  no I/O reachable) and `internal/engine` (the sequence around it).
  Python's re-entrancy requeue stage is deliberately ABSENT: it guarded an inline
  dispatch that re-entered a handler within one asyncio task, and every Go backend
  forecloses that structurally — the pull loops fetch again only after a handler
  returns, and the in-process twin defers a nested drain to the loop already running.
  Measured on both backends before the stage was dropped; `queuetest`'s Reentrancy
  group pins the property, so a backend change that brought the hazard back fails
  there rather than deadlocking a seat.
- **[BUILD] Providers** — Anthropic/OpenAI on official SDKs, `max_retries=0` (the
  credential pool owns rotation), input_tokens = base + cache_read + cache_write,
  Retry-After / x-ratelimit-reset parsing, cooldowns on monotonic clocks, fallback
  chains, per-key least-in-flight selection. cli-agent: profiles as embedded data
  (all 8, YAML-overridable), per-seat HOME/XDG isolation + allowlisted child env,
  the forgiving envelope parser (port its pinned tests exactly), spent-subscription
  classified RATE_LIMIT by STRUCTURE not keywords, generation protocol keyed by
  resolved state_dir in a package-level registry.
- **[BUILD] MCP** — official go-sdk; stdio child supervision (whole-env inherit +
  overlay, stderr pipe with 50-line crash tail, 120s/300s silence deadlines),
  per-role instances (`server::Role`), registration-only-on-success, restart/
  unregister edge cases (capture doomed names BEFORE stop), discover-then-activate
  meta-tools over the MERGED universe, tri-state annotations +
  `writes_to_shared_surface`, origin grammar recorded at registration.
- **[BUILD] Learning, A2A, scheduler** — ReflectEngine harness, diary/episodes with
  work-key exactly-once (now a real unique index), counterparty profiles, skill
  synthesis seams; A2A one-ask-one-answer over the durable queue, reply does not
  charge delegation depth; scheduler = port the existing 5-field cron evaluator
  (dependency-free), at-most-once via the scheduled_runs claim, catchup clamp,
  180s per-run cap, fleet-singleton via claim-per-tick duty.
- **[BUILD] Prompts** — port `prompts.py` (752 lines of load-bearing English) verbatim
  with its token-budget tests (Plan<2400, Execute<300, Review<600 — this plan
  said 450, which is a transcription error: `tests/test_agent/test_prompts.py`
  asserts 600, and REVIEW_HEADER alone is ~555, so 450 is below the floor of
  the constant and unreachable without deleting decision rules the same item
  requires be carried verbatim).

**[GATE G4]** Golden-turn suite green: scripted mock-LLM turns (the Python repo's
mock-provider approach) reproduce Plan→Execute→Review including suspend/resume
across an engine restart, self_iterate with ledger, sub-agent spawn, A2A round-trip,
budget refusal, and a hot-reload mid-turn under the pin. Phase-record output satisfies the
frozen dashboard wire contract (the dashboard — not the Python engine — is the
compatibility reference, and consumes it unchanged).

## 10. Phase 5 — API + dashboard (L)

Spec: `src/crewlet/api/` (app, auth 267, streaming 575, live_state 1160, queries,
config_refresh 663, routes/ incl. webhooks 1351), `src/crewlet/static/dashboard/`.
Suites FIRST: `tests/test_api/` (8.8k — live_state's suite is the spec, port it
before the module), `tests/test_dashboard/js/` (runs under bare node, unchanged).

- **[DECIDED] The WS wire protocol is frozen**: envelope kinds, query frames with id
  correlation, close-1008-before-accept, `?token=` handshake, snapshot-on-connect,
  drop-oldest 512 queue, `seats` replaces / `agents` merges. The JS suite passing
  against the Go server is the compatibility proof.
- **[BUILD]** Auth guard mounted unconditionally (posture from Tier A, existence
  never conditional); one payload function per question serving both REST and WS;
  seven webhook verifiers (GitHub HMAC-SHA256, GitLab Standard-Webhooks, Plane
  hexdigest, Atlassian sha256=, Slack v0 per-agent secret map, Forge RS256+JWKS,
  503+Retry-After when secretless/unconfigured, verify BEFORE any persistence);
  /health vs /ready split (wait/isolated stay ready).
- **[DECIDE d-501] NodeRuntime over request/reply.** Owner-only facts (in-flight
  detail, live MCP tools, seat memory) served fleet-wide via NATS request/reply to
  the owning node (lease table locates it). Decide envelope, timeouts, and authz.
  Note: on Pulsar-stream topologies the embedded coordination cluster carries the
  RPC; solo mode answers in-process (today's seam).

**[GATE G5]** Dashboard loads from the Go binary and passes its own JS suite
unmodified; live_state ported suite green; a golden company runs end-to-end with the
UI showing live turns.

## 11. Phase 6 — Sandbox (M; E2B deferred) 

Spec: `src/crewlet/sandbox/` (coordinator, waiter, pending_store, local 4.7k total,
coding_agents/), `docs/concepts/code-sandbox.md`. Suites: `tests/test_sandbox/`
(9.3k with providers) — test_coordinator (1050) and test_execute_sandbox (1129) pin
the state machine; test_local pins the reaper/pid-reuse/escape guards.

- **[BUILD] Local provider is the flagship** (zero SDK): process groups via
  `SysProcAttr{Setsid}`, killpg with SIGCONT-first, explicit `Wait()` goroutine
  reaping (an unreaped zombie answers kill-0 as alive and the completion probe hangs
  — the Python comments document this exactly), /proc start-time pid-reuse guard,
  RunPaths derived from box home (never module constants), container mode via
  docker/podman argv with `--init` and `--env-file` (never `-e`).
- **[BUILD] Coordinator/waiter/pending-store** — at-most-once resume claim
  (conditional flip running|awaiting_clarification|reseed→resumed, revert-on-failed-
  dispatch to the snapshotted prior status), poll-as-completion-signal with the three
  signals (non-empty done marker with exit code; terminal streamed event for
  finish-but-never-exit agents; wrapper liveness), pause reaper CAS-to-reseed FIRST
  then release then kill-by-id-never-connect, per-seat control topic for completion
  routing, epoch-fenced mutations, recovery inside seat-acquire only.
- **[DECIDED] E2B is post-v1.** The seam (`SandboxProvider`) ships in v1; the E2B
  backend (owned OpenAPI-generated control client + connect-go to envd) lands when a
  deployment needs it.

**[GATE G6]** A golden coding turn: run_sandbox suspends, engine restarts mid-run,
resume completes the same turn; clarification park → reseed path; local container +
direct modes both green.

## 12. Phase 7 — Notifications + integrations (L–XL, demand-ordered)

Spec: `src/crewlet/notifications/` + per-vendor packages; suites:
`tests/test_notifications/` (11.5k) + per-vendor suites (Plane 5.8k, Mattermost
4.8k, Slack 1.6k, Confluence 1.6k, GitLab 1.1k, knowledge 1.8k).

- **[BUILD] The backend-neutral spine first** (this is the 30% that carries over):
  raw-webhook envelope + one fleet-wide inbound group, org-derived party resolution
  (HandleRegistry cascade), self-action + own-app guards, per-agent rate valve,
  thread-follow model + MentionGrammar, conversation coalescing with per-source
  supersede rules, WorkingStatusDriver, NotificationPrompt registry, KnowledgeSearcher
  contract (never raises; fail-closed draft hiding; exactly one backend per org).
- **[DECIDED d-701] Vendor order for v1: Mattermost (chat), Plane (tracker +
  knowledge), GitLab (code host)** — the three the example company enables, the
  three with docker-compose profiles, and the three with bootstrap scripts. Spine
  first, then the three in parallel (they share nothing beyond it), parser before
  transport in each. Provisioning CLIs and `doctor` trail the transports. Full
  reasoning: `rewrite/decisions/701-vendor-order.md`.
- **[BUILD]** Per-vendor ports in the decided order, each: parser + routing fidelity
  suite, transport, prompt, provisioning CLI (if in scope), doctor (Mattermost).

**[GATE G7]** The chosen golden company (real vendor sandbox accounts) runs the
quickstart end-to-end on the Go binary.

## 13. Phase 8 — Built-in surfaces (post-v1; design-gated)

Explicitly OUT of v1. Before any code: an architect-tier design doc per surface
(chat first — it is the wake-up channel), covering the genuinely new ground the
Python repo has no answer for: human identity/auth/ACLs, human notification
delivery, ticket lifecycle state machines, page storage/editor, websocket fan-out to
human clients, and the agent-facing write tools registered as native tools (through
the registry seam with code-declared annotations — never loopback MCP). Each surface
slots behind the Phase-7 seams; keep external vendors as config-selected peers.

## 14. Constants

**A. Carry verbatim** (rationale documented at each Python definition site):
delegation depth 3 · round caps 16/20/10, ceilings 32/40/20, extension step 8 ·
sub-agent defaults (20 turns, 120s, 0.2 budget fraction, 3 parallel, 500 min tokens)
· ledger elision budgets (VALUE 200 / BLOB 800 / PLAN 1200 / ARTIFACT 2000 / 12 read
calls) · seat lease TTL 45s, heartbeat 15s, sweep 5s, claims 4 / releases 2 per
sweep, acquire backoff = TTL, undead alarm 300s · watchdog exit code 75, beat/poll ≤
threshold/5 · reconcile 15s ±20% jitter, LAG_GRACE 3, MAX_APPLY 3/epoch, peer
freshness 60s · ledger retention 7d with redelivery-horizon + catchup floors ·
dedupe TTL 300s, maintenance tick 900s (< every retention) · sandbox poll 15s,
connect-failure limit 4, pause TTL 1800s, term grace 5s, setup timeout 600s ·
cli-agent timeout 300s, max_concurrent 4, cooldowns 3600s/300s + 6-doubling auth
backoff, min sandbox budget 2000 / fraction 0.5 · MCP startup 120s / call 300s /
100-page cap / 50-line stderr tail · EVENT_FEED_LIMIT 400 end-to-end, client queue
512, health tick 5s, rollup 1s, ping 25s, reconnect cap 30s, query timeout 10s / 4
concurrent · scheduler tick 10s, catchup clamp [120s, 7200s], per-run cap 180s.

**B. Re-derive per broker with the harness** (never inherit): ack-timeout /
AckWait (Pulsar ships 30 min — sized to wait + worst-case turn) · max-redeliver /
DLQ budget (Pulsar 10 — poison ∧ node-death budget; JetStream may differ, d-102) ·
NAK/redeliver delay (1s) · prefetch / fetch sizing (64 / 200 / 2×batch on Pulsar;
pull consumers change the question) · batch dispatch budget 60s + linger cap 60s
(exists ONLY because Pulsar's ack clock starts at receive — may be deletable).

## 15. Fail-open / fail-closed polarity (per store — deliberate, do not "fix")

OPEN (store unreachable ⇒ proceed): completion ledger (both directions — the safe
answer is the pre-ledger one: run it), webhook dedupe (a duplicate is recoverable, a
dropped delivery is lost work), rate valve, cooldown read-through.
CLOSED (unknown ⇒ refuse/hold): budget spend; lease renew ambiguity = LeaseError
(keep seats, quiesce admission, retry until TTL-elapsed — NEVER treat unknown as
lost); onboarding pass claim (never run a possibly-duplicate pass); secret store
(loud failure — "" would become an empty Bearer token hours later).

## 16. Non-promises (preserve honestly — a rewrite that "fixes" these is wrong)

No exactly-once external side effects — bounded duplication only (a billed sandbox
box can launch before the fenced row). A wedged zombie can act for up to one LLM
round + one heartbeat after losing its lease. Diary rewordings and token-usage rows
may duplicate (budget enforcement reads the shared counter and stays correct).
Singletons stay singletons (the fleet does not parallelize scheduler/curator/waiter).
A rolling upgrade across a protocol bump has a visible claim-freeze window.

## 17. Phase 9 — Root replacement (the complete-replacement commit) (S)

The rewrite ends with replacement, not cohabitation. After G7:

- **[BUILD]** Move the Go tree from `go/` to the repository root; delete the Python
  implementation (`src/crewlet/`, `tests/`, `pyproject.toml`, the Python CI
  workflows). Its job as the specification is done; git history preserves it.
- **[DECIDE d-901]** Release tooling for a Go binary product (e.g. goreleaser:
  platform matrix, checksums, container images) replacing the PyPI
  Trusted-Publishing pipeline; rewrite `CONTRIBUTING.md` / `RELEASING.md` for the Go
  toolchain in the same commit (root process files stay canonical — CLAUDE.md rule).
- **[BUILD]** Sweep `docs/` for Python-implementation references. The architecture
  pages (`docs/concepts/`) largely survive with mechanics updated, and each earlier
  phase already owns its docs section (§2 rule 7), so this is a sweep, not a rewrite.
- Deployments stand up the Go binary **fresh**. Per D15 there is no data migration,
  export tool, or compatibility shim from Python deployments — a clean break,
  accepted and stated in the release notes.
