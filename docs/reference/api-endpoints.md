# API Endpoints

The Crewlet API is a Starlette app. By default it runs **inside** the engine process (`api.port > 0` in the Tier A config); it can also run on its own (`crewlet run api`), talking to the engine over Pulsar. The routes below are identical either way.

Install with the `api` extra: `pip install "crewlet[api]"`

---

## Routes

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check + `configured` flag |
| `GET` | `/agents` | List agent roles, each merged with live state from the in-memory projection (including the in-flight `live_call`). [Human seats](../concepts/humans-in-the-org.md) are excluded — they appear only in `/org` with `"kind": "human"` |
| `GET` | `/agents/{id}` | Single agent — static config + live state (incl. `live_call`) + LLM history |
| `GET` | `/agents/{id}/memory` | Durable memories (personal, episodic, counterparty, synthesized skills) |
| `GET` | `/org` | Full org tree (units, roles — including human seats with `"kind": "human"`) |
| `GET` | `/tools` | Registered tools (builtins + discovered MCP tools) |
| `GET` | `/events` | Recent engine events from the event store (`limit` caps at 500) |
| `GET` | `/events/{event_id}` | Single event incl. payload |
| `GET` | `/events/trace/{trace_id}` | All events in one trace, ordered by timestamp |
| `GET` | `/tokens/breakdown` | Per-stage / model / worker / agent / turn token-spend rollup |
| `GET` | `/schedules` | Configured role/unit schedules + next-run + recent dispatch ledger |
| `GET` | `/stream/snapshot` | Dashboard initial-state bundle, served from the in-memory projection (REST fallback for the WebSocket) |
| `WS`  | `/ws/stream` | Live dashboard stream — agents, events, LLM invocations, health |
| `GET` | `/dashboard` | Dashboard shell (`/` redirects here; `/static/{path}` serves its assets) |
| `POST` | `/webhooks/jira` | Receive Jira webhooks (200-and-drop when unconfigured) |
| `POST` | `/webhooks/slack/{handle}` | Receive Slack webhooks (per-agent) |
| `GET` | `/webhooks/slack-oauth` | OAuth install landing page for `crewlet slack provision` |
| `POST` | `/webhooks/github` | Receive GitHub webhooks |
| `POST` | `/webhooks/gitlab` | Receive GitLab webhooks |
| `POST` | `/webhooks/plane` | Receive Plane webhooks |
| `POST` | `/webhooks/confluence` | Receive Confluence webhooks |
| `POST` | `/webhooks/forge` | Receive Forge events (FIT-verified) |
| `POST` | `/otlp/{token}/v1/{signal}` | Engine-fronted OTLP receiver for [sandbox](../concepts/code-sandbox.md) telemetry (per-run token in the path) |

Read-side handlers live in the `crewlet.api.routes` package (one module
per domain — `agents`, `events`, `tokens`, `org`, `stream`,
`webhooks`, `dashboard`, `health`); `webhooks` and `/config/*` keep a
stable external contract, while the read/stream surface is free to
evolve since the dashboard is its only consumer.

### `/config/*` — live config management (auth-gated)

