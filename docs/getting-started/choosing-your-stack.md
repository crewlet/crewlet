# Choosing Your Stack

Crewlet is the engine; the LLM, tracker, knowledge base, code host, chat and code sandbox are services you pick and connect, hosted or self-hosted. Per choice: what it implies, what you create **yourself**, where setup lives.

**Only the LLM, and a place for its runs, is required.** The tracker, knowledge base and chat ship with the engine and are on by default. Every seat's executor runs in a [code sandbox](#code-sandbox-where-every-executor-runs) cell; `local: {}` with `default_run_in: direct` is the engine host itself, so one signed-in Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Copilot, Grok Build, Muse Code, Kimi Code, Hermes Agent or Pi plus that cell runs a company. Use it only for a first run on your machine while seats take work from you alone; before a seat takes work from anyone else, move its runs into a container ([Code
sandbox](#code-sandbox-where-every-executor-runs)).

Each integration usually has:

1. **Something only you can create**: an Atlassian site, Slack workspace, GitLab group. Crewlet never creates top-level tenancy.
2. **Per-agent identities inside it**: service accounts, bot apps, tokens. A provisioning CLI creates them idempotently for Mattermost, Slack and GitLab; for Atlassian and GitHub you create them by hand, because neither issues a credential on a provisioner's behalf. Those two still have a harness that reports which account each seat's credential turned out to be, and registers the webhooks.
3. **A webhook back to the engine**, so external activity wakes the right agent: self-registered where the API allows, manual otherwise. Mattermost has no usable inbound webhook, so the engine dials out and needs no reachable address.

---

## LLM provider (required)

**Every model is reached through the vendor's own harness**, which the engine drives as a process, signed in with your plan or key. Choose the vendor and how you pay:

| Models | Config | Pay with | Notes |
|---|---|---|---|
| **Anthropic's** (Claude) | `model: sonnet` — Claude Code is the default `harness` | a Claude Pro / Max plan, or an Anthropic API key (`auth.mode: api-key`) | Only harness that mints a headless token (`crewlet llm login -capture-token`), the best sign-in for a container. Runs a seat's executor as [its own run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box). |
| **OpenAI's** (GPT) | `harness: codex` | a ChatGPT Plus / Pro plan, or an OpenAI API key (`auth.mode: api-key`) | Codex CLI 0.171.0. Runs a seat's executor ([its runner](../concepts/code-sandbox.md#codex)). |
| **Open models** — OpenRouter, a hosted provider, your own server | `harness: opencode`, `model: <provider>/<model>` | the provider's key in `env`, or an OpenCode login; nothing for a keyless server | OpenCode 2.3.2. Self-hostable: a vLLM, llama.cpp or Ollama server is a provider declared in OpenCode's own configuration ([how](../concepts/llm-backends.md#open-models-openrouter-and-local-servers-opencode)). Runs a seat's executor. |
| **GitHub's** (Copilot) | `harness: copilot` | a fine-grained PAT with Copilot Requests in `COPILOT_GITHUB_TOKEN` (`auth.mode: api-key`), or a Copilot login | GitHub Copilot CLI 1.0.114, the release supported. Runs a seat's executor ([its runner](../concepts/code-sandbox.md#github-copilot-cli)). |
| **Moonshot's** (Kimi) | `harness: kimi-code` | a Moonshot API key in `KIMI_MODEL_API_KEY` (`auth.mode: api-key`), or a Kimi Code login | Kimi Code CLI 2.7.2. Runs a seat's executor ([its runner](../concepts/code-sandbox.md#kimi-code)). |
| **xAI's** (Grok) | `harness: grok` | an xAI API key in `XAI_API_KEY` (`auth.mode: api-key`), or a Grok login | Grok Build 1.0.92. Runs a seat's executor ([its runner](../concepts/code-sandbox.md#grok-build)). |
| **Gemini, Qwen, Muse, Hermes, Pi** | `harness:` the profile's name | the vendor's login or key; Hermes and Pi take the model provider's key in `env`, or their own login | Each runs a seat's executor ([the runners](../concepts/code-sandbox.md#harnesses)): Gemini CLI 0.70.0, Qwen Code 0.28.4, Muse Code 1.4.25, Hermes Agent 0.23.4 ([its runner](../concepts/code-sandbox.md#hermes-agent)), Pi 1.6.0 ([its runner](../concepts/code-sandbox.md#pi)). |
| **Cursor** | `harness:` the profile's name | the vendor's login or key | No executor: reviewer, workers, side passes. [Supported harnesses](../concepts/llm-backends.md#supported-harnesses) pairs each profile with its binary (`cursor-agent` drives `cursor-agent`, …). |

The harness's binary must be installed on the engine host; the container image ships none ([Installation](installation.md#a-harness)). Each seat gets an isolated home in it, so agents never share a harness's memory. See [LLM Backends](../concepts/llm-backends.md).

Configure several named entries and pick per role (`role.llm`), plus a cheap auxiliary model per role (`role.llm_auxiliary`) for reflection and summarisation; each call pays a harness launch under a fixed deadline ([why](../concepts/llm-backends.md#auxiliary-work-and-latency)). `role.llm` also accepts a **list** composing a plan and a key: `llm: [plan, key]` runs on the flat-rate plan and falls through to the metered key while the plan's window is spent. See [Quickstart § LLM options](quickstart.md#llm-options).

**Which to pick.** A key suits a fleet that must be fast and always available: billed per token, throttled far less than a plan. A plan suits developing, evaluating or a small company you run yourself, at predictable cost; check its terms, generally written for interactive use by the subscriber. Same harness, so it is one `auth.mode`, changeable any time. A plan can also back the [code sandbox](../concepts/code-sandbox.md): a box in either cell is seeded with the entry's login, or handed a headless token.

**The harness runs the turn itself.** Every seat's executor is its harness's own agentic loop (real shell, editor, checkout), with the seat's tools reaching it over an MCP bridge, as a detached run that outlives the turn. It needs a harness with a runner (`claude-code`, `codex`, `gemini-cli`, `qwen-code`, `opencode`, `copilot`, `grok`, `muse-code`, `kimi-code`, `hermes` or `pi`) in the seat's chain, a `providers.sandbox` catalogue to place the run in, and a bridge its box can dial: a `direct` box dials the node's own loopback listener wherever the node binds `api.port`; a `container` box, or a `direct` one on a node started with `api.port: 0`, needs `CREWLET_MCP_BRIDGE_URL`, the fleet's address. `crewlet llm doctor` checks all three. Every other phase (reviewer, delegated workers, side passes) runs the same harness on this host, shell and editor denied, calling the seat's tools over MCP. See [LLM Backends § Where the harness runs](../concepts/llm-backends.md#where-the-harness-runs).

**Embeddings** (`providers.embeddings`) power the semantic half of a Doclet or Worklet search (pages and work items embedded once for the fleet, plus each query) and the agent-learning subsystem's similarity recall (personal diary + episode recall). They are the one model call the engine makes itself, since a harness computes no vectors: any OpenAI-compatible endpoint works via `base_url`, self-hosted included. A model this build does not know states its width and limits (tokens per input; inputs and tokens per request), as do the request limits of `gemini-embedding-001` and `embed-v4.0`, whose compatible endpoints document none; see [Configuration](configuration.md#providers). Without a provider the engine still runs: search is keyword only, personal memory comes from a seat's recent notes alone, and similar-prior-work recall renders nothing.

---

## Core infrastructure

**There is none to choose.** The engine is one binary: it embeds a NATS JetStream server as its event stream, and owns two local files as its store. One host runs a whole company in development and production.

Two slots change once a deployment outgrows one node:

| Slot | Options |
|---|---|
| **Stream** | `embedded` (default) — and a fleet is the *same* embedded server with `stream.cluster.name/port/peers` naming its peers and `stream.replicas: 3` · or `nats` + `stream.url` to dial a NATS server or cluster somebody else runs |
| **Coordination** | `local` (one node) · `embedded-kv` (a fleet — one node or three; two has no quorum and is refused by name) |

Coordination takes no address: its KV (leases, ledgers, token counter) lives on the stream's **own** broker. On the embedded broker it holds a second connection beside the queue's, and the node's holds ride a lease connection of their own on every broker. Each is watched: any closing for good stops the node, so it cannot keep renewing leases while its inbox link is gone.

The **store** stays one file per node, so everything shared lives in coordination. See [Running a Fleet](../guides/fleet.md) and [Deployment](../guides/deployment.md) for sizing, and [Deployment § The fleet's broker admits only its own nodes](../guides/deployment.md#the-fleets-broker-admits-only-its-own-nodes) for broker authentication: every route, leaf link and operator connection to an embedded fleet's broker presents a key derived from `secrets.keys`.

---

## Work-item tracker + knowledge base

Agents file and pick up work in a tracker and search a shared knowledge base. **You need bring neither**: the engine ships Worklet and Doclet, the default:

```yaml
tracker:
  backend: worklet     # the default
knowledge:
  backend: doclet      # the default
```

That is the whole setup. Items and pages live in the fleet's store, the dashboard has a board and a page browser, seats get twenty-four tools for
them (nineteen over the tracker and five over the pages), and your own AI assistant reaches them over
[`/operator/mcp`](../reference/api-endpoints.md#operatormcp--your-own-assistant). No provisioning, per-seat accounts or webhook.

### Which to choose

| | Worklet and Doclet | Atlassian |
|---|---|---|
| Setup | none | a site, a project, a space, a per-seat account each, webhooks |
| Where the record lives | your own deployment | Atlassian's |
| People can use it | through the Crewlet dashboard | through Jira and Confluence, which they may already live in |
| Workflows, custom fields, sprints, permission schemes | no | yes |
| Existing tickets | none — it starts empty | whatever you already have |

**Take Worklet and Doclet unless you have a reason not to.** The reasons concern people, not agents: a team already in Jira should not watch a second board, and an existing backlog does not migrate (no migration path, below). Otherwise the vendor path costs a provisioning afternoon and buys nothing the agents use.

A unit's `project` and `space` name its project and container on whichever backend runs, which is why the fields are not called `jira_project` and `confluence_space`.

> **No migration between them.** Switching `tracker.backend` moves nothing, either way, and the engine does not offer to: a half-migrated tracker is worse than either and undetectable from outside. Choose once, per company.

### Atlassian (Jira + Confluence Cloud or Data Center)

Atlassian runs the tracker and wiki; agents work them through per-agent Atlassian identities. Setup is manual: Atlassian has no service-account-provisioning API a CLI can drive.

What **you** do, by hand:

1. **Create the Atlassian site** (or use yours); Crewlet never creates the tenancy.
2. **Create per-agent identities**: an Atlassian account (or API token identity) per agent seat, so issues can be assigned to agents and comments attribute correctly. Mint one API token per agent and reference it from
   `role.mcp_env`, beside the account's address (`JIRA_USERNAME` and
   `JIRA_API_TOKEN`, `CONFLUENCE_USERNAME` and `CONFLUENCE_API_TOKEN` for the
   `atlassian` MCP server, which refuses a token without its address), plus
   one admin/service token for the engine's org-level lookups
   (`integrations.jira.token`).
3. **Webhooks**:
   - **Cloud**: install the [Crewlet Forge app](https://github.com/crewlet/forge) in your site; it forwards Jira + Confluence events to
     `POST /webhooks/forge` (signature-verified; needs the `forge` install
     extra).
   - **Data Center**: register webhooks directly against
     `POST /webhooks/jira` / `POST /webhooks/confluence` with an HMAC secret.
4. **Create the spaces/projects** your units use (`project` / `space` per unit) and an `Onboarding` page per space.

Details: [Jira](../integrations/jira.md) ·
[Confluence](../integrations/confluence.md).

> **One knowledge backend per company**, enforced by validation: `knowledge.backend: doclet` *and* `integrations.confluence` is refused, so "What do we already know about this" never depends on which searcher was asked. See [Knowledge System](../concepts/knowledge-system.md).
>
> An empty `backend` **derives**: declare `integrations.confluence` and you get `confluence`; declare nothing and you get `doclet`. So an Atlassian company that never read this page still searches Confluence.

---

## Code host

Engineer roles read, review and track code through per-role MCP tools, and author changes through the [code sandbox](../concepts/code-sandbox.md) under their **own identities**, so MRs/PRs come from the agent.

### Option A: GitLab — gitlab.com or self-hosted

Per-agent identities are API-provisionable end to end, so one CLI run sets up the fleet. [GitLab Integration](../integrations/gitlab.md#walkthrough-nimbus-against-local-gitlab) adds a local GitLab to a copy of the bundled Nimbus example:

1. **Create the top-level group** (you), e.g. `gitlab.com/your-group`, or run a self-hosted GitLab (any modern one; a local instance ships as the compose `gitlab` profile for end-to-end testing). Set `integrations.gitlab.url`; the same config shape covers both.
2. **Run the provisioner**: `crewlet gitlab provision company.yaml
   -public-url https://<engine>` creates one **service
   account per engineering seat** (mentionable, assignable, reviewer-able), group/project memberships, per-agent PATs minted into your config's own
   `${VAR}` references, and the webhooks. Needs an admin-capable operator token; see the
   [permission matrix](../integrations/gitlab.md#permission-matrix--the-operator-credential).
   On gitlab.com see the [prerequisites](../integrations/gitlab.md#prerequisites-gitlabcom)
   (service accounts need a paid tier; self-hosted has no such gate).
3. Agents drive GitLab via the `glab` CLI's MCP server (`glab mcp serve`, spawned per role with that role's PAT); the sandbox git-auth recipe makes
   `git push` + MR creation work headlessly under the agent's identity.

Details: [GitLab integration](../integrations/gitlab.md).

### Option B: GitHub

github.com or a GitHub Enterprise Server: leave `integrations.github.url` unset for the former, name the instance for the latter:

1. **Create the org/repos** (you), plus **one PAT per engineer seat**. GitHub has no API-provisionable service accounts, so create machine users or fine-grained PATs by hand
   (`role.mcp_env.github` carries `Authorization: Bearer ${GITHUB_TOKEN_X}`). The engine derives each seat's login from its token; nothing is declared.
2. **Register the webhooks** with
   [`crewlet github provision`](../reference/cli.md#crewlet-github-provision),
   which mints the secret and registers one organization hook where the credential may (covering later repositories) or one per repository where it may not, and reports which account each seat turned out to be, the finding that decides whether the integration routes at all.
3. Agents get the full toolset of the hosted [GitHub MCP server](https://github.com/github/github-mcp-server) per role; the sandbox git-auth recipe has a GitHub form (credential helper on
   `github.com` + `GITHUB_TOKEN` in `role.sandbox.env`); see
   [GitHub integration](../integrations/github.md).

A company can run **both** hosts, with different repositories on each.

### Option C: Gitlet — `code.backend: gitlet`

Gitlet, the engine's own git host, keeps repositories on its state log, with each push's bytes in the [object store](../concepts/object-store.md): set `code.backend: gitlet` and declare the repositories in `code.repos`, and every activation creates, settles and retires them (see
[Gitlet](../concepts/gitlet.md)). It is **not exclusive**: GitLab and GitHub integrations keep working beside it.

It is a place for **people and agents** to keep and review code. People clone, fetch and push over HTTPS with their own API token ([Hosting
Code on Gitlet](../guides/gitlet.md)); every push is recorded as whoever made it. Using it is a permission, as with Worklet: a person reaches the code only while they hold **Gitlet**, which an administrator ticks, so a company can keep its source from somebody it keeps off its tracker.
Agents change it through their code tools (a small commit, a revert or a pick, a branch, each built by the engine and recorded as the seat's push) or push from a coding box's clone with their run's own credential ([Coding
boxes](../concepts/gitlet.md#coding-boxes)). A **change** proposes one branch into another; people and agents review it, raising findings, a blocker among them holding it back until cleared ([Review a
change](../guides/gitlet.md#review-a-change)). The repository's home Gitlet node queues its checks, and a change merges as its branch's gate allows, through the merge queue where the gate requires a check ([The merge
queue](../concepts/gitlet.md#the-merge-queue)). Read [What it does not do
yet](../concepts/gitlet.md#what-it-does-not-do-yet) before choosing it as an engineering team's only host. In production take the [S3 object store](../concepts/object-store.md#an-s3-bucket--s3), since repositories grow as long as the company works in them.

---

## Chat

Chat is the human↔agent surface (DMs, channels, escalations) and where the DACI [decision framework](../concepts/decision-framework.md) plays out. **Three backends ship, one of them the engine itself.** The choice is one exclusive field; see [Chatlet](../concepts/chatlet.md) for why two live chat homes is not offered:

```yaml
chat:
  backend: chatlet   # the default — the engine is the chat system
# backend: vendor    # Slack or Mattermost
# backend: none      # no chat at all
```

| | Chatlet | Mattermost | Slack |
|---|---|---|---|
| Hosting | none — it is the engine | self-hosted, open source | SaaS |
| Credentials per agent | none | 1 (bot token) | 2 (bot token + signing secret) |
| Manual steps per agent | none | none | one OAuth **Allow** click |
| Engine must be publicly reachable | no | no — the engine dials out | yes — the Events API POSTs to it |
| Where people read it | the Crewlet dashboard | the Mattermost app | the Slack app |
| Notified while away | **no** — unread counts and a mention feed only | yes, by Mattermost | yes, by Slack |
| File attachments | no — links only | yes | yes |
| Search | keyword, over the engine's own index | the vendor's | the vendor's |
| Working status | free text, per turn phase | fixed *"is typing…"* (`always` by default; `addressed` is cheaper) | free text, per turn phase |

**Chatlet is the default, right for a company that will not connect a workspace** (where the data would sit, per-seat cost, or a handful of people and a dozen agents for whom a chat product priced for humans is the wrong shape). Nothing to stand up, provision or credential: channels, threads, DMs, mentions, reactions and search are in the dashboard once the engine runs. Give a unit a `channel` and the engine opens that room with the unit's seats in it.

**Know this first:** the engine never sends anything as itself, so nobody is emailed, pushed or paged; a person learns they were mentioned when they next open the dashboard. If that is unacceptable, choose a vendor, for this row alone.

**Mattermost is the quickest vendor to try.** It ships in this repo's `docker-compose.yml`, provisioning is one non-interactive command, and nothing has to reach the engine, so a laptop can go from nothing to an agent answering in a channel with no account or tunnel ([First run, end to
end](../integrations/mattermost.md#first-run-end-to-end)).

**Slack is where most companies already are.** If yours is, choose it regardless: agents appear where conversations happen, under your workspace admin, SSO, retention and compliance setup, with nothing new to host. It also renders the per-phase working indicator as real text, which Mattermost has no API for.

Mattermost and Slack at once is supported: each seat carries whichever identities it needs (a company migrating between them), which is why the field's value is `vendor`. Chatlet beside either is **not** supported and is refused at validation: a reply in one place would answer a thread the agents read half of, and no rule could say which conversation was real.

### Option A: the engine's own chat (no setup)

Nothing. `chat.backend` unset with no vendor chat block derives `chatlet`. Give each unit a room:

```yaml
units:
  - name: Engineering
    channel: engineering       # lowercase letters, digits, `-` and `_`
```

The name is an **address**, not a rendering of one: write `engineering`, never
`#engineering` and never a vendor's opaque id. The engine opens that room with every seat in the unit's subtree as a member and hands the name to the agents to type into their tools.

The founder's policy (how long messages are kept, whether new channels are private, when a working seat shows it (`typing_status`)) is the optional `chat.chatlet` block in
[Configuration § Chat](configuration.md#chat). The default keeps messages for a year; `0` keeps them for ever, and chat is the highest-volume thing the engine stores, so read
[Retention](../guides/retention.md) first. How much of a thread a woken seat is shown is not in it: the engine decides that once for every chat surface (see
[the thread a turn was woken in](../concepts/agent-runtime.md#the-thread-a-turn-was-woken-in)).

### Option B: Mattermost (self-hosted)

1. **Run the Mattermost server yourself** (official Docker image; it needs its own PostgreSQL) and **create the team** agents will live in. Crewlet never creates top-level tenancy.
2. **Give the integration a system-admin token**, once, under
   `integrations.mattermost.provisioning.admin_token`, and declare the Mattermost MCP tool server in `mcp_servers`.
3. **Turn each agent on** in the Mattermost integration's
   [Agents tab](../integrations/mattermost.md#each-agents-access-roles-and-channels),
   with its roles and channels. The engine creates one bot account per agent, adds it to the team and its channels, and seals its access token under the name it derives for the seat.

Nothing needs to reach the engine from outside: it opens outbound websockets per seat rather than receiving webhooks, so no tunnel or public URL.
Details: [Mattermost integration](../integrations/mattermost.md).

### Option C: Slack

There is no self-hosted variant:

1. **Create the Slack workspace yourself** (or use your company's).
2. **Declare each agent's Slack identity in the company YAML**: the per-agent bot token + signing secret as `${VAR}` placeholders under
   `role.integrations.slack`, and the shared Slack MCP tool server in
   `mcp_servers`. Each agent is its own bot identity: own token, own DM, own @-mention.
3. **Let the engine create the apps.** Enter one app *configuration token* on Slack's connect form, under Company › Integrations, and turn each agent on from the Slack integration's
   [Agents tab](../integrations/slack.md#per-agent-access-from-the-agents-tab).
   The engine creates one app per agent through Slack's App Manifest APIs, points each app's event subscriptions at `POST /webhooks/slack/{handle}`, and seals what Slack serves under names it derives from the handle. Two things stay manual, since Slack has no API for either: generating the configuration token (once, ever) and clicking **Allow** on each app's install. The engine's API must be reachable from Slack (public URL, or a tunnel in development) for both the events-URL verification and the OAuth landing page.

Clicking through [api.slack.com/apps](https://api.slack.com/apps) per agent still works; see
[Manual Setup](../integrations/slack.md#manual-setup).

Details (scopes, Events API, thread routing, the working-status indicator):
[Slack integration](../integrations/slack.md).

---

## Code sandbox (where every executor runs)

Every seat's executor is its harness's own run (Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, GitHub Copilot CLI, Grok Build, Muse Code, Kimi Code, Hermes Agent or Pi), fully permissioned, with a shell, filesystem and git checkout, in a box from this catalogue, which a company with an agent seat and a model must configure. `direct` is the engine host itself; `container` keeps the run off its files. A `direct` or `container` box runs the harness as the engine's own user, and Claude Code refuses its executor's mode for root, so the engine must not run as root there ([the engine's user](../guides/deployment.md#the-engines-user-and-claude-code)). See the [Code Sandbox](../concepts/code-sandbox.md) concept page.
| Option | How | Notes |
|---|---|---|
| **None** | omit `providers.sandbox` | Only for a company with no agent seat, or no `providers.llm` entry yet; any other is refused at `providers.sandbox`. |
| **Local — container** (recommended on one host) | `local: {image: …}` + an entry's `run_in: container` (or `default_run_in`) + `CREWLET_MCP_BRIDGE_URL` | Docker/Podman on the engine host. Real host isolation. You [bring the image](../concepts/code-sandbox.md#bring-your-own-image), with the harness in it, or a stock one plus a setup step that installs it. A loopback `CREWLET_MCP_BRIDGE_URL` or `CREWLET_BOX_GIT_URL` reaches the box under the runtime's name for the engine host, and on Linux `api.host` must not be loopback. Can use a [plan's login](../concepts/llm-backends.md#authentication) instead of an API key. |
| **Local — direct** | `local: {}` + an entry's `run_in: direct` (or `default_run_in`) | A process tree on the engine host, using the harness (and login) already installed there. Fastest to stand up and the natural pair for an entry signed in on a plan — but the harness runs fully permissioned as the engine user, so it isolates *state*, not the host: it can read whatever your account can. Only on a dedicated host you are willing to hand to an autonomous agent, for the entry that needs the host itself, or for a first run on your own machine while its seats take work from you alone. |

For any other instance, a workstation included, and before a seat takes work from anyone else, put code work in a container:

```yaml
providers:
  sandbox:
    local:
      image: node:22-bookworm       # plus the setup step that installs the harness:
                                    #   see "Bring your own image"
    default_run_in: container
```


`providers.sandbox` is a **catalogue**: `local:` serves both cells, and each `providers.llm` entry may name its own `run_in`. The entry whose harness needs the host's own reach runs `direct`; the rest run in `container`. An entry naming none goes to `default_run_in`, required when some seat's executor entry would fall to it (`local:` offers two cells, and neither is chosen for you). A company whose every such entry names its own needs none, as long as *something* names a cell: a backend is built only for a cell that is reached.

Harness choice: the executor's harness **is** the harness of the entry its seat launches on (the first member of the `llm` chain that can run one), with that entry's model, `env` and sign-in, so it needs no extra secret. **Claude Code** runs a `claude-code` entry, **Codex** (0.171.0 or later, in the box) a `codex` entry, **Gemini CLI** a `gemini-cli` entry, **Qwen Code** a `qwen-code` entry, **OpenCode** (version 2, the `@opencode/cli` package, in the box) an `opencode` entry and so any model OpenCode reaches, **GitHub Copilot CLI** a `copilot` entry, **Grok Build** a `grok` entry, **Muse Code** a `muse-code` entry, **Kimi Code** a `kimi-code` entry, and **Pi** a `pi` entry. A seat whose `llm` chain holds none of the ten is [refused](../concepts/code-sandbox.md#failure-modes); a harness with no harness runner (Cursor Agent, the rest) serves the seat's reviewer, workers and side passes instead.

---

## Putting it together

The bundled **Nimbus example** (`examples/nimbus.company.yaml` + `examples/nimbus.config.yaml`) is a seven-seat company making the pick on every row of this page that needs **no outside account**: chat on **Chatlet**, work items on **Worklet**, pages on **Doclet**, every repository on **Gitlet** (the engine's own, each), the model **Claude Code on your own Claude plan**, and a sandbox on the **engine host** (`run_in: direct`) reusing that plan login, where engineers check repositories out of Gitlet and propose work as changes. Every seat runs the harness's [own run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box) as its executor, with a real shell: a first run on your own machine while its seats take work from you alone. The one other key it reads is `OPENAI_API_KEY`, for embeddings. It comes up from the binary and one `crewlet llm login`:

```bash
export CREWLET_SECRET_KEY_2026_01="<the key `crewlet secrets keygen -key-id 2026-01` prints>"
export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
crewlet llm login default -from-host \
    -company examples/nimbus.company.yaml \
    -config examples/nimbus.config.yaml
export OPENAI_API_KEY="<for-embeddings>"
export CREWLET_BOX_GIT_URL="http://localhost:8000/box/git"   # where a box reaches Gitlet
crewlet run -config examples/nimbus.config.yaml \
            -company examples/nimbus.company.yaml
```

**Every vendor row is additive; each page shows the blocks to add to a copy of the example.** A code host ([GitLab](../integrations/gitlab.md) or [GitHub](../integrations/github.md)) runs beside Gitlet. Chat, the tracker and the wiki are each one axis, so a vendor there *replaces* the engine's own in the same edit: [Mattermost](../integrations/mattermost.md#first-run-end-to-end) or [Slack](../integrations/slack.md) means `chat.backend: vendor` (Chatlet beside a vendor chat is refused); [Jira](../integrations/jira.md) / [Confluence](../integrations/confluence.md) mean `tracker.backend: jira` / `knowledge.backend: confluence`.

Each example block carries its rationale in comments, including the choices it did *not* make and what adding them takes. Add the other rows one at a time from the integration pages; nothing in the example has to be undone first.
