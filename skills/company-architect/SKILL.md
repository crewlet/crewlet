---
name: company-architect
description: >
  Interview a founder and author their Crewlet company configuration —
  the Tier B company.yaml org chart (mission, policies, units, seats,
  schedules, integrations) and the Tier A crewlet.yaml bootstrap. Use
  when someone wants to set up, design, extend, or restructure a Crewlet
  AI agent company, add or remove a seat or team, wire in an integration
  (Jira, Confluence, Mattermost, Slack, GitLab, GitHub, the code
  sandbox), or
  when they hand you a company.yaml to review, fix, or explain.
---

# Authoring a Crewlet company

You are helping a founder describe a company that Crewlet will then run:
one persistent agent per seat, working on real external surfaces. The
deliverable is two YAML files plus a `.env`.

Read this whole file before writing any YAML. It is short on purpose —
the reference material lives behind the links, and there is a validator
that will catch what you get wrong.

## The one thing that makes this work

**Never hand-write config from memory, and never guess a field name.**
Crewlet's config layer forbids unknown keys at every level, so an
invented field is a hard error rather than a silent no-op. The
authoritative field list is a JSON Schema generated from the code
itself:

- Tier B (`company.yaml`) — <https://docs.crewlet.ai/engine/latest/schema/company.schema.json>
- Tier A (`crewlet.yaml`) — <https://docs.crewlet.ai/engine/latest/schema/bootstrap.schema.json>

**Fetch the Tier B schema before you write any YAML.** It carries every
field name, type, and enum, plus `additionalProperties: false`
throughout. If you cannot fetch it, say so and stop — do not proceed
from memory.

Those URLs are the current release's schema. When `crewlet` is
installed, `crewlet schema company` (and `crewlet schema bootstrap`)
prints the schema of the binary that will run the config, which is the
one to trust when the two differ — a binary older or newer than the
current release knows a different field list.

### The loop: write → validate → fix

Run it after **every** edit, and don't report success until it passes.
Use the best rung available to you:

**1. `crewlet` is installed** (best — full fidelity):

```bash
crewlet validate company.yaml --json
```

Returns `{"valid": …, "tier": …, "problems": [{"path", "segments", "kind", "message", "seat", "unit", "line"}], "warnings": [{"kind", "ref", "path", "segments", "seat", "unit", "from", "to", "message"}], "summary": {…}}`:
every offending field with its exact path, all at once. Fix every problem's
`path` and re-run. `segments` is the same path as keys and indexes (read it
rather than splitting `path`, since a map key can contain a dot), `kind` is
one of `missing`, `out_of_range`, `conflict`, `shape`, `unknown_field`,
`unknown_value` or `invalid`, and `seat`, `unit` and `line` are present when
the problem is about a seat, a unit, or a line the parser pointed at. Two
seats sharing a name are one problem beside each seat. A **warning** never
fails validation, but it is a reference that resolves to nothing (a `lead`,
a `unit:`, a `manages` entry, a GitLab access level — each a handle or a
key that matches nothing): fix every one before you
report the company finished, since nothing else will ever point at it.

**2. No `crewlet`, but you can run code** (catches nearly everything):
validate the parsed YAML against the fetched schema with any JSON Schema
validator — `jsonschema` in Python, `ajv` in Node. The schema is Draft
2020-12.

**3. No tooling at all** (weakest): check the document against the
schema by reading it — field by field, including that no key is absent
from `properties`.

Whichever rung you used, then walk the residual checklist below.

None of this needs **credentials, a database, or a network** (beyond
fetching the schema once): Tier B stores `${VAR}` references verbatim
and resolves each only when the engine builds the provider or transport
that uses it, so a complete config validates before a single secret
exists. That is also why a reference is judged as the literal text it is:
write one only where a value is free text or a credential, never in an
enum, a handle or a unit id (the Mattermost `username` is the one
patterned field that takes one whole reference).

**Say which rung you used.** If you validated by reading rather than
running a validator, tell the founder that plainly — never imply a check
you didn't run.

### What the schema does *not* catch

These survive a clean schema pass, so check them yourself on every
config — rungs 2 and 3 especially:

- **`lead` is the HANDLE of a seat that exists.** A unit whose `lead`
  doesn't match any seat's handle silently gets no lead (it's only a
  warning even in `crewlet validate`, because roles can be added
  incrementally). A display name there resolves to nobody.
- **Every `manages` entry is a real seat handle or unit key.** Same class
  of silent miss.
