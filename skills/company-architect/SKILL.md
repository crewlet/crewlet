---
name: company-architect
description: >
  Interview a founder and author their Crewlet company configuration —
  the Tier B company.yaml org chart (mission, policies, units, seats,
  schedules, integrations) and the Tier A config.yaml bootstrap. Use
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

- Tier B (`company.yaml`) — <https://docs.crewlet.ai/schema/company.schema.json>
- Tier A (`config.yaml`) — <https://docs.crewlet.ai/schema/bootstrap.schema.json>

**Fetch the Tier B schema before you write any YAML.** It carries every
field name, type, and enum, plus `additionalProperties: false`
throughout. If you cannot fetch it, say so and stop — do not proceed
from memory.

### The loop: write → validate → fix

Run it after **every** edit, and don't report success until it passes.
Use the best rung available to you:

**1. `crewlet` is installed** (best — full fidelity):

```bash
crewlet validate company.yaml --json
```

Returns `{"valid": …, "tier": …, "errors": [{"path", "message", "type"}], "summary": {…}}`
— every offending field with its exact path, all at once. Fix every
`path` and re-run.

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
and resolves them at engine start, so a complete config validates before
a single secret exists.

**Say which rung you used.** If you validated by reading rather than
running a validator, tell the founder that plainly — never imply a check
you didn't run.

### What the schema does *not* catch

These survive a clean schema pass, so check them yourself on every
config — rungs 2 and 3 especially:

- **`lead` names a role that exists.** A unit whose `lead` doesn't match
  any role name silently gets no lead (it's only a warning even in
  `crewlet validate`, because roles can be added incrementally).
- **Every `manages` entry names a real role or unit.** Same class of
  silent miss.
- **Timezones are real IANA names** (`Europe/Amsterdam`, not `CET` or
  `Mars/Olympus`).
- **Cron expressions are semantically valid.** The schema only checks
  there are five fields — `99 * * * *` has the right shape and is still
  nonsense.
- **Everything under "Invariants"** below. Those are judgement calls; no
  validator will ever catch them.

## Interview before you write

Do not open with a YAML dump. Ask, in this order, and stop as soon as
you have enough for a first running company — the config is
live-editable, so the first version does not need to be the last.

1. **What does the company do?** One line of mission. This becomes
   `name` / `mission` / `vision` and lands in every agent's prompt.
2. **Who is on it?** Walk the org chart aloud: what teams, what seats,
   who leads what, who reports to whom. Names, one-line goals, and a
   sentence of backstory each. This is 90% of the config.
3. **Where does the founder sit?** They should be *in* the chart — see
   the founder seat under invariants.
