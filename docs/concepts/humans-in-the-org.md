# Humans in the Org Chart

A Role in Crewlet is a **seat** in the org chart, held by either an AI
agent (the default) or a **human teammate**. Human seats participate in
the full hierarchy — they can manage agents, lead units, appear in
rosters, and be escalation targets — but they are never executed:
no `AgentInstance`, no inbox topic, no LLM, no learning rows.

The design follows one observation: agents already collaborate through
human-native surfaces (Slack, Jira, Confluence, GitHub). A human
teammate doesn't need an engine runtime — they need to **exist in the
model** so agents know who they are, how to reach them, and what to
expect when they do. Agents reach humans exactly as they reach each
other: their own colleague-surface tools, with an @-mention. **The
engine never sends as itself** — there is no system bot, no
engine→human notification channel.

---

## Declaring a Human Seat

```yaml
units:
  - name: Core Engineering
    type: team
    lead: Sarah Chen              # a human can lead an AI team
    roles:
      - name: Sarah Chen
        kind: human
        email: sarah@acme.com     # informational (not a delivery channel)
        goal: "Keep the team unblocked and own final calls"
        backstory: "20 years in infrastructure"
        responsibilities:
          - "Approvals and vendor decisions"
        contact:                  # how agents mention & reach her
          slack_user_id: U0123456789
          mattermost_user_id: sarah.chen       # Mattermost username, not an ID
          atlassian_account_id: 5b10ac8d-...   # one ID covers Jira + Confluence
          github_login: sarahchen
          gitlab_username: sarahchen
          plane_user_id: 9c41be2f-...          # Plane workspace-member user UUID
        availability: "CET business hours; replies within ~4h"
      - name: Engineer            # AI agent, unchanged
        goal: "Implement features and ship quality code"
```

### Human seat fields

