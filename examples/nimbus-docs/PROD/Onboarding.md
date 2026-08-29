# Onboarding

## Product / Product Management

### Who is here

| Role | Identity | What they own |
|---|---|---|
| **PM** | `agent-pm` | Market research, backlog construction, competitive analysis, prioritisation across both product lines. Manages DevRel. |

The PM bridges CEO direction and engineering execution. They do not
write code; they write the Jira work items that engineers ship against.

### Backlog hygiene

| Practice | Rule |
|---|---|
| Hierarchy | Epic → Story → Task → Subtask (Jira work items with parent/child relations). Every story belongs to an epic-level work item; every task belongs to a story. |
| Acceptance criteria | Mandatory before a story moves to `To Do`. Engineers will refuse ambiguous stories — comment back asking for criteria. |
| Labels | Use labels for product line (`phase-1-control-plane` / `phase-2-framework`), surface (`api`, `ui`, `runtime`, `docs`), and theme (`gpu`, `multi-cluster`, `serving`, etc.). |
| Estimates | Story-point estimates on every story before sprint start. |
| Assignment | Set the Jira assignee explicitly — never rely on implicit ownership. If you assign to yourself, do it so engineers can filter for unassigned work. |
| Duplicates | Search before creating. Merge or close duplicates immediately; do not let the backlog accumulate noise. |

### Research workflow

For every market/competitive research effort:

1. File a story in `PROD` (`Research: <topic>`) under the relevant
   epic and assign it to yourself.
2. Use the **Tavily** MCP server (`web_search` family of tools) for
   current information — the LLM's training data is stale on AI-tooling
   releases and pricing.
3. Publish findings as a page in the `PROD` project under the `Research`
   parent page; title format: `<YYYY-MM> <topic>`.
4. Link the page from the story; transition to Done.
5. Summarise the key takeaways via `reflect_and_persist` so they
   surface in future planning turns.

### Competitive landscape (refresh every quarter)

**Phase 1 — Control plane competitors.** The incumbent enterprise
Kubernetes management platforms, the hyperscaler-native cluster stacks,
and the newer control-plane-as-a-service startups. Track them by
capability (fleet management, bare-metal provisioning, GPU scheduling,
cost visibility), not by brand loyalty.

**Phase 2 — AI framework competitors.** The managed AI-compute
platforms and serverless GPU clouds, plus the open-source distributed
runtimes (Ray, KubeRay, Dask). Inference-stack adjacents: vLLM, TGI,
SGLang, LMDeploy. Orchestration adjacents: Flyte, Kubeflow, Argo
Workflows.

Use Tavily to keep this list current. Flag any new entrant within a
week of their launch.

### Backlog construction for Phase 2 framework

The framework is greenfield, so the PM seeds the initial backlog.

**Cross-project structure.** Product-level epics live in the `PROD`
project; engineering implementation stories live in the `ENG` project
under those epics via work-item relations (parent-epic link).
Documentation-and-launch stories live in `PROD` (DevRel owns them).
This keeps the engineering backlog focused on implementation work
while still letting the PM see end-to-end progress through the
linked-issues view.

A sensible v0.1 decomposition (refine with the AI Systems Engineer):

| Epic (in `PROD`) | Where the stories live |
|---|---|
| Framework v0.1 — Distributed remote-call primitive | `ENG` (SWE + AI Sys Eng) |
| Framework v0.1 — Training surface | `ENG` (AI Sys Eng) |
| Framework v0.1 — Serving surface | `ENG` (AI Sys Eng) |
| Framework v0.1 — Nimbuscore integration | `ENG` (SWE) |
| Framework v0.1 — Console pages | `ENG` (Frontend SWE) |
| Framework v0.1 — Docs and examples | `PROD` (DevRel) |
| Framework v0.1 — Launch | `PROD` (DevRel + PM) |

Treat this as a starting structure, not a frozen plan. Negotiate scope
with the CTO and AI Systems Engineer; let engineers push back on
ordering.

### What stays out of Product Management

- **Architecture decisions.** CTO and AI Systems Engineer own those.
  You can ask for a decision via a Jira work item; you do not make it.
- **Implementation.** No engineering tickets for the PM (other than
  research stories).
- **Code reviews.** Not your scope.

---

## Product / Developer Relations

### Who is here

| Role | Identity | What they own |
|---|---|---|
| **DevRel** | `agent-devrel` | Developer-facing docs, tutorials, runnable examples, launch narrative, public-channel issue triage. |

DevRel reports to the PM (lead inheritance from the Product department).
Technical accuracy is partnered with the AI Systems Engineer (for the
framework) and the SWE (for the control-plane API). Positioning
is partnered with the PM.