All `/config/*` routes require `Authorization: Bearer <token>` matching one of the tokens listed in Tier A `api.auth.tokens`. See the [Configuration concept doc](../concepts/configuration.md#auth) for the full auth model.

**Read-only:**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config` | Active revision (full JSON; `?format=yaml` for YAML) |
| `GET` | `/config/revisions` | Paginated history (newest first) |
| `GET` | `/config/revisions/{id}` | Single revision incl. payload |
| `GET` | `/config/revisions/{id}/diff?against=<uuid\|active>` | Structural diff |
| `GET` | `/config/audit?limit=N` | Recent revision metadata (for dashboard / ops scraping) |

**Full-document write:**

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/config` | Replace active revision. Body JSON or `Content-Type: application/yaml`. Requires `X-Summary` header (or top-level `_summary` body key). Optional `If-Match: <revision_id>` for optimistic concurrency (`If-Match: none` for the unconfigured case). |
| `POST` | `/config/revisions/{id}/revert` | Create a new active revision whose payload equals revision `{id}` |

**Per-entity convenience CRUD** — all funnel through `RevisionDispatcher.write_entity_patch`; all return `409 company_not_initialised` when the engine is unconfigured:

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/config/identity` | Replace `{name, mission, vision, policies}` |
| `GET`/`POST`/`PUT`/`DELETE` | `/config/roles[/{handle}]` | CRUD root + nested roles. `POST` body may set `unit: <name>` so the role lands inside the named unit at engine build time (inheriting that unit's `mcp_env`). |
| `GET`/`POST`/`PUT`/`DELETE` | `/config/units[/{name}]` | CRUD nested org units |
| `GET`/`PUT`/`DELETE` | `/config/llm-providers[/{key}]` | CRUD provider entries |
| `PUT` | `/config/embeddings` | Replace embedding provider |
| `GET`/`POST`/`PUT`/`DELETE` | `/config/mcp-servers[/{name}]` | CRUD MCP servers |
| `PUT` | `/config/integrations/{jira\|confluence\|slack\|github\|gitlab\|plane}` | Replace one integration block |
| `GET`/`POST`/`DELETE` | `/config/notification-transports[/{name}]` | CRUD outbound transports |
| `PUT` | `/config/turn-engine` | Replace `TurnEngineConfig` |
| `PUT` | `/config/learning` | Replace `LearningConfig` |
| `PUT` | `/config/budgets` | Replace org `token_budget` |
| `GET`/`POST`/`DELETE` | `/config/extensions[/{name}]` | CRUD extensions |

### Status codes

- `200 OK` — successful read
- `201 Created` — write produced a new revision; body has `{"revision_id": ..., "summary": ...}`
- `400 Bad Request` — invalid body / validation error (Pydantic detail in `detail`); `summary_required` if `X-Summary` missing on full PUT
- `401 Unauthorized` — missing or invalid bearer token (`{"error": "invalid_token"}`)
- `404 Not Found` — `no_active_revision` (read on unconfigured) or revision not found
- `409 Conflict` — `revision_advanced` (stale `If-Match` or TOCTOU race with a concurrent writer) or `company_not_initialised` (per-entity write on unconfigured)
- `412 Precondition Failed` — `If-Match` supplied when no revision exists yet

### `GET /config/audit`

Recent revision metadata for the dashboard Configuration tab and ops-side audit scraping. Read-only; returns the same revision records as `GET /config/revisions` but in a wrapper object suited to ops tools that key on a top-level shape.

```
GET /config/audit?limit=<N>
```

| Query parameter | Default | Range | Description |
|-----------------|---------|-------|-------------|
| `limit` | `50` | `1..500` | Number of revisions to return, newest first. Out-of-range returns `400 invalid_limit`. |

Response (`200 OK`):

```json
{
  "revisions": [
    {
      "revision_id": "11111111-1111-1111-1111-111111111111",
      "parent_revision_id": "00000000-0000-0000-0000-000000000000",
      "created_at": "2026-05-17T10:31:02.118431+00:00",
      "created_by": "founder",
      "source": "api",
      "summary": "add Designer role",
      "is_active": true,
      "activated_at": "2026-05-17T10:31:02.118431+00:00"
    }
  ]
}
```

Payloads are NOT included — fetch a specific revision via `GET /config/revisions/{id}` for the full JSON.

---

## Live Stream

`/ws/stream` is the dashboard's **only** data channel. State comes down
it and requests go up it, so a running dashboard makes no HTTP request
at all: the handshake snapshot carries every section a screen needs on
first paint, subsequent pushes carry what changed, and anything fetched
on demand — an agent's LLM history, one event's payload, a trace, a
different spend window, the configuration document — is a query sent on
the same socket and answered on it.

The REST endpoints below remain a public read API, and
`GET /stream/snapshot` is still the fallback for a browser that cannot
upgrade to a WebSocket (corporate proxies). They are no longer part of
the dashboard's normal operation.

### Live-state projection (`StreamService` + `LiveState`)

The API process maintains an **in-memory projection** of every agent's
current state (`crewlet.api.live_state.LiveState`, owned by
`crewlet.api.streaming.StreamService`).  It is fed by the same event
stream the WebSocket fan-out consumes, hydrated once from the event
store at startup, and read in O(1) thereafter — so `/agents`,
`/stream/snapshot`, and the WebSocket handshake never re-derive
state from a multi-query event scan on a request.

What the projection computes, it also **pushes**. A dashboard mirrors
it rather than re-deriving it: before this, every tab ran its own copy
of the state machine below, its own sandbox tracking, and its own
re-implementation of the spend aggregation, all applied to the raw event
stream — three copies of server logic, three ways to drift, and a
refresh that regularly disagreed with what had been on screen a moment
earlier. Applying an event now yields a change set, and the changed
agents' overlays, the sandbox list, and the spend rollup go out as their
own envelopes.

Crucially, the projection holds each agent's **in-flight LLM call** —
the latest `agent_turn_progress` (phase, round, model, accumulated
response, tool calls so far) — and surfaces it as a `live_call` field on
the agent in every snapshot.  `agent_turn_progress` is *stream-only*
(never written to the event store); carrying `live_call` in the
snapshot means a tab that refreshes or reconnects mid-call re-renders
the live row immediately instead of waiting for the next progress
event.  State transitions in the projection are
gated on the event timestamp so out-of-order delivery (the standalone
API's Pulsar topics order only *within* a topic) can't clobber newer
state with an older event.

A **failed** phase is a first-class part of this. When a phase dies, the
projection stamps its in-flight call `failed`, keeps it on screen rather
than clearing it, and records the classified cause on the agent as
`last_error` — so a seat that stopped can say *why*, and the call it
stopped on is still there to read.

The agent detail page streams the same way: it paints from a `agent`
query, then keeps itself current from the pushes.
`agent_phase_completed` / `agent_turn_completed` envelopes append to the
LLM Invocations list live, and the agent's own `live_call` — pushed on
every round — is the in-flight row inside its turn card, replaced by the
completed record when the phase finishes. Progress envelopes carry
`turn_id` / `phase` / `iteration` for this correlation; they are
stream-only and never persisted to the event store.

The **spend rollup** is maintained by the projection too, over the same
window per-agent totals hydrate over, using the same
`aggregate_phase_events` the REST endpoint calls. It ships in the
snapshot and is re-pushed (coalesced to at most one frame per second)
whenever a phase completes, so the Tokens view and the overview widget
stay live without a fetch and without a second implementation of the
aggregation in the browser.

### `GET /stream/snapshot`

Single-shot bundle equivalent to the WebSocket handshake's first
envelope: every section a dashboard screen needs on first paint.
Assembled entirely from the in-memory projection — no database
round-trip on the hot path.  Used as a fallback when the browser cannot
upgrade to a WebSocket (corporate proxies, etc.).

**Response**

```json
{
  "health":    { "status": "ok", "in_flight": 0, "shutting_down": false },
  "agents":    [ { /* /agents row: live state + tokens + live_call (the
                      in-flight LLM call, or null between turns) +
                      last_error (the phase failure that stopped this
                      seat, or null) */ }, ... ],
  "events":    [ { /* recent event row, newest first — payload-free, plus
                      a `failed` boolean */ }, ... ],
  "sandboxes": [ { /* in-flight detached coding run */ }, ... ],
  "tools":     [ { "name": "...", "description": "...", "source": "..." } ],
  "org":       { /* /org payload */ },
  "tokens":    { /* the spend rollup — same shape as /tokens/breakdown */ },
  "schedules": [ { /* configured schedule + computed next_run */ }, ... ]
}
```

Each `events` row is the payload-free feed shape — `id`, `type`,
`timestamp`, `source`, `actor`, `summary`, `category`, `trace_id`,
`span_id`, `parent_span_id`, `topic` — plus **`failed`**: `true` when the
work the event reports did not succeed.  It is `true` for an event carrying
its own `failed` field (a phase or turn that died) and for an event type that
*is* a failure (`task_failed`, `llm_unavailable`, `budget_exhausted`,
`turn.guard_breach`).  Deciding it once, here, is what lets a dashboard mark
failures without re-deriving them from a type list of its own.

The flag survives a restart: the event-store writer stamps a `failed` tag on
those events, and the projection reads it back when it hydrates its feed from
history.  `list_events` deliberately never selects the payload column, so
without the tag every historical failure would read back as a success.

Each agent's `live_call` is `null` between turns, or
`{ turn_id, phase, iteration, model, response, tool_executions, rounds,
in_progress }` while an LLM call is under way.  A call whose phase
failed keeps `in_progress: false` plus `failed: true` and an `error`
object, so the dashboard renders the failure instead of an answer that
never arrives.

### `WS /ws/stream`

Upgrades to a WebSocket.  All frames are JSON envelopes of the form
`{"kind": "...", "data": ..., "ts": "<iso8601>"}`.

**Server → client kinds**

| `kind` | When | `data` |
|--------|------|--------|
| `snapshot` | First envelope after the upgrade succeeds, and again on reconnect. | Same payload as `GET /stream/snapshot` — agents carry their in-flight `live_call`, so a reconnect re-renders the live row. |
| `event`    | Every engine event published to `crewlet.events.>`. | `{ id, type, timestamp, source, actor, summary, category, trace_id, span_id, parent_span_id, topic, payload }` — the same shape as a `/events` row, plus the full event `payload` (from which the snapshot feed's `failed` flag is derived).  `agent_phase_completed` events carry the system prompt, response, and tool calls, so LLM invocations stream live; `agent_turn_progress` events (per tool-call round, tagged with `turn_id` / `phase` / `iteration`) stream the in-flight call before its phase record exists. |
| `agents`   | After an event moved one or more agents. | The changed agents' overlays, each with its `role` — the *result* of applying the event, so a client merges them rather than running its own state machine over the raw stream. |
| `seats`    | After a config revision changed the roster. | The COMPLETE seat list, replacing what the client holds. Distinct from `agents` on purpose: that one is a per-role merge, and a merge cannot express the deletion of a role a revision removed. |
| `sandboxes`| After a detached sandbox run started, asked a question, or finished. | The full in-flight sandbox list. |
| `tokens`   | After a phase completed, coalesced to at most one per second. | The spend rollup, same shape as `GET /tokens/breakdown`. |
| `org` / `tools` / `schedules` | After a config revision is activated. | The new org tree / tool surface / schedule list, so open tabs stop showing seats that no longer exist. |
| `health`   | Pulsed every 5s by a **single shared tick** (one timer for all clients, not one per connection) with the engine's `in_flight_count` and `shutting_down` flag. `shutting_down` flips `true` (and `status` reads `"shutting_down"`) from the first moment of a graceful stop, so the dashboard shows the drain while it happens — the API server itself keeps serving until the engine has fully stopped. | `{ status, in_flight?, shutting_down? }` |
| `result`   | Reply to a client `query` that succeeded. | `{ id, what, data }` — `id` echoes the request's. |
| `error`    | Reply to a client `query` that could not be answered. | `{ id, what, error }` where `error` is a code: `not_found`, `unauthorized`, `unknown_query`, `no_event_store`, `query_failed`. |
| `pong`     | Reply to a client `ping`. | `null` |

**Client → server kinds**

| `kind` | Purpose |
|--------|---------|
| `ping` | Keepalive; server replies with `pong`. |
| `query` | Request one thing, answered with exactly one `result` or `error` frame. `{ kind, id, what, params, token? }` — `id` is any client-chosen value echoed back on the reply, and `token` carries the operator bearer token that the `config`-family queries require (validated with the same constant-time comparison the `/config` middleware performs). Queries run concurrently with each other and with the push stream, so one database read cannot stall a tab's live rows. |

**Queries** (`what`), each answered by the *same* function the matching
REST route calls, so the two surfaces cannot diverge:

| `what` | `params` | Answers with |
|--------|----------|--------------|
| `agent` | `{id}` | `GET /agents/{id}` — config + live state + `llm_history` |
| `agent_memory` | `{id}` | `GET /agents/{id}/memory` |
| `event` | `{id}` | `GET /events/{id}` — one event with its full payload |
| `events` | `{limit, type, source, trace_id, actor, related_agent}` | `GET /events` |
| `trace` | `{trace_id}` | `GET /events/trace/{trace_id}` |
| `tokens` | `{since_days, agent_role, recent_turns}` | `GET /tokens/breakdown` — for a window other than the live one |
| `schedules` | — | `GET /schedules` |
| `config` | — | `GET /config` *(operator token required)* |
| `config_audit` | `{limit}` | `GET /config/audit` *(operator token required)* |
| `config_diff` | `{revision_id}` | `GET /config/revisions/{id}/diff` *(operator token required)* |

### Wiring

Both deployment paths feed events into a single `StreamService.ingest`
entry point.  The standalone API process subscribes to the engine's
Pulsar event stream with an **ephemeral broadcast consumer** (it receives
every event — this is a broadcast, not a work queue); the
embedded API path (engine + API in the same process) wires `ingest` as
a publish listener directly, no queue round-trip required.  Each event
updates the live-state projection *and* fans out to connected
dashboards.  Backpressure is per-WebSocket: a stalled tab drops the
oldest queued envelope so it cannot stall the publish path or other
tabs.

The dashboard itself is a zero-build, modular ES-module app served from
`crewlet/static/dashboard/` (`index.html` shell + `styles/*.css` +
`js/**` modules — a store that mirrors the projection, a reconnecting
WebSocket client with heartbeat, query channel and REST-snapshot
fallback, a keyed DOM patcher, a frame-coalescing render scheduler, a
hash router so a refresh keeps your current view, and one view module
per screen).  `/dashboard` serves the shell; `/static/{path}` serves its
assets.  Its visual system —
the token layer, the shared panel recipe, and the categorical hues — is
documented in [Dashboard Design System](dashboard-design.md).

---

## Agent Memory

### `GET /agents/{id}/memory`

Returns the four durable memory surfaces produced by the
[agent-learning subsystem](../concepts/agent-learning.md) for one agent,
combined into one JSON response so the dashboard can render the agent's
"memory" view in a single round trip.

```json
{
  "agent_id": "<static config id>",
  "handle": "<agent handle>",
  "role": "<role name>",
  "runtime_id": "<live AgentInstance UUID, when known>",
  "personal_memories": {
    "long":  [{ "id", "content", "metadata", "ttl_until" }, ...],
    "short": [{ "id", "content", "metadata", "ttl_until", "expired" }, ...]
  },
  "episodes": [
    { "id", "task_summary", "plan_summary", "tool_sequence",
      "skills_used", "review_outcome", "started_at", "ended_at",
      "duration_ms", "task_id", "turn_id" }, ...
  ],
  "counterparty_profiles": [
    { "subject_label", "subject_handle", "subject_external_id",
      "subject_platform", "subject_name", "traits",
      "interaction_count", "first_seen_at", "last_updated_at",
      "last_corroborated_at" }, ...
  ],
  "synthesized_skills": [
    { "id", "name", "description", "content",
      "tool_sequence", "version", "created_at", "updated_at" }, ...
  ]
}
```

Sources:

* **`personal_memories`** — knowledge documents at `AGENT` scope with
  metadata `kind=memory_long` or `kind=memory_short`, written by the
  `PersistDecider` and `reflect_and_persist` builtin. SHORT entries past
  their `ttl_until` are returned with `expired=true` so the dashboard
  can show them grayed out rather than hide them silently.
* **`episodes`** — recent rows from the `episodes` table (one per
  completed turn).  Ordered by `ended_at` descending, capped at 50.
* **`counterparty_profiles`** — rows from `counterparty_profiles`
  observed by this agent, ordered by `last_updated_at` descending.
* **`synthesized_skills`** — the agent's own rows from
  `synthesized_skills` via `SynthesizedSkillStore.list_for_agent`. The
  table is strictly per-agent; cross-agent procedural artefacts are
  [promoted](../concepts/agent-learning.md) as draft pages in the shared
  knowledge backend, reachable by all members via query-time search.

Each section degrades independently: a missing knowledge provider, a
non-Postgres storage backend, or a per-section query error returns an
empty list for that section instead of erroring the whole response.
Returns 404 when no role with the given `id` is configured.

---

## Schedules

### `GET /schedules`

Backs the dashboard's **Schedules** view. Returns every configured
role/unit [schedule](../concepts/scheduling.md) with its cron, effective
timezone, target → resolved runner handles, and a per-request `next_run`
(computed from the cron), plus the most recent rows from the
`scheduled_runs` dispatch ledger.

```json
{
  "schedules": [
    {
      "scope_type": "unit", "scope_id": "Backend", "name": "daily-standup",
      "cron": "30 9 * * 1-5", "timezone": "Europe/Amsterdam",
      "task": "Post your standup…", "target": "each",
      "enabled": true, "timeout_seconds": 180, "catchup": true,
      "runners": ["backend-lead", "backend-dev"],
      "next_run": "2026-06-09T07:30:00+00:00"
    }
  ],
  "recent_runs": [
    {
      "scope_type": "unit", "scope_id": "Backend",
      "schedule_name": "daily-standup", "target_handle": "backend-dev",
      "scheduled_at": "2026-06-08T07:30:00+00:00",
      "fired_at": "2026-06-08T07:30:02+00:00", "outcome": "fired"
    }
  ]
}
```

`recent_runs` is empty when no database is configured (the configured
list and `next_run` still render). Disabled schedules return an empty
`next_run`.

---

## Token Spend Breakdown

### `GET /tokens/breakdown`

Rolls up per-phase LLM spend across the whole org so the dashboard's
**Tokens** view can render every breakdown from a single fetch.
Reads `agent_phase_completed` events via
`EventStore.list_phase_token_events` and groups them by phase, model,
auxiliary worker, agent, and turn.

**Query parameters**

| Name | Default | Description |
|------|---------|-------------|
| `since_days` | `7` | Window in days. Clamped to `[1, 30]` — the event store keeps 30 days. |
| `agent_role` | (none) | Restrict to one role. Used by the agent detail page's per-phase summary. |
| `recent_turns` | `50` | Cap on the per-turn list. |

**Response**

```json
{
  "since_days": 7,
  "agent_role": "",
  "totals": {
    "input_tokens": 17700, "output_tokens": 2750,
    "total_tokens": 20450, "calls": 6
  },
  "by_phase": [
    { "phase": "execute", "input_tokens": 14000, "output_tokens": 2000,
      "total_tokens": 16000, "calls": 2 },
    { "phase": "plan", "input_tokens": 1700, "output_tokens": 450,
      "total_tokens": 2150, "calls": 2 },
    ...
  ],
  "by_model": [
    { "model": "claude-sonnet-5", "input_tokens": 16700,
      "output_tokens": 2600, "total_tokens": 19300, "calls": 4 },
    ...
  ],
  "by_worker": [
    { "worker": "persist_decider", "input_tokens": 800,
      "output_tokens": 100, "total_tokens": 900, "calls": 1 }
  ],
  "by_agent": [
    { "role": "PM", "handle": "pm", "agent_id": "<runtime uuid>",
      "input_tokens": 17500, "output_tokens": 2700,
      "total_tokens": 20200, "calls": 5,
      "by_phase": {
        "plan":      { "input_tokens": 1500, "output_tokens": 400,  "total_tokens": 1900, "calls": 1 },
        "execute":   { "input_tokens": 14000,"output_tokens": 2000, "total_tokens": 16000,"calls": 2 },
        "review":    { "input_tokens": 1200, "output_tokens": 200,  "total_tokens": 1400, "calls": 1 },
        "auxiliary": { "input_tokens": 800,  "output_tokens": 100,  "total_tokens": 900,  "calls": 1 }
      }
    },
    ...
  ],
  "by_turn": [
    { "turn_id": "<uuid>", "role": "PM", "handle": "pm",
      "agent_id": "<runtime uuid>",
      "started_at": "...", "ended_at": "...",
      "input_tokens": 17500, "output_tokens": 2700,
      "total_tokens": 20200, "calls": 5,
      "by_phase": { "plan": {...}, "execute": {...}, ... } },
    ...
  ],
  "aggregated_through": "2026-06-21T10:00:30+00:00"
}
```

Notes:

- `by_phase` covers every phase emitted by the
  [Turn Engine](../concepts/turn-engine.md): `plan`, `execute`,
  `review`, `subagent`, `auxiliary`, and `judge` (the round-cap
  extension judge).
- `by_worker` covers only `phase == "auxiliary"` rows — the worker
  name identifies the learning-subsystem caller (e.g.
  `persist_decider`, `counterparty_profiler`, `skill_synthesizer`).
- `by_model` is useful when roles override `llm_auxiliary` with a
  cheaper model for reflection / summarisation work.
- All lists are sorted by `total_tokens` descending; `by_turn` is
  sorted by `ended_at` descending and capped at `recent_turns`.
- `aggregated_through` is the latest event timestamp this rollup
  aggregated (empty when no events matched). It is a **live-folding
  watermark**: this endpoint is a one-shot snapshot, so the dashboard
  treats the response as a baseline and folds subsequent
  `agent_phase_completed` events streamed over `/ws/stream` onto it,
  skipping any event at or before the watermark (already counted) to
  avoid double-counting. This is why the **Token Spend** widget keeps
  climbing in lock-step with the per-agent rows instead of freezing at
  the value of the initial fetch.
- Returns the same skeleton with zero totals (and an empty
  `aggregated_through`) when the event store is unavailable rather than
  erroring.

---

## Webhook Endpoints

### `/webhooks/jira`

Receives Jira webhook payloads (issue created, updated, commented). Verifies HMAC-SHA256 signature if `webhook_secret` is configured. Publishes parsed events to `crewlet.notifications.inbound`.

### `/webhooks/slack/{handle}`

Receives Slack Events API payloads for a specific agent (identified by handle). Verifies the signing secret for that agent's Slack app. Publishes to `crewlet.notifications.inbound`. Slack's `url_verification` challenge is answered unconditionally (no engine or company config needed), so a freshly provisioned app's Request URL verifies even before the engine is configured.

### `GET /webhooks/slack-oauth`

The OAuth install landing page for [`crewlet slack provision`](cli.md#crewlet-slack-provision) — every provisioned Slack app has this as its OAuth redirect URL. After the operator approves an install, Slack redirects here with a temporary `code` (and `state` carrying the agent handle); the page displays the code for pasting back into the waiting CLI prompt. Unauthenticated by design: the code expires after 10 minutes and is useless without the app's client secret, which only the provisioning CLI holds.

### `/webhooks/github`

Receives GitHub webhook payloads. Verifies HMAC-SHA256 signature via the `x-hub-signature-256` header using the required `webhook_secret` from the `github` config block. Invalid or missing signatures are rejected with 401. Returns 500 if the server has no `webhook_secret` configured. Publishes to `crewlet.notifications.inbound`.

### `/webhooks/gitlab`

Receives GitLab webhook payloads. Verification is the GitLab 19.1+ signing token **only**: the `webhook-signature` header is verified as a Standard-Webhooks HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}` (±5-minute timestamp tolerance) against the configured `signing_secret`; the plain `X-Gitlab-Token` scheme is unsupported. Invalid or missing signatures are rejected with 401. Returns 500 if no `signing_secret` is configured. Publishes to `crewlet.notifications.inbound`. See [GitLab Integration — Webhooks](../integrations/gitlab.md#webhooks).

### `/webhooks/plane`

Receives Plane webhook payloads from the [Plane fork](../integrations/plane.md). The `X-Plane-Signature` header is verified as the HMAC-SHA256 hexdigest of the raw body keyed with `integrations.plane.webhook_secret` (constant-time compare; Plane CE's only scheme). Invalid or missing signatures are rejected with 401; returns 500 if no secret is configured; malformed JSON returns 400. When the engine is unconfigured, a *verified* request is answered 200 `{"status": "dropped"}` — verification runs first so forgeries never earn a 200, while genuine deliveries don't poison Plane's five-retry auto-disable counter. Publishes to `crewlet.notifications.inbound`. See [Plane Integration — Webhooks](../integrations/plane.md#webhooks).

### `/webhooks/confluence`

Receives Confluence webhook payloads (page created/updated, comments). Publishes to `crewlet.notifications.inbound`.

### `/webhooks/forge`

Receives events from the Atlassian Forge app. Every request must carry a Forge Invocation Token (FIT) as an `Authorization: Bearer` JWT; the token is verified against Atlassian's JWKS endpoint and its `aud` claim must match the configured `forge_app_id` (401 on failure, 500 when no app id is configured). The request body is drained **before** FIT verification — verification can block on a JWKS fetch, and the body must be off the socket before the sender's delivery deadline aborts the request. Maps `avi:jira:*` / `avi:confluence:*` events onto the native Jira/Confluence pipeline and publishes to `crewlet.notifications.inbound`. Self-generated events (an agent's own actions echoed back by Forge) are acknowledged and dropped.

### Aborted deliveries (client disconnects)

Webhook senders enforce delivery deadlines and abort requests that respond too slowly. When a sender hangs up before the request body is fully read, Starlette raises `ClientDisconnect`; the API handles this app-wide with a structured `client_disconnected` warning (`component=api.app`, keyed by `method`, `path`, `remote`) instead of letting it escape as an unhandled ERROR traceback. The aborted delivery is dropped — whether it is redelivered is up to the sender's retry policy — so recurring `client_disconnected` warnings on a webhook path mean events are being lost because the API is answering too slowly.

---

## Running

```bash
crewlet run api config.yaml --host 0.0.0.0    # binds port 8000 by default
```

The API is read-only against the database and publishes commands/notifications to Pulsar. It does not run agents — the engine process handles that.

See [Deployment](../guides/deployment.md) for how the API and engine run together, and the integration docs ([Slack](../integrations/slack.md), [Jira](../integrations/jira.md)) for webhook setup.