- **A human seat with no `contact` identity is legal and worth a look.** It
  passes the schema and `crewlet validate` alike — a person who works only
  through the dashboard has no chat account to declare — but `crewlet
  validate` prints an advisory, because nothing addressed to that seat on a
  chat surface reaches anybody. Give it one for the surface the company runs
  unless that is the intent.
- **Every `knowledge.*` scope list names a backend the config actually
  configures.** A scope for a backend with no integration block narrows
  nothing, and `crewlet validate` refuses it.
- **Timezones are real IANA names** (`Europe/Amsterdam`, not `CET` or
  `Mars/Olympus`) — the company's `timezone` and any schedule's own —
  and never `Local` or `localtime`, which the validator refuses because
  each is whatever zone the host reading it happens to be set to. There
  is ONE company clock, the top-level `timezone`: no block under
  `tracker:` or `scheduling:` takes a zone of its own.
- **An LLM entry names its CLI's model in that CLI's grammar.** The schema
  sees a string; `crewlet validate` refuses an `opencode` model with no
  `<provider>/` and a credential the entry's CLI would never read (an
  `auth.api_key` outside `auth.mode: api-key`, an `auth.token` on any CLI but
  `claude-code`, an `api-key` entry on `opencode`, `hermes` or `pi`, whose keys
  go in `env`). Nothing validates the host: whether the CLI is installed and
  signed in is `crewlet llm doctor`'s to say.
- **Cron expressions are valid.** The schema takes a cron as plain text,
  so it checks neither the field count nor the values — `99 * * * *` and
  `0 9 * *` both pass it. `crewlet validate` is what checks one.
- **Everything under "Invariants"** below. Those are judgement calls; no
  validator will ever catch them.

## Interview before you write

Do not open with a YAML dump. Ask, in this order, and stop as soon as
you have enough for a first running company — the config is
live-editable, so the first version does not need to be the last.

1. **What does the company do, and where?** One line of mission. This
   becomes `name` / `mission` / `vision` and lands in every agent's
   prompt. *Where* is the company's `timezone` — its ONE clock, an IANA
   name like `Europe/Berlin` at the top of the document (UTC when
   absent). Every "today", "this week" and overdue mark is cut on it, and
   so is the hour of every schedule that does not name a zone of its
   own, so a 09:00 standup is 09:00 there. Ask; a company whose people
   are not on UTC wants it set.
2. **Who is on it?** Walk the org chart aloud: what teams, what seats,
   who leads what, who reports to whom. Names, one-line goals, and a
   sentence of backstory each. This is 90% of the config.
3. **Where does the founder sit?** They should be *in* the chart — see
   the founder seat under invariants. And who else will sign in? Every
   person who uses the dashboard holds a `kind: human` seat of their own,
   from the moment they join until they are removed — they are invited or
   created *onto* it — so each human teammate needs one too, and a
   company with no human seat can admit nobody.
