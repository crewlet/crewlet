# Environment Variables

All environment variables used by Crewlet and its integrations.

Two kinds of variable appear below. A few names are **read directly by the engine or CLI** (marked as such). Everything else is a **`${VAR}` reference convention**: any string value in the YAML config can reference any environment variable (see [Usage in YAML](#usage-in-yaml)), and the names listed here are simply the conventions the bundled [`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml) uses — rename them freely as long as the config references match. (Its smaller sibling, [`examples/nimbus-claude-cli.company.yaml`](../../examples/nimbus-claude-cli.company.yaml), is chat-only on a coding-CLI subscription and so references a small subset: `MATTERMOST_*` and `CREWLET_*`.)

---

## Core

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_NODE_ID` | This process's identity, when `node.id` is unset in the Tier A file. Labels every log line, health payload, and config-apply event. Must be **stable across restarts**; defaults to `node-0` | Your orchestrator (Kubernetes pod name / StatefulSet ordinal, or the host name) |
| `TURSO_GO_CACHE_DIR` | Read directly by the `turso` driver (and by the engine, which prepares it): where its embedded ~20 MB native library is extracted and loaded from. Default `os.UserCacheDir()` — `~/.cache` on Linux. Point it at a writable, persistent path in an ephemeral container, or every restart pays the extraction again. See [Deployment § The store](../guides/deployment.md#the-store) | — |
| `CREWLET_API_TOKEN_FOUNDER` | Bearer token for the founder API identity (`api.auth.tokens`) | Generate one: `openssl rand -hex 32` |
| `LLM_MODEL` | The model the example's OpenCode entry runs, as OpenCode spells it — `<provider>/<model>`, e.g. `openrouter/<model-id>` (`providers.llm.default.model` in the example) | `opencode models --standalone` lists them |
| `OPENROUTER_API_KEY` | The key of the provider the example's model names, handed to OpenCode through the entry's `env` (`providers.llm.default.env` in the example). OpenCode reads each provider's key from that provider's own variable, so an entry naming another provider sets that provider's variable instead | OpenRouter dashboard |
| `ANTHROPIC_API_KEY` | The key a Claude Code entry under `auth.mode: api-key` is signed in with when it names no `auth.api_key` — read through the [secret store](../concepts/secret-store.md) first, then the environment, and put in the CLI's own key variable, which is this one. **Only under that mode, and only when the field names nothing:** an entry whose `auth.api_key` reference resolves to nothing runs on no key rather than on this one, and a subscription entry never hands it to the CLI, so a stray export cannot move a plan-billed seat onto metered billing | Anthropic Console |
| `CODEX_API_KEY` | The key a Codex entry under `auth.mode: api-key` is signed in with when it names no `auth.api_key` — read through the [secret store](../concepts/secret-store.md) first, then the environment, and put in the CLI's own key variable, which is this one: `codex exec` signs its requests with it, and with only `OPENAI_API_KEY` set sends no `Authorization` header at all. **Only under that mode, and only when the field names nothing:** an entry whose `auth.api_key` reference resolves to nothing runs on no key rather than on this one, and a subscription entry never hands it to the CLI. An OpenAI key kept under another name is named there — `api_key: "${OPENAI_API_KEY}"` | OpenAI dashboard |
| `OPENAI_API_KEY` | Read by the OpenAI embeddings provider when `api_key` is unset. Resolved through the [secret store](../concepts/secret-store.md) first. An embeddings `api_key` whose `${VAR}` resolves to nothing runs on no key rather than on this one. **Not a key a coding CLI entry falls back to**: Codex reads `CODEX_API_KEY` instead, and a Qwen Code entry, whose CLI does read this variable, names its key in `auth.api_key` — other vendors' keys are kept under this name too, so the engine never fills it from the secret store or its environment in that field's place, and refuses `auth.mode: inherit-env` there, which would forward the engine's own value | OpenAI dashboard |
| `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`, `OPENAI_ADMIN_KEY`, `OPENAI_BASE_URL`, `OPENAI_CUSTOM_HEADERS`, `OPENAI_WEBHOOK_SECRET` | **Never read by the engine's embeddings client.** The OpenAI SDK loads each at construction and would send them to whatever endpoint the embedder names; the provider undoes every one, so the only OpenAI settings it uses are the ones its configuration states. Nor does a coding CLI, unless its entry declares one: its child environment is the host allowlist plus what its profile and its entry's `env` declare | — |
| `CREWLET_LLM_CLI_HOME` | Read directly by every [`providers.llm`](../concepts/llm-backends.md) entry: the root under which each keeps its credential directory and per-seat CLI homes (`<root>/<provider key>`, each CLI in a subdirectory named for it: `<root>/<provider key>/<agent>`). Default `~/.crewlet/llm-cli`. Point it at a persistent volume when the engine runs in an ephemeral container, or the subscription login is lost on every restart. Overridden per entry by `state_dir`. | — |
| `CLAUDE_CODE_OAUTH_TOKEN` | Read as the subscription credential by a Claude Code entry when `auth.token` is unset. Resolved through the [secret store](../concepts/secret-store.md) first, so `crewlet llm login <key> -capture-token` stores it there and nothing needs exporting. | `claude setup-token`, or `crewlet llm login … -capture-token` |
| `CREWLET_LLM_CLI_<KEY>_CREDENTIALS` | Conventional name for a `providers.llm` entry's exported credential bundle (`<KEY>` is the `providers.llm` key upper-cased, non-alphanumerics folded to `_`). Restored into the provider's credential directory (every file of the login in one write, so a restore stopped part way is finished by the next reader rather than left as half a login) whenever the engine builds a company to run (at boot and at every apply it publishes) into a directory holding no login, or over a login installed before the bundle's, by the install record the bundle carries (the newest login wins), and by `crewlet llm list`, `status`, `export` and `doctor` before they look, so a fresh container comes up already authenticated. Never restored after `crewlet llm logout` until a login, `-from-host` or `import` writes one again; a fresh state directory has no such record of a logout, so removing this secret is what retires the bundle. Overridden by `auth.credential_bundle`. | `crewlet llm export <key> -secret-store` |
| `CREWLET_SANDBOX_LOCAL_HOME` | Read directly by the [`local` sandbox backend](../concepts/code-sandbox.md#local-sandboxes): the box root, the directory its per-box homes are created under, a seat's under `boxes/` and a Dashboard Assistant deep-work run's confined one under `assistant/boxes/`. Default `~/.crewlet/sandboxes`. Overridden per provider by `providers.sandbox.local.state_dir`. Point either at a volume of its own on a read-only root filesystem ([Where a box writes](../guides/deployment.md#where-a-box-writes)). | — |
| `CREWLET_TOOL_SKILLS_SPACE` | The `-space` default for `crewlet confluence import` and `crewlet confluence resync`: which Confluence space [Tool Skill](../concepts/tool-skills.md) pages are published into and read back from. Default `knowledge.skills_container`, itself defaulting to `TS`. **The engine never reads this variable** — the space it watches comes from the company document and only from there, because a fleet whose nodes each read a variable out of whoever's shell started them would disagree about which space holds the skills. To turn tool skills off, set `skills_container: ""` in the company config. | — |
| `CREWLET_API_TOKEN` | Read directly by the CLI commands that talk to a running node, reads included (`crewlet budgets`, `crewlet backup`, `crewlet retention`, `crewlet worklet`, `crewlet seats`, `crewlet config` against a live node and `crewlet iam`; and `crewlet secrets`, `crewlet llm`, the provisioners' `-secret-store` and every command resolving the company's `${VAR}`s, whenever they go through a node, this node's engine while it runs, or the one `-api` names ([Where a command reads the company's values](cli.md#where-a-command-reads-the-companys-values)). With this node's engine stopped, on a solo node or an external broker, they reach the fleet's store themselves and send none): the bearer token to send. It is the **only** source: there is no fallback to the Tier A file's own `api.auth.tokens`, because that list is what the node accepts and a write authored by whichever entry came first landed under a name nobody chose. Never taken as a flag value, because a token on a command line lands in shell history and `ps` | The value of one of the node's `api.auth.tokens`, or a machine token from `crewlet iam token` (`-login` for your own, which is the one `crewlet iam` command that reads no token: it signs in as you). A command that reads stored values through a node reveals each one, which takes one of the `api.auth.tokens`: a machine token is refused a reveal. |
| `CREWLET_OPERATOR` | Read directly by the CLI as the operator it records when it writes or reads the stores itself rather than through a node: a revision `crewlet config import` writes locally or `crewlet config rekey` writes, a secret `crewlet secrets set`, `crewlet llm login`, `crewlet llm export -secret-store` or a provisioner's `-secret-store` run writes to the fleet's store directly with this node's engine stopped (through a running node the row records the token's identity instead), and the `secret_revealed` line `crewlet secrets get -reveal` writes to its own log, whichever way it read the value. Falls back to `USER`, then `LOGNAME`, then `unknown` | Your own name or a CI job id |

---

## Logging

Read directly by the engine and the CLI. Each describes the **invocation**
rather than the company: the same `crewlet.yaml` is deployed to a container
with no terminal and run on a laptop with one, and the level a CI step wants
out of `crewlet migrate` has nothing to do with the node it is migrating.

Two of them are variables *because there is nowhere else for them to live*:
`CREWLET_LOG_COLOR` and `NO_COLOR` describe the screen someone is looking at,
which no deployment document can know, so colour has no Tier A field at all.
The other three answer for **every command except `crewlet run`**, which takes
no logging flags — `crewlet run` has `-log-level`, `-log-format`, `-log-file`
and `-debug` of its own, reads the `logging:` block from its Tier A file, and
ignores all three variables. So `CREWLET_LOG_LEVEL`, `CREWLET_LOG_FORMAT` and
`CREWLET_LOG_FILE` do have Tier A counterparts; what they have no counterpart
*for* is the one-shot commands, which read no `logging:` block.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_LOG_LEVEL` | The level **every command except `crewlet run`** logs at — `debug`, `info`, `warn` (the default) or `error`. Those commands are quiet by design (a store open logs a line per migration, which is noise on a one-shot command whose stdout is piped or diffed), and this is the escape hatch when a half-applied migration or a failing deploy gate is what you are looking at. A value this build does not recognise resolves to `warn`, so a typo can never be why an operator cannot run a migration. `crewlet run` ignores it and takes its level from `logging.level` in Tier A and its own `-log-level` / `-debug` flags | — |
| `CREWLET_LOG_FORMAT` | The shape those same commands log in — `console` (the default), `text` or `json`. The sibling of `CREWLET_LOG_LEVEL`, and it exists for the same reason: these commands take no logging flags, so a CI step shipping a `crewlet migrate` run to a collector has no other way to ask for `json`. An unrecognised name resolves to `console` | — |
| `CREWLET_LOG_FILE` | A file **every command except `crewlet run`** appends its log to, beside stderr — the third sibling of the two above, and it exists for the same reason: these commands take no logging flags, so a CI step that wants a `crewlet migrate` in the same durable record as the node it is migrating for has no other way to ask. It carries what the command *logs*; whatever it prints for its caller still goes to stdout, so a piped or diffed output is unchanged. Rotation is the same as a node's, at the defaults (100 MB, 5 backups) — the caps belong to a deployment's own `logging.file` block, and a one-shot command reads none, nor does it have a `logging.stderr` to switch off: stderr always stays on here. A path that cannot be opened **fails the command**: unlike a level, a path has no sane default to fall back to, and a record an operator asked for and silently did not get is worse than a refusal. `crewlet run` ignores it and takes its file from `logging.file` in Tier A and its own `-log-file` flag | — |
| `CREWLET_LOG_COLOR` | Whether `console` output carries ANSI colour — `auto` (the default: colour only when the stream is a live terminal), `always` or `never`. `always` is for a CI log viewer that renders ANSI without being a terminal, which auto-detection cannot discover on its own. Applies to `crewlet run` and every other command | — |
| `NO_COLOR` | Set to any non-empty value to suppress colour, following the [no-color.org](https://no-color.org) convention. It overrides `auto` — colour this program would have added on its own initiative — but **not** an explicit `CREWLET_LOG_COLOR=always`, which is an instruction about this program rather than initiative | — |

`TERM=dumb` also disables colour: an editor's shell pane sets it precisely to
say it cannot render escape sequences. None of the three colour levers reach a
log file: a file is never a terminal, so a `console`-format file is never
coloured and always carries the full date.

---

## Slack

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `SLACK_BOT_TOKEN_<HANDLE>` | Bot User OAuth Token (`xoxb-...`) | Sealed by the engine when a person approves the agent's install from the Agents tab, or Slack app > OAuth & Permissions for an app created by hand |
| `SLACK_SIGNING_SECRET_<HANDLE>` | Signing Secret | Sealed by the engine when it creates the agent's app, or Slack app > Basic Information for an app created by hand |
| `SLACK_APP_CONFIG_TOKEN` | The app-configuration token pair the engine creates and keeps agents' apps with, as one sealed value. Enter the *refresh* token (`xoxe-1-...`) once on Slack's connect form; the engine rotates it before it expires and seals each new pair back under this name before using it, since Slack's refresh token works once. | [api.slack.com/apps](https://api.slack.com/apps) > Your App Configuration Tokens (once) |

`<HANDLE>` is the seat's handle in uppercase, with every character a variable name refuses as `_` (e.g., `SLACK_BOT_TOKEN_SENIOR_ENGINEER`). The engine derives these names; a seat's block does not choose them.

---

## Jira

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `JIRA_URL` | Your Jira instance URL (`integrations.jira.url`, and the `JIRA_URL` the shared `atlassian` MCP server reads) | e.g., `https://company.atlassian.net` |
| `JIRA_ADMIN_TOKEN` | Admin or service-account API token (`integrations.jira.token`) | Atlassian account > API tokens |
| `JIRA_ADMIN_EMAIL` | That account's email, for Cloud Basic Auth (`integrations.jira.email`) | Your Atlassian account email |
| `JIRA_WEBHOOK_SECRET` | HMAC secret for Data Center webhooks (`integrations.jira.webhook_secret`); Cloud relays through the Forge app instead | Set when creating the Jira webhook |
| `JIRA_SITE_URL` | The human-clickable site base, as the `jira_base_url` [skill variable](../concepts/tool-skills.md#skill-variables) | e.g., `https://company.atlassian.net` |

### Per-agent Atlassian credentials

Each seat's own Atlassian account covers Jira and Confluence alike, on the shared `atlassian` MCP server's `mcp_env` entry.

| Variable | Description |
|----------|-------------|
| `ATLASSIAN_EMAIL_<SEAT>` | The seat's Atlassian account email (`mcp_env.atlassian.JIRA_USERNAME` and `CONFLUENCE_USERNAME`, e.g. `ATLASSIAN_EMAIL_CTO`) |
| `ATLASSIAN_TOKEN_<SEAT>` | That account's API token (`mcp_env.atlassian.JIRA_API_TOKEN` and `CONFLUENCE_API_TOKEN`, e.g. `ATLASSIAN_TOKEN_CTO`). The knowledge search runs as the seat when it holds one |
| `ATLASSIAN_FOUNDER_ACCOUNT_ID` | A human seat's Atlassian account id (`contact.atlassian_account_id`) |

---

## Confluence

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CONFLUENCE_URL` | Your Confluence instance URL (`integrations.confluence.url`) | e.g., `https://company.atlassian.net/wiki` |
| `CONFLUENCE_ADMIN_TOKEN` | Admin or service-account API token (`integrations.confluence.token`), what the tool-skill sync and skill promotion run on | Atlassian account > API tokens |
| `CONFLUENCE_ADMIN_EMAIL` | That account's email, for Cloud Basic Auth (`integrations.confluence.email`) | Your Atlassian account email |
| `CONFLUENCE_SITE_URL` | The human-clickable wiki base, as the `confluence_base_url` [skill variable](../concepts/tool-skills.md#skill-variables) | e.g., `https://company.atlassian.net/wiki` |
| `CONFLUENCE_WEBHOOK_SECRET` | HMAC secret for Data Center webhooks (`integrations.confluence.webhook_secret`) | Set when creating the webhook |
| `CONFLUENCE_WEBHOOK_TOKEN` | Shared token every **Cloud** hook carries in its URL (`integrations.confluence.webhook_token`), compared constant-time by `/webhooks/confluence/{event}`. Confluence Cloud signs nothing, so this is the whole authentication: treat it as a signing key. | Minted by `crewlet confluence provision`; `-recreate-webhooks` rotates it |

Per-agent Confluence credentials go through `role.mcp_env` on the `atlassian`
MCP server (`CONFLUENCE_USERNAME` / `CONFLUENCE_API_TOKEN`), the
`ATLASSIAN_EMAIL_<SEAT>` and `ATLASSIAN_TOKEN_<SEAT>` pair above.

---

## Web Search (optional)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `TAVILY_API_KEY` | Key for the shared Tavily web-search MCP server the example org declares | <https://tavily.com> |

---

## Mattermost

Conventions used by the [Mattermost integration](../integrations/mattermost.md) and its provisioning/bootstrap tooling. Apart from `MATTERMOST_PUBLIC_URL` (read by `docker-compose.yml`, the bootstrap script and `crewlet mattermost doctor`), these are `${VAR}` references in the company YAML or names the engine seals values under.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `MATTERMOST_URL` | Mattermost instance base URL (`integrations.mattermost.url`, and the `MATTERMOST_URL` each role's MCP server reads): the address the engine reaches the server at, which may be another route than `MATTERMOST_PUBLIC_URL` — an in-cluster name, say — under the same path, answered without a redirect (see [The address the engine dials](../integrations/mattermost.md#the-address-the-engine-dials)) | Written to `.env` by `scripts/mattermost-dev-bootstrap.sh` locally; your deployment's URL otherwise |
| `MATTERMOST_PUBLIC_URL` | **The address browsers use.** Read by the bundled `docker-compose.yml` (it becomes `MM_SERVICESETTINGS_SITEURL`), by the bootstrap script, and by [`crewlet mattermost doctor`](cli.md#crewlet-mattermost-doctor) as `-browser-url`'s default, which holds the server's Site URL to it. Getting it wrong costs every human live updates while the engine keeps working — see [The Site URL](../integrations/mattermost.md#the-site-url). Defaults to `http://localhost:8065`; the bootstrap defaults it to the address you reached the host on over SSH. | You — it is the address in your browser's address bar |
| `MATTERMOST_ADMIN_TOKEN` | The system-admin personal access token the engine provisions each agent's bot with (`integrations.mattermost.provisioning.admin_token`); `crewlet mattermost doctor` also reads it (`-admin-token` overrides) | Mattermost profile > Security > Personal Access Tokens (system-admin account) |
| `MATTERMOST_BOT_TOKEN_<HANDLE>` | Per-agent bot personal access token, sealed by the engine under this name when the agent is turned on in the Agents tab (e.g. `MATTERMOST_BOT_TOKEN_PM`); never written in the company document | Minted by the engine |

---

## GitLab

Conventions used by the [GitLab integration](../integrations/gitlab.md). Apart from the operator credential — read from the environment only, and never from the secret store — these are `${VAR}` references in the company YAML, resolved through the [secret store first and the environment behind it](../concepts/secret-store.md). [`crewlet gitlab provision`](cli.md#crewlet-gitlab-provision) mints the PAT values into whichever sink you name.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `GITLAB_ADMIN_TOKEN` | Read directly by `crewlet gitlab provision` as the operator credential fallback (group Owner / admin PAT with `api` scope; `-admin-token` overrides) | GitLab > Access tokens |
| `GITLAB_ROUTING_TOKEN` | Read-only routing token (`integrations.gitlab.token`), optional — without it, thread activity reaches only the people a payload names | A PAT with the `read_api` scope. **Nothing mints it**: the GitLab card asks for it, or set the variable yourself |
| `GITLAB_SIGNING_SECRET` | The hook's **signing token** (`integrations.gitlab.signing_secret`) — the HMAC key every delivery is verified against, and the route's only credential. Must be `whsec_` over standard base64 of a 32-byte key. | `crewlet gitlab provision` mints one into this variable; GitLab's own **Generate signing token** button produces the same shape |
| `GITLAB_TOKEN_<SEAT>` | Per-agent service-account PAT (each role's `mcp_env.gitlab.GITLAB_TOKEN`, also referenced from `role.sandbox.env`, e.g. `GITLAB_TOKEN_SWE`) | Minted by `crewlet gitlab provision` |

---

## GitHub

Conventions used by the [GitHub integration](../integrations/github.md). All of them are `${VAR}` references in the company YAML, resolved through the [secret store first and the environment behind it](../concepts/secret-store.md).

Unlike GitLab, **nothing here is minted for you**: GitHub issues no credential on a provisioner's behalf, so `crewlet github provision` registers webhooks and *reports* which account each seat's own token authenticates as. The tokens themselves are ones you create.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `GITHUB_WEBHOOK_SECRET` | HMAC secret every inbound delivery is verified against (`integrations.github.webhook_secret`), and the route's only credential. A route with nothing to check against answers **503** rather than accepting a delivery. | `crewlet github provision` mints one into this variable when it is empty, or GitHub > repository/org > Webhooks |
| `GITHUB_ENGINE_TOKEN` | The credential `crewlet github provision` registers webhooks with (`integrations.github.token`), where it is **required**: there is no degraded form of registering a hook. The **engine** does not read it: participant fan-out goes through each agent's own App, scoped to what that agent may see rather than to whatever the person who minted a token could reach, which is why the connect form asks for this token only when the company chooses an organization-wide hook. | A PAT with repository and organization webhook access |
| `GITHUB_TOKEN_<SEAT>` | Optional per-agent token (each role's `mcp_env.github`, e.g. `GITHUB_TOKEN_SENIOR`), for a seat whose tools read one. It is **not** what gives a seat its GitHub identity any more: each agent carries [its own GitHub App](../integrations/github.md#one-github-app-per-agent), which is what mints the token its tools use and what inbound routing resolves it by. A seat with no token here and an installed App of its own is complete. | A PAT on that agent's own GitHub account |
| `GITHUB_APP_KEY_<SEAT>` | The private key of that agent's own GitHub App, written by the engine when it converts the App manifest — **GitHub returns it exactly once**, so this is the only copy there is. Never typed in, and not deleted by a disconnect: GitHub offers no API for deleting an App registration, so the key stays valid for something that still exists and a disconnect names it rather than destroying it. | Written by the engine; nothing to obtain |
| `GITHUB_APP_WEBHOOK_SECRET_<SEAT>` | The secret that agent's own App signs its deliveries with, also written by the engine at conversion time and also named rather than deleted by a disconnect. | Written by the engine; nothing to obtain |

---

## Datadog

Conventions used by the [Datadog integration](../integrations/datadog.md).

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `DATADOG_WEBHOOK_TOKEN` | The shared token compared against the `X-Crewlet-Token` header on every delivery (`integrations.datadog.webhook_token`). **Treat it as a signing key**: Datadog's webhook can attach headers only with fixed values, so there is nothing varying with the payload to sign, and this constant-time comparison is the entire authentication. A replayed delivery is indistinguishable from a fresh one and anyone holding the token can forge an alert. Rotate it the way you would a signing secret; the next reconcile pass writes the new value into the webhook it keeps at Datadog. | Generate one: `openssl rand -base64 32`, or let the setup form mint it |
| `DATADOG_API_KEY` | Says which organization the engine acts in (`integrations.datadog.provisioning.api_key`). **Required when the block is enabled**: it is half of what registers the webhook that makes alerts arrive. | Datadog > Organization Settings > API Keys |
| `DATADOG_APP_KEY` | Says which user acts (`integrations.datadog.provisioning.app_key`). Datadog refuses a write carrying only an API key, and its message names neither. | Datadog > Organization Settings > Application Keys |

`integrations.datadog.route_to`, `webhook_name` and `handle_tag` are **not** secrets and belong in the company document as plain values, not as `${VAR}` references — nothing resolves them, so a reference written there is used as the literal text it is. The address a vendor reaches this deployment on is no longer here at all: it is in the operator's own Tier A file — `api.public.external_url` when the deployment has a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise — which resolves its `${VAR}` references before the document is decoded, so a reference that resolves to nothing is refused at load rather than silently yielding an address nothing can reach. `route_to` is required when the block is enabled: it names the seat an alert wakes when no monitor tag names an owner, and without it those alerts are verified, counted and delivered to nobody. See [Routing](../integrations/datadog.md#routing-is-by-ownership-not-by-mention).

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

There is no driver to pick: Turso is the store. `TURSO_GO_CACHE_DIR` (see Core
above) is where its native database engine is extracted.
The event store is a table in that same file, created by the engine's own
migrations — there is no separate observability database to configure.

---

## The external broker

Every Tier A field below takes a `${VAR}`, so these are **conventions** rather than variables the engine looks up by name — the name is whatever your `crewlet.yaml` writes. They are listed because a deployment that invents its own names ends up with the same value spelled three ways.

The whole block lives under `stream:`, which is the only place an external NATS estate is configured. (There is no `providers.queue` — Tier A refuses unknown keys, so a config written against that path fails to load.)

| Variable | Tier A field | Where to get it |
|----------|--------------|-----------------|
| `CREWLET_STREAM_URL` | `stream.url` — the NATS server to dial. Required for `type: nats`, and **refused** for `embedded`, which starts a server in this process and has no address for anyone to dial. The value goes to the NATS client verbatim, so a comma-separated list of a cluster's members is one setting and the client fails over between them. Every server it reaches must hold the engine's connection to a `max_payload` of at least 8 MiB, or the node refuses to start naming it — see [Deployment](../guides/deployment.md#an-external-nats-server) | Your cluster's client address (`nats://nats-1.internal:4222`) |
| `CREWLET_NATS_TOKEN` | `stream.token` — this engine's bearer token, for a server running with token authentication (see [Deployment § An external NATS server](../guides/deployment.md#an-external-nats-server)) | Whoever runs the cluster: it is the value in the server's `authorization { token: … }` |

Four more `stream:` fields carry no `${VAR}` convention because they are paths on disk rather than credentials. `stream.credentials` is a NATS credentials file — an NKey/JWT `.creds`, and the usual way to authenticate to an external estate instead of `stream.token`. `stream.tls.ca` / `.cert` / `.key` are the transport underneath that authentication: the private CA to trust for the server's certificate, and the client certificate to present. A broker configured the way a hardened NATS deployment is — `tls { verify: true }` — refuses a connection presenting no client certificate, whatever credentials would have followed.

**There is no coordination variable, because there is no second broker to authenticate against.** The coordination KV — the completion ledger, the delivery dedupe, the token counter, the activation pointer, the company's sealed secrets, and in a fleet the seat leases too — lives on the stream's *own* NATS broker on every topology — over the queue's connection on an external broker, over a second connection to the same in-process server on the embedded one, which has no address of its own to give — so `coordination:` carries only `type` (`local` or `embedded-kv`) and `lease_ttl_seconds`, and has nothing to point anywhere. A second estate would be worse than a second variable: two estates fail independently, so a node could hold live leases on one while the other has lost its inbox — alive to its peers, deaf to its work. See [Coordination](../concepts/coordination.md#backends).

---

## The Keyring (Required on Every Node)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_SECRET_KEY_<ID>` | Base64-encoded 32-byte key referenced by a Tier A `secrets.keys[].material`. **Required on every node**, whatever its roles and whether or not it serves the API: every record on every state log is signed and verified under the keyring, and the company document a node fetches from its peers is authenticated by its seal. On a node serving the API it also signs every session cookie and derives the key that verifies each per-run token. The **entire** Tier B config is stored encrypted at rest in the DB as one opaque blob. Every node of a fleet holds the same keys. | `crewlet secrets keygen` |

The keyring lives in Tier A (`crewlet.yaml`) and is the sole root of trust — the DB holds only the encrypted document, never the key, and the key is required for **every** config read. `crewlet validate` refuses a Tier A file without one, and a node refuses to start. The `${VAR}` references *inside* the config still resolve from the environment at construction time — behind the secret store below, which is consulted first. See [Configuration § Secrets](../concepts/configuration.md#secrets).

A keyring lets you retire the per-secret env vars on this page (`OPENROUTER_API_KEY`, `ATLASSIAN_TOKEN_<SEAT>`, `SLACK_BOT_TOKEN_<ROLE>`, `*_WEBHOOK_SECRET`, and the rest) two different ways:

- **[Secret store](../concepts/secret-store.md)** *(recommended)* — keep the `${VAR}` references in the config and store the values in the encrypted store (`crewlet secrets set`, or `-secret-store` on a provisioning CLI). The engine consults it **ahead of** the process environment, so a name it answers no longer needs to be exported at all. Rotation is a write of one record, and it reaches every node.
- **Literal values in the encrypted config** — set them via `PUT /config` or import a `company.yaml` with literals. Simpler, but every rotation writes a new immutable revision that archives the superseded secret, and one credential referenced from two places (a Slack bot token is both `role.integrations.slack.bot_token` and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`) becomes two literals that must change together.

Either way, `${VAR}` references that remain unanswered by the store still resolve from the environment.

**Those are the only two sources. The engine does not read a `.env` file.**
`crewlet … provision -env-file PATH` writes one for an operator to `source`,
and the values reach the engine only once they are in the process
environment — so an `-env-file` run that recorded something tells you to
source it and restart: on its `Next:` line for GitLab, Mattermost and Slack,
and in the note naming the minted secret for GitHub, Jira and Confluence. A
dotenv loader in the engine would be a third source of truth for secrets,
discovered by filename and able to override the Tier A keyring that opens the
store, which is the inversion the two-tier design exists to refuse.
`-secret-store` is the path that needs no file and no restart: the values land
in the fleet's encrypted store, and `POST /config/reload` to a node that is up
makes a running fleet re-read them, as the run says in the same place; a
stopped node reads them when it starts
([where a command reads the company's values](cli.md#where-a-command-reads-the-companys-values)).
[`crewlet config activate`](cli.md#crewlet-config-activate) opens the node's
own store, so it runs only while that engine is stopped, and is never what a
running fleet needs.

**Nothing in Tier A can move into the store**, no matter how it is configured — `CREWLET_SECRET_KEY_<ID>` above all. Tier A is what locates and decrypts the store, so it is always env- or file-sourced; it resolves with the store deliberately switched off.

---

## OpenTelemetry (Optional)

All read directly by the engine.

| Variable | Description | Example |
|----------|-------------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's **base** URL. The engine appends the signal path, so do **not** include `/v1/traces` here. It is also the upstream the sandbox forwarder sends a box's telemetry to, so on a node that runs seats and binds `api.port` it turns on the in-box telemetry of that node's own `direct` boxes, which export to the node's listener on loopback (see `CREWLET_SANDBOX_OTEL_RECEIVER_URL` below). | `http://localhost:4318` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | The traces endpoint in full, used verbatim. Overrides the base above when you need a non-standard path. | `https://collector.example.com/otlp/v1/traces` |
| `OTEL_EXPORTER_OTLP_HEADERS` | `k=v,k2=v2` headers for the OTLP backend (e.g. auth). Also used engine-side as the upstream auth for forwarded sandbox telemetry — never handed to the sandbox itself. | `authorization=Bearer%20...` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | How the **engine** talks to the collector: `http/protobuf` (default) or `grpc`. Anything else is refused at startup, naming the two that work. | `http/protobuf` |
| `OTEL_SERVICE_NAME` | The service the spans are reported under. Defaults to `crewlet`; set it when two companies share one collector. | `crewlet-acme` |
| `OTEL_TRACES_SAMPLER_ARG` | Head-sampling ratio for traces this node **roots**, `0`–`1`. Unset samples everything. An unparseable or out-of-range value warns and falls back to always-on rather than refusing to boot. | `0.1` |

> **The base and the signal endpoint are different settings.** `OTEL_EXPORTER_OTLP_ENDPOINT`
> is a base that the exporter appends `/v1/traces` to; `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`
> is the complete URL. Putting `/v1/traces` on the base is the common mistake — it also
> reaches the sandbox forwarder, which appends `/v1/{signal}` of its own, so the
> collector sees `/v1/traces/v1/traces`.

**Sampling is parent-based**, always. A remote sampling decision is honoured
whatever the ratio says, because these traces cross processes routinely and an
unsampled parent with sampled children is a broken tree at the collector. The
ratio governs only the traces this node starts itself.

When `OTEL_EXPORTER_OTLP_ENDPOINT` (or the traces endpoint) is set, the engine
exports spans to it — Jaeger, Grafana Tempo, or any OTLP backend. Without it,
spans are still created and their ids still flow into every event, the event
store and the dashboard's trace view; nothing is shipped anywhere. The engine's
exporter and the [sandbox OTLP forwarder](../concepts/code-sandbox.md) read the
**same** endpoint and headers on purpose, so a coding agent's spans land in the
same backend as the turn that started it and nest underneath it. A `direct`
box exports to its own node's listener whenever that node forwards, so setting
the endpoint on a node that runs seats is all its own boxes need; a box off the
host needs `CREWLET_SANDBOX_OTEL_RECEIVER_URL` as well.

---

## Code Sandbox

Used by `providers.sandbox`, the catalogue of boxes every agent seat's executor runs in as its coding CLI's own run, required once a company has an agent seat and a `providers.llm` entry — see [Code Sandbox](../concepts/code-sandbox.md). There is nothing to install: the binary carries every backend it has, and talks to E2B's REST API directly. The variable names below are the conventions the [Nimbus example](../../examples/nimbus.company.yaml) references; any `${ENV}` name works.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `E2B_API_KEY` | E2B API key, referenced by `providers.sandbox.e2b.api_key`. **Required for self-hosted clusters too** — every call authenticates with it, sent as `X-API-KEY`; the cluster domain only changes *which* API is talked to. | [e2b.dev](https://e2b.dev) dashboard (cloud) or your self-hosted cluster's key management |
| `E2B_DOMAIN` | Self-hosted / local cluster domain, referenced by `providers.sandbox.e2b.domain`. Omit for the vendor cloud. The engine reads it **through the config field**, never from the environment directly, so a stale export cannot silently reroute a box. | Your self-hosted E2B deployment |
| `CREWLET_SANDBOX_OTEL_RECEIVER_URL` | Read by every node: the externally-reachable base URL of whichever node serves your webhooks (an `ingress` one) (e.g. `http://host.docker.internal:80`). With Tier A `api.public.port` set, the receiver is served on that public listener only, so this names it. When set, the engine wires its `/otlp/{token}/v1/{signal}` receiver route and sandbox runs export telemetry through it (forwarded to `OTEL_EXPORTER_OTLP_*` when configured). A loopback address is handed to a `container` box under the runtime's name for the engine host, as the bridge's is. A **`direct`** box needs none of it on a node that forwards (`OTEL_EXPORTER_OTLP_ENDPOINT` set) and binds `api.port`: it exports to that node's own listener on loopback, by the tool bridge's rule, which a node without `ingress` serves its own host alone. Unset, every other box gets no sandbox telemetry, and so does a `direct` box on a node that forwards nothing. | Your engine's public address |
| `CREWLET_MCP_BRIDGE_URL` | Read by every node: the **fleet's** bridge address, as a sandbox can dial it — any node that serves the API (an `ingress` one), a load balancer in front of several included, and the same value on every node. Every seat's executor is its coding CLI's run in a box, and the box calls the seat's own tools back through the `/mcp/{token}` tool bridge the `ingress` nodes mount (see [Code sandbox § The tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)). A node mounts it only where this is set, or, for its own `direct` boxes, where it runs seats and binds `api.port` — an `ingress` node to anyone, a node without `ingress` beside its probes to its own host's loopback alone — so an `ingress` node that runs no seats needs it to serve the route at all, and gets it like every other node. A session lives in the process that holds the run's seat, which may serve no API at all; the node a box's call reaches relays it to that process over the broker, so a run whose seat moves to a peer goes on reaching its tools through the address it started with. A **`direct`** box on a node that binds `api.port` needs none of it: it is a process on that host and dials the node's own listener on loopback, which the node derives from the socket it binds — on a node without `ingress`, where that socket takes every interface or a loopback address. Unset, a launch into a `container` or `e2b` cell — or into `direct` on a node with no such listener, `api.port: 0` — is refused naming the variable and the cell rather than started without tools; on an `ingress` node with `api.port: 0`, which binds no listener for the route, every launch is refused naming `api.port`. With Tier A `api.public.port` set the bridge is served on the ingress nodes' public listener only, so this names that port. A loopback address serves a `container` box on the engine host too: it is handed the runtime's name for the engine host in its place ([Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)), which on Linux needs the API to listen beyond loopback; Docker Desktop and Podman's machine on macOS and Windows deliver that name on the host's loopback instead, where a loopback bind hears it. A reverse proxy or tunnel in front of the node may reach it on `127.0.0.1` and pass the public `Host` on: the bridge admits a call on the token in its path, whatever name the call arrives under (see [Code sandbox § The tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)). | The address your `ingress` nodes are reached at, as a sandbox reaches it |

Inside each sandbox run the engine **injects** `CREWLET_AGENT_HANDLE` and `CREWLET_AGENT_EMAIL` — the running agent's identity facts, readable by `role.sandbox.setup` recipes (e.g. to configure `git config user.name`/`user.email`). They are outputs of the engine, not inputs you set.

A run's coding agent is the CLI of the `providers.llm` entry its launch picks — the first `claude-code` or `opencode` member of the seat's executor chain not sitting out — with that entry's model, its `env` and its sign-in, so no sandbox-specific LLM secret is needed. External-service tokens are **explicit config**: each seat declares them in `role.sandbox.env` (e.g. `GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"` — by convention the same PAT the seat already uses for its `mcp_env.gitlab` server, so merge requests land under the agent's identity). The engine itself injects only the generic facts above — it never names a tool-specific variable; a declared `${ENV}` reference that resolves to nothing is left out of the run, which starts anyway and logs `sandbox_env_unresolved` naming the key — quietly, without that line, for a key listed under `optional_keys` (see [Code sandbox § Credentials and the run environment](../concepts/code-sandbox.md#credentials-and-the-run-environment)).

---

## Usage in YAML

A `${ENV_VAR}` reference works in both config files, and the two resolve it at different moments, from different places:

```yaml
providers:
  llm:
    default:
      agent: opencode
      model: "${LLM_MODEL}"     # a reference works in any string field
      env:                      # the CLI's own environment, every value resolved
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"
  embeddings:
    api_key: "${OPENAI_API_KEY}"
```

- **The company (Tier B)** keeps a reference verbatim — in the store, in an export, on `GET /config` — and resolves it where the provider, transport or integration that uses it is built: from the [secret store](../concepts/secret-store.md) first (when one is configured and holds the name), then the process environment.
- **`crewlet.yaml` (Tier A)** resolves every reference at startup, from the process environment alone — it holds the keyring that opens the secret store, so it cannot read a value out of it. Into a text field a reference works whole or embedded; into a number or a switch it must be the whole value, and what it resolves to is read as the same characters written there would be. [Environment Variable References](../getting-started/configuration.md#environment-variable-references) has the rules for both tiers.

An unanswered reference resolves to the empty string — except in a Tier A number or switch, where a reference that resolves to nothing is refused by name rather than read as unset.

**Tier A trims what a reference carries.** A value out of a file, a `--from-file` secret, a `.env` line or a captured command's output routinely keeps a trailing newline, so every string in `crewlet.yaml` is trimmed of the whitespace around it before a single rule reads one — which is also what the engine then runs — and so is what a reference into a number or a switch resolves to, before it is read. That matters because the two used to differ: `stream.cluster.advertise: " "` validated as *unset* and reached the broker as a space, where nats-server refuses it while starting, and a `node.labels` key with a stray space passed its own check and then matched no `role.placement` selector. Two keys in one map that are the same key once trimmed are refused rather than collapsed, since which survived would be decided by map order. Tier B is left alone: it carries the company's prompts and personas, where the whitespace around a fragment is the author's.

Only the braced identifier form is substituted — `${NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Bare `$NAME` and shell parameter expansions (`${1:-x}`, `${line#host=}`) are left untouched. A sandbox setup step's commands and file bodies are not substituted at all: the engine resolves only a file whose whole body is one `${VAR}`, and the step's `env`, and hands everything else to the box as written, where its own shell expands what it expands from the run environment ([setup steps](../concepts/code-sandbox.md#setup-steps--provisioning-the-box)).
