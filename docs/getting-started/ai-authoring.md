# Authoring Your Company with an AI Assistant

A Crewlet company is described in YAML: a mission, an org chart, seats
with backstories, policies, integrations. That is a lot of surface to
write from a blank file — so Crewlet ships the pieces an AI assistant
needs to write it *with* you and check its own work.

Three things make this work, and you can use any of them on their own:

| Piece | What it gives you |
|---|---|
| **`crewlet schema`** | JSON Schema generated from the models — the authoritative field list, for your editor, your CI, or an agent |
| **`crewlet validate -json`** | Machine-readable problems with exact field paths and kinds, so a fix loop converges |
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

You get `company.yaml` (Tier B — the company) and `crewlet.yaml`
(Tier A — the infrastructure). After every edit it runs:

```bash
crewlet validate company.yaml -json
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
# nothing to bring up: the engine embeds its stream and creates its store

export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."                    # embeddings
export MATTERMOST_FOUNDER_USERNAME="you"          # your chat username
```

### 6. Run it

```bash
crewlet run crewlet.yaml -company company.yaml
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
`crewlet config import company.yaml`, no restart. A full worked
seven-seat company, with every setting's reasoning in a comment:
[`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml) — the
full stack, on a tracker, a wiki and a code host. Its sibling
[`examples/nimbus-claude-cli.company.yaml`](../../examples/nimbus-claude-cli.company.yaml)
is the same company with chat on Mattermost and nothing else, which is the
shape one of these passes should land on before it adds the next
integration.

---

## Why not just point an assistant at the docs

Two reasons the raw docs aren't enough on their own.

**The surface is large and strict.** Tier B validates against the typed config models
models that forbid unknown keys, so a plausible-but-invented field name
(`responsibilites`, `leed`, `commnad`) is a hard error, not a silent
no-op. An assistant working from prose will occasionally invent one.

**The expensive mistakes aren't type errors.** Choosing a handle you
later rename, putting a secret in the config file, or confusing a unit's
`space` (routing identity) with `knowledge.scope` (read scope) all
produce a *valid* config that behaves wrong. Those are judgement calls, and they're what the
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
unknown keys and the cross-field rules are generated from the same Go
types the engine parses with, a schema-only check catches:

- unknown keys, at every level including roles, units, and MCP servers
- wrong types, and bad enums (`kind: robot`, `type: openaii`)
- malformed handles, and cron expressions with the wrong field count
- a human seat with no `contact` identity
- a `knowledge.*` scope list naming a backend the config does not configure

Three things still need the binary, and the skill tells the assistant to
check them by reading:

| Gap | Why the schema can't |
|---|---|
| `lead` / `manages` naming a seat handle or unit key that exists | Reference integrity across the document — not expressible in JSON Schema |
| Real IANA timezone | Needs the timezone database |
| Cron *semantics* (`99 * * * *` has the right shape) | Needs a cron parser |

The two encodings are held in sync by
`internal/config/schema_test.go`, which runs every rule through
both paths and fails if they disagree — a schema that quietly diverges
from the loader would be worse than no schema, because an assistant
would trust it.

---

## The validation loop

The property that makes automated authoring safe:

```bash
crewlet validate company.yaml -json
```

```json
{
  "valid": false,
  "tier": "company",
  "file": "company.yaml",
  "problems": [
    { "path": "roles[1].llm", "segments": ["roles", 1, "llm"],
      "kind": "unknown_value", "seat": "cto",
      "message": "roles[1].llm: value not in the allowed set: \"nonexistent\" is not a configured provider: providers.llm has primary. A key that misses is not an error at run time: the seat falls back to another model and bills against it, so this is the only place the typo can be seen" },
    { "path": "units[0].roles[0].name", "segments": ["units", 0, "roles", 0, "name"],
      "kind": "conflict", "seat": "software-engineer",
      "message": "duplicate seat name \"Software Engineer\": 2 seats carry it (handle \"software-engineer\" in unit \"Engineering\"; handle \"software-engineer-2\" in unit \"Engineering\"). A colleague named in prose is resolved by this name, so an agent asking for it is offered both of them every time; give each of these seats its own name" },
    { "path": "units[0].roles[1].name", "segments": ["units", 0, "roles", 1, "name"],
      "kind": "conflict", "seat": "software-engineer-2",
      "message": "duplicate seat name \"Software Engineer\": 2 seats carry it (handle \"software-engineer\" in unit \"Engineering\"; handle \"software-engineer-2\" in unit \"Engineering\"). A colleague named in prose is resolved by this name, so an agent asking for it is offered both of them every time; give each of these seats its own name" }
  ],
  "warnings": [
    { "kind": "dangling_reference", "ref": "lead",
      "path": "units[0].lead", "segments": ["units", 0, "lead"],
      "seat": "", "unit": "Engineering", "from": "Engineering", "to": "tech-lead",
      "message": "unit \"Engineering\" names lead \"tech-lead\", which is no seat's handle, so the unit and every descendant inheriting its lead run with no lead. Correct the handle or add a seat that derives it" }
  ]
}
```

Every offending field, with its exact path, all at once, so an assistant
fixes them in one pass instead of re-guessing. A rule broken inside a seat or
a unit names that seat's handle or that unit as well, and a seat is located
where it was written even when its `unit:` reference moves it into a unit.

