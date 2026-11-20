# One-on-Ones

A **1:1** is the in-life management tier: a recurring or on-demand coaching conversation between a manager and a report, to review recent work, give feedback, surface blockers and agree on action items. It is **not a subsystem and not a dedicated tool**: it is a usage pattern composing three existing pieces.

| Piece | Role in a 1:1 | Where it lives |
|---|---|---|
| **Scheduler** | fires the recurring 1:1 (the trigger) | [Scheduling](scheduling.md) |
| **A2A channels** | the private manager↔report exchange | [Agent Runtime](agent-runtime.md#built-in-tools) (`a2a_ask`) |
| **Learning loop** | turns the conversation into durable memory | [Agent Learning](agent-learning.md) (`PersistDecider`) |

The engine ships **no 1:1 code**. An operator provides a schedule and a [playbook](#the-11-playbook).

---

## Why no dedicated tool

A 1:1 is a private agent-to-agent exchange, which `a2a_ask` already provides: the ask wakes the other participant for a fresh turn, that turn's answer wakes the asker back, and either side can open another channel to go deeper (see
[A2A](event-system.md#ephemeral-a2a-channels-internala2a)). The engine sends the answer when the answering turn ends. A 1:1 tool wrapping `a2a.Service` would be a thin alias, like the engine-side `slack_message` or `jira_comment` wrappers the project avoids over third-party MCP tools.

**One exchange is one channel.** Ask, answer, closed. The reply is the engine's to send, never a tool's, so no tool is left for a model to forget and no channel waits for a round that never comes. A deeper conversation is a follow-up `a2a_ask`, another exchange; the [delegation-depth cap](turn-engine.md#runtime-invariants) (default 3) bounds how many a single 1:1 can run, so design the playbook around it.

`a2a_ask`'s guidance steers agents to Slack/Jira for "reviews / feedback / status" because work-product reviews need a **visible** trail. A 1:1 is **private by design** and passes `a2a_ask`'s test ("if a human teammate would want to see this message, it does not belong on A2A"), so A2A is the correct surface.

> **Rule of thumb.** Public coaching artifacts (action items, a standing rule that becomes team policy) belong on Slack / Jira / Confluence. The 1:1 *conversation itself* belongs on A2A.

---

## The two triggers

The A2A conversation is the same whichever trigger starts it.

### Recurring — a schedule

Put a schedule on the **unit** with the default `each` target. The scheduler fans out **one independent turn per direct member**, each with its own at-most-once ledger, trace and timeout, so a slow report never blocks the others. Each member's turn opens the 1:1 with its manager:

```yaml
units:
  - name: Core
    id: core                      # the unit's key; `name` above is display
    type: team
    lead: cto                     # the lead seat's handle
    schedules:
      - name: weekly-1on1
        cron: "0 14 * * 4"          # 14:00 every Thursday
        # target: each  (default) — every direct member runs its own 1:1
        task: >
          Hold your weekly 1:1 with your manager. Open it on a private A2A
          channel with a2a_ask (a 1:1 is private — do NOT use Slack or Jira):
          use query_episodes to recall what you shipped and where you got
          stuck, and put the whole review in the one brief — what you shipped,
          where you are blocked, and the specific feedback you want — because
          your manager answers it in one reply. Note the action items you
          agree on. Follow the "Manager 1:1" playbook page in the team
          knowledge base.
    roles:
      - name: Engineer A
      - name: Engineer B
```

**The manager does not loop.** The engine does the fan-out (`each`), so there is no long manager turn opening N channels under one timeout. Each report initiates its own 1:1 (a 1:1 belongs to the employee), and the manager answers each ask in its own turn as woken (A2A wakes are per-channel).

> `each` fires to every **direct** member, including a lead that is itself a direct role of the unit (it then 1:1s *its* manager), so everyone gets a 1:1 with their manager at every level. `each` resolves direct roles only, never descendant units, and schedules are not inherited: declare the schedule on each unit that should run it.

### On-demand — the manager initiates

A manager who wants a 1:1 *now* calls `a2a_ask` against the report (usually one, no fan-out) with a brief framing it as a 1:1. Same conversation, playbook and durability.

---

## Who initiates, and the human-manager case

- **Agent manager ↔ agent report**: A2A channels. The report opens the channel on the scheduled path, the manager on-demand; either way the *manager drives the review*, since authority is in the playbook, not in who started it.
- **Human manager ↔ agent report**: A2A is agents-only (`a2a_ask` rejects human targets; no inbox sits behind a [human seat](humans-in-the-org.md)). The human holds the 1:1 in a **Slack thread / DM**: each reply is a turn and the same learning loop persists the takeaways. It needs only the human seat in the org chart.

---

## Durability — nothing new to build

Every round of the report's 1:1 is a normal turn, so the post-turn
[`PersistDecider`](agent-learning.md#1-persistdecider-post-turn-personal-memory) runs on each, seeing who spoke and what they said (inbound message and sender are in its prompt). Residue lands on its own:

- **Facts** ("my manager wants EOD updates") are written to the report's private `agent_diary` as `LONG` / `SHORT`.
- **Standing directives** ("always get review before merging") are classified `DOC` and deliberately **not** memorised: the decider writes nothing and logs the rule as `persist_decider_doc_observed`, with the model's hint for where it belongs. A standing rule reaches the team when somebody puts it in the **team docs** (Confluence), which every member picks up via the
  [`## Relevant knowledge`](agent-learning.md#relevant-knowledge-prefetch) prefetch; the bundled `skill:observed_directives` tool skill tells agents to share a team-relevant directive on their broadcast surface. Memory holds declarative facts, not instructions (the PersistDecider writing-style rule).

The conversation stays private and ephemeral: the channel is a row saying who spoke to whom and that it is closed, never a browsable transcript. What persists is its *outcomes*: diary facts, Jira action items, Confluence policy.

**The report's own kickoff session carries over.** A schedule is a conversation of each seat that runs it, so this week's `weekly-1on1` fire continues the session last week's fire left on the report's seat (its preparation and brief) and is told how that fire ended, for as long as `turn_engine.conversation_session.retention_days` keeps it (thirty days by default, so a weekly 1:1 always does). Every request of its run pays for that context ([Each fire continues the last fire's session](scheduling.md#each-fire-continues-the-last-fires-session)). The manager's answering turn and the turn its answer wakes the report with are A2A wakes, which serve no conversation and continue nothing.

---

## Bounds

- **Convergence** is the playbook's job: put the substance in the *first* exchange and wrap up with explicit action items rather than trading one question at a time.
- **Loop safety** is the engine's, twice over. A channel carries one exchange: the answering turn replies and closes it, and a closed channel refuses a second answer, so no volley starts on one channel. Across channels each `a2a_ask` increments the delegation depth (a reply carries the ask's depth unchanged, so only asks are charged) and the turn loop's depth cap stops the chain whatever the LLMs do.
- **Scheduled timeout** bounds only the **kickoff** turn, not the whole conversation: each exchange is its own turn on each side, so `timeout_seconds` is not the convergence control.

---

## The 1:1 playbook

The *behavioral contract* (how the report prepares, how the manager gives feedback, the round budget, what to capture) lives as a **knowledge-base page** ([Confluence](../integrations/confluence.md)), the standard home for shared procedures. It reaches both parties through the `## Relevant knowledge` prefetch and the backend's page-search tools. It is **not** a [Tool Skill](tool-skills.md): tool skills trigger on a tool / MCP-server name and are enforced load-before-use, so a skill keyed on `a2a_ask` would block *every* mechanical A2A call.

A ready-to-publish playbook ships at
[`examples/nimbus-docs/LEAD/Manager 1-1.md`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus-docs/LEAD/Manager%201-1.md): its `LEAD/` parent directory routes it to the org-wide `LEAD` space every agent can read, and its `# Manager 1:1` H1 becomes the page title. The example runs Doclet, so publish it through your own assistant on
[`/operator/mcp`](../reference/api-endpoints.md#operatormcp--your-own-assistant)
(see [publishing knowledge docs](knowledge-system.md#publishing-knowledge-docs)). On a company that moved its wiki to Confluence,
`crewlet confluence import <company.yaml> examples/nimbus-docs/` does the same (no `trigger:`, so it imports as a knowledge doc). Edit it in the page editor thereafter, no redeploy.

**A 1:1 does not need the page.** The schedule and `a2a_ask` are the whole mechanism. A company with no knowledge backend runs the same `weekly-1on1` schedule, with the task text carrying the shape of the conversation.

---

## What this is deliberately *not*

- **No 1:1 tool and no 1:1 event.** The conversation rides `a2a_ask` and is observable through `a2a_channel_opened`, `a2a_message_sent` and `a2a_channel_closed`; scheduled 1:1s are identifiable by the `schedule_name` on their `scheduled_task_fired`.
- **No memory reset.** Wiping a report's memory is a destructive operator break-glass action (memory poisoning, role repurposing), not a coaching move, and is tracked separately.
- **No 1:1-specific scheduler primitive.** A 1:1 is a scheduled task like a standup; the fan-out is the generic `each` target.

---

## See also

- [Scheduling](scheduling.md): the `each` / `lead` targets, at-most-once
  delivery, catchup, the per-fire timeout.
- [Agent Runtime](agent-runtime.md): `a2a_ask`; built-in tools
  (`query_episodes`).
- [Event System](event-system.md#ephemeral-a2a-channels-internala2a): what an
  A2A channel is, and why one exchange is one channel.
- [Agent Learning](agent-learning.md): the `PersistDecider` tiers and the
  `## Relevant knowledge` prefetch that surfaces the playbook.
- [Humans in the Org Chart](humans-in-the-org.md): why a human manager's
  1:1 runs over Slack, not A2A.
