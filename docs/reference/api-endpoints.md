# API Endpoints

The Crewlet API is served by any node with the `ingress` role (`api.port > 0`
in the Tier A config). By default a node has every role, so it runs alongside
the agents in one process; a node with `-roles ingress` serves only these
routes and reaches the rest of the fleet over the event stream. The routes
below are identical either way.

There is nothing to install for it — it is compiled into the binary, along
with the dashboard it serves and the WebSocket that is the dashboard's data
plane (see [`WS /ws/stream`](#ws-wsstream)).

---

## Routes

| Method | Path | Description |
|--------|------|-------------|
> **Auth.** Writes and every `/config` route require
> `Authorization: Bearer <token>`. Reads (`GET` / `HEAD` outside `/config`)
> serve without one unless `api.auth.allow_anonymous_read: false` is set, at
> which point they need the same token — `/ws/stream` included, and it accepts
> `?token=…` too since browsers cannot set headers on a WebSocket. Never
> guarded either way: `/health`, `/ready`, `/webhooks/*`, `/otlp/*`, and the
> dashboard shell (`/`, `/dashboard`, `/static/*`). See
> [Configuration § Auth](../concepts/configuration.md#auth).
>
> **The guard is always mounted**, whether or not Tier A is present. An API
> built without `api.auth` configuration has no token, and a route that needs
> one is therefore refused rather than served: reads work, every write and the
> whole `/config` surface answers `401`. There is no way to start a process
> that serves `/config` writes without a guard in front of them.
>
> **Every `/webhooks/*` route fails closed.** They are exempt from the bearer
> token because each verifies its provider's signature instead — so a route
> whose secret is not configured has nothing to verify with, and answers `503`
> with `Retry-After` rather than accepting the delivery. The sender retries and
> the delivery flows once the secret is set; nothing is discarded, and nothing
> unsigned is ever recorded, published, or shown on the dashboard.

| `GET` | `/health` | Liveness + the engine-health envelope (see [below](#the-health-envelope)). Stays `200` through a drain — use `/ready` to steer traffic |
| `GET` | `/ready` | Readiness for a load balancer: `503` while draining or before the first config revision applies, `200` otherwise |
| `GET` | `/agents` | List agent roles, each merged with live state from the in-memory projection (including the in-flight `live_call`). [Human seats](../concepts/humans-in-the-org.md) are excluded — they appear only in `/org` with `"kind": "human"` |
| `GET` | `/agents/{id}` | Single agent — static config + live state (incl. `live_call`) + LLM history |
| `GET` | `/agents/{id}/memory` | Durable memories (personal, episodic, counterparty, synthesized skills) |
| `GET` | `/org` | Full org tree (units, roles — including human seats with `"kind": "human"`) |
| `GET` | `/tools` | Registered tools, each tagged with the `source` that registered it — `builtin` or `mcp:<server>` (see [Where a tool comes from](../guides/tools-and-mcp.md#where-a-tool-comes-from)) |
| `GET` | `/events` | Recent engine events from the event store (`limit` caps at 500; keyset-paged, see below) |
| `GET` | `/events/{event_id}` | Single event incl. payload |
| `GET` | `/events/trace/{trace_id}` | All events in one trace, oldest first, capped at 500 |
| `GET` | `/tokens/breakdown` | Per-stage / model / worker / agent / turn token-spend rollup |
| `GET` | `/schedules` | Configured role/unit schedules + next-run + recent dispatch ledger |
| `GET` | `/fleet` | Every live node, its roles and labels, seat ownership, singleton duties, and per-node config epoch |
| `GET` | `/sandbox-runs` | Every detached [sandbox](../concepts/code-sandbox.md) run the engine still holds, read from the durable `pending_sandbox_run` row (see [below](#get-sandbox-runs)) |
| `GET` | `/budgets` | Token caps, the durable shared counter they are enforced against, and which scopes are being refused (see [below](#get-budgets)) |
| `POST` | `/budgets/reset` | Zero the fleet's token counter. `?scope=` clears one (`org`, `agent:<id>`); its absence clears every one. **Always needs a token** — a write is a write whatever `allow_anonymous_read` opens (see [below](#post-budgetsreset)) |
| `GET` | `/conversations` | What a seat already said in each thread / issue / pull request it works — the same [conversation-session](../concepts/conversation-sessions.md) rows the engine renders into that conversation's next turn. `?handle=` lists them; `?handle=&key=` returns one conversation's entries. `available: false` means this node cannot see the ledger, never that the seat has said nothing |
| `GET` | `/integrations` | Every inbound surface, how it is wired, whether a signing secret is present, and what has arrived through it (see [below](#get-integrations)) |
| `GET` | `/stream/snapshot` | Dashboard initial-state bundle, served from the in-memory projection (REST fallback for the WebSocket) |
| `WS`  | `/ws/stream` | Live dashboard stream — agents, events, LLM invocations, health |
| `GET` | `/dashboard` | Dashboard shell (`/` redirects here; `/static/{path}` serves its assets) |
| `POST` | `/webhooks/jira` | Receive Jira webhooks — **inert**: the config block is refused, so 503 |
| `POST` | `/webhooks/slack/{handle}` | Receive Slack webhooks (per-agent) — **inert**: the config block is refused, so 503 |
| `GET` | `/webhooks/slack-oauth` | OAuth install landing page for `crewlet slack provision` |
| `POST` | `/webhooks/github` | Receive GitHub webhooks — **inert**: the config block is refused, so 503 |
| `POST` | `/webhooks/gitlab` | Receive GitLab webhooks |
| `POST` | `/webhooks/plane` | Receive Plane webhooks |
| `POST` | `/webhooks/confluence` | Receive Confluence webhooks — **inert**: the config block is refused, so 503 |
| `POST` | `/webhooks/forge` | Receive Forge events (FIT-verified) |
| `POST` | `/otlp/{token}/v1/{signal}` | Engine-fronted OTLP receiver for [sandbox](../concepts/code-sandbox.md) telemetry (per-run token in the path) |

Read-side handlers live in the `internal/api` package (one module
per domain — `agents`, `events`, `tokens`, `org`, `fleet`,
`sandbox_runs`, `budgets`, `integrations`, `stream`, `webhooks`,
`dashboard`, `health`);
`webhooks` and `/config/*` keep a stable external contract, while the
read/stream surface is free to evolve since the dashboard is its only
consumer.

### `/config/*` — live config management (auth-gated)

All `/config/*` routes require `Authorization: Bearer <token>` matching one of the tokens listed in Tier A `api.auth.tokens`. See the [Configuration concept doc](../concepts/configuration.md#auth) for the full auth model.

**Read-only:**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config` | Active revision, redacted (full JSON; `?format=yaml` for YAML) |
| `GET` | `/config/revisions` | Paginated history (newest first), metadata only |
| `GET` | `/config/revisions/{id}` | Single revision including its payload |
| `GET` | `/config/revisions/{id}/diff?against=<uuid\|active>` | Structural diff |

The dashboard reads the same four facts over the query channel rather than
these routes — `config`, `config_audit`, `config_diff` and `config_entities`,
each operator-gated for the same reason the prefix is.

**Full-document write:**

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/config` | Replace the active revision. Body JSON or `Content-Type: application/yaml`. Requires an `X-Summary` header. Optional `If-Match: <revision_id>` for optimistic concurrency (`If-Match: none` for the unconfigured case). |
| `POST` | `/config/revisions/{id}/revert` | Create a new active revision whose payload equals revision `{id}` |

**Per-entity write** — four collections, `PUT` only:

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/config/roles/{handle}` | Replace one seat, wherever it lives — root-level or inside a unit, at any depth |
| `PUT` | `/config/units/{name}` | Replace one org unit |
| `PUT` | `/config/llm-providers/{key}` | Replace one named LLM provider |
| `PUT` | `/config/mcp-servers/{name}` | Replace one MCP server entry |

Why these exist beside the whole-document write: `PUT /config` makes every edit
a company-wide one. A founder renaming one seat's goal sends back a document
carrying every other seat, every provider and every integration, and a
concurrent edit anywhere in it is theirs to lose. Editing one entity narrows
what a write *claims* to have changed, which is what makes the revision summary
mean something.

It is the same write underneath, and that matters more than the convenience:
an entity `PUT` opens the active revision, splices the entity in, restores the
masks the read showed against that same revision, **validates the whole
document**, and stores a new revision. A change that would leave the company
invalid is refused even when the entity itself is fine — a seat naming a
provider that no longer exists is exactly the break a per-entity surface
invites, because the caller never sees the rest of the document.

Three rules follow from that:

- **A `PUT` never creates.** An id nothing carries is `404 no_such_entity`, not
  a new entity: naming one that is not there is far more often a typo than an
  intent to add one, and creating through this route would grow the company
  without the caller ever seeing the document they changed. Add through
  `PUT /config`, which shows the whole thing.
- **The id in the path is the identity.** A body that renames the entity is
  stored as-is under that path's id; the URL is the address, not a rename
  request.
- **The same `X-Summary` and `If-Match` rules apply**, and a node with no
  active revision answers `409 no_active_revision` — there is nothing to splice
  into, and building a company out of one seat is not what this route is for.

### Status codes

- `200 OK` — successful read
- `201 Created` — a write produced a new revision; body has `{"revision_id": ..., "epoch": ...}`. A per-entity write returns this too: it changed one entity and created one revision.
- `400 Bad Request` — invalid body / validation error (`error` names which, and `detail` carries the field path and what to change); `summary_required` when `X-Summary` is missing on any write
- `401 Unauthorized` — missing or invalid bearer token (`{"error": "invalid_token"}`)
- `404 Not Found` — a revision that is not there, or `no_such_entity` on a per-entity write naming an id the active revision does not carry
- `409 Conflict` — `revision_advanced` (stale `If-Match`, or a race with a concurrent writer) or `no_active_revision` (per-entity write on an unconfigured node)
- `412 Precondition Failed` — `If-Match` supplied when no revision exists yet

### The `config_audit` query

Recent revision metadata for the dashboard's Configuration room. **A query, not a REST route** — there is no `GET /config/audit` in this build; the room asks the query channel for `config_audit` and gets the same revision records `GET /config/revisions` serves, in a wrapper object.

```
query config_audit { "limit": <N> }
```

| Parameter | Default | Range | Description |
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

### What the handshake snapshot carries

One frame, `kind: "snapshot"`, with every section a screen needs on first
paint. Three of them are derived from **configuration** rather than from
anything that has happened, and they are present from the moment the socket
opens — before a single turn has run:

| Section | What it is |
|---|---|
| `agents` | The company's agent seats, each merged with its live overlay. Every seat in the company, not the ones this node runs, because the dashboard is a view of the company. Human seats are excluded — they have no turn, no phase and no spend; they appear in `org` with `"kind": "human"` |
| `org` | The role and unit tree, **verbatim** as the company document holds it: root-level `roles` plus `units` nesting to any depth. The client walks it and reads the config's own field names, so it is not reshaped on the way out |
| `tools` | The catalogue this node serves, each entry tagged with the `source` that registered it — `builtin` or the MCP server's name. Absent on a standalone API, which has no engine to ask |
| `events`, `sandboxes`, `tokens`, `budget`, `health` | The live projection: what has happened |

A seat carries `state: "idle"` when **this node** is serving it. A seat it
does not hold carries no state at all and the dashboard reads that as
`offline` — which is right for a seat nothing has claimed, and is this node
declining to claim knowledge of a seat a peer may be running. [Fleet](#fleet)
answers "who holds what" from the lease table, which is the one place that
knows.

The three config-derived sections are **re-sent on every config apply**, as
`seats`, `org` and `tools` pushes. Nothing else would correct them: a
revision that adds, renames or removes a role produces no event a projection
could learn from, and an overlay merge cannot express a row going away.

### Live-state projection (`api/stream` + `LiveState`)

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
response *including the model's reasoning*, tool calls so far) — and
surfaces it as a `live_call` field on the agent in every snapshot.  Its
`response` is built by the same function that builds the finished
phase record's, so the live row and the turn you expand afterwards are
the same text rather than two assemblies of it — see [Turn Engine §
What streams during a
turn](../concepts/turn-engine.md#what-streams-during-a-turn).
Each phase publishes an opening
`agent_turn_progress` (`round_num = -1`) before its first provider call
carrying the prompt, so the live row shows what the agent was asked while
it is still answering.  `agent_turn_progress` is *stream-only*
(never written to the event store); carrying `live_call` in the
snapshot means a tab that refreshes or reconnects mid-call re-renders
the live row immediately instead of waiting for the next progress
event.  State transitions in the projection are
gated on the event timestamp so out-of-order delivery (the standalone
API's Pulsar topics order only *within* a topic) can't clobber newer
state with an older event — including a final progress round that
overtakes its own phase completion, which would otherwise re-open the
row of a phase that had already finished.

Every one of those comparisons normalizes to a single ordering key first.  The same instant reaches the API in
more than one encoding (`Z`, `+00:00`, naive, a non-UTC offset), and as
raw strings those order differently: `…05Z` sorts *after* `…05+00:00`
for the same moment, and `13:00+01:00` sorts after `12:30+00:00` while
being half an hour earlier.  Compared that way a straggler round
resurrects a phase that has already finished, and a stale event
clobbers newer state.

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
  "health":    { /* the health envelope — see below */ },
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
  "budget":    { /* the live org-wide token meter, or {} — see below */ },
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

### The health envelope

One builder (`api.streaming.build_health_envelope`) answers `GET /health`,
the snapshot's `health` section, and the 5-second push, so those three
surfaces cannot disagree about whether the engine is healthy — and a
reconnect restores every field without a second round trip.

```json
{
  "status": "ok",
  "configured": true,
  "engine": true,
  "version": "0.4.0",
  "started_at": "2026-04-01T12:00:00+00:00",
  "queue": "pulsar",
  "event_store": "durable",
  "feed_hydrated": true,
  "clients": 3,
  "in_flight": 2,
  "engine_started_at": "2026-04-01T11:58:03+00:00",
  "shutting_down": false
}
```

| Field | Meaning |
|-------|---------|
| `status` | `ok`, `unconfigured`, or `shutting_down`. Precedence is `shutting_down > unconfigured > ok` — a draining engine is draining first, whatever else is true of it. |
| `configured` | Whether a company revision is active. When `false` the engine accepts and **discards** every inbound webhook, so an operator watching empty screens needs to be told this rather than left to infer it. |
| `engine` | Whether this process has an engine to ask. `false` on the [standalone API](../guides/deployment.md), where `in_flight` / `engine_started_at` / `shutting_down` are absent — the flag is what lets a client tell "nothing is running" from "this process cannot know", instead of rendering a confident zero for both. |
| `version` | The `crewlet` version this process is running. |
| `started_at` | When the **API process** started. Deliberately separate from `engine_started_at`: on the standalone deployment those are two processes on two clocks, and one merged "uptime" would be wrong for at least one of them. |
| `queue` | The event queue's backend — `pulsar` or `memory`. Read off the `EventQueue` protocol, never sniffed from a class name. Display only; nothing may branch on it. |
| `event_store` | `durable`, `memory`, or `none`. Three-valued because "a store is wired" is not "history survives a restart": with no database the CLI still wraps in-memory legs in a `CompositeEventStore`, so a presence check answers yes while every event is one process death from gone. |
| `feed_hydrated` | Whether the live-state projection was seeded from stored history at startup. Hydration is best-effort and swallows its own store errors, so this is the only signal that the activity feed starts at this process's boot rather than at the retained history. |
| `clients` | Dashboards currently connected to this API process. |
| `in_flight` | Handler invocations mid-flight (embedded API only). |
| `shutting_down` | `true` from the first moment of a graceful stop, so a dashboard shows the drain while it happens — the API server keeps serving until the engine has fully stopped. |

Per-socket facts — how many envelopes *this* connection dropped, how deep
its queue is — are deliberately **not** here. The tick encodes one JSON
string and hands the same string to every client, so a per-client field
would force one encode per client per tick; they are answered on demand
by the `stream` query instead.

`GET /health` always returns **200**, including when `status` is
`unconfigured`: the status code is liveness, and an engine waiting for a
configuration is alive. A readiness probe should read `configured`.

### Paging the event history

`GET /events` and the `events` query return rows ordered by
`(timestamp, id)` **descending**, and accept an exclusive keyset cursor:

```
GET /events?limit=100&before=2026-04-01T12:00:00%2B00:00&before_id=<event_id>
```

Pass the oldest row you already hold to get the page beneath it. The id
half is not optional — burst writes routinely share a timestamp at
microsecond resolution, and a cursor over a non-unique key silently
skips or repeats whatever collided with it.

**A page shorter than `limit` is the end of the history.** That rule
holds for every filter the store pushes into SQL. It does *not* hold for
`related_agent`, which over-fetches and post-filters (it also pulls in
every event sharing a trace with a direct match, so a caller must dedupe
by id); that surface only knows it is done when a page returns zero rows.

The persistent store retains 30 days. Once a cursor crosses that floor
every page is empty — which is why a client must distinguish it from
quiet, rather than drawing the gap as silence. A deployment with no
event store answers **503** (and `no_event_store` on the query channel)
rather than an empty page, for the same reason: "there is nothing older"
and "I cannot answer" are different facts.

`category` is a filter for the same reason paging exists at all —
filtering a paged list client-side silently excludes, because a 100-row
page holding 2 matches reads as "only 2 exist".

### The live token meter

`budget` carries the engine's in-memory token counters — the only figures
that can honestly be divided into a configured cap, because both cover the
same span: **the engine's run**. The dashboard's other two token figures
are a 24-hour spend rollup and a 7-day per-agent total; dividing either
into a cap produces a percentage wrong by however long the engine has been
up.

- `meter_id` identifies the reporting run. `used` is comparable only
  within one `meter_id`; a new one means the engine restarted and every
  prior figure is dead, so a consumer must **replace** what it holds
  rather than merge or take a maximum.
- `seq` is monotonic within a `meter_id`. Broker ordering holds only
  within a topic and the standalone API reads a broadcast subscription
  across all of them, so a report at or below the held `seq` is dropped.
- `refused_at` — when the cap last turned a charge away. That, and not
  `used >= max`, is what "exhausted" means: a refused charge increments
  nothing, so the counter stops short of the cap by the size of the round
  that would not fit.
- `{}` means no engine is reporting one (the standalone API has no meter
  of its own). Per-agent, `budget: null` means the same, or that the seat
  has no per-agent cap at all — the engine seeds one only for a non-zero
  `token_budget`.

It is deliberately never persisted: replaying a live meter from history
would show a dead process's counters as the current ones.

Each agent's `live_call` is `null` between turns, or
`{ turn_id, phase, iteration, model, response, tool_executions, rounds,
in_progress }` while an LLM call is under way.  A call whose phase
failed keeps `in_progress: false` plus `failed: true` and an `error`
object, so the dashboard renders the failure instead of an answer that
never arrives.

### `WS /ws/stream`

Upgrades to a WebSocket.  All frames are JSON envelopes of the form
`{"kind": "...", "data": ..., "ts": "<iso8601>"}`.

> **The socket is the dashboard's only data channel.** Everything it draws
> arrives here — pushes plus a request/response query channel — and the
> REST snapshot exists only for degraded mode, when the socket is down. The
> dashboard survives losing it by polling `/stream/snapshot` every five
> seconds, which is exactly the kind of failure that is easy to miss:
> nothing looks broken, the page is simply always a few seconds stale.
> `internal/e2e` closes that gap by replaying the frames a real server
> produced through the dashboard's own `store.js`, so both halves of the
> protocol are checked against each other rather than each against its own
> idea of the other.

**Server → client kinds**

| `kind` | When | `data` |
|--------|------|--------|
| `snapshot` | First envelope after the upgrade succeeds, and again on reconnect. | Same payload as `GET /stream/snapshot` — agents carry their in-flight `live_call`, so a reconnect re-renders the live row. |
| `event`    | Every engine event published to `crewlet.events.>`. | `{ id, type, timestamp, source, actor, summary, category, trace_id, span_id, parent_span_id, topic, payload }` — the same shape as a `/events` row, plus the full event `payload` (from which the snapshot feed's `failed` flag is derived).  `agent_phase_completed` events carry the system prompt, response, and tool calls, so LLM invocations stream live; `agent_turn_progress` events (per tool-call round, tagged with `turn_id` / `phase` / `iteration`) stream the in-flight call before its phase record exists. |
| `agents`   | After an event moved one or more agents. | The changed agents' overlays, each with its `role` — the *result* of applying the event, so a client merges them rather than running its own state machine over the raw stream. |
| `seats`    | After a config revision changed the roster. | The COMPLETE seat list, replacing what the client holds. Distinct from `agents` on purpose: that one is a per-role merge, and a merge cannot express the deletion of a role a revision removed. |
| `sandboxes`| After a detached sandbox run started, asked a question, or finished. | The full in-flight sandbox list. |
| `tokens`   | After a phase completed, coalesced to at most one per second. | The spend rollup, same shape as `GET /tokens/breakdown`. |
| `budget`   | After the engine reported a moved token meter (coalesced engine-side to at most one report per second). | `{ meter_id, seq, org: { used, max, refused_at } }` — the org-wide half. Per-seat figures ride on each agent's overlay in the `agents` push. |
| `org` / `tools` / `schedules` | After a config revision is activated. | The new org tree / tool surface / schedule list, so open tabs stop showing seats that no longer exist. |
| `health`   | Pulsed every 5s by a **single shared tick** (one timer for all clients, not one per connection). | The health envelope — see [below](#the-health-envelope). |
| `result`   | Reply to a client `query` that succeeded. | `{ id, what, data }` — `id` echoes the request's. |
| `error`    | Reply to a client `query` that could not be answered. | `{ id, what, error }` where `error` is a code: `not_found`, `unauthorized`, `unknown_query`, `no_event_store`, `no_pending_store`, `fleet_unavailable`, `query_failed`. |
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
| `events` | `{limit, type, source, category, trace_id, actor, related_agent, before, before_id}` | `GET /events` |
| `trace` | `{trace_id}` | `GET /events/trace/{trace_id}` |
| `tokens` | `{since_days, agent_role, recent_turns}` | `GET /tokens/breakdown` — for a window other than the live one |
| `schedules` | — | `GET /schedules` |
| `fleet` | — | `GET /fleet` — leases move with no event to push, so the Fleet view polls this rather than waiting for one. `fleet_unavailable` when a configured lease store cannot be read (the REST twin answers `503` for the same case) |
| `sandbox_runs` | — | `GET /sandbox-runs` — `no_pending_store` when no database is configured; the REST twin answers that case with the `degraded` body below |
| `budgets` | — | `GET /budgets` |
| `conversations` | `{handle, key}` | `GET /conversations` — the seat page's **Threads** tab |
| `integrations` | — | `GET /integrations` |
| `stream` | — | Facts about **this** socket — `{ client_id, dropped, queued, capacity, connected_at, clients }`. The only query with no REST twin, because there is no connection to describe outside one. |
| `config` | — | `GET /config` *(operator token required)* |
| `config_audit` | `{limit}` | The revision history — no REST twin; `GET /config/revisions` serves the same records *(operator token required)* |
| `config_diff` | `{revision_id}` | `GET /config/revisions/{id}/diff` *(operator token required)* |
| `config_entities` | `{kind, id}` | One addressable collection of the active revision: its ids, or one entity out of it. The read half of the Config room's entity editor, whose write half is `PUT /config/{kind}/{id}` *(operator token required)* |

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
  `synthesized_skills` via `learning.Skills.List`. The
  table is strictly per-agent; cross-agent procedural artefacts are
  [promoted](../concepts/agent-learning.md) as draft pages in the shared
  knowledge backend, reachable by all members via query-time search.

Each section degrades independently: a missing knowledge provider, an
unreadable store, or a per-section query error returns an
empty list for that section instead of erroring the whole response.
Returns 404 when no role with the given `id` is configured.

---

## Fleet, Sandbox Runs & Schedules

### `GET /fleet`

Backs the dashboard's **Fleet** view — the questions `/health` cannot
answer, because it answers about the node that served it and a load
balancer sends the next refresh somewhere else.

Read from the lease table, so every node gives the same answer: node
presence carries each node's `node.roles` and `node.labels`, seat and
worker leases name their holder, and the per-node config epoch comes from
the control plane's apply status.

Presence also carries what each node is **doing** — `in_flight`,
`draining`, `posture` and `started_at` — because only the node running a
seat knows those, and `/health` answers about whichever node served the
request. They ride on the heartbeat that already re-sends roles and labels
on every beat, rather than over a request/reply to the owning node: every
answer would then be partial, it opens a new trust edge, and it duplicates
the mechanism the lease table already is. See
[d-501](https://github.com/crewlet/crewlet/blob/main/decisions/501-node-runtime.md).

**Absent is not zero.** A node that publishes no status — one whose engine
is not co-located — omits those fields entirely, and the dashboard draws an
em dash. A confident `0` would render an idle row for a process that is
simply not saying.

Two fields report the failures that are otherwise invisible, because
their only symptom is an absence: `unmanned_roles` lists roles no live
node performs, and `unplaceable` lists seats whose `role.placement`
matches no live node. Without a database configured there is no lease
table and no fleet; the response says so in `degraded` rather than
failing.

```json
{
  "nodes": [
    {
      "id": "core-1", "roles": ["ingress", "seats", "workers"], "labels": {},
      "owner": "core-1:8f2a", "protocol": 3, "seats": 4, "expires_in": 41.2,
      "config_epoch": 7, "config_status": "ok", "config_error": ""
    }
  ],
  "seats": [
    {"handle": "ceo", "node": "core-1", "owner": "core-1:8f2a",
     "epoch": 4, "expires_in": 41.2}
  ],
  "duties": [{"duty": "maintenance", "node": "core-1", "expires_in": 41.2}],
  "unplaceable": [{"handle": "gpu-eng", "placement": "labels=gpu=true"}],
  "unmanned_roles": [],
  "this_node": "core-1"
}
```

### `GET /sandbox-runs`

Every detached [coding run](../concepts/code-sandbox.md) the engine still
holds, oldest first — `running`, `awaiting_clarification`, `reseed`, and
`resumed` rows of the `pending_sandbox_run` table.

Read from the durable row rather than from the live projection, which is
the wrong source for this question twice over: it is in-memory, so it
starts empty after a restart, and it sweeps an entry after twelve hours
while a run parked on a question can legitimately wait days for a person
to answer. The states that most need somebody were therefore the ones
least likely to be on screen, and a `reseed` run (pause expired, box
reclaimed, work preserved on a pushed branch) had no surface at all — it
looked exactly like work that had finished.

```json
{
  "runs": [
    {
      "turn_id": "<uuid>", "agent_handle": "eng", "role": "Engineer",
      "status": "awaiting_clarification", "coding_agent": "claude-code",
      "task_description": "Add retry to the webhook client",
      "question": "Which backoff ceiling should I use?", "audience": "founder",
      "branch": "crewlet/eng/retry", "trace_id": "<hex>", "owner": "core-1:8f2a",
      "box_exists": true, "paused_at": "2026-06-08T07:30:02+00:00",
      "pause_ttl_seconds": 3600,
      "started_at": "2026-06-08T07:12:44+00:00",
      "updated_at": "2026-06-08T07:30:02+00:00",
      "answerable_in_chat": true
    }
  ]
}
```

`box_exists` and `paused_at` stand in for the sandbox id: a board wants to
know that a box exists and that it is currently paused (and being billed
for as a snapshot), not which box it is. `answerable_in_chat` is `false`
for a run whose turn was triggered by something other than an inbound
message — a schedule tick, a task assignment, an A2A wake — because the
resume path matches an inbound conversation key by exact equality, and
those runs stored a key no chat message can reproduce. Telling somebody to
"reply in the thread" would send them to a thread that does not exist.

`execute_state` — the serialised Execute-loop conversation — is
deliberately not returned: it is the largest column in the row and every
prompt in it is already reachable through the event store.

Without a database the engine cannot park a run at all, so that
deployment gets `{"runs": [], "degraded": "..."}` rather than an error;
a store that is configured and unreadable answers `503`.

### `GET /budgets`

Backs the dashboard's **Spend** room. Three numbers describe a token
budget and they cover three different spans, which is why any surface
mixing them can only be wrong:

- the **cap** is configuration, from the active company revision;
- **durable usage** is the fleet's shared counter, in the
  [coordination store](../concepts/coordination.md), written by every node
  running the company and surviving restarts. It is what the engine
  actually enforces against;
- the **live meter** is per engine *run*. It is pushed to the dashboard as
  `budget_reported` and resets when the process does.

Only the meter and the cap share a span, which is why a seat card can draw
a bar and this room mostly cannot. What it could never show before is the
more useful picture — "this seat has burned 94% of its cap across two
restarts" — because the durable half was reachable only from
`crewlet budgets show`, which is itself a client of this route.

```json
{
  "durable": true,
  "org": {
    "max_tokens": 5000000, "durable_used": 1284410,
    "durable_updated_at": "2026-06-08T07:30:02+00:00",
    "live_used": 91200, "live_max": 5000000, "refused_at": ""
  },
  "seats": [
    {
      "agent_id": "<uuid>", "role": "Engineer", "handle": "eng",
      "max_tokens": 100000, "durable_used": 99120,
      "durable_updated_at": "2026-06-08T07:29:51+00:00",
      "live_used": 41000, "live_max": 100000,
      "refused_at": "2026-06-08T07:29:51+00:00"
    }
  ]
}
```

Two fields carry the honesty. `durable` is `false` when the shared counter
could not be read — a counter that cannot be read is not a counter that
reads zero, and without the flag a database blip renders every seat at the
bottom of its cap, which is the most reassuring possible picture drawn at
the moment nothing is known. `live_used` / `live_max` are `null`, never
`0`, on a node with no engine in the process: zero would let a client draw
an empty bar and call it "nothing spent this run", a claim about a run
that is not happening.

Exhaustion is `refused_at`, the moment a charge was turned away — never
`used >= max`. `TokenBudget` refuses a charge that would exceed the cap
and increments nothing, so a seat charged in 3k-token rounds against a
100k cap stalls near 99k and never compares equal to its own maximum. A
ratio test shows a permanently blocked seat at 99% and calls it healthy.

### `POST /budgets/reset`

Zeroes the fleet's token counter. `?scope=` names one (`org`, `agent:<id>`);
its absence clears every one.

```bash
curl -X POST -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  "http://localhost:8080/budgets/reset?scope=agent:<uuid>"
```

```json
{"cleared": 1, "scopes": ["agent:<uuid>"]}
```

The answer **names what it cleared** rather than only counting it: this is an
irreversible action against a spend ceiling, and a bare count leaves an
operator unable to tell "reset the seat I meant" from "reset a scope that was
already empty".

This route exists because the counter is fleet state. On the default topology
the [coordination store](../concepts/coordination.md) is the engine's own
embedded broker, so a running node is the only thing that can reach it —
which is why `crewlet budgets reset` is a client of this route rather than a
command that opens a file.

Two refusals, both deliberate:

- **401 without a token.** `allow_anonymous_read` is on by default and opens
  the whole read surface; a reset is a write, so it is never eligible.
- **503 with no coordination store.** A standalone API with no counter
  attached answers `{"error":"no_coordination_store"}` rather than 404: the
  route exists on this build, and a 404 sends an operator looking for a
  version mismatch that is not there.

### `GET /integrations`

Backs the dashboard's **Integrations** room: how each external surface is
wired, and what has come through it over the last 24 hours.

Integrations had close to no surface at all before this. The dashboard
branded an event once it had already been accepted and routed, so every
failure mode an operator actually hits was invisible — a Mattermost
`SiteURL` that blinds every browser while agents keep working, a revoked
bot token, a mis-pasted webhook secret. Rejected deliveries are
deliberately never written to the event store (verification runs before
the row is logged, which is correct), so a signature mismatch left no
trace anywhere except the provider's own delivery UI.

```json
{
  "traffic_known": true,
  "traffic_since": "2026-06-07T09:12:00Z",
  "integrations": [
    {
      "key": "gitlab", "configured": true, "enabled": true,
      "url": "https://gitlab.example.com",
      "inbound_kind": "webhook", "inbound_path": "/webhooks/gitlab",
      "routes": true,
      "secret_present": true, "secret_usable": true,
      "seats": ["eng", "pm"],
      "inbound": 128,
      "last_at": "2026-06-08T07:31:10+00:00"
    }
  ]
}
```

`secret_present` and `secret_usable` are **two different facts**, and the gap
between them is a silent outage.

`secret_present` is a claim about the **document**: an operator wrote a secret
down. It is three-valued because the cases mean opposite things: `null` — this
surface does not use one (Mattermost authenticates its websocket with the
bot's own token); `false` — it does, and none is configured, which means the
webhook route answers `503` to every delivery.

`secret_usable` is a claim about what this process **resolved**. A secret lives
in the config as a `${VAR}`, so `secret_present: true, secret_usable: false` is
a route refusing every delivery while the config shows a secret and the
vendor's settings page shows a healthy hook — with nothing anywhere naming the
variable. For GitLab the bar is higher than non-empty: the value must be
`whsec_` over standard base64 of a 32-byte key, the only shape the vendor
signs with. `null` means this process cannot say — a standalone API has no
engine whose resolution to read — or the surface has no secret to resolve.

Only the booleans are ever returned; no secret value leaves the process.

`routes` is the third of the same family: whether a **verified** delivery
would wake a seat. The three fail independently, and an operator staring at a
silent integration needs to know which half broke.

**Health is deliberately not inferred.** An idle Slack and a 401-ing Slack
are indistinguishable in the event store, so silence is reported as "no
traffic seen" — never as healthy, never as down. `traffic_known` is
`false` on a deployment whose event store cannot group by source, so the
zeros below it are absence of measurement rather than measurement of
absence.

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
the event store's phase-token query and groups them by phase, model,
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

Receives Jira webhook payloads (issue created, updated, commented). Verifies HMAC-SHA256 signature if `webhook_secret` is configured. Publishes parsed events to `crewlet.notifications.inbound`. **This build refuses the config block this route verifies against**, so it answers 503 to everything — see the integration page. It comes alive in the same change that ships the vendor's parser.

### `/webhooks/slack/{handle}`

Receives Slack Events API payloads for a specific agent (identified by handle). Verifies the signing secret for that agent's Slack app. Publishes to `crewlet.notifications.inbound`. Slack's `url_verification` challenge is answered unconditionally (no engine or company config needed), so a freshly provisioned app's Request URL verifies even before the engine is configured. **This build refuses the config block this route verifies against**, so it answers 503 to everything — see the integration page. It comes alive in the same change that ships the vendor's parser.

### `GET /webhooks/slack-oauth`

The OAuth install landing page for [`crewlet slack provision`](../integrations/slack.md) — every provisioned Slack app has this as its OAuth redirect URL. After the operator approves an install, Slack redirects here with a temporary `code` (and `state` carrying the agent handle); the page displays the code for pasting back into the waiting CLI prompt. Unauthenticated by design: the code expires after 10 minutes and is useless without the app's client secret, which only the provisioning CLI holds.

### `/webhooks/github`

Receives GitHub webhook payloads. Verifies HMAC-SHA256 signature via the `x-hub-signature-256` header using the required `webhook_secret` from the `github` config block. Invalid or missing signatures are rejected with 401. Returns 500 if the server has no `webhook_secret` configured. Publishes to `crewlet.notifications.inbound`. **This build refuses the config block this route verifies against**, so it answers 503 to everything — see the integration page. It comes alive in the same change that ships the vendor's parser.

### `/webhooks/gitlab`

Receives GitLab webhook payloads. **The signature is the only credential**: `webhook-signature` is verified as a Standard-Webhooks HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}`, keyed on the `signing_secret`'s base64 payload, constant-time against any of the header's space-separated `v1,…` entries, with a ±5-minute timestamp tolerance. A missing or wrong signature is rejected with 401 — the plaintext `X-Gitlab-Token` is not accepted, so omitting the signature header is not a downgrade path. Answers 503 with a `Retry-After` when no `signing_secret` is configured, or when its value is not a usable `whsec_` key, so the delivery is held for retry rather than blamed on the sender. GitLab signs whenever the hook has a `signing_token` (GitLab 19.1+); see [GitLab § Verification](../integrations/gitlab.md#verification). Publishes to `crewlet.notifications.inbound`. See [GitLab Integration — Webhooks](../integrations/gitlab.md#webhooks).

### `/webhooks/plane`

Receives Plane webhook payloads from the [Plane fork](../integrations/plane.md). The `X-Plane-Signature` header is verified as the HMAC-SHA256 hexdigest of the raw body keyed with `integrations.plane.webhook_secret` (constant-time compare; Plane CE's only scheme). Invalid or missing signatures are rejected with 401; returns 500 if no secret is configured; malformed JSON returns 400. When the engine is unconfigured, a *verified* request is answered 200 `{"status": "dropped"}` — verification runs first so forgeries never earn a 200, while genuine deliveries don't poison Plane's five-retry auto-disable counter. Publishes to `crewlet.notifications.inbound`. See [Plane Integration — Webhooks](../integrations/plane.md#webhooks).

### `/webhooks/confluence`

Receives Confluence webhook payloads (page created/updated, comments). Publishes to `crewlet.notifications.inbound`. **This build refuses the config block this route verifies against**, so it answers 503 to everything — see the integration page. It comes alive in the same change that ships the vendor's parser.

### `/webhooks/forge`

Receives events from the Atlassian Forge app. Every request must carry a Forge Invocation Token (FIT) as an `Authorization: Bearer` JWT; the token is verified against Atlassian's JWKS endpoint and its `aud` claim must match the configured `forge_app_id` (401 on failure, 500 when no app id is configured). The request body is drained **before** FIT verification — verification can block on a JWKS fetch, and the body must be off the socket before the sender's delivery deadline aborts the request. Maps `avi:jira:*` / `avi:confluence:*` events onto the native Jira/Confluence pipeline and publishes to `crewlet.notifications.inbound`. Self-generated events (an agent's own actions echoed back by Forge) are acknowledged and dropped. **This build refuses the config block this route verifies against**, so it answers 503 to everything — see the integration page. It comes alive in the same change that ships the vendor's parser.

### Aborted deliveries (client disconnects)

Webhook senders enforce delivery deadlines and abort requests that respond too slowly. When a sender hangs up before the request body is fully read, Starlette raises `ClientDisconnect`; the API handles this app-wide with a structured `client_disconnected` warning (`component=api.app`, keyed by `method`, `path`, `remote`) instead of letting it escape as an unhandled ERROR traceback. The aborted delivery is dropped — whether it is redelivered is up to the sender's retry policy — so recurring `client_disconnected` warnings on a webhook path mean events are being lost because the API is answering too slowly.

---

## Running

```bash
crewlet run -config config.yaml -roles ingress -api-host 0.0.0.0 -api-port 8000
```

The API is read-only against the database and publishes commands/notifications to Pulsar. It does not run agents — the engine process handles that.

See [Deployment](../guides/deployment.md) for how the API and engine run together, and the integration docs ([Slack](../integrations/slack.md), [Jira](../integrations/jira.md)) for webhook setup.
