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
crewlet slack provision company.yaml --base-url https://your-server.com
```

For every Slack-enabled agent seat (a role with `integrations.slack` `${VAR}` placeholders) the command:

1. Builds the canonical app manifest — app name from `role.name`, bot display name from the handle, the [bot scopes](#bot-scopes-and-events) both credential consumers need, event subscriptions pointed at `{base-url}/webhooks/slack/{handle}`, and the OAuth redirect at `{base-url}/webhooks/slack-oauth`.
2. Calls [`apps.manifest.create`](https://docs.slack.dev/reference/methods/apps.manifest.create/) (first run) or `apps.manifest.update` (subsequent runs, keyed by the local ledger). A ledgered app whose manifest is byte-identical to the last push is skipped entirely (`unchanged` in the summary) — the manifest endpoints are Tier 1 rate-limited, so scope or URL changes are pushed by simply re-running, and no-op re-runs cost nothing.
3. Records the app in the ledger (`slack-apps.json` next to the company YAML) and writes the returned **signing secret** into the env file under the exact `${VAR}` name the YAML references.
4. Walks you through the one step Slack has no API for: an **authorize click** per app. It prints the install URL; after you click **Allow**, the browser lands on the Crewlet API's `/webhooks/slack-oauth` page showing a short-lived code — paste it back into the prompt and the CLI exchanges it (`oauth.v2.access`) for the **bot token**, written to the env file. A freshly created app always installs, even if a (necessarily stale) token is already present under its env var.

One agent failing — a mistyped code paste, an invalid manifest — is recorded as a `FAILED` line and the remaining agents still provision; the command then exits non-zero. Re-running resumes exactly the failed agents (everything completed is persisted).

Because a bot token only carries the scopes it was minted with, the provisioner pairs naturally with `--reinstall`: after a scope change lands via `apps.manifest.update`, run with `--reinstall` to mint fresh tokens that actually carry the new scopes.

Afterwards: invite each bot to its team channels (`/invite @handle`) and (re)start `crewlet run` so the engine reads the new credentials.

### The env file: one path, file wins

Everything the provisioner reads and writes goes through **one** env file — `--env-file`, defaulting to the `.env` next to the company YAML (the same file `crewlet run` loads). Two rules worth knowing:

- **Read and write are the same file.** Secrets persisted by one run are always visible to the next, wherever you point `--env-file`.
- **For the keys provisioning manages, the file beats the shell.** A value present in the file wins over a same-named environment variable. This is deliberate (and the opposite of `crewlet run`'s env precedence): the file is the provisioner's durable store, and a stale `export SLACK_CONFIG_REFRESH_TOKEN=…` left in your shell must never shadow the freshly rotated pair — with Slack's rotation semantics that shadowing would brick provisioning once the old access token expires. Shell exports are only the first-run bootstrap input.

### One-time bootstrap: the app configuration token

The manifest APIs authenticate with an **app configuration token**, which Slack only issues manually: go to [api.slack.com/apps](https://api.slack.com/apps) → **Your App Configuration Tokens** → **Generate Token** (any member of the workspace can), then export both values (or put them straight in the env file):

```bash
export SLACK_CONFIG_TOKEN="xoxe.xoxp-..."          # access token, 12 h lifetime
export SLACK_CONFIG_REFRESH_TOKEN="xoxe-1-..."     # refresh token
```

That's the last manual token handling. Rotation is expiry-aware: the persisted access token is reused while fresh (its expiry is recorded as `SLACK_CONFIG_TOKEN_EXPIRES_AT`), and when it nears expiry the pair is rotated via `tooling.tokens.rotate` and persisted back to the env file — which from then on is authoritative, so the shell exports above can go stale harmlessly.

### Ordering and URL verification

Run the API server **before** provisioning, publicly reachable at `--base-url`:

```bash
crewlet run config.yaml --roles ingress --api-host 0.0.0.0 --api-port 8080   # unconfigured is fine
```

Slack verifies each app's events **Request URL** with a `url_verification` challenge; the API answers it unconditionally — no engine, company config, or credentials needed. If an app's Request URL still shows *unverified* in its **App Manifest** settings page (Slack does not always re-verify on manifest updates), click **Verify** there — the endpoint answers automatically. The same server renders the OAuth landing page; if it isn't up yet during the install step, copy the `code=` value straight from the browser's address bar instead (the prompt accepts the full URL).

Slack requires HTTPS for both URLs — for local development put a tunnel (e.g. ngrok) in front and pass its URL as `--base-url`.

### Flags

| Flag | Description |
|------|-------------|
| `--base-url URL` | Public HTTPS base URL of the Crewlet API (required). |
| `--env-file PATH` | The env file secrets are read from **and** written to (default: `.env` next to the company YAML — the same file `crewlet run` loads). |
| `--state-file PATH` | The provisioning ledger (default: `slack-apps.json` next to the company YAML). |
| `--handles a,b` | Only provision these agent handles. |
| `--dry-run` | Validate every manifest via `apps.manifest.validate` and print the plan; no app is created or updated. (It may refresh the stored config-token pair if it is missing or near expiry — validation needs a live token.) |
| `--skip-install` | Create/update apps + write signing secrets, but skip the interactive OAuth step — for non-interactive runs, e.g. pushing a scope change to every app from CI. |
| `--reinstall` | Redo the OAuth install even where a bot token is already set — required for scope changes to take effect (a token only carries the scopes it was minted with), and useful after a revoke. |

### The ledger (`slack-apps.json`)

Maps each handle to its Slack `app_id`, the fingerprint of the last-pushed manifest (what makes unchanged re-runs free), plus the credentials Slack only returns once at creation: the OAuth client id/secret (needed to redo an install later) and the signing secret (so a lost `.env` heals on the next run). It is a secrets file — written `0600`, atomically; **gitignore it like `.env`** (the default repo `.gitignore` covers it). Deleting it makes the next run create duplicate apps, since Slack has no API to list existing ones.

Re-runs are idempotent and resumable: state is saved after every mutation, so an interrupted run continues where it stopped. If an app was deleted in the Slack console, the provisioner detects the dangling ledger entry, creates a replacement, and forces a fresh OAuth install (the old env token belongs to the dead app).

**Adopting a manually created app** (avoiding a duplicate create) is possible by hand-seeding a ledger entry — but it needs more than the `app_id`: copy `client_id`, `client_secret`, and `signing_secret` from the app's **Basic Information** page too, or the OAuth install step will fail with an actionable error naming the missing fields.

### Bot scopes and events

The single source of truth is `crewlet.slack.manifest` (`BOT_SCOPES` / `BOT_EVENTS`); the manual steps below list the same values. The scopes cover **both** consumers of the bot token — the notification transport and every tool enabled on the Slack MCP server:

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
3. The API verifies the `x-slack-signature` HMAC **at the edge** against
   that handle's signing secret and returns `401` on failure — before the
   payload is persisted, streamed to dashboards, or enqueued. The
   transport verifies again before acting on it; the edge check is what
   keeps an unauthenticated request out of the event log and off the
   dashboard. (If the API process has no signing secrets loaded at all —
   a company with no Slack seats — it defers to the transport rather than
   rejecting.)
4. API publishes to `crewlet.notifications.inbound` on the EventQueue
5. NotificationService resolves handle → agent
6. Publishes to `crewlet.agent.{handle}.inbox`
7. Agent handler fires

#### Which events wake an agent

Only `message` / `app_mention` events that carry **new user-visible content** are delivered: regular messages, `thread_broadcast` replies, file shares (a share without a comment renders as `(shared file: …)` so the body is never blank), and other bots' messages (legacy `bot_message` events resolve the sender from `username` / `bot_id`).

Slack reuses `type: "message"` for channel **bookkeeping**, and those events are skipped with a recorded skip reason (`NotificationSkipped` event) instead of waking the agent with an empty notification:

- `message_changed` — edits, **including Slack's own link-unfurl edits** of a message an agent just posted
- `message_deleted` — deletions
- `message_replied` — thread-reply bookkeeping; Slack delivers it **without its subtype** (a documented Slack bug), so it is recognized by its `hidden: true` flag
- system subtypes (`channel_join`, `channel_leave`, `channel_topic`, …) — lines *about* the channel, not messages *to* anyone

These envelopes have no top-level `user`/`text`; delivering them would produce phantom agent turns triaging an empty message.

### Outbound (via Slack MCP tools)

All Slack capabilities — messaging, threading, search, reactions — are provided by **MCP tools** powered by the agent's bot token via [slack-mcp-server](https://github.com/korotovsky/slack-mcp-server). Messages are posted with the agent's own bot identity.

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
`src/crewlet/notifications/typing_status.py`; replace any of them with
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
4. **Outbound send** — the agent sends a reply via ``SlackTransport.send()``

Thread tracking state is persisted in PostgreSQL (``chat_thread_follows`` table, rows keyed ``backend = 'slack'``) so it survives engine restarts. Bot messages are automatically ignored to prevent loops.

Disable thread routing:

```python
slack_transport = SlackTransport(thread_routing=False)
```

---

## Programmatic Setup

```python
from crewlet.notifications.transports.slack import SlackAppConfig, SlackTransport
from crewlet.notifications.typing_status import SlackTypingStatusMode

slack_transport = SlackTransport(
    typing_status_mode=SlackTypingStatusMode.ADDRESSED,
)
slack_transport.register_app("engineer", SlackAppConfig(
    bot_token="xoxb-eng-...",
    signing_secret="secret-eng",
    channel="C0123456789",
))

engine = Engine(
    organization=org,
    notification_transports=[slack_transport],
)
```

The engine automatically registers per-agent Slack apps from `role.integrations.slack` configs during `start()`.