| Field | Required | Description |
|-------|----------|-------------|
| `kind: human` | yes | Marks the seat as human |
| `contact.slack_user_id` | one identity | Slack member ID (`U…`) — `<@…>` mentions and the channel an agent DMs on escalation |
| `contact.mattermost_user_id` | one identity | [Mattermost](../integrations/mattermost.md) **username** — the name an agent writes as a literal `@username` mention, and the account it opens a DM channel with. Not the opaque 26-character user ID; normalized to lowercase |
| `contact.atlassian_account_id` | one identity | Atlassian Cloud account ID — Jira assignments, Confluence `<ri:user>` mentions, webhook sender attribution |
| `contact.github_login` | one identity | GitHub username — review requests, sender attribution |
| `contact.gitlab_username` | one identity | GitLab username — assignment / review / mention routing + sender attribution |
| `contact.plane_user_id` | one identity | [Plane](../integrations/plane.md) workspace-member user UUID (the member table [`crewlet plane provision`](../integrations/plane.md#provisioning--crewlet-plane-provision) prints exists to fill this in, or `GET /api/v1/workspaces/{slug}/members/`; normalized to lowercase) — assignment, subscriber, and `<mention-component>` mention routing + sender attribution |
| `email` | no | Informational only — rendered in `lookup_colleague`; **not** a delivery channel (no agent has an email tool by default) |
| `availability` | no | Free text rendered into rosters and `lookup_colleague` results (timezone, hours, response expectations) |

A human seat needs **at least one `contact` identity** — that is how
agents mention and reach them, and how inbound webhooks attribute their
activity by name. A seat with no contact would be inert (visible in the
chart but unreachable), so it's rejected at validation.

Every `contact` field accepts either a literal ID or exactly one
whole-value `${VAR}` environment reference — e.g.
`plane_user_id: "${PLANE_FOUNDER_USER_ID}"` in a shipped example config,
where the real UUID is instance-specific. Values are whitespace-stripped
at validation; references are stored verbatim (never case-mangled) and
resolved from the process environment wherever the identity is consumed:
contact registration, roster / identity prompts, and `lookup_colleague`.
For the case-normalized fields (`github_login`, `gitlab_username`,
`plane_user_id`) the *resolved* value is lowercased. A reference whose
variable is unset counts as a declared identity for validation, but is
omitted from registration and prompts (with a debug log) until the
variable is exported — the raw `${VAR}` text is never emitted. A value
that merely *embeds* a `${VAR}` inside a longer string
(`"acme-${SUFFIX}"`) is rejected at validation — half-substituting it
would silently register a wrong identity.

Human seats keep the descriptive identity fields (`goal`, `backstory`,
`responsibilities`) — they are the **routing context**: rendered into
an agent lead's roster (so work goes to the person who owns it) and
into `lookup_colleague` results (so any agent can learn what a human
does, including a human lead). They also keep the hierarchy fields
(`manages`, unit `lead`). Every runtime-only field is **rejected at
validation time**: `llm*`, `token_budget`, `learning_enabled`,
`schedules`, `slack` (bot credentials), `github` (token), `mcp_env`,
`behavioral_guidelines`.

Handles are validated for format (`[a-z0-9][a-z0-9-]*`) and org-wide
uniqueness — they are the canonical seat identity, and an agent/human
collision would silently misattribute the person's activity to the
agent.

---

## How Agents Know Humans Exist

- **Identity prompt** — `Reports to: Sarah Chen (human)`; human direct
  reports carry the same marker.
- **Lead roster** — human members render as a distinct block: handle +
  "human teammate" marker, contact IDs, availability, and hand-off
  guidance (assign in the PM tool + mention; no engine turn expected).
- **`## Human colleagues` contract block** — appears in the Plan prompt
  (and the monolithic introspection prompt) *only when the org contains
  human seats*: reach humans on external surfaces, never via `a2a_ask`; they
  reply asynchronously — leave full context and end the turn; their
  reply re-triggers you.
- **`lookup_colleague`** — resolves agents *and* humans; human results
  carry `kind: human`, what the person owns (goal, background,
  responsibilities), contact IDs, availability, and the interaction
  note. This is how a report learns what its **human lead** does —
  the roster only renders downward. Disambiguation rows mark human
  candidates.
- **Sender attribution** — inbound webhook prompts resolve actor IDs
  through the party registry: a Jira comment from Sarah renders as
  `Sarah Chen (sarah-chen, human colleague)` instead of an opaque
  account ID. Counterparty profiles accrue for humans like anyone else
  (they are keyed by handle).

---

## The Interaction Loop

Agent → human and back needs **no new machinery** — it is the existing
webhook pipeline:

```
agent mentions Sarah on Jira / DMs her on Slack (its own tools)
        │
        ▼
Sarah reads it natively in Jira / Slack (the engine forwards nothing)
        │  …hours later…
        ▼
Sarah replies → webhook → notifications inbound → agent inbox → digest turn
```

Two consequences:

1. **The engine never pushes to humans.** Internal task-lifecycle
   events (`TaskCreated`, `TaskAssigned`, `TaskCompleted`,
   `TaskDelegated`) exist to wake an *agent* into a turn — a human has
   no turn to wake. When a recipient resolves to a human seat the event
   is skipped quietly: the human is already notified natively by the PM
   tool / Slack where the work lives (a Jira assignment emails the
   assignee; a Slack mention pings them). Inbound external-surface
   webhooks addressed to a human are likewise recorded as an info-level
   skip ("notified natively by the external tool"), not an undeliverable
   warning.
2. **Agents must never wait.** The turn model is already asynchronous —
   the prompts and tool errors steer the LLM to leave state on the
   surface and end the turn.

---

## Escalation (reaching a human)

A human seat is the natural **terminus** of an escalation chain, and an
agent reaches one exactly as it reaches any colleague — with its **own
colleague-surface tools** during Execute, never via the engine:

- **Chat (Slack / Mattermost)** — the agent DMs the human's member ID
  (or username) with its own chat tool, mention-prefixed. The engine
  never names that tool: the deployed MCP server's names are not
  knowable here, so the prompts describe the *capability* and the LLM
  picks the match from its catalogue (see
  [Tool Capabilities](tool-capabilities.md)). The human's reply lands on
  the agent's own bot identity and re-enters through the normal inbound
  pipeline, so the answer goes back to the agent that asked.
- **Jira / Confluence / GitHub** — the agent comments / requests review
  with its own tools and the mention markup; the target is the artifact
  (issue / page / PR), the human is mentioned in the body.
- **A2A is not a human surface** — humans are not on the bus, so
  `a2a_ask` against a human returns an actionable error pointing the
  agent at Slack / Jira, and the `A2AService` refuses any non-agent
  target so a typo or a stale `"human"` entry fails visibly
  instead of waking a subscriber-less topic. The question the guard
  asks is whether the target is an **agent seat in the org**, not
  whether it is running in the asking process: a colleague owned by
  another node is a normal A2A target, because the wake lands on its
  inbox and that node consumes it.

When a turn only discovers it needs a human at Review, Review returns
`self_iterate` with a note; the next Plan pass adds the outreach step and
Execute makes the mention. If the agent genuinely **can't reach the
human** — it has no Slack tool, or the human has no contact ID — that
surfaces as a config gap to fix (give the agent the tool, or route the
work through a colleague who has it). The engine never manufactures a
sender to bridge the gap: there is no "Crewlet" DM, and no engine-side
`fallback` chain — escalation is ordinary colleague-tool use, so a
report reaches a human exactly the way it reaches an agent.

---

## The Founder Seat

The recommended way to put yourself in the company: a **root-level
human seat managing the top agent(s)**. There is no dedicated founder
concept in the engine — the org chart is the model, and the founder is
simply the top of it (the same reasoning behind having no dedicated
escalate tool: the manager handoff IS escalation).

```yaml
roles:
  - name: Jane Founder
    kind: human
    goal: "Own direction; final call on what ships"
    responsibilities: ["Approvals", "Unblock the CEO"]
    manages: [CEO]            # top agents only — lead inheritance does the rest
    contact:
      slack_user_id: U0FOUNDER
      atlassian_account_id: 5b10ac8d-...
      github_login: janedoe
      gitlab_username: janedoe
      plane_user_id: 9c41be2f-...
```

What this buys, with no further config:

- Top agents' prompts read `Reports to: Jane Founder (human)` — manager
  handoffs from your most senior agents terminate at a person instead
  of `None (top-level)`. When the CEO is stuck it DMs you on Slack /
  mentions you on Jira with its own tools, and your reply re-triggers
  it.
- Your Slack / Jira / GitHub activity is attributed by name — agents
  know when the founder is speaking.
- DACI: the Approver is "the driver's manager", so you become the
  approver-of-last-resort for top-level decisions behaviorally.

Two boundaries to keep in mind:

- **The seat is the colleague hat, not the operator hat.** Config
  ownership (`PUT /config`, API auth tokens, the dashboard) stays an
  API-auth concern — the seat makes agents know you; the token makes
  the engine obey you. Different hats, deliberately separate.
- **Scope `manages` to the top roles.** A founder managing every unit
  becomes the default manager and escalation terminus for every
  otherwise-unmanaged role. Manage the CEO (or the unit leads) and let
  lead inheritance handle the rest; `availability` sets response
  expectations.

See `examples/nimbus.company.yaml` for a complete working org with a
founder seat above the agent CEO.

---

## What Humans Never Do

| Subsystem | Behavior |
|-----------|----------|
| Agent pool / spawn | Never spawned; `spawn_role` rejects human seats |
| Inbox topics | None — nothing ever publishes to a human "inbox" |
| Engine notifications | None — the engine never sends as itself; agents reach humans with their own tools |
| Scheduler | `target: each` fans out to agent members only; an enabled `target: lead` schedule under a (possibly inherited) human lead is a **config error**; human seats cannot define role schedules |
| A2A channels | Not addressable; `a2a_ask` returns guidance |
| Learning | No diary, no episodes, no synthesized skills (counterparty profiles *about* them still accrue) |
| `GET /agents` | Excluded — they appear in `GET /org` with `"kind": "human"`; the dashboard org chart badges them `human` |

## Hot Reload

Seat kind flips are first-class in `apply_config`:

- `human → agent` spawns an instance (budget, inbox, per-role MCP).
- `agent → human` decommissions the instance. Agent IDs are
  deterministic (`derive_agent_id`), so flipping back later reattaches
  the old diary and onboarding markers.
- Contact / availability edits ride the org swap; human contact IDs
  re-register on every swap.

## Identity Resolution (party API)

`HandleRegistry` resolves **parties** — agents and human seats — via
`resolve_party`, `resolve_party_role_name`, `resolve_party_email`,
`resolve_party_external`, and enumerates them via `all_parties()`. The
agent-only methods keep their narrow signatures so inbox routing can
never target a human. Human external IDs come straight from config
(`contact`), **reconciled** into the registry at boot and on every org
swap — stale pairs from contact edits, seat removals, and kind flips
are unregistered, while an ID owned by an agent identity is never
silently taken over. External-ID resolution is pure index lookups (it
runs per webhook for sender attribution); for Slack it consults both
namespaces — human member IDs under `slack`, agent bot-user IDs under
`slack_bot` — so agent and human senders annotate alike.
