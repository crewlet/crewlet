# Environment Variables

All environment variables used by Crewlet and its integrations.

Two kinds of variable appear below. A few names are **read directly by the engine or CLI** (marked as such). Everything else is a **`${VAR}` reference convention**: any string value in the YAML config can reference any environment variable (see [Usage in YAML](#usage-in-yaml)), and the names listed here are simply the conventions the bundled [`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml) uses — rename them freely as long as the config references match.

---

## Core

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_NODE_ID` | This process's identity, when `node.id` is unset in the Tier A file. Labels every log line, health payload, and config-apply event. Must be **stable across restarts**; defaults to `node-0` | Your orchestrator (Kubernetes pod name / StatefulSet ordinal, or the host name) |
| `CREWLET_STORE_DRIVER` | Which certified driver opens the store file — `turso` (the default) or `sqlite`. Overridden by `store.driver` in the Tier A file | Leave unset unless you are exercising the fallback driver |
| `CREWLET_API_TOKEN_FOUNDER` | Bearer token for the founder API identity (`api.auth.tokens`) | Generate one: `openssl rand -hex 32` |
| `LLM_API_KEY` | API key for your LLM provider (`providers.llm.default.api_keys`) | Your LLM provider dashboard |
| `LLM_MODEL` | Model id served by your OpenAI-compatible endpoint (`providers.llm.default.model` in the example) | Your LLM provider docs |
| `LLM_BASE_URL` | Your OpenAI-compatible endpoint's `/v1/` base URL (`providers.llm.default.base_url` in the example) | Your LLM provider docs |
| `OPENAI_API_KEY` | Read directly as a fallback by the `openai` / `openai-compatible` LLM providers (when `api_keys` is empty) and the OpenAI embeddings provider (when `api_key` is unset) | OpenAI dashboard |
| `ANTHROPIC_API_KEY` | Read directly as a fallback by the `anthropic` LLM provider when `api_keys` is empty | Anthropic Console |
| `CREWLET_LLM_CLI_HOME` | Read directly by every [`cli-agent`](../concepts/subscription-llm-backends.md) LLM provider: the root under which each provider keeps its credential directory and per-seat CLI homes (`<root>/<provider key>`). Default `~/.crewlet/llm-cli`. Point it at a persistent volume when the engine runs in an ephemeral container, or the subscription login is lost on every restart. Overridden per provider by `cli.state_dir`. | — |
| `CLAUDE_CODE_OAUTH_TOKEN` | Read as the subscription credential by a `cli-agent` provider on the `claude-code` profile when `cli.auth.token` is unset. Resolved through the [secret store](../concepts/secret-store.md) first, so `crewlet llm login <key> --capture-token` stores it there and nothing needs exporting. | `claude setup-token`, or `crewlet llm login … --capture-token` |
| `CREWLET_LLM_CLI_<KEY>_CREDENTIALS` | Conventional name for a `cli-agent` provider's exported credential bundle (`<KEY>` is the `providers.llm` key upper-cased, non-alphanumerics folded to `_`). Restored into the provider's credential directory at boot when that directory is empty, so a fresh container comes up already authenticated. Overridden by `cli.auth.credential_bundle`. | `crewlet llm export <key> --secret-store` |
| `CREWLET_SANDBOX_LOCAL_HOME` | Read directly by the [`local` sandbox backend](../concepts/code-sandbox.md#local-sandboxes): the parent directory its per-box homes are created under. Default `~/.crewlet/sandboxes`. Overridden per provider by `providers.sandbox.local.state_dir`. | — |
| `CREWLET_TOOL_SKILLS_SPACE` | Read directly by the engine and `crewlet confluence import`: the Confluence space key the [Tool Skills](../concepts/tool-skills.md) sync watches. Default `TS`. Set to empty string to disable the sync entirely. | — |
| `CREWLET_TOOL_SKILLS_PROJECT` | The `--project` default for `crewlet plane import`: which [Plane](../integrations/plane.md) project [Tool Skill](../concepts/tool-skills.md) pages are PUBLISHED into. Default `TS`. The engine reads the project it WATCHES from `integrations.plane.skills_project` instead — a routing decision belongs in the document describing the company — so set the two to the same value. | — |

---

## Slack

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `SLACK_BOT_TOKEN_<ROLE>` | Bot User OAuth Token (`xoxb-...`) | Written by `crewlet slack provision`, or Slack app > OAuth & Permissions |
| `SLACK_SIGNING_SECRET_<ROLE>` | Signing Secret | Written by `crewlet slack provision`, or Slack app > Basic Information |
| `SLACK_CONFIG_TOKEN` | App configuration token (`xoxe.xoxp-...`, 12 h lifetime) authenticating the App Manifest APIs used by [`crewlet slack provision`](../integrations/slack.md) | [api.slack.com/apps](https://api.slack.com/apps) > Your App Configuration Tokens (once); rotated near expiry + persisted to the env file automatically |
| `SLACK_CONFIG_REFRESH_TOKEN` | Refresh token (`xoxe-1-...`) minting the next config-token pair via `tooling.tokens.rotate` | Issued alongside `SLACK_CONFIG_TOKEN`; rotated automatically |
| `SLACK_CONFIG_TOKEN_EXPIRES_AT` | Unix timestamp of the access token's expiry — lets re-runs skip rotation while the token is fresh | Written by `crewlet slack provision`; never set by hand |

Replace `<ROLE>` with the role name in uppercase (e.g., `SLACK_BOT_TOKEN_ENGINEER`). The per-role names are conventions — any `${VAR}` name referenced from `role.integrations.slack` works, and `crewlet slack provision` writes whatever names the YAML uses.

---

## Jira

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `JIRA_URL` | Your Jira instance URL | e.g., `https://company.atlassian.net` |
| `JIRA_API_TOKEN` | Jira API token (admin/service account) | Atlassian account > API tokens |
| `JIRA_EMAIL` | Admin email for Cloud Basic Auth | Your Atlassian account email |
| `JIRA_WEBHOOK_SECRET` | Secret for HMAC verification | Set when creating the Jira webhook |

### Per-Agent Jira Tokens

| Variable | Description |
|----------|-------------|
| `<ROLE>_JIRA_TOKEN` | Per-agent Jira API token (e.g., `CTO_JIRA_TOKEN`) |

---

## Confluence

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CONFLUENCE_URL` | Your Confluence instance URL (`integrations.confluence.url`) | e.g., `https://company.atlassian.net/wiki` |
| `CONFLUENCE_API_TOKEN` | Admin/service API token (`integrations.confluence.token`) | Atlassian account > API tokens |
| `CONFLUENCE_EMAIL` | Admin email for Cloud Basic Auth (`integrations.confluence.email`) | Your Atlassian account email |
| `CONFLUENCE_WEBHOOK_SECRET` | HMAC secret for Data Center webhooks (`integrations.confluence.webhook_secret`) | Set when creating the webhook |

Per-agent Confluence credentials go through `role.mcp_env` on the `atlassian`
MCP server (`CONFLUENCE_USERNAME` / `CONFLUENCE_API_TOKEN`), like Jira.

---

## Web Search (optional)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `TAVILY_API_KEY` | Key for the shared Tavily web-search MCP server the example org declares | <https://tavily.com> |

---

## Plane

Conventions used by the [Plane integration](../integrations/plane.md) and its provisioning/bootstrap tooling. Apart from `PLANE_ADMIN_TOKEN` (read directly by the CLI), these are `${VAR}` references in the company YAML — [`crewlet plane provision`](cli.md#crewlet-plane-provision) **mints** the token values into `.env.plane` for you.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `PLANE_PUBLIC_URL` | The address browsers use to reach Plane. Read by the bundled `docker-compose.yml` (it becomes `WEB_URL` + `CORS_ALLOWED_ORIGINS`) and by `scripts/plane-dev-bootstrap.sh`; keep it in lock-step with `PLANE_URL`. Defaults to `http://localhost:8091`. | You — it is the address in your browser's address bar |
| `PLANE_URL` | Plane instance base URL — the one reference the example config resolves for `integrations.plane.url` and `skill_variables.plane_base_url` | Written to `.env.plane` by `scripts/plane-dev-bootstrap.sh` locally; your Plane deployment's URL otherwise |
| `PLANE_ADMIN_TOKEN` | Read directly by `crewlet plane provision` as the operator credential fallback (a workspace-admin personal API token; `-admin-token` overrides) | Plane profile > API tokens (workspace-admin account) |
| `PLANE_ENGINE_TOKEN` | The `crewlet-engine` read account's API token (`integrations.plane.token`) | Minted by `crewlet plane provision` |
| `PLANE_WEBHOOK_SECRET` | Workspace webhook secret (`integrations.plane.webhook_secret`) — generated by Plane at hook creation | Captured by `crewlet plane provision -public-url …` |
| `PLANE_TOKEN_<SEAT>` | Per-agent service-account API token (each role's `mcp_env.plane.PLANE_API_KEY`, e.g. `PLANE_TOKEN_CEO`) | Minted by `crewlet plane provision` |

---

## Mattermost

Conventions used by the [Mattermost integration](../integrations/mattermost.md) and its provisioning/bootstrap tooling. Apart from `MATTERMOST_ADMIN_TOKEN` (read directly by the CLI) and `MATTERMOST_PUBLIC_URL` (read by `docker-compose.yml` and the bootstrap script), these are `${VAR}` references in the company YAML — [`crewlet mattermost provision`](cli.md#crewlet-mattermost-provision) **mints** the token values into `.env` for you.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `MATTERMOST_URL` | Mattermost instance base URL (`integrations.mattermost.url`, and the `MATTERMOST_URL` each role's MCP server reads) | Written to `.env` by `scripts/mattermost-dev-bootstrap.sh` locally; your deployment's URL otherwise |
| `MATTERMOST_PUBLIC_URL` | **The address browsers use.** Read by the bundled `docker-compose.yml` (it becomes `MM_SERVICESETTINGS_SITEURL`) and by the bootstrap script. Getting it wrong costs every human live updates while the engine keeps working — see [The Site URL](../integrations/mattermost.md#the-site-url). Defaults to `http://localhost:8065`; the bootstrap defaults it to the address you reached the host on over SSH. | You — it is the address in your browser's address bar |
| `MATTERMOST_ADMIN_TOKEN` | Read directly by `crewlet mattermost provision` as the operator credential (a system-admin personal access token; `--admin-token` overrides) | Mattermost profile > Security > Personal Access Tokens (system-admin account) |
| `MATTERMOST_TOKEN_<SEAT>` | Per-agent bot personal access token (each role's `integrations.mattermost.bot_token` **and** `mcp_env.mattermost.MATTERMOST_TOKEN`, e.g. `MATTERMOST_TOKEN_PM`) | Minted by `crewlet mattermost provision` |

---

## GitLab

Conventions used by the [GitLab integration](../integrations/gitlab.md). Apart from the operator credentials (read directly by the CLI), these are `${VAR}` references in the company YAML — [`crewlet gitlab provision`](cli.md#crewlet-gitlab-provision) mints the PAT values into `.env.gitlab`.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `GITLAB_ADMIN_TOKEN` | Read directly by `crewlet gitlab provision` as the operator credential fallback (group Owner / admin PAT with `api` scope; `-admin-token` overrides) | GitLab > Access tokens |
| `GITLAB_ADMIN_TOKEN` | Second fallback for the same operator credential | GitLab > Access tokens |
| `GITLAB_ENGINE_TOKEN` | Engine read token (`integrations.gitlab.token`) | GitLab service account, or minted by provisioning |
| `GITLAB_SIGNING_SECRET` | Webhook signing token (`integrations.gitlab.signing_secret`, GitLab 19.1+ Standard-Webhooks scheme) | Set when registering the webhook |
| `GITLAB_TOKEN_<SEAT>` | Per-agent service-account PAT (each role's `mcp_env.gitlab.GITLAB_TOKEN`, also referenced from `role.sandbox.env`, e.g. `GITLAB_TOKEN_SWE`) | Minted by `crewlet gitlab provision` |

---

## Email

| Variable | Description |
|----------|-------------|
| `GMAIL_APP_PASSWORD` | Gmail app password (if using Gmail) |

---

## The store

**There is no database environment variable, because there is no database
server.** The store is a local file, named by `store.path` in the Tier A
bootstrap YAML:

```yaml
store:
  path: "/var/lib/crewlet/company.db"
```

That file is owned **exclusively** by one engine process. It is not a shared
database and there is no DSN to point anywhere; two engines opening one file
corrupt it. Everything that genuinely has to be shared between nodes — seat
leases, config activations, the completion ledger, dedupe and the rate
valves — lives in the `coordination` slot instead.

`CREWLET_STORE_DRIVER` picks which certified driver opens it (see Core above).
The event store is a table in that same file, created by the engine's own
migrations — there is no separate observability database to configure.

---

## Pulsar (Optional Auth)

Only needed when the broker runs with token authentication (see [Deployment § Authentication](../guides/deployment.md)). Both are `${VAR}` conventions — the first in the Tier A YAML, the second in the compose `.env`.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_PULSAR_TOKEN` | This engine's broker token (`providers.queue.auth_token`) | `bin/pulsar tokens create --subject <engine-role>` |
| `PULSAR_ADMIN_TOKEN` | Operator/superuser token used by the compose broker config, its healthcheck, and `pulsar-admin` | `bin/pulsar tokens create --subject admin` |

---

## Secret Encryption (Optional)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_SECRET_KEY_<ID>` | Base64-encoded 32-byte key referenced by a Tier A `secrets.keys[].material`. When a keyring is configured, the **entire** Tier B config is stored **encrypted at rest** in the DB as one opaque blob instead of as verbatim `${VAR}` references. | `crewlet secrets keygen` |

The keyring lives in Tier A (`config.yaml`) and is the sole root of trust — the DB holds only the encrypted document, never the key, and the key is required for **every** config read. Without a keyring, Crewlet keeps the default `${VAR}`-reference behaviour and every env var on this page is resolved from the environment at construction time. See [Configuration § Secrets](../concepts/configuration.md#secrets).

A keyring lets you retire the per-secret env vars on this page (`LLM_API_KEY`, `<ROLE>_JIRA_TOKEN`, `SLACK_BOT_TOKEN_<ROLE>`, `*_WEBHOOK_SECRET`, …) two different ways:

- **[Secret store](../concepts/secret-store.md)** *(recommended)* — keep the `${VAR}` references in the config and store the values in the encrypted `secret_values` table (`crewlet secrets set`, or `-secret-store` on a provisioning CLI). The engine consults that table **ahead of** the process environment, so a name it answers no longer needs to be exported at all. Rotation is an update of one row.
- **Literal values in the encrypted config** — set them via `PUT /config` or import a `company.yaml` with literals. Simpler, but every rotation writes a new immutable revision that archives the superseded secret, and one credential referenced from two places (a Slack bot token is both `role.integrations.slack.bot_token` and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`) becomes two literals that must change together.

Either way, `${VAR}` references that remain unanswered by the store still resolve from the environment.

**Nothing in Tier A can move into the store**, no matter how it is configured — `CREWLET_SECRET_KEY_<ID>` above all. Tier A is what locates and decrypts the store, so it is always env- or file-sourced; it resolves with the store deliberately switched off.

---

## OpenTelemetry (Optional)

All read directly by the engine.

| Variable | Description | Example |
|----------|-------------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP HTTP endpoint for trace export | `http://localhost:4318/v1/traces` |
| `OTEL_EXPORTER_OTLP_HEADERS` | `k=v,k2=v2` headers for the OTLP backend (e.g. auth). Also used engine-side as the upstream auth for forwarded sandbox telemetry — never handed to the sandbox itself. | `authorization=Bearer%20...` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | OTLP protocol selector, propagated into sandbox runs so the coding agent exports the same way | `http/protobuf` |

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, the engine exports OTel spans to the specified endpoint (Jaeger, Grafana Tempo, etc.). Without it, spans are still created for internal trace context propagation but not exported.

---

## Code Runtime (Sandbox, Optional)

Used only when `providers.sandbox` is configured so sandbox-enabled roles can author code in an isolated E2B sandbox — see [Code Sandbox](../concepts/code-sandbox.md). Requires the `sandbox` extra — `pip install 'crewlet[sandbox]'` (or `uv sync --extra sandbox` from a checkout) — which pulls in the `e2b` SDK. The variable names below are the conventions the [Nimbus example](../../examples/nimbus.company.yaml) references; any `${ENV}` name works.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `E2B_API_KEY` | E2B API key (`providers.sandbox.api_key`). **Required even for self-hosted/local E2B** — the SDK always authenticates (sends it as an `X-API-KEY` header); `E2B_DOMAIN` only changes *which* API it talks to. | [e2b.dev](https://e2b.dev) dashboard (cloud) or your self-hosted E2B's key management |
| `E2B_DOMAIN` | Self-hosted / local E2B cluster domain (`providers.sandbox.domain`). Omit for E2B cloud. | Your self-hosted E2B deployment |
| `E2B_VALIDATE_API_KEY` | Set to `false` to skip the SDK's `e2b_<hex>` key-format check — needed if your self-hosted cluster issues keys in a different format. Default `true`. Read directly by the `e2b` SDK from the env. | — |
| `CREWLET_SANDBOX_OTEL_RECEIVER_URL` | Read by every node: the externally-reachable base URL of whichever node serves your webhooks (an `ingress` one) (e.g. `http://host.docker.internal:80`). When set, the engine wires its `/otlp/{token}/v1/{signal}` receiver route and sandbox runs export telemetry through it (forwarded to `OTEL_EXPORTER_OTLP_*` when configured). Unset = no sandbox telemetry. | Your engine's public address |

Inside each sandbox run the engine **injects** `CREWLET_AGENT_HANDLE` and `CREWLET_AGENT_EMAIL` — the running agent's identity facts, readable by `role.sandbox.setup` recipes (e.g. to configure `git config user.name`/`user.email`). They are outputs of the engine, not inputs you set.

The coding agent's LLM credential derives from the role's resolved `providers.llm` entry — no sandbox-specific LLM secret is needed. External-service tokens are **explicit config**: each seat declares them in `role.sandbox.env` (e.g. `GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"` — by convention the same PAT the seat already uses for its `mcp_env.gitlab` server, so merge requests land under the agent's identity). The engine itself injects only the generic facts above — it never names a tool-specific variable; a declared `${ENV}` reference that resolves to empty logs `sandbox_env_unresolved` at launch. With an OpenAI-compatible LLM provider, keep `default_coding_agent: opencode` (provider-agnostic); `claude-code` additionally requires an Anthropic-compatible credential.

---

## Usage in YAML

All string values in YAML support `${ENV_VAR}` references:

```yaml
providers:
  llm:
    default:
      api_keys:                 # LLM providers take a list (one or many)
        - "${LLM_API_KEY}"
  embeddings:
    api_key: "${OPENAI_API_KEY}"  # embeddings still take a single scalar
```

Variables are resolved at startup from the [secret store](../concepts/secret-store.md) first (when one is configured and holds the name), then the process environment. An unanswered reference resolves to the empty string.

Only the braced identifier form is substituted — `${NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Bare `$NAME` and shell parameter expansions (`${1:-x}`, `${line#host=}`) are left untouched, so config-authored script content — a sandbox setup step's helper script, say — survives intact.
