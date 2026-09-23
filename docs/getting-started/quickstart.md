# Quickstart: Build a Company with CEO, CTO, PM & Engineer

This guide brings up a four-agent company on your machine, with no external
integrations — just the engine, its infrastructure, and an LLM key. At the end
you'll watch an agent run its first turn on the dashboard, then wire in real
integrations one at a time.

Crewlet uses a [two-tier config](../concepts/configuration.md):

- **Tier A** — ops-owned `crewlet.yaml` on disk (where this node's stream,
  store and leases live, API host/port/auth, logging — including where it
  writes). Restart to change.
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
  file:                 # optional, and worth it even here: a durable copy
    path: "./acme-data/crewlet.log"   # IN ADDITION to what you are watching.
                        #   It rotates itself at 100 MB, keeping five files,
                        #   so scrolling past the interesting line costs
                        #   nothing. See Deployment → Logging

stream:
  type: embedded          # a JetStream server inside this process: no
                          #   listener, no port, no service to operate
  store_dir: "./acme-data/stream"   # REQUIRED. Leave it empty and the
                          #   stream is in-memory, so nothing published
                          #   survives a restart — including this company's
                          #   own org chart, which every company keeps on a
                          #   state log whatever backends it names, and its
                          #   items and pages, which are on the engine's own
                          #   tracker and knowledge base here. The engine
                          #   REFUSES to boot without one rather than lose
                          #   them at the first restart

store:
  path: "./acme-data/acme.db"   # this node's own database, owned
                          #   EXCLUSIVELY by this process. Not a shared
                          #   database and no DSN: two engines pointed at one
                          #   path corrupt it. A second file is created beside
                          #   it for the replicated estate — a snapshot is a
                          #   copy of one of the two, which is why they are
                          #   not one file. See concepts/architecture.md

coordination:
  type: local             # a single node holding its own seat leases;
                          #   a fleet needs embedded-kv (see guides/fleet.md)

api:
  host: "0.0.0.0"
  port: 8000        # a port > 0 makes `crewlet run` serve the API EMBEDDED in
                    # the engine process (dashboard + webhooks included) — one
                    # process is the whole stack. (Any free port will do; pick
                    # one nothing else on the host has already taken. Port 80
                    # is worth the privileged bind only once an external
                    # service registers a webhook URL against this engine —
                    # see guides/deployment.md.)
  external_url: "http://localhost:8000"
                    # REQUIRED once a port is set. Where a BROWSER reaches
                    # this deployment, which is not what the two lines above
                    # bind: its scheme decides the session cookie's flags and
                    # its host is the origin every write is checked against.
                    # Behind a proxy, the public address.
  auth:
    backend: none   # nobody signs in on a laptop — the token below is the
                    #   only credential. `local` adds passwords and a second
                    #   factor; `oidc` hands sign-in to a provider. On `local`,
                    #   `POST /auth/bootstrap` creates the first person from a
                    #   one-time code this node writes beside its store.
    max_grants:     # THE CEILING on what this deployment will ever let a
                    #   directory record confer. Required once a port is set.
      [state:read, audit:read, config:read, secrets:read, work:write,
       knowledge:write, config:write, secrets:write, fleet:operate,
       people:manage, sandbox:run]
    tokens:         # at least one is REQUIRED once a port is set: every route
                    #   needs a credential, and this is also what creates the
                    #   first person on a fresh deployment.
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"   # 26 characters at minimum
        grants:     # what this credential may do — required and non-empty.
          [state:read, audit:read, config:read, secrets:read, work:write,
           knowledge:write, config:write, secrets:write, fleet:operate,
           people:manage, sandbox:run]

secrets:                  # REQUIRED. Every record on every state log — the
                          #   tracker's, the knowledge base's — is signed
                          #   under this keyring, because the broker has no
                          #   auth of its own and a record is whatever the
                          #   next node applies. `crewlet secrets keygen`
                          #   mints one and prints the export line
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"
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
    model: text-embedding-3-large       # the model decides the vector width
                                        # (3072 here), so there is no
                                        # `dimensions` to set
    api_key: "${OPENAI_API_KEY}"        # used by the agent-learning subsystem
                                        # (diary vector search + episode recall)

