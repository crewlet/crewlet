# One-on-Ones

A **1:1** is the in-life management tier — the recurring (or on-demand)
coaching conversation a manager and a report have *between* hire and fire:
review recent work together, give feedback, surface blockers, agree on
action items. In Crewlet a 1:1 is **not a subsystem and not a dedicated
tool** — it is a *usage pattern* that composes three things that already
exist:

| Piece | Role in a 1:1 | Where it lives |
|---|---|---|
| **Scheduler** | fires the recurring 1:1 (the trigger) | [Scheduling](scheduling.md) |
| **A2A channels** | the private manager↔report exchange | [Agent Runtime](agent-runtime.md#built-in-tools) (`a2a_ask`) |
| **Learning loop** | turns the conversation into durable memory | [Agent Learning](agent-learning.md) (`PersistDecider`) |

Because it reuses these, the engine ships **no 1:1 code** — what an operator
provides is a schedule and a [playbook](#the-11-playbook).

---

## Why no dedicated tool

A 1:1 is mechanically just a private agent-to-agent exchange, which
`a2a_ask` already provides: the ask wakes the other participant for a fresh
turn, that turn's answer wakes the asker back, and either side can open
another channel to go a round deeper (see
[A2A](event-system.md#ephemeral-a2a-channels-crewleta2a)). A `request_1on1`
wrapper over `A2AService` would be a thin alias of the kind the project
deliberately avoids (`slack_message`, `jira_comment`-style wrappers —
see `internal/agent/builtin`); it would carry a different description
and nothing else.

**One exchange is one channel.** Ask, answer, closed — the answering turn's
final response *is* the reply, so there is no tool for a model to remember to
call and no channel left open waiting for a second round that never comes. A
deeper conversation is a follow-up `a2a_ask`, which is another exchange, and
the [delegation-depth cap](turn-engine.md#runtime-invariants) (default 3) bounds how
many of them a single 1:1 can run before the engine stops it — a real ceiling
worth designing the playbook around.

The one apparent objection — `a2a_ask`'s own guidance steers agents toward
Slack/Jira for "reviews / feedback / status" — dissolves on inspection. That
guidance exists because *work-product* reviews (a PR review, a status update)
need a **visible** trail a human teammate would want to see. A 1:1 is the
opposite: it is **private by design**. It passes `a2a_ask`'s actual test
("if a human teammate would want to see this message, it does not belong on
A2A") cleanly, so the private bus is the *correct* surface for it — not an
exception to the rule.

> **Rule of thumb.** Public coaching artifacts (action items, a standing
> rule that becomes team policy) belong on Slack / Jira / Confluence. The
> 1:1 *conversation itself* belongs on A2A.

---

## The two triggers

The 1:1 *action* (the A2A conversation) is the same regardless of what kicks
it off. Only the trigger differs:

### Recurring — a schedule

Put a schedule on the **unit** with the default `each` target. The scheduler
fans out **one independent turn per direct member** (its at-most-once ledger,
own trace, and own timeout per member — a slow report never blocks the
others). Each member's turn opens the 1:1 with its manager:

```yaml
units:
  - name: Core
    type: team
    lead: CTO
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

**The manager does not loop.** The engine does the fan-out (that is what
`each` *is*), so there is no long O(N) manager turn opening N channels under
one timeout. Each report initiates its own 1:1; the manager simply responds
on each channel as it is woken (A2A wakes are per-channel, so each 1:1 is an
independent, bounded unit). This is also the natural model — a 1:1 belongs to
the *employee* ("my weekly 1:1 with my manager"), so scheduling it per
employee is correct, not a workaround.

> `each` fires to every **direct** member, including a lead that is itself a
> direct role of the unit (it would then 1:1 *its* manager). That is usually
> desirable — everyone gets a 1:1 with their manager, the same pattern at
> every level. `each` resolves direct roles only (never descendant units),
> and schedules are not inherited, so declare the 1:1 schedule on each unit
> that should run it.

### On-demand — the manager initiates

When a manager notices a problem and wants a 1:1 *now*, it just calls
`a2a_ask` against the report (usually one report — no fan-out needed) with a
brief that frames the conversation as a 1:1. Same conversation, same
playbook, same durability.

---

## Who initiates, and the human-manager case

- **Agent manager ↔ agent report** → A2A channels, as above. On the scheduled
  path the report opens the channel; on-demand the manager opens it. Either
  way the *manager drives the review* — authority is in the playbook, not in
  who clicked "start".
- **Human manager ↔ agent report** → A2A is agents-only (`a2a_ask` rejects
  human targets — there is no inbox behind a [human seat](humans-in-the-org.md)).
  A human manager holds the 1:1 in a **Slack thread / DM** instead: each
  reply is a turn, and the same learning loop persists the takeaways. This
  needs no configuration beyond the human seat already being in the org
  chart.

---

## Durability — nothing new to build

Every round of the report's 1:1 is a normal turn, so the post-turn
[`PersistDecider`](agent-learning.md#1-persistdecider--post-turn-personal-memory)
runs on each one automatically — it even sees *who* spoke and *what* they
said (the inbound message + sender are in its prompt). The conversation's
residue lands in the right place on its own:

- **Facts** ("my manager wants EOD updates", "Sam reviews PRs in the
  morning") → written to the report's private `agent_diary` as `LONG` /
  `SHORT`.
- **Standing directives** ("always get review before merging") → classified
  `DOC`: deliberately **not** memorised, but surfaced as a `DirectiveObserved`
  observation that nudges a handoff to update the **team docs** (Confluence),
  which every member then picks up via the
  [`## Relevant knowledge`](agent-learning.md#relevant-knowledge-prefetch)
  prefetch. (Memory holds declarative facts, not instructions — see the
  PersistDecider writing-style rule.)

So a 1:1 does not need its own memory machinery; it inherits the standard
loop. The conversation itself stays private and ephemeral — the channel is a
row that says who spoke to whom and that it is closed, never a transcript
anyone can browse; only the *outcomes* — diary facts, Jira action items,
Confluence policy — persist.

---

## Bounds

- **Convergence** is the playbook's job: it tells both sides to put the
  substance in the *first* exchange and to wrap up with explicit action items
  rather than trading one question at a time.
- **Loop safety** is the engine's job, twice over. A channel carries exactly
  one exchange — the answering turn replies and closes it, and a closed
  channel refuses a second answer — so a volley cannot start on one channel
  at all. Across channels, each `a2a_ask` increments the delegation depth
  (the reply carries the ask's depth unchanged, so only asks are charged),
  and the TurnEngine's cap stops the chain regardless of what the LLMs do.
- **Scheduled timeout** bounds only the **kickoff** turn (the report opening
  the channel), *not* the whole conversation — each exchange is its own turn
  on each side, so `timeout_seconds` is not the convergence control.

---

## The 1:1 playbook

The *behavioral contract* — how the report prepares, how the manager gives
feedback, the round budget, what to capture — lives as a **knowledge-base
page** ([Confluence](../integrations/confluence.md)), not in engine prose. This is the project's
standard home for shared procedures (the same place runbooks, ADRs, and
onboarding pages live), and it reaches both parties through the
`## Relevant knowledge` prefetch and the backend's page-search tools. It is
**not** a [Tool Skill](tool-skills.md): tool
skills trigger on a tool / MCP-server name and are enforced load-before-use,
so a skill keyed on `a2a_ask` would block *every* mechanical A2A call — there
is no 1:1-specific tool to key on.

A ready-to-publish playbook ships at
[`examples/nimbus-docs/LEAD/Manager 1-1.md`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus-docs/LEAD/Manager%201-1.md)
— its `LEAD/` parent directory routes it to the org-wide `LEAD` space
every agent can read, and its `# Manager 1:1` H1 becomes the page title.
Publish it with
`crewlet confluence import <company.yaml> examples/nimbus-docs/`
(it has no `trigger:`, so it imports as a [knowledge doc](knowledge-system.md#publishing-knowledge-docs)).
Edit it in the page editor thereafter — no redeploy.

---

## What this is deliberately *not*

- **No `request_1on1` tool, no `OneOnOneCompleted` event.** The
  conversation rides `a2a_ask` and is observable through the existing
  `A2AChannelOpened` / `A2AMessageSent` / `A2AChannelClosed` events;
  scheduled 1:1s are additionally identifiable by their `ScheduledTaskFired`
  `schedule_name`.
- **No memory reset.** Wiping a report's memory is a destructive operator
  break-glass action (memory poisoning, role repurposing), not a coaching
  move — it is tracked separately, not as part of the 1:1 pattern.
- **No 1:1-specific scheduler primitive.** A 1:1 is "just a scheduled task",
  exactly like a standup; the fan-out is the generic `each` target.

---

## See also

- [Scheduling](scheduling.md) — the `each` / `lead` targets, at-most-once
  delivery, catchup, the per-fire timeout.
- [Agent Runtime](agent-runtime.md) — `a2a_ask`; built-in tools
  (`query_episodes`).
- [Event System](event-system.md#ephemeral-a2a-channels-crewleta2a) — what an
  A2A channel is, and why one exchange is one channel.
- [Agent Learning](agent-learning.md) — the `PersistDecider` tiers and the
  `## Relevant knowledge` prefetch that surfaces the playbook.
- [Humans in the Org Chart](humans-in-the-org.md) — why a human manager's
  1:1 runs over Slack, not A2A.