4. **What surfaces does the work live on?** Chat and the code host are
   the ones to ask about; the tracker and the knowledge base **ship with
   the engine and are on by default**, so a founder needs a positive
   reason to bring Jira and Confluence rather than a reason not to. The
   reason is usually about their people — a team already living in Jira
   should not be asked to watch a second board — and never about the
   agents, who cannot tell the difference. Say so, then point them at
   [Choosing your stack](https://docs.crewlet.ai/engine/latest/getting-started/choosing-your-stack/)
   rather than re-deriving the trade-offs. **Do not wire integrations in
   the first pass** — see the sequencing rule below.

   Give each unit a `project` and a `space` in the first pass regardless:
   on the default backends that is the whole setup, and it is what gives
   a team somewhere to file work and write things down: Doclet
   creates a page only in a declared space (a unit's or a
   seat's `space`, `knowledge.root_space`, or the tool-skills container),
   exactly as Worklet files work only in a declared project.
5. **What model, and what budget?** One `providers.llm` entry is enough
   to start. Every model is reached through the vendor's own **coding
   CLI**, which the engine drives as a process, so ask *whose* models and
   *how they pay*, then write the entry for that CLI:

   - **Anthropic's, on a Claude Pro / Max plan** — `model: sonnet` (Claude
     Code is the default `agent`). No key; the founder signs the CLI in once
     on the engine host with `crewlet llm login <key> -from-host` (or
     `-capture-token`).
   - **Anthropic's, on an API key** — `model: sonnet` plus
     `auth: {mode: api-key}`, which reads `ANTHROPIC_API_KEY` from the secret
     store or the environment. Name another variable with
     `auth.api_key: "${VAR}"` only if theirs is called something else.
   - **Open models** (OpenRouter, a hosted provider, a local server) —
     `agent: opencode`, `model: <provider>/<model>` (a bare id is refused), and
     the provider's key in `env` under that provider's own variable, e.g.
     `env: {OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"}`.
   - **OpenAI's** — `agent: codex`, `model: gpt-5`, and the same `auth`, which
     reads an OpenAI API key from `CODEX_API_KEY`, the variable codex signs in
     with; leave `auth` out for a ChatGPT login. **Not on its own:** every
     agent seat's executor is its coding CLI's own run, which only
     `claude-code` and `opencode` can be, so a company on Codex alone is
     refused. Codex (and every other CLI) serves a seat's reviewer, workers
     and side passes through `llm_review`, `llm_subagent`, `llm_auxiliary` or
     `llm_judge`, beside a Claude Code or OpenCode entry the seat's `llm`
     names.

   **Write `providers.sandbox` with the model**, because every agent seat's
   executor runs in a box from it and a company with an agent seat and a
   model is refused without one. For a local instance, the founder's own
   machine included, recommend `container`: a `local.image` carrying the CLI
   (an image of their own, or a stock one plus a setup step that installs it)
   and `default_run_in: container`. `sandbox: {local: {}, default_run_in:
   direct}` is the shortest start and runs it as the engine's user — a fully
   permissioned CLI on that host, with whatever that account can read — so
   keep it for a dedicated host the founder is willing to hand to an
   autonomous agent, the entry that needs the host itself, or a first run on
   their own machine while its seats take work from them alone, and say that
   the runs move into `container` before a seat takes work from anyone else;
   `e2b` keeps the run off the host altogether. See
   [Code sandbox](https://docs.crewlet.ai/engine/latest/concepts/code-sandbox/).

   Whichever it is, the CLI must be installed on the machine running
   `crewlet run`, and `crewlet llm doctor` is what proves the entry works —
   tell the founder to run it, and where: with the engine stopped it reaches
   the fleet's secret store itself, through the node's own broker; while the
   engine runs it reads the company's values through it, so it is run beside
   it with `CREWLET_API_TOKEN` set to one of the node's Tier A tokens (or with
   `-api URL` naming a running node). A member of a cluster whose engine is
   stopped cannot reach the store alone, so the doctor is refused there,
   naming `-api`. If they want a plan with a key behind it, that is two
   entries on the same CLI and a list: `llm: [plan, key]`. A cheap, fast entry
   for `llm_auxiliary` is worth suggesting once the company grows: every
   auxiliary call pays a CLI launch under a fixed deadline. See
   [LLM Backends](https://docs.crewlet.ai/engine/latest/concepts/llm-backends/).

   Ask whether they want a cost ceiling, and on which window:
   `token_budget` is a mapping of `day`, `week` and `month` ceilings on the
   company's clock, each optional — a month for the bill, a day as the fuse
   that stops a runaway loop — e.g. `token_budget: {day: 3000000, month:
   40000000}`. Never write `0` for "no ceiling" (it is refused); leave the
   key out.

If a founder gives you a vague answer ("a few engineers"), propose a
concrete chart and let them correct it. Concrete beats complete.

## Sequence: get a turn on screen first

A company with no integrations only reacts to schedules. That is the
right first milestone, because it proves the engine, the model, and the
config all work before any external credential is in play.

It is not a hollow one: Worklet and Doclet are on by
default, so that first turn can already file work and write pages, and
the founder can watch it happen in the dashboard's Worklet and Doclet
workspaces. The integrations that come later are about where their PEOPLE
are, not about what the company can do.

**First pass** — no `integrations`, no `mcp_servers`, one LLM provider,
the org chart with a `project` and `space` per unit, and one schedule on
the top seat so a turn actually fires:

```yaml
roles:
  - name: CEO
    goal: "Set product direction and make final calls"
    schedules:
      - name: hello-crewlet
        cron: "*/5 * * * *"
        task: "Write a short note on what the company should focus on this week."
```

Tell them to delete that schedule once real work arrives. Then layer
integrations one at a time, validating after each.

Full worked examples to model the shape on — read them, don't
reinvent them:

- [Quickstart](https://docs.crewlet.ai/engine/latest/getting-started/quickstart/) — four seats,
  zero integrations, the minimal end-to-end config.
- [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) —
  a complete seven-seat company with GitLab + Mattermost + sandbox on
  Worklet and Doclet, its model open models through
  OpenCode on an OpenRouter key.
- [`examples/nimbus-claude-cli.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus-claude-cli.company.yaml) —
  the SAME seven seats with everything but chat removed: Mattermost alone, on
  Claude Code with the founder's Claude plan rather than a metered key. The one
  to copy when a founder has no tracker, wiki or code host yet.

## Invariants

Get these wrong and the fix is expensive or lossy. The schema cannot
catch most of them.

**The company `name` and every handle are permanent.** An agent's durable
id is `uuid5(ns, f"{org.name}:{handle}")`, and a document that gives a seat
a different handle removes that seat and adds a new one with an empty
mailbox and no memory. Every write stores the handle a seat leaves out —
derived from its name — so correcting a seat's `name` by editing the STORED
company (the org builder, `PATCH /config`, the per-entity routes) keeps its
handle, its mailbox, its lease, its diary, its onboarding markers and its
scheduled work. A FILE is minted afresh on every import: correct a `name` in
a file that leaves the `handle` out, import it, and the seat is replaced —
an agent seat with no warning. So every file you write declares `handle`
on every seat and `id` on every unit. Changing the company `name` mints a
new id for every agent and orphans every one of those things — settle it
before the company runs, and warn the founder explicitly if they later ask
to change it.

**Nothing is referenced by a display name.** A `lead` and a `manages`
entry name a seat by its **handle**; a `manages` entry and a root seat's
`unit:` name a unit by its **key** (its `id`, or its name where it
declares none). A `name` on a seat or a unit is prose a founder edits, so
writing one into a reference is how a chart comes apart on the first
rename. Write handles and keys, and set both explicitly.

**Every unit needs an `id`, and it is minted from the name if you leave
it out — afresh on every import of a file.** The id is the unit's key —
lowercase letters, digits, `-` and `_`, starting with a letter — chosen
once and read by nobody, so it can be short and dull (`engineering`,
`platform`). Write it yourself, on every unit, so renaming a team in the
file moves nothing.

**Handles and unit keys are unique across the whole company; names are
not.** Each reference resolves to the first match anywhere in the tree, so
two units answering to one key silently share one team's work, routing and
pages, and two seats on one handle share one inbox — `crewlet validate`
refuses both, and so does every write. A `name` is prose and may repeat:
two seats called `Software Engineer` are fine on two handles, but two that
declare no `handle` derive the same one and are refused for that. Prefer
names a reader can tell apart anyway (`Software Engineer 2`), since a
colleague asked for by a shared name is offered every seat that carries it,
and give every seat an explicit `handle` and every unit its own `id`.

**A unit's `id` is what work filed into that team is keyed on.** A unit's
name is prose and gets renamed; its `id` (lowercase, starting with a letter,
e.g. `id: eng`) is chosen once and read by nobody, so renaming the team moves
nothing while the `id` stays. Two units on one key are refused, a minted key
included: a unit that leaves `id` out collides with any unit whose `id` is
the key its name mints.

**Secrets are `${VAR}` references, never literals.** Every string field
supports `${ENV_VAR}`. Put the reference in the YAML and the value in
`.env`. Never write a token, key, or webhook secret into a config file,
even a draft, even one you expect to be deleted. The schema says where
they go: every position that holds a credential carries
`"x-crewlet-secret": true` (on a list's `items` and a map's
`additionalProperties` for the members), and a credential in one of
them is a `${VAR}`. Some of them are maps that carry settings beside the
credentials — a tool server's `env`, a seat's `mcp_env` — and a setting
there that is no secret is written as itself: `GITLAB_HOST: gitlab.com`,
`GLAB_CHECK_UPDATE: "false"`. A read of the stored company masks it, as it
masks everything in such a position that is not exactly one reference,
and writing the masked value back keeps the stored one.

**Seats you will provision need a *whole-value* `${VAR}`.**
`crewlet mattermost provision` / `crewlet gitlab provision` mint per-agent
tokens *into the config's own references*, and that capture contract
requires the value to be exactly one reference —
`"${MATTERMOST_TOKEN_CEO}"`, never `"tok-${SUFFIX}"` or a literal. One
distinct variable per seat. Atlassian and GitHub mint nothing: create
those accounts and tokens by hand, and their commands report which
account each credential turned out to be.

**The knowledge base and the tracker are each single-homed, and each is
named.** `knowledge.backend` is `doclet` (the default), `confluence` or
`none`; `tracker.backend` is `worklet` (the default), `jira` or `none`. Two knowledge
bases would make an agent's answer to "what do we already know about
this" depend on which was asked, so naming `doclet` beside an
`integrations.confluence` block is refused, and the same for the
tracker. Leaving a backend unset **derives** it from whether the vendor
block is there, so an existing config keeps what it had. The two axes are
independent: Worklet against a Confluence wiki is ordinary. Put
the org-wide read scope in `knowledge.scope`, and note a scope on a
backend switched off is refused, because it reads as a working narrowing
and narrows nothing.

**`project` / `space` on a unit or root role is identity, not read scope,
and not a credential.** It means "this team's home": where inbound
activity with no better recipient routes, and where the team files work
and writes pages. It does **not** widen what agents can read (only
`knowledge.scope` does) and it is **not** an MCP credential (those live
in `role.mcp_env`). This is the single most-confused part of the config —
get it right and say which one you mean. The keys are **vendor-neutral**
by design: the same `project: ENG` names a Worklet project and a Jira one,
so switching backends never rewrites the org chart. Shape them as an
upper-case letter plus 1–9 upper-case letters or digits (`ENG`, `PROD`),
which is what every backend accepts, and never use the reserved `TS` or
`HOME`.

**`mcp_env` is per-agent tool credentials, keyed by MCP server name** —
env vars for `stdio` servers, HTTP headers for `http` ones. A unit's
`mcp_env` is inherited by its direct agent roles, with the role's own value
winning per key. Human seats inherit none and may not declare one.

**`manages` accepts unit keys as well as seat handles.** A unit key
expands to every seat in it and its descendants. If one token is both a
handle and a key, the seat wins. A unit lead auto-manages every direct
member that no other direct member of that unit already manages (a unit
key in a member's `manages` counts for every seat it reaches), and a
child unit with no `lead` inherits its parent's.

**Put the founder in the chart.** A human seat at the root, above the
top agent, so escalation terminates at a person and agents recognise
their activity in chat / the tracker / the code host. It is not optional:
it is also the seat the founder **joins on**, since every person who signs
in holds exactly one human seat and is put onto it rather than given one
later:

```yaml
roles:
  - name: Jane Founder
    handle: jane-founder    # declared: an import mints a missing one from the name
    kind: human
    manages: [ceo]          # the CEO seat's handle, never its display name
    contact: { mattermost_user_id: "${MATTERMOST_FOUNDER_USERNAME}" }
```

`kind: human` seats are addressable but never spawned: no runtime, no
inbox, no LLM. They reject the runtime-only fields, and `contact` is
optional: a person with none works through the dashboard only, agents
cannot @-mention them, and `crewlet validate` warns about the seat at its
`contact`. Scope their `manages` to the top roles — a
founder managing every seat floods them. Give every other person who will
sign in a `kind: human` seat the same way, where they sit in the chart. See
[Humans in the org chart](https://docs.crewlet.ai/engine/latest/concepts/humans-in-the-org/).

**Every agent seat writes code in its own run.** Its executor is its coding
CLI's own run (`claude-code` or `opencode`) with a shell and git, in a box
from `providers.sandbox`, which the company must configure once it has an
agent seat and a model; reading and reviewing code on a code host is MCP.
`role.sandbox` adds what one seat's box reads: `setup` steps, `env` (its
code-host token), `optional_keys`, `mcp`, `pause_ttl_seconds` and
`max_turns`. See
[Code sandbox](https://docs.crewlet.ai/engine/latest/concepts/code-sandbox/).

**The engine ships no tool-skill prose.** If they want agents to know
*how* to use a tool, that is a knowledge-base page published with
`crewlet confluence import`, not prompt text in
the config. See [Tool skills](https://docs.crewlet.ai/engine/latest/concepts/tool-skills/).

**`workers:` are helpers, not seats.** A worker template is a
short-lived delegate a seat's executor hands narrowly-scoped work to
with `delegate` — it runs inside that seat's turn, has no inbox, no
memory and no identity, cannot write anywhere, and reports back to the
seat that called it. Reach for one when a seat repeatedly needs the same
bounded, read-shaped work done (research a question, summarise a long
page, check three repositories at once). Reach for a **seat** when the
work has an owner, needs to be addressable by a person, or has to write
somewhere.

```yaml
workers:
  researcher:
    description: reads the team's pages and reports what they say, with sources
    system_prompt: |
      You research a narrow question against the team's knowledge base.
      Report only what you can point at.
    # On the default backends these are the engine's own. Swap them for
    # the vendor's MCP tool names if the company runs Confluence.
    tools: [search_knowledge, get_page, list_pages]
    output:
      type: object
      properties:
        findings: {type: string}
        sources:  {type: array, items: {type: string}}
      required: [findings]
```

Two things to get right. **`description` is written for the model
choosing a worker**, not for the operator reading the file — it is the
only part of the template the executor sees. And **naming a tool grants
nothing**: every name still passes the same filter as any delegation, so
a template can never hand a seat a tool the seat does not already have.
Declare `output` where the parent will *act* on the answer — fields it
can index beat prose it has to re-read. Leave `roles[].workers` off
unless a seat should see a narrower set; empty means every template. See
[Turn engine — workers](https://docs.crewlet.ai/engine/latest/concepts/turn-engine/#workers).

## Writing style for the fields that become prompts

`mission`, `policies`, `goal`, `backstory`, `responsibilities`, and
`behavioral_guidelines` are rendered verbatim into agent prompts. Write
them as you would a real role description:

- **`goal`** — one sentence, an outcome the seat owns.
- **`backstory`** — the seat's judgement and expertise, not a résumé.
  It is what makes two engineer seats behave differently.
- **`policies`** — org-wide rules, in the imperative, that an agent
  could actually violate. Vague values ("be excellent") cost tokens on
  every turn and change nothing.
- **`responsibilities`** — the recurring work of the seat.

Distinct seats need distinct text. Five agents with the same backstory
are five copies of one agent with five times the bill.

## Deliverables

When you finish, the founder should have:

1. `company.yaml` — Tier B, validated, `${VAR}` for every secret.
2. `crewlet.yaml` — Tier A: the store's file path (two local files this
   process owns, the node's own and the replicated estate beside it — there
   is no DSN), the stream slot, `coordination.type`, the API host/port, and
   the three things a node serving the API refuses to start without:
   `api.external_url` (where a browser reaches it — `http://localhost:8000`
   on one machine, the public address behind a proxy), `api.auth.admin`
   and at least one auth token. Every node also needs the `secrets:`
   keyring — `active_key_id` and a key whose `material:` is a `${VAR}` —
   because every record it writes is signed under it; `crewlet secrets
   keygen -key-id <id>` mints a key and prints the snippet and the export
   line. Leave the stream `type: embedded` with a `store_dir` and
   `coordination.type: local`: that is one node with no broker to operate
   and no service to point anything at, and it is what every company
   starts as. Step 1 of the
   [quickstart](https://docs.crewlet.ai/engine/latest/getting-started/quickstart/)
   writes a complete one. Validate it too (`crewlet validate crewlet.yaml`).
3. `.env` — every `${VAR}` the two files reference, with real values,
   the keyring's key among them. List them explicitly; a missing one
   keeps a tool server that reads it from starting, is left out of an
   executor run (which starts anyway and logs `sandbox_env_unresolved`),
   and anywhere else can resolve to an empty string that fails later,
   deep in a turn.
4. The run command, and what they should expect to see:

```bash
crewlet validate company.yaml
crewlet run crewlet.yaml -company company.yaml
# dashboard on the configured api.port
```

5. How they join it. Nobody holds a password until they are put on a
   seat, and the API token is what puts the first person on theirs — so
   its `grants` must list `admin` (`grants: [admin]`), which runs the
   company and includes every app, and the node's `api.auth.admin` must be
   `serve`. Sign in at the dashboard with the API token, open **Company › Org
   chart**, open the founder's seat (drawn **Vacant**) and press
   **Invite**, then open the link it shows once and choose a login and a
   password — or, from a shell, `crewlet iam invite <address> -seat
   jane-founder -grants admin` with `CREWLET_API_TOKEN` set. Every teammate
   after them is invited or created from their own vacant seat the same
   way, by an administrator, with every app (Worklet, Doclet, Artifacts,
   Chatlet) ticked by default, and Admin only for the people who run the company. See [How the first person
   exists](https://docs.crewlet.ai/engine/latest/concepts/identity-and-access/#how-the-first-person-exists).

Full field reference:
[Configuration](https://docs.crewlet.ai/engine/latest/getting-started/configuration/). Two-tier
design, live reload, and secrets-at-rest:
[Configuration concepts](https://docs.crewlet.ai/engine/latest/concepts/configuration/).

Add this line at the top of each file so their editor autocompletes and
flags typos as they type:

```yaml
# yaml-language-server: $schema=https://docs.crewlet.ai/engine/latest/schema/company.schema.json
```
