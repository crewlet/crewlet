# The Tracker

A company's work has to live somewhere. Crewlet gives you two shapes, chosen
with one field:

```yaml
tracker:
  backend: native    # the default — the engine is the tracker
# backend: jira      # the tracker is Jira, and the engine mirrors none of it
```

The two are not variations on one design. They are opposite answers to the
same question, and each is coherent on its own terms.

## `native` — the engine is the tracker

The company's items live in the fleet's own
[coordination store](coordination.md) as versioned documents, and every node
keeps a rebuildable [projection](#the-projection) of them to read from. There
is a board on the dashboard, tools a seat calls, and an
[MCP surface](../reference/api-endpoints.md#operatormcp--your-own-assistant)
your own AI assistant can reach.

**This is not a mirror.** The record here *is* the source of truth, so the
staleness argument below does not apply to it: there is no other copy to
disagree with, no webhook to miss, and no reconciliation poller because
nothing is being reconciled. A node's projection can be behind, and that is
handled by saying so — a read refuses with "still catching up" rather than
answering "there is no such item", because the second is an answer somebody
acts on.

What it is deliberately **not** is a Jira. There is no workflow engine, no
custom field, no sprint, no board configuration and no permission scheme. An
item has a key, a type, a status from a closed set, an assignee, a thread and
a history. That is what an agent company actually uses, and every one of the
missing features is a screen somebody would have to configure before their
company could file its first ticket.

### Why the engine grew one

Because the alternative — the paragraph below — costs a founder an Atlassian
site, a project, six service accounts and a webhook before their company can
record that it did anything. That is a real barrier for the case Crewlet is
for, and the vendor path stays fully supported for the companies that are
already on it.

## `jira` — the tracker is somebody else's

Task lifecycle lives entirely in an external PM tool — Jira, GitHub or GitLab
issues — and **the engine mirrors none of it**: no task table, no status
field, no assignee map, no dependency graph, no reconciliation poller. A
ticket's state is whatever the PM tool says it is, read live through an
agent's own MCP tools.

That is the design, not a gap. A mirror of somebody else's task state is a
cache with no invalidation story: every webhook you miss, every edit made in
the PM tool's own UI, and every retry that arrives out of order leaves the
engine confidently wrong about work a person can see is finished. Keeping
nothing means there is nothing to be stale.

## What is identical either way

Everything below this line. Routing, assignment, hand-offs, the lead fallback,
the way a webhook becomes a turn — none of it knows which backend answered. A
unit's `project` key names its project on whichever tracker the company runs,
which is why the field is not called `jira_project`.

## The projection

On the native backend each node keeps its own copy of the fleet's record, in
its local store, rebuilt from the coordination bucket at boot. It exists so
that "what is on the board" is a SQL query rather than a listing over the
whole coordination family — which is `O(keys)` message deliveries, on a
request path, for a screen somebody refreshes.

Three properties follow from it, and each is visible:

- **A node claims no new seats until its projection has caught up.** A seat
  whose tools read an incomplete projection would answer "there is no such
  item" and act on it, by filing the duplicate or telling a person their link
  is dead. The node keeps every seat it already holds — a projection catching
  up is not a reason to drop work in hand — and the fleet view reports how
  many of its projections are ready.
- **A read that cannot be answered says so.** `503`, not an empty list.
- **A write waits for its own projection before it answers**, so a turn that
  files an item and then lists the project sees what it just filed.

Retention is the one asymmetry worth knowing: items, comments and pages are
kept **for ever** — a tracker that forgot would stop answering the question it
exists for — while the change records behind them age out after a year, and a
page's history is capped at a hundred revisions.

---

## How it works with webhooks

A change is a change, whoever recorded it: a Jira webhook and a native item's
own change record both arrive at the notification service as a delivery and
both become an `ExternalNotification`. Nothing above that seam knows which.

PM-tool webhooks do **not** become dedicated task events. Every webhook is parsed by the notification service into an `ExternalNotification` delivered to the routed agents' inboxes — the assignee, watchers, @-mentioned agents, or the project lead as a fallback (see [Jira Integration](../integrations/jira.md)). The woken agent then acts on the PM tool through its own MCP tools.

```mermaid
sequenceDiagram
    participant PM as PM tool (Jira/GitLab)
    participant EN as Engine
    PM->>EN: Ticket created (webhook)
    Note over EN: NotificationService parses + routes →<br/>ExternalNotification to the project lead's inbox<br/>(fallback routing) → lead agent turn
    PM->>EN: Ticket assigned (webhook)
    Note over EN: → ExternalNotification to the assignee's inbox → agent turn
    PM->>EN: Comment added (webhook)
    Note over EN: → ExternalNotification to watchers, assignee,<br/>and @-mentioned agents
    EN->>PM: MCP tool call
    Note over PM: Agent creates subtask, transitions ticket, posts<br/>comment — all through MCP tools, same as a human would
```

The `TaskAssigned` event type exists for engine-internal work injection — the [Scheduler](scheduling.md) fires it for cron-style recurring tasks (`internal/schedule/scheduler.go`) — and never for the PM-tool webhook pipeline, which produces `ExternalNotification` instead. The two are deliberately different types: one is the engine giving a seat work, the other is the world telling a seat something happened.

---

## Assignment: Team Lead as Decision Maker

See [Organization Model](organization-model.md#unit-lead) for how unit leads and rosters are configured.

Task assignment is **not** an algorithmic strategy — it is a **team lead agent's reasoning decision**. When a task appears (via webhook or builtin tool), the engine notifies the team lead. The lead reasons about each member's backstory, skills, workload, and knowledge scopes, then assigns the task — either via a builtin tool or by setting the assignee in the PM tool via MCP. The engine then wakes the assigned agent.

A human can also assign directly in the PM tool — the same webhook fires, the same agent wakes up. For **top-level tasks** (no team lead above), the founder assigns directly in the PM tool, or a C-level agent role acts as the top-level assigner.

---

## Manager handoffs (no special escalation)

There is no special escalation mechanism in Crewlet. When an agent is blocked or out of its depth, it hands off the same way a human would:

- The agent reaches its manager during Execute with the colleague-surface tool that fits where the work lives — a Jira comment, a Slack mention, or `a2a_ask` for tight-loop sync. If the blocker only becomes clear at Review, Review returns `decision="self_iterate"` with a note telling Plan to add that outreach step, and the next pass makes the call.
- The `getting-unstuck` tool skill (see `examples/tool-skills/getting-unstuck.md`) teaches the agent the discipline: include what you tried, options you see, your recommendation, and urgency. Never hand a naked problem.
- The agent's identity prompt names its manager, so the handoff target is always resolvable.

When the engine itself can't continue — stall guard fires, max-iter exhausted, unhandled exception, LLM unavailable — it publishes a `turn.guard_breach` (or `llm_unavailable`) event and terminates the turn as `failed`. The dashboard derives an `afk` state from the latest failure event and surfaces a cause-specific status line so the founder sees what happened.

---

## Data Flow Examples

### Task Created in PM Tool

```mermaid
flowchart TD
    JIRA["<b>PM Tool (Jira)</b><br/>Issue created: 'Build auth API'<br/>Webhook fires → NotificationService"]
    ENG["<b>Engine</b><br/>ExternalNotification (project-lead fallback routing)"]
    LEAD["EventQueue → Team Lead inbox<br/><i>a human lead has no inbox — the task falls through to the<br/>target role's own agents; the human sees it in the PM tool</i>"]
    SUB["Team lead reads task, queries knowledge.<br/>Creates subtasks in Jira via MCP tools:<br/>'Design auth endpoints' → Senior Engineer<br/>'Implement JWT middleware' → Senior Engineer<br/>'Write auth tests' → Junior Engineer"]
    HOOK["Jira webhooks fire for each assignment"]
    ROUTE["ExternalNotification routed to each assignee's inbox"]
    WORK["Agents work in parallel, transition tickets via MCP"]
    WATCH["Transition webhooks → watchers (incl. the lead) notified"]
    REVIEW["Lead reviews results, transitions parent ticket"]
    MGR["Transition webhook → manager notified (watcher/mention)"]
    JIRA --> ENG --> LEAD --> SUB --> HOOK --> ROUTE --> WORK --> WATCH --> REVIEW --> MGR
```

### Manager-handoff Flow

```mermaid
flowchart TD
    subgraph blocked["Agent-detected blocker"]
        direction TB
        A["Agent (e.g. Junior Engineer)<br/>working on task, encounters blocker"]
        B["Execute calls the colleague-surface tool for the manager<br/>(or Review self_iterates so Plan adds that outreach step),<br/>targeting the surface that fits where the work lives"]
        C["Colleague-surface tool fires<br/>(slack / jira / confluence / a2a)"]
        D["Manager sees the mention on the same surface they already<br/>use for human teammates; their next turn fires when they reply"]
        A --> B --> C --> D
    end
    subgraph enginefail["Engine-driven failure (stall / max-iter / exception / LLM down)"]
        direction TB
        E["TurnGuardBreach (or LLMUnavailable)"]
        F["The dashboard reports the seat as AFK with its cause,<br/>first in the overview's attention queue.<br/>The founder follows it to the seat, the turn, or the log."]
        E --> F
    end
```
