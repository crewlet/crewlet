# Event System

All inter-component communication in Crewlet flows through a persistent event queue (`internal/queue`), backed by Apache Pulsar.

---

## Interfaces

One protocol serves all inter-component communication:

- **`EventQueue`** — persistent pub/sub with consumer groups. For fire-and-forget messages: task routing to agent inboxes, inbound/outbound notifications.

An in-memory implementation (`queue/memory` in `internal/queue/memory`) is used exclusively in tests. Production deployments use Apache Pulsar.

---

## Topic Structure

```
# EventQueue topics (persistent, at-least-once via Apache Pulsar)
crewlet.agent.{handle}.inbox         # Per-agent inbox — all work arrives here
crewlet.notifications.inbound        # Inbound webhooks from external systems
crewlet.notifications.outbound       # Outbound messages to external systems
```

---

## Routing

Routing is two-stage: events are first published to internal topics (e.g., `crewlet.events.task_assigned`), where the Engine's subscription handlers determine the target agent and re-publish to that agent's inbox topic. This keeps the event producers decoupled from the routing logic — they emit events without knowing which agent should handle them.

Handlers read the org through a provider on every event (never a captured snapshot), so a hot reload that swaps `engine.org` — including seat-kind flips — re-routes immediately.

**Routing is an org function, not a process-local one.** Every handler resolves its recipient from the live organization — a role name, or an agent id, to a seat, and a seat to its inbox subject — and never from the local agent pool. Each `crewlet.events.*` topic has ONE fleet-wide consumer group, so whichever node wins a delivery is the node that has to route it; that node usually is not the one running the recipient. This works because agent ids are *derived* rather than assigned: `org.Organization.AgentIDFor` is a `uuid5` over the org name and the seat's handle, so every node computes the same id for the same seat, and `AgentSeatByID` / `AgentSeatByHandle` invert it with no database and no live instance. The inbox subject itself has one definition, `topics.AgentInbox` — a producer and a consumer that disagree about a topic name do not raise, they just stop talking to each other.

### A seat's mailbox exists before the seat is running

A durable subscription **is** the mailbox, and it exists independently of whether anyone is consuming it. That is not a detail — publishing to a topic that no subscription covers **drops the event silently**, with nothing anywhere reporting a loss.

So every node creates the subscription behind **every agent seat in the company** as it starts, before it claims a single one, and again whenever an applied revision adds a role. Not its own share: a mailbox is a fact about the company, and the node that ends up serving a seat may not be the one that made its mailbox. Creating one is idempotent, so a fleet doing it N times costs N−1 no-ops.

What this buys is that a trigger aimed at a seat nobody is running yet **waits** instead of vanishing:

- during boot and during a rollout, where seats are claimed a few per sweep and the rest are briefly unowned;
- for a seat whose placement no live node matches — a role pinned to a node that is down, or carrying a label nobody has. The sweep already reports those as `seats_unplaceable`; their work now accumulates and drains the moment a matching node appears.

When the resolved target is a **[human seat](humans-in-the-org.md)**, the event is skipped: a human has no inbox and no turn to wake, and the engine never sends as itself. These internal events route to agents only; the human is notified natively by the PM tool / Slack where the work lives (and agents reach humans through their own colleague-surface tools with an @-mention). Inbound external-surface webhooks addressed to a human are likewise recorded as an info-level skip, not an undeliverable warning.

---

## Inbox Batching & Coalescing

An agent turn takes minutes; webhooks arrive in seconds. Without batching, ten comments on one Jira issue that arrived while the agent was mid-turn would drain as **ten sequential full turns** — 10× LLM cost, each turn seeing one comment in isolation, potentially ten separate replies. Inbox delivery is therefore **batched per conversation**.

**The buffer is the broker backlog** — no second in-memory buffering layer. Holding events in process memory after their broker message was acknowledged would silently lose them on a crash; instead, the inbox consume loop changes how the backlog is *drained*:

```mermaid
flowchart TD
    BACKLOG["Pulsar backlog (the buffer)<br/>inbox: [c1 POC-7] [c2 POC-7] [c3 thread-A] [c4 POC-7]"]
    DRAIN["1. DRAIN — collect everything available<br/>(+ optional linger window)"]
    PART["2. PARTITION by conversation key,<br/>preserving arrival order"]
    P1["[c1, c2, c4] — jira:POC-7"]
    P2["[c3] — slack:C9:1718.001"]
    T1["3. one digest turn<br/>4. ack c1, c2, c4"]
    T2["3. normal single-event turn<br/>4. ack c3"]
    BACKLOG --> DRAIN --> PART
    PART --> P1 --> T1
    PART --> P2 --> T2
```

