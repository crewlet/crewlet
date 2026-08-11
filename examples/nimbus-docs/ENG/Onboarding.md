# Onboarding

## Who is here

| Role | Identity | What they own |
|---|---|---|
| **SWE** | `agent-swe` | `nimbuscore` (Go, control-plane REST), `nimbusk0s` (k0s worker image), operational `nimbuscode` (Terraform). |
| **Frontend SWE** | `agent-frontend-swe` | `console` (React/Vite dashboard), `website` (React marketing landing). |
| **AI Systems Engineer** | `agent-ai-systems-engineer` | `nimbus-hq/<framework>` (Python SDK + runtime — repo name TBD; they propose it). |

CTO leads this team (via Engineering department lead inheritance) and
also serves as code-review fallback for any of the three repos.

## Repo ownership — read before touching a repo

Read the [Repo Ownership](Repo-Ownership) page (it lives in the LEAD
project) in full. The short version: **stay in your repos.** If a story
you receive belongs to a different engineer, reassign it in Plane with a
comment rather than crossing repo boundaries. Per-role GitLab PATs are
scoped to the repos the role owns; if `git push` fails with a permission
error, that is the control working.

## Coordination model

Engineering stories live in the `ENG` Plane project. Most belong to one
engineer end-to-end; the exceptions are cross-repo features (Phase 2
will have many) and cross-project links up to PM epics in `PROD`.

**Cross-project links to PROD.** PM-driven epics live in `PROD`
(e.g. `PROD-1 — Framework v0.1 launch`). Engineering stories that
contribute to a PROD epic use work-item relations with the
`parent-of`/`child-of` link type. When you pick up such a story, the
epic context is one click away in the related-work-items panel.

**Pattern: framework needs a new control-plane primitive.**

1. **AI Systems Engineer** sketches the endpoint contract in an
   ADR page (ENG or LEAD project, depending on scope). Includes:
   URL shape, request/response payloads, error cases, performance
   budget.
2. AI Systems Engineer files a story in `ENG` against the **Backend
   SWE** with the ADR linked.
3. AI Systems Engineer files a *blocked-by* story in `ENG` against
   themselves for the framework-side integration.
4. SWE ships the endpoint; updates Swagger.
5. **Frontend SWE** (if the endpoint surfaces in the console) regenerates
   the TypeScript client and files their own UI story in `ENG`.
6. AI Systems Engineer unblocks; ships the framework-side integration.

All four stories live in `ENG`; the parent PROD epic shows their
collective progress via the related-work-items panel.

**Anti-pattern.** AI Systems Engineer calling the cluster directly
(kubectl, k8s client-go) to work around a missing nimbuscore endpoint.
*Do not do this.* The control plane is the single source of truth for
the cluster surface.

## Branching and MR conventions

- Branch from `main`; one branch per work item; branch name
  `<role>/<work-item-key>-<short-slug>` (e.g. `backend/ENG-142-job-placement-api`).
- MR title format: `<work-item-key>: <one-line summary>` — so the work
  item is traceable from the MR (and vice versa via a link in the MR
  description).
- MRs include: what changed, why, how to verify, screenshots (Frontend),
  benchmark numbers (perf-sensitive paths).
- MRs **must** include tests — unit for logic, integration for API
  contracts, smoke for critical user flows.
- Squash-merge into `main`. Delete the branch on merge.
- Conventional commit style (`feat:`, `fix:`, `refactor:`, `docs:`,
  `test:`, `chore:`) so changelog generation works.

## ADR convention

- Parent page: `Architecture Decisions` in the ENG project
  (default) or LEAD (for cross-cutting / strategic-impact decisions).
- Title: `ADR-NNN: <short decision>` (NNN = next free integer).
- Sections: Context, Options considered, Decision, Consequences.
- Linked from the epic that triggered the decision.
- The CTO reviews ADRs; engineers can self-approve trivial ones (mark
  the ADR `trivial` and ping the CTO for awareness).

## Observability

Every new code path adds:

- **Structured logs** (Go: `zap`/`slog`; Python: `structlog`).
  Snake-case event names; key-value fields.
- **Metrics** for any work item that has a rate, latency, or error
  count (Prometheus).
- **Trace spans** for any path that crosses a service boundary
  (OpenTelemetry).

If you cannot debug your own code from logs + metrics + traces alone,
add the missing observability before declaring the story Done.

## Local dev pointers

| Repo | Run locally |
|---|---|
| `nimbuscore` | `docker-compose up` from the repo root (see `docker-compose.yml`); exposes `:8080`. |
| `console` | `npm install && npm start` from the repo root; exposes `:3001`. Set `VITE_API_URL=http://localhost:8080`. |
| `website` | `npm install && npm start`; exposes `:3000`. |
| `nimbusk0s` | Built as a Docker image; smoke-test by joining a local k0s cluster with `JOIN_TOKEN`. |
| Framework | TBD — AI Systems Engineer documents `make dev` flow when the repo lands. |

## Coordination with non-engineering teams

- **PM** drives the backlog. If you think a story is in the wrong epic,
  the wrong priority, or missing acceptance criteria, comment on the
  work item — do not unilaterally re-organise the backlog.
- **DevRel** consumes your ADRs and your ships. When you ship a public
  API change, **mention DevRel in the MR**. They will draft the docs
  page and ask you for technical review.
- **CTO** is your escalation target. When stuck for >2 hours, surface
  it on the work item with what you tried, options you see, your
  recommendation, and urgency.

## What stays out of this unit

- **Backlog prioritisation.** PM.
- **Strategic direction.** CEO.
- **Marketing / launch narrative.** DevRel + PM.
- **Customer-facing positioning of your work.** DevRel writes the
  user-facing version; you review for accuracy.
