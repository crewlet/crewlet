# Quickstart: Build a Company with CEO, CTO, PM & Engineer

This guide brings up a four-agent company on your machine, with no external
integrations — just the engine, its infrastructure, and a model: Claude Code
signed in with an Anthropic API key or your Claude plan. At the end
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
embeds, and its database is two local files it creates — so a company runs
with no broker to operate and nothing to point a DSN at. The stream takes an
external address when a deployment outgrows that; see
[Deployment](../guides/deployment.md).

What it does need is the **coding CLI its model runs through**: every model is
reached through one, driven as a process on this machine. This page uses
Claude Code, so `claude --version` should answer before you go on — see
[Installation § A coding CLI](installation.md#a-coding-cli) for it, Codex and
OpenCode.

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
                          #   survives a restart — including the people
                          #   who sign in, whom every node keeps on a
                          #   state log whatever backends the company
                          #   names, and its items, pages and messages,
                          #   which are on Worklet, Doclet and Chatlet
                          #   here. The engine
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
    totp: optional  # people sign in with a login and a password; unset, a
                    #   second factor (an authenticator app) is REQUIRED too.
                    #   `optional` is fine here because the browser reaches
                    #   this on loopback; leave the line out anywhere else.
                    #   Nobody holds a password yet: the token below invites
                    #   the first person onto their seat (step 4).
    admin: serve    # whether THIS node grants admin to those who hold it:
                    #   `serve`, or `refuse` to clamp admin away here and keep
                    #   every app. Required once a port is set.
    tokens:         # at least one is REQUIRED once a port is set: every route
                    #   needs a credential, and this is also what puts the
                    #   first person on their seat on a fresh deployment.
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"   # 26 characters at minimum
        grants: [admin]   # what this credential may do — required and
                          #   non-empty: admin, worklet, doclet,
                          #   artifacts, chatlet. Admin runs the company and
                          #   includes every app.

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
    default:                            # Claude Code, the default CLI
      model: sonnet                     # in the CLI's own grammar: an alias or a model id
      auth:
        mode: api-key                   # your Anthropic API key, read from ANTHROPIC_API_KEY.
                                        #   On a Claude Pro / Max plan instead, drop this block
                                        #   and sign in once — see "LLM options" below
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
  # you connect later. It is also the seat you join on in step 4: every
  # person holds a human seat, so a company with none admits nobody.
  # `contact` is optional — without one you are reached through the
  # dashboard only, whose chat is the engine's own and mentions you by your
  # handle, and agents cannot @-mention you on a vendor's chat you connect
  # later; scope `manages` to the top seat so you aren't copied on
  # everything.
  - name: Your Name
    handle: founder             # see the note under this block — set these now
    kind: human
    manages: [ceo]              # BY HANDLE. A `manages:` entry names a seat by
                                # its handle and a unit by its key — never by a
                                # display name, which is prose you will edit
    contact:
      # One identity per surface you connect. Swap this for
      # `slack_user_id` (a `U…` member ID) if Slack is your chat.
      mattermost_user_id: "${MATTERMOST_FOUNDER_USERNAME}"   # your chat username

  - name: CEO
    handle: ceo
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
    # The team's room. Chat defaults to Chatlet too, so this opens
    # a channel called `product` with this unit's seats in it — and it is an
    # address, so it is `product` rather than `#product` or `Product Team`.
    channel: product
    # A unit's identity on the tracker and the knowledge base. Both default
    # to Worklet and Doclet, so these two keys are all it takes to
    # give the team somewhere to file work and write things down: no site,
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
    channel: engineering
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
> A seat that leaves `handle` unset gets one derived from its `name`, and
> every write stores it — so an edit of the *stored* company (the org
> builder, `PATCH /config`, the per-entity routes) keeps it when a name is
> corrected. A *file* does not: every import of one (`crewlet config import`,
> `crewlet run -import-company`) mints a missing handle afresh from the name
> the file holds, so correcting a name in a file
> that leaves the handle out replaces the seat. Declare `handle` on every
> seat, and `id` on every unit, in any file you keep. With its handle
> declared, a seat's `name` is display and referenced by nothing, so renaming
> one is free. See
> [Agent Runtime](../concepts/agent-runtime.md#seat-definition-and-the-runner).

### LLM options

The `providers.llm` map takes named entries (roles select one via `role.llm`;
`default` is the fallback). **Every entry is a coding CLI** the engine drives —
the vendor's own harness, signed in with your plan or your key — and its fields
are that CLI's settings: which CLI (`agent`), which model in its grammar, and
how it signs in (`auth`):

```yaml
providers:
  llm:
    # Anthropic's models through Claude Code, on an API key
    default:
      model: sonnet
      auth: { mode: api-key }           # ANTHROPIC_API_KEY

    # ... or on your Claude Pro / Max plan: no key at all
    plan:
      model: sonnet

    # OpenAI's models through Codex
    gpt:
      agent: codex
      model: gpt-5
      auth: { mode: api-key }           # CODEX_API_KEY; leave it out for a ChatGPT login

    # Open models through OpenCode — on OpenRouter here, or any provider it speaks to
    open:
      agent: opencode
      model: openrouter/qwen/qwen3-coder  # <provider>/<model>
      env:
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"
```

An entry on a **plan** is signed in once, on the engine host — the CLI's own
login, landing in a directory of Crewlet's rather than your personal one:

```bash
crewlet llm login plan -from-host       # adopt the `claude` login this machine already has
crewlet llm login plan -capture-token   # or mint a headless token into the secret store
```

An entry on a **key** needs nothing but the key, exported or stored with
`crewlet secrets set ANTHROPIC_API_KEY`. Before the node's first start, or
with it stopped, a captured token and a stored key both go straight into the
fleet's [secret store](../concepts/secret-store.md), sealed under the keyring
[step 3](#3-run-it) exports: the command starts the node's own broker on
`stream.store_dir`, listening on nothing, writes, and stops it, and the engine
adopts that store when it starts. While the command holds the store,
`crewlet run` on the node is refused, naming its pid — two brokers on one
directory would corrupt it. Once the node runs, the same commands go through
its API instead, with a Tier A token in `CREWLET_API_TOKEN`. Either way,
`crewlet llm doctor` checks
every entry end to end — the binary, its version, how it is signed in, and a
real tool call — before a seat depends on it. Each seat gets its own isolated
CLI home, so agents never inherit one another's sessions or memory — that,
every sign-in route and the rest of the CLIs are covered in
[LLM Backends](../concepts/llm-backends.md).

Useful knobs: different roles can use different entries (executives on a
frontier model, junior agents on a cheaper one), and a role's `llm` also
accepts a **list** — `llm: [plan, default]` runs on the flat-rate plan and
falls through to the metered key while the plan's window is spent. Point a
role's `llm_auxiliary` (reflection, summaries, the turn-start filters) at a
fast, cheap entry: each of those calls pays a CLI launch under a fixed
deadline — see [Auxiliary work and
latency](../concepts/llm-backends.md#auxiliary-work-and-latency). There is no
temperature, effort or output cap to set: each CLI configures its own model.
Embeddings (`providers.embeddings`) power the agent-learning subsystem and are
the one model call the engine makes itself, to any OpenAI-compatible embeddings
endpoint via `base_url`.

### Token budgets (optional)

Control costs with ceilings at the org and/or per-agent level, one per
calendar window on the company's clock — `day`, `week` and `month`, each
optional:

```yaml
token_budget:          # org-wide, every seat together
  day: 500000          #   a runaway loop burns at most a day's allowance
  month: 8000000       #   the bill

units:
  - name: Core
    id: core
    type: team
    lead: cto
    roles:
      - name: CTO
        handle: cto
        token_budget: {day: 100000}     # this seat alone, on top of the org's
      - name: Engineer
        token_budget: {week: 250000}
```

A day runs from local midnight to midnight, a week is the ISO week from
Monday, a month the calendar month, and each window opens again on its own
when it turns over. Leave a key out for no ceiling on that window — `0` is
refused rather than read as unlimited. When a round would take a window past
its ceiling, the agent's turn stops immediately and a `budget_exhausted` event
is emitted, naming the window and when it resets. Until then the agent's new
messages wait on its inbox rather than being lost, and they are delivered when
the window turns over — or at once, if you raise the ceiling.

Usage is **durable** — it lives in the fleet's
[coordination store](../concepts/coordination.md), so it survives restarts and
is one number for the whole company however many nodes run it. Read it
against a running node:

```bash
crewlet budgets show     # every scope's day, week and month, spent and capped
```

There is no reset: a window's allowance comes back when it turns over, and
room before then is made by raising its ceiling through `/config` — a seat's
with `PUT /config/roles/{handle}` (see
[Budgets and spend](../guides/budgets-and-spend.md#budget-windows)).

## 3. Run it

```bash
crewlet secrets keygen -key-id 2026-01   # prints the export line below
export CREWLET_SECRET_KEY_2026_01="<the base64 key it printed>"
export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
export ANTHROPIC_API_KEY="sk-ant-..."   # the `default` entry's key (auth.mode api-key)
export OPENAI_API_KEY="sk-..."          # embeddings
export MATTERMOST_FOUNDER_USERNAME="you"  # your chat username (the human seat)
```

Validate the Tier B YAML first (this reads no environment — `${VAR}`
references are checked as references, so it works before any secret is
exported):

```bash
crewlet validate            # both tiers, from ./crewlet.yaml and ./company.yaml
```

Then check the model end to end. This one *does* read the environment, and it
sends real calls — a smoke test with a tool and two isolation probes — so it is
the one check that proves the CLI is installed, signed in and still matches the
profile Crewlet drives it with:

```bash
crewlet llm doctor          # every providers.llm entry; -no-smoke skips the real calls
```

**One command** — migrate the store, seed the Tier B company and run the
engine in a single invocation:

```bash
crewlet run crewlet.yaml -company company.yaml
```

`-company` is a **bootstrap seed**: it is imported when no company exists yet,
in the node's store or anywhere in its fleet, and once one exists it is
ignored — with a warning that says so, naming the file and the two ways to
apply it. So a restart never reverts a change you made live, however stale the
file on disk is.

When you do want the file to win, that is a different flag:
`crewlet run -import-company company.yaml` makes it the company over
whatever the fleet is running. And to change a **running** fleet with no
restart at all, use `crewlet config import company.yaml` — it goes through the
node's API and every node converges on it.

A running node never goes back to the file: it runs what its fleet activated.

**Or two steps** — import once, then run:

```bash
crewlet config import company.yaml
crewlet run                             # boots from the store
```

With the engine stopped, the import is what this node boots on, and its start
publishes it to the fleet: as the first company where the fleet has none, and
otherwise only while the fleet still runs the revision the import was built
on, so a change made on a node that has fallen behind is never published over
what the fleet moved on to ([`crewlet config
import`](../reference/cli.md#crewlet-config-import)).

Both flags default to files in the working directory — `crewlet.yaml` and
`company.yaml` — so a node whose files are named that way needs neither.

**Starting:** the port answers within a second of `crewlet run`, before the
node is ready to serve. While it boots, `curl -s localhost:8000/health` says
`"status": "starting"` — `503` while the node opens its store and broker, `200`
once it is bringing up the company's tools — and every other route answers
`503 starting` with a `Retry-After`; the dashboard is there once the log says
`api_listening` and `/ready` answers `200`. See
[While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting).

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

## 4. Sign in, and invite yourself

Open the dashboard at <http://localhost:8000/>. **Opening it means signing
in**: every route needs a credential, reads included, so the first thing it
shows is its sign-in screen. People sign in with a login and a password, and
nobody has one yet — nobody has joined — so the screen says so and opens its
**API token** form (on a deployment somebody has joined, it is behind **Use an
API token instead**). Paste `$CREWLET_API_TOKEN_FOUNDER`: it is
exchanged for a one-hour session, and the browser keeps the session's cookie
and never the token, which is sent once, in a header, and held nowhere
afterwards.

**Invite yourself onto your seat.** Every person holds exactly one human seat
for as long as they exist, so you join the company on the one `company.yaml`
declares for you, the human seat with the handle `founder`. Open **Company ›
Org chart**: your seat's card (under the name you gave it) reads **Open seat**,
because nobody holds it yet. Click the card to open the seat beside the
chart: its foot offers **Invite** and **Add person** (the same two are on
the card itself, as icons that appear when you point at it or in its menu
from the keyboard, and on the seat's page). Press **Invite** — the dialog is
for that seat and offers no other — and fill in

- **your email address** — what the invitation is held against;
- **your access**: tick **Admin**, which ticks and locks every app (Worklet,
  Doclet, Artifacts and Chatlet) beside it. The founder token holds `admin`, and nobody can
  confer a permission they do not hold. Keep Admin, or nobody after you can be
  invited or created except with the token again.

The link it shows is the credential and is shown **once** — copy it now. The
engine sends no mail; for anybody else you invite, getting it to them is
yours. Until the link is redeemed, the open invitation holds the seat: the
card reads **Invited ·** your address, and nobody else can be put on it.

(**Add person** is the other way onto a vacant seat:
it creates the person active at once and shows a one-time **password link**
that sets their first password. For yourself the invitation is the shorter
road, since redeeming it signs you in.)

**Open the link** (this browser is fine). It is the dashboard's invitation
screen: it names who the invitation is for and the seat it binds — and who
sent it when a person did; this one was issued under the founder token, a
machine nobody holding the link would recognise, so it names no sender — and
asks for a login — it proposes one from your address, dotted like
`jane.doe`, how you sign in beside your address — your name, and a
password of at least twelve characters (`api.auth.min_password_length` raises
that). Redeeming it puts you on the `founder` seat and signs you in: from your
first sign-in you act as that seat. The secret after the `.` never leaves the
page in a URL: the link carries it in the fragment, which no browser sends to a
server, and the screen sends it in a header and a body instead.

This walkthrough's `crewlet.yaml` says `totp: optional`, so a password alone
signs you in here. **Leave that line out on a deployment reached off
loopback** — unset, a second factor is required, and the session a redemption
or a password sign-in opens for somebody holding none may only enrol one: the
dashboard shows a QR code to scan with an authenticator app on your phone
(with the key to type, and the `otpauth://` link for an app on the same
device, beside it), takes the first code the app shows, and shows the
**recovery codes once**. See [A required second factor
is enrolled before anything
else](../concepts/identity-and-access.md#a-required-second-factor-is-enrolled-before-anything-else).

From now on the sign-in screen takes your login or address and your password.
**Your name at the sidebar's foot opens your own Account** — your password,
two-step verification (an authenticator app, which you can set up there even
where it is optional) and recovery codes, where you are signed in and your
personal access tokens. The menu beside your name holds the same link and
signing out, here or everywhere at once. A gesture that changes who may do what, or how somebody
proves who they are, asks you to confirm your password first, in one dialog,
and then carries on. The founder token stays: it is what a pipeline uses, and
the way back in when nobody who can sign in is available — a machine
credential, not how people should be signing in. Everybody after you arrives
the same way, on a seat of their own: add a human seat for them (**Org chart ›
Edit org › Add human seat**), and once it is saved an administrator invites
them onto it, or creates them on it, from its card in the org chart — with
every app ticked by default and Admin not. A person handed no permission at all
still sees the company's overview: the org chart, what the agents are doing,
spend and schedules — just not its tracker, its knowledge base or its chat.

**From a shell instead**, `crewlet iam invite` issues the same invitation
through the running node, authenticating with `CREWLET_API_TOKEN` like every
command that talks to one, and prints its link once. `-seat` is required —
every invitation names the seat its redemption binds — and `crewlet iam seats
-unheld` lists the human seats nothing holds yet:

```bash
export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"
crewlet iam invite you@example.com -seat founder -grants admin
```

`-grants admin` stores `admin` and every app beside it; `-grants` takes
`admin`, `worklet`, `doclet`, `artifacts`, `chatlet` or `none`, and left out
on an invitation it sends every app and not admin.

```
invitation 0192f4c8-…
http://localhost:8000/dashboard#/invite/0192f4c8-….kR3…

expires 2026-06-21 12:00
This link is shown once. Send it to them yourself; Crewlet doesn't send email.
```

## 5. Watch the first turn

It lands on **Home**, which is what a person opening this wants first: how the
company is (who is working, what waits on you, the work in progress and
finished, the tokens spent), the decisions only you can make — answered right
there, an option of an agent's question is a button — who is working now, and
what the company has done. With no company activity yet each card says so, and
a condition the engine itself raised takes over the sentence under the
greeting.

The sidebar reads top to bottom: Home, Inbox and My work; the Company group
(Overview, Policies, Org chart, Schedules, Integrations and Budgets); the
Workspace group (Worklet, Chatlet, Doclet, Artifacts and Live); and Settings at
its foot. Each workspace's sections are tabs in its page header, or Chatlet's
rail beside its rooms. `g` then a letter jumps between them, and `?` lists
every key.
While an agent is working, the Live row counts the seats at work and a
**Running** group under the sidebar's shortcuts (after Projects, Pinned and
Starred) lists up to four running turns. Press one to watch it — its
transcript, with the phase it is on open, or the coding run it is parked on
drawn live above its phases. `g r` does the same for the one running turn, and
opens Live's *Now running* when several are running (or none).

Within five minutes the `hello-crewlet` schedule fires a `TaskAssigned` at the
CEO. **Live** shows it: *Now running* has the seat working, and **Turns**
shows the turn as it runs — Execute, then Review, each phase listing the rounds
it took, the tools each round called, and the prompts the model actually saw.
A turn has those two phases: Execute both decides and acts, because the frame
that works out what to do is the frame that does it, and Review then judges
what the round actually landed. On a seat's very first turn one more runs
before them — Onboarding, where the agent reads its own team's pages once. A
phase that finishes updates in place rather than moving, so you can read one
while the next is running. [Turn Engine](../concepts/turn-engine.md) is what
each one does.

Follow the turn to **Worklet** and **Doclet**. Both are the engine's own:
`tracker.backend` defaults to `worklet` and `knowledge.backend` to `doclet`, so your company
has a tracker and a wiki from its first minute with nothing to set up. A seat
files with `create_work_item` and writes with `write_page`; a row opens the
item beside the list, and ⌘-click opens its page. Worklet opens as a **list**
(one line per item, which reads the same on your first task as on your five
hundredth), and **Filter** and **Display** in the bar decide what is on it and
how it is drawn: a board, a table, a calendar or a timeline over the same rows.
**Projects** beside it is the directory of what your units have declared.

**My work** and the **Inbox** answer for you — the person bound to the founder
seat — and every rule that asks "do you lead this" is asked about your seat. My work is one tab per claim
on somebody's attention — what they hold, the order somebody put it in, the
questions waiting on them and the ones they are waiting on — with every count
on the strip, and a band above it naming whose day is on screen. The **Queue**
is the work list narrowed to that person: the same Filter, Display and scope
controls, opening grouped by when each task is due; read in priority order, its
rows are reordered by dragging them. A question put to them is answered on its
row in **Asked of me**. Signed in with the API token instead, the dashboard says
so rather than guessing — a token bound to no seat is an ordinary state, not a
fault, and its day is the one kept under its own login. See [Humans in the Org
Chart](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api).

**Chatlet** is yours the same way, and only that way. Its rooms are the engine's
own — `chat.backend` defaults to `chatlet` — and every one of them is read and
spoken in as your seat, because a room shows each speaker by seat and its
members are seats: the units' channels above are already open, with their
seats in them. Signed in with the API token instead, Chatlet reads nothing at
all, not even a public room, because that credential acts as no seat. See
[Chatlet](../concepts/chatlet.md#people).

**⌘K (Ctrl+K elsewhere) searches everything and acts on it.** Type to find a
screen, a task, a page or a colleague — `#` narrows to tasks, `@` to agents,
`>` to actions — or paste an event, trace or turn id out of a log to open it.
Type a question of three words or more and pause, and a caller bound to a
seat — a signed-in person, who always holds one, or a directory-bound token —
gets a short answer written from your company's own pages and tasks, with its
sources and the tokens it spent, charged to the company's budget. It runs on
the auxiliary model of the seat you act as, so a token bound to no seat is
told so instead. From the same box you
can assign the task you found to an agent, ask an agent about what you typed
(the answer lands in your Inbox) or file it as a task — each made as you, under
your seat when you are signed in as yourself and under the token's own login
when you are signed in with one that no seat binds,
filed in the project on screen or your team's, and where neither says, in the
project you pick from the list it offers; an action the authority table would
refuse you says why on its row instead.

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

It gets the same tracker, knowledge-base and chat tools a seat holds — eighteen
over the tracker, five over the pages and nine in the chat — and twenty-three
more that no seat is given: the saved views, the catalogue write, a person's own
queue, inbox and pins, the trash, the board drag, the chat's furniture — making
a room, opening a conversation, a room's settings and membership, editing and
taking back a message, and how far you have read — and the gestures a person
makes about the company rather than its work — pausing and resuming a seat,
steering a running turn, answering a coding run's question and answering a
question from the company's knowledge.
Each is decided by the same authority table a seat's calls are, and knowledge
search sits beside them. On the deployment's token its writes carry the token's
own login, `token:founder`, with author kind `operator` — never an agent's — so
an audit can tell them from an agent's. The token cannot write as your seat: a
seat is held by one row of the identity directory, and yours is you. Which is
also why it is refused every chat tool, the reads included (`unseated`): a room
renders a speaker by seat and its membership is seats, so chat is spoken in as
somebody — your own token, below, does. See
[the operator surface](../reference/api-endpoints.md#operatormcp--your-own-assistant).

Once you sign in as a person, give the assistant **your own** token rather than
the deployment's. It acts as you — your seat, your permissions, its writes carrying
your seat's handle with author kind `human` — and every write it
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
That is also why the dashboard opened on its sign-in screen. Reads used
to serve without one, which meant anyone who could reach port 8000 could read
the LLM transcripts on `/events`; see
[Configuration § Auth](../concepts/configuration.md#auth) for what replaced
that posture and why it could not simply be defaulted the other way.

If you skipped the import, `crewlet run` boots in the **unconfigured** state
with the API still serving — you can then bootstrap live without restarting.
An unconfigured node is not an idle one: its identity estate runs from boot,
so you can sign in on it with the founder token before any company exists. You
cannot invite yourself yet — there is no seat to put you on, so an invitation
there answers `409 no_active_revision` — and the company comes first: import
or create it with your human seat in it, then invite yourself onto that seat
as above. Worklet's, Doclet's and Chatlet's routes
answer `503 no_active_revision` until the first revision arrives. That
revision brings up everything the company needs, Worklet, Doclet and Chatlet
included, with their projects, spaces and rooms, and the
[code sandbox](../concepts/code-sandbox.md) when it configures
`providers.sandbox` — under the same API, with nothing restarted:

```bash
export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"
crewlet config import company.yaml
```

The engine holds its store, so the command goes through the node's API: a
`PUT /config` of the whole file, which stores it as the company's first
revision and activates it fleet-wide — exactly what a script driving the API
directly sends ([Configure via the API](../guides/configure-via-api.md)).

Or create the company from the dashboard: open **Org chart** under Company
and its **Edit org** button (`#/company/org/edit`). With no configuration
active it opens on a form that starts the company from a template, has the
engine check it, and creates it with `PUT /config`. Keep its **Add a seat for
yourself** ticked:
that seat is the one you then invite yourself onto, from the org chart, as
step 4 describes. The builder reads and writes the company
document, so it needs a session holding `admin`: the one you signed in with
using `$CREWLET_API_TOKEN_FOUNDER` carries it, and a person you invite needs
it ticked to build the company — or to lead a unit, whose own team they may
edit without it. The dashboard adds no model provider — Settings › Models &
keys edits one the configuration already declares — so one step stays outside
it. Until it is done the company runs and no agent seat takes a turn; whatever
is sent to a seat waits on its inbox. The builder's next steps give it: with
`crewlet run` stopped, write the company to a file with `crewlet config
export`, add `providers.llm` to it, sign the CLI in with `crewlet llm login`
(a captured token goes into the fleet's secret store through the node's own
broker, which the command starts while the node is stopped), import the file
and start the node again
([Giving the company a model](../guides/org-builder.md#giving-the-company-a-model)),
and the work that waited runs.

## Split deployment (optional)

Run the agents as their own node when you want webhooks to keep arriving
while you restart them, or the agents on a host of their own. The API, the
duties and the company's data stay on the node you already have; the agents
move to a node that **holds no data** — it keeps nothing that has to outlive
it, and restarts in seconds whatever the company's size.

Two changes to the `crewlet.yaml` above make it the data node: it becomes a
member other nodes can join, and its leases become the fleet's rather than its
own.

```yaml
# crewlet.yaml — the data node, as above plus:
node:
  id: data-1
  roles: [data, ingress, workers]   # everything but the agents
stream:
  store_dir: "./acme-data/stream"
  leaf:
    host: 127.0.0.1
    port: 7422                      # where the agents node joins
coordination:
  type: embedded-kv                 # leases the agents node can see
```

The agents node gets a Tier A of its own. It needs no `company.yaml`, no API
port and no stream directory — it fetches the company from the data node, and
its store is deleted every time it starts:

```yaml
# agents.yaml — the agents node
node:
  id: agents-1
  roles: [seats]
store:
  path: "./acme-agents/agents.db"
  scratch: true
stream:
  leaf:
    urls: ["nats-leaf://127.0.0.1:7422"]
coordination:
  type: embedded-kv
# and the same `secrets:` block as crewlet.yaml, if you configured one:
# this node decrypts the company document and the secret store itself
```

```bash
crewlet run -config crewlet.yaml -api-port 8000   # terminal 1
crewlet run -config agents.yaml -api-port 0       # terminal 2
```

`-api-port 0` binds nothing on the agents node. Give it a port of its own
instead (`-api-port 8001`) when something should probe it: without the
`ingress` role it serves `/health` and `/ready` there and nothing else, and its
`/ready` turns `200` once it has joined the fleet and been admitted — see
[Probes on a node without ingress](../reference/api-endpoints.md#probes-on-a-node-without-ingress).

The agents node claims the seats once the data node answers that its copy of
the company's records is level with its logs, and its seats' tracker, knowledge
and chat tools read and write through it. See
[Running One Agent Somewhere Else](../guides/satellite-nodes.md) for what a
node without data can and cannot do.

The data node exposes the same REST endpoints, webhook handlers
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
crewlet config import company.yaml     # store the company; the run publishes it
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

- **You already have chat.** `chat.backend` defaults to the engine's own, so
  the units you gave a `channel` above have rooms in the dashboard right now,
  with their seats in them — and you are in them as your own seat once you
  have signed in (above): post in one and its team's agents wake to answer.
  What it does not do is reach you when you are not looking — the engine
  sends no email, no push and no SMS, so a mention becomes an unread count
  and a mention feed. See [Chatlet](../concepts/chatlet.md).
- Connect a **vendor** chat instead — Slack or Mattermost, both below — if
  your people already live in one, or if being notified while away matters.
  It is exclusive with the engine's own — one live chat home, or a person
  answers in the place the agents are not reading. Whichever you pick, first
  add the integration's block to `company.yaml` — the page's *Configure in
  YAML* section — with a `${VAR}` for each credential: the provisioner mints
  exactly the variables the file names. It records what it mints through the
  running node, as the founder token, and the `crewlet config import` that
  ends each snippet is what hands the engine both: the node took its company
  from the file once, at step 3, so an edit to the file reaches it only
  through an import, and an apply is when the engine reads its secret store.
  If your company already runs on [Slack](../integrations/slack.md), use it —
  the agents land where the conversations already happen, under the
  workspace admin and compliance setup you already have. It needs a public
  URL for its Events API and one OAuth **Allow** click per agent:
  ```bash
  export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"
  crewlet slack provision company.yaml -secret-store -public-url https://your-server.com
  crewlet config import company.yaml        # the block, with what was just minted
  ```
  To try a vendor chat on this machine first,
  [Mattermost](../integrations/mattermost.md) ships in this repo's
  `docker-compose.yml` and needs no account, no inbound URL and no clicks.
  Its block names the instance as `url: "${MATTERMOST_URL}"` and
  `team: nimbus`, the team the script creates:
  ```bash
  docker compose --profile mattermost up -d --wait
  scripts/mattermost-dev-bootstrap.sh       # writes MATTERMOST_* to .env
  set -a; . ./.env; set +a                  # the admin token the provisioner uses
  export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"
  printf %s "$MATTERMOST_URL" | crewlet secrets set MATTERMOST_URL
  crewlet mattermost provision company.yaml -secret-store
  crewlet config import company.yaml        # the block, with what was just minted
  ```
  The engine was started before `.env` existed, so `MATTERMOST_URL` reaches
  it through the secret store, beside the bots' tokens, rather than through
  its environment, which nothing changes while it runs.
  Running that stack on a **remote host** rather than this machine? Set
  `MATTERMOST_PUBLIC_URL` to the address your browser uses first — see
  [The Site URL](../integrations/mattermost.md#the-site-url). The engine
  needs no public URL; the Mattermost server still needs to know its own.
  The import that ends each snippet makes the file the **whole** running
  document, so it is the right last step only while `company.yaml` is what
  the node runs. A company created in the dashboard's org builder, or changed
  there since — a human seat added under **Org chart › Edit org** included — is
  replaced by the file, and the write is refused `409 seat_held` where the
  file drops a human seat somebody holds or is invited to. Connect chat from
  Company › Integrations then, as each page's *Setting it up from the
  dashboard* section describes
  ([Slack](../integrations/slack.md#setting-it-up-from-the-dashboard),
  [Mattermost](../integrations/mattermost.md#setting-it-up-from-the-dashboard)):
  it seals the credentials and re-activates the company as it stands.
- Connect a work-item tracker — [Jira](../integrations/jira.md), or the issue
  tracker of the code host you already run
- Connect a knowledge backend — [Confluence](../integrations/confluence.md) —
  so shared procedures surface in the executor's `## Relevant knowledge`
  block; publish version-controlled docs with `crewlet confluence import`
- Connect a code host — [GitLab](../integrations/gitlab.md) or
  [GitHub](../integrations/github.md) — and enable the
  [code sandbox](../concepts/code-sandbox.md) so engineer roles author real
  merge requests; on one host, run it in a container with
  [an image of your own](../concepts/code-sandbox.md#bring-your-own-image)
- Bring your team in: add a human seat for each person who is to sign in
  (**Org chart › Edit org › Add human seat**), then invite them onto it or create
  them on it from its card in **Company › Org chart**, every person holds a
  seat of their own. See [Putting a person in the
  seat](../concepts/humans-in-the-org.md#putting-a-person-in-the-seat)
- Fill in the [founder seat](../concepts/humans-in-the-org.md#the-founder-seat)
  you already have at the root — add the `contact` identities for each
  surface you connect (`mattermost_user_id`, `atlassian_account_id`,
  `gitlab_username`, …) so escalations land in your DMs and agents recognise
  your activity
- Your config is already encrypted at rest — the keyring every node holds seals
  the **entire** company document into the DB as one opaque blob on every
  write, and that seal is also what lets a peer trust a revision it fetches.
  See [Configuration § Secrets](../concepts/configuration.md#secrets)
- Stop exporting a variable per credential — with that keyring in place,
  `crewlet secrets set ANTHROPIC_API_KEY` puts the value in the encrypted
  [secret store](../concepts/secret-store.md), which the engine consults ahead
  of the process environment when resolving the `${...}` references you already
  wrote — and when an `api-key` entry reads its CLI's own key variable.
  Provisioners can write there directly (`crewlet gitlab provision …
  -secret-store`), so a minted credential reaches the engine with no file to
  source and no shell to be in
- Explore the [Nimbus example](../../examples/) — a seven-seat company on
  Mattermost, running on Claude Code with your Claude plan rather than an API
  key (`nimbus-claude-cli`), its engineering seats running that CLI as their own
  agentic loop.
  It is the next step up from this page and still needs nothing but the chat
  server in this repo's compose file; every integration above adds on top of
  it
- Add [MCP servers](../guides/tools-and-mcp.md#extending-the-engine) so agents can reach your own systems
- See the full [configuration reference](configuration.md) for all YAML options
