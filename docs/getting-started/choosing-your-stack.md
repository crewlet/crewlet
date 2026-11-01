# Choosing Your Stack

Crewlet is the engine; the surfaces your agents work on — the LLM, the
work-item tracker, the knowledge base, the code host, chat, the code sandbox —
are services you pick and connect. Every one of them has a hosted and a
self-hosted path. This page is the decision guide: what each choice implies,
what you must create **yourself** in the external service, and where the
detailed setup steps live.

**Only the LLM is required.** The tracker and the knowledge base ship with the
engine and are on by default; everything else is a surface you add when you
want your company working where your people already are. A company with one
signed-in coding CLI and nothing else runs.

A useful mental model: for each integration there is usually

1. **Something only you can create** — an Atlassian site, a Slack workspace, a
   GitLab group, an E2B account. Crewlet never creates top-level tenancy for
   you.
2. **Per-agent identities inside it** — service accounts, bot apps, tokens.
   For Mattermost, Slack, and GitLab a provisioning CLI creates
   these idempotently; for Atlassian and GitHub you create them by hand,
   because neither third-party app issues a credential on a provisioner's behalf.
   Those two still have a CLI — it reports which account each seat's own
   credential turned out to be, and registers the webhooks.
3. **A webhook back to the engine** — so external activity wakes the right
   agent. Self-registered where the API allows it, manual where it doesn't.
   Mattermost is the exception: it has no usable inbound webhook, so the
   engine dials out instead and needs no reachable address at all.

---

## LLM provider (required)

**Every model is reached through a coding CLI** the engine drives as a process —
the vendor's own harness, which follows every model the vendor ships, signed in
with your plan or your key. So the choice is which vendor's models you want,
and how you pay for them:

