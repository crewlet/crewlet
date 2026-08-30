# Design Decisions

Key architectural choices in Crewlet and why they were made.

---

## One Queue Contract, Several Backends

Messaging is a contract, not a product. One interface with three certified
backends — an in-memory twin, an **embedded NATS JetStream** server (the
default: it runs inside the engine process, so a company needs no broker at
all), and **Apache Pulsar** for a fleet that wants one. A backend the
conformance suite has not certified does not exist as far as the engine is
concerned, and **nothing above the queue package may branch on which one is
running** — that rule is what keeps the twin honest and the default
operable.

What the contract demands of any backend:

- A **durable subscription is a seat's mailbox**: it exists without a
  consumer, retains what is published while nothing is attached, and replays
  in order when someone attaches. Publishing to a subject with no
  subscription drops the event silently, which is why creating one is an
  explicit step rather than a side effect of subscribing.
- Handlers have **three outcomes, not two** — ack, nak, and *defer* (leave it
  unacked and stop consuming), which is how a node hands a seat back without
  losing its mail.
- Attachment has **four verbs of differing destructiveness** — quiesce,
  unquiesce, detach, delete-subscription. A single `unsubscribe` never said
  which one it was, and the difference is whether a seat's mail survives.

On Pulsar specifically:

- **Persistent topics**: each internal subject (`crewlet.agent.*.inbox`, `crewlet.notifications.*`) maps to a persistent topic `persistent://{tenant}/{namespace}/{subject}` (default `public/default`; configurable via `stream.tenant` / `stream.namespace`). Tenants and namespaces are never auto-created, so a custom pair is created out-of-band by the operator; the engine calls the admin REST API only for subscription lifecycle, where both create and delete must work with no consumer attached (see [Seat ownership](../concepts/seat-ownership.md#the-unowned-seat)). Each durable subscription's undelivered backlog survives engine restarts; time-based retention of already-acknowledged messages is an optional namespace retention/TTL policy (off by default in a standalone broker).
- **Competing consumers** (per-agent inboxes, notification fan-out) map to **Shared subscriptions** — one shared cursor per consumer group, so each message reaches exactly one member. **Broadcast** stream consumers (dashboards) map to per-caller **regex topic-pattern subscriptions**. Poison-message redelivery is capped by Pulsar's **dead-letter policy**.
**A2A channels** ride the same agent inbox subjects as everything else: the ask wakes the target's seat, the answer wakes the asker's. They used to be an in-memory queue per channel, on the premise that every agent shares one process — which stopped being true the moment a fleet ran more than one node. The queue lived on the node that opened the channel while the wake was delivered to whichever node owned the target's seat, so the target woke to an empty channel. Channel *state* (participants, open/closed) is durable for the same reason: every authorization decision reads it, and the two parties are usually on different nodes.

**Infrastructure footprint**: none by default. The embedded stream and the local store file mean a company runs on one binary; Pulsar is what a deployment adds when it wants durable per-subscription cursors, dead-letter queues and namespace retention policies across a fleet.

---

## Goroutines for Concurrency

Everything concurrent is a goroutine whose lifetime belongs to whoever started it, and every one has a way to stop. This is real parallelism rather than one cooperative loop, which is the single biggest behavioural change from the engine's first implementation: every "atomic because it is single-threaded" assumption became something to prove, so CI runs the whole suite under `-race`. A blocking MCP server needs no special handling — a goroutine blocking on I/O costs nothing.

---

## Event Schema Versioning

Every persisted event can carry versioning. Schema changes must be **additive only** — new fields with defaults, existing fields never removed. Consumers ignore unknown fields. This allows old and new events to coexist in the same stream without migration.

---

## Hot Reload: An Activation Epoch, Applied Per Node

A running company takes a new configuration without a restart. **Which** revision is current is a fleet-wide fact; **applying** it is a per-node act.

Within one process, applying a revision is shared-memory work: the engine swaps the `Organization` object, cancels removed agents' handlers and publishes `AgentTerminated`, spawns new ones onto their inbox subscriptions, and updates a modified `AgentDefinition` in place so the agent picks it up on its next turn. Every agent handler is a goroutine in that process, so the swap has nothing to propagate — but they run in genuine parallel, so what they share is guarded rather than assumed safe, and the race detector is what holds that.

Across processes that is not enough, and the way it failed is worth naming: activation used to be delivered over a competing-consumer subscription, which means **exactly one replica applied a revision and every other node went on running the previous company indefinitely** — with no error anywhere, because from each node's point of view nothing had happened.

So the fleet-wide half is an **append-only activation epoch** that every node polls and reconciles against, recording its own outcome (`ok` / `error` / `degraded`) so a stalled rollout is visible rather than silent. The in-process steps above are what a node does once it decides to move. See [Control Plane](../concepts/control-plane.md).

The log is append-only rather than a pointer row for a specific reason: **re-activating an unchanged revision is the documented credential-rotation gesture**, so "the payload did not change" cannot be treated as "nothing to do".

---

## Unique Agent Model (1:1 Role → Agent)

Each Role defines a unique individual agent — not a pool of interchangeable instances. This was a deliberate choice:

- Agents have distinct personalities, backstories, and domain expertise
- Task assignment is a **team lead reasoning decision**, not load-balancing
- External identity (Slack bot, Jira assignee, email) maps to one entity
- Simplifies the model: no `role.count`, no suffixed handles, no `AgentPool.get_available()`

---

## Team-Lead-Driven Assignment

There are no algorithmic assignment strategies — no role-based, hierarchical, or claim-based auto-routing. Task assignment is a **team lead reasoning decision**: the lead agent assigns the work item in the external PM tool (Jira, GitLab issues, …) through that tool's own MCP tools.

The team lead's Plan-phase system prompt includes a compact team roster (names + handles), plus a per-member detail block (backstory, goal, responsibilities, Jira identity, skills) rendered directly from the in-memory `Organization` model so the lead has full context when reasoning about an assignment.

---

## External PM Tool as Source of Truth

Task lifecycle lives in the PM tool (Jira, GitHub/GitLab issues), not in the engine, which keeps no task state at all — no assignee map, no dependency graph, no reconciliation poller. A mirror of somebody else's task state is a cache with no invalidation story; keeping nothing means there is nothing to be stale. See [Task Engine](../concepts/task-engine.md).

This avoids duplicating task state and keeps the audit trail in the tool the team already uses.

---

## One Store Driver, and the Platforms It Bounds

The store is **Turso** (`turso.tech/database/tursogo`), and it is the only
driver. There was a second — mainline SQLite (`modernc.org/sqlite`) behind
`store.driver` in the Tier A file and `CREWLET_STORE_DRIVER` in the
environment — kept as an escape hatch, and the escape hatch turned out not to
be one. Only Turso has the vector distance functions the [agent-learning
subsystem's](../concepts/agent-learning.md) recall reads through, so an
operator who flipped the variable kept every table and lost their agents'
memory, with nothing saying so. Both the field and the variable are retired: a
Tier A file that still sets the field is refused with a message naming the
change rather than reported as a misspelling.

**Nothing about the data moves.** Turso is SQLite-compatible in its file
format, so an existing store opens untouched, there is no migration to run,
and any SQLite-compatible client still reads the file for forensics.

What the single dialect buys is spent on recall: the cosine distance now runs
**in the database** rather than in Go over every row the seat owns. There is
still no approximate-nearest-neighbour index reachable from the Go driver, so
recall remains a scan behind the per-agent index — that is the honest claim,
and it is not the same as "native vector search". Nor is full-text search
available: knowledge search is the external backend behind
`knowledge.Searcher`, as it always was.

**The driver also decides what Crewlet ships for.** "Pure Go" is not
"self-contained" here: the database engine is a native library embedded in the
driver module and extracted at run time. Upstream embeds that library for
linux and macOS on amd64 and arm64 — linux in a glibc and a musl variant — and
for windows/amd64, and for nothing else. What Crewlet ships is narrower than
that list, and the two bullets below are why.

- **There is no Windows build.** The release used to publish windows/amd64 and
  windows/arm64, and the second had no library at all: it started, then failed
  at its first query. Shipping one architecture of an operating system while
  silently breaking the other is worse than shipping neither, and a Windows
  binary was in any case the one that refused
  [`providers.sandbox: {type: local}`](../concepts/code-sandbox.md).
- **There is no musl build either, so the linux binaries need glibc.** Loading
  that native library means `dlopen`, which makes the binary dynamically linked
  against `libc.so.6` even though it is pure Go and built with
  `CGO_ENABLED=0`. On Alpine and other musl systems it fails at `execve`,
  reported as `no such file or directory` about a file that plainly exists. Use
  a glibc base image — the published one is `debian:trixie-slim` for exactly
  this reason — or a glibc host. Building it `-static` is not the way out: a
  static program has no dynamic loader and cannot `dlopen` at all. macOS is
  unaffected.

---

## One Knowledge Backend, Behind a Seam That Keeps It Swappable

The knowledge base is **single-homed**: the engine wires exactly one
`knowledge.Searcher`, and every consumer — the Plan-phase `## Relevant
knowledge` prefetch, the first-turn onboarding hint, the cross-agent
skill-promotion pass — reads through it. Two searchers would make an agent's
answer to "what do we already know about this" depend on which one was asked,
and neither would be wrong.

Confluence is the implementation. The seam stays an **interface** anyway, and
that is the decision worth stating: it is declared by its consumers, so a
second backend is a new implementation rather than a rewrite of everything
that searches. The same shape keeps the tool-skill sync taking rendered pages
rather than a backend client, and the promotion writer an interface — both
subsystems stay ignorant of which product answered.

What validation still refuses is a read scope naming a backend the config does
not configure: `knowledge.confluence_spaces` with no `integrations.confluence`
reads as a working narrowing and narrows nothing. See
[Knowledge System](../concepts/knowledge-system.md).

---

## No Dedicated Escalation Mechanism

There is no special escalation mechanism. When stuck, agents hand off to their manager with the same colleague-surface tools they use for any collaboration (Jira comment, Slack mention, A2A) — same as a human would. There is no dedicated handoff decision and no `fallback` chain: Review routes a blocked turn back through `self_iterate` so Plan adds the outreach step, and Execute makes the call. The hierarchy is informational + downward-delegation routing; it does not drive a special upward path at runtime.

Engine-detected failures (stall, max-iter exhaustion, depth cap, unhandled exception, LLM-provider chain exhausted) publish `turn.guard_breach` / `llm_unavailable` events and terminate the turn as `failed`. The dashboard derives an `afk` state from the latest failure event and surfaces a cause-specific quip for the founder. No active push notification is sent — visibility is logs + dashboard, by design.

---

## Webhook Deliveries Are Deduplicated at the Edge

Every inbound webhook is **claimed fleet-wide before it is published**, so a
provider's retry — or an operator's replay — is answered `200
{"status":"duplicate"}` and wakes nobody. The claim lasts five minutes and is
taken *before* the republish, because two concurrent retries must not both wake
the seat; a republish that then fails releases it, so the provider's retry is
not refused by a row nothing clears.

**A stable identifier is used where the provider sends one.** GitHub's
`X-GitHub-Delivery`, GitLab's `X-Gitlab-Event-UUID`, Slack's envelope
`event_id`, and the `X-Atlassian-Webhook-Identifier` that Jira and Confluence
Data Center both send are all repeated unchanged across the provider's own
retries, which is exactly the identity this needs.

**Where the provider sends none, the key is a hash of the raw body.** A Cloud
event relayed through the Forge app carries no delivery header at all, and
which Atlassian Data Center builds set the identifier has moved between
versions. The payload is what stays identical across a retry.

Byte identity is preferred to derived coordinates deliberately. Coordinates —
event, action, entity id, activity id — are the tempting shape and are strictly
worse in the direction that matters: **every field left out of a coordinate set
is a way for two different events to collapse into one**, and a collapsed event
is a message nobody ever answers. A vendor that fires one webhook per changed
field with an identical entity snapshot makes this concrete: a bulk edit is N
deliveries differing only in the activity record. A hash over the whole body
cannot collapse them — any difference at all yields a different key — and its failure
mode is the safe one: a provider that re-serialized between attempts fails to
collapse a redelivery, which is a duplicate rather than a silence. It also
needs to know nothing about the vendor, which keeps three routes from each
growing their own half-right field list.

The one input that must not produce a key is an **empty body**: it is identical
for every delivery, so claiming on it would refuse every later delivery from
that vendor for the whole window.

**The whole mechanism fails open.** No claim store, no key, or a store that
cannot be reached all mean "handle it": a duplicate is recoverable noise, while
a delivery dropped because the store blinked is a message nobody ever answers.

---

## The Config Diff Is Paths and Values, Not Lines

The stored form of a revision is JSON produced by marshalling a struct, so
re-ordering a map or adding a field with a default rewrites lines that mean
nothing to a reader. A textual diff would report all of it as change. The
question an operator actually asks is *"what changed about the company"*, and
**paths answer it** — so `crewlet config diff` and
`GET /config/revisions/{id}/diff` are the same differ, reporting one entry per
changed path rather than a hunk of text.

A string value is quoted and other values are not, because `"true"` and `true`
are different settings and a renderer that printed both bare would show a type
change as no change at all. A diff longer than the cap reports its own
truncation rather than stopping silently.

**Both sides are always redacted, and there is no flag to turn that off.** A
diff is what an operator pastes into a ticket to ask a colleague whether a
change looks right, which is the single most likely way a credential leaves the
machine. `crewlet config export -revision <UUID>` covers the rare case that
needs real values, and it takes a deliberate act.

Two further properties of that surface are worth stating because they are
easy to assume the other way round:

- **`/config` is guarded in full, reads included.** It is one of exactly two
  prefixes — `/secrets` is the other — that a read can never reach through
  `allow_anonymous_read`, held in one list so a third surface cannot be added
  to half of the rule. Reading it exposes the whole company document: the org
  chart, which integrations are wired, and the shape of every credential.
- **A write does not apply anything.** `PUT /config` stores a revision and
  moves the activation pointer; it does not touch the running epoch, *not even
  on the node that served the request*. Every node applies on its own reconcile
  tick — which is exactly what makes a write on one node reach the whole fleet.

---

## Every Vendor Is Served

The engine once refused an `integrations.*` block it had no parser for, on the
theory that a config naming a vendor the build could not serve should fail
loudly rather than be silently ignored. That mechanism is **gone**, because the
premise stopped being true: all six vendors — Mattermost, Slack, GitLab,
GitHub, Jira and Confluence — route end to end, so there is nothing left to
refuse. The table it kept held four rows, and each was struck as that vendor
shipped its parser.

What outlives it is the rule it was built to enforce: **a config block the
engine cannot honour must fail, not be ignored.** A silently dropped
integration block looks exactly like one that is working until someone notices
the messages never arrived. If a vendor is ever added to the schema ahead of
its parser again, that is the shape to rebuild.

---

## Tracing Is Configured by the Standard OTel Environment, Not by `crewlet.yaml`

The OTLP endpoint, headers, protocol, service name and sampling ratio are the
`OTEL_*` variables every collector's own documentation uses — there is no
`tracing:` block in Tier A. Two reasons, and both are about not making you
translate.

An operator wiring a collector should be able to paste the vendor's snippet.
And the engine's own exporter shares those variables with the
[sandbox OTLP forwarder](../concepts/code-sandbox.md), deliberately: the
forwarder stamps a trace context into a coding sandbox so the box's spans nest
under the turn that started them, and two settings would let those halves point
at different backends, where that link resolves on neither.

**The tracer is always running; only the exporter is optional.** A trace id is
not just an exporter's concern here — it is an indexed column in the event
store, the key `GET /events/trace/{id}` answers on, and what the dashboard's
trace view arranges into a tree. All of that works with no collector anywhere,
so ids flow whether or not anything is collecting them, and no part of the
engine branches on whether tracing is "on".

**Spans carry timing; events carry content.** Prompts, responses, tool
arguments and results, and token counts are all already on the phase events in
the store. A span adds the one thing no event records — how long it took — so
span attributes stay to small enums and counts rather than shipping whole
prompts to a tracing backend.

---

## Provider Abstraction via Protocols

All external dependencies (LLM, embeddings, storage) are behind interfaces **defined by the package that calls them**, kept to what that caller needs — there is no `interfaces.go`, and a provider package exports a concrete type. No vendor SDK lock-in. This enables:

- Different roles using different LLM providers/models
- Configurable embedding providers for the agent-learning subsystem (e.g., OpenAI, or any compatible endpoint)
- In-memory implementations for unit tests (not used in production code paths)
- Easy addition of new providers without touching core logic
