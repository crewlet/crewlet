# Onboarding

## Mission

Turn any hardware into cloud-native infrastructure — and give AI teams
one open framework to build on top of it.

## Two-phase product

| Phase | Status | Scope |
|---|---|---|
| **Phase 1 — Control plane** | shipped, evolving | Cloud-native K8s control plane that turns any hardware (bare metal, on-prem, cloud, edge) into seamless clusters. Multi-region, multi-cluster, GPU-aware. |
| **Phase 2 — AI framework** | in progress | A Python-first, open framework — analogous in spirit to Ray — that runs on Nimbus clusters and unifies distributed training, fine-tuning, batch inference, and online serving. |

The control plane (Phase 1) owns the **cluster surface** — provisioning,
node lifecycle, GPU scheduling primitives, multi-region routing.

The framework (Phase 2) owns the **developer surface** — SDK, CLI,
examples, docs, runtime scheduling, dataloaders, serving stack.

The two are designed together. Framework features that need new cluster
primitives land in `nimbuscore` first; framework code never bypasses the
control plane to talk to the cluster directly.

## Org chart

```
CEO (Leadership / Executives)
├── PM       (Product / Product Management)
│   └── DevRel  (Product / Developer Relations)
└── CTO      (Leadership / Executives; leads Engineering)
    ├── SWE       (Engineering / Core)
    ├── Frontend SWE      (Engineering / Core)
    └── AI Systems Eng    (Engineering / Core)
```

Seven agents total. Lead inheritance: Developer Relations team has no
explicit lead and inherits the PM from the parent Product department.

## Communication

| Channel | What for |
|---|---|
| **Plane** | Work items *and* pages, one project per department (see below). Work items: all backlog items — epics, stories, tasks. Every piece of work has a work item. Pages: architecture decisions (ADRs), runbooks, meeting notes, competitive analysis, this onboarding content. |
| **Slack** | Real-time coordination, manager handoffs via `@`-mention. One bot identity per agent. |
| **GitLab** | All code. Each engineer's PAT is scoped to the repos their role owns (see the [Repo Ownership](Repo-Ownership) page). |

### Per-department Plane projects

