<div align="center">
<img src="assets/crewlet-icon.svg" alt="Crewlet" width="88">
</div>

# Crewlet Documentation

Crewlet is an open-source engine for orchestrating hierarchically organized AI
agent companies. It treats the organizational hierarchy as the primary
orchestration structure — knowledge, permissions, communication, and decisions
are all scoped by position in the org chart.

New here? Read in this order: **[Installation](getting-started/installation.md)
→ [Quickstart](getting-started/quickstart.md) →
[Choosing your stack](getting-started/choosing-your-stack.md)**, then dip into
the concept pages for the subsystems you touch.

Rather not write the YAML by hand? An AI assistant can interview you and
author it — see
**[Authoring with an AI assistant](getting-started/ai-authoring.md)**.

---

## Getting Started

- **[Installation](getting-started/installation.md)** — Prerequisites, install extras, and the local Pulsar + PostgreSQL stack
- **[Quickstart](getting-started/quickstart.md)** — Build a four-agent company and watch its first turn, with LLM provider options (Anthropic / OpenAI / any OpenAI-compatible)
- **[Choosing Your Stack](getting-started/choosing-your-stack.md)** — The decision guide for every external dependency: the tracker and knowledge base, the code host, the code sandbox, chat, email — what each path sets up for you, and what you must create manually
- **[Authoring with an AI Assistant](getting-started/ai-authoring.md)** — Let an AI write your company config: a step-by-step walkthrough, the `company-architect` skill, `crewlet schema` for editor autocomplete, and the `crewlet validate --json` fix loop
- **[Configuration Reference](getting-started/configuration.md)** — Full YAML config schema and examples

## Core Concepts

How the engine works, one subsystem per page:

- **[Overview](concepts/overview.md)** — The org chart as execution graph, design principles, high-level architecture
- **[Configuration](concepts/configuration.md)** — Two-tier config (ops-owned `config.yaml` + founder-owned versioned PostgreSQL), bootstrap sequence, unconfigured state, live propagation, auth, snapshot/rollback, whole-config encryption at rest
- **[Scaling Out](concepts/scaling.md)** — Why one node is the design's degenerate case rather than a lesser path, what a node is (`ingress` / `seats` / `workers`), the five kinds of coupling that had to be resolved and which one a lock actually fixes, what the fleet shares in PostgreSQL versus what stays per-process, the measured broker numbers the lease TTL and prefetch cap come from, and what the design does not promise
- **[Seat Ownership](concepts/seat-ownership.md)** — How a fleet decides which node runs which seat, and why no two ever run the same one: TTL leases with epoch fencing, fair-share placement with give-back, the two release modes, freshness-based admission, owner-only inbox and sandbox-control attachment, the durable subscription that holds an unowned seat's mail, the broker settings that must not delete it, and why a wedged-but-alive node ends its own process
- **[Control Plane](concepts/control-plane.md)** — How every node converges on one company config: the append-only activation epoch log, the reconcile poll, per-node apply status (`ok` / `error` / `degraded`), the posture a lagging node takes (`serve` / `wait` / `shed` / `isolated` / `stuck`), what a running turn sees through a live apply, and what `/health` and `/ready` report
- **[Secret Store](concepts/secret-store.md)** — Encrypted `secret_values` table consulted ahead of `os.environ` when resolving `${VAR}`: `crewlet secrets set/list/unset/get/rekey`, the `--secret-store` provisioning sink that hands minted credentials straight to the engine, store-wins precedence, and the Tier A root-of-trust boundary
- **[Organization Model](concepts/organization-model.md)** — Hierarchy, departments, teams, roles (seats), handles
- **[Humans in the Org Chart](concepts/humans-in-the-org.md)** — Human seats (`kind: human`): hierarchy membership, contact identities, notify delivery, escalation terminus, prompts and lookup
- **[Agent Runtime](concepts/agent-runtime.md)** — Agent lifecycle, states, execution model, graceful shutdown
- **[Turn Engine](concepts/turn-engine.md)** — Per-agent Plan / Execute / Review loop, sub-agents, colleague-surface tools, per-phase LLM models
- **[Subscription LLM Backends](concepts/subscription-llm-backends.md)** — Run agents on a coding CLI you already subscribe to (Claude Code, Codex, Gemini CLI, OpenCode, …) instead of a metered API key: per-seat state isolation, the in-prompt tool-call channel, `crewlet llm login`, and falling back to a key when the window is spent
- **[Code Sandbox](concepts/code-sandbox.md)** — Sandboxed coding-agent execution: the `run_sandbox` tool, E2B cloud/self-hosted, local boxes (direct or containerised, on a subscription login), Claude Code & OpenCode runners, git-auth recipes, mid-run clarifications
- **[Tool Skills](concepts/tool-skills.md)** — Knowledge-base-sourced prompt fragments (Confluence or Plane pages) that teach agents *how to use* each tool / MCP server
- **[Tool Capabilities](concepts/tool-capabilities.md)** — How the engine stays tool-stack agnostic: capability prose + MCP annotations, no hardcoded tool names
- **[Event System](concepts/event-system.md)** — EventQueue, topics, routing, inbox batching, distributed tracing
- **[Task Engine](concepts/task-engine.md)** — ExecutionTracker, external PM tool integration
- **[Scheduling](concepts/scheduling.md)** — Role/unit-scoped cron-style recurring work (standups, audits, nightly jobs)
- **[Knowledge System](concepts/knowledge-system.md)** — Query-time knowledge-base search behind the `KnowledgeSearcher` seam (Confluence CQL or Plane page search — one backend per org) + private `agent_diary`
- **[Agent Learning](concepts/agent-learning.md)** — Reflection loop, skill induction, episodic memory, counterparty profiles
- **[Conversation Sessions](concepts/conversation-sessions.md)** — What a seat already said in one Slack thread / issue / pull request, carried into that conversation's next turn: the entry shape and its elision budgets, which turns are recorded, why the key is the conversation and the dedupe is the work key, and why this is a structured ledger rather than a transcript replay
- **[One-on-Ones](concepts/one-on-ones.md)** — Manager↔report coaching as a usage pattern over the scheduler + A2A channels + learning loop
- **[Decision Framework](concepts/decision-framework.md)** — DACI model for multi-agent decisions

