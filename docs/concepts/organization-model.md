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
    ├── mcp_env: dict[server → env vars]  (per-agent tool creds, inherited by the
    │                                       unit's direct agent roles; human seats
    │                                       inherit none)
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
| MCP env inheritance | No parent unit to inherit from | An agent seat inherits its unit's `mcp_env`; a human seat inherits nothing |
| Lead auto-management | N/A (no unit lead concept) | Auto-managed by the unit lead unless another direct member of the unit already manages it |
| `org.Organization.UnitFor` | Returns `nil` | Returns the containing unit |

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

**Removing a seat, and adding one back.** Because identity is the handle, what a removed agent seat leaves behind is keyed by it too. Its **mailbox**, and the mail still addressed to it, is kept for 24 hours after the seat leaves the active revision and then retired, so a seat restored within a day finds its backlog and a seat added under the same handle later starts with an empty mailbox. Its **memory** (diary, episodes, counterparty profiles, onboarding markers) is kept, and because it is keyed by the handle or by the agent id derived from it, a seat added again under the same handle reattaches to it. Renaming a seat's handle is a removal of the old handle and an addition of the new one. See [Seat Ownership § The removed seat](seat-ownership.md#the-removed-seat).

### Names and handles are unique

Three identities must each name exactly one thing in the whole company:

| Identity | Unique across | Why |
|---|---|---|
| Seat **handle** | Every seat, agent and human | It names the seat's inbox, its derived agent id and its external accounts. Two seats on one handle share an inbox, or an agent absorbs a person's activity. |
| Seat **name** | Every seat, at any depth | A unit's `lead` and every `manages` entry name a seat and resolve to the first seat of that name. A second seat called the same is unreachable through either, even when its handle differs. |
| Unit **name** | Every unit in the tree, not only siblings | A `manages` entry naming a unit and a root seat's `unit:` reference search the whole tree and resolve to the first match. Two teams called `Platform` under different departments read as distinct on every screen while each reference reaches only one of them. |

Names are compared as the exact string, the same way a reference resolves them: `Platform` and `platform` are two different units. A seat or unit with no name (or a name that derives no handle) is refused by its own rule and is never reported as a duplicate of another. A refusal names every entity that shares the key, in one message per key, and where each one sits:

```
duplicate unit name "Platform": 2 units carry it (under unit "Engineering"; under unit "Product"). ...
```

#### Companies stored before the name rules

