# Choosing Your Stack

Crewlet is the engine; the surfaces your agents work on — the LLM, the
work-item tracker, the knowledge base, the code host, chat, the code sandbox —
are external services you pick and connect. Every one of them has a hosted and
a self-hosted path. This page is the decision guide: what each choice implies,
what you must create **yourself** in the external service, and where the
detailed setup steps live.

A useful mental model: for each integration there is usually

1. **Something only you can create** — an Atlassian site, a Slack workspace, a
   GitLab group, an E2B account. Crewlet never creates top-level tenancy for
   you.
2. **Per-agent identities inside it** — service accounts, bot apps, tokens.
   For Mattermost, Slack, Plane, and GitLab a provisioning CLI creates
   these idempotently; for Atlassian and GitHub you create them by hand.
3. **A webhook back to the engine** — so external activity wakes the right
   agent. Self-registered where the API allows it, manual where it doesn't.
   Mattermost is the exception: it has no usable inbound webhook, so the
   engine dials out instead and needs no reachable address at all.

---

## LLM provider (required)

| Option | Config | Notes |
|---|---|---|
| **Anthropic** | `type: anthropic` | Official SDK; prompt caching set explicitly by the provider. Required if you want Claude Code as the sandbox coding agent. |
| **OpenAI** | `type: openai` | Official SDK; automatic prefix caching. |
| **Any OpenAI-compatible endpoint** | `type: openai-compatible` + `base_url` | Hosted aggregators (OpenRouter, Together, …), cloud gateways, or your own vLLM / LiteLLM deployment. Fully self-hostable. OpenCode (the provider-agnostic sandbox coding agent) can reuse this same provider. |
| **A coding CLI you subscribe to** | `type: cli-agent` + `cli.agent` | No API key: drives `claude` / `codex` / `gemini` / `opencode` / `cursor-agent` / `copilot` on the operator's own subscription. The CLI must be installed on the engine host. Flat-rate cost, higher per-call latency, and each seat gets an isolated CLI home so agents never share memory. See [Subscription LLM Backends](../concepts/subscription-llm-backends.md). |

