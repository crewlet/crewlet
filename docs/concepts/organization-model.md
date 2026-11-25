# Organization Model

The organization model (`internal/org`) is the company hierarchy. It determines how agents communicate, what knowledge they can access, who they report to, and how tasks flow.

---

## Flexible Hierarchy

Units (`org.Unit`) nest recursively to any depth: flat teams, departments with sub-teams, divisions, squads, pods, or any custom structure.

The runtime tree the engine builds from a revision (`org.Organization`, with the Go type of each field):

```
Organization
├── Name, Mission, Vision string; Policies []string
├── TokenBudget TokenCeilings              (org-wide ceiling per calendar window
│                                           — day, week, month — on the company
│                                           clock; an absent window is uncapped)
├── KnowledgeScope []string                (knowledge.scope: the one org-wide
│                                           knowledge read scope)
├── Roles []*Role                          (root-level org-wide seats)
└── Units []*Unit
    ├── Name string                         (DISPLAY only: nothing references a
    │                                        unit by it)
    ├── ID string                           (`id`: the unit's KEY — what a
    │                                        `manages:` entry and a root seat's
    │                                        `unit:` resolve, and what everything
    │                                        durable is filed under. Minted from
    │                                        the name at import when unset. See
    │                                        "A unit's key" below)
    ├── Type UnitType; Purpose string; Goals []string
    ├── Lead string                         (the HANDLE of the seat leading it)
    ├── KnowledgeRefs []string
    ├── Channel string                     (team channel on the company's chat
    │                                       surface, inherited by children. An
    │                                       ADDRESS: lowercase letters, digits, `-`
    │                                       and `_`, up to 64 — `backend`, never
    │                                       `#backend` or `C_BACKEND`. On Chatlet
    │                                       the engine opens this room for
    │                                       the unit that DECLARES it — a child
    │                                       that only inherits it gets no room of
    │                                       its own; its seats are in the
    │                                       parent's, as its subtree. A room
    │                                       belongs to ONE unit there, so two
    │                                       units declaring one channel is
    │                                       refused — an admission rule)
    ├── Project string                     (tracker identity: lead-fallback routing,
    │                                       the project the team files under, and the
    │                                       unit an item filed into that project with
    │                                       no unit of its own belongs to.
    │                                       VENDOR-NEUTRAL: it names a Worklet project
    │                                       or a Jira one, whichever tracker.backend
    │                                       the company runs)
    ├── Space string                       (knowledge identity: where its pages live
    │                                       and where page activity routes. Vendor-
    │                                       neutral likewise. Does NOT scope knowledge
    │                                       reads)
    ├── MCPEnv MCPEnv                      (per-server tool credentials, inherited by
    │                                       the unit's direct agent seats; human seats
    │                                       inherit none)
    ├── Roles []*Role                      (seats directly in this unit)
    ├── Children []*Unit                   (nested sub-units, recursive)
    └── Schedules []Schedule               (unit recurring work, NOT inherited;
                                            see Scheduling)