Handle uniqueness has always been enforced. Seat name and unit name uniqueness are **admission rules**: they were added after companies existed, and a company that breaks one still runs exactly as it did before. The same holds for a [`unit:` reference on a seat declared inside another unit](#a-seats-unit-reference). So they are enforced where a document is *submitted* and reported where a stored one is *applied*:

- **Refused on every write.** `PUT /config`, `PATCH /config`, a per-entity write, a `/setup` submission that changes the document, `crewlet config import`, `crewlet validate` and a company file `crewlet run` imports as a new revision (`-company` into an empty store, `-import-company` over a different company) all refuse a document with a duplicate seat or unit name, including a write to a company whose stored revision already carries one and a write that does not touch the duplicates. The write that corrects them is accepted.
- **Applied with a warning.** A stored revision carrying a duplicate name (written by an earlier build, or activated by an older peer during a rolling upgrade) is applied by every node, a node boots on it (including one started with `-company` or `-import-company` naming a file that is that revision, or a `-company` file the store's own company outranks), and `POST /config/reload`, a `/setup` credential rotation (which reloads) and a revert to it still work. The vendor commands that act on a company file without storing it (`crewlet gitlab provision`, `crewlet slack provision` and their siblings, `crewlet llm status`) read such a file too. Each node logs `org_admission_warning` once for every violation when it applies the epoch, naming the revision and the entities.
- **Always readable.** `GET /config`, the revision reads, diffs, `crewlet config show` and `crewlet config export` serve the stored document as it is, so the duplicates can be seen and corrected.

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

If a name matches both a role and a unit, the **role takes priority** (no expansion happens for that entry). A unit name expands to every seat in that unit's subtree **except the seat that lists it**: a lead that manages its own team by name does not manage itself. A name matching neither a role nor a unit is kept as written, so a seat that has not been added yet can already be named, and the engine reports it as a [dangling reference](#dangling-references).

### Unit Lead

An OrgUnit may designate a lead via the `lead` field. The lead is responsible for:

- Task routing and assignment within the unit
- Acting as the single point of contact for the unit
- Reasoning about members' properties (backstory, skills, knowledge) to assign tasks to the right individual

When a unit has direct roles and a lead is set, the lead **auto-manages** every direct member that no direct member of the same unit already manages. Three rules decide what counts as already managed, and all three read each `manages` entry the way [unit-name expansion](#managing-by-unit-name) resolves it, so a unit name counts for every seat it reaches:

- **A member another direct member manages keeps that manager.** A tech lead who lists `Dev A`, or who lists the unit `Backend` that `Dev A` sits in, shields `Dev A` from the unit lead.
- **A member the lead already manages is not listed twice.**
- **A member that manages the lead is never claimed.** An engineering manager who manages their own unit by name reaches the unit lead too, and claiming them back would make a two-seat management cycle.

```yaml
units:
  - name: "Engineering"
    lead: "VP Engineering"
    roles:
      - name: "VP Engineering"
    children:
      - name: "Backend"              # inherits VP Engineering as lead
        roles:
          - name: "Tech Lead"
            manages: ["Backend"]     # Dev A and Dev B, by unit name
          - name: "Dev A"
          - name: "Dev B"
```

Here VP Engineering auto-manages only `Tech Lead`. `Dev A` and `Dev B` report to Tech Lead alone.

**Only the unit's own direct members shield.** A seat outside the unit that manages it, such as a root-level CEO with `manages: ["Backend"]`, lists every seat in `Backend` but does not stop `Backend`'s lead from auto-managing those seats as well. That scope is deliberate: management is stored on the manager, so a CEO managing a whole division by name would otherwise leave every lead inside it with an empty roster. The consequence is that such a member has **two managers**. `org.Organization.Manager` reports the first seat in walk order that lists it, and root-level seats are walked first, so the CEO is the one an identity prompt names. To keep the unit lead as the primary manager, have the outside seat manage the lead (`manages: ["Backend Lead"]`) rather than the unit.

The lead can be a **human seat** — a human manager running an AI team is a first-class pattern: agents escalate to the human with their own Slack/Jira tools (an @-mention), and the human assigns work in the PM tool. See [Humans in the Org Chart](humans-in-the-org.md).

The lead's system prompt includes a **roster** of direct reports. Detailed per-member profiles (skills, backstory, responsibilities) render directly into the lead's executor prompt from the in-memory `Organization` model.

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

Inherited leads work the same as explicit leads for auto-management, task routing, `org.Organization.IsUnitLead`, and the Jira project-key mapping. The only difference is that the lead role lives in an ancestor unit rather than the current one. Use `org.Organization.EffectiveLead` to resolve the lead seat in code.

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

#### A seat's `unit` reference

A seat declared at the **root** can name the unit it belongs to with `unit:`, which is how the per-entity configuration API adds a seat to a unit. The engine moves such a seat into that unit before anything else is derived, so it inherits the unit's tool credentials and is auto-managed by the unit's lead exactly as a seat written inside the unit is. A seat moved this way is still reported, and edited, where it was written.

The reference places a root seat and nothing else. A seat declared **inside** a unit is never moved by one, so a `unit:` on it that names a different unit reads as a placement and does nothing: the seat stays where it is written while the document says it belongs elsewhere. A document carrying one is refused at that seat's `unit`; repeating the name of the unit the seat is declared in is accepted. This is an [admission rule](#companies-stored-before-the-name-rules): a stored company that already carries such a reference still runs exactly as it did.

### Dangling references

A `lead`, a root seat's `unit`, or a `manages` entry names another entity by name, and that name may resolve to nothing: a misspelling, a seat that was removed, or a seat that has not been added yet. None of these refuses the revision. Live configuration changes build an organization in pieces (a unit can be added before the seat that leads it, and every node applies each intermediate revision), so refusing a partly wired organization would make that sequence impossible. Every reader treats the reference as absent instead: a unit whose lead is dangling runs with no lead, a seat whose `unit` names nothing stays at the root, and a `manages` entry naming nothing manages nobody.

The engine reports them rather than letting them pass silently. **Each node logs every dangling reference once for each epoch it applies**, as a warning named `org_dangling_reference`, and never for a revision it refused. A reference that is still logged after the organization is fully wired is a misspelling nothing else will report.

| `ref` | Reported when | `from` | `to` |
|---|---|---|---|
| `lead` | A unit's own `lead` names no seat | The unit | The lead as written |
| `unit` | A root seat's `unit` names no unit | The seat | The unit as written |
| `manages` | A `manages` entry names neither a seat nor a unit | The seat | The entry as written |
| `gitlab_access_level` | A key of `integrations.gitlab.provisioning.access_levels` is no seat's handle | `integrations.gitlab.provisioning.access_levels` | The handle |

Each line also carries `epoch`, `revision` and a `detail` sentence saying what the engine does meanwhile and how to resolve it.

**What was written is reported, once.** A dangling lead is reported on the unit that declares it, never on the child units that inherit it: they wrote nothing, and there is nothing to fix on them. A child unit that writes the same name itself is reported separately, because it is a second place to correct. A `manages` entry naming a unit that holds no seats resolves to nobody but is not a misspelling, so it is not reported.

**A stale GitLab access level is worth removing promptly.** Access level overrides are looked up by handle when a seat's service account is provisioned, so the entry left behind by a removed seat grants its level to the next seat that derives the same handle. It is reported whether or not GitLab is currently enabled, since re-enabling it is exactly when the stale grant would take effect.

---

## Onboarding convention

Each `OrgUnit` (and the organisation root) is expected to publish a page titled exactly **`Onboarding`** in its container of the knowledge base — its Confluence space.  When an agent spawns into a role, the engine writes nothing into the prompt itself — instead a dedicated first-turn onboarding pass runs before the executor, shown a short `## First-turn onboarding` block listing the unit chain (org → ancestor units → own unit).  The agent reads each `Onboarding` page using its knowledge backend's page-search / page-read MCP tools (`confluence_search` / `confluence_get_page`), captures the conventions that matter via `reflect_and_persist` (scope=agent), and calls `mark_onboarded` when done.  After that, the hint disappears from subsequent prompts.

Re-onboarding fires automatically when the org structure changes (the role moves between units, a new ancestor unit is inserted, the role is renamed) — the engine recomputes a chain hash and the prior marker no longer matches.  Source-page content drift is **not** automatic: the agent re-reads at its own discretion, or in response to a page-update notification routed through the existing notification pipeline.

This mirrors how a real new hire learns.  A founder doesn't need YAML config for which onboarding doc to point at — they just maintain an `Onboarding` page per scope, the same way they would for human team members.  See [Agent Learning](agent-learning.md) for the full design.

---

## Hot Reload

The organization is part of the company configuration, so it changes without a restart. Activating a revision (`PUT /config`, `PATCH /config`, a per-entity write, a revert, or `crewlet config import`) moves the fleet's activation pointer, and each node's reconcile tick applies the revision it names. The stages of that apply, and what a refused one leaves behind, are in [Live Propagation](configuration.md#live-propagation).

**A running organization is never edited in place.** The apply builds a new `Organization` from the revision, normalizes and validates it, and publishes it as part of a new epoch together with everything else built from the same document. Turns on many goroutines read the published tree at once, so editing it would be a data race with no owner. A turn pins the epoch it starts on and reads only that epoch until it ends, so an organization change reaches a seat at its next turn. A revision whose organization does not validate is refused before its epoch is published, and the node keeps serving the previous one.

**Seats follow the new chart.** After the swap the node creates a mailbox for every seat the revision adds, and [seat ownership](seat-ownership.md) converges placement onto the new seat list, releasing a seat the organization no longer has. An agent seat is identified by its handle, and its runtime id is derived from the company name and that handle (`org.DeriveAgentID`). A seat therefore keeps its identity and its memory through a rename or a move for as long as its handle is unchanged. A seat whose handle changes is a different seat, and renaming the company gives every agent seat a new id.