You can configure several named providers and pick per role (`role.llm`), plus
a cheap auxiliary model per role (`role.llm_auxiliary`) for reflection and
summarisation work. `role.llm` also accepts a **list**, which is how a
subscription and a metered key compose: `llm: [subscription, default]` runs
on the flat-rate CLI and falls through to the API key when the subscription
window is spent. See [Quickstart § LLM options](quickstart.md#llm-options).

**Which to pick.** A metered key is the default answer for a fleet that has
to be fast and always available. A subscription CLI is the better answer when
you are developing, evaluating, or running a small company yourself and want
predictable cost — check your plan's terms, which are generally written for
interactive use by the subscriber. A subscription can also back the
[code sandbox](../concepts/code-sandbox.md) — via a headless token on any
backend, or via `providers.sandbox.type: local`, which runs the coding agent
on the engine host against the same login.

**Embeddings** (`providers.embeddings`) power the agent-learning subsystem
(personal diary + episode recall). Any OpenAI-compatible embeddings endpoint
works via `base_url` — including a self-hosted one. Without an embeddings
provider the engine still runs; learning features degrade gracefully.

---

## Core infrastructure (required)

**Apache Pulsar** and **PostgreSQL (TimescaleDB + pgvector)**. Local: the
bundled `docker-compose.yml` (see [Installation](installation.md)). Production:
any Pulsar cluster and PostgreSQL server you operate — including a dedicated
Pulsar tenant/namespace with token auth when Crewlet shares a cluster with
your other workloads. See [Deployment](../guides/deployment.md) for sizing
and broker authentication.

---

## Work-item tracker + knowledge base

Agents file and pick up work in a tracker, and search a shared knowledge base
at query time. Two stacks are supported — pick **one**:

### Option A: Plane (self-hosted)

[Plane](https://plane.so) covers **both halves in one product**: work items and
pages. Everything runs on infrastructure you own. Crewlet targets a self-hosted
deployment of the **[crewlet/plane fork](https://github.com/crewlet/plane)**
(upstream Plane CE plus API capabilities the integration depends on — public
pages CRUD + search, API-provisionable service accounts, webhook CRUD,
@-mentions for service accounts; the fork's images are published under
`ghcr.io/crewlet/plane-*`, tag `preview`). Against stock Plane CE the
integration degrades to work-item routing only — see
[Plane § The fork](../integrations/plane.md#the-fork).

What **you** do once:

1. **Deploy the fork** — locally via the bundled compose profile
   (`docker compose --profile plane up -d`, UI on `http://localhost:8091`) or
   on a server with the fork images.
2. **Create the workspace and your own (founder) account.** Locally,
   `scripts/plane-dev-bootstrap.sh` automates this; against a remote instance
   you sign up and create the workspace in the UI.
3. **Run the provisioner** — `crewlet plane provision company.yaml
   -public-url https://<engine>` creates one service account
   per agent seat, project memberships, per-agent API tokens (minted into the
   `${VAR}` references your config already declares), the engine's read
   account, and the workspace webhook — idempotently.
4. **Publish docs + tool skills** — `crewlet plane import company.yaml <dir>`.

Everything else (webhook routing, knowledge search, skill sync, promotion) is
engine-side. Full walkthrough: [Plane integration](../integrations/plane.md).

### Option B: Atlassian (Jira + Confluence Cloud or Data Center)

The managed-SaaS path: Atlassian runs the tracker and the wiki, and agents work
them through per-agent Atlassian identities. Per-agent setup is manual, since
Atlassian exposes no service-account-provisioning API a CLI can drive.

What **you** do, by hand:

1. **Create the Atlassian site** (or use your existing one) — Crewlet never
   creates the tenancy.
2. **Create per-agent identities**: an Atlassian account (or API token
   identity) per agent seat, so issues can be assigned to agents and comments
   attribute correctly. Mint one API token per agent and reference them from
   `role.mcp_env` (`JIRA_API_TOKEN` / `CONFLUENCE_API_TOKEN` for the
   `atlassian` MCP server), plus one admin/service token for the engine's
   org-level lookups (`integrations.jira.token`).
3. **Webhooks**:
   - **Cloud** — install the [Crewlet Forge app](https://github.com/crewlet/forge)
     in your site; it forwards Jira + Confluence events to
     `POST /webhooks/forge` (signature-verified; needs the `forge` install
     extra).
   - **Data Center** — register webhooks directly against
     `POST /webhooks/jira` / `POST /webhooks/confluence` with an HMAC secret.
4. **Create the spaces/projects** your units use (`integrations.jira.project`
   / `integrations.confluence.space` per unit) and an `Onboarding` page per
   space.

Details: [Jira](../integrations/jira.md) ·
[Confluence](../integrations/confluence.md).

> **Don't mix knowledge backends**: the engine wires exactly one
> `KnowledgeSearcher` — Confluence CQL or Plane page search — selected by
> which integration is configured. See
> [Knowledge System](../concepts/knowledge-system.md).

---

## Code host

Engineer roles read, review, and track code through per-role MCP tools, and
author changes through the [code sandbox](../concepts/code-sandbox.md) under
their **own identities** — so MRs/PRs come from the agent, not from you.

### Option A: GitLab — gitlab.com or self-hosted

Per-agent identities are API-provisionable end to end, so one CLI run sets up
the whole fleet. This is the path the bundled Nimbus example uses:

1. **Create the top-level group** (you) — e.g. `gitlab.com/your-group` — or run
   a self-hosted GitLab (any modern GitLab; a local instance ships as the
   compose `gitlab` profile for end-to-end testing). Set `integrations.gitlab.url`
   accordingly — the same config shape covers gitlab.com and self-hosted.
2. **Run the provisioner** — `crewlet gitlab provision company.yaml
   -public-url https://<engine>` creates one **service
   account per engineering seat** (mentionable, assignable, reviewer-able),
   group/project memberships, per-agent PATs minted into your config's own
   `${VAR}` references, and the webhooks. Requires an admin-capable operator
   token for the run; see the
   [permission matrix](../integrations/gitlab.md#permission-matrix--the-operator-credential).
   On gitlab.com, note the [prerequisites](../integrations/gitlab.md#prerequisites-gitlabcom)
   (service accounts need a paid tier; self-hosted has no such gate).
3. Agents drive GitLab via the `glab` CLI's MCP server (`glab mcp serve`,
   spawned per role with that role's PAT); the sandbox git-auth recipe makes
   `git push` + MR creation work headlessly under the agent's identity.

Details: [GitLab integration](../integrations/gitlab.md).

### Option B: GitHub

github.com only (no self-hosted GitHub support in the integration):

1. **Create the org/repos** (you), plus **one PAT per engineer seat** — GitHub
   has no API-provisionable service accounts, so per-agent identities are
   machine users or fine-grained PATs you create by hand
   (`role.mcp_env.github` carries `Authorization: Bearer ${GITHUB_TOKEN_X}`).
2. **Register the webhook** on the repos/org (manual): target
   `POST /webhooks/github` with the shared `integrations.github.webhook_secret`.
3. Agents get the full toolset of the hosted
   [GitHub MCP server](https://github.com/github/github-mcp-server) per role;
   the sandbox git-auth recipe has a GitHub form (credential helper on
   `github.com` + `GITHUB_TOKEN` in `role.sandbox.env`) — see
   [GitHub integration](../integrations/github.md).

---

## Chat

Chat is the human↔agent conversational surface (DMs, channels, escalations)
and is also where the DACI [decision framework](../concepts/decision-framework.md)
plays out. Two backends ship.

| | Mattermost | Slack |
|---|---|---|
| Hosting | self-hosted, open source | SaaS |
| Credentials per agent | 1 (bot token) | 2 (bot token + signing secret) |
| Manual steps per agent | none | one OAuth **Allow** click |
| Engine must be publicly reachable | no — the engine dials out | yes — the Events API POSTs to it |
| Working status | fixed *"is typing…"* (default off) | free text, per turn phase |

The two are interchangeable as far as the engine is concerned; they differ in
what you have to stand up and where your people already are.

**Mattermost is the quickest to try.** It ships in this repo's
`docker-compose.yml`, provisioning is one non-interactive command, and nothing
has to reach the engine — so you can go from nothing to an agent answering in a
channel on a laptop, with no account to create and no tunnel. It is what the
bundled example runs on, which makes it the path with the least between you and
a working loop.

**Slack is where most companies already are.** If yours is one of them, that is
the answer regardless of anything above: the agents show up where the
conversations already happen, under the workspace admin, SSO, retention and
compliance setup your organization already runs, with nothing new to host or
patch. It also renders the per-phase working indicator as real text, which
Mattermost has no API for.

Running both at once is supported — each agent seat carries whichever
identities it needs.

### Option A: Mattermost (self-hosted)

1. **Run the Mattermost server yourself** (official Docker image; it needs
   its own PostgreSQL) and **create the team** agents will live in. Crewlet
   never creates top-level tenancy.
2. **Declare each agent's identity in the company YAML** — one per-agent bot
   token as a `${VAR}` placeholder under `role.integrations.mattermost`, and
   the Mattermost MCP tool server in `mcp_servers`.
3. **Provision the bots** with
   [`crewlet mattermost provision`](../reference/cli.md#crewlet-mattermost-provision),
   which creates one bot account per agent, adds it to the team and its
   channels, and mints its access token into the `${VAR}` the YAML
   references. The only thing you do by hand is generate one system-admin
   token, once.

Nothing needs to reach the engine from outside: it opens outbound websockets
per seat rather than receiving webhooks, so no tunnel and no public URL.
Details: [Mattermost integration](../integrations/mattermost.md).

### Option B: Slack

There is no self-hosted variant:

1. **Create the Slack workspace yourself** (or use your company's).
2. **Declare each agent's Slack identity in the company YAML** — the
   per-agent bot token + signing secret as `${VAR}` placeholders under
   `role.integrations.slack`, and the shared Slack MCP tool server in
   `mcp_servers`. Each agent is its own bot identity: own token, own DM,
   own @-mention.
3. **Provision the apps** with
   [`crewlet slack provision`](../integrations/slack.md),
   which creates one app per agent through Slack's App Manifest APIs, points
   each app's event subscriptions at `POST /webhooks/slack/{handle}`, and
   writes the obtained secrets back under the exact `${VAR}` names the YAML
   references. Two things stay manual, because Slack has no API for either:
   generating one app *configuration token* (once, ever) and clicking
   **Allow** on each app's install. The engine's API must be reachable from
   Slack (public URL, or a tunnel during development) for both the events-URL
   verification and the OAuth landing page.

Clicking through [api.slack.com/apps](https://api.slack.com/apps) per agent
still works if you prefer it — see
[Manual Setup](../integrations/slack.md#manual-setup).

Details (scopes, Events API, thread routing, the working-status indicator):
[Slack integration](../integrations/slack.md).

---

## Code sandbox (optional but recommended for engineer roles)

Lets a role's Execute phase run a real coding agent (Claude Code or OpenCode)
with a shell, a filesystem, and a git checkout, inside an isolated sandbox —
see the [Code Sandbox](../concepts/code-sandbox.md) concept page.

| Option | How | Notes |
|---|---|---|
| **None** | omit `providers.sandbox` (or `type: none`) | Roles use the native Execute tool-loop; no code authoring. |
| **E2B cloud** | `type: e2b` + `E2B_API_KEY` | Sign up at <https://e2b.dev>, create an API key in the dashboard. Fastest path. |
| **Self-hosted E2B** | `type: e2b` + `domain: "${E2B_DOMAIN}"` | Deploy [e2b-dev/infra](https://github.com/e2b-dev/infra) on your own cloud account, then point the same SDK/code path at it via `domain`. Your cluster issues its own `E2B_API_KEY`. |
| **Local — container** | `type: local` + `local: {containment: container, image: …}` | Docker/Podman on the engine host. Real host isolation, no E2B account. You supply an image with the coding CLI installed. Can use a [subscription login](../concepts/subscription-llm-backends.md) instead of an API key. |
| **Local — direct** | `type: local` + `local: {containment: direct}` | A process tree on the engine host, using the CLI (and login) already installed there. Fastest to stand up and the natural pair for a subscription backend — but the coding agent runs as the engine user, so it isolates *state*, not the host. Workstation or dedicated VM only. |

Coding-agent choice: **OpenCode** is provider-agnostic (reuses any
OpenAI-compatible provider you already configured — no extra secret);
**Claude Code** requires an `anthropic` provider entry, a
[`cli-agent`](../concepts/subscription-llm-backends.md) one, or an
`ANTHROPIC_*` credential in `role.sandbox.env` (select it per role via
`role.llm_sandbox`).

One networking caveat for local development: a **cloud** E2B sandbox cannot
reach services on your laptop (`localhost` Plane/GitLab) — in-sandbox tool
access to those needs a reachable deployment, a tunnel, or self-hosted E2B on
the same network.

---

## Putting it together

The bundled **Nimbus example** (`examples/nimbus.company.yaml` +
`examples/nimbus.config.yaml`) is a complete seven-seat reference wired for
Plane + GitLab + Mattermost + E2B sandbox + an OpenAI-compatible LLM, with a
fully-local loop: `docker compose --profile plane --profile mattermost up -d`,
then `scripts/plane-dev-bootstrap.sh` and `scripts/mattermost-dev-bootstrap.sh`,
then
`crewlet run -config examples/nimbus.config.yaml -company
examples/nimbus.company.yaml`. Reading it top to bottom is the fastest way to
see every choice on this page made concretely — each block carries the
rationale in comments.
