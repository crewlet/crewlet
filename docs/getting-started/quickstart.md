# Quickstart: Build a Company with CEO, CTO, PM & Engineer

This guide brings up a four-agent company on your machine, with no external
integrations — just the engine, its infrastructure, and an LLM key. At the end
you'll watch an agent run its first turn on the dashboard, then wire in real
integrations one at a time.

Crewlet uses a [two-tier config](../concepts/configuration.md):

- **Tier A** — ops-owned `crewlet.yaml` on disk (where this node's stream,
  store and leases live, API host/port/auth, logging). Restart to change.
- **Tier B** — founder-owned `company.yaml`, imported into the store
  (everything else: identity, roles, units, LLM providers, MCP servers,
  integrations, budgets). Versioned and live-editable.

> **Prefer not to write the YAML by hand?** An AI assistant can interview
> you and author both files, checking its own work against the shipped
> schema — see
> [Authoring with an AI assistant](ai-authoring.md), which walks the same
> ground step by step. This page builds the config manually, which is the
> better way to learn what the fields mean.

## 0. There is no infrastructure to start

Crewlet is one binary. Its event stream is a NATS JetStream server it
embeds, and its database is a local file it creates — so a company runs with
no broker to operate and nothing to point a DSN at. Both slots take an
external address when a deployment outgrows that; see
[Deployment](../guides/deployment.md).

The compose file in a repo checkout is for the *integration* loops
(Mattermost, GitLab) further down this page, not for the engine.

## 1. Write the Tier A bootstrap (`crewlet.yaml`)

```yaml
logging:
  level: debug          # every subsystem's DEBUG lines, in colour when you
                        #   are watching a terminal. Drop the block (or set
                        #   `info`) once the company runs
  format: console       # console (default), text, json

stream:
  type: embedded          # a JetStream server inside this process: no
                          #   listener, no port, no service to operate
  store_dir: "./acme-data/stream"   # leave empty and the stream is
                          #   in-memory — right for a test, and nothing
                          #   published survives a restart

store:
  path: "./acme-data/acme.db"   # ONE file, owned exclusively by this
                          #   process. Not a shared database and no DSN:
                          #   two engines pointed at one file corrupt it

coordination:
  type: local             # a single node holding its own seat leases;
                          #   a fleet needs embedded-kv (see guides/fleet.md)

api:
  host: "0.0.0.0"
  port: 8000        # a port > 0 makes `crewlet run` serve the API EMBEDDED in
                    # the engine process (dashboard + webhooks included) — one
                    # process is the whole stack. (Any free port will do; pick
                    # one nothing else on the host has already taken. The full
                    # Nimbus example uses port 80 instead so webhook URLs need
                    # no port suffix — see examples/nimbus.config.yaml for the
                    # trade-offs.)
  auth:
    # Needed for WRITES and for /config. Reads — the dashboard, /events,
    # /agents — serve without one by default; add
    # `allow_anonymous_read: false` here to guard those too.
    tokens:
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"
```

## 2. Write the Tier B company (`company.yaml`)

