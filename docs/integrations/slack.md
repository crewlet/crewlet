# Slack Integration

Crewlet uses a **one Slack app per agent** model — each agent gets its own bot identity, token, and webhook endpoint in Slack.

> **Prerequisites.** You need a Slack **workspace you administer** (create one if you don't have it). The engine's API endpoint must be **reachable by Slack over public HTTPS** so the Events API can deliver webhooks, and so the OAuth install can land (for local development, use a tunnel such as ngrok or cloudflared).

There are two ways to create those apps:

- **[Automated (recommended)](#automated-setup-crewlet-slack-provision)** — `crewlet slack provision` creates and maintains every app through Slack's [App Manifest APIs](https://docs.slack.dev/app-manifests/configuring-apps-with-app-manifests/). One config token to bootstrap, one authorize click per agent, and every secret lands in `.env` automatically.
- **[Manual](#manual-setup)** — click through [api.slack.com/apps](https://api.slack.com/apps) per agent.

Both end in the same place: per-agent credentials referenced from the company YAML ([Configure in YAML](#configure-in-yaml)).

---

## Configure in YAML

`integrations.slack: {}` (org-level) is a marker that enables the outbound Slack **transport**; its one setting is [`typing_status`](#working-status-is-thinking). The Slack **MCP tool** server is a separate `mcp_servers` entry (`shared: false`). Per agent, the Slack identity has two consumers: the **transport** reads `role.integrations.slack` (`bot_token`, `signing_secret`, optional `channel`), and the **Slack MCP subprocess** reads `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN`. Name the same `${VAR}` in both — one credential, two readers, no secret duplicated:

```yaml
integrations:
  slack:                                 # enable the Slack transport
    typing_status: addressed             # addressed (default) | always | off

mcp_servers:
  - name: slack
    shared: false                        # per-agent identity (token from role.mcp_env.slack)
    command: npm
    args: ["exec", "--yes", "--", "slack-mcp-server@latest", "--transport", "stdio"]
    tool_prefix: "slack_"

units:
  - name: Core
    type: team
    lead: Engineer
    roles:
      - name: Engineer
        integrations:                                     # per-agent transport identity
          slack:
            bot_token: "${SLACK_BOT_TOKEN_ENGINEER}"
            signing_secret: "${SLACK_SIGNING_SECRET_ENGINEER}"
            channel: C0123456789                          # optional default channel
        mcp_env:
          slack:
            SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_ENGINEER}"   # same token, the Slack MCP
      - name: Designer
        integrations:
          slack:
            bot_token: "${SLACK_BOT_TOKEN_DESIGNER}"
            signing_secret: "${SLACK_SIGNING_SECRET_DESIGNER}"
        mcp_env:
          slack:
            SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_DESIGNER}"
```

The `bot_token` drives the transport (inbound webhook verification + the outbound `send()` fallback); the same value, named as `SLACK_MCP_XOXB_TOKEN`, drives the Slack MCP tools — two independent subsystems, one credential referenced in two places. `signing_secret` and `channel` are transport-only and belong on `role.integrations.slack`.

Write this block **first**, with `${VAR}` placeholders — the automated provisioning below reads the placeholder names out of the YAML and fills exactly those variables in `.env`. Whole-value placeholders are required (`"${SLACK_BOT_TOKEN_ENGINEER}"`, not a literal token) so the provisioner knows which env vars to write. When both credentials are set they must be the **same kind** — both placeholders (provisionable) or both literals (a manually managed app, reported and left untouched); a mixed pair is rejected at config validation, since it would always leave one credential dead. A `bot_token`-only identity (outbound-only, no webhooks) is legal but not provisionable; a `signing_secret`-only identity is rejected (the app is never registered without a token).

---

## Automated Setup: `crewlet slack provision`

```
crewlet slack provision company.yaml -secret-store -public-url https://your-server.com
```

For every Slack-enabled agent seat (a role whose `integrations.slack` credentials are `${VAR}` references) the command:

1. Builds the canonical app manifest — app name from `role.name`, bot display name from the handle, the [bot scopes](#bot-scopes-and-events) both credential consumers need, event subscriptions pointed at `<public-url>/webhooks/slack/{handle}`, and the OAuth redirect at `<public-url>/webhooks/slack-oauth`.
2. Calls [`apps.manifest.create`](https://docs.slack.dev/reference/methods/apps.manifest.create/) on the first run, or `apps.manifest.update` on later ones, keyed by the local ledger. **A ledgered app whose manifest fingerprint has not changed is skipped entirely** — the manifest endpoints are Tier 1 rate limited to roughly one request a minute, so a no-op re-run over seven seats would otherwise spend minutes achieving nothing.
3. Records the app in the ledger (`slack-apps.json` beside the company YAML) and writes the returned **signing secret** into whichever `${VAR}` the YAML's `signing_secret` points at, through the sink you chose.
4. Walks you through the one step Slack has no API for: **an authorize click per app.** It prints the install URL; after you click **Allow** the browser lands on the API's `/webhooks/slack-oauth` page showing a short-lived code — paste it back and the command exchanges it (`oauth.v2.access`) for the **bot token**, recorded into the `bot_token` variable.

**One seat failing does not cost the others.** A mistyped code paste or a refused manifest is recorded against that handle, the remaining seats still provision, and the command exits non-zero naming what failed. Everything completed is durable — the ledger is written after every mutation — so re-running resumes rather than starting over.

Afterwards: invite each bot to its channels (`/invite @handle`) and restart `crewlet run` so the engine reads the new credentials.

> **A bot token carries only the scopes it was minted with.** Pushing a new manifest does not give an existing token a new scope — the app has to be installed again, which is what `-reinstall` is for. It is destructive on its own: the new install revokes the token every running node is currently authenticating with, so plan the restart that follows it.

### Where the secrets go

The same three-way choice every provisioning command in Crewlet takes, and there is **no default** — a run with nowhere to put what it mints would create live credentials and print none of them:

- `-secret-store` — the node's [encrypted secret store](../concepts/secret-store.md). The running engine rebuilds its resolver on apply, so re-activate the current revision afterwards ([`crewlet config activate`](../reference/cli.md#crewlet-config-activate)).
- `-env-file PATH` — a `.env` file, which then has to be sourced and the engine restarted.
- `-print` — stdout, for moving the values into a password manager or a deployment system this binary knows nothing about.

### One-time bootstrap: the app configuration token

The manifest APIs authenticate with an **app configuration token**, which Slack only issues manually: go to [api.slack.com/apps](https://api.slack.com/apps) → **Your App Configuration Tokens** → **Generate Token**, then pass its *refresh* token:

```bash
export SLACK_CONFIG_REFRESH_TOKEN="xoxe-1-..."     # or pass -config-token
```

The run exchanges it for a 12-hour access token and **records both in the ledger before using either**. That ordering is not tidiness: Slack's rotation is single-use in both directions — the call that returns a new refresh token invalidates the one it was given — so a run that rotated and failed to persist the result would lock you out of your own apps. For the same reason a still-valid access token is reused rather than rotated again, and the shell export above only ever bootstraps the first run.

### Ordering and URL verification

Run the API server **before** provisioning, publicly reachable at `-public-url`:

```bash
crewlet run -config config.yaml -roles ingress -api-host 0.0.0.0 -api-port 8080   # unconfigured is fine
```

Slack verifies each app's events **Request URL** with a `url_verification` challenge, and the API answers it unconditionally — no engine, company config or credentials needed. That exemption is deliberate and safe: the response is a pure echo of the caller's own challenge, and it has to work because during provisioning the signing secret does not exist yet, so a verified handshake would be impossible and the app could never be installed. If an app's Request URL still shows *unverified* in its settings page, click **Verify** there.

Slack requires HTTPS for both URLs — for local development put a tunnel (ngrok, cloudflared) in front and pass its URL.

### Flags

| Flag | Description |
|------|-------------|
| `-public-url URL` | Public HTTPS base URL of the Crewlet API. **Required**: every app's request URL and redirect URL are built from it, so an app created without one delivers nowhere and cannot be installed. |
| `-secret-store` / `-env-file PATH` / `-print` | Where the minted bot token and signing secret go — exactly one, no default. |
| `-config-token TOKEN` | The app-configuration **refresh** token; empty reads `$SLACK_CONFIG_REFRESH_TOKEN`. |
| `-ledger PATH` | The app ledger (default: `slack-apps.json` beside the company YAML). |
| `-handles a,b` | Only provision these handles. Worth having against a method that allows about one request a minute: fixing one seat in a company of twenty should not cost twenty minutes. |
| `-reinstall` | Redo the OAuth install even where a bot token is already recorded. Required for a scope change to take effect, and destructive — it revokes the seat's current token. |
| `-no-install` | Create and update the apps and write the signing secrets, then print the authorize URLs instead of asking for codes. For a non-interactive run, e.g. pushing a scope change from CI. |
| `-dry-run` | Print the plan and touch nothing. No app is created, no manifest pushed, no token rotated — and the sink is not opened, so it prompts for no passphrase. |

### The ledger (`slack-apps.json`)

Maps each handle to its Slack `app_id`, the fingerprint of the last-pushed manifest (what makes unchanged re-runs free), and the credentials **Slack only returns once, at creation**: the OAuth client id and secret (needed to redo an install later) and the signing secret. It also holds the rotating app-configuration token pair.

It is a **secrets file** — written `0600` through a temp file and a rename, because a truncate-then-write interrupted half way would destroy values that cannot be read back. It is gitignored by name in the repo's own `.gitignore`; if you keep your company document elsewhere, gitignore it there too. Committing it publishes credentials nothing can rotate for you.

Why a file at all, when no other vendor needs one: two of those four values have no field in the company config (nothing in the running engine reads a client id), and Slack has no method that reads them back. Deleting the ledger makes the next run create duplicate apps, since Slack has no API to list the ones you already have.

### Bot scopes and events

The single source of truth is `internal/slack` (`BotScopes` / `BotEvents`); the manual steps below list the same values. The scopes cover **both** consumers of the bot token — the notification transport and every tool enabled on the Slack MCP server:

| Scope | Used by |
|-------|---------|
| `app_mentions:read` | `app_mention` events (thread-follow trigger) |
| `channels:history`, `channels:read` | public channels — thread routing + MCP `conversations_history` / `conversations_replies` / `channels_list` |
| `chat:write` | transport `send()` + MCP `conversations_add_message` |
| `files:read` | shared-file notifications |
| `groups:history`, `groups:read` | private channels — **required**, see the note below |
| `im:history`, `im:read`, `im:write` | DMs, incl. escalation DMs to human seats |
| `mpim:history`, `mpim:read` | group DMs — **required**, see the note below |
| `reactions:write` | MCP `reactions_add` / `reactions_remove` |
| `search:read.public` | the bot-token search scope (the plain `search:read` is user-token-only) |
| `usergroups:read`, `usergroups:write` | MCP `usergroups_*` tools |
| `users:read` | MCP `users_search`, sender attribution |

> **`groups:read` and `mpim:read` are required, not optional.** On startup
> [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) refreshes its
> channel cache with a single `conversations.list` call that covers **all four**
> conversation types — `public_channel`, `private_channel`, `mpim`, `im` — and the
> set is hard-coded (there is no env var to narrow it). If the bot token is missing
> `groups:read` (private channels) or `mpim:read` (group DMs), Slack rejects the
> whole call with `missing_scope`, and the server logs `Failed to fetch channels` →
> `API returned zero channels, keeping existing cache`. The result is that the bot
> sees **no channels at all** (not just missing private ones), so `slack_channels_list`
> comes back empty even though everything else — including the user cache (`users:read`)
> — works. The fix is always: grant the missing scope **and reinstall the app** to
> mint a new token.
>
> `search:read.public` (not the user-token `search:read`) is the bot-token search
> scope; even so, `slack-mcp-server`'s `conversations_search_messages` tool is
> unavailable with a bot token because bots cannot call `search.messages`. Bots also
> only see channels they have been **invited** to — granting the read scopes lets the
> bot *list and read* those channels, but membership is still required.

Bot events: `app_mention`, `message.channels`, `message.groups`, `message.im`, `message.mpim` — one `message.*` event per conversation type the transport handles, so a bot invited to a private channel or group DM wakes on non-mention messages and thread replies exactly like in public channels. (The transport dedups the message/app_mention double delivery by `handle:channel:ts`.)

---

## Manual Setup

The click-through equivalent of the provisioner — useful when you can't (or don't want to) use configuration tokens.

### Step 1: Create a Slack App Per Agent

For each agent that will use Slack, create a dedicated Slack app:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App**
2. Choose **From scratch**
3. Name it after the agent (e.g., "Crewlet Engineer", "Crewlet Designer")
4. Select your workspace and click **Create App**

### Step 2: Configure Each App's Tokens

For each app:

1. Go to **OAuth & Permissions** in the sidebar
2. Under **Bot Token Scopes**, add **every** scope from the [table above](#bot-scopes-and-events)
3. Click **Install to Workspace** — or **Reinstall to Workspace** if you added scopes to an existing app (new scopes only take effect on a freshly minted token, so adding a scope without reinstalling changes nothing)
4. Copy the **Bot User OAuth Token** (`xoxb-...`)
5. Go to **Basic Information** > **App Credentials** and copy the **Signing Secret**

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

Then export each `SLACK_BOT_TOKEN_*` / `SLACK_SIGNING_SECRET_*` pair (or put them in `.env`) under the names your YAML references.

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
   - A secret map that is populated but does not name this handle → `401`. That is a delivery for a seat with no Slack app, not a node that cannot check.
4. The delivery is claimed fleet-wide on Slack's own `event_id`, which is stable across its retries — so a redelivery, or a message that arrives twice because the app subscribes to both `message.*` and `app_mention`, wakes the seat once.
5. The API publishes to `crewlet.notifications.inbound` on the EventQueue.
6. The notification service resolves the handle to its seat and publishes to `crewlet.agent.{handle}.inbox`.
7. The agent's handler fires.

#### Which events wake an agent

Only `message` / `app_mention` events that carry **new user-visible content** are delivered: regular messages, `thread_broadcast` replies, file shares (a share without a comment renders as `(shared file: …)` so the body is never blank), and other bots' messages (legacy `bot_message` events resolve the sender from `username` / `bot_id`).

Slack reuses `type: "message"` for channel **bookkeeping**, and those events are skipped with a recorded skip reason (`NotificationSkipped` event) instead of waking the agent with an empty notification:

- `message_changed` — edits, **including Slack's own link-unfurl edits** of a message an agent just posted
- `message_deleted` — deletions
- `message_replied` — thread-reply bookkeeping; Slack delivers it **without its subtype** (a documented Slack bug), so it is recognized by its `hidden: true` flag
- system subtypes (`channel_join`, `channel_leave`, `channel_topic`, …) — lines *about* the channel, not messages *to* anyone

These envelopes have no top-level `user`/`text`; delivering them would produce phantom agent turns triaging an empty message.

#### The seat's own messages

A seat never wakes on its own post, and the check is made **twice** because Slack echoes a bot in two shapes: an ordinary post carries `user` equal to the bot user id, while one made through an incoming webhook or with a custom username arrives as a `bot_message` with no `user` at all and only the app id to identify it. Missing either test makes the seat answer itself — one turn per reply, for ever.

An agent's own reply in a thread **subscribes it to that thread**, exactly as replying does in any chat client: it hears what comes back without having to be named again.

### Outbound (via Slack MCP tools)

All Slack capabilities an agent uses deliberately — messaging, threading, search, reactions — are **MCP tools** powered by that agent's own bot token via [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server), so a message comes from the agent rather than from a shared company bot.

The engine's own transport posts only where the engine itself is speaking, and it uses the same per-seat token. Its one visible use is the [working indicator](#working-status-is-thinking).

---

## Working Status ("is thinking…")

An agent turn takes time — a Plan → Execute → Review pass with tool calls
routinely runs minutes. Without a signal, the human who posted sees
nothing until the reply lands and cannot tell "the bot is working" from
"the bot is dead". Crewlet closes that gap: while an agent reasons about a
Slack message it shows a **working status** in the thread — "*Agent SWE is
thinking…*" under the composer — and clears it when the agent replies or
gives up.

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
| Plan | *is crewleting…* · *is thinking…* · *is mulling it over…* · … |
| Execute | *is crewing…* · *is cracking on…* · *is executing the cunning plan…* · … |
| Review | *is re-crewleting…* · *is double-checking…* · *is marking its own homework…* · … |

The full pools are `PHASE_PHRASES` in
`internal/notify/status.go`; replace any of them with
your own wording via [`status_phrases`](#custom-status-phrases) below.

Every line describes the **phase**, never a specific action. The pick is
arbitrary — nothing consults what the agent is doing — so a line naming
work it merely *could* be doing ("is consulting the org chart…", "is
reading the handbook…") would read as a status report and be wrong most
of the time it appears, which teaches the reader to distrust the whole
indicator. Generic ("is thinking…") or plainly figurative ("is finding
the coffee machine…") is safe; plausible-and-specific is not.

- **One line per phase, held for that phase.** The pick is deterministic
  in `(handle, channel, thread_ts, turn_id, phase)`, so the 45 s heartbeat
  re-asserts the *same* words — text that churned mid-phase would read as
  the agent restarting. Moving to the next phase draws the next line, and
  a `self_iterate` loop back through Plan draws a different one than the
  turn opened with, so a second pass is visible instead of looking stuck.
- **Raised before the work starts** — including while the turn is queued
  behind a busy agent, so a ping to a working agent is acknowledged
  immediately.
- **Kept alive across long turns.** Slack expires a status after 2
  minutes; the engine re-asserts it every 45 s (two attempts inside every
  expiry window, ~1.3 requests/min against Slack's 600/min per-app limit).
- **Cleared when the turn ends** — a posted reply, a planner `skip`
  decision ("not addressed to me"), a failure, or an exhausted budget all
  clear it. Slack also clears it by itself the instant the agent posts
  into the thread; the engine re-asserts only while a later phase is still
  running, which is what keeps the indicator honest across a
  `self_iterate` loop.
- **Held across a detached [sandbox](../concepts/code-sandbox.md)
  run.** When Execute suspends for a background coding job the agent has
  neither replied nor given up, so the indicator stays up until the
  resumed turn finishes.
- **Self-healing.** If a turn dies without clearing, a liveness probe
  drops the indicator within one refresh once the agent stops being busy;
  an absolute one-hour cap bounds the pathological case. Every Slack call
  is best-effort — a failed or rate-limited `setStatus` is logged and the
  status simply expires.

### `typing_status` modes

```yaml
integrations:
  slack:
    typing_status: addressed    # default
```

| Mode | Shows the status when… |
|---|---|
| `addressed` *(default)* | a human is plausibly waiting on **this** agent: a DM or group DM, a direct `@mention` (including `app_mention`), or a thread the agent already follows |
| `always` | every Slack-triggered turn, including passive top-level channel messages and `@here` / `@channel` broadcasts |
| `off` | never |

`addressed` deliberately excludes passive channel traffic and collective
addresses. Every bot in a channel is woken by a top-level message, and the
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
      plan:       ["is nimbusing...", "is thinking very hard...", "is scheming..."]
      execute:    ["is nimbusing it...", "is on the case...", "is cracking on..."]
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
- An **empty list** (or an omitted phase) keeps the built-in pool. A blank
  string is rejected at config load: an empty status doesn't render, it
  *clears* the indicator.
- `default` covers any future phase with no pool of its own. You rarely
  need it.

Like `typing_status`, this is live-editable — a `PUT /config` swaps the
wording without a restart. Turns already in flight keep the line they
opened with.

---

## Thread Routing

By default, agents only receive thread replies in threads they are **following**. Top-level channel messages are always delivered.

**Follow triggers:**

1. **Direct mention** — `<@BOT_USER_ID>` or `app_mention` event
2. **Collective address** — `<!channel>` or `<!here>`
3. **Outbound participation** — the agent sends a reply in the thread
4. **Outbound send** — the agent posts its reply into the thread

Thread tracking state is persisted in the store (``chat_thread_follows`` table, rows keyed ``backend = 'slack'``) so it survives engine restarts. Bot messages are automatically ignored to prevent loops.

A follow is dropped after **90 days without activity** — the row's timestamp is refreshed every time the follow is re-asserted (a mention, a collective address, the agent posting), so it means last activity rather than when the thread started. The asymmetry is what sets the number: a dropped stale follow costs at most one missed non-mention reply, and the next mention re-follows through the ordinary path above, while keeping every follow forever grows a table that is read on the hot path of every inbound message. The sweep runs on the maintenance worker, once per fleet.

