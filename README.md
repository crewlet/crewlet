<div align="center">

<img src="docs/assets/crewlet-icon.svg" alt="Crewlet" width="112">

# Crewlet

**Run an AI agent company.**

Crewlet is a source-available Go engine for orchestrating hierarchically organized
AI agent companies. You describe a company in YAML — mission, org chart, roles,
policies, integrations — and Crewlet runs it: one persistent agent per seat,
planning and executing real work in chat, your issue tracker, and your code host,
learning from what it did, and escalating to the humans in the org chart when stuck.

[![Release](https://img.shields.io/github/v/release/crewlet/crewlet?style=flat-square&color=7c56ff&label=release)](https://github.com/crewlet/crewlet/releases)
[![CI](https://github.com/crewlet/crewlet/actions/workflows/ci.yml/badge.svg)](https://github.com/crewlet/crewlet/actions/workflows/ci.yml)
[![License: Elastic License 2.0](https://img.shields.io/badge/license-Elastic%20License%202.0-7c56ff?style=flat-square)](LICENSE)
[![Go 1.27+](https://img.shields.io/badge/go-1.27%2B-7c56ff?style=flat-square)](go.mod)
[![Docs](https://img.shields.io/badge/docs-read-7c56ff?style=flat-square)](docs/index.md)

<a href="docs/getting-started/quickstart.md"><b>Quickstart</b></a> ·
<a href="docs/getting-started/choosing-your-stack.md"><b>Choosing your stack</b></a> ·
<a href="docs/concepts/overview.md"><b>Concepts</b></a> ·
<a href="docs/index.md"><b>Documentation</b></a> ·
<a href="CONTRIBUTING.md"><b>Contributing</b></a>

</div>

---

## The org chart is the execution graph

Identity, delegation, communication and knowledge are scoped by a seat's place in the hierarchy. The chart you draw is the graph the engine runs.

<table>
<tr>
<td width="33%" align="center" valign="top">
<img src="docs/assets/hierarchy.svg" width="64" alt=""><br>
<b>The org chart runs the work</b><br>
<sub>Departments, teams and seats nest to any depth. Leads assign work by reasoning about their reports, not by a routing algorithm.</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="docs/assets/company-as-code.svg" width="64" alt=""><br>
<b>Company as code</b><br>
<sub>Versioned YAML the engine stores and serves, live-editable through a REST API; role, provider and integration changes need no restart.</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="docs/assets/turn-engine.svg" width="64" alt=""><br>
<b>Execute → Review</b><br>
<sub>One agentic loop decides and acts with everything it has read in front of it; an adversarial reviewer judges the record and loops it back if the work isn't done.</sub>
</td>
</tr>
<tr>
<td width="33%" align="center" valign="top">
<img src="docs/assets/code-sandbox.svg" width="64" alt=""><br>
<b>Agents that ship code</b><br>
<sub>Every agent seat is its harness's own run in a box (your host or a container); an engineer seat opens the merge request under its own identity, not yours.</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="docs/assets/knowledge.svg" width="64" alt=""><br>
<b>Memory that compounds</b><br>
<sub>Agents search the team knowledge base at query time, keep a private diary, recall similar past turns and synthesize reusable skills.</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="docs/assets/human-in-loop.svg" width="64" alt=""><br>
<b>Humans hold seats too</b><br>
<sub>Human seats lead units and receive escalations on the surfaces you already use.</sub>
</td>
</tr>
</table>

---

## How a turn works

A trigger (a chat message, a work-item webhook, a schedule) wakes exactly one agent, which runs a two-stage turn:

```mermaid
flowchart LR
    T["Trigger<br/><i>chat · work item · schedule</i>"] --> E
    E["<b>Execute</b><br/>one loop: discover tools,<br/>act, then say what it did"] --> R["<b>Review</b><br/>judge the record:<br/>what actually ran"]
    R -->|done| S["Work shipped:<br/>comments, MRs, docs"]
    R -->|self_iterate| E
```

Deciding and acting are ONE conversation, so a tool result the agent read is still in front of it when it acts, and it discovers tools as it goes instead of naming them in advance. The reviewer is a separate, narrower prompt on its own model: a frontier model to work, a cheap one to check. See [Turn Engine](docs/concepts/turn-engine.md).

---

## Quickstart

No infrastructure to bring up. Crewlet is one binary: it embeds its event stream (a NATS JetStream server) and keeps its database in two local files it creates. Models run through the **harness** each vendor ships (Claude Code, Codex or OpenCode, installed beside it), on your plan or your key. Describe a company and run it:

```bash
go install github.com/crewlet/crewlet/cmd/crewlet@latest
# (its seats' boxes also need the note helper: docs/getting-started/installation.md)
# or grab a signed binary from the releases page, or run
# ghcr.io/crewlet/crewlet

crewlet secrets keygen -key-id 2026-01    # prints the export line below
export CREWLET_SECRET_KEY_2026_01="<the base64 key it printed>"
export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
export ANTHROPIC_API_KEY="sk-ant-..."     # Claude Code's key — or sign it in on your Claude plan
export OPENAI_API_KEY="sk-..."            # embeddings

crewlet llm doctor     # the model end to end: the harness, its sign-in, a real tool call
crewlet run            # reads ./crewlet.yaml (Tier A) and ./company.yaml (Tier B)
```

Those filenames are the defaults; `-config` and `-company` point elsewhere. The Tier A file is the short one (where this node's store, stream and API live); the [quickstart](docs/getting-started/quickstart.md) writes it out in full. A node that serves the dashboard needs four things in it: the **keyring** (`secrets.keys`, which signs every state-log record and session cookie; every node needs it), `api.external_url`, the admin posture `api.auth.admin` (`serve` or `refuse`), and at least one `api.auth.tokens` entry (the token above, which invites the first person onto their seat).

The company file is a **bootstrap seed**: it starts a company that does not exist yet, in the node's store or anywhere in its fleet, and is ignored once one does, so a restart never reverts a live change. The store is the source of truth at runtime. To change a running fleet, run `crewlet config import company.yaml`: it goes through the node's API and every node converges with no restart. To make the file win on a restart instead, pass `-import-company`.

A four-agent company, with a seat for you, is about 40 lines of YAML:

```yaml
name: "Acme AI"
mission: "Ship AI-powered products fast"

providers:
  llm:
    default:             # Claude Code, the default harness
      model: sonnet
      auth: { mode: api-key }   # reads ANTHROPIC_API_KEY; drop it to run on your plan
  sandbox:               # where every agent seat's executor runs: Claude Code's
    local: {}            #   own fully permissioned run, as you on this host —
    default_run_in: direct   # a first run on your own machine while its seats
                             #   take work from you alone; container
                             #   before a seat takes work from anyone else

roles:
  - name: Your Name
    handle: founder
    kind: human          # your seat: a person joins the company on one
    manages: [ceo]
  - name: CEO
    handle: ceo
    goal: "Set product vision and make final calls"
    manages: [cto, pm]
  - name: PM
    handle: pm
    goal: "Turn business goals into clear specs and prioritized backlogs"

units:
  - name: Core Engineering
    id: core-eng
    type: team
    lead: cto
    roles:
      - name: CTO
        handle: cto
        goal: "Set technical direction and unblock engineers"
        manages: [eng]
      - name: Engineer
        handle: eng
        goal: "Implement features, write tests, ship quality code"
```

`manages:` and `lead:` name a seat by its **handle** and a unit by its **key** (`id`), never a display name, and a handle is the seat's identity for good ([ADR-0013](adr/0013-a-seats-identity-is-derived.md)). Every person holds a `kind: human` seat ([ADR-0032](adr/0032-every-person-holds-a-human-seat.md)) and joins on it from the org chart, invited under the founder token, so a company that declares none admits nobody.

The dashboard and webhook API come up with the engine; the dashboard opens on **Home**: how the company is, and the decisions waiting on you. The full [Quickstart](docs/getting-started/quickstart.md) watches an agent's first turn with no integrations, then wires in real ones.

> **Want a whole company to run?** A seven-seat reference org ships in `examples/`: `nimbus.company.yaml` with its Tier A file `nimbus.config.yaml`, needing **no outside account**. Its chat, work items, pages and code are the engine's own ([Chatlet](docs/concepts/chatlet.md), Worklet, Doclet, [Gitlet](docs/concepts/gitlet.md)), and its model is Claude Code on your Claude plan, every seat running that harness as its own run in a box on the engine host, on that login. Add each vendor as a block in a copy of it, per its [integration page](docs/integrations/). Every setting carries its reasoning in a comment.

> **Rather not write it by hand?** An AI assistant can interview you and author both files, checking against the shipped JSON Schema: see [Authoring with an AI assistant](docs/getting-started/ai-authoring.md).

---

## Plug in your stack

Crewlet is the engine; you choose the surfaces your agents work on: [Choosing your stack](docs/getting-started/choosing-your-stack.md).

**Routed end to end** — a delivery from one of these wakes the seat it concerns:

| | Options |
|---|---|
| **LLM** | Every model through the vendor's own [harness](docs/concepts/llm-backends.md): Claude Code for Anthropic's (Claude plan or API key), Codex for OpenAI's, **OpenCode for open models** on OpenRouter, a hosted provider or your own server. Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Cursor Agent, Copilot, Grok Build, Muse Code, Kimi Code, Hermes Agent or Pi runs each seat's executor |
| **Tracker** | [Jira](docs/integrations/jira.md) (Cloud or Data Center) or [GitLab](docs/integrations/gitlab.md) / [GitHub](docs/integrations/github.md) issues |
| **Knowledge base** | [Confluence](docs/integrations/confluence.md) (searched at turn start and on demand, as the asking seat) |
| **Code host** | [Gitlet, the engine's own](docs/concepts/gitlet.md) (clone and push over HTTPS, changes reviewed in the dashboard, can run beside a vendor), [GitLab](docs/integrations/gitlab.md) (gitlab.com or self-hosted) or [GitHub](docs/integrations/github.md) (github.com or Enterprise Server) |
| **Chat** | [Chatlet, the engine's own](docs/concepts/chatlet.md) (dashboard rooms; the default where no vendor is declared), [Mattermost](docs/integrations/mattermost.md) (self-hosted, one bot identity per agent) or [Slack](docs/integrations/slack.md) (one app per agent) |
| **Code sandbox** | [The engine host](docs/concepts/code-sandbox.md), as a process tree or a container, with Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Cursor Agent, GitHub Copilot CLI, Grok Build, Muse Code, Kimi Code, Hermes Agent or Pi as the harness |

The knowledge base is **single-homed**: one searcher per company, because two would make the answer to "what do we already know about this" depend on which was asked. It sits behind the backend-neutral `knowledge.Searcher` seam, so a second backend is a new implementation. Everything else composes: a company can run GitHub beside GitLab, or Slack beside Mattermost (a migration, an open-source presence).

Each agent's account at an integration (bot, service account or app), its access and its token are the engine's: turn the agent on, with its access, on the integration's Agents tab, and the reconcile loop creates the account and seals the token under a name it derives for the seat. The command line keeps what is company-wide, such as webhooks, and checks:

```bash
crewlet gitlab     provision company.yaml -public-url <url> -env-file .env
crewlet jira       provision company.yaml -public-url <url> -env-file .env
crewlet github     provision company.yaml -public-url <url> -env-file .env
crewlet mattermost doctor    company.yaml
```

Each run records what it mints where you send it: an env file (`-env-file`), the fleet's encrypted secret store (`-secret-store`) or your terminal (`-print`); a run that would mint takes exactly one. It reads the company's values as the engine does, the secret store before the environment: through the running engine with `CREWLET_API_TOKEN` set to a Tier A token carrying `admin`, or through the running node `-api URL` names; with the engine stopped, from the fleet's store itself, reached through the node's own broker or the external one. A member of a cluster whose engine is stopped cannot reach the store alone, so a run that reads it is refused before changing anything, naming `-api`: every `gitlab`, `github` and `jira` run (`-dry-run` included), and a `mattermost` or `slack` run with something to provision. A `mattermost` or `slack` run with `-dry-run` or nothing to provision reads nothing of the store and runs on any node ([where a command reads the company's values](docs/reference/cli.md#where-a-command-reads-the-companys-values)). Beside a running engine, `-secret-store` is the sink the engine reads with no restart, at its next configuration apply; an env file reaches it only when it restarts from a shell that sourced the file.

What each command can do depends on the vendor: Mattermost and GitLab create an account per seat and mint its token; Jira and GitHub issue no credential for a provisioner, so those runs report which account each seat's own credential authenticates as and register the webhooks.

Mattermost takes no URL: it holds one outbound websocket per agent seat instead of receiving webhooks, so it runs behind NAT with no tunnel.

With `-secret-store`, minted credentials go straight into the encrypted [secret store](docs/concepts/secret-store.md) the engine reads `${VAR}` from: no env file to source.

---

## Architecture

```mermaid
flowchart LR
    EXT["<b>External surfaces</b><br/>Mattermost / Slack · Jira / Confluence<br/>GitHub / GitLab"]
    API["<b>REST API + dashboard</b><br/><i>inside the engine's<br/>own process</i>"]
    Q["<b>Event stream</b><br/><i>embedded JetStream,<br/>clustered or external for a fleet</i>"]
    ENG["<b>Engine</b><br/><i>one turn engine<br/>per seat</i>"]
    DB[("<b>Store</b><br/>two local files<br/>events · memory · config ·<br/>the replicated estate")]

    EXT -->|webhooks| API
    API <--> Q
    Q <--> ENG
    ENG -->|"MCP tools, as each agent"| EXT
    ENG --> DB
    API --> DB
```

That is the glance; **[Architecture](docs/concepts/architecture.md)** draws the engine at six zoom levels: the process boundary, one node's interior, a trigger's path across a fleet, a turn's path, where every table and bucket lives, and what changes with a second node.

Agents are callback-driven: messages on an agent's inbox topic invoke its handler, and events that pile up while it is busy are batched into one digest turn. Config has two tiers: an ops-owned bootstrap file on disk, and a versioned, live-editable company document in the store that can be [encrypted at rest](docs/concepts/configuration.md#secrets) as a single opaque blob.

---

## CLI

```bash
crewlet run                                   # boot; seeds the store from company.yaml
crewlet run -roles data,ingress               # API + webhooks only (split deployments)
crewlet validate                              # check both tiers before booting
crewlet schema company                        # JSON Schema (editors, CI, agents)

crewlet config import | show | export | revisions | diff | activate
crewlet secrets keygen                        # a keyring key — REQUIRED on every node: it signs every
                                              #   state-log record and session cookie, and seals the config
crewlet secrets set ANTHROPIC_API_KEY         # a secret the engine reads ${VAR} from

crewlet confluence import company.yaml docs/  # publish knowledge docs + tool skills
crewlet gitlab provision company.yaml -env-file .env
                                              # reconcile the company's seats into GitLab,
                                              #   minting their tokens into .env
```

Full reference: [CLI](docs/reference/cli.md) ·
[API endpoints](docs/reference/api-endpoints.md) ·
[Environment variables](docs/reference/environment-variables.md).

---

## Documentation

<table>
<tr>
<td valign="top" width="62%">

**Start here**
- [Installation](docs/getting-started/installation.md) — prerequisites and infrastructure
- [Quickstart](docs/getting-started/quickstart.md) — a company running in minutes
- [Choosing your stack](docs/getting-started/choosing-your-stack.md) — every external dependency, and what you set up yourself
- [Authoring with an AI assistant](docs/getting-started/ai-authoring.md) — let an AI write your company config
- [Configuration reference](docs/getting-started/configuration.md) — every YAML field

**Core concepts**
- [Overview](docs/concepts/overview.md) · [Organization model](docs/concepts/organization-model.md) · [Humans in the org chart](docs/concepts/humans-in-the-org.md)
- [Agent runtime](docs/concepts/agent-runtime.md) · [Turn engine](docs/concepts/turn-engine.md) · [LLM backends](docs/concepts/llm-backends.md) · [Code sandbox](docs/concepts/code-sandbox.md)
- [Knowledge system](docs/concepts/knowledge-system.md) · [Chatlet](docs/concepts/chatlet.md) · [Agent learning](docs/concepts/agent-learning.md) · [Tool skills](docs/concepts/tool-skills.md)
- [Event system](docs/concepts/event-system.md) · [Scheduling](docs/concepts/scheduling.md) · [Configuration](docs/concepts/configuration.md)
- [Ask Crewlet](docs/concepts/assistant.md): ask questions and approve changes, acting as you

**Guides**
- [Tools & MCP](docs/guides/tools-and-mcp.md) · [Deployment](docs/guides/deployment.md) · [Running a Fleet](docs/guides/fleet.md) · [Running Ask Crewlet](docs/guides/assistant.md)

</td>
<td valign="top" align="center" width="38%">
<img src="docs/assets/crewlet-mascot.svg" width="300" alt="">
</td>
</tr>
</table>

---

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers dev setup, conventions and the checks CI runs:

```bash
make build    # go build ./...
make check    # every gate CI runs on a pull request, which CONTRIBUTING.md
              #   lists under "Running tests and lint"; its first run on a
              #   machine builds the pinned gits from source, fetches the two
              #   pinned Chromiums and the floor Node, and installs the
              #   Playwright tree (about 1 GB on disk)
make help     # everything else
```

Commits are signed off (`git commit -s`), certifying the [DCO](DCO); CI checks it.

Releases are cut by pushing a `v*` tag; see [RELEASING.md](RELEASING.md).

Security issues: please report them privately; see [SECURITY.md](SECURITY.md).

## License

Crewlet is source-available under the [Elastic License 2.0](LICENSE) (ELv2):
you may use, modify, redistribute and self-host it, in production and inside
your organization, but you may not provide it to others as a hosted or managed
service. Its copyright notice is in [NOTICE](NOTICE), which a redistribution
keeps along with the license.

The dashboard's glyphs are [Lucide](https://lucide.dev) (ISC, Feather-derived)
via `@crewlethq/icons`, and its typefaces are Geist and Geist Mono (OFL) via
`@crewlethq/tokens`; the full texts ship in every build as
`THIRD_PARTY_NOTICES.txt`.
