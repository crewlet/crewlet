# API Endpoints

The Crewlet API is served by any node with the `ingress` role (`api.port > 0`
in the Tier A config). By default a node has every role, so it runs alongside
the agents in one process; a node with `-roles data,ingress` serves only these
routes and reaches the rest of the fleet over the event stream. The routes
below are identical either way.

A node **without** the `ingress` role binds `api.port` too, and serves the
network only its two probes there — `/health` and `/ready`: see
[Probes on a node without ingress](#probes-on-a-node-without-ingress). An
executor run in a box off its host reaches its tools through an ingress node's
[tool bridge](#routes), which relays each call to the node holding the seat; a
`direct` run on its own host dials it there on loopback, where it serves the
bridge to its own host alone.

There is nothing to install for it — it is compiled into the binary, along
with the dashboard it serves and the WebSocket that is the dashboard's data
plane (see [`WS /ws/stream`](#ws-wsstream)).

---

## Which listener serves what

Every route below is served on `api.port`, unless the node's Tier A sets
`api.public.port`. Then the routes **outside parties** call — the ones that
authenticate by a provider signature or a signed per-run token, never an
operator credential — move to that listener and are served nowhere else:

| Prefix | Who calls it | Without `api.public` | With `api.public` |
|---|---|---|---|
| `/webhooks/*` | Vendors (deliveries) and a person's browser returning from a vendor's app flow (`/webhooks/slack-oauth`, `/webhooks/github-app`) | `api.port` | `api.public` only |
| `/otlp/{token}/v1/{signal}` | A box exporting its executor's telemetry | `api.port` | `api.public` only |
| `/mcp/{token}` | A seat's executor run, in its box, calling the seat's tools — and, beneath it, polling for its turn's notes (`/mcp/{token}/steer/{notes,read}`) | `api.port` | `api.public` only |
| Everything else: `/health`, `/ready`, `/`, `/dashboard`, `/favicon.ico`, `/static/*`, `/auth/*`, `/iam/*`, `/ws/stream`, `/query/*`, every REST read, `/config`, `/secrets`, `/setup`, `/operator/*`, `/backup`, the `/worklet/*` and `/fleet/*` gestures | People, the dashboard, the CLI, orchestrators | `api.port` | `api.port` only |

A public route asked of `api.port` answers `404` with the code `no_route` and a
`hint` naming `api.public`, since whoever asks there is on the operator's own
network, most likely a vendor configured with the wrong port. Every other route
asked of the public listener answers exactly what a path nothing serves answers
— the same `404 no_route`, the same generic hint — so the published socket says
nothing about the admin routes behind it. It asks for no credential and
decides before the guard runs, so a guarded route there is that same `404`
whatever credential is sent. The probes are deliberately
**not** public: `/health` describes the node to whoever runs it, so point every
health check at `api.port`. A node without the `ingress` role serves the
network its [probes](#probes-on-a-node-without-ingress) on `api.port` and
nothing else, on either listener — beside them only the routes its own boxes
dial, to its own host's loopback — and nothing at all when its `api.port` is
`0`. See
[Deployment → Exposing webhooks without the admin API](../guides/deployment.md#exposing-webhooks-without-the-admin-api).

---

## Request timeouts

Three deadlines bound a request on **every** route below, and each covers a phase the others do not. None is configurable — they are properties of what the surface is for, not of a deployment.

| Phase | Bound | What it stops |
|---|---|---|
| Request line and headers | 10 s | A connection opened and left silent — the cheapest denial there is against a listener. |
| Reading the request body | 30 s | A client that *dribbles*: a body under every size cap, delivered a few bytes at a time. The size cap and this are different failures, and a cap alone stops only the first. 30 s carries the largest body any other route accepts — a 25 MiB webhook delivery — at roughly 7 Mbit/s sustained, far below what any forge, CI runner or operator workstation delivers. A project file's upload is the one exception: it streams up to a gibibyte, which no single deadline could carry without capping a real upload, so it is held instead to thirty seconds for each mebibyte (see [Project files](#project-files)). |
| Between keep-alive requests | 60 s | A client that completed one request and then went quiet while holding its connection slot. |

A body that does not arrive inside its deadline fails the read like any other truncated delivery: `400` with `unreadable_body` (logged as `webhook_body_unreadable` on a webhook path). There is deliberately **no** whole-request timeout: it would have to be large enough for the largest body on the slowest link, which makes it no bound at all on a small one. Long-lived responses — [`WS /ws/stream`](#ws-wsstream) above all — bound themselves.

---

## Every refusal is one envelope

A refused request — on any route, from any surface, whatever the status — is
one JSON object, and it always has the same three parts in the same places:

```json
{
  "error": "no_external_url",
  "message": "No external address is set. Set `api.external_url` in this node's config file and restart.",
  "config_path": "api.external_url",
  "hint": "Set the HTTPS address third-party apps reach this deployment on; without it the pass can register no webhook."
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
  more to say says it in the detail rather than rewording the sentence. A code
  whose refusals have different remedies picks its sentence by the facts the
  detail carries, never by which route wrote it: `unauthorized` says the rule
  its `reason` names ([below](#which-permission-a-route-needs)), and a `503`
  `unavailable` or `identity_unavailable` with no `Retry-After` says that
  waiting won't change it, where the same code under a `Retry-After` says to
  try again in a moment (a write whose `outcome` is `unknown` keeps the code's
  own sentence, since it may have landed).
  Never match on it: the wording is copy and can be improved at any time. Every
  refusal carries one — `/config`, `/secrets` and `/setup` included,
  which used to build their own bodies with the code alone, and every
  `/webhooks/*` route, which answered a code with a space in it
  (`invalid signature`) and a `{"status": "unavailable", "reason": …}` shape of
  its own ([Webhook refusals](#webhook-refusals)).
- **Everything else is the detail** — the machine-readable facts about *this*
  refusal, as typed JSON beside the two reserved keys rather than nested under
  one: `config_path` and `hint` above, `fields` on an integration that is
  missing values, `current_revision_id` on a lost update, `problems` on a
  refused configuration document. Values keep their own types,
  so a count is a number and a list of located problems is a list. The two
  text keys a person reads, `detail` (what happened) and `hint` (what to do),
  are always written as sentences: a capital first letter, unless the text
  opens with a field name, flag, path, address or Go package prefix
  (`statelog: …`), and a full stop. The
  WebSocket's error frames write their `detail` the same way.
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
  with a `503` (`/worklet`, `/doclet/pages`, `/chatlet`, `/artifacts` with its
  datastore's `/artifacts/{id}/data`, `/artifact-folders`, `/brand`,
  `/operator/act`, `/iam`, `/auth`, `/config`, `/setup`, `/secrets`, `/assistant`,
  and every question the query registry answers, `/query/{what}` and its
  named read routes): the same request is
  refused however often it is sent, so the answer is to ask another node, to
  split the change, or for an operator to readmit, upgrade or resize, never to
  poll this one. The detail says which in a sentence for the person reading
  it — what happened and what to do, asking them to try again in a moment
  exactly where a `Retry-After` is sent — and never in the refusal's own
  text, which names positions, settings and the broker's words: that goes to
  the node's log. `/operator/act`, and the human write surface's tool-backed
  routes and tool-less verbs, carry no detail instead (ADR-0060): the code's
  own `message` stands alone beside the header. [Read
  consistency](../guides/consistency.md#a-writes-refusals) says what each one
  asks of whom.
- **A write that may have landed is not a refusal, and says so as a field.**
  A `503` about a write this node cannot account for — its outcome is
  `unknown` — carries **`"outcome": "unknown"`** and the **`op_id`** to retry
  under, on every surface that writes through a state log (`/worklet`,
  `/doclet/pages`, `/chatlet`, `/artifacts` with its datastore's
  `/artifacts/{id}/data`, `/artifact-folders`, `/brand`, `/operator/act`, `/iam`,
  `/auth`, `/assistant`); a `503` without `outcome` is
  a refusal, and what it refused was not written — a gesture refused partway
  lists what did land before it as `landed`, and a `/setup` submission the
  credentials it sealed first (below). The two send a client opposite ways — the
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
  The config surface and a [`/setup`](#supplying-them) submission carry no
  operation, so their unknown has no `op_id`: it names the revision the write
  stored and the `node` whose history alone holds it, and the pointer says
  whether it landed — it is not sent again either (see [`/config`'s status
  codes](#status-codes)). A `/setup` submission's `503` **without** `outcome`
  left the company document as it was, and is still not a write that wrote
  nothing: it names the credentials it sealed before the refusal as
  `wrote_secrets`, and the one whose own write the store did not answer — which
  may have landed — as `unanswered_secret`. Its remedy is the plain `503`'s all
  the same, sending the submission again as it stands, because that writes every
  credential in it again by name.
- **A `500 internal_error` carries no reason.** Its sentence says the reason
  is in this node's log, and that is where it is: a fault's own words are a
  store's or a driver's — a database path, a connection string — and holding
  a permission does not make a caller somebody they are meant for. A route may
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

## While a node is starting

`crewlet run` binds `api.port` — and `api.public` and `api.artifacts`, where
the node serves them — as soon as it has read and checked its Tier A, before it
opens its store, and logs `api_bound`. A port somebody else holds therefore fails the boot at once,
naming the address. Until the node's own surface takes the socket over (logged
as `api_listening`, or `api_probes_listening` on a node without `ingress`), it
answers its probes and tells a caller to come back for any route it will
serve — and only for one it will serve:

| Request | While the node starts | Why |
|---|---|---|
| `GET /health`, on `api.port` | `503` while the node recovers what it holds, then `200`, `status: "starting"` both times | The `503` is what a startup probe sized for the node's store — and, on a data node of a fleet, for the join it may have to make — bounds; nothing after the recovery holds it. See the phases below. |
| `GET /ready`, on `api.port` | `503`, `reason: "starting"` | The node does no work and takes no traffic until it serves, so no load balancer and no rollout counts it. |
| A route this listener will serve | `503 starting`, with `Retry-After: 10` | The caller is told to come back rather than meeting a refused connection, which reads as a node that is not there. On an `ingress` node that is every route of [Which listener serves what](#which-listener-serves-what) on the listener that serves it; a node without `ingress` serves the network no route but its probes, and a node without it that runs seats tells a caller on its own host's loopback to come back for `/mcp/` and `/otlp/`, the routes its own boxes dial — a `direct` run outlives its node's restart, and calls back while the node starts. |
| A request on the artifact origin (`api.artifacts`) | An empty `503` with `Retry-After: 10` for a ticketed path, the runtime or the kit, and the empty `404` or `405` the origin gives anything else | The origin's own refusal under its own inert headers, never a JSON body, which would be a document an artifact page could read. |
| Anything else | The `404` that listener answers once the node serves | So a socket says no more while the node starts than it does afterwards, and nobody is sent back to wait for a route this node will never serve. On `api.public` that is the router's own `404 no_route`, the probes included; on `api.port` of an `ingress` node with `api.public` set, the `404` naming `api.public` for a route outside parties call; and on `api.port` of a node without `ingress`, the router's own `404` for every path but the two above and, from its own host's loopback on a node that runs seats, its boxes' two routes (once it serves, its guard answers `401` for a path that asks for a credential, and a `404` says no more than that). |

Nothing here asks for a credential, since nothing is served that one could
unlock. Every answer carries the security headers every answer of the node's
carries, and on an `ingress` node the CORS permission its own answers carry, so
a dashboard on an origin `api.auth.allowed_origins` lists reads `starting`
rather than a network error; a path with a dot segment is
refused `400 non_canonical_path` as always.

`/health` names how far the boot has got:

```json
{
  "status": "starting",
  "node": "core-1",
  "version": "v0.4.0",
  "phase": "recovering"
}
```

| `phase` | `/health` | What the node is doing |
|---|---|---|
| `recovering` | `503` | Opening and recovering what it holds itself: its own store and that release's schema migrations, the embedded broker's file store and listeners, JetStream catching up with its peers — or, on a node without `data`, its leaf link to a member — the streams and buckets it provisions, the batches of a stateless node's events its own event log left unsettled, the replicated estate and its state logs, and a joining node's snapshot. It takes as long as the store is large — and, on a data node of a fleet whose rows the logs can no longer bring level, as long as fetching a peer's snapshot of the whole replicated estate takes, which `stream.tracker_retention.rejoin_window` budgets and `crewlet retention status` projects; a lone data node never fetches one. The deployment bounds it with a startup probe: see [Sizing the startup probe](../guides/deployment.md#sizing-the-startup-probe). A join logs `statelog_join_progress` every ten seconds while it transfers. It ends with `engine_recovered` in the log. Applying the state logs is not part of it: that catches up in the background from then on, and what waits for it is the node's first seat, never `/health`. |
| `preparing` | `200` | Bringing up what its company configures: the snapshot of the company's secrets its `${VAR}` references resolve from, the company built from its document, its shared MCP servers, its sandboxes, Worklet, Doclet and Chatlet, its harnesses' credential bundles, the first apply of the revision it boots on, and the dashboard's projection. None of it holds a probe. A part that waits on something else is bounded by its own setting (a tool server by its `startup_timeout_seconds`), but the first apply has no deadline beyond its parts', so a part that never returns leaves the node here, with `/ready` naming `starting`, and nothing restarts it: watch for a node that stays unready (see [Sizing the startup probe](../guides/deployment.md#sizing-the-startup-probe)). So does a secret store that does not answer: the node builds no company from the environment alone, and asks again, logging `secret_store_unreadable`, until the store answers. A value its keyring cannot open ends the boot instead, naming the secret, since no wait changes that answer. |

Every field of that body is a field of [the health envelope](#the-health-envelope)
under the same name, so a script reading `/health` reads one shape whichever
answered it. What it leaves out is every fact about a running engine — its
queue, its seats, its posture, its applied epoch, `started_at` — because a
starting node has none of them yet, and a zero standing in for one would be a
fiction. The envelope carries no `phase`: the node that answers it is serving,
so its boot is over.

`/ready` says the same thing in its own shape:

```json
{
  "ready": false,
  "node": "core-1",
  "configured": false,
  "draining": false,
  "posture": "",
  "reason": "starting"
}
```

`configured` is `false` because it is true: no revision is active in this
process yet, whatever its store holds, and `posture` is empty because nothing
has concluded one. A route the node will serve is refused in the engine's
envelope:

```json
{
  "error": "starting",
  "message": "This node is still starting and is not serving yet. Try again in a moment.",
  "detail": "This node is recovering what it holds (its store, its broker and its copy of the estate) and serves nothing until it has started.",
  "hint": "Retry after the Retry-After: once the node serves, /health stops answering status \"starting\" and /ready stops naming reason \"starting\"."
}
```

A node told to stop before it serves (a signal during the boot) answers a
drain's answers from then on, as far as a starting node can: `/health` says
`status: "shutting_down"` but keeps the status code of its phase — `503` while
it still recovers, since a stop does not cut a recovery short and the startup
probe is still what bounds it, and `200` after — `/ready` answers
`reason: "draining"` and `draining: true`, and every route it would have
served `503 draining` with the drain's `Retry-After` of 30 seconds.

---

## During a drain

A node that has been told to stop (SIGTERM, or `Ctrl+C` once) keeps serving HTTP for the whole of its [drain](../concepts/agent-runtime.md#graceful-shutdown), and closes its listener only once the drain has completed. What changes from the drain's first moment is which requests it will still take:

| Request | During a drain | Why |
|---|---|---|
| `GET /health` | `200`, `status: "shutting_down"` | Liveness. An orchestrator that could not reach the node would kill it in the middle of the turns the drain exists to finish. |
| `GET /ready` | `503`, `reason: "draining"` | Readiness: takes the node out of rotation so traffic moves to a peer. |
| Every other read (`GET`, `HEAD`, `OPTIONS`): the dashboard, the REST reads, `/query/*`, `/ws/stream` | Served | A read starts nothing, and it is how the drain is watched. |
| `/mcp/{token}` (its notes route included) and `/otlp/{token}/v1/{signal}` | Served | They carry the tool calls, the notes and the spans of executors launched before the drain. An [executor in its box](../concepts/code-sandbox.md) outlives the turn segment that launched it, so the drain never waits on one, and refusing these would shorten no drain and only break an executor mid-flight. A tool call whose executor's seat is held by another node is relayed there, as it is outside a drain. |
| Every `/webhooks/*` route, whatever its method | `503` | A delivery is new work, and one of the two `GET` landings acts: the GitHub App return seals a credential and writes a config revision, and an install arrival asks the reconcile loop for a pass. The Slack OAuth landing only renders a page and is refused with the rest, because a per-route carve-out is what refusing by default avoids. |
| Every other write: `/config`, `/secrets`, `/setup`, `/backup`, the `/worklet/*` writes, `POST /operator/mcp`, `POST /operator/act/{tool}` | `503` | Each one starts work or changes the company the drain is leaving. Refusing by default is what keeps a write route added later from slipping through a drain. `POST /secrets/reveal` rides with them although it changes nothing — it is a `POST` so the names it reads travel in a body — and the CLI run asking it is told to try another node, as a write is. |

`/operator/mcp` is the one route the by-method rule splits, because it is mounted for every verb: its `POST` — every JSON-RPC call, reads included — is refused. It is served [statelessly](#every-call-is-decided-by-the-request-that-carries-it), so it offers no server-to-client stream and holds no session: its `GET` is served like any other read and answers the MCP transport's own `405 Allow: POST`, and its `DELETE` rides the default with the writes. `/mcp/{token}` is not split, because the whole prefix is served: an executor's tool calls are the one thing on this listener the node must not break.

A refusal is `503` with a `Retry-After` of 30 seconds, long enough for a load balancer following `/ready` to have moved traffic to a peer, and a body the CLI and the dashboard both render:

```json
{
  "error": "draining",
  "message": "This node is shutting down and is not taking new work. Try another node, or this one once it has restarted.",
  "detail": "This node is draining for a shutdown: the turns already running finish, and nothing new is started here.",
  "hint": "Retry against another node, or once this one has restarted; /ready answers 503 for as long as the drain lasts."
}
```

A write still needs its token first: an unauthenticated write answers `401` whether or not the node is draining. And a request that was already running when the drain began is not interrupted by it; it is cut only if it is still running five seconds after the listener starts to close.

A node with a [public listener](#which-listener-serves-what) drains both by this one table — its webhooks are refused on `api.public` and its sandbox endpoints served there — and closes `api.public` first, then `api.port`, each with its own five seconds, so `/health` answers until the very end even while an open bridge session holds the public listener for its whole grace.

---

## A request nothing serves, and an answer with no code

The [envelope](#every-refusal-is-one-envelope) covers a request nothing on the
node serves as well as one a route refuses: `404 no_route` for a path (one
this node's build does not have, or one its configuration leaves unmounted,
such as `POST /worklet/items/{key}/purge` on a company that does not run Worklet)
and `405 method_not_allowed`, with an `Allow` header, for a served path under
another method. Both carry a `detail` naming the method and the path, where the
router's own answer was a line of plain text no client could branch on.

The CLI and the dashboard rely on it. A non-2xx answer with no code was written
by something **in front of** the node (most often a reverse proxy's read
timeout) and says nothing about what the node did, so a write that meets one
is reported as *unknown*, with the operation id to finish it under, rather than
as refused. A refusal from the node means nothing was done.

**Every answer the node's API writes carries `X-Crewlet-Engine: 1`**: a route's
answer, every refusal, the router's own `404` and `405`, the canonical-path
refusal and the `503 starting` of a node still starting. A body alone cannot
say whose answer it is, since a proxy can answer JSON with an `error` field of
its own; this header is set by nothing in front of the node. The dashboard and
the CLI read an answer as the node's only when it carries both the header and a
code, so a proxy's `401`, `403` or `413` is an answer that says nothing about
what the node did, never a refused token, a grant to ask for or a smaller
document. A proxy in front of the node must pass the header through unchanged;
one that strips it makes every answer read as one the node never gave, so a
write's outcome is reported unknown and never as refused. A dashboard served
from another origin (`api.auth.allowed_origins`) reads it because the node
exposes it to permitted origins (`Access-Control-Expose-Headers`, beside the
`ETag`, `Retry-After` and `X-Request-Id` it reads too); a proxy that rewrites
that header must keep `X-Crewlet-Engine` in it.

---

## Routes

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness + the engine-health envelope (see [below](#the-health-envelope)). Stays `200` through a drain (see [During a drain](#during-a-drain)); use `/ready` to steer traffic. Before the node serves it answers a shorter body, `503` while the node recovers what it holds and `200` after (see [While a node is starting](#while-a-node-is-starting)) |
| `GET` | `/ready` | Readiness for a load balancer: `503` with `reason: "starting"` before the node serves (see [While a node is starting](#while-a-node-is-starting)), then `503` while draining, before the first config revision applies, on a `shed` or `stuck` posture, or while this node can no longer route the estate's reads and writes, and `200` otherwise. A `503` names why in `reason`: `draining`, `unconfigured`, `shed` or `stuck`, then `estate_behind`, in that order of precedence. `estate_behind` is the verdict that also gives a node's seats back ([`unserviceable`](../concepts/seat-ownership.md#a-copy-that-is-behind-and-a-copy-that-is-wrong)): its calls need another data node — its own copy is out of service — and its view of who serves the estate has been unreadable for longer than 60 seconds. A copy that is merely behind never takes a node out of rotation. The term that decided it is logged at debug (`ready_withheld_for_the_estate`), never put in the body, so a load balancer records one code for one outage. It never reads the fleet's presence or alarm counts, which decide nothing here. A node without the `ingress` role answers a different question on the same route — whether it is doing its work — see [Probes on a node without ingress](#probes-on-a-node-without-ingress) |
| `GET` | `/agents` | List agent roles, each merged with live state from the in-memory projection (including the in-flight `live_call`). [Human seats](../concepts/humans-in-the-org.md) are excluded — they appear only in `/org` with `"kind": "human"` |
| `GET` | `/agents/{id}` | Single agent — its `handle`, `agent_id` and `role` (its name), the live overlay (incl. `live_call`), and `llm_history`: the seat's finished phases newest first, capped at 50. `{id}` is the seat's **handle**, which is what every roster row carries as its `id`. A role name is NOT accepted: two seats may share one, and a name-addressed answer read one seat's live call and transcript as the other's. A handle no agent seat answers to is `404` |
| `GET` | `/agents/{id}/memory` | Durable memories (personal, episodic, counterparty, synthesized skills). Same `{id}` — the seat's handle, which the rest is keyed by, resolves to the derived agent id the diary is keyed by |
| `GET` | `/agents/{id}/memory/episodes/{episode}` | One episode **whole** — its ask and its account complete, which the memory listing carries the openings of (see [below](#get-agentsidmemoryepisodesepisode)) |
| `GET` | `/org` | The company's charter and its seat and unit tree, in an explicit public shape that carries no contact identity, email, credential or deployment setting (see [below](#get-org)). Human seats appear with `"kind": "human"` |
| `GET` | `/tools` | Registered tools, each tagged with the `source` that registered it — `builtin` or `mcp:<server>` (see [Where a tool comes from](../guides/tools-and-mcp.md#where-a-tool-comes-from)) — plus its behavioural `annotations`, where it `delivers`, and its `input_schema` (see [below](#the-tool-catalogue)) |
| `GET` | `/events` | Recent engine events from the event store (`limit` caps at 400; keyset-paged, see below) |
| `GET` | `/events/{event_id}` | Single event incl. payload — inside the 30-day history (`event_history_seconds`) like every other read of the log, so a link to an older event answers `not_found`: every data node floors its lookup at the serving node's horizon, whatever the retention sweep of the node holding a copy has or has not reached (see [coverage](#reading-the-fleets-history-coverage)) |
| `GET` | `/events/trace/{trace_id}` | All events in one trace, oldest first, capped at 500 |
| `GET` | `/tokens/breakdown` | The token-spend rollup by phase / model / provider entry / worker / seat — the live 24 hours, or any window of up to 90 company days from the replicated usage domain (see [below](#token-spend-breakdown)) |
| `GET` | `/tokens/series` | The same spend **with a time axis** — one bucket per company day or ISO week, split into bands (see [below](#get-tokensseries)) |
| `GET` | `/agents/activity` | Every seat's turns over a window of company days — counts, the first-pass rate over reviewed turns, turn-duration quantiles and a day-by-day series (see [`seat_activity`](#queries)) |
| `GET` | `/schedules` | Configured role/unit schedules + next-run + recent dispatch ledger |
| `GET` | `/fleet` | Every live node, its roles and labels, seat ownership, singleton duties, the fleet's activation and each node's config record — the revision it last attempted, how that went and the revision it runs — and where the company's files are kept with what the object store's collector last found. **Always needs a credential carrying `admin`** — a signed-in session or a bearer, like every route here — because it describes the deployment rather than the company, and the dashboard locks the screen that draws it (see [below](#get-fleet)) |
| `GET` | `/executors` | Every seat's [executor](../concepts/code-sandbox.md) the engine still holds a record of, read from the executors' durable record in the [coordination store](../concepts/coordination.md) (see [below](#get-executors)) |
| `GET` | `/budgets` | Token caps, the durable shared counter they are enforced against — per calendar window — and which scopes are being refused (see [below](#get-budgets)) |
| `POST` | `/backup` | Copy this node's store and stream estate into `?dir=` **on the engine's host**. **Takes `admin`** — it writes every credential the company holds to a path the caller names (see [below](#post-backup)) |
| `GET` | `/fleet/broker` | The fleet broker's membership: every live node's broker kind as its presence advertises it, the JetStream metadata group as a member reports it, and every disagreement between the two — a member gone for good first among them. **Takes `admin`** (see [The broker's membership](#the-brokers-membership)) |
| `POST` | `/fleet/broker/remove/{node}` | Remove a member from the metadata group through a live member's system account. `?confirm=` repeats the node id; refused while the node holds a live presence lease as a member unless `?force=true`. **Takes `admin`** and a [recent proof](#some-gestures-ask-how-recently-you-proved-who-you-are) |
| `POST` | `/fleet/broker/remove-peer/{peer}` | The same removal, naming the voter by the raft peer id `GET /fleet/broker` shows — for a voter whose name no member has heard. `?confirm=` repeats the peer id. **Takes `admin`** and a recent proof, as the removal by name does |
| `GET` | `/integrations` | Every inbound surface, how it is wired, whether a signing secret is present, and what has arrived through it (see [below](#get-integrations)) |
| `GET` | `/link-hosts` | Where the company's own apps live in a browser: one `{kind, url}` per configured GitHub, GitLab, Mattermost, Jira and Confluence, and on a code host its `owners`. **Everybody's**, unlike `/integrations`: it carries no repository, project, team, credential or health, and each `url` is reduced to scheme, host, port and path (user information, query and fragment dropped). `owners` are the organizations or top-level groups the company works in (GitHub's `provisioning.org`, GitLab's `provisioning.group`, and the owner part of every repository or project the integration and the seats name), lower case and sorted, never the repositories themselves. The dashboard reads it to put an app's mark beside a link to that app, to link a bare `owner/repo#14`, and to read a bare `owner/repo` as a repository only under one of the owners. A Jira or Confluence on a cloud id with no `site_url` has no browser address, so it adds no row |
| `GET` | `/backups` | What the fleet has backed up: each node's newest point and the operator's newest acknowledgement of each log, as each log's trim reads them, and every backup a person asked a node for, failures included. **Takes `admin`** (see [below](#get-backups)) |
| `GET` | `/mcp-servers` | What each configured MCP server is doing on each live node — serving, failed, tools served and the first failure — read off every node's presence heartbeat, beside what the configuration declares (never a credential), and the engine's built-in tools by group, each naming who may call it. **Takes `admin`** (see [below](#get-mcp-servers)) |
| `GET` | `/mcp-servers/catalogue` | What an MCP server is added from: the curated catalogue whole, and with `q=` the company's community registry (`mcp_registry.url`) searched for it, each listing marked as not reviewed. **Takes `admin`** (see [below](#get-mcp-serverscatalogue)) |
| `GET` | `/worklet` | Worklet: a filtered listing of work items, plus the last key number minted per project. Served only where `tracker.backend` is `worklet`; a company on Jira gets `404 unknown_query`, not an empty board (see [below](#worklet-and-doclet)) |
| `GET` | `/worklet/retention` | What the state log is holding, what the trim concluded and which term is stopping it, every data node's position, and what this node costs to replace. **Takes `admin`, reads included** (see [below](#get-workletretention-what-the-log-is-holding)) |
| `POST` | `/worklet/retention/ack` | Publish an operator backup floor, for `backup_floor: operator` |
| `POST` | `/worklet/retention/evict/{node}` | Install the eviction gate on a node on every log the trim counts nodes on (Worklet's, Doclet's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, Chatlet's and the executor log's), so the trim can pass a floor it is pinning. Refused `409` while the node holds a live presence lease; answers per log (see [below](#the-three-retention-gestures-that-write)) |
| `POST` | `/worklet/retention/readmit/{node}` | Lift it on every one of those logs: the inverse commit rather than a delete. Refused `409` while the node still lacks records a trim floor lets the log delete |
| `POST` | `/worklet/retention/capacity` | Drive a log's byte-ceiling change as far as this node's mode allows |
| `GET` | `/worklet/retention/maintenance` | Where that window stands and what is holding it |
| `POST` | `/worklet/retention/maintenance/abandon` | Change what the operation is trying to reach, never the barrier it must cross |
| `POST` | `/worklet/retention/maintenance/exclude` | Record that a participant's process is stopped and holds no outstanding request |
| `GET` | `/worklet/retention/reanchor` | The live stream's own `created_at`, which a reanchor's confirmation has to echo, and the case a reanchor would answer |
| `POST` | `/worklet/retention/reanchor` | Adopt a recreated stream, or a broker restored from an older copy, at the next generation |
| `GET` | `/worklet/views` | One container's **view strip**: the six every container has without anybody saving one, and whatever was saved beyond them. `?container=` takes the query grammar's own spelling (`workspace`, `project:ENG`, `unit:engineering`, `person:ana`): a project key is upper-cased and a unit is resolved through the chart by any key it answers to or by its name, so a team's strip is one strip under every one of its spellings. The strip is ordered by **the caller's own** personal views and pins, and there is no parameter naming whose: a strip is furniture a person arranges for themselves, and a parameter that could name somebody else's was a way to read their arrangement. The caller's own is their [own record](#whose-record-a-personal-question-answers-for): the seat a person holds, or the login of a Tier A token or service account bound to none |
| `GET` | `/worklet/views/saved` | **Every saved view** the caller can see, in EVERY container: the shared ones and their own personal ones, pinned first, each row carrying the `container` it lives in. What the dashboard's view inventory and its sidebar's pinned group read, because a view saved on a project board is in no workspace strip. There is no parameter naming whose, for the reason `/worklet/views` has none. `?counts=true` counts each pinned view in its own container, and is refused `400 bad_params` to a credential that names no record of its own, since only a record has pins |
| `GET` | `/worklet/catalogue` | The company's **vocabulary**: the task types a create may name and the workspace's custom-field declarations. `?archived=true` also lists what was retired. The types are the EFFECTIVE set: the six this build ships plus whatever the company declared, a declaration replacing a builtin of the same slug |
| `GET` | `/worklet/projects` | Every **project** work is filed into, with its `task_counts` (`{todo, active, done, closed}`, one per status group, read from maintained columns and never aggregated per poll; there is no `open`, which folded the waiting work into the started work, so a caller wanting every unfinished item adds `todo` and `active`), its `target_date` (the day, `YYYY-MM-DD` on the company's clock, the lead means it to be finished; omitted when none is set), its `last_change` (when the project's work last changed and who changed it, ABSENT for a project nothing has been filed into), its chart-owned unit and its lead. `?q=` narrows by a word in the key, the name or the purpose and `?unit=` to the projects one unit owns, **named by the unit's key or by its name where exactly one unit carries it, in any case**, and matched against the team's key and its name. `?archived=` SELECTS a set rather than widening one: `false` (the default) for the live projects, `only` for the retired ones alone and `true` for both, so "what did we retire" is a query rather than a caller's own filter over a wider answer. `?sort=` orders the whole selected set before the page is taken: one of `key`, `name`, `unit`, `todo`, `active`, `done`, `closed`, `last_change`, `target`, each optionally with a leading `-` for descending, defaulting to `key`, with the key breaking every tie; a project with no `last_change` or no `target_date` sorts last in both directions. An `archived` or `sort` value that is neither is a **400** naming the parameter and what it accepts. `?limit=` caps at 200, which is also the default. The answer carries a `census` (`{active, archived}`, the same question under the same `q` and `unit` MINUS its archival term), so a caller that selected one set can still tell an empty set from an empty company; `total` is the census of the mode that was asked for. A set read, so it carries `complete` and its `incomplete` beside the read level |
| `GET` | `/worklet/projects/{key}` | One project in **full**: the six statuses with their labels, groups and descriptions; the effective types; the custom fields grouped by which type they apply to, required first, with the workspace ids this project **shadows** named; its tags; its default assignee, lead and owning unit. `?for_type=` narrows the fields to one type plus the ones that apply to every type. Unknown key answers 404 naming the nearest three; a `for_type` the company does not file answers 400 `bad_params`, not 404: the project is there and the argument is what to change |
| `GET` | `/worklet/files` | One project's **files**, in path order: `?project=` (required), `?folder=` to narrow to the paths under one folder, `?removed=true` to include removed files, `?limit=` (default 200, max 1000) and `?after=`, the `next` the previous page answered. An unknown project is `404 no_project`, never an empty page (see [below](#project-files)) |
| `GET` | `/worklet/files/{project}/{path...}` | One file's **bytes**, streamed, with its version as the `ETag`. `503 content_unavailable` where the file is listed and its content could not be read right now (see [below](#project-files)) |
| `PUT` | `/worklet/files/{project}/{path...}` | Write a file: the request body is its content, `Content-Type` its type, `If-Match` the version it replaces and an `Idempotency-Key` how an `unknown` answer is retried. **Takes `worklet`**, and the write is attributed to whoever the request resolved to (see [below](#project-files)) |
| `DELETE` | `/worklet/files/{project}/{path...}` | Remove a file. `If-Match` and `Idempotency-Key` as the write. **Its project's lead, or `admin`**, either with `worklet`: taking a file out of a project is the project's, as removing a work item is |
| `GET` | `/worklet/activity` | The **activity feed**: one durable row per applied commit, quiet ones included, at any age with no live/archive boundary to cross. Ordered by the COMPOSED LOG POSITION rather than by any clock, so `?since=` and `?cursor=` are both positions written `<stream>@<generation>:<sequence>`, which is what lets a cursor span a reanchor with no gap and no repeat. `?task=` (by key, id or a FORMER key), `?container=`, `?kinds=`, `?actor=`, `?assignee=`, `?notified=`, `?from=`/`?to=` (RFC3339, bounding the AUTHORED instants), `?limit=` ≤200. `?q=` is an escaped `LIKE` over the excerpt and is REFUSED unless it names a task, or a project **and** a `since` inside 90 days. Each record carries `fields`, what MOVED, as `{"<field>": {"from": …, "to": …}}`, and for a SET (`tags`, `watchers`, `muted`, `collaborators`, each link kind, `blocking`, a catalogue's or tag set's declarations, a person's favorites) the members that joined and left, as `{"<field>": {"from": "", "to": "", "added": […], "removed": […]}}`, each sorted and every member whole. That holds for every kind and not only the ones about a task: a project reconcile names the name, purpose, unit or epoch that changed, a view save the query parameters, a priorities write the order before and after, and a dependency the item it now waits on. A task's own row draws on twenty-eight names: `title`, `status`, `assignee`, `priority`, `project`, `type`, `tags`, `due`, `due_all_day`, `start`, `estimate`, `points`, `reporter`, `watchers`, `muted`, `collaborators`, `parent`, `routing_unit`, `archived`, `removed_with`, `waiting_on`, `linked`, `duplicates`, `page`, `blocking`, `checklists`, `fields` and `body`. Values are the STORED form (a status slug, a whole RFC3339 instant, an item's id) rather than a rendering, because every data node writes the row identically and a rendering would depend on the reader's zone and the company's live vocabulary. Nothing is cut: an ordered list (`priorities`, `pinned_views`, `checklists`) is carried whole, free text past 600 bytes (a project's `purpose`, say) is described as `<N> bytes`, and every field a change moved is on the row; only a notification card stops at 32, saying how many it left off (`fields_omitted`). The two largest are MARKED rather than carried: `body` is `<N> bytes` on each side (empty where there was none) and never the prose, and `checklists` is `<list>: <done> of <total> done` per named list. `fields` names each custom value by its SLUG (resolved against the project's catalogue by the node applying the change, which is why a NOTIFICATION carries every other delta and not this one), with a choice as its option's slug, a multi-valued field's members joined with `/`, a value past 150 bytes as `<N> bytes`, and a count of any whose field the project no longer declares. The ANSWER also carries `keys`, an id-to-item-key map naming the tasks those deltas point at (`waiting_on`, `linked` and `duplicates` but never `page`, which names a knowledge-base page; the `blocking` mirror; a person's `priorities` queue; and the two scalars that name a task, `parent` and `removed_with`), resolved on the answering node: a delta records another task by its ID, because a key belongs to that task's own row and a history row is written once and never repaired. An id this node holds no row for is absent rather than empty, and a renderer falls back to the id |
| `GET` | `/worklet/my-work` | Everything one person is expected to look at, in seven bounded lists: `priorities` in the stored order, `assigned`, `asked_of_me` (each with the literal call that answers it, whether it is `open`, and the `decision` it carries when it asks somebody to choose; see [Asking for a decision](../guides/worklet.md#asking-for-a-decision)), `checklist_items` (which live on other people's tasks and no assignee filter reaches), `collaborating`, `watching_recent` and `unblocked_recent`. `?handle=` is whose, and it **defaults to the caller's own record** (see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for)). Naming somebody else's takes whoever leads them, or `admin`, and `worklet` either way |
| `GET` | `/worklet/inbox` | One person's **inbox**: the notices a change wrote to them, each naming the ONE [reason](../guides/worklet.md) of eighteen it found them under, whether it **asks** something or merely informs, whether it arrived only because nobody better was found, and their own read and snooze marks, plus the comment and turn the change came from, and the `ask` it is about, read as it stands now. Same scope rule as `/worklet/my-work`. `?unread=`, `?primary_only=`, `?snoozed=` (`exclude`, the default, since a snooze means *not now*; `include` or `only`; anything else refused naming the three), `?reasons=` (comma-separated, refused naming the eighteen); every one of them narrows the SCAN, so a page is full whenever the scope holds 50 notices and `next_cursor` is never a cursor past an empty page, `?limit=` ≤50, `?cursor=`, and `?since=`, a LOG POSITION written `<stream>@<generation>:<sequence>`, which is what `seen_through` reports back, never a bare sequence: the comparison is on the packed `(generation << 40) | seq`, so a sequence with no generation re-delivers everything after a reanchor |
| `GET` | `/worklet/people/{handle}` | One human's **own state**: their inbox (unread, read, snoozed, and which snoozes are now **due**), the order they mean to work in and who set it, and their pinned views. Same scope rule as `/worklet/my-work`, with the handle always named here because it is the path: your own seat's is yours, and anybody else's takes the same owner-or-lead rule. A person nobody has written yet answers the EMPTY state with `held: false`, not a 404: every human starts this way and the first write is what creates the record |
| `GET` | `/worklet/{id}` | One item with its description, thread, history and links. `{id}` is either the key (`ENG-42`) or the id: a person holds the first and every internal link the second |
| `GET` | `/worklet/comments` | One page of an item's thread, walking back from the newest: `?item=` (key or id), `?cursor=`, `?limit=` (default 20, at most 50), `?thread=` (one comment's conversation) and `?with_root=`; see [`work_comments`](#queries) |
| `GET` | `/worklet/turns` | One page of the agent turns charged to an item, newest first: `?id=` (key or id), `?cursor=`, `?limit=` (default 20, at most 50); see [`work_item_turns`](#queries) |
| `GET` | `/doclet/pages` | Doclet: a filtered listing of its pages. Served only where `knowledge.backend` is `doclet` |
| `GET` | `/doclet/pages/{id}` | One page with its body, comments, revision metadata, children and ancestor breadcrumb. `{id}` is the id, or `CONTAINER/Title`: the title matches the way the fleet CLAIMED it, so case and runs of whitespace are ignored and `ENG/deploy runbook` reaches a page called "Deploy  Runbook" |
| `GET` | `/doclet/pages/{id}/revisions/{version}` | One revision of a page: its own body, message and author at that version, the newest included. Decided as reading the page; see [`page_revision`](#queries) |
| `GET` | `/containers` | Every knowledge container this node knows about, with how many pages each holds. The engine materialises one per `space:` the org chart names, plus the two reserved ones, on every config apply and every boot; each carries `chart_epoch`, the activation its name and purpose were last written from (Unix milliseconds), so a configuration activated earlier never overwrites them |
| `GET` | `/chatlet/channels` | The company's own **chat**: the caller's rooms, newest activity first, each with its unread count against the caller's own read cursor and whether they muted it, plus their whole read state: every room (at most 256), or with `?limit=` the most recently active that many, and with `?unread=true` only those with something unread. Served only where `chat.backend` is `chatlet`, to a principal holding `chatlet` **and acting as a seat** — see [Chatlet](#chatlet) |
| `GET` | `/chatlet/channels/{channel_id}` | One room's metadata and membership. On this route and the two below, `{channel_id}` is the room's id or its name (`launch`, or `%23launch` for `#launch`), so a room the caller is not in is reached by the name everybody calls it. A room the caller may not read answers `404`, by name as by id, exactly as one that does not exist |
| `GET` | `/chatlet/channels/{channel_id}/messages` | One page of a room's transcript, newest first: `?cursor=` (the last answer's `next_cursor`), `?limit=` (default 50, at most 500) |
| `GET` | `/chatlet/channels/{channel_id}/threads/{root_id}` | One thread from its root, oldest first, with who has spoken in it; paged as the transcript, the other way |
| `GET` | `/chatlet/activity` | What waits for the caller across the rooms they may read, newest first: mentions of them (and of a whole room they are in), replies in the threads they follow, and reactions to their messages. Each row says whether it is unread against the caller's own read state and carries the position that marks it read; the answer counts the unread mentions and replies over the newest 100 of each. `?kind=` (`mention`, `reply` or `reaction`) narrows it, `?unread=true` answers the unread rows alone, whole, `?cursor=` and `?limit=` page it. See [what a reader keeps](../concepts/chatlet.md#what-a-reader-keeps) |
| `GET` | `/chatlet/threads` | The threads the caller follows, newest reply first, each with its root (carrying the thread line) and its two newest replies: the threads they started, replied or were named in, and those they followed by hand, less those they unfollowed. `?cursor=`, `?limit=` (default 20, at most 50) |
| `GET` | `/chatlet/saved` | The messages the caller saved for later, the newest save first, each with its message, or `gone` where it was taken back, erased, pruned or is in a room they may no longer read. `?cursor=`, `?limit=` |
| `GET` | `/chatlet/sent` | What the caller said, newest first, in every room they may still read; a message they took back is left out. `?cursor=`, `?limit=` |
| `GET` | `/chatlet/channels/{channel_id}/links` | Every link the room's messages carried, newest first, each with who shared it, when, and the message it came from; a taken-back message's are left out. `?cursor=`, `?limit=` (messages a page reads) |
| `GET` | `/chatlet/channels/{channel_id}/messages/{message_id}` | One message, read in place: with its thread's root and the two replies just before it when it is a reply. A message not in that room answers `404`, as a room the caller may not read does |
| `GET` | `/chatlet/search` | The rooms the caller may read **ranked** against `?q=` — keyword only, over the engine's own chat index: `?channel_id=` and `?author=` narrow it, `?limit=` (default 20, at most 40) |
| `GET` | `/chatlet/room-names` | The name of every room the caller may read, joined or not, the archived ones included: each room's `id`, `name`, `kind` and `archived`, the live rooms first, at most 1000 with `truncated` where rooms were left out. A direct conversation has no name and is not here |
| `GET` | `/chatlet/rooms` | Every named room the caller may read, joined or not, the archived ones included, each with its member count, whether the caller is in it and when it was last active: the `chat_rooms` question |
| `POST` | `/worklet/items` `/doclet/pages` | **File an item, write a page**, and the rest of the [write surface](#the-human-write-surface): the same tools a seat and your own assistant hold, as the person you signed in as. Guarded; the item routes are absent on a company that does not run Worklet, and the page routes on one that does not run Doclet |
| `PATCH` | `/worklet/items/{key}` | Change an item, and its `/comments`, `/rank`, `/depend`, `/relate`, `/restore` and `/purge` beside it. See [below](#the-human-write-surface) for every route and the authority each takes |
| `GET` | `/artifacts/{id}/data/{collection}` `/{collection}/{doc}` `/{collection}/query` `/history` `/schema` `/state` | **An artifact's datastore, read**: a collection's documents, one document, a query over its field index, the store's history, its schema and the caller's own per-viewer state, each decided per artifact and shaped by the collection's read rule (see [below](#artifactsiddata-an-artifacts-datastore-auth-gated)) |
| `POST` `PUT` | `/artifacts/{id}/data/writes` `/undo` `/state` `/schema` | **An artifact's datastore, written**: one atomic batch of ops, an undo, a per-viewer state key, a schema change. `Idempotency-Key` required; each answers the verdict the applier wrote |
| `GET` | `/artifacts/{id}/data/export.csv` | One collection as CSV, to an editor of the artifact |
| `POST` | `/auth/login` | **Sign in.** Login or address, password, and a second factor where one is held. **Unguarded**, and throttled on the login **as typed** from the caller's source: a failure costs the next attempt a wait, never a lockout — see [below](#a-failure-costs-a-wait-never-a-lockout). Every failure answers one code after the same argon2id derivation — see [below](#every-failed-sign-in-is-one-refusal). A node that cannot read the identity estate or record the session answers `503` — and **every `503` under `/auth` carries a `Retry-After`**, so a client can tell "ask again in a moment" from a node that is gone. A write whose outcome nobody can establish — the second factor's spend, the session's own start — is that `503` with the `op_id`, and **never a session**: a code whose spend may not have landed is a code that still works. A success answers `{person, login, seat?, expires_at, position, status}`, and `status` is `signed_in` — or, where `api.auth.totp` is `required` (its default) and the person holds no second factor, `second_factor_enrolment_required`: the session it opened may only enrol one (see [A required second factor is enrolled before anything else](../concepts/identity-and-access.md#a-required-second-factor-is-enrolled-before-anything-else)). `POST /auth/invite/{id}` and a password `POST /auth/step-up` answer the same shape, restricted on the same rule |
| `GET` | `/auth/config` | What a sign-in page needs to know before anybody has signed in: `{min_password_length, second_factor}` — the deployment's own `api.auth.min_password_length`, never below the engine's twelve, which is exactly the floor every password a person sets is held to, and `required` or `optional` from `api.auth.totp` (unset is `required`). Password sign-in is always served, so there is no backend to report. **Unguarded**, and it carries **no user list and no count of people** |
| `GET` | `/auth/invite/{id}` | **Renders an invitation and never spends it** — a link is followed by mail clients prefetching, scanners and preview cards, and one spent by a GET is an account created for somebody who never saw it. **Unguarded**: holding the link is the credential — the **secret** it carries after the id, presented in the `X-Crewlet-Invite-Secret` header and never in the URL. The link a person follows is the dashboard's screen, `<api.external_url>/dashboard#/invite/<id>.<secret>`, whose fragment no browser sends to a server; the screen calls this route with the two halves apart. Answers the address it is for, who sent it (`invited_by` — a person by their seat's name as the chart gives it, since a person writes as their seat's handle, and absent for a machine — a Tier A token or a service account — whose coloned login, `token:founder`, is nothing the person holding the link would recognise), the password floor, the `seat` redeeming it binds (**always present**, because every invitation names the human seat its invitee will hold: `{handle, name}` as the chart calls it now, or `{}` where the company this node runs no longer holds it as a human seat, which the redemption refuses as a link that no longer works), a `login` **proposed** from the address in the person grammar (`jane.doe@example.com` → `jane.doe`, `jane@example.com` → `jane.example`) for the form to pre-fill, and `signed_in_as` — the login of the session the browser asking is already signed in with, absent for one signed in as nobody — because redeeming ends that session, and the form says so first. Absent, redeemed, expired and a missing or wrong secret are one `410` — the same bytes, so a guessed secret against a leaked id does not say the id exists — and an id nobody issued or a secret that is not the link's is a **failed attempt**, counted on the audit trail's per-minute failure row (`iam_login_failures`) under the source it came from: walking ids or secrets is guessing at a link, and that source's count climbing is what shows it. It meets no [curve](#a-failure-costs-a-wait-never-a-lockout) — the secret is 256 bits nobody walks, and a curve keyed on the address a link came from is one a stranger there holds shut for everybody else. A link that **proved itself** and is spent — redeemed, expired, its address enrolled — is the same `410` and is not counted: that is the link's holder, or a mail scanner re-reading it, and a guesser who does not hold the link can never reach the difference. A node whose identity rows cannot **vouch** for a link that does not open — behind the identity log, or holding a record it could not apply — answers `503 identity_unavailable` instead, for an id it does not hold and a wrong secret alike, and counts nothing: the link may have been issued through a peer |
| `POST` | `/auth/invite/{id}` | Redeems it, conferring exactly the grants and seat whoever issued it decided — the enrolment names the invitation as its authority and presents the link's secret, and the record refuses anything the invitation does not cover, so a link spent or aged out between the form and the post is `410 invite_spent`. **Unguarded**. `{secret, login, name, password}`: `secret` is the half of the link after the id, checked exactly as the view checks it; the login is **required** — every person enrols with one: it is what they sign in with, the name a lead finds them by, and the `by jane.doe` in every trail reason about what they do; the view proposes one. An absent login, or one outside a person's grammar (dotted, `jane.doe`), is `400` naming the rule and a login somebody already holds is `409` — without saying who, because a link is evidence of who the caller is and of nothing about anybody else. The redemption is **one record** — the person, their address, their login, the seat the invitation binds and the spent link — so every refusal writes nothing: a seat removed or made an agent's since the issue is `410`, and one a person holds since — a residue only a retained record or a restore leaves, because an open invitation holds its seat until it is redeemed — is `409` saying the seat is taken, naming nobody. Only a person holding the seat refuses it: an older open invitation on the same seat, which a restore can leave, does not. Only a record that could not land is `503`. **Retry until it lands**: a redeemer told their login is taken posts another. A link whose address somebody is already enrolled under is `410`, like a redeemed one. A browser that already holds a session — somebody else's, or the redeemer's own — has it **ended** as `POST /auth/logout` ends it, before the new one opens: its cookie is replaced, and the session it named was left live with no browser holding it. Every sign-in that opens a fresh session does the same (`POST /auth/login`, `POST /auth/token`). The company's **first person** redeems exactly this way: a Tier A token issues their invitation into a human seat the company declares — see [How the first person exists](../concepts/identity-and-access.md#how-the-first-person-exists) |
| `GET` | `/auth/session` | **Who you are**: your id, login, seat, kind, stage and grants; your own `name` as your directory row holds it — absent for a Tier A token, which has no row, and where this node cannot read or open it, which never fails the answer; for a session cookie its `expires_at` and its `lineage` — the id a listing of your sessions names it by, so a client can tell which of them is this browser; and `reauth_at`, the instant your proof of identity stops counting for a [step-up](#some-gestures-ask-how-recently-you-proved-who-you-are) gesture — with `step_up_due` saying whether the next one will ask you to confirm it — and `status`: `signed_in`, or `second_factor_enrolment_required` for a session that may only enrol a second factor, which is one of the routes such a session reaches |
| `POST` | `/auth/token` | Exchanges a **Tier A bearer** — presented as `Authorization: Bearer`, never a cookie — for a one-hour session cookie. The session **is the token**: it names the token's login, and every request re-composes it from the entry this node holds now — the entry's permissions under this node's `api.auth.admin` posture, the seat the identity directory binds the token to, stepped up by construction as the bearer is. Removing or renaming the entry ends it on the next request. It answers to the token's **id**, not its value: a new value put under the same id leaves the sessions the old value opened working until their hour ends, so to cut a leaked value's sessions off at once, rotate by giving the token a **new id** — see [Identity and access](../concepts/identity-and-access.md#the-three-credential-shapes-meet-at-one-frame). `POST /auth/logout` from it closes it as it closes a person's, and `POST /auth/logout/all` from it and `crewlet iam invalidate-all` end it too. A refused bearer here is answered exactly as on every guarded route: `401`, counted on the audit trail's per-minute failure row, and never slowed or refused on its address — see [A bearer is its own protection](#a-bearer-is-its-own-protection) |
| `POST` | `/auth/step-up` | Confirm who you are on a session that is already valid. The only route here that is **both guarded and throttled**: the caller is known, and unbounded retries against a known person is a password oracle with the enumeration already done. It answers a **fresh session cookie** and **ends the session it replaces** first — a close that does not land is `503` with a `Retry-After` and opens nothing, and a presented session that is no longer live is `401`. The replacement confirms the sign-in rather than repeating it, so it keeps the replaced session's absolute deadline. **Only a person confirms**: a machine token is `403 token_refused`, the answer the authority table gives a token wherever a person must be present, and a session that is no person's — one exchanged from a Tier A token, or a service account's — is `403 forbidden` saying why, as `POST /auth/password` answers it, because no proof would lift either |
| `POST` | `/auth/totp` | Enrol a second factor, replacing any you hold. **Two requests**: the first answers a seed and stores nothing, the second presents a code derived from it — which is the only evidence the authenticator app works. Needs a proof inside `step_up`, the second-factor reset's window, and an older one is `403 step_up_required` naming it (`reason`, `window`) as every step-up refusal does. **Never through a machine token**: one is `403 token_refused`, whatever its owner may do, because a token proves nobody is present. **Never by a principal that is no person** — a session exchanged from a Tier A token, a service account: it is `403 forbidden` saying why, as `POST /auth/password` answers it, never `step_up_required`, since no proof it could give would lift it. A factor nobody can confirm is stored is `503` with its `op_id`, never "enrolled". The second leg answers `{"status": "enrolled"}` — and, through a session that could only enrol a second factor, **replaces that session**: the restricted one is ended first, a whole one opens keeping its absolute deadline and its proof instant (the password's — the enrolment's code proves a seed, not who holds it, so it opens no fresh step-up window), its cookie is on the response and it is answered beside the status as `session` (the sign-in's own shape, `status: signed_in`). Such a session enrols only while its person holds **no** second factor — decided in the write's own snapshot — so one that has come to hold one since the session opened is `403 second_factor_required` and nothing is stored: a password alone never replaces a factor. The seed is sealed under your own key before it is stored, and opened only to check a code |
| `POST` | `/auth/totp/recovery` | Issue ten fresh single-use codes, retiring the old set. Answered **once**, in the clear; what is stored is their hashes, so a lost set is regenerated rather than recovered. Needs a proof inside `step_up`, refused as `POST /auth/totp` is, a machine token and a principal that is no person included. Issued only beside an authenticator app: a person who holds none is refused `409 invalid` and issued nothing, because recovery codes held alone are a second factor every sign-in would ask for with only those ten single-use answers — the first set comes with the first enrolment. A set nobody can confirm is stored is `503` with its `op_id`, and the codes are not shown |
| `POST` | `/auth/logout` | End **this** session. The cookie is cleared whatever the write did — a logout that answered 503 would leave somebody looking at a signed-in page on a shared machine. It ends the session behind **either** cookie name — the one this deployment issues, and the other a browser may still hold from before `api.external_url` moved to https, which the guard no longer authenticates — and clears both. Only a session this node's rows still hold is closed and announced as `iam_session_ended`: a cookie past its deadline, revoked, or naming a session a record already ended is cleared and nothing is written, and a node that cannot read its rows records the close without announcing it. **Unguarded**, and that is what makes the promise true: behind the request guard, a node that could not read its identity estate answered `503 identity_unavailable` before the sign-out ran and the cookie stayed set. It verifies every bearer the browser holds itself, and the origin check still judges it |
| `POST` | `/auth/logout/all` | End **every** session you hold, by bumping your own revocation epoch — the one move that is immediate on every node. A revocation nobody can confirm is `503` with its `op_id` rather than a claim that your other sessions ended |
| `GET` | `/auth/reset/{id}` | **Says whose password a reset link sets, and never spends it** — the invitation view's rule, for its reason. The same route opens a person's **first password link**, which `POST /iam/people` hands back when an administrator creates them on their seat: the same kind of credential, opened and spent here identically. **Unguarded**: the link is the credential, its **secret** presented in the `X-Crewlet-Reset-Secret` header and never in the URL; the link a person follows is the dashboard's screen, `<api.external_url>/dashboard#/reset/<id>.<secret>`. Answers `{login, expires_at, min_password_length, first}`: `first` is `true` when the person holds no password yet, so the screen asks them to choose their password rather than a new one and warns of no sessions ending — it is derived from what the person holds, not stored on the link, so a reset link issued to somebody whose first link lapsed unspent is a first password too. Every way a link fails to open — an id that is no link, a secret that is not its link's, a link spent, revoked, ended by a counter or aged out, a person suspended, retired or removed since — is one `410 reset_spent` in the same bytes, and only a link that did **not** prove itself is a failed attempt on the per-minute failure row (method `reset`). A node whose identity rows cannot vouch for a link that does not open is `503 identity_unavailable` instead, counting nothing, as the invitation's view is. It meets no [curve](#a-failure-costs-a-wait-never-a-lockout) |
| `POST` | `/auth/reset/{id}` | **Sets a new password from a reset link, once.** **Unguarded**. `{secret, password}`, the password held to the floor (`422 invalid`, naming the rule). One record sets it, spends the link with every other the person held and moves their revocation epoch — every session and machine token they held ends — judged by the link again in that record's own snapshot, so a link spent from another tab in between is the same `410 reset_spent`. Answers `{status: "password_set", login}` and **no session**: the person signs in next, where a second factor they hold still applies. A first password link is spent by the same record, and for somebody who never held a password it has nothing to end. Each spend is its own operation, so a second one — another tab, or a retry of a spend whose answer was lost — decides again, finds the link spent and is the same `410 reset_spent`, never a `password_set` for a password nothing set; the password the first spend set is the one that works. Issued by an administrator — `POST /iam/people/{id}/password-reset`, or, for a person's first password, the create itself (`POST /iam/people`) |
| `POST` | `/auth/password` | **Change your own password.** Takes `{current_password, new_password, code}`, and the proof is a step-up's, taken in this request — so no step-up is asked first. The current password is verified on the step-up's terms — on the throttle's curve for the login you are signed in as, a wrong one the `401 sign_in_refused` every failed sign-in answers, with `field: current_password` and a `detail` saying so, since the caller is already signed in as the person it is checked for. **`code` is asked of anybody who holds a second factor** — the current code from their authenticator app, or one of their recovery codes — exactly as a step-up asks it: absent, `401 second_factor_required`, which changes nothing and counts as no attempt; wrong or already spent, `401 sign_in_refused`, counted on the person's own curve as well as the pair's (a wait past five seconds is `429` with a `Retry-After`); right, it is spent. Without it, somebody holding your session cookie and your password but not your authenticator could end every session you held and lock you out. Somebody who holds no second factor sends no `code`. The new password is held to the password floor (`422 invalid`, naming the rule). Needs a person present: a machine token is `403 token_refused`, and a session exchanged from a Tier A token, which is no person's, is `403 forbidden`. **One record** sets the password and moves the person's revocation epoch, ending every session and machine token they held and every outstanding reset link; this browser is then handed a new session, proved now and answered in the sign-in's own shape. A change this node has not applied yet is `202` with `status: password_changed` and no session — sign in again with the new password — and one that finds the password changed by somebody else since it was verified is `409 stale` with the cookie cleared, because that change ended this session too. See [Changing your own password ends everything else](../concepts/identity-and-access.md#changing-your-own-password-ends-everything-else) |
| `POST` | `/auth/logout/{lineage}` | End **one named** session, which is how you sign out of a laptop you left somewhere from the browser you are using. The owner is read from this node's rows, and whether you may end it is the authority table's — the same rule as `DELETE /iam/people/{id}/sessions`, so ending one session and ending all of somebody's never answer differently: your **own** on no proof at all (a session exchanged from a Tier A token counts the token's other sessions as its own), and **anybody else's** on `admin`, a proof inside `step_up` and a person present. A refusal is the table's own envelope — `403 unauthorized` naming `admin`, `403 step_up_required`, or `403 token_refused` for a machine token. A lineage nobody holds answers `ended`, as a session already over does — ended by a record, past its absolute deadline, revoked or invalidated — with nothing written or announced; "nobody holds it" is **proved** against the identity log's end, so a node whose rows have not applied every record the log holds (it is behind, or holds one it could not apply) does not say it — a session opened through a peer is a row such a node is still missing, and `ended` would be a lie about a session still running. That node, and one that cannot read its rows at all, answers `503 identity_unavailable` with a `Retry-After` and writes nothing, because the owner the caller is checked against is one of those rows — unlike `POST /auth/logout`, whose lineage comes off the cookie's own signature. A close nobody can confirm is `503` with its `op_id`; asking again is a fresh close, and one that finds the first had landed answers `ended` |
| `GET` | `/viewer` | **Who is asking.** The caller's `login`, the `grants` they carry **on this node**, the effective set: every app an admin holds spelled out, unknown names dropped, and `admin` only where this node's `api.auth.admin` serves it, so a client reads what it holds without expanding anything itself, the seat the identity directory binds them to, its `handle`, `name` and `kind`, all empty for a credential nobody is bound through, and `owner`, the name the caller's own record (inbox, pins, priorities, personal views) is kept under: the seat for a person, or for a token or service account the directory binds to one, and the login for a credential bound to none. `acts` names the tools [`/operator/act`](#operatoract--the-dashboards-write-surface) would serve this caller, the catalogue's writes the authority table can admit them to before any object is named, always an array, `project` is where a create of theirs that names no project files, `""` for a caller bound to no seat or a seat whose team owns no project, and `space` is where a page of theirs that names no container lands, by the same rule. A node that cannot yet decide `acts` answers `503` rather than an empty list, which would lock every control the caller holds the authority for. A credential bound to no seat, a Tier A token's session, a service account bound to none, is an **ordinary state**, not an error: a pipeline's token acts under its own login, and binding it to a seat is a directory row rather than a different credential. A person always holds a human seat, so a person is never answered unbound. `config_writer` is whether the caller's credential may change the company document, false for an anonymous caller and for every credential a [managed document's](../concepts/configuration.md#managed-configuration) `api.auth.company_writers` does not list, a person's session, a machine token and a lead's among them; it says nothing about authority, which `admin` or a lead's subtree still decides, and `config_managed_by` the Tier A token ids that alone may change it when another system manages it, empty when nothing does and for an anonymous caller. `assistant` is whether [Ask Crewlet](#ask-crewlet) is available to the caller on this node, `{available, reason?}`, with the first reason it is not: `disabled` (the company has not turned it on, or this node does not serve Ask Crewlet), `no_model` (`assistant.llm` names no chain Ask Crewlet's rules admit), `not_a_person` (a machine token, a service account or a Tier A token's session), `no_permission`, or `model_not_ready`, checked last so only a person Ask Crewlet would answer learns it: every model in `assistant.llm` is known unusable right now, read from what the node already holds and never by calling a model (its plan spent until a reset, its harness signed out or with nothing that signs it in, or its harness unable to start). With that reason the member carries `model: {code, message, hint?, retry_at?}`, the [turn error](#how-a-turn-fails-on-the-model) a question would have failed with, in the same words, and the hint an administrator or anybody else is given. `chat_vendors` names the vendor chat a company talks on, to a holder of `chatlet` (see the [`viewer`](#queries) question) |
| `GET` | `/stream/snapshot` | Dashboard initial-state bundle, served from the in-memory projection (REST fallback for the WebSocket) |
| `WS`  | `/ws/stream` | Live dashboard stream — agents, events, LLM invocations, health |
| `GET` | `/dashboard` | Dashboard shell (`/` redirects here; `/static/{path}` serves its assets) |
| `POST` | `/webhooks/jira` | Receive Jira Data Center webhooks (Cloud arrives via `/webhooks/forge`) |
| `POST` | `/webhooks/slack/{handle}` | Receive Slack Events API deliveries for one seat's app |
| `GET` | `/webhooks/slack-oauth` | OAuth install landing: finishes an install the Agents tab began |
| `POST` | `/webhooks/github` | Receive GitHub webhooks — HMAC-SHA256 over the raw body |
| `POST` | `/webhooks/github/{handle}` | The same route addressed to one seat, which is where that seat's own [GitHub App](../integrations/github.md#one-github-app-per-agent) delivers |
| `GET` | `/webhooks/github-app` | Landing page for the per-agent GitHub App flow: converts the one-time creation code, or reports an install (see [below](#get-webhooksgithub-app)) |
| `POST` | `/webhooks/gitlab` | Receive GitLab webhooks |
| `POST` | `/webhooks/confluence` | Receive Confluence Data Center webhooks, HMAC-signed. Cloud arrives on the two routes below instead |
| `POST` | `/webhooks/confluence/{event}` | Receive one Confluence **Cloud** event, authenticated by the shared token the registered URL carries — `X-Crewlet-Token` first, `?token=` as the fallback, because Cloud honours no registration field for a header |
| `POST` | `/webhooks/datadog` | Receive a Datadog monitor alert, authenticated by a constant-time comparison of `X-Crewlet-Token`. Datadog signs nothing, so the token is the whole check: an unset one answers `503`, and one shorter than 26 characters answers `503` too — see [Datadog](../integrations/datadog.md#verification-is-weaker-here-and-that-is-the-providers-ceiling) |
| `POST` | `/webhooks/forge` | Receive Forge events (FIT-verified) |
| `POST` | `/otlp/{token}/v1/{signal}` | Engine-fronted OTLP receiver for [sandbox](../concepts/code-sandbox.md) telemetry (per-run token in the path). Mounted where `CREWLET_SANDBOX_OTEL_RECEIVER_URL` is set, or, for a node's own `direct` boxes, where it runs seats, binds `api.port` and forwards to `OTEL_EXPORTER_OTLP_ENDPOINT` — on a node without `ingress` to its own host's loopback alone, as the bridge is |
| `GET` `POST` `DELETE` | `/mcp/{token}` | The [tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box): one running seat's tool surface, served over streamable-HTTP MCP to the seat's executor, its harness's run in a box — by the node holding the seat, to which the node the call reaches relays it over the broker. Per-run token in the path; all three verbs because that is what the transport uses. A call is admitted under any `Host`, a proxy's or tunnel's public name on the node's loopback included. `401` for a token that is forged, expired, or names a run that is over, and for a call to an Ask Crewlet deep-work run held to the engine host's loopback that comes from any other address, or names a `Host` that is neither loopback nor a container runtime's name for the engine host (`host.docker.internal`, `host.containers.internal`); `503` with a `Retry-After` while no node can answer for the run's seat, after waiting up to two seat-lease lifetimes for one; `502` when the seat's holder took the call and stopped answering before it wrote a status. Mounted only on a node that has a bridge: one given `CREWLET_MCP_BRIDGE_URL`, or one that runs seats and binds `api.port`, for its own `direct` boxes. A node without `ingress` mounts it on `api.port` beside its probes and serves it to its own host's loopback alone, answering any other caller `404 no_route` (see [Probes on a node without ingress](#probes-on-a-node-without-ingress)); on any other node the route is absent |
| `GET` | `/mcp/{token}/steer/notes?after={seq}` | The run's [notes](../concepts/code-sandbox.md#notes-to-a-running-run): the notes waiting for its turn past the cursor, `{"notes": [{seq, id, inbox, message}]}`, held up to ten seconds while none waits and answered at once when one arrives. `503` with a `Retry-After` once the node is stopping (`draining`) or the run's session has ended here (`unavailable`), the held poll included: its notes wait for the session's next opening. The same token, routing and refusals as the bridge; `404` for a run that has nothing to steer |
| `POST` | `/mcp/{token}/steer/read` | Record the notes the run's harness put in front of its model, `{"ids": [...]}` (one to ten, at most 64 KiB): `204`, or `503` with a `Retry-After` when the record could not be written, and the notes wait |
| `GET` `POST` `DELETE` | `/operator/mcp` | Worklet, Doclet and Chatlet, served over MCP to **your** AI assistant. **Always needs a credential**, which for an assistant is a [machine token](#post-iamcredentials-mints-a-machine-token) acting as you: it files and moves work (see [below](#operatormcp--your-own-assistant)). The catalogue is built per request: `503 no_active_revision` until this node has met its first company, and never empty after that. Pausing, resuming and steering a seat and answering a parked run are about the fleet's own records, so every company is served them whatever backends it runs |
| `POST` | `/operator/act/{tool}` | The same catalogue's writes, one tool per request, **as the principal your request resolves to**: the dashboard's write surface. Any credential the guard resolved; every tool is decided by the authority table, as on every other surface, and the `Idempotency-Key` header is required (see [below](#operatoract--the-dashboards-write-surface)). Answers `503 no_active_revision` before the first company, as `/operator/mcp` does, and `404 unknown_tool` for a tool the company's catalogue does not serve |
| `POST` | `/assistant/proposals/{id}/approve` | Approve one of **your own** Ask Crewlet proposals. A proposal whose turn read through an app you no longer hold is not sent: it settles `failed` with the refusal `access_withdrawn`, and the turn ends rather than carrying on with what it read. Your session cookie and a matching `Origin` only (an assistant grant is refused before routing), `Content-Type: application/json`, the `Idempotency-Key` header, and `{"protocol_version": 1, "digest": "sha256:..."}` echoing the digest the card showed, with an optional `page_context`: the screen you decided from, as it is now, which the resumed turn runs on (the actions it accepts, the draft as the change left it). A turn you asked without a page never gains one this way, and a `page_context` the screen's own rules refuse is `400 invalid_body` before anything is decided. Refusals: `404 not_found` (not yours, or no such proposal), `409 proposal_already_decided` (with its `state`), `410 proposal_expired`, `409 proposal_digest_mismatch` (nothing sent), `409 proposal_stale` (the record moved since it was shown), `403 step_up_required` (nothing recorded; confirm it is you and send the same request again), `503 unavailable` with `"outcome": "unknown"` and the `op_id` to retry under for a decision nobody can confirm, and `503 unavailable` without `outcome` for a condition of the node, which recorded nothing: its `detail` says what happened in a sentence for you, with a `Retry-After` exactly where waiting clears it. A server write is sent in process as you, once, under the proposal's own operation key, and `202` answers its state (`applied`, `failed` or `unknown`), the turn and where its continuation streams. An `unknown` proposal is retried by approving it again: the same write, never a second one |
| `POST` | `/assistant/proposals/{id}/decline` | Decline one of your proposals: the same body, header and refusals as the approval; nothing is sent, and Ask Crewlet is told. `200` with the proposal's state |
| `PUT` | `/assistant/actions/{id}` | A page claiming (`{"state": "applying"}`), then reporting, a page action you approved (the report may carry `page_context`, the screen as the action left it, read as the approval's is), or a view change (`confirm: none`: a filter, a range, a lens) the turn sent to the page at once, recorded as a proposal it approved itself; the report of a view change settles it and resumes nothing, since its turn never waited for it. The claim succeeds once across your tabs; another tab is `409 action_already_claimed`. A claim must name the revision the screen is at for that action (its `action_revisions` entry, else `source_revision`): one whose screen moved since the action was prepared is `409 proposal_stale`, and nothing is applied. The report (`applied`, `stale`, `blocked`, `invalid`, `no_handler`, with the page's `revision`) is the page's own statement, which Ask Crewlet reads as client-reported. `Idempotency-Key` is optional: a claim sent again under its key is the same claim |
| `POST` | `/assistant/audit/conversations/{id}/read` | An administrator's **compliance read** of somebody else's Ask Crewlet conversation (`assistant.conversation.audit_read`: admin, a recent proof, a person present, never Ask Crewlet). `{"protocol_version": 1, "reason": "..."}`, the reason required and at most 500 characters. The read is recorded first, beside your name and reason, and the conversation's owner is shown it; then `200` answers the transcript and an `assistant_conversation_read` audit record names you, the owner and the reason |
| `POST` | `/assistant/audit/conversations/{id}/files/{file}/read` | The same **compliance read** of one file the conversation keeps, on the same verb and with the same body: the read is recorded beside the conversation first, the `assistant_conversation_read` audit record names the file as well (`file_id`), and `200` answers the file with every header of [the owner's download](#ask-crewlet). A file the conversation does not keep is `404 not_found`, and the read stays on its record. Absent on a node with no object store |

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
> `/otlp/*`, `/mcp/*`, the dashboard shell (`/`, `/dashboard`, `/favicon.ico`,
> `/static/*`), and `/auth/config`, `/auth/login`, `/auth/logout`,
> `/auth/invite/*` and `/auth/reset/*` — a login cannot require a login. That is an **exact list and not a `/auth/` prefix**: the same surface
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
> `Retry-After` says rather than on its own backoff, and a `409
> reader_changed` — a handshake made for a reader the browser's cookie is no
> longer — and hands the tab to whoever it is.
>
> **`api.auth.admin` clamps a person exactly as it clamps a token.** On a node
> whose posture is `refuse`, every principal composed there loses `admin` and
> keeps every app. The posture is applied when the request is resolved, not
> written anywhere, so a restart that changes it takes effect on this node's
> next request — including for somebody already signed in.
>
> **A person whose seat is gone is `403 seat_unavailable`, naming the seat** —
> and so is a Tier A token the identity directory binds to one, since both
> bindings are resolved against the company this node runs. Their credential is
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
> guards them — `POST /auth/login`, `/auth/invite/{id}` and `/auth/reset/{id}`
> — because a sign-in posted from somebody else's
> page is how an attacker leaves a victim's browser signed in as somebody the
> attacker controls. See
> [Identity and Access § A cross-site write is refused by its Origin](../concepts/identity-and-access.md#a-cross-site-write-is-refused-by-its-origin).
>
> **A permitted origin may send exactly the request headers the routes read,
> and read exactly the response headers a client acts on.** The preflight
> answers a closed set and never echoes what the browser asked for, and the
> answer exposes a closed set, so a page on an `api.auth.allowed_origins` entry
> can make every request the dashboard can — the browser refuses the whole
> request when it carries a header the preflight did not allow, and hides from
> the page every response header the answer did not expose:
>
> | Request header a permitted origin may send | Read by |
> |---|---|
> | `Authorization` | the guard: the bearer token |
> | `Content-Type` | every route that takes a body |
> | `Idempotency-Key` | [`/operator/act`](#operatoract--the-dashboards-write-surface), which requires it, and `/worklet`, `/doclet/pages`, `/chatlet` and `/iam`, which make a retry the same write by it |
> | `If-Match`, `If-None-Match` | `/config` and its entities, a work item, a page, a project file, an artifact's publish and `/setup`'s forced disconnect — see [Conditional requests](#conditional-requests) |
> | `X-Summary` | a `/config` write's revision summary |
> | `X-Crewlet-Invite-Secret`, `X-Crewlet-Reset-Secret` | `GET /auth/invite/{id}` and `GET /auth/reset/{id}`: a link's secret, kept out of the URL |
>
> | Response header a permitted origin's script may read | What a client does with it |
> |---|---|
> | `ETag` | writes it back as `If-Match`, or revalidates with it as `If-None-Match` |
> | `Retry-After` | waits that many seconds before asking again |
> | `Accept-Patch` | sends a refused `PATCH /config` again in a format it names |
> | `Allow` | uses a method an `OPTIONS` or a `405` lists |
> | `Content-Disposition` | saves a downloaded project file under its name |
>
> `Content-Type`, `Content-Length` and `Cache-Control` are readable without
> being listed, as every CORS-safelisted header is. `X-Crewlet-Reader` is
> **not** offered and stays same-origin only: only the dashboard sends it, from
> this process's own origin, and its whole job is to say which person a tab of
> the dashboard was opened for. Neither are the headers only a browser or a
> proxy sets (`Origin`, `Upgrade`, `Accept-Encoding`, `X-Forwarded-For`), the
> webhook routes' vendor headers — no page calls `/webhooks/*` — nor the MCP
> transport's own session headers: `/operator/mcp` and `/mcp/` are reached by
> programs, not pages. Nor is `Last-Event-ID`, which
> [Ask Crewlet's stream](#the-turns-stream) resumes by: the stream
> answers nothing but a person's own browser session, and no answer here
> allows another origin credentials, so only the dashboard, on this process's
> own origin, ever opens it. A gate in `internal/api/auth` walks the API's source
> for every header a route reads or sets and fails on one neither offered here
> nor withheld for a stated reason.
>
> **And it may use every method a route serves**: `GET`, `HEAD`, `POST`,
> `PUT`, `PATCH`, `DELETE` and `OPTIONS`, the last for `OPTIONS /config`,
> whose `Allow` and `Accept-Patch` say what the document takes. Every method
> but the first three is preflighted, and the preflight names exactly the
> methods the routes are registered with — a gate beside the header one holds
> it to them — so a method left out is a request the browser refuses outright.
> See [Configuration § Auth](../concepts/configuration.md#auth).
>
> **A request that says who it is for is served only to them.**
> `X-Crewlet-Reader: <principal id>` — the `person` `GET /auth/session`
> answers — names the principal a request is made FOR, and the dashboard sends
> it on every request a tab makes for the person it was read by. A guarded
> route serves such a request only when its credential resolves to that
> principal; anybody else is `409 reader_changed`, written by the guard before
> any handler runs, so nothing is read, written or audited and no
> `Idempotency-Key` is scoped. It is what keeps a window left open beside a
> sign-in as somebody else — whose cookie moved under it, and whose socket the
> sign-in's best-effort close of its session never reached — from making the
> last person's change, or retrying it, as the new one; the dashboard hands
> that tab to the new person on the answer. It holds under `/auth/*` too, so a
> **Sign out everywhere** pressed on the last person's Account ends nobody's
> sessions, and an unguarded route never reads it. A request that names no
> reader is decided exactly as before — scripts, assistants and the command
> line send none — and so are nobody (`401`) and a node that cannot tell
> (`503`): there is no principal to hold the name against. The value is
> compared as an id, and one that is not an id names nobody the credential
> is. It is not a credential and widens nothing.
>
> **A socket's handshake names its reader in its URL**, because a browser sets
> no header on a WebSocket: `/ws/stream?reader=<principal id>`. The guard reads
> the `reader` parameter on a WebSocket upgrade only, and only where the
> request carries no `X-Crewlet-Reader` — on any other request a URL is never a
> second way to say who it is for — and holds it to the credential exactly as
> it holds the header, so a handshake made for somebody else is `409
> reader_changed` before the stream is served and draws nothing of the new
> person's company. A browser reports that refused handshake as a bare 1006,
> so the dashboard reads the `409` off its plain-HTTP re-ask of the path
> (above), which names the reader in the header, and hands the tab over. The
> principal id is not a secret — every `GET /auth/session` answer carries it —
> but it is in the socket's URL, so a proxy's or a balancer's access log of
> `/ws/stream` records it; the engine's own logs name the path alone.
>
> **Every `/webhooks/*` route fails closed.** They are exempt from the bearer
> token because each verifies its provider's signature instead — so a route
> whose secret is not configured has nothing to verify with, and answers `503`
> with `Retry-After` rather than accepting the delivery. The sender retries and
> the delivery flows once the secret is set; nothing is discarded, and nothing
> unsigned is ever recorded, published, or shown on the dashboard.

Plus the surfaces whose reads are as sensitive as their writes: [`/config/*`](#config--live-config-management-auth-gated), [`/secrets/*`](#secrets--the-companys-credentials-auth-gated), [`/setup/*`](#setting-an-integration-up), and `/operator/*` — [`/operator/mcp`](#operatormcp--your-own-assistant) and [`/operator/act`](#operatoract--the-dashboards-write-surface). The list of which credentials a company has not configured yet is a map of what to attack, and so is the shape of the company itself.

### Every failed sign-in is one refusal

`POST /auth/login` answers `401 sign_in_refused` for every way it can fail — a
login nobody holds, a wrong password, a person suspended, a person removed, a
second factor that does not check out — and every one of them spends the same
argon2id derivation first.

**Both halves are needed.** A code that distinguished the arms would make the
equal work pointless, and a missing derivation would make the single code
pointless: a miss is verified against a dummy verifier at the same cost a
real one is, because otherwise the *absence* of that cost is the answer. Every
derivation waits for a slot of the node's verify cap, first come first served,
and a request that goes away while it waits is answered `503` and counts as no
attempt at all. Nothing is padded: what still separates the arms is the
directory read before the derivation, and a stored verifier written at another
cost than this build's — see
[Identity and Access](../concepts/identity-and-access.md#a-sign-in-endpoint-is-not-a-roster).

What that buys is that this surface is not a roster: a caller cannot learn who
works here, nor test a list of addresses against it.

The exceptions are specific, and each discloses nothing the caller did not
already have:

| Code | Why it is safe to be specific |
|---|---|
| `throttled` | `429` with a `Retry-After`: the seconds until this attempt is admitted. Keyed on the subject **as typed** from the caller's source — never on the source alone, and never on what it resolved to — a name nobody holds climbs the curve exactly as a real one does, so a stranger learns only that they failed recently from where they are, which they already knew. Keyed on the resolved person it would be an oracle — "this account exists and I can slow it down" — which is why the one curve that is, a second factor's, is reached only past the password |
| `second_factor_required` | Reached only by somebody who already passed the first factor, so it discloses nothing to a stranger — and without it a client cannot tell "your password is wrong" from "now type your code", which are different screens. `POST /auth/step-up` answers it on the same rule, a `401`, to a right password with no code from a person who holds a second factor. It is also the `403` a session that may only enrol a second factor meets at `POST /auth/totp` once its person holds one — enrolled since, from another session: a password alone never enrols over a factor, and the remedy is to sign in again with it |
| `second_factor_enrolment_required` | A `status` on a successful sign-in, and a `403` from every guarded route but three for the session it opened — reached only by somebody who proved the password, so it says nothing to a stranger. The deployment requires a second factor and this person holds none: the session may read `GET /auth/session`, enrol one at `POST /auth/totp` and re-confirm the password at `POST /auth/step-up`, and sign out at `POST /auth/logout`, which no guard stands in front of — and enrolling replaces it with a whole one. Its own code rather than `step_up_required`, because no fresher password changes the answer |
| `invite_spent` | Read by somebody holding the link, which is already evidence it was issued to them. One code for redeemed, withdrawn and expired — and for a secret that is not the link's, answered in the same bytes as an id nobody issued — because the remedy is the same and telling them apart would say "already used" to somebody whose link merely aged out, or say which ids exist to somebody guessing secrets |
| `reset_spent` | A password reset link's `410`, for `invite_spent`'s reasons: read by somebody holding the link, one code for spent, revoked, expired and a person the reset no longer reaches — and for a secret that is not the link's, in the same bytes as an id nobody issued. The remedy is the same: ask an administrator for a new one |

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
did not. The code that completes the sign-in lifts it, and the wrong code that
takes the curve to its ceiling is announced as `iam_second_factor_throttled` —
once per climb, so a run held there announces nothing more — because somebody
holding that person's password is guessing at their code, and the password is
what to rotate.

**No address is ever refused on its own.** A curve on the source alone —
which this surface had, ten failures free and then the same doubling wait — is
one anybody sharing the address holds shut for everybody else: one stranger's
failure every twenty-five seconds, at any name, kept every sign-in from that
office or proxy at `429`, the right passwords included. So one password tried
against many names from one address meets no curve; it is bounded by cost —
every name tried is an argon2id derivation under the node's verify cap — and
by the password floor and blocklist, and it is shown by the audit trail's
per-minute failure count, which names the source. A credential that names
nobody — an invitation link — meets no curve at all, and every refusal of one
is still counted.

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
a guarded route is still a failed attempt in the audit trail's per-minute
count by source, so a spray is seen; it is never answered `429`, however many
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
trusted, and every start of the node logs the same warning as
`config_advisory`, because the engine never terminates TLS itself.

### Which permission a route needs

Holding a credential is not the same as being allowed to use it here. Every
route and every socket question declares who may ask it — **everybody**, one
of the [permissions](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app)
(`admin`, `worklet`, `doclet`, `artifacts`, `chatlet`), or a verb of the
authority table that the answer decides record by record — and a principal that
does not carry what it declares is refused, a Tier A token included, whose
declared permissions are read through the node's `api.auth.admin` posture on
every request.

**`403`, not `401`**, and the difference matters to a client: `401` means
*present a credential* and `403` means *the one you presented does not carry
what this needs*. A person using one app meets the second the moment they open
a screen of the other, which is the ordinary case — so it must not be reported
as the first, which tells them to go and get a new credential.

**A `403` says which rule refused, and what would have admitted you.** Beside
`"error": "unauthorized"` it carries two detail fields wherever the authority
table made the refusal — every question, the policy every `/config/*` and
`/iam/*` route is mounted with, and the snapshot mirrors alike. Which code a
refusal is answered with is the table's own mapping on every surface, the
WebSocket's frames included — `unauthorized`, `step_up_required` for a stale
proof, `token_refused` for a machine token where a person must be present — and
no route spells one of those codes for itself:

```json
{
  "error": "unauthorized",
  "message": "…",
  "reason": "no_grant",
  "grants": ["worklet"]
}
```

`reason` is the authority table's own word for the rule that decided —
`no_grant`, `not_self`, `not_lead`, `not_author`, `stage`, `seat_refused`,
`seat_only` (a person or a token asking for a verb only an agent seat takes),
`unseated` (a credential the identity directory binds to no seat asking for a
verb made as one — bind it to a human seat, `crewlet iam bind`),
`token_refused` (a [machine token](#post-iamcredentials-mints-a-machine-token)
on a gesture that needs a person present, answered under the code
`token_refused` rather than `unauthorized`), `assist_refused` and
`assist_scope` (a request Ask Crewlet made on a verb its row keeps
from it, or a write through a read grant: see
[Every row says what Ask Crewlet may do with it](../concepts/identity-and-access.md#every-row-says-what-ask-crewlet-may-do-with-it)),
`unnamed`, and `step_up` on the one
refusal the caller clears themselves
([below](#some-gestures-ask-how-recently-you-proved-who-you-are)) — and `grants`
are the **least** that would have admitted this caller for this object: any
**one** of them, added to what the caller holds, would. A Worklet row names
`["worklet"]`, a relation the caller does not hold names `["admin"]`, a lead
missing the app names the app, and a question either app answers names both.
An empty `grants` is an answer rather than an omission: no permission would —
a seat-only verb, a verb made as a seat asked by a credential bound to none, or
a relation the org chart does not hold. The [human write
surface](#the-human-write-surface) answers its own refusals `403 forbidden` in
its tools' wording instead.

The `message` follows the `reason`, and names a permission only where one is
the remedy. `no_grant` names the permissions in `grants` ("You don't have the
access this needs (Worklet)"); a refused relation (`not_self`, `not_lead`,
`not_author` or `unnamed`) says what the rule asked and names the `grants`
beside it as what would admit you anyway, or says that no access would; and
every other reason says what does clear it, whatever `grants` holds: `role`, a
share of an artifact below the role the verb asks (which every route answering
it names as `role`, the role to ask the owner for), that its owner can share it
with you at a higher one (the admin path admits whatever the shares say, but
the share is what to ask for), `unseated` a credential bound to a human seat
(`crewlet iam bind`),
`stage` an account an admin activates (`crewlet iam activate`),
`seat_refused` and `seat_only` that the verb is a person's or an agent's,
`ceiling` that the person an agent is working for could not do it either,
`assist_refused` that Ask Crewlet never takes the gesture, which
stays with you, and `assist_scope` that Ask Crewlet makes the change only as
one you approve.
(`step_up` and `token_refused` are answered under codes of their own, whose
messages say what clears them — a fresh proof of who you are, signing in as
yourself — and `not_shared` is never seen at all: an artifact nothing relates
you to is answered `404 not_found`.) A refusal carrying no `reason` is told
the code's own sentence, which names every permission there is.

A request the node **cannot decide** — it holds no company yet — is
`503 unavailable` with a `Retry-After`, never a `403`: a lead
told they lead nobody goes looking for an authority they already hold.

| Needs | What it reaches |
|---|---|
| **everybody** | The [overview](../concepts/identity-and-access.md#everybodys-overview), for every principal the guard resolved whose stage may act: `/agents`, `/agents/activity` and the `agent` and `live_call` questions (an agent's **status**, with its **body** to an administrator and to whoever leads the seat, [below](#a-status-reader-never-receives-a-transcript)), `/org`, `/tools`, `/schedules` and their runs, `/budgets`, `/executors` (status; the same body rule), `/tokens/*`, `/viewer`, `/stream/snapshot`, `/ws/stream`, `GET /iam/seats` (masked per reader), the `seat_activity` and `colleague` questions, and the cross-app answers filtered row by row: `/feed`, `decisions`, `answered_decisions` and the `knowledge` question |
| `worklet` | The reads under `/worklet/*`, the [write surface's](#the-human-write-surface) item routes, a project's files (`GET`/`PUT /worklet/files/…`; removing one is the project lead's), every `work_*` question, the socket's `inbox_changed` push, and `/operator/mcp`'s Worklet half. Some verbs also ask a RELATION beside the app: re-routing, a project's policy and taking an item out of circulation are its project lead's, and a person's inbox, queue and person view are their own and their lead's |
| `doclet` | The reads under `/doclet/pages/*` and `/containers`, the page questions, and writing Doclet's pages. A rename, the trash and a restore are also the container's lead's or the page's creator's; see [the write surface](#the-human-write-surface) |
| `chatlet` | The reads under `/chatlet/*`, every `chat_*` question, the socket's eight `chat_*` pushes, and the chat tools of `/operator/mcp` and `/operator/act` — each made **as a seat**, so a principal acting as itself is refused them `unseated` whatever else it holds, and which room it reaches is the chat domain's own rule rather than a permission (see [Chatlet](#chatlet)). A room's horizon, an erase and a prune are `admin`'s instead, with a recent proof; see [A chat erase, prune or horizon](#a-chat-erase-prune-or-horizon) |
| `admin` | Running the company and the engine, and **host access**, conferred like it: the configuration it writes runs commands on every engine host (an `mcp_servers` entry, a seat's `mcp_env`, a `providers.llm` model — a harness the host runs — and a `direct` cell, an entry's `run_in` or `providers.sandbox.default_run_in`, which runs agent seats' executors there as fully permissioned harnesses), so a holder can run anything there and read whatever a process there can, the keyring included (see [Identity and Access](../concepts/identity-and-access.md#what-admin-runs)). Every route under `/config*` (the org chart's leads may also read and write what is inside the units they lead: [A lead edits their own team](#a-lead-edits-their-own-team)), `/integrations`, `/mcp-servers`, `/setup`, every `/secrets` route, every `/iam/*` route about anybody but the caller, the deployment's own controls (`/fleet`, `/fleet/broker*`, every `/worklet/retention*` route, `/backup`, `/backups`), the company-wide trail (`/events*`, the socket's `event` push and the snapshot's `events` section, the turn, phase, trace and A2A-channel questions, `/iam/audit`, `/artifacts/trail`), a seat's resolved model chain and tool sources on `/org`, the workspace catalogue's writes, a page in the tool-skills container, the artifacts' quota counters (`GET /artifacts/usage`), the three purges (`/worklet/items/{key}/purge`, `/doclet/pages/{id}/purge`, `/artifacts/{id}/purge`) and chat's three destructions (`/chatlet/channels/{channel_id}/erase`, `/prune` and `/retention`). It **includes every app**, and it is the **admin path** of every relation rule: a holder is admitted where a lead or an owner would be |
| `audit.seat.read` | One seat's own trail — `/agents/{id}/memory`, `/agents/{id}/memory/episodes/{episode}`, `/agents/{id}/conversations`, `memory_overview` (filtered to the seats the caller may read), `turns?seat=` and `phases?seat=`, and an executor's tail (`/executors/{turn_id}/tail`) — decided per seat: **its lead**, or `admin` |

A question asked on the socket is decided by the same declaration the REST
route is — one registry, both transports — so there is no way round a
permission by choosing a channel.

### A status reader never receives a transcript

Everybody sees what the agents are doing; only some read what they *said*. An
agent's and an executor's answers split into a **status** half and a **body**
half — on the `agent` and `live_call` questions, the `agents`, `snapshot` and
`executors` pushes and their mirrors (`/agents`, `/stream/snapshot`), and
`executors` — and the body goes only to an administrator, to whoever leads
that seat (`audit.seat.read`), and, for an executor's parked question, to the
person it is put to. The body is a call's `trigger`, `prompt`,
`prompt_messages`, `response`, `tool_executions`, `round_narration`,
`partial_round` and the `arguments` of each `running_calls` entry, an `llm_history` entry's
`payload`, and an executor's `task_description`, `question`,
`question_context`, `decision`, `reply` and `bridge_calls`. A status reader receives the rest with **`body_withheld:
true`** and the body's version counters at zero, so a client never marks the
call stale and asks for a body it will not be sent. A work item a call or a
turn names (`work_item`, an `llm_history` entry's `work_item` and `task_id`
tags) is Worklet's, and is left out for a reader without `worklet`.

### Some gestures ask how recently you proved who you are

A session lives for days, so the gestures that change what a company *is* ask
for a proof of identity taken recently — the **step-up**. It is decided by the
same authority table as the permission, on the same row, so a REST route and a
socket question about one verb cannot disagree, and it is asked only **after**
the permission or relation admitted you: a caller who could never make the gesture
is told what they lack, not sent to confirm who they are first.

| Window | Setting (default) | What asks for it |
|---|---|---|
| `step_up` | `api.auth.session.step_up` (1 hour) | Every write under `/config*` (a [dry run](#dry-runs) excepted, which changes nothing), `/setup`'s writes, `PUT`/`DELETE /secrets/{name}`, `POST /secrets/rekey` and revealing a value (`GET /secrets/{name}?reveal=true`, `POST /secrets/reveal`), the deployment's own controls (`POST /backup`, `POST /objects/migrate`, every `POST /worklet/retention*` and both `POST /fleet/broker/remove*`), the three purges (`POST /worklet/items/{key}/purge`, `POST /doclet/pages/{id}/purge`, `POST /artifacts/{id}/purge`), chat's three destructions (`POST /chatlet/channels/{channel_id}/erase` and `/prune`, `PUT /chatlet/channels/{channel_id}/retention`), and every `/iam` write: `POST /iam/people`, `PATCH /iam/people/{id}`, `DELETE /iam/people/{id}`, `POST /iam/invitations`, `DELETE /iam/invitations/{id}`, `POST /iam/people/{id}/mfa/reset`, `POST /iam/people/{id}/password-reset`, `POST /iam/credentials`, `DELETE /iam/credentials/{id}`, ending somebody else's sessions and `POST /iam/invalidate-all`; and your own `POST /auth/totp` and `POST /auth/totp/recovery` |
| none | | Every read, the two deployment reads (`GET /worklet/retention/maintenance`, `GET /worklet/retention/reanchor`) included; ending your own sessions (`DELETE /iam/people/{id}/sessions` naming yourself), which is the first thing to do on finding somebody else in your account, while an administrator ending somebody else's asks `step_up`; changing your own password (`POST /auth/password`), whose current password (and code, from somebody who holds a second factor) is itself the proof; and every work, knowledge, artifact and chat verb but the three purges and chat's three destructions: the tools, `/operator/mcp`, `/operator/act` and the rest of the human write surface |

A proof that is too old is **`403 step_up_required`**, the code the sign-in
surface answers for the same fact, carrying the window it needs so a client can
ask the person to confirm who they are and send the same request again —
`POST /auth/step-up` with a password and second factor:

```json
{
  "error": "step_up_required",
  "message": "…",
  "reason": "step_up",
  "window": "step_up",
  "grants": ["admin"]
}
```

The step-up **replaces** the session it was made from, ending it first and
keeping its absolute deadline.

`window` is spelled as the setting that sizes it, and there is ONE: the
practice of GitHub's sudo mode, one window over every sensitive gesture, at the
stricter end of it. `GET /auth/session` answers the deadline the table judges
against (`reauth_at`) and whether it is already due (`step_up_due`), so a
screen can say so before somebody starts rather than after they submit.

**A request is judged at the instant it arrived.** Every check one request
makes — its route's, whatever the handler decides once it has read the body,
such as each part of a `/config` write or `?reveal=true` on a secret, and every
tool a route calls on the request's behalf, as the
[human write surface](#the-human-write-surface)'s routes do — asks the proof
at the instant the route decided, so a proof that was fresh when the request
arrived never lapses half-way through it.

**A credential with nobody at a keyboard is fresh by construction** — a Tier A
token (and a session exchanged from one), the development principal, and a
personal access or service token. There is nothing else any of them could
present, and the break-glass credential has to reach a step-up gesture on the
day nobody can sign in as a person. What keeps a machine token off the
gestures that need a person present is therefore never the clock, nor a
permission it cannot carry — a token may be minted with `admin` — and it is two
locks. Every row that needs a person says so, and a request that presented a
machine token is refused there `403 token_refused` with `reason:
token_refused` and no `grants`, since no permission would change it — a code
of its own, whose `message` says a token cannot do this and its owner must,
signed in, because `unauthorized`'s is about a missing permission: revealing a secret, every `/iam/*` write that
changes who may do anything, ending every session in the company, changing how
somebody proves who they are (`POST /auth/totp`, `POST /auth/totp/recovery`,
and revoking a password, a second factor or the recovery codes through
`DELETE /iam/credentials/{id}`), an artifact's forced publish, transfer and
purge, and — about anybody but the token's own owner — minting or revoking a
token and ending sessions. A token may still revoke its
owner's tokens, itself included, and end its owner's sessions, which ends it
too: that is what somebody who finds one leaked reaches for. And the identity
domain refuses every administrative record — an enrolment, an invitation, an
edit of somebody's row, a suspension, a removal, ending everybody's sessions —
written through a machine token, `403 token_refused`, whatever the token
carries, at the record, so a CLI or a duty is held to it as a route is.
`POST /auth/step-up` refuses a
token itself: there is nobody to confirm. No tool asks for a proof: a seat has
no keyboard, and `/operator/mcp` is not a step-up surface.

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
| The dashboard shell (`/dashboard`), `/favicon.ico` and every `/static/*` asset | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self' https:; require-trusted-types-for 'script'; trusted-types 'none'` (Trusted Types enforced with no policy, since the dashboard writes no markup or script sink), then `frame-src` naming the artifact origin on a deployment that serves artifacts |
| The landing pages [`/webhooks/github-app`](#get-webhooksgithub-app) and [`/webhooks/slack-oauth`](#get-webhooksslack-oauth) | `default-src 'none'; img-src data:` (a page's mark is inlined, since `/static/` is not served on the [public listener](#which-listener-serves-what) these pages move to), then `style-src` and `script-src` naming the `sha256` hash of each page's own inline block (`'none'` where a page has none), then `base-uri 'none'; form-action 'none'; frame-ancestors 'none'` |
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
`executors`, `budgets`, `integrations`, `webhooks`,
`dashboard`, `health`);
`webhooks` and `/config/*` keep a stable external contract, while the
read/stream surface is free to evolve since the dashboard is its only
consumer.

### `/iam/*` — the company's people, credentials and sessions (auth-gated)

**Always guarded, reads included**, for the reason `/secrets` guards its
listing: a map of who can reach a company and how is worth as much to an
attacker as the permissions themselves. Nothing under `/iam` is on the exemption
list and nothing ever will be.

| Route | Who |
|---|---|
| `GET /iam/people` | `admin` |
| `POST /iam/people` | `admin` |
| `GET /iam/people/{id}` | the person themselves, or `admin` |
| `PATCH /iam/people/{id}` | `admin` |
| `DELETE /iam/people/{id}` | `admin` |
| `POST /iam/invitations` | `admin` |
| `GET /iam/invitations[?all=true]` | `admin` |
| `DELETE /iam/invitations/{id}` | `admin` |
| `GET /iam/people/{id}/sessions` | the person themselves, or `admin` |
| `DELETE /iam/people/{id}/sessions` | the person themselves, or `admin` |
| `POST /iam/people/{id}/mfa/reset` | `admin` |
| `POST /iam/people/{id}/password-reset` | `admin`, for somebody holding no permission the caller does not |
| `GET /iam/credentials[?person=]` | the person themselves, or `admin` |
| `POST /iam/credentials[?person=]` | the person themselves, from their own session; `admin` for a **service account** only; never a request presenting a machine token |
| `DELETE /iam/credentials/{id}[?person=]` | the person themselves, or `admin`; revoking a password, a second factor or the recovery codes needs a person present, so a machine token revokes machine tokens only — `403 token_refused` with nothing written, on a node that had listed the credential before the write and on one that had not alike. Revoking a person's **last** authenticator app revokes their recovery codes with it, in the same record and each announced, because codes held alone are a second factor every sign-in would ask for with only those single-use answers |
| `POST /iam/invalidate-all` | `admin`, a person present and a recent proof — the record layer refuses it through a machine token too. Ends every session **and every machine token** |
| `GET /iam/seats[?unheld=true]` | **everybody** — each holder's name and kind to every reader, their person id, login and stage to an administrator and to the holder themselves, and the open invitations and `?unheld=true` to an administrator alone (`403` naming `["admin"]` to anybody else) |
| `GET /iam/check` | `admin` |
| `GET /iam/node-tokens` | `admin` |
| `GET /iam/audit` | `admin` |

**Every write here asks for a recent proof**, inside `step_up` (an hour by
default); see
[Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are).
Ending sessions asks two answers of its two arms: ending your own asks for none, because it is the first thing somebody
does on finding an intruder in their account — and it ends every machine token
they hold too — while an administrator ending somebody else's asks `step_up`,
since it signs a colleague out of everything.

**The object a route names is a person ID, never a login.** The identity
estate keys on an id precisely because a person changes their login, so a self
check against a mutable name would open somebody else's row the day they
swapped.

**Editing your own row is not a self gesture.** Changing your own permissions
is the escalation this estate exists to close, so `PATCH /iam/people/{id}` has
no self path at all — unlike the credential mint and the session end beside it,
which are the two gestures a person legitimately makes about themselves. The
record layer refuses it a second time: a caller may not confer a permission
they do not hold, on anybody, themselves included — on an edit, on a
`POST /iam/people` enrolment and on a `POST /iam/invitations` alike, each
answered `403` naming the permission. Judged through `iam.Expand`, so an
administrator may confer every app.

**An administrative record is never written through a machine token.** An
enrolment, an invitation, an edit of somebody's row, a suspension, a removal,
a reset link and `POST /iam/invalidate-all` are refused `403 token_refused`
when the request presented a `pat:` token, whatever the token carries — by the
authority table's row and again by the identity record itself, so the CLI and
every other path that publishes a record is held to it too.

**A listing says what still works.** A session's `live` and a credential's
`revoked` are the reading the request guard makes: a session or a machine token
ended by its person's revocation epoch — a password change, signing out
everywhere, an administrator ending their sessions, a suspension — or by
`POST /iam/invalidate-all` is listed ended, and a named sign-out of a session
listed live finds it live. A session keeps the `ended_reason` and `ended_at` of
whatever ended it — a revocation writes its own on every session it ends, and a
later one never rewrites them — while `POST /iam/invalidate-all`, which writes
nothing per session, lists its sessions ended "with every session in the
company" and no `ended_at`; neither counter writes a `revoked_at` on a token. A
session a revocation had already ended when its start landed is listed ended
"with every session they held", with no `ended_at`, and no later revocation
claims it.

#### Every write answers three ways

`applied` is `200` (`201` on the four routes that hand back what they created:
`POST /iam/people`, `POST /iam/invitations`, `POST /iam/credentials` and
`POST /iam/people/{id}/password-reset`) and means this node has the change, so the next read *here* sees it. `pending`
is `202` with the position, and means the record is durable and every data node
will apply it while this one has not yet — read at that position to see it.
`unknown` is `503` with a `Retry-After`, `"outcome": "unknown"` and the op
id, and the only safe retry is the **same** one: send it back as `Idempotency-Key`, because a fresh id
would defeat the ledger that makes the retry safe. Every body carries its
`outcome` and `op_id`.

**Nothing is built on, handed out or announced for an `unknown`.** A removal,
a revocation, a second-factor reset or a credential withdrawn announces its
event only once the record is durable; an invitation whose outcome is unknown
hands out no link, a person's create no first password link and a mint no
token, because each would be a value that answers `410` or `401` the first
time somebody uses it. A write this
node refused to decide at all — it is behind, below the trim floor, or holding
a record it cannot decode — is `503 unavailable` with the op id, and a
`Retry-After` only where waiting clears it: a node that is behind or below the
floor carries the identity estate's two seconds, while one that is evicted or
holds a record it cannot decode, a log at its byte ceiling, a record larger
than the broker takes and a refusal the broker named carry **none** — the same
request is refused the same however often it is sent, as on `/worklet`. The
sign-in surface, `/auth`, follows the same rule on every write it makes. On
both, a write whose snapshot kept moving under it until the framework gave up
is `409 stale`, as on `/worklet`: nothing about the request or the node was
wrong, so it carries no `Retry-After`, and the same request sent again decides
afresh and lands. An edit — of a person here, or of your own account on
`/auth` (a password change, a second factor, a set of recovery codes, a
sign-out) — is told in the words every surface tells a lost race in, which
ask you to look at it again first. A gesture on `/auth` that edits nothing
you can look at — a sign-in, a step-up, a token exchange, an invitation's
redemption, a reset link's spend — is told only to try again.

**A gesture that is several records answers its weakest.** An edit that moves a
login and a stage, and a reset followed by its revocation, are each a sequence,
and a step answered `unknown` ends the sequence there — nothing after it is
published over a guess — while a step still `pending` here makes the whole
answer `202`. A create is one record, its seat and a person's first password
link included.

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
names what its first attempt may have created: a person that attempt created is
answered from the ledger as applied, and an invitation that landed is answered
with its own link and the deadline it was issued with. The invitation's id and the secret its link carries
are both derived under the company's own key — the id from the operation key and
the secret from the id — so the retry hands back the very link the first
attempt issued, and an operation key is never a way to compute one. A
person's **first password link** is derived the same way, from the person
under the company's key, so the retry of their create hands back the link the
first attempt issued with the expiry it stored — or, where that link no longer
opens (spent, revoked or aged out), no `url` and a `detail` saying to issue a
[password reset link](#post-iampeopleidpassword-reset-issues-a-one-time-link)
instead. A retry that reaches a node which has not applied the create yet finds
the person, or the invitation, it made absent there, and answers `503
unavailable` with the write's own `outcome` (`pending` or `applied`, never
`unknown`), the `op_id`, a person's `id`, a `Retry-After` and a `detail` saying
what landed: send the same request again, under the same key, to be handed the
link. A new key is a new create, refused by the address and the seat the first
one now holds. Because the key names what a create creates, it is a **uuid7**, as
every `op_id` is, and any other value is `400 op_id_invalid` naming the header.
The same key with a *different* body — another address, another login, other
grants — is `409 invalid_input` naming `Idempotency-Key` as its `field` and
carrying the `op_id`, as every surface that takes a key answers one, and so is
the key of an invitation since redeemed or aged out: a retry is the same
request, and a new person or a new link is a new key.

An address, a login or a seat somebody else holds is `409 invalid` **naming who
holds it** — `field` says which value, `held_by` is the holder's id, and the
`detail` names them by their login. An **open invitation** holds its address
and its seat as surely as a person does, so a value one holds is the same
`409 invalid` naming the `invitation` instead — the id
`DELETE /iam/invitations/{id}` takes — its `detail` saying to cancel the
invitation first or let it lapse. A seat
is named in the `detail` — a handle is no secret — and an address never is,
since the estate holds it only as a blind.

**A person holds a human seat for as long as they are here.** So a person's
create or an invitation that names no seat, or an edit that would clear a
person's seat, is `400 seat_required` with `field: seat` — its own code,
because its remedy is always the same field. A seat the company this node runs does not
hold, or holds as an agent's, is `400 invalid_body` (every binding, a service
account's too, names a **human** seat), and a seat named on a node that runs no
company yet is `409 no_active_revision` with a `hint`, as `GET /iam/seats` is
there: no wait supplies a company, so it is not the `503` a retry clears.

A refusal on authority is `403` and will never land however often it is
retried, and its code says what would change it. One a permission would lift —
a record only `admin` writes, a permission the caller cannot confer because
they do not hold it — is `403 unauthorized` with `reason: no_grant` and the
`grants` that would lift it, as the authority table answers a missing
permission. One the directory's own rule makes whatever the caller holds — a
person's token asked for by anybody but that person, a permission the owner
does not hold, an owner who may not act, a permission this build does not know
on the person's row — is `403 forbidden`, carrying the rule's sentence in
`detail` and no `reason` or `grants`, because no permission would change it. A
request through a machine token is `403 token_refused`.
A login outside its holder's kind is `400`: a person's is dotted (`jane.doe`)
and a machine's is coloned (`ci:release`, or `token:<id>` to bind a Tier A
token), at most 64 characters either way, checked on a create and on a rename
alike. **Every principal holds a login** — it is what a person signs in with,
the name a lead finds them by and the `by jane.doe` in every trail reason, and
the only thing that finds a service account — so a service account's create
without one is `400`, and a person's create without one takes the login
proposed from their address, as an invitation's form does
(`jane.doe@example.com` → `jane.doe`). A `kind` other than `person` or
`machine` is `400` too, since a seat belongs to the company document and the
engine is the node. A write's `reason` is optional, and without one the
record says what was done and by whose login, in words — `suspended by
jane.doe`, `every session ended by jane.doe`, `invited by jane.doe` — which is
the trail's detail and, for a suspension or ending somebody's sessions, what
each session it ends is listed as ended by. A value outside a bound is `400`
too: a `reason` longer than 256
bytes (it is rendered into the authentication trail beside the op, so it
names which cause fired rather than narrating), and a permission this build
does not know, **added** — by a create, an invitation, `add_grants` or a
whole-set `grants` naming one the row does not already carry — which is
refused naming the five; one already on the row is kept, and
`remove_grants` may name it. An enrolment is one record, so a
refused create leaves nothing behind and the corrected retry lands.

#### `POST /iam/people` creates a person on their seat, or a service account

```json
{
  "id": "0192f00d-…",
  "kind": "person",
  "login": "sam.lee",
  "seat": "qa-lead",
  "credential": "0192f00e-…",
  "url": "https://crewlet.example.com/dashboard#/reset/<id>.<secret>",
  "expires_at": "2026-06-15T09:12:00Z",
  "detail": "This link is shown only once. Send it to them yourself (Crewlet doesn't send email). It expires at the time above.",
  "outcome": "applied",
  "op_id": "0192f00c-…",
  "position": "CREWLET_IAM_LOG@0:1842"
}
```

The body is `{kind, email, seat, login, name, grants, reason}`, and `kind`
defaults to `person`. `email`, `seat` and `login` are trimmed before anything
judges them. **`grants` is required**, for a person and a service account
alike (any of `admin`, `worklet`, `doclet`, `artifacts` and `chatlet`, and `[]` for the
[overview](../concepts/identity-and-access.md#everybodys-overview) alone), and
an omitted field is `400 invalid_body` naming `grants`, decided before any key
is read or link minted: a person enrolled with every app and one enrolled with
none are both legitimate, so a body that names neither is one whose author did
not decide. A name this build does not know is the same `400`, naming the
three. The whole create is **one record** on the directory, which
decides the address, the login and the seat together against every row — so a
create refused for any of them has created nobody and issued nothing.

**A person is created onto their seat.** A person holds a human seat for as
long as they are here, so `seat` is **required** — a human seat that no person
and no open invitation holds, by its handle (`GET /iam/seats?unheld=true` lists
them). One left out is `400 seat_required` naming `field: seat`, answered
before any key is read or anything minted. `email` is required too: it is what
they sign in with. `login` may be left out, and the create then takes the one
proposed from the address, as an invitation's form does (`jane.doe@example.com`
→ `jane.doe`, `jane@example.com` → `jane.example`), and answers it as `login`;
an address with nothing to propose from is `400 invalid_body` naming
`field: login`. A created person is `active` from the moment the create lands:
an administrator creating them **is** their enrolment, and what they lack is a
password.

**And handed their first password link.** The same record issues it, and the
`201` carries it once — `credential`, `url` and `expires_at` — with a `detail`
saying so. The `url` is the dashboard's reset screen,
`<api.external_url>/dashboard#/reset/<id>.<secret>` with the `credential` as
its id, and its view (`GET /auth/reset/{id}`) answers `first: true` so it asks the person to choose
their password. Spent through `POST /auth/reset/{id}`, it sets that password
**once** and signs nobody in: they sign in afterwards, and enrol a second
factor there where `api.auth.totp` requires one. It is good for **a week** — an
invitation's week, because it is the same person's position — and it is a
`reset` credential like any other: listed among their credentials, replaced by
a [password reset link](#post-iampeopleidpassword-reset-issues-a-one-time-link)
issued later, and ended by whatever ends one (an edit that adds a permission
to them, a suspension, their sessions ended, `POST /iam/invalidate-all`). Send it to them
yourself: this engine sends no mail. Its issue is one
`iam_password_reset_issued` event with `first: true` — for whom, by whom,
until when, never the link — published by the call whose record carried it and
by no retry. So a person's create needs `api.external_url`, and a node with
none answers `500 no_external_url` with nothing created; a service account's
does not.

**A service account** (`kind: machine`) needs a `login` — coloned, `svc:ci`,
or `token:<id>` to bind a Tier A token — because it has no address to be found
or proposed by, and `email` is optional. Its `seat` is optional: one it is
given binds it to act as that seat and must be a human seat that no person and
no open invitation holds, as a person's must. It gets no link — it has no
password — and proves itself with a token minted on it,
`POST /iam/credentials?person=<id>`.

**A retry hands back the same link.** The person is derived from the
operation's key and the link from the person under the company's key, so the
retry an `unknown` answer asks for — the same body under the `op_id` it
handed back — answers the link the first attempt issued, with the expiry that
attempt stored. One that no longer opens by then is not handed back: the answer
carries no `url` and a `detail` saying to issue a password reset link. An
`unknown` carries the person's `id` and the `op_id`, and never a link.

| Answer | When |
|---|---|
| `201` | Created, a person and a service account alike: `{id, kind, login, seat?, credential?, url?, expires_at?, detail?}` beside the `outcome`, `op_id` and `position` every write carries — the link's three fields for a person only. `202` is the same answer for a record this node has not applied yet |
| `400 seat_required` | A person's create that names no seat |
| `400 invalid_body` | No `grants` field, or a name in it this build does not know (`field: grants`); a seat the company this node runs does not hold, or holds as an agent's; a person with no address; a service account with no login; a login outside its kind's grammar, or none to propose from the address (`field: login`); a `kind` other than `person` or `machine`; a name or a reason past its cap |
| `400 op_id_invalid` | An `Idempotency-Key` that is not a uuid7 |
| `403 unauthorized` | A permission the caller does not hold |
| `403 token_refused` | The request presented a machine token: an enrolment is an administrative record, refused through a token whatever it carries |
| `409 invalid` | An address, a login or a seat somebody holds (`held_by`), or an open invitation holds (`invitation`) |
| `409 invalid_input` | The key of an earlier create sent with a different body (`field: Idempotency-Key`) |
| `409 no_active_revision` | A node that runs no company yet: it holds no human seat to create anybody on, and the `hint` says to import or create a company that declares one |
| `500 no_external_url` | A person's create on a node with no `api.external_url` |
| `503 unavailable` | A create nothing can confirm (`"outcome": "unknown"`, with `id` and `op_id`), or a node that refused to decide it — send the same request again under that `op_id` |

#### `POST /iam/credentials` mints a machine token

The owner is the person the **route** names — `?person=`, or the caller — and
never a body field, because that is the value the authority table decided on.
**A person's token is minted by that person alone**: it acts as them and its
value is shown to whoever minted it, so `admin` mints for service
accounts and never on a person's account. A person mints their own with no
`?person=` from their own session — `crewlet iam token -login` makes exactly
that request, signing in for it and signing out after.
The body is `{"label", "expires_in_days", "grants"}`, every field optional:
`grants` omitted, the token carries every permission its owner holds that
whoever mints it may confer — `admin` included where the minting session holds
it, and the owner's apps alone on a node whose `api.auth.admin` is `refuse`,
where no session carries admin; `[]` mints one that reads the overview alone;
and the token lasts 90 days. A token may carry `admin` — what keeps it off the
gestures that need a person present is the authority table's presence rows and
the identity record's own refusal of an administrative record written through a
token, not a permission it cannot carry. The answer is `201` with
`{"id", "person", "token", "grants", "expires_at", "position"}`.

| Answer | When |
|---|---|
| `400 bad_params` | No owner: a Tier A token owns no machine tokens, so it names the service account with `?person=` |
| `400 invalid_body` | An expiry in the past or more than 365 days away, a label past 128 bytes, an owner already holding 64 live credentials — a revoked or expired one gives up its place to the new token, the earliest to lapse first, so revoking a token nothing uses makes room at once |
| `400 invalid_body` (`field: grants`) | A permission this build does not know, named beside the five |
| `403 token_refused` | The request presented a machine token |
| `403 unauthorized` | A service account's token asked for without `admin`; a permission the caller does not hold — `grants` names it |
| `403 forbidden` | A **person's** token asked for by anybody but that person; a permission the owner does not hold; an owner who may not act; the row that binds a Tier A token (`token:<id>`) — `detail` says which |
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
a person's seat, or a service account's where it is bound to one — carrying the grants it was minted with
that the owner **still** holds, cut to the node's ceiling. It answers `401`
where the node knows it is no good and `503 identity_unavailable` where the node
cannot tell, including a node that has not yet applied the mint. See [Machine
tokens](../concepts/identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts).

#### An edit moves a login and a seat in one record, and that record goes first

A person's login and seat are the **directory's**, decided in one record
against every other row; their stage and their document (name, grants) are
their own subject's. So `PATCH /iam/people/{id}` is up to three records, and
the login-and-seat record goes **first**: it is the one a value somebody else
holds can refuse — a `login` somebody holds, or a `seat` somebody is bound to
or an open invitation holds, is `409` naming which, and a `login` its holder's
kind's grammar refuses is `400` — and refused first, it is refused with nothing
landed. The record states the login and the seat the person holds from now on,
so the old ones are free the moment it lands. `login` and `seat` are trimmed
before anything judges them.

**A person's seat is moved, never cleared.** `seat` names the human seat to
move somebody to. A person holds one for as long as they are here, so
`seat: ""` about a person is `400 seat_required` — the identity record's own
refusal, decided on the row's kind in that record's snapshot, so nothing has
landed — and the way to free a person's seat is to move them to another or
remove them. A **service account** is unbound with `seat: ""`.

**Grants are the whole set or a change to it, never both.** `grants` replaces
what the person holds — what `crewlet iam grant` sends, stating it outright —
among `admin`, `worklet`, `doclet`, `artifacts` and `chatlet`; `admin` includes every app, and the
dashboard and the CLI store every app beside it, so taking admin away later
leaves the apps.
`add_grants` and `remove_grants` are applied to what they hold **when the
record is decided**, which is what an editor working from an earlier read
means: sent whole, the grants that editor never touched are the ones the read
held, so a grant another administrator took away meanwhile would come back and
one they gave would go — and nothing would refuse it, since only an addition
needs the caller to hold the permission. The dashboard's edit sends the
change.

**Everything the surface can judge alone is refused before the first record**:
a `login` of `""` (a login is never cleared, only changed), a `stage` this
build cannot name, a `reason` past 256 bytes, `grants` beside `add_grants` or
`remove_grants`, a name in both, a name this build does not know **added** —
in `add_grants`, or in a whole `grants` naming one the row does not already
carry (`400`, naming the five); `remove_grants` may name one, and one already
on the row is kept and ignored — and permissions the caller may not confer
(`403`). What only a later record can decide — a grant the caller's own
row stopped letting them confer a moment ago — is refused by that record, and
the steps before it have landed. So a refusal met after the first record
carries `landed`, the fields whose change was made (`identity`, `stage`), and a
`hint`; an `unknown` met partway carries `landed` too.

A `seat` is named by its **handle**, which is immutable, and the binding
records it; naming the login and the seat a person already holds is a no-op
(`200`, no record). A seat the running company does not hold, and one it holds
as an agent's, are `400 invalid_body` (a value that was typed); a node that runs
no company yet answers `409 no_active_revision`, because it holds no human seat
to move anybody to and no wait supplies one. A node holding an identity record it
cannot decode refuses a login or a seat change `503` with no `Retry-After`: the
record may hold the value, so another node decides it.

#### Values that are shown once

`POST /iam/invitations` answers the invitation URL, `POST /iam/people` a
person's first password link, `POST /iam/people/{id}/password-reset` a reset
link and `POST /iam/credentials` the token. None is stored and none can be read
back: what the estate holds is the invitation's id and a SHA-256 of the secret
its link carries, the same for a first password link and a reset link, and a
SHA-256 of the token. An invitation an administrator lost is cancelled and
issued again rather than recovered; a first password link lost is handed back
only by the retry of its own create under the same key, and is otherwise
replaced by a reset link; and a reset link is simply issued again.

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

`POST /iam/invitations` takes `{email, seat, grants, reason}` and answers
`201` with `{id, url, expires_at, seat, detail}` beside the `outcome`, `op_id`
and `position`. **`grants` is required**, as on `POST /iam/people`: what the
link confers (any of `admin`, `worklet`, `doclet`, `artifacts` and `chatlet`, `[]` for the
overview alone), and an omitted field or a name this build does not know is
`400 invalid_body` naming `grants`, decided before any key is read or link
minted. **`seat` is required**: every invitation names the human seat
its invitee will hold, by its handle, and one left out is `400 seat_required`,
answered before any key is read or anything minted. It must be a **human**
seat that no person and no open invitation holds: a seat the running company
does not hold and an agent's seat are `400 invalid_body`, one a person holds is
`409 invalid` naming them, one another open invitation holds is `409 invalid`
naming that `invitation`, and a node that runs no company yet answers
`409 no_active_revision` — there is no human seat to invite anybody into until
it does. The link is good for **a week**.

**An open invitation holds its seat** exactly as it holds its address, from its
issue until it is redeemed, cancelled or lapses: a create, a move or another
invitation naming the seat is refused naming it, `GET /iam/seats` lists it
against the seat, and a `/config` write that would take the seat out of the
company is [`409 seat_held`](#what-a-write-answers) naming it. Redeeming it
creates the person bound to that seat in one record; a seat the company has
removed, or made an agent's, since the issue — which a `/config` write refuses while the invitation holds
it, so only an offline `crewlet config import` or a write racing the issue
leaves — is the link's `410`, and the remedy is a new invitation.

`POST /iam/invitations` on a node with no `api.external_url` is `500
no_external_url`: there is no address a link could point at, and only the
node's own configuration file can supply one.

#### `GET /iam/invitations` lists them, and never a link

```json
{
  "invitations": [
    {"id": "0192f00d-…", "email": "sam@example.com", "seat": "qa-lead",
     "grants": ["worklet", "doclet"], "invited_by": "people-lead",
     "created_at": "2026-06-08T09:12:00Z", "expires_at": "2026-06-15T09:12:00Z",
     "state": "open"}
  ],
  "next": "",
  "position": "CREWLET_IAM_LOG@0:1840"
}
```

The invitations nobody has redeemed and that are still good, paged on their id
as `GET /iam/people` pages (`?after=`, `?limit=`). `?all=true` adds the
expired and the redeemed ones this estate still holds — until the sweep
collects them — and `state` says which: `open`, `expired`, or `redeemed`, with
`redeemed_at` and the `person` it created. Every row names the `seat` it binds
and holds while it is open.
`invited_by` names its author as every record does — a person by their seat's
handle, a Tier A token or a service account by its login. The address is opened on this
node's own keyring and a row it cannot open reads `sealed`, as a person's does;
a redeemed row whose person has since been **removed** carries no `email` and is
not `sealed`, because a removal erases every value of theirs other rows hold.
**No row carries the link**, its secret or what the estate keeps of it: the
link is shown once, and an inviter who lost it cancels and issues another.

#### `DELETE /iam/invitations/{id}` withdraws one nobody redeemed

One directory record, `?reason=` recorded on it. Its apply deletes the
invitation's row, so the link answers `410` exactly as one nobody issued, and
the address and the seat it held are free at once — the address for a new
invitation or a create, the seat for a create, another invitation or a move,
and for a `/config` write that removes it. It reads
an `Idempotency-Key` like every keyed write here. An id this estate does not
hold is `404` — never issued, cancelled already, or collected by the sweep —
and a **redeemed** invitation is `409 stale` naming the `person` it created:
the redemption overtook the reading the cancel was made from, the link is
spent, and what undoes it is removing that person. Each cancellation
is an `iam_invitation_cancelled` event, by the invitation's id and never its
address.

#### `POST /iam/people/{id}/password-reset` issues a one-time link

```json
{
  "id": "018f3a9c-…",
  "credential": "0192f00d-…",
  "url": "https://crewlet.example.com/dashboard#/reset/<id>.<secret>",
  "expires_at": "2026-06-09T09:12:00Z",
  "outcome": "applied",
  "position": "CREWLET_IAM_LOG@0:1841"
}
```

`201` with a link that sets the person a new password **once**: the
dashboard's reset screen, the credential's id and a secret in the fragment, as
an invitation's link is built — and on a node with no `api.external_url` it is
the same `500 no_external_url`. The link is a `reset` credential on the person,
stored as the SHA-256 of its secret and good for **24 hours**: it travels out
of band to somebody locked out today. It is listed among the person's
credentials (`GET /iam/credentials?person=`), revoked like any of them, and
issuing another revokes the one before it — a person holds at most one, the
[first password link](#post-iampeople-creates-a-person-on-their-seat-or-a-service-account)
their create issued included, so a reset link is also how somebody whose first
link lapsed or went astray gets a new one; their reset screen then says it
sets their first password. Once
spent it is listed `revoked` and `spent: true`, since setting the password
revokes every link the person held, the one they used included. It also ends
with the person's sessions and tokens: stamped with their revocation epoch and
the company's session generation, it is refused — and listed `revoked` — once
a suspension, a second factor's reset (`POST /iam/people/{id}/mfa/reset`),
signing them out everywhere, ending their sessions or `POST
/iam/invalidate-all` moves either, and a reactivation does not bring it back —
so a link needed beside any of those is issued after it, or it is `410
reset_spent` by the time it is opened. A
machine is `409` (it has no password; mint it a token), and so is a person
`suspended` or `retired`, naming the `stage`, because a link would hand back an
account somebody stopped: reactivate them first. A person holding a
permission the caller does not is `403 unauthorized` naming it, judged in the
record's own snapshot: whoever issues the link is shown it and could spend it
themselves, so a link is held to the rule an invitation and an edit are —
nobody hands out what they do not hold. An administrator holds every app, so
what reaches that refusal today is a name this build does not know, kept on
the row — and since no permission would make it one, that is `403 forbidden`,
naming the name in `detail`.

**Shown once, and no key is read.** Like a token's mint, a replay of the issue
could not hand back a secret its first attempt never showed, so the route
ignores `Idempotency-Key`; an issue whose outcome is unknown is `503` naming
its `op_id` and carrying no link, and the remedy is to issue again, which
revokes the one that may have landed. Each issue is an
`iam_password_reset_issued` event — for whom, by whom, until when, never the
link. **Spending it** is `POST /auth/reset/{id}`, among the `/auth` routes
above: it sets the password, ends every session and machine token the person
held, and signs nobody in.

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

#### `GET /iam/seats` lists the human seats and who holds each

```json
{
  "seats": [
    {"handle": "founder", "name": "Founder",
     "holder": {"person": "018f3a9c-…", "name": "Jane Doe", "kind": "person",
                "login": "jane.doe", "stage": "active"}},
    {"handle": "qa-lead", "name": "QA Lead", "unit": "quality",
     "invitation": {"id": "0192f00d-…", "email": "sam@example.com",
                    "invited_by": "founder",
                    "expires_at": "2026-06-15T09:12:00Z"}},
    {"handle": "designer", "name": "Designer", "unit": "product"}
  ]
}
```

Every human seat of the company this node runs, in the order the company
declares them, with what holds each. **Everybody reads it** — it is the org
chart's people, named, and the org chart is [everybody's
overview](../concepts/identity-and-access.md#everybodys-overview) — and what
each reader sees is masked per row. The example above is an administrator's.

- **`holder`** is whoever the directory binds to the seat **at any stage
  short of removal** — a suspended or retired person still holds their seat —
  with `name`, their own name opened on this node's keyring (or `sealed: true`
  where it does not open), and `kind` saying which remedy freeing it takes: a
  `person` is moved to another human seat or removed, a `machine` (a service
  account, or a Tier A token's `token:<id>` row) is unbound. `name` and `kind`
  go to every reader; `person`, `login` and `stage` only to an administrator
  and to the holder themselves.
- **`invitation`** is the **open** invitation that names the seat — not
  redeemed, not past its expiry at this node's clock — which holds the seat
  until it is redeemed, cancelled or lapses: `id` (what
  `DELETE /iam/invitations/{id}` cancels), the address it was sent to, opened on
  this node's keyring or `sealed` where it cannot be, `invited_by` and
  `expires_at`. **Never a link**, which was shown once by the issue. **Only an
  administrator** is sent it; to anybody else an invited seat lists as
  vacant.
- Neither, for a **vacant** seat — the one a person can be invited into or
  created on (`POST /iam/invitations`, `POST /iam/people`), which is what the
  seat's **Invite** and **Add person** in **Company › Org chart** do.

`?unheld=true` keeps only the vacant seats: one with a holder **or** an open
invitation is left out, because each holds it against a create, an invitation
and a move alike. It is an administrator's helper, for the gestures that need
one — anybody else is refused `403 unauthorized` naming `["admin"]`. Where two open invitations name one seat — a residue only a
build before invitations held their seats, or a restore, leaves — the newest is
listed; a holder and an invitation on one seat are both listed. An `unheld`
that is not a boolean is `400 invalid_query`.

**A listing, not a report**: what is wrong with a binding is `/iam/check`'s.
A node that runs no company yet answers `409 no_active_revision` with a `hint`:
it holds no human seat to list or to put anybody on, so the company declaring
one comes first. One that cannot read the directory answers `503` — never a
list of vacancies built from what it could not read. So does one holding two
rows bound to one seat, which only a record it retained or a restore leaves
behind: it cannot say which of them holds the seat, and another node can.

#### `GET /iam/check` walks the whole directory

```json
{
  "findings": [
    {"kind": "binding_dangling", "person": "018f3a9c-…", "login": "jane.doe",
     "seat": "platform-lead",
     "detail": "seat \"platform-lead\" isn't in the org chart anymore. Move them to another human seat, or remove them"}
  ],
  "position": "CREWLET_IAM_LOG@0:1840",
  "people_with_admin": 2,
  "bindings_unchecked": 0
}
```

`kind` is one of four, and the report is **sorted in this order** — stably,
so within a kind the directory's own order holds and two nodes over the same
rows print the same report:

1. `no_admin_holder` — nobody left who can administer the company except
   through a Tier A token: no active **person** holding an **enrolled**
   credential (a password, a second factor, a token — a way in they have used)
   holds `admin`. A first password link nobody has spent yet does not count,
   because a company whose only administrator has not chosen a password is one
   nobody can administer yet — and neither does a service account holding
   `admin`, because a machine token never writes an administrative identity
   record, so a company whose only administrator is one can invite, grant and
   revoke nobody. `people_with_admin` is that count.
2. `person_without_credential` — somebody active with no live way in at all: a
   person whose first password link lapsed unspent or was revoked, or whose
   every credential has been; a service account holding no live token. A live
   link is a way in, so a person created a minute ago is not reported until
   theirs lapses. An invitation creates nobody until it is redeemed, so it is
   never the cause.
3. `binding_dangling` — somebody bound to a seat the company no longer holds as
   a human seat.
4. `admin_clamped_by_node` — a row holding `admin` on a node whose
   `api.auth.admin` is `refuse`, which clamps it away here: that person keeps
   every app on this node and administers nothing through it.

Each finding's `detail` says what to do, and the table in
[`crewlet iam check`](cli.md#crewlet-iam-check) says the same per kind.

`binding_dangling` is decided by the **request path's own seat table**, so it
names exactly the people a request would refuse or hold off for want of their
seat: a seat the company this node runs no longer holds, or holds as an agent
seat. The `detail` says which, and the remedy for the holder's kind: a person
is moved to another human seat or removed, a service account unbound or moved.
This report is the one place a dangling binding
is said — no [alarm](alarms.md) raises it, because it is a fact about the
company's content rather than about a node — and a Tier A token's row is
reported here like anybody's, under its `token:<id>` login.

`bindings_unchecked` counts the bound people whose seat this node **could not
judge** — it runs no company yet. They are neither reported as dangling nor
left out silently, so a report answered before the node's first apply does not
read as a clean directory; ask a node that runs the company.

#### `GET /iam/node-tokens` joins this node's Tier A labels to the directory

A Tier A token acts under its own login, `token:<id>`, and a directory row
holding that login is what binds it to a seat — a service account's row,
created bound to a human seat in one request (`POST /iam/people` with
`kind: machine`, `login: token:<id>` and `seat`). The two are written in two
places by two people — the label in a node's Tier A file, the row through this
surface — so a label mistyped on either names a login nobody holds, and the
token goes on working as itself, unbound, while its operator believes it acts
as a seat. `/iam/check` names a binding whose seat is gone; this names a token
whose row was never there. It is a fact about **one node's** configuration, so
ask each node whose Tier A you want checked.

```json
{
  "tokens": [
    {"id": "ci", "login": "token:ci", "row": "none", "company_writer": true},
    {"id": "ops", "login": "token:ops", "row": "held", "person": "018f3a9c-…",
     "stage": "active", "seat": "platform-lead"}
  ],
  "company_writers": ["ci"]
}
```

**Labels, never values**: `id` is the token's label in this node's Tier A.
`row` is `none` when nobody holds the login (the token acts as itself) and
`held` for a row — whose `person`, `stage` and `seat` follow, the seat named
by its handle and absent when the row binds the token to none. **What the row
holds, never a verdict on it**: whether that seat is still one the company
holds is [`/iam/check`](#get-iamcheck-walks-the-whole-directory)'s
`binding_dangling` finding about the same row. A directory read that fails
answers `503` with a `Retry-After`, never a list missing a row. It backs
**Settings › API tokens**.

**`company_writers` is `api.auth.company_writers`**, sorted: the token ids
that alone may change the company document when another system [manages
it](../concepts/configuration.md#managed-configuration), and `[]` when nothing
does — every credential may, as far as that rule goes. `company_writer: true`
marks each of this node's tokens the list names, and the key is absent on every
other token (and on every token when the document is not managed), so the
screen names the writers beside the tokens and an operator reading the deployment's
`api.auth` sees whether the document is managed, and by which credential.

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
gesture from their own. A gesture a person makes through the sign-in surface
— signing in or out, spending a second factor, redeeming an invitation,
setting a password from a reset link, enrolling their own authenticator — is
theirs: the node writes it down under its own authority, and the entry names
the person as `actor` and the session it opened or closed as `operator_id`
(their login, where it came through no session yet). An entry the engine
decided for itself — a sweep, a re-seal, a password re-hashed at the current
cost, a company-wide invalidation — names the node, with `actor_kind`
`engine`, and no `operator_id`: the node acts on nobody's credential.

An entry about somebody names them by `person`, their id, and by `login`, the
login their row holds when the trail is read — absent once they are removed,
since the removal erases what identified them and the trail keeps the id.

### `/config/*` — live config management (auth-gated)

Every `/config/*` route takes `admin`, reads and writes alike, whatever the credential — a Tier A token, a session, or the development principal. A caller without it is refused `403 unauthorized` naming `admin` (see [Which permission a route needs](#which-permission-a-route-needs)). The routes that can change the org chart — `PUT` and `PATCH /config`, a revert, and the seat and unit entity routes — also admit a unit's **lead** for what is inside the units they lead, read and judged change by change (see [A lead edits their own team](#a-lead-edits-their-own-team)). A write also asks for a proof of identity inside `step_up`, and a session whose proof is older is refused `403 step_up_required` (see [Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are)). See the [Configuration concept doc](../concepts/configuration.md#auth) for the full auth model.

**A managed document refuses every other writer.** When Tier A lists
`api.auth.company_writers`, only those token ids may change the document:
`PUT`, `PATCH`, every per-entity `PUT`, a revert and the `?dry_run=true` check
of each answer any other credential `403 config_managed`, before anything is
read or stored, and `/setup`'s connect, disconnect and GitHub App routes
answer the same refusal before they touch the secret store or a vendor. Reads
and `POST /config/reload` stay open to whoever the authority table admits:
`admin`, for a read and a reload alike. The body names who
manages the document and what to do:

```json
{
  "error": "config_managed",
  "managed_by": ["gitops"],
  "detail": "The company config is managed by gitops. The credential \"founder\" can read it but not change it.",
  "hint": "Change it in the system that manages it (the token \"gitops\"). Edits here get overwritten. To rotate a leaked credential, write it to /secrets and POST /config/reload. To manage it here, remove `api.auth.company_writers` from every node's config file and restart."
}
```

`403` rather than `401`, because the credential was accepted and a different
one is the managing system's to hold, not the caller's to retry with. See
[Managed configuration](../concepts/configuration.md#managed-configuration).

**The active revision is the fleet's, on every node.** Every answer here —
`GET /config` and its `ETag`, the revision list's `is_active`, a diff against
`active`, the references, the charter and the entity reads — and the base every
write is built on is the revision the fleet's activation pointer names. That is
what this section calls the **active revision**, and every node answers it
alike, whatever that node runs: one still applying it, or one that refused it
and runs the revision before, answers the fleet's revision all the same. So the
read after a write answers the write on every node, and a write sent to any node
is built on what the fleet runs. What each node runs is a separate fact, which
[`GET /fleet`](#get-fleet) reports per node as `config_serving_revision_id`. A
node that has not reconciled since the pointer moved holds no copy of the
revision yet and answers it from the body the pointer carries: no read writes
anything, and the node's next reconcile keeps the copy. With no pointer at all —
on a new fleet, or one whose pointer was lost — the active revision is the
company this node knows (see [Concurrent writes](#concurrent-writes)).

**A coordination store that cannot be read is `503 unavailable`.** Every write,
and every read but a diff between two revisions this node holds, both named by
id, reads the pointer, so while it cannot be read the request answers `503
unavailable` with `Retry-After: 2` and a `reason` saying the fleet's activation
could not be read — never an answer from this node's own store, whose `ETag`
every write would refuse, and never "nothing is configured", which would send a
create-only write to replace the running company. A coordination store electing
a leader is out for a few seconds; send the request again. That holds for a
`503` **without** an `outcome` field only: a write that had already stored its
revision when the store stopped answering is `503` with `"outcome": "unknown"`,
which is not sent again — see [Status codes](#status-codes).

**Read-only:**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config` | The active revision — the fleet's — redacted (full JSON; `?format=yaml` for YAML) |
| `GET` | `/config/revisions` | Paginated history of the revisions this node holds (newest first), metadata only; the fleet's revision is marked `is_active` |
| `GET` | `/config/revisions/{id}` | Single revision including its payload |
| `GET` | `/config/revisions/{id}/diff?against=<uuid\|active>` | Structural diff |
| `GET` | `/config/references` | Every `${VAR}` the active document names, and every variable it reads without naming it, each with the config path of the field that reads it, plus the `revision` they were read from |
| `GET` | `/config/charter` | The company's mission, vision and policies, with an `ETag` of their content. See [The charter](#the-charter-mission-vision-and-policies) |

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
reports, and a name with several readers appears once per reader. It also
lists every variable the document reads **without naming it**: a
`providers.llm` entry whose credential field is empty signs in from a
conventional variable instead — its harness's token variable under
`subscription` (`CLAUDE_CODE_OAUTH_TOKEN`, which `crewlet llm login
-capture-token` stores), its key variable under `api-key`
(`ANTHROPIC_API_KEY`; never a variable other vendors' keys share, such as Qwen
Code's `OPENAI_API_KEY`, which such an entry names in `auth.api_key`), and `CREWLET_LLM_CLI_<KEY>_CREDENTIALS` for the bundle
`crewlet llm export -secret-store` stores — and `providers.embeddings` with no
`api_key` reads `OPENAI_API_KEY`. Each is reported at the empty field
(`providers.llm.default.auth.token`), which is where a reference would be
written instead; removing that row signs the entry out as surely as removing
one it names. It carries the document's own `ETag`, because the index changes
exactly when the revision does.

**Full-document write:**

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/config` | Replace the active revision. The body is JSON or YAML, read the same way whatever `Content-Type` says. Requires a revision summary: an `X-Summary` header, **or** a top-level `_summary` key in the body. Conditional via `If-Match` / `If-None-Match`, see [below](#conditional-requests). `?dry_run=true` checks it and stores nothing, see [Dry runs](#dry-runs). `?import=true` writes it as an import of a restored file, see [The charter](#the-charter-mission-vision-and-policies) |
| `OPTIONS` | `/config` | `204` with `Allow` and `Accept-Patch: application/merge-patch+json` |
| `PATCH` | `/config` | Merge one or more sections into the active revision, see [below](#patch-config--the-narrower-write). `?dry_run=true` checks it and stores nothing |
| `POST` | `/config/reload` | Re-publish the fleet's document unchanged, so every node re-applies and re-reads the secret store. See [below](#post-configreload-after-a-secret-changes) |
| `POST` | `/config/revisions/{id}/revert` | Create a new active revision whose payload equals revision `{id}` |
| `PUT` | `/config/charter` | Replace the mission, vision and policies, as a new revision of the whole document. Requires `If-Match`. See [The charter](#the-charter-mission-vision-and-policies) |

#### `PATCH /config` — the narrower write

A [JSON Merge Patch (RFC 7396)](https://www.rfc-editor.org/rfc/rfc7396): send only the sections you are changing, in the shape the document already has.

The registered media type is `application/merge-patch+json`; plain `application/json` and an absent `Content-Type` are accepted too, since every example here sends one of those. **Any other patch format is `415`** with an `Accept-Patch` header naming what would have worked — notably `application/json-patch+json`, an [RFC 6902](https://www.rfc-editor.org/rfc/rfc6902) list of operations, which is a different format this surface does not serve. Editing one list member is what the [per-entity routes](#per-entity-read-and-write) are for. A patch format that *can* address a list member does not replace them: a patch addresses by structure, and a seat's position in a unit's list is not its identity — so an index-addressed edit rewrites a different seat the moment anything above it moves.

```bash
curl -X PATCH https://engine.example.com/config \
  -H "Authorization: Bearer $TOKEN" -H "X-Summary: allow one more self-iteration per turn" \
  -d '{"turn_engine": {"max_iterations": 4}}'
```

- **Deep merge.** `{"providers": {"llm": {"main": {"model": "claude-opus-5"}}}}` changes that model and leaves the provider's type, its keys and every other provider alone.
- **`null` deletes.** `{"integrations": {"gitlab": null}}` removes the section — without it a config surface can only add.
- **Arrays replace.** RFC 7396 cannot address a list element, so `mcp_servers: [...]` in a patch replaces the whole list. Editing one server is what [`PUT /config/mcp-servers/{name}`](#per-entity-read-and-write) is for; inventing a list syntax here would give two answers to one question. So does `roles:` or `units:`, which replaces every seat or every unit at that level: edit one through `PUT /config/roles/{handle}` or `PUT /config/units/{id}`. What a replacement does not remove is a field this build cannot represent: see [Fields a newer build wrote survive every write](#fields-a-newer-build-wrote-survive-every-write).
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
      "kind": "dangling_reference", "ref": "manages",
      "path": "roles[0].manages[1]", "segments": ["roles", 0, "manages", 1],
      "seat": "ceo", "unit": "", "from": "ceo", "to": "ghost",
      "message": "seat \"ceo\" manages \"ghost\", which is neither a seat's handle nor a unit's key, so the entry manages nobody. ..."
    }
  ],
  "derived": {"seats": [...], "units": [...]}
}
```

- **`warnings`** is what the engine will run but a person should know about. Always a list, empty when there is nothing to say. Each has the same locators as a [problem](#refusals-carry-located-problems) (`path`, `segments`, and the `seat` handle or `unit` key it is about, empty when neither), plus `from` and `to` as display text. Three kinds:
  - `dangling_reference`: a reference that resolves to nothing. `ref` says what carries it: `lead` (a unit's lead, a seat handle), `unit` (a root seat's `unit:`, a unit key) or `manages` (one `manages` entry, a seat handle or a unit key, at the index it was written).
  - `admission`: an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to) the stored company breaks, with `ref`, `from` and `to` empty. A write that keeps one is refused, so only a reload or a revert of a company a newer peer admitted under other rules answers with one, one beside each entity the violation names.
  - `advisory`: something valid and worth knowing — a human seat with no account on the Mattermost or Slack workspace the company talks on, which nobody can @-mention there; a token ceiling another ceiling makes unable to refuse a turn; a `${VAR}` nothing supplies. Every node also logs each one when it applies the revision, as a [`config_advisory`](../concepts/configuration.md#what-an-applied-company-is-worth-knowing) line, so the answer is not the only place it is said.
- **`derived`** is the hierarchy the engine derives from the document, in full: every seat in the engine's own order with its effective unit, primary manager, managers, reports, automatic reports and onboarding chain, and every unit with its effective type, lead and channel (and whether each was inherited). Each seat and unit carries its authored `path`. The fields are the ones [`GET /org`](#get-org) carries without paths; a client draws the hierarchy from this rather than deriving it again.

**Every seat's handle and every unit's key is written into the stored
document.** A seat that declares no `handle` is stored with the one its `name`
derives, and a unit with no `id` with one minted from its name, so a write
built from the stored document — `GET` then `PUT`, `PATCH`, the per-entity
routes — never moves the identity when it corrects a name. A body that leaves
them out is minted again from the names it carries, so a `PUT` of a file whose
names changed and whose handles or keys are absent replaces those seats and
units.

**A write that takes a human seat away while something holds it is refused.**
Removing a human seat — or making it an agent's — while the identity directory
binds somebody to it, at any stage short of their removal, or while an **open
invitation** holds it, is `409 seat_held`. `held` maps each such seat's handle
to the one thing holding it — never an invitee's address, since the writer
may be a unit's lead who holds no grant over the directory:

```json
{
  "error": "seat_held",
  "message": "This change removes a seat someone holds or is invited to. …",
  "detail": "This change removes human seats that somebody still holds: designer (held by sam.lee) and qa-lead (held by an open invitation (0192f00d-…)).",
  "held": {
    "designer": {"person": "018f3a9c-…", "kind": "person", "login": "sam.lee",
                 "stage": "active"},
    "qa-lead": {"invitation": "0192f00d-…",
                "expires_at": "2026-06-15T09:12:00Z"}
  },
  "hint": "Move each person to another human seat (PATCH /iam/people/{person} with seat) or remove them, unbind a service account (PATCH /iam/people/{person} with no seat), cancel an open invitation (DELETE /iam/invitations/{id}), each an admin's gesture, then send the write again."
}
```

The holder's `kind` decides the remedy, which the `hint` spells out: a
**person** holds a human seat for as long as they are here, so they are moved
to another human seat or removed — never unbound; a **service account**
(`kind: machine`) is unbound; an **invitation** is cancelled, or left to be
redeemed or to lapse at its `expires_at`. Each is a gesture for an
administrator. Where a person and an invitation both claim one seat — a
residue of a build before invitations held their seats — the person is named.
The check is advisory: a bind or an invitation landing between it and the
activation is not seen, and an offline `crewlet config import` asks nothing,
which leaves the residue [`GET /iam/check`](#get-iamcheck-walks-the-whole-directory)
reports. A node that cannot read the directory answers `503
seat_holders_unavailable` naming the `seats` rather than allowing it, and so does
one holding two rows bound to a seat the write removes; its `detail` says what
clears it, and a read no wait clears (an evicted node, a full log) carries no
`Retry-After`. A write that takes no human seat away never asks. A whole
document [replacing an unsealed
revision](#a-whole-document-replaces-an-unsealed-revision) always asks, about
every seat it does not keep as a human seat, since nothing says which seats the
revision it replaces had; its `503` names no `seats`.

#### A lead edits their own team

A person whose seat leads a unit may write the seats and units
inside it — the unit and every unit beneath it — without `admin`,
through the routes that write the org chart: `PUT` and `PATCH /config`,
`POST /config/revisions/{id}/revert`, and `PUT /config/roles/{handle}` and
`PUT /config/units/{id}`. The route admits anybody bound to a seat before the
body is read, and refuses everybody else `403 unauthorized`; an agent is
refused `seat_refused` whatever it holds. Whatever the route, the write is
judged on **what it changes**, on both sides of it: every seat and unit it
adds, removes, moves, reorders or edits has to sit inside the caller's subtree
where it was and where it lands, a unit's own fields are judged on the unit itself
before and after (so handing it to another `lead:` is refused), a new `lead:`
or `manages:` entry has to name something inside it, a seat or unit added
under a name another object's `lead:` or `manages:` already states has to be
named only from inside it. A project, space or channel, a seat's email or
contact identity, a credential, a `${VAR}` in any field and a setting are
never a lead's. The rule, change by change, is in [Identity and
Access](../concepts/identity-and-access.md#a-lead-edits-their-own-team).

A write any part of which is not the caller's is refused whole, by the rule
that refused it. On authority — a part outside the caller's subtree, or one
that is never a lead's — it is `403 unauthorized` with a `refused` list naming
every such part, so a client can show each beside what it is about. The
top-level `reason` is the parts' own where they agree and `no_grant` where they
do not, and `grants` names the permission that would admit **every** part,
today `admin`; each part carries its own `reason` and `grants` too. A bar on
the caller — an agent's seat on the org chart's routes — is `403 unauthorized`
with that reason, no permission named and no list, since no part is what shut
it. A write refused only because the caller's proof of who they are lapsed is
`403 step_up_required` naming the window, exactly as the route answers it — and
since every check of one request is judged at the instant it arrived, a proof
the route accepted never lapses half-way through the write. A
[dry run](#dry-runs) answers the same before anything is stored.

```json
{
  "error": "unauthorized", "message": "...",
  "reason": "no_grant", "grants": ["admin"],
  "refused": [
    {"kind": "unit", "id": "platform", "op": "changed", "side": "after",
     "place": "platform", "why": "self", "reason": "not_lead", "grants": ["admin"]},
    {"kind": "unit", "id": "platform", "op": "changed", "side": "after",
     "place": "data", "why": "lead", "value": "data-lead", "reason": "not_lead",
     "grants": ["admin"]},
    {"kind": "seat", "id": "staff-eng", "op": "changed", "place": "",
     "why": "credential", "value": "mcp_env", "reason": "no_grant",
     "grants": ["admin"]},
    {"kind": "setting", "id": "mission", "place": "", "reason": "no_grant",
     "grants": ["admin"]}
  ]
}
```

| Field | What it says |
|---|---|
| `kind`, `id` | A `seat` by handle, a `unit` by key, a `setting` by its top-level key, or the `document` (no `id`) for a write that changes nothing — re-publishing the company unchanged re-activates it on every node, which is `POST /config/reload`'s and `admin`'s; a [dry run](#dry-runs) of a seat or unit sent back unchanged is answered to its lead instead |
| `op` | What the write does to the seat or unit: `added`, `removed`, `moved` or `changed` — its fields, or where it sits among its siblings, since order decides which of a seat's managers is its own |
| `side` | `before` (the revision replaced) or `after` (the one proposed): which document the place was read in |
| `place` | The key of the unit the change reaches; `""` is the company root, which is nobody's subtree |
| `why` | What reaches it: `place` (where the object sits), `self` (a unit's own fields), `lead` or `manages` (a new reference, with the name in `value`), `named` (another object's `lead:` or `manages:` entry that already states the id an added seat or unit takes, with the id in `value`), `duplicate` (two objects on one id, which only `admin` may write), `credential` (a credential field, compared whole so an `mcp_env` key with nothing under it counts, or a `${VAR}` in any other field, with the field's path in `value`), `key` (a `project`, `space`, `channel`, `email` or `contact` set, cleared or changed, with the field in `value`), or `unchanged` (the `document` itself) |
| `reason` | The authority table's reason: `not_lead` for a place outside the caller's subtree, the company root included, or `no_grant` for a credential, a key, a setting or an unchanged document |
| `grants` | The permissions any one of which would admit this part — `admin` for every part a lead is refused |

A lead reads what they may write the same way: `GET /config/roles/{handle}`
and `GET /config/units/{id}` serve a seat or a unit of their subtree, masked
like every read here, and refuse one outside it `403 unauthorized`. Both the
read and the write of one seat or unit are decided where the id sits **before**
it is looked up or the body is read — at the company root for an id the company
does not have — so a seat in another team, a seat at the root and an id that
does not exist are refused in the same bytes, whatever the body, and only
`admin` is told a seat or a unit
is missing. A refused write that reaches outside from inside the caller's
subtree is more forthcoming by design: each refused part names the place it
reaches, which is where something outside the caller's subtree sits — the unit
a seat a `manages:` entry names sits in, or the root for a name nothing answers
to. So the chart's shape — which seats exist and where they sit — is no secret
from anybody bound to a seat, as it is none from anybody signed in, who is
served the org chart whole; a seat's or a unit's fields are, and only a read serves
them. The whole document, its history and its references stay `admin`'s,
so a lead edits through the entity routes.

#### Dry runs

`PUT /config?dry_run=true`, `PATCH /config?dry_run=true`, `PUT /config/{kind}/{id}?dry_run=true` and `PUT /config/charter?dry_run=true` are the same request, checked in the same order, that store, activate and publish nothing. The dashboard's organization builder sends one on every edit, and its Budgets screen one before every ceiling it saves, so a check is always exactly the write a save would send. An entity write needs its check more than the whole-document writes do: its caller never sees the rest of the document, so the whole-company validation behind the splice is the only place it learns that a seat fine on its own leaves the company invalid, or that a ceiling it raised now sits above the company's (a warning, which only a check shows before the save).

```bash
curl -X PATCH "https://engine.example.com/config?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "If-Match: \"$REV\"" \
  -d '{"mission": "Ship the thing"}'
```

A valid check answers `200`:

```json
{"valid": true, "base_revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41", "warnings": [], "derived": {"seats": [...], "units": [...]}}
```

- **`dry_run` is read before anything else**, and takes exactly `true` or `false`, or nothing. Any other value (`1`, `yes`, an empty value, the parameter twice) is `400 invalid_query`: the two readings of a guess differ by whether the fleet's configuration changes.
- **No summary is needed**, because nothing is stored to record one on. A `_summary` key in the body is still lifted out, so the document checked is the one the write reads.
- **`base_revision_id`** is the fleet's revision the check was built on, and `""` when nothing is active. A client whose draft was built on a different revision learns that the configuration moved without a second request.
- **Each warning says whether this write introduces it.** A check's `warnings` are the [write's](#what-a-write-answers), judged on the whole document it would store, so they include every advisory the active revision already carries. Each also has `"introduced": true` when the active revision does not carry it (compared by its kind, seat, unit, reference and message, never by its `path`, which a write moves by adding a seat ahead of it), and `false` when it does. Every warning of a check made with nothing active is introduced. Ask Crewlet shows a change's introduced warnings on its proposal card and leaves the rest off, because a warning the company already had is not news about the change.
- **Every other refusal is the write's, in the write's order**: `409 no_active_revision` for a patch with nothing to patch, `400 conflicting_preconditions` for `If-Match` beside `If-None-Match`, `412 revision_advanced` for a stale `If-Match`, `412 already_configured` for `If-None-Match: *` on a configured company, and `400` with [problems](#refusals-carry-located-problems) for a document the write would refuse.
- **A dry run is decided as the write, without the step-up** (ADR-0055): the same permission, a [lead's](#a-lead-edits-their-own-team) included, but not the recent proof, because it changes nothing. A person whose sign-in is old checks a change and is asked to confirm it is them only to make it. Ask Crewlet's read access sends a dry run to these routes, and to no other, which is how it checks a change before the person is shown it.
- A dry run needs the same token a write does, and a [lead's](#a-lead-edits-their-own-team) is judged as the write would be: a part that is not theirs is `403` with the `refused` list. One answer differs: a seat or unit sent back **unchanged** to its own address is checked for its lead, though the write would be refused as a re-publish, because the check stores nothing and is how a lead reads what the engine derives of their unit as it stands. The whole document sent back unchanged stays refused, since no address decided whose check it is.

#### Refusals carry located problems

A refused document (`400 validation_error`, `400 invalid_patch`, `400 invalid_body`) is the [refusal envelope](#every-refusal-is-one-envelope) with a detail of its own: `detail` (one line per failure) and `hint` beside the `error` code and its `message`, and **`problems`** — the same failures, located and classified, so a client puts each beside the field it is about without parsing anything.

```json
{
  "error": "validation_error",
  "detail": "workers.researcher.model: value not in the allowed set: \"nowhere\" is not a configured provider. Use one of providers.llm's: \"zulu\". ...",
  "hint": "The WHOLE document a write produces is validated, ...",
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

A `validation_error` also carries the `derived` hierarchy the engine built from the refused document, as far as it got, so a client can place each problem on the chart it would have drawn.

No message repeats a credential. A document read from `GET /config` carries masks, which a write restores from the stored revision before validating, so the values a refusal judges are ones the caller was never shown: a message says what rule a value breaks and never the value, a fragment of it, or its length.

The [`/setup`](#setting-an-integration-up) submissions that change the document answer their `validation_error` with the same `problems`.

#### Conditional requests

`GET /config` and every entity `GET` return an **`ETag`** — the fleet's revision id, quoted, the same on every node while the fleet has an activation. It is the token the write side takes, so a read-modify-write needs no second request to find it, and a tag read from one node is good on any other, one that refused the fleet's revision included.

| Header | On | Meaning |
|--------|-----|---------|
| `If-None-Match: <etag>` | `GET` | `304 Not Modified` when the fleet's revision has not moved |
| `If-Match: <etag>` | writes | Proceed only against that revision; `412 revision_advanced` otherwise, naming the fleet's as `current_revision_id` beside `your_base`, when the request arrives or when another write is activated before this one can be. Never beside `If-None-Match`: `400 conflicting_preconditions` |
| `If-Match: *` | writes | Proceed only if *something* is active; `412 no_active_revision` when nothing is configured |
| `If-None-Match: *` | `/config` writes | Proceed only if **nothing** is configured: the fleet has no activation, and this node knows no company either. `412 already_configured` otherwise, naming the revision it lost to — on a node that joined the fleet and has not reconciled yet too, since it answers the pointer like every other |
| `If-None-Match: *` | entity `PUT` | Proceed only if **that entity** does not exist: the create-only write that adds an MCP server or an LLM provider. `412 entity_exists` when one does; `400 conflicting_preconditions` beside an `If-Match` |

The bare revision id is accepted wherever an `ETag` is, unquoted, because this surface shipped that form before it had entity tags. `If-None-Match: *` is the only create-only precondition — about the company at `/config`, about the entity at an entity's own address — and every `If-Match` value other than `*` is an entity tag, matched against the fleet's revision and nothing else.

Independently of any header, every write names the revision it derived from as the new revision's parent, and the activation is a compare-and-set on that parent — or, with no pointer, a create-only one — so a lost update is refused **whether or not** the caller sent a precondition. See [Concurrent writes](#concurrent-writes).

**Nothing under `/config` is cacheable.** Every response the surface writes carries `Cache-Control: no-store`: reads, `304`s, writes, refusals and error bodies, and the `404` and `405` it answers for a path or method it does not serve. A body here is the company's settings: which integrations are wired and where, and the `${VAR}` name behind every credential, and a stored copy would outlive the session and the token that read it. Revalidation still works, because it never depended on a cache: a client that wants a `304` sends `If-None-Match` with the `ETag` it kept.

#### The charter: mission, vision and policies

The company's **charter** is the part of the document every seat is handed
word for word on every turn: `mission`, `vision` and `policies`. It has a
resource of its own, which is what Company › Principles edits in place.

```http
GET /config/charter

200 OK
ETag: "charter-5f0c…"

{"mission": "Help friends split costs fairly.",
 "vision": "Splitting a bill is never the awkward part again.",
 "policies": ["Every amount is exact to the cent.", "Work is tracked on the board."]}
```

Every field is always sent: `""` and `[]` when nothing is written.

```http
PUT /config/charter
If-Match: "charter-5f0c…"
X-Summary: tighten the money policy

{"mission": "…", "vision": "…", "policies": ["…", "…"]}

200 OK
ETag: "charter-91ab…"

{"charter": {"mission": "…", "vision": "…", "policies": ["…", "…"]},
 "changed": true, "revision_id": "0192…", "epoch": 42}
```

- **The body names all three fields and nothing else**, and replaces the
  charter whole: `policies` is the list in the order every seat reads it.
  A missing or unknown field is `400 invalid_body`. Each value is trimmed of
  the space around it, and an empty mission or vision is removed from the
  document.
- **`If-Match` is required** (`428 bad_if_match` without it) and its tag is
  the charter's **content**, not the revision's. A write elsewhere in the
  document is not a conflict, so a draft of the mission is not refused because
  somebody connected an integration meanwhile. A charter that changed since
  the tag was read is `412 version_moved`, carrying the charter now as
  `current` and its tag as `etag` (and in `ETag`): show the draft beside it,
  then send the draft again with that tag to keep it. `If-Match: *` writes
  whatever the charter says now.
- **It is a revision like any other write.** The same pipeline as `PATCH
  /config`: the managed-document check, the whole document validated, a sealed
  revision recording its author and credential, the fleet's compare-and-set
  and the `config_revision_activated` event. `X-Summary` is optional here: one
  left out is derived from what changed (`edited the company's mission and
  policies`). A revision that moved for another reason between the read and
  the compare-and-set is merged onto, up to three attempts, after which the
  answer is `409 revision_advanced`.
- **A charter that changes nothing writes nothing**: `200` with
  `"changed": false` and the active `revision_id`, because a new revision
  re-activates every node and rebuilds every seat. This is asked before
  `If-Match`, which makes a save safe to send again: a retry whose first
  answer was lost finds the charter already saying what it asks for, and is
  answered rather than refused.
- `?dry_run=true` judges the write and stores nothing, answering
  `{"valid": true, "charter": …, "changed": …}`.
- It is `admin`'s, read and written on verbs of its own
  (`config.principles.read`, `config.principles.write`), decided as
  `config.read` and `config.write` decide the whole document; the write asks a
  recent proof as every write here does, and is admitted only while it changes
  the mission, the vision or the policies and nothing else. The verbs are its
  own because Ask Crewlet may read the charter as the person and
  write it only as a change its owner approved from a before-and-after, sent
  here with the `If-Match` its preview read (ADR-0052); no other part of the
  document is Ask Crewlet's.

**The bounds**, in characters (Unicode code points, the count an editor shows),
because every character is in every seat's prompt on every round. A charter
past one is `400 validation_error` with the problem located at `mission`,
`vision`, `policies` or `policies[i]` (`segments` `["policies", 3]`):

| Field | Bound |
|-------|-------|
| `mission` | at most 1024 characters, on one line |
| `vision` | at most 2048 characters; paragraphs allowed |
| `policies` | at most 25 entries |
| each policy | at most 1024 characters, on one line, not blank, and not a repeat of another |

The mission and each policy are one line because a seat reads the mission
inline after its label and each policy as one item of a list; a line break is
refused with kind `shape`. A vision in paragraphs is quoted line by line in the
prompt, so no line of it reads as the prompt's own.

Each bound is counted on the trimmed value. Every write of the document
through the API (`PUT` and `PATCH /config`, a per-entity write, `/setup`)
holds them too, judged on [what the write
changes](../concepts/configuration.md#mission-vision-and-policies): a write
that leaves a faulty part untouched is admitted, with a warning naming it in
its answer, so an old fault never locks a company out of its other settings.
Here, the parts the charter write changes are judged, and the parts it leaves
as they were are not.

**An import is never refused for the charter.** `PUT /config?import=true`
writes the document as an import of a restored or migrated file: every other
rule holds as for any write, and the charter is judged warn-only, its faults
named in the answer's `warnings`. It is what `crewlet config import` sends to a
running node, so a file that ran under an earlier build always imports, as it
does into a stopped node and at boot. It is `admin`'s alone (a lead the route
admits for their own units is refused `403` before the body is read), asks a
recent proof as every write does, and records the revision with `source`
`file`, as an offline import does, and its activation event with
`revision_source: file`, beside the author and the credential (`operator_id`).
Decided by `config.write` on the authority table, so a Tier A token may use it
(fresh by construction), and so may a machine token whose owner holds `admin`;
on a [managed](../concepts/configuration.md#managed-configuration) document
only the listed writers may. It relaxes the charter's change rule and nothing
else: every other rule, `If-Match` and the managed check hold as for any write.
The flag is `true` or `false`, given once, or `400 invalid_query`. The
dashboard never sends it (a gate fails if its source does), and the operator's
MCP and act surfaces serve no `/config` write at all.

#### Per-entity read and write

Four collections, `GET` and `PUT`, and `DELETE` for a model or an MCP server:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/config/{kind}/{id}` | One entity, redacted, with the entity's own `ETag` (a digest of what it serves, not the document's revision). **The body is the entity itself**, so it goes straight back into the `PUT` |
| `PUT` | `/config/roles/{handle}` | Replace one seat, wherever it lives — root-level or inside a unit, at any depth. Every entity `PUT` takes `?dry_run=true`, see [Dry runs](#dry-runs) |
| `PUT` | `/config/units/{id}` | Replace one org unit, by its key |
| `PUT` | `/config/llm-providers/{key}` | Replace one named LLM provider; with `If-None-Match: *`, add one under that key |
| `PUT` | `/config/mcp-servers/{name}` | Replace one MCP server entry; with `If-None-Match: *`, add one after every server already declared |
| `DELETE` | `/config/llm-providers/{key}` | Remove one model and repoint everything that names it, in one revision. See [below](#delete-configllm-providersid-remove-a-model-and-repoint-what-names-it) |
| `DELETE` | `/config/mcp-servers/{name}` | Remove one MCP server and clear everything that names it, in one revision, deleting the stored values only it read where asked. See [below](#delete-configmcp-serversid-remove-an-mcp-server-and-clear-what-names-it) |

Any other method is `405` with an `Allow` header naming the methods the path takes. There is no other `DELETE`: removing a seat or a unit is a full-document edit, for the reasons below.

Why these exist beside the whole-document write: `PUT /config` makes every edit
a company-wide one. A founder changing one seat's goal sends back a document
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
invites, because the caller never sees the rest of the document. The
[seat-holder check](#what-a-write-answers) applies here too: a unit `PUT` that
drops a human seat somebody holds, or an open invitation holds, is
`409 seat_held`.

**The precondition is the entity's.** An entity `PUT` takes `If-Match` with
the `ETag` its `GET` answered (or `*`), judged against the entity as it stands
in the revision active when the write lands, never against the whole
document. A revision that moved for another reason (another seat added, a
ceiling patched, a credential sealed) is no conflict: the write is spliced into
the revision active then, up to three times while other writes keep landing,
and `409 revision_advanced` only after that. An entity that itself changed
since it was read is `412 version_moved`, carrying the entity as it stands
(`current`) and its `etag`, so the caller can show its change beside what
replaced it and send it again with that tag. `If-None-Match: *` is the
create-only write, never beside `If-Match`, and any other `If-None-Match` is
`400 bad_if_match`.

Four rules follow from that:

- **An unknown field is refused, not dropped.** The entity body is read by the
  whole-document parser, JSON or YAML: `gaol` where `goal` was meant is
  `400 invalid_body` with an `unknown_field` [problem](#refusals-carry-located-problems)
  placed where the seat sits in the document (`roles[1].gaol`), with its line in
  the body. A decoder that ignored what it did not recognise would answer `201`
  and store the seat with its goal silently gone.
- **A plain `PUT` never creates.** An id nothing carries is `404 no_such_entity`
  (on `roles` and `units`, to an administrator alone — anybody else
  is refused `403` as for a seat outside their team), its `hint` saying how
  that collection is added to, not a new entity: naming
  one that is not there is far more often a typo than
  an intent to add one. The intent is SAID with `If-None-Match: *` — the
  create-only write, for the two flat collections (`mcp-servers`,
  `llm-providers`): a taken id is `412 entity_exists` rather than a
  replacement, the body's identity must match the path as on any `PUT`, and
  the whole company is validated with the new entity in it. An added entity
  lands LAST in its collection's order — a server after every declared server,
  a provider at the end of `providers.llm_order`, the order an unpinned seat
  resolves a provider in. A seat and a unit
  are `400 not_creatable` — each has a place in the chart the path cannot
  name — and are added by replacing the unit they sit in, or through
  `PUT /config`, which shows the whole thing. The id is looked up before the
  body is read, so a mistyped one is a `404` whatever the body holds.
- **The id in the path is the identity, and a `PUT` never renames.** A body
  whose own identity disagrees with the path is `400 identity_mismatch`, not a
  move: nothing that points at the old identity travels with the splice. A
  seat's durable id is a UUIDv5 over (company name, handle), so a different
  handle is a different seat; a unit's key is referenced by every `manages:`
  entry and root seat `unit:` that names it; a provider's key is what every
  seat's `llm:` chain names; and an MCP server's name is the key every
  `mcp_env` block is keyed on and the prefix its tools carry. A body that
  leaves `handle` (or a unit's `id`) out keeps the one the path names, so
  changing a seat's display name is an ordinary edit. A seat's handle and a
  unit's key are permanent: a different one is a removal and a creation, made
  through `PUT /config`, where what has to move with it is visible.
- **The same summary and `If-Match` rules apply**, and with nothing
  configured the write answers `409 no_active_revision` — there is nothing to splice
  into, and building a company out of one seat is not what this route is for.

#### `DELETE /config/llm-providers/{id}`: remove a model and repoint what names it

A model is named by every seat field and Ask Crewlet field that runs on it, so
removing it alone leaves a document validation refuses. This route takes the
removal **and the repoint of every dependant** as one write, addressed by
identity (a seat's handle and the job the field points at a model, or
Ask Crewlet's chat or deep work), never by a list index. `config.write` (admin,
a recent proof); `?dry_run=true` checks it and stores nothing, `If-Match` takes
the entry's own `ETag` from its `GET` (optional, as on the entity `PUT`), and
a summary is required unless it is a dry run: the `X-Summary` header or a
`_summary` key in the body, the header winning when both are sent, as on every
config write.

```json
{
  "repoint": [
    {"at": {"seat": "qa", "job": "review"}, "to": "sonnet"},
    {"at": {"seat": "backend-engineer", "job": "executor"}, "to": null},
    {"at": {"assistant": "chat"}, "to": "aux"},
    {"at": {"assistant": "deep_work"}, "to": null}
  ],
  "assistant_off": false
}
```

- `job` is `executor`, `review`, `subagent`, `auxiliary` or `judge`. A seat
  that writes one job twice (`llm_review` and `llm.review`) has both fields
  answered by its one `repoint`. Each `at` is answered once: a body naming one
  twice is `400 invalid_body`, naming both rows.
- **`to: <key>`** replaces the entry in that field's chain where it stood,
  each key once; `to` naming the entry being removed is refused at the field
  (`kind: "invalid"`). **`to: null`** removes it; a chain left empty clears the field,
  so the job resolves as an unset one does. `{"assistant": "deep_work"}` with
  `null` removes the `assistant.deep_work` block.
- The entry leaves `providers.llm` and `providers.llm_order`.
- Ask Crewlet's chat left with no model while Ask Crewlet is on is refused
  at `assistant.llm` unless the body sends `"assistant_off": true`, which writes
  `assistant.enabled: false` in the same revision.
- The **whole document** is validated with the admission rules, so an executor
  job moved to a harness that cannot run one, or an Ask Crewlet target that allows web
  tools, is refused at its field.

**Every reference has to be answered.** One left out, or a `repoint` that
answers nothing, is `422 validation_error` with one problem per reference
(`kind: "conflict"`, its `segments` the written field, its `seat`, and a message
such as "qa's reviewing work still names codex-review: repoint it or leave it
unset in the same write") and `references`, every written reference as
`{at, segments, chain}`. A `repoint` that answers nothing is located at its own
row, `segments: ["repoint", i, "at"]` (`kind: "unknown_value"`). A dry run with an empty body is how a page draws the
list.

**A move nobody wrote is a warning.** A seat's job that names no model runs on
the company's fallback, the entry keyed `default` or else the first in
`llm_order`, so removing that entry moves the job although no field named it.
Each such move is a warning of kind `resolution_moved` with the seat (`seat`),
the job (`ref`), the model it ran on (`from`), the one it will (`to`) and a
sentence ("pm's memory and summaries will run on aux"), in the dry run and in the
write's answer. Resolution is the engine's own, so the warning says exactly
where the work lands. A job the seat names no model for but whose executor
names one runs on that executor's chain, and moves with the executor's
`repoint`, so it is not warned about. Removing the company's last model leaves
`to` empty, and the sentence says the job will have no model to run on.

Answers: a dry run `200 {valid, base_revision_id, warnings, derived}`, a write
`201 {revision_id, epoch, warnings, derived}`. Refusals: `404 no_such_entity`,
`409 revision_advanced`, `403 config_managed` (a managed document),
`412 version_moved` (the entry changed since its `ETag`), `422 validation_error`
and `400 summary_required`.

#### `DELETE /config/mcp-servers/{id}`: remove an MCP server and clear what names it

An MCP server is named by its name from two other places: each seat's and
unit's `mcp_env` block keyed on it, and each seat's `sandbox.mcp.servers` entry
listing it. Removing the entry alone leaves those naming nothing, which
validation refuses. This route takes the removal **and the clearing of every
field that names the server** as one write, and can delete the stored values
only that server read. `config.write` (admin, a recent proof), and
`secrets.write` as well when the body names a value to delete, asked before
anything is read or written; `?dry_run=true` checks it and stores nothing,
`If-Match` takes the server's own `ETag` from its `GET` (optional, as on the
entity `PUT`), and a summary is required unless it is a dry run: the
`X-Summary` header or a `_summary` key in the body, the header winning when
both are sent.

```json
{
  "clear": true,
  "delete_secrets": ["LINEAR_ORG", "PM_LINEAR_TOKEN"]
}
```

- **`clear: true`** clears every field that names the server: each `mcp_env`
  block keyed on it, a seat's or a unit's, and each `sandbox.mcp.servers`
  entry listing it. A mapping left empty is dropped, and so is a seat's
  `sandbox` that held nothing else. An `mcp_env` block the engine reads by its
  own name (`github`, `gitlab`, `datadog`, `atlassian`, `jira`, `confluence`)
  is the integration's as well as the server's, so it stays.
- **`delete_secrets`** names values in the
  [credential store](#secrets--the-companys-credentials-auth-gated) to delete
  once the removal is written, each once. Each must be one nothing reads once
  the server and the fields it clears are gone, judged as `/secrets` judges
  what reads a value: every `${VAR}` the document names, and every variable it
  reads without naming one.
- An empty body clears nothing and deletes nothing. Any other field is
  `400 invalid_body`.

**Every field that names the server has to be cleared.** Without `clear`, a
server anything names is `422 validation_error`, with one problem per field
(`kind: "conflict"`, its `segments` the written field, its `seat` or `unit`,
and a message such as `pm's mcp_env block for "linear" still holds
credentials: send clear to clear it in the same write`) and `dependants`. A
value in `delete_secrets` that something else still reads is a problem at
`segments: ["delete_secrets", i]` (`kind: "conflict"`, naming the paths that
read it, as the document stands now), and a name the server never read is
`kind: "unknown_value"`. A dry run with an empty body is how a page draws the
list. The **whole document** the removal produces is validated, as on every
write.

`dependants`, on the refusal and on a dry run that passes:

```json
{
  "mcp_env": [
    {"kind": "seat", "key": "pm", "name": "PM", "segments": ["roles", 0, "mcp_env", "linear"]},
    {"kind": "unit", "key": "eng", "name": "Eng", "segments": ["units", 0, "mcp_env", "linear"]}
  ],
  "sandbox": [
    {"seat": "pm", "name": "PM", "segments": ["roles", 0, "sandbox", "mcp", "servers", 0]}
  ],
  "secrets": ["LINEAR_ORG", "PM_LINEAR_TOKEN"]
}
```

`kind` is `seat` (its `key` the handle) or `unit` (its `key` the unit's key),
and `name` is the display name. `secrets` lists the values the store holds
that nothing reads once the server is gone, which is what `delete_secrets` may
name: a variable a node takes from its environment is never listed, since the
store holds nothing to delete. Every list is names, never a value.

**The values go after the revision is written,** never before, so a removal
that loses its write deletes nothing. Each is asked again of the fleet's
revision just before it goes, so one that a write landing meanwhile reads is
kept. The answer's `secrets` says what became of each, as
`{name, outcome, read_by?}`:

- `deleted`: the store held it and no longer does;
- `absent`: the store held nothing under that name;
- `kept`: a revision activated after the removal reads it, and `read_by` names
  the paths, so it stays;
- `failed`: the store, or the fleet's revision, did not answer, so the value
  may still be there, and `DELETE /secrets/{name}` deletes it.

The status stays `201` whatever became of a value, since the server is removed
either way. Each value deleted logs `secret_removed` with its name, the server
and who removed it, as `DELETE /secrets/{name}` does.

Answers: a dry run `200 {valid, base_revision_id, warnings, derived,
dependants}`, a write `201 {revision_id, epoch, warnings, derived, removed,
cleared, secrets}`, where `removed` is the server's name and `cleared` is
`{mcp_env, sandbox}` as above. Refusals: `404 no_such_entity`,
`409 revision_advanced`, `403 config_managed` (a managed document),
`403 unauthorized` for a caller without `secrets.write` (the Dashboard
Assistant never takes it, so it may remove a server and never its values),
`412 version_moved` (the server changed since its `ETag`),
`422 validation_error` and `400 summary_required`.

### `/secrets/*` — the company's credentials (auth-gated)

Every `/secrets/*` and `/setup/*` route takes `admin`, reads included: the
listing alone says which credentials a company holds and when each last
changed. Every node serving the API serves them, because every node opens the fleet's
[coordination store](../concepts/coordination.md) that holds the rows.

| Route | Needs |
|---|---|
| `GET /secrets`, `GET /secrets/{name}` | `admin` |
| `GET /secrets/{name}?reveal=true`, `POST /secrets/reveal` | `admin`, a **person present** — a request that presented a machine token is refused `403 token_refused` — and a proof of identity inside `step_up`, decided before the store is read, so a caller refused is refused alike for a name that exists and one that does not |
| `PUT /secrets/{name}`, `PUT /secrets`, `DELETE /secrets/{name}`, `POST /secrets/rekey` | `admin`, and a proof inside `step_up`. A write of a name the active revision reads also asks `config.write`, before anything is sealed, for the re-activation that follows (below) |
| `GET /setup/integrations`, `GET /setup/integrations/{kind}`, the two `runs` reads | `admin` |
| `POST /setup/integrations/{kind}/inputs`, `POST /setup/integrations/{kind}/verify`, `DELETE /setup/integrations/{kind}`, `POST /setup/integrations/{kind}/check` | `admin` |
| `POST /setup/integrations/{kind}/provision`, `POST /setup/integrations/github/app` | `admin`: a writing pass is handed a sink that mints and seals, and an app's private key is sealed on its way back |
| `GET /setup/integrations/{kind}/access`, `POST /setup/integrations/{kind}/access/plan` | `admin` (`setup.read`): the plan writes nothing, so it asks for no proof |
| `PUT /setup/integrations/{kind}/access` | `admin` (`setup.access.write`) and a proof inside `step_up`. A save that raises an agent to an administration role at the vendor also asks `setup.access.raise`, which needs a **person present**: a machine token is refused `403 token_refused` |
| `POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}`, `POST /setup/integrations/slack/access/seats/{handle}/{lane}/install` | `admin` (`setup.connect`) and a proof inside `step_up`; `rotate` also asks `secrets.write`, since it seals a fresh credential; `keep` writes the document as a save does, so it also asks `setup.access.write`, and `setup.access.raise` where what it keeps is an administration role |

**Connecting is `admin`'s and nothing more.** `/setup` performs no write of
its own — a credential goes through the store `/secrets` serves — so connecting
asks exactly what writing a credential there asks.
Every `/setup` and `/secrets` write asks for a proof of identity inside
`step_up` as well, and so does a reveal; the reads ask for none (see
[Some gestures ask how recently you proved who you are](#some-gestures-ask-how-recently-you-proved-who-you-are)).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/setup/integrations` | What every integration this build can set up still needs, plus the address third-party apps reach this deployment on. `external_url` carries a single `value` and the `config_path` it was read from: `api.public.external_url` when the node has a [public listener](#which-listener-serves-what), and `api.external_url` otherwise — never the person's address where the two differ, since every webhook, manifest and redirect is built on it. Both are Tier A, required on a node serving this listing and resolved before that file is decoded, so the `present`/`resolved`/`reference` triple the Tier B pointer needed describes a state that can no longer occur |
| `GET` | `/setup/integrations/{kind}` | One integration's requirement list and state |
| `POST` | `/setup/integrations/{kind}/inputs` | Supply or generate those values: credentials are sealed, the rest is patched into the company |
| `POST` | `/setup/integrations/{kind}/verify` | Check one typed credential at its vendor with a read-only call, before anything stores it. Writes nothing: no secret, no config, no revision and no reconcile state. See [Checking a credential before it is stored](#checking-a-credential-before-it-is-stored) |
| `DELETE` | `/setup/integrations/{kind}` | Disconnect: remove what the integration holds at the third-party app, then its block |
| `POST` | `/setup/integrations/{kind}/provision` | Run the third-party app's provisioning pass: mint what it needs, register its webhook |
| `POST` | `/setup/integrations/{kind}/check` | Run the same pass read-only, to see whether something fixed at the third-party app took |
| `GET` | `/setup/integrations/{kind}/runs` | The passes THIS NODE remembers for one surface, newest first, ten at a time. A pass is executed by whichever node held the surface's lease and is remembered in that node's own process, so the answer carries `scope` saying as much — an empty list on a fleet where another node ran the pass is an honest answer to a question the reader did not mean to ask. It exists because nothing could name a run id: the route below answered one pass and was reachable only by a caller that had just started it |
| `GET` | `/setup/integrations/{kind}/runs/{id}` | One pass, as the node that executed it remembers it |
| `GET` | `/setup/integrations/{kind}/access` | Every agent seat on one integration: what the document asks (`desired`), what the vendor showed on the last sync (`observed`), the last sync, the plan the next sync takes, and whether the agent's tool is live; what each lane offers now; and the integration's notice. The revision it was read at is `revision` and the `ETag`. See [Each agent's access to an integration](#each-agents-access-to-an-integration) |
| `POST` | `/setup/integrations/{kind}/access/plan` | What a draft would do at the vendor, step by step, writing nothing |
| `PUT` | `/setup/integrations/{kind}/access` | Save a draft as one revision of the company document, held to the revision the tab was read at, then start the sync. `?dry_run=true` answers the plan and the warnings and writes nothing |
| `POST` | `/setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}` | One action on one agent's account: `rotate`, `remove` (only once the agent is off), `keep` (write the vendor's setting into the document), `restore`, `retry` or `done` |
| `POST` | `/setup/integrations/slack/access/seats/{handle}/{lane}/install` | A fresh, single-use link a person follows to approve one agent's Slack app install, while the app waits for it. `{"url": "..."}`, `Cache-Control: no-store`, never stored and never part of a read |
| `GET` | `/secrets` | Every stored name with its `key_id`, `updated_at`, `updated_by` (the author), `updated_by_kind`, `operator_id` (the credential it was stored through, empty where none made the write) and `source` (the write path that stored it, in that path's own word: `api` for `PUT /secrets/{name}` whoever calls it, `setup` for an integration's setup, `provision` for a provisioning pass, and `cli` or `llm-login` for a `crewlet` command's own write to the fleet's store with its node's engine stopped (`crewlet secrets set`, and `crewlet llm`'s captured token or exported bundle), where a provisioner's `-secret-store` run made that way records `provision`; see [the secret store](../concepts/secret-store.md#the-design)); `engine_keys`, a count of the engine's own keys, `{"total": N, "by_key": {"<key id>": n}}`, naming none of them; and `identity_values`, the identity estate's sealed values counted the same way, `{"people": {"<key id>": n}, "invitations": {"<key id>": n}}` (a person's name, address and second-factor seed, and the address of every invitation still redeemable), or `null` on a node that runs no identity estate; and `assistant_values`, Ask Crewlet's sealed values counted the same way (a file a conversation keeps counted by the key its content is sealed under in the object store), `{"values": {"<key id>": n}}`, which no rekey moves (each leaves with its conversation), or `null` on a node that runs no Ask Crewlet estate; and `executor_values`, the seats' executor sessions counted the same way (each session a turn's row names counted by the key its object is sealed under in the object store), `{"values": {"<key id>": n}}`, which no rekey moves either (each leaves with its row), or `null` on a node that runs no executor log. **Never a value**. A rekey keeps each row's `updated_*` and `source`: it re-seals a value it did not choose, so the row goes on saying who stored it |
| `GET` | `/secrets/{name}` | The same fields for one name. `404 not_found` when it is unset |
| `GET` | `/secrets/{name}?reveal=true` | **Break-glass.** The decrypted value, `Cache-Control: no-store`, logged by name against the authenticated operator. The longest answer is some 384 KiB — the largest value a write admits, in the characters JSON spells longest — which is what a client reading it must be prepared to take |
| `POST` | `/secrets/reveal` | **Break-glass, for several names at once.** The body is `{"names": ["GITLAB_TOKEN_SWE", …]}` — at most 64 names, each an environment-variable name — and the answer is `{"values": {"<name>": "<value>"}, "missing": ["<name>"]}`: every name asked that is held, with its decrypted value (an empty one included), and every name nothing holds. Nothing beside the names asked is opened. `Cache-Control: no-store`, logged by every name asked against the authenticated operator, and decided on the reveal's own row before the body is read. It is what a [CLI run resolving a company document through a running node](cli.md#where-a-command-reads-the-companys-values) reads: having listed the names with `GET /secrets`, it asks for the variables its command reads that the node lists — and, with a `-secret-store` sink, the planned seats' tokens it reads back — in requests of at most 64, never one reveal per name. `400 invalid_body` for no names, more than 64, a field it does not read or a body that is not JSON; `400 invalid_name` for a name no `${VAR}` reference can hold; `400 invalid_query` for any query parameter; `403 reserved_name` for the engine's own key. The longest answer is about 24 MiB — 64 values of the largest size a write admits, each in the characters JSON spells longest — which is what a client reading it must be prepared to take |
| `PUT` | `/secrets/{name}` | Store or rotate one value. **The request body is the value**, raw bytes, up to 64 KiB — the size every store holds a value to, a direct write with the engine stopped included, which is what every reveal is sized from — and it is the only thing the request supplies; a longer body is `413 body_too_large`. The author is the caller the guard resolved and the `source` is this route's own, `api`, whoever calls it — never anything the request names; the answer carries `updated_by`, `updated_by_kind`, `operator_id` and `source` as the row records them, which is the only confirmation a client that could not choose them gets. `400 invalid_name` when the name is not an environment-variable name; `400 invalid_query` when the request carries any query parameter but one `defer_reload=true|false` (`?source=` included), with a `detail` naming each and a `hint` saying where the provenance comes from instead — checked after the name and before the body is read. `?defer_reload=true` seals the value and re-activates nothing, as the batch's `defer_reload` does (`reload` is `{"deferred": true}`, and it asks `secrets.write` alone): it is for a value the JSON batch cannot carry, one that is not UTF-8, sealed first so that the batch after it re-activates once for both. The answer also carries `read_by`, the paths of the active revision that read the name (an empty list when none does), and `reload`, what the [re-activation](#a-secret-write-re-activates-the-revision-that-reads-it) did |
| `PUT` | `/secrets` | Store or rotate **several** values as one write. The body is `{"values": {"NAME": "value", …}}`, at most 64 names, each held to the name grammar and the 64 KiB value bound; any fault refuses the whole body, `400 invalid_body` with `invalid` mapping each bad name to its reason, and nothing is stored. `403 reserved_name` for the engine's own key, `400 invalid_query` for any query parameter. Every value is sealed and the active revision is re-activated **once** when it reads any of them. With `"defer_reload": true` the values are sealed and nothing is re-activated: `reload` is `{"deferred": true}`, saying the values are **not active yet**, and `read_by` says what a later re-activation will reach. A deferred batch asks `secrets.write` alone, never `config.write`, since it changes nothing the company runs: `config.write` is asked of the re-activation that follows (`POST /config/reload`). It is for a writer that seals values one at a time, each durable the moment it exists, and re-activates once with its last write, as a provisioning run through a node does. Nothing that runs reads a deferred value until a re-activation: a writer that dies before its re-activation leaves its values sealed and inactive until the company is next re-activated, by anyone (`POST /config/reload`, or any apply). `200 {"names": [...], "bytes": {"NAME": n}, "read_by": {"NAME": [paths]}, "reload": {...}\|null}`. A seal that fails part way answers `500 internal_error` with `sealed` (the names stored before it) and the `reload` those were given |
| `DELETE` | `/secrets/{name}` | Remove one value. `200` either way, with `{"removed": true\|false, "read_by": [paths], "reload": {...}\|null}`: a removal of a name the active revision reads re-activates it, so whatever read it falls back or signs out now |
| `POST` | `/secrets/rekey` | Re-seal every record not already under this node's `secrets.active_key_id` — the engine's own keys included — and every person's sealed values in the identity estate, one record per person, answering the names of yours it moved, `engine_keys_moved`, a count of the engine's, and `identity` — `{"people": n, "values": n}`, or `null` on a node that runs no identity estate. An outstanding invitation's address is not moved (only a re-issue could carry it again) and is counted by the listing instead. A person whose record nobody could confirm, or whose value would not open, is `500 rekey_incomplete` carrying what did move — run it again, which moves only what is still under an old key. `?key_id=` is refused with `409` when it names a different key |

#### A secret write re-activates the revision that reads it

A `${VAR}` is resolved when a provider or a transport is built, so a value
sealed and left there changes nothing that is running. `PUT /secrets/{name}`,
`PUT /secrets` and `DELETE /secrets/{name}` therefore re-activate the active
revision, once per request and synchronously, when it reads a name they wrote
([ADR-0070](../../adr/0070-a-secret-write-re-activates-the-revision-that-reads-it.md);
what reads a name is what [`GET /config/references`](#config--live-config-management-auth-gated)
answers). The answer's `reload` is one of:

| `reload` | Means |
|---|---|
| `null` | Nothing the active revision reads names it; nothing was re-activated |
| `{"deferred": true}` | A batch sent with `defer_reload`: sealed and **not active yet**, until the caller's own re-activation or any later one |
| `{"revision_id": "…", "epoch": 42}` | The revision that re-published the company, and the epoch every node converges on |
| `{"error": "revision_advanced", "detail": "…", "stored_revision_id": "…", "node": "…"}` | Another configuration write activated first. The value is sealed; `POST /config/reload` re-activates the company that runs now |
| `{"error": "validation_error", "detail": "…"}` | The active company does not pass this build's validation, so it was not re-published |
| `{"error": "unavailable", "detail": "…", "stored_revision_id": "…", "node": "…"}` | The re-activation was stored and the coordination store did not answer it: whether the fleet runs it is unknown |
| `{"error": "no_active_revision" \| "internal_error", "detail": "…"}` | Nothing to re-activate, or a failure this node's log names |

**The status is `200` whatever `reload` says**, because the value is sealed
before the re-activation is asked for: a client that reads only the status, as
crewlet-operator does, is told the truth about the write.

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
name is what the answer points at. A name outside the grammar names no
secret, so reading one answers `404` and removing one answers
`{"removed": false}`.

**The body is the value, not a JSON wrapper.** A credential is arbitrary bytes
— a PEM key has newlines, a token can hold anything — and an encoding step
between the operator and the sequence the vendor compares is a `401` nobody
can explain.

**Provenance is the node's, and a write that names its own is refused.** Who
stored a value is the credential the request authenticated with
(`updated_by`, `updated_by_kind`, `operator_id`), and its `source` is the
route's own, `api` — so a write cannot make a row claim another path, and a
client that recognises its own rows does it by the credential that wrote them,
never by a word. The route takes no query parameter at all: one sent anyway,
`?source=` included, is `400 invalid_query` naming it, rather than ignored, so
a client still sending one learns at its first write that the row would not
record it.

**Reveal is opt-in on the wire**, not merely in the CLI. Without `?reveal=true`
the route answers what a listing answers for one name, so a browser, a crawl or
a link preview cannot pull a credential out by accident.

**Every value is sealed under the node's keyring.** The store has no plaintext
mode, and there is no node without a keyring to ask about: `crewlet validate`
refuses a Tier A file with no `secrets.keys`, and the engine refuses to start
without one.

**Every refusal here is the engine's envelope** — `error`, the sentence
`message` a screen shows, and the route's own detail beside them:
`reserved_name`, `invalid_name`, `invalid_query` (on a write or a reveal of
several names carrying any query parameter, with a `detail` naming each and a
`hint`), `invalid_body` (a reveal or a write of several names that names none,
too many, or a field it does not read, and a write of several whose names or
values the store refuses, with `invalid`), `not_found`, and on a rekey
`key_id_mismatch` (with `key_id` and `your_key_id`) and `rekey_incomplete`
(with the `moved` names and the `engine_keys_moved` count).

`crewlet secrets` is the client for all of this while its node's engine runs,
or with `-api` naming a node that does — see
[the secret store](../concepts/secret-store.md#which-store-the-cli-writes) for
why the CLI goes through a running node rather than reaching the KV beside it.
With that node's engine stopped it reaches the fleet's store itself, through no
route here: no node logs a `secret_revealed` line for what it reads, and a
write records the shell's user as its author, with no credential, under the
command's own `source`.

### `/llm/*`: models (Settings › Models)

What each model is doing is a question, answered on the query channel and by
the named routes below; what changes something is a route of its own, one verb
each. Every answer that depends on the node names it (`node`).

| Method | Path | Needs | Description |
|--------|------|-------|-------------|
| `GET` | `/llm/entries` | anybody | Every model's condition across the fleet (query `llm_entries`): an administrator's reading, or a member's with nothing that names a credential |
| `GET` | `/llm/runners` | `admin` | Whether each executor cell (`direct`, `container`, `e2b`) can start a box on each node (query `llm_runners`) |
| `GET` | `/llm/models?harness=claude-code\|codex\|opencode[&provider=…]` | `admin` | The models a model step suggests (query `llm_models`) |
| `GET` | `/llm/device-sign-ins/{id}` | `admin` | One device sign-in while its record lingers (query `llm_device_sign_in`) |
| `PUT` | `/llm/providers/{key}/credential` | `secrets.write`, and `config.write` with a draft `entity` or when it re-activates | Test candidate credential values with the model's own harness, and store them only if they sign it in; `?dry_run=true` tests and stores nothing |
| `POST` | `/llm/providers/{key}/check` | `models.check`; with a draft `entity`, also `config.write` and `secrets.write` | The doctor over the API, on this node |
| `POST` | `/llm/providers/{key}/retry` | `models.retry` | Probe a sitting-out model once, or clear its sit-out on every node |
| `POST` | `/llm/providers/{key}/host-login/check` | `models.check` | Read a harness login's status on this node now |
| `POST` | `/llm/providers/{key}/device-sign-in` | `secrets.write` | Start a Codex device-code sign-in |
| `DELETE` | `/llm/device-sign-ins/{id}` | `secrets.write` | Cancel one |

`models.check` and `models.retry` are an administrator's at any recency, and
Ask Crewlet's only on the person's approval: they change runtime
state, never config or a credential. `secrets.write` asks a recent proof and is
never Ask Crewlet's, since a credential's value travels in the body.

**A draft is decided as the writes it stands for.** A draft entry chooses its
harness, its overrides and its env, and resolves its `${VAR}`s through the secret
store as an apply would, so a check or a credential test of one asks
`config.write` and `secrets.write` too: an administrator's, with a recent
proof, and never Ask Crewlet's. A credential test's `dry_run=true` is asked at
the write's own proof, not a check's: it runs the harness and spends tokens.

**Every call is audited**, refused ones included (by the route, or by the
guard before it: a member's `403 unauthorized`, a stale proof's
`step_up_required`), as one `model_acted` in the runtime audit (`actor`,
`actor_kind`, `operator_id`, `action`, `provider_key`, the secret `names` a
credential call touched and never a value, `dry_run`, `outcome` and `refusal`,
and the `revision_id` a credential's re-activation activated). The node is the
envelope's. `action` is `check`, `retry`, `clear`, `credential`,
`host_login_check`, `device_sign_in` (a start) or `device_sign_in_cancel`. How
a device sign-in **ended** is a row of its own, `device_sign_in_ended`, with
`status` (`approved`, `failed`, `expired` or `cancelled`), published by the node
that ran the session, or by the sweep that fails one whose node is gone, and
attributed to whoever started it; an approved session whose login could not be
stored or carried to the fleet is `outcome: "failed"`.

#### `GET /llm/entries`

Built from every live node's heartbeat, which carries each model's sign-in
route, sit-out, last answer and last failure, parked seats and host login, plus
what this node resolved for each entry. An unreadable lease table is
`503 unavailable`: the table is the answer, so a blip is never drawn as "no
node knows".

```json
{
  "reading": "admin",
  "node": "node-1",
  "nodes": [{"id": "node-1", "reported": true}, {"id": "node-2", "reported": false}],
  "entries": [{
    "key": "default", "harness": "claude-code", "model": "claude-opus-5-5",
    "health": "expiring", "plan": "3f9a1c0e22b1",
    "sign_in": {"route": "token", "mode": "subscription", "variable": "CLAUDE_CODE_OAUTH_TOKEN",
      "source": "store", "resolves": true, "hint": "sk-ant-oat01-…Qm4x",
      "stored_at": "2025-11-04T18:37:00Z", "stored_by": "miles",
      "expires_at": "2026-11-04T18:37:00Z", "expiry_basis": "stored_at+365d", "host_logins": []},
    "sitting_out": null, "last_success": "2026-10-28T21:56:00Z", "last_failure": null,
    "waiting": null,
    "usage": {"days": 7, "calls": 412, "total_tokens": 38400000},
    "used_by": [{"seat": "pm", "job": "executor", "position": 0, "written": false, "source": "default-entry"}],
    "device_sign_in": null
  }],
  "alarms": [{"kind": "llm_credential_expiring", "detail": "…", "remedy": "…", "entries": [...]}]
}
```

- `health` is the first of `sitting_out` (a live node sits it out and the window
  has not ended), `no_sign_in` (no route on the answering node, or a credential
  that does not resolve), `rejected` (an auth failure newer than its last answer,
  or an expiry past), `expiring` (within seven days), `untested` (no answer, no
  failure and no call in the window) and `ready`.
- `sign_in.route` is `token`, `credentials`, `api-key`, `env`, `inherit-env`,
  `environment`, `device` or `none`, as the answering node resolves it;
  `source` is `store`, `environment` or `none`. A stored token's `expires_at` is
  its row's `stored_at` plus the lifetime its vendor documents, and
  `expiry_basis` says so: the token itself is opaque.
- `sitting_out` is the latest window across live nodes (`until`, `resets_at`,
  `window`, `said`, `since`) and the `nodes` sitting it out; `last_success` and
  `last_failure` (`kind`, `at`, `said`, `node`) are the latest across live nodes;
  `waiting` is the seats parked on a chain naming it, by handle, and since when.
- `usage` is the trailing seven days of the [spend rollup](#get-tokensbreakdown)'s per-entry
  row, in calls and tokens and never money: the dashboard renders tokens, never a
  price, and the rollup itself carries `cost_usd` for a reader that wants one.
- `used_by` is the engine's own resolution of every agent seat's jobs
  (`position` 0 runs the job, more is a fallback; `written` is whether the seat
  wrote the field; `source` the level that decided it: `seat`, `executor`,
  `default-entry`, `first-entry`), and `{"assistant": "chat" | "deep_work",
  "position": n}` for Ask Crewlet.
- `alarms` is this node's latest evaluation of the five model alarms.

**A member's reading** (`"reading": "member"`) keeps each entry's `key`,
`harness`, `model`, `health`, `sign_in.route` and `sign_in.resolves`,
`sitting_out` without `said` and `nodes`, `last_success`, `waiting` and
`used_by`, and answers `alarms: null`: nothing in it names a credential, an
account, a vendor's sentence or a cost.

#### `GET /llm/runners`

```json
{
  "default_run_in": "container",
  "cells": {"direct": {"declared": true, "ready_on": ["node-1"], "not_ready_on": []}, …},
  "nodes": [{"id": "node-1", "reported": true,
    "direct": {"state": "ready", "detail": "Landlock confinement; box root writable", "fix": null, "checked_at": "…"}, …}]
}
```

A cell `providers.sandbox` does not declare reads `unconfigured`; a refusal
carries its `fix`.

#### `GET /llm/models`

```json
{"source": "models.dev", "fetched_at": "…", "provider": "anthropic",
 "aliases": [{"id": "sonnet", "name": "Sonnet", "detail": "The harness's latest Sonnet"}],
 "models": [{"id": "claude-opus-5-5", "name": "Claude Opus 5.5", "family": "claude-opus",
   "released": "2026-…", "tool_call": true}],
 "docs_url": "https://code.claude.com/docs/en/model-config"}
```

Read from the public [models.dev](https://models.dev) catalogue, which needs no
key: no credential leaves the engine to name models, and whether a plan reaches
one is what the test call says. Claude Code reads provider `anthropic` and Codex
`openai`; OpenCode names its `provider` and is offered `<provider>/<id>`. A copy
is kept for a day and revalidated; a catalogue that cannot be read keeps the
last copy and says how old it is in `reason`, and with no copy the answer is
`"source": "none"` with `models: []` and the `reason`. `400 bad_params` for an
unknown `harness` or OpenCode without `provider`; `422 unknown_provider` for a
provider the catalogue does not list.

#### `PUT /llm/providers/{key}/credential`

```json
{"values": {"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-…"},
 "entity": {"harness": "claude-code", "model": "sonnet",
   "auth": {"mode": "api-key", "api_key": "${ANTHROPIC_API_KEY_SONNET}"}}}
```

**Tested before it is stored.** In this order:

1. The target is the active entry, or the draft `entity`: a create, or an edit
   that changes how it signs in, validated by splicing it into a copy of the
   active document as the entity route's dry run does (`422 validation_error`
   with problems under `providers.llm.<key>`). `404 not_found` for an absent key
   with no `entity`; `403 config_managed` for a draft on a managed document.
2. Every name must be a variable the entry reads (`422 validation_error` at
   `["values", NAME]`), and a value of the wrong kind by its vendor's prefix (an
   API key in a token's slot, or the reverse) is refused there too, before any
   call. The whitespace around each value is trimmed, and a value holding a
   space, a line break, a tab or an invisible formatting character inside is
   refused at the value with how many and where ("This token contains 2 spaces
   (characters 66 and 67)."): a terminal that wraps a long token turns the
   break into spaces when it is copied, and Claude Code ignores such a token
   outright.
3. `If-None-Match: *` refuses a name already stored: `412 already_exists` with
   `names`, so a create never overwrites a secret another model reads. It is
   decided again at the seal, in one step of the store's: a name another write
   stored while this one was tested is the same `412`, with `sealed` naming
   what this write stored before it, and nothing is re-activated.
   **A create is keyed** by its operation key: the `Idempotency-Key` header,
   or one minted for it, scoped by the caller as on every keyed route. Each
   row it seals carries the key. A create that stopped part way (a `500` with
   `sealed`, or a `412` on a later name) answers `op_id`. The same request sent
   again under that key re-seals the names its key sealed, within a day of
   their seal by the engine's own clock (never the time the key carries, which
   its caller writes), with the values it sends now, and creates the rest. Any other create naming them
   is `412`, whatever value it sends: the store is never read for a value, so
   no answer says whether a guess equals what is stored. The key is in no
   other answer, listing, log or audit row.
4. When the write will re-activate the company, `config.write` is asked before
   anything is tested or sealed.
5. The candidate values are handed to the model's own harness, in an isolated home,
   for one one-turn call, within the entry's `timeout_seconds` capped at 120 s.
   A refusal is `422 credential_rejected` at the value, and nothing is stored.
   Its `detail` says what the failure meant rather than quoting the harness: the
   vendor refused the credential (wrong, expired or revoked, with the command
   that makes a new token where the harness has one), or the harness never used it, so
   the vendor never saw it (the profile's `auth_markers` `meaning: unread`).
   The harness's own words stay on the check's lines, and a failing line carries
   `meaning` (`refused` or `unread`) beside its `kind`; a vendor that did not answer is
   `502 credential_untested`; a harness that cannot run (no binary) is
   `422 validation_error` at `providers.llm.<key>.harness` with the doctor's
   remedy. A `429` passes: it proves the credential authenticated, and the
   answer's `limited` says so, `{"until", "resets_at", "window", "said"}`, with
   `until` null, since a test sits nothing out, and `resets_at` the vendor's
   reset when it named one.
6. With `dry_run=true` the answer is `200 {"ok": true, "verified", "hints",
   "check", "limited"}` and nothing is stored.
7. Each value is sealed (source `models`), and the revision that reads any of
   them is [re-activated](#a-secret-write-re-activates-the-revision-that-reads-it)
   once, held to `If-Match` when the caller sent one. From here the write
   finishes even when the caller disconnects. A seal that fails part way is
   `500 internal_error` with `sealed`, the names stored before it, and
   re-activates nothing; send the values again.

```json
{"verified": "Answered a one-turn Claude Code run in 3.4 s with only this value.",
 "check": {"at": "…", "node": "node-1", "ok": true, "lines": [{"check": "binary", "ok": true, "detail": "…"}]},
 "names": ["ANTHROPIC_API_KEY_SONNET"], "hints": {"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-…7d2K"},
 "entries": ["sonnet"], "reload": {"revision_id": "…", "epoch": 42},
 "cleared": ["sonnet"], "running_runs": 1, "limited": null}
```

`entries` is every model that reads one of the names, which all sign in with it
now; `cleared` the ones this node had sitting out on the sign-in it replaced, which
start clean; `running_runs` the executor runs still running on them, which keep
the old credential until their segment ends (`null` when it could not be
counted). A re-activation that raced another write answers `200` with
`reload.error: "revision_advanced"`: the values are sealed and stay.

#### `POST /llm/providers/{key}/check`

`{"smoke": true}` checks the active entry, `{"smoke": true, "entity": {...}}`
a draft, which resolves its `${VAR}`s through the store as an apply would. The
body carries **no credential value**: a typed credential is tested by the
credential route's dry run. The answer is `{"at", "node", "ok", "failure",
"lines": [{"check", "ok", "detail", "kind", "remedy"}]}`, one line per check
(`binary`, `version`, `sign-in`, `usage`, `smoke`, `shell`, `web`, `executor`),
within 180 s. A check of an existing entry is a real call on its sign-in, so its
outcome is remembered as any call's is. `404`, `422 validation_error` for a
draft.

#### `POST /llm/providers/{key}/retry`

`{"mode": "probe"}` makes one call within 60 s on the answering node and, if
it answers, clears the sit-out on every node; a limit met again is
`{"cleared": false, "still": {"until", "resets_at", "window", "said"}}`, and
nothing is cleared; a probe that failed for any other reason (a refused sign-in,
a vendor that did not answer) clears nothing either and says why in `failed`,
present only then, so the person tests the model rather than retrying again. `{"mode": "clear"}` clears it at once, and every chain
parked on the model probes again. `200 {"cleared", "mode", "node", "still",
"failed"}`;
`404`; `409 not_sitting_out` for a probe of a model no live node sits out.

#### `POST /llm/providers/{key}/host-login/check`

Runs the harness's own status command on the answering node:
`{"node", "state": "signed_in" | "signed_out" | "unknown", "method", "account",
"checked_at", "command": "crewlet llm login <key>", "shares_login_with": [...]}`.
`shares_login_with` is every model on the same harness and login directory, which
one login signs in. It never signs anything in: `command` is what to run on that
node. `404`; `422 no_host_login` for a model that signs in with a token, a key
or its env.

#### Device sign-in

`POST /llm/providers/{key}/device-sign-in` (body `{}`) starts the harness's
device-code sign-in on this node and answers `201` as soon as the harness printed
its code: `{"id", "entry", "node", "url", "code", "status": "pending",
"started_at", "expires_at"}`. The person approves at the vendor's page; nothing
is pasted back. On approval the node seals the login as the model's credential
bundle and re-activates, so every node restores it, and the record's `reload`
names the revision. `GET /llm/device-sign-ins/{id}` reads it (`status`
`starting`, `pending`, `approved`, `failed`, `expired` or `cancelled`, a
`detail` on failure) until ten minutes after its code expires, then `404`.
`DELETE /llm/device-sign-ins/{id}` cancels it: `200 {"id", "status":
"cancelled"}`, forwarded to the node running it.

Refusals: `404`; `409 sign_in_in_progress` with the `running` session;
`422 no_device_sign_in` for a harness with none or a model whose sign-in reads no
login (an API key or its env); `502 device_sign_in_unavailable` with the harness's
first line when it printed no code in time.

### Concurrent writes

**There is no leader** — any node's API can write the config, and the activation pointer in the coordination KV is the shared truth. Writes are **not** serialized by a lock, but a concurrent one is *detected*: the activation is a compare-and-set.

- Every write on this surface reads the fleet's revision from the pointer, derives from it, and names it as the new revision's parent. The activation lands only while the pointer still names that parent, so a write that lost is refused **`revision_advanced`** — **whether or not the caller sent `If-Match`**, because the server knows what it read. A pointer removed since the read — deleted, or lost with a rebuilt coordination store — refuses it the same way: what the write was built on is gone, so read `/config` again and rebuild the edit. The status says which kind of write lost, as HTTP has it (RFC 9110 §13.1.1): **`412`** for one whose `If-Match` named a revision, since the condition the request stated is false, and **`409`** for one that named none, or sent `If-Match: *` — whose condition, that a company exists, is still true of the company that won. A write whose `If-Match` is another resource's tag (a charter write's, a per-entity write's), and a reload or a revert, which read none, name no revision and lose as `409`.
- `If-Match: <revision_id>` is still worth sending: it is checked before any work is done, so a caller editing a revision that has already moved is told so without a document being built, validated and stored first.
- A losing write's revision **is kept**, and the `409` (or `412`) names it as `stored_revision_id`, beside the **`node`** that served the write. It is stored in that node's history alone, valid and inert, so the operator's work survives as history they can revert to — on that node: only an activation naming it would have carried it to the others, so `GET /config/revisions/{id}/diff` and `POST /config/revisions/{id}/revert` reach it there, by its own address where a load balancer or a shared Service fronts several, and any other node answers `404` for it. Inert on every node: a write stores its revision as history and moves the pointer, and no node boots on a revision until its own engine has applied it, so nothing offers the loser to the fleet at a restart. Unwinding it instead would mean a second write that can itself fail, on the path where something has already gone wrong.
- **What `/config` answers and what a node runs are two facts.** `/config` answers the fleet's revision on every node. A node runs the revision its engine last applied, which trails the pointer by an apply and stays behind it on a node that refused the fleet's revision — a missing environment variable, an MCP binary its image lacks. Such a node keeps running the revision it served, and if it restarts comes back on the revision its boot row names; its row in [`GET /fleet`](#get-fleet) names the revision it refused, the epoch it last applied (`config_applied_epoch`), the revision it runs (`config_serving_revision_id`) and the one it would boot on (`config_boot_revision_id`) — the same revision as the one it runs, except while a write to its boot row is being retried, and absent on a node whose own database is discarded at every start (`store.scratch`, so every node without the `data` role), which comes back on nothing and applies the fleet's revision from scratch. Every write through it is still built on the fleet's revision and lands, so the write that corrects the refused revision can be sent to any node, that one included.
- **With no pointer, every write is a create.** When the coordination store holds no activation — a new fleet, or one whose pointer was lost under it, such as an external NATS cluster rebuilt or its bucket removed — `/config` answers the company this node knows: a change an offline command made on the fleet's revision as this node last saw it, or else that revision, refused here or not, or else the revision it boots on (see [Control Plane](../concepts/control-plane.md#a-nodes-own-store-keeps-three-facts-apart)); `404 no_active_revision` when it knows none. Every write is then a **create-only compare-and-set**: it lands only while the fleet still has no activation, and the first to land is the fleet's pointer again. A node that joined a running fleet and has not reconciled yet reads the same pointer every node does, so it answers the fleet's company, and `If-None-Match: *` there is `412 already_configured`. A write that replaced a running company would rename it, change every seat id derived from the name and orphan all of their memory.
- **The boot publish is a compare-and-set too.** A starting node publishes at most one revision: a change an offline command made on it (`crewlet config import`, `activate` or `rekey` while its engine was stopped) only while the fleet still runs the revision that change was built on, expecting the very activation of it the start read — or the earlier offline change it replaced, where a start published that one and could not confirm it — or, with no pointer, when that is the revision this node last saw the fleet run; otherwise, over a lost pointer, or a pointer another node **restored** naming a revision this node holds that was activated before the one it last saw, the fleet's revision it last saw (having seen none, the one it boots on). A pointer somebody chose is the company however old its instant reads, and no boot replaces it. One that loses re-reads the pointer and decides again, so a write that lands while a node starts is never replaced. An offline change the fleet has moved on from is withheld — never published over the fleet — and the node logs `offline_revision_not_published` at warn: at every start while the change is what it boots on, and once more when it applies the fleet's revision, which replaces the change as what it boots on and keeps it as history. Its hint names the change rather than the document: `GET /config/revisions/{id}/diff?against=<built_on>` on that node shows what the offline change did, to make again on the fleet's document; `POST /config/revisions/{id}/revert` there puts it up as it stands, undoing what the fleet changed since. See [What a booting node publishes](../concepts/control-plane.md#what-a-booting-node-publishes). The one unconditional activation is `crewlet run -import-company`, which replaces whatever the fleet runs with its file.

On either, re-read `/config` and send the edit again.

### Status codes

- `200 OK`: a successful read, or a [dry run](#dry-runs) that found the write valid (`{"valid", "base_revision_id", "warnings", "derived"}`), or a [charter](#the-charter-mission-vision-and-policies) write (`{"charter", "changed", "revision_id", "epoch"}`), which replaces a resource rather than creating one
- `201 Created`: a write produced a new revision; the body is `{"revision_id", "epoch", "warnings", "derived"}` (see [What a write answers](#what-a-write-answers)). A per-entity write, a reload and a revert return this too: each created one revision.
- `400 Bad Request`: `invalid_body`, `invalid_patch` or `validation_error`, each with `detail` (the field path and what to change) and [`problems`](#refusals-carry-located-problems); `summary_required` when a write has neither an `X-Summary` header nor a `_summary` body key — asked last, once the write is admitted and its document checked, so a caller who may not make the change is answered the `403` first; `invalid_query` when `dry_run` is anything but `true` or `false`; `identity_mismatch` when a per-entity body renames what the path addresses; `conflicting_preconditions` when a write sends `If-Match` beside `If-None-Match`
- `401 Unauthorized`: missing or invalid bearer token — `invalid_token`, in the same [refusal envelope](#every-refusal-is-one-envelope) every route answers with, written by the guard itself before any route runs
- `403 Forbidden`: `unauthorized` when the credential holds neither the grant nor, on the org chart, the lead relation — its `reason` the rule that refused the write, with a `refused` list for [a lead's write](#a-lead-edits-their-own-team) — and `step_up_required` for a write whose proof of identity is older than `step_up`, whether the route or the write's parts found it; and `config_managed` when `api.auth.company_writers` names the document's writers and this credential is not one of them — a person's session, a machine token and a lead's write among them — with `managed_by`, `detail` and `hint`, a write and its dry run alike, never a read or a reload, and asked before the body is read (see [Managed configuration](../concepts/configuration.md#managed-configuration))
- `404 Not Found`: a revision that is not there, `no_active_revision` on a read when nothing is configured (the fleet has no activation and this node knows no company), `no_such_entity` on a per-entity read or write naming an id the active revision does not carry (of a seat or a unit, told only to `admin`, and anybody else is refused `403` as for one outside their team), or `no_route` for a path under `/config` this surface does not serve
- `405 Method Not Allowed`: `method_not_allowed` for a `/config` path under a method it does not take, with `Allow`
- `409 Conflict`: `revision_advanced` (a race with a concurrent writer or with the pointer's removal, for a write that named no revision in `If-Match`) or `no_active_revision` (a `PATCH`, a charter write, a per-entity write or a reload when nothing is configured); `seat_held` when a write takes away a human seat somebody holds or is invited to (see [What a write answers](#what-a-write-answers)); and `unreadable_revision`, `unsealed_revision`, `unreadable_fleet_revision`, `unsealed_fleet_revision` or `unknown_seal_revision` when the revision a read or a write needs does not open on this node (see [A revision this node cannot open is refused, whichever route reads it](#a-revision-this-node-cannot-open-is-refused-whichever-route-reads-it)). A `revision_advanced` from a write that lost at its activation names the revision it kept as `stored_revision_id` and the node holding it as `node` — see [Concurrent writes](#concurrent-writes)
- `412 Precondition Failed`: `revision_advanced` when `If-Match` names a revision that is not the fleet's, or stops being so before the write is activated, naming the revision it kept and its `node` as the `409` does where it lost at its activation; `already_configured` when `If-None-Match: *` meets an active revision — naming, for a create-only write that lost at its activation, the revision it kept and its `node` as the `409` does — `no_active_revision` when `If-Match` names a revision and none is active, or `version_moved` when a charter write's or a per-entity write's `If-Match` names a charter or an entity that has changed, carrying it now as `current` and its `etag`
- `428 Precondition Required`: `bad_if_match` for a charter write with no `If-Match`
- `415 Unsupported Media Type`: `unsupported_patch_media_type` when a `PATCH` body is a patch format other than a JSON Merge Patch, with `Accept-Patch`
- `503 Service Unavailable`: `unavailable` when the coordination store cannot be read, so the fleet's revision cannot be either — on every write and on every read that needs it — with `Retry-After: 2` and a `reason`; `unavailable` for any other condition of the node (a store too busy with other writes to take a write's revision, one closed while the node adopts a snapshot or shuts down, a state-log refusal behind a read the route makes, a request out of time), with a `detail` in a sentence for the person reading it and a `Retry-After` exactly where waiting clears it; and `unavailable` with **`"outcome": "unknown"`** when a write stored its revision and the coordination store did not answer the activation, which may have landed: the body names the stored revision as `stored_revision_id`, the revision the write was built on as `your_base` and the node that stored it as `node`, and carries no `op_id`, because what says whether it landed is the pointer — `GET /config` names the stored revision or not once the store answers. Sending the write again makes another revision, over this one if it landed. If it did not, the stored revision is in the history of that node alone — only an activation naming it carries it to the others — so `GET /config/revisions/{id}/diff` shows it and `POST /config/revisions/{id}/revert` makes it the company **on that node**, reached by its own address where a load balancer or a shared Service fronts several; any other node answers `404` for it. Every write here can answer it — `PUT`, `PATCH`, a per-entity or charter write, a reload and a revert — and so can a [`/setup`](#supplying-them) submission. `seat_holders_unavailable` when a write that takes a human seat away cannot read the identity directory to see who holds it: nothing was written, `seats` names the seats (none for a whole document [replacing an unsealed revision](#a-whole-document-replaces-an-unsealed-revision), which asks about every seat it does not keep), and a `Retry-After` comes where waiting clears it. `draining` when the node has been told to stop, with a `Retry-After` — see [During a drain](#during-a-drain)

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

The records are the revisions **this node** holds, and two fields are about the
fleet rather than about the revision:

- **`is_active`** marks the fleet's revision, the one the activation pointer
  names — the same row on every node that holds it, whatever the node answering
  runs. A node that refused the fleet's revision marks the fleet's, not the one
  it still runs; what each node runs is [`GET /fleet`](#get-fleet)'s.
- **`activated_at`** is the latest activation of the revision this node has
  met, on every row alike, the revision it boots on included: raised by each
  activation of it this node's reconciler sees on the pointer and by each the
  node publishes itself, and never lowered. It is what the [retention
  sweep](../guides/retention.md#the-configuration-archive-and-the-one-thing-a-purge-cannot-reach)
  dates an old revision activated again by. It is **not** the activation a node
  applied: a node that refused a re-activation of the revision it runs — the
  [rotation](../concepts/control-plane.md#rotation) gesture, on a node that lost
  the credential — reports the fleet's newer instant all the same. What each
  node runs is [`GET /fleet`](#get-fleet)'s, and the instant its engine applied
  is the store's `served_at`, which this answer does not carry (see [A node's
  own store keeps three facts
  apart](../concepts/control-plane.md#a-nodes-own-store-keeps-three-facts-apart)).
  Absent on a revision nothing has activated, such as one that lost a race or
  an offline import not yet published.

`created_by` is the revision's author, `created_by_kind` its kind and
`operator_id` the credential it was written through — see [who a write is
attributed to](#who-a-write-is-attributed-to) — on this answer, on
`GET /config/revisions` and on `GET /config/revisions/{id}` alike. The kind is
one vocabulary across every trail the engine keeps (`agent`, `human`,
`operator`, `system`):

| `created_by_kind` | Who wrote the revision | `created_by` |
|---|---|---|
| `human` | A person — every person holds a seat — or a Tier A token or service account the identity directory binds to one, on `/config` or `/setup` | The seat's handle |
| `operator` | A credential nobody is bound through — a Tier A token or a service account bound to no seat — on `/config` or `/setup`; or whoever ran `crewlet config import` / `crewlet config rekey` on the host | The credential's login (`token:<id>` for a Tier A token), or the host login (`$CREWLET_OPERATOR`, else `$USER`) |
| `system` | The engine itself: a node seeding the store from its `-company` file at boot, or the reconcile loop's own writes (removing a disconnected integration, recording a discovered site, reloading after sealing a credential) | The node's id for a seed; `reconcile loop` for the loop |

`operator_id` is empty where no credential made the write — every `system`
revision and every host-side CLI write. Read the kind rather than inferring
it from the label: the name spaces overlap. A revision reads the same on every
node: the fleet's activation pointer carries its author, kind and credential
beside its source and creation instant, so a node keeping a copy of it records
the origin's rather than its own (see [Control Plane](../concepts/control-plane.md#the-design)).
A kind a newer engine adds arrives as itself.

### `GET /config/revisions/{id}/diff`

A **structural** diff of two revisions — one entry per path that moved, with
the value on each side. `?against=` names the other side and defaults to the
fleet's revision (`active`); the direction reads as "what `against` became", so a bare
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
fleet's document** and activates it over the fleet's revision, which advances
the epoch and makes every node apply again. Whichever node it is sent to, it
re-publishes the fleet's revision — on a node that refused that revision too,
never the one the node still runs.

That is the gesture a rotated credential needs. A secret lives in the company
config as a `${VAR}` pointer, resolved when a provider or a transport is
constructed, from a snapshot taken at apply time: the pointer is already
correct, so there is no patch to make, and with no activation there is no apply
and no refreshed snapshot. A write through `/secrets` of a name the active
revision reads performs this same reload itself
([above](#a-secret-write-re-activates-the-revision-that-reads-it)); the route is
for a value written any other way (with the node's engine stopped, on a store
an external broker holds) and for a write whose own re-activation answered an
error.
Re-activating an unchanged revision is exactly why the activation pointer is
append-only rather than keyed on a revision id.

A new revision rather than a re-pointed old one, for the same reason a revert
writes one: the history stays append-only, so "the credentials were reloaded
at 04:12" is a fact somebody can find later. `X-Summary` names it; unset, it
records `reload configuration`.

Answers `201` with the revision, its epoch and its warnings (see
[What a write answers](#what-a-write-answers)),
`409 no_active_revision` when nothing is configured, `409 revision_advanced`
when another write moved the pointer between the reload's read and its
activation, and `400 validation_error` when the active document breaks a
runnable rule of this build (a reload is an apply, so it re-publishes only a
company every node can run; correct it with `PUT` or `PATCH`). A document that
breaks only an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to),
such as two sandbox setup steps of one name a newer peer admitted, reloads: that
is how a credential rotation still reaches a company carrying one. Its
answer lists each violation as an `admission` warning.

On a **stopped** node the same gesture is
[`crewlet config activate <UUID>`](cli.md#crewlet-config-activate) naming the
company's own revision, the one [`crewlet config revisions`](cli.md#crewlet-config-revisions)
marks `fleet's`. It opens the node's own store, so it cannot run beside that
node's engine, and makes an offer: the node publishes it at its next start as a
new activation of the same revision, but only while the fleet still runs the
revision the offer was built on (or, with no pointer, while that is the
revision this node last saw the fleet run). If the fleet has moved on
meanwhile, the offer is withheld and the node says so at warn, as
`offline_revision_not_published`; once it applies the fleet's revision it runs
that one, and boots on it once its boot row has moved, and the offer stays in
its history. A running fleet
takes this route.

**Open on a managed document.** A reload changes no byte of the company, so
[`api.auth.company_writers`](../concepts/configuration.md#managed-configuration)
does not refuse it: it is how a credential somebody rotated in an emergency
reaches the running seats when the system that manages the document cannot
know it has to.

#### Stored revisions are read as they are

A read never validates what it reads. `GET /config`, a revision read, a diff,
the reference index, the entity reads and the prior a write restores its masks
from all open a stored revision as it is, even one this build's validator would
refuse: a revision is valid under the build that wrote it, and a later build
can leave one in the store that this build refuses. What validates is whatever would RUN a
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
revert and a write whose prior it is (but for the one write
[below](#a-whole-document-replaces-an-unsealed-revision)), opens the revision
under the node's keyring first, and a revision that does not open is answered
`409` naming it (`revision_id`) under one of five codes: three causes, the first
two each met in this node's own copy or only through the fleet's pointer, and
no two of them share a remedy:

- **`unreadable_revision`**: it is sealed under a key the keyring no longer
  holds. `hint` says to put that key back in the node's `secrets.keys`.
- **`unsealed_revision`**: it was stored **without a seal**. A node reads only
  sealed revisions, because the seal is what says a node of this fleet wrote
  one; a plaintext revision could have been written by anything that reached
  the store, so it can't be shown, compared, edited or reverted to. `hint` says
  what replaces it: an admin sends the whole document again from your own copy
  with `PUT /config`, or imports it with `crewlet config import`. Either one
  stores it sealed (see [below](#a-whole-document-replaces-an-unsealed-revision)).
- **`unreadable_fleet_revision`**: the fleet's revision, which this node has
  met only through the pointer and holds no row for, is sealed under a key
  this node was never given. The deployment holds the key; `hint` says to add
  it to this node's `secrets.keys` and restart it.
- **`unsealed_fleet_revision`**: the fleet's revision arrived on the pointer
  **without a seal**. Every node seals what it activates, so it came from
  outside the fleet; `hint` says what replaces it: an admin sends the whole
  document with `PUT /config`, or imports it with `crewlet config import`.
  Either one stores it sealed and activates it.
- **`unknown_seal_revision`**: it is **sealed in a form this build does not
  read**, this node's own copy or the fleet's, most likely by a newer build of
  Crewlet. It is not unsealed: it is somebody's document, so no write replaces
  it, `PUT /config` included, and `hint` names the build that sealed it, which
  reads it: upgrade this node to the build the rest of the fleet runs, or run
  that build on this node again where this node's own copy came from it.

`detail` says what happened in words, and the keyring's own error goes to the
node's log. The dashboard's config questions answer the same five codes, on
the socket and on [`/query`](#live-stream), with `detail` and `hint` as one
sentence on the frame's `detail`, and so does a [`/setup`](#supplying-them)
write that meets one, naming what it sealed before it as `wrote_secrets`.

These used to be `500 internal_error` everywhere but a revert, and a revert to
an unsealed revision answered `unreadable_revision`, whose message tells the
caller to put back a key that was never involved.

#### A whole document replaces an unsealed revision

`PUT /config`, with or without `?import=true` and as a `?dry_run=true` check,
is accepted over an active revision stored without a seal, and reads
**nothing** from it, exactly as an offline `crewlet config import` does. It is
still the revision the write is built on: `If-Match` and `If-None-Match` are
judged against it as against any active revision, and the activation is
conditioned on it. Reading nothing means:

- **No mask is restored.** A credential still holding the redaction marker
  has no value behind it, so it is refused `400 validation_error`, the problem
  naming the field and saying to write the real value or a `${VAR}` reference.
  Every other problem the document has is reported beside it.
- **Nothing a newer build wrote is carried back**: [that
  rule](#fields-a-newer-build-wrote-survive-every-write) holds only for a
  revision that opens.
- **It is judged as a document replacing nothing.** Against nothing a write
  changes every setting, so only `admin` replaces one, and a lead is refused
  `403 unauthorized`, `refused` listing every part outside their team, the
  company's settings among them. The charter's rules are judged as for a first
  write, which warns, and `?import=true` keeps its own rules.
- **Every held seat is kept.** With no revision to say which seats the write
  takes away, it asks the identity directory about every seat its document does
  not keep as a human seat, and one somebody holds (a person at any stage short
  of removal, a service account, an open invitation) is refused `409
  seat_held` as a [removal](#what-a-write-answers) is. Its `detail` names the
  seats the document doesn't keep, a seat no revision names any more included,
  and its `hint` offers keeping each seat in the document as a human seat
  first: freeing one by moving a person to another human seat needs a company
  running on the node, which one whose active revision is unsealed may not
  have. A node that cannot read the directory refuses it `503
  seat_holders_unavailable`, naming no `seats`.
- **A body the fleet's pointer carried is not kept.** Where the unsealed
  revision is this node's own copy, the new revision names it as its parent,
  as the offline import does. One met only through the pointer never enters
  this node's history (the reconciler never keeps one either), so there the
  new revision names no parent, and its activation is still conditioned on it.

Every other write (`PATCH /config`, a per-entity `PUT`, a revert, `PUT
/config/charter`, a `/setup` submission, `POST /config/reload`) is built from
the active document and is refused over an unsealed one with the same `409`.
Over a revision that is sealed but does not open here, under a key the keyring
does not hold or in a form this build does not read, every write is refused,
`PUT /config` included: giving the node that key, or the build that sealed it,
recovers the document whole, and a replacement would discard every credential
and field in it.

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
  handle and a unit by its key anywhere in the document, so a seat moved to
  another unit keeps its settings; an MCP server or a sandbox setup step by its
  name within its own list. An identity held twice in the stored document, or
  empty, matches nothing, so no member's setting reaches another. A member of a list
  with no identity keeps nothing the write replaced.
- **A `PATCH` replaces only what it names.** Everything it does not name is
  stored exactly as it was, so a list the patch leaves alone keeps every
  member's settings, identity or not, and a member with no identity loses a
  newer build's settings only when the patch replaces the list that holds it.
- **A reload and a revert store the document exactly as it was stored**, since
  neither changes it.
- **A renamed MCP server or setup step is a new identity**, so it keeps
  nothing of the old one's unknown settings.

### `/artifacts/{id}/data/*`: an artifact's datastore (auth-gated)

An artifact page published with the `db` capability keeps its rows in the
artifact's **datastore**: named collections of JSON documents, each collection
declaring its columns (checkbox, chip, select, person, status, text, long
text, number, date) and its rules (who reads its rows, who writes them, which
fields only a person may set, which are written once). Every route here is
decided **per artifact**: the caller's role on it, through the owner's own
standing or a share, is read from the artifact's head on every request and
stamped into the write, and the applier decides the write from that stamp and
the store's rows alone, so every data node reaches the same verdict. An
artifact the caller has no relation to is answered `404` `not_found`, exactly
as one that does not exist.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/artifacts/{id}/data/{collection}` | One page of a collection's documents (`limit`, 100 by default and at most 500, and the `cursor` a page answered as `after`), each with its fields, who created and last changed it, and its `version`. `since=` a version answers only what changed after it, deletions included, which is the catch-up a page runs after a reconnect. Each page answers `through`, the `since` of the next catch-up, which a client adopts only once the read's last page (the one with no `after`) is in: a whole read's pages answer the version its first page was read at, so a document that changed under a page already read is caught up rather than skipped. A catch-up from before a read rule changed, a collection was dropped with its deletions, or a row changed hands in an `own_rows` or `private` collection answers `resync` (read whole). The query `artifact_data` |
| `GET` | `/artifacts/{id}/data/{collection}/{doc}` | One document. The query `artifact_data_doc` |
| `GET` | `/artifacts/{id}/data/{collection}/query` | `q=` a query as JSON: up to 16 filters (`eq`, `ne`, `lt`, `le`, `gt`, `ge`, `in`, `contains`, `prefix`) on declared columns, ANDed, an `order_by` and a `limit`, compiled to the field index rather than run over the rows. Every top-level field is indexed, and a nested path (`a.b`) once the collection lists it in `indexed`; a filter or an order on any other path is `400` `bad_params`, naming it. The query `artifact_data_query` |
| `GET` | `/artifacts/{id}/data/history` | The store's last seven days of writes, newest first, narrowed by `collection`, `doc`, `field` (one cell's history) or `actor`: who wrote, through which surface, the verdict, each changed field's value before and after (a value over 4 KiB as its hash) and whether it can still be undone. In an `own_rows` or `private` collection each change is shown by the row's creator when it was made, never its creator now, so a row deleted and created again by somebody else shows them only their own changes. The query `artifact_data_history` |
| `GET` | `/artifacts/{id}/data/schema` | The store's collections, its footprint against its quota, its state digest and the caller's role. A collection only editors may read is left out for anybody below editor. The query `artifact_data_schema` |
| `GET` | `/artifacts/{id}/data/state` | The caller's own per-viewer state keys on this artifact, and nobody else's. The query `artifact_data_state` |
| `GET` | `/artifacts/data/usage` | The company's datastore footprint against `artifacts.limits.data_bytes` *(needs `admin`)*. The query `artifact_data_usage` |
| `POST` | `/artifacts/{id}/data/writes` | **One atomic batch**: `{"ops": [...]}`, 1 to 100 ops of `set`, `merge`, `delete`, `increment`, `append`, `add` and `remove`, at most 1 MiB, applied all or none. An op carrying `if_version` is refused as a conflict when its document moved since; one without it is a blind write, and where two blind writes meet, each field keeps the value the log ordered last |
| `POST` | `/artifacts/{id}/data/undo` | `{"seq": N}`: reverts one write of the last seven days, as a write of the caller's own under the same rules, conditioned on every document it changed being where it left them, so a document changed since is `409` `stale_version` naming it. A write older than the history, one that was refused, a schema change, one holding a value the history kept only as its hash, and one whose undo would restore more than one write carries (a bulk delete of large rows, whose inverse is over 1 MiB) are `409` `not_undoable`, and the detail says which. The history's `undoable` is false for every one of them, so a client never offers an undo that cannot land |
| `POST` | `/artifacts/{id}/data/state` | `{"key", "value"}` or `{"key", "delete": true}`: sets or removes one of the caller's own per-viewer state keys, the backing of the page runtime's state API |
| `PUT` | `/artifacts/{id}/data/schema` | `{"collections": [...]}`, every collection the store keeps after the change *(editor and above)*. Decided against every write before it; a change that would rewrite more documents than one record carries is `422` `migration_too_large` |
| `GET` | `/artifacts/{id}/data/export.csv` | `collection=` one collection as CSV, streamed a page at a time *(editor and above)*, under the collection's read rule, written for a person to read: each column headed by its label (its key where it has none), choices as their labels, a person as the seat's name, the row's attribution under `ID`, `Created by`, `Created at`, `Updated by`, `Updated at` and `Version`, and any cell a spreadsheet would run as a formula written as text. An artifact in the trash is `409` `artifact_trashed`, and a purged one's store `410` `data_gone` |

Every read here is a read of the artifact's content: an artifact in the trash
answers none of them (`409` `artifact_trashed` on the routes and the
questions alike, to a reader it is disclosed to), and an administrator
only the admin path admits is recorded as reading it before a row is sent, or
answered `503` with nothing when that record cannot be written. Every write
route takes an `Idempotency-Key`, refuses a field it does not declare, and
refuses a write to an artifact in the trash (`409` `artifact_trashed`, as every
artifacts route answers the trash — `410` `data_gone` is a purge's) until it is
restored. A write from a browser session states the surface it
came through as `via`: `dashboard`, or `bridge` with the `version` of the page
whose script made it, which must declare `db` (or `state`, for a state key) or
the write is `403`. A bearer credential's write is stamped as the API's
whatever it says.

**A write answers its verdict**, read back from the node's own rows once it
has applied the record. `verdict` is the one field that says how the write
ended, beside `op_id`, `position` and, once decided, the `seq` an undo names;
a refusal carries it next to its `error`:

| Status | `error` | When |
|---|---|---|
| `200` | | `verdict: "applied"`, with the write's `seq`, which an undo names |
| `202` | | `verdict: "pending"`: the record landed and this node has not applied it yet, so its verdict is on the `position` answered |
| `409` | `stale_version` | An `if_version` that no longer matches; `conflict` names the documents |
| `422` | `invalid` | A value outside its column's type, a field the collection does not declare, a long text written whole without its guard |
| `403` | `forbidden` | The caller's role, or the column's rule, does not let them write it |
| `409` | `quota_exceeded` | The write would take what is kept past a limit on it: the store's or the company's footprint (a Tier A setting an operator raises), a collection's rows or one participant's rows (caps its schema sets), or the collections and documents one store holds. `detail` is the applier's reason, which names the limit. A write that shrinks the store is never refused for it |
| `410` | `data_gone` | The store was purged |
| `429` | `write_budget` | This node's write budget for the caller on this store, or for the store, is spent. Weighed by the rows a batch's apply writes, which are each named document's whole field index rather than the fields the batch names, in units of sixteen rows (20 units a second with a burst of 100 per caller and store; 40 a second with a burst of 100 per store), with `Retry-After`, `scope` and `retry_after_ms` saying when it holds the cost again. Per node, never a lockout |

Every refused write is still on the log and in the store's history with its
reason, applied identically on every data node, so `GET .../history` answers
what was refused as well as what landed.

## Setting an integration up

Connecting an integration means putting values in two places: a credential
into the fleet's sealed secret store, and everything else into the company
document. `/setup` is the surface that does both, in the one order that is
safe, so the dashboard never has to sequence it and never holds a credential
across two requests.

**Every route takes a grant, reads included**, on the same terms as `/config`
and `/secrets`: this surface answers with the *names* of the credentials a
company holds, which of them are unset, and the pages at each third-party app an
administrator would visit. That is a map of what to attack. Reading and
connecting both take `admin` — see [what each route
takes](#secrets--the-companys-credentials-auth-gated).

**On a managed document** (Tier A `api.auth.company_writers`), a submission
that would write a pointer into the document, a disconnect, and the begin of
an agent's GitHub App answer a credential the list does not name
`403 config_managed` — the [same refusal `/config` answers](#config--live-config-management-auth-gated)
— before a value is sealed, a teardown is queued or GitHub is asked for
anything. A submission that only rotates a value the document already points
at is not a change to it and is served, ending in a reload as always; so are
the provisioning pass and its read-only check. A provisioning pass any
operator starts may still record what it discovered into the document — where
`integrations.jira` or `integrations.confluence` has no `cloud_id`, the
Atlassian pass records the site's `cloud_id` and `site_url` — and that revision is
written as the node (`reconcile loop`), not as the credential that started
the pass. A GitHub App creation carries
the operator who began it in its signed state, so the callback records the
app — its revision and its sealed key — as that credential, and asks again
before it exchanges GitHub's one-time code.

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

`checkable` means the credential can be [checked at its
vendor](#checking-a-credential-before-it-is-stored) before it is stored, so a
form offers a Test button for it. A `secret` that cannot be checked ahead
carries `check_note` instead: the sentence saying when the vendor proves it. A
webhook signing secret is the usual case, since the vendor never presents it
and only signs its deliveries with it.

`check_with`, on a `checkable` field only, names the other fields of the same
list that one check of it is made with, for a vendor that accepts or refuses
several values together. A form offers a single Test control after that
field, enabled once it and every field named holds a value, and sends them in
one request. A credential named there is not `checkable` on its own, and its
`check_note` says where it is tested. Datadog is the one surface that uses it
today: `app_key` carries `"check_with": ["site", "api_key"]`. Absent, a field
is checked on its own.

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
   list was read against — the fleet's, which `GET /setup/integrations` answers
   as `revision_id` on every node, whatever that node runs. A submission built on an older one is refused
   *before anything is sealed*, so a caller working from a stale page does not
   end up with a credential in the store that nothing points at. Without
   `if_match`, the submission is built on the revision this request read the
   fields from, in the same read: what each field holds — empty, a `${VAR}`, a
   literal — decides the write, so it lands on that revision or is refused
   `409 revision_advanced`, never applied to a document a write landing in
   between changed.
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
   when that is what happened. The reload is held to the base of step 1 as
   the patch is, up to its own compare-and-set: a write that lands between
   the read and the reload — one that put a literal in the slot, or the
   company from before the integration was connected — is `409
   revision_advanced`, never a reload of a document that does not read the
   value just sealed.

Answers `201 {"revision_id", "epoch", "wrote_secrets", "reloaded", "state"}`.
Refusals: `400 invalid_input`, `400 validation_error`, `404 unknown_kind`,
`409 revision_advanced` (the body's `if_match` is stale), `409 literal_in_config`,
`409 no_active_revision`; `409` under one of the five codes for a revision this
node [cannot open](#a-revision-this-node-cannot-open-is-refused-whichever-route-reads-it),
naming what the submission sealed before it as `wrote_secrets`;
`503 unavailable` with a `Retry-After` when the coordination store cannot be
read, or did not answer a credential's own write in step 2 — the body then
names that credential as **`unanswered_secret`**, since its write may have
landed — with a `detail` in a person's words for any other condition of the
node (a store too busy to take the revision, one closed for a snapshot, a
request out of time) and a `Retry-After` exactly where waiting clears it, and
with `"outcome": "unknown"`, `stored_revision_id` and `node` when the document
was stored and its activation went unanswered, as on [the config
surface](#status-codes). A `409 revision_advanced` from a write that lost at its
activation names the revision it kept, inert, as `stored_revision_id`, beside
the `node` whose history alone holds it.
**A refusal after the first credential is sealed names what it sealed** as
`wrote_secrets` — a lost race, an unanswered activation, an unreadable pointer
and a later credential's failed write alike, a rotation's reload included:
the credentials are in the store whatever became of the document, nothing may
point at them yet, and a submission sent again overwrites them by name. So a
`503` **without** `outcome` is sent again as it stands, whatever
`wrote_secrets` and `unanswered_secret` it names: the document was not
changed, and the resend writes every credential in it again, the unanswered
one included. Only the `503` with `"outcome": "unknown"` is not, because its
document may already be the company.

### Checking a credential before it is stored

`POST /setup/integrations/{kind}/verify` asks the vendor whether a credential
somebody has typed works, with one read-only identity call, and **stores
nothing**: not the value, not a `${VAR}` pointer, not a revision and not a
reconcile finding. It is what a connect form's Test button calls, so a key is
checked while the vendor's page is still open rather than by the next
reconcile pass, minutes later. It is `setup.connect`, the verb connecting is,
because it spends a credential at a vendor on a person's hand.

```bash
curl -X POST https://engine.example.com/setup/integrations/datadog/verify \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"field": "app_key", "value": "<the typed key>",
       "values": {"site": "datadoghq.eu", "api_key": "${DATADOG_API_KEY}"}}'
```

| Field | Meaning |
|---|---|
| `field` | The requirement to check, by its `field`. It must be a `secret` |
| `value` | What was typed, or a whole `${NAME}` naming a stored secret |
| `seat` | The seat a per-seat credential belongs to, such as a Slack agent's bot token |
| `values` | The other fields typed in the same form, by `field`. Each may be a whole `${NAME}` too |

**Every field the person did not type is read the way a submission would read
it**: what the company document holds for it (a whole `${VAR}` resolved
through this node's chain), else the field's `default`. So a key changed on a
working integration is checked against the region, the instance and the other
key already in force. Jira and Confluence are checked under the company's
declared `integrations.atlassian.deployment`, and at the site the Atlassian
organization names when neither the form nor the document gives one.

Answers `200 {"outcome", "detail"}`. `detail` is one sentence for a person and
never repeats the value that was checked.

| `outcome` | Meaning |
|---|---|
| `works` | The vendor accepted it, and `detail` says who or what it authenticates as where the vendor says: "Works. Both keys are valid for the FairSplit organization in datadoghq.eu, and can read users, roles and webhooks." |
| `refused` | The vendor answered and refused it: a 401 or 403, Slack's `invalid_auth`, or a credential that authenticates without what the integration needs (a Mattermost account that is not a system administrator, a GitLab token without its scope, an Atlassian key created with scopes, a Slack user token where a bot token belongs). The vendor's own reason is repeated where it is short |
| `unreachable` | The vendor gave no answer about the credential: no connection, ten seconds without an answer, a `429` or `5xx`, or an answer that is not the vendor's API (often a wrong address). The detail says nothing was stored |
| `unchecked` | The field cannot be checked ahead without acting on it, and `detail` is its `check_note` |

What each surface checks, and with what:

| Surface | Checkable | Read-only call | Proved later, by the vendor's first delivery |
|---|---|---|---|
| `atlassian` | `api_key`, against `org_id` | one page of the organization's service accounts, which a scoped key is refused | |
| `confluence` | `token`, with `email` on Cloud | `GET /rest/api/user/current` | `webhook_token` (Cloud), `webhook_secret` (Data Center) |
| `datadog` | `app_key`, checked with `api_key` in `site` (`check_with`); `api_key` alone answers its `check_note` | in stages, stopping at the first refusal so the detail names the key or permission that failed: `GET /api/v1/validate` (the API key alone), `GET /api/v1/org` (both keys), then one row of `/api/v2/users` and `/api/v2/roles` and a read of the configured webhook (absent is fine). See [Testing the keys](../integrations/datadog.md#testing-the-keys-before-you-save-them) | `webhook_token` |
| `github` | `token`, at `url` for Enterprise Server | `GET /user` | `webhook_secret` |
| `gitlab` | `admin_token` (needs `api`) and `token` (needs `read_api`), at `url` | `GET /user`, then the token's own scopes and expiry where the instance describes them | `signing_secret` |
| `jira` | `token`, with `email` on Cloud | `GET /myself` | `webhook_secret` |
| `mattermost` | `admin_token`, at `url` | `GET /users/me`, and the `system_admin` role | |
| `slack` | a seat's `bot_token` | `auth.test` | a seat's `signing_secret` |

Refusals: `400 invalid_body` for an unknown key; `400 invalid_input` when
`field` is not a requirement of that surface (or seat) or is not a `secret`,
when `value` is empty, when a key in `values` is not a field of the surface,
when a `${NAME}` names no stored secret ("There is no secret NAME."), and when
the check cannot be made from what was given (Datadog's other key, a site
address, an organization id), each with a sentence saying what is missing;
`404 unknown_kind`; `404 invalid_input` for a seat that takes no per-seat
setup; `409 no_active_revision`; `409` under one of the five codes for a
revision this node [cannot open](#a-revision-this-node-cannot-open-is-refused-whichever-route-reads-it).

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
will remove what the third-party app holds themselves. A forced disconnect may
carry `If-Match` naming the revision it was decided on, and one the
configuration has moved past is `412 revision_advanced`, with nothing dropped
— the status `/config` answers the same header with, where a connect, whose
base is the body's `if_match`, loses as `409`.

**Either way the removal is recorded as yours.** The intent carries who asked
— the author, their kind and the credential they asked through — and the loop
carries the teardown out for them: the revision that drops the block, each
Slack seat's cleared app credentials and the sealed values the teardown
deletes are recorded under that party, exactly as the `force` path, which drops
the block inside the request, records it. They used to be the loop's own
(`reconcile loop`, of kind `system`), so the one revision that says an
integration went away named no person on the ordinary path.

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

A check **does** get the webhook address — `api.public.external_url` behind a
[public listener](#which-listener-serves-what), `api.external_url` otherwise — and the `409
no_external_url` refusal above is the writing route's alone; its `config_path`
names whichever of the two keys this node reads. Withholding
the address from a check made it report the wrong fact: a vendor handed no
base reads that as *this deployment has no inbound address* and reports
`ingress_blocked` owed by an admin — and a check records its findings through
the same fold as everything else, so pressing Check on a healthy company wrote
"every monitor that fires reaches nobody" into the live status row and flipped
the card to **Action required** over a value that was already set. Supplying
the base is not the permission to register; having a sink is.

Both record their outcome on the same fleet integration status the reconcile
loop writes, through the same fold, so a pass run by hand and a tick that runs
a minute later cannot disagree, and Company › Integrations updates with no
extra plumbing. A pass that **failed** is recorded too, as the loop records
one: phase `activating`, actor `engine`, findings dropped, because a pass that
failed did not observe anything.

`recreate_webhooks` re-registers with a fresh secret and is **destructive
across deployments**: the previous secret stops working everywhere else this
company runs. On GitLab it also rotates every seat's token, which revokes the
credential each agent is currently authenticating with. A fresh secret is
minted into the whole `${VAR}` the secret's field names, so on GitHub, Jira
and Confluence a field holding a literal — or a reference inside other text —
has nowhere to put one, and the pass is refused naming the field, its shape
(never its value) and `recreate_webhooks` itself: point the field at a whole
`${VAR}` and send it again, or leave `recreate_webhooks` out to register the
value the field holds as it is. GitLab leaves a literal signing secret alone
and rotates the seats' tokens all the same.

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
did not send stays exactly as stored. The revision is written as the caller, [attributed](#who-a-write-is-attributed-to) like every other write
here: a person connecting their seat through their own machine token is its
author, of their own kind, with `pat:<id>` beside them — it used to record the
token as the author, of the operator kind.

Every agent seat is listed, configured or not: the list is what a screen
renders a form from, so leaving out a seat with no app yet would leave an
operator no way to give it one. Human seats are excluded, because a person's
Slack account is not something this engine holds a token for.

**Without an app configuration token it does not create the apps.** That goes
through Slack's app-manifest API, which authenticates with a configuration
token Slack issues only by hand and which an organisation may decline to allow
at all. With one (the `app_config_token` field), the reconcile loop creates
and keeps every agent's app. Without one, this surface makes a hand-created
app usable: it takes the two values Slack shows on the app's own page and
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
  "step": "install_app",
  "action_url": "https://github.com/apps/acme-builder/installations/new",
  "present": true,
  "satisfied": false,
  "detail": "the app exists and nothing has installed it, so it sees no repository and mints no usable token"
}
```

`step` and `action_url` are the two clicks, described under
[One agent's own GitHub App](#one-agents-own-github-app). What the seat may do
at GitHub is its own permissions, read and changed through
`GET /setup/integrations/{kind}/access` (with `github` as the kind) and `PUT`
on the same path, and not part of this roster.

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
  "permissions": {"contents": "read", "issues": "write", "pull_requests": "write"},
  "action_url": "https://github.com/organizations/acme/settings/apps/new",
  "manifest": {"name": "Acme Senior Engineer", "public": false, "...": "..."},
  "state": "<sealed token naming the seat and who began the creation>",
  "webhook_active": true
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

`webhook_active` says whether the app will deliver events here. It is `false`
when the webhook address is one GitHub cannot reach, which is what a local
engine's `http://localhost:8200` is: the manifest then carries no
`hook_attributes` and no `default_events`, which is the only form GitHub
accepts from an address it cannot reach, rather than a hook that fails every
delivery. The app still works for everything the agent does on GitHub, which
is outbound; see [Running locally, with no public
address](../integrations/github.md#running-locally-with-no-public-address).

Refusals: `400 invalid_body`, `400 seat_required`, `409 no_active_revision`,
`404 no_such_seat`, and `409 no_external_url` when the webhook address is
unset — `api.public.external_url` behind a [public listener](#which-listener-serves-what), `api.external_url`
otherwise, named in `config_path`. The last one matters more than it
looks: an app is created with its redirect and setup addresses baked in, and
only a person at GitHub can change them afterwards, so creating one now would
mean creating it again later. An address that is set and that GitHub cannot
deliver to is not refused: the operator's browser is all the redirect and the
setup page need.

**`GET /webhooks/github-app`** is where GitHub returns the browser, twice. It is
unauthenticated because a redirect carries no engine credential; the `state` is
what stands in its place, and it is validated before anything else happens.

- With a `code`, the engine converts the manifest, **seals the app's private
  key and webhook secret first**, then records `app_id`, `app_slug` and a
  `${VAR}` pointing at the sealed key on the seat, through the entity route as
  [per-seat setup](#per-seat-setup) does. All three writes are **the
  beginner's**: the sealed rows and the revision name whoever
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
- With `?error=`, it says in its own words that the creation was cancelled
  (`access_denied`) or that GitHub didn't create the app, and logs GitHub's
  `error` and `error_description` as `github_app_create_refused`. The
  description is never printed: the route is unauthenticated, so it is
  whatever the address that brought the browser here says, on the dashboard's
  own origin.

The page names a seat only where the value can be a seat's handle: an install
arrival's `installed` is whatever the caller typed.

A link is spent the first time it's presented, so asking for the same address
again (a refresh, or Back from GitHub's install page) runs nothing. The page
then says what the agent the link names has: its app, in which case there's
nothing left to do and the install button is still offered, or no app yet, in
which case it says to give the app a minute to show before starting again.

The status is whose the stop was:

| Status | When |
|---|---|
| `200` | A completion, an install, or a link presented again whose agent has its app |
| `400` | GitHub refused or the person cancelled, GitHub sent no code, or the link or GitHub's code is expired, forged or already used |
| `403` | The company's settings are managed outside Crewlet |
| `404` | The agent the link names is no longer one of the company's |
| `409` | No company is set up |
| `502` | GitHub didn't finish converting the code |
| `500` | Crewlet couldn't finish: it couldn't check the link, read the company's settings, save the app's key or webhook secret, add the app to the agent, or confirm that it did |

None is a `503`, because the same request can't succeed later: the link is
spent before anything that can fail for a while, so sent again it runs nothing
and is answered as already used, by whether the agent it names has its app.
That answer is what settles an app whose record on the agent the coordination
store took without answering (the `500` that says to refresh: the change may
have landed, so the page reads the agent back once first and answers `200`
when the app is there), and a new creation is what clears any other passing
condition, which the page says. Once GitHub has handed the app over, the
heading says the app was created but not finished, or, on that unanswered
record, that Crewlet is confirming it was added. What the page says is always
the engine's own wording, chosen by the step that stopped, and never a quote of
an error or of GitHub's response body, because that body carries the app's
private key. Every answer carries `Cache-Control: no-store`.

**The roster says what is outstanding.** A seat entry in a tool's `seats` array
carries two more fields where a per-seat app has clicks: `step` is what is
left as a closed set of `create_app` and `install_app`, and `action_url` is
where a person goes to do it. What the seat may do at GitHub is not on the
roster; it is the seat's own permissions, on the integration's access route. Two steps
rather than one, because they are two acts minutes or days apart and an
operator who has done the first needs to be told the second is left rather than
shown the same button. `action_url` is **empty for `create_app`**, and that is
not an omission: an app is created by POSTing a manifest, not by following a
link, so the dashboard asks the begin route above for one and submits a form.

### Each agent's access to an integration

Every integration the engine creates an account per agent on (Slack,
Mattermost, Atlassian for Jira and Confluence, GitHub, GitLab and Datadog)
has an Agents tab, and these four routes are what it reads and writes. Each
agent seat's setting lives in the company document under
`roles[].integrations.<lane>`, in the vendor's own terms: whether the agent is
on there, its role or level as the identifier the vendor's API takes (a
Datadog role id, a GitLab access level, a Jira project role id), GitHub's
permissions, and the scope it is narrowed to. A lane is the block's key:
`atlassian` has two, `jira` and `confluence`, over one account per agent, and
every other integration has one named after itself. Jira and Confluence asked
for by their own names answer `404 no_seat_access`.

The agent's credential is never in the document and never in a request. The
engine seals it under a name it derives for the seat (`DATADOG_APP_KEY_SRE`,
`GITLAB_TOKEN_SRE`), binds it to the integration's tool server itself, and
answers the name and when it was minted, never the value.

**`GET /setup/integrations/{kind}/access`** answers the tab:

```json
{
  "kind": "datadog",
  "revision": "01J9...",
  "syncing": false,
  "last_sync_at": "2026-10-29T17:02:11Z",
  "options": [{"lane": "datadog", "live": true, "levels": [
    {"id": "4f0e2b3c-...", "name": "Datadog Read Only Role"},
    {"id": "7d1a9e44-...", "name": "Datadog Admin Role", "administers": true}]}],
  "seats": [{"handle": "sre", "name": "SRE", "lanes": {"datadog": {
    "desired": {"enabled": true, "level": "7d1a9e44-..."},
    "observed": {"exists": true, "enabled": true, "level": "7d1a9e44-...",
      "account": "crewlet-sre@agents.crewlet.invalid",
      "credential": {"variable": "DATADOG_APP_KEY_SRE", "minted_at": "2026-10-29T17:02:10Z"}},
    "sync": {"at": "2026-10-29T17:02:11Z", "outcome": "ok", "steps": []},
    "plan": [], "tool": "live", "administers": true}}}]
}
```

Each seat lane's `tool` says whether the agent can use its credential through
a tool right now: `live`, `waiting` (a server reads it and it is not sealed
yet), `builtin` (Jira and Confluence, whose credential the engine's own tools
read), `no_server` (no server the company declares reads it), `server_off`
(every server that reads it is turned off, `enabled: false`) or `off` (the
seat or its account is off).

`revision` is the company revision the answer was read at, and the `ETag` is
the same value quoted. An entry of `options` whose vendor did not answer just
now carries no levels: which of its levels administers is unknown, so a
turn-on or a change of level on that lane asks the step-up a raise does, and
the next read asks the vendor again. Each entry of `options` also says whether a person can
ask for a credential on that lane to be rotated (`rotatable`), and carries
`"managed": "external"` where the engine does not provision the integration
for the company, which is wherever it does not hold the authority that creates
an agent's account: Datadog, GitLab or Mattermost without a `provisioning`
block, Jira and Confluence on Data Center or without the Atlassian organization
key, GitHub without `provisioning` or on a document only the operator writes
(the engine records each agent's app in the document), and Slack without
`integrations.slack.app_config_token`, the token the engine creates agents'
apps with. On such a lane a seat's switch still binds its
tool to the credential handed in from outside Crewlet, its access is managed
where that credential was issued, and its `observed.credential` names the
variable the seat holds with `"source": "external"`. A credential Crewlet
minted says `"source": "minted"`. Where the operator writes the company
document (`api.auth.company_writers`), the answer carries
`"managed_by": "operator"` and the tab is read-only: a save and `keep` are
refused `403 config_managed`, naming the Seat object as the place to change
the agent's access. A seat the document no longer has whose account the
engine still records is listed with `"departed": true`, and only `remove`
applies to it. `notice` carries a company-level warning, such as the one the
migration raises when an agent's credential was kept in a variable something
else also names (a company setting, a unit's shared credentials or another
agent): the integration's own
[`credential_shared`](../concepts/integration-reconcile.md#what-a-pass-reports)
finding, drawn on the tab until the variable is written again.

A lane has a `plan` only where the sync takes steps on it: an integration the
engine provisions, once its migration onto per-seat access has finished. On a
lane managed outside Crewlet, and on an integration still waiting on its
migration, `plan` is empty and a draft's plan lists no steps.

**`POST /setup/integrations/{kind}/access/plan`** takes a draft and answers
what saving it would do at the vendor, writing nothing. It is the same plan
the sync takes its steps from, so a preview cannot promise what a sync will
not do:

```bash
curl -X POST https://engine.example.com/setup/integrations/datadog/access/plan \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"seats": {"qa": {"datadog": {"enabled": true, "level": "4f0e2b3c-..."}}}}'
```

```json
{"kind": "datadog", "revision": "01J9...", "step_up": false,
 "plans": [{"handle": "qa", "lane": "datadog", "steps": [
   {"op": "create", "verb": "create", "text": "Create crewlet-qa@agents.crewlet.invalid with Datadog Read Only Role", "by": "crewlet"},
   {"op": "create", "verb": "mint", "text": "Mint an application key, sealed as DATADOG_APP_KEY_QA", "by": "crewlet"}]}]}
```

A draft names each seat by handle and each lane by its key, and a seat or lane
it leaves out keeps its setting. Its fields are `enabled`, `level`,
`permissions` (GitHub only, each `read` or `write`) and `scope`; any other
field is refused `400 invalid_body`, so a body cannot name a variable. A
person's seat or a seat the company does not hold is `404 no_such_seat`; a
lane the integration does not grant, or a level, permission or scope entry the
vendor does not offer now, is `400 access_not_offered` with the lane, the value
and what is offered. A live-listed level (a Datadog role, a Jira project role)
is judged only when the vendor answered just now; otherwise the sync reports
an id it does not hold as a finding. A GitLab project is within reach only
inside the group the company's connection joins agents to, so where the
connection lists no projects, a project outside that group is
`400 access_not_offered` too.

**`PUT /setup/integrations/{kind}/access`** saves the same body as one revision
of the company document and starts the sync for that integration. Every access
sync ends in the same read-only pass `POST /setup/integrations/{kind}/check`
runs, recorded on the integration's status through the loop's fold, and the tab
reads `syncing` until that pass is recorded, so the integration's issues and
notes say what the change left as soon as the sync is over. Send the
tab's `revision` back as `If-Match` (or as `revision` in the body); a save on
a revision the fleet has left is `409 revision_advanced` and writes nothing.
The revision records the caller as its author and a history line naming what
changed (`agents' access on datadog: turn on QA Engineer on datadog`), with an
optional `summary` after it. The answer is the tab, as `GET` answers it, with
`syncing` while the sync runs; where the tab cannot be read again just after a
save landed, the answer is the tab as read before the save, at the revision the
save wrote, with the saved settings in place, rather than an error for a save
that landed. A seat removed from the document between the tab's read and the
save is `404 no_such_seat`, and nothing is written. A save made while a sync runs is not refused: it
lands, and a sync follows. `?dry_run=true` answers the plan, `step_up` and the
`warnings` the revision would introduce, and stores nothing (see
[Dry runs](#dry-runs)).

A save that raises an agent to an administration role at the vendor (Datadog
Admin Role, GitLab Maintainer or Owner, Jira Administrators, Confluence Admin,
Mattermost System Admin, or GitHub Read and write on Contents or Workflows)
asks for a person present and a fresh proof inside `step_up`: a machine token
is refused `403 token_refused`, and a stale proof `403 step_up_required`. The
plan's `step_up` says so before the save. Keeping a role an agent already holds
and lowering one never ask. Where the vendor's levels could not be read, a
turn-on or a change of level asks as a raise does. An integration still
waiting on its migration asks it as well: the saved setting is the one the sync
applies once the migration finishes.

The same rule holds on every other write of the company document by a person
or a token: `PUT` and `PATCH /config`, a seat's `PUT /config/roles/{id}` and a
revert. Each compares the revision it replaces with the one it proposes, and
one that raises an agent's access is refused for a machine token
(`403 token_refused`) or a stale proof (`403 step_up_required`), naming each
raise in `raises` (`kind`, `handle`, `lane`) and storing nothing. A dry run of
it is answered, since it writes nothing, and a document the operator writes is
judged by the operator's own authority instead.

Turning an agent off disables its account at the vendor and keeps it, so its
history keeps its author and turning it back on needs no person (except
Slack's install). Nothing a document edit does removes an account.

**`POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}`**
acts on one agent's account, and answers the tab:

| Action | Does |
|---|---|
| `rotate` | Mints a new credential, seals it under the agent's name, and revokes the old one. Asks `secrets.write` too. A credential handed in from outside Crewlet is refused `409 credential_external`: it is rotated where it was issued |
| `remove` | Removes the account at the vendor, where the vendor allows it, and stops the engine tracking it. Only once the agent is off on this lane: `409 seat_still_on` otherwise. On a lane managed outside Crewlet it is `409 access_external`: the account is removed at the vendor |
| `keep` | Writes what the vendor shows into the document as a new revision, in the vendor's own terms: after a role or scope was changed at the vendor ("Keep the vendor's setting"), or after the account was turned off there ("Keep off", which keeps the role the document names for when it is turned back on). Nothing is restored. Asks `setup.access.write`, and `setup.access.raise` where the role kept is an administration role. A Datadog account holding several roles is refused `409 invalid_input`, since an agent holds one |
| `restore` | Turns back on an account somebody turned off at the vendor, which no sync does unasked, or restores a changed role now rather than on the next pass. On a lane managed outside Crewlet it is `409 access_external` |
| `retry` | Runs the sync again after a vendor refusal was settled |
| `done` | Reads the vendor again after a person took a step there (a GitHub App's creation or permission request, a Slack install) |

Every action but `keep` is carried by a sync, and one asked while a sync of the
same integration runs is `409 pass_in_flight`. `keep` writes its revision and
then starts a sync, as a save does, which finds the vendor already matching.
Each of these syncs ends in the check's own pass, as a save's does.

**`POST /setup/integrations/slack/access/seats/{handle}/{lane}/install`**
answers `{"url": "https://slack.com/oauth/v2/authorize?..."}`: a link signed for
the agent's own app, recorded as issued and spent by the install it completes,
so it works once and only for a short while. It is minted only while the
agent is on in Slack and its app waits for a person to approve the install
(`409 invalid_input` otherwise), never stored, never logged and never part of
`GET`. It writes nothing to the company document, so it is offered where the
operator writes the document too.

#### Moving a company onto per-seat access

A company document written before per-seat access said an agent was on at a
vendor by naming a `${VAR}` where the vendor's provisioning looked for one, and
said what it could do once for the whole company (`provisioning.role`,
`access_level`, a GitHub `tier`). On its first start the engine migrates the
document, one integration at a time, in one revision attributed to
`seat access migration`: each agent seat states `enabled` per integration,
judged on the credentials it named itself and the ones its unit's `mcp_env`
shares with it, as the earlier engine read them. An agent whose account exists
keeps the role or level the vendor shows for it; only an agent with no account
yet takes the company's role, in the vendor's own terms (a Datadog role is
looked up at Datadog by name). A Datadog account holding several roles is not
guessed at: Datadog waits, naming the agent, until the account holds one role
or a person saves the one it keeps. A Jira or Confluence agent with no role of
its own keeps product access only (no project or space role, which is all the
earlier engine granted), a Mattermost bot's channels are written out, each
credential the engine minted is re-sealed under the agent's own name, every
account the engine made for an agent it had on is recorded, and the retired
fields are removed. A credential is never sealed over one the agent already
holds under its own name, and GitHub's installation tokens, which the engine
issues at use, are never moved. A credential that named a variable something
else also names (a company setting, a unit's shared credentials or another
agent) is never moved: the agent gets a new one from its next sync, and the
Agents tab warns that the shared credential must be replaced until it is. A
unit's shared key for a tool server the engine now binds itself is removed from
the unit.

The document lists the integrations it has moved in `seat_access_migrated`,
and the access sync for an integration acts on no agent until it is listed. An
integration whose vendor does not answer stays unlisted, with its seats exactly
as they were, and the engine tries again every minute. Its access pass reports
an `identity_missing` finding that names the integration, says its access
migration has not finished, and gives the reason the last attempt gave (for
example, that the lookup of Datadog's role failed); the log's
`seat_access_migration_pending` line carries the same reason. An integration
the company does not connect is not waiting on the migration and reports
nothing. Reverting to a
revision written before the migration closes the gate again, and the next
attempt migrates that document.

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
  [`/operator/act`](#operatoract--the-dashboards-write-surface)); a person's
  directory row is `/iam`; the company document — its org chart included — is
  `/config`; a credential is `/secrets`; an integration's
  setup is `/setup`; a backup is `POST /backup`. A write answers
  with the position it landed at, and the reads it moved are asked again on the
  socket at that position.
- **Guarded reads the query registry does not answer** — the secret names
  (`/secrets`), where each `${VAR}` resolves from (`/config/references`), an
  integration's setup (`/setup/integrations`) and the identity directory
  (`/iam`). They are credential-scoped
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
`no_route` a path nothing serves answers. A question refused on authority
answers `403` in the [refusal envelope](#every-refusal-is-one-envelope) every route
answers the same decision with — `unauthorized`, its `reason` and its
`grants` — and never `step_up_required` or `token_refused`, because no question
is decided by a verb that asks either.

### An event on the wire

One shape, whether it came from the live projection or from the event store,
and the field names are the same either way — a screen shows a live row beside
a historical one, so two spellings of one event would render the two halves of
one list differently:

```json
{
  "id": "…", "type": "external_notification", "source": "notify.mattermost",
  "timestamp": "2026-08-25T12:00:00Z", "category": "notification",
  "summary": "…", "actor": "…",
  "trace_id": "…", "span_id": "…", "parent_span_id": "…",
  "failed": false,
  "payload": { }
}
```

`payload` is present only where the event was fetched by id or by trace: a
listing deliberately never selects it, because a page of events with every
payload attached is the query that makes a live screen slow.

A stored event also carries `node`, the node that **published** it — where the
work it describes ran — on every read, listings included. It is not the node
that answered with it: a node without the `data` role keeps no event log, and
the data node keeping its events in
[custody](../guides/deployment.md#custody-the-rows-of-a-node-without-data)
holds rows it never published. It is absent only on an event no node published
(a test harness's).

Every stored type reaches the activity feed — the snapshot's `events` and each
`event` frame — except `auxiliary_spend`, which is accounting rather than
activity: what the auxiliary model cost, coalesced per key per flush. It is a
row like any other in `GET /events`, a turn's `turn` answer and a trace, and it
reaches every spend figure through the rollups; it is never pushed as an
`event` frame and never takes a row of the feed (see
[Event System](../concepts/event-system.md)). A read that is merged with the
feed or drawn beside it asks `feed_only=true` on `events` and `event_series`,
and gets the feed's rows alone — what the dashboard's event log, its Live
strip and a seat's latest events do, so a page scrolled on from the ring and the
bars above it hold exactly what the ring would.

### A seat's LLM history

`llm_history` is the seat's **finished** phases, read from the event store of
**every live data node** — placement moves a seat, and the phases it ran are
kept by the data node it ran on, or, where it ran on a node without `data`, by
the data node keeping that node's events in custody — one row per
`agent_phase_completed` and per `executor_collected`, newest first,
capped at 50, with the answer's `coverage` saying which nodes it heard from
(see [Reading the fleet's history](#reading-the-fleets-history-coverage)). The call
*in flight* is not in it; that is `live.live_call`, which comes from the
projection, and the two are different sources on purpose: the store holds what
completed, memory holds what is happening. A screen renders both with one
renderer, so each history row carries the same fields a live one does —
`turn_id`, `phase`, `iteration`, `model`, `response`, `tool_executions`,
`round_narration`, `partial_round`,
`total_tokens` — plus the envelope's `timestamp` and `failed`. It is the
stored record, so it also carries the phase's price where its own harness
reported one (`cost_usd`), which the dashboard never reads
([rule 19](dashboard-design.md#rules-a-change-has-to-keep)). Each launch of an
executor's run is a row of its own, an `executor_collected`, published when it
is collected: **no phase** — it names none, and is the work of the `execute`
round that launched it (`iteration`) — with its tokens, its harness's own turns
(`harness_turns`), its `launch_id` (a round can launch twice, so the launch is part
of that row's identity), its report as `response` and its
`activity_transcript` — see
[each launch is published as its own record](../concepts/code-sandbox.md#what-ends-a-run-each-launch-is-published-as-its-own-record).
A finished row also carries `duration_ms`, which a live one cannot: it is the
engine's own measurement of the phase, published on the record rather than
reconstructed by pairing it with the `agent_phase_started` that shares its key.
Zero means *not measured* — an executor's rounds ran inside its coding
harness's own loop, in another process — never *took no time*. A phase that failed
before it reached a provider also truncates to zero at this resolution;
`failed` is what separates the two.

It measures the phase's **own segments**. The executor launches its run and
parks, and when the run is collected the same turn re-enters the phase — after
a restart, possibly on another node — so the one record the phase publishes
carries the launch segment's time, carried across on the park, plus the
collection's, and never the run between them: the run's own time and tokens
are on each launch's `executor_collected` row.

A finished row carries the phase's **timeline** too, and so does every other
reader of the record — `phases`, `turn`, `trace` and `event` answer the stored
payload verbatim: `started_at` (when this segment began — not published minus
`duration_ms`, which on a resumed phase also counts the launch segment), `rounds[]` (one
`{round, started_at, duration_ms, model, input_tokens, output_tokens,
cache_read_tokens, cache_write_tokens, tool_calls, stop_reason}` per model
request the harness's record showed, the model's half of the round only;
`stop_reason` is `end`, `tool_use` or `cap`), `invocations[]` (one
`{started_at, duration_ms, provider_key, model, outcome, first_round,
last_round, requests_observed, input_tokens, output_tokens, cache_read_tokens,
cache_write_tokens, tool_calls}` per harness run, `outcome` one of `end`, `cap`,
`stopped` and `failed`), each `tool_executions[]` row's `call_id`, `started_at`,
`duration_ms`, `origin` (`builtin` or `mcp:<server>`) and `server`, the
phase's `cache_read_tokens` / `cache_write_tokens` (a breakdown of
`input_tokens`, never an addition to it), `max_rounds` / `round_ceiling`, a
worker's or a judge's `host_round`, a resumed executor's `launch_id` and the
turn's `work_item`, and `steers[]` — `{round, note_id, boundary}` for each
person's note the phase read; a read by the phase's executor in its box is at round 1,
with the kind of step that read it. The live frame carries the same so far plus
`round_started_at`, the `run_started_at` of the harness run in flight and the
`running_calls` in flight. Each is **absent** where
nothing measured it — a tool call nobody timed has no `duration_ms`, never a
zero — so a reader treats absent as *not recorded*. The whole field list, and
why each is measured where it is, is in [what a turn records about its time](../concepts/turn-engine.md#what-a-turn-records-about-its-time).

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
| `org` | The same projection [`GET /org`](#get-org) answers, built for the socket's audience: the charter, the company's resolved `timezone`, its `token_budget` and the `services` it runs, root-level `roles` and `units` nesting to any depth, with only the fields that projection declares for each, a seat's resolved `llm` chain and `tool_sources` only to an audience holding `admin` |
| `tools` | The catalogue this node serves, each entry tagged with the `source` that registered it — `builtin` or the MCP server's name. Empty on a node that runs no company yet, which has no catalogue |
| `events`, `executors`, `tokens`, `budget`, `health` | The live projection: what has happened |

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
correct them: a change that adds, edits or removes a seat produces no event a
projection could learn from, and an overlay merge cannot express a row going
away.

**A company is published by a node's boot and by every config apply**, and
these four pushes follow every one of them, as the last step of
[the convergence](../concepts/configuration.md#what-follows-a-published-company).
Each is a WHOLE payload that replaces its predecessor: the hub drops the
oldest queued envelope without telling the client, so a delta it dropped would
be unrecoverable.

**The `tools` push is also re-sent alone** when the catalogue moves between
publishes: the one part of a published company that does is its shared MCP
servers' tools, refiled when a server
[comes back](../guides/tools-and-mcp.md#a-server-that-dies-or-will-not-start)
from a crash listing different tools, or comes up on a retry of a start that
failed. The other three have not moved, so they are not sent.

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
the seat's handle ([organization model](../concepts/organization-model.md)),
which every event a seat publishes carries as `agent_id`. Never by its name:
a seat's name is prose and two seats may share one, and a projection keyed
on it gave both of them every overlay either moved, so each card rendered
whatever the other was last doing. An event
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
response, tool calls so far) — and
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
JetStream returns a failed (NAK'd) message *behind* never-delivered ones
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
spend records for its own 24-hour window — each phase's, and each
`auxiliary_spend` record of what the auxiliary model cost — the newest 24 000
of them, the WHOLE COMPANY's since every projection is fed fleet-wide (about
2 600 turns a day; a company past that sees a rollup covering slightly less
than a day rather than a wrong total) and folds them with
`internal/tokens`, which folds the named windows' company days into the
same breakdown, so changing the window cannot change what a phase is
counted as. The window is a ROLLING one, aged on the serving node's clock
and never on a record's own stamp — every read leaves out what the window
has aged past, and both an arriving record and the shared tick drop it:

- a record stamped before the window is not counted, however late it
  arrives;
- one stamped ahead, by a node whose clock runs fast, is aged from when it
  arrived — from the startup seed, for one read from history — so it leaves
  a day after it came and the cap takes it in that place, rather than moving
  the window or outliving it. It still shows the stamp it was published
  with, which is how the node with the wrong clock is found;
- one whose stamp does not parse is kept (nothing can age it) and is the
  first the 24 000 cap drops.

The rollup's `since` and `until` are the two instants the window was cut
at. It ships in the snapshot and is re-pushed on the shared 5-second tick
after any phase completed or any record aged out, so a client holding it
stays live without a fetch — and a company that has gone quiet sees its
spend leave the window as it ages rather than keep its last busy day —
without a second implementation of the aggregation in the browser. (The
bundled dashboard's Settings › Token spend reads [named windows](#token-spend-breakdown)
instead.) What a seat has spent is that rollup's per-agent row: the
projection keeps no second total of its own.

**The projection is seeded from the fleet's event stores when the process
starts**, after the broadcast subscription is attached and before the node
hands its listener the API surface — before it serves. The seed makes three bounded reads, side by side, and each read is bounded by the projection's own limit:

- the newest 400 persisted events, for the feed;
- the newest 24 000 spend records — phases and auxiliary records — inside the 24-hour spend window, read in pages of 4 096 records a node, because a busy node's whole day is more than one reply of the scatter carries (8 MiB) — asked whole, its reply was cut, and every node's records were cut with it;
- the newest three turns of every agent seat, for the seat's `last_turn` and for a turn it left parked.

Each read goes to **every live data node**, through the same scatter the
history queries use (see [Reading the fleet's history](#reading-the-fleets-history-coverage)).
Each data node's store holds only what that node published — plus what any
node holding no data handed it, see
[Deployment → The event store](../guides/deployment.md#the-event-store) — so a
seed that read this node's store alone showed a restarted node only its own
share of the company. Without any seed, every one of these surfaces started at this
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
reads above and before the node serves: a pause taken before this process started is
in no event it will hear, and a paused seat drawn as working is the one state a
person pausing it must not be shown. A failed read is logged as
`seat_pauses_not_seeded`; the pause itself is in force either way.

**The executors in flight are reconciled against their durable record**. The record is read once before the node serves and then every 30 seconds. The stream is lossy and in memory, so it cannot say which executors are out, and the record can. See [the panel of executors in flight](../concepts/code-sandbox.md)
for which of the two wins when they disagree. A reconcile that changed the set pushes it as `executors`, and a reconcile that changed nothing pushes nothing.

### `GET /stream/snapshot`

Single-shot bundle equivalent to the WebSocket handshake's first
envelope: every section a dashboard screen needs on first paint.
Assembled entirely from the in-memory projection — no database
round-trip on the hot path.  Used as a fallback when the browser cannot
upgrade to a WebSocket (corporate proxies, etc.).

**Each section is the caller's**, decided by what its push kind needs on the
socket: the bundle is everybody's — every principal the guard resolved whose
stage may act — and the `events` section is present only for an
administrator, absent rather than empty, because an empty feed reads as a
company that has done nothing. The `agents` and `executors` sections carry
each call's and run's **body** only where the caller may read it ([A status
reader never receives a transcript](#a-status-reader-never-receives-a-transcript)).
`GET /agents`, `GET /org` and `GET /tools` are the same sections served alone,
shaped the same way.

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
  "executors": [ { /* an executor run in flight: turn_id, role,
                      agent_handle, agent_id, harness, provider_key
                      (the executor-chain member it was launched on, to an
                      admin alone) and model, sandbox_id, task, status (the
                      run record's own word), started_at, question,
                      audience, work_item, owner, paused_at */ }, ... ],
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
*is* a failure (`executor_failed`, `llm_unavailable`, `budget_exhausted`,
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
is the `admin` [`fleet`](#get-fleet) answer, and what each alarm measured is
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
  "config_attempt": {"epoch": 41, "revision_id": "01JA8Q2W5H5S1Q9P0N1C7K3M2D", "status": "ok"},
  "seats": ["ceo", "cto"],
  "identity_duty_seconds": {"iam_sweep": 3600},
  "unproven_seconds": {"eng": 312.5},
  "nodes": 3,
  "alarms": {"count": 1, "worst": "trim_blocked"},
  "seeded_from": {
    "nodes": [
      {"id": "core-1", "answered": true, "error": ""},
      {"id": "core-2", "answered": true, "error": ""}
    ],
    "complete": true
  },
  "identity": "ready",
  "objects": {"direct_upload": "available"}
}
```

Before anybody is in the company, the same body says so. The remedy is the
first person, put onto a human seat the company declares under a Tier A token —
invited into it, or created on it — so a company that declares no human seat
yet declares one first (see [How the first person
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
| `status` | `shutting_down`, `unconfigured`, a diverged posture (`shed`, `stuck` or `isolated`), or `ok`, in that order of precedence — and `starting` on the shorter body a node answers before it serves ([While a node is starting](#while-a-node-is-starting)), which never appears in this envelope. A draining engine is draining first, whatever else is true of it, and a node running no company is that before it is anything else. The two ordinary postures, `serve` and `wait`, read as `ok`. A node applying its first company reads `unconfigured` until that apply completes and `ok` from then on; a later apply reads `ok` throughout. |
| `node` | The name this node's engine runs under — `node.id`, else `CREWLET_NODE_ID`, else `node-0` — which is what its presence lease carries and the only way a caller can tell which node a load balancer sent it to. |
| `configured` | Whether this node's engine runs a company revision. Read off the engine's live epoch on every call, so an apply that brings this node its first revision flips it. When `false` the node **refuses** every inbound webhook with `503`, so an operator watching empty screens needs to be told this rather than left to infer it. |
| `version` | The `crewlet` version this process is running. |
| `started_at` | When this node's **engine** started, which is when the node started: the API is served inside the engine's process. The fleet view reports the same instant for this node. |
| `queue` | The event queue's backend — `jetstream-embedded` (a NATS server inside this process), `jetstream` (an external NATS cluster this node dialled), or `memory`. Read off the `EventQueue` contract's own `Backend()`, never sniffed from a type name. Display only; nothing may branch on it. |
| `clients` | Dashboards currently connected to this node. |
| `event_history_seconds` | How far back the event log can be read — the hard bottom of [paging](#paging-the-event-history): once a cursor crosses it every page is empty forever, so a client that cannot name the floor draws the store's own horizon as "the org went quiet". The store's constant, not a number this API picked, so a change to the retention reaches every screen without an edit. Seconds rather than days, because the retention is a duration and a client re-deriving the unit is a second place the number can be wrong. |
| `spend_history_seconds` | How far back a **named** spend window can reach: the replicated `usage` domain's own history (181 days, [ADR-0020](../../adr/0020-a-nodes-own-day-is-a-compacted-domain.md)), which is not the event log's. A spend chart states this floor and never `event_history_seconds` — the two answer "can I still chart that month" and "can I still open that turn". |
| `in_flight` | Turns running on this node. Always present, and a `0` is a real zero: every process that serves the API runs the engine beside it. |
| `shutting_down` | `true` from the first moment of a drain, so a dashboard shows the drain while it happens: the listener keeps serving until the drain has completed. See [During a drain](#during-a-drain). |
| `posture` | The node's [config posture](../concepts/control-plane.md#posture-what-a-lagging-node-does): `serve`, `wait`, `shed`, `isolated` or `stuck`. The only place an operator can see *why* a node left rotation, since `/ready` answers a bare `503` either way. A node applying a revision reads `wait` until the apply ends — `serve` once it succeeds — so a client polling across a `PUT /config` sees no diverged posture from a write that applies, a corrected revision written after a failed one included; `shed`, `isolated` and `stuck` follow only an attempt at that epoch that concluded in failure, or (`shed`) a lag on that epoch past 45 s while a peer has it. |
| `identity_duty_seconds` | Each [identity duty](../guides/retention.md#the-identity-duties) **this node** armed, mapped to the interval it runs at, in seconds, whenever it holds that duty's lease. `{}` on a node that armed none — one running no `workers` role, since every one of them is a worker singleton, or one in a maintenance mode, which publishes nothing. A node with no company arms them like any other: the identity estate runs from boot on every data node. A duty that was never armed looks from every other vantage point exactly like one quietly finding nothing to do, so this is where you read that the retention sweep is running at all. Which node holds each lease right now is the coordination store's answer, not this node's. |
| `applied_epoch` | The activation epoch this node last applied, `0` until its first apply since it started: a node that booted on one revision and then refused the fleet's answers `0` while it runs the one it booted on. Which revision a node runs is [`GET /fleet`](#get-fleet)'s `config_serving_revision_id`. |
| `config_attempt` | `{epoch, revision_id, status}`: this node's last concluded apply attempt — the epoch, the revision it tried at that epoch, and `ok`, `error` (refused, still running the epoch before) or `degraded` (refused after part of the company had already been rebuilt). **Absent** until this process has concluded one. It is what tells a refusal from an apply still running, which `applied_epoch` cannot: a failed attempt leaves that where it was. Never the reason, which can quote the config value it was given: why a node refused is [`GET /fleet`](#get-fleet)'s `config_error`, an admin's. The revision rides beside the epoch because one epoch number names two revisions once a rebuilt coordination store has restarted the count. |
| `seats` | The handles of the seats this node holds, `[]` on a node holding none. |
| `stall_lag_seconds` | Present only when the node's watched duty is behind: when its last beat is older than the beat interval it keeps (a second at the shipped lease TTL, which is how often a turning duty beats), and then how long since that beat, in seconds. It climbs towards the seat lease TTL, at which the watchdog ends the process. Absent on a node whose duty is on its beat, so its presence is what to alert on. |
| `identity` | Whether this company has its **first person**: `ready` once a person is enrolled, `unclaimed` while nobody is — a service account alone signs nobody in with a password, so it does not count — a fresh install waiting for [its first person](../concepts/identity-and-access.md#how-the-first-person-exists) to be invited into, or created on, a human seat its company declares, under a Tier A token. A person an administrator creates makes it `ready` the moment the create lands, before they have spent the first password link it handed back: it says somebody exists, not that they have chosen a password yet. It is `unknown` where this node cannot read its identity estate **or has not applied all of it** — a node that has just joined a fleet holds empty rows until its applier catches up, so "nobody" is proved against the identity log's end — which is never reported as `unclaimed`, because a dashboard told nobody is in would tell an operator to put a first person into a company that may have started. Every node answers it, one with no company included: the identity estate runs from boot, so such a node answers for the fleet's. `/health` is the one surface an unclaimed company can reach — it is never guarded, and before the first person there is no credential to present anywhere else — which is why this is here. It does **not** move `status`. |
| `nodes` | How many nodes hold a presence lease — the fleet this node's fan-outs (search, fleet history) divide their work by. **Absent** when the presence read failed or did not finish inside the probe's coordination budget (an eighth of the 15-second reconcile interval, under two seconds) (it runs beside the posture read, so a wedged broker slows `/health` by that budget rather than hanging it); never `0`, since the node answering is itself one. A screen says "node count unavailable" for an absence rather than guessing. |
| `alarms` | `{count, worst}`: how many of this node's [alarms](alarms.md) are firing, one per kind however many logs it stands on, and `worst`, the one that has been firing **longest** (absent when `count` is 0); the table asserts no severity of its own, and the condition that has gone unanswered longest is the one the dashboard's engine panel names first. `worst` is a KIND and names no log: an alarm that stands on logs (`log_headroom`, `log_ceiling_short`, `backup_age`, `trim_blocked`, `deferred_old`, `floor_unknown` and `census_drift`) lists them in its `domains`, each entry a measurement with the logs it was taken on, on the operator-only [`/worklet/retention`](#get-workletretention-what-the-log-is-holding) answer, which this public body points at rather than restates. From the **same** evaluation the `crewlet.alarm.active` gauge (one series per kind) and the `alarm_raised` / `alarm_cleared` log lines come from (and a condition that is a property of a log writes those lines per log, so a second log joining an alarm already up, and the first one leaving it, are each said), which runs every ten seconds on every node running a state log. **Absent** before that evaluation first runs, and on a node running none, one without the `data` role: neither has looked, and `{count: 0}` would read as healthy. |
| `seeded_from` | Which nodes this node's live projection was seeded from at boot — the activity feed and each seat's last turn, which the pushed screens start from, and the live spend window behind the [`tokens` push](#pushes) — in the fleet [`coverage`](#reading-the-fleets-history-coverage) shape. Absent until the seed has run. A seed that missed a node started those surfaces a node short, and this is where that stays visible after the log line has scrolled away. |
| `objects` | `{direct_upload}`: whether an upload may go straight to the object store's S3 bucket on a presigned put. `available`, or `unavailable (<reason>)`: always so on the default `nats` backend, whose bucket only the fleet reaches, and on an `s3` bucket whose provider the boot probe could not prove refuses a body that does not match its `x-amz-checksum-sha256` and a second put under `If-None-Match: *` (see [The boot probe](../concepts/object-store.md#the-boot-probe-and-direct-uploads)). Unavailable is no fault: every upload then streams through the engine. Absent on a node running no object store. |
| `unproven_seconds` | Each seat whose teardown this node could not prove, mapped to how long it has been stranded, present only when one is. Such a seat is still leased by this node, so no peer can claim it, and this node will not run it: it is absent from `seats` for exactly that reason. Alert on the duration rather than on the field's presence: a release that fails once and succeeds on the next heartbeat is a working system. See [Seat ownership](../concepts/seat-ownership.md#what-ownership-looks-like-from-outside). |

Per-socket facts, such as how many envelopes *this* connection dropped or
how deep its queue is, are deliberately **not** here. The tick encodes one
JSON string and hands the same string to every client, so a per-client field
would force one encode per client per tick. A connection that lost envelopes
to backpressure is logged as `stream_client_left_behind`, with the count, when
it disconnects.

`GET /health` always returns **200** once the node serves, including when
`status` is `unconfigured` or a diverged posture: the status code is liveness,
and an engine waiting for a configuration is alive. Before then it answers
`503` only while the node recovers what it holds — see
[While a node is starting](#while-a-node-is-starting). Steer traffic with
[`GET /ready`](#routes) instead, which answers `503` and names the reason.

### Probes on a node without ingress

A node whose `node.roles` leave out `ingress` — a [satellite](../guides/satellite-nodes.md),
or the seats and workers half of a split deployment — serves no API, no
dashboard and no webhook. But an orchestrator runs every node, and one it
cannot probe it can neither restart when it wedges nor wait on during a
rollout. So such a node binds `api.port` for exactly two routes the network
can reach, and binds no [public listener](#which-listener-serves-what): an
executor in a box off its host reaches its tools through an ingress node's
[tool bridge](#routes), which relays each call here. The guard stands in front
of them as it does on an ingress node, so every other path is judged as one
there is: `401` to a request that presents no credential, since everything
outside the guard's exemptions is guarded (`/agents`, say), and `404 no_route`
for an exempt path this node does not serve (`/dashboard`) and for anything
once a credential is presented. The node logs `api_probes_listening`, with the
address its own boxes dial in `box_host_url`. `api.port: 0` binds nothing, on
this node as on any other.

**A node that runs seats serves its own boxes beside the probes.** A `direct`
box is a process of the node's host, and dials the node that launched it on
loopback: the [tool bridge](#routes) under `/mcp/{token}` (its notes route
included) and, where the node forwards to `OTEL_EXPORTER_OTLP_ENDPOINT`, the
telemetry receiver under `/otlp/{token}/v1/{signal}`. Both are served here to a
caller on this host's loopback alone, read from the connection's own address,
and are the same routes an ingress node serves: a token the fleet did not sign
answers `401`, and a call for a seat this node has since handed on is relayed to
its holder. Any caller off the host meets `404 no_route` under both prefixes,
whatever its method, so the network reaches nothing here but the probes. While
the node starts, a loopback caller of either is answered `503 starting` with a
`Retry-After`, as every starting node answers a route it will serve. The node
hands its boxes this address only where loopback reaches the listener — an
`api.host` of every interface or a loopback address; one bound to a single
other address leaves its `direct` boxes to `CREWLET_MCP_BRIDGE_URL`.

`GET /health` is liveness, `200` while the process is alive — through a drain
and through a broker link that is down — once the node serves; before then it
answers as every starting node does (see
[While a node is starting](#while-a-node-is-starting)). Its body is the node's half of
[the health envelope](#the-health-envelope), under the same field names, plus
the node's `roles`; it leaves out what describes a dashboard this node does not
serve (`clients`, `event_history_seconds`, `spend_history_seconds`,
`seeded_from`):

```json
{
  "status": "ok",
  "node": "sat-eu-1",
  "roles": ["seats"],
  "configured": true,
  "version": "v0.4.0",
  "started_at": "2026-04-01T11:58:03Z",
  "queue": "jetstream-embedded",
  "in_flight": 1,
  "shutting_down": false,
  "posture": "serve",
  "applied_epoch": 41,
  "identity_duty_seconds": {},
  "seats": ["eu-support"],
  "nodes": 4
}
```

`GET /ready` answers whether the node is **doing its work**, because it takes
no traffic for the probe to steer: what waits on it is a rollout that must not
replace the next node until this one has joined. `200` once every condition
below holds; otherwise `503`, with the first that does not in `reason`, in this
order of precedence:

| `reason` | The node is not ready because |
|---|---|
| `starting` | It has not started yet, and its probes are still the boot's: see [While a node is starting](#while-a-node-is-starting). |
| `draining` | It is stopping. |
| `broker_unlinked` | It cannot reach the fleet's broker: a leaf whose link to every member is down, or a connection to an external cluster that is reconnecting. `detail` carries the cause. Ahead of what follows, because each of those is what a lost broker looks like. |
| `unconfigured` | This node's engine runs no company revision. |
| `shed`, `stuck` | Its [config posture](../concepts/control-plane.md#operator-surface) is diverged. `wait` and `isolated` stay ready, as they do on an ingress node. |
| `no_presence` | It does not hold its presence lease — its last renew is older than the lease's TTL — so no peer counts it and nothing routes to it. |
| `admission_withheld` | It runs seats, and its latest placement pass was not admitted to claim: no data node has yet answered that a copy of the estate admits a seat (a stateless node), its own copy is behind (a data node), or it cannot serve the seats it holds. A node that runs no seats, or one started in a [maintenance mode](../guides/retention.md#the-three-modes), is not judged on this. |

```json
{
  "ready": false,
  "node": "sat-eu-1",
  "configured": true,
  "draining": false,
  "posture": "serve",
  "reason": "broker_unlinked",
  "detail": "jetstream: this leaf has no link to any member of the fleet, so every stream and bucket it uses is out of reach"
}
```

An ingress node's `/ready` is unchanged by any of this: it answers whether
traffic should come to it, on `draining`, `unconfigured`, `shed`, `stuck` and
`estate_behind` alone. The two share one judgement (`judgeReadiness`, `internal/api/health.go`)
and one precedence, so a reason both can give means the same thing on both.

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

**The end of the history is `exhausted: true`, `next: null`** — and only that.
A page SHORTER than `limit` is not the end: the page is merged from every
node's store (see [Reading the fleet's
history](#reading-the-fleets-history-coverage)), and it stops at the newest
point any node's page stopped at, so a node whose reply was cut to fit the
transport shortens the page without ending it. The `agent` filter adds another
reason — it also pulls in every event sharing a trace with a direct match,
from every data node, so a caller must dedupe by id.

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
`0038`, and a counterpart or a delivery's recipient named before it is not in
the related view for the rest of the retention window.

The persistent store retains 30 days, and
[`event_history_seconds`](#the-health-envelope) on the health envelope is
that floor on the wire — read it rather than restating the number, which
is the store's own constant and not a promise this page makes. Once a
cursor crosses that floor every page is empty — which is why a client
must distinguish it from quiet, rather than drawing the gap as silence.

`seat` narrows the log to the events ONE agent seat published, named by its
handle and resolved on the server to the derived agent id every
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

Every data node's event store holds what that node published, and the events of
a node without `data` it keeps in
[custody](../guides/deployment.md#custody-the-rows-of-a-node-without-data), and
nothing else, so the history questions — `events`, `event`, `event_series`,
`trace`, `turn`, `turns`, `phases`, a seat's `llm_history` on `agent`, the
delivery and outcome counts on `integrations`, and the live projection's boot
seed — are answered by **every live data node at query time**: the
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
  asked is always one and always answered — it is a data node, since serving
  the API takes the `data` role, and a failure of its own store is an error,
  not a gap.
- `complete` is true only when the node roster could be read **and** every
  node on it answered. A node that did not is named with why: no answer
  inside the two-second fleet read budget, a build speaking another version
  of the scatter's protocol, a reply that could not be read, or its own read
  failing.
- **Every node answers in one protocol version, its own**, and refuses a
  question in any other — a node on another build is named, with that
  reason, on every answer, and nothing of what it holds is merged. No node
  is asked to answer around a filter it would drop, a count it would never
  send or the instant it would ignore, so every part that is merged was read
  exactly as asked: the axis's window is cut alike on every node, a window
  the history clips down to the bucket the horizon falls in, and the serving
  node drops that partial first bar after the sum (see [the time
  axis](#the-event-logs-time-axis)).
- It sits at the top of each answer — beside the record's own fields on
  `event` and on `event_series` — and is `null` on `agent` and `integrations`
  when the history could not be read at all.
- **A node that has left the fleet is not asked**, because it is not live:
  its turn-level detail left with it. The spend and turn counts it recorded
  are answered by the replicated `usage` domain instead.
- **A stateless node's row is listed and counted once**, though for a moment
  two data nodes can hold it — a
  [custody](../guides/deployment.md#custody-the-rows-of-a-node-without-data)
  batch the second keeper wrote before the first had settled it. A listing
  holds it once by its `(timestamp, id)`; every count — `event_series`'s bars,
  `total`, `failed` and `by_category`, `trace`'s and `turn`'s `total`, a
  `turns` row's tokens, `phases` and `duration_ms`, and the outcome counts on
  `integrations` — counts it once, because each node counts the rows it keeps
  and names the ones it has not settled, and the serving node asks which node
  keeps each named row before it adds it.
- **`event` not found** answers `not_found` naming any node that did not
  answer, because a link whose node was merely silent is a different fact
  from a dead one. An event older than the 30-day history is not found
  either: it is past the horizon every read of the log stops at, and a row a
  node's sweep has not deleted yet is not history it serves — every node
  floors its lookup by id at the serving node's horizon.
- **Every question is asked at one instant** — the serving node's clock when
  the question arrived — and that instant travels with it, so every node
  floors the 30-day history at the serving node's horizon rather than at its
  own, and one node's part of an answer (a turn's rows, its count, its ending
  and its traces) is read at that one instant throughout. A node answering a
  second late, or with a clock a little ahead, therefore holds back nothing
  the others include, and a question carrying no instant is refused rather
  than answered as of a node's own clock.
`turns` merges in two passes — every data node's page, then every data node's share of
exactly the turns any page listed — so a turn resumed on another node after a
restart is one row folded from both halves. The window is pinned to the asker's clock
first — and the instant travels with the question, so every node floors the
window and its share of each turn at the asker's 30-day horizon rather than at
its own — and the merged turn is held to it whole: a node's half of a resumed
turn can start inside the window while the turn began before it elsewhere.

A turn is **paged where a node lists it**: at the earliest start at which any
node's own page lists it, which each node says of its share in the second
pass. That is not always its `started_at`, which is where the turn began: a
turn resumed across nodes is selected by each node on its own half, so under
a filter the whole turn passes, the half where it began may not — the clean
half of a turn that failed later, under `failed=true`; the half that names no
item of one charged to an item only by its later records, under
`work_item=`. Paged at its `started_at`, such a turn could fall below where a page was cut
and behind the cursor that page returned, and so on no page of the walk.
Walking `next` lists every turn exactly once, two turns that start at one
microsecond included, whichever side of a page's cut they fall: the cursor is
that position and the turn's id, and every node resumes after exactly the turn
the last page ended on. The rows are newest first by that position, the higher
`turn_id` first within one microsecond, so a row whose `started_at` is earlier
than where it is listed can sit above rows that began after it.
`next` is the fleet's cursor:
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
| `operator_acted` | Every [operator tool](#operatormcp--your-own-assistant) call that is not a proven read, on **all three** transports: a button on the dashboard (`/operator/act`), a person's own assistant (`/operator/mcp`) and a script on the [human write surface](#the-human-write-surface) (`/worklet`, `/doclet/pages`), whose verbs with no tool behind them are recorded too. A write Ask Crewlet sent after you approved it is recorded under the transport `assistant`, its credential `assist:<grant>`, with an `assistant` reference (`conversation_id`, `turn_id`, `grant_id`, `session`, `proposal_id`) and `conversation_id` beside it as a listing field |
| `backup_requested` | Every [`POST /backup`](#post-backup) that began copying, whether the copy finished or not |
| `assistant_conversation_read` | Every administrator's compliance read of a person's Ask Crewlet conversation: the reader, `conversation_id`, `owner_id` and the reason they gave, and `file_id` for a compliance download of one file it keeps. The conversation itself is never copied here |
| `assistant_turn_completed` | Every segment of a person's Ask Crewlet turn: answered, stopped, failed, out of budget, or parked on a proposal. It names the conversation, the turn (`assistant_turn_id`, never a seat's `turn_id`), how it ended and every step's tool, route pattern, outcome, HTTP status and bytes read, and never an argument, an answer or the person's question |

Each carries `source: "operator"` on the envelope, so
`GET /events?source=operator` is the runtime audit on its own. Who acted is
[the one attribution](#who-a-write-is-attributed-to) every surface with a
request records: `actor` (the seat's handle for a person, or for a token or
service account bound to one, and the login otherwise), `actor_kind` (`human`, `operator` or `system`) and
`operator_id` (the credential — `session:<lineage>`, `pat:<id>`, or a Tier A
token's login) — resolved once for the call, so a directory rebind landing
while it ran never makes the record name a different person. All of them are filed
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
the row's own `actor` column), `tool` (an `operator_acted` call's tool),
`dir` (a `backup_requested` copy's directory) and `conversation_id` (an
`assistant_turn_completed` turn's conversation), beside `node`, the envelope's
publishing node, which every event row carries. That is what the dashboard's
Audit log and backup history draw from.

**What is not here.** A request refused before any tool ran — a read sent to
the write transport, a token that is nobody, a malformed body, a backup with
no destination or with one it refused (the `400`s below), a chat erase or
prune missing its confirmation, its reason or its cutoff — changed nothing and
is not recorded — except where the write surface refused it on an authority
decision of its own, on a stored row it read before any tool ran (a remark
somebody else wrote, an item's own project), which is `refused` like a tool's
refusal, because "you may not" is a fact about a person an audit is read for.
A gesture with no tool behind it — a purge, a page's rename, trash and restore,
a comment take-down, a remark rewritten, and [a chat erase, prune or
horizon](#a-chat-erase-prune-or-horizon) under its verb (`chat.erase`,
`chat.prune`, `chat.channel.retention`) — is recorded as a tool call is. A
chat erase's `reason` is not on its row, by the rule above: it travels on the
erase record itself, on the chat log, and in the serving node's `chat_erased`
log line — no row any data node keeps carries it. A
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
| `since` / `until` | (the 30-day history, to now) | RFC 3339 instants, half-open. Both edges are snapped **outward** to whole buckets, so the first and last bars are whole ones — a partial bar has a height that means something different from its neighbours' and a reader has no way to know. The one edge no snap crosses is the 30-day history (`event_history_seconds`): a window it clips — the default one included — begins at the **first whole bucket inside it**, since a bar reaching below it could count only its upper part, and a window lying wholly below that bucket covers nothing (`since` equals `until`, no bars). The answer is labelled with the window it actually covers. |

The answer is `{bucket, since, until, bars, total, failed, by_category}`.
`bars` is **every** bucket in the window including the empty ones, so a
quiet hour is a gap of full width rather than a bar the chart squeezed out.

The whole answer is cut against **one instant** — the serving node's clock
when the question arrived — and every count in it, `bars`, `total`, `failed`
and `by_category` alike, keeps rows from 30 days before that same instant.
Across a fleet that instant is sent with the question, so every node cuts the
same bars — a window the history clips is cut down to the bucket the horizon
falls in, its first bar counting only what lies above the horizon — and
floors what it counts at the serving node's horizon rather than its own. The
serving node sums every node's bars and only then drops that partial bar, so
no bar in the answer starts below the horizon (see `since` above), every bar
lies wholly inside the history, and a row any node holds inside a bar — the
first one included — is counted in it however long that node took to answer.

Each bar is `{at, count, failed}`. `failed` is how many of the bar's rows
reported a failure — by the rule a turn's own `failed` mark uses: the event
said so, or its type is one (`llm_unavailable`, `budget_exhausted`,
`turn.guard_breach`, `executor_failed`) — and it is a **split** of
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
which are whole buckets. On a window the history clips, the chips reach down
to the horizon itself and so also count the rows between it and the first
whole bucket, which no bar draws.

Two refusals, both **400** rather than a smaller answer:

- **`related_agent`** is not accepted. That filter folds each page's trace
  siblings into it (see above), so a count over the predicate alone is a
  smaller set than the listing shows — and a bar that disagrees with its
  own rows is the one thing this axis exists not to be.
- **A window of more than 1,500 buckets** is refused naming the bucket and
  the span. Truncating would put a month's heading over a day of bars and
  coarsening would answer a different question from the one the axis is
  labelled with; the caller's fix is a coarser bucket or a shorter window.
  The buckets counted are the ones every node cuts, so on a window the
  history clips they include the one the horizon falls in, which the answer
  then drops: the number every build refuses at is one count, not two.

### The live token meter

`budget` carries the fleet's **shared token counters** as the budget gate
enforces them: for the company and for every seat whose `token_budget` caps a
window, **one entry per capped calendar window** — the day, the ISO week and the
month on the company's clock — with that window's spend, its ceiling (the
company's and a seat's, from the revision this node runs), the gate's refusal stamp and the engine's judgement of it. It
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
  no screen holds a threshold of its own: `refusing` when the window has no
  room left for a single token — which every window that refused a round has,
  since the refused round is counted, and the condition a seat is
  [parked](../concepts/agent-runtime.md#the-budget-park) on — `near` once
  `used` reaches **nine tenths** of `limit`
  (`engine.BudgetNearFraction`, served as `near_fraction` on
  [`GET /budgets`](#get-budgets)), and `ok` otherwise.
- `refused_at` is when the window last turned a call away, in UTC, and
  **absent** while it has not: a round whose charge it refused, or work turned
  away before its first call because the window was already full — a turn's
  next call, a seat's delivery parked, a person's question, a reflection pass —
  each recorded as the gate's refusal on the scope a charge would be refused
  by, the company's before the seat's. A round it turned away is counted in
  `used` like any other — the vendor billed it — so a window that refused one
  reads past its `limit` by that round, and every later charge is refused
  against that figure. The stamp is kept in the shared counter beside the
  spend, so every node reports the same one, and it clears on the scope's next
  admitted charge or when the window turns over. A window whose ceiling was
  raised after a refusal keeps its `refused_at` until then, under a `state` of
  `ok` or `near`: it has room again, and the stamp is history.

Every node publishes a `budget_meters` snapshot of the counters as soon as its
seat host is running and every **15 seconds** (`engine.BudgetReportInterval`)
after that — and **at once** when a budget window first refuses a charge, so
the moment a company or a seat stops reaches every open screen with the
refusal rather than up to a tick later; a repeat refusal in the same window
waits for the tick — and the projection folds each one in as it arrives. Until
the first one lands, `budget` is **`null`** — nobody has read the counter —
which is a different fact from a report whose `org.windows` is `[]`, "nothing
is capped". A company with no ceiling anywhere publishes exactly that: an empty
list and no seats, without reading the counter. A node whose read of the
counter fails publishes nothing that interval, so the meter keeps the last
reading it had rather than drawing zeroes.

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
invocations, max_rounds, round_ceiling, round_started_at, run_started_at,
running_calls, steers,
cache_read_tokens, cache_write_tokens, work_item, node, in_progress, versions }`
while an LLM call is under
way. The fields are these:

- `prompt_messages` is `[{role, content, sections}]`, the conversation the phase opened with. `sections` is that message's outline as the prompt's builder recorded it — `[{key, title, bytes, headed?}]`, tiling `content` exactly, with `headed: true` on a part that opens on its own heading line whose text is `title` (absent on a part the builder named, which may still open on a heading of its content's) — and is absent on a message no builder outlined or whose text is not valid UTF-8; a finished phase record carries the same maps as `system_sections` and `user_sections`. See [a prompt carries its outline](../concepts/turn-engine.md#what-streams-during-a-turn) for the keys and the rules a reader checks before slicing by one.
- `round_narration` is `[{round, content, declined}]`, what the model said in each round so far; `declined: true` marks a round that answered in prose where the phase had to end in a call, and is absent otherwise.
- `rounds_used` is the round the phase is on, counted from one: the rounds that have come back, and the one in flight from the frame the engine publishes as that round's provider call is made. A finished phase record carries the same count under the same name.
- `rounds` is each round's own timing and tokens — a round is one model request, as the harness's record shows it — `{round, started_at, duration_ms, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, tool_calls, stop_reason}`. This used to be the count, under the name the phase record uses for the list.
- `invocations` is the phase's harness runs that have **ended**, each `{started_at, duration_ms, provider_key, model, outcome, first_round, last_round, requests_observed, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, tool_calls}`, as on the [phase record](../concepts/turn-engine.md#what-a-turn-records-about-its-time). The run in flight is not on it until it ends: its rounds are the ones past every entry's `last_round`. Empty until the first run ends.
- `max_rounds` is the round cap currently granted, which an extension can raise mid-phase.
- `round_ceiling` is the highest value any extension may raise `max_rounds` to.
- `round_started_at` is when the latest round made its provider call. It is later than every entry in `rounds` while that call is out, and equal to the last entry's start while the round's tools run — only the first is a model call in flight.
- `run_started_at` is when the harness run in flight was launched — the run whose rounds are past every entry in `invocations`. Absent between runs, so a reader draws the run in flight from its launch rather than from the first round it happens to have timed.
- `running_calls` is every tool call running right now, in the order the calls arrived, each `{id, round, name, arguments, started_at}` — a **list**, because a harness may make a response's calls at once. `id` is the call's id, which its `tool_executions` row carries as `call_id` once it has answered. It is absent while no call runs, and never carried forward from an earlier frame: a frame that stops listing a call means the call returned.
- `steers` is every person's note the phase has read so far, `{round, note_id, boundary}` — the round whose provider call first saw it, and the kind of step that read it (`round`, or `brief`, `tool_batch`, `finish` or `step` for a read by the phase's executor in its box, which is round 1). What the note said and who sent it are on the turn's `agent_turn_steered` rows. See [steering a running turn](#steering-a-running-turn).
- `node` is the node running the call.
- `versions` is `{prompt, response, narration, executions, rounds}`: the version of the copy the call holds of each of its heavy fields — `prompt` numbers `prompt` and `prompt_messages`, `response` the `response`, `narration` the `round_narration`, `executions` the `tool_executions` and `rounds` the `rounds` — a number the node's projection never hands out twice, taken whenever the field is written: every field of a call when the call begins, and after that each field a frame changes. So on the node a tab's socket reads, a version only grows, for every call under every key — and a call cleared and built again under its own key (`turn_id`, `phase`, `iteration`) is newer in every field than the copy a tab held from before. A tab that reconnects is sent a snapshot, which replaces every copy it holds. Every surface that carries a call carries it.

Beside `live_call`, each seat carries `live_call_seq`: the node projection's
own sequence — the one `versions` are taken from — read when the seat's call
last changed: began, took a frame, froze as failed, or was cleared to `null`.
It is on every surface that carries the call, a `null` call included, and it
orders the call slot itself where a field's version cannot: across a clear,
which leaves no version behind, and across two calls, whose fields are never
compared (see [What the projection carries, and what the wire
sends](#what-the-projection-carries-and-what-the-wire-sends)). Like a version it
is a fact about one node's projection, and a snapshot resets what a tab holds.

Beside `live_call`, each seat carries `turn`, `last_turn` and `paused`: the turn the seat is on, with its `stage` of `context`, `phase` or `parked`, the newest turn it ended, and who paused the seat, when and why (`null` while nobody has). See [Agent States](../concepts/agent-runtime.md#agent-states).  A call whose phase failed keeps `in_progress: false`
plus `failed: true` and an `error` object, so the dashboard renders the failure
instead of an answer that never arrives — and no `partial_round`, because the
phase is over and nothing is still arriving. The failed call stays until the
seat's next work replaces it, and the same phase running again under the same
turn and iteration — the review of a parked turn's resume that is held or
retried — replaces it with a call of its own rather than continuing the failed
one's rounds.

**A failure is told by its kind, and the company's configuration only to an
admin.** The `error` object and a seat's `last_error` are
`{kind, detail, phase, turn_id, at, event_id}`: `kind` is the classifier's
(`rate_limit`, `auth`, `server`, and the others), which the dashboard words for a
person, and `detail` is the failure in its own words, sent only to a reader who
is an admin. Those words name the company's `providers.llm` entries ("llm chain
exhausted after 1 providers (default)") and carry whatever a vendor or a tool
wrote, and which entries a company configures is an admin's to read; the same
goes for each `invocations` entry's `provider_key`, and for the `provider_key`
of an executor on the `executors` push and snapshot and on the
[`executors`](#get-executors) board, which every other
reader, a seat's lead included, is sent without. The full text stays in the
engine's log and on the turn's own record.

#### What the projection carries, and what the wire sends

A running phase moves its seat's row on every progress frame — five times a
second while a round streams — so both the frames into the projection and the
pushes out of it are trimmed, and each is reassembled on the other side:

- **Into the projection**, the `prompt` and `prompt_messages` are sent **once**,
  on the phase's opening frame, and carried forward from there (a 30 KB system
  prompt does not change mid-phase, and past `queue.MaxPayloadBytes` the
  publish is refused outright and the live row simply stops); a tool result
  and the joined `response` are sent **tail-bounded** on a live frame, because
  they are what a reader is watching the end of. The durable
  `agent_phase_completed` record keeps every one of them verbatim, which is
  what a reader opens the finished card for.
- **Out to a tab**, an `agents` push carries a call's HEAVY fields — `prompt`
  and `prompt_messages`, `response`, `round_narration`, `tool_executions` and
  `rounds` — only when their version (`versions`) moved since the last push for
  the same call, and leaves them out otherwise: the key is ABSENT, never empty,
  and the tab keeps the copy it holds. They move once a round or once a tool
  call; between those moments a push is the round being written
  (`partial_round`), the call in flight and the light fields. Measured over one
  long phase streamed on the engine host (thirty rounds, 1 311 frames), each
  open tab was sent 185 MiB with every push whole, and is sent 5.9 MiB.
- **A dropped push is noticed by the next one.** The socket drops a slow tab's
  oldest envelope rather than stalling every other tab, which is why the pushes
  used to be whole: a frame that carried only what changed, dropped, would
  leave a tab wrong with nothing to say so. Every push names every version, so
  the push after a dropped one names a version past the copy the tab holds,
  and the tab asks for the call whole ([`live_call`](#queries)) rather than
  drawing the older copy as current. A field a push carries at a version older
  than the one a tab holds — a push overtaken by a snapshot or an answer — is
  not taken. The handshake snapshot, `GET /agents`, the `agent` answer and the
  `live_call` answer always carry every field.
- **An answer is ordered against the pushes that overtake it.** The
  `live_call` answer is computed on its own and can reach the tab after a push
  the node generated later, so it carries the seat's `live_call_seq`, and the
  tab compares it with the newest it has applied for the seat. An answer at or
  past it is merged like a push carrying everything. One BEHIND it describes
  the slot as it was before those pushes: when they cleared the call or began
  another, it is dropped — so a stopped seat never shows the running call
  read a moment before its completion — and when they are the same call, it
  still brings each heavy field it holds at a newer version than the tab's,
  while the light fields stay the pushes'. And if a push found the call behind
  while the answer was out, the tab asks again once that answer lands behind
  it, rather than waiting for a next push a frozen call never sends. An
  `agents` push whose `live_call_seq` is behind an answer already applied
  leaves the call as the answer had it, and its other fields still apply.
- **A version is never handed out twice**, so "moved" and "newer" are one
  question. Counted per call, a call cleared and built again under the same
  turn, phase and iteration would number its fields from one again, and a tab
  that had missed the clearing push and the first one after it would read
  every later push as older than the copy it held from before, and keep that
  copy.

### `WS /ws/stream`

Upgrades to a WebSocket.  All frames are JSON envelopes of the form
`{"kind": "...", "data": ..., "ts": "<iso8601>"}`.

> **The socket is the dashboard's channel for state, not for everything.**
> The projection arrives here as pushes, and every question the query
> registry answers is asked and answered here too. What the socket does not
> carry is REST: every write — through
> [`/operator/act`](#operatoract--the-dashboards-write-surface) as the
> principal the session resolves to, through `/iam`, or through
> `/config`, `/secrets`, `/setup` and `/backup`, because a write has to be
> able to say whether it happened and a frame into a dropped socket has no
> answer — and the few guarded reads no query answers (`GET /secrets`,
> `GET /config/references`, `GET /setup/integrations` and its passes), which
> the dashboard reads through one loader
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

**Who may open it, and what each reader receives.** The handshake admits
every principal the guard resolves whose stage may act — the overview is
everybody's — and refuses one whose stage may not `403` before the upgrade.
Every push kind is received only by a socket whose caller holds what that
kind's question takes: `event` needs `admin` (it is an `events` row, carrying
every phase's prompt and response), `inbox_changed` needs `worklet`, the eight
`chat_*` kinds need `chatlet` and a caller acting as a human seat — a socket whose
principal acts as no seat is shown no chat at all, a public room included — and
each chat frame then goes only to the sockets whose seat may see its room
(`chatlet.Visible`), and every other company kind is everybody's. The `agents`, `seats` and `executors`
pushes carry a call's and a run's **body** only to a caller who may read that
seat's — an administrator, or its lead — and to everybody else its status with
`body_withheld: true` ([A status reader never receives a
transcript](#a-status-reader-never-receives-a-transcript)). The snapshot is
built for the same audience. When the socket's credential is decided again and
it resolves somebody else — another login, another seat or none, other
permissions — or a published company moves which seats' bodies the caller may
read, the socket closes `1013`, and the reconnect's handshake builds the
snapshot for the new audience while the client asks who it is again — see
[Close codes](#close-codes).

#### Pushes

Server → client kinds:

| `kind` | When | `data` |
|--------|------|--------|
| `snapshot` | First envelope after the upgrade succeeds, and again on reconnect. | Same payload as `GET /stream/snapshot` — agents carry their in-flight `live_call` and its `live_call_seq`, so a reconnect re-renders the live row and starts the ordering of the call slot afresh. |
| `event`    | Every engine event published to `crewlet.events.>`. | `{ id, type, timestamp, source, actor, summary, category, trace_id, span_id, parent_span_id, topic, agent_id?, channel_id?, payload }` — `agent_id` the seat the event concerns and `channel_id` the agent-to-agent channel it belongs to, each read by the rule that fills the store's own column, so the event log narrows its live rows to a seat or a channel exactly as the store narrows its pages — the same shape as a `/events` row, plus the full event `payload` (from which the snapshot feed's `failed` flag is derived).  `agent_phase_completed` events carry the system prompt, response, and tool calls, so LLM invocations stream live; `agent_turn_progress` events (per tool-call round, tagged with `turn_id` / `phase` / `iteration`) stream the in-flight call before its phase record exists. |
| `agents`   | After an event moved one or more agents — or a read moved their state: a run record reconcile, or the seat-lease read the five-second tick makes. | The changed agents' overlays, each with its `agent_id`, its `role`, its `activity` and its `stopped_reason` — the *result* of applying the change, so a client merges them rather than running its own state machine over the raw stream. A client merges an overlay onto the roster row carrying the same `agent_id` and DROPS one for a seat its roster does not carry: a seat reaches the roster through the snapshot or a `seats` push, each of which carries its live overlay already merged. A `live_call` carries its heavy fields only when their `versions` moved since the last push for the same call, and a client keeps the copy it holds of one left out; every row carries the seat's `live_call_seq`, a `null` call included, which orders the call slot against a `live_call` answer (see [What the projection carries, and what the wire sends](#what-the-projection-carries-and-what-the-wire-sends)). |
| `seats`    | After anything changed the company — a config activation, a hire, a move or a removal among them. | The COMPLETE seat list, replacing what the client holds, each row carrying its `agent_id`, its current `handle` and its `role` (name). Distinct from `agents` on purpose: that one is a per-seat merge, and a merge cannot express the deletion of a seat that has gone. |
| `executors`| After an executor was launched in its box, asked a question, finished or was lost, and after a reconcile against the durable run record changed the set. | Every executor in flight. |
| `tokens`   | On the shared 5-second tick, when a spend record arrived or one aged out of the live window since the last one. The fold runs on the tick rather than on the publish, so a busy company costs one aggregation every five seconds rather than one per phase. | The spend rollup, same shape as `GET /tokens/breakdown`. |
| `budget`   | After a node's token meter report is applied (every node reports at start, every 15 seconds and at once when a budget window first refuses a charge, a company that caps nothing included). | `{ meter_id, seq, timezone, org: { windows: [...] } }`, the org-wide half: one entry per capped calendar window, each with its span, spend, ceiling, refusal stamp and `state`. Per-seat figures ride on each agent's overlay in the `agents` push. See [the live token meter](#the-live-token-meter). |
| `org` / `tools` / `schedules` | On the same edge as `seats`: every published company. | The new org tree / tool surface / schedule list, so open tabs stop showing seats that no longer exist. |
| `health`   | Pulsed every 5s by a **single shared tick** (one timer for all clients, not one per connection). | The whole [health envelope](#the-health-envelope), exactly what `GET /health` answers. There is no query for it. |
| `result`   | Reply to a client `query` that succeeded. | `{ id, what, data }` — `id` echoes the request's. |
| `error`    | Reply to a client `query` that could not be answered, a `watch` that was not installed, or an `artifact_join` refused or an artifact's room withdrawn (`what: "artifact_join"`, naming the `artifact`; see `artifact_join` below). | `{ id, what, error, reason?, grants?, role?, retry_after?, refusal?, detail? }` where `error` is a code, and an `unauthorized` one adds `reason` and `grants`, exactly as the REST envelope does: the rule that decided, and the grants any one of which would have admitted the caller (an EMPTY list means no grant would, because what is missing is a relation such as leading that person), and on an artifact refused for a role below the one the verb needs, `role`, the role to ask the owner for. A refusal on authority is worded by the authority table's one mapping on both transports, a query's REST twin and every other surface, so the code, `reason`, `grants` and `role` a frame carries are the ones the same decision is answered with everywhere — and a caller with no relation to an artifact is answered `not_found`, with nothing beside it, as an id nothing holds is. **No question or watch is decided by a verb that asks a recent proof or needs a person present** — a question registered that way is refused when the node starts, and the build checks the verbs an answer decides by inside itself — so a frame, and a query's REST answer, is never `step_up_required` or `token_refused`: the socket cannot carry a step-up. A **watch refusal** has no `id` and names `what: "watch"`: `unauthorized` when the caller may not watch that seat (with its `reason` and `grants`); `unavailable` when this node could not read the chart that decides it or the directory a login resolves through (carrying `retry_after` as a query's `unavailable` does, by the same rule and with the same `0` for a read no wait clears, but never the `refusal` or `detail` behind it, because what the directory says about a login names the seat it is bound to); and `not_found` when a login names no record, which only `admin` is ever told. For a query, the codes are: `unknown_query`, `unauthorized`, `not_found`, `bad_params`, `unavailable`, `artifact_trashed`, the five codes for a configuration revision this node cannot open (`unreadable_revision`, `unsealed_revision`, `unreadable_fleet_revision`, `unsealed_fleet_revision`, `unknown_seal_revision`, each with a `detail` naming what fixes it, as [`/config`](#a-revision-this-node-cannot-open-is-refused-whichever-route-reads-it) answers them), or `query_failed` for every other failure (the reason goes to the log, never to the socket). **`artifact_trashed` is not `not_found`**: it answers a question about the content of an artifact in the trash — its datastore — asked by somebody the artifact is disclosed to, the state its owner's restore ends; its REST twin is the artifacts routes' own `409 artifact_trashed`, and a reader with no relation is told `not_found`. **`unknown_query` covers a surface this process does not have**: a question whose source is not wired here is never registered, so it is unknown rather than empty and never carries a `Retry-After`, because waiting cannot give this node a store it was not configured with. Its REST twin is `404`. **`unavailable` is not `query_failed`**: it says this node understood the question and cannot answer it *here*: a projection still catching up after a restart or a fresh join, a coordination store it could not reach, no copy of the estate to answer from (Worklet, Doclet and Chatlet are read through the [estate router](../concepts/scaling.md#how-a-request-reaches-the-estate), so a copy out of service or a data node restarting is a holder to ask again rather than a fault), or a refusal by its state log, so a client does not report a fault. A query's `unavailable` frame also carries `retry_after`, the whole seconds before asking this node again, `detail` — a sentence for the person reading it: what happened and what to do, whose advice to try again agrees with `retry_after`, carried for every condition, a coordination store that did not answer and no data node serving included, in the words the act route and the work surface tell the same condition in; the refusal's own detail, which names positions and settings, goes to the answering node's log — and, where a state-log refusal is behind it, `refusal` (its code: `behind`, `log_full`, `deferred`, `broker_refused`, …). **A `retry_after` of `0` is the answer, never an omission**: waiting will not change it (a log at its byte ceiling, a record this node cannot decode, a barrier its broker refused), so the client asks another node or waits for an operator rather than polling this one; see [Read consistency](../guides/consistency.md#the-fourteen-refusals). Its REST twin is `503` with the same `detail`, and `refusal` where there is one, in the body and `Retry-After` carrying the hint, absent where the hint is zero. **A work, page, chat or search question on a node that has not been handed a company yet** is `unavailable` with `refusal: "no_active_revision"`, Worklet's, Doclet's and Chatlet's one sentence as its `detail`, and `retry_after: 15` (the reconcile poll that brings the company), and its REST twin is `503 no_active_revision` with the same detail and `Retry-After: 15`, as every other surface over those halves answers such a node. **`bad_params` is not `query_failed` either**, in the opposite direction: the node understood the question and *refused* it (a parameter missing, malformed, or outside the set the field accepts), so the fault is the caller's and retrying sends the same bad request again. Its REST twin is `400`, and it carries **`detail`**: the refusal's own sentence, which names the parameter to change and what it accepts (`days is 91, and a spend window is 1 to 90 company days. Ask for at most 90`), written for the person who will read it: no class name, the engine's or a finer one (`tokens.ErrWindowLength`, which a Go caller tests with `errors.Is`), and no echo of the query's name; the REST `400` body carries the same `detail` beside its `error`. A frame has ONE `detail` key, `unavailable`'s words, `bad_params`'s sentence and a revision refusal's remedy alike. Every other code's text stays in the node's log, since a failure's own text can carry a path. The dashboard asks again after exactly a frame's `retry_after` (and not at all after a `0`), and keeps no wait of its own, so this hint and the `Retry-After` on a REST refusal are the only retry clock it has. |
| `pong`     | Reply to a client `ping`. | `null` |
| `inbox_changed` | A committed tracker batch wrote the watched seat one or more notices. Every node serving the API holds the estate (`ingress` requires `data`) and applies every tracker record, so each node pushes to its OWN sockets from its own applier after the batch commits. While a node's own copy is out of service it stays in rotation and pushes a sound data node's movements instead, relayed over the broker, and withholds its own applier's; every switch of source sends each watched seat one frame with `unread_delta: 0` and an empty `subject` and `reason`, because what moved during the switch is not replayed: read the inbox again, as on any frame. One frame per seat per batch. | `{ handle, unread_delta, subject, reason }`, and the frame itself carries `seat`. **Identifiers and a count, never content**: `subject` is the id of the object the newest of those notices was written on (the task for a task commit, and the PERSON for a `prioritised` notice, whose task `work_inbox` names as `task`), and `reason` the reason it was routed under, and what the notices say is read through `work_inbox`, the question that decides who may read them. `unread_delta` is a **hint** (the notices the batch wrote, which a redelivery the engine has already collapsed may count twice), so a client re-asks rather than adding it to a badge. **Routed by seat**: it reaches only the clients whose `watch` for that seat was allowed, never every tab. It needs `worklet`, the permission `work_inbox` takes. |
| `artifact_changed` | A committed artifacts batch moved an artifact whose room this socket is in: a version published, its title, description, icon or a label, its shares, its assets, the trash, a restore, a transfer, a purge or the folder it is filed in. Each node pushes from its own applier; while its own copy is out of service it stays in rotation and pushes a sound data node's movements instead, relayed over the broker, and every switch of source pushes every room a `reset`, as for `inbox_changed`. | `{ artifact, what, head, position }`, where `what` is `version`, `meta`, `share`, `asset`, `trash`, `restore`, `purge`, `transfer`, `folder` (it was filed in another folder; a folder's removal, which re-files what it held, is not pushed per artifact) or `reset` (this node cannot say which artifacts moved, or what moved across a switch of source, so re-read; the room is decided again). **A hint, never content**: re-read through the questions that decide what you may see. |
| `artifact_data` | A committed datastore batch changed documents of an artifact whose room this socket is in. Each data node pushes from its own applier, after the commit, or from a sound data node's while its own copy is out of service, as `artifact_changed` does. | `{ artifact, deltas?, keys?, reread?, schema?, resync?, gone?, through }`. **Shaped per reader**: a document in an `own_rows`, `editors` or `private` collection is pushed only to the members its rule admits, by the role each member's join decided, and a batch a member may see none of is not pushed to them at all. The changed documents travel inline (`deltas`: `collection`, `doc`, `version`, `deleted`, `body`, `read`, `created_by`, `updated_by`, `updated_at`) while the frame stays within 64 KiB, and as `keys` with `reread: true` past it. `schema` says the collections changed; `resync` says this node's pushes may have gaps, a switch of source among them (read whole); `gone` says the datastore was purged. `through` is the store's version after the batch, the `since` of the next catch-up. |
| `artifact_presence` | Who is in an artifact's room changed: somebody joined, left, or moved what they are doing or looking at, on this node or another. Coalesced to at most ten a second per room, each carrying at most ten new or changed members (the one waiting longest first, the rest in the frames that follow). | `{ artifact, upsert, leave, counts?, truncated }`: the members now presented that are new or changed (`session`, `seat`, `kind`, `activity`, `focus`, `at`, and for an agent `turn`, the run of the turn whose tool call put it there), and the sessions no longer presented. Past 50 members a room presents the 50 most recently active with `truncated: true` and `counts: { human, agent }` of the rest, and returns to everybody below 40. **Told, never stored**: see [Presence](#presence-in-an-artifacts-room). |
| `chat_posted` / `chat_edited` / `chat_deleted` / `chat_reacted` | A committed chat record put, rewrote, tombstoned or reacted to a message — sent only to the sockets whose seat may read the room (see [Chatlet](#chatlet)), decided at the batch's own position. | `{ channel_id, op, op_id, message_id, channel_seq, thread_root?, author, author_kind, at, position, excerpt?, mentions?, channel_name?, channel_kind? }` — evidence that something landed, never the message: `channel_seq` is contiguous per room, so a client holding 41 and handed 43 reads 42, and `op_id` is how a tab recognises its own gesture coming back. |
| `chat_room_changed` / `chat_membership_changed` | A room was created, patched, given a horizon, erased or pruned; its membership was replaced. **The people a membership change took out of a private room are sent its `chat_membership_changed` too**, though at the batch's own position they may no longer read the room — it is how their rail lets the room go without waiting for its next read; the frame names the room and the record and no member. **A `chat_room_changed` naming no room** (`channel_id` empty) is a GAP: this node switched the source its chat batches come from, or fell behind and dropped some, and the screen reads every room it holds again. | The same shape, the room's own fields only. |
| `chat_cursor_moved` | A person's own read state moved — a flush, a mute, do-not-disturb — through any surface on any node: every node serving sockets watches the coordination record and pushes the change to that seat's own sockets, and to nobody else's. | `{ handle, cursors?, muted?, dnd_until?, updated_at?, revision }` — the WHOLE state; keep the one with the highest `revision`, which the rail's `read` carries too. |
| `chat_presence` | Who is in the chat room this socket's `presence` frame named (`room: "chat"`) changed: somebody opened it, left it, started or stopped typing, or a seat began or stopped working on an answer there, on this node or another — and the room whole, as the socket joins it, which reaches the socket ahead of every change after it. Only a socket whose seat may read the room is put in its presence, and a record that narrows the room takes out every socket whose seat no longer may. Coalesced as `artifact_presence` is. | `{ channel_id, roster?, upsert, leave, counts?, truncated }`: with `roster: true` the room whole — replace what was held; a roster naming nobody is the socket taken out of the room — otherwise the members now presented that are new or changed and the sessions no longer presented, in `artifact_presence`'s member shape. A person's member is `viewing`, or `editing` while they type (the engine puts it back to `viewing` six seconds after the last re-assert); a seat at work is an `agent` member, `editing`, with `focus.thread` the message its answer will land under (empty for the room), `status` the phrase the room shows and `turn` its run. **Told, never stored**, by the presence hub every room shares (ADR-0038). |

#### Client frames

Client → server kinds:

| `kind` | Purpose |
|--------|---------|
| `ping` | Keepalive; server replies with `pong`. |
| `watch` | Become a recipient for one seat's seat-routed frames: `{ kind: "watch", seat }`. An empty `seat` clears it (always allowed), and one socket watches one seat at a time: a tab is looking at one screen, so a second watch replaces the first. **`seat` names a record the way `work_inbox`'s `handle` does**: a seat's handle, either of your own names, or somebody's **login**, which is resolved to the record their notices are kept under (the seat the identity directory binds them to), and the watch is installed there, because that is the name every `inbox_changed` frame for them is pushed to; see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for), whose rules it follows to the letter, the directory asked only once the watch is decided as far as it can be without it. A login that names nobody answers `not_found` to `admin` and is refused like any other seat to everybody else. **It is decided like `work_inbox` about the same seat**, because it buys that seat's `inbox_changed` frames: the seat's own holder, whoever leads it through the chart, or `admin`, and `worklet` either way. Three answers, and none closes the socket. **Allowed** installs the watch. **Refused** installs nothing, drops any watch the socket held, and replies `error` with `unauthorized`. It is not a `4403` close, because a client reads that as "this browser may not have the live channel at all" when all that was refused is one seat's frames. **Undecidable** (this node could not read the chart, or the directory a login resolves through) installs nothing either and replies `unavailable`, with the `retry_after` that says when asking again can change the answer: `0` where no wait will. Installing would hand the seat's frames to somebody this node could not show was allowed them. Refusing would tell a lead they lead nobody because the node is behind. **Every time the socket's credential is decided again by an identity record that names it, and on every published company, which is when a lead can move, so is the current watch**: a refusal withdraws it and says so, and an undecidable answer keeps it, since nothing has been learned against a decision this node already made. A watch with nobody resolved behind it closes the socket with **4401**. |
| `artifact_join` | Enter an artifact's room: `{ kind, id, artifact, since? }`, where `since` is the `through` this tab last held. **Decided like reading the artifact** (`get_artifact`, the owner, a share naming you, your unit or the company, or leading the agent that owns it), three-valued, and none of the answers closes the socket. **Allowed** answers `result` with `{ artifact, session, role, roster, through, resync }`: your session in the room, your role, the room as this node's sockets were last shown it with you in it — every `artifact_presence` frame the room sends this socket comes after this answer and is the difference from that `roster`, so replace the room with it and apply each push after — the datastore's version, and `resync: true` when `since` is absent or older than the datastore can still account for (read the collections whole) and `false` when a catch-up with `since` is enough. **Refused** answers `error` with `not_found` and the `artifact`, in exactly the words an artifact nobody holds is answered with — or with `artifact_trashed` for an artifact disclosed to you and in the trash, whose room carries nothing until its owner restores it. **Undecidable** answers `unavailable` with `retry_after`. A socket is in at most **8** rooms; a ninth is `bad_params`. **The join is decided again** on every decision of the socket's credential and on every share, unshare, trash, transfer or purge of the artifact: a refusal withdraws the room with an `error` frame (`what: "artifact_join"`, `error: "not_found"` — `"artifact_trashed"` for the trash — `artifact`, no `id`) on a socket that stays open, and a role that moved shapes the pushes after it. From the moment such a movement lands until its decision does, the room's `artifact_data` pushes are held back, and a member the decision keeps is sent `resync`. **An undecidable answer fails closed**, as the join does: the room stays joined (its `artifact_changed` and `artifact_presence` frames keep flowing) but its `artifact_data` pushes stay held back, and the node asks again after a backoff (250 ms, doubling to at most 30 s, or the refusal's own longer wait) until a decision lands, which then resumes the room with `resync` or withdraws it. A room is never kept at the role it held before a share was withdrawn on an answer nobody could vouch for. |
| `artifact_leave` | Leave an artifact's room: `{ kind, artifact }`. No answer; leaving a room the socket is not in is not an error. A socket that closes leaves every room it was in. |
| `presence` | Say what you are doing in a room: `{ kind, room, … }`, where `room` names the kind of room and so the field that names it. **`room: "artifact"`** — `{ kind, room, artifact, activity, focus }` in an artifact's room you are in (joined with `artifact_join`), where `focus` is `{ version?, region?, collection?, doc?, field? }`. **`room: "chat"`** — `{ kind, room, channel_id, activity, focus? }` for the chat room this tab is looking at, and this frame IS the join: a tab looks at one chat room at a time, so naming a room takes the socket out of the one it was in and puts it in the one it names once that room's own visibility admits your seat (a room you may not read puts you in nobody's presence and shows you nobody's, with nothing said), and an empty `channel_id` leaves. Send `editing` while your person types, re-asserted at least every six seconds, and `viewing` otherwise; `focus.thread` is the thread they type under. A tab re-sends it after every reconnect, since the new socket is in no room. `activity` is `viewing`, `editing` or `idle`. No answer. **Who** is doing it is never the frame's to say: the room shows the seat your credential resolved to. At most **ten a second** per socket, whatever rooms they name; frames past that are dropped, and a frame naming no kind this build keeps is dropped too. |
| `query` | Request one thing, answered with exactly one `result` or `error` frame. `{ kind, id, what, params }` — `id` is any client-chosen value echoed back on the reply. **There is no per-frame credential**: every question is asked as the principal the handshake resolved (and each decision of the credential since), and decided by what it is registered under — the overview's questions for everybody, an app's for that app, admin's for admin, and a seat's trail by `audit.seat.read`. Queries run concurrently with each other and with the push stream, so one database read cannot stall a tab's live rows — at most **four** at a time per **socket**, which is a tab's share of the node's reader pool: the pool's floor is sized as two full dashboards at four each, and a fifth query waits on its own socket rather than in the pool the engine's own reads share. |

#### Queries

Each query (`what`) is answered by the *same* function the matching
REST route calls, so the two surfaces cannot diverge:

| `what` | `params` | Answers with |
|--------|----------|--------------|
| `agent` | `{id, history}` | `GET /agents/{id}`: config, live state and `llm_history`. `history=false` answers the seat and its live state with an empty `llm_history` and no `next`, for a caller asking what a seat is doing rather than what it said: the finished calls' payloads are whole transcripts |
| `live_call` | `{agent_id}` | `GET /query/live_call?agent_id=…`. `{agent_id, handle, live_call, live_call_seq}`: the seat's call in flight with every field and its `versions`, or `live_call: null` while it has none (a seat the projection has never seen included, at `live_call_seq` 0), and the seat's `live_call_seq` beside it either way, which the tab orders the answer by against the pushes that may overtake it. Asked and answered by the seat's AGENT ID, the key every overlay is merged by, and never by its name, which two seats may share; `handle` is a label. From the projection alone, so it costs what a push does — the REPAIR a tab makes when an `agents` push names a version of a heavy field newer than the copy it holds: the push that carried the field was dropped (see [What the projection carries, and what the wire sends](#what-the-projection-carries-and-what-the-wire-sends)) |
| `agent_memory` | `{id, limit, skills_order}` | `GET /agents/{id}/memory`. A seat's trail, so it takes `audit.seat.read` — the seat's lead, or `admin`. ANSWERED BY THE NODE HOLDING THE SEAT, which it names (`held_by`, or `none` with an empty answer for a seat no node holds; `unavailable` while the holder is silent or still taking the seat) — every node keeps a copy of a seat's memory and only the holder keeps it current. Four collections, each a page (`limit`, at most 50) with its counted total beside it: the diary (`diary_total`), the episodes (`episodes_total`), the synthesized skills (`skills_total`, newest drafted first, or in `skills_order` — `uses`, the most used first, or `updated`, the most recently changed — which the holder applies to the whole set BEFORE it cuts the page, so the page is the top of the set and never the newest page re-sorted; any other value is `bad_params`) and the COUNTERPARTY PROFILES (`counterparties_total`) — what this seat has learned about the colleagues it works with, both instants carried because `last_updated_at` moves on every interaction and `last_corroborated_at` only when the traits changed. `traits` is a bag whose keys the model invents, never a fixed schema. Plus `latest_reflection` (the newest live diary entry, whatever the page) and `onboarded_at`. An episode's `ask` and `plan_summary` are their OPENINGS, at most 600 bytes each — the figure a seat is shown of a past turn's words before they are condensed (`learning.EpisodeAccountBytes`) — with each whole text's size in `ask_bytes` and `plan_summary_bytes`. The diary is keyed on the derived agent id and the other three on the seat's handle; both are resolved from the one identifier a caller has, the handle. See [the route](#get-agentsidmemory) |
| `agent_episode` | `{id, episode}` | `GET /agents/{id}/memory/episodes/{episode}`. A seat's trail, so it takes `audit.seat.read`, as `agent_memory` does. ONE EPISODE WHOLE — what the turn was asked and what it did, complete — answered by the seat's holder as `agent_memory` is (`held_by`); `episode: null` for one the seat no longer holds. See [the route](#get-agentsidmemoryepisodesepisode) |
| `memory_overview` | `{}` | **Decided per seat by `audit.seat.read`**, as `agent_memory` is, and answered for exactly the seats the caller may read — every one for `admin`, a lead's own agents for a lead, none for anybody else; a seat this node cannot decide makes the whole answer `unavailable` rather than a shorter list. Each seat's memory totals — `diary_total`, `episodes_total`, `skills_total`, `last_reflection_at` and the `latest_reflection` itself — each counted by the node holding the seat, gathered in ONE scatter rather than a read per seat, with `held_by` per row (`none` for a seat no node holds, nothing counted), an `unavailable` reason on a row whose holder did not answer, and the fleet `coverage`. The seats the caller may read, handle order, no cap. See [the section](#memory_overview) |
| `conversations` | `{handle, conversation, limit}` | `GET /agents/{id}/conversations`. The seat's own thread ledger — the engine's only account of what a seat said on a surface it does not own, and what stops it replying twice in one thread. TWO SHAPES IN ONE ANSWER, because a screen asks two questions with one navigation: `conversations` is every thread this seat holds entries in, and naming one in `conversation` adds that thread's turns as `entries`. Each turn's `reply` and `unsent` carry the same artifact and WHICH ONE HOLDS IT is the whole record of whether anybody received it — a turn can end with real work done and no way to say so. The ledger is keyed on the seat's handle. An absent `handle` is the caller's own SEAT — a seat's trail is the seat's, so a caller bound to none names one — and a named one is a seat's TRAIL rather than a person's queue, so it takes `audit.seat.read` — the seat's lead, or `admin`. The listing is a page (default 50, at most 200) with `conversations_total` beside it, ANSWERED BY THE SEAT'S HOLDER, as `agent_memory` is and for its reason: the ledger travels with a seat's memory and only the holder's copy is current (`held_by`) — see [Whose record a personal question answers for](#whose-record-a-personal-question-answers-for) |
| `event` | `{id}` | `GET /events/{id}` — one event with its full payload, and `node`, the node that published it (every stored event carries it, not only this answer's) |
| `events` | `{limit, type, source, category, trace_id, channel_id, seat, actor, agent, turn_id, work_key, work_item, suspended, failed, feed_only, since, until, before_id, before_time}` | `GET /events`. `feed_only=true` selects the rows the activity feed carries — every stored type but the ones it keeps out (`auxiliary_spend`), a trace's `agent` siblings included — and is what every read merged with the live feed asks; `true`, `false` or absent, any other word a **400**. `failed` is THREE-VALUED the same way — `true`, `false` or absent, any other word a **400** — and selects by the rule every row's own `failed` is stamped by: a stored `failed` tag, or a type that is itself a failure (`llm_unavailable`, `budget_exhausted`, `turn.guard_breach`, `executor_failed`). It is the event log's "Failures only", applied by the engine so the axis and every page are one set. `suspended` is THREE-VALUED — `true`, `false` or absent for every row, any other word a **400** — and selects by whether a completion record PARKED its turn on its executor in its box: a turn that parks and resumes writes two `agent_turn_completed` records, the first marked `suspended`, so `type=agent_turn_completed&suspended=false` is the turns that ENDED, one record each, and is what a turns axis counts over. `trace_id` selects one trace as a FILTER — paged, windowed and combinable with every other filter, where [`GET /events/trace/{trace_id}`](#routes) is the whole trace oldest first. `channel_id` selects one agent-to-agent conversation's events, by the channel id every A2A event carries (an index seek, migration `0034`). `seat` is a seat's **handle** and selects the events that seat published, resolved on the server to the id every node derives for it — so a seat since removed from the chart still names its history, and a role name, which a rename changes, is never the key; anything that is not a handle, or a person's seat's handle, is a **400** — an empty page would read as a seat that never did anything — and before a company configuration is applied it is **503** `unavailable`, since the id is derived from the company's name. `agent` is the broader question — the **related** filter: every event involving one agent seat, named by any handle it answers to and matched on its agent id, plus every event sharing a trace with one (see [Paging the event history](#paging-the-event-history)). `turn_id` selects ONE RUN of a turn; `work_key` selects every run of one unit of work — the attempts at a trigger that was redelivered; `work_item` selects every event on one work item — each turn's start, its phases and completions, an executor it launched — named by the item's identity across trackers, `<backend>:<id>` (`native:<task id>`, `jira:<issue id>`), never by its key, which a move rewrites. A value that is not that shape (a key such as `ENG-4`, or either half missing) is a **400** rather than an empty page, because an empty answer reads as "nothing happened on this item". The filter reads the `work_item` COLUMN (migration `0033`), filled from the same payload field as the row's `work_item` tag. Every row answers with its own `work_key` read off the `work_key` COLUMN (migration `0029`) rather than out of its `tags` — the column is what the `work_key` filter reads, so a row and the filter that matched it never disagree about its unit of work |
| `event_series` | `{bucket, since, until, type, source, category, trace_id, channel_id, seat, actor, agent, turn_id, work_key, work_item, suspended, failed, feed_only}` | `GET /events/series`. Every bar carries `failed` beside `count` — how many of its rows reported a failure — and the answer a `failed` total beside `total` (see [the event log's time axis](#the-event-logs-time-axis)). THE SAME ROWS WITH A TIME AXIS, which a page of rows has no dimension for: a burst at four in the morning and a steady trickle across a week are the same hundred rows in the same column. A second question rather than a flag on the first, because the two answers have different shapes and one route returning either would make every caller branch on what came back — the same split `tokens` and `token_series` carry. Both halves compile their filters through ONE predicate in the store, so a bar can never claim rows the listing beside it would not show |
| `trace` | `{trace_id}` | `GET /events/trace/{trace_id}`. Answers `{trace_id, events, truncated}`; `truncated` is true when the read stopped at the store's per-trace cap (500) rather than at the end of the trace, which the caller must say — a trace shown short with no note reads as a complete causal chain that simply ends. It is **counted, not inferred** from the row count: a trace of exactly the cap holds every row it has, and `len(rows) == cap` would put a truncation warning on a complete one. Across a fleet the rows are every node's oldest, merged, and they stop at the last row a node sent when that node holds more than it sent — its own read stopped at the cap, or its reply was cut to fit the transport — rather than show another node's newer rows past the ones it did not send: such a view can hold fewer than 500 rows, and `truncated` says it is cut |
| `turns` | `{days, since, until, seat, model, work_key, work_item, failed, sort, before, limit}` | `GET /turns`. The window is on the turn's START: `days` back from now (default 7, at most 30), OR `since` (inclusive) and `until` (exclusive) as RFC 3339 instants — either alone is a one-sided window — which is what a bar in the past needs, since `days` counts back from now; naming both forms, or a `since` not before `until`, is **400**. A turn is in the window when it STARTED there: one that began before `since` and ran on into the window is not listed, rather than listed from the window's edge with half its tokens. `seat` is a seat's **handle** and narrows to that seat's turns, resolved on the server exactly as on `events` (and refused the same way); there is no role-name filter, because a role name is changed by a rename while the history keeps the old one. ONE ROW PER RUN of a turn — a wake, a decision, its rounds and its reply — which is the view of a working company that did not exist anywhere. A turn that broke before reaching outside the engine is redelivered, so one TRIGGER is legitimately several rows; each carries the `work_key` they share and `work_key=` narrows to every attempt at one (see [a turn's two identities](../concepts/turn-engine.md#a-turns-two-identities)). A turn is what this engine DOES and every other surface is a projection of one: the spend rollup groups them, the seat page shows one seat's, an item's history links to the ones that touched it, and none of them is a list of them. The dashboard faked one by paging the raw event feed sixty-one times and folding in the browser — slow, capped at whatever the caller gave up on, and wrong at the page boundary, where a turn straddling two pages appeared twice. The aggregates are over PROMOTED COLUMNS (migration 0015) rather than payloads; only the duration, the summary and the work item come from the completion record's own payload, read from the one row per turn that carries it. `work_item` is `{backend, id, key, project}` — the one item the turn was charged to (see [which work a turn is on](../concepts/turn-engine.md#which-work-a-turn-is-on)) — and ABSENT for a turn on nothing, including one still running, since a sole write names its item only at the end. `work_item=` narrows to the turns on one item, by the same `<backend>:<id>` identity `/events` takes (and the same **400** for anything else); it selects TURNS rather than rows, so a turn is listed whole — every phase and every segment folded — when any of its records names the item. There is no `task_id` on a row: the key it read was declared on the completion and never set, and `task_id` elsewhere means a delegated worker's task or a schedule fire's run, never a tracker item. A turn whose executor launched in its box PARKS: the segment that launched it publishes a completion with `suspended: true`, and the same turn completes again when the launch is collected — so one turn can hold several completion records and "a completion exists" is not "the turn ended". The NEWEST completion decides: `complete` is true when it is not a suspension, and `parked` when it is, so the two are never both true; a turn with neither is running or died mid-flight. An `executor_failed` also counts as an end. It is an executor's launch that was LOST, and nothing resumes the turn that was parked on it, so a list reading completions alone called that turn parked for good. The newest end still decides: a launch lost while it was still launching is followed by its turn's own completion. A turn a PERSON stopped — a pause with `stop_running` reached it at a round, or ended the executor it was parked on — ends with a completion of its own, carrying `stopped`: such a row is `complete`, reads `stopped: true`, and is not `failed` for the stop (an `executor_stopped` is not a failure by type). `duration_ms` is the turn's OWN measurement — the SUM of every segment's, so the wait for the executor between them is not counted — which is not the span of its events either: the span covers the reflection pass that publishes afterwards. A turn's tokens are its COST: its phases', its executor's launches' (`executor_collected`, where its run's tokens are) and its in-turn `auxiliary_spend` records' (its turn-start context, its ledgers' rewrites, its card), never the reflection after it — whose records carry the turn's id, so the Turn screen can draw them, and which is the seat's learning rather than what the work cost; `sort=-tokens` ranks by the same sum, and `model=` selects over the same rows: a turn is listed for a model one of its phases or in-turn calls ran on — exactly what its `models` names — and never for one only its reflection used. `cache_read_tokens` and `cache_write_tokens` are that cost's prompt-cache counts (migrations `0032`, `0040`), a breakdown of `input_tokens` rather than an addition to `total_tokens`. `failed` is THREE-VALUED and absent means every turn, because folding it into `false` would hide every failing turn from an unparameterised list — and a turn counts as failed when ANY of its events carried a failure OR was a failure BY ITS TYPE (`llm_unavailable`, `budget_exhausted`, `turn.guard_breach`, `executor_failed`), which is the same rule `/events` applies to a row. The second half is what a turn the engine killed BETWEEN phases leaves behind — a refused charge, an exhausted chain, a breached guard — so reading the failure flag alone reported those as clean turns with no completion record, which is indistinguishable from a turn still running. A failed turn says WHY on its row: `error` and `error_kind` are the message and class of the record that ended it, the turn's own `agent_turn_completed` first, then any other failed record (a failed phase, a provider chain that ran out, whose `last_error` is read, or a failure by its type, whose kind is that type), and both are absent for a turn that carried no failure, including one a person stopped. They are read by whoever may read the row, so a lead reads why their agent's turn failed without the administrator's event log. The rows are newest first by where each turn is listed, and the higher `turn_id` first within one microsecond, so the order is total. The cursor is on the TURN — a keyset on any one event pages a turn twice — and it is OPAQUE: `next` is a token, and `before` takes exactly the `next` a page answered (anything else is **400** `bad_params`, an RFC 3339 instant included). It is opaque because the position is not a row's own fields: it is where the page's last turn is LISTED, which is not always its `started_at` (see [Reading the fleet's history](#reading-the-fleets-history-coverage)), and that turn's id, without which two turns starting at one microsecond either side of a page's cut were one position and the one past the cut was on no page; and it can stand on an empty page, which has no row to build one from. Where `/events` and `phases` echo `before_time`/`before_id`, the position IS the last row's key. `next` is present only while more turns lie past the page — `null` on the last one, which is how a reader paging a seat's record knows the walk has ended rather than offering "older" onto an empty page. `sort` is `-started` (the default, newest first) or `-tokens` (the most total tokens first, the spend screen's drill-down per turn) — anything else is **400** naming both. Read from EVERY data node and merged, with a `coverage` — see [Reading the fleet's history](#reading-the-fleets-history-coverage) |
| `turn` | `{turn_id}` | Every event of ONE RUN of a turn, oldest first, payloads included — each phase, the turn's own completion, and the fallbacks and guard breaches that happened inside it. Not a slice of the trace: one trace can span several turns and one turn several traces. Answers `{turn_id, events, truncated}`; `truncated` is true when the read stopped at the store's per-turn cap (500) rather than at the end of the turn. A cut answer is the turn's **opening and its ending**, not its opening alone: a turn is read oldest first, so a head-only read would drop `agent_turn_completed` and `turn_completed` — the two records a reader takes the outcome, the duration and the plan summary from — and a turn cut at the cap would be indistinguishable from one that never finished. The last rows are recovered beside the first (up to 32 more, merged on the store's own identity, `(event_time, event_id)`, so the two reads cannot overlap into duplicates — the id alone is not unique, and a narrower key would drop a row the two reads legitimately both carry and then report a gap over a page holding the whole turn), so what `truncated` names is a gap in the **middle** — and it is **counted, not inferred** from the row count, because a turn between the cap and the cap plus thirty-two ends up whole on the page and must not carry a truncation warning. Across a fleet the opening stops, as `trace`'s rows do, at the last opening row of a node holding rows it sent in neither half, so it never shows a row past a gap it does not report. It also answers `work_key` and `attempts`: the unit of work this run was an attempt at, and every run of it the store holds, OLDEST FIRST — over the SAME thirty-day horizon the events above come from, not the turns list's own default week, and read at the same instant they were, so a turn between eight and thirty days old, or one whose rows sit at the very edge of the horizon, names its attempts rather than reporting none while displaying one — so the screen a deep link lands on can say "attempt 2 of 2" and link the other, rather than leaving a reader to conclude the company did the work twice. One element is the ordinary case; an empty `work_key` means the trigger had none to collapse on, and `attempts` is then empty too. And `nodes`: where it RAN — the nodes that PUBLISHED any of this turn's events, read off the rows (every event carries the node that published it as `node`), sorted, and an empty list rather than an absent key where no node this read could reach holds any. Not the nodes in `coverage`: a turn run on a node without the `data` role is held by the data node keeping its events in [custody](../guides/deployment.md#custody-the-rows-of-a-node-without-data), which never ran it |
| `phases` | `{seat, limit, before_time, before_id}` | The company's `agent_phase_completed` records and its `executor_collected` ones — each launch of an executor's run, which is no phase but the work of the `execute` round that launched it — newest first, **payloads included**, keyset-paged; each node reads a page of each type and merges them, so a page holds both. `seat` is a seat's **handle** and narrows to that one seat's phases, resolved on the server exactly as on `events` (and refused the same way) — never a role name, which two unit seats stamped from one template share, so a role filter answered one seat's phases with every such seat's. `events?type=agent_phase_completed` is not a substitute: the event listing deliberately never selects the payload, and a phase record without one has no prompts, no response, no tool calls and no decision. `next` is present only while more records lie past the page — each node's read asks one row past it rather than guessing from a page that filled, so a history exactly a page long ends without offering "older" onto nothing |
| `tokens` | `{days, since, until, previous, seat}` | `GET /tokens/breakdown`. With no parameters, the live 24-hour window from the projection; with any, whole company days from the replicated usage domain — every data node's, up to 90 days, within the 181-day horizon (see [Token Spend Breakdown](#token-spend-breakdown)) |
| `token_series` | `{days, since, until, previous, seat, group, bucket, groups}` | `GET /tokens/series`. THE SAME SPEND WITH A TIME AXIS, which the breakdown has no dimension for: every one of its rows is a sum over the whole window, so a runaway loop, a spike and a quiet weekend are the same number. A second question rather than a flag on the first, because the two answers have different shapes and one route returning either would make every caller branch on what came back. Bucketed by the ENGINE, by company day or ISO week from the usage domain. An unknown `group` or `bucket` is refused naming what is accepted, never defaulted: a chart legended by one dimension over another's bands is worse than an error. By `unit`, each band of `by_group` carries `parts`: the seats that spent in that unit over the window, biggest first, each `{group, handle, ...bucket}` keyed as `group=seat` keys a band, and summing to the band, so a team's bar can be split by seat from the same answer. Every other grouping, and the residual row that folds several units, carries none |
| `seat_activity` | `{seat?, days?, previous?}` | `GET /agents/activity`. Every seat's TURNS over the `days` (1 to 90, default 7) company days ending today, summed across every data node from the replicated usage domain — so the answer is the same on whichever node is asked and still counts a node that has left. `{since, until, days, previous_since?, previous_until?, seats, quantile_resolution}`; each seat is `{handle, role, agent_id, in_chart, turns, failed, reviewed, first_pass, first_pass_pct?, sent_back, p50_ms?, p90_ms?, tokens, per_day, last_turn_at?, previous?}`. `first_pass_pct` is `first_pass` over REVIEWED turns (0–100) and is ABSENT when none was reviewed — a 0% for a seat nobody reviewed would be a verdict nobody gave. `sent_back` counts reviews that sent work back. `p50_ms` and `p90_ms` are read from the merged turn-duration histogram and are within `quantile_resolution` (0.06) of the true value; absent when no turn ended. `per_day` is every day of the window, oldest first, a quiet day included as zeros. `previous` (with `previous=true`) is the seat's `{turns, failed, reviewed, first_pass, sent_back, tokens, per_day}` over the same number of days before, `per_day` being every one of those days, oldest first — so a profile draws the fortnight its week-on-week figure is made over from this one answer. Every AGENT seat of the current chart has a row, a quiet one with zeros ("took no turns" is a measurement); a seat that has left the chart appears with `in_chart: false` while its days are in the window; a human seat has none. `seat=` narrows to one handle, and a handle with no rows answers one row of zeros rather than none; a human seat's handle is refused (`bad_params`, naming the person), because the engine runs no turns for a person and a zero row would say one took none. Ordered by handle |
| `schedule_runs` | `{scope_type, scope_id, name, limit}` | `GET /schedules/{scope_type}/{scope_id}/{name}/runs`. ONE schedule's dispatch history, newest first, fifty to a page. `scope_id` is the scope's IDENTITY (a seat's agent id, a unit's key), which is what a `schedules` row carries and what the ledger keys on — hand back what that row gave you rather than the handle beside it. `schedules.recent_runs` is the COMPANY's fifty most recent fires across every schedule, so twenty hourly ones fill it in two and a half hours — "did the standup fire this week" was unanswerable while every row of the answer sat in the table. The identity is all THREE parts and each is required: two units may each declare a `standup`, and a role and a unit may both, so a name alone merges two teams' histories. `truncated` says the page filled, because a full page is otherwise indistinguishable from a schedule that has fired exactly that many times |
| `schedules` | `{}` | `GET /schedules` |
| `fleet_broker` | `{}` | `GET /fleet/broker`: what every live node advertises about its broker, the metadata group as a member reports it, and where the two disagree. **Takes `admin`**. See [below](#the-brokers-membership) |
| `backups` | `{}` | `GET /backups`: each node's newest backup, each log's newest acknowledgement and the backup history, the Settings › Backups & retention screen. **Takes `admin`**. See [below](#get-backups) |
| `mcp_servers_status` | `{}` | `GET /mcp-servers`: each MCP server's condition and its per-node counts, the Settings › MCP Tools screen's Servers section, and the built-in tools by group. **Takes `admin`**. See [below](#get-mcp-servers) |
| `limits` | `{}` | The caps the engine holds the company's work to, as it ENFORCES them: `{limits: [{key, group, unit, value, default, off, default_off}]}`, in drawing order. `key` is the setting's path in the company document (`turn_engine.max_iterations`, `turn_engine.onboarding_max_tool_rounds`, `turn_engine.delegation_depth_limit`, the six `turn_engine.delegation.*` caps, `notification_rate_limit`, `notification_coalesce_max_batch`, and `schedules.timeout_seconds`, the timeout every schedule takes unless it sets its own); `group` is `turn`, `delegation`, `notifications` or `schedules`; `unit` is `count`, `seconds`, `fraction` or `per_second`. `value` is what the applied company holds, which is the default wherever the document says nothing, and `default` is the engine's own, so a screen draws both and never keeps a copy of a default. There is no "was it written" flag, because the applied company does not remember. `off` marks a value that switches its limit off: a `notification_rate_limit` of 0 (no limit) and an `onboarding_max_tool_rounds` of 0 (no onboarding pass). `default_off` asks the same of `default`, so a screen saying which default a set value replaced says Off where the default is no limit. Empty on a node running no company. **Takes `admin`**, as the `config` question it is read beside does |
| `sign_in_policy` | `{}` | How a person signs in on THIS node, read from its own `api.auth`: `{node, policy: {second_factor, min_password_length, session_absolute_seconds, session_idle_seconds, step_up_seconds}}`. Every value is the one the engine enforces, so a setting the file leaves out arrives as its default (`required`, 12, 168 hours, 1 hour), and `session_idle_seconds` is the engine's constant 12 hours. Tier A, so nodes of one fleet may disagree and `node` names the one that answered; changing a value is an edit to that node's file and a restart. **Takes `admin`**. The two values a sign-in form needs before anybody has signed in are also on the unguarded [`/auth/config`](#routes) |
| `fleet` | `{}` | `GET /fleet`: leases move with no event to push, so Settings › Nodes polls this rather than waiting for one. **Takes `admin`**. A lease table that could not be read answers `unavailable`, which is a blip to ask again about rather than a fault (the REST twin answers `503` with a `Retry-After`) |
| `executors` | `{audience?}` | `GET /executors`: `unknown_query` on a company with no sandbox configured, and `unavailable` when the fleet's run record could not be read. Each run carries `work_item` (`{backend, id, key, project}`, or null), the item the launching turn was charged to, and `launch_id`, the job the row holds now — what `executor_tail` is asked by |
| `executor_tail` | `{turn_id, launch_id, epoch?, after?, digest?}` | `GET /executors/{turn_id}/tail?launch_id=…[&epoch=…&after=…&digest=…]`. What ONE running launch of an executor has said so far, read from its box by the node that can reach it: the executor's owner for an `e2b` box, and for a `direct` or `container` box the node that launched it (`launch_node`), whoever holds the seat now (see [Watching a run live](../concepts/code-sandbox.md#watching-a-run-live)). Both ids are required (`bad_params` otherwise): a turn can launch its executor more than once, and the launch id is the one `executor_launched` and the launch's own record carry. Answers `{outcome, turn_id, launch_id, node?, status?, output?}`: `outcome` is `tail` with `output`; `launching` for a launch whose box is still being made (not stopped — ask again); `not_running` with the record's `status` (`awaiting_clarification`, `resumed`, `reseed`, `replaced` for a launch a later one replaced, or absent where no record is left); `owner_silent` naming the `node` that did not answer inside the 2 s fleet read budget — the executor's owner, or a `direct` or `container` box's launching node, held or not, a draining node or one whose heartbeat lapsed included (no `node` only for an `e2b` box nobody holds right now); `box_paused` for a running record whose box is paused where its backend cannot read it without waking it (a peek never wakes a box); or `box_gone` for a running record whose box its backend says is gone for good (its provider reaped it, or a local box whose root did not outlive its node), which nothing more is read from: its waiter records the launch lost and its round is carried on in a fresh launch (see [Durability](../concepts/code-sandbox.md#durability-completion-and-observability)), so the record moves on (not stopped yet: ask again). The asker says what it holds — `epoch`, `after` (the byte offset it holds through: a whole number of zero or more, `bad_params` otherwise — a fraction is refused rather than rounded) and `digest`, all three absent for one holding nothing yet — and `output` is `{text, source: transcript\|stderr\|none, cut, as_of, finished, epoch, start, end, digest, reset, front, held}`, redacted — `epoch`, `start`, `end` and `digest` on every answer, `0` included (a launch that has settled nothing yet is answered at `end: 0` and followed from `start: 0`): `text` is what follows `after` (`start` = `after`), or on `reset: true` the last 256 KiB in whole lines, which replaces what the asker held (`start` says where it begins; `cut` that earlier output exists). Send `end` and `digest` back as the next `after` and `digest`. `front` says that node's reading began after the launch's own start; `held` counts bytes written but not shown until their redaction is settled. What a harness prints is its seat's trail, so the tail is read by an admin and by the lead of the seat the executor's record names (`audit.seat.read`) and refused to anybody else naming `admin` — and a caller who leads nobody is refused before the record is read, so a record that could not be read, or a box that node could not read, is an error only to somebody it could have admitted. A box the owner couldn't read is answered by what clears it, never in the provider's words: `unavailable` with a `Retry-After` where a wait does (a provider that didn't answer or is at its limit, an owner that couldn't answer just now), `unavailable` with none for a sandbox setup, a refused key or an unpaid account, and `query_failed` for a fault of the node that owns the executor, whose log has the detail (`sandbox_tail_failed`; this surface's own line about the answer is a debug trace). There is no event and no row: the dashboard asks it every 3 s for each running launch it is showing — an open launch span on a turn's timeline, a parked turn's **Executor** card, and the executor's own page — and nothing outside the dashboard asks |
| `budgets` | `{}` | `GET /budgets` |
| `a2a_channels` | `{}` | The fleet's agent-to-agent authorization record: who asked whom, how many messages crossed, and when. `available: false` when this node could not reach the coordination store — which is not the same as no channels having been opened |
| `knowledge` | `{q, mode?}` | The company's own knowledge search, run live through the same `knowledge.Searcher` seam a seat's own `search_knowledge` tool uses. Searched as the ORG with no seat, so it applies the engine's own account and nothing more: searching as a named seat would let a dashboard reader read, through that seat's credential, material their own account may not have. Registered whenever a company is active, NOT only when a searcher exists: "this company has no knowledge backend" is a fact the company establishes on its own, and it is a far more useful answer than an unknown query. Everybody may ask it, and what it searches is the caller's apps': it searches pages only for a caller holding `doclet`, so a caller holding `worklet` alone is answered with no hits, `available: false` and `reason: no_access` (and the searcher is never asked), while a caller holding neither app is refused `unauthorized` naming both. `available: false` covers all five of no access, no company, no backend, a backend wired with no org-wide read scope, and a node whose index is still `building`. `reason` (`no_access` / `no_company` / `no_backend` / `no_scope` / `building`, empty when the search ran) is the value to branch on and `note` is the prose for a person: a screen picking which remedy to offer must not string-match the note, nor infer the state from an empty `backend`, which means "no backend" and "no company" alike. The `no_scope` note names `knowledge.scope`, because an operator whose integration is correct must not be sent to re-check it. A search that RAN and fell short says so in its outcome rather than its `note` (which is empty whenever `available` is true): `served_mode` empty with `coverage.complete` false is a backend that did not answer, and a partial `coverage` is part of the corpus unsearched. An EMPTY `q` is the seam's PROBE: nothing runs and nothing is read, no hits are returned, but `modes` and (for the `mode` asked) the `degraded` its CONFIGURATION decides are answered, so a screen offers the modes honestly before anybody types (the dashboard asks the probe for `semantic`, the mode whose reason names what is missing). A transient reason (`embedding_failed`) is a property of a search and is never predicted. `mode` is `hybrid` (the default), `keyword` or `semantic`; the screen's label for the last is "Meaning", and any other value is refused rather than run as the default. A `q` past 400 bytes (trimmed) is refused `bad_params`, naming its size and the limit, rather than cut: the bound every search surface shares ([Search](../guides/search.md#three-modes-and-what-an-answer-says-it-served)). Every answer carries what the search actually did: `mode` (resolved), `served_mode` (the ranking the hits came from; `""` when nothing ran), `modes` (what this backend can serve as asked: `[keyword]` with no embeddings provider and on Confluence), `degraded` (`no_embeddings` / `embedding_failed` / `semantic_partial` / `unsupported`, empty when it served what was asked) and `coverage{nodes:[{id,answered,error}], complete, buckets_missing}`, the fleet the scan was divided across, which is how a partial answer is told from a short corpus. A hybrid search with nothing to rank by meaning serves its keyword half; a semantic one serves nothing and says why. See [Search](../guides/search.md#three-modes-and-what-an-answer-says-it-served) |
| `colleague` | `{q}` | A NAME TO A SEAT, through the same four tiers an agent's `lookup_colleague` and `a2a_ask` resolve through (`internal/agent/colleague`): exact handle, chat id and role name, then case and separators folded, then part of a name, then a close spelling — each tier answering only when every tier above it found nothing, so a name that is exactly somebody's handle is never diluted by near misses. Answers `{match, candidates[{handle, why}]}`: `match` is the one seat the text names when EXACTLY ONE does and `null` otherwise — including when several do, which is a list for a person to choose from and never a pick — and `candidates` is every seat it could name, best tier first (the match included; empty when nothing matched, which reads differently from ambiguity). `why` is the tier in a person's words ("handle matches exactly", "part of the name matches"). The chat-id tier needs a credential: a seat's contact identities are operator-gated configuration, so an anonymous caller resolves over handles and names alone. `q` is at most 200 bytes — a name, not the sentence around it. The command palette's assign and ask pickers read it. Registered whenever a company is active |
| `integrations` | `{}` | `GET /integrations` |
| `link_hosts` | `{}` | `GET /link-hosts`. `{hosts: [{kind, url, owners?}]}`, `kind` one of `github`, `gitlab`, `mattermost`, `jira`, `confluence`; `owners` only on `github` and `gitlab`; an empty list when no company is active |
| `work_items` | `{container, status, status_group, assignee, reporter, watcher, collaborator, tag, type, priority, parent, root, q, key, references, linked_page, removed, blocked, blocking, has_dependencies, has_open_asks, flag, asked_of, asked_by, subtasks, show_closed, closed_since, f.<slug>, view, preset, group_by, group_by2, group, subgroup, group_limit, totals, sort, fields, around, cursor, limit, …}` | `GET /worklet`. `container` is the scope: `workspace`, or `project:ENG` (a bare `ENG` works too, and the key is upper-cased because the column is); any other `<kind>:` (`unit:eng`, `person:ana`) is REFUSED, because a task lives in the workspace or a project and a team's or a person's work is `unit=` or `assignee=` (read as a project key, `unit:eng` answered an empty board), and an ABSENT container is neither: the engine refuses to default it, because an omitted key would otherwise be the most expensive query in the system. Every list key is comma-separated, because a socket frame's JSON object cannot carry a repeated key and a filter only one transport can express is exactly the divergence this channel exists to prevent; `status` also takes `!` negation. There is no `open` flag: open and closed are STATUS GROUPS (`not_started`, `active`, `done`, `closed`), which is the level every rule in Worklet is written at. `f.<slug>=<value>` filters on a custom field, resolved against the company's catalogue by slug, id or label, and compared on the column its DECLARED TYPE says, so `f.effort=gt:9` is a numeric comparison and not a lexical one; the seventeen operators are `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `contains`, `startswith`, `in`, `range`, `any`, `all`, `not_any`, `not_all`, `me`, `null` and `not_null`; and which of them a field admits is a property of its TYPE, so `eq` on a `labels` field is REFUSED naming `any`, `all`, `not_any` and `not_all` rather than compiling to a clause that matches nothing and reads as "no task has this label". `null` and `not_null` are on every type, because "is this set" is a question about the ROW. A bare value is the type's NATURAL comparison: `any` on a set, because naming a value is not claiming the set IS it, and `eq` everywhere else. A set operator takes a comma-separated list (`any:api,ui`, at most 16) and `range` takes both ends (`range:3..8`), because a range with one end is `gte` or `lte`. A value whose text begins `<scheme>://` is a VALUE rather than an operator call, so a `url` field can be filtered by what it holds; anything else before a colon is carried through as an operator, so a typo is refused naming the set rather than silently answered. `f.<slug>=me` is resolved to the reader by the SURFACE before the query is parsed, which is what makes one saved view mean whoever opens it. A ref nothing resolves is REFUSED naming it. `unit=` and `routing_unit=` name a team by its key or by its NAME where exactly one unit carries it, in any case, and each resolves through the chart and matches the work filed under the team's key or its name, whichever the reference was spelled as. A team the chart does not have matches nothing rather than refusing, because a task's filed unit is a record of what was true and may name a team since dissolved. `q=` is a FIND rather than a search: a substring of a key (from the front) or a title (anywhere), which is what finds the item somebody half remembers; there is no `mode`, because this grammar has no ranker and ranked search over what the work says is `work_search`'s. A `q` longer than the longest title (256 bytes) is refused naming the bound, because it can match nothing and an empty board reads as "there is no such item". `key=ENG-1,ENG-7` narrows to keys a caller already holds (upper-cased, like `container=` and `references=`, because a key is what somebody pasted and the column it is compared against is minted upper-case), and `removed=true` is the TRASH: the only way to list what a removal hid, which is what a restore is a gesture about. `references=ENG-12` lists the tasks whose DESCRIPTION mentions that key (a task's former key resolves like its current one; a comment's mentions are not references), and `linked_page=<page id>` the tasks that name that knowledge-base page through `linked_pages`. A parameter this grammar does not read is REFUSED naming it, never ignored: a filter nobody parsed is a board showing more than the person asked for, silently. An unknown status or group is refused naming the closed set rather than matching nothing. A custom field VALUE is checked against its own declaration at the write and refused naming the rule, never rounded or coerced to fit; see the coercion table in [the Worklet guide](../guides/worklet.md). `flag=` is the ATTENTION queue and its values OR: `cycle`, `too_deep`, `inconsistent_project` and `key_collision` are facts about a task's own row, and `one_sided` and `one_sided_final` are about a DEPENDENCY of it: an authored `waiting_on` whose blocker does not list it, and one whose mirror was refused permanently (the blocker is gone, was removed, or is full). The first is what the `tracker` duty repairs 30 seconds on; the second is what a person resolves. `asked_of=` is whose answer an open ask is waiting on and `asked_by=` whose question it is (the work a person is WAITING on), each a record owner's name: a person's seat, and the login of a credential bound to none, which is the name every ask they put is authored under whichever surface they put it from. `inconsistent_project` marks a subtask filed in another project from its root, which the writer refuses and the applier flags rather than stall on if a record carries it anyway. `key_collision` marks a task holding a key another task claimed first (which is what a key counter restored beside newer work mints), and the key goes on opening the claimant on every node, so the flagged task is reached by its id; purging the claimant hands the key to the remaining holder with the lowest id. Every row this API lists an item in SAYS SO, because a row is what a link, a peek or a composed call is built from and one carrying the key alone opens the claimant: a row here and a `work_my_work` row carry `key_collision`, as do a `work_search` hit, a `work_item` answer (beside `blocked`) and each of its `links`; a `work_activity` record and a `work_inbox` notice carry `subject_key_collision` beside `subject_key` (a notice's id to open by is its `task`, since a `prioritised` notice's subject is a person); a `work_my_work` checklist item carries `task_key_collision` beside `task_key`. The rule a reader applies is one sentence: open a row by its key unless the flag is set, and then by its id; and an ask's `answer_with` already follows it. A notice's flag is asked of the key the notice STORED rather than read off the task, because a duplicate moved since answers to a new key while the one its notice carries still opens the claimant. They OR because an attention queue asks "is anything wrong with this", and a conjunction over six flags answers nothing on every company. `totals=<column>:<op>` adds aggregates over the WHOLE matched set rather than the page: a number that changed as somebody scrolled would be the one thing a header must not do. The seven ops are `sum`, `avg`, `min`, `max`, `count`, `median` and `p90`. `median` and `p90` are ORDER STATISTICS by nearest rank (the value at position ⌈p·n⌉ of the set's values in ascending order), so each answers a value some task actually holds: the median of four values is the second, never an average of two. The columns are the summable ones (`points`, `estimate_min`, the `spend_*` family (`spend_workers` and `spend_sent_back` included), `reassignments`, `reopens`, `depth`), the date columns for the four order statistics `min`/`max`/`median`/`p90` only (a sum of dates is a number of microseconds nobody meant), `tasks:count`, and `f.<slug>` for a declared number or date field. A total with nothing to add up is ABSENT rather than zero: "nothing is estimated" and "everything is estimated at nothing" are different facts. `subtasks=` is how a tree is filtered: `collapsed` (the default) and `expanded` filter ROOT tasks and let their subtrees ride along unfiltered (so a todo root brings its done subtask), while `separate` filters every task on its own. With `removed=true` the unit is the REMOVAL rather than the tree: each removal is filtered on the task it was made on and what it took rides along, which is what a restore of that task brings back, so a subtask removed on its own is listed as a removal of its own. And a task whose root (in the trash, whose removal) is on the other side of the trash — a subtask left live under a parent removed alone, or one still in the trash after its removal's root came back — is filtered on its own, as the root of what is left. The first two answer the same SET and differ only in how a caller renders it. Asking for a subtree with `parent=` or `root=` turns the mode off, because those are questions *about* subtasks and filtering their roots would answer the parent's siblings. `any=[{…},{…}]` is one level of disjunction, ANDed with the top-level keys: a branch is a PREDICATE, so it may not carry the keys that decide the answer's own shape (`removed`, `archived`, `show_closed`, `closed_since`, `subtasks`, `fields`, `around`) or how fresh it must be (`read_level`, `max_lag_seconds`, `max_lag_seq`, `min_position`): those are the same decision at every branch or they are incoherent, and a branch that carried one would narrow what was asked for at the top level rather than widening it. An empty branch is refused, because it matches every task and makes the others decoration. `view=<id>` and `preset=<name>` are loaded FIRST and every explicit key overrides them: a saved view is a set of defaults rather than a lock, so somebody who opens a board and picks another assignee gets the view with that one key changed. A view beats a preset (somebody saved it) and what was typed beats both. The five presets are `my_queue`, `priorities`, `triage`, `blocked` and `overdue`. `my_queue` is *what can I pick up*: a DISJUNCTION of the work the viewer holds and the work in their OWN project nobody holds, open and unblocked, most important first: both arms matter, because written as "assigned to me" alone a seat with an empty queue reads the company as having nothing for it while its project's unclaimed backlog sits there, and the second arm is scoped to their project because unscoped it offers every unassigned task in the company, so a viewer with no project of their own, a seat whose unit files none or a caller the directory binds to no seat, has no second arm and picks up what they hold. `priorities` is the viewer's own ordered list, open tasks only, IN THE ORDER somebody arranged it: that order is the answer, so nothing sorts over it, and a finished task drops out of the answer without the list being rewritten. `triage` is the unassigned open work, which with one fixed status set is the honest definition of "needs somebody to decide". `my_queue` and `priorities` are the CALLER's own (there is no parameter naming whose, because a question about somebody's queue asked on their behalf is the owner-or-lead rule's, not a filter's), read under their [own record](#whose-record-a-personal-question-answers-for), which for a caller bound to no seat is their login; a caller the engine can name nothing for is refused both, because a list with nobody's name on it is everybody's. A `view=` nothing resolves is REFUSED, never answered as the whole board. `group_by=` turns the answer into a BOARD: `groups` replaces `rows` (returning both would be the same rows twice), and each column carries its own `count` over the whole set beside a bounded slice of its rows (`group_limit`, default 20, max 100). The axes are `status`, `status_group`, `assignee`, `priority`, `type`, `tag`, `project`, `unit`, `routing_unit`, `parent`, `due:day`, `due:week`, `start:week`, `due:bucket` and `f.<slug>` for a custom field; anything else is REFUSED naming the key rather than answered ungrouped. `unit` and `routing_unit` group on the TEAM rather than on the stored string, for the reason `unit=` matches both the team's key and its name: grouping on the column drew a team whose rows hold both as two columns (both headed with its name), with its counts split between them. The expression folds every spelling onto the unit's key, and `group=` is folded the same way, so `group=eng` and `group=Engineering` load the one column. A stored unit the chart no longer has keeps its own column under the literal its rows hold, since folding it into anything would invent a home for work whose team is gone. A grouped answer mints no cursor, because across a set of columns there is no single order to be after; `group=<value>` is how a board loads one column further, and it narrows the WHOLE query, so the hint and the totals describe that column too. `group_by2=` adds swimlanes inside each column and `subgroup=` names one: a swimlane board is bounded by its CELLS rather than by either axis alone, because the work it costs is the PRODUCT of the two, so asking for lanes lowers the column cap and `subgroups_dropped` says how many lanes a column has beyond it. A `group_by=` over the WHOLE COMPANY is refused when the query's own narrowed predicate still matches more than 20 000 tasks: a board is drawn by sorting every one of them, and the refusal names the ceiling and what narrows it. It is a bounded COUNT rather than a check for the presence of a filter key, deliberately: `status_group=not_started,active` is a filter and narrows nothing, so a gate spelled "needs a narrowing filter" is one a caller clears in a single attempt without making the query any cheaper. Scoping to one project with `container=project:<key>` lifts it, because there the input is an index range whose width is one project's own size. An absent value is its own labelled column ("nobody is assigned" is a question a board answers rather than a row it hides), and `group=` with no value is how that column is loaded one further, because a key named and left empty asks for the rows with no value where an absent key asks for all of them. `group_by=due:bucket` is the one axis that is not a stored value: it is WHEN the work is due, read against the query's own day: `overdue` (still open and past it), `earlier` (finished, and past it; work delivered late is not overdue and calling it so would be a false claim, so it is its own band and is empty unless `show_closed` brings finished work into the answer), `today`, `this_week` (through the end of the current Monday-anchored week, which is the week `due=range:sow..eow` means), `later`, and the empty key for a task with no due date. Its six headings read Overdue, Earlier, Today, This week, Later and No due date. The day it cuts on is the COMPANY's midnight in the company's own zone (the same instant the row's `overdue` flag is derived from and the same one every `due=` filter compiles against), so the bands, the flag and the filters cannot disagree about a task. A band cut in the reader's own browser could and did: for anybody whose local day differs from the company's, a task landed under Earlier on a row the same answer flagged as due today and not overdue. A CLOSED axis (`status`, `status_group`, `priority` and the `due:bucket` bands) carries every column the query itself admits, the empty ones at `count: 0` with `rows: []`, in the declared order: a board is the shape of the process rather than of this week's rows, so an open-work board draws To do, In progress and In review whether or not anything is in them, and never Done, which the query excluded, because "nothing is done" said about a set that was never asked is a claim rather than an absence. The admission is the predicate's own (`status`, `status!`, `status_group`, `show_closed`, the overdue alias, `due=` for the bands, and `group=` down to one column, where a key NAMED AND LEFT EMPTY admits the undated band on `due:bucket` and nothing at all on the three whose values are never empty). An OPEN axis (assignee, tag, type, a field) carries only the values present, since every seat as an empty column is a roster rather than a board, and the second axis is never filled. `group_by=tag` is the one axis where a task is on several columns at once; the answer sets `groups_overlap` so a reader knows the counts do not sum to `total_hint`, and `groups_dropped` says how many columns did not fit. `sort=` takes `rank`, `updated`, `due`, `start`, `priority`, `created`, `title`, `estimate`, `points`, `spend_tokens`, `reopens`, `status_entered` and `removed`, each reversible with a leading `-`; `spend_tokens` is the column's own name, as a total's is, since `spend` on a row is an object of five. The numeric filters `estimate=`, `points=` and `spend_tokens=` take `lt:`, `lte:`, `gt:`, `gte:`, `range:a..b`, `null` or `not_null`. `closed_since=<date token>` is the open work PLUS what finished (done or cancelled) at or after that date, resolved on the COMPANY's clock through the same calendar as `due=`: `closed_since=sow` begins at the company's Monday midnight, so a Done lane on it is this week's work and empties itself when the week ends; it is the Board's Recent scope, and naming it beside `show_closed` is refused because both say which finished work is in the answer. `fields=tags,dependents_count,open_asks,spend` puts a board card's facts on each row, and only when asked: `tags` (sorted, `[]` when none), `dependents_count` (live tasks waiting on it through the authored `waiting_on` edges `blocking=` reads), `open_asks` (unanswered questions, by the same test `has_open_asks=` makes) and `spend: {tokens, turns, workers, sent_back, reopens}`, each present, `0` included, when asked, and absent otherwise, because the same row is what a seat reads through `list_work_items`, which never asks for them even through a saved view that does; an unknown name is refused. `around=<task>` (a key, a former key or an id) adds `around: {position, prev, next, total_hint, total_capped?}`: where that task sits in this answer's DRAWING order over the whole answer rather than the page: a flat answer's own sort, or a board read column by column (and lane by lane within a column) in the answer's column order with each column's rows in the row order, so the card after the last one in a column is the first in the next. `prev`/`next` are keys, null at the ends; on a label board the order counts cards and a task is placed at its first column; `position` is null past the 10 000 a count stops at. A task the answer does not hold answers `around: null` (never a refusal), and a saved view cannot carry `around`. A `priorities=` answer is paged in the list's own order: the whole list (at most 32) is read each page and its cursor is a place in the list. **An absent value sorts LAST in both directions**: "soonest first" and "latest first" are both questions about values, and a task with no due date is the answer to neither, so `sort=due` puts the undated at the end rather than ahead of the one due tomorrow, and a cursor resumes in the same place the order put it. `sort=f.<slug>` orders by a custom field, LEFT-joined so a task that set no value still appears: a sort that also filtered would be two things the caller asked for once, and such a task sorts last by the same rule. The answer carries `total_hint` (capped: an exact total over an unbounded set turns a poll into a scan), `next_cursor`, `totals`, `groups`, and an echo of the `view`/`preset` it was expanded from (these answers travel detached from their requests, so a board restored from a URL can still say which saved view it is showing), plus the coverage half below |
| `work_item` | `{id}` | `GET /worklet/{id}`: key or id, and the id is how a task flagged `key_collision` is reached, since its key opens the task that claimed it first. Answers `{task, comments, comments_cursor?, history, links, parent?, fields, units, blocked, key_collision?, due_standing?, keys, reassignment_budget}` plus the same coverage half. `key_collision` is on the ANSWER for `blocked`'s reason (it is the applier's derived column rather than a field of the record), and each link carries its other end's. `due_standing` is where the due date stands on the COMPANY's calendar, `{days, overdue?}`: whole calendar days from the company's today to the due day (negative once it has passed, counted on the day labels so a DST day is still one day), and `overdue` for open work whose due instant is before the company's midnight, the same predicate as a board row's `overdue`, and is absent for a task with no due date, so a task page never re-derives either from the reader's own clock. `task.spend` is the item's running totals as its turns added them, read from the counters the turns wrote, never from the create's copy in the document. `parent` names the item this one is filed under (`{id, key, title, status, key_collision?}`, read in the same transaction), and is absent for a top-level item and for a parent this node holds no row for. `comments` is the newest page of the thread and `comments_cursor` the page before it, which [`work_comments`](#queries) reads. `reassignment_budget` is how many times agents may hand the item on before the engine refuses the next hand-off (the limit `task.reassignments` counts against, served so no screen carries a figure of its own), and each `history` row carries `reassignments`, the item's hand-off count AS THAT CHANGE LEFT IT (absent on a row the answering node holds no count for); an assignment made with a `reason` shows the reason as that row's `excerpt`. `units` is the task's two unit references RESOLVED against the org chart (`{filed: {key, name, resolved}, routing: {…}}`, the same shape a project row's `unit` carries), because what the document holds is the unit's KEY, the `id` the org chart gives it: a word chosen so that a rename moves nothing, and therefore a word nobody reads. The document's own `filed_unit` / `routing_unit` are untouched beside it: they are the record, and `key` repeats exactly what they hold, so a filter link built from it reaches the same rows. `resolved: false` is the finding "this names a team the chart no longer has", and it is ABSENT for a task filed into no team at all, because that is what its two empty strings already say. `blocked` is on the ANSWER rather than on `task` because it is DERIVED: an open dependency edge, computed in the same transaction as the task, so the badge here and the badge on the board row cannot disagree; `links` say what the relations are, not whether any blocker is still open. `fields` are the task's CUSTOM fields resolved against the company's catalogue (each carrying its declared name, type and whether its declaration was archived), because a stored choice is an option's UUID and a panel rendering the raw value would print it under a heading. `keys` names the tasks this answer's `history` POINTS AT, id to item key: the same map `work_activity` carries, resolved by the same walk, and present only when `history` was asked for. A delta names the other end of a relation by its ID because a key is a fact about another task's row, so without this map the item's own History tab rendered a re-parent as `Parent: 1d573f85-… → 50a01576-…` while the company-wide log rendered the same commit as `Parent: — → ENG-1`. An id this node holds no row for is simply ABSENT, so a renderer falls back to the id |
| `work_catalogue` | `{archived}` | `GET /worklet/catalogue`. Answers `{types, fields, policy_version, types_version, fields_version}` plus the coverage half. `policy_version` moves on every *fields* edit and is what a task's policy stamp records having validated against; a *types* edit does not move it, because the two are separate objects on separate subjects so an unrelated edit never invalidates every task's stamp |
| `work_projects` | `{q, unit, archived, sort, limit}` | `GET /worklet/projects`. `task_counts` (`{todo, active, done, closed}`) is read from `tracker_projects.open_count/active_count/done_count/closed_count` (`todo` being the unfinished work less the started part), MAINTAINED by the task apply whenever a status group changes or a task enters, leaves or is removed, never aggregated per poll, which over every task in every project is what a sixty-second refresh used to cost. `last_change` (`{at, actor, actor_kind}`) is maintained on the same row and by the same apply, from the commit that writes the project's own history row: it is the HEAD OF THAT PROJECT'S ACTIVITY FEED, so the two never disagree: a turn's spend, a board re-order and an edit to the project's own settings write no such row and do not move it. It is OMITTED for a project no work has ever been filed into, because "nothing yet" is a different answer from any instant. `unit.resolved` is a FIELD rather than an absence: "this project names a unit the chart no longer has" is a finding, and an absent unit would be indistinguishable from a project that names none. `archived` and `sort` are both CLOSED SETS the engine owns, and both act on the whole company rather than on the page: the answer is capped at 200 rows, so a caller that widened and then narrowed found no archived row at all once the live projects filled the page, and a caller that re-sorted the page ranked the first two hundred keys rather than the company. `total` counts the SELECTED set, which is what makes "N of M" readable on every one of them, and it IS `census[archived]`, computed from the one aggregate rather than a second `COUNT(*)`, so the number printed beside the rows cannot disagree with the counts printed on the control that chose them. The census is what a SEGMENTED screen needs and cannot derive: on the live segment the archived count has no row on screen to be derived from, so without it an empty answer could not tell a company with no projects from one that has archived every one of them |
| `work_project` | `{key, for_type}` | `GET /worklet/projects/{key}`. A SET read, not a point read: its shape is dominated by aggregates over task rows (the counts), so it carries `complete`/`incomplete` under the same contract every set answer takes, and its closure is the project's container plus both catalogues. It carries the listing's `task_counts`, `target_date` and `last_change` too, read from the same row so the directory and the project's own page cannot disagree. `shadowed` names the workspace field ids this project redeclares, which is what a field in the middle of a move between scopes looks like |
| `work_workload` | `{unit, read_level, …}` | `GET /worklet/workload`. Who is carrying how much, for everybody at once. It counts OPEN work (every task assigned to somebody, whatever its dates), because "who is carrying the most" is a question about a whole queue. Both measures a company may size in are carried rather than one chosen, since which is used differs by team and an answer that picked one would be wrong for everybody sizing in the other. Beside them it carries `blocked`, `overdue` and `unscheduled`, because a person whose whole queue is blocked has a different problem from one who is simply busy; `overdue` is cut on the company's own midnight, the day `work_items` and `work_my_work` cut on. Ordered heaviest first and capped, with `truncated` when the cap was reached. `unit=` narrows to the people whose open work sits in projects that unit owns, named by the unit's key or by its name, in any case |
| `work_flow` | `{bucket, points, project, read_level, …}` | `GET /worklet/flow`. The company's work as a SERIES: `points` windows (1–90, default 14) of a `bucket` (`day`, the default, or `week`) on the company's clock, oldest first, the last the window now falls in. Each point is `{window, start, end, not_started, active, done, closed, completed}`: the census at the window's END (at now for the current one) and how many changes in it took a task from not delivered to delivered (a cancellation is not one; done → closed is not a second). Replayed BACKWARD from today's census over `tracker_history` (status, create, remove, restore, purge and project moves, through the partial index replicated migration 0029 adds), so the cost is the window's changes, not the company's history. `now` is `{not_started, active, blocked, overdue}` (`overdue` cut on the company's midnight), and `blocked_history` is always `false`: nothing records when a task became blocked, so no blocked series is drawn from a guess. `project=` narrows to one project, following a task to where it was at each instant |
| `work_item_turns` | `{id, cursor?, limit?, read_level, …}` | `GET /worklet/turns`. One page of the agent TURNS charged to an item, newest first: `{item, key, turns, next_cursor?}` plus the coverage half. Each turn is `{turn_id, ordinal, seat, trigger?, segments, tokens, cache_read, rounds, wall_ms, workers?, sent_back?, outcome, phases, failed_in?, summary?, review?, tools?, at}`: a turn's SEGMENTS folded into one (a turn that parked on its executor is charged once per segment): tokens, rounds and wall time summed, `phases` in the order each first ran, and `outcome`, `summary` (what it did, in the agent's words) and `review` (what the newest review that sent the work back asked for) off its newest segment; `tools` is each tool its executor called with its `calls`. `failed_in` names the phase that failed when `outcome` is `failed` because one did (`phases` lists every phase that ran, the failed one included), and is absent for a failure outside every phase and on a turn that did not fail. `ordinal` is "Turn n": the position of the turn's counted segment among every counted segment on the item, so the newest turn's `ordinal` is the item's `spend.turns`; a turn with no counted segment on this item (more of a turn charged to another) has `0`. `at` is when the newest segment landed. Read from Worklet's own turn rows (the account `spend` sums), so it answers on any node and outlives the thirty days a node keeps its event history, where `turns?work_item=` reads the event history. `next_cursor` is the position of the oldest turn's first segment; the next page is the turns whose first segment is below it, so a turn that gains a segment while you page never moves between pages. `limit` defaults to 20 and is held to 50; a cursor this endpoint did not hand out is `bad_params` |
| `work_comments` | `{item, cursor?, limit?, thread?, with_root?, read_level, …}` | `GET /worklet/comments`. One page of an item's THREAD, newest page first and each page in the order it was written: `{item, key, title, comments, next_cursor?, key_collision?}` plus the coverage half. `next_cursor` reads the page before and is absent when this page reaches the first comment; it continues from `work_item`'s own `comments_cursor` exactly, since both are the same read. `limit` defaults to the detail's 20 and is held to 50. `thread` names a comment and narrows every page to its conversation: the comments replying to it and the answers to it when it is an ask, with the comment itself only when `with_root` is true. Narrowed in the read rather than by the caller, so `next_cursor` is present only when that conversation goes back further: a page of the whole thread kept to one question's replies carried the task's cursor, and a question nobody had replied to offered earlier replies. Each comment's `author` is who wrote it (a person as their seat, beside `author_kind`), so a pane draws a name with no second lookup. `key_collision` says the item's key opens another task, as on `work_item`. Its own question beside `work_item` because nothing followed that cursor, so every comment older than the twentieth was unreachable, and because a pane that draws a conversation wants the thread without the history, links and fields a detail assembles |
| `decisions` | `{handle?, read_level, …}` | `GET /worklet/decisions`. What waits on ONE PERSON's decision: the open asks put to them (to their own record, a person's seat) and the executors parked on a question put to them (`awaiting_clarification` or `reseed`), newest first, at most 50. `{handle, items: [{kind: "ask"\|"run", at, ask?, run?}], total, capped, oldest_at?}`: `total` counts every one, not the page; `capped` says the ask count stopped at the engine's ceiling, so `total` is a floor; `oldest_at` is when the longest-waiting began. An ask is `work_my_work`'s `asked_of_me` row and a run is `executors`' row. The same scope rule as `work_my_work`: absent is the caller's own record, and a named one is resolved first (a login to the record its holder's work is kept under) and then takes whoever leads them, or `admin`. Your own asks need no app at all, so a person without `worklet` still sees what they were asked. On a company that does not run Worklet (Jira, or none), the asks are left out and the parked runs are still answered, with no `read_level` or `complete` beside them, since no tracker was read |
| `answered_decisions` | `{handle?, read_level, …}` | `GET /worklet/decisions/answered`. What ONE PERSON decided lately: the asks put to them and closed in the last seven days (`worklet.AnsweredWindow`), by them or by somebody else, or resolved without an answer, newest closing first, at most 50. `{handle, items: [{at, ask, answer, followed_up?}], since, truncated, read_level, complete}`: `ask` is the row `decisions` listed while it was open, with `open: false` and no `answer_with`; `answer` is `{by, by_kind?, at, comment?, choice?, body, resolved?}`, where `by` is whoever closed it (not always the person asked), `choice` the option chosen by id and `body` the answer's words; `followed_up` is the asker's first record on the task after the commit that closed the ask (`{at, kind, excerpt?, fields?, comment_id?}`, `work_activity`'s fields), absent while there is none, and left out for a reader the tracker shows no task to, since it is the task's record rather than the question's. An executor's question is listed only for the person who answered it (ADR-0068), never for the rest of the people it was put to: it is listed beside them as `{at, run, answer}`, with no `ask` and no `followed_up`: `run` is `{turn_id, launch_id, agent_handle, question, question_context?, decision?, asked_at, work_item?}` and `answer` is the same shape, read from the `executor_answered` record the fleet's event log keeps, found by who answered it (answers the run took, given by that person, shown to whoever reads the run's seat trail and to the person who answered). `since` is the start of the window and `truncated` says it held more than the page. The same scope rule as `decisions`, and the same answer without the app: your own are listed whether or not you hold `worklet`. A company that does not run Worklet is answered its run questions alone, with no `read_level`. Served from the index replicated migration 0052 adds, one person's closed asks |
| `company_feed` | `{kinds, actor, limit, cursor, read_level, …}` | `GET /feed`. What the company DID, newest first: `completed`, `created` and `handoff` from Worklet, `page` (created or saved) from Doclet, and `schedule` fires from the usage domain, never a chat message, which is conversation rather than something the company did and is readable only where its room is. `kinds` is a comma list, empty for every kind this node keeps; asking for one it keeps no source for is `bad_params`. A kind is kept only where the company runs its half: a company on Jira keeps no Worklet kinds and a company on Confluence no `page` kind, and the feed answers the kinds it does keep (the `schedule` runs on every company), never refusing whole for the half it leaves out. Which halves a company runs is the org answer's [`services`](#get-org). `limit` is 1–50 (default 20). `{rows: [{kind, at, work?, page?, schedule?}], next_cursor?, complete, read_level}`. A Worklet row carries its task's key and title, and, by kind, the task's running `spend` (`{tokens, turns}`, tokens only) and `first_pass` (no reviewer sent it back), the create's `origin` (`{surface, conversation}`: the chat surface the filing turn was woken on), or the hand-off's `from`, `to`, `reassignments` and `reassignment_budget`. A `schedule` row is a RUN, a fire the scheduler dispatched and never a tick it skipped (those stay in `schedule_runs`), and consecutive runs of one schedule for one runner that no other row falls between are ONE row: `schedule.runs` counts them (at least 1) and `schedule.since` is the oldest one's instant (set when `runs` is above 1). A page that folds many runs reads its source again from where it stopped, at most four reads per source; past that the page is answered short, never out of order. MERGED BY INSTANT, CURSORED PER SOURCE: `next_cursor` resumes each source exactly where this page stopped consuming it, so a scroll never repeats or skips a row however the three interleave, and is absent once every source is read to its end. A `read_level=session` floor names Worklet's log (this is a Worklet session question); the Doclet half is read at the dashboard's own level |
| `work_activity` | `{task, container, kinds, actor, actor_kinds, assignee, q, notified, since, from, to, limit, cursor}` | `GET /worklet/activity`. Every commit writes a history row, so this is an account of what HAPPENED rather than of what was announced: `notified` is how a reader tells the two apart, and `kinds` is what the change WAS whether or not anybody heard about it. The two used to be one: a record's kind was read off its notification, so the same change filed under one word with an audience and another without, and a quiet removal reached the feed as `tombstone` while the filter spells it `removed`. `kinds` accepts the twenty-seven change kinds and nothing else: one this build does not have is refused `bad_params` naming the set, by this route and by a seat's `task_activity` alike, because a filter matching nothing answers an empty feed that reads exactly like a quiet company. Each record carries BOTH instants: `at` is the authored one a card renders, and `effective_at` is the fleet-agreed one every duration is measured on. `task` answers that task's WHOLE history, the changes it had before it moved projects included, and a `container` named beside it is a check rather than a filter: it must be the project the task is filed in, and any other is refused `bad_params` naming the one it is — a tracker holder reads the whole tracker, so a container would keep out nothing the reader is not owed, and as a filter it hid every change written while the task sat elsewhere. `actor` is who made the change; `assignee` is whose work it is, and the two are routinely different people. `actor_kinds` is a CSV of `agent`, `human`, `operator` and `system` and narrows to WHO WAS WRITING rather than to which handle, which is not the same question and cannot be asked as a set of handles, since an `operator` commit carries a token's own label where an `agent` one carries a seat handle, and the set of people is the roster, which changes. It is REFUSED when it names a kind this build does not have, as `kinds` is: a filter silently ignored answers a wider question than the caller asked, and on an audit feed that reads as a company where everybody is an operator. Each kind named is read as its own newest-first range and the ranges are merged into one page, so an audit of several kinds costs a page of each rather than every commit those kinds ever wrote, at any age of the company. The `q` gate is on what the query would SCAN, never on which keys were named: `container=workspace&q=` and a five-year `since` both name a key and narrow nothing. `fields` is what MOVED, in the same `{from, to}` shape for every kind, the full list of names a task's own row draws on is there too, and `keys` names the tasks those deltas point at; `ask` is the question a commit asked or answered, in `work_inbox`'s shape and read the same way; see [`GET /worklet/activity`](#routes). `subject_key_collision` says the record's `subject_key` opens ANOTHER task (one that claimed the key first), so a link to the subject goes by `subject_id`. A `cursor` or a position `since` must be on Worklet's own log, and one from another domain's log (or a `cursor` that is not a position) is refused `bad_params` rather than picking a page by a number from elsewhere |
| `work_my_work` | `{handle}` | `GET /worklet/my-work`. Seven lists, each bounded at 20 so no block crowds out another: the whole answer is read as one page. `priorities` is NOT re-sorted: the order is what somebody decided. A finished or removed task is filtered out of it rather than rewritten out, because a read must not write to somebody's own object. `totals` counts each list IN FULL (`{priorities, assigned, asked_of_me, checklist_items, collaborating, watching_recent, unblocked_recent}`, each `{total, capped}`), by the predicate that drew its page and in the same read, so a count is drawn from there and never from a list's length; `capped` means the count stopped at 10,000. Every row's `overdue` mark is cut on the company's own midnight (the [`timezone`](../getting-started/configuration.md#the-companys-clock) a board's due bands are cut on), so a task due today is not overdue here while a board says today. `handle` DEFAULTS to the caller's own record, and naming anybody else's takes the owner-or-lead rule: whoever leads them, or `admin`, with `worklet`; see below. Every task row carries `key_collision` and every checklist item `task_key_collision`, and an ask's `answer_with` names the task by its key or, where that key opens another task, by its id |
| `work_inbox` | `{handle, unread, primary_only, snoozed, reasons, limit, cursor, since}` | `GET /worklet/inbox`. One person's notices, newest first, 50 to a page. Each names the ONE reason of eighteen it reached them under, `addressed` (it asks something of them rather than informing them), `fallback` (nobody better was found), and their own read and snooze marks. `actor` is who made the change and `actor_kind` its kind: a person as their seat, a credential nobody is bound through under its login. `comment_id` and `turn_id` are the comment the change wrote and the turn that made it; `ask` is the question the notice is about (the ask itself for `asked`, the ask it answered for `answered`), as `{comment, asked_of, open, answered_by, answered_at, resolved, choice, decision}`, read NOW, so an ask somebody has since answered reads `open: false`. `snoozed` is `exclude` (the default), `include` (snoozed notices kept and marked) or `only` (just what was put off); a snooze whose time has come is back under every scope and is not `only`. `snoozed`, `unread`, `primary_only` and `reasons` all narrow the SCAN rather than the page, by the same rules the marks are computed with, so a page holds `limit` notices whenever the scope does: they used to be applied to the page after it was read while the cursor was taken before, so a person with their newest fifty notices snoozed or read opened an empty page with a cursor behind it. `subject_key` is the key the notice's task held when it was written, and `task` is that task's id: the subject for a task commit, and the task a lead put first for a `prioritised` notice, whose `subject_id` is the PERSON whose list it is; a notice naming no task carries none. `subject_key_collision` says the stored key now opens a task OTHER than `task` (asked of the stored key itself, so a notice about a duplicate moved since is still flagged), and the notice is then reached by `task`; it is never set on a notice that names no task, whose key is the only address it has. `primary_reasons` is the split that was APPLIED, defaulted, so a caller renders *you are seeing these because* without repeating the rule; `unread` and `primary` are counts over the PAGE and say so, because a total over the table is a second scan of rows this answer did not return. `reasons` FILTERS rather than classifies (the primary split classifies the same rows), and an unknown one is refused naming the eighteen. `since` is a log POSITION (`<stream>@<generation>:<sequence>`, what `seen_through` renders), never a bare sequence, and a position on THIS log: a `since` or a `cursor` from another domain's log, or a `cursor` that is not a position, is `bad_params`, because both are compared against Worklet's own sequence numbers and one from another log would pick a page by a number that means nothing here. Same scope rule as `work_my_work` |
| `work_search` | `{q, limit, mode?}` | `GET /worklet/search`. The company's work RANKED against a phrase: `hybrid` by default (BM25 over the engine's own inverted list fused with the semantic scan over the replicated vectors), or `keyword` / `semantic` alone, which is the same fan-out a seat's `search_work_items` runs. Not a filter: `work_activity`'s `q` is an escaped LIKE over an excerpt, gated to a span of days, and answers a different question. Answered only where the company runs Worklet (a company whose tracker is a vendor's, or none, gets `unknown_query`, as it does for every other work question, and a node with no company yet `unavailable` with `refusal: "no_active_revision"`), and holding the board is not holding an index: a node that joined recently has every row and no index, and answers `available: false` with `reason: "building"` rather than an error or an empty result: nothing is wrong, and a reader told *nothing matched* files the duplicate. A hit carries `rank`, its 1-based PLACE in the answer, and never a score: the fan-out finishes the ordering on the nodes that scanned each slice of the corpus and hands the coordinator ids best first, so a score on the wire could only ever read zero. It carries the item's `id`, `key`, `title`, `project`, `type`, `status`, `assignee` and `priority`, the index's own `snippet`, and `key_collision` exactly as a board row does. The answer carries the same outcome fields as `knowledge` (`mode`, `served_mode`, `modes`, `degraded` and `coverage{nodes, complete, buckets_missing}`), and a mode it does not know is refused, as a `q` past 400 bytes is: `bad_params` naming its size and the limit, asked before the mode, rather than cutting it |
| `work_routing` | `{record_id}` | `GET /worklet/routing/{record_id}`. Who ONE change woke, and under which reason: the fact no other tracker records. `tracker_notifications` has always been readable by RECIPIENT (`work_inbox`); this is the same rows by RECORD, which is a primary-key prefix scan and needs no index of its own. Each recipient names the ONE reason of eighteen that found them, `addressed` (it asks something of them), and `fallback`/`fallback_rank` (nobody better was found). `notified` is the history row's own flag and means the commit CARRIED a notification, never that somebody was woken, since the applier deliberately does not hold the roster that would need. So an empty recipient list is THREE facts and `delivery` tells them apart: `nobody` (announced, inside the retention window, and every candidate was the actor or has left), `swept` (older than `tracker.worklet.inbox_retention_days`, so their absence is not evidence), `unknown` (no horizon stated) and `quiet` (the commit announced nothing, which is most of them). `retained_from` is the instant that decision was made against |
| `viewer` | `{}` | `GET /viewer`. `{login, grants, handle, name, kind, owner, acts, project, config_writer, config_managed_by, assistant, chat_vendors}`; `assistant` is [Ask Crewlet](#ask-crewlet)'s availability to this caller, `{available, reason?}`; `acts` is what [`/operator/act`](#operatoract--the-dashboards-write-surface) would serve this caller, the catalogue's writes the authority table can admit them to before any object is named, and `project` is where their `create_work_item` lands when it names none: the engine's own default for their seat (the seat's project, else its unit's, else the nearest ancestor's), so a screen offering "Create task" says where rather than working out a second answer; `""` when there is none, and a create must name one. Registered on EVERY build with no seam of its own: who is asking is a property of the request rather than of anything this node stores. A bound caller, every person, and a token bound to a seat, carries a `handle`; an unbound one, a Tier A token or a service account bound to none, carries a login and no seat, which is an ordinary state rather than a refusal (a person is never unbound: every person holds a seat). `owner` is the name the caller's OWN record is kept under, the seat when bound, the login when not, and it is what a screen asks the personal questions by, never `handle`. `config_writer` is whether the caller's credential may change the company document, false for an anonymous caller and for every credential a managed document's `api.auth.company_writers` does not list, and `config_managed_by` is that list, `[]` when the document is not managed and for an anonymous caller, so a screen draws every editing control disabled with who manages it rather than offering a save the engine refuses. `chat_vendors` is the vendor chat surfaces (`mattermost`, `slack`) a company whose chat is a vendor's declares, the one further fact a screen naming where the company talks needs beside the org answer's [`services`](#get-org), answered to a principal holding `chatlet` and `[]` to anybody else, on a company on its own chat or on none, and on a node with no company yet |
| `work_person` | `{handle}` | `GET /worklet/people/{handle}`. Scoped like `work_my_work`: absent is the caller's own record, and somebody else's takes their lead or `admin`, with `worklet` either way. `due` is the snoozes whose time has come, REPORTED rather than promoted: putting one back in the unread list is a write, and a read that performed one would change fleet state from a path with no operation id and no record. `priorities_set_by` is who last set the queue when it was not this person (the name their writes are recorded under, a person's seat), which is how a lead's authority is made visible beside the `prioritised` wake the write sends them: a wake that names the task now at the top of the list, because a notification here is task-shaped and "your list changed" names nothing to act on. `max_snooze_ahead` is how far ahead a snooze may be set, in SECONDS: the engine's bound, so a screen offers only the presets `mark_inbox` will accept |
| `work_views` | `{container, counts}` | `GET /worklet/views`. `container` is the strip's own (`workspace`, `project:ENG`, `unit:engineering`, `person:ana`), and it is REQUIRED, because a strip belongs to exactly one. Whose personal views appear and whose pins come first is the CALLER's, never a parameter: their [own record](#whose-record-a-personal-question-answers-for), which is the login of a Tier A token or service account bound to no seat; a caller the engine can name nothing for gets the shared strip: no pins and no personal views but the shared ones. Every row carries `builtin`, which is what tells the six nobody saved from the ones somebody did: a builtin row has no `id`, so there is nothing to rename, protect, rank or pin. `params` is the saved query in `work_items`' own parameter names (this channel's, not the `list_work_items` TOOL's, which renames four of them for a model), so a caller either hands them straight back or, simpler, passes the view's `id` as `view=` and lets the engine expand it. `counts=true` adds `count` to every row PINNED for the caller (the `total_hint` `work_items{view, container}` answers for it, run as the caller on the company's clock, with `count_capped` when it stopped at 10,000), or `count_refused` naming why a view that no longer compiles could not be counted. At most 32 counts, in the strip's one read; only a record has pins, so it is refused `bad_params` to a credential that names no record of its own |
| `work_saved_views` | `{counts}` | `GET /worklet/views/saved`. Every SAVED view (never a builtin) that the caller can see across every container: the shared ones, and the caller's own personal ones, pinned-for-them first and then by container (the workspace, projects, units, people) and the strip's own rank. Each row is `work_views`' row shape, so `container` says where it lives and `params` is its saved query. A sibling of `work_views` rather than a `container=` it takes, because a strip is ONE container's tabs and this is one PERSON's views (the inventory of what somebody saved and the pins a sidebar draws), which the workspace strip could not answer: a view saved on a project board appeared in neither. Whose is the caller's [own record](#whose-record-a-personal-question-answers-for), never a parameter, and `counts=true` takes the same rule as `work_views`: every pinned row's `count` is its view run IN ITS OWN CONTAINER, which is what opening it runs |
| `work_files` | `{project, folder, removed, limit, after}` | `GET /worklet/files`. Answers `{project, files, next}` plus the coverage half. Each file carries its `path`, `content_type`, `size`, `hash` (the SHA-256 of its whole content), `version`, who created and last wrote it and when, and (only with `removed`) who removed it and when. `next` is absent on the last page. The listing is rows only: the bytes are read through the byte route, never through the socket |
| `pages` | `{container, parent, roots, status, label, watcher, title, skills, onboarding, order, limit, after}` | `GET /doclet/pages`. `skills` is three-stated: only the tool-skill pages, everything but them, or everything. `order` is `title` (the default: by space, title and id, the order every tree is drawn in) or `newest` (by when each page last changed, newest first, then by id), and `newest` is served beside `skills=true` alone — anything else is `bad_params` — because only the tool skills have an index in that order (replicated migration 0067). A cursor is a place in ONE order, so a cursor minted in the other is `bad_params` too. `roots=true` is the TOP of a container (the pages with no parent), and is refused beside `parent`, because an empty `parent` already means "under any parent" and the two ask opposite questions. The answer carries `total` (every page the filter matches, counted in the same transaction as the rows) and, while there is more, `after`: pass it back as `after` for the next window; a cursor this listing did not mint is `bad_params`. Each page carries `children`: how many pages sit directly under it that the SAME listing would show (its `status` and `skills` narrowing applied to them), absent when none: what a tree draws its expander off. With `skills=true` on a node that reads the replicated `usage` domain the answer also carries `skill_loaded_by`: page id → the `page` answer's `skill_loaded_by` list for that page, answered for the whole window in ONE read rather than one per row. Each page also says whether it is a `folder` (its body is empty: a page that only groups the pages under it), and its last save: `saved_at` and `saved_by`, the last create, save or rename, which a label, a watch or a move does not move. `saved_by` is the writer under the one name every row of theirs carries, as every author field on this surface is: a seat's handle, which is how a person appears since they write as the seat they hold, or the whole login of a credential bound to no seat |
| `page` | `{id}` | `GET /doclet/pages/{id}`: id or `CONTAINER/Title`. `children` is the first 50 by title and `children_total` all of them. `skill` says the page is a TOOL SKILL (admitted to the skills registry) and `onboarding` that it is an onboarding page. On a tool-skill page, `skill_loaded_by` is every seat it reached as a skill over the last 30 company days, most recent first: `{handle, last_at, count, loaded, offered}`, where `loaded` counts the seat asking for the body (`load_tool_skill`) and `offered` a phase's catalogue putting its summary in front of the seat; absent on a node that does not read the `usage` domain, and `[]` when nobody was. `linked_from` is who LINKS here, from this node's index: `pages` (`{id, container, title}`, published pages whose body carries this page's address) and `tasks` (`{id, key, key_collision?, title, status, via}`, where `via` is `linked_page` for a page relation and `description` for an address in the description, and `key_collision` marks a task whose key another task claimed first, so a link to it goes by its `id`: the key opens the claimant), each capped at 50 with `pages_total` / `tasks_total` beside it. A link is a page id in either address the engine reads (`/doclet/pages/<id>` or the dashboard's `#/doclet/pages/<id>`), outside code; a title is not an address, since a rename moves it. Absent on a node with no index. A node that HAS an index and cannot answer sends `linked_from_status` instead of an empty list: `building` while its index is on its first lap over pages and tasks (a fresh or joined node's first minutes: an empty list then would claim nothing links here before every body was read), `unavailable` when the read failed; the page itself is served either way. `folder` is the listing's |
| `page_reads` | `{page, days}` | Who READ a page: one row per (seat, way of reading) over `days` company days (1–30, default 30, anything else `bad_params`) from the replicated `usage` domain, so every data node's reads are counted (a departed node's included), and every node answers alike. `via` is `get_page`, `search`, `prefetch` or `skill_loaded`; a skill's catalogue OFFER is deliberately not a read (it would make every skill "read by every agent today") and is what `page`'s `skill_loaded_by` counts. Each row carries `count`, `last_at`, and the newest read's `last_turn_id`, `last_work_key`, `last_query` and, where the run was charged to a task in Worklet, `last_work_item{id, key, title, ordinal}`, as in "turn 2 on ENG-412". At most 100 rows, newest first, with `readers_total`; `distinct_seats_today` is the seats that read it since the company's midnight, and `elided` how many (page, way) entries the per-seat-day cap dropped across the window, COMPANY-WIDE rather than for this page: some may have been this page's and none may have been, so a non-zero value says the list MAY be short (0: it is certainly complete). `since`, `until` and `days` name the window. Registered only where this node reads the `usage` domain |
| `containers` | `{}` | `GET /containers`. A separate question from `pages` rather than a facet of it: a browser draws the container list once and the page list on every navigation |
| `page_activity` | `{page, container, kinds, actor_kinds, since, cursor, limit}` | What happened to a page, or to everything in a container: Doclet's own change log, mirroring `work_activity`. `kinds` is a CSV of the ten change kinds and `actor_kinds` of the three author kinds (`agent`, `human`, `operator`), the latter refused when it names one this build does not have and read one kind at a time, merged into one page; `work_activity`'s note says why. `since` bounds the window and `cursor` pages it: the same unit, two parameters, because the cursor moves with every page and the bound does not. The answer carries `log_seq` and `applied_through` as a Worklet answer does (this node's checkpoint on Doclet's log and the prefix of it whose records it applied, read in the transaction that read the changes), so a node holding a record it cannot decode says it is behind |
| `page_revision` | `{page, version}` | One revision's own body, message and author. Revision N is the body AT version N — including the newest — so a reader comparing two versions asks for both rather than for one and the head |
| `artifact_data` | `{id, collection, since, cursor, limit}` | `GET /artifacts/{id}/data/{collection}`. One page of a collection's documents under its read rule, or with `since` the documents changed after that version, deletions included |
| `artifact_data_doc` | `{id, collection, doc}` | `GET /artifacts/{id}/data/{collection}/{doc}` |
| `artifact_data_query` | `{id, collection, q}` | `GET /artifacts/{id}/data/{collection}/query`. `q` is the query as JSON: `where`, `order_by`, `limit`, `after` |
| `artifact_data_history` | `{id, collection, doc, field, actor, cursor, limit}` | `GET /artifacts/{id}/data/history`. Seven days of writes, refusals included, with each changed field's values before and after |
| `artifact_data_schema` | `{id}` | `GET /artifacts/{id}/data/schema`. The collections the caller may read, the store's footprint and state digest, and the caller's role |
| `artifact_data_state` | `{id}` | `GET /artifacts/{id}/data/state`. The caller's own per-viewer state keys |
| `artifact_data_usage` | `{}` | `GET /artifacts/data/usage` *(needs `admin`)*. The company's datastore footprint against its quota |
| `chat_channels` | `{include_archived, unread, limit, with, read_level, …}` | `GET /chatlet/channels`. `{channels, read_state, read, default_private, truncated, unreadable}` plus the read's `read_level`, `complete` and `position`. Each room carries the reader's own summary (the room, its revision, the last message, its participants), with `unread` counted above the caller's cursor over the room's posts (a reply in a thread is read there and does not count) and stopped at 100 (`unread_capped` says it stopped), `mentions` (how many of those name the caller or the whole room with `@channel`; room events, the caller's own messages and tombstones are never counted), and `muted`. `with` (a seat's handle) keeps only the caller's rooms that seat is also in, which is how a profile lists the rooms two people share. `read` is the caller's WHOLE read state, named by their seat and carrying its `revision`, the order a screen keeps it against the `chat_cursor_moved` pushes by; `read_state: false` says the coordination record could not be read, and then every badge is zero, every mute absent and `read` absent with them: an unknown cursor is never a cursor at zero, which would badge every room with its whole history. `default_private` is what a room created without a stated visibility will be (`chat.chatlet.default_channel_private`), so the create form opens on the company's own default. An archived room is left out unless `include_archived=true`. `limit` (1 to 256, every room by default) answers the most recently active that many, cut from the one list sorted by activity and never paged; `unread=true` keeps only the rooms with something unread, and is refused `503 unavailable` while the read state cannot be read, since zeroed counts would say nothing is unread. `truncated` says rooms were left out: the caller is in more than 256, or more matched than `limit`. See [Chatlet](#chatlet) |
| `chat_channel` | `{channel_id, read_level, …}` | `GET /chatlet/channels/{channel_id}`. The room, its revision, its message count, its members and whether the caller is one. Here and in `chat_messages` and `chat_thread`, `channel_id` is the room's id or its name, with or without the `#` people write before it, so a room the caller is not in is reached by the name everybody calls it |
| `chat_messages` | `{channel_id, cursor, limit, read_level, …}` | `GET /chatlet/channels/{channel_id}/messages`. `{channel_id, messages, next_cursor, unreadable}` plus the read's level and position. The cursor is the room's own message number, sent back as it was answered; a cursor the room refuses is `400 bad_params` in the domain's own words. A room post somebody replied to carries `thread`: `replies` (a tombstone included), `last_reply_at`, `last_reply_position`, `repliers` (the first three, in the order they first replied) and `replier_count`. A row with `event` is a room event, the room's own change rather than anything said (see [Chatlet](../concepts/chatlet.md#a-room-says-what-changed-in-it)) |
| `chat_thread` | `{channel_id, root_id, cursor, limit, read_level, …}` | `GET /chatlet/channels/{channel_id}/threads/{root_id}`. `{channel_id, root, replies, participants, next_cursor, unreadable}`. A thread is one level deep, so this is a range in one room |
| `chat_activity` | `{kind, unread, cursor, limit, read_level, …}` | `GET /chatlet/activity`. `{items, next_cursor, unread, read_state}`: what waits for the caller, newest first (mentions, replies in threads they follow, reactions to their messages), each row with `unread` and the `read_through` position that marks it read; `unread` counts the unread mentions and replies over the newest 100 of each. `unread: true` answers the unread rows alone, whole, and takes no cursor; it is refused `unavailable` while the read state cannot be read |
| `chat_threads` | `{cursor, limit, read_level, …}` | `GET /chatlet/threads`. `{threads, next_cursor, read_state}`: the threads the caller follows, newest reply first, each `{channel_id, channel_name, channel_kind, reason, root, latest}` |
| `chat_saved` | `{cursor, limit, read_level, …}` | `GET /chatlet/saved`. `{saved, next_cursor, read_state}`: the caller's saved messages, the newest save first, each `{mark, channel_name, channel_kind, view}` or `gone` |
| `chat_sent` | `{cursor, limit, read_level, …}` | `GET /chatlet/sent`. `{messages, next_cursor}`: what the caller said, newest first, each `{channel_name, channel_kind, view}` |
| `chat_links` | `{channel_id, cursor, limit, read_level, …}` | `GET /chatlet/channels/{channel_id}/links`. `{channel_id, links, next_cursor}`: every link the room's messages carried, newest first, each `{url, message_id, thread_root, author, author_kind, at, channel_seq}` |
| `chat_message` | `{channel_id, message_id, read_level, …}` | `GET /chatlet/channels/{channel_id}/messages/{message_id}`. `{channel_name, channel_kind, view, root, before}`: one message, with its thread's root and the two replies before it when it is a reply |
| `chat_unfurls` | `{urls}` | The socket only. `{unfurls}`: the previews of one message's links (at most 8), each `{url, kind, href, code, title, status, tone, owner, meta}`. A link is previewed only when it is one of this dashboard's own addresses (the origin of `api.external_url` and a task's, a page's or an artifact's route) and the caller may open its record: each record is asked of its own screen's question (`work_item`, `page`, `artifact`) as the caller, and `href` is built from the record, never taken from the message. A github.com pull request under an owner the GitHub configuration names is previewed with its repository and number (`code`), title, state and size (`meta`), read through the company's GitHub wiring ([pull request previews](../integrations/github.md#pull-request-previews-in-chat)). Every other link is absent and drawn by the link rules |
| `chat_search` | `{q, channel_id, author, limit, read_level, …}` | `GET /chatlet/search`. `{hits, searched, building, note, read_level, position}`: each hit its `message_id`, `channel_id`, the room's `channel_name` (none for a direct conversation) and `channel_kind`, as a mention carries them, `thread_root` (empty for a room post), `author`, `snippet` — the message around the words that matched, cut by the index and marked where it was — `at` and `score`. `searched` is how many rooms the search ran over, which tells "nothing matched" from "nothing was looked at"; `building` (with a `note` saying it in words) says the answering node's index had not caught up with its transcript — a message, an edit or an erasure past what it has indexed, a rebuild under way, or an index built from a copy of the estate that has since been replaced, measured on both sides of the search and `true` too when the node could not tell — so the hits may be drawn from part of it. The visible set is resolved by the data node that answers, from the caller's seat: every public and unit room, joined or not, and the private rooms and conversations the caller is in; a `channel_id` the caller may not read narrows to nothing rather than being refused. `q` takes at most 400 bytes and is refused past that, never cut. `limit` is 20 when absent and 40 at most — a larger one is answered with 40, the most one answer holds — and there is no next page, so a full page may have more matches behind it. Unregistered on a node with no chat index |
| `chat_room_names` | `{read_level, …}` | `GET /chatlet/room-names`. `{rooms, truncated}` plus the read's `read_level`, `complete` and `position`: every named room the caller may read, each its `id`, `name`, `kind` and `archived`: every public and unit room, joined or not, and the private rooms the caller is in. The live rooms come in name order, then the archived ones, the most recently archived first, at most 1000 (`chatlet.MaxRoomNames`, the most live rooms a company holds), and `truncated` says rooms were left out. What a reader draws a room named in prose by: a room's name may be spelled as a colour (`#e2e`) or an issue's number (`#2026`) |
| `chat_rooms` | `{read_level, …}` | `GET /chatlet/rooms`. `{rooms, truncated, unreadable}` plus the read's `read_level`, `complete` and `position`: every named room the caller may read, joined or not, the archived ones included, in `chat_room_names`' set and order and at its bound. Each is the whole `channel`, its `members` (how many seats are in it), `member` (the caller is one) and `last_at` (when somebody last said something there, absent for a room nobody has spoken in; a room event is not activity). What a person browses to find a room to join |
| `config` | `{}` | `GET /config` *(needs `admin`)* |
| `config_audit` | `{limit}` | The revision history — no REST twin; `GET /config/revisions` serves the same records *(needs `admin`)* |
| `config_diff` | `{revision_id}` | [`GET /config/revisions/{id}/diff`](#get-configrevisionsiddiff) — the listing is cut at 500 and `changes_total` is how many there are *(needs `admin`)* |
| `config_entities` | `{kind, id}` | One addressable collection of the active revision: its ids, or one entity out of it. The read half of Settings › Configuration, whose write half is `PUT /config/{kind}/{id}` *(needs `admin`)* |

**Every Worklet answer carries how far this node had got, and both halves
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
every question that carries a read level**, not only on `/worklet/items`:
`/worklet/items/{id}`, `/worklet/views`, `/worklet/catalogue`,
`/worklet/people/{handle}`, `/worklet/projects`, `/worklet/projects/{key}`,
`/worklet/activity`, `/worklet/my-work`, `/doclet/pages`, `/doclet/pages/{id}`,
`/containers` and every `/chatlet/` read all resolve them the same way. They did not until recently:
each of those wrote a hardcoded `stale` into its read and never looked at the
keys, so a caller asking for a stronger answer was served this node's rows and
told, in the answer's own `read_level`, that it came back at the level asked
for, and the two single-object reads, `/worklet/{id}` and `/doclet/pages/{id}`,
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

**A read no data node could answer is `unavailable`, never a list.**
Worklet, Doclet and Chatlet are answered by a data node's copy of the estate
(this node's own where it holds one, another data node's otherwise), and a
read nobody answered is refused `unavailable` (REST: `503` with a
`Retry-After`) rather than answered as an empty list, which would say the
company has nothing in it. `knowledge` is the one exception, because a search is
best effort: a search no data node ran answers no hits with `served_mode`
empty and `coverage.complete` false, which a screen renders as "could not be
searched" rather than "nothing matched".

### Whose record a personal question answers for

Four questions answer about **one person or seat** rather than about the
company: `work_my_work`, `work_inbox`, `work_person` and `conversations`. Every
one of them takes a `handle`, and the rule for whose is the same, in one place:

1. **No handle** answers for the caller's **own record** — and for
   `conversations`, their own seat.
2. **Your own handle** is the same thing said explicitly — and on the three
   person questions either of your names is: a person's login names the same
   record their seat does.
3. **Somebody else's login** names **their** record, looked up in the identity
   directory: the seat it binds them to — every person holds one — or the
   login itself for a service account bound to none. `ana.diaz`, bound to the seat
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
   it can without it**: `admin` is admitted whatever the login turns
   out to be, and a caller who leads somebody may be, so for those two the
   login is looked up and the question decided again on the record it names;
   everybody else — somebody who leads nobody — is refused exactly as they
   would be on a seat they do not lead, before anything is looked up, so the
   refusal never says whether the login exists or whose seat it holds. A
   login nobody holds, or one whose holder is bound to a seat the chart no
   longer has, then names no record (`404`) for `admin` and is
   refused as below for a lead, who leads nobody by that name. A node that
   cannot read the directory answers `503` — to those two only, and without
   the directory's own words, which name the seat a login is bound to.
4. **Anybody else's handle** is decided by the authority table on the record
   it resolved to, each question asking its own verb. The first three are
   somebody's QUEUE, and take the owner-or-lead rule: whoever leads that
   person — so a lead may name their report by seat or by login — or a caller
   holding `admin`, each with `worklet`. A node that cannot read the chart to
   tell answers `503`, never a refusal — a lead told they lead nobody goes
   looking for an authority they already hold. `conversations` is a seat's
   TRAIL — what it said on a surface the engine does not own — and takes
   `audit.seat.read`, the rule `/agents/{id}/memory` takes: the seat's lead, or
   `admin`, with no app.

A refusal on authority is `403 unauthorized` naming the rule that decided in
`reason` and, in `grants`, the permissions any one of which would have
admitted the caller — `admin` for a colleague's queue or a seat's threads,
`worklet` for a lead who does not use it. An empty `grants` means no
permission would.

**A caller's own record is kept under ONE name**, and it is the name every
write they make is [attributed to](#who-a-write-is-attributed-to): the seat's
handle for a person — every person holds a seat — and for a token or service
account the identity directory binds to one, and the login — `svc:ci`,
`token:ops` — for one it binds to none. The tools write a
caller's inbox marks, pins, priorities and personal views under that name, and
every question here reads them back under it, so what a caller's assistant
arranged is what their screens show. The binding is the directory's: a
person's row names the seat they hold, and a Tier A token or a service account
acts as a seat when the directory binds its login to one — and what binding
changes is where the record is kept, not whether there is one.

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
| broadcast | `event`, `agents`, `seats`, `executors`, `tokens`, `budget`, `schedules`, `org`, `tools`, `health` | Every connected client whose posture takes the kind. |
| room | `inbox_changed`, `artifact_changed`, `artifact_data`, `artifact_presence` | Only the clients in the frame's room: a seat's room (the clients whose `watch` for that seat was allowed; the frame names the seat in its own `seat` field) or an artifact's room (the clients whose `artifact_join` was allowed; the payload names the artifact). |
| direct | `snapshot`, `result`, `error`, `pong` | The one client it answers. |

One keyed index holds both kinds of room. A room-routed frame that lost its
room is **dropped**, never fanned out: a result carries one client's
correlation id and a targeted frame carries one room's audience, so the unsafe
default in both cases is "everybody".

### Presence in an artifact's room

Presence is **told, never stored**: who is in a room and what they are looking
at lives in the memory of the node holding each socket and crosses between
nodes on an ephemeral broadcast, with no row, no log record and no coordination
key. Every frame between nodes is sealed under the fleet keyring
(`secrets.keys`) with a 60-second validity inside the seal, and a frame the
fleet did not seal shows nobody. Each node rebroadcasts its members every 15
seconds, a member not heard for 45 seconds is dropped, and a node gaining a
room's first socket asks its peers once so a newcomer sees the room at once.
Everybody who can open the artifact and has it open is in the roster; there is
no invisible mode. An agent reading or writing the artifact appears in the room
too, as an `agent`, for 45 seconds after its last call.

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
These are for a socket that is already open.

**An open socket is authenticated at its handshake and ended by what ends its
credential.** There is no periodic re-check. What can change the answer is an
event, and each kind is handled the cheapest correct way:

- **an identity record names it** — its session was signed out, ended by an
  administrator or replaced by a step-up; the person it acts for was suspended,
  removed, signed out everywhere, given other grants, bound to another seat or
  had a machine token revoked; or the directory row a Tier A token is bound
  through (`token:<id>`) moved. Every node serving the API holds the estate
  and applies the identity log, so every such node hears this from its own
  applier as the record applies; the sockets the
  record names — and only those — are decided again by the guard, over the
  request they were opened with, and a revocation reaches an open tab within
  the time it takes to apply;
- **an identity move names nobody** — the company's session generation moved
  (every session ended at once), this node **retained** a record instead of
  applying it (one a newer build wrote during a rolling upgrade, or one signed
  under a keyring key it was not restarted with, whose person is inside a
  payload it cannot read), a join replaced its identity estate, or its identity
  applier **stopped keeping up** (halted on a record it cannot read, or frozen
  behind a broker) and its lag has just passed the stall grace, the instant
  every identity read on the node starts answering `503`. **Every** open socket
  closes `1013` at once and nothing is read on its behalf: each tab reconnects
  on its backoff and its handshake decides — `401` sends it to sign in, `503`
  says this node cannot tell yet, and a credential the move did not touch is
  back on a fresh snapshot;
- **a company was published** — every configuration apply, a hire or any other
  edit of the org chart among them.
  Each socket is decided against the company just published, in memory, with
  no identity read: one whose person's seat that company no longer holds as a
  human seat closes `4403`, one whose seat now answers to another handle closes
  `1013` so its handshake resolves the binding afresh, and every `watch` is
  decided again;
- **its credential ends on its own** — a session's absolute deadline, a
  machine token's expiry; no record is written at that instant, so each socket
  keeps a timer for it, and the guard decides it;
- and **once as it starts listening**, so a record that landed between the
  handshake and that moment is not missed.

A session's **idle** deadline does not end an open socket: an open live view is
activity, and a socket can never be re-issued a cookie, so the session is
decided on what its records say and its absolute deadline. The browser's own
cookie is moved only by a REST response, so a client that talks to the node
over the socket alone makes an ordinary REST request now and then to keep its
session past the idle deadline.

Each answer does one thing:

| Answer | Code | What a client does |
|---|---|---|
| The session ended, expired or was revoked; the token is no longer accepted | `4401` | Re-dial with the credential the browser holds now; if the handshake answers `401`, sign in. |
| The person resolves but their seat is gone from the chart | `4403` | Stop reconnecting and show why: the credential is fine, what it may do is not. |
| This node will not vouch for the credential now: it could not read what decides it, an identity move named nobody, or the seat answers to another handle | `1013` | The standard's *try again later*: reconnect on a backoff. The handshake decides — and answers `503 identity_unavailable` with a `Retry-After` for as long as the node cannot say, before any snapshot is built. |
| Resolved as somebody else: renamed, bound to another seat or to none, given or refused a grant | `1013` | Reconnect, and ask who you are again (`viewer`): the handshake builds the snapshot for the new grants, and a screen that named the old login or seat — "your day" on a seat no longer yours — is redrawn as the new one. |
| Resolved as the same login, seat and grants | *(no close)* | Nothing — and later questions are asked as the principal just resolved. |

**A node behind its identity log** hears no record it has not applied, so
inside the stall grace its open sockets are served on their last decision, as
its REST routes are served on the rows it has. Past the grace it can vouch for
nobody, and every open socket closes `1013` at that instant (above) — including
on a node whose applier **halted** on the very record meant to end them, which
no applied record would ever have announced.

`4401` and `4403` sit in the 4000–4999 range the standard reserves for
applications, and both deliberately echo the HTTP status they mean; `1013` is
the standard's own, because what it asks of a client is what any close outside
that range asks.

Nothing else closes this socket for a fault.

### Wiring

Every node that serves the API feeds its projection through one entry
point, `stream.Service.Ingest`, from an **ephemeral broadcast
subscription** to the event stream (`observe.Projector`). It receives
every event from every node, because a dashboard served by one node must
show turns that ran on another, and that is why it is not a publish
listener: a listener sees only what its own node published. The event
store takes the other route, a publish listener inline on the publishing
node — or, for a node without `data`, the one data node that keeps the event
in [custody](../guides/deployment.md#custody-the-rows-of-a-node-without-data)
— so no two nodes keep one row. Each event updates the live-state projection
*and* fans out to connected dashboards. Backpressure is per-WebSocket: a
stalled tab drops the oldest queued envelope so it cannot stall the publish
path or other tabs.

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

The company's charter and its organization tree, as a reader holding no
permission at all may see it. The same object is the `org` section of the [handshake
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
  "services": {"tracker": "worklet", "knowledge": "confluence"},
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
          "avatar": {"character": "hexlet", "color": "cyan"},
          "llm": {
            "execute": ["fast", "backup"], "review": ["big", "fast"],
            "subagent": ["fast", "backup"], "auxiliary": ["cheap"], "judge": ["cheap"],
            "onboarding": ["fast", "backup"]
          },
          "tool_sources": ["builtin", "mcp:search", "mcp:github"]
        }
      ],
      "children": [
        {
          "id": "platform",
          "name": "Platform",
          "purpose": "...",
          "roles": [
            {
              "name": "Platform Engineer",
              "goal": "...",
              "avatar": {"character": "crewlet", "color": "purple"}
            }
          ]
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
these in TypeScript and had already diverged on three of them. It is on every
answer for a company, derived in the same call as the tree beside it, and
absent only from `{}`.

**From the company this node runs.** The whole answer — the `roles:` and
`units:` above it as well as `derived` — is cut from the organization of the
epoch this node applied, so it changes exactly when a revision is applied.

The fields above it stay as WRITTEN, so a reader can still tell a declared lead
from an inherited one. Every list here may arrive as `null` (Go marshals a nil
slice that way); a reader treats `null` as empty. The authored `path`,
`unit_path` and `placed_by_ref` of each entry are omitted, because they say
where a seat was WRITTEN — a fact about a document an ordinary reader is given
no way to point into. Membership is each unit's `seats`.

**What it carries, and nothing else.** The company's `name`, `mission`,
`vision`, `policies`, `timezone`, `token_budget`, `services` and `derived`; for each seat its `name`, `kind`, `handle`, `goal`,
`backstory`, `responsibilities`, `behavioral_guidelines`, `manages`,
`availability`, `token_budget`, `avatar`, `llm` and `tool_sources`; for each unit its `name`, `type`, `purpose`, `lead`, `goals`,
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

**`services` is which tracker, knowledge base and chat the company runs**,
as the engine resolves them, on every answer for a company and to every
reader: `tracker` is `worklet`, `jira` or `none`, `knowledge` is `doclet`,
`confluence` or `none`, and `chat` is `chatlet` (Crewlet's own chat), `vendor`
or `none`. A `backend` the document leaves empty is resolved here as the
engine resolves it: the vendor whose `integrations` block is connected, else
`worklet`, `doclet` and `chatlet` respectively. All three are closed sets. It
is what decides which of the dashboard's own surfaces exist: a company that
does not run Worklet has no Worklet section and no My work, one that does not
run Chatlet has no Chatlet, and every question about such a service is
refused there. It names the backend and nothing else of the `tracker:`,
`knowledge:` and `chat:` blocks, which stay guarded: no project or space key,
no site, no URL, no room policy, no credential. `chat` names a posture rather
than a product: which vendors a `vendor` company talks on is told to a holder
of `chatlet` as the [`viewer`](#queries) answer's `chat_vendors`. Absent only
from `{}`, so a reader never mistakes "no company yet" for `none`.

**`token_budget` is the ceilings as written**, on the company and on each seat
that names its own: one number per calendar window it caps (`day`, `week`,
`month`, on the company clock), and nothing for a window it leaves open — so
an absent key is "no ceiling", never zero. How much of each window is spent,
and when it resets, is [`GET /budgets`](#get-budgets); this is the rule those
meters count against.

**A seat's `avatar`, `llm` and `tool_sources` are RESOLVED**, for the reason
`derived` is: the rule is one a client would get wrong. All three are absent on
a human seat, which is drawn as the person it is and runs neither. `avatar` is
published to everybody, as `timezone` and `token_budget` are; `llm` and
`tool_sources` are published **only to a reader holding `admin`**: they
are computed from the seat's configuration, which no other reader is shown, so
a label derived from it follows the same rule.

- **`avatar`** is how the agent seat is drawn: its `character` and its
  `color`, each as the seat's own `avatar` block writes it and the default
  where it writes none (`crewlet`, the original, and `purple`), so every agent
  seat carries both and a seat that never chose is drawn as every agent was
  before a seat could. A value a newer engine admitted that this dashboard
  does not draw arrives as written; the dashboard draws that part as the
  default.

- **`llm`** is every phase's provider chain exactly as a turn resolves it —
  keyed by phase (`execute`, `review`, `subagent`, `auxiliary`, `judge`,
  `onboarding`), the first key the model that phase runs on and
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
(providers, MCP servers, integrations, the knowledge and tracker blocks beyond
the resolved `services`, notification and learning settings, worker
templates). Those are read through the
`admin`-gated `config` query or
[`GET /config`](#config--live-config-management-auth-gated), which masks
credentials. Schedules also have a read surface of their own, under the same
posture as `/org`, and the tree does not repeat them:
[`GET /schedules`](#routes) answers every configured schedule with its task
and next run.

**Why an explicit shape.** `/org` is readable by every caller — it is the
overview everybody signed in sees. Serialising the config's own
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
`list_project_files`, `read_project_file`, `write_project_file`, `remove_project_file`,
`task_activity`, `my_work`,
`list_pages`, `get_page`, `write_page`, `save_page`, `comment_on_page`,
`search_knowledge`; wherever the node takes artifact uploads,
`list_artifacts`, `get_artifact`, `list_dashboard_sources`,
`get_artifact_format`, `get_company_brand`, `create_artifact`,
`publish_artifact`, `update_artifact`,
`share_artifact`, `unshare_artifact`, `upload_artifact_asset`,
`create_artifact_folder`, `move_artifact`, `read_artifact_data`,
`query_artifact_data`,
`write_artifact_data` and `set_artifact_schema` (see
[Artifacts](../guides/tools-and-mcp.md#artifacts)), each artifact write
answering its `op_id` here as Worklet's do; and, where the company runs
Chatlet, `list_channels`, `read_channel`, `search_messages`,
`post_message`, `reply_in_thread`, `send_dm`, `react_to_message`,
`join_channel` and `leave_channel` — where `list_channels` also counts each
room's unread messages against the caller's own read state, which no seat's
turn has: each row carries `unread` (and `unread_capped` past the cap), a room
the caller muted carries `muted: true`, and a caller in do-not-disturb gets a
top-level `dnd_until`, as the rail shows them. When that read state cannot be
read it says so in a top-level `read_state: "unknown: …"` and puts no count on
any row, rather than counting from zero. A schema, a default, a trimmed field
and the wording of a refusal are each written once — two copies of "file an
item" drift on exactly the parts nobody looks at, and only one of the two is
ever tested.

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
board over `GET /worklet` is served an answer that includes it rather than
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
seat. A person — every person holds a human seat — acts **as their seat**,
and so does a token the identity directory binds to one: their writes are recorded under its handle with author kind `human`, and every
tool that asks who the caller **is** rather than who wrote it keys on it —
`mark_inbox`, `set_pins` and `set_priorities` write the seat's record;
`my_work`, `get_person` and `work_inbox` answer for it, and so does the viewer
`list_work_items` expands `preset=my_queue` and `preset=priorities` against; a
`watch: true`, a comment and a create record it as the watcher; a create with
no `project` files into its team's; and the lead relation `routing_unit` and
`write_project` are gated on is resolved for it. A credential nobody is bound
through — a Tier A token or a service account bound to no seat — writes under
its own login with author kind `operator`, and keeps its own record under that login,
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

Plus **twenty-four no seat is given**: `list_work_views`, `save_work_view`,
`write_work_catalogue`, `get_person`, `work_inbox`,
`mark_inbox`, `set_pins`, `set_priorities`,
`remove_work_item`, `restore_work_item`,
[`place_work_item`](#moving-a-card-on-a-board),
[`answer_executor`](#answering-a-parked-executor),
[`pause_seat` and `resume_seat`](#pausing-and-resuming-a-seat),
[`steer_turn`](#steering-a-running-turn),
[`answer_knowledge`](#answering-a-question-from-the-companys-knowledge), and
[the eight chat gestures only a person makes](#the-chat-gestures-only-a-person-makes). A view is furniture (a name, a shape
and a filter, arranged so a person finds the same question tomorrow), and a
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
anything: a removal is reversible at any age, and a purge (`crewlet worklet purge`, or `POST /worklet/items/{key}/purge`) is the
one that is not. A board's manual order is furniture too — where a card sits
says what a person wants looked at first — so dragging one is a person's; a
seat moves work between lanes with `update_work_item`. And an executor's question is a person's to answer: a seat
that could answer its own executor would be guessing on its own behalf. Whether a
seat works at all is a person's decision about it too: a seat that could pause
a colleague, or resume itself, would be overruling the people who run the
company. And a note to a running turn is a person redirecting the work: a seat
that could steer a colleague's turn would be directing it past the person who
asked for the work.

### The chat gestures only a person makes

Where the company runs Chatlet, the dashboard's chat screen makes every
change it makes through this catalogue (a person's own assistant reaches the
same tools), and eight of them are given to no seat:

| Tool | Arguments | What it does |
|---|---|---|
| `create_channel` | `channel` — the new room's name — and optionally `kind` (`public` or `private`; omitted takes `chat.chatlet.default_channel_private`), `topic`, `purpose`, `members` | Makes a named room, with the caller in it. The name is its address, so two people asking for one contend and one is refused `exists`. A unit's own room is the org chart's: the engine opens one for each unit that declares a `channel`, and a create of kind `unit` is refused `forbidden`. A room keeps what the company keeps; a horizon of its own is an administrator's to set. |
| `open_dm` | `to` — the other handles | Opens the conversation between the caller and them, or reaches the one already open, without saying anything; `created` says which. Its id is its participants, so opening it twice is one room. |
| `update_channel` | `channel`, and any of `topic`, `purpose`, `archived`, `auto_join`, and `template` beside a `topic` or `purpose` | Changes only what it names: an empty `topic` clears the topic, an absent one leaves it. A topic and a purpose are any member's, each at most 250 characters, and each change is a line in the room's transcript; `template` names the template they were set from (at most 64 characters), and the transcript records that one line instead. `auto_join` puts every seat the company takes on from then in the room, and is an administrator's, on a public room only (`chat.channel.autojoin`). `archived` closes the room to new messages (or, `false`, opens it again), and that is **not** any member's: it is decided on the stored room (`chat.channel.archive`), for its creator, the lead of the unit a unit's room belongs to or of a unit above it, or an administrator. An archived room is still read and searched. |
| `set_channel_members` | `channel`, `members` | States the room's WHOLE membership — whoever is left out is taken out, and an empty list empties it. Refused for a unit's room, whose membership is the org chart's, and for a direct conversation, whose id is its participants. |
| `add_channel_members` | `channel`, `members` (handles) | Puts the people and agents named in the room and takes nobody out, decided against the room as it stands when the write lands, so two people adding at once both land. `added` answers who it put there; anybody already in the room is left as they are. Refused for a unit's room and a direct conversation as `set_channel_members` is, and for a handle that is no seat of the company. |
| `edit_message` | `channel`, `message_id`, `body`, and optionally `links` | Rewrites a message. Only its author may. Whoever the new words name for the first time is told; nobody else is woken again. `links` changes only when sent: omitted, the message keeps the links it has, so fixing a typo needs only the new `body`; a list replaces them, and an empty list removes them. Each link is an absolute `http`, `https` or `mailto` address, stored as sent; any other is refused `invalid`, naming every entry that is not one (`links[1]`). |
| `delete_message` | `channel`, `message_id` | Takes a message back: its words go and its row stays, so a thread hung off it keeps its root. Its author may, and so may an administrator — moderation, decided before the message is read, so it reaches a message in any room, a private room or a direct conversation the administrator is not in included, without showing it to them. |
| `mark_chat_read` | any of `cursors` (room id → the position read through), `mute` and `unmute` (room ids), `dnd_until` (RFC 3339; empty ends it) | Records how far the caller has read, which rooms they mute or unmute and whether they are in do-not-disturb — the caller's own record, kept under their seat. A cursor only moves forward, and past 256 rooms the one read least recently is forgotten. A mute is a ROOM AT A TIME rather than a list: each named room is muted or unmuted and every other keeps its state, so muting one needs nothing read first and two screens toggling two rooms at once both land. A mute that would take the set past 256 rooms is refused `invalid` rather than cut, and so is one room both muted and unmuted in one call. The answer's `read` is the caller's whole read state as the write left it, named by its `handle`, and `read.revision` orders it against any other state of the same record. Not a record on the log — it is written to the coordination store and answers `applied` at no position. A flush whose write went out and whose answer was lost answers `unknown` (on `/operator/act`, the 503 with `outcome: unknown`): it may have landed, and sending the same call again is safe, because a cursor only moves forward and a mute, an unmute or a do-not-disturb sent twice is stated twice. |

`members` is a list of handles, or of `{handle, follow_all}` where the member
should hear everything said in the room. Each is a person's for its own
reason: a seat corrects what it said by replying, which the room sees arrive,
rather than rewriting a remark colleagues already acted on; a room, its
settings and its membership are the company's furniture and a private room's
readership; and a read cursor is a person's attention — a seat is woken rather
than reading, and has none.

**Every chat verb is made as a seat**, the reads included: a room renders a
speaker by seat, its membership is seats, and a private room's contents are
its members'. A person writes and reads as the human seat they hold, and a
credential acting as itself — a Tier A token, a service account bound to no
seat — is refused every one of them `403 unauthorized` with reason `unseated`
before anything is read; binding it to a human seat (`crewlet iam bind`) is
what admits it.

Every one of them answers `outcome` and `position` like every other write,
and is made once however often it is asked: through `/operator/act` the retry
of a gesture is the same request under the same `Idempotency-Key`, and every
operation it makes is derived from that key. The ones that state something —
the settings, the members, the words, the tombstone, the conversation — answer
an `unknown` by offering the same call as harmless, because stating it again
changes nothing where the first landed; `create_channel` says to look first,
since a second create of a name that landed is refused as taken.

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
the two, the lane change stands and the answer says `placed: false` with a
`lane_changed_not_placed` caveat, rather than failing a call whose status write
landed.
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

### Answering a parked executor

A seat's [executor](../concepts/code-sandbox.md#answering-a-parked-run) that stops
to ask a person something parks until it is answered. A reply on the
conversation it was asked in answers it — but an executor launched by a schedule, a
task assignment or a colleague's ask has **no conversation**, so `answer_executor`
answers any parked executor by naming its turn:

| Argument | |
|---|---|
| `turn_id` | The parked executor's `turn_id`, as [`GET /executors`](#get-executors) lists it. |
| `launch_id` | The question you are answering: the executor's `launch_id` as [`GET /executors`](#get-executors) listed it beside the question you read. Required. An executor asks each question under a new launch, so one that has moved on to another question since is refused `stale_version` and nothing is answered: read its question again and answer that one. |
| `choice` | The id of the option chosen, where the executor's question offers options (its `decision` in [`GET /executors`](#get-executors)). It must be one of them, and is refused `invalid`, naming the options, otherwise and on an open question. The executor reads it as the option's label and id: `Chosen option: <label> (`<id>`).`, followed by `answer` when both are given. |
| `answer` | What the harness should be told: the whole answer to an open question, or what to add to a `choice`. One of `choice` and `answer` is required. It carries the executor on: its harness is launched again in the box it kept, continuing its own session with the answer as its next message, where the relaunch reuses that box (the box was kept, the member picked runs the same harness in the same cell, and the box is the resuming node's to drive); otherwise afresh in a new box from the branch it pushed, told the question and the answer. Either way the answer is part of the executor's prompt from then on, which its harness sends again with every request it makes, so it is held to the page its question is: at most 16 KiB. Put anything longer on a page the executor can read and answer with where it is; a longer answer is refused `invalid`. |

It answers `{"turn_id", "agent_handle", "question", "launch_id", "answer_id",
"choice", "outcome": "pending"}` (`choice` only where one was given).
A model reading it (a seat's, or an assistant's over `/operator/mcp`) is handed
the question [fenced](../concepts/agent-runtime.md#text-somebody-else-wrote) as
`executor` under the seat whose executor asked it, at `external`, and the
`provenance` envelope beside it (the harness wrote it in the seat's name,
and the caller is often an assistant reading for the person answering; an
executor's ask is a harness's own words, read at `external` wherever a model reads it);
the act route answers a
person the question as the executor asked it. The answer is on the control topic of
the seat holding the executor, and **that** node records it on the executor's row
as soon as it takes it off (its `status` reads `answered`), then launches the
executor again with it as soon as the seat is free: its executor holding it for
no other turn, no other of its turns running, no person's pause. The resume is then the first
thing the seat does, ahead of its mail. The control topic rather than the
seat's inbox because the inbox is held while the seat's executor holds it for
any turn and is worked oldest first after that, and an answer that rode it
could wait out the executor's pause TTL behind them. The answer is to the
**question** the executor was waiting on when you gave it (`launch_id`), and it
resumes the executor only while it still waits on that one: if somebody else's
answer resumed it meanwhile and it has asked something new since, yours is
announced `not_awaiting` rather than taken as the answer to a question you
never saw. What it became is read off the executor's own row in
[`GET /executors`](#get-executors): `answered` or `resumed` on that
`launch_id` once it was taken, `answer_id` among its `declined_answers` if
every attempt to resume the executor with it failed and it was let go of (the
executor waits on its question again, and you answer it again), or a new
`launch_id` if the executor moved past the question first. It is also announced on the event stream
as `executor_answered` (`resumed`, `not_awaiting`, `gone`, `declined` or `unoffered`),
naming who answered: their name, its kind and the credential. A node that stops
on the way to the resume leaves the recorded answer for the seat's next holder
rather than losing it. An `unknown` outcome is a delivery the broker never
confirmed; answering again is harmless, because whichever copy arrives second
finds that question no longer waiting.

**Who may answer**: whoever asked for the work — the seat its requester holds — or whoever leads the executor's seat, or `admin`. Everybody else is refused `forbidden`, as the authority table refuses a lead's verb, because everybody signed in holds a credential and an answer steers somebody else's work.

It refuses `stale_version` for a `launch_id` that is not the launch the executor waits on now, the question you read having been answered and followed by another. It refuses `already_answered` for a question another answer was recorded against first (the first recorded answer wins), naming who gave it and when; the executor's row in `GET /executors` carries both as values (`answered_by`, `answered_at`). It refuses `not_running` for an executor that is not waiting for an answer, or has no
record at all (one that ended has none). It is
served on every company, whether it runs Worklet, Doclet and Chatlet or not: the executors' record is the
fleet's, and a company on Jira runs executors too.

### Pausing and resuming a seat

`pause_seat` stops an agent seat taking work until somebody resumes it:
it starts no new turn, its incoming mail waits on its inbox in order, and its
scheduled runs are recorded `skipped_paused` rather than sent. The turn it is on
finishes first, unless the pause asks to stop it — which also ends its
executor in its box, at once. `resume_seat` lifts the
pause, and what waited is delivered first. See
[Agent Runtime § Pausing a seat](../concepts/agent-runtime.md#pausing-a-seat).

| Tool | Arguments |
|---|---|
| `pause_seat` | `handle` — the agent seat; `reason` — one line, at most 500 characters, optional; `stop_running` — also end the seat's running work: the turn it is on at its next round, and its executor in its box at once, the box reclaimed (`executor_stopped`). What was stopped is not run again; an executor parked on a question stays parked. |
| `resume_seat` | `handle` |

**Who may**: the seat's holder, whoever leads it, or `admin` — never every reader, since everybody signed in holds a credential.

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
and `conflict` after losing four compare-and-sets in a row to other changes to
the same pause.

### Steering a running turn

`steer_turn` sends a short note to a turn **while it runs**. The turn reads it
at its next step — the next round of the engine's own loop, after the tool
call in flight returns, or, while the turn's executor is in its box, its
next launch's brief, a step where its harness takes notes, or the
review that judges its work — as a correction or addition to the work in hand,
and keeps to it for the rest of the turn: the reviewer that judges the work and
every later executor iteration open with it too. See [Turn Engine § Steering a
running turn](../concepts/turn-engine.md#steering-a-running-turn).

| Argument | |
|---|---|
| `turn_id` | The running turn's `turn_id`, as the `agents` push names it on each seat's `live_call`. |
| `note` | What the turn should take into account, at most 2,000 characters. Longer is a brief, and belongs on the work item. |

It answers `{"turn_id", "note_id", "agent_handle", "outcome": "pending", "reads"}`:
the turn took the note, and `reads` says when it reads it — `next_step`, or
`after_answer` for a turn whose executor is parked on a question, which reads
it once the question is answered. What became of it is recorded as
`agent_turn_steered` — `delivered` naming the phase and round that read it, the
kind of step (`boundary`) and, for a read by the executor in its box, its
launch (`launch_id`); or `expired` if the turn ended first. `note_id` is the request's
own id, so a retry of one request is one note however often it is sent.

**Where it goes**: a turn whose executor has launched in its box keeps its
notes on the executor's record in the coordination store, which every node reads, so
this node files the note there itself — sealed under the fleet keyring — and
asks nobody. A turn that has not launched one holds its notes on the node
running it, and the note is scattered to that node.

**Who may**: the turn's seat's holder, whoever leads that seat, or `admin`. Once the turn's run is out, the seat is the one the run's record names under a sealed anchor, read here; a record whose anchor does not open decides nothing, and the note is refused `unavailable` with no `Retry-After`, since the launch sealed the anchor once, the tool's sentence saying why. Before that the seat is the one thing this node cannot know — the turn runs wherever its seat is held — so `admin` is admitted at once and a caller who leads nobody is refused at once, and anybody else is decided after a PROBE asks the fleet whose turn it is, without offering the note. A probe nobody answers inside two seconds is `unavailable`: nothing was offered, so there is nothing to be uncertain about.

`unknown` means no node answered the OFFER inside two seconds. A reply lost on its way
back is indistinguishable from none, so the note may have been taken; sending it
again is safe for that reason.

It refuses `not_running` for a turn that has ended, `notes_full` for one already
holding five notes it has not read yet (once it reads them, the note may be sent
again), `notes_spent` for one that has taken the ten notes a turn takes (sending
it again changes nothing: put the rest on the work item), and `invalid` for an
empty or oversized note. Where the run's record could not be read or written,
the answer is what stopped it: `unavailable` for a condition of the node or the
fleet, such as a coordination store that did not answer, with a `Retry-After`
where waiting clears it, and `internal_error` for a fault of this node's own,
such as a record it cannot decode, which no wait clears. A filing the store did
not answer may have landed all the same: sent again under the same
`Idempotency-Key` it is the same note, so the turn never takes it twice.

The note is **signed**: a scattered request and the running node's answer are
signed under the fleet keyring and bound to that one request, and a note on a
run's record is sealed under the same keyring, bound to its turn and its id —
so nothing else reaches a turn
([ADR-0031](https://github.com/crewlet/crewlet/blob/main/adr/0031-every-state-log-record-is-signed.md),
[ADR-0067](https://github.com/crewlet/crewlet/blob/main/adr/0067-a-note-to-an-executor-waits-sealed-on-its-record.md)).
An answer the asking node cannot believe is no answer, so it reads as
`unknown`. A running node that does not hold the key the note is signed under
takes nothing and says so, and a run's record whose seat is sealed under a key
the asking node does not hold decides nothing: the tool refuses either
`unavailable` with no `Retry-After`. Its sentence, which your assistant reads
over `/operator/mcp`, says that an administrator needs to give the nodes the
same keys; over `/operator/act` the refusal carries no `detail` (ADR-0060), so
the person steering is answered in the code's own `message` for a refusal no
wait clears. The keys the running node holds are in its log
(`steer_request_refused`, `held_keys`) and the asking node's
(`steer_turn_unverified`, which names the node running the turn as
`running_node` beside its `held_keys`), and the keys an asking node holds that
do not open a record's seat in its own (`steer_seat_key_unknown`): bring
`secrets.keys` into agreement across the fleet, and send the note again.

### Answering a question from the company's knowledge

`answer_knowledge` answers a person's question — the dashboard's ⌘K answer —
from what the company has written down: it searches the knowledge base
(`hybrid`, auto-drafts hidden) for five pages and Worklet for three
items, reads each WHOLE where this node holds it (a Doclet page's body, an
item's description; an external wiki's search snippet, said to be one),
condenses any source past 4 KiB for the question with the same auxiliary
model — never cut — and asks one model to answer from those sources alone,
citing each claim as `[n]`. A source that cannot be condensed is dropped from
the answer and from its `sources` alike, so nothing is cited that the model
was not shown.
See [Knowledge System § Answering a question](../concepts/knowledge-system.md#answering-a-question).

| Argument | |
|---|---|
| `q` | The question, in plain words; at most 400 bytes — a longer one is refused, never cut. |

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

**Only a caller who holds a seat may ask.** It takes either app — it searches
the pages only for a `doclet` holder and the board only for a `worklet`
holder, and keeps the two answers apart in its cache, so an answer one reader
was given is never served to a reader holding the other — and it runs on
the auxiliary model of the seat the identity directory binds the caller to —
every person's, and a token's bound to one — so a principal no seat binds, a
pipeline's token or a service account bound to none, is refused `403
unauthorized` with the reason `unseated` and no permission named, on every
transport, rather than answered on the company's default model, because spend
has to be attributable to somebody; `viewer.acts` does not offer it such a
caller either. No seat is given it: a seat has `search_knowledge` and a model of
its own.

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
and is served even while the budget is spent. A company that does not run
Doclet has no position to key on, so its answers are never cached.

It refuses `invalid` for an empty question, `forbidden` for a caller no seat
binds or a seat the chart no longer has, `budget_exhausted` as above (with
`detail` in a person's words and `remedy: {"setting": "company_token_budget"}`),
and
`unavailable` for a counter the coordination store did not answer for (nothing
is spent, and a counter this node cannot read for a fault of its own is
`internal_error`, which no wait clears), a knowledge base
and tracker that could not be searched at all, no model configured, or a model
that failed or wrote nothing — a reply that spent tokens and wrote nothing is
still charged. Over `/operator/act` an `unavailable` other than a usage
limit's (below) carries no `detail` (ADR-0060), so the person who asked reads
its code's own sentence; the tool's words for which one it was are a model's,
over `/operator/mcp`. One that no
wait clears carries no
`Retry-After`: no model set up, a login the model's harness reports refused or
missing, or a call this node can't make at all (a harness it can't start, a
prompt it can't render). A model at a usage limit its harness reported is a wait
rather than a failure: nothing is launched, and the refusal carries the
seconds until the model is tried again as its `Retry-After`, a person's words
for the limit as `detail`, and `remedy: {"setting": "model_providers"}`, the
company's models, where an administrator gives it a fallback
([ADR-0064](https://github.com/crewlet/crewlet/blob/main/adr/0064-a-model-at-its-limit-parks-the-seats-it-holds-and-a-seat-says-one-steady-thing.md)).
A slow or failing model, and the rest, carry one.

### One catalogue, and how a retry is the same call

Every operator transport reaches a tool through **one dispatch** over one
catalogue, built per request from this node's Worklet, Doclet and Chatlet halves, so a verb, its
schema, its hints and the wording of its refusals cannot differ between the
ways a person reaches it, and a node handed its first company serves its tools
on the next request. The hints each tool is listed with (read-only,
destructive, idempotent, open-world) are the catalogue's own, and a verb this
company is not served is not listed at all.

An MCP call names no request of its own, so each one is a new operation and a
Worklet write answers the `op_id` it was: sent back with exactly the same
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

Each tool appears only where the company runs its half (Worklet, Doclet or Chatlet):
a company on
`tracker.backend: jira` gets the page tools and not the work tools, one off
Chatlet gets no chat tool, and one native in none of the three gets only
the verbs about the fleet's own records — `answer_executor`, `pause_seat`,
`resume_seat` and `steer_turn` — which every node serves whatever the company
runs, so the catalogue is never empty. It is read per request, so
a node that has not been handed a company yet answers `503 no_active_revision`
with a 15-second `Retry-After`, and serves the tools its first revision brings
up with no restart. `search_knowledge` and `answer_knowledge` are the exception and
are offered against **any** knowledge backend, Confluence included — a ranked
search over the company's own wiki, or an answer drawn from it, is exactly as
useful to an assistant there.

The turn-only tools are deliberately absent: the memory tools (a diary belongs
to a seat), the skill tools (a skill is loaded into a phase) and `a2a_ask` (a
colleague's answer comes back by waking a seat, and there is nobody here for
it to reach). Answering a run a seat started is `answer_executor`.

### Seeding a knowledge base with it

On Doclet this endpoint is also how a directory of version-
controlled markdown gets published — there is no import CLI for it and there
does not need to be one:

> Publish everything under `examples/nimbus-docs/` — one container per
> directory, the page title from each file's first `# H1`.

The assistant calls `write_page` per file. That handles what a flag-driven CLI
handles badly: the parent chain, a title that already exists (`save_page` with
the version it read), and a file that turns out to be a tool skill rather than
prose. The [reserved containers](../concepts/knowledge-system.md#who-may-write-where)
are refused to a seat and not to it: a person publishing the onboarding tree
and the tool skills is what they are for. A tool skill takes `admin` rather
than `doclet`, though (it is injected into every seat's turn, so writing
one is a configuration change), and a token without it is refused
`pages.skill.write`.

### Who a write is attributed to

The **person or credential the request resolved to**, converted once for every
surface that has a request rather than a turn — this one and the
[write surface](#the-human-write-surface) below:

| Who is calling | Recorded as | Author kind | `operator_id` |
|---|---|---|---|
| A person signed in at a browser — every person holds a seat | the **seat's handle** | `human` | **`session:<lineage>`** |
| A person's own [machine token](#post-iamcredentials-mints-a-machine-token) | the **owner**, as above — their seat | `human` | **`pat:<credential id>`** |
| A service account's machine token | the account's **login** (`svc:ci`), or its seat when bound | `human` when bound, `operator` otherwise | **`pat:<credential id>`** |
| A Tier A token | its **whole login** (`token:ops`), or its seat when the directory binds it | `human` when bound, `operator` otherwise | its login (`token:ops`) |
| A Tier A token's exchanged session ([`POST /auth/token`](#routes)) | as the token, above | as the token | **`session:<lineage>`** |

The seat half is what lets Worklet leave you out of the wake your own
change sends: nobody is woken about what they just did, and Worklet
recognises the author by the name the record carries, so a person recorded
under a token's id was woken about every edit they made. The login half keeps
the colon: `token:ops` can never be a seat's handle, where the bare `ops` could
be, and an own-record rule comparing names would then admit a credential into
the record of the seat that shares its spelling.

**Chat has no login arm.** A room renders a speaker by seat and a private
room's contents are decided by its membership, so a credential acting as itself
is refused every chat tool, reads included, `403 unauthorized` with reason
`unseated` — whatever it holds, `admin` included. The one exception is the three
[destructions](#a-chat-erase-prune-or-horizon), which are said in nobody's
voice and recorded by this table like any other write.

**The credential is recorded again beside the author**, as `operator_id` on
every work, page and chat record and every configuration revision, so an audit can ask what one credential did
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

- **A permission withdrawn is withdrawn from the next call.** Restarting a
  node with `api.auth.admin: refuse`, editing a person's permissions, revoking
  their session or
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
and every tool is decided by the [authority table](#which-permission-a-route-needs)
exactly as on every other surface — a person signed in writes as the seat
they hold, and a credential nobody is bound through — a Tier A token or a
service account bound to no seat — writes under its own login. Nobody is
refused for being unbound: a write
attributed to a credential acting as itself is one an audit can still ask
about, and the table already says what each one may do. `GET /viewer`'s `acts`
names what this route would serve the caller, so a screen disables a control
with the reason rather than offering a press the engine refuses.

**Who the write is attributed to** is [the one rule](#who-a-write-is-attributed-to)
every surface with a request applies, so a write from the dashboard and one
from the same person's assistant read identically in the audit and in every
thread. The principal is resolved **once per call**, when the call is
admitted, and the write and its audit record both name that one answer: a
directory rebind or a revision applied while the call runs applies from the
next call on, and never admits a call as one person and makes it as another.

**The body is JSON and nothing else**: `{"args": {…}}`, declared
`Content-Type: application/json` (UTF-8). A form post, `text/plain` or no
content type is `415` before anything is read — a cross-site form can send
those without a preflight — and a key the envelope does not take is refused
`400 invalid_body` rather than ignored. `args` is the tool's own arguments,
exactly as the tool's schema names them, and its numbers reach the tool
exactly as written — an integer past 2^53 is never rounded on the way in, as
it is not from a seat or over `/operator/mcp`. The body is capped at 1 114 112
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
comment's, a chat message's — so a retry is the first attempt's operations
rather than new ones, and two different calls under one key are still two; one
whose operation now meets another object is `409 invalid_input`, to be sent
under a new key. For
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
             "version": 4711, "position": "CREWLET_TRACKER_LOG@1:4711"},
 "caveats": [{"code": "date_truncated", "action": "none",
              "message": "Launch holds a date without a time, so it was saved as 2026-03-04, the day 2026-03-05T04:00:00Z falls on in America/Los_Angeles."}]}
```

`outcome` is the write's three-valued answer and `position` where its record
landed — the value a read hands back as `min_position` so the answer after the
write includes it. `pending` is durable and not yet applied here; `unknown`
means the broker never said, and the only safe retry is the same request under
the same key. A write that appended nothing answers `applied` at a `null`
position: the state asked for already holds. `op_id` is the operation the
request was made under (the key as **this principal's** operation, named by a
tag of their id, so sending it back as the header is the same operation as
sending the original key). `receipt` is the tool's own answer, verbatim except
for its `warnings`, with what somebody else wrote in it as it was stored: a
room's topic and purpose, a page's title, a parked run's question, which the
same tool answers a model fenced and labelled (see
[Text somebody else wrote](../concepts/agent-runtime.md#text-somebody-else-wrote)).

`caveats` is what the write tells you beside its outcome, and was not refused
for: an item filed to triage because the project's default assignee has left
the org chart, a timestamp stored as the day it falls on, a description naming
more items than are linked. It is always a list, empty when there is nothing
to add, and each entry is written for a **person**:

- **`code`**: the kind of caveat, stable to branch on (`unaddressed_comment`,
  `default_assignee_gone`, `body_links_capped`, `date_truncated`,
  `tag_near_duplicate`, `filed_earlier_unread`, `filed_earlier_removed`,
  `lane_changed_not_placed`, `dependencies_not_recorded`, `move_unfinished`,
  `move_waiting`, `subtree_unfinished`, `datastore_pending`, `file_finding`).
  A write that landed part of what it was asked (an item filed whose
  dependencies were not recorded, a move or a removal whose walk stopped) says
  so here, and never in a field of the receipt: an agent reads the same in its
  own words under that field, which this route takes off.
- **`message`**: one or two sentences in the words of the screen, naming no
  tool, argument or internal id.
- **`action`**: `required` when something stays as it is until somebody acts
  (a setting that no longer works), `none` when it is only something to know.
  The dashboard keeps a `required` caveat on screen until it is dismissed and
  lets a `none` one go by itself.

The same tool called by an agent answers the same caveats under `warnings`,
in words for a model that name the tool and argument it would use next. This
route answers people, so it answers `caveats` and takes `warnings` off the
receipt with every other key written for a model: a partial outcome's
sentence, and the `provenance` envelope a fenced answer carries beside the
words it labels (ADR-0060).

A tool that appends more than one record — an item filed or edited together
with a dependency, a page saved and renamed in one call, a project's tags and
settings, the catalogue's types and fields — answers for all of them: the
**least certain** outcome (`unknown` over `pending` over `applied`) at the
**latest** position, and for a work item the version it is at after the
last of them. So `min_position` never floors a read below a record the call
made, and a write with one unconfirmed record is never reported `applied`.
A later record refused after an earlier one landed answers the later
record's refusal, and its `detail` says what did land — with that record's
outcome and position — because the earlier change is on every data node and "not
made" would send a person to redo it.

A refusal is the engine's envelope, `{error, message}` with `tool` and the
object the arguments named (`item`, `page`, `container` for a page not written
yet, `project`, `handle`) beside them, so a refusal or an unknown outcome says
which object to read. **Every sentence on it is written for a person**
(ADR-0060). `message` is the code's own sentence. `detail`, where present, is
the refusal in a person's words, naming what to change or whom to ask; a
refusal nobody has worded for a person yet carries **no** `detail`, and its
`message` stands alone. The sentence the same tool answers a model with
(naming the tool, its arguments and what to call next) is never on this
answer. The authority table's refusal adds the rule's `reason` and the
`grants` that would have admitted the caller; an argument the tool does not
read is named in `fields`; and the route's own refusals below carry the code
and `tool` alone. **The status is read from the refusal's CLASS, never from
its sentence**, through the one mapping the [human write
surface](#the-human-write-surface) answers with too. The transport's own codes:

| `error` | Status | When |
|---|---|---|
| `invalid_token` | `401` | Nobody presented a credential (the guard's own refusal) |
| `identity_unavailable` | `503` | This node cannot read its identity estate, with a `Retry-After` |
| `seat_unavailable`, `second_factor_enrolment_required` | `403` | The guard's refusals of somebody it resolved — see [Auth](#routes) |
| `reader_changed` | `409` | The request names, in `X-Crewlet-Reader`, a principal the credential is not — see [Auth](#routes); nothing ran, so a retry under the same key is no write of the new principal's |
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
| `starting` | `503` | This node is [still starting](#while-a-node-is-starting): its listener is bound and the surface that serves this route is not up yet; `Retry-After: 10` |

And the tool's own refusal, carrying its person's words as `detail` where it
has them, never the tool's sentence, which is written for a model and names
the tool to call next. A `conflict` or an `exists` a native domain decided (a
room archived, an item in the trash, a label another tag carries, a write that
lost its race), a write about something deleted, and an `unavailable` about a
condition of the node or the fleet (a node behind its log, a full log, a store
that didn't answer, a note the node running a turn couldn't verify, or an
executor's record whose seat this node couldn't open, because the fleet's nodes don't
hold the same keys) have none yet, so each answers its code's own `message`
alone, the condition's in step with the answer's `Retry-After`.

The dashboard shows a refusal's `detail` in place of its own sentence for the
code. Its class decides its status:

| `error` | Status |
|---|---|
| `invalid` | `422` |
| `not_found` | `404` |
| `forbidden` | `403` |
| `stale_version`, `conflict`, `exists`, `already_answered`, `reassignment_budget`, `inbox_full`, `quota_exceeded`, `notes_full`, `notes_spent`, `not_running`, `budget_exhausted` | `409` |
| `unavailable` | `503` |
| `internal_error` | `500` — a fault of the node's own store or code: a fixed sentence, the error itself only in the node's log, and **no** `Retry-After`, since no wait clears it. It is also the answer for a failure that carried no class |

#### Which class a refusal is

One rule, for every domain — the tracker, the knowledge base, chat, the
artifacts and their datastores — and every surface that answers one, a seat's
tool, your assistant and this route alike. A refusal is classed by **what it
is about** and **what would make the same request land**:

- **The request itself** is `invalid` (`422`): an argument outside its
  grammar, a value past a limit of its own (a message's bytes, the members one
  call names, the views one gesture pins), or an argument naming *another*
  object that cannot take part — a parent in the trash, a blocker at its
  dependents' cap, an archived task type, a destination project that is
  archived. Nothing anybody does makes the same request land; another one does.
- **A collision** is `exists` (`409`): a name, a title, a label, an address or
  an id the call gives an object is another object's already — a page title in
  its container, a room's name, a tag label another tag carries, a label
  another version of an artifact holds, a source address another of the
  owner's artifacts holds — and so is a create of an object that is already
  there. The request is well formed, and the object it is about refuses
  nothing by its state: another value lands now, the same one once its holder
  gives it up, and the `detail` names the holder, because using it is often
  what you meant.
- **The object the call is about** — or the container it writes into, a room,
  a project, an artifact — **not being there** is `not_found` (`404`), and
  gone for good is the same.
- **That object in a state that refuses the gesture until somebody changes
  it** is `conflict` (`409`): a room or a project archived, an item frozen in
  the trash, an artifact in the trash or being purged, a restore with nothing
  in the trash, and the object's own contents at their cap — a message
  carrying all 32 distinct emoji it may, a project keeping all its tags, a
  task's watchers or checklists, a person's pins, every label an artifact
  keeps in use. Nothing about the
  request is wrong: read the object again, and
  the very same request lands once whoever the `detail` names reopens,
  restores or makes room — not before. A write that lost its race is `conflict`
  for the same reason.
- **A limit with a class of its own** is that class, because what lifts it is
  not a change of the object and a surface names the remedy:
  `quota_exceeded` (the company's plan), `inbox_full` (a person's marks),
  `reassignment_budget` (an item's hand-offs, which must not invite another
  attempt), `notes_full` (the notes a running turn holds unread),
  `notes_spent` (the notes a turn takes, which must not invite one either)
  and `budget_exhausted` (a token window).

Every `503` carries a `Retry-After` where waiting can clear it and **none**
where it cannot — an evicted node, a full log, a record this node cannot
decode: ask another node, or an operator. A call interrupted before the tool
answered is `503` `unavailable`, never a refusal: whether it landed is
unknown, so the answer carries **`outcome: "unknown"`** and the `op_id`, and
says to send it again with the same key. A tool that made its write and could
not confirm it — the broker's acknowledgement was lost — is answered the same
way, naming the object to read; and one this node's
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
circulation — and the three things an administrator does to its chat that no
tool does, as HTTP routes, under the name of whoever the request resolved
to. They are the **same tools** a seat holds in its turn and your own assistant
holds over [`/operator/mcp`](#operatormcp--your-own-assistant): a route's body
is the tool's own arguments, by the tool's own names, and its answer is the
tool's own receipt. One implementation of "file an item" rather than three,
because copies drift on exactly the parts nobody looks at — which field is
trimmed, which default applies, what a refusal says.

A body is exactly **one JSON object** — a second value after it is `400
invalid_body` — and its numbers reach the tool exactly as written, as a seat's
and your assistant's do: an integer past 2^53 (a vendor's 19-digit id, a
whole-number custom-field value) is never rounded on the way in. Where a route
takes a version, an `If-Match` header supplies it as the number you read, and
one that names no version (`"three"`, a negative, or a number of 2^63 or more)
is `400 invalid_body`; a body that names the same argument must name the same
version as the tool reads it (`3`, `3.0` and `"3"` are one version), and
anything else — another version, a fraction, a negative, text that spells no
whole number — is `400 invalid_body` rather than one of the two chosen for you.

Every route is **guarded**, reads and writes alike, and the half the company
does not run is **absent** rather than refusing: a company on
`tracker.backend: jira` gets `404 no_route` from the work routes, and one on a
vendor's chat from the chat routes, not a `503` that reads as an outage. The routes are mounted on **every** node and read the
halves each request finds, so a node that has not been handed a company yet
answers them `503 no_active_revision` with a 15-second `Retry-After`: Worklet,
Doclet and Chatlet come up with its first revision, under the same
API, and the same request is served from then on with no restart.

### The routes, and the authority each takes

The authority is the [authority table's](#which-permission-a-route-needs), decided
ONCE. Where the object is in the path — a project — the route decides the verb
itself. Where it needs a stored row — which project an item is filed under,
which space a page is in, who wrote a comment, whose record a person's name is
(a login is its holder's, which only the identity directory can say) — the
route admits on the weakest honest precondition and the verb is decided once
the row is read, by the tool's own ask or, for a verb with no tool, by the
route.

| Method | Path | Does | Decided by |
|---|---|---|---|
| `POST` | `/worklet/items` | `create_work_item` | `worklet` |
| `PATCH` | `/worklet/items/{key}` | `update_work_item`. `If-Match` is its `if_match`, and an item somebody changed since is `412 version_moved` | `worklet`; re-routing asks the project's lead |
| `POST` | `/worklet/items/{key}/comments` | `comment_on_work_item` | the route admits everybody (`work.thread`) and the tool decides once the item is read: `worklet`, or (for **the answer to an open ask that names the caller**, and nothing else) the person asked, with no app ([You may answer what you were asked](../concepts/identity-and-access.md#what-each-app-opens)) |
| `PATCH` | `/worklet/items/{key}/comments/{cid}` | Rewrite a remark's words: `{"body": …}`. Only the words change — what the remark asked, the answer that closed it and whom it woke are kept, so an answered question is not reopened | its **author**, with `worklet`: the table admits `admin` too, and Worklet then refuses anybody but the author, because a remark somebody else can rewrite is one attributed to a person who did not make it |
| `POST` | `/worklet/items/{key}/rank` | `place_work_item`, a board drag: `{"after": ref}` or `{"before": ref}` (one neighbour, never both) and the `status` lane it was dropped into. `If-Match` is its required `if_match`, and a move of an item somebody changed since is `412 version_moved` | `worklet`, and **never a seat** |
| `POST` | `/worklet/items/{key}/depend` | `update_work_item`'s `waiting_on`, `blocking` and `dependency_note`, and nothing else; `If-Match` is its `if_match` | `worklet` |
| `POST` | `/worklet/items/{key}/relate` | `update_work_item`'s `linked` and `linked_pages`, and nothing else; `If-Match` is its `if_match` | `worklet` |
| `DELETE` | `/worklet/items/{key}` | `remove_work_item` (`{"subtree": true}` takes its children) | the item's **reporter**, the lead of the item's **own** project, or `admin`, with `worklet`; `subtree` is the lead's or `admin`'s alone |
| `POST` | `/worklet/items/{key}/restore` | `restore_work_item` | the same |
| `POST` | `/worklet/items/{key}/move` | `move_work_item`: `{"project": "OPS"}` moves the item and everything under it. A move whose walk stopped after the item itself moved answers `200` with the item's receipt, `subtree_followed` of `subtree_total`, and a `move_stopped` instruction: the **same** `Idempotency-Key` finishes it (the answer's `op_id`, sent back as that header, which is the key even when the request sent none), and a new one is refused as somebody else's move | the lead of the item's **own** project, or `admin`, with `worklet` |
| `POST` | `/worklet/items/{key}/purge` | Destroy it and every row it produced; see [below](#a-purge) | `admin`, a recent proof (`step_up`), and **never a seat** |
| `PUT` | `/worklet/projects/{key}` | `write_project`'s policy: `fields`, `default_assignee`, `archived` | the project's lead or `admin`, with `worklet`; archiving never a seat |
| `POST` | `/worklet/projects/{key}/tags` | `write_project`'s `tags_add`, `tags_rename`, `tags_archive` | declaring: `worklet`; renaming and archiving: the project's lead, with `worklet` |
| `POST` | `/worklet/views` | `save_work_view` | `{"personal": true}` makes it the CALLER's own view (its owner is always the caller's [own record](#whose-record-a-personal-question-answers-for), and there is no naming somebody else's; a shared one is its container's lead's), and a save naming a stored view's `id` also needs that authority over the view it replaces. An `id` must name a stored view — one no view holds is `404 not_found` — and a new view is saved with none, its id minted and answered. With `worklet`. Only a shared view can be `default` |
| `PUT` | `/worklet/catalogue` | `write_work_catalogue` | `admin` |
| `PUT` | `/worklet/people/{handle}/inbox` | `mark_inbox`; see [below](#somebody-elses-inbox) | the person, or `admin`, with `worklet` |
| `PUT` | `/worklet/people/{handle}/pins` | `set_pins` | the person, or `admin`, with `worklet` |
| `PUT` | `/worklet/people/{handle}/priorities` | `set_priorities` (`{"items": [...]}`) | the person, whoever leads them, or `admin`, with `worklet`; never a seat reordering a colleague |
| `POST` | `/doclet/pages` | `write_page` | `doclet`, and only in a space the company declares ([Who may write where](../concepts/knowledge-system.md#who-may-write-where)) |
| `PUT` | `/doclet/pages/{id}` | `save_page`. `If-Match` is its required `base_version`, and a page saved since is `412 version_moved` | `doclet`; a `title` in the body is a rename, and asks what a rename asks |
| `POST` | `/doclet/pages/{id}/rename` | Move the page to a new title (its address): `{"title": …, "quiet": false}` | the page's **creator**, the lead of the page's container, or `admin`, with `doclet` |
| `POST` | `/doclet/pages/{id}/comments` | `comment_on_page` | `doclet` |
| `PATCH` | `/doclet/pages/{id}/comments/{cid}` | Rewrite a remark: `{"body": …}` | its **author**, with `doclet`, for the work comment's reason |
| `DELETE` | `/doclet/pages/{id}/comments/{cid}` | Take a remark down | its author, or `admin` as a moderator, with `doclet` |
| `DELETE` | `/doclet/pages/{id}` | Put the page in the trash | the page's **creator**, the lead of the page's container, or `admin`, with `doclet` |
| `POST` | `/doclet/pages/{id}/restore` | Take it back | the same |
| `POST` | `/doclet/pages/{id}/purge` | Destroy it: `?confirm=` repeats its **title**, `?reason=` is required | `admin`, a recent proof (`step_up`), and **never a seat** |
| `POST` | `/chatlet/channels/{channel_id}/erase` | Destroy named messages of a room: `{"message_ids": […], "reason": "…"}`, with `?confirm=` repeating how many — see [below](#a-chat-erase-prune-or-horizon) | `admin`, a recent proof (`step_up`), and **never a seat** — a credential acting as itself included |
| `POST` | `/chatlet/channels/{channel_id}/prune` | Destroy everything a room said before `?cutoff=` (RFC 3339), with `?confirm=` repeating it | the same |
| `PUT` | `/chatlet/channels/{channel_id}/retention` | Set a room's own horizon: `{"retention_days": n}`, `0` keeping it for ever, or `null` for the company's, with `?confirm=` repeating it — the days, or `company` | the same |

**A page in the tool-skills container asks one question more.** Creating,
saving, renaming, trashing, restoring or purging one is decided as
`pages.skill.write` on top of the rule in the table — **`admin`**, and
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

**Not here, and deliberately:** sprints and goals. Worklet has neither,
so there is nothing to route.

### What a request answers

Through the **one mapping** the act route answers with, read from the
refusal's class and cause, never from its sentence, so a status and a code
mean the same thing on every write surface. As there, a refusal's `detail` is
written for a person, and is present only where the refusal has been worded
for one; otherwise its `message`, the code's own sentence, stands alone
(ADR-0060):

| Status | `error` | When | Carries |
|---|---|---|---|
| `200` | | The write was **applied** on this node, so the next read here sees it | the tool's own receipt without its `warnings` (written for a model), its `outcome`, `position` and `op_id`, and its `caveats` in a person's words, always a list |
| `202` | | The write is **pending**: durable, and not yet applied on this node | the same, and the `position` to read at |
| `400` | `invalid_body` | The body is malformed, names a different object than the path, or carries an argument the tool does not declare | the undeclared arguments in `fields` |
| `401` | `invalid_token` | Nobody presented a credential | |
| `403` | `unauthorized` / `step_up_required` | The authority table refused, or the verb needs a fresher proof | `reason` and `grants` (or the window), and the verb in `action` where the route decided it itself |
| `403` | `forbidden` | The domain forbids this caller the gesture, such as rewriting somebody else's remark | `detail` in a person's words, where the refusal has them |
| `404` | `not_found` | The item, page or comment does not exist — or it was purged | |
| `409` | `stale_version`, `conflict`, `exists`, `reassignment_budget`, `inbox_full`, `notes_full`, `notes_spent`, … | Somebody changed it after you read it (a stale `if_match` or `base_version` in the body, a race lost), or a name it gives is another object's already (a title, a room's name, a tag's label: `exists`), or the object is in a state that refuses it: archived, frozen in the trash, its contents at their cap ([which class a refusal is](#which-class-a-refusal-is)) | `message`: read it again, or change what is in the way; and `detail` in a person's words, where the refusal has them (a `conflict` or an `exists` a native domain decided has none yet) |
| `412` | `version_moved` | The version the request's `If-Match` header named is not the one it holds: the same refusal as a stale `if_match` in the body, answered as HTTP answers a precondition that failed (RFC 9110 §13.1.1), as a project file's and an artifact's are | `message`: read it again and decide again |
| `409` | `invalid_input` | The `Idempotency-Key` already names a write to another object | send it under a new key |
| `409` | `reader_changed` | The request names, in `X-Crewlet-Reader`, a principal its credential is not, and the guard refused it before it ran | nothing; ask `GET /auth/session` who the credential is — see [Auth](#routes) |
| `422` | `invalid` | The domain refused the write on its own rules, such as a purge whose `?confirm=` names another item, an erase whose count or a prune whose cutoff it does not repeat, among them | `detail` in a person's words, where the refusal has them |
| `500` | `internal_error` | The engine failed at something of its own, or a tool answered something that is not a receipt | nothing a retry changes; the reason, and anything the tool said instead, is in the node's log |
| `503` | `unavailable`, `no_active_revision`, `identity_unavailable` | This node could not decide (it cannot tell who you are, or cannot read the chart yet), cannot establish whether the write landed, or its log refused the request | a `Retry-After`, and — for an unknown outcome — `"outcome": "unknown"` and the `op_id` to retry with, on every route, a tool-backed one included. A refusal waiting cannot clear on this node — it was evicted, it holds a record it cannot decode, the log is at its ceiling — carries **no** `Retry-After`: ask another node, or an operator. A gate's refusal of **another node's copy** of the write (`evicted`, `abandoned` or `overtaken`, which the node that refused finishes itself once the log's duplicate window lets go of the operation) carries that window as its `Retry-After` |

The `unauthorized` row is one DECISION on all three surfaces that serve these
verbs: a seat and your assistant read it in one sentence, formed by one
function, and this surface answers the same rule's `reason` and `grants`, so a
person told one thing by the dashboard and another by their assistant is never
left asking which is wrong.

### A retry is the same operation

Send an `Idempotency-Key` header and every record the request writes derives
its operation id from it; send none and a fresh key is minted and handed back
as `op_id`. An `unknown` outcome is the one to retry — **with the same key**,
sent back as `Idempotency-Key`, because a fresh one would defeat the ledger
that makes a retry safe and a retried create would file the item twice.

Every operation is bound to what its write SAYS as well as to the key: the
same key sent with another request — a page saved with another body, renamed
to another title, the same words posted in another comment thread, a remark
rewritten again — is another operation and lands as asked, rather than being
answered from the ledger as the first one with nothing of it written. The same
request again is the same operation, which is what makes it a retry; what the
engine reads off the words for you, such as who a comment mentions, is not
part of it.

### A purge

`POST /worklet/items/{key}/purge?confirm=<KEY>&reason=<why>` destroys an item and
every row it produced, on every data node, and nothing undoes it. It replaced
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
is the whole account of what used to be at that key. It is carried **whole** on
the one line the project's lead is told, so a reason that would not fit that
600-byte line is refused before anything is destroyed, rather than cut. A node that is offline or
evicted keeps its copy until it replays, adopts a snapshot, is replaced or is
destroyed. `crewlet worklet purge` is the same route from a shell.

### A chat erase, prune or horizon

The engine's own chat has three gestures no seat is given and no tool makes,
because each destroys what people said — now or on a delay — and nothing
undoes it. They are decided as the purges are: **`admin`, a recent proof, and
never a seat**. Unlike every chat tool, none of them is said in the room, so
none needs a seat to act as: an administrator's own token may make them, and
each is recorded under whoever made it — the seat of a person, the login of a
credential acting as itself.

`POST /chatlet/channels/{channel_id}/erase?confirm=<count>` destroys the messages
the body names — `{"message_ids": ["…"], "reason": "…"}`, at most 250 a
request — on every data node, with the reactions, mentions and thread rows
that hang off them. It is the precise tool: a credential pasted into one
message. `?confirm=` repeats how many messages the request names, once a
repeated id is folded, and a count that does not match is `422 invalid` and
destroys nothing. The **reason is required, in the body** — at most 600 bytes,
and never on the URL, which carries it into every proxy's access log —
because the messages are gone and the erase record's reason is the only
account of them that survives.

`POST /chatlet/channels/{channel_id}/prune?cutoff=<instant>&confirm=<instant>`
destroys everything the room said before the instant, on every data node — the
blunt tool, and the one the retention duty runs on its own. `?confirm=`
repeats the cutoff (the same instant, in any zone), and a cutoff in the future
is refused, since that would empty the room rather than enforce a horizon.

`PUT /chatlet/channels/{channel_id}/retention?confirm=<days|company>` sets how
long one room keeps what it says — `{"retention_days": 90}`,
`{"retention_days": 0}` to keep it for ever, `{"retention_days": null}` to
return it to the company's `chat.chatlet.message_retention_days`. The field is
required: an absent one would read as the company's horizon, and a request
that said nothing would then destroy the past of a room somebody had set to
keep for ever. A shorter horizon is a prune on a delay — the retention duty
takes what it no longer covers on its next pass — which is why it is decided as
one, and confirmed as one: `?confirm=` repeats the horizon, the number of days
in decimal (`0` included) or `company` for `null`; a missing one is
`400 invalid_query` and one naming another horizon `422 invalid`, both before
anything is written. Every horizon is confirmed, a longer one too, since
whether it shortens the room's depends on what the room had, which the route
does not read.

**None of the three reads the room first.** The purges confirm against the row
— the item's key, the page's title — and an administrator who could do that
here would have to be shown a private room they are not in, or a direct
conversation between two other people, to take a pasted credential out of it.
So the confirmation is what the request itself destroys, repeated — a count,
an instant, a horizon — and a room the store does not hold is the store's own
answer.

**A retry is the same destruction.** Each is bound to the request's
`Idempotency-Key` and to what it destroys, in one canonical form — an erase's
set of ids sorted and deduplicated with its reason, a prune's cutoff as an
instant, the horizon as days or null — so the same erase named in another
order is the same operation, and the same key sent with other messages, a
later cutoff or another horizon is another operation that lands as asked,
never answered from the first record while nothing it named was destroyed. An
`unknown` answer is `503` with `outcome: "unknown"` and the `op_id` to send
back.

### Somebody else's inbox

`mark_inbox` and `set_pins` write the **caller's own** record and take no
handle — a model that could name whose inbox to mark could mark anybody's. So
`PUT /worklet/people/{handle}/inbox` goes through the same parsing and the same
writer as the tool, and `{handle}` resolves exactly as [a personal
question's](#whose-record-a-personal-question-answers-for) does: either of your
own names is your own record; somebody else's **login** is their record,
looked up in the identity directory — the seat it binds them to, or the login
for a credential bound to none; and any other name must be a seat the chart has
**exactly** — never looked up by resemblance, so a name no record could be kept
under (`Jane Doe`, a seat the chart lacks) is refused rather than guessed at.
The verb is decided **on the record the name resolved to** — yours, or the
admin grant, to unstick a departed person's queue — because deciding on the
spelling admitted an administrator to a record under a person's login,
and that is where the write then landed, read by nothing of theirs. A login
nobody holds is `404` to a caller who may write anybody's record and `403` to
everybody else, and a node that cannot read the directory is `503`.

`set_priorities` (`PUT /worklet/people/{handle}/priorities`) is the one person
verb that DOES take words for somebody else, because a lead sets a report's
queue — and a model types what it remembers. Your own queue, named by omission
or by either of your names, is never looked up; somebody else's **login** is
the identity directory's to resolve, exactly as above, and is **never** a seat
— matched against the chart, `jane.doe` replaced the queue of the seat `jane`
("Jane Doe"); only a name that is neither — a handle, a role, a display name,
an address — is resolved against the chart. The lead relation is then asked
about the seat it all resolved to, so a lead may name their report either way.
A name nothing answers to is refused, and the refusal lists the seats — the
roster is everybody's overview.

## Worklet and Doclet

`GET /worklet`, `GET /doclet/pages` and their neighbours read Worklet and
Doclet **through this node's estate router**, answered from this node's copy
while it serves and from a data node's otherwise. A seat's tools read through
the same facade, so an operator and an agent looking at one item see one item.

Three properties are worth stating because each is a decision rather than an
implementation detail.

**They are registered only where the company runs that backend.** A company on
`tracker.backend: jira` has no `work_items` question at all, and asking gets
`unknown_query` / `404`. That is deliberate: an operator who wired Jira and
then found a blank Worklet board would reasonably conclude their integration
was broken. There is no Worklet record for any node to hold, and an empty
board would claim otherwise.

**A copy that has not caught up refuses.** Every one of these answers
`unavailable` (`503`, with a `Retry-After`) where the copy that answers is
behind what the read needs, rather than returning an empty list; a copy that
has not applied this node's own writes is passed over for another data node's,
and a read no data node's copy could take is refused the same way. "This
company has no work" is an answer a person acts on — they file the duplicate,
they conclude the migration failed — so a copy that has not applied what the
fleet holds must not be able to say it.

The hint is **derived, not fixed**: how far behind the refusing copy is over
how fast it is actually draining, so a node grinding through a long backlog asks
for longer than one that caught up in milliseconds. **Every** refusal by the state
log is `unavailable` — `503` over REST, the `unavailable` frame on the socket —
carrying the refusal's code as `refusal` and a sentence for the person reading
it as `detail` (what happened and what to do; the refusal's own detail goes to
the node's log), because none of them is a fault: the node understood the
question and cannot answer it *here*. What differs is whether to come back. A `Retry-After` is sent only
where waiting clears the refusal, and it is that refusal's own hint — the
derived one above for `behind`, the broker's election timeout for `no_quorum`
and `broker_unreachable`, one position heartbeat for `below_floor` and
`floor_unknown`. A refusal waiting cannot clear — a record this node cannot
decode (`deferred`), a log at its byte ceiling (`log_full`), a barrier its
broker refused (`broker_refused`), an evicted node — carries **none**, so a
client asks another node or waits for an operator rather than polling this one;
see [the fourteen refusals](../guides/consistency.md#the-fourteen-refusals) for
what each one asks of whom. A `min_position` on another domain's log is not a
refusal at all: it is the request's mistake, and it is `400 bad_params`.

**The reads are here and the writes are tools.** Every write is attributed to
somebody — a seat through its own tools, a person at the dashboard through
[`/operator/act`](#operatoract--the-dashboards-write-surface), a script through
[the write surface](#the-human-write-surface), an assistant through the
[MCP surface](#operatormcp--your-own-assistant) — the same tools every time,
and never to "the dashboard", which is not a person and not a seat and cannot
be asked why. The routes under `/worklet/retention/` below are not about items:
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
are what every rule in Worklet is written at, so open work is
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

### Project files

A project holds **files** beside its work: a report a seat wrote, a spreadsheet
an operator uploaded, the output of a run somebody wants to keep. Each is a row
in Worklet (a path, a type, a size and SHA-256, a version and who wrote
it), and its content is one object in the
[object store](../concepts/object-store.md), the one store the whole fleet
shares, rather than a copy in every data node's database. See
[A project's files](../guides/worklet.md#a-projects-files) for what a seat
does with them.

The listing is an ordinary read (`GET /worklet/files`, the `work_files` query).
What the three byte routes add is what a question cannot carry: a download
**streams** the content back and an upload streams it in, neither holding the
file in memory.

```bash
# upload, replacing version 3 and nothing newer
curl -X PUT -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  -H "Content-Type: text/csv" -H 'If-Match: "3"' \
  --data-binary @q3.csv "http://localhost:8000/worklet/files/ENG/reports/q3.csv"

# download
curl -OJ -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  "http://localhost:8000/worklet/files/ENG/reports/q3.csv"
```

```json
{
  "project": "ENG", "path": "reports/q3.csv", "size": 48213,
  "hash": "9f2c…", "content_type": "text/csv",
  "outcome": "applied", "op_id": "…", "position": "…", "version": 4
}
```

**The content goes first.** An upload streams the whole body into one new
object in the store before it records the file, so a file that is listed is a
file whose content the fleet holds. A write cut off halfway, or refused once its
object is stored, leaves an object nothing names, which the collector removes a
day later; it never leaves a row pointing at content that is not there. A body
whose `Content-Length` is already over 1 GiB, and an upload into a project that
does not exist (`404 no_project`) or is archived (`409 conflict`, the project's
state), are refused before any of it is read.

**A write names the version it replaces.** `If-Match` carries the `ETag` the
download answered. Absent, the write creates the path or overwrites whatever is
there; present, a file that moved since is `412 version_moved` and nothing is
written. Unlike a work item, a file has no per-field merge — two people editing
one spreadsheet would lose one of them.

**A write is decided and attributed like every other write on this surface.**
Reading a file or the listing and uploading one take `worklet`; removing one is
its project's lead's, or `admin`'s, with `worklet`: taking a file out
of a project is the project's, as taking a work item out is. The write is
recorded under whoever the request resolved to — a person as their seat, a
credential nobody is bound through under its own login — with the credential it
came through beside it ([Who a write is attributed to](#who-a-write-is-attributed-to)),
so an upload names the person who made it in the file's history, never the
process that relayed it, and every write leaves an `operator_acted` record as a
[write-surface](#the-human-write-surface) verb does.

**The answer is three-valued.** `outcome` is `applied`, `pending` or
`unknown`, as every Worklet write's is (see
[Read Consistency](../guides/consistency.md)), and `position` and `version` are
present only where the write is known to have landed. An `unknown` is retried
by sending the same request again with the `op_id` it answered as its
`Idempotency-Key` — the key it sent, or the one minted for it — as every write
on the [write surface](#a-retry-is-the-same-operation) is: a fresh key would
make the retry a second write.

**Each mebibyte has 30 seconds to cross**, in either direction: an upload's
mebibyte has 30 seconds of the time the server spends waiting on the client to
arrive, never counting the time the server spends storing what already arrived,
and a download's next to be
taken by the client once it has been fetched — a floor of about 35 KB/s, far
under any real link, and the same floor the object store holds its own
transfers to. A whole-body deadline cannot bound a stream of up to a
gibibyte without capping real uploads, and none at all let a client trickle one
a byte at a time, or open a download and never read it, holding the handler and
its connection for as long as it liked. An upload that stops arriving — a
connection closed mid-body included — answers `400 unreadable_body` and records
nothing; a download the client stops taking, or whose content stops being
readable part way, is cut, which the client sees as a body shorter than its
`Content-Length`.

**A download is served as an attachment**, with the file's own type and the
same `default-src 'none'` policy every non-dashboard response carries, so a file
somebody uploaded as HTML downloads rather than running on the dashboard's
origin.

| Status | `error` | When |
|---|---|---|
| `400` | `bad_path` | The path is empty, longer than 1024 bytes, not UTF-8, ends in `/`, has an empty folder or a `.` or `..` in it, or carries a control character or a backslash. A leading `/` is dropped rather than refused |
| `400` | `bad_if_match` | `If-Match` is not a version |
| `409` | `conflict` | The project is archived: its state, which the same request meets until somebody unarchives it. Answered by the [one refusal rule](#which-class-a-refusal-is), as every write surface answers it |
| `422` | `invalid` | The write can never land as sent: the content type is over 255 bytes or spans lines |
| `400` | `unreadable_body` | The upload's body stopped arriving: the connection closed, or a mebibyte of it took more than 30 seconds (see below) |
| `403` | `unauthorized` | The authority table refused: `reason` and `grants` say why and what would admit the caller |
| `404` | `no_project` / `no_file` | No such project, or no file at that path |
| `412` | `version_moved` | `If-Match` names a version the file has moved past |
| `409` | `upload_expired` | The upload took longer than a write may name its object after (twelve hours from when its key was minted); send it again |
| `413` | `body_too_large` | The content is over 1 GiB — refused before reading where `Content-Length` says so, and otherwise once the body passes it |
| `500` | `content_corrupt` | The store answered with content that is not what the file records; trying again reads the same bytes, so restore the file from a backup or upload it again |
| `500` | `internal_error` | An upload failed in this node itself, in a way the store classed as neither of the two below (a store that stopped reading the content before its end); trying again doesn't clear it, and the reason is in the node's log |
| `503` | `unavailable` / `content_unavailable` | Worklet or the object store could not be reached, or the file's content could not be read right now. An upload the store did not take is `unavailable` under an election's `Retry-After`, and one it refused because it is full is `unavailable` with no `Retry-After`: no wait clears it, and an administrator makes room (see [its byte reservation](../concepts/object-store.md#its-byte-reservation)). An artifact's upload, an image attached to an Ask Crewlet question, `write_project_file` and the artifact tools answer both the same way |

The byte routes are mounted only where the company runs Worklet **and** the node
runs the object store. A company on Jira has no project files to serve.

### `GET /worklet/retention`: what the log is holding

**Takes `admin`, reads included**, on the same rule `/config` and `/secrets`
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
      "backup_at": "2031-03-29T02:00:00Z",
      "prose": "Nothing is being trimmed on tracker: ...",
      "terms": [{"name": "applied", "state": "ok", "seq": 918279004, "detail": "..."}]
    }
  ],
  "nodes": [...],
  "snapshots": [...],
  "replica": {"store_bytes": 10415140864, "projected_join_seconds": 308,
              "rejoin_window_seconds": 1800},
  "alarms": [{"kind": "backup_age", "detail": "...", "remedy": "...",
              "domains": [{"domains": ["tracker"],
                           "detail": "the newest verified backup of this log is 97h 14m old, and the policy asks for 24h; the trim has not advanced for 73h 14m, and the log is keeping up to 1840 record(s) older than its 24h replay window",
                           "term": "backup_floor", "since": "2031-03-30T02:00:00Z"}]}]
}
```

**`alarms`** holds each standing [alarm](alarms.md) once: `kind`, `detail` (the
measurement, in the units of the thing measured) and `remedy`. A condition that
is a property of a log names every log it stands on in **`domains`**: each entry
is one measurement, its `detail`, and the logs it was taken on, its own
**`domains`** list — one log for a held trim, with the `term` holding it and
`since`, when it stopped advancing, and every log measured in the same words
otherwise, so a fleet with no backup yet is one entry naming every log rather
than the same sentence once per log; the alarm's own `detail` is then the most
pressing log's. A log that would raise `trim_blocked` (held longer than its
replay window plus a trim tick, while keeping records older than the window) is
listed under `backup_age` instead while the backup term holds it and that log's
own `backup_age` fires, because the backup is its remedy: its entry there
carries the `term` and `since`, and its `detail` is its backup's measurement
followed by what the hold is keeping. A younger backup hold raises no
`trim_blocked` to fold, and a backup hold on a log whose own backup is inside
the policy stays under `trim_blocked`. So `alarms` has one entry per kind, and
its length is the health envelope's `alarms.count`. The `alarm_raised` and
`alarm_cleared` log lines are the one place a log stands alone: a condition that
is a property of a log writes a pair per log it stands on, carrying it as
`domain`, so a log joining an alarm that is already up, and a log leaving one
that stays up, are each said with their own start.

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

`maintenance_unread` does the same for the capacity windows: every log whose
window the answering node couldn't read, and **absent** when it read every one.
`maintenance` lists only the windows the read found open, so a log missing from
both has no window, and a log named here may have one. The `maintenance_open`
alarm judges a log named here on the window the node last read there, as old
as it was when that read answered, and its `detail` says so
([the retention guide](../guides/retention.md#where-it-stands-and-what-is-holding-it)).
Each open window's `participants_missing` and `admissions_blocking` are read
beside it, and one the node couldn't read is `acks_unread: true` or
`admissions_unread: true` with its list absent, never an empty list.

A node row's **`evicted`** is its tombstone once every log holds one, dated
from the latest and naming in `by` whoever made the eviction that reached the
last of them.

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

**`backup_at`** is when the backup this log's trim counts was taken: the newest
verified point the `backup_floor` policy takes the word of that covers this
log's stream — a node's copy under `engine`, the operator's acknowledgement of
this log under `operator`. It is **absent** where no counted backup covers the
log, which is the state the `backup_floor` term blocks on. The `backup_age`
alarm is evaluated per log from it and lists each log in its `domains` with its
own age, because the logs' backups have one age each: under `operator` every
log is acknowledged on its own. Logs whose measurement reads the same — none
of them backed up yet — share one entry naming them.

`headroom_fraction` is a **pointer** and is absent when the broker could not be
asked. A fraction of an unknown ceiling is not zero headroom, and zero is what
the one alarm an operator cannot ignore fires on. It is a fraction of the
ceiling **ordinary writes** are held to: `max_bytes` less `reserve_bytes`,
which on every identity-claiming log is the top of the ceiling kept for the
records that install or lift a gate — the larger of a sixteenth of it and
seven of the log's largest records with a mebibyte beside them — so that an eviction still lands on a log full for
everything else ([the gate reserve](../guides/retention.md#the-gate-reserve)).
`reserve_bytes` is absent on a log that keeps none — the vector changelog and
the usage log — where the headroom is of the whole ceiling.

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

All three are **POSTs**, and all three (like every other `/worklet/retention*`
route) take `admin`: moving the floor the trim deletes against,
stopping a machine writing and letting it write again are the deployment's own
controls, and a caller without it is refused `403` naming it.

| Route | What it does |
|---|---|
| `POST /worklet/retention/ack?stream=NAME&position=N` | Publishes an operator backup floor. Refused `400` naming both when either is missing, and `404` when the stream is not one this node runs, which on a node running no state log is every stream. The point is stamped with **that stream's own generation**, read from the running log: a bare sequence at another log's generation names a number space the copy does not cover. It is **filed under that stream**: acknowledging another log writes another point and leaves this one standing, and each log's trim reads its own — under `backup_floor: operator` that is one acknowledgement per log. |
| `POST /worklet/retention/evict/{node}?confirm={node}` | Installs the eviction gate on every identity-claiming log: Worklet's, Doclet's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, Chatlet's and the executor log's; the vector log and the usage log count no node and get none. **Refused `409 eviction_refused`**, with nothing written to any log, while the node still holds a live presence lease: it is still reaching the fleet, and an eviction would drop everything it writes. The body carries `detail`, `hint`, `op_id` and `actions`, `["wait", "force"]`: stop the node and let its lease lapse, or force it. `force=true` overrides that refusal for a node wedged in a way that still renews its lease. A node that cannot read the presence leases at all answers **`503 eviction_unjudged`** (a judgement nobody could make is not one that came back clear), with a `hint` and `actions` `["retry_same_op", "force"]`; `force=true` takes the eviction past that too, since the leases are the judgement's only input, and the node logs `retention_eviction_forced_unjudged`. A node in a [capacity window](../guides/retention.md#changing-a-logs-ceiling) answers **`409 not_publishing`**, before anything is judged, with `detail`, `hint` and `actions` `["wait"]`: a gate record is an append, and the window's mode appends nothing; readmission answers the same. |
| `POST /worklet/retention/readmit/{node}?confirm={node}` | The inverse commit, on every one of those logs. **Refused `409 readmission_refused`**, with nothing written to any log, when in any one of those logs the node has not applied every record up to the one just before the higher of that log's published floor (at the log's current generation) and its first surviving sequence (its last published position is more than one below that bound), or its last published position is from a generation the log has since left. The body carries the sentence (`detail`), what to do (`hint`, and `actions` `["wait"]`), and the numbers: `domain`, `position`, `generation`, `floor`, `first_seq`, `floor_generation`, and `published`: false for a node that has never published a position and is judged as holding nothing. A judgement that could not be made at all refuses too, with nothing written, as **`503 readmission_unjudged`**: the register or a floor unreadable, a log's first surviving sequence unreadable, or this node behind a reanchor the fleet has made. The body carries `detail`, `hint`, `actions` `["retry_same_op", "other_node"]` and, where one log could not be judged, that `log`. `force` has no effect here and nothing overrides this refusal: the floor is a fact about what the node holds, not a lease it might be wedged into renewing. |

`confirm` echoes the node id, and a mismatch is `400`. A node id no node could
run under — the [`node.id`](../concepts/configuration.md#nodeid) rule, since the
id becomes a subject token on every log — is `400 invalid_gate`, with nothing
judged or written.

**A refusal's `detail` is the refusal's own sentence**, the one its operator
acts on, and never the error underneath: no package name in front of it and no
broker's or store's words behind it. Those go to this node's log. A judgement
that stopped on a fault nothing told is `500 gate_failed`, saying nothing was
written and carrying `node` and `op_id`, so the same request can be sent again
under it.

The gate is written **as the caller**: the eviction's `by` — what `crewlet
retention status` prints as "evicted by" — is their name as [a write is
attributed](#who-a-write-is-attributed-to), and the record carries the
credential they pressed it through beside it. It used to be written as the
serving node's own writer, so "who stopped this machine writing" had one
answer, whichever node the request happened to reach. A capacity operation
names its opener the same way, `by` beside `operator_id`, on the operation and
on its entry in the report's `maintenance` list, which names every open window,
the oldest first.

**The gesture is judged once, before any log is written**, and then its record
goes to every identity-claiming log at once, each written by the node the
request reached through its own write authority — every data node holds the
whole estate ([the retention guide](../guides/retention.md#eviction)). The
trim counts nodes per log, so a record on one log lifts that log's pin and no
other. A `200` answers **per log**:

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
    {"domain": "iam", "stream": "CREWLET_IAM_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.iam:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_IAM_LOG", "generation": 0, "seq": 58311}},
    {"domain": "artifacts", "stream": "CREWLET_ARTIFACTS_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.artifacts:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_ARTIFACTS_LOG", "generation": 0, "seq": 30552}},
    {"domain": "artifactdata", "stream": "CREWLET_ARTIFACT_DATA_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.artifactdata:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_ARTIFACT_DATA_LOG", "generation": 0, "seq": 112940}},
    {"domain": "assistant", "stream": "CREWLET_ASSISTANT_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.assistant:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_ASSISTANT_LOG", "generation": 0, "seq": 15734}},
    {"domain": "chat", "stream": "CREWLET_CHAT_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.chat:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_CHAT_LOG", "generation": 0, "seq": 207714}},
    {"domain": "executor", "stream": "CREWLET_EXECUTOR_LOG",
     "op_id": "01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4.evict.executor:node-4",
     "outcome": "applied",
     "position": {"stream": "CREWLET_EXECUTOR_LOG", "generation": 0, "seq": 30870}}
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
before the ledger's sweep reached it — so the node published nothing and cannot tell whether the record
landed, and the same request here answers the same way every time. Such an
entry offers `other_node` rather than `retry_same_op`: send the same request,
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
| `retry_same_op` | Send the same request again with the answer's `op_id` | An `unknown` outcome (unless it is `unvouched`), a lost race, a failure before the write answered, and a refusal that clears on its own (`behind`, `deferred`, `floor_unknown`, `below_floor`, `eviction_unknown`); a gate refusal — `evicted`, `overtaken`, `abandoned` — of **another node's copy** of the operation, which this node's append was collapsed onto (`hint` names that node): the reason is that node's standing, not this one's, so this node finishes it once the log's duplicate window (two minutes from when the copy landed) has passed; `503 eviction_unjudged`; and `503 readmission_unjudged` (beside `other_node`) |
| `new_gesture` | Start a new gesture, without `op_id` | `superseded` — the operation's record landed and a later gate record on the same node has undone it since (an eviction retried after a readmission) — and `op_reused`, an operation id that already names a record on another object |
| `force` | Send the eviction again with `force=true` | `409 eviction_refused`, `503 eviction_unjudged` |
| `other_node` | Send it, with the same `op_id`, through another node the fleet still counts | Of this node's own standing or its own record: `evicted` (this node is evicted itself), `overtaken` and `abandoned` (this node wrote the record from rows a reanchor left behind — send it through a node on the log's current generation), an `unvouched` unknown (this node's ledger cannot say whether the record landed), and beside `retry_same_op` on `deferred`, `below_floor` and `503 readmission_unjudged`. An `overtaken` or `abandoned` refusal, and an `evicted` one that names a `position`, is a record that landed and applies nowhere: it holds the `op_id` on that log for the log's duplicate window (two minutes) from when it landed, and the same `op_id` sent sooner — through any node — is answered the same way again, so `hint` says to wait that out first |
| `reanchor` | [Re-anchor the log](../guides/retention.md#re-anchoring-a-recreated-or-restored-log) first, then send the same request with the same `op_id` | `wrong_stream` |
| `set_capacity` | [Raise the log's ceiling](../guides/retention.md#changing-a-logs-ceiling), then send the same request with the same `op_id` | `log_full` — a gate record is admitted into the log's [gate reserve](../guides/retention.md#the-gate-reserve), so this is a log full to its broker ceiling past even that |
| `wait` | Wait for what `hint` names to clear on its own, then run it again — what that is depends on the refusal, so a client reads the answer's `error` beside the action | `409 eviction_refused` (the lease to lapse), `409 readmission_refused` (the node to catch up), `409 not_publishing` (the fleet to leave its capacity window) |
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
the request, within twenty seconds, so a request abandoned before it wrote
nothing; once the first record is about to be written the node finishes the
gesture under its own budget — thirty seconds for the logs, every one written
at once — so a dropped connection or a client timeout never leaves a node
evicted on one log and counted on another. A node answers one gesture within
**fifty seconds** at most — the judgement and the logs together — so a client
waiting a minute, as both of the engine's own do, always reads the node's own
answer, and so does a reverse proxy at its usual sixty-second read timeout. A
caller that sends its own
`op_id` — `crewlet retention evict` and the dashboard both do — can ask again
with it and read every log's answer; one that let the route mint it has lost
the id with the answer that never arrived.

Every node running the state log serves both routes, whichever backends the
company uses: a company on an external tracker still runs every log. A
node with no log to write a gate to answers `503` rather than `404` — the
routes exist on this build, and telling an operator they do not sends them
looking for a version mismatch that is not there.

### The capacity window

A log's Tier A ceiling is only the value its stream is created with, and these
routes are the window in which a running log's ceiling changes. The
[procedure is documented once](../guides/retention.md#changing-a-logs-ceiling);
what follows is the wire surface. Every route here, the status read included,
takes `admin`.

| Route | What it does |
|---|---|
| `POST /worklet/retention/capacity?stream=NAME&bytes=N&confirm=N` | Opens or resumes the operation and drives it as far as this node's mode allows. `confirm` repeats `bytes` and a mismatch is `400`: the target is chosen once for the life of an operation. `assert_excluded=true` is required only on an external broker. An admission from a process that is still running (it holds its node's presence) closes the window again as it opens: `409 capacity_refused` naming the node, with no `operation`. An admission from a process that isn't running answers `409 capacity_refused` naming the node and the commands past it, with the window open at `opened` in `operation`: nothing is sent until that node restarts into a maintenance mode or is excluded, and the window refuses every node that starts in service until then. An admission whose node another process now holds is withdrawn by the window. A `200` also answers `open_windows` (every window open on any log, this one included while it stands, each with its `stream`, `operation_id`, `phase` and `target_max_bytes`) and `unread_windows` (every log whose window this node couldn't read): a node won't start in service while either is non-empty. |
| `GET /worklet/retention/maintenance?stream=NAME` | The operation, every acknowledgement of this log's window, each naming its `stream` (one a node made as it booted with no window open on the log names no operation: an empty `operation_id` and `attempt` 0, which a baseline reads and a seal never takes), every admission, and (computed here rather than by each client) what the window's next step is waiting on. At `opened` that is the baseline: `restarts_missing` (the participants with no restart into a maintenance mode recorded) and `admissions_blocking` (every admission but an excluded node's). From the baseline to the seal it is the seal: `sealed` and `blocking`, which no longer reads an admission. Once the window has sealed (`sealed` and `confirmed`) it is `sealed: true` and no `blocking`, since nothing after the seal waits on a node. |
| `POST /worklet/retention/maintenance/abandon?stream=NAME&confirm=OPERATION-ID` | From `opened` clears the operation outright and answers no `operation`; from anywhere else enters the seal, and the operation records who abandoned it as `abandoned_by`. Either answers `open_windows` and `unread_windows` as `capacity` does. `confirm` repeats the open operation's id and is checked before anything changes: a missing one is `400 confirm_required`, and one naming another operation is `400 confirm_required` naming the open one in `detail`, with it in `operation`. |
| `POST /worklet/retention/maintenance/exclude?stream=NAME&node=ID&confirm=ID` | Waives one node's acknowledgement and withdraws its admission, and the operation records who asserted it under `excluded_by`, keyed by the node. An excluded node's admission no longer stops the window, whether or not it could be withdrawn. Answers `open_windows` and `unread_windows` as `capacity` does. |

**A blocked operation says why twice**: `blocked` is a code for scripts
(`abandoned_by_operator`, `capacity_operation_unsealed`), and `blocked_detail`
beside it is the sentence a person reads: what blocked the window and what
clears it, with every command as the CLI takes it. An abandoned window's
sentence is decided by its seal, so an answer carrying one reads the window's
acknowledgements to say it. Every answer here that carries an `operation`
carries both, and so does each entry in the retention report's `maintenance`
list. A code this node's build doesn't know still gets a sentence, saying so.

**Every gesture on the window names who made it**: `by` and `operator_id` for
whoever opened it, and the same pair under `abandoned_by` and, per node, under
`excluded_by` — the author and the credential they acted through, as every
other trail records a write. An exclusion is the one fact the seal takes on
somebody's word, and whose word it was used to be said only in the log of the
node that served the request. An abandonment from `opened` clears the record
it would have been written on, so only that log names who made it.

**A node in `normal` mode refuses the write routes**, naming the restart: the
usage a resize is decided against has to be a quantity nothing can move.

**Every refusal is `409 capacity_refused` in the refusal's own words**: what
was refused and what to do, the numbers it turns on included, and never the
error underneath, which goes to this node's log. A gesture that stopped on a
fault nothing told is `500 capacity_failed`, pointing at the status read; a
status read that couldn't read the window is `503 maintenance_unreadable` with a
`Retry-After`, since asking again once coordination answers is the remedy, while
one that read the window and then couldn't read its acknowledgements or the
admissions answers `200` with the window and `acks_unread: true` or
`admissions_unread: true`, leaving out that register and every list formed from
it (`restarts_missing`, `admissions_blocking`, `sealed`, `blocking`); and a stream this node
doesn't keep is `404 unknown_stream`, naming the ones it does. Each answer to a
write carries `operation` where there is one. The
three-mode boot is `crewlet run -mode`; see
[the CLI reference](cli.md#crewlet-run).

The `restarts_missing` / `admissions_blocking` and `sealed` / `blocking`
fields are **computed server-side**, from the same predicates the coordinator
itself runs. A client that re-derived them would be a second opinion about one
barrier, and the two would drift.

### Re-anchoring

| Route | What it does |
|---|---|
| `GET /worklet/retention/reanchor?stream=NAME` | The LIVE stream's own `created_at`, read from the broker on the call (the instant a `wrong_stream` refusal names), the generation that stream's domain stands at, and what a reanchor run now would do: `case`: `recreated` (another stream than the rows are keyed to, followed from its first surviving record), `restored` (the same stream brought back from an older copy, ending below the checkpoint or holding another record at it since, followed from its end, one below the node's own generation record where an earlier attempt already appended it) or `abandoned` (the same stream, continuing in a generation only an evicted peer held, followed from this node's own checkpoint with that generation's records void), with `cursor`, the sequence the new checkpoint would sit at. With nothing to re-anchor there is no `case`, and `nothing_to_reanchor` says why. A restored log holding records this node's rows do not hold, written after the restore, also answers `discards` (the newest of them: `seq`, `kind`, `subject`, `writer`, `op_id`, `stored_at`, and `ledger_lost_before` when its operation is older than the instant this node's operation ledger may have lost rows from: the rows may hold it after all) and `discarding` (why a reanchor refuses without `discard=true`). `404 unknown_stream` for a stream this node does not run; `503 stream_unreadable` when the broker did not answer the read, which is worth retrying. |
| `POST /worklet/retention/reanchor?stream=NAME&confirm=<created_at>[&force=true][&discard=true]` | Runs that ONE domain's generation transition, answering with the new `generation`, the `case` it answered, the `cursor` its checkpoint went to (for a restored log, one below the generation record the transition appended; once that record is appended the transition finishes on its own time, so a client that disconnects does not stop it halfway) and, when it discarded records written after a restore, the newest as `discarded`. No other domain's checkpoint moves, and the domain's applier resumes with no restart. |

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
question. A node in a [capacity window](../guides/retention.md#changing-a-logs-ceiling)
refuses every reanchor `409 reanchor_refused`, naming its mode: the reanchor
appends a generation record, and the window's mode appends nothing.

Every refusal, and every transition that stopped where it can say what was
committed and what to do, is `409 reanchor_refused` in those words, as
`nothing_to_reanchor` and `discarding` are: no package name and no broker's
words, which go to this node's log. A transition that stopped on a fault
nothing told is `500 reanchor_failed`, pointing at the status read; a status
read the broker didn't answer is `503 stream_unreadable`; and a stream this
node doesn't keep is `404 unknown_stream`, naming the ones it does.

## Artifacts

An **artifact** is a page, a doc or a native sheet a person or a
seat publishes for others to open: immutable numbered versions of its files,
shared with seats, units or the whole company at a role (`viewer`,
`commenter`, `contributor`, `editor`; the owner holds every role). Its records
are the `artifacts` state log's, every data node holds them, and its bytes are
objects in the [object store](../concepts/object-store.md). The routes below
are served on `api.port`; the pages themselves are served only on the
separate artifact origin (`api.artifacts`), inside a sandboxed frame, and
never here.

### Who may do what

Every route decides one verb of the authority table on the artifact's own
owner and shares, **before** a version, a file or a byte is read:

- **A caller with no relation to an artifact is answered `404 not_found`**,
  byte for byte as an id nothing holds, on every route, so nothing says a
  private artifact exists.
- **A relation below the verb's role** is `403 unauthorized` with `reason:
  role` and `role` naming what it needs, which the owner can share.
- **Authoring asks the artifacts app** beside the role: creating, publishing,
  renaming, labelling, adding an asset. Reading and sharing ask the relation
  alone, so a person invited with no app reads what was shared with them.
- **An agent is kept to `contributor`** on an artifact it does not own unless
  the company sets `artifacts.agents_manage_others`. **The lead of an agent**
  holds owner on that agent's artifacts, and nothing on a person's.
- **Trash, restore and transfer are a person's**; a transfer and a forced
  publish need a person present, which a machine token never is
  (`403 token_refused`). **A purge is an administrator's**, with a proof of
  identity inside the step-up window (`403 step_up_required` otherwise) and a
  person present.
- **An administrator may open any artifact.** Every read only the admin path
  admits writes an elevated-read record first (one per day, administrator,
  artifact and version), which the artifact's history shows its owner and
  editors and the company trail (`GET /artifacts/trail`) shows every
  administrator; a read whose record cannot be written is answered `503` and not
  served. That covers every read of content: a version's files, source and
  frame, every datastore question and the CSV export, a room's join, and the
  live entry a `412 version_moved` hands back. An administrator who holds a
  share is admitted as that share, at its role, and the admin path (owner,
  recorded) opens only for a verb their relations do not reach. A listing
  never shows an artifact on the admin path.
- **The trash stops serving content at once.** A trashed artifact's head, its
  versions and its history stay readable to its audience for the trash view;
  its files, source, frames, export and datastore questions are `409
  artifact_trashed` (`artifact_trashed` on the socket), and a room's join is
  refused `artifact_trashed`, before anything is read or recorded — to a reader
  it is disclosed to: one with no relation is told `not_found`, as for an id
  nothing holds. The trash is a state its owner's restore ends, which
  `not_found` would deny; a purge is for good, which `not_found` — and a
  purged store's `410 data_gone` — say truly.
- **A purge is committed before anything it removes is gone.** Its first step
  stamps the artifact on its own record (`purge_begun_at` on the head, and into
  the trash if it was live), and only then does the datastore go, which no
  restore can bring back. From that step on a restore is `409 artifact_purging`
  and the artifact is never served or written again; a purge that stopped part
  way finishes on its retry under the same key, an administrator's purge, or
  the trash reaper's next pass, which takes every begun purge whatever its
  trash term.

What the decision concluded is stamped into the record every write forms: the
caller's role on the artifact, whether only the admin path admitted them, the
Tier A quotas in force, the surface the request came through (`dashboard` for
a browser session, `api` for a bearer token) and the work item it was made
for. The domain's applier decides from the record alone.

### Writes

| Method and path | Verb | What it does |
|---|---|---|
| `POST /artifacts/uploads` | `create_artifact` | An upload intent for a new artifact |
| `POST /artifacts/{id}/uploads` | `publish_artifact`, or `upload_artifact_asset` with `asset: true` | An upload intent for the next version, or into the asset store |
| `PUT /artifacts/uploads/{upload}/files/{path}` | the intent's own verb, decided again | One file's bytes, streamed through the engine |
| `POST /artifacts` | `create_artifact` | A new artifact and its first version, owned by the caller. `"starter": true` begins it from its kind's starter content, which the engine holds and publishes inline, naming no upload, files or entry; a kind with no starter is `422 invalid`. A `slides` starter takes an optional `"deck": {"theme": "<id>", "slides": [{"layout": "<id>", "title": "..."}]}`, the theme it is drawn in and the slides it opens with (at most 60, each title at most 200 bytes); absent, it opens with a cover, an agenda, two columns and a closing slide in the `ink` theme. A plan naming a theme or layout there is not is `422 invalid`. A doc's starter may carry the text its writer already wrote, `"doc": {"body": "<markdown>"}`: the engine writes the title as the tab's heading and the body under it, held to the doc format, at most 1 MiB in all. A design's starter is the `layout` sent with it: its `boards` (each a `title`, `x`, `y`, `w`, `h` and an optional `brief`, the first the cover) and `notes` (as `canvas.json` holds them), published as `canvas.json` and one placeholder page per board under `boards/`, every typed word escaped as text; a design starter with no layout, and a layout outside the canvas format's bounds, are `422 invalid`, each problem named at its place in the layout (`boards[2].w`). `deck`, `doc` or `layout` without `starter`, or on any kind but its own, is `422 invalid`. `"folder": "<id>"` creates it in that folder, which asks `artifacts.folder.file` on it beside the create: a folder the caller cannot see, or one removed, is `404 folder_not_found` |
| `POST /artifacts/checks` | `create_artifact` | A page's text files judged as a publish of its kind would judge them, writing nothing: `{"kind": "html", "entry": "index.html", "files": [{"path": "index.html", "text": "..."}]}`, answered `200` with every `refusals` and `warnings` finding (path, line, code, message). Framed kinds only, 1 to 64 text files and at most 2 MiB of text. An allowlisted CDN file is named where its copy would be served and never fetched, so a CDN that refuses the file is found only at publish. Held to a budget per node (two checks a second for one person, a burst of ten): `429 check_budget` with a `Retry-After` |
| `POST /artifacts/{id}/versions` | `publish_artifact`; `artifacts.publish.force` beside it when forced | The next version, based on `If-Match: "<n>"` |
| `POST /artifacts/{id}/versions/{n}/restore` | `publish_artifact` | Version `n` again as a new version, every file carried, based on `If-Match` |
| `PATCH /artifacts/{id}` | `update_artifact` | The title, the description or the icon |
| `PATCH /artifacts/{id}/versions/{n}` | `update_artifact` | Version `n`'s label (`{"label": ""}` clears it); a labelled version is kept past the hundred unlabelled ones |
| `PUT /artifacts/{id}/shares` | `share_artifact` | `{"audience": {"kind": "seat", "id": "bob"}, "role": "editor"}`; never above the sharer's own role, and never the company from an agent. A NEW audience is held to the grant rule: a seat audience is a seat handle, never a login (`token:maya` or `jane.doe` is refused 422, `invalid`), and a seat or a unit the company's roster does not hold is refused the same way, with the sentence that says which; a create's `shares` are held to the same rule. An audience the artifact already shares with may change its role whatever it names |
| `DELETE /artifacts/{id}/shares/{audience}` | `unshare_artifact` | The audience as `seat:<handle>`, `unit:<key>` or `company`. Any audience a share can be stored under is withdrawn, never held to the grant rule or the roster: a share to a login written before the rule, and a share to a seat the chart has since removed, are the ones an owner most needs to remove |

| `POST /artifacts/{id}/transfer` | `artifacts.transfer` | `{"to": "<seat>"}` |
| `POST /artifacts/{id}/work-items` | `get_artifact` on the artifact and `update_work_item` on the item | `{"work_item": "ENG-12", "after"?: "CREWLET_TRACKER_LOG@1:9"}`: links the artifact to a work item Worklet holds, so the item's Artifacts panel lists it. `after` is the `position` the item's create answered with, and the item is read from no earlier than it. See [Linking to a work item](#linking-to-a-work-item) |
| `DELETE /artifacts/{id}` | `artifacts.trash` | Into the trash: every frame stops loading at once, and it is purged after thirty days |
| `POST /artifacts/{id}/restore` | `artifacts.restore` | Out of the trash; `409 artifact_purging` once a purge has begun on it |
| `POST /artifacts/{id}/purge` | `artifacts.purge` | Gone for good, in three steps: the artifact is committed to its purge (into the trash, never restored again), its datastore goes, then its own rows |
| `POST /artifacts/{id}/assets` | `upload_artifact_asset` | `{"upload": "<upload>", "name": "<name>"}`: finalize an asset |
| `DELETE /artifacts/{id}/assets/{asset}` | `upload_artifact_asset` | Remove one |
| `PUT`, `DELETE /artifacts/{id}/pin` | `artifacts.pin` | The caller's own pin |
| `PUT /artifacts/{id}/folder` | `move_artifact` on the artifact and `artifacts.folder.file` on the folder | `{"folder": "<id>"}` files it in a folder, `{"folder": ""}` at the top level. See [Folders](#folders) |
| `POST /artifacts/folder-moves` | as `PUT /artifacts/{id}/folder`, for each item alone | `{"items": [{"artifact", "folder"}]}`, at most 25, answered `200` with `{"results": [{"artifact", "outcome", "message"?}]}`. See [Folders](#folders) |

**A share that reaches nobody is surfaced, not dropped.** A build before the
grant rule could store a share to a login named as a seat (`seat:token:maya`),
which no principal's seat matches, so it grants nothing. The engine neither
deletes such a share on read nor sweeps it at boot: a share is authorisation
data its owner wrote, and removing it silently would rewrite the record of who
was meant to have access without anybody deciding to. Instead `artifact_access`
marks it `invalid: true`, the share dialog lists it with why ("token:maya is a
sign-in, not a seat. Remove it and share with their seat.") and offers Remove as
its one action, and the listing never counts it toward the login's Shared
section, where every read of it would be withheld.

Every write but the transfer carries an `Idempotency-Key`, from which every
record id is derived, so a retry under the same key is the same operation
(`400 op_id_invalid` without one). A write answers `200` with its `outcome`,
`202` for one durable and not yet applied on this node, and `503` with
`outcome: unknown` and the `op_id` for one nobody could account for: send it
again under the same key, never a fresh one. The transfer needs no key: the
upload claims each file before a byte is read, and a second put of it is
refused `409 upload_consumed`. A node that runs no upload pipeline (no object
store, or no coordination store to reserve the bytes in) answers every route
that checks or publishes — create, check, publish, assets and transfer —
`503 unavailable` with **no** `Retry-After`: it builds the pipeline once for the
life of its process, so no wait changes the answer, and why is in its log
rather than the answer.

### Linking to a work item

`POST /artifacts/{id}/work-items` puts an artifact that already exists on a
Worklet item's Artifacts panel, the way a create or a publish carrying
`work_item` does for what it makes. It records one `link_work_item` row in the
artifact's history, filed under the item, and changes nothing else.

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" \
  -d '{"work_item": "ENG-12"}' \
  "http://localhost:8000/artifacts/$ID/work-items"
```

- **The artifact is decided as a read** (`get_artifact`): any role on it,
  viewer and above. A caller with no relation is `404 not_found` like any
  other artifact route, and an administrator with none is admitted on the
  admin path, recorded as an elevated read first.
- **The item is decided as a write** (`update_work_item`), by the same rule
  `PATCH /worklet/items/{key}` takes: a caller who does not use Worklet is
  `403 unauthorized`.
- **The item must be live.** `work_item` is a key or an item id, at most 64
  bytes of visible ASCII; one Worklet does not hold, or holds removed, is
  `422 invalid`, as is a company whose work items another tracker keeps. A node
  not yet handed a company answers `503 no_active_revision`. A trashed
  artifact is `409 artifact_trashed`.
- **A link sent right after the item was filed names its create.** On a fleet
  the link can reach a node that has not applied the create yet, which would
  read the item as one nobody filed. So `after` carries the `position` the
  create answered with (`<stream>@<generation>:<sequence>`, the act answer's
  `position`), and the item is read through Worklet's read-your-writes
  floor, the same one `min_position` sets on every Worklet read: the read waits
  up to the read budget for this node to apply the create. A node that does
  not is refused as every floored Worklet read is, `503 unavailable` with
  `refusal: "behind"`, its `detail` and a `Retry-After`, and nothing is linked;
  send the link again under the same key. An `after` that is not a position,
  or one on another log than Worklet's (an artifact's own `position`, for
  example), is `422 invalid` and is never waited for. Without `after` the item
  is read at this surface's own level, as before.

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" \
  -d '{"work_item": "ENG-12", "after": "CREWLET_TRACKER_LOG@1:9"}' \
  "http://localhost:8000/artifacts/$ID/work-items"
```

The answer is `{"artifact", "work_item", "op_id", "outcome", "position"}`,
where `work_item` is the item's current key: an id or a former key is linked
under it, so the panel finds it. A link the artifact's history already makes
(an earlier link, or a write made for the item) answers `200` and writes
nothing.

### Publishing a version

Bytes are **reserved before they move**. An intent declares every file's path,
SHA-256 and size; the node answers which of them the artifact already holds
(`carried`, which move no byte) and how each of the rest is sent (`transfers`):
a presigned `PUT` straight to the bucket where the S3 backend proved it can
take one, a `PUT` to this API otherwise. Then the publish names the upload and
every file of the version.

```bash
# 1. declare the files
curl -X POST -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  -H "Idempotency-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"entries":[{"path":"index.html","sha256":"<hex>","size":5120}]}' \
  "http://localhost:8000/artifacts/$ID/uploads"

# 2. send each file the answer lists under transfers
curl -X PUT -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  --data-binary @index.html \
  "http://localhost:8000/artifacts/uploads/$UPLOAD/files/index.html"

# 3. publish, based on the head you read
curl -X POST -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  -H "Idempotency-Key: $KEY2" -H 'If-Match: "3"' -H "Content-Type: application/json" \
  -d '{"upload":"'$UPLOAD'","files":[{"path":"index.html","upload":true},
       {"path":"style.css","carry":"<hex>"}],"capabilities":["db"]}' \
  "http://localhost:8000/artifacts/$ID/versions"
```

A file is named by `upload: true` (its bytes came through the upload),
`carry: "<sha256>"` (bytes the artifact holds), or `from: <n>` (version `n`'s
file at that path); in `mode: patch` a file left out is carried from the base
and `remove: true` drops one, and `edits` (`old_str`/`new_str`, each matching
exactly once) change the base version's text. The answer is the new
version's `number`, the policy linter's `warnings` and `files`
(`{"stored", "carried"}`: how many of the version's files this publish stored
new and how many the artifact already held, counted from the manifest that
landed, so an unchanged page the upload sent is counted as held); `work_item` names the
Worklet item the version is made for, which lists the artifact there
(`GET /artifacts?work_item=CRW-412`).

**A publish based on a head that has moved** is `412 version_moved`, carrying
the live `head`, the live version (`live`, its manifest included) and, where
the entry is text, up to 1 MiB of it (`entry.text`, `entry.truncated` past
that): merge onto it and publish again with `If-Match` naming the live number.
`"force": true` publishes over the moved head instead, a person's gesture with
a person present, and the version is marked forced.

### Reads

| Path | Question | Answers |
|---|---|---|
| `GET /artifacts` | `artifacts` | One page of what the caller can see, in S3 `ListObjectsV2` terms: `prefix` (a folder path, `Marketing/Q4/`, empty for the top level) or `folder` (the folder's id in place of the path), `delimiter=/` (one level, its child folders answered as `common_prefixes`; absent, everything beneath), `max-keys` (50 by default, at most 200) and `continuation-token`; beside them `scope` (`mine`, `shared`, `company`, `agents`, `all`), `kind`, `q`, `status` (`live`, `trashed`), `pinned`, `work_item`, `sort` (`updated`, `created`, `title`, `opened`). Answers `{prefix, delimiter?, contents, common_prefixes?, key_count, is_truncated, next_continuation_token?, total, read_level, position, complete}`. Filtered for the caller's relations and decided row by row, never refused. See [Folders](#folders) |
| `GET /artifact-folders` | `artifact_folders` | The folder tree the caller sees: every folder, parents before children and siblings A to Z, each with its `role` for the caller and the artifacts and folders directly in it they can see, and `top_level`, the artifacts they can see filed in no folder they see. See [Folders](#folders) |
| `GET /artifacts/{id}` | `artifact` | The head, the caller's `role`, whether they may publish (`writer`), and the shares to the owner and editors (`share_count` below) |
| `GET /artifacts/{id}/versions` | `artifact_versions` | The retained versions, newest first |
| `GET /artifacts/{id}/versions/{n}` | `artifact_version` | One version and its manifest |
| `GET /artifacts/{id}/history` | `artifact_history` | Every publish, share, trash, link to a work item and elevated read, newest first; below editor, without the elevated reads or who a share named |
| `GET /artifacts/{id}/assets` | `artifact_assets` | The asset store |
| `GET /artifacts/{id}/seen-by` | `artifact_seen_by` | Who has opened it, to the owner and editors while the company's `artifacts.seen_by` is `editors` (`shown: false` otherwise) |
| `GET /artifacts/{id}/access` | `artifact_access` | The share dialog's list: the owner, each share with the seats a unit share reaches today (and `invalid: true` on a share that reaches nobody, see above), and an agent owner's leads; to a viewer, the owner and a count |
| `GET /artifacts/usage` | `artifact_usage` | The company's counters against the Tier A quotas; `artifacts.usage`, an administrator's |
| `GET /artifacts/trail` | `artifact_trail` | **The company trail of every artifact**: every artifact's history rows across the company, private ones included, newest first in the log's order, each naming its `artifact`, its current `title` and `owner`, the op, the actor and whether only the admin path admitted them. `op=` narrows it to one op (`read_elevated` lists every administrator's override, so one nobody would find in an artifact's own history is found here), with `cursor` and `limit`; an op no history row is filed under (a pin, an opened mark, a purge, which takes the history with it) is `400 bad_params`. `audit.read`, an administrator's, decided by no relation to any artifact |

A head and every version carry `capabilities`, the set the version declares,
always present and `[]` for none, so a version that declares nothing is told
from one whose field went missing; `contract` is absent for a kind that runs
no page runtime (a Markdown document, a sheet).

The same questions are asked over the socket by name. Every question about one
artifact is decided by `get_artifact` and records the admin path's override
as every read route does.

### Folders

An artifact is filed in at most one folder, and **a folder grants nothing**:
who may open an artifact is still its owner and its own shares, so filing it,
moving it and removing its folder never widen or narrow its access
([ADR-0047](../../adr/0047-an-artifact-lives-in-one-folder-and-a-folder-grants-nothing.md)).
A folder's own visibility is its **top-level folder's**: that folder's owner
and its shares, a preset chosen when it is made (`company`, everyone in the
company as `contributor`, or `private`, its owner alone). So a tree is seen
whole or not at all, a breadcrumb never names a folder its reader cannot see,
and a folder the caller cannot see is answered exactly as one that does not
exist.

| Method and path | Verb | What it does |
|---|---|---|
| `POST /artifact-folders` | `create_artifact_folder`, and `artifacts.folder.file` on the parent | `{"name", "parent"?, "visibility"?}`, or `{"path": "Q3/Week 1", "parents": true, "parent"?, "visibility"?}` to create a path with its missing parents in one record (`mkdir -p`). A top-level folder states `visibility` (`company` or `private`) and is a person's to make; an agent creates folders only inside one. Answers `201` with `{folders, created, op_id, outcome, position}`, the chain parents first, and `200` with `created: 0` when every name was already there |
| `PATCH /artifact-folders/{id}` | `artifacts.folder.rename`, `artifacts.folder.move`, and `artifacts.folder.file` on a new parent | `{"name"?, "parent"?, "confirm_widen"?}`: a rename or a move (`"parent": ""` to the top level), one a request, since they are two records and a move refused after a rename would leave the rename behind; a body naming both is `422 invalid`. A move into a tree whose audience reaches somebody the folder's present one does not (a private folder moved under a company one) is `409 folder_widens` until `confirm_widen` is sent. Answers `{folder, op_id, outcome, position}` |
| `DELETE /artifact-folders/{id}` | `artifacts.folder.remove` | Removes the folder: its child folders and every artifact in it, live and trashed, move up to its parent in one record. Nothing is trashed and nobody loses access. A child whose name its new place already holds is `409 folder_name_taken`; the answer carries no counts, which would include artifacts the caller cannot see |
| `PUT /artifacts/{id}/folder` | `move_artifact` (editor) on the artifact and `artifacts.folder.file` (contributor) on the folder | Files one artifact; to the top level from a folder it is `move_artifact`, or the folder's manager's `artifacts.folder.unfile` for an artifact they can read. A move is not an edit: the artifact's `updated_at` stays, and its history row names the folders by id only |
| `POST /artifacts/folder-moves` | as above, per item | Up to 25 artifacts, each decided and written alone: one item's refusal is named in its result (`outcome` is `moved` or the refusal's code, with a `message`) and stops nothing else |

**Who manages a folder.** Seeing a folder and filing into it are decided on its
top-level folder; renaming, moving and removing it and taking an artifact out
of it are decided on the folder's **own owner**, and are a person's: its owner,
the owner's lead where the owner is an agent, or an administrator, recorded as
the admin path. The tree answers each folder's `role` for the caller (`owner`,
`contributor` or `viewer`) from those decisions, and a folder's `shares` only
to its managers. An administrator is never shown a private folder: the admin
path lists no folder, as it lists no artifact.

**Names.** A name is 1 to 80 bytes, trimmed and NFC, with no `/`, no control,
bidi or zero-width character, and not `.` or `..`. Siblings are unique
without regard to case. At the top level the company's folders are unique
among themselves and each owner's own among their own, so a name is never
refused because of a folder its creator cannot see; a path naming both a
private and a company folder is `422 folder_ambiguous` on a write (`candidates`
lists each by id) and `400 bad_params` on the listing: list it by `folder` id.
Folders nest at most four deep; a company holds at most 2000 live folders and
one folder at most 1000 children.

**Listing by folder.** The listing places every row in the tree as its reader
sees it: an artifact filed in a folder the reader cannot see is at their top
level, never hidden by it, and its `folder` and `folder_path` are answered only
where they see that folder. One artifact's head and its history's moves
(`from_folder`, `to_folder`) name a folder on the same rule, so a folder's id
reaches nobody it is not shown to. A `prefix` or `folder` that names nothing the caller sees is
`404 not_found`, never an empty page. The continuation token carries the
folder's id rather than its path, so a rename between two pages does not move
the next one, a token is held to the delimiter it was minted under, and a
query naming another folder beside a token (by `folder`, or by a `prefix` that
names a folder now) is refused rather than answered as the token's folder. Each
common prefix is `{prefix, folder, name, visibility, items, folders}`, counting
only what the reader may open, by the same filter the page and its `total`
are counted by.

### Viewing a version

The API origin **never renders an artifact**:

- `POST /artifacts/{id}/tickets` (`{"version": n, "document": "path"}`, the
  head by default) mints the frame the dashboard embeds a page in, or one
  artboard of a design: `frame_url` on the artifact
  origin under a one-hour view ticket bound to the caller's own sign-in or
  token, the `origin` its bridge must hear from, the `sandbox` and `allow`
  attributes the dashboard sets verbatim, and `capabilities`, what the
  ticket grants in the canonical order (`[]` for none): the origin writes
  that grant on the page's runtime element, and the dashboard's host tells
  the page the same list at the handshake, never one it works out from the
  version or a gallery item. The page is re-authorized on every
  document request there, so a withdrawn share, a sign-out or the trash closes
  it at once. `document` names the page the frame opens: left out for a
  page's entry, and always one artboard's path for a design, whose ticket
  then opens that page and no other as a document. A `document` that is not
  an HTML page of the version (a design's `canvas.json`, a stylesheet, a path
  the version does not hold, or none for a design) is `422 invalid`. A
  doc, a deck or a sheet is `409 artifact_not_framed`, and
  a deployment with no `api.artifacts.external_url` is
  `409 artifact_origin_unset`. Minting a frame records that the caller opened
  the artifact, at most once in fifteen minutes.

  **`?purpose=preview`** mints a gallery card's live preview: the same read
  under the same authorization as an open (a reader who may not open the
  artifact may not preview it, and an administrator's preview of an artifact
  only the admin path admits is still recorded as the elevated read it is),
  and nothing more than a read. A preview records no open and changes no Seen
  by list; its ticket carries the purpose, is granted only `assets`, `db`
  and `state` of what the version declares (never `identity` or `presence`,
  so the page's runtime offers no viewer and no room), and lives at most five
  minutes. The dashboard answers a preview's `db` with a read-only host: a
  document, a page, a query, the schema, and a subscription answered once
  with the collection as it stands, each read as the reader through the
  datastore's own questions; every write and undo is refused, nothing is
  pushed after a subscription's answer, and no viewer is named. It answers a
  preview's `state` as a newcomer's: empty at first, what the page sets kept
  by the host for the frame's life (at most 64 keys of 16 KiB each), never
  the reader's own state and never sent to the engine;
  and its frame files no ready report (`POST /artifacts/ready` refuses one as
  `422 invalid`). A purpose this build does not know is `422 invalid`, never
  read as an open.
- `GET /artifacts/{id}/versions/{n}/source` answers a version's entry as JSON
  (`text`, `content_type`, `size`), read back and checked against its digest:
  how the dashboard reads a design's `canvas.json`, which it draws natively.
  Reading a design's source records that the caller opened it (its canvas is
  drawn whether or not any artboard is framed).
- `GET /artifacts/{id}/versions/{n}/deck` answers a deck's version (a
  `slides` artifact) as its **canonical tree**: the title, the theme, the cover, the
  outline's sections, the typefaces and every slide in order, each its id,
  title, notes, transition, hidden flag and the `<section>` as elements,
  attributes and canonical style declarations, read through the same
  [slide format](slide-format.md) parser the publish was held to. The
  dashboard draws, presents and prints a deck from this answer alone and never
  parses a slide's markup. Reading it records that the caller opened the deck,
  as a frame's ticket does for a page. Any other kind is `409 content_refused`.
- `GET /artifacts/starters/slides` (`list_artifacts`, everybody's) answers
  what a deck can begin from: `themes`, each its `id`, `name`, `tag` and
  `sample` (a deck of every layout once, drawn in that theme, as the same
  canonical tree a version's deck is answered as), `layouts`, each its `id`,
  `name`, `description` and `heading` (the title a slide of it opens with), and
  `default`, the plan a starter opens with when it names none. Whenever the
  company has adopted a brand, its theme (`brand`, named for the brand, its
  colours and typefaces resolved from the adoption) comes first, its sample's
  cover drawing the logo a deck begun in it shows, at the path the starter
  copies it to, with `logo` (`{path, slot}`) naming that path and the adopted
  brand's file slot it is drawn from, since a sample holds no bytes; the answer is
  composed per request, so an adoption or a clear shows at once. The create page
  draws its theme picker and preview from it, and the deck editor adds a slide
  from the sample of the theme its deck's index names (see
  [Slide Format](slide-format.md#the-index)).
- `GET /artifacts/{id}/versions/{n}/doc` answers a doc's version (a `doc`
  artifact) **tab by tab**: `indexed` (whether it opens on a `doc.json` index
  rather than one Markdown file) and every tab in the index's order, each its
  `path`, `title` and Markdown `text`, read through the same
  [doc format](../guides/artifact-authoring.md#writing-a-doc) the publish was
  held to. This is how the dashboard draws a doc, which runs no author code:
  with its own element-constructing renderer, never as markup. Reading it
  records that the caller opened the doc. Any other kind is
  `409 content_refused`, and so is a deck, a doc or a design whose version no
  longer reads in its format: said in words that it can't be shown and that
  publishing it again fixes that, with the format's own findings in the
  node's log.
- `GET /artifacts/{id}/versions/{n}/outline` answers a version's structure
  without every file: for a deck, its title, slide count, cover, sections and
  each slide's id, title and hidden flag, plus the cover slide's tree; for a
  doc, each tab's `path`, `title` and size in `bytes`, plus the `cover`: the
  first tab's opening as Markdown, cut at a line boundary within 1 KiB; for a
  dashboard, its title, every widget's id, type, title, width and dataset, and
  its first three stats' labels with their datasets' titles (never a value,
  which is each viewer's); for a design (`design`), its title, board count,
  cover board, the `bounds` every board and note fits in, each board's `path`,
  `title`, `x`, `y`, `w` and `h` in the canvas's order and the number of
  `notes`, read from `canvas.json` alone; for a brand kit (`brand`), its whole
  `brand.json` as `brand.document`, read under the brand format again. The
  gallery draws each as the card's cover (a design's as a schematic). Any other
  kind answers its kind alone. A
  cover is not an open, so none is recorded. A node keeps the decks, docs and
  canvases it has read by version (16 MiB of their files at most for each),
  and the boards (64 MiB of their spec and file datasets), since a version
  never changes.
- `GET /artifacts/{id}/versions/{n}/dashboard` answers a dashboard's version
  (a `dashboard` artifact) as its **board**: the spec held to the
  [dashboard spec](dashboard-spec.md) format again on read, and every dataset's
  shape (its columns and key), how it is kept current (`live`, `signal`,
  `interval`, `manual`, `fixed` or `derived`, with the collection, the push or
  the interval it follows) and, for an engine dataset, the catalogue's words
  for what it reads and who may read it. It carries **no row**: every reader is
  answered the same board. Reading it records that the caller opened the
  dashboard, unless the request carries `?purpose=preview` (a gallery card's
  preview, as for a frame: the same board, the same authorization, no open
  recorded). Any other kind is `409 content_refused`.
- `GET /artifacts/{id}/versions/{n}/sources/{dataset}` answers one dataset of
  a dashboard **as its viewer** ([ADR-0043](../../adr/0043-a-dashboards-live-data-is-read-as-its-viewer.md)):
  the query string is the board's filters, each refused unless the board offers
  it and the value is one of its choices (`400 bad_params`). An `engine`
  dataset is answered by the same question the dashboard's own screens ask, on
  the caller's own request, so a viewer without `worklet` is never handed a
  task; a `store` dataset is read under the collection's read rule; a `calc`
  dataset is computed from its inputs read the same way. The answer is
  `{artifact, version, dataset, status}` and: for `ready`, `table` (`columns`,
  `key`, `rows`, `truncated`) and `as_of`, when the rows were true; for
  `refused`, `refusal` (the `dataset` whose source refused, which for a
  calculation may be an input, the rule's `reason` and the `grants` that
  would admit the caller) and a `detail` sentence; for `unanswered`, a
  `detail` saying why this node cannot answer it for this caller (a domain it
  does not run, a personal scope for a credential that names no person). An
  engine answer is kept for 30 seconds per principal and arguments; a node
  answers at most two live reads a second per viewer on one artifact (a burst
  of 64) and twenty a second for everybody on it (a burst of 320), past which
  it is `429 read_budget` with a `Retry-After`. A read this node could not make
  for a condition of its own is `503 unavailable`, its `detail` the
  condition's words and its `Retry-After` the condition's (none where no wait
  clears it); a failure that is no condition is `500 internal_error`, its
  reason in this node's log.
- `GET /artifacts/sources` answers the **engine source catalogue** a
  dashboard's `engine` datasets may read: each source's name, domain, purpose,
  who may read it, its typed parameters, its columns and key, the push that
  announces a change and the most rows it answers. Any resolved caller.
- `GET /artifacts/sources/preview/{name}` answers one catalogue source's rows
  **as the caller**, before any board exists: what the dashboard's New
  dashboard builder draws its suggested figures from. Decided by
  `artifacts.source.read` (the `artifacts` app, as making a board asks), then
  answered by the very read a board's `engine`
  dataset over the source is: the same question on the caller's own request,
  the same 30 second per-principal cache, and at most the source's most rows.
  The query string is the source's parameters, each held to the catalogue's
  type and bounds and every one left out at its default; a parameter the source
  does not take, a value outside its type or a key sent twice is
  `400 bad_params`, and a name the catalogue does not list is `404 not_found`.
  The answer is `{source, status}` with `table` and `as_of`, or `refusal` and
  `detail`, exactly as a dataset's. It reads no artifact and records nothing.
  One caller's previews are budgeted as their reads of one board are (two a
  second, a burst of 64), past which it is `429 read_budget` with a
  `Retry-After`.
- `GET /artifacts/{id}/versions/{n}/files/{path}` streams one file as an
  **attachment**, any text and anything a browser could run (HTML, SVG, a
  script) as `text/plain`, under the API's own `default-src 'none'` policy and
  `Cache-Control: private, no-store`, with its SHA-256 as a strong `ETag`.
- `POST /artifacts/ready` (`artifacts.ready`, everybody's) is the dashboard's
  **ready report** for the [artifact-ready objective](service-objectives.md):
  `{"frame_url", "outcome": "ready" | "failed", "elapsed_ms"}`, the frame
  exactly as the ticket route answered it, how it ended (the page's bridge
  hello, or a frame that failed or never said hello), and the milliseconds
  since the frame was handed over, by the browser's clock. Answered `204` once
  counted. The frame must be a live one this deployment minted for the
  caller's own sign-in or token (`422 invalid` otherwise, the same answer as a
  URL that is no frame); each frame reports once (`409 conflict`), never a
  time longer than the frame has existed (`422 invalid`), and one credential
  sends at most a sustained report a second after a burst of thirty
  (`429 throttled`, with `Retry-After`). A deployment with no
  `api.artifacts.external_url` is `409 artifact_origin_unset`.

### The company's brand

A brand kit is an artifact of kind `brand` (its `brand.json` and the logos and
faces it names, held to the
[brand format](../concepts/artifacts.md#the-company-brand) at publish; a draft
needs only its `name`). The
company's brand is the one version of one kit an administrator **adopted**:
the adoption snapshots that version's index, the stylesheet generated from it
and a copy of every file it serves, so publishing to the kit, trashing it or
purging it changes nothing until somebody adopts again. What is adopted is the
company's public face: everybody reads it.

| Method and path | Verb | What it does |
|---|---|---|
| `GET /brand` | `get_company_brand` (everybody's) | The adopted brand, or `{"brand": null}`: the `kit` and its `kit_version`, the `document` (the adopted `brand.json`), the generated stylesheet's `css_sha`, every served file in `files` (`slot`, `content_type` and `object` as `{hash, size}`, the stylesheet first), `adopted_by` and `adopted_at`. `newer_version` and `newer_by` name the kit's newest version and its publisher where it is newer than the adopted one, answered only to a reader who may open the kit (by a share, or as an administrator, which reads no content and leaves no elevated-read record). Also the `brand` question on the socket |
| `GET /brand/files/{hash}` | `get_company_brand` | One file an adoption serves, by its SHA-256: an **attachment**, an SVG as `text/plain`, under the API's own policy, as every artifact file is. A hash no current or retained adoption serves is `404` |
| `POST /brand/adoptions` | `artifacts.brand.adopt` (an administrator, a person present, a recent proof) | `{"kit": "<id>", "version": <n>}`: adopt that version. Reading the kit is decided as any content read, so an administrator with no share on it is admitted on the admin path and the read recorded. The kit is read back and held to the brand format (`422 policy_refused` with its `findings`, code `brand_format`). A brand that leaves out `logos.primary` or `colors.primary` is `422 policy_refused` with one `brand_format` finding per missing field (its message `field: <name>: is required to adopt the brand: ...`) and `missing`, the bare field names in that order; only a brand that holds both is measured for contrast: its text colour on its light ground must reach 4.5:1 (`422 brand_contrast`, with `contrast`: the pair, its two colours, the unrounded `ratio` and the `min`), and a version the kit no longer keeps is `404 not_found`. The adoption also labels the version `Adopted <date>` where that label is free. Carries an `Idempotency-Key` |
| `DELETE /brand/adoptions/current` | `artifacts.brand.clear` (an administrator, a person present, a recent proof) | Withdraw the adopted brand. A framed page pinned to its stylesheet keeps it while its version is retained. Clearing when nothing is adopted answers success and writes nothing. Carries an `Idempotency-Key` |

A framed version (`html`, `design`) **pins** the stylesheet adopted when it is
published: its `versions` answer carries it as `brand`, and the artifact origin
places `<link rel="stylesheet" href="/_crewlet/brand/<sha256>.css">` at the top
of every document of that version it serves, leaving the author's bytes as
they are. A page whose `<html>` element carries `data-crewlet-brand="off"` is
served without it, and an `html` version whose entry page says so pins none. A
deck copies the brand into its own files when it is created, and a doc and a
dashboard follow the adopted brand live, drawn by the dashboard.

### Refusals

| Status | `error` | When |
|---|---|---|
| `400` | `bad_if_match` | `If-Match` is not `"<n>"` |
| `400` | `op_id_invalid` | No `Idempotency-Key`, or one outside the operation-id grammar |
| `403` | `unauthorized` | The authority table refused; `reason: role` names the `role` needed |
| `403` | `token_refused` / `step_up_required` | A machine token on a verb that needs a person present, or a proof of identity older than the verb's window |
| `403` | `forbidden` | A share above the sharer's own role, or the company from an agent |
| `404` | `not_found` | No such artifact for this caller, or no such version, asset or file |
| `404` | `upload_not_found` | No such upload of this caller's |
| `409` | `artifact_trashed` | The artifact is in the trash |
| `409` | `artifact_purging` | A restore of an artifact whose purge has begun; the purge finishes on its own |
| `409` | `carried_object_gone` / `upload_missing` | A carried file's bytes are gone, or a declared file has not arrived; `paths` names them |
| `409` | `upload_consumed` / `upload_expired` | The upload is used, or an uploaded object is too old to name; upload again |
| `409` | `label_taken` / `source_taken` | Another version holds the label, or another of the owner's artifacts the source address: a collision, which another label or address avoids now (the tools' class is `exists`) |
| `409` | `exists` | A create of an artifact or an asset that is already there — most often the same create sent again after its operation was forgotten: read it rather than make it again |
| `409` | `labels_full` | Every label the artifact keeps is in use: remove one from a version that no longer needs it, then label this one (the tools' class is `conflict`, the artifact's state) |
| `404` | `folder_not_found` | A folder a body names (a parent, a destination, the folder a create files into) that does not exist, was removed, or that the caller cannot see; one answer for all three |
| `409` | `folder_name_taken` / `folder_limit` | A name its place already holds (the tools' class is `exists`), or past the company's or one folder's bound (the tools' class is `conflict`: the state of what the company holds) |
| `409` | `folder_widens` | A move that would show a folder to more people, unconfirmed |
| `422` | `folder_cycle` / `folder_depth` / `folder_ambiguous` | A folder moved into itself or beneath itself, a folder deeper than four, or a path naming several folders the caller sees (`candidates`): the argument to change, as the tools' class `invalid` says |
| `409` | `artifact_not_framed` / `artifact_origin_unset` | See [Viewing a version](#viewing-a-version) |
| `409` | `conflict` | A frame whose ready time was already reported; see [Viewing a version](#viewing-a-version) |
| `410` | `upload_expired` | The upload's window closed before it was finished |
| `412` | `version_moved` | The head moved past the base; see [Publishing a version](#publishing-a-version) |
| `409` | `quota_exceeded` | The company's quota; `key` names the Tier A setting an operator raises. A `409` because it is the state of what the company holds that the write collided with: no smaller request changes it, and every surface answers it so — the tools' class is `quota_exceeded` too |
| `415` | `media_type_refused` | A file type an artifact may not publish |
| `422` | `upload_mismatch` / `content_refused` | Bytes that are not the declared ones, or not what their type promises |
| `422` | `edit_refused` / `policy_refused` | An edit that matched zero or several times (`index`, `matches`), or a page the publish policy refuses (`findings`) |
| `422` | `policy_refused` | An adoption of a brand that leaves out a field an adopted brand holds (`findings`, `missing`) |
| `422` | `brand_contrast` | An adoption whose text colour does not reach 4.5:1 on its light ground; `contrast` names the pair |
| `422` | `capability_unknown` / `contract_unknown` / `credential_refused` / `invalid` | A capability or runtime this build does not serve, a credential in what an agent published, or a value the domain refuses |
| `429` | `uploads_outstanding` | The pending-upload budget is spent; `Retry-After` is when the earliest hold lapses |
| `429` | `throttled` | Ready reports past one credential's rate; `Retry-After` is when the next is admitted |
| `503` | `unavailable` | This node could not decide or reach the estate, or the log refused the write; a `Retry-After` says when to come back, and comes only where waiting clears it |

## Ask Crewlet

Ask Crewlet's routes, all under `/assistant/`. Every route is guarded
and mounted on the authority table with one of two verbs, `assistant.converse`
(starting, asking, stopping, clearing) and `assistant.conversation.read`
(listing, reading, streaming, exporting). Both are human-only and need a person
present, so a machine token, a service account and a request riding an
assistant grant are refused before a handler runs, and a Tier A token's session
is answered `403 assistant_not_available` with `reason: not_a_person`.
Ask Crewlet acts as the person and nobody else: every tool call a turn makes is an
ordinary request through this API's own handler, carrying a read grant derived
from the request that asked the question, which ends with the turn, with the
session, or after twenty minutes (the longest a turn may run), whichever is
first.

**Any route of this reference, as the person.** Beside its dedicated tools,
Ask Crewlet reaches every route this page documents that the person may use:
it lists them and reads one route's entry from this very page, which the engine
embeds, sends a `GET` or `HEAD` on its read grant, and proposes a `POST`, `PUT`,
`PATCH` or `DELETE`, which is sent only from the person's approval of a card
the engine renders from the resource as the route serves it now. A call names
a route by its method and pattern, never a URL, and each path value is one
escaped segment. A proposal prepared against a resource whose `ETag` the route
answers is sent under that tag as its `If-Match`, so a change made meanwhile is
refused rather than overwritten. Never through Ask Crewlet, whoever
approves: the sign-in, session and token ceremonies, a credential or secret in
clear (a secret's value, an integration's inputs, a frame's view ticket), and
the routes on this section's own surface.

**Somebody else's conversation does not exist.** Every read and write is keyed
on the caller, and a conversation, turn or card that is another person's is
`404 not_found`, exactly as one that never existed. No route takes an owner.

Every `POST` takes `application/json` and names the `protocol_version` it was
written against; a version this engine does not speak is
`409 assistant_protocol_mismatch` with the hint to reload the dashboard.

| Method | Path | Body | Answer |
|---|---|---|---|
| `GET` | `/assistant/conversations?cursor=&limit=` | | `200 {conversations, next_cursor?}`, the most recently used first, `limit` 1 to 50. Each is `{id, title, created_at, updated_at, last_activity_at, last_turn_state?}`: the order is `last_activity_at`'s (the last turn, message, step, card or proposal), which a client shows as the time a conversation was last used; `updated_at` also moves on a new title |
| `POST` | `/assistant/conversations` | `{protocol_version}` | `201 {conversation}`. An `Idempotency-Key`, when sent, names the conversation, so a retried create is one conversation |
| `GET` | `/assistant/conversations/{id}` | | `200` the transcript: `{conversation, messages, steps, cards, open_turn, audit_reads}`. A card built from an app the caller no longer holds is replaced by a notice, and so is a message written after its turn read through one (each message records the apps its turn had read through); every step names its `turn_id` |
| `DELETE` | `/assistant/conversations/{id}` | | `200 {outcome, position}`: the conversation is purged at once, a turn of it running here is stopped and every grant of its open turn is ended |
| `POST` | `/assistant/conversations/{id}/turns` | `{protocol_version, input: {text}, page_context, mode?, attachments?}` | `202 {turn_id, stream_url, after_seq}`. `Idempotency-Key` is **required**, and the same key is the same turn. `attachments` names at most 4 files uploaded to this conversation, each once (`400 invalid_body` otherwise). `mode` is `ask` (the default) or `brief` (see [Briefs](#briefs)), which needs a `page_context` and takes no `attachments` (`400 invalid_body` otherwise) |
| `GET` | `/assistant/conversations/{id}/stream?turn=<id>` | | `200 text/event-stream`, or `204` when the segment has ended and nothing follows `Last-Event-ID`, below |
| `POST` | `/assistant/turns/{id}/stop` | `{protocol_version}` | `202 {state}`: a running turn is stopped where it runs, on whichever node of the fleet that is, a turn waiting on an approval is closed stopped and every proposal it waits on declined (under the request's `Idempotency-Key`, when sent), and a turn already over answers the state it ended in. `state` is what the turn truly reached: `stopped`, or what it came to first (`completed`, `waiting_for_approval`), and `running` while its runner has not yet recorded its end, which then arrives on the turn's stream |
| `GET` | `/assistant/cards/{id}/export.csv` | | `200` the table card as CSV, from the snapshot the conversation stored, never a fresh read |
| `GET` | `/assistant/cards/{id}/export.xlsx` | | `200` the same table as XLSX |
| `POST` | `/assistant/conversations/{id}/files?name=` | the image's bytes | `201 {file: {file_id, name, media_type, size, download}}`: one image the person attaches to a later question, the raw file as the body (declared `image/png`, `image/jpeg`, `image/webp`, `image/gif` or `application/octet-stream`) and its name in `name`. `Idempotency-Key` is **required**, and the same key is the same file. The type is read from the bytes and never from the name or the declared type, and only PNG, JPEG, WebP and GIF are kept (`415 assistant_file_refused` otherwise, HTML called `.png` included); a body over 5 MiB is `413 body_too_large`, refused by its declared length before it is read; a conversation keeps at most 40 attachments (`409 assistant_file_limit`). The file is sealed under the fleet keyring into the [object store](../concepts/object-store.md) and goes with its conversation. An upload the store did not take, or refused because the company's file storage is full, is answered as [a project file's](#project-files) is. Absent on a node with no object store |
| `GET` | `/assistant/conversations/{id}/files/{file}` | | `200` one file the conversation keeps (an attachment, or a file a deep-work run delivered), always as a download: `Content-Type` is the type its bytes were read as, `Content-Disposition: attachment; filename="..."` (with RFC 5987's `filename*` for a name that is not plain ASCII), `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox` and `Cache-Control: private, no-store`. Another person's file, or one of another conversation, is `404 not_found` |

`input.text` is at most 4000 characters, and `page_context` is the open screen
as the dashboard describes it (at most 16 KiB, 50 resources and 32 page actions)
or `null` when the person removed the page chip. The model reads the page
context fenced as somebody else's words: it is a hint and never an authority.

Beside `accepts`, the page context carries `action_args`: what each accepted
page action takes on that screen now, by action id, as a closed subset of JSON
Schema (`type`, `description`, `enum`, `const`, `minLength`, `maxLength`,
`minItems`, `maxItems`, `items`, `properties`, `required`,
`patternProperties`, `additionalProperties`, `oneOf`, and top-level
`examples`). The screen builds it from the same declaration its parser reads
(the address keys a view change writes, the seats and windows a screen draws,
the org builder's operations), so the model is told the members to send rather
than guessing them. Every accepted action declares it (`draft.set` may go
without, since the draft's editable fields declare what it takes). The engine
reads it strictly: an unknown keyword, a pattern RE2 cannot compile, a schema
past 6 KiB, 8 levels or 160 parts, or an example its own schema refuses is
`400 invalid_body`. Before a `propose_page_change` call is previewed or
recorded, its `args` are held to that schema, and one that does not match is
the model's refused step, naming the members the screen takes, never a card.
An action the screen declares no arguments for is never proposed.

`selection` is the one record the person has open, or `null`: `{kind, id,
noun?}`, the row a screen has open beside its list, else the peeked row, else
the record a record's own screen is of (a task's page is that task, a seat's
that agent). `noun` is what the person calls it where its kind does not say:
`decision`, `update`, `task`, `project`, `agent`, `artifact`, `page`, `turn` or
`run` (an ask open in the Inbox is a `decision` on its task); any other value is
`400 invalid_body`. A brief is limited to the selection.

`action_revisions` gives each accepted action its own revision: the screen and
its path, and only the parts of the page the action depends on (its address,
its selection, its data). A page action's proposal is bound to its action's
revision, and its claim names the revision of that action, so a change goes
stale only when what it depends on moved, never because the person opened a
row's peek or another editor beside it. An action without one is bound to
`source_revision`.

A screen with a draft or a form open adds `draft`: `{kind, id?, revision?,
step?, help?, fields, more}`, where `kind` is `client` (the values live in the
page until saved) or `server` (saved, at `id` and `revision`), and each field is
`{path, label, kind, value, hint?, editable?, redacted?, truncated?}`, `kind`
being `text`, `list` (items one per line) or `file` (the value is the file's
name). At most
40 fields, each value at most 1000 characters (a longer one is cut and marked
`truncated`), and `help` at most 600; fields past the count or the byte budget
are counted in `more`. A field whose path names a credential (a `password`,
`secret`, `token` or `credential` segment, or an API, private, signing or access
key) must arrive `redacted` with an empty value, and is refused `400` otherwise;
every other value passes the engine's secret redaction before a model reads it.
The page action `draft.set` (`{"values": {"<path>": "<value>"}}`) changes the
editable fields through the screen's own setters, after the person approves.
The engine holds it to the turn's draft before any card is drawn: a path the
draft does not list as editable, or a value not of its field's kind, is
refused. A `file` field takes a file, never markup: the model names it by its
`file_id` alone (a file of the person's own in this conversation, attached or
delivered by deep work), and the engine resolves the rest from its own record
and writes the whole reference into the proposal,
`{"kind": "file", "file_id", "conversation_id", "name", "media_type"}`. A file
of another conversation or another person is refused before any card. The
screen fetches the file through
`GET /assistant/conversations/{conversation}/files/{file}` once approved and
keeps it through its own save.

**Before anything is spent.** A question is refused, recording nothing, when the
company's token budget, or the person's own `assistant.token_budget`, has no
room in a window (`429 budget_exhausted` with the `scope` that is spent, `org`
or `person`, the window, its `resets_at`, a `Retry-After` until it resets, and
the `remedy` naming the ceiling that refused it: `{"setting":
"company_token_budget"}` or `{"setting": "assistant_token_budget"}`; see
[ADR-0060](../../adr/0060-a-caveat-or-a-refusal-is-worded-for-each-reader-and-a-person-is-never-shown-a-models.md)),
when the person already has two turns running on the node, or has asked twenty
in a minute across every node of the fleet (`429 assistant_busy` with a
`Retry-After`; while the fleet's counter cannot be read, the node's own rate of
twenty a minute, five at once, still holds), when the conversation already has a
turn running (`409 assistant_turn_running`), when no model in the chain can
be resolved (`503 assistant_unavailable`), and when every model in it is known
unusable right now, the state `/viewer` reports as `model_not_ready`
(`503 assistant_model_not_ready`, with `reason`, the turn error code it would
have failed with, `detail` and `hint` in the words a failed turn would use,
and `retry_at` with a `Retry-After` only for a spent plan whose reset is known:
a signed-out model needs somebody to sign it in, not a wait). A question asked while the
conversation's turn waits on an approval is taken: that turn ends as
`superseded`, and its proposals stay approvable until they expire. One approved
later is applied and its card updated on that turn's stream, without the
superseded turn's answer resuming.

### Briefs

A turn asked with `"mode": "brief"` answers with one `brief` card rather than
prose: a brief of the page, or of the page context's `selection` where it names
one. It is a turn like any other (admitted, metered, recorded and streamed the
same way, the first answer of a conversation the person goes on asking in),
with these differences:

- It only reads: the model is offered the person's read tools and its answer
  tool, no write, page action, link or deep work. Its reads draw no card and
  its prose is never drawn; its steps are shown as any turn's.
- The card is `{"type": "brief", "brief": {scope, summary, needs, recommended?,
  follow_ups, as_of}}`. `scope` is `{screen, title, record?}`, `record` the
  selection the brief was limited to. `needs` holds at most three rows,
  `{ref: {kind, id}, text, detail?, tone, action?}`, and `[]` is "Nothing needs
  you". `tone` is `warning`, `neutral` or `danger`. An `action` is `{kind,
  label, link?, question?}`: `open` carries a `link` the engine built from its
  link table (`href` a dashboard address), `ask` a `question` to ask in the same
  conversation. `follow_ups` holds at most three questions. Lines are plain
  text: a summary of at most 200 characters, a row's text and detail of at
  most 120 each.
- A row's `ref` is a `task`, `project`, `seat`, `page` or `artifact` that this
  turn's own reads returned and whose own read, sent again as the person,
  answers it; any other row is left out. `danger` is drawn only where that read
  says a seat is stopped or its last turn failed, or a task is urgent; the
  model's other dangers are drawn `warning`.
- A brief the model could not write within one retry ends the turn with the
  error code `brief_invalid`; one that read more than its own token cap ends
  with the same code and a sentence saying so. Neither draws a card.

### The turn's stream

A server-sent event stream read over `fetch`. Every frame is one event: `id` is
`<turn_id>.<seq>`, `event` is the frame's kind (`conversation`, `page_context`,
`status`, `delta`, `step`, `card`, `action`, `done`, `error`, `snapshot`), and
`data` is the frame as one line of JSON with its payload nested under a member
named for its kind. `seq` counts from 1 and only grows within a turn, across the
segments an approval resumes. A `: keepalive` comment is written every fifteen
seconds.

Send `Last-Event-ID` to resume: every frame after it is replayed. A client that
falls 512 frames behind is disconnected rather than dropped from, and resumes
the same way. A resume after the last frame of a segment that has already ended
(a turn waiting on an approval, reopened at the transcript's
`open_turn.after_seq`) answers `204 No Content` with no body: the client holds
the whole segment, its `done` included, and does not dial again until a
decision hands it the next segment's `stream_url`. Frames the running node no longer keeps (it keeps a turn's frames
two minutes after a segment ends) are answered with a `snapshot` of the turn as
its transcript holds it, then how it ended.

A person holds at most four streams open on a node, and opens at most sixty a
minute (ten at once) on a node and across the fleet; past either a stream is
refused `429 assistant_busy` with a `Retry-After`, before anything is read.

**Any node answers the stream.** A stream that reaches a node not running its
turn follows it there: the frames it missed are asked of the running node, and
the rest are heard as the running node tells the fleet, numbered as the running
node numbered them, so a reconnect that lands on another node resumes exactly
where it left off. The numbering continues across nodes too: a turn an approval
resumes, or whose page action it emits, on a node that never ran it is numbered
after every frame any node told of it, so `after_seq` and `Last-Event-ID` stay
meaningful wherever the next request lands. Frames cross the broker encrypted under the fleet keyring,
bound to their turn, and a node shows only a frame the fleet sealed. A turn whose
runner is gone (its node restarted, or has left the fleet) is closed
`interrupted` once, by whichever node notices first, and answered with
`error {code: turn_interrupted}`; a runner that is only slow to answer leaves the
turn open, and the stream answers the `snapshot` it can.

A `delta` appends prose to one assistant message while the model writes it, a
settled line at a time: nothing of a line still being written is sent, so a
credential is never on screen before the end that would redact it arrives. A
delta with `restart: true` replaces everything the message drew with its own
text; it is sent when the chain fails over to another model, or when the engine
withdraws a view of a round that the finished round does not bear out, so the
message always ends as exactly the answer recorded. A model whose harness cannot
stream draws each round once, whole, when the round ends.

Each round that writes prose is its own assistant message, in order, and a
`card` names the message it follows: the round that asked for it, or the last
one that wrote prose before it. So a round's prose sits above the cards its
calls showed and the final round's below them, and a reload's transcript lays
the turn out exactly as it streamed. A card shown before the turn has written
any prose hangs on an empty message opened for it (a `delta` with no text).

A turn never streams a raw tool argument, a raw answer, a prompt, a token count
or a grant: a step carries a label the engine renders and the route pattern it
asked, and a card is built by the engine from the route's own answer.

A step or a card a request was refused on carries the refusal as a person
reads it, `{code, message, hint?, reason?, grants?}`. `message` is the
engine's sentence for that kind of refusal, never a `detail` the route wrote
for a model, which names tools and fields and goes to the model alone: where
the [authority table](#which-permission-a-route-needs) refused, the sentence
the envelope says for the rule its `reason` names; where the route answered
the refusal in a person's words (its `detail`, ADR-0060), those words;
otherwise one chosen by the code, and by the status for a code with
none of its own. A `409` is a race only where its code is one
(`stale_version`, `version_moved`, `revision_advanced`, `proposal_stale`,
`stale`): the state of what a write is about, such as `artifact_trashed` or
`quota_exceeded`, says what it is. `hint` is the engine's own next step where
it has one. `reason` and `grants` (the rule that refused, and the permissions
any one of which would have admitted you) are there where the authority table
refused, and the grants alone never say whether access is the way past: a
`step_up_required` refusal names the grants that admitted you.

| Status | Code | Meaning |
|---|---|---|
| `400` | `invalid_body` / `invalid_query` / `op_id_invalid` | A body, query or `Idempotency-Key` refused as written, naming what to change |
| `403` | `assistant_not_available` | Ask Crewlet is not available to the caller; `reason` is the viewer's |
| `404` | `not_found` | No such conversation, turn or card, or another person's |
| `409` | `assistant_protocol_mismatch` | The request speaks another protocol version; reload the dashboard |
| `415` | `unsupported_media_type` | A body not declared `Content-Type: application/json` |
| `409` | `assistant_turn_running` | The conversation's turn is still running |
| `409` | `assistant_file_limit` | The conversation already keeps 40 attachments; `limit` names the bound |
| `413` | `body_too_large` | An upload past 5 MiB |
| `415` | `assistant_file_refused` | An upload whose bytes are not a PNG, JPEG, WebP or GIF image, or declared as something no image is |
| `429` | `assistant_busy` / `budget_exhausted` | Too many turns at once, or a window of the company's or the person's own budget is spent (`scope`); `Retry-After` says when |
| `500` | `internal_error` | A fault of this node, which trying again doesn't clear; the reason is in its log |
| `503` | `unavailable` | A condition of the node (a copy behind its log, a log at its byte ceiling, a store or a broker that did not answer), told in `detail` in a sentence for the person, with a `Retry-After` exactly where waiting clears it. With `"outcome": "unknown"` and the `op_id`, a write whose outcome nobody can establish: send it again with the same `Idempotency-Key` |
| `503` | `assistant_unavailable` | Ask Crewlet can't run here for a reason of its own: no model chain it can build, the node shutting down, or a turn whose runner it can't reach. No `Retry-After`, since no wait it knows clears it |
| `503` | `assistant_model_not_ready` | Every model in the chain is known unusable now; `reason` is the turn error code, with `detail` and `hint`, and a `Retry-After` only for a spent plan whose reset is known |

### How a turn fails on the model

When the model chain fails a round, the turn ends `failed` with an `error`
frame whose `code` says which failure it was, read from the harness's own
classification and never from its prose. Each carries a `message` a person
reads and a `hint`: an administrator (the caller holds `admin`) is told where to
fix it, and anybody else whom to ask. `retryable` is true only where asking
again can help without anybody acting.

| `code` | When | `retryable` |
|---|---|---|
| `model_signed_out` | The harness's login expired or is missing, or its token is invalid. An administrator is told to run `crewlet llm login <key>`, anybody else to ask an administrator | No |
| `model_busy` | The plan is spent or rate limited; `retry_after_seconds` is the wait until the harness's named reset | Yes, after the reset |
| `model_timeout` | The harness ran out of its own time (`providers.llm.<key>.timeout_seconds`) before it answered | Yes |
| `model_unavailable` | The harness could not be started on the node, or failed for a reason none of the codes above names (`retry_after_seconds: 10`) | Only for the second |

The first three, and a harness that cannot start, are also known before a question
is asked when every model in the chain is in that state: `/viewer` reports it
as `model_not_ready`, and a question asked anyway is refused
`503 assistant_model_not_ready` before any round.

Every segment of a turn leaves an `assistant_turn_completed` row in the
[runtime audit](#the-runtime-audit-sourceoperator), naming every step's tool and
route and never its arguments or answers.

## Chatlet

Every `GET` under `/chatlet/` reads the company's own chat — the default where it
declares no vendor chat surface (`chat.backend: chatlet`) — through this node's
estate router, as the tracker and the knowledge base are read, and it refuses
a copy that has not caught up exactly as they do. A company on Slack or
Mattermost, or with `chat.backend: none`, has no chat question at all and
asking gets `unknown_query` / `404`; [`/viewer`](#queries) says which chat it
runs, so a screen asks there first rather than reading that as a node missing
something.

**A room is read as the seat the caller acts as, and the caller never names
one.** Every other personal question here takes a `handle` and falls back to
the caller's own record. Chat has no such arm: a transcript is the most
sensitive thing a deployment holds, so the viewer is the seat the request's
principal acts as — the same seat a chat write of theirs is authored by, so
whoever may post in a room is exactly whoever is shown it — and a `handle=` on
one of these requests is an ignored parameter. Two refusals follow, both in
the [one envelope](#every-refusal-is-one-envelope):

| Who is asking | Answer |
|---|---|
| A principal without `chatlet` | `403 unauthorized`, `grants: ["chatlet"]` |
| A credential acting as itself — a Tier A token, a service account bound to no seat — whatever it holds | `403 unauthorized`, `reason: unseated`, `grants: []`. No permission seats a credential; the remedy is a binding in the identity directory (`crewlet iam bind`) |

**A room the caller may not read is not found.** A private room's existence is
itself information, so `chat_channel`, `chat_messages` and `chat_thread` answer
a room the caller is not in exactly as a room that does not exist, and a search
narrowed to one answers nothing. A public room and a unit's room are readable
by every seat, joined or not; a private room and a direct conversation are
their members', and `chat_room_names` names a private room only to a member
of it.

**The badges are the one thing not on the log.** Where each person has read
to, which rooms they muted and their do-not-disturb window are one reader's
attention, kept in the [coordination store](../concepts/coordination.md)
rather than replayed on every data node. They reach the rail as the caller's
own read state, and a store that could not be read leaves them unknown —
`read_state: false`, every badge zero — rather than counted from zero, which
would tell everybody they are a hundred behind in every room. The write half
is the `mark_chat_read` tool.

**The live half rides `/ws/stream`.** A committed message, edit, reaction or
room change is pushed to each socket whose seat may read the room — the same
visibility the reads apply, decided per recipient and never by a broadcast — a
person's read state to their own sockets on every node, and a room's presence
to the sockets looking at it ([the push kinds](#pushes)). The seat a socket is
shown chat as is fixed for its life: a person moved to another seat, or whose
`chatlet` is taken away, has their sockets closed to reconnect as who they are
now. A node whose copy of the estate is out of service still pushes, from a
sound peer's batches, and a gap in what it hears is a `chat_room_changed`
naming no room, which asks every screen to read its rooms again.

**The reads are here and the writes are tools.** Posting, editing, a room's
membership, opening a conversation and marking what you read are the
[operator catalogue's](#the-chat-gestures-only-a-person-makes) tools, through
[`/operator/act`](#operatoract--the-dashboards-write-surface) for the dashboard
and [`/operator/mcp`](#operatormcp--your-own-assistant) for an assistant. The
three gestures that destroy or set a room's horizon are administrator routes on
[the write surface](#a-chat-erase-prune-or-horizon).

**A message gesture is refused where it is applied, too.** A post, an edit, a
delete and a reaction are decided again by every data node at the record's
place in the log ([ADR-0046](../../adr/0046-a-message-is-sequenced-on-its-channels-subject.md)),
against what landed just ahead of it: a room archived a moment before, an
author taken out of a private room or a direct conversation a moment before, a
message taken back or erased a moment before, a message already carrying 32
distinct emoji. Where the node that answered has applied the record, such a
gesture is refused exactly as it would have been had that change reached it
first — `conflict` for the archive and for the emoji, each the state of the
room or the message ([which class a refusal is](#which-class-a-refusal-is)),
`forbidden` for the membership, `not_found` for a message that is gone or taken
back — and a retry
under the same `Idempotency-Key` is refused the same way rather than reported
as said. An answer of `pending` names a record this node has not applied yet,
which may still be refused there.

## Agent Memory

### `GET /agents/{id}/memory`

What one seat has learned, in one round trip — **answered by the node holding
the seat**. Also served as the `agent_memory` query, which takes `{id, limit,
skills_order}`; the route takes `?limit=` and `?skills_order=` (`uses` or
`updated`; the newest drafted first when absent), the order the holder reads
the seat's whole set of skills in before it cuts their page.

`{id}` is the seat's **handle** — the canonical identifier everywhere in
the system — or a person's **login**, which names the seat the identity directory binds them
to and is never read as a handle. It is a seat's TRAIL, so it takes
`audit.seat.read` — the seat's lead, or `admin` — and an absent name is the
caller's own seat. The halves are keyed differently in the store — the diary and the
onboarding marker by the derived agent id, the episodes, skills and
counterparty profiles by the handle — and the answer resolves both itself
rather than making a caller know which.

```json
{
  "id": "<handle>",
  "diary": [
    { "id", "content", "retention", "source", "turn_id",
      "created_at", "ttl_until", "retrievals" }
  ],
  "diary_total": 142,
  "episodes": [
    { "id", "turn_id", "agent_handle", "task_summary", "ask", "ask_bytes",
      "plan_summary", "plan_summary_bytes",
      "review_outcome", "tool_sequence", "skills_used",
      "conversation_key", "work_key", "created_at", "ended_at",
      "duration_ms", "compacted", "count",
      "compaction": { "common_task_pattern", "done", "notable_patterns" } }
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
| a peer | asked of that incarnation on an ephemeral scatter (`crewlet.held.read`), with a 2 s budget; silence is `unavailable` naming the node, never an empty memory — a holder that is draining, or whose heartbeat lapsed, included |

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

**An episode says what it is.** `task_summary` is the label of the event that
woke the turn, `ask` what it was asked (`""` for a turn that was told nothing)
and `plan_summary` what it did. A compacted row stands for `count`
turns and carries `compaction` instead — what they had in common, how many of
them ended `done`, counted from the members, and what varied — which is `null`
on a raw row.

**A listed episode carries the openings of its two long texts.** An ask is
bounded only by the event that delivered it and an account by nothing, so a
page of fifty whole could be more than the transport carries — and the holder
would refuse the whole read. So `ask` and `plan_summary` are each at most their
first 600 bytes, cut between characters — the figure a seat is shown of a past
turn's words before they are condensed (`learning.EpisodeAccountBytes`), so a
text listed whole is one a seat was shown whole — and `ask_bytes` and
`plan_summary_bytes` are the whole texts' sizes: a text is whole exactly when
its size is its length. The whole row is
[`GET /agents/{id}/memory/episodes/{episode}`](#get-agentsidmemoryepisodesepisode).

### `GET /agents/{id}/memory/episodes/{episode}`

One episode **whole**: the same row as the listing's, with `ask` and
`plan_summary` complete. Also served as the `agent_episode` query, which takes
`{id, episode}`. Answered by the node holding the seat under exactly the rules
of `GET /agents/{id}/memory` above, and naming it — its rule included: a
seat's trail takes `audit.seat.read`, the seat's lead or `admin`.

```json
{
  "handle": "<handle>",
  "episode": { "id", "task_summary", "ask", "ask_bytes", "plan_summary",
               "plan_summary_bytes", "…": "an episode row" },
  "held_by": "node-2"
}
```

`episode` is `null` for one the seat does not hold — the lifecycle dropped it
or folded it into a compacted row since it was listed, or it is another seat's
— which is an ordinary absence. An episode's ask and account came in one
completed turn's event, which the transport held to 8 MiB, so one read whole fits
as that event did; should one ever not, it is `unavailable`, with the holder
saying how large the answer was, rather than cut.

### `memory_overview`

Every agent seat's memory at a glance: the list **Doclet › Agent diaries**
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
| `held_by` a node, `unavailable` set | that node did not answer (or its answer could not be read), or is still taking the seat — the reason is here and the zeros beside it are not a count |

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

## Fleet, Executors & Schedules

### `GET /mcp-servers`

Backs the **Servers** section of **Settings › MCP Tools**: every MCP server,
what the configuration declares for it and what each live node did with it,
and the engine's built-in tools beside them.
**It takes `admin`**, as `GET /config` does — it names the nodes, the
launch commands and the first line of each failure, which is what the
configuration it reports on carries.

**Off the heartbeats, not a fan-out.** Each node re-publishes its MCP servers
as they are at each beat on its presence lease — a child that died after it
started counts as failed until the engine has
[brought it back](../guides/tools-and-mcp.md#a-server-that-dies-or-will-not-start),
and a shared server whose start failed counts as started once a retry brings it
up — one row per server with its instances counted (a per-seat template has one instance per seat that node holds), so
one read of the lease table is every node's answer at once. A node whose last
heartbeat carried no status (its status hook did not answer within the beat) is
`reported: false` and its cells are UNKNOWN — never a row of zeros, which would
read as "started nothing". A node that reports and started nothing is
`reported: true` with no rows.

`servers` lists every server the configuration this node runs declares, then
any a node reports that the configuration does not carry (`configured: false`
— a node still on an older revision mid-rollout). The launch is the parts
that are not credentials — `transport`, `command`, `args`, `url`; `env` and
`headers` are never here. `enabled` is whether the configuration runs the
server at all (`mcp_servers[].enabled`, true on a server it does not carry),
and `description` what it says the server is for, the line each agent's
prompt lists beside the server's name (`""` when it says nothing). `title`, `catalogue` and `vendor` are what the
[curated catalogue](#get-mcp-serverscatalogue) recognises the server as, by
its address or its command: the entry's title, its key and the vendor mark
the dashboard draws. A server the catalogue does not know is titled by its
name, with `catalogue` and `vendor` empty. `started`, `failed` and `waiting` are summed over
the nodes, `tools` is the most one started instance serves, and `state` is
decided here so no screen re-derives it. `waiting` counts a seat's instances
held back, unstarted, for a credential a
[provisioning pass mints for that seat](../guides/tools-and-mcp.md#shared-vs-per-role-servers)
and has not sealed yet, where nothing that pass last reported says it cannot —
neither started nor failed, since nothing is wrong and each starts once the
seal reaches its node. One whose pass reports that a person must act first
(its own credential missing or refused, an approval outstanding, the seat's
account failed or switched off, or the integration being disconnected) counts
as `failed`, judged again at every heartbeat, since a wait with no end is a
failure somebody has to act on:

| `state` | Means |
|---|---|
| `disabled` | The configuration turns the server off (`enabled: false`): nothing is started for it and no seat is granted it. Decided before anything a node reports, so a node still on an earlier revision that runs it shows only in its own cell until it applies this one |
| `running` | Every instance any node launched is serving: it started, listed its tools and was up at its node's last beat |
| `partial` | Some are serving and some are not — a node's environment, one seat's credentials, or one child that died and is being restarted, rather than the server |
| `failing` | Something failed and nothing is serving — instances were launched and none serves (the server, its command or address, or credentials every seat shares), every instance went down and is being restarted, or the server was held back, unlaunched, for a value that resolves to nothing: a shared server's own, or one every seat's instance lacks |
| `waiting` | Nothing was launched and nothing failed, and some seat's instance is waiting for its provisioned credential. Only where nothing else decides: a server serving one seat while another waits is `running`, and one with a failure beside a wait is `partial` or `failing` |
| `not_started` | Every reporting node started nothing for it — a per-seat template no seat on a live node declares credentials for |
| `unreported` | No live node's latest heartbeat carried the report |

`error` is one failed instance's reason, cut to 512 bytes on the heartbeat,
and `error_seat` the seat it was launched for (none for a shared server); the
whole text is the node's `mcp_server_failed` log line for a start that failed,
its `mcp_server_credential_missing` line for a server or a seat's instance held
back, unlaunched, because a value it is started with resolves to nothing and
nothing mints it, or the integration minting it cannot mint until a person
acts (that one from the node's last pass over the seat; the heartbeat re-judges
it against the integration's latest report), and its `server_exited` or
`server_restart_failed` line for an instance that went down after it started.
`wait` is one waiting instance's sentence — the variables it waits for and the
integration minting them — bounded the same way, and `wait_seat` its seat; the
node's `mcp_server_awaiting_credential` line carries it.

`unlisted_annotations` are the entry's `tool_annotations` keys that no
instance started on a reporting node lists a tool for — by the server's own
name or with the entry's `tool_prefix` — sorted: each overrides nothing
anywhere it runs, which is a misspelling or a tool the server no longer
serves. A key only SOME instances lack is not here, because a per-seat child
serves what its seat's credentials reach (an `mcp-atlassian` child for a seat
holding only a Jira credential lists no Confluence tool), and that key is
right for the others. Each node publishes the keys none of its own started
instances lists and the answer keeps the ones every such node names; with no
instance started there is nothing to judge, and the list is empty. Each
instance's own keys are on its `tools_discovered` log line
(`annotation_keys_unlisted`).

`refused_tools` are the server's tools that are not offered for their name,
and `shadowed_tools` the ones another server's tool of the same name is
offered in place of ([Naming a server and its tools](../guides/tools-and-mcp.md#naming-a-server-and-its-tools)).
Each is `{tool, reason, message}` or `{tool, by, message}`: `tool` is the name
it would be filed under (the server's `tool_prefix` and its own name),
`reason` is `characters`, `too_long` or `builtin`, `by` is the configured name
of the server whose tool is offered instead, and `message` is the engine's
sentence saying why and what would offer it (a shorter `tool_prefix`, one
where two servers share a name), which a screen shows as written. Each tool is
listed once across the fleet, sorted, every one any node named; a node names
at most 8 of each on its heartbeat, so `refused_count` and `shadowed_count`
say how many there are, as the node counting the most counts them, and never
fewer than are listed. Both lists are always present, empty where every tool
is offered. The node's own log names each one too (`mcp_tool_refused`,
`tool_name_collision`).

```json
{
  "nodes": [{"id": "node-1", "reported": true}, {"id": "node-2", "reported": true}],
  "servers": [
    {"name": "github", "title": "GitHub", "catalogue": "github", "vendor": "github",
     "configured": true, "shared": false, "transport": "http",
     "command": "", "args": [], "url": "https://api.githubcopilot.com/mcp/",
     "state": "partial", "started": 3, "failed": 1, "waiting": 0, "tools": 26,
     "unlisted_annotations": [],
     "refused_tools": [
       {"tool": "gh_add_pull_request_review_comment_to_pending_review", "reason": "too_long",
        "message": "\"gh_add_pull_request_review_comment_to_pending_review\" is not offered: it is 52 characters, and 50 fit beside the longest prefix a harness gives the engine's tools. Shorten the tool_prefix \"gh_\" by 2 to offer it"}
     ],
     "refused_count": 1, "shadowed_tools": [], "shadowed_count": 0,
     "nodes": [
       {"node": "node-1", "reported": true, "started": 2, "failed": 0, "waiting": 0, "tools": 26,
        "error": "", "error_seat": "", "wait": "", "wait_seat": ""},
       {"node": "node-2", "reported": true, "started": 1, "failed": 1, "waiting": 0, "tools": 26,
        "error": "401 Bad credentials", "error_seat": "backend-dev", "wait": "", "wait_seat": ""}
     ]}
  ],
  "builtin": [
    {"key": "work", "label": "Work items",
     "description": "Worklet, the company's tracker: read, file, discuss and move work.",
     "tools": [
       {"name": "list_work_items", "description": "…", "source": "builtin",
        "annotations": {"read_only": "yes", "destructive": "unknown",
                        "idempotent": "yes", "open_world": "unknown"},
        "delivers": "", "input_schema": {"type": "object", "properties": {"…": {}}},
        "group": "work", "gate": "worklet"}
     ]}
  ]
}
```

`builtin` is the engine's own tools as an agent is offered them, group by group
in the order the catalogue lists them, each tool a
[catalogue row](#the-tool-catalogue) whose `gate` says who may call it (the
words are in
[Groups, and who may call each one](../guides/tools-and-mcp.md#groups-and-who-may-call-each-one)).
It is the built-ins of the registry this node applied, which every node
applies alike, plus `list_mcp_server_tools` and `activate_tool`, which the
engine hands the onboarding pass and a worker and no registry holds. A group
the company offers nothing of is left out: a company on Jira gets no `work`
group, and one on Slack no `chat` group. A node with no active revision sends
an empty list.

To add a server, `PUT /config/mcp-servers/{name}` with `If-None-Match: *` —
see [Per-entity read and write](#per-entity-read-and-write).

### `GET /mcp-servers/catalogue`

What a server is added from, for the add flow of **Settings › MCP & tools**.
It is decided by `config.read`, so **it takes `admin`**, as adding a server
does: an entry is the draft of a launch line every engine host runs, and the
answer names the registry the company's configuration does. A plain read
rather than a question: nothing in it is live, so the socket does not carry
it.

`curated` is the curated catalogue whole, in its order, whatever `q` says: a
table compiled into the engine and reviewed in pull requests, so it answers
the same on a node with no egress. Each entry is a vendor's server as its own
page described it on `read_at`, and a local server launched through a package
runner names the `package` it fetches at the exact `release` its facts were
read against. The curated catalogue is also what the engine
[recognises a configured server by](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry).

`community` is empty unless `q` is given and the company names a registry
(`mcp_registry.url`), and then holds what that registry answered for `q`:
the newest version of each server whose name holds `q`, at most 20, in the
registry's order. The registry speaks the [MCP Registry](https://registry.modelcontextprotocol.io)
v0.1 API. Every listing is `community: true` and is a draft for the same add
flow an address typed by hand goes through: it carries no `read_at`, no
`integration`, no `prerequisite` and no fixed `value`, and its `vendor` is set
only when its remote address is a host the curated catalogue recognises. A
listing is offered with a Streamable HTTP remote at a fixed https address, or
else an npm or PyPI package from the public registry at an exact release,
launched as `npx -y <package>@<release>` or `uvx <package>==<release>`. A
listing the engine cannot read whole (a field past its bound, a direction
override in its text, a templated address, a runner argument, a package from
another registry, a deprecated or deleted listing) is left out and counted in
`left_out`. The engine dials the registry alone, never anything a listing
names, and follows no redirect.

`registry` is the address the company names, or `""`. `reason` says why a
search found nothing to offer: the company names no registry, or the
registry could not be reached, refused, or answered something that is not the
v0.1 server list. A search that fails is that `reason` beside the curated
half, never an error in its place; only `q` itself (empty after trimming,
over 200 characters, or holding a control character) is refused, as
`bad_params`.

```json
{
  "curated": [
    {"id": "linear", "name": "linear", "title": "Linear", "vendor": "linear",
     "description": "Issues, projects, cycles and documents.", "transport": "http",
     "url": "https://mcp.linear.app/mcp", "command": "", "args": [],
     "headers": [{"name": "Authorization", "description": "A Linear API key.", "required": true,
                  "secret": true, "value": "", "format": "Bearer <API key>", "from_integration": false}],
     "env": [], "tool_prefix": "", "tool_annotations": {}, "auth": ["oauth", "headers"],
     "oauth_client": "cimd", "integration": "", "prerequisite": "",
     "docs": "https://linear.app/docs/mcp", "read_at": "2026-11-01", "package": "", "release": "",
     "community": false, "publisher": "", "version": "", "updated_at": null}
  ],
  "community": [
    {"id": "com.example/weather", "name": "weather", "title": "Weather", "vendor": "",
     "description": "Forecasts and alerts for a place.", "transport": "http",
     "url": "https://mcp.weather.example.com/mcp", "command": "", "args": [],
     "headers": [{"name": "X-API-Key", "description": "Your API key.", "required": true,
                  "secret": true, "value": "", "format": "", "from_integration": false}],
     "env": [], "tool_prefix": "", "tool_annotations": {}, "auth": ["headers"], "oauth_client": "",
     "integration": "", "prerequisite": "", "docs": "https://weather.example.com/docs/mcp",
     "read_at": "", "package": "", "release": "", "community": true,
     "publisher": "com.example", "version": "2.1.0", "updated_at": "2026-10-27T08:00:00Z"}
  ],
  "registry": "https://registry.modelcontextprotocol.io",
  "reason": "",
  "left_out": 1
}
```

### `GET /fleet`

Backs the dashboard's **Settings › Nodes** screen — the questions `/health` cannot
answer, because it answers about the node that served it and a load
balancer sends the next refresh somewhere else.

Read from the lease table, so every node gives the same answer: node
presence carries each node's `node.roles` and `node.labels`, seat and
worker leases name their holder, and each node's config record comes from
the control plane's apply status.

**It needs a credential carrying `admin`, reads included** — a
signed-in session or a bearer — like every other answer the dashboard's
Settings › Nodes screen draws. What it describes is the DEPLOYMENT
rather than the company's work — the node ids, which node holds which
seat, the lease epochs, how far a rollout has reached — so it is scoped
the way `/integrations` beside it always has been, and
Authentication alone does not open it: it takes its own grant.

Presence also carries what each node is **doing** — `in_flight`,
`draining`, `posture`, `started_at`, how far its own copy of the company's
records has come up (`appliers_ready` of `appliers_total`) and whether
it serves admin (`admin`) — because only the node running a seat knows those,
and `/health` answers about whichever node served the request. They ride on
the heartbeat that already re-sends roles and labels on every beat, rather
than over a request/reply to the owning node: every answer would then be
partial, it opens a new trust edge, and it duplicates the mechanism the lease
table already is.

**Absent is not zero.** A node whose status read did not answer within its
share of the heartbeat (`seat.StatusBudgetRatio`) — a read cut short by that
budget, or one that failed — publishes no status on that beat and omits those
fields entirely, and the dashboard draws an em dash. A confident `0` would
render an idle row for a process that is simply not saying. Whether the read
answered is decided by the read itself, not by the clock after it returned, so
a complete answer is never dropped because the process was briefly descheduled.
`posture` alone can be absent beside the rest: a node that could not read the
control plane — the read refused at once, or ran out of its budget — still
publishes the rest of its status, its in-flight count, drain state, start time
and `admin` among them, and no posture, never the `serve` its readiness probe
falls back to in that case.

`appliers_ready` of `appliers_total` counts the node's state-log
appliers — one per domain, which every data node runs from boot — and how
many of them are current. It answers a different question from
`config_epoch`: a node can hold the current revision and still be applying
the records its copy is derived from, and a node that is behind keeps the
seats it holds but claims no new ones until it is level ([a copy that is
behind](../concepts/seat-ownership.md#a-copy-that-is-behind-and-a-copy-that-is-wrong)).
That is why it is here and not on `/ready`: a node catching up is healthy —
it serves its dashboard, answers its probes and runs the duties it holds — and
only taking on a new seat waits for it. Both fields are absent on
a node without `data`, which runs no loop and reads a data node's copy —
never `0` of `0`, which a reader would have to know is not a stalled one.

`admin` is the node's own [`api.auth.admin`](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app):
`serve` or `refuse`. It is decided per node and per request, and nothing is
written when it changes, so a fleet mid-rollout — or one that publishes
admin on some nodes only — legally disagrees about it; this column is where
that is said, rather than whether somebody can administer the company
depending on which node a load balancer sent them to. Absent on a node that
declares none, which only a node serving no API may do: a node not saying,
never one that refuses.

The configuration columns come from the control plane's apply status. The
node's last attempt is `config_epoch`, `config_status` (`ok`, `error` or
`degraded`), `config_error` and the revision that epoch stands for
(`config_revision_id`). What it runs is `config_serving_revision_id`, which
is not the attempt's revision where that attempt failed: a node that refuses
the fleet's revision goes on serving the one before it. What it would start
on if it restarted now is `config_boot_revision_id`, which moves only once an
apply has succeeded, and is absent on a node that starts on nothing (one
whose own database is discarded at every start, or one that has applied
nothing and holds no offline choice). The epoch it last applied is
`config_applied_epoch`, absent, never `0`, on a node that has applied
nothing since it started. And `config_reported_at` is when the node last
reported, so a node still on the previous revision reads as such. A node
with no report there — one whose report aged past the freshness bound, the
same record the fleet's posture decision reads — has `config_epoch` `0`, no
boot revision or applied epoch, and the other columns empty. What they are
compared against is `target_epoch` — the epoch the fleet is converging on
— and `activation`, which names what that number stands for: its `epoch`,
`revision_id`, when it was activated (`at`) and the `summary` whoever
activated it wrote. Both are read from the one activation pointer, so they
cannot describe two different activations; `activation` is `null` and
`target_epoch` `0` where no revision has been activated or the pointer could
not be read.

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

```json
{
  "nodes": [
    {
      "id": "core-1", "roles": ["data", "ingress", "seats", "workers"], "broker": "member", "labels": {},
      "owner": "core-1:8f2a", "protocol": 1, "seats": 4, "expires_in": 41.2,
      "config_epoch": 7, "config_revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41",
      "config_status": "ok", "config_error": "", "config_applied_epoch": 7,
      "config_serving_revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41",
      "config_boot_revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41",
      "config_reported_at": "2026-09-23T08:14:02.118Z",
      "in_flight": 1, "draining": false, "started_at": "2026-09-23T08:01:57Z",
      "posture": "serve", "appliers_ready": 10, "appliers_total": 10,
      "admin": "serve"
    }
  ],
  "seats": [
    {"handle": "ceo", "node": "core-1", "owner": "core-1:8f2a",
     "epoch": 4, "expires_in": 41.2, "acquired_at": "2026-09-23T08:02:11.482Z"}
  ],
  "duties": [{"duty": "maintenance", "node": "core-1", "expires_in": 41.2}],
  "unplaceable": [{"handle": "gpu-eng", "placement": "labels=gpu=true"}],
  "unmanned_roles": [],
  "this_node": "core-1",
  "target_epoch": 7,
  "activation": {"epoch": 7, "revision_id": "3f1c0f0e-8a52-4d3b-9d7e-2b6f3f0c9a41",
                 "at": "2026-09-23T08:13:40.552Z", "summary": "raise the executor round cap"},
  "objects": {
    "state": "reported",
    "backend": "s3:https://s3.example.com/files/acme/",
    "node": "data-a",
    "collect": {
      "at": "2026-09-01T12:00:00Z",
      "completed": true,
      "listed": 1840,
      "aged": 1702,
      "deleted": 12,
      "referenced": 1690,
      "abandoned": 1
    },
    "audit": {
      "at": "2026-09-01T09:00:00Z",
      "completed": true,
      "referenced": 1828,
      "missing": 0,
      "damaged": 0,
      "found": {
        "at": "2026-09-01T09:00:00Z",
        "completed": true,
        "referenced": 1828,
        "missing": 0,
        "damaged": 0
      }
    }
  }
}
```

**`objects` is where the company's files are kept, and what the
[object store's](../concepts/object-store.md) collector last found** — read
from the coordination store like the rest of this answer, so every node reports
the same record whichever node ran the passes. It is absent on a node that
reads no record.

`state` names three things apart rather than drawing any of them as an empty
report: `unavailable` (the coordination store did not answer), `not_yet` (no
collector has finished a pass — a new fleet, or no data node running the duty)
and `reported`. Only a report carries the other fields. The example is the
engine's own rendering: a test holds its `objects` block to what the renderer
writes.

| Field | What it is |
|---|---|
| `backend` | The store every node agreed on at boot: `nats` (the data nodes' replicated bucket) or `s3:<endpoint>/<bucket>/<prefix>` |
| `node` | The data node holding the collector's duty when it ran the passes below |
| `collect` | The last **collection**: when it ended (`at`), whether it listed the whole store and judged every object past the day's grace (`completed`), and its counts — `listed` (the objects under the engine's own namespace), `aged` (past the grace by both the store's clock and the key's own, so judged), `deleted` (no row named them), `referenced` (a row still did) and `abandoned` (uploads begun more than a day ago and never finished, which no listing shows). `skipped` says why it stopped judging — an estate this node could not fully read — and the counts are what it did before it stopped; `sweep_error` what kept it from abandoning unfinished uploads, which does not fail the collection (on S3, an identity without `s3:ListBucketMultipartUploads` or `s3:AbortMultipartUpload`); `error` what stopped it. Absent before the first one ends |
| `audit` | The last **audit attempt**, which asks the store about every object a row names: when it ended (`at`), `referenced`, `missing` (objects the store does not hold), `damaged` (objects it holds at another size, or under another digest where it keeps one), `completed` — false over an estate that was not complete, when the counts are floors — and `error`, what stopped it. Absent before the first one ends |
| `audit.found` | What the last audit to **run to its end** found — the attempt above, or the one before it when that one failed, so a failed attempt never hides what was found: `at`, `completed`, `referenced`, `missing`, `damaged` and `missing_files`, the first hundred files that cannot be read, each `{"object", "named_by", "damaged"}` — the object's key, the file as `PROJECT/path`, and `true` where the store holds it wrong rather than not at all (absent when none). A non-zero `missing` plus `damaged` here raises [`objects_missing`](alarms.md). Absent before any audit has run to its end |

A collection runs hourly and an audit daily, on one data node at a time; a pass
that fails is tried again ten minutes later.

Each node row carries `broker`: how the node's broker takes part in the
fleet's — `member` (embedded, a voter in the JetStream cluster), `leaf`
(embedded, joining the members through `stream.leaf.urls`, holding nothing),
`client` (`stream.type: nats`) — or `unknown` for a node advertising a kind
this build does not know (a newer build's). It is derived from the node's own `stream` block, never from
its roles; [`GET /fleet/broker`](#the-brokers-membership) holds it against what
the broker itself counts.

**Each node row carries its config record**, which the node's reconciler writes
to the control plane's apply status and renews on every tick, and the answer
carries the activation every row is compared against:

| Field | What it is |
|---|---|
| `target_epoch`, `activation` | The fleet's activation pointer, read once for both: its `epoch`, the `revision_id` it names, when it was activated (`at`) and the `summary` its writer gave. `0` and `null` when nothing is activated or the pointer could not be read |
| `config_epoch`, `config_revision_id` | The activation this node last **attempted**: its epoch and the revision it named. `0` and `""` on a node that has reported nothing |
| `config_status`, `config_error` | How that attempt went — `ok`, `error` (refused and rolled back, so the node runs what it ran before) or `degraded` (failed after a part that cannot be rolled back, such as a restarted MCP server, had already changed, so it is never counted as converged) — and the error it failed with, cut at 2 000 bytes |
| `config_applied_epoch` | The activation epoch this node last **applied** — the `applied_epoch` its own [health envelope](#the-health-envelope) carries. It differs from `config_epoch` exactly when `config_status` says that attempt failed: a node that applied 5 and refused 6 reports `config_epoch` 6 and this 5, and only this says the epoch it runs, so a writer judging whether a save reached a node compares the saved epoch with this. **Absent** on a node that has applied nothing since it started — it runs what it booted on, or nothing — and from a build older than the field, never `0` |
| `config_serving_revision_id` | The revision this node's engine **runs**: the one its last successful apply installed, or the one it booted on. Empty while it runs none, and from a build older than the field |
| `config_boot_revision_id` | The revision this node would **come back on** if it restarted now — what its boot row names, read before the node's first record and after every write to the row, so a read of the row that fails leaves the last answer standing rather than naming none. It equals `config_serving_revision_id` except while a write to that row is being retried (the node runs a revision it would not start on until the next tick repairs it), and is **absent** on a node that boots on nothing: one whose own database is discarded at every start (`store.scratch`, so a node without the `data` role), one that has never applied a revision and holds no offline change, and a build older than the field. Whatever a node needs in order to start — a revision's credentials among them — is kept against this one, never against the revision it happens to be running |
| `config_reported_at` | When the node last wrote its record. A record nobody renews ages out after four reconcile intervals, about a minute, and the row then carries none |

**A node runs the fleet's company only when** it reports the activation's
epoch and revision with `ok` and serves that revision. A node that refused the
fleet's revision reports the activation's epoch and revision with `error`, and
its `config_serving_revision_id` names the revision before, which it goes on
running until it can apply the fleet's — and `config_boot_revision_id` the one
it would come back on, the same revision unless a write to its boot row is
still being retried, and absent on a node whose own database is discarded at
every start; a
comparison of `config_epoch` with `target_epoch` alone reads it as current.
Every dashboard figure that counts nodes behind on config uses this rule, and the
**Settings › Nodes** screen names the revision each such node runs. See [Reading
a stuck node](../concepts/control-plane.md#reading-a-stuck-node).

### The broker's membership

```
GET  /fleet/broker
POST /fleet/broker/remove/{node}?confirm={node}[&force=true]
POST /fleet/broker/remove-peer/{peer}?confirm={peer}[&force=true]
```

Two records say who the fleet broker's members are, and they can disagree.
Every node advertises its broker kind on its presence lease; the broker's
**metadata group** — the raft group that places every stream and consumer —
counts its voters for itself. A member that is gone for good is the
disagreement that matters: its presence lapses with its process, while the
metadata group goes on counting it in every election and every create until it
is removed, so a three-member fleet that loses two for good has no quorum left
to create anything with.

**`GET /fleet/broker`** answers both and where they disagree. Any node answers
it: only a member holds the metadata group, so a node that is not one asks
every live member at once, and `group_from` names the member whose view `group`
is. A fleet on an external cluster answers `external: true` and lists no group —
that cluster's membership is its operator's. A group no member could report
within five seconds is absent with `group_error` saying why, never an empty one.

The group counts each voter by its raft **peer id**, which nats-server derives
from the server's name — the node id. A member learns another's name only from
that server itself, so a voter whose survivors have restarted since it died is
listed with no `name` at all, by its `peer` alone; each node row carries the
`peer` it is counted by, and the two records are held against each other by
peer id, so such a voter is still recognised as the live node it is where one
is.

```json
{
  "node": "node-a", "kind": "member", "external": false,
  "nodes": [
    {"node": "node-a", "peer": "ePFsSWs4", "kind": "member", "roles": ["data", "ingress", "seats", "workers"]},
    {"node": "old-1", "peer": "mTNLbbt3", "kind": "unknown", "roles": ["data", "ingress", "seats", "workers"]},
    {"node": "sat-eu-1", "peer": "zYqdsRpE", "kind": "leaf", "roles": ["seats"]}
  ],
  "group": {"cluster": "acme", "leader": "node-a", "peers": [
    {"name": "", "peer": "X57jblDH", "current": false, "offline": true, "active": 7200000000000},
    {"name": "node-a", "peer": "ePFsSWs4", "self": true, "leader": true, "current": true, "active": 0},
    {"name": "node-c", "peer": "9iReXzcw", "current": false, "offline": true, "active": 5400000000000}
  ]},
  "group_from": "node-a",
  "findings": [
    {"kind": "dead_member", "node": "", "peer": "X57jblDH", "detail": "the metadata group counts it as a voter, no live node is it, and the member that answered has not heard its name since it started"},
    {"kind": "dead_member", "node": "node-c", "peer": "9iReXzcw", "detail": "the metadata group counts it as a voter and no live node is it"},
    {"kind": "unknown_kind", "node": "old-1", "detail": "its presence does not say what its broker is"}
  ]
}
```

`active` is nanoseconds since the answering member last heard from the peer.
The finding kinds:

| `kind` | Means |
|---|---|
| `dead_member` | A voter no live node is — its process is gone, or its node came back as a leaf or a client. It is counted in every election until it returns or is removed |
| `not_in_group` | A live node advertising a member that the group does not count: still joining, or removed while it ran, in which case it rejoins as a voter at its next restart |
| `unknown_kind` | A live node whose presence advertises no broker kind this build knows — a newer build's. It is counted as a member wherever that is the safe reading, a [capacity seal](../guides/retention.md#who-has-to-acknowledge) included |

It is the `fleet_broker` question, so the [socket's query channel](#ws-wsstream)
answers it too. A lease table that could not be reached answers `503
unavailable` with a `Retry-After` rather than a fleet with no nodes.

**`POST /fleet/broker/remove/{node}`** removes a member from the metadata group,
by the peer id its node id hashes to — so it reaches a member whose name no
survivor remembers, as long as you know which node it was. nats-server answers
that request only on the broker's **system account**, which a member reaches
inside its own process and nothing else can — so the node that receives the
request asks a live member, never the one being removed, and that member's
system account carries it. The answer comes once the group has committed the
change, and names the voter, the member that carried it and the group as it
reads afterwards:

```json
{"node": "node-c", "peer": "9iReXzcw", "by": "node-a", "group": {"cluster": "acme", "leader": "node-a", "peers": [...]}}
```

**What the node asks a member is signed**, the removal and the read of the
group alike, under the fleet keyring every state-log record is signed under,
in a key domain of its own — because a member carries a removal out through
its system account, and the broker authenticates nothing itself, so an unsigned
request on the member's subject would change the quorum past every check this
route makes. Each request is bound to an id and a deadline inside its
signature, so one captured off the wire is not carried out twice, and the
answer is believed only where the fleet signed it for that request. A member
that does not hold the key a request is signed under runs nothing and says
so, and the next member is asked; a request the fleet did not sign is refused
and logged at error on the member (`fleet_broker_request_refused`). The
carrying member's `fleet_broker_member_removed` line names who asked — the
author, their kind and the credential they pressed it through — read off the
signed request ([ADR-0031](https://github.com/crewlet/crewlet/blob/main/adr/0031-every-state-log-record-is-signed.md)).

**`POST /fleet/broker/remove-peer/{peer}`** is the same removal naming the voter
by the `peer` the listing shows — the form for a voter listed with no name,
which has no node id to give. Its answer's `node` is empty unless a live node is
that voter.

A removal is **refused while the voter's node holds a live presence lease as a
member**, or without saying what its broker is: the node is running, and a
running member removed from the group rejoins it as a voter at its next restart.
`force=true` removes it anyway — for a member wedged in a way that still renews
its lease. A node alive as a **leaf** or a **client** under the voter's name is
removed without force: the member it was is gone for good, and its broker never
rejoins. The whole gesture is bounded by one wait — the carrying member's own
commit budget and one round trip; a member that does not answer within it ends
the gesture as `outcome_unknown` rather than another member proposing it again.
`crewlet fleet broker remove` is a client of both routes. **Both take
`admin`** and a [recent proof](#some-gestures-ask-how-recently-you-proved-who-you-are),
as every other control of the deployment does, and both are refused during a
[drain](#during-a-drain). Every refusal carries `detail` and
`hint`:

| Status | `error` | When |
|---|---|---|
| `400` | `confirm_required` | `?confirm=` does not repeat the node id or the peer id |
| `400` | `voter_invalid` | The name is not a node id (or, on `remove-peer`, not a peer id) |
| `404` | `not_a_member` | The metadata group counts no voter by that peer id — a typo, or a removal already made |
| `409` | `member_live` | The voter's node holds a live presence lease as a member (or does not say); stop it first, or `force=true` |
| `409` | `membership_changing` | Another membership change is still being committed; ask again once it has |
| `409` | `external_broker` | The fleet's broker is an external cluster, whose membership is its operator's |
| `503` | `no_leader` | Nobody answered as the group's leader for the whole wait. The request is asked again every second, so an election alone does not end here: the group has lost its quorum and can change nothing about itself. A removal whose answer was lost may still have been committed — read the group first |
| `503` | `no_member` | No live member could carry the removal — including when the only live member is the one being removed, which never carries its own, and when every member asked holds no key the request is signed under (bring `secrets.keys` into agreement across the fleet) |
| `504` | `outcome_unknown` | The member carrying it did not answer, or answered with nothing this node can believe — not signed by the fleet, or not the answer to this request; it may have committed the removal first — read the group before asking again |
| `500` | `broker_remove_failed` | Anything else; read the group before asking again |

### `GET /executors`

Every seat's [executor run](../concepts/code-sandbox.md) the engine still
holds, oldest first — `launching`, `running`, `awaiting_clarification`,
`reseed`, `answered` and `resumed` run records. An `answered` run has a
person's reply recorded as the answer to its question and is owed the resume
that answer drives; it is no longer waiting on anybody, so `audience=` — what
is waiting on one person — lists only runs still waiting (`awaiting_clarification`
or `reseed`).

A run that has settled, whether its turn finished, it was lost or a person
stopped it, is not listed because it has no record: its record is deleted once
its box is reclaimed. How it ended is on the event stream, in the resumed
turn's own events, an `executor_failed` event naming the reason, or a
`executor_stopped` event naming who stopped it.

A `launching` executor is one whose launch has started while the turn that
started it is still unwinding, so the parked turn state its collection reads
is not on the row yet; it is listed but never polled, because a row nobody
lists is a box nobody reclaims.

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
      "status": "awaiting_clarification", "harness": "claude-code",
      "provider_key": "default", "model": "sonnet",
      "task_description": "Add retry to the webhook client",
      "question": "Which backoff ceiling should I use?",
      "question_context": "The webhook provider retries for **24 hours**; see the [retry spec](https://example.com/retry).",
      "decision": {
        "question": "Which backoff ceiling should I use?",
        "options": [
          {"id": "five-min", "label": "Five minutes", "detail": "Inside the provider's own retries."},
          {"id": "one-hour", "label": "One hour"}
        ],
        "recommended": "five-min", "rationale": "The provider retries anyway.",
        "role": "approver"
      },
      "audience": "manager",
      "audience_handles": ["founder"], "audience_fallback": false,
      "branch": "crewlet/eng/retry", "trace_id": "<hex>", "owner": "core-1:8f2a",
      "box_exists": true, "paused_at": "2026-06-08T07:30:02+00:00",
      "pause_ttl_seconds": 3600,
      "started_at": "2026-06-08T07:12:44+00:00",
      "updated_at": "2026-06-08T07:30:02+00:00",
      "answerable_in_chat": true,
      "declined_answers": []
    }
  ]
}
```

`question` is what a parked run asked, in one sentence; `question_context` is
its context in Markdown, empty when it gave none; and `decision` is the choice
it asks for, in the shape a [work item's decision](../guides/worklet.md#asking-for-a-decision)
has (never with `inform`), or `null` for an open question answered in words.
All three are the run's body, sent to the people its question is put to as the
question always was, and answered with [`answer_executor`](#answering-a-parked-executor)
(`choice` names one of `decision.options`).

`declined_answers` names the answers the run's question let go of, by the id
of the delivery each arrived in, which `answer_executor` answers as `answer_id`. An
answer named there was not used: the run could not be resumed with it and
waits on its question again, so the person who gave it is asked to answer
again rather than left looking at a question that never moves.

`launch_id` names the job the row holds now. A turn can launch more than
one — a reviewer that sends the work back, or a person's answer carrying a
parked run on, relaunches the executor under the same row — and
`executor_tail` is asked by it, so a run's page polls the live
output of the job it shows rather than of whichever replaced it.
`provider_key` and `model` name the member of the seat's executor chain the
job was launched on, which a fallback at launch makes other than the chain's
head. `provider_key` is the company's configuration and is sent to an admin
alone; every other reader, a seat's lead included, is told the model the job
runs under and not the entry. `bridge_calls` is always an array on a row the
store answered — every run's tools reach it over the bridge, so an empty one is
a run that has called nothing yet — and absent only where the run's body is
withheld from the reader.

`box_exists` and `paused_at` stand in for the sandbox id: a board wants to
know that a box exists and that it is currently paused (and being billed
for as a snapshot), not which box it is. `paused_at` is the answer the
[pause reaper](../concepts/code-sandbox.md#mid-run-clarification-ask_and_wait)
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
[`answer_executor`](#answering-a-parked-executor) names it by its `turn_id` instead.

`audience` is the harness's own label for who should answer —
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
it addresses, matched on its handle. A login that holds no seat
is put nothing, and answers an empty board. It is a filter over the board and
not a personal read, so it has no scope rule beyond that: the unfiltered board
already names every run's audience. A run whose question resolved to nobody —
the label named no one the chart has and the seat has no lead chain, or the
node held no company when it parked — matches no one.

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
- **`limit`** is configuration — the company's and a seat's, from the active
  revision — and **absent** where no ceiling caps the window — never `0`, which would state a
  range of nothing that is already full;
- **`refused_at`** is when a capped window last turned a call away — a refused
  charge, or work turned away unsent because the window was already full: a
  turn's next call, a parked delivery, a person's question, a reflection pass —
  kept in the same counter and cleared by the scope's next admitted charge or by
  the window turning over, and absent while it has not;
- **`state`** is the engine's judgement — `refusing`, `near` or `ok`, exactly as
  on the [live meter](#the-live-token-meter) — and `near_fraction` beside it is
  the one threshold behind `near` (0.9), for a screen that draws it as a mark.

Where the counter is already on a **later** window than the moment of the
answer — a peer's clock a few seconds ahead across a boundary, or the company's
`timezone` moved west — the row states the later window's spend, because that
is what the gate refuses against. Each window's allowance comes back when it
turns over; there is no route that resets a counter, and room before then is
made by raising the ceiling.

**`assistant`** is the ceiling every person's Ask Crewlet questions are
held to (`assistant.token_budget`): its three windows, each with the `limit`
every person is held to where one is set. `used`, `refused_at` and `state` in
it are the CALLER's own spend against that ceiling, and **`counted`** says
whether they are real: a person's Ask Crewlet spend is counted on a scope of
their own only while the company caps it, and a caller who is not a person
spends nothing through Ask Crewlet, so where `counted` is `false` every
`used` is a zero nobody measured and a client draws no figure. It is here
because it is what refuses a person's question, and the Budgets screen is
where that refusal's remedy sends whoever may raise it.

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
         "resets_at": "2026-06-08T22:00:00Z", "used": 102120, "limit": 100000,
         "refused_at": "2026-06-08T07:29:51Z", "state": "refusing"},
        {"period": "week", "window": "2026-W24", "starts_at": "2026-06-07T22:00:00Z",
         "resets_at": "2026-06-14T22:00:00Z", "used": 301877, "state": "ok"},
        {"period": "month", "window": "2026-06", "starts_at": "2026-05-31T22:00:00Z",
         "resets_at": "2026-06-30T22:00:00Z", "used": 702311, "state": "ok"}
      ]
    }
  ],
  "assistant": {
    "counted": true,
    "windows": [
      {"period": "day", "window": "2026-06-08", "starts_at": "2026-06-07T22:00:00Z",
       "resets_at": "2026-06-08T22:00:00Z", "used": 41200, "limit": 200000, "state": "ok"},
      {"period": "week", "window": "2026-W24", "starts_at": "2026-06-07T22:00:00Z",
       "resets_at": "2026-06-14T22:00:00Z", "used": 120400, "state": "ok"},
      {"period": "month", "window": "2026-06", "starts_at": "2026-05-31T22:00:00Z",
       "resets_at": "2026-06-30T22:00:00Z", "used": 380900, "state": "ok"}
    ]
  }
}
```

`durable` carries the honesty. It is `false` when the shared counter could not
be read, and every window list is then empty: a counter that cannot be read is
not a counter that reads zero, and without the flag a coordination blip renders
every seat at the bottom of its ceiling, which is the most reassuring possible
picture drawn at the moment nothing is known. Human seats have no row, because
they spend nothing.

Exhaustion is the engine's `refusing`, never a ratio a client computes, so
every surface and the [budget park](../concepts/agent-runtime.md#the-budget-park)
agree on which windows can take another round. A refused round is counted like
any other — the vendor billed it — so a window that refused one reads past its
ceiling by that round: the Engineer above had 99 120 of 100 000 when a
3 000-token round was refused, and reads 102 120.

### `POST /backup`

Copies this node's durable state — both of its store files, every JetStream
stream and coordination bucket, and every object holding a company file that
the store copy names — into `?dir=`, a directory **on the engine's host**.

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
  "stores": [
    {"estate": "node", "file": "store.db", "source": "/data/company.db",
     "bytes": 258048, "sha256": "…", "migrations": ["0001_events.sql", "…"]},
    {"estate": "replicated", "file": "store-replicated.db",
     "source": "/data/crewlet-replicated.db", "bytes": 131072, "sha256": "…",
     "migrations": ["0001_tracker.sql", "…"]}
  ],
  "objects": {"dir": "objects", "objects": 40, "bytes": 41943040, "reused": 36,
              "reused_from": "/var/backups/crewlet/2026-08-29T18-00/objects"},
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

Four refusals, each pointing somewhere different:

- **401 without a credential, and 403 without `admin`.** This writes
  every credential the company holds to a path the caller chooses, so it is
  the deployment's own control and nothing narrower reaches it.
- **400 for a destination this node cannot use** — relative, already occupied,
  one this host cannot create, read or make private (a path through a regular
  file, a parent that does not exist or is not writable, a read-only mount), or
  a path the database engine mishandles. The reason is returned in `detail`
  rather than only logged, unlike every other route here, because it is the
  caller's own command to fix. A disk that fails or fills while the directory
  is prepared is the node's failure, not the path's, and answers `500`.
- **503 `objects_unreachable`** — the store copy names objects holding the
  company's files that the object store did not answer for. Nothing is wrong
  with this node or the command: check that it reaches the store and take the
  backup again. A backup without those objects is refused rather than written,
  because they may well be intact there. An object the store *answered* it does
  not hold, or holds as bytes other than the ones the file records, is
  different — it is lost whatever the backup does — and is listed in the
  manifest's `objects.lost` (each with the file that named it, as
  `{"object", "named_by"}`) rather than refused, so one lost file never stops
  every later backup and, with them, the trim (see
  [Backup](../guides/backup.md)). On an S3 bucket each object is copied into
  `objects/` under the bucket's own layout and checked against its row as it
  is written. On the default `nats` backend the objects are in a stream the
  backup snapshots with every other (`objects.stream` names it, and no object
  is copied on its own); once the snapshot is taken the store is asked about
  each object the copy names, so the same refusal and the same `lost` list
  apply there too.
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

### `POST /objects/migrate`

Copies every object the company's files name from the fleet's object store
into an S3-compatible bucket, and on the final pass moves the fleet's record
of where its files are. `crewlet objects migrate --to s3` is a client of this
route; the procedure is in [Moving the files to a
bucket](../concepts/object-store.md#moving-the-files-to-a-bucket).

| Parameter | Meaning |
|-----------|---------|
| `target` | Required. An absolute path **on the engine's host** to a Tier A file whose `store.objects` block names the bucket (`backend: s3`): the file every node runs with once the move is made. Its `${VAR}` references resolve from the engine's environment, so the bucket's credential never crosses this API. |
| `final` | `true` runs the final pass, which seals the `nats` bucket and moves the fleet's record. Without it the pass only copies. |

```json
{
  "from": "nats",
  "to": "s3:https://s3.example.com/crewlet-files/acme/",
  "final": true,
  "sealed": true,
  "moved": true,
  "direct_upload": "available",
  "pass": {"referenced": 18211, "present": 18190, "copied": 21,
           "copied_bytes": 4194304, "unreadable": 0, "complete": true},
  "next": "The fleet's files are in …"
}
```

`pass.present` counts objects the bucket already held at their size (and
digest, where the provider keeps one), so a pass that stopped resumes by
running again. `pass.unreadable` counts objects the fleet's own store could not
give back whole, missing or damaged there, named in `pass.unreadable_files`;
no copy can bring them back, and `objects_missing` already counts them.

It is **synchronous** and runs to its end even if the client gives up, for the
backup's reason. It needs `admin` and a recent proof of identity, like every
deployment control. Refusals:

- **409 `objects_migration_refused`**, with the reason in `detail`: a target
  that is not an absolute path, does not load, names no bucket, names the
  store the fleet already uses, or fails the bucket's probe; a node holding no
  copy of the company's records (run it against a data node); and for the
  final pass, a node not in `maintenance` mode, any node still admitted to
  publish, an estate this node has not applied to the end, one it holds
  incomplete, or rows it cannot read that may name a file: a data node the
  fleet counts running a state log this build does not, or this node's copy
  of the estate migrated by a newer build ([What a build cannot
  see](../concepts/object-store.md#what-a-build-cannot-see)). A final pass
  copies only what this build's tables name, and the store it leaves behind
  is deleted after it.
- **503 `objects_migration_failed`**, with a `Retry-After`: the fleet's store
  or the bucket stopped answering. The body's `report` counts what was copied;
  the stores' own words are in this node's log. Run it again.

### `GET /backups`

Backs **Settings › Backups & retention**: what the fleet has backed up. **It
takes `admin`** — every row names a directory on a named host that
holds the company's sealed credentials.

Two records, because they answer two questions:

- **`points`** — each node's NEWEST backup, from the fleet's backup register:
  what each node announced when its manifest was written, plus the operator's
  newest acknowledgement of each log (`crewlet retention ack`, `kind:
  "operator"`), one point per log acknowledged, each covering that log alone.
  `counted` says the trim may count it — the `backup_floor` policy (`policy`)
  takes this owner's word and the copy was verified — and a point is `newest`
  where some log's backup term reads it: for at least one log it covers, the
  newest counted point covering that log. Several can be — under `operator`,
  one per log acknowledged. `bytes` is the whole
  artefact (every database copy and stream snapshot) and is absent on an
  acknowledgement, which asserts a copy the engine never saw. `covers` is how
  far the copy reaches in each state-log stream. A node the fleet has
  [evicted](../guides/retention.md#eviction) from every log has no point: the
  trim's duty forgets it once the eviction takes effect.
- **`history`** — every `POST /backup` a person made, newest first, from the
  [runtime audit](#the-runtime-audit-sourceoperator) every data node keeps for
  the event log's 30 days: when it finished, the `node` whose disk holds it, who
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

Every count is over **one window**, from `traffic_since` (inclusive) up to the
instant the answer was read (exclusive), and `traffic_since` is what makes the
counts a measurement: "42 inbound" alone could be an hour or a year; "42 since
Tuesday" is not. The window is the widest one whose every delivery the answer
holds. The deliveries are **page-capped, not time-bounded** — the most recent
page of the delivery log, at most 400 deliveries across the fleet — and which
window that is depends on whether any delivery may lie past the page, never
on how long the page is:

- **Nothing lies past the page** when no node holds 400 or more deliveries in
  the 30-day history, no node's reply had to be cut to fit the transport, and
  the fleet holds no more than 400 between them. The page is then every
  delivery the history keeps — it can hold exactly 400, two nodes holding 200
  each — and the window is **the whole history**: `traffic_since` is 30 days
  before the instant the answer was read, so that instant is `traffic_since`
  plus 30 days. An empty page is this case too: no delivery in 30 days is a
  count of `0` over those 30 days, beside the drops and merges of the same
  30 days.
- **Deliveries may lie past the page** otherwise — the fleet holds more than
  400, or one node holds 400 or more (its own page filled, which it reads as
  more behind it even at exactly 400), or a node's reply was cut to fit the
  transport (a page that can then be shorter than 400). The window reaches no
  further back than the page does: `traffic_since` is **one microsecond after
  the oldest delivery the page reached**. Every later delivery is on the page,
  but one sharing that instant may not be, so the deliveries at that instant
  are left out of the window rather than counted short.

`traffic_since` is `null` only when no event log could be read, which
`traffic_known: false` says. It is written to the store's own resolution
(RFC 3339 with fractional seconds, at most microseconds), because an edge cut
to the second would name a window up to a second wider than the one counted;
`last_at` is written the same way.

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
  "traffic_since": "2026-06-07T09:12:00.418207Z",
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
      "last_at": "2026-06-08T07:31:10.052914Z"
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
misleading apart. `inbound` counts **deliveries** — one delivery presented to
one seat, counted once across the fleet — that arrived at this row's own
ingress (`inbound_path`, or the websocket for Mattermost): a message two
agents' Slack apps both receive is two, a Mattermost post two bots' sockets
both read is two, and the same post read by every node holding the bot's
socket is still one, because the fleet claims it before it is published.
Counted by **ingress**, not by integration: the Forge relay hands Jira and
Confluence events on under its own token and they belong to those products, so
a relayed event counts once, under `forge`, and the Atlassian card, which sums
its surfaces, counts it once — while what became of it (`skipped`,
`coalesced`) counts under the product it was parsed as.
`skipped` counts those the routing gate dropped without waking anybody;
`coalesced`
counts merges, where N same-conversation notifications became one turn. "128
arrived" on its own cannot tell a working integration from one whose every
delivery reaches nobody — "128 arrived, 30 dropped, 2 merges" can, and a seat
draining a thread's backlog as one turn stops looking like a seat that ignored
twelve messages. All three cover the window above, and so does `last_at`, the
newest delivery in it (`null` when it holds none), never earlier than
`traffic_since`.

The two outcome counts are **three-valued** like the secret fields: `null`
means this node could not read its event log, and reporting that as `0` would
claim every delivery woke a seat on a node that cannot tell. They come from
the engine's own `notification_skipped` and `notifications_coalesced` events
rather than from the inbound rows, and they are bounded by the same event-log
window `traffic_since` names: every node counts its own outcome events from
`traffic_since` up to the instant the inbound rows were read — the same
instant every node floors the 30-day history at — and the node you asked sums
them, counting once a row two data nodes hold while a stateless node's custody
batch is still being settled
([Reading the fleet's history](../concepts/event-system.md#reading-the-fleets-history)). They are counted, not paged, so every outcome in the window is in the
count however many more of them there are than deliveries, and one before
`traffic_since` or written after the answer was read is not. A row counts
under the third-party app its `notification_source` tag names; one whose event
names no app carries none and is not counted. The window is one for
the whole answer, so a surface whose drops and merges outnumber its deliveries
has its outcomes counted over the window every surface's deliveries leave: the
whole 30 days whenever nothing lies past the page (a company receiving nothing
at all included), and only as far back as the page reaches when the busiest
surfaces' traffic fills it.

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
the hierarchy. A seat in a unit is a seat: the list is read from every seat of the
company's org chart, at any depth, not from the top-level `roles:` alone —
which is by definition only the seats belonging to no unit.

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
`false` when this node could not read the event log at all, so the zeros
below it are absence of measurement rather than measurement of absence.

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
      "enabled": true, "timeout_seconds": 1800, "catchup": true,
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
a unit's key for a unit one — which is what the at-most-once ledger keys a fire
on and what `schedule_runs` takes as its parameter. `scope_name` is the same
scope as a person reads it (the handle, or the unit's key), resolved through
the company this node is running.

---

## Token Spend Breakdown

Two sources, one aggregation, and which one answers is decided by the
parameters:

- **The live window** — a request naming no `days`, no dates, no `seat` and no
  `previous` — is the projection's: the spend records of the last
  24 hours (`livestate.LiveSpendWindow`, rolling), held in memory and pushed as
  the [`tokens` push](#pushes), for a client that wants the last day as it
  happens. No screen of the bundled dashboard draws it: Settings › Token spend reads
  named windows only, so its figures are the company's. It is the only answer
  with a per-turn tail (`by_turn`) and a watermark (`aggregated_through`).
- **Every named window** is whole **company days** read from the replicated
  [`usage` domain](../guides/replication.md#two-compacted-domains-the-embeddings-and-each-data-nodes-day) (ADR-0020): every
  data node's day, applied on every data node. So the answer is the same whichever node
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
| `since` / `until` | — | Instead of `days`: two company dates, `2026-06-01`, **both inclusive** — `since=2026-06-01&until=2026-06-08` is eight days. A pair or neither; at most 90 days — a longer pair is **400** (`tokens.ErrWindowLength`: `2026-05-01 to 2026-09-29 is 152 days, and a spend window is at most 90. Bring since and until closer together`) wherever in the history it lies; never together with `days`. |
| `previous` | `false` | The same number of company days ending the day before the window begins — compare-to-previous, cut on the company's calendar rather than a browser's, so the two windows are never different weeks. |
| `seat` | (every seat) | One seat, by its **handle** — resolved to the agent id every node derives from it, so a seat since removed from the company still answers for the days it left behind. A person's seat is refused `bad_params`: it spends nothing. |

A window whose first day — or whose `previous` window's first day — is older
than the history's floor is **400** (`tokens.ErrOutOfRange`, a different
class from a window that is merely too long) naming the parameter to change, never answered short: the rows before the floor are gone
on every data node, and a heading over fewer days than it names is a lie about the
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
    { "provider_key": "claude", "models": ["claude-sonnet-5"],
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
  emits (`onboarding`, `execute`, `review`, `subagent`, `judge`), as
  recorded — `execute` with every launch of the executor's run, whose
  `executor_collected` record names no phase and is filed there — and
  `auxiliary` — what the seats' auxiliary model spent, from the
  `auxiliary_spend` records: the turn-start context, every compaction, the
  reflection workers, the background learning passes and a person's answered
  question. The series folds these into four bands; the rollup does not.
- `by_worker` breaks the `auxiliary` phase down by what it was FOR — its
  purpose: `memory_filter`, `knowledge_query`, `episode_summary`,
  `persist_decider`, `counterparty_profiler`, `skill_synthesizer`,
  `skill_refiner`, `episode_compaction`, `skill_clustering`,
  `skill_promotion`, `provider_probe`, `answer_knowledge`, and `condense_<kind>` for each kind of
  text a compaction rewrites. Its rows sum to that phase.
- `calls` is PROVIDER CALLS on every bucket: a phase's model rounds, a
  launch's own harness turns as its harness reported them (`harness_turns`, one for a
  launch whose harness reports none — OpenCode reports none), an auxiliary record's
  coalesced calls. The executor's own record counts none: its run's calls are
  its launches'. It counted records once, which made a forty-turn run one call.
- `by_provider` answers "which configured entry (`providers.llm.<key>`) do we
  pay for", which `by_model` cannot: a fallback chain serves several models
  under one key. `models` is every model the entry answered with, biggest
  first; `seats` the three handles that spent the most through it and
  `seats_total` how many did at all. A call recorded before the key was
  promoted (node migration 0032) is under `unknown`. **An admin's alone**:
  the entries are the company's configuration, so every other reader, of
  this answer and of the socket's `tokens` push, is sent `by_provider` empty
  and `entries_withheld: true`, with every other figure unchanged. A model a
  call did not name is `unknown` in `by_model`, never its entry's key.
- `by_agent[].turns` and `failed` are how many of the seat's turns ENDED in
  the window, and how many of those failed — a named window only. The live
  window holds phase records, not endings, so it carries neither rather than
  a count of "turns that spent", which is a different number. A seat that
  ended a turn without spending is still listed.
- `by_turn` — one row per RUN, newest first, capped at 50, each the turn's
  COST: its phases and its in-turn auxiliary records, never the reflection
  after it, which is the seat's learning rather than the work's cost — and
  `aggregated_through`, the newest record counted, are the **live window's
  only**. A company day holds no turn and no per-call instant, so a named
  window has neither. Per-turn spend over any window is
  [`GET /turns?sort=-tokens`](#queries). Both carry each record's stamp as it
  was published, so a record from a node whose clock runs fast puts them past
  `until` for the day the window holds it — which is how that node is found.
- A seat is one row per derived agent id, on both windows, named from the
  company — its handle and name — or, for a seat the company no longer holds,
  by the newest day that named it: a seat whose display name changed
  mid-window is one row under its current name. `agent_id` and `seat` on the
  answer echo a `seat=` narrowing the same way, absent when nothing narrowed it.
- A **person** is a row of their own with `person: true`, on both windows:
  what the auxiliary model spent for a human seat — a question answered with
  `answer_knowledge`, a background pass on a unit a person leads — keyed and
  named by that seat's `handle` (never a person's id in the identity
  directory), with no `agent_id` and, on a named window, no `turns` or
  `failed`, since a person takes no turns. A caller bound to no seat charges
  the company alone, and has no row. `seat=<handle>` narrows a window to one
  person as it does to one seat. A person's spend reaches the named windows as
  its own usage record, published with the rest of the day of the data node
  whose event log holds its spend.
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
  harness reports a price, so a `cost_usd` of 0 over `priced_calls: 0`
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
| `group` | `phase` | The dimension the bands are: `phase`, `model`, `provider`, `seat`, `unit` or `worker`. Anything else (`turn` included) is **400** naming the set. `provider` is an admin's alone, its bands being the company's `providers.llm` entries, and is refused to everybody else on authority. `phase` is the **four bands** below; `seat` is keyed by the seat's agent id (never a name, which two seats may share), and carries its name as its `label`, beside its `handle`; `unit` resolves through the org chart's DIRECT unit for each seat, keyed by the unit's key and labelled with its name, not the chain, because a band per nesting level would count the same spend for the team and again for the department above it. A usage row carries no project and no work item; that attribution is Worklet's own per-item counters. |
| `bucket` | `day` | `day` (a company day) or `week` (the company's ISO week, from Monday midnight on its clock). Nothing finer: a day is the smallest thing every data node's usage agrees on, and `hour` is **400**. |
| `groups` | `4` | How many bands before the rest fold into the residual. Capped at 20. Four is how many data hues the design system has, and exactly the phase breakdown's band count — a phase grouping never folds. |

**The four phase bands**, folded once in `tokens.PhaseBand`:

| Band | Phases |
|------|--------|
| `execute` | `execute` — each launch of the executor's run included, recorded from the box it ran in (`executor_collected`) |
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

Every delivery — one delivery presented to one seat, counted once across the
fleet — writes one row to the event log under `category: "webhook"`, so the
listing answers "what has been arriving" without reading a payload. Both
inbound edges write it the same way: a webhook route for each delivery it
verified, and the [Mattermost socket](../integrations/mattermost.md#running-on-a-fleet)
for each post it claimed. Each publishes an `inbound_delivery` event, which the
node's own event log files — or, on a node without `data`, a data node's,
through custody — so a delivery read on any node is a row:

| Field | What it carries |
|---|---|
| `type` | The delivery's own label: `webhook:<event>` from a webhook route, `forge:<event>` for an Atlassian Cloud relay, `socket:posted` for a Mattermost post |
| `source` | The integration the **payload** belongs to — the route for six of the seven webhook routes, the relayed product for Forge, and `mattermost` for a post |
| `summary` | The delivery in one sentence |
| `tags.recipient` | The seat a per-seat delivery was addressed to — a Slack or GitHub app's seat, or the bot whose socket read a post — absent for a company-wide one. The log resolves it to that seat's agent id and indexes the delivery under it as a **party**, so `GET /events?agent=<handle>` also returns what reached an agent seat from outside |
| `tags.delivery_key` | The provider's own delivery id — a delivery header, or a Mattermost post id — absent for the providers that send none (the Forge relay, Datadog, and an Atlassian build without the identifier header, which this edge deduplicates on a hash of the body instead): what an operator has in front of them in the provider's console |
| `tags.route` | The ingress that authenticated it, which is not always its `source`: a Forge-relayed Jira event is `source: jira`, `route: forge`. The integrations answer counts `inbound` by it |
| `payload` | The **raw body the provider sent** (a Mattermost post as its socket carried it), on `GET /events/{event_id}` only. A listing never carries a payload, so a deliveries screen is one request rather than one per row |

Note that a row exists only for a delivery that was **verified, claimed and
queued**. A refusal — a bad signature, an unset secret, a body too large — is
answered at the edge and appears in the engine's log rather than here; a post a
peer node already claimed is that node's row; and a post whose publish failed
is read again rather than recorded.

---

## The Tool Catalogue

Carried by `GET /tools`, by the `tools` push, and inside the socket snapshot.
One row per registered tool, in the same shape as each tool on
[`GET /mcp-servers`](#get-mcp-servers)'s built-in groups:

```json
{
  "name": "post_message",
  "description": "Post a message to a channel",
  "source": "mcp:slack",
  "title": "Post message",
  "annotations": {
    "read_only": "no",
    "destructive": "no",
    "idempotent": "unknown",
    "open_world": "yes"
  },
  "delivers": "slack",
  "input_schema": { "type": "object", "properties": { "…": {} }, "required": ["…"] },
  "group": "",
  "gate": ""
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
  read-only MCP tool delivers nowhere, and Worklet's comment tool
  delivers although it is a builtin.
- `title` is the human-readable name a server advertised, omitted when it
  advertised none.
- `input_schema` is the JSON Schema the model is offered, verbatim. It is
  **absent** when the tool takes no arguments, which is not the same as `{}`:
  only the first means this build did not send one.
- `group` and `gate` are, on one of the engine's own tools, the group it is
  filed in and who may call it: what its row in the authority table asks of
  the caller, since holding a tool is not being allowed to call it. The words
  are listed in
  [Groups, and who may call each one](../guides/tools-and-mcp.md#groups-and-who-may-call-each-one).
  Both are present and empty on an MCP server's tool, whose calls no
  authority row decides.

---

## Webhook Endpoints

### Webhook refusals

A refusal on every `/webhooks/*` route is the engine's one [refusal envelope](#every-refusal-is-one-envelope) — `error`, the code a client branches on, and `message`, the sentence the code carries — and every `503` says when to come back in `Retry-After`. A sender reads only the status and the `Retry-After`, and both are chosen for it: a `401` is a delivery that will never verify, a `503` is one to hold and retry. The body is for the operator reading a vendor's delivery log, a proxy's access log or the engine's own.

| Status | `error` | When | `Retry-After` |
|---|---|---|---|
| `401` | `invalid_signature` | The delivery's credential did not verify — an HMAC signature, a shared token or a Forge invocation token, missing or wrong — or a per-seat Slack delivery names a seat that has no Slack app here while other seats have one. The log says which check failed; the body never does, because that is what somebody probing the route would read, and a code of its own for the seat with no app would be a list of which handles have one | none |
| `503` | `no_active_revision` | This node runs no company yet, so it has no secrets and nothing to route by. Answered **before** the signature check, because verifying first would answer every delivery with the no-secret `503` and its five-minute wait | `15` — the reconcile poll that brings a node a missed activation |
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

The OAuth install landing every agent's Slack app redirects to. An install begun with **Install** on the Slack integration's Agents tab carries a `state` the engine signed (the seat, its app, a single-use nonce and a 15-minute expiry): the page verifies and spends it, exchanges the `code` for the bot token, refuses a token that belongs to another agent's app, and seals it under the seat's own name. A link used twice, edited or expired is refused with what to do instead, and neither the code nor the token is ever shown. An install made from the app's own settings page carries no state, and the page says where to copy the bot token. A link from the retired `crewlet slack provision` is answered `410` with where installs are made now. A refusal Slack sends back is said in the page's own words (the install was cancelled, or Slack didn't approve it) and its code is logged as `slack_oauth_refused`, never shown; no answer is cached (`Cache-Control: no-store`). Unauthenticated by design, since a browser redirect carries no engine credential. Every value on the page comes from the query string, so it is served under a policy that allows its one inline style by hash and no script at all (see [Security headers on every response](#security-headers-on-every-response)).

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
and reissues neither. A record of the app on the seat that the coordination
store took without answering is read back once before the page answers, and
is a completion when the seat has the app.
A link presented again (a refresh, or Back from GitHub's
install page) runs nothing, and is answered by whether the agent it names has
its app. Answers `200` for a completion, an install, or a link presented again
whose agent has its app; `400` for a refusal from GitHub, a missing code, or a
link or code that is expired, forged or already used; `403`, `404` and `409` as
the begin route does for a managed document, an agent the company no longer has
and no company; `502` when GitHub didn't finish the conversion; and `500` when
Crewlet couldn't (the link couldn't be checked, the company couldn't be read, the
key or webhook secret couldn't be sealed, or the app couldn't be recorded or
its record confirmed). See
[the table](#one-agents-own-github-app) for why none is a `503`. What the page says
is always the engine's own wording, chosen by the step that stopped so it says
what to do (see
[GitHub Integration](../integrations/github.md#when-a-creation-doesnt-finish)),
and never an error's text or GitHub's: GitHub's response body here carries the
private key, and the error is the log's (`github_app_create_failed`, which names
the app GitHub handed over, never its key). Every answer carries
`Cache-Control: no-store`.
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
crewlet run -config crewlet.yaml -roles data,ingress -api-host 0.0.0.0 -api-port 8000
```

The API is served inside the engine's own process, over the engine's own store, broker and coordination plane. Its reads answer from that node's store and projection, and `/config`'s from the fleet's activation pointer, which every node answers alike. Its writes are few and each is named above: the operator surface (`/operator/mcp`, `/operator/act`) writes to Worklet, Doclet and Chatlet through the tools a seat holds, `/config`, `/secrets` and `/setup` write the company's configuration and credentials, `/backup` and the `/worklet/retention` gestures act on the node and the log, and the webhook edge publishes inbound deliveries onto `crewlet.notifications.inbound`. It runs no agent itself: a seat's turn is the engine's.

See [Deployment](../guides/deployment.md) for how the API and engine run together, and the integration docs ([Slack](../integrations/slack.md), [Jira](../integrations/jira.md)) for webhook setup.