```yaml
name: "Acme AI"
mission: "Ship AI-powered products fast"

policies:
  - "All features need PM sign-off before development starts"
  - "Communicate decisions in writing"

providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys:
        - "${ANTHROPIC_API_KEY}"
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: "${OPENAI_API_KEY}"        # used by the agent-learning subsystem
                                        # (diary vector search + episode recall)

# Org-wide roles — these sit above all teams and manage team leads.
roles:
  # You, in the chart. A `kind: human` seat is addressable but never
  # spawned (no runtime, no inbox, no LLM) — it gives escalation a person
  # to stop at, and lets agents recognise your activity on the surfaces
  # you connect later. Needs at least one `contact` identity; scope
  # `manages` to the top seat so you aren't copied on everything.
  - name: Your Name
    kind: human
    manages: [CEO]
    contact:
      # One identity per surface you connect. Swap this for
      # `slack_user_id` (a `U…` member ID) if Slack is your chat.
      mattermost_user_id: "${MATTERMOST_FOUNDER_USERNAME}"   # your chat username

  - name: CEO
    handle: ceo                 # see the note under this block — set these now
    goal: "Set product vision, prioritize initiatives, and make final calls"
    backstory: "Experienced founder who balances speed with quality"
    manages: [CTO, PM]
    # A zero-integration way to see your first agent turn: a scheduled task.
    # Delete this once you have real integrations delivering work.
    schedules:
      - name: hello-crewlet
        cron: "*/5 * * * *"
        task: "Write a short status note on what the company should focus on this week."

# Flexible org structure — use any nesting depth and unit types.
units:
  - name: Product Management
    type: team
    lead: PM
    purpose: "Define what gets built and why"
    roles:
      - name: PM
        handle: pm
        goal: "Turn business goals into clear specs and prioritized backlogs"
        backstory: "Data-driven product manager who writes crisp requirements"
        manages: [Engineer]

  - name: Core Engineering
    type: team
    lead: CTO
    purpose: "Build and ship the product"
    goals:
      - "Ship MVP in 4 weeks"
      - "Maintain test coverage above 80%"
    roles:
      - name: CTO
        handle: cto
        goal: "Set technical direction, make architecture decisions, unblock engineers"
        backstory: "Senior architect with deep distributed systems experience"
        manages: [Engineer]

      - name: Engineer
        handle: eng
        goal: "Implement features, write tests, and ship quality code"
        backstory: "Full-stack engineer who writes clean, tested code"
```

