# Slack Integration

Crewlet uses **one Slack app per agent**: each agent gets its own bot identity, token, and webhook endpoint.

> **Prerequisites.** A Slack **workspace you administer**. The engine's API endpoint must be **reachable by Slack over public HTTPS**, for Events API webhooks and the OAuth install (locally, use a tunnel such as ngrok or cloudflared).

> **One of three chat backends; the axis is exclusive.** A Slack app (the org-level `integrations.slack` block **or any seat's own**, since credentials are per seat) puts the company on `chat.backend: vendor`. The engine's own chat ([Chatlet](../concepts/chatlet.md)) is what a company gets with no vendor chat surface; naming `chat.backend: chatlet` beside a Slack app is **refused at validation**, because two live chat homes mean a person replies in one while the agents read the other. Slack and [Mattermost](mattermost.md) together are fine: two workspaces with different people, as in a months-long migration.

Two ways to create the apps:

- **[Automated (recommended)](#per-agent-access-from-the-agents-tab)**: the engine creates and keeps every app through Slack's [App Manifest APIs](https://docs.slack.dev/app-manifests/configuring-apps-with-app-manifests/). Enter one app configuration token on the connect form, click **Allow** per agent on the Agents tab, and every secret Slack serves is sealed in the fleet's encrypted secret store under a name the engine derives for the seat.
- **[Manual](#manual-setup)**: click through [api.slack.com/apps](https://api.slack.com/apps) per agent and enter each app's two values on the connect form.

Both end the same way: each agent's own app, its credentials sealed and bound to its transport and Slack tool ([Configure in YAML](#configure-in-yaml)).

---

## Setting it up from the dashboard

Company › Integrations shows one section per agent. Paste the two values from the app's own Slack page; the engine seals them, gives the seat a `${VAR}` for each, and activates. Each seat's delivery address is shown beside its fields.

**Without an app-configuration token the engine does not create apps.** Slack's app-manifest API needs a token Slack issues only by hand, which your organisation may not permit. Where one is sealed, the engine creates and keeps every app ([Per-agent access from the Agents tab](#per-agent-access-from-the-agents-tab)). Where not, each agent's block in Company › Integrations carries the manifest its app is created from: copy it, paste it into Slack's **From an app manifest** flow, install, and paste the two values back. See [manual setup](#manual-setup) for the by-hand steps.

### Per-agent access from the Agents tab

Each agent is turned on or off on the integration's **Agents** tab, and the engine keeps its app in line on every pass. Slack has no roles: an app holds the bot scopes in [its manifest](#bot-scopes-and-events) (shown read-only) and reads and posts wherever it is invited. Nothing else is set per agent.

**The engine creates the apps** once an app-configuration token is sealed. Create one at [api.slack.com/apps](https://api.slack.com/apps) and paste its refresh token into **App configuration token** on the connect form. The engine seals it as `SLACK_APP_CONFIG_TOKEN`, points `integrations.slack.app_config_token` at it, and keeps the token pair in that one value:

- It rotates when under half an hour of life is left (a pass creating apps at Slack's rate of about one a minute needs that long).
- Each new pair is saved before use, because Slack's refresh token works once.
- A rotation and its save finish even if the pass is cancelled.
- A token Slack refuses as expired early is renewed once and the call retried.

The field is always one whole `${VAR}`, never the token. Without a token the engine does not manage Slack apps; set them up as described below.

The install stays a person's click: the tab shows a waiting step with **Install**, which mints a single-use link valid fifteen minutes and sends you to Slack's approval page. When Slack returns you to `/webhooks/slack-oauth`, the engine checks the link, confirms the approval belongs to that agent's app, and seals the bot token. A link used twice, expired or edited is refused, and the page says what to do.

| Action | What happens at Slack |
|---|---|
| Turn on, first time | The engine creates the app from its manifest and seals its client secret and signing secret (served once, so an app that cannot be sealed is deleted). A person approves the install. |
| Turn off | The engine revokes the bot token (`auth.revoke`). The app and its OAuth client are kept. |
| Turn back on | A person approves the install again; Slack issues a bot token only to an install. |
| Rotate | The engine revokes the bot token; a person approves a reinstall, which issues a new one. |
| Remove (only when off) | The engine deletes the app (`apps.manifest.delete`) and clears every value sealed for it once Slack confirms. Where Slack refuses the configuration token, the tab links to the app's page to delete it by hand. |

A scope or setting changed in Slack's console is restored from the engine's manifest on the next pass and reported. A bot token keeps the scopes it was installed with, so when a scope was added or removed at Slack the engine also revokes the bot token and a person approves a reinstall, issuing exactly the manifest's scopes. A bot token revoked or an app uninstalled by somebody at Slack is never reinstalled automatically (it may be incident response): the tab offers **Turn back on** or **Keep off**.

**The engine names every value.** A seat's values are sealed as `SLACK_BOT_TOKEN_<HANDLE>`, `SLACK_SIGNING_SECRET_<HANDLE>`, `SLACK_CLIENT_SECRET_<HANDLE>` and `SLACK_CLIENT_ID_<HANDLE>`. The transport and the seat's Slack tool server get the one bot token, so they cannot drift, and no document edit can point a seat's token at another variable.

### Disconnecting

Disconnect first turns every agent off (the engine revokes each bot token, so no agent posts once it starts), then removes `integrations.slack` from the company document, which retires the transport: the engine stops talking to Slack and the per-seat webhook routes stop turning deliveries into work.

**Tick *also remove the accounts Crewlet created*** and the engine deletes each agent's app at Slack with the app-configuration token, clears its sealed values (`SLACK_BOT_TOKEN_<HANDLE>`, `SLACK_SIGNING_SECRET_<HANDLE>`, `SLACK_CLIENT_SECRET_<HANDLE>` and `SLACK_CLIENT_ID_<HANDLE>`) once Slack confirms the app is gone, and removes each agent's `integrations.slack` block. Where an app cannot be deleted that way (no usable configuration token, or Slack refused it), the Agents tab links to its page, **Settings › Basic Information › Delete App**, and the disconnect stays in progress until every app is gone: dropping the integration first would drop the only token that confirms deletion, while an undeleted app could still be installed again.

Leave it unticked and every app and OAuth client is kept, so turning an agent back on after a reconnect is an install a person approves.

## Configure in YAML

`integrations.slack: {}` (org-level) is a marker that enables the Slack **transport**; its one setting is [`typing_status`](#working-status-is-thinking). The Slack **MCP tool** server is a separate `mcp_servers` entry (`shared: false`). Per agent, the identity has two consumers: the **transport** (bot token and signing secret) and the **Slack MCP subprocess**, which reads the same bot token as `SLACK_MCP_XOXB_TOKEN`. The engine binds the one sealed token to both, so neither is named in the document.

The transport writes and reads on that token: it raises the working indicator and, at the start of a turn woken in a thread, calls `conversations.replies` so the agent is handed the conversation (see [the thread block](../concepts/agent-runtime.md#the-thread-a-turn-was-woken-in)). That needs no new scope (the `*:history` scopes the manifest requests cover it) and reads as that app, so a channel the bot is not in answers `not_in_channel` and the turn runs without the block. `conversations.replies` pages from the *oldest* end, so a thread past ~1000 messages cannot be read to its newest end: the agent gets the newest of what was reached plus a note that the block stops short and the rest must be read with its chat tools, never a truncated thread presented as whole. A seat that is on needs only its `enabled`:

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

**The two `env` switches are what let an agent answer.** [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) registers write tools only when told to. Without `SLACK_MCP_ADD_MESSAGE_TOOL` it never offers `conversations_add_message`, its one posting tool, so a seat reasons about a reply with no tool to send it and the channel stays quiet while the server reports healthy.

- `"true"` lets the agent post in every channel and DM its bot can reach. A comma-separated list of channel ids, or of `!`-prefixed ids to exclude, narrows that, as the server's own [settings](https://github.com/korotovsky/slack-mcp-server/blob/v1.3.0/README.md#environment-variables-quick-reference) describe. Anything else is read as such a list, so `"false"` registers the tool and lets it post only in a channel whose id is `false`, which is none.
- A narrowed list mutes the seat wherever it is addressed outside it, and a seat is addressed wherever its bot is invited.
- `SLACK_MCP_REACTION_TOOL` is the same switch for `reactions_add` and `reactions_remove`, for which the [`reactions:write`](#bot-scopes-and-events) scope is requested.
- Neither is a credential, so both sit in the server entry's `env` (every seat's instance starts with it), not in each seat's `mcp_env`.
- They go in `env`, not an `--enabled-tools` argument: calls read the environment alone, so a tool enabled only on the command line is offered and refuses each post.
- The release is pinned because `npm exec` with `@latest` runs whatever was published last, and a release renaming a tool or changing what must be set would arrive unannounced.
- The bundled tool skills (mentions, channel, hand-off, directives) trigger on this release's tool names under the `slack_` prefix (`slack_conversations_add_message`, `slack_channels_me`, `slack_channels_list`, `slack_users_search`), teaching a seat Slack's `<@member-id>` mentions and thread replies before its first post. Under another prefix, or after a release renaming one of those tools, the skills go quiet silently.

**Each seat names a channel cache of its own.** slack-mcp-server keeps the channels its bot can see in a file named after the workspace alone (`<team id>_channels_cache_v2.json`, in the user's cache directory) and serves it for 24 hours without asking Slack again. Every seat's server on one node runs as the same user against the same workspace, so without `SLACK_MCP_CHANNELS_CACHE` they share one file, and whichever wrote it last decides what `channels_list` tells the rest: the private channels and DMs that bot is in, named to seats that are not, and none of the reader's own. Give each seat a path no other seat uses. The directory must exist for the list to outlive a restart; otherwise the server keeps the list in memory and refetches at every start, still that seat's own.

The bot token drives the transport (identity at start, working indicator) and, as `SLACK_MCP_XOXB_TOKEN`, the Slack MCP tools. The signing secret is transport-only: it verifies this seat's inbound deliveries. Both are sealed as `SLACK_BOT_TOKEN_<HANDLE>` and `SLACK_SIGNING_SECRET_<HANDLE>`. There is no per-seat channel setting: the transport posts no message, so has nothing to aim, and the room a seat talks in is [`units[].channel`](../concepts/organization-model.md), which the executor prompt renders as the team channel for whichever chat backend the company runs.

**An app created by hand** (no app configuration token) is an external credential: its block names `bot_token` and `signing_secret` as whole `${VAR}`s, which the connect form writes when you paste the two values. A `bot_token` is one whole `${VAR}` or a literal, never a reference inside other text (`xoxb-${REST}`), which the transport would send as written.

---

## Automated setup

The engine creates every agent's app once an app configuration token is on the company block, and a person approves each install from the Agents tab: see [Per-agent access from the Agents tab](#per-agent-access-from-the-agents-tab).

### Upgrading from `crewlet slack provision`

The retired `crewlet slack provision` command made each agent's app from your machine and kept what Slack returned in a local ledger, `slack-apps.json`. The engine never reads that file, so moving onto the engine's own setup takes three steps.

1. **Enter an app configuration token on Slack's connect form.** Generate a fresh one from [api.slack.com/apps](https://api.slack.com/apps) (Your App Configuration Tokens) rather than reusing the pair in `slack-apps.json`, which Slack may have rotated or expired. On its next sync the engine adopts each app the command made by the seat's working bot token, so no app is created twice.
2. **Store each app's OAuth client.** Slack serves an app's client id and secret only once, at creation, so the ledger holds the only copy, and an adopted app cannot be installed again (after turn-off, or for a rotate) until both are in the secret store. Pipe each value straight from the ledger so it never shows on screen or in shell history, once per agent handle:

   ```sh
   jq -r '.apps["senior-engineer"].client_id' slack-apps.json \
     | crewlet secrets set SLACK_CLIENT_ID_SENIOR_ENGINEER
   jq -r '.apps["senior-engineer"].client_secret' slack-apps.json \
     | crewlet secrets set SLACK_CLIENT_SECRET_SENIOR_ENGINEER
   ```

   The variable is the handle in upper case with each character other than a letter or digit written as `_`. Never pass these values with `-value`, which exposes them in the process list and shell history.
3. **Delete `slack-apps.json`** once every client is stored. It holds each app's client secret and signing secret in plain text.

### Ordering and URL verification

The API must be publicly reachable at the deployment's webhook address before an app is created: Slack verifies each app's events **Request URL** with a `url_verification` challenge, which the API answers unconditionally, needing no engine, company config or credentials. This is safe because the response is a pure echo of the caller's challenge, and necessary because the signing secret is sealed only after Slack creates the app, so a verified handshake is impossible. If an app's Request URL still shows *unverified* in its settings page, click **Verify** there.

Slack requires HTTPS for both URLs; for local development, put a tunnel (ngrok, cloudflared) in front and set the webhook address to its URL.
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

**Prefer Company › Integrations.** Connect Slack there and every agent gets a block with **the manifest its app is created from**: the same definition the engine uses, with that agent's scopes, events and request URL. Copy it, paste it into Slack, install, and paste the two values back. That is Steps 1 to 3 in one paste, and it avoids the hand-built path's failure: one missing scope (see the [cache note](#bot-scopes-and-events)) gives a bot that installs, reports success and sees an empty workspace.

The steps below do the same by hand.

### Step 1: Create a Slack App Per Agent

For each agent that will use Slack, create a dedicated Slack app:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App**
2. Choose **From an app manifest**, and paste that agent's manifest from Company › Integrations. Steps 2 and 3 are then done. Select your Slack workspace > **Next** > **Create and Install** > **Allow**; it ends on the page holding the app's credentials. Copy the **Bot token** from **Your app credentials** there, and the **Signing Secret** from **Basic Information** > **App Credentials**. That is all of Step 2 for a manifest app.
3. **From scratch** leaves scopes, events and request URL to set by hand (the rest of this section). Name it after the agent (e.g., "Crewlet Engineer", "Crewlet Designer"), select your workspace and click **Create App**.

### Step 2: Configure Each App's Tokens

**Only for an app built from scratch.** A manifest app already has its scopes, and Step 1 says where its tokens are.

For each app:

1. Go to **OAuth & Permissions** in the sidebar
2. Under **Bot Token Scopes**, add **every** scope from the [table above](#bot-scopes-and-events)
3. Click **Install to Workspace**, or **Reinstall to Workspace** if you added scopes to an existing app (new scopes only take effect on a freshly minted token)
4. Copy the **Bot User OAuth Token** (`xoxb-...`)
5. Go to **Basic Information** > **App Credentials** and copy the **Signing Secret**

If you cancel the install, or Slack doesn't approve it, the page Slack sends you to says so and tells you to install again from the app's settings in Slack.

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

Then paste each pair into that agent's block in Company › Integrations, or store each value under the name your YAML references with [`crewlet secrets set`](../reference/cli.md#crewlet-secrets-set) (a running node re-activates the revision that reads it as it stores it, as the command says after the write), or export them as `SLACK_BOT_TOKEN_*` / `SLACK_SIGNING_SECRET_*` in the engine's environment before it starts. The engine reads no `.env` file ([Environment variables](../reference/environment-variables.md)).

> **The `signing_secret` is what makes the endpoint usable.** `/webhooks/slack/{handle}` is
> exempt from the API's bearer token because it verifies Slack's own signature instead. Until
> the secret is set there is nothing to verify with, so the route answers `503` with
> `Retry-After` rather than accepting the delivery. Slack retries, and deliveries flow once
> the secret is configured; nothing is lost.
>
> An unsigned delivery is never recorded, published, or shown on the dashboard.

---

## Reaching humans on Slack

When an agent escalates to a [human seat](../concepts/humans-in-the-org.md), it DMs the human's `contact.slack_user_id` with **its own** bot token, the same credential it uses for every other Slack message. There is no org-level "system" Slack app and no extra bot token for human seats: the engine never sends as itself. An escalating agent with no Slack app of its own can't DM the human; that is a config gap to fix (give it a Slack app, or route the work through a colleague who has one).

---

## How Slack Routing Works

### Inbound

1. Human posts in a channel where the bot is present
2. Slack sends webhook to `https://your-server.com/webhooks/slack/{handle}`
3. The API verifies the `X-Slack-Signature` v0 HMAC **at the edge**, against that handle's own signing secret, before the payload is persisted, streamed to a dashboard or published. The timestamp is part of the signed string and is checked against a replay window, so a captured request stops working. This is the **only** verification (nothing downstream checks again), which is why the route is exempt from the API's bearer token.
   - No signing secret for **any** seat → `503`. A node with no secrets loaded must not accept an unsigned POST for any handle.
   - A populated secret map that does not name this handle → `401 invalid_signature`. It is answered exactly like a wrong signature on a real seat, after the same check, so an outsider cannot tell which handles have an app. The log names it `slack_webhook_unknown_handle`.
4. The delivery is claimed fleet-wide on Slack's `event_id`, stable across retries, so a redelivery, or a message that arrives twice because the app subscribes to both `message.*` and `app_mention`, wakes the seat once.
5. The API publishes to `crewlet.notifications.inbound` on the EventQueue.
6. The notification service resolves the handle to its seat and publishes to that seat's mailbox, `crewlet.agent.{seat-id}.inbox`.
7. The agent's handler fires.

#### Which events wake an agent

Only `message` / `app_mention` events with **new user-visible content** are delivered: regular messages, `thread_broadcast` replies, file shares (a share without a comment renders as `(shared file: …)`, so the body is never blank), and other bots' messages (legacy `bot_message` events take the sender from `username` / `bot_id`).

Slack reuses `type: "message"` for channel **bookkeeping**. Those events are skipped, logged at debug as `slack_event_skipped` with the reason, and not recorded as a `NotificationSkipped` event (they concern nobody, and a skip row each would bury the drops that matter):

- `message_changed`: edits, **including Slack's own link-unfurl edits** of a message an agent just posted
- `message_deleted`: deletions
- `message_replied`: thread-reply bookkeeping; Slack delivers it **without its subtype** (a documented Slack bug), so it is recognized by its `hidden: true` flag
- system subtypes (`channel_join`, `channel_leave`, `channel_topic`, …): lines *about* the channel

These envelopes have no top-level `user`/`text`; delivering them would cause phantom turns triaging an empty message.

#### The seat's own messages

A seat never wakes on its own post. The check is made **twice** because Slack echoes a bot in two shapes: an ordinary post has `user` equal to the bot user id; one made through an incoming webhook or with a custom username arrives as a `bot_message` with no `user`, only the app id. Missing either test makes the seat answer itself, one turn per reply, for ever.

An agent's own reply in a thread **subscribes it to that thread**: it hears what comes back without being named again.

### Outbound (via Slack MCP tools)

Everything an agent does deliberately (messaging, threading, reading history, reactions) is an **MCP tool** using that agent's own bot token via [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server), so a message comes from the agent, not a shared company bot. Posting and reacting exist only where the server entry sets [`SLACK_MCP_ADD_MESSAGE_TOOL` and `SLACK_MCP_REACTION_TOOL`](#configure-in-yaml). Message search is not offered: the server gives `conversations_search_messages` only to a user token, because Slack's `search.messages` refuses a bot's.

The engine's transport posts no messages. It holds the same per-seat token for two calls: `auth.test`, which resolves the seat's identity at start, and the [working indicator](#working-status-is-thinking).

---

## Working Status ("is thinking…")

While an agent reasons about a Slack message it shows a **working status** in the thread ("*Agent SWE is thinking…*" under the composer), and takes it down once nothing is working behind it. Turns can run minutes; without it the human cannot tell "working" from "dead".

### What Slack actually supports

Slack has **no public typing API for bots**: the classic `user_typing` signal lived on the RTM API, which granular-permission apps cannot use ([slackapi/bolt-js#885](https://github.com/slackapi/bolt-js/issues/885), duplicate [#2580](https://github.com/slackapi/bolt-js/issues/2580)).

The supported mechanism is [`assistant.threads.setStatus`](https://docs.slack.dev/reference/methods/assistant.threads.setStatus/), which renders a working-state line in the thread. It used to need `assistant:write` and an AI-assistant split-view app; since [March 2026](https://docs.slack.dev/changelog/2026/03/05/set-status-scope-update/) it accepts plain **`chat:write`**.

**Every Slack-enabled agent already holds `chat:write`** (Step 2 above), so this needs **no new scope, no manifest change, and no reinstall**.

### Behaviour

Each phase draws one line from its own pool:

| Turn phase | Drawn from |
|---|---|
| First-turn onboarding | *is getting crewleted in…* · *is settling in…* · *is finding the coffee machine…* · … |
| Execute | *is crewleting…* · *is thinking it through…* · *is cracking on…* · … |
| Review | *is re-crewleting…* · *is double-checking…* · *is marking its own homework…* · … |

These are a turn's phases: the executor decides *and* acts in one pass, so there is no planning line. The full pools are `PhasePhrases` in `internal/notify/phrases.go`; replace any with your own wording via [`status_phrases`](#custom-status-phrases).

Every line describes the **phase**, never a specific action. The pick is arbitrary, so a line naming work the agent merely *could* be doing ("is consulting the org chart…") would be wrong most of the time and teach readers to distrust the indicator. Generic ("is thinking…") or plainly figurative ("is finding the coffee machine…") is safe.

**When it goes up, is held and comes down is the turn's, not this page's.** The lifecycle, including what an executor in its [box](../concepts/code-sandbox.md) does to it, is the table in [Turn Engine § The working status](../concepts/turn-engine.md#the-working-status), the only copy. It is identical on Mattermost; the backends differ only in what the indicator can *say*. What follows is true of Slack only.

- **One line per phase, held for that phase.** The pick is deterministic in `(turn_id, phase, how many phases the turn has been through)`, so the 45 s heartbeat re-asserts the *same* words (churning text would read as a restart). The next phase draws the next line, and a `self_iterate` loop back into Execute draws a different line than the first pass, so a second pass is visible. Two turns in one thread start at different points in the pool, because the seed is the turn's id.
- **Kept alive across long turns.** Slack expires a status after 2 minutes; the engine re-asserts every 45 s (two attempts per expiry window, ~1.3 requests/min against Slack's 600/min per-app limit).
- **Slack clears it the instant the agent posts into the thread.** The engine re-asserts only while a later phase is still running, which keeps the indicator honest across a `self_iterate` loop.
- **A refused or rate-limited `setStatus` is logged and the status simply expires.** The indicator never fails or delays a turn.
- **Two minutes is what stands when this process cannot clear it.** A process killed outright leaves its last indicator to that expiry. A `PUT /config` that rebuilds the Slack transport clears the ones it held; turns in flight then run without an indicator, and their replies land as usual.

### `typing_status` modes

```yaml
integrations:
  slack:
    typing_status: always    # default
```

| Mode | Shows the status when… |
|---|---|
| `always` *(default)* | every Slack-triggered turn, including passive top-level channel messages and `@here` / `@channel` broadcasts |
| `addressed` | a human is plausibly waiting on **this** agent: a DM or group DM, a direct `@mention` (including `app_mention`), or a thread the agent is in because it was named there (not one it merely spoke in, or one a broadcast started) |

> **There is no `off`.** It would leave agents thinking in silence for minutes, the state this feature removes. A workspace where several agents light up for one message wants `addressed`.

`addressed` excludes passive channel traffic, collective addresses, and replies in a thread the agent only spoke in or a broadcast started. Every bot in a channel is woken by a top-level message and the [triage prompt](../concepts/turn-engine.md) tells most to stay silent, so `always` in a channel with five agents lights up five indicators for a message none will answer. Use `always` in a single-agent workspace, or where agents are expected to weigh in on everything.

The setting is org-wide and live-editable: a `PUT /config` that changes it rebuilds the Slack transport in place, no restart.

### Custom status phrases

Override any phase under `status_phrases`; phases you leave out keep their built-in pool.

```yaml
integrations:
  slack:
    typing_status: addressed
    status_phrases:
      onboarding: ["is getting nimbused in...", "is settling in..."]
      execute:    ["is nimbusing...", "is on the case...", "is cracking on..."]
      review:     ["is re-nimbusing...", "is double-checking..."]
```

- Slack renders the line **after the agent's name**, so each phrase must finish that sentence: `"is nimbusing..."` shows as *Agent SWE is nimbusing…*.
- **Describe the phase, not the work.** A phrase is picked arbitrarily; one specific enough to sound like a report ("is checking Jira…") is usually false and costs the reader's trust in every later line.
- One phrase makes a fixed label; more add variety across turns. The engine holds one line for the whole phase either way.
- An **empty list** (or an omitted phase) keeps the built-in pool, and so does a list with nothing usable left: a blank string is dropped, because an empty status *clears* the indicator.
- `default` covers any future phase with no pool of its own. You rarely need it.

Like `typing_status`, this is live-editable: a `PUT /config` swaps the wording without a restart. The apply rebuilds the Slack transport, clearing held indicators: a turn in flight finishes without one, and the next turn in that thread uses the new wording.

---

## What the atlas reads

The engine asks Slack which workspace the agents' apps are installed in, for the company's [atlas](../concepts/atlas.md#what-integrations-read), once an hour unless `atlas.refresh_interval_seconds` says otherwise. It adds the workspace's id, name and address to the Slack account the atlas already shows. Nothing at Slack is changed.

| Part | Calls | Scope | What the atlas gains |
|---|---|---|---|
| Workspace | `auth.test`, once | none: every bot token may call it | the workspace's id, its name and its address |

**Which token.** The bot token of the first agent, by handle, whose Slack app is switched on. If Slack says that token no longer works (app uninstalled), the next agent's is asked. An agent whose app is switched off is never asked, and a company with no Slack app is not read.

**Nobody is read.** `auth.test` also says which bot user asked; the engine ignores it. No member, channel or message is ever listed.

**A refusal is a finding, never a failed read.** Where Slack refuses every agent's bot token, the source's standing on the atlas says so, and what an earlier read recorded stays. On Slack's rate limit the call waits as long as Slack asks, up to three times and thirty seconds a wait; past that, or on any other failure, the read stops and records nothing, and the engine asks again five minutes later.

## Thread Routing

By default, agents only receive thread replies in threads they are **following**. Top-level channel messages are always delivered.

**Follow triggers:**

1. **Direct mention** — `<@BOT_USER_ID>` or `app_mention` event
2. **Collective address** — `<!channel>` or `<!here>`
3. **Participation** — the agent replies in the thread. The reply goes out through the Slack MCP tools, not the engine, so the follow is recorded from Slack's **echo** of it: the parser writes it as it suppresses the seat's own post, which also lets a node record a follow for a seat it is not running.

A **top-level** message that mentions the agent, or the room, starts the thread its own `ts` becomes, so the agent hears the answers without being named again, and follows that thread. The follow's reason decides whether a reply [obliges an answer](../concepts/turn-engine.md#who-is-waiting-and-how-they-get-an-answer) and raises the [`addressed` indicator](#working-status-is-thinking): a thread the agent was named in does; one a broadcast started, or one it merely spoke in, only lets it hear. A follow keeps the **strongest** reason ever recorded (mention, then collective address, then participation), so an `@here` in a thread the agent was named in reaches it as a broadcast and still obliges an answer, and the agent's own posts refresh a follow without weakening it. This holds when two messages in one thread are parsed on two nodes at once: each write is decided against the record it replaces.

Thread tracking state is persisted in the fleet's [coordination store](../concepts/coordination.md), keyed by backend, so it survives restarts *and* is visible to whichever node claims the next reply (an inbound message is parsed by one node, and a follow only that node could see would make a non-mention reply reach its seat by chance). Bot messages are ignored to prevent loops.

A follow is dropped after **90 days without activity**, and nothing else ends one; you cannot make an agent stop following sooner. Nothing sweeps it: the horizon is the coordination bucket's age, and every re-assert (a mention, a collective address, the agent posting) rewrites the record, while a reply that only rides the follow does not, so the age is a true last-activity stamp and the broker expires what has gone quiet. The number follows from an asymmetry: a dropped stale follow costs at most one missed non-mention reply (the next mention re-follows), while keeping every follow forever grows a record read on the hot path of every inbound message.
