# Authoring Your Company with an AI Assistant

A Crewlet company is described in YAML: a mission, an org chart, seats
with backstories, policies, integrations. That is a lot of surface to
write from a blank file — so Crewlet ships the pieces an AI assistant
needs to write it *with* you and check its own work.

Three things make this work, and you can use any of them on their own:

| Piece | What it gives you |
|---|---|
| **`crewlet schema`** | JSON Schema generated from the models — the authoritative field list, for your editor, your CI, or an agent |
| **`crewlet validate --json`** | Machine-readable errors with exact field paths, so a fix loop converges |
| **The `company-architect` skill** | An interview script, the invariants, and the write → validate → fix loop |

---

## Walkthrough

End to end, from nothing to a running company. The
[quickstart](quickstart.md) builds the same shape by hand if you'd
rather see every field explained first.

### 1. Give your assistant the skill

Claude Code discovers skills on disk. From a checkout:

```bash
mkdir -p ~/.claude/skills && cp -r skills/company-architect ~/.claude/skills/
```

No checkout, or a different assistant? The skill is one self-contained
markdown file with absolute links —
[fetch it](https://github.com/crewlet/crewlet/blob/main/skills/company-architect/SKILL.md)
and paste or attach it. See [Installing the skill](#the-company-architect-skill)
for per-tool detail.

### 2. Say what you want

> Set up a Crewlet company for a small dev-tools startup — a CEO, a CTO
> with two engineers, and a PM.

### 3. Answer the interview

It won't dump YAML at you first. It asks what the company does, who's on
it, where *you* sit in the chart, which surfaces the work lives on, and
what model and budget to use. Give rough answers — it proposes a
concrete org chart and you correct it. It should also, without being
asked:

- put you in the chart as a **human seat** managing the top agent
- keep every secret as a `${VAR}`, never a literal
- **skip integrations on the first pass** (step 8 adds them)
- warn you that handles are effectively permanent

### 4. It writes and checks its own work

You get `company.yaml` (Tier B — the company) and `config.yaml`
(Tier A — the infrastructure). After every edit it runs:

```bash
crewlet validate company.yaml --json
```

and fixes each reported `path` until `valid` is true. It should tell you
which rung of the [validation ladder](#without-crewlet-installed) it
used — full `crewlet` fidelity, a plain JSON Schema validator, or
reading the schema. **None of this needs your API keys**, so the whole
design pass happens before you set up a single account.

### 5. Fill in the environment

The config references variables; you supply the values. Ask the
assistant to list every `${VAR}` it used — a missing one resolves to an
empty string and fails later, deep in a turn.

```bash
cp .env.example .env && docker compose up -d      # Pulsar + PostgreSQL

export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."                    # embeddings
export MATTERMOST_FOUNDER_USERNAME="you"          # your chat username
```

### 6. Run it

```bash
crewlet run -config config.yaml -company company.yaml
```

### 7. Watch the first turn

Open <http://localhost:8000/>. A company with no integrations has no
inbound work, which is why the first pass puts a five-minute schedule on
the top seat purely to prove the loop:

```yaml
schedules:
  - name: hello-crewlet
    cron: "*/5 * * * *"
    task: "Write a short note on what the company should focus on this week."
```

Within five minutes the CEO goes `Working`, steps through **Plan →
Execute → Review**, and returns to `Idle` — every prompt and tool call
inspectable. That's the engine, your model, and your config all
confirmed working, with no external account involved. Delete the
schedule once real work arrives.

### 8. Add one integration at a time

Now go back to the assistant:

> Add Mattermost so the team can talk in channels and DM me.

Pick your stack in [Choosing your stack](choosing-your-stack.md), create
the accounts it names (the assistant writes config, it can't stand up a
Mattermost server or create a Slack app for you), then let it wire the
config and re-validate. One integration per pass — a failure is then
unambiguous.

Tier B is live-editable, so applying a change is
`crewlet config import company.yaml --force`, no restart. Full worked
reference with everything connected:
[`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml).

---

## Why not just point an assistant at the docs

Two reasons the raw docs aren't enough on their own.

**The surface is large and strict.** Tier B validates through Pydantic
models that forbid unknown keys, so a plausible-but-invented field name
(`responsibilites`, `leed`, `commnad`) is a hard error, not a silent
no-op. An assistant working from prose will occasionally invent one.

**The expensive mistakes aren't type errors.** Choosing a handle you
later rename, putting a secret in the config file, or confusing
`integrations.plane.project` (routing identity) with
`knowledge.plane_projects` (read scope) all produce a *valid* config
that behaves wrong. Those are judgement calls, and they're what the
skill front-loads.

The schema fixes the first problem. The skill fixes the second.

---

## Without Crewlet installed

Config authoring naturally happens *before* installation — you design
the company, then set up the infrastructure to run it. So the schema is
deliberately a **static file with no dependency on the `crewlet`
binary**: an assistant fetches it from a URL and validates against it
with any standards-compliant JSON Schema validator (`jsonschema`,
`ajv`), or by reading it.

It carries more than field names. Because the config models forbid
unknown keys and the cross-field rules are encoded alongside their
Python validators, a schema-only check catches:

- unknown keys, at every level including roles, units, and MCP servers
- wrong types, and bad enums (`kind: robot`, `type: openaii`)
- malformed handles, and cron expressions with the wrong field count
- a human seat with no `contact` identity
- Confluence and Plane both enabled, and a `knowledge.*` scope list for
  the backend that isn't active

Three things still need the binary, and the skill tells the assistant to
check them by reading:

| Gap | Why the schema can't |
|---|---|
| `lead` / `manages` naming a role or unit that exists | Reference integrity across the document — not expressible in JSON Schema |
| Real IANA timezone | Needs the timezone database |
| Cron *semantics* (`99 * * * *` has the right shape) | Needs a cron parser |

The two encodings are held in sync by
`tests/test_cli/test_schema_rules.py`, which runs every rule through
both paths and fails if they disagree — a schema that quietly diverges
from the loader would be worse than no schema, because an assistant
would trust it.

---

## The validation loop

The property that makes automated authoring safe:

```bash
crewlet validate company.yaml --json
```

```json
{
  "valid": false,
  "tier": "company",
  "errors": [
    { "path": "roles.0.backstroy", "message": "Extra inputs are not permitted", "type": "extra_forbidden" },
    { "path": "units.0.leed",      "message": "Extra inputs are not permitted", "type": "extra_forbidden" }
  ]
}
```

Every offending field, with its exact path, all at once — so an
assistant fixes them in one pass instead of re-guessing. Exit code is
`0` when valid, `1` otherwise.

**It needs no credentials, no database, and no network.** Tier B stores
`${VAR}` references verbatim and resolves them at engine start (see
[Configuration § Environment variables](configuration.md#environment-variable-references)),
so a complete config validates *before any secret exists*. You can draft
and check an entire company offline.

Validation is deep: it builds the `Organization`, so unknown unit leads,
bad cron expressions, invalid timezones, human seats with no contact
identity, and the Confluence-XOR-Plane rule all fail here rather than at
run time.

`--tier auto` (the default) picks the model from the file: Tier B
requires a top-level `name` and Tier A forbids it, so the split is exact.
Force it with `--tier company` / `--tier bootstrap`.

---

## Editor autocomplete

The same schema drives your editor. Add a modeline to the top of the
file — with the [YAML Language Server](https://github.com/redhat-developer/yaml-language-server)
(built into VS Code's YAML extension, and available in Neovim, JetBrains,
and Helix) you get completion, inline docs from the field docstrings, and
typo squiggles as you type:

```yaml
# yaml-language-server: $schema=https://docs.crewlet.ai/schema/company.schema.json
name: "Acme AI"
```

Or generate it locally and point at the file:

```bash
crewlet schema company -o schema/company.schema.json
crewlet schema bootstrap -o schema/bootstrap.schema.json
```

Both are also checked into [`schema/`](../../schema/) in the repo. They
are generated artifacts — a test regenerates and compares them, so they
cannot drift from what the loader accepts.

Wire it into CI to catch a bad config before it reaches the engine:

```bash
crewlet validate company.yaml --json || exit 1
```

---

## The `company-architect` skill

[`skills/company-architect/SKILL.md`](../../skills/company-architect/SKILL.md)
is a prompt for an AI assistant. It carries the interview script, the
invariants that the schema can't express, and the validation loop above.

It is written provider-neutral — it's a markdown file, not a
vendor format — so it works anywhere you can give an assistant
instructions.

**Claude Code** discovers skills automatically. Install it for one
project or for every project:

```bash
# this project only
mkdir -p .claude/skills && cp -r skills/company-architect .claude/skills/

# every project
mkdir -p ~/.claude/skills && cp -r skills/company-architect ~/.claude/skills/
```

Then just say what you want:

> Set up a Crewlet company for a 5-person dev-tools startup.

**Any other assistant** — Cursor, Copilot, Codex, the ChatGPT/Claude web
apps: paste the file, attach it, or add it to your rules/context
directory. If you installed Crewlet with `pip` and don't have a repo
checkout, fetch it from
[GitHub](https://github.com/crewlet/crewlet/blob/main/skills/company-architect/SKILL.md).

### What it will do

- Interview you about the company before writing anything.
- Start you **integration-free** with one scheduled task, so you see a
  real agent turn within minutes — then layer integrations one at a
  time.
- Keep every secret as a `${VAR}` reference, never a literal.
- Put you in the org chart as a
  [human seat](../concepts/humans-in-the-org.md#the-founder-seat).
- Warn you that handles are effectively permanent (see below).
- Validate after every edit, and not claim success until it passes.

### What it can't do

It writes config; it doesn't provision. Standing up the Mattermost server
(or the Slack workspace), the Plane workspace, the GitLab group, and the
API keys is still your job — [Choosing your stack](choosing-your-stack.md)
lists what you must create by hand for each. Once those exist,
[`crewlet mattermost provision`](../reference/cli.md#crewlet-mattermost-provision),
[`crewlet plane provision`](../reference/cli.md#crewlet-plane-provision)
and [`crewlet gitlab provision`](../reference/cli.md#crewlet-gitlab-provision)
mint the per-seat accounts and tokens into the `${VAR}` references the
config already declares.

---

## The one thing to get right before you run

**Handles are effectively permanent.** An agent's durable id is
`uuid5(namespace, f"{org.name}:{handle}")`, so renaming a seat's
`handle` — *or the company `name`* — mints a new id and orphans that
agent's diary, onboarding markers, and counterparty profiles. The seat
keeps working, but it has lost its memory.

Settle the company name and each handle before the company runs. See
[Agent Runtime § Agent Definition vs Agent Instance](../concepts/agent-runtime.md#agent-definition-vs-agent-instance).

---

## Next steps

- [Quickstart](quickstart.md) — the minimal four-seat company, by hand
- [Choosing your stack](choosing-your-stack.md) — every external
  dependency, hosted vs self-hosted
- [Configuration reference](configuration.md) — the full field list in
  prose
- [Configure via API](../guides/configure-via-api.md) — the same config
  over `/config/*` instead of a file