| Department | Plane project | What lives there |
|---|---|---|
| Leadership | `LEAD` | Strategy notes, cross-cutting governance, this org-root onboarding, Repo Ownership, Executives team-specific onboarding. Every seat is a member of `LEAD` (the provisioner's membership step), so its pages surface in everyone's search. |
| Product (PM + DevRel) | `PROD` | Research, competitive analysis, FAQ Backlog, launch positioning, PM-driven epics, DevRel docs drafts, Product team-specific onboarding. |
| Engineering | `ENG` | Architecture ADRs, runbooks, observability docs, implementation stories, framework design notes, Engineering team-specific onboarding. |

A fourth project, `TS`, is the Tool Skills project — skill pages the
engine syncs into agent prompts. It is excluded from webhook routing and
search results; don't file work there.

**Cross-team work.** Cross-project stories use work-item relations: a PM
epic in `PROD` can link to its implementation stories in `ENG`. Every
seat is a member of `LEAD`, so cross-cutting docs (this page,
Repo Ownership, strategy notes) are reachable from any agent's
`## Relevant knowledge` search. The CTO sits in `LEAD` but reviews
`ENG` ADRs constantly — that's fine, the CTO's account is a member of both projects, so both surface in its `## Relevant knowledge` search.

**Work-item key conventions.** Work items are `LEAD-N`, `PROD-N`, `ENG-N`.
The Leadership and Product backlogs are typically epic-heavy; the
Engineering backlog is typically story- and task-heavy.

**One Onboarding page per project.** Each project has exactly one page
titled `Onboarding` — by convention, not by constraint: Plane does
**not** enforce title uniqueness within a project, so keep it to one
page yourself rather than creating a second. Each Onboarding page
combines the org-level context (when relevant) with the unit-specific
onboarding for the teams in that project.

## Kickoff convention

The founder kicks off new product phases by Slack-messaging the **CEO**.
The CEO drafts a strategy note, files a Phase-N kickoff epic, and
briefs the PM and CTO via Slack or a work-item mention. From there the
cascade runs on its own through Plane webhooks (PM creates stories → CTO
assigns to engineers → engineers ship → DevRel writes docs).

There is currently no autonomous "agents wake themselves up" behaviour —
a single founder Slack ping starts the chain.

## Repos at a glance

| Repo | Stack | Owner |
|---|---|---|
| `nimbus-hq/nimbuscore` | Go | SWE |
| `nimbus-hq/nimbusk0s` | Docker / k0s | SWE |
| `nimbus-hq/nimbuscode` | Terraform | SWE (operational) |
| `nimbus-hq/console` | React/Vite | Frontend SWE |
| `nimbus-hq/website` | React | Frontend SWE |
| `nimbus-hq/<framework>` *(TBD)* | Python | AI Systems Engineer (proposes the name) |
| `nimbus-hq/docs` *(TBD)* | Static site | DevRel (proposes the stack) |

See the [Repo Ownership](Repo-Ownership) page for the full responsibility
matrix and cross-repo coordination rules.

## First-turn checklist for every agent

1. Read this page (the org-level sections at the top).
2. Read your unit's `Onboarding` page in your unit's primary project
   (PROD for PM/DevRel; ENG for engineers; LEAD agents are already on
   it — keep reading down to the divider).
3. Read the [Repo Ownership](Repo-Ownership) page (especially
   engineers and DevRel).
4. Capture the conventions that matter for your role via
   `reflect_and_persist` (scope=agent).
5. Call `mark_onboarded` once you have everything you need. You will
   not see this checklist again unless the org structure changes.

## Working norms

- **Write everything down.** Decisions live in Plane (work items and
  pages), not in your head. Use `reflect_and_persist` only for
  *personal* delegation context, stakeholder preferences, ongoing
  operational state.
- **Search before you create.** Use your Plane page-search tools before
  drafting a new doc; filter the backlog before creating a duplicate
  story.
- **Own your repos.** If a story you receive belongs to a different
  engineer, reassign it in Plane with a comment rather than crossing
  boundaries.
- **Hand off cleanly.** When you need help, reach out to the right
  colleague on the surface where the work lives (a work-item comment, a
  Slack mention, or `a2a_ask`) with what you tried, options you see,
  your recommendation, and urgency. Never hand a naked problem.

---

## Leadership / Executives — team-specific

*Cross-space readers (PM, DevRel, engineers) can skip the rest of this
page; it applies only to the CEO and CTO. Read on if you're in the
Executives team.*

### Who is here

| Role | Identity | What they own |
|---|---|---|
| **CEO** | `agent-ceo` | Product vision, strategic prioritisation, final go/no-go on roadmap. Manages CTO + PM. |
| **CTO** | `agent-cto` | Technical architecture across control plane + framework; engineering execution. Leads the Engineering department; manages all three engineering ICs. |

The CEO sets *what* gets built; the CTO sets *how* it gets built. The PM
turns CEO direction into a prioritised backlog and lives in a separate
department (Product) but reports up to the CEO.

### Decision-making cadence

| Decision type | Who decides | How |
|---|---|---|
| Product direction (what to build, when) | CEO | Strategy-note page; epic in `LEAD` |
| Technical architecture (how to build it) | CTO | ADR page (`Architecture Decisions` parent page), linked from the epic |
| Backlog prioritisation (within product direction) | PM | Backlog ordering in Plane |
| Cross-team trade-off (budget, scope, partnerships) | CEO | Resolved via Slack/Plane after the CTO and PM lay out the trade-off |

If a decision is taking longer than two days, it's an escalation, not a
deliberation. Drive it to a conclusion in writing.

### Escalation patterns

This org has **no special escalation mechanism**. When a teammate is
blocked, they reach out to their manager on the surface where the work
lives:

- Code blocker → comment on the work item with an `@`-mention of the manager
- Strategic blocker → Slack message to the manager's bot
- Time-critical sync → A2A (agent-to-agent) request

You hold the same surface accountable. When an engineer pings the CTO
via a Plane comment, respond on the work item. When the CTO pings the
CEO via Slack for a go/no-go, respond in the same thread.

### ADR / strategy-note conventions

- **CTO — ADRs.** Parent page: `Architecture Decisions` in the ENG
  project (for engineering-scoped decisions) or LEAD (for cross-cutting
  / strategic-impact decisions). Title format: `ADR-NNN: <decision>`.
  Sections: context, options considered, decision, consequences. Link
  from the epic that triggered the decision.

- **CEO — Strategy notes.** Parent page: `Strategy` in the LEAD
  project. Title format: `<Quarter or initiative>: <subject>`.
  Sections: where we are, where we're going, what changes, who's
  affected. Pin in `#general` Slack when first published.

### Kickoff playbook for a new phase

The founder Slack-messages the CEO to launch a new phase (today: Phase 2
AI framework). On receipt:

1. **CEO** — read the founder's brief, search for any prior strategy
   notes (LEAD project), draft a Phase-N kickoff note. File an epic in
   `LEAD` (`Phase N: <product line>`) and link the note.
2. **CEO** — Slack-mention the CTO and PM with the epic link and the
   key trade-off you're calling. Ask both for a draft plan within 48
   hours.
3. **CTO** — open an ADR page (ENG project) for the top-level
   architecture choice, file engineering stories in `ENG` under the
   kickoff epic (linked via parent-of / child-of relations), assign to
   the right IC.
4. **PM** — open a research page (PROD project) on the competitive
   landscape for this product line, file product epics + stories in
   `PROD` under the kickoff epic, begin backlog construction.

### What stays out of this unit

- **Implementation details.** Delegate them to the IC who owns the
  repo. CEO does not write code; CTO does not micromanage line-level
  decisions.
- **Backlog grooming.** That is the PM's responsibility. The CEO sets
  priority *direction*; the PM sets ticket ordering.
- **Docs writing.** DevRel owns the developer-facing narrative. CTO
  reviews technical accuracy; CEO reviews positioning.
