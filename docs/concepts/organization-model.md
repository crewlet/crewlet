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
    ├── confluence_space: str              (integrations.confluence.space — the unit's
    │                                       Confluence space: where its pages live and
    │                                       where page activity routes. Does NOT scope
    │                                       knowledge reads)
    ├── mcp_env: dict[server → env vars]  (per-agent tool creds, inherited by roles)
    ├── roles: Role[]                      (agents directly in this unit)
    ├── children: OrgUnit[]                (nested sub-units, recursive)
    └── schedules: Schedule[]              (unit recurring work, NOT inherited;
                                            see Scheduling)

Role (a SEAT — can live at root level OR inside an OrgUnit)
├── kind: agent | human                (who holds the seat; default agent)
├── name, responsibilities, behavioral_guidelines
├── contact: {slack_user_id, mattermost_user_id, atlassian_account_id,
│             github_login, gitlab_username}  (human seats — external
│                                              identities)
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
├── confluence_space: str (root-level roles — integrations.confluence.space;
│                          the role's Confluence space. Does NOT scope
│                          knowledge reads — that is the org-wide
│                          knowledge.confluence_spaces only)
├── atlassian_products: str[] (integrations.atlassian.products — which
│                              Atlassian products this seat is LICENSED for.
│                              Not an identity: absent = every product the
│                              company configures, [] = none, a list = those.
│                              Agent seats only; read by `crewlet atlassian
│                              provision` and reported by the engine's own
│                              company view, never by a routing decision)
├── token_budget: int  (0 = unlimited)
├── llm: str           (provider key, default = "default")
├── llm_auxiliary: str (optional cheap-model key for reflection /
│                       summarisation work)
├── learning_enabled: bool? (per-role override for the agent-learning
│                            subsystem)
├── slack: dict        (role.integrations.slack — this seat's OWN Slack
│                       app: bot_token + signing_secret, both required
│                       together. Slack gives each agent its own app, so
│                       there is no company-wide credential)
└── schedules: Schedule[]  (role-scoped recurring work; see
                            [Scheduling](scheduling.md))
```

Roles can live in two places:

- **Inside an OrgUnit** (`units[].roles`) — scoped to that unit for MCP env inheritance and lead auto-management. The unit's [`integrations.jira.project`](../integrations/jira.md) gives the team its tracker "home" (webhook routing + write target), but does not scope what the role can *read*.
- **At the root level** (`roles`) — org-wide agents that don't belong to any specific team. They participate in the `manages[]` hierarchy like any other role and are fully visible to task routing; a root-level role can carry its own `integrations.jira.project` identity. Knowledge **read** scope for every agent is the org-wide `org.Organization.ConfluenceSpaces` only.

> Every one of these identities is consulted. Each tracker routes an item that names nobody to the lead of the unit that owns the project, and Confluence does the same for a page change nobody was mentioned in. Neither narrows what an agent can READ: knowledge scope is the org-wide `knowledge.confluence_spaces` only, because letting a unit's identity double as a read scope is how an agent ends up unable to read the page it was told to follow. See [Jira](../integrations/jira.md) and [Confluence](../integrations/confluence.md).

**`integrations.atlassian.products` is the one entry in a seat's integrations block that is not an identity, and the distinction is the point.** `jira_project` and `confluence_space` say *where a seat works* — where activity that names nobody lands, and where the seat files what it produces. `products` says what the seat is **licensed** for, and it exists because an Atlassian product licence is billable and the free service-account allowance an organization gets without Atlassian Guard is small. Collapsing it into "every product the company configures" would buy a documentation seat a Jira licence it never opens, once per seat, per run. `products: [confluence]` is how that seat consumes one licence — and it is also the only thing it can *do*, because the minted token's scopes are derived from this list at mint time, so a writer agent holds no credential that can move a sprint. Narrowing the list on a seat that already has an account re-mints its credential with the smaller scope set, because the provisioner exercises the credential against the products the seat no longer names and a success there proves the token still reaches one. The **licence** is not given back — Atlassian offers no route for that — so the run says which one is still billable.

```yaml
roles:
  - name: "Tech Writer"
    integrations:
      atlassian:
        products: [confluence]     # absent = every configured product; [] = none
```

All three states are real settings and none of them is a default for another. **Absent** takes every product the company configures, which is what a seat working across the org wants. **An explicit empty list** takes none — how a seat that lives entirely in chat is kept out of a provisioning run without deleting its `mcp_env`. **A list** takes exactly those, narrowed to what the company actually configures, so a seat naming Confluence in a company that has none is not licensed into a site that is not there. The block sits on a **role** and never on a unit, because a licence is bought per seat rather than per team; on a **human** seat it is refused by validation, since a person already holds their own Atlassian account — reached by `contact.atlassian_account_id`, one id covering both products — and a product list on one is a licence nobody would ever buy. It is read by [`crewlet atlassian provision`](../integrations/atlassian.md#which-products-a-seat-is-licensed-for), and by the engine's own company view: [`GET /integrations`](../reference/api-endpoints.md#get-integrations) — the dashboard's Integrations room — runs the provisioner's own planner over the org chart to name the seats a run would mint an account for, so a licence the config grants is one an operator can see without running anything. What never reads it is the runtime: no routing, parser, transport or delivery decision consults it, so narrowing a seat's products changes what that seat is licensed to *do* and never what reaches it.

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

Each `OrgUnit` (and the organisation root) is expected to publish a page titled exactly **`Onboarding`** in its container of the knowledge base — its Confluence space.  When an agent spawns into a role, the engine writes nothing into the prompt itself — instead the Plan-phase shows a short `## First-turn onboarding` block listing the unit chain (org → ancestor units → own unit).  The agent reads each `Onboarding` page using its knowledge backend's page-search / page-read MCP tools (`confluence_search` / `confluence_get_page`), captures the conventions that matter via `reflect_and_persist` (scope=agent), and calls `mark_onboarded` when done.  After that, the hint disappears from subsequent prompts.

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
