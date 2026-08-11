# Design Decisions

Key architectural choices in Crewlet and why they were made.

---

## Apache Pulsar for Messaging

Apache Pulsar is the sole external infrastructure for messaging:

- **Persistent topics** (Pulsar): each internal subject (`crewlet.agent.*.inbox`, `crewlet.notifications.*`) maps to a persistent topic `persistent://{tenant}/{namespace}/{subject}` (default `public/default`; configurable via `providers.queue.tenant` / `.namespace`). The engine is data-plane only — it never calls the admin API, so a custom tenant/namespace is created out-of-band by the operator. Each durable subscription's undelivered backlog survives engine restarts; time-based retention of already-acknowledged messages is an optional namespace retention/TTL policy (off by default in a standalone broker).
- **Competing consumers** (per-agent inboxes, notification fan-out) map to **Shared subscriptions** — one shared cursor per consumer group, so each message reaches exactly one member. **Broadcast** stream consumers (dashboards) map to per-caller **regex topic-pattern subscriptions**. Poison-message redelivery is capped by Pulsar's **dead-letter policy**.
- **A2A Bus**: In-memory `asyncio.Queue` per channel (not Pulsar). Agent threads share the same Engine process, so cross-process messaging is unnecessary.

**Infrastructure footprint**: Pulsar + PostgreSQL. Pulsar buys durable per-subscription cursors, dead-letter queues, and configurable retention/TTL policies; for local work its `standalone` mode bundles the broker, BookKeeper, and the metadata store into a single container.

---

## Agent Threads: `asyncio.Task`

The entire codebase is async (LLM calls via httpx, MCP via async SDK, storage via async drivers). `asyncio.Task` is lightweight (~500 bytes per task vs ~8MB per OS thread) and sufficient. If a blocking MCP server is encountered, wrap with `asyncio.to_thread()` on a case-by-case basis.

---

## Event Schema Versioning

Every persisted event can carry versioning. Schema changes must be **additive only** — new fields with defaults, existing fields never removed. Consumers ignore unknown fields. This allows old and new events to coexist in the same stream without migration.

---

## Hot Reload: Shared-Memory Propagation

Since all agent threads run in the same Engine process, they share memory. Hot reload works as follows:

1. Engine reloads the org model (YAML or API call)
2. Engine updates the shared `Organization` object (thread-safe via asyncio — single event loop, no data races)
3. **Removed agents**: Engine cancels their handler and publishes `AgentTerminated`
4. **New agents**: Engine spawns new instances, subscribes them to inbox topics
5. **Modified agents**: Engine updates the `AgentDefinition` in place — the agent picks up the new definition on its next turn
6. **No message-based propagation needed** — agents read from the same `Organization` and `AgentPool` objects in memory

---

## Unique Agent Model (1:1 Role → Agent)

Each Role defines a unique individual agent — not a pool of interchangeable instances. This was a deliberate choice:

- Agents have distinct personalities, backstories, and domain expertise
- Task assignment is a **team lead reasoning decision**, not load-balancing
- External identity (Slack bot, Jira assignee, email) maps to one entity
- Simplifies the model: no `role.count`, no suffixed handles, no `AgentPool.get_available()`

---

## Team-Lead-Driven Assignment

There are no algorithmic assignment strategies — no role-based, hierarchical, or claim-based auto-routing. Task assignment is a **team lead reasoning decision**: the lead agent assigns the work item in the external PM tool (Jira, Plane, …) through that tool's own MCP tools.

The team lead's Plan-phase system prompt includes a compact team roster (names + handles), plus a per-member detail block (backstory, goal, responsibilities, Jira identity, skills) rendered directly from the in-memory `Organization` model so the lead has full context when reasoning about an assignment.

---

## External PM Tool as Source of Truth

Task lifecycle lives in the PM tool (Jira, Plane, GitHub/GitLab issues), not in the engine. The `ExecutionTracker` is a thin orchestration layer that tracks agent-to-issue mappings and the dependency graph between issues — the orchestration concerns the PM tool doesn't cover.

This avoids duplicating task state and keeps the audit trail in the tool the team already uses.

---

## No Dedicated Escalation Mechanism

There is no special escalation mechanism. When stuck, agents hand off to their manager with the same colleague-surface tools they use for any collaboration (Jira comment, Slack mention, A2A) — same as a human would. There is no dedicated handoff decision and no `fallback` chain: Review routes a blocked turn back through `self_iterate` so Plan adds the outreach step, and Execute makes the call. The hierarchy is informational + downward-delegation routing; it does not drive a special upward path at runtime.

Engine-detected failures (stall, max-iter exhaustion, depth cap, unhandled exception, LLM-provider chain exhausted) publish `turn.guard_breach` / `llm_unavailable` events and terminate the turn as `failed`. The dashboard derives an `afk` state from the latest failure event and surfaces a cause-specific quip for the founder. No active push notification is sent — visibility is logs + dashboard, by design.

---

## Provider Abstraction via Protocols

All external dependencies (LLM, embeddings, storage) are behind `Protocol` interfaces. No vendor SDK lock-in. This enables:

- Different roles using different LLM providers/models
- Configurable embedding providers for the agent-learning subsystem (e.g., OpenAI, or any compatible endpoint)
- In-memory implementations for unit tests (not used in production code paths)
- Easy addition of new providers without touching core logic
