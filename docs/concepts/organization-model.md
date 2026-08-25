# Organization Model

The organization model (`internal/org`) is the foundational data structure representing the company hierarchy. It determines how agents communicate, what knowledge they can access, who they report to, and how tasks flow.

---

## Flexible Hierarchy

The org structure uses a recursive `OrgUnit` model that can nest to any depth. This lets founders design their org however they want — flat teams, departments with sub-teams, divisions, squads, pods, or any custom structure.

```
Organization
├── name, mission, vision, policies
├── roles: Role[]                          (root-level org-wide agents)
└── units: OrgUnit[]
    ├── name, type, purpose, lead, goals, knowledge_refs
    ├── channel: str                       (team channel on the company's chat
    │                                       surface, inherited by children)
    ├── jira_project: str                  (integrations.jira.project — the unit's Jira
    │                                       project identity: lead-fallback webhook
    │                                       routing + the project the team files under)
    ├── confluence_space: str              (REFUSED in this build — integrations.confluence
    │                                       is not served, so nothing consults this
    │                                       identity)
    ├── plane_project: str                 (integrations.plane.project — the unit's Plane
    │                                       project identity: webhook fallback routing +
    │                                       the project the team files work under, NOT an
    │                                       MCP credential, does NOT scope reads)
    ├── mcp_env: dict[server → env vars]  (per-agent tool creds, inherited by roles)
    ├── roles: Role[]                      (agents directly in this unit)
    ├── children: OrgUnit[]                (nested sub-units, recursive)
    └── schedules: Schedule[]              (unit recurring work, NOT inherited;
                                            see Scheduling)

Role (a SEAT — can live at root level OR inside an OrgUnit)
├── kind: agent | human                (who holds the seat; default agent)
├── name, responsibilities, behavioral_guidelines
├── contact: {slack_user_id, atlassian_account_id, github_login, gitlab_username,
│             plane_user_id}     (human seats — external identities)
├── availability: str (human seats — rendered into rosters)
├── backstory: str   (unique personality, background, expertise)
├── goal: str        (individual mission)
├── handle: str      (canonical identity slug, auto-derived if empty)
├── email: str       (agent email for notifications & external tools)
├── manages: str[]  (role names or unit names this role manages)
├── mcp_env: dict[server → overrides] (per-agent tool credentials — env
│                                      vars for stdio servers, http headers
│                                      for http servers like the remote
│                                      GitHub MCP. Tool creds only; the
│                                      project/space identity is the
│                                      integrations block below)
├── jira_project: str  (root-level roles — integrations.jira.project;
│                       the role's Jira project identity: lead-fallback
│                       webhook routing + write home, NOT an MCP credential)
├── confluence_space: str (REFUSED in this build — integrations.confluence
│                          is not served, so nothing consults this identity;
│                          see [Confluence](../integrations/confluence.md))
├── plane_project: str (root-level roles — integrations.plane.project;
│                       the role's Plane project identity: webhook fallback
│                       routing + write home, NOT an MCP credential, does
│                       NOT scope reads — read scope is the org-wide
│                       knowledge.plane_projects only)
├── token_budget: int  (0 = unlimited)
├── llm: str           (provider key, default = "default")
├── llm_auxiliary: str (optional cheap-model key for reflection /
│                       summarisation work)
├── learning_enabled: bool? (per-role override for the agent-learning
│                            subsystem)
├── slack: dict        (REFUSED in this build — role.integrations.slack is
│                       not served; the per-agent transport identity that
│                       is, is mattermost: {bot_token})
└── schedules: Schedule[]  (role-scoped recurring work; see
                            [Scheduling](scheduling.md))
```

Roles can live in two places:

- **Inside an OrgUnit** (`units[].roles`) — scoped to that unit for MCP env inheritance and lead auto-management. The unit's [`integrations.plane.project`](../integrations/plane.md#project-identity) gives the team its tracker "home" (webhook routing + write target), but does not scope what the role can *read*.
- **At the root level** (`roles`) — org-wide agents that don't belong to any specific team. They participate in the `manages[]` hierarchy like any other role and are fully visible to task routing; a root-level role can carry its own `integrations.plane.project` identity. Knowledge **read** scope for every agent is the org-wide `org.Organization.PlaneProjects` only.