**`subscribe_batch`** (`EventQueue` protocol; Pulsar + memory backends) implements steps 1–4: after the first message arrives it drains everything immediately available — plus anything arriving within `BatchOptions.LingerSeconds` of the first message — up to `BatchOptions.MaxBatch`, partitions by a caller-supplied key, invokes the handler **once per partition** (sequentially — per-agent serialization is unchanged), and acknowledges a partition's messages only after its handler returns. A failing partition negatively-acknowledges exactly its own messages (normal redelivery / DLQ policy per message) without blocking or replaying other conversations from the same drain. `pause_delivery` during collection NAKs anything fetched-but-undispatched so the next engine subscription gets it promptly.

**The ack budget, and the one backend it does not bite on.** Every drained message's ack clock starts at receive, but a partition handler is typically a full multi-minute turn — so dispatching a long tail of partitions sequentially holds later messages delivered-but-unacked for the *sum* of the preceding turns. On **JetStream**, the default backend, that clock is real: `ackWait` is 30 minutes, sized for a wait behind a running turn plus one worst-case turn, and collection plus one handler run must fit inside it — which is why `BatchOptions.LingerSeconds` is capped at 60s (`queue.MaxLingerSeconds`), enforced at the contract rather than in config so programmatic construction cannot bypass it. On **Pulsar** there is no such clock at all: `apache/pulsar-client-go` exposes no `ConsumerOptions.AckTimeout`, it keeps no client-side unacked tracker, and Pulsar has no broker-side equivalent for a *connected* consumer, so a fetched message stays that consumer's until it acks, naks, or closes. A 60-second batch dispatch budget, with a requeue-by-republish path for the partitions left over when it expired, is the natural answer to a client whose ack clock starts at receive. On this one there is no clock to race, so no budget exists (adr-104). Nothing is republished, which is just as well: [adr-101 §1](https://github.com/crewlet/crewlet/blob/main/adrs/101-queue-contract.md) forbids substituting a republish, because it sends an event to the topic tail while its prefetched siblings replay from the head and reorders the conversation. What the absence costs, stated so nobody has to rediscover it: a drain of N slow partitions holds N messages unacked for the sum of the turns, and the only ceiling is Pulsar's `maxUnackedMessagesPerConsumer` (50 000 by default) against a batch capped at `max_batch` (20 by default) — four orders of magnitude of margin. Partitions dispatch **oldest conversation first** (by oldest constituent event timestamp): a waiting conversation ages and outranks the hot conversation's fresh arrivals on the next drain, so steady inflow on one issue cannot starve a waiting DM.

**Conversation keys** (`notify.Prompt.ConversationKey`) are derived by pure logic from webhook metadata, via the same per-source `notify.Prompt` classes that own prompt building: Jira keys on the issue (`jira:POC-7`), Confluence on the page, GitHub on `repo#number`. Slack keys on the **whole channel for top-level DM and group-DM messages** (`channel_type` `im`/`mpim`, or a `D`-prefixed channel id when the event variant omits the field — a human firing four rapid top-level DM messages is one conversation; a DM *thread reply* keeps its thread key so the merged trigger never carries the wrong reply target) and on channel + thread root elsewhere (`slack:C9:1718.001` — a top-level channel message keys on its own `ts` so its replies join it, while two unrelated asks in a shared channel never merge). Everything else — `task_assigned`, A2A wakes, notifications without a derivable conversation — keys uniquely on the event id and is **never coalesced**: single-event partitions follow exactly the pre-batching dispatch path.

The same key now has a second consumer that outlives the drain. Coalescing merges the messages of one conversation that arrive *together*; [conversation sessions](conversation-sessions.md) carry what the seat did about them into that conversation's **next** turn, and the episode row and the turn's telemetry are stamped with the key so history can be asked for by thread rather than only by agent and time. The `event:{uuid}` fallback above is exactly why those consumers store nothing for a trigger without a real conversation: no later message could ever reproduce that key to read the row back.

**Busy agents queue; parked agents requeue.** A delivery that finds its agent mid-turn does not fail: `turn.Engine.Run` WAITS for the running turn to finish (the handler already holds the delivery for a full turn, so JetStream's ack window — 30 minutes — is sized for exactly that: a wait plus a worst-case turn). A delivery that finds the agent parked on a detached sandbox job (`AWAITING_SANDBOX`, potentially hours) is requeued + acked instead — the coordinator keeps the topic paused, so the copies buffer on the broker and flow when the job completes. Before the engine has a turn engine at all (booted with zero LLM providers), the handler pauses the topic and requeues likewise; the late turn-engine build resumes every inbox. Busy-agent handling therefore never consumes-and-drops and never pushes healthy events toward the dead-letter topic.

**Unsubscribe.** `EventQueue.Unsubscribe` tears down the durable group consumer(s) for the pair and deletes the broker-side subscription (retained messages for the group are dropped). The engine calls it when a role is decommissioned live, so a removed seat neither keeps a consumer bound to a terminated instance nor accumulates undeliverable events forever. Inbox subscription is idempotent per agent handle — boot and the late turn-engine path both walk the pool, and only the first subscribe per agent creates a consumer.

**The digest trigger.** A multi-event partition is merged by `internal/notify`'s coalescer into ONE notification: a chronological digest of the earlier messages, then the **latest** constituent's full enriched body — so the per-source scaffolding (triage rules, `## Get Full Context`) renders exactly once and points at the most recent state. Two noise filters apply in the digest: per-source supersede rules (`notify.Prompt.DigestBody` — Jira `issue_updated` bodies, stale full descriptions whose current state the Jira prompt never renders anyway, collapse to their event lead) and a source-agnostic **same-sender duplicate dedupe** — a constituent whose effective body is byte-identical to a later message from the same sender collapses to a marker (GitHub lifecycle webhooks re-emit the full PR description per event; the later copy still renders, so nothing is lost, and two different people saying "+1" both survive). Comments and messages always keep their text. The merged event carries every constituent in `messages` (sender, salient body, metadata, per-message recon flag — full fidelity for the [learning workers](agent-learning.md), which observe **each distinct sender**), a conservative event-level recon merge, the max-depth constituent's delegation bookkeeping (batching cannot launder the depth cap), and the latest constituent's trace context. Same-id duplicate deliveries (an at-least-once edge the requeue machinery itself can produce) are dropped at the handler before any merging. If a partition cannot be merged (a malformed constituent), the engine degrades to per-event dispatch — the tail is requeued as independent inbox messages FIRST, then the first event runs in the current ack scope — so a requeue failure aborts before any turn ran and a completed turn is never replayed by a later event's failure; partially-requeued copies collapse via the same-id dedupe on redelivery. A `NotificationsCoalesced` telemetry event records each merge for the dashboard / event store.

**Two knobs** (Tier B, hot-reloadable — see the [configuration reference](../getting-started/configuration.md)):

| Field | Default | Meaning |
|---|---|---|
| `notification_coalesce_window_seconds` | `0` | Linger after the first pending event before dispatching. `0` adds **no latency** and still coalesces the busy case — backlog that accumulated during a turn is drained together regardless. A positive window (5–15 s) additionally absorbs bursts while the agent is idle (a human typing several messages, a Jira comment+status+assign webhook cluster). |
| `notification_coalesce_max_batch` | `20` | Cap per digest; a larger backlog arrives as successive capped batches rather than one unbounded megaprompt. |

With the window at `0`, an idle-agent burst worst-cases at **two** turns (the first message wakes the agent immediately; everything arriving during that turn coalesces into one follow-up turn per conversation) — never N.

**Relation to the rate limiter.** `notification_rate_limit` (NotificationService, default off) *drops* notifications above N/agent/second — it remains purely a safety valve against pathological webhook storms and notification loops. Burst handling is coalescing's job: a coalesced comment is context preserved, a dropped one is context lost.

DACI decisions are conducted in **Slack threads** — the driver opens a thread in the team channel with its own Slack MCP tools and all contributions, proposals, and approvals are thread replies; there is no engine-side decision machinery. See [Decision Framework](decision-framework.md) for details.

---

## Event Types

Grouped by the **category** each one is filed under — the same closed set the
`GET /events?category=` filter and the dashboard's category chips use. The
authoritative list, generated from the engine's own map, is
[Deployment § What gets stored](../guides/deployment.md#what-gets-stored-and-under-which-category);
this is the shape of it, with the notes that need a sentence.

```text
# lifecycle — the org and its seats coming and going, plus the config
#             changes an operator goes looking for after the fact
OrgStarted, OrgStopped
AgentSpawned, AgentTerminated, AgentReassigned
RoleUpdated                # role definition changed during a config apply
ConfigRevisionActivated    # a new revision is the one to serve
ConfigRevisionApplied      # one node's outcome, and how far it got

# task — work created, assigned and done, including a detached coding run
#        (the execution of a task) and a schedule firing (which creates one)
TaskCreated, TaskAssigned, TaskStarted
TaskCompleted, TaskFailed, TaskDelegated
SandboxRunStarted, SandboxClarificationRequested, SandboxRunCompleted
ScheduledTaskFired

# communication
MessageSent                # agent sent a message to a channel

# a2a — one ask, one answer, then closed
A2AChannelOpened, A2AMessageSent, A2AMessageDelivered, A2AChannelClosed

# knowledge
DocumentCreated, DocumentUpdated

# decision — DACI is behavioural guidance on the org's own chat surfaces,
#            so NOTHING in Crewlet publishes these four. They stay mapped as
#            the seam an extension that does model decisions writes through,
#            and they are why the category exists to filter on at all
DecisionRequested, DecisionResolved
ContributionRequested, ContributionReceived

# notification — what arrived from outside, and what the engine decided
ExternalNotification       # inbound from a vendor webhook or chat socket
NotificationSkipped        # dropped notification with reason (traceability)
NotificationsCoalesced     # N same-conversation inbox events merged into one
                           # digest trigger (see Inbox Batching above)
TurnTriggerSkipped         # a redelivery the completion ledger had already
                           # worked -- emitted precisely so it is not invisible

# learning — the reflection subsystem and the skill lifecycle, grouped so a
#            dashboard can include or exclude all of it with one toggle
TurnCompleted, EpisodeWritten, PersistDeciderCompleted
CounterpartyProfileUpdated, ReflectionCompleted
SkillSynthesized, SkillRefined, SkillPromoted, SkillUsed
SkillStaled, SkillArchived, SkillRevived
PlanPrefetchSummary, RelevantKnowledgeRefetched
CompactionRequested, CompactionCompleted

# system — the engine talking about itself
AgentTurnCompleted         # full LLM reasoning cycle with tokens/tools
AgentPhaseStarted, AgentPhaseCompleted
BudgetExhausted
TurnGuardBreach            # runtime invariant fired (stall / max_iter /
                           # depth_cap / unhandled_exception /
                           # scheduled_timeout). Drives the dashboard `afk`
                           # state
LLMUnavailable             # the fallback chain is exhausted. Drives `afk` too
ProviderFallback           # the chain moved to its next provider
SubagentBatched, PromptSize, ExecuteMissingTool
PhaseToolActivated, PhaseToolSkillBlocked, SkillTelemetryWriteFailed

# webhook — no event type: the receiver writes the delivery's row itself,
#           with the provider's exact bytes as the payload
```

**Three types are published and deliberately never stored**, each for a stated
reason — `AgentTurnProgress` (a live-only per-round signal whose durable record
is `AgentPhaseCompleted`), `BudgetReported` (a snapshot of in-memory meters
that mean nothing outside the run that produced them) and `RawWebhook` (the
delivery is already a row). The first two still drive the live projection. See
the exclusions table in the Deployment page above.

---

## Event Schema

Every event carries a common set of fields: a unique ID (UUID), a type string, a UTC timestamp, an optional source identifier, and a free-form payload dict. Specialized event types (e.g., `TaskAssigned`) add their own fields with defaults.

Events also carry **OpenTelemetry trace context** and self-describing properties:

```go
type Event struct {
    ID        uuid.UUID
    Type      string
    Timestamp time.Time
    Source    string
    Payload   map[string]any   // free-form extras; typed fields live in Data

    // OpenTelemetry trace context — captured at construction from the
    // active span.
    TraceID      string // W3C 32-char hex, groups causally related events
    SpanID       string // W3C 16-char hex, identifies this event in the trace
    ParentSpanID string // links to the event/span that caused this one

    // Turn-engine bookkeeping, so an agent woken by a colleague handoff
    // inherits the correct depth and chain.
    DelegationDepth int
    ParentTurnID    string
    DelegationChain []string

    // Data is the typed body, non-nil when Type is registered in this
    // build. Marshalled flat into the same JSON object as the envelope.
    Data Payload
}
```

`Payload` is the typed half: each registered event type is a Go type with its
own fields and its own `Summary()` — "who did what", in a person's words — and
an `Actor()` (role, then source, then agent id, then `system`). An event type
this build does not know decodes into the envelope with `Data` nil, and
re-publishes losslessly.

Changes are additive-only — new fields get defaults, existing fields are never removed, and an event type this build does not know round-trips through it losslessly rather than being dropped: a rolling upgrade puts unknown types on the wire in both directions. Every backend retains each subscription's undelivered backlog until it is consumed, so a restart resumes cleanly; durable, replayable event history is the [event store](../guides/deployment.md#the-event-store), not the queue. On Pulsar, time-based retention of already-acknowledged messages is an optional namespace retention policy.

---

## Distributed Tracing

Events carry **OpenTelemetry-compatible trace context** (W3C Trace Context format). Trace IDs propagate automatically through the system — no manual threading:

```mermaid
flowchart TD
    W["Slack webhook (trace starts here)"] --> N["NotificationService routes to agent"]
    N --> E["Executor wraps turn in OTel span"]
    E --> A["TaskStarted"]
    E --> B["Tool: send_message"]
    E --> C["AgentTurnCompleted"]
    E --> D["TaskCompleted"]
```

**How it works:**

1. The webhook edge opens a span, so a delivery is the root of the trace
   everything it causes hangs under. An inbound W3C `traceparent` is honoured
   if one is present, which is what makes a delivery forwarded through your own
   gateway join your trace rather than start a second one.
2. Events carry `trace_id` / `span_id` / `parent_span_id` in the envelope. That
   is the only carrier — the queue backends move an event's bytes and nothing
   else — and it is what crosses the broker, the store and a node boundary.
3. When an event wakes a seat, the dispatcher restores its trace onto the
   context before the turn runs, so the turn's span is a child of the span that
   caused it rather than a new root.

**The trace is passed, never captured.** An event's trace is an argument to its
constructor (`events.New(payload, trace)`), and the caller derives that
argument from the context with `tracing.TraceOf(ctx)`. This is deliberate and
[decisions/405](https://github.com/crewlet/crewlet/blob/main/decisions/405-event-type-system.md)
requires it: an event that read an *ambient* span at construction would be one
whose trace depends on which frame happened to build it.

`TraceOf` never returns empty. Inside a span it reports that span's ids; with
no span open it mints a fresh root, which is what every publisher in the engine
used to do by hand. So there is no "events published outside a span lose their
trace" case any more — the older behaviour, where such an event got an empty
`trace_id` and became unreachable from the work it belonged to, is gone.

**Each event's `span_id` is the span that emitted it**, and `parent_span_id`
the span above. A turn does not republish its trigger's span id as its own;
that made every event in a turn look like the same span and collapsed the
dashboard's tree onto the wake that started it.

**When notifications are dropped** (own message, not following thread, rate limit), a `NotificationSkipped` event is emitted with the skip reason — visible in the trace so you can see why a webhook didn't reach an agent.

The dashboard groups events by `trace_id` into collapsible trace trees. See [Deployment — Tracing](../guides/deployment.md#tracing) for OTLP export configuration.

---

## Publish Listeners

The `EventQueue` supports **publish listeners** — async callbacks invoked inline during every `publish()` call. Listeners receive the topic and event, and run in the same coroutine as the publish. Exceptions in listeners are logged but do not prevent the publish or affect queue delivery.

This is used by the **event store writer** to persist events directly at publish time, inline on the node that published — no subscription, and therefore no consumer group that could let two nodes write one row or lose one in a rebalance. See [Deployment — The event store](../guides/deployment.md#the-event-store) for details.

---

## Broadcast Streams (`subscribe_stream`)

Beyond competing-consumer `subscribe()` and inline `add_publish_listener`, the `EventQueue` exposes **`subscribe_stream(topic_pattern, handler)`** for live-stream consumers (dashboards, real-time log views). Every subscriber receives every matching event — no consumer-group division.

The Pulsar backend implements this with a per-caller **regex topic-pattern subscription**, started at the latest message (so it streams new events — backfill is served separately by the REST event store) and torn down when the caller unsubscribes. Because Pulsar discovers pattern-matching topics on a periodic cycle, a brand-new agent's first events may lag the stream briefly; already-active topics match immediately. The memory backend implements it with a topic-filtered publish listener.

The dashboard's `/ws/stream` endpoint uses this primitive: each connected tab is one ephemeral consumer, and the in-process `api/stream` both updates the live-state projection and fans every event out to every connected WebSocket. See [API Endpoints — Live Stream](../reference/api-endpoints.md#live-stream).

`topic_pattern` accepts subject wildcards: `*` matches one segment, `>` matches one-or-more trailing segments.

---

## Communication

Two communication systems:

### External Channels (Slack, Email)

Org-wide announcements, department coordination, and team discussions happen through external tools (Slack channels, email) via the **Notification Service**. Agents use MCP tools to post and receive messages from Slack, and the notification service routes inbound webhooks to agent inboxes.

- **Org-wide** — announcements (via Slack `#announcements` channel)
- **Department** — leads-only coordination (via Slack department channel)
- **Team** — team coordination, DACI decisions (via Slack team channel)

### Ephemeral A2A channels (`crewlet.a2a`)

Private 1:1 conversations between agents, for tight-loop / mechanical sync that should *not* show up on the team's chat or issue tracker. One question, one answer, then the channel closes.

An agent opens one with the `a2a_ask` tool and ends its turn. The brief travels **on the wake event** in the target's inbox, so it reaches whichever node owns that seat; the answering agent's **final response is the reply**, delivered back on the same channel and waking the asker. There is no `send_a2a_message` tool and no channel lifecycle for a model to manage — replying is just answering.

```mermaid
sequenceDiagram
    participant A as Agent A (asks)
    participant Q as Inbox topics
    participant B as Agent B (answers)
    A->>Q: a2a_ask → open channel, publish brief
    Q->>B: a2a_request (brief on the event)
    B->>B: turn runs
    B->>Q: final response → reply, close channel
    Q->>A: a2a_message (the answer)
    A->>A: turn runs, acts on the answer
```

Both hops are ordinary inbox deliveries: durable, ordered per conversation, routed to the seat's owner, and covered by the [completion ledger](seat-ownership.md#the-completion-ledger) so a redelivery does not run the turn twice. Channel state — the two participants, open or closed, the message count — lives in the [coordination store](coordination.md)'s `channels` slot, because the two parties are usually on different nodes and the node that authorizes the *answer* is the one that owns the answering seat, never the one that opened the channel. A single node uses the in-process coordination twin, which is a real implementation of the same certified contract rather than a fallback.

A channel is closed by the answering turn. One whose answer never came — a crashed turn, a node that died between the wake and the reply — is closed by the [maintenance duty](seat-ownership.md#singleton-duties) after **1 hour** idle (three times the longest a turn can legitimately still be running), and the record is deleted seven days after that. Neither is configurable, and neither is a bucket age: the coordination store expires its other slots on a clock, but a clock cannot tell an *open* channel from a closed one, so it would reap the authorization record of an ask still waiting for its answer. Closed records outlive the conversation on purpose: *closed* is the answer to "why did my reply bounce", while a vanished record is indistinguishable from a typo'd channel id.

Either way the close publishes `a2a_channel_closed` naming both participants, the message count and how long the channel was open — including on the sweep path, which is where it matters most, since a channel only reaches the sweep because a turn did not finish. The duration is the difference between the record's own `opened_at` and `closed_at`, not between two nodes' clocks: a channel is opened on one node and closed on another as a matter of course, and the difference of two machines' opinions of the time is skew rather than a duration.

| Aspect | External Channels (Slack) | A2A channels |
|---|---|---|
| **Lifetime** | Permanent (Slack workspace) | Ephemeral (one question and its answer) |
| **Backend** | Slack API + Notification Service | Agent inbox topics + the coordination store's `channels` slot |
| **Persistence** | Yes (Slack history) | State yes, content only as events |
| **Visibility** | The team sees it | Private to the two agents |
| **Use case** | Broadcasting, team coordination | Tight-loop / mechanical sync |