> **Set `handle` now, and keep it.** An agent's durable id is
> `uuid5(namespace, f"{company name}:{handle}")`, so changing a handle —
> *or the company `name`* — mints a new id and orphans that seat's diary,
> onboarding markers, and counterparty profiles. It keeps working, but it
> has lost its memory. Leaving `handle` unset auto-derives it from the
> role name, which ties the id to a label you may well rename later. See
> [Agent Runtime](../concepts/agent-runtime.md#agent-definition-vs-agent-instance).

### LLM options

The `providers.llm` map takes named provider entries (roles select one via
`role.llm`; `default` is the fallback). Three provider types are built in:

```yaml
providers:
  llm:
    # Anthropic (official SDK)
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]

    # OpenAI (official SDK)
    gpt:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]

    # ANY OpenAI-compatible endpoint — a hosted aggregator (OpenRouter,
    # Together, ...), a cloud provider's OpenAI-compatible gateway, or your
    # own vLLM / LiteLLM deployment.
    hosted:
      type: openai-compatible
      model: "your-provider/model-id"
      base_url: "https://api.your-llm-provider.example/v1/"
      timeout_seconds: 300            # large models can be slow; default 120
      api_keys: ["${LLM_API_KEY}"]

    # A coding CLI you already subscribe to (Claude Pro/Max, ChatGPT
    # Plus/Pro, Google AI Pro, a Copilot or Cursor seat) — no API key.
    # The CLI must be installed on the machine running `crewlet run`.
    subscription:
      type: cli-agent
      model: sonnet                   # whatever the CLI's --model accepts
      cli:
        agent: claude-code            # or codex | gemini-cli | opencode | ...
```

A `cli-agent` entry is authenticated once, on the engine host, and then
verified:

```bash
crewlet llm login subscription --capture-token   # or plain `login` for the
                                                 # vendor's browser flow
crewlet llm doctor subscription
```

Each seat gets its own isolated CLI home, so agents never inherit one
another's sessions or memory — that and the auth options are covered in
[Subscription LLM Backends](../concepts/subscription-llm-backends.md).

Useful knobs on every entry: `api_keys` accepts **multiple** keys (the
provider rotates on rate-limit/auth errors), `reasoning: true` enables
extended thinking / reasoning where the model supports it, and different
roles can use different entries (e.g. executives on a frontier model, junior
agents on a cheaper one). A role's `llm` also accepts a **list** — 
`llm: [subscription, default]` runs on the flat-rate CLI and falls through
to the metered key when the subscription window is spent. Embeddings (`providers.embeddings`) power the
agent-learning subsystem and accept any OpenAI-compatible embeddings endpoint
via `base_url`.

### Token budgets (optional)

Control costs with hard caps at the org and/or per-agent level:

```yaml
token_budget: 500000  # org-wide limit (0 or omit = unlimited)

units:
  - name: Core
    type: team
    lead: CTO
    roles:
      - name: CTO
        token_budget: 100000  # per-agent limit
      - name: Engineer
        token_budget: 50000
```

When a budget is exceeded, the agent's turn stops immediately and a
`BudgetExhausted` event is emitted.

Usage is **durable** — it lives in the fleet's
[coordination store](../concepts/coordination.md), so it survives restarts and
is one number for the whole company however many nodes run it. Reset it
deliberately, against a running node:

```bash
crewlet budgets show     # usage per scope, read from the running node
crewlet budgets reset    # zero everything (or -scope agent:<id>)
```

(Usage used to reset on every engine start, which made a cap advisory in
exactly the situation that motivates one — an agent burning budget in a
crash loop.)

## 3. Run it

```bash
export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."          # embeddings
export MATTERMOST_FOUNDER_USERNAME="you"  # your chat username (the human seat)
```

Validate the Tier B YAML first (this reads no environment — `${VAR}`
references are checked as references, so it works before any secret is
exported):

```bash
crewlet validate            # both tiers, from ./crewlet.yaml and ./company.yaml
```

**One command** — migrate the store, seed the Tier B company and run the
engine in a single invocation:

```bash
crewlet run crewlet.yaml -company company.yaml
```

`-company` is a **seed**: it is imported when the store does not already
hold a company, and ignored when it does — so re-running the same command
boots straight from the store rather than overwriting what is live. A
running node then serves the store, not the file.

**Or two steps** — import once, then run:

```bash
crewlet config import company.yaml
crewlet run                             # boots from the store
```

Both flags default to files in the working directory — `crewlet.yaml` and
`company.yaml` — so a node whose files are named that way needs neither.

**Stopping:** press `Ctrl+C` once for a graceful drain (running agent turns
finish; the dashboard stays up so you can watch the in-flight count converge),
twice to force-stop, three times to hard-exit. See
[Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown). Piping
the output? Use `tee -i` (`crewlet run 2>&1 | tee -i run.log`) — a plain `tee`
dies on the first Ctrl+C and the drain logs have nowhere to go.

## 4. Watch the first turn

Open the dashboard at <http://localhost:8000/>. The **Overview** greets you
with your company's name and mission, and says what — if anything — needs a
person. Within five minutes the `hello-crewlet` schedule fires a
`TaskAssigned` at the CEO: the seat appears under **Live seats**, and
**Model activity** shows the turn as it runs — Plan, then Execute, then Review,
each phase listing the rounds it took, the tools each round called, and the
prompts the model actually saw. A phase that finishes updates in place rather
than moving, so you can read one while the next is running.

The same picture is available over the API:

```bash
curl -s http://localhost:8000/agents | python3 -m json.tool
curl -s http://localhost:8000/health
```

No token on those: reads serve without one by default, which is why the
dashboard opened without asking you for anything. That also means anyone who can
reach port 8000 can read the LLM transcripts on `/events` — fine on a laptop,
a decision to make deliberately anywhere else. Set
`api.auth.allow_anonymous_read: false` to guard reads, at which point every call
above needs `-H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER"` and the
dashboard prompts for the token on first load. See
[Configuration § Auth](../concepts/configuration.md#auth) for the full rule.

If you skipped the import, `crewlet run` boots in the **unconfigured** state
with the API still serving — you can then bootstrap live without restarting:

```bash
curl -X PUT http://localhost:8000/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: initial bootstrap" \
  --data-binary @company.yaml
```

## Split deployment (optional)

Run ingress as its own node when you want webhooks to keep arriving while
you restart the agents, or the two on different hosts. Same command, given
different roles:

```bash
crewlet run -roles seats,workers -api-port 0   # terminal 1
crewlet run -roles ingress -api-port 8000      # terminal 2
```

The ingress node exposes the same REST endpoints, webhook handlers
(`/webhooks/jira`, `/webhooks/slack/{handle}`, `/webhooks/github`,
`/webhooks/gitlab`, `/webhooks/confluence`,
`/webhooks/forge`), and the `/config/*` CRUD surface (see
[API Endpoints](../reference/api-endpoints.md)). Mattermost is deliberately
absent from that list — it has no usable inbound webhook, so the **engine**
holds one outbound websocket per agent seat instead, and its inbound path
does not go through the API process at all.

## There is no programmatic setup

The engine has no importable API: every package lives under `internal/`, so
the CLI, the config format and the wire protocol are the whole public
surface. That is deliberate — it means the two-tier config is the only way
to describe a company, and nothing can drift between what a YAML file says
and what an embedding program set up by hand.

Automating a deployment means driving `crewlet` and the REST API:

```bash
crewlet validate                       # check both tiers in CI
crewlet config import company.yaml     # write a new active revision
crewlet run                            # start the node
```

See [Configure via the API](../guides/configure-via-api.md) for editing a
live company, and [Tools & MCP](../guides/tools-and-mcp.md#extending-the-engine)
for adding behaviour the agents can reach.

## Next steps

An org with no integrations only reacts to schedules. Real work arrives
through external surfaces — pick yours in
**[Choosing your stack](choosing-your-stack.md)** (LLM, tracker + knowledge
base, code host, chat, sandbox — with the hosted vs self-hosted options for
each), then wire them in:

- Connect chat so agents collaborate in channels and you can DM them. If
  your company already runs on [Slack](../integrations/slack.md), use it —
  the agents land where the conversations already happen, under the
  workspace admin and compliance setup you already have. It needs a public
  URL for its Events API and one OAuth **Allow** click per agent:
  ```bash
  crewlet slack provision company.yaml -public-url https://your-server.com
  ```
  To try chat on this machine first,
  [Mattermost](../integrations/mattermost.md) ships in this repo's
  `docker-compose.yml` and needs no account, no inbound URL and no clicks:
  ```bash
  docker compose --profile mattermost up -d --wait
  scripts/mattermost-dev-bootstrap.sh
  crewlet mattermost provision company.yaml
  ```
  Running that stack on a **remote host** rather than this machine? Set
  `MATTERMOST_PUBLIC_URL` to the address your browser uses first — see
  [The Site URL](../integrations/mattermost.md#the-site-url). The engine
  needs no public URL; the Mattermost server still needs to know its own.
- Connect a work-item tracker — [Jira](../integrations/jira.md), or the issue
  tracker of the code host you already run
- Connect a knowledge backend — [Confluence](../integrations/confluence.md) —
  so shared procedures surface in the Plan-phase `## Relevant knowledge`
  block; publish version-controlled docs with `crewlet confluence import`
- Connect a code host — [GitLab](../integrations/gitlab.md) or
  [GitHub](../integrations/github.md) — and enable the
  [code sandbox](../concepts/code-sandbox.md) so engineer roles author real
  merge requests
- Fill in the [founder seat](../concepts/humans-in-the-org.md#the-founder-seat)
  you already have at the root — add the `contact` identities for each
  surface you connect (`mattermost_user_id`, `atlassian_account_id`,
  `gitlab_username`, …) so escalations land in your DMs and agents recognise
  your activity
- Encrypt your config at rest — add a Tier A keyring (`crewlet secrets keygen`)
  and run `crewlet config seal` so the **entire** company config is stored
  encrypted in the DB as one opaque blob. See
  [Configuration § Secrets](../concepts/configuration.md#secrets)
- Stop exporting a variable per credential — with that keyring in place,
  `crewlet secrets set LLM_API_KEY` puts the value in the encrypted
  [secret store](../concepts/secret-store.md), which the engine consults ahead
  of the process environment when resolving the `${...}` references you already wrote.
  Provisioners can write there directly (`crewlet gitlab provision …
  -secret-store`), so a minted credential reaches the engine with no file to
  source and no shell to be in
- Explore the full [Nimbus example](../../examples/) — a seven-seat company
  with Jira + Confluence + GitLab + Mattermost + sandbox wired end-to-end
- Add [MCP servers](../guides/tools-and-mcp.md#extending-the-engine) so agents can reach your own systems
- See the full [configuration reference](configuration.md) for all YAML options