4. **What surfaces does the work live on?** Chat and the code host are
   the ones to ask about; the tracker and the knowledge base **ship with
   the engine and are on by default**, so a founder needs a positive
   reason to bring Jira and Confluence rather than a reason not to. The
   reason is usually about their people — a team already living in Jira
   should not be asked to watch a second board — and never about the
   agents, who cannot tell the difference. Say so, then point them at
   [Choosing your stack](https://docs.crewlet.ai/getting-started/choosing-your-stack)
   rather than re-deriving the trade-offs. **Do not wire integrations in
   the first pass** — see the sequencing rule below.

   Give each unit a `project` and a `space` in the first pass regardless:
   on the default backends that is the whole setup, and it is what gives
   a team somewhere to file work and write things down.
5. **What model, and what budget?** One `providers.llm` entry is enough
   to start. Ask whether they want a cost ceiling (`token_budget`).

If a founder gives you a vague answer ("a few engineers"), propose a
concrete chart and let them correct it. Concrete beats complete.

## Sequence: get a turn on screen first

A company with no integrations only reacts to schedules. That is the
right first milestone, because it proves the engine, the model, and the
config all work before any external credential is in play.

It is not a hollow one: the tracker and the knowledge base are on by
default, so that first turn can already file work and write pages, and
the founder can watch it happen on the Work board and Pages screens. The
integrations that come later are about where their PEOPLE are, not about
what the company can do.

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

- [Quickstart](https://docs.crewlet.ai/getting-started/quickstart) — four seats,
  zero integrations, the minimal end-to-end config.
- [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) —
  a complete seven-seat company with Jira + Confluence + GitLab + Mattermost
  + sandbox.

## Invariants

Get these wrong and the fix is expensive or lossy. The schema cannot
catch most of them.

**Handles are permanent.** An agent's durable id is
`uuid5(ns, f"{org.name}:{handle}")`. Changing a seat's `handle` — or the
company `name` — mints a new id and orphans that agent's diary,
onboarding markers, and counterparty profiles. Its memory is gone.
Settle `name` and each `handle` before the company runs, and warn the
founder explicitly if they later ask to rename either.

**Secrets are `${VAR}` references, never literals.** Every string field
supports `${ENV_VAR}`. Put the reference in the YAML and the value in
`.env`. Never write a token, key, or webhook secret into a config file,
even a draft, even one you expect to be deleted.

**Seats you will provision need a *whole-value* `${VAR}`.**
`crewlet mattermost provision` / `crewlet gitlab provision` mint per-agent
tokens *into the config's own references*, and that capture contract
requires the value to be exactly one reference —
`"${MATTERMOST_TOKEN_CEO}"`, never `"tok-${SUFFIX}"` or a literal. One
distinct variable per seat. Atlassian and GitHub mint nothing: create
those accounts and tokens by hand, and their commands report which
account each credential turned out to be.

**The knowledge base and the tracker are each single-homed, and each is
named.** `knowledge.backend` is `native` (the default), `confluence` or
`none`; `tracker.backend` is `native`, `jira` or `none`. Two knowledge
bases would make an agent's answer to "what do we already know about
this" depend on which was asked, so naming `native` beside an
`integrations.confluence` block is refused — and the same for the
tracker. Leaving a backend unset **derives** it from whether the vendor
block is there, so an existing config keeps what it had. The two axes are
independent: a native tracker against a Confluence wiki is ordinary. Put
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
by design: the same `project: ENG` names a native project and a Jira one,
so switching backends never rewrites the org chart. Shape them as an
upper-case letter plus 1–9 upper-case letters or digits (`ENG`, `PROD`),
which is what every backend accepts, and never use the reserved `TS` or
`HOME`.

**`mcp_env` is per-agent tool credentials, keyed by MCP server name** —
env vars for `stdio` servers, HTTP headers for `http` ones. A unit's
`mcp_env` is inherited by its direct roles, with the role's own value
winning per key.

**`manages` accepts unit names as well as role names.** A unit name
expands to every role in it and its descendants. If a name matches both,
the role wins. Unit leads auto-manage otherwise-unmanaged direct
members, and a child unit with no `lead` inherits its parent's.

**Put the founder in the chart.** A human seat at the root, above the
top agent, so escalation terminates at a person and agents recognise
their activity in chat / the tracker / the code host:

```yaml
roles:
  - name: Jane Founder
    kind: human
    manages: [CEO]
    contact: { mattermost_user_id: "${MATTERMOST_FOUNDER_USERNAME}" }
```

`kind: human` seats are addressable but never spawned: no runtime, no
inbox, no LLM. They require at least one `contact` identity and reject
the runtime-only fields. Scope their `manages` to the top roles — a
founder managing every seat floods them. See
[Humans in the org chart](https://docs.crewlet.ai/concepts/humans-in-the-org).

**Agents do not get code-authoring tools by default.** Reading and
reviewing code is MCP; *writing* it is the sandbox (`role.sandbox`),
which needs `providers.sandbox` configured. See
[Code sandbox](https://docs.crewlet.ai/concepts/code-sandbox).

**The engine ships no tool-skill prose.** If they want agents to know
*how* to use a tool, that is a knowledge-base page published with
`crewlet confluence import`, not prompt text in
the config. See [Tool skills](https://docs.crewlet.ai/concepts/tool-skills).

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
[Turn engine — workers](https://docs.crewlet.ai/concepts/turn-engine#workers).

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
2. `config.yaml` — Tier A: the store's file path (one local file this
   process owns — there is no DSN), the stream slot, `coordination.type`,
   and the API host/port with at least one auth token. Leave the stream
   `type: embedded` with a `store_dir` and `coordination.type: local`:
   that is one node with no broker to operate and no service to point
   anything at, and it is what every company starts as. Validate it too
   (`crewlet validate config.yaml`).
3. `.env` — every `${VAR}` the two files reference, with real values.
   List them explicitly; a missing one resolves to an empty string and
   fails later, deep in a turn.
4. The run command, and what they should expect to see:

```bash
crewlet validate company.yaml
crewlet run config.yaml -company company.yaml
# dashboard on the configured api.port
```

Full field reference:
[Configuration](https://docs.crewlet.ai/getting-started/configuration). Two-tier
design, live reload, and secrets-at-rest:
[Configuration concepts](https://docs.crewlet.ai/concepts/configuration).

Add this line at the top of each file so their editor autocompletes and
flags typos as they type:

```yaml
# yaml-language-server: $schema=https://docs.crewlet.ai/schema/company.schema.json
```