A document the parser refuses (an unknown key, a value of the wrong shape)
reports every such key at once, at the key as it sits in the file and with the
line it is on. The rules above run once it parses:

```json
{ "path": "roles[0].backstroy", "segments": ["roles", 0, "backstroy"],
  "kind": "unknown_field", "line": 6,
  "message": "roles[0].backstroy: unknown field: \"backstroy\" is not a setting: check the spelling, or the block it belongs under (line 6)" }
```

Each problem carries:

| Field | What it is |
|---|---|
| `path` | Where it is, as the document spells it. Empty only for a failure that belongs to no place in the document, such as a file that is not YAML at all. |
| `segments` | The same place taken apart: strings for keys, numbers for list indexes. A map key can contain a dot (a provider called `claude-3.5`), so read these rather than splitting `path`. `null` when `path` is empty. |
| `kind` | One of `missing`, `out_of_range`, `conflict`, `shape`, `unknown_field`, `unknown_value`, or `invalid` for anything this build does not classify. |
| `message` | The whole line, exactly as the prose output prints it. |
| `seat`, `unit` | The handle of the seat, or the name of the unit, the problem is about. Omitted when it is about neither. |
| `line` | The line in the file, for a problem the parser found. Omitted otherwise. |

`kind` is a closed set with a fallback deliberately: a loop branching on it
must never receive an empty string and read it as a field somebody forgot to
populate. Two seats sharing a name are one message and **one problem per
seat**, each at the name that seat wrote, so `problems` can hold more entries
than the prose output has lines (which leads such a message with every path
it applies to).

`warnings` are references that resolve to nothing: a unit `lead` (a seat
handle), a root seat's `unit` (a unit key), a `manages` entry (either), or a
GitLab access level naming no seat.
The engine runs a company with one (live configuration assembles an
organization in pieces), so a warning never fails validation, but one that
survives a finished document is a misspelling nothing else will report.
`kind` is `dangling_reference`, `ref` says which kind of reference, `from`
and `to` are what holds it and what it names, and `path`, `segments`,
`seat` and `unit` locate it as they do a problem. Both lists are always
arrays, empty rather than absent.

Exit code is `0` when valid and `1` otherwise, **in both output modes**, so
`crewlet validate company.yaml -json || exit 1` actually gates. Nothing is
echoed on stderr in `-json` mode: the payload already carries every problem,
and a second copy is what makes the loop's log unreadable.

**It needs no credentials, no database, and no network.** Tier B stores
`${VAR}` references verbatim and resolves them at engine start (see
[Configuration § Environment variables](configuration.md#environment-variable-references)),
so a complete config validates *before any secret exists*. You can draft
and check an entire company offline.

Validation is deep: it builds the `Organization`, so duplicate seat names,
duplicate unit keys, bad cron expressions, invalid timezones and a
knowledge scope with no backend behind it all fail here rather than at run
time, and a unit lead that is no seat's handle is reported as a warning.

`-tier auto` (the default) picks the tier from the document's **keys**, not
its filename: the one thing this has to get right is the case where the file
was named something else. The two tiers share no top-level key:
`name` / `roles` / `units` / `providers` / `integrations` mean Tier B,
`node` / `stream` / `store` / `coordination` / `api` mean Tier A.

A document carrying neither, or an equal count of both, is **refused naming
the flag** rather than guessed at: guessing wrong reports every field of the
file as invalid, and a fix loop reading that has no way to tell it from a
genuinely broken document. Force it with `-tier company` / `-tier bootstrap`.

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
crewlet validate company.yaml -json || exit 1
```

---

## The `company-architect` skill

[`skills/company-architect/SKILL.md`](../../skills/company-architect/SKILL.md)
is a prompt for an AI assistant. It carries the interview script, the
invariants that the schema can't express, and the validation loop above.

It is written provider-neutral (it's a markdown file, not a
vendor format), so it works anywhere you can give an assistant
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
directory. If you installed a released binary and don't have a repo
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
(or the Slack workspace), the Atlassian site, the GitLab group, and the
API keys is still your job — [Choosing your stack](choosing-your-stack.md)
lists what you must create by hand for each. Once those exist,
[`crewlet mattermost provision`](../reference/cli.md#crewlet-mattermost-provision)
and [`crewlet gitlab provision`](../reference/cli.md#crewlet-gitlab-provision)
mint the per-seat accounts and tokens into the `${VAR}` references the
config already declares; Atlassian and GitHub issue no credential on a
provisioner's behalf, so their commands report which account each
hand-created credential turned out to be.

---

## The one thing to get right before you run

**Handles are effectively permanent.** An agent's durable id is a UUIDv5
over `"<company name>:<handle>"` (`org.DeriveAgentID`), so renaming a
seat's `handle`, *or the company `name`*, mints a new id and orphans that
agent's diary, onboarding markers, and counterparty profiles. The seat
keeps working, but it has lost its memory.

Settle the company name and each handle before the company runs. See
[Agent Runtime § Seat Definition and the Runner](../concepts/agent-runtime.md#seat-definition-and-the-runner).

---

## Next steps

- [Quickstart](quickstart.md) — the minimal four-seat company, by hand
- [Choosing your stack](choosing-your-stack.md) — every external
  dependency, hosted vs self-hosted
- [Configuration reference](configuration.md) — the full field list in
  prose
- [Configure via API](../guides/configure-via-api.md) — the same config
  over `/config/*` instead of a file
