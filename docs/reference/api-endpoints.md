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

## Request timeouts

Three deadlines bound a request on **every** route below, and each covers a phase the others do not. None is configurable — they are properties of what the surface is for, not of a deployment.

| Phase | Bound | What it stops |
|---|---|---|
| Request line and headers | 10 s | A connection opened and left silent — the cheapest denial there is against a listener. |
| Reading the request body | 30 s | A client that *dribbles*: a body under every size cap, delivered a few bytes at a time. The size cap and this are different failures, and a cap alone stops only the first. 30 s carries the largest body any route accepts — a 25 MiB webhook delivery — at roughly 7 Mbit/s sustained, far below what any forge, CI runner or operator workstation delivers. |
| Between keep-alive requests | 60 s | A client that completed one request and then went quiet while holding its connection slot. |

A body that does not arrive inside its deadline fails the read like any other truncated delivery: `400` with `unreadable_body` (logged as `webhook_body_unreadable` on a webhook path). There is deliberately **no** whole-request timeout: it would have to be large enough for the largest body on the slowest link, which makes it no bound at all on a small one. Long-lived responses — [`WS /ws/stream`](#ws-wsstream) above all — bound themselves.

---

## Every refusal is one envelope

A refused request — on any route, from any surface, whatever the status — is
one JSON object, and it always has the same three parts in the same places:

```json
{
  "error": "no_external_url",
  "message": "This deployment has no external address, so nothing that has to point back at it can be made. Set the external URL in this node's own configuration file and restart it.",
  "config_path": "api.external_url",
  "hint": "set the HTTPS address third-party apps reach this deployment on; without it the pass can register no webhook"
}
```

- **`error` is the code, and the only thing to branch on.** It comes from one
  closed vocabulary shared by every surface (`internal/api/httpjson`), so the
  same failure has the same name wherever it is met: a `413` is
  `body_too_large` whether what overflowed was a config document, a secret
  value or a webhook delivery, and a query the engine does not serve is
  `unknown_query` over both the WebSocket and its REST twin. Never parse the
  message to find out what happened.
- **`message` is one sentence for a person**, written as product copy and
  rendered verbatim by the dashboard. It belongs to the *code*, not to the call
  site, so a refusal reads the same wherever it came from — and a route with
  more to say says it in the detail rather than rewording the sentence.
  Never match on it: the wording is copy and can be improved at any time. Every
  refusal carries one — `/config`, `/secrets`, `/setup` and `/chart` included,
  which used to build their own bodies with the code alone, and every
  `/webhooks/*` route, which answered a code with a space in it
  (`invalid signature`) and a `{"status": "unavailable", "reason": …}` shape of
  its own ([Webhook refusals](#webhook-refusals)).
- **Everything else is the detail** — the machine-readable facts about *this*
  refusal, as typed JSON beside the two reserved keys rather than nested under
  one: `config_path` and `hint` above, `fields` on an integration that is
  missing values, `current_revision_id` on a lost update, `problems` on a
  refused configuration document. Values keep their own types,
  so a count is a number and a list of located problems is a list.
- **A `503` says when, or says it will not help.** A `503` that waiting clears
  — a node behind its log, a coordination store it could not reach, a write
  whose outcome it cannot establish, an integration another pass is holding
  (`surface_busy`) — carries a `Retry-After` in seconds, derived from the
  refusal where the refusal knows (a node behind its log estimates from its
  own backlog) and a few seconds otherwise. A `503` that no wait changes
  carries NONE: the header's absence is the answer, and the detail names what
  to do instead. That is a refusal from the state log
  that waiting cannot clear on THIS node — a node evicted from the fleet, one
  holding a record it cannot decode, a log at its byte ceiling
  (`log_full`), a record larger than the broker takes in one message
  (`record_too_large`), one the broker refused for a reason of its own
  (`broker_refused`), a deleted object — on every surface that answers one
  with a `503` (`/chart`, `/work`, `/pages`, `/iam`, `/auth`, and every
  question the query registry answers, `/query/{what}` and its named read
  routes): the same request is
  refused however often it is sent, so the answer is to ask another node, to
  split the change, or for an operator to readmit, upgrade or resize, never to
  poll this one. The reason is in the detail; [Read
  consistency](../guides/consistency.md#a-writes-refusals) says what each one
  asks of whom.
- **A write that may have landed is not a refusal, and says so as a field.**
  A `503` about a write this node cannot account for — its outcome is
  `unknown` — carries **`"outcome": "unknown"`** and the **`op_id`** to retry
  under, on every surface that writes through a state log (`/chart`, `/work`,
  `/pages`, `/iam`, `/auth`); a `503` without `outcome` is a refusal, and
  what it refused was not written — a gesture refused partway lists what did
  land before it as `landed`. The two send a client opposite ways — the
  unknown is retried under the **same** `op_id`, never a fresh one, which
  would make the change twice if the first landed — so branch on the field
  rather than on the sentence. The routes that read no `Idempotency-Key` — a
  token's mint, and every `/auth` gesture — are the exception the `detail`
  names: there the `op_id` finds the attempt in the trail, and the retry is
  the gesture asked again. Where
  this node's operation ledger cannot vouch for the operation it also carries
  **`"unvouched": true`** and **no** `Retry-After`: the same request here answers
  the same way until the change reaches this node, so send it, with the same
  `op_id`, through another node. `outcome`, `op_id` and `unvouched` are the
  answer's own on that `503` and are never displaced by a route's detail.
- **A `500 internal_error` carries no reason.** Its sentence says the reason
  is in this node's log, and that is where it is: a fault's own words are a
  store's or a driver's — a database path, a connection string — and holding
  a grant does not make a caller somebody they are meant for. A route may
  still say what the request itself carried and what of it already landed
  (`/iam` does, beside the code), never what failed underneath.

`error` and `message` are RESERVED: a route's own detail can never displace
them, so a client that branches on the code cannot find it missing because a
route happened to use the same key.

Two shapes sit deliberately outside this envelope. A **WebSocket query answer**
is a frame rather than a response — `{"kind": "error", "id", "what", "error"}`
— and carries no `message`, because the client switches on the value and the
sentence for each code is one the dashboard already holds (see
[`WS /ws/stream`](#ws-wsstream)). A refusal on **authority** is the exception
to "the code alone": its frame carries the same `reason` and `grants` the REST
envelope does, under the same keys, because those are facts about this
refusal rather than a sentence about the code. And the routes that are not JSON at all —
the dashboard shell, the Slack OAuth landing — answer as what they are.

---

## During a drain

A node that has been told to stop (SIGTERM, or `Ctrl+C` once) keeps serving HTTP for the whole of its [drain](../concepts/agent-runtime.md#graceful-shutdown), and closes its listener only once the drain has completed. What changes from the drain's first moment is which requests it will still take:

| Request | During a drain | Why |
|---|---|---|
| `GET /health` | `200`, `status: "shutting_down"` | Liveness. An orchestrator that could not reach the node would kill it in the middle of the turns the drain exists to finish. |
| `GET /ready` | `503`, `reason: "draining"` | Readiness: takes the node out of rotation so traffic moves to a peer. |
| Every other read (`GET`, `HEAD`, `OPTIONS`): the dashboard, the REST reads, `/query/*`, `/ws/stream` | Served | A read starts nothing, and it is how the drain is watched. |
| `/mcp/{token}` and `/otlp/{token}/v1/{signal}` | Served | They carry the tool calls and spans of coding runs that started before the drain. A [detached run](../concepts/code-sandbox.md) outlives the turn that started it, so the drain never waits on one, and refusing these would shorten no drain and only break a run mid-flight. |
| Every `/webhooks/*` route, whatever its method | `503` | A delivery is new work, and one of the two `GET` landings acts: the GitHub App return seals a credential and writes a config revision, and an install arrival asks the reconcile loop for a pass. The Slack OAuth landing only renders a page and is refused with the rest, because a per-route carve-out is what refusing by default avoids. |
| Every other write: `/config`, `/secrets`, `/setup`, `/backup`, the `/work/*` writes, `POST /operator/mcp`, `POST /operator/act/{tool}` | `503` | Each one starts work or changes the company the drain is leaving. Refusing by default is what keeps a write route added later from slipping through a drain. |

`/operator/mcp` is the one route the by-method rule splits, because it is mounted for every verb: its `POST` — every JSON-RPC call, reads included — is refused. It is served [statelessly](#every-call-is-decided-by-the-request-that-carries-it), so it offers no server-to-client stream and holds no session: its `GET` is served like any other read and answers the MCP transport's own `405 Allow: POST`, and its `DELETE` rides the default with the writes. `/mcp/{token}` is not split, because the whole prefix is served: a coding run's tool calls are the one thing on this listener the node must not break.

A refusal is `503` with a `Retry-After` of 30 seconds, long enough for a load balancer following `/ready` to have moved traffic to a peer, and a body the CLI and the dashboard both render:

```json
{
  "error": "draining",
  "message": "This node is shutting down and is not taking new work. Try another node, or this one once it has restarted.",
  "detail": "this node is draining for a shutdown: the turns already running finish, and nothing new is started here",
  "hint": "retry against another node, or once this one has restarted; /ready answers 503 for as long as the drain lasts"
}
```

A write still needs its token first: an unauthenticated write answers `401` whether or not the node is draining. And a request that was already running when the drain began is not interrupted by it; it is cut only if it is still running five seconds after the listener starts to close.

---

## A request nothing serves, and an answer with no code

The [envelope](#every-refusal-is-one-envelope) covers a request nothing on the
node serves as well as one a route refuses: `404 no_route` for a path — one
this node's build does not have, or one its configuration leaves unmounted,
such as `POST /work/items/{key}/purge` on a company with no native tracker —
and `405 method_not_allowed`, with an `Allow` header, for a served path under
another method. Both carry a `detail` naming the method and the path, where the
router's own answer was a line of plain text no client could branch on.

The CLI and the dashboard rely on it. A non-2xx answer with no code was written
by something **in front of** the node — most often a reverse proxy's read
timeout — and says nothing about what the node did, so a write that meets one
is reported as *unknown*, with the operation id to finish it under, rather than
as refused. An answer with a code is the node's own, and a refusal from the
node means nothing was done.

---

## Routes

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness + the engine-health envelope (see [below](#the-health-envelope)). Stays `200` through a drain (see [During a drain](#during-a-drain)); use `/ready` to steer traffic |
| `GET` | `/ready` | Readiness for a load balancer: `503` while draining, before the first config revision applies, or on a `shed` or `stuck` posture, and `200` otherwise. A `503` names why in `reason`: `draining`, `unconfigured`, `shed` or `stuck`, in that order of precedence. It never reads the fleet's presence or alarm counts, which decide nothing here |
| `GET` | `/agents` | List agent roles, each merged with live state from the in-memory projection (including the in-flight `live_call`). [Human seats](../concepts/humans-in-the-org.md) are excluded — they appear only in `/org` with `"kind": "human"` |
| `GET` | `/agents/{id}` | Single agent — its `handle`, `agent_id` and `role` (its name), the live overlay (incl. `live_call`), and `llm_history`: the seat's finished phases newest first, capped at 50. `{id}` is the seat's **handle**, which is what every roster row carries as its `id`; a handle a rename retired still resolves to the seat it named. A role name is NOT accepted: two seats may share one, and a name-addressed answer read one seat's live call and transcript as the other's. A handle no agent seat answers to is `404` |
| `GET` | `/agents/{id}/memory` | Durable memories (personal, episodic, counterparty, synthesized skills). Same `{id}` — any handle the seat answers to, a retired one included, resolves to the derived agent id the diary is keyed by and to the handle the seat was created under, which the rest is keyed by |
| `GET` | `/org` | The company's charter and its seat and unit tree, in an explicit public shape that carries no contact identity, email, credential or deployment setting (see [below](#get-org)). Human seats appear with `"kind": "human"` |
| `GET` | `/tools` | Registered tools, each tagged with the `source` that registered it — `builtin` or `mcp:<server>` (see [Where a tool comes from](../guides/tools-and-mcp.md#where-a-tool-comes-from)) — plus its behavioural `annotations`, where it `delivers`, and its `input_schema` (see [below](#the-tool-catalogue)) |
| `GET` | `/events` | Recent engine events from the event store (`limit` caps at 400; keyset-paged, see below) |
| `GET` | `/events/{event_id}` | Single event incl. payload |
| `GET` | `/events/trace/{trace_id}` | All events in one trace, oldest first, capped at 500 |
| `GET` | `/tokens/breakdown` | The token-spend rollup by phase / model / provider entry / worker / seat — the live 24 hours, or any window of up to 90 company days from the replicated usage domain (see [below](#token-spend-breakdown)) |
| `GET` | `/tokens/series` | The same spend **with a time axis** — one bucket per company day or ISO week, split into bands (see [below](#get-tokensseries)) |
| `GET` | `/agents/activity` | Every seat's turns over a window of company days — counts, the first-pass rate over reviewed turns, turn-duration quantiles and a day-by-day series (see [`seat_activity`](#queries)) |
| `GET` | `/schedules` | Configured role/unit schedules + next-run + recent dispatch ledger |
| `GET` | `/fleet` | Every live node, its roles and labels, seat ownership, singleton duties, and per-node config epoch. **Always needs a credential carrying `fleet:operate`** — a signed-in session or a bearer, like every route here — because it describes the deployment rather than the company, and the dashboard locks the screen that draws it (see [below](#get-fleet)) |
| `GET` | `/sandbox-runs` | Every detached [sandbox](../concepts/code-sandbox.md) run the engine still holds, read from the durable run record in the [coordination store](../concepts/coordination.md) (see [below](#get-sandbox-runs)) |
| `GET` | `/budgets` | Token caps, the durable shared counter they are enforced against — per calendar window — and which scopes are being refused (see [below](#get-budgets)) |
| `POST` | `/backup` | Copy this node's store and stream estate into `?dir=` **on the engine's host**. **Takes `fleet:operate`** — it writes every credential the company holds to a path the caller names (see [below](#post-backup)) |
| `GET` | `/integrations` | Every inbound surface, how it is wired, whether a signing secret is present, and what has arrived through it (see [below](#get-integrations)) |
| `GET` | `/credential-pool` | Every `providers.llm` entry, each key it rotates through by variable name, and which of them a vendor is refusing and until when — this node's pools beside the fleet's cooldown ledger (never a value). **Takes `config:read`** (see [below](#get-credential-pool)) |
| `GET` | `/backups` | What the fleet has backed up: each owner's newest point as the trim reads it, and every backup a person asked a node for, failures included. **Takes `fleet:operate`** (see [below](#get-backups)) |
| `GET` | `/mcp-servers` | What each configured MCP server did on each live node — started, failed, tools served and the first failure — read off every node's presence heartbeat, beside what the configuration declares (never a credential). **Takes `config:read`** (see [below](#get-mcp-servers)) |
| `GET` | `/work` | The company's own tracker: a filtered listing of work items, plus the last key number minted per project. Served only where `tracker.backend` is `native` — a company on Jira gets `404 unknown_query`, not an empty board (see [below](#the-native-tracker-and-knowledge-base)) |
| `GET` | `/work/retention` | What the state log is holding, what the trim concluded and which term is stopping it, every node's position, and what this node costs to replace. **Takes `fleet:operate`, reads included** (see [below](#get-workretention--what-the-log-is-holding)) |
| `POST` | `/work/retention/ack` | Publish an operator backup floor, for `backup_floor: operator` |
| `POST` | `/work/retention/evict/{node}` | Install the eviction gate on a node on every log the trim counts nodes on — the tracker's, the knowledge base's, the org chart's and the identity estate's — so the trim can pass a floor it is pinning. Refused `409` while the node holds a live presence lease; answers per log (see [below](#the-three-retention-gestures-that-write)) |
| `POST` | `/work/retention/readmit/{node}` | Lift it on every one of those logs — the inverse commit rather than a delete. Refused `409` while the node still lacks records a trim floor lets the log delete |
| `POST` | `/work/retention/capacity` | Drive a log's byte-ceiling change as far as this node's mode allows |
| `GET` | `/work/retention/maintenance` | Where that window stands and what is holding it |
| `POST` | `/work/retention/maintenance/abandon` | Change what the operation is trying to reach, never the barrier it must cross |
| `POST` | `/work/retention/maintenance/exclude` | Record that a participant's process is stopped and holds no outstanding request |
| `GET` | `/work/retention/reanchor` | The live stream's own `created_at`, which a reanchor's confirmation has to echo, and the case a reanchor would answer |
| `POST` | `/work/retention/reanchor` | Adopt a recreated stream, or a broker restored from an older copy, at the next generation |
| `GET` | `/work/views` | One container's **view strip**: the six every container has without anybody saving one, and whatever was saved beyond them. `?container=` takes the query grammar's own spelling (`workspace`, `project:ENG`, `unit:engineering`, `person:ana`) — a project key is upper-cased and a unit is resolved through the chart by any key it answers to or by its name, so a team's strip is one strip under every one of its spellings. The strip is ordered by **the caller's own** personal views and pins, and there is no parameter naming whose: a strip is furniture a person arranges for themselves, and a parameter that could name somebody else's was a way to read their arrangement. The caller's own is their [own record](#whose-record-a-personal-question-answers-for) — their seat when bound, their login when not |
| `GET` | `/work/views/saved` | **Every saved view** the caller can see, in EVERY container — the shared ones and their own personal ones, pinned first, each row carrying the `container` it lives in. What the dashboard's view inventory and its sidebar's pinned group read, because a view saved on a project board is in no workspace strip. There is no parameter naming whose, for the reason `/work/views` has none. `?counts=true` counts each pinned view in its own container, and is refused `400 bad_params` to a credential that names no record of its own, since only a record has pins |
| `GET` | `/work/catalogue` | The company's **vocabulary**: the task types a create may name and the workspace's custom-field declarations. `?archived=true` also lists what was retired. The types are the EFFECTIVE set — the six this build ships plus whatever the company declared, a declaration replacing a builtin of the same slug |
| `GET` | `/work/projects` | Every **project** work is filed into, with its `task_counts` — `{todo, active, done, closed}`, one per status group, read from maintained columns and never aggregated per poll; there is no `open`, which folded the waiting work into the started work, so a caller wanting every unfinished item adds `todo` and `active` — its `target_date` (the day, `YYYY-MM-DD` on the company's clock, the lead means it to be finished; omitted when none is set), its `last_change` (when the project's work last changed and who changed it, ABSENT for a project nothing has been filed into), its chart-owned unit and its lead. `?q=` narrows by a word in the key, the name or the purpose and `?unit=` to the projects one unit owns — **named by the unit's key — its current one, the one it was created under or one it used to answer to — or by its name where exactly one unit carries it, in any case**, and matched against the team's key and its name. `?archived=` SELECTS a set rather than widening one — `false` (the default) for the live projects, `only` for the retired ones alone, `true` for both — so "what did we retire" is a query rather than a caller's own filter over a wider answer. `?sort=` orders the whole selected set before the page is taken: one of `key`, `name`, `unit`, `todo`, `active`, `done`, `closed`, `last_change`, `target`, each optionally with a leading `-` for descending, defaulting to `key`, with the key breaking every tie; a project with no `last_change` or no `target_date` sorts last in both directions. An `archived` or `sort` value that is neither is a **400** naming the parameter and what it accepts. `?limit=` caps at 200, which is also the default. The answer carries a `census` — `{active, archived}`, the same question under the same `q` and `unit` MINUS its archival term — so a caller that selected one set can still tell an empty set from an empty company; `total` is the census of the mode that was asked for. A set read, so it carries `complete` and its `incomplete` beside the read level |
| `GET` | `/work/projects/{key}` | One project in **full**: the six statuses with their labels, groups and descriptions; the effective types; the custom fields grouped by which type they apply to, required first, with the workspace ids this project **shadows** named; its tags; its default assignee, lead and owning unit. `?for_type=` narrows the fields to one type plus the ones that apply to every type. Unknown key answers 404 naming the nearest three; a `for_type` the company does not file answers 400 `bad_params`, not 404 — the project is there and the argument is what to change |
| `GET` | `/work/activity` | The **activity feed** — one durable row per applied commit, quiet ones included, at any age with no live/archive boundary to cross. Ordered by the COMPOSED LOG POSITION rather than by any clock, so `?since=` and `?cursor=` are both positions written `<stream>@<generation>:<sequence>` — which is what lets a cursor span a reanchor with no gap and no repeat. `?task=` (by key, id or a FORMER key), `?container=`, `?kinds=`, `?actor=`, `?assignee=`, `?notified=`, `?from=`/`?to=` (RFC3339, bounding the AUTHORED instants), `?limit=` ≤200. `?q=` is an escaped `LIKE` over the excerpt and is REFUSED unless it names a task, or a project **and** a `since` inside 90 days. Each record carries `fields` — what MOVED, as `{"<field>": {"from": …, "to": …}}` — for every kind and not only the ones about a task: a project reconcile names the name, purpose or unit that changed, a view save the query parameters, a priorities write the order before and after, and a dependency the item it now waits on. A task's own row draws on twenty-eight names: `title`, `status`, `assignee`, `priority`, `project`, `type`, `tags`, `due`, `due_all_day`, `start`, `estimate`, `points`, `reporter`, `watchers`, `muted`, `collaborators`, `parent`, `routing_unit`, `archived`, `removed_with`, `waiting_on`, `linked`, `duplicates`, `page`, `blocking`, `checklists`, `fields` and `body`. Values are the STORED form (a status slug, a whole RFC3339 instant, an item's id) rather than a rendering, because every node writes the row identically and a rendering would depend on the reader's zone and the company's live vocabulary; a collection is cut at a whole member and ends with `+N more`. The two largest are MARKED rather than carried: `body` is `<N> bytes` on each side (empty where there was none) and never the prose, and `checklists` is `<list>: <done> of <total> done` per named list. `fields` names each custom value by its SLUG — resolved against the project's catalogue by the node applying the change, which is why a NOTIFICATION carries every other delta and not this one — with a choice as its option's slug, a multi-valued field's members joined with `/`, and a count of any whose field the project no longer declares The ANSWER also carries `keys`, an id-to-item-key map naming the tasks those deltas point at — `waiting_on`, `linked` and `duplicates` but never `page`, which names a knowledge-base page; the `blocking` mirror; a person's `priorities` queue; and the two scalars that name a task, `parent` and `removed_with` — resolved on the answering node: a delta records another task by its ID, because a key belongs to that task's own row and a history row is written once and never repaired. An id this node holds no row for is absent rather than empty, and a renderer falls back to the id |
| `GET` | `/work/my-work` | Everything one person is expected to look at, in seven bounded lists: `priorities` in the stored order, `assigned`, `asked_of_me` (each with the literal call that answers it, whether it is `open`, and the `decision` it carries when it asks somebody to choose — see [Asking for a decision](../guides/work-tracker.md#asking-for-a-decision)), `checklist_items` (which live on other people's tasks and no assignee filter reaches), `collaborating`, `watching_recent` and `unblocked_recent`. `?handle=` is whose, and it **defaults to the caller's own record** — see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for). Naming somebody else's takes whoever leads them, or `fleet:operate` |
| `GET` | `/work/inbox` | One person's **inbox**: the notices a change wrote to them, each naming the ONE [reason](../guides/work-tracker.md) of eighteen it found them under, whether it **asks** something or merely informs, whether it arrived only because nobody better was found, and their own read and snooze marks — plus the comment and turn the change came from, and the `ask` it is about, read as it stands now. Same scope rule as `/work/my-work`. `?unread=`, `?primary_only=`, `?snoozed=` — `exclude` (the default: a snooze means *not now*), `include` or `only`, anything else refused naming the three — `?reasons=` (comma-separated, refused naming the eighteen); every one of them narrows the SCAN, so a page is full whenever the scope holds 50 notices and `next_cursor` is never a cursor past an empty page, `?limit=` ≤50, `?cursor=`, and `?since=` — a LOG POSITION written `<stream>@<generation>:<sequence>`, which is what `seen_through` reports back, never a bare sequence: the comparison is on the packed `(generation << 40) | seq`, so a sequence with no generation re-delivers everything after a reanchor |
| `GET` | `/work/people/{handle}` | One human's **own state**: their inbox (unread, read, snoozed, and which snoozes are now **due**), the order they mean to work in and who set it, and their pinned views. Same scope rule as `/work/my-work`, with the handle always named here because it is the path: your own seat's is yours, and anybody else's takes the same owner-or-lead rule. A person nobody has written yet answers the EMPTY state with `held: false`, not a 404 — every human starts this way and the first write is what creates the record |
| `GET` | `/work/{id}` | One item with its description, thread, history and links. `{id}` is either the key (`ENG-42`) or the id — a person holds the first and every internal link the second |
| `GET` | `/work/comments` | One page of an item's thread, walking back from the newest: `?item=` (key or id), `?cursor=`, `?limit=` (default 20, at most 50) — see [`work_comments`](#queries) |
| `GET` | `/work/turns` | One page of the agent turns charged to an item, newest first: `?id=` (key or id), `?cursor=`, `?limit=` (default 20, at most 50) — see [`work_item_turns`](#queries) |
| `GET` | `/pages` | The company's own knowledge base: a filtered listing. Served only where `knowledge.backend` is `native` |
| `GET` | `/pages/{id}` | One page with its body, comments, revision metadata, children and ancestor breadcrumb. `{id}` is the id, or `CONTAINER/Title` — the title matches the way the fleet CLAIMED it, so case and runs of whitespace are ignored and `ENG/deploy runbook` reaches a page called "Deploy  Runbook" |
| `GET` | `/containers` | Every knowledge container this node knows about, with how many pages each holds. The engine materialises one per `space:` the org chart names, plus the two reserved ones, whenever a node publishes a company — every settings apply, every chart write and every boot; each carries `chart_position`, the packed position on the [org chart's log](../concepts/chart-domain.md) its name and purpose were last derived from (absent on a container written before containers carried one), so a node whose chart is older never overwrites them |
| `POST` | `/work/items` `/pages` | **File an item, write a page** — and the rest of the [write surface](#the-human-write-surface): the same tools a seat and your own assistant hold, as the person you signed in as. Guarded, and absent on a company whose tracker or knowledge base is not native |
| `PATCH` | `/work/items/{key}` | Change an item — and its `/comments`, `/rank`, `/depend`, `/relate`, `/restore` and `/purge` beside it. See [below](#the-human-write-surface) for every route and the authority each takes |
| `POST` | `/auth/login` | **Sign in.** Login or address, password, and a second factor where one is held. **Unguarded**, and throttled on the login **as typed** from the caller's source: a failure costs the next attempt a wait, never a lockout — see [below](#a-failure-costs-a-wait-never-a-lockout). Every failure answers one code at one deadline — see [below](#every-failed-sign-in-is-one-refusal). A node that cannot read the identity estate or record the session answers `503` — and **every `503` under `/auth` carries a `Retry-After`**, so a client can tell "ask again in a moment" from a node that is gone. A write whose outcome nobody can establish — the second factor's spend, the session's own start — is that `503` with the `op_id`, and **never a session**: a code whose spend may not have landed is a code that still works. A success answers `{person, login, seat?, expires_at, position, status}`, and `status` is `signed_in` — or, where `api.auth.local.totp` is `required` and the person holds no second factor, `second_factor_enrolment_required`: the session it opened may only enrol one (see [A required second factor is enrolled before anything else](../concepts/identity-and-access.md#a-required-second-factor-is-enrolled-before-anything-else)). `POST /auth/invite/{id}` and a password `POST /auth/step-up` answer the same shape, restricted on the same rule |
| `GET` | `/auth/config` | What a sign-in page needs to know before anybody has signed in: which backend, and `min_password_length` — the deployment's own `api.auth.local.min_password_length`, never below the engine's twelve, which is exactly the floor every redemption enforces. **Unguarded**, and it carries **no user list and no count of people** |
| `GET` | `/auth/invite/{id}` | **Renders an invitation and never spends it** — a link is followed by mail clients prefetching, scanners and preview cards, and one spent by a GET is an account created for somebody who never saw it. **Unguarded**: holding the link is the credential — the **secret** it carries after the id, presented in the `X-Crewlet-Invite-Secret` header and never in the URL. The link a person follows is the dashboard's screen, `<api.external_url>/dashboard#/invite/<id>.<secret>`, whose fragment no browser sends to a server; the screen calls this route with the two halves apart. Answers the address it is for, who sent it, the password floor, the `seat` it binds (`{handle, name}` as the chart calls it now — absent for an invitation that binds none), and a `login` **proposed** from the address in the person grammar (`jane.doe@example.com` → `jane.doe`, `jane@example.com` → `jane.example`) for the form to pre-fill. Absent, redeemed, expired and a missing or wrong secret are one `410` — the same bytes, so a guessed secret against a leaked id does not say the id exists — and an id nobody issued or a secret that is not the link's is a **failed attempt in the audit trail's tally**: walking ids or secrets is guessing at a link, which the tally shows. It meets no [curve](#a-failure-costs-a-wait-never-a-lockout) — the secret is 256 bits nobody walks, and a curve keyed on the address a link came from is one a stranger there holds shut for everybody else. A link that **proved itself** and is spent — redeemed, expired, its address enrolled — is the same `410` and is not counted: that is the link's holder, or a mail scanner re-reading it, and a guesser who does not hold the link can never reach the difference |
| `POST` | `/auth/invite/{id}` | Redeems it, conferring exactly the grants, reach and seat whoever issued it decided — the enrolment names the invitation as its authority and presents the link's secret, and the record refuses anything the invitation does not cover, so a link spent or aged out between the form and the post is `410 invite_spent`. **Unguarded**. `{secret, login, name, password}`: `secret` is the half of the link after the id, checked exactly as the view checks it; the login is **required** — every person enrols with one, the name their changes are recorded under while they hold no seat. An absent login, or one outside a person's grammar (dotted, `jane.doe`), is `400` naming the rule and a login or address somebody already holds is `409` — without saying who, because a link is evidence of who the caller is and of nothing about anybody else. An invitation that binds a seat claims it **first** and binds the person to it: a seat removed, made an agent's or bound to somebody else since the issue is `410` before anything is written, and one a colleague's bind races is `409` saying the seat is taken, naming nobody. Only a record that could not land is `503`. **Retry until it lands**: the person a redemption creates is derived from the invitation, so every attempt names one person and a redeemer told their login is taken posts another. A link whose address somebody is already enrolled under is `410`, like a redeemed one. The company's **first person** redeems exactly this way: a Tier A token issues their invitation — see [How the first person exists](../concepts/identity-and-access.md#how-the-first-person-exists) |
| `GET` | `/auth/session` | **Who you are**: your id, login, seat, kind, stage, grants and colleague level, and the two instants your proof of identity stops counting — `reauth_at` for an ordinary [step-up](#some-gestures-ask-how-recently-you-proved-who-you-are) gesture and `sensitive_reauth_at` for a sensitive one — with `step_up_due` and `sensitive_step_up_due` saying whether the next one of each will ask you to confirm it — and `status`: `signed_in`, or `second_factor_enrolment_required` for a session that may only enrol a second factor, which is one of the routes such a session reaches |
| `POST` | `/auth/token` | Exchanges a **Tier A bearer** — presented as `Authorization: Bearer`, never a cookie — for a one-hour session cookie. The session **is the token**: it names the token's login, and every request re-composes it from the entry this node holds now — the entry's grants cut to the ceiling, the seat the identity directory binds the token to, stepped up by construction as the bearer is. Removing or renaming the entry ends it on the next request, and so does **putting a new value under the same id**: the cookie is bound to the value it was exchanged with, so rotating a leaked token's value ends every session the old value opened — each announced once as `iam_session_ended` with `credential_changed`, as a removed entry's is, since no record states either. `POST /auth/logout` from it closes it as it closes a person's, and `POST /auth/logout/all` from it and `crewlet iam invalidate-all` end it too. A refused bearer here is answered exactly as on every guarded route: `401`, counted in the audit trail's failure tally, and never slowed or refused on its address — see [A bearer is its own protection](#a-bearer-is-its-own-protection) |
| `POST` | `/auth/step-up` | Confirm who you are on a session that is already valid. The only route here that is **both guarded and throttled**: the caller is known, and unbounded retries against a known person is a password oracle with the enumeration already done. It answers a **fresh session cookie** and **ends the session it replaces** first — a close that does not land is `503` with a `Retry-After` and opens nothing, and a presented session that is no longer live is `401`. The replacement confirms the sign-in rather than repeating it, so it keeps the replaced session's absolute deadline |
| `POST` | `/auth/totp` | Enrol a second factor, replacing any you hold. **Two requests**: the first answers a seed and stores nothing, the second presents a code derived from it — which is the only evidence the authenticator app works. Needs a proof inside `step_up_sensitive`, the second-factor reset's window, and an older one is `403 step_up_required` naming it (`reason`, `window`) as every step-up refusal does. A factor nobody can confirm is stored is `503` with its `op_id`, never "enrolled". The second leg answers `{"status": "enrolled"}` — and, through a session that could only enrol a second factor, **replaces that session**: the restricted one is ended first, a whole one opens keeping its absolute deadline and its proof instant (the password's — the enrolment's code proves a seed, not who holds it, so it opens no fresh step-up window), its cookie is on the response and it is answered beside the status as `session` (the sign-in's own shape, `status: signed_in`). Such a session enrols only while its person holds **no** second factor — decided in the write's own snapshot — so one that has come to hold one since the session opened is `403 second_factor_required` and nothing is stored: a password alone never replaces a factor. The seed is sealed under your own key before it is stored, and opened only to check a code |
| `POST` | `/auth/totp/recovery` | Issue ten fresh single-use codes, retiring the old set. Answered **once**, in the clear; what is stored is their hashes, so a lost set is regenerated rather than recovered. Needs a proof inside `step_up_sensitive`, refused as `POST /auth/totp` is. A set nobody can confirm is stored is `503` with its `op_id`, and the codes are not shown |
| `POST` | `/auth/logout` | End **this** session. The cookie is cleared whatever the write did — a logout that answered 503 would leave somebody looking at a signed-in page on a shared machine. It ends the session behind **either** cookie name — the one this deployment issues, and the other a browser may still hold from before `api.external_url` moved to https, which the guard no longer authenticates — and clears both. Only a session this node's rows still hold is closed and announced as `iam_session_ended`: a cookie past its deadline, revoked, or naming a session a record already ended is cleared and nothing is written, and a node that cannot read its rows records the close without announcing it. **Unguarded**, and that is what makes the promise true: behind the request guard, a node that could not read its identity estate answered `503 identity_unavailable` before the sign-out ran and the cookie stayed set. It verifies every bearer the browser holds itself, and the origin check still judges it |
| `POST` | `/auth/logout/all` | End **every** session you hold, by bumping your own revocation epoch — the one move that is immediate on every node. A revocation nobody can confirm is `503` with its `op_id` rather than a claim that your other sessions ended |
| `POST` | `/auth/logout/{lineage}` | End **one named** session, which is how you sign out of a laptop you left somewhere from the browser you are using. The owner is read from this node's rows and compared against the caller the guard resolved; `fleet:operate` may end one they do not own. A session already over — ended by a record, past its absolute deadline, revoked or invalidated — answers `ended` with nothing written or announced. A node that cannot read its rows answers `503 identity_unavailable` with a `Retry-After` and writes nothing, because the owner the caller is checked against is one of those rows — unlike `POST /auth/logout`, whose lineage comes off the cookie's own signature. A close nobody can confirm is `503` with its `op_id` — the id is derived from the lineage, so asking again is the same operation |
| `GET` | `/viewer` | **Who is asking.** The caller's `login`, the `grants` they hold, the seat the identity directory binds them to — its `handle`, `name` and `kind`, all empty for a credential nobody is bound through — and `owner`, the name the caller's own record (inbox, pins, priorities, personal views) is kept under: the seat for a bound person, the login for everybody else. `acts` names the tools [`/operator/act`](#operatoract--the-dashboards-write-surface) would serve this caller — the catalogue's writes the authority table can admit them to before any object is named, always an array — and `project` is where a create of theirs that names no project files, `""` for a caller bound to no seat or a seat whose team owns no project. A node that cannot yet decide `acts` answers `503` rather than an empty list, which would lock every control the caller holds the authority for. An unbound credential is an **ordinary state**, not an error — a pipeline's token acts under its own login, and binding a person to a seat is a directory row rather than a different credential |
| `GET` | `/chart` | The company's **org chart** — its units, its seats, every `manages:` edge and every unit's lead — with the position the answer was read at. The **runtime half of every object is stripped** unless the caller asks for it AND may read it; the answer says which it got in `runtime`. Its credentials and a seat's address are **masked** either way, as `GET /config` masks the settings. **Always needs a credential** — a signed-in session or a bearer (see [below](#chart--the-org-chart-auth-gated)) |
| `GET` | `/chart/units` `/chart/seats` | One half each, for a client that renders people constantly and the tree once. `/chart/seats` filters: `kind=human` (or `agent`) keeps one kind, and `unheld=true` keeps the seats **nobody in the identity directory is bound to**, asked by the handle each seat was created under so a renamed seat is judged by its binding. `unheld=true` is the **directory's** question, so it takes what the directory's own listing takes — `people:manage` (whoever invites somebody into one of those seats) or `audit:read` — and a reader holding the board's grant alone is refused `403` naming both. Every node holds the directory from boot, company or none, so a node waiting for its first company answers it too; where a read of the directory fails it is `503`, with a `Retry-After`, rather than a list missing a seat or carrying one it should not |
| `GET` | `/chart/units/{key}` | One unit, what it directly holds, and its own history; `404 not_found` for a key nothing answers to |
| `GET` | `/chart/seats/{handle}` | One seat, what it manages, and its own history; `404 not_found` for a handle nothing answers to |
| `GET` | `/chart/history` | The company-wide **reorganisation feed**, newest first: who moved, who was hired, which team was dissolved — quiet changes included. **Takes `audit:read`**: the record of what happened across the whole company, where one object's own history above is the context of the object you asked about and takes the board's read |
| `PATCH` | `/chart/units/{key}` | Edit one unit's content. Its **prose** is whoever leads that unit; changing its `project`, `space` or `channel`, or a body carrying `runtime`, takes `config:write` (see [below](#who-may-write-which-part-of-an-object)) |
| `PATCH` | `/chart/seats/{handle}` | Edit one seat's content, on the same classes — its prose decided by whoever leads **that seat**, and a change to its `project`, `space` or `email` taking `config:write`. It carries no `kind` and no `manages`: both are structure, a batch's `set_kind` and `set_manages` |
| `POST` | `/chart/batch` | One **structural** change: create, move, set a lead, rename, set a kind, set whom a seat manages, remove. One batch is one record, arbitrated against every other structural write in the company. Takes `config:write`, and a batch that **removes** anything takes `fleet:operate` as well |
| `POST` | `/chart/units/{key}/rename` `/chart/seats/{handle}/rename` | Change an object's **address**, as a one-operation structural batch. The former one goes on resolving. Takes `config:write` |
| `POST` | `/chart/import` | Publish one revision's **complete authored structure**, keyed on the revision so a re-import is a no-op. Each edge is `{"object":{...},"parent":...,"lead":...}`, and a seat's edge states its `seat_kind` and its whole `manages` list too — the content writes that follow carry neither. Takes `config:write` |
| `GET` | `/chart/imports` `/chart/imports/{revision}` | Which revision this company's structure is running, and when it landed |
| `GET` | `/chart/check` | The **continuous report**: every way the chart and the applied settings disagree (see [below](#the-continuous-report)). **Takes `audit:read`** — it names every seat nobody in the identity directory holds; the counts ride `/health` for everybody |
| `GET` | `/company/export` | The chart as an authored **document**, whole and unstripped — every unit, every seat with its runtime half, and every seat's `manages:` list under `manages` — for a round trip through a file, its credentials masked as every read of the runtime half is. Takes `config:read` |
| `GET` | `/stream/snapshot` | Dashboard initial-state bundle, served from the in-memory projection (REST fallback for the WebSocket) |
| `WS`  | `/ws/stream` | Live dashboard stream — agents, events, LLM invocations, health |
| `GET` | `/dashboard` | Dashboard shell (`/` redirects here; `/static/{path}` serves its assets) |
| `POST` | `/webhooks/jira` | Receive Jira Data Center webhooks (Cloud arrives via `/webhooks/forge`) |
| `POST` | `/webhooks/slack/{handle}` | Receive Slack Events API deliveries for one seat's app |
| `GET` | `/webhooks/slack-oauth` | OAuth install landing page for `crewlet slack provision` |
| `POST` | `/webhooks/github` | Receive GitHub webhooks — HMAC-SHA256 over the raw body |
| `POST` | `/webhooks/github/{handle}` | The same route addressed to one seat, which is where that seat's own [GitHub App](../integrations/github.md#one-github-app-per-agent) delivers |
| `GET` | `/webhooks/github-app` | Landing page for the per-agent GitHub App flow: converts the one-time creation code, or reports an install (see [below](#get-webhooksgithub-app)) |
| `POST` | `/webhooks/gitlab` | Receive GitLab webhooks |
| `POST` | `/webhooks/confluence` | Receive Confluence Data Center webhooks, HMAC-signed. Cloud arrives on the two routes below instead |
| `POST` | `/webhooks/confluence/{event}` | Receive one Confluence **Cloud** event, authenticated by the shared token the registered URL carries — `X-Crewlet-Token` first, `?token=` as the fallback, because Cloud honours no registration field for a header |
| `POST` | `/webhooks/datadog` | Receive a Datadog monitor alert, authenticated by a constant-time comparison of `X-Crewlet-Token`. Datadog signs nothing, so the token is the whole check: an unset one answers `503`, and one shorter than 26 characters answers `503` too — see [Datadog](../integrations/datadog.md#verification-is-weaker-here-and-that-is-the-providers-ceiling) |
| `POST` | `/webhooks/forge` | Receive Forge events (FIT-verified) |
| `POST` | `/otlp/{token}/v1/{signal}` | Engine-fronted OTLP receiver for [sandbox](../concepts/code-sandbox.md) telemetry (per-run token in the path) |
| `GET` `POST` `DELETE` | `/mcp/{token}` | The [tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box): one running seat's tool surface, served over streamable-HTTP MCP to a coding agent in agent mode. Per-run token in the path; all three verbs because that is what the transport uses |
| `GET` `POST` `DELETE` | `/operator/mcp` | The company's own tracker and knowledge base, served over MCP to **your** AI assistant. **Always needs a credential**, which for an assistant is a [machine token](#post-iamcredentials-mints-a-machine-token) acting as you — it files and moves work (see [below](#operatormcp--your-own-assistant)). The catalogue is built per request: `503 no_active_revision` until this node's first company brings a native backend up, and `404 no_route` on a company that runs neither natively |
| `POST` | `/operator/act/{tool}` | The same catalogue's writes, one tool per request, **as the principal your request resolves to** — the dashboard's write surface. Any credential the guard resolved; every tool is decided by the authority table, as on every other surface, and the `Idempotency-Key` header is required (see [below](#operatoract--the-dashboards-write-surface)). Answers as `/operator/mcp` does before the first company and on a company with no native backend |

> **Auth.** Every route needs a credential, reads included, and three shapes
> of one reach every route: a **session cookie** a person gets by signing in
> (`__Host-crewlet_session` on an https deployment, `crewlet_session` on plain
> http, and only the name the deployment issues is read — the bare name is one
> a sibling host can plant); `Authorization: Bearer` with a **Tier A token**, the deployment's own
> machine credential from `api.auth.tokens`; and `Authorization: Bearer` with a
> **[machine token](#post-iamcredentials-mints-a-machine-token)** (`cwl_pat_…`),
> somebody in the directory — a person's own token or a service account's —
> acting as its owner. The two bearers are told apart by the value's shape,
> which picks how it is checked and admits nothing by itself, and all three
> resolve to the same principal, so no route knows which arrived. **When a
> request carries a header and a cookie, the header decides**: a browser sends
> its cookie whether or not the caller meant to, and an `Authorization` header
> is only ever there because somebody put it there — so a request presenting a
> *wrong* header stays anonymous rather than being upgraded by whatever cookie
> is in the jar. `/ws/stream` reads the same two: a browser cannot set a
> header on a WebSocket, so the dashboard's handshake carries its session
> cookie, and any other client sets the header. **No route reads a credential
> off its URL**, the socket included: a `?token=…` authenticates nobody,
> because a URL lands in proxy logs and browser history. Never guarded: `/health`, `/ready`, `/webhooks/*`,
> `/otlp/*`, `/mcp/*`, the dashboard shell (`/`, `/dashboard`, `/static/*`),
> and the five sign-in routes plus `/auth/invite/*` — a login cannot require a
> login. That is an **exact list and not a `/auth/` prefix**: the same surface
> ends sessions and enrols second factors, and a prefix would put those behind
> no credential at all. See
> [Configuration § Auth](../concepts/configuration.md#auth).
>
> **A token that is present and wrong is refused even where reads are open.**
> Sending a credential says you meant to be somebody, so `/ws/stream` answers
> `401` rather than quietly serving you as anonymous — which is how a revoked
> token goes on appearing to work. A browser presents only its session cookie,
> so a dashboard whose session ended is refused `401` and sends its reader to
> sign in (see below).
>
> **`GET /ws/stream` without an `Upgrade` header** answers `401` for a refused
> credential and `426 Upgrade Required` for an accepted one. That pairing is a
> contract, not an accident: a browser is told nothing about why a WebSocket
> handshake failed — no status, and no close code, because a connection that
> never opened sends no close frame — so the dashboard re-asks over plain HTTP
> to tell "nobody is signed in" from "the engine is down", and sends the reader
> to its sign-in screen on the first. Without it a reader whose session ended
> sees "retrying" for ever. The same re-ask reads a `503 identity_unavailable`
> — a node that cannot read its identity estate yet — and dials again when its
> `Retry-After` says rather than on its own backoff.
>
> **`api.auth.max_grants` clamps a person exactly as it clamps a token.** The
> ceiling is applied when the request is resolved, not written anywhere, so
> lowering it takes effect on this node's next request — including for
> somebody already signed in.
>
> **A person whose seat is gone is `403 seat_unavailable`, naming the seat** —
> and so is a Tier A token the identity directory binds to one, since both
> bindings are resolved through the same chart lookup. Their credential is
> perfectly valid and signing in again changes nothing, so
> `401` would loop a browser through the sign-in page for ever. The one surface
> it does not cover is `/auth/*`, whose subject is a person's own credential
> rather than the seat they hold: ending a session, re-proving identity and
> enrolling a second factor go on working, or an offboarded person would be
> left holding a live cookie with no way to sign out.
>
> **The guard is always mounted**, whether or not Tier A is present. An API
> built without `api.auth` configuration has no token, so no candidate can
> match and every guarded route answers `401` — which is all of them bar the
> exemptions above. There is no way to start a process that serves this
> surface without a guard in front of it.
>
> **A cross-site write is refused by its `Origin`** with `403 csrf_origin`,
> whatever credential it carries — or none. The match is `api.external_url`'s
> own origin plus every `api.auth.allowed_origins` entry. Reads are never
> refused for theirs, and neither are the server-to-server edges (`/webhooks/*`,
> `/otlp/*`, `/mcp/*`), which a server reaches with a credential of its own.
> The sign-in routes are judged like every other write although no credential
> guards them — `POST /auth/login` and `/auth/invite/{id}`
> — because a sign-in posted from somebody else's
> page is how an attacker leaves a victim's browser signed in as somebody the
> attacker controls. See
> [Identity and Access § A cross-site write is refused by its Origin](../concepts/identity-and-access.md#a-cross-site-write-is-refused-by-its-origin).
>
> **Every `/webhooks/*` route fails closed.** They are exempt from the bearer
> token because each verifies its provider's signature instead — so a route
> whose secret is not configured has nothing to verify with, and answers `503`
> with `Retry-After` rather than accepting the delivery. The sender retries and
> the delivery flows once the secret is set; nothing is discarded, and nothing
> unsigned is ever recorded, published, or shown on the dashboard.

Plus the surfaces whose reads are as sensitive as their writes: [`/config/*`](#config--live-config-management-auth-gated), [`/secrets/*`](#secrets--the-companys-credentials-auth-gated), [`/setup/*`](#setting-an-integration-up), [`/chart/*` and `/company/export`](#chart--the-org-chart-auth-gated), and `/operator/*` — [`/operator/mcp`](#operatormcp--your-own-assistant) and [`/operator/act`](#operatoract--the-dashboards-write-surface). The list of which credentials a company has not configured yet is a map of what to attack, and so is the shape of the company itself.

### Every failed sign-in is one refusal

`POST /auth/login` answers `401 sign_in_refused` for every way it can fail — a
login nobody holds, a wrong password, a person suspended, a person removed, a
second factor that does not check out — at the same wall-clock instant,
measured from when the request was admitted.

**Both halves are needed.** A code that distinguished the arms would make the
timing pad pointless, and a delay that distinguished them would make the single
code pointless. A miss takes the same turn at the node's verify cap that a
hit's argon2 verification does, through a decoy that holds it for as long as a
verification takes, because otherwise the *absence* of that cost is the answer.
Every verification and decoy waits for its own address's turn — one at a time
per address, served in turn — so one address's flood queues behind itself
rather than in front of everybody, and a request that goes away while it waits
is answered `503` and counts as no attempt at all. A turn that has begun is
held to its end whether or not its request is still there — a decoy's as much
as a verification's — so hanging up says nothing about which one it was.

What that buys is that this surface is not a roster: a caller cannot learn who
works here, nor test a list of addresses against it.

The exceptions are specific, and each discloses nothing the caller did not
already have:

| Code | Why it is safe to be specific |
|---|---|
| `throttled` | `429` with a `Retry-After`: the seconds until this attempt is admitted. Keyed on the subject **as typed** from the caller's source — never on the source alone, and never on what it resolved to — a name nobody holds climbs the curve exactly as a real one does, so a stranger learns only that they failed recently from where they are, which they already knew. Keyed on the resolved person it would be an oracle — "this account exists and I can slow it down" — which is why the one curve that is, a second factor's, is reached only past the password |
| `second_factor_required` | Reached only by somebody who already passed the first factor, so it discloses nothing to a stranger — and without it a client cannot tell "your password is wrong" from "now type your code", which are different screens. It is also the `403` a session that may only enrol a second factor meets at `POST /auth/totp` once its person holds one — enrolled since, from another session: a password alone never enrols over a factor, and the remedy is to sign in again with it |
| `second_factor_enrolment_required` | A `status` on a successful sign-in, and a `403` from every guarded route but three for the session it opened — reached only by somebody who proved the password, so it says nothing to a stranger. The deployment requires a second factor and this person holds none: the session may read `GET /auth/session`, enrol one at `POST /auth/totp` and re-confirm the password at `POST /auth/step-up`, and sign out at `POST /auth/logout`, which no guard stands in front of — and enrolling replaces it with a whole one. Its own code rather than `step_up_required`, because no fresher password changes the answer |
| `invite_spent` | Read by somebody holding the link, which is already evidence it was issued to them. One code for redeemed, withdrawn and expired — and for a secret that is not the link's, answered in the same bytes as an id nobody issued — because the remedy is the same and telling them apart would say "already used" to somebody whose link merely aged out, or say which ids exist to somebody guessing secrets |

### A failure costs a wait, never a lockout

The sign-in routes are throttled by a **curve**, not a ceiling. Each failure
doubles the wait before the next attempt on the same key — 1, 2, 4, 8, 16, then
30 seconds, and never more — and a correct credential after the wait always
succeeds. A wait of up to five seconds is served inside the request, which a
person at a form reads as a slow answer; a longer one is `429 throttled` with a
`Retry-After` naming it. There is no lockout, because a lockout is something an
outsider can cause: keyed on a login it would shut that person out for as long
as anybody kept typing their name.

One key: the login or address **as typed** (an address folded the way the
directory folds one), from one source — the client's address as the trusted
proxies resolve it, an IPv6 client by its `/64`. It has no allowance, so the
first failure already costs a second, and it catches a run at one account. A
success clears that pair and nothing else: clearing more was a
bypass, since anybody holding an account could sign in as themselves between
guesses at somebody else's and wipe the record of every one. An attempt still
being checked counts as a failure against its pair until it resolves, so a
burst of concurrent guesses at one account is served one after another along
the curve rather than all at once.

**A second factor also climbs the person's own curve.** Somebody holding a
person's password could otherwise guess at the six digits from every address
they have, each a fresh pair — a `/48` of IPv6 is sixty-five thousand of them,
enough to find a code in about an hour. So once the password has proved
itself, the code is also decided on a curve keyed on the person the login
resolved to, the same 1-to-30-second doubling: every
address's wrong codes climb it together, and a wait past five seconds is
`429 throttled`. Keyed on the resolved person here and nowhere else, because
it is reached only past the password, so it tells nobody anything the password
did not. The code that completes the sign-in lifts it, and a curve that reaches
its ceiling is announced as `iam_second_factor_throttled` — somebody holding
that person's password is guessing at their code, and the password is what to
rotate.

**No address is ever refused on its own.** A curve on the source alone —
which this surface had, ten failures free and then the same doubling wait — is
one anybody sharing the address holds shut for everybody else: one stranger's
failure every twenty-five seconds, at any name, kept every sign-in from that
office or proxy at `429`, the right passwords included. So one password tried
against many names from one address meets no curve; it is bounded by the
address's one turn at the node's verify cap — one name per verification,
however many it sends at once — by the password floor and blocklist, and by
the pad, and it is shown by the audit trail's per-client failure tally. A
credential that names nobody — an invitation link — meets no curve at all, and
every refusal of one is still counted in the tally.

The window is fifteen minutes, and **each node keeps its own curve**: nothing
about it is written to the coordination store, so a sign-in never waits on a
round trip. On a fleet of N nodes serving sign-ins, a guesser whose attempts a
load balancer spreads across all of them is admitted up to N times as often as
on one node — still one guess per node every thirty seconds at the ceiling,
each an argon2id verification, against a password of at least twelve
characters that is not on the blocklist, and a second factor behind it for a
person who holds one. A node holds a pair under a keyed digest, never what was
typed. See [identity and access](../concepts/identity-and-access.md#a-failure-costs-a-wait-never-a-lockout)
for why the curve is not shared.

#### A bearer is its own protection

Every guarded route compares the bearer it is handed — a Tier A token, a
machine token or a session cookie — and whether it matched is the whole of the
answer. No curve stands in front of that comparison: a bearer names nobody
until it is compared, so a curve there could only be keyed on the address, and
a refusal decided on an address is one anybody sharing it holds shut for
everybody else — including the break-glass Tier A token. What protects a
bearer is its value: 32 bytes of `crypto/rand` in a machine token, an HMAC
under the fleet's keyring in a session cookie, and at least 26 characters in a
Tier A value, which `crewlet validate` refuses shorter. Every refused bearer on
a guarded route is still a failed attempt in the audit trail's per-client,
per-minute tally, so a spray is seen; it is never answered `429`, however many
valid or refused requests the address has in flight. An unguarded route —
`/health`, `/ready`, the dashboard's shell and assets, the webhooks, the
sign-in routes — never compares a bearer at all: the request is anonymous
there.

**Every source is the proxy's unless you say otherwise.** Behind a proxy that is
not in [`api.trusted_proxies`](../getting-started/configuration.md#tier-a), every
caller shares the proxy's address and therefore one curve per login — a
stranger guessing at somebody's login puts that person's own sign-in behind the
same wait — and every audit row names the proxy.
`crewlet validate` warns when `api.external_url` is `https` and no proxy is
trusted, because the engine never terminates TLS itself.

### Which grant a route needs

Holding a credential is not the same as being allowed to use it here. Every
route and every socket question declares one of the
[eleven grants](../concepts/identity-and-access.md#grants-the-eleven-things-there-are-to-allow),
and a principal that does not carry it is refused — including a Tier A token,
whose declared grants are intersected with `api.auth.max_grants` on every
request.

**`403`, not `401`**, and the difference matters to a client: `401` means
*present a credential* and `403` means *the one you presented does not carry
this grant*. A narrow reader meets the second the moment they open a screen
outside their grants, which is the ordinary case — so it must not be reported
as the first, which tells them to go and get a new credential.

**A `403` says which rule refused, and what would have admitted you.** Beside
`"error": "unauthorized"` it carries two detail fields wherever the authority
table made the refusal — every question, the policy every `/chart/*` and
`/iam/*` route is mounted with, and the snapshot mirrors alike:

```json
{
  "error": "unauthorized",
  "message": "…",
  "reason": "not_self",
  "grants": ["fleet:operate"]
}
```

`reason` is the authority table's own word for the rule that decided —
`no_grant`, `not_self`, `not_lead`, `not_author`, `stage`, `seat_refused`,
`unnamed`, and `step_up` on the one refusal the caller clears themselves
([below](#some-gestures-ask-how-recently-you-proved-who-you-are)) — and `grants` are the capabilities any **one** of which would have
admitted this caller for this object. An empty `grants` is an answer rather
than an omission: no capability would, and what is missing is a relation the
chart does not hold. The [human write surface](#the-human-write-surface)
answers its own refusals `403 forbidden` in its tools' wording instead.

A request the node **cannot decide** — it is behind its chart log, or holds no
company yet — is `503 unavailable` with a `Retry-After`, never a `403`: a lead
told they lead nobody goes looking for an authority they already hold.

| Grant | What it reaches |
|---|---|
| `state:read` | The company's working state: `/agents`, `/agents/activity`, `/org`, `/tools`, `/schedules`, `/budgets`, `/sandbox-runs`, the reads under `/work/*` and `/pages/*`, `/containers`, `/feed`, `/viewer`, `/stream/snapshot`, `/tokens/*`, `/ws/stream` |
| `audit:read` | The record of what happened: `/events*`, the socket's `event` push and the snapshot's `events` section, any seat's `/agents/{id}/memory` and `/agents/{id}/conversations`, every seat's memory totals (`memory_overview`), a running coding job's tail (`/sandbox-runs/{turn_id}/tail`), the turn, phase, trace and A2A-channel questions on the socket, `/iam/audit`, the org chart's company-wide feed (`/chart/history`) and its continuous report (`/chart/check`), and — beside `people:manage` — the seats nobody holds (`/chart/seats?unheld=true`). Separate from `state:read` because a prompt and a tool argument are the company's most sensitive read |
| `config:read` | Every read under `/config*`, `/company/export`, `/integrations`, `/credential-pool`, `/mcp-servers`, a seat's resolved model chain and tool sources on `/org`, the org chart's **runtime half** (`/chart?runtime=true`) — a seat's model chain, its credentials (masked), its sandbox cell and its `mcp_env` — and the `/setup` and `/secrets` **listings**: they carry no values and still say which credentials a company holds, which it has not set, and when each last changed |
| `secrets:read` | Revealing a credential's value: `GET /secrets/{name}?reveal=true`, which takes `config:read` as well — the value's grant on top of the row's |
| `people:manage` | `/iam/*` — inviting somebody, changing what they carry, suspending them, revoking their sessions, resetting a second factor, removing them — the seats nobody holds (`/chart/seats?unheld=true`), which is what an invitation is sent into, and this node's Tier A token labels (`/iam/node-tokens`, as `audit:read` may too). **The grant that can grant**, and it bounds itself: a caller may not confer a grant they do not hold |
| `work:write` | Filing and moving work — the [write surface's](#the-human-write-surface) item routes — and `/operator/mcp`'s write half. Some of those verbs also ask a RELATION: re-routing, a project's policy and taking an item out of circulation are its project lead's |
| `knowledge:write` | Writing the company's own pages. A rename, the trash and a restore are also the container's lead's; see [the write surface](#the-human-write-surface) |
| `config:write` | **Host access**, conferred like it: the configuration it writes runs commands on every engine host (an `mcp_servers` entry, a seat's `mcp_env`, a `cli-agent` model, a `run_in: self` sandbox), so a holder can run anything there and read whatever a process there can, the keyring included — see [Identity and Access](../concepts/identity-and-access.md#grants-the-eleven-things-there-are-to-allow). Every write under `/config*`, `/chart/batch`, the rename and import routes, a chart object's runtime half and the relations authority is derived from, and `/setup`'s writes — with `secrets:write` as well wherever the write seals a credential — and, on top of the page's own rule, the create, save, rename, trash, restore and purge of a page in the tool-skills container (a comment on one is not gated: a remark is not the skill). It is also the **admin path** over the org chart's prose: a holder corrects any unit's or seat's name, purpose and goal, leading nothing |
| `secrets:write` | `PUT`/`DELETE /secrets/{name}` and `POST /secrets/rekey`, and on top of `config:write` a `/setup` write that seals a credential: a submission carrying one, a provisioning pass, a GitHub App |
| `fleet:operate` | The deployment rather than the company: `/fleet`, every `/work/retention*` route (the maintenance status and the reanchor value included), `/backup`, `/backups`, the two purges (`/work/items/{key}/purge`, `/pages/{id}/purge`) — which no seat may make whatever it holds — and, beside `config:write`, taking an object out of the org chart (a `remove` in `POST /chart/batch`). It is also the **admin path** of every relation rule but the org chart's: a holder is admitted where a lead or an owner would be — on everybody's work, and not on what a seat is told to do |
| `sandbox:run` | Starting a coding run |

A question asked on the socket is decided by the same declaration the REST
route is — one registry, both transports — so there is no way round a grant by
choosing a channel.

### Some gestures ask how recently you proved who you are

A session lives for days, so the gestures that change what a company *is* ask
for a proof of identity taken recently — the **step-up**. It is decided by the
same authority table as the grant, on the same row, so a REST route and a
socket question about one verb cannot disagree, and it is asked only **after**
the grant or relation admitted you: a caller who could never make the gesture
is told what they lack, not sent to confirm who they are first.

| Window | Setting (default) | What asks for it |
|---|---|---|
| `step_up` | `api.auth.session.step_up` (1 hour) | Every write under `/config*` and `/chart*` (a lead editing their own unit included), `/setup`'s writes, `PUT`/`DELETE /secrets/{name}` and `POST /secrets/rekey`, the deployment's own controls — `POST /backup` and every `POST /work/retention*` — and every `/iam` write the row below does not name: `POST /iam/people`, `DELETE /iam/people/{id}`, `POST /iam/invitations`, `POST /iam/credentials`, revoking a machine token through `DELETE /iam/credentials/{id}`, and ending somebody else's sessions |
| `step_up_sensitive` | `api.auth.session.step_up_sensitive` (15 minutes) | Revealing a value (`GET /secrets/{name}?reveal=true`); changing what an enrolled person may do or how they prove who they are — `PATCH /iam/people/{id}`, `POST /iam/people/{id}/mfa/reset`, `DELETE /iam/credentials/{id}` naming a password, a second factor or the recovery codes, and your own `POST /auth/totp` and `POST /auth/totp/recovery`; and `POST /iam/invalidate-all` |
| none | | Every read, the two deployment reads (`GET /work/retention/maintenance`, `GET /work/retention/reanchor`) and the import ledger a client polls (`GET /chart/imports*`) included; ending your own sessions (`DELETE /iam/people/{id}/sessions` naming yourself), which is the first thing to do on finding somebody else in your account — an administrator ending somebody else's asks `step_up`; and every work and knowledge verb — the tools, `/operator/mcp`, `/operator/act` and the human write surface |

A proof that is too old is **`403 step_up_required`**, the code the sign-in
surface answers for the same fact, carrying the window it needs so a client can
ask the person to confirm who they are and send the same request again —
`POST /auth/step-up` with a password and second factor:

```json
{
  "error": "step_up_required",
  "message": "…",
  "reason": "step_up",
  "window": "step_up_sensitive",
  "grants": ["secrets:read"]
}
```

The step-up **replaces** the session it was made from, ending it first and
keeping its absolute deadline.

`window` is spelled as the setting that sizes it. `GET /auth/session` answers
the two deadlines the table judges against (`reauth_at`,
`sensitive_reauth_at`) and whether each is already due, so a screen can say so
before somebody starts rather than after they submit.

**A credential with nobody at a keyboard is fresh by construction** — a Tier A
token (and a session exchanged from one) and the development principal in both
windows, and a personal access or service token in `step_up` only. There is
nothing else any of them could present, and the break-glass credential has to
reach a sensitive gesture on the day nobody can sign in as a person; a machine
token never does, because every sensitive gesture needs a person present and a
token proves nobody is. It is bounded twice over: `secrets:read` and
`people:manage` — the grants behind the sensitive gestures about somebody else
— can never be minted onto one, and it is never proved for the sensitive window,
which closes the ones a person makes about themselves. No tool asks for a proof:
a seat has no keyboard, and `/operator/mcp` is not a step-up surface.

**And a node that cannot read identity answers `503`, never `403`.** The
principal a node could not check and the principal that carries nothing are
the same empty value, and reporting the first as the second tells everybody
holding a good credential that theirs is invalid for as long as the outage
lasts. So an unreadable identity estate is `503 identity_unavailable` with a
`Retry-After`, and only a credential this node positively checked and refused
is a `401` or a `403`.

### A path is taken as it was sent, or refused

A request path carrying a `.` or `..` segment, or an empty one (`//`) — spelled
out or percent-encoded (`%2e%2e`) — is refused `400 non_canonical_path` before
anything reads it, rather than cleaned and redirected. Which routes are exempt
from the credential check is decided from the path, and so is which handler
runs; a path that reads as `/webhooks/…` to one of them and as `/config` to
the other is exactly what an authority gate must never be asked to agree with.
Send the path it resolves to.

### Security headers on every response

Every response the API writes carries four headers, set before its status line
(so a `304` carries them as well as a `200`):

| Header | Value |
|---|---|
| `Content-Security-Policy` | The policy for what the response is (below) |
| `X-Frame-Options` | `DENY` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |

The policy depends on what was served:

| Response | `Content-Security-Policy` |
|---|---|
| The dashboard shell (`/dashboard`), `/favicon.ico` and every `/static/*` asset | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self' https:` |
| The landing pages [`/webhooks/github-app`](#get-webhooksgithub-app) and [`/webhooks/slack-oauth`](#get-webhooksslack-oauth) | `default-src 'none'; img-src 'self'`, then `style-src` and `script-src` naming the `sha256` hash of each page's own inline block (`'none'` where a page has none), then `base-uri 'none'; form-action 'none'; frame-ancestors 'none'` |
| Everything else: JSON, plain text, the redirect from `/`, a `404` or a `401` | `default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'` |

These matter because a script running on this origin acts with the signed-in
person's session — the cookie is `HttpOnly`, so no script can read it, but
every request it makes carries it — and the two landing pages are
unauthenticated pages on that same origin that render values from their query
string. A policy
is per response, so each page carries its own: the dashboard runs only the
bundle it was built into, and a landing page runs only the style and script the
engine wrote into it. No response may be framed by another site.

`form-action` on the dashboard allows `https:` as well as `'self'` for one flow:
creating a seat's GitHub App posts the app manifest as a form to the code host,
which is `github.com` or the GitHub Enterprise Server base the company
configures. A reverse proxy in front of the engine should pass these headers
through unchanged; one that adds its own `Content-Security-Policy` produces two
policies, and a browser enforces both.

**Some answers also carry `Cache-Control: no-store`.** Every response under
`/config`, whose body is the whole company document; a `/secrets` reveal's
value; and **every response under `/auth`**, a refusal and a redirect included —
the request guard's own (`401 invalid_token`, `403
second_factor_enrolment_required`, `503 identity_unavailable`), the origin
check's `403`, the router's `404` and `405`, and every route's — whose answers
are a second-factor seed and the `otpauth://` URI carrying it, recovery codes
shown exactly once, who the caller is, the address an invitation was sent to,
and every sign-in's `Set-Cookie`. The guard sets it for the whole prefix before
anything beneath it writes, so a route added there is covered the moment it is
mounted. Without it a browser's disk cache — or a
shared proxy, for everybody behind it — keeps those after the tab, the session
and the step-up that was needed to read them. `no-store` rather than `private`,
which still lets the browser keep it, or `no-cache`, which only revalidates
what was kept.

Read-side handlers live in the `internal/api` package (one module
per domain — `agents`, `events`, `tokens`, `org`, `fleet`, `mcpstatus`,
`sandbox_runs`, `budgets`, `integrations`, `webhooks`,
`dashboard`, `health`);
`webhooks` and `/config/*` keep a stable external contract, while the
read/stream surface is free to evolve since the dashboard is its only
consumer.

### `/chart/*` — the org chart (auth-gated)

The org chart is **not part of the stored configuration revision**. It is a
domain of its own: every change is one record on an ordered log, arbitrated at
the broker on the subject of the object it changes, with its own history and
its own author. `PUT`/`PATCH /config` therefore refuses a body carrying
`units:` or `roles:` by name — `400 chart_not_writable_here` — and points here.

A company file still carries both halves, and always will: an operator authors
one document describing a company. `crewlet validate` reads it whole and
`crewlet config import` is what divides it — the settings to a revision, the
chart to its log.

#### Who may write which part of an object

Every field of a unit and a seat is in one of four classes, and only the first
is decided by who leads the object:

| Class | Fields | Who may write it |
|---|---|---|
| **Prose** | a seat's `name`, `backstory`, `goal`, `responsibilities`, `behavioral_guidelines`; a unit's `name`, `type`, `purpose`, `goals`, `knowledge_refs` | whoever **leads that object** — the unit's lead for a unit, the seat's lead for a seat — or `config:write` |
| **Authority-bearing relations** | a seat's `project`, `space`, `email`; a unit's `project`, `space`, `channel` | `config:write` |
| **Runtime** | `runtime` (a seat's model chain, its credentials, its sandbox cell, its worker grants, its schedules, its `mcp_env`, its `contact` and `availability`), and `clear_runtime` | `config:write` |
| **Structure** | create, move, lead, kind, whom a seat manages, rename, remove — [`POST /chart/batch`](#structure-is-neither) | `config:write`; a removal also `fleet:operate` |

The **relations** are what somebody's authority is derived from, which is why
leading the object does not reach them: `project` and `space` are which tracker
project and which page container a seat or unit leads, so a lead pointing their
unit at another team's key — or the org root's container — would take over its
removals, its archive and its policy; a unit's `channel` is which chat channel
it answers; and a seat's `email` is whose vendor actions — a Jira comment, a
push — are attributed and routed to it. The **runtime** half is the company's
configuration under another name, and writing it is equivalent to shell on
every engine host, because a stdio MCP server is `exec.Command` with the
config's command.

**A seat's `manages` list is structure**, and a `PATCH` naming it is refused
`400 invalid_body`. It is who the seat's manager is — a lead adding the founder
to a report's list would become the founder's ancestor — and a rename moves the
entries naming its object on the chart's structural subject. While a seat's
content carried the list, a lead's goal edit decided on a node that had not
applied a rename yet sent the list back as it was read and wrote the renamed
entry back onto the retired address; now a batch's `set_manages` states it,
ordered against every rename.

**What a write asks for is what it CHANGES.** A body is full post-state, so a
lead's `PATCH` carries the relations too — and sending back the values they
read (the `email` as the read served it) changes nothing and asks for
nothing. Only a relation whose value differs from
the one the object holds, compared inside the write's own snapshot, asks for
`config:write`. The route can see the runtime half in the body and asks for the
grant before it writes; it cannot see which relations a body changes, so the
domain refuses those — and both refusals are the same answer: `403
unauthorized` with `reason: no_grant` and `grants: ["config:write"]`, the
domain's naming the `fields` that asked. A `503 unavailable` with a
`Retry-After` is a node that cannot decide.

**A body that leaves `runtime` out keeps the runtime half the object has.** It is
the one field that is not full post-state, because the person editing a goal
may neither change that half nor read it back: a lead's `PATCH` of a seat's
prose lands and the seat keeps its model chain and its credentials. Taking the
half away is `"clear_runtime": true`, never an empty or absent `runtime` — a
clear that could be spelled by leaving something out is one a caller makes by
accident. A `runtime` of `null` is refused `400`, since it could mean either;
one stated beside `clear_runtime` is refused `422 refused`, and so is a stated
`runtime` that is not a JSON object.

**A runtime half is held to the company's own rules before anything in it is
sealed.** A changed half that breaks what a company file would be refused for
— a `token_budget` ceiling below 1 or keyed on anything but `day`, `week` or
`month` (`{"year": 5}`), a schedule the scheduler cannot parse, a field the
seat's kind may not carry — is `422 refused` with the rule's own sentence, and
no credential in it reaches the secret store. A half carried from the row is
not re-judged: refusing a lead's goal edit over a half they did not touch would
refuse them for somebody else's change.

**Every chart write asks for a proof inside `step_up`**, a lead's edit of their
own team included: it changes what the company executes. The reads — the import
ledger a client polls among them — ask for none. See
[Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are).

Reads split the same way and **default to stripped**. A caller asks for the
runtime half with `?runtime=true` and gets it only if they also hold
`config:read`; otherwise the answer is served **stripped rather than refused** —
the rows they asked for are rows they may read. Every answer says which it got:

```json
{
  "units": [ … ],
  "seats": [ … ],
  "answer": { "level": "linearizable", "position": "CREWLET_CHART_LOG@1:412", "lag": 0 },
  "runtime": false
}
```

Without that flag a company whose seats declare no runtime at all renders
exactly like a caller who was silently stripped.

**A runtime half you may read is still masked.** Every credential in it — an
`mcp_env` value, the sandbox's `env` and its setup steps' `files` and `env`, a
seat's own Slack, Mattermost and GitHub App credentials — is served only as a
whole `${VAR}` reference and anything else as `"__redacted__"`, found by the
same `secret` tags `GET /config` masks by; a seat's `email` is served
the same way. The writer seals every literal into the
[secret store](../concepts/secret-store.md#what-the-org-chart-puts-here-and-what-it-deliberately-does-not)
before a record is published, so what you normally see is the `${CHART_…}`
reference the value was sealed under, and sending it back changes nothing. A
`"__redacted__"` sent back is **restored** from the row the write patches,
matched by where it sits — a list member by its own name — and refused
`422 refused`, naming the field, where the row holds nothing to restore it
from or the value sits in a list member with no name of its own. The same holds
for a file `GET /company/export` produced, because an import's content writes
are these `PATCH`es: imported back into this deployment every mask is restored,
and imported into one that holds no such row it is refused rather than storing
the marker. A `${CHART_…}` reference the row being written does not already
name is **confirmed** against the secret store: one whose value is still held
is accepted (and kept from the sweep), and one whose value is not — collected
an hour after the last row stopped naming it, or never stored on this
deployment — is refused `422 refused`, naming the field and the name, rather
than written as a reference that resolves to nothing. So a sealed reference in
an exported file is good only while some row still names it.

**A content write never creates its object.** A `PATCH` naming a unit or a seat
the chart does not hold is refused (`422 refused`), and nothing is published. The
refusal names `POST /chart/batch`, which is where an object is created — or,
where the chart knows why it is not there, the removal that took it (with who
made it and the reason given) or the address it was renamed to. The write first waits for this node to apply everything the
structure had been written by, so a `PATCH` straight after the `202` of the
batch that created its object lands rather than being refused — a batch that
creates a new object on another one's retired address, or renames an object
back onto an address it used to hold, included — and a node that cannot catch
up in time answers `503` as it does for any write it is behind on. Only a
removed object's address is refused without that wait, since nothing can ever
be placed on it again.

The same rules are enforced a second time **inside the domain**, against the
grants the authoring party holds — so a surface that skipped its own check
still cannot write a seat's credentials or its relations. A `runtime` identical
to the one the object holds (key order and whitespace aside) changes nothing
and asks for nothing there, while a different one or a clear asks for
`config:write` — which is what the route asked for already on seeing the field.
A refused write publishes nothing and seals nothing: a literal `email` is sealed
into the secret store only once the write is admitted.

#### Structure is neither

A create, a move, a lead change and a removal go through `POST /chart/batch`,
and they take `config:write` whoever leads the team. **A removal takes
`fleet:operate` as well**, because it is the one structural change nothing
undoes: the removed address is tombstoned for ever — no create, rename or import
may take it again — the seat's mailbox goes with it, and every node drops what
is still in flight to the object. That is a purge's reach, and it takes a
purge's grant: an automation holding only `config:write` to apply a
configuration cannot dissolve a team between two of its runs. The route asks for
it the moment a batch carries a `remove`, and the domain asks again at the
record, so a caller holding only `config:write` is refused `403` naming
`fleet:operate` and nothing is published. The domain serialises
every structural record on **one subject for the whole chart**, deliberately:
two reparents through a common ancestor can each be locally valid and jointly
produce a cycle no node could see from the subject it arbitrated on. A caller
that means to move three seats sends three operations in **one** batch — the
batch is the unit that is ordered, and three requests are three chances to land
half a reorganisation.

```bash
curl -X POST localhost:8000/chart/batch \
  -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  -d '{"operations":[
        {"kind":"create_unit","object":{"kind":"unit","id":"platform"}},
        {"kind":"move","object":{"kind":"seat","id":"sre"},"parent":"platform"}
      ]}'
```

Each operation names its `kind` and its `object`, and carries only the fields
that kind reads:

| `kind` | Takes | Notes |
|---|---|---|
| `create_unit` | `parent`, `lead` | The new unit's place and its own lead, so a team is created led in one operation |
| `create_seat` | `parent`, `seat_kind` | The unit the seat sits in, and what holds it — `agent` or `human`, **required**: an agent is the one kind that runs, so it is never a default. Until the seat's content is written it is **incomplete**, and no node places it |
| `move` | `parent` | A unit's lead stays with it |
| `set_lead` | `lead` | The unit stays where it is |
| `set_kind` | `seat_kind` | A seat's kind is structure, so it changes here and never in the seat's content. Making a person's seat an agent's is refused while somebody in the identity directory holds it, naming them — on a node that cannot read the directory too |
| `set_manages` | `manages` | The seat's **whole** `manages` list — seat handles and unit keys, kept as written (folded) whether or not they resolve yet, at most 64 of them; an empty or absent list is a seat that manages nobody. A seat only: a unit's reports are its lead's. A batch that also renames an object the list reaches publishes the list the rename leaves |
| `rename` | `to` | The object is named by the address it answers to **at this point in the batch**, and moves onto `to`; its former address goes on resolving. An operation after it uses the new key, and an object the same batch creates cannot be renamed — create it under the address you mean |
| `remove` | nothing | The removal is named by the address the chart holds the object at — the one it answered to when the batch began — because it is published as a record of its own and applied against the chart as the batch found it. A rename earlier in the same batch is superseded: the object is removed from the address it held, its identity is tombstoned beside it, and the address the rename would have given it is never tombstoned, so it stays free |

An empty `parent` is the org root, an empty `lead` clears the unit's own lead
and an empty `manages` clears the seat's list. A field the kind does not take is refused `422 refused` (`the operation
does not take the field`) rather than dropped, because a batch that dropped it would
answer as though it asked for less than it said.

A batch whose only effect is removals — its other operations, if any, being a
rename of an object it then removes — is published as a removal record instead,
and its answer's `objects` name each object by the address it was removed from;
one that places and removes is refused, because a removal installs a gate
and a record that installed one for some of its objects and not others would
make "does this install a gate" a question about a payload.

#### What a write answers

| Outcome | Status | What to do |
|---|---|---|
| `applied` | `200` | The record is durable **and this node has applied it**, so the next read here sees it |
| `pending` | `202` | Durable at the position in the body; every node will apply it, this one has not yet. Read at that position to see it |
| `unknown` | `503` | Nothing can be established from this node. The body says `"outcome": "unknown"` — which a refusal never does — and carries the operation id as `op_id`: retry with the **same** one, which the route reads back from `Idempotency-Key` |

Retrying an `unknown` under a *fresh* id would write the change twice if the
first had in fact landed, which is the one thing the operation ledger exists to
prevent.

A body carrying a field the route does not read is refused `400 invalid_body`,
naming the field, rather than having it dropped — every write here is full
post-state or a structural gesture, so a dropped field would answer `200` for a
request that asked for more than landed. So is a body holding anything after its
one JSON value but whitespace — a stray `}` or `]`, or a second value.

A refusal by the chart's own rules is `422 refused`, with the rule's own sentence
as its `detail` — the body was well formed and the chart will not take it, so no
reshaping of the body would change the answer. A refused **batch** also names
which operation broke which rule, as fields rather than only in the sentence:
`index` (the operation's position in `operations`, from 0), `rule` (the rule's
name — `the key is taken`, `the key was removed`, `no such parent`, `the move
closes a cycle`, `the unit is not empty`, …) and `object` (the `{kind, id}` that
operation named), so a client that sent five hundred operations knows which one
to take back without parsing the sentence. A content write's refusal names no
operation and carries none of the three. A contention another writer won is
`409 stale` (re-read and write again — nothing about the request was wrong), and
a node that cannot decide **authority** is `503` rather than `403`: a node that
is booting or behind the log cannot say who leads a unit, and `403` would send
somebody to ask for an authority they already hold. Every retryable `503` here
carries a `Retry-After` — estimated from this node's own backlog when it is
behind the chart log, a couple of seconds otherwise — and one waiting cannot
clear carries none: an evicted node, or one holding a chart record it cannot
decode, answers the same however often it is asked.

#### A rename keeps the old address working

`POST /chart/units/{key}/rename` with `{"to": "..."}` changes an object's
address. A key is not an identity — the row is — so the former address goes on
resolving until something else claims it, and a reference somebody typed last
year — a chat mention, a unit's `lead:` naming a handle the seat has since given
up — still finds what it named. Every read carries `former_keys` /
`former_handles` so a client rendering a stale reference can say **why** it
still works rather than reporting it broken, and — once an object has been
renamed — `origin_key` / `origin_handle`: the address it was **created**
under, which is its identity. That one never moves and is never issued to
anything else, while a retired alias may be claimed by a new object, so a
client that keeps its own picture of the chart across renames matches objects
by it rather than by their address. It is absent until the first rename,
while the object still answers to it; the capped former list cannot stand in
for it, because enough renames push the origin off its end.

The **`manages:` entries** naming the object move with it: every entry that
reached it before the rename names its new address after, so it does not hang
on an alias a new object may take or sixteen further renames retire — and since
a seat's list is structure, ordered against the rename, no content write can
put the old address back. An entry
the organisation reads as naming something else is left — one naming a unit by
a key some seat also answers to names the seat, and a unit renamed onto a key a
seat answers to keeps its entries on the retired key, which still reaches it.
See [The org chart](../concepts/chart-domain.md).

The route publishes a batch of one `rename` operation, so it is ordered against
every other structural write: a create of the same address is decided against
the rename and refused, never applied on top of it. Its refusals are the
batch's — `422 refused` naming the rule for an address that is taken, reserved, removed,
somebody's identity or the one the object already answers to — and `rename` is
equally an operation you can put in a `POST /chart/batch` beside others.

#### The continuous report

`GET /chart/check` answers one evaluation over the two halves of the running
company: the chart this node holds and the settings epoch it has applied. It
takes `audit:read`, like the company-wide `/chart/history`: it names every human
seat nobody in the identity directory holds and every one nobody can reach,
which is the directory's question read off the chart. The counts are on
`/health` for every reader.

Nothing can refuse these at a write, and that is the point rather than a
limitation — the two halves are written by different people at different times,
so every finding is reachable through two writes that were each correct when
they were made:

| Kind | Severity | What it means |
|---|---|---|
| `provider_unknown` | error | A seat's model chain names a provider the settings do not declare. The seat runs on the company's fallback model — `default`, else the first provider declared — which nobody chose for it, and bills against it |
| `sandbox_unconfigured` | error | A seat's code gate is open on a company with no sandbox backend |
| `worker_unknown` | warning | A seat's `workers:` narrowing names a template that is gone, so it narrows to fewer workers than the list suggests |
| `reference_dangling` | warning | A `manages:` entry, a unit's lead or a seat's unit resolves to nothing — named on the seat's handle or the unit's key that holds it — or a key of `integrations.gitlab.provisioning.access_levels` names no seat, named on that setting: it grants nothing today and its level to whichever seat is next given the handle |
| `reference_retired` | warning | A **setting** — a GitLab access level's key, or `integrations.datadog.route_to` — names its seat by a handle the seat **no longer answers to**, named on the setting with the handle as written. It still reaches the seat, because a reference resolves through the handles a seat used to have; but a rename in the chart cannot rewrite a setting, and a retired handle other than the one a seat was created under may be given to a later seat, which would then silently take the setting over. Write the seat's current handle |
| `alert_fallback_unrouted` | error | `integrations.datadog.route_to` names **no seat that can be woken** — nobody answers to it, or it is a human seat, whose delivery is dropped as the person's own action — so every alert whose monitor names no owner is verified, counted and delivered to nobody. A company file is refused for one; a settings write cannot be, because the seat is the running chart's, and a chart write removing the seat or making it a person's cannot see the setting. `none` dismisses those alerts on purpose and is not reported |
| `seat_unheld` | warning | A human seat **nobody in the identity directory is bound to**, so no person can sign in and act as it — work routed there waits for somebody who cannot arrive. **Left undecided, not answered,** where the directory cannot be asked: a seat whose holder this node failed to read is counted in the report's `unchecked` instead of being reported as held by nobody, which is the answer an operator would act on |
| `seat_unreachable` | warning | A human seat with no contact identity, so nothing addressed to it reaches anybody on the chat surface this company runs. Validation **admits** such a seat — a person who works only through the dashboard has no chat account to declare — so this report is the only place it is named, and a warning rather than an error because the state is legitimate. Independent of the above and with a different remedy — a seat can have either without the other |
| `schedule_unrunnable` | error | An **enabled** schedule nothing can run, named on its scope — the unit's key or the seat's handle — with the schedule's name: a unit's `each` schedule on a unit with no **direct** agent member (a person and a child unit's seats are never runners), a unit's `lead` schedule whose effective lead is a human seat or nobody at all, or a schedule on a human seat. The scheduler resolves no runner for it and skips it on every tick (logging `schedule_no_runners` when one is due), so it never fires. A company file is refused for one; the chart cannot be, because the schedule is its own object's content while what makes it runnable — a member's kind, a lead inherited from an ancestor — is written on other objects. A disabled schedule is not reported |
| `identity_shared` | error | A seat's **address** or one of its **contact identities** is also another seat's, so routing reaches only one of them — an address the first seat declaring it, a contact identity the last — and this seat never hears a delivery addressed to it, or has a person's word attributed to the other. Reported on the seat routing does not reach, naming the one it does and which surfaces (`email`, `slack`, `jira, confluence`, …). Addresses are compared as the party registry compares them, resolved through **this node's** secret snapshot and folded the same way, so two seats plus-addressing one shared mailbox with their own handles share nothing and are not reported. A node that cannot resolve skips this arm: every address on the chart's rows is a sealed reference, and two references never collide |
| `budget_idle` | warning | An **agent seat's token ceiling** the company's own ceiling makes unable to refuse the seat a turn, one finding per idle window, naming the window: everything a seat spends is the company's spend too, so a company ceiling at or below the seat's, on a window that holds the seat's whole, is always reached first. It is here because the two ceilings live in **two halves** — a seat's in its org chart runtime, raised through `PATCH /chart/seats/{handle}`, the company's in its settings, raised through `/config` — so no write to a running company sees both. Judged by the rule and worded in the sentence `crewlet validate` gives a company file, so the same pair reads the same in both. A warning: it runs exactly as written and admits nothing a working ceiling would refuse, and what it does is mislead a founder into reading headroom the seat does not have |

The **same** evaluation is summarised on `/health` under `consistency`, so a
gauge, a probe and this screen can never disagree about whether something is
wrong. It does **not** move `/health`'s `status`: a company referencing a
provider somebody deleted is a company with a problem, not a node with one, and
taking a node out of rotation over a configuration typo would turn one broken
seat into an outage.

`evaluated: false` means this node could not evaluate at all — it holds no
chart view, or has applied no settings epoch. Check it before the count:
`findings: 0` from a node that read nothing is the most misleading answer this
surface could give. `unchecked`, present only when it is not zero, is the same
rule one arm down: how many human seats this node could not ask the identity
directory about, so `seat_unheld` was not decided for them. Both the report and
`/health`'s `consistency` carry it.

### `/iam/*` — the company's people, credentials and sessions (auth-gated)

**Always guarded, reads included**, for the reason `/secrets` guards its
listing: a map of who can reach a company and how is worth as much to an
attacker as the grants themselves. Nothing under `/iam` is on the exemption
list and nothing ever will be.

| Route | Who |
|---|---|
| `GET /iam/people` | `people:manage` or `audit:read` |
| `POST /iam/people` | `people:manage` |
| `GET /iam/people/{id}` | the person themselves, `people:manage` or `audit:read` |
| `PATCH /iam/people/{id}` | `people:manage` |
| `DELETE /iam/people/{id}` | `people:manage` |
| `POST /iam/invitations` | `people:manage` |
| `GET /iam/people/{id}/sessions` | the person themselves, `people:manage` or `audit:read` |
| `DELETE /iam/people/{id}/sessions` | the person themselves or `people:manage` |
| `POST /iam/people/{id}/mfa/reset` | `people:manage` |
| `GET /iam/credentials[?person=]` | the person themselves, `people:manage` or `audit:read` |
| `POST /iam/credentials[?person=]` | the person themselves, from their own session; `people:manage` for a **service account** only; never a request presenting a machine token |
| `DELETE /iam/credentials/{id}[?person=]` | the person themselves or `people:manage`; a machine token revokes machine tokens only, refused before anything is written. An id naming a password, a second factor or the recovery codes also asks a proof inside `step_up_sensitive` |
| `POST /iam/invalidate-all` | `fleet:operate` **and** `people:manage` — the deployment's grant and the directory's, both, as the record layer holds too. Ends every session **and every machine token** |
| `GET /iam/check` | `people:manage` or `audit:read` |
| `GET /iam/node-tokens` | `people:manage` or `audit:read` |
| `GET /iam/audit` | `audit:read` |

**Every write here asks for a recent proof**, and which window is the
design's; see
[Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are).
Enrolling, inviting and removing somebody, and minting or revoking a machine
token ask `step_up` (an hour by default). Changing what an
enrolled person may do or how they prove it asks `step_up_sensitive` (fifteen
minutes): `PATCH /iam/people/{id}` whatever it carries, a second-factor reset,
and a revocation through `DELETE /iam/credentials/{id}` that names a password,
a second factor or the recovery codes — which the route asks
once it has read what the id names, since the pattern cannot see it — and so
does `POST /iam/invalidate-all`. Ending sessions asks two windows of its two
arms: ending your own asks for none, because it is the first thing somebody
does on finding an intruder in their account — and it ends every machine token
they hold too — while an administrator ending somebody else's asks `step_up`,
since it signs a colleague out of everything.

**The object a route names is a person ID, never a login.** The identity
estate keys on an id precisely because a person changes their login, so a self
check against a mutable name would open somebody else's row the day they
swapped.

**Editing your own row is not a self gesture.** Changing your own grants is
the escalation this estate exists to close, so `PATCH /iam/people/{id}` has no
self path at all — unlike the credential mint and the session end beside it,
which are the two gestures a person legitimately makes about themselves. The
record layer refuses it a second time: a caller may not confer a grant they do
not hold, on anybody, themselves included — on an edit, on a
`POST /iam/people` enrolment and on a `POST /iam/invitations` alike, each
answered `403` naming the grant.

**An auditor reads and never writes.** `audit:read` opens the directory
because "who can reach this company, and how" is the audit question — and it
opens nothing that changes it, because a grant that could end a session is a
grant that can lock a company out of its own engine.

#### Every write answers three ways

`applied` is `200` (`201` on the two routes that hand back what they created)
and means this node has the change, so the next read *here* sees it. `pending`
is `202` with the position, and means the record is durable and every node
will apply it while this one has not yet — read at that position to see it.
`unknown` is `503` with a `Retry-After`, `"outcome": "unknown"` and the op
id, and the only safe retry is the **same** one: send it back as `Idempotency-Key`, because a fresh id
would defeat the ledger that makes the retry safe. Every body carries its
`outcome` and `op_id`.

**Nothing is built on, handed out or announced for an `unknown`.** A removal,
a revocation, a second-factor reset or a credential withdrawn announces its
event only once the record is durable; an invitation whose outcome is unknown
hands out no link and a mint hands out no token, because either would be a
value that answers `410` or `401` the first time somebody uses it. A write this
node refused to decide at all — it is behind, below the trim floor, or holding
a record it cannot decode — is `503 unavailable` with the op id, and a
`Retry-After` only where waiting clears it: a node that is behind or below the
floor carries the identity estate's two seconds, while one that is evicted or
holds a record it cannot decode, a log at its byte ceiling, a record larger
than the broker takes and a refusal the broker named carry **none** — the same
request is refused the same however often it is sent, as on `/chart` and
`/work`. The sign-in surface, `/auth`, follows the same rule on every write it
makes. A write whose snapshot kept moving under it until the framework
gave up is `409 stale`, as on `/chart` and `/work`: nothing about the request
was wrong, and the same request read again lands.

**A gesture that is several records answers its weakest.** A create with a
seat, an edit that moves a login and a stage, and a reset followed by its
revocation are each a sequence, and a step answered `unknown` ends the
sequence there — nothing after it is published over a guess — while a step
still `pending` here makes the whole answer `202`.

A request **without** an `Idempotency-Key` is a new operation every time, with
an op id of its own. Two different edits of one person, a second "end every
session", or a second company-wide invalidation are two operations, and an op
id derived from the object alone made the broker collapse the second into the
first inside its duplicate window — acknowledged, and never applied. Send a key
when you mean a retry, and only then.

**A create is named by its key.** `POST /iam/people` derives the person it
creates, and `POST /iam/invitations` the invitation it issues, from the
operation's key — the `Idempotency-Key` when one is sent, otherwise one minted
for the request and answered as `op_id`. So the retry an `unknown` asks for
names what its first attempt may have created: a person whose address and login
that attempt already claimed is finished rather than refused `409` by its own
first half, and an invitation that landed is answered with its own link and the
deadline it was issued with. The invitation's id and the secret its link carries
are both derived under the company's own key — the id from the operation key and
the secret from the id — so the retry hands back the very link the first
attempt issued, and an operation key is never a way to compute one. A create's key is therefore a **uuid7**, as every `op_id` is,
and any other value is `400 bad_params`. The same key with a *different* body —
another address, another login, other grants — is `409 bad_params` carrying the
`op_id`, and so is the key of an invitation since redeemed or aged out: a retry
is the same request, and a new person or a new link is a new key.

A lost race on an address, a login or a seat is `409` **naming who holds it**.
An authority refusal is `403` and will never land however often it is retried.
A login that is absent or outside its holder's kind is `400` — `POST
/iam/people` requires one for a person as for a machine, because it is the
name their changes are recorded under while they hold no seat: a person's is dotted (`jane.doe`)
and a machine's is coloned (`ci:release`, or `token:<id>` to bind a Tier A
token), at most 64 characters either way, checked on a create and on a rename
alike — and so is a `kind` other
than `person` or `machine`, since a seat belongs to the chart and the engine
is the node. A value outside a bound is `400` too: a `reason` longer than 256
bytes (it is rendered into the authentication trail beside the op, so it
names which cause fired rather than narrating) or a `colleague` level other
than `none`, `read` and `write`. An enrolment checks every one of these
**before its first claim**, so a refused create leaves nothing holding the
address or the login and the corrected retry lands.

#### `POST /iam/credentials` mints a machine token

The owner is the person the **route** names — `?person=`, or the caller — and
never a body field, because that is the value the authority table decided on.
**A person's token is minted by that person alone**: it acts as them and its
value is shown to whoever minted it, so `people:manage` mints for service
accounts and never on a person's account. A person mints their own with no
`?person=` from their own session — `crewlet iam token -login` makes exactly
that request, signing in for it and signing out after.
The body is `{"label", "expires_in_days", "grants", "colleague"}`, every field
optional: omitted, the token carries every grant its owner holds that a token
may carry, at the owner's own reach, for 90 days. The answer is `201` with
`{"id", "person", "token", "grants", "colleague", "expires_at", "position"}`.

| Answer | When |
|---|---|
| `400 bad_params` | No owner: a Tier A token owns no machine tokens, so it names the service account with `?person=` |
| `400 invalid_body` | An expiry in the past or more than 365 days away, a label past 128 bytes, a reach that is no level, an owner already holding 64 live credentials — a revoked or expired one gives up its place to the new token, the earliest to lapse first, so revoking a token nothing uses makes room at once |
| `403` | The request presented a machine token; a **person's** token asked for by anybody but that person; a service account's asked for without `people:manage`; a grant the owner does not hold, or `secrets:read` / `people:manage`; a grant the caller does not hold; a reach wider than the owner's; an owner who may not act |
| `404` | Nobody by that id |

**`Idempotency-Key` is ignored here, deliberately.** A retry that landed once
would hand back the first attempt's record — whose secret was shown to nobody
— beside this attempt's value, a token that verifies against nothing. A mint
answered `unknown` is retried as a new mint, and the one that may have landed
is a token nobody holds, which expires — so its `503` carries
`"outcome": "unknown"` and the `op_id` (which finds the attempt in the trail),
and its `detail` says to mint again rather than, as every other unknown on
this surface does, to send the `op_id` back.

Presented as `Authorization: Bearer cwl_pat_…`, the token acts as its owner —
their seat if they are bound to one — carrying the grants it was minted with
that the owner **still** holds, cut to the node's ceiling. It answers `401`
where the node knows it is no good and `503 identity_unavailable` where the node
cannot tell, including a node that has not yet applied the mint. See [Machine
tokens](../concepts/identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts).

#### An edit moves a login or a seat, the new one first

The claim **is** the move — once it lands the person holds the new one and not
the old — so the release after it only closes the old one's trail, and one that
does not land is logged (`iam_move_release_unrecorded`) rather than failing an
edit that already happened.
`PATCH /iam/people/{id}` is a sequence of records — a seat and a login each
arbitrate on their own subject, and the person's stage and document on the
person's — so it is ordered by what a refusal leaves behind, and **everything
the node can judge is refused before the first record**, so a body's later
fields cannot be refused after its earlier ones have landed:
- what the surface judges alone — a `login` of `""` (a login is never cleared,
  only changed), a `stage` or `colleague` level this build cannot name, a
  `reason` past 256 bytes;
- what the node's rows already say a later record would refuse — a `login` its
  holder's kind's grammar refuses (`400`), a `login` somebody else holds
  (`409`, naming the holder), and `grants` the caller may not confer (`403`).
The seat moves **first**, so a seat the chart does not hold, or one somebody
else is bound to, is refused before anything has landed. A new `login` or
`seat` is **moved**: the new one is claimed first and the old one released
after, so either refusal leaves the person exactly as they were. `seat: ""`
unbinds.
What only a record can decide — a login somebody took a moment ago, a seat
removed from the chart since the read — is refused by that record, and the
steps before it have landed. So a refusal met after the first record carries
`landed`, the fields whose change was made (`seat`, `login`, `stage`), and a
`hint`; an `unknown` met partway carries `landed` too. A
`POST /iam/people` whose seat bind is refused answers the bind's refusal with
`landed: ["person"]` and the person's `id`: the person exists, unbound, and
`PATCH` binds them.

A `seat` may be named by **any handle it answers to** — its current one, one it
used to have, the one it was created under — and the binding records the seat's
**identity**: the handle it was *created* under, which no rename moves. So the
`seat` a person reads back is that identity, a renamed seat still resolves from
it, and naming the seat a person already holds by its new handle is a no-op
(`200`, no record) rather than a move. A seat that has been renamed can never be
bound to a second person under its new name: both names are one seat. A seat
the chart does not hold is `400` (a value that was typed), and one this node's
chart could not be read to resolve is `503` with a `Retry-After`.

#### Values that are shown once

`POST /iam/invitations` answers the invitation URL and `POST /iam/credentials`
answers the token. Neither is stored and neither can be read back: what the
estate holds is the invitation's id and a SHA-256 of the secret its link
carries, and a SHA-256 of the token. An invitation an administrator lost is
re-issued rather than recovered.

The invitation URL is the **dashboard's invitation screen**:

```text
<api.external_url>/dashboard#/invite/<id>.<secret>
```

with the credential in the fragment, which a browser never sends — so neither
half is in any access log on the way to the page. The screen reads
`GET /auth/invite/{id}` with the secret in the `X-Crewlet-Invite-Secret` header
and redeems with `POST /auth/invite/{id}` carrying it in the body. It used to be
the JSON route itself, which a person clicking it in their mail saw as a JSON
document with no form.

`POST /iam/invitations` takes `{email, grants, colleague, seat, reason}`.
`seat` names a seat redeeming the invitation **binds** the person to — by any
handle the chart answers to it by — and must be a **human** seat nobody is
bound to: a seat the chart does not hold and an agent's seat are `400`, and one
somebody holds is `409` naming them. The invitation records the seat's
identity, so a rename before the redemption binds the same seat.

`POST /iam/invitations` on a node with no `api.external_url` is `500
no_external_url`: there is no address a link could point at, and only the
node's own configuration file can supply one.

#### `GET /iam/people` pages on a key the applier writes

A person's id is a uuid7, so ordering by it is creation order and `next` is
the last id of the page. Nothing sorts on a name or an address: both are
ciphertext in every row, so ordering by one would be ordering by the
ciphertext.

`?q=` narrows on the **login and the seat**, which are the only two identity
values this estate holds in the clear. A search over names would have to open
every person in the company to compare one, on every keystroke.

A removal deletes the person's row, so a removed person is simply not listed —
on a node that has not applied the removal yet they are listed as they were.
Ciphertext this node's keyring cannot open — a key dropped from the ring before
`crewlet secrets rekey` moved the values off it, or a restore under a different
keyring — renders as `sealed`: a state the right keyring ends, and never an
outage.

#### `GET /iam/check` walks the whole directory

```json
{
  "findings": [
    {"kind": "binding_dangling", "person": "018f3a9c-…", "login": "jane.doe",
     "seat": "platform-lead",
     "detail": "seat \"platform-lead\" is tombstoned; unbind them, or bind them to another seat"}
  ],
  "position": "CREWLET_IAM_LOG@0:1840",
  "people_with_people_manage": 2,
  "bindings_unchecked": 0
}
```

`kind` is one of `no_people_manage_holder` (listed first: nobody left who can
administer the company except through a Tier A token),
`person_without_credential`, `binding_dangling`,
`grant_clamped_by_ceiling`, `claim_duplicated` and `claim_orphaned`; the table
in [`crewlet iam check`](cli.md#crewlet-iam-check) says what each one means and
what to do.

`binding_dangling` is decided by the **request path's own seat table**, so it
names exactly the people a request would refuse or hold off for want of their
seat: a seat removed, tombstoned or turned into an agent seat, or — on a node
whose chart applier is behind — a hire this node has not applied yet, which
clears by itself. The `detail` says which. The same evaluation raises the
`iam_binding_dangling` [alarm](alarms.md) once a residue has persisted past the
60-second stall grace.

`bindings_unchecked` counts the bound people whose seat this node's chart
**could not judge** — its applier past the stall grace, or a view it could not
read. They are neither reported as dangling nor left out silently, so a report
answered during a chart stall does not read as a clean directory; ask a node
whose chart is current.

A row marked `reserved` is an enrolment whose claims landed and whose person
record has not: it holds its address, login or seat and has no kind, no stage
and no grants. It is how an administrator whose enrolment was refused as
claimed by an id they do not recognise finds what claimed it; it acts as
nobody everywhere, and is never a reason for a 503.

#### `GET /iam/node-tokens` joins this node's Tier A labels to the directory

A Tier A token acts under its own login, `token:<id>`, and a directory row
holding that login is what binds it to a seat. The two are written in two
places by two people — the label in a node's Tier A file, the row through this
surface — so a label mistyped on either names a login nobody holds, and the
token goes on working as itself, unbound, while its operator believes it acts
as a seat. `/iam/check` names a binding whose seat is gone; this names a token
whose row was never there. It is a fact about **one node's** configuration, so
ask each node whose Tier A you want checked.

```json
{
  "tokens": [
    {"id": "ci", "login": "token:ci", "row": "none", "binding": "unbound"},
    {"id": "ops", "login": "token:ops", "row": "held", "person": "018f3a9c-…",
     "stage": "active", "seat": "platform-lead", "binding": "bound"}
  ]
}
```

**Labels, never values**: `id` is the token's label in this node's Tier A.
`row` is `none` when nobody holds the login (the token acts as itself),
`reserved` for an enrolment that stopped after claiming it, and `held` for a
row — whose `person`, `stage` and `seat` follow, the seat named by the handle
it was created under. `binding` is `bound`, `unbound` (a row naming no seat),
`dangling` with a `detail` saying why — the same judgement `/iam/check` reports
a dangling binding from — or `unknown` where this node could not tell. A
directory read that fails answers `503` with a `Retry-After`, never a list
missing a row. It backs the token half of **Settings › People & access**.

#### `GET /iam/audit` pages by position, never by time

Two nodes' clocks are compared nowhere in this engine, so `since` and `before`
are log positions. A caller holding a timestamp passes `at=` instead and the
route resolves it **once**, against the rows' own instants. The answer carries
the position this node had applied when it answered, so a reader can tell a
quiet directory from a lagging node.

Each entry names its `actor` and `actor_kind`, and — where there is one —
`operator_id`, the credential the actor acted through: `pat:<id>` for a
machine token acting as its owner, `session:<lineage>` for one of their
browser sessions, a Tier A token's own login. A token acts as its owner, so
`actor` is the owner either way, and `operator_id` is what tells their token's
gesture from their own. It is absent from an entry the sign-in surface or a
duty wrote (the node acts on nobody's credential), from a removal and a
company-wide invalidation (their records are gates, pinned at their first
version for ever; `iam_session_ended` and `iam_session_generation_bumped`
carry it), and from every entry written before the field existed. During a
rolling upgrade an older node **defers** a record that names a credential —
it has no column to write it into — and applies it once upgraded, so the
directory changes somebody made through `/iam` reach an older node late
rather than without their credential.

### `/config/*` — live config management (auth-gated)

Every `/config/*` route takes a grant: `config:read` for the reads below and `config:write` for the writes, whatever the credential — a Tier A token, a session, or the development principal. A caller without it is refused `403 unauthorized` naming the grant (see [Which grant a route needs](#which-grant-a-route-needs)). A write also asks for a proof of identity inside `step_up`, and a session whose proof is older is refused `403 step_up_required` (see [Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are)). See the [Configuration concept doc](../concepts/configuration.md#auth) for the full auth model.

**Read-only:**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config` | Active revision, redacted (full JSON; `?format=yaml` for YAML) |
| `GET` | `/config/revisions` | Paginated history (newest first), metadata only |
| `GET` | `/config/revisions/{id}` | Single revision including its payload |
| `GET` | `/config/revisions/{id}/diff?against=<uuid\|active>` | Structural diff |
| `GET` | `/config/references` | Every `${VAR}` the active document names, each with the config path of the field that names it, plus the `revision` they were read from |

The dashboard reads four of those facts over the query channel rather than
these routes — `config`, `config_audit`, `config_diff` and `config_entities`,
each operator-gated for the same reason the prefix is. The reference index has
no query of its own; the Secrets screen reads it over REST beside `/secrets`.

**Why the reference index is a route and not a client-side scan.** It answers
"what breaks if I remove this credential", which is the question in front of an
operator about to delete or rename a secret: the config keeps `${VAR}`
**pointers**, so a removed row leaves every pointer at it resolving to the
empty string and the surfaces holding one start refusing deliveries with
nothing naming the row that went away. Deriving it from `GET /config` in the
client would mean a second copy of the `${VAR}` grammar, and the engine has
already paid for that twice: a looser pattern once displayed a literal secret
unmasked, and another once minted a live credential into a variable nothing
reads. It would also miss references the read masks: `GET /config` shows a
credential only when it is one whole `${VAR}`, so `"Bearer ${TOKEN}"` arrives
as `"__redacted__"`. The index is built from the unredacted document and
answers names and paths only, never a value. The path is the operator's own spelling
(`integrations.github.webhook_secret`), the same one a validation failure
reports, and a name with several readers appears once per reader. It carries
the document's own `ETag`, because the index changes exactly when the revision
does.

**Full-document write:**

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/config` | Replace the active revision. The body is JSON or YAML, read the same way whatever `Content-Type` says. Requires a revision summary: an `X-Summary` header, **or** a top-level `_summary` key in the body. Conditional via `If-Match` / `If-None-Match`, see [below](#conditional-requests). `?dry_run=true` checks it and stores nothing, see [Dry runs](#dry-runs) |
| `OPTIONS` | `/config` | `204` with `Allow` and `Accept-Patch: application/merge-patch+json` |
| `PATCH` | `/config` | Merge one or more sections into the active revision, see [below](#patch-config--the-narrower-write). `?dry_run=true` checks it and stores nothing |
| `POST` | `/config/reload` | Re-publish the active document unchanged, so every node re-applies and re-reads the secret store. See [below](#post-configreload-after-a-secret-changes) |
| `POST` | `/config/revisions/{id}/revert` | Create a new active revision whose payload equals revision `{id}` |

#### `PATCH /config` — the narrower write

A [JSON Merge Patch (RFC 7396)](https://www.rfc-editor.org/rfc/rfc7396): send only the sections you are changing, in the shape the document already has.

The registered media type is `application/merge-patch+json`; plain `application/json` and an absent `Content-Type` are accepted too, since every example here sends one of those. **Any other patch format is `415`** with an `Accept-Patch` header naming what would have worked — notably `application/json-patch+json`, an [RFC 6902](https://www.rfc-editor.org/rfc/rfc6902) list of operations, which is a different format this surface does not serve. Editing one list member is what the [per-entity routes](#per-entity-read-and-write) are for — and, for a seat or a unit, the [chart's own routes](#chart--the-org-chart-auth-gated). A patch format that *can* address a list member does not replace them: a patch addresses by structure, and a seat's position in a unit's list is not its identity — so an index-addressed edit rewrites a different seat the moment anything above it moves.

```bash
curl -X PATCH https://engine.example.com/config \
  -H "Authorization: Bearer $TOKEN" -H "X-Summary: raise the executor round cap" \
  -d '{"turn_engine": {"max_tool_rounds": 32}}'
```

- **Deep merge.** `{"providers": {"llm": {"main": {"model": "claude-opus-5"}}}}` changes that model and leaves the provider's type, its keys and every other provider alone.
- **`null` deletes.** `{"integrations": {"gitlab": null}}` removes the section — without it a config surface can only add.
- **Arrays replace.** RFC 7396 cannot address a list element, so `mcp_servers: [...]` in a patch replaces the whole list. Editing one server is what [`PUT /config/mcp-servers/{name}`](#per-entity-read-and-write) is for; inventing a list syntax here would give two answers to one question. A patch naming `roles` or `units` is refused outright (`400 chart_not_writable_here`): a seat or a unit is the [org chart](#chart--the-org-chart-auth-gated)'s, edited one object at a time through `PATCH /chart/seats/{handle}` and `PATCH /chart/units/{key}`. What a replacement does not remove is a field this build cannot represent: see [Fields a newer build wrote survive every write](#fields-a-newer-build-wrote-survive-every-write).
- **Unknown keys are refused**, not ignored. A patch is the edit least visible in a diff, so a typo that silently changes nothing is the worst outcome available, because the caller believes they changed something. That holds whatever the key is set to, `null` included: deleting a key this build does not know is refused rather than ignored, because the write [carries it back](#fields-a-newer-build-wrote-survive-every-write) and the caller would be told a deletion landed that did not.
- **Validated as the whole document it produces.** A section that is fine alone is still refused when it leaves the company invalid.
- Same summary rule and same `If-Match` as `PUT /config`, and a **409** when nothing is active: a patch is defined against a document, and building a company out of one section is not what this route is for.

**`If-Match` matters more here than on the full write.** A `PUT` carries the caller's whole intended document; a `PATCH` is merged against whatever is active at that instant. See [Concurrent writes](#concurrent-writes) for what the engine does and does not guarantee.

#### What a write answers

Every write that stores a revision (`PUT`, `PATCH`, a per-entity `PUT`, a reload and a revert) answers `201` with the revision, its epoch, and what the engine makes of the document it stored:

```json
{
  "revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41",
  "epoch": 42,
  "warnings": [
    {
      "kind": "admission", "ref": "",
      "path": "providers.sandbox.setup", "segments": ["providers", "sandbox", "setup"],
      "seat": "", "unit": "", "from": "", "to": "",
      "message": "providers.sandbox.setup: conflicting settings: duplicate setup step name \"git-auth\": 2 steps carry it ..."
    }
  ]
}
```

- **`warnings`** is what the engine will run but a person should know about. Always a list, empty when there is nothing to say. Each has the same locators as a [problem](#refusals-carry-located-problems) (`path`, `segments`, and the `seat` handle or `unit` name it is about, empty when neither), plus `from` and `to` as display text. Two kinds:
  - `dangling_reference`: a reference that resolves to nothing. `ref` says what carries it: `lead` (a unit's lead, a seat handle), `unit` (a root seat's `unit:`, a unit key), `manages` (one `manages` entry — a seat handle or a unit key — at the index it was written) or `gitlab_access_level` (a key under `integrations.gitlab.provisioning.access_levels` naming no seat).
  - `admission`: an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to) the stored company breaks, with `ref`, `from` and `to` empty. A write that keeps one is refused, so only a reload or a revert of a company stored before the rule answers with one, one beside each entity the violation names.

  A **settings** revision reports no `dangling_reference` at all, and that is a
  property of the split rather than a gap: every reference here — a unit's
  lead, a seat's `manages`, a GitLab access level's key — is answered by the
  **org chart**, which is a domain of its own. Asked of bytes that carry no
  chart, the check would report every reference in the company as dangling, on
  every write, for ever. `crewlet validate` still reports them over an
  authored file, which carries both halves.

**There is no `derived` hierarchy in this answer.** It used to carry the whole
org chart the document produced; a settings revision produces none, and
answering an empty one would tell every client that the company has no seats.
Read the hierarchy from [`GET /org`](#get-org).

#### Dry runs

`PUT /config?dry_run=true`, `PATCH /config?dry_run=true` and `PUT /config/{kind}/{id}?dry_run=true` are the same request, checked in the same order, that store, activate and publish nothing. The dashboard's organization builder sends one on every edit, and its Budgets screen one before it saves the company's own ceilings (a seat's are its [org chart](#chart--the-org-chart-auth-gated) runtime half, which has no dry run), so a check is always exactly the write a save would send. An entity write needs its check more than the whole-document writes do: its caller never sees the rest of the document, so the whole-company validation behind the splice is the only place it learns that a provider fine on its own leaves the company invalid.

```bash
curl -X PATCH "https://engine.example.com/config?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "If-Match: \"$REV\"" \
  -d '{"mission": "Ship the thing"}'
```

A valid check answers `200`:

```json
{"valid": true, "base_revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41", "warnings": []}
```

- **`dry_run` is read before anything else**, and takes exactly `true` or `false`, or nothing. Any other value (`1`, `yes`, an empty value, the parameter twice) is `400 invalid_query`: the two readings of a guess differ by whether the fleet's configuration changes.
- **No summary is needed**, because nothing is stored to record one on. A `_summary` key in the body is still lifted out, so the document checked is the one the write reads.
- **`base_revision_id`** is the revision the check was built on, and `""` when nothing is active. A client whose draft was built on a different revision learns that the configuration moved without a second request.
- **Every other refusal is the write's, in the write's order**: `409 no_active_revision` for a patch with nothing to patch, `409 revision_advanced` for a stale `If-Match`, `412 already_configured` for `If-None-Match: *` on a configured company, and `400` with [problems](#refusals-carry-located-problems) for a document the write would refuse.
- A dry run needs the same token a write does.

#### Refusals carry located problems

A refused document (`400 validation_error`, `400 invalid_patch`, `400 invalid_body`) is the [refusal envelope](#every-refusal-is-one-envelope) with a detail of its own: `detail` (one line per failure) and `hint` beside the `error` code and its `message`, and **`problems`** — the same failures, located and classified, so a client puts each beside the field it is about without parsing anything.

```json
{
  "error": "validation_error",
  "detail": "workers.researcher.model: value not in the allowed set: \"nowhere\" is not a configured provider: providers.llm has zulu. ...",
  "hint": "the WHOLE document a write produces is validated, ...",
  "problems": [
    {
      "path": "workers.researcher.model", "segments": ["workers", "researcher", "model"],
      "kind": "unknown_value",
      "message": "workers.researcher.model: value not in the allowed set: ..."
    }
  ]
}
```

| Field | Meaning |
|-------|---------|
| `path` | The authored path in the whole document that was validated. For a per-entity write that is the document the entity was spliced into, and an entity body it cannot read is placed where that entity sits (`mcp_servers[1].comand` for a typo in the second server). `""` only for a failure that belongs to no place in it, such as a whole document that is not YAML at all |
| `segments` | The same path taken apart: strings for keys, numbers for list indexes. A map key can hold a dot, so read these rather than splitting `path`. `null` when `path` is `""` |
| `kind` | `missing`, `unknown_value`, `out_of_range`, `conflict`, `unknown_field`, `shape`, or `invalid` for anything this build does not classify |
| `message` | The failure's whole line, exactly as it appears in `detail`. A duplicate handle or unit key is one line naming every entity and one problem beside each, so there can be more problems than lines |
| `seat` | The engine-derived handle of the seat the problem is about, when it is about one |
| `unit` | The name of the unit the problem is about, when it is about one |
| `line` | The 1-based line in the text that was sent, for a failure the parser found. A patch's failure found in the merged document names no line, because that text is the engine's merge rather than anything sent |

A refusal carries no `derived` hierarchy, for the reason [a write's answer](#what-a-write-answers) gives: a settings document produces none, and an empty one would read as a company with no seats.

No message repeats a credential. A document read from `GET /config` carries masks, which a write restores from the stored revision before validating, so the values a refusal judges are ones the caller was never shown: a message says what rule a value breaks and never the value, a fragment of it, or its length.

The [`/setup`](#setting-an-integration-up) submissions that change the document answer their `validation_error` with the same `problems`.

#### Conditional requests

`GET /config` and every entity `GET` return an **`ETag`** — the active revision id, quoted. It is the token the write side takes, so a read-modify-write needs no second request to find it.

| Header | On | Meaning |
|--------|-----|---------|
| `If-None-Match: <etag>` | `GET` | `304 Not Modified` when the document has not moved |
| `If-Match: <etag>` | writes | Proceed only against that revision; `409 revision_advanced` otherwise |
| `If-Match: *` | writes | Proceed only if *something* is active; `412` on an unconfigured node |
| `If-None-Match: *` | `/config` writes | Proceed only if **nothing** is configured, on this node **or anywhere in the fleet**; `412 already_configured` otherwise, naming the revision it lost to |
| `If-None-Match: *` | entity `PUT` | Proceed only if **that entity** does not exist: the create-only write that adds an MCP server or an LLM provider. `412 entity_exists` when one does; `400 conflicting_preconditions` beside an `If-Match` |

The bare revision id is accepted wherever an `ETag` is, unquoted, because this surface shipped that form before it had entity tags. `If-None-Match: *` is the only create-only precondition — about the company at `/config`, about the entity at an entity's own address — and every `If-Match` value other than `*` is an entity tag, matched against the active revision and nothing else.

Independently of any header, every write names the revision it derived from as the new revision's parent, and the activation is a compare-and-set on that parent — so a lost update is refused **whether or not** the caller sent a precondition. See [Concurrent writes](#concurrent-writes).

**Nothing under `/config` is cacheable.** Every response the surface writes carries `Cache-Control: no-store`: reads, `304`s, writes, refusals and error bodies, and the `404` and `405` it answers for a path or method it does not serve. A body here is the company's settings: which integrations are wired and where, and the `${VAR}` name behind every credential, and a stored copy would outlive the session and the token that read it. Revalidation still works, because it never depended on a cache: a client that wants a `304` sends `If-None-Match` with the `ETag` it kept.

#### Per-entity read and write

Two collections, `llm-providers` and `mcp-servers`, each readable and writable:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config/{kind}/{id}` | One entity, redacted, with an `ETag`. **The body is the entity itself**, so it goes straight back into the `PUT` |
| `PUT` | `/config/llm-providers/{key}` | Replace one named LLM provider; with `If-None-Match: *`, add one under that key. Every entity `PUT` takes `?dry_run=true`, see [Dry runs](#dry-runs) |
| `PUT` | `/config/mcp-servers/{name}` | Replace one MCP server entry; with `If-None-Match: *`, add one after every server already declared |

A seat and a unit are not collections of the settings: they are the [org chart](../concepts/chart-domain.md)'s, which no revision carries, so this surface lists, reads and writes neither — `/config/roles/{handle}` and `/config/units/{key}` are `404 no_route`, like any other path it does not serve. Read and write them through [`/chart/*`](#chart--the-org-chart-auth-gated): `PATCH /chart/seats/{handle}` and `PATCH /chart/units/{key}` for content, `POST /chart/batch` to hire, open, move or remove one, and `POST /chart/seats/{handle}/rename` or `POST /chart/units/{key}/rename` for its address. A fresh deployment's chart is also seeded from its company file at boot; see [the boot seed](../concepts/control-plane.md#the-boot-seed).

Any other method is `405` with an `Allow` header naming `GET, PUT`. There is no `DELETE`: what names a provider or a server is mostly the org chart's, which no write to `/config` can see (the rename rule below says why), so remove one in the company file, with everything that names it, and run `crewlet config import`, which validates both halves together. A seat or a unit is removed through `/chart/*`.

Why these exist beside the whole-document write: `PUT /config` makes every edit
a company-wide one. A founder changing one provider's model sends back a
document carrying every other provider, every MCP server and every
integration, and a concurrent edit anywhere in it is theirs to lose. Editing one entity narrows
what a write *claims* to have changed, which is what makes the revision summary
mean something.

It is the same write underneath, and that matters more than the convenience:
an entity `PUT` opens the active revision, splices the entity in, restores the
masks the read showed against that same revision, **validates the whole
document**, and stores a new revision. A change that would leave the company
invalid is refused even when the entity itself is fine — a delegate template
naming a provider that no longer exists is exactly the break a per-entity
surface invites, because the caller never sees the rest of the document.

Four rules follow from that:

- **An unknown field is refused, not dropped.** The entity body is read by the
  whole-document parser, JSON or YAML: `modell` where `model` was meant is
  `400 invalid_body` with an `unknown_field` [problem](#refusals-carry-located-problems)
  placed where the entity sits in the document
  (`providers.llm.zulu.modell`), with its line in the body. A decoder that
  ignored what it did not recognise would answer `201` and store the provider
  with its model silently gone.
- **A plain `PUT` never creates.** An id nothing carries is `404 no_such_entity`,
  not a new entity: naming one that is not there is far more often a typo than
  an intent to add one. The intent is SAID with `If-None-Match: *` — the
  create-only write, for the two collections this route writes
  (`mcp-servers`, `llm-providers`): a taken id is `412 entity_exists` rather
  than a replacement, the body's identity must match the path as on any `PUT`,
  and the whole company is validated with the new entity in it. A seat and a
  unit are not addressed here at all (above). The id is looked up before the
  body is read, so a mistyped one is a `404` whatever the body holds.
- **The id in the path is the identity, and a `PUT` never renames.** A body
  whose own identity disagrees with the path is `400 identity_mismatch`, not a
  move: nothing that points at the old identity travels with the splice. A
  provider's key is what every seat's `llm:` chain names, and an MCP server's
  name is the key every `mcp_env` block is keyed on and the prefix its tools
  carry. Send the identity back unchanged and change whatever else you like.
  Most of what names a server or a provider is the org chart's — a seat's and
  a unit's runtime half — which no write to `/config` can see, a
  whole-document `PUT` included: rename it in the company file, together with
  everything that names it, and `crewlet config import` it, which validates
  both halves as one company before it writes either.
- **The same summary and `If-Match` rules apply**, and a node with no
  active revision answers `409 no_active_revision` — there is nothing to splice
  into, and building a company out of one provider is not what this route is for.

### `/secrets/*` — the company's credentials (auth-gated)

Every `/secrets/*` and `/setup/*` route takes a grant, reads included: the
listing alone says which credentials a company holds and when each last
changed. Every node serves them, because every node opens the fleet's
[coordination store](../concepts/coordination.md) that holds the rows.

| Route | Grant |
|---|---|
| `GET /secrets`, `GET /secrets/{name}` | `config:read` |
| `GET /secrets/{name}?reveal=true` | `config:read` **and** `secrets:read`, and a proof of identity inside `step_up_sensitive` — decided before the store is read, so a caller without the second is refused alike for a name that exists and one that does not |
| `PUT /secrets/{name}`, `DELETE /secrets/{name}`, `POST /secrets/rekey` | `secrets:write`, and a proof inside `step_up` |
| `GET /setup/integrations`, `GET /setup/integrations/{kind}`, the two `runs` reads | `config:read` |
| `POST /setup/integrations/{kind}/inputs` | `config:write` — and `secrets:write` as well when the submission carries a credential, supplied or to be minted |
| `DELETE /setup/integrations/{kind}`, `POST /setup/integrations/{kind}/check` | `config:write` |
| `POST /setup/integrations/{kind}/provision`, `POST /setup/integrations/github/app` | `config:write` **and** `secrets:write`: a writing pass is handed a sink that mints and seals, and an app's private key is sealed on its way back |

**Connecting is `config:write` and `secrets:write`.** `/setup` performs no
write of its own — a credential goes through the store `/secrets` serves — so
a caller who could not write a credential there cannot write one here either.
Every `/setup` and `/secrets` write asks for a proof of identity inside
`step_up` as well, and a reveal one inside `step_up_sensitive`; the reads ask
for none (see
[Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are)).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/setup/integrations` | What every integration this build can set up still needs, plus the address third-party apps reach this deployment on. `external_url` carries a single `value`: it is `api.external_url` in Tier A, required once the API is served and resolved before that file is decoded, so the `present`/`resolved`/`reference` triple the Tier B pointer needed describes a state that can no longer occur |
| `GET` | `/setup/integrations/{kind}` | One integration's requirement list and state |
| `POST` | `/setup/integrations/{kind}/inputs` | Supply or generate those values: credentials are sealed, the rest is patched into the company |
| `DELETE` | `/setup/integrations/{kind}` | Disconnect: remove what the integration holds at the third-party app, then its block |
| `POST` | `/setup/integrations/{kind}/provision` | Run the third-party app's provisioning pass: mint what it needs, register its webhook |
| `POST` | `/setup/integrations/{kind}/check` | Run the same pass read-only, to see whether something fixed at the third-party app took |
| `GET` | `/setup/integrations/{kind}/runs` | The passes THIS NODE remembers for one surface, newest first, ten at a time. A pass is executed by whichever node held the surface's lease and is remembered in that node's own process, so the answer carries `scope` saying as much — an empty list on a fleet where another node ran the pass is an honest answer to a question the reader did not mean to ask. It exists because nothing could name a run id: the route below answered one pass and was reachable only by a caller that had just started it |
| `GET` | `/setup/integrations/{kind}/runs/{id}` | One pass, as the node that executed it remembers it |
| `GET` | `/secrets` | Every stored name with its `key_id`, `updated_at`, `updated_by` (the author), `updated_by_kind`, `operator_id` (the credential it was stored through, empty where none made the write) and `source`; `engine_keys` — a count of the engine's own keys, `{"total": N, "by_key": {"<key id>": n}}`, naming none of them; and `identity_values` — the identity estate's sealed values counted the same way, `{"people": {"<key id>": n}, "invitations": {"<key id>": n}}` (a person's name, address and second-factor seed, and the address of every invitation still redeemable), or `null` on a node that runs no identity estate. **Never a value**. A rekey keeps each row's `updated_*` and `source`: it re-seals a value it did not choose, so the row goes on saying who stored it |
| `GET` | `/secrets/{name}` | The same fields for one name. `404 not_found` when it is unset |
| `GET` | `/secrets/{name}?reveal=true` | **Break-glass.** The decrypted value, `Cache-Control: no-store`, logged by name against the authenticated operator |
| `PUT` | `/secrets/{name}` | Store or rotate one value. **The request body is the value**, raw bytes, up to 64 KiB. `?source=` records provenance (default `api`). The author is the caller the guard resolved and never anything the request names; the answer carries `updated_by`, `updated_by_kind` and `operator_id` as the row records them, which is the only confirmation a client that could not choose them gets. `400 invalid_name` when the name is not an environment-variable name |
| `DELETE` | `/secrets/{name}` | Remove one value. `200` either way, with `{"removed": true\|false}` |
| `POST` | `/secrets/rekey` | Re-seal every record not already under this node's `secrets.active_key_id` — the engine's own keys included — and every person's sealed values in the identity estate, one record per person, answering the names of yours it moved, `engine_keys_moved`, a count of the engine's, and `identity` — `{"people": n, "values": n}`, or `null` on a node that runs no identity estate. An outstanding invitation's address is not moved (only a re-issue could carry it again) and is counted by the listing instead. A person whose record nobody could confirm, or whose value would not open, is `500 rekey_incomplete` carrying what did move — run it again, which moves only what is still under an old key. `?key_id=` is refused with `409` when it names a different key |

**The engine's own keys are not addressable here.** The identity directory's
blind-index key shares the bucket under a path-shaped name
(`iam/blind-index-key`). Every route that takes a name answers one `403
reserved_name` before anything else — a reveal, an overwrite and a delete
alike, whatever the caller holds — because a delete or an overwrite would
orphan every address in the directory. See
[the secret store](../concepts/secret-store.md#the-engines-own-keys-share-the-bucket-and-never-the-namespace).

**The name is an environment-variable name, and a write that is not one is
refused.** The store is keyed by the name a `${VAR}` resolves through, so
`gitlab-token` or `my token` would be sealed, listed and read by nothing at
all, a success the operator only discovers when a provider fails to
authenticate hours later. Letters, digits and underscores, starting with a
letter or an underscore. The refusal comes before the body is read, so the
name is what the answer points at. Reading and removing take the name as
given, so a row written before the check can still be inspected and deleted.

**The body is the value, not a JSON wrapper.** A credential is arbitrary bytes
— a PEM key has newlines, a token can hold anything — and an encoding step
between the operator and the sequence the vendor compares is a `401` nobody
can explain.

**Reveal is opt-in on the wire**, not merely in the CLI. Without `?reveal=true`
the route answers what a listing answers for one name, so a browser, a crawl or
a link preview cannot pull a credential out by accident.

**Every value is sealed under the node's keyring.** The store has no plaintext
mode, and there is no node without a keyring to ask about: `crewlet validate`
refuses a Tier A file with no `secrets.keys`, and the engine refuses to start
without one.

**Every refusal here is the engine's envelope** — `error`, the sentence
`message` a screen shows, and the route's own detail beside them:
`reserved_name`, `invalid_name`, `not_found`, and on a rekey
`key_id_mismatch` (with `key_id` and `your_key_id`) and `rekey_incomplete`
(with the `moved` names and the `engine_keys_moved` count).

`crewlet secrets` is the client for all of this — see
[the secret store](../concepts/secret-store.md#which-store-the-cli-writes) for
why the CLI goes through a running node rather than writing the KV itself.

### Concurrent writes

**There is no leader** — any node's API can write the config, and the coordination KV is the shared truth. Writes are **not** serialized by a lock, but a concurrent one is *detected*: the activation is a compare-and-set.

- Every write on this surface reads the active revision, derives from it, and names it as the new revision's parent. That parent is what the flip compares against, so a write that lost is refused with **`409 revision_advanced`** — **whether or not the caller sent `If-Match`**, because the server knows what it read.
- `If-Match: <revision_id>` is still worth sending: it is checked before any work is done, so a caller editing a revision that has already moved is told so without a document being built, validated and stored first.
- A losing write's revision **is kept**, and the `409` (or `412`) names it as `stored_revision_id`. It is stored in the history, valid and inert, so the operator's work survives as history they can revert to. Inert means on the node that served the write too: a revision becomes that node's active one only once the fleet has taken it, so the node goes on serving what it served, never offers the loser to the fleet at a restart, and adopts whichever revision actually won. Unwinding it instead would mean a second write that can itself fail, on the path where something has already gone wrong.
- **A node's active revision follows the fleet.** Once a node applies the fleet's epoch, the fleet's revision is its active one, which is what its `GET /config` serves and what it boots on; its reconciler checks that on every tick and corrects a copy that says otherwise.
- **An unset pointer is not a race.** A node seeded from a file holds a locally-active revision before it has published anything; refusing there would fail every config write on a fresh single-node deployment that had done nothing wrong.
- **A write built on nothing is a create.** A node whose own store is empty answers `404 no_active_revision` on `GET /config`, and it reaches that state while its fleet runs a company: it joined and has not reconciled yet, or its best-effort copy of the fleet's pointer failed. A write there was derived from nothing, so its activation is a **create-only compare-and-set**: it lands only while the fleet has no activation. `If-None-Match: *` also consults the fleet's pointer before anything is built, and answers `412 already_configured` naming the revision the fleet is on. Without both, the dashboard's create flow on such a node replaced the running company outright, which renames it, changes every seat id derived from the name and orphans all of their memory.
- The **boot publish** is deliberately unconditional. Two nodes starting at once may both offer the revision they hold; both are legitimate, last-write-wins is the right answer, and every node converges. It is the *edit* path that must not lose a write.

On a `409`, re-read `/config` and send the edit again.

### Status codes

- `200 OK`: a successful read, or a [dry run](#dry-runs) that found the write valid (`{"valid", "base_revision_id", "warnings"}`)
- `201 Created`: a write produced a new revision; the body is `{"revision_id", "epoch", "warnings"}` (see [What a write answers](#what-a-write-answers)). A per-entity write, a reload and a revert return this too: each created one revision.
- `400 Bad Request`: `invalid_body`, `invalid_patch` or `validation_error`, each with `detail` (the field path and what to change) and [`problems`](#refusals-carry-located-problems); `summary_required` when a write has neither an `X-Summary` header nor a `_summary` body key; `invalid_query` when `dry_run` is anything but `true` or `false`; `identity_mismatch` when a per-entity body renames what the path addresses
- `401 Unauthorized`: missing or invalid bearer token — `invalid_token`, in the same [refusal envelope](#every-refusal-is-one-envelope) every route answers with, written by the guard itself before any route runs
- `404 Not Found`: a revision that is not there, `no_active_revision` on a read before the first write, `no_such_entity` on a per-entity write naming an id the active revision does not carry, or `no_route` for a path under `/config` this surface does not serve
- `405 Method Not Allowed`: `method_not_allowed` for a `/config` path under a method it does not take, with `Allow`
- `409 Conflict`: `revision_advanced` (a stale `If-Match`, or a race with a concurrent writer) or `no_active_revision` (a `PATCH` or a per-entity write on an unconfigured node, or a reload)
- `412 Precondition Failed`: `already_configured` when `If-None-Match: *` meets an active revision, or `no_active_revision` when `If-Match` names a revision and none is active
- `415 Unsupported Media Type`: `unsupported_patch_media_type` when a `PATCH` body is a patch format other than a JSON Merge Patch, with `Accept-Patch`
- `503 Service Unavailable`: `draining` when the node has been told to stop, with a `Retry-After` — see [During a drain](#during-a-drain)

### The `config_audit` query

Recent revision metadata for the dashboard's Configuration and Audit screens. **A query, not a REST route** — there is no `GET /config/audit` in this build; the screen asks the query channel for `config_audit` and gets the same revision records `GET /config/revisions` serves, as a bare array (the one question in the set that is not an object).

```
query config_audit { "limit": <N> }
```

| Parameter | Default | Range | Description |
|-----------------|---------|-------|-------------|
| `limit` | `50` | `1..500` | Number of revisions to return, newest first. An out-of-range number is CLAMPED to the range, and a value that is not a number reads as the default. |

Response (`200 OK`):

```json
[
  {
    "revision_id": "11111111-1111-1111-1111-111111111111",
    "parent_revision_id": "00000000-0000-0000-0000-000000000000",
    "created_at": "2026-05-17T10:31:02.118431Z",
    "created_by": "jane.doe",
    "created_by_kind": "operator",
    "operator_id": "pat:0192f00d-0000-7000-8000-00000000000a",
    "source": "api",
    "summary": "add Designer role",
    "is_active": true,
    "activated_at": "2026-05-17T10:31:02.118431Z"
  }
]
```

Payloads are NOT included — fetch a specific revision via `GET /config/revisions/{id}` for the full JSON.

`created_by` is the revision's author, `created_by_kind` its kind and
`operator_id` the credential it was written through — see [who a write is
attributed to](#who-a-write-is-attributed-to) — on this answer, on
`GET /config/revisions` and on `GET /config/revisions/{id}` alike. The kind is
one vocabulary across every trail the engine keeps (`agent`, `human`,
`operator`, `system`):

| `created_by_kind` | Who wrote the revision | `created_by` |
|---|---|---|
| `human` | A person the identity directory binds to a seat, on `/config` or `/setup` | The seat's handle |
| `operator` | A credential nobody is bound through — a Tier A token, or a person or machine token bound to no seat — on `/config` or `/setup`; or whoever ran `crewlet config import` / `crewlet config rekey` on the host | The credential's login (`token:<id>` for a Tier A token), or the host login (`$CREWLET_OPERATOR`, else `$USER`) |
| `system` | The engine itself: a node seeding the store from its `-company` file at boot, or the reconcile loop's own writes (removing a disconnected integration, recording a discovered site, reloading after sealing a credential) | The node's id for a seed; `reconcile loop` for the loop |

`operator_id` is empty where no credential made the write — every `system`
revision and every host-side CLI write. Read the kind rather than inferring
it from the label: the name spaces overlap. A revision reads the same on every
node: the fleet's activation pointer carries its author, kind and credential
beside its source and creation instant, so a node adopting it records the
origin's rather than its own (see [Control Plane](../concepts/control-plane.md#the-design)).
A kind a newer engine adds arrives as itself.

### `GET /config/revisions/{id}/diff`

A **structural** diff of two revisions — one entry per path that moved, with
the value on each side. `?against=` names the other side and defaults to the
active revision; the direction reads as "what `against` became", so a bare
call answers "what would reverting to this change". The same answer serves the
dashboard as the `config_diff` query.

```json
{
  "from": "00000000-0000-0000-0000-000000000000",
  "to": "11111111-1111-1111-1111-111111111111",
  "changes": [
    { "path": "providers.llm.main.model", "kind": "changed",
      "from": "claude-sonnet-5", "to": "claude-opus-5" },
    { "path": "mcp_servers[1].url", "kind": "added", "to": "https://mcp.example.com/sse" }
  ],
  "changes_total": 2
}
```

`kind` is `added`, `removed` or `changed`, and `from`/`to` are absent on the
side where the path does not exist — which is what makes the first two
readable without consulting `kind`. **Both sides are redacted**, always: a
rotated credential shows as a changed mask and never as either value.

**`changes_total` is how many differences there are; `changes` is how many
this answer carries.** The listing is cut at 500 entries because a response
body and a socket frame have a size budget, so render the total and say what
was left out — a short listing read as the whole comparison is a caller
believing nothing else moved. The two differ only on a document several times
the size of the example company, whose 401 leaves all changing at once still
fits. `crewlet config diff` writes to a terminal, which has no such budget,
and prints every change.

`changes` is always a list: two identical revisions answer `[]` with a
`changes_total` of `0`, never `null`.

---


#### `POST /config/reload`: after a secret changes

Takes no body and changes nothing. It stores a **new revision carrying the
same document** and activates it, which advances the epoch and makes every
node apply again.

That is the gesture a rotated credential needs, and no other route performs
it. A secret lives in the company config as a `${VAR}` pointer, resolved when
a provider or a transport is constructed, from a snapshot taken at apply time.
Writing a new value with `PUT /secrets/{name}` therefore changes nothing in a
running process: the pointer is already correct, so there is no patch to make,
and with no activation there is no apply and no refreshed snapshot.
Re-activating an unchanged revision is exactly why the activation pointer is
append-only rather than keyed on a revision id.

A new revision rather than a re-pointed old one, for the same reason a revert
writes one: the history stays append-only, so "the credentials were reloaded
at 04:12" is a fact somebody can find later. `X-Summary` names it; unset, it
records `reload configuration`.

Answers `201` with the revision, its epoch and its warnings (see
[What a write answers](#what-a-write-answers)),
`409 no_active_revision` when nothing is configured, and
`400 validation_error` when the active document breaks a
runnable rule of this build (a reload is an apply, so it re-publishes only a
company every node can run; correct it with `PUT` or `PATCH`). A document that
breaks only an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to),
such as two sandbox setup steps of one name stored before the rule existed, reloads:
that is how a credential rotation still reaches a company carrying one. Its
answer lists each violation as an `admission` warning.

The command-line equivalent is [`crewlet config activate <UUID>`](cli.md#crewlet-config-activate)
naming the revision that is already current.

#### Stored revisions are read as they are

A read never validates what it reads. `GET /config`, a revision read, a diff,
the reference index, the entity reads and the prior a write restores its masks
from all open a stored revision as it is, even one this build's validator would
refuse: a revision is valid under the build that wrote it, and a later build
(or an older peer still activating during a rolling upgrade) can leave one in
the store that this build refuses. What validates is whatever would RUN a
document, and to the rules its question needs:

- **A write** (`PUT`, `PATCH`, a per-entity `PUT`, a `/setup` submission that
  changes the document) validates
  the whole document it produces against every rule, admission rules
  included. So a revision this build refuses is always readable, a corrected
  `PUT` or `PATCH` always replaces it, and a write that leaves it uncorrected
  is refused with `400 validation_error`, even when the write touched nothing
  near the problem.
- **A reload or a revert** (including a `/setup` submission that only rotates a
  sealed credential, which reloads) validates what it re-activates against the
  runnable rules only. A revert to a revision that breaks one answers
  `400 validation_error` naming the field; a revert to one that breaks only an
  admission rule is accepted, and each node logs `org_admission_warning` when it
  applies it.

#### A revision this node cannot open is refused, whichever route reads it

What a read does not skip is the **seal**. Every one of those readers, and a
revert and a write whose prior it is, opens the revision under the node's
keyring first, and a revision that does not open is answered `409` naming it
(`revision_id`) under one of two codes, because the two causes share no
remedy:

- **`unreadable_revision`**: it is sealed under a key the keyring no longer
  holds. `hint` says to put that key back in the node's `secrets.keys`.
- **`unsealed_revision`**: it was stored **without a seal**, which only a build
  older than the mandatory keyring wrote. A node reads only sealed revisions,
  because the seal is what says a node of this fleet wrote one; a plaintext
  revision could have been written by anything that reached the store. `hint`
  depends on which revision it is. The **active** revision is sealed by
  running [`crewlet config seal`](cli.md#crewlet-config-seal) on the node,
  which stores it sealed and activates it. A **superseded** one is sealed in
  place by nothing, so it can be neither shown, compared nor reverted to; to
  have its document again, import it from your own copy with `PUT /config` or
  `crewlet config import`, which stores it sealed.

These used to be `500 internal_error` everywhere but a revert, and a revert to
an unsealed revision answered `unreadable_revision`, whose message tells the
caller to put back a key that was never involved.

#### Fields a newer build wrote survive every write

During a rolling upgrade an older node holds, byte for byte, documents a newer
node wrote, including settings the older build has no field for. `GET /config`
on the older node cannot show them, because its types cannot hold them, so a
document read there and sent back never names them. Every write keeps them
anyway: `PUT`, `PATCH`, a per-entity `PUT`, a reload and a revert all store a
document built from the stored bytes, and carry back each key the writing
build cannot represent that the write does not name.

- **Only keys the writing build cannot decode are carried.** A key it knows and
  the write left out was removed on purpose and stays removed. A caller of the
  older build cannot name an unknown key at all (the strict reader refuses it),
  so no write through it can mean to remove one.
- **A list member is matched by identity, not by position**: an MCP server or
  a sandbox setup step by its name within its own list, so one that moved
  keeps its settings. A name held twice in the stored document, or empty,
  matches nothing, so no member's setting reaches another. A member of a list
  with no identity keeps nothing the write replaced.
- **A `PATCH` replaces only what it names.** Everything it does not name is
  stored exactly as it was, so a list the patch leaves alone keeps every
  member's settings, identity or not, and a member with no identity loses a
  newer build's settings only when the patch replaces the list that holds it.
- **A reload and a revert store the document exactly as it was stored**, since
  neither changes it.
- **A renamed MCP server or setup step is a new identity**, so it keeps
  nothing of the old one's unknown settings.

## Setting an integration up

Connecting an integration means putting values in two places: a credential
into the fleet's sealed secret store, and everything else into the company
document. `/setup` is the surface that does both, in the one order that is
safe, so the dashboard never has to sequence it and never holds a credential
across two requests.

**Every route takes a grant, reads included**, on the same terms as `/config`
and `/secrets`: this surface answers with the *names* of the credentials a
company holds, which of them are unset, and the pages at each third-party app an
administrator would visit. That is a map of what to attack. Reading takes
`config:read`; connecting takes `config:write`, and `secrets:write` as well
wherever a credential is sealed — see [the grant each route
takes](#secrets--the-companys-credentials-auth-gated).

### The requirement list

`GET /setup/integrations/{kind}` answers what that third-party app needs,
whether or not the company has configured it:

```json
{
  "key": "datadog",
  "configured": false,
  "enabled": false,
  "satisfied": false,
  "inbound_path": "/webhooks/datadog",
  "public_url": "https://engine.example.com/webhooks/datadog",
  "requirements": [
    {
      "field": "webhook_token",
      "label": "Shared token",
      "kind": "secret",
      "config_path": "integrations.datadog.webhook_token",
      "secret_name": "DATADOG_WEBHOOK_TOKEN",
      "required": true,
      "mintable": true,
      "help": "...",
      "where": "...",
      "vendor_url": "https://app.datadoghq.com/integrations/webhooks",
      "blocks": "credential_missing",
      "present": false,
      "resolved": null
    }
  ]
}
```

`kind` is one of `secret`, `url`, `id`, `choice`, `text`, `handle`, `toggle`.
Each third-party app's own package declares its list, so the surface serves a
third-party app it has no screen for and the dashboard renders a third-party
app it has no code for. A `toggle` is a JSON boolean in the document; a
`handle` must name a seat this company has.

A `secret`'s **credential** is never echoed. Its `value` carries the field's
`${VAR}` reference when the document holds one, because that is a *name*
rather than a credential: it says which entry of the sealed store the field
reads, it is already visible through `GET /config` to anybody this surface
answers, and a client that could not see it would have no way to tell
"this reads `SHARED_TOKEN`" from "type here to replace what is behind this
field". A document holding a **literal** in that position sends no `value` at
all, and a composite such as `https://${HOST}/hook` is a literal for this
purpose: it names a variable and carries an address beside it, so it is not a
reference to anything.

`present` and `resolved` are the same two facts `secret_present` and
`secret_usable` are, asked per field: written down, and actually usable in
this process. `resolved` is `null` for a field the document leaves empty,
since there is nothing to resolve.
`blocks` names the [reconcile finding](../concepts/integration-reconcile.md)
that this input being absent produces, which is what lets a row reporting
`credential_missing` offer exactly the fields that clear it.

`mintable` means the engine can generate the value, so nobody should be asked
to invent it. **No route here ever returns a credential.**

### Supplying them

```bash
curl -X POST https://engine.example.com/setup/integrations/datadog/inputs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "if_match": "<revision_id>",
        "values": {"enabled": "true", "route_to": "sre-lead"},
        "generate": ["webhook_token"]
      }'
```

`generate` is separate from `values` on purpose: a client that could send both
under one key would eventually send a weak token by accident, and a field can
be one or the other, never both.

**Any field the engine reads through the resolver may hold a `${VAR}`**, not
only the credentials: the Atlassian organization id, a site address, a cloud
id and an account email are all read that way, so a company can keep them in
the sealed store and name them here. `present` and `resolved` then say the two
things separately, and a reference naming an entry that is not there reports
`resolved: false` rather than passing for a working setting.

A field holding a reference also carries `resolved_value`: what that `${VAR}`
currently reads as. It exists because the links this surface describes are
built out of values, and a reference is a name: the Atlassian API keys page is
per-organization, so an organization id kept in the store would otherwise put
the literal text `${ATLASSIAN_ORG_ID}` in the path. **Never present on a
`secret`**, on a literal (which is already the value), or on a reference
naming nothing.

**A `secret` field's value may be a `${VAR}` instead of a credential.** Sent
one, the route writes that reference into the config path and seals nothing,
so a credential already in the store can serve several fields and rotating it
is one write in one place. It needs no secret store in the answering process,
because naming an entry is not writing one. Anything else in that position is
a credential and is sealed under the field's own name, a composite included.

What the route does, in this order:

1. **Refuses a stale base.** `if_match` names the revision the requirement
   list was read against. A submission built on an older one is refused
   *before anything is sealed*, so a caller working from a stale page does not
   end up with a credential in the store that nothing points at.
2. **Seals every credential**, under the name the third-party app declared or
   one derived as `VENDOR_FIELD[_HANDLE]`. The row records `source: "setup"`.
3. **Patches the document** with the non-secret values and, for a credential
   whose slot was empty, a whole `${VAR}` pointing at the name from step 2.
   A slot that already holds a `${VAR}` is written *through*, which is what
   makes rotating a credential a change to the store and not to the company.
   A slot holding a literal is `409 literal_in_config`, naming the path:
   overwriting it would edit the company from a setup form and destroy a
   credential somebody put there on purpose.
4. **Activates**, through the same merge, validation and compare-and-set
   `PATCH /config` performs. When the pointer needed no change (a rotation)
   it [reloads](#post-configreload-after-a-secret-changes) instead, because a
   value written into the store after the last apply is invisible to every
   running seat until something activates. The answer says `"reloaded": true`
   when that is what happened.

Answers `201 {"revision_id", "epoch", "wrote_secrets", "reloaded", "state"}`.
Refusals: `400 invalid_input`, `400 validation_error`, `404 unknown_kind`,
`409 revision_advanced`, `409 literal_in_config`, `409 no_active_revision`.

### Running the provisioning pass

Some third-party apps need something done *at* them, not just written down: a
webhook registered, a signing secret minted and pushed. That is the third-party
app's provisioning pass, and `POST /setup/integrations/{kind}/provision` is
what runs it.

**The reconcile loop does this on its own.** It runs the same provisioning
function every few minutes with the sink and the public base supplied, because
connecting an integration is the permission: a person named that one app and
handed over an administrator credential for exactly this. This route is the
same work on demand, for an operator who wants a pass to run now rather than at
the next tick, and it holds a fleet lease under its own name so the two never
overlap. Nothing in the dashboard calls it.

`can_provision` on a tool's state says whether this build has a pass for it.
`needs_operator` is present only for a third-party app whose pass still asks
for a credential per run; no third-party app in this build does. GitLab's group
Owner token and Mattermost's system-admin token are ordinary **stored**
requirements now, sealed in the fleet secret store with a `${VAR}` in the
document like every other credential.

They used to be transient, asked for on every pass and dropped the moment it
returned, on the reasoning that a one-time grant held permanently is a
standing power. What that reasoning did not price is the **disconnect**:
removing a service account needs the authority that created it, so with
nothing held there was no way to take one away from here, and every account
the engine created outlived the integration that created it. The credential
is held so that it can be undone, and it is named in `orphaned_secrets` when
an integration is disconnected, so an operator knows exactly what to revoke.

### Disconnecting

`DELETE /setup/integrations/{kind}` **asks**; it does not remove. It answers
`202` and records the intent on the fleet row, and the reconcile loop removes
what the integration holds at the third-party app (the webhooks it registered,
and the accounts it created when asked) before the block leaves the company
document.

That order is the whole design. The block carries the credential the teardown
authenticates with, so dropping it first would strand every webhook and
account with nothing left to authenticate a second attempt. Until the teardown
succeeds the surface reports phase `disconnecting`, labelled **Disconnecting**,
and a failure holds it there and retries rather than letting it drift back to
looking connected.

```json
{ "remove_seats": false, "force": false }
```

`remove_seats` is the console's *"also remove the accounts Crewlet created"*.
It defaults to **false** and is never inferred: the engine's own webhooks come
out either way, because nothing else uses them, but an account is a colleague
at that third-party app with history attached. Mattermost bots are **disabled**
rather than deleted, because deleting a Mattermost user takes its posts with
it.

`force` drops the block immediately without waiting for the third-party app,
answers `200`, and is the way out of a teardown that can never succeed: a
revoked credential, an instance that is gone. It is the operator saying they
will remove what the third-party app holds themselves.

**Either way the removal is recorded as yours.** The intent carries who asked
— the author, their kind and the credential they asked through — and the loop
carries the teardown out for them: the revision that drops the block, each
Slack seat's cleared app credentials and the sealed values the teardown
deletes are recorded under that party, exactly as the `force` path, which drops
the block inside the request, records it. They used to be the loop's own
(`reconcile loop`, of kind `system`), so the one revision that says an
integration went away named no person on the ordinary path. An intent recorded
by a build that did not carry who asked is still finished as the loop's.

Either way the sealed credentials are **named, not deleted**, in
`orphaned_secrets`: one an operator may be sharing with another deployment is
not something a disconnect decides about on its own. `crewlet secrets unset`
is the deliberate path.

> **"Either way" is newly true.** The list was built only on the `force` path —
> the ordinary `202` returned no `orphaned_secrets` field at all — and it walked
> the company-level requirements only, so no seat's own credential was ever
> named on either path: not the token a pass minted under the name that seat's
> `mcp_env` points at, not Atlassian's address slot beside it, not Slack's
> per-seat bot token and signing secret. It is computed once now, before the
> request splits, which is the only moment it can be: every name is derived from
> a `${VAR}` in the company document, and both paths end with that block gone.
> The dashboard shows the list rather than discarding the response and closing.

> **A credential a sibling surface still reads is not on the list.** Jira and
> Confluence normally share one seat credential — Atlassian issues one API
> token per account, and the ordinary place for it is the shared
> `mcp_env.atlassian` block — so disconnecting one product alone used to name a
> token the other went on resolving. Following that list takes the product that
> stayed down. A sibling counts as a user unless it is itself disconnecting,
> which is what makes a whole card work: the dashboard takes the card's
> surfaces in order, each request records its intent before the next is made,
> and the union the dialog shows names the shared credential exactly once.

> **GitHub's per-seat App credentials are on the list too**, and they are the
> ones nobody typed in: the engine converts a manifest and writes what GitHub
> returns, so an agent's `private_key` and its App's webhook secret exist
> without an operator having chosen either name. They also survive a disconnect
> by design — GitHub has no API for deleting an App registration, so the engine
> uninstalls the App and hands over a link to the page a person deletes it from,
> and the key stays valid for something that still exists. That makes this list
> the only place either value is ever mentioned; it named the company-level
> signing secret alone, so two sealed per-seat credentials stayed in the store
> with nothing telling the operator they were there.

Refusals: `503 surface_busy` when a reconcile tick or an operator's own pass is
writing at this surface right now, which is the one refusal here that clears on
its own and carries its own code for that reason: a caller that cannot tell a
race from a fault treats both as terminal. The request waits a
busy surface out for a few seconds first (a tick a moment from finishing is the
common collision) and then names it; repeating the request is correct, because
every step of a disconnect is idempotent. It used to answer `internal_error`,
which a caller can only treat as terminal — the dashboard stopped at the first
refusal, so a collision on the second of Atlassian's three surfaces left the
tool half disconnected. A busy surface is now waited out rather than skipped,
because the order matters: the organization's credential is what removes the
accounts. The `Retry-After` it carries (three seconds) is the whole of how long
the dashboard waits between attempts, for at most 45 seconds in all; a
`surface_busy` with no `Retry-After` is not waited out.

Refusals worth knowing: `409 requirements_outstanding` names the fields still
missing (a pass writes at the third-party app and must not run against a
half-configured integration), `409 no_external_url` when nothing has told
the engine what address third-party apps reach it on, and `409 pass_in_flight`
when another pass for the same third-party app is already running. That last
one is a refusal rather than a queue on purpose: minting twice is not something
a retry should paper over.

`POST /setup/integrations/{kind}/check` runs the **same pass with no sink**,
which is what makes it read-only: every vendor gates its registration and its
minting on having somewhere to seal a credential, so a run without one reads
and reports and writes nothing at the third-party app. It answers "did what I
just fixed at the third-party app take".

A check **does** get `api.external_url`, and the `409
no_external_url` refusal above is the writing route's alone. Withholding
the address from a check made it report the wrong fact: a vendor handed no
base reads that as *this deployment has no inbound address* and reports
`ingress_blocked` owed by an admin — and a check records its findings through
the same fold as everything else, so pressing Check on a healthy company wrote
"every monitor that fires reaches nobody" into the live status row and flipped
the card to **Action required** over a value that was already set. Supplying
the base is not the permission to register; having a sink is.

Both record their outcome on the same fleet integration status the reconcile
loop writes, through the same fold, so a pass run by hand and a tick that runs
a minute later cannot disagree, and Settings › Integrations updates with no
extra plumbing. A pass that **failed** is recorded too, as the loop records
one: phase `activating`, actor `engine`, findings dropped, because a pass that
failed did not observe anything.

`recreate_webhooks` re-registers with a fresh secret and is **destructive
across deployments**: the previous secret stops working everywhere else this
company runs. On GitLab it also rotates every seat's token, which revokes the
credential each agent is currently authenticating with.

**No pass on this surface deletes anything.** Decommissioning a service
account whose seat left the configuration stays a command-line gesture,
because a company mid-edit looks exactly like one that removed a seat.

### Per-seat setup

Two third-party apps put an agent's identity on the **seat** rather than on the
company, because on both of them one app is one bot: Slack, whose credentials
an operator pastes in, and GitHub, whose app the engine creates. Both carry a
`seats` array in their tool state, one entry per agent seat.

Slack's entries are a form: each agent has its own Slack app, so each has its
own bot token and signing secret, and every entry carries its own requirement
list, its own `inbound_path` and its own `satisfied`.

A submission for one of them names it:

```json
{"seat": "sre-lead", "values": {"bot_token": "...", "signing_secret": "..."}}
```

Those write through the **org chart** rather than a merge patch: a merge patch
replaces an array wholesale, and a seat is not in the settings at all any more —
it is its own object on the chart's log, arbitrated on its handle, so two seats'
submissions never contend. The engine addresses the seat by its handle, which
is its identity rather than its position, and everything the submission did
not send stays exactly as stored. The seat's chart record is written as the
caller, [attributed](#who-a-write-is-attributed-to) like every other write
here: a person connecting their seat through their own machine token is its
author, of their own kind, with `pat:<id>` beside them — it used to record the
token as the author, of the operator kind.

Every agent seat is listed, configured or not: the list is what a screen
renders a form from, so leaving out a seat with no app yet would leave an
operator no way to give it one. Human seats are excluded, because a person's
Slack account is not something this engine holds a token for.

**It does not create the apps.** That goes through Slack's app-manifest API,
which authenticates with a configuration token Slack issues only by hand and
which an organisation may decline to allow at all, so
[`crewlet slack provision`](cli.md) remains the automated path where those
tokens are available. What this surface does is make a hand-created app usable
without one: it takes the two values Slack shows on the app's own page and
seals them.

#### GitHub: the app the engine writes

A GitHub seat's entry carries **no requirement list**, and the empty one is
deliberate rather than unfinished: nothing here is typed in. The app is created
from a manifest, and GitHub returns its id, its slug and its private key once,
to the engine, which seals the key and records the rest on the seat. What the
entry answers instead is what is still outstanding for that agent:

```json
{
  "handle": "builder",
  "name": "Builder",
  "requirements": [],
  "tier": "full_access",
  "step": "install_app",
  "action_url": "https://github.com/apps/acme-builder/installations/new",
  "present": true,
  "satisfied": false,
  "detail": "the app exists and nothing has installed it, so it sees no repository and mints no usable token"
}
```

`step` and `action_url` are the two clicks, described under
[One agent's own GitHub App](#one-agents-own-github-app). `tier` is the seat's
access tier, answered before the app exists because it is what the manifest
asks for, and defaulting to `read_only` on a seat whose configuration is
silent.

**`present`** is whether the seat has an app at all, and **`satisfied`** needs
that app installed *and* its sealed key readable by this node. A `${VAR}`
naming a secret the store does not hold is the state that reads as configured
everywhere else while the agent mints no token, so `detail` names the variable
to set. It names the reference, never a key.

An unfinished roster does **not** hold the card open: the company block is what
decides whether deliveries arrive, and a company running apps for three of its
ten agents chose that.

**A seat is never `satisfied` on an integration the company does not declare.**
The roster used to answer about the credential alone, so a seat whose `${VAR}`
resolved read as satisfied over a surface a disconnect had removed from the
document entirely — with `detail` naming the config path the value sits at,
which is a fact about YAML rather than a state. It says the seat is waiting for
the integration to be connected instead.

**Nor is it `satisfied` when the reconcile loop has a finding about it.** What
the document and the sealed store can establish is that a credential exists
where the app looks for one — a real fact, and not the one a green row is read
as. A key deleted at the third-party app leaves the pointer resolving perfectly
while every call the agent makes is refused. The loop is the only thing that
has asked the vendor, so a seat named by a finding on that surface's status row
comes back unsatisfied, carrying the loop's own sentence as its `detail`.
Advisory findings are skipped on their own verdict — a permission wider
than the role asked for is a note on a working agent, and reporting it as a
broken one would contradict the card's own tag.

There is a window this cannot close: between a credential being destroyed at
the vendor and the loop next looking, nothing anywhere knows. That window is
`integrations.check_interval_seconds` (600 by default, floor 60), and
`POST /setup/integrations/{kind}/check` is how to ask immediately.

### One agent's own GitHub App

GitHub is the other per-seat case, and it is not a form. A
[GitHub App](../integrations/github.md#one-github-app-per-agent) is created by
POSTing a manifest from a page carrying the operator's own GitHub session, so
this surface hands the dashboard what to submit rather than collecting values,
and the two clicks that follow are a person's. There is no server-to-server
equivalent, which is why a reconcile pass cannot do this one alone.

**`POST /setup/integrations/github/app`** begins it, naming the seat:

```bash
curl -X POST https://engine.example.com/setup/integrations/github/app \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"seat": "senior-engineer"}'
```

```json
{
  "seat": "senior-engineer",
  "tier": "review",
  "action_url": "https://github.com/organizations/acme/settings/apps/new",
  "manifest": {"name": "Acme Senior Engineer", "public": false, "...": "..."},
  "state": "<sealed token naming the seat and who began the creation>"
}
```

Those three values are what GitHub's manifest flow takes: the browser POSTs the
`manifest` as a form field to `action_url`, carrying `state` on that URL so the
redirect can be tied back to the seat that started — and to whoever started it,
because the callback's writes are theirs. The state is **sealed** under the
fleet keyring rather than signed: it travels through GitHub, a browser history
and every ingress log, and who began a creation is nobody's business there. It
is URL-safe as it stands, and any node opens it, including one that has already
made the next keyring key active. `action_url` is the
organization's own app registration page whenever
`integrations.github.provisioning.org` names one, because an app registered
under a person's account cannot be installed on the organization that owns the
repositories.

Refusals: `400 invalid_body`, `400 seat_required`, `409 no_active_revision`,
`404 no_such_seat`, and `409 no_external_url` when
`api.external_url` is unset. The last one matters more than it
looks: an app is created with its delivery, redirect and setup addresses baked
in, and only a person at GitHub can change them afterwards, so creating one
now would mean creating it again later.

**`GET /webhooks/github-app`** is where GitHub returns the browser, twice. It is
unauthenticated because a redirect carries no engine credential; the `state` is
what stands in its place, and it is validated before anything else happens.

- With a `code`, the engine converts the manifest, **seals the app's private
  key and webhook secret first**, then records `app_id`, `app_slug` and a
  `${VAR}` pointing at the sealed key on the seat, through the org chart as
  [per-seat setup](#per-seat-setup) does. All three writes are **the
  beginner's**: the sealed rows and the seat's chart record name whoever
  began the creation, with the credential they began it through — they used
  to name `setup`, which is nobody. `installation_id` is
  written as `0`: the install is a second act. The page then links to the
  install. The seal comes first because GitHub returns those two values exactly
  once and reissues neither, so a failure after it costs a retry and a failure
  before it costs the app.
- With `?installed=<handle>`, it confirms the install and **writes nothing**.
  There is nothing to convert — installing is GitHub's own act and returns no
  code — and nothing to check the query against either: an agent's app is
  private, so it is installed from the organization's own installations page,
  which sends back no `state`. An unsigned query is therefore never allowed to
  reach the company document. The
  [reconcile loop](../concepts/integration-reconcile.md) adopts the
  installation on its next pass, having *listed the app's own installations* —
  the reading that can tell a real id from a typed one.
- With `?error=`, it renders GitHub's own `error_description`, which is the
  operator's to read (they cancelled, or they may not create apps on that
  organization).

It answers `200` for a completion or an install, `400` for a refusal, a missing
code, or a state or code the engine will not accept, and `503` when this
process has no setup surface behind it. An error message here is always the
engine's own wording and never a quote of GitHub's response body, because that
body carries the app's private key.

**The roster says what is outstanding.** A seat entry in a tool's `seats` array
carries three more fields where a per-seat app has tiers and clicks: `tier` is
the seat's access tier, `step` is what is left as a closed set of `create_app`
and `install_app`, and `action_url` is where a person goes to do it. Two steps
rather than one, because they are two acts minutes or days apart and an
operator who has done the first needs to be told the second is left rather than
shown the same button. `action_url` is **empty for `create_app`**, and that is
not an omission: an app is created by POSTing a manifest, not by following a
link, so the dashboard asks the begin route above for one and submits a form.

### One tool, several surfaces

The engine reaches Atlassian over three of these keys: `jira`, `confluence`
and the Forge relay. Each is its own block in the company document and its own
entry here, and each is written on its own, which is what keeps a submission
atomic on the thing it changes. A screen that presents them as one tool asks
for one section per key and submits one request per section.

The Forge app id is the exception, and it rides with `jira`: one app relays
both surfaces, so it is one value with two consumers. Listing it under both
would be two forms writing one field.


## Live Stream

`/ws/stream` is where the dashboard READS. State comes down it and questions
go up it: the handshake snapshot carries every section a screen needs on first
paint, subsequent pushes carry what changed, and anything fetched on demand — an
agent's LLM history, one event's payload, a trace, a different spend window —
is a query sent on the same socket and answered on it. The socket carries no
write.

REST carries the rest, and it is two things the socket deliberately is not:

- **Writes.** A change to the company's work is `POST /operator/act/{tool}`,
  made as the principal the dashboard's session resolves to (see
  [`/operator/act`](#operatoract--the-dashboards-write-surface)); a change to
  the org chart is `/chart`; a person's directory row is `/iam`; the company
  document is `PATCH /config`; a credential is `/secrets`; an integration's
  setup is `/setup`; a backup is `POST /backup`. A write answers
  with the position it landed at, and the reads it moved are asked again on the
  socket at that position.
- **Guarded reads the query registry does not answer** — the secret names
  (`/secrets`), where each `${VAR}` resolves from (`/config/references`), an
  integration's setup (`/setup/integrations`), the org chart and its runtime
  half (`/chart`, `/company/export`) and the identity directory (`/iam`). They are credential-scoped
  surfaces with their own refusals, read through the dashboard's one REST
  loader rather than mirrored onto the socket.

The REST read routes below are also a public read API, and
`GET /stream/snapshot` is the fallback for a browser that cannot upgrade to a
WebSocket (corporate proxies).

Every named read route is an **adapter**, never a second implementation: it
resolves its path values and hands them to the same answer the socket's query
channel reaches. The generic form `GET /query/{what}?a=b` reaches the same
answers by name and is what the socket's own frames map onto, so the two can
never drift. A question whose source this node lacks — an event log, a
schedule ledger — is left *unregistered* rather than answered empty, so its
route replies `404` with `unknown_query`, which tells it apart from the
`no_route` a path nothing serves answers.

### An event on the wire

One shape, whether it came from the live projection or from the event store,
and the field names are the same either way — a screen shows a live row beside
a historical one, so two spellings of one event would render the two halves of
one list differently:

```json
{
  "id": "…", "type": "chat_message_received", "source": "mattermost",
  "timestamp": "2026-08-25T12:00:00Z", "category": "chat",
  "summary": "…", "actor": "…",
  "trace_id": "…", "span_id": "…", "parent_span_id": "…",
  "failed": false,
  "payload": { }
}
```

`payload` is present only where the event was fetched by id or by trace: a
listing deliberately never selects it, because a page of events with every
payload attached is the query that makes a live screen slow.

### A seat's LLM history

`llm_history` is the seat's **finished** phases, read from the event store of
**every node that ever held the seat** — placement moves a seat, and each node
keeps the phases it ran — one row per `agent_phase_completed`, newest first,
capped at 50, with the answer's `coverage` saying which nodes it heard from
(see [Reading the fleet's history](#reading-the-fleets-history-coverage)). The call
*in flight* is not in it; that is `live.live_call`, which comes from the
projection, and the two are different sources on purpose: the store holds what
completed, memory holds what is happening. A screen renders both with one
renderer, so each history row carries the same fields a live one does —
`turn_id`, `phase`, `iteration`, `model`, `response`, `tool_executions`,
`round_narration`, `partial_round`,
`total_tokens` — plus the envelope's `timestamp` and `failed`. It is the
stored record, so it also carries the phase's price where its own CLI
reported one (`cost_usd`), which the dashboard never reads
([rule 19](dashboard-design.md#rules-a-change-has-to-keep)). A detached
coding run is a row of its own, `phase: sandbox`, published when the run is
collected: its tokens, its `launch_id` (a turn can launch two runs in one
iteration, so the launch is part of that row's identity), its report as
`response` and its `activity_transcript` — see
[each run is published as a phase](../concepts/code-sandbox.md#runs-are-uncapped-each-run-is-published-as-a-phase).
A finished row also carries `duration_ms`, which a live one cannot: it is the
engine's own measurement of the phase, published on the record rather than
reconstructed by pairing it with the `agent_phase_started` that shares its key.
Zero means *not measured* — an agent-mode executor's rounds ran inside a coding
CLI's own loop, in another process — never *took no time*. A phase that failed
before it reached a provider also truncates to zero at this resolution;
`failed` is what separates the two.

It measures the **whole** phase, including a suspend. A detached coding run
parks the executor mid-loop and the phase is re-entered later — after a
restart, possibly on another node — and the parked row carries the clock
across with the rounds and the tokens, so the one record the phase publishes
reports the run rather than the seconds spent collecting its answer.

A finished row carries the phase's **timeline** too, and so does every other
reader of the record — `phases`, `turn`, `trace` and `event` answer the stored
payload verbatim: `started_at` (when this segment began — not published minus
`duration_ms`, which on a resumed phase spans a coding run), `rounds[]` (one
`{round, started_at, duration_ms, model, input_tokens, output_tokens,
cache_read_tokens, cache_write_tokens, tool_calls}` per provider call, the
model's half of the round only), each `tool_executions[]` row's `started_at`,
`duration_ms`, `origin` (`builtin` or `mcp:<server>`) and `server`, the
phase's `cache_read_tokens` / `cache_write_tokens` (a breakdown of
`input_tokens`, never an addition to it), `max_rounds` / `round_ceiling`, a
worker's or a judge's `host_round`, a resumed executor's `launch_id` and the
turn's `work_item`, and `steers[]` — `{round, note_id}` for each person's note
the phase read. The live frame carries the same so far plus
`round_started_at` and the `running_call` in flight. Each is **absent** on a
record an older engine wrote, and on a figure nothing measured — a tool call
nobody timed has no `duration_ms`, never a zero — so a reader treats absent as
*not recorded*. The whole field list, and why each is measured where it is, is
in [what a turn records about its time](../concepts/turn-engine.md#what-a-turn-records-about-its-time).

An unreadable or absent event log costs the history and nothing else: the
answer still carries the seat and its live state.

### What the handshake snapshot carries

One frame, `kind: "snapshot"`, with every section a screen needs on first
paint. Three of them are derived from **configuration** rather than from
anything that has happened, and they are present from the moment the socket
opens — before a single turn has run:

| Section | What it is |
|---|---|
| `agents` | The company's agent seats, each merged with its live overlay. Every seat in the company, not the ones this node runs, because the dashboard is a view of the company. Human seats are excluded — they have no turn, no phase and no spend; they appear in `org` with `"kind": "human"` |
| `org` | The same projection [`GET /org`](#get-org) answers, built for the socket's audience: the charter, the company's resolved `timezone` and its `token_budget`, root-level `roles` and `units` nesting to any depth, with only the fields that projection declares for each — a seat's resolved `llm` chain and `tool_sources` only to an audience holding `config:read` |
| `tools` | The catalogue this node serves, each entry tagged with the `source` that registered it — `builtin` or the MCP server's name. Empty on a node with no active revision, which has no catalogue yet |
| `events`, `sandboxes`, `tokens`, `budget`, `health` | The live projection: what has happened |

Every seat carries **one seat-state vocabulary**, computed by the engine and
never by a client: `activity` is `working`, `needs`, `stopped` or `idle`, and
`stopped_reason` is `paused`, `unplaced`, `budget` or `provider` on a stopped
seat and `null` otherwise. Both keys are on every row, a seat no event has
mentioned included. What each word means, which inputs it is read from and in
what order they win is [Agent States](../concepts/agent-runtime.md#agent-states).
Placement is the **fleet's** lease table, read on every snapshot and on the
five-second tick, so a seat a peer runs reads `idle` or `working` here rather
than being left without a state — which the dashboard used to draw as offline,
on every seat another node held — and a seat no node holds reads
`stopped`/`unplaced`. A token meter report names every capped seat whether or
not anything runs it, and moves a seat's state only when one of its windows is
refusing.

The three company-derived sections are **re-sent on every published company**,
as `seats`, `org` and `tools` pushes (with `schedules`). Nothing else would
correct them: a change that adds, renames or removes a seat produces no event a
projection could learn from, and an overlay merge cannot express a row going
away.

**A published company is not the same thing as a config activation.** The org
chart is a [log of its own](../concepts/chart-domain.md), so a hire, a move, a
rename or a first schedule publishes a company with no revision anywhere in it
— and these four pushes follow every one of them, as the last step of
[the convergence](../concepts/configuration.md#what-follows-a-published-company).
Each is a WHOLE payload that replaces its predecessor: the hub drops the
oldest queued envelope without telling the client, so a delta it dropped would
be unrecoverable.

### Live-state projection (`api/stream` + `LiveState`)

Every node that serves the API maintains an **in-memory projection** of
every agent's current state (`internal/api/livestate.LiveState`, owned by
`internal/api/stream.Service`).  It is fed by the same event stream the
WebSocket fan-out consumes and read in O(1) thereafter — so `/agents`,
`/stream/snapshot`, and the WebSocket handshake never re-derive state from a
multi-query event scan on a request.

What the projection holds is what has **happened**: a phase, an in-flight
call, a spend. Who the seats *are*, how they are organised, what tools they
can reach and what they are scheduled to do all come from the company
document instead — see [the handshake snapshot](#what-the-handshake-snapshot-carries).

**Every seat's live state is keyed by its AGENT ID** — the id derived from
the handle the seat was created under ([organization model](../concepts/organization-model.md)),
which every event a seat publishes carries as `agent_id`. Never by its name:
a seat's name is prose and two seats may share one, and a projection keyed
on it gave both of them every overlay either moved, so each card rendered
whatever the other was last doing. Never by its handle either, which a rename
moves while the events already in flight still carry the old one. An event
that names no agent id moves no seat's state. The roster row a client holds
carries the same `agent_id`, and every overlay the `agents` push carries is
matched to its row by it; the durable seat-scoped reads — a seat's history,
its turns, its phases and its spend — are narrowed by it too.

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
it is still answering.  The field is otherwise ZERO-BASED, so the round a
reader counts is `round_num + 1`, and `-1` is neither round zero nor a missing
value — it is the phase's FIRST round, in flight and not yet answered, and the
frame carries `max_rounds` so a row can say "round 1 of 24" from that moment.
A surface drawing it resolves every case through `lib/seats.ts`'s `roundOf`
(at least 1 while there is a call) and `roundLabel`, whose hint says when the
first round has not come back; the roster once drew a bare dash, and the
stepper a bare "Execute" for as long as a slow first answer took.  `agent_turn_progress` is *stream-only*
(never written to the event store); carrying `live_call` in the
snapshot means a tab that refreshes or reconnects mid-call re-renders
the live row immediately instead of waiting for the next progress
event.  State transitions in the projection are
gated on the event timestamp so out-of-order delivery (measured,
JetStream returns a redelivered message *behind* never-delivered ones
rather than replaying it from the head, and in a fleet several nodes
publish into the event stream at once) can't clobber newer
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
phase transcript live, and the agent's own `live_call` — pushed on
every round — is the in-flight row inside its turn card, replaced by the
completed record when the phase finishes. Progress envelopes carry
`turn_id` / `phase` / `iteration` for this correlation; they are
stream-only and never persisted to the event store.

The **spend rollup** is maintained by the projection too. It HOLDS the
per-phase records for its own 24-hour window (the newest 8 000 of them, so a
company past that many phases in a day sees a rollup covering slightly less
than a day rather than a wrong total) and folds them with
`internal/tokens`, which is the same aggregation the event store's wider
windows are folded with, so changing the window on screen cannot change
what a phase is counted as. It ships in the snapshot and is re-pushed on
the shared 5-second tick after any phase completed, so the Spend screen
and the overview widget stay live without a fetch and without a second
implementation of the aggregation in the browser. What a seat has spent
is that rollup's per-agent row: the projection keeps no second total of
its own.

**The projection is seeded from the fleet's event stores when the process
starts**, after the broadcast subscription is attached and before the HTTP
listener binds. The seed makes three bounded reads, side by side, and each read is bounded by the projection's own limit:

- the newest 400 persisted events, for the feed;
- the newest 8 000 phase records inside the 24-hour spend window;
- the newest three turns of every agent seat, for the seat's `last_turn` and for a turn it left parked.

Each read goes to **every live node**, through the same scatter the history
queries use (see [Reading the fleet's history](#reading-the-fleets-history-coverage)).
Each node's store holds only what that node published, so a seed that read
this node's store alone showed a restarted node only its own share of the
company. Without any seed, every one of these surfaces started at this
process's boot: a restart, a deploy or a node joining a fleet showed an
operator a company that had apparently done nothing, beside a store that
said otherwise. The projection records which nodes answered, as a `coverage`,
and a read that failed makes that coverage incomplete. An event that arrives
both ways is recognised by its id and is listed and counted once, whichever
arrives first. The stream can deliver it before the read, and the read can find a row that the
publishing node wrote inline before the stream delivered it. History is
ordered behind the live rows it predates, and a seat's turn that the stream has
already moved is left as the stream left it. A read that fails is logged as
`live_projection_not_seeded` and costs that history, never the start-up. The
reads run side by side, so a slow read does not use up the time budget of the others.

**The paused seats are seeded from the coordination record**, beside the
reads above and before the bind: a pause taken before this process started is
in no event it will hear, and a paused seat drawn as working is the one state a
person pausing it must not be shown. A failed read is logged as
`seat_pauses_not_seeded`; the pause itself is in force either way.

**The running coding runs are reconciled against the durable run record**. The record is read once before the listener binds and then every 30 seconds. The stream is lossy and in memory, so it cannot say which runs exist, and the record can. See [the running-runs panel](../concepts/code-sandbox.md)
for which of the two wins when they disagree. A reconcile that changed the set pushes it as `sandboxes`, and a reconcile that changed nothing pushes nothing.

### `GET /stream/snapshot`

Single-shot bundle equivalent to the WebSocket handshake's first
envelope: every section a dashboard screen needs on first paint.
Assembled entirely from the in-memory projection — no database
round-trip on the hot path.  Used as a fallback when the browser cannot
upgrade to a WebSocket (corporate proxies, etc.).

**Each section is the caller's**, decided by the grant its push kind takes on
the socket: the whole bundle needs `state:read` (`403 unauthorized` without
it), and the `events` section is present only for a caller who also holds
`audit:read` — absent rather than empty, because an empty feed reads as a
company that has done nothing. `GET /agents`, `GET /org` and `GET /tools` are
the same sections served alone, under the same `state:read`.

**Response**

```json
{
  "health":    { /* the whole health envelope described below */ },
  "agents":    [ { /* /agents row: activity (working | needs | stopped |
                      idle) + stopped_reason (paused | unplaced | budget |
                      provider, or null) + budget meter + live_call (the
                      in-flight LLM call, or null between turns) +
                      last_error (the phase failure that stopped this
                      seat, or null) + turn (the turn the seat is on,
                      or null) + last_turn (the newest turn it ended,
                      or null) + paused ({by, at, reason, stop_running}
                      while a person has the seat paused, or null) */ }, ... ],
  "events":    [ { /* recent event row, newest first — payload-free, plus
                      a `failed` boolean */ }, ... ],
  "sandboxes": [ { /* in-flight detached coding run: turn_id, role,
                      agent_handle, agent_id, coding_agent, sandbox_id,
                      task, status (the run record's own word), started_at,
                      question, audience, work_item, owner, paused_at */ }, ... ],
  "tools":     [ { /* one catalogue entry — see The Tool Catalogue below */ } ],
  "org":       { /* /org payload */ },
  "tokens":    { /* the spend rollup — same shape as /tokens/breakdown */ },
  "budget":    { /* the live org-wide token meter, or null before any node has reported — see below */ },
  "schedules": [ { /* configured schedule + computed next_run */ }, ... ]
}
```

Each `events` row is the payload-free feed shape — `id`, `type`,
`timestamp`, `source`, `actor`, `summary`, `category`, `trace_id`,
`span_id`, `parent_span_id`, `topic` — plus **`agent_id`**, the id of the seat
the event concerns (the store's own `agent_id`, absent for an event about no
seat), which is what lets a client narrow its LIVE rows to one seat exactly as
`/events?seat=` narrows the stored ones — and **`failed`**: `true` when the
work the event reports did not succeed.  It is `true` for an event carrying
its own `failed` field (a phase or turn that died) and for an event type that
*is* a failure (`sandbox_run_failed`, `llm_unavailable`, `budget_exhausted`,
`turn.guard_breach`).  Deciding it once, here, is what lets a dashboard mark
failures without re-deriving them from a type list of its own.

The flag survives a restart: the event-store writer stamps a `failed` tag on
those events, and the projection reads it back when it seeds its feed from the
store at startup. The store's listing (`EventLog.List`) deliberately never
selects the payload column, so without the tag every historical failure would
read back as a success.

### The health envelope

One builder (`App.health`, `internal/api/health.go`) answers `GET /health`,
the snapshot's `health` section and the 5-second `health` push, and all three
carry the **whole** envelope, so no two of them can disagree about whether the
engine is healthy. There is no query for it: a screen reads the push. (The
push once carried three fields while a `stream` query answered the rest, and
five screens polled that query at cadences of their own, so the rail and the
panel in front of it could disagree for fifteen seconds about whether a
revision had applied.)

The envelope is **public** — `GET /health` is an unguarded probe and the push
reaches an anonymous tab — which is why the fleet and the alarm table appear on
it as counts (`nodes`, `alarms`) and never as their rows. Which node holds what
is the `fleet:operate` [`fleet`](#get-fleet) answer, and what each alarm measured is
`work_retention`'s.

```json
{
  "status": "ok",
  "node": "core-1",
  "configured": true,
  "version": "v0.4.0",
  "started_at": "2026-04-01T11:58:03Z",
  "queue": "jetstream-embedded",
  "clients": 3,
  "event_history_seconds": 2592000,
  "spend_history_seconds": 15638400,
  "in_flight": 2,
  "shutting_down": false,
  "posture": "serve",
  "applied_epoch": 41,
  "seats": ["ceo", "cto"],
  "identity_duty_seconds": {"iam_sweep": 3600, "iam_claims": 3600},
  "consistency": {"evaluated": true, "findings": 0},
  "unproven_seconds": {"eng": 312.5},
  "nodes": 3,
  "alarms": {"count": 1, "worst": "trim_blocked", "worst_domain": "tracker"},
  "seeded_from": {
    "nodes": [
      {"id": "core-1", "answered": true, "error": ""},
      {"id": "core-2", "answered": true, "error": ""}
    ],
    "complete": true
  },
  "identity": "ready"
}
```

Before anybody is in the company, the same body says so, and the remedy is an
invitation issued under a Tier A token (see [How the first person
exists](../concepts/identity-and-access.md#how-the-first-person-exists)):

```json
{
  "status": "ok",
  "node": "core-1",
  "identity": "unclaimed"
}
```

| Field | Meaning |
|-------|---------|
| `status` | `shutting_down`, `unconfigured`, a diverged posture (`shed`, `stuck` or `isolated`), or `ok`, in that order of precedence. A draining engine is draining first, whatever else is true of it, and a node with no active revision is that before it is anything else. The two ordinary postures, `serve` and `wait`, read as `ok`. |
| `node` | The name this node's engine runs under — `node.id`, else `CREWLET_NODE_ID`, else `node-0` — which is what its presence lease carries and the only way a caller can tell which node a load balancer sent it to. |
| `configured` | Whether a company revision is active. Read off the engine's live epoch on every call, so an apply that brings this node its first revision flips it. When `false` the node **refuses** every inbound webhook with `503`, so an operator watching empty screens needs to be told this rather than left to infer it. |
| `version` | The `crewlet` version this process is running. |
| `started_at` | When this node's **engine** started, which is when the node started: the API is served inside the engine's process. The fleet view reports the same instant for this node. |
| `queue` | The event queue's backend — `jetstream-embedded` (a NATS server inside this process), `jetstream` (an external NATS cluster this node dialled), or `memory`. Read off the `EventQueue` contract's own `Backend()`, never sniffed from a type name. Display only; nothing may branch on it. |
| `clients` | Dashboards currently connected to this node. |
| `event_history_seconds` | How far back the event log can be read — the hard bottom of [paging](#paging-the-event-history): once a cursor crosses it every page is empty forever, so a client that cannot name the floor draws the store's own horizon as "the org went quiet". The store's constant, not a number this API picked, so a change to the retention reaches every screen without an edit. Seconds rather than days, because the retention is a duration and a client re-deriving the unit is a second place the number can be wrong. |
| `spend_history_seconds` | How far back a **named** spend window can reach: the replicated `usage` domain's own history (181 days, [ADR-0020](../../adr/0020-a-nodes-own-day-is-a-compacted-domain.md)), which is not the event log's. A spend chart states this floor and never `event_history_seconds` — the two answer "can I still chart that month" and "can I still open that turn". |
| `in_flight` | Turns running on this node. Always present, and a `0` is a real zero: every process that serves the API runs the engine beside it. |
| `shutting_down` | `true` from the first moment of a drain, so a dashboard shows the drain while it happens: the listener keeps serving until the drain has completed. See [During a drain](#during-a-drain). |
| `posture` | The node's [config posture](../concepts/control-plane.md#posture-what-a-lagging-node-does): `serve`, `wait`, `shed`, `isolated` or `stuck`. The only place an operator can see *why* a node left rotation, since `/ready` answers a bare `503` either way. |
| `identity_duty_seconds` | Each [identity duty](../guides/retention.md#the-identity-duties) **this node** armed, mapped to the interval it runs at, in seconds, whenever it holds that duty's lease. `{}` on a node that armed none — one running no `workers` role, since every one of them is a worker singleton, or one in a maintenance mode, which publishes nothing. A node with no company arms them like any other: the identity estate runs from boot. A duty that was never armed looks from every other vantage point exactly like one quietly finding nothing to do, so this is where you read that the retention sweep and the claim report are running at all. Which node holds each lease right now is the coordination store's answer, not this node's. |
| `applied_epoch` | The activation epoch this node last applied. |
| `seats` | The handles of the seats this node holds, `[]` on a node holding none. |
| `stall_lag_seconds` | Present only when the node's watched duty is behind: how far, in seconds. It climbs towards the seat lease TTL, at which the watchdog ends the process. |
| `consistency` | The [continuous report](#the-continuous-report)'s summary — `{evaluated, findings, worst, counts, unchecked}`, from the same evaluation `/chart/check` serves whole. Read `evaluated` before the count: `false` means this node holds no chart view or has applied no settings epoch, and `findings: 0` from a node that read nothing is no clean bill. It does **not** move `status`. |
| `identity` | Whether this company has its **first person**: `ready` once anybody is enrolled, `unclaimed` while nobody is — a fresh install waiting for [its first invitation](../concepts/identity-and-access.md#how-the-first-person-exists), issued under a Tier A token — and `unknown` where this node cannot read its identity estate **or has not applied all of it** — a node that has just joined a fleet holds empty rows until its applier catches up, so "nobody" is proved against the identity log's end — which is never reported as `unclaimed`, because a dashboard told nobody is in would tell an operator to invite a first person into a company that may have started. Every node answers it, one with no company included: the identity estate runs from boot, so such a node answers for the fleet's. `/health` is the one surface an unclaimed company can reach — it is never guarded, and before the first person there is no credential to present anywhere else — which is why this is here. It does **not** move `status`. |
| `nodes` | How many nodes hold a presence lease — the fleet this node's fan-outs (search, fleet history) divide their work by. **Absent** when the presence read failed or did not finish inside the probe's coordination budget (an eighth of the 15-second reconcile interval, under two seconds) (it runs beside the posture read, so a wedged broker slows `/health` by that budget rather than hanging it), and on a node older than the field; never `0`, since the node answering is itself one. A screen says "node count unavailable" for an absence rather than guessing. |
| `alarms` | `{count, worst, worst_domain}`: how many of this node's [alarms](alarms.md) are firing, and `worst`, the one that has been firing **longest** (absent when `count` is 0) — the table asserts no severity of its own, and the condition that has gone unanswered longest is the one a health card names. `worst_domain` is the state log it fired on, for an alarm the table keeps **per log** (`log_headroom`, `log_ceiling_short`, `trim_blocked`, `deferred_old`, `floor_unknown`), and absent for one about the node as a whole: two logs' `trim_blocked` are two conditions with two remedies, and a card naming the kind alone could not say which log to look at. From the **same** evaluation the `crewlet.alarm.active` gauge and the `alarm_raised` / `alarm_cleared` log lines come from, which runs every fifteen seconds on every node. **Absent** before that evaluation first runs: nothing has looked yet, and `{count: 0}` would read as healthy. |
| `seeded_from` | Which nodes this node's live projection was seeded from at boot — the activity feed, the live spend window and each seat's last turn that every screen starts from — in the fleet [`coverage`](#reading-the-fleets-history-coverage) shape. Absent until the seed has run. A seed that missed a node started those screens a node short, and this is where that stays visible after the log line has scrolled away. |
| `unproven_seconds` | Each seat whose teardown this node could not prove, mapped to how long it has been stranded, present only when one is. Such a seat is still leased by this node, so no peer can claim it, and this node will not run it: it is absent from `seats` for exactly that reason. Alert on the duration rather than on the field's presence: a release that fails once and succeeds on the next heartbeat is a working system. See [Seat ownership](../concepts/seat-ownership.md#what-ownership-looks-like-from-outside). |

Per-socket facts, such as how many envelopes *this* connection dropped or
how deep its queue is, are deliberately **not** here. The tick encodes one
JSON string and hands the same string to every client, so a per-client field
would force one encode per client per tick. A connection that lost envelopes
to backpressure is logged as `stream_client_left_behind`, with the count, when
it disconnects.

`GET /health` always returns **200**, including when `status` is
`unconfigured` or a diverged posture: the status code is liveness, and an
engine waiting for a configuration is alive. Steer traffic with
[`GET /ready`](#routes) instead, which answers `503` and names the reason.

### Paging the event history

`GET /events` and the `events` query return rows ordered by
`(timestamp, id)` **descending**, and accept an exclusive keyset cursor:

```
GET /events?limit=100&before_time=2026-04-01T12:00:00Z&before_id=<event_id>
```

Pass the oldest row you already hold to get the page beneath it. The id
half is not optional — burst writes routinely share a timestamp at
microsecond resolution, and a cursor over a non-unique key silently
skips or repeats whatever collided with it.

**The end of the history is a page with no rows** — `exhausted: true`,
`next: null`. A page SHORTER than `limit` is not the end: the page is
merged from every node's store (see
[Reading the fleet's history](#reading-the-fleets-history-coverage)), and
it stops at the newest point any node's page stopped at, so a node whose
reply was cut to fit the transport shortens the page without ending it. The
`agent` filter adds a second reason — it also pulls in every event sharing a
trace with a direct match, from every node, so a caller must dedupe by id.

`agent` is the **related** filter: every event that involves one agent seat,
named by any **handle** it answers to — the events that are its own, and every
one naming it as a party: an A2A channel it asked or was asked on, a message it
sent or was sent there, and a vendor delivery addressed to it — plus the
trigger that caused its work, by trace. Both sides are keyed on the seat's
**agent id**, never a name: the handle is resolved to one through the org chart
the answering node holds, as each event's participants were resolved when it
was written, so a seat's namesake is never listed as the seat, and a handle the
seat has given up still finds it rather than whoever took the handle next. A
handle no agent seat answers to — a human seat's, or nobody's — is refused
`bad_params`, because there is no id it could match and an empty page would
read as an agent that has done nothing; a person's events are the ones they
acted in, under `actor`. The index was keyed on names before node migration
`0042`, and a counterpart or a delivery's recipient named before it is not in
the related view for the rest of the retention window.

The persistent store retains 30 days, and
[`event_history_seconds`](#the-health-envelope) on the health envelope is
that floor on the wire — read it rather than restating the number, which
is the store's own constant and not a promise this page makes. Once a
cursor crosses that floor every page is empty — which is why a client
must distinguish it from quiet, rather than drawing the gap as silence.

`seat` narrows the log to the events ONE agent seat published, named by any
handle it answers to and resolved on the server to the derived agent id every
seat-level event carries and every row names as its own `agent_id` — the live
`event` push and a stored row alike, so a client merging the two filters both
by the same id. It is what a seat's own "all
activity" asks by, and never `actor`, which is who acted as prose: a seat's
name there matches every seat carrying that name.

`category` is a filter for the same reason paging exists at all —
filtering a paged list client-side silently excludes, because a 100-row
page holding 2 matches reads as "only 2 exist". Its vocabulary is a closed
set of eight values, and which event type lands under which is in
[Deployment § What gets stored](../guides/deployment.md#what-gets-stored-and-under-which-category).

### Reading the fleet's history: `coverage`

Every node's event store holds what that node published and nothing else, so
the history questions — `events`, `event`, `event_series`, `trace`, `turn`,
`turns`, `phases`, a seat's `llm_history` on `agent`, the delivery counts
on `integrations`, and the live projection's boot seed — are answered by **every live node at query time**: the
node you asked reads its own store and scatters the same question to its
peers, merges the answers, and says which nodes it heard from. Each of those
answers carries one shape:

```json
"coverage": {
  "nodes": [
    {"id": "node-a", "answered": true,  "error": ""},
    {"id": "node-b", "answered": false, "error": "no answer within the 2s fleet read budget"}
  ],
  "complete": false
}
```

- `nodes` is every node asked or heard from, sorted by id; the node you
  asked is always one and always answered — a failure of its own store is an
  error, not a gap.
- `complete` is true only when the node roster could be read **and** every
  node on it answered. A node that did not is named with why: no answer
  inside the two-second fleet read budget, a build speaking another version
  of the scatter's protocol, a reply that could not be read, or its own read
  failing.
- **A question is asked in the lowest protocol version that answers it**, so
  during a rolling upgrade a node on the older build keeps answering what it
  can answer correctly — and refuses, and is named, where it cannot. An older
  build ignores a filter it does not know, so a listing or an axis narrowed
  by `channel_id`, `seat`, `suspended` or `failed` is asked in the version that
  introduced it, as are `phases` narrowed to a `seat` (an older build reads
  only the role name that question used to carry, and would answer every
  seat's), and `event_series` always is at least the version that added its
  `failed` split, a field an older node never sends: its bars would be summed
  in as though none of its events failed.
- It sits at the top of each answer — beside the record's own fields on
  `event` and on `event_series` — and is `null` on `agent` and `integrations`
  when the history could not be read at all.
- **A node that has left the fleet is not asked**, because it is not live:
  its turn-level detail left with it. The spend and turn counts it recorded
  are answered by the replicated `usage` domain instead.
- **`event` not found** answers `not_found` naming any node that did not
  answer, because a link whose node was merely silent is a different fact
  from a dead one.

`turns` merges in two passes — every node's page, then every node's share of
exactly the turns any page listed — so a turn resumed on another node after a
restart is one row folded from both halves. The window is pinned to the asker's clock
first, and the merged turn is held to it whole: a node's half of a resumed
turn can start inside the window while the turn began before it elsewhere. Its `next` is the fleet's cursor:
it can be present on an empty page, where a node's page stopped before any
turn above it could be shown. With `sort=-tokens` the page ranks each node's
top turns by their MERGED totals and carries no cursor (`before=` is refused
`bad_params`): a ranking has no position to resume from, and it can miss a
turn split across nodes whose every half fell below every node's cut.

### The runtime audit: `source=operator`

Every change a person makes through a running node leaves one event, whether
or not it went through:

| Event type | Written for |
|---|---|
| `operator_acted` | Every [operator tool](#operatormcp--your-own-assistant) call that is not a proven read, on **all three** transports — a button on the dashboard (`/operator/act`), a person's own assistant (`/operator/mcp`) and a script on the [human write surface](#the-human-write-surface) (`/work`, `/pages`), whose verbs with no tool behind them are recorded too |
| `backup_requested` | Every [`POST /backup`](#post-backup) that began copying, whether the copy finished or not |

Each carries `source: "operator"` on the envelope, so
`GET /events?source=operator` is the runtime audit on its own. Who acted is
[the one attribution](#who-a-write-is-attributed-to) every surface with a
request records: `actor` (the seat's handle for a person bound to one, the
login otherwise), `actor_kind` (`human`, `operator` or `system`) and
`operator_id` (the credential — `session:<lineage>`, `pat:<id>`, or a Tier A
token's login) — resolved once for the call, so a directory rebind landing
while it ran never makes the record name a different person. Both are filed
under `lifecycle`.

```json
{"type": "operator_acted", "source": "operator",
 "actor": "jane-founder", "actor_kind": "human",
 "operator_id": "session:0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a01",
 "transport": "act", "tool": "update_work_item",
 "request_id": "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12.k3f9a1c0d2e4b6a88",
 "outcome": "applied", "position": "CREWLET_TRACKER_LOG@1:4711"}
```

`outcome` is one of five: `applied`, `pending` and `unknown` are the write's own
answer, exactly as the call returned it — and a call interrupted before it
answered is `unknown`, because whether it landed is precisely what nobody
knows; `refused` names the tool's refusal class in `refusal`; `failed` is a
fault of the node (`internal_error`) or a failure the tool did not classify,
and carries no `refusal`. The last two also carry `failed: true`, so
the row is tagged failed and the log's failure filter finds it. `request_id`
is the operation the call was made under — the request's `Idempotency-Key` as
the caller's own operation on the act and write surfaces, absent over MCP — so
every retry of one gesture reads as the same request. A backup's record names the `dir` and the number of
`streams` the manifest covers; the node whose disk it was written to is the
envelope's `node`, which the queue stamps on every event with the node that
published it — and the backup route publishes from the node that took the copy.

**The arguments are never recorded.** A page body or a comment is the
company's content and already lives in the history of the object it changed;
the audit says who called what, and what became of it.

**A listing carries what a row needs without its payload.** `GET /events`
never returns payloads, so the store promotes the runtime audit's dimensions
into each row's `tags`: `actor_kind` and `operator_id` (who acted, beside
the row's own `actor` column), `tool` (an `operator_acted` call's tool) and
`dir` (a `backup_requested` copy's directory) — beside `node`, the envelope's
publishing node, which every event row carries. That is what the dashboard's
Audit log and backup history draw from.

**What is not here.** A request refused before any tool ran — a read sent to
the write transport, a token that is nobody, a malformed body, a backup with
no destination or with one it refused (the `400`s below) — changed nothing and
is not recorded — except where the write surface refused it on an authority
decision of its own, on a stored row it read before any tool ran (a remark
somebody else wrote, an item's own project), which is `refused` like a tool's
refusal, because "you may not" is a fact about a person an audit is read for.
A gesture with no tool behind it — a purge, a page's rename, trash and restore,
a comment take-down, a remark rewritten — is recorded as a tool call is. A
proven read over MCP is not recorded either: an assistant asks many questions, and a row per question
would bury the writes. Configuration and credentials keep their own records
(a revision names who created it, a credential who stored it), which is why
`/config` and `/secrets` publish nothing here. And like every event, the row
is written by the node the call reached, so a fleet's audit is the union of
its nodes' logs.

A record that could not be published is logged as
`operator_audit_not_published` at error on that node; the call itself has
already been answered, and its tracker or page history is unaffected.

### The event log's time axis

`GET /events/series` counts the matching rows per bucket over a window. It
takes every filter `GET /events` takes bar the cursor and `agent` (see
below), plus:

| Name | Default | Description |
|------|---------|-------------|
| `bucket` | *(required)* | `minute`, `hour` or `day`. A closed set rather than a duration, for the reason the spend series gives for its two: an axis with an arbitrary bucket width is one nobody can label. The log has `minute` and the spend series does not, because "what just happened" is the commonest question asked of a log and an hour is the whole of that answer's window. An unknown value is refused naming what is accepted, never defaulted. |
| `since` / `until` | (the retention window, to now) | RFC 3339 instants, half-open. Both edges are snapped **outward** to whole buckets, so the first and last bars are whole ones — a partial bar has a height that means something different from its neighbours' and a reader has no way to know. The answer is labelled with the window it actually covers. |

The answer is `{bucket, since, until, bars, total, failed, by_category}`.
`bars` is **every** bucket in the window including the empty ones, so a
quiet hour is a gap of full width rather than a bar the chart squeezed out.

Each bar is `{at, count, failed}`. `failed` is how many of the bar's rows
reported a failure — by the rule a turn's own `failed` mark uses: the event
said so, or its type is one (`llm_unavailable`, `budget_exhausted`,
`turn.guard_breach`, `sandbox_run_failed`) — and it is a **split** of
`count`, never an addition to it. The answer's own `failed` is the window's,
the sum of the bars'. Counted by the engine for the reason the bars are: a
screen holds a page of rows and never the window.

`by_category` is how many rows each category would give, with the
**category filter lifted** and every other one applied. That is the only
meaning a facet count can have: counted through its own filter, every value
but the selected one reads zero. A category with no rows is absent from the
map, so a caller rendering the closed set reads a missing key as the zero it
is.

It is counted over the window **that was asked for**, not the snapped one
`since` and `until` report: a chip says how many rows choosing it would
show, and the rows come from `GET /events`, which takes the caller's own
edges. So the chips need not sum to `total` — `total` describes the bars,
which are whole buckets.

Two refusals, both **400** rather than a smaller answer:

- **`related_agent`** is not accepted. That filter folds each page's trace
  siblings into it (see above), so a count over the predicate alone is a
  smaller set than the listing shows — and a bar that disagrees with its
  own rows is the one thing this axis exists not to be.
- **A window of more than 1,500 buckets** is refused naming the bucket and
  the span. Truncating would put a month's heading over a day of bars and
  coarsening would answer a different question from the one the axis is
  labelled with; the caller's fix is a coarser bucket or a shorter window.

### The live token meter

`budget` carries the fleet's **shared token counters** as the budget gate
enforces them: for the company and for every seat whose `token_budget` caps a
window, **one entry per capped calendar window** — the day, the ISO week and the
month on the company's clock — with that window's spend, its ceiling (the
company's from the active settings revision, a seat's from its runtime half on
the org chart), the gate's refusal stamp and the engine's judgement of it. It
is the only figure that can honestly be divided into a configured ceiling,
because both cover the same span. The dashboard's other token figures are spend
rollups over a window of time the reader chose; dividing one of those into a
ceiling produces a percentage that is wrong by however much was spent outside
the window.

```json
{
  "meter_id": "node-a:7f3c…", "seq": 42, "timezone": "Europe/Berlin",
  "org": {
    "windows": [
      {
        "period": "day", "window": "2026-09-23",
        "starts_at": "2026-09-22T22:00:00Z", "resets_at": "2026-09-23T22:00:00Z",
        "used": 2710450, "limit": 3000000, "state": "near"
      }
    ]
  }
}
```

A seat's meter rides on its row of the `agents` push as `budget: {windows: […]}`
in the same shape.

- `period` is `day`, `week` or `month`, and `window` is its label on the
  company's clock — the identity every node computes alike. `starts_at` and
  `resets_at` are the window's half-open span in UTC; `resets_at` is when its
  allowance comes back without a ceiling being raised. `timezone` names the
  clock the windows were cut on.
- Only **capped** windows are listed, each with its `limit`; a scope that caps
  none carries `windows: []`, and a seat that caps none carries no meter.
- `state` is the engine's own judgement, computed once beside the counter so
  no screen holds a threshold of its own: `refusing` when the gate has turned a
  charge away in the window or it has no room left for a single token — the
  condition a seat is [parked](../concepts/agent-runtime.md#the-budget-park) on
  — `near` once `used` reaches **nine tenths** of `limit`
  (`engine.BudgetNearFraction`, served as `near_fraction` on
  [`GET /budgets`](#get-budgets)), and `ok` otherwise.
- `refused_at` is when the window last turned a charge away, in UTC, and
  **absent** while it has not. That, and not `used >= limit`, is what the gate
  said: a refused charge increments nothing, so the counter stops short of the
  ceiling by the size of the round that would not fit. The stamp is kept in the
  shared counter beside the spend, so every node reports the same one, and it
  clears on the scope's next admitted charge or when the window turns over.

Every node publishes a `budget_meters` snapshot of the counters as soon as its
seat host is running and every **15 seconds** (`engine.BudgetReportInterval`)
after that, and the projection folds each one in as it arrives. Until the first
one lands, `budget` is **`null`** — nobody has read the counter — which is a
different fact from a report whose `org.windows` is `[]`, "nothing is capped". A
company with no ceiling anywhere publishes exactly that: an empty list and no
seats, without reading the counter.

- `meter_id` identifies the node incarnation whose report is held. Every node
  reads the same counter, so reports under different ids describe the same
  figures read at different moments. A report is a complete snapshot, so a
  consumer **replaces** what it holds rather than merging or taking a
  maximum: a window turning over has to be able to lower the figure, and a
  ceiling removed has to take its window's bar with it.
- `seq` is monotonic within a `meter_id`. The feed it arrives on is
  **best-effort**: an ephemeral broadcast subscription that takes no acks,
  starts at the stream's tail on every (re)connect, and lets a slow consumer
  miss frames rather than hold them. So a report at or below the held `seq`
  from the same meter is dropped, a report from another meter that was read
  **earlier** than the held one is dropped, and a gap is closed by the next
  report rather than replayed.
- `{}` means no report has arrived yet. Per-agent, `budget: null` means the
  same, or that the seat has no per-agent ceiling at all: the engine meters a
  seat only when its `token_budget` caps a window.

It is deliberately never persisted: a report is a reading of a counter that
moves every round, so a copy replayed from history would show figures the
counter has since left behind as the current ones.

Each agent's `live_call` is `null` between turns, or
`{ turn_id, phase, iteration, model, prompt, prompt_messages, response,
tool_executions, round_narration, partial_round, round_num, rounds_used, rounds,
max_rounds, round_ceiling, round_started_at, running_call, steers,
cache_read_tokens, cache_write_tokens, work_item, node, in_progress }` while an
LLM call is under
way. The fields are these:

- `rounds_used` is the round the phase is on, counted from one: the rounds that have come back, and the one in flight from the frame the engine publishes as that round's provider call is made. A finished phase record carries the same count under the same name.
- `rounds` is each round's own timing and tokens, `{round, started_at, duration_ms, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, tool_calls}`. This used to be the count, under the name the phase record uses for the list.
- `max_rounds` is the round cap currently granted, which an extension can raise mid-phase.
- `round_ceiling` is the highest value any extension may raise `max_rounds` to.
- `round_started_at` is when the latest round made its provider call. It is later than every entry in `rounds` while that call is out, and equal to the last entry's start while the round's tools run — only the first is a model call in flight.
- `running_call` is `{round, name, arguments, started_at}`, the tool call running right now. It is absent between calls, and it is never carried forward from an earlier frame. A frame that stops naming a call means the call returned.
- `steers` is every person's note the phase has read so far, `{round, note_id}` — the round whose provider call first saw it. What the note said and who sent it are on the turn's `agent_turn_steered` rows. See [steering a running turn](#steering-a-running-turn).
- `node` is the node running the call.

Beside `live_call`, each seat carries `turn`, `last_turn` and `paused`: the turn the seat is on, with its `stage` of `context`, `phase` or `parked`, the newest turn it ended, and who paused the seat, when and why (`null` while nobody has). See [Agent States](../concepts/agent-runtime.md#agent-states).  A call whose phase failed keeps `in_progress: false`
plus `failed: true` and an `error` object, so the dashboard renders the failure
instead of an answer that never arrives — and no `partial_round`, because the
phase is over and nothing is still arriving.

**What the projection carries, and what the wire sends.** The whole call is
republished to every open dashboard five times a second for the length of a
phase, so the frames behind it are trimmed and the projection reassembles them:
the `prompt` and `prompt_messages` are sent **once**, on the phase's opening
frame, and carried forward from there (a 30 KB system prompt does not change
mid-phase, and past `queue.MaxPayloadBytes` the publish is refused outright and
the live row simply stops); a tool result and the joined `response` are sent
**tail-bounded** on a live frame, because they are what a reader is watching
the end of. The durable `agent_phase_completed` record keeps every one of them
verbatim, which is what a reader opens the finished card for.

### `WS /ws/stream`

Upgrades to a WebSocket.  All frames are JSON envelopes of the form
`{"kind": "...", "data": ..., "ts": "<iso8601>"}`.

> **The socket is the dashboard's channel for state, not for everything.**
> The projection arrives here as pushes, and every question the query
> registry answers is asked and answered here too. What the socket does not
> carry is REST: every write — through
> [`/operator/act`](#operatoract--the-dashboards-write-surface) as the
> principal the session resolves to, through `/chart` and `/iam`, or through
> `/config`, `/secrets`, `/setup` and `/backup`, because a write has to be
> able to say whether it happened and a frame into a dropped socket has no
> answer — and the few guarded reads no query answers (`GET /secrets`,
> `GET /config/references`, `GET /setup/integrations` and its passes, the
> org chart's runtime half), which the dashboard reads through one loader
> that re-reads when the session changes and honours a `Retry-After`. The
> dashboard survives losing the socket by polling `/stream/snapshot` every
> five seconds — or when a `503` it answers says, and not at all after one
> with no `Retry-After` — which is exactly the kind of failure that is easy
> to miss: nothing looks broken, the page is simply always a few seconds
> stale. `internal/e2e` closes that gap by replaying the frames a real server
> produced through the dashboard's own protocol module
> (`static/dashboard/protocol.js`, the same source its bundle contains), so
> both halves of the protocol are checked against each other rather than each
> against its own idea of the other.
>
> **A kind a client does not know is ignored, never an error.** A fleet part
> way through a rolling upgrade has a node pushing kinds an older bundle was
> built before, so the dashboard drops such a frame — and counts it, because
> the same fall-through is what a kind this build's engine sends and its own
> client forgot looks like, and the e2e replay fails on a non-zero count.

**Who may open it, and what each reader receives.** The handshake needs
`state:read` — a caller without it is answered `403 unauthorized` before the
upgrade, naming the grant — and every push kind is received only by a socket
whose caller holds the grant that kind's question takes: `event` needs
`audit:read` (it is an `events` row, carrying every phase's prompt and
response), and every other company kind `state:read`. The snapshot is built for
the same audience. A re-check that finds the grants changed sends a fresh
snapshot for the new audience, and one that finds `state:read` gone closes the
socket `4403` — see [Close codes](#close-codes).

#### Pushes

Server → client kinds:

| `kind` | When | `data` |
|--------|------|--------|
| `snapshot` | First envelope after the upgrade succeeds, and again on reconnect. | Same payload as `GET /stream/snapshot` — agents carry their in-flight `live_call`, so a reconnect re-renders the live row. |
| `event`    | Every engine event published to `crewlet.events.>`. | `{ id, type, timestamp, source, actor, summary, category, trace_id, span_id, parent_span_id, topic, agent_id?, channel_id?, payload }` — `agent_id` the seat the event concerns and `channel_id` the agent-to-agent channel it belongs to, each read by the rule that fills the store's own column, so the event log narrows its live rows to a seat or a channel exactly as the store narrows its pages — the same shape as a `/events` row, plus the full event `payload` (from which the snapshot feed's `failed` flag is derived).  `agent_phase_completed` events carry the system prompt, response, and tool calls, so LLM invocations stream live; `agent_turn_progress` events (per tool-call round, tagged with `turn_id` / `phase` / `iteration`) stream the in-flight call before its phase record exists. |
| `agents`   | After an event moved one or more agents — or a read moved their state: a run record reconcile, or the seat-lease read the five-second tick makes. | The changed agents' overlays, each with its `agent_id`, its `role`, its `activity` and its `stopped_reason` — the *result* of applying the change, so a client merges them rather than running its own state machine over the raw stream. A client merges an overlay onto the roster row carrying the same `agent_id` and DROPS one for a seat its roster does not carry: a seat reaches the roster through the snapshot or a `seats` push, each of which carries its live overlay already merged. |
| `seats`    | After anything changed the company — a config activation, or a write to the org chart's own log such as a hire, a move or a rename. | The COMPLETE seat list, replacing what the client holds, each row carrying its `agent_id`, its current `handle` and its `role` (name). Distinct from `agents` on purpose: that one is a per-seat merge, and a merge cannot express the deletion of a seat that has gone. |
| `sandboxes`| After a detached sandbox run started, asked a question, finished or was lost, and after a reconcile against the durable run record changed the set. | The full in-flight sandbox list. |
| `tokens`   | On the shared 5-second tick, when a phase completed since the last one. The fold runs on the tick rather than on the publish, so a busy company costs one aggregation every five seconds rather than one per phase. | The spend rollup, same shape as `GET /tokens/breakdown`. |
| `budget`   | After a node's token meter report is applied (every node reports at start and every 15 seconds, a company that caps nothing included). | `{ meter_id, seq, timezone, org: { windows: [...] } }`, the org-wide half: one entry per capped calendar window, each with its span, spend, ceiling, refusal stamp and `state`. Per-seat figures ride on each agent's overlay in the `agents` push. See [the live token meter](#the-live-token-meter). |
| `org` / `tools` / `schedules` | On the same edge as `seats`: every published company, whether an activation or a chart write. | The new org tree / tool surface / schedule list, so open tabs stop showing seats that no longer exist. |
| `health`   | Pulsed every 5s by a **single shared tick** (one timer for all clients, not one per connection). | The whole [health envelope](#the-health-envelope), exactly what `GET /health` answers. There is no query for it. |
| `result`   | Reply to a client `query` that succeeded. | `{ id, what, data }` — `id` echoes the request's. |
| `error`    | Reply to a client `query` that could not be answered, or a `watch` that was not installed. | `{ id, what, error, reason?, grants?, retry_after?, refusal?, detail? }` where `error` is a code — and an `unauthorized` one adds `reason` and `grants`, exactly as the REST envelope does: the rule that decided, and the grants any one of which would have admitted the caller (an EMPTY list means no grant would, because what is missing is a relation such as leading that person). A **watch refusal** has no `id` and names `what: "watch"`: `unauthorized` when the caller may not watch that seat — with its `reason` and `grants` — `unavailable` when this node could not read the chart that decides it or the directory a login resolves through — carrying `retry_after` as a query's `unavailable` does, by the same rule and with the same `0` for a read no wait clears, but never the `refusal` or `detail` behind it, because what the directory says about a login names the seat it is bound to — and `not_found` when a login names no record, which only `fleet:operate` is ever told. For a query, the codes are: `unknown_query`, `unauthorized`, `not_found`, `bad_params`, `unavailable`, or `query_failed` for every other failure (the reason goes to the log, never to the socket). **`unknown_query` covers a surface this process does not have**: a question whose source is not wired here is never registered, so it is unknown rather than empty and never carries a `Retry-After`, because waiting cannot give this node a store it was not configured with. Its REST twin is `404`. **`unavailable` is not `query_failed`**: it says this node understood the question and cannot answer it *here* — a projection still catching up after a restart or a fresh join, a coordination store it could not reach, or a refusal by its state log — so a client does not report a fault. A query's `unavailable` frame also carries `retry_after`, the whole seconds before asking this node again, and, where a state-log refusal is behind it, `refusal` (its code — `behind`, `log_full`, `deferred`, `broker_refused`, …) and `detail` (its own words, the remedy). **A `retry_after` of `0` is the answer, never an omission**: waiting will not change it — a log at its byte ceiling, a record this node cannot decode, a barrier its broker refused — so the client asks another node or waits for an operator rather than polling this one; see [Read consistency](../guides/consistency.md#the-thirteen-refusals). Its REST twin is `503` with the same `refusal` and `detail` in the body and `Retry-After` carrying the hint, absent where the hint is zero. **A work, page or search question on a node that has not been handed a company yet** is `unavailable` with `refusal: "no_active_revision"`, the tracker's and the knowledge base's own sentence as its `detail`, and `retry_after: 15` — the reconcile poll that brings the company — and its REST twin is `503 no_active_revision` with the same detail and `Retry-After: 15`, as every other surface over those two halves answers such a node. **`bad_params` is not `query_failed` either**, in the opposite direction: the node understood the question and *refused* it — a parameter missing, malformed, or outside the set the field accepts — so the fault is the caller's and retrying sends the same bad request again. Its REST twin is `400`, and it carries **`detail`**: the refusal's own sentence, which names the parameter to change and what it accepts (`days is 91, and a spend window is 1 to 90 company days — ask for at most 90`), written for the person who will read it: no class name, the engine's or a finer one (`tokens.ErrWindowLength`, which a Go caller tests with `errors.Is`), and no echo of the query's name — the REST `400` body carries the same `detail` beside its `error`. A frame has ONE `detail` key, `unavailable`'s words and `bad_params`'s sentence alike. Every other code's text stays in the node's log, since a failure's own text can carry a path. The dashboard asks again after exactly a frame's `retry_after` — and not at all after a `0` — and keeps no wait of its own, so this hint and the `Retry-After` on a REST refusal are the only retry clock it has. |
| `pong`     | Reply to a client `ping`. | `null` |
| `identity` | This socket's own credential, at its last re-check. **Direct** — a fact about one tab, not the company, which is why it never rides the node's `health`. | `{ state: "unverifiable" \| "verified", retry_after? }` |
| `inbox_changed` | A committed tracker batch wrote the watched seat one or more notices. Every node applies every tracker record, so each node pushes to its OWN sockets from its own applier after the batch commits; nothing is forwarded between nodes. One frame per seat per batch. | `{ handle, unread_delta, subject, reason }`, and the frame itself carries `seat`. **Identifiers and a count, never content**: `subject` is the id of the object the newest of those notices was written on — the task for a task commit, and the PERSON for a `prioritised` notice, whose task `work_inbox` names as `task` — and `reason` the reason it was routed under, and what the notices say is read through `work_inbox`, the question that decides who may read them. `unread_delta` is a **hint** — the notices the batch wrote, which a redelivery the engine has already collapsed may count twice — so a client re-asks rather than adding it to a badge. **Routed by seat**: it reaches only the clients whose `watch` for that seat was allowed, never every tab. It needs `state:read`, the grant `work_inbox` takes. |

#### Client frames

Client → server kinds:

| `kind` | Purpose |
|--------|---------|
| `ping` | Keepalive; server replies with `pong`. |
| `watch` | Become a recipient for one seat's seat-routed frames: `{ kind: "watch", seat }`. An empty `seat` clears it (always allowed), and one socket watches one seat at a time — a tab is looking at one screen, so a second watch replaces the first. **`seat` names a record the way `work_inbox`'s `handle` does**: a seat's handle, either of your own names, or somebody's **login**, which is resolved to the record their notices are kept under — the seat the identity directory binds them to — and the watch is installed there, because that is the name every `inbox_changed` frame for them is pushed to; see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for), whose rules it follows to the letter, the directory asked only once the watch is decided as far as it can be without it. A login that names nobody answers `not_found` to `fleet:operate` and is refused like any other seat to everybody else. **It is decided like `work_inbox` about the same seat**, because it buys that seat's `inbox_changed` frames: the seat's own holder, whoever leads it through the chart, or the admin grant. Three answers, and none closes the socket. **Allowed** installs the watch. **Refused** installs nothing, drops any watch the socket held, and replies `error` with `unauthorized`. It is not a `4403` close, because a client reads that as "this browser may not have the live channel at all" when all that was refused is one seat's frames. **Undecidable** (this node could not read the chart, or the directory a login resolves through) installs nothing either and replies `unavailable`, with the `retry_after` that says when asking again can change the answer — `0` where no wait will. Installing would hand the seat's frames to somebody this node could not show was allowed them. Refusing would tell a lead they lead nobody because the node is behind. **Every credential re-check re-decides the current watch**: a refusal withdraws it and says so, and an undecidable answer keeps it, since nothing has been learned against a decision this node already made. A watch with nobody resolved behind it closes the socket with **4401**. |
| `query` | Request one thing, answered with exactly one `result` or `error` frame. `{ kind, id, what, params }` — `id` is any client-chosen value echoed back on the reply. **There is no per-frame credential**: every question is asked as the principal the handshake resolved (and each re-check since), and decided by the grant it is registered under. Queries run concurrently with each other and with the push stream, so one database read cannot stall a tab's live rows — at most **four** at a time per **socket**, which is a tab's share of the node's reader pool: the pool's floor is sized as two full dashboards at four each, and a fifth query waits on its own socket rather than in the pool the engine's own reads share. |

#### Queries

Each query (`what`) is answered by the *same* function the matching
REST route calls, so the two surfaces cannot diverge:

| `what` | `params` | Answers with |
|--------|----------|--------------|
| `agent` | `{id}` | `GET /agents/{id}` — config + live state + `llm_history` |
| `agent_memory` | `{id, limit}` | `GET /agents/{id}/memory`. A seat's trail, so it takes `audit:read` whoever's seat it is. ANSWERED BY THE NODE HOLDING THE SEAT, which it names (`held_by`, or `none` with an empty answer for a seat no node holds; `unavailable` while the holder is silent, still taking the seat, or on a build that cannot answer) — every node keeps a copy of a seat's memory and only the holder keeps it current. Four collections, each a page (`limit`, at most 50) with its counted total beside it: the diary (`diary_total`), the episodes (`episodes_total`), the synthesized skills (`skills_total`) and the COUNTERPARTY PROFILES (`counterparties_total`) — what this seat has learned about the colleagues it works with, both instants carried because `last_updated_at` moves on every interaction and `last_corroborated_at` only when the traits changed. `traits` is a bag whose keys the model invents, never a fixed schema. Plus `latest_reflection` (the newest live diary entry, whatever the page) and `onboarded_at`. The diary is keyed on the derived agent id and the other three on the handle the seat was CREATED under; both are resolved from the one identifier a caller has — any handle the seat answers to — so a renamed seat's page shows what it learned before the rename, and every row names the seat, and a colleague in a profile, by the handle they answer to NOW. See [the route](#get-agentsidmemory) |
| `memory_overview` | `{}` | **Takes `audit:read`**, as `agent_memory` does. EVERY AGENT SEAT'S memory totals — `diary_total`, `episodes_total`, `skills_total`, `last_reflection_at` and the `latest_reflection` itself — each counted by the node holding the seat, gathered in ONE scatter rather than a read per seat, with `held_by` per row (`none` for a seat no node holds, nothing counted), an `unavailable` reason on a row whose holder did not answer, and the fleet `coverage`. Every agent in the chart, handle order, no cap. See [the section](#memory_overview) |
| `conversations` | `{handle, conversation, limit}` | `GET /agents/{id}/conversations`. The seat's own thread ledger — the engine's only account of what a seat said on a surface it does not own, and what stops it replying twice in one thread. TWO SHAPES IN ONE ANSWER, because a screen asks two questions with one navigation: `conversations` is every thread this seat holds entries in, and naming one in `conversation` adds that thread's turns as `entries`. Each turn's `reply` and `unsent` carry the same artifact and WHICH ONE HOLDS IT is the whole record of whether anybody received it — a turn can end with real work done and no way to say so. The ledger is keyed on the handle the seat was CREATED under, so `handle` may be any handle the seat answers to and a renamed seat's threads include the ones it carried before the rename; the answer's `handle` is the one it answers to now. An absent `handle` is the caller's own SEAT — a seat's trail is the seat's, so a caller bound to none names one — and a named one is a seat's TRAIL rather than a person's queue, so it takes `audit:read` whoever's seat it is. The listing is a page (default 50, at most 200) with `conversations_total` beside it, ANSWERED BY THE SEAT'S HOLDER, as `agent_memory` is and for its reason: the ledger travels with a seat's memory and only the holder's copy is current (`held_by`) — see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for) |
| `event` | `{id}` | `GET /events/{id}` — one event with its full payload |
| `events` | `{limit, type, source, category, trace_id, channel_id, seat, actor, agent, turn_id, work_key, work_item, suspended, failed, since, until, before_id, before_time}` | `GET /events`. `failed` is THREE-VALUED the same way — `true`, `false` or absent, any other word a **400** — and selects by the rule every row's own `failed` is stamped by: a stored `failed` tag, or a type that is itself a failure (`llm_unavailable`, `budget_exhausted`, `turn.guard_breach`, `sandbox_run_failed`). It is the event log's "Failures only", applied by the engine so the axis and every page are one set. `suspended` is THREE-VALUED — `true`, `false` or absent for every row, any other word a **400** — and selects by whether a completion record PARKED its turn on a detached coding run: a turn that parks and resumes writes two `agent_turn_completed` records, the first marked `suspended`, so `type=agent_turn_completed&suspended=false` is the turns that ENDED, one record each, and is what a turns axis counts over. `trace_id` selects one trace as a FILTER — paged, windowed and combinable with every other filter, where [`GET /events/trace/{trace_id}`](#routes) is the whole trace oldest first. `channel_id` selects one agent-to-agent conversation's events, by the channel id every A2A event carries (an index seek, migration `0034`). `seat` is a seat's **handle** and selects the events that seat published, resolved on the server to the id every node derives for it — so a seat since removed from the chart still names its history, and a role name, which a rename changes, is never the key; anything that is not a handle, or a person's seat's handle, is a **400** — an empty page would read as a seat that never did anything — and before a company configuration is applied it is **503** `unavailable`, since the id is derived from the company's name. `agent` is the broader question — the **related** filter: every event involving one agent seat, named by any handle it answers to and matched on its agent id, plus every event sharing a trace with one (see [Paging the event history](#paging-the-event-history)). `turn_id` selects ONE RUN of a turn; `work_key` selects every run of one unit of work — the attempts at a trigger that was redelivered; `work_item` selects every event on one work item — each turn's start, its phases and completions, a coding run it launched — named by the item's identity across trackers, `<backend>:<id>` (`native:<task id>`, `jira:<issue id>`), never by its key, which a move rewrites. A value that is not that shape (a key such as `ENG-4`, or either half missing) is a **400** rather than an empty page, because an empty answer reads as "nothing happened on this item". The filter reads the `work_item` COLUMN (migration `0033`), whose backfill gives the rows already stored their item from the payload; their stored `tags` are not rewritten, so the column, not `tags.work_item`, is what answers for history. Rows written before migration `0029` carry the work key in `turn_id`, and that migration backfills it into the COLUMN, so history answers both. Every row answers with its own `work_key` read off that column rather than out of its `tags`, which is the one promoted value that is not a copy of a tag: the backfill deliberately does not rewrite a stored tags blob, since those record what the writer extracted from an event whose JSON carried no such field |
| `event_series` | `{bucket, since, until, type, source, category, trace_id, channel_id, seat, actor, agent, turn_id, work_key, work_item, suspended, failed}` | `GET /events/series`. Every bar carries `failed` beside `count` — how many of its rows reported a failure — and the answer a `failed` total beside `total` (see [the event log's time axis](#the-event-logs-time-axis)). THE SAME ROWS WITH A TIME AXIS, which a page of rows has no dimension for: a burst at four in the morning and a steady trickle across a week are the same hundred rows in the same column. A second question rather than a flag on the first, because the two answers have different shapes and one route returning either would make every caller branch on what came back — the same split `tokens` and `token_series` carry. Both halves compile their filters through ONE predicate in the store, so a bar can never claim rows the listing beside it would not show |
| `trace` | `{trace_id}` | `GET /events/trace/{trace_id}`. Answers `{trace_id, events, truncated}`; `truncated` is true when the read stopped at the store's per-trace cap (500) rather than at the end of the trace, which the caller must say — a trace shown short with no note reads as a complete causal chain that simply ends. It is **counted, not inferred** from the row count: a trace of exactly the cap holds every row it has, and `len(rows) == cap` would put a truncation warning on a complete one |
| `turns` | `{days, since, until, seat, model, work_key, work_item, failed, sort, before, limit}` | `GET /turns`. The window is on the turn's START: `days` back from now (default 7, at most 30), OR `since` (inclusive) and `until` (exclusive) as RFC 3339 instants — either alone is a one-sided window — which is what a bar in the past needs, since `days` counts back from now; naming both forms, or a `since` not before `until`, is **400**. A turn is in the window when it STARTED there: one that began before `since` and ran on into the window is not listed, rather than listed from the window's edge with half its tokens. `seat` is a seat's **handle** and narrows to that seat's turns, resolved on the server exactly as on `events` (and refused the same way); there is no role-name filter, because a role name is changed by a rename while the history keeps the old one. ONE ROW PER RUN of a turn — a wake, a decision, its rounds and its reply — which is the view of a working company that did not exist anywhere. A turn that broke before reaching outside the engine is redelivered, so one TRIGGER is legitimately several rows; each carries the `work_key` they share and `work_key=` narrows to every attempt at one (see [a turn's two identities](../concepts/turn-engine.md#a-turns-two-identities)). A turn is what this engine DOES and every other surface is a projection of one: the spend rollup groups them, the seat page shows one seat's, an item's history links to the ones that touched it, and none of them is a list of them. The dashboard faked one by paging the raw event feed sixty-one times and folding in the browser — slow, capped at whatever the caller gave up on, and wrong at the page boundary, where a turn straddling two pages appeared twice. The aggregates are over PROMOTED COLUMNS (migration 0015) rather than payloads; only the duration, the summary and the work item come from the completion record's own payload, read from the one row per turn that carries it. `work_item` is `{backend, id, key, project}` — the one item the turn was charged to (see [which work a turn is on](../concepts/turn-engine.md#which-work-a-turn-is-on)) — and ABSENT for a turn on nothing, including one still running, since a sole write names its item only at the end. `work_item=` narrows to the turns on one item, by the same `<backend>:<id>` identity `/events` takes (and the same **400** for anything else); it selects TURNS rather than rows, so a turn is listed whole — every phase and every segment folded — when any of its records names the item. There is no `task_id` on a row: the key it read was declared on the completion and never set, and `task_id` elsewhere means a delegated worker's task or a schedule fire's run, never a tracker item. A turn that launched a detached coding run PARKS: the segment that launched it publishes a completion with `suspended: true`, and the same turn completes again when the run is collected — so one turn can hold several completion records and "a completion exists" is not "the turn ended". The NEWEST completion decides: `complete` is true when it is not a suspension, and `parked` when it is, so the two are never both true; a turn with neither is running or died mid-flight. A `sandbox_run_failed` also counts as an end. It is a coding run that was LOST, and nothing resumes the turn that was parked on it, so a list reading completions alone called that turn parked for good. The newest end still decides: a run lost while it was still launching is followed by its turn's own completion. A completion from a build that predates the flag names no `suspended` and reads as an end. `duration_ms` is the turn's OWN measurement — the SUM of every segment's, so the wait for the coding run between them is not counted — which is not the span of its events either: the span covers the reflection pass that publishes afterwards. `cache_read_tokens` and `cache_write_tokens` are the turn's phases' prompt-cache counts (migration `0032`), a breakdown of `input_tokens` rather than an addition to `total_tokens`. `failed` is THREE-VALUED and absent means every turn, because folding it into `false` would hide every failing turn from an unparameterised list — and a turn counts as failed when ANY of its events carried a failure OR was a failure BY ITS TYPE (`llm_unavailable`, `budget_exhausted`, `turn.guard_breach`, `sandbox_run_failed`), which is the same rule `/events` applies to a row. The second half is what a turn the engine killed BETWEEN phases leaves behind — a refused charge, an exhausted chain, a breached guard — so reading the failure flag alone reported those as clean turns with no completion record, which is indistinguishable from a turn still running. The cursor is on the turn's START, which is what the listing is ordered by; a keyset on any one event pages a turn twice. `next` is present only while more turns lie past the page — `null` on the last one, which is how a reader paging a seat's record knows the walk has ended rather than offering "older" onto an empty page. `sort` is `-started` (the default, newest first) or `-tokens` (the most total tokens first, the spend screen's drill-down per turn) — anything else is **400** naming both. Read from EVERY node and merged, with a `coverage` — see [Reading the fleet's history](#reading-the-fleets-history-coverage) |
| `turn` | `{turn_id}` | Every event of ONE RUN of a turn, oldest first, payloads included — each phase, the turn's own completion, and the fallbacks and guard breaches that happened inside it. Not a slice of the trace: one trace can span several turns and one turn several traces. Rows written before migration `0014` carry no `turn_id` and do not answer this. Answers `{turn_id, events, truncated}`; `truncated` is true when the read stopped at the store's per-turn cap (500) rather than at the end of the turn. A cut answer is the turn's **opening and its ending**, not its opening alone: a turn is read oldest first, so a head-only read would drop `agent_turn_completed` and `turn_completed` — the two records a reader takes the outcome, the duration and the plan summary from — and a turn cut at the cap would be indistinguishable from one that never finished. The last rows are recovered beside the first (up to 20 more, merged on the store's own identity, `(event_time, event_id)`, so the two reads cannot overlap into duplicates — the id alone is not unique, and a narrower key would drop a row the two reads legitimately both carry and then report a gap over a page holding the whole turn), so what `truncated` names is a gap in the **middle** — and it is **counted, not inferred** from the row count, because a turn between the cap and the cap plus twenty ends up whole on the page and must not carry a truncation warning. It also answers `work_key` and `attempts`: the unit of work this run was an attempt at, and every run of it the store holds, OLDEST FIRST — over the SAME thirty-day horizon the events above come from, not the turns list's own default week, so a turn between eight and thirty days old names its attempts rather than reporting none while displaying one — so the screen a deep link lands on can say "attempt 2 of 2" and link the other, rather than leaving a reader to conclude the company did the work twice. One element is the ordinary case; an empty `work_key` means the trigger had none to collapse on, and `attempts` is then empty too. And `nodes`: the nodes whose own store holds any of this turn's events — where it RAN, since each node's store holds only what that node published — sorted, and an empty list rather than an absent key where no node this read could reach holds any |
| `phases` | `{seat, limit, before_time, before_id}` | The company's `agent_phase_completed` records, newest first, **payloads included**, keyset-paged. `seat` is a seat's **handle** and narrows to that one seat's phases, resolved on the server exactly as on `events` (and refused the same way) — never a role name, which two unit seats stamped from one template share, so a role filter answered one seat's phases with every such seat's. `events?type=agent_phase_completed` is not a substitute: the event listing deliberately never selects the payload, and a phase record without one has no prompts, no response, no tool calls and no decision. `next` is present only while more records lie past the page — each node's read asks one row past it rather than guessing from a page that filled, so a history exactly a page long ends without offering "older" onto nothing |
| `tokens` | `{days, since, until, previous, seat}` | `GET /tokens/breakdown`. With no parameters, the live 24-hour window from the projection; with any, whole company days from the replicated usage domain — every node's, up to 90 days, within the 181-day horizon (see [Token Spend Breakdown](#token-spend-breakdown)) |
| `token_series` | `{days, since, until, previous, seat, group, bucket, groups}` | `GET /tokens/series`. THE SAME SPEND WITH A TIME AXIS, which the breakdown has no dimension for: every one of its rows is a sum over the whole window, so a runaway loop, a spike and a quiet weekend are the same number. A second question rather than a flag on the first, because the two answers have different shapes and one route returning either would make every caller branch on what came back. Bucketed by the ENGINE, by company day or ISO week from the usage domain. An unknown `group` or `bucket` is refused naming what is accepted, never defaulted: a chart legended by one dimension over another's bands is worse than an error |
| `seat_activity` | `{seat?, days?, previous?}` | `GET /agents/activity`. Every seat's TURNS over the `days` (1 to 90, default 7) company days ending today, summed across every node from the replicated usage domain — so the answer is the same on whichever node is asked and still counts a node that has left. `{since, until, days, previous_since?, previous_until?, seats, quantile_resolution}`; each seat is `{handle, role, agent_id, in_chart, turns, failed, reviewed, first_pass, first_pass_pct?, sent_back, p50_ms?, p90_ms?, tokens, per_day, last_turn_at?, previous?}`. `first_pass_pct` is `first_pass` over REVIEWED turns (0–100) and is ABSENT when none was reviewed — a 0% for a seat nobody reviewed would be a verdict nobody gave. `sent_back` counts reviews that sent work back. `p50_ms` and `p90_ms` are read from the merged turn-duration histogram and are within `quantile_resolution` (0.06) of the true value; absent when no turn ended. `per_day` is every day of the window, oldest first, a quiet day included as zeros. `previous` (with `previous=true`) is the seat's `{turns, failed, reviewed, first_pass, sent_back, tokens, per_day}` over the same number of days before, `per_day` being every one of those days, oldest first — so a profile draws the fortnight its week-on-week figure is made over from this one answer. Every AGENT seat of the current chart has a row, a quiet one with zeros ("took no turns" is a measurement); a seat that has left the chart appears with `in_chart: false` while its days are in the window; a human seat has none. `seat=` narrows to one handle, and a handle with no rows answers one row of zeros rather than none; a human seat's handle is refused (`bad_params`, naming the person), because the engine runs no turns for a person and a zero row would say one took none. Ordered by handle |
| `schedule_runs` | `{scope_type, scope_id, name, limit}` | `GET /schedules/{scope_type}/{scope_id}/{name}/runs`. ONE schedule's dispatch history, newest first, fifty to a page. `scope_id` is the scope's IDENTITY (a seat's agent id, a unit's origin key), which is what a `schedules` row carries and what the ledger keys on — hand back what that row gave you rather than the handle beside it. `schedules.recent_runs` is the COMPANY's fifty most recent fires across every schedule, so twenty hourly ones fill it in two and a half hours — "did the standup fire this week" was unanswerable while every row of the answer sat in the table. The identity is all THREE parts and each is required: two units may each declare a `standup`, and a role and a unit may both, so a name alone merges two teams' histories. `truncated` says the page filled, because a full page is otherwise indistinguishable from a schedule that has fired exactly that many times |
| `schedules` | `{}` | `GET /schedules` |
| `credential_pool` | `{}` | `GET /credential-pool`: every model's keys and their cooldowns, the Settings › Models & keys screen. **Takes `config:read`**. See [below](#get-credential-pool) |
| `backups` | `{}` | `GET /backups`: each owner's newest backup and the backup history, the Settings › Backups & retention screen. **Takes `fleet:operate`**. See [below](#get-backups) |
| `mcp_servers_status` | `{}` | `GET /mcp-servers`: each MCP server's condition and its per-node counts, the Settings › Tools & MCP screen's Servers section. **Takes `config:read`**. See [below](#get-mcp-servers) |
| `fleet` | `{}` | `GET /fleet`: leases move with no event to push, so Settings › Nodes polls this rather than waiting for one. **Takes `fleet:operate`**. A lease table that could not be read answers `unavailable`, which is a blip to ask again about rather than a fault (the REST twin answers `503` with a `Retry-After`) |
| `sandbox_runs` | `{audience?}` | `GET /sandbox-runs`: `unknown_query` on a company with no sandbox configured, and `unavailable` when the fleet's run record could not be read. Each run carries `work_item` (`{backend, id, key, project}`, or null), the item the launching turn was charged to, and `launch_id`, the job the row holds now — what `sandbox_tail` is asked by (empty on a row an older build wrote) |
| `sandbox_tail` | `{turn_id, launch_id}` | `GET /sandbox-runs/{turn_id}/tail?launch_id=…`. What ONE running coding job has said so far, read from its box by the node that owns the run (see [Watching a run live](../concepts/code-sandbox.md#watching-a-run-live)). Both ids are required (`bad_params` otherwise): a turn can launch more than one job, and the launch id is the one `sandbox_run_started` and the run's phase record carry. Answers `{outcome, turn_id, launch_id, node?, status?, output?}`: `outcome` is `tail` with `output: {text, source: transcript\|stderr\|none, cut, as_of, finished}` (the last 8 KiB, redacted), `not_running` with the record's `status` (`awaiting_clarification`, `launching`, `resumed`, `reseed`, `replaced` for a job a later launch replaced, or absent where no record is left), `owner_silent` naming the owning `node` that did not answer inside the 2 s fleet read budget (no `node` for a run nobody holds right now), or `owner_upgrading` naming an owner whose build does not advertise the `sandbox_tail` feature. A record that could not be read, or a box the owner could not read, is an error carrying the reason. There is no event and no row: the dashboard asks it every 3 s while a running job's span is open, and nothing else asks |
| `budgets` | `{}` | `GET /budgets` |
| `a2a_channels` | `{}` | The fleet's agent-to-agent authorization record: who asked whom, how many messages crossed, and when. `available: false` when this node could not reach the coordination store — which is not the same as no channels having been opened |
| `knowledge` | `{q, mode?}` | The company's own knowledge search, run live through the same `knowledge.Searcher` seam a seat's own `search_knowledge` tool uses. Searched as the ORG with no seat, so it applies the engine's own account and nothing more — searching as a named seat would let a dashboard reader read, through that seat's credential, material their own account may not have. Registered whenever a company is active, NOT only when a searcher exists — "this company has no knowledge backend" is a fact the company establishes on its own, and it is a far more useful answer than an unknown query. `available: false` covers all four of no company, no backend, a backend wired with no org-wide read scope, and a node whose index is still `building`. `reason` (`no_company` / `no_backend` / `no_scope` / `building`, empty when the search ran) is the value to branch on and `note` is the prose for a person — a screen picking which remedy to offer must not string-match the note, nor infer the state from an empty `backend`, which means "no backend" and "no company" alike. The `no_scope` note names `knowledge.scope`, because an operator whose integration is correct must not be sent to re-check it. A search that RAN and fell short says so in its outcome rather than its `note` (which is empty whenever `available` is true): `served_mode` empty with `coverage.complete` false is a backend that did not answer, and a partial `coverage` is part of the corpus unsearched. An EMPTY `q` is the seam's PROBE: nothing runs and nothing is read, no hits are returned, but `modes` and — for the `mode` asked — the `degraded` its CONFIGURATION decides are answered, so a screen offers the modes honestly before anybody types (the dashboard asks the probe for `semantic`, the mode whose reason names what is missing). A transient reason (`embedding_failed`) is a property of a search and is never predicted. `mode` is `hybrid` (the default), `keyword` or `semantic` — the screen's label for the last is "Meaning", and any other value is refused rather than run as the default. Every answer carries what the search actually did: `mode` (resolved), `served_mode` (the ranking the hits came from; `""` when nothing ran), `modes` (what this backend can serve as asked — `[keyword]` with no embeddings provider and on Confluence), `degraded` (`no_embeddings` / `embedding_failed` / `semantic_partial` / `unsupported`, empty when it served what was asked) and `coverage{nodes:[{id,answered,error}], complete, buckets_missing}` — the fleet the scan was divided across, which is how a partial answer is told from a short corpus. A hybrid search with nothing to rank by meaning serves its keyword half; a semantic one serves nothing and says why. See [Search](../guides/search.md#three-modes-and-what-an-answer-says-it-served) |
| `colleague` | `{q}` | A NAME TO A SEAT, through the same four tiers an agent's `lookup_colleague` and `a2a_ask` resolve through (`internal/agent/colleague`): exact handle, chat id and role name, then case and separators folded, then part of a name, then a close spelling — each tier answering only when every tier above it found nothing, so a name that is exactly somebody's handle is never diluted by near misses. Answers `{match, candidates[{handle, why}]}`: `match` is the one seat the text names when EXACTLY ONE does and `null` otherwise — including when several do, which is a list for a person to choose from and never a pick — and `candidates` is every seat it could name, best tier first (the match included; empty when nothing matched, which reads differently from ambiguity). `why` is the tier in a person's words ("handle matches exactly", "part of the name matches"). The chat-id tier needs a credential: a seat's contact identities are operator-gated configuration, so an anonymous caller resolves over handles and names alone. `q` is at most 200 bytes — a name, not the sentence around it. The command palette's assign and ask pickers read it. Registered whenever a company is active |
| `integrations` | `{}` | `GET /integrations` |
| `work_items` | `{container, status, status_group, assignee, reporter, watcher, collaborator, tag, type, priority, parent, root, q, key, removed, blocked, blocking, has_dependencies, has_open_asks, flag, asked_of, asked_by, subtasks, show_closed, closed_since, f.<slug>, view, preset, group_by, group_by2, group, subgroup, group_limit, totals, sort, fields, around, cursor, limit, …}` | `GET /work`. `container` is the scope — `workspace`, or `project:ENG` (a bare `ENG` works too, and the key is upper-cased because the column is); any other `<kind>:` — `unit:eng`, `person:ana` — is REFUSED, because a task lives in the workspace or a project and a team's or a person's work is `unit=` or `assignee=` (read as a project key, `unit:eng` answered an empty board) — and an ABSENT container is neither: the engine refuses to default it, because an omitted key would otherwise be the most expensive query in the system. Every list key is comma-separated, because a socket frame's JSON object cannot carry a repeated key and a filter only one transport can express is exactly the divergence this channel exists to prevent; `status` also takes `!` negation. There is no `open` flag — open and closed are STATUS GROUPS (`not_started`, `active`, `done`, `closed`), which is the level every rule in the tracker is written at. `f.<slug>=<value>` filters on a custom field — resolved against the company's catalogue by slug, id or label, and compared on the column its DECLARED TYPE says, so `f.effort=gt:9` is a numeric comparison and not a lexical one; the seventeen operators are `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `contains`, `startswith`, `in`, `range`, `any`, `all`, `not_any`, `not_all`, `me`, `null` and `not_null` — and which of them a field admits is a property of its TYPE, so `eq` on a `labels` field is REFUSED naming `any`, `all`, `not_any` and `not_all` rather than compiling to a clause that matches nothing and reads as "no task has this label". `null` and `not_null` are on every type, because "is this set" is a question about the ROW. A bare value is the type's NATURAL comparison — `any` on a set, because naming a value is not claiming the set IS it, and `eq` everywhere else. A set operator takes a comma-separated list (`any:api,ui`, at most 16) and `range` takes both ends (`range:3..8`), because a range with one end is `gte` or `lte`. A value whose text begins `<scheme>://` is a VALUE rather than an operator call, so a `url` field can be filtered by what it holds — anything else before a colon is carried through as an operator, so a typo is refused naming the set rather than silently answered. `f.<slug>=me` is resolved to the reader by the SURFACE before the query is parsed, which is what makes one saved view mean whoever opens it. A ref nothing resolves is REFUSED naming it. `unit=` and `routing_unit=` name a team by its key — its current one, the one it was created under, or one it used to answer to — or by its NAME where exactly one unit carries it, in any case, and each resolves through the chart and matches the work filed under the team's key or its name, whichever the reference was spelled as. A team the chart does not have matches nothing rather than refusing, because a task's filed unit is a record of what was true and may name a team since dissolved. `q=` is a FIND rather than a search — a substring of a key (from the front) or a title (anywhere), which is what finds the item somebody half remembers; there is no `mode`, because this grammar has no ranker and ranked search over the company's prose is `search_knowledge`'s. `key=ENG-1,ENG-7` narrows to keys a caller already holds — upper-cased, like `container=` and `references=`, because a key is what somebody pasted and the column it is compared against is minted upper-case — and `removed=true` is the TRASH — the only way to list what a removal hid, which is what a restore is a gesture about. A parameter this grammar does not read is REFUSED naming it, never ignored: a filter nobody parsed is a board showing more than the person asked for, silently. An unknown status or group is refused naming the closed set rather than matching nothing. A custom field VALUE is checked against its own declaration at the write and refused naming the rule — never rounded or coerced to fit; see the coercion table in [the work tracker guide](../guides/work-tracker.md). `flag=` is the ATTENTION queue and its values OR: `cycle`, `too_deep`, `inconsistent_project` and `key_collision` are facts about a task's own row, and `one_sided` and `one_sided_final` are about a DEPENDENCY of it — an authored `waiting_on` whose blocker does not list it, and one whose mirror was refused permanently (the blocker is gone, was removed, or is full). The first is what the `tracker` duty repairs 30 seconds on; the second is what a person resolves. `asked_of=` is whose answer an open ask is waiting on and `asked_by=` whose question it is — the work a person is WAITING on — each a record owner's name: a bound person's seat, everybody else's login, which is the name every ask they put is authored under whichever surface they put it from. `inconsistent_project` marks a subtask filed in another project from its root, which the writer refuses and the applier flags rather than stall on if a record carries it anyway. `key_collision` marks a task holding a key another task claimed first — which is what a key counter restored beside newer work mints — and the key goes on opening the claimant on every node, so the flagged task is reached by its id; purging the claimant hands the key to the remaining holder with the lowest id. Every row this API lists an item in SAYS SO, because a row is what a link, a peek or a composed call is built from and one carrying the key alone opens the claimant: a row here and a `work_my_work` row carry `key_collision`, as do a `work_search` hit, a `work_item` answer (beside `blocked`) and each of its `links`; a `work_activity` record and a `work_inbox` notice carry `subject_key_collision` beside `subject_key` (a notice's id to open by is its `task`, since a `prioritised` notice's subject is a person); a `work_my_work` checklist item carries `task_key_collision` beside `task_key`. The rule a reader applies is one sentence — open a row by its key unless the flag is set, and then by its id — and an ask's `answer_with` already follows it. A notice's flag is asked of the key the notice STORED rather than read off the task, because a duplicate moved since answers to a new key while the one its notice carries still opens the claimant. They OR because an attention queue asks "is anything wrong with this", and a conjunction over six flags answers nothing on every company. `totals=<column>:<op>` adds aggregates over the WHOLE matched set rather than the page — a number that changed as somebody scrolled would be the one thing a header must not do. The seven ops are `sum`, `avg`, `min`, `max`, `count`, `median` and `p90`. `median` and `p90` are ORDER STATISTICS by nearest rank — the value at position ⌈p·n⌉ of the set's values in ascending order — so each answers a value some task actually holds: the median of four values is the second, never an average of two. The columns are the summable ones (`points`, `estimate_min`, the `spend_*` family — `spend_workers` and `spend_sent_back` included — `reassignments`, `reopens`, `depth`), the date columns for the four order statistics `min`/`max`/`median`/`p90` only (a sum of dates is a number of microseconds nobody meant), `tasks:count`, and `f.<slug>` for a declared number or date field. A total with nothing to add up is ABSENT rather than zero: "nothing is estimated" and "everything is estimated at nothing" are different facts. `subtasks=` is how a tree is filtered: `collapsed` (the default) and `expanded` filter ROOT tasks and let their subtrees ride along unfiltered — so a todo root brings its done subtask — while `separate` filters every task on its own. The first two answer the same SET and differ only in how a caller renders it. Asking for a subtree with `parent=` or `root=` turns the mode off, because those are questions *about* subtasks and filtering their roots would answer the parent's siblings. `any=[{…},{…}]` is one level of disjunction, ANDed with the top-level keys: a branch is a PREDICATE, so it may not carry the keys that decide the answer's own shape (`removed`, `archived`, `show_closed`, `closed_since`, `subtasks`, `fields`, `around`) or how fresh it must be (`read_level`, `max_lag_seconds`, `max_lag_seq`, `min_position`) — those are the same decision at every branch or they are incoherent, and a branch that carried one would narrow what was asked for at the top level rather than widening it. An empty branch is refused, because it matches every task and makes the others decoration. `view=<id>` and `preset=<name>` are loaded FIRST and every explicit key overrides them — a saved view is a set of defaults rather than a lock, so somebody who opens a board and picks another assignee gets the view with that one key changed. A view beats a preset (somebody saved it) and what was typed beats both. The five presets are `my_queue`, `priorities`, `triage`, `blocked` and `overdue`. `my_queue` is *what can I pick up*: a DISJUNCTION of the work the viewer holds and the work in their OWN project nobody holds, open and unblocked, most important first — both arms matter, because written as "assigned to me" alone a seat with an empty queue reads the company as having nothing for it while its project's unclaimed backlog sits there, and the second arm is scoped to their project because unscoped it offers every unassigned task in the company — so a viewer with no project of their own, a seat whose unit files none or a caller the directory binds to no seat, has no second arm and picks up what they hold. `priorities` is the viewer's own ordered list, open tasks only, IN THE ORDER somebody arranged it — that order is the answer, so nothing sorts over it, and a finished task drops out of the answer without the list being rewritten. `triage` is the unassigned open work, which with one fixed status set is the honest definition of "needs somebody to decide". `my_queue` and `priorities` are the CALLER's own — there is no parameter naming whose, because a question about somebody's queue asked on their behalf is the owner-or-lead rule's, not a filter's — read under their [own record](#whose-record-a-personal-question-answers-for), which for a caller bound to no seat is their login; a caller the engine can name nothing for is refused both, because a list with nobody's name on it is everybody's. A `view=` nothing resolves is REFUSED, never answered as the whole board. `group_by=` turns the answer into a BOARD: `groups` replaces `rows` — returning both would be the same rows twice — and each column carries its own `count` over the whole set beside a bounded slice of its rows (`group_limit`, default 20, max 100). The axes are `status`, `status_group`, `assignee`, `priority`, `type`, `tag`, `project`, `unit`, `routing_unit`, `parent`, `due:day`, `due:week`, `start:week`, `due:bucket` and `f.<slug>` for a custom field; anything else is REFUSED naming the key rather than answered ungrouped. `unit` and `routing_unit` group on the TEAM rather than on the stored string, for the reason `unit=` matches both the team's key and its name: grouping on the column drew a team whose rows hold both as two columns — both headed with its name — with its counts split between them. The expression folds every spelling onto the unit's key, and `group=` is folded the same way, so `group=eng` and `group=Engineering` load the one column. A stored unit the chart no longer has keeps its own column under the literal its rows hold, since folding it into anything would invent a home for work whose team is gone. A grouped answer mints no cursor, because across a set of columns there is no single order to be after; `group=<value>` is how a board loads one column further, and it narrows the WHOLE query, so the hint and the totals describe that column too. `group_by2=` adds swimlanes inside each column and `subgroup=` names one — a swimlane board is bounded by its CELLS rather than by either axis alone, because the work it costs is the PRODUCT of the two, so asking for lanes lowers the column cap and `subgroups_dropped` says how many lanes a column has beyond it. A `group_by=` over the WHOLE COMPANY is refused when the query's own narrowed predicate still matches more than 20 000 tasks: a board is drawn by sorting every one of them, and the refusal names the ceiling and what narrows it. It is a bounded COUNT rather than a check for the presence of a filter key, deliberately — `status_group=not_started,active` is a filter and narrows nothing, so a gate spelled "needs a narrowing filter" is one a caller clears in a single attempt without making the query any cheaper. Scoping to one project with `container=project:<key>` lifts it, because there the input is an index range whose width is one project's own size. An absent value is its own labelled column — "nobody is assigned" is a question a board answers rather than a row it hides — and `group=` with no value is how that column is loaded one further, because a key named and left empty asks for the rows with no value where an absent key asks for all of them. `group_by=due:bucket` is the one axis that is not a stored value: it is WHEN the work is due, read against the query's own day — `overdue` (still open and past it), `earlier` (finished, and past it — work delivered late is not overdue and calling it so would be a false claim, so it is its own band and is empty unless `show_closed` brings finished work into the answer), `today`, `this_week` (through the end of the current Monday-anchored week, which is the week `due=range:sow..eow` means), `later`, and the empty key for a task with no due date. Its six headings read Overdue, Earlier, Today, This week, Later and No due date. The day it cuts on is the COMPANY's midnight in the company's own zone — the same instant the row's `overdue` flag is derived from and the same one every `due=` filter compiles against — so the bands, the flag and the filters cannot disagree about a task. A band cut in the reader's own browser could and did: for anybody whose local day differs from the company's, a task landed under Earlier on a row the same answer flagged as due today and not overdue. A CLOSED axis — `status`, `status_group`, `priority` and the `due:bucket` bands — carries every column the query itself admits, the empty ones at `count: 0` with `rows: []`, in the declared order: a board is the shape of the process rather than of this week's rows, so an open-work board draws To do, In progress and In review whether or not anything is in them — and never Done, which the query excluded, because "nothing is done" said about a set that was never asked is a claim rather than an absence. The admission is the predicate's own (`status`, `status!`, `status_group`, `show_closed`, the overdue alias, `due=` for the bands, and `group=` down to one column — where a key NAMED AND LEFT EMPTY admits the undated band on `due:bucket` and nothing at all on the three whose values are never empty). An OPEN axis — assignee, tag, type, a field — carries only the values present, since every seat as an empty column is a roster rather than a board, and the second axis is never filled. `group_by=tag` is the one axis where a task is on several columns at once; the answer sets `groups_overlap` so a reader knows the counts do not sum to `total_hint`, and `groups_dropped` says how many columns did not fit. `sort=` takes `rank`, `updated`, `due`, `start`, `priority`, `created`, `title`, `estimate`, `points`, `spend_tokens`, `reopens`, `status_entered` and `removed`, each reversible with a leading `-` — `spend_tokens` is the column's own name, as a total's is, since `spend` on a row is an object of five. The numeric filters `estimate=`, `points=` and `spend_tokens=` take `lt:`, `lte:`, `gt:`, `gte:`, `range:a..b`, `null` or `not_null`. `closed_since=<date token>` is the open work PLUS what finished (done or cancelled) at or after that date, resolved on the COMPANY's clock through the same calendar as `due=` — `closed_since=sow` begins at the company's Monday midnight, so a Done lane on it is this week's work and empties itself when the week ends; it is the Board's Recent scope, and naming it beside `show_closed` is refused because both say which finished work is in the answer. `fields=tags,dependents_count,open_asks,spend` puts a board card's facts on each row, and only when asked: `tags` (sorted, `[]` when none), `dependents_count` (live tasks waiting on it through the authored `waiting_on` edges `blocking=` reads), `open_asks` (unanswered questions, by the same test `has_open_asks=` makes) and `spend: {tokens, turns, workers, sent_back, reopens}` — each present, `0` included, when asked, and absent otherwise, because the same row is what a seat reads through `list_work_items`, which never asks for them even through a saved view that does; an unknown name is refused. `around=<task>` (a key, a former key or an id) adds `around: {position, prev, next, total_hint, total_capped?}` — where that task sits in this answer's DRAWING order over the whole answer rather than the page: a flat answer's own sort, or a board read column by column (and lane by lane within a column) in the answer's column order with each column's rows in the row order, so the card after the last one in a column is the first in the next. `prev`/`next` are keys, null at the ends; on a label board the order counts cards and a task is placed at its first column; `position` is null past the 10 000 a count stops at. A task the answer does not hold answers `around: null` — never a refusal — and a saved view cannot carry `around`. A `priorities=` answer is paged in the list's own order: the whole list (at most 32) is read each page and its cursor **An absent value sorts LAST in both directions**: "soonest first" and "latest first" are both questions about values, and a task with no due date is the answer to neither — so `sort=due` puts the undated at the end rather than ahead of the one due tomorrow, and a cursor resumes in the same place the order put it. `sort=f.<slug>` orders by a custom field, LEFT-joined so a task that set no value still appears — a sort that also filtered would be two things the caller asked for once, and such a task sorts last by the same rule. The answer carries `total_hint` (capped — an exact total over an unbounded set turns a poll into a scan), `next_cursor`, `totals`, `groups`, and an echo of the `view`/`preset` it was expanded from — these answers travel detached from their requests, so a board restored from a URL can still say which saved view it is showing — plus the coverage half below |
| `work_item` | `{id}` | `GET /work/{id}` — key or id, and the id is how a task flagged `key_collision` is reached, since its key opens the task that claimed it first. Answers `{task, comments, comments_cursor?, history, links, parent?, fields, units, blocked, key_collision?, due_standing?, keys, reassignment_budget}` plus the same coverage half. `key_collision` is on the ANSWER for `blocked`'s reason — it is the applier's derived column rather than a field of the record — and each link carries its other end's. `due_standing` is where the due date stands on the COMPANY's calendar — `{days, overdue?}`: whole calendar days from the company's today to the due day (negative once it has passed, counted on the day labels so a DST day is still one day), and `overdue` for open work whose due instant is before the company's midnight, the same predicate as a board row's `overdue` — and is absent for a task with no due date, so a task page never re-derives either from the reader's own clock. `task.spend` is the item's running totals as its turns added them — read from the counters the turns wrote, never from the create's copy in the document. `parent` names the item this one is filed under — `{id, key, title, status, key_collision?}`, read in the same transaction — and is absent for a top-level item and for a parent this node holds no row for. `comments` is the newest page of the thread and `comments_cursor` the page before it, which [`work_comments`](#queries) reads. `reassignment_budget` is how many times agents may hand the item on before the engine refuses the next hand-off — the limit `task.reassignments` counts against, served so no screen carries a figure of its own — and each `history` row carries `reassignments`, the item's hand-off count AS THAT CHANGE LEFT IT (absent on a row the answering node holds no count for); an assignment made with a `reason` shows the reason as that row's `excerpt`. `units` is the task's two unit references RESOLVED against the org chart — `{filed: {key, name, resolved}, routing: {…}}`, the same shape a project row's `unit` carries — because what the document holds is the unit's KEY — the `id` the org chart gives it: a word chosen so that a rename moves nothing, and therefore a word nobody reads. The document's own `filed_unit` / `routing_unit` are untouched beside it — they are the record, and `key` repeats exactly what they hold, so a filter link built from it reaches the same rows. `resolved: false` is the finding "this names a team the chart no longer has", and it is ABSENT for a task filed into no team at all, because that is what its two empty strings already say. `blocked` is on the ANSWER rather than on `task` because it is DERIVED — an open dependency edge, computed in the same transaction as the task, so the badge here and the badge on the board row cannot disagree; `links` say what the relations are, not whether any blocker is still open. `fields` are the task's CUSTOM fields resolved against the company's catalogue — each carrying its declared name, type and whether its declaration was archived — because a stored choice is an option's UUID and a panel rendering the raw value would print it under a heading. `keys` names the tasks this answer's `history` POINTS AT, id to item key — the same map `work_activity` carries, resolved by the same walk, and present only when `history` was asked for. A delta names the other end of a relation by its ID because a key is a fact about another task's row, so without this map the item's own History tab rendered a re-parent as `Parent: 1d573f85-… → 50a01576-…` while the company-wide log rendered the same commit as `Parent: — → ENG-1`. An id this node holds no row for is simply ABSENT, so a renderer falls back to the id |
| `work_catalogue` | `{archived}` | `GET /work/catalogue`. Answers `{types, fields, policy_version, types_version, fields_version}` plus the coverage half. `policy_version` moves on every *fields* edit and is what a task's policy stamp records having validated against; a *types* edit does not move it, because the two are separate objects on separate subjects so an unrelated edit never invalidates every task's stamp |
| `work_projects` | `{q, unit, archived, sort, limit}` | `GET /work/projects`. `task_counts` (`{todo, active, done, closed}`) is read from `tracker_projects.open_count/active_count/done_count/closed_count` — `todo` being the unfinished work less the started part — MAINTAINED by the task apply whenever a status group changes or a task enters, leaves or is removed — never aggregated per poll, which over every task in every project is what a sixty-second refresh used to cost. `last_change` (`{at, actor, actor_kind}`) is maintained on the same row and by the same apply, from the commit that writes the project's own history row: it is the HEAD OF THAT PROJECT'S ACTIVITY FEED, so the two never disagree — a turn's spend, a board re-order and an edit to the project's own settings write no such row and do not move it. It is OMITTED for a project no work has ever been filed into, because "nothing yet" is a different answer from any instant. `unit.resolved` is a FIELD rather than an absence: "this project names a unit the chart no longer has" is a finding, and an absent unit would be indistinguishable from a project that names none. `archived` and `sort` are both CLOSED SETS the engine owns, and both act on the whole company rather than on the page: the answer is capped at 200 rows, so a caller that widened and then narrowed found no archived row at all once the live projects filled the page, and a caller that re-sorted the page ranked the first two hundred keys rather than the company. `total` counts the SELECTED set, which is what makes "N of M" readable on every one of them — and it IS `census[archived]`, computed from the one aggregate rather than a second `COUNT(*)`, so the number printed beside the rows cannot disagree with the counts printed on the control that chose them. The census is what a SEGMENTED screen needs and cannot derive: on the live segment the archived count has no row on screen to be derived from, so without it an empty answer could not tell a company with no projects from one that has archived every one of them |
| `work_project` | `{key, for_type}` | `GET /work/projects/{key}`. A SET read, not a point read: its shape is dominated by aggregates over task rows — the counts — so it carries `complete`/`incomplete` under the same contract every set answer takes, and its closure is the project's container plus both catalogues. It carries the listing's `task_counts`, `target_date` and `last_change` too, read from the same row so the directory and the project's own page cannot disagree. `shadowed` names the workspace field ids this project redeclares, which is what a field in the middle of a move between scopes looks like |
| `work_workload` | `{unit, read_level, …}` | `GET /work/workload`. Who is carrying how much, for everybody at once. It counts OPEN work — every task assigned to somebody, whatever its dates — because "who is carrying the most" is a question about a whole queue. Both measures a company may size in are carried rather than one chosen, since which is used differs by team and an answer that picked one would be wrong for everybody sizing in the other. Beside them it carries `blocked`, `overdue` and `unscheduled`, because a person whose whole queue is blocked has a different problem from one who is simply busy; `overdue` is cut on the company's own midnight, the day `work_items` and `work_my_work` cut on. Ordered heaviest first and capped, with `truncated` when the cap was reached. `unit=` narrows to the people whose open work sits in projects that unit owns, named by the unit's key or by its name, in any case |
| `work_flow` | `{bucket, points, project, read_level, …}` | `GET /work/flow`. The company's work as a SERIES: `points` windows (1–90, default 14) of a `bucket` (`day`, the default, or `week`) on the company's clock, oldest first, the last the window now falls in. Each point is `{window, start, end, not_started, active, done, closed, completed}` — the census at the window's END (at now for the current one) and how many changes in it took a task from not delivered to delivered (a cancellation is not one; done → closed is not a second). Replayed BACKWARD from today's census over `tracker_history` (status, create, remove, restore, purge and project moves, through the partial index replicated migration 0029 adds), so the cost is the window's changes, not the company's history. `now` is `{not_started, active, blocked, overdue}` — `overdue` cut on the company's midnight — and `blocked_history` is always `false`: nothing records when a task became blocked, so no blocked series is drawn from a guess. `project=` narrows to one project, following a task to where it was at each instant |
| `work_item_turns` | `{id, cursor?, limit?, read_level, …}` | `GET /work/turns`. One page of the agent TURNS charged to an item, newest first: `{item, key, turns, next_cursor?}` plus the coverage half. Each turn is `{turn_id, ordinal, seat, trigger?, segments, tokens, cache_read, rounds, wall_ms, workers?, sent_back?, outcome, phases, failed_in?, summary?, review?, tools?, at}` — a turn's SEGMENTS folded into one (a turn that parked on a coding run is charged once per segment): tokens, rounds and wall time summed, `phases` in the order each first ran, and `outcome`, `summary` (what it did, in the agent's words) and `review` (what the newest review that sent the work back asked for) off its newest segment; `tools` is each tool its executor called with its `calls`. `failed_in` names the phase that failed when `outcome` is `failed` because one did — `phases` lists every phase that ran, the failed one included — and is absent for a failure outside every phase and on a turn that did not fail. `ordinal` is "Turn n" — the position of the turn's counted segment among every counted segment on the item, so the newest turn's `ordinal` is the item's `spend.turns`; a turn with no counted segment on this item (more of a turn charged to another) has `0`. `at` is when the newest segment landed. Read from the tracker's own turn rows — the account `spend` sums — so it answers on any node and outlives the thirty days a node keeps its event history, where `turns?work_item=` reads the event history. `next_cursor` is the position of the oldest turn's first segment; the next page is the turns whose first segment is below it, so a turn that gains a segment while you page never moves between pages. `limit` defaults to 20 and is held to 50; a cursor this endpoint did not hand out is `bad_params` |
| `work_comments` | `{item, cursor?, limit?, read_level, …}` | `GET /work/comments`. One page of an item's THREAD, newest page first and each page in the order it was written: `{item, key, title, comments, next_cursor?, key_collision?}` plus the coverage half. `next_cursor` reads the page before and is absent when this page reaches the first comment; it continues from `work_item`'s own `comments_cursor` exactly, since both are the same read. `limit` defaults to the detail's 20 and is held to 50. Each comment's `author` is who wrote it — a bound person as their seat, beside `author_kind` — so a pane draws a name with no second lookup. `key_collision` says the item's key opens another task, as on `work_item`. Its own question beside `work_item` because nothing followed that cursor, so every comment older than the twentieth was unreachable, and because a pane that draws a conversation wants the thread without the history, links and fields a detail assembles |
| `decisions` | `{handle?, read_level, …}` | `GET /work/decisions`. What waits on ONE PERSON's decision: the open asks put to them — to their own record, a bound person's seat — and the coding runs parked on a question put to them (`awaiting_clarification` or `reseed`), newest first, at most 50. `{handle, items: [{kind: "ask"\|"run", at, ask?, run?}], total, capped, oldest_at?}` — `total` counts every one, not the page; `capped` says the ask count stopped at the engine's ceiling, so `total` is a floor; `oldest_at` is when the longest-waiting began. An ask is `work_my_work`'s `asked_of_me` row and a run is `sandbox_runs`' row. The same scope rule as `work_my_work`: absent is the caller's own record, and a named one is resolved first — a login to the record its holder's work is kept under — and then takes whoever leads them, or `fleet:operate` |
| `company_feed` | `{kinds, actor, limit, cursor, read_level, …}` | `GET /feed`. What the company DID, newest first: `completed`, `created` and `handoff` from the tracker, `page` (created or saved) from the knowledge base, and `schedule` fires from the usage domain — `kinds` a comma list, empty for every kind this node keeps; asking for one it keeps no source for is `bad_params`. `limit` is 1–50 (default 20). `{rows: [{kind, at, work?, page?, schedule?}], next_cursor?, complete, read_level}`. A tracker row carries its task's key and title, and — by kind — the task's running `spend` (`{tokens, turns}`, tokens only) and `first_pass` (no reviewer sent it back), the create's `origin` (`{surface, conversation}`: the chat surface the filing turn was woken on), or the hand-off's `from`, `to`, `reassignments` and `reassignment_budget`. A `schedule` row is a RUN — a fire the scheduler dispatched, never a tick it skipped (those stay in `schedule_runs`) — and consecutive runs of one schedule for one runner that no other row falls between are ONE row: `schedule.runs` counts them (at least 1) and `schedule.since` is the oldest one's instant (set when `runs` is above 1). A page that folds many runs reads its source again from where it stopped, at most four reads per source; past that the page is answered short, never out of order. MERGED BY INSTANT, CURSORED PER SOURCE: `next_cursor` resumes each source exactly where this page stopped consuming it, so a scroll never repeats or skips a row however the three interleave, and is absent once every source is read to its end. A `read_level=session` floor names the TRACKER's log (this is a tracker session question); the pages half is read at the dashboard's own level |
| `work_activity` | `{task, container, kinds, actor, actor_kinds, assignee, q, notified, since, from, to, limit, cursor}` | `GET /work/activity`. Every commit writes a history row, so this is an account of what HAPPENED rather than of what was announced — `notified` is how a reader tells the two apart, and `kinds` is what the change WAS whether or not anybody heard about it. The two used to be one: a record's kind was read off its notification, so the same change filed under one word with an audience and another without, and a quiet removal reached the feed as `tombstone` while the filter spells it `removed`. `kinds` accepts the twenty-seven change kinds and nothing else — one this build does not have is refused `bad_params` naming the set, by this route and by a seat's `task_activity` alike, because a filter matching nothing answers an empty feed that reads exactly like a quiet company. Each record carries BOTH instants: `at` is the authored one a card renders, and `effective_at` is the fleet-agreed one every duration is measured on. `actor` is who made the change; `assignee` is whose work it is, and the two are routinely different people. `actor_kinds` is a CSV of `agent`, `human`, `operator` and `system` and narrows to WHO WAS WRITING rather than to which handle — which is not the same question and cannot be asked as a set of handles, since an `operator` commit carries a token's own label where an `agent` one carries a seat handle, and the set of people is the roster, which changes. It is REFUSED when it names a kind this build does not have, as `kinds` is: a filter silently ignored answers a wider question than the caller asked, and on an audit feed that reads as a company where everybody is an operator. The `q` gate is on what the query would SCAN, never on which keys were named — `container=workspace&q=` and a five-year `since` both name a key and narrow nothing. `fields` is what MOVED, in the same `{from, to}` shape for every kind, the full list of names a task's own row draws on is there too, and `keys` names the tasks those deltas point at — `ask` is the question a commit asked or answered, in `work_inbox`'s shape and read the same way; see [`GET /work/activity`](#routes). `subject_key_collision` says the record's `subject_key` opens ANOTHER task — one that claimed the key first — so a link to the subject goes by `subject_id`. A `cursor` or a position `since` must be on the tracker's own log, and one from another domain's log — or a `cursor` that is not a position — is refused `bad_params` rather than picking a page by a number from elsewhere |
| `work_my_work` | `{handle}` | `GET /work/my-work`. Seven lists, each bounded at 20 so no block crowds out another — the whole answer is read as one page. `priorities` is NOT re-sorted: the order is what somebody decided. A finished or removed task is filtered out of it rather than rewritten out, because a read must not write to somebody's own object. `totals` counts each list IN FULL — `{priorities, assigned, asked_of_me, checklist_items, collaborating, watching_recent, unblocked_recent}`, each `{total, capped}` — by the predicate that drew its page and in the same read, so a count is drawn from there and never from a list's length; `capped` means the count stopped at 10,000. Every row's `overdue` mark is cut on the company's own midnight — the [`timezone`](../getting-started/configuration.md#the-companys-clock) a board's due bands are cut on — so a task due today is not overdue here while a board says today. `handle` DEFAULTS to the caller's own record, and naming anybody else's takes the owner-or-lead rule — whoever leads them, or `fleet:operate` — see below. Every task row carries `key_collision` and every checklist item `task_key_collision`, and an ask's `answer_with` names the task by its key or, where that key opens another task, by its id |
| `work_inbox` | `{handle, unread, primary_only, snoozed, reasons, limit, cursor, since}` | `GET /work/inbox`. One person's notices, newest first, 50 to a page. Each names the ONE reason of eighteen it reached them under, `addressed` (it asks something of them rather than informing them), `fallback` (nobody better was found), and their own read and snooze marks. `actor` is who made the change and `actor_kind` its kind — a bound person as their seat, a credential nobody is bound through under its login. `comment_id` and `turn_id` are the comment the change wrote and the turn that made it; `ask` is the question the notice is about — the ask itself for `asked`, the ask it answered for `answered` — as `{comment, asked_of, open, answered_by, answered_at, resolved, choice, decision}`, read NOW, so an ask somebody has since answered reads `open: false`. `snoozed` is `exclude` (the default), `include` (snoozed notices kept and marked) or `only` (just what was put off); a snooze whose time has come is back under every scope and is not `only`. `snoozed`, `unread`, `primary_only` and `reasons` all narrow the SCAN rather than the page, by the same rules the marks are computed with, so a page holds `limit` notices whenever the scope does — they used to be applied to the page after it was read while the cursor was taken before, so a person with their newest fifty notices snoozed or read opened an empty page with a cursor behind it. `subject_key` is the key the notice's task held when it was written, and `task` is that task's id — the subject for a task commit, and the task a lead put first for a `prioritised` notice, whose `subject_id` is the PERSON whose list it is; a notice naming no task carries none. `subject_key_collision` says the stored key now opens a task OTHER than `task` — asked of the stored key itself, so a notice about a duplicate moved since is still flagged — and the notice is then reached by `task`; it is never set on a notice that names no task, whose key is the only address it has. `primary_reasons` is the split that was APPLIED, defaulted, so a caller renders *you are seeing these because* without repeating the rule; `unread` and `primary` are counts over the PAGE and say so, because a total over the table is a second scan of rows this answer did not return. `reasons` FILTERS rather than classifies — the primary split classifies the same rows — and an unknown one is refused naming the eighteen. `since` is a log POSITION (`<stream>@<generation>:<sequence>`, what `seen_through` renders), never a bare sequence — and a position on THIS log: a `since` or a `cursor` from another domain's log, or a `cursor` that is not a position, is `bad_params`, because both are compared against the tracker's own sequence numbers and one from another log would pick a page by a number that means nothing here. Same scope rule as `work_my_work` |
| `work_search` | `{q, limit, mode?}` | `GET /work/search`. The company's work RANKED against a phrase — `hybrid` by default (BM25 over the engine's own inverted list fused with the semantic scan over the replicated vectors), or `keyword` / `semantic` alone, which is the same fan-out a seat's `search_work_items` runs. Not a filter: `work_activity`'s `q` is an escaped LIKE over an excerpt, gated to a span of days, and answers a different question. Answered only where the company keeps its tracker in the engine — a company whose tracker is a vendor's, or none, gets `unknown_query`, as it does for every other work question, and a node with no company yet `unavailable` with `refusal: "no_active_revision"` — and holding the board is not holding an index: a node that joined recently has every row and no index, and answers `available: false` with `reason: "building"` rather than an error or an empty result — nothing is wrong, and a reader told *nothing matched* files the duplicate. A hit carries `rank`, its 1-based PLACE in the answer, and never a score: the fan-out finishes the ordering on the nodes that scanned each slice of the corpus and hands the coordinator ids best first, so a score on the wire could only ever read zero. It carries the item's `id`, `key`, `title`, `project`, `type`, `status`, `assignee` and `priority`, the index's own `snippet`, and `key_collision` exactly as a board row does. The answer carries the same outcome fields as `knowledge` — `mode`, `served_mode`, `modes`, `degraded` and `coverage{nodes, complete, buckets_missing}` — and a mode it does not know is refused |
| `work_routing` | `{record_id}` | `GET /work/routing/{record_id}`. Who ONE change woke, and under which reason — the fact no other tracker records. `tracker_notifications` has always been readable by RECIPIENT (`work_inbox`); this is the same rows by RECORD, which is a primary-key prefix scan and needs no index of its own. Each recipient names the ONE reason of eighteen that found them, `addressed` (it asks something of them), and `fallback`/`fallback_rank` (nobody better was found). `notified` is the history row's own flag and means the commit CARRIED a notification — never that somebody was woken, since the applier deliberately does not hold the roster that would need. So an empty recipient list is THREE facts and `delivery` tells them apart: `nobody` (announced, inside the retention window, and every candidate was the actor or has left), `swept` (older than `tracker.native.inbox_retention_days`, so their absence is not evidence), `unknown` (no horizon stated) and `quiet` (the commit announced nothing, which is most of them). `retained_from` is the instant that decision was made against |
| `viewer` | `{}` | `GET /viewer`. `{login, grants, handle, name, kind, owner, acts, project}`; `acts` is what [`/operator/act`](#operatoract--the-dashboards-write-surface) would serve this caller — the catalogue's writes the authority table can admit them to before any object is named — and `project` is where their `create_work_item` lands when it names none: the engine's own default for their seat (the seat's project, else its unit's, else the nearest ancestor's), so a screen offering "Create task" says where rather than working out a second answer; `""` when there is none, and a create must name one. Registered on EVERY build with no seam of its own: who is asking is a property of the request rather than of anything this node stores. A bound caller carries a `handle`; an unbound one carries a login and no seat, which is an ordinary state rather than a refusal. `owner` is the name the caller's OWN record is kept under — the seat when bound, the login when not — and it is what a screen asks the personal questions by, never `handle` |
| `work_person` | `{handle}` | `GET /work/people/{handle}`. Scoped like `work_my_work`: absent is the caller's own record, and somebody else's takes their lead or `fleet:operate`. `due` is the snoozes whose time has come, REPORTED rather than promoted: putting one back in the unread list is a write, and a read that performed one would change fleet state from a path with no operation id and no record. `priorities_set_by` is who last set the queue when it was not this person — the name their writes are recorded under, a bound person's seat — which is how a lead's authority is made visible beside the `prioritised` wake the write sends them — a wake that names the task now at the top of the list, because a notification here is task-shaped and "your list changed" names nothing to act on. `max_snooze_ahead` is how far ahead a snooze may be set, in SECONDS — the engine's bound, so a screen offers only the presets `mark_inbox` will accept |
| `work_views` | `{container, counts}` | `GET /work/views`. `container` is the strip's own — `workspace`, `project:ENG`, `unit:engineering`, `person:ana` — and it is REQUIRED, because a strip belongs to exactly one. Whose personal views appear and whose pins come first is the CALLER's, never a parameter — their [own record](#whose-record-a-personal-question-answers-for), which is their login when they are bound to no seat; a caller the engine can name nothing for gets the shared strip — no pins and no personal views but the shared ones. Every row carries `builtin`, which is what tells the six nobody saved from the ones somebody did: a builtin row has no `id`, so there is nothing to rename, protect, rank or pin. `params` is the saved query in `work_items`' own parameter names — this channel's, not the `list_work_items` TOOL's, which renames four of them for a model — so a caller either hands them straight back or, simpler, passes the view's `id` as `view=` and lets the engine expand it. `counts=true` adds `count` to every row PINNED for the caller — the `total_hint` `work_items{view, container}` answers for it, run as the caller on the company's clock, with `count_capped` when it stopped at 10,000 — or `count_refused` naming why a view that no longer compiles could not be counted. At most 32 counts, in the strip's one read; only a record has pins, so it is refused `bad_params` to a credential that names no record of its own |
| `work_saved_views` | `{counts}` | `GET /work/views/saved`. Every SAVED view — never a builtin — that the caller can see across every container: the shared ones, and the caller's own personal ones, pinned-for-them first and then by container (the workspace, projects, units, people) and the strip's own rank. Each row is `work_views`' row shape, so `container` says where it lives and `params` is its saved query. A sibling of `work_views` rather than a `container=` it takes, because a strip is ONE container's tabs and this is one PERSON's views — the inventory of what somebody saved and the pins a sidebar draws — which the workspace strip could not answer: a view saved on a project board appeared in neither. Whose is the caller's [own record](#whose-record-a-personal-question-answers-for), never a parameter, and `counts=true` takes the same rule as `work_views`: every pinned row's `count` is its view run IN ITS OWN CONTAINER, which is what opening it runs |
| `pages` | `{container, parent, roots, status, label, watcher, title, skills, onboarding, limit, after}` | `GET /pages`. `skills` is three-stated: only the tool-skill pages, everything but them, or everything. `roots=true` is the TOP of a container — the pages with no parent — and is refused beside `parent`, because an empty `parent` already means "under any parent" and the two ask opposite questions. The answer carries `total` (every page the filter matches, counted in the same transaction as the rows) and, while there is more, `after` — pass it back as `after` for the next window; a cursor this listing did not mint is `bad_params`. Each page carries `children`: how many pages sit directly under it that the SAME listing would show (its `status` and `skills` narrowing applied to them), absent when none — what a tree draws its expander off. With `skills=true` on a node that reads the replicated `usage` domain the answer also carries `skill_loaded_by`: page id → the `page` answer's `skill_loaded_by` list for that page, answered for the whole window in ONE read rather than one per row |
| `page` | `{id}` | `GET /pages/{id}` — id or `CONTAINER/Title`. `children` is the first 50 by title and `children_total` all of them. `skill` says the page is a TOOL SKILL (admitted to the skills registry) and `onboarding` that it is an onboarding page. On a tool-skill page, `skill_loaded_by` is every seat it reached as a skill over the last 30 company days, most recent first — `{handle, last_at, count, loaded, offered}`, where `loaded` counts the seat asking for the body (`load_tool_skill`) and `offered` a phase's catalogue putting its summary in front of the seat; absent on a node that does not read the `usage` domain, and `[]` when nobody was. `linked_from` is who LINKS here, from this node's index: `pages` (`{id, container, title}`, published pages whose body carries this page's address) and `tasks` (`{id, key, key_collision?, title, status, via}`, where `via` is `linked_page` for a page relation and `description` for an address in the description, and `key_collision` marks a task whose key another task claimed first, so a link to it goes by its `id` — the key opens the claimant), each capped at 50 with `pages_total` / `tasks_total` beside it. A link is a page id in either address the engine reads — `/pages/<id>` or the dashboard's `#/knowledge/pages/<id>` — outside code; a title is not an address, since a rename moves it. Absent on a node with no index. A node that HAS an index and cannot answer sends `linked_from_status` instead of an empty list: `building` while its index is on its first lap over pages and tasks (a fresh or joined node's first minutes — an empty list then would claim nothing links here before every body was read), `unavailable` when the read failed; the page itself is served either way |
| `page_reads` | `{page, days}` | Who READ a page: one row per (seat, way of reading) over `days` company days (1–30, default 30, anything else `bad_params`) from the replicated `usage` domain, so every node's reads are counted — a departed node's included — and every node answers alike. `via` is `get_page`, `search`, `prefetch` or `skill_loaded`; a skill's catalogue OFFER is deliberately not a read (it would make every skill "read by every agent today") and is what `page`'s `skill_loaded_by` counts. Each row carries `count`, `last_at`, and the newest read's `last_turn_id`, `last_work_key`, `last_query` and, where the run was charged to a task in the engine's tracker, `last_work_item{id, key, title, ordinal}` — "turn 2 on ENG-412". At most 100 rows, newest first, with `readers_total`; `distinct_seats_today` is the seats that read it since the company's midnight, and `elided` how many (page, way) entries the per-seat-day cap dropped across the window, COMPANY-WIDE rather than for this page — some may have been this page's and none may have been, so a non-zero value says the list MAY be short (0: it is certainly complete). `since`, `until` and `days` name the window. Registered only where this node reads the `usage` domain |
| `containers` | `{}` | `GET /containers`. A separate question from `pages` rather than a facet of it: a browser draws the container list once and the page list on every navigation |
| `page_activity` | `{page, container, kinds, actor_kinds, since, cursor, limit}` | What happened to a page, or to everything in a container — the wiki's own change log, mirroring `work_activity`. `kinds` is a CSV of the ten change kinds and `actor_kinds` of the three author kinds (`agent`, `human`, `operator`), the latter refused when it names one this build does not have — `work_activity`'s note says why. `since` bounds the window and `cursor` pages it: the same unit, two parameters, because the cursor moves with every page and the bound does not. The answer carries `log_seq` and `applied_through` as a tracker answer does — this node's checkpoint on the pages log and the prefix of it whose records it applied, read in the transaction that read the changes — so a node holding a record it cannot decode says it is behind |
| `page_revision` | `{page, version}` | One revision's own body, message and author. Revision N is the body AT version N — including the newest — so a reader comparing two versions asks for both rather than for one and the head |
| `config` | `{}` | `GET /config` *(needs `config:read`)* |
| `config_audit` | `{limit}` | The revision history — no REST twin; `GET /config/revisions` serves the same records *(needs `config:read`)* |
| `config_diff` | `{revision_id}` | [`GET /config/revisions/{id}/diff`](#get-configrevisionsiddiff) — the listing is cut at 500 and `changes_total` is how many there are *(needs `config:read`)* |
| `config_entities` | `{kind, id}` | One addressable collection of the active revision: its ids, or one entity out of it. The read half of Settings › Configuration, whose write half is `PUT /config/{kind}/{id}` *(needs `config:read`)* |

**Every tracker answer carries how far this node had got, and both halves
matter.** `read_level` is the level the read was ACTUALLY served at, never the
one asked for — a level never silently downgrades, so the two can differ only
by a refusal you can see. `log_seq` is this node's committed position and
`applied_through` the prefix whose consequences it actually holds; they are two
numbers because a node applying nothing while its position advances looks
identical to a caught-up one from either alone. `log_lag` is **absent** rather
than zero when the broker could not be reached, because a read asks how far
behind an answer may be and an unreachable broker answering "not at all" is the
confident wrong answer. A caller that will not accept an answer of any age says
so with `max_lag_seconds` and/or `max_lag_seq` beside `read_level=stale`, and
the read refuses `too_stale` past whichever is reached first — the two are
readings of ONE distance rather than two, the record count being what the
broker actually answers and the duration being derived from it through this
node's own drain rate. Both are refused at every other level, because they are
a staleness bound and nothing else is. **`min_position` is the read-your-writes
half**: a log position — `<stream>@<generation>:<sequence>`, the form every
write's answer carries as `position` and every wake carries — and the answer
is served from no earlier than it, at whatever level. At `stale` it turns
"whatever this node holds" into "whatever this node holds, from here on", still
labelled with its lag; a node that has not reached it within the read budget
refuses `behind` rather than serving rows from before the write, and a position
on another domain's log is refused `400 bad_params` rather than waited for —
the caller's mistake, which every node answers the same, so it is never an
`unavailable` telling a client to ask another node. It is
also what makes `read_level=session` an honest ask here: a session read waits
for the caller's own last write, so it is accepted **only beside a
`min_position`** and refused without one, the refusal naming the key and the
two other asks a caller who typed `session` might have meant — `linearizable`,
or `stale` with `max_lag_seq`. Absent, the level resolves to **this surface's**
default, which is `stale`; a seat's own tools resolve theirs to `linearizable`
and cannot be asked for anything else. See [Read consistency](../guides/consistency.md).

**`read_level`, `max_lag_seconds`, `max_lag_seq` and `min_position` work on
every question that carries a read level**, not only on `/work/items` —
`/work/items/{id}`, `/work/views`, `/work/catalogue`,
`/work/people/{handle}`, `/work/projects`, `/work/projects/{key}`,
`/work/activity`, `/work/my-work`, `/pages`, `/pages/{id}`
and `/containers` all resolve them the same way. They did not until recently:
each of those wrote a hardcoded `stale` into its read and never looked at the
keys, so a caller asking for a stronger answer was served this node's rows and
told, in the answer's own `read_level`, that it came back at the level asked
for — and the two single-object reads, `/work/items/{id}` and `/pages/{id}`,
then took the level and dropped the bounds beside it, on the claim that a bound
is enforced against a set's coverage and one row has no set. It is not: a bound
is about **this node's lag**, checked before any row is read, and one row is
exactly as far behind as a board on the same node. And `complete: false` — with `incomplete` naming the
count, the lowest position among the records (`from`, as `{stream, generation,
seq}` — the object form every position in an answer takes, `seen_through` and a
listing's `position` included; a position in a *parameter* is the token
`<stream>@<generation>:<sequence>`) and the record version — says rows may be missing,
rows that should have gone may still be present, and the totals were computed
over the incomplete set. That is a different fact from staleness, and a client
that renders `read_level` and swallows `complete` looks confidently right.

### Whose record a personal question answers for

Four questions answer about **one person or seat** rather than about the
company: `work_my_work`, `work_inbox`, `work_person` and `conversations`. Every
one of them takes a `handle`, and the rule for whose is the same, in one place:

1. **No handle** answers for the caller's **own record** — and for
   `conversations`, their own seat.
2. **Your own handle** is the same thing said explicitly — and on the three
   person questions either of your names is: a bound person's login names the
   same record their seat does.
3. **Somebody else's login** names **their** record, looked up in the identity
   directory: the seat it binds them to — followed through a rename — or the
   login itself for somebody bound to none. `ana.diaz`, bound to the seat
   `ana`, reads `ana`'s inbox, which is where everything addressed to her is
   kept; it used to be read literally, as an empty record under the login. A
   login is never matched against the chart's seats. A **`token:<id>` login**
   is held by the Tier A entry of that id in the answering node's
   `api.auth.tokens`, not by a directory row: it names that token's record —
   the seat an active directory row binds it to, or the login itself — and a
   `token:` login no entry declares is held by nobody, whatever the directory
   says, because no credential can act as it. So a mistyped `token:opps`
   names no record rather than a queue kept for a credential that does not
   exist. **The directory is asked only after the question has decided what
   it can without it**: `fleet:operate` is admitted whatever the login turns
   out to be, and a caller who leads somebody may be, so for those two the
   login is looked up and the question decided again on the record it names;
   everybody else — somebody who leads nobody — is refused exactly as they
   would be on a seat they do not lead, before anything is looked up, so the
   refusal never says whether the login exists or whose seat it holds. A
   login nobody holds, or one whose holder is bound to a seat the chart no
   longer has, then names no record (`404`) for `fleet:operate` and is
   refused as below for a lead, who leads nobody by that name. A node that
   cannot read the directory answers `503` — to those two only, and without
   the directory's own words, which name the seat a login is bound to.
4. **Anybody else's handle** is decided by the authority table on the record
   it resolved to, each question asking its own verb. The first three are
   somebody's QUEUE, and take the owner-or-lead rule: whoever leads that
   person — so a lead may name their report by seat or by login — or a caller
   holding `fleet:operate`. A node that cannot read the chart to tell answers
   `503`, never a refusal — a lead told they lead nobody goes looking for an
   authority they already hold. `conversations` is a seat's TRAIL — what it
   said on a surface the engine does not own — and takes `audit:read`, the
   grant `/events` and `/agents/{id}/memory` already take for every seat at
   once; a lead does not read it by leading, and `fleet:operate` does not
   open it either.

A refusal on authority is `403 unauthorized` naming the rule that decided in
`reason` and, in `grants`, the capabilities any one of which would have
admitted the caller — `fleet:operate` for a colleague's queue, `audit:read`
for a seat's threads. An empty `grants` means no capability would: the answer
is a relation the chart does not hold.

**A caller's own record is kept under ONE name**, and it is the name every
write they make is [attributed to](#who-a-write-is-attributed-to): the seat's
handle for a person the identity directory binds to a seat, and the login —
`jane.doe`, `token:ops` — for everybody it binds to none. The tools write a
caller's inbox marks, pins, priorities and personal views under that name, and
every question here reads them back under it, so what a caller's assistant
arranged is what their screens show. The binding is the directory's: a
person's row names the seat they hold, and a Tier A token acts as a seat when
the directory binds its login to one — and what binding changes is where the
record is kept, not whether there is one.

**A seat's record survives its rename.** The tracker keeps every person it
names — an assignee, a watcher, whose inbox, queue and pins a record is — under
the handle the seat was **created** under, and answers each of these questions
for any handle the seat answers to: `chief`, renamed from `cto`, reads the
queue, the notices and the pins it had as `cto`. Every handle in an answer, and
in an `inbox_changed` frame, is the one the seat answers to now. See
[a renamed seat keeps its work](../guides/work-tracker.md#a-renamed-seat-keeps-its-work).

`conversations` is the exception, because a seat's trail belongs to the seat:
a caller bound to no seat who names none is refused `bad_params`, not
`unauthorized`. Nobody was denied anything: there is no seat to answer about,
and the remedy is a binding in the directory rather than a different
credential.

There is no parameter naming a viewer anywhere on this surface. Whose board, whose queue and
whose view strip is the CALLER's, and a question about somebody else takes the
`handle` above and the rule that goes with it — a parameter that named a
viewer was a way to be told somebody else's arrangement without being asked
who you were.

### One frame per posture, and how a frame is routed

Two facts decide what a connected client receives, and both are properties of
the *client* rather than of the push.

**Its posture decides the bytes.** A socket is served `live` or `degraded`, and
two sockets in the same posture receive the byte-identical frame produced by a
single encode. The engine marshals one push **once per posture present**, never
once per connection: with sixteen tabs open, one event used to be sixteen
marshals of identical JSON on sixteen goroutines, and the cost of a push grew
with how many people happened to be watching.

**Its route decides the audience.** Every kind in the table above is one of
three:

| Route | Kinds | Reaches |
|---|---|---|
| broadcast | `event`, `agents`, `seats`, `sandboxes`, `tokens`, `budget`, `schedules`, `org`, `tools`, `health` | Every connected client whose posture takes the kind. |
| seat | `inbox_changed` | Only the clients whose `watch` for the seat the frame names in its own `seat` field was allowed — see `watch` above. |
| direct | `snapshot`, `result`, `error`, `pong` | The one client it answers. |

A seat-routed frame that lost its `seat` is **dropped**, never fanned out: a
result carries one client's correlation id and a per-seat frame carries one
seat's audience, so the unsafe default in both cases is "everybody".

### A degraded node keeps the socket open

When a node's own posture leaves rotation for a reason that means its copy of
the company is **wrong rather than behind** — `shed` or `stuck`, the same set
[`/ready`](#routes) refuses on — its sockets move to the `degraded` posture
instead of closing. A dashboard whose socket closes reconnects on a backoff for
as long as the node is degraded, learns nothing from any attempt, and shows
"retrying"; one that stays open is told.

In that posture:

- **Keepalives continue.** `ping` is answered with `pong`, and the 5-second
  `health` frame still arrives — it is both the keepalive and the explanation,
  because its `status` names the posture.
- **Pushes stop.** Every other broadcast kind is withheld, because all of them
  are derived from the copy the fleet has abandoned.
- **Queries are refused** with `unavailable`, and the query surface is never
  reached. `unavailable` is the one code that must never be flattened into an
  empty result: "this company has no work" is an answer a person acts on. The
  frame carries `retry_after` like every other `unavailable` frame — one
  health tick, the soonest the posture can change — and names no `refusal`,
  because none of the state log's is behind it; the `health` frame is what
  says why.
- **`snapshot` still arrives** on connect. It is a direct frame and it carries
  the node's own health, so the one frame a degraded client is handed is the
  one that says why the rest stopped.

`wait` and `isolated` stay **live**, for the reason `/ready` stays ready on
them: `wait` is ordinary propagation during a rollout, and `isolated` means no
node applied the revision — blinding every operator there would be an outage
caused by one bad revision.

### Close codes

A refused handshake **cannot** carry a close code: a close code rides a close
frame, and a connection that never opened has none. That case is answered `401`
before the upgrade and is covered under [`GET /ws/stream`](#ws-wsstream) above.
These two are for a socket that is already open.

**An open socket re-checks its credential every 60 seconds** — the same
[stall grace](../concepts/identity-and-access.md) a node may serve identity it
has not caught up on — by running the guard again over the credential it was
opened with. A handshake decision alone would leave a revoked session's socket
pushing the company's state for as long as the tab stayed open. Each answer
does one thing:

| Answer | Code | What a client does |
|---|---|---|
| The session ended, expired or was revoked; the token is no longer accepted | `4401` | Re-dial with the credential the browser holds now; if the handshake answers `401`, sign in. |
| The person resolves but their seat is gone from the chart, or they no longer hold `state:read` | `4403` | Stop reconnecting and show why: the credential is fine, what it may do is not. |
| Resolved with different grants | *(no close)* | Nothing — the socket's pushes follow the new grants, and a fresh `snapshot` built for them replaces what the screen was showing. |
| This node cannot read its identity estate, or is behind it | *(no close)* | The socket is **degraded** and told so on an `identity` frame: pushes stop, questions answer `unavailable`, and the next check that can answer sends `identity: verified` and a fresh `snapshot`. |
| Resolved | *(no close)* | Nothing — and later questions are asked as the principal just resolved, so a narrowed grant takes effect within one interval. |

Both sit in the 4000–4999 range the standard reserves for applications, and
both deliberately echo the HTTP status they mean.

Nothing else closes this socket for a fault.

### Wiring

Every node that serves the API feeds its projection through one entry
point, `stream.Service.Ingest`, from an **ephemeral broadcast
subscription** to the event stream (`observe.Projector`). It receives
every event from every node, because a dashboard served by one node must
show turns that ran on another, and that is why it is not a publish
listener: a listener sees only what its own node published. The event
store takes the other route, a publish listener inline on the publishing
node, so no two nodes can write one row.  Each event
updates the live-state projection *and* fans out to connected
dashboards.  Backpressure is per-WebSocket: a stalled tab drops the
oldest queued envelope so it cannot stall the publish path or other
tabs.

The dashboard itself is a React + TypeScript application, built by Vite
from `crewlet/dashboard/` into `crewlet/static/dashboard/`, which the
binary embeds. Its wire half is `src/protocol/`: a store that mirrors the
projection and derives nothing (`protocol/store.ts`), a reconnecting WebSocket
client with heartbeat, query channel and REST-snapshot fallback
(`protocol/socket.ts`), the one REST transport (`protocol/rest.ts`) and the
one write client (`protocol/act.ts`). Every list the engine owns and the
dashboard must repeat — the event categories, the push kinds, the act
refusals, the tools a button may call — is declared once in `src/contract/`,
and a Go gate holds each against the engine's own value. Around that sit a
hash router that keeps every screen, section and filter in the URL, and one
file per screen.  `/dashboard` serves the
shell; `/static/{path}` serves its assets.  The build output is
COMMITTED, so `go build ./...` needs no Node.

The shell and its assets are served under two caching classes, decided by
where the build put the file:

| Files | `Cache-Control` | Why |
|---|---|---|
| Everything under `/static/dashboard/assets/` — the entry module, every chunk, the stylesheet | `public, max-age=31536000, immutable` | Each name carries a content hash, so different bytes are a different URL. A browser that has the file never asks again, reload included |
| Everything else — the shell (`/dashboard`), `/favicon.ico`, `/static/dashboard/crewlet-icon.svg`, the fonts, the notices and `protocol.js` | `no-cache` | The name does not change with the bytes. The browser keeps its copy and revalidates it on every load, so a redeploy is picked up on the next one; the shell is what names the new hashed files |

Every file answers with a strong `ETag`, and `If-None-Match` is read as a
list under weak comparison, so `"a", "b"`, `W/"a"` and `*` each earn a
`304`. `HEAD` and `Range` are answered too.

**Text is gzipped for a client that asks for it**: HTML, JavaScript, CSS,
SVG, JSON, plain text and the `.ico` favicon are compressed once per file
per process, at gzip's best level, and served with `Content-Encoding: gzip`
when the request's `Accept-Encoding` admits `gzip` (or `x-gzip`, or `*`)
with a weight above zero and the result is smaller than the file. A member
that names gzip outranks the wildcard, so `*, gzip;q=0` gets the file as it
is, and so does a request with no `Accept-Encoding` at all — the clients that
send none are scripts and probes, which would print the compressed bytes.
Fonts and images are never recompressed: woff2 and PNG already are. The gzip
representation has its own `ETag` (the identity tag with `-gz` before the
closing quote), and every response for a file that has one carries
`Vary: Accept-Encoding`, its `304` included. Measured on the committed build,
the four files a first load fetches go from 1.47 MB to 401 KB. A reverse
proxy in front of the engine needs no compression or caching rule of its own
for the dashboard; one that compresses leaves an already-encoded response as
it is.

`/static/dashboard/THIRD_PARTY_NOTICES.txt` (served as `text/plain`) is the
license text of every npm package the bundle contains, the design system's
three among them, written by Vite's `build.license`, followed by the SIL Open
Font License of the embedded Geist and Geist Mono faces and the ISC License of
the Lucide drawings every glyph is one of (with Feather's MIT text for the
glyphs Lucide derives from it). The release archives and the container image
carry the same file, beside the notices for the Go modules the binary links.

The product's mark is `/static/dashboard/crewlet-icon.svg`, emitted by the
build from `@crewlethq/icons` beside the raster `favicon.ico`; the tab icon, the
dashboard's lockup and the GitHub App landing page all draw that one file.

A second build target, `/static/dashboard/protocol.js`, is the wire
protocol alone as plain ESM — `src/protocol/`, the store (`protocol/store.ts`)
included, with nothing of React: `internal/e2e` replays a real company's
captured frames through it under `node`, so the client's understanding
of this contract is checked against a real server rather than against a
fixture.

Its visual system — the token layer, the measured palette, and the rules
a change has to keep — is documented in
[Dashboard Design System](dashboard-design.md).

---

## Organization

### `GET /org`

The company's charter and its organization tree, as a reader with no grant beyond state:read may see
it. The same object is the `org` section of the [handshake
snapshot](#what-the-handshake-snapshot-carries) and the body of every `org`
push, so all three surfaces carry exactly one shape.

Every reference in it is an **identity, not a display name**: a seat's
`manages` entry carries another seat's `handle` or a unit's `id`, and a unit's
`lead` carries a seat's `handle`. A unit therefore carries its `id` — its key,
which is its `id` field or its name where it declares none — so a client can
resolve a reference it reads here against something else in the same
response.

```json
{
  "name": "Nimbus",
  "mission": "...",
  "vision": "...",
  "policies": ["..."],
  "timezone": "Europe/Berlin",
  "token_budget": {"month": 40000000},
  "roles": [
    {"name": "Founder", "kind": "human", "manages": ["cto"], "availability": "CET business hours"}
  ],
  "units": [
    {
      "id": "engineering",
      "name": "Engineering",
      "type": "department",
      "purpose": "...",
      "lead": "cto",
      "goals": ["..."],
      "channel": "engineering",
      "knowledge": ["..."],
      "roles": [
        {
          "name": "CTO",
          "handle": "cto",
          "goal": "...",
          "backstory": "...",
          "responsibilities": ["..."],
          "behavioral_guidelines": ["..."],
          "manages": ["platform"],
          "token_budget": {"day": 2000000},
          "llm": {
            "execute": ["fast", "backup"], "review": ["big", "fast"],
            "subagent": ["fast", "backup"], "auxiliary": ["cheap"], "judge": ["cheap"],
            "sandbox": ["fast", "backup"], "onboarding": ["fast", "backup"]
          },
          "tool_sources": ["builtin", "mcp:search", "mcp:github"]
        }
      ],
      "children": [
        {
          "id": "platform",
          "name": "Platform",
          "purpose": "...",
          "roles": [{"name": "Platform Engineer", "goal": "..."}]
        }
      ]
    }
  ],
  "derived": {
    "seats": [
      {
        "handle": "cto", "name": "CTO", "kind": "agent",
        "manager": "founder", "managers": ["founder"],
        "reports": ["platform-engineer"], "auto_reports": ["platform-engineer"],
        "onboarding_chain": ["Engineering"]
      }
    ],
    "units": [
      {
        "name": "Platform", "type": "team", "lead": "cto", "lead_inherited": true,
        "channel": "engineering", "channel_inherited": true,
        "seats": ["platform-engineer"]
      }
    ]
  }
}
```

In that document `manages: ["cto"]` is a seat **handle** and `manages: ["platform"]` is a unit **key**. A seat is always named by its handle and a unit always by its key; neither is ever named by a display name.

**`derived` is the hierarchy the engine derives from the company it is
RUNNING**, so a client draws a chart rather than deriving one. Each rule in it
is one a second implementation gets wrong: a handle is a slug with Go's own
case mapping, a lead and a channel cascade to child units that set none, a
`manages` entry keying a unit stands for the seats in its subtree, a unit's
lead manages the members nobody else manages, and the primary manager is the
first seat in the engine's own order that manages a seat. The dashboard derived
these in TypeScript and had already diverged on three of them.

**From the org chart, not from the revision.** The seats and units are the
[chart's own log](../concepts/chart-domain.md) and a stored revision carries
neither, so this whole answer — the `roles:` and `units:` above it as well as
`derived` — is cut from the company this node composed. Only the company's
`name`, `mission`, `vision`, `policies`, `timezone` and `token_budget` come
from the revision, because those are settings.

**A renamed seat or unit states the addresses it still answers to.** A derived
seat carries `origin_handle` — the handle it was created under, present only
once a rename has moved it off it — and `former_handles`, every handle it has
answered to since, newest first; a derived unit carries `origin_key` and
`former_keys` the same way. They resolve exactly as the engine resolves them:
every current handle (or key) first, then every origin, then every former
one. A link somebody kept names the address its seat or unit had when they
kept it, and the dashboard opens that link on the object that holds the
address now and replaces the route with the current one. A seat or unit never
renamed carries none of the four.

The fields above it stay as WRITTEN, so a reader can still tell a declared lead
from an inherited one. Every list here may arrive as `null` (Go marshals a nil
slice that way); a reader treats `null` as empty. The authored `path`,
`unit_path` and `placed_by_ref` of each entry are omitted, because they say
where a seat was WRITTEN — a fact about a document an ordinary reader is given
no way to point into, and one a company composed from chart rows cannot answer
at all, since each row states its unit directly and nothing was moved by a
reference. Membership is each unit's `seats`.

**What it carries, and nothing else.** The company's `name`, `mission`,
`vision`, `policies`, `timezone`, `token_budget` and `derived`; for each seat its `name`, `kind`, `handle`, `goal`,
`backstory`, `responsibilities`, `behavioral_guidelines`, `manages`,
`availability`, `token_budget`, `llm` and `tool_sources`; for each unit its `name`, `type`, `purpose`, `lead`, `goals`,
`channel`, `knowledge`, `roles` and `children`. Every value is the one the
company document holds, as written: a seat with no declared `handle` has none
here (the engine derives it from the name), and a unit that inherits its lead
has no `lead` of its own. An empty field is omitted, and a node with no active
company answers `{}`.

**`timezone` is the one value the engine resolves**, and it is not a mixture:
it is the company's [one clock](../getting-started/configuration.md#the-companys-clock),
and an unwritten clock IS UTC, so a running company always carries a zone
name here — `UTC` where the document writes none — rather than an empty
string a client would default to its own browser's zone. Every day the engine
cuts is cut on it ("today", a due band, an overdue mark, a person's own day), so
a screen deciding which day something falls on cuts on this.

**`token_budget` is the ceilings as written**, on the company and on each seat
that names its own: one number per calendar window it caps (`day`, `week`,
`month`, on the company clock), and nothing for a window it leaves open — so
an absent key is "no ceiling", never zero. How much of each window is spent,
and when it resets, is [`GET /budgets`](#get-budgets); this is the rule those
meters count against.

**A seat's `llm` and `tool_sources` are RESOLVED**, for the reason `derived`
is: the rule is one a client would get wrong. Both are absent on a human seat,
which runs neither, and both are published **only to a reader holding
`config:read`**: they are computed from the seat's runtime half, which every
other reader is never shown, so a label derived from it follows the same rule.
`timezone` and `token_budget` are published at `state:read`.

- **`llm`** is every phase's provider chain exactly as a turn resolves it —
  keyed by phase (`execute`, `review`, `subagent`, `auxiliary`, `judge`,
  `sandbox`, `onboarding`), the first key the model that phase runs on and
  every later one a fallback in the order it is tried. A flat `llm_<phase>`
  field wins over the same phase inside the `llm` mapping, a phase naming
  nothing takes the seat's `llm`, and a seat naming nothing lands on the
  company's `default` provider or, without one, the first provider declared.
  The values are provider KEYS, the labels `providers.llm` gives its entries;
  the model, endpoint and credentials behind each stay guarded. Absent when the
  company configures no provider at all.
- **`tool_sources`** is where the seat's tools come from, in the tool
  registry's own origin grammar: `builtin` first, then `mcp:<server>` for each
  server the seat is granted, in the order `mcp_servers` declares them. A
  shared server is granted to every agent seat; a `shared: false` template only
  to a seat that declares credentials for it under `mcp_env`, its own or its
  unit's — the rule the engine starts a seat's own server instances by. It is
  the GRANT, not what is running: a server that failed to start is still
  listed, and the node heartbeat's MCP report is what says it failed.

**What it never carries.** A seat's `contact` identities, `email`, `unit`
reference, `workers`, `learning_enabled`, `mcp_env`, `sandbox`, `placement`,
`integrations` and `schedules`, and its authored `llm` / `llm_*` fields (their
effect is the resolved `llm` above); a unit's `mcp_env`, `integrations` and
`schedules`; and every company block outside the charter and its budget
(providers, MCP servers, integrations, knowledge, the tracker, notification and
learning settings, worker templates). Those are read through the
`config:read`-gated `config` query or
[`GET /config`](#config--live-config-management-auth-gated), which masks
credentials. Schedules also have a read surface of their own, under the same
posture as `/org`, and the tree does not repeat them:
[`GET /schedules`](#routes) answers every configured schedule with its task
and next run.

**Why an explicit shape.** `/org` is readable by every caller holding
`state:read`, which is nearly everybody signed in. Serialising the config's own
seat and unit types would make every field added to a seat public the day it
landed, whatever it held. The shape is declared field by field in
`internal/api` instead, and a test fails when the config gains a company, seat
or unit field nobody has classified as public, resolved or guarded.

Founder prose is served as written. Nothing in the public fields is resolved as
a `${VAR}`, so a reference typed into a goal is shown as the text it is; keep
credentials in the fields built for them.

---

## `/operator/mcp` — your own assistant

The premise of running Crewlet is that an AI manages your company. The person
doing that management is very often working through an AI of their own — a
coding agent, an assistant, whatever they already have open — and this is the
endpoint that lets it read the board, file the thing you just decided, and
read what the company has written down. Without it they are reduced to
describing the dashboard to it.

Point any MCP client at it:

```json
{
  "mcpServers": {
    "crewlet": {
      "type": "http",
      "url": "https://crewlet.example.com/operator/mcp",
      "headers": { "Authorization": "Bearer ${CREWLET_API_TOKEN}" }
    }
  }
}
```

### What it serves

The **same tools a seat holds**, not a parallel implementation: `list_work_items`,
`get_work_item`, `create_work_item`, `update_work_item`, `comment_on_work_item`,
`merge_work_item`, `move_work_item`, `search_work_items`, `get_work_catalogue`, `list_projects`, `describe_project`, `write_project`,
`task_activity`, `my_work`,
`list_pages`, `get_page`, `write_page`, `save_page`, `comment_on_page`, and
`search_knowledge`. A schema, a default, a trimmed field and the wording of a
refusal are each written once — two copies of "file an item" drift on exactly
the parts nobody looks at, and only one of the two is ever tested.

**An argument a tool does not take is refused, naming it** and the arguments
the tool does read, rather than dropped. A tool reads what its schema declares
and nothing else, so an assistant still sending an argument a tool has retired
— `save_work_view`'s old `owner` is the one that saved a personal view as a
shared tab on the project, under an answer saying it had worked — or one it
misspelt was answered as though it had asked for less, and never told which.

Every write's answer carries its three-valued `outcome`, the object's new
`version`, and **`position`** — where the record landed, as
`<stream>@<generation>:<sequence>`, or `null` on an `unknown` outcome, where
the broker never said. That is the value a client hands back as `min_position`
on any read above, so an assistant that files an item here and redraws the
board over `GET /work/items` is served an answer that includes it rather than
whatever this node happened to hold. The tools' own reads need none of it —
they read `linearizable` — but the answer travels to a client that does not.

A write's answer here also carries **`op_id`**: the operation that call *was*.
A seat's writes derive their operation ids from its turn, so a seat repeating a
call is the same operation; a call here has no turn, so each one is minted an
operation of its own and told it. To finish a write that came back `unknown`,
or a gesture that stopped part of the way through, send the call again with
exactly the same arguments and that `op_id` — every write it makes is then the
same operation again, a created item or a new comment or saved view included,
answered from the ledger where it landed and finished where it did not. A call
without one is a new operation: repeated, a create files a second item. The
argument is offered by `create_work_item`, `update_work_item`,
`comment_on_work_item`, `merge_work_item`, `move_work_item`, `place_work_item`,
`remove_work_item`, `restore_work_item`, `set_priorities`, `set_pins`,
`mark_inbox` and `save_work_view`, and held to the rule every route that takes
an `Idempotency-Key` holds that key
to: an id this engine minted, at most 128 bytes of visible ASCII — anything
else is refused naming `op_id`. An `op_id` belongs to **the one call it was
answered for**: the id names that call's tool and carries a digest of its
arguments, so brought back with the same tool and exactly the same arguments it
is that operation again, and with any other argument — another item, another
project, another title, another blocker — or with another tool, it is refused
naming `op_id` before anything is written. Nothing is ever made twice under
one, and nothing is half-made: a looser rule would answer the steps the two
calls share from the first call and write the ones they do not. To make a
different write, leave `op_id` out. One case is refused later, by the ledger,
and says so: an operation whose step now meets another object — a move whose
item somebody else moved in between, so the key it aliases is another — is
refused `op_reused`, and the answer says to leave `op_id` out.

The one field that differs is **who the call acts as**. There is no turn and no
seat here, so this surface supplies its own identity, and every tool resolves
the caller through that rather than through the turn — a read that asked the
turn first refused every call on this endpoint while the writes beside it
worked. A comment's `@handle` is resolved here too, against the company chart
current when the comment is written, so mentioning somebody from your own
assistant wakes them exactly as it does from a seat.

That identity is the **request's principal**, and nothing the caller sends
can change it — there is deliberately no way to ask this surface to act as a
seat. A person the identity directory binds to a seat acts **as that seat**:
their writes are recorded under its handle with author kind `human`, and every
tool that asks who the caller **is** rather than who wrote it keys on it —
`mark_inbox`, `set_pins` and `set_priorities` write the seat's record;
`my_work`, `get_person` and `work_inbox` answer for it, and so does the viewer
`list_work_items` expands `preset=my_queue` and `preset=priorities` against; a
`watch: true`, a comment and a create record it as the watcher; a create with
no `project` files into its team's; and the lead relation `routing_unit` and
`write_project` are gated on is resolved for it. A credential nobody is bound
through — a Tier A token, or a person bound to no seat — writes under its own
login with author kind `operator`, and keeps its own record under that login,
which is ordinary. Being the seat is also what keeps a wake from coming back to
you: the change you just made is not announced to whoever made it, so filing
work through your own assistant does not wake you about it. See
[who a write is attributed to](#who-a-write-is-attributed-to) and
[Humans in the org](../concepts/humans-in-the-org.md).

The three person writes take **moves, not lists**, and each is resolved against
the person's record as the write finds it — so two screens writing at once both
land, and nothing a call does not name changes:

| Tool | Arguments |
|---|---|
| `mark_inbox` | `read`, `unread`, `unsnooze` — lists of the `record_id`s `work_inbox` returns; `snooze` — `[{record_id, until}]`, `until` RFC3339, in the future and at most a year away; `read_through` — a log position, `<stream>@<generation>:<sequence>`, that only ever moves forward; `primary_reasons` — omitted leaves the choice, `[]` takes the default back. A notice named twice in one call is refused `invalid`; a list the call would leave past 256 entries is refused `inbox_full`. |
| `set_pins` | `views` and `favorites`, each `{add, remove}` or `{set}` — never both, and a bare list is refused naming the shape. The caps are held against the list the change would leave. |
| `set_priorities` | `handle`, `items` — the whole order, most important first — and `if_match`, the `version` `get_person` answered: given, a reorder against an older record is refused `stale_version`. |

Plus **sixteen no seat is given**: `list_work_views`, `save_work_view`,
`write_work_catalogue`, `get_person`, `work_inbox`,
`mark_inbox`, `set_pins`, `set_priorities`,
`remove_work_item`, `restore_work_item`,
[`place_work_item`](#moving-a-card-on-a-board),
[`answer_run`](#answering-a-parked-coding-run),
[`pause_seat` and `resume_seat`](#pausing-and-resuming-a-seat),
[`steer_turn`](#steering-a-running-turn), and
[`answer_knowledge`](#answering-a-question-from-the-companys-knowledge). A view is furniture — a name, a shape
and a filter, arranged so a person finds the same question tomorrow — and a
seat's job is the work rather than the furniture around it. And the
catalogue is the company's own vocabulary: a seat adding a type so its own
create succeeds is a seat editing the rules it is judged by, and the refusal it
was working around is the signal a person needs to see — which is why reading
the catalogue *is* a seat's and writing it is not. And a person's record is a
HUMAN's: a seat has a mailbox — the durable subscription the engine attaches
when it acquires the seat — and nothing on a person's record describes one.
The trash is another: a removal takes an item off every board in the
company, and a seat that could hide work it did not want to do would be marking
its own homework in the one way that leaves no trace. Neither destroys
anything — a removal is reversible at any age, and a purge (`crewlet work purge`, or `POST /work/items/{key}/purge`) is the
one that is not. A board's manual order is furniture too — where a card sits
says what a person wants looked at first — so dragging one is a person's; a
seat moves work between lanes with `update_work_item`. And a coding run's question is a person's to answer: a seat
that could answer its own run would be guessing on its own behalf. Whether a
seat works at all is a person's decision about it too: a seat that could pause
a colleague, or resume itself, would be overruling the people who run the
company. And a note to a running turn is a person redirecting the work: a seat
that could steer a colleague's turn would be directing it past the person who
asked for the work.

### Moving a card on a board

`place_work_item` is a board drag: one item dropped beside another of the same
project, in its own lane or into the next one, in one call. It is never a move
to another project — that re-keys the item and everything under it, and is
`move_work_item`, which a seat holds too.

| Argument | |
|---|---|
| `item` | The item being moved, by key or id. |
| `before` / `after` | The item it now sits directly above, or directly below — one of them, never both. It names a **neighbour, never a position**: the engine mints the new place inside its own write, between that item and the one beside it as the board stands when the move lands, so two people dragging in one project at once both land where they dropped. |
| `status` | The lane it was dropped into — send it even when it is the item's own lane, which writes nothing on the item. Omitted keeps its own. Given with neither neighbour, it is a drop into an empty lane: the status changes and its place in the order does not. |
| `if_match` | Required: the item's `version` as the board read it. A move of an item somebody changed since is refused `stale_version`, and nothing lands. A move never changes the version itself, so dragging the same card twice needs no re-read. |

It answers `{key, status, placed, rank, version, outcome, position}`. A move
across lanes is **two records** — the status change on the item, which is
history and wakes the people on it exactly as `update_work_item` does, then the
place in the project's order, which wakes nobody. If the item changes between
the two, the lane change stands and the answer says `placed: false` with the
reason in `unplaced`, rather than failing a call whose status write landed.
Dropping a card where it already sits writes nothing and answers `applied`.

A retry is the same call. The two records are steps of the call's one
operation — its `op_id` over MCP, the request's `Idempotency-Key` over
[`/operator/act`](#operatoract--the-dashboards-write-surface) — so a retry is
answered from the ledger step by step, the lane change the first attempt made
included. That is why the call sends the lane the card was **dropped into**,
never the lane it reads now: compared with the card and left out after the
first attempt's lane change had landed, the placement would be conditioned on
the version read before that change and refused as stale by nothing but the
retry itself.

### Answering a parked coding run

A [coding run](../concepts/code-sandbox.md#answering-a-parked-run) that stops
to ask a person something parks until it is answered. A reply on the
conversation it was asked in answers it — but a run started by a schedule, a
task assignment or a colleague's ask has **no conversation**, so `answer_run`
answers any parked run by naming it:

| Argument | |
|---|---|
| `turn_id` | The parked run's `turn_id`, as [`GET /sandbox-runs`](#get-sandbox-runs) lists it. |
| `answer` | What the coding agent should be told, at most 32 KiB — it is spliced into the run as one tool reply. |

It answers `{"turn_id", "agent_handle", "question", "outcome": "pending"}`:
the answer is on the inbox of the seat holding the run, and **that** node
resumes the run with it — so an answer given while the seat is paused waits
for the resume, exactly as a chat reply would. What it became is announced on
the event stream as `sandbox_run_answered` (`resumed`, `not_awaiting` or
`gone`), naming who answered: their name, its kind and the credential. An `unknown` outcome is a
delivery the broker never confirmed; answering again is harmless, because
whichever copy arrives second finds the run no longer waiting.

**Who may answer**: whoever asked for the run — the seat its requester holds — or whoever leads the run's seat, or `fleet:operate`. Everybody else is refused `forbidden`, as the authority table refuses a lead's verb, because everybody signed in holds a credential and an answer steers somebody else's work.

It refuses `not_running` for a run that is not waiting for an answer, or has no
record at all (a run that ended has none), and `peer_upgrading` while the node
holding the seat runs a build that cannot route an answer by turn — that build
would read the answer as an ordinary wake and run a turn about nothing. It is
served on every company, native backends or not: the run record is the
fleet's, and a company on Jira runs coding agents too.

### Pausing and resuming a seat

`pause_seat` stops an agent seat taking work until somebody resumes it:
it starts no new turn, its incoming mail waits on its inbox in order, and its
scheduled runs are recorded `skipped_paused` rather than sent. The turn it is on
finishes first, unless the pause asks to stop it. `resume_seat` lifts the
pause, and what waited is delivered first. See
[Agent Runtime § Pausing a seat](../concepts/agent-runtime.md#pausing-a-seat).

| Tool | Arguments |
|---|---|
| `pause_seat` | `handle` — the agent seat; `reason` — one line, at most 500 characters, optional; `stop_running` — also end the turn the seat is on at its next round. A stopped turn is not run again. |
| `resume_seat` | `handle` |

**Who may**: the seat's holder, whoever leads it, or `fleet:operate` — never every reader, since everybody signed in holds a credential.

Both answer `{"handle", "outcome", "paused", "changed", …}` — a pause adds
`paused_by`, `paused_by_kind`, `operator_id`, `paused_at`, `reason` and `stop_running`:
who paused the seat, as every write records its author, beside the credential.
`applied` means the pause is the fleet's record; the node holding the seat
carries it out from its own copy, typically within a second. `changed` is false
for a pause of a paused seat and a resume of a free one: the seat is already in
the state asked for, and nothing is announced. The one exception is a pause
that adds `stop_running` to a pause without it — the record is amended, names
whoever asked for the stop, and is announced again. Each real change is
announced once, as `seat_paused` or `seat_resumed`, by the caller whose
compare-and-set won. `unknown` is a write the store may or may not have taken,
and a retry is safe.

They refuse `not_found` for a handle that names no agent seat (a person's seat
takes no work a pause could hold), `invalid` for a reason past its bound,
`conflict` after losing four compare-and-sets in a row to other changes to the
same pause, and `peer_upgrading` while **any** live node runs a build that
cannot carry a pause — any of them may be the next to hold the seat, and an
older build would run its mail as if nothing had happened.

### Steering a running turn

`steer_turn` sends a short note to a turn **while it runs**. The turn reads it
at its next round — after the tool call in flight returns — as a correction or
addition to the work in hand, and keeps to it for the rest of the turn: the
reviewer that judges the work and every later executor iteration open with it
too. See [Turn Engine § Steering a running turn](../concepts/turn-engine.md#steering-a-running-turn).

| Argument | |
|---|---|
| `turn_id` | The running turn's `turn_id`, as the `agents` push names it on each seat's `live_call`. |
| `note` | What the turn should take into account, at most 2,000 characters. Longer is a brief, and belongs on the work item. |

It answers `{"turn_id", "note_id", "agent_handle", "outcome": "pending"}`: the
node running the turn took the note, and the turn reads it at its next round.
What became of it is recorded there, as `agent_turn_steered` — `delivered`
naming the phase and round that read it, or `expired` if the turn ended or
parked first. `note_id` is the request's own id, so a retry of one request is
one note however often it is sent.

**Who may**: the turn's seat's holder, whoever leads that seat, or `fleet:operate`. The seat is the one thing this node cannot know — the turn runs wherever its seat is held — so `fleet:operate` is admitted at once and a caller who leads nobody is refused at once, and anybody else is decided after a PROBE asks the fleet whose turn it is, without offering the note. A probe nobody answers inside two seconds is `unavailable`: nothing was offered, so there is nothing to be uncertain about.

`unknown` means no node answered the OFFER inside two seconds. A reply lost on its way
back is indistinguishable from none, so the note may have been taken; sending it
again is safe for that reason.

It refuses `not_running` for a turn that has ended or parked, `conflict` for one
already holding five notes it has not read yet (once it reads them, the note may
be sent again), `steer_unsupported` for a turn whose executor runs as a coding
CLI's own agentic loop — its rounds are the CLI's, and the engine has no round
boundary to hand a note to — `invalid` for an empty or oversized note, and
`peer_upgrading` while **any** live node runs a build that cannot take a note:
which node runs the turn is not known until one answers.

### Answering a question from the company's knowledge

`answer_knowledge` answers a person's question — the dashboard's ⌘K answer —
from what the company has written down: it searches the knowledge base
(`hybrid`, auto-drafts hidden) for five pages and the work tracker for three
items, reads each whole where this node holds it (the first 4 KiB of a native
page's body or an item's description; an external wiki's search snippet), and
asks one model to answer from those sources alone, citing each claim as `[n]`.
See [Knowledge System § Answering a question](../concepts/knowledge-system.md#answering-a-question).

| Argument | |
|---|---|
| `q` | The question, in plain words; at most 400 bytes. |

It answers:

```json
{
  "answer_md": "Run `make deploy` from `main` [1]; it is being automated [3].",
  "sources": [
    {"kind": "page", "ref": "0f7c…", "title": "Deploy runbook"},
    {"kind": "page", "ref": "5a1d…", "title": "Rollback"},
    {"kind": "task", "ref": "ENG-7", "title": "Automate the deploy"}
  ],
  "tokens": {"input": 7120, "output": 184},
  "model": "claude-haiku-…",
  "cached": false
}
```

Source `[n]` is the n-th entry of `sources`: a page by its id (and its `url`,
for a page on an external wiki), a work item by its key. `tokens` is what
**this call** spent, and nothing converts it to money. A question nothing
matches is answered in one sentence with no sources, no model and no tokens.

**Only a person bound to a seat may ask.** It takes `state:read`, and it runs on
the auxiliary model of the seat the identity directory binds the caller to — so
a principal no seat binds, an unbound person or a pipeline's token, is refused
`forbidden` on every transport rather than answered on the company's default
model, because spend has to be attributable to somebody. No seat is given it: a
seat has `search_knowledge` and a model of its own.

**It is charged to the company's windows.** The model is the asker's own
seat's auxiliary one (`llm_auxiliary`, falling back as every auxiliary pass
does). A person has no seat budget, so before any model call it reads the
company's day, week and month and refuses `budget_exhausted` — naming the
window that ends last and when it resets — if one has no room; after the call
it records exactly what the reply spent on the company's counter alone, past a
ceiling included. It is a write rather than a read for this reason: every
answer that misses the cache is a model call, and the dashboard's reads are
refetched on focus and on reconnect.

**A repeated question spends nothing.** Answers are cached on each node, 256
of them, keyed on the question (case and spacing folded) and the node's
**corpus position** — where its tracker, pages and vector logs are applied
through — so any write that could change the answer retires every older one. A
cache hit answers `"cached": true` and `"tokens": {"input": 0, "output": 0}`,
and is served even while the budget is spent. A company whose knowledge base
is not native has no position to key on, so its answers are never cached.

It refuses `invalid` for an empty question, `forbidden` for a caller no seat
binds or a seat the chart no longer has, `budget_exhausted` as above, and
`unavailable` for a counter it cannot read (nothing is spent), a knowledge base
and tracker that could not be searched at all, no model configured, or a model
that failed or wrote nothing — a reply that spent tokens and wrote nothing is
still charged.

### One catalogue, and how a retry is the same call

Every operator transport reaches a tool through **one dispatch** over one
catalogue, built per request from this node's native halves — so a verb, its
schema, its hints and the wording of its refusals cannot differ between the
ways a person reaches it, and a node handed its first company serves its tools
on the next request. The hints each tool is listed with (read-only,
destructive, idempotent, open-world) are the catalogue's own, and a verb this
company is not served is not listed at all.

An MCP call names no request of its own, so each one is a new operation and a
tracker write answers the `op_id` it was — sent back with exactly the same
arguments, it finishes that write rather than making another (see
[What it serves](#what-it-serves)). An assistant that files the same item twice
without one gets two items, because that is what it asked for twice.
[`/operator/act`](#operatoract--the-dashboards-write-surface) takes the
operation from the request's `Idempotency-Key`, scoped by the principal that
sent it, and derives every operation it writes from it, so a retry of that one
request — sent again after an `unknown` — is the first attempt's operations
rather than new ones; a create, a comment, an update, a page write, a project
or catalogue change, a saved view and a person's own marks, pins and queue all
follow the one rule.

Each tool appears only where its half of the company is native: a company on
`tracker.backend: jira` gets the page tools and not the work tools, and one on
neither gets only the verbs about the fleet's own records — `answer_run`,
`pause_seat`, `resume_seat` and `steer_turn` — answering `404 no_route` only when
it would serve nothing at all. The catalogue is read per request, so a node
that has not been handed a company yet answers `503 no_active_revision` with a
15-second `Retry-After`, and serves the tools its first revision brings up with
no restart. `search_knowledge` is the exception and is
offered against **any** knowledge backend, Confluence included — a ranked
search over the company's own wiki is exactly as useful to an assistant there.

The turn-only tools are deliberately absent: the memory tools (a diary belongs
to a seat), the skill tools (a skill is loaded into a phase), `a2a_ask` (a
colleague's answer comes back by waking a seat, and there is nobody here for
it to reach) and `run_sandbox` (a detached run resumes a suspended phase that
does not exist — answering a run a seat started is `answer_run`).

### Seeding a knowledge base with it

On the native backend this endpoint is also how a directory of version-
controlled markdown gets published — there is no import CLI for it and there
does not need to be one:

> Publish everything under `examples/nimbus-docs/` — one container per
> directory, the page title from each file's first `# H1`.

The assistant calls `write_page` per file. That handles what a flag-driven CLI
handles badly: the parent chain, a title that already exists (`save_page` with
the version it read), and a file that turns out to be a tool skill rather than
prose. The [reserved containers](../concepts/knowledge-system.md#who-may-write-where)
are refused to a seat and not to it: a person publishing the onboarding tree
and the tool skills is what they are for. A tool skill takes `config:write` on
top of `knowledge:write`, though — it is injected into every seat's turn, so
writing one is a configuration change — and a token without it is refused
`pages.skill.write`.

### Who a write is attributed to

The **person or credential the request resolved to**, converted once for every
surface that has a request rather than a turn — this one and the
[write surface](#the-human-write-surface) below:

| Who is calling | Recorded as | Author kind | `operator_id` |
|---|---|---|---|
| A person signed in at a browser, the identity directory binding them to a seat | the **seat's handle** | `human` | **`session:<lineage>`** |
| A person signed in at a browser, bound to no seat | their **login** (`jane.doe`) | `operator` | **`session:<lineage>`** |
| A person's own [machine token](#post-iamcredentials-mints-a-machine-token) | the **owner**, as above — their seat when bound | `human` when bound, `operator` otherwise | **`pat:<credential id>`** |
| A service account's machine token | the account's **login** (`svc:ci`), or its seat when bound | `human` when bound, `operator` otherwise | **`pat:<credential id>`** |
| A Tier A token | its **whole login** (`token:ops`), or its seat when the directory binds it | `human` when bound, `operator` otherwise | its login (`token:ops`) |
| A Tier A token's exchanged session ([`POST /auth/token`](#routes)) | as the token, above | as the token | **`session:<lineage>`** |

The seat half is what lets the tracker leave you out of the wake your own
change sends: nobody is woken about what they just did, and the tracker
recognises the author by the name the record carries — so a person recorded
under a token's id was woken about every edit they made. The login half keeps
the colon: `token:ops` can never be a seat's handle, where the bare `ops` could
be, and an own-record rule comparing names would then admit a credential into
the record of the seat that shares its spelling.

**The credential is recorded again beside the author**, as `operator_id` on
every work, page and chart record, so an audit can ask what one credential did
without reasoning about kinds. A machine token acts as its owner — it is their
authority being exercised, so they are the author — and `operator_id` is what
tells a write their assistant made through it from one they made themselves;
before it carried `pat:<id>` the two were the same row, and a token minted on
somebody's account could file, edit and close their work with nothing saying a
token was used. A browser session is the same question asked of a person at a
dashboard: the author is them, and `session:<lineage>` is which of their
sign-ins made the write — it used to repeat their login, which the author
already names, so two browsers, or a tab left open on a shared machine, were
one name in every trail. The lineage is what an investigation follows from a
row to the sign-in behind it: `GET /iam/people/{id}/sessions` lists it, and
`iam_session_started` announced it. A configuration revision and a stored
secret record all three the same way: `created_by` / `updated_by` is the author,
`created_by_kind` / `updated_by_kind` its kind, and `operator_id` the
credential. They had room for one name and gave it to the credential, so a
revision written through somebody's machine token named `pat:<id>` — and the
token's row, the one thing that said whose it was, is swept a week after it
lapses while a revision is kept for ever. A Tier A token is recorded as
`token:<id>` in both, since its login is its credential; the engine's own
writes (the reconcile loop, a boot import, a key the engine minted) record
their own name, of kind `system`, and no credential — except the writes a
[disconnect](#disconnecting) makes, which the loop carries out for whoever
asked and records as theirs. The identity estate's own audit events
(`iam_credential_revoked`, `iam_session_ended`,
`iam_session_generation_bumped`, …) carry it as `operator_id` beside `by`, and
the trail `GET /iam/audit` reads carries it beside `actor` — except on a
removal and a company-wide invalidation, whose records are gates pinned at
their first version for ever, so the event announcing each carries it instead
(see [`GET /iam/audit`](#get-iamaudit-pages-by-position-never-by-time)).

There is deliberately **no way for the caller to name a seat to act as**. That
would let anybody holding the token write as anybody, and a tracker whose
author field is chosen by the writer is not an audit trail.

### Every call is decided by the request that carries it

The endpoint is served **statelessly**: it issues no `Mcp-Session-Id`, opens
no server-to-client stream (`GET` and `DELETE` answer `405` with
`Allow: POST`), and decides each JSON-RPC call as the credential the `POST`
carrying it presented, on that request. Two things follow, and both are the
point:

- **A grant withdrawn is withdrawn from the next call.** Lowering
  `api.auth.max_grants`, editing a person's grants, revoking their session or
  removing a token takes effect on the assistant's very next call, however
  long it has been connected.
- **A session id is not a credential.** Nothing another client echoes can make
  it act as somebody else: a request presenting a different credential is
  decided as that credential, whatever headers it copies.

A stateful session — the transport's default — decided every call as the
request that *opened* it, which made both of those false. Nothing this surface
serves needs one: its tool list is read per request rather than pushed, so
there is nothing to notify a client about, and no tool here asks the client
anything back.

Every call that is not a proven read also leaves one `operator_acted` event —
the same record a dashboard press leaves, with `transport: "mcp"` — in the
node's event store: see [the runtime audit](#the-runtime-audit-sourceoperator).

### Why it is not under `/mcp/`

`/mcp/` is exempt from authentication wholesale, because the sandbox
[tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)
lives there and a box running generated code holds no API token — a signed
per-run token in its own path is what authenticates it instead. Mounting a
writable company surface under the same prefix would have put it behind no
credential at all. `/operator` is its own always-guarded prefix, alongside
`/config` and `/secrets`.

## `/operator/act` — the dashboard's write surface

The dashboard changes the company **as you**: every button that writes posts
one tool of the [operator catalogue](#operatormcp--your-own-assistant) here,
and the write is made by the principal your request resolves to. It is the
same catalogue, the same tools, the same authority and the same attribution as
`/operator/mcp` — this transport adds only how a call names its operation, so a
press whose answer never arrived can be sent again safely.

```http
POST /operator/act/update_work_item
Content-Type: application/json
Idempotency-Key: 0192f1a4-9b2d-7e51-8c3a-6d7e8f9a0b1c

{"args": {"item": "ENG-8", "status": "in_progress"}}
```

The dashboard's request carries its session cookie; a script sets
`Authorization: Bearer` as on every other route.

**Every principal the guard resolved may call it**
([ADR-0024](../../adr/0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)),
and every tool is decided by the [authority table](#which-grant-a-route-needs)
exactly as on every other surface — a person signed in writes as the seat the
identity directory binds them to, and a credential nobody is bound through
writes under its own login. Nobody is refused for being unbound: a write
attributed to a credential acting as itself is one an audit can still ask
about, and the table already says what each one may do. `GET /viewer`'s `acts`
names what this route would serve the caller, so a screen disables a control
with the reason rather than offering a press the engine refuses.

**Who the write is attributed to** is [the one rule](#who-a-write-is-attributed-to)
every surface with a request applies, so a write from the dashboard and one
from the same person's assistant read identically in the audit and in every
thread. The principal is resolved **once per call**, when the call is
admitted, and the write and its audit record both name that one answer: a
directory rebind or a chart rename landing while the call runs applies from the
next call on, and never admits a call as one person and makes it as another.

**The body is JSON and nothing else**: `{"args": {…}}`, declared
`Content-Type: application/json` (UTF-8). A form post, `text/plain` or no
content type is `415` before anything is read — a cross-site form can send
those without a preflight — and a key the envelope does not take is refused
`400 invalid_body` rather than ignored. `args` is the tool's own arguments,
exactly as the tool's schema names them. The body is capped at 1 114 112
bytes: twice the largest legal page, because a page escapes to up to twice its
length as a JSON string, plus 64 KiB for the rest.

**The `Idempotency-Key` header is what names a retry, and it is required.** An
operation id the client mints once per gesture — a **UUIDv7**, in the engine's
own operation grammar — and sends again, unchanged, if it never heard the
answer. An absent or malformed one is `400 op_id_invalid` naming the header:
a key minted by the engine would be handed back in exactly the answer a retry
exists because it never arrived. It must carry its own instant because that is
when the gesture began: every retry reproduces it, and it is what a node reads
to decide whether its operation ledger can still vouch for the operation — a
key with no instant would read as minted before every row the ledger has ever
lost. The key is **scoped by the principal's id**, so one key from two people
is two requests and nobody can make their write the first attempt of somebody
else's. Every id the write derives is derived from it and from what the call
sent — each record's operation, a created item's, page's or view's own id, a
comment's — so a retry is the first attempt's operations rather than new ones,
and two different calls under one key are still two; one whose operation now
meets another object is `409 invalid_input`, to be sent under a new key. For
the same reason the arguments may not carry an **`op_id`**: the header already
names the operation, and a second answer beside it is refused `400
invalid_body` before anything is written. What that buys, precisely:

- a retry whose append reaches the log while the first attempt's is still in
  flight is collapsed into that record by the log's two-minute duplicate
  window, and answered at its position. One that arrives after the first
  attempt's record waits until this node has applied it (or answers
  `unavailable` if it does not catch up), and is then decided as below;
- a create retried after the first attempt landed files under the same id
  and the same address, so it is refused `exists` rather than filed twice, and
  a page save retried after it landed is refused `stale_version` — each the
  sign the first attempt went through: re-read rather than retry again;
- any other retry is decided again against what the first attempt produced. A
  comment edit to the text it already holds, or a rename to the title the page
  already has, is `applied` with no record; anything else is written again
  under the same operation — a comment under the same comment id.

### What it answers

A write that went through answers `200`:

```json
{"tool": "update_work_item", "outcome": "applied",
 "position": "CREWLET_TRACKER_LOG@1:4711",
 "op_id": "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12.k3f9a1c0d2e4b6a88",
 "receipt": {"key": "ENG-8", "labels_created": null, "outcome": "applied",
             "version": 4711, "position": "CREWLET_TRACKER_LOG@1:4711"}}
```

`outcome` is the write's three-valued answer and `position` where its record
landed — the value a read hands back as `min_position` so the answer after the
write includes it. `pending` is durable and not yet applied here; `unknown`
means the broker never said, and the only safe retry is the same request under
the same key. A write that appended nothing answers `applied` at a `null`
position: the state asked for already holds. `op_id` is the operation the
request was made under — the key as **this principal's** operation, named by a
tag of their id, so sending it back as the header is the same operation as
sending the original key — and `receipt` is the tool's own answer, verbatim.

A tool that appends more than one record — an item filed or edited together
with a dependency, a page saved and renamed in one call, a project's tags and
settings, the catalogue's types and fields — answers for all of them: the
**least certain** outcome (`unknown` over `pending` over `applied`) at the
**latest** position, and for a work item the version it is at after the
last of them. So `min_position` never floors a read below a record the call
made, and a write with one unconfirmed record is never reported `applied`.
A later record refused after an earlier one landed answers the later
record's refusal, and its `detail` says what did land — with that record's
outcome and position — because the earlier change is on every node and "not
made" would send a person to redo it.

A refusal is the engine's envelope — `{error, detail}` with `tool` and the
object the arguments named (`item`, `page`, `project`, `handle`) beside them,
so a refusal or an unknown outcome says which object to read. A tool's refusal
is its own sentence, which names the argument it refused, and the authority
table's adds the rule's `reason` and the `grants` that would have admitted the
caller. **The status is read from the refusal's CLASS, never from its
sentence**, through the one mapping the [human write
surface](#the-human-write-surface) answers with too. The transport's own codes:

| `error` | Status | When |
|---|---|---|
| `invalid_token` | `401` | Nobody presented a credential (the guard's own refusal) |
| `identity_unavailable` | `503` | This node cannot read its identity estate, with a `Retry-After` |
| `seat_unavailable`, `second_factor_enrolment_required` | `403` | The guard's refusals of somebody it resolved — see [Auth](#routes) |
| `csrf_origin` | `403` | A cross-site write |
| `unauthorized` | `403` | The authority table refused: `reason` and `grants` say why and what would admit the caller |
| `step_up_required` | `403` | The verb needs a fresher proof of who you are; names the window |
| `unknown_tool` | `404` | The company's catalogue serves no such tool |
| `read_only_tool` | `400` | The tool is a read; ask it over the socket or its REST route |
| `unsupported_media_type` | `415` | The body is not declared `application/json` |
| `op_id_invalid` | `400` | The `Idempotency-Key` header is absent or outside the operation grammar |
| `invalid_input` | `409` | The key already names a write to another object |
| `invalid_body` / `body_too_large` / `unreadable_body` | `400` / `413` / `400` | The envelope is not one JSON object of `{args}`, carries an `op_id`, names an argument the tool does not declare, is over the cap, or did not arrive |
| `no_active_revision` | `503` | This node has not been handed a company yet; `Retry-After: 15` |
| `draining` | `503` | This node is [draining](#during-a-drain) |

And the tool's own refusal, carrying the tool's sentence as `detail`:

| `error` | Status |
|---|---|
| `invalid` | `422` |
| `not_found` | `404` |
| `forbidden` | `403` |
| `stale_version`, `conflict`, `exists`, `already_answered`, `reassignment_budget`, `inbox_full`, `not_running`, `steer_unsupported`, `budget_exhausted` | `409` |
| `unavailable`, `peer_upgrading` | `503` |
| `internal_error` | `500` — a fault of the node's own store or code: a fixed sentence, the error itself only in the node's log, and **no** `Retry-After`, since no wait clears it. It is also the answer for a failure that carried no class |

Every `503` carries a `Retry-After` where waiting can clear it and **none**
where it cannot — an evicted node, a full log, a record this node cannot
decode: ask another node, or an operator. A call interrupted before the tool
answered is `503` `unavailable`, never a refusal: whether it landed is
unknown, so the answer carries **`outcome: "unknown"`** and the `op_id`, and
says to send it again with the same key. A tool that made its write and could
not confirm it — the broker's acknowledgement was lost — is answered the same
way, with the tool's own sentence as `tool_detail`; and one this node's
operation ledger cannot vouch for adds `unvouched: true` and **no**
`Retry-After`, because the same request asked here answers the same way until
the write reaches this node — ask another node, or read whether it landed.
That key is what tells either from a tool's own `unavailable` refusal (a node
in maintenance, a sealed log), which wrote nothing and carries no `outcome` —
the class alone would read both as "nothing happened". No transport code is
also a refusal class, so a client otherwise branches on `error` alone. The
dashboard reads any other `5xx` — a gateway that gave up waiting, a success
whose body was cut short — as `unknown` too: it says nothing about an engine
that may have written.

Every act is logged as `operator_act` at info with the tool, the operation and
the outcome or refusal — never the arguments — and every act that reached its
tool leaves an `operator_acted` event with the same facts in this node's event
store, whatever became of it: see
[the runtime audit](#the-runtime-audit-sourceoperator).

## The human write surface

Everything a person does to the company's own work and pages — filing an item,
moving it, commenting, arranging a board, writing a page, taking one out of
circulation — as HTTP routes, under the name of whoever the request resolved
to. They are the **same tools** a seat holds in its turn and your own assistant
holds over [`/operator/mcp`](#operatormcp--your-own-assistant): a route's body
is the tool's own arguments, by the tool's own names, and its answer is the
tool's own receipt. One implementation of "file an item" rather than three,
because copies drift on exactly the parts nobody looks at — which field is
trimmed, which default applies, what a refusal says.

Every route is **guarded**, reads and writes alike, and the half whose backend
is not native is **absent** rather than refusing: a company on
`tracker.backend: jira` gets `404 no_route` from the work routes, not a `503`
that reads as an outage. The routes are mounted on **every** node and read the
halves each request finds, so a node that has not been handed a company yet
answers them `503 no_active_revision` with a 15-second `Retry-After` — the
tracker and the knowledge base come up with its first revision, under the same
API, and the same request is served from then on with no restart.

### The routes, and the authority each takes

The authority is the [authority table's](#which-grant-a-route-needs), decided
ONCE. Where the object is in the path — a project — the route decides the verb
itself. Where it needs a stored row — which project an item is filed under,
which space a page is in, who wrote a comment, whose record a person's name is
(a login is its holder's, which only the identity directory can say) — the
route admits on the weakest honest precondition and the verb is decided once
the row is read, by the tool's own ask or, for a verb with no tool, by the
route.

| Method | Path | Does | Decided by |
|---|---|---|---|
| `POST` | `/work/items` | `create_work_item` | `work:write` |
| `PATCH` | `/work/items/{key}` | `update_work_item`. `If-Match` is its `if_match` | `work:write`; re-routing asks the project's lead |
| `POST` | `/work/items/{key}/comments` | `comment_on_work_item` | `work:write` |
| `PATCH` | `/work/items/{key}/comments/{cid}` | Rewrite a remark: `{"body": …}` | its **author** — the table admits `fleet:operate` too, and the tracker then refuses anybody but the author, because a remark somebody else can rewrite is one attributed to a person who did not make it |
| `POST` | `/work/items/{key}/rank` | `place_work_item` — a board drag: `{"after": ref}` or `{"before": ref}` (one neighbour, never both) and the `status` lane it was dropped into. `If-Match` is its required `if_match`, and a move of an item somebody changed since is `409 stale_version` | `work:write`, and **never a seat** |
| `POST` | `/work/items/{key}/depend` | `update_work_item`'s `waiting_on`, `blocking` and `dependency_note`, and nothing else | `work:write` |
| `POST` | `/work/items/{key}/relate` | `update_work_item`'s `linked` and `linked_pages`, and nothing else | `work:write` |
| `DELETE` | `/work/items/{key}` | `remove_work_item` (`{"subtree": true}` takes its children) | the lead of the item's **own** project, or `fleet:operate` |
| `POST` | `/work/items/{key}/restore` | `restore_work_item` | the same |
| `POST` | `/work/items/{key}/move` | `move_work_item`: `{"project": "OPS"}` moves the item and everything under it. A move whose walk stopped after the item itself moved answers `200` with the item's receipt, `subtree_followed` of `subtree_total`, and a `move_stopped` instruction — the **same** `Idempotency-Key` finishes it (the answer's `op_id`, sent back as that header, which is the key even when the request sent none), and a new one is refused as somebody else's move | the lead of the item's **own** project, or `fleet:operate` |
| `POST` | `/work/items/{key}/purge` | Destroy it and every row it produced — see [below](#a-purge) | `fleet:operate`, and **never a seat** |
| `PUT` | `/work/projects/{key}` | `write_project`'s policy: `fields`, `default_assignee`, `archived` | the project's lead or `fleet:operate`; archiving never a seat |
| `POST` | `/work/projects/{key}/tags` | `write_project`'s `tags_add`, `tags_rename`, `tags_archive` | declaring: `work:write`; renaming and archiving: the project's lead |
| `POST` | `/work/views` | `save_work_view` | `{"personal": true}` makes it the CALLER's own view — its owner is always the caller's [own record](#whose-record-a-personal-question-answers-for), and there is no naming somebody else's; a shared one is its container's lead's — and a save naming a stored view's `id` also needs that authority over the view it replaces. Only a shared view can be `default` |
| `PUT` | `/work/catalogue` | `write_work_catalogue` | `config:write` |
| `PUT` | `/work/people/{handle}/inbox` | `mark_inbox` — see [below](#somebody-elses-inbox) | the person, or `fleet:operate` |
| `PUT` | `/work/people/{handle}/pins` | `set_pins` | the person, or `fleet:operate` |
| `PUT` | `/work/people/{handle}/priorities` | `set_priorities` (`{"items": [...]}`) | the person, whoever leads them, or `fleet:operate`; never a seat reordering a colleague |
| `POST` | `/pages` | `write_page` | `knowledge:write` |
| `PUT` | `/pages/{id}` | `save_page`. `If-Match` is its required `base_version` | `knowledge:write`; a `title` in the body is a rename, and asks what a rename asks |
| `POST` | `/pages/{id}/rename` | Move the page to a new title — its address: `{"title": …, "quiet": false}` | the lead of the page's container, or `fleet:operate` |
| `POST` | `/pages/{id}/comments` | `comment_on_page` | `knowledge:write` |
| `PATCH` | `/pages/{id}/comments/{cid}` | Rewrite a remark: `{"body": …}` | its **author** — for the work comment's reason |
| `DELETE` | `/pages/{id}/comments/{cid}` | Take a remark down | its author, or `fleet:operate` as a moderator |
| `DELETE` | `/pages/{id}` | Put the page in the trash | the lead of the page's container, or `fleet:operate` |
| `POST` | `/pages/{id}/restore` | Take it back | the same |
| `POST` | `/pages/{id}/purge` | Destroy it: `?confirm=` repeats its **title**, `?reason=` is required | `fleet:operate`, and **never a seat** |

**A page in the tool-skills container asks one question more.** Creating,
saving, renaming, trashing, restoring or purging one is decided as
`pages.skill.write` on top of the rule in the table — **`config:write`**, and
never a seat — because a tool skill is injected into every seat's turn and
writing one rewrites the prompt the company runs under. A remark on one is an
ordinary comment. See [who may write where](../concepts/knowledge-system.md#who-may-write-where).

`{key}` is an item's key (`ENG-42`) or its id — and its id for an item flagged
`key_collision`, whose key opens the task that claimed it first. A receipt
names the item it is about in `item`, by that same rule, with `key` beside it
and `key_collision: true` where the two differ: `item` is what to put back in
the path. `{id}` is a page's id. A body
naming a different object than the path is refused `400` rather than
overwritten, and a route that is a narrower door onto a wider tool — `/depend`,
`/relate`, `/tags`, a project's policy — refuses an argument that belongs to
another. **Every** route refuses, `400` naming it, an argument its tool does not
take: the tool reads what its schema declares and nothing else, so an
undeclared argument would be DROPPED — and a request answered as though it had
asked for less is not a no-op. A client still sending `save_work_view`'s old
`owner` had a personal view saved as a shared tab on the project, under a
`200`. The refusal is the **tools' own**, made where every call of every tool
passes, so a seat's turn and your assistant over
[`/operator/mcp`](#operatormcp--your-own-assistant) are told exactly what a
route answers here — it used to be this surface's alone, and the same
argument sent through either of the other two was dropped. A body over 1 MiB (twice a page at its own cap, for JSON escaping) is
refused `413` before it is read.

**Not here, and deliberately:** sprints and goals. The tracker has neither —
both left the domain — so there is nothing to route.

### What a request answers

Through the **one mapping** the act route answers with — read from the
refusal's class and cause, never from its sentence — so a status and a code
mean the same thing on every write surface:

| Status | `error` | When | Carries |
|---|---|---|---|
| `200` | | The write was **applied** on this node, so the next read here sees it | the tool's own receipt, its `outcome`, `position` and `op_id` |
| `202` | | The write is **pending**: durable, and not yet applied on this node | the same, and the `position` to read at |
| `400` | `invalid_body` | The body is malformed, names a different object than the path, or carries an argument the tool does not declare | `detail` naming it |
| `401` | `invalid_token` | Nobody presented a credential | |
| `403` | `unauthorized` / `step_up_required` | The authority table refused, or the verb needs a fresher proof | `detail`, worded exactly as a seat and your assistant are told it, beside `reason` and `grants` (or the window) |
| `403` | `forbidden` | The domain forbids this caller the gesture — rewriting somebody else's remark | `detail`: the domain's own sentence |
| `404` | `not_found` | The item, page or comment does not exist — or it was purged | |
| `409` | `stale_version`, `conflict`, `exists`, `reassignment_budget`, `inbox_full`, … | Somebody changed it after you read it — a stale `If-Match`, a title taken, a race lost — or the state the request needs is not there | `detail`: read it again, or change what is in the way |
| `409` | `invalid_input` | The `Idempotency-Key` already names a write to another object | send it under a new key |
| `422` | `invalid` | The domain refused the write on its own rules — a purge whose `?confirm=` names another item among them | `detail`: the domain's own sentence |
| `500` | `internal_error` | The engine failed at something of its own | nothing a retry changes; the detail is in the node's log |
| `503` | `unavailable`, `peer_upgrading`, `no_active_revision`, `identity_unavailable` | This node could not decide (it cannot tell who you are, or cannot read the chart yet), cannot establish whether the write landed, or its log refused the request | a `Retry-After`, and — for an unknown outcome — `"outcome": "unknown"` and the `op_id` to retry with, on every route, a tool-backed one included. A refusal waiting cannot clear on this node — it was evicted, it holds a record it cannot decode, the log is at its ceiling — carries **no** `Retry-After`: ask another node, or an operator |

The `403` wording is the point of the `unauthorized` row: the three surfaces that serve
these verbs refuse in ONE sentence, formed by one function, so a person told one
thing by the dashboard and another by their assistant is never left asking
which is wrong.

### A retry is the same operation

Send an `Idempotency-Key` header and every record the request writes derives
its operation id from it; send none and a fresh key is minted and handed back
as `op_id`. An `unknown` outcome is the one to retry — **with the same key**,
sent back as `Idempotency-Key`, because a fresh one would defeat the ledger
that makes a retry safe and a retried create would file the item twice.

Every operation is bound to what its write SAYS as well as to the key: the
same key sent with another request — a page saved with another body, renamed
to another title, a remark rewritten again — is another operation and lands
as asked, rather than being answered from the ledger as the first one with
nothing of it written. The same request again is the same operation, which is
what makes it a retry.

### A purge

`POST /work/items/{key}/purge?confirm=<KEY>&reason=<why>` destroys an item and
every row it produced, on every node, and nothing undoes it. It replaced
the purge route that stood beside the retention gestures, and it differs in the
three places that one was wrong:

- **`?confirm=` is checked**, against the item the path resolves to — the key a
  person sees on the board, not the id a script carries. A mismatch is `422
  invalid` and destroys nothing.
- **The project is the stored row's.** There is no `?project=`: the path may
  name the item by its id, which names no project, and a purge filed under the
  wrong project blocks writes to a project it is not about.
- **A retry reuses the operation** through `Idempotency-Key`, like every write
  here, rather than an `?op_id=` of its own.

`?reason=` is required: the rows are destroyed and the deletion marker's reason
is the whole account of what used to be at that key. A node that is offline or
evicted keeps its copy until it replays, adopts a snapshot, is replaced or is
destroyed. `crewlet work purge` is the same route from a shell.

### Somebody else's inbox

`mark_inbox` and `set_pins` write the **caller's own** record and take no
handle — a model that could name whose inbox to mark could mark anybody's. So
`PUT /work/people/{handle}/inbox` goes through the same parsing and the same
writer as the tool, and `{handle}` resolves exactly as [a personal
question's](#whose-record-a-personal-question-answers-for) does: either of your
own names is your own record; somebody else's **login** is their record,
looked up in the identity directory — the seat it binds them to, or the login
for somebody bound to none; and any other name must be a seat the chart has
**exactly** — never looked up by resemblance, so a name no record could be kept
under (`Jane Doe`, a seat the chart lacks) is refused rather than guessed at.
The verb is decided **on the record the name resolved to** — yours, or the
admin grant, to unstick a departed person's queue — because deciding on the
spelling admitted an administrator to a record under a bound person's login,
and that is where the write then landed, read by nothing of theirs. A login
nobody holds is `404` to a caller who may write anybody's record and `403` to
everybody else, and a node that cannot read the directory is `503`.

`set_priorities` (`PUT /work/people/{handle}/priorities`) is the one person
verb that DOES take words for somebody else, because a lead sets a report's
queue — and a model types what it remembers. Your own queue, named by omission
or by either of your names, is never looked up; somebody else's **login** is
the identity directory's to resolve, exactly as above, and is **never** a seat
— matched against the chart, `jane.doe` replaced the queue of the seat `jane`
("Jane Doe"); only a name that is neither — a handle, a role, a display name,
an address — is resolved against the chart. The lead relation is then asked
about the seat it all resolved to, so a lead may name their report either way.
A name nothing answers to is refused, and the refusal lists the seats only to a
caller who may read the roster (`state:read`).

## The native tracker and knowledge base

`GET /work`, `GET /pages` and their neighbours read **this node's own
projection** of the company's tracker and knowledge base — the same copy a
seat's tools read, so an operator and an agent looking at one item see one
item.

Three properties are worth stating because each is a decision rather than an
implementation detail.

**They are registered only where the company runs that backend.** A company on
`tracker.backend: jira` has no `work_items` question at all, and asking gets
`unknown_query` / `404`. That is deliberate: an operator who wired Jira and
then found a blank Crewlet board would reasonably conclude their integration
was broken. There is no native record for this node to have a copy of, and an
empty board would claim otherwise.

**A node that has not caught up refuses.** Every one of these answers
`unavailable` (`503`, with a `Retry-After`) while this node is behind the log,
rather than returning an empty list. "This company has no work" is an answer a
person acts on — they file the duplicate, they conclude the migration failed —
so a node that has not applied what the fleet holds must not be able to say it.

The hint is **derived, not fixed**: how far behind this node is over how fast
it is actually draining, so a node grinding through a bulk apply asks for
longer than one that caught up in milliseconds. **Every** refusal by the state
log is `unavailable` — `503` over REST, the `unavailable` frame on the socket —
carrying the refusal's code as `refusal` and its own words as `detail`, because
none of them is a fault: the node understood the question and cannot answer it
*here*. What differs is whether to come back. A `Retry-After` is sent only
where waiting clears the refusal, and it is that refusal's own hint — the
derived one above for `behind`, the broker's election timeout for `no_quorum`
and `broker_unreachable`, one position heartbeat for `below_floor` and
`floor_unknown`. A refusal waiting cannot clear — a record this node cannot
decode (`deferred`), a log at its byte ceiling (`log_full`), a barrier its
broker refused (`broker_refused`), an evicted node — carries **none**, so a
client asks another node or waits for an operator rather than polling this one;
see [the thirteen refusals](../guides/consistency.md#the-thirteen-refusals) for
what each one asks of whom. A `min_position` on another domain's log is not a
refusal at all: it is the request's mistake, and it is `400 bad_params`.

**The reads are here and the writes are tools.** Every write is attributed to
somebody — a seat through its own tools, a person at the dashboard through
[`/operator/act`](#operatoract--the-dashboards-write-surface), a script through
[the write surface](#the-human-write-surface), an assistant through the
[MCP surface](#operatormcp--your-own-assistant) — the same tools every time,
and never to "the dashboard", which is not a person and not a seat and cannot
be asked why. The routes under `/work/retention/` below are not about items:
they are operator gestures against the log's own history, attributed to the
credential that made them.

### Paging and filters

Both listings take `limit` (default 50, max 500) and page by an opaque
CURSOR, never an offset: both are read while seats write, and an offset over a
set a create or a rename just moved skips one row and repeats another with
nothing to say it did. The work listing takes `cursor` and answers
`next_cursor`; the pages listing takes `after` and answers `after`, beside its
`total`. (The pages listing took an `offset` on the argument that a title
order is an arrangement nobody moves; a page created ahead of a reader's place
moves every row after it, which is exactly what a tree's "Load more" noticed.)

Multi-valued filters are **comma-separated** — `?status=todo,in_progress` —
because the socket's query channel carries a JSON object, which cannot express
a repeated key. A filter only one transport could send is exactly the
divergence the shared answer function exists to prevent.

Openness is **not** a boolean on the work listing. The four status *groups*
are what every rule in the tracker is written at, so open work is
`?status_group=not_started,active` — and `show_closed` is the separate
three-stated question of whether finished work belongs in an answer at all:
absent or `false` leaves it out, `true` puts it in, and `recent:<dur>` puts in
only what finished inside that window. A single `open=false` could not express
the third.

The pages listing's `skills` is **three-stated** for the reason a boolean
would lose:

| Filter | Absent | `true` | `false` |
|---|---|---|---|
| `skills` (pages) | every page | only tool-skill pages | everything but them |

An unknown enum value is refused naming the closed set — `?status=finished`
answers `400` saying which statuses exist — rather than matching nothing. A
listing that answered empty for a typo would send somebody looking for items
that were never missing.

### `GET /work/retention` — what the log is holding

**Takes `fleet:operate`, reads included**, on the same rule `/config` and `/secrets`
follow: the answer names every node in the fleet, its position, its disk and
its snapshot repository, which is a map of which machine to take out to lose
the company's history, and it takes its own grant on top of authentication.

The document is assembled by the node you ask, and says so: half its fields are
facts only that node can state — its own applier's lag, its snapshot, its disk
— and half are fleet-wide, read from coordination. `node_id` is on the document
rather than beside it, because a report pasted into a ticket without its author
is three per-node facts attributed to a fleet.

```json
{
  "node_id": "node-1",
  "at": "2031-04-02T03:14:00Z",
  "backup_owner": "platform-oncall",
  "register_readable": true,
  "domains": [
    {
      "domain": "tracker",
      "stream": "CREWLET_TRACKER_LOG",
      "generation": 0,
      "first_seq": 918100000,
      "last_seq": 918280001,
      "bytes": 67108864,
      "max_bytes": 4294967296,
      "reserve_bytes": 268435456,
      "headroom_fraction": 0.983,
      "bytes_per_day": 23068672,
      "trim_floor": 918100000,
      "trim_to": 0,
      "blocked_by": "backup_floor",
      "blocked_since": "2031-03-30T02:00:00Z",
      "prose": "Nothing is being trimmed on tracker: ...",
      "terms": [{"name": "applied", "state": "ok", "seq": 918279004, "detail": "..."}]
    }
  ],
  "nodes": [...],
  "snapshots": [...],
  "replica": {"store_bytes": 10415140864, "projected_join_seconds": 308,
              "rejoin_window_seconds": 1800},
  "alarms": [{"kind": "backup_age", "detail": "...", "remedy": "..."}]
}
```

`trim_floor` and `trim_to` are two different numbers, and a blocked domain is
where they part. `trim_floor` is the floor the fleet has published: everything
below it may already have been deleted, it is written before the delete it
licenses, and it never moves down within a generation — so a blocked domain
keeps the floor its last advance reached. `trim_to` is what the last tick
itself concluded: zero while blocked, and below the floor whenever the lowest
counted node is. The first is what a node must hold to replay; the second says
whether the trim is moving. Both, with `terms`, `blocked_by` and
`blocked_since`, come from the row the trim published at the domain's OWN
`generation` only: just after a reanchor that row is about the stream the
domain left. **`trim_floor_state`** says which it is — `published` (the floor,
the conclusion, the terms and the blocking term are that tick's),
`none_at_generation` (the trim has concluded nothing about this generation yet:
a fresh fleet before its first tick, or any fleet just after a reanchor, until
the trim's first tick on the adopted stream) or `unreadable` (the floor
register could not be read). Only `published` makes `trim_floor` and `trim_to`
a number worth reading; `terms` is always a list, **empty** rather than null
where nothing is concluded. See [Retention](../guides/retention.md#the-trim-floor).

**`not_ready`** is present when the answering node refuses every read of the
domain right now — the same refusal its readiness probe reads — with `code`
(`wrong_stream`, `stalled`, `below_floor`, `behind`, `floor_unknown`,
`evicted`, or `broker_unreachable` where its health could not be read),
`causes` naming each identity finding behind a `wrong_stream` (`recreated`,
`ahead_of_log`, `log_diverged`, `generation_passed`) and `detail`, the sentence
the refusal carries everywhere else. **`writes_refused`** is present while the
node serves the domain's reads and refuses its writes: `code` `log_truncated`,
with `detail` naming the peer whose rows hold records the log lost. Both are
the answering node's own facts, like the replica block.

Each node's per-domain row carries **`generation_state`** — `current`, `left`
(a generation the log has since left), `ahead` (one above the answering node's)
or `unknown` (a domain the answering node does not run) — and `lag` only where
it is `current`, because a sequence from another generation is a number in
another space and the difference is not a distance. It also carries the node's
own **`log_diverged`**, and **`stream_created_at`** and
**`checkpoint_stored_at`** — the stream its rows are keyed to and the record its
checkpoint stands on — where the node published them.

A term's `state` is one of four, and `seq` is an answer in exactly one of them:
`ok` carries the sequence the term permits, `unknown` is a term that could not
be evaluated (which blocks), `n/a` is one this domain does not have, and
`unbounded` is one that was read and binds nothing — no hold pins the log, or a
solo fleet takes no snapshots. `seq` is `0` in the other three rather than
absent, so a term permitting nothing *yet* — the state that holds a young
fleet's trim, and the one worth reading — is distinguishable from a term with
no sequence to give.

`register_readable` is the field that keeps an empty `nodes` block honest: "this
fleet has no nodes" cannot happen, and "coordination could not be listed"
happens during exactly the outage somebody is running this in. Without the flag
a renderer prints the impossible one.

**`evictions_unreadable`** is `true` on an identity-claiming log's row
when the answering node could not read that log's evictions as it assembled
the report — a failed store read, or its replicated estate closed for a
snapshot adoption's rename or a shutdown — and absent otherwise. It is the
same kind of honesty for the node block's `evicted`: an unread log contributes
no tombstone, so every node reads as **not** evicted there and stays counted
(the conservative side, which is the trim's own), and "not evicted" is then not
an answer. Read it before concluding a node was readmitted; both `crewlet
retention status` and **Settings › Backups & retention** say so above the node
block, and that screen keeps an eviction it just made on the row until a report
whose evictions were read.

`headroom_fraction` is a **pointer** and is absent when the broker could not be
asked. A fraction of an unknown ceiling is not zero headroom, and zero is what
the one alarm an operator cannot ignore fires on. It is a fraction of the
ceiling **ordinary writes** are held to: `max_bytes` less `reserve_bytes`,
which on every identity-claiming log is the top of the ceiling kept for the
records that install or lift a gate — the larger of a sixteenth of it and
seven of the log's largest records with a mebibyte beside them — so that an eviction still lands on a log full for
everything else ([the gate reserve](../guides/retention.md#the-gate-reserve)).
`reserve_bytes` is absent on a log that keeps none — the vector changelog —
where the headroom is of the whole ceiling.

`bytes_per_day` is what the log took in over the trailing day, as this node's
last trim tick measured it from the log's own records — the rate the
`log_ceiling_short` alarm holds `max_bytes` against `min_age` of. It is a
pointer too, for the opposite reason: `0` is a log that took in nothing, the
most benign reading there is, and the field is **absent** where nothing was
measured — a compacted log (whose size follows its subjects, not its age), a
log in its first two days (whose trailing day would contain the day it was
imported into), and every log on a node whose trim has not ticked yet.
See [Retention](../guides/retention.md#the-one-number-to-watch).

`crewlet retention status` renders exactly these bytes.

### The three retention gestures that write

All three are **POSTs**, and all three — like every other `/work/retention*`
route — take `fleet:operate`: moving the floor the trim deletes against,
stopping a machine writing and letting it write again are the deployment's own
controls, and a caller without the grant is refused `403` naming it.

| Route | What it does |
|---|---|
| `POST /work/retention/ack?stream=NAME&position=N` | Publishes an operator backup floor. Refused `400` naming both when either is missing, and `404` when the stream is not one this node runs, which on a node running no state log is every stream. The point is stamped with **that stream's own generation**, read from the running log: a bare sequence at another log's generation names a number space the copy does not cover. |
| `POST /work/retention/evict/{node}?confirm={node}` | Installs the eviction gate on every identity-claiming log — the tracker's, the knowledge base's, the org chart's and the identity estate's; the vector log counts no node and gets none. **Refused `409 eviction_refused`**, with nothing written to any log, while the node still holds a live presence lease: it is still reaching the fleet, and an eviction would drop everything it writes. The body carries `detail`, `hint`, `op_id` and `actions` — `["wait", "force"]`: stop the node and let its lease lapse, or force it. `force=true` overrides that refusal for a node wedged in a way that still renews its lease. A node that cannot read the presence leases at all answers **`503 eviction_unjudged`** — a judgement nobody could make is not one that came back clear — with a `hint` and `actions` `["retry_same_op", "force"]`; `force=true` takes the eviction past that too, since the leases are the judgement's only input, and the node logs `retention_eviction_forced_unjudged`. |
| `POST /work/retention/readmit/{node}?confirm={node}` | The inverse commit, on every one of those logs. **Refused `409 readmission_refused`**, with nothing written to any log, when in any one of those logs the node has not applied every record up to the one just before the higher of that log's published floor (at the log's current generation) and its first surviving sequence — its last published position is more than one below that bound — or its last published position is from a generation the log has since left. The body carries the sentence (`detail`), what to do (`hint`, and `actions` `["wait"]`), and the numbers: `domain`, `position`, `generation`, `floor`, `first_seq`, `floor_generation`, and `published` — false for a node that has never published a position and is judged as holding nothing. A position that could not be compared at all — the register or the floor unreadable, or this node behind a reanchor the fleet has made — is `500 gate_failed`, and refuses too. `force` has no effect here and nothing overrides this refusal: the floor is a fact about what the node holds, not a lease it might be wedged into renewing. |

`confirm` echoes the node id, and a mismatch is `400`. A node id no node could
run under — the [`node.id`](../concepts/configuration.md#nodeid) rule, since the
id becomes a subject token on every log — is `400 invalid_gate`, with nothing
judged or written.

The gate is written **as the caller**: the eviction's `by` — what `crewlet
retention status` prints as "evicted by" — is their name as [a write is
attributed](#who-a-write-is-attributed-to), and the record carries the
credential they pressed it through beside it. It used to be written as the
serving node's own writer, so "who stopped this machine writing" had one
answer, whichever node the request happened to reach. A capacity operation
names its opener the same way, `by` beside `operator_id`, on the operation and
in the report's `maintenance` block.

**The gesture is judged once, before any log is written**, and then its record
goes to each identity-claiming log in turn. The trim counts nodes per log, so a
record on one log lifts that log's pin and no other. A `200` answers **per
log**:

```json
{
  "node": "node-4",
  "evicted": true,
  "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4",
  "complete": false,
  "domains": [
    {"domain": "tracker", "stream": "CREWLET_TRACKER_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.tracker:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_TRACKER_LOG", "generation": 1, "seq": 918280002}},
    {"domain": "pages", "stream": "CREWLET_PAGES_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.pages:node-4",
     "outcome": "unknown",
     "actions": ["retry_same_op"],
     "hint": "its outcome is unknown: the same gesture under the same operation id answers from this log's own ledger if the record landed, and writes it if it did not"},
    {"domain": "chart", "stream": "CREWLET_CHART_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.chart:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_CHART_LOG", "generation": 0, "seq": 4127}},
    {"domain": "iam", "stream": "CREWLET_IAM_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.iam:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_IAM_LOG", "generation": 0, "seq": 58311}}
  ]
}
```

Each entry is one log's own answer. `outcome` is the write's
[three-valued outcome](../guides/replication.md#a-write-has-three-outcomes) —
`applied`, `pending` or `unknown` — with the `position` the record holds, absent
for `unknown`, which has none: a gate the caller believes has landed and which is only
`pending` is the difference between a node that has stopped writing and one
that is about to. An entry with `error` in place of an `outcome` was **not
written**, and `reason` names why in the vocabulary every write refusal uses
(`log_full`, `evicted`, …). A log that answered holds its record whatever the
others did.

An `unknown` entry may also carry **`"unvouched": true`**: this node's
operation ledger may have lost the row the operation needs — it was minted
before the node adopted a peer's snapshot, or before the ledger's own sweep
reached it — so the node published nothing and cannot tell whether the record
landed, and the same request through it answers the same way every time. Such
an entry offers `other_node` rather than `retry_same_op`: send the same request,
with the same `op_id`, through a node whose ledger reaches back that far.

A log the gesture did not finish also carries **`actions`** — what to do, in
order — and **`hint`**, the sentence saying why. Both are absent on a log that
holds its record. The actions are a closed set a client switches on, and
`hint` names no client's controls: `crewlet retention evict` renders an action
as its own flags and the dashboard's evict dialog as its own buttons, so a
sentence spelling `-force` is never shown beside a screen that has no such
flag. Every refusal answer above carries `actions` beside its `hint` too,
and the `op_id` the request was sent under — so `retry_same_op` on a refusal
(`503 eviction_unjudged`) is the same request with that `op_id`, read off the
answer like any other. Nothing was written under it, so sending it again with
a fresh one is equally safe.

| Action | What the operator does | Where the gate answers it |
|---|---|---|
| `retry_same_op` | Send the same request again with the answer's `op_id` | An `unknown` outcome (unless it is `unvouched`), a lost race, a failure before the write answered, and a refusal that clears on its own (`behind`, `deferred`, `floor_unknown`, `below_floor`, `eviction_unknown`); `503 eviction_unjudged` |
| `new_gesture` | Start a new gesture, without `op_id` | `superseded` — the operation's record landed and a later gate record on the same node has undone it since (an eviction retried after a readmission) — and `op_reused`, an operation id that already names a record on another object |
| `force` | Send the eviction again with `force=true` | `409 eviction_refused`, `503 eviction_unjudged` |
| `other_node` | Send it, with the same `op_id`, through another node the fleet still counts | `evicted` (this node is evicted itself), an `unvouched` unknown (this node's ledger cannot say whether the record landed), and beside `retry_same_op` on `deferred` and `below_floor` |
| `reanchor` | [Re-anchor the log](../guides/retention.md#re-anchoring-a-recreated-or-restored-log) first, then send the same request with the same `op_id` | `wrong_stream` |
| `set_capacity` | [Raise the log's ceiling](../guides/retention.md#changing-a-logs-ceiling), then send the same request with the same `op_id` | `log_full` — a gate record is admitted into the log's [gate reserve](../guides/retention.md#the-gate-reserve), so this is a log full to its broker ceiling past even that |
| `wait` | Wait for what `hint` names to clear on its own, then run it again | `409 eviction_refused` (the lease to lapse), `409 readmission_refused` (the node to catch up) |
| `restore` | Restore the store and the stream from one backup | `skew` |

A log with a `hint` and **no** `actions` is one no gesture clears — a record
larger than its log's declared largest or the broker's `max_payload`, a
refusal the broker named, or a refusal reason this build has no word for — and
the hint carries the refusal's own detail, which says what does: the limit
that refused a record and what moves it, or the broker's words. Only `retry_same_op` makes the same
request, sent again **now**, the remedy. But four actions — `retry_same_op`,
`other_node`, `reanchor` and `set_capacity` — keep the gesture's own `op_id` as
how it is finished once the operator has acted: a gesture sent afresh after
raising a ceiling is a second one, which writes every log that already holds
the first one's record again, re-dates that eviction, restarts its fence
window and turns the first `op_id` into `superseded`. Only `new_gesture` ends
the operation for good.

`complete` is true only when every log answered `applied` or `pending`. When it
is false and a log offers `retry_same_op`, send **the same request again with `op_id`**
set to the operation id the answer carried. Each log's record is published
under an id derived from it — the entry's own `op_id`, which carries the
gesture's sign, the log and the node — and each log's snapshot reads that id's
ledger row before anything is decided: a log whose record is already the gate
in force answers at the position it has and is not written twice, and only a
missing log is written — through a node whose applier had not reached the
first record yet as well, and however long after the first request the retry
comes, since the broker's two-minute duplicate window plays no part in it.
Without `op_id` the route mints a fresh one, which is a second gesture rather
than this one finished; an id carried from an eviction to the readmission after
it is a different operation on every log, and one carried to another node is
that node's own operation.

An `op_id` is held to one rule, and anything else
is `400 op_id_invalid` with nothing judged or written: an id **in the engine's
grammar** — a UUIDv7 whose leading 48 bits are the Unix millisecond it was
minted at, optionally followed by `.` and a name — of **at most 128 bytes** of
**visible ASCII with no space**. A client may mint its own, and both of the
engine's own clients do, before the first request: `crewlet retention evict`
on the workstation's clock and the dashboard's evict dialog on the browser's.
The mint instant is read by the state log to decide whether its ledger can
vouch for a retry, so a client clock far ahead of the fleet's is the one case
this trusts a caller with — the same one it trusts the engine's own nodes with.
An id with no instant carries nothing to read, so no node could tell whether it
already ran. The rest of the rule is the broker's: only the id's leading uuid
is minted and the rest is free text, while the whole id travels as the
broker's message-id header, which trims its ends and turns a line break into a
space — so such an id would be deduplicated at the broker as a different id
from the one every log's ledger answers for. It is refused rather than
cleaned, because a retry has to send back the id it holds, byte for byte; every
id an answer carries already fits.

**The gesture does not stop when its caller does.** The judgement runs under
the request, so a request abandoned before it wrote nothing; once the first
record is about to be written the node finishes the gesture under its own
budget, so a dropped connection or a client timeout never leaves a node
evicted on one log and counted on another. A caller that sends its own
`op_id` — `crewlet retention evict` and the dashboard both do — can ask again
with it and read every log's answer; one that let the route mint it has lost
the id with the answer that never arrived.

Every node running the state log serves both routes, whichever backends the
company uses: a company on an external tracker still runs all four logs. A
node with no log to write a gate to answers `503` rather than `404` — the
routes exist on this build, and telling an operator they do not sends them
looking for a version mismatch that is not there.

### The capacity window

A log's Tier A ceiling is only the value its stream is created with, and these
routes are the window in which a running log's ceiling changes. The
[procedure is documented once](../guides/retention.md#changing-a-logs-ceiling);
what follows is the wire surface. Every route here, the status read included,
takes `fleet:operate`.

| Route | What it does |
|---|---|
| `POST /work/retention/capacity?stream=NAME&bytes=N&confirm=N` | Opens or resumes the operation and drives it as far as this node's mode allows. `confirm` repeats `bytes` and a mismatch is `400`: the target is chosen once for the life of an operation. `assert_excluded=true` is required only on an external broker. |
| `GET /work/retention/maintenance?stream=NAME` | The operation, every acknowledgement, every admission, and — computed here rather than by each client — whether the seal holds, what is blocking it, and which admissions block activation. |
| `POST /work/retention/maintenance/abandon?stream=NAME` | From `opened` clears the operation outright; from anywhere else enters the seal, and the operation records who abandoned it as `abandoned_by`. |
| `POST /work/retention/maintenance/exclude?stream=NAME&node=ID&confirm=ID` | Waives one participant's acknowledgement and withdraws its admission, and the operation records who asserted it under `excluded_by`, keyed by the node. |

**Every gesture on the window names who made it**: `by` and `operator_id` for
whoever opened it, and the same pair under `abandoned_by` and, per node, under
`excluded_by` — the author and the credential they acted through, as every
other trail records a write. An exclusion is the one fact the seal takes on
somebody's word, and whose word it was used to be said only in the log of the
node that served the request. An abandonment from `opened` clears the record
it would have been written on, so only that log names who made it; an entry
missing from a window an older build wrote is an operator nobody recorded.

**A node in `normal` mode refuses the write routes**, naming the restart: the
usage a resize is decided against has to be a quantity nothing can move. The
three-mode boot is `crewlet run -mode`; see
[the CLI reference](cli.md#crewlet-run).

The `sealed` / `blocking` / `admissions_blocking` fields are **computed
server-side**, from the same predicate the coordinator itself runs. A client
that re-derived them would be a second opinion about one barrier, and the two
would drift.

### Re-anchoring

| Route | What it does |
|---|---|
| `GET /work/retention/reanchor?stream=NAME` | The LIVE stream's own `created_at`, read from the broker on the call — the instant a `wrong_stream` refusal names — the generation that stream's domain stands at, and what a reanchor run now would do: `case` — `recreated` (another stream than the rows are keyed to, followed from its first surviving record), `restored` (the same stream brought back from an older copy, ending below the checkpoint or holding another record at it since, followed from its end — one below the node's own generation record where an earlier attempt already appended it) or `abandoned` (the same stream, continuing in a generation only an evicted peer held, followed from this node's own checkpoint with that generation's records void) — with `cursor`, the sequence the new checkpoint would sit at. With nothing to re-anchor there is no `case`, and `nothing_to_reanchor` says why. A restored log holding records this node's rows do not hold, written after the restore, also answers `discards` (the newest of them: `seq`, `kind`, `subject`, `writer`, `op_id`, `stored_at`, and `ledger_lost_before` when its operation is older than the instant this node's operation ledger may have lost rows from — the rows may hold it after all) and `discarding` (why a reanchor refuses without `discard=true`). `404 unknown_stream` for a stream this node does not run; `503 stream_unreadable` when the broker did not answer the read, which is worth retrying. |
| `POST /work/retention/reanchor?stream=NAME&confirm=<created_at>[&force=true][&discard=true]` | Runs that ONE domain's generation transition, answering with the new `generation`, the `case` it answered, the `cursor` its checkpoint went to (for a restored log, one below the generation record the transition appended; once that record is appended the transition finishes on its own time, so a client that disconnects does not stop it halfway) and, when it discarded records written after a restore, the newest as `discarded`. No other domain's checkpoint moves, and the domain's applier resumes with no restart. |

`confirm` is the value the `GET` returns, supplied by the caller: the
confirmation means *I looked at the thing I am re-anchoring*, so the two are
deliberately separate round trips rather than one route that reads and acts.
It is compared as an instant at microsecond precision, so the RFC 3339 value the
`GET` answers is accepted as it came. `force=true` overrides the rule that only
the most caught-up node may re-anchor, for a fleet whose register cannot say;
it never overrides a peer that has already re-anchored the stream, whose rows
are the fleet's history in the new generation and which the refusal names.
`discard=true` accepts that a restored log's records this node's rows do not
hold — written after the restore, and named by the `GET` as `discards` — are
applied on no node; without it that reanchor is refused `409
reanchor_refused`, and with it the answer carries the newest one as
`discarded`. The two flags are separate because neither answers the other's
question.

## Agent Memory

### `GET /agents/{id}/memory`

What one seat has learned, in one round trip — **answered by the node holding
the seat**. Also served as the `agent_memory` query, which takes `{id, limit}`.

`{id}` is the seat's **handle** — the canonical identifier everywhere in
the system, and any handle the seat answers to, a retired one included — or
a person's **login**, which names the seat the identity directory binds them
to and is never read as a handle. It is a seat's TRAIL, so it takes
`audit:read` whoever's seat it is, and an absent name is the caller's own
seat. The halves are keyed differently in the store — the diary and the
onboarding marker by the derived agent id, the episodes, skills and
counterparty profiles by the handle the seat was **created** under — and the
answer resolves both itself rather than making a caller know which, so a
renamed seat's page shows what it learned before the rename. Every row names
the seat, and a colleague in a profile's `subject`, by the handle they answer
to **now**.

```json
{
  "id": "<handle>",
  "diary": [
    { "id", "content", "retention", "source", "turn_id",
      "created_at", "ttl_until", "retrievals" }
  ],
  "diary_total": 142,
  "episodes": [
    { "id", "turn_id", "agent_handle", "task_summary", "plan_summary",
      "review_outcome", "tool_sequence", "skills_used",
      "conversation_key", "work_key", "created_at", "ended_at",
      "duration_ms", "compacted", "count" }
  ],
  "episodes_total": 38,
  "skills": [
    { "id", "key", "title", "summary", "version", "updated_at", "uses" }
  ],
  "skills_total": 4,
  "counterparties": [
    { "subject": { "handle" | "external_id" + "platform", "name" },
      "resolved", "traits", "interactions",
      "first_seen_at", "last_updated_at", "last_corroborated_at" }
  ],
  "counterparties_total": 11,
  "latest_reflection": { "id", "content", "…": "a diary row" },
  "onboarded_at": "2026-09-01T08:02:11Z",
  "held_by": "node-2"
}
```

**Who answers.** A seat's memory is written to the store of the node running
it and carried to every other node on a compacted changelog, so every node that
ever held a seat keeps a copy and only the holder keeps it CURRENT. The node
serving the request reads the seat's lease and:

| The lease names | The answer |
|---|---|
| nobody | empty, with `held_by: "none"` — the copies on disk are of unknown age, and none is shown as the seat's memory |
| this node's own incarnation | read here, once the seat is attached; while it is still arriving (hydrating before its mailbox attaches) the read is `unavailable` rather than short |
| a peer | asked of that incarnation on an ephemeral scatter (`crewlet.held.read`), with a 2 s budget; silence is `unavailable` naming the node, never an empty memory. A peer whose build does not advertise the `held_read` [feature](../concepts/coordination.md#what-a-node-says-about-itself) is not asked at all: the read is `unavailable` at once and says the holder runs an older build, rather than waiting out the budget on every poll of a rolling upgrade |

`held_by` is the node that answered. A node with no broker is the whole fleet
and answers every read from its own store. `unavailable` is a `503` with a
`Retry-After` on REST and the socket's `unavailable` code with
`retry_after` — a moment's wait, not a fault.

**Every collection is a page with its total beside it.** `limit` (1–50, default
50) pages all four; `diary_total`, `episodes_total`, `skills_total` and
`counterparties_total` are COUNTED in the store over the seat's whole set, so a
screen renders the total rather than the length of a page that was cut.
`latest_reflection` is the newest live diary entry whatever the page — a
profile's summary asks for `limit=1` and reads the totals and this. It is null
when the diary holds none.

**Every key is present on every answer**, as an empty list or a zero rather
than an absent one: a caller cannot tell "this seat has learned nothing" from
"this answer does not carry that half" if the key is simply not there.

### `memory_overview`

Every agent seat's memory at a glance — the list **Knowledge › Agent diaries**
draws. A socket query with no parameters (there is no REST route: it is a
screen's list, and `GET /agents/{id}/memory` is the one seat's record).

```json
{
  "seats": [
    { "handle": "swe", "diary_total": 142, "episodes_total": 38,
      "skills_total": 4, "last_reflection_at": "2026-09-28T16:02:11Z",
      "latest_reflection": { "id", "content", "…": "a diary row" },
      "held_by": "node-2", "unavailable": "" }
  ],
  "coverage": { "nodes": [{ "id": "node-1", "answered": true, "error": "" }],
                "complete": true }
}
```

**Every agent seat in the chart, in handle order, and no cap** — a person keeps
no memory the engine writes and is not listed. Each row is counted by the node
HOLDING the seat, under exactly the rules of `agent_memory` above, but gathered
in ONE round: the serving node lists every seat lease once, groups the seats by
the incarnation holding them, reads its own from its store and puts ONE request
on `crewlet.held.read` naming each holder's seats; every holder answers for its
own in one reply, inside the same 2 s budget. So a row is one of three things:

| Row | Meaning |
|---|---|
| `held_by` a node, `unavailable` empty | counted by that node — the totals are the ones `agent_memory` carries |
| `held_by: "none"` | no node holds the seat; nothing is counted, because no copy anywhere is current |
| `held_by` a node, `unavailable` set | that node did not answer, runs a build that cannot, or is still taking the seat — the reason is here and the zeros beside it are not a count |

`coverage` is the shape every fleet answer carries: this node and every holder
that was asked, each `answered` or with its `error`, and `complete` only when
all of them answered. A lease table that cannot be read fails the whole answer
as `unavailable`, since without it every row would be a guess at who holds
what.

The rows are **projected** by `internal/learning/memread` rather than being
the learning package's own structs marshalled directly: those are domain types
whose fields exist for the recall path, they carry no `json` tags, and
marshalling them shipped Go field names plus every row's raw embedding vector
to a screen with no use for one.

Sources:

* **`diary`** — the seat's private observation log, written by
  `reflect_and_persist` and the reflection pass, live entries only, newest
  first. `retention` is `diary_long` or `diary_short`; a short entry carries the
  `ttl_until` it lapses at. `retrievals` is how often it has actually been
  recalled, which is the difference between a memory that keeps proving useful
  and one written once and never read.
* **`episodes`** — one row per completed turn (or per compacted cluster),
  newest first. `duration_ms` is milliseconds: a Go `time.Duration` marshals as
  an integer count of NANOSECONDS, which renders as a plausible and wildly wrong
  number.
* **`skills`** — the seat's own synthesized skills, drafted from its repeated
  work and loadable mid-turn via `use_skill`. Archived rows are hidden and stale
  ones shown, because a stale skill still works and still revives on use.
* **`counterparties`** — what the seat learned about the people it works with,
  most recently updated first. Both instants are carried and they measure
  different cadences: `last_updated_at` moves on every interaction and
  `last_corroborated_at` only when the traits changed. `traits` is a bag whose
  keys the model invents. `subject` carries a `handle` for a seat of this
  company and an `external_id` with its `platform` for anybody else.
* **`onboarded_at`** — when the seat first finished onboarding, or `""`; a pass
  claimed and never finished is not an onboarding.

A store that cannot be read fails the read rather than answering an empty
section: "this seat remembers nothing" and "the store did not answer" are
opposite facts. The table is strictly per-agent; cross-agent procedural
artefacts are [promoted](../concepts/agent-learning.md) as draft pages in the
shared knowledge backend, reachable by all members via query-time search.

---

## Fleet, Sandbox Runs & Schedules

### `GET /credential-pool`

Backs **Settings › Models & keys**: every model the company configures, in
config order (the order a seat that names no model falls back through), the
keys each rotates through and which of them is benched. **It takes
`config:read`, reads included** — which variable holds each model's key and
when each is refused is a map of which credential to take.

**Names, never values.** A key is `ref`, the variable a whole `${VAR}` names
(or the vendor's conventional variable, `source: "default"`, for a model that
names no `api_keys`), or its position alone for a value written into the
document (`source: "inline"`, `ref: ""`). `hint` is the 12-character,
non-reversible identifier the engine's `credential_cooled` log lines carry.

**The pool and the fleet's ledger, the later deadline winning.** A bench is
published to the fleet when a node takes it and pulled by every other node
every 15 s; this answer reads the ledger directly, so a key a peer benched a
second ago is `cooling` here before the answering node has pulled it, and a
bench whose publish failed still reads `cooling` on the node that took it. A
ledger that cannot be read does not fail the answer: `fleet` is `false`,
`fleet_error` says why, and every deadline is the answering node's own.
`uses` and `in_flight` are the answering node's leases of the key since it
applied its configuration, and `unresolved` is what ITS environment and the
company's secrets resolve.

| key `state` | Means |
|---|---|
| `ready` | A call can lease it now |
| `cooling` | Benched after a rate-limit or auth refusal until `cooling_until` |
| `unresolved` | It resolved to nothing on the answering node and is not in the pool |
| `duplicate` | The same value as the key at `same_as` (1-based), held once |

| model `state` | Means |
|---|---|
| `ready` | Every key resolves and none is cooling |
| `degraded` | Some keys can be leased and some cannot |
| `exhausted` | Every key that resolves is cooling: each call falls through to the seat's next model |
| `no_key` | No key resolves: every call is refused as unauthorised |
| `login` | A `cli-agent` entry — one login held by the CLI, no key bag |

```json
{
  "node": "node-1",
  "fleet": true,
  "fleet_error": "",
  "providers": [
    {"key": "smart", "type": "anthropic", "model": "claude-sonnet-5", "state": "degraded",
     "ready": 1, "rate_limit_seconds": 3600, "auth_seconds": 300,
     "keys": [
       {"ref": "ANTHROPIC_KEY_A", "source": "reference", "hint": "3f9a1c0b7e2d", "state": "ready",
        "cooling_until": null, "same_as": 0, "uses": 12, "in_flight": 1},
       {"ref": "ANTHROPIC_KEY_B", "source": "reference", "hint": "8c41d2e9a0f7", "state": "cooling",
        "cooling_until": "2026-09-29T12:40:00Z", "same_as": 0, "uses": 4, "in_flight": 0}
     ]}
  ]
}
```

To change a model's keys, `PUT /config/llm-providers/{id}` — see
[Per-entity read and write](#per-entity-read-and-write).

### `GET /mcp-servers`

Backs the **Servers** section of **Settings › Tools & MCP**: every MCP server,
what the configuration declares for it and what each live node did with it.
**It takes `config:read`**, as `GET /config` does — it names the nodes, the
launch commands and the first line of each failure, which is what the
configuration it reports on carries.

**Off the heartbeats, not a fan-out.** Each node re-publishes what its MCP
starts concluded on its presence lease, one row per server with its instances
counted (a per-seat template has one instance per seat that node holds), so
one read of the lease table is every node's answer at once. A node running a
build older than that report is `reported: false` and its cells are UNKNOWN —
never a row of zeros, which would read as "started nothing".

`servers` lists every server this node's active configuration declares, then
any a node reports that the configuration does not carry (`configured: false`
— a node still on an older revision mid-rollout). The launch is the parts
that are not credentials — `transport`, `command`, `args`, `url`; `env` and
`headers` are never here. `started` and `failed` are summed over the nodes,
`tools` is the most one started instance serves, and `state` is decided here
so no screen re-derives it:

| `state` | Means |
|---|---|
| `running` | Every instance any node launched started and listed its tools |
| `partial` | Some started and some did not — a node's environment or one seat's credentials rather than the server |
| `failing` | Instances were launched and none started — the server, its command or address, or credentials every seat shares |
| `not_started` | Every reporting node started nothing for it — a per-seat template no seat on a live node declares credentials for |
| `unreported` | No live node publishes the report at all |

`error` is one failed instance's reason, cut to 240 bytes on the heartbeat,
and `error_seat` the seat it was launched for; the whole text is the node's
`mcp_server_failed` log line.

```json
{
  "nodes": [{"id": "node-1", "reported": true}, {"id": "node-2", "reported": true}],
  "servers": [
    {"name": "github", "configured": true, "shared": false, "transport": "stdio",
     "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"], "url": "",
     "state": "partial", "started": 3, "failed": 1, "tools": 26,
     "nodes": [
       {"node": "node-1", "reported": true, "started": 2, "failed": 0, "tools": 26, "error": "", "error_seat": ""},
       {"node": "node-2", "reported": true, "started": 1, "failed": 1, "tools": 26,
        "error": "401 Bad credentials", "error_seat": "backend-dev"}
     ]}
  ]
}
```

To add a server, `PUT /config/mcp-servers/{name}` with `If-None-Match: *` —
see [Per-entity read and write](#per-entity-read-and-write).

### `GET /fleet`

Backs the dashboard's **Settings › Nodes** screen — the questions `/health` cannot
answer, because it answers about the node that served it and a load
balancer sends the next refresh somewhere else.

Read from the lease table, so every node gives the same answer: node
presence carries each node's `node.roles` and `node.labels`, seat and
worker leases name their holder, and the per-node config epoch comes from
the control plane's apply status.

**It needs a credential carrying `fleet:operate`, reads included** — a
signed-in session or a bearer — like every other answer the dashboard's
Settings › Nodes screen draws. What it describes is the DEPLOYMENT
rather than the company's work — the node ids, which node holds which
seat, the lease epochs, how far a rollout has reached — so it is scoped
the way `/integrations` beside it always has been, and
Authentication alone does not open it: it takes its own grant.

Presence also carries what each node is **doing** — `in_flight`,
`draining`, `posture` and `started_at` — because only the node running a
seat knows those, and `/health` answers about whichever node served the
request. They ride on the heartbeat that already re-sends roles and labels
on every beat, rather than over a request/reply to the owning node: every
answer would then be partial, it opens a new trust edge, and it duplicates
the mechanism the lease table already is.


**Absent is not zero.** A node that publishes no status (one running a build
older than the field) omits those fields entirely, and the dashboard draws an
em dash. A confident `0` would render an idle row for a process that is
simply not saying.

Two fields report the failures that are otherwise invisible, because
their only symptom is an absence: `unmanned_roles` lists roles no live
node performs, and `unplaceable` lists seats whose `role.placement`
matches no live node. A lease table that could not be read answers `503`
with a `Retry-After` rather than an empty fleet: "no node is live" is a
claim, and a store blip is not evidence for it.

Each seat row carries `acquired_at` — **since when** its node has held it,
as an RFC 3339 UTC time on the coordination store's clock. It is the
tenure's start, stamped when the lease's `epoch` was minted and carried
unchanged through every renewal, so it moves exactly when `epoch` does: on
a takeover, and on the same node re-claiming after its own lease lapsed.
A seat held by a node of a build older than the stamp has no recorded
start and **omits** the field rather than rendering one.

```json
{
  "nodes": [
    {
      "id": "core-1", "roles": ["ingress", "seats", "workers"], "labels": {},
      "owner": "core-1:8f2a", "protocol": 5, "seats": 4, "expires_in": 41.2,
      "config_epoch": 7, "config_status": "ok", "config_error": ""
    }
  ],
  "seats": [
    {"handle": "ceo", "node": "core-1", "owner": "core-1:8f2a",
     "epoch": 4, "expires_in": 41.2, "acquired_at": "2026-09-23T08:02:11.482Z"}
  ],
  "duties": [{"duty": "maintenance", "node": "core-1", "expires_in": 41.2}],
  "unplaceable": [{"handle": "gpu-eng", "placement": "labels=gpu=true"}],
  "unmanned_roles": [],
  "this_node": "core-1"
}
```

### `GET /sandbox-runs`

Every detached [coding run](../concepts/code-sandbox.md) the engine still
holds, oldest first — `launching`, `running`, `awaiting_clarification`,
`reseed`, and `resumed` run records.

A run that has settled, whether its turn finished or it was lost, is not
listed because it has no record: its record is deleted once its box is
reclaimed. How it ended is on the event stream, in the resumed turn's own
events or a `sandbox_run_failed` event naming the reason.

A `launching` run is one whose coding job has started while the turn that
started it is still unwinding, so the suspended conversation a resume
re-enters is not on the row yet; it is listed but never polled, because a
row nobody lists is a box nobody reclaims.

This query reads the durable row directly. The live projection's panel is
reconciled against the same record every 30 seconds, but it carries only what
a running-runs panel draws. This board needs the row's own facts: the branch,
the placement, the pause TTL, whether a box still exists, and the bridge's
call log. It also lists `resumed` runs, which the panel drops because their
turn has already taken back the result. A `reseed` run (pause expired, box
reclaimed, work preserved on a pushed branch) is listed on both.

```json
{
  "runs": [
    {
      "turn_id": "<uuid>", "launch_id": "<uuid>", "agent_handle": "eng", "role": "Engineer",
      "status": "awaiting_clarification", "coding_agent": "claude-code",
      "task_description": "Add retry to the webhook client",
      "question": "Which backoff ceiling should I use?", "audience": "manager",
      "audience_handles": ["founder"], "audience_fallback": false,
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

`launch_id` names the job the row holds now. A turn can launch more than
one — a resumed executor that calls `run_sandbox` again reuses the row — and
`sandbox_tail` is asked by it, so a run's page polls the live
output of the job it shows rather than of whichever replaced it. It is empty
on a row an older build wrote, and such a run has no live output to ask for.

`box_exists` and `paused_at` stand in for the sandbox id: a board wants to
know that a box exists and that it is currently paused (and being billed
for as a snapshot), not which box it is. `paused_at` is the answer the
[pause reaper](../concepts/code-sandbox.md#mid-run-clarification-crewlet-ask)
acts on rather than the raw stamp, so a run parked on a question whose pause
instant never reached its row still reads as held: that box is being paid for,
and a board drawing the stamp alone showed it as a live one. `answerable_in_chat` is `false`
for a run whose turn was triggered by something other than an inbound
message — a schedule tick, a task assignment, an A2A wake — because the
resume path matches an inbound message's conversation identity against
the one the run was parked with, and those runs stored **no conversation
at all**: their trigger names neither key, so neither is stamped and
neither reaches the row. Telling somebody to "reply in the thread" would
send them to a thread that does not exist. Such a run is still answerable:
[`answer_run`](#answering-a-parked-coding-run) names it by its `turn_id` instead.

`audience` is the coding agent's own label for who should answer —
`requester`, `manager`, `team`, or a name it typed. `audience_handles` is
that label resolved against the org chart **when the run parked**: the seat
whose message or ask woke the turn, the seat's manager, its unit's lead and the
people in that unit, or the one colleague an exact match names.
`audience_fallback` is `true` when the label named nobody the chart has and the
question was put to the seat's lead chain instead (its managers, or the leads
of the units above it). Both are empty on a run that is not parked. See
[who is asked](../concepts/code-sandbox.md#who-a-question-is-put-to).

`?audience=<name>` narrows the board to the runs whose question is put to
one person, the name resolved as [a personal
question's](#whose-record-a-personal-question-answers-for) is: absent or either
of your own names is your own record, somebody's **login** is the seat the
identity directory binds them to — never read as a seat's handle, and asked of
the directory only once the caller may look — and any other name is the seat
it addresses, matched on the handle that seat was created under so a rename
between the park and the read is still one person. A login that holds no seat
is put nothing, and answers an empty board. It is a filter over the board and
not a personal read, so it has no scope rule beyond that: the unfiltered board
already names every run's audience. A run parked by a build that resolved no
audience matches no one.

`execute_state` — the serialised Execute-loop conversation — is
deliberately not returned: it is the largest column in the row and every
prompt in it is already reachable through the event store.

The run record lives in the fleet's coordination store, which every node
opens, so every node answers with the fleet's runs. A company with no sandbox
configured does not register the question at all, so the route answers `404`
with `unknown_query` rather than an empty board; a record that could not be
read answers `503` with a `Retry-After`, because "no run is parked" is a
claim and a store blip is not evidence for it.

### `GET /budgets`

Backs the dashboard's **Budgets** screen and `crewlet budgets show`. Every scope
— the company, and each agent seat — states **all three calendar windows**, the
day, the ISO week and the month on the company's clock, cut at the moment of the
answer:

- **`used`** is the fleet's shared counter for that window, in the
  [coordination store](../concepts/coordination.md#token-budgets-are-windows),
  written by every node running the company and surviving restarts. It is what
  the engine actually enforces against, and it is the same counter the
  [live token meter](#the-live-token-meter) pushes. A window no ceiling caps is
  still counted, because what a seat spent this week is a fact whether or not a
  ceiling is written for the week;
- **`limit`** is configuration — the company's from the active settings
  revision, a seat's from its runtime half on the
  [org chart](../concepts/chart-domain.md) — and **absent** where no ceiling caps the window — never `0`, which would state a
  range of nothing that is already full;
- **`refused_at`** is when a capped window last turned a charge away, kept in
  the same counter and cleared by the scope's next admitted charge or by the
  window turning over, and absent while it has not;
- **`state`** is the engine's judgement — `refusing`, `near` or `ok`, exactly as
  on the [live meter](#the-live-token-meter) — and `near_fraction` beside it is
  the one threshold behind `near` (0.9), for a screen that draws it as a mark.

Where the counter is already on a **later** window than the moment of the
answer — a peer's clock a few seconds ahead across a boundary, or the company's
`timezone` moved west — the row states the later window's spend, because that
is what the gate refuses against. Each window's allowance comes back when it
turns over; there is no route that resets a counter, and room before then is
made by raising the ceiling.

What a seat *spent over a window you choose* is not here: that is the per-agent
row of the [spend breakdown](#get-tokensbreakdown), a different span that must
not be divided into a ceiling. The ceiling and the durable counter are the pair
that can be, which is how this screen can say "this seat has burned 94% of
today's ceiling across two restarts".

```json
{
  "timezone": "Europe/Berlin",
  "durable": true,
  "near_fraction": 0.9,
  "org": {
    "windows": [
      {"period": "day", "window": "2026-06-08", "starts_at": "2026-06-07T22:00:00Z",
       "resets_at": "2026-06-08T22:00:00Z", "used": 1284410, "limit": 5000000, "state": "ok"},
      {"period": "week", "window": "2026-W24", "starts_at": "2026-06-07T22:00:00Z",
       "resets_at": "2026-06-14T22:00:00Z", "used": 4015220, "state": "ok"},
      {"period": "month", "window": "2026-06", "starts_at": "2026-05-31T22:00:00Z",
       "resets_at": "2026-06-30T22:00:00Z", "used": 9120045, "state": "ok"}
    ]
  },
  "seats": [
    {
      "agent_id": "<uuid>", "role": "Engineer", "handle": "eng",
      "windows": [
        {"period": "day", "window": "2026-06-08", "starts_at": "2026-06-07T22:00:00Z",
         "resets_at": "2026-06-08T22:00:00Z", "used": 99120, "limit": 100000,
         "refused_at": "2026-06-08T07:29:51Z", "state": "refusing"},
        {"period": "week", "window": "2026-W24", "starts_at": "2026-06-07T22:00:00Z",
         "resets_at": "2026-06-14T22:00:00Z", "used": 301877, "state": "ok"},
        {"period": "month", "window": "2026-06", "starts_at": "2026-05-31T22:00:00Z",
         "resets_at": "2026-06-30T22:00:00Z", "used": 702311, "state": "ok"}
      ]
    }
  ]
}
```

`durable` carries the honesty. It is `false` when the shared counter could not
be read, and every window list is then empty: a counter that cannot be read is
not a counter that reads zero, and without the flag a coordination blip renders
every seat at the bottom of its ceiling, which is the most reassuring possible
picture drawn at the moment nothing is known. Human seats have no row, because
they spend nothing.

Exhaustion is the engine's `refusing`, never a ratio a client computes. The
gate refuses a charge that would exceed the ceiling and increments nothing, so
a seat charged in 3k-token rounds against a 100k ceiling stalls near 99k and
never compares equal to its own limit: a ratio test shows a permanently blocked
seat at 99% and calls it healthy, and the Engineer above is refusing at 99 120
of 100 000.

### `POST /backup`

Copies this node's durable state — its store file, and every JetStream stream
and coordination bucket — into `?dir=`, a directory **on the engine's host**.

```bash
curl -X POST -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  "http://localhost:8000/backup?dir=/var/backups/crewlet/2026-08-30T18-00"
```

```json
{
  "taken_at": "2026-08-30T18:00:00Z",
  "finished_at": "2026-08-30T18:00:01.412Z",
  "node_id": "node-0",
  "engine_version": "v0.1.0",
  "store": {
    "file": "store.db", "source": "/data/company.db",
    "bytes": 258048, "migrations": ["0001_events.sql", "…"]
  },
  "streams": [
    {"name": "CREWLET_AGENT", "file": "streams/CREWLET_AGENT.snapshot",
     "bytes": 1087, "messages": 5, "config": {…}, "state": {…}}
  ]
}
```

The answer is the **manifest**, which is also written into the directory as
`manifest.json` — and its presence there is what marks the backup complete. A
failure anywhere leaves the directory without one, because a backup missing an
estate is unrestorable rather than partial.

This route exists because the state it copies is reachable only from inside
the engine, twice over. The store is locked to the engine's process and the
driver refuses a second process on a database file, so nothing outside can read
it; the embedded broker binds no socket, so nothing outside can reach the stream
estate either.
`crewlet backup` is a client of this route.

It is **synchronous and can take a while** — the duration is a property of the
data, not of this handler. That is deliberate: a job outliving its request
would need somewhere durable to record itself, and the only place is the store
being copied. The work is safe to be cut off, since the store copy is renamed
into place only after it verifies and the manifest is written last, so a
client that gives up leaves an unfinished directory rather than a false one.

Three refusals, each pointing somewhere different:

- **401 without a credential, and 403 without `fleet:operate`.** This writes
  every credential the company holds to a path the caller chooses, so it is
  the deployment's own control and no narrower grant reaches it.
- **400 for a destination this node cannot use** — relative, already occupied,
  one this host cannot create, read or make private (a path through a regular
  file, a parent that does not exist or is not writable, a read-only mount), or
  a path the database engine mishandles. The reason is returned in `detail`
  rather than only logged, unlike every other route here, because it is the
  caller's own command to fix. A disk that fails or fills while the directory
  is prepared is the node's failure, not the path's, and answers `500`.
- **A copy without the stream estate.** A node that dialled an external NATS
  cluster has no connection to snapshot the streams over, so its manifest
  carries the store copies alone and `crewlet backup` says where the rest
  lives. Back that half up at the cluster, from the same moment.

Every backup that began copying leaves a `backup_requested` event naming the
caller, the node, the directory and whether it finished — a failed one
included, since it may have left files there. A destination refused with a
`400` wrote nothing and leaves no event — a `400` is only ever answered before
a byte is copied; a refusal that arrives with part of the copy already in the
directory is a failed backup and is recorded as one: see
[the runtime audit](#the-runtime-audit-sourceoperator).

The request has no deadline of its own on the engine's side, and a client
should give it a long one: `crewlet backup` and the dashboard both wait up to
**30 minutes** for the answer, because the copy is bounded by the size of the
store and the stream estate and a client that gave up early would report a
failure while the engine finishes a good backup. Taking one from the dashboard
is **Settings › Backups & retention › Take a backup**.

### `GET /backups`

Backs **Settings › Backups & retention**: what the fleet has backed up. **It
takes `fleet:operate`** — every row names a directory on a named host that
holds the company's sealed credentials.

Two records, because they answer two questions:

- **`points`** — each owner's NEWEST backup, from the fleet's backup register:
  what each node announced when its manifest was written, plus the operator's
  acknowledgement (`crewlet retention ack`, `kind: "operator"`). `counted`
  says the trim may count it — the `backup_floor` policy (`policy`) takes this
  owner's word and the copy was verified — and exactly one counted point is
  `newest`: the one the trim's backup term reads. `bytes` is the whole
  artefact (every database copy and stream snapshot) and is absent on an
  acknowledgement, which asserts a copy the engine never saw. `covers` is how
  far the copy reaches in each state-log stream.
- **`history`** — every `POST /backup` a person made, newest first, from the
  [runtime audit](#the-runtime-audit-sourceoperator) every node keeps for the
  event log's 30 days: when it finished, the `node` whose disk holds it, who
  asked (`actor`, `actor_kind` and the credential, `operator_id`), the `dir`, and
  `outcome` — `applied` (the manifest was written) or `failed` (it was not: the
  directory holds debris, not a backup). At most 100 rows; `more` says the page
  filled. `coverage` names the nodes the history was read from, since a node
  that did not answer takes its backups' rows with it.

```json
{
  "policy": "engine",
  "points": [
    {"owner": "node-a", "kind": "node", "taken_at": "2026-09-30T02:00:00Z",
     "dir": "/var/backups/crewlet-20260930-0200", "verified": true, "bytes": 83886080,
     "covers": [{"stream": "CREWLET_TRACKER_LOG", "generation": 2, "seq": 9001}],
     "counted": true, "newest": true}
  ],
  "history": [
    {"id": "6ac15845-97c8-4ac2-9ac8-bfa7729a3572", "at": "2026-09-30T02:00:21Z",
     "node": "node-a", "actor": "jane-founder", "actor_kind": "human",
     "operator_id": "session:0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a01",
     "dir": "/var/backups/crewlet-20260930-0200", "outcome": "applied",
     "summary": "jane-founder backed up to /var/backups/crewlet-20260930-0200 (25 streams)"}
  ],
  "more": false,
  "coverage": {"nodes": [{"id": "node-a", "answered": true, "error": ""}], "complete": true}
}
```

### `GET /integrations`

Backs the dashboard's **Integrations** screen: how each external surface is
wired, and what has come through it.

The counts are **page-capped, not time-bounded** — the most recent page of the
delivery log, however long that spans — which is why each response carries the
timestamp of the oldest delivery it counted. "42 inbound" alone could be an
hour or a year; "42 since Tuesday" is a measurement.

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
      "skipped": 30,
      "coalesced": 2,
      "last_at": "2026-06-08T07:31:10+00:00"
    }
  ],
  "tools": [
    {
      "key": "gitlab", "surfaces": ["gitlab"],
      "state": "attention", "label": "Credential expiring",
      "reason": "the group Owner token this integration runs on expires on 2026-06-20, …",
      "surface": "gitlab"
    }
  ]
}
```

`tools` is **one roll-up per tool** this build serves — `slack`, `mattermost`,
`atlassian` (the organization, Confluence, Jira and the Forge relay),
`github`, `gitlab`, `datadog` — whether or not the company configured it, so a
reader never invents a state for a missing one. `state` is `attention` (a
person has to act), `not_connected` (configured and not working yet, with
nobody owing anything — or this node could not read the status),
`connected` or `not_in_use` (no block, or every block switched off; `label`
says which). `label` is the state in a reader's words, `reason` one sentence
on why, and `surface` the surface it was taken from. The rules are
[One state per tool](../concepts/integration-reconcile.md#one-state-per-tool).

A reconcile finding of kind `credential_expiring` carries `expires_at`, the
instant the credential stops working.

`inbound`, `skipped` and `coalesced` answer one question together and are
misleading apart. `inbound` counts deliveries the edge accepted; `skipped`
counts those the routing gate dropped without waking anybody; `coalesced`
counts merges, where N same-conversation notifications became one turn. "128
arrived" on its own cannot tell a working integration from one whose every
delivery reaches nobody — "128 arrived, 30 dropped, 2 merges" can, and a seat
draining a thread's backlog as one turn stops looking like a seat that ignored
twelve messages.

The two outcome counts are **three-valued** like the secret fields: `null`
means this node could not read its event log, and reporting that as `0` would
claim every delivery woke a seat on a node that cannot tell. They come from
the engine's own `notification_skipped` and `notifications_coalesced` events
rather than from the inbound rows, so they are bounded by the same event-log
window `traffic_since` names.

`secret_present` and `secret_usable` are **two different facts**, and the gap
between them is a silent outage.

`secret_present` is a claim about the **document**: an operator wrote a secret
down. It is three-valued because the cases mean opposite things: `null` — this
surface does not use one (Mattermost authenticates its websocket with the
bot's own token, and Atlassian receives no delivery to verify); `false` — it
does, and none is configured, which means the webhook route answers `503` to
every delivery.

`secret_usable` is a claim about what this process **resolved**. A secret lives
in the config as a `${VAR}`, so `secret_present: true, secret_usable: false` is
a route refusing every delivery while the config shows a secret and the
third-party app's settings page shows a healthy hook, with nothing anywhere
naming the variable. For GitLab the bar is higher than non-empty: the value
must be `whsec_` over standard base64 of a 32-byte key, the only shape the
third-party app signs with. For Slack, whose material is one signing secret per
seat, it is lower: **one** seat whose secret resolved makes the surface usable,
because a delivery addressed to that seat's path would be accepted, and a seat
whose own secret is unresolved is reported by that seat's identity finding
rather than by the whole surface. `null` means this node cannot say (nothing has resolved
yet), or the surface has no secret to resolve.

Only the booleans are ever returned; no secret value leaves the process.

**A row exists for a surface the company's document turns on, and for no
other.** That sounds like a restatement of "the block is present", and for six
of the eight it is. Slack and GitHub are the two where a seat carries its own
app — its own credential, its own inbound path — and both used to be reported
on those per-seat values *as well as* on the company block, so that a company
holding nothing but per-agent apps still had a row.

Neither half of that survives contact with what the engine does. The **parser**
that turns a verified delivery into work for a seat is registered only where
the company block is present and enabled: drop `integrations.slack` and the
transport is retired (`slack_retired`); drop or disable `integrations.github`
and the same happens (`github_retired`). A seat app without it delivers to a
route that verifies the signature and then has nowhere to send it. So a
company in that state is not a surface missing a row — it is a surface that is
**off**, and a row for it is a row with `enabled: false`, which the dashboard
draws as *Paused*.

That matters because `enabled: false` is the one field on this row that claims
somebody's **intent**. A disconnect produced the other reading every time: the
block went, the seats kept their sealed credentials, and the card an operator
had just disconnected settled on *Paused* — a word for a state nobody had
chosen — and stayed there. `enabled: false` now means a block that says
`enabled: false`, on every surface, and a company whose block is gone gets no
row and a Connect button. What each seat is still holding is on the [setup
screen's seat roster](#per-seat-setup), which is where a seat is acted on.

`seats` lists the agents carrying their **own** identity on that surface: a
Slack app, a Mattermost bot, a per-seat project or space, wherever they sit in
the hierarchy. A seat in a unit is a seat: the list is read from the company's
own [org chart](../concepts/chart-domain.md), not from the stored revision's
`roles:` — a revision carries no seats at all, and the top-level block was by
definition only the seats belonging to no unit.

`routes` is the third of the same family: whether a **verified** delivery
would wake a seat. The three fail independently, and an operator staring at a
silent integration needs to know which half broke.

It is `null` for a surface nothing ever arrives from, which is a different
answer from `false` and the only honest one. **Atlassian** is that surface:
an organization is where an agent's account is *created*, and the products
that account then works in — Jira, Confluence — are separate surfaces with
their own webhooks and their own parsers. Reporting `routes: false` there
described a real fault ("deliveries are verified and stored and no parser
turns them into work") about a surface that is not asked the question, beside
a `secret_usable: false` for a secret it does not have. Both are `null` now,
and so are `inbound_kind` and `inbound_path` — the row used to name
`/webhooks/atlassian`, a route this engine does not serve, as the address to
check a settings page against.

Mattermost is `false` rather than `null` when it does not route: it has no
inbound *address* because the engine dials out, but everything said in its
team arrives, so a missing parser there is the outage the field is for.

**A surface is asked about the source its deliveries are published as**, which
is not always its own name. The **Forge relay** is the case: a Cloud event it
relays is republished as the product it belongs to — `jira` or `confluence` —
and parsed by that product's parser, so nothing is ever registered under
`forge`. Asked about itself the relay answered `false` on every Cloud
deployment for ever, and because the dashboard groups it under the Atlassian
row, a tenant whose relay was feeding both products correctly carried a
permanent *Forge relay — routes nowhere* beside the two rows saying they
routed fine. It now answers `true` when either product's parser is registered;
the finer answer is on those two rows, immediately below it.

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
      "scope_type": "unit", "scope_id": "backend", "scope_name": "backend",
      "name": "daily-standup",
      "cron": "30 9 * * 1-5", "timezone": "Europe/Amsterdam",
      "task": "Post your standup…", "target": "each",
      "enabled": true, "timeout_seconds": 180, "catchup": true,
      "runners": ["backend-lead", "backend-dev"],
      "next_run": "2026-06-09T07:30:00+00:00"
    }
  ],
  "recent_runs": [
    {
      "scope_type": "unit", "scope_id": "backend", "scope_name": "backend",
      "schedule_name": "daily-standup", "target_handle": "backend-dev",
      "scheduled_at": "2026-06-08T07:30:00+00:00",
      "fired_at": "2026-06-08T07:30:02+00:00", "outcome": "fired"
    }
  ]
}
```

A run's `outcome` is `fired`, `skipped_catchup` (a missed tick outside the
catchup window) or `skipped_paused` (the runner seat was
[paused](../concepts/agent-runtime.md#pausing-a-seat) when the fire came due).
`recent_runs` is empty when the dispatch ledger cannot be read (the
configured list and `next_run` still render). A schedule with no next fire —
disabled, an unparseable cron or timezone (`problem` says which), or a date
the calendar never reaches — carries **no** `next_run` key, never a zero
instant.

`scope_id` is the scope's **identity** — a seat's agent id for a role schedule,
a unit's origin key for a unit one — which is what the at-most-once ledger keys
a fire on and what `schedule_runs` takes as its parameter. `scope_name` is the
same scope as a person reads it (the handle, or the unit's key), resolved
through the company this node is running: a renamed scope reads under the name
it answers to now, on every row of its history.

---

## Token Spend Breakdown

Two sources, one aggregation, and which one answers is decided by the
parameters:

- **The live window** — a request naming no `days`, no dates, no `seat` and no
  `previous` — is the projection's: the phase records of the last
  24 hours (`livestate.LiveSpendWindow`, rolling), held in memory and pushed as
  the [`tokens` push](#pushes). The dashboard's live views read it; the Spend
  screen reads named windows only, so its figures are the company's. It is the
  only answer with a per-turn tail (`by_turn`) and a watermark
  (`aggregated_through`).
- **Every named window** is whole **company days** read from the replicated
  [`usage` domain](../guides/replication.md#two-compacted-domains-the-embeddings-and-each-nodes-day) (ADR-0020): every
  node's day, applied on every node. So the answer is the same whichever node
  is asked, reaches back **181 days** (the domain's history, not the event
  log's 30), and still counts a node that has left the fleet. It replaced a
  scan of the answering node's own event log, which reported a third of a
  three-node fleet's spend under the company's name and drew a ninety-day chart
  over thirty days of rows.

Both are folded by `internal/tokens`, so a reader moving between the two
compares like with like. The guide [Budgets and spend](../guides/budgets-and-spend.md)
explains the windows, the counter a budget enforces and how it differs from
this rollup.

**The window parameters**, shared by both routes:

| Name | Default | Description |
|------|---------|-------------|
| `days` | `1` on a named window | The company days ending today, on the company's [clock](../getting-started/configuration.md#the-companys-clock): `7` is today and the six before it. `1` to `90` (`tokens.MaxSpendRangeDays`); anything else is **400** (`tokens.ErrWindowLength`) naming `days`. |
| `since` / `until` | — | Instead of `days`: two company dates, `2026-06-01`, **both inclusive** — `since=2026-06-01&until=2026-06-08` is eight days. A pair or neither; at most 90 days — a longer pair is **400** (`tokens.ErrWindowLength`: `2026-05-01 to 2026-09-29 is 152 days, and a spend window is at most 90 — bring since and until closer together`) wherever in the history it lies; never together with `days`. |
| `previous` | `false` | The same number of company days ending the day before the window begins — compare-to-previous, cut on the company's calendar rather than a browser's, so the two windows are never different weeks. |
| `seat` | (every seat) | One seat, by any **handle** it answers to, a retired one included — resolved through the chart to the agent id every node derives from the handle the seat was created under, so a renamed seat answers for its whole history and a seat since removed from the chart still answers for the days it left behind. A person's seat is refused `bad_params`: it spends nothing. |

A window whose first day — or whose `previous` window's first day — is older
than the history's floor is **400** (`tokens.ErrOutOfRange`, a different
class from a window that is merely too long) naming the parameter to change, never answered short: the rows before the floor are gone
on every node, and a heading over fewer days than it names is a lie about the
numbers under it. At 90 days the previous window begins 179 days back, inside
the 181.

### `GET /tokens/breakdown`

The rollup: the window's spend by phase, model, provider entry, worker and seat.

**Response** (a named window)

```json
{
  "since": "2026-06-08T15:00:00Z",
  "until": "2026-06-15T15:00:00Z",
  "from": "2026-06-09", "to": "2026-06-15", "days": 7,
  "horizon": { "days": 181, "floor": "2025-12-16" },
  "totals": {
    "input_tokens": 17700, "output_tokens": 2750,
    "total_tokens": 20450, "calls": 6,
    "cache_read_tokens": 12100, "cache_write_tokens": 900
  },
  "by_phase": [
    { "phase": "execute", "input_tokens": 14000, "output_tokens": 2000,
      "total_tokens": 16000, "calls": 2 },
    ...
  ],
  "by_model": [
    { "model": "claude-sonnet-5", "total_tokens": 19300, "calls": 4, ... },
    ...
  ],
  "by_provider": [
    { "provider_key": "anthropic", "models": ["claude-sonnet-5", "claude-haiku-4"],
      "seats": ["pm", "coder", "reviewer"], "seats_total": 5,
      "total_tokens": 20100, "calls": 5, ... }
  ],
  "by_worker": [
    { "worker": "persist_decider", "total_tokens": 900, "calls": 1, ... }
  ],
  "by_agent": [
    { "role": "PM", "handle": "pm", "agent_id": "<seat's agent id>",
      "turns": 4, "failed": 1,
      "total_tokens": 20200, "calls": 5, ...,
      "by_phase": {
        "execute":   { "total_tokens": 16000, "calls": 2, ... },
        "review":    { "total_tokens": 1400,  "calls": 1, ... },
        "auxiliary": { "total_tokens": 900,   "calls": 1, ... }
      }
    },
    ...
  ]
}
```

Notes:

- `since`/`until` are the window as instants — the first instant of its first
  company day and the first instant after its last, `until` exclusive — and
  `from`/`to`/`days` name the same window by its days. The live window carries
  only the instants.
- `horizon` states how far back a named window can reach: the history in days
  and `floor`, the oldest company day still answerable. It is named `horizon`
  rather than `coverage` because nothing here was asked of a node — the rows
  are replicated whole, and what bounds them is time, not presence.
- `by_phase` covers every phase the [Turn Engine](../concepts/turn-engine.md)
  emits (`onboarding`, `execute`, `review`, `subagent`, `auxiliary`, `judge`,
  `sandbox`), as recorded; a store that predates the two-stage redesign also
  holds `plan`. The series folds these into four bands; the rollup does not.
- `by_provider` answers "which configured entry (`providers.llm.<key>`) do we
  pay for", which `by_model` cannot: a fallback chain serves several models
  under one key. `models` is every model the entry answered with, biggest
  first; `seats` the three handles that spent the most through it and
  `seats_total` how many did at all. A call recorded before the key was
  promoted (node migration 0032) is under `unknown`.
- `by_agent[].turns` and `failed` are how many of the seat's turns ENDED in
  the window, and how many of those failed — a named window only. The live
  window holds phase records, not endings, so it carries neither rather than
  a count of "turns that spent", which is a different number. A seat that
  ended a turn without spending is still listed.
- `by_turn` — one row per RUN, newest first, capped at 50 — and
  `aggregated_through`, the newest phase counted, are the **live window's
  only**. A company day holds no turn and no per-call instant, so a named
  window has neither. Per-turn spend over any window is
  [`GET /turns?sort=-tokens`](#queries).
- A seat is one row per derived agent id, named from the chart — the handle and
  name it answers to now — or, for a seat the chart no longer holds, by the
  newest day that named it: a seat renamed mid-window is one row under its
  current name. `agent_id` and `seat` on the answer echo a `seat=` narrowing
  the same way, absent when nothing narrowed it.
- All lists are sorted by `total_tokens` descending, ties on the name.
- Every bucket also carries `cache_read_tokens` and `cache_write_tokens`:
  the share of `input_tokens` the providers' prompt caches served and
  stored. A **breakdown** of the input, never an addition to it —
  `input_tokens` already counts the cached prefix on every backend, so the
  cache's share of a bucket is `cache_read_tokens / input_tokens`, and
  `total_tokens` stays input plus output. Both sources carry them, so a
  window reads the same share whichever answered it. A phase recorded by a
  build that did not count the cache reads 0.
- Every live-window bucket also carries `cost_usd` and `priced_calls`. **Two
  numbers, because zero dollars is two different facts**: only a subscription
  coding CLI reports a price, so a `cost_usd` of 0 over `priced_calls: 0`
  means nobody said what this cost, while 0 over 3 means three runs were billed
  nothing. The usage domain does not carry a price, so a named window's are
  zero over zero. The dashboard reads neither field: it measures spend in
  tokens and never in money
  ([rule 19](dashboard-design.md#rules-a-change-has-to-keep)).

### `GET /tokens/series`

The same spend with a **time axis**: one bucket per company day or ISO week
over a named window, each split into bands on one dimension. There is no live
path — its buckets are company days, which only the usage domain holds.

**Query parameters** — the window parameters above, plus:

| Name | Default | Description |
|------|---------|-------------|
| `group` | `phase` | The dimension the bands are: `phase`, `model`, `provider`, `seat`, `unit` or `worker`. Anything else — `turn` included — is **400** naming the set. `phase` is the **four bands** below; `seat` is keyed by the seat's agent id — never a name, which two seats may share and a rename moves — and carries the name it answers to now as its `label`, beside its `handle`; `unit` resolves through the org chart's DIRECT unit for each seat, keyed by the unit's key and labelled with its name — not the chain, because a band per nesting level would count the same spend for the team and again for the department above it. A usage row carries no project and no work item; that attribution is the tracker's own per-item counters. |
| `bucket` | `day` | `day` (a company day) or `week` (the company's ISO week, from Monday midnight on its clock). Nothing finer: a day is the smallest thing every node's usage agrees on, and `hour` is **400**. |
| `groups` | `4` | How many bands before the rest fold into the residual. Capped at 20. Four is how many data hues the design system has, and exactly the phase breakdown's band count — a phase grouping never folds. |

**The four phase bands**, folded once in `tokens.PhaseBand`:

| Band | Phases |
|------|--------|
| `execute` | `execute`, `sandbox` (a detached coding run is the executor's own work done elsewhere), and the retired `plan` |
| `review` | `review` |
| `workers` | `subagent` — the workers an executor delegated to |
| `auxiliary` | `auxiliary`, `judge`, `onboarding`, and any phase this build does not know |

**Response**

```json
{
  "group": "phase",
  "bucket": "week",
  "since": "2026-06-09T15:00:00Z", "until": "2026-06-23T15:00:00Z",
  "from": "2026-06-10", "to": "2026-06-23", "days": 14,
  "horizon": { "days": 181, "floor": "2025-12-24" },
  "series": [
    { "at": "2026-06-07T15:00:00Z", "window": "2026-W24", "days": 5,
      "total_tokens": 80, "calls": 1, ...,
      "groups": { "execute": { "total_tokens": 80, "calls": 1, ... } },
      "other":  { "total_tokens": 0, "calls": 0, ... } },
    { "at": "2026-06-14T15:00:00Z", "window": "2026-W25", "days": 7, ... },
    ...
  ],
  "by_group": [
    { "group": "execute", "other": false, "folded": 0, "total_tokens": 16000, "calls": 2, ... },
    { "group": "review",  "other": false, "folded": 0, "total_tokens": 300,   "calls": 9, ... }
  ],
  "totals":  { "total_tokens": 20450, "calls": 6, ... },
  "grouped": { "total_tokens": 20450, "calls": 6, ... }
}
```

Notes:

- **Every bucket in the window is present, including the empty ones.** A
  series with holes is a chart the client has to repair. A quiet day is a gap
  of full height, not a column the chart squeezed out.
- `at` is the bucket's **start** and `window` its label on the company
  calendar (`2026-06-14`, `2026-W25`). A week's `at` can be before the
  window's `since`: the first and last weeks can be partial, and `days` says
  how many of the window's days each bucket holds.
- `by_group` is the **legend and the grid**: each band's total over the whole
  window, with the residual last. By `phase` ALL FOUR bands are listed, in the
  stacking order above whatever their size — a band nothing spent in is there
  at zero, so the legend is the same four every time; every other grouping is
  biggest first and lists only what spent. Which bands survive the cap is decided over the WHOLE window, never
  per bucket — a per-bucket decision would put a band in the chart for the
  days it happened to lead and in the residual for the rest.
- By `seat` and by `unit` a band's `group` is an **identity** — the seat's
  agent id, the unit's key — and its `label` is what it is called. Two seats
  or two units may share a name, and a band keyed on the name was both of
  them. A `seat` band also carries its `handle`, absent for a seat the chart
  no longer holds; a `unit` band carries `seats`, how many seats spent in it.
  A seat at the root of the chart sits in the `no unit` band, which no unit
  key can collide with.
- The residual carries an **empty `group` and `other: true`**, rather than a
  reserved name: a model or seat genuinely called `other` must not be mistaken
  for the fold. `folded` is how many distinct groups it stands for, and it
  sums every field of the bands it stands for, the cache counts included.
- `totals` is every cell in the window, **including the ones this grouping
  places in no band at all** — grouping by `worker` leaves out every phase
  that is not a worker's. `grouped` is what the bands do cover, so the gap is
  a number rather than an inference a reader has to make by subtracting.

---

## Webhook Deliveries

Every verified delivery writes one row to the event log under
`category: "webhook"`, so the listing answers "what has been arriving" without
reading a payload:

| Field | What it carries |
|---|---|
| `type` | `webhook:<event>`, or `forge:<event>` for an Atlassian Cloud relay |
| `source` | The integration the **payload** belongs to — the route for six of the seven, and the relayed product for Forge |
| `summary` | The delivery in one sentence |
| `tags.recipient` | The seat a per-seat delivery was addressed to, absent for a company-wide one. The log resolves it to that seat's agent id and indexes the delivery under it as a **party**, so `GET /events?agent=<handle>` also returns what reached an agent seat from outside |
| `tags.delivery_key` | The provider's own delivery id, absent for the providers that send none — what an operator has in front of them in the provider's console |
| `payload` | The **raw body the provider sent**, on `GET /events/{event_id}` only. A listing never carries a payload, so a deliveries screen is one request rather than one per row |

Note that a row exists only for a delivery that was **verified, claimed and
queued**. A refusal — a bad signature, an unset secret, a body too large — is
answered at the edge and appears in the engine's log rather than here.

---

## The Tool Catalogue

Carried by `GET /tools`, by the `tools` push, and inside the socket snapshot.
One row per registered tool:

```json
{
  "name": "post_message",
  "description": "Post a message to a channel",
  "source": "slack",
  "title": "Post message",
  "annotations": {
    "read_only": "no",
    "destructive": "no",
    "idempotent": "unknown",
    "open_world": "yes"
  },
  "delivers": "slack",
  "input_schema": { "type": "object", "properties": { "…": {} }, "required": ["…"] }
}
```

- **Every hint is a WORD, never a bool**, and the third word is the point:
  `unknown` means the server did not advertise the hint, which is a different
  fact from `no`. A bool cannot hold the difference — an absent hint would
  arrive as `false` and read as a positive denial, so a fresh MCP server's
  unannotated tools would look like proven reads on the one screen an
  operator audits them on. The engine's own delivery fence has always read
  them this way (see [`Registry.KnownReads`](../guides/tools-and-mcp.md)): an
  unannotated tool is **not** a known read.
- `delivers` names **where** calling this tool puts something in front of
  somebody outside the turn, and is empty for a tool that reaches nobody. It
  is the registry's own predicate, not "was this served by MCP": a proven
  read-only MCP tool delivers nowhere, and the native tracker's comment tool
  delivers although it is a builtin.
- `title` is the human-readable name a server advertised, omitted when it
  advertised none.
- `input_schema` is the JSON Schema the model is offered, verbatim. It is
  **absent** when the tool takes no arguments, which is not the same as `{}`:
  only the first means this build did not send one.

---

## Webhook Endpoints

### Webhook refusals

A refusal on every `/webhooks/*` route is the engine's one [refusal envelope](#every-refusal-is-one-envelope) — `error`, the code a client branches on, and `message`, the sentence the code carries — and every `503` says when to come back in `Retry-After`. A sender reads only the status and the `Retry-After`, and both are chosen for it: a `401` is a delivery that will never verify, a `503` is one to hold and retry. The body is for the operator reading a vendor's delivery log, a proxy's access log or the engine's own.

| Status | `error` | When | `Retry-After` |
|---|---|---|---|
| `401` | `invalid_signature` | The delivery's credential did not verify — an HMAC signature, a shared token or a Forge invocation token, missing or wrong — or a per-seat Slack delivery names a seat that has no Slack app here while other seats have one. The log says which check failed; the body never does, because that is what somebody probing the route would read, and a code of its own for the seat with no app would be a list of which handles have one | none |
| `503` | `no_active_revision` | This node has no active company revision, so it has no secrets and nothing to route by. Answered **before** the signature check, because verifying first would answer every delivery with the no-secret `503` and its five-minute wait | `15` — the reconcile poll that brings a node a missed activation |
| `503` | `no_webhook_secret` | The route has no secret configured to verify against | `300` — a person editing the configuration |
| `503` | `unusable_webhook_secret` | The configured secret cannot do the check it is for: a shared token shorter than 26 characters on a route whose provider signs nothing, or a GitLab `signing_secret` that is not a `whsec_` key | `300` |
| `503` | `unavailable` | The delivery verified and the broker would not take it; its claim is released so the retry is not refused as a duplicate | `4` — the broker's own minimum election timeout, the hint every surface gives for a broker it cannot reach: a refused publish waits on a reconnection or an election, and nothing changes sooner |
| `413` | `body_too_large` | Over 25 MiB, or a verified delivery too large to publish — refused for good, so the provider stops retrying it | none |
| `400` | `unreadable_body`, `invalid_body` | The body did not arrive whole, or is not one JSON object | none |

### `/webhooks/jira`

Receives **Data Center** Jira webhook payloads (issue created, updated, commented, assigned). Verifies HMAC-SHA256 over the raw body against `X-Hub-Signature`, keyed on `integrations.jira.webhook_secret`; a wrong or missing signature is `401 invalid_signature`, and a route with no resolved secret answers `503 no_webhook_secret` rather than accepting the delivery. Deduped on `X-Atlassian-Webhook-Identifier`, which is stable across Jira's own retries. **Jira Cloud does not use this route** — a Cloud webhook belongs to an app, so those events arrive through [`/webhooks/forge`](#webhooksforge) with their own JWT, and `webhook_secret` is unused there. Publishes to `crewlet.notifications.inbound`. See [Jira Integration — Webhooks](../integrations/jira.md#webhooks-jira-pushes-to-agents).

### `/webhooks/slack/{handle}`

Receives Slack Events API payloads for a specific agent (identified by handle). Verifies the signing secret for **that agent's own app** — Slack gives each seat its own, so the handle in the path is what selects the key: a wrong signature is `401 invalid_signature`, and so is a handle with no app while other seats have one — the same body, after the same signature check against a key that verifies nothing, so an unsigned request cannot tell which handles have a Slack app; the engine's log says which it was (`slack_webhook_unknown_handle`). No Slack secret for any seat is `503 no_webhook_secret`. Publishes to `crewlet.notifications.inbound`. Slack's `url_verification` challenge is answered **without a signature check**, so a freshly provisioned app's Request URL verifies before its signing secret is in config — it has to, because during provisioning that secret does not exist yet. It is not answered by a node with no active company revision, which answers `503 no_active_revision` like every other delivery: verifying a URL that then discards every event would be worse than making Slack retry. See [Slack Integration](../integrations/slack.md).

### `GET /webhooks/slack-oauth`

The OAuth install landing page for [`crewlet slack provision`](../integrations/slack.md). Every provisioned Slack app has this as its OAuth redirect URL. After the operator approves an install, Slack redirects here with a temporary `code` (and `state` carrying the agent handle); the page displays the code for pasting back into the waiting CLI prompt. Unauthenticated by design: the code expires after 10 minutes and is useless without the app's client secret, which only the provisioning CLI holds. Every value on the page comes from the query string, so it is served under a policy that allows its one inline style by hash and no script at all (see [Security headers on every response](#security-headers-on-every-response)).

### `/webhooks/github`

Receives GitHub webhook payloads. Verifies HMAC-SHA256 over the raw body against the `x-hub-signature-256` header, keyed on the required `webhook_secret` from the `github` config block; invalid or missing signatures are rejected with `401 invalid_signature`, and a route with no resolved secret answers `503 no_webhook_secret` with a `Retry-After` so the delivery is held for retry rather than blamed on the sender. Deliveries are deduped on `X-GitHub-Delivery`, which is stable across GitHub's own retries and an operator's manual redelivery. **The event name is in the `X-GitHub-Event` header**, not the body — the payload carries only the action — so the header is carried onto the envelope and read by the parser. Publishes to `crewlet.notifications.inbound`. The same handler serves `POST /webhooks/github/{handle}`, which is the address a seat's own [GitHub App](../integrations/github.md#one-github-app-per-agent) is created with: the handle travels onto the published event so five agents' apps reporting one comment are five wakes rather than four duplicates, and both forms verify against the same company `webhook_secret`, so a seat in the path is not a way past the signature check. See [GitHub Integration — Webhooks](../integrations/github.md#webhooks).

### `GET /webhooks/github-app`

Where GitHub returns an operator's browser during the per-agent
[GitHub App](../integrations/github.md#one-github-app-per-agent) flow, and one
of the two `/webhooks/*` routes that render a page rather than accept a delivery
(the other is [`/webhooks/slack-oauth`](#get-webhooksslack-oauth)).
Two arrivals, one route: after the app is **created**, with a one-time code to
convert, and after it is **installed**, with nothing but `?installed=<handle>`.
Unauthenticated, because a redirect from GitHub carries no engine credential;
the `state` minted by [`POST /setup/integrations/github/app`](#one-agents-own-github-app)
stands in its place and is a token sealed under the fleet keyring naming the
seat and who began the creation, opened before the code is converted — and
**spent** there, so a state that reached a browser
history or an ingress access log cannot be presented a second time within the
fifteen minutes it stays valid. The spend goes through the fleet's claim
registry, so it holds when the two halves of the flow land on different nodes,
and it fails CLOSED: a registry that cannot answer has not said the link is
unused. **The install arrival carries no such proof and so changes
nothing** — it renders a page and no more; the
[reconcile loop](../concepts/integration-reconcile.md) is what records the
installation, from GitHub's own list rather than from the query. On a conversion the app's private key and webhook secret are
sealed before anything else can fail, because GitHub returns both exactly once
and reissues neither. Answers `200` for a completion or an install, `400` for a
refusal from GitHub, a missing code, or a state or code the engine will not
accept, and `503` when this process has no setup surface. Error text is always
the engine's own wording: GitHub's response body here carries the private key.
The page runs only its own inline style and install countdown script, allowed
by hash (see [Security headers on every response](#security-headers-on-every-response)).

### `/webhooks/gitlab`

Receives GitLab webhook payloads. **The signature is the only credential**: `webhook-signature` is verified as a Standard-Webhooks HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}`, keyed on the `signing_secret`'s base64 payload, constant-time against any of the header's space-separated `v1,…` entries, with a ±5-minute timestamp tolerance. A missing or wrong signature is rejected with `401 invalid_signature` — the plaintext `X-Gitlab-Token` is not accepted, so omitting the signature header is not a downgrade path. Answers `503` with a `Retry-After` when no `signing_secret` is configured (`no_webhook_secret`), or when its value is not a usable `whsec_` key (`unusable_webhook_secret`), so the delivery is held for retry rather than blamed on the sender. GitLab signs whenever the hook has a `signing_token` (GitLab 19.1+); see [GitLab § Verification](../integrations/gitlab.md#verification). Publishes to `crewlet.notifications.inbound`. See [GitLab Integration — Webhooks](../integrations/gitlab.md#webhooks).

### `/webhooks/confluence`

Receives **Data Center** Confluence webhook payloads (page created/updated, comments). Verifies HMAC-SHA256 over the raw body against `X-Hub-Signature`, keyed on `integrations.confluence.webhook_secret`; a wrong or missing signature is `401 invalid_signature`, and a route with no resolved secret answers `503 no_webhook_secret`. **Confluence Cloud does not use this route** — those events arrive on [`/webhooks/confluence/{event}`](#webhooksconfluenceevent) or through [`/webhooks/forge`](#webhooksforge), which is why `webhook_secret` is required on Data Center and unused on Cloud. Publishes to `crewlet.notifications.inbound`. See [Confluence Integration](../integrations/confluence.md).

### `/webhooks/confluence/{event}`

Receives one **Confluence Cloud** event, named by the path because a Cloud payload does not say which event fired — the registered URL is the only thing that knows. Cloud signs nothing and honours no registration field for a header, so the authentication is a **shared token**, compared constant-time: `X-Crewlet-Token` is read first and `?token=` in the query is the fallback, which is where `crewlet confluence provision` puts it. A route whose `integrations.confluence.webhook_token` is unset answers `503 no_webhook_secret`, and one whose token is shorter than 26 characters `503 unusable_webhook_secret` — the token is the entire check, so its length is the entire strength. A wrong or missing token is `401 invalid_signature`. The engine never logs the query string on this route. Deduped on a hash of the raw body, because Cloud sends no per-delivery identifier. See [Confluence Integration — Webhooks](../integrations/confluence.md#webhooks-confluence-pushes-to-agents).

### `/webhooks/datadog`

Receives a Datadog **monitor alert**. Datadog's Webhooks integration attaches custom headers with fixed values only, so there is nothing varying with the payload to sign and the authentication is a **shared token**, compared constant-time against `X-Crewlet-Token` and keyed on `integrations.datadog.webhook_token`. An unset token answers `503 no_webhook_secret`, and one shorter than 26 characters `503 unusable_webhook_secret`. A mismatch is `401 invalid_signature`. Deduped on the payload's own `id`, which Datadog repeats across its retries. The alert routes by the monitor's TAGS — `crewlet:<handle>` by default — falling back to `integrations.datadog.route_to`, because a monitor is addressed to nobody. Publishes to `crewlet.notifications.inbound`. See [Datadog Integration](../integrations/datadog.md).

### `/webhooks/forge`

Receives events from the Atlassian Forge app. Every request must carry a Forge Invocation Token (FIT) as an `Authorization: Bearer` JWT; the token is verified against Atlassian's JWKS endpoint and its `aud` claim must match the configured `forge_app_id` (`401 invalid_signature` on failure, and `503 no_webhook_secret` with a `Retry-After` when no app id is configured, because the app id is the audience this route verifies against). The request body is drained **before** FIT verification — verification can block on a JWKS fetch, and the body must be off the socket before the sender's delivery deadline aborts the request. Maps `avi:jira:*` / `avi:confluence:*` events onto the native Jira/Confluence pipeline and publishes to `crewlet.notifications.inbound`. Self-generated events (an agent's own actions echoed back by Forge) are acknowledged and dropped. Jira Cloud and Confluence Cloud both ride this route and are served end to end — see the integration pages.

### Aborted deliveries (client disconnects)

Webhook senders enforce delivery deadlines and abort requests that respond too slowly. When a sender hangs up before the request body is fully read, the read fails part way: there is nothing to verify and nobody left to tell, so the receiver logs `webhook_body_unreadable` (`component=api.webhooks`, keyed by `path` and `error`) and still writes a `400` — a handler that returns without writing one answers `200`, telling the sender a delivery it abandoned was accepted. The aborted delivery is dropped, and whether it is redelivered is up to the sender's retry policy, so recurring `webhook_body_unreadable` warnings on a webhook path mean events are being lost because the API is answering too slowly.

The body is read **whole even when the request will be refused**, and bounded at 25 MiB (`body_too_large`, then `413` — every JSON surface answers a 413 with that one code) and at 30 s (see [Request timeouts](#request-timeouts)). Answering without draining leaves unread bytes in the socket and the sender sees a connection reset instead of the status — which for a `401` reads as "retry forever" rather than "your signature is wrong".

---

## Running

```bash
crewlet run -config crewlet.yaml -roles ingress -api-host 0.0.0.0 -api-port 8000
```

The API is served inside the engine's own process, over the engine's own store, broker and coordination plane. Its reads answer from that node's store and projection. Its writes are few and each is named above: the operator surface (`/operator/mcp`, `/operator/act`) writes the tracker and knowledge base through the tools a seat holds, `/config`, `/secrets` and `/setup` write the company's configuration and credentials, `/backup` and the `/work/retention` gestures act on the node and the log, and the webhook edge publishes inbound deliveries onto `crewlet.notifications.inbound`. It runs no agent itself — a seat's turn is the engine's.

See [Deployment](../guides/deployment.md) for how the API and engine run together, and the integration docs ([Slack](../integrations/slack.md), [Jira](../integrations/jira.md)) for webhook setup.