# Org-wide roles — these sit above all teams and manage team leads.
roles:
  # You, in the chart. A `kind: human` seat is addressable but never
  # spawned (no runtime, no inbox, no LLM) — it gives escalation a person
  # to stop at, and lets agents recognise your activity on the surfaces
  # you connect later. `contact` is optional — without one you are
  # reached through the dashboard only, and agents cannot @-mention you on
  # chat; scope `manages` to the top seat so you aren't copied on everything.
  - name: Your Name
    kind: human
    manages: [ceo]              # BY HANDLE. A `manages:` entry names a seat by
                                # its handle and a unit by its key — never by a
                                # display name, which is prose you will edit
    contact:
      # One identity per surface you connect. Swap this for
      # `slack_user_id` (a `U…` member ID) if Slack is your chat.
      mattermost_user_id: "${MATTERMOST_FOUNDER_USERNAME}"   # your chat username

  - name: CEO
    handle: ceo                 # see the note under this block — set these now
    goal: "Set product vision, prioritize initiatives, and make final calls"
    backstory: "Experienced founder who balances speed with quality"
    manages: [cto, pm]
    # A zero-integration way to see your first agent turn: a scheduled task.
    # Delete this once you have real integrations delivering work.
    schedules:
      - name: hello-crewlet
        cron: "*/5 * * * *"
        task: "Write a short status note on what the company should focus on this week."

# Flexible org structure — use any nesting depth and unit types.
units:
  - name: Product Management
    id: pm-team                 # the unit's KEY: what a `manages:` entry and a
                                # root seat's `unit:` resolve. The name above is
                                # display. Leave it out and one is minted from
                                # the name at import (`product-management` here)
    type: team
    lead: pm                    # the lead seat's HANDLE
    purpose: "Define what gets built and why"
    # A unit's identity on the tracker and the knowledge base. Both default
    # to the engine's own backends, so these two keys are all it takes to
    # give the team somewhere to file work and write things down — no site,
    # no project to create, no per-seat account. They name a Jira project and
    # a Confluence space just as well if you switch the backend later, which
    # is why they are not called `jira_project` / `confluence_space`.
    project: PROD
    space: PROD
    roles:
      - name: PM
        handle: pm
        goal: "Turn business goals into clear specs and prioritized backlogs"
        backstory: "Data-driven product manager who writes crisp requirements"
        manages: [eng]

  - name: Core Engineering
    id: eng-team
    type: team
    lead: cto
    purpose: "Build and ship the product"
    project: ENG
    space: ENG
    goals:
      - "Ship MVP in 4 weeks"
      - "Maintain test coverage above 80%"
    roles:
      - name: CTO
        handle: cto
        goal: "Set technical direction, make architecture decisions, unblock engineers"
        backstory: "Senior architect with deep distributed systems experience"
        manages: [eng]

      - name: Engineer
        handle: eng
        goal: "Implement features, write tests, and ship quality code"
        backstory: "Full-stack engineer who writes clean, tested code"
