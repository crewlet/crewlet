# Choosing Your Stack

Crewlet is the engine; the surfaces your agents work on — the LLM, the
work-item tracker, the knowledge base, the code host, chat, the code sandbox —
are services you pick and connect. Every one of them has a hosted and a
self-hosted path. This page is the decision guide: what each choice implies,
what you must create **yourself** in the external service, and where the
detailed setup steps live.

**Only the LLM, and a place for its runs, is required.** The tracker and the
knowledge base ship with the engine and are on by default; everything else is a
surface you add when you want your company working where your people already
are. Every agent seat's executor runs in a [code sandbox](#code-sandbox-where-every-executor-runs)
cell, and `local: {}` with `default_run_in: direct` is the engine host itself,
with nothing to sign up for — so a company with one signed-in coding CLI, that
cell, and nothing else runs. That cell is for a first run on your own machine
while its seats take work from you alone; before a seat takes work from anyone
else, its runs move into a container ([Code
sandbox](#code-sandbox-where-every-executor-runs)).

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
| **Anthropic's** (Claude) | `model: sonnet` — Claude Code is the default `agent` | a Claude Pro / Max plan, or an Anthropic API key (`auth.mode: api-key`) | The one CLI that mints a headless token (`crewlet llm login -capture-token`), the best sign-in for a container. Runs a seat's executor as [its own run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box). |
| **OpenAI's** (GPT) | `agent: codex` | a ChatGPT Plus / Pro plan, or an OpenAI API key (`auth.mode: api-key`) | No executor: its own loop on this host, the seat's tools as its only working tools, for a seat's reviewer, workers and side passes; the seat's `llm` points at a Claude Code or OpenCode entry for its executor. |
| **Open models** — OpenRouter, a hosted provider, your own server | `agent: opencode`, `model: <provider>/<model>` | the provider's key in `env`, or a login of OpenCode's own; nothing at all for a server that takes no key | OpenCode 2. Fully self-hostable: a vLLM, llama.cpp or Ollama server is a provider declared in OpenCode's own configuration ([how](../concepts/llm-backends.md#open-models-openrouter-and-local-servers-opencode)). Runs a seat's executor, like Claude Code. |
| **Gemini, Qwen, Cursor, Copilot, Grok, Muse, Kimi, Hermes, Pi** | `agent:` the profile's name | the vendor's login or key | No executor: a seat's reviewer, workers and side passes. [Supported CLIs](../concepts/llm-backends.md#supported-clis) pairs each profile with its binary (`gemini-cli` drives `gemini`, `kimi-code` drives `kimi`, …). |

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

**And the CLI runs the turn itself.** Every agent seat's executor is its CLI's
own agentic loop — a real shell, a real editor, a real checkout — with the
seat's tools reaching it over an MCP bridge, as a detached run that outlives
the turn. It needs more than a sign-in: a CLI with a coding-agent runner
(`claude-code` or `opencode`) in the seat's chain, a `providers.sandbox`
catalogue to place the run in, and a bridge its box can dial — a `direct` box
dials the node's own listener on loopback, while a `container` or `e2b` box,
or any box in a fleet whose node serves no API, needs `CREWLET_MCP_BRIDGE_URL`,
the fleet's address. `crewlet llm doctor` checks all three. Every other phase —
the reviewer, delegated workers and the side passes — runs the same CLI on this
host, its shell and editor denied, calling the seat's tools over MCP. See
[LLM Backends § Where the CLI runs](../concepts/llm-backends.md#where-the-cli-runs).

**Embeddings** (`providers.embeddings`) power the semantic half of a Doclet
or Worklet search (the company's own pages and work items, embedded once for
the whole fleet, and each search's query) and the agent-learning subsystem's
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
[Deployment](../guides/deployment.md) for sizing and broker authentication.

---

## Work-item tracker + knowledge base

Agents file and pick up work in a tracker, and search a shared knowledge base.

**You do not have to bring either one.** The engine ships its own, Worklet and
Doclet, and they are the default:

```yaml
tracker:
  backend: worklet     # the default
knowledge:
  backend: doclet      # the default
```

That is the whole setup. Items and pages live in the fleet's own store, there
is a board and a page browser on the dashboard, seats get twenty-three tools for
them — eighteen over the tracker and five over the pages — and your own AI
assistant can reach them over
[`/operator/mcp`](../reference/api-endpoints.md#operatormcp--your-own-assistant).
Nothing to create, nothing to provision, no per-seat accounts, no webhook.

### Which to choose

| | Worklet and Doclet | Atlassian |
|---|---|---|
| Setup | none | a site, a project, a space, a per-seat account each, webhooks |
| Where the record lives | your own deployment | Atlassian's |
| People can use it | through the Crewlet dashboard | through Jira and Confluence, which they may already live in |
| Workflows, custom fields, sprints, permission schemes | no | yes |
| Existing tickets | none — it starts empty | whatever you already have |

**Take Worklet and Doclet unless you have a reason not to.** The reasons are real
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
> that sets `knowledge.backend: doclet` *and* declares
> `integrations.confluence` is refused. "What do we already know about this"
> must not depend on which searcher was asked. See
> [Knowledge System](../concepts/knowledge-system.md).
>
> An empty `backend` **derives**: declare `integrations.confluence` and you get
> `confluence`; declare nothing and you get `doclet`. So an Atlassian company
> that has not read this page still searches Confluence.

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

## Code sandbox (where every executor runs)

Every agent seat's executor is its coding CLI's own run — Claude Code or
OpenCode — fully permissioned, with a shell, a filesystem and a git checkout,
in a box from this catalogue, which a company with an agent seat and a model
must configure. `direct` is the engine host itself; `container` and `e2b` keep
the run off it. See the [Code Sandbox](../concepts/code-sandbox.md) concept
page.

| Option | How | Notes |
|---|---|---|
| **None** | omit `providers.sandbox` | Only for a company with no agent seat, or no `providers.llm` entry yet; any other is refused at `providers.sandbox`. |
| **E2B cloud** | `e2b: {api_key: "${E2B_API_KEY}"}` + `CREWLET_MCP_BRIDGE_URL` | Sign up at <https://e2b.dev>, create an API key in the dashboard. The run is on E2B's network and calls its seat's tools back at `CREWLET_MCP_BRIDGE_URL`, so the engine's API has to be published where E2B's cloud reaches it — see the caveat below. |
| **Self-hosted E2B** | `e2b: {api_key: …, domain: "${E2B_DOMAIN}"}` + `CREWLET_MCP_BRIDGE_URL` | Deploy [e2b-dev/infra](https://github.com/e2b-dev/infra) on your own cloud account, then point the same SDK/code path at it via `domain`. Your cluster issues its own `E2B_API_KEY`, and the bridge address has to be one your cluster reaches. |
| **Local — container** (recommended on one host) | `local: {image: …}` + an entry's `run_in: container` (or `default_run_in`) + `CREWLET_MCP_BRIDGE_URL` | Docker/Podman on the engine host. Real host isolation, no E2B account. You [bring the image](../concepts/code-sandbox.md#bring-your-own-image), with the coding CLI in it, or a stock one plus a setup step that installs it. A loopback `CREWLET_MCP_BRIDGE_URL` reaches the box under the runtime's name for the engine host, and on Linux `api.host` must not be loopback. Can use a [plan's login](../concepts/llm-backends.md#authentication) instead of an API key. |
| **Local — direct** | `local: {}` + an entry's `run_in: direct` (or `default_run_in`) | A process tree on the engine host, using the CLI (and login) already installed there. Fastest to stand up and the natural pair for an entry signed in on a plan — but the coding agent runs fully permissioned as the engine user, so it isolates *state*, not the host: it can read whatever your account can. Only on a dedicated host you are willing to hand to an autonomous agent, for the entry that needs the host itself, or for a first run on your own machine while its seats take work from you alone. |

For any other local instance, a workstation included, and before a seat takes work from anyone else, put code work in a container:

```yaml
providers:
  sandbox:
    local:
      image: node:22-bookworm       # plus the setup step that installs the CLI:
                                    #   see "Bring your own image"
    default_run_in: container
```


`providers.sandbox` is a **catalogue**, so these are not exclusive: configure
`e2b:` and `local:` together and give each `providers.llm` entry its own
`run_in`, pointing each seat at the entry that is right for it — the entry
signed in on the host's plan runs `direct`, the entry for work that must never
touch that host runs `e2b`. Where an entry that names none goes is
`default_run_in`, which is required whenever some seat's executor entry would
fall to it on a catalogue offering more than one cell (and `local:` alone
offers two); a company whose every such entry names its own needs none — as
long as *something* names a cell, since a backend is only built for a cell
that is reached.

Coding-agent choice: the coding agent **is** the CLI of the entry the seat's
executor launches on (the first member of its `llm` chain that can run one),
with that entry's model, its `env` and its sign-in, so it needs no extra secret.
**Claude Code** runs a `claude-code` entry; **OpenCode** — version 2, the
`@opencode/cli` package, in the box — runs an `opencode` entry and so any model
OpenCode reaches. A seat whose `llm` chain holds no `claude-code` or `opencode`
entry is [refused](../concepts/code-sandbox.md#failure-modes); a CLI with no
coding-agent runner — Codex, Gemini CLI, the rest — serves the seat's reviewer,
workers and side passes instead. A sign-in the seat carries itself in `role.sandbox.env` — a Claude Code
token, say — counts toward the check that refuses a remote box its entry's login
cannot follow.

One networking caveat for local development: a **cloud** E2B sandbox cannot
reach anything on your laptop, and that includes the engine. Every `e2b` run
calls all of its seat's tools back through the bridge at
`CREWLET_MCP_BRIDGE_URL`, so that has to be an address E2B's cloud reaches —
the engine's API published to the internet, its
[public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api)
behind your proxy, or a tunnel to it — and never `localhost` or `127.0.0.1`.
A proxy or tunnel on your laptop that connects to the engine on `127.0.0.1`
has to send it a loopback `Host` (ngrok's `--host-header=rewrite`,
cloudflared's `httpHostHeader: localhost`, Caddy's
`header_up Host {upstream_hostport}`): the bridge answers a call that reaches
it on the loopback under a `Host` that is neither loopback nor a container
runtime's name for the engine host with `403 Forbidden: invalid Host header`
([The tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)).
Left unset, every `e2b` launch is refused; set to an address the VM cannot
reach, or through a hop that keeps the public `Host`, every call the run makes
over the bridge fails, so the run has none of its seat's tools, cannot call
`submit_work`, and is rescued as `incomplete`.
Services on your laptop the run would use directly (`localhost` GitLab) need
the same: a reachable deployment, a tunnel, or self-hosted E2B on the same
network.

---

## Putting it together

Two bundled **Nimbus examples** model the same seven-seat company at opposite
ends of this page. `examples/nimbus.company.yaml` + `examples/nimbus.config.yaml`
is the **reference**: a pick from every row — GitLab, Mattermost, open models
through OpenCode on an OpenRouter key, a sandbox its engineers push merge
requests from, and
Worklet and Doclet rather than a vendor's. Read it to
see what a full stack looks like written out, and
[Jira](../integrations/jira.md) / [Confluence](../integrations/confluence.md)
for the blocks that move those two halves to Atlassian.

`examples/nimbus-claude-cli.company.yaml` + `examples/nimbus-claude-cli.config.yaml`
is the **short path**: every pick that costs nothing extra to stand up — chat
on **Mattermost**, the model **Claude Code on your own Claude plan**, the
tracker and knowledge base the **engine's own**, and the code sandbox on the
**engine host** (`run_in: direct`), which reuses that same plan login
rather than needing an account of its own — a first run on your own machine
while its seats take work from you alone. Every seat runs that CLI's
[own run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box) as
its executor, so the executor *is* the CLI's own agentic loop with a real shell. The rows that need
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
