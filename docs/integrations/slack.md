# Slack Integration

Crewlet uses a **one Slack app per agent** model — each agent gets its own bot identity, token, and webhook endpoint in Slack.

> **Prerequisites.** You need a Slack **workspace you administer** (create one if you don't have it). The engine's API endpoint must be **reachable by Slack over public HTTPS** so the Events API can deliver webhooks, and so the OAuth install can land (for local development, use a tunnel such as ngrok or cloudflared).

> **This is one of three chat backends, and the axis is exclusive.** A Slack app — the org-level `integrations.slack` block **or any seat's own**, since Slack's credentials are per seat — puts this company on `chat.backend: vendor`. The engine's own chat ([Chatlet](../concepts/chatlet.md)) is what a company gets when it declares no vendor chat surface at all, and naming `chat.backend: chatlet` beside a Slack app is **refused at validation**: two live chat homes means a person replies in one while the agents read the other, with no rule anywhere to say which conversation was real. Slack and [Mattermost](mattermost.md) together are fine — two workspaces with different people in them is what an org migrating between them looks like for months.

There are two ways to create those apps:

- **[Automated (recommended)](#automated-setup-crewlet-slack-provision)** — `crewlet slack provision` creates and maintains every app through Slack's [App Manifest APIs](https://docs.slack.dev/app-manifests/configuring-apps-with-app-manifests/). One config token to bootstrap, one authorize click per agent, and every secret it mints lands where you send it — the fleet's encrypted secret store, an env file or your terminal ([Where the secrets go](#where-the-secrets-go)).
- **[Manual](#manual-setup)** — click through [api.slack.com/apps](https://api.slack.com/apps) per agent.

Both end in the same place: per-agent credentials referenced from the company YAML ([Configure in YAML](#configure-in-yaml)).

---

## Setting it up from the dashboard

Company › Integrations shows one section per agent, because on Slack the
credentials belong to the seat: each agent has its own app, so each has its own
bot token and signing secret. Paste the two values Slack shows on the app's own
page and the engine seals them, gives the seat a `${VAR}` pointing at each, and
activates. Each seat's own delivery address is shown beside its fields.

**It does not create the apps.** Creating one goes through Slack's app-manifest
API, which authenticates with an app configuration token Slack issues only by
hand from its own pages, and which your organisation may not permit at all.
Where those tokens are available, [`crewlet slack provision`](#automated-setup-crewlet-slack-provision)
below remains the automated path and does the whole thing. Where they are not,
connect Slack from the dashboard's Company › Integrations: every agent gets its
own block there carrying the manifest its app is created from, so building each
app is a copy, a paste into Slack's **From an app manifest** flow, an install,
and the two values pasted back. See the [manual setup](#manual-setup) for the
same steps done entirely by hand.

### Disconnecting

Disconnect removes `integrations.slack` from the company document, which
retires the transport: the engine stops talking to Slack and the per-seat
webhook routes stop turning deliveries into work.

**Tick *also remove the accounts Crewlet created* and each agent's
`integrations.slack` block goes too, along with the sealed values it named** —
`SLACK_BOT_TOKEN_*`, `SLACK_SIGNING_SECRET_*`, whatever the `${VAR}`s are
called. That is safe here in a way it would not be elsewhere: Slack shows both
values on the app's own settings page on every visit, so anything deleted can
be read back by whoever owns the app. Leave it unticked and every credential
stays where it is.

**Deleting the apps is yours to do**, either way. Slack's `apps.manifest.delete`
authenticates with an app-configuration token Slack issues only by hand — the
same credential this surface exists because you may not have — so the
disconnect names each agent's app and links to the page it is deleted from:
**Settings › Basic Information › Delete App**.

Until an app is deleted it can still post as that agent to anyone holding its
bot token, which is the reason the handover is a list rather than a sentence.

## Configure in YAML

`integrations.slack: {}` (org-level) is a marker that enables the Slack **transport**; its one setting is [`typing_status`](#working-status-is-thinking). The Slack **MCP tool** server is a separate `mcp_servers` entry (`shared: false`). Per agent, the Slack identity has two consumers: the **transport** reads `role.integrations.slack` (`bot_token` and `signing_secret`, both required together), and the **Slack MCP subprocess** reads `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`. The transport both writes and reads on that token: it raises the working indicator, and at the start of a turn woken in a thread it calls `conversations.replies` so the agent is handed the conversation rather than told to go and fetch it (see [the thread block](../concepts/agent-runtime.md#the-thread-a-turn-was-woken-in)). That needs no new scope — the `*:history` scopes the manifest already requests cover it — and it reads as that app, so a channel the bot is not in answers `not_in_channel` and the turn runs without the block. `conversations.replies` pages from the *oldest* end, so a thread past ~1000 messages cannot be read to its newest end: the agent is then handed the newest of what was reached plus a note that the block stops short and the rest has to be read with its chat tools, never a truncated thread presented as the whole one. Name the same `${VAR}` in both — one credential, two readers, no secret duplicated:

```yaml
integrations:
  slack:                                 # enable the Slack transport
    typing_status: always                # always (default) | addressed

mcp_servers:
  - name: slack
    shared: false                        # per-agent identity (token from role.mcp_env.slack)
    command: npm
    args: ["exec", "--yes", "--", "slack-mcp-server@1.3.0", "--transport", "stdio"]
    tool_prefix: "slack_"
    env:
      SLACK_MCP_ADD_MESSAGE_TOOL: "true"   # registers conversations_add_message: without it the agent cannot post
      SLACK_MCP_REACTION_TOOL: "true"      # registers reactions_add / reactions_remove

units:
  - name: Core
    id: core                                              # the unit's key; `name` above is display
    type: team
    lead: engineer                                        # the lead seat's handle
    roles:
      - name: Engineer
        integrations:                                     # per-agent transport identity
          slack:
            bot_token: "${SLACK_BOT_TOKEN_ENGINEER}"
            signing_secret: "${SLACK_SIGNING_SECRET_ENGINEER}"
        mcp_env:
          slack:
            SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_ENGINEER}"   # same token, the Slack MCP
            SLACK_MCP_CHANNELS_CACHE: "/var/cache/crewlet/slack/engineer-channels.json"   # this seat's own
      - name: Designer
        integrations:
          slack:
            bot_token: "${SLACK_BOT_TOKEN_DESIGNER}"
            signing_secret: "${SLACK_SIGNING_SECRET_DESIGNER}"
        mcp_env:
          slack:
            SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_DESIGNER}"
            SLACK_MCP_CHANNELS_CACHE: "/var/cache/crewlet/slack/designer-channels.json"
```

**The two `env` switches are what let an agent answer.** [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) registers its write tools only when told to: without `SLACK_MCP_ADD_MESSAGE_TOOL` it never offers `conversations_add_message`, its one tool that posts, so a seat receives every message, reasons about a reply and has no tool to send it with, and the channel stays quiet while the server reports itself healthy. (The 1.1 releases that had the tool registered it and refused every call without the variable, which ends the same way.) `"true"` lets the agent post in every channel and DM its bot can reach; a comma-separated list of channel ids, or of `!`-prefixed ids to exclude, narrows that, as the server's own [settings](https://github.com/korotovsky/slack-mcp-server/blob/v1.3.0/README.md#environment-variables-quick-reference) describe — and anything else is read as such a list, so `"false"` registers the tool and lets it post only in a channel whose id is `false`, which is none. A narrowed list mutes the seat wherever it is addressed outside it, and a seat is addressed wherever its bot has been invited. `SLACK_MCP_REACTION_TOOL` is the same switch for `reactions_add` and `reactions_remove`, which the [`reactions:write`](#bot-scopes-and-events) scope is requested for. Neither is a credential, so both sit in the server entry's `env`, which every seat's instance starts with, rather than in each seat's `mcp_env`. And in `env` rather than in an `--enabled-tools` argument: the server registers a tool that argument names, but every call reads the environment alone, so a tool switched on only on the command line is offered and refuses each post. The release is pinned so a working run names the build it ran: `npm exec` with `@latest` runs whatever was published last, and a later release that changes what a tool is called, or what has to be set before it exists, would arrive unannounced. The bundled tool skills depend on it too: the mentions, channel, hand-off and directives skills trigger on this release's tool names under the entry's `slack_` prefix (`slack_conversations_add_message`, `slack_channels_me`, `slack_channels_list`, `slack_users_search`), so a seat posting on Slack is taught Slack's `<@member-id>` mentions and thread replies before its first post. Under another prefix, or after a release that renamed one of those tools, the skills would go quiet without a sound.

**Each seat names a channel cache of its own.** slack-mcp-server keeps the channels its bot can see in a file, and by default names that file after the workspace alone (`<team id>_channels_cache_v2.json`, in the user's cache directory) and serves it for 24 hours without asking Slack again. Every seat's server on one node runs as the same user against the same workspace, so without `SLACK_MCP_CHANNELS_CACHE` they share one file, and whichever wrote it last decides what `channels_list` tells the rest: the private channels and DMs that bot is in, named to seats that are not, and none of the reader's own. Give each seat a path no other seat uses. The directory has to exist for the list to outlive a restart; where it does not, the server keeps the list in memory and fetches it afresh at every start, which is still that seat's own.

The `bot_token` drives the transport (the seat's own identity at start, and the working indicator); the same value, named as `SLACK_MCP_XOXB_TOKEN`, drives the Slack MCP tools — two independent subsystems, one credential referenced in two places. `signing_secret` is transport-only — it verifies this seat's inbound deliveries and nothing else reads it. Both live on `role.integrations.slack`, in the file and through the API alike. There is no per-seat channel setting: the engine's transport posts no message, so it has nothing to aim, and the room a seat talks in is [`units[].channel`](../concepts/organization-model.md), which the executor prompt renders as the team channel for whichever chat backend the company runs.

Write this block **first**, with `${VAR}` placeholders — the automated provisioning below reads the placeholder names out of the YAML and fills exactly those variables in the sink you choose ([Where the secrets go](#where-the-secrets-go)). Whole-value placeholders are required (`"${SLACK_BOT_TOKEN_ENGINEER}"`, not a literal token) so the provisioner knows which variables to write. Declaring the block at all means declaring **both** credentials: a seat with only a `bot_token` receives messages it can never answer, and one with only a `signing_secret` answers 503 to every delivery while the app's own settings page reports a healthy request URL — so config validation rejects either half on its own, naming the missing field. Whether each one is a placeholder or a literal is a separate question, and config validation's only opinion on it is that a `bot_token` is one or the other — never a reference inside other text (`xoxb-${REST}`), which the transport, resolving the token only when it is one whole `${VAR}`, would send as written: a literal marks a credential you manage by hand, so `crewlet slack provision` reports that half and leaves it alone rather than editing your company document. A **mixed** pair therefore loads and runs, but the seat is only half provisionable — the run says which half and why, and the other value stays yours to paste in.

---

## Automated Setup: `crewlet slack provision`

```
crewlet slack provision company.yaml -secret-store -public-url https://your-server.com
```

For every Slack-enabled agent seat (a role whose `integrations.slack` credentials are `${VAR}` references) the command:

1. Builds the canonical app manifest — app name from `role.name`, bot display name from the handle, the [bot scopes](#bot-scopes-and-events) both credential consumers need, event subscriptions pointed at `<public-url>/webhooks/slack/{handle}`, and the OAuth redirect at `<public-url>/webhooks/slack-oauth`.
2. Calls [`apps.manifest.create`](https://docs.slack.dev/reference/methods/apps.manifest.create/) on the first run, or `apps.manifest.update` on later ones, keyed by the local ledger. **A ledgered app whose manifest fingerprint has not changed is skipped entirely** — the manifest endpoints are Tier 1 rate limited to roughly one request a minute, so a no-op re-run over seven seats would otherwise spend minutes achieving nothing.
3. Records the app in the ledger (`slack-apps.json` beside the company YAML) and writes the returned **signing secret** into whichever `${VAR}` the YAML's `signing_secret` points at, through the sink you chose.
4. Walks you through the one step Slack has no API for: **an authorize click per app.** It prints the install URL; after you click **Allow** the browser lands on the API's `/webhooks/slack-oauth` page showing a short-lived code — paste it back and the command exchanges it (`oauth.v2.access`) for the **bot token**, recorded into the `bot_token` variable. If you cancel, or Slack doesn't approve the install, the page says so in its own words and tells you to run the command again; Slack's error code is in Crewlet's log as `slack_oauth_refused`.

**One seat failing does not cost the others.** A mistyped code paste or a refused manifest is recorded against that handle, the remaining seats still provision, and the command exits non-zero naming what failed. Everything completed is durable — the ledger is written after every mutation — so re-running resumes rather than starting over.

Afterwards: invite each bot to its channels (`/invite @handle`), and do what the run's `Next:` line says so the engine reads the new credentials — with `-secret-store`, re-activate the current revision on a running node (`POST /config/reload`), or apply the company document that adds the seats' blocks (`crewlet config import`), since an apply is when the engine reads its secret store; with `-env-file`, source the file into the engine's environment and restart it. The line is printed only when the run recorded a value a running engine does not hold yet: a re-run that kept every app has nothing to say.

> **A bot token carries only the scopes it was minted with.** Pushing a new manifest does not give an existing token a new scope — the app has to be installed again, which is what `-reinstall` is for. It is destructive on its own: the new install revokes the token every running node is currently authenticating with, so plan the step the run's `Next:` line names straight after it — a reload with `-secret-store`, a restart with `-env-file` or `-print`.

### Where the secrets go

The same three-way choice every provisioning command in Crewlet takes, and there is **no default** — a run with nowhere to put what it mints would create live credentials and print none of them:

- `-secret-store` — the fleet's [encrypted secret store](../concepts/secret-store.md), written through this node's engine while it runs and, with it stopped, reached by the command itself through the node's own broker or the external one ([Which store the CLI writes](../concepts/secret-store.md#which-store-the-cli-writes)); `-config` names this node, and `-api` a running node to write through instead. A running engine resolves through a snapshot of the store it rebuilds at each configuration apply, so re-activate the current revision afterwards with `POST /config/reload` to a node that is up — [`crewlet config activate`](../reference/cli.md#crewlet-config-activate), the same gesture offline, opens the node's database and so runs only while its engine is stopped; a stopped engine reads the values when it starts ([where a command reads the company's values](../reference/cli.md#where-a-command-reads-the-companys-values)).
- `-env-file PATH` — a `.env` file, which then has to be sourced and the engine restarted.
- `-print` — stdout, for moving the values into a password manager or a deployment system this binary knows nothing about.

**The store outranks the other two.** The engine resolves every `${VAR}` from the [secret store](../concepts/secret-store.md) before its environment, so a bot token installed into an env file or printed, for a seat whose `bot_token` variable the store already holds, would never be read — while the install revokes the token the store holds. So a real run with `-env-file` or `-print` asks the store which `signing_secret` and `bot_token` variables it holds of the seats the run provisions (those whose two credentials are each a whole `${VAR}`) — by its listing, revealing nothing, and leaving the `bot_token` variables out under `-no-install`, which installs nothing — and where it holds any it is refused before it creates or installs anything, naming them: record with `-secret-store`, or remove the stored value first with `crewlet secrets unset`. Asking is a read of the store like any other, so such a run decides its route as a `-secret-store` one does (see the `-config` / `-api` row below) and is refused where that route cannot reach the store, as on a member of a cluster whose engine is stopped — see [a sink the store outranks](../reference/cli.md#a-sink-the-store-outranks).

### One-time bootstrap: the app configuration token

The manifest APIs authenticate with an **app configuration token**, which Slack only issues manually: go to [api.slack.com/apps](https://api.slack.com/apps) → **Your App Configuration Tokens** → **Generate Token**, then pass its *refresh* token:

```bash
export SLACK_CONFIG_REFRESH_TOKEN="xoxe-1-..."     # or pass -config-token
```

The run exchanges it for a 12-hour access token and **records both in the ledger before using either**. That ordering is not tidiness: Slack's rotation is single-use in both directions — the call that returns a new refresh token invalidates the one it was given — so a run that rotated and failed to persist the result would lock you out of your own apps. For the same reason a still-valid access token is reused rather than rotated again, and the shell export above only ever bootstraps the first run.

### Ordering and URL verification

Run the API server **before** provisioning, publicly reachable at `-public-url`:

```bash
crewlet run -config crewlet.yaml -roles data,ingress -api-host 0.0.0.0 -api-port 8000   # unconfigured is fine
```

A real provisioning run beside it reads the store through it, so set `CREWLET_API_TOKEN` for that run: one of Tier A's `api.auth` tokens with `-secret-store`, and any token permitted to list secrets with `-env-file` or `-print`.

Slack verifies each app's events **Request URL** with a `url_verification` challenge, and the API answers it unconditionally — no engine, company config or credentials needed. That exemption is deliberate and safe: the response is a pure echo of the caller's own challenge, and it has to work because during provisioning the signing secret does not exist yet, so a verified handshake would be impossible and the app could never be installed. If an app's Request URL still shows *unverified* in its settings page, click **Verify** there.

Slack requires HTTPS for both URLs — for local development put a tunnel (ngrok, cloudflared) in front and pass its URL.

### Flags

| Flag | Description |
|------|-------------|
| `-public-url URL` | Public HTTPS base URL of the Crewlet API. **Required**: every app's request URL and redirect URL are built from it, so an app created without one delivers nowhere and cannot be installed. |
| `-secret-store` / `-env-file PATH` / `-print` | Where the minted bot token and signing secret go — exactly one, no default. |
| `-config-token TOKEN` | The app-configuration **refresh** token; empty reads `$SLACK_CONFIG_REFRESH_TOKEN`. **Bootstrap only** — see [The ledger beats the shell](#the-ledger-beats-the-shell). |
| `-ledger PATH` | The app ledger (default: `slack-apps.json` beside the company YAML). |
| `-config PATH` / `-api URL` | The Tier A file (default `crewlet.yaml`), which names this node's store and the secret keyring `-secret-store` seals each value under, and a running node to go through in place of the one it names — how a run goes from a node that holds no data, from a member of a cluster whose engine is stopped, or from a machine that is not a node. This command reads none of the company's `${VAR}`s, and neither a `-dry-run` nor a run with nothing to provision opens a store — a seat is provisioned only where its `bot_token` and `signing_secret` are each a whole `${VAR}`, and one with a literal credential is reported and left alone. A real run with seats to provision does, whatever its sink, and decides its route before the plan prints: with `-secret-store` it reads back, in one batch, the bot token of each seat it provisions whose app the ledger records installed, to leave that install alone, and records through the same route — an app nobody has installed yet, one the run creates among them, has no token anywhere to keep, so it is installed without its token being read, whatever the store holds; with `-env-file` or `-print` it asks only which of those seats' variables the store holds (see [Where the secrets go](#where-the-secrets-go)). The route is the running engine — with `CREWLET_API_TOKEN` set to one of its Tier A tokens for `-secret-store`, since reading an installed app's token back is a reveal (a run whose ledger records none of its apps installed reveals nothing, but the run after an install does), or to any token the listing serves for `-env-file` and `-print` — the node `-api` names, or, with this node's engine stopped, the fleet's store itself, which the run reaches through the node's own broker or the external one. Without `-api`, on a node that holds no data or a member of a cluster whose engine is stopped, a real run is refused before it creates or installs anything, whatever its sink, `-reinstall` included. See [where a command reads the company's values](../reference/cli.md#where-a-command-reads-the-companys-values) |
| `-handles a,b` | Only provision these handles. Worth having against a method that allows about one request a minute: fixing one seat in a company of twenty should not cost twenty minutes. |
| `-reinstall` | Redo the OAuth install even where a bot token is already recorded. Required for a scope change to take effect, and destructive — it revokes the seat's current token. |
| `-no-install` | Create and update the apps and write the signing secrets, then print the authorize URLs instead of asking for codes. For a non-interactive run, e.g. pushing a scope change from CI. |
| `-dry-run` | Print the plan **and check every manifest** through `apps.manifest.validate`, which writes nothing. No app is created, no manifest pushed, no install run — and no sink is opened, since a dry run records nothing: it needs none of `-secret-store`, `-env-file` and `-print`, and an env file it names that does not exist yet is not created. The check is the reason a dry run touches the network at all: `apps.manifest.create` is rate limited to roughly one request a minute, so discovering a malformed manifest from the create costs a minute per seat *and* leaves the seats before the bad one already created. Validating needs a config token, so a dry run that cannot get one prints the plan and says the manifests were not checked — that is a note, not a failure, because an operator who has not made a config token yet is exactly the person reading the plan. It does make one write: getting the config token may rotate it, and a rotation has to be persisted the moment Slack answers. |

### The ledger (`slack-apps.json`)

Maps each handle to its Slack `app_id`, the fingerprint of the last-pushed manifest (what makes unchanged re-runs free), and the credentials **Slack only returns once, at creation**: the OAuth client id and secret (needed to redo an install later) and the signing secret. It also holds the rotating app-configuration token pair.

#### The ledger beats the shell

Slack's config-token rotation is **single-use in both directions**: every successful rotate invalidates the refresh token it was given. So the value in a `SLACK_CONFIG_REFRESH_TOKEN` export is dead the moment this command first used it, and preferring it over the ledger's stored pair would trade the only live way back into the operator's apps for a token Slack has already retired — on every run after the first, for ever. `-config-token` and the variable therefore **seed a ledger that holds nothing, and are ignored once it does**. There is no way to force the shell's value short of clearing `config_token` out of the ledger by hand, which is the honest shape: if the stored pair is wrong, the pair is what has to change.

#### Recovering from an app deleted in the console

A ledger entry naming an app that no longer exists is worse than no entry: its manifest fingerprint still matches, so the seat reads as **kept**, while the bot token in its `${VAR}` authenticates as nothing. Every run therefore probes each recorded app with `apps.manifest.validate` before deciding anything — one call against an id the run already holds — and reads `app_not_found` / `invalid_app_id` / `invalid_app` as *gone*. A gone app is replaced: the entry is dropped, a fresh app is created, and the run says so. The install re-runs even though the `${VAR}` still holds a value, because the replacement record carries no bot user id, which only a completed OAuth exchange records — so the install runs without consulting the stale token, and overwrites it rather than trusting it.

A **permission** refusal is not an absence and is never treated as one: an app this credential may not touch still exists, and replacing it would leave two apps for one seat.

#### Adopting an app you created by hand

Create the app in the Slack console, then add its entry to the ledger yourself:

```json
{"apps": {"swe": {"app_id": "A0…", "client_id": "…", "client_secret": "…", "signing_secret": "…"}}}
```

From there `crewlet slack provision` takes over — it pushes the manifest, records the signing secret and runs the install. A **half**-seeded entry is refused naming exactly what to copy: an app id alone builds an authorize URL with an empty `client_id`, which Slack answers with a page saying nothing useful, or an `invalid_client_id` from the exchange minutes later.

#### One authorize URL per seat, and they look alike

The OAuth exchange's answer names the app it was for, and the run **refuses a code belonging to a different app than the seat's**, recording nothing. Pasting the URL printed for one agent into another agent's prompt would otherwise mint that app's bot token into this seat's `${VAR}` — and the seat would post as a colleague, with nothing anywhere reporting it. An app per seat is only an identity boundary if the identities cannot cross.

It is a **secrets file** — written `0600` through a temp file and a rename, because a truncate-then-write interrupted half way would destroy values that cannot be read back. It is gitignored by name in the repo's own `.gitignore`; if you keep your company document elsewhere, gitignore it there too. Committing it publishes credentials nothing can rotate for you.

Why a file at all, when no other third-party app needs one: two of those four values have no field in the company config (nothing in the running engine reads a client id), and Slack has no method that reads them back. Deleting the ledger makes the next run create duplicate apps, since Slack has no API to list the ones you already have.

### Bot scopes and events

The single source of truth is `internal/slack` (`BotScopes` / `BotEvents`); the manual steps below list the same values. The scopes cover **both** consumers of the bot token — the notification transport and every tool enabled on the Slack MCP server:

| Scope | Used by |
|-------|---------|
| `app_mentions:read` | `app_mention` events (thread-follow trigger) |
| `channels:history`, `channels:read` | public channels — thread routing, the engine's own turn-start thread read, + MCP `conversations_history` / `conversations_replies` / `channels_list` |
| `chat:write` | the working indicator (`assistant.threads.setStatus`) + MCP `conversations_add_message` |
| `files:read` | shared-file notifications |
| `groups:history`, `groups:read` | private channels — thread routing, the engine's own turn-start thread read, + MCP `conversations_history` / `conversations_replies`; `groups:read` **required**, see the note below |
| `im:history`, `im:read`, `im:write` | DMs, incl. escalation DMs to human seats — and the engine's own turn-start thread read, which is the case it exists for: a DM reply is the thinnest trigger there is |
| `mpim:history`, `mpim:read` | group DMs — thread routing, the engine's own turn-start thread read, + MCP `conversations_history` / `conversations_replies`; `mpim:read` **required**, see the note below |
| `reactions:write` | MCP `reactions_add` / `reactions_remove` |
| `usergroups:read`, `usergroups:write` | MCP `usergroups_*` tools |
| `users:read` | MCP `users_search`, sender attribution |

> **`groups:read` and `mpim:read` are required, not optional.** On startup
> [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) builds its
> channel list with a single `conversations.list` call that covers **all four**
> conversation types — `public_channel`, `private_channel`, `mpim`, `im` — and the
> set is hard-coded (there is no env var to narrow it). If the bot token is missing
> `groups:read` (private channels) or `mpim:read` (group DMs), Slack rejects the
> whole call with `missing_scope` and the server logs `Failed to fetch channels`.
> What follows depends on whether a channel cache from an earlier run is on disk.
> With none, the server exits at startup (`Error booting provider`: `API returned
> zero channels and no existing cache is available`), so the seat has **no Slack
> tools at all**, and the engine reports those lines as the server's
> `server_stderr_tail`. With one, it keeps serving that old list and never sees a
> channel created since. The fix is always: grant the missing scope **and reinstall
> the app** to mint a new token.
>
> No search scope is requested, `search:read.public` included: `slack-mcp-server`
> registers `conversations_search_messages` only for a user token, because Slack's
> `search.messages` refuses a bot's, and nothing else an agent's token is used for
> searches. Bots also only see channels they have been **invited** to — granting the
> read scopes lets the bot *list and read* those channels, but membership is still
> required.

Bot events: `app_mention`, `message.channels`, `message.groups`, `message.im`, `message.mpim` — one `message.*` event per conversation type the transport handles, so a bot invited to a private channel or group DM wakes on non-mention messages and thread replies exactly like in public channels. (The transport dedups the message/app_mention double delivery by `handle:channel:ts`.)

---

## Manual Setup

The click-through equivalent of the provisioner, for when you cannot (or do not want to) use configuration tokens.

**Do this from Company › Integrations if you can.** Connect Slack there and every agent gets its own block carrying **the manifest its app is created from**: the same definition `crewlet slack provision` pushes, with that agent's scopes, events and request URL already in it. Copy it, paste it into Slack, install, and paste the two values back. That is Steps 1 to 3 below in one paste, and it removes the failure this section's hand-built path invites, which is a single missing scope (see the [cache note](#bot-scopes-and-events)) turning into a bot that installs, reports success and sees an empty workspace.

The steps below are the same thing done by hand.

### Step 1: Create a Slack App Per Agent

For each agent that will use Slack, create a dedicated Slack app:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App**
2. Choose **From an app manifest**, and paste that agent's manifest from Company › Integrations. Steps 2 and 3 below are then already done. The wizard installs it for you: select your Slack workspace > **Next** > **Create and Install** > **Allow**, and it finishes on the page holding the app's credentials, copy the **Bot token** value from **Your app credentials** there, and the **Signing Secret** from **Basic Information** > **App Credentials**. That is the whole of Step 2 for a manifest app.
3. Choosing **From scratch** instead leaves the scopes, the events and the request URL for you to set by hand, which is the rest of this section. Name it after the agent (e.g., "Crewlet Engineer", "Crewlet Designer"), select your workspace and click **Create App**.

### Step 2: Configure Each App's Tokens

**Only for an app built from scratch.** A manifest app already carries its
scopes, and Step 1 says where its tokens are — following the walk below
would send you to a page that is not where the wizard left you.

For each app:

1. Go to **OAuth & Permissions** in the sidebar
2. Under **Bot Token Scopes**, add **every** scope from the [table above](#bot-scopes-and-events)
3. Click **Install to Workspace**, or **Reinstall to Workspace** if you added scopes to an existing app (new scopes only take effect on a freshly minted token, so adding a scope without reinstalling changes nothing)
4. Copy the **Bot User OAuth Token** (`xoxb-...`)
5. Go to **Basic Information** > **App Credentials** and copy the **Signing Secret**

If you cancel the install, or Slack doesn't approve it, the page Slack sends you
to says so and tells you to go back to the app's settings in Slack and install it
again.

### Step 3: Enable Events API (Per App)

For each app:

1. Go to **Event Subscriptions** > toggle ON
2. Set the **Request URL** to the agent's webhook endpoint:
   ```
   https://your-server.com/webhooks/slack/{handle}
   ```
   Replace `{handle}` with the agent's handle (e.g., `engineer`, `tech-lead`).
3. Subscribe to bot events: `app_mention`, `message.channels`, `message.groups`, `message.im`, `message.mpim`
4. Click **Save Changes**

Then paste each pair into that agent's block in Company › Integrations, or store each value under the name your YAML references with [`crewlet secrets set`](../reference/cli.md#crewlet-secrets-set) — the engine reads it at its next apply, `POST /config/reload` on a running node, as the command says after the write — or export them as `SLACK_BOT_TOKEN_*` / `SLACK_SIGNING_SECRET_*` in the engine's own environment before it starts. The engine reads no `.env` file ([Environment variables](../reference/environment-variables.md)).

> **The `signing_secret` is what makes the endpoint usable.** `/webhooks/slack/{handle}` is
> exempt from the API's bearer token because it verifies Slack's own signature instead — so
> until the secret is set there is nothing to verify with, and the route answers `503` with
> `Retry-After` rather than accepting the delivery. Slack retries, and deliveries flow the
> moment the secret is configured; nothing is lost in the meantime.
>
> An unsigned delivery is never recorded, published, or shown on the dashboard. Earlier
> releases let one through when *no* Slack secret was configured anywhere — the payload could
> not wake an agent (the transport re-verifies and refuses), but it did reach the event store
> and every connected dashboard, attacker-controlled text and all.

---

## Reaching humans on Slack

When an agent escalates to a [human seat](../concepts/humans-in-the-org.md),
it DMs the human's `contact.slack_user_id` with **its own** bot token —
the same credential it uses for every other Slack message. There is no
org-level "system" Slack app and no extra bot token for human seats:
the engine never sends as itself. If the escalating agent has no Slack
app of its own it can't DM the human directly — that's a config gap to
fix (give the agent a Slack app, or route the work through a colleague
who has one), not something the engine papers over with a shared
identity.

---

## How Slack Routing Works

### Inbound

1. Human posts in a channel where the bot is present
2. Slack sends webhook to `https://your-server.com/webhooks/slack/{handle}`
3. The API verifies the `X-Slack-Signature` v0 HMAC **at the edge**, against that handle's own signing secret, before the payload is persisted, streamed to a dashboard or published. The timestamp is part of the signed string and is checked against a replay window, which is what stops a captured request from working for ever. This is the **only** verification: nothing downstream checks again, which is why the route is exempt from the API's bearer token and why the check has to be here.
   - No signing secret for **any** seat → `503`. "Cannot verify" is not "nothing to verify": a node with no secrets loaded must not accept an unsigned POST addressed at any handle.
   - A secret map that is populated but does not name this handle → `401 invalid_signature`. That is a delivery for a seat with no Slack app, not a node that cannot check — and it is answered exactly as a wrong signature on a real seat is, after the same signature check, so a request from outside cannot tell which handles have an app. The engine's log names it `slack_webhook_unknown_handle`.
4. The delivery is claimed fleet-wide on Slack's own `event_id`, which is stable across its retries — so a redelivery, or a message that arrives twice because the app subscribes to both `message.*` and `app_mention`, wakes the seat once.
5. The API publishes to `crewlet.notifications.inbound` on the EventQueue.
6. The notification service resolves the handle to its seat and publishes to that seat's own mailbox, `crewlet.agent.{seat-id}.inbox`.
7. The agent's handler fires.

#### Which events wake an agent

Only `message` / `app_mention` events that carry **new user-visible content** are delivered: regular messages, `thread_broadcast` replies, file shares (a share without a comment renders as `(shared file: …)` so the body is never blank), and other bots' messages (legacy `bot_message` events resolve the sender from `username` / `bot_id`).

Slack reuses `type: "message"` for channel **bookkeeping**, and those events are skipped instead of waking the agent with an empty notification — logged at debug as `slack_event_skipped` with the reason, not recorded as a `NotificationSkipped` event, because they concern nobody and a skip row for each would bury the drops that do matter (a seat no recipient matches, the routing gate, the rate valve) under a busy channel's ordinary traffic:

- `message_changed` — edits, **including Slack's own link-unfurl edits** of a message an agent just posted
- `message_deleted` — deletions
- `message_replied` — thread-reply bookkeeping; Slack delivers it **without its subtype** (a documented Slack bug), so it is recognized by its `hidden: true` flag
- system subtypes (`channel_join`, `channel_leave`, `channel_topic`, …) — lines *about* the channel, not messages *to* anyone

These envelopes have no top-level `user`/`text`; delivering them would produce phantom agent turns triaging an empty message.

#### The seat's own messages

A seat never wakes on its own post, and the check is made **twice** because Slack echoes a bot in two shapes: an ordinary post carries `user` equal to the bot user id, while one made through an incoming webhook or with a custom username arrives as a `bot_message` with no `user` at all and only the app id to identify it. Missing either test makes the seat answer itself — one turn per reply, for ever.

An agent's own reply in a thread **subscribes it to that thread**, exactly as replying does in any chat client: it hears what comes back without having to be named again.

### Outbound (via Slack MCP tools)

All Slack capabilities an agent uses deliberately — messaging, threading, reading history, reactions — are **MCP tools** powered by that agent's own bot token via [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server), so a message comes from the agent rather than from a shared company bot. Posting and reacting exist only where the server entry sets [`SLACK_MCP_ADD_MESSAGE_TOOL` and `SLACK_MCP_REACTION_TOOL`](#configure-in-yaml). Searching messages is not among them: the server offers `conversations_search_messages` only to a user token, because Slack's `search.messages` refuses a bot's.

The engine's own transport posts no messages at all. It holds the same per-seat token for two calls of its own: `auth.test`, which resolves the seat's identity at start, and the [working indicator](#working-status-is-thinking).

---

## Working Status ("is thinking…")

An agent turn takes time — an executor → reviewer pass with tool calls
routinely runs minutes. Without a signal, the human who posted sees
nothing until the reply lands and cannot tell "the bot is working" from
"the bot is dead". Crewlet closes that gap: while an agent reasons about a
Slack message it shows a **working status** in the thread — "*Agent SWE is
thinking…*" under the composer — and takes it down once nothing is working
behind it.

### What Slack actually supports

Slack has **no public typing API for bots**. The classic `user_typing`
signal lived on the RTM API, which granular-permission apps cannot use —
this is the long-standing ask in
[slackapi/bolt-js#885](https://github.com/slackapi/bolt-js/issues/885) (and
its duplicate [#2580](https://github.com/slackapi/bolt-js/issues/2580)).

The supported mechanism is
[`assistant.threads.setStatus`](https://docs.slack.dev/reference/methods/assistant.threads.setStatus/),
which renders a working-state line in the thread. It used to require
`assistant:write` and an AI-assistant split-view app; since
[March 2026](https://docs.slack.dev/changelog/2026/03/05/set-status-scope-update/)
it accepts plain **`chat:write`**, so ordinary channel apps can use it.

**Every Slack-enabled Crewlet agent already holds `chat:write`** (Step 2
above), so this needs **no new scope, no app-manifest change, and no
reinstall**.

### Behaviour

Each phase draws one line from its own pool, so the indicator reads as
movement rather than a fixed label:

| Turn phase | Drawn from |
|---|---|
| First-turn onboarding | *is getting crewleted in…* · *is settling in…* · *is finding the coffee machine…* · … |
| Execute | *is crewleting…* · *is thinking it through…* · *is cracking on…* · … |
| Review | *is re-crewleting…* · *is double-checking…* · *is marking its own homework…* · … |

Those are the phases a turn has: the executor decides *and* acts in one pass,
so there is no separate planning line. The full pools are `PhasePhrases` in
`internal/notify/phrases.go`; replace any of them with
your own wording via [`status_phrases`](#custom-status-phrases) below.

Every line describes the **phase**, never a specific action. The pick is
arbitrary — nothing consults what the agent is doing — so a line naming
work it merely *could* be doing ("is consulting the org chart…", "is
reading the handbook…") would read as a status report and be wrong most
of the time it appears, which teaches the reader to distrust the whole
indicator. Generic ("is thinking…") or plainly figurative ("is finding
the coffee machine…") is safe; plausible-and-specific is not.

**When it goes up, when it is held and when it comes down is the turn's, not
this page's.** Every point in that lifecycle — including what a detached
[sandbox](../concepts/code-sandbox.md) run does to it, which is where most of
its rules are — is the table in
[Turn Engine § The working status](../concepts/turn-engine.md#the-working-status),
and that table is the only copy of it. It is identical on Mattermost; what
differs between the two backends is only what the indicator can *say*. What
follows is what is true of **this** backend and of no other.

- **One line per phase, held for that phase.** The pick is deterministic
  in `(turn_id, phase, how many phases the turn has been through)`, so the
  45 s heartbeat re-asserts the *same* words — text that churned mid-phase
  would read as the agent restarting. Moving to the next phase draws the
  next line, and a `self_iterate` loop back into Execute draws a different
  one than that phase showed the first time, so a second pass is visible
  instead of looking stuck. Two turns in one thread start from different
  points in the pool, because the seed is the turn's own id.
- **Kept alive across long turns.** Slack expires a status after 2
  minutes; the engine re-asserts it every 45 s (two attempts inside every
  expiry window, ~1.3 requests/min against Slack's 600/min per-app limit).
- **Slack clears it by itself the instant the agent posts into the thread.**
  The engine re-asserts only while a later phase is still running, which is
  what keeps the indicator honest across a `self_iterate` loop.
- **A refused or rate-limited `setStatus` is logged and the status simply
  expires.** Nothing about the indicator can fail a turn, and nothing about
  it delays one.
- **Two minutes is what is left standing when this process cannot clear
  it.** A process killed outright leaves its last indicator to that expiry,
  and a `PUT /config` that rebuilds the Slack transport clears the ones it
  was holding — turns in flight then run without an indicator until they
  end, and their replies land as usual.

### `typing_status` modes

```yaml
integrations:
  slack:
    typing_status: always    # default
```

| Mode | Shows the status when… |
|---|---|
| `always` *(default)* | every Slack-triggered turn, including passive top-level channel messages and `@here` / `@channel` broadcasts |
| `addressed` | a human is plausibly waiting on **this** agent: a DM or group DM, a direct `@mention` (including `app_mention`), or a thread the agent is in because it was named there — a thread it merely spoke in, or one a broadcast started, does not count |

> **There is no `off`.** What it bought was a company whose agents think in
> silence for minutes at a time, which is the state this feature exists to
> remove. A workspace where several agents light up for one message wants
> `addressed`, which is the same judgement made per message rather than once
> for the deployment.

`addressed` deliberately excludes passive channel traffic, collective
addresses, and the replies in a thread the agent only spoke in or that a
broadcast started. Every bot in a
channel is woken by a top-level message, and the
[triage prompt](../concepts/turn-engine.md) tells most of them to stay
silent — so `always` in a shared channel with five agents lights up five
indicators for a message none of them will answer. Use `always` in a
single-agent workspace, or where agents are expected to weigh in on
everything.

The setting is org-wide and live-editable: a `PUT /config` that changes it
rebuilds the Slack transport in place, no restart.

### Custom status phrases

The built-in pools lean on the engine's own name — a company running
Crewlet will want its own verbs. Override any phase under
`status_phrases`; every phase you leave out keeps its built-in pool.

```yaml
integrations:
  slack:
    typing_status: addressed
    status_phrases:
      onboarding: ["is getting nimbused in...", "is settling in..."]
      execute:    ["is nimbusing...", "is on the case...", "is cracking on..."]
      review:     ["is re-nimbusing...", "is double-checking..."]
```

Rules that keep the indicator readable:

- Slack renders the line **after the agent's name**, so each phrase has to
  finish that sentence — `"is nimbusing..."` shows as *Agent SWE is
  nimbusing…*.
- **Describe the phase, not the work.** A phrase is picked arbitrarily, so
  anything specific enough to sound like a report ("is checking Jira…",
  "is reading the spec…") is a claim the agent usually isn't fulfilling —
  and one caught mismatch costs the reader's trust in every line after it.
  Keep phrases generic to the phase or plainly figurative.
- A phase with **one** phrase is a fixed label; more phrases give it
  variety across turns. Either is fine — the engine holds one line for the
  whole phase regardless.
- An **empty list** (or an omitted phase) keeps the built-in pool, and so
  does a list with nothing usable left in it: a blank string is dropped
  rather than shown, because an empty status doesn't render — it *clears*
  the indicator.
- `default` covers any future phase with no pool of its own. You rarely
  need it.

Like `typing_status`, this is live-editable — a `PUT /config` swaps the
wording without a restart. The apply rebuilds the Slack transport, which
clears the indicators it was holding: a turn already in flight finishes
without one, and the next turn in that thread opens on the new wording.

---

## Thread Routing

By default, agents only receive thread replies in threads they are **following**. Top-level channel messages are always delivered.

**Follow triggers:**

1. **Direct mention** — `<@BOT_USER_ID>` or `app_mention` event
2. **Collective address** — `<!channel>` or `<!here>`
3. **Participation** — the agent replies in the thread. Because the reply goes out through the Slack MCP tools rather than the engine, the follow is recorded from Slack's own **echo** of that message: the parser writes it on the way past as it suppresses the seat's own post, which is also what lets a node record a follow for a seat it is not running.

A **top-level** message that mentions the agent, or the room, starts the thread its own `ts` becomes, so the agent hears the answers without being named again — and it follows that thread for the same reason. The reason is what decides whether a reply [obliges an answer](../concepts/turn-engine.md#who-is-waiting-and-how-they-get-an-answer) and raises the [`addressed` indicator](#working-status-is-thinking): a thread the agent was named in does, while one a broadcast started, or one it merely spoke in, only lets it hear what is said there. A follow keeps the **strongest** reason it was ever recorded for — a mention, then a collective address, then participation — so an `@here` in a thread the agent was named in reaches it as a broadcast and still obliges an answer, and the agent's own posts refresh a follow without weakening it. That holds when two messages in one thread are parsed on two nodes at once: each write is decided against the record it replaces.

Thread tracking state is persisted in the fleet's [coordination store](../concepts/coordination.md), keyed by backend, so it survives engine restarts *and* is visible to whichever node claims the next reply — an inbound message is parsed by one node of the fleet, and a follow only that node could see would make a non-mention reply reach its seat by chance. Bot messages are automatically ignored to prevent loops.

A follow is dropped after **90 days without activity**, and nothing else ends one — there is no way to make an agent stop following a thread sooner. Nothing sweeps it either: the horizon is the coordination bucket's own age, and every re-assert (a mention, a collective address, the agent posting) rewrites the record — a reply that only rides the follow is not one — so the age is a true last-activity stamp rather than a creation date and the broker expires what has gone quiet. The asymmetry is what sets the number: a dropped stale follow costs at most one missed non-mention reply, and the next mention re-follows through the ordinary path above, while keeping every follow forever grows a record that is read on the hot path of every inbound message.