> A unit's or a role's `integrations.jira.project` and `integrations.plane.project` are both consulted: each tracker routes an item that names nobody to the lead of the unit that owns the project. The `integrations.confluence` identity is **refused** by this build — no searcher reads a Confluence space, so the identity would be recorded and never consulted, and `knowledge.confluence_spaces` is refused for the same reason. See [Jira](../integrations/jira.md) and [Confluence](../integrations/confluence.md).

---

## Common Org Patterns

### Root-Level Roles (CEO/CTO above all teams)

Org-wide leaders can be defined at the root, outside any unit. They manage unit leads via `manages[]` and participate in task routing like any other role.

The root is also where the **founder** belongs — as a [human seat](humans-in-the-org.md#the-founder-seat) above the top agent, so escalations terminate at a person and agents recognise the founder's activity on Slack/Jira/GitHub:

```yaml
roles:
  - name: "Jane Founder"
    kind: human
    manages: ["CEO"]
    contact: { slack_user_id: U0FOUNDER }
  - name: "CEO"
    goal: "Set company direction"
    manages: ["VP Engineering", "VP Product"]
```

```yaml
roles:
  - name: "CEO"
    goal: "Set company direction"
    manages: ["VP Engineering", "VP Product"]

units:
  - name: "Engineering"
    type: department
    lead: "VP Engineering"
    roles:
      - name: "VP Engineering"
        manages: ["Backend Lead"]
    children:
      - name: "Backend"
        type: team
        lead: "Backend Lead"
        roles: [...]
  - name: "Product"
    type: department
    lead: "VP Product"
    roles: [...]
```

Root-level roles differ from unit roles in a few ways:

| Aspect | Root-level role | Unit role |
|--------|----------------|-----------|
| Knowledge scope | Org-wide (reads are role-independent — see [Knowledge System](knowledge-system.md)) | Org-wide (same) |
| MCP env inheritance | No parent unit to inherit from | Inherits unit's `mcp_env` |
| Lead auto-management | N/A (no unit lead concept) | Auto-managed by unit lead if unmanaged |
| `org.Organization.UnitFor` | Returns `None` | Returns the containing unit |

### Flat Startup (no departments)

```yaml
units:
  - name: "Product Team"
    type: team
    lead: "Founder"
    roles:
      - name: "Founder"
        manages: ["Dev 1", "Dev 2"]
      - name: "Dev 1"
      - name: "Dev 2"
```

### Departments with Teams (traditional)

```yaml
units:
  - name: "Engineering"
    type: department
    lead: "VP Engineering"
    children:
      - name: "Backend"
        type: team
        lead: "Backend Lead"
        roles: [...]
      - name: "Frontend"
        type: team
        lead: "Frontend Lead"
        roles: [...]
  - name: "Product"
    type: department
    children:
      - name: "Product Management"
        type: team
        lead: "PM"
        roles: [...]
```

### Division > Department > Team (enterprise)

When only the top-level unit has a lead, it cascades down via [lead inheritance](#lead-inheritance):

```yaml
units:
  - name: "Technology"
    type: division
    lead: "CTO"
    roles:
      - name: "CTO"
    children:
      - name: "Engineering"
        type: department          # inherits CTO as lead
        children:
          - name: "Platform"
            type: team
            lead: "Platform Lead" # explicit — overrides inherited CTO
            roles: [...]
          - name: "Application"
            type: team            # inherits CTO as lead
            roles: [...]
```

### Spotify Model (Tribes + Squads)

```yaml
units:
  - name: "Infrastructure Tribe"
    type: tribe
    lead: "Tribe Lead"
    children:
      - name: "Provisioning Squad"
        type: squad
        lead: "Squad Lead"
        roles: [...]
      - name: "Networking Squad"
        type: squad
        lead: "Squad Lead 2"
        roles: [...]
```

### Pod-Based (cross-functional)

```yaml
units:
  - name: "Auth Pod"
    type: pod
    lead: "Auth Lead"
    roles:
      - name: "Auth Lead"
        manages: ["Auth Dev", "Auth Designer"]
      - name: "Auth Dev"
      - name: "Auth Designer"
  - name: "Billing Pod"
    type: pod
    lead: "Billing Lead"
    roles: [...]
```

---

## OrgUnit Types

The `type` field on an OrgUnit can be any string. These well-known types are provided for convenience:

| Type | Description | Typical Use |
|------|-------------|-------------|
| `division` | Large business unit | Top-level grouping in enterprises |
| `department` | Functional area | Engineering, Product, Marketing |
| `group` | Cross-functional group | Working groups, task forces |
| `team` | Core delivery unit | Backend, Frontend, DevOps |
| `squad` | Autonomous cross-functional unit | Spotify model |
| `pod` | Small cross-functional group | 3-5 person focused teams |
| `guild` | Interest-based community | Knowledge sharing groups |
| `chapter` | Skill-based group | Design chapter, QA chapter |
| `unit` | Generic default | When no specific type fits |

Custom types are welcome — use whatever fits your org. The type is informational and does not affect behavior.

---

## Key Concepts

### Role = Seat

Each Role defines a unique **seat** with its own backstory, skills, personality, and domain expertise. A seat is held by an AI agent (`kind: agent`, the default) or a **human teammate** (`kind: human`). Agent seats map 1:1 to an AgentInstance; human seats participate in the same hierarchy (manages, unit lead, rosters, escalation) but are addressable-only — no runtime, no inbox, no LLM. The founder defines each seat individually — they are not interchangeable. See [Humans in the Org Chart](humans-in-the-org.md).

### Handle-Based Identity

Every agent gets a deterministic **handle** slug derived from its role name:

```
Role Name           Handle
─────────────────   ────────────────
Sarah Chen          sarah-chen
Marcus Rivera       marcus-rivera
Alex Kim            alex-kim
```

Handles are the canonical identity for notification routing and external system mappings (e.g. a Jira assignee, a GitLab service account). A seat's `email` is matched too — inbound Jira and GitHub payloads identify people by address, and a plus-addressed form (`notif+sarah-chen@co.com`) resolves back to the handle. You can set a custom handle:

```yaml
roles:
  - name: Senior Engineer
    handle: sr-eng        # Override auto-derived "senior-engineer"
```

### Management Hierarchy

Hierarchy is encoded through `manages` relationships on roles. A Team Lead manages Engineers; a VP manages Team Leads. See [Agent Runtime](agent-runtime.md) for how the hierarchy drives agent execution.

- **Permissions** flow from hierarchy — a manager can assign tasks to reports, knowledge access is scoped, and the agent's identity prompt names the manager so handoffs (a Slack mention, a Jira comment, or `a2a_ask` during Execute) reach the right person
- **Task assignment** is the unit lead's responsibility — the lead agent reasons about its members and assigns tasks

#### Managing by unit name

The `manages` list accepts both **role names** and **unit names**. When an entry matches an OrgUnit name (and does not match any role name), it is expanded to all roles contained in that unit, including roles in descendant child units. This avoids listing every agent individually when a role manages an entire team or department.

```yaml
roles:
  - name: "CEO"
    manages: ["Engineering", "Product"]   # unit names — expands to all roles in each unit

units:
  - name: "Engineering"
    type: team
    lead: "Tech Lead"
    roles:
      - name: "Tech Lead"
      - name: "Dev A"
      - name: "Dev B"
  - name: "Product"
    type: team
    lead: "PM"
    roles:
      - name: "PM"
      - name: "Designer"
```

After expansion the CEO manages: `Tech Lead`, `Dev A`, `Dev B`, `PM`, `Designer`.

You can mix role names and unit names freely:

```yaml
manages: ["CTO", "Backend"]   # CTO is a role, Backend is a unit
```

If a name matches both a role and a unit, the **role takes priority** (no expansion happens for that entry).

### Unit Lead

An OrgUnit may designate a lead via the `lead` field. The lead is responsible for:

- Task routing and assignment within the unit
- Acting as the single point of contact for the unit
- Reasoning about members' properties (backstory, skills, knowledge) to assign tasks to the right individual

When a unit has direct roles and a lead is set, the lead auto-manages any role not already managed by another role in the unit.

The lead can be a **human seat** — a human manager running an AI team is a first-class pattern: agents escalate to the human with their own Slack/Jira tools (an @-mention), and the human assigns work in the PM tool. See [Humans in the Org Chart](humans-in-the-org.md).

The lead's system prompt includes a **roster** of direct reports. Detailed per-member profiles (skills, backstory, responsibilities) render directly into the lead's Plan-phase prompt from the in-memory `Organization` model.

#### Lead inheritance

When a child unit has no `lead` set, it automatically inherits the lead from its parent unit. This cascades through any number of levels — a division lead becomes the effective lead for every descendant that doesn't specify its own.

```yaml
units:
  - name: "Engineering"
    type: department
    lead: "VP Engineering"          # ← set here
    roles:
      - name: "VP Engineering"
    children:
      - name: "Backend"
        type: team                  # no lead — inherits "VP Engineering"
        roles:
          - name: "Dev A"
          - name: "Dev B"
      - name: "Frontend"
        type: team
        lead: "Frontend Lead"       # explicit — NOT overwritten
        roles:
          - name: "Frontend Lead"
          - name: "Dev C"
```

In this example:

- **Backend** has no lead, so it inherits `VP Engineering`. VP Engineering auto-manages `Dev A` and `Dev B`.
- **Frontend** has an explicit lead (`Frontend Lead`), so the parent's lead is ignored.

Inherited leads work the same as explicit leads for auto-management, task routing, `org.Organization.IsUnitLead`, and the Jira project-key mapping. The only difference is that the lead role lives in an ancestor unit rather than the current one — use `get_effective_lead(unit, org)` from `internal/org` to resolve the lead `Role` object in code.

### Roles at Any Level

Roles can be placed at the org root or directly in any OrgUnit. A department-level role (like a VP) can sit alongside child teams, and org-wide roles (like a CEO) can sit at the root:

```yaml
roles:
  - name: "CEO"
    manages: ["VP Engineering"]

units:
  - name: "Engineering"
    type: department
    roles:
      - name: "VP Engineering"
        manages: ["Backend Lead", "Frontend Lead"]
    children:
      - name: "Backend"
        type: team
        lead: "Backend Lead"
        roles: [...]
```

---

## Onboarding convention

Each `OrgUnit` (and the organisation root) is expected to publish a page titled exactly **`Onboarding`** in its container of the knowledge base — its Confluence space or Plane project.  When an agent spawns into a role, the engine writes nothing into the prompt itself — instead the Plan-phase shows a short `## First-turn onboarding` block listing the unit chain (org → ancestor units → own unit).  The agent reads each `Onboarding` page using its knowledge backend's page-search / page-read MCP tools (`confluence_search` / `confluence_get_page` on Confluence, the `plane` server's page tools on Plane), captures the conventions that matter via `reflect_and_persist` (scope=agent), and calls `mark_onboarded` when done.  After that, the hint disappears from subsequent prompts.

Re-onboarding fires automatically when the org structure changes (the role moves between units, a new ancestor unit is inserted, the role is renamed) — the engine recomputes a chain hash and the prior marker no longer matches.  Source-page content drift is **not** automatic: the agent re-reads at its own discretion, or in response to a page-update notification routed through the existing notification pipeline.

This mirrors how a real new hire learns.  A founder doesn't need YAML config for which onboarding doc to point at — they just maintain an `Onboarding` page per scope, the same way they would for human team members.  See [Agent Learning](agent-learning.md) for the full design.

---

## Hot Reload

The org model is loaded once at startup and can be hot-reloaded at runtime via the Engine API:

- **`engine.reassign()`** — move an agent to a different role (optionally with a new manager)
- **The config apply** — full Tier B hot-reload: spawn new roles, terminate removed roles, swap the agent definition for changed roles, plus diff-and-apply for every other Tier B subsystem (LLM providers, MCP servers, integrations, transports, turn engine, budgets). Driven by each node's reconcile tick once a `PUT /config` moves the activation pointer; see [Configuration concept doc](configuration.md).

Since all agent handlers run in the same Engine process (shared memory), hot reload works by:

1. Updating the shared `Organization` object
2. Cancelling handlers for removed agents, spawning new ones
3. Updating `AgentDefinition` in place for modified agents — picked up on next turn

For non-org subsystems (LLM providers, MCP servers, integrations, transports, learning workers), the apply runs per-subsystem diff handlers that rewire the live instances: providers re-instantiated, per-role MCP children restarted, notification transports swapped. A mid-apply failure rolls back to a snapshot of the pre-apply state.