```

> **Set `handle` now, and keep it.** The handle is the seat's identity twice
> over: an agent's durable id is a UUIDv5 over `"<company name>:<handle>"`
> (`org.DeriveAgentID`), and it is also what every `lead:` and `manages:` in
> the chart above resolves. So changing a handle, *or the company `name`*,
> mints a new id and orphans that seat's diary, onboarding markers and
> counterparty profiles. It keeps working, but it has lost its memory.
> Leaving `handle` unset auto-derives it from the role name, which ties the id
> to a label you may well rename later. A seat's `name` is display and is
> referenced by nothing, so renaming one is free. See
> [Agent Runtime](../concepts/agent-runtime.md#seat-definition-and-the-runner).

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
        agent: claude-code            # or codex | gemini-cli | opencode
                                      #    | muse-code | kimi-code | hermes
                                      #    | pi | ...
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
    id: core
    type: team
    lead: cto
    roles:
      - name: CTO
        handle: cto
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
crewlet secrets keygen -key-id 2026-01   # prints the export line below
export CREWLET_SECRET_KEY_2026_01="<the base64 key it printed>"
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

`-company` is a **bootstrap seed**: it is imported when the store holds no
company yet, and once one exists it is ignored — with a warning that says so,
naming the file and the two ways to apply it. So a restart never reverts a
change you made live, however stale the file on disk is.

When you do want the file to win, that is a different flag:
`crewlet run -import-company company.yaml` makes it the active revision over
whatever the fleet is running. And to change a **running** fleet with no
restart at all, use `crewlet config import company.yaml` — it goes through the
node's API and every node converges on it.

Both of those write the file's **settings**. A company file also carries the
org chart — its `roles:` and `units:` — and that is a domain of its own, with
its own records and its own history (see
[The org chart domain](../concepts/chart-domain.md)). A first deployment gets
its chart from the file, seeded by `crewlet run -company` when the chart is
empty; after that the chart belongs to whoever edits it, and `config import`
says plainly that the file's units and seats were not published.

A running node always serves the store, not the file.

**Or two steps** — import once, then run:

```bash
crewlet config import company.yaml
crewlet run                             # boots from the store
```

Both flags default to files in the working directory — `crewlet.yaml` and
`company.yaml` — so a node whose files are named that way needs neither.

**Stopping:** press `Ctrl+C` once for a graceful drain. Running agent turns
finish, and the HTTP surface stays up until they have: the dashboard shows the
node draining, `/health` stays `200` and `/ready` answers `503`, while every
route that would start new work (a webhook, a config write) answers `503`
naming the drain, so nothing new arrives while it converges. The listener
closes once the drain completes. Press it a **second** time to exit at once;
the first press hands signal handling back to the OS precisely so that works.
There is no third tier. See
[Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown). Want to
keep the drain's log? The `logging.file` block above already has it: the
engine writes the file itself, so the drain is recorded whatever happens to the
terminal. Piping instead? Use `tee -i` (`crewlet run 2>&1 | tee -i run.log`),
because a plain `tee` dies on the first Ctrl+C and the drain's log has nowhere
to go.

## 4. Watch the first turn

Open the dashboard at <http://localhost:8000/>. It lands on the **Inbox**,
which is what a person opening this wants first: whether anything is waiting on
them. With no company activity yet it says so, and lists any condition the
engine itself raised.

The rail on the left is the product in eight rows — Inbox, My work, Work,
Company, Knowledge, Activity, Cost, Admin — and each one opens its own tree
beside it. `g` then a letter jumps between them.

Within five minutes the `hello-crewlet` schedule fires a `TaskAssigned` at the
CEO. **Activity** shows it: *Live now* has the seat working, and **Turns**
shows the turn as it runs — Plan, then Execute, then Review, each phase listing
the rounds it took, the tools each round called, and the prompts the model
actually saw. A phase that finishes updates in place rather than moving, so you
can read one while the next is running.

Follow the turn to **Work** and **Knowledge**. Both are the engine's own —
`tracker.backend` and `knowledge.backend` default to `native`, so your company
has a tracker and a wiki from its first minute with nothing to set up. A seat
files with `create_work_item` and writes with `write_page`; a board row opens
the item beside the board, and ⌘-click opens its page.

**Bind your token to your seat** and the personal screens become yours. The
binding lives in the engine's identity directory rather than in either config
file: your token acts under the login `token:founder`, so enrol that login as a
machine and bind it to the human seat above (`your-name`, the handle derived
from `name: Your Name`):

```bash
export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"   # what `crewlet iam` authenticates with
crewlet iam create -kind machine -login token:founder   # prints the new row's id
crewlet iam bind <that id> your-name
```

**My work** and the **Inbox** then answer for that person, and every rule that
asks "do you lead this" is asked about your seat. Until then the dashboard says
so rather than guessing — an unbound token is an ordinary state, not a fault.
Both commands take `people:manage`, which is why the token above carries it.
See [Humans in the Org
Chart](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api).

### When people sign in rather than share a token

`backend: none` is right for a laptop and wrong the moment more than one person
uses this. On `local` or `oidc` the first operator is created once, from a
**one-time code this node writes beside its store**:

```
2026-06-14 12:00 WARN engine  bootstrap_code_written path=/var/lib/crewlet/bootstrap-code
```

Read that file on the host, open the dashboard, and set a login (dotted, like
`jane.doe` — every person has one, and it is the name your changes are recorded
under until you bind a seat), your address and a password.
Nothing ever serves the code — the log line, `/health` and the welcome screen
carry its **path**, so reading it means having access to the machine, which is
the only credential a company genuinely has before it has any. The file is
removed when it is used, and the route closes for good the moment anybody is
enrolled.

Everybody after the first arrives by invitation, which confers exactly the
grants and reach whoever issued it chose; the link's form proposes a login from
their address, which they keep or change. A Tier A token stays — it is the way
back in when the provider is down, and what a pipeline uses — but it is a
machine credential rather than a person, and it is not how people should be
signing in.

Your own AI assistant can read and write the same records over MCP. Point any
client at `/operator/mcp` with your API token:

```json
{
  "mcpServers": {
    "crewlet": {
      "type": "http",
      "url": "http://localhost:8000/operator/mcp",
      "headers": { "Authorization": "Bearer ${CREWLET_API_TOKEN_FOUNDER}" }
    }
  }
}
```

It gets the same thirteen tools a seat holds, and its writes are attributed to
the token's own name rather than to a seat — so an audit can tell your edit
from an agent's. See
[the operator surface](../reference/api-endpoints.md#operatormcp--your-own-assistant).

Once you sign in as a person, give the assistant **your own** token rather than
the deployment's. It acts as you — your seat, your grants — and every write it
makes also records `pat:<its id>`, so what the assistant did stays tellable
from what you did. Nobody else can mint it, an administrator included; you mint
it yourself, and the command signs in as you for that one request:

```bash
crewlet iam token -login jane.doe -label "my assistant"   # asks for your password
```

See [machine tokens](../concepts/identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts).

The same picture is available over the API:

```bash
curl -s -H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER" \
  http://localhost:8000/agents | python3 -m json.tool