## Integrations

Connecting the external surfaces agents work on:

- **[Plane](integrations/plane.md)** — Self-hosted tracker **and** knowledge backend in one product: webhook routing, per-role MCP tools, knowledge search, `crewlet plane import`, tool-skill sync, skill promotion, `crewlet plane provision`, and a complete local docker-compose loop
- **[Jira](integrations/jira.md)** — Webhooks (Forge app for Cloud, direct for Data Center), MCP tools, per-team projects
- **[Confluence](integrations/confluence.md)** — Webhooks, MCP tools, query-time CQL knowledge search, `crewlet confluence import`
- **[GitLab](integrations/gitlab.md)** — gitlab.com or self-hosted: API-provisioned per-agent service accounts, `crewlet gitlab provision`, webhook routing, per-role MCP tools, sandbox code authoring
- **[GitHub](integrations/github.md)** — Per-role remote MCP tools for read/review/track; sandbox code authoring
- **[Slack](integrations/slack.md)** — One-app-per-agent setup with automated app provisioning via `crewlet slack provision` (App Manifest APIs), thread routing, and the per-phase working indicator
- **[Mattermost](integrations/mattermost.md)** — Self-hosted open-source chat: one bot account per agent, `crewlet mattermost provision`, a websocket event fleet instead of webhooks (no public URL needed), and thread routing
- **[Custom Transports](integrations/custom-transports.md)** — Build your own notification transport

## Guides

- **[Tools & MCP](guides/tools-and-mcp.md)** — Built-in tools, MCP integration, tool registry
- **[Extensions](guides/extensions.md)** — Extension system, hooks, writing extensions
- **[Deployment](guides/deployment.md)** — Docker, Pulsar sizing & auth, TimescaleDB observability, tracing
- **[Running a Fleet](guides/fleet.md)** — When to run more than one node, node roles, seat placement, draining and rolling upgrades
- **[Running One Agent Somewhere Else](guides/satellite-nodes.md)** — Put a single seat on a host that can reach what it needs — an internal API, a licensed binary, a GPU, a lab network — without moving the company: what a satellite is, what moves with the seat (its MCP servers above all), what the node still needs outbound, and what a pin costs when the host is down
- **[Configure via API](guides/configure-via-api.md)** — End-to-end curl recipes for bootstrapping a company through `/config/*`

## Reference

- **[CLI](reference/cli.md)** — Command reference
- **[API Endpoints](reference/api-endpoints.md)** — REST API routes and schemas
- **[Dashboard Design System](reference/dashboard-design.md)** — The dashboard's rooms and its visual system: what each room answers that no other one does, how a seat is coloured by what it is doing rather than by who it is, tokens measured against the worst surface they can land on, the shared panel recipe, the validated categorical hues, and the rules a change has to keep
- **[Environment Variables](reference/environment-variables.md)** — All configuration env vars
- **[Design Decisions](reference/design-decisions.md)** — Why certain architectural choices were made