| Models | Config | Pay with | Notes |
|---|---|---|---|
| **Anthropic's** (Claude) | `model: sonnet` — Claude Code is the default `agent` | a Claude Pro / Max plan, or an Anthropic API key (`auth.mode: api-key`) | The one CLI that mints a headless token (`crewlet llm login -capture-token`), the best sign-in for a container. Runs the executor itself in [agent mode](../concepts/llm-backends.md#agent-mode), and is a sandbox coding agent. |
| **OpenAI's** (GPT) | `agent: codex` | a ChatGPT Plus / Pro plan, or an OpenAI API key (`auth.mode: api-key`) | A text model behind the engine's tool loop; a seat on it that writes code takes its sandbox work to a Claude Code or OpenCode entry. |
| **Open models** — OpenRouter, a hosted provider, your own server | `agent: opencode`, `model: <provider>/<model>` | the provider's key in `env`, or a login of OpenCode's own; nothing at all for a server that takes no key | OpenCode 2. Fully self-hostable: a vLLM, llama.cpp or Ollama server is a provider declared in OpenCode's own configuration ([how](../concepts/llm-backends.md#open-models-openrouter-and-local-servers-opencode)). Agent mode and a sandbox coding agent, like Claude Code. |
| **Gemini, Qwen, Cursor, Copilot, Grok, Muse, Kimi, Hermes, Pi** | `agent:` the profile's name | the vendor's login or key | Text models. [Supported CLIs](../concepts/llm-backends.md#supported-clis) pairs each profile with its binary (`gemini-cli` drives `gemini`, `kimi-code` drives `kimi`, …). |

Whichever you pick, the CLI's binary has to be installed on the engine host —
the container image ships none, see [Installation](installation.md#a-coding-cli)
— and each seat gets an isolated home in it, so agents never share a CLI's
memory. See [LLM Backends](../concepts/llm-backends.md) for the whole design.

You can configure several named entries and pick per role (`role.llm`), plus
a cheap auxiliary model per role (`role.llm_auxiliary`) for reflection and
summarisation work — worth choosing deliberately, because each of those calls
pays a CLI launch under a fixed deadline
([why](../concepts/llm-backends.md#auxiliary-work-and-latency)). `role.llm` also
accepts a **list**, which is how a plan and a key compose: `llm: [plan, key]`
runs on the flat-rate plan and falls through to the metered key while the
plan's window is spent. See [Quickstart § LLM options](quickstart.md#llm-options).

**Which to pick.** A key is the answer for a fleet that has to be fast and
always available: it is billed per token and throttled far less than a plan.
A plan is the better answer when you are developing, evaluating, or running a
small company yourself and want predictable cost — check its terms, which are
generally written for interactive use by the subscriber. Both are the same CLI,
so the choice is one `auth.mode` and can change at any time. A plan can also
back the [code sandbox](../concepts/code-sandbox.md) — via a headless token on
any backend, or via a local cell (`providers.sandbox.local` plus `run_in:
direct`), which runs the coding agent on the engine host against the same
login.

**And the CLI can run the turn itself.** `mode: agent` makes the seat's executor
the CLI's own agentic loop — a real shell, a real editor, a real checkout — with
the seat's tools reaching it over an MCP bridge, as a detached run that outlives
the turn. It needs more than a sign-in: a CLI with a coding-agent runner
(`claude-code` or `opencode`), a `providers.sandbox` catalogue to place the run
in, and `CREWLET_MCP_BRIDGE_URL` set to something a box can dial. `crewlet llm
doctor` checks all three. Text mode stays the default and is the better answer
when you want the tool log to be the engine's own. See
[LLM Backends § Two modes](../concepts/llm-backends.md#two-modes-a-text-model-or-the-agent-itself).

**Embeddings** (`providers.embeddings`) power the semantic half of a native
knowledge search — the company's own pages and work items, embedded once for
the whole fleet, and each search's query — and the agent-learning subsystem's
similarity recall (personal diary + episode recall). They are the one model
call the engine makes itself, since a coding CLI computes no vectors: any
OpenAI-compatible embeddings endpoint works via `base_url`, including a
self-hosted one; a model
this build does not know states its width and its limits (the tokens one input
may hold, and the inputs and tokens one request may carry), and so do the
request limits of `gemini-embedding-001` and `embed-v4.0`, whose compatible
endpoints document none — see
[Configuration](configuration.md#providers). Without an embeddings
provider the engine still runs: search is keyword only, personal memory is
chosen from a seat's recent notes alone, and similar-prior-work recall renders
nothing, because a seat's recent turns are not similar work.

---

## Core infrastructure

**There is none to choose.** The engine is one binary: its event stream is a
NATS JetStream server it embeds, and its store is a local file it creates and
owns exclusively. A single host runs a whole company with nothing else
installed, in development and in production alike.

Two slots change once a deployment outgrows one node:

| Slot | Options |
|---|---|
| **Stream** | `embedded` (default) — and a fleet is the *same* embedded server with `stream.cluster.name/port/peers` naming its peers and `stream.replicas: 3` · or `nats` + `stream.url` to dial a NATS server or cluster somebody else runs |
| **Coordination** | `local` (one node) · `embedded-kv` (a fleet — one node or three; two has no quorum and is refused by name) |

Coordination takes no address of its own, and that is deliberate rather than an
omission: the KV holding leases, ledgers and the token counter lives on the
stream's **own** broker. On the embedded broker it holds a second connection
beside the queue's, and both are watched: either one closing for good stops the
node, so it cannot keep renewing leases over a link that still works while the
one carrying its inbox is gone — alive to its peers, deaf to its work — for
longer than a reconnect.

The **store** is never one of these: it stays one file per node, which is why
everything genuinely shared lives in coordination instead. See
[Running a Fleet](../guides/fleet.md) and
[Deployment](../guides/deployment.md) for sizing, and
[Deployment § The fleet's broker admits only its own nodes](../guides/deployment.md#the-fleets-broker-admits-only-its-own-nodes)
for broker authentication: every route, leaf link and operator connection to an
embedded fleet's broker presents a key derived from `secrets.keys`.

---

## Work-item tracker + knowledge base

Agents file and pick up work in a tracker, and search a shared knowledge base.

**You do not have to bring either one.** The engine ships its own, and they
are the default:

```yaml
tracker:
  backend: native      # the default
knowledge:
  backend: native      # the default
```

That is the whole setup. Items and pages live in the fleet's own store, there
is a board and a page browser on the dashboard, seats get twenty-three tools for
them — eighteen over the tracker and five over the pages — and your own AI
assistant can reach them over
[`/operator/mcp`](../reference/api-endpoints.md#operatormcp--your-own-assistant).
Nothing to create, nothing to provision, no per-seat accounts, no webhook.

### Which to choose

| | Native | Atlassian |
|---|---|---|
| Setup | none | a site, a project, a space, a per-seat account each, webhooks |
| Where the record lives | your own deployment | Atlassian's |
| People can use it | through the Crewlet dashboard | through Jira and Confluence, which they may already live in |
| Workflows, custom fields, sprints, permission schemes | no | yes |
| Existing tickets | none — it starts empty | whatever you already have |

**Take the native one unless you have a reason not to.** The reasons are real
and they are all about the people rather than the agents: a team that already
works in Jira should not be asked to watch a second board, and an existing
backlog does not migrate (there is deliberately no migration path — see
below). If neither applies, the vendor path costs you a provisioning afternoon
and buys nothing the agents use.

Either way, the org chart is the same. A unit's `project` and `space` name its
project and its container on whichever backend the company runs — which is why
the fields are not called `jira_project` and `confluence_space`.

> **No migration between them.** Switching `tracker.backend` does not move
> anything, in either direction, and the engine does not offer to: a
> half-migrated tracker where some items answer to one system and some to the
> other is worse than either, and it is a state nothing can detect from the
> outside. Choose once, per company.

### Atlassian (Jira + Confluence Cloud or Data Center)

The managed-SaaS path: Atlassian runs the tracker and the wiki, and agents work
them through per-agent Atlassian identities. Per-agent setup is manual, since
Atlassian exposes no service-account-provisioning API a CLI can drive.

What **you** do, by hand:

1. **Create the Atlassian site** (or use your existing one) — Crewlet never
   creates the tenancy.
2. **Create per-agent identities**: an Atlassian account (or API token
   identity) per agent seat, so issues can be assigned to agents and comments
   attribute correctly. Mint one API token per agent and reference it from
   `role.mcp_env`, beside the account's address (`JIRA_USERNAME` and
   `JIRA_API_TOKEN`, `CONFLUENCE_USERNAME` and `CONFLUENCE_API_TOKEN` for the
   `atlassian` MCP server, which refuses a token without its address), plus
   one admin/service token for the engine's org-level lookups
   (`integrations.jira.token`).
3. **Webhooks**:
   - **Cloud** — install the [Crewlet Forge app](https://github.com/crewlet/forge)
     in your site; it forwards Jira + Confluence events to
     `POST /webhooks/forge` (signature-verified; needs the `forge` install
     extra).
   - **Data Center** — register webhooks directly against
     `POST /webhooks/jira` / `POST /webhooks/confluence` with an HMAC secret.
4. **Create the spaces/projects** your units use (`project` / `space` per
   unit) and an `Onboarding` page per space.

Details: [Jira](../integrations/jira.md) ·
[Confluence](../integrations/confluence.md).

> **One knowledge backend per company**, and validation enforces it: a company
> that sets `knowledge.backend: native` *and* declares
> `integrations.confluence` is refused. "What do we already know about this"
> must not depend on which searcher was asked. See
> [Knowledge System](../concepts/knowledge-system.md).
>
> An empty `backend` **derives**: declare `integrations.confluence` and you get
> `confluence`; declare nothing and you get `native`. So an Atlassian company
> that has not read this page keeps the backend it had.

---

## Code host

Engineer roles read, review, and track code through per-role MCP tools, and
author changes through the [code sandbox](../concepts/code-sandbox.md) under
their **own identities** — so MRs/PRs come from the agent, not from you.

### Option A: GitLab — gitlab.com or self-hosted

Per-agent identities are API-provisionable end to end, so one CLI run sets up
the whole fleet — the same property that makes Mattermost the easiest chat to
start on. [GitLab Integration](../integrations/gitlab.md#walkthrough-nimbus-against-local-gitlab)
walks the bundled Nimbus example onto a local GitLab end to end:

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

github.com or a GitHub Enterprise Server — leave `integrations.github.url`
unset for the former, name the instance for the latter:

1. **Create the org/repos** (you), plus **one PAT per engineer seat** — GitHub
   has no API-provisionable service accounts, so per-agent identities are
   machine users or fine-grained PATs you create by hand
   (`role.mcp_env.github` carries `Authorization: Bearer ${GITHUB_TOKEN_X}`).
   The engine derives each seat's login from its own token; nothing is
   declared.
2. **Register the webhooks** with
   [`crewlet github provision`](../reference/cli.md#crewlet-github-provision),
   which mints the secret and registers one organization hook where the
   credential may (covering repositories created later) or one per repository
   where it may not — and reports which account each seat turned out to be,
   which is the finding that decides whether the integration routes at all.
3. Agents get the full toolset of the hosted
   [GitHub MCP server](https://github.com/github/github-mcp-server) per role;
   the sandbox git-auth recipe has a GitHub form (credential helper on
   `github.com` + `GITHUB_TOKEN` in `role.sandbox.env`) — see
   [GitHub integration](../integrations/github.md).

A company can run **both** hosts: they are two hosts with different
repositories on them, which is what a migration and an open-source presence
both look like.

### Option C: the engine's own host — `code.native`

The engine can host the company's repositories itself, on its own state log,
with each push's bytes in the [object store](../concepts/object-store.md):
set `code.native` and declare the repositories in `code.repos`, and every
activation creates, settles and retires them (see
[Code Host](../concepts/code-host.md)). It is **not exclusive** — the GitLab
and GitHub integrations keep working beside it, so a company may host some
repositories natively and keep others on a vendor.

Today it is a place for **people and agents** to keep and review their code:
people clone, fetch and push over HTTPS with their own API token ([Hosting
Code](../guides/code.md)), and every push is recorded as whoever made it.
Agents change it through their code tools — a small commit, a revert or a
pick, a branch, each built by the engine and recorded as the seat's push — or
push from a coding box's clone with their run's own credential ([Coding
boxes](../concepts/code-host.md#coding-boxes)). A **change** proposes one
branch into another, and people and agents review it, raising findings, a
blocker among them holding it back until it is cleared ([Review a
change](../guides/code.md#review-a-change)), while the repository's home git
node queues its checks, and a change is merged as its branch's gate allows —
through the branch's merge queue where its gate requires a check ([The merge
queue](../concepts/code-host.md#the-merge-queue)). Read [What it does not do
yet](../concepts/code-host.md#what-it-does-not-do-yet) before choosing it as
an engineering team's only host. A company that hosts
code on it takes the [S3 object store](../concepts/object-store.md#an-s3-bucket--s3)
in production, since its repositories grow for as long as it works in them.

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
| **None** | omit `providers.sandbox` | Roles use the native executor tool-loop; no code authoring. |
| **E2B cloud** | `e2b: {api_key: "${E2B_API_KEY}"}` | Sign up at <https://e2b.dev>, create an API key in the dashboard. Fastest path. |
| **Self-hosted E2B** | `e2b: {api_key: …, domain: "${E2B_DOMAIN}"}` | Deploy [e2b-dev/infra](https://github.com/e2b-dev/infra) on your own cloud account, then point the same SDK/code path at it via `domain`. Your cluster issues its own `E2B_API_KEY`. |
| **Local — container** | `local: {image: …}` + `run_in: container` | Docker/Podman on the engine host. Real host isolation, no E2B account. You supply an image with the coding CLI installed. Can use a [plan's login](../concepts/llm-backends.md#authentication) instead of an API key. |
| **Local — direct** | `local: {}` + `run_in: direct` | A process tree on the engine host, using the CLI (and login) already installed there. Fastest to stand up and the natural pair for an entry signed in on a plan — but the coding agent runs as the engine user, so it isolates *state*, not the host. Workstation or dedicated VM only. |

`providers.sandbox` is a **catalogue**, so these are not exclusive: configure
`e2b:` and `local:` together and give each seat its own `run_in` — the seat
that needs the host's plan login runs `direct`, the seat whose
generated code must never touch that host runs `e2b`. Where a seat that names
none goes is `default_run_in`, which is required whenever some seat (or
agent-mode entry) would fall to it on a catalogue offering more than one cell
(and `local:` alone offers two); a company whose every seat names its own
needs none — as long as *something* names a cell, since a backend is only
built for a cell that is reached.

Coding-agent choice: the coding agent **is** the CLI of the entry the seat's
code work runs on (`role.llm_sandbox`, else its `llm`), with that entry's model
and that entry's sign-in, so it needs no extra secret. **Claude Code** runs a
`claude-code` entry; **OpenCode** — version 2, the `@opencode/cli` package, in
the box — runs an `opencode` entry and so any model OpenCode reaches, which
takes `coding_agent: opencode` on the seat (or as the provider default). Any
other pairing is [refused](../concepts/code-sandbox.md#failure-modes), and a
seat whose own model is on a CLI with no coding-agent runner — Codex, Gemini
CLI, the rest — points `role.llm_sandbox` at a `claude-code` or `opencode`
entry. A sign-in the seat carries itself in `role.sandbox.env` — a Claude Code
token, say — counts toward the check that refuses a remote box its entry's login
cannot follow.

One networking caveat for local development: a **cloud** E2B sandbox cannot
reach services on your laptop (`localhost` GitLab) — in-sandbox tool
access to those needs a reachable deployment, a tunnel, or self-hosted E2B on
the same network.

---

## Putting it together

Two bundled **Nimbus examples** model the same seven-seat company at opposite
ends of this page. `examples/nimbus.company.yaml` + `examples/nimbus.config.yaml`
is the **reference**: a pick from every row — GitLab, Mattermost, open models
through OpenCode on an OpenRouter key, a sandbox its engineers push merge
requests from, the engine's own tracker and knowledge base rather than a
vendor's, and two repositories on the engine's own code host beside GitLab's.
Read it to
see what a full stack looks like written out, and
[Jira](../integrations/jira.md) / [Confluence](../integrations/confluence.md)
for the blocks that move those two halves to Atlassian.

`examples/nimbus-claude-cli.company.yaml` + `examples/nimbus-claude-cli.config.yaml`
is the **short path**: every pick that costs nothing extra to stand up — chat
on **Mattermost**, the model **Claude Code on your own Claude plan**, the
tracker and knowledge base the **engine's own**, and the code sandbox on the
**engine host** (`run_in: direct`), which reuses that same plan login
rather than needing an account of its own. Its three
engineering seats run that CLI in
[agent mode](../concepts/llm-backends.md#agent-mode), so their
executor *is* the CLI's own agentic loop with a real shell. The rows that need
somebody else's service are deliberately empty, which is what lets the whole
thing run from one compose profile and one `crewlet llm login`:

```bash
docker compose --profile mattermost up -d --wait
COMPANY=examples/nimbus-claude-cli.company.yaml scripts/mattermost-dev-bootstrap.sh
crewlet llm login default -from-host \
    -company examples/nimbus-claude-cli.company.yaml \
    -config examples/nimbus-claude-cli.config.yaml
crewlet run -config examples/nimbus-claude-cli.config.yaml \
            -company examples/nimbus-claude-cli.company.yaml
```

Reading it top to bottom is the fastest way to see the choices on this page
made concretely — each block carries the rationale in comments, including the
ones it did *not* make and what adding them would take. Fill in the other rows
one at a time from the integration pages linked above; nothing in the example
has to be undone first.
