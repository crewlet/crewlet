# Organization Model

The organization model (`internal/org`) is the foundational data structure representing the company hierarchy. It determines how agents communicate, what knowledge they can access, who they report to, and how tasks flow.

**This page is about what you author and what the engine derives from it.** Where
the authored chart is *stored* — as an ordered log with one SQL copy per node,
rather than as a nested object inside the company document — is
[The Org Chart Domain](chart-domain.md). The division is the same one that runs
through this whole page: those tables hold what somebody wrote, and every
derivation below is computed from them on the way out rather than written down
beside them. A derived value written down is a second answer that goes stale the
moment an ancestor moves, with nothing to recompute it — because the change that
moved the ancestor never named the row that went stale.

Three things on this page are derived and therefore stored nowhere:

- **A unit's effective lead**, which is a walk up the tree from the unit that
  declares none. Only the *authored* lead is a row.
- **What a `manages:` entry expands to**, when it names a unit: every seat in
  that unit's subtree, as the subtree stands when the entry is read.
- **Who manages a seat**, which is the other end of an edge only one end of
  which is authored.

### The view is a function of rows and a position

The tree every turn reads is **derived**, and where it is derived *from* is
what the chart's split changed. It used to be a document: parse the YAML,
normalise the tree in place, publish the pointer. It is now a pure function
over the chart's rows — rows in, an immutable value out, carrying **the
position those rows were read at**.

Two properties come with that, and neither was available before:

- **Two nodes at one position produce the identical view.** That is the whole
  claim the replicated estate rests on, and a function can be tested for it
  where a mutation could only be inspected.
- **A view can say what it is true of.** A company derived from a document is
  true of that document; one derived from a log is true as of a position, and
  a screen that renders a chart can now say which.

**It produces the same tree the document path does** — deliberately, and it is
checked: the row derivation is compared against the document derivation over
every example and fixture this repository ships, on the derivations that
matter (the tree's shape, each unit's effective lead and channel, each seat's
placement, every expanded `manages` list). The document side is normalised
**twice**, because normalising in place is idempotent only by discipline and a
comparison against one pass would certify less than the contract.

**A cycle in the rows is broken and reported.** The write path refuses one —
the batch validator replays every move and walks up from the new parent — but
rows can predate that rule or be repaired by hand, and a build that recursed
into one would not produce a wrong answer, it would not terminate. So a cyclic
unit is re-parented to the root, ordered by key so every node breaks it the
same way, and the units that moved are named.

### What a structural change is, and what it refuses

A change to the *shape* of the company — a hire, a move, a promotion, a team
dissolved — is a **batch**: an ordered list of operations that lands as one
arbitrated record, so exactly one batch at a time can change the chart. It is
refused **whole**, naming the **first** operation that failed and the rule it
broke, because a batch that applied what it could and skipped the rest would
produce half a reorganisation with no record of which half — and which half
would depend on the order you happened to write them in.

The operations are `create_unit`, `create_seat`, `move`, `set_lead` and
`remove`. Each is checked against the state the ones **before it** produced,
which is the only reading under which the ordinary ways of editing a chart
work:

- **A parent an earlier operation created is a parent.** Building a department,
  then a team inside it, then a seat inside that is one gesture and must be one
  batch.
- **A unit emptied by one operation can be removed by the next.** "Move
  everybody out, then dissolve the team" is likewise one gesture.

And it is the only reading under which the dangerous case is caught:

- **A cycle the batch's own moves close is refused.** Moving Engineering under
  Platform is fine while Platform is at the root; moving Platform under
  Engineering is fine while Engineering is at the root. Together they put each
  under the other, and no per-object check would ever have shown either writer
  the other's move. This is why the whole structure arbitrates on one subject.

The rest of the rules:

| Refused | Why |
|---|---|
| A create onto a key something already holds | Two objects on one address |
| A create onto a key a removal **retired** | A removed address never resolves again — its history, its references and the tombstone that stops its old records applying are all keyed on it. Its own rule, because the remedy differs: a taken key needs a different name, a removed one can never be used at all |
| A placement under a unit nothing creates | A reference to a parent that is not there, and will not be |
| Removing a unit that still holds children or seats | An orphaned subtree is reachable from nothing and removable by nothing |
| A reserved key (`root`, `tree`, `barrier`) | Each already means something: the org root, and two of this log's own subject kinds |
| More than 500 operations | One batch is one record, and a record past the broker's maximum payload is refused **permanently** with no retry that can place it. Submit several batches; each is arbitrated on its own |

A **removal is its own record** and cannot ride with a placement, because a
removal installs a gate and that has to be answerable without reading the
record's contents. A batch that does both is refused, telling you to publish
the placements first.

---

## Flexible Hierarchy

The org structure uses a recursive unit model (`org.Unit`) that can nest to any depth. This lets founders design their org however they want: flat teams, departments with sub-teams, divisions, squads, pods, or any custom structure.

The runtime tree the engine builds from a revision (`org.Organization`, with the Go type of each field):

```
Organization
├── Name, Mission, Vision string; Policies []string
├── TokenBudget int                        (org-wide ceiling; 0 = unlimited)
├── KnowledgeScope []string                (knowledge.scope: the one org-wide
│                                           knowledge read scope)
├── Roles []*Role                          (root-level org-wide seats)
└── Units []*Unit
    ├── Name string                         (DISPLAY only: nothing references a
    │                                        unit by it)
    ├── ID string                           (`id`: the unit's KEY — what a
    │                                        `manages:` entry and a root seat's
    │                                        `unit:` resolve. Minted from the name
    │                                        at import when unset)
    ├── Type UnitType; Purpose string; Goals []string
    ├── Lead string                         (the HANDLE of the seat leading it)
    ├── KnowledgeRefs []string
    ├── Channel string                     (team channel on the company's chat
    │                                       surface, inherited by children)
    ├── Project string                     (tracker identity: lead-fallback routing
    │                                       and the project the team files under.
    │                                       VENDOR-NEUTRAL: it names a native project
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
├── Contact *HumanContact              (human seats: slack_user_id,
│                                       mattermost_user_id, atlassian_account_id,
│                                       github_login, gitlab_username,
│                                       crewlet_operator_id)
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
├── TokenBudget int                    (0 = unlimited)
├── LLM, LLMReview, LLMSubagent,
│   LLMAuxiliary, LLMJudge,
│   LLMSandbox ProviderKeys            (the executor's chain and the per-phase
│                                       satellites; see Turn Engine)
├── Workers []string                   (the worker templates this seat may use)
├── LearningEnabled Toggle             (per-seat override for agent learning)
├── Sandbox *RoleSandbox               (role.sandbox: the code-sandbox gate)
├── Placement                          (role.placement: which nodes may run it)
├── Slack SlackIdentity                (role.integrations.slack: this seat's OWN
│                                       Slack app, bot_token and signing_secret)
├── Mattermost MattermostIdentity      (role.integrations.mattermost: its bot)
└── Schedules []Schedule               (role-scoped recurring work; see
                                        [Scheduling](scheduling.md))
```

Roles can live in two places:

- **Inside a unit** (`units[].roles`): scoped to that unit for MCP env inheritance and lead auto-management. The unit's `project` gives the team its tracker "home" (routing + write target), but does not scope what the role can *read*.
- **At the root level** (`roles`) — org-wide agents that don't belong to any specific team. They participate in the `manages[]` hierarchy like any other role and are fully visible to task routing; a root-level role can carry its own `project` identity. Knowledge **read** scope for every agent is the org-wide `org.Organization.KnowledgeScope` only.

> Every one of these identities is consulted. The tracker routes an item that names nobody to the lead of the unit that owns the project, and the knowledge base does the same for a page change nobody was mentioned in — whichever backend serves each, which is why the keys name neither. Neither narrows what an agent can READ: knowledge scope is the org-wide `knowledge.scope` only, because letting a unit's identity double as a read scope is how an agent ends up unable to read the page it was told to follow. See [Jira](../integrations/jira.md) and [Confluence](../integrations/confluence.md).

---

## Common Org Patterns

### Root-Level Roles (CEO/CTO above all teams)

