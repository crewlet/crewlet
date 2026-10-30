# Slack Integration

Crewlet uses a **one Slack app per agent** model — each agent gets its own bot identity, token, and webhook endpoint in Slack.

> **Prerequisites.** You need a Slack **workspace you administer** (create one if you don't have it). The engine's API endpoint must be **reachable by Slack over public HTTPS** so the Events API can deliver webhooks, and so the OAuth install can land (for local development, use a tunnel such as ngrok or cloudflared).

> **This is one of three chat backends, and the axis is exclusive.** A Slack app — the org-level `integrations.slack` block **or any seat's own**, since Slack's credentials are per seat — puts this company on `chat.backend: vendor`. The engine's own chat ([Chatlet](../concepts/chatlet.md)) is what a company gets when it declares no vendor chat surface at all, and naming `chat.backend: chatlet` beside a Slack app is **refused at validation**: two live chat homes means a person replies in one while the agents read the other, with no rule anywhere to say which conversation was real. Slack and [Mattermost](mattermost.md) together are fine — two workspaces with different people in them is what an org migrating between them looks like for months.

There are two ways to create those apps:

- **[Automated (recommended)](#per-agent-access-from-the-agents-tab)**: the engine creates and keeps every app through Slack's [App Manifest APIs](https://docs.slack.dev/app-manifests/configuring-apps-with-app-manifests/). One app configuration token entered on the connect form, one **Allow** click per agent from the Agents tab, and every secret Slack serves is sealed in the fleet's encrypted secret store under a name the engine derives for the seat.
- **[Manual](#manual-setup)**: click through [api.slack.com/apps](https://api.slack.com/apps) per agent, and hand each app's two values in on the connect form.

Both end in the same place: each agent's own app, its credentials sealed and bound to its transport and its Slack tool ([Configure in YAML](#configure-in-yaml)).

---

## Setting it up from the dashboard

Company › Integrations shows one section per agent, because on Slack the
credentials belong to the seat: each agent has its own app, so each has its own
bot token and signing secret. Paste the two values Slack shows on the app's own
page and the engine seals them, gives the seat a `${VAR}` pointing at each, and
activates. Each seat's own delivery address is shown beside its fields.

**Without an app-configuration token it does not create the apps.** Creating
one goes through Slack's app-manifest API, which authenticates with an app
configuration token Slack issues only by hand from its own pages, and which
your organisation may not permit at all. Where one is sealed, the engine
creates and keeps every agent's app itself ([Per-agent access from the Agents
tab](#per-agent-access-from-the-agents-tab)). Where one is not,
connect Slack from the dashboard's Company › Integrations: every agent gets its
own block there carrying the manifest its app is created from, so building each
app is a copy, a paste into Slack's **From an app manifest** flow, an install,
and the two values pasted back. See the [manual setup](#manual-setup) for the
same steps done entirely by hand.

### Per-agent access from the Agents tab

Each agent is turned on or off on the integration's **Agents** tab, and the
engine keeps its app in line on every pass. Slack has no roles: an agent's app
holds the bot scopes in [its manifest](#bot-scopes-and-events), shown on the tab
read-only, and it reads and posts wherever it is invited. Nothing else is set
per agent.

**The engine creates the apps** once an app-configuration token is sealed for
the company. Create one at [api.slack.com/apps](https://api.slack.com/apps) and
paste its refresh token into **App configuration token** on Slack's connect
form, under Company › Integrations. The engine seals it as
`SLACK_APP_CONFIG_TOKEN`, points `integrations.slack.app_config_token` at it,
and from then on keeps the token pair in that one sealed value, rotating it
when less than half an hour of its life is left (a pass creating apps at
Slack's rate of about one a minute needs that long) and saving each new pair
before using it, because Slack's refresh token works once. A rotation and its
save finish even when the pass that started them is cancelled, and a token
Slack refuses as expired early is renewed once and the call retried. The field is always one whole `${VAR}`, never the
token itself. Without one, the engine does not manage the company's Slack apps,
and they are set up as described below.

The install stays a person's click: the tab shows a waiting step with
**Install**, which mints a link valid for fifteen minutes and once, and sends
you to Slack's approval page. When Slack sends you back to
`/webhooks/slack-oauth`, the engine checks the link, confirms the approval
belongs to that agent's app, and seals the bot token. A link used twice,
expired or edited is refused, and the page says what to do instead.

| Action | What happens at Slack |
|---|---|
| Turn on, first time | The engine creates the app from its manifest and seals its client secret and signing secret (served once, so an app that cannot be sealed is deleted again). A person approves the install. |
| Turn off | The engine revokes the bot token (`auth.revoke`). The app and its OAuth client are kept. |
| Turn back on | A person approves the install again; Slack issues a bot token only to an install. |
| Rotate | The engine revokes the bot token, and a person approves a reinstall, which issues a new one. |
| Remove (only when off) | The engine deletes the app (`apps.manifest.delete`) and clears every value sealed for it once Slack confirms. Where Slack refuses the configuration token, the tab links to the app's page to delete it by hand. |

A scope or setting changed in Slack's console is restored from the engine's
manifest on the next pass and reported. A bot token keeps the scopes it was
installed with, so where a scope was added or removed at Slack the engine also
revokes the bot token and a person approves a reinstall, which issues a token
with exactly the manifest's scopes. A bot token revoked or an app
uninstalled by somebody at Slack is never reinstalled automatically, in case it
was incident response: the tab offers **Turn back on** or **Keep off**.

**The engine names every value.** A seat's values are sealed as
`SLACK_BOT_TOKEN_<HANDLE>`, `SLACK_SIGNING_SECRET_<HANDLE>`,
`SLACK_CLIENT_SECRET_<HANDLE>` and `SLACK_CLIENT_ID_<HANDLE>`. The transport and
the seat's Slack tool server are both given the one bot token, so the two
cannot drift apart, and no document edit can point a seat's token at another
variable.

### Disconnecting

Disconnect turns every agent off first: the engine revokes each bot token, so
no agent posts once the disconnect has started. It then removes
`integrations.slack` from the company document, which retires the transport:
the engine stops talking to Slack and the per-seat webhook routes stop turning
deliveries into work.

**Tick *also remove the accounts Crewlet created*** and the engine deletes each
agent's app at Slack with the app-configuration token, clears the values
sealed for it (`SLACK_BOT_TOKEN_<HANDLE>`, `SLACK_SIGNING_SECRET_<HANDLE>`,
`SLACK_CLIENT_SECRET_<HANDLE>` and `SLACK_CLIENT_ID_<HANDLE>`) once Slack
confirms the app is gone, and removes each agent's `integrations.slack` block.
Where an app cannot be deleted that way (no usable configuration token, or
Slack refused it), the Agents tab links to its page,
**Settings › Basic Information › Delete App**, and the disconnect stays in
progress until every app is gone: dropping the integration first would drop
the only token that confirms the deletion, while an app nobody deleted could
still be installed again.

Leave it unticked and every app and its OAuth client is kept, so turning an
agent back on after a reconnect is an install a person approves.

## Configure in YAML

`integrations.slack: {}` (org-level) is a marker that enables the Slack **transport**; its one setting is [`typing_status`](#working-status-is-thinking). The Slack **MCP tool** server is a separate `mcp_servers` entry (`shared: false`). Per agent, the Slack identity has two consumers: the **transport** (the bot token and the signing secret), and the **Slack MCP subprocess**, which reads the same bot token as `SLACK_MCP_XOXB_TOKEN`. The engine binds the one sealed token to both, so neither is named in the document. The transport both writes and reads on that token: it raises the working indicator, and at the start of a turn woken in a thread it calls `conversations.replies` so the agent is handed the conversation rather than told to go and fetch it (see [the thread block](../concepts/agent-runtime.md#the-thread-a-turn-was-woken-in)). That needs no new scope, the `*:history` scopes the manifest already requests cover it, and it reads as that app, so a channel the bot is not in answers `not_in_channel` and the turn runs without the block. `conversations.replies` pages from the *oldest* end, so a thread past ~1000 messages cannot be read to its newest end: the agent is then handed the newest of what was reached plus a note that the block stops short and the rest has to be read with its chat tools, never a truncated thread presented as the whole one. A seat that is on needs only its `enabled`:

```yaml
integrations:
  slack:                                 # enable the Slack transport
    typing_status: always                # always (default) | addressed

mcp_servers:
  - name: slack
    shared: false                        # per-agent identity: the engine binds each seat's bot token
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
        integrations:
          slack:
            enabled: true                                 # the engine names and binds its credentials
        mcp_env:
          slack:
            SLACK_MCP_CHANNELS_CACHE: "/var/cache/crewlet/slack/engineer-channels.json"   # this seat's own
      - name: Designer
        integrations:
          slack:
            enabled: true
        mcp_env:
          slack:
            SLACK_MCP_CHANNELS_CACHE: "/var/cache/crewlet/slack/designer-channels.json"
```

**The two `env` switches are what let an agent answer.** [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) registers its write tools only when told to: without `SLACK_MCP_ADD_MESSAGE_TOOL` it never offers `conversations_add_message`, its one tool that posts, so a seat receives every message, reasons about a reply and has no tool to send it with, and the channel stays quiet while the server reports itself healthy. (The 1.1 releases that had the tool registered it and refused every call without the variable, which ends the same way.) `"true"` lets the agent post in every channel and DM its bot can reach; a comma-separated list of channel ids, or of `!`-prefixed ids to exclude, narrows that, as the server's own [settings](https://github.com/korotovsky/slack-mcp-server/blob/v1.3.0/README.md#environment-variables-quick-reference) describe — and anything else is read as such a list, so `"false"` registers the tool and lets it post only in a channel whose id is `false`, which is none. A narrowed list mutes the seat wherever it is addressed outside it, and a seat is addressed wherever its bot has been invited. `SLACK_MCP_REACTION_TOOL` is the same switch for `reactions_add` and `reactions_remove`, which the [`reactions:write`](#bot-scopes-and-events) scope is requested for. Neither is a credential, so both sit in the server entry's `env`, which every seat's instance starts with, rather than in each seat's `mcp_env`. And in `env` rather than in an `--enabled-tools` argument: the server registers a tool that argument names, but every call reads the environment alone, so a tool switched on only on the command line is offered and refuses each post. The release is pinned so a working run names the build it ran: `npm exec` with `@latest` runs whatever was published last, and a later release that changes what a tool is called, or what has to be set before it exists, would arrive unannounced. The bundled tool skills depend on it too: the mentions, channel, hand-off and directives skills trigger on this release's tool names under the entry's `slack_` prefix (`slack_conversations_add_message`, `slack_channels_me`, `slack_channels_list`, `slack_users_search`), so a seat posting on Slack is taught Slack's `<@member-id>` mentions and thread replies before its first post. Under another prefix, or after a release that renamed one of those tools, the skills would go quiet without a sound.

**Each seat names a channel cache of its own.** slack-mcp-server keeps the channels its bot can see in a file, and by default names that file after the workspace alone (`<team id>_channels_cache_v2.json`, in the user's cache directory) and serves it for 24 hours without asking Slack again. Every seat's server on one node runs as the same user against the same workspace, so without `SLACK_MCP_CHANNELS_CACHE` they share one file, and whichever wrote it last decides what `channels_list` tells the rest: the private channels and DMs that bot is in, named to seats that are not, and none of the reader's own. Give each seat a path no other seat uses. The directory has to exist for the list to outlive a restart; where it does not, the server keeps the list in memory and fetches it afresh at every start, which is still that seat's own.

The bot token drives the transport (the seat's own identity at start, and the working indicator) and, as `SLACK_MCP_XOXB_TOKEN`, the Slack MCP tools: two independent subsystems, one sealed credential. The signing secret is transport-only: it verifies this seat's inbound deliveries and nothing else reads it. Both are sealed as `SLACK_BOT_TOKEN_<HANDLE>` and `SLACK_SIGNING_SECRET_<HANDLE>`. There is no per-seat channel setting: the engine's transport posts no message, so it has nothing to aim, and the room a seat talks in is [`units[].channel`](../concepts/organization-model.md), which the executor prompt renders as the team channel for whichever chat backend the company runs.

**An app created by hand** (no app configuration token) is handed in as an external credential: its block names `bot_token` and `signing_secret` as whole `${VAR}`s, which the connect form writes when you paste the two values Slack shows on the app's own page. A `bot_token` is one whole `${VAR}` or a literal, never a reference inside other text (`xoxb-${REST}`), which the transport would send as written.

---

## Automated setup

The engine creates every agent's app once an app configuration token is on the
company block, and a person approves each install from the Agents tab: see
[Per-agent access from the Agents tab](#per-agent-access-from-the-agents-tab).

### Upgrading from `crewlet slack provision`

The retired `crewlet slack provision` command made each agent's app from your
machine and kept what Slack returned in a local ledger, `slack-apps.json`.
Nothing on the engine reads that file, so moving a company onto the engine's
own setup takes three steps.

1. **Enter an app configuration token on Slack's connect form.** Generate a
   fresh one from [api.slack.com/apps](https://api.slack.com/apps) (Your App
   Configuration Tokens) rather than reusing the pair in `slack-apps.json`,
   which Slack may have rotated or expired since the command last ran. On its
   next sync the engine adopts each app the command made by the seat's working
   bot token, so no app is created twice.
2. **Store each app's OAuth client.** Slack serves an app's client id and
   secret only once, at creation, so the command's ledger holds the only
   copy, and an adopted app cannot be installed again (after it is turned off,
   or for a rotate) until both are in the secret store. Pipe each value
   straight from the ledger so it is never shown on screen or written to shell
   history, once per agent handle:

   ```sh
   jq -r '.apps["senior-engineer"].client_id' slack-apps.json \
     | crewlet secrets set SLACK_CLIENT_ID_SENIOR_ENGINEER
   jq -r '.apps["senior-engineer"].client_secret' slack-apps.json \
     | crewlet secrets set SLACK_CLIENT_SECRET_SENIOR_ENGINEER
   ```

   The variable is the handle in upper case with each character other than a
   letter or digit written as `_`. Never pass these values with `-value`,
   which exposes them in the process list and shell history.
3. **Delete `slack-apps.json`** once every client is stored. It holds each
   app's client secret and signing secret in plain text, and the engine never
   reads it.

### Ordering and URL verification

The API must be publicly reachable at the deployment's webhook address before
an app is created: Slack verifies each app's events **Request URL** with a
`url_verification` challenge, and the API answers it unconditionally, with no
engine, company config or credentials needed. That exemption is deliberate and
safe: the response is a pure echo of the caller's own challenge, and it has to
work because an app's signing secret is sealed only once Slack has created the
app, so a verified handshake would be impossible and the app could never be
installed. If an app's Request URL still shows *unverified* in its settings
page, click **Verify** there.

Slack requires HTTPS for both URLs; for local development, put a tunnel
(ngrok, cloudflared) in front and set the webhook address to its URL.

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

The click-through equivalent of the automated setup, for when you cannot (or do not want to) use an app configuration token.

**Do this from Company › Integrations if you can.** Connect Slack there and every agent gets its own block carrying **the manifest its app is created from**: the same definition the engine creates apps from, with that agent's scopes, events and request URL already in it. Copy it, paste it into Slack, install, and paste the two values back. That is Steps 1 to 3 below in one paste, and it removes the failure this section's hand-built path invites, which is a single missing scope (see the [cache note](#bot-scopes-and-events)) turning into a bot that installs, reports success and sees an empty workspace.

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

Then paste each pair into that agent's block in Company › Integrations, or store each value under the name your YAML references with [`crewlet secrets set`](../reference/cli.md#crewlet-secrets-set) — a running node re-activates the revision that reads it as it stores it, as the command says after the write — or export them as `SLACK_BOT_TOKEN_*` / `SLACK_SIGNING_SECRET_*` in the engine's own environment before it starts. The engine reads no `.env` file ([Environment variables](../reference/environment-variables.md)).

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