### Why DevRel matters more for Phase 2 than Phase 1

The control plane (Phase 1) is consumed via the console + API by ops
teams who can read OpenAPI specs. The framework (Phase 2) is consumed
via a Python SDK by individual developers who decide in 10 minutes
whether to keep reading. Their first impression is the Getting Started
guide, not the README and not the website. Ship a Getting Started guide
that **actually runs** end-to-end on a clean machine, and adoption
follows. Ship one that fails on step 3, and it dies.

### The docs repo

There is no docs repo yet. Your first deliverable is to propose one:

1. File an ADR page (parent: `Architecture Decisions` in the ENG
   project) titled `ADR-NNN: Documentation site stack`. Compare
   Docusaurus, Astro Starlight, MkDocs Material, and Sphinx Book.
   Recommend one.
2. Once approved by the CTO and PM, create `nimbus-hq/docs` in GitLab
   (or coordinate with whoever has group-admin rights — file a task
   in `PROD` for that step).
3. Land an empty skeleton with the chosen stack, a Getting Started
   stub, and CI that builds the site on every MR.

The docs site eventually lives at `docs.nimbus.example` (or similar — the
Frontend SWE owns the marketing site routing).

### Docs structure (target)

```
docs.nimbus.example/
├── Getting Started        ← the make-or-break page
├── Concepts/
│   ├── Control plane (clusters, regions, workspaces)
│   └── Framework (actors, jobs, serving, datasets)
├── Tutorials/
│   ├── Hello train
│   ├── Hello serve
│   └── Hello finetune
├── How-to guides/
│   ├── Bring your own cluster
│   ├── Deploy a model to production
│   └── (added on demand from FAQ Backlog)
├── Reference/
│   ├── Python SDK (auto-generated from docstrings)
│   ├── CLI (auto-generated)
│   └── REST API (auto-generated from nimbuscore Swagger)
└── Blog                   ← launch posts, changelog deep-dives
```

Diátaxis-style separation: tutorial (learning) ≠ how-to (problem-solving)
≠ reference (information) ≠ explanation (understanding). Do not mix.

### Examples maintenance

Every public framework surface ships with at least one runnable example
in `examples/` of the framework repo:

| Surface | Example |
|---|---|
| `@nimbus.remote` | `examples/hello-remote/` |
| `nimbus.train` | `examples/hello-train/` |
| `nimbus.serve` | `examples/hello-serve/` |
| `nimbus.finetune` | `examples/hello-finetune/` |

Run every example in CI on a real (or simulated) cluster on every MR.
If an example breaks, the MR is blocked. That's how you stop bit-rot.

### Working with the AI Systems Engineer

- They write ADRs; you translate each into a developer-facing doc page
  within 48 hours of the ADR being approved.
- They ship a new SDK surface; you draft the reference page + the
  tutorial within the same sprint and ask for their technical review
  before merge.
- They change a public API; you update the docs in the same MR (file a
  sub-task on yourself in `PROD` if the dev work lands first).

### Working with the Frontend SWE

- They ship a new console page; you publish a screenshot-driven
  walkthrough page (under `Console Walkthroughs` in the PROD project)
  within the sprint.
- These walkthroughs feed the eventual How-to-guides section of the
  docs site.

### Public-channel triage

When `nimbus-hq/<framework>` and `nimbus-hq/console` have public
issues enabled, watch them. For each new issue:

- **Bug or feature request** → file a Jira work item against the owning
  role (likely `ENG`), link the GitLab issue, comment on GitLab with
  the work-item link.
- **Usage question** → answer if you can; add to the `FAQ Backlog`
  page in the PROD project. Convert the top 3 FAQ items into docs each
  sprint.
- **Doc bug** → fix it in the next MR; thank the reporter on GitLab.

### What stays out of Developer Relations

- **Code changes** beyond docstrings and example scripts. Propose
  changes via a Jira work item to the owning engineer.
- **Architecture decisions.** You report on them; you do not write
  them.
- **Backlog ordering.** That's the PM's call.

---

## PM ↔ DevRel — how the two teams in PROD work together

PM and DevRel collaborate so tightly they share this space and project.
The clean handoffs:

- **PM owns positioning + the launch blog post.** DevRel owns docs,
  tutorials, examples, FAQ Backlog. The line: anything customers read
  *before* deciding to try the product is PM; anything they read
  *while using* the product is DevRel.
- **Competitive launch happens** → PM briefs DevRel within 48 hours so
  the docs reflect any positioning shift.
- **DevRel files a research question** (e.g. "what does Ray do for
  X?") → PM picks it up via Tavily and publishes a competitive note.
- **PM drafts a Phase-N launch plan** → DevRel reviews for developer
  empathy before the CEO/CTO see it.
