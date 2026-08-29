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

## Provider Abstraction via Protocols

All external dependencies (LLM, embeddings, storage) are behind interfaces **defined by the package that calls them**, kept to what that caller needs — there is no `interfaces.go`, and a provider package exports a concrete type. No vendor SDK lock-in. This enables:

- Different roles using different LLM providers/models
- Configurable embedding providers for the agent-learning subsystem (e.g., OpenAI, or any compatible endpoint)
- In-memory implementations for unit tests (not used in production code paths)
- Easy addition of new providers without touching core logic
