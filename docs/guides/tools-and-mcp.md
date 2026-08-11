# Tools & MCP Integration

Agents interact with external systems and internal engine operations through tools. Crewlet supports both built-in tools and dynamically discovered MCP tools.

---

## Built-in Tools

These tools are registered globally and available to all agents:

| Tool | Description |
|------|-------------|
| `lookup_colleague` | Resolve any agent identifier (handle, Slack user, Jira ID, GitHub login, …) to a canonical handle and its known cross-platform identities. Falls back to case-insensitive / substring / fuzzy match (e.g. `ceo` → `agent-ceo`); ambiguous fuzzy queries return the candidate list so the LLM picks rather than guessing |
| `reflect_and_persist` | Capture a durable fact in the agent's private diary (LONG / SHORT) |
| `refresh_memory` | Re-run the personal-memory filter mid-turn after gathering richer context |
| `query_episodes` | Search the agent's own past completed turns by similarity |
| `use_skill` | Load one of the agent's own [synthesized skills](../concepts/agent-learning.md#5-skillsynthesizer--skill-induction) on demand |
| `refine_skill` | Append a bullet to a synthesized skill (or replace its body) |
| `mark_onboarded` | Stamp the agent's onboarding marker after reading the relevant onboarding pages |
| `a2a_ask` | Tight-loop synchronous handoff to a colleague (see [Turn Engine § Colleague-surface tools](../concepts/turn-engine.md#colleague-surface-tools)) |

Note the deliberate split between personal and shared writes: `reflect_and_persist` is **personal-only** (it writes to the agent's private `agent_diary`), while team-shared content lives in the knowledge backend (Confluence or Plane) — written through that backend's own MCP tools and searched live at query time (see [Knowledge System](../concepts/knowledge-system.md)). `use_skill` resolves the agent's own synthesized skills; shared procedures are knowledge-backend pages.

Task-management tools (`create_task`, `assign_task`, `update_task`, `list_tasks`, `delegate`) are **not** registered as builtins — agents interact with the external PM tool (Jira, Plane, etc.) via MCP tools instead.

### Per-Role MCP Servers (GitHub)

Roles with GitHub credentials in `mcp_env.github` get a per-role instance of the [remote GitHub MCP server](https://github.com/github/github-mcp-server) (declared as a `shared: false` `http` entry in `mcp_servers`), giving them the full GitHub toolset for reading/reviewing/tracking code (issues, PRs, repos, code search, actions); code authoring goes through the [code sandbox](../concepts/code-sandbox.md). See [GitHub Integration](../integrations/github.md).

### Tool Protocol

```python
class Tool(Protocol):
    name: str
    description: str
    parameters: dict  # JSON Schema

    async def execute(self, params: dict, context: AgentContext) -> ToolResult: ...
```

---

## MCP Integration

Instead of building hardcoded API wrappers for external tools (Jira, Slack, Confluence, GitHub, etc.), Crewlet uses the **Model Context Protocol (MCP)** for dynamic tool discovery. This gives agents access to the full capabilities of external tools — not just a curated subset.

### Architecture

```
┌──────────────────────────────────────────────────────────┐
│                    Crewlet Engine                         │
│                                                          │
│  ┌──────────────┐     ┌──────────────────────────────┐   │
│  │  MCPToolBridge│────▶│  MCPClient (stdio)           │   │
│  │              │     │  Launches MCP server as child │   │
│  │  Manages all │     │  process, communicates via    │   │
│  │  MCP servers │     │  JSON-RPC 2.0 over stdin/out  │   │
│  │  and wraps   │     └──────────────────────────────┘   │
│  │  discovered  │                                        │
│  │  tools as    │     ┌──────────────────────────────┐   │
│  │  Crewlet     │────▶│  MCPHttpClient (HTTP/SSE)    │   │
│  │  Tool        │     │  Connects to remote MCP      │   │
│  │  protocol    │     │  server via URL              │   │
│  └──────────────┘     └──────────────────────────────┘   │
│         │                                                │
│         ▼                                                │
│  ┌──────────────────────────────────────────────────┐    │
│  │  MCPToolWrapper[]                                │    │
│  │  Each discovered tool becomes a Crewlet Tool     │    │
│  │  registered in the ToolRegistry or per-role map  │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

### Two Transport Modes

1. **Stdio (Crewlet launches the server)** — The engine spawns MCP servers as child processes (e.g., `npx @anthropic/mcp-atlassian`). Crewlet manages the full lifecycle: start on engine boot, stop on shutdown. Communication uses JSON-RPC 2.0 over stdin/stdout.

2. **HTTP/SSE (externally provided)** — The engine connects to an already-running MCP server via URL. Supports JSON and SSE response modes with automatic session management and reconnection.

### Handshake and server diagnostics

On connect the engine (identifying itself as `crewlet` in the handshake) negotiates the newest protocol the server speaks: it probes the modern `server/discover` method first and falls back to the legacy `initialize` handshake automatically. Servers built on older MCP SDKs may log a one-time "unknown method" warning when they see the probe — harmless, and it stays out of your console because of the rule below.

A stdio server's **stderr is never passed through raw**. Every line the child process writes (startup banners, tracebacks, that probe warning) becomes a structured `server_stderr` DEBUG event attributed to the server, instead of foreign log lines interleaving with the engine's own stream. When a server fails to start, the last lines it wrote are surfaced with the failure as a single `server_stderr_tail` ERROR event — that tail usually names the real cause (bad token, missing binary, import error). Run with `--debug` to watch a server's full stderr live.

---

## Per-agent identity

Each Role names its per-server credentials directly in `mcp_env`, so every agent authenticates as itself in external tools. The engine applies these as **env vars** for `stdio` servers and **HTTP headers** for `http` servers — it stays tool-agnostic, reading only `mcp_env` (and, for the Slack transport, `integrations.slack`):

```yaml
roles:
  - name: Senior Engineer
    integrations:                          # per-agent transport identity (inbound webhook
      slack:                               #   verification + outbound send() fallback)
        bot_token: "${ALICE_SLACK_BOT}"
        signing_secret: "${ALICE_SLACK_SIGNING}"
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ALICE_JIRA_USER}"
        JIRA_API_TOKEN: "${ALICE_JIRA_TOKEN}"
      slack:
        SLACK_MCP_XOXB_TOKEN: "${ALICE_SLACK_BOT}"   # same token, the Slack MCP subprocess
      github:
        Authorization: "Bearer ${ALICE_GH_TOKEN}"
```

A Slack-enabled agent names its bot token in **both** `role.integrations.slack.bot_token` and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`: the two are different consumers — the notification transport vs. the Slack MCP subprocess — and both reference the same `${VAR}`, so no secret is duplicated. The Atlassian token (`JIRA_API_TOKEN` / `CONFLUENCE_API_TOKEN`) and the GitHub PAT (`Authorization: Bearer …`) likewise live wherever the consuming MCP server reads them.

When the engine launches a per-role MCP server instance, it merges the base server config with the role's `mcp_env` (applied as **env vars** for `stdio` servers, **HTTP headers** for `http` servers).

### Per-Unit Config

A unit declares its Jira project / Confluence space *identity* under `integrations` (used for inbound webhook routing and as the team's write home — not a tool credential, and it does not scope knowledge reads). Real per-agent tool credentials still live in `mcp_env`, which all roles inherit:

```yaml
units:
  - name: Backend
    type: team
    lead: Tech Lead
    integrations:
      jira:
        project: "BACK"             # the unit's Jira project (integration identity)
    mcp_env:
      atlassian:
        JIRA_URL: "${JIRA_URL}"     # shared by the whole unit
    roles:
      - name: Tech Lead
        mcp_env:
          atlassian: { JIRA_API_TOKEN: "${TL_JIRA_TOKEN}" }
      - name: Engineer
        mcp_env:
          atlassian: { JIRA_API_TOKEN: "${ENG_JIRA_TOKEN}" }
```

Inheritance: the unit's `mcp_env` is the base, role values override per key. The unit's `integrations` identity (Jira project / Confluence space) is separate from these credentials.

---

## Global vs Per-Role MCP Servers

- **Global servers** — launched once, shared by all agents. Tools are registered in the global `ToolRegistry`. Use for servers that don't need per-agent identity (e.g., a shared knowledge base).
- **Per-role instances** — launched with the role's merged env vars. Tools are stored in `_role_mcp_tools` and only available to agents with that role. Use for servers where each agent needs its own identity (e.g., Jira, Slack, GitHub).

---

## YAML Configuration

**All** tool servers go in `mcp_servers` — including the Jira/Confluence (`atlassian`), Slack, and GitHub servers. The `integrations.jira` / `.confluence` / `.slack` / `.github` sections carry only non-tool config (admin credentials, webhook secrets) — MCP servers are never declared there. Per-agent identity comes from `role.mcp_env[name]` — env vars for `stdio` servers, HTTP headers for `http` servers:

```yaml
mcp_servers:
  # stdio, shared by all agents
  - name: tavily
    command: npm
    args: ["exec", "--yes", "--", "tavily-mcp@latest"]
    env: { TAVILY_API_KEY: "${TAVILY_API_KEY}" }
  # stdio, per-role (Jira + Confluence share one mcp-atlassian)
  - name: atlassian
    shared: false
    command: uvx
    args: ["mcp-atlassian"]
    env: { JIRA_URL: "https://mycompany.atlassian.net" }
  # stdio, per-role Slack
  - name: slack
    shared: false
    command: npm
    args: ["exec", "--yes", "--", "slack-mcp-server@latest", "--transport", "stdio"]
    tool_prefix: "slack_"
  # http, per-role remote GitHub MCP (token supplied per agent)
  - name: github
    transport: http
    shared: false
    url: "https://api.githubcopilot.com/mcp/"

roles:
  - name: Senior Engineer
    integrations:
      slack: { bot_token: "${ALICE_SLACK_BOT}", signing_secret: "${ALICE_SLACK_SIGNING}" }
    mcp_env:
      atlassian: { JIRA_API_TOKEN: "${ALICE_JIRA_TOKEN}" }
      slack:     { SLACK_MCP_XOXB_TOKEN: "${ALICE_SLACK_BOT}" }
      github:    { Authorization: "Bearer ${ALICE_GH_TOKEN}" }
```

Environment variables and HTTP header values support `${VAR}` references that resolve from `os.environ` at startup — both whole-value (`"${TOKEN}"`) and embedded (`"Bearer ${TOKEN}"`) — so secrets stay out of config files.

### Tool annotation overrides

The engine derives some behaviour from a tool's [capabilities](../concepts/tool-capabilities.md) — for example, the sub-agent guard denies tools that write to a shared surface, classified from the MCP `readOnlyHint` / `destructiveHint` / `openWorldHint` annotations the server advertises. Most servers advertise these; for one that doesn't, supply them per server via `tool_annotations` (keyed by bare tool name):

```yaml
mcp_servers:
  - name: linear
    command: uvx
    args: ["mcp-linear"]
    tool_annotations:
      linear_create_comment: { read_only: false, open_world: true }
      linear_get_issue:      { read_only: true }
```

Keys accept snake_case (`read_only`) or the MCP camelCase (`readOnlyHint`); overrides win over whatever the server advertised. Because every tool server is now an `mcp_servers` entry — including `atlassian`, `slack`, and `github` — `tool_annotations` is declared there for all of them. This is the *only* place tool names appear in config for behaviour purposes — the engine itself never hardcodes them. See [Tool Capabilities](../concepts/tool-capabilities.md) for the full mechanism.

---

## How Tool Calls Work

From the LLM's perspective, builtin tools and MCP tools are identical — both appear as JSON schema tool definitions. The LLM doesn't know whether a tool is a Python function or an MCP server talking to Jira.

**Tool resolution order** (in `_execute_tool`):

1. **Per-role MCP tools** — checked first (role-specific credentials)
2. **Global tools** — builtin tools + global MCP tools

**Tool output is returned to the LLM in full.** `sanitize_tool_output` strips control characters and `validate_tool_result` redacts secrets / rejects binary, but results are **never length-truncated** — a truncated result silently hides content the agent reasons over (the tail of a `list_mcp_server_tools` listing, for example, where the tool the agent needs may sort past any cap). The same principle applies across the engine: phase / aux telemetry, the agent diary (stored content), coalesced notification digests, and the Review / extension-judge tool logs all carry their full text. The one deliberate bound is the *embeddings* input (diary writes, similarity-query keys), trimmed only because the embeddings provider has a hard token limit — the stored/displayed text stays complete.

See [Agent Runtime](../concepts/agent-runtime.md) for the full execution loop.
