# Quickstart: Build a Company with CEO, CTO, PM & Engineer

This guide brings up a four-agent company on your machine, with no external
integrations — just the engine, its infrastructure, and an LLM key. At the end
you'll watch an agent run its first turn on the dashboard, then wire in real
integrations one at a time.

Crewlet uses a [two-tier config](../concepts/configuration.md):

- **Tier A** — ops-owned `config.yaml` on disk (database DSN, Pulsar URL, API
  host/port/auth, debug). Restart to change.
- **Tier B** — founder-owned `company.yaml`, imported into PostgreSQL
  (everything else: identity, roles, units, LLM providers, MCP servers,
  integrations, budgets). Versioned and live-editable.

> **Prefer not to write the YAML by hand?** An AI assistant can interview
> you and author both files, checking its own work against the shipped
> schema — see
> [Authoring with an AI assistant](ai-authoring.md), which walks the same
> ground step by step. This page builds the config manually, which is the
> better way to learn what the fields mean.

## 0. Start the infrastructure

Crewlet needs **Apache Pulsar** and **PostgreSQL (TimescaleDB + pgvector)**.
From a repo checkout, the bundled compose file provides both (see
[Installation](installation.md) for details and ports):

```bash
cp .env.example .env
docker compose up -d
```

## 1. Write the Tier A bootstrap (`config.yaml`)

```yaml
debug: true

providers:
  queue:
    type: pulsar
    url: "pulsar://localhost:6650"        # the compose broker
  database:
    dsn: "postgresql://crewlet:crewlet@localhost:5432/crewlet"  # the compose DB
  knowledge:
    type: pgvector

api:
  host: "0.0.0.0"
  port: 8000        # a port > 0 makes `crewlet run` serve the API EMBEDDED in
                    # the engine process (dashboard + webhooks included) — one
                    # process is the whole stack. (Avoid 8080: that's Pulsar's
                    # admin port in the bundled compose. The full Nimbus example
                    # uses port 80 instead so webhook URLs need no port suffix —
                    # see examples/nimbus.config.yaml for the trade-offs.)
  auth:
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
      slack_user_id: "${SLACK_FOUNDER_USER_ID}"   # your Slack member ID

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
```

Useful knobs on every entry: `api_keys` accepts **multiple** keys (the
provider rotates on rate-limit/auth errors), `reasoning: true` enables
extended thinking / reasoning where the model supports it, and different
roles can use different entries (e.g. executives on a frontier model, junior
agents on a cheaper one). Embeddings (`providers.embeddings`) power the
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
`BudgetExhausted` event is emitted. Budgets are per-engine-run (the in-memory
counter resets on restart).

## 3. Run it

```bash
export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."          # embeddings
export SLACK_FOUNDER_USER_ID="U0FOUNDER"  # your Slack member ID (the human seat)
```

Validate the Tier B YAML first (this reads no environment — `${VAR}`
references are checked as references, so it works before any secret is
exported):

```bash
crewlet validate company.yaml
crewlet validate config.yaml     # the Tier A file too — tier is auto-detected
```

**One command** — migrate, import the Tier B company, and run the engine in a
single invocation:

```bash
crewlet run config.yaml --import-company company.yaml
```

`--import-company` is idempotent: once a revision is active it's a no-op and
the engine boots straight from the DB. (Use `crewlet config import --force` to
overwrite an existing active revision.)

**Or two steps** — import once, then run:

```bash
crewlet config import company.yaml      # one-shot bootstrap of Tier B
crewlet run                             # boots from ./config.yaml + DB
```

`crewlet run` defaults its config path to `./config.yaml`; pass
`crewlet run /path/to/config.yaml` for non-standard locations.

**Stopping:** press `Ctrl+C` once for a graceful drain (running agent turns
finish; the dashboard stays up so you can watch the in-flight count converge),
twice to force-stop, three times to hard-exit. See
[Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown). Piping
the output? Use `tee -i` (`crewlet run 2>&1 | tee -i run.log`) — a plain `tee`
dies on the first Ctrl+C and the drain logs have nowhere to go.

## 4. Watch the first turn