Org-wide leaders can be defined at the root, outside any unit. They manage unit leads via `manages[]` and participate in task routing like any other role.

The root is also where the **founder** belongs — as a [human seat](humans-in-the-org.md#the-founder-seat) above the top agent, so escalations terminate at a person and agents recognise the founder's activity on Slack/Jira/GitHub:

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

The `type` field on a unit can be any string. These well-known types are provided for convenience:

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

Each Role defines a unique **seat** with its own backstory, skills, personality, and domain expertise. A seat is held by an AI agent (`kind: agent`, the default) or a **human teammate** (`kind: human`). Each agent seat is one agent, identified by an id derived from the company name and the handle it was created under; human seats participate in the same hierarchy (manages, unit lead, rosters, escalation) but are addressable-only: no runtime, no inbox, no LLM. The founder defines each seat individually, and seats are not interchangeable. See [Humans in the Org Chart](humans-in-the-org.md).

### Handle-Based Identity

Every agent gets a deterministic **handle** slug derived from its role name:

```
Role Name           Handle
─────────────────   ────────────────
Sarah Chen          sarah-chen
Marcus Rivera       marcus-rivera
Alex Kim            alex-kim
```

Handles are the canonical identity **inside the document as well as outside it**: a unit's `lead:` and every `manages:` entry name a seat by its handle, and so do notification routing and external system mappings (e.g. a Jira assignee, a GitLab service account). A name is prose a founder edits; an identity is not, so nothing references a seat by its `name`. A seat's `email` is matched too — inbound Jira and GitHub payloads identify people by address, and a plus-addressed form (`notif+sarah-chen@co.com`) resolves back to the handle. You can set a custom handle:

```yaml
roles:
  - name: Senior Engineer
    handle: sr-eng        # Override auto-derived "senior-engineer"
```

**Removing a seat, and adding one back.** Because identity is the handle, what a removed agent seat leaves behind is keyed by it too. Its **mailbox**, and the mail still addressed to it, is kept for 24 hours after the seat leaves the active revision and then retired, so a seat restored within a day finds its backlog and a seat added under the same handle later starts with an empty mailbox. Its **coding runs** are kept for the same 24 hours and ended when the mailbox is retired, each one announced as lost. Its **memory** (diary, episodes, counterparty profiles, onboarding markers) is kept, and because it is keyed by the handle or by the agent id derived from it, a seat added again under the same handle reattaches to it. Renaming a seat's handle is a removal of the old handle and an addition of the new one. See [Seat Ownership § The removed seat](seat-ownership.md#the-removed-seat).

### Names and handles are unique

Three identities must each name exactly one thing in the whole company:

| Identity | Unique across | Why |
|---|---|---|
| Seat **handle** | Every seat, agent and human | It names the seat's inbox, its derived agent id, its external accounts — and it is what a unit's `lead` and every `manages` entry resolve. Two seats on one handle share an inbox, or an agent absorbs a person's activity. |
| Seat **name** | Every seat, at any depth | The name is display, and a colleague named in prose is resolved by it: a model reaching a teammate types the name it remembers, and an exact role-name match answers with one seat or an honest list. Two seats of one name are permanently that list, on every ask and every roster row. |
| Unit **key** (`id`, or the name where none is declared) | Every unit in the tree, not only siblings | A `manages` entry naming a unit and a root seat's `unit:` reference search the whole tree and resolve to the first unit answering to the key. Two units on one key read as distinct on every screen while each reference reaches only one of them. |

Seat names are compared as the exact string, the way an exact role-name match is made: `Dev` and `dev` are two different names. Unit keys are compared **folded**, ids and names alike, because a name is prose and a reader who cannot tell two teams apart files one team's work under the other. A seat or unit with no name (or a name that derives no handle) is refused by its own rule and is never reported as a duplicate of another. A refusal names every entity that shares the key, in one message per key, and where each one sits:

```
duplicate unit key "Platform": 2 units answer to it (under unit "Engineering"; under unit "Product"). ...
```

#### Companies stored before the name rules

Handle uniqueness has always been enforced. Seat name, seat `id` and unit key uniqueness are **admission rules**: they were added after companies existed, and a company that breaks one still runs exactly as it did before. The same holds for a [`unit:` reference on a seat declared inside another unit](#a-seats-unit-reference). So they are enforced where a document is *submitted* and reported where a stored one is *applied*:

- **Refused on every write.** `PUT /config`, `PATCH /config`, a per-entity write, a `/setup` submission that changes the document, `crewlet config import`, `crewlet validate` and a company file `crewlet run` imports as a new revision (`-company` into an empty store, `-import-company` over a different company) all refuse a document with a duplicate seat name, seat id or unit key, including a write to a company whose stored revision already carries one and a write that does not touch the duplicates. The write that corrects them is accepted.
- **Applied with a warning.** A stored revision carrying a duplicate name (written by an earlier build, or activated by an older peer during a rolling upgrade) is applied by every node, a node boots on it (including one started with `-company` or `-import-company` naming a file that is that revision, or a `-company` file the store's own company outranks), and `POST /config/reload`, a `/setup` credential rotation (which reloads) and a revert to it still work. The vendor commands that act on a company file without storing it (`crewlet gitlab provision`, `crewlet slack provision` and their siblings, `crewlet llm status`) read such a file too. Each node logs `org_admission_warning` once for every violation when it applies the epoch, naming the revision and the entities, with the document path of each under `paths`.
- **Always readable.** `GET /config`, the revision reads, diffs, `crewlet config show` and `crewlet config export` serve the stored document as it is, so the duplicates can be seen and corrected.

### Management Hierarchy

Hierarchy is encoded through `manages` relationships on roles. A Team Lead manages Engineers; a VP manages Team Leads. See [Agent Runtime](agent-runtime.md) for how the hierarchy drives agent execution.

- **Permissions** flow from hierarchy — a manager can assign tasks to reports, knowledge access is scoped, and the agent's identity prompt names the manager so handoffs (a Slack mention, a Jira comment, or `a2a_ask` during Execute) reach the right person
- **Task assignment** is the unit lead's responsibility — the lead agent reasons about its members and assigns tasks

#### Managing by unit key

The `manages` list accepts both **seat handles** and **unit keys**. When an entry matches a unit's key (and does not match any seat's handle), it is expanded to all roles contained in that unit, including roles in descendant child units. This avoids listing every agent individually when a role manages an entire team or department.

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

After expansion the CEO manages: `tech-lead`, `dev-a`, `dev-b`, `pm`, `designer` — handles, because that is what a manages list holds once it is normalized.

You can mix handles and unit keys freely:

```yaml
manages: [cto, backend]   # cto is a seat handle, backend is a unit key
```

If one token is both a seat's handle and a unit's key, the **seat takes priority** (no expansion happens for that entry). A unit key expands to every seat in that unit's subtree **except the seat that lists it**: a lead that manages its own team by key does not manage itself. A token matching neither is kept as written, so a seat that has not been added yet can already be named, and the engine reports it as a [dangling reference](#dangling-references).

### Unit Lead

A unit may designate a lead via the `lead` field. The lead is responsible for:

- Task routing and assignment within the unit
- Acting as the single point of contact for the unit
- Reasoning about members' profiles (background, goal, responsibilities) to assign tasks to the right individual

When a unit has direct roles and a lead is set, the lead **auto-manages** every direct member that no direct member of the same unit already manages. Three rules decide what counts as already managed, and all three read each `manages` entry the way [unit-key expansion](#managing-by-unit-key) resolves it, so a unit key counts for every seat it reaches:

- **A member another direct member manages keeps that manager.** A tech lead who lists `dev-a`, or who lists the unit key `backend` that `dev-a` sits in, shields `dev-a` from the unit lead.
- **A member the lead already manages is not listed twice.**
- **A member that manages the lead is never claimed.** An engineering manager who manages their own unit by key reaches the unit lead too, and claiming them back would make a two-seat management cycle.

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

Here VP Engineering auto-manages only `tech-lead`. `dev-a` and `dev-b` report to Tech Lead alone.

**Only the unit's own direct members shield.** A seat outside the unit that manages it, such as a root-level CEO with `manages: [backend]`, lists every seat in `Backend` but does not stop `Backend`'s lead from auto-managing those seats as well. That scope is deliberate: management is stored on the manager, so a CEO managing a whole division by key would otherwise leave every lead inside it with an empty roster. The consequence is that such a member has **two managers**. `org.Organization.Manager` reports the first seat in walk order that lists it, and root-level seats are walked first, so the CEO is the one an identity prompt names. To keep the unit lead as the primary manager, have the outside seat manage the lead (`manages: [backend-lead]`) rather than the unit.

The lead can be a **human seat** — a human manager running an AI team is a first-class pattern: agents escalate to the human with their own Slack/Jira tools (an @-mention), and the human assigns work in the PM tool. See [Humans in the Org Chart](humans-in-the-org.md).

The lead's system prompt includes a **roster** of direct reports. Each member's profile (background, goal, responsibilities, and for a human report its contact identities and availability) renders directly into the lead's executor prompt from the in-memory `Organization` model.

#### Lead inheritance

When a child unit has no `lead` set, it automatically inherits the lead from its parent unit. This cascades through any number of levels — a division lead becomes the effective lead for every descendant that doesn't specify its own.

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

In this example:

- **Backend** has no lead, so it inherits `vp-eng`. VP Engineering auto-manages `dev-a` and `dev-b`.
- **Frontend** has an explicit lead (`frontend-lead`), so the parent's lead is ignored.

Inherited leads work the same as explicit leads for auto-management, task routing, `org.Organization.IsUnitLead`, and the Jira project-key mapping. The only difference is that the lead role lives in an ancestor unit rather than the current one. Use `org.Organization.EffectiveLead` to resolve the lead seat in code.

### Roles at Any Level

Roles can be placed at the org root or directly in any unit. A department-level role (like a VP) can sit alongside child teams, and org-wide roles (like a CEO) can sit at the root:

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

A seat declared at the **root** can name the unit it belongs to with `unit:`, **by that unit's key**, which is how the per-entity configuration API adds a seat to a unit. The engine moves such a seat into that unit before anything else is derived, so it inherits the unit's tool credentials and is auto-managed by the unit's lead exactly as a seat written inside the unit is. A seat moved this way is still reported, and edited, where it was written.

The reference places a root seat and nothing else. A seat declared **inside** a unit is never moved by one, so a `unit:` on it that keys a different unit reads as a placement and does nothing: the seat stays where it is written while the document says it belongs elsewhere. A document carrying one is refused at that seat's `unit`; repeating the key of the unit the seat is declared in is accepted. This is an [admission rule](#companies-stored-before-the-name-rules): a stored company that already carries such a reference still runs exactly as it did.

### Dangling references

A `lead`, a root seat's `unit`, or a `manages` entry names another entity — a seat by its handle, a unit by its key — and that reference may resolve to nothing: a misspelling, a seat that was removed, or a seat that has not been added yet. None of these refuses the revision. Live configuration changes build an organization in pieces (a unit can be added before the seat that leads it, and every node applies each intermediate revision), so refusing a partly wired organization would make that sequence impossible. Every reader treats the reference as absent instead: a unit whose lead is dangling runs with no lead, a seat whose `unit` keys nothing stays at the root, and a `manages` entry naming nothing manages nobody.

The engine reports them rather than letting them pass silently. **Each node logs every dangling reference once for each epoch it applies**, as a warning named `org_dangling_reference`, and never for a revision it refused. A reference that is still logged after the organization is fully wired is a misspelling nothing else will report.

| `ref` | Reported when | `from` | `to` |
|---|---|---|---|
| `lead` | A unit's own `lead` is no seat's handle | The unit | The handle as written |
| `unit` | A root seat's `unit` is no unit's key | The seat | The key as written |
| `manages` | A `manages` entry is neither a seat's handle nor a unit's key | The seat | The entry as written |
| `gitlab_access_level` | A key of `integrations.gitlab.provisioning.access_levels` is no seat's handle | `integrations.gitlab.provisioning.access_levels` | The handle |

Each line also carries `epoch`, `revision` and a `detail` sentence saying what the engine does meanwhile and how to resolve it.

**What was written is reported, once.** A dangling lead is reported on the unit that declares it, never on the child units that inherit it: they wrote nothing, and there is nothing to fix on them. A child unit that writes the same name itself is reported separately, because it is a second place to correct. A `manages` entry keying a unit that holds no seats resolves to nobody but is not a misspelling, so it is not reported.

**A stale GitLab access level is worth removing promptly.** Access level overrides are looked up by handle when a seat's service account is provisioned, so the entry left behind by a removed seat grants its level to the next seat that derives the same handle. It is reported whether or not GitLab is currently enabled, since re-enabling it is exactly when the stale grant would take effect.

---

## Onboarding convention

Each unit (and the organisation root) is expected to publish a page titled exactly **`Onboarding`** in its container of the knowledge base, its Confluence space. On an agent seat's first turn for its current org chain, a dedicated onboarding pass runs before the executor, with a short `## First-turn onboarding` block listing the unit chain (org → ancestor units → own unit). The agent reads each `Onboarding` page using its knowledge backend's page-search and page-read MCP tools (`confluence_search` / `confluence_get_page`), captures the conventions that matter via `reflect_and_persist`, and calls `mark_onboarded` when done. After that, the hint disappears from subsequent prompts.

Re-onboarding fires automatically when the org structure changes (the role moves between units, a new ancestor unit is inserted) — the engine recomputes a chain hash and the prior marker no longer matches. A **rename** is not a structural change: the hash is built from the company name and the chain of origin identities, so relabelling a division or a person moves nobody and re-onboards nobody.  Source-page content drift is **not** automatic: the agent re-reads at its own discretion, or in response to a page-update notification routed through the existing notification pipeline.

This mirrors how a real new hire learns.  A founder doesn't need YAML config for which onboarding doc to point at — they just maintain an `Onboarding` page per scope, the same way they would for human team members.  See [Agent Learning](agent-learning.md) for the full design.

---

## Hot Reload

The organization is part of the company configuration, so it changes without a restart. Activating a revision (`PUT /config`, `PATCH /config`, a per-entity write, a revert, or `crewlet config import`) moves the fleet's activation pointer, and each node's reconcile tick applies the revision it names. The stages of that apply, and what a refused one leaves behind, are in [Live Propagation](configuration.md#live-propagation).

**A running organization is never edited in place.** The apply builds a new `Organization` from the revision, normalizes and validates it, and publishes it as part of a new epoch together with everything else built from the same document. Turns on many goroutines read the published tree at once, so editing it would be a data race with no owner. A turn pins the epoch it starts on and reads only that epoch until it ends, so an organization change reaches a seat at its next turn. A revision whose organization does not validate is refused before its epoch is published, and the node keeps serving the previous one.

**Seats follow the new chart.** After the swap the node creates a mailbox for every seat the revision adds, and [seat ownership](seat-ownership.md) converges placement onto the new seat list, releasing a seat the organization no longer has.

A seat is **addressed** by its handle and **identified** by its id, and the two are deliberately different. The handle is what a document references and a screen shows; the id is a UUIDv5 over the company name and the handle the seat was *created* under (`org.DeriveAgentID`, applied by `org.Organization.AgentIDFor`), recorded on the chart row as its origin. So a seat keeps its identity through a rename and through a move: its mailbox, its seat lease, its memory changelog and its schedule history are all named by the id, and the handle it used to answer to goes on resolving, so a `manages:` entry or a `lead:` naming the old one still reaches them — and so does everything else that resolves a name somebody wrote: an agent's `lookup_colleague`, a chat or page mention, a Datadog monitor tagged with the old handle, a tracker field naming a person. A retired handle is ranked below every live match and above every approximate one, so a seat that has taken that name since always wins it, and a name written down exactly never loses to a substring of somebody else's. Its accounts at Mattermost, GitLab, Datadog and Atlassian keep their names too — those are named after the origin handle directly, since no third-party app stores a Crewlet id. See [Renaming a seat](integration-reconcile.md#renaming-a-seat).

Renaming the **company** is the one edit that does move every agent seat's id, because the company name is the other half of the derivation.