curl -s http://localhost:8000/health
```

**Every route needs that credential, reads included** — `/health` and `/ready`
are probes and are the exception, which is why the second call carries nothing.
That is also why the dashboard asked you for the token on first load. Reads used
to serve without one, which meant anyone who could reach port 8000 could read
the LLM transcripts on `/events`; see
[Configuration § Auth](../concepts/configuration.md#auth) for what replaced
that posture and why it could not simply be defaulted the other way.

If you skipped the import, `crewlet run` boots in the **unconfigured** state
with the API still serving — you can then bootstrap live without restarting:

```bash
curl -X PUT http://localhost:8000/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN_FOUNDER" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: initial bootstrap" \
  --data-binary @company.yaml
```

Or create the company from the dashboard: open **Company** and its
**Builder** lens (`#/company?lens=builder`). With no configuration active it opens
on a form that starts the company from a template, has the engine check it,
and creates it with `PUT /config`. The builder reads and writes `/config`, so
it asks for a credential: press **Set token** and paste
`$CREWLET_API_TOKEN_FOUNDER`, whose entry carries the `config:read` and
`config:write` it needs. The
dashboard writes no model provider, so one step stays outside it. Until it is
done the company runs and no agent seat takes a turn; whatever is sent to a
seat waits on its inbox. Add `providers.llm` afterwards with
`crewlet config import` or `PATCH /config`, as the builder's next steps show
([The Org Builder](../guides/org-builder.md#creating-the-company)), and the
work that waited runs.

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
does not go through an HTTP route at all.

## There is no programmatic setup

The engine has no importable API: every package lives under `internal/`, so
the CLI, the config format and the wire protocol are the whole public
surface. That is deliberate — it means the two-tier config is the only way
to describe a company, and nothing can drift between what a YAML file says
and what an embedding program set up by hand.

Automating a deployment means driving `crewlet` and the REST API:

```bash
crewlet validate                       # check both tiers in CI
crewlet config import company.yaml     # write a new active revision (settings)
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
  so shared procedures surface in the executor's `## Relevant knowledge`
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
- Encrypt your config at rest — you already have the keyring, so this is one
  command: run `crewlet config seal` and the **entire** company config is stored
  encrypted in the DB as one opaque blob. See
  [Configuration § Secrets](../concepts/configuration.md#secrets)
- Stop exporting a variable per credential — with that keyring in place,
  `crewlet secrets set LLM_API_KEY` puts the value in the encrypted
  [secret store](../concepts/secret-store.md), which the engine consults ahead
  of the process environment when resolving the `${...}` references you already wrote.
  Provisioners can write there directly (`crewlet gitlab provision …
  -secret-store`), so a minted credential reaches the engine with no file to
  source and no shell to be in
- Explore the [Nimbus example](../../examples/) — a seven-seat company on
  Mattermost, running on a coding-CLI subscription rather than an API key.
  It is the next step up from this page and still needs nothing but the chat
  server in this repo's compose file; every integration above adds on top of
  it
- Add [MCP servers](../guides/tools-and-mcp.md#extending-the-engine) so agents can reach your own systems
- See the full [configuration reference](configuration.md) for all YAML options
