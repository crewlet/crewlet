# Environment Variables

All environment variables used by Crewlet and its integrations.

Names marked **read directly** are read by the engine or CLI. Everything else is a **`${VAR}` reference convention**: any YAML string value can reference any variable (see [Usage in YAML](#usage-in-yaml)); rename freely if the references match. The bundled [`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml) runs on no vendor, so it references only `OPENAI_API_KEY`; the engine itself reads the `CREWLET_*` names below, `CREWLET_BOX_GIT_URL` among them, which a node running seats needs for their checkouts of Gitlet.

---

## Core

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_NODE_ID` | This process's identity, when `node.id` is unset in the Tier A file. Labels every log line, health payload and config-apply event. Must be **stable across restarts**; defaults to `node-0` | Your orchestrator (Kubernetes pod name / StatefulSet ordinal, or the host name) |
| `TURSO_GO_CACHE_DIR` | Read directly by the `turso` driver (and prepared by the engine): where its embedded ~20 MB native library is extracted and loaded from. Default `os.UserCacheDir()` (`~/.cache` on Linux). In an ephemeral container point it at a writable, persistent path, or every restart re-extracts. See [Deployment § The store](../guides/deployment.md#the-store) | — |
| `CREWLET_API_TOKEN_FOUNDER` | Bearer token for the founder API identity (`api.auth.tokens`) | Generate one: `openssl rand -hex 32` |
| `ANTHROPIC_API_KEY` | The key a Claude Code entry under `auth.mode: api-key` is signed in with when it names no `auth.api_key`: read through the [secret store](../concepts/secret-store.md) first, then the environment, and put in the harness's own key variable (this one). **Only under that mode, and only when the field names nothing.** An entry whose `auth.api_key` reference resolves to nothing runs on no key, and a subscription entry never gets it, so a stray export cannot move a plan-billed seat onto metered billing | Anthropic Console |
| `CODEX_API_KEY` | The key a Codex entry under `auth.mode: api-key` is signed in with when it names no `auth.api_key`: read through the [secret store](../concepts/secret-store.md) first, then the environment, and put in the harness's own key variable (this one). `codex exec` signs its requests with it and, with only `OPENAI_API_KEY` set, sends no `Authorization` header. **Only under that mode, and only when the field names nothing.** An entry whose `auth.api_key` reference resolves to nothing runs on no key, and a subscription entry never gets it. An OpenAI key kept under another name is named there: `api_key: "${OPENAI_API_KEY}"` | OpenAI dashboard |
| `OPENAI_API_KEY` | Read by the OpenAI embeddings provider when `api_key` is unset, through the [secret store](../concepts/secret-store.md) first. An embeddings `api_key` whose `${VAR}` resolves to nothing runs on no key. **Not a key a harness entry falls back to**: Codex reads `CODEX_API_KEY`, and a Qwen Code entry (whose CLI does read this variable) names its key in `auth.api_key`. Other vendors' keys are kept under this name too, so the engine never fills it from the secret store or its environment in that field's place, and refuses `auth.mode: inherit-env` there, which would forward the engine's own value | OpenAI dashboard |
| `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`, `OPENAI_ADMIN_KEY`, `OPENAI_BASE_URL`, `OPENAI_CUSTOM_HEADERS`, `OPENAI_WEBHOOK_SECRET` | **Never read by the engine's embeddings client.** The OpenAI SDK would load each and send them to whatever endpoint the embedder names, so the provider undoes every one: it uses only the OpenAI settings its configuration states. A harness reads them only if its entry declares one: its child environment is the host allowlist plus what its profile and its entry's `env` declare | — |
| `CREWLET_LLM_HARNESS_HOME` | Read directly by every [`providers.llm`](../concepts/llm-backends.md) entry: the root under which each keeps its credential directory and per-seat harness homes (`<root>/<provider key>`, each harness in `<root>/<provider key>/<harness>`). Default `~/.crewlet/llm-harness`. In an ephemeral container point it at a persistent volume, or the subscription login is lost on every restart. Overridden per entry by `state_dir`. | — |
| `CLAUDE_CODE_OAUTH_TOKEN` | The subscription credential a Claude Code entry reads when `auth.token` is unset, through the [secret store](../concepts/secret-store.md) first, so `crewlet llm login <key> -capture-token` stores it there and nothing needs exporting. | `claude setup-token`, or `crewlet llm login … -capture-token` |
| `CREWLET_LLM_CLI_<KEY>_CREDENTIALS` | Conventional name for a `providers.llm` entry's exported credential bundle (`<KEY>` is the `providers.llm` key upper-cased, non-alphanumerics folded to `_`). Restored into the provider's credential directory (every file in one write; a part-way restore is finished by the next reader) whenever the engine builds a company to run (at boot and at every apply it publishes) into a directory holding no login, or over a login installed before the bundle's, by the install record the bundle carries (the newest login wins), and by `crewlet llm list`, `status`, `export` and `doctor` before they look. Never restored after `crewlet llm logout` until a login, `-from-host` or `import` writes one again; a fresh state directory has no logout record, so removing this secret retires the bundle. Overridden by `auth.credential_bundle`. | `crewlet llm export <key> -secret-store` |
| `CREWLET_SANDBOX_LOCAL_HOME` | Read directly by the [`local` sandbox backend](../concepts/code-sandbox.md#local-sandboxes): the box root, under which per-box homes are created (a seat's under `boxes/`). Default `~/.crewlet/sandboxes`. Overridden per provider by `providers.sandbox.local.state_dir`. On a read-only root filesystem point either at a volume of its own ([Where a box writes](../guides/deployment.md#where-a-box-writes)). | — |
| `CREWLET_TOOL_SKILLS_SPACE` | The `-space` default for `crewlet confluence import` and `crewlet confluence resync`: the Confluence space [Tool Skill](../concepts/tool-skills.md) pages are published into and read back from. Default `knowledge.skills_container`, itself defaulting to `TS`. **The engine never reads this variable**: the watched space comes only from the company document, since nodes would otherwise disagree. To turn tool skills off, set `skills_container: ""` in the company config. | — |
| `CREWLET_API_TOKEN` | Read directly by every CLI command that talks to a running node, reads included, as the bearer token to send. Each names that node with `-api`, defaulting to the `api.host:port` of its `-config`. A command that reads or writes the fleet's secret store (a company `${VAR}` it resolves included) goes through a node only while this node's engine runs or `-api` names one ([Where a command reads the company's values](cli.md#where-a-command-reads-the-companys-values)); with this node's engine stopped, on a solo node or an external broker, it reaches the store itself and sends no token. It is the **only** source: no fallback to the Tier A file's own `api.auth.tokens`, since a write by whichever entry came first landed under a name nobody chose. Never a flag value, since a token on a command line lands in shell history and `ps`. [`crewlet gitlet credential`](cli.md#crewlet-gitlet-credential) answers it to git as the password, so a person's git reaches [Gitlet](../concepts/gitlet.md) with their own token and no prompt; `crewlet gitlet export` and `crewlet gitlet import` hand it to the git they run the same way | The value of one of the node's `api.auth.tokens`, or a machine token from `crewlet iam token` (`-login` for your own, the one `crewlet iam` command that reads no token: it signs in as you). A command that reads stored values through a node reveals each one, which takes one of the `api.auth.tokens`: a machine token is refused a reveal. |
| `CREWLET_OPERATOR` | Read directly by the CLI as the operator it records when it writes or reads the stores itself rather than through a node: a revision `crewlet config import` writes locally or `crewlet config rekey` writes; a secret `crewlet secrets set`, `crewlet llm login`, `crewlet llm export -secret-store` or a provisioner's `-secret-store` run writes to the fleet's store directly with this node's engine stopped (through a running node the row records the token's identity instead), where a name past 64 bytes, a login's width, is refused naming the variable; and the `secret_revealed` line `crewlet secrets get -reveal` writes to its own log, whichever way it read the value. Falls back to `USER`, then `LOGNAME`, then `unknown` | Your own name or a CI job id |

---

## Logging

Read directly by the engine and the CLI. Each describes the **invocation**, not the company: the same `crewlet.yaml` runs in a terminal-less container and on a laptop, and the level a CI step wants from `crewlet migrate` has nothing to do with the node it migrates.

`CREWLET_LOG_COLOR` and `NO_COLOR` are variables because they describe the screen someone is looking at, which no deployment document can know, so colour has no Tier A field. The other three apply to **every command except `crewlet run`**, which takes no logging flags: `crewlet run` has `-log-level`, `-log-format`, `-log-file` and `-debug` of its own, reads the `logging:` block from its Tier A file, and ignores all three variables. So `CREWLET_LOG_LEVEL`, `CREWLET_LOG_FORMAT` and `CREWLET_LOG_FILE` have Tier A counterparts, just none for the one-shot commands, which read no `logging:` block.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_LOG_LEVEL` | The level **every command except `crewlet run`** logs at: `debug`, `info`, `warn` (default) or `error`. Those commands are quiet by design (a store open logs a line per migration); use this to debug a half-applied migration or failing deploy gate. An unrecognised value resolves to `warn`. `crewlet run` ignores it and takes its level from `logging.level` in Tier A and its own `-log-level` / `-debug` flags | — |
| `CREWLET_LOG_FORMAT` | The shape those commands log in: `console` (default), `text` or `json`. An unrecognised name resolves to `console` | — |
| `CREWLET_LOG_FILE` | A file **every command except `crewlet run`** appends its log to, beside stderr. It carries what the command *logs*; what it prints for its caller still goes to stdout, so piped or diffed output is unchanged. Rotation is a node's, at the defaults (100 MB, 5 backups): the caps belong to a deployment's `logging.file` block, which a one-shot command does not read, and stderr always stays on (no `logging.stderr` to switch off). A path that cannot be opened **fails the command**: a path has no sane default, and a silently missing record is worse than a refusal. `crewlet run` ignores it and takes its file from `logging.file` in Tier A and its own `-log-file` flag | — |
| `CREWLET_LOG_COLOR` | Whether `console` output carries ANSI colour: `auto` (default: only when the stream is a live terminal), `always` or `never`. `always` is for a CI log viewer that renders ANSI without being a terminal. Applies to `crewlet run` and every other command | — |
| `NO_COLOR` | Any non-empty value suppresses colour, following the [no-color.org](https://no-color.org) convention. It overrides `auto` but **not** an explicit `CREWLET_LOG_COLOR=always`, which is an instruction rather than initiative | — |

`TERM=dumb` also disables colour. None of the three colour levers reach a log file: a file is never a terminal, so a `console`-format file is never coloured and always carries the full date.

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

Per-agent Confluence credentials go through `role.mcp_env` on the `atlassian` MCP server (`CONFLUENCE_USERNAME` / `CONFLUENCE_API_TOKEN`), the `ATLASSIAN_EMAIL_<SEAT>` and `ATLASSIAN_TOKEN_<SEAT>` pair above.

---

## Web Search (optional)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `TAVILY_API_KEY` | Key for a shared Tavily web-search MCP server, for a company that declares one. The bundled example does not: its seats search with their harness's own web tools | <https://tavily.com> |

---

## Mattermost

Conventions used by the [Mattermost integration](../integrations/mattermost.md) and its provisioning/bootstrap tooling. Apart from `MATTERMOST_PUBLIC_URL` (read by `docker-compose.yml`, the bootstrap script and `crewlet mattermost doctor`), these are `${VAR}` references in the company YAML or names the engine seals values under.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `MATTERMOST_URL` | Mattermost instance base URL (`integrations.mattermost.url`, and the `MATTERMOST_URL` each role's MCP server reads): the address the engine reaches the server at, which may be another route than `MATTERMOST_PUBLIC_URL` (an in-cluster name, say) under the same path, answered without a redirect (see [The address the engine dials](../integrations/mattermost.md#the-address-the-engine-dials)) | Written to `.env` by `scripts/mattermost-dev-bootstrap.sh` locally; your deployment's URL otherwise |
| `MATTERMOST_PUBLIC_URL` | **The address browsers use.** Read by the bundled `docker-compose.yml` (it becomes `MM_SERVICESETTINGS_SITEURL`), by the bootstrap script, and by [`crewlet mattermost doctor`](cli.md#crewlet-mattermost-doctor) as `-browser-url`'s default, which holds the server's Site URL to it. A wrong value costs every human live updates while the engine keeps working; see [The Site URL](../integrations/mattermost.md#the-site-url). Defaults to `http://localhost:8065`; the bootstrap defaults it to the address you reached the host on over SSH. | You: the address in your browser's address bar |
| `MATTERMOST_ADMIN_TOKEN` | The system-admin personal access token the engine provisions each agent's bot with (`integrations.mattermost.provisioning.admin_token`); `crewlet mattermost doctor` also reads it (`-admin-token` overrides) | Mattermost profile > Security > Personal Access Tokens (system-admin account) |
| `MATTERMOST_BOT_TOKEN_<HANDLE>` | Per-agent bot personal access token, sealed by the engine under this name when the agent is turned on in the Agents tab (e.g. `MATTERMOST_BOT_TOKEN_PM`); never written in the company document | Minted by the engine |

---

## GitLab

Conventions used by the [GitLab integration](../integrations/gitlab.md). Apart from the operator credential (read from the environment only, never the secret store), these are `${VAR}` references in the company YAML, resolved through the [secret store first and the environment behind it](../concepts/secret-store.md). [`crewlet gitlab provision`](cli.md#crewlet-gitlab-provision) mints the PAT values into whichever sink you name.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `GITLAB_ADMIN_TOKEN` | Read directly by `crewlet gitlab provision` as the operator credential fallback (group Owner / admin PAT with `api` scope; `-admin-token` overrides) | GitLab > Access tokens |
| `GITLAB_ROUTING_TOKEN` | Read-only routing token (`integrations.gitlab.token`), optional: without it, thread activity reaches only the people a payload names | A PAT with the `read_api` scope. **Nothing mints it**: the GitLab card asks for it, or set the variable yourself |
| `GITLAB_SIGNING_SECRET` | The hook's **signing token** (`integrations.gitlab.signing_secret`): the HMAC key every delivery is verified against, and the route's only credential. Must be `whsec_` over standard base64 of a 32-byte key. | `crewlet gitlab provision` mints one into this variable; GitLab's own **Generate signing token** button produces the same shape |
| `GITLAB_TOKEN_<SEAT>` | Per-agent service-account PAT (each role's `mcp_env.gitlab.GITLAB_TOKEN`, also referenced from `role.sandbox.env`, e.g. `GITLAB_TOKEN_SWE`) | Minted by `crewlet gitlab provision` |

---

## GitHub

Conventions used by the [GitHub integration](../integrations/github.md). All are `${VAR}` references in the company YAML, resolved through the [secret store first and the environment behind it](../concepts/secret-store.md).

Unlike GitLab, **nothing here is minted for you**: GitHub issues no credential on a provisioner's behalf, so `crewlet github provision` registers webhooks and *reports* which account each seat's own token authenticates as. You create the tokens.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `GITHUB_WEBHOOK_SECRET` | HMAC secret every inbound delivery is verified against (`integrations.github.webhook_secret`), and the route's only credential. A route with nothing to check against answers **503** rather than accepting a delivery. | `crewlet github provision` mints one into this variable when it is empty, or GitHub > repository/org > Webhooks |
| `GITHUB_ENGINE_TOKEN` | The credential `crewlet github provision` registers webhooks with (`integrations.github.token`), where it is **required**: there is no degraded form of registering a hook. The **engine** does not read it: participant fan-out goes through each agent's own App, scoped to what that agent may see, which is why the connect form asks for this token only when the company chooses an organization-wide hook. | A PAT with repository and organization webhook access |
| `GITHUB_TOKEN_<SEAT>` | Optional per-agent token (each role's `mcp_env.github`, e.g. `GITHUB_TOKEN_SENIOR`), for a seat whose tools read one. It does **not** give a seat its GitHub identity: each agent carries [its own GitHub App](../integrations/github.md#one-github-app-per-agent), which mints the token its tools use and which inbound routing resolves it by. A seat with no token here and an installed App of its own is complete. | A PAT on that agent's own GitHub account |
| `GITHUB_APP_KEY_<SEAT>` | The private key of that agent's own GitHub App, written by the engine when it converts the App manifest. **GitHub returns it exactly once**, so this is the only copy. Never typed in, and not deleted by a disconnect: GitHub has no API for deleting an App registration, so the key stays valid for something that still exists and a disconnect names it rather than destroying it. | Written by the engine; nothing to obtain |
| `GITHUB_APP_WEBHOOK_SECRET_<SEAT>` | The secret that agent's own App signs its deliveries with, also written by the engine at conversion time and also named rather than deleted by a disconnect. | Written by the engine; nothing to obtain |

---

## Datadog

Conventions used by the [Datadog integration](../integrations/datadog.md).

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `DATADOG_WEBHOOK_TOKEN` | The shared token compared against the `X-Crewlet-Token` header on every delivery (`integrations.datadog.webhook_token`). **Treat it as a signing key**: Datadog's webhook can attach headers only with fixed values, so nothing varies with the payload to sign, and this constant-time comparison is the entire authentication. A replay looks fresh and anyone holding the token can forge an alert. Rotate it like a signing secret; the next reconcile pass writes the new value into the Datadog webhook. | Generate one: `openssl rand -base64 32`, or let the setup form mint it |
| `DATADOG_API_KEY` | Says which organization the engine acts in (`integrations.datadog.provisioning.api_key`). **Required when the block is enabled**: with the app key it registers the webhook that makes alerts arrive. | Datadog > Organization Settings > API Keys |
| `DATADOG_APP_KEY` | Says which user acts (`integrations.datadog.provisioning.app_key`). Datadog refuses a write carrying only an API key, and its message names neither. | Datadog > Organization Settings > Application Keys |

`integrations.datadog.route_to`, `webhook_name` and `handle_tag` are **not** secrets: write plain values in the company document, not `${VAR}` references, which nothing resolves there (a reference is used as literal text). The address a vendor reaches this deployment on is in the operator's Tier A file: `api.public.external_url` when the deployment has a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise. Tier A resolves its `${VAR}` references before the document is decoded, so a reference that resolves to nothing is refused at load. `route_to` is required when the block is enabled: it names the seat an alert wakes when no monitor tag names an owner, and without it those alerts are verified, counted and delivered to nobody. See [Routing](../integrations/datadog.md#routing-is-by-ownership-not-by-mention).

---

## The store

**There is no database environment variable, because there is no database server.** The store is two local files: the node's own, named by `store.path` in the Tier A bootstrap YAML, and the replicated estate beside it (`store.replicated_path`, beside `path` unless you name it):

```yaml
store:
  path: "/var/lib/crewlet/company.db"
```

One engine process owns each file **exclusively**. Neither is a shared database and there is no DSN; two engines opening one file corrupt it. What must be shared between nodes (seat leases, config activations, the completion ledger, dedupe and the rate valves) lives in the `coordination` slot.

There is no driver to pick: Turso is the store. `TURSO_GO_CACHE_DIR` (see Core above) is where its native database engine is extracted. The event store is a table in the node's own file, created by the engine's migrations; there is no separate observability database to configure.

---

## The external broker

Every Tier A field below takes a `${VAR}`, so these are **conventions**, not names the engine looks up: the name is whatever your `crewlet.yaml` writes.

The whole block lives under `stream:`, the only place an external NATS estate is configured. (There is no `providers.queue`: Tier A refuses unknown keys, so a config using that path fails to load.)

| Variable | Tier A field | Where to get it |
|----------|--------------|-----------------|
| `CREWLET_STREAM_URL` | `stream.url`: the NATS server to dial. Required for `type: nats`, **refused** for `embedded` (it starts a server in this process, with no address to dial). The value goes to the NATS client verbatim, so a comma-separated list of a cluster's members is one setting and the client fails over between them. Every server it reaches must hold the engine's connection to a `max_payload` of at least 8 MiB, or the node refuses to start naming it, and must set no `jetstream { limits { duplicate_window } }` below 15 minutes, or the boot fails provisioning the dead letters, naming it too; see [Deployment](../guides/deployment.md#an-external-nats-server) | Your cluster's client address (`nats://nats-1.internal:4222`) |
| `CREWLET_NATS_TOKEN` | `stream.token`: this engine's bearer token, for a server running with token authentication. `type: nats` needs one of `stream.credentials`, `stream.token`, `stream.tls.cert` or a credential in `stream.url`, and the node refuses to start against a server that authenticates nobody (see [Deployment § An external NATS server](../guides/deployment.md#an-external-nats-server)) | Whoever runs the cluster: the value in the server's `authorization { token: … }` |

Four more `stream:` fields have no `${VAR}` convention because they are disk paths, not credentials. `stream.credentials` is a NATS credentials file (an NKey/JWT `.creds`), the usual way to authenticate to an external estate instead of `stream.token`. `stream.tls.ca` / `.cert` / `.key` are the transport under that authentication: the private CA to trust for the server's certificate, and the client certificate to present. A broker configured like a hardened NATS deployment (`tls { verify: true }`) refuses a connection presenting no client certificate, whatever credentials would have followed.

**There is no coordination variable, because there is no second broker.** The coordination KV (the completion ledger, the delivery dedupe, the token counter, the activation pointer, the company's sealed secrets, and in a fleet the seat leases) lives on the stream's *own* NATS broker on every topology: over the queue's connection on an external broker, over a second connection to the same in-process server on the embedded one, the seat leases' holds riding a lease connection of their own on both. So `coordination:` carries only `type` (`local` or `embedded-kv`) and `lease_ttl_seconds`. A second estate would fail independently (live leases on one, inbox lost on the other). See [Coordination](../concepts/coordination.md#backends).

---

## The Keyring (Required on Every Node)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_SECRET_KEY_<ID>` | Base64-encoded 32-byte key referenced by a Tier A `secrets.keys[].material`. **Required on every node**, whatever its roles and whether or not it serves the API: every record on every state log is signed and verified under the keyring, and the company document a node fetches from its peers is authenticated by its seal. On a node serving the API it also signs every session cookie and derives the key that verifies each per-run token. On an embedded fleet every route, leaf link and operator connection to the broker presents a credential derived from it, so a node holding other keys is refused by every member ([Deployment § The fleet's broker admits only its own nodes](../guides/deployment.md#the-fleets-broker-admits-only-its-own-nodes)). The **entire** Tier B config is stored encrypted at rest in the DB as one opaque blob. Every node of a fleet holds the same keys. | `crewlet secrets keygen` |

The keyring lives in Tier A (`crewlet.yaml`) and is the sole root of trust: the DB holds only the encrypted document, never the key, and the key is required for **every** config read. `crewlet validate` refuses a Tier A file without one, and a node refuses to start. The `${VAR}` references *inside* the config still resolve from the environment at construction time, behind the secret store below, which is consulted first. See [Configuration § Secrets](../concepts/configuration.md#secrets).

A keyring lets you retire the per-secret env vars on this page (`OPENAI_API_KEY`, `ATLASSIAN_TOKEN_<SEAT>`, `SLACK_BOT_TOKEN_<ROLE>`, `*_WEBHOOK_SECRET`, and the rest) two ways:

- **[Secret store](../concepts/secret-store.md)** *(recommended)*: keep the `${VAR}` references in the config and store the values in the encrypted store (`crewlet secrets set`, or `-secret-store` on a provisioning CLI). The engine consults it **ahead of** the process environment, so a name it answers need not be exported. Rotation is a write of one record, and it reaches every node.
- **Literal values in the encrypted config**: set them via `PUT /config` or import a `company.yaml` with literals. Simpler, but every rotation writes a new immutable revision that archives the superseded secret, and one credential referenced from two places (a Slack bot token is both `role.integrations.slack.bot_token` and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`) becomes two literals that must change together.

Either way, `${VAR}` references the store does not answer still resolve from the environment.

**Those are the only two sources. The engine does not read a `.env` file.** `crewlet … provision -env-file PATH` writes one for an operator to `source`, and the values reach the engine only once in the process environment, so an `-env-file` run that recorded something tells you to source it and restart: on its `Next:` line for GitLab, Mattermost and Slack, and in the note naming the minted secret for GitHub, Jira and Confluence. A dotenv loader would be a third source able to override the Tier A keyring that opens the store. `-secret-store` needs no file and no restart: the values land in the fleet's encrypted store, and `POST /config/reload` to a node that is up makes a running fleet re-read them; a stopped node reads them when it starts ([where a command reads the company's values](cli.md#where-a-command-reads-the-companys-values)). [`crewlet config activate`](cli.md#crewlet-config-activate) opens the node's own store, so it runs only while that engine is stopped, and a running fleet never needs it.

**Nothing in Tier A can move into the store**, `CREWLET_SECRET_KEY_<ID>` above all. Tier A locates and decrypts the store, so it is always env- or file-sourced and resolves with the store switched off.

---

## OpenTelemetry (Optional)

All read directly by the engine.

| Variable | Description | Example |
|----------|-------------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's **base** URL. The engine appends the signal path, so do **not** include `/v1/traces`. It is also the upstream the sandbox forwarder sends a box's telemetry to, so on a node that runs seats and binds `api.port` it turns on the in-box telemetry of that node's own `direct` boxes, which export to the node's listener on loopback (see `CREWLET_SANDBOX_OTEL_RECEIVER_URL` below). | `http://localhost:4318` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | The traces endpoint in full, used verbatim. Overrides the base above for a non-standard path. | `https://collector.example.com/otlp/v1/traces` |
| `OTEL_EXPORTER_OTLP_HEADERS` | `k=v,k2=v2` headers for the OTLP backend (e.g. auth). Also the engine-side upstream auth for forwarded sandbox telemetry, never handed to the sandbox itself. | `authorization=Bearer%20...` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | How the **engine** talks to the collector: `http/protobuf` (default) or `grpc`. Anything else is refused at startup, naming the two that work. | `http/protobuf` |
| `OTEL_SERVICE_NAME` | The service the spans are reported under. Defaults to `crewlet`; set it when two companies share one collector. | `crewlet-acme` |
| `OTEL_TRACES_SAMPLER_ARG` | Head-sampling ratio for traces this node **roots**, `0`-`1`. Unset samples everything. An unparseable or out-of-range value warns and falls back to always-on rather than refusing to boot. | `0.1` |

> **The base and the signal endpoint are different settings.** `OTEL_EXPORTER_OTLP_ENDPOINT`
> is a base that the exporter appends `/v1/traces` to; `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`
> is the complete URL. Putting `/v1/traces` on the base is the common mistake — it also
> reaches the sandbox forwarder, which appends `/v1/{signal}` of its own, so the
> collector sees `/v1/traces/v1/traces`.

**Sampling is parent-based**, always. A remote sampling decision is honoured whatever the ratio says, because an unsampled parent with sampled children is a broken tree at the collector. The ratio governs only traces this node starts itself.

When `OTEL_EXPORTER_OTLP_ENDPOINT` (or the traces endpoint) is set, the engine exports spans to it (Jaeger, Grafana Tempo, any OTLP backend). Without it, spans are still created and their ids still flow into every event, the event store and the dashboard's trace view; nothing is shipped. The engine's exporter and the [sandbox OTLP forwarder](../concepts/code-sandbox.md) read the **same** endpoint and headers on purpose, so a harness's spans land in the same backend as the turn that started it and nest under it. A `direct` box exports to its own node's listener whenever that node forwards, so setting the endpoint on a node that runs seats is all its own boxes need; a `container` box needs `CREWLET_SANDBOX_OTEL_RECEIVER_URL` as well.

---

## Code Sandbox

Used by `providers.sandbox`, the catalogue of boxes every agent seat's executor runs in as its harness's own run. It is required once a company has an agent seat and a `providers.llm` entry — see [Code Sandbox](../concepts/code-sandbox.md). The binary carries both cells: `direct` needs nothing more, and `container` needs Docker or Podman on the host. The names below are the conventions the [Nimbus example](../../examples/nimbus.company.yaml) references; any `${ENV}` name works.

A URL variable below (`CREWLET_SANDBOX_OTEL_RECEIVER_URL`, `CREWLET_MCP_BRIDGE_URL`, `CREWLET_BOX_GIT_URL`) must be an absolute `http://` or `https://` URL a box can use: a host a URL parser accepts (a name, or an address with no zone; an unfilled `<host>` placeholder or a mistyped IPv4 address such as `10.0.0.300` is refused), a port from 1 to 65535 if given, no user information, query or fragment. Otherwise the boot is refused naming the variable, and `crewlet llm doctor` reports it as refused. `http://` is accepted, but then each run's token crosses every network between the box and the host in clear: use `https://` wherever the URL names another node or a balancer, or crosses to a satellite's site — see [Code sandbox § Security model](../concepts/code-sandbox.md#security-model).

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_SANDBOX_OTEL_RECEIVER_URL` | Read by every node: the externally reachable base URL of whichever node serves your webhooks (an `ingress` one), e.g. `http://host.docker.internal:80`. With Tier A `api.public.port` set, the receiver is served on that public listener only, so this names it. When set, the engine wires its `/otlp/{token}/v1/{signal}` receiver route and executors in their boxes export telemetry through it (forwarded to `OTEL_EXPORTER_OTLP_*` when configured). A loopback address is handed to a `container` box under the runtime's name for the engine host, as the bridge's is. A **`direct`** box needs none of it on a node that forwards (`OTEL_EXPORTER_OTLP_ENDPOINT` set) and binds `api.port`: it exports to that node's own listener on loopback, by the tool bridge's rule (a node without `ingress` serves its own host alone). Unset, every other box gets no sandbox telemetry, and so does a `direct` box on a node that forwards nothing. | Your engine's public address |
| `CREWLET_MCP_BRIDGE_URL` | Read by every node: the **fleet's** bridge address as a sandbox can dial it: any node serving the API (an `ingress` one), or a load balancer in front of several, the same value on every node. The box calls the seat's own tools back through the `/mcp/{token}` tool bridge the `ingress` nodes mount (see [Code sandbox § The tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)). A node mounts it only where this is set, or, for its own `direct` boxes, where it runs seats and binds `api.port` (an `ingress` node to anyone; a node without `ingress` to its own host's loopback alone), so an `ingress` node that runs no seats needs it to serve the route at all. A session lives in the process holding the run's seat, which may serve no API; the node a box's call reaches relays it to that process over the broker, so a run whose seat moves to a peer keeps reaching its tools through the address it started with. A **`direct`** box on a node that binds `api.port` needs none of it: it dials the node's own listener on loopback, derived from the socket it binds (on a node without `ingress`, that socket takes every interface or a loopback address). Unset, a launch into a `container` cell, or into `direct` on a node with no such listener (`api.port: 0`), is refused naming the variable and the cell rather than started without tools; on an `ingress` node with `api.port: 0` (no listener for the route) every launch is refused naming `api.port`. With Tier A `api.public.port` set the bridge is served on the ingress nodes' public listener only, so this names that port. A loopback address serves a `container` box on the engine host too: it is handed the runtime's name for the engine host in its place ([Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)), which on Linux needs the API to listen beyond loopback; Docker Desktop and Podman's machine on macOS and Windows deliver that name on the host's loopback, where a loopback bind hears it. A reverse proxy or tunnel may reach the node on `127.0.0.1` and pass the public `Host` on: the bridge admits a call on the token in its path, whatever name it arrives under (see [Code sandbox § The tool bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)). | The address your `ingress` nodes are reached at, as a sandbox reaches it |
| `CREWLET_BOX_GIT_URL` | Read by every node that runs seats, for a company that hosts code (`code.backend: gitlet`): the base URL a coding box reaches [Gitlet](../concepts/gitlet.md#coding-boxes) at, `/box/git` included (e.g. `https://git.example.com/box/git`); every clone a run given repositories makes uses `<this>/<repo>.git` as its remote. Any Gitlet node serves any box, so name a Gitlet node's public listener or a balancer over them; `/box/git/` is mounted only on a node whose roles name `gitlet`, and with Tier A `api.public.port` set only on that public listener. Unset, a run asked to be given repositories is refused on this node (`sandbox_git_url_unset`) rather than given a repository its box cannot reach; a run given none is unaffected. Use `https://` on any fleet whose seat nodes are not its Gitlet nodes. A loopback address serves a `container` box, a check's included, on the engine host too: it is handed the runtime's name for the engine host in its place, as the bridge's is ([Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)), which on Linux needs the API to listen beyond loopback. | A Gitlet node's public address, as a sandbox reaches it, with `/box/git` on the end |

Inside each executor's box the engine **injects** `CREWLET_AGENT_HANDLE` and `CREWLET_AGENT_EMAIL`, the running agent's identity, readable by `role.sandbox.setup` recipes (e.g. to configure `git config user.name`/`user.email`). They are outputs, not inputs.

A run's harness is that of the `providers.llm` entry its launch picks (the first `claude-code`, `codex`, `gemini-cli`, `qwen-code`, `opencode`, `cursor-agent`, `copilot`, `grok`, `muse-code`, `kimi-code`, `hermes` or `pi` member of the seat's executor chain not sitting out), with that entry's model, its `env` and its sign-in, so no sandbox-specific LLM secret is needed. External-service tokens are **explicit config**: each seat declares them in `role.sandbox.env` (e.g. `GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"`, by convention the PAT the seat already uses for its `mcp_env.gitlab` server, so merge requests land under the agent's identity). The engine injects only the generic facts above, and names no tool-specific variable except for Gitlet, whose access it gives a run that is given repositories (ADR-0093), below. A declared `${ENV}` reference that resolves to nothing is left out of the run, which starts anyway and logs `sandbox_env_unresolved` naming the key; the line is omitted for a key listed under `optional_keys`, unless its value is one an integration mints or issues for the seat, which the line always names (see [Code sandbox § Credentials and the run environment](../concepts/code-sandbox.md#credentials-and-the-run-environment)).

### Set by the engine inside a box

Every launch on a node that serves [Gitlet](../concepts/code-sandbox.md#repositories-on-gitlet) with `CREWLET_BOX_GIT_URL` set is handed the run's access to it, in that launch's environment alone, before any repository is checked out. It is minted for the launch, never written on the run's record and never read from your configuration. The two `CREWLET_GIT_*` names below are **reserved**: `role.sandbox.env`, a seat's setup steps' `env`, `providers.sandbox.setup`'s `env` and `code.checks.env` are refused naming one. The engine never reads any of them back.

| Variable | What it is |
|----------|------------|
| `CREWLET_GIT_URL` | The launching node's `CREWLET_BOX_GIT_URL` as the box dials it (a loopback host, in a `container` box, under the runtime's name for the engine host): every clone's remote is `$CREWLET_GIT_URL/<repo>.git` |
| `CREWLET_GIT_ACCESS_FILE` | The path of the file in the box holding the run's Gitlet credential: a token naming the turn and this one launch, ending with its run, written there before the job starts (readable by the box's user alone). git presents it as the Basic password of user `run` through the credential helper below, which reads the file at each request, so the token is in no environment variable a process could print or hand on |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_<n>`, `GIT_CONFIG_VALUE_<n>` | git's configuration through the environment: your own run-environment entries are kept, and five of the engine's appended after them: `user.name` and `user.email`, the seat's commit identity; `protocol.version 2`; and `index.version 4` with `core.untrackedCache true`, the index settings a wide checkout's status was measured faster with ([What a clone costs](../concepts/code-sandbox.md#what-a-clone-costs)). Not reserved: your own entries stay yours, and a count that is not a number or names an entry it does not set refuses the launch |
| `GIT_CONFIG_PARAMETERS` | Read by git after every `GIT_CONFIG_COUNT` entry, so a harness's own entries cannot switch these off. Your own items are kept, and the engine appends after them, for a run given repositories on Gitlet, an empty `credential.<base>/.helper`, `<base>` being `$CREWLET_GIT_URL` (clears every helper configured before it for Gitlet's URL) and the helper answering with the token in the file `$CREWLET_GIT_ACCESS_FILE` names. Where [commit attribution](../concepts/code-sandbox.md#commit-attribution) is on, the run's wrapper appends one item to it after its login shell's profile, `core.hooksPath`, the hooks directory whose hook writes each of the run's own commits' trailers ([Code sandbox § Commit trailers](../concepts/code-sandbox.md#commit-trailers)). A box can forge it, so the push's record is the provenance. Not reserved: your own items stay, ahead of the run's |

---

## Set by the engine, never by you

[Gitlet](../concepts/gitlet.md) runs git as a subprocess, and git starts the engine's own binary as its `proc-receive` hook inside every push (`crewlet gitlet hook proc-receive`, which hands git's ref updates back to the engine). The engine tells the hook where to reach it with two variables it sets on that one git process:

| Variable | What it is |
|----------|------------|
| `CREWLET_HOOK_SOCKET` | The unix socket the engine listens on for its pushes' hooks, made under `$TMPDIR`, so a node that serves git needs a `$TMPDIR` short enough for a socket's path ([Deployment § Gitlet](../guides/deployment.md#gitlet)) |
| `CREWLET_HOOK_TOKEN` | A one-time token naming the one push the hook belongs to, consumed when the hook connects |

Neither is read from the engine's own environment; setting either does nothing. The engine builds every git process's environment itself (its `PATH` and otherwise only variables of its own choosing), so no variable of yours and none of the host's git configuration or attributes reaches git or its hook. Run by hand, `crewlet gitlet hook` says it is run by the engine's git and exits 2.

---

## Usage in YAML

A `${ENV_VAR}` reference works in both config files, resolved at different moments from different places:

```yaml
providers:
  llm:
    default:
      harness: opencode
      model: "${LLM_MODEL}"     # a reference works in any string field
      env:                      # the harness's own environment, every value resolved
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"
  embeddings:
    api_key: "${OPENAI_API_KEY}"
```

- **The company (Tier B)** keeps a reference verbatim (in the store, in an export, on `GET /config`) and resolves it where the provider, transport or integration using it is built: from the [secret store](../concepts/secret-store.md) first (when one is configured and holds the name), then the process environment.
- **`crewlet.yaml` (Tier A)** resolves every reference at startup, from the process environment alone: it holds the keyring that opens the secret store, so it cannot read a value out of it. In a text field a reference works whole or embedded; in a number or a switch it must be the whole value, read as the same characters written there would be. [Environment Variable References](../getting-started/configuration.md#environment-variable-references) has the rules for both tiers.

An unanswered reference resolves to the empty string, except in a Tier A number or switch, where it is refused by name rather than read as unset.

**Tier A trims what a reference carries.** Values from a file, a `--from-file` secret, a `.env` line or command output often keep a trailing newline, so every string in `crewlet.yaml` is trimmed of surrounding whitespace before any rule reads it (which is also what the engine runs), as is what a reference into a number or a switch resolves to. Without it, `stream.cluster.advertise: " "` validated as *unset* yet reached the broker as a space, which nats-server refuses at start, and a `node.labels` key with a stray space passed its check but matched no `role.placement` selector. Two keys in one map that are the same once trimmed are refused rather than collapsed, since map order would decide the survivor. Tier B is left alone: it carries prompts and personas, where the whitespace around a fragment is the author's.

Only the braced identifier form is substituted: `${NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Bare `$NAME` and shell parameter expansions (`${1:-x}`, `${line#host=}`) are left untouched. A sandbox setup step's commands and file bodies are not substituted at all: the engine resolves only a file whose whole body is one `${VAR}`, and the step's `env`, and hands everything else to the box as written, where its own shell expands from the run environment ([setup steps](../concepts/code-sandbox.md#setup-steps--provisioning-the-box)).
