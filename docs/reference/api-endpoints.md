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
  "message": "This deployment has no public address, so no webhook can be registered for it. Set the public base URL and run the pass again.",
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
  Never match on it: the wording is copy and can be improved at any time. A few
  refusals still build their body themselves and answer with the code alone —
  several under `/config` and `/secrets` — so a client renders `message` where
  it is present and its own line for the code where it is not.
- **Everything else is the detail** — the machine-readable facts about *this*
  refusal, as typed JSON beside the two reserved keys rather than nested under
  one: `config_path` and `hint` above, `fields` on an integration that is
  missing values, `current_revision_id` on a lost update, `problems` on a
  refused configuration document. Values keep their own types,
  so a count is a number and a list of located problems is a list.

`error` and `message` are RESERVED: a route's own detail can never displace
them, so a client that branches on the code cannot find it missing because a
route happened to use the same key.

Two shapes sit deliberately outside this envelope. A **WebSocket query answer**
is a frame rather than a response — `{"kind": "error", "id", "what", "error"}`
— and carries the code alone, because the client switches on the value and the
sentence for each code is one the dashboard already holds (see
[`WS /ws/stream`](#ws-wsstream)). And the routes that are not JSON at all —
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
| Every other write: `/config`, `/secrets`, `/setup`, `/budgets/reset`, `/backup`, the `/work/*` writes, `POST /operator/mcp` | `503` | Each one starts work or changes the company the drain is leaving. Refusing by default is what keeps a write route added later from slipping through a drain. |

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

## Routes

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness + the engine-health envelope (see [below](#the-health-envelope)). Stays `200` through a drain (see [During a drain](#during-a-drain)); use `/ready` to steer traffic |
| `GET` | `/ready` | Readiness for a load balancer: `503` while draining, before the first config revision applies, or on a `shed` or `stuck` posture, and `200` otherwise. A `503` names why in `reason`: `draining`, `unconfigured`, `shed` or `stuck`, in that order of precedence |
| `GET` | `/agents` | List agent roles, each merged with live state from the in-memory projection (including the in-flight `live_call`). [Human seats](../concepts/humans-in-the-org.md) are excluded — they appear only in `/org` with `"kind": "human"` |
| `GET` | `/agents/{id}` | Single agent — `role`, the live overlay (incl. `live_call`), and `llm_history`: the seat's finished phases newest first, capped at 50. `{id}` is the seat's **handle**, which is what every roster row carries as its `id`; a role name is accepted too |
| `GET` | `/agents/{id}/memory` | Durable memories (personal, episodic, counterparty, synthesized skills). Same `{id}` — the handle resolves to the derived agent id the diary is keyed by |
| `GET` | `/org` | The company's charter and its seat and unit tree, in an explicit public shape that carries no contact identity, email, credential or deployment setting (see [below](#get-org)). Human seats appear with `"kind": "human"` |
| `GET` | `/tools` | Registered tools, each tagged with the `source` that registered it — `builtin` or `mcp:<server>` (see [Where a tool comes from](../guides/tools-and-mcp.md#where-a-tool-comes-from)) — plus its behavioural `annotations`, where it `delivers`, and its `input_schema` (see [below](#the-tool-catalogue)) |
| `GET` | `/events` | Recent engine events from the event store (`limit` caps at 400; keyset-paged, see below) |
| `GET` | `/events/{event_id}` | Single event incl. payload |
| `GET` | `/events/trace/{trace_id}` | All events in one trace, oldest first, capped at 500 |
| `GET` | `/tokens/breakdown` | Per-stage / model / worker / agent / turn token-spend rollup |
| `GET` | `/tokens/series` | The same spend **with a time axis** — one bucket per hour or day, split into bands (see [below](#get-tokensseries)) |
| `GET` | `/schedules` | Configured role/unit schedules + next-run + recent dispatch ledger |
| `GET` | `/fleet` | Every live node, its roles and labels, seat ownership, singleton duties, and per-node config epoch. **Always needs a token** — it describes the deployment rather than the company, and the dashboard locks the screen that draws it (see [below](#get-fleet)) |
| `GET` | `/sandbox-runs` | Every detached [sandbox](../concepts/code-sandbox.md) run the engine still holds, read from the durable run record in the [coordination store](../concepts/coordination.md) (see [below](#get-sandbox-runs)) |
| `GET` | `/budgets` | Token caps, the durable shared counter they are enforced against, and which scopes are being refused (see [below](#get-budgets)) |
| `POST` | `/budgets/reset` | Zero the fleet's token counter. `?scope=` clears one (`org`, `agent:<id>`); its absence clears every one. **Takes `fleet:operate`** (see [below](#post-budgetsreset)) |
| `POST` | `/backup` | Copy this node's store and stream estate into `?dir=` **on the engine's host**. **Takes `fleet:operate`** — it writes every credential the company holds to a path the caller names (see [below](#post-backup)) |
| `GET` | `/integrations` | Every inbound surface, how it is wired, whether a signing secret is present, and what has arrived through it (see [below](#get-integrations)) |
| `GET` | `/work` | The company's own tracker: a filtered listing of work items, plus the last key number minted per project. Served only where `tracker.backend` is `native` — a company on Jira gets `404 unknown_query`, not an empty board (see [below](#the-native-tracker-and-knowledge-base)) |
| `GET` | `/work/retention` | What the state log is holding, what the trim concluded and which term is stopping it, every node's position, and what this node costs to replace. **Operator-only, reads included** (see [below](#get-workretention--what-the-log-is-holding)) |
| `POST` | `/work/retention/ack` | Publish an operator backup floor, for `backup_floor: operator` |
| `POST` | `/work/retention/evict/{node}` | Install the eviction gate on a node, so the trim can pass a floor it is pinning |
| `POST` | `/work/retention/readmit/{node}` | Lift it — the inverse commit rather than a delete |
| `POST` | `/work/retention/capacity` | Drive a log's byte-ceiling change as far as this node's mode allows |
| `GET` | `/work/retention/maintenance` | Where that window stands and what is holding it |
| `POST` | `/work/retention/maintenance/abandon` | Change what the operation is trying to reach, never the barrier it must cross |
| `POST` | `/work/retention/maintenance/exclude` | Record that a participant's process is stopped and holds no outstanding request |
| `GET` | `/work/retention/reanchor` | The live stream's own `created_at`, which a reanchor's confirmation has to echo |
| `POST` | `/work/retention/reanchor` | Adopt a recreated stream at the next generation |
| `GET` | `/work/views` | One container's **view strip**: the six every container has without anybody saving one, and whatever was saved beyond them. `?container=` takes the query grammar's own spelling (`workspace`, `project:ENG`, `unit:engineering`, `person:ana`). The strip is ordered by **the caller's own** personal views and pins, and there is no parameter naming whose: a strip is furniture a person arranges for themselves, and a parameter that could name somebody else's was a way to read their arrangement. A caller bound to no seat gets the shared strip |
| `GET` | `/work/catalogue` | The company's **vocabulary**: the task types a create may name and the workspace's custom-field declarations. `?archived=true` also lists what was retired. The types are the EFFECTIVE set — the six this build ships plus whatever the company declared, a declaration replacing a builtin of the same slug |
| `GET` | `/work/projects` | Every **project** work is filed into, with its `task_counts` — the maintained `open`/`done`/`closed` columns, never an aggregate per poll — its chart-owned unit and its lead. `?q=` narrows by a word in the key, the name or the purpose; `?unit=` to the projects one unit owns; `?archived=true` includes the retired ones; `?limit=` caps at 200, which is also the default. A set read, so it carries `complete` and its `incomplete` beside the read level |
| `GET` | `/work/projects/{key}` | One project in **full**: the six statuses with their labels, groups and descriptions; the effective types; the custom fields grouped by which type they apply to, required first, with the workspace ids this project **shadows** named; its tags; its default assignee, lead and owning unit. `?for_type=` narrows the fields to one type plus the ones that apply to every type. Unknown key answers 404 naming the nearest three |
| `GET` | `/work/activity` | The **activity feed** — one durable row per applied commit, quiet ones included, at any age with no live/archive boundary to cross. Ordered by the COMPOSED LOG POSITION rather than by any clock, so `?since=` and `?cursor=` are both positions written `<stream>@<generation>:<sequence>` — which is what lets a cursor span a reanchor with no gap and no repeat. `?task=` (by key, id or a FORMER key), `?container=`, `?kinds=`, `?actor=`, `?assignee=`, `?notified=`, `?from=`/`?to=` (RFC3339, bounding the AUTHORED instants), `?limit=` ≤200. `?q=` is an escaped `LIKE` over the excerpt and is REFUSED unless it names a task, or a project **and** a `since` inside 90 days |
| `GET` | `/work/my-work` | Everything one person is expected to look at, in seven bounded lists: `priorities` in the stored order, `assigned`, `asked_of_me` (each with the literal call that answers it), `checklist_items` (which live on other people's tasks and no assignee filter reaches), `collaborating`, `watching_recent` and `unblocked_recent`. `?handle=` is whose, and it **defaults to the caller's own seat** — see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for). Naming somebody else's takes whoever leads them, or `fleet:operate` |
| `GET` | `/work/inbox` | One person's **inbox**: the notices a change wrote to them, each naming the ONE [reason](../guides/work-tracker.md) of eighteen it found them under, whether it **asks** something or merely informs, whether it arrived only because nobody better was found, and their own read and snooze marks. Same scope rule as `/work/my-work`. `?unread=`, `?primary_only=`, `?include_snoozed=` (a snooze means *not now*, so they are hidden by default), `?reasons=` (comma-separated, refused naming the eighteen), `?limit=` ≤50, `?cursor=`, and `?since=` — a LOG POSITION written `<stream>@<generation>:<sequence>`, which is what `seen_through` reports back, never a bare sequence: the comparison is on the packed `(generation << 40) | seq`, so a sequence with no generation re-delivers everything after a reanchor |
| `GET` | `/work/people/{handle}` | One human's **own state**: their inbox (unread, read, snoozed, and which snoozes are now **due**), the order they mean to work in and who set it, and their pinned views. Same scope rule as `/work/my-work`, with the handle always named here because it is the path: your own seat's is yours, and anybody else's takes the same owner-or-lead rule. A person nobody has written yet answers the EMPTY state with `held: false`, not a 404 — every human starts this way and the first write is what creates the record |
| `GET` | `/work/{id}` | One item with its description, thread, history and links. `{id}` is either the key (`ENG-42`) or the id — a person holds the first and every internal link the second |
| `GET` | `/pages` | The company's own knowledge base: a filtered listing. Served only where `knowledge.backend` is `native` |
| `GET` | `/pages/{id}` | One page with its body, comments, revision metadata, children and ancestor breadcrumb. `{id}` is the id, or `CONTAINER/Title` — the title matches the way the fleet CLAIMED it, so case and runs of whitespace are ignored and `ENG/deploy runbook` reaches a page called "Deploy  Runbook" |
| `GET` | `/containers` | Every knowledge container this node knows about, with how many pages each holds. The engine materialises one per `space:` the org chart names, plus the two reserved ones, on every config apply |
| `POST` | `/work/items` `/pages` | **File an item, write a page** — and the rest of the [write surface](#the-human-write-surface): the same tools a seat and your own assistant hold, as the person you signed in as. Guarded, and absent on a company whose tracker or knowledge base is not native |
| `PATCH` | `/work/items/{key}` | Change an item — and its `/comments`, `/rank`, `/depend`, `/relate`, `/restore` and `/purge` beside it. See [below](#the-human-write-surface) for every route and the authority each takes |
| `POST` | `/auth/login` | **Sign in.** Login or address, password, and a second factor where one is held. **Unguarded** and throttled per source. Every failure answers one code at one deadline — see [below](#every-failed-sign-in-is-one-refusal). A node that cannot read the identity estate or record the session answers `503` — and **every `503` under `/auth` carries a `Retry-After`**, so a client can tell "ask again in a moment" from a node that is gone |
| `GET` | `/auth/config` | What a sign-in page needs to know before anybody has signed in: which backend, whether the first-operator route is still open, the password floor. **Unguarded**, and it carries **no user list and no count of people** |
| `POST` | `/auth/bootstrap` | **The first person.** Redeems a one-time code this node wrote to a file beside its store, 0600, and creates an operator carrying the whole `max_grants` ceiling — the one stated exemption in the authority model. The enrolment names the code as its authority, and the record honours it only while the code is live on the log and nobody else is enrolled, so a second caller racing the first is `409 bootstrap_closed`. **Unguarded**, and closed for good the moment anybody is enrolled |
| `GET` | `/auth/invite/{id}` | **Renders an invitation and never spends it** — a link is followed by mail clients prefetching, scanners and preview cards, and one spent by a GET is an account created for somebody who never saw it. **Unguarded**: holding the link is the credential. Answers the address it is for, who sent it, the password floor, and a `login` **proposed** from the address in the person grammar (`jane.doe@example.com` → `jane.doe`, `jane@example.com` → `jane.example`) for the form to pre-fill. Absent, redeemed and expired are one `410`, and every `410` on either invitation route is a **failed attempt counted against the caller's source** — the id in the link is the credential, so walking ids is guessing at one, and it is turned away at the same ceiling as a guessed password |
| `POST` | `/auth/invite/{id}` | Redeems it, conferring exactly the grants and reach whoever issued it decided — the enrolment names the invitation as its authority and the record refuses anything it does not cover, so a link spent or aged out between the form and the post is `410 invite_spent`. **Unguarded**. `{login, name, password}`, and the login is **required** — every person enrols with one, the name their changes are recorded under while they hold no seat. An absent login, or one outside a person's grammar (dotted, `jane.doe`), is `400` naming the rule and a login or address somebody already holds is `409` — without saying who, because a link is evidence of who the caller is and of nothing about anybody else. Only a record that could not land is `503`. **Retry until it lands**: the person a redemption creates is derived from the invitation, so every attempt names one person and a redeemer told their login is taken posts another. A link whose address somebody is already enrolled under is `410`, like a redeemed one. `POST /auth/bootstrap` takes `{code, login, email, name, password}`, accepts the code only while the log holds it outstanding (not withdrawn, spent or past its 24 hours), derives the founder from the code the same way, and answers its enrolment the same way |
| `GET` | `/auth/oidc/start` `/auth/oidc/callback` | The identity-provider round trip, with PKCE and a sealed 10-minute flight cookie so a login begun on one node finishes on another. **Unguarded** — a browser following a redirect carries nothing this engine issued. **Absent** where no provider is configured. Linking is explicit: a subject this estate holds no credential for is refused, never provisioned. A provider whose discovery cannot be reached is `503` with a `Retry-After`; a start this node's own configuration cannot make — a provider block that does not validate, no keyring — is `500`, because waiting does not fix it |
| `GET` | `/auth/session` | **Who you are**: your id, login, seat, kind, stage, grants, colleague level and whether the next sensitive action will ask you to confirm your identity |
| `POST` | `/auth/token` | Exchanges a **Tier A bearer** — presented as `Authorization: Bearer`, never a cookie — for a one-hour session cookie. The session **is the token**: it names the token's login, and every request re-composes it from the entry this node holds now — the entry's grants cut to the ceiling, the seat the identity directory binds the token to, stepped up by construction as the bearer is. Removing or renaming the entry ends it on the next request, and so do `POST /auth/logout/all` from it and `crewlet iam invalidate-all` |
| `POST` | `/auth/step-up` | Confirm who you are on a session that is already valid. The only route here that is **both guarded and throttled**: the caller is known, and unbounded retries against a known person is a password oracle with the enumeration already done. It answers a **fresh session cookie** and **ends the session it replaces** first — a close that does not land is `503` with a `Retry-After` and opens nothing, and a presented session that is no longer live is `401`. The replacement confirms the sign-in rather than repeating it, so it keeps the replaced session's absolute deadline and the grants its identity provider's groups conferred |
| `POST` | `/auth/totp` | Enrol a second factor. **Two requests**: the first answers a seed and stores nothing, the second presents a code derived from it — which is the only evidence the authenticator app works. Needs a step-up |
| `POST` | `/auth/totp/recovery` | Issue ten fresh single-use codes, retiring the old set. Answered **once**, in the clear; what is stored is their hashes, so a lost set is regenerated rather than recovered. Needs a step-up |
| `POST` | `/auth/logout` | End **this** session. The cookie is cleared whatever the write did — a logout that answered 503 would leave somebody looking at a signed-in page on a shared machine. It reads the session under either cookie name, as the guard does, and clears both |
| `POST` | `/auth/logout/all` | End **every** session you hold, by bumping your own revocation epoch — the one move that is immediate on every node |
| `POST` | `/auth/logout/{lineage}` | End **one named** session, which is how you sign out of a laptop you left somewhere from the browser you are using. The owner is read from this node's rows and compared against the caller the guard resolved; `fleet:operate` may end one they do not own |
| `GET` | `/viewer` | **Who is asking.** The caller's `login`, the `grants` they hold, and the seat the identity directory binds them to — its `handle`, `name` and `kind`, all empty for a credential nobody is bound through. An unbound credential is an **ordinary state**, not an error — a pipeline's token acts under its own login, and binding a person to a seat is a directory row rather than a different credential |
| `GET` | `/chart` | The company's **org chart** — its units, its seats, every `manages:` edge and every unit's lead — with the position the answer was read at. The **runtime half of every object is stripped** unless the caller asks for it AND may read it; the answer says which it got in `runtime`. **Always needs a token** (see [below](#chart--the-org-chart-auth-gated)) |
| `GET` | `/chart/units` `/chart/seats` | One half each, for a client that renders people constantly and the tree once |
| `GET` | `/chart/units/{key}` | One unit, what it directly holds, and its own history |
| `GET` | `/chart/seats/{handle}` | One seat, what it manages, and its own history |
| `GET` | `/chart/history` | The company-wide **reorganisation feed**, newest first: who moved, who was hired, which team was dissolved — quiet changes included |
| `PATCH` | `/chart/units/{key}` | Edit one unit's content. The **public half** is whoever leads that unit; a body carrying `runtime` takes `config:write` (see [below](#the-two-halves-of-every-object)) |
| `PATCH` | `/chart/seats/{handle}` | Edit one seat's content, on the same split — decided by whoever leads **that seat** |
| `POST` | `/chart/batch` | One **structural** change: create, move, set a lead, remove. One batch is one record, arbitrated against every other structural write in the company. Takes `config:write` |
| `POST` | `/chart/units/{key}/rename` `/chart/seats/{handle}/rename` | Change an object's **address**. The former one goes on resolving. Takes `config:write` |
| `POST` | `/chart/import` | Publish one revision's **complete authored structure**, keyed on the revision so a re-import is a no-op. Takes `config:write` |
| `GET` | `/chart/imports` `/chart/imports/{revision}` | Which revision this company's structure is running, and when it landed |
| `GET` | `/chart/check` | The **continuous report**: every way the chart and the applied settings disagree (see [below](#the-continuous-report)) |
| `GET` | `/company/export` | The chart as an authored **document**, whole and unstripped, for a round trip through a file. Takes `config:read` |
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
| `GET` `POST` `DELETE` | `/operator/mcp` | The company's own tracker and knowledge base, served over MCP to **your** AI assistant. **Always needs a token** — it files and moves work (see [below](#operatormcp--your-own-assistant)). Absent where the company runs neither native backend |

> **Auth.** Every route requires `Authorization: Bearer <token>`, reads
> included — `/ws/stream` too, and it accepts `?token=…` since browsers cannot
> set headers on a WebSocket. Only there: a token in the query string of any
> other route authenticates nobody, because a URL lands in proxy logs and
> browser history. Never guarded: `/health`, `/ready`, `/webhooks/*`,
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
> token goes on appearing to work. The practical consequence is that a stale
> token in a browser breaks a dashboard that would have connected with none at
> all; the dashboard detects that and offers to forget it (see below).
>
> **`GET /ws/stream` without an `Upgrade` header** answers `401` for a refused
> credential and `426 Upgrade Required` for an accepted one. That pairing is a
> contract, not an accident: a browser is told nothing about why a WebSocket
> handshake failed — no status, and no close code, because a connection that
> never opened sends no close frame — so the dashboard re-asks over plain HTTP
> to tell "your token is wrong" from "the engine is down". Without it a reader
> holding a stale token sees "retrying" for ever.
>
> **Two credential shapes reach every route, and the header wins.** A Tier A
> bearer in `Authorization` is the deployment's own machine credential; a
> `crewlet_session` cookie is a person who signed in. Both resolve to the same
> principal, so no route knows which arrived. When a request carries both, the
> **header** decides — a browser sends its cookie whether or not the caller
> meant to, and an `Authorization` header is only ever there because somebody
> put it there. A request presenting a *wrong* header therefore stays anonymous
> rather than being upgraded by whatever cookie is in the jar.
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
> whatever credential it carries. The match is `api.external_url`'s own origin
> plus every `api.auth.allowed_origins` entry; reads and the exempt routes are
> never refused for theirs. See
> [Identity and Access § A cross-site write is refused by its Origin](../concepts/identity-and-access.md#a-cross-site-write-is-refused-by-its-origin).
>
> **Every `/webhooks/*` route fails closed.** They are exempt from the bearer
> token because each verifies its provider's signature instead — so a route
> whose secret is not configured has nothing to verify with, and answers `503`
> with `Retry-After` rather than accepting the delivery. The sender retries and
> the delivery flows once the secret is set; nothing is discarded, and nothing
> unsigned is ever recorded, published, or shown on the dashboard.

Plus the surfaces whose reads are as sensitive as their writes: [`/config/*`](#config--live-config-management-auth-gated), [`/secrets/*`](#secrets--the-companys-credentials-auth-gated), [`/setup/*`](#setting-an-integration-up), [`/chart/*` and `/company/export`](#chart--the-org-chart-auth-gated), and [`/operator/mcp`](#operatormcp--your-own-assistant). The list of which credentials a company has not configured yet is a map of what to attack, and so is the shape of the company itself.

### Every failed sign-in is one refusal

`POST /auth/login` answers `401 sign_in_refused` for every way it can fail — a
login nobody holds, a wrong password, a person suspended, a person removed, a
second factor that does not check out — at the same wall-clock instant,
measured from when the request arrived.

**Both halves are needed.** A code that distinguished the arms would make the
timing pad pointless, and a delay that distinguished them would make the single
code pointless. A miss pays the same argon2 verify a hit does, through a
fixed-cost decoy, because otherwise the *absence* of that cost is the answer.

What that buys is that this surface is not a roster: a caller cannot learn who
works here, nor test a list of addresses against it.

The exceptions are specific, and each discloses nothing the caller did not
already have:

| Code | Why it is safe to be specific |
|---|---|
| `throttled` | Keyed on the **source**, not the subject. A stranger learns they are rate-limited, which they already knew. Keyed on a login it would be an oracle — "this account exists and I can lock it" |
| `second_factor_required` | Reached only by somebody who already passed the first factor, so it discloses nothing to a stranger — and without it a client cannot tell "your password is wrong" from "now type your code", which are different screens |
| `bootstrap_closed` | Says this company has started, which whoever can reach an unstarted one would find out by trying |
| `invite_spent` | Read by somebody holding the link, which is already evidence it was issued to them. One code for redeemed, withdrawn and expired, because the remedy is the same and telling them apart would say "already used" to somebody whose link merely aged out |

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
`unnamed` — and `grants` are the capabilities any **one** of which would have
admitted this caller for this object. An empty `grants` is an answer rather
than an omission: no capability would, and what is missing is a relation the
chart does not hold. The [human write surface](#the-human-write-surface)
answers its own refusals `403 forbidden` in its tools' wording instead.

A request the node **cannot decide** — it is behind its chart log, or holds no
company yet — is `503 unavailable` with a `Retry-After`, never a `403`: a lead
told they lead nobody goes looking for an authority they already hold.

| Grant | What it reaches |
|---|---|
| `state:read` | The company's working state: `/agents`, `/org`, `/tools`, `/schedules`, `/budgets`, `/sandbox-runs`, the reads under `/work/*` and `/pages/*`, `/containers`, `/viewer`, `/stream/snapshot`, `/tokens/*`, `/ws/stream` |
| `audit:read` | The record of what happened: `/events*`, the socket's `event` push and the snapshot's `events` section, any seat's `/agents/{id}/memory` and `/agents/{id}/conversations`, the turn, phase, trace and A2A-channel questions on the socket, and `/iam/audit`. Separate from `state:read` because a prompt and a tool argument are the company's most sensitive read |
| `config:read` | Every read under `/config*`, `/company/export`, `/integrations`, the org chart's **runtime half** (`/chart?runtime=true`) — a seat's model chain, its credentials, its sandbox cell and its `mcp_env` — and the `/setup` and `/secrets` **listings**: they carry no values and still say which credentials a company holds, which it has not set, and when each last changed |
| `secrets:read` | Revealing a credential's value: `GET /secrets/{name}?reveal=true`, which takes `config:read` as well — the value's grant on top of the row's |
| `people:manage` | `/iam/*` — inviting somebody, changing what they carry, suspending them, revoking their sessions, resetting a second factor, removing them. **The grant that can grant**, and it bounds itself: a caller may not confer a grant they do not hold |
| `work:write` | Filing and moving work — the [write surface's](#the-human-write-surface) item routes — and `/operator/mcp`'s write half. Some of those verbs also ask a RELATION: re-routing, a project's policy and taking an item out of circulation are its project lead's |
| `knowledge:write` | Writing the company's own pages. A rename, the trash and a restore are also the container's lead's; see [the write surface](#the-human-write-surface) |
| `config:write` | Every write under `/config*`, `/chart/batch`, the rename and import routes, and `/setup`'s writes — with `secrets:write` as well wherever the write seals a credential — and, on top of the page's own rule, the create, save, rename, trash, restore and purge of a page in the tool-skills container (a comment on one is not gated: a remark is not the skill) |
| `secrets:write` | `PUT`/`DELETE /secrets/{name}` and `POST /secrets/rekey`, and on top of `config:write` a `/setup` write that seals a credential: a submission carrying one, a provisioning pass, a GitHub App |
| `fleet:operate` | The deployment rather than the company: `/fleet`, every `/work/retention*` route (the maintenance status and the reanchor value included), `/backup`, `/budgets/reset`, and the two purges (`/work/items/{key}/purge`, `/pages/{id}/purge`) — which no seat may make whatever it holds. It is also the **admin path** of every relation rule: a holder is admitted where a lead or an owner would be |
| `sandbox:run` | Starting a coding run |

A question asked on the socket is decided by the same declaration the REST
route is — one registry, both transports — so there is no way round a grant by
choosing a channel.

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

These matter because the operator token the dashboard stores lives in the
browser's storage for this origin, and the two landing pages are unauthenticated
pages on that same origin that render values from their query string. A policy
is per response, so each page carries its own: the dashboard runs only the
bundle it was built into, and a landing page runs only the style and script the
engine wrote into it. No response may be framed by another site.

`form-action` on the dashboard allows `https:` as well as `'self'` for one flow:
creating a seat's GitHub App posts the app manifest as a form to the code host,
which is `github.com` or the GitHub Enterprise Server base the company
configures. A reverse proxy in front of the engine should pass these headers
through unchanged; one that adds its own `Content-Security-Policy` produces two
policies, and a browser enforces both.

Read-side handlers live in the `internal/api` package (one module
per domain — `agents`, `events`, `tokens`, `org`, `fleet`,
`sandbox_runs`, `budgets`, `integrations`, `stream`, `webhooks`,
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

#### The two halves of every object

A unit and a seat each carry a **public** half — a name, a purpose, a goal, who
somebody manages — and a **runtime** half: a seat's model chain, its
credentials, its sandbox cell, its worker grants, its schedules, its `mcp_env`.
The second is the company's configuration under another name, and writing it is
equivalent to shell on every engine host, because a stdio MCP server is
`exec.Command` with the config's command.

So the two are decided differently, and the **payload picks the question**:

| What the body carries | Who may write it |
|---|---|
| the public half alone | whoever **leads that object** — the unit's lead for a unit, the seat's lead for a seat |
| anything under `runtime` | `config:write`, the company's own grant |

A body refused on the second question answers exactly as a route refused at its
pattern does: `403 unauthorized` with `reason` and the `grants` that would have
admitted it — here `["config:write"]` — and a `503 unavailable` with a
`Retry-After` when this node cannot decide.

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

The same rules are enforced a second time **inside the domain**, against the
grants the authoring party holds — so a surface that skipped its own check
still cannot write a seat's credentials, and a write that would CLEAR the
runtime half is a privileged write too (a content record is full post-state, so
omitting the half sets it to empty).

#### Structure is neither

A create, a move, a lead change and a removal go through `POST /chart/batch`,
and they take `config:write` whoever leads the team. The domain serialises
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

A batch whose operations are **all removals** is published as a removal record
instead; one that mixes the two is refused, because a removal installs a gate
and a record that installed one for some of its objects and not others would
make "does this install a gate" a question about a payload.

#### What a write answers

| Outcome | Status | What to do |
|---|---|---|
| `applied` | `200` | The record is durable **and this node has applied it**, so the next read here sees it |
| `pending` | `202` | Durable at the position in the body; every node will apply it, this one has not yet. Read at that position to see it |
| `unknown` | `503` | Nothing can be established from this node. Retry with the **same** operation id — the body carries it as `op_id`, and the route reads it back from `Idempotency-Key` |

Retrying an `unknown` under a *fresh* id would write the change twice if the
first had in fact landed, which is the one thing the operation ledger exists to
prevent.

A refusal by the chart's own rules is `400`, a contention another writer won is
`409` (re-read and write again), and a node that cannot decide **authority** is
`503` rather than `403`: a node that is booting or behind the log cannot say who
leads a unit, and `403` would send somebody to ask for an authority they already
hold.

#### A rename keeps the old address working

`POST /chart/units/{key}/rename` with `{"to": "..."}` changes an object's
address. A key is not an identity — the row is — so the former address goes on
resolving until something else claims it, and a `manages:` entry somebody wrote
last year still finds the seat it named. Every read carries `former_keys` /
`former_handles` so a client rendering a stale reference can say **why** it
still works rather than reporting it broken.

#### The continuous report

`GET /chart/check` answers one evaluation over the two halves of the running
company: the chart this node holds and the settings epoch it has applied.

Nothing can refuse these at a write, and that is the point rather than a
limitation — the two halves are written by different people at different times,
so every finding is reachable through two writes that were each correct when
they were made:

| Kind | Severity | What it means |
|---|---|---|
| `provider_unknown` | error | A seat's model chain names a provider the settings do not declare. The seat resolves to **no model at all** |
| `sandbox_unconfigured` | error | A seat's code gate is open on a company with no sandbox backend |
| `worker_unknown` | warning | A seat's `workers:` narrowing names a template that is gone, so it narrows to fewer workers than the list suggests |
| `reference_dangling` | warning | A `manages:` entry, a unit's lead or a seat's unit resolves to nothing |
| `seat_unheld` | warning | A human seat **nobody in the identity directory is bound to**, so no person can sign in and act as it — work routed there waits for somebody who cannot arrive. **Skipped, not answered,** on a node that runs no identity domain: its copy of that estate is legitimately empty, so reading it would report every human seat in the company |
| `seat_unreachable` | warning | A human seat with no contact identity, so nothing addressed to it reaches anybody on the chat surface this company runs. Validation **admits** such a seat — a person who works only through the dashboard has no chat account to declare — so this report is the only place it is named, and a warning rather than an error because the state is legitimate. Independent of the above and with a different remedy — a seat can have either without the other |

The **same** evaluation is summarised on `/health` under `consistency`, so a
gauge, a probe and this screen can never disagree about whether something is
wrong. It does **not** move `/health`'s `status`: a company referencing a
provider somebody deleted is a company with a problem, not a node with one, and
taking a node out of rotation over a configuration typo would turn one broken
seat into an outage.

`evaluated: false` means this node could not evaluate at all — it holds no
chart view, or has applied no settings epoch. Check it before the count:
`findings: 0` from a node that read nothing is the most misleading answer this
surface could give.

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
| `POST /iam/credentials[?person=]` | the person themselves or `people:manage`; never a request presenting a machine token |
| `DELETE /iam/credentials/{id}[?person=]` | the person themselves or `people:manage`; a machine token revokes machine tokens only |
| `POST /iam/invalidate-all` | `fleet:operate`. Ends every session **and every machine token** |
| `GET /iam/check` | `people:manage` or `audit:read` |
| `POST /iam/bootstrap-code` | `people:manage`; refused once anybody can administer this company |
| `GET /iam/audit` | `audit:read` |

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

`applied` is `200` and means this node has the change, so the next read *here*
sees it. `pending` is `202` with the position, and means the record is durable
and every node will apply it while this one has not yet — read at that
position to see it. `unknown` is `503` with the op id, and the only safe retry
is the **same** one: send it back as `Idempotency-Key`, because a fresh id
would defeat the ledger that makes the retry safe.

A request **without** an `Idempotency-Key` is a new operation every time, with
an op id of its own. Two different edits of one person, a second "end every
session", or a second company-wide invalidation are two operations, and an op
id derived from the object alone made the broker collapse the second into the
first inside its duplicate window — acknowledged, and never applied. Send a key
when you mean a retry, and only then.

A lost race on an address, a login or a seat is `409` **naming who holds it**.
An authority refusal is `403` and will never land however often it is retried.
A login that is absent or outside its holder's kind is `400` — `POST
/iam/people` requires one for a person as for a machine, because it is the
name their changes are recorded under while they hold no seat: a person's is dotted (`jane.doe`)
and a machine's is coloned (`ci:release`, or `token:<id>` to bind a Tier A
token), checked on a create and on a rename alike — and so is a `kind` other
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
The body is `{"label", "expires_in_days", "grants", "colleague"}`, every field
optional: omitted, the token carries every grant its owner holds that a token
may carry, at the owner's own reach, for 90 days. The answer is `201` with
`{"id", "person", "token", "grants", "colleague", "expires_at", "position"}`.

| Answer | When |
|---|---|
| `400 bad_params` | No owner: a Tier A token owns no machine tokens, so it names one with `?person=` |
| `400 invalid_body` | An expiry in the past or more than 365 days away, a label past 128 bytes, a reach that is no level |
| `403` | The request presented a machine token; a grant the owner does not hold, or `secrets:read` / `people:manage`; a grant the caller does not hold; a reach wider than the owner's; an owner who may not act |
| `404` | Nobody by that id |

**`Idempotency-Key` is ignored here, deliberately.** A retry that landed once
would hand back the first attempt's record — whose secret was shown to nobody
— beside this attempt's value, a token that verifies against nothing. A mint
answered `unknown` is retried as a new mint, and the one that may have landed
is a token nobody holds, which expires.

Presented as `Authorization: Bearer cwl_pat_…`, the token acts as its owner —
their seat if they are bound to one — carrying the grants it was minted with
that the owner **still** holds, cut to the node's ceiling. It answers `401`
where the node knows it is no good and `503 identity_unavailable` where the node
cannot tell, including a node that has not yet applied the mint. See [Machine
tokens](../concepts/identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts).

#### An edit moves a login or a seat, the new one first

`PATCH /iam/people/{id}` is a sequence of records — a login and a seat each
arbitrate on their own subject, and the person's document and stage on the
person's — so it is ordered by what a refusal leaves behind. Everything the
surface can judge alone is refused **before the first record**: a `login` of
`""` (a login is never cleared, only changed), a `stage` or `colleague` level
this build cannot name, and a `reason` past 256 bytes. A new `login` or `seat`
is then **moved**: the new one is claimed first and the old one released after,
so a login its holder's grammar refuses, a seat the chart does not hold, or
either one somebody else holds is refused with the person exactly as they were.
`seat: ""` unbinds.

#### Values that are shown once

`POST /iam/invitations` answers the invitation URL and `POST /iam/credentials`
answers the token. Neither is stored and neither can be read back: what the
estate holds is the invitation's id (which *is* the verifier — holding the
link is holding the id) and a SHA-256 of the token. An invitation an
administrator lost is re-issued rather than recovered.

`POST /iam/bootstrap-code` answers the **file path** and never the value. The
code is written `0600` beside the store, and reading it needs shell on that
host — which is the point. Re-issuing withdraws every outstanding code first,
so exactly one is live afterwards.

#### `GET /iam/people` pages on a key the applier writes

A person's id is a uuid7, so ordering by it is creation order and `next` is
the last id of the page. Nothing sorts on a name or an address: both are
ciphertext in every row, so ordering by one would be ordering by the
ciphertext.

`?q=` narrows on the **login and the seat**, which are the only two identity
values this estate holds in the clear. A search over names would have to open
every person in the company to compare one, which is a fan-out of
coordination reads per keystroke.

A row whose key a removal destroyed renders as `removed`, and ciphertext this
deployment's keyring cannot open renders as `sealed`. They are different
states with different remedies — one is finished, the other is a keyring
somebody still has — and neither is an outage.

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
`person_without_credential`, `binding_dangling`, `identity_shredded` and
`grant_clamped_by_ceiling`.

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

#### `GET /iam/audit` pages by position, never by time

Two nodes' clocks are compared nowhere in this engine, so `since` and `before`
are log positions. A caller holding a timestamp passes `at=` instead and the
route resolves it **once**, against the rows' own instants. The answer carries
the position this node had applied when it answered, so a reader can tell a
quiet directory from a lagging node.

### `/config/*` — live config management (auth-gated)

Every `/config/*` route takes a grant: `config:read` for the reads below and `config:write` for the writes, whatever the credential — a Tier A token, a session, or the development principal. A caller without it is refused `403 unauthorized` naming the grant (see [Which grant a route needs](#which-grant-a-route-needs)). See the [Configuration concept doc](../concepts/configuration.md#auth) for the full auth model.

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
(`roles[0].integrations.slack.bot_token`), the same one a validation failure
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
- **Arrays replace.** RFC 7396 cannot address a list element, so `roles: [...]` in a patch replaces the whole roster. Editing one seat is what [`PATCH /chart/seats/{handle}`](#chart--the-org-chart-auth-gated) is for, on a surface this one refuses a chart body outright; inventing a list syntax here would give two answers to one question. What a replacement does not remove is a field this build cannot represent: see [Fields a newer build wrote survive every write](#fields-a-newer-build-wrote-survive-every-write).
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

`PUT /config?dry_run=true` and `PATCH /config?dry_run=true` are the same request, checked in the same order, that store, activate and publish nothing. The dashboard's organization builder sends one on every edit, so a check is always exactly the write a save would send.

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
| `message` | The failure's whole line, exactly as it appears in `detail`. A duplicate name is one line naming every entity and one problem beside each, so there can be more problems than lines |
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
| `If-None-Match: *` | writes | Proceed only if **nothing** is configured, on this node **or anywhere in the fleet**; `412 already_configured` otherwise, naming the revision it lost to |

The bare revision id is accepted wherever an `ETag` is, unquoted, because this surface shipped that form before it had entity tags. `If-None-Match: *` is the only create-only precondition, and every `If-Match` value other than `*` is an entity tag, matched against the active revision and nothing else.

Independently of any header, every write names the revision it derived from as the new revision's parent, and the activation is a compare-and-set on that parent — so a lost update is refused **whether or not** the caller sent a precondition. See [Concurrent writes](#concurrent-writes).

**Nothing under `/config` is cacheable.** Every response the surface writes carries `Cache-Control: no-store`: reads, `304`s, writes, refusals and error bodies, and the `404` and `405` it answers for a path or method it does not serve. A body here is the company document, with its contact identities and the `${VAR}` name behind every credential, and a stored copy would outlive the session and the token that read it. Revalidation still works, because it never depended on a cache: a client that wants a `304` sends `If-None-Match` with the `ETag` it kept.

#### Per-entity read and write

Four collections readable, **two writable**:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config/{kind}/{id}` | One entity, redacted, with an `ETag`. **The body is the entity itself**, so it goes straight back into the `PUT` |
| `PUT` | `/config/llm-providers/{key}` | Replace one named LLM provider |
| `PUT` | `/config/mcp-servers/{name}` | Replace one MCP server entry |
| `PUT` | `/config/roles/{handle}` | **`400 chart_not_writable_here`** — a seat is the [org chart](../concepts/chart-domain.md)'s, not the settings'. The refusal names the routes that do the job: `PATCH /chart/seats/{handle}`, `POST /chart/batch`, `POST /chart/seats/{handle}/rename` |
| `PUT` | `/config/units/{key}` | **`400 chart_not_writable_here`** — likewise for a unit, naming `PATCH /chart/units/{key}`, `POST /chart/batch` and `POST /chart/units/{key}/rename` |

Writing a seat or a unit goes through [`/chart/*`](#chart--the-org-chart-auth-gated). A fresh deployment's chart is also seeded from its company file at boot; see [the boot seed](../concepts/control-plane.md#the-boot-seed).

Any other method is `405` with an `Allow` header naming `GET, PUT`. There is no `DELETE` — removal is a full-document edit, for the reasons below.

`roles` and `units` stay **readable** because a revision written before the
chart's split still carries both, and an operator repairing one has to be able
to see it. The write is refused rather than answered `404`: on such a revision
the splice would succeed and store another revision carrying a chart, which
every node then refuses to apply — so the failure would arrive at the next
restart instead of at the request that caused it.

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
- **A `PUT` never creates.** An id nothing carries is `404 no_such_entity`, not
  a new entity: naming one that is not there is far more often a typo than an
  intent to add one, and creating through this route would grow the company
  without the caller ever seeing the document they changed. Add through
  `PUT /config`, which shows the whole thing. The id is looked up before the
  body is read, so a mistyped one is a `404` whatever the body holds.
- **The id in the path is the identity, and a `PUT` never renames.** A body
  whose own identity disagrees with the path is `400 identity_mismatch`, not a
  move: nothing that points at the old identity travels with the splice. A
  provider's key is what every seat's `llm:` chain names, and an MCP server's
  name is the key every `mcp_env` block is keyed on and the prefix its tools
  carry. Send the identity back unchanged and change whatever else you like;
  rename through `PUT /config`, where what has to move with it is visible.
- **The same summary and `If-Match` rules apply**, and a node with no
  active revision answers `409 no_active_revision` — there is nothing to splice
  into, and building a company out of one seat is not what this route is for.

### `/secrets/*` — the company's credentials (auth-gated)

Every `/secrets/*` and `/setup/*` route takes a grant, reads included: the
listing alone says which credentials a company holds and when each last
changed. Every node serves them, because every node opens the fleet's
[coordination store](../concepts/coordination.md) that holds the rows.

| Route | Grant |
|---|---|
| `GET /secrets`, `GET /secrets/{name}` | `config:read` |
| `GET /secrets/{name}?reveal=true` | `config:read` **and** `secrets:read` — decided before the store is read, so a caller without the second is refused alike for a name that exists and one that does not |
| `PUT /secrets/{name}`, `DELETE /secrets/{name}`, `POST /secrets/rekey` | `secrets:write` |
| `GET /setup/integrations`, `GET /setup/integrations/{kind}`, the two `runs` reads | `config:read` |
| `POST /setup/integrations/{kind}/inputs` | `config:write` — and `secrets:write` as well when the submission carries a credential, supplied or to be minted |
| `DELETE /setup/integrations/{kind}`, `POST /setup/integrations/{kind}/check` | `config:write` |
| `POST /setup/integrations/{kind}/provision`, `POST /setup/integrations/github/app` | `config:write` **and** `secrets:write`: a writing pass is handed a sink that mints and seals, and an app's private key is sealed on its way back |

**Connecting is `config:write` and `secrets:write`.** `/setup` performs no
write of its own — a credential goes through the store `/secrets` serves — so
a caller who could not write a credential there cannot write one here either.

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
| `GET` | `/secrets` | Every stored name with its `key_id`, `updated_at`, `updated_by` and `source`. **Never a value** |
| `GET` | `/secrets/{name}` | The same fields for one name. `404 not_found` when it is unset |
| `GET` | `/secrets/{name}?reveal=true` | **Break-glass.** The decrypted value, `Cache-Control: no-store`, logged by name against the authenticated operator |
| `PUT` | `/secrets/{name}` | Store or rotate one value. **The request body is the value**, raw bytes, up to 64 KiB. `?source=` records provenance (default `api`). `400 invalid_name` when the name is not an environment-variable name |
| `DELETE` | `/secrets/{name}` | Remove one value. `200` either way, with `{"removed": true\|false}` |
| `POST` | `/secrets/rekey` | Re-seal every record not already under this node's `secrets.active_key_id`, answering the names it moved. `?key_id=` is refused with `409` when it names a different key |

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

**A node with no `secrets.keys` answers `503 no_keyring`** on every route that
seals or opens, pointing at `crewlet secrets keygen`. The store has no
plaintext mode; refusing is the only alternative to holding credentials in the
clear.

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
- `404 Not Found`: a revision that is not there, `no_active_revision` on a read before the first write, or `no_such_entity` on a per-entity write naming an id the active revision does not carry
- `409 Conflict`: `revision_advanced` (a stale `If-Match`, or a race with a concurrent writer) or `no_active_revision` (a `PATCH` or a per-entity write on an unconfigured node, or a reload)
- `412 Precondition Failed`: `already_configured` when `If-None-Match: *` meets an active revision, or `no_active_revision` when `If-Match` names a revision and none is active
- `415 Unsupported Media Type`: `unsupported_patch_media_type` when a `PATCH` body is a patch format other than a JSON Merge Patch, with `Accept-Patch`
- `503 Service Unavailable`: `draining` when the node has been told to stop, with a `Retry-After` — see [During a drain](#during-a-drain)

### The `config_audit` query

Recent revision metadata for the dashboard's Configuration screen. **A query, not a REST route** — there is no `GET /config/audit` in this build; the screen asks the query channel for `config_audit` and gets the same revision records `GET /config/revisions` serves, in a wrapper object.

```
query config_audit { "limit": <N> }
```

| Parameter | Default | Range | Description |
|-----------------|---------|-------|-------------|
| `limit` | `50` | `1..500` | Number of revisions to return, newest first. An out-of-range number is CLAMPED to the range; only a non-numeric value is `400 invalid_limit`. |

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
    { "path": "roles[3].handle", "kind": "added", "to": "qa" }
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
such as a duplicate seat or unit name stored before the rule existed, reloads:
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
  applies it. A revert to a revision sealed under a key this node does not hold
  answers `409 unreadable_revision`.

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
- **A list member is matched by identity, not by position**: a seat by its
  handle and a unit by its name anywhere in the document, so a seat moved to
  another unit keeps its settings; an MCP server or a sandbox setup step by its
  name within its own list. An identity held twice in the stored document, or
  empty, matches nothing, so no seat's setting reaches another. A member of a
  list with no identity (a schedule, for example) keeps nothing the write
  replaced.
- **A `PATCH` replaces only what it names.** Everything it does not name is
  stored exactly as it was, so a list the patch leaves alone keeps every
  member's settings, identity or not, and a schedule loses a newer build's
  settings only when the patch replaces the seat or unit list that holds it.
- **A reload and a revert store the document exactly as it was stored**, since
  neither changes it.
- **A renamed seat or unit is a new identity**, so it keeps nothing of the old
  one's unknown settings.

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
`409 revision_advanced`, `409 literal_in_config`, `409 no_active_revision`,
`503 no_keyring`.

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
accounts.

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
a minute later cannot disagree, and the Integrations screen updates with no
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

Those write through the **entity route** rather than a merge patch, because a
merge patch replaces an array wholesale and patching the roster to change one
seat would delete every other one. The engine addresses the seat by its handle,
which is its identity rather than its position, and everything the submission
did not send stays exactly as stored.

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
  "state": "<signed token naming the seat>"
}
```

Those three values are what GitHub's manifest flow takes: the browser POSTs the
`manifest` as a form field to `action_url`, carrying `state` on that URL so the
redirect can be tied back to the seat that started. `action_url` is the
organization's own app registration page whenever
`integrations.github.provisioning.org` names one, because an app registered
under a person's account cannot be installed on the organization that owns the
repositories.

Refusals: `400 bad_body`, `400 seat_required`, `409 no_active_revision`,
`404 no_such_seat`, and `409 no_public_url` when
`api.external_url` is unset. The last one matters more than it
looks: an app is created with its delivery, redirect and setup addresses baked
in, and only a person at GitHub can change them afterwards, so creating one
now would mean creating it again later.

**`GET /webhooks/github-app`** is where GitHub returns the browser, twice. It is
unauthenticated because a redirect carries no engine credential; the `state` is
what stands in its place, and it is validated before anything else happens.

- With a `code`, the engine converts the manifest, **seals the app's private
  key and webhook secret first**, then records `app_id`, `app_slug` and a
  `${VAR}` pointing at the sealed key on the seat, through the same per-entity
  config route [per-seat setup](#per-seat-setup) uses. `installation_id` is
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

Every named read route is an **adapter**, never a second implementation: it
resolves its path values and hands them to the same answer the socket's query
channel reaches. The generic form `GET /query/{what}?a=b` reaches the same
answers by name and is what the socket's own frames map onto, so the two can
never drift. A question whose source this node lacks — an event log, a
schedule ledger — is left *unregistered* rather than answered empty, so its
route replies `404` with a JSON error code rather than a bare mux miss.

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
payload attached is the query that makes an activity screen slow.

### A seat's LLM history

`llm_history` is the seat's **finished** phases, read from the event store —
one row per `agent_phase_completed`, newest first, capped at 50. The call
*in flight* is not in it; that is `live.live_call`, which comes from the
projection, and the two are different sources on purpose: the store holds what
completed, memory holds what is happening. A screen renders both with one
renderer, so each history row carries the same fields a live one does —
`turn_id`, `phase`, `iteration`, `model`, `response`, `tool_executions`,
`round_narration`, `partial_round`,
`total_tokens`, `cost_usd` — plus the envelope's `timestamp` and `failed`.
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
| `org` | The same public projection [`GET /org`](#get-org) answers: the charter, root-level `roles` and `units` nesting to any depth, with only the public fields of each |
| `tools` | The catalogue this node serves, each entry tagged with the `source` that registered it — `builtin` or the MCP server's name. Empty on a node with no active revision, which has no catalogue yet |
| `events`, `sandboxes`, `tokens`, `budget`, `health` | The live projection: what has happened |

A seat carries `state: "idle"` when **this node** is serving it. A seat it
does not hold carries no state at all and the dashboard reads that as
`offline` — which is right for a seat nothing has claimed, and is this node
declining to claim knowledge of a seat a peer may be running. [Fleet](#fleet-sandbox-runs--schedules)
answers "who holds what" from the lease table, which is the one place that
knows. The live overlay merged on top replaces that state only once an event
says what the seat is doing (a spawn, a phase, a turn ending): a token meter
report names every capped seat whether or not anything runs it, so it carries
no state at all.

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
reader counts is `round_num + 1`, and `-1` is neither a round nor a missing
value — it is a turn that has begun and is waiting.  A surface drawing it
resolves both through `lib/seats.ts`'s `roundLabel`, which answers "starting"
for the sentinel and `round N+1` otherwise; the roster drew a bare dash and
two of five working seats read "round —" with nothing saying why.  `agent_turn_progress` is *stream-only*
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

**The projection is seeded from the event store when the process starts**,
after the broadcast subscription is attached and before the HTTP listener
binds. Two bounded reads, each bound the projection's own: the newest 400
persisted events for the feed, and the newest 8 000 phase records inside the
24-hour spend window. Without it every one of these surfaces started at this
process's boot, so a restart, a deploy or a node joining a fleet showed an
operator a company that had apparently done nothing beside a store that
said otherwise. An event that arrives both ways is recognised by its id and
listed and counted once, in either order: the stream can deliver it before
the read, and the read can find a row the publishing node wrote inline before
the stream delivered it. History is ordered behind the live rows it predates.
On a fleet the seed is what
**this node** published (the event store is per node), while everything
after the boot is the whole company's. A read that fails is logged as
`live_projection_not_seeded` and costs the history, never the start-up:
`GET /events` and the `tokens` query still read the store directly.

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
  "health":    { /* status, in_flight and shutting_down, from the
                      health envelope described below */ },
  "agents":    [ { /* /agents row: live state + budget meter + live_call (the
                      in-flight LLM call, or null between turns) +
                      last_error (the phase failure that stopped this
                      seat, or null) */ }, ... ],
  "events":    [ { /* recent event row, newest first — payload-free, plus
                      a `failed` boolean */ }, ... ],
  "sandboxes": [ { /* in-flight detached coding run */ }, ... ],
  "tools":     [ { /* one catalogue entry — see The Tool Catalogue below */ } ],
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
*is* a failure (`sandbox_run_failed`, `llm_unavailable`, `budget_exhausted`,
`turn.guard_breach`).  Deciding it once, here, is what lets a dashboard mark
failures without re-deriving them from a type list of its own.

The flag survives a restart: the event-store writer stamps a `failed` tag on
those events, and the projection reads it back when it seeds its feed from the
store at startup. The store's listing (`EventLog.List`) deliberately never
selects the payload column, so without the tag every historical failure would
read back as a success.

### The health envelope

One builder (`App.health`, `internal/api/health.go`) answers `GET /health` and
the socket's `stream` query, so the two cannot disagree about whether the
engine is healthy. The 5-second `health` push and the snapshot's `health`
section are cut from the same read but carry only `status`, `in_flight` and
`shutting_down`, which is what every open tab receives on every tick; a screen
that needs the rest of the envelope asks the `stream` query for it.

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
  "in_flight": 2,
  "shutting_down": false,
  "posture": "serve",
  "applied_epoch": 41,
  "seats": ["ceo", "cto"],
  "domains": ["tracker", "vectors", "pages"],
  "unproven_seconds": {"eng": 312.5}
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
| `event_history_seconds` | How far back the event log can be read — the hard bottom of [paging](#paging-the-event-history): once a cursor crosses it every page is empty forever, so a client that cannot name the floor draws the store's own horizon as "the org went quiet". The store's constant, not a number this API picked, so a change to the retention reaches every screen without an edit. Seconds rather than days, because the retention is a duration and a client re-deriving the unit is a second place the number can be wrong. Carried by `GET /health` and the `stream` query; neither the 5-second push nor the snapshot's `health` section repeats it, because those two carry `status`, `in_flight` and `shutting_down` only, and this one does not change. |
| `in_flight` | Turns running on this node. Always present, and a `0` is a real zero: every process that serves the API runs the engine beside it. |
| `shutting_down` | `true` from the first moment of a drain, so a dashboard shows the drain while it happens: the listener keeps serving until the drain has completed. See [During a drain](#during-a-drain). |
| `posture` | The node's [config posture](../concepts/control-plane.md#posture-what-a-lagging-node-does): `serve`, `wait`, `shed`, `isolated` or `stuck`. The only place an operator can see *why* a node left rotation, since `/ready` answers a bare `503` either way. |
| `domains` | The state-log domains **this node** applies, derived from `node.roles`. A fleet's members may legitimately run different sets, so this is how you read off which node is applying what — and the only other symptom of a node that declines a domain is a peer's board answering a question this node's copy cannot. Always a list: a node that runs no domain at all refuses to start, so this is never `null`. `crewlet validate` reports the same set from a Tier A document, before the node boots. |
| `applied_epoch` | The activation epoch this node last applied. |
| `seats` | The handles of the seats this node holds, `[]` on a node holding none. |
| `stall_lag_seconds` | Present only when the node's watched duty is behind: how far, in seconds. It climbs towards the seat lease TTL, at which the watchdog ends the process. |
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

**A page shorter than `limit` is the end of the history.** That rule
holds for every filter the store pushes into SQL. It does *not* hold for
the `agent` filter, which over-fetches and post-filters (it also pulls in
every event sharing a trace with a direct match, so a caller must dedupe
by id); that surface only knows it is done when a page returns zero rows.

The persistent store retains 30 days, and
[`event_history_seconds`](#the-health-envelope) on the health envelope is
that floor on the wire — read it rather than restating the number, which
is the store's own constant and not a promise this page makes. Once a
cursor crosses that floor every page is empty — which is why a client
must distinguish it from quiet, rather than drawing the gap as silence.

`category` is a filter for the same reason paging exists at all —
filtering a paged list client-side silently excludes, because a 100-row
page holding 2 matches reads as "only 2 exist". Its vocabulary is a closed
set of ten values, and which event type lands under which is in
[Deployment § What gets stored](../guides/deployment.md#what-gets-stored-and-under-which-category).

### The event log's time axis

`GET /events/series` counts the matching rows per bucket over a window. It
takes every filter `GET /events` takes bar the cursor and `agent` (see
below), plus:

| Name | Default | Description |
|------|---------|-------------|
| `bucket` | *(required)* | `minute`, `hour` or `day`. A closed set rather than a duration, for the reason the spend series gives for its two: an axis with an arbitrary bucket width is one nobody can label. The log has `minute` and the spend series does not, because "what just happened" is the commonest question asked of a log and an hour is the whole of that answer's window. An unknown value is refused naming what is accepted, never defaulted. |
| `since` / `until` | (the retention window, to now) | RFC 3339 instants, half-open. Both edges are snapped **outward** to whole buckets, so the first and last bars are whole ones — a partial bar has a height that means something different from its neighbours' and a reader has no way to know. The answer is labelled with the window it actually covers. |

The answer is `{bucket, since, until, bars, total, by_category}`. `bars`
is **every** bucket in the window including the empty ones, so a quiet
hour is a gap of full width rather than a bar the chart squeezed out.

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

- **`related_agent`** is not accepted. That filter over-fetches and
  post-filters (see above), so a count over the predicate alone is a
  smaller set than the listing shows — and a bar that disagrees with its
  own rows is the one thing this axis exists not to be.
- **A window of more than 1,500 buckets** is refused naming the bucket and
  the span. Truncating would put a month's heading over a day of bars and
  coarsening would answer a different question from the one the axis is
  labelled with; the caller's fix is a coarser bucket or a shorter window.

### The live token meter

`budget` carries the fleet's **shared token counter** as the budget gate
enforces it: every node's spend since the last deliberate reset
(`POST /budgets/reset`), beside the cap in the active revision. It is the only
figure that can honestly be divided into a configured cap, because both cover
the same span. The dashboard's other token figures are spend rollups over a
window of time; dividing one of those into a cap produces a percentage that is
wrong by however much was spent outside the window.

Every node publishes a `budget_reported` snapshot of the counter every
**15 seconds** (`engine.BudgetReportInterval`), and the projection folds each
one in as it arrives. A company with no cap anywhere publishes none.

- `meter_id` identifies the node incarnation whose report is held. Every node
  reads the same counter, so reports under different ids describe the same
  figures read at different moments. A report is a complete snapshot, so a
  consumer **replaces** what it holds rather than merging or taking a
  maximum: a reset has to be able to lower the figure.
- `seq` is monotonic within a `meter_id`. The feed it arrives on is
  **best-effort**: an ephemeral broadcast subscription that takes no acks,
  starts at the stream's tail on every (re)connect, and lets a slow consumer
  miss frames rather than hold them. So a report at or below the held `seq`
  from the same meter is dropped, a report from another meter that was read
  **earlier** than the held one is dropped, and a gap is closed by the next
  report rather than replayed.
- `refused_at` is when the cap last turned a charge away, in UTC, and empty
  while the scope is not refusing. That, and not `used >= max`, is what
  "exhausted" means: a refused charge increments nothing, so the counter stops
  short of the cap by the size of the round that would not fit. The stamp is
  kept in the shared counter beside the spend, so every node reports the same
  one, and it clears on the scope's next admitted charge (or a reset).
- `{}` means no report has arrived yet. Per-agent, `budget: null` means the
  same, or that the seat has no per-agent cap at all: the engine meters a seat
  only for a non-zero `token_budget`.

It is deliberately never persisted: a report is a reading of a counter that
moves every round, so a copy replayed from history would show figures the
counter has since left behind as the current ones.

Each agent's `live_call` is `null` between turns, or
`{ turn_id, phase, iteration, model, prompt, prompt_messages, response,
tool_executions, round_narration, partial_round, rounds, in_progress }` while an
LLM call is under way.  A call whose phase failed keeps `in_progress: false`
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

**Who may open it, and what each reader receives.** The handshake needs
`state:read` — a caller without it is answered `403 unauthorized` before the
upgrade, naming the grant — and every push kind is received only by a socket
whose caller holds the grant that kind's question takes: `event` needs
`audit:read` (it is an `events` row, carrying every phase's prompt and
response), and every other company kind `state:read`. The snapshot is built for
the same audience. A re-check that finds the grants changed sends a fresh
snapshot for the new audience, and one that finds `state:read` gone closes the
socket `4403` — see [Close codes](#close-codes).

**Server → client kinds**

| `kind` | When | `data` |
|--------|------|--------|
| `snapshot` | First envelope after the upgrade succeeds, and again on reconnect. | Same payload as `GET /stream/snapshot` — agents carry their in-flight `live_call`, so a reconnect re-renders the live row. |
| `event`    | Every engine event published to `crewlet.events.>`. | `{ id, type, timestamp, source, actor, summary, category, trace_id, span_id, parent_span_id, topic, payload }` — the same shape as a `/events` row, plus the full event `payload` (from which the snapshot feed's `failed` flag is derived).  `agent_phase_completed` events carry the system prompt, response, and tool calls, so LLM invocations stream live; `agent_turn_progress` events (per tool-call round, tagged with `turn_id` / `phase` / `iteration`) stream the in-flight call before its phase record exists. |
| `agents`   | After an event moved one or more agents. | The changed agents' overlays, each with its `role` — the *result* of applying the event, so a client merges them rather than running its own state machine over the raw stream. |
| `seats`    | After anything changed the company — a config activation, or a write to the org chart's own log such as a hire, a move or a rename. | The COMPLETE seat list, replacing what the client holds. Distinct from `agents` on purpose: that one is a per-role merge, and a merge cannot express the deletion of a seat that has gone. |
| `sandboxes`| After a detached sandbox run started, asked a question, or finished. | The full in-flight sandbox list. |
| `tokens`   | On the shared 5-second tick, when a phase completed since the last one. The fold runs on the tick rather than on the publish, so a busy company costs one aggregation every five seconds rather than one per phase. | The spend rollup, same shape as `GET /tokens/breakdown`. |
| `budget`   | After a node's token meter report is applied (every node reports every 15 seconds while anything is capped). | `{ meter_id, seq, org: { used, max, refused_at } }`, the org-wide half. Per-seat figures ride on each agent's overlay in the `agents` push. See [the live token meter](#the-live-token-meter). |
| `org` / `tools` / `schedules` | On the same edge as `seats`: every published company, whether an activation or a chart write. | The new org tree / tool surface / schedule list, so open tabs stop showing seats that no longer exist. |
| `health`   | Pulsed every 5s by a **single shared tick** (one timer for all clients, not one per connection). | `{ status, in_flight, shutting_down }`, cut from the [health envelope](#the-health-envelope)'s read. The whole envelope is the `stream` query. |
| `result`   | Reply to a client `query` that succeeded. | `{ id, what, data }` — `id` echoes the request's. |
| `error`    | Reply to a client `query` that could not be answered, or a `watch` that was not installed. | `{ id, what, error }` where `error` is a code. A **watch refusal** has no `id` and names `what: "watch"`: `unauthorized` when the caller may not watch that seat, `unavailable` when this node could not read the chart that decides it (ask again after a few seconds). For a query, the codes are: `unknown_query`, `unauthorized`, `not_found`, `bad_params`, `unavailable`, or `query_failed` for every other failure (the reason goes to the log, never to the socket). **`unknown_query` covers a surface this process does not have**: a question whose source is not wired here is never registered, so it is unknown rather than empty and never carries a `Retry-After`, because waiting cannot give this node a store it was not configured with. Its REST twin is `404`. **`unavailable` is not `query_failed`**: it says this node understood the question and cannot answer it *yet* — a projection still catching up after a restart or a fresh join, or a coordination store it could not reach — so a client says "ask again in a moment" rather than reporting a fault. Its REST twin is `503` with `Retry-After`. **`bad_params` is not `query_failed` either**, in the opposite direction: the node understood the question and *refused* it — a parameter missing, malformed, or outside the set the field accepts — so the fault is the caller's and retrying sends the same bad request again. Its REST twin is `400`. |
| `pong`     | Reply to a client `ping`. | `null` |
| `identity` | This socket's own credential, at its last re-check. **Direct** — a fact about one tab, not the company, which is why it never rides the node's `health`. | `{ state: "unverifiable" \| "verified", retry_after_seconds? }` |
| `inbox_changed` | A committed tracker batch wrote the watched seat one or more notices. Every node applies every tracker record, so each node pushes to its OWN sockets from its own applier after the batch commits; nothing is forwarded between nodes. One frame per seat per batch. | `{ handle, unread_delta, subject, reason }`, and the frame itself carries `seat`. **Identifiers and a count, never content**: `subject` is the task the newest of those notices is about and `reason` the reason it was routed under, and what the notices say is read through `work_inbox`, the question that decides who may read them. `unread_delta` is a **hint** — the notices the batch wrote, which a redelivery the engine has already collapsed may count twice — so a client re-asks rather than adding it to a badge. **Routed by seat**: it reaches only the clients whose `watch` for that seat was allowed, never every tab. It needs `state:read`, the grant `work_inbox` takes. |

**Client → server kinds**

| `kind` | Purpose |
|--------|---------|
| `ping` | Keepalive; server replies with `pong`. |
| `watch` | Become a recipient for one seat's seat-routed frames: `{ kind: "watch", seat }`. An empty `seat` clears it (always allowed), and one socket watches one seat at a time — a tab is looking at one screen, so a second watch replaces the first. **It is decided like `work_inbox` about the same seat**, because it buys that seat's `inbox_changed` frames: the seat's own holder, whoever leads it through the chart, or the admin grant. Three answers, and none closes the socket. **Allowed** installs the watch. **Refused** installs nothing, drops any watch the socket held, and replies `error` with `unauthorized`. It is not a `4403` close, because a client reads that as "this browser may not have the live channel at all" when all that was refused is one seat's frames. **Undecidable** (this node could not read the chart) installs nothing either and replies `unavailable`. Installing would hand the seat's frames to somebody this node could not show was allowed them. Refusing would tell a lead they lead nobody because the node is behind. **Every credential re-check re-decides the current watch**: a refusal withdraws it and says so, and an undecidable answer keeps it, since nothing has been learned against a decision this node already made. A watch with nobody resolved behind it closes the socket with **4401**. |
| `query` | Request one thing, answered with exactly one `result` or `error` frame. `{ kind, id, what, params }` — `id` is any client-chosen value echoed back on the reply. **There is no per-frame credential**: every question is asked as the principal the handshake resolved (and each re-check since), and decided by the grant it is registered under. Queries run concurrently with each other and with the push stream, so one database read cannot stall a tab's live rows. |

**Queries** (`what`), each answered by the *same* function the matching
REST route calls, so the two surfaces cannot diverge:

| `what` | `params` | Answers with |
|--------|----------|--------------|
| `agent` | `{id}` | `GET /agents/{id}` — config + live state + `llm_history` |
| `agent_memory` | `{id}` | `GET /agents/{id}/memory`. Four collections: the diary, the episodes, the synthesized skills (with `skills_total` beside them, because the listing is a page of a set), and the COUNTERPARTY PROFILES — what this seat has learned about the colleagues it works with. Each profile carries both instants and they measure different cadences: `last_updated_at` moves on every interaction and `last_corroborated_at` only when the traits actually changed, so a colleague seen daily whose profile has not moved in months is one this seat has stopped learning about. `traits` is a bag whose keys the model invents, never a fixed schema. The diary is keyed on the derived agent id and the other three on the HANDLE; both are asked with the one identifier a caller has, and the half that does not recognise it answers nothing |
| `conversations` | `{handle, conversation, limit}` | `GET /agents/{id}/conversations`. The seat's own thread ledger — the engine's only account of what a seat said on a surface it does not own, and what stops it replying twice in one thread. TWO SHAPES IN ONE ANSWER, because a screen asks two questions with one navigation: `conversations` is every thread this seat holds entries in, and naming one in `conversation` adds that thread's turns as `entries`. Each turn's `reply` and `unsent` carry the same artifact and WHICH ONE HOLDS IT is the whole record of whether anybody received it — a turn can end with real work done and no way to say so. An absent `handle` is the caller's own seat, as on `work_my_work`; a named one is a seat's TRAIL rather than a person's queue, so it takes `audit:read` whoever's seat it is — see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for) |
| `event` | `{id}` | `GET /events/{id}` — one event with its full payload |
| `events` | `{limit, type, source, category, trace_id, actor, agent, turn_id, work_key, since, until, before_id, before_time}` | `GET /events`. `turn_id` selects ONE RUN of a turn; `work_key` selects every run of one unit of work — the attempts at a trigger that was redelivered. Rows written before migration `0029` carry the work key in `turn_id`, and that migration backfills it into the COLUMN, so history answers both. Every row answers with its own `work_key` read off that column rather than out of its `tags`, which is the one promoted value that is not a copy of a tag: the backfill deliberately does not rewrite a stored tags blob, since those record what the writer extracted from an event whose JSON carried no such field |
| `event_series` | `{bucket, since, until, type, source, category, trace_id, actor, turn_id, work_key}` | `GET /events/series`. THE SAME ROWS WITH A TIME AXIS, which a page of rows has no dimension for: a burst at four in the morning and a steady trickle across a week are the same hundred rows in the same column. A second question rather than a flag on the first, because the two answers have different shapes and one route returning either would make every caller branch on what came back — the same split `tokens` and `token_series` carry. Both halves compile their filters through ONE predicate in the store, so a bar can never claim rows the listing beside it would not show |
| `trace` | `{trace_id}` | `GET /events/trace/{trace_id}`. Answers `{trace_id, events, truncated}`; `truncated` is true when the read stopped at the store's per-trace cap (500) rather than at the end of the trace, which the caller must say — a trace shown short with no note reads as a complete causal chain that simply ends. It is **counted, not inferred** from the row count: a trace of exactly the cap holds every row it has, and `len(rows) == cap` would put a truncation warning on a complete one |
| `turns` | `{days, role, agent_id, model, work_key, failed, before, limit}` | `GET /turns`. ONE ROW PER RUN of a turn — a wake, a decision, its rounds and its reply — which is the view of a working company that did not exist anywhere. A turn that broke before reaching outside the engine is redelivered, so one TRIGGER is legitimately several rows; each carries the `work_key` they share and `work_key=` narrows to every attempt at one (see [a turn's two identities](../concepts/turn-engine.md#a-turns-two-identities)). A turn is what this engine DOES and every other surface is a projection of one: the spend rollup groups them, the seat page shows one seat's, an item's history links to the ones that touched it, and none of them is a list of them. The dashboard faked one by paging the raw event feed sixty-one times and folding in the browser — slow, capped at whatever the caller gave up on, and wrong at the page boundary, where a turn straddling two pages appeared twice. The aggregates are over PROMOTED COLUMNS (migration 0015) rather than payloads; only the duration, the summary and the task come from the completion record's own payload, read from the one row per turn that carries it. `complete` says whether a completion record exists — a turn with none is running or died mid-flight — and `duration_ms` is the turn's OWN measurement, which is not the span of its events: the span covers the reflection pass that publishes afterwards. `failed` is THREE-VALUED and absent means every turn, because folding it into `false` would hide every failing turn from an unparameterised list — and a turn counts as failed when ANY of its events carried a failure OR was a failure BY ITS TYPE (`llm_unavailable`, `budget_exhausted`, `turn.guard_breach`, `sandbox_run_failed`), which is the same rule `/events` applies to a row. The second half is what a turn the engine killed BETWEEN phases leaves behind — a refused charge, an exhausted chain, a breached guard — so reading the failure flag alone reported those as clean turns with no completion record, which is indistinguishable from a turn still running. The cursor is on the turn's START, which is what the listing is ordered by; a keyset on any one event pages a turn twice |
| `turn` | `{turn_id}` | Every event of ONE RUN of a turn, oldest first, payloads included — each phase, the turn's own completion, and the fallbacks and guard breaches that happened inside it. Not a slice of the trace: one trace can span several turns and one turn several traces. Rows written before migration `0014` carry no `turn_id` and do not answer this. Answers `{turn_id, events, truncated}`; `truncated` is true when the read stopped at the store's per-turn cap (500) rather than at the end of the turn. A cut answer is the turn's **opening and its ending**, not its opening alone: a turn is read oldest first, so a head-only read would drop `agent_turn_completed` and `turn_completed` — the two records a reader takes the outcome, the duration and the plan summary from — and a turn cut at the cap would be indistinguishable from one that never finished. The last rows are recovered beside the first (up to 20 more, merged on the store's own identity, `(event_time, event_id)`, so the two reads cannot overlap into duplicates — the id alone is not unique, and a narrower key would drop a row the two reads legitimately both carry and then report a gap over a page holding the whole turn), so what `truncated` names is a gap in the **middle** — and it is **counted, not inferred** from the row count, because a turn between the cap and the cap plus twenty ends up whole on the page and must not carry a truncation warning. It also answers `work_key` and `attempts`: the unit of work this run was an attempt at, and every run of it the store holds, OLDEST FIRST — over the SAME thirty-day horizon the events above come from, not the turns list's own default week, so a turn between eight and thirty days old names its attempts rather than reporting none while displaying one — so the screen a deep link lands on can say "attempt 2 of 2" and link the other, rather than leaving a reader to conclude the company did the work twice. One element is the ordinary case; an empty `work_key` means the trigger had none to collapse on, and `attempts` is then empty too |
| `phases` | `{role, limit, before_time, before_id}` | The company's `agent_phase_completed` records, newest first, **payloads included**, keyset-paged. `events?type=agent_phase_completed` is not a substitute: the event listing deliberately never selects the payload, and a phase record without one has no prompts, no response, no tool calls and no decision |
| `tokens` | `{since, until, since_days, agent_role, recent_turns}` | `GET /tokens/breakdown` — for a window other than the live one |
| `token_series` | `{group, bucket, since, until, previous, groups, agent_role, since_days}` | `GET /tokens/series`. THE SAME SPEND WITH A TIME AXIS, which the breakdown has no dimension for: every one of its rows is a sum over the whole window, so a runaway loop, a spike and a quiet weekend are the same number. A second question rather than a flag on the first, because the two answers have different shapes and one route returning either would make every caller branch on what came back. Bucketed by the ENGINE — the browser holds at most the live window's records, so an axis folded client-side would be right for a day and absent for every other range. An unknown `group` or `bucket` is refused naming what is accepted, never defaulted: a chart legended by one dimension over another's bands is worse than an error |
| `schedule_runs` | `{scope_type, scope_id, name, limit}` | `GET /schedules/{scope_type}/{scope_id}/{name}/runs`. ONE schedule's dispatch history, newest first, fifty to a page. `scope_id` is the scope's IDENTITY (a seat's agent id, a unit's origin key), which is what a `schedules` row carries and what the ledger keys on — hand back what that row gave you rather than the handle beside it. `schedules.recent_runs` is the COMPANY's fifty most recent fires across every schedule, so twenty hourly ones fill it in two and a half hours — "did the standup fire this week" was unanswerable while every row of the answer sat in the table. The identity is all THREE parts and each is required: two units may each declare a `standup`, and a role and a unit may both, so a name alone merges two teams' histories. `truncated` says the page filled, because a full page is otherwise indistinguishable from a schedule that has fired exactly that many times |
| `schedules` | `{}` | `GET /schedules` |
| `fleet` | `{}` | `GET /fleet`: leases move with no event to push, so the Fleet view polls this rather than waiting for one. **Operator-only**, like the rest of the Admin workspace. A lease table that could not be read answers `unavailable`, which is a blip to ask again about rather than a fault (the REST twin answers `503` with a `Retry-After`) |
| `sandbox_runs` | `{}` | `GET /sandbox-runs`: `unknown_query` on a company with no sandbox configured, and `unavailable` when the fleet's run record could not be read |
| `budgets` | `{}` | `GET /budgets` |
| `a2a_channels` | `{}` | The fleet's agent-to-agent authorization record: who asked whom, how many messages crossed, and when. `available: false` when this node could not reach the coordination store — which is not the same as no channels having been opened |
| `knowledge` | `{q}` | The company's own knowledge search, run live through the same `knowledge.Searcher` seam a seat's own `search_knowledge` tool uses. Searched as the ORG with no seat, so it applies the engine's own account and nothing more — searching as a named seat would let a dashboard reader read, through that seat's credential, material their own account may not have. Registered whenever a company is active, NOT only when a searcher exists — "this company has no knowledge backend" is a fact the company establishes on its own, and it is a far more useful answer than an unknown query. `available: false` covers all three of no company, no backend, and a backend wired with no org-wide read scope. `reason` (`no_company` / `no_backend` / `no_scope`, empty when the search ran) is the value to branch on and `note` is the prose for a person — a screen picking which remedy to offer must not string-match the note, nor infer the state from an empty `backend`, which means "no backend" and "no company" alike. The `no_scope` note names `knowledge.scope`, because an operator whose integration is correct must not be sent to re-check it. It carries a reason on a failed search too, because search is best effort by contract and an empty result is not proof that nothing matches |
| `integrations` | `{}` | `GET /integrations` |
| `work_items` | `{container, status, status_group, assignee, reporter, watcher, collaborator, tag, type, priority, parent, root, q, key, removed, blocked, blocking, has_dependencies, has_open_asks, flag, asked_of, asked_by, subtasks, f.<slug>, view, preset, group_by, group_by2, group, subgroup, group_limit, totals, sort, cursor, limit, …}` | `GET /work`. `container` is the scope — `workspace`, or `project:ENG` (a bare `ENG` works too, and the key is upper-cased because the column is) — and an ABSENT container is neither: the engine refuses to default it, because an omitted key would otherwise be the most expensive query in the system. Every list key is comma-separated, because a socket frame's JSON object cannot carry a repeated key and a filter only one transport can express is exactly the divergence this channel exists to prevent; `status` also takes `!` negation. There is no `open` flag — open and closed are STATUS GROUPS (`not_started`, `active`, `done`, `closed`), which is the level every rule in the tracker is written at. `f.<slug>=<value>` filters on a custom field — resolved against the company's catalogue by slug, id or label, and compared on the column its DECLARED TYPE says, so `f.effort=gt:9` is a numeric comparison and not a lexical one; the seventeen operators are `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `contains`, `startswith`, `in`, `range`, `any`, `all`, `not_any`, `not_all`, `me`, `null` and `not_null` — and which of them a field admits is a property of its TYPE, so `eq` on a `labels` field is REFUSED naming `any`, `all`, `not_any` and `not_all` rather than compiling to a clause that matches nothing and reads as "no task has this label". `null` and `not_null` are on every type, because "is this set" is a question about the ROW. A bare value is the type's NATURAL comparison — `any` on a set, because naming a value is not claiming the set IS it, and `eq` everywhere else. A set operator takes a comma-separated list (`any:api,ui`, at most 16) and `range` takes both ends (`range:3..8`), because a range with one end is `gte` or `lte`. A value whose text begins `<scheme>://` is a VALUE rather than an operator call, so a `url` field can be filtered by what it holds — anything else before a colon is carried through as an operator, so a typo is refused naming the set rather than silently answered. `f.<slug>=me` is resolved to the reader by the SURFACE before the query is parsed, which is what makes one saved view mean whoever opens it. A ref nothing resolves is REFUSED naming it. `q=` is a FIND rather than a search — a substring of a key (from the front) or a title (anywhere), which is what finds the item somebody half remembers; there is no `mode`, because this grammar has no ranker and ranked search over the company's prose is `search_knowledge`'s. `key=ENG-1,ENG-7` narrows to keys a caller already holds — upper-cased, like `container=` and `references=`, because a key is what somebody pasted and the column it is compared against is minted upper-case — and `removed=true` is the TRASH — the only way to list what a removal hid, which is what a restore is a gesture about. A parameter this grammar does not read is REFUSED naming it, never ignored: a filter nobody parsed is a board showing more than the person asked for, silently. An unknown status or group is refused naming the closed set rather than matching nothing. A custom field VALUE is checked against its own declaration at the write and refused naming the rule — never rounded or coerced to fit; see the coercion table in [the work tracker guide](../guides/work-tracker.md). `flag=` is the ATTENTION queue and its values OR: `cycle`, `too_deep`, `inconsistent_project` and `key_collision` are facts about a task's own row, and `one_sided` and `one_sided_final` are about a DEPENDENCY of it — an authored `waiting_on` whose blocker does not list it, and one whose mirror was refused permanently (the blocker is gone, was removed, or is full). The first is what the `tracker` duty repairs 30 seconds on; the second is what a person resolves. They OR because an attention queue asks "is anything wrong with this", and a conjunction over six flags answers nothing on every company. `totals=<column>:<op>` adds aggregates over the WHOLE matched set rather than the page — a number that changed as somebody scrolled would be the one thing a header must not do. The five ops are `sum`, `avg`, `min`, `max` and `count`; the columns are the summable ones (`points`, `estimate_min`, the `spend_*` family, `reassignments`, `depth`), the date columns for `min`/`max` only (a sum of dates is a number of microseconds nobody meant), `tasks:count`, and `f.<slug>` for a declared number or date field. A total with nothing to add up is ABSENT rather than zero: "nothing is estimated" and "everything is estimated at nothing" are different facts. `subtasks=` is how a tree is filtered: `collapsed` (the default) and `expanded` filter ROOT tasks and let their subtrees ride along unfiltered — so a todo root brings its done subtask — while `separate` filters every task on its own. The first two answer the same SET and differ only in how a caller renders it. Asking for a subtree with `parent=` or `root=` turns the mode off, because those are questions *about* subtasks and filtering their roots would answer the parent's siblings. `any=[{…},{…}]` is one level of disjunction, ANDed with the top-level keys: a branch is a PREDICATE, so it may not carry the keys that decide the answer's own shape (`removed`, `archived`, `show_closed`, `subtasks`) or how fresh it must be (`read_level`, `max_lag_seconds`, `max_lag_seq`, `min_position`) — those are the same decision at every branch or they are incoherent, and a branch that carried one would narrow what was asked for at the top level rather than widening it. An empty branch is refused, because it matches every task and makes the others decoration. `view=<id>` and `preset=<name>` are loaded FIRST and every explicit key overrides them — a saved view is a set of defaults rather than a lock, so somebody who opens a board and picks another assignee gets the view with that one key changed. A view beats a preset (somebody saved it) and what was typed beats both. The five presets are `my_queue`, `priorities`, `triage`, `blocked` and `overdue`. `my_queue` is *what can I pick up*: a DISJUNCTION of the work the viewer holds and the work in their OWN project nobody holds, open and unblocked, most important first — both arms matter, because written as "assigned to me" alone a seat with an empty queue reads the company as having nothing for it while its project's unclaimed backlog sits there, and the second arm is scoped to their project because unscoped it offers every unassigned task in the company. `priorities` is the viewer's own ordered list, open tasks only, IN THE ORDER somebody arranged it — that order is the answer, so nothing sorts over it, and a finished task drops out of the answer without the list being rewritten. `triage` is the unassigned open work, which with one fixed status set is the honest definition of "needs somebody to decide". `my_queue` and `priorities` are the CALLER's own — there is no parameter naming whose, because a question about somebody's queue asked on their behalf is the owner-or-lead rule's, not a filter's — and a caller bound to no seat is refused both, because a list with nobody's name on it is everybody's. A `view=` nothing resolves is REFUSED, never answered as the whole board. `group_by=` turns the answer into a BOARD: `groups` replaces `rows` — returning both would be the same rows twice — and each column carries its own `count` over the whole set beside a bounded slice of its rows (`group_limit`, default 20, max 100). A grouped answer mints no cursor, because across a set of columns there is no single order to be after; `group=<value>` is how a board loads one column further, and it narrows the WHOLE query, so the hint and the totals describe that column too. `group_by2=` adds swimlanes inside each column and `subgroup=` names one — a swimlane board is bounded by its CELLS rather than by either axis alone, because the work it costs is the PRODUCT of the two, so asking for lanes lowers the column cap and `subgroups_dropped` says how many lanes a column has beyond it. A `group_by=` over the WHOLE COMPANY is refused when the query's own narrowed predicate still matches more than 20 000 tasks: a board is drawn by sorting every one of them, and the refusal names the ceiling and what narrows it. It is a bounded COUNT rather than a check for the presence of a filter key, deliberately — `status_group=not_started,active` is a filter and narrows nothing, so a gate spelled "needs a narrowing filter" is one a caller clears in a single attempt without making the query any cheaper. Scoping to one project with `container=project:<key>` lifts it, because there the input is an index range whose width is one project's own size. An absent value is its own labelled column — "nobody is assigned" is a question a board answers rather than a row it hides. `group_by=tag` is the one axis where a task is on several columns at once; the answer sets `groups_overlap` so a reader knows the counts do not sum to `total_hint`, and `groups_dropped` says how many columns did not fit. `sort=` takes `rank`, `updated`, `due`, `start`, `priority`, `created`, `title`, `estimate`, `points`, `spend` and `status_entered`, each reversible with a leading `-`. **An absent value sorts LAST in both directions**: "soonest first" and "latest first" are both questions about values, and a task with no due date is the answer to neither — so `sort=due` puts the undated at the end rather than ahead of the one due tomorrow, and a cursor resumes in the same place the order put it. `sort=f.<slug>` orders by a custom field, LEFT-joined so a task that set no value still appears — a sort that also filtered would be two things the caller asked for once, and such a task sorts last by the same rule. The answer carries `total_hint` (capped — an exact total over an unbounded set turns a poll into a scan), `next_cursor`, `totals`, `groups`, and an echo of the `view`/`preset` it was expanded from — these answers travel detached from their requests, so a board restored from a URL can still say which saved view it is showing — plus the coverage half below |
| `work_item` | `{id}` | `GET /work/{id}` — key or id. Answers `{task, comments, history, links, fields, blocked}` plus the same coverage half. `blocked` is on the ANSWER rather than on `task` because it is DERIVED — an open dependency edge, computed in the same transaction as the task, so the badge here and the badge on the board row cannot disagree; `links` say what the relations are, not whether any blocker is still open. `fields` are the task's CUSTOM fields resolved against the company's catalogue — each carrying its declared name, type and whether its declaration was archived — because a stored choice is an option's UUID and a panel rendering the raw value would print it under a heading |
| `work_catalogue` | `{archived}` | `GET /work/catalogue`. Answers `{types, fields, policy_version, types_version, fields_version}` plus the coverage half. `policy_version` moves on every *fields* edit and is what a task's policy stamp records having validated against; a *types* edit does not move it, because the two are separate objects on separate subjects so an unrelated edit never invalidates every task's stamp |
| `work_projects` | `{q, unit, archived, limit}` | `GET /work/projects`. `task_counts` is read from `tracker_projects.open_count/done_count/closed_count`, MAINTAINED by the task apply whenever a status group changes or a task enters, leaves or is removed — never aggregated per poll, which over every task in every project is what a sixty-second refresh used to cost. `unit.resolved` is a FIELD rather than an absence: "this project names a unit the chart no longer has" is a finding, and an absent unit would be indistinguishable from a project that names none |
| `work_project` | `{key, for_type}` | `GET /work/projects/{key}`. A SET read, not a point read: its shape is dominated by aggregates over task rows — the counts — so it carries `complete`/`incomplete` under the same contract every set answer takes, and its closure is the project's container plus both catalogues. `shadowed` names the workspace field ids this project redeclares, which is what a field in the middle of a move between scopes looks like |
| `work_workload` | `{unit, read_level, …}` | `GET /work/workload`. Who is carrying how much, for everybody at once. It counts OPEN work — every task assigned to somebody, whatever its dates — because "who is carrying the most" is a question about a whole queue. Both measures a company may size in are carried rather than one chosen, since which is used differs by team and an answer that picked one would be wrong for everybody sizing in the other. Beside them it carries `blocked`, `overdue` and `unscheduled`, because a person whose whole queue is blocked has a different problem from one who is simply busy. Ordered heaviest first and capped, with `truncated` when the cap was reached. `unit=` narrows to the people whose open work sits in projects that unit owns |
| `work_activity` | `{task, container, kinds, actor, actor_kinds, assignee, q, notified, since, from, to, limit, cursor}` | `GET /work/activity`. Every commit writes a history row, so this is an account of what HAPPENED rather than of what was announced — `notified` is how a reader tells the two apart, and `kinds` is what the change WAS whether or not anybody heard about it. The two used to be one: a record's kind was read off its notification, so the same change filed under one word with an audience and another without, and a quiet removal reached the feed as `tombstone` while the filter spells it `removed`. `kinds` accepts the twenty-seven change kinds and nothing else. Each record carries BOTH instants: `at` is the authored one a card renders, and `effective_at` is the fleet-agreed one every duration is measured on. `actor` is who made the change; `assignee` is whose work it is, and the two are routinely different people. `actor_kinds` is a CSV of `agent`, `human`, `operator` and `system` and narrows to WHO WAS WRITING rather than to which handle — which is not the same question and cannot be asked as a set of handles, since an `operator` commit carries a token's own label where an `agent` one carries a seat handle, and the set of people is the roster, which changes. It is REFUSED when it names a kind this build does not have, unlike `kinds`: a filter silently ignored answers a wider question than the caller asked, and on an audit feed that reads as a company where everybody is an operator. The `q` gate is on what the query would SCAN, never on which keys were named — `container=workspace&q=` and a five-year `since` both name a key and narrow nothing |
| `work_my_work` | `{handle}` | `GET /work/my-work`. Seven lists, each bounded at 20 so no block crowds out another — the whole answer is read as one page. `priorities` is NOT re-sorted: the order is what somebody decided. A finished or removed task is filtered out of it rather than rewritten out, because a read must not write to somebody's own object  `handle` DEFAULTS to the caller's own seat and naming anybody else's needs an operator credential — see below |
| `work_inbox` | `{handle, unread, primary_only, include_snoozed, reasons, limit, cursor, since}` | `GET /work/inbox`. One person's notices, newest first, 50 to a page. Each names the ONE reason of eighteen it reached them under, `addressed` (it asks something of them rather than informing them), `fallback` (nobody better was found), and their own read and snooze marks. `primary_reasons` is the split that was APPLIED, defaulted, so a caller renders *you are seeing these because* without repeating the rule; `unread` and `primary` are counts over the PAGE and say so, because a total over the table is a second scan of rows this answer did not return. `reasons` FILTERS rather than classifies — the primary split classifies the same rows — and an unknown one is refused naming the eighteen. `since` is a log POSITION (`<stream>@<generation>:<sequence>`, what `seen_through` renders), never a bare sequence. Same scope rule as `work_my_work` |
| `work_search` | `{q, limit}` | `GET /work/search`. The company's work RANKED against a phrase — BM25 over the engine's own inverted list, which is the same ranking a seat gets from `search_work`. Not a filter: `work_activity`'s `q` is an escaped LIKE over an excerpt, gated to a span of days, and answers a different question. Registered only where this node HOLDS an index, which is separate from holding the board: a node that joined recently has every row and no index, and answers `available: false` with `reason: "building"` rather than an error or an empty result — nothing is wrong, and a reader told *nothing matched* files the duplicate. A score is comparable WITHIN one answer and nowhere else, because the statistics it is computed against are this corpus's |
| `work_routing` | `{record_id}` | `GET /work/routing/{record_id}`. Who ONE change woke, and under which reason — the fact no other tracker records. `tracker_notifications` has always been readable by RECIPIENT (`work_inbox`); this is the same rows by RECORD, which is a primary-key prefix scan and needs no index of its own. Each recipient names the ONE reason of eighteen that found them, `addressed` (it asks something of them), and `fallback`/`fallback_rank` (nobody better was found). `notified` is the history row's own flag and means the commit CARRIED a notification — never that somebody was woken, since the applier deliberately does not hold the roster that would need. So an empty recipient list is THREE facts and `delivery` tells them apart: `nobody` (announced, inside the retention window, and every candidate was the actor or has left), `swept` (older than `tracker.native.inbox_retention_days`, so their absence is not evidence), `unknown` (no horizon stated) and `quiet` (the commit announced nothing, which is most of them). `retained_from` is the instant that decision was made against |
| `viewer` | `{}` | `GET /viewer`. `{login, grants, handle, name, kind}`. Registered on EVERY build with no seam of its own: who is asking is a property of the request rather than of anything this node stores. A bound caller carries a `handle`; an unbound one carries a login and no seat, which is an ordinary state rather than a refusal |
| `work_person` | `{handle}` | `GET /work/people/{handle}`. Scoped like `work_my_work`: absent is the caller's own seat, somebody else's needs an operator credential. `due` is the snoozes whose time has come, REPORTED rather than promoted: putting one back in the unread list is a write, and a read that performed one would change fleet state from a path with no operation id and no record. `priorities_set_by` is who last set the queue when it was not this person, which is how a lead's authority is made visible — every notification this domain carries is task-shaped, so one attached to a person record would render no card and reach nobody |
| `work_views` | `{container}` | `GET /work/views`. `container` is the strip's own — `workspace`, `project:ENG`, `unit:engineering`, `person:ana` — and it is REQUIRED, because a strip belongs to exactly one. Whose personal views appear and whose pins come first is the CALLER's, never a parameter; a caller bound to no seat gets the shared strip — no pins and no personal views but the shared ones. Every row carries `builtin`, which is what tells the six nobody saved from the ones somebody did: a builtin row has no `id`, so there is nothing to rename, protect, rank or pin. `params` is the saved query in `work_items`' own parameter names — this channel's, not the `list_work_items` TOOL's, which renames four of them for a model — so a caller either hands them straight back or, simpler, passes the view's `id` as `view=` and lets the engine expand it |
| `pages` | `{container, parent, status, label, watcher, title, skills, onboarding, limit, offset}` | `GET /pages`. `skills` is three-stated: only the tool-skill pages, everything but them, or everything |
| `page` | `{id}` | `GET /pages/{id}` — id or `CONTAINER/Title` |
| `containers` | `{}` | `GET /containers`. A separate question from `pages` rather than a facet of it: a browser draws the container list once and the page list on every navigation |
| `page_activity` | `{page, container, kinds, actor_kinds, since, cursor, limit}` | What happened to a page, or to everything in a container — the wiki's own change log, mirroring `work_activity`. `kinds` is a CSV of the ten change kinds and `actor_kinds` of the three author kinds (`agent`, `human`, `operator`), the latter refused when it names one this build does not have — `work_activity`'s note says why. `since` bounds the window and `cursor` pages it: the same unit, two parameters, because the cursor moves with every page and the bound does not |
| `page_revision` | `{page, version}` | One revision's own body, message and author. Revision N is the body AT version N — including the newest — so a reader comparing two versions asks for both rather than for one and the head |
| `stream` | `{}` | The [health envelope](#the-health-envelope), from the builder `GET /health` answers with. Named `stream` rather than `health` so a query never shares a name with a push kind: the `health` push carries three of those fields, and a reader of the protocol should not have to know which direction a frame travelled to know what it holds |
| `config` | `{}` | `GET /config` *(operator token required)* |
| `config_audit` | `{limit}` | The revision history — no REST twin; `GET /config/revisions` serves the same records *(operator token required)* |
| `config_diff` | `{revision_id}` | [`GET /config/revisions/{id}/diff`](#get-configrevisionsiddiff) — the listing is cut at 500 and `changes_total` is how many there are *(operator token required)* |
| `config_entities` | `{kind, id}` | One addressable collection of the active revision: its ids, or one entity out of it. The read half of the Configuration screen, whose write half is `PUT /config/{kind}/{id}` *(operator token required)* |

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
on another domain's log is refused `wrong_stream` rather than waited for. It is
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

1. **No handle** answers for the seat the caller is bound to.
2. **Your own handle** is the same thing said explicitly.
3. **Anybody else's handle** is decided by the authority table, each question
   asking its own verb. The first three are somebody's QUEUE, and take the
   owner-or-lead rule: whoever leads that person, or a caller holding
   `fleet:operate`. A node that cannot read the chart to tell answers `503`,
   never a refusal — a lead told they lead nobody goes looking for an
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

The binding is the **identity directory's**: a person's row names the seat they
hold, and a Tier A token acts as a seat when the directory binds its login to
one. A caller bound to no seat is refused `bad_params`, not `unauthorized`.
Nobody was denied anything: there is no person to answer about, and the remedy
is a binding in the directory rather than a different credential.

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
  empty result: "this company has no work" is an answer a person acts on.
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
binary embeds — a store that mirrors the projection and derives nothing,
a reconnecting WebSocket client with heartbeat, query channel and
REST-snapshot fallback, a hash router that keeps every screen, section
and filter in the URL, and one file per screen.  `/dashboard` serves the
shell; `/static/{path}` serves its assets.  The build output is
COMMITTED, so `go build ./...` needs no Node.

`/static/dashboard/THIRD_PARTY_NOTICES.txt` (served as `text/plain`) is the
license text of every npm package the bundle contains, the design system's
three among them, written by Vite's `build.license`, followed by the SIL Open
Font License of the embedded Inter and JetBrains Mono faces and the Apache
License and notice of the Material Symbols drawings every glyph is one of. The
release archives and the container image carry the same file, beside the
notices for the Go modules the binary links.

A second build target, `/static/dashboard/protocol.js`, is the wire
protocol alone as plain ESM: `internal/e2e` replays a real company's
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
          "manages": ["platform"]
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

In that document `manages: ["cto"]` is a seat **handle** and `manages: ["Platform"]` is a unit **key** — the unit declares no `id`, so its name is its key. A seat is always named by its handle and a unit always by its key; neither is ever named by a display name.

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
`name`, `mission`, `vision` and `policies` come from the revision, because
those are the settings it does still hold.

The fields above it stay as WRITTEN, so a reader can still tell a declared lead
from an inherited one. Every list here may arrive as `null` (Go marshals a nil
slice that way); a reader treats `null` as empty. The authored `path`,
`unit_path` and `placed_by_ref` of each entry are omitted, because they say
where a seat was WRITTEN — a fact about a document an ordinary reader is given
no way to point into, and one a company composed from chart rows cannot answer
at all, since each row states its unit directly and nothing was moved by a
reference. Membership is each unit's `seats`; the same block with paths comes
back from [a configuration write or dry run](#what-a-write-answers), which
reads an authored document.

**What it carries, and nothing else.** The company's `name`, `mission`,
`vision`, `policies` and `derived`; for each seat its `name`, `kind`, `handle`, `goal`,
`backstory`, `responsibilities`, `behavioral_guidelines`, `manages` and
`availability`; for each unit its `name`, `type`, `purpose`, `lead`, `goals`,
`channel`, `knowledge`, `roles` and `children`. Every value is the one the
company document holds, as written: a seat with no declared `handle` has none
here (the engine derives it from the name), and a unit that inherits its lead
has no `lead` of its own. An empty field is omitted, and a node with no active
company answers `{}`.

**What it never carries.** A seat's `contact` identities, `email`, `unit`
reference, `llm` and per-phase `llm_*` chains, `workers`, `token_budget`,
`learning_enabled`, `mcp_env`, `sandbox`, `placement`, `integrations` and
`schedules`; a unit's `mcp_env`, `integrations` and `schedules`; and every
company block outside the charter (providers, MCP servers, integrations,
knowledge, budgets). Those are read through the operator-gated `config` query
or [`GET /config`](#config--live-config-management-auth-gated), which masks
credentials. Two of them also have a read surface of their own, under the same
posture as `/org`, and the tree does not repeat them:
[`GET /schedules`](#routes) answers every configured schedule with its
task and next run, and [`GET /budgets`](#get-budgets) answers each seat's token
cap beside the counter it is enforced against.

**Why an explicit shape.** `/org` is readable without a token under the
default read posture. Serialising the config's own
seat and unit types would make every field added to a seat public the day it
landed, whatever it held. The shape is declared field by field in
`internal/api` instead, and a test fails when the config gains a seat or unit
field nobody has classified as public or guarded.

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
`merge_work_item`, `search_work_items`, `get_work_catalogue`, `list_projects`, `describe_project`, `write_project`,
`task_activity`, `my_work`,
`list_pages`, `get_page`, `write_page`, `save_page`, `comment_on_page`, and
`search_knowledge`. A schema, a default, a trimmed field and the wording of a
refusal are each written once — two copies of "file an item" drift on exactly
the parts nobody looks at, and only one of the two is ever tested.

Every write's answer carries its three-valued `outcome`, the object's new
`version`, and **`position`** — where the record landed, as
`<stream>@<generation>:<sequence>`, or `null` on an `unknown` outcome, where
the broker never said. That is the value a client hands back as `min_position`
on any read above, so an assistant that files an item here and redraws the
board over `GET /work/items` is served an answer that includes it rather than
whatever this node happened to hold. The tools' own reads need none of it —
they read `linearizable` — but the answer travels to a client that does not.

The one field that differs is **who the call acts as**. There is no turn and no
seat here, so this surface supplies its own identity, and every tool resolves
the caller through that rather than through the turn — a read that asked the
turn first refused every call on this endpoint while the writes beside it
worked. A comment's `@handle` is resolved here too, against the company chart
current when the comment is written, so mentioning somebody from your own
assistant wakes them exactly as it does from a seat.

Plus **ten no seat is given**: `list_work_views`, `save_work_view`,
`write_work_catalogue`, `get_person`, `work_inbox`,
`mark_inbox`, `set_pins`, `set_priorities`,
`remove_work_item` and `restore_work_item`. A view is furniture — a name, a shape
and a filter, arranged so a person finds the same question tomorrow — and a
seat's job is the work rather than the furniture around it. And the
catalogue is the company's own vocabulary: a seat adding a type so its own
create succeeds is a seat editing the rules it is judged by, and the refusal it
was working around is the signal a person needs to see — which is why reading
the catalogue *is* a seat's and writing it is not. And a person's record is a
HUMAN's: a seat has a mailbox — the durable subscription the engine attaches
when it acquires the seat — and nothing on a person's record describes one.
The trash is the last of them: a removal takes an item off every board in the
company, and a seat that could hide work it did not want to do would be marking
its own homework in the one way that leaves no trace. Neither destroys
anything — a removal is reversible at any age, and a purge (`crewlet work purge`, or `POST /work/items/{key}/purge`) is the
one that is not.

Each tool appears only where its half of the company is native: a company on
`tracker.backend: jira` gets the page tools and not the work tools, and one on
neither gets no endpoint at all. `search_knowledge` is the exception and is
offered against **any** knowledge backend, Confluence included — a ranked
search over the company's own wiki is exactly as useful to an assistant there.

The turn-only tools are deliberately absent: the memory tools (a diary belongs
to a seat), the skill tools (a skill is loaded into a phase), `a2a_ask` (a
colleague's answer comes back by waking a seat, and there is nobody here for
it to reach) and `run_sandbox` (a detached run resumes a suspended phase that
does not exist).

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

| Who is calling | Recorded as | Author kind |
|---|---|---|
| A person the identity directory binds to a seat | the **seat's handle** | `human` |
| A person bound to no seat | their **login** (`jane.doe`) | `operator` |
| A Tier A token or a machine credential | its **whole login** (`token:ops`) | `operator` |

The seat half is what lets the tracker leave you out of the wake your own
change sends: nobody is woken about what they just did, and the tracker
recognises the author by the name the record carries — so a person recorded
under a token's id was woken about every edit they made. The login half keeps
the colon: `token:ops` can never be a seat's handle, where the bare `ops` could
be, and an own-record rule comparing names would then admit a credential into
the record of the seat that shares its spelling. The credential is recorded
again beside the author, so an audit can ask what one token did without
reasoning about kinds.

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
serves needs one: its tool list is fixed when the node builds it, so there is
nothing to notify a client about, and no tool here asks the client anything
back.

### Why it is not under `/mcp/`

`/mcp/` is exempt from authentication wholesale, because the sandbox
[tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)
lives there and a box running generated code holds no API token — a signed
per-run token in its own path is what authenticates it instead. Mounting a
writable company surface under the same prefix would have put it behind no
credential at all. `/operator` is its own always-guarded prefix, alongside
`/config` and `/secrets`.

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
`tracker.backend: jira` gets `404` from the work routes, not a `503` that reads
as an outage.

### The routes, and the authority each takes

The authority is the [authority table's](#which-grant-a-route-needs), decided
ONCE. Where the object is in the path — a project, a person — the route
decides the verb itself. Where it needs a stored row — which project an item is
filed under, which space a page is in, who wrote a comment — the route admits a
reader and the verb is decided once the row is read, by the tool's own ask or,
for a verb with no tool, by the route.

| Method | Path | Does | Decided by |
|---|---|---|---|
| `POST` | `/work/items` | `create_work_item` | `work:write` |
| `PATCH` | `/work/items/{key}` | `update_work_item`. `If-Match` is its `if_match` | `work:write`; re-routing asks the project's lead |
| `POST` | `/work/items/{key}/comments` | `comment_on_work_item` | `work:write` |
| `PATCH` | `/work/items/{key}/comments/{cid}` | Rewrite a remark: `{"body": …}` | its **author** — the table admits `fleet:operate` too, and the tracker then refuses anybody but the author, because a remark somebody else can rewrite is one attributed to a person who did not make it |
| `POST` | `/work/items/{key}/rank` | Place the item `{"after": ref, "before": ref}` — one or both neighbours, in its own project | `work:write` |
| `POST` | `/work/items/{key}/depend` | `update_work_item`'s `waiting_on`, `blocking` and `dependency_note`, and nothing else | `work:write` |
| `POST` | `/work/items/{key}/relate` | `update_work_item`'s `linked` and `linked_pages`, and nothing else | `work:write` |
| `DELETE` | `/work/items/{key}` | `remove_work_item` (`{"subtree": true}` takes its children) | the lead of the item's **own** project, or `fleet:operate` |
| `POST` | `/work/items/{key}/restore` | `restore_work_item` | the same |
| `POST` | `/work/items/{key}/purge` | Destroy it and every row it produced — see [below](#a-purge) | `fleet:operate`, and **never a seat** |
| `PUT` | `/work/projects/{key}` | `write_project`'s policy: `fields`, `default_assignee`, `archived` | the project's lead or `fleet:operate`; archiving never a seat |
| `POST` | `/work/projects/{key}/tags` | `write_project`'s `tags_add`, `tags_rename`, `tags_archive` | declaring: `work:write`; renaming and archiving: the project's lead |
| `POST` | `/work/views` | `save_work_view` | a personal view is its owner's; a shared one its container's lead's — and a save naming a stored view's `id` also needs that authority over the view it replaces. Only a shared view can be `default` |
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

`{key}` is an item's key (`ENG-42`) or its id; `{id}` is a page's id. A body
naming a different object than the path is refused `400` rather than
overwritten, and a route that is a narrower door onto a wider tool — `/depend`,
`/relate`, `/tags`, a project's policy — refuses an argument that belongs to
another. A body over 1 MiB (twice a page at its own cap, for JSON escaping) is
refused `413` before it is read.

**Not here, and deliberately:** sprints and goals. The tracker has neither —
both left the domain — so there is nothing to route.

### What a request answers

| Status | When | Carries |
|---|---|---|
| `200` | The write was **applied** on this node, so the next read here sees it | the tool's own receipt, its `outcome`, `position` and `op_id` |
| `202` | The write is **pending**: durable, and not yet applied on this node | the same, and the `position` to read at |
| `401` | Nobody presented a credential | |
| `403` `forbidden` | The authority table refused | `detail`: the refusal, worded exactly as a seat and your assistant are told it |
| `404` | The item, page or comment does not exist — or this node runs no such backend | |
| `409` `stale` | Somebody changed it after you read it: a stale `If-Match`, a title taken, a race lost | `detail`: read it again |
| `422` `refused` | The domain refused the write on its own rules | `detail`: the domain's own sentence |
| `503` | This node could not decide (it cannot tell who you are, or cannot read the chart yet), or cannot establish whether the write landed | a `Retry-After`, and — for an unknown outcome — the `op_id` to retry with |

The `403` wording is the point of the second row: the three surfaces that serve
these verbs refuse in ONE sentence, formed by one function, so a person told one
thing by the dashboard and another by their assistant is never left asking
which is wrong.

### A retry is the same operation

Send an `Idempotency-Key` header and every record the request writes derives
its operation id from it; send none and a fresh key is minted and handed back
as `op_id`. An `unknown` outcome is the one to retry — **with the same key**,
sent back as `Idempotency-Key`, because a fresh one would defeat the ledger
that makes a retry safe and a retried create would file the item twice.

### A purge

`POST /work/items/{key}/purge?confirm=<KEY>&reason=<why>` destroys an item and
every row it produced, on every node, and nothing undoes it. It replaced
the purge route that stood beside the retention gestures, and it differs in the
three places that one was wrong:

- **`?confirm=` is checked**, against the item the path resolves to — the key a
  person sees on the board, not the id a script carries. A mismatch is `422`
  and destroys nothing.
- **The project is the stored row's.** There is no `?project=`: a key's prefix
  names where the item was *filed*, which a move leaves behind as an alias, and
  a purge filed under the wrong project blocks writes to a project it is not
  about.
- **A retry reuses the operation** through `Idempotency-Key`, like every write
  here, rather than an `?op_id=` of its own.

`?reason=` is required: the rows are destroyed and the deletion marker's reason
is the whole account of what used to be at that key. A node that is offline or
evicted keeps its copy until it replays, adopts a snapshot, is replaced or is
destroyed. `crewlet work purge` is the same route from a shell.

### Somebody else's inbox

`mark_inbox` and `set_pins` write the **caller's own** record and take no
handle — a model that could name whose inbox to mark could mark anybody's. So
`PUT /work/people/{handle}/inbox` with your own handle goes through the tool
exactly as your assistant's call would, and with **somebody else's** — which
only an administrator is admitted to, to unstick a departed person's queue —
through the same parsing and the same writer, with the table's answer as its
authority. The handle is a **seat** handle: a person's record in the tracker is
keyed on the seat they hold.

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
longer than one that caught up in milliseconds. A refusal that waiting cannot
clear — a node holding a record its build cannot decode — is **not** a `503`,
because a client told to come back would go round a loop that cannot
terminate; those are ordinary failures and the log names them. See
[Read Consistency](../guides/consistency.md).

**The reads are here and the writes are [the write surface's](#the-human-write-surface).**
Every write is attributed to somebody — a seat through its own tools, a person
or a credential through the write surface or the
[MCP surface](../guides/tools-and-mcp.md) — and never to "the dashboard",
which is not a person and not a seat and cannot be asked why. The routes under
`/work/retention/` below are not about items: they are operator gestures
against the log's own history, attributed to the token that made them.

### Paging and filters

Both listings take `limit` (default 50, max 500). **They page differently, and
that is not an inconsistency.** The pages listing takes `offset`, because a
page's order is a title within a container and a reader scrolling it is
reading a list somebody arranged. The work listing takes an opaque `cursor`
instead, echoed as `next_cursor` on every answer that has more: a board is
ordered on values seats are changing while it is read, and an offset over a
moving set skips rows and repeats rows with nothing to say it did.

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

**Operator-only, reads included**, on the same rule `/config` and `/secrets`
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
      "headroom_fraction": 0.984,
      "bytes_per_day": 23068672,
      "trim_floor": 918100000,
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

`headroom_fraction` is a **pointer** and is absent when the broker could not be
asked. A fraction of an unknown ceiling is not zero headroom, and zero is what
the one alarm an operator cannot ignore fires on.

`bytes_per_day` is what the log took in over the trailing day, as this node's
last trim tick measured it from the log's own records — the rate the
`log_ceiling_short` alarm holds `max_bytes` against `min_age` of. It is a
pointer too, for the opposite reason: `0` is a log that took in nothing, the
most benign reading there is, and the field is **absent** where nothing was
measured — a compacted log (whose size follows its subjects, not its age), a
log younger than a day, and every log on a node whose trim has not ticked yet.
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
| `POST /work/retention/evict/{node}?confirm={node}` | Installs the eviction gate. |
| `POST /work/retention/readmit/{node}?confirm={node}` | The inverse commit. |

`confirm` echoes the node id, and a mismatch is `400`. Both gate routes answer
with the write's **three-valued outcome** — `applied`, `pending` or `unknown` —
its position and its operation id: a gate the caller believes has landed and
which is only `pending` is the difference between a node that has stopped
writing and one that is about to.

A node whose company runs no native tracker has no eviction gate, and both gate
routes answer `503 no_tracker` rather than `404`. The routes exist on this
build, and telling an operator they do not sends them looking for a version
mismatch that is not there.

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
| `POST /work/retention/maintenance/abandon?stream=NAME` | From `opened` clears the operation outright; from anywhere else enters the seal. |
| `POST /work/retention/maintenance/exclude?stream=NAME&node=ID&confirm=ID` | Waives one participant's acknowledgement and withdraws its admission. |

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
| `GET /work/retention/reanchor?stream=NAME` | The stream's own `created_at` and the current generation. |
| `POST /work/retention/reanchor?stream=NAME&confirm=<created_at>[&force=true]` | Runs the generation transition, answering with the new generation. |

`confirm` is the value the `GET` returns, supplied by the caller: the
confirmation means *I looked at the thing I am re-anchoring*, so the two are
deliberately separate round trips rather than one route that reads and acts.
`force=true` is refused-by-default's escape, for a fleet whose hydrated peer
cannot be reached — adopting that peer's snapshot is strictly the better
recovery, and the refusal names it.

## Agent Memory

### `GET /agents/{id}/memory`

What one seat has learned, in one round trip. Also served as the
`agent_memory` query.

`{id}` is the seat's **handle** — the canonical identifier everywhere in
the system. The two halves are keyed differently in the store (the diary
by the derived agent id, the episodes by the handle), and this route
resolves that itself rather than making a caller know which.

```json
{
  "id": "<handle>",
  "diary": [
    { "id", "content", "retention", "source", "turn_id",
      "created_at", "ttl_until", "retrievals" }
  ],
  "episodes": [
    { "id", "turn_id", "agent_handle", "task_summary", "plan_summary",
      "review_outcome", "tool_sequence", "skills_used",
      "conversation_key", "work_key", "created_at", "ended_at",
      "duration_ms", "compacted", "count" }
  ],
  "skills": [
    { "id", "key", "title", "summary", "version", "updated_at", "uses" }
  ],
  "skills_total": 0,
  "counterparties": [
    { "observer_handle", "subject", "summary", "updated_at" }
  ],
  "onboarded_at": ""
}
```

**Every key is present on every answer**, as an empty list rather than an
absent one. A caller cannot tell "this seat has learned nothing" from
"this node does not keep that half" if the key is simply not there, and
both are ordinary states.

The rows are **projected here**, at the API boundary, rather than being
the learning package's own structs marshalled directly. Two reasons, and
the first is not stylistic: those are domain types whose fields exist for
the recall path, they carry no `json` tags, and marshalling them shipped
Go field names plus every row's raw embedding vector — up to a hundred
`float32` arrays per request — to a screen with no use for one. Second,
a wire shape belongs where the wire is.

Sources:

* **`diary`** — the seat's private observation log, written by
  `reflect_and_persist` and the reflection pass. `retention` is `long` or
  `short`; a short entry carries the `ttl_until` it expires at.
  `retrievals` is how often it has actually been recalled, which is the
  difference between a memory that keeps proving useful and one written
  once and never read.
* **`episodes`** — one row per completed turn, newest first, capped at
  50. `duration_ms` is milliseconds: a Go `time.Duration` marshals as an
  integer count of NANOSECONDS, which renders as a plausible and wildly
  wrong number.
* **`skills`** — the seat's own synthesized skills, drafted from its
  repeated work and loadable mid-turn via `use_skill`. Archived rows are
  hidden and stale ones shown, because a stale skill still works and
  still revives on use. Capped at 50 like the two above, and
  **`skills_total`** is how many the seat actually has — the diary and the
  episodes ask their store for a recency feed, where "the most recent 50"
  is the question, but skills are a set a seat loads from, so the store
  returns a bounded page and counts the set beside it. Render the total,
  not the length of the list.
* **`counterparties`** — profiles built up from observed interactions.

The table is strictly per-agent; cross-agent procedural artefacts are
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

**It needs a token, reads included**, like every other answer the
dashboard's Admin workspace draws. What it describes is the DEPLOYMENT
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
send them to a thread that does not exist.

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

Backs the dashboard's **Spend & budgets** screen. A token budget is described by
two numbers that share a span, and one stamp:

- the **cap** is configuration, from the active company revision;
- **durable usage** is the fleet's shared counter, in the
  [coordination store](../concepts/coordination.md), written by every node
  running the company and surviving restarts, until an operator resets it. It
  is what the engine actually enforces against, and it is the same counter the
  [live token meter](#the-live-token-meter) pushes;
- **`refused_at`** is when that scope last turned a charge away, kept in the
  same counter and cleared by the scope's next admitted charge — or by
  [`POST /budgets/reset`](#post-budgetsreset), which drops the counter and the
  stamp together, since an operator who zeroes a counter has made room.

What a seat *spent over a window* is not here: that is the per-agent row of the
[spend breakdown](#get-tokensbreakdown), a different span that must not be
divided into a cap. The cap and the durable counter are the pair that can be,
which is how this screen can say "this seat has burned 94% of its cap across two
restarts". That was reachable only from `crewlet budgets show` before, which is
itself a client of this route.

```json
{
  "durable": true,
  "org": {
    "max_tokens": 5000000, "durable_used": 1284410,
    "durable_updated_at": "2026-06-08T07:30:02Z",
    "refused_at": ""
  },
  "seats": [
    {
      "agent_id": "<uuid>", "role": "Engineer", "handle": "eng",
      "max_tokens": 100000, "durable_used": 99120,
      "durable_updated_at": "2026-06-08T07:29:51Z",
      "refused_at": "2026-06-08T07:29:51Z"
    }
  ]
}
```

`durable` carries the honesty. It is `false` when the shared counter could not
be read: a counter that cannot be read is not a counter that reads zero, and
without the flag a coordination blip renders every seat at the bottom of its
cap, which is the most reassuring possible picture drawn at the moment nothing
is known. Human seats have no row, because they spend nothing.

Exhaustion is `refused_at`, the moment a charge was turned away, never
`durable_used >= max_tokens`. The gate refuses a charge that would exceed the
cap and increments nothing, so a seat charged in 3k-token rounds against a 100k
cap stalls near 99k and never compares equal to its own maximum. A ratio test
shows a permanently blocked seat at 99% and calls it healthy. A scope known
only for a refusal (refused on its very first charge) is listed with no spend
and an empty `durable_updated_at`.

### `POST /budgets/reset`

Zeroes the fleet's token counter. `?scope=` names one (`org`, `agent:<id>`);
its absence clears every one.

```bash
curl -X POST -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  "http://localhost:8000/budgets/reset?scope=agent:<uuid>"
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

Two refusals, deliberate: **401 without a credential**, and **403 without
`fleet:operate`** — clearing a spend ceiling is the deployment's own control,
not something a reader of the board or a pipeline filing work may do. There is
no "no counter here" refusal beside them, because every node opens the fleet's
coordination store that holds the counter.

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

This route exists for the same reason the budget reset does, twice over. The
store is locked to the engine's process and the driver refuses a second
process on a database file, so nothing outside can read it; the embedded
broker binds no socket, so nothing outside can reach the stream estate either.
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
  or a path the database engine mishandles. The reason is returned in `detail`
  rather than only logged, unlike every other route here, because it is the
  caller's own command to fix.
- **A copy without the stream estate.** A node that dialled an external NATS
  cluster has no connection to snapshot the streams over, so its manifest
  carries the store copies alone and `crewlet backup` says where the rest
  lives. Back that half up at the cluster, from the same moment.

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
  ]
}
```

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

`recent_runs` is empty when the dispatch ledger cannot be read (the
configured list and `next_run` still render). Disabled schedules return an
empty `next_run`.

`scope_id` is the scope's **identity** — a seat's agent id for a role schedule,
a unit's origin key for a unit one — which is what the at-most-once ledger keys
a fire on and what `schedule_runs` takes as its parameter. `scope_name` is the
same scope as a person reads it (the handle, or the unit's key), resolved
through the company this node is running: a renamed scope reads under the name
it answers to now, on every row of its history.

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
| `since` / `until` | (the `since_days` window) | RFC 3339 instants, and the pair a time-range control produces. The window is **half-open** — `[since, until)` — so two adjacent windows share their boundary instant without either losing it or counting it twice, and one that ends where it begins is refused rather than answered as a quiet company. `since` is floored at the store's 30-day retention. The same pair `GET /tokens/series` takes, so a reader scrubbing a range sees the figures and the chart move together. |
| `since_days` | `7` | The same window as a count of days back from now, for a caller that has no instants. Clamped to `[1, 30]` — the event store keeps 30 days. Ignored when `since` or `until` is given. The **whole** window is folded either way: there is no row cap, so the number is the window's real total rather than a prefix of it. |
| `agent_role` | (none) | Restrict to one role. Used by the agent detail page's per-phase summary. |
| `recent_turns` | `50` | Cap on the per-turn list. |

The answer is labelled with the window it actually **covered**, never with the
one that was asked for: a request further back than the retention is floored,
and a rollup headed with a year over a month of rows is a lie about the numbers
beside it.

**Response**

```json
{
  "since": "2026-06-08T12:00:00Z",
  "until": "2026-06-15T12:00:00Z",
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
  [Turn Engine](../concepts/turn-engine.md): `onboarding`, `execute`,
  `review`, `subagent` (a delegated worker), `auxiliary`, and `judge`
  (the round-cap extension judge). A store that predates the two-stage
  redesign also holds `plan` rows, and they still roll up.
- `by_worker` covers the rows that name one: an `auxiliary` row's worker
  is the learning-subsystem caller (e.g. `persist_decider`,
  `counterparty_profiler`, `skill_synthesizer`), and a `subagent` row's
  is the `workers:` template it ran — empty on a delegation that wrote
  its prompt inline.
- `by_model` is useful when roles override `llm_auxiliary` with a
  cheaper model for reflection / summarisation work.
- All lists are sorted by `total_tokens` descending; `by_turn` is
  sorted by `ended_at` descending and capped at `recent_turns`.
- `aggregated_through` is the latest event timestamp this rollup
  aggregated, and empty when no events matched. It is the rollup's own
  freshness: the dashboard renders it as "counted through", so a reader
  looking at a total knows how recent the last thing in it is. It is not a
  baseline a client folds onto: the whole rollup is re-folded and pushed
  by the server, which is what keeps one aggregation rather than a second
  one in the browser.
- Returns the same skeleton with zero totals (and an empty
  `aggregated_through`) when the event store is unavailable rather than
  erroring.
- Every bucket — the totals, each row, and each nested `by_phase` entry —
  also carries `cost_usd` and `priced_calls`. **Two numbers, because zero
  dollars is two different facts**: only a subscription coding CLI reports
  a price, so a `cost_usd` of 0 over `priced_calls: 0` means nobody said
  what this cost, while 0 over 3 means three runs were billed nothing.
  Rendering the first as `$0.00` states a price nobody quoted. Only a
  POSITIVE price is summed — a negative one is a bad payload, not a
  rebate, and summing it would silently reduce a company's reported spend.

### `GET /tokens/series`

The same spend with a **time axis**: one bucket per hour or per day over a
window, each split into bands on one dimension.

**Query parameters**

| Name | Default | Description |
|------|---------|-------------|
| `group` | `phase` | The dimension the bands are: `phase`, `model`, `seat`, `unit`, `worker` or `turn`. Anything else is refused naming the set. `unit` resolves through the org chart's DIRECT unit for each seat — not the chain, because a band per nesting level would count the same spend for the team and again for the department above it. A record carries no project and no work item at all; that attribution is the tracker's own per-item counters. |
| `bucket` | `hour` | `hour` or `day`, in **UTC**. Two, deliberately: a chart with an arbitrary bucket width has an x axis nobody can label. |
| `since` / `until` | (the `since_days` window) | RFC 3339 instants. The window is **half-open** — `[since, until)` — so two adjacent windows share their boundary instant without either losing it or counting it twice. `since` is floored at the store's 30-day retention, and the answer is labelled with the window it actually COVERED rather than the one asked for. An absent `until` runs to now, so a company quiet for six hours has six empty buckets rather than a chart that stops where the spending did. |
| `previous` | `false` | Shift the window back by its own length, for compare-to-previous. Needs both edges — the window before an open-ended one has no length. Computed here rather than in the browser: a client subtracting in local time produces two windows of different lengths across a DST boundary, and the chart then reports a change nobody made. |
| `groups` | `5` | How many bands before the rest fold into the residual. Capped at 20. Five is how many distinguishable hues the design system has. |
| `agent_role` | (none) | Restrict to one seat. Accepts a handle or a role name. |

**Response**

```json
{
  "group": "phase",
  "bucket": "hour",
  "since": "2026-06-14T12:00:00Z",
  "until": "2026-06-14T15:00:00Z",
  "series": [
    { "at": "2026-06-14T12:00:00Z",
      "input_tokens": 60, "output_tokens": 20, "total_tokens": 80,
      "calls": 1, "cost_usd": 0, "priced_calls": 0,
      "groups": { "plan": { "total_tokens": 80, "calls": 1, ... } },
      "other":  { "total_tokens": 0, "calls": 0, ... } },
    { "at": "2026-06-14T13:00:00Z", "total_tokens": 0, "calls": 0,
      "groups": {}, "other": { "total_tokens": 0, ... }, ... },
    ...
  ],
  "by_group": [
    { "group": "execute", "other": false, "folded": 0,
      "total_tokens": 16000, "calls": 2, "cost_usd": 0.74, "priced_calls": 2 },
    { "group": "", "other": true, "folded": 12,
      "total_tokens": 300, "calls": 9, "cost_usd": 0, "priced_calls": 0 }
  ],
  "totals":  { "total_tokens": 20450, "calls": 6, ... },
  "grouped": { "total_tokens": 16300, "calls": 5, ... }
}
```

Notes:

- **Every bucket in the window is present, including the empty ones.** A
  series with holes is a chart the client has to repair, and repairing it
  in the browser is the copy of this bucketing the engine exists to have
  written once. A quiet hour is a gap of full height, not a column the
  chart squeezed out.
- `by_group` is the **legend and the grid**: each band's total over the
  whole window, biggest first, with the residual last. Which bands survive
  the cap is decided over the WHOLE window, never per bucket — a per-bucket
  decision would put a band in the chart for the hours it happened to lead
  and in the residual for the rest, which reads as spend that stopped.
- The residual carries an **empty `group` and `other: true`**, rather than
  a reserved name: a phase, model or seat genuinely called `other` must not
  be mistaken for the fold. `folded` is how many distinct groups it stands
  for, so a legend can say "other (12)".
- `totals` is every record in the window, **including the ones this
  grouping places in no band at all** — grouping by `worker` leaves out
  every phase that is not a worker's, and by `turn` every phase that
  carried no turn id. `grouped` is what the bands do cover, so the gap is a
  number rather than an inference a reader has to make by subtracting.
- `at` is the bucket's **start**, never its middle or its end. A bucket
  reaches from `at` to `at` plus one hour or one day.
- A window longer than 1000 buckets keeps the **newest** of them and
  reports `since` as what it drew: a cost explorer is read from its
  right-hand edge, and dropping the oldest silently would put a year's
  heading over a month of bars.

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
| `tags.recipient` | The seat a per-seat delivery was addressed to, absent for a company-wide one. It is one of the four keys the log indexes as a **party**, so `GET /events?agent=<handle>` also returns what reached that seat from outside |
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

### `/webhooks/jira`

Receives **Data Center** Jira webhook payloads (issue created, updated, commented, assigned). Verifies HMAC-SHA256 over the raw body against `X-Hub-Signature`, keyed on `integrations.jira.webhook_secret`; a route with no resolved secret answers 503 rather than accepting the delivery. Deduped on `X-Atlassian-Webhook-Identifier`, which is stable across Jira's own retries. **Jira Cloud does not use this route** — a Cloud webhook belongs to an app, so those events arrive through [`/webhooks/forge`](#webhooksforge) with their own JWT, and `webhook_secret` is unused there. Publishes to `crewlet.notifications.inbound`. See [Jira Integration — Webhooks](../integrations/jira.md#webhooks-jira-pushes-to-agents).

### `/webhooks/slack/{handle}`

Receives Slack Events API payloads for a specific agent (identified by handle). Verifies the signing secret for **that agent's own app** — Slack gives each seat its own, so the handle in the path is what selects the key. Publishes to `crewlet.notifications.inbound`. Slack's `url_verification` challenge is answered unconditionally (no engine or company config needed), so a freshly provisioned app's Request URL verifies even before the engine is configured — it has to, because during provisioning that app's signing secret does not exist yet. See [Slack Integration](../integrations/slack.md).

### `GET /webhooks/slack-oauth`

The OAuth install landing page for [`crewlet slack provision`](../integrations/slack.md). Every provisioned Slack app has this as its OAuth redirect URL. After the operator approves an install, Slack redirects here with a temporary `code` (and `state` carrying the agent handle); the page displays the code for pasting back into the waiting CLI prompt. Unauthenticated by design: the code expires after 10 minutes and is useless without the app's client secret, which only the provisioning CLI holds. Every value on the page comes from the query string, so it is served under a policy that allows its one inline style by hash and no script at all (see [Security headers on every response](#security-headers-on-every-response)).

### `/webhooks/github`

Receives GitHub webhook payloads. Verifies HMAC-SHA256 over the raw body against the `x-hub-signature-256` header, keyed on the required `webhook_secret` from the `github` config block; invalid or missing signatures are rejected with 401, and a route with no resolved secret answers 503 with a `Retry-After` so the delivery is held for retry rather than blamed on the sender. Deliveries are deduped on `X-GitHub-Delivery`, which is stable across GitHub's own retries and an operator's manual redelivery. **The event name is in the `X-GitHub-Event` header**, not the body — the payload carries only the action — so the header is carried onto the envelope and read by the parser. Publishes to `crewlet.notifications.inbound`. The same handler serves `POST /webhooks/github/{handle}`, which is the address a seat's own [GitHub App](../integrations/github.md#one-github-app-per-agent) is created with: the handle travels onto the published event so five agents' apps reporting one comment are five wakes rather than four duplicates, and both forms verify against the same company `webhook_secret`, so a seat in the path is not a way past the signature check. See [GitHub Integration — Webhooks](../integrations/github.md#webhooks).

### `GET /webhooks/github-app`

Where GitHub returns an operator's browser during the per-agent
[GitHub App](../integrations/github.md#one-github-app-per-agent) flow, and one
of the two `/webhooks/*` routes that render a page rather than accept a delivery
(the other is [`/webhooks/slack-oauth`](#get-webhooksslack-oauth)).
Two arrivals, one route: after the app is **created**, with a one-time code to
convert, and after it is **installed**, with nothing but `?installed=<handle>`.
Unauthenticated, because a redirect from GitHub carries no engine credential;
the `state` minted by [`POST /setup/integrations/github/app`](#one-agents-own-github-app)
stands in its place and is a signed token naming the seat, checked before the
code is converted — and **spent** there, so a state that reached a browser
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

Receives GitLab webhook payloads. **The signature is the only credential**: `webhook-signature` is verified as a Standard-Webhooks HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}`, keyed on the `signing_secret`'s base64 payload, constant-time against any of the header's space-separated `v1,…` entries, with a ±5-minute timestamp tolerance. A missing or wrong signature is rejected with 401 — the plaintext `X-Gitlab-Token` is not accepted, so omitting the signature header is not a downgrade path. Answers 503 with a `Retry-After` when no `signing_secret` is configured, or when its value is not a usable `whsec_` key, so the delivery is held for retry rather than blamed on the sender. GitLab signs whenever the hook has a `signing_token` (GitLab 19.1+); see [GitLab § Verification](../integrations/gitlab.md#verification). Publishes to `crewlet.notifications.inbound`. See [GitLab Integration — Webhooks](../integrations/gitlab.md#webhooks).

### `/webhooks/confluence`

Receives **Data Center** Confluence webhook payloads (page created/updated, comments). Verifies HMAC-SHA256 over the raw body against `X-Hub-Signature`, keyed on `integrations.confluence.webhook_secret`; a route with no resolved secret answers 503. **Confluence Cloud does not use this route** — those events arrive on [`/webhooks/confluence/{event}`](#webhooksconfluenceevent) or through [`/webhooks/forge`](#webhooksforge), which is why `webhook_secret` is required on Data Center and unused on Cloud. Publishes to `crewlet.notifications.inbound`. See [Confluence Integration](../integrations/confluence.md).

### `/webhooks/confluence/{event}`

Receives one **Confluence Cloud** event, named by the path because a Cloud payload does not say which event fired — the registered URL is the only thing that knows. Cloud signs nothing and honours no registration field for a header, so the authentication is a **shared token**, compared constant-time: `X-Crewlet-Token` is read first and `?token=` in the query is the fallback, which is where `crewlet confluence provision` puts it. A route whose `integrations.confluence.webhook_token` is unset answers 503, and so does one whose token is shorter than 26 characters — the token is the entire check, so its length is the entire strength. The engine never logs the query string on this route. Deduped on a hash of the raw body, because Cloud sends no per-delivery identifier. See [Confluence Integration — Webhooks](../integrations/confluence.md#webhooks-confluence-pushes-to-agents).

### `/webhooks/datadog`

Receives a Datadog **monitor alert**. Datadog's Webhooks integration attaches custom headers with fixed values only, so there is nothing varying with the payload to sign and the authentication is a **shared token**, compared constant-time against `X-Crewlet-Token` and keyed on `integrations.datadog.webhook_token`. An unset token answers 503, and so does one shorter than 26 characters. A mismatch is 401. Deduped on the payload's own `id`, which Datadog repeats across its retries. The alert routes by the monitor's TAGS — `crewlet:<handle>` by default — falling back to `integrations.datadog.route_to`, because a monitor is addressed to nobody. Publishes to `crewlet.notifications.inbound`. See [Datadog Integration](../integrations/datadog.md).

### `/webhooks/forge`

Receives events from the Atlassian Forge app. Every request must carry a Forge Invocation Token (FIT) as an `Authorization: Bearer` JWT; the token is verified against Atlassian's JWKS endpoint and its `aud` claim must match the configured `forge_app_id` (401 on failure, 500 when no app id is configured). The request body is drained **before** FIT verification — verification can block on a JWKS fetch, and the body must be off the socket before the sender's delivery deadline aborts the request. Maps `avi:jira:*` / `avi:confluence:*` events onto the native Jira/Confluence pipeline and publishes to `crewlet.notifications.inbound`. Self-generated events (an agent's own actions echoed back by Forge) are acknowledged and dropped. Jira Cloud and Confluence Cloud both ride this route and are served end to end — see the integration pages.

### Aborted deliveries (client disconnects)

Webhook senders enforce delivery deadlines and abort requests that respond too slowly. When a sender hangs up before the request body is fully read, the read fails part way: there is nothing to verify and nobody left to tell, so the receiver logs `webhook_body_unreadable` (`component=api.webhooks`, keyed by `path` and `error`) and still writes a `400` — a handler that returns without writing one answers `200`, telling the sender a delivery it abandoned was accepted. The aborted delivery is dropped, and whether it is redelivered is up to the sender's retry policy, so recurring `webhook_body_unreadable` warnings on a webhook path mean events are being lost because the API is answering too slowly.

The body is read **whole even when the request will be refused**, and bounded at 25 MiB (`body_too_large`, then `413` — every JSON surface answers a 413 with that one code) and at 30 s (see [Request timeouts](#request-timeouts)). Answering without draining leaves unread bytes in the socket and the sender sees a connection reset instead of the status — which for a `401` reads as "retry forever" rather than "your signature is wrong".

---

## Running

```bash
crewlet run -config crewlet.yaml -roles ingress -api-host 0.0.0.0 -api-port 8000
```

The API is read-only against the database, and the one thing it publishes is inbound webhook deliveries, onto `crewlet.notifications.inbound`. It does not run agents — the engine process handles that.

See [Deployment](../guides/deployment.md) for how the API and engine run together, and the integration docs ([Slack](../integrations/slack.md), [Jira](../integrations/jira.md)) for webhook setup.
