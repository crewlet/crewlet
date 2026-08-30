# Environment Variables

All environment variables used by Crewlet and its integrations.

Two kinds of variable appear below. A few names are **read directly by the engine or CLI** (marked as such). Everything else is a **`${VAR}` reference convention**: any string value in the YAML config can reference any environment variable (see [Usage in YAML](#usage-in-yaml)), and the names listed here are simply the conventions the bundled [`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml) uses — rename them freely as long as the config references match.

---

## Core

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_NODE_ID` | This process's identity, when `node.id` is unset in the Tier A file. Labels every log line, health payload, and config-apply event. Must be **stable across restarts**; defaults to `node-0` | Your orchestrator (Kubernetes pod name / StatefulSet ordinal, or the host name) |
| `TURSO_GO_CACHE_DIR` | Read directly by the `turso` driver (and by the engine, which prepares it): where its embedded ~20 MB native library is extracted and loaded from. Default `os.UserCacheDir()` — `~/.cache` on Linux. Point it at a writable, persistent path in an ephemeral container, or every restart pays the extraction again. See [Deployment § The store](../guides/deployment.md#the-store) | — |
| `CREWLET_API_TOKEN_FOUNDER` | Bearer token for the founder API identity (`api.auth.tokens`) | Generate one: `openssl rand -hex 32` |
| `LLM_API_KEY` | API key for your LLM provider (`providers.llm.default.api_keys`) | Your LLM provider dashboard |
| `LLM_MODEL` | Model id served by your OpenAI-compatible endpoint (`providers.llm.default.model` in the example) | Your LLM provider docs |
| `LLM_BASE_URL` | Your OpenAI-compatible endpoint's `/v1/` base URL (`providers.llm.default.base_url` in the example) | Your LLM provider docs |
| `OPENAI_API_KEY` | Read directly as a fallback by the `openai` / `openai-compatible` LLM providers (when `api_keys` is empty) and the OpenAI embeddings provider (when `api_key` is unset) | OpenAI dashboard |
| `ANTHROPIC_API_KEY` | Read directly as a fallback by the `anthropic` LLM provider when `api_keys` is empty | Anthropic Console |
| `CREWLET_LLM_CLI_HOME` | Read directly by every [`cli-agent`](../concepts/subscription-llm-backends.md) LLM provider: the root under which each provider keeps its credential directory and per-seat CLI homes (`<root>/<provider key>`). Default `~/.crewlet/llm-cli`. Point it at a persistent volume when the engine runs in an ephemeral container, or the subscription login is lost on every restart. Overridden per provider by `cli.state_dir`. | — |
| `CLAUDE_CODE_OAUTH_TOKEN` | Read as the subscription credential by a `cli-agent` provider on the `claude-code` profile when `cli.auth.token` is unset. Resolved through the [secret store](../concepts/secret-store.md) first, so `crewlet llm login <key> -capture-token` stores it there and nothing needs exporting. | `claude setup-token`, or `crewlet llm login … -capture-token` |
| `CREWLET_LLM_CLI_<KEY>_CREDENTIALS` | Conventional name for a `cli-agent` provider's exported credential bundle (`<KEY>` is the `providers.llm` key upper-cased, non-alphanumerics folded to `_`). Restored into the provider's credential directory at boot when that directory is empty, so a fresh container comes up already authenticated. Overridden by `cli.auth.credential_bundle`. | `crewlet llm export <key> -secret-store` |
| `CREWLET_SANDBOX_LOCAL_HOME` | Read directly by the [`local` sandbox backend](../concepts/code-sandbox.md#local-sandboxes): the parent directory its per-box homes are created under. Default `~/.crewlet/sandboxes`. Overridden per provider by `providers.sandbox.local.state_dir`. | — |
| `CREWLET_TOOL_SKILLS_SPACE` | The `-space` default for `crewlet confluence import` and `crewlet confluence resync`: which Confluence space [Tool Skill](../concepts/tool-skills.md) pages are published into and read back from. Default `integrations.confluence.skills_space`, itself defaulting to `TS`. **The engine never reads this variable** — the space it watches comes from the company document and only from there, because a fleet whose nodes each read a variable out of whoever's shell started them would disagree about which space holds the skills. To turn tool skills off, set `skills_space: ""` in the company config. | — |

---

## Logging

Read directly by the engine and the CLI. All four describe the **invocation**
rather than the company, which is why they are variables and not Tier A
fields: the same `crewlet.yaml` is deployed to a container with no terminal
and run on a laptop with one, and the level a CI step wants out of `crewlet
migrate` has nothing to do with the node it is migrating.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_LOG_LEVEL` | The level **every command except `crewlet run`** logs at — `debug`, `info`, `warn` (the default) or `error`. Those commands are quiet by design (a store open logs a line per migration, which is noise on a one-shot command whose stdout is piped or diffed), and this is the escape hatch when a half-applied migration or a failing deploy gate is what you are looking at. A value this build does not recognise resolves to `warn`, so a typo can never be why an operator cannot run a migration. `crewlet run` ignores it and takes its level from `logging.level` in Tier A and its own `-log-level` / `-debug` flags | — |
| `CREWLET_LOG_FORMAT` | The shape those same commands log in — `console` (the default), `text` or `json`. The sibling of `CREWLET_LOG_LEVEL`, and it exists for the same reason: these commands take no logging flags, so a CI step shipping a `crewlet migrate` run to a collector has no other way to ask for `json`. An unrecognised name resolves to `console` | — |
| `CREWLET_LOG_COLOR` | Whether `console` output carries ANSI colour — `auto` (the default: colour only when the stream is a live terminal), `always` or `never`. `always` is for a CI log viewer that renders ANSI without being a terminal, which auto-detection cannot discover on its own. Applies to `crewlet run` and every other command | — |
| `NO_COLOR` | Set to any non-empty value to suppress colour, following the [no-color.org](https://no-color.org) convention. It overrides `auto` — colour this program would have added on its own initiative — but **not** an explicit `CREWLET_LOG_COLOR=always`, which is an instruction about this program rather than initiative | — |

`TERM=dumb` also disables colour: an editor's shell pane sets it precisely to
say it cannot render escape sequences.

---

## Slack

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `SLACK_BOT_TOKEN_<ROLE>` | Bot User OAuth Token (`xoxb-...`) | Written by `crewlet slack provision`, or Slack app > OAuth & Permissions |
| `SLACK_SIGNING_SECRET_<ROLE>` | Signing Secret | Written by `crewlet slack provision`, or Slack app > Basic Information |
| `SLACK_CONFIG_REFRESH_TOKEN` | **Bootstrap only.** The app-configuration *refresh* token (`xoxe-1-...`) that seeds an empty [app ledger](../integrations/slack.md#the-ledger-slack-appsjson); `crewlet slack provision` exchanges it for a 12-hour access token and stores **both** halves of the rotated pair in the ledger. Once the ledger holds a pair, this variable is ignored — see the precedence note below. | [api.slack.com/apps](https://api.slack.com/apps) > Your App Configuration Tokens (once) |

Replace `<ROLE>` with the role name in uppercase (e.g., `SLACK_BOT_TOKEN_ENGINEER`). The per-role names are conventions — any `${VAR}` name referenced from `role.integrations.slack` works, and `crewlet slack provision` writes whatever names the YAML uses.

> **The ledger beats the shell**, which is the reverse of the usual "an explicit input wins" rule, and the reverse is the point. Slack's config-token rotation is **single-use in both directions**: every successful rotate invalidates the refresh token it was given, so the value sitting in a `SLACK_CONFIG_REFRESH_TOKEN` export is dead the moment this command first used it. Preferring it would trade the ledger's live pair — the only way back into the operator's apps — for a token Slack has already retired, on every run after the first, for ever. So `-config-token` and `$SLACK_CONFIG_REFRESH_TOKEN` seed a ledger that holds nothing, and are ignored once it does.

---

## Atlassian

Conventions used by the [Atlassian organization](../integrations/atlassian.md) — the organization that *contains* the Jira and Confluence sites below, and where an agent's own Atlassian identity comes from. Apart from the operator credential — read from the environment only, and never from the secret store — these are `${VAR}` references in the company YAML, resolved through the [secret store first and the environment behind it](../concepts/secret-store.md). [`crewlet atlassian provision`](cli.md#crewlet-atlassian-provision) mints the per-seat values into whichever sink you name; on Data Center there is nothing to mint and both halves are created by hand.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `ATLASSIAN_ORG_API_KEY` | Read directly by `crewlet atlassian provision` as the operator credential (`-admin-token` overrides). An organization API key created **without scopes**: the account-management API refuses a scoped key with `403` whatever scopes it holds, which looks exactly like having no permission at all | admin.atlassian.com > Settings > API keys |
| `ATLASSIAN_ADMIN_TOKEN` | The second name read for that same key, behind `ATLASSIAN_ORG_API_KEY`. Same credential, same rules | as above |
| `ATLASSIAN_ORG_ID` | The organization every service account is created in (`integrations.atlassian.org_id`). A company value like any other, so it resolves store-first; the run refuses to start when it resolves empty, because there is no organization to create an account in | admin.atlassian.com > Settings |
| `ATLASSIAN_TOKEN_<SEAT>` | A seat's own Atlassian API token, referenced from `role.mcp_env` under whichever key that seat's server reads — `JIRA_API_TOKEN`, `CONFLUENCE_API_TOKEN`, `ATLASSIAN_API_TOKEN`, … (e.g. `ATLASSIAN_TOKEN_CTO`) | Minted by `crewlet atlassian provision` on Cloud; created by hand in the admin console on Data Center |
| `ATLASSIAN_EMAIL_<SEAT>` | The address that token authenticates as (`JIRA_USERNAME`, `CONFLUENCE_USERNAME`, `ATLASSIAN_EMAIL`, …). Half of the credential rather than a label: Cloud authenticates an API token as `Basic base64(email:token)` and rejects the identical token presented as a bearer | Recorded by `crewlet atlassian provision` — **Atlassian assigns it**, so Crewlet never chooses a service account's address |

**The two operator names are environment-only, and that is enforced rather than conventional.** They are read through the path `GITLAB_ADMIN_TOKEN` and `MATTERMOST_ADMIN_TOKEN` take, which deliberately bypasses the secret store: the key may create billable accounts across the whole organization, the store is replicated to every node holding the keyring, and reading one back from it would imply it may be kept there — in the same table as the seat credentials it exists to mint. See [Secret store § What still has to be in the environment](../concepts/secret-store.md#what-still-has-to-be-in-the-environment).

**The per-seat names are conventions, and the provisioner has none of its own.** It mints into the variables a seat's `mcp_env` already references — a `CREWLET_ATLASSIAN_TOKEN_<seat>` would be a variable nothing reads — so any names work, as long as each half is a whole `${VAR}` reference: a literal, or a reference embedded in a longer string, is reported as a note and skipped. One credential serves both products, which is why the pair is spelled `ATLASSIAN_*` rather than once per product. See [the seat credential contract](../integrations/atlassian.md#the-seat-credential-contract).

Human seats are never provisioned. A person holds their own Atlassian account, named by `contact.atlassian_account_id` — one id covers Jira and Confluence, and the [example org](../../examples/nimbus.company.yaml) references it as `${ATLASSIAN_FOUNDER_ACCOUNT_ID}`.

---

## Jira

Conventions used by the [Jira integration](../integrations/jira.md), resolved [store first, environment second](../concepts/secret-store.md). This block is the **org read account** — the credential that answers "who is watching this issue", which is the one routing input a Jira webhook never carries — and not an agent identity: every agent acts through its own credential, the Atlassian pair above.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `JIRA_URL` | The instance address (`integrations.jira.url`) — a Data Center instance or a Cloud site. Mutually exclusive with `cloud_id`, and giving both is refused rather than resolved: the engine reads through the gateway when both are set, so the url would silently become links-only | e.g. `https://example.atlassian.net` |
| `JIRA_CLOUD_ID` | An Atlassian Cloud site id (`integrations.jira.cloud_id`); the `api.atlassian.com/ex/jira` gateway URL is built from it. It is also the site a Jira licence is granted on, which is why the provisioner reads it from this block rather than from the organization | The URL of admin.atlassian.com |
| `JIRA_SITE_URL` | The human-readable base for links handed to a person (`integrations.jira.site_url`, and the example's `skill_variables.jira_base_url`). Needed with a cloud id: the API gateway is not a place a browser can go, so a link built from it looks right and opens nothing | Your site's browser address |
| `JIRA_ADMIN_TOKEN` | The org read account's API token or PAT (`integrations.jira.token`) | Atlassian account > API tokens (Cloud), or a Data Center PAT |
| `JIRA_ADMIN_EMAIL` | That account's address (`integrations.jira.email`). Setting it switches authentication to `Basic base64(email:token)`, which is what Cloud requires; leaving it unset sends a bearer token, which is what a Data Center PAT wants. The same credential is refused purely on which scheme carried it | The org account's Atlassian address |
| `JIRA_WEBHOOK_SECRET` | HMAC secret for `POST /webhooks/jira` (`integrations.jira.webhook_secret`). **Data Center only** — Cloud events arrive through the Forge app on `/webhooks/forge`, verified against `integrations.forge_app_id`, and there is no HMAC secret anywhere in that path. A route whose secret is unset has nothing to verify with and answers `503` rather than accepting the delivery | Minted into this variable by `crewlet jira provision` when it resolves empty; set by hand when you create the webhook yourself |

`JIRA_API_TOKEN` is deliberately not the name in that table. That spelling is one of the *keys* a seat's `mcp_env` block carries, where its value is that seat's own `${ATLASSIAN_TOKEN_<SEAT>}` — and one name that means both the org account and an agent is how the wrong credential ends up in the wrong place. Per-agent Jira credentials are the Atlassian pair above; there is no separate per-role Jira token.

---

## Confluence

Conventions used by the [Confluence integration](../integrations/confluence.md), on the same terms as Jira. This account is what the tool-skill sync and skill promotion run as, and it is the **fallback** for the query-time `## Relevant knowledge` search: that search runs as the asking seat wherever the seat carries its own credential — which is what lets Confluence enforce its own page permissions — and drops back to this account only for a seat that carries none.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CONFLUENCE_URL` | The instance address (`integrations.confluence.url`) — Data Center or a Cloud site, mutually exclusive with `cloud_id` on the same terms as Jira | e.g. `https://example.atlassian.net/wiki` |
| `CONFLUENCE_CLOUD_ID` | An Atlassian Cloud site id (`integrations.confluence.cloud_id`), behind the `api.atlassian.com/ex/confluence` gateway, and the site a Confluence licence is granted on. A company that gives one product a cloud id and the other a direct URL is refused as half Cloud and half Data Center | The URL of admin.atlassian.com |
| `CONFLUENCE_SITE_URL` | The human-readable base for page links (`integrations.confluence.site_url`, and the example's `skill_variables.confluence_base_url`) | Your wiki's browser address |
| `CONFLUENCE_ADMIN_TOKEN` | The org account's API token or PAT (`integrations.confluence.token`) | Atlassian account > API tokens |
| `CONFLUENCE_ADMIN_EMAIL` | That account's address (`integrations.confluence.email`); present selects Cloud Basic auth, absent selects a bearer token | The org account's Atlassian address |
| `CONFLUENCE_WEBHOOK_SECRET` | HMAC secret for Data Center webhooks (`integrations.confluence.webhook_secret`), on the same 503-rather-than-accept terms as Jira's. Nothing mints this one: there is no `crewlet confluence provision` — the confluence CLI is `import` and `resync` | Set when creating the webhook |

Per-agent Confluence credentials are the same Atlassian pair — one account, one token, both products — declared in `role.mcp_env` under `CONFLUENCE_USERNAME` / `CONFLUENCE_API_TOKEN` beside the Jira spellings.

The tool-skills space is **not** one of these. `CREWLET_TOOL_SKILLS_SPACE` is a flag default for the import and resync commands only (see [Core](#core)); the space the engine watches comes from `integrations.confluence.skills_space` and from nowhere else.

---

## Web Search (optional)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `TAVILY_API_KEY` | Key for the shared Tavily web-search MCP server the example org declares | <https://tavily.com> |

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

Conventions used by the [GitLab integration](../integrations/gitlab.md). Apart from the operator credential — read from the environment only, and never from the secret store — these are `${VAR}` references in the company YAML, resolved through the [secret store first and the environment behind it](../concepts/secret-store.md). [`crewlet gitlab provision`](cli.md#crewlet-gitlab-provision) mints the PAT values into whichever sink you name.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `GITLAB_ADMIN_TOKEN` | Read directly by `crewlet gitlab provision` as the operator credential fallback (group Owner / admin PAT with `api` scope; `-admin-token` overrides) | GitLab > Access tokens |
| `GITLAB_ENGINE_TOKEN` | Engine read token (`integrations.gitlab.token`) | GitLab service account, or minted by provisioning |
| `GITLAB_SIGNING_SECRET` | The hook's **signing token** (`integrations.gitlab.signing_secret`) — the HMAC key every delivery is verified against, and the route's only credential. Must be `whsec_` over standard base64 of a 32-byte key. | `crewlet gitlab provision` mints one into this variable; GitLab's own **Generate signing token** button produces the same shape |
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

There is no driver to pick. Turso is the store, and both the `store.driver`
field and the `CREWLET_STORE_DRIVER` variable that used to select mainline
SQLite instead are retired — a Tier A file that still sets the field is refused
by name. `TURSO_GO_CACHE_DIR` (see Core above) is where its native database
engine is extracted.
The event store is a table in that same file, created by the engine's own
migrations — there is no separate observability database to configure.

---

## The external broker

Every Tier A field below takes a `${VAR}`, so these are **conventions** rather than variables the engine looks up by name — the name is whatever your `crewlet.yaml` writes. They are listed because a deployment that invents its own names ends up with the same value spelled three ways.

The whole block lives under `stream:`, which is where an external NATS estate and a Pulsar estate are both configured. (There is no `providers.queue` — Tier A refuses unknown keys, so a config written against that path fails to load.)

| Variable | Tier A field | Where to get it |
|----------|--------------|-----------------|
| `CREWLET_PULSAR_URL` | `stream.url` — the broker address. Required for `type: nats` and `type: pulsar`, and **refused** for `embedded` | Your broker's service address (`pulsar://…`, `nats://…`) |
| `CREWLET_PULSAR_TOKEN` | `stream.token` — this engine's bearer token, for a broker running with token authentication (see [Deployment § Authentication](../guides/deployment.md)) | `bin/pulsar tokens create --subject <engine-role>` |
| `PULSAR_ADMIN_TOKEN` | None — read by the compose broker config, its healthcheck and `pulsar-admin`, never by the engine | `bin/pulsar tokens create --subject admin` |

Two more `stream:` fields carry no `${VAR}` convention because they are paths and names rather than credentials: `stream.credentials` is a NATS credentials file on disk (the usual way to authenticate to an external NATS estate, instead of `stream.token`), and `stream.tenant` / `stream.namespace` scope this company's topics inside a Pulsar estate.

A Pulsar stream keeps its **leases** on a NATS estate rather than on Pulsar — Pulsar has no compare-and-set — so `coordination.nats.url` / `.token` / `.credentials` are configured alongside, with the same shapes. See [Coordination](../concepts/coordination.md#backends).

---

## Secret Encryption (Optional)

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `CREWLET_SECRET_KEY_<ID>` | Base64-encoded 32-byte key referenced by a Tier A `secrets.keys[].material`. When a keyring is configured, the **entire** Tier B config is stored **encrypted at rest** in the DB as one opaque blob instead of as verbatim `${VAR}` references. | `crewlet secrets keygen` |

The keyring lives in Tier A (`crewlet.yaml`) and is the sole root of trust — the DB holds only the encrypted document, never the key, and the key is required for **every** config read. Without a keyring, Crewlet keeps the default `${VAR}`-reference behaviour and every env var on this page is resolved from the environment at construction time. See [Configuration § Secrets](../concepts/configuration.md#secrets).

A keyring lets you retire the per-secret env vars on this page (`LLM_API_KEY`, `ATLASSIAN_TOKEN_<SEAT>`, `SLACK_BOT_TOKEN_<ROLE>`, `*_WEBHOOK_SECRET`, …) two different ways:

- **[Secret store](../concepts/secret-store.md)** *(recommended)* — keep the `${VAR}` references in the config and store the values in the encrypted store (`crewlet secrets set`, or `-secret-store` on a provisioning CLI). The engine consults it **ahead of** the process environment, so a name it answers no longer needs to be exported at all. Rotation is a write of one record, and it reaches every node.
- **Literal values in the encrypted config** — set them via `PUT /config` or import a `company.yaml` with literals. Simpler, but every rotation writes a new immutable revision that archives the superseded secret, and one credential referenced from two places (a Slack bot token is both `role.integrations.slack.bot_token` and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`) becomes two literals that must change together.

Either way, `${VAR}` references that remain unanswered by the store still resolve from the environment.

**Those are the only two sources. The engine does not read a `.env` file.**
`crewlet … provision -env-file PATH` writes one for an operator to `source`,
and the values reach the engine only once they are in the process
environment — so an `-env-file` run ends with "source it and restart", every
time. A dotenv loader in the engine would be a third source of truth for
secrets, discovered by filename and able to override the Tier A keyring that
opens the store, which is the inversion the two-tier design exists to refuse.
`-secret-store` is the path that needs no file and no restart: the values land
in the encrypted table, and [`crewlet config activate`](cli.md#crewlet-config-activate)
makes a running fleet re-read them.

**Nothing in Tier A can move into the store**, no matter how it is configured — `CREWLET_SECRET_KEY_<ID>` above all. Tier A is what locates and decrypts the store, so it is always env- or file-sourced; it resolves with the store deliberately switched off.

---

## OpenTelemetry (Optional)

All read directly by the engine.

| Variable | Description | Example |
|----------|-------------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's **base** URL. The engine appends the signal path, so do **not** include `/v1/traces` here. | `http://localhost:4318` |
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
same backend as the turn that started it and nest underneath it.

---

## Code Runtime (Sandbox, Optional)

Used only when `providers.sandbox` is configured so sandbox-enabled roles can author code in an isolated sandbox — see [Code Sandbox](../concepts/code-sandbox.md). There is nothing to install: the binary carries every backend it has, and talks to E2B's REST API directly. The variable names below are the conventions the [Nimbus example](../../examples/nimbus.company.yaml) references; any `${ENV}` name works.

| Variable | Description | Where to get it |
|----------|-------------|-----------------|
| `E2B_API_KEY` | E2B API key, referenced by `providers.sandbox.api_key`. **Required for self-hosted clusters too** — every call authenticates with it, sent as `X-API-KEY`; the cluster domain only changes *which* API is talked to. | [e2b.dev](https://e2b.dev) dashboard (cloud) or your self-hosted cluster's key management |
| `E2B_DOMAIN` | Self-hosted / local cluster domain, referenced by `providers.sandbox.domain`. Omit for the vendor cloud. The engine reads it **through the config field**, never from the environment directly, so a stale export cannot silently reroute a box. | Your self-hosted E2B deployment |
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