Role (a SEAT: can live at root level OR inside a unit)
├── Kind RoleKind                      (agent | human; default agent)
├── Name string                        (DISPLAY, plus the source of the derived
│                                       handle: nothing references a seat by it)
├── Responsibilities, BehavioralGuidelines []string
├── Contact *HumanContact              (human seats, optional: slack_user_id,
│                                       mattermost_user_id, atlassian_account_id,
│                                       github_login, gitlab_username — where a
│                                       message can be sent. Who holds the seat
│                                       on the engine's own surface is the
│                                       identity directory's, not a field here)
├── Availability string                (human seats: rendered into rosters)
├── Backstory string                   (personality, background, expertise)
├── Goal string                        (individual mission)
├── DeclaredHandle string              (the `handle` override; Role.Handle()
│                                       derives the slug when empty. THE HANDLE
│                                       is what `lead:` and `manages:` resolve)
├── Email string                       (indexed so an address resolves to the seat)
├── Manages []string                   (seat HANDLES or unit KEYS this seat
│                                       manages)
├── MCPEnv MCPEnv                      (per-server tool credentials: env vars for
│                                       stdio servers, headers for http servers
│                                       like the remote GitHub MCP. Tool creds
│                                       only; the tracker and knowledge identity
│                                       is the Project / Space fields below)
├── Project string                     (a root-level seat's own tracker identity;
│                                       a seat inside a unit takes the unit's:
│                                       lead-fallback routing and write home, NOT
│                                       a credential)
├── Space string                       (likewise, the seat's own knowledge
│                                       container. Does NOT scope knowledge reads,
│                                       that is the org-wide knowledge.scope only)
├── TokenBudget TokenCeilings          (this seat's own ceiling per window, on
│                                       top of the company's; absent = uncapped)
├── LLM, LLMReview, LLMSubagent,
│   LLMAuxiliary, LLMJudge ProviderKeys (the executor's chain and the per-phase
│                                       satellites; see Turn Engine)
├── Workers []string                   (the worker templates this seat may use)
├── LearningEnabled Toggle             (per-seat override for agent learning)
├── Sandbox *RoleSandbox               (role.sandbox: what the seat's executor
│                                       box reads: setup, env, optional_keys, mcp,
│                                       pause_ttl_seconds, max_turns,
│                                       max_concurrent_runs, browser)
├── Placement                          (role.placement: which nodes may run it)
├── CodeAccess string                  (role.code.access as written:
│                                       read_only, review or full_access;
│                                       empty where the document is silent,
│                                       which reads as read_only. The one
│                                       per-seat code tier: Gitlet's cap on
│                                       what the chart grants.
│                                       Agent seats only)
├── Slack SlackIdentity                (role.integrations.slack: this seat's OWN
│                                       Slack app, bot_token and signing_secret)
├── Mattermost MattermostIdentity      (role.integrations.mattermost: its bot)
└── Schedules []Schedule               (role-scoped recurring work; see
                                        [Scheduling](scheduling.md))
```

A seat's `avatar` (a Crewlet character and a colour) is presentation only, so it is not on the runtime `Role`: it goes from the document to the org the dashboard reads (`GET /org`) and no prompt, route or placement reads it. See the [Role Fields Summary](../getting-started/configuration.md#role-fields-summary).

Roles live in two places:

- **Inside a unit** (`units[].roles`): scoped to that unit for MCP env inheritance and lead auto-management. The unit's `project` is the team's tracker home (routing and write target) but does not scope what the role can *read*.
- **At the root level** (`roles`): org-wide agents in no team. They join the `manages[]` hierarchy and task routing like any role, and may carry their own `project`. Knowledge **read** scope for every agent is only the org-wide `org.Organization.KnowledgeScope`.

> Every one of these identities is consulted. The tracker routes an item that names nobody to the lead of the unit that owns the project, and the knowledge base does the same for a page change nobody was mentioned in, whichever backend serves each (so the keys name neither). Neither narrows what an agent can READ: knowledge scope is only the org-wide `knowledge.scope`, because a unit identity doubling as a read scope leaves an agent unable to read the page it was told to follow. See [Jira](../integrations/jira.md) and [Confluence](../integrations/confluence.md).

> **They also decide who leads what.** On Worklet and Doclet, the unit's effective lead leads the project its `project:` names and the page container its `space:` names; a root seat leads its own. On the engine's own chat the lead is also who wakes on a person's message in the room its `channel:` opens when nobody else was reached. These are what the [authority table](identity-and-access.md#the-authority-table-one-function-decides) asks when a write is a lead's to make: a project's field declarations, re-routing its work, renaming, trashing and restoring a page in the container. The two keys are separate relations: a unit whose project and space are spelled differently has a lead for each. A `space:` is also what lets pages be created at all: a page is created only in a space the company declares: a unit's or a seat's `space:`, `knowledge.root_space`, or the tool-skills container ([Who may write where](knowledge-system.md#who-may-write-where)).

---

## Common Org Patterns

### Root-Level Roles (CEO/CTO above all teams)

Org-wide leaders sit at the root, outside any unit. They manage unit leads via `manages[]` and take part in task routing.

The root is also where the **founder** belongs, as a [human seat](humans-in-the-org.md#the-founder-seat) above the top agent, so escalations end at a person and agents recognise the founder's activity on Slack/Jira/GitHub:

```yaml
roles:
  - name: "Jane Founder"
    kind: human
    manages: [ceo]            # handles, not display names
    contact: { slack_user_id: U0FOUNDER }
  - name: "CEO"
    handle: ceo
    goal: "Set company direction"
    manages: [vp-eng, vp-product]
```

```yaml
roles:
  - name: "CEO"
    goal: "Set company direction"
    manages: [vp-eng, vp-product]

units:
  - name: "Engineering"
    id: engineering           # the unit's key: what `manages:` and `unit:` resolve
    type: department
    lead: vp-eng              # the lead seat's handle
    roles:
      - name: "VP Engineering"
        handle: vp-eng
        manages: [backend-lead]
    children:
      - name: "Backend"
        id: backend
        type: team
        lead: backend-lead
        roles: [...]
  - name: "Product"
    id: product
    type: department
    lead: vp-product
    roles: [...]
```

| Aspect | Root-level role | Unit role |
|--------|----------------|-----------|
| Knowledge scope | Org-wide (reads are role-independent — see [Knowledge System](knowledge-system.md)) | Org-wide (same) |
| MCP env inheritance | No parent unit | An agent seat inherits its unit's `mcp_env`; a human seat inherits nothing |
| Lead auto-management | N/A (no unit lead) | Auto-managed by the unit lead unless another direct member of the unit already manages it |
| `org.Organization.UnitFor` | Returns `nil` | Returns the containing unit |

### Flat Startup (no departments)

```yaml
units:
  - name: "Product Team"
    id: product-team
    type: team
    lead: founder
    roles:
      - name: "Founder"
        handle: founder
        manages: [dev-1, dev-2]
      - name: "Dev 1"
      - name: "Dev 2"
```

### Departments with Teams (traditional)

```yaml
units:
  - name: "Engineering"
    id: engineering
    type: department
    lead: vp-eng
    children:
      - name: "Backend"
        id: backend
        type: team
        lead: backend-lead
        roles: [...]
      - name: "Frontend"
        id: frontend
        type: team
        lead: frontend-lead
        roles: [...]
  - name: "Product"
    id: product
    type: department
    children:
      - name: "Product Management"
        id: product-management
        type: team
        lead: pm
        roles: [...]
```

### Division > Department > Team (enterprise)

When only the top-level unit has a lead, it cascades down via [lead inheritance](#lead-inheritance):

```yaml
units:
  - name: "Technology"
    id: technology
    type: division
    lead: cto
    roles:
      - name: "CTO"
        handle: cto
    children:
      - name: "Engineering"
        id: engineering
        type: department          # inherits cto as lead
        children:
          - name: "Platform"
            id: platform
            type: team
            lead: platform-lead   # explicit — overrides inherited cto
            roles: [...]
          - name: "Application"
            id: application
            type: team            # inherits cto as lead
            roles: [...]
```

### Spotify Model (Tribes + Squads)

```yaml
units:
  - name: "Infrastructure Tribe"
    id: infra-tribe
    type: tribe
    lead: tribe-lead
    children:
      - name: "Provisioning Squad"
        id: provisioning-squad
        type: squad
        lead: provisioning-lead
        roles: [...]
      - name: "Networking Squad"
        id: networking-squad
        type: squad
        lead: networking-lead
        roles: [...]
```

### Pod-Based (cross-functional)

```yaml
units:
  - name: "Auth Pod"
    id: auth-pod
    type: pod
    lead: auth-lead
    roles:
      - name: "Auth Lead"
        manages: [auth-dev, auth-designer]
      - name: "Auth Dev"
      - name: "Auth Designer"
  - name: "Billing Pod"
    id: billing-pod
    type: pod
    lead: billing-lead
    roles: [...]
```

---

## Unit Types

`type` can be any string and is informational only. Well-known types:

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

---

## Key Concepts

### Role = Seat

A Role is a unique **seat** with its own backstory, skills, personality and expertise, held by an AI agent (`kind: agent`, the default) or a **human teammate** (`kind: human`). Each agent seat is one agent, identified by an id derived from the company name and its handle. Human seats join the same hierarchy (manages, unit lead, rosters, escalation) but are addressable-only: no runtime, no inbox, no LLM. Every person who signs in holds exactly one human seat, and is invited or created onto it from the seat in Company › Structure, so declare a human seat for each person who is to join. Seats are defined individually, never interchangeable. See [Humans in the Org Chart](humans-in-the-org.md).

### What a signed-in reader can see about a seat

The org chart is a projection every signed-in reader may read with no permission (the [overview](identity-and-access.md#everybodys-overview) everybody sees: [`GET /org`](../reference/api-endpoints.md#get-org) and the socket's `org` push; never without a credential). Each field is decided one at a time, some by who is reading. Beside the founder's prose and the hierarchy it carries:

- **The company's clock** (`timezone`): the one clock every day, week and month is cut on; an unwritten `timezone` is UTC. Every reader gets it.
- **Its token budget**: the ceilings the document writes for the seat (and, at the top, the company) per calendar window. Every reader gets them; the meters spending against them are [`GET /budgets`](../reference/api-endpoints.md#get-budgets), also everybody's.
- **Its model chain** (`llm`): every phase's chain of provider keys as a turn resolves it: the flat `llm_<phase>` fields over the `llm` mapping, the seat's `llm` for a phase naming nothing, then the company's `default` provider or its first. A key is the label `providers.llm` gives an entry; the model and credentials behind it are not shown. **Only a reader holding `admin`** gets it, because it derives from configuration no other reader may read.
- **Its tool sources** (`tool_sources`): `builtin`, then `mcp:<server>` for each MCP server the seat is granted: by the server's own `access` (every agent seat where it has none; the units it names reach every agent seat beneath them), or, for a server reading each agent's own vendor credential, by the integration lane the seat is on. Never a server the company turned off (`enabled: false`) or a limit on the way down turns off (`mcp_limits`). A seat's or unit's `mcp_env` grants nothing: it holds the values a server the seat already gets is started with ([Choosing which agents get a server](../guides/tools-and-mcp.md#choosing-which-agents-get-a-server)). The engine builds the seat's surface and starts its server instances by the same rule. Credentials are never shown, and the list only to an `admin` reader, for the model chain's reason.

A human seat carries neither of the last two (no model, no tools). Everything else about a seat (contact identities, email, `mcp_env`, `mcp_limits`, sandbox, placement, integrations, workers, schedules) is read only through the configuration (`GET /config`, `GET /config/roles/{handle}`; `admin`, credentials masked).

### Handle-Based Identity

Every agent gets a deterministic **handle** slug derived from its role name:

```
Role Name           Handle
─────────────────   ────────────────
Sarah Chen          sarah-chen
Marcus Rivera       marcus-rivera
Alex Kim            alex-kim
```

Handles are the canonical identity inside and outside the document: a unit's `lead:` and every `manages:` entry name a seat by handle, as do notification routing and external mappings (e.g. a Jira assignee, a GitLab service account). Nothing references a seat by its `name`. A seat's `email` is matched too, since inbound Jira and GitHub payloads identify people by address. A plus tag spelling a seat's handle names that seat (`notif+sarah-chen@co.com` is `sarah-chen`, whatever address she declares); otherwise an address matches the seat that declared it, compared lower-cased with any plus tag dropped, so `sarah+jira@co.com` reaches the seat declaring `sarah@co.com`. Two seats whose addresses compare equal that way are refused, and a person's own sub-address tag should not be a seat's handle. Set a custom handle:

```yaml
roles:
  - name: Senior Engineer
    handle: sr-eng        # Override auto-derived "senior-engineer"
```

**Removing a seat, and adding one back.** What a removed agent seat leaves behind is keyed by its handle:

- **Mailbox** (and mail still addressed to it): kept 24 hours after the seat leaves the running company's agent seats, then retired. A seat restored within a day finds its backlog; one added under the same handle later starts empty.
- **Executor**, on any turn it is out for: kept the same 24 hours and ended when the mailbox is retired, each launch announced as lost.
- **Memory** (diary, episodes, counterparty profiles, onboarding markers): kept, keyed by the handle or the agent id derived from it, so a seat added again under the same handle reattaches to it.
- **A removed human seat's chat read state** (how far its holder had read in each room, mutes, do-not-disturb): kept 24 hours, then forgotten.
- **A removed human seat's Home board** (the widgets its holder placed on the dashboard's Home): kept 24 hours, then forgotten.
- **An agent seat made a person's** gets the same 24 hours; it is the one departure that can be reversed: made an agent's again within them, it is the same seat and returns to its backlog.

A handle never changes in place: a document giving a seat a different handle removes the old seat and adds a new one. Every write mints the handle a seat leaves out and stores it, so correcting a `name` by editing the stored document (the org builder, `GET` then `PUT /config`, `PATCH /config`, the per-entity routes) keeps the handle. A **file** is minted afresh on every import: correcting a `name` in a file that leaves `handle` out and importing again replaces the agent seat with no warning. Declare `handle` on every seat in a file you keep, or start it from `crewlet config export`, which writes every handle out.

A human seat somebody holds cannot be removed, nor made an agent's, until freed: the write is refused `409 seat_held` naming the holder: a person (moved to another human seat or removed; a person's seat is never cleared), a service account (unbound), or an open invitation (cancelled). See [Seat Ownership § The removed seat](seat-ownership.md#the-removed-seat).

### Handles and keys are unique; names are not

Two identities must each name exactly one thing in the whole company:

| Identity | Unique across | Why |
|---|---|---|
| Seat **handle** | Every seat, agent and human | It names the seat's inbox, derived agent id and external accounts, and is what a unit's `lead` and every `manages` entry resolve. Two seats on one handle share an inbox, or an agent absorbs a person's activity. |
| Unit **key** (`id`, minted from the name where none is declared) | Every unit in the tree, not only siblings | A `manages` entry naming a unit and a root seat's `unit:` search the whole tree and resolve to the first unit answering to the key. Two units on one key look distinct on every screen while each reference reaches only one. |

**A display name is neither.** Two seats may share a name on two handles, and two units a name on two keys (two "Platform" teams in two departments, two people called "Alex"). A colleague lookup offers every seat an exact name matches, each with its handle, and a roster row carries the handle beside the name. The [org builder](../guides/org-builder.md) suggests "Software Engineer 2" beside an existing "Software Engineer" as a convenience, not a rule.

A unit key is compared folded (lower-cased, whitespace as a hyphen), so `Product Team` and `product-team` are one key. A unit with no `id` is keyed on one minted from its name, which is how two alike-named units collide when neither declares an id. A seat or unit with no name (or a name deriving no handle) is refused by its own rule and never reported as a duplicate. A refusal names every entity sharing the key, one message per key, where each sits, and for a unit the key as written:

```
duplicate unit key "platform": 2 units answer to it (unit "Platform" under unit "Engineering" (key "platform"); unit "Platform" under unit "Product" (key "platform")). ...
```

#### Where the rules are held

Handle uniqueness is a **runnable** rule: a document breaking it is refused everywhere it is read. Unit-key uniqueness is an **admission rule**, as is a [`unit:` reference on a seat declared inside another unit](#a-seats-unit-reference): authoring hygiene that a company breaking one still runs under, which a later build may add to or relax. They are enforced where a document is *submitted* and only reported where a revision is *applied*, because in a rolling upgrade a newer peer may activate a revision admitted under rules this build does not share, and refusing it would split the fleet's epoch:

- **Refused on every write.** `PUT /config`, `PATCH /config`, a per-entity write, a `/setup` submission that changes the document, `crewlet config import`, `crewlet validate` and a company file `crewlet run` imports as a new revision (`-company` where no company exists yet, `-import-company` over a different company) all refuse a document that breaks one, naming the rule. This includes a write to a company whose stored revision already breaks it and a write that does not touch what breaks it. The write that corrects it is accepted.
- **Applied with a warning.** A stored revision that breaks one (activated by a newer peer) is applied by every node: one started with `-import-company` naming a file that is that revision, a node booting on it (with a `-company` file beside it that is that revision, or one the store's own company outranks), and `POST /config/reload`, a `/setup` credential rotation (which reloads) and a revert to it all still work. The vendor commands that act on a company file without storing it (`crewlet gitlab provision` and its siblings, `crewlet llm status`) read one as it stands. Each node logs `org_admission_warning` once per violation when it applies the epoch, naming the revision and the entities, with each document path under `paths`.
- **Always readable.** `GET /config`, the revision reads, diffs, `crewlet config show` and `crewlet config export` serve the stored document as it is, so a violation can be seen and corrected.

### A unit's key is its identity

A unit's **name** is what people read (prompt, board, channel topic) and is renamed like prose. Its **key** (`id`) is its address: what a `lead`, a `manages` entry and a root seat's `unit:` name it by, and what everything durable is filed under (which team a work item is filed into, whose lead hears about it, which unit a project belongs to). Every unit has one: a document with no `id` gets a key minted from the name on every write, lower-cased with whitespace as a hyphen, and stored; re-importing the same file unchanged mints the same keys.

```yaml
units:
  - name: Engineering
    id: eng            # minted as `engineering` when left out — on every import
    project: ENG
```

So **renaming a unit moves nothing** as long as its key stays. The **key** never changes in place: a document giving a unit a different key removes the old unit and adds a new one. Every write mints and stores the key a unit leaves out, so renaming by editing the stored document keeps the key; a file is minted afresh on every import, so a unit renamed in a file that leaves `id` out gets the key its new name mints. Declare `id` on every unit in a file you keep.

**Every spelling names the team wherever a stored reference is resolved** (a `unit` on `create_work_item`, a `routing_unit`, every `unit=` filter, a project listing, a workload, a view strip's container): the unit's key, compared folded, and its **name**, in any case, where exactly one unit carries it. So `unit: Engineering` reaches the team keyed `eng` when no other unit is called that. A name two units share names neither, and a reference that is one unit's key and another's name resolves to the unit whose **key** it is.

References *inside the org chart itself* (a unit's `lead`, a `manages` entry, a root seat's `unit:`) are stricter, resolved against their own document: a unit by key, a seat by handle. One naming nothing is a [dangling reference](#dangling-references), not silently ignored.

**A new key does not take the old one's work.** Work keeps the key it was filed under and nothing rewrites it; see [Naming a team](../guides/worklet.md#naming-a-team-its-key-or-its-name). Nor does a new name re-onboard anybody: onboarding turns on the chain of unit keys and handles above a seat (see [Onboarding convention](#onboarding-convention)).

### Management Hierarchy

Hierarchy is encoded through `manages` on roles: a Team Lead manages Engineers, a VP manages Team Leads. See [Agent Runtime](agent-runtime.md) for how it drives execution.

- **Authority** flows from hierarchy, beside the [permissions](identity-and-access.md#access-admin-and-one-permission-per-app) a person holds: a lead re-orders their reports' queues, pauses, steers and answers for their agents, reads their trail and edits their own team. The agent's identity prompt names the manager so handoffs (a Slack mention, a Jira comment, or `a2a_ask` during Execute) reach the right person.
- **Task assignment** is the unit lead's job: the lead agent reasons about its members and assigns tasks.

#### Managing by unit key

`manages` accepts **seat handles** and **unit keys**. An entry matching a unit's key (and no seat's handle) expands to all roles in that unit, including descendant child units.

```yaml
roles:
  - name: "CEO"
    manages: [engineering, product]   # unit keys — expand to every seat in each unit

units:
  - name: "Engineering"
    id: engineering
    type: team
    lead: tech-lead
    roles:
      - name: "Tech Lead"
      - name: "Dev A"
      - name: "Dev B"
  - name: "Product"
    id: product
    type: team
    lead: pm
    roles:
      - name: "PM"
      - name: "Designer"
```

After expansion the CEO manages `tech-lead`, `dev-a`, `dev-b`, `pm`, `designer` (handles, which is what a normalized manages list holds).

Handles and unit keys mix freely:

```yaml
manages: [cto, backend]   # cto is a seat handle, backend is a unit key
```

If a token is both a seat's handle and a unit's key, the **seat takes priority** (no expansion). A unit key expands to every seat in the subtree **except the seat that lists it**, so a lead managing its own team by key does not manage itself. A token matching neither is kept as written, so a seat not yet added can be named; the engine reports it as a [dangling reference](#dangling-references).

### Unit Lead

A unit may designate a lead via `lead`, who:

- routes and assigns tasks within the unit
- is the unit's single point of contact
- reasons about members' profiles (background, goal, responsibilities) to assign tasks to the right individual

When a unit has direct roles and a lead, the lead **auto-manages** every direct member that no direct member of the same unit already manages. All three rules read each `manages` entry as [unit-key expansion](#managing-by-unit-key) resolves it, so a unit key counts for every seat it reaches:

- **A member another direct member manages keeps that manager.** A tech lead who lists `dev-a`, or the unit key `backend` that `dev-a` sits in, shields `dev-a` from the unit lead.
- **A member the lead already manages is not listed twice.**
- **A member that manages the lead is never claimed.** An engineering manager managing their own unit by key reaches the unit lead too; claiming them back would make a two-seat cycle.

```yaml
units:
  - name: "Engineering"
    id: engineering
    lead: vp-eng
    roles:
      - name: "VP Engineering"
        handle: vp-eng
    children:
      - name: "Backend"              # inherits vp-eng as lead
        id: backend
        roles:
          - name: "Tech Lead"
            manages: [backend]       # Dev A and Dev B, by unit key
          - name: "Dev A"
          - name: "Dev B"
```

Here VP Engineering auto-manages only `tech-lead`; `dev-a` and `dev-b` report to Tech Lead alone.

**Only the unit's own direct members shield.** A seat outside the unit that manages it, such as a root-level CEO with `manages: [backend]`, lists every seat in `Backend` but does not stop its lead auto-managing them too. This is deliberate: management is stored on the manager, so a CEO managing a division by key would otherwise leave every lead inside it with an empty roster. Such a member has **two managers**: `org.Organization.Manager` reports the first seat in walk order that lists it, root-level seats first, so the CEO is the one an identity prompt names. To keep the unit lead primary, have the outside seat manage the lead (`manages: [backend-lead]`) rather than the unit.

**The dashboard draws exactly this primary line.** In **Company › Structure**, the Org chart's **Reporting view** hangs each seat's card under the manager `Organization.Manager` names, and the Table's **Reports to** column names the same seat, so a member with two managers is drawn once, under the one an identity prompt names; a chart that looks wrong is a `manages` list worth reading. See [Structure is the company running](../reference/dashboard-design.md#structure-is-the-company-running). Both are the org builder and draw the DRAFT on screen from the engine's dry run, so a change to `manages` or a unit's lead can be read before saving, and the builder says so when reordering seats moves a primary manager ([The Org Builder](../guides/org-builder.md#the-reporting-view)).

A lead may **narrow the MCP servers** their teams get, with `mcp_limits` on the units and seats they lead: a server turned off for a team, or a tool held at `approve` (each call waits for a person) or `blocked`. A seat is held to the strictest of its own block, every enclosing unit's at any depth, and the server's own entry, so a lead never loosens what a lead above set, and only an administrator lifts a limit ([Narrowing a server for a team](../guides/tools-and-mcp.md#narrowing-a-server-for-a-team)).

The lead can be a **human seat**: a human manager running an AI team is first-class. Agents escalate to the human with their own Slack/Jira tools (an @-mention), and the human assigns work in the PM tool. See [Humans in the Org Chart](humans-in-the-org.md).

The lead's system prompt includes a **roster** of direct reports. Each member's profile (background, goal, responsibilities, and for a human report its contact identities and availability) renders into the lead's executor prompt from the in-memory `Organization` model.

#### Lead inheritance

A child unit with no `lead` inherits its parent's, through any number of levels: a division lead is the effective lead of every descendant that doesn't specify its own.

```yaml
units:
  - name: "Engineering"
    id: engineering
    type: department
    lead: vp-eng                    # ← set here
    roles:
      - name: "VP Engineering"
        handle: vp-eng
    children:
      - name: "Backend"
        id: backend
        type: team                  # no lead — inherits vp-eng
        roles:
          - name: "Dev A"
          - name: "Dev B"
      - name: "Frontend"
        id: frontend
        type: team
        lead: frontend-lead         # explicit — NOT overwritten
        roles:
          - name: "Frontend Lead"
          - name: "Dev C"
```

- **Backend** inherits `vp-eng`, who auto-manages `dev-a` and `dev-b`.
- **Frontend** has an explicit lead (`frontend-lead`), so the parent's is ignored.

Inherited leads work as explicit ones for auto-management, task routing, `org.Unit.IsLedBy`, and the Jira project-key mapping; the lead role just lives in an ancestor. `org.Organization.EffectiveLead` resolves the lead seat in code.

### Roles at Any Level

Roles sit at the org root or directly in any unit: a department-level role (a VP) beside child teams, an org-wide role (a CEO) at the root:

```yaml
roles:
  - name: "CEO"
    manages: [vp-eng]

units:
  - name: "Engineering"
    id: engineering
    type: department
    roles:
      - name: "VP Engineering"
        handle: vp-eng
        manages: [backend-lead, frontend-lead]
    children:
      - name: "Backend"
        id: backend
        type: team
        lead: backend-lead
        roles: [...]
```

#### A seat's `unit` reference

A seat declared at the **root** can name its unit with `unit:`, **by that unit's key**; this is how the per-entity configuration API adds a seat to a unit. The engine moves it into that unit before anything else is derived, so it inherits the unit's tool credentials and is auto-managed by the unit's lead exactly as a seat written inside the unit. It is still reported, and edited, where it was written.

The reference places a root seat and nothing else. A seat declared **inside** a unit is never moved by one, so a `unit:` on it keying a different unit reads as a placement and does nothing. A document carrying one is refused at that seat's `unit`; repeating the key of the unit the seat is declared in is accepted. This is an [admission rule](#where-the-rules-are-held): refused where submitted, and a revision being applied that carries one still runs, with a warning.
### Dangling references

A `lead`, a root seat's `unit` or a `manages` entry names a seat by handle or a unit by key, and may resolve to nothing: a misspelling, a removed seat, or a seat not added yet. None of these refuses the revision, because live changes build an organization in pieces (a unit can be added before its lead seat, and every node applies each intermediate revision). Every reader treats the reference as absent: a unit with a dangling lead runs with no lead, a seat whose `unit` keys nothing stays at the root, and a `manages` entry naming nothing manages nobody.

**Each node logs every dangling reference once per epoch it applies**, as a warning named `org_dangling_reference`, never for a refused revision. One still logged after the organization is fully wired is a misspelling nothing else reports.

| `ref` | Reported when | `from` | `to` |
|---|---|---|---|
| `lead` | A unit's own `lead` is no seat's handle | The unit | The handle as written |
| `unit` | A root seat's `unit` is no unit's key | The seat | The key as written |
| `manages` | A `manages` entry is neither a seat's handle nor a unit's key | The seat | The entry as written |

Each line also carries `epoch`, `revision` and a `detail` sentence saying what the engine does meanwhile and how to resolve it.

**What was written is reported, once.** A dangling lead is reported on the unit that declares it, not on child units that inherit it. A child that writes the same name itself is reported separately (a second place to correct). A `manages` entry keying a unit with no seats resolves to nobody but is not reported.

**Remove a stale GitLab access level promptly.** An override names a seat by handle, so it dangles only when no seat holds the key, and then grants its level to whichever seat is next given that handle. It is reported whether or not GitLab is enabled, since re-enabling is when the stale grant takes effect.

---

## Onboarding convention

Each unit (and the organisation root) is expected to publish a page titled exactly **`Onboarding`** in its knowledge-base container (its Confluence space, on that backend). On an agent seat's first turn for its current org chain, a dedicated [onboarding pass](turn-engine.md#first-turn-onboarding) runs before the executor, its prompt listing the unit chain (org, ancestor units, own unit) under `## What to do`. The agent reads each `Onboarding` page with its knowledge-base tools (`search_knowledge` on either backend, then `get_page` on Doclet or the Confluence server's own page tools on Confluence), activating them from the tools its prompt lists, captures the conventions that matter via `reflect_and_persist`, and calls `mark_onboarded` when done. The pass does not run again for that org chain. The executor is never told to onboard and never reaches `mark_onboarded`.

Re-onboarding fires automatically when the structure changes (the role moves between units, a new ancestor unit is inserted): the engine recomputes a chain hash and the prior marker no longer matches. A **rename** is not structural: the hash is built from the company name, the keys of the units in the chain and the seat's handle, so relabelling a division or person re-onboards nobody. A different key or handle is a removal plus a creation, and the seat it leaves in the chain onboards as new. Source-page drift is **not** automatic: the agent re-reads at its own discretion, or on a page-update notification routed through the notification pipeline.

This mirrors a real new hire: a founder maintains an `Onboarding` page per scope, with no YAML config pointing at it. See [Agent Learning](agent-learning.md).

---

## Hot Reload

The organization is part of the company configuration, so it changes without a restart. Activating a revision (`PUT /config`, `PATCH /config`, a per-entity write, a revert, or `crewlet config import`) moves the fleet's activation pointer, and each node's reconcile tick applies the revision it names. The apply stages, and what a refused one leaves behind, are in [Live Propagation](configuration.md#live-propagation).

**Who may change it.** An administrator (`admin`) may change any of the organization. A person who **leads** a unit may change the seats and units inside it (their team and every team beneath it): add, edit, move and remove seats and sub-teams there, and edit their own unit's fields. They may not hand their unit to another lead, move it or remove it; reach a seat or team outside it through a `lead:` or `manages:` entry (their own, or another team's naming a seat or team they add); or change a project, space, channel, address or contact id, a credential, a `${VAR}` in any field, or a setting. See [A lead edits their own team](identity-and-access.md#a-lead-edits-their-own-team).

**A running organization is never edited in place.** The apply builds a new `Organization` from the revision, normalizes and validates it, and publishes it in a new epoch with everything else built from the same document; editing the published tree would race with turns reading it. A turn pins the epoch it starts on, so a change reaches a seat at its next turn. A revision whose organization does not validate is refused before its epoch is published, and the node keeps serving the previous one.

**Seats follow the new chart.** After the swap the node creates a mailbox for every added seat, and [seat ownership](seat-ownership.md) converges placement onto the new seat list, releasing a seat the organization no longer has, at the end of the apply rather than on placement's next periodic pass. Everything else derived from the organization (party registry, seats' tool surfaces, tracker projects, knowledge containers, scheduler) follows the same published company; see [What follows a published company](configuration.md#what-follows-a-published-company). A seat is identified by its handle, and its runtime id is derived from the company name and that handle (`org.DeriveAgentID`). A seat keeps its identity and memory through a rename or move while its handle is unchanged. A changed handle is a different seat, and renaming the company gives every agent seat a new id.