Open the dashboard at <http://localhost:8000/>. Within five minutes the
`hello-crewlet` schedule fires a `TaskAssigned` at the CEO, and you'll see the
agent go `Working`, step through **Plan → Execute → Review**, and return to
`Idle` — with every LLM invocation, prompt, and tool call inspectable in the
UI. The same picture is available over the API:

```bash
curl -s http://localhost:8000/agents | python3 -m json.tool
curl -s http://localhost:8000/health
```

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

Run the API as its own process when you want webhooks to keep arriving while
you restart the engine, or the two on different hosts. Set `api.port: 0` in
the Tier A file (so the engine's embedded API stays off), then:

```bash
crewlet run config.yaml           # terminal 1: the engine
crewlet run api config.yaml       # terminal 2: the API (binds 8000 by default)
```

The standalone API exposes the same REST endpoints, webhook handlers
(`/webhooks/jira`, `/webhooks/slack/{handle}`, `/webhooks/github`,
`/webhooks/gitlab`, `/webhooks/plane`, `/webhooks/confluence`,
`/webhooks/forge`), and the `/config/*` CRUD surface (see
[API Endpoints](../reference/api-endpoints.md)).

## Programmatic setup

```python
import asyncio
from crewlet import Engine
from crewlet.config import load_bootstrap_config, load_company_config


async def main():
    bootstrap = load_bootstrap_config("config.yaml")
    engine = Engine.from_bootstrap(bootstrap)
    await engine.apply_config(load_company_config("company.yaml"))
    await engine.run()  # blocks until Ctrl+C


asyncio.run(main())
```

## Next steps

An org with no integrations only reacts to schedules. Real work arrives
through external surfaces — pick yours in
**[Choosing your stack](choosing-your-stack.md)** (LLM, tracker + knowledge
base, code host, chat, sandbox — with the hosted vs self-hosted options for
each), then wire them in:

- Connect [Slack](../integrations/slack.md) so agents collaborate in channels
  and you can DM them — write the `${SLACK_*}` placeholders into each role's
  `integrations.slack`, then let
  [`crewlet slack provision`](../reference/cli.md#crewlet-slack-provision)
  create one app per agent and fill them in:
  ```bash
  crewlet slack provision company.yaml --base-url https://your-server.com
  ```
- Connect a work-item tracker — self-hosted [Plane](../integrations/plane.md)
  (also ships a full local docker-compose loop), or
  [Jira](../integrations/jira.md)
- Connect a knowledge backend — [Plane](../integrations/plane.md) pages or
  [Confluence](../integrations/confluence.md) (one, not both) — so shared
  procedures surface in the Plan-phase `## Relevant knowledge` block; publish
  version-controlled docs with `crewlet plane import` / `crewlet confluence import`
- Connect a code host — [GitLab](../integrations/gitlab.md) or
  [GitHub](../integrations/github.md) — and enable the
  [code sandbox](../concepts/code-sandbox.md) so engineer roles author real
  merge requests
- Fill in the [founder seat](../concepts/humans-in-the-org.md#the-founder-seat)
  you already have at the root — add the `contact` identities for each
  surface you connect (`slack_user_id`, `plane_user_id`, `gitlab_username`,
  …) so escalations land in your DMs and agents recognise your activity
- Encrypt your config at rest — add a Tier A keyring (`crewlet secrets keygen`)
  and run `crewlet config seal` so the **entire** company config is stored
  encrypted in the DB as one opaque blob. See
  [Configuration § Secrets](../concepts/configuration.md#secrets)
- Stop exporting a variable per credential — with that keyring in place,
  `crewlet secrets set LLM_API_KEY` puts the value in the encrypted
  [secret store](../concepts/secret-store.md), which the engine consults ahead
  of `os.environ` when resolving the `${...}` references you already wrote.
  Provisioners can write there directly (`crewlet gitlab provision …
  --secret-store`), so a minted credential reaches the engine with no file to
  source and no shell to be in
- Explore the full [Nimbus example](../../examples/) — a seven-seat company
  with Plane + GitLab + Slack + sandbox wired end-to-end
- Write [extensions](../guides/extensions.md) to hook into engine events
- See the full [configuration reference](configuration.md) for all YAML options
